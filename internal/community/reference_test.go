package community

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// Phase 12D - the reference a post carries
// (docs/PHASE-12-STORY-DEPTH.md §12D).
//
// The database-backed rules are proved by the integration suite; what is worth
// pinning here is the part that decides, from a row alone, whether a card
// exists at all - and the fact that "no card" is invisible on the wire rather
// than a null the UI has to interpret.

func TestReferenceInputEmpty(t *testing.T) {
	empty := []ReferenceInput{
		{},
		{NovelRef: "   "},
		{ChapterRef: "\t\n"},
		{NovelRef: " ", ChapterRef: " "},
	}
	for _, input := range empty {
		if !input.Empty() {
			t.Fatalf("%+v should count as no attachment", input)
		}
	}

	if (ReferenceInput{NovelRef: "the-old-pier"}).Empty() {
		t.Fatal("a fiction reference is an attachment")
	}
	// A chapter without its fiction is NOT empty: it is a malformed
	// attachment, and the service must be given the chance to say so rather
	// than silently treating it as "attached nothing".
	if (ReferenceInput{ChapterRef: "chapter-7"}).Empty() {
		t.Fatal("a lone chapter reference must reach validation, not be dropped")
	}
}

func TestReferenceScanBuild(t *testing.T) {
	novelID := uuid.New()
	chapterID := uuid.New()

	t.Run("no fiction means no card", func(t *testing.T) {
		// What the LEFT JOIN produces when the viewer may not open the work -
		// exactly what it produces when the post attached nothing at all. The
		// two are indistinguishable on purpose (§12D).
		var scan referenceScan
		if scan.build() != nil {
			t.Fatal("an unresolved reference must not become a card")
		}
	})

	t.Run("a fiction without a chapter carries no chapter fields", func(t *testing.T) {
		scan := referenceScan{
			novelID:      uuid.NullUUID{UUID: novelID, Valid: true},
			novelSlug:    sql.NullString{String: "the-old-pier", Valid: true},
			novelTitle:   sql.NullString{String: "ปลายฝนที่ท่าน้ำเก่า", Valid: true},
			presentation: sql.NullString{String: "standard", Valid: true},
		}

		ref := scan.build()
		if ref == nil || ref.NovelID != novelID {
			t.Fatalf("fiction reference lost: %+v", ref)
		}
		if ref.NovelTitle != "ปลายฝนที่ท่าน้ำเก่า" {
			t.Fatalf("Thai title mangled: %q", ref.NovelTitle)
		}
		if ref.ChapterID != nil || ref.ChapterNumber != nil || ref.WordCount != nil {
			t.Fatalf("chapter fields invented for a fiction-only card: %+v", ref)
		}
		if ref.CoverURL != nil {
			t.Fatalf("a fiction with no cover must not get one: %+v", ref.CoverURL)
		}
	})

	t.Run("a chapter carries its own label fields", func(t *testing.T) {
		scan := referenceScan{
			novelID:       uuid.NullUUID{UUID: novelID, Valid: true},
			novelSlug:     sql.NullString{String: "the-old-pier", Valid: true},
			novelTitle:    sql.NullString{String: "ปลายฝนที่ท่าน้ำเก่า", Valid: true},
			chapterID:     uuid.NullUUID{UUID: chapterID, Valid: true},
			chapterSlug:   sql.NullString{String: "high-tide", Valid: true},
			chapterNumber: sql.NullInt64{Int64: 7, Valid: true},
			chapterTitle:  sql.NullString{String: "น้ำขึ้นตอนตีสาม", Valid: true},
			wordCount:     sql.NullInt64{Int64: 2140, Valid: true},
		}

		ref := scan.build()
		if ref.ChapterID == nil || *ref.ChapterID != chapterID {
			t.Fatalf("chapter id lost: %+v", ref)
		}
		if ref.ChapterNumber == nil || *ref.ChapterNumber != 7 {
			t.Fatalf("chapter number lost: %+v", ref)
		}
		if ref.WordCount == nil || *ref.WordCount != 2140 {
			t.Fatalf("word count lost: %+v", ref)
		}
	})

	t.Run("an untitled chapter stays untitled", func(t *testing.T) {
		// Chapters may have no title; the card must say nothing rather than
		// inventing one from the fiction.
		scan := referenceScan{
			novelID:       uuid.NullUUID{UUID: novelID, Valid: true},
			novelTitle:    sql.NullString{String: "เมืองไร้เงา", Valid: true},
			chapterID:     uuid.NullUUID{UUID: chapterID, Valid: true},
			chapterNumber: sql.NullInt64{Int64: 3, Valid: true},
		}
		if ref := scan.build(); ref.ChapterTitle != nil {
			t.Fatalf("an untitled chapter was given a title: %q", *ref.ChapterTitle)
		}
	})
}

// A post with no visible reference must serialise WITHOUT the key, so a client
// never has to distinguish "absent" from "null" to decide whether to render a
// card (§12D).
func TestPostViewOmitsAbsentReference(t *testing.T) {
	post := Post{ID: uuid.New(), AuthorID: uuid.New(), Content: "ไม่ได้แนบอะไร"}

	encoded, err := json.Marshal(post.Render(uuid.Nil))
	if err != nil {
		t.Fatalf("marshal post view: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal post view: %v", err)
	}
	if _, present := decoded["reference"]; present {
		t.Fatalf("a post with no card still carries a reference key: %s", encoded)
	}
	// The raw ids are never on the wire at all: the only way a client learns
	// about the attachment is the resolved card.
	for _, leaked := range []string{"novel_id", "chapter_id"} {
		if _, present := decoded[leaked]; present {
			t.Fatalf("%s leaked onto the post resource: %s", leaked, encoded)
		}
	}
}
