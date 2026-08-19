package chapters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Field bounds (docs/09 §36). Counted in RUNES: a byte limit would give a Thai
// writer roughly a third of the room it gives an English one.
const (
	TitleMaxLength       = 200
	ContentMaxLength     = 200_000
	SpeakerMaxLength     = 64
	MessageTextMaxLength = 5_000
	URLMaxLength         = 2_048

	// MaxMessagesPerChapter bounds one request and one chapter. docs/07 §21
	// keeps the reader at one chapter at a time, so a chapter is the unit that
	// must stay loadable; an unbounded conversation would break that and give a
	// cheap way to exhaust memory.
	MaxMessagesPerChapter = 2_000

	// Chapter numbering bounds (§13R). A writer may now type the number rather
	// than always taking MAX+1, because a fiction that numbers its side stories
	// separately or restarts at 1 for a second arc is an arrangement the author
	// chose. The bounds exist so a slipped keypress is caught: the column is an
	// INTEGER, and a chapter numbered 900 million is a typo in every fiction
	// anyone has written.
	//
	// The floor is 1 rather than 0 because the repository reads 0 as "append",
	// and a sentinel that is also a legal value is a bug waiting for the first
	// writer who wants a prologue at zero.
	MinChapterNumber = 1
	MaxChapterNumber = 100_000

	// Headcanon entries (12F). An entry BODY is deliberately not capped -
	// 12F is explicit that headcanon entry length is unknown by nature - but
	// the number of entries and the shape of a topic's header are, for the same
	// loadability reason the message cap exists for.
	EntryNameMaxLength   = 120
	EntryFieldMaxLength  = 48
	MaxEntryFields       = 12
	MaxEntriesPerChapter = 500

	// EntryImageURLMaxLength bounds the stored reference (13M), matching the
	// bound characters.avatar_url carries. It is a length limit on a URL, not a
	// claim about the image: the BYTES were validated by the media service when
	// they were uploaded (docs/11 §28).
	EntryImageURLMaxLength = 500
)

type validationErrors map[string][]string

func (v validationErrors) add(field, message string) {
	v[field] = append(v[field], message)
}

func (v validationErrors) err() error {
	if len(v) == 0 {
		return nil
	}
	return apierror.Validation(map[string][]string(v))
}

// safeText rejects anything that is not plain, human-readable content.
//
// docs/11 §17 requires an explicitly allowed content model rather than arbitrary
// markup. For this phase the model is plain text plus the whitespace a
// manuscript needs, which leaves stored XSS with no vector at all: there is no
// parser and the value is never emitted as markup (docs/CONTENT-MODEL.md §3).
func safeText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		switch r {
		case '\n', '\r', '\t':
			continue
		}
		// Cc controls, Cf format characters (including the bidi overrides used
		// to disguise text), Co private use, Cs surrogates.
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}

func singleLine(value string) bool {
	return safeText(value) && !strings.ContainsAny(value, "\n\r")
}

func runeLen(value string) int { return utf8.RuneCountInString(value) }

func validateOptionalURL(errs validationErrors, field string, value *string) {
	if value == nil || *value == "" {
		return
	}
	if runeLen(*value) > URLMaxLength {
		errs.add(field, fmt.Sprintf("Must be at most %d characters.", URLMaxLength))
		return
	}
	// The scheme allowlist matters: without it `javascript:` would be stored
	// here and become an XSS vector the moment a client rendered it as an image
	// or a link (docs/11 §16).
	parsed, err := url.Parse(*value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		errs.add(field, "Must be an absolute http or https URL.")
	}
}

// MessageInput is one message as submitted by a writer.
//
// There is deliberately no Position field. The server assigns positions from
// array order, so a client cannot create a gap, a duplicate, or a negative
// index, and ordering is deterministic without trusting the client
// (docs/CONTENT-MODEL.md §4).
type MessageInput struct {
	SpeakerName      string          `json:"speaker_name"`
	SpeakerAvatarURL *string         `json:"speaker_avatar_url"`
	MessageType      string          `json:"message_type"`
	Content          string          `json:"content"`
	Metadata         json.RawMessage `json:"metadata"`
}

// parseMetadata decodes and ALLOWLISTS message metadata.
//
// DisallowUnknownFields is the mechanism docs/11 §18 calls for: a writer
// submitting `{"is_admin": true}` gets a validation error rather than having the
// value stored and later rendered as if the platform had asserted it.
func parseMetadata(raw json.RawMessage) (*Metadata, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var metadata Metadata
	if err := decoder.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}

	if metadata.Side != nil {
		if *metadata.Side != SideLeft && *metadata.Side != SideRight {
			return nil, fmt.Errorf(`metadata.side must be %q or %q`, SideLeft, SideRight)
		}
	}
	if metadata.IsEmpty() {
		return nil, nil
	}
	return &metadata, nil
}

