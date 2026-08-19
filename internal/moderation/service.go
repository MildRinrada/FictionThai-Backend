package moderation

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/comments"
	"github.com/fictionthai/fictionthai/backend/internal/community"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// The *Access interfaces are the thin, consumer-defined slices of the other
// domains this service needs (the same pattern as comments.NovelAccess and
// every Notifier). Moderation NEVER re-implements a domain's visibility or
// ownership rules - it asks the owning service, which stays the single
// authorization boundary for its rows (docs/10 §27, Phase 8 brief §5).

// NovelAccess resolves fictions for reporters and applies staff state changes.
type NovelAccess interface {
	// ForReader answers "can this caller open this fiction" with the exact 404
	// non-oracle semantics readers already get (docs/11 §21).
	ForReader(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)

	// ModerateRemove / ModerateRestore toggle the soft delete, staff-gated
	// inside the novels service. Both return the fiction's author, who is the
	// moderation notification's recipient.
	ModerateRemove(ctx context.Context, identity *auth.Identity, id uuid.UUID) (uuid.UUID, error)
	ModerateRestore(ctx context.Context, identity *auth.Identity, id uuid.UUID) (uuid.UUID, error)
}

// ChapterAccess is the chapter counterpart of NovelAccess.
type ChapterAccess interface {
	ForReaderByID(ctx context.Context, identity *auth.Identity, id uuid.UUID) (*chapters.Chapter, error)
	ModerateRemove(ctx context.Context, identity *auth.Identity, id uuid.UUID) (uuid.UUID, error)
	ModerateRestore(ctx context.Context, identity *auth.Identity, id uuid.UUID) (uuid.UUID, error)
}

// CommentAccess covers fiction comments.
type CommentAccess interface {
	// VisibleForViewer answers "can this caller read this comment" - visible
	// comment on a fiction (and chapter) the caller may open; anything else is
	// the same 404.
	VisibleForViewer(ctx context.Context, identity *auth.Identity, id uuid.UUID) error

	// Moderate sets the platform status axis (docs/08 §20.1), staff-gated
	// inside the comments service, and returns the comment's author.
	Moderate(ctx context.Context, identity *auth.Identity, id uuid.UUID, status comments.Status) (uuid.UUID, error)
}

// CommunityAccess covers community posts and their comments.
type CommunityAccess interface {
	VisiblePostForViewer(ctx context.Context, identity *auth.Identity, id uuid.UUID) error
	VisibleCommentForViewer(ctx context.Context, identity *auth.Identity, id uuid.UUID) error
	ModeratePost(ctx context.Context, identity *auth.Identity, id uuid.UUID, status community.PostStatus) (uuid.UUID, error)
	ModerateComment(ctx context.Context, identity *auth.Identity, id uuid.UUID, status community.CommentStatus) (uuid.UUID, error)
}

// UserDirectory is the slice of the users repository the user-targeted
// actions need. There is no users service; account status is simple enough
// that THIS service is its authorization boundary, exactly as auth.Service is
// for passwords.
type UserDirectory interface {
	FindByID(ctx context.Context, id uuid.UUID) (*users.User, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status users.Status) error
	InvalidateSessionsBefore(ctx context.Context, id uuid.UUID, at time.Time) error
}

// MediaAccess covers uploaded files (docs/11 §38 - media is reportable;
// Phase 9). Removal keeps the stored object, so restore is lossless.
type MediaAccess interface {
	VisibleForViewer(ctx context.Context, identity *auth.Identity, id uuid.UUID) error
	ModerateRemove(ctx context.Context, identity *auth.Identity, id uuid.UUID) (uuid.UUID, error)
	ModerateRestore(ctx context.Context, identity *auth.Identity, id uuid.UUID) (uuid.UUID, error)
}

// Notifier is the slice of the notifications domain this service needs -
// docs/01 §26 "Moderation notification", delivered through the EXISTING
// queue and worker (Phase 8 brief §13). Fire-and-forget: the action has
// already committed.
type Notifier interface {
	// ModerationActionTaken tells recipientID that targetType/targetID was
	// acted on. actorID is used only for the never-notify-yourself rule and is
	// NEVER stored or shown: sanctioned users must not learn which individual
	// moderator acted (docs/11 §39).
	ModerationActionTaken(ctx context.Context, actorID, recipientID uuid.UUID, targetType string, targetID uuid.UUID)
}

