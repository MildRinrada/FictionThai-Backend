// Package characters owns a fiction's cast (Phase 12A,
// docs/PHASE-12-STORY-DEPTH.md §12A).
//
// A character is AUTHORED content that belongs to one fiction. It is never
// derived from the chapter text, and nothing in this package reads or rewrites
// what a writer wrote.
//
// Layering (docs/09 §44): Handler -> Service -> Repository -> PostgreSQL.
// Authorization is decided in the Service, which asks the novels service whether
// the caller may read or write the parent fiction - the cast inherits the
// fiction's gate exactly, so a character of a private fiction is as invisible as
// the fiction (docs/11 §31).
package characters

import (
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Field limits. They mirror the column widths in the migration so a value that
// would be truncated by PostgreSQL is a clean 422 instead.
const (
	nameMaxLength        = 120
	roleMaxLength        = 120
	summaryMaxLength     = 300
	quoteMaxLength       = 500
	descriptionMaxLength = 20_000
	avatarURLMaxLength   = 2048

	maxTraits = 12
	// Generous on purpose: writers record a personality as a full sentence
	// ("มีนิสัยสุภาพ สุขุม รอบรู้ และรักสันโดษ ในฐานะ…"), and refusing that
	// shape only teaches them the field is broken. The AI trait scan reads
	// known trait words out of long phrases just as well as out of chips.
	traitMaxLength    = 300
	maxDetails        = 20
	detailLabelMaxLen = 200
	detailValueMaxLen = 2000

	chatDisplayNameMaxLength = 60
)

// Detail is one author-defined fact about a character.
//
// The label is the author's own words. There is deliberately no fixed field
// schema: the prototype's ชื่อเต็ม / อายุ / อาชีพ are examples, and a fantasy
// fiction wants different labels than a school romance
// (docs/PHASE-12-STORY-DEPTH.md §12A).
type Detail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Character mirrors a characters row.
type Character struct {
	ID      uuid.UUID
	NovelID uuid.UUID

	Name        string
	Role        *string
	Summary     *string
	AvatarURL   *string
	Description *string
	Quote       *string

	Traits  []string
	Details []Detail

	// Chat presentation preferences (chat-editor review 2026-08): the colour
	// that identifies this character in the composer, the side their bubbles
	// sit on, and the short name the speaker strip shows. On the character
	// because the strip must agree across every chapter.
	ChatColor       *string
	ChatSide        *string
	ChatDisplayName *string

	Position       int
	FirstChapterID *uuid.UUID

	CreatedAt time.Time
	UpdatedAt time.Time
}

// View is the API shape. It is identical for a reader and for the owner: a
// character carries no owner-only state, and the decision about whether the
// caller may see it at all was already made by the fiction's gate.
type View struct {
	ID      string `json:"id"`
	NovelID string `json:"novel_id"`

	Name        string  `json:"name"`
	Role        *string `json:"role,omitempty"`
	Summary     *string `json:"summary,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
	Description *string `json:"description,omitempty"`
	Quote       *string `json:"quote,omitempty"`

	// Always arrays, never null, so a client never has to guard before mapping.
	Traits  []string `json:"traits"`
	Details []Detail `json:"details"`

	ChatColor       *string `json:"chat_color,omitempty"`
	ChatSide        *string `json:"chat_side,omitempty"`
	ChatDisplayName *string `json:"chat_display_name,omitempty"`

	Position       int     `json:"position"`
	FirstChapterID *string `json:"first_chapter_id,omitempty"`

	// Chapter ids this character appears in, in chapter order. Carried by both
	// the single-character read and the list (the list loads every cast
	// member's appearances in one grouped query, not one per card). omitempty
	// means "no appearances" arrives as an absent key; clients default it.
	AppearsIn []string `json:"appears_in,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c *Character) View() View {
	view := View{
		ID:          c.ID.String(),
		NovelID:     c.NovelID.String(),
		Name:        c.Name,
		Role:        c.Role,
		Summary:     c.Summary,
		AvatarURL:   c.AvatarURL,
		Description: c.Description,
		Quote:       c.Quote,
		Traits:      c.Traits,
		Details:     c.Details,

		ChatColor:       c.ChatColor,
		ChatSide:        c.ChatSide,
		ChatDisplayName: c.ChatDisplayName,

		Position:  c.Position,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
	if view.Traits == nil {
		view.Traits = []string{}
	}
	if view.Details == nil {
		view.Details = []Detail{}
	}
	if c.FirstChapterID != nil {
		id := c.FirstChapterID.String()
		view.FirstChapterID = &id
	}
	return view
}

// CreateInput is a new character. Only the name is required - a writer sketching
// a cast should not have to fill in a form before the character exists.
type CreateInput struct {
	Name        string
	Role        *string
	Summary     *string
	AvatarURL   *string
	Description *string
	Quote       *string
	Traits      []string
	Details     []Detail
	// FirstChapterID is validated against the SAME fiction by the service, so a
	// character can never point at a chapter of someone else's work.
	FirstChapterID *uuid.UUID
}

// UpdateInput is a partial update.
//
// Every field is a pointer, and the nullable ones are **string, so the three
// PATCH cases stay distinguishable: absent (leave alone), null (clear), value.
// Collapsing them would let a PATCH that renames a character silently wipe their
// backstory (docs/09 §3).
type UpdateInput struct {
	Name        *string
	Role        **string
	Summary     **string
	AvatarURL   **string
	Description **string
	Quote       **string

	// A present slice replaces the whole set; nil leaves it untouched.
	Traits  *[]string
	Details *[]Detail

	ChatColor       **string
	ChatSide        **string
	ChatDisplayName **string

	FirstChapterID **uuid.UUID
}

// normalise trims a nullable text field and turns an emptied one into a clear.
func normalise(value *string, max int, field string, errs map[string][]string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	if utf8.RuneCountInString(trimmed) > max {
		errs[field] = append(errs[field], "This value is too long.")
		return nil
	}
	return &trimmed
}

// validateAvatarURL rejects a non-web avatar reference. The value is rendered
// as an <img src> on reader pages, so only an http(s) URL with a host passes -
// a javascript: or data: value is refused before it is ever stored.
func validateAvatarURL(value *string, errs map[string][]string) *string {
	if value == nil {
		return nil
	}
	parsed, err := url.Parse(*value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		errs["avatar_url"] = append(errs["avatar_url"], "Must be an http(s) URL.")
		return nil
	}
	return value
}

// validateName enforces the one required field.
func validateName(name string, errs map[string][]string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		errs["name"] = append(errs["name"], "A character needs a name.")
		return ""
	}
	if utf8.RuneCountInString(trimmed) > nameMaxLength {
		errs["name"] = append(errs["name"], "The name is too long.")
	}
	return trimmed
}

// validateTraits cleans the chip list: trimmed, de-duplicated, bounded.
func validateTraits(traits []string, errs map[string][]string) []string {
	cleaned := make([]string, 0, len(traits))
	seen := map[string]bool{}

	for _, trait := range traits {
		trimmed := strings.TrimSpace(trait)
		if trimmed == "" {
			continue
		}
		if utf8.RuneCountInString(trimmed) > traitMaxLength {
			errs["traits"] = append(errs["traits"], "A trait is too long.")
			return nil
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		cleaned = append(cleaned, trimmed)
	}

	if len(cleaned) > maxTraits {
		errs["traits"] = append(errs["traits"], "Too many traits.")
		return nil
	}
	return cleaned
}

// validateDetails cleans the author-defined fact list.
//
// A pair with no label is dropped rather than rejected: a half-filled row in an
// editor is a work in progress, not an error to shout about. A labelled pair
// with an empty value is KEPT, because "อาชีพ: -" is a deliberate statement.
func validateDetails(details []Detail, errs map[string][]string) []Detail {
	cleaned := make([]Detail, 0, len(details))

	for _, detail := range details {
		label := strings.TrimSpace(detail.Label)
		value := strings.TrimSpace(detail.Value)
		if label == "" {
			continue
		}
		if utf8.RuneCountInString(label) > detailLabelMaxLen ||
			utf8.RuneCountInString(value) > detailValueMaxLen {
			errs["details"] = append(errs["details"], "A detail is too long.")
			return nil
		}
		cleaned = append(cleaned, Detail{Label: label, Value: value})
	}

	if len(cleaned) > maxDetails {
		errs["details"] = append(errs["details"], "Too many details.")
		return nil
	}
	return cleaned
}

// chatColorPattern accepts only a six-digit hex colour. The value lands in a
// style attribute on the composer, so anything freer would be a CSS injection
// waiting for a renderer to trust it.
var chatColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// validateChatColor accepts #RRGGBB or nothing.
func validateChatColor(value *string, errs map[string][]string) *string {
	if value == nil {
		return nil
	}
	if !chatColorPattern.MatchString(*value) {
		errs["chat_color"] = append(errs["chat_color"], "Must be a #RRGGBB colour.")
		return nil
	}
	lowered := strings.ToLower(*value)
	return &lowered
}

// validateChatSide accepts left, right, or nothing.
func validateChatSide(value *string, errs map[string][]string) *string {
	if value == nil {
		return nil
	}
	if *value != "left" && *value != "right" {
		errs["chat_side"] = append(errs["chat_side"], "Must be left or right.")
		return nil
	}
	return value
}

func validationError(errs map[string][]string) error {
	if len(errs) == 0 {
		return nil
	}
	return apierror.Validation(errs)
}
