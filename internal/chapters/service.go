package chapters

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/slug"
)

// createAttempts bounds the retry loop for a chapter number or slug collision.
const createAttempts = 5

// NovelAccess is the slice of the novels domain this service needs.
//
// Declaring it here rather than importing the concrete service keeps the
// dependency one-directional (chapters -> novels, never back) and makes the
// authorization boundary explicit: every chapter operation starts by asking the
// novels service whether the caller may read or write the parent fiction
// (docs/08 §10.2).
type NovelAccess interface {
	ForReader(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
	ForEditor(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
	// OwnedByUser is the by-user-id ownership gate the AI worker needs, since a
	// background job has no request Identity (docs/12 §27, §33).
	OwnedByUser(ctx context.Context, userID uuid.UUID, ref novels.Ref) (*novels.Novel, error)
}

// Notifier is the slice of the notifications domain this service needs
// (docs/07 §38 "ChapterPublished"). Consumer-defined, like NovelAccess, so the
// dependency stays one-directional: this package never imports notifications.
//
// Emitting is FIRE-AND-FORGET: by the time it is called the publish has
// committed, and a notification failure must never turn a successful publish
// into an error (docs/07 §27 "The API should remain responsive").
type Notifier interface {
	ChapterPublished(ctx context.Context, actorID, novelID, chapterID uuid.UUID)
}

// Achiever is the slice of the achievements domain this service needs
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3). Consumer-defined for the same
// reason Notifier is: chapters must not import achievements, and the
// achievement key - เริ่มต้น - stays where its threshold lives.
//
// Fire-and-forget by contract, like the notifier beside it. Nothing an
// achievement does may turn a successful publish into an error.
type Achiever interface {
	ChapterPublished(ctx context.Context, authorID uuid.UUID)
}

// Service owns chapter business rules.
type Service struct {
	repo     *Repository
	novels   NovelAccess
	notifier Notifier
	log      *slog.Logger

	// achievements is optional and set after construction, so adding it did
	// not change a constructor every test in the repository already calls.
	// nil simply records nothing.
	achievements Achiever

	// now is injectable so tests can exercise scheduled-publication boundaries
	// without sleeping.
	now func() time.Time
}

// NewService wires the service. notifier may be nil (tests that predate
// Phase 6, tooling): publishing then simply emits nothing.
func NewService(repo *Repository, novelAccess NovelAccess, notifier Notifier, log *slog.Logger) *Service {
	return &Service{repo: repo, novels: novelAccess, notifier: notifier, log: log, now: time.Now}
}

// notifyFirstPublish emits the follower notification exactly once per chapter:
// on the transition that stamps published_at. Republishing after an unpublish
// keeps the original timestamp (repository COALESCE), so followers can never
// be notified twice for the same chapter, whichever route the publish took
// (Create with status=published, PATCH, or POST /publish).
func (s *Service) notifyFirstPublish(
	ctx context.Context, actorID uuid.UUID, novelID uuid.UUID,
	publishedBefore *time.Time, after *Chapter,
) {
	if publishedBefore != nil || after.Status != StatusPublished || after.PublishedAt == nil {
		return
	}
	// เริ่มต้น (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3). The same transition
	// the follower notification hangs off, because it is the same moment: the
	// first time a chapter of theirs became readable.
	if s.achievements != nil {
		s.achievements.ChapterPublished(ctx, actorID)
	}
	if s.notifier == nil {
		return
	}
	s.notifier.ChapterPublished(ctx, actorID, novelID, after.ID)
}

// SetAchiever attaches the achievement service after construction.
func (s *Service) SetAchiever(achiever Achiever) { s.achievements = achiever }

func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "CHAPTER_NOT_FOUND", "Chapter not found.")
}

func verificationRequired() *apierror.Error {
	return apierror.New(http.StatusForbidden, "EMAIL_VERIFICATION_REQUIRED",
		"Please verify your email address before publishing.")
}

