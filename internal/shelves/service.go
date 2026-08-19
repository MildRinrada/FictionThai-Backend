package shelves

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/profiles"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// NovelAccess is the slice of the novels domain this service needs. Putting a
// fiction on a shelf starts by asking whether the caller may READ it, so a
// shelf cannot be used to probe private slugs and a 404 reveals nothing
// (docs/11 §21).
type NovelAccess interface {
	ForReader(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
}

// NovelStore is the slice of the novels repository this service needs.
//
// Find is the visibility-free resolver, used ONLY for removal: taking a fiction
// off a shelf must keep working after it goes private. RecordsByIDs batch-loads
// the cards for one page of shelf rows, which keeps `novels` the single source
// of truth for what a card contains.
type NovelStore interface {
	Find(ctx context.Context, ref novels.Ref) (*novels.Novel, error)
	RecordsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]novels.Record, error)
}

// Service owns the shelf rules and is the authorization boundary for every
// shelf endpoint (docs/10 §27).
type Service struct {
	repo    *Repository
	novels  NovelAccess
	records NovelStore
	log     *slog.Logger
}

func NewService(
	repo *Repository, novelAccess NovelAccess, novelStore NovelStore, log *slog.Logger,
) *Service {
	return &Service{repo: repo, novels: novelAccess, records: novelStore, log: log}
}

// notFound is the single answer to every "you may not see this" on a shelf: a
// missing id, a malformed one, someone else's private shelf, and a shelf on a
// banned account all look identical (docs/11 §3.4).
func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "SHELF_NOT_FOUND", "Shelf not found.")
}

func userNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "USER_NOT_FOUND", "User not found.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// Input is a shelf create request.
type Input struct {
	Name     string
	Note     string
	IsPublic bool
}

// Edit is a partial shelf update: a nil field is UNTOUCHED, an empty note
// CLEARS. Pointers for the same reason profiles.Edit uses them - a PATCH that
// sent zero values for everything the client did not know about would quietly
// erase fields added after that client shipped.
type Edit struct {
	Name     *string
	Note     *string
	IsPublic *bool
	Position *int
}

// ---------------------------------------------------------------------------
// The public read
// ---------------------------------------------------------------------------

// ListPublic returns one person's PUBLIC shelves - the "ที่ฉันอ่าน" section of
// their profile.
//
// It takes no identity at all, exactly like profiles.Get: the answer is the
// same for a guest, a stranger, and the person themselves, so one cached
// response serves every visitor (docs/14 §7). The owner's own management view
// is a different endpoint, and it is the only one that can see a private shelf.
func (s *Service) ListPublic(ctx context.Context, raw string) ([]View, error) {
	ref, err := profiles.ParseRef(raw)
	if err != nil {
		return nil, userNotFound()
	}
	ownerID, err := s.repo.ResolveOwner(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, userNotFound()
	}
	if err != nil {
		return nil, s.internal("resolve shelf owner", err)
	}
	return s.list(ctx, ownerID, true)
}

// Mine returns the caller's own shelves, public and private, in their order.
func (s *Service) Mine(ctx context.Context, identity *auth.Identity) ([]View, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	return s.list(ctx, userID, false)
}

// list is the shared read: shelves, then one query for every shelf's items,
// then one batch load of the fiction cards. Three queries however many shelves
// there are (docs/07 §67).
func (s *Service) list(ctx context.Context, ownerID uuid.UUID, publicOnly bool) ([]View, error) {
	shelves, err := s.repo.List(ctx, ownerID, publicOnly)
	if err != nil {
		return nil, s.internal("list shelves", err)
	}
	if len(shelves) == 0 {
		return []View{}, nil
	}

	ids := make([]uuid.UUID, 0, len(shelves))
	for i := range shelves {
		ids = append(ids, shelves[i].ID)
	}

	rowsByShelf, err := s.repo.Items(ctx, ids, ownerID, publicOnly)
	if err != nil {
		return nil, s.internal("list shelf items", err)
	}

	novelIDs := []uuid.UUID{}
	for _, rows := range rowsByShelf {
		for _, row := range rows {
			novelIDs = append(novelIDs, row.NovelID)
		}
	}
	records, err := s.records.RecordsByIDs(ctx, novelIDs)
	if err != nil {
		return nil, s.internal("load shelf cards", err)
	}

	views := make([]View, 0, len(shelves))
	for i := range shelves {
		shelf := shelves[i]
		rows := rowsByShelf[shelf.ID]
		items := make([]Item, 0, len(rows))
		for _, row := range rows {
			record, ok := records[row.NovelID]
			if !ok {
				continue // the fiction vanished between the two queries
			}
			// ViewFor(false): a shelf renders the PUBLIC card even for the
			// owner. Owner-only fields belong in the studio, not on a shelf,
			// and a card that grew extra fields on one page would eventually
			// carry them onto the public one.
			items = append(items, Item{
				Novel:   record.ViewFor(false),
				Note:    row.Note,
				AddedAt: row.AddedAt.Time,
			})
		}
		views = append(views, shelf.Render(items))
	}
	return views, nil
}

