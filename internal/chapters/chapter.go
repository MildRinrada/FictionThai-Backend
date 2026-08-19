// Package chapters owns chapter content: prose, structured chat messages,
// headcanon entries, and revision history.
//
// A chapter can hold ALL THREE representations at once - prose in `content`, a
// conversation in `chapter_messages`, and entries in `chapter_entries`
// (docs/08 §10.3, docs/PHASE-12-STORY-DEPTH.md §12F). The active
// presentation_format selects which one readers see; it does not select which
// one exists. That is what makes a format change a metadata-only operation with
// nothing to convert and therefore nothing to lose. See docs/CONTENT-MODEL.md.
//
// Ownership flows Novel -> Chapter -> ChapterMessage (docs/08 §10.2), so this
// package never stores an owner of its own: it asks the novels service, which is
// the single authorization boundary for the whole subtree.
package chapters

import (
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/fiction"
)

// Status is the chapter publication state (docs/08 §8.1).
type Status string

const (
	StatusDraft       Status = "draft"
	StatusScheduled   Status = "scheduled"
	StatusPublished   Status = "published"
	StatusUnpublished Status = "unpublished"
)

// DefaultStatus keeps new chapters private until the writer publishes
// (docs/11 §31).
const DefaultStatus = StatusDraft

func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusScheduled, StatusPublished, StatusUnpublished:
		return true
	}
	return false
}

// Statuses returns every supported value.
func Statuses() []Status {
	return []Status{StatusDraft, StatusScheduled, StatusPublished, StatusUnpublished}
}

// MessageType classifies one chat entry (docs/08 §10.1).
type MessageType string

const (
	// MessageTypeMessage is someone speaking. It requires a speaker and text.
	MessageTypeMessage MessageType = "message"
	// MessageTypeSystem is narration or a scene note inside the conversation.
	MessageTypeSystem MessageType = "system"
	// MessageTypeSeparator is a visual divider and may carry no text at all.
	MessageTypeSeparator MessageType = "separator"
)

func (t MessageType) Valid() bool {
	switch t {
	case MessageTypeMessage, MessageTypeSystem, MessageTypeSeparator:
		return true
	}
	return false
}

// MessageTypes returns every supported value.
func MessageTypes() []MessageType {
	return []MessageType{MessageTypeMessage, MessageTypeSystem, MessageTypeSeparator}
}

// ContentFormat says how a chapter's prose is RENDERED (§13N).
//
// It answers the discriminator question docs/CONTENT-MODEL.md §3 left open. The
// server never parses either value: what is stored is text under both, and the
// difference exists entirely in the reader. That is what keeps the platform's
// oldest safety property intact - there is no markup parser and no sanitizer on
// the write path, so no stored value can carry an injection (docs/11 §17).
type ContentFormat string

const (
	// FormatPlain is the pre-13N model and the default for every chapter written
	// before it: literal text, newlines preserved, nothing interpreted.
	FormatPlain ContentFormat = "plain"
	// FormatMarkdown is the restricted subset the writer editor produces. It is
	// a strict superset of plain text, so moving a chapter onto it changes no
	// bytes - only how they are read.
	FormatMarkdown ContentFormat = "markdown"

	// DefaultContentFormat is what a NEW chapter is created as. A chapter with
	// nothing in it has nothing to reinterpret, so the editor's own model is
	// safe here; an existing chapter keeps 'plain' until its author says so.
	DefaultContentFormat = FormatMarkdown
)

func (f ContentFormat) Valid() bool {
	switch f {
	case FormatPlain, FormatMarkdown:
		return true
	}
	return false
}

// ContentFormats returns every supported value, for clients and error messages.
func ContentFormats() []ContentFormat { return []ContentFormat{FormatPlain, FormatMarkdown} }

