package promo_test

import (
	"testing"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/promo"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// docs/HOME-PROMO.md - the queue's two binding rules live in code, so they
// are pinned here: the paid ratio is enforced at read time, and a slide's
// link can never leave the site.

func slide(source promo.Source) promo.Slide {
	return promo.Slide{Source: source, Enabled: true}
}

func fieldsOf(t *testing.T, err error) map[string][]string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	apiErr, ok := err.(*apierror.Error)
	if !ok {
		t.Fatalf("expected an *apierror.Error, got %T", err)
	}
	return apiErr.Fields
}

func TestServeQueue_CapsPaidAtOneInFour(t *testing.T) {
	// Three editorial beside it: the ONE paid slide rides along; a second is
	// dropped however it is positioned.
	served := promo.ServeQueue([]promo.Slide{
		slide(promo.SourcePaid),
		slide(promo.SourceEditorial),
		slide(promo.SourcePaid),
		slide(promo.SourceEvent),
		slide(promo.SourceEditorial),
	})
	if len(served) != 4 {
		t.Fatalf("served %d slides, want the full deck of 4", len(served))
	}
	paid := 0
	for _, s := range served {
		if s.Source == promo.SourcePaid {
			paid++
		}
	}
	if paid != 1 {
		t.Fatalf("served %d paid slides, want exactly 1", paid)
	}
}

func TestServeQueue_PaidNeverCarriesAThinDeck(t *testing.T) {
	// Fewer than three editorial slides: the bought placement stays out
	// entirely - a deck that is mostly paid is a billboard, not a front page.
	served := promo.ServeQueue([]promo.Slide{
		slide(promo.SourcePaid),
		slide(promo.SourceEditorial),
		slide(promo.SourceEditorial),
	})
	for _, s := range served {
		if s.Source == promo.SourcePaid {
			t.Fatal("a paid slide must not serve without three editorial slides beside it")
		}
	}
	if len(served) != 2 {
		t.Fatalf("served %d, want the 2 editorial slides", len(served))
	}
}

func TestServeQueue_CapsTheDeckAtFour(t *testing.T) {
	live := make([]promo.Slide, 6)
	for i := range live {
		live[i] = slide(promo.SourceEditorial)
	}
	if got := len(promo.ServeQueue(live)); got != promo.MaxServed {
		t.Fatalf("served %d, want %d", got, promo.MaxServed)
	}
}

func TestValidate_RejectsExternalLinks(t *testing.T) {
	for _, link := range []string{"https://elsewhere.example", "//elsewhere.example", "javascript:alert(1)", ""} {
		_, err := promo.Validate(promo.Input{Headline: "หัวเรื่อง", LinkURL: link})
		if len(fieldsOf(t, err)["link_url"]) == 0 {
			t.Errorf("link %q must be rejected - internal paths only", link)
		}
	}

	if _, err := promo.Validate(promo.Input{Headline: "หัวเรื่อง", LinkURL: "/novel/my-story"}); err != nil {
		t.Fatalf("an internal path should validate: %v", err)
	}
}

func TestValidate_WindowMustEndAfterItStarts(t *testing.T) {
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(-time.Hour)
	_, err := promo.Validate(promo.Input{
		Headline: "หัวเรื่อง", LinkURL: "/novel/x",
		StartsAt: &start, EndsAt: &end,
	})
	if len(fieldsOf(t, err)["ends_at"]) == 0 {
		t.Error("a window that ends before it starts must be rejected")
	}
}

func TestValidate_DefaultsAndSourceVocabulary(t *testing.T) {
	valid, err := promo.Validate(promo.Input{Headline: "หัวเรื่อง", LinkURL: "/x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if valid.Source != promo.SourceEditorial || valid.TextSide != promo.TextStart {
		t.Errorf("defaults = %s/%s, want editorial/start", valid.Source, valid.TextSide)
	}

	_, err = promo.Validate(promo.Input{Headline: "ห", LinkURL: "/x", Source: "sponsored"})
	if len(fieldsOf(t, err)["source"]) == 0 {
		t.Error("an unknown source must be rejected, not defaulted")
	}
}
