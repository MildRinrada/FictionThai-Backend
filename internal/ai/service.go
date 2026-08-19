package ai

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/ratelimit"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// Output-validation limits (docs/12 §35). Provider output is UNTRUSTED, even
// from the local provider: a suggestion with an impossible offset, an unknown
// type, or absurd length is dropped rather than persisted.
const (
	maxInlineSuggestions = 200
	maxSuggestionRunes   = 1000
	maxSummaryRunes      = 4000
	daysQuotaWindow      = 24 * time.Hour
)

// ChapterAccess is the sliver of the chapters domain the AI service needs: a
// chapter's analyzable content, gated to its OWNER by user id (docs/12 §33). It
// is the ONLY way this package reaches a manuscript - never a repository, never
// a privileged read (the Phase 10 brief "use that domain's service/reader
// boundary").
type ChapterAccess interface {
	ContentForOwnerID(ctx context.Context, ownerID, chapterID uuid.UUID) (*chapters.OwnedContent, error)
}

// Notifier is the sliver of notifications this service needs: telling a writer
// their asynchronous job finished (docs/12 §27 "Notify frontend"). It is
// fire-and-forget - a notification failure never changes a job's outcome.
type Notifier interface {
	AIRequestCompleted(ctx context.Context, recipientID, requestID uuid.UUID)
}

// Achiever is the sliver of the achievements domain this package needs
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3). One interface for both services
// here, because both live in this package.
//
// The pair it feeds is deliberately symmetrical: เจ้าของภาษา for taking the
// assistant's advice, ไม่เชื่อ AI for teaching it to be quiet. Disagreeing
// with the assistant is a legitimate way to write on this platform, and the
// achievement set is where that is said out loud.
//
// Fire-and-forget, like the notifier above it.
type Achiever interface {
	SuggestionAccepted(ctx context.Context, userID uuid.UUID)
	SuggestionMuted(ctx context.Context, userID uuid.UUID)
}

// Config is the AI runtime configuration (docs/12 §36 env-level knobs).
type Config struct {
	// Enabled is the platform master switch. When false every AI operation
	// returns a clean 503 and the rest of the app is untouched (docs/12 §31 "AI
	// is an optional dependency").
	Enabled bool
	// MaxInputRunes caps analyzed text (docs/11 §53 "extremely long prompts",
	// docs/12 §29 input limits). Counted in runes - Thai per character.
	MaxInputRunes int
	// DailyQuota is the per-user persisted-request budget per 24h (docs/12
	// §29–§30). Enforced through the shared atomic limiter, so it is race-safe.
	DailyQuota int
	// ModelURL is the optional consistency sidecar
	// (docs/AI-CONSISTENCY-MODEL.md). Empty = rules only, the default.
	ModelURL string
}

// Service owns AI business rules and is the authorization boundary for /ai.
type Service struct {
	repo     *Repository
	provider Provider
	chapters ChapterAccess
	notifier Notifier
	queue    Queue
	limiter  ratelimit.Limiter
	cfg      Config
	log      *slog.Logger

	// achievements is optional and set after construction. nil records nothing.
	achievements Achiever

	dailyPolicy ratelimit.Policy
}

// SetAchiever attaches the achievement service after construction.
func (s *Service) SetAchiever(achiever Achiever) { s.achievements = achiever }

// NewService wires the AI service. notifier may be nil (async completion then
// simply notifies nobody).
func NewService(
	repo *Repository, provider Provider, chapterAccess ChapterAccess, notifier Notifier,
	queue Queue, limiter ratelimit.Limiter, cfg Config, log *slog.Logger,
) *Service {
	return &Service{
		repo: repo, provider: provider, chapters: chapterAccess, notifier: notifier,
		queue: queue, limiter: limiter, cfg: cfg, log: log,
		dailyPolicy: ratelimit.Policy{Name: "ai_daily", Limit: cfg.DailyQuota, Window: daysQuotaWindow},
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

func unavailable() *apierror.Error {
	return apierror.New(http.StatusServiceUnavailable, apierror.CodeUnavailable,
		"AI assistance is currently unavailable.")
}

func requestNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "AI_REQUEST_NOT_FOUND", "AI request not found.")
}

func suggestionNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "AI_SUGGESTION_NOT_FOUND", "AI suggestion not found.")
}

