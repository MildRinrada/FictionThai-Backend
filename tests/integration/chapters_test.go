package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Creation, ordering, and content
// ---------------------------------------------------------------------------

func TestChapters_CreateStandardProse(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title":   "บทที่ 1",
		"content": "ฝนหยุดตกแล้ว\n\nเธอเดินออกไปจากบ้าน",
	})

	if chapter.ChapterNumber != 1 {
		t.Errorf("chapter_number = %d, want 1", chapter.ChapterNumber)
	}
	if chapter.Status != "draft" {
		t.Errorf("status = %q; a new chapter must start as a draft (docs/11 §31)", chapter.Status)
	}
	if chapter.Content == nil || !strings.Contains(*chapter.Content, "ฝนหยุดตกแล้ว") {
		t.Error("the prose was not stored")
	}
	if !chapter.ContentReady {
		t.Error("a standard chapter with prose should report content_ready")
	}
	if chapter.Slug == "" {
		t.Error("a chapter needs a slug for its reader URL (docs/08 §35)")
	}
}

// docs/09 §16: the API must support the structured message representation
// rather than forcing chat fiction into ordinary prose.
func TestChapters_CreateChatMessages(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "chat"})

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title":    "บทที่ 1",
		"messages": chatMessages(),
	})

	if len(chapter.Messages) != 4 {
		t.Fatalf("messages = %d, want 4. body: %+v", len(chapter.Messages), chapter.Messages)
	}
	// docs/15 §5.4: message order and speaker identity must be preserved.
	for i, message := range chapter.Messages {
		if message.Position != i {
			t.Errorf("message %d has position %d; positions must be dense and ordered", i, message.Position)
		}
	}
	if chapter.Messages[0].SpeakerName != "Alice" || chapter.Messages[1].SpeakerName != "Bob" {
		t.Error("speaker identity was not preserved")
	}
	if chapter.Messages[2].MessageType != "separator" {
		t.Errorf("message type = %q, want separator", chapter.Messages[2].MessageType)
	}
	if chapter.Messages[0].Metadata == nil || chapter.Messages[0].Metadata.Side == nil ||
		*chapter.Messages[0].Metadata.Side != "left" {
		t.Error("the documented `side` metadata did not round-trip")
	}
	if !chapter.ContentReady {
		t.Error("a chat chapter with messages should report content_ready")
	}
}

// docs/11 §18: chat content visually resembles application UI, so a writer must
// not be able to submit application-level values through message metadata.
func TestChapters_RejectsForgedMessageMetadata(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "chat"})

	hostile := []map[string]any{
		{"is_admin": true},
		{"verified": true},
		{"system_message": true},
		{"side": "left", "is_admin": true},
		{"side": "centre"},
	}

	for _, metadata := range hostile {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/chapters",
			map[string]any{"messages": []map[string]any{{
				"speaker_name": "Alice",
				"message_type": "message",
				"content":      "hello",
				"metadata":     metadata,
			}}})

		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("metadata %v was accepted with status %d; only documented keys are allowed",
				metadata, res.status)
		}
	}
}

// docs/15 §5.3: chapter order must be correct and must not depend on insertion
// order or on row identity.
func TestChapters_OrderingIsDeterministic(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	for i := 1; i <= 5; i++ {
		env.createChapter(t, w, novel.ID, map[string]any{"title": "Chapter " + string(rune('0'+i))})
	}

	// Edit an early chapter: with a physical-order default this would move it to
	// the end of the result.
	first := env.chapterList(t, w, novel.ID)[0]
	env.asOwner(t, w, http.MethodPatch,
		"/api/v1/novels/"+novel.ID+"/chapters/"+first.ID,
		map[string]any{"content": "Edited after the others were written."})

	summaries := env.chapterList(t, w, novel.ID)
	if len(summaries) != 5 {
		t.Fatalf("chapters = %d, want 5", len(summaries))
	}
	for i, summary := range summaries {
		if summary.ChapterNumber != i+1 {
			t.Errorf("position %d holds chapter_number %d; ordering is not by chapter number",
				i, summary.ChapterNumber)
		}
	}
}

// ---------------------------------------------------------------------------
// Visibility
// ---------------------------------------------------------------------------

