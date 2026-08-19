package chapters_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/fiction"
)

func ptr[T any](value T) *T { return &value }

var reference = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func newChapter() chapters.Chapter {
	return chapters.Chapter{
		ID:      uuid.New(),
		NovelID: uuid.New(),
		Number:  1,
		Slug:    "chapter-one",
		Status:  chapters.StatusPublished,
	}
}

// docs/11 §21: a public fiction does not make its unpublished chapters public.
func TestChapter_Live(t *testing.T) {
	past := reference.Add(-time.Hour)
	future := reference.Add(time.Hour)
	deleted := reference

	tests := map[string]struct {
		status      chapters.Status
		publishedAt *time.Time
		scheduledAt *time.Time
		deletedAt   *time.Time
		want        bool
	}{
		"published with a past date": {chapters.StatusPublished, &past, nil, nil, true},
		"published with no date":     {chapters.StatusPublished, nil, nil, nil, true},

		"draft":       {chapters.StatusDraft, nil, nil, nil, false},
		"unpublished": {chapters.StatusUnpublished, &past, nil, nil, false},

		// A schedule goes live when its time arrives. Computing that at read
		// time needs no worker, so a worker that failed to run cannot leave a
		// chapter permanently unpublished.
		"scheduled for the future": {chapters.StatusScheduled, nil, &future, nil, false},
		"scheduled for the past":   {chapters.StatusScheduled, nil, &past, nil, true},
		"scheduled with no time":   {chapters.StatusScheduled, nil, nil, nil, false},

		"published but deleted": {chapters.StatusPublished, &past, nil, &deleted, false},

		// A future published_at is a scheduled publication by another name and
		// must not be readable yet.
		"published with a future date": {chapters.StatusPublished, &future, nil, nil, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			chapter := newChapter()
			chapter.Status = tc.status
			chapter.PublishedAt = tc.publishedAt
			chapter.ScheduledAt = tc.scheduledAt
			chapter.DeletedAt = tc.deletedAt

			if got := chapter.Live(reference); got != tc.want {
				t.Errorf("Live() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The boundary is inclusive: a chapter scheduled for exactly now is live.
func TestChapter_Live_AtTheExactScheduledInstant(t *testing.T) {
	chapter := newChapter()
	chapter.Status = chapters.StatusScheduled
	chapter.ScheduledAt = &reference

	if !chapter.Live(reference) {
		t.Error("a chapter scheduled for exactly now should be live")
	}
	if chapter.Live(reference.Add(-time.Nanosecond)) {
		t.Error("a chapter must not be live one nanosecond before its time")
	}
}

func entries() []chapters.Entry {
	return []chapters.Entry{
		{Position: 0, Name: "อลิซ", Values: []string{"20"}, Body: "ชอบฝนตอนเช้า"},
	}
}

func messages() []chapters.Message {
	return []chapters.Message{
		{Position: 0, SpeakerName: "Alice", Type: chapters.MessageTypeMessage, Content: "อยู่ไหน?"},
		{Position: 1, SpeakerName: "Bob", Type: chapters.MessageTypeMessage, Content: "กำลังกลับ"},
	}
}

// The heart of docs/CONTENT-MODEL.md §6: a reader receives ONLY the active
// representation, so a standard reader never sees the chat rows and vice versa.
func TestChapter_Render_ReaderSeesOnlyTheActiveRepresentation(t *testing.T) {
	chapter := newChapter()
	chapter.Content = ptr("The rain had already stopped.")

	t.Run("standard fiction serves prose", func(t *testing.T) {
		view := chapter.Render(chapters.ViewParams{Active: fiction.Standard, Messages: messages()})

		if view.Content == nil || *view.Content != "The rain had already stopped." {
			t.Error("a standard reader must receive the prose")
		}
		if view.Messages != nil {
			t.Error("a standard reader must not receive chat messages")
		}
	})

	t.Run("chat fiction serves messages", func(t *testing.T) {
		view := chapter.Render(chapters.ViewParams{Active: fiction.Chat, Messages: messages()})

		if view.Content != nil {
			t.Error("a chat reader must not receive the prose")
		}
		if len(view.Messages) != 2 {
			t.Fatalf("messages = %d, want 2", len(view.Messages))
		}
		if view.Messages[0].SpeakerName != "Alice" || view.Messages[1].SpeakerName != "Bob" {
			t.Error("speaker identity must be preserved (docs/15 §5.4)")
		}
		if view.Messages[0].Position != 0 || view.Messages[1].Position != 1 {
			t.Error("message order must be preserved (docs/15 §5.4)")
		}
	})
}

// The writer must be able to SEE that switching format destroyed nothing
// (docs/08 §10.3, docs/CONTENT-MODEL.md §6).
func TestChapter_Render_OwnerSeesBothRepresentations(t *testing.T) {
	chapter := newChapter()
	chapter.Content = ptr("Prose the author wrote before switching to chat.")

	view := chapter.Render(chapters.ViewParams{
		Active:   fiction.Chat, // the fiction now presents as chat
		Messages: messages(),
		Entries:  entries(),
		IsOwner:  true,
	})

	if view.Content == nil {
		t.Fatal("the owner's prose vanished from the response after switching to chat")
	}
	if len(view.Messages) != 2 {
		t.Error("the owner must also see the chat messages")
	}
	if view.HasStandardContent == nil || !*view.HasStandardContent {
		t.Error("has_standard_content must tell the writer their prose is still saved")
	}
	if view.HasChatContent == nil || !*view.HasChatContent {
		t.Error("has_chat_content must report the prepared conversation")
	}
	if len(view.Entries) != 1 {
		t.Error("the owner must also see the headcanon entries")
	}
	if view.HasEntries == nil || !*view.HasEntries {
		t.Error("has_entries must report the prepared topic")
	}
}

// docs/08 §11: a chat fiction whose chapter has no messages yet gets a setup
// state, not a silent rewrite of the manuscript.
func TestChapter_Render_ContentReadyTracksTheActiveRepresentation(t *testing.T) {
	proseOnly := newChapter()
	proseOnly.Content = ptr("Prose only.")

	if !proseOnly.Render(chapters.ViewParams{Active: fiction.Standard}).ContentReady {
		t.Error("prose in a standard fiction is ready")
	}
	if proseOnly.Render(chapters.ViewParams{Active: fiction.Chat}).ContentReady {
		t.Error("prose in a CHAT fiction is not chat content; the author must be shown a setup state")
	}

	chatOnly := newChapter()
	if chatOnly.Render(chapters.ViewParams{Active: fiction.Standard, Messages: messages()}).ContentReady {
		t.Error("messages in a STANDARD fiction do not make the prose ready")
	}
	if !chatOnly.Render(chapters.ViewParams{Active: fiction.Chat, Messages: messages()}).ContentReady {
		t.Error("messages in a chat fiction are ready")
	}
}

// Blank prose is not content. Reporting it as ready would hide the setup state
// from an author who created a chapter and has not written in it yet.
func TestChapter_Render_EmptyProseIsNotReady(t *testing.T) {
	chapter := newChapter()
	chapter.Content = ptr("")

	if chapter.Render(chapters.ViewParams{}).ContentReady {
		t.Error("an empty chapter must not report content_ready")
	}
}

// A reader must never be handed owner metadata (docs/08 §1.4).
func TestChapter_Render_ReaderJSONOmitsOwnerFields(t *testing.T) {
	chapter := newChapter()
	chapter.Content = ptr("Prose.")
	chapter.ScheduledAt = &reference

	encoded, err := json.Marshal(chapter.Render(chapters.ViewParams{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, field := range []string{"scheduled_at", "has_standard_content", "has_chat_content"} {
		if _, present := decoded[field]; present {
			t.Errorf("%q is owner metadata and leaked into a reader's JSON", field)
		}
	}

	// content and messages are explicitly null rather than absent, so a client
	// can tell "no prose" from "this API build has no such field".
	for _, field := range []string{"content", "messages"} {
		if _, present := decoded[field]; !present {
			t.Errorf("%q must always be present, even when null", field)
		}
	}
}

func TestSummary_CarriesNoContent(t *testing.T) {
	chapter := newChapter()
	chapter.Content = ptr("A whole chapter of prose that must not appear in a list.")

	// docs/07 §21: never load a whole fiction into the browser.
	encoded, err := json.Marshal(chapter.Summarize(fiction.Standard, chapters.Presence{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, field := range []string{"content", "messages"} {
		if _, present := decoded[field]; present {
			t.Errorf("the chapter list must not carry %q", field)
		}
	}
	if _, present := decoded["content_ready"]; !present {
		t.Error("the list needs content_ready so the writer UI can flag unprepared chapters")
	}
}

func TestStatusAndMessageType_RejectUnknownValues(t *testing.T) {
	for _, status := range chapters.Statuses() {
		if !status.Valid() {
			t.Errorf("documented status %q was rejected", status)
		}
	}
	for _, bad := range []chapters.Status{"", "archived", "PUBLISHED", "ongoing"} {
		if bad.Valid() {
			t.Errorf("undocumented status %q was accepted", bad)
		}
	}

	for _, messageType := range chapters.MessageTypes() {
		if !messageType.Valid() {
			t.Errorf("documented message type %q was rejected", messageType)
		}
	}
	// docs/08 §10.1 lists image/reaction/choice as POSSIBLE future types. They
	// are not implemented, so they must not be accepted yet.
	for _, bad := range []chapters.MessageType{"", "image", "reaction", "choice", "MESSAGE"} {
		if bad.Valid() {
			t.Errorf("unimplemented message type %q was accepted", bad)
		}
	}
}

func TestChapter_VisibleAt_FallsBackToTheSchedule(t *testing.T) {
	scheduled := newChapter()
	scheduled.Status = chapters.StatusScheduled
	scheduled.ScheduledAt = &reference

	if got := scheduled.VisibleAt(); got == nil || !got.Equal(reference) {
		t.Error("a scheduled chapter should report its scheduled time as the publication date")
	}

	draft := newChapter()
	draft.Status = chapters.StatusDraft
	if draft.VisibleAt() != nil {
		t.Error("a draft has no publication date")
	}
}
