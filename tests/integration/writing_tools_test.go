package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fictionthai/fictionthai/backend/internal/ai"
)

// 13Y - the writing tools. What these tests defend: the word bank silences
// fandom names automatically (and can be taught), mutes teach a silence, the
// chapter mode changes the rules, character findings cite the sheet and honour
// the evolution marker, the fact book powers a continuity check that only
// looks BACKWARD, search reaches drafts, prefs layer user ← novel, and the
// account master switch turns everything off.

type checkSuggestion struct {
	Type        string   `json:"type"`
	Original    string   `json:"original"`
	Suggestions []string `json:"suggestions"`
	Severity    string   `json:"severity"`
}

type checkBody struct {
	Disabled    bool              `json:"disabled"`
	Suggestions []checkSuggestion `json:"suggestions"`
}

func (e *authEnv) runCheck(t *testing.T, w writer, novelID, mode, text string) checkBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPost, "/api/v1/ai/check",
		map[string]any{"novel": novelID, "mode": mode, "text": text})
	if res.status != http.StatusOK {
		t.Fatalf("check status = %d. body: %s", res.status, res.body)
	}
	return dataOf[checkBody](t, res)
}

func hasSuggestionFor(body checkBody, original string) bool {
	for _, s := range body.Suggestions {
		if strings.Contains(s.Original, original) {
			return true
		}
	}
	return false
}

// The text used throughout: one certain typo (อนุญาติ) plus a fandom name a
// naive spellchecker would also flag.
const toolsSample = "Zhongli บอกว่าเรื่องนี้ไม่ต้องขอ อนุญาติ ใครทั้งนั้น แล้วเดินจากไปอย่างสงบ"

func TestWritingTools_WordBankSilencesTheCastAutomatically(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Bank "), nil))

	// Before any cast exists, the typo is flagged; the Latin name is not a
	// spelling issue for the rule engine either way, so assert on the typo.
	first := env.runCheck(t, w, novel.ID, "standard", toolsSample)
	if !hasSuggestionFor(first, "อนุญาติ") {
		t.Fatalf("expected the certain typo to be flagged: %+v", first.Suggestions)
	}

	// Teach the bank the typo spelling as a fiction term ("เพิ่มคำนี้ในคลัง")
	// - and the flag goes away.
	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/lexicon",
		map[string]any{"term": "อนุญาติ"})
	if res.status != http.StatusOK {
		t.Fatalf("add lexicon term status = %d. body: %s", res.status, res.body)
	}
	after := env.runCheck(t, w, novel.ID, "standard", toolsSample)
	if hasSuggestionFor(after, "อนุญาติ") {
		t.Fatalf("lexicon term still flagged: %+v", after.Suggestions)
	}

	// The bank lists both halves: the custom term and the auto part (the cast).
	env.createCharacter(t, w, novel.ID, "Zhongli")
	bank := dataOf[struct {
		Custom []struct {
			ID   string `json:"id"`
			Term string `json:"term"`
		} `json:"custom"`
		Auto []string `json:"auto"`
	}](t, env.asOwner(t, w, http.MethodGet, "/api/v1/novels/"+novel.ID+"/lexicon"))
	if len(bank.Custom) != 1 || bank.Custom[0].Term != "อนุญาติ" {
		t.Fatalf("custom bank = %+v", bank.Custom)
	}
	foundAuto := false
	for _, term := range bank.Auto {
		if term == "Zhongli" {
			foundAuto = true
		}
	}
	if !foundAuto {
		t.Fatalf("cast name absent from the auto bank: %v", bank.Auto)
	}

	// A stranger cannot read someone's word bank - same 404 as the fiction.
	stranger := writer{webSession: env.registerWeb(t)}
	if res := env.asOwner(t, stranger, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/lexicon"); res.status != http.StatusNotFound {
		t.Fatalf("stranger lexicon read = %d, want 404", res.status)
	}
}

func TestWritingTools_MutesTeachASilence(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Mute "), nil))

	before := env.runCheck(t, w, novel.ID, "standard", toolsSample)
	if !hasSuggestionFor(before, "อนุญาติ") {
		t.Fatalf("expected the typo flagged before muting: %+v", before.Suggestions)
	}

	// "ไม่เตือนแบบนี้อีก" for this fiction.
	res := env.asOwner(t, w, http.MethodPost, "/api/v1/ai/mutes",
		map[string]any{"novel": novel.ID, "kind": "spelling", "term": "อนุญาติ"})
	if res.status != http.StatusNoContent {
		t.Fatalf("add mute status = %d. body: %s", res.status, res.body)
	}
	after := env.runCheck(t, w, novel.ID, "standard", toolsSample)
	if hasSuggestionFor(after, "อนุญาติ") {
		t.Fatalf("muted issue still flagged: %+v", after.Suggestions)
	}

	// The mute is the writer's own: a second writer still sees the flag on
	// THEIR fiction.
	other := env.newWriter(t)
	otherNovel := env.createNovel(t, other, createNovelBody(uniqueName(t, "Other "), nil))
	theirs := env.runCheck(t, other, otherNovel.ID, "standard", toolsSample)
	if !hasSuggestionFor(theirs, "อนุญาติ") {
		t.Fatalf("someone else's mute leaked: %+v", theirs.Suggestions)
	}
}

