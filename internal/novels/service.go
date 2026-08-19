package novels

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/internal/taxonomy"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/slug"
)

// slugAttempts bounds the retry loop that resolves a slug collision. Each retry
// appends fresh randomness, so exhausting five is effectively impossible and
// signals a real fault rather than contention.
const slugAttempts = 5

// ptrTo is the address of a value, for the double-pointer PATCH fields.
func ptrTo[T any](value T) *T { return &value }

// UserLookup is the slice of the users domain this service needs: resolving
// `?author=<username>` to an ID. Declaring the interface here rather than
// importing the concrete repository keeps the dependency one-directional and
// makes the service testable without a database.
type UserLookup interface {
	FindByUsername(ctx context.Context, username string) (*users.User, error)
}

// TermLookup is the slice of the taxonomy domain this service needs: checking
// that assigned genre and tag ids exist, so a bad id is a clean field error
// rather than a constraint violation. The dependency runs novels -> taxonomy,
// never back.
type TermLookup interface {
	GenreIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
	TagIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// FollowLookup is the slice of the library domain this service needs: the one
// fact the followers-only rung depends on (§13C).
//
// Declared here rather than imported, because library already imports novels
// for the shared SQL predicates. A consumer-defined interface is what keeps the
// dependency one-directional; there is deliberately no compile-time assertion
// against library.Repository below, since writing one would create the cycle
// the interface exists to avoid. The router's wiring is the check.
type FollowLookup interface {
	IsFollowing(ctx context.Context, followerID, followingID uuid.UUID) (bool, error)
}

// Achiever is the slice of the achievements domain this service needs
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3). Consumer-defined, so novels never
// imports achievements and never learns that ปิดจบ wants three chapters - it
// reports the fact and the count, and the other side decides.
//
// Fire-and-forget by contract: a fiction was just marked finished, and nothing
// about a badge may turn that into an error.
type Achiever interface {
	FictionCompleted(ctx context.Context, authorID uuid.UUID, chapterCount int)
}

// Service owns fiction business rules and is the authorization boundary for
// everything under /novels.
//
// docs/10 §27 and the Phase 2 brief both require ownership to be decided here
// rather than in HTTP middleware: middleware sees a route, but only the service
// can see who owns the row behind it.
type Service struct {
	repo    *Repository
	users   UserLookup
	terms   TermLookup
	follows FollowLookup
	log     *slog.Logger

	// achievements is optional and set after construction, so adding it did
	// not change a constructor half the test suite already calls. nil simply
	// records nothing.
	achievements Achiever
}

func NewService(
	repo *Repository, userLookup UserLookup, termLookup TermLookup,
	followLookup FollowLookup, log *slog.Logger,
) *Service {
	return &Service{
		repo: repo, users: userLookup, terms: termLookup,
		follows: followLookup, log: log,
	}
}

// SetAchiever attaches the achievement service after construction.
func (s *Service) SetAchiever(achiever Achiever) { s.achievements = achiever }

// notFound is the response for a fiction the caller may not see.
//
// It is deliberately identical whether the fiction is absent, soft-deleted, or a
// private draft belonging to someone else. A 403 there would confirm that the
// slug exists, which is exactly the probing docs/11 §3.4 and §31 rule out.
func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "NOVEL_NOT_FOUND", "Fiction not found.")
}

// forbidden is used only when the caller can already see the fiction but may not
// change it - there is nothing left to leak (docs/09 §35).
func forbidden() *apierror.Error {
	return apierror.Forbidden("You do not have permission to modify this fiction.")
}

func verificationRequired() *apierror.Error {
	return apierror.New(http.StatusForbidden, "EMAIL_VERIFICATION_REQUIRED",
		"Please verify your email address before publishing.")
}

// canManage reports whether the caller may modify this fiction.
//
// Staff are included because docs/09 §14.7 and §15 grant moderators and admins
// the same edit rights as the owner. Role alone never grants access to a
// resource - this is still a per-row check (docs/10 §19).
func canManage(identity *auth.Identity, novel *Novel) bool {
	if !identity.Authenticated() {
		return false
	}
	return novel.OwnedBy(identity.UserID()) || identity.IsStaff()
}

// canRead reports whether the caller may read this fiction, given an already
// resolved audience. A collaborator reads what they co-write (13U), private
// drafts included - one cannot edit what one cannot open.
func canRead(identity *auth.Identity, novel *Novel, audience Audience) bool {
	return novel.ReadableBy(audience) || canManage(identity, novel) ||
		novel.EditableBy(identity.UserID())
}

// audienceFor resolves what this caller brings to the visibility ladder.
//
// The follow lookup runs ONLY when the fiction is followers-only and the answer
// could still change the outcome - so the ordinary read of ordinary work costs
// exactly what it did before the ladder existed (§13C).
func (s *Service) audienceFor(
	ctx context.Context, identity *auth.Identity, novel *Novel,
) (Audience, error) {
	audience := Audience{SignedIn: identity.Authenticated()}
	if !audience.SignedIn || s.follows == nil || !novel.NeedsFollowCheck() {
		return audience, nil
	}
	// The author is inside their own audience by ownership, not by following
	// themselves - and self-follows do not exist (docs/09 §19).
	if novel.OwnedBy(identity.UserID()) {
		return audience, nil
	}

	following, err := s.follows.IsFollowing(ctx, identity.UserID(), novel.AuthorID)
	if err != nil {
		return audience, s.internal("check follow", err)
	}
	audience.Follows = following
	return audience, nil
}

// ForReader resolves a reference for reading, applying visibility.
//
// Chapters call this so that the fiction-level gate is applied exactly once, in
// one place, before any chapter is considered (docs/11 §21).
func (s *Service) ForReader(ctx context.Context, identity *auth.Identity, ref Ref) (*Novel, error) {
	novel, err := s.repo.Find(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load novel", err)
	}
	audience, err := s.audienceFor(ctx, identity, novel)
	if err != nil {
		return nil, err
	}
	if !canRead(identity, novel, audience) {
		return nil, notFound()
	}
	return novel, nil
}

// ForWriter resolves a reference for modification, applying ownership.
//
// The two-step answer is deliberate: a caller who cannot even READ the fiction
// gets the same 404 a stranger gets, and only a caller who can see it but does
// not own it gets 403.
func (s *Service) ForWriter(ctx context.Context, identity *auth.Identity, ref Ref) (*Novel, error) {
	novel, err := s.repo.Find(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load novel", err)
	}
	if canManage(identity, novel) {
		return novel, nil
	}
	audience, err := s.audienceFor(ctx, identity, novel)
	if err != nil {
		return nil, err
	}
	// A collaborator can SEE the fiction (13U), so an ownership refusal is an
	// honest 403 for them - the 404 is reserved for callers who could not
	// know the work exists.
	if novel.ReadableBy(audience) || novel.EditableBy(identity.UserID()) {
		return nil, forbidden()
	}
	return nil, notFound()
}

