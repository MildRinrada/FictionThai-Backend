package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Phase 13A - the create form's two new answers
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13A, §13B).
//
// The rating is the only required field the create form adds, and the reason it
// is required is the behaviour proved below: it decides whether the work
// appears on a browse surface at all. A fiction that quietly defaulted to
// ทั่วไป because a dropdown went unanswered would be making a claim its author
// never made.

type ratingBody struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	AgeRating  string  `json:"age_rating"`
	AgeGate    string  `json:"age_gate"`
	OriginType string  `json:"origin_type"`
	Fandom     *string `json:"fandom"`
}

func TestCreate_AgeRatingIsRequired(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	// Deliberately NOT createNovelBody: this is the case that proves the rule.
	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		map[string]any{"title": uniqueName(t, "No rating ")})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("create without a rating status = %d, want 422. body: %s", res.status, res.body)
	}
	if !strings.Contains(string(res.body), "age_rating") {
		t.Fatalf("the error does not name the missing field: %s", res.body)
	}

	// An unknown value is refused too, rather than falling back to a default.
	res = env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		map[string]any{"title": uniqueName(t, "Bad rating "), "age_rating": "18+"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown rating status = %d, want 422. body: %s", res.status, res.body)
	}
}

func TestCreate_RatingGateAndOriginRoundTrip(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	created := dataOf[ratingBody](t, env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		map[string]any{
			"title":       uniqueName(t, "Fanfic "),
			"age_rating":  "mature",
			"age_gate":    "verified",
			"origin_type": "fanfiction",
			"fandom":      "  วรรณคดีไทย  ",
		}))

	if created.AgeRating != "mature" || created.AgeGate != "verified" {
		t.Fatalf("rating/gate not stored: %+v", created)
	}
	if created.OriginType != "fanfiction" {
		t.Fatalf("origin_type = %q", created.OriginType)
	}
	if created.Fandom == nil || *created.Fandom != "วรรณคดีไทย" {
		t.Fatalf("fandom not trimmed or lost: %+v", created.Fandom)
	}

	// The gate defaults to the WIDER option, so nobody is locked out of a work
	// because the writer never opened the setting.
	plain := dataOf[ratingBody](t, env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		map[string]any{"title": uniqueName(t, "Plain "), "age_rating": "general"}))
	if plain.AgeGate != "warning" {
		t.Fatalf("default gate = %q, want warning", plain.AgeGate)
	}
	if plain.OriginType != "original" {
		t.Fatalf("default origin = %q, want original", plain.OriginType)
	}
}

// The origin/fandom pair stays coherent: switching a work to original clears
// the source it no longer has, rather than failing at the CHECK constraint.
func TestUpdate_OriginAndFandomStayCoherent(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Origin "), map[string]any{
		"origin_type": "fanfiction",
		"fandom":      "ต้นฉบับเดิม",
	}))
	path := "/api/v1/novels/" + novel.ID

	res := env.asOwner(t, w, http.MethodPatch, path, map[string]any{"origin_type": "original"})
	if res.status != http.StatusOK {
		t.Fatalf("switch to original status = %d. body: %s", res.status, res.body)
	}
	if got := dataOf[ratingBody](t, res); got.Fandom != nil {
		t.Fatalf("original work kept a source: %q", *got.Fandom)
	}

	// Naming a source on original work is a field error, not a silent store.
	res = env.asOwner(t, w, http.MethodPatch, path, map[string]any{"fandom": "อะไรสักอย่าง"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("naming a source on original work status = %d, want 422. body: %s",
			res.status, res.body)
	}

	// An edit that does not mention the rating leaves it alone (docs/09 §3).
	before := dataOf[ratingBody](t, env.asOwner(t, w, http.MethodGet, path))
	env.asOwner(t, w, http.MethodPatch, path, map[string]any{"title": "ชื่อใหม่"})
	after := dataOf[ratingBody](t, env.asOwner(t, w, http.MethodGet, path))
	if after.AgeRating != before.AgeRating || after.AgeGate != before.AgeGate {
		t.Fatalf("a title edit changed the rating: %+v -> %+v", before, after)
	}
}

// 18+ work is kept off browse surfaces by default. It is still reachable by
// direct link - this hides it from discovery, which is a different thing from
// gating it (§13B; the gate itself is 13B's job).
func TestListing_ExcludesMatureByDefault(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	title := uniqueName(t, "Mature ")
	mature := env.publishedNovel(t, w, map[string]any{
		"title": title, "age_rating": "mature",
	})
	teen := env.publishedNovel(t, w, map[string]any{"age_rating": "teen"})

	res := env.asGuest(t, http.MethodGet, "/api/v1/novels?per_page=100")
	items, _ := collectionOf[ratingBody](t, res)

	var sawMature, sawTeen bool
	for _, item := range items {
		switch item.ID {
		case mature.ID:
			sawMature = true
		case teen.ID:
			sawTeen = true
		}
	}
	if sawMature {
		t.Fatal("18+ work appeared in the default listing")
	}
	// 15+ carries a warning, not a disappearance: hiding it would punish a
	// writer for being honest about their own work.
	if !sawTeen {
		t.Fatal("15+ work was hidden from the listing")
	}

	// Search must not become the way around the exclusion.
	res = env.asGuest(t, http.MethodGet, "/api/v1/novels?q="+url.QueryEscape(title))
	found, _ := collectionOf[ratingBody](t, res)
	for _, item := range found {
		if item.ID == mature.ID {
			t.Fatal("search returned 18+ work the listing excludes")
		}
	}

	// Reachable by direct link, though - excluded from discovery is not gone.
	direct := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+mature.ID)
	if direct.status != http.StatusOK {
		t.Fatalf("18+ work unreachable by direct link: %d", direct.status)
	}
}