// docs/11 §21: a public fiction does not make its unpublished chapters public,
// even to someone who knows the chapter id.
func TestChapters_DraftChaptersAreInvisibleToReaders(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	stranger := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	published := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "Chapter One", "content": "Readable.",
	})
	env.publishChapter(t, w, novel.ID, published.ID)

	draft := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "Chapter Two", "content": "Not ready yet.",
	})

	t.Run("the chapter list hides drafts", func(t *testing.T) {
		for name, res := range map[string]apiResponse{
			"guest":    env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID+"/chapters"),
			"stranger": env.asOwner(t, stranger, http.MethodGet, "/api/v1/novels/"+novel.ID+"/chapters"),
		} {
			summaries, _ := collectionOf[chapterBody](t, res)
			if len(summaries) != 1 {
				t.Errorf("%s sees %d chapters, want 1", name, len(summaries))
			}
			for _, summary := range summaries {
				if summary.ID == draft.ID {
					t.Errorf("%s can see a draft chapter", name)
				}
			}
		}
	})

	t.Run("a known draft id is still not readable", func(t *testing.T) {
		path := "/api/v1/novels/" + novel.ID + "/chapters/" + draft.ID

		for name, res := range map[string]apiResponse{
			"guest":    env.asGuest(t, http.MethodGet, path),
			"stranger": env.asOwner(t, stranger, http.MethodGet, path),
		} {
			// 404, not 403: a 403 would confirm the chapter exists.
			if res.status != http.StatusNotFound {
				t.Errorf("%s got %d for a draft chapter, want 404. body: %s",
					name, res.status, res.body)
			}
		}
	})

	t.Run("the owner sees both", func(t *testing.T) {
		summaries := env.chapterList(t, w, novel.ID)
		if len(summaries) != 2 {
			t.Errorf("the owner sees %d chapters, want 2", len(summaries))
		}
	})
}

// A draft fiction's chapters are unreachable even when the chapter itself is
// published: the fiction gate applies first (docs/11 §21, §31).
func TestChapters_DraftFictionHidesEvenPublishedChapters(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	draft := env.createNovel(t, w, createNovelBody("Unpublished Fiction", nil))
	chapter := env.createChapter(t, w, draft.ID, map[string]any{"content": "Hidden."})
	env.publishChapter(t, w, draft.ID, chapter.ID)

	for name, path := range map[string]string{
		"chapter list": "/api/v1/novels/" + draft.ID + "/chapters",
		"chapter":      "/api/v1/novels/" + draft.ID + "/chapters/" + chapter.ID,
	} {
		t.Run(name, func(t *testing.T) {
			if res := env.asGuest(t, http.MethodGet, path); res.status != http.StatusNotFound {
				t.Errorf("status = %d, want 404", res.status)
			}
		})
	}
}

// A chapter id belonging to one fiction must not be reachable through another
// fiction's URL - the confused-deputy shape of an IDOR (docs/11 §8).
func TestChapters_CannotBeReachedThroughAnotherFiction(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	source := env.publishedNovel(t, w, nil)
	decoy := env.publishedNovel(t, w, nil)

	chapter := env.createChapter(t, w, source.ID, map[string]any{"content": "Belongs elsewhere."})
	env.publishChapter(t, w, source.ID, chapter.ID)

	res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+decoy.ID+"/chapters/"+chapter.ID)
	if res.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", res.status, res.body)
	}
}

// docs/11 §32 lists Scheduled as a visibility state. A chapter goes live when
// its time arrives, computed at read time so no worker can leave it stranded.
func TestChapters_ScheduledChapterIsNotReadableUntilItsTime(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"content":      "Coming soon.",
		"status":       "scheduled",
		"scheduled_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})

	if res := env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID); res.status != http.StatusNotFound {
		t.Errorf("a future-scheduled chapter is readable: %d", res.status)
	}

	// A schedule in the past would publish the chapter the instant it saved,
	// which is a surprising way to make work public.
	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/chapters",
		map[string]any{
			"content":      "Backdated.",
			"status":       "scheduled",
			"scheduled_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("a past schedule was accepted with status %d, want 422", res.status)
	}

	// status=scheduled with no time must be rejected.
	res = env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/chapters",
		map[string]any{"content": "No time.", "status": "scheduled"})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("a schedule with no time was accepted with status %d, want 422", res.status)
	}
}

