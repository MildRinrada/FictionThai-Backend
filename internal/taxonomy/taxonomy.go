// Package taxonomy owns the discovery vocabularies: genres and tags
// (docs/08 §14, §15).
//
// They are two SEPARATE vocabularies, deliberately not one polymorphic table:
//
//	genres  controlled classification - a curated list writers select from;
//	        rows arrive seeded and are changed operationally, never by API
//	tags    flexible discovery metadata - created by writers as they tag
//	        their work, through one validated get-or-create path
//
// The Fiction Format System is NOT represented here. docs/08 §15.2 forbids
// duplicating first-class format metadata ("one-shot", "chat-fiction",
// "headcanon") as tags, and CreateTag enforces that ban server-side.
//
// Assignment of genres and tags TO a fiction lives in the novels domain - the
// edge rows are fiction metadata, owned and authorized through the fiction.
// The dependency runs novels -> taxonomy, never back.
package taxonomy

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/slug"
)

/*
 * GenreKind is which question a controlled term answers (13S).
 *
 * One vocabulary, three questions. "โรแมนติก" and "Boy's Love (BL)" are not
 * alternatives - a fiction is routinely both, and a reader browsing for one is
 * not browsing for the other. The flat list forced writers to spend their three
 * genre slots choosing between facts about their own work.
 *
 * A column rather than three tables: they attach through the same edge table,
 * they are curated the same way, and every query that already reads them keeps
 * working. What changes is that a picker can ask three questions.
 */
type GenreKind string

const (
	// KindContent - what the story is like. โรแมนติก, ดราม่าปวดตับ, ตลก.
	KindContent GenreKind = "content"
	// KindRelationship - who it is about. BL, GL, ชาย-หญิง, Reader, OC.
	KindRelationship GenreKind = "relationship"
	// KindAU - which alternate universe. AU ไทย, AU มหาลัย, AU คาเฟ่.
	KindAU GenreKind = "au"
)

// Genre is one row of the controlled vocabulary (docs/08 §14.1).
type Genre struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Kind        GenreKind `json:"kind"`
	Description *string   `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Tag is one row of the flexible vocabulary (docs/08 §15.1).
type Tag struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`

	// NovelCount is how many LISTED fictions carry the tag. Populated on the
	// browse listing so readers see which tags are alive (docs/01 §6).
	NovelCount int64 `json:"novel_count,omitempty"`
}

// Term is the compact form both vocabularies share on a fiction resource -
// enough to render a badge and build a filter link, nothing more.
type Term struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
}

// TagNameMaxLength bounds a tag name (docs/09 §36 requires maximum lengths at
// the API boundary). Counted in runes: a Thai tag must get the same room as an
// English one.
const TagNameMaxLength = 40

// formatTagNames are the names docs/08 §15.2 explicitly forbids as tags: they
// duplicate first-class fiction format metadata, and a tag copy would let the
// two disagree about what a fiction is.
var formatTagNames = map[string]struct{}{
	"one-shot":      {},
	"one_shot":      {},
	"multi-chapter": {},
	"multi_chapter": {},
	"chat-fiction":  {},
	"chat_fiction":  {},
	"chat":          {},
	"standard":      {},
	"headcanon":     {},
	"general":       {},
}

// NormalizeTagName canonicalizes writer input: trimmed, inner whitespace
// collapsed to single spaces, and lowercased for its ASCII letters so "Slow
// Burn" and "slow burn" are one tag rather than two. Thai has no case, so Thai
// tags pass through untouched.
func NormalizeTagName(raw string) string {
	fields := strings.Fields(raw)
	return strings.ToLower(strings.Join(fields, " "))
}

// ValidTagName reports whether a normalized name may become a tag, and if not,
// why not.
func ValidTagName(name string) (bool, string) {
	if name == "" {
		return false, "A tag name is required."
	}
	if len([]rune(name)) > TagNameMaxLength {
		return false, "Tag names must be at most 40 characters."
	}
	// Reuse the slug alphabet plus spaces: letters (Thai included), digits,
	// hyphens. This is what keeps a tag usable as a URL filter value.
	for _, r := range name {
		if r == ' ' || r == '-' || slug.IsSlugRune(r) {
			continue
		}
		return false, "Tag names may only contain letters, numbers, spaces, and hyphens."
	}
	if _, forbidden := formatTagNames[name]; forbidden {
		return false, "Fiction formats are first-class metadata, not tags. Set the format on the fiction instead."
	}
	// A name of nothing but hyphens or spaces flattens to an empty slug, which
	// could never be used as a filter value.
	if TagSlug(name) == "" {
		return false, "A tag name needs at least one letter or number."
	}
	return true, ""
}

// TagSlug derives the URL form of a tag name.
func TagSlug(name string) string {
	return slug.Make(name)
}
