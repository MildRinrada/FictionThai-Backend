package integration

import (
	"net/http"
	"testing"
)

// The chapter list's studio round (13X): each row states its quantity in the
// active mode's own unit (words / messages / entries), and the writer can
// rearrange the shelf - a reorder renumbers to 1..N atomically, refuses a
// partial order, and stays behind the fiction's editor gate.

// chapterRow decodes one list summary, including the 13X count fields.
type chapterRow struct {
	ID            string  `json:"id"`
	ChapterNumber int     `json:"chapter_number"`
	Title         *string `json:"title"`
	Status        string  `json:"status"`
	WordCount     int     `json:"word_count"`
	ContentReady  bool    `json:"content_ready"`
	ActiveFormat  string  `json:"active_format"`
	MessageCount  int     `json:"message_count"`
	EntryCount    int     `json:"entry_count"`
}

func (e *authEnv) listChapterRows(t *testing.T, w writer, novelID string) map[string]chapterRow {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/novels/"+novelID+"/chapters")
	if res.status != http.StatusOK {
		t.Fatalf("list chapters status = %d. body: %s", res.status, res.body)
	}
	rows := dataOf[[]chapterRow](t, res)
	byID := map[string]chapterRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	return byID
}

func TestChapters_ListStatesQuantityInTheModesOwnUnit(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Counts "),
		map[string]any{"mixed_formats": true}))

	prose := env.createChapter(t, w, novel.ID, map[string]any{
		"content": "ฝนตกทั้งคืน แต่เช้านี้ฟ้าเปิดแล้ว",
	})
	chat := env.createChapter(t, w, novel.ID, map[string]any{
		"presentation_format": "chat",
		"messages":            chatMessages(),
	})
	topic := env.createChapter(t, w, novel.ID, map[string]any{
		"presentation_format": "headcanon",
		"entries":             headcanonEntries(),
	})
	empty := env.createChapter(t, w, novel.ID, map[string]any{})

	rows := env.listChapterRows(t, w, novel.ID)

	if row := rows[prose.ID]; row.WordCount == 0 || row.MessageCount != 0 || row.EntryCount != 0 {
		t.Fatalf("prose row counts = %+v", row)
	}
	if row := rows[chat.ID]; row.MessageCount != len(chatMessages()) || !row.ContentReady {
		t.Fatalf("chat row = %+v, want %d messages", row, len(chatMessages()))
	}
	if row := rows[topic.ID]; row.EntryCount != len(headcanonEntries()) || !row.ContentReady {
		t.Fatalf("headcanon row = %+v, want %d entries", row, len(headcanonEntries()))
	}
	if row := rows[empty.ID]; row.ContentReady || row.WordCount != 0 ||
		row.MessageCount != 0 || row.EntryCount != 0 {
		t.Fatalf("empty row = %+v, want nothing anywhere", row)
	}
}

func TestChapters_ReorderRenumbersInTheGivenOrder(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Reorder "), nil))
	path := "/api/v1/novels/" + novel.ID + "/chapters"

	first := env.createChapter(t, w, novel.ID, map[string]any{"content": "หนึ่ง"})
	second := env.createChapter(t, w, novel.ID, map[string]any{"content": "สอง"})
	third := env.createChapter(t, w, novel.ID, map[string]any{"content": "สาม"})
	env.publishChapter(t, w, novel.ID, first.ID)

	// A partial order is refused, not half-applied.
	res := env.asOwner(t, w, http.MethodPut, path+"/order",
		map[string]any{"chapter_ids": []string{third.ID, first.ID}})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("partial order status = %d, want 422. body: %s", res.status, res.body)
	}
	// So is one that names a chapter twice.
	res = env.asOwner(t, w, http.MethodPut, path+"/order",
		map[string]any{"chapter_ids": []string{third.ID, third.ID, first.ID}})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate order status = %d, want 422. body: %s", res.status, res.body)
	}

	// The real move: 3-1-2. Numbers become 1..N in exactly that order.
	res = env.asOwner(t, w, http.MethodPut, path+"/order",
		map[string]any{"chapter_ids": []string{third.ID, first.ID, second.ID}})
	if res.status != http.StatusOK {
		t.Fatalf("reorder status = %d. body: %s", res.status, res.body)
	}
	rows := env.listChapterRows(t, w, novel.ID)
	if rows[third.ID].ChapterNumber != 1 || rows[first.ID].ChapterNumber != 2 ||
		rows[second.ID].ChapterNumber != 3 {
		t.Fatalf("numbers after reorder = %d/%d/%d, want 1/2/3",
			rows[third.ID].ChapterNumber, rows[first.ID].ChapterNumber,
			rows[second.ID].ChapterNumber)
	}
	// Rearranging the shelf publishes and unpublishes nothing.
	if rows[first.ID].Status != "published" || rows[third.ID].Status != "draft" {
		t.Fatalf("statuses after reorder = %s/%s", rows[first.ID].Status, rows[third.ID].Status)
	}

	// A stranger gets the private fiction's 404; a guest is refused outright.
	stranger := writer{webSession: env.registerWeb(t)}
	res = env.asOwner(t, stranger, http.MethodPut, path+"/order",
		map[string]any{"chapter_ids": []string{third.ID, first.ID, second.ID}})
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger reorder status = %d, want 404", res.status)
	}
	if res := env.asGuest(t, http.MethodPut, path+"/order",
		map[string]any{"chapter_ids": []string{first.ID}}); res.status != http.StatusUnauthorized {
		t.Fatalf("guest reorder status = %d, want 401", res.status)
	}
}