// ---------------------------------------------------------------------------
// Owner CRUD
// ---------------------------------------------------------------------------

// Create makes a new shelf. It is PRIVATE unless the request says otherwise -
// the default is the product decision, not a convenience.
func (s *Service) Create(
	ctx context.Context, identity *auth.Identity, input Input,
) (*View, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	fields := map[string][]string{}
	name := validateName(fields, input.Name)
	note := validateNote(fields, input.Note)
	if len(fields) > 0 {
		return nil, apierror.Validation(fields)
	}

	total, err := s.repo.CountForOwner(ctx, userID)
	if err != nil {
		return nil, s.internal("count shelves", err)
	}
	if total >= MaxShelves {
		return nil, apierror.Validation(map[string][]string{
			"name": {"สร้างชั้นหนังสือได้ไม่เกิน 20 ชั้น"},
		})
	}

	shelf, err := s.repo.Create(ctx, userID, name, optional(note), input.IsPublic)
	if err != nil {
		return nil, s.internal("create shelf", err)
	}
	view := shelf.Render(nil)
	return &view, nil
}

// Update edits one of the caller's own shelves, including the public/private
// switch. Flipping is_public changes NOTHING about the items - it changes who
// may see the shelf they are already on.
func (s *Service) Update(
	ctx context.Context, identity *auth.Identity, shelfID uuid.UUID, edit Edit,
) (*View, error) {
	shelf, err := s.owned(ctx, identity, shelfID)
	if err != nil {
		return nil, err
	}

	clean := Edit{IsPublic: edit.IsPublic, Position: edit.Position}
	fields := map[string][]string{}
	if edit.Name != nil {
		name := validateName(fields, *edit.Name)
		clean.Name = &name
	}
	if edit.Note != nil {
		// An empty string reaches the repository as "" and CLEARS the row; a
		// nil pointer never gets here at all.
		note := validateNote(fields, *edit.Note)
		clean.Note = &note
	}
	if clean.Position != nil && (*clean.Position < 0 || *clean.Position > MaxShelves) {
		fields["position"] = []string{"ลำดับไม่ถูกต้อง"}
	}
	if len(fields) > 0 {
		return nil, apierror.Validation(fields)
	}

	updated, err := s.repo.Update(ctx, shelf.ID, &clean)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("update shelf", err)
	}
	view := updated.Render(nil)
	return &view, nil
}

// Delete removes one of the caller's own shelves.
//
// The fictions on it are untouched, and so is any bookmark of them: a shelf is
// a collection ABOUT other people's work, never a copy of it. Deleting an
// already-deleted shelf is a success, matching every other DELETE on the API
// (docs/09 §33).
func (s *Service) Delete(
	ctx context.Context, identity *auth.Identity, shelfID uuid.UUID,
) error {
	shelf, err := s.owned(ctx, identity, shelfID)
	if err != nil {
		return err
	}
	if err := s.repo.Delete(ctx, shelf.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // raced with another delete of the same shelf
		}
		return s.internal("delete shelf", err)
	}
	return nil
}

// AddItem places a fiction on one of the caller's shelves.
//
// The caller must be able to READ the fiction, through the novels service's own
// gate - so a shelf cannot be used to discover that a private slug exists, and
// the fiction's author decides what is reachable, not the person shelving it.
func (s *Service) AddItem(
	ctx context.Context, identity *auth.Identity,
	shelfID uuid.UUID, novelRef novels.Ref, rawNote string,
) (*View, error) {
	shelf, err := s.owned(ctx, identity, shelfID)
	if err != nil {
		return nil, err
	}
	fields := map[string][]string{}
	note := validateNote(fields, rawNote)
	if len(fields) > 0 {
		return nil, apierror.Validation(fields)
	}
	novel, err := s.novels.ForReader(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}

	total, err := s.repo.CountItems(ctx, shelf.ID)
	if err != nil {
		return nil, s.internal("count shelf items", err)
	}
	if total >= MaxItems {
		return nil, apierror.Validation(map[string][]string{
			"novel": {"ชั้นนี้เต็มแล้ว (ไม่เกิน 500 เรื่อง)"},
		})
	}

	if err := s.repo.AddItem(ctx, shelf.ID, novel.ID, optional(note)); err != nil {
		return nil, s.internal("add shelf item", err)
	}
	return s.reload(ctx, shelf.ID, identity)
}

