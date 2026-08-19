package integration

import (
	"net/http"
	"strings"
	"testing"
)

// Phase 13J - รูปแบบผลงาน: the four-way work format, per-chapter formats, and
// headcanon as a real representation
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13J, docs/PHASE-12-STORY-DEPTH.md §12F).
//
// The promise the create form now makes out loud is that none of the four
// choices is a lock. These tests are that promise, checked against a real
// database: every switch below writes metadata and nothing else, and the
// writer's three representations come back byte-identical afterwards.

// A chapter may hold prose, chat, AND entries at once. Changing which one
// readers see moves none of them (docs/CONTENT-MODEL.md §2, 12F's test
// obligation).
func TestWorkFormat_ThreeRepresentationsSurviveEveryFormatChange(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	const prose = "ฝนหยุดตกไปตั้งแต่เมื่อไหร่ไม่มีใครทันสังเกต"

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Mixed "), map[string]any{
		"presentation_format": "standard",
	}))
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title":        "ตอนที่มีทุกอย่าง",
		"content":      prose,
		"messages":     chatMessages(),
		"entries":      headcanonEntries(),
		"entry_fields": []string{"อายุ", "ราศี"},
	})

	if chapter.HasStandardContent == nil || !*chapter.HasStandardContent ||
		chapter.HasChatContent == nil || !*chapter.HasChatContent ||
		chapter.HasEntries == nil || !*chapter.HasEntries {
		t.Fatalf("the owner must be told all three representations exist: %+v", chapter)
	}

	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	// Since 13P a chapter's own mode is fixed at creation, so the only trip
	// left is the FICTION-level one - and it must touch nothing. That is the
	// older guarantee, and it is the one that survives.
	res := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID+"/format",
		map[string]any{"presentation_format": "headcanon", "content_mode": "headcanon"})
	if res.status != http.StatusOK {
		t.Fatalf("fiction format change status = %d. body: %s", res.status, res.body)
	}

	after := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, path))
	if after.Content == nil || *after.Content != prose ||
		len(after.Messages) != 4 || len(after.Entries) != 2 ||
		len(after.EntryFields) != 2 {
		t.Fatalf("a fiction-level format change touched content: %+v", after)
	}
	// And it did not move the chapter either: the mode is the chapter's own.
	if after.ActiveFormat != "standard" {
		t.Fatalf("the chapter followed the fiction to %q; it is stamped (13P)",
			after.ActiveFormat)
	}
}

// The resolution rule since 13P: every chapter carries its OWN mode, stamped
// when it was created, and the fiction's format is what a NEW chapter starts
// from rather than what an existing one obeys.
func TestWorkFormat_ChapterFormatResolution(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Resolve "), map[string]any{
		"presentation_format": "standard",
	}))

	inherits := env.createChapter(t, w, novel.ID, map[string]any{"content": "ฟิคธรรมดา"})
	declares := env.createChapter(t, w, novel.ID, map[string]any{
		"presentation_format": "chat",
		"messages":            chatMessages(),
	})

	// Stamped from the fiction, not left null. NULL used to mean "follow the
	// fiction", which made a later fiction-level change able to move a prose
	// chapter into chat behind its author's back (13P).
	if inherits.PresentationFormat == nil || *inherits.PresentationFormat != "standard" {
		t.Errorf("a chapter must record its own mode, got %v", inherits.PresentationFormat)
	}
	if inherits.ActiveFormat != "standard" {
		t.Errorf("inheriting chapter active_format = %q, want standard", inherits.ActiveFormat)
	}
	if declares.ActiveFormat != "chat" {
		t.Errorf("declaring chapter active_format = %q, want chat", declares.ActiveFormat)
	}

	// The badge is DERIVED: it says "ผสมรูปแบบ" only when a reader would
	// actually meet more than one format (§13J, revised). A stored flag would
	// have been a lock the writer could not see the effect of.
	fiction := dataOf[novelBody](t, env.asOwner(t, w, http.MethodGet,
		"/api/v1/novels/"+novel.ID))
	if !fiction.HasMixedFormats {
		t.Error("a fiction whose chapters disagree with it is mixed, and must say so")
	}

	// And it stays mixed, because the way out is no longer a switch - it is the
	// writer deciding what each chapter is. The mode is locked, so the badge
	// cannot be turned off by an accidental PATCH either.
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + declares.ID
	res := env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"presentation_format": ""})
	if res.status != http.StatusConflict {
		t.Fatalf("clearing a chapter's mode status = %d, want 409 (13P). body: %s",
			res.status, res.body)
	}
}