// ForEditor resolves a reference for CONTENT work: the owner, staff, or a
// collaborator (13U). Chapters, characters, and variables authorize through
// this - the work of co-writing - while settings, publishing, and deletion
// keep answering to ForWriter's ownership rule.
//
// The same two-step answer as ForWriter: a caller who could not even read the
// fiction gets the reader-identical 404.
func (s *Service) ForEditor(ctx context.Context, identity *auth.Identity, ref Ref) (*Novel, error) {
	novel, err := s.repo.Find(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load novel", err)
	}
	if canManage(identity, novel) || novel.EditableBy(identity.UserID()) {
		return novel, nil
	}
	audience, err := s.audienceFor(ctx, identity, novel)
	if err != nil {
		return nil, err
	}
	if novel.ReadableBy(audience) {
		return nil, forbidden()
	}
	return nil, notFound()
}

// OwnedByUser resolves a fiction for a BACKGROUND operation acting as its owner,
// by user id rather than a request Identity (the AI worker, docs/12 §27/§33).
//
// It is owner-only and non-oracle: a fiction that is missing, soft-deleted, or
// owned by someone else is the same NOVEL_NOT_FOUND. It is NOT a privileged
// bypass - it enforces the identical ownership rule canManage checks
// (novel.AuthorID == userID), keyed by user id so a job with no HTTP session can
// still act strictly as the requesting writer over their own work.
func (s *Service) OwnedByUser(ctx context.Context, userID uuid.UUID, ref Ref) (*Novel, error) {
	novel, err := s.repo.Find(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load novel", err)
	}
	if !novel.OwnedBy(userID) {
		return nil, notFound()
	}
	return novel, nil
}

// Create registers a new fiction owned by the caller.
func (s *Service) Create(
	ctx context.Context, identity *auth.Identity, input CreateInput,
) (*Record, error) {
	if !identity.Authenticated() {
		return nil, apierror.Unauthorized("Authentication required.")
	}

	errs := validationErrors{}
	validateTitle(errs, "title", input.Title)
	validateOptionalText(errs, "description", input.Description, DescriptionMaxLength)
	validateOptionalText(errs, "tagline", input.Tagline, TaglineMaxLength)
	validateOptionalText(errs, "foreword", input.Foreword, ForewordMaxLength)
	validateOptionalText(errs, "content_warning", input.ContentWarning, ContentWarningMaxLength)
	validateOptionalURL(errs, "cover_url", input.CoverURL)

	// Each format dimension defaults independently (docs/09 §15). An omitted
	// dimension is never an error, and supplying one never resets another.
	format := fiction.DefaultFormat()
	if input.StoryStructure != nil {
		format.StoryStructure = fiction.StoryStructure(*input.StoryStructure)
	}
	if input.PresentationFormat != nil {
		format.PresentationFormat = fiction.PresentationFormat(*input.PresentationFormat)
	}
	if input.ContentMode != nil {
		format.ContentMode = fiction.ContentMode(*input.ContentMode)
	}

	status := DefaultStatus
	if input.Status != nil {
		status = validateStatus(errs, *input.Status)
	}
	visibility := DefaultVisibility
	if input.Visibility != nil {
		visibility = validateVisibility(errs, *input.Visibility)
	}

	// Required here and only here (§13A): the rating is asked once, on the
	// create form, because it decides where the work may appear at all.
	rating := validateAgeRating(errs, input.AgeRating, true)
	gate := validateAgeGate(errs, input.AgeGate, rating)
	origin, fandom := validateOrigin(errs, input.OriginType, input.Fandom)
	// The collapsed section resolves against the DEFAULTS on create, so a
	// writer who never opened it gets a complete, valid state (§13K).
	extras := validateExtras(errs, input.Extras, DefaultExtras())

	// 13U display choices. show_donate defaults FALSE (13V): a money-facing
	// control is something the writer turns ON, never something they discover
	// was on.
	var themeColor *string
	if input.ThemeColor != nil && strings.TrimSpace(*input.ThemeColor) != "" {
		color := validateThemeColor(errs, *input.ThemeColor)
		themeColor = &color
	}
	spoiler := input.ContentWarningSpoiler != nil && *input.ContentWarningSpoiler
	hideCounts := input.HideCounts != nil && *input.HideCounts
	showDonate := input.ShowDonate != nil && *input.ShowDonate

	genreIDs, tagIDs := dedupeIDs(input.GenreIDs), dedupeIDs(input.TagIDs)
	s.validateTerms(ctx, errs, genreIDs, tagIDs)

	if err := errs.err(); err != nil {
		return nil, err
	}
	// Validated after the field errors so a caller sees both at once, and as a
	// COMPLETE state rather than field by field (docs/09 §15).
	if err := format.Validate(); err != nil {
		return nil, err
	}
	if err := checkPublishability(status, visibility); err != nil {
		return nil, err
	}
	if err := requireVerifiedToPublish(identity, status, visibility); err != nil {
		return nil, err
	}

	// Creating a fiction ALREADY exposed goes through the same two gates a
	// later publish does. The create form only ever makes private drafts, so
	// this costs an ordinary create nothing - but the API is the contract, and
	// a rule that only the form enforces is not a rule (§13L).
	if exposed(status, visibility) {
		if err := requireAdultAttestation(identity, rating); err != nil {
			return nil, err
		}
		draft := &Novel{
			Description:    input.Description,
			CoverURL:       input.CoverURL,
			ContentWarning: input.ContentWarning,
			AgeRating:      rating,
		}
		readiness := CheckReadiness(draft, len(genreIDs), len(tagIDs), identity)
		if err := requireReadyToPublish(readiness); err != nil {
			return nil, err
		}
	}

	title := strings.TrimSpace(input.Title)

	// Every address is a bare random token (docs/SLUGS.md, address review
	// 2026-08).
	//
	// The title used to be the address's body, which meant the address froze
	// the title as it stood at creation: rename "Test Headcanon" to "Test
	// Headcanon Collection" and the URL keeps asserting the old name forever,
	// because changing it would break every link already shared. A token
	// asserts nothing, so it never goes stale and a rename costs nothing.
	// Addresses generated before this decision keep resolving as they are.
	// The retry loop stays for the astronomically rare token collision.
	var novel *Novel
	for attempt := 0; attempt < slugAttempts; attempt++ {
		publicID, err := slug.NewPublicID()
		if err != nil {
			return nil, s.internal("generate public id", err)
		}
		token, err := slug.NewToken()
		if err != nil {
			return nil, s.internal("generate address token", err)
		}

		novel, err = s.repo.Create(ctx, CreateParams{
			AuthorID:              identity.UserID(),
			Title:                 title,
			PublicID:              publicID,
			Slug:                  token,
			Description:           input.Description,
			Tagline:               input.Tagline,
			Foreword:              input.Foreword,
			CoverURL:              input.CoverURL,
			ContentWarning:        input.ContentWarning,
			Format:                format,
			Status:                status,
			Visibility:            visibility,
			Extras:                extras,
			AgeRating:             rating,
			AgeGate:               gate,
			OriginType:            origin,
			Fandom:                fandom,
			ContentWarningSpoiler: spoiler,
			HideCounts:            hideCounts,
			ShowDonate:            showDonate,
			ThemeColor:            themeColor,
		})
		if errors.Is(err, ErrSlugTaken) {
			continue
		}
		if err != nil {
			return nil, s.internal("create novel", err)
		}
		break
	}
	if novel == nil {
		return nil, s.internal("create novel",
			fmt.Errorf("could not allocate a unique slug for %q after %d attempts", title, slugAttempts))
	}

	if len(genreIDs) > 0 {
		if err := s.repo.ReplaceGenres(ctx, novel.ID, genreIDs); err != nil {
			return nil, s.internal("assign genres", err)
		}
	}
	if len(tagIDs) > 0 {
		if err := s.repo.ReplaceTags(ctx, novel.ID, tagIDs); err != nil {
			return nil, s.internal("assign tags", err)
		}
	}

	record, err := s.repo.FindRecord(ctx, Ref{ID: novel.ID})
	if err != nil {
		return nil, s.internal("reload novel", err)
	}
	if err := s.attachTerms(ctx, record); err != nil {
		return nil, err
	}
	return record, nil
}

