// Package thai is the platform's LOCAL, dependency-free Thai NLP layer - the
// "Layer 1 (Rules)" and lightweight tokenization of docs/12 §7, kept behind the
// AI provider so that the rest of the application never couples to a specific
// tokenizer or model (docs/12 §6, §26; the Phase 10 brief "isolate NLP
// preprocessing behind a service/interface").
//
// Everything here is DETERMINISTIC and PURE: the same text always yields the
// same issues, which is exactly what makes it cheap, testable, and safe to run
// synchronously (docs/12 §7 "Fast / Cheap / Predictable / Easy to test",
// docs/12 §27 synchronous path). No network, no model, no randomness.
//
// It deliberately does NOT attempt dictionary-based Thai word segmentation.
// Real segmentation needs a lexicon (PyThaiNLP is the documented Layer-2
// candidate, docs/12 §256); until that lands behind this same package, the
// tokenizer segments on whitespace and punctuation and classifies runs by
// script. That is honest about its limits (see Repetition) rather than shipping
// a fragile heuristic segmenter.
//
// Offsets are UNICODE CODE-POINT (rune) offsets, not bytes: Thai text is
// counted per character, never per byte (docs/09 §20 "runes, not bytes"). Thai,
// Latin, digits, and common punctuation are all in the Basic Multilingual
// Plane, so a rune offset equals a JavaScript string index for them; only
// astral characters (most emoji) would shift, which callers highlight on a
// best-effort basis.
package thai

// IssueKind is the category of a detected writing issue. It maps 1:1 to the
// persisted ai_suggestions.type vocabulary (docs/08 §26.1) and the transient
// suggestion `type` of docs/12 §13.
type IssueKind string

const (
	IssueSpelling    IssueKind = "spelling"
	IssuePunctuation IssueKind = "punctuation"
	IssueRepetition  IssueKind = "repetition"
	// IssuePolish is เกลาภาษา (13Y §7): readability suggestions, never
	// corrections. Always the softest band, never auto-applied, and its rules
	// only flag what measurably impedes reading - taste is not an error.
	IssuePolish IssueKind = "polish"
)

// Confidence is docs/12 §12's severity scale. Low-confidence issues are meant
// to be rendered less intrusively (docs/12 §12, §41).
type Confidence string

const (
	High   Confidence = "high"
	Medium Confidence = "medium"
	Low    Confidence = "low"
)

// Issue is one detected problem with a proposal. It carries the position
// information the editor uses to highlight the span (docs/12 §13).
//
// Suggestions may be empty: some issues are FLAG-ONLY (a possible repetition,
// an emphatic spelling that is probably intentional) - the writer is told, and
// nothing is proposed to replace it (docs/12 §10, §15). Nothing here ever
// mutates text; that is the whole point (docs/12 §15, §43).
type Issue struct {
	Kind        IssueKind
	Start       int // rune offset, inclusive
	End         int // rune offset, exclusive
	Original    string
	Suggestions []string
	Confidence  Confidence
	Explanation string // Thai, plain language (docs/12 §14 "Explain")
}

// Script classifies a token's dominant writing system, so repetition can fold
// Latin case while leaving Thai exact.
type Script string

const (
	ScriptThai  Script = "thai"
	ScriptLatin Script = "latin"
	ScriptOther Script = "other"
)

// Token is one whitespace/punctuation-delimited run with its rune span.
type Token struct {
	Text   string
	Start  int // rune offset, inclusive
	End    int // rune offset, exclusive
	Script Script
}

// ---------------------------------------------------------------------------
// Rune classification (Thai block U+0E00–U+0E7F)
// ---------------------------------------------------------------------------

func isThai(r rune) bool { return r >= 0x0E00 && r <= 0x0E7F }

// isThaiConsonant covers ก (0E01) … ฮ (0E2E).
func isThaiConsonant(r rune) bool { return r >= 0x0E01 && r <= 0x0E2E }

// isThaiDigit covers ๐ (0E50) … ๙ (0E59).
func isThaiDigit(r rune) bool { return r >= 0x0E50 && r <= 0x0E59 }

