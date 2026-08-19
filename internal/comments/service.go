package comments

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// MaxContentRunes bounds one comment. Runes, not bytes: Thai text is three
// bytes per character in UTF-8 and must not get a third of the room an
// English comment gets (same rationale as every other text limit here).
const MaxContentRunes = 5000

// NovelAccess is the slice of the novels domain this service needs. Reading
// the fiction is the gate for BOTH reading and writing comments: if you may
// not open a fiction, you may not see or join its discussion, and the 404
// reveals nothing (docs/11 §21, §31).
//
// ForWriter is the review queue's gate: approving a comment is an act of
// authority over the FICTION, so the decision reuses the same ownership rule
// every other write on that fiction goes through rather than growing a second
// one here (docs/10 §27).
type NovelAccess interface {
	ForReader(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
	ForWriter(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
}

// ChapterAccess resolves a chapter under the reader visibility rule, so a
// comment can only anchor to a chapter its author could actually read.
type ChapterAccess interface {
	ResolveForReader(
		ctx context.Context, identity *auth.Identity,
		novelRef novels.Ref, chapterRef chapters.Ref,
	) (*novels.Novel, *chapters.Chapter, error)
}

// Notifier is the slice of the notifications domain this service needs
// (docs/07 §38 "CommentCreated"). Fire-and-forget: the comment has already
// committed when it is called.
type Notifier interface {
	CommentCreated(ctx context.Context, actorID, commentID uuid.UUID)
}

// Service owns comment business rules and is the authorization boundary for
// every comment endpoint (docs/10 §27).
type Service struct {
	repo     *Repository
	novels   NovelAccess
	chapters ChapterAccess
	notifier Notifier
	log      *slog.Logger
}

// NewService wires the service. notifier may be nil: commenting then simply
// emits nothing.
func NewService(
	repo *Repository, novelAccess NovelAccess, chapterAccess ChapterAccess,
	notifier Notifier, log *slog.Logger,
) *Service {
	return &Service{
		repo: repo, novels: novelAccess, chapters: chapterAccess,
		notifier: notifier, log: log,
	}
}

func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "COMMENT_NOT_FOUND", "Comment not found.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// validateContent trims and bounds the text. The trimmed form is what gets
// stored - trailing whitespace is never meaningful in a comment.
func validateContent(raw string) (string, error) {
	content := strings.TrimSpace(raw)
	if content == "" {
		return "", apierror.Validation(map[string][]string{
			"content": {"A comment cannot be empty."},
		})
	}
	if utf8.RuneCountInString(content) > MaxContentRunes {
		return "", apierror.Validation(map[string][]string{
			"content": {"A comment cannot be longer than 5000 characters."},
		})
	}
	return content, nil
}

// viewerID is uuid.Nil for guests; used only to mark is_owner on views.
func viewerID(identity *auth.Identity) uuid.UUID {
	if !identity.Authenticated() {
		return uuid.Nil
	}
	return identity.UserID()
}

// canModerate implements docs/09 §20 "the comment owner or authorized
// moderator": the author of the comment, or staff.
//
// A GUEST comment has no owner clause at all - WrittenBy is false for every
// caller - so only staff and the review queue can act on one. That is the
// honest consequence of storing no guest identity: nobody can prove they wrote
// it, so nobody is allowed to claim they did (§13D).
func canModerate(identity *auth.Identity, comment *Comment) bool {
	if !identity.Authenticated() {
		return false
	}
	return comment.WrittenBy(identity.UserID()) || identity.IsStaff()
}

// render maps a page of records to views.
func render(items []Comment, viewer uuid.UUID) []View {
	views := make([]View, 0, len(items))
	for i := range items {
		views = append(views, items[i].Render(viewer))
	}
	return views
}

// withLikes enriches one page of comments with their hearts (comment design
// review 2026-08): two batched queries, never one per row. A failure here
// degrades to zero counts rather than taking the thread down - the words
// matter more than the score.
func (s *Service) withLikes(ctx context.Context, items []Comment, viewer uuid.UUID) {
	ids := make([]uuid.UUID, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	counts, mine, err := s.repo.LikeStats(ctx, ids, viewer)
	if err != nil {
		s.log.Error("comments: like stats failed", slog.Any("error", err))
		return
	}
	for i := range items {
		items[i].LikeCount = counts[items[i].ID]
		items[i].IsLiked = mine[items[i].ID]
	}
}

// ---------------------------------------------------------------------------
// Listing (docs/09 §20) - guest-first reads
// ---------------------------------------------------------------------------

// ListForNovel returns the fiction-level thread: top-level comments with
// chapter_id IS NULL. Chapter discussion lives under the chapter, so the
// fiction page never mixes chapter spoilers into its own thread.
func (s *Service) ListForNovel(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref, page pagination.Params,
) ([]View, pagination.Meta, error) {
	novel, err := s.novels.ForReader(ctx, identity, novelRef)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	items, total, err := s.repo.List(ctx, Filter{NovelID: novel.ID}, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list novel comments", err)
	}
	s.withLikes(ctx, items, viewerID(identity))
	return render(items, viewerID(identity)), page.MetaFor(total), nil
}

// ListForChapter returns one chapter's top-level thread.
func (s *Service) ListForChapter(
	ctx context.Context, identity *auth.Identity,
	novelRef novels.Ref, chapterRef chapters.Ref, page pagination.Params,
) ([]View, pagination.Meta, error) {
	_, chapter, err := s.chapters.ResolveForReader(ctx, identity, novelRef, chapterRef)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	items, total, err := s.repo.List(ctx, Filter{
		NovelID: chapter.NovelID, ChapterID: &chapter.ID,
	}, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list chapter comments", err)
	}
	s.withLikes(ctx, items, viewerID(identity))
	return render(items, viewerID(identity)), page.MetaFor(total), nil
}

// ListReplies returns one comment's replies, oldest first.
func (s *Service) ListReplies(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID, page pagination.Params,
) ([]View, pagination.Meta, error) {
	parent, err := s.visibleParent(ctx, identity, commentID)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	items, total, err := s.repo.List(ctx, Filter{
		NovelID: parent.NovelID, ParentID: &parent.ID,
	}, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list replies", err)
	}
	s.withLikes(ctx, items, viewerID(identity))
	return render(items, viewerID(identity)), page.MetaFor(total), nil
}

// ---------------------------------------------------------------------------
// Creation (docs/09 §20) - authenticated, but NEVER verification-gated:
// email verification gates publishing fiction, not ordinary account use
// (docs/AUTHENTICATION.md §9).
// ---------------------------------------------------------------------------

// CreateForNovel posts a comment on the fiction itself.
func (s *Service) CreateForNovel(
	ctx context.Context, identity *auth.Identity,
	novelRef novels.Ref, rawContent, rawGuestName string,
) (*View, error) {
	content, err := validateContent(rawContent)
	if err != nil {
		return nil, err
	}

	novel, err := s.novels.ForReader(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}
	poster, err := resolvePoster(novel, identity, rawGuestName)
	if err != nil {
		return nil, err
	}

	return s.create(ctx, CreateParams{
		UserID: poster.userID, GuestName: poster.guestName, Status: poster.status,
		NovelID: novel.ID, Content: content,
	})
}

// CreateForChapter posts a comment on one chapter.
func (s *Service) CreateForChapter(
	ctx context.Context, identity *auth.Identity,
	novelRef novels.Ref, chapterRef chapters.Ref, rawContent, rawGuestName string,
) (*View, error) {
	content, err := validateContent(rawContent)
	if err != nil {
		return nil, err
	}

	novel, chapter, err := s.chapters.ResolveForReader(ctx, identity, novelRef, chapterRef)
	if err != nil {
		return nil, err
	}
	poster, err := resolvePoster(novel, identity, rawGuestName)
	if err != nil {
		return nil, err
	}

	return s.create(ctx, CreateParams{
		UserID: poster.userID, GuestName: poster.guestName, Status: poster.status,
		NovelID: chapter.NovelID, ChapterID: &chapter.ID, Content: content,
	})
}

// MaxGuestNameRunes bounds the name a guest types. Runes, like every other
// text limit here.
const MaxGuestNameRunes = 40

// poster is the identity and starting state one create resolves to.
type poster struct {
	userID    *uuid.UUID
	guestName *string
	status    Status
}

// resolvePoster answers both questions a new comment raises at once: may this
// caller post here at all, and does what they post appear immediately (§13D).
//
// They are one function because they are one decision. Splitting them produced
// the bug this feature exists to avoid - a thread opened to guests with no
// queue behind it - and keeping the rule in a single place is what makes
// "guests are always held" true of every path rather than of the paths someone
// remembered.
//
// The rules, in order:
//
//	off        nobody adds to the thread. 403, not 404: the reader can see the
//	           thread, and saying so plainly beats implying the fiction vanished
//	members    an account is required. A guest gets 401 - the UI turns that
//	           into a sign-in offer without losing what they typed (docs/02 §5.2)
//	everyone   a guest may post, with a name, and their comment WAITS. Always,
//	           whatever comment_approval says: there is no account behind it to
//	           warn, suspend, or hold responsible
//	approval   the author's switch, which holds member comments too. The
//	           author's own comments skip it - approving yourself is not review
func resolvePoster(
	novel *novels.Novel, identity *auth.Identity, rawGuestName string,
) (poster, error) {
	access := novel.Extras.CommentAccess

	if !access.Open() {
		return poster{}, apierror.New(http.StatusForbidden, "COMMENTS_CLOSED",
			"The author has turned comments off for this fiction.")
	}

	if !identity.Authenticated() {
		if !access.AllowsGuests() {
			return poster{}, apierror.Unauthorized("Authentication required.")
		}
		name, err := validateGuestName(rawGuestName)
		if err != nil {
			return poster{}, err
		}
		return poster{guestName: &name, status: StatusPending}, nil
	}

	userID := identity.UserID()
	status := StatusVisible
	if novel.Extras.CommentApproval && !novel.OwnedBy(userID) {
		status = StatusPending
	}
	return poster{userID: &userID, status: status}, nil
}

// validateGuestName checks the one field a guest fills in besides the comment.
//
// It is required rather than defaulted to "ผู้อ่าน": a thread of identical
// anonymous rows is unreadable, and asking for a name is the smallest thing
// that makes a guest comment a comment by someone.
func validateGuestName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		return "", apierror.Validation(map[string][]string{
			"guest_name": {"ใส่ชื่อที่อยากให้แสดงด้วย"},
		})
	case utf8.RuneCountInString(name) > MaxGuestNameRunes:
		return "", apierror.Validation(map[string][]string{
			"guest_name": {"ชื่อต้องไม่เกิน 40 ตัวอักษร"},
		})
	case !novels.SafeText(name) || strings.ContainsAny(name, "\n\r"):
		return "", apierror.Validation(map[string][]string{
			"guest_name": {"ชื่อมีอักขระที่ใช้ไม่ได้"},
		})
	}
	return name, nil
}

// Reply posts a reply under a top-level comment (docs/09 §20).
//
// Threading is deliberately SINGLE-LEVEL for now: a reply to a reply is
// rejected with a clear 422 rather than silently re-attached to the thread
// root - predictable behavior over guessed intent (CLAUDE.md Writer-First).
// The schema supports arbitrary depth, so deepening later is a policy change,
// not a migration.
func (s *Service) Reply(
	ctx context.Context, identity *auth.Identity,
	parentID uuid.UUID, rawContent, rawGuestName string,
) (*View, error) {
	content, err := validateContent(rawContent)
	if err != nil {
		return nil, err
	}

	parent, err := s.visibleParent(ctx, identity, parentID)
	if err != nil {
		return nil, err
	}
	// Depth (comment design review 2026-08): three levels read as a
	// conversation - ความคิดเห็น, ตอบกลับ, ตอบกลับซ้อน. A reply to a comment
	// already on the third level JOINS that level, beside what it answers,
	// rather than digging deeper: a thread can always be replied to, and the
	// page never grows a staircase.
	if parent.ParentID != nil {
		grand, err := s.repo.Find(ctx, *parent.ParentID)
		if err != nil {
			return nil, s.internal("load thread root", err)
		}
		if grand.ParentID != nil {
			parent = grand
		}
	}

	// A reply obeys the same three-level rule as a top-level comment. It has to:
	// otherwise "เฉพาะสมาชิก" would mean "members start threads, anyone
	// continues them".
	novel, err := s.novels.ForReader(ctx, identity, novels.Ref{ID: parent.NovelID})
	if err != nil {
		return nil, err
	}
	poster, err := resolvePoster(novel, identity, rawGuestName)
	if err != nil {
		return nil, err
	}

	return s.create(ctx, CreateParams{
		UserID: poster.userID, GuestName: poster.guestName, Status: poster.status,
		NovelID: parent.NovelID, ChapterID: parent.ChapterID,
		ParentID: &parent.ID, Content: content,
	})
}

// create runs the shared insert-and-notify tail.
//
// The notification fires only for a comment that is actually THERE. A pending
// one has not joined the conversation yet, so telling the author "someone
// replied to you" would be describing something no reader can see; the review
// queue is where a held comment announces itself, and Decide sends the ordinary
// notification if it is approved.
func (s *Service) create(ctx context.Context, params CreateParams) (*View, error) {
	comment, err := s.repo.Create(ctx, params)
	if err != nil {
		return nil, s.internal("create comment", err)
	}

	if s.notifier != nil && params.UserID != nil && comment.Status == StatusVisible {
		s.notifier.CommentCreated(ctx, *params.UserID, comment.ID)
	}

	view := comment.Render(viewerOf(params.UserID))
	return &view, nil
}

// viewerOf turns an optional poster id into the viewer key Render expects.
func viewerOf(userID *uuid.UUID) uuid.UUID {
	if userID == nil {
		return uuid.Nil
	}
	return *userID
}

// visibleParent loads a comment for reading through it: it must be visible,
// and its fiction must still be readable by the caller. Everything else is
// the same 404 - a hidden comment's existence is not confirmed to anyone
// (docs/11 §3.4).
func (s *Service) visibleParent(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID,
) (*Comment, error) {
	comment, err := s.repo.Find(ctx, commentID)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load comment", err)
	}
	if comment.DeletedAt != nil || comment.Status != StatusVisible {
		return nil, notFound()
	}
	if _, err := s.novels.ForReader(ctx, identity, novels.Ref{ID: comment.NovelID}); err != nil {
		// The fiction went private or away; its discussion goes with it.
		return nil, notFound()
	}
	return comment, nil
}

