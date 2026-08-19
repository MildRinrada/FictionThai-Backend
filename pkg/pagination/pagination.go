// Package pagination implements the shared `?page=&per_page=` contract.
//
// docs/09 - API Specification.md §9 requires that the maximum per_page is
// enforced server-side; docs/15 - Testing Strategy.md §31 forbids unbounded
// collection reads on public endpoints. Every list endpoint must go through
// Parse so neither rule can be forgotten at a call site.
package pagination

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

const (
	DefaultPage    = 1
	DefaultPerPage = 20
	MaxPerPage     = 100
)

// Params is a validated, clamped page request.
type Params struct {
	Page    int
	PerPage int
}

// Offset is the SQL OFFSET for these params.
func (p Params) Offset() int { return (p.Page - 1) * p.PerPage }

// Limit is the SQL LIMIT for these params.
func (p Params) Limit() int { return p.PerPage }

// Meta is the `meta` object returned alongside a collection.
type Meta struct {
	Page    int   `json:"page"`
	PerPage int   `json:"per_page"`
	Total   int64 `json:"total"`
}

// MetaFor builds the response meta for a set of params and a total count.
func (p Params) MetaFor(total int64) Meta {
	return Meta{Page: p.Page, PerPage: p.PerPage, Total: total}
}

// Parse reads page/per_page from the query string. Invalid, missing, or
// out-of-range values fall back to safe defaults rather than erroring, so a
// malformed link can never produce an unbounded query.
func Parse(c *gin.Context) Params {
	return Params{
		Page:    clamp(c.Query("page"), DefaultPage, 1, 0),
		PerPage: clamp(c.Query("per_page"), DefaultPerPage, 1, MaxPerPage),
	}
}

// clamp parses raw and constrains it to [min, max]. A max of 0 means unbounded
// above (used for page numbers, which have no fixed ceiling).
func clamp(raw string, fallback, min, max int) int {
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min {
		return fallback
	}
	if max > 0 && v > max {
		return max
	}
	return v
}
