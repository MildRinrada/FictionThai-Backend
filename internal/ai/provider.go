package ai

import (
	"context"
	"errors"
	"fmt"
)

// Provider is the NARROW boundary between the AI domain and whatever actually
// performs language work (docs/09 §24 "Go API → AI Service → Thai NLP / Model",
// docs/12 §26 task router). Today it is a local, deterministic rule engine; a
// self-hosted model or an external LLM can replace it later WITHOUT the domain,
// the handlers, or the database learning which - provider-specific types never
// cross this line (the Phase 10 brief "AI Provider Boundary").
//
// A provider receives user content as STRUCTURED DATA (AnalyzeInput.Text,
// SummarizeInput.Text), never spliced into instructions. A future LLM adapter
// MUST keep that separation - manuscript text goes in the model's user/content
// role, never its system role (docs/12 §34, docs/11 §53).
type Provider interface {
	// Name is stored on ai_requests.provider. It is non-sensitive by contract:
	// never a URL, key, or endpoint (docs/12 §36, docs/11 §67).
	Name() string

	// Analyze runs fast checks and returns transient inline suggestions
	// (docs/12 §13, §27 synchronous path).
	Analyze(ctx context.Context, in AnalyzeInput) (AnalyzeResult, error)

	// Summarize condenses text - the expensive, asynchronous operation
	// (docs/12 §22, §27).
	Summarize(ctx context.Context, in SummarizeInput) (SummarizeResult, error)
}

// AnalyzeInput is untrusted text plus which check families to run. Kinds is a
// subset of {spelling, punctuation, repetition}; empty means all.
type AnalyzeInput struct {
	Text  string
	Kinds []string
}

// AnalyzeResult carries the model identifier and the inline suggestions.
type AnalyzeResult struct {
	Model       string
	Suggestions []InlineSuggestion
}

// SummarizeInput is untrusted text and the target length in sentences.
type SummarizeInput struct {
	Text         string
	MaxSentences int
}

// SummarizeResult carries the model identifier and the extractive summary.
type SummarizeResult struct {
	Model   string
	Summary string
}

// InlineSuggestion is the transient, position-bearing suggestion of docs/12 §13
// - returned to the editor for highlighting and NOT persisted (the persisted
// shape is the narrower ai_suggestions row, docs/08 §26.1). It carries both a
// numeric `confidence` and a `severity` band, exactly the §13 object.
type InlineSuggestion struct {
	Type        string   `json:"type"`
	Start       int      `json:"start"`
	End         int      `json:"end"`
	Original    string   `json:"original"`
	Suggestions []string `json:"suggestions"`
	Confidence  float64  `json:"confidence"`
	Severity    string   `json:"severity"`
	Explanation string   `json:"explanation"`
}

// ---------------------------------------------------------------------------
// Provider failure taxonomy
//
// A provider translates its OWN failures (a timeout, a 429, a 5xx, malformed
// output) into one of these project-level kinds, so the service maps outcomes
// without ever importing a vendor SDK's error types (the Phase 10 brief "The
// provider adapter should translate provider-specific failures"). The local
// provider never emits these - it cannot fail - but the taxonomy is exercised
// by the fake provider used in tests, which is the whole point of having it.
// ---------------------------------------------------------------------------

// FailureKind classifies a provider failure. The value doubles as the safe
// error_code stored on ai_requests (docs/12 §36) - it is a classification,
// never provider detail.
type FailureKind string

const (
	FailUnavailable FailureKind = "provider_unavailable"
	FailTimeout     FailureKind = "provider_timeout"
	FailRateLimited FailureKind = "provider_rate_limited"
	FailMalformed   FailureKind = "invalid_output"
	FailRefused     FailureKind = "provider_refused"
	FailOversize    FailureKind = "output_too_large"
	FailInternal    FailureKind = "provider_error"
)

// Retryable reports whether re-running the request could plausibly succeed. Only
// TRANSIENT failures are retryable; a malformed output or a refusal is not
// (the Phase 10 brief "Differentiate transient ... from permanent"). Retries are
// never automatic - they are user-triggered (docs/12 §29 avoids retry storms).
func (k FailureKind) Retryable() bool {
	switch k {
	case FailUnavailable, FailTimeout, FailRateLimited:
		return true
	}
	return false
}

// ProviderError wraps a provider failure with its classification.
type ProviderError struct {
	Kind FailureKind
	Err  error
}

func (e *ProviderError) Error() string {
	if e.Err == nil {
		return string(e.Kind)
	}
	return fmt.Sprintf("%s: %v", e.Kind, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// NewProviderError builds a classified provider failure.
func NewProviderError(kind FailureKind, err error) *ProviderError {
	return &ProviderError{Kind: kind, Err: err}
}

// failureKindOf extracts the classification from any error a provider returned.
// A plain (unclassified) error is treated as a generic provider_error, never
// surfaced verbatim (docs/11 §67).
func failureKindOf(err error) FailureKind {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return FailInternal
}