// The OWNER's chapter list carries the schedule (§13T): "ตารางลงตอน" in the
// studio is built from the list, and a schedule the list cannot show is a
// schedule the writer cannot see. A reader's list never carries the field -
// nor the scheduled row itself.
func TestChapters_ListCarriesScheduleForOwnerOnly(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	stranger := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	when := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	scheduled := env.createChapter(t, w, novel.ID, map[string]any{
		"content":      "Queued.",
		"status":       "scheduled",
		"scheduled_at": when,
	})

	owned, _ := collectionOf[chapterBody](t,
		env.asOwner(t, w, http.MethodGet, "/api/v1/novels/"+novel.ID+"/chapters"))
	var found bool
	for _, summary := range owned {
		if summary.ID != scheduled.ID {
			continue
		}
		found = true
		if summary.ScheduledAt == nil {
			t.Error("the owner's list row has no scheduled_at")
		}
	}
	if !found {
		t.Fatal("the owner cannot see their scheduled chapter in the list")
	}

	strangers, _ := collectionOf[chapterBody](t,
		env.asOwner(t, stranger, http.MethodGet, "/api/v1/novels/"+novel.ID+"/chapters"))
	for _, summary := range strangers {
		if summary.ID == scheduled.ID {
			t.Error("a stranger sees the scheduled chapter itself")
		}
		if summary.ScheduledAt != nil {
			t.Error("a stranger's list row carries scheduled_at")
		}
	}
}

// ---------------------------------------------------------------------------
// Non-destructive format changes - docs/08 §3.1, docs/15 §5.7
// ---------------------------------------------------------------------------

// The core guarantee of this phase: changing presentation format must not
// rewrite prose into chat messages, or chat messages into prose.
func TestFormatChange_StandardToChatAndBackPreservesProseExactly(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil) // standard

	const prose = "ฝนหยุดตกแล้ว\n\nเธอเดินออกไปจากบ้าน โดยไม่หันกลับมามอง\n\nจบ"
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "บทที่ 1", "content": prose,
	})
	env.publishChapter(t, w, novel.ID, chapter.ID)

	// standard -> chat
	format := env.changeFormat(t, w, novel.ID, map[string]any{"presentation_format": "chat"})
	if format.PresentationFormat != "chat" {
		t.Fatalf("presentation_format = %q, want chat", format.PresentationFormat)
	}
	// docs/08 §11: the author is WARNED that chat content is not prepared. It is
	// a hint, never a trigger for conversion.
	if !format.NeedsChatSetup {
		t.Error("needs_chat_setup should warn that no chat content is prepared")
	}

	// The prose is still there, byte for byte, and the owner can see it.
	owned := env.chapter(t, w, novel.ID, chapter.ID)
	if owned.Content == nil || *owned.Content != prose {
		t.Fatalf("prose was altered by the format change.\n got: %q\nwant: %q",
			derefString(owned.Content), prose)
	}
	if owned.HasStandardContent == nil || !*owned.HasStandardContent {
		t.Error("has_standard_content must tell the writer their prose survived")
	}
	if owned.HasChatContent == nil || *owned.HasChatContent {
		t.Error("no chat content was authored, so has_chat_content must be false")
	}
	// Nothing was invented: the platform did not generate messages from prose.
	if len(owned.Messages) != 0 {
		t.Errorf("%d messages appeared from nowhere; prose must not be auto-converted",
			len(owned.Messages))
	}
	// The CHAPTER keeps the mode it was created in (13P), so a reader still gets
	// the prose. needs_chat_setup above is the FICTION-level warning that new
	// chapters will start as chat with nothing prepared; it is not a claim that
	// this one moved.
	read := env.guestChapter(t, novel.ID, chapter.ID)
	if read.ActiveFormat != "standard" {
		t.Errorf("the chapter followed the fiction to %q; it is stamped (13P)",
			read.ActiveFormat)
	}
	if !read.ContentReady {
		t.Error("the chapter is prose and has prose, so content_ready must stay true")
	}

	// chat -> standard, back to where we started.
	env.changeFormat(t, w, novel.ID, map[string]any{"presentation_format": "standard"})

	restored := env.guestChapter(t, novel.ID, chapter.ID)
	if restored.Content == nil || *restored.Content != prose {
		t.Fatalf("the round trip did not restore the prose.\n got: %q\nwant: %q",
			derefString(restored.Content), prose)
	}
	if !restored.ContentReady {
		t.Error("the prose is readable again, so content_ready should be true")
	}
}

