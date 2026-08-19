package library

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// NovelAccess is the slice of the novels domain this service needs for
// visibility decisions. Every operation that ADDS shelf state starts by asking
// the novels service whether the caller may read the fiction - the gate is
// applied exactly once, in the one place that owns it (docs/11 §21).
type NovelAccess interface {
	ForReader(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
}

// NovelStore is the slice of the novels repository this service needs.
//
// Find is the visibility-free resolver, used ONLY for removal: taking a
// bookmark off the shelf must keep working after the fiction goes private, and
// an idempotent 204 reveals nothing either way. RecordsByIDs batch-loads the
// cards for one page of shelf rows.
type NovelStore interface {
	Find(ctx context.Context, ref novels.Ref) (*novels.Novel, error)
	RecordsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]novels.Record, error)
}

// UserLookup resolves a follow target. Soft-deleted accounts are already
// excluded by the users repository.
type UserLookup interface {
	FindByID(ctx context.Context, id uuid.UUID) (*users.User, error)
}

// Notifier is the slice of the notifications domain this service needs
// (docs/07 §38 "NovelFollowed"). Consumer-defined so the dependency stays
// one-directional; fire-and-forget because the follow has already committed.
type Notifier interface {
	FollowerAdded(ctx context.Context, followerID, followingID uuid.UUID)
}

// Service owns the shelf's business rules and is the authorization boundary
// for /me/*, bookmark, follow, and progress endpoints (docs/10 §27).
type Service struct {
	repo     *Repository
	novels   NovelAccess
	records  NovelStore
	users    UserLookup
	notifier Notifier
	log      *slog.Logger
}

// NewService wires the service. notifier may be nil (tests that predate
// Phase 6): following then simply emits nothing.
func NewService(
	repo *Repository, novelAccess NovelAccess, novelStore NovelStore,
	userLookup UserLookup, notifier Notifier, log *slog.Logger,
) *Service {
	return &Service{
		repo: repo, novels: novelAccess, records: novelStore,
		users: userLookup, notifier: notifier, log: log,
	}
}

func userNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "USER_NOT_FOUND", "User not found.")
}

func progressNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "READING_PROGRESS_NOT_FOUND",
		"No reading progress for this fiction yet.")
}

// requireUser is the guard on every method here. The routes also carry
// RequireAuth, but the service must not depend on being mounted correctly
// (docs/10 §27 - the service layer is the boundary).
func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// isOwner mirrors the novels service's canManage for rendering cards.
func isOwner(identity *auth.Identity, novel *novels.Novel) bool {
	if !identity.Authenticated() {
		return false
	}
	return novel.OwnedBy(identity.UserID()) || identity.IsStaff()
}

// ---------------------------------------------------------------------------
// Bookmarks
// ---------------------------------------------------------------------------

// Bookmark saves a fiction to the caller's shelf (docs/09 §18).
//
// The caller must be able to READ the fiction to save it - ForReader answers
// 404 for anything else, so this endpoint cannot be used to probe private
// slugs. Repeats are idempotent (docs/09 §33).
func (s *Service) Bookmark(ctx context.Context, identity *auth.Identity, ref novels.Ref) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return err
	}
	if err := s.repo.UpsertBookmark(ctx, userID, novel.ID); err != nil {
		return s.internal("bookmark", err)
	}
	return nil
}

// Unbookmark removes a fiction from the caller's shelf.
//
// Deliberately NOT gated on readability: users must always be able to remove
// items from their library (docs/01 §11), including a fiction that has since
// gone private or been deleted. The response is 204 whether or not anything
// existed, so nothing about the fiction is confirmed either way.
func (s *Service) Unbookmark(ctx context.Context, identity *auth.Identity, ref novels.Ref) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	novel, err := s.records.Find(ctx, ref)
	if errors.Is(err, novels.ErrNotFound) {
		return nil // removing what is not there is a successful no-op
	}
	if err != nil {
		return s.internal("resolve novel", err)
	}
	if err := s.repo.DeleteBookmark(ctx, userID, novel.ID); err != nil {
		return s.internal("unbookmark", err)
	}
	return nil
}

// BookmarkStatus reports whether the caller has saved this fiction. It exists
// so the fiction page can render its bookmark control without fetching the
// whole library (docs/02 §5.2).
func (s *Service) BookmarkStatus(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
) (bool, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return false, err
	}
	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return false, err
	}
	saved, err := s.repo.IsBookmarked(ctx, userID, novel.ID)
	if err != nil {
		return false, s.internal("check bookmark", err)
	}
	return saved, nil
}

// ---------------------------------------------------------------------------
// Fiction likes (docs/01 §20.2, docs/PHASE-12-STORY-DEPTH.md §12C)
// ---------------------------------------------------------------------------

