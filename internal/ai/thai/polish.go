package thai

import "fmt"

// เกลาภาษา (13Y §7) - the SOFTEST rule family in the package.
//
// The risk of a polish feature is homogenising everyone's voice, so these
// rules keep three properties: they only flag what measurably impedes
// reading (never taste), everything is Low confidence, and most issues are
// flag-only - the writer is told, nothing is proposed to replace their words.

// polishLongRunRunes is the length of an unbroken (space-less) Thai run that
// gets a readability flag. Thai has no word spaces, so the unit of reader rest
// is the phrase break; ~120 characters without one is a wall.
const polishLongRunRunes = 120

// polishConnectorThreshold is how many times a connector may recur in one
// paragraph before the paragraph gets a (single) flag.
const polishConnectorThreshold = 3

// maxPolishIssues caps the family's output - a soft advisor must never be the
// loudest thing on the page.
const maxPolishIssues = 10

// overusedConnectors are the fillers whose dense repetition reads as a first
// draft. Flagged per paragraph, once, and only past the threshold.
var overusedConnectors = []string{"แล้วก็", "จากนั้นก็", "ต่อมาก็"}

// Polish runs the readability rules and returns Low-confidence issues.
func Polish(text string) []Issue {
	runes := []rune(text)
	var issues []Issue

	// 1. A doubled word - the same token twice in a row ("เขาก็ ก็ เดินไป") -
	//    is almost always an editing leftover. The one polish rule concrete
	//    enough to carry a proposed fix.
	tokens := Tokenize(text)
	for i := 0; i+1 < len(tokens); i++ {
		a, b := tokens[i], tokens[i+1]
		if !a.wordy() || a.Text != b.Text {
			continue
		}
		// Only when nothing but whitespace separates them.
		adjacent := true
		for _, r := range runes[a.End:b.Start] {
			if r != ' ' && r != '\t' {
				adjacent = false
				break
			}
		}
		if !adjacent {
			continue
		}
		issues = append(issues, Issue{
			Kind: IssuePolish, Start: a.Start, End: b.End,
			Original:    string(runes[a.Start:b.End]),
			Suggestions: []string{a.Text},
			Confidence:  Low,
			Explanation: fmt.Sprintf(`คำว่า "%s" ซ้ำติดกันสองครั้ง อาจเป็นคำที่พิมพ์เกิน`, a.Text),
		})
		i++
	}

	// 2. A wall of text: an unbroken run past the threshold. Flag-only - WHERE
	//    to breathe is the writer's call.
	runStart := -1
	flagRun := func(end int) {
		if runStart < 0 || end-runStart < polishLongRunRunes {
			runStart = -1
			return
		}
		issues = append(issues, Issue{
			Kind: IssuePolish, Start: runStart, End: end,
			Original:    string(runes[runStart:min(runStart+40, end)]) + "…",
			Confidence:  Low,
			Explanation: fmt.Sprintf("ช่วงนี้ยาวติดกัน %d อักษรโดยไม่มีการเว้นวรรค การแบ่งวรรคช่วยให้อ่านง่ายขึ้น", end-runStart),
		})
		runStart = -1
	}
	for i, r := range runes {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			flagRun(i)
			continue
		}
		if runStart < 0 {
			runStart = i
		}
	}
	flagRun(len(runes))

	// 3. An overused connector, counted per paragraph and flagged once at its
	//    first appearance there.
	paragraphs := splitParagraphs(runes)
	for _, p := range paragraphs {
		for _, connector := range overusedConnectors {
			positions := findAll(runes[p.start:p.end], []rune(connector))
			if len(positions) < polishConnectorThreshold {
				continue
			}
			at := p.start + positions[0]
			issues = append(issues, Issue{
				Kind: IssuePolish, Start: at, End: at + len([]rune(connector)),
				Original:    connector,
				Confidence:  Low,
				Explanation: fmt.Sprintf(`"%s" ถูกใช้ %d ครั้งในย่อหน้าเดียว การสลับคำเชื่อมช่วยให้จังหวะไม่ซ้ำ`, connector, len(positions)),
			})
		}
	}

	if len(issues) > maxPolishIssues {
		issues = issues[:maxPolishIssues]
	}
	return issues
}

type span struct{ start, end int }

// splitParagraphs returns the rune spans between newlines, skipping blanks.
func splitParagraphs(runes []rune) []span {
	var out []span
	start := 0
	flush := func(end int) {
		trimmedStart, trimmedEnd := start, end
		for trimmedStart < trimmedEnd && runes[trimmedStart] == ' ' {
			trimmedStart++
		}
		if trimmedStart < trimmedEnd {
			out = append(out, span{trimmedStart, trimmedEnd})
		}
	}
	for i, r := range runes {
		if r == '\n' {
			flush(i)
			start = i + 1
		}
	}
	flush(len(runes))
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