// The mirror case: switching away from chat must not destroy the conversation.
func TestFormatChange_ChatToStandardPreservesMessages(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "chat"})

	chapter := env.createChapter(t, w, novel.ID, map[string]any{"messages": chatMessages()})
	env.publishChapter(t, w, novel.ID, chapter.ID)

	env.changeFormat(t, w, novel.ID, map[string]any{"presentation_format": "standard"})

	owned := env.chapter(t, w, novel.ID, chapter.ID)
	if len(owned.Messages) != 4 {
		t.Fatalf("messages = %d, want 4; the conversation was destroyed", len(owned.Messages))
	}
	if owned.Messages[0].SpeakerName != "Alice" || owned.Messages[0].Content != "นายอยู่ไหน?" {
		t.Error("message content was altered by the format change")
	}
	if owned.HasChatContent == nil || !*owned.HasChatContent {
		t.Error("has_chat_content must tell the writer their conversation survived")
	}
	// Nothing was flattened into the prose column.
	if owned.Content != nil {
		t.Errorf("content = %q; messages must not be auto-converted into prose",
			derefString(owned.Content))
	}

	// And back again.
	env.changeFormat(t, w, novel.ID, map[string]any{"presentation_format": "chat"})
	restored := env.guestChapter(t, novel.ID, chapter.ID)
	if len(restored.Messages) != 4 {
		t.Fatalf("messages = %d after the round trip, want 4", len(restored.Messages))
	}
}

// docs/15 §5.7 and §5.8: a chapter holding BOTH representations must keep both
// through every transition.
func TestFormatChange_KeepsBothRepresentationsThroughEveryTransition(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	const prose = "The rain had already stopped."
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"content":  prose,
		"messages": chatMessages(),
	})

	transitions := []map[string]any{
		{"presentation_format": "chat"},
		{"content_mode": "headcanon"},
		{"story_structure": "one_shot"},
		{"presentation_format": "standard"},
		{"story_structure": "multi_chapter"},
		{"content_mode": "general"},
	}

	for _, patch := range transitions {
		env.changeFormat(t, w, novel.ID, patch)

		owned := env.chapter(t, w, novel.ID, chapter.ID)
		if owned.Content == nil || *owned.Content != prose {
			t.Fatalf("after %v the prose was lost: %q", patch, derefString(owned.Content))
		}
		if len(owned.Messages) != 4 {
			t.Fatalf("after %v the conversation was lost: %d messages", patch, len(owned.Messages))
		}
	}
}

// docs/15 §5.7: story structure changes navigation, not storage. A writer who
// switches multi_chapter -> one_shot must not have chapters merged or deleted.
func TestFormatChange_StructureChangeKeepsEveryChapter(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	for i := 0; i < 3; i++ {
		chapter := env.createChapter(t, w, novel.ID, map[string]any{
			"content": "Chapter body " + string(rune('A'+i)),
		})
		env.publishChapter(t, w, novel.ID, chapter.ID)
	}

	format := env.changeFormat(t, w, novel.ID, map[string]any{"story_structure": "one_shot"})
	if format.StoryStructure != "one_shot" {
		t.Fatalf("story_structure = %q, want one_shot", format.StoryStructure)
	}

	summaries := env.chapterList(t, w, novel.ID)
	if len(summaries) != 3 {
		t.Fatalf("chapters = %d after switching to one_shot, want 3", len(summaries))
	}

	// The reader stops being offered chapter navigation; the content stays.
	read := dataOf[novelBody](t, env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID))
	if read.UsesChapterNavigation {
		t.Error("a one-shot must not advertise chapter navigation (docs/15 §5.2)")
	}
	if read.ChapterCount != 3 {
		t.Errorf("chapter_count = %d, want 3", read.ChapterCount)
	}
}

// docs/11 §31: changing format must not change visibility, and must not publish
// a draft.
func TestFormatChange_DoesNotAlterVisibilityOrPublicationState(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	draft := env.createNovel(t, w, createNovelBody("Private Standard Fiction", nil))

	env.changeFormat(t, w, draft.ID, map[string]any{
		"presentation_format": "chat", "content_mode": "headcanon",
	})

	after := dataOf[novelBody](t, env.asOwner(t, w, http.MethodGet, "/api/v1/novels/"+draft.ID))
	if after.Status != "draft" {
		t.Errorf("status = %q, want draft", after.Status)
	}
	if after.Visibility == nil || *after.Visibility != "private" {
		t.Error("the format change altered visibility")
	}
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+draft.Slug); res.status != http.StatusNotFound {
		t.Errorf("the draft became publicly reachable after a format change: %d", res.status)
	}
}

