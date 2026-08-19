package integration

import (
	"net/http"
	"testing"
)

// Phase 13R - the chapter number is a field the writer can fill in.
//
// It was always MAX+1, which is right until it is not: a fiction that numbers
// its side stories separately, restarts at 1 for a second arc, or leaves a gap
// for a chapter still being written is an arrangement its author chose. The
// studio suggests the next number and sends nothing unless the writer changes
// it, so the common case is exactly what it was.
//
// Two properties hold it up, and both are here: a number the writer chose is
// HONOURED, and one that is already taken is REFUSED rather than quietly moved.

// Omitting the field still appends, which is what every ordinary create does.
func TestChapterNumber_OmittedStillAppends(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	first := env.createChapter(t, w, novel.ID, map[string]any{"title": "หนึ่ง"})
	second := env.createChapter(t, w, novel.ID, map[string]any{"title": "สอง"})

	if second.ChapterNumber != first.ChapterNumber+1 {
		t.Fatalf("appended chapter number = %d, want %d",
			second.ChapterNumber, first.ChapterNumber+1)
	}
}

// A number the writer typed is the number the chapter gets - including a gap,
// which is a thing writers deliberately leave.
func TestChapterNumber_HonoursTheNumberTheWriterChose(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	env.createChapter(t, w, novel.ID, map[string]any{"title": "หนึ่ง"})

	chosen := env.createChapter(t, w, novel.ID, map[string]any{
		"title":          "บทพิเศษ",
		"chapter_number": 99,
	})
	if chosen.ChapterNumber != 99 {
		t.Fatalf("chapter_number = %d, want 99", chosen.ChapterNumber)
	}

	// And the gap is left alone: the next append goes after the highest, not
	// into the hole the writer opened.
	next := env.createChapter(t, w, novel.ID, map[string]any{"title": "ต่อไป"})
	if next.ChapterNumber != 100 {
		t.Fatalf("append after a chosen 99 = %d, want 100", next.ChapterNumber)
	}
}

// Refused, not relocated. A writer who asked for 3 and silently got 12 has had
// their chapter put somewhere they did not put it.
func TestChapterNumber_RefusesOneAlreadyInUse(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	taken := env.createChapter(t, w, novel.ID, map[string]any{"title": "หนึ่ง"})

	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/chapters",
		map[string]any{"title": "ชนกัน", "chapter_number": taken.ChapterNumber})
	if res.status != http.StatusConflict {
		t.Fatalf("duplicate chapter_number status = %d, want 409. body: %s",
			res.status, res.body)
	}

	// And nothing was created behind the refusal.
	listed := env.chapterList(t, w, novel.ID)
	if len(listed) != 1 {
		t.Fatalf("chapters after a refused create = %d, want 1", len(listed))
	}
}

// The bounds catch a slipped keypress in a numeric field. Zero is refused as
// well: the repository reads it as "append", and a sentinel that is also a
// legal value is a bug waiting for the first writer who wants a prologue at 0.
func TestChapterNumber_RefusesOneOutsideTheBounds(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	for _, number := range []int{0, -1, 100_001} {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/chapters",
			map[string]any{"chapter_number": number})
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("chapter_number=%d status = %d, want 422. body: %s",
				number, res.status, res.body)
		}
	}
}