// A reader receives ONLY the active representation, whichever of the three it
// is (docs/CONTENT-MODEL.md §6). This is the leak test: a guest reading a
// headcanon chapter must not receive the prose sitting beside it.
func TestWorkFormat_ReaderReceivesOnlyTheActiveRepresentation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	const secret = "ฟิคที่ยังไม่อยากให้ใครอ่าน"

	novel := env.publishedNovel(t, w, map[string]any{
		"presentation_format": "headcanon",
		"content_mode":        "headcanon",
	})
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"content":      secret,
		"messages":     chatMessages(),
		"entries":      headcanonEntries(),
		"entry_fields": []string{"อายุ", "ราศี"},
	})
	env.publishChapter(t, w, novel.ID, chapter.ID)

	res := env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.Slug+"/chapters/"+chapter.Slug)
	if res.status != http.StatusOK {
		t.Fatalf("guest read status = %d. body: %s", res.status, res.body)
	}

	got := dataOf[chapterBody](t, res)
	if len(got.Entries) != 2 {
		t.Fatalf("the guest must receive the entries: %+v", got.Entries)
	}
	if got.Content != nil {
		t.Error("a headcanon reader must not receive the prose")
	}
	if got.Messages != nil {
		t.Error("a headcanon reader must not receive the chat messages")
	}
	// Not merely absent from the field - absent from the response entirely.
	if strings.Contains(string(res.body), secret) {
		t.Fatal("the inactive prose leaked into a reader's response body")
	}
	if got.HasStandardContent != nil || got.HasEntries != nil {
		t.Error("owner-only presence flags must not reach a reader")
	}
}

// Positions come from array order and are never accepted from the client, so a
// gap or a duplicate is not representable (docs/CONTENT-MODEL.md §4).
func TestWorkFormat_EntryPositionsComeFromArrayOrder(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Topic "), map[string]any{
		"presentation_format": "headcanon",
	}))
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"entries": []map[string]any{
			{"name": "หนึ่ง", "body": "ก", "position": 99},
			{"name": "สอง", "body": "ข", "position": 99},
			{"name": "สาม", "body": "ค"},
		},
	})

	for i, entry := range chapter.Entries {
		if entry.Position != i {
			t.Fatalf("entry %d has position %d - the client's value was trusted", i, entry.Position)
		}
	}

	// Replacing the topic reorders it wholesale, and the previous version is
	// kept as a revision rather than lost.
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID
	updated := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"entries": []map[string]any{
			{"name": "สาม", "body": "ค"},
			{"name": "หนึ่ง", "body": "ก"},
		}}))
	if len(updated.Entries) != 2 || updated.Entries[0].Name != "สาม" {
		t.Fatalf("entries were not replaced in order: %+v", updated.Entries)
	}

	// An entry needs a name; a nameless row would render as an unlabelled card.
	res := env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"entries": []map[string]any{{"name": "  ", "body": "ง"}}})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("nameless entry status = %d, want 422. body: %s", res.status, res.body)
	}
}

// An entry may link to a character, but only to one of THIS fiction's - the
// mirror of the rule characters already applies to chapter references
// (docs/11 §8).
func TestWorkFormat_EntryCannotBorrowAnotherFictionsCharacter(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	mine := env.createNovel(t, w, createNovelBody(uniqueName(t, "Mine "), nil))
	theirs := env.createNovel(t, w, createNovelBody(uniqueName(t, "Theirs "), nil))

	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+theirs.ID+"/characters",
		map[string]any{"name": "ตัวละครของอีกเรื่อง"})
	if res.status != http.StatusCreated {
		t.Fatalf("create character status = %d. body: %s", res.status, res.body)
	}
	outsider := dataOf[struct {
		ID string `json:"id"`
	}](t, res)

	res = env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+mine.ID+"/chapters",
		map[string]any{"entries": []map[string]any{
			{"name": "ยืมมา", "character_id": outsider.ID, "body": "ไม่ควรผ่าน"},
		}})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("borrowed character status = %d, want 422. body: %s", res.status, res.body)
	}
	if !strings.Contains(string(res.body), "character_id") {
		t.Errorf("the error does not name the offending field: %s", res.body)
	}
}

// The word count is what a writer is told they have written. It must not drop
// because they changed which version readers see.
func TestWorkFormat_WordCountCoversEveryRepresentation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Counting "), map[string]any{}))
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"content": "one two three",
		"entries": []map[string]any{{"name": "someone", "body": "four five"}},
	})
	if chapter.WordCount != 5 {
		t.Fatalf("word_count = %d, want 5 (prose + entry bodies)", chapter.WordCount)
	}

	// The count covers every representation the chapter holds, so editing one
	// never makes the other's words disappear from the total.
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID
	edited := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"content": "one two three four"}))
	if edited.WordCount != 6 {
		t.Errorf("word_count = %d, want 6 (new prose + the untouched entry)",
			edited.WordCount)
	}
}