func quotaExceeded() *apierror.Error {
	return apierror.New(http.StatusTooManyRequests, "AI_QUOTA_EXCEEDED",
		"You have reached today's AI usage limit. Please try again later.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("ai: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

// ---------------------------------------------------------------------------
// Stateless analysis (docs/09 §24 spell-check; docs/12 §27 sync path)
// ---------------------------------------------------------------------------

// Analyze runs the fast local checks over raw text and returns transient inline
// suggestions. Nothing is persisted (docs/11 §54): this is the editor's inline
// pass, the cheapest, most frequent path (docs/12 §29). It is bounded by the
// per-minute AI rate tier at the route, not the daily quota.
func (s *Service) Analyze(
	ctx context.Context, identity *auth.Identity, text string,
) ([]InlineSuggestion, error) {
	if !s.cfg.Enabled {
		return nil, unavailable()
	}
	if _, err := requireUser(identity); err != nil {
		return nil, err
	}
	if err := s.validateText(text); err != nil {
		return nil, err
	}

	result, err := s.provider.Analyze(ctx, AnalyzeInput{Text: text})
	if err != nil {
		return nil, s.mapProviderError("analyze", err)
	}
	return s.validateInline(result.Suggestions, len([]rune(text))), nil
}

// ---------------------------------------------------------------------------
// Persisted requests (docs/08 §25–§26; docs/12 §28 lifecycle)
// ---------------------------------------------------------------------------

// CreateRequestInput is the raw request from the handler.
type CreateRequestInput struct {
	Feature   string
	ChapterID string
}

// CreateRequest creates an AI request against a chapter the caller OWNS. Fast
// features run synchronously and return completed with their suggestions;
// summary is queued for the worker and returned as queued (docs/12 §27).
func (s *Service) CreateRequest(
	ctx context.Context, identity *auth.Identity, input CreateRequestInput,
) (RequestView, error) {
	if !s.cfg.Enabled {
		return RequestView{}, unavailable()
	}
	userID, err := requireUser(identity)
	if err != nil {
		return RequestView{}, err
	}

	feature := Feature(strings.TrimSpace(input.Feature))
	errs := map[string][]string{}
	if !feature.Valid() {
		errs["feature"] = []string{"Unsupported AI feature."}
	}
	chapterID, cerr := uuid.Parse(strings.TrimSpace(input.ChapterID))
	if cerr != nil {
		errs["chapter_id"] = []string{"A valid chapter id is required."}
	}
	if len(errs) > 0 {
		return RequestView{}, apierror.Validation(errs)
	}

	// Authorization + content, owner-only and non-oracle (docs/12 §33). This is
	// the ONLY manuscript access, and it happens before any quota is spent.
	content, err := s.chapters.ContentForOwnerID(ctx, userID, chapterID)
	if err != nil {
		return RequestView{}, err
	}
	if strings.TrimSpace(content.Content) == "" {
		return RequestView{}, apierror.Validation(map[string][]string{
			"chapter_id": {"This chapter has no prose to analyze."},
		})
	}
	if len([]rune(content.Content)) > s.cfg.MaxInputRunes {
		// A chapter longer than the cap is analyzed up to the cap; that is a
		// cost control, not an error (docs/12 §29). The excess is simply not sent.
		content.Content = string([]rune(content.Content)[:s.cfg.MaxInputRunes])
	}

	// Quota is consumed at CREATION, before the work - so a failed generation
	// still counts (the attempt has a cost) and concurrent creates cannot
	// overspend (the limiter is atomic). Documented in docs/12.
	if !s.consumeQuota(ctx, userID) {
		return RequestView{}, quotaExceeded()
	}

	if feature.Async() {
		return s.enqueue(ctx, userID, chapterID, feature)
	}
	return s.runSync(ctx, userID, chapterID, feature, content.Content)
}

// enqueue records a queued request and hands its id to the worker.
func (s *Service) enqueue(
	ctx context.Context, userID uuid.UUID, chapterID uuid.UUID, feature Feature,
) (RequestView, error) {
	req, err := s.repo.Enqueue(ctx, userID, &chapterID, feature, s.provider.Name())
	if err != nil {
		return RequestView{}, s.internal("enqueue request", err)
	}
	// The request context ends when the response is written; the enqueue must
	// outlive it (mirrors the notifications emit).
	if err := s.queue.Enqueue(context.WithoutCancel(ctx), req.ID); err != nil {
		// The row is durable and RecoverQueued will re-enqueue it on the next
		// worker start, so this is logged, not fatal.
		s.log.Error("ai: enqueue job failed",
			slog.String("request_id", req.ID.String()), slog.Any("error", err))
	}
	s.log.Info("ai request queued",
		slog.String("request_id", req.ID.String()),
		slog.String("user_id", userID.String()),
		slog.String("feature", string(feature)))
	return req.View(nil), nil
}

// runSync executes a fast feature in-request and persists the outcome.
func (s *Service) runSync(
	ctx context.Context, userID, chapterID uuid.UUID, feature Feature, text string,
) (RequestView, error) {
	result, err := s.provider.Analyze(ctx, AnalyzeInput{Text: text, Kinds: feature.analyzeKinds()})
	if err != nil {
		// A synchronous provider failure becomes a persisted FAILED request (the
		// resource exists and is queryable), not an HTTP error - consistent with
		// the async model. Only the safe classification is stored.
		code := string(failureKindOf(err))
		req, ierr := s.repo.InsertFailed(ctx, InsertFailedParams{
			UserID: userID, ChapterID: &chapterID, Feature: feature,
			Provider: s.provider.Name(), ErrorCode: code,
		})
		if ierr != nil {
			return RequestView{}, s.internal("record failed request", ierr)
		}
		s.log.Warn("ai sync request failed",
			slog.String("request_id", req.ID.String()),
			slog.String("feature", string(feature)),
			slog.String("error_code", code))
		return req.View(nil), nil
	}

	inline := s.validateInline(result.Suggestions, len([]rune(text)))
	news := toNewSuggestions(chapterID, inline)
	model := result.Model
	req, sugs, err := s.repo.InsertCompleted(ctx, InsertCompletedParams{
		UserID: userID, ChapterID: &chapterID, Feature: feature,
		Provider: s.provider.Name(), Model: &model, Suggestions: news,
	})
	if err != nil {
		return RequestView{}, s.internal("record completed request", err)
	}
	s.log.Info("ai request completed",
		slog.String("request_id", req.ID.String()),
		slog.String("feature", string(feature)),
		slog.String("model", model),
		slog.Int("suggestions", len(sugs)))
	return req.View(sugs), nil
}

// GetRequest returns one of the caller's requests with its suggestions. Someone
// else's request id is the same 404 as a missing one (docs/11 §31).
func (s *Service) GetRequest(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (RequestView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return RequestView{}, err
	}
	req, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return RequestView{}, requestNotFound()
	}
	if err != nil {
		return RequestView{}, s.internal("load request", err)
	}
	if req.UserID != userID {
		return RequestView{}, requestNotFound()
	}
	sugs, err := s.repo.Suggestions(ctx, req.ID)
	if err != nil {
		return RequestView{}, s.internal("load suggestions", err)
	}
	return req.View(sugs), nil
}

// ListRequests returns a page of the caller's request history, newest first,
// without suggestions (docs/11 §1461).
func (s *Service) ListRequests(
	ctx context.Context, identity *auth.Identity, page pagination.Params,
) ([]RequestView, pagination.Meta, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	reqs, total, err := s.repo.ListForUser(ctx, userID, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list requests", err)
	}
	views := make([]RequestView, 0, len(reqs))
	for i := range reqs {
		views = append(views, reqs[i].View(nil))
	}
	return views, page.MetaFor(int64(total)), nil
}

// DecideSuggestion records the writer's decision on a suggestion. Accepting one
// NEVER modifies the manuscript (docs/12 §15): it records the choice and returns
// the suggested text; the writer applies it through the normal, revisioned
// chapter-edit path (docs/12 §16, §43).
func (s *Service) DecideSuggestion(
	ctx context.Context, identity *auth.Identity, id uuid.UUID, decision string,
) (SuggestionView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return SuggestionView{}, err
	}
	status := SuggestionStatus(strings.TrimSpace(decision))
	if !decisionValid(status) {
		return SuggestionView{}, apierror.Validation(map[string][]string{
			"decision": {"Must be one of: accepted, rejected, dismissed."},
		})
	}

	sug, err := s.repo.FindSuggestionForUser(ctx, id, userID)
	if errors.Is(err, ErrNotFound) {
		return SuggestionView{}, suggestionNotFound()
	}
	if err != nil {
		return SuggestionView{}, s.internal("load suggestion", err)
	}
	if sug.Status != SuggestionPending {
		return SuggestionView{}, apierror.Conflict("This suggestion has already been decided.")
	}

	ok, err := s.repo.SetSuggestionStatus(ctx, id, status)
	if err != nil {
		return SuggestionView{}, s.internal("decide suggestion", err)
	}
	if !ok {
		// Lost a race with another concurrent decision.
		return SuggestionView{}, apierror.Conflict("This suggestion has already been decided.")
	}
	sug.Status = status
	// เจ้าของภาษา (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3). Accepting counts
	// a DECISION - the manuscript is untouched either way (docs/12 §15).
	if s.achievements != nil && status == SuggestionAccepted {
		s.achievements.SuggestionAccepted(ctx, userID)
	}
	return sug.View(), nil
}

