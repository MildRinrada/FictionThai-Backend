package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The library redesign (library review 2026-08): continue-reading counts, the
// private finished mark, reading history with its privacy switch, and the
// per-follow notification toggle.

type continueEntryBody struct {
	Novel struct {
		ID     string `json:"id"`
		Slug   string `json:"slug"`
		Status string `json:"status"`
	} `json:"novel"`
	Chapter *struct {
		ID            string `json:"id"`
		ChapterNumber int    `json:"chapter_number"`
	} `json:"chapter"`
	ProgressPercent float64 `json:"progress_percent"`
	TotalChapters   int     `json:"total_chapters"`
	ChaptersLeft    int     `json:"chapters_left"`
	NewSinceRead    int     `json:"new_since_read"`
}

func (e *authEnv) saveProgress(t *testing.T, w writer, novelSlug, chapterID string, percent float64) {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPut, "/api/v1/novels/"+novelSlug+"/progress",
		map[string]any{"chapter_id": chapterID, "progress_percent": percent})
	if res.status != http.StatusOK {
		t.Fatalf("save progress status = %d. body: %s", res.status, res.body)
	}
}

func TestLibrary_ContinueReadingCarriesTheCounts(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	chapters := make([]chapterBody, 0, 3)
	for _, text := range []string{"หนึ่ง", "สอง", "สาม"} {
		chapter := env.createChapter(t, author, novel.ID, map[string]any{"content": text})
		env.publishChapter(t, author, novel.ID, chapter.ID)
		chapters = append(chapters, chapter)
	}

	reader := env.newUnverifiedWriter(t)
	env.saveProgress(t, reader, novel.Slug, chapters[0].ID, 80)

	res := env.asOwner(t, reader, http.MethodGet, "/api/v1/me/reading-progress")
	if res.status != http.StatusOK {
		t.Fatalf("continue reading status = %d. body: %s", res.status, res.body)
	}
	items, _ := collectionOf[continueEntryBody](t, res)
	if len(items) != 1 {
		t.Fatalf("continue reading = %d entries, want 1", len(items))
	}
	entry := items[0]
	// Three live chapters, two after the stopped-at first one, and none
	// published SINCE the save (they all existed before the reader stopped).
	if entry.TotalChapters != 3 || entry.ChaptersLeft != 2 || entry.NewSinceRead != 0 {
		t.Fatalf("counts = total %d left %d new %d, want 3/2/0",
			entry.TotalChapters, entry.ChaptersLeft, entry.NewSinceRead)
	}

	// A chapter published AFTER the reader last read is the badge that
	// brings them back.
	late := env.createChapter(t, author, novel.ID, map[string]any{"content": "สี่"})
	env.publishChapter(t, author, novel.ID, late.ID)

	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/me/reading-progress")
	items, _ = collectionOf[continueEntryBody](t, res)
	entry = items[0]
	if entry.TotalChapters != 4 || entry.ChaptersLeft != 3 || entry.NewSinceRead != 1 {
		t.Fatalf("after publish counts = total %d left %d new %d, want 4/3/1",
			entry.TotalChapters, entry.ChaptersLeft, entry.NewSinceRead)
	}
}

func TestLibrary_RemoveFromContinueReading(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{"content": "เนื้อหา"})
	env.publishChapter(t, author, novel.ID, chapter.ID)

	reader := env.newUnverifiedWriter(t)
	env.saveProgress(t, reader, novel.Slug, chapter.ID, 40)

	res := env.asOwner(t, reader, http.MethodDelete, "/api/v1/novels/"+novel.Slug+"/progress")
	if res.status != http.StatusNoContent {
		t.Fatalf("delete progress status = %d. body: %s", res.status, res.body)
	}

	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/me/reading-progress")
	items, _ := collectionOf[continueEntryBody](t, res)
	if len(items) != 0 {
		t.Fatalf("continue reading after removal = %d entries, want 0", len(items))
	}
}