// validateTerms checks assignment counts and that every id names an existing
// term, reporting per-field errors (docs/09 §36).
func (s *Service) validateTerms(
	ctx context.Context, errs validationErrors, genreIDs, tagIDs []uuid.UUID,
) {
	if len(genreIDs) > MaxGenresPerNovel {
		errs.add("genre_ids", fmt.Sprintf("A fiction can have at most %d genres.", MaxGenresPerNovel))
	} else if len(genreIDs) > 0 {
		found, err := s.terms.GenreIDs(ctx, genreIDs)
		if err != nil {
			s.log.Error("novels: check genre ids failed", slog.Any("error", err))
			errs.add("genre_ids", "Could not verify genres. Please try again.")
		} else {
			for _, id := range genreIDs {
				if !found[id] {
					errs.add("genre_ids", "Unknown genre: "+id.String()+".")
				}
			}
		}
	}

	if len(tagIDs) > MaxTagsPerNovel {
		errs.add("tag_ids", fmt.Sprintf("A fiction can have at most %d tags.", MaxTagsPerNovel))
	} else if len(tagIDs) > 0 {
		found, err := s.terms.TagIDs(ctx, tagIDs)
		if err != nil {
			s.log.Error("novels: check tag ids failed", slog.Any("error", err))
			errs.add("tag_ids", "Could not verify tags. Please try again.")
		} else {
			for _, id := range tagIDs {
				if !found[id] {
					errs.add("tag_ids", "Unknown tag: "+id.String()+".")
				}
			}
		}
	}
}

// attachTerms loads genres and tags onto one record.
func (s *Service) attachTerms(ctx context.Context, record *Record) error {
	genres, tags, err := s.repo.TermsForNovels(ctx, []uuid.UUID{record.ID})
	if err != nil {
		return s.internal("load terms", err)
	}
	record.Genres, record.Tags = genres[record.ID], tags[record.ID]
	return nil
}

// attachTermsToPage loads genres and tags onto a page of records in two
// queries total (docs/07 §67).
func (s *Service) attachTermsToPage(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(records))
	for i := range records {
		ids = append(ids, records[i].ID)
	}
	genres, tags, err := s.repo.TermsForNovels(ctx, ids)
	if err != nil {
		return s.internal("load terms", err)
	}
	for i := range records {
		records[i].Genres = genres[records[i].ID]
		records[i].Tags = tags[records[i].ID]
	}
	return nil
}

// Get returns one fiction for the caller.
func (s *Service) Get(ctx context.Context, identity *auth.Identity, ref Ref) (*View, error) {
	record, err := s.repo.FindRecord(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load novel", err)
	}
	audience, err := s.audienceFor(ctx, identity, &record.Novel)
	if err != nil {
		return nil, err
	}
	if !canRead(identity, &record.Novel, audience) {
		return nil, notFound()
	}
	if err := s.attachTerms(ctx, record); err != nil {
		return nil, err
	}

	// A collaborator gets the working view - drafts counted, visibility shown
	// - because the studio opens on it (13U). IsOwner stays honest: the
	// ownership-only surfaces key off it.
	owner := canManage(identity, &record.Novel)
	editor := owner || record.Novel.EditableBy(identity.UserID())
	view := record.ViewFor(editor)
	view.IsOwner = owner
	view.CanEdit = editor

	// The public co-writer credit rides on the single-fiction view (13U).
	credits, err := s.repo.CollaboratorCredits(ctx, record.ID)
	if err != nil {
		return nil, s.internal("load collaborators", err)
	}
	if len(credits) > 0 {
		view.Collaborators = credits
	}
	return &view, nil
}

// ListQuery is the raw, unvalidated listing request from the handler.
type ListQuery struct {
	Query              string
	StoryStructure     string
	PresentationFormat string
	ContentMode        string
	Status             string
	Sort               string

	// Genre and Tag narrow by term slug (docs/09 §11). An unknown slug is an
	// empty page, not an error - the same non-oracle behaviour as Author.
	Genre string
	Tag   string

	// Author scopes the listing to one writer's public work. When it names the
	// authenticated user, their unpublished work is included as well - that is
	// the writer's own shelf.
	Author string

	// IncludeAdult is the reader turning "ซ่อนเนื้อหา 18+" off (§13B).
	//
	// It is a REQUEST, not a decision: it is honoured only for a signed-in
	// caller, and never for explicit work. A guest asking for it changes
	// nothing, which is why it can be a plain query parameter.
	IncludeAdult bool

	// CoWriter asks for the CALLER's co-written shelf (13U): fictions where
	// they are a collaborator, drafts included. It carries no username on
	// purpose - it is only ever "mine", so it cannot become a way to list
	// someone else's involvements. A guest asking gets an empty page.
	CoWriter bool

	// --- the reader-side dimensions of the 2026-08 search rework ------------

	// Rating narrows to exactly one age rating: general, teen, or mature.
	// Explicit is not offered - no browse surface lists it (§13B).
	Rating string

	// Origin is ประเภทงาน: original, fanfiction, crossover, or single (one
	// fandom only). The last two are derived from the fandom field's " × "
	// convention (docs/FANDOM.md).
	Origin string

	// Fandom and Character are free-text substring filters - the writer's own
	// words matched against the reader's own words, per docs/FANDOM.md's rule
	// that the platform keeps no vocabulary.
	Fandom    string
	Character string

	// ExcludeTag and ExcludeWarning are the "ไม่เอา" half of filtering:
	// comma-separated tag slugs, and comma-separated words the content
	// warning must not mention.
	ExcludeTag     string
	ExcludeWarning string

	// MinChapters / MaxChapters bound the live chapter count; UpdatedWithin
	// is a day count. All strings as they arrived, validated here.
	MinChapters   string
	MaxChapters   string
	UpdatedWithin string

	// HasVariables keeps only works with reader variables (y/n).
	HasVariables bool
}