// docs/09 §14.7: a PATCH must not silently reset the dimensions it does not
// mention.
func TestFormatChange_LeavesUnmentionedDimensionsAlone(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, map[string]any{
		"story_structure":     "one_shot",
		"presentation_format": "chat",
		"content_mode":        "headcanon",
	})

	format := env.changeFormat(t, w, novel.ID, map[string]any{"content_mode": "general"})

	if format.ContentMode != "general" {
		t.Errorf("content_mode = %q, want general", format.ContentMode)
	}
	if format.StoryStructure != "one_shot" {
		t.Errorf("story_structure = %q; an unmentioned dimension was reset", format.StoryStructure)
	}
	if format.PresentationFormat != "chat" {
		t.Errorf("presentation_format = %q; an unmentioned dimension was reset", format.PresentationFormat)
	}
}

// docs/09 §33: repeating the same format state must be safe and must not
// duplicate content or create chapters.
func TestFormatChange_IsIdempotent(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": "Body."})

	for i := 0; i < 3; i++ {
		env.changeFormat(t, w, novel.ID, map[string]any{"presentation_format": "chat"})
	}

	summaries := env.chapterList(t, w, novel.ID)
	if len(summaries) != 1 {
		t.Errorf("chapters = %d after repeating the same format change, want 1", len(summaries))
	}
	owned := env.chapter(t, w, novel.ID, chapter.ID)
	if owned.Content == nil || *owned.Content != "Body." {
		t.Error("repeating the format change altered the content")
	}
}

func TestFormatChange_RejectsUnsupportedValuesAndEmptyPatches(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	for name, body := range map[string]map[string]any{
		"unknown presentation": {"presentation_format": "script"},
		"unknown structure":    {"story_structure": "trilogy"},
		"unknown mode":         {"content_mode": "fanfic"},
		"nothing at all":       {},
	} {
		t.Run(name, func(t *testing.T) {
			res := env.asOwner(t, w, http.MethodPatch,
				"/api/v1/novels/"+novel.ID+"/format", body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422. body: %s", res.status, res.body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Revisions - docs/08 §12, docs/CONTENT-MODEL.md §5
// ---------------------------------------------------------------------------

func TestChapters_ContentEditsRecordARevision(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "chat"})

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "Original", "content": "First draft.", "messages": chatMessages(),
	})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	if got := env.revisionCount(t, chapter.ID); got != 0 {
		t.Errorf("a new chapter has %d revisions, want 0", got)
	}

	// A content edit records what it replaces.
	env.asOwner(t, w, http.MethodPatch, path, map[string]any{"content": "Second draft."})
	if got := env.revisionCount(t, chapter.ID); got != 1 {
		t.Fatalf("revisions = %d after a content edit, want 1", got)
	}

	// The snapshot holds BOTH representations, so restoring it would restore the
	// complete authored state (docs/CONTENT-MODEL.md §5).
	var (
		content  *string
		messages *string
		version  int
	)
	err := env.db.QueryRowContext(t.Context(), `
		SELECT content, messages::text, version
		FROM chapter_revisions WHERE chapter_id = $1 ORDER BY version DESC LIMIT 1`,
		chapter.ID).Scan(&content, &messages, &version)
	if err != nil {
		t.Fatalf("read revision: %v", err)
	}
	if content == nil || *content != "First draft." {
		t.Errorf("the revision holds %q, want the replaced prose", derefString(content))
	}
	if messages == nil || !strings.Contains(*messages, "Alice") {
		t.Error("the revision did not snapshot the chat messages")
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}

	// A metadata-only change records nothing: there is no content difference.
	env.asOwner(t, w, http.MethodPost, path+"/publish", nil)
	if got := env.revisionCount(t, chapter.ID); got != 1 {
		t.Errorf("revisions = %d after publishing, want 1 - publishing changes no content", got)
	}

	// Versions increment.
	env.asOwner(t, w, http.MethodPatch, path, map[string]any{"title": "Renamed"})
	if got := env.revisionCount(t, chapter.ID); got != 2 {
		t.Errorf("revisions = %d after a title edit, want 2", got)
	}
}

// Replacing the message list is an author edit, and the previous conversation
// must survive in a revision (docs/08 §3.2).
func TestChapters_ReplacingMessagesPreservesThePreviousConversation(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "chat"})

	chapter := env.createChapter(t, w, novel.ID, map[string]any{"messages": chatMessages()})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	res := env.asOwner(t, w, http.MethodPatch, path, map[string]any{
		"messages": []map[string]any{
			{"speaker_name": "Carol", "message_type": "message", "content": "A whole new scene."},
		},
	})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", res.status, res.body)
	}

	updated := dataOf[chapterBody](t, res)
	if len(updated.Messages) != 1 || updated.Messages[0].SpeakerName != "Carol" {
		t.Fatalf("the replacement did not take effect: %+v", updated.Messages)
	}

	var snapshot *string
	err := env.db.QueryRowContext(t.Context(),
		`SELECT messages::text FROM chapter_revisions WHERE chapter_id = $1 ORDER BY version DESC LIMIT 1`,
		chapter.ID).Scan(&snapshot)
	if err != nil {
		t.Fatalf("read revision: %v", err)
	}
	if snapshot == nil || !strings.Contains(*snapshot, "Alice") {
		t.Error("the replaced conversation was not preserved in a revision")
	}
}