// List returns a fiction's table of contents.
//
// A non-owner sees only live chapters. docs/11 §21 is explicit: a public fiction
// does not make its unpublished chapters public, even to someone who knows the
// chapter id.
func (s *Service) List(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref,
) ([]Summary, error) {
	novel, err := s.novels.ForReader(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}

	isOwner := s.canManage(identity, novel)
	listings, err := s.repo.List(ctx, novel.ID, !isOwner)
	if err != nil {
		return nil, s.internal("list chapters", err)
	}

	// Each chapter resolves its own format: a mixed fiction's table of contents
	// is exactly where the per-chapter answer has to be right (§13J).
	summaries := make([]Summary, 0, len(listings))
	for i := range listings {
		active := listings[i].ActiveFormat(novel.Format)
		summary := listings[i].Summarize(active, listings[i].Presence)
		// The schedule is the owner's business (§13T). A reader's list carries
		// no scheduled rows at all, but the guard stays: what a reader must
		// never see is decided here, not by which rows the query returned.
		if isOwner {
			summary.ScheduledAt = listings[i].ScheduledAt
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// Get returns one chapter with the content the caller is entitled to.
func (s *Service) Get(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref, chapterRef Ref,
) (*View, error) {
	novel, err := s.novels.ForReader(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}

	chapter, err := s.repo.Find(ctx, novel.ID, chapterRef)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load chapter", err)
	}

	isOwner := s.canManage(identity, novel)
	if !isOwner && !chapter.Live(s.now()) {
		// Same 404 a missing chapter gets. A 403 would confirm that an
		// unpublished chapter exists behind this id (docs/11 §3.4).
		return nil, notFound()
	}

	active := chapter.ActiveFormat(novel.Format)

	// Each representation is loaded only when someone will actually receive it:
	// a reader of a prose chapter pays for neither the chat nor the entry query.
	var messages []Message
	if active == fiction.Chat || isOwner {
		messages, err = s.repo.Messages(ctx, chapter.ID)
		if err != nil {
			return nil, s.internal("load messages", err)
		}
	}
	var entries []Entry
	if active == fiction.HeadcanonFormat || isOwner {
		entries, err = s.repo.Entries(ctx, chapter.ID)
		if err != nil {
			return nil, s.internal("load entries", err)
		}
	}

	previous, next, err := s.repo.Neighbours(ctx, novel.ID, chapter.Number, !isOwner)
	if err != nil {
		return nil, s.internal("load chapter neighbours", err)
	}

	view := chapter.Render(ViewParams{
		Active:   active,
		Messages: messages,
		Entries:  entries,
		IsOwner:  isOwner,
		Previous: previous,
		Next:     next,
	})
	return &view, nil
}

// ResolveForReader resolves a chapter reference under exactly the visibility
// rule Get applies, without paying for content, messages, or neighbours.
//
// Interaction features anchor to a chapter through this, so a comment can only
// ever be attached to a chapter its author could actually read (docs/11 §21) -
// and the 404 for an unpublished chapter is indistinguishable from a missing
// one, exactly as in Get.
func (s *Service) ResolveForReader(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref, chapterRef Ref,
) (*novels.Novel, *Chapter, error) {
	novel, err := s.novels.ForReader(ctx, identity, novelRef)
	if err != nil {
		return nil, nil, err
	}

	chapter, err := s.repo.Find(ctx, novel.ID, chapterRef)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, notFound()
	}
	if err != nil {
		return nil, nil, s.internal("load chapter", err)
	}
	if !s.canManage(identity, novel) && !chapter.Live(s.now()) {
		return nil, nil, notFound()
	}
	return novel, chapter, nil
}

// CreateInput is a validated request to add a chapter.
type CreateInput struct {
	Title    *string
	Content  *string
	Messages *[]MessageInput
	Entries  *[]EntryInput

	// PresentationFormat is what THIS chapter renders as. Absent or empty means
	// "follow the fiction", which is what every chapter of a non-mixed work
	// sends (§13J).
	PresentationFormat *string
	EntryFields        *[]string

	// ContentFormat is how the prose renders (§13N). Absent takes
	// DefaultContentFormat: a chapter with nothing in it has nothing to
	// reinterpret, so the editor's own model is safe on a new one.
	ContentFormat *string

	// Number is the chapter number the writer chose (§13R). Nil appends after
	// the highest existing one, which is what the studio sends unless the
	// writer typed something else.
	Number *int

	Status      *string
	ScheduledAt *time.Time
}

// Create adds a chapter to a fiction.
//
// It deliberately does NOT refuse a second chapter on a one-shot. docs/08 §7.2
// leaves that to the product layer and warns the database must not rely on it,
// and enforcing it here would break the guarantee that matters more: a writer
// switching multi_chapter -> one_shot with five chapters must not have the change
// rejected or their chapters merged (docs/08 §3, docs/15 §5.7). Story structure
// changes navigation, not storage.
func (s *Service) Create(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref, input CreateInput,
) (*View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}

	errs := validationErrors{}
	validateOptionalTitle(errs, input.Title)
	validateContent(errs, input.Content)

	var messages []Message
	if input.Messages != nil {
		messages = validateMessages(errs, *input.Messages)
	}

	var entries []Entry
	if input.Entries != nil {
		entries = validateEntries(errs, *input.Entries)
	}
	var entryFields []string
	if input.EntryFields != nil {
		entryFields = validateEntryFields(errs, *input.EntryFields)
	}

	// The chapter is STAMPED with its own mode, always (§13P).
	//
	// Before this, an omitted format stored NULL and meant "follow the fiction",
	// which was cheap but made "ล็อกตั้งแต่สร้าง" untrue: a later fiction-level
	// change would silently turn a prose chapter into a chat one. A chapter that
	// declares its own mode at birth cannot be moved by anything but its author
	// creating a different chapter - and it is still one column on one row, so
	// nothing about the fiction-level change becomes a mass write.
	format := validateChapterFormat(errs, input.PresentationFormat)
	if format == nil {
		inherited := novel.Format.PresentationFormat
		format = &inherited
	}

	contentFormat := DefaultContentFormat
	if chosen := validateContentFormat(errs, input.ContentFormat); chosen != nil {
		contentFormat = *chosen
	}

	if err := s.checkEntryCharacters(ctx, errs, novel.ID, entries); err != nil {
		return nil, err
	}

	status := DefaultStatus
	if input.Status != nil {
		status = Status(*input.Status)
		if !status.Valid() {
			errs.add("status", fmt.Sprintf("Must be one of: %s.", joinValues(Statuses())))
		}
	}
	scheduled := validateSchedule(errs, status, input.ScheduledAt, s.now())

	// The number the writer typed, if they typed one (§13R). Bounded rather than
	// free: a number is what a reader navigates by, and one outside this range
	// is a typo in a numeric field, not an arrangement anyone chose.
	number := 0
	if input.Number != nil {
		number = *input.Number
		if number < MinChapterNumber || number > MaxChapterNumber {
			errs.add("chapter_number", fmt.Sprintf(
				"Must be between %d and %d.", MinChapterNumber, MaxChapterNumber))
		}
	}

	if err := errs.err(); err != nil {
		return nil, err
	}
	if err := s.requireVerifiedToPublish(identity, status); err != nil {
		return nil, err
	}

	title := trimmedOrNil(input.Title)

	for attempt := 0; attempt < createAttempts; attempt++ {
		// A bare random token, never the title (docs/SLUGS.md, address review
		// 2026-08): a title-based address froze the chapter's name at creation
		// and lied after every rename. Older title-based addresses still
		// resolve as they are.
		candidate, err := slug.NewToken()
		if err != nil {
			return nil, s.internal("generate chapter slug", err)
		}

		chapter, err := s.repo.Create(ctx, CreateParams{
			NovelID:       novel.ID,
			ActorID:       identity.UserID(),
			Title:         title,
			Slug:          candidate,
			Content:       input.Content,
			Messages:      messages,
			Entries:       entries,
			EntryFields:   entryFields,
			Format:        format,
			ContentFormat: contentFormat,
			Number:        number,
			Status:        status,
			ScheduledAt:   scheduled,
		})
		// A number the WRITER chose cannot be resolved by retrying - the next
		// attempt would ask for the same one - and quietly moving it to the end
		// would put their chapter somewhere they did not put it. They are told
		// instead (§13R).
		if number != 0 && errors.Is(err, ErrNumberTaken) {
			return nil, apierror.New(http.StatusConflict, "CHAPTER_NUMBER_TAKEN",
				fmt.Sprintf("มีตอนที่ %d อยู่แล้ว - ใช้เลขอื่น หรือเว้นว่างให้ระบบต่อเลขให้", number))
		}
		// Both collisions are resolved the same way: try again. The number is
		// reallocated from MAX+1 on the next attempt, and the slug gets fresh
		// randomness.
		if errors.Is(err, ErrSlugTaken) || errors.Is(err, ErrNumberTaken) {
			continue
		}
		if err != nil {
			return nil, s.internal("create chapter", err)
		}

		s.notifyFirstPublish(ctx, identity.UserID(), novel.ID, nil, chapter)

		view := chapter.Render(ViewParams{
			Active:   chapter.ActiveFormat(novel.Format),
			Messages: messages,
			Entries:  entries,
			IsOwner:  true,
		})
		return &view, nil
	}

	return nil, s.internal("create chapter",
		fmt.Errorf("could not allocate a chapter slot after %d attempts", createAttempts))
}

