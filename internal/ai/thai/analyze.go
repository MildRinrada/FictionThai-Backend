package thai

import (
	"fmt"
	"sort"
	"strings"
)

// repetitionThreshold is how many times a token must recur before it is flagged
// (docs/12 §10 shows a word appearing ×4). Kept as a constant so the rule is
// one edit away from tuning.
const repetitionThreshold = 4

// maxRepetitionIssues caps repetition output so a pathological input cannot
// flood the editor (docs/12 §41 "Suggestions should not visually overwhelm").
const maxRepetitionIssues = 20

// commonTypos is a curated map of unambiguous Thai misspellings to their
// accepted form. A real spellchecker uses a lexicon (PyThaiNLP, docs/12 §256)
// that would replace this behind the same package. Every entry here is a
// well-established correction, not a judgement call, so it can carry High
// confidence without a dictionary (docs/12 §8).
//
// Matching is raw substring search (Thai has no word boundaries), so an entry
// is admitted ONLY if it cannot occur inside a correctly spelled word. That
// bar excludes classics like กฏ→กฎ (inside ปรากฏ), เซ็นต์→เซ็น (inside
// เปอร์เซ็นต์), เท่ห์→เท่ (inside สนเท่ห์) and สร้างสรร→สร้างสรรค์ (inside
// สร้างสรรค์ itself). Deliberate colloquialisms (เค้า, มั้ย, ยังไง) are NOT
// typos and must never appear here - fiction dialogue owns its register.
var commonTypos = map[string]string{
	// Consonant/vowel confusions.
	"อนุญาติ":     "อนุญาต",
	"ผลลัพท์":     "ผลลัพธ์",
	"สังเกตุ":     "สังเกต",
	"ปรากฎ":       "ปรากฏ",
	"ลายเซ็นต์":   "ลายเซ็น",
	"โอกาศ":       "โอกาส",
	"อากาส":       "อากาศ",
	"คำนวน":       "คำนวณ",
	"รสชาด":       "รสชาติ",
	"ศรีษะ":       "ศีรษะ",
	"ธุระกิจ":     "ธุรกิจ",
	"โทรศัพย์":    "โทรศัพท์",
	"ประสพการณ์":  "ประสบการณ์",
	"อนุเสาวรีย์": "อนุสาวรีย์",
	"กระเพรา":     "กะเพรา",
	"ผูกพันธ์":    "ผูกพัน",
	"สมดุลย์":     "สมดุล",
	"สาบแช่ง":     "สาปแช่ง",
	"บรรได":       "บันได",
	"บรรทึก":      "บันทึก",
	"กรกฏาคม":     "กรกฎาคม",
	"พฤษจิกายน":   "พฤศจิกายน",
	// Particles written with an impossible tone mark or the wrong particle.
	// (ไม้ตรี/ไม้จัตวาบนอักษรต่ำเป็นรูปที่สะกดไม่ได้ จึงชี้ได้อย่างมั่นใจ)
	"นะค่ะ": "นะคะ",
	"ค๊ะ":   "คะ",
	"น๊ะ":   "นะ",
	"มั๊ย":  "มั้ย",
	// Loanwords with one Royal-Institute-established spelling.
	"อีเมล์":     "อีเมล",
	"ไอศครีม":    "ไอศกรีม",
	"เว็ป":       "เว็บ",
	"แก๊งค์":     "แก๊ง",
	"เปอร์เซนต์": "เปอร์เซ็นต์",
	"โน๊ต":       "โน้ต",
	"แบงค์":      "แบงก์",
	"ลิฟท์":      "ลิฟต์",
}

// Analyze runs the requested rule kinds over text and returns the issues sorted
// by position. With no kinds it runs all three MVP checks (docs/12 §40:
// spelling, punctuation, repetition).
//
// text is treated purely as DATA: it is scanned, never interpreted as an
// instruction (docs/12 §34). There is no prompt and no model here to subvert.
func Analyze(text string, kinds ...IssueKind) []Issue {
	want := map[IssueKind]bool{}
	if len(kinds) == 0 {
		want[IssueSpelling], want[IssuePunctuation], want[IssueRepetition] = true, true, true
	} else {
		for _, k := range kinds {
			want[k] = true
		}
	}

	var issues []Issue
	if want[IssueSpelling] {
		issues = append(issues, SpellCheck(text)...)
	}
	if want[IssuePunctuation] {
		issues = append(issues, Punctuation(text)...)
	}
	if want[IssueRepetition] {
		issues = append(issues, Repetition(text)...)
	}
	// Polish is OPT-IN only (13Y §7): it never runs implicitly, so the
	// original three-check default is unchanged for existing callers.
	if want[IssuePolish] {
		issues = append(issues, Polish(text)...)
	}

	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Start != issues[j].Start {
			return issues[i].Start < issues[j].Start
		}
		return issues[i].End < issues[j].End
	})
	return issues
}

