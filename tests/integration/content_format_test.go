package integration

import (
	"net/http"
	"testing"
)

// Phase 13N - the writer editor's content model
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13N, answering docs/CONTENT-MODEL.md §3).
//
// The formatting toolbar is a client feature; what the API owns is the
// DISCRIMINATOR, and these tests are the two things it has to be true about:
// changing it writes no content, and it is never changed on an author's behalf.
//
// There is deliberately no test that the server rejects "dangerous markup".
// There is no markup parser on the write path at all - the value stays text and
// the renderer builds elements from it - so there is no such thing to reject.

// A format change is metadata. Not one byte of the manuscript moves, in either
// direction, which is what makes it safe to offer at all (docs/08 §43 Rule 7).
func TestContentFormat_SwitchingWritesNoContent(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	const manuscript = "**เขาเดินเข้ามา**\n\n- หนึ่ง\n- สอง"

	novel := env.publishedNovel(t, w, nil)
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title":   "ตอนที่มีเครื่องหมาย",
		"content": manuscript,
	})

	// A new chapter has nothing to reinterpret, so it gets the editor's model.
	if chapter.ContentFormat != "markdown" {
		t.Fatalf("new chapter content_format = %q, want markdown", chapter.ContentFormat)
	}

	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	for _, format := range []string{"plain", "markdown", "plain"} {
		res := env.asOwner(t, w, http.MethodPatch, path,
			map[string]any{"content_format": format})
		if res.status != http.StatusOK {
			t.Fatalf("set content_format %q status = %d. body: %s", format, res.status, res.body)
		}

		got := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, path))
		if got.ContentFormat != format {
			t.Fatalf("content_format = %q, want %q", got.ContentFormat, format)
		}
		// The markers the author typed are still exactly where they typed them.
		if got.Content == nil || *got.Content != manuscript {
			t.Fatalf("manuscript changed on the way to %q: %+v", format, got.Content)
		}
		if got.WordCount != chapter.WordCount {
			t.Fatalf("word count moved on a metadata change: %d -> %d",
				chapter.WordCount, got.WordCount)
		}
	}
}

// An ordinary save never mentions the format, and must therefore never move it.
// This is what keeps a chapter written before the editor existed literal until
// its author decides otherwise.
func TestContentFormat_AnOrdinarySaveLeavesItAlone(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, nil)
	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": "เริ่มต้น"})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	// Put it on the pre-13N model, the way an older chapter sits.
	if res := env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"content_format": "plain"}); res.status != http.StatusOK {
		t.Fatalf("set plain status = %d. body: %s", res.status, res.body)
	}

	// Several ordinary edits, none of which mentions the format.
	for _, body := range []map[string]any{
		{"content": "เขียนต่อ **แบบนี้**"},
		{"title": "ชื่อใหม่"},
		{"status": "draft"},
	} {
		if res := env.asOwner(t, w, http.MethodPatch, path, body); res.status != http.StatusOK {
			t.Fatalf("patch %v status = %d. body: %s", body, res.status, res.body)
		}
		got := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, path))
		if got.ContentFormat != "plain" {
			t.Fatalf("patch %v moved content_format to %q", body, got.ContentFormat)
		}
	}
}

// The vocabulary is closed, and an unknown value is a field error rather than a
// silently-accepted string the reader would have to guess at (docs/09 §52).
func TestContentFormat_RefusesAnUnknownValue(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, nil)
	chapter := env.createChapter(t, w, novel.ID, nil)

	res := env.asOwner(t, w, http.MethodPatch,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID,
		map[string]any{"content_format": "html"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("content_format=html status = %d, want 422. body: %s", res.status, res.body)
	}

	create := env.asOwner(t, w, http.MethodPost,
		"/api/v1/novels/"+novel.ID+"/chapters",
		map[string]any{"content_format": "rtf"})
	if create.status != http.StatusUnprocessableEntity {
		t.Fatalf("create with content_format=rtf status = %d, want 422. body: %s",
			create.status, create.body)
	}
}

// A reader gets the format with the chapter, because it is what decides how the
// text they were sent is read. Without it a client would have to guess, and a
// guess is the thing the discriminator exists to prevent.
func TestContentFormat_ReachesTheReader(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, nil)
	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": "**หนา**"})
	env.publishChapter(t, w, novel.ID, chapter.ID)

	got := dataOf[chapterBody](t, env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID))
	if got.ContentFormat != "markdown" {
		t.Fatalf("guest received content_format = %q, want markdown", got.ContentFormat)
	}

	// And on the LIST, which is where the studio shows a writer which of their
	// older chapters are still literal text.
	listed, _ := collectionOf[chapterBody](t, env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters"))
	if len(listed) != 1 || listed[0].ContentFormat != "markdown" {
		t.Fatalf("chapter summary content_format = %+v", listed)
	}
}