// validateMessages converts submitted messages into storable ones, assigning
// positions from array order.
func validateMessages(errs validationErrors, inputs []MessageInput) []Message {
	if len(inputs) > MaxMessagesPerChapter {
		errs.add("messages", fmt.Sprintf(
			"A chapter may contain at most %d messages.", MaxMessagesPerChapter))
		return nil
	}

	messages := make([]Message, 0, len(inputs))
	for i, input := range inputs {
		field := fmt.Sprintf("messages[%d]", i)

		messageType := MessageType(strings.TrimSpace(input.MessageType))
		if messageType == "" {
			messageType = MessageTypeMessage
		}
		if !messageType.Valid() {
			errs.add(field+".message_type", fmt.Sprintf(
				"Must be one of: %s.", joinValues(MessageTypes())))
			continue
		}

		speaker := strings.TrimSpace(input.SpeakerName)
		switch {
		case runeLen(speaker) > SpeakerMaxLength:
			errs.add(field+".speaker_name", fmt.Sprintf(
				"Must be at most %d characters.", SpeakerMaxLength))
		case speaker != "" && !singleLine(speaker):
			// A newline in a speaker name would let an author break out of the
			// chat bubble and forge what looks like another speaker's line.
			errs.add(field+".speaker_name", "Contains characters that are not allowed.")
		case speaker == "" && messageType == MessageTypeMessage:
			errs.add(field+".speaker_name", "A message needs a speaker.")
		}

		// Line breaks ARE allowed in message content - docs/15 §5.4 lists them
		// as an edge case a chat chapter must handle.
		switch {
		case runeLen(input.Content) > MessageTextMaxLength:
			errs.add(field+".content", fmt.Sprintf(
				"Must be at most %d characters.", MessageTextMaxLength))
		case !safeText(input.Content):
			errs.add(field+".content", "Contains characters that are not allowed.")
		case strings.TrimSpace(input.Content) == "" && messageType == MessageTypeMessage:
			// A separator is a divider and a system note may be empty, but a
			// spoken line with no words is not a message.
			errs.add(field+".content", "A message needs text.")
		}

		validateOptionalURL(errs, field+".speaker_avatar_url", input.SpeakerAvatarURL)

		metadata, err := parseMetadata(input.Metadata)
		if err != nil {
			errs.add(field+".metadata", "Only the documented message properties are accepted.")
		}

		messages = append(messages, Message{
			Position:         i,
			SpeakerName:      speaker,
			SpeakerAvatarURL: input.SpeakerAvatarURL,
			Type:             messageType,
			Content:          input.Content,
			Metadata:         metadata,
		})
	}
	return messages
}

// EntryInput is one headcanon entry as submitted by a writer.
//
// There is deliberately no Position field, for the same reason MessageInput has
// none: the server assigns positions from array order, so a gap, a duplicate, or
// a negative index is not representable (docs/CONTENT-MODEL.md §4).
type EntryInput struct {
	CharacterID *string  `json:"character_id"`
	Name        string   `json:"name"`
	Values      []string `json:"values"`
	Body        string   `json:"body"`
	// ImageURL is the entry's picture (13M). Absent or empty clears it - the
	// whole topic is replaced on every save, so "not sent" cannot mean "keep"
	// here the way it does for a chapter's own fields.
	ImageURL *string `json:"image_url"`
}

// validateEntryFields checks a topic's field labels.
func validateEntryFields(errs validationErrors, fields []string) []string {
	if len(fields) > MaxEntryFields {
		errs.add("entry_fields", fmt.Sprintf(
			"A topic may have at most %d fields.", MaxEntryFields))
		return nil
	}

	cleaned := make([]string, 0, len(fields))
	for i, field := range fields {
		trimmed := strings.TrimSpace(field)
		switch {
		case trimmed == "":
			// A blank heading would render as an unlabelled column on every
			// entry, so it is dropped rather than stored.
			continue
		case runeLen(trimmed) > EntryFieldMaxLength:
			errs.add(fmt.Sprintf("entry_fields[%d]", i), fmt.Sprintf(
				"Must be at most %d characters.", EntryFieldMaxLength))
		case !singleLine(trimmed):
			errs.add(fmt.Sprintf("entry_fields[%d]", i),
				"Contains characters that are not allowed.")
		default:
			cleaned = append(cleaned, trimmed)
		}
	}
	return cleaned
}