// SpellCheck detects likely Thai spelling problems with rule-based checks that
// need no dictionary (docs/12 §7 Layer 1, §8). Everything is a SUGGESTION; a
// probable-intentional case (emphatic repetition) is flag-only and Low
// confidence, honouring docs/12 §11 (informal writing is not automatically
// wrong).
func SpellCheck(text string) []Issue {
	runes := []rune(text)
	var issues []Issue

	// 1. Double sara-e "เเ" (two U+0E40) is almost always a mistyped sara-ae
	//    "แ" (U+0E41). High confidence, concrete fix.
	for i := 0; i < len(runes)-1; i++ {
		if runes[i] == 0x0E40 && runes[i+1] == 0x0E40 {
			end := i + 2
			for end < len(runes) && runes[end] == 0x0E40 {
				end++
			}
			issues = append(issues, Issue{
				Kind: IssueSpelling, Start: i, End: end,
				Original:    string(runes[i:end]),
				Suggestions: []string{"แ"},
				Confidence:  High,
				Explanation: `พบสระเอสองตัวติดกัน ("เเ") อาจตั้งใจพิมพ์เป็นสระแอ ("แ")`,
			})
			i = end - 1
		}
	}

	// 2. Stacked tone marks ("ที่่") - two or more tone marks in a row cannot be
	//    correct; keep the first.
	for i := 0; i < len(runes); i++ {
		if !isThaiToneMark(runes[i]) {
			continue
		}
		end := i + 1
		for end < len(runes) && isThaiToneMark(runes[end]) {
			end++
		}
		if end-i >= 2 {
			issues = append(issues, Issue{
				Kind: IssueSpelling, Start: i, End: end,
				Original:    string(runes[i:end]),
				Suggestions: []string{string(runes[i])},
				Confidence:  High,
				Explanation: "พบเครื่องหมายวรรณยุกต์ซ้อนกัน ควรมีเพียงตัวเดียว",
			})
		}
		i = end - 1
	}

	// 3. Orphaned combining marks - a tone/above/below mark that does not sit on
	//    a consonant (or a mark stacked on one). Only the unambiguous cases are
	//    flagged, to keep precision high; it is flag-only, since silently
	//    deleting a mark would itself be a modification (docs/12 §15).
	for i := 0; i < len(runes); i++ {
		if !isThaiUpperLowerMark(runes[i]) {
			continue
		}
		if i > 0 && isThaiToneMark(runes[i]) && isThaiToneMark(runes[i-1]) {
			continue // already covered by the stacked-tone rule
		}
		orphan := false
		switch {
		case i == 0:
			orphan = true
		case isThaiConsonant(runes[i-1]) || isThaiUpperLowerMark(runes[i-1]):
			orphan = false
		case isThaiLeadingVowel(runes[i-1]):
			orphan = true
		case !isThai(runes[i-1]) || isThaiDigit(runes[i-1]):
			orphan = true
		}
		if orphan {
			issues = append(issues, Issue{
				Kind: IssueSpelling, Start: i, End: i + 1,
				Original:    string(runes[i]),
				Confidence:  Medium,
				Explanation: "เครื่องหมายสระ/วรรณยุกต์นี้อาจไม่มีพยัญชนะรองรับ",
			})
		}
	}

	// 4. A letter repeated four or more times is usually emphatic dialogue, not
	//    a typo (docs/12 §11) - Low confidence, flag-only.
	for i := 0; i < len(runes); i++ {
		if !isRepeatableLetter(runes[i]) {
			continue
		}
		end := i + 1
		for end < len(runes) && runes[end] == runes[i] {
			end++
		}
		if end-i >= 4 {
			issues = append(issues, Issue{
				Kind: IssueSpelling, Start: i, End: end,
				Original:    string(runes[i:end]),
				Confidence:  Low,
				Explanation: "มีตัวอักษรซ้ำหลายตัว อาจตั้งใจเน้นเสียง หรืออาจพิมพ์เกิน",
			})
		}
		i = end - 1
	}

	// 5. Curated common misspellings.
	for bad, good := range commonTypos {
		for _, start := range findAll(runes, []rune(bad)) {
			issues = append(issues, Issue{
				Kind: IssueSpelling, Start: start, End: start + len([]rune(bad)),
				Original:    bad,
				Suggestions: []string{good},
				Confidence:  High,
				Explanation: fmt.Sprintf(`"%s" น่าจะสะกดว่า "%s"`, bad, good),
			})
		}
	}

	return issues
}

