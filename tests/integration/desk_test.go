package integration

import (
	"net/http"
	"testing"
)

// GET /me/desk - the writer's shell.
//
// The header on every page asks this one question, so the properties that
// matter are about trust and about privacy:
//
//   - The badge counts drafts WITH WORDS IN THEM. An empty chapter somebody
//     opened and walked away from is not work owed, and a number a writer
//     cannot clear without deleting something is a number they stop reading.
//   - Published work never counts. The badge is "what is not out yet".
//   - It describes the CALLER and nobody else. There is no id in the path, and
//     one writer's drafts never appear on another writer's desk.
//   - "วันนี้ N คำ" is what a save ADDED, not what the chapter now holds -
//     fixing a typo in a long chapter is not thousands of words written.

type deskBody struct {
	Unfinished int `json:"unfinished"`
	WordsToday int `json:"words_today"`
	Recent     []struct {
		Slug       string `json:"slug"`
		Title      string `json:"title"`
		Unfinished int    `json:"unfinished"`
	} `json:"recent"`
	Resume *struct {
		NovelSlug    string `json:"novel_slug"`
		NovelTitle   string `json:"novel_title"`
		ChapterSlug  string `json:"chapter_slug"`
		ChapterLabel string `json:"chapter_label"`
	} `json:"resume"`
}

func (e *authEnv) desk(t *testing.T, w writer) deskBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/me/desk")
	if res.status != http.StatusOK {
		t.Fatalf("desk status = %d, want 200. body: %s", res.status, res.body)
	}
	return dataOf[deskBody](t, res)
}

// A writer with nothing owes nothing, and gets an array rather than a null.
func TestDesk_ANewWriterOwesNothing(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	got := env.desk(t, w)
	if got.Unfinished != 0 {
		t.Errorf("unfinished = %d on a brand-new account", got.Unfinished)
	}
	if got.WordsToday != 0 {
		t.Errorf("words_today = %d before writing anything", got.WordsToday)
	}
	if got.Recent == nil {
		t.Error("recent must be an empty array, not null")
	}
	if got.Resume != nil {
		t.Errorf("resume = %+v with nothing ever edited", got.Resume)
	}
}

// The badge counts drafts with content, and only those.
func TestDesk_CountsOnlyDraftsWithWordsInThem(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	// One draft with prose in it - this is the one that counts.
	env.createChapter(t, w, novel.ID, map[string]any{
		"title":   "ค้างไว้ตรงนี้",
		"content": "ฝนตกตั้งแต่บ่าย และเขายังไม่กลับมา",
	})
	// One empty draft - opened, never written in.
	env.createChapter(t, w, novel.ID, map[string]any{"title": "ยังไม่ได้เขียน"})
	// One published chapter - finished work, nothing owed.
	published := env.createChapter(t, w, novel.ID, map[string]any{
		"title":   "ออกไปแล้ว",
		"content": "เธอเดินออกไปโดยไม่หันกลับมา",
	})
	env.publishChapter(t, w, novel.ID, published.ID)

	got := env.desk(t, w)
	if got.Unfinished != 1 {
		t.Fatalf("unfinished = %d, want 1 (the empty draft and the published chapter must not count)", got.Unfinished)
	}
	if len(got.Recent) != 1 {
		t.Fatalf("recent = %d fictions, want 1: %+v", len(got.Recent), got.Recent)
	}
	if got.Recent[0].Slug != novel.Slug {
		t.Errorf("recent[0].slug = %q, want %q", got.Recent[0].Slug, novel.Slug)
	}
	if got.Recent[0].Unfinished != 1 {
		t.Errorf("recent[0].unfinished = %d, want 1", got.Recent[0].Unfinished)
	}
}

// Words are counted as they are ADDED, not as they stand.
func TestDesk_CountsWordsAddedNotWordsHeld(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title":   "บทแรก",
		"content": "หนึ่ง สอง สาม สี่ ห้า",
	})
	after := env.desk(t, w)
	if after.WordsToday < 5 {
		t.Fatalf("words_today = %d after writing five words", after.WordsToday)
	}
	written := after.WordsToday

	// A save that changes almost nothing must add almost nothing - NOT the
	// whole chapter again.
	res := env.asOwner(t, w, http.MethodPatch,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID,
		map[string]any{"content": "หนึ่ง สอง สาม สี่ ห้า หก"})
	if res.status != http.StatusOK {
		t.Fatalf("patch status = %d, want 200. body: %s", res.status, res.body)
	}

	got := env.desk(t, w)
	if got.WordsToday != written+1 {
		t.Fatalf("words_today = %d after adding one word to a %d-word day, want %d",
			got.WordsToday, written, written+1)
	}

	// And cutting words never takes the day backwards.
	res = env.asOwner(t, w, http.MethodPatch,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID,
		map[string]any{"content": "หนึ่ง"})
	if res.status != http.StatusOK {
		t.Fatalf("patch status = %d, want 200. body: %s", res.status, res.body)
	}
	if trimmed := env.desk(t, w); trimmed.WordsToday != written+1 {
		t.Fatalf("words_today = %d after deleting words, want it to hold at %d",
			trimmed.WordsToday, written+1)
	}
}

// "เขียนต่อจากที่ค้าง" points at the chapter the writer actually touched last.
func TestDesk_ResumePointsAtTheLastChapterTouched(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	env.createChapter(t, w, novel.ID, map[string]any{
		"title": "ตอนเก่า", "content": "เมื่อวานนี้",
	})
	latest := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "ตอนที่กำลังเขียน", "content": "ยังไม่จบ",
	})

	got := env.desk(t, w)
	if got.Resume == nil {
		t.Fatal("resume is nil after editing two chapters")
	}
	if got.Resume.ChapterSlug != latest.Slug {
		t.Errorf("resume points at %q, want the last one touched (%q)",
			got.Resume.ChapterSlug, latest.Slug)
	}
	if got.Resume.ChapterLabel != "ตอนที่กำลังเขียน" {
		t.Errorf("chapter_label = %q, want the chapter's own title", got.Resume.ChapterLabel)
	}
	if got.Resume.NovelSlug != novel.Slug || got.Resume.NovelTitle == "" {
		t.Errorf("resume must name its fiction: %+v", got.Resume)
	}
}

// One writer's unfinished work never appears on another writer's desk.
func TestDesk_DescribesOnlyTheCaller(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	mine := env.newWriter(t)
	theirs := env.newWriter(t)

	novel := env.publishedNovel(t, theirs, nil)
	env.createChapter(t, theirs, novel.ID, map[string]any{
		"title": "ร่างของคนอื่น", "content": "เนื้อหาที่ยังไม่เผยแพร่",
	})

	got := env.desk(t, mine)
	if got.Unfinished != 0 {
		t.Errorf("unfinished = %d - another writer's drafts leaked onto this desk", got.Unfinished)
	}
	if len(got.Recent) != 0 {
		t.Errorf("recent = %+v - another writer's fictions leaked onto this desk", got.Recent)
	}
	if got.Resume != nil {
		t.Errorf("resume = %+v - it pointed into someone else's manuscript", got.Resume)
	}
}

// A guest has no desk to describe.
func TestDesk_RefusesAGuest(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	res := env.asGuest(t, http.MethodGet, "/api/v1/me/desk")
	if res.status != http.StatusUnauthorized {
		t.Fatalf("guest desk status = %d, want 401. body: %s", res.status, res.body)
	}
}