// A PATCH that mentions only the status must not erase a manuscript. This is
// the single most damaging bug this domain could have.
func TestChapters_PartialUpdateDoesNotEraseContent(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "chat"})

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "Kept", "content": "Prose worth keeping.", "messages": chatMessages(),
	})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	env.asOwner(t, w, http.MethodPatch, path, map[string]any{"status": "published"})

	after := env.chapter(t, w, novel.ID, chapter.ID)
	if after.Content == nil || *after.Content != "Prose worth keeping." {
		t.Errorf("content = %q; a status-only PATCH erased the prose", derefString(after.Content))
	}
	if len(after.Messages) != 4 {
		t.Errorf("messages = %d; a status-only PATCH erased the conversation", len(after.Messages))
	}
	if after.Title == nil || *after.Title != "Kept" {
		t.Error("a status-only PATCH erased the title")
	}
}

// An explicit null clears the field - the distinction a PATCH must express.
func TestChapters_ExplicitNullClearsContent(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": "Temporary."})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	env.asOwner(t, w, http.MethodPatch, path, map[string]any{"content": nil})

	after := env.chapter(t, w, novel.ID, chapter.ID)
	if after.Content != nil {
		t.Errorf("content = %q, want nil after an explicit null", derefString(after.Content))
	}
	// The cleared prose is still recoverable from the revision.
	if got := env.revisionCount(t, chapter.ID); got != 1 {
		t.Errorf("revisions = %d; clearing content must be recoverable", got)
	}
}

// docs/09 §45: an invalid message must abort the whole write, leaving no
// half-updated chapter behind.
func TestChapters_AnInvalidMessageAbortsTheWholeUpdate(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "chat"})

	chapter := env.createChapter(t, w, novel.ID, map[string]any{"messages": chatMessages()})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	res := env.asOwner(t, w, http.MethodPatch, path, map[string]any{
		"content": "This must not be written either.",
		"messages": []map[string]any{
			{"speaker_name": "Alice", "message_type": "message", "content": "fine"},
			{"speaker_name": "Bob", "message_type": "message", "content": ""}, // invalid
		},
	})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", res.status, res.body)
	}

	after := env.chapter(t, w, novel.ID, chapter.ID)
	if len(after.Messages) != 4 {
		t.Errorf("messages = %d; the rejected update partially applied", len(after.Messages))
	}
	if after.Content != nil {
		t.Errorf("content = %q; the rejected update partially applied", derefString(after.Content))
	}
	if got := env.revisionCount(t, chapter.ID); got != 0 {
		t.Errorf("revisions = %d; a rejected update must record nothing", got)
	}
}

// ---------------------------------------------------------------------------
// Ownership and publishing
// ---------------------------------------------------------------------------

