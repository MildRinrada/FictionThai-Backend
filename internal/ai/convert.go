package ai

// The fiction-format conversion engine (docs/CHAT-CONVERSION.md): a Standard
// prose chapter, restructured as Chat Fiction blocks - the format converted,
// never the story.
//
// The engine is DETERMINISTIC and runs on this machine, like every AI tier
// here (docs/AI-CONSISTENCY-MODEL.md: no text leaves the deployment). The
// product spec it implements is an instruction set - identify units, attribute
// speakers only on evidence, preserve wording and order, flag everything
// uncertain for the author - and those instructions are exactly the
// attribution rules the character check already trusts (`attributeLines`,
// `splitQuoted`, `speechVerbs`). What an LLM would be ASKED to do, this does
// by construction:
//
//   - Nothing is added, removed, reordered, or rewritten: every block's text
//     is a verbatim slice of the source (markup markers aside).
//   - A speaker is claimed only on the documented evidence ladder - a speech
//     verb tying the subject to the utterance, a named subject on the line,
//     or the bare-reply inheritance rule - and every rung below "speech verb"
//     is flagged `needs_review`.
//   - Reader-insert voice (คุณ, ฉัน...) is preserved as the reader, never
//     renamed.
//   - Anything the rules cannot place stays put, unattributed, flagged.
//
// The engine only ever RETURNS the structure. Writing it into
// `chapter_messages` is a separate, explicit press by the author, through the
// same update endpoint every manual edit uses - the conversion itself touches
// nothing (CLAUDE.md rules 11-14).

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
)

// ReaderSpeakerID marks a block spoken or acted by the reader-insert voice.
// A pseudo-id, not a cast record: the reader has no character sheet, but a
// chat block still needs to say whose bubble it is.
const ReaderSpeakerID = "reader"

// ConversionCharacter is one speaker the engine found evidence for.
type ConversionCharacter struct {
	SpeakerID string `json:"speaker_id"`
	Name      string `json:"name"`
}

// ConversionBlock is one content unit, in source order.
type ConversionBlock struct {
	ID          string  `json:"id"`
	Type        string  `json:"type"` // narration | dialogue | action | unknown
	SpeakerID   *string `json:"speaker_id"`
	Text        string  `json:"text"`
	Confidence  string  `json:"confidence"` // high | medium | low
	NeedsReview bool    `json:"needs_review"`
	Reason      *string `json:"reason,omitempty"`
}

// ConversionReviewItem points the author at one block needing their eye.
type ConversionReviewItem struct {
	BlockID string `json:"block_id"`
	Reason  string `json:"reason"`
}

// ChatConversion is the whole answer, per the product spec's schema.
type ChatConversion struct {
	Status      string                 `json:"conversion_status"` // success | needs_review
	Characters  []ConversionCharacter  `json:"characters"`
	Blocks      []ConversionBlock      `json:"blocks"`
	ReviewItems []ConversionReviewItem `json:"review_items"`
}

// ConvertChat restructures a prose chapter's text as chat blocks. Read-only:
// the caller receives the structure and the author decides what to do with it.
func (t *Tools) ConvertChat(
	ctx context.Context, identity *auth.Identity, novelRef, text string,
) (ChatConversion, error) {
	if !t.enabled {
		return ChatConversion{}, unavailable()
	}
	if _, err := requireUser(identity); err != nil {
		return ChatConversion{}, err
	}
	if _, err := t.editorNovel(ctx, identity, novelRef); err != nil {
		return ChatConversion{}, err
	}

	ref, _ := novels.ParseRef(novelRef)
	cast, err := t.cast.List(ctx, identity, ref)
	if err != nil {
		return ChatConversion{}, err
	}

	castNames := make(map[string][]string, len(cast))
	memberName := make(map[string]string, len(cast))
	for _, member := range cast {
		castNames[member.ID] = nameVariants(member.Name)
		memberName[member.ID] = member.Name
	}

	return convertProse(text, castNames, memberName), nil
}

