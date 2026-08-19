package integration

import (
	"net/http"
	"testing"
)

// Revision history (chat-editor review 2026-08, item 10): every content save
// has always recorded a snapshot; these tests cover the endpoints that make
// them reachable - the list, the restore, and the guarantee that a restore is
// itself just another save that can be restored away.

type revisionRow struct {
	Version      int     `json:"version"`
	Title        *string `json:"title"`
	WordCount    int     `json:"word_count"`
	MessageCount int     `json:"message_count"`
	EntryCount   int     `json:"entry_count"`
	CreatedAt    string  `json:"created_at"`
}

func TestRevisions_ListAndRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Revisions "), nil))
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"content": "ฉบับแรก ฝนตกทั้งคืน",
	})
	base := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	// Two edits -> two snapshots (the create itself records none).
	for _, text := range []string{"ฉบับสอง ฟ้าเปิดแล้ว", "ฉบับสาม แดดออกแล้ว"} {
		res := env.asOwner(t, w, http.MethodPatch, base, map[string]any{"content": text})
		if res.status != http.StatusOK {
			t.Fatalf("edit status = %d. body: %s", res.status, res.body)
		}
	}

	res := env.asOwner(t, w, http.MethodGet, base+"/revisions")
	if res.status != http.StatusOK {
		t.Fatalf("list revisions status = %d. body: %s", res.status, res.body)
	}
	rows := dataOf[[]revisionRow](t, res)
	if len(rows) != 2 || rows[0].Version != 2 || rows[1].Version != 1 {
		t.Fatalf("revisions = %+v, want versions [2 1]", rows)
	}

	// Restore version 1: the first manuscript comes back...
	res = env.asOwner(t, w, http.MethodPost, base+"/revisions/1/restore")
	if res.status != http.StatusOK {
		t.Fatalf("restore status = %d. body: %s", res.status, res.body)
	}
	restored := dataOf[chapterBody](t, res)
	if restored.Content == nil || *restored.Content != "ฉบับแรก ฝนตกทั้งคืน" {
		t.Fatalf("restored content = %v, want the first manuscript", restored.Content)
	}

	// ...and the state it displaced became version 3, so the restore itself
	// can be undone. Nothing was destroyed.
	res = env.asOwner(t, w, http.MethodGet, base+"/revisions")
	rows = dataOf[[]revisionRow](t, res)
	if len(rows) != 3 || rows[0].Version != 3 {
		t.Fatalf("after restore revisions = %+v, want three with version 3 newest", rows)
	}
}

func TestRevisions_RestoreRecoversAConversation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "ChatRevisions "),
		map[string]any{"presentation_format": "chat"}))
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"presentation_format": "chat",
		"messages":            chatMessages(),
	})
	base := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	// The writer wipes the conversation in an edit...
	res := env.asOwner(t, w, http.MethodPatch, base, map[string]any{
		"messages": []map[string]any{},
	})
	if res.status != http.StatusOK {
		t.Fatalf("wipe status = %d. body: %s", res.status, res.body)
	}

	// ...and version 1 brings every bubble back.
	res = env.asOwner(t, w, http.MethodPost, base+"/revisions/1/restore")
	if res.status != http.StatusOK {
		t.Fatalf("restore status = %d. body: %s", res.status, res.body)
	}
	restored := dataOf[chapterBody](t, res)
	if len(restored.Messages) != len(chatMessages()) {
		t.Fatalf("restored %d messages, want %d", len(restored.Messages), len(chatMessages()))
	}
}

func TestRevisions_AreInvisibleToNonEditors(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "PrivateHistory "), nil))
	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": "ต้นฉบับ"})
	env.publishChapter(t, w, novel.ID, chapter.ID)
	base := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	stranger := env.newWriter(t)
	res := env.asOwner(t, stranger, http.MethodGet, base+"/revisions")
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger list status = %d, want 404", res.status)
	}
	res = env.asOwner(t, stranger, http.MethodPost, base+"/revisions/1/restore")
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger restore status = %d, want 404", res.status)
	}
}

// --- character chat preferences (chat-editor review items 1-2) --------------

type chatPrefsBody struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	ChatColor       *string `json:"chat_color"`
	ChatSide        *string `json:"chat_side"`
	ChatDisplayName *string `json:"chat_display_name"`
}

func TestCharacters_ChatPrefsPersistAndValidate(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "ChatPrefs "), nil))
	member := env.createCharacter(t, w, novel.ID, "จงหลี่ (Zhongli)")
	path := "/api/v1/novels/" + novel.ID + "/characters/" + member.ID

	// The composer's popover saves colour, side, and display name in one PATCH.
	res := env.asOwner(t, w, http.MethodPatch, path, map[string]any{
		"chat_color":        "#8B5CF6",
		"chat_side":         "right",
		"chat_display_name": " จงหลี่ ",
	})
	if res.status != http.StatusOK {
		t.Fatalf("set prefs status = %d. body: %s", res.status, res.body)
	}
	got := dataOf[chatPrefsBody](t, res)
	if got.ChatColor == nil || *got.ChatColor != "#8b5cf6" {
		t.Fatalf("chat_color = %v, want #8b5cf6 (normalised lower)", got.ChatColor)
	}
	if got.ChatSide == nil || *got.ChatSide != "right" {
		t.Fatalf("chat_side = %v, want right", got.ChatSide)
	}
	if got.ChatDisplayName == nil || *got.ChatDisplayName != "จงหลี่" {
		t.Fatalf("chat_display_name = %v, want trimmed จงหลี่", got.ChatDisplayName)
	}

	// Junk is refused before it can reach a style attribute.
	for _, bad := range []map[string]any{
		{"chat_color": "red"},
		{"chat_color": "#12345"},
		{"chat_color": "url(javascript:1)"},
		{"chat_side": "middle"},
	} {
		res = env.asOwner(t, w, http.MethodPatch, path, bad)
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("bad prefs %v status = %d, want 422. body: %s", bad, res.status, res.body)
		}
	}

	// null clears back to the composer's defaults.
	res = env.asOwner(t, w, http.MethodPatch, path, map[string]any{
		"chat_color": nil, "chat_side": nil, "chat_display_name": nil,
	})
	if res.status != http.StatusOK {
		t.Fatalf("clear prefs status = %d. body: %s", res.status, res.body)
	}
	got = dataOf[chatPrefsBody](t, res)
	if got.ChatColor != nil || got.ChatSide != nil || got.ChatDisplayName != nil {
		t.Fatalf("cleared prefs = %+v, want all nil", got)
	}
}