// Like records that the caller likes this fiction.
//
// The same gate as a bookmark: the caller must be able to READ the fiction, so
// the endpoint cannot be used to probe private slugs, and repeats are
// idempotent. A like is a lightweight reaction (docs/01 §20.2), not a shelf
// action - it is deliberately separate from the bookmark it sits beside.
func (s *Service) Like(ctx context.Context, identity *auth.Identity, ref novels.Ref) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return err
	}
	if err := s.repo.UpsertReaction(ctx, userID, novel.ID); err != nil {
		return s.internal("like", err)
	}
	return nil
}

// Unlike withdraws a like.
//
// Ungated for the same reason unbookmarking is: taking something back must
// always work, including for a fiction that has since gone private.
func (s *Service) Unlike(ctx context.Context, identity *auth.Identity, ref novels.Ref) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	novel, err := s.records.Find(ctx, ref)
	if errors.Is(err, novels.ErrNotFound) {
		return nil
	}
	if err != nil {
		return s.internal("resolve novel", err)
	}
	if err := s.repo.DeleteReaction(ctx, userID, novel.ID); err != nil {
		return s.internal("unlike", err)
	}
	return nil
}

// LikeStatus reports whether the caller has liked this fiction, so the fiction
// page can render its own control without loading anything else.
func (s *Service) LikeStatus(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
) (bool, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return false, err
	}
	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return false, err
	}
	liked, err := s.repo.HasReacted(ctx, userID, novel.ID)
	if err != nil {
		return false, s.internal("check reaction", err)
	}
	return liked, nil
}

// Library returns one page of the caller's shelf (docs/09 §18 "My Library").
//
// status optionally narrows to one fiction status - the library's "Completed"
// section is ?status=completed (docs/03 §13).
func (s *Service) Library(
	ctx context.Context, identity *auth.Identity, status string, page pagination.Params,
) ([]Entry, pagination.Meta, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	if status != "" && !novels.Status(status).Valid() {
		return nil, pagination.Meta{}, apierror.Validation(map[string][]string{
			"status": {"Unsupported value."},
		})
	}

	rows, total, err := s.repo.ListBookmarks(ctx, userID, status, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list bookmarks", err)
	}

	records, err := s.novelCards(ctx, rowIDs(rows, func(r bookmarkRow) uuid.UUID { return r.NovelID }))
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	entries := make([]Entry, 0, len(rows))
	for _, row := range rows {
		record, ok := records[row.NovelID]
		if !ok {
			continue // the fiction vanished between the two queries
		}
		entries = append(entries, Entry{
			Novel:        record.ViewFor(isOwner(identity, &record.Novel)),
			BookmarkedAt: row.CreatedAt.Time,
		})
	}
	return entries, page.MetaFor(total), nil
}

// ---------------------------------------------------------------------------
// Follows
// ---------------------------------------------------------------------------

// Follow records that the caller follows another user (docs/09 §19).
func (s *Service) Follow(ctx context.Context, identity *auth.Identity, targetID uuid.UUID) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	if targetID == userID {
		return apierror.Validation(map[string][]string{
			"user_id": {"You cannot follow yourself."},
		})
	}
	if _, err := s.users.FindByID(ctx, targetID); err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return userNotFound()
		}
		return s.internal("resolve follow target", err)
	}
	inserted, err := s.repo.UpsertFollow(ctx, userID, targetID)
	if err != nil {
		return s.internal("follow", err)
	}

	// Only a NEW follow notifies - an idempotent repeat inserted nothing, so it
	// emits nothing, and unfollow/refollow cycles cannot spam the author.
	if inserted && s.notifier != nil {
		s.notifier.FollowerAdded(ctx, userID, targetID)
	}
	return nil
}

// Unfollow removes a follow. Idempotent, and deliberately not gated on the
// target still existing - unfollowing a deleted account must work.
func (s *Service) Unfollow(ctx context.Context, identity *auth.Identity, targetID uuid.UUID) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteFollow(ctx, userID, targetID); err != nil {
		return s.internal("unfollow", err)
	}
	return nil
}

// FollowStatusFor answers GET /users/:user/follow-status (docs/09 §19).
func (s *Service) FollowStatusFor(
	ctx context.Context, identity *auth.Identity, targetID uuid.UUID,
) (*FollowStatus, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	if _, err := s.users.FindByID(ctx, targetID); err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, userNotFound()
		}
		return nil, s.internal("resolve follow target", err)
	}
	following, err := s.repo.IsFollowing(ctx, userID, targetID)
	if err != nil {
		return nil, s.internal("check follow", err)
	}
	return &FollowStatus{IsFollowing: following}, nil
}

// Following returns one page of the authors the caller follows - the library's
// "Following" section (docs/03 §13).
func (s *Service) Following(
	ctx context.Context, identity *auth.Identity, page pagination.Params,
) ([]FollowedAuthor, pagination.Meta, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	entries, total, err := s.repo.ListFollowing(ctx, userID, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list following", err)
	}
	return entries, page.MetaFor(total), nil
}

