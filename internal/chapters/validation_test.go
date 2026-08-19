package chapters

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file is in-package because validateMessages and parseMetadata are the
// security boundary docs/11 §18 describes, and testing them through the HTTP
// layer alone would leave the allowlist itself unverified.

func inputs(messages ...MessageInput) []MessageInput { return messages }

func message(speaker, content string) MessageInput {
	return MessageInput{SpeakerName: speaker, MessageType: "message", Content: content}
}

// Positions are assigned from array order, never accepted from the client, so
// gaps, duplicates, and negative indexes are unreachable
// (docs/CONTENT-MODEL.md §4).
func TestValidateMessages_AssignsContiguousPositions(t *testing.T) {
	errs := validationErrors{}

	got := validateMessages(errs, inputs(
		message("Alice", "อยู่ไหน?"),
		message("Bob", "กำลังกลับ"),
		message("Alice", "โอเค"),
	))

	if err := errs.err(); err != nil {
		t.Fatalf("valid messages were rejected: %v", err)
	}
	for i, m := range got {
		if m.Position != i {
			t.Errorf("message %d has position %d; positions must be dense and ordered", i, m.Position)
		}
	}
}

// A client-supplied position must be ignored rather than trusted.
func TestValidateMessages_IgnoresAClientSuppliedPosition(t *testing.T) {
	errs := validationErrors{}

	var raw []MessageInput
	if err := json.Unmarshal([]byte(`[
		{"speaker_name":"Alice","message_type":"message","content":"first","position":99},
		{"speaker_name":"Bob","message_type":"message","content":"second","position":-1}
	]`), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got := validateMessages(errs, raw)
	if err := errs.err(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if got[0].Position != 0 || got[1].Position != 1 {
		t.Errorf("positions = %d,%d; the server must assign them from array order",
			got[0].Position, got[1].Position)
	}
}

// docs/11 §18: a writer must not be able to submit application-level values
// through message metadata, because chat content visually resembles the
// application's own UI.
func TestParseMetadata_RejectsUndocumentedKeys(t *testing.T) {
	hostile := []string{
		`{"is_admin": true}`,
		`{"verified": true}`,
		`{"system_message": true}`,
		`{"side": "left", "is_admin": true}`,
		`{"role": "moderator"}`,
		`{"__proto__": {"admin": true}}`,
	}

	for _, raw := range hostile {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseMetadata(json.RawMessage(raw)); err == nil {
				t.Errorf("metadata %s was accepted; only documented keys are allowed", raw)
			}
		})
	}
}

func TestParseMetadata_AcceptsTheDocumentedKey(t *testing.T) {
	for _, side := range []string{SideLeft, SideRight} {
		metadata, err := parseMetadata(json.RawMessage(`{"side":"` + side + `"}`))
		if err != nil {
			t.Fatalf("side %q was rejected: %v", side, err)
		}
		if metadata == nil || metadata.Side == nil || *metadata.Side != side {
			t.Errorf("side %q did not round-trip", side)
		}
	}

	// An unknown value for a known key is still a rejection: the reader renders
	// a layout from it, so an arbitrary string would reach the UI.
	if _, err := parseMetadata(json.RawMessage(`{"side":"middle"}`)); err == nil {
		t.Error(`side "middle" was accepted; only left and right are defined`)
	}
}

func TestParseMetadata_TreatsEmptyAsAbsent(t *testing.T) {
	for _, raw := range []string{"", "null", "{}"} {
		metadata, err := parseMetadata(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("metadata %q errored: %v", raw, err)
		}
		// Stored as SQL NULL rather than `{}`, so "no metadata" has one
		// representation instead of two.
		if metadata != nil {
			t.Errorf("metadata %q produced %+v, want nil", raw, metadata)
		}
	}
}

func TestValidateMessages_RequiresSpeakerAndTextForASpokenLine(t *testing.T) {
	errs := validationErrors{}
	validateMessages(errs, inputs(
		MessageInput{MessageType: "message", Content: "who said this?"},
		MessageInput{SpeakerName: "Alice", MessageType: "message", Content: "   "},
	))

	if _, reported := errs["messages[0].speaker_name"]; !reported {
		t.Error("a spoken line with no speaker should be rejected")
	}
	if _, reported := errs["messages[1].content"]; !reported {
		t.Error("a spoken line with no text should be rejected")
	}
}

