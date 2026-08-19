// Package ai implements Phase 10 - AI / Thai NLP assistance (docs/12; docs/08
// §25–§26; docs/09 §24). It is an OPTIONAL, assistive layer: it detects
// possible issues and proposes suggestions, and the writer accepts or rejects
// them. It never edits a manuscript itself, never trains on user content, and
// never blocks writing when it is unavailable (docs/12 §2, §15, §31, §43).
//
// Domain boundaries, following the pattern established through Phases 2–9:
//
//   - The actual language work lives behind the narrow Provider interface, so
//     the domain is not coupled to a tokenizer or model (provider.go, local.go).
//   - Chapter content is reached ONLY through the chapters writer-authorization
//     boundary (ChapterAccess), never a privileged cross-user read: a writer can
//     only run AI over fiction they may edit (docs/12 §33, docs/11 §53).
//   - Notifications for finished async work go through the existing
//     notifications domain via a consumer-defined Notifier (service.go).
//
// This package never imports chapters' or novels' internals; it declares the
// slivers it needs as interfaces and lets the wiring satisfy them.
package ai

import (
	"time"

	"github.com/google/uuid"
)

// Feature is the AI operation requested (ai_requests.feature). Stored as an
// OPEN VARCHAR vocabulary (docs/08 §25.1, following notifications.type): the
// documented list spans several rollout phases (docs/12 §5, §39), so the
// service - not the database - is the authority on what is ENABLED today.
type Feature string

const (
	// FeatureSpellCheck runs the fast spelling + punctuation rules synchronously
	// (docs/12 §40 MVP, §27 sync path).
	FeatureSpellCheck Feature = "spell_check"
	// FeatureRepetition flags over-repeated wording synchronously (docs/12 §10).
	FeatureRepetition Feature = "repetition"
	// FeatureSummary produces a chapter summary ASYNCHRONOUSLY (docs/12 §22,
	// §27 async path) - the one feature routed through the queue and worker.
	FeatureSummary Feature = "summary"
)

// EnabledFeatures is the vocabulary the API accepts today. Widening it is a
// one-line change here plus a provider capable of the work - never a migration.
func EnabledFeatures() []Feature {
	return []Feature{FeatureSpellCheck, FeatureRepetition, FeatureSummary}
}

// Valid reports whether f is an enabled feature.
func (f Feature) Valid() bool {
	for _, e := range EnabledFeatures() {
		if f == e {
			return true
		}
	}
	return false
}

// Async reports whether the feature is processed by the worker rather than in
// the request (docs/12 §27). Summary is the expensive one.
func (f Feature) Async() bool { return f == FeatureSummary }

// analyzeKinds is the set of inline-suggestion families a synchronous feature
// produces (docs/12 §5). Summary is not analysis and returns nil.
func (f Feature) analyzeKinds() []string {
	switch f {
	case FeatureSpellCheck:
		return []string{SuggestionSpelling, SuggestionPunctuation}
	case FeatureRepetition:
		return []string{SuggestionRepetition}
	default:
		return nil
	}
}

// RequestStatus is the ai_requests lifecycle (docs/12 §28). Closed set, enforced
// by a CHECK constraint (unlike the open feature vocabulary).
type RequestStatus string

const (
	StatusQueued     RequestStatus = "queued"
	StatusProcessing RequestStatus = "processing"
	StatusCompleted  RequestStatus = "completed"
	StatusFailed     RequestStatus = "failed"
	StatusCancelled  RequestStatus = "cancelled"
)

// Terminal reports whether no further processing will occur.
func (s RequestStatus) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// SuggestionType is ai_suggestions.type - what kind of proposal it is. The first
// three mirror thai.IssueKind; summary is the async-only kind.
const (
	SuggestionSpelling    = "spelling"
	SuggestionPunctuation = "punctuation"
	SuggestionRepetition  = "repetition"
	// SuggestionPolish is เกลาภาษา (13Y §7) - opt-in readability advice.
	SuggestionPolish  = "polish"
	SuggestionSummary = "summary"
)

