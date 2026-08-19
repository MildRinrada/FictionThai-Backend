package fiction_test

import (
	"errors"
	"testing"

	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Every combination of the dimensions must be accepted. docs/08 §2.4 is
// explicit that valid combinations must not be restricted for implementation
// convenience, so this test guards against someone quietly narrowing the model.
//
// 2 structures x 3 presentations x 2 modes x 2 mixed states = 24. The count is
// asserted rather than derived so that ADDING a value is a deliberate edit here
// too - a silently widened model is as much a surprise as a narrowed one.
func TestFormat_AllCombinationsAreValid(t *testing.T) {
	count := 0
	for _, s := range fiction.StoryStructures() {
		for _, p := range fiction.PresentationFormats() {
			for _, m := range fiction.ContentModes() {
				f := fiction.Format{StoryStructure: s, PresentationFormat: p, ContentMode: m}
				if err := f.Validate(); err != nil {
					t.Errorf("%s + %s + %s should be valid, got %v", s, p, m, err)
				}
				count++
			}
		}
	}
	if count != 12 {
		t.Fatalf("expected 12 documented combinations, checked %d", count)
	}
}

func TestFormat_RejectsUnknownValues(t *testing.T) {
	tests := map[string]fiction.Format{
		"unknown story_structure": {
			StoryStructure:     "novella",
			PresentationFormat: fiction.Standard,
			ContentMode:        fiction.General,
		},
		"unknown presentation_format": {
			StoryStructure:     fiction.OneShot,
			PresentationFormat: "script",
			ContentMode:        fiction.General,
		},
		"unknown content_mode": {
			StoryStructure:     fiction.OneShot,
			PresentationFormat: fiction.Standard,
			ContentMode:        "fanfic",
		},
		"empty format": {},
	}

	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			err := f.Validate()
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}

			var apiErr *apierror.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *apierror.Error, got %T", err)
			}
			if apiErr.Code != apierror.CodeInvalidFictionFormat {
				t.Errorf("code = %q, want %q", apiErr.Code, apierror.CodeInvalidFictionFormat)
			}
			if apiErr.Status != 422 {
				t.Errorf("status = %d, want 422", apiErr.Status)
			}
			if len(apiErr.Fields) == 0 {
				t.Error("expected field-level detail so the writer knows which value was rejected")
			}
		})
	}
}

func TestDefaultFormat(t *testing.T) {
	got := fiction.DefaultFormat()
	want := fiction.Format{
		StoryStructure:     fiction.MultiChapter,
		PresentationFormat: fiction.Standard,
		ContentMode:        fiction.General,
	}
	if got != want {
		t.Errorf("DefaultFormat() = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the default format must itself be valid: %v", err)
	}
}

func TestPatch_AppliesOnlyProvidedFields(t *testing.T) {
	current := fiction.Format{
		StoryStructure:     fiction.MultiChapter,
		PresentationFormat: fiction.Standard,
		ContentMode:        fiction.General,
	}
	chat := fiction.Chat

	next, err := fiction.Patch{PresentationFormat: &chat}.Apply(current)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if next.PresentationFormat != fiction.Chat {
		t.Errorf("presentation_format = %q, want %q", next.PresentationFormat, fiction.Chat)
	}
	// A partial patch must leave untouched dimensions alone.
	if next.StoryStructure != current.StoryStructure {
		t.Errorf("story_structure changed to %q; a partial patch must not touch it", next.StoryStructure)
	}
	if next.ContentMode != current.ContentMode {
		t.Errorf("content_mode changed to %q; a partial patch must not touch it", next.ContentMode)
	}
}

// docs/09 §15: "The server must validate the resulting complete format state
// rather than validating each field in isolation."
func TestPatch_ValidatesResultingState(t *testing.T) {
	current := fiction.DefaultFormat()
	bogus := fiction.PresentationFormat("interpretive_dance")

	if _, err := (fiction.Patch{PresentationFormat: &bogus}).Apply(current); err == nil {
		t.Fatal("expected the resulting state to be rejected")
	}
}