// Service owns report and audit business rules and is the authorization
// boundary for every moderation endpoint (docs/10 §27).
type Service struct {
	repo      *Repository
	novels    NovelAccess
	chapters  ChapterAccess
	comments  CommentAccess
	community CommunityAccess
	users     UserDirectory
	media     MediaAccess
	notifier  Notifier
	log       *slog.Logger
}

// NewService wires the service. notifier may be nil: actions then simply
// emit nothing. mediaAccess may be nil in tests that predate Phase 9; media
// targets then answer 404.
func NewService(
	repo *Repository,
	novelAccess NovelAccess, chapterAccess ChapterAccess,
	commentAccess CommentAccess, communityAccess CommunityAccess,
	userDirectory UserDirectory, mediaAccess MediaAccess,
	notifier Notifier, log *slog.Logger,
) *Service {
	return &Service{
		repo:      repo,
		novels:    novelAccess,
		chapters:  chapterAccess,
		comments:  commentAccess,
		community: communityAccess,
		users:     userDirectory,
		media:     mediaAccess,
		notifier:  notifier,
		log:       log,
	}
}

func reportNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "REPORT_NOT_FOUND", "Report not found.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// requireStaff is defense in depth behind middleware.RequireStaff: the
// service must hold even if a future route forgets the middleware
// (docs/10 §27 - the service layer is the boundary).
func requireStaff(identity *auth.Identity) (uuid.UUID, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return uuid.Nil, err
	}
	if !identity.IsStaff() {
		return uuid.Nil, apierror.Forbidden("You do not have permission to do that.")
	}
	return userID, nil
}

// ---------------------------------------------------------------------------
// Reports - the user side (docs/09 §28, docs/02 §38)
// ---------------------------------------------------------------------------

// ReportInput is the client's report request.
type ReportInput struct {
	TargetType  string
	TargetID    string
	Reason      string
	Description string
}

// CreateReport files a report against a target the reporter can actually see.
//
// The visibility check is the anti-oracle rule (Phase 8 brief §4): resolving
// the target goes through the owning domain's READER path, so reporting a
// private or hidden object answers exactly like fetching it - the same 404,
// revealing nothing (docs/11 §21, §31).
//
// Filing again while a previous report on the same target is still open
// returns the existing report unchanged (created=false → HTTP 200, not 201).
func (s *Service) CreateReport(
	ctx context.Context, identity *auth.Identity, input ReportInput,
) (*View, bool, error) {
	reporterID, err := requireUser(identity)
	if err != nil {
		return nil, false, err
	}

	fields := map[string][]string{}
	if !ValidTargetType(input.TargetType) {
		fields["target_type"] = []string{"Unknown target type."}
	}
	targetID, idErr := uuid.Parse(strings.TrimSpace(input.TargetID))
	if idErr != nil {
		fields["target_id"] = []string{"A valid target id is required."}
	}
	if !ValidReason(input.Reason) {
		fields["reason"] = []string{"Unknown report reason."}
	}
	description := strings.TrimSpace(input.Description)
	if utf8.RuneCountInString(description) > MaxDescriptionRunes {
		fields["description"] = []string{"A description cannot be longer than 2000 characters."}
	}
	if len(fields) > 0 {
		return nil, false, apierror.Validation(fields)
	}

	if err := s.resolveTargetForReporter(ctx, identity, TargetType(input.TargetType), targetID); err != nil {
		return nil, false, err
	}

	var descPtr *string
	if description != "" {
		descPtr = &description
	}

	report, created, err := s.repo.Create(ctx, CreateParams{
		ReporterID:  reporterID,
		TargetType:  TargetType(input.TargetType),
		TargetID:    targetID,
		Reason:      Reason(input.Reason),
		Description: descPtr,
	})
	if err != nil {
		return nil, false, s.internal("create report", err)
	}

	view := report.Render()
	return &view, created, nil
}