// UpdateInput is a validated partial update.
//
// The double pointers distinguish absent from explicitly cleared. Collapsing
// them would let a PATCH that only changes the status erase a manuscript, which
// is the single most damaging bug this domain could have.
type UpdateInput struct {
	Title    **string
	Content  **string
	Messages *[]MessageInput
	Entries  *[]EntryInput

	// PresentationFormat present-but-empty means "go back to following the
	// fiction"; absent leaves the chapter's choice alone.
	PresentationFormat *string
	EntryFields        *[]string

	// ContentFormat moves a chapter between the literal and the marked-up
	// reading of its own text (§13N). Absent leaves it alone, which is what
	// every ordinary save sends - a chapter written before the editor existed
	// stays literal until its author asks otherwise.
	ContentFormat *string

	Status      *string
	ScheduledAt **time.Time
}

// Update edits a chapter, recording a revision of what it replaces.
func (s *Service) Update(
	ctx context.Context, identity *auth.Identity,
	novelRef novels.Ref, chapterRef Ref, input UpdateInput,
) (*View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}

	chapter, err := s.repo.Find(ctx, novel.ID, chapterRef)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load chapter", err)
	}

	errs := validationErrors{}
	params := UpdateParams{ChapterID: chapter.ID, ActorID: identity.UserID()}

	if input.Title != nil {
		validateOptionalTitle(errs, *input.Title)
		title := trimmedOrNil(*input.Title)
		params.Title = &title
	}
	if input.Content != nil {
		validateContent(errs, *input.Content)
		params.Content = input.Content
	}
	if input.Messages != nil {
		messages := validateMessages(errs, *input.Messages)
		params.Messages = &messages
	}
	if input.Entries != nil {
		entries := validateEntries(errs, *input.Entries)
		if err := s.checkEntryCharacters(ctx, errs, novel.ID, entries); err != nil {
			return nil, err
		}
		params.Entries = &entries
	}
	if input.EntryFields != nil {
		fields := validateEntryFields(errs, *input.EntryFields)
		params.EntryFields = &fields
	}
	// A chapter's mode is LOCKED at creation (§13P).
	//
	// It used to be a dropdown in the editor. That was wrong in a way the schema
	// hid: the three representations are stored side by side, so switching was
	// cheap - but a chapter is a piece of writing with a shape, and one that can
	// become a chat and back is one whose writer is never sure what they are
	// looking at. The choice is made once, on the screen that creates the
	// chapter, and the editor then has one job.
	//
	// Refused rather than ignored: a client that asked for something and got a
	// silent no-op is a client that will keep asking.
	if input.PresentationFormat != nil {
		requested := validateChapterFormat(errs, input.PresentationFormat)
		if len(errs) == 0 && !sameFormat(requested, chapter.Format) {
			return nil, apierror.New(http.StatusConflict, "CHAPTER_FORMAT_LOCKED",
				"โหมดของตอนถูกเลือกไว้ตั้งแต่ตอนสร้างและเปลี่ยนไม่ได้ - สร้างตอนใหม่ถ้าต้องการโหมดอื่น")
		}
	}
	// A metadata-only change, like the presentation format beside it: no
	// revision is taken because no content is written, and the author's markers
	// stay exactly as typed whichever way the switch goes.
	params.ContentFormat = validateContentFormat(errs, input.ContentFormat)

	status := chapter.Status
	if input.Status != nil {
		status = Status(*input.Status)
		if !status.Valid() {
			errs.add("status", fmt.Sprintf("Must be one of: %s.", joinValues(Statuses())))
		}
		params.Status = &status
	}

	// Validate the RESULTING schedule, not the supplied field in isolation: a
	// request that sets status=scheduled without a time must be rejected even
	// though neither field is individually wrong.
	scheduledAt := chapter.ScheduledAt
	if input.ScheduledAt != nil {
		scheduledAt = *input.ScheduledAt
		params.ScheduledAt = input.ScheduledAt
	}
	validateSchedule(errs, status, scheduledAt, s.now())

	if err := errs.err(); err != nil {
		return nil, err
	}
	if input.Status != nil && status != chapter.Status {
		if err := s.requireVerifiedToPublish(identity, status); err != nil {
			return nil, err
		}
	}

	updated, err := s.repo.Update(ctx, params)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("update chapter", err)
	}

	s.notifyFirstPublish(ctx, identity.UserID(), novel.ID, chapter.PublishedAt, updated)

	return s.ownerView(ctx, novel, updated)
}

