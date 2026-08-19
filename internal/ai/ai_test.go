package ai

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testService() *Service {
	return &Service{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: Config{Enabled: true, MaxInputRunes: 1000, DailyQuota: 10},
	}
}

func TestFeature_Vocabulary(t *testing.T) {
	if !FeatureSpellCheck.Valid() || !FeatureRepetition.Valid() || !FeatureSummary.Valid() {
		t.Fatal("enabled features must be valid")
	}
	if Feature("grammar_check").Valid() {
		t.Fatal("a feature not yet enabled must be invalid")
	}
	if !FeatureSummary.Async() {
		t.Fatal("summary must be async")
	}
	if FeatureSpellCheck.Async() || FeatureRepetition.Async() {
		t.Fatal("spell_check and repetition must be synchronous")
	}
	if got := FeatureSpellCheck.analyzeKinds(); len(got) != 2 {
		t.Fatalf("spell_check should run spelling+punctuation, got %v", got)
	}
}

func TestFailureKind_Retryable(t *testing.T) {
	retryable := []FailureKind{FailUnavailable, FailTimeout, FailRateLimited}
	for _, k := range retryable {
		if !k.Retryable() {
			t.Errorf("%s should be retryable", k)
		}
	}
	permanent := []FailureKind{FailMalformed, FailRefused, FailOversize, FailInternal}
	for _, k := range permanent {
		if k.Retryable() {
			t.Errorf("%s should NOT be retryable", k)
		}
	}
}

func TestFailureKindOf(t *testing.T) {
	if got := failureKindOf(NewProviderError(FailTimeout, errors.New("boom"))); got != FailTimeout {
		t.Errorf("classified error = %s, want %s", got, FailTimeout)
	}
	// A plain error is never surfaced verbatim; it classifies as a generic
	// provider error.
	if got := failureKindOf(errors.New("raw")); got != FailInternal {
		t.Errorf("unclassified error = %s, want %s", got, FailInternal)
	}
}

func TestLocalProvider_AnalyzeMapsIssues(t *testing.T) {
	p := NewLocalProvider()
	res, err := p.Analyze(context.Background(), AnalyzeInput{Text: "เเมว"})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if res.Model == "" {
		t.Error("model identifier should be set")
	}
	var found bool
	for _, s := range res.Suggestions {
		if s.Type == SuggestionSpelling && s.Severity == "high" {
			found = true
			if s.Confidence < 0.8 {
				t.Errorf("high severity should map to a high numeric confidence, got %v", s.Confidence)
			}
			if len(s.Suggestions) == 0 || s.Suggestions[0] != "แ" {
				t.Errorf("expected suggestion แ, got %v", s.Suggestions)
			}
		}
	}
	if !found {
		t.Fatalf("expected a high-severity spelling suggestion, got %+v", res.Suggestions)
	}
}