func TestWritingTools_ChatModeKeepsOnlyConfidentSpelling(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Mode "), nil))

	// Doubled punctuation would be flagged in prose - in chat it is voice.
	text := "จริงดิ!!! ไม่มีทางงง เธอพูดเรื่อง อนุญาติ อะไรนะ"
	prose := env.runCheck(t, w, novel.ID, "standard", text)
	chat := env.runCheck(t, w, novel.ID, "chat", text)

	prosePunct, chatPunct := 0, 0
	for _, s := range prose.Suggestions {
		if s.Type == "punctuation" {
			prosePunct++
		}
	}
	for _, s := range chat.Suggestions {
		if s.Type == "punctuation" {
			chatPunct++
		}
		if s.Severity != "high" {
			t.Fatalf("chat mode let a %s-severity issue through: %+v", s.Severity, s)
		}
	}
	if prosePunct == 0 {
		t.Fatal("expected punctuation flags in prose mode")
	}
	if chatPunct != 0 {
		t.Fatalf("chat mode must not police punctuation, got %d", chatPunct)
	}
	// The certain typo survives even in chat.
	if !hasSuggestionFor(chat, "อนุญาติ") {
		t.Fatalf("high-confidence typo lost in chat mode: %+v", chat.Suggestions)
	}
}

func TestWritingTools_CharacterCheckCitesTheSheet(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "OOC "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	// A fleshed-out character and a name-only one (the "test" case).
	kazuha := env.createCharacter(t, w, novel.ID, "คาซึฮะ")
	res := env.asOwner(t, w, http.MethodPatch, path+"/"+kazuha.ID,
		map[string]any{"traits": []string{"เก็บความรู้สึก"}})
	if res.status != http.StatusOK {
		t.Fatalf("set traits = %d. body: %s", res.status, res.body)
	}
	env.createCharacter(t, w, novel.ID, "จงหลี")

	text := "คาซึฮะ ตะโกน ใส่คนแปลกหน้ากลางตลาดโดยไม่ฟังใครเลยสักคำ"
	type characterCheck struct {
		Total     int `json:"total"`
		Checkable int `json:"checkable"`
		Skipped   []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
		Issues []struct {
			CharacterName string `json:"character_name"`
			Field         string `json:"field"`
			FieldValue    string `json:"field_value"`
			Quote         string `json:"quote"`
			Explanation   string `json:"explanation"`
		} `json:"issues"`
	}
	run := func() characterCheck {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/ai/character-check",
			map[string]any{"novel": novel.ID, "chapter_number": 3, "text": text})
		if res.status != http.StatusOK {
			t.Fatalf("character check status = %d. body: %s", res.status, res.body)
		}
		return dataOf[characterCheck](t, res)
	}

	first := run()
	// Coverage is honest: 1 of 2 checkable, and the skip names the fix.
	if first.Total != 2 || first.Checkable != 1 {
		t.Fatalf("coverage = %d/%d, want 1/2", first.Checkable, first.Total)
	}
	if len(first.Skipped) != 1 || !strings.Contains(first.Skipped[0].Reason, "เพิ่มนิสัย") {
		t.Fatalf("skip reason should point back to the character page: %+v", first.Skipped)
	}
	// The finding cites the sheet field AND quotes the line, phrased as a
	// question ("อาจ"), never a verdict.
	if len(first.Issues) != 1 {
		t.Fatalf("issues = %+v, want exactly one", first.Issues)
	}
	issue := first.Issues[0]
	if issue.Field != "ลักษณะนิสัย" || issue.FieldValue != "เก็บความรู้สึก" ||
		!strings.Contains(issue.Quote, "ตะโกน") || !strings.Contains(issue.Explanation, "อาจ") {
		t.Fatalf("finding lacks its citation: %+v", issue)
	}

	// "ตัวละครเปลี่ยนไปตั้งแต่ตอนนี้": from chapter 2 on, the sheet stops
	// being compared - the check must go quiet for chapter 3.
	res = env.asOwner(t, w, http.MethodPut, "/api/v1/ai/character-evolution",
		map[string]any{"novel": novel.ID, "character_id": kazuha.ID, "from_chapter_number": 2})
	if res.status != http.StatusNoContent {
		t.Fatalf("set evolution status = %d. body: %s", res.status, res.body)
	}
	after := run()
	if len(after.Issues) != 0 {
		t.Fatalf("evolved character still compared: %+v", after.Issues)
	}
	evolved := false
	for _, skip := range after.Skipped {
		if strings.Contains(skip.Reason, "ตอนที่ 2") {
			evolved = true
		}
	}
	if !evolved {
		t.Fatalf("evolution skip not reported: %+v", after.Skipped)
	}
}