// imageMarkup and linkMarkup drop pictures and unwrap links: a banner is not
// a message, and a link's words are. Colour spans keep their words the same
// way.
var (
	imageMarkup  = regexp.MustCompile(`!\[[^\]]*\]\([^)\n]*\)`)
	linkMarkup   = regexp.MustCompile(`\[([^\]]*)\]\([^)\n]*\)`)
	colourMarkup = regexp.MustCompile(`\{[a-z-]+\|([^{}\n]*)\}`)
	alignPrefix  = regexp.MustCompile(`^:(?:center|right): `)
)

// plainLine strips the manuscript's markers from one line, keeping every word.
func plainLine(line string) string {
	line = alignPrefix.ReplaceAllString(line, "")
	line = imageMarkup.ReplaceAllString(line, "")
	line = colourMarkup.ReplaceAllString(line, "$1")
	line = linkMarkup.ReplaceAllString(line, "$1")
	line = strings.NewReplacer("**", "", "__", "", "~~", "").Replace(line)
	line = strings.TrimPrefix(strings.TrimSuffix(line, "_"), "_")
	return strings.TrimSpace(line)
}

// isRule reports a scene-separator line (--- / ***) - structure, not story.
func isRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return false
	}
	return strings.Trim(trimmed, "-") == "" || strings.Trim(trimmed, "*") == ""
}

// lineSegment is one ordered span of a line: what was said, or what happened.
type lineSegment struct {
	speech bool
	text   string
}

// segmentLine walks a line and yields its narration and speech spans IN
// ORDER. The quote pairs are the platform's (splitQuoted's set), plus the
// paired straight single quote the reader rule recognises - a lone
// apostrophe with no partner on the line stays narration.
func segmentLine(line string) []lineSegment {
	var segments []lineSegment
	var current strings.Builder
	depth := 0
	straight := false
	single := false

	inSpeech := func() bool { return depth > 0 || straight || single }
	flush := func(speech bool) {
		text := strings.TrimSpace(current.String())
		current.Reset()
		if text != "" {
			segments = append(segments, lineSegment{speech: speech, text: text})
		}
	}

	runes := []rune(line)
	for at, r := range runes {
		switch r {
		case '"':
			if depth == 0 && !single {
				flush(straight)
				straight = !straight
				continue
			}
		case '\'':
			if !straight && depth == 0 {
				// An opener only when a partner exists further along the line.
				if single || strings.ContainsRune(string(runes[at+1:]), '\'') {
					flush(single)
					single = !single
					continue
				}
			}
		case '“', '‘', '«', '„', '「':
			if !straight && !single {
				if depth == 0 {
					flush(false)
				}
				depth++
				continue
			}
		case '”', '’', '»', '」':
			if !straight && !single && depth > 0 {
				depth--
				if depth == 0 {
					flush(true)
				}
				continue
			}
		}
		current.WriteRune(r)
	}
	flush(inSpeech())
	return segments
}

// hasSpeechVerbIn reports whether narration ties its subject to the utterance.
func hasSpeechVerbIn(narration string) bool {
	for _, verb := range speechVerbs {
		if strings.Contains(narration, verb) {
			return true
		}
	}
	return false
}