// ---------------------------------------------------------------------------
// Reading progress
// ---------------------------------------------------------------------------

// ProgressInput is a validated position save.
type ProgressInput struct {
	ChapterID       uuid.UUID
	ProgressPercent float64
}

// SaveProgress upserts the caller's position in one fiction (docs/09 §17).
//
// The chapter must belong to THIS fiction and be one the caller could actually
// read - the same predicates the read path uses, so progress can never be
// attached across fictions (IDOR) or point at a chapter its owner has not
// published (docs/11 §21).
func (s *Service) SaveProgress(
	ctx context.Context, identity *auth.Identity, ref novels.Ref, input ProgressInput,
) (*Progress, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	errs := map[string][]string{}
	if input.ChapterID == uuid.Nil {
		errs["chapter_id"] = []string{"A chapter is required."}
	}
	if input.ProgressPercent < 0 || input.ProgressPercent > 100 {
		errs["progress_percent"] = []string{"Must be between 0 and 100."}
	}
	if len(errs) > 0 {
		return nil, apierror.Validation(errs)
	}

	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	resumable, err := s.repo.ChapterResumable(ctx, userID, novel.ID, input.ChapterID)
	if err != nil {
		return nil, s.internal("check chapter", err)
	}
	if !resumable {
		// From this caller's point of view the chapter does not exist in this
		// fiction, whether it is absent, another fiction's, or unpublished - one
		// answer for all three (docs/11 §3.4).
		return nil, apierror.Validation(map[string][]string{
			"chapter_id": {"Unknown chapter for this fiction."},
		})
	}

	progress, err := s.repo.SaveProgress(ctx, userID, novel.ID, input.ChapterID, input.ProgressPercent)
	if err != nil {
		return nil, s.internal("save progress", err)
	}

	// The history row rides the same save (library review 2026-08). The
	// opt-out gate lives INSIDE the statement, and a failure here must never
	// fail the position save - history is a convenience, the place is the
	// contract.
	if err := s.repo.RecordEvent(ctx, userID, novel.ID, input.ChapterID); err != nil {
		s.log.Error("library: record reading event failed", slog.Any("error", err))
	}
	return progress, nil
}

// GetProgress returns the caller's position in one fiction (docs/09 §17).
func (s *Service) GetProgress(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
) (*Progress, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return nil, err
	}
	progress, err := s.repo.GetProgress(ctx, userID, novel.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, progressNotFound()
	}
	if err != nil {
		return nil, s.internal("get progress", err)
	}
	return progress, nil
}

// ContinueReading returns one page of the caller's most recent positions
// (docs/09 §17 GET /me/reading-progress, docs/08 §18.1).
func (s *Service) ContinueReading(
	ctx context.Context, identity *auth.Identity, page pagination.Params,
) ([]ContinueReading, pagination.Meta, error) {
	_, err := requireUser(identity)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	rows, total, err := s.repo.ListProgress(ctx, identity.UserID(), page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list progress", err)
	}

	records, err := s.novelCards(ctx, rowIDs(rows, func(r progressRow) uuid.UUID { return r.NovelID }))
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	entries := make([]ContinueReading, 0, len(rows))
	for _, row := range rows {
		record, ok := records[row.NovelID]
		if !ok {
			continue
		}

		entry := ContinueReading{
			Novel:           record.ViewFor(isOwner(identity, &record.Novel)),
			ProgressPercent: row.ProgressPercent,
			LastReadAt:      row.LastReadAt.Time,
			TotalChapters:   row.TotalChapters,
			ChaptersLeft:    row.ChaptersLeft,
			NewSinceRead:    row.NewSinceRead,
		}
		if row.ChapterID.Valid {
			chapter := ChapterRef{
				ID:            row.ChapterID.UUID,
				ChapterNumber: int(row.ChapterNumber.Int64),
				Slug:          row.ChapterSlug.String,
			}
			if row.ChapterTitle.Valid {
				title := row.ChapterTitle.String
				chapter.Title = &title
			}
			entry.Chapter = &chapter
		}
		entries = append(entries, entry)
	}
	return entries, page.MetaFor(total), nil
}

// novelCards batch-loads the fiction cards for one page of shelf rows.
func (s *Service) novelCards(
	ctx context.Context, ids []uuid.UUID,
) (map[uuid.UUID]novels.Record, error) {
	records, err := s.records.RecordsByIDs(ctx, ids)
	if err != nil {
		return nil, s.internal("load novel cards", err)
	}
	return records, nil
}

// rowIDs projects the novel id out of a page of shelf rows.
func rowIDs[T any](rows []T, id func(T) uuid.UUID) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, id(row))
	}
	return ids
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("library: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

// Compile-time assurance that the real collaborators satisfy the narrow
// interfaces declared above.
var (
	_ NovelAccess = (*novels.Service)(nil)
	_ NovelStore  = (*novels.Repository)(nil)
	_ UserLookup  = (*users.Repository)(nil)
)