func TestWritingTools_CharacterCheckHearsRegisterAndNearbyNames(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Register "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	// The trait never became a chip: it lives in the description, the way
	// writers actually record it. The check must still find it there.
	zhongli := env.createCharacter(t, w, novel.ID, "จงหลี")
	res := env.asOwner(t, w, http.MethodPatch, path+"/"+zhongli.ID,
		map[string]any{"description": "เขาเป็นคนสุขุม พูดจาช้าและหนักแน่นเสมอมา"})
	if res.status != http.StatusOK {
		t.Fatalf("set description = %d. body: %s", res.status, res.body)
	}

	type characterCheck struct {
		Checkable int `json:"checkable"`
		Issues    []struct {
			Field       string `json:"field"`
			FieldValue  string `json:"field_value"`
			Quote       string `json:"quote"`
			Explanation string `json:"explanation"`
		} `json:"issues"`
	}
	run := func(text string) characterCheck {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/ai/character-check",
			map[string]any{"novel": novel.ID, "chapter_number": 1, "text": text})
		if res.status != http.StatusOK {
			t.Fatalf("character check status = %d. body: %s", res.status, res.body)
		}
		return dataOf[characterCheck](t, res)
	}

	// Dialogue names its speaker on the PREVIOUS line, as dialogue does, and
	// the contradiction is the bubbly end-particle จ้า - no single "opposite
	// word" appears anywhere.
	first := run("จงหลีหันมาหาทุกคนอย่างช้า ๆ\n" +
		"\"สวัสดีจ้า วันนี้มากันครบเลยเนอะ\"\n" +
		"ทุกคนมองหน้ากันด้วยความประหลาดใจ")
	if first.Checkable != 1 {
		t.Fatalf("checkable = %d, want 1 (description-only sheet)", first.Checkable)
	}
	if len(first.Issues) != 1 {
		t.Fatalf("issues = %+v, want exactly one", first.Issues)
	}
	issue := first.Issues[0]
	if issue.Field != "ภูมิหลัง" || issue.FieldValue != "สุขุม" ||
		!strings.Contains(issue.Quote, "สวัสดีจ้า") ||
		!strings.Contains(issue.Explanation, "«จ้า»") ||
		!strings.Contains(issue.Explanation, "อาจ") {
		t.Fatalf("register finding lacks its citation: %+v", issue)
	}

	// เจ้า (the pronoun) and จ้าง (to hire) must never trip the particle rule.
	second := run("จงหลีบอกว่าของของเจ้าอยู่ที่นี่ และเขาจะจ้างคนมาดูแลให้อย่างดี")
	if len(second.Issues) != 0 {
		t.Fatalf("เจ้า/จ้าง misread as the particle จ้า: %+v", second.Issues)
	}
}

// The finding a writer reported as wrong: «คุณแกล้งเดินไปตบไหล่เขา» was
// blamed on เอเธอร์ because his name sat on the line above, when the one
// doing the slapping is the reader. A finding must follow the ACTOR.
func TestWritingTools_FindingsFollowTheActorNotTheNearestName(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Actor "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	aether := env.createCharacter(t, w, novel.ID, "เอเธอร์ (Aether)")
	res := env.asOwner(t, w, http.MethodPatch, path+"/"+aether.ID,
		map[string]any{"traits": []string{"มีนิสัยรักการผจญภัย อ่อนโยน เป็นมิตร"}})
	if res.status != http.StatusOK {
		t.Fatalf("set traits = %d. body: %s", res.status, res.body)
	}

	type characterCheck struct {
		Checkable int `json:"checkable"`
		Issues    []struct {
			Quote       string `json:"quote"`
			Explanation string `json:"explanation"`
		} `json:"issues"`
	}
	run := func(text string) characterCheck {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/ai/character-check",
			map[string]any{"novel": novel.ID, "chapter_number": 1, "text": text})
		if res.status != http.StatusOK {
			t.Fatalf("character check status = %d. body: %s", res.status, res.body)
		}
		return dataOf[characterCheck](t, res)
	}

	// The reader acts, the character is only the target: no finding.
	readerActs := run("เอเธอร์หันมามองคุณด้วยแววตาอ่อนโยน\n" +
		"“นายเขินเหรอ?” คุณแกล้งเดินไปตบไหล่เขา ซึ่งแรงตบแบบผู้ชายทำเขาเกือบหน้าคะมำ\n" +
		"“ไม่ได้เขินสักหน่อย” เขาตอบเสียงเบา")
	if readerActs.Checkable != 1 {
		t.Fatalf("checkable = %d, want 1", readerActs.Checkable)
	}
	if len(readerActs.Issues) != 0 {
		t.Fatalf("the reader's action was blamed on the character: %+v", readerActs.Issues)
	}

	// Spoken, not done - a behaviour word inside quotes is speech.
	spoken := run("เอเธอร์ยิ้มบาง ๆ\n“อย่าไปตบหน้าใครนะ” เขาเตือนเพื่อนร่วมทาง")
	if len(spoken.Issues) != 0 {
		t.Fatalf("a word inside quotes was read as an action: %+v", spoken.Issues)
	}

	// Done TO them - the passive marker keeps it off their record.
	passive := run("เอเธอร์ถูกตบหน้าจนล้มลงกับพื้น")
	if len(passive.Issues) != 0 {
		t.Fatalf("a passive clause was read as the character's doing: %+v", passive.Issues)
	}

	// The character themself does it: this one IS the finding, still asking.
	acts := run("เอเธอร์ตบหน้าเด็กคนนั้นอย่างไม่ใยดี")
	if len(acts.Issues) != 1 {
		t.Fatalf("issues = %+v, want exactly one", acts.Issues)
	}
	if !strings.Contains(acts.Issues[0].Explanation, "«ตบหน้า»") ||
		!strings.Contains(acts.Issues[0].Explanation, "อาจ") {
		t.Fatalf("finding lacks its citation or its question: %+v", acts.Issues[0])
	}
}