// Chapter mirrors the `chapters` table.
type Chapter struct {
	ID      uuid.UUID
	NovelID uuid.UUID

	Number int
	Title  *string
	Slug   string

	// Content is the standard prose manuscript, or nil if the chapter has none.
	// It is always TEXT and is never interpreted as markup by this server: the
	// only consumer of ContentFormat is the renderer (docs/11 §16, §17).
	Content *string

	// ContentFormat says how a reader renders Content (§13N). It is metadata
	// about presentation and never a claim about what is stored - changing it
	// writes no content, in either direction.
	ContentFormat ContentFormat

	// Format is what THIS chapter renders as, or nil to follow the fiction
	// (docs/PHASE-13-CREATION-AND-CONTROL.md §13J). Nil is the norm; a value is
	// the writer of a mixed fiction saying "this one is different".
	Format *fiction.PresentationFormat

	// EntryFields are the topic's field labels, defined per topic - and a topic
	// is a chapter (12F). Always non-nil so a chapter with no fields serialises
	// as [] rather than null.
	EntryFields []string

	Status      Status
	PublishedAt *time.Time
	ScheduledAt *time.Time
	WordCount   int

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Live reports whether someone who is not the owner may read this chapter now.
//
// docs/11 §21 is explicit that a public fiction does not make its unpublished
// chapters public. This is the only definition of chapter readability in Go;
// novels.LiveChapterSQL is the identical rule in SQL, and
// TestChapterVisibility_GoAndSQLAgree keeps the two honest.
//
// A `scheduled` chapter goes live when its time arrives. Computing that at read
// time is deliberate: it needs no background worker to be correct, and a worker
// that failed to run could otherwise leave a chapter permanently unpublished.
func (c *Chapter) Live(now time.Time) bool {
	if c.DeletedAt != nil {
		return false
	}
	switch c.Status {
	case StatusPublished:
		return c.PublishedAt == nil || !c.PublishedAt.After(now)
	case StatusScheduled:
		return c.ScheduledAt != nil && !c.ScheduledAt.After(now)
	}
	return false
}

// ActiveFormat resolves which representation this chapter renders as.
//
// This is the ONLY definition of the rule (docs/CONTENT-MODEL.md §2):
//
//	active = chapter.Format ?? novel.PresentationFormat
//
// with the chapter's own value consulted only while the fiction allows mixed
// formats. Turning mixed OFF therefore makes the work render uniformly again
// WITHOUT erasing what a chapter declared - turning it back on must find the
// work exactly as the writer left it.
//
// The API sends the result on every chapter so clients render what the server
// decided rather than re-deriving this (docs/09 §51).
func (c *Chapter) ActiveFormat(novel fiction.Format) fiction.PresentationFormat {
	if c.Format != nil && c.Format.Valid() {
		return *c.Format
	}
	return novel.PresentationFormat
}

// VisibleAt returns the timestamp readers should see as the publication date.
func (c *Chapter) VisibleAt() *time.Time {
	if c.PublishedAt != nil {
		return c.PublishedAt
	}
	if c.Status == StatusScheduled {
		return c.ScheduledAt
	}
	return nil
}

// Message mirrors one `chapter_messages` row.
type Message struct {
	ID       uuid.UUID
	Position int

	SpeakerName      string
	SpeakerAvatarURL *string

	Type     MessageType
	Content  string
	Metadata *Metadata
}

// Metadata is the ALLOWLISTED set of content properties a message may carry.
//
// docs/11 §18 requires exactly this: a writer must not be able to submit
// application-level values such as is_admin, verified, or system_message through
// message metadata, because chat content visually resembles application UI and
// a reader must never confuse authored content with application state.
//
// Adding a key here is a documented decision, not something a client can drive.
type Metadata struct {
	// Side is which side of the conversation the speaker appears on, which
	// docs/06 §16 renders as a two-sided layout. It describes the story, not the
	// application.
	Side *string `json:"side,omitempty"`
}

// Sides are the accepted values for Metadata.Side.
const (
	SideLeft  = "left"
	SideRight = "right"
)

// IsEmpty reports whether the metadata carries nothing, so an empty object is
// stored as SQL NULL rather than as `{}`.
func (m *Metadata) IsEmpty() bool { return m == nil || m.Side == nil }

// MessageView is one message as returned by the API.
type MessageView struct {
	ID               uuid.UUID   `json:"id"`
	Position         int         `json:"position"`
	SpeakerName      string      `json:"speaker_name"`
	SpeakerAvatarURL *string     `json:"speaker_avatar_url,omitempty"`
	MessageType      MessageType `json:"message_type"`
	Content          string      `json:"content"`
	Metadata         *Metadata   `json:"metadata,omitempty"`
}

func (m Message) View() MessageView {
	return MessageView{
		ID:               m.ID,
		Position:         m.Position,
		SpeakerName:      m.SpeakerName,
		SpeakerAvatarURL: m.SpeakerAvatarURL,
		MessageType:      m.Type,
		Content:          m.Content,
		Metadata:         m.Metadata,
	}
}

// Entry is one headcanon entry - the third representation (12F).
//
// Name is denormalised beside CharacterID on purpose: an entry may be about
// someone who has no character record, and deleting a character must empty the
// link rather than delete the author's paragraph.
type Entry struct {
	ID          uuid.UUID
	Position    int
	CharacterID *uuid.UUID
	Name        string
	// Values answer the chapter's EntryFields, positionally.
	Values []string
	Body   string
	// ImageURL is a picture the author attached to this entry (13M). nil is the
	// norm: an entry is complete with a name and a body, and the image is a
	// fourth, optional thing rather than a required avatar.
	ImageURL *string
}

// EntryView is one entry as returned by the API.
type EntryView struct {
	ID          uuid.UUID  `json:"id"`
	Position    int        `json:"position"`
	CharacterID *uuid.UUID `json:"character_id,omitempty"`
	Name        string     `json:"name"`
	Values      []string   `json:"values"`
	Body        string     `json:"body"`
	ImageURL    *string    `json:"image_url,omitempty"`
}

func (e Entry) View() EntryView {
	values := e.Values
	if values == nil {
		values = []string{}
	}
	return EntryView{
		ID:          e.ID,
		Position:    e.Position,
		CharacterID: e.CharacterID,
		Name:        e.Name,
		Values:      values,
		Body:        e.Body,
		ImageURL:    e.ImageURL,
	}
}

// Summary is a chapter in a list: metadata only, never content.
//
// docs/07 §21 forbids loading a whole fiction into the browser, so the chapter
// list carries what navigation needs and nothing more.
type Summary struct {
	ID            uuid.UUID `json:"id"`
	ChapterNumber int       `json:"chapter_number"`
	Title         *string   `json:"title,omitempty"`
	Slug          string    `json:"slug"`
	Status        Status    `json:"status"`
	WordCount     int       `json:"word_count"`

	// PresentationFormat is what THIS chapter declared, or null to follow the
	// fiction. Null rather than omitted: a client must be able to tell "follows
	// the fiction" from "this build of the API does not send the field".
	PresentationFormat *fiction.PresentationFormat `json:"presentation_format"`

	// ActiveFormat is what it RESOLVED to. Sent so a client renders the
	// server's answer instead of re-deriving the rule (docs/09 §51).
	ActiveFormat fiction.PresentationFormat `json:"active_format"`

	// ContentReady reports whether the ACTIVE representation has content - that
	// is, prose for standard, messages for chat, entries for headcanon. It is
	// derived per request, so it can never drift from the current format.
	ContentReady bool `json:"content_ready"`

	// MessageCount and EntryCount size the chat and headcanon representations,
	// so a list row can state its quantity in the active mode's own unit
	// (words / messages / entries) instead of a word count that only fits prose.
	MessageCount int `json:"message_count"`
	EntryCount   int `json:"entry_count"`

	// ContentFormat is how the prose is rendered (§13N). Sent on the SUMMARY as
	// well as the full view because the studio's chapter list is where a writer
	// sees which of their older chapters are still literal text.
	ContentFormat ContentFormat `json:"content_format"`

	// ScheduledAt is owner-only, set by the service after Summarize - never by
	// Summarize itself, which cannot know who is asking. It lives on the
	// SUMMARY because the studio's "ตารางลงตอน" is built from the chapter list,
	// and a schedule the list cannot show is a schedule the writer cannot see.
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`

	PublishedAt *time.Time `json:"published_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// View is a full chapter, including the content the caller is entitled to.
//
// Readers receive only the ACTIVE representation; the inactive ones are null.
// Owners receive all of them, because a writer who switches to chat must be able
// to see that their prose is still there (docs/CONTENT-MODEL.md §6).
type View struct {
	Summary

	NovelID uuid.UUID `json:"novel_id"`

	// Content is prose. Explicitly null rather than omitted, so a client can
	// tell "no prose" from "field missing from this build of the API".
	Content *string `json:"content"`

	// Messages is the ordered conversation, or null when the chapter has none
	// the caller may see.
	Messages []MessageView `json:"messages"`

	// Entries is the ordered headcanon topic, or null when the chapter has none
	// the caller may see. EntryFields are the topic's labels and are always
	// sent, so a client can render an empty topic's columns.
	Entries     []EntryView `json:"entries"`
	EntryFields []string    `json:"entry_fields"`

	// Adjacent chapter navigation, so a reader can move on without fetching the
	// whole chapter list (docs/07 §21).
	PreviousChapterID *uuid.UUID `json:"previous_chapter_id,omitempty"`
	NextChapterID     *uuid.UUID `json:"next_chapter_id,omitempty"`

	// --- owner-only ---------------------------------------------------------
	IsOwner bool `json:"is_owner"`

	// HasStandardContent, HasChatContent and HasEntries tell the writer UI that
	// the representations the chapter is NOT currently rendering still exist -
	// the visible proof that changing format destroyed nothing (docs/08 §10.3).
	HasStandardContent *bool `json:"has_standard_content,omitempty"`
	HasChatContent     *bool `json:"has_chat_content,omitempty"`
	HasEntries         *bool `json:"has_entries,omitempty"`
}

// hasProse reports whether the chapter has non-blank prose.
func (c *Chapter) hasProse() bool {
	return c.Content != nil && *c.Content != ""
}

// Presence is which representations a chapter actually holds - and how much.
// It travels with the chapter from the query that loaded it, so
// `content_ready` (and the studio's per-mode quantity) never costs a second
// round trip per row on a table of contents (docs/07 §67).
type Presence struct {
	HasMessages bool
	HasEntries  bool

	// The counts behind the booleans. A chat chapter's size is its message
	// count and a headcanon topic's is its entry count - the word count means
	// little for either, and the studio list says "48 ข้อความ", not "48 คำ".
	MessageCount int
	EntryCount   int
}

// Summarize renders the chapter for a list, given the format it resolved to and
// which representations exist.
// contentFormat is the stored value with a floor under it. A row read from
// before the column existed, or a zero-valued Chapter in a test, must render as
// the literal text it was written as rather than as an empty discriminator.
func (c *Chapter) contentFormat() ContentFormat {
	if !c.ContentFormat.Valid() {
		return FormatPlain
	}
	return c.ContentFormat
}

func (c *Chapter) Summarize(active fiction.PresentationFormat, has Presence) Summary {
	var ready bool
	switch active {
	case fiction.Chat:
		ready = has.HasMessages
	case fiction.HeadcanonFormat:
		ready = has.HasEntries
	default:
		ready = c.hasProse()
	}

	return Summary{
		ID:                 c.ID,
		ChapterNumber:      c.Number,
		Title:              c.Title,
		Slug:               c.Slug,
		Status:             c.Status,
		WordCount:          c.WordCount,
		PresentationFormat: c.Format,
		ActiveFormat:       active,
		ContentReady:       ready,
		MessageCount:       has.MessageCount,
		EntryCount:         has.EntryCount,
		ContentFormat:      c.contentFormat(),
		PublishedAt:        c.VisibleAt(),
		CreatedAt:          c.CreatedAt,
		UpdatedAt:          c.UpdatedAt,
	}
}

// ViewParams carries everything the view needs beyond the chapter row itself.
type ViewParams struct {
	// Active is the format this chapter resolved to, from ActiveFormat.
	Active   fiction.PresentationFormat
	Messages []Message
	Entries  []Entry
	IsOwner  bool
	Previous *uuid.UUID
	Next     *uuid.UUID
}

// Render builds the API view.
//
// This method is the single place that decides which representation a caller
// receives, so a reader cannot be handed an inactive one by a handler that
// forgot to filter (docs/08 §1.4).
func (c *Chapter) Render(params ViewParams) View {
	presence := Presence{
		HasMessages:  len(params.Messages) > 0,
		HasEntries:   len(params.Entries) > 0,
		MessageCount: len(params.Messages),
		EntryCount:   len(params.Entries),
	}

	fields := c.EntryFields
	if fields == nil {
		fields = []string{}
	}

	view := View{
		Summary:           c.Summarize(params.Active, presence),
		NovelID:           c.NovelID,
		EntryFields:       fields,
		PreviousChapterID: params.Previous,
		NextChapterID:     params.Next,
		IsOwner:           params.IsOwner,
	}

	messageViews := make([]MessageView, 0, len(params.Messages))
	for _, message := range params.Messages {
		messageViews = append(messageViews, message.View())
	}
	entryViews := make([]EntryView, 0, len(params.Entries))
	for _, entry := range params.Entries {
		entryViews = append(entryViews, entry.View())
	}

	switch {
	case params.IsOwner:
		// Every representation, so the writer can see nothing was lost.
		view.Content = c.Content
		if presence.HasMessages {
			view.Messages = messageViews
		}
		if presence.HasEntries {
			view.Entries = entryViews
		}
		view.ScheduledAt = c.ScheduledAt

		hasProse := c.hasProse()
		view.HasStandardContent = &hasProse
		view.HasChatContent = &presence.HasMessages
		view.HasEntries = &presence.HasEntries

	case params.Active == fiction.Chat:
		if presence.HasMessages {
			view.Messages = messageViews
		}

	case params.Active == fiction.HeadcanonFormat:
		if presence.HasEntries {
			view.Entries = entryViews
		}

	default:
		view.Content = c.Content
	}

	return view
}