// ---------------------------------------------------------------------------
// ถูกใจ (comment design review 2026-08)
// ---------------------------------------------------------------------------

// LikeView is a like/unlike's answer: the fresh total and the caller's state.
type LikeView struct {
	LikeCount int64 `json:"like_count"`
	IsLiked   bool  `json:"is_liked"`
}

// Like records the caller's heart on a comment they can see. Members only -
// a guest heart would be an anonymous counter nobody could ever take back,
// the same reason a guest comment cannot be edited (§13D). Idempotent.
func (s *Service) Like(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID,
) (*LikeView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	comment, err := s.visibleParent(ctx, identity, commentID)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Like(ctx, comment.ID, userID); err != nil {
		return nil, s.internal("like comment", err)
	}
	count, err := s.repo.LikeCount(ctx, comment.ID)
	if err != nil {
		return nil, s.internal("count likes", err)
	}
	return &LikeView{LikeCount: count, IsLiked: true}, nil
}

// Unlike takes the caller's heart back. Idempotent like Like.
func (s *Service) Unlike(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID,
) (*LikeView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	comment, err := s.visibleParent(ctx, identity, commentID)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Unlike(ctx, comment.ID, userID); err != nil {
		return nil, s.internal("unlike comment", err)
	}
	count, err := s.repo.LikeCount(ctx, comment.ID)
	if err != nil {
		return nil, s.internal("count likes", err)
	}
	return &LikeView{LikeCount: count, IsLiked: false}, nil
}

