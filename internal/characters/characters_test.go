package characters

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func ptr(s string) *string { return &s }

func TestValidateName(t *testing.T) {
	errs := map[string][]string{}
	if got := validateName("  ป้าแดง  ", errs); got != "ป้าแดง" {
		t.Errorf("name = %q, want trimmed", got)
	}
	if len(errs) != 0 {
		t.Errorf("a valid name should not error: %v", errs)
	}

	// The one required field: a nameless character cannot be referred to at all.
	errs = map[string][]string{}
	validateName("   ", errs)
	if len(errs["name"]) == 0 {
		t.Error("expected a blank name to be rejected")
	}

	errs = map[string][]string{}
	validateName(strings.Repeat("ก", nameMaxLength+1), errs)
	if len(errs["name"]) == 0 {
		t.Error("expected an over-long name to be rejected")
	}
}

func TestNormaliseClearsEmptyAndTrims(t *testing.T) {
	errs := map[string][]string{}

	if got := normalise(ptr("  ตัวละครหลัก  "), roleMaxLength, "role", errs); got == nil || *got != "ตัวละครหลัก" {
		t.Errorf("role = %v, want trimmed value", got)
	}
	// An emptied field is a CLEAR, not an error: a writer wiping a box means it.
	if got := normalise(ptr("   "), roleMaxLength, "role", errs); got != nil {
		t.Errorf("blank role should clear to nil, got %q", *got)
	}
	if got := normalise(nil, roleMaxLength, "role", errs); got != nil {
		t.Error("absent role should stay nil")
	}
	if len(errs) != 0 {
		t.Errorf("clearing should not produce errors: %v", errs)
	}

	normalise(ptr(strings.Repeat("ก", summaryMaxLength+1)), summaryMaxLength, "summary", errs)
	if len(errs["summary"]) == 0 {
		t.Error("expected an over-long summary to be rejected")
	}
}

func TestValidateTraitsDeduplicatesAndBounds(t *testing.T) {
	errs := map[string][]string{}

	got := validateTraits([]string{" เก็บความรู้สึก ", "เก็บความรู้สึก", "", "ใจแข็ง"}, errs)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(got) != 2 || got[0] != "เก็บความรู้สึก" || got[1] != "ใจแข็ง" {
		t.Errorf("traits = %v, want deduplicated and trimmed in order", got)
	}

	errs = map[string][]string{}
	validateTraits([]string{strings.Repeat("ก", traitMaxLength+1)}, errs)
	if len(errs["traits"]) == 0 {
		t.Error("expected an over-long trait to be rejected")
	}

	errs = map[string][]string{}
	many := make([]string, maxTraits+1)
	for i := range many {
		many[i] = string(rune('a' + i))
	}
	validateTraits(many, errs)
	if len(errs["traits"]) == 0 {
		t.Error("expected too many traits to be rejected")
	}
}

func TestValidateDetails(t *testing.T) {
	errs := map[string][]string{}

	got := validateDetails([]Detail{
		{Label: "  อายุ  ", Value: "  34  "},
		// No label: a half-filled editor row is work in progress, not an error.
		{Label: "   ", Value: "ignored"},
		// Labelled but empty: "อาชีพ: -" is a deliberate statement, so it stays.
		{Label: "อาชีพ", Value: ""},
	}, errs)

	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(got) != 2 {
		t.Fatalf("details = %v, want 2 entries", got)
	}
	if got[0] != (Detail{Label: "อายุ", Value: "34"}) {
		t.Errorf("first detail = %+v, want trimmed", got[0])
	}
	if got[1] != (Detail{Label: "อาชีพ", Value: ""}) {
		t.Errorf("second detail = %+v, want a labelled empty value kept", got[1])
	}

	errs = map[string][]string{}
	validateDetails([]Detail{{Label: strings.Repeat("ก", detailLabelMaxLen+1), Value: "x"}}, errs)
	if len(errs["details"]) == 0 {
		t.Error("expected an over-long label to be rejected")
	}

	errs = map[string][]string{}
	many := make([]Detail, maxDetails+1)
	for i := range many {
		many[i] = Detail{Label: string(rune('a' + i)), Value: "x"}
	}
	validateDetails(many, errs)
	if len(errs["details"]) == 0 {
		t.Error("expected too many details to be rejected")
	}
}

func TestViewNeverEmitsNullCollections(t *testing.T) {
	// A client maps over traits and details without guarding, so the API must
	// send [] rather than null for a character with neither.
	character := Character{ID: uuid.New(), NovelID: uuid.New(), Name: "ปุณณ์"}

	view := character.View()
	if view.Traits == nil {
		t.Error("traits should serialise as an empty array, not null")
	}
	if view.Details == nil {
		t.Error("details should serialise as an empty array, not null")
	}
	if view.FirstChapterID != nil {
		t.Error("an unset first appearance should be omitted, not zero-valued")
	}
	if view.AppearsIn != nil {
		t.Error("appearances are omitted from the list shape")
	}
}

func TestViewRendersFirstChapter(t *testing.T) {
	chapterID := uuid.New()
	character := Character{
		ID:             uuid.New(),
		NovelID:        uuid.New(),
		Name:           "ป้าแดง",
		FirstChapterID: &chapterID,
	}

	view := character.View()
	if view.FirstChapterID == nil || *view.FirstChapterID != chapterID.String() {
		t.Errorf("first_chapter_id = %v, want %s", view.FirstChapterID, chapterID)
	}
}