// SetStatus implements the publish and unpublish endpoints (docs/09 §16).
func (s *Service) SetStatus(
	ctx context.Context, identity *auth.Identity,
	novelRef novels.Ref, chapterRef Ref, status Status,
) (*View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}

	chapter, err := s.repo.Find(ctx, novel.ID, chapterRef)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load chapter", err)
	}

	if err := s.requireVerifiedToPublish(identity, status); err != nil {
		return nil, err
	}

	// Publishing clears any pending schedule: the chapter is live now, and
	// leaving a future timestamp behind would make Live() ambiguous.
	params := UpdateParams{ChapterID: chapter.ID, ActorID: identity.UserID(), Status: &status}
	if status == StatusPublished && chapter.ScheduledAt != nil {
		var cleared *time.Time
		params.ScheduledAt = &cleared
	}

	updated, err := s.repo.Update(ctx, params)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("set chapter status", err)
	}

	s.notifyFirstPublish(ctx, identity.UserID(), novel.ID, chapter.PublishedAt, updated)

	s.log.Info("chapter status changed",
		slog.String("novel_id", novel.ID.String()),
		slog.String("chapter_id", chapter.ID.String()),
		slog.String("actor_id", identity.UserID().String()),
		slog.String("from", string(chapter.Status)),
		slog.String("to", string(status)),
	)

	return s.ownerView(ctx, novel, updated)
}