// maxSearchLength bounds the search term (docs/09 §36 "Search query limits").
const maxSearchLength = 100

// List returns a page of fictions the caller may see.
func (s *Service) List(
	ctx context.Context, identity *auth.Identity, query ListQuery, page pagination.Params,
) ([]View, pagination.Meta, error) {
	return s.listWithFilter(ctx, identity, query, page, false)
}

// filterSlugList validates one comma-separated slug list (search review
// 2026-08: multi-term filters). Blank entries are dropped; a malformed slug or
// an oversized list is a field error.
func filterSlugList(errs validationErrors, field, raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	slugs := []string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !slug.Valid(part) {
			errs.add(field, "Unsupported value.")
			return nil
		}
		slugs = append(slugs, part)
	}
	if len(slugs) > 10 {
		errs.add(field, "At most 10 values.")
		return nil
	}
	return slugs
}

// filterFrom validates the listing/search query into a repository Filter.
// Shared by the listing pipeline and the facets endpoint, so both always
// agree on what a parameter means.
func filterFrom(identity *auth.Identity, query ListQuery, searchAll bool) (Filter, error) {
	errs := validationErrors{}

	filter := Filter{
		Query:     strings.TrimSpace(query.Query),
		SearchAll: searchAll,
		Sort:      DefaultSort,
		// Both are resolved from the IDENTITY, never taken from the request as
		// given: the viewer decides two visibility rungs, and only a signed-in
		// reader can lift the 18+ exclusion (§13B, §13C).
		Viewer:        identity.UserID(),
		IncludeMature: query.IncludeAdult && identity.Authenticated(),
	}
	if len(filter.Query) > maxSearchLength {
		errs.add("q", fmt.Sprintf("Search must be at most %d characters.", maxSearchLength))
	}

	// Format filters are validated against the same vocabulary the columns use,
	// so an unsupported value is a clean 422 rather than a silently empty page
	// (docs/09 §11, §36).
	if query.StoryStructure != "" {
		if !fiction.StoryStructure(query.StoryStructure).Valid() {
			errs.add("story_structure", "Unsupported value.")
		}
		filter.StoryStructure = query.StoryStructure
	}
	if query.PresentationFormat != "" {
		if !fiction.PresentationFormat(query.PresentationFormat).Valid() {
			errs.add("presentation_format", "Unsupported value.")
		}
		filter.PresentationFormat = query.PresentationFormat
	}
	if query.ContentMode != "" {
		if !fiction.ContentMode(query.ContentMode).Valid() {
			errs.add("content_mode", "Unsupported value.")
		}
		filter.ContentMode = query.ContentMode
	}
	if query.Status != "" {
		if !Status(query.Status).Valid() {
			errs.add("status", "Unsupported value.")
		}
		filter.Status = query.Status
	}
	if query.Sort != "" {
		if !ValidSort(query.Sort) {
			errs.add("sort", fmt.Sprintf("Must be one of: %s.", strings.Join(SortOptions(), ", ")))
		} else {
			filter.Sort = query.Sort
		}
	}

	// Term filters are validated by SHAPE only; whether the slug names a real
	// term is answered by the result set. A malformed value can never match, so
	// it is a clean 422 rather than a guaranteed-empty query (docs/09 §36).
	// Both accept a comma-separated list (search review 2026-08): every named
	// term must match, and excluded tags must all be absent.
	filter.GenreSlugs = filterSlugList(errs, "genre", query.Genre)
	filter.TagSlugs = filterSlugList(errs, "tag", query.Tag)
	filter.ExcludeTagSlugs = filterSlugList(errs, "exclude_tag", query.ExcludeTag)

	if query.Rating != "" {
		switch AgeRating(query.Rating) {
		case RatingGeneral, RatingTeen, RatingMature:
			filter.Rating = query.Rating
		default:
			// Explicit is deliberately not accepted: no browse surface lists
			// it (§13B), so a filter for it would only ever be an empty page
			// pretending to be a feature.
			errs.add("rating", "Unsupported value.")
		}
	}
	if query.Origin != "" {
		switch query.Origin {
		case "original", "fanfiction", "crossover", "single":
			filter.Origin = query.Origin
		default:
			errs.add("origin", "Unsupported value.")
		}
	}

	if fandom := strings.TrimSpace(query.Fandom); fandom != "" {
		if utf8.RuneCountInString(fandom) > 120 {
			errs.add("fandom", "Must be at most 120 characters.")
		}
		filter.Fandom = fandom
	}
	if character := strings.TrimSpace(query.Character); character != "" {
		if utf8.RuneCountInString(character) > 120 {
			errs.add("character", "Must be at most 120 characters.")
		}
		filter.Character = character
	}

	if query.ExcludeWarning != "" {
		words := []string{}
		for _, part := range strings.Split(query.ExcludeWarning, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if utf8.RuneCountInString(part) > 60 {
				errs.add("exclude_warning", "Each word must be at most 60 characters.")
				break
			}
			words = append(words, part)
		}
		if len(words) > 10 {
			errs.add("exclude_warning", "At most 10 words.")
		} else {
			filter.ExcludeWarnings = words
		}
	}

	filter.MinChapters = filterCount(errs, "min_chapters", query.MinChapters)
	filter.MaxChapters = filterCount(errs, "max_chapters", query.MaxChapters)
	if filter.MinChapters > 0 && filter.MaxChapters > 0 &&
		filter.MinChapters > filter.MaxChapters {
		errs.add("min_chapters", "Must not exceed max_chapters.")
	}

	if query.UpdatedWithin != "" {
		days, err := strconv.Atoi(query.UpdatedWithin)
		if err != nil || days < 1 || days > 366 {
			errs.add("updated_within", "Must be a day count between 1 and 366.")
		} else {
			filter.UpdatedWithinDays = days
		}
	}

	filter.HasVariables = query.HasVariables

	return filter, errs.err()
}

// filterCount parses one non-negative bound. Empty means unbounded (zero).
func filterCount(errs validationErrors, field, raw string) int {
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > 100000 {
		errs.add(field, "Must be a number between 0 and 100000.")
		return 0
	}
	return value
}

