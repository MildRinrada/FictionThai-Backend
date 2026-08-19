package thai

import (
	"sort"
	"strings"
)

// Summarize produces an EXTRACTIVE summary: it selects the highest-scoring
// existing sentences and returns them in their original order (docs/12 §22
// chapter summary). It is deterministic - the same text always yields the same
// summary - which is what lets it stand in for the "expensive" async operation
// (docs/12 §27) without a model, while a real LLM slots in behind the provider
// later for abstractive quality.
//
// The summary is always the writer's OWN sentences, verbatim; nothing is
// invented or rewritten (docs/12 §2, §15).
func Summarize(text string, maxSentences int) string {
	if maxSentences < 1 {
		maxSentences = 1
	}
	sentences := splitSentences(text)
	if len(sentences) == 0 {
		return ""
	}
	if len(sentences) <= maxSentences {
		return strings.TrimSpace(text)
	}

	// Score each sentence by summed term frequency of its wordy tokens,
	// normalised by token count so a long sentence does not win by length alone.
	freq := map[string]int{}
	perSentence := make([][]string, len(sentences))
	for i, s := range sentences {
		for _, t := range Tokenize(s) {
			if !t.wordy() {
				continue
			}
			key := t.Text
			if t.Script == ScriptLatin {
				key = strings.ToLower(key)
			}
			freq[key]++
			perSentence[i] = append(perSentence[i], key)
		}
	}

	type scored struct {
		index int
		score float64
	}
	ranked := make([]scored, len(sentences))
	for i, keys := range perSentence {
		var sum int
		for _, k := range keys {
			sum += freq[k]
		}
		score := 0.0
		if len(keys) > 0 {
			score = float64(sum) / float64(len(keys))
		}
		ranked[i] = scored{index: i, score: score}
	}

	// Highest score first; ties keep the earlier sentence (stable, deterministic).
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score
		}
		return ranked[a].index < ranked[b].index
	})

	chosen := make([]int, 0, maxSentences)
	for _, r := range ranked[:maxSentences] {
		chosen = append(chosen, r.index)
	}
	sort.Ints(chosen)

	parts := make([]string, 0, len(chosen))
	for _, idx := range chosen {
		if s := strings.TrimSpace(sentences[idx]); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// splitSentences breaks text into candidate sentences. Thai has no full stop,
// so sentence boundaries are approximated by newlines, ASCII sentence
// punctuation, and runs of two or more spaces (a common Thai phrase break). If
// none of those appear, single spaces are the last-resort boundary so a
// space-separated wall of text can still be summarised.
func splitSentences(text string) []string {
	fields := splitOn(text, func(prev, r rune) bool {
		switch r {
		case '\n', '\r', '.', '!', '?':
			return true
		}
		// Two-or-more spaces: break on the SECOND space.
		if r == ' ' && prev == ' ' {
			return true
		}
		return false
	})

	var sentences []string
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			sentences = append(sentences, s)
		}
	}
	if len(sentences) > 1 {
		return sentences
	}

	// Fall back to single-space segmentation only when nothing stronger split
	// the text (a single spaceless or single-space run).
	var single []string
	for _, f := range strings.Fields(text) {
		if f != "" {
			single = append(single, f)
		}
	}
	if len(single) > 1 {
		return single
	}
	return sentences
}

// splitOn splits text wherever brk(previousRune, currentRune) reports a
// boundary, dropping the boundary rune itself.
func splitOn(text string, brk func(prev, r rune) bool) []string {
	var out []string
	var b strings.Builder
	var prev rune = -1
	for _, r := range text {
		if brk(prev, r) {
			out = append(out, b.String())
			b.Reset()
			prev = r
			continue
		}
		b.WriteRune(r)
		prev = r
	}
	out = append(out, b.String())
	return out
}
