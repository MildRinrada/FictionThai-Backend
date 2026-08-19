package chapters_test

import (
	"testing"

	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/fiction"
)

// Phase 13J - where the presentation selector lives
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13J, docs/CONTENT-MODEL.md §2).
//
// One rule, one function: active = chapter.Format ?? novel.PresentationFormat,
// unconditionally. Everything a client renders follows from this, so it is
// tested directly rather than only through the API.

func fictionFormat(presentation fiction.PresentationFormat) fiction.Format {
	return fiction.Format{
		StoryStructure:     fiction.MultiChapter,
		PresentationFormat: presentation,
		ContentMode:        fiction.General,
	}
}

func TestChapter_ActiveFormat(t *testing.T) {
	chat := fiction.Chat
	headcanon := fiction.HeadcanonFormat

	cases := []struct {
		name    string
		novel   fiction.Format
		chapter *fiction.PresentationFormat
		want    fiction.PresentationFormat
	}{
		{
			name:  "a chapter that declares nothing follows the fiction",
			novel: fictionFormat(fiction.Chat),
			want:  fiction.Chat,
		},
		{
			name:    "a mixed fiction honours what the chapter declared",
			novel:   fictionFormat(fiction.Standard),
			chapter: &chat,
			want:    fiction.Chat,
		},
		{
			name:    "headcanon is a chapter a mixed fiction may hold",
			novel:   fictionFormat(fiction.Standard),
			chapter: &headcanon,
			want:    fiction.HeadcanonFormat,
		},
		{
			// EVERY fiction can mix. There is no flag to turn it on, because a
			// writer who picked one format can already change a chapter later -
			// a gate would have prevented nothing while telling them the other
			// choices had locked something (§13J, revised after review).
			name:    "a declaration is honoured whatever the fiction chose",
			novel:   fictionFormat(fiction.Standard),
			chapter: &chat,
			want:    fiction.Chat,
		},
		{
			name:    "an unknown value from an older row falls back rather than rendering wrongly",
			novel:   fictionFormat(fiction.Standard),
			chapter: ptr(fiction.PresentationFormat("script")),
			want:    fiction.Standard,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chapter := newChapter()
			chapter.Format = tc.chapter

			if got := chapter.ActiveFormat(tc.novel); got != tc.want {
				t.Errorf("ActiveFormat() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A chapter's declaration is stored, not applied to the text, and it is honoured
// no matter what the fiction itself is set to.
func TestChapter_ActiveFormat_DeclarationSurvivesAFictionLevelChange(t *testing.T) {
	chapter := newChapter()
	chapter.Format = ptr(fiction.HeadcanonFormat)

	for _, presentation := range []fiction.PresentationFormat{
		fiction.Standard, fiction.Chat, fiction.HeadcanonFormat,
	} {
		if got := chapter.ActiveFormat(fictionFormat(presentation)); got != fiction.HeadcanonFormat {
			t.Fatalf("with the fiction on %q, ActiveFormat() = %q - the chapter's own "+
				"declaration must win", presentation, got)
		}
	}
	if chapter.Format == nil || *chapter.Format != fiction.HeadcanonFormat {
		t.Fatal("resolving the format mutated the declaration")
	}
}

// A headcanon chapter renders its entries and nothing else, exactly as a chat
// chapter renders its messages (docs/CONTENT-MODEL.md §6).
func TestChapter_Render_HeadcanonServesEntriesOnly(t *testing.T) {
	chapter := newChapter()
	chapter.Content = ptr("Prose written before this became a topic.")
	chapter.EntryFields = []string{"อายุ"}

	view := chapter.Render(chapters.ViewParams{
		Active:   fiction.HeadcanonFormat,
		Messages: messages(),
		Entries:  entries(),
	})

	if view.Content != nil {
		t.Error("a headcanon reader must not receive the prose")
	}
	if view.Messages != nil {
		t.Error("a headcanon reader must not receive chat messages")
	}
	if len(view.Entries) != 1 || view.Entries[0].Name != "อลิซ" {
		t.Fatalf("entries = %+v", view.Entries)
	}
	if len(view.EntryFields) != 1 || view.EntryFields[0] != "อายุ" {
		t.Errorf("the topic's field labels must travel with it: %v", view.EntryFields)
	}
	if !view.ContentReady {
		t.Error("a topic with entries is ready")
	}
}

// The setup state, for the third representation: a chapter that became a topic
// but has no entries yet reports not-ready rather than having its prose
// silently reinterpreted as one (docs/08 §11).
func TestChapter_Render_HeadcanonWithoutEntriesIsNotReady(t *testing.T) {
	chapter := newChapter()
	chapter.Content = ptr("Prose only.")

	view := chapter.Render(chapters.ViewParams{Active: fiction.HeadcanonFormat})
	if view.ContentReady {
		t.Error("prose does not make a headcanon topic ready")
	}
	if view.Entries != nil {
		t.Error("there are no entries to send")
	}
}

// The resolved format is on the wire, so a client renders the server's answer
// instead of re-deriving the rule (docs/09 §51).
func TestSummary_CarriesTheResolvedFormat(t *testing.T) {
	chapter := newChapter()
	chapter.Format = ptr(fiction.HeadcanonFormat)

	summary := chapter.Summarize(fiction.HeadcanonFormat, chapters.Presence{HasEntries: true})

	if summary.ActiveFormat != fiction.HeadcanonFormat {
		t.Errorf("active_format = %q", summary.ActiveFormat)
	}
	if summary.PresentationFormat == nil || *summary.PresentationFormat != fiction.HeadcanonFormat {
		t.Error("the chapter's own declaration must travel too, so an editor can show it")
	}

	// A chapter that declares nothing sends null - distinguishable from a build
	// of the API that does not send the field at all.
	inheriting := newChapter()
	if inheriting.Summarize(fiction.Standard, chapters.Presence{}).PresentationFormat != nil {
		t.Error("a chapter that follows the fiction must report null, not a value")
	}
}
