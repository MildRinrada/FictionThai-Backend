// Package wall owns the profile comment wall: the messages people leave on
// somebody's page.
//
// It is its own package rather than a corner of `comments` because the two
// answer to different things. A fiction comment inherits the FICTION's gate -
// who may read it, whether the author holds new comments, whether guests are
// allowed - and every rule in that package is expressed in terms of a novel. A
// wall has no fiction behind it. Its gate is one switch owned by the person
// whose page it is (user_profiles.wall_enabled), its audience is everyone who
// can see that profile, and it accepts no guests at all: with no work to
// moderate against and no author to review the queue, "an account someone can
// hold responsible" is the only rule that survives contact with a bad day.
//
// Two people may remove an entry, and that is deliberate:
//
//	the AUTHOR         taking back your own words is never someone else's
//	                   decision (docs/09 §20's owner rule)
//	the PROFILE OWNER  it is their page. A person who cannot clear their own
//	                   wall does not really have the switch either - they would
//	                   be choosing between living with it and closing the wall
//
// Layering matches the rest of the backend (docs/09 §44): Handler -> Service ->
// Repository -> PostgreSQL, with authorization decided in the Service
// (docs/10 §27).
package wall

import (
	"time"

	"github.com/google/uuid"
)

// RefParam is the path parameter name for a wall entry id. Entries have no
// slugs - the id is the only reference, like a comment's.
const RefParam = "entry"

// UserRefParam names the owner segment of `/users/:user/wall`. It matches
// profiles.RefParam because Gin allows one wildcard name per path position and
// both share `/users/:user`.
const UserRefParam = "user"

// MaxBodyRunes bounds one message. Runes, not bytes: Thai text is three bytes
// per character in UTF-8 and must not get a third of the room a Latin message
// gets (the same rule every other text limit here follows).
//
// It is far shorter than a fiction comment's 5000 on purpose. A wall is for a
// word in passing; anything longer belongs under the work it is about, where
// the author of that work can moderate it.
const MaxBodyRunes = 1000

// Status is the PLATFORM's axis on an entry, the same vocabulary
// comments.Status uses. It is distinct from deletion: DeletedAt is the author
// taking their words back, Status is the platform acting.
type Status string

const (
	StatusVisible Status = "visible"
	StatusHidden  Status = "hidden"
	StatusRemoved Status = "removed"
)

// Author is the public card shown beside an entry - the same public-profile
// slice every other card uses, never an email address (docs/08 §1.4).
type Author struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`
}

// Entry is the storage row plus its joined author card.
type Entry struct {
	ID            uuid.UUID
	ProfileUserID uuid.UUID
	AuthorID      uuid.UUID

	// Body is PLAIN TEXT. Stored raw and escaped at render time, never
	// interpreted as markup (docs/11 §16).
	Body string

	Status    Status
	CreatedAt time.Time
	DeletedAt *time.Time

	Author Author
}

// RemovableBy reports whether this account may take the entry down: the person
// who wrote it, or the person whose wall it is.
func (e *Entry) RemovableBy(userID uuid.UUID) bool {
	if userID == uuid.Nil {
		return false
	}
	return e.AuthorID == userID || e.ProfileUserID == userID
}

// View is the API shape of one entry (docs/09 §7 envelope `data`).
type View struct {
	ID        uuid.UUID `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	Author    Author    `json:"author"`

	// IsOwner marks the caller's own entries; CanDelete is the wider rule, so
	// the profile owner's "clear this" control does not have to be re-derived
	// client-side. Both are viewer-dependent, which is why a wall listing is
	// never cached like the profile it sits on.
	IsOwner   bool `json:"is_owner"`
	CanDelete bool `json:"can_delete"`
}

// Render builds the API view for one viewer.
func (e *Entry) Render(viewerID uuid.UUID) View {
	return View{
		ID:        e.ID,
		Body:      e.Body,
		CreatedAt: e.CreatedAt,
		Author:    e.Author,
		IsOwner:   viewerID != uuid.Nil && e.AuthorID == viewerID,
		CanDelete: e.RemovableBy(viewerID),
	}
}

// Target is the person a wall belongs to, and whether they have it open.
type Target struct {
	UserID  uuid.UUID
	Enabled bool
}