// Punctuation applies deterministic punctuation rules (docs/12 §7 Layer 1).
func Punctuation(text string) []Issue {
	runes := []rune(text)
	var issues []Issue

	emit := func(start, end int, original string, suggestions []string, conf Confidence, why string) {
		issues = append(issues, Issue{
			Kind: IssuePunctuation, Start: start, End: end,
			Original: original, Suggestions: suggestions, Confidence: conf, Explanation: why,
		})
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == ' ':
			end := i + 1
			for end < len(runes) && runes[end] == ' ' {
				end++
			}
			if end-i >= 2 {
				emit(i, end, string(runes[i:end]), []string{" "}, High, "มีการเว้นวรรคหลายช่องติดกัน")
			}
			i = end - 1
		case r == '!' || r == '?':
			end := i + 1
			for end < len(runes) && runes[end] == r {
				end++
			}
			if end-i >= 2 {
				emit(i, end, string(runes[i:end]), []string{string(r)}, Medium,
					"มีเครื่องหมายวรรคตอนซ้ำกัน โดยทั่วไปใช้เพียงตัวเดียว")
			}
			i = end - 1
		case r == 0x0E46: // ๆ maiyamok
			end := i + 1
			for end < len(runes) && runes[end] == 0x0E46 {
				end++
			}
			if end-i >= 2 {
				emit(i, end, string(runes[i:end]), []string{"ๆ"}, Medium, "ไม้ยมก (ๆ) ซ้ำกัน ควรใช้เพียงตัวเดียว")
			}
			i = end - 1
		case r == '.':
			end := i + 1
			for end < len(runes) && runes[end] == '.' {
				end++
			}
			if end-i >= 4 {
				emit(i, end, string(runes[i:end]), []string{"..."}, Low, "จุดไข่ปลาโดยทั่วไปใช้สามจุด")
			}
			i = end - 1
		}
	}

	return issues
}

// Repetition flags a token that recurs unusually often (docs/12 §10). It counts
// whitespace/punctuation-delimited tokens, so it reliably catches repeated
// names, repeated Latin words, and repeated space-separated Thai terms.
//
// KNOWN LIMIT: dense Thai prose has no spaces, so a repeated *syllable* inside a
// run (the docs/12 §10 "มอง ×4" example) is NOT segmented out here - that needs
// dictionary-based word segmentation (PyThaiNLP, docs/12 §256), which would slot
// in behind Tokenize. The rule is intentionally precise rather than a noisy
// heuristic (docs/12 §41).
func Repetition(text string) []Issue {
	tokens := Tokenize(text)

	type acc struct {
		count int
		first Token
	}
	counts := map[string]*acc{}
	var order []string

	for _, t := range tokens {
		if !t.wordy() {
			continue
		}
		key := t.Text
		if t.Script == ScriptLatin {
			key = strings.ToLower(key)
		}
		a, ok := counts[key]
		if !ok {
			a = &acc{first: t}
			counts[key] = a
			order = append(order, key)
		}
		a.count++
	}

	var issues []Issue
	for _, key := range order {
		a := counts[key]
		if a.count < repetitionThreshold {
			continue
		}
		issues = append(issues, Issue{
			Kind: IssueRepetition, Start: a.first.Start, End: a.first.End,
			Original:    a.first.Text,
			Confidence:  Medium,
			Explanation: fmt.Sprintf(`คำว่า "%s" ปรากฏ %d ครั้ง อาจซ้ำเกินไป`, a.first.Text, a.count),
		})
		if len(issues) >= maxRepetitionIssues {
			break
		}
	}
	return issues
}

// isRepeatableLetter reports whether a rune is a Thai or Latin letter whose
// long run signals possible emphasis/typo (excludes spaces, punctuation,
// digits, and combining marks, whose repetition the other rules own).
func isRepeatableLetter(r rune) bool {
	if isThaiConsonant(r) {
		return true
	}
	switch r {
	case 0x0E30, 0x0E32, 0x0E33, 0x0E40, 0x0E41, 0x0E42, 0x0E43, 0x0E44: // ะ า ำ เ แ โ ใ ไ
		return true
	}
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// findAll returns the non-overlapping start rune-indices of pat within runes.
func findAll(runes, pat []rune) []int {
	if len(pat) == 0 || len(pat) > len(runes) {
		return nil
	}
	var out []int
	for i := 0; i+len(pat) <= len(runes); {
		match := true
		for j := range pat {
			if runes[i+j] != pat[j] {
				match = false
				break
			}
		}
		if match {
			out = append(out, i)
			i += len(pat)
		} else {
			i++
		}
	}
	return out
}