// SuggestionStatus is the writer's decision on a suggestion (docs/08 §26.1,
// docs/12 §14). Every suggestion starts pending; the writer moves it, and the
// writer alone (docs/12 §43).
type SuggestionStatus string

const (
	SuggestionPending   SuggestionStatus = "pending"
	SuggestionAccepted  SuggestionStatus = "accepted"
	SuggestionRejected  SuggestionStatus = "rejected"
	SuggestionDismissed SuggestionStatus = "dismissed"
)

// Decisions are the non-pending statuses a writer may set. Validated in the
// service so an unknown decision is a clean 422.
func decisionValid(s SuggestionStatus) bool {
	switch s {
	case SuggestionAccepted, SuggestionRejected, SuggestionDismissed:
		return true
	}
	return false
}

// Request mirrors an ai_requests row.
type Request struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	ChapterID   *uuid.UUID
	Feature     Feature
	Provider    string
	Model       *string
	Status      RequestStatus
	ErrorCode   *string
	CreatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
}

// Suggestion mirrors an ai_suggestions row.
type Suggestion struct {
	ID            uuid.UUID
	RequestID     uuid.UUID
	ChapterID     uuid.UUID
	Type          string
	OriginalText  string
	SuggestedText *string
	Explanation   *string
	Status        SuggestionStatus
	CreatedAt     time.Time
}

// ---------------------------------------------------------------------------
// API views (docs/09 §7 envelope)
// ---------------------------------------------------------------------------

// RequestView is one AI request as the API returns it. It never carries prompt
// or manuscript content - only metadata and the writer's own suggestion spans
// (docs/12 §36, docs/11 §67).
type RequestView struct {
	ID          uuid.UUID        `json:"id"`
	Feature     Feature          `json:"feature"`
	Provider    string           `json:"provider"`
	Model       *string          `json:"model,omitempty"`
	Status      RequestStatus    `json:"status"`
	ChapterID   *uuid.UUID       `json:"chapter_id,omitempty"`
	ErrorCode   *string          `json:"error_code,omitempty"`
	Retryable   bool             `json:"retryable"`
	CreatedAt   time.Time        `json:"created_at"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
	Suggestions []SuggestionView `json:"suggestions"`
}

// SuggestionView is one suggestion as the API returns it.
type SuggestionView struct {
	ID            uuid.UUID        `json:"id"`
	Type          string           `json:"type"`
	OriginalText  string           `json:"original_text"`
	SuggestedText *string          `json:"suggested_text,omitempty"`
	Explanation   *string          `json:"explanation,omitempty"`
	Status        SuggestionStatus `json:"status"`
	CreatedAt     time.Time        `json:"created_at"`
}

// View renders a request and its suggestions. suggestions may be nil (the row
// was loaded without them); it is rendered as [] so clients iterate
// unconditionally.
func (r *Request) View(suggestions []Suggestion) RequestView {
	retryable := r.Status == StatusFailed && r.ErrorCode != nil &&
		FailureKind(*r.ErrorCode).Retryable()

	views := make([]SuggestionView, 0, len(suggestions))
	for i := range suggestions {
		views = append(views, suggestions[i].View())
	}
	return RequestView{
		ID:          r.ID,
		Feature:     r.Feature,
		Provider:    r.Provider,
		Model:       r.Model,
		Status:      r.Status,
		ChapterID:   r.ChapterID,
		ErrorCode:   r.ErrorCode,
		Retryable:   retryable,
		CreatedAt:   r.CreatedAt,
		CompletedAt: r.CompletedAt,
		Suggestions: views,
	}
}

// View renders one suggestion.
func (s *Suggestion) View() SuggestionView {
	return SuggestionView{
		ID:            s.ID,
		Type:          s.Type,
		OriginalText:  s.OriginalText,
		SuggestedText: s.SuggestedText,
		Explanation:   s.Explanation,
		Status:        s.Status,
		CreatedAt:     s.CreatedAt,
	}
}