// Reorder rewrites the fiction's chapter order, renumbering to 1..N.
//
// The list must name every live chapter exactly once - a partial order would
// leave the rest at numbers that may collide with the new sequence, so it is
// refused rather than half-applied (the same contract the cast reorder has).
// Content work, so a collaborator may do it (13U).
func (s *Service) Reorder(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref, ids []uuid.UUID,
) ([]Summary, error) {
	novel, err := s.novels.ForEditor(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}

	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, apierror.Validation(map[string][]string{
				"chapter_ids": {"Each chapter may appear only once."},
			})
		}
		seen[id] = true
	}
	total, err := s.repo.CountLive(ctx, novel.ID)
	if err != nil {
		return nil, s.internal("count chapters", err)
	}
	if len(ids) != total {
		return nil, apierror.Validation(map[string][]string{
			"chapter_ids": {"The order must list every chapter of this fiction exactly once."},
		})
	}

	if err := s.repo.Reorder(ctx, novel.ID, ids); err != nil {
		return nil, s.internal("reorder chapters", err)
	}

	s.log.Info("chapters reordered",
		slog.String("novel_id", novel.ID.String()),
		slog.String("actor_id", identity.UserID().String()),
		slog.Int("chapters", len(ids)),
	)

	return s.List(ctx, identity, novelRef)
}