// resolveTargetForReporter verifies the target exists AND is visible to the
// reporter, through the owning domain's own reader rules.
func (s *Service) resolveTargetForReporter(
	ctx context.Context, identity *auth.Identity, targetType TargetType, id uuid.UUID,
) error {
	switch targetType {
	case TargetNovel:
		_, err := s.novels.ForReader(ctx, identity, novels.Ref{ID: id})
		return err
	case TargetChapter:
		_, err := s.chapters.ForReaderByID(ctx, identity, id)
		return err
	case TargetComment:
		return s.comments.VisibleForViewer(ctx, identity, id)
	case TargetCommunityPost:
		return s.community.VisiblePostForViewer(ctx, identity, id)
	case TargetCommunityComment:
		return s.community.VisibleCommentForViewer(ctx, identity, id)
	case TargetUser:
		// Every live account is publicly visible through /author/{username};
		// only a deleted account is a 404.
		if _, err := s.users.FindByID(ctx, id); err != nil {
			if errors.Is(err, users.ErrNotFound) {
				return apierror.NotFound("User not found.")
			}
			return s.internal("resolve reported user", err)
		}
		return nil
	case TargetMedia:
		if s.media == nil {
			return apierror.NotFound("Media not found.")
		}
		return s.media.VisibleForViewer(ctx, identity, id)
	default:
		return apierror.Validation(map[string][]string{
			"target_type": {"Unknown target type."},
		})
	}
}

// MyReports returns one page of the caller's own reports, newest first. Only
// ever the caller's - someone else's reports are not reachable through any
// non-staff surface (docs/11 §31).
func (s *Service) MyReports(
	ctx context.Context, identity *auth.Identity, page pagination.Params,
) ([]View, pagination.Meta, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	items, total, err := s.repo.ListForReporter(ctx, userID, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list reports", err)
	}

	views := make([]View, 0, len(items))
	for i := range items {
		views = append(views, items[i].Render())
	}
	return views, page.MetaFor(total), nil
}

// ---------------------------------------------------------------------------
// Reports - the staff side (docs/09 §29)
// ---------------------------------------------------------------------------

// QueueQuery is the validated shape of GET /admin/reports.
type QueueQuery struct {
	Status     string
	TargetType string
}

// Queue returns one page of the moderation queue, oldest first.
func (s *Service) Queue(
	ctx context.Context, identity *auth.Identity, query QueueQuery, page pagination.Params,
) ([]ModeratorView, pagination.Meta, error) {
	if _, err := requireStaff(identity); err != nil {
		return nil, pagination.Meta{}, err
	}

	fields := map[string][]string{}
	if query.Status != "" && !ValidStatus(query.Status) {
		fields["status"] = []string{"Unknown report status."}
	}
	if query.TargetType != "" && !ValidTargetType(query.TargetType) {
		fields["target_type"] = []string{"Unknown target type."}
	}
	if len(fields) > 0 {
		return nil, pagination.Meta{}, apierror.Validation(fields)
	}

	items, total, err := s.repo.AdminList(ctx, AdminFilter{
		Status:     Status(query.Status),
		TargetType: TargetType(query.TargetType),
	}, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list report queue", err)
	}

	views := make([]ModeratorView, 0, len(items))
	for i := range items {
		views = append(views, items[i].RenderForModerator())
	}
	return views, page.MetaFor(total), nil
}

// GetReport returns the staff detail: the report, a live snapshot of its
// target, the target's audit history, and the actions the target supports.
func (s *Service) GetReport(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (*ReportDetail, error) {
	if _, err := requireStaff(identity); err != nil {
		return nil, err
	}

	report, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, reportNotFound()
	}
	if err != nil {
		return nil, s.internal("load report", err)
	}

	snapshot, err := s.repo.Snapshot(ctx, report.TargetType, report.TargetID)
	if err != nil {
		return nil, s.internal("snapshot target", err)
	}

	history, _, err := s.repo.ListActions(ctx, ActionFilter{
		TargetType: report.TargetType,
		TargetID:   report.TargetID,
	}, pagination.Params{Page: 1, PerPage: 20})
	if err != nil {
		return nil, s.internal("load target history", err)
	}

	historyViews := make([]ActionView, 0, len(history))
	for i := range history {
		historyViews = append(historyViews, history[i].Render())
	}

	return &ReportDetail{
		Report:  report.RenderForModerator(),
		Target:  snapshot,
		History: historyViews,
		Actions: ActionsFor(report.TargetType),
	}, nil
}