// validateEntries converts submitted entries into storable ones, assigning
// positions from array order.
//
// Character ids are only PARSED here. Whether an id belongs to this fiction is
// an ownership question, and ownership is decided in the service (docs/10 §27).
func validateEntries(errs validationErrors, inputs []EntryInput) []Entry {
	if len(inputs) > MaxEntriesPerChapter {
		errs.add("entries", fmt.Sprintf(
			"A topic may contain at most %d entries.", MaxEntriesPerChapter))
		return nil
	}

	entries := make([]Entry, 0, len(inputs))
	for i, input := range inputs {
		field := fmt.Sprintf("entries[%d]", i)

		name := strings.TrimSpace(input.Name)
		switch {
		case name == "":
			errs.add(field+".name", "An entry needs a name.")
		case runeLen(name) > EntryNameMaxLength:
			errs.add(field+".name", fmt.Sprintf(
				"Must be at most %d characters.", EntryNameMaxLength))
		case !singleLine(name):
			errs.add(field+".name", "Contains characters that are not allowed.")
		}

		// The body is uncapped by design (12F) but still has to be text.
		if !safeText(input.Body) {
			errs.add(field+".body", "Contains characters that are not allowed.")
		}

		if len(input.Values) > MaxEntryFields {
			errs.add(field+".values", fmt.Sprintf(
				"At most %d values.", MaxEntryFields))
		}
		values := make([]string, 0, len(input.Values))
		for _, value := range input.Values {
			trimmed := strings.TrimSpace(value)
			if runeLen(trimmed) > EntryFieldMaxLength*4 {
				errs.add(field+".values", fmt.Sprintf(
					"Each value must be at most %d characters.", EntryFieldMaxLength*4))
				break
			}
			if !singleLine(trimmed) {
				errs.add(field+".values", "Contains characters that are not allowed.")
				break
			}
			values = append(values, trimmed)
		}

		var characterID *uuid.UUID
		if input.CharacterID != nil && strings.TrimSpace(*input.CharacterID) != "" {
			parsed, err := uuid.Parse(strings.TrimSpace(*input.CharacterID))
			if err != nil {
				errs.add(field+".character_id", "Must be a valid id.")
			} else {
				characterID = &parsed
			}
		}

		// The image is a reference the client got back from the media endpoint,
		// so it is bounded and checked for shape - never re-validated as an
		// image, which happened when the bytes were stored (docs/11 §28).
		var imageURL *string
		if input.ImageURL != nil {
			trimmed := strings.TrimSpace(*input.ImageURL)
			switch {
			case trimmed == "":
				// An empty string is the author removing the picture.
			case runeLen(trimmed) > EntryImageURLMaxLength:
				errs.add(field+".image_url", fmt.Sprintf(
					"Must be at most %d characters.", EntryImageURLMaxLength))
			case !singleLine(trimmed):
				errs.add(field+".image_url", "Contains characters that are not allowed.")
			default:
				imageURL = &trimmed
			}
		}

		entries = append(entries, Entry{
			Position:    i,
			CharacterID: characterID,
			Name:        name,
			Values:      values,
			Body:        input.Body,
			ImageURL:    imageURL,
		})
	}
	return entries
}

// validateContentFormat checks how a chapter's prose should be rendered (§13N).
//
// The value is checked against the vocabulary and stored; the CONTENT is never
// touched, parsed, or converted on either side of a change. That is the whole
// safety argument: with no markup parser on the write path, no stored value can
// carry an injection (docs/11 §17), and a format change stays a metadata update
// with nothing to lose (docs/08 §43 Rule 7).
func validateContentFormat(errs validationErrors, raw *string) *ContentFormat {
	if raw == nil {
		return nil
	}
	value := ContentFormat(strings.TrimSpace(*raw))
	if !value.Valid() {
		errs.add("content_format", fmt.Sprintf("Must be one of: %s.",
			joinValues(ContentFormats())))
		return nil
	}
	return &value
}

// validateChapterFormat checks a chapter's own presentation format.
//
// An empty string is "follow the fiction", the same as omitting the field -
// clearing a select in a browser sends the empty string, and treating that as
// an error would make the picker's "ตามที่ตั้งไว้" option unusable.
func validateChapterFormat(errs validationErrors, raw *string) *fiction.PresentationFormat {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	value := fiction.PresentationFormat(strings.TrimSpace(*raw))
	if !value.Valid() {
		errs.add("presentation_format", fmt.Sprintf("Must be one of: %s.",
			joinValues(fiction.PresentationFormats())))
		return nil
	}
	return &value
}

func validateOptionalTitle(errs validationErrors, value *string) {
	if value == nil {
		return
	}
	trimmed := strings.TrimSpace(*value)
	switch {
	case runeLen(trimmed) > TitleMaxLength:
		errs.add("title", fmt.Sprintf("Must be at most %d characters.", TitleMaxLength))
	case trimmed != "" && !singleLine(trimmed):
		errs.add("title", "Contains characters that are not allowed.")
	}
}

func validateContent(errs validationErrors, value *string) {
	if value == nil {
		return
	}
	switch {
	case runeLen(*value) > ContentMaxLength:
		errs.add("content", fmt.Sprintf("Must be at most %d characters.", ContentMaxLength))
	case !safeText(*value):
		errs.add("content", "Contains characters that are not allowed.")
	}
}

func joinValues[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

// CountWords approximates the length of a manuscript.
//
// KNOWN LIMITATION: this counts whitespace-delimited tokens, which is correct
// for scripts that separate words with spaces and wrong for Thai, which does
// not. Thai word segmentation needs a dictionary and belongs to the Thai NLP
// service specified in docs/12; until that exists, a Thai chapter's count is a
// floor rather than an accurate figure. The value is advisory - nothing
// authorizes, orders, or bills on it.
func CountWords(text string) int {
	return len(strings.Fields(text))
}
