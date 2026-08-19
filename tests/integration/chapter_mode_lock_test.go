package integration

import (
	"net/http"
	"testing"
)

// Phase 13P - a chapter's mode is chosen once, at creation
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13P).
//
// The editor used to carry a dropdown that could move a chapter between prose,
// chat, and headcanon at any moment. The schema made that cheap - all three
// representations are stored side by side - and that was exactly why it was
// easy to get wrong: cheap to implement is not the same as right to offer. A
// chapter is a piece of writing with a shape, and one that can become a chat
// and back is one whose writer is never sure what they are looking at.
//
// Two properties hold it up, and both are here: the mode is STAMPED at creation
// so nothing else can move it, and a later attempt to change it is REFUSED
// rather than silently ignored.

// A chapter records its own mode even when it matches the fiction's, so a
// fiction-level change cannot reach back and move it.
func TestChapterMode_StampedAtCreation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "standard"})
	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": "ร้อยแก้ว"})

	if chapter.PresentationFormat == nil || *chapter.PresentationFormat != "standard" {
		t.Fatalf("chapter presentation_format = %v, want its own 'standard'",
			chapter.PresentationFormat)
	}

	// Move the FICTION to chat. The chapter must not follow.
	res := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID+"/format",
		map[string]any{"presentation_format": "chat"})
	if res.status != http.StatusOK {
		t.Fatalf("fiction format change status = %d. body: %s", res.status, res.body)
	}

	got := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID))
	if got.ActiveFormat != "standard" {
		t.Fatalf("chapter active_format = %q after a fiction-level change, want standard",
			got.ActiveFormat)
	}
	if got.Content == nil || *got.Content != "ร้อยแก้ว" {
		t.Fatalf("the manuscript moved: %v", got.Content)
	}
}

// The writer picks at creation, and what they picked is what they get.
func TestChapterMode_HonoursTheChoiceAtCreation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "standard"})

	for _, mode := range []string{"standard", "chat", "headcanon"} {
		chapter := env.createChapter(t, w, novel.ID,
			map[string]any{"presentation_format": mode})
		if chapter.ActiveFormat != mode {
			t.Fatalf("created with %q, got active_format %q", mode, chapter.ActiveFormat)
		}
	}
}

// Refused, not ignored. A client that asked for something and got a silent
// no-op is a client that will keep asking - and a writer who was told nothing
// is a writer who thinks it worked.
func TestChapterMode_RefusesAChangeAfterCreation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "standard"})
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"presentation_format": "headcanon",
		"entries":             headcanonEntries(),
		"entry_fields":        []string{"อายุ", "ราศี"},
	})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	for _, mode := range []string{"standard", "chat", ""} {
		res := env.asOwner(t, w, http.MethodPatch, path,
			map[string]any{"presentation_format": mode})
		if res.status != http.StatusConflict {
			t.Fatalf("changing the mode to %q status = %d, want 409. body: %s",
				mode, res.status, res.body)
		}
	}

	// Refusing must cost the writer nothing: the topic is exactly as it was.
	got := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, path))
	if got.ActiveFormat != "headcanon" || len(got.Entries) != 2 {
		t.Fatalf("a refused mode change disturbed the chapter: %+v", got)
	}
}

// Sending the mode it ALREADY has is not a change, and an editor that echoes
// the current value on an ordinary save must not be punished for it.
func TestChapterMode_AcceptsTheModeItAlreadyHas(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "standard"})
	chapter := env.createChapter(t, w, novel.ID,
		map[string]any{"presentation_format": "chat", "messages": chatMessages()})

	res := env.asOwner(t, w, http.MethodPatch,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID,
		map[string]any{"presentation_format": "chat", "title": "ชื่อใหม่"})
	if res.status != http.StatusOK {
		t.Fatalf("re-sending the same mode status = %d, want 200. body: %s",
			res.status, res.body)
	}
}