// ---------------------------------------------------------------------------
// Edit / delete (docs/09 §20: "Only the comment owner or authorized
// moderator")
// ---------------------------------------------------------------------------

// Update replaces a comment's text.
func (s *Service) Update(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID, rawContent string,
) (*View, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	content, err := validateContent(rawContent)
	if err != nil {
		return nil, err
	}

	comment, err := s.repo.Find(ctx, commentID)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load comment", err)
	}
	if comment.DeletedAt != nil || comment.Status != StatusVisible {
		return nil, notFound()
	}
	// A comment is public, so 403 confirms nothing that its listing did not
	// already reveal - unlike private fiction, where a 404 is required.
	if !canModerate(identity, comment) {
		return nil, apierror.Forbidden("Only the comment's author may edit it.")
	}

	updated, err := s.repo.UpdateContent(ctx, comment.ID, content)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("update comment", err)
	}

	view := updated.Render(userID)
	return &view, nil
}

// Delete soft-deletes a comment. Idempotent: deleting an already-deleted
// comment is a success, matching every other DELETE on the API (docs/09 §33).
func (s *Service) Delete(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID,
) error {
	if _, err := requireUser(identity); err != nil {
		return err
	}

	comment, err := s.repo.Find(ctx, commentID)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return s.internal("load comment", err)
	}
	if comment.DeletedAt != nil {
		return nil
	}
	if !canModerate(identity, comment) {
		return apierror.Forbidden("Only the comment's author may delete it.")
	}

	if err := s.repo.SoftDelete(ctx, comment.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Raced with another delete of the same comment - same outcome.
			return nil
		}
		return s.internal("delete comment", err)
	}

	s.log.Info("comment deleted",
		slog.String("comment_id", comment.ID.String()),
		slog.String("actor_id", identity.UserID().String()),
		slog.Bool("by_author", comment.WrittenBy(identity.UserID())),
	)
	return nil
}