// isThaiLeadingVowel covers the vowels written BEFORE their consonant:
// เ แ โ ใ ไ ๅ. A tone mark directly after one of these is orphaned.
func isThaiLeadingVowel(r rune) bool {
	switch r {
	case 0x0E40, 0x0E41, 0x0E42, 0x0E43, 0x0E44, 0x0E45:
		return true
	}
	return false
}

// isThaiToneMark covers the four tone marks ่ ้ ๊ ๋ (0E48–0E4B).
func isThaiToneMark(r rune) bool { return r >= 0x0E48 && r <= 0x0E4B }

// isThaiUpperLowerMark covers the combining marks that MUST attach to a
// preceding consonant cluster: the above/below vowels ◌ั ◌ิ ◌ี ◌ึ ◌ื ◌ุ ◌ู,
// phinthu ◌ฺ, maitaikhu ◌็, the tone marks, thanthakhat ◌์, nikhahit ◌ํ, and
// yamakkan ◌๎. None of these can legally open a syllable.
func isThaiUpperLowerMark(r rune) bool {
	switch {
	case r == 0x0E31: // ◌ั mai han-akat
		return true
	case r >= 0x0E34 && r <= 0x0E3A: // ◌ิ ◌ี ◌ึ ◌ื ◌ุ ◌ู ◌ฺ
		return true
	case r == 0x0E47: // ◌็ maitaikhu
		return true
	case isThaiToneMark(r): // ◌่ ◌้ ◌๊ ◌๋
		return true
	case r >= 0x0E4C && r <= 0x0E4E: // ◌์ ◌ํ ◌๎
		return true
	}
	return false
}

// isTokenBreak reports whether a rune separates tokens: any whitespace, ASCII
// punctuation/symbol, or the Thai punctuation ฯ (0E2F) and ๆ (0E46).
func isTokenBreak(r rune) bool {
	if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' || r == 0x00A0 {
		return true
	}
	if r == 0x0E2F || r == 0x0E46 { // ฯ paiyannoi, ๆ maiyamok
		return true
	}
	if r < 0x80 {
		// ASCII: a token is a letter or digit; everything else breaks.
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		return !isAlnum
	}
	// Non-ASCII, non-Thai symbols (e.g. - … “ ”) break tokens too.
	if !isThai(r) {
		switch r {
		case 0x2018, 0x2019, 0x201C, 0x201D, 0x2013, 0x2014, 0x2026, 0x00B7:
			return true
		}
	}
	return false
}

// Tokenize splits text into whitespace/punctuation-delimited tokens with rune
// spans. It is the only tokenizer in the package; every analyzer that needs
// "words" goes through it, so replacing it with a real segmenter later is a
// one-function change (docs/12 §6, §26).
func Tokenize(text string) []Token {
	runes := []rune(text)
	var tokens []Token
	start := -1

	flush := func(end int) {
		if start < 0 {
			return
		}
		tok := string(runes[start:end])
		tokens = append(tokens, Token{
			Text:   tok,
			Start:  start,
			End:    end,
			Script: scriptOf(runes[start:end]),
		})
		start = -1
	}

	for i, r := range runes {
		if isTokenBreak(r) {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(runes))
	return tokens
}

// scriptOf reports a run's dominant script by counting Thai vs Latin letters.
func scriptOf(runes []rune) Script {
	var thaiN, latinN int
	for _, r := range runes {
		switch {
		case isThai(r) && !isThaiDigit(r):
			thaiN++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			latinN++
		}
	}
	switch {
	case thaiN > 0 && thaiN >= latinN:
		return ScriptThai
	case latinN > 0:
		return ScriptLatin
	default:
		return ScriptOther
	}
}

// wordy reports whether a token is substantial enough to consider for
// repetition: at least two runes and containing at least one letter (not pure
// digits/symbols).
func (t Token) wordy() bool {
	n := 0
	hasLetter := false
	for _, r := range t.Text {
		n++
		if isThaiConsonant(r) || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
		}
	}
	return n >= 2 && hasLetter
}