// UpdateReport moves a report along the documented lifecycle.
//
// An unknown status is a validation error; a legal status that this report
// cannot move to from where it is now is a 409 - the state machine, not the
// vocabulary, said no (Phase 8 brief §12).
func (s *Service) UpdateReport(
	ctx context.Context, identity *auth.Identity, id uuid.UUID, status string,
) (*ModeratorView, error) {
	actorID, err := requireStaff(identity)
	if err != nil {
		return nil, err
	}

	if !ValidStatus(status) {
		return nil, apierror.Validation(map[string][]string{
			"status": {"Unknown report status."},
		})
	}
	to := Status(status)

	// The legal FROM states for this destination, from the transition table.
	from := make([]Status, 0, 2)
	for source, targets := range transitions {
		for _, t := range targets {
			if t == to {
				from = append(from, source)
			}
		}
	}
	if len(from) == 0 {
		// "pending" is a valid status but never a destination.
		return nil, apierror.Conflict("A report cannot move to that status.")
	}

	moved, err := s.repo.Transition(ctx, id, to, from, actorID)
	if err != nil {
		return nil, s.internal("transition report", err)
	}

	report, err := s.repo.Find(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, reportNotFound()
	}
	if err != nil {
		return nil, s.internal("load report", err)
	}

	if !moved {
		// The row exists but was not in a legal FROM state.
		return nil, apierror.Conflict("The report has already moved past that status.")
	}

	view := report.RenderForModerator()
	return &view, nil
}

// ---------------------------------------------------------------------------
// Moderation actions (docs/08 §24.2, docs/02 §46)
// ---------------------------------------------------------------------------

// ActionInput is the staff action request.
type ActionInput struct {
	TargetType string
	TargetID   string
	Action     string
	Reason     string
}

// PerformAction executes one moderation action: the state change through the
// owning domain's service, then the append-only audit row, then the
// moderation notification to the affected user (docs/01 §26).
//
// The audit insert follows the state change; if it fails the action is
// reported as failed loudly (500) while the state change stands - re-running
// then answers 409 from the domain, which tells the moderator the change
// already applied. That trade keeps domain authorization in the domain
// services rather than pulling their writes into one cross-package
// transaction.
func (s *Service) PerformAction(
	ctx context.Context, identity *auth.Identity, input ActionInput,
) (*ActionView, error) {
	moderatorID, err := requireStaff(identity)
	if err != nil {
		return nil, err
	}

	fields := map[string][]string{}
	if !ValidTargetType(input.TargetType) {
		fields["target_type"] = []string{"Unknown target type."}
	}
	targetID, idErr := uuid.Parse(strings.TrimSpace(input.TargetID))
	if idErr != nil {
		fields["target_id"] = []string{"A valid target id is required."}
	}
	if !ValidAction(input.Action) {
		fields["action"] = []string{"Unknown moderation action."}
	}
	reason := strings.TrimSpace(input.Reason)
	if utf8.RuneCountInString(reason) > MaxActionReasonRunes {
		fields["reason"] = []string{"A reason cannot be longer than 2000 characters."}
	}
	if len(fields) > 0 {
		return nil, apierror.Validation(fields)
	}

	targetType := TargetType(input.TargetType)
	action := Action(input.Action)
	if !ActionAllowed(targetType, action) {
		return nil, apierror.Validation(map[string][]string{
			"action": {"That action does not apply to this target type."},
		})
	}

	recipientID, err := s.applyAction(ctx, identity, targetType, targetID, action)
	if err != nil {
		return nil, err
	}

	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	record, err := s.repo.InsertAction(ctx, ActionParams{
		ModeratorID: moderatorID,
		TargetType:  targetType,
		TargetID:    targetID,
		Action:      action,
		Reason:      reasonPtr,
	})
	if err != nil {
		// The state change applied but the audit row did not - surface it as a
		// failure so it cannot pass silently (docs/11 §39: the log IS part of
		// the security model).
		return nil, s.internal("record moderation action", err)
	}

	if s.notifier != nil && recipientID != uuid.Nil {
		s.notifier.ModerationActionTaken(ctx, moderatorID, recipientID, string(targetType), targetID)
	}

	view := record.Render()
	return &view, nil
}

