// Package fiction owns the Fiction Format System.
//
// FictionThai models a fiction's format as three INDEPENDENT dimensions
// (docs/08 - Database Design.md §2, docs/09 - API Specification.md §14):
//
//	story_structure      one_shot | multi_chapter          - how the work is organised
//	presentation_format  standard | chat | headcanon       - how it is rendered
//	content_mode         general  | headcanon              - how the author classifies it
//
// They are deliberately NOT collapsed into a single `type` enum such as
// "headcanon_chat_one_shot" (docs/08 §43 Rule 6). Keeping them orthogonal is
// what lets every valid combination exist today and lets a future value (e.g.
// a SCRIPT presentation) be added without a schema redesign.
//
// This package holds only format values and their rules. It has no database,
// HTTP, or novel-entity dependencies, so the novels service, the chapters
// service, validation, and tests can all share exactly one definition of what a
// valid format is.
package fiction

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// StoryStructure describes how a fiction is organised into reading units.
type StoryStructure string

const (
	OneShot      StoryStructure = "one_shot"
	MultiChapter StoryStructure = "multi_chapter"
)

// PresentationFormat describes how published content is rendered to readers.
//
// Each value names one of the three representations a chapter may hold
// (docs/CONTENT-MODEL.md §2). It selects which one is ACTIVE; it never selects
// which one exists, and changing it converts nothing.
type PresentationFormat string

const (
	Standard PresentationFormat = "standard"
	Chat     PresentationFormat = "chat"
	// HeadcanonFormat renders chapter_entries - a topic of character entries
	// (docs/PHASE-13-CREATION-AND-CONTROL.md §13J, docs/PHASE-12 §12F).
	//
	// Named apart from the ContentMode below because Go cannot hold two
	// package-level `Headcanon` identifiers, and the two genuinely answer
	// different questions: this one is "render entries", the mode is "this work
	// IS a headcanon work" - which a mixed fiction may be without every chapter
	// being one.
	HeadcanonFormat PresentationFormat = "headcanon"
)

// ContentMode describes the author-selected content classification.
type ContentMode string

const (
	General   ContentMode = "general"
	Headcanon ContentMode = "headcanon"
)

// Defaults applied when a writer does not choose explicitly
// (docs/09 - API Specification.md §15 "Create Novel").
const (
	DefaultStoryStructure     = MultiChapter
	DefaultPresentationFormat = Standard
	DefaultContentMode        = General
)

var (
	storyStructures = map[StoryStructure]struct{}{
		OneShot:      {},
		MultiChapter: {},
	}
	presentationFormats = map[PresentationFormat]struct{}{
		Standard:        {},
		Chat:            {},
		HeadcanonFormat: {},
	}
	contentModes = map[ContentMode]struct{}{
		General:   {},
		Headcanon: {},
	}
)

func (s StoryStructure) Valid() bool     { _, ok := storyStructures[s]; return ok }
func (p PresentationFormat) Valid() bool { _, ok := presentationFormats[p]; return ok }
func (m ContentMode) Valid() bool        { _, ok := contentModes[m]; return ok }

func (s StoryStructure) String() string     { return string(s) }
func (p PresentationFormat) String() string { return string(p) }
func (m ContentMode) String() string        { return string(m) }

// UsesChapterNavigation reports whether readers should be offered chapter
// navigation. One-shots are a single reading unit (docs/15 §5.2), so the
// clients must not render a chapter list for them.
func (f Format) UsesChapterNavigation() bool { return f.StoryStructure == MultiChapter }

// UsesStructuredMessages reports whether the reader representation comes from
// chapter_messages rather than chapters.content (docs/08 §11).
//
// It answers for the FICTION. A chapter of a mixed fiction may resolve to a
// different format entirely - ask chapters.Active for that, never this.
func (f Format) UsesStructuredMessages() bool { return f.PresentationFormat == Chat }

// StoryStructures returns every supported value, for API metadata and OpenAPI.
func StoryStructures() []StoryStructure {
	return []StoryStructure{OneShot, MultiChapter}
}