func TestChapters_WriterCannotTouchAnotherWritersChapters(t *testing.T) {
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	attacker := env.newWriter(t)

	novel := env.publishedNovel(t, owner, nil)
	chapter := env.createChapter(t, owner, novel.ID, map[string]any{"content": "Mine."})
	env.publishChapter(t, owner, novel.ID, chapter.ID)

	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID
	attempts := map[string]apiRequest{
		"update":    {method: http.MethodPatch, path: path, body: map[string]any{"content": "Rewritten."}},
		"delete":    {method: http.MethodDelete, path: path},
		"publish":   {method: http.MethodPost, path: path + "/publish"},
		"unpublish": {method: http.MethodPost, path: path + "/unpublish"},
	}

	for name, attempt := range attempts {
		t.Run(name, func(t *testing.T) {
			attempt.cookies = attacker.authCookies()
			attempt.csrf = attacker.csrfToken

			res := env.do(t, attempt)
			if res.status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403. body: %s", res.status, res.body)
			}
		})
	}

	after := env.guestChapter(t, novel.ID, chapter.ID)
	if after.Content == nil || *after.Content != "Mine." {
		t.Errorf("content = %q; the attacker's write took effect", derefString(after.Content))
	}
}

// docs/AUTHENTICATION.md §9: verification gates PUBLISHING, never drafting.
func TestChapters_PublishingRequiresAVerifiedEmail(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newUnverifiedWriter(t)

	novel := env.createNovel(t, w, createNovelBody("Unverified Writer's Fiction", nil))

	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": "Drafting is fine."})
	if chapter.Status != "draft" {
		t.Fatalf("status = %q, want draft", chapter.Status)
	}

	// Editing the draft is unaffected.
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID
	if res := env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"content": "Still drafting."}); res.status != http.StatusOK {
		t.Fatalf("an unverified writer could not edit their own draft: %d %s", res.status, res.body)
	}

	for name, res := range map[string]apiResponse{
		"the publish endpoint": env.asOwner(t, w, http.MethodPost, path+"/publish", nil),
		"a status PATCH":       env.asOwner(t, w, http.MethodPatch, path, map[string]any{"status": "published"}),
		"creating published":   env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/chapters", map[string]any{"content": "x", "status": "published"}),
	} {
		t.Run(name, func(t *testing.T) {
			if res.status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403. body: %s", res.status, res.body)
			}
			if code := errorCodeOf(t, res); code != "EMAIL_VERIFICATION_REQUIRED" {
				t.Errorf("error code = %q, want EMAIL_VERIFICATION_REQUIRED", code)
			}
		})
	}
}

func TestChapters_PublishAndUnpublish(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": "Ready."})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	published := env.publishChapter(t, w, novel.ID, chapter.ID)
	if published.Status != "published" {
		t.Fatalf("status = %q, want published", published.Status)
	}
	if res := env.asGuest(t, http.MethodGet, path); res.status != http.StatusOK {
		t.Fatalf("a published chapter is not readable by a guest: %d", res.status)
	}

	if res := env.asOwner(t, w, http.MethodPost, path+"/unpublish", nil); res.status != http.StatusOK {
		t.Fatalf("unpublish status = %d, want 200. body: %s", res.status, res.body)
	}
	if res := env.asGuest(t, http.MethodGet, path); res.status != http.StatusNotFound {
		t.Errorf("an unpublished chapter is still readable: %d", res.status)
	}

	// The content survives retraction.
	after := env.chapter(t, w, novel.ID, chapter.ID)
	if after.Content == nil || *after.Content != "Ready." {
		t.Error("unpublishing destroyed the content")
	}
}

func TestChapters_DeleteIsSoftAndReleasesTheChapterNumber(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	first := env.createChapter(t, w, novel.ID, map[string]any{"content": "One."})
	if res := env.asOwner(t, w, http.MethodDelete,
		"/api/v1/novels/"+novel.ID+"/chapters/"+first.ID, nil); res.status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204. body: %s", res.status, res.body)
	}

	// docs/08 §37: soft delete, so the author's work is recoverable.
	var deletedAt *string
	if err := env.db.QueryRowContext(t.Context(),
		`SELECT deleted_at FROM chapters WHERE id = $1`, first.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("the chapter row was hard-deleted: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at was not set")
	}

	// The partial unique index releases the number, so chapter 1 can exist again.
	replacement := env.createChapter(t, w, novel.ID, map[string]any{"content": "One, again."})
	if replacement.ChapterNumber != 1 {
		t.Errorf("chapter_number = %d, want 1 - a deleted chapter should not hold its slot",
			replacement.ChapterNumber)
	}
}