func TestWritingTools_ModelTierScoresSubtleOOC(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Model "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	zhongli := env.createCharacter(t, w, novel.ID, "จงหลี")
	res := env.asOwner(t, w, http.MethodPatch, path+"/"+zhongli.ID,
		map[string]any{"traits": []string{"สุขุม ใจเย็น พูดจาช้าและหนักแน่น"}})
	if res.status != http.StatusOK {
		t.Fatalf("set traits = %d. body: %s", res.status, res.body)
	}

	// The subtle line: no signal word from the rule table at all.
	subtle := "จงหลีวิ่งพล่านไปทั่วร้านแล้วพูดเร็วรัวจนไม่มีใครฟังทัน"

	// A fake sidecar standing where the WangchanBERTa one runs
	// (docs/AI-CONSISTENCY-MODEL.md): asserts the contract, scores the
	// subtle line as a contradiction.
	var gotPairs []ai.ModelPair
	sidecar := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/consistency" {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Pairs []ai.ModelPair `json:"pairs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		gotPairs = req.Pairs
		results := make([]map[string]any, 0, len(req.Pairs))
		for _, pair := range req.Pairs {
			score := 0.05
			if strings.Contains(pair.Line, "วิ่งพล่าน") {
				score = 0.91
			}
			results = append(results, map[string]any{
				"character_id":   pair.CharacterID,
				"line":           pair.Line,
				"similarity":     0.4,
				"contradiction":  score,
				"labels":         []map[string]any{{"label": "ตื่นตระหนก", "score": 0.72}},
				"profile_labels": []map[string]any{{"label": "ใจเย็น", "score": 0.5}},
			})
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(sidecar.Close)
	env.aiTools.SetModelClient(ai.NewModelClient(sidecar.URL))

	type characterCheck struct {
		Issues []struct {
			Field       string `json:"field"`
			Quote       string `json:"quote"`
			Explanation string `json:"explanation"`
		} `json:"issues"`
	}
	run := func() characterCheck {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/ai/character-check",
			map[string]any{"novel": novel.ID, "chapter_number": 1,
				"text": "จงหลีเดินเข้ามาในร้านน้ำชาตอนบ่าย\n" + subtle + "\nทุกคนหันมามองอย่างงุนงง"})
		if res.status != http.StatusOK {
			t.Fatalf("character check status = %d. body: %s", res.status, res.body)
		}
		return dataOf[characterCheck](t, res)
	}

	first := run()
	// The sidecar received the verbatim profile with the attributed lines.
	if len(gotPairs) == 0 || !strings.Contains(gotPairs[0].Profile, "สุขุม") {
		t.Fatalf("sidecar did not receive the profile: %+v", gotPairs)
	}
	// The subtle line - invisible to the rules - is now a finding, question-
	// phrased, citing the score and the detected tone.
	found := false
	for _, issue := range first.Issues {
		if strings.Contains(issue.Quote, "วิ่งพล่าน") &&
			strings.Contains(issue.Explanation, "91%") &&
			// The tone carries its own confidence - a label a writer can
			// argue with, not a verdict.
			strings.Contains(issue.Explanation, "«ตื่นตระหนก» (72%)") &&
			strings.Contains(issue.Explanation, "อาจ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("model finding missing or uncited: %+v", first.Issues)
	}

	// The sidecar dying degrades to rules-only - never to an error.
	sidecar.Close()
	after := run()
	for _, issue := range after.Issues {
		if strings.Contains(issue.Quote, "วิ่งพล่าน") {
			t.Fatalf("model finding survived a dead sidecar: %+v", after.Issues)
		}
	}
}

// Contradiction saturates near 1.00 across a whole chapter, so the cap must
// spread: one question per character before anyone gets a second, or three
// cards about the talkative one bury the scene that actually went off the
// rails elsewhere.
func TestWritingTools_ModelFindingsSpreadAcrossTheCast(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Spread "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	for _, member := range []string{"จงหลี่", "เวนติ"} {
		created := env.createCharacter(t, w, novel.ID, member)
		res := env.asOwner(t, w, http.MethodPatch, path+"/"+created.ID,
			map[string]any{"traits": []string{"สุขุม ใจเย็น พูดจาช้าและหนักแน่น"}})
		if res.status != http.StatusOK {
			t.Fatalf("set traits for %s = %d. body: %s", member, res.status, res.body)
		}
	}

	// Four off-key lines for จงหลี่, one for เวนติ - all equally contradicted.
	var sb strings.Builder
	for i := 1; i <= 4; i++ {
		fmt.Fprintf(&sb, "จงหลี่กระโดดโลดเต้นไปมาทั่วร้านรอบที่ %d อย่างไม่หยุดหย่อน\n", i)
	}
	sb.WriteString("เวนติกระโดดโลดเต้นไปมาทั่วร้านอย่างไม่หยุดหย่อนเช่นกัน")

	sidecar := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			Pairs []ai.ModelPair `json:"pairs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		results := make([]map[string]any, 0, len(req.Pairs))
		for _, pair := range req.Pairs {
			results = append(results, map[string]any{
				"character_id": pair.CharacterID, "line": pair.Line,
				"similarity": 0.3, "contradiction": 0.99,
				"labels":         []map[string]any{{"label": "ร่าเริง", "score": 0.6}},
				"profile_labels": []map[string]any{{"label": "ใจเย็น", "score": 0.5}},
			})
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(sidecar.Close)
	env.aiTools.SetModelClient(ai.NewModelClient(sidecar.URL))

	type characterCheck struct {
		Issues []struct {
			CharacterName string `json:"character_name"`
		} `json:"issues"`
	}
	res := env.asOwner(t, w, http.MethodPost, "/api/v1/ai/character-check",
		map[string]any{"novel": novel.ID, "chapter_number": 1, "text": sb.String()})
	if res.status != http.StatusOK {
		t.Fatalf("character check status = %d. body: %s", res.status, res.body)
	}
	got := dataOf[characterCheck](t, res)

	named := map[string]int{}
	for _, issue := range got.Issues {
		named[issue.CharacterName]++
	}
	if named["เวนติ"] == 0 {
		t.Fatalf("the quieter character was crowded out entirely: %+v", got.Issues)
	}
	if named["จงหลี่"] > 2 {
		t.Fatalf("one character took the whole cap: %+v", got.Issues)
	}
}

// The model judges the character on the part of the line that is theirs -
// but the CARD must quote the manuscript line it came from, or the underline
// (which finds the quote by text) would have nothing to match.
func TestWritingTools_ModelJudgesTheirActionButQuotesTheLine(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Quote "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	zhongli := env.createCharacter(t, w, novel.ID, "จงหลี่")
	res := env.asOwner(t, w, http.MethodPatch, path+"/"+zhongli.ID,
		map[string]any{"traits": []string{"สุขุม ใจเย็น พูดจาช้าและหนักแน่น"}})
	if res.status != http.StatusOK {
		t.Fatalf("set traits = %d. body: %s", res.status, res.body)
	}

	// Narration with no speech verb: the shout beside it may be anyone's, so
	// only the narration is evidence.
	line := "จงหลี่กระโดดโลดเต้นไปมาทั่วร้านอย่างไม่หยุดหย่อน “ไปให้พ้นเลยนะ!”"

	var gotPairs []ai.ModelPair
	sidecar := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			Pairs []ai.ModelPair `json:"pairs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		gotPairs = req.Pairs
		results := make([]map[string]any, 0, len(req.Pairs))
		for _, pair := range req.Pairs {
			results = append(results, map[string]any{
				"character_id": pair.CharacterID, "line": pair.Line,
				"similarity": 0.3, "contradiction": 0.95,
				"labels":         []map[string]any{{"label": "ร่าเริง", "score": 0.58}},
				"profile_labels": []map[string]any{{"label": "ใจเย็น", "score": 0.5}},
			})
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(sidecar.Close)
	env.aiTools.SetModelClient(ai.NewModelClient(sidecar.URL))

	type characterCheck struct {
		Issues []struct {
			Quote       string `json:"quote"`
			Explanation string `json:"explanation"`
		} `json:"issues"`
	}
	res = env.asOwner(t, w, http.MethodPost, "/api/v1/ai/character-check",
		map[string]any{"novel": novel.ID, "chapter_number": 1, "text": line})
	if res.status != http.StatusOK {
		t.Fatalf("character check status = %d. body: %s", res.status, res.body)
	}
	got := dataOf[characterCheck](t, res)

	if len(gotPairs) != 1 {
		t.Fatalf("pairs = %+v, want exactly one", gotPairs)
	}
	if strings.Contains(gotPairs[0].Line, "ไปให้พ้น") {
		t.Errorf("the model was shown speech that is not tied to the character: %q", gotPairs[0].Line)
	}
	if !strings.Contains(gotPairs[0].Line, "กระโดดโลดเต้น") {
		t.Errorf("the model was not shown the character's action: %q", gotPairs[0].Line)
	}
	if len(got.Issues) != 1 {
		t.Fatalf("issues = %+v, want exactly one", got.Issues)
	}
	// The card quotes the LINE, so the manuscript can underline it.
	if got.Issues[0].Quote != line {
		t.Errorf("quote = %q, want the manuscript line %q", got.Issues[0].Quote, line)
	}
	// The card names BOTH readings - the sheet's tone and the line's.
	if !strings.Contains(got.Issues[0].Explanation, "«ใจเย็น»") ||
		!strings.Contains(got.Issues[0].Explanation, "«ร่าเริง»") {
		t.Errorf("finding does not name the clash: %q", got.Issues[0].Explanation)
	}
}

// The finding a writer rejected outright: «จริงจัง» flagged on a สุขุม
// character is the tone their sheet ASKED for. A high contradiction score
// with no nameable clash between the two readings is not a finding.
func TestWritingTools_ModelStaysQuietWhenTheToneMatchesTheSheet(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Tone "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	zhongli := env.createCharacter(t, w, novel.ID, "จงหลี่")
	res := env.asOwner(t, w, http.MethodPatch, path+"/"+zhongli.ID,
		map[string]any{"traits": []string{"มีนิสัยสุภาพ สุขุม และสง่างาม"}})
	if res.status != http.StatusOK {
		t.Fatalf("set traits = %d. body: %s", res.status, res.body)
	}

	// The sidecar is as sure as it ever gets - and reads BOTH sides as the
	// same composed register.
	sidecar := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			Pairs []ai.ModelPair `json:"pairs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		results := make([]map[string]any, 0, len(req.Pairs))
		for _, pair := range req.Pairs {
			results = append(results, map[string]any{
				"character_id": pair.CharacterID, "line": pair.Line,
				"similarity": 0.4, "contradiction": 0.99,
				"labels":         []map[string]any{{"label": "จริงจัง", "score": 0.66}},
				"profile_labels": []map[string]any{{"label": "ใจเย็น", "score": 0.5}, {"label": "สุภาพ", "score": 0.3}},
			})
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(sidecar.Close)
	env.aiTools.SetModelClient(ai.NewModelClient(sidecar.URL))

	type characterCheck struct {
		Issues []struct {
			Explanation string `json:"explanation"`
		} `json:"issues"`
	}
	res = env.asOwner(t, w, http.MethodPost, "/api/v1/ai/character-check",
		map[string]any{"novel": novel.ID, "chapter_number": 1,
			"text": "จงหลี่หันกลับมาแล้วมองคุณตั้งแต่หัวจรดเท้า ก่อนจะพยักหน้าอย่างจริงจัง"})
	if res.status != http.StatusOK {
		t.Fatalf("character check status = %d. body: %s", res.status, res.body)
	}
	if got := dataOf[characterCheck](t, res); len(got.Issues) != 0 {
		t.Fatalf("a tone the sheet asked for was flagged as a conflict: %+v", got.Issues)
	}
}

// The case that made the whole check blind to a real writer's scene: the
// sheet says «จงหลี่ (Zhongli)», the prose says just «จงหลี่». Attribution
// must match name VARIANTS, the pair cap must keep the NEWEST lines (the
// scene being written), and the sidecar's queue depth must reach the client
// as model_pending so the panel knows to ask again.
func TestWritingTools_ModelTierMatchesAnnotatedSheetNames(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Alias "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	zhongli := env.createCharacter(t, w, novel.ID, "จงหลี่ (Zhongli)")
	res := env.asOwner(t, w, http.MethodPatch, path+"/"+zhongli.ID,
		map[string]any{"traits": []string{"มีนิสัยสุภาพ สุขุม รอบรู้ และรักสันโดษ"}})
	if res.status != http.StatusOK {
		t.Fatalf("set traits = %d. body: %s", res.status, res.body)
	}

	// 45 attributed lines - over the 40-pair cap - with the out-of-character
	// one LAST, where the writer is actually typing.
	var sb strings.Builder
	for i := 1; i <= 44; i++ {
		fmt.Fprintf(&sb, "จงหลี่พูดประโยคที่ %d อย่างสงบ\n", i)
	}
	ooc := "จงหลี่วิ่งพล่านไปทั่วร้านแล้วหัวเราะเสียงดังลั่น"
	sb.WriteString(ooc)

	var gotPairs []ai.ModelPair
	sidecar := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			Pairs []ai.ModelPair `json:"pairs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		gotPairs = req.Pairs
		results := make([]map[string]any, 0, len(req.Pairs))
		for _, pair := range req.Pairs {
			score := 0.02
			if strings.Contains(pair.Line, "วิ่งพล่าน") {
				score = 0.93
			}
			results = append(results, map[string]any{
				"character_id": pair.CharacterID, "line": pair.Line,
				"similarity": 0.4, "contradiction": score,
				"labels":         []map[string]any{{"label": "ร่าเริง", "score": 0.9}},
				"profile_labels": []map[string]any{{"label": "ใจเย็น", "score": 0.5}},
			})
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"results": results, "pending": 2})
	}))
	t.Cleanup(sidecar.Close)
	env.aiTools.SetModelClient(ai.NewModelClient(sidecar.URL))

	type characterCheck struct {
		Issues []struct {
			CharacterName string `json:"character_name"`
			Quote         string `json:"quote"`
			Explanation   string `json:"explanation"`
		} `json:"issues"`
		ModelPending int `json:"model_pending"`
	}
	res = env.asOwner(t, w, http.MethodPost, "/api/v1/ai/character-check",
		map[string]any{"novel": novel.ID, "chapter_number": 1, "text": sb.String()})
	if res.status != http.StatusOK {
		t.Fatalf("character check status = %d. body: %s", res.status, res.body)
	}
	got := dataOf[characterCheck](t, res)

	// Attribution matched the prose variant, not the annotated sheet name.
	if len(gotPairs) != 40 {
		t.Fatalf("pairs sent = %d, want the cap of 40", len(gotPairs))
	}
	if gotPairs[0].Name != "จงหลี่" {
		t.Fatalf("premise name = %q, want the primary variant จงหลี่", gotPairs[0].Name)
	}
	// Newest lines first: the cap dropped the OPENING, kept the scene's end.
	if !strings.Contains(gotPairs[0].Line, "วิ่งพล่าน") {
		t.Fatalf("first pair = %q, want the newest (OOC) line", gotPairs[0].Line)
	}
	for _, pair := range gotPairs {
		if strings.Contains(pair.Line, "ประโยคที่ 1 ") {
			t.Fatalf("the cap kept the chapter opening instead of the newest lines")
		}
	}

	// The finding cites the ANNOTATED sheet name the writer will recognise.
	found := false
	for _, issue := range got.Issues {
		if strings.Contains(issue.Quote, "วิ่งพล่าน") &&
			issue.CharacterName == "จงหลี่ (Zhongli)" &&
			strings.Contains(issue.Explanation, "93%") {
			found = true
		}
	}
	if !found {
		t.Fatalf("alias-attributed model finding missing: %+v", got.Issues)
	}
	// The queue depth travels through, so the panel keeps following up.
	if got.ModelPending != 2 {
		t.Fatalf("model_pending = %d, want 2", got.ModelPending)
	}
}