// RetryRequest re-queues a caller's FAILED, retryable request (docs/12 §32).
// Retry is explicit and user-triggered - never automatic - so a broken provider
// cannot cause a retry storm (docs/12 §29).
func (s *Service) RetryRequest(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (RequestView, error) {
	if !s.cfg.Enabled {
		return RequestView{}, unavailable()
	}
	userID, err := requireUser(identity)
	if err != nil {
		return RequestView{}, err
	}
	req, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return RequestView{}, requestNotFound()
	}
	if err != nil {
		return RequestView{}, s.internal("load request", err)
	}
	if req.UserID != userID {
		return RequestView{}, requestNotFound()
	}
	if req.Status != StatusFailed {
		return RequestView{}, apierror.Conflict("Only a failed request can be retried.")
	}
	if req.ErrorCode == nil || !FailureKind(*req.ErrorCode).Retryable() {
		return RequestView{}, apierror.Conflict("This failure cannot be retried.")
	}

	// A retry is a fresh attempt with its own cost (docs/12 §30).
	if !s.consumeQuota(ctx, userID) {
		return RequestView{}, quotaExceeded()
	}

	ok, err := s.repo.Requeue(ctx, id, userID)
	if err != nil {
		return RequestView{}, s.internal("requeue request", err)
	}
	if !ok {
		return RequestView{}, apierror.Conflict("This request is no longer retryable.")
	}
	if err := s.queue.Enqueue(context.WithoutCancel(ctx), id); err != nil {
		s.log.Error("ai: re-enqueue job failed",
			slog.String("request_id", id.String()), slog.Any("error", err))
	}

	reloaded, err := s.repo.Find(ctx, id)
	if err != nil {
		return RequestView{}, s.internal("reload request", err)
	}
	return reloaded.View(nil), nil
}

