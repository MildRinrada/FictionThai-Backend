package ai

import (
	"context"

	"github.com/fictionthai/fictionthai/backend/internal/ai/thai"
)

// defaultSummarySentences is how many sentences the extractive summary keeps
// when the caller does not specify (docs/12 §22).
const defaultSummarySentences = 3

// localProvider is the default AI backend: the deterministic, dependency-free
// Thai rule engine of the thai package (docs/12 §7 Layer 1, docs/11 §52 "local
// Thai NLP"). It runs IN-PROCESS, sends nothing to any external service, and
// cannot fail on transient conditions - which is exactly why it is the safe,
// zero-config default and the one every automated test uses (Phase 10 brief
// "simplest deterministic backend").
type localProvider struct {
	summarySentences int
}

// NewLocalProvider builds the local rule-based provider.
func NewLocalProvider() Provider {
	return &localProvider{summarySentences: defaultSummarySentences}
}

func (p *localProvider) Name() string { return "local" }

func (p *localProvider) Analyze(_ context.Context, in AnalyzeInput) (AnalyzeResult, error) {
	issues := thai.Analyze(in.Text, toIssueKinds(in.Kinds)...)
	suggestions := make([]InlineSuggestion, 0, len(issues))
	for _, is := range issues {
		suggestions = append(suggestions, fromIssue(is))
	}
	return AnalyzeResult{Model: "rules-v1", Suggestions: suggestions}, nil
}

func (p *localProvider) Summarize(_ context.Context, in SummarizeInput) (SummarizeResult, error) {
	n := in.MaxSentences
	if n <= 0 {
		n = p.summarySentences
	}
	return SummarizeResult{Model: "extractive-v1", Summary: thai.Summarize(in.Text, n)}, nil
}

// toIssueKinds maps the request's suggestion-type strings to thai issue kinds.
// An unknown kind is dropped rather than erroring - the service has already
// validated the vocabulary, and thai.Analyze with no kinds runs everything.
func toIssueKinds(kinds []string) []thai.IssueKind {
	out := make([]thai.IssueKind, 0, len(kinds))
	for _, k := range kinds {
		switch thai.IssueKind(k) {
		case thai.IssueSpelling, thai.IssuePunctuation, thai.IssueRepetition, thai.IssuePolish:
			out = append(out, thai.IssueKind(k))
		}
	}
	return out
}

// fromIssue maps a thai.Issue to the transient inline suggestion of docs/12 §13,
// deriving the numeric confidence from the severity band.
func fromIssue(is thai.Issue) InlineSuggestion {
	suggestions := is.Suggestions
	if suggestions == nil {
		suggestions = []string{}
	}
	return InlineSuggestion{
		Type:        string(is.Kind),
		Start:       is.Start,
		End:         is.End,
		Original:    is.Original,
		Suggestions: suggestions,
		Confidence:  confidenceScore(is.Confidence),
		Severity:    string(is.Confidence),
		Explanation: is.Explanation,
	}
}

// confidenceScore turns a severity band into the numeric confidence of
// docs/12 §13's example object.
func confidenceScore(c thai.Confidence) float64 {
	switch c {
	case thai.High:
		return 0.9
	case thai.Medium:
		return 0.6
	default:
		return 0.3
	}
}