// applyAction routes the state change to the owning domain and returns who
// should be notified.
func (s *Service) applyAction(
	ctx context.Context, identity *auth.Identity,
	targetType TargetType, targetID uuid.UUID, action Action,
) (uuid.UUID, error) {
	switch targetType {
	case TargetNovel:
		if action == ActionRemove {
			return s.novels.ModerateRemove(ctx, identity, targetID)
		}
		return s.novels.ModerateRestore(ctx, identity, targetID)

	case TargetChapter:
		if action == ActionRemove {
			return s.chapters.ModerateRemove(ctx, identity, targetID)
		}
		return s.chapters.ModerateRestore(ctx, identity, targetID)

	case TargetComment:
		return s.comments.Moderate(ctx, identity, targetID, commentStatusFor(action))

	case TargetCommunityPost:
		return s.community.ModeratePost(ctx, identity, targetID, postStatusFor(action))

	case TargetCommunityComment:
		return s.community.ModerateComment(ctx, identity, targetID, communityCommentStatusFor(action))

	case TargetUser:
		return s.moderateUser(ctx, identity, targetID, action)

	case TargetMedia:
		if s.media == nil {
			return uuid.Nil, apierror.NotFound("Media not found.")
		}
		if action == ActionRemove {
			return s.media.ModerateRemove(ctx, identity, targetID)
		}
		return s.media.ModerateRestore(ctx, identity, targetID)
	}
	// Unreachable: the action matrix already validated the pair.
	return uuid.Nil, apierror.Validation(map[string][]string{
		"target_type": {"Unknown target type."},
	})
}

// The action → domain-status maps. Restore returns content to its domain's
// normal state; it never touches deleted_at, which belongs to the author
// (docs/08 §20.1's three independent axes).
func commentStatusFor(action Action) comments.Status {
	switch action {
	case ActionHide:
		return comments.StatusHidden
	case ActionRemove:
		return comments.StatusRemoved
	default:
		return comments.StatusVisible
	}
}

func postStatusFor(action Action) community.PostStatus {
	switch action {
	case ActionHide:
		return community.PostStatusHidden
	case ActionRemove:
		return community.PostStatusRemoved
	default:
		return community.PostStatusPublished
	}
}

func communityCommentStatusFor(action Action) community.CommentStatus {
	switch action {
	case ActionHide:
		return community.CommentStatusHidden
	case ActionRemove:
		return community.CommentStatusRemoved
	default:
		return community.CommentStatusVisible
	}
}

