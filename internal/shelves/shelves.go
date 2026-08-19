// Package shelves owns the OPT-IN public collection: a reader's own shelves of
// other people's fiction, and the one place on the platform where what somebody
// has been reading is published on purpose.
//
// It is deliberately NOT part of `library`, and the separation is the feature.
// README ("Bookmarks & Personal Library") says bookmarks are private by default
// and that users MAY OPTIONALLY create public collections; the library package
// states the private half as a contract - "Nothing here is ever publicly listed
// - every read is scoped to the authenticated caller". A public shelf is
// therefore a different object with a different table: it is assembled one
// fiction at a time by a person who meant to publish each one, never a switch
// that republishes what they had already saved in private. Nothing in this
// package reads `bookmarks`, and nothing in it can.
//
// Two visibility rules meet here and both are enforced in SQL, never in Go:
//
//	the SHELF     is_public is per shelf and defaults to false. A private shelf
//	              does not appear in anyone else's listing, and its id is not an
//	              oracle either - a stranger asking for it gets the 404 a
//	              nonexistent shelf gets.
//	the FICTION   every item is filtered through novels.ReadableSQL - the SHARED
//	              predicate, not a copy - so a fiction that has gone private, or
//	              was never public, cannot ride onto a public page on somebody's
//	              shelf (docs/11 §31). Explicit-rated work is filtered too: a
//	              public shelf is a browse surface, and 18+ เนื้อหาทางเพศชัดเจน
//	              work is never listed on one (§13B, novels.RatingExplicit).
//
// Layering matches the rest of the backend (docs/09 §44): Handler -> Service ->
// Repository -> PostgreSQL, with authorization decided in the Service
// (docs/10 §27). The dependency is one-directional: shelves -> novels/profiles,
// never back.
package shelves

import (
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
)

// RefParam is the path parameter name for a shelf id. Shelves have no slugs -
// the id is the only reference, like a comment's.
const RefParam = "shelf"

// UserRefParam names the owner segment of `/users/:user/shelves`. It matches
// profiles.RefParam and library.UserRefParam because Gin allows one wildcard
// name per path position and all three share `/users/:user`.
const UserRefParam = "user"

// ItemPreviewLimit is how many fictions one shelf carries in a listing.
//
// A listing shows shelves, not a shelf: the point of the page is "here are the
// collections", and a reader who wants all of one opens it. Capping here is
// also what keeps the whole response three queries however many shelves a
// person has.
const ItemPreviewLimit = 24

// Bounds. name and note are VARCHAR in the schema, so these mirror storage; the
// two counts are product limits - a shelf list is something a person arranges
// by hand, and neither number is a technical ceiling.
const (
	NameMaxRunes = 60
	NoteMaxRunes = 160
	MaxShelves   = 20
	MaxItems     = 500
)

// Shelf is one stored collection plus the count of items VISIBLE in the
// listing it was loaded for.
//
// ItemCount is filtered, not raw, and that is deliberate: a public shelf whose
// count said 12 while showing 9 would be telling a stranger that three fictions
// they may not see exist. The owner's own listing counts what the owner can
// see, which is the same rule applied to a different viewer.
type Shelf struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	Note      *string
	IsPublic  bool
	Position  int
	ItemCount int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OwnedBy reports whether this shelf belongs to the given account.
func (s *Shelf) OwnedBy(userID uuid.UUID) bool {
	return userID != uuid.Nil && s.UserID == userID
}

// Item is one fiction on a shelf: the card, plus the reader's own line about
// why it is there.
type Item struct {
	Novel   novels.View `json:"novel"`
	Note    *string     `json:"note,omitempty"`
	AddedAt time.Time   `json:"added_at"`
}

// View is the API shape of one shelf (docs/09 §7 envelope `data`).
type View struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Note *string   `json:"note,omitempty"`

	// IsPublic is present on every response, including the public one, so a
	// reader browsing their own profile can tell at a glance which shelves
	// strangers are seeing.
	IsPublic bool `json:"is_public"`
	Position int  `json:"position"`

	// ItemCount is how many items this viewer may see - always consistent with
	// Items, which is capped at ItemPreviewLimit.
	ItemCount int64  `json:"item_count"`
	Items     []Item `json:"items"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Render builds the API view for one shelf and its already-filtered items.
func (s *Shelf) Render(items []Item) View {
	if items == nil {
		items = []Item{}
	}
	return View{
		ID:        s.ID,
		Name:      s.Name,
		Note:      s.Note,
		IsPublic:  s.IsPublic,
		Position:  s.Position,
		ItemCount: s.ItemCount,
		Items:     items,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}