// docs/11 §16: user content must never become executable markup. It is stored
// verbatim as text and returned verbatim, never as HTML.
func TestChapters_StoresHostileContentAsInertText(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "chat"})

	const payload = `<script>alert("xss")</script><img src=x onerror=alert(1)>`

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title":   "Chapter " + payload,
		"content": payload,
		"messages": []map[string]any{
			{"speaker_name": payload[:40], "message_type": "message", "content": payload},
		},
	})

	// Stored verbatim: escaping here would corrupt a fiction that legitimately
	// discusses HTML (docs/CONTENT-MODEL.md §3).
	if chapter.Content == nil || *chapter.Content != payload {
		t.Errorf("content = %q, want the text stored verbatim", derefString(chapter.Content))
	}

	// Returned as a JSON string, so it can never be parsed as markup by a client
	// that renders it into a text node.
	res := env.asOwner(t, w, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID)
	if contentType := res.header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	if strings.Contains(string(res.body), `<script>`) {
		t.Error("the payload appears unescaped in the JSON body")
	}

	// A control character IS rejected, because it is never authored deliberately.
	bad := env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/chapters",
		map[string]any{"content": "text\x00with a NUL"})
	if bad.status != http.StatusUnprocessableEntity {
		t.Errorf("a NUL byte was accepted with status %d, want 422", bad.status)
	}
}

// The Go visibility rule and its SQL twin must agree, or a count would disclose
// a chapter the reader endpoint hides (novels.LiveChapterSQL).
func TestChapters_VisibilityAgreesBetweenGoAndSQL(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	states := []map[string]any{
		{"content": "draft"},
		{"content": "published", "status": "published"},
		{"content": "unpublished", "status": "unpublished"},
		{"content": "scheduled", "status": "scheduled", "scheduled_at": future},
	}
	for _, state := range states {
		env.createChapter(t, w, novel.ID, state)
	}

	// Only the published one is live, so both the list a guest receives and the
	// count on the fiction card must say exactly one.
	summaries, _ := collectionOf[chapterBody](t,
		env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID+"/chapters"))
	if len(summaries) != 1 {
		t.Errorf("a guest sees %d chapters, want 1", len(summaries))
	}

	card := dataOf[novelBody](t, env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID))
	if card.ChapterCount != 1 {
		t.Errorf("chapter_count = %d, want 1 - the SQL count disagrees with the Go rule",
			card.ChapterCount)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (e *authEnv) chapterList(t *testing.T, w writer, novelID string) []chapterBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/novels/"+novelID+"/chapters")
	if res.status != http.StatusOK {
		t.Fatalf("chapter list status = %d, want 200. body: %s", res.status, res.body)
	}
	summaries, _ := collectionOf[chapterBody](t, res)
	return summaries
}

func (e *authEnv) chapter(t *testing.T, w writer, novelID, chapterID string) chapterBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/novels/"+novelID+"/chapters/"+chapterID)
	if res.status != http.StatusOK {
		t.Fatalf("chapter status = %d, want 200. body: %s", res.status, res.body)
	}
	return dataOf[chapterBody](t, res)
}

func (e *authEnv) guestChapter(t *testing.T, novelID, chapterID string) chapterBody {
	t.Helper()
	res := e.asGuest(t, http.MethodGet, "/api/v1/novels/"+novelID+"/chapters/"+chapterID)
	if res.status != http.StatusOK {
		t.Fatalf("guest chapter status = %d, want 200. body: %s", res.status, res.body)
	}
	return dataOf[chapterBody](t, res)
}

func (e *authEnv) changeFormat(t *testing.T, w writer, novelID string, patch map[string]any) formatBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novelID+"/format", patch)
	if res.status != http.StatusOK {
		t.Fatalf("format change status = %d, want 200. body: %s", res.status, res.body)
	}
	return dataOf[formatBody](t, res)
}

func (e *authEnv) revisionCount(t *testing.T, chapterID string) int {
	t.Helper()
	var count int
	if err := e.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM chapter_revisions WHERE chapter_id = $1`, chapterID).Scan(&count); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	return count
}

func derefString(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}