// ---------------------------------------------------------------------------
// ตรวจก่อนโพสต์ - the author's review queue (§13D)
//
// This is the half that makes the three access levels survivable. A writer who
// opens their thread to guests and is handed no way to review what arrives will
// close it again after the first bad day, and the level might as well not
// exist. The queue is therefore not a follow-up: it ships with the levels.
//
// It is the FICTION's author who decides here, not staff. Moderation
// (docs/08 §24) is a different axis with different powers and stays where it
// is; this is the author saying what appears under their own work.
// ---------------------------------------------------------------------------

// Pending returns the comments waiting on one fiction, newest first.
//
// Owner-only through novels.ForWriter, which gives the same 404-then-403 shape
// every other write path on a fiction gives: a stranger cannot learn that a
// fiction has an unreviewed comment, or that it exists.
func (s *Service) Pending(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref, page pagination.Params,
) ([]View, pagination.Meta, error) {
	novel, err := s.novels.ForWriter(ctx, identity, novelRef)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	items, total, err := s.repo.List(ctx, Filter{
		NovelID: novel.ID, Status: StatusPending, AllLevels: true,
	}, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list pending comments", err)
	}
	return render(items, viewerID(identity)), page.MetaFor(total), nil
}

// PendingCount backs the studio badge.
func (s *Service) PendingCount(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref,
) (int64, error) {
	novel, err := s.novels.ForWriter(ctx, identity, novelRef)
	if err != nil {
		return 0, err
	}
	total, err := s.repo.CountPending(ctx, novel.ID)
	if err != nil {
		return 0, s.internal("count pending comments", err)
	}
	return total, nil
}