// moderateUser applies a user-targeted action against the EXISTING account
// lifecycle (docs/10 §18): warn changes nothing, suspend/ban move status,
// restore returns the account to the state its email verification implies.
//
// The rank guard is role-escalation prevention (Phase 8 brief §15, docs/10):
// a moderator sanctioning an admin - or another moderator - would be
// authority they do not hold, so an actor may only sanction accounts BELOW
// their own role: moderators act on users; admins act on users and
// moderators; nobody acts on admins (role changes are operational, not an
// API).
func (s *Service) moderateUser(
	ctx context.Context, identity *auth.Identity, targetID uuid.UUID, action Action,
) (uuid.UUID, error) {
	target, err := s.users.FindByID(ctx, targetID)
	if errors.Is(err, users.ErrNotFound) {
		return uuid.Nil, apierror.NotFound("User not found.")
	}
	if err != nil {
		return uuid.Nil, s.internal("load target user", err)
	}
	if target.Status == users.StatusDeleted {
		return uuid.Nil, apierror.NotFound("User not found.")
	}

	if !outranks(identity, target.Role) {
		return uuid.Nil, apierror.Forbidden("You cannot moderate this account.")
	}

	switch action {
	case ActionWarn:
		// A warning is the audit row plus the notification - no state change.
		return target.ID, nil

	case ActionSuspend:
		if target.Status == users.StatusSuspended || target.Status == users.StatusBanned {
			return uuid.Nil, apierror.Conflict("The account is already restricted.")
		}
		return target.ID, s.setUserStatus(ctx, target.ID, users.StatusSuspended)

	case ActionBan:
		if target.Status == users.StatusBanned {
			return uuid.Nil, apierror.Conflict("The account is already banned.")
		}
		return target.ID, s.setUserStatus(ctx, target.ID, users.StatusBanned)

	case ActionRestore:
		if target.Status != users.StatusSuspended && target.Status != users.StatusBanned {
			return uuid.Nil, apierror.Conflict("The account is not restricted.")
		}
		restored := users.StatusActive
		if !target.EmailVerified() {
			restored = users.StatusPendingVerification
		}
		if err := s.users.UpdateStatus(ctx, target.ID, restored); err != nil {
			return uuid.Nil, s.internal("restore account", err)
		}
		return target.ID, nil
	}

	// Unreachable: the action matrix already validated the pair.
	return uuid.Nil, apierror.Validation(map[string][]string{
		"action": {"That action does not apply to this target type."},
	})
}

// setUserStatus applies a restriction and cuts every live session. The
// per-request status check in auth already locks the account out; the session
// cutoff is defense in depth (docs/10 §37).
func (s *Service) setUserStatus(ctx context.Context, id uuid.UUID, status users.Status) error {
	if err := s.users.UpdateStatus(ctx, id, status); err != nil {
		return s.internal("update account status", err)
	}
	if err := s.users.InvalidateSessionsBefore(ctx, id, time.Now()); err != nil {
		return s.internal("invalidate sessions", err)
	}
	return nil
}

// outranks reports whether the actor's role is strictly above the target's.
func outranks(identity *auth.Identity, target users.Role) bool {
	switch {
	case identity.HasRole(users.RoleAdmin):
		return target != users.RoleAdmin
	case identity.HasRole(users.RoleModerator):
		return target == users.RoleUser
	default:
		return false
	}
}

// ActionsQuery is the validated shape of GET /admin/moderation/actions.
type ActionsQuery struct {
	TargetType string
	TargetID   string
}

// Actions returns one page of the audit trail, newest first.
func (s *Service) Actions(
	ctx context.Context, identity *auth.Identity, query ActionsQuery, page pagination.Params,
) ([]ActionView, pagination.Meta, error) {
	if _, err := requireStaff(identity); err != nil {
		return nil, pagination.Meta{}, err
	}

	fields := map[string][]string{}
	if query.TargetType != "" && !ValidTargetType(query.TargetType) {
		fields["target_type"] = []string{"Unknown target type."}
	}
	var targetID uuid.UUID
	if query.TargetID != "" {
		parsed, err := uuid.Parse(strings.TrimSpace(query.TargetID))
		if err != nil {
			fields["target_id"] = []string{"A valid target id is required."}
		} else {
			targetID = parsed
		}
	}
	if len(fields) > 0 {
		return nil, pagination.Meta{}, apierror.Validation(fields)
	}

	items, total, err := s.repo.ListActions(ctx, ActionFilter{
		TargetType: TargetType(query.TargetType),
		TargetID:   targetID,
	}, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list moderation actions", err)
	}

	views := make([]ActionView, 0, len(items))
	for i := range items {
		views = append(views, items[i].Render())
	}
	return views, page.MetaFor(total), nil
}

// internal logs the real failure and returns the opaque error (docs/11 §67).
func (s *Service) internal(op string, err error) error {
	s.log.Error("moderation service failure", slog.String("op", op), slog.Any("error", err))
	return apierror.Internal()
}