// convertProse is the pure engine: text in, blocks out. Split from the
// service method so tests need no service.
func convertProse(text string, castNames map[string][]string, memberName map[string]string) ChatConversion {
	lines := strings.Split(text, "\n")
	actors := attributeLines(lines, castNames)

	out := ChatConversion{
		Status:      "success",
		Characters:  []ConversionCharacter{},
		Blocks:      []ConversionBlock{},
		ReviewItems: []ConversionReviewItem{},
	}
	seenSpeaker := map[string]bool{}

	speakerRef := func(id string) *string {
		if id == "" {
			return nil
		}
		if !seenSpeaker[id] {
			seenSpeaker[id] = true
			name := memberName[id]
			if id == ReaderSpeakerID {
				name = "คุณ (ผู้อ่าน)"
			}
			out.Characters = append(out.Characters, ConversionCharacter{
				SpeakerID: id, Name: name,
			})
		}
		return &id
	}

	push := func(block ConversionBlock) {
		block.ID = fmt.Sprintf("block_%03d", len(out.Blocks)+1)
		if block.NeedsReview && block.Reason != nil {
			out.ReviewItems = append(out.ReviewItems, ConversionReviewItem{
				BlockID: block.ID, Reason: *block.Reason,
			})
		}
		out.Blocks = append(out.Blocks, block)
	}
	reason := func(text string) *string { return &text }

	for i, raw := range lines {
		if strings.TrimSpace(raw) == "" || isRule(raw) {
			continue
		}
		line := plainLine(raw)
		if line == "" {
			continue
		}

		narration, _ := splitQuoted(line)
		verbTied := hasSpeechVerbIn(narration)
		lineActor := actors[i]
		// attributeLines collapses the reader-insert subject to "own, nobody":
		// that IS the reader's line (docs/AI-CONSISTENCY-MODEL.md rule 3).
		readerActed := lineActor.own && lineActor.actor == ""

		for _, segment := range segmentLine(line) {
			if !segment.speech {
				// What happened, kept verbatim. An identified doer makes it an
				// action; otherwise it is narration - never a message.
				if lineActor.own && lineActor.actor != "" {
					push(ConversionBlock{
						Type: "action", SpeakerID: speakerRef(lineActor.actor),
						Text: segment.text, Confidence: "high",
					})
				} else if readerActed {
					push(ConversionBlock{
						Type: "action", SpeakerID: speakerRef(ReaderSpeakerID),
						Text: segment.text, Confidence: "high",
					})
				} else {
					push(ConversionBlock{
						Type: "narration", Text: segment.text, Confidence: "high",
					})
				}
				continue
			}

			// An utterance. The speaker is claimed only on the evidence ladder;
			// every rung below "speech verb ties the subject" is flagged.
			switch {
			case lineActor.own && lineActor.actor != "" && verbTied:
				push(ConversionBlock{
					Type: "dialogue", SpeakerID: speakerRef(lineActor.actor),
					Text: segment.text, Confidence: "high",
				})
			case readerActed && verbTied:
				push(ConversionBlock{
					Type: "dialogue", SpeakerID: speakerRef(ReaderSpeakerID),
					Text: segment.text, Confidence: "high",
				})
			case lineActor.actor != "":
				kind := "ผู้พูดอนุมานจากบริบทใกล้เคียง ไม่มีกริยาบอกการพูดยืนยัน - ตรวจก่อนใช้"
				push(ConversionBlock{
					Type: "dialogue", SpeakerID: speakerRef(lineActor.actor),
					Text: segment.text, Confidence: "medium",
					NeedsReview: true, Reason: reason(kind),
				})
			case readerActed:
				push(ConversionBlock{
					Type: "dialogue", SpeakerID: speakerRef(ReaderSpeakerID),
					Text: segment.text, Confidence: "medium",
					NeedsReview: true,
					Reason:      reason("บรรทัดนี้เป็นของผู้อ่านตามบริบท แต่ไม่มีกริยาบอกการพูดยืนยัน - ตรวจก่อนใช้"),
				})
			default:
				push(ConversionBlock{
					Type: "dialogue", SpeakerID: nil,
					Text: segment.text, Confidence: "low",
					NeedsReview: true,
					Reason:      reason("ระบุผู้พูดจากต้นฉบับไม่ได้ - ต้องให้ผู้เขียนกำหนดเอง"),
				})
			}
		}
	}

	if len(out.ReviewItems) > 0 {
		out.Status = "needs_review"
	}
	return out
}
