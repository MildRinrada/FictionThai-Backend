// Package pennames owns the identities a writer publishes under
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2).
//
// The rule the whole package exists for: a pen name is CHANGEABLE, a handle is
// not. `users.username` stays permanent and addressable; the name on the cover
// is the writer's to change, to split per วง, or to share for a collaboration.
//
// Layering (docs/09 §44): Handler -> Service -> Repository -> PostgreSQL.
//
// Every operation is SELF-SCOPED by construction: the row set is chosen by
// identity.UserID(), never by anything in the request, so there is no
// cross-user path to authorize and none to get wrong (the authors and profiles
// precedent). A pen name id belonging to someone else simply does not match,
// and answers with the same 404 an absent one gets.
//
// Nothing in this package deletes or rewrites a word of anyone's fiction.
// Removing an identity relies on `novels.pen_name_id ON DELETE SET NULL`: the
// work keeps its text and falls back to the writer's default name.
package pennames

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// RefParam is the path parameter name for a pen name id.
const RefParam = "pen_name"

// Field limits. They mirror the column widths in the migration so a value
// PostgreSQL would truncate is a clean 422 instead.
const (
	nameMaxLength = 64
	noteMaxLength = 40

	// maxPerUser bounds the list. A writer keeps a handful of identities - one
	// per แนว, one for a collaboration - and the profile renders them all at
	// once with no pagination, so an unbounded list would be a page nobody can
	// read rather than a feature.
	maxPerUser = 20
)

// HistoryWindow is how long a rename stays visible as «เคยใช้ชื่อ …».
//
// Thirty days, and deliberately not forever: the point is to make a name being
// taken over visible while it matters, not to follow someone around
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2).
const HistoryWindow = 30 * 24 * time.Hour

// HistoryInterval renders HistoryWindow as a PostgreSQL interval literal.
//
// Exported because the window is asked about from two places - this package's
// own Recent read and the public profile read that publishes the line - and one
// window written twice is one window that eventually disagrees with itself. It
// is bound as a parameter, never interpolated into SQL.
func HistoryInterval() string {
	return fmt.Sprintf("%d seconds", int64(HistoryWindow/time.Second))
}

// PenName mirrors a pen_names row.
type PenName struct {
	ID     uuid.UUID
	UserID uuid.UUID

	Name string
	Note *string

	IsDefault bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// View is the API shape.
//
// There is nothing owner-only on it: a pen name is published on the writer's
// work, so the same fields serve the owner's editor and the public profile
// list. What differs is only WHICH endpoint a caller can reach.
type View struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Note *string `json:"note,omitempty"`

	IsDefault bool `json:"is_default"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (p *PenName) View() View {
	return View{
		ID:        p.ID.String(),
		Name:      p.Name,
		Note:      p.Note,
		IsDefault: p.IsDefault,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

// CreateInput is a new identity. Only the name is required.
type CreateInput struct {
	Name string
	Note *string
	// IsDefault asks for this one to become the fallback. The first pen name an
	// account creates becomes the default regardless, because a list of one
	// with no default would mean "no name on my work" - which nobody asked for.
	IsDefault bool
}

// UpdateInput is a partial update.
//
// Note is **string so the three PATCH cases stay distinguishable: absent
// (leave it), null (clear it), value. Collapsing them would let a rename wipe
// the writer's own label for what the identity is for (docs/09 §3).
type UpdateInput struct {
	Name      *string
	Note      **string
	IsDefault *bool
}

// ParseID turns a path parameter into a pen name id.
func ParseID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse pen name id: %w", err)
	}
	return id, nil
}

// validateName enforces the one required field.
func validateName(name string, errs map[string][]string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		errs["name"] = append(errs["name"], "A pen name needs a name.")
		return ""
	}
	if utf8.RuneCountInString(trimmed) > nameMaxLength {
		errs["name"] = append(errs["name"], "The pen name is too long.")
	}
	return trimmed
}

// normaliseNote trims the label and turns an emptied one into a clear.
func normaliseNote(value *string, errs map[string][]string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	if utf8.RuneCountInString(trimmed) > noteMaxLength {
		errs["note"] = append(errs["note"], "This note is too long.")
		return nil
	}
	return &trimmed
}