// listWithFilter is the shared listing pipeline behind List and Search;
// searchAll selects the widened match scope of docs/01 §7.
func (s *Service) listWithFilter(
	ctx context.Context, identity *auth.Identity, query ListQuery, page pagination.Params,
	searchAll bool,
) ([]View, pagination.Meta, error) {
	filter, err := filterFrom(identity, query, searchAll)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	if query.CoWriter {
		// The co-writer shelf (13U): resolved from the identity alone. For a
		// guest there is nothing to list, and an empty page says so without
		// becoming an authentication oracle.
		if !identity.Authenticated() {
			return []View{}, page.MetaFor(0), nil
		}
		filter.CoWriterID = identity.UserID()
		filter.IncludeUnpublished = true
	} else if author := strings.TrimSpace(query.Author); author != "" {
		user, err := s.users.FindByUsername(ctx, author)
		if errors.Is(err, users.ErrNotFound) {
			// An unknown author is an empty page, not an error: a 404 here would
			// turn the listing into a username oracle.
			return []View{}, page.MetaFor(0), nil
		}
		if err != nil {
			return nil, pagination.Meta{}, s.internal("resolve author", err)
		}
		filter.AuthorID = user.ID

		// Set only here and in the co-writer branch above, and never from the
		// request as given: it requires the listing to be scoped to the
		// authenticated user, so a guest can never reach it.
		filter.IncludeUnpublished = identity.Authenticated() && identity.UserID() == user.ID
	}

	records, total, err := s.repo.List(ctx, filter, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list novels", err)
	}
	if err := s.attachTermsToPage(ctx, records); err != nil {
		return nil, pagination.Meta{}, err
	}

	views := make([]View, 0, len(records))
	for i := range records {
		owner := canManage(identity, &records[i].Novel)
		// Every row of the co-writer shelf is co-written by the caller - the
		// filter guarantees it - so the shelf renders the working view (13U).
		editor := owner || query.CoWriter
		view := records[i].ViewFor(editor)
		view.IsOwner = owner
		view.CanEdit = editor
		views = append(views, view)
	}
	return views, page.MetaFor(total), nil
}

// Search implements GET /search/novels (docs/09 §22) - search as its own
// concern, with the widened match scope of docs/01 §7: title, description,
// author, and genre/tag names.
//
// It reuses the listing pipeline (same filters, same sorts, same guest-first
// visibility), differing only in requiring a query and matching broadly. ILIKE
// substring matching is deliberate: PostgreSQL full-text search cannot segment
// Thai, substring matching handles it correctly, and docs/09 §22 names FTS as
// a potential future step, not a requirement.
func (s *Service) Search(
	ctx context.Context, identity *auth.Identity, query ListQuery, page pagination.Params,
) ([]View, pagination.Meta, error) {
	if strings.TrimSpace(query.Query) == "" {
		return nil, pagination.Meta{}, apierror.Validation(map[string][]string{
			"q": {"A search query is required."},
		})
	}
	// Search never includes anyone's unpublished work: drop the author scoping
	// that List uses for the writer's own shelf.
	query.Author = ""
	// With a text query and no explicit choice, relevance beats recency: an
	// exact title hit should never rank below an old work whose description
	// happens to contain the term (search review 2026-08 section B).
	if query.Sort == "" {
		query.Sort = "relevance"
	}
	views, meta, err := s.listWithFilter(ctx, identity, query, page, true)
	return views, meta, err
}

// SearchFacets implements GET /search/facets: the filter-panel counts for the
// same parameter set the search takes (search review 2026-08 section A). A
// text query is NOT required - the panel also serves filter-only browsing.
func (s *Service) SearchFacets(
	ctx context.Context, identity *auth.Identity, query ListQuery,
) (*Facets, error) {
	// Facets describe the PUBLIC result set only, like search itself.
	query.Author = ""
	query.CoWriter = false

	filter, err := filterFrom(identity, query, true)
	if err != nil {
		return nil, err
	}

	// The คู่ชิป dimension releases its own selections, per the faceting rule.
	// Which genre slugs ARE relationship-kind takes the taxonomy to answer,
	// so it is resolved here rather than in the repository.
	relationship, err := s.repo.RelationshipSlugs(ctx)
	if err != nil {
		return nil, s.internal("list relationship slugs", err)
	}
	isRelationship := map[string]bool{}
	for _, value := range relationship {
		isRelationship[value] = true
	}
	relationshipFilter := filter
	kept := []string{}
	for _, value := range filter.GenreSlugs {
		if !isRelationship[value] {
			kept = append(kept, value)
		}
	}
	relationshipFilter.GenreSlugs = kept

	facets, err := s.repo.Facets(ctx, filter, relationshipFilter)
	if err != nil {
		return nil, s.internal("compute search facets", err)
	}
	return facets, nil
}

