package characters

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// NovelAccess is the slice of the novels domain this service needs.
//
// Consumer-defined, like the chapters service's own gate, so the dependency
// stays one-directional (characters -> novels, never back) and the authorization
// boundary is explicit: every operation begins by asking the novels service
// whether the caller may read or write the parent fiction.
type NovelAccess interface {
	ForReader(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
	ForEditor(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
}

// Achiever is the slice of the achievements domain this service needs
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3). Consumer-defined, like NovelAccess
// above it, so characters never imports achievements.
//
// This side answers only "does this sheet say anything beyond a name" - a fact
// about this domain's own data. How many such sheets make a นักสร้างโลก is the
// other side's business.
type Achiever interface {
	CharacterDetailed(ctx context.Context, authorID uuid.UUID)
}

// Service owns character business rules and is the authorization boundary for
// everything under /novels/:novel/characters.
type Service struct {
	repo   *Repository
	novels NovelAccess
	log    *slog.Logger

	// achievements is optional and set after construction. nil records nothing.
	achievements Achiever
}

func NewService(repo *Repository, novelAccess NovelAccess, log *slog.Logger) *Service {
	return &Service{repo: repo, novels: novelAccess, log: log}
}

// SetAchiever attaches the achievement service after construction.
func (s *Service) SetAchiever(achiever Achiever) { s.achievements = achiever }

// filledBeyondName reports whether a cast member's sheet says anything at all
// besides who they are called. นักสร้างโลก counts CHARACTERS, not rows: ten
// bare names are a list, and the achievement is meant to notice the writer who
// actually wrote them (Part 3 - nothing rewards volume).
func filledBeyondName(record *Character) bool {
	if len(record.Traits) > 0 || len(record.Details) > 0 {
		return true
	}
	for _, field := range []*string{
		record.Role, record.Summary, record.Description, record.Quote,
	} {
		if field != nil && strings.TrimSpace(*field) != "" {
			return true
		}
	}
	return false
}

// notFound is the response for a character the caller may not see.
//
// Identical whether the character is absent or belongs to a fiction the caller
// cannot read, so the endpoint cannot be used to probe for the existence of
// someone's private work (docs/11 §3.4, §31).
func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "CHARACTER_NOT_FOUND", "Character not found.")
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("characters: "+op, slog.Any("error", err))
	return apierror.Internal()
}

// List returns a fiction's cast.
//
// The fiction gate runs first and decides everything: a reader who may not see
// the fiction gets its 404, not an empty list.
func (s *Service) List(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
) ([]View, error) {
	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	list, err := s.repo.ListForNovel(ctx, novel.ID)
	if err != nil {
		return nil, s.internal("list", err)
	}

	// One grouped query, not one per card - the studio's cast list shows each
	// character's appearances (count on the closed card, ticks in the timeline)
	// without a follow-up read per character.
	appearances, err := s.repo.AppearancesForNovel(ctx, novel.ID)
	if err != nil {
		return nil, s.internal("list appearances", err)
	}

	views := make([]View, 0, len(list))
	for i := range list {
		view := list[i].View()
		view.AppearsIn = make([]string, 0, len(appearances[list[i].ID]))
		for _, id := range appearances[list[i].ID] {
			view.AppearsIn = append(view.AppearsIn, id.String())
		}
		views = append(views, view)
	}
	return views, nil
}

// Get returns one character, with the chapters they appear in.
func (s *Service) Get(
	ctx context.Context, identity *auth.Identity, ref novels.Ref, characterID uuid.UUID,
) (*View, error) {
	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	character, err := s.repo.Find(ctx, novel.ID, characterID)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("get", err)
	}

	appearances, err := s.repo.AppearancesFor(ctx, character.ID)
	if err != nil {
		return nil, s.internal("appearances", err)
	}

	view := character.View()
	view.AppearsIn = make([]string, 0, len(appearances))
	for _, id := range appearances {
		view.AppearsIn = append(view.AppearsIn, id.String())
	}
	return &view, nil
}

// Create adds a character to the caller's own fiction.
func (s *Service) Create(
	ctx context.Context, identity *auth.Identity, ref novels.Ref, input CreateInput,
) (*View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	errs := map[string][]string{}
	name := validateName(input.Name, errs)
	record := &Character{
		NovelID:     novel.ID,
		Name:        name,
		Role:        normalise(input.Role, roleMaxLength, "role", errs),
		Summary:     normalise(input.Summary, summaryMaxLength, "summary", errs),
		AvatarURL:   validateAvatarURL(normalise(input.AvatarURL, avatarURLMaxLength, "avatar_url", errs), errs),
		Description: normalise(input.Description, descriptionMaxLength, "description", errs),
		Quote:       normalise(input.Quote, quoteMaxLength, "quote", errs),
		Traits:      validateTraits(input.Traits, errs),
		Details:     validateDetails(input.Details, errs),
	}
	if err := validationError(errs); err != nil {
		return nil, err
	}

	// Two cast members with the same name would be indistinguishable everywhere
	// the cast appears - reader cards, the timeline, the appearance picker - so
	// a duplicate is refused as a field error rather than silently created.
	taken, err := s.repo.NameTaken(ctx, novel.ID, name, uuid.Nil)
	if err != nil {
		return nil, s.internal("check name", err)
	}
	if taken {
		return nil, apierror.Validation(map[string][]string{
			"name": {"A character with this name already exists in this fiction."},
		})
	}

	if input.FirstChapterID != nil {
		if err := s.requireOwnChapter(ctx, novel.ID, *input.FirstChapterID); err != nil {
			return nil, err
		}
		record.FirstChapterID = input.FirstChapterID
	}

	created, err := s.repo.Create(ctx, record)
	if err != nil {
		return nil, s.internal("create", err)
	}
	// นักสร้างโลก (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3).
	if s.achievements != nil && filledBeyondName(created) {
		s.achievements.CharacterDetailed(ctx, identity.UserID())
	}
	view := created.View()
	return &view, nil
}

