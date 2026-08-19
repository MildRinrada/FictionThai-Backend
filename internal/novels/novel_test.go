package novels_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
)

// newNovel builds a published, public fiction that individual tests mutate.
func newNovel() novels.Novel {
	return novels.Novel{
		ID:         uuid.New(),
		AuthorID:   uuid.New(),
		Title:      "ตัวอย่างนิยาย",
		Slug:       "example",
		Format:     fiction.DefaultFormat(),
		Status:     novels.StatusOngoing,
		Visibility: novels.VisibilityPublic,
	}
}

// docs/08 §7.1: "A draft should never become publicly readable simply because
// the chapter has a public URL."
func TestNovel_Readable(t *testing.T) {
	deleted := time.Now()

	tests := map[string]struct {
		status     novels.Status
		visibility novels.Visibility
		deletedAt  *time.Time
		want       bool
	}{
		"published and public":   {novels.StatusOngoing, novels.VisibilityPublic, nil, true},
		"published and unlisted": {novels.StatusOngoing, novels.VisibilityUnlisted, nil, true},
		"completed and public":   {novels.StatusCompleted, novels.VisibilityPublic, nil, true},

		"private is never readable": {novels.StatusOngoing, novels.VisibilityPrivate, nil, false},
		"a draft is never readable": {novels.StatusDraft, novels.VisibilityPublic, nil, false},
		"deleted is never readable": {novels.StatusOngoing, novels.VisibilityPublic, &deleted, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			novel := newNovel()
			novel.Status, novel.Visibility, novel.DeletedAt = tc.status, tc.visibility, tc.deletedAt

			if got := novel.Readable(); got != tc.want {
				t.Errorf("Readable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// docs/11 §31: unlisted work is reachable by link but must not be discoverable.
func TestNovel_Listed_ExcludesUnlisted(t *testing.T) {
	novel := newNovel()
	novel.Visibility = novels.VisibilityUnlisted

	if !novel.Readable() {
		t.Error("unlisted work should be readable by direct link")
	}
	if novel.Listed() {
		t.Error("unlisted work must not appear in listings or search")
	}
}

func TestNovel_OwnedBy(t *testing.T) {
	novel := newNovel()

	if !novel.OwnedBy(novel.AuthorID) {
		t.Error("the author should own their own fiction")
	}
	if novel.OwnedBy(uuid.New()) {
		t.Error("a stranger must not own the fiction")
	}
	// A guest carries uuid.Nil. If that matched, every guest would own every
	// fiction whose author_id had somehow been left blank.
	if novel.OwnedBy(uuid.Nil) {
		t.Error("the nil UUID must never be treated as an owner")
	}
}

// docs/08 §2: the three dimensions are independent, so every combination is
// valid and none may be rejected for implementation convenience.
func TestNovel_EveryFormatCombinationIsRepresentable(t *testing.T) {
	combinations := 0

	for _, structure := range fiction.StoryStructures() {
		for _, presentation := range fiction.PresentationFormats() {
			for _, mode := range fiction.ContentModes() {
				format := fiction.Format{
					StoryStructure:     structure,
					PresentationFormat: presentation,
					ContentMode:        mode,
				}
				if err := format.Validate(); err != nil {
					t.Errorf("%v+%v+%v was rejected: %v", structure, presentation, mode, err)
				}
				combinations++
			}
		}
	}

	if combinations != 12 {
		t.Errorf("checked %d combinations, want 12", combinations)
	}
}

func record(novel novels.Novel) novels.Record {
	return novels.Record{
		Novel:             novel,
		Author:            novels.Author{ID: novel.AuthorID, Username: "writer"},
		PublishedChapters: 3,
		TotalChapters:     5,
	}
}

// docs/08 §1.4: private state must not leak into a public response.
func TestRecord_ViewFor_HidesOwnerFieldsFromReaders(t *testing.T) {
	view := record(newNovel()).ViewFor(false)

	if view.Visibility != nil {
		t.Error("visibility is owner metadata and must be omitted for a reader")
	}
	if view.DraftChapterCount != nil {
		t.Error("the draft chapter count must be omitted for a reader")
	}
	if view.IsOwner {
		t.Error("is_owner must be false for a reader")
	}
	// docs/11 §31: a chapter count that included drafts would leak how much
	// unpublished work exists.
	if view.ChapterCount != 3 {
		t.Errorf("chapter_count = %d, want 3 (published only)", view.ChapterCount)
	}
}

func TestRecord_ViewFor_GivesTheOwnerTheirPrivateState(t *testing.T) {
	view := record(newNovel()).ViewFor(true)

	if view.Visibility == nil || *view.Visibility != novels.VisibilityPublic {
		t.Error("the owner must see the fiction's visibility")
	}
	if view.ChapterCount != 5 {
		t.Errorf("chapter_count = %d, want 5 (every chapter) for the owner", view.ChapterCount)
	}
	if view.DraftChapterCount == nil || *view.DraftChapterCount != 2 {
		t.Error("the owner must see how many chapters are still unpublished")
	}
	if !view.IsOwner {
		t.Error("is_owner must be true for the owner")
	}
}

// docs/09 §14.5 shows the three dimensions flat on the resource, not nested.
func TestView_SerialisesFormatDimensionsFlat(t *testing.T) {
	novel := newNovel()
	novel.Format = fiction.Format{
		StoryStructure:     fiction.OneShot,
		PresentationFormat: fiction.Chat,
		ContentMode:        fiction.Headcanon,
	}

	encoded, err := json.Marshal(record(novel).ViewFor(false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for field, want := range map[string]string{
		"story_structure":     "one_shot",
		"presentation_format": "chat",
		"content_mode":        "headcanon",
	} {
		got, present := decoded[field]
		if !present {
			t.Errorf("%q is missing; clients read it directly off the resource", field)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %q", field, got, want)
		}
	}

	// The single-enum shape docs/08 §43 Rule 6 forbids.
	if _, present := decoded["type"]; present {
		t.Error(`the resource must not carry a collapsed "type" field`)
	}
	// A reader never receives owner-only state.
	if _, present := decoded["visibility"]; present {
		t.Error("visibility leaked into a reader's JSON")
	}
}

// A one-shot is a single reading unit, so clients must not render a chapter list
// for it (docs/15 §5.2). The flag is served by the API so web and mobile cannot
// disagree about the rule (docs/09 §51).
func TestView_ReportsChapterNavigation(t *testing.T) {
	novel := newNovel()

	novel.Format.StoryStructure = fiction.OneShot
	if record(novel).ViewFor(false).UsesChapterNavigation {
		t.Error("a one-shot must not advertise chapter navigation")
	}

	novel.Format.StoryStructure = fiction.MultiChapter
	if !record(novel).ViewFor(false).UsesChapterNavigation {
		t.Error("a multi-chapter fiction must advertise chapter navigation")
	}
}

// Presentation and structure are independent: chat says nothing about whether
// the work has chapters (docs/09 §14.4, §16).
func TestView_ChapterNavigationIsIndependentOfPresentation(t *testing.T) {
	novel := newNovel()
	novel.Format = fiction.Format{
		StoryStructure:     fiction.MultiChapter,
		PresentationFormat: fiction.Chat,
		ContentMode:        fiction.Headcanon,
	}

	if !record(novel).ViewFor(false).UsesChapterNavigation {
		t.Error("multi_chapter + chat must still offer chapter navigation")
	}
}

func TestStatusAndVisibility_RejectUnknownValues(t *testing.T) {
	for _, status := range novels.Statuses() {
		if !status.Valid() {
			t.Errorf("documented status %q was rejected", status)
		}
	}
	for _, bad := range []novels.Status{"", "published", "archived", "DRAFT"} {
		if bad.Valid() {
			t.Errorf("undocumented status %q was accepted", bad)
		}
	}

	for _, visibility := range novels.Visibilities() {
		if !visibility.Valid() {
			t.Errorf("documented visibility %q was rejected", visibility)
		}
	}
	for _, bad := range []novels.Visibility{"", "hidden", "PUBLIC"} {
		if bad.Valid() {
			t.Errorf("undocumented visibility %q was accepted", bad)
		}
	}
}