// Update applies a partial metadata update.
//
// It cannot change the format - that has its own endpoint so the resulting
// format state is validated as a whole and the change stays auditable
// (docs/09 §15).
func (s *Service) Update(
	ctx context.Context, identity *auth.Identity, ref Ref, input UpdateInput,
) (*View, error) {
	novel, err := s.ForWriter(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	errs := validationErrors{}
	params := UpdateParams{
		Description:           input.Description,
		Tagline:               input.Tagline,
		Foreword:              input.Foreword,
		CoverURL:              input.CoverURL,
		ContentWarning:        input.ContentWarning,
		ContentWarningSpoiler: input.ContentWarningSpoiler,
		HideCounts:            input.HideCounts,
		ShowDonate:            input.ShowDonate,
	}

	// theme_color (13U): absent / null (clear) / value, validated when set.
	if input.ThemeColor != nil {
		if *input.ThemeColor == nil {
			params.ThemeColor = ptrTo[*string](nil)
		} else {
			color := validateThemeColor(errs, **input.ThemeColor)
			params.ThemeColor = ptrTo(&color)
		}
	}

	if input.Title != nil {
		validateTitle(errs, "title", *input.Title)
		trimmed := strings.TrimSpace(*input.Title)
		params.Title = &trimmed
	}
	if input.Description != nil {
		validateOptionalText(errs, "description", *input.Description, DescriptionMaxLength)
	}
	if input.Tagline != nil {
		validateOptionalText(errs, "tagline", *input.Tagline, TaglineMaxLength)
	}
	if input.Foreword != nil {
		validateOptionalText(errs, "foreword", *input.Foreword, ForewordMaxLength)
	}
	if input.ContentWarning != nil {
		validateOptionalText(errs, "content_warning", *input.ContentWarning, ContentWarningMaxLength)
	}
	if input.CoverURL != nil {
		validateOptionalURL(errs, "cover_url", *input.CoverURL)
	}

	// Creation fields (§13A). The origin/fandom pair is resolved together
	// because the database CHECK requires them to stay coherent: a work that
	// stops being fanfiction has its source cleared here rather than failing at
	// the constraint.
	// The rating and the gate are resolved TOGETHER against the resulting
	// state, because explicit work has a floor the gate must clear: changing
	// only the rating can invalidate a gate that was legal a moment ago, and a
	// half-checked pair would be caught by the CHECK constraint as a 500
	// instead of a field error (§13B).
	rating := novel.AgeRating
	if input.AgeRating != nil {
		rating = validateAgeRating(errs, *input.AgeRating, false)
		params.AgeRating = &rating
	}
	if input.AgeGate != nil {
		gate := validateAgeGate(errs, *input.AgeGate, rating)
		params.AgeGate = &gate
	} else if !GateSatisfies(rating, novel.AgeGate) {
		// The rating moved up under a gate that no longer clears it. Raising
		// the gate silently would be deciding for the author; this asks.
		errs.add("age_gate",
			"เรตนี้ต้องให้ผู้อ่านล็อกอินก่อน - เลือกการเข้าถึงใหม่พร้อมกับเรต")
	}
	applyOrigin(errs, novel, input, &params)

	// The collapsed section resolves against the row as it stands, so an edit
	// that mentions one field leaves the other twelve exactly as they were
	// (§13K). It is written whole or not at all - series_name and
	// series_position are tied by a CHECK and must never disagree.
	if extras := validateExtras(errs, input.Extras, novel.Extras); extras != novel.Extras {
		params.Extras = &extras
	}

	// Term assignments: a present slice replaces the whole set; nil leaves it
	// alone (docs/09 §3 PATCH semantics).
	var genreIDs, tagIDs []uuid.UUID
	if input.GenreIDs != nil {
		genreIDs = dedupeIDs(*input.GenreIDs)
	}
	if input.TagIDs != nil {
		tagIDs = dedupeIDs(*input.TagIDs)
	}
	s.validateTerms(ctx, errs, genreIDs, tagIDs)

	// The resulting state is validated, not the individual fields: a request
	// that supplies only one of status/visibility still has to produce a legal
	// pair with whatever the row already holds.
	status, visibility := novel.Status, novel.Visibility
	if input.Status != nil {
		status = validateStatus(errs, *input.Status)
		params.Status = &status
	}
	if input.Visibility != nil {
		visibility = validateVisibility(errs, *input.Visibility)
		params.Visibility = &visibility
	}

	// นามปากกา (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2). Choosing which
	// identity a work is published under is a METADATA change: it writes one
	// column and touches no chapter, message, or revision - the same rule a
	// format change follows.
	//
	// The id is checked against the fiction's AUTHOR, not merely against the
	// caller: the name on a cover has to be one the person who wrote it owns,
	// and staff editing someone's fiction can never attach their own identity
	// to it. A null returns the work to the author's default.
	if input.PenNameID != nil {
		if *input.PenNameID == nil {
			params.PenNameID = ptrTo[*uuid.UUID](nil)
		} else {
			owned, err := s.repo.PenNameBelongsTo(ctx, novel.AuthorID, **input.PenNameID)
			if err != nil {
				return nil, s.internal("check pen name", err)
			}
			if !owned {
				errs.add("pen_name_id", "That pen name is not one of yours.")
			} else {
				params.PenNameID = ptrTo(*input.PenNameID)
			}
		}
	}

	if err := errs.err(); err != nil {
		return nil, err
	}
	if err := checkPublishability(status, visibility); err != nil {
		return nil, err
	}

	// Only a transition that newly exposes the work requires a verified address.
	// Editing a draft, or editing already-published work, does not.
	nowExposed := exposed(status, visibility)
	wasExposed := exposed(novel.Status, novel.Visibility)
	params.Exposed = nowExposed

	// ตั้งเวลาเผยแพร่ (13U). A schedule is part of a PUBLISH decision, so it
	// is only accepted alongside an exposed resulting state, and it must be in
	// the future - a past time is a publish, and the ordinary publish is how
	// that is asked for. Moving the work back to private clears any pending
	// schedule: an archived fiction that silently went public later would be
	// the worst surprise this platform could arrange.
	if input.PublishAt != nil {
		if *input.PublishAt == nil {
			params.PublishAt = ptrTo[*time.Time](nil)
		} else {
			when := (*input.PublishAt).UTC()
			switch {
			case !nowExposed:
				return nil, apierror.Validation(map[string][]string{
					"publish_at": {"ตั้งเวลาได้เฉพาะตอนกำลังเผยแพร่ - เลือกการมองเห็นที่ไม่ใช่ส่วนตัวพร้อมกัน"},
				})
			case !when.After(time.Now()):
				return nil, apierror.Validation(map[string][]string{
					"publish_at": {"เวลาที่ตั้งต้องอยู่ในอนาคต - ถ้าต้องการเผยแพร่ทันที ไม่ต้องตั้งเวลา"},
				})
			}
			params.PublishAt = ptrTo(&when)
			// The publish stamp is the moment readers could first open it.
			params.ExposedAt = &when
		}
	} else if !nowExposed && novel.PublishAt != nil {
		params.PublishAt = ptrTo[*time.Time](nil)
	}

	// The attestation is checked on the publish AND on a later move up to 18+:
	// work that becomes adult after publication is the same claim, made later.
	if nowExposed && (!wasExposed || params.AgeRating != nil) {
		if err := requireAdultAttestation(identity, rating); err != nil {
			return nil, err
		}
	}

	if nowExposed && !wasExposed {
		if err := requireVerifiedToPublish(identity, status, visibility); err != nil {
			return nil, err
		}
		// The second gate (§13L). It runs only on the transition that first
		// exposes the work: editing a draft, or editing something already
		// published, is never blocked by it - nothing here may stop a writer
		// from writing.
		readiness, err := s.readinessFor(ctx, novel, identity, params, genreIDs, tagIDs,
			input.GenreIDs != nil, input.TagIDs != nil)
		if err != nil {
			return nil, err
		}
		if err := requireReadyToPublish(readiness); err != nil {
			return nil, err
		}
	}

	if _, err := s.repo.Update(ctx, novel.ID, params); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, notFound()
		}
		return nil, s.internal("update novel", err)
	}

	if input.GenreIDs != nil {
		if err := s.repo.ReplaceGenres(ctx, novel.ID, genreIDs); err != nil {
			return nil, s.internal("assign genres", err)
		}
	}
	if input.TagIDs != nil {
		if err := s.repo.ReplaceTags(ctx, novel.ID, tagIDs); err != nil {
			return nil, s.internal("assign tags", err)
		}
	}

	record, err := s.repo.FindRecord(ctx, Ref{ID: novel.ID})
	if err != nil {
		return nil, s.internal("reload novel", err)
	}
	if err := s.attachTerms(ctx, record); err != nil {
		return nil, err
	}
	view := record.ViewFor(true)

	// ปิดจบ (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3). The TRANSITION into
	// completed, not the state: re-saving a finished work signals nothing, so
	// this cannot be farmed by toggling a status back and forth.
	if s.achievements != nil && status == StatusCompleted && novel.Status != StatusCompleted {
		s.achievements.FictionCompleted(ctx, identity.UserID(), view.ChapterCount)
	}
	return &view, nil
}