// CancelRequest cancels a caller's still-queued request (docs/12 §28 cancelled).
// Once claimed or terminal it can no longer be cancelled - a conflict.
func (s *Service) CancelRequest(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (RequestView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return RequestView{}, err
	}
	req, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return RequestView{}, requestNotFound()
	}
	if err != nil {
		return RequestView{}, s.internal("load request", err)
	}
	if req.UserID != userID {
		return RequestView{}, requestNotFound()
	}
	ok, err := s.repo.Cancel(ctx, id, userID)
	if err != nil {
		return RequestView{}, s.internal("cancel request", err)
	}
	if !ok {
		return RequestView{}, apierror.Conflict("Only a queued request can be cancelled.")
	}
	reloaded, err := s.repo.Find(ctx, id)
	if err != nil {
		return RequestView{}, s.internal("reload request", err)
	}
	return reloaded.View(nil), nil
}

// ---------------------------------------------------------------------------
// Validation & helpers
// ---------------------------------------------------------------------------

// validateText enforces the presence and size limits on raw analyzed text
// (docs/11 §53, docs/12 §29).
func (s *Service) validateText(text string) error {
	if strings.TrimSpace(text) == "" {
		return apierror.Validation(map[string][]string{"text": {"Text is required."}})
	}
	if len([]rune(text)) > s.cfg.MaxInputRunes {
		return apierror.Validation(map[string][]string{
			"text": {"Text is too long to analyze."},
		})
	}
	return nil
}