// PresentationFormats returns every supported value.
func PresentationFormats() []PresentationFormat {
	return []PresentationFormat{Standard, Chat, HeadcanonFormat}
}

// ContentModes returns every supported value.
func ContentModes() []ContentMode {
	return []ContentMode{General, Headcanon}
}

// Format is a complete, three-dimensional fiction format state.
//
// The JSON field names match the API contract and the `novels` columns, so one
// definition serves the wire format, the domain, and the persistence layer.
type Format struct {
	StoryStructure     StoryStructure     `json:"story_structure"`
	PresentationFormat PresentationFormat `json:"presentation_format"`
	ContentMode        ContentMode        `json:"content_mode"`
}

// DefaultFormat is the format a fiction receives when the writer expresses no
// preference.
func DefaultFormat() Format {
	return Format{
		StoryStructure:     DefaultStoryStructure,
		PresentationFormat: DefaultPresentationFormat,
		ContentMode:        DefaultContentMode,
	}
}

// Validate checks the COMPLETE format state, not each field in isolation
// (docs/09 §15 "Update Fiction Format"). All 2x2x2 combinations are currently
// supported; the doc is explicit that combinations must not be restricted
// merely for implementation convenience (docs/08 §2.4). If a genuine product
// rule ever forbids a combination, this is the single place to express it.
func (f Format) Validate() error {
	fields := map[string][]string{}

	if !f.StoryStructure.Valid() {
		fields["story_structure"] = []string{
			fmt.Sprintf("Must be one of: %s.", join(StoryStructures())),
		}
	}
	if !f.PresentationFormat.Valid() {
		fields["presentation_format"] = []string{
			fmt.Sprintf("Must be one of: %s.", join(PresentationFormats())),
		}
	}
	if !f.ContentMode.Valid() {
		fields["content_mode"] = []string{
			fmt.Sprintf("Must be one of: %s.", join(ContentModes())),
		}
	}

	if len(fields) > 0 {
		err := apierror.Validation(fields)
		err.Code = apierror.CodeInvalidFictionFormat
		err.Message = "The requested fiction format is not supported."
		err.Status = http.StatusUnprocessableEntity
		return err
	}
	return nil
}

// Patch is a partial format change: every field is optional, matching
// PATCH /api/v1/novels/:id/format (docs/09 §14.7).
type Patch struct {
	StoryStructure     *StoryStructure     `json:"story_structure,omitempty"`
	PresentationFormat *PresentationFormat `json:"presentation_format,omitempty"`
	ContentMode        *ContentMode        `json:"content_mode,omitempty"`
}

// IsEmpty reports whether the patch would change nothing.
func (p Patch) IsEmpty() bool {
	return p.StoryStructure == nil && p.PresentationFormat == nil && p.ContentMode == nil
}

// Apply returns the format that results from applying p to current, and
// validates the RESULTING state as a whole.
//
// This is metadata-only by contract. Applying a patch must never delete
// chapters, rewrite prose into chat messages, or otherwise transform author
// content (docs/08 §3.1, docs/09 §14.6, docs/15 §5.7). Content conversion, if
// it is ever offered, is a separate explicit author action.
//
// Apply does not mutate current, so a rejected change cannot leave a caller
// holding a half-updated format.
func (p Patch) Apply(current Format) (Format, error) {
	next := current
	if p.StoryStructure != nil {
		next.StoryStructure = *p.StoryStructure
	}
	if p.PresentationFormat != nil {
		next.PresentationFormat = *p.PresentationFormat
	}
	if p.ContentMode != nil {
		next.ContentMode = *p.ContentMode
	}

	if err := next.Validate(); err != nil {
		return Format{}, err
	}
	return next, nil
}

// NeedsChatSetupWarning reports whether a format change moves a fiction into
// chat presentation, which means existing standard content will not render as
// chat until the author prepares it.
//
// The caller surfaces this as a WARNING to the author (docs/08 §3.1, §11); it
// must never be used to trigger an automatic conversion.
func NeedsChatSetupWarning(from, to Format) bool {
	return from.PresentationFormat != Chat && to.PresentationFormat == Chat
}

func join[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}