// Decide approves or rejects one waiting comment.
//
// Approving publishes it and sends the notification its poster would have
// triggered on arrival - so an approved comment behaves in every way like one
// that never waited.
//
// Rejecting sets `removed`, the same terminal state moderation uses. It is
// deliberately NOT a delete: the row survives, which keeps any reply it
// collected attached to a parent and leaves an audit trail. The poster is not
// notified of a rejection - a guest has no account to notify, and telling a
// member their comment was refused is an invitation to argue about it under
// someone else's fiction.
func (s *Service) Decide(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID, approve bool,
) (*View, error) {
	comment, err := s.repo.Find(ctx, commentID)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load comment", err)
	}
	if comment.DeletedAt != nil {
		// Taken back by its poster before anyone looked. Nothing to decide.
		return nil, notFound()
	}

	// Authority comes from the FICTION. A caller who may not write to it gets
	// the fiction's own answer, which is a 404 for a stranger.
	if _, err := s.novels.ForWriter(ctx, identity, novels.Ref{ID: comment.NovelID}); err != nil {
		return nil, err
	}

	if comment.Status != StatusPending {
		return nil, apierror.Conflict("This comment has already been decided.")
	}

	next := StatusRemoved
	if approve {
		next = StatusVisible
	}
	if err := s.repo.SetStatus(ctx, comment.ID, next); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, notFound()
		}
		return nil, s.internal("decide comment", err)
	}

	if approve && s.notifier != nil && comment.UserID != nil {
		s.notifier.CommentCreated(ctx, *comment.UserID, comment.ID)
	}

	s.log.Info("comment decided",
		slog.String("comment_id", comment.ID.String()),
		slog.String("actor_id", identity.UserID().String()),
		slog.String("to", string(next)),
		slog.Bool("guest", comment.UserID == nil),
	)

	comment.Status = next
	view := comment.Render(viewerID(identity))
	return &view, nil
}