type finishedBody struct {
	Novel struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"novel"`
	FinishedAt time.Time `json:"finished_at"`
	Stars      *int      `json:"stars"`
	Note       *string   `json:"note"`
}

func TestLibrary_FinishedMarksLifecycle(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	reader := env.newUnverifiedWriter(t)
	path := "/api/v1/novels/" + novel.Slug + "/finished"

	// Stars out of range are refused; the note is private and trimmed.
	res := env.asOwner(t, reader, http.MethodPut, path, map[string]any{"stars": 9})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("bad stars status = %d, want 422. body: %s", res.status, res.body)
	}

	res = env.asOwner(t, reader, http.MethodPut, path,
		map[string]any{"stars": 5, "note": "  อ่านถึงตอนที่พระเอกหาย  "})
	if res.status != http.StatusNoContent {
		t.Fatalf("mark finished status = %d. body: %s", res.status, res.body)
	}

	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/me/finished")
	if res.status != http.StatusOK {
		t.Fatalf("list finished status = %d. body: %s", res.status, res.body)
	}
	items, _ := collectionOf[finishedBody](t, res)
	if len(items) != 1 || items[0].Stars == nil || *items[0].Stars != 5 ||
		items[0].Note == nil || *items[0].Note != "อ่านถึงตอนที่พระเอกหาย" {
		t.Fatalf("finished list = %+v, want one 5-star trimmed-note entry", items)
	}

	// Editing keeps the original finished date; unmarking removes the entry.
	res = env.asOwner(t, reader, http.MethodPut, path, map[string]any{"stars": 4})
	if res.status != http.StatusNoContent {
		t.Fatalf("edit mark status = %d. body: %s", res.status, res.body)
	}
	res = env.asOwner(t, reader, http.MethodDelete, path)
	if res.status != http.StatusNoContent {
		t.Fatalf("unmark status = %d. body: %s", res.status, res.body)
	}
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/me/finished")
	items, _ = collectionOf[finishedBody](t, res)
	if len(items) != 0 {
		t.Fatalf("finished after unmark = %d entries, want 0", len(items))
	}
}

type historyBody struct {
	Novel struct {
		ID string `json:"id"`
	} `json:"novel"`
	Chapter *struct {
		ChapterNumber int `json:"chapter_number"`
	} `json:"chapter"`
	ReadAt time.Time `json:"read_at"`
}

func TestLibrary_HistoryRecordsRespectsOptOutAndClears(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	first := env.createChapter(t, author, novel.ID, map[string]any{"content": "หนึ่ง"})
	second := env.createChapter(t, author, novel.ID, map[string]any{"content": "สอง"})
	env.publishChapter(t, author, novel.ID, first.ID)
	env.publishChapter(t, author, novel.ID, second.ID)

	reader := env.newUnverifiedWriter(t)

	// A save records a history row.
	env.saveProgress(t, reader, novel.Slug, first.ID, 50)
	res := env.asOwner(t, reader, http.MethodGet, "/api/v1/me/history")
	if res.status != http.StatusOK {
		t.Fatalf("history status = %d. body: %s", res.status, res.body)
	}
	items, _ := collectionOf[historyBody](t, res)
	if len(items) != 1 {
		t.Fatalf("history = %d entries, want 1", len(items))
	}

	// Recording off: the switch answers, and the NEXT save leaves no trace.
	res = env.asOwner(t, reader, http.MethodPut, "/api/v1/me/history/settings",
		map[string]any{"record_history": false})
	if res.status != http.StatusOK {
		t.Fatalf("set history settings status = %d. body: %s", res.status, res.body)
	}
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/me/history/settings")
	if got := string(res.body); res.status != http.StatusOK ||
		!strings.Contains(got, `"record_history":false`) {
		t.Fatalf("history settings = %d %s, want record_history false", res.status, got)
	}

	env.saveProgress(t, reader, novel.Slug, second.ID, 10)
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/me/history")
	items, _ = collectionOf[historyBody](t, res)
	if len(items) != 1 {
		t.Fatalf("history after opt-out save = %d entries, want still 1", len(items))
	}

	// ล้างประวัติ erases everything, and stays erased.
	res = env.asOwner(t, reader, http.MethodDelete, "/api/v1/me/history")
	if res.status != http.StatusNoContent {
		t.Fatalf("clear history status = %d. body: %s", res.status, res.body)
	}
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/me/history")
	items, _ = collectionOf[historyBody](t, res)
	if len(items) != 0 {
		t.Fatalf("history after clear = %d entries, want 0", len(items))
	}
}

type followingBody struct {
	Author struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
	LastPublishedAt   *time.Time `json:"last_published_at"`
	WritingCount      int        `json:"writing_count"`
	NotifyNewChapters bool       `json:"notify_new_chapters"`
}

func TestLibrary_FollowingCarriesActivityAndNotifyToggleSilencesFanout(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	muted := env.newUnverifiedWriter(t)
	loud := env.newUnverifiedWriter(t)
	followPath := "/api/v1/users/" + author.userID + "/follow"
	for _, follower := range []writer{muted, loud} {
		res := env.asOwner(t, follower, http.MethodPost, followPath)
		if res.status != http.StatusNoContent {
			t.Fatalf("follow status = %d. body: %s", res.status, res.body)
		}
	}

	// The default switch is on; no chapter exists yet, so no activity date.
	res := env.asOwner(t, muted, http.MethodGet, "/api/v1/me/following")
	entries, _ := collectionOf[followingBody](t, res)
	if len(entries) != 1 || !entries[0].NotifyNewChapters {
		t.Fatalf("following = %+v, want one entry with notify on", entries)
	}
	if entries[0].LastPublishedAt != nil {
		t.Fatalf("last_published_at = %v before any chapter, want nil", entries[0].LastPublishedAt)
	}

	// One reader turns this author's alerts off...
	res = env.asOwner(t, muted, http.MethodPatch, followPath,
		map[string]any{"notify_new_chapters": false})
	if res.status != http.StatusNoContent {
		t.Fatalf("set notify status = %d. body: %s", res.status, res.body)
	}

	// ...the author publishes...
	chapter := env.createChapter(t, author, novel.ID, map[string]any{"content": "ตอนใหม่"})
	env.publishChapter(t, author, novel.ID, chapter.ID)

	// ...the follower who kept alerts on hears about it; the muted one never
	// does. loud's arrival proves the fan-out RAN, so muted's silence is the
	// filter, not timing.
	env.awaitNotifications(t, loud, 1)
	res = env.asOwner(t, muted, http.MethodGet, "/api/v1/me/notifications")
	items, _ := collectionOf[notificationBody](t, res)
	for _, item := range items {
		if item.Type == "novel_update" {
			t.Fatalf("muted follower still received a chapter notification: %+v", items)
		}
	}

	// ...and the published chapter now shows as the author's last activity,
	// with notify remembered as off.
	res = env.asOwner(t, muted, http.MethodGet, "/api/v1/me/following")
	entries, _ = collectionOf[followingBody](t, res)
	if entries[0].LastPublishedAt == nil || entries[0].NotifyNewChapters {
		t.Fatalf("following after publish = %+v, want activity date and notify off", entries[0])
	}
}