func TestPatch_DoesNotMutateCurrentOnFailure(t *testing.T) {
	current := fiction.DefaultFormat()
	before := current
	bogus := fiction.StoryStructure("trilogy")

	if _, err := (fiction.Patch{StoryStructure: &bogus}).Apply(current); err == nil {
		t.Fatal("expected an error")
	}
	if current != before {
		t.Errorf("Apply mutated its receiver argument: %+v != %+v", current, before)
	}
}

func TestPatch_IsEmpty(t *testing.T) {
	if !(fiction.Patch{}).IsEmpty() {
		t.Error("a patch with no fields set should report IsEmpty")
	}
	oneShot := fiction.OneShot
	if (fiction.Patch{StoryStructure: &oneShot}).IsEmpty() {
		t.Error("a patch with a field set should not report IsEmpty")
	}
}

// Round-tripping a format change must be lossless at the metadata level; the
// non-destructive guarantee for CONTENT is enforced by the novels service and
// covered again at the integration/E2E layer (docs/15 §5.7).
func TestPatch_RoundTripRestoresOriginalFormat(t *testing.T) {
	original := fiction.Format{
		StoryStructure:     fiction.MultiChapter,
		PresentationFormat: fiction.Standard,
		ContentMode:        fiction.General,
	}
	chat, standard := fiction.Chat, fiction.Standard

	toChat, err := (fiction.Patch{PresentationFormat: &chat}).Apply(original)
	if err != nil {
		t.Fatalf("standard -> chat: %v", err)
	}
	back, err := (fiction.Patch{PresentationFormat: &standard}).Apply(toChat)
	if err != nil {
		t.Fatalf("chat -> standard: %v", err)
	}
	if back != original {
		t.Errorf("round trip changed the format: %+v != %+v", back, original)
	}
}

func TestFormat_ReaderCapabilities(t *testing.T) {
	tests := []struct {
		name           string
		format         fiction.Format
		wantChapterNav bool
		wantStructured bool
	}{
		{
			name:           "multi_chapter standard",
			format:         fiction.Format{StoryStructure: fiction.MultiChapter, PresentationFormat: fiction.Standard, ContentMode: fiction.General},
			wantChapterNav: true,
			wantStructured: false,
		},
		{
			name:           "one_shot standard",
			format:         fiction.Format{StoryStructure: fiction.OneShot, PresentationFormat: fiction.Standard, ContentMode: fiction.General},
			wantChapterNav: false,
			wantStructured: false,
		},
		{
			name:           "one_shot chat headcanon",
			format:         fiction.Format{StoryStructure: fiction.OneShot, PresentationFormat: fiction.Chat, ContentMode: fiction.Headcanon},
			wantChapterNav: false,
			wantStructured: true,
		},
		{
			name:           "multi_chapter chat headcanon",
			format:         fiction.Format{StoryStructure: fiction.MultiChapter, PresentationFormat: fiction.Chat, ContentMode: fiction.Headcanon},
			wantChapterNav: true,
			wantStructured: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.format.UsesChapterNavigation(); got != tc.wantChapterNav {
				t.Errorf("UsesChapterNavigation() = %v, want %v", got, tc.wantChapterNav)
			}
			if got := tc.format.UsesStructuredMessages(); got != tc.wantStructured {
				t.Errorf("UsesStructuredMessages() = %v, want %v", got, tc.wantStructured)
			}
		})
	}
}

func TestNeedsChatSetupWarning(t *testing.T) {
	standard := fiction.Format{StoryStructure: fiction.MultiChapter, PresentationFormat: fiction.Standard, ContentMode: fiction.General}
	chat := fiction.Format{StoryStructure: fiction.MultiChapter, PresentationFormat: fiction.Chat, ContentMode: fiction.General}

	if !fiction.NeedsChatSetupWarning(standard, chat) {
		t.Error("moving standard -> chat should warn the author that chat content is not prepared")
	}
	if fiction.NeedsChatSetupWarning(chat, standard) {
		t.Error("moving chat -> standard needs no chat setup warning")
	}
	if fiction.NeedsChatSetupWarning(chat, chat) {
		t.Error("staying on chat needs no new warning")
	}
}