func TestWritingTools_FactBookDrivesTheContinuityCheck(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Facts "), nil))

	one := env.createChapter(t, w, novel.ID, map[string]any{"content": "ตอนที่หนึ่ง"})
	two := env.createChapter(t, w, novel.ID, map[string]any{"content": "ตอนที่สอง"})

	putFacts := func(chapterID string, facts []map[string]string) {
		res := env.asOwner(t, w, http.MethodPut,
			fmt.Sprintf("/api/v1/novels/%s/chapters/%s/facts", novel.ID, chapterID),
			map[string]any{"facts": facts})
		if res.status != http.StatusOK {
			t.Fatalf("put facts status = %d. body: %s", res.status, res.body)
		}
	}
	putFacts(one.ID, []map[string]string{{"label": "ดาบของคาซึฮะ", "value": "อยู่กับตัวเขา"}})
	putFacts(two.ID, []map[string]string{{"label": "ดาบของคาซึฮะ", "value": "หายไปแล้ว"}})

	// Continuity defaults OFF (the expensive one) - the check answers quiet.
	type continuity struct {
		Checked bool `json:"checked"`
		Issues  []struct {
			Label           string `json:"label"`
			ThisValue       string `json:"this_value"`
			PreviousValue   string `json:"previous_value"`
			PreviousChapter int    `json:"previous_chapter"`
		} `json:"issues"`
	}
	runContinuity := func(chapterID string) continuity {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/ai/continuity",
			map[string]any{"novel": novel.ID, "chapter_id": chapterID})
		if res.status != http.StatusOK {
			t.Fatalf("continuity status = %d. body: %s", res.status, res.body)
		}
		return dataOf[continuity](t, res)
	}
	if got := runContinuity(two.ID); got.Checked {
		t.Fatalf("continuity ran while switched off: %+v", got)
	}

	// The writer opts in (novel tier) - now the conflict surfaces, citing
	// both values and the chapter the earlier one came from.
	res := env.asOwner(t, w, http.MethodPut, "/api/v1/ai/prefs",
		map[string]any{"novel": novel.ID, "prefs": map[string]any{"continuity": true}})
	if res.status != http.StatusOK {
		t.Fatalf("set prefs status = %d. body: %s", res.status, res.body)
	}
	conflict := runContinuity(two.ID)
	if !conflict.Checked || len(conflict.Issues) != 1 {
		t.Fatalf("continuity = %+v, want one conflict", conflict)
	}
	if conflict.Issues[0].PreviousChapter != 1 ||
		conflict.Issues[0].PreviousValue != "อยู่กับตัวเขา" {
		t.Fatalf("conflict lacks its source: %+v", conflict.Issues[0])
	}

	// Only BACKWARD: chapter 1 checked against nothing earlier finds nothing.
	if got := runContinuity(one.ID); len(got.Issues) != 0 {
		t.Fatalf("a later chapter was treated as a contradiction: %+v", got.Issues)
	}
}

