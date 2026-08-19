package pagination_test

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// parse runs Parse against a query string.
func parse(t *testing.T, query string) pagination.Params {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/novels?"+query, nil)
	return pagination.Parse(c)
}

func TestParse_Defaults(t *testing.T) {
	p := parse(t, "")

	if p.Page != pagination.DefaultPage {
		t.Errorf("page = %d, want %d", p.Page, pagination.DefaultPage)
	}
	if p.PerPage != pagination.DefaultPerPage {
		t.Errorf("per_page = %d, want %d", p.PerPage, pagination.DefaultPerPage)
	}
}

func TestParse_ReadsValidValues(t *testing.T) {
	p := parse(t, "page=3&per_page=50")

	if p.Page != 3 {
		t.Errorf("page = %d, want 3", p.Page)
	}
	if p.PerPage != 50 {
		t.Errorf("per_page = %d, want 50", p.PerPage)
	}
	if got := p.Offset(); got != 100 {
		t.Errorf("offset = %d, want 100", got)
	}
}

// docs/09 §9: the maximum per_page must be enforced server-side, and
// docs/15 §31 forbids unbounded reads on public endpoints.
func TestParse_ClampsPerPageToMaximum(t *testing.T) {
	p := parse(t, "per_page=100000")

	if p.PerPage != pagination.MaxPerPage {
		t.Errorf("per_page = %d, want it clamped to %d", p.PerPage, pagination.MaxPerPage)
	}
}

func TestParse_RejectsHostileValues(t *testing.T) {
	tests := map[string]string{
		"negative page":     "page=-1",
		"zero page":         "page=0",
		"negative per_page": "per_page=-20",
		"zero per_page":     "per_page=0",
		"non-numeric page":  "page=abc",
		"sql-ish per_page":  "per_page=" + url.QueryEscape("20;DROP TABLE novels"),
		"overflowing page":  "page=99999999999999999999999",
		"float per_page":    "per_page=20.5",
		"empty page":        "page=",
	}

	for name, query := range tests {
		t.Run(name, func(t *testing.T) {
			p := parse(t, query)

			if p.Page < 1 {
				t.Errorf("page = %d; a page below 1 would produce a negative OFFSET", p.Page)
			}
			if p.PerPage < 1 || p.PerPage > pagination.MaxPerPage {
				t.Errorf("per_page = %d, want it within [1, %d]", p.PerPage, pagination.MaxPerPage)
			}
			if p.Offset() < 0 {
				t.Errorf("offset = %d, want a non-negative offset", p.Offset())
			}
		})
	}
}

func TestMetaFor(t *testing.T) {
	meta := pagination.Params{Page: 2, PerPage: 20}.MetaFor(125)

	if meta.Page != 2 || meta.PerPage != 20 || meta.Total != 125 {
		t.Errorf("meta = %+v, want {Page:2 PerPage:20 Total:125}", meta)
	}
}