func TestLocalProvider_SummarizeNeverEmptyForRealText(t *testing.T) {
	p := NewLocalProvider()
	res, err := p.Summarize(context.Background(), SummarizeInput{
		Text: "แมวนั่งบนเสื่อ. หมาวิ่งในสวน. แมวกระโดดขึ้นโต๊ะ. นกบินผ่าน.",
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if res.Summary == "" {
		t.Fatal("summary of real text should not be empty")
	}
}

func TestValidateInline_DropsInvalid(t *testing.T) {
	s := testService()
	const textLen = 10
	in := []InlineSuggestion{
		{Type: SuggestionSpelling, Start: 0, End: 3, Original: "abc"},         // ok
		{Type: SuggestionSpelling, Start: -1, End: 2, Original: "x"},          // bad start
		{Type: SuggestionSpelling, Start: 5, End: 20, Original: "y"},          // end past text
		{Type: SuggestionSpelling, Start: 8, End: 4, Original: "z"},           // end < start
		{Type: "malware", Start: 0, End: 1, Original: "q"},                    // unknown type
		{Type: SuggestionRepetition, Start: 0, End: 2, Original: bigString()}, // oversize original
	}
	out := s.validateInline(in, textLen)
	if len(out) != 1 {
		t.Fatalf("expected exactly one valid suggestion to survive, got %d: %+v", len(out), out)
	}
	if out[0].Original != "abc" {
		t.Errorf("wrong suggestion survived: %+v", out[0])
	}
}

func TestValidateInline_CapsCount(t *testing.T) {
	s := testService()
	in := make([]InlineSuggestion, maxInlineSuggestions+50)
	for i := range in {
		in[i] = InlineSuggestion{Type: SuggestionSpelling, Start: 0, End: 1, Original: "a"}
	}
	if got := len(s.validateInline(in, 10)); got != maxInlineSuggestions {
		t.Fatalf("count not capped: got %d, want %d", got, maxInlineSuggestions)
	}
}

func TestToNewSuggestions(t *testing.T) {
	chapter := uuid.New()
	inline := []InlineSuggestion{
		{Type: SuggestionSpelling, Original: "เเ", Suggestions: []string{"แ"}, Explanation: "why"},
		{Type: SuggestionRepetition, Original: "มอง", Suggestions: nil, Explanation: ""},
	}
	got := toNewSuggestions(chapter, inline)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].SuggestedText == nil || *got[0].SuggestedText != "แ" {
		t.Errorf("first suggestion should carry suggested text")
	}
	if got[0].Explanation == nil {
		t.Errorf("first suggestion should carry explanation")
	}
	// A flag-only repetition persists with no suggested text (docs/12 §10).
	if got[1].SuggestedText != nil {
		t.Errorf("flag-only suggestion should have null suggested text")
	}
	if got[1].Explanation != nil {
		t.Errorf("empty explanation should be null")
	}
	if got[0].ChapterID != chapter {
		t.Errorf("chapter id not propagated")
	}
}

func bigString() string {
	b := make([]rune, maxSuggestionRunes+1)
	for i := range b {
		b[i] = 'ก'
	}
	return string(b)
}

func TestNameVariants(t *testing.T) {
	cases := []struct {
		name string
		want []string
	}{
		// The annotated sheet name that made the whole check blind: prose
		// says «จงหลี่», the sheet says «จงหลี่ (Zhongli)».
		{"จงหลี่ (Zhongli)", []string{"จงหลี่", "Zhongli"}},
		{"มายา / Maya", []string{"มายา", "Maya"}},
		{"จงหลี", []string{"จงหลี"}},
		// Full-width parentheses, as Thai IMEs sometimes produce.
		{"เวนติ（Venti）", []string{"เวนติ", "Venti"}},
		// A one-rune alias would match everywhere - dropped.
		{"มินตรา (ม)", []string{"มินตรา"}},
		// Degenerate names still yield something to match on.
		{"  จันทร์  ", []string{"จันทร์"}},
	}
	for _, tc := range cases {
		got := nameVariants(tc.name)
		if len(got) != len(tc.want) {
			t.Errorf("nameVariants(%q) = %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("nameVariants(%q) = %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

func TestAttributeLines_TheActorOwnsTheLine(t *testing.T) {
	names := map[string][]string{
		"aether":  nameVariants("เอเธอร์ (Aether)"),
		"zhongli": nameVariants("จงหลี่ (Zhongli)"),
	}
	lines := []string{
		"เอเธอร์หันมามองคุณด้วยแววตาอ่อนโยน",
		// The reader does the slapping here; the character is only its
		// target. Attributing this line to เอเธอร์ was the false finding.
		"“นายเขินเหรอ?” คุณแกล้งเดินไปตบไหล่เขา ซึ่งแรงตบแบบผู้ชายทำเขาเกือบหน้าคะมำ",
		"“ไม่ได้เขินสักหน่อย” เขาตอบเสียงเบา",
		"",
		"จงหลี่วางถ้วยชาลงเบา ๆ",
	}
	actors := attributeLines(lines, names)
	want := []string{"aether", "", "aether", "", "zhongli"}
	for i := range want {
		if actors[i].actor != want[i] {
			t.Errorf("line %d actor = %q, want %q (%s)", i, actors[i].actor, want[i], lines[i])
		}
	}
}

func TestAttributeLines_DialogueTakesOnlyItsOwnNarration(t *testing.T) {
	names := map[string][]string{"z": {"จงหลี่"}}
	lines := []string{
		"จงหลี่เบิกตากว้าง ก่อนจะหัวเราะออกมาเสียงดัง",
		"“ฮ่า ๆ! นี่มันอะไรกันเนี่ย!”",
		// One reply further down the exchange the speaker has alternated -
		// guessing here would blame the wrong person, so it stays unowned.
		"“คุณจะหัวเราะทำไมเล่า!”",
	}
	actors := attributeLines(lines, names)
	if actors[1].actor != "z" {
		t.Errorf("dialogue right under its narration = %q, want z", actors[1].actor)
	}
	if actors[2].actor != "" {
		t.Errorf("second reply in an exchange = %q, want unattributed", actors[2].actor)
	}
}

func TestAttributeLines_SpeechAddressingTheCharacterIsNotTheirs(t *testing.T) {
	names := map[string][]string{"v": {"เวนติ"}}
	lines := []string{
		"เวนติหัวเราะคิกคักอย่างสนุกสนาน",
		// Somebody is shouting AT เวนติ here - a speaker does not call
		// themself by name.
		"“เวนติ! เลิกแกล้งแล้วไปหายาแก้มาเดี๋ยวนี้เลย!”",
	}
	actors := attributeLines(lines, names)
	if actors[0].actor != "v" {
		t.Errorf("narration actor = %q, want v", actors[0].actor)
	}
	if actors[1].actor != "" {
		t.Errorf("speech addressing เวนติ = %q, want unattributed", actors[1].actor)
	}
}

func TestSplitQuoted_SeparatesSpeechFromAction(t *testing.T) {
	narration, dialogue := splitQuoted(`เขายิ้ม “อย่าตบหน้าใครนะ” ก่อนจะเดินจากไป`)
	if strings.Contains(narration, "ตบหน้า") {
		t.Errorf("a word inside quotes must not read as an action: %q", narration)
	}
	if !strings.Contains(dialogue, "อย่าตบหน้าใครนะ") {
		t.Errorf("dialogue = %q, want the quoted utterance", dialogue)
	}
	if !strings.Contains(narration, "เดินจากไป") {
		t.Errorf("narration = %q, want the unquoted action", narration)
	}
}

func TestActedUpon_PassiveIsNotTheCharactersDoing(t *testing.T) {
	if !actedUpon("เอเธอร์ถูกตบหน้าจนล้ม", "ตบหน้า") {
		t.Error("«ถูก» marks the character as the target, not the actor")
	}
	if actedUpon("เอเธอร์ตบหน้าเด็กคนนั้น", "ตบหน้า") {
		t.Error("a plain active clause must stay a finding")
	}
}