// RemoveItem takes a fiction off one of the caller's shelves.
//
// Deliberately NOT gated on readability: removing must always work, including
// for a fiction that has since gone private or been deleted.
func (s *Service) RemoveItem(
	ctx context.Context, identity *auth.Identity, shelfID uuid.UUID, novelRef novels.Ref,
) error {
	shelf, err := s.owned(ctx, identity, shelfID)
	if err != nil {
		return err
	}
	novel, err := s.records.Find(ctx, novelRef)
	if errors.Is(err, novels.ErrNotFound) {
		return nil // removing what is not there is a successful no-op
	}
	if err != nil {
		return s.internal("resolve novel", err)
	}
	if err := s.repo.RemoveItem(ctx, shelf.ID, novel.ID); err != nil {
		return s.internal("remove shelf item", err)
	}
	return nil
}

// owned loads a shelf the caller may manage, or the shared 404.
//
// A shelf that belongs to somebody else is NOT a 403: shelves are private by
// default, and confirming that an id names a real shelf would make this
// endpoint an oracle for other people's private collections (docs/11 §3.4).
func (s *Service) owned(
	ctx context.Context, identity *auth.Identity, shelfID uuid.UUID,
) (*Shelf, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	shelf, err := s.repo.Find(ctx, shelfID)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("load shelf", err)
	}
	if !shelf.OwnedBy(userID) {
		return nil, notFound()
	}
	return shelf, nil
}

// reload re-reads one shelf with its items, so a mutation answers with what the
// owner will now see.
func (s *Service) reload(
	ctx context.Context, shelfID uuid.UUID, identity *auth.Identity,
) (*View, error) {
	views, err := s.Mine(ctx, identity)
	if err != nil {
		return nil, err
	}
	for i := range views {
		if views[i].ID == shelfID {
			return &views[i], nil
		}
	}
	return nil, notFound()
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// validateName trims and bounds a shelf name, recording any problem against
// the shared field map so one save reports everything wrong with it at once.
func validateName(fields map[string][]string, raw string) string {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		fields["name"] = []string{"ตั้งชื่อชั้นหนังสือด้วย"}
	case utf8.RuneCountInString(name) > NameMaxRunes:
		fields["name"] = []string{"ชื่อชั้นยาวเกินไป (ไม่เกิน 60 ตัวอักษร)"}
	case !novels.SafeText(name) || strings.ContainsAny(name, "\n\r"):
		fields["name"] = []string{"ชื่อชั้นมีอักขระที่ใช้ไม่ได้"}
	}
	return name
}

// validateNote trims and bounds the optional line under a shelf or an item.
// Empty is allowed and means "no note" - it is how a person removes one.
func validateNote(fields map[string][]string, raw string) string {
	note := strings.TrimSpace(raw)
	if note == "" {
		return ""
	}
	switch {
	case utf8.RuneCountInString(note) > NoteMaxRunes:
		fields["note"] = []string{"คำอธิบายยาวเกินไป (ไม่เกิน 160 ตัวอักษร)"}
	case !novels.SafeText(note) || strings.ContainsAny(note, "\n\r"):
		fields["note"] = []string{"คำอธิบายมีอักขระที่ใช้ไม่ได้"}
	}
	return note
}

// optional turns a validated note into the nullable column value: an empty
// string is the ABSENCE of a note, never an empty one.
func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// internal logs the real failure and returns the opaque error (docs/11 §67).
func (s *Service) internal(op string, err error) error {
	s.log.Error("shelves service failure", slog.String("op", op), slog.Any("error", err))
	return apierror.Internal()
}

// Compile-time assurance that the real collaborators satisfy the narrow
// interfaces declared above.
var (
	_ NovelAccess = (*novels.Service)(nil)
	_ NovelStore  = (*novels.Repository)(nil)
)