func TestWritingTools_SearchReachesDrafts(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Search "), nil))

	env.createChapter(t, w, novel.ID, map[string]any{
		"title":   "คืนฝนแรก",
		"content": "เขายื่นร่มสีแดงให้เธอ ก่อนหายเข้าไปในสายฝน",
	})
	env.createChapter(t, w, novel.ID, map[string]any{
		"presentation_format": "chat",
		"messages": []map[string]any{{
			"speaker_name": "มายด์", "message_type": "message",
			"content": "ร่มคันนั้นยังอยู่กับฉันนะ",
		}},
	})

	type hits struct {
		Results []struct {
			Where   string `json:"where"`
			Snippet string `json:"snippet"`
			Status  string `json:"status"`
		} `json:"results"`
	}
	res := env.asOwner(t, w, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/search?q="+url.QueryEscape("ร่ม"))
	if res.status != http.StatusOK {
		t.Fatalf("search status = %d. body: %s", res.status, res.body)
	}
	found := dataOf[hits](t, res)
	if len(found.Results) != 2 {
		t.Fatalf("results = %+v, want prose + chat", found.Results)
	}
	wheres := map[string]bool{}
	for _, hit := range found.Results {
		wheres[hit.Where] = true
		if hit.Status != "draft" {
			t.Fatalf("drafts are the point of studio search: %+v", hit)
		}
		if !strings.Contains(hit.Snippet, "ร่ม") {
			t.Fatalf("snippet does not show the match: %q", hit.Snippet)
		}
	}
	if !wheres["prose"] || !wheres["chat"] {
		t.Fatalf("expected hits in prose and chat, got %v", wheres)
	}

	// A stranger gets the private fiction's 404.
	stranger := writer{webSession: env.registerWeb(t)}
	if res := env.asOwner(t, stranger, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/search?q=x"); res.status != http.StatusNotFound {
		t.Fatalf("stranger search = %d, want 404", res.status)
	}
}

func TestWritingTools_TheMasterSwitchTurnsEverythingOff(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Master "), nil))

	// Account-level: ปิดผู้ช่วยทั้งหมด (13Y §12).
	res := env.asOwner(t, w, http.MethodPut, "/api/v1/ai/prefs",
		map[string]any{"prefs": map[string]any{"assistant": false}})
	if res.status != http.StatusOK {
		t.Fatalf("set master switch status = %d. body: %s", res.status, res.body)
	}

	check := env.runCheck(t, w, novel.ID, "standard", toolsSample)
	if !check.Disabled || len(check.Suggestions) != 0 {
		t.Fatalf("master switch ignored: %+v", check)
	}

	// The prefs endpoint reports the layered truth.
	prefs := dataOf[struct {
		Effective struct {
			Assistant  bool `json:"assistant"`
			Spell      bool `json:"spell"`
			Continuity bool `json:"continuity"`
		} `json:"effective"`
	}](t, env.asOwner(t, w, http.MethodGet, "/api/v1/ai/prefs?novel="+novel.ID))
	if prefs.Effective.Assistant {
		t.Fatal("effective prefs claim the assistant is on")
	}
	if prefs.Effective.Continuity {
		t.Fatal("continuity must default off")
	}
}

func TestWritingTools_PrecheckBundlesTheRound(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Precheck "), nil))

	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": toolsSample})
	tiny := env.createChapter(t, w, novel.ID, map[string]any{"content": "สั้น"})

	type precheck struct {
		Skipped    bool `json:"skipped"`
		SpellCount int  `json:"spell_count"`
		IssueCount int  `json:"issue_count"`
	}
	run := func(chapterID string) precheck {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/ai/precheck",
			map[string]any{"novel": novel.ID, "chapter_id": chapterID})
		if res.status != http.StatusOK {
			t.Fatalf("precheck status = %d. body: %s", res.status, res.body)
		}
		return dataOf[precheck](t, res)
	}

	full := run(chapter.ID)
	if full.Skipped || full.SpellCount == 0 || full.IssueCount < full.SpellCount {
		t.Fatalf("precheck = %+v, want the typo counted", full)
	}
	// A ~one-word chapter is not checked at all (13Y §12).
	if short := run(tiny.ID); !short.Skipped {
		t.Fatalf("short chapter was checked: %+v", short)
	}
}