// Delete soft-deletes a chapter. Its messages and revisions survive.
func (s *Service) Delete(
	ctx context.Context, identity *auth.Identity, novelRef novels.Ref, chapterRef Ref,
) error {
	novel, err := s.novels.ForEditor(ctx, identity, novelRef)
	if err != nil {
		return err
	}

	chapter, err := s.repo.Find(ctx, novel.ID, chapterRef)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return s.internal("load chapter", err)
	}

	if err := s.repo.SoftDelete(ctx, chapter.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound()
		}
		return s.internal("delete chapter", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Moderation (docs/08 §24, Phase 8)
// ---------------------------------------------------------------------------

// ForReaderByID resolves a chapter from a bare id under the reader rules -
// the same gate ResolveForReader applies, for callers (the moderation
// service) that hold only a chapter UUID from a report. The 404 semantics
// are identical: a chapter the caller may not read does not exist
// (docs/11 §21).
func (s *Service) ForReaderByID(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (*Chapter, error) {
	chapter, err := s.repo.FindByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load chapter", err)
	}

	novel, err := s.novels.ForReader(ctx, identity, novels.Ref{ID: chapter.NovelID})
	if err != nil {
		return nil, err
	}
	if !s.canManage(identity, novel) && !chapter.Live(s.now()) {
		return nil, notFound()
	}
	return chapter, nil
}

// ModerateRemove soft-deletes a chapter as a moderation action and returns
// the fiction's author (the moderation notification's recipient). Staff-only;
// like the novels counterpart, "already removed" is a conflict, not a 404.
func (s *Service) ModerateRemove(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (uuid.UUID, error) {
	chapter, novel, err := s.forModeration(ctx, identity, id)
	if err != nil {
		return uuid.Nil, err
	}
	if chapter.DeletedAt != nil {
		return uuid.Nil, apierror.Conflict("The chapter is already removed.")
	}
	if err := s.repo.SoftDelete(ctx, chapter.ID); err != nil {
		return uuid.Nil, s.internal("moderate-remove chapter", err)
	}
	return novel.AuthorID, nil
}

// ModerateRestore clears a moderation removal of a chapter.
func (s *Service) ModerateRestore(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (uuid.UUID, error) {
	chapter, novel, err := s.forModeration(ctx, identity, id)
	if err != nil {
		return uuid.Nil, err
	}
	if chapter.DeletedAt == nil {
		return uuid.Nil, apierror.Conflict("The chapter is not removed.")
	}
	if err := s.repo.Restore(ctx, chapter.ID); err != nil {
		return uuid.Nil, s.internal("moderate-restore chapter", err)
	}
	return novel.AuthorID, nil
}

// forModeration loads a chapter (soft-deleted or not) and its parent fiction
// for a staff state change. The parent must itself be live: a chapter of a
// removed fiction is handled at the fiction level first.
func (s *Service) forModeration(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (*Chapter, *novels.Novel, error) {
	if !identity.IsStaff() {
		return nil, nil, apierror.Forbidden("You do not have permission to do that.")
	}
	chapter, err := s.repo.FindByIDIncludingDeleted(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, notFound()
	}
	if err != nil {
		return nil, nil, s.internal("load chapter for moderation", err)
	}
	novel, err := s.novels.ForEditor(ctx, identity, novels.Ref{ID: chapter.NovelID})
	if err != nil {
		return nil, nil, err
	}
	return chapter, novel, nil
}

// ---------------------------------------------------------------------------
// AI access (docs/12 §33, Phase 10)
// ---------------------------------------------------------------------------

// OwnedContent is a chapter's analyzable plain-text prose plus its identity,
// handed to the AI domain. It carries ONLY what analysis needs - never chat
// messages, navigation, or reader-facing view state.
type OwnedContent struct {
	ChapterID uuid.UUID
	NovelID   uuid.UUID
	Title     *string
	Content   string
}

// ContentForOwnerID is the AI domain's single authorization boundary into
// chapters (docs/12 §33). It resolves a chapter's prose for the user who OWNS
// it, by user id:
//
//   - owner-only: no staff shortcut and no reader access - a writer may run AI
//     only over fiction they can edit (docs/11 §53);
//   - non-oracle: a chapter that is missing, soft-deleted, or owned by someone
//     else is the SAME CHAPTER_NOT_FOUND (docs/11 §21);
//   - by user id, not Identity, so the background worker can act as the
//     requesting writer with no HTTP session - enforcing the identical
//     ownership rule ForWriter does, never a privileged content read.
//
// It returns prose only. Chat-message analysis is future work (docs/12 §5
// classifies most model features as later); the boundary is here so adding it
// is a change to this one method.
func (s *Service) ContentForOwnerID(
	ctx context.Context, ownerID, chapterID uuid.UUID,
) (*OwnedContent, error) {
	chapter, err := s.repo.FindByID(ctx, chapterID)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load chapter", err)
	}

	if _, err := s.novels.OwnedByUser(ctx, ownerID, novels.Ref{ID: chapter.NovelID}); err != nil {
		// Fold "novel missing / not owned" into the CHAPTER 404 so it is
		// indistinguishable from a missing chapter; a genuine fault still
		// surfaces as its 500.
		var apiErr *apierror.Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, notFound()
		}
		return nil, err
	}

	content := ""
	if chapter.Content != nil {
		content = *chapter.Content
	}
	return &OwnedContent{
		ChapterID: chapter.ID,
		NovelID:   chapter.NovelID,
		Title:     chapter.Title,
		Content:   content,
	}, nil
}

// sameFormat reports whether a requested chapter format is the one already
// stored. Both are pointers because "" means "follow the fiction", which a
// chapter created after §13P never does - it is stamped with its own.
func sameFormat(requested, stored *fiction.PresentationFormat) bool {
	if requested == nil || stored == nil {
		return requested == stored
	}
	return *requested == *stored
}

// checkEntryCharacters rejects an entry pointing at a character outside this
// fiction.
//
// The mirror of characters.Service.requireOwnChapter, and it is here rather
// than in validation.go because it is an OWNERSHIP question, and ownership is
// decided in the service (docs/10 §27). Without it, an entry could confirm that
// a character id exists in someone else's fiction and render their cast on this
// page (docs/11 §8).
func (s *Service) checkEntryCharacters(
	ctx context.Context, errs validationErrors, novelID uuid.UUID, entries []Entry,
) error {
	ids := make([]uuid.UUID, 0, len(entries))
	for _, entry := range entries {
		if entry.CharacterID != nil {
			ids = append(ids, *entry.CharacterID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	found, err := s.repo.CharactersInNovel(ctx, novelID, ids)
	if err != nil {
		return s.internal("check entry characters", err)
	}
	for i, entry := range entries {
		if entry.CharacterID != nil && !found[*entry.CharacterID] {
			errs.add(fmt.Sprintf("entries[%d].character_id", i),
				"That character is not part of this fiction.")
		}
	}
	return nil
}

// ownerView reloads every representation and renders the owner's view after a
// write. The owner sees all of them - that is the proof that the write did not
// destroy the ones it did not touch (docs/CONTENT-MODEL.md §6).
func (s *Service) ownerView(ctx context.Context, novel *novels.Novel, chapter *Chapter) (*View, error) {
	messages, err := s.repo.Messages(ctx, chapter.ID)
	if err != nil {
		return nil, s.internal("load messages", err)
	}
	entries, err := s.repo.Entries(ctx, chapter.ID)
	if err != nil {
		return nil, s.internal("load entries", err)
	}
	previous, next, err := s.repo.Neighbours(ctx, novel.ID, chapter.Number, false)
	if err != nil {
		return nil, s.internal("load chapter neighbours", err)
	}

	view := chapter.Render(ViewParams{
		Active:   chapter.ActiveFormat(novel.Format),
		Messages: messages,
		Entries:  entries,
		IsOwner:  true,
		Previous: previous,
		Next:     next,
	})
	return &view, nil
}

// canManage mirrors the novels service: the owner, a collaborator (13U - a
// co-writer edits chapters, which is what collaborating IS), or staff acting
// under docs/09 §15.
func (s *Service) canManage(identity *auth.Identity, novel *novels.Novel) bool {
	if !identity.Authenticated() {
		return false
	}
	return novel.EditableBy(identity.UserID()) || identity.IsStaff()
}

// requireVerifiedToPublish enforces the Phase 1 decision in
// docs/AUTHENTICATION.md §9: email verification gates PUBLISHING, never reading
// or ordinary drafting.
func (s *Service) requireVerifiedToPublish(identity *auth.Identity, status Status) error {
	if status != StatusPublished && status != StatusScheduled {
		return nil
	}
	if identity.EmailVerified() || identity.IsStaff() {
		return nil
	}
	return verificationRequired()
}

// validateSchedule checks the resulting status/scheduled_at pair.
func validateSchedule(
	errs validationErrors, status Status, scheduledAt *time.Time, now time.Time,
) *time.Time {
	if status != StatusScheduled {
		return scheduledAt
	}
	if scheduledAt == nil {
		errs.add("scheduled_at", "A scheduled chapter needs a publication time.")
		return nil
	}
	if !scheduledAt.After(now) {
		// A schedule in the past would publish the chapter the instant it was
		// saved, which is a surprising way to make work public.
		errs.add("scheduled_at", "Must be in the future. Publish the chapter instead.")
	}
	return scheduledAt
}

func trimmedOrNil(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("chapters: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

// Compile-time assurance that the real novels service satisfies the narrow
// interface declared above.
var _ NovelAccess = (*novels.Service)(nil)