// ---------------------------------------------------------------------------
// Moderation (docs/08 §24, Phase 8)
// ---------------------------------------------------------------------------

// VisibleForViewer answers (by error) whether the caller can read this
// comment - the report-target check (docs/08 §24.1). Same rules, same 404 as
// every read path: a hidden comment or one on an unreadable fiction does not
// exist for this caller (docs/11 §3.4).
func (s *Service) VisibleForViewer(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID,
) error {
	_, err := s.visibleParent(ctx, identity, commentID)
	return err
}

// Moderate sets the platform's status axis on a comment (docs/08 §20.1) and
// returns its author - the moderation notification's recipient, or uuid.Nil
// for a guest comment, which has nobody to notify. Staff-only; deleted_at is
// the AUTHOR's axis and is never touched, so a comment the author took back
// stays gone whatever moderation does.
func (s *Service) Moderate(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID, status Status,
) (uuid.UUID, error) {
	if !identity.IsStaff() {
		return uuid.Nil, apierror.Forbidden("You do not have permission to do that.")
	}
	switch status {
	case StatusVisible, StatusHidden, StatusRemoved:
	default:
		return uuid.Nil, apierror.Validation(map[string][]string{
			"status": {"Unknown comment status."},
		})
	}

	comment, err := s.repo.Find(ctx, commentID)
	if errors.Is(err, ErrNotFound) {
		return uuid.Nil, notFound()
	}
	if err != nil {
		return uuid.Nil, s.internal("load comment", err)
	}
	if comment.DeletedAt != nil {
		// Author-deleted: nothing left to moderate, and restoring it is not
		// moderation's call (docs/08 §20.1's independent axes).
		return uuid.Nil, notFound()
	}
	if comment.Status == status {
		return uuid.Nil, apierror.Conflict("The comment is already in that state.")
	}

	if err := s.repo.SetStatus(ctx, comment.ID, status); err != nil {
		return uuid.Nil, s.internal("moderate comment", err)
	}

	s.log.Info("comment moderated",
		slog.String("comment_id", comment.ID.String()),
		slog.String("actor_id", identity.UserID().String()),
		slog.String("from", string(comment.Status)),
		slog.String("to", string(status)),
	)
	return viewerOf(comment.UserID), nil
}

// internal logs the real failure and returns the opaque error (docs/11 §67).
func (s *Service) internal(op string, err error) error {
	s.log.Error("comments service failure", slog.String("op", op), slog.Any("error", err))
	return apierror.Internal()
}