// UpdateFormat changes format metadata and nothing else.
//
// This is the endpoint docs/09 §14.7 specifies. It is METADATA-ONLY by
// construction: it issues a single UPDATE against `novels` and holds no code
// path that could reach `chapters`, `chapter_messages`, or `chapter_revisions`.
// Prose and chat messages both survive every transition untouched
// (docs/08 §3.1, docs/15 §5.7).
func (s *Service) UpdateFormat(
	ctx context.Context, identity *auth.Identity, ref Ref, patch fiction.Patch,
) (*FormatView, error) {
	novel, err := s.ForWriter(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	if patch.IsEmpty() {
		return nil, apierror.Validation(map[string][]string{
			"format": {"Supply at least one of: story_structure, presentation_format, content_mode."},
		})
	}

	// Apply validates the RESULTING state, so an omitted dimension keeps its
	// current value and is never silently reset (docs/09 §14.7).
	next, err := patch.Apply(novel.Format)
	if err != nil {
		return nil, err
	}

	updated := novel
	if next != novel.Format {
		// A compare-and-set against the format we validated. A concurrent change
		// makes this match no row, and the caller is asked to retry rather than
		// having their partial view of the format silently win (docs/09 §34).
		updated, err = s.repo.UpdateFormat(ctx, novel.ID, novel.Format, next)
		if errors.Is(err, ErrNotFound) {
			return nil, apierror.Conflict("This fiction's format changed while you were editing it. Please try again.")
		}
		if err != nil {
			return nil, s.internal("update format", err)
		}

		s.log.Info("fiction format changed",
			slog.String("novel_id", novel.ID.String()),
			slog.String("actor_id", identity.UserID().String()),
			slog.String("from", formatLabel(novel.Format)),
			slog.String("to", formatLabel(next)),
		)
	}

	view := &FormatView{ID: updated.ID, Format: updated.Format}

	// docs/08 §11: when a fiction presents as chat but no chat content has been
	// prepared, warn the author. This is a hint for the writer UI; it triggers
	// nothing and converts nothing.
	if updated.Format.UsesStructuredMessages() {
		prepared, err := s.repo.HasChatContent(ctx, updated.ID)
		if err != nil {
			return nil, s.internal("check chat content", err)
		}
		view.NeedsChatSetup = !prepared
	}
	return view, nil
}

// Readiness returns the pre-publish checklist for a fiction (§13L).
//
// Owner-only, because it is a work list for the person doing the work. It is a
// READ: it never changes anything, and a writer may look at it as often as they
// like while a draft is still a draft.
func (s *Service) Readiness(
	ctx context.Context, identity *auth.Identity, ref Ref,
) (*Readiness, error) {
	novel, err := s.ForWriter(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	genres, tags, err := s.repo.TermsForNovels(ctx, []uuid.UUID{novel.ID})
	if err != nil {
		return nil, s.internal("load terms", err)
	}

	readiness := CheckReadiness(novel, len(genres[novel.ID]), len(tags[novel.ID]), identity)
	return &readiness, nil
}

// readinessFor evaluates the checklist against the state the request is ABOUT
// to produce, not the state on disk.
//
// That distinction is the whole usability of the gate: a writer who fills in
// the synopsis and presses publish in one request must not be told the synopsis
// is missing.
func (s *Service) readinessFor(
	ctx context.Context, novel *Novel, identity *auth.Identity, params UpdateParams,
	genreIDs, tagIDs []uuid.UUID, genresReplaced, tagsReplaced bool,
) (Readiness, error) {
	stored, storedTags, err := s.repo.TermsForNovels(ctx, []uuid.UUID{novel.ID})
	if err != nil {
		return Readiness{}, s.internal("load terms", err)
	}

	genreCount := len(stored[novel.ID])
	if genresReplaced {
		genreCount = len(genreIDs)
	}
	tagCount := len(storedTags[novel.ID])
	if tagsReplaced {
		tagCount = len(tagIDs)
	}

	// A copy carrying this request's values, so the checklist judges what the
	// writer is submitting.
	next := *novel
	if params.Description != nil {
		next.Description = *params.Description
	}
	if params.CoverURL != nil {
		next.CoverURL = *params.CoverURL
	}
	if params.ContentWarning != nil {
		next.ContentWarning = *params.ContentWarning
	}
	if params.AgeRating != nil {
		next.AgeRating = *params.AgeRating
	}

	return CheckReadiness(&next, genreCount, tagCount, identity), nil
}

// Delete soft-deletes a fiction (docs/08 §37, docs/09 §15).
func (s *Service) Delete(ctx context.Context, identity *auth.Identity, ref Ref) error {
	novel, err := s.ForWriter(ctx, identity, ref)
	if err != nil {
		return err
	}
	if err := s.repo.SoftDelete(ctx, novel.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound()
		}
		return s.internal("delete novel", err)
	}
	return nil
}

// MaxCollaborators bounds one fiction's co-writer list (13U). A team of ten is
// a writing circle; beyond that it is a distribution list.
const MaxCollaborators = 10

// CollaboratorCreditMaxLength bounds the credit wording.
const CollaboratorCreditMaxLength = 120

// ListCollaborators returns the fiction's co-writer credit, for its editors.
//
// The OWNER manages the list; a collaborator may see it (they are on it), and
// everyone else gets it through the fiction view where it is already public.
func (s *Service) ListCollaborators(
	ctx context.Context, identity *auth.Identity, ref Ref,
) ([]CollaboratorCredit, error) {
	novel, err := s.ForEditor(ctx, identity, ref)
	if err != nil {
		return nil, err
	}
	credits, err := s.repo.CollaboratorCredits(ctx, novel.ID)
	if err != nil {
		return nil, s.internal("list collaborators", err)
	}
	return credits, nil
}

// AddCollaborator attaches a co-writer by username (13U). Owner only: the
// collaborator list is one of the things collaborating does not grant.
func (s *Service) AddCollaborator(
	ctx context.Context, identity *auth.Identity, ref Ref, username, credit string,
) ([]CollaboratorCredit, error) {
	novel, err := s.ForWriter(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	username = strings.TrimSpace(username)
	credit = strings.TrimSpace(credit)
	errs := validationErrors{}
	if username == "" {
		errs.add("username", "ใส่ชื่อผู้ใช้ของคนที่จะเพิ่ม")
	}
	if utf8.RuneCountInString(credit) > CollaboratorCreditMaxLength {
		errs.add("credit",
			fmt.Sprintf("เครดิตยาวเกินไป (ไม่เกิน %d ตัวอักษร)", CollaboratorCreditMaxLength))
	}
	if strings.EqualFold(username, identity.User.Username) {
		errs.add("username", "คุณเป็นเจ้าของเรื่องอยู่แล้ว")
	}
	if len(novel.CollaboratorIDs) >= MaxCollaborators {
		errs.add("username", fmt.Sprintf("เพิ่มผู้เขียนร่วมได้สูงสุด %d คน", MaxCollaborators))
	}
	if err := errs.err(); err != nil {
		return nil, err
	}

	if err := s.repo.AddCollaboratorByUsername(ctx, novel.ID, username, credit); err != nil {
		if errors.Is(err, ErrCollaboratorUser) {
			return nil, apierror.Validation(map[string][]string{
				"username": {"ไม่พบบัญชีชื่อนี้ - ตรวจตัวสะกดอีกครั้ง"},
			})
		}
		return nil, s.internal("add collaborator", err)
	}

	credits, err := s.repo.CollaboratorCredits(ctx, novel.ID)
	if err != nil {
		return nil, s.internal("list collaborators", err)
	}
	return credits, nil
}

// RemoveCollaborator detaches a co-writer by username. Owner only. Their
// past writing stays exactly where it is - removing access is not removing
// work (writer-first: nothing here deletes content).
func (s *Service) RemoveCollaborator(
	ctx context.Context, identity *auth.Identity, ref Ref, username string,
) error {
	novel, err := s.ForWriter(ctx, identity, ref)
	if err != nil {
		return err
	}
	if err := s.repo.RemoveCollaboratorByUsername(ctx, novel.ID, strings.TrimSpace(username)); err != nil {
		return s.internal("remove collaborator", err)
	}
	return nil
}

// SetCover points a fiction at a newly uploaded cover (docs/08 §7.1
// cover_url, Phase 9). The media domain calls this so ownership stays
// enforced HERE - the same owner-or-staff rule and non-oracle 404 every
// other write path applies. nil clears the cover.
//
// It deliberately does not revalidate the URL shape: the value comes from the
// media service's own configuration, not from a client.
func (s *Service) SetCover(
	ctx context.Context, identity *auth.Identity, ref Ref, coverURL *string,
) error {
	novel, err := s.ForWriter(ctx, identity, ref)
	if err != nil {
		return err
	}
	if _, err := s.repo.Update(ctx, novel.ID, UpdateParams{CoverURL: &coverURL}); err != nil {
		return s.internal("set cover", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Moderation (docs/08 §24, Phase 8)
//
// Novels have no hidden/removed status axis - their only platform-level
// state is the soft delete, which staff could already apply through Delete
// (docs/09 §14.7). These methods expose that same capability to the
// moderation service BY ID with moderation semantics: "already removed" is a
// conflict rather than a 404, and restore exists at all. Both are staff-only;
// ownership never plays a part.
// ---------------------------------------------------------------------------

// ModerateRemove soft-deletes a fiction as a moderation action and returns
// its author (the moderation notification's recipient).
func (s *Service) ModerateRemove(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (uuid.UUID, error) {
	novel, err := s.forModeration(ctx, identity, id)
	if err != nil {
		return uuid.Nil, err
	}
	if novel.DeletedAt != nil {
		return uuid.Nil, apierror.Conflict("The fiction is already removed.")
	}
	if err := s.repo.SoftDelete(ctx, novel.ID); err != nil {
		return uuid.Nil, s.internal("moderate-remove novel", err)
	}
	return novel.AuthorID, nil
}

// ModerateRestore clears a moderation removal. It deliberately cannot tell a
// moderator's removal from the author's own deletion - the schema has one
// deleted_at (docs/08 §7.1) - so restoring is a staff judgement call made
// with the audit trail in view.
func (s *Service) ModerateRestore(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (uuid.UUID, error) {
	novel, err := s.forModeration(ctx, identity, id)
	if err != nil {
		return uuid.Nil, err
	}
	if novel.DeletedAt == nil {
		return uuid.Nil, apierror.Conflict("The fiction is not removed.")
	}
	if err := s.repo.Restore(ctx, novel.ID); err != nil {
		return uuid.Nil, s.internal("moderate-restore novel", err)
	}
	return novel.AuthorID, nil
}

// forModeration loads a fiction for a staff state change, soft-deleted or not.
func (s *Service) forModeration(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (*Novel, error) {
	if !identity.IsStaff() {
		return nil, apierror.Forbidden("You do not have permission to do that.")
	}
	novel, err := s.repo.FindIncludingDeleted(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load novel for moderation", err)
	}
	return novel, nil
}

// checkPublishability rejects a status/visibility pair that would make an
// unfinished draft reachable.
//
// The database carries the same rule as a CHECK constraint; this exists so the
// writer gets an explanatory 422 rather than the constraint's 500.
func checkPublishability(status Status, visibility Visibility) error {
	if status.IsDraft() && visibility != VisibilityPrivate {
		return apierror.Validation(map[string][]string{
			"visibility": {"A draft cannot be shared. Change the status first, or keep it private."},
		})
	}
	return nil
}

// requireVerifiedToPublish enforces the Phase 1 decision recorded in
// docs/AUTHENTICATION.md §9: email verification gates PUBLISHING, never reading
// or ordinary account use.
func requireVerifiedToPublish(identity *auth.Identity, status Status, visibility Visibility) error {
	if status.IsDraft() && visibility == VisibilityPrivate {
		return nil
	}
	if identity.EmailVerified() || identity.IsStaff() {
		return nil
	}
	return verificationRequired()
}

// requireAdultAttestation refuses to publish 18+ work for an account that has
// not stated it belongs to an adult (§13B).
//
// It is asked ONCE per account, at the profile, and never again - which is why
// it is a gate here rather than a field on the fiction. What is stored is a
// timestamp and nothing else: no date of birth, no document, no third party.
// docs/11 §34 does not permit collecting more than the question needs, and the
// question is yes or no.
//
// Staff are exempt for the same reason they are exempt from the verification
// gate: they act on other people's work, and blocking a moderator on the
// author's paperwork would make moderation depend on it.
func requireAdultAttestation(identity *auth.Identity, rating AgeRating) error {
	if !rating.Adult() || identity.IsStaff() {
		return nil
	}
	if identity.Authenticated() && identity.User.AdultAttested() {
		return nil
	}
	return apierror.New(http.StatusForbidden, "ADULT_ATTESTATION_REQUIRED",
		"ยืนยันว่าคุณอายุ 18 ปีขึ้นไปที่หน้าโปรไฟล์ก่อนเผยแพร่งาน 18+")
}

func formatLabel(f fiction.Format) string {
	return fmt.Sprintf("%s+%s+%s", f.StoryStructure, f.PresentationFormat, f.ContentMode)
}

// internal logs the cause and returns an opaque error.
//
// docs/09 §39 and docs/11 §67: internal detail belongs in the logs, never in a
// response body.
func (s *Service) internal(op string, err error) error {
	s.log.Error("novels: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

// Compile-time assurance that the real repositories satisfy the narrow
// interfaces this service declares.
var (
	_ UserLookup = (*users.Repository)(nil)
	_ TermLookup = (*taxonomy.Repository)(nil)
)
