// Package ratelimit provides the rate-limiting architecture for the API.
//
// docs/09 - API Specification.md §31 and docs/11 - Security & Privacy.md §24
// require DIFFERENT limits per endpoint class, not one global limit. That is
// modelled here as a set of named Policies plus a pluggable Limiter, so the
// auth, search, AI, and public-read tiers can be tuned independently as they
// are implemented.
//
// Two Limiter implementations are provided:
//
//	Redis  - shared across API instances; correct once the API scales
//	         horizontally (docs/14 §40).
//	Memory - per-process fallback used when Redis is not configured, which
//	         docs/07 §18 explicitly allows during early development.
//
// Both use a fixed window. That is intentionally simple and sufficient for
// abuse control; a sliding window or token bucket can replace it behind this
// interface without touching handlers.
package ratelimit

import (
	"context"
	"time"
)

// Policy is a named limit for a class of endpoints.
type Policy struct {
	Name   string
	Limit  int
	Window time.Duration
}

// Documented endpoint classes (docs/09 §31, docs/11 §24). The numbers are
// starting points for the foundation phase and are expected to be tuned
// against real traffic before launch; they live here so that tuning is a
// one-file change rather than a hunt through handlers.
var (
	// PublicRead covers guest browsing and reading, which must stay generous -
	// content protection must not break normal reading (docs/07 §32).
	PublicRead = Policy{Name: "public_read", Limit: 300, Window: time.Minute}

	// Search is moderate: it is more expensive than a keyed read.
	Search = Policy{Name: "search", Limit: 60, Window: time.Minute}

	// Auth is strict, to blunt brute force and credential stuffing
	// (docs/10 §38).
	Auth = Policy{Name: "auth", Limit: 10, Window: time.Minute}

	// Write covers comments and community posting.
	Write = Policy{Name: "write", Limit: 30, Window: time.Minute}

	// Progress covers reading-position saves - the highest-frequency legitimate
	// write on the platform (docs/09 §17). It is its own class so a reader's
	// debounced saves never consume the Write quota a writer needs for edits,
	// and so this one endpoint can be tuned independently (docs/09 §31).
	Progress = Policy{Name: "progress", Limit: 60, Window: time.Minute}

	// AI is very strict because each request carries real infrastructure cost
	// (docs/07 §33).
	AI = Policy{Name: "ai", Limit: 10, Window: time.Minute}

	// Upload guards signed-URL issuance.
	Upload = Policy{Name: "upload", Limit: 20, Window: time.Minute}
)

// Result describes the outcome of one rate-limit check.
type Result struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
}

// Limiter records a hit against key under policy and reports whether it is
// allowed.
//
// A Limiter must fail OPEN: if the backing store is unavailable, it returns an
// allowing Result rather than an error, so a Redis outage degrades protection
// instead of taking the site down (docs/15 §38 "fail gracefully").
type Limiter interface {
	Allow(ctx context.Context, key string, policy Policy) Result

	// Peek reports the key's standing WITHOUT recording a hit - what a quota
	// display reads, so that looking at "เหลือเท่าไหร่" never spends any of it.
	// It fails open the same way Allow does.
	Peek(ctx context.Context, key string, policy Policy) Result

	// Close releases any resources held by the limiter.
	Close() error
}

// allowed builds a permitting result.
func allowed(policy Policy, used int64) Result {
	remaining := policy.Limit - int(used)
	if remaining < 0 {
		remaining = 0
	}
	return Result{Allowed: true, Limit: policy.Limit, Remaining: remaining}
}

// denied builds a rejecting result.
func denied(policy Policy, retryAfter time.Duration) Result {
	if retryAfter <= 0 {
		retryAfter = policy.Window
	}
	return Result{Allowed: false, Limit: policy.Limit, Remaining: 0, RetryAfter: retryAfter}
}