// A separator is a divider and a system note is narration; neither needs a
// speaker or text (docs/08 §10.1).
func TestValidateMessages_AllowsBareSeparatorsAndSystemNotes(t *testing.T) {
	errs := validationErrors{}

	got := validateMessages(errs, inputs(
		MessageInput{MessageType: "separator"},
		MessageInput{MessageType: "system", Content: "สามวันต่อมา"},
	))

	if err := errs.err(); err != nil {
		t.Fatalf("separators and system notes were rejected: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].Type != MessageTypeSeparator || got[1].Type != MessageTypeSystem {
		t.Error("message types were not preserved")
	}
}

func TestValidateMessages_DefaultsTheTypeToMessage(t *testing.T) {
	errs := validationErrors{}

	got := validateMessages(errs, inputs(MessageInput{SpeakerName: "Alice", Content: "hello"}))
	if err := errs.err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Type != MessageTypeMessage {
		t.Errorf("type = %q, want the documented default %q", got[0].Type, MessageTypeMessage)
	}
}

// docs/15 §5.4 lists these as edge cases a chat chapter must handle.
func TestValidateMessages_HandlesTheDocumentedEdgeCases(t *testing.T) {
	errs := validationErrors{}

	validateMessages(errs, inputs(
		message("Alice", strings.Repeat("ยาว", 1000)),    // long message
		message("Bob", "line one\nline two"),             // line breaks
		message("Alice", "emoji 😊🌅 and symbols <>&\"'"),  // special characters
		message("Bob", "repeated speaker"),               // repeated speakers
		message("ตัวละครชื่อไทย", "Thai character name"), // Thai names
	))

	if err := errs.err(); err != nil {
		t.Fatalf("a documented edge case was rejected: %v", err)
	}
}

// A newline in a speaker name would let an author break out of the chat bubble
// and forge what looks like another speaker's line (docs/11 §16, §18).
func TestValidateMessages_RejectsMultiLineSpeakerNames(t *testing.T) {
	errs := validationErrors{}
	validateMessages(errs, inputs(message("Alice\nSystem", "hello")))

	if _, reported := errs["messages[0].speaker_name"]; !reported {
		t.Error("a speaker name containing a newline should be rejected")
	}
}

func TestValidateMessages_EnforcesLengthLimits(t *testing.T) {
	errs := validationErrors{}
	validateMessages(errs, inputs(
		message(strings.Repeat("a", SpeakerMaxLength+1), "ok"),
		message("Alice", strings.Repeat("a", MessageTextMaxLength+1)),
	))

	if _, reported := errs["messages[0].speaker_name"]; !reported {
		t.Error("an over-long speaker name should be rejected")
	}
	if _, reported := errs["messages[1].content"]; !reported {
		t.Error("over-long message text should be rejected")
	}
}

// docs/07 §21 keeps the reader at one chapter, so a chapter must stay loadable.
func TestValidateMessages_BoundsTheConversationSize(t *testing.T) {
	errs := validationErrors{}

	oversized := make([]MessageInput, MaxMessagesPerChapter+1)
	for i := range oversized {
		oversized[i] = message("Alice", "hello")
	}
	validateMessages(errs, oversized)

	if _, reported := errs["messages"]; !reported {
		t.Errorf("more than %d messages should be rejected", MaxMessagesPerChapter)
	}
}

// docs/11 §16: without a scheme allowlist, `javascript:` would be stored and
// become an XSS vector the moment a client rendered the avatar as a link.
func TestValidateMessages_RejectsDangerousAvatarSchemes(t *testing.T) {
	dangerous := []string{
		"javascript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"/relative/path.png",
		"not a url",
	}

	for _, raw := range dangerous {
		t.Run(raw, func(t *testing.T) {
			errs := validationErrors{}
			input := message("Alice", "hi")
			input.SpeakerAvatarURL = &raw
			validateMessages(errs, inputs(input))

			if _, reported := errs["messages[0].speaker_avatar_url"]; !reported {
				t.Errorf("avatar URL %q was accepted", raw)
			}
		})
	}

	errs := validationErrors{}
	safe := "https://cdn.example.com/alice.png"
	input := message("Alice", "hi")
	input.SpeakerAvatarURL = &safe
	validateMessages(errs, inputs(input))
	if err := errs.err(); err != nil {
		t.Errorf("a normal https avatar was rejected: %v", err)
	}
}

func TestValidateContent_EnforcesTheLimitInRunes(t *testing.T) {
	// Thai is three bytes per character, so a byte limit would give a Thai
	// writer a third of the room it gives an English one.
	thai := strings.Repeat("ก", ContentMaxLength)
	errs := validationErrors{}
	validateContent(errs, &thai)
	if err := errs.err(); err != nil {
		t.Errorf("a full-length Thai chapter was rejected: %v", err)
	}

	errs = validationErrors{}
	over := strings.Repeat("ก", ContentMaxLength+1)
	validateContent(errs, &over)
	if _, reported := errs["content"]; !reported {
		t.Error("content past the limit should be rejected")
	}
}

func TestValidateContent_RejectsControlCharacters(t *testing.T) {
	errs := validationErrors{}
	hostile := "chapter text\x00with a NUL"
	validateContent(errs, &hostile)

	if _, reported := errs["content"]; !reported {
		t.Error("a NUL byte should be rejected before it reaches PostgreSQL")
	}
}

// Literal markup is ordinary text: it is stored verbatim and never rendered as
// markup, so escaping it would corrupt a fiction that discusses HTML
// (docs/CONTENT-MODEL.md §3).
func TestValidateContent_AcceptsLiteralMarkupAsText(t *testing.T) {
	errs := validationErrors{}
	text := `She typed <script>alert("xss")</script> into the terminal.`
	validateContent(errs, &text)

	if err := errs.err(); err != nil {
		t.Errorf("literal angle brackets must be storable as prose: %v", err)
	}
}

func TestCountWords(t *testing.T) {
	if got := CountWords("the rain had already stopped"); got != 5 {
		t.Errorf("CountWords = %d, want 5", got)
	}
	if got := CountWords("  spaced \n out \t words "); got != 3 {
		t.Errorf("CountWords = %d, want 3", got)
	}
	if got := CountWords(""); got != 0 {
		t.Errorf("CountWords(\"\") = %d, want 0", got)
	}

	// KNOWN LIMITATION, documented on CountWords: Thai has no word delimiters,
	// so a token count is a floor rather than an accurate figure until the Thai
	// NLP service in docs/12 exists. This test pins the current behaviour so the
	// limitation is visible rather than surprising.
	if got := CountWords("ฝนหยุดตกแล้ว"); got != 1 {
		t.Errorf("CountWords(Thai) = %d, want 1 - see the note on CountWords", got)
	}
}