// validateInline enforces docs/12 §35 on provider output: valid rune offsets
// within the text, a known suggestion type, sane lengths, and a hard count cap.
// Anything failing is DROPPED, never persisted or returned.
func (s *Service) validateInline(in []InlineSuggestion, textRuneLen int) []InlineSuggestion {
	out := make([]InlineSuggestion, 0, len(in))
	dropped := 0
	for _, sug := range in {
		if len(out) >= maxInlineSuggestions {
			dropped += len(in) - len(out)
			break
		}
		if !isKnownInlineType(sug.Type) {
			dropped++
			continue
		}
		if sug.Start < 0 || sug.End < sug.Start || sug.End > textRuneLen {
			dropped++
			continue
		}
		if len([]rune(sug.Original)) > maxSuggestionRunes {
			dropped++
			continue
		}
		clean := make([]string, 0, len(sug.Suggestions))
		for _, alt := range sug.Suggestions {
			if len([]rune(alt)) <= maxSuggestionRunes {
				clean = append(clean, alt)
			}
		}
		sug.Suggestions = clean
		out = append(out, sug)
	}
	if dropped > 0 {
		// No silent caps: record that output was trimmed (docs/15, brief).
		s.log.Warn("ai: dropped invalid provider suggestions", slog.Int("dropped", dropped))
	}
	return out
}

// consumeQuota records one unit against the caller's daily budget and reports
// whether it was within the limit. Fails OPEN like every limiter check: a
// limiter outage degrades the quota, never the feature (docs/15 §38).
func (s *Service) consumeQuota(ctx context.Context, userID uuid.UUID) bool {
	if s.cfg.DailyQuota <= 0 {
		return true
	}
	return s.limiter.Allow(ctx, "user:"+userID.String(), s.dailyPolicy).Allowed
}

// UsageView is the caller's standing against the daily budget - what the
// assistant settings page shows so "ครบโควตา" is never the first time a
// writer hears a quota exists.
type UsageView struct {
	// Limited is false when the platform runs without a daily cap; the other
	// fields are then zero and the client shows nothing.
	Limited bool `json:"limited"`
	// DailyQuota is the persisted-request budget per 24h window. The window
	// starts at the first request, not at midnight.
	DailyQuota int `json:"daily_quota"`
	Used       int `json:"used"`
	Remaining  int `json:"remaining"`
}

// Usage reads the caller's daily-quota standing WITHOUT spending any of it -
// the limiter is peeked, never hit.
func (s *Service) Usage(ctx context.Context, identity *auth.Identity) (UsageView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return UsageView{}, err
	}
	if s.cfg.DailyQuota <= 0 {
		return UsageView{}, nil
	}
	result := s.limiter.Peek(ctx, "user:"+userID.String(), s.dailyPolicy)
	used := s.cfg.DailyQuota - result.Remaining
	if !result.Allowed {
		used = s.cfg.DailyQuota
	}
	return UsageView{
		Limited:    true,
		DailyQuota: s.cfg.DailyQuota,
		Used:       used,
		Remaining:  result.Remaining,
	}, nil
}

// mapProviderError turns a provider failure into a SAFE client error, logging
// the real classification. Implementation detail never reaches the client
// (docs/12 §"AI UX Safety", docs/11 §67).
func (s *Service) mapProviderError(op string, err error) error {
	kind := failureKindOf(err)
	s.log.Warn("ai: provider failure",
		slog.String("op", op), slog.String("kind", string(kind)))
	return unavailable()
}

// toNewSuggestions maps validated inline suggestions to persistable rows. The
// first proposed replacement (if any) becomes suggested_text; a flag-only issue
// persists with a null suggestion (docs/12 §10).
func toNewSuggestions(chapterID uuid.UUID, inline []InlineSuggestion) []NewSuggestion {
	out := make([]NewSuggestion, 0, len(inline))
	for _, sug := range inline {
		var suggested *string
		if len(sug.Suggestions) > 0 {
			s := sug.Suggestions[0]
			suggested = &s
		}
		var explanation *string
		if sug.Explanation != "" {
			e := sug.Explanation
			explanation = &e
		}
		out = append(out, NewSuggestion{
			ChapterID:     chapterID,
			Type:          sug.Type,
			OriginalText:  sug.Original,
			SuggestedText: suggested,
			Explanation:   explanation,
		})
	}
	return out
}

func isKnownInlineType(t string) bool {
	switch t {
	case SuggestionSpelling, SuggestionPunctuation, SuggestionRepetition, SuggestionPolish:
		return true
	}
	return false
}

// Compile-time assurance the real chapters service satisfies the narrow
// interface this package declares.
var _ ChapterAccess = (*chapters.Service)(nil)