// Update edits a character in the caller's own fiction.
func (s *Service) Update(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
	characterID uuid.UUID, input UpdateInput,
) (*View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	errs := map[string][]string{}

	if input.Name != nil {
		name := validateName(*input.Name, errs)
		input.Name = &name
	}
	clean := func(target **string, max int, field string) {
		if target == nil || *target == nil {
			return
		}
		*target = normalise(*target, max, field, errs)
	}
	clean(input.Role, roleMaxLength, "role")
	clean(input.Summary, summaryMaxLength, "summary")
	clean(input.AvatarURL, avatarURLMaxLength, "avatar_url")
	clean(input.Description, descriptionMaxLength, "description")
	clean(input.Quote, quoteMaxLength, "quote")

	// A present avatar value must be a web URL; a clear (null) passes through.
	if input.AvatarURL != nil && *input.AvatarURL != nil {
		*input.AvatarURL = validateAvatarURL(*input.AvatarURL, errs)
	}

	// Chat preferences (chat-editor review 2026-08): a colour is a strict
	// #RRGGBB, a side is left or right, and the display name is short. A null
	// clears any of them back to the composer's defaults.
	if input.ChatColor != nil && *input.ChatColor != nil {
		*input.ChatColor = validateChatColor(*input.ChatColor, errs)
	}
	if input.ChatSide != nil && *input.ChatSide != nil {
		*input.ChatSide = validateChatSide(*input.ChatSide, errs)
	}
	clean(input.ChatDisplayName, chatDisplayNameMaxLength, "chat_display_name")

	if input.Traits != nil {
		traits := validateTraits(*input.Traits, errs)
		input.Traits = &traits
	}
	if input.Details != nil {
		details := validateDetails(*input.Details, errs)
		input.Details = &details
	}
	if err := validationError(errs); err != nil {
		return nil, err
	}

	// A rename into an existing cast member's name is the same collision a
	// duplicate create would be. The character keeps its own name, of course.
	if input.Name != nil {
		taken, err := s.repo.NameTaken(ctx, novel.ID, *input.Name, characterID)
		if err != nil {
			return nil, s.internal("check name", err)
		}
		if taken {
			return nil, apierror.Validation(map[string][]string{
				"name": {"A character with this name already exists in this fiction."},
			})
		}
	}

	if input.FirstChapterID != nil && *input.FirstChapterID != nil {
		if err := s.requireOwnChapter(ctx, novel.ID, **input.FirstChapterID); err != nil {
			return nil, err
		}
	}

	updated, err := s.repo.Update(ctx, novel.ID, characterID, input)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("update", err)
	}
	view := updated.View()
	return &view, nil
}

// Delete removes a character from the caller's own fiction.
//
// This deletes AUTHOR-CREATED metadata, so it is only ever reached through an
// explicit request against a character the caller owns. It does not touch a
// single chapter, message, or word of the fiction itself.
func (s *Service) Delete(
	ctx context.Context, identity *auth.Identity, ref novels.Ref, characterID uuid.UUID,
) error {
	novel, err := s.novels.ForEditor(ctx, identity, ref)
	if err != nil {
		return err
	}

	err = s.repo.Delete(ctx, novel.ID, characterID)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return s.internal("delete", err)
	}
	return nil
}

// Reorder rewrites the cast order.
//
// The list must name every character exactly once. A partial list would leave
// some characters at stale positions and collide on the uniqueness constraint,
// so it is rejected as a validation error rather than half-applied.
func (s *Service) Reorder(
	ctx context.Context, identity *auth.Identity, ref novels.Ref, ids []uuid.UUID,
) ([]View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	total, err := s.repo.CountForNovel(ctx, novel.ID)
	if err != nil {
		return nil, s.internal("count", err)
	}

	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, apierror.Validation(map[string][]string{
				"character_ids": {"Each character may appear only once."},
			})
		}
		seen[id] = true
	}
	if len(ids) != total {
		return nil, apierror.Validation(map[string][]string{
			"character_ids": {"The order must list every character in this fiction exactly once."},
		})
	}

	if err := s.repo.Reorder(ctx, novel.ID, ids); err != nil {
		return nil, s.internal("reorder", err)
	}
	return s.List(ctx, identity, ref)
}

// SetAppearances records which chapters a character appears in.
func (s *Service) SetAppearances(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
	characterID uuid.UUID, chapterIDs []uuid.UUID,
) (*View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	if _, err := s.repo.Find(ctx, novel.ID, characterID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, notFound()
		}
		return nil, s.internal("find", err)
	}

	if err := s.repo.SetAppearances(ctx, novel.ID, characterID, chapterIDs); err != nil {
		return nil, s.internal("set appearances", err)
	}
	return s.Get(ctx, identity, ref, characterID)
}

// requireOwnChapter rejects a first-appearance pointing outside this fiction.
func (s *Service) requireOwnChapter(
	ctx context.Context, novelID, chapterID uuid.UUID,
) error {
	belongs, err := s.repo.ChapterBelongsTo(ctx, novelID, chapterID)
	if err != nil {
		return s.internal("check chapter", err)
	}
	if !belongs {
		return apierror.Validation(map[string][]string{
			"first_chapter_id": {"That chapter is not part of this fiction."},
		})
	}
	return nil
}

// ParseID turns a path parameter into a character id.
func ParseID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse character id: %w", err)
	}
	return id, nil
}
