package ai

import (
	"strings"
	"testing"
)

// The conversion engine's contract (docs/CHAT-CONVERSION.md): convert the
// format, not the story. Every block is a verbatim slice of the source, in
// source order; a speaker is claimed only on evidence; everything below the
// top rung of the evidence ladder is flagged for the author.

func castFixture() (map[string][]string, map[string]string) {
	names := map[string][]string{
		"char-aether":  nameVariants("เอเธอร์ (Aether)"),
		"char-zhongli": nameVariants("จงหลี่ (Zhongli)"),
	}
	labels := map[string]string{
		"char-aether":  "เอเธอร์ (Aether)",
		"char-zhongli": "จงหลี่ (Zhongli)",
	}
	return names, labels
}

func TestConvertProse_SpeechVerbGivesAHighConfidenceSpeaker(t *testing.T) {
	names, labels := castFixture()
	got := convertProse(`เอเธอร์พูดเสียงเบา "อย่าเพิ่งไปนะ"`, names, labels)

	if len(got.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (action + dialogue)", len(got.Blocks))
	}
	action, dialogue := got.Blocks[0], got.Blocks[1]
	if action.Type != "action" || *action.SpeakerID != "char-aether" {
		t.Errorf("first block = %+v, want เอเธอร์'s action", action)
	}
	if dialogue.Type != "dialogue" || dialogue.SpeakerID == nil || *dialogue.SpeakerID != "char-aether" {
		t.Fatalf("second block = %+v, want เอเธอร์'s dialogue", dialogue)
	}
	if dialogue.Confidence != "high" || dialogue.NeedsReview {
		t.Errorf("a speech-verb tie is the top rung: %+v", dialogue)
	}
	if dialogue.Text != "อย่าเพิ่งไปนะ" {
		t.Errorf("dialogue text = %q, want the verbatim utterance", dialogue.Text)
	}
	if got.Status != "success" {
		t.Errorf("status = %q, want success", got.Status)
	}
}

func TestConvertProse_NeverInventsASpeaker(t *testing.T) {
	names, labels := castFixture()
	// Bare dialogue after another quote: the reply rule refuses to guess.
	got := convertProse("\"ใครอยู่ตรงนั้น\"\n\"ไม่มีใครหรอก\"", names, labels)

	var speeches []ConversionBlock
	for _, block := range got.Blocks {
		if block.Type == "dialogue" {
			speeches = append(speeches, block)
		}
	}
	if len(speeches) != 2 {
		t.Fatalf("dialogue blocks = %d, want 2", len(speeches))
	}
	for _, speech := range speeches {
		if speech.SpeakerID != nil {
			t.Errorf("speaker invented for %q: %v", speech.Text, *speech.SpeakerID)
		}
		if !speech.NeedsReview || speech.Confidence != "low" {
			t.Errorf("an unattributable quote must be flagged: %+v", speech)
		}
	}
	if got.Status != "needs_review" {
		t.Errorf("status = %q, want needs_review", got.Status)
	}
	if len(got.ReviewItems) != 2 {
		t.Errorf("review items = %d, want one per flagged block", len(got.ReviewItems))
	}
}

func TestConvertProse_PreservesOrderAndEveryWord(t *testing.T) {
	names, labels := castFixture()
	source := `จงหลี่หันกลับมามองท่าไม้เก่า

"เธอจะไปจริง ๆ เหรอ" เขาถามโดยไม่หันมา

คุณพยักหน้าช้า ๆ`
	got := convertProse(source, names, labels)

	var texts []string
	for _, block := range got.Blocks {
		texts = append(texts, block.Text)
	}
	joined := strings.Join(texts, "\n")
	for _, fragment := range []string{
		"จงหลี่หันกลับมามองท่าไม้เก่า",
		"เธอจะไปจริง ๆ เหรอ",
		"เขาถามโดยไม่หันมา",
		"คุณพยักหน้าช้า ๆ",
	} {
		if !strings.Contains(joined, fragment) {
			t.Errorf("source fragment %q missing from the output", fragment)
		}
	}
	// Order is the source's order.
	if !(got.Blocks[0].Type == "action" && got.Blocks[1].Type == "dialogue") {
		t.Errorf("order changed: %+v", got.Blocks)
	}
	// คุณ is the reader and stays the reader (spec §6).
	last := got.Blocks[len(got.Blocks)-1]
	if last.SpeakerID == nil || *last.SpeakerID != ReaderSpeakerID {
		t.Errorf("reader-insert action lost its voice: %+v", last)
	}
	if !strings.Contains(last.Text, "คุณ") {
		t.Errorf("reader notation rewritten: %q", last.Text)
	}
}

func TestConvertProse_PairedSingleQuoteIsDialogue(t *testing.T) {
	names, labels := castFixture()
	got := convertProse(`เอเธอร์กระซิบ 'อย่าบอกใคร' แล้วเดินจากไป`, names, labels)

	var dialogue *ConversionBlock
	for i := range got.Blocks {
		if got.Blocks[i].Type == "dialogue" {
			dialogue = &got.Blocks[i]
		}
	}
	if dialogue == nil {
		t.Fatalf("no dialogue derived from a paired single quote: %+v", got.Blocks)
	}
	if dialogue.Text != "อย่าบอกใคร" {
		t.Errorf("dialogue = %q, want the quoted words alone", dialogue.Text)
	}
	// กระซิบ is a speech verb, so the speaker is claimed with confidence.
	if dialogue.SpeakerID == nil || *dialogue.SpeakerID != "char-aether" || dialogue.Confidence != "high" {
		t.Errorf("speech-verb tie missed: %+v", dialogue)
	}
}

func TestConvertProse_LoneApostropheStaysNarration(t *testing.T) {
	names, labels := castFixture()
	got := convertProse("หนังสือของ O'Brien วางอยู่บนโต๊ะ", names, labels)
	if len(got.Blocks) != 1 || got.Blocks[0].Type != "narration" {
		t.Fatalf("blocks = %+v, want one narration block", got.Blocks)
	}
	if !strings.Contains(got.Blocks[0].Text, "O'Brien") {
		t.Errorf("the apostrophe was eaten: %q", got.Blocks[0].Text)
	}
}

func TestConvertProse_DropsMarkupNeverWords(t *testing.T) {
	names, labels := castFixture()
	got := convertProse(
		"---\n\n**เอเธอร์ (Aether)**\n\n![แบนเนอร์](https://cdn.example/x.png)\n\nจงหลี่ยิ้ม \"ดื่มชาก่อนสิ\"",
		names, labels,
	)
	joined := ""
	for _, block := range got.Blocks {
		joined += block.Text + "\n"
	}
	for _, marker := range []string{"**", "![", "---", "https://"} {
		if strings.Contains(joined, marker) {
			t.Errorf("markup %q leaked into a block", marker)
		}
	}
	if !strings.Contains(joined, "เอเธอร์ (Aether)") {
		t.Error("the heading's words were lost with its markers")
	}
	if !strings.Contains(joined, "ดื่มชาก่อนสิ") {
		t.Error("the dialogue was lost")
	}
}

func TestConvertProse_CharactersListOnlyWhoAppears(t *testing.T) {
	names, labels := castFixture()
	got := convertProse(`เอเธอร์พูด "ไปกันเถอะ"`, names, labels)

	if len(got.Characters) != 1 || got.Characters[0].SpeakerID != "char-aether" {
		t.Errorf("characters = %+v, want เอเธอร์ alone - จงหลี่ never appears", got.Characters)
	}
}
