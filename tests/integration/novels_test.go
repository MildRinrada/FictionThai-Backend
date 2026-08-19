package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/fictionthai/fictionthai/backend/pkg/slug"
)

// ---------------------------------------------------------------------------
// Creation and the Fiction Format System
// ---------------------------------------------------------------------------

// docs/09 §15: the format fields have documented defaults, and a fiction starts
// private and unpublished (docs/11 §31).
func TestNovels_CreateAppliesDocumentedDefaults(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	// The title must be unique per run: the suite shares a database, and a
	// repeated title would legitimately pick up a collision suffix.
	title := "นิยายเรื่องแรกของฉัน " + uniqueName(t, "")
	novel := env.createNovel(t, w, createNovelBody(title, nil))

	if novel.StoryStructure != "multi_chapter" {
		t.Errorf("story_structure = %q, want multi_chapter", novel.StoryStructure)
	}
	if novel.PresentationFormat != "standard" {
		t.Errorf("presentation_format = %q, want standard", novel.PresentationFormat)
	}
	if novel.ContentMode != "general" {
		t.Errorf("content_mode = %q, want general", novel.ContentMode)
	}
	if novel.Status != "draft" {
		t.Errorf("status = %q, want draft", novel.Status)
	}
	if novel.Visibility == nil || *novel.Visibility != "private" {
		t.Error("a new fiction must start private")
	}
	// An address is a bare random token (docs/SLUGS.md, address review
	// 2026-08): the title never enters the URL, so a rename can never leave
	// the address asserting a name the work no longer has.
	if !slug.IsToken(novel.Slug) {
		t.Fatalf("slug = %q, want a bare %d-character address token", novel.Slug, slug.TokenLength)
	}
	if strings.Contains(novel.Slug, "fiction") {
		t.Errorf("slug = %q, want no words from the title in the address", novel.Slug)
	}
	if !novel.IsOwner {
		t.Error("the creator must be reported as the owner")
	}
}

// docs/08 §2.4 and docs/09 §14.4: the three dimensions are independent and every
// combination must be creatable. docs/15 §5.1 requires this verified before the
// phase moves on.
func TestNovels_CreateAcceptsEveryFormatCombination(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	created := 0
	for _, structure := range []string{"one_shot", "multi_chapter"} {
		for _, presentation := range []string{"standard", "chat"} {
			for _, mode := range []string{"general", "headcanon"} {
				name := strings.Join([]string{structure, presentation, mode}, "+")

				t.Run(name, func(t *testing.T) {
					novel := env.createNovel(t, w, createNovelBody("Fiction "+name, map[string]any{
						"story_structure":     structure,
						"presentation_format": presentation,
						"content_mode":        mode,
					}))

					if novel.StoryStructure != structure ||
						novel.PresentationFormat != presentation ||
						novel.ContentMode != mode {
						t.Errorf("stored %s+%s+%s, want %s",
							novel.StoryStructure, novel.PresentationFormat, novel.ContentMode, name)
					}

					// Structure drives navigation; presentation must not
					// (docs/09 §14.4).
					wantNavigation := structure == "multi_chapter"
					if novel.UsesChapterNavigation != wantNavigation {
						t.Errorf("uses_chapter_navigation = %v for %s, want %v",
							novel.UsesChapterNavigation, name, wantNavigation)
					}
				})
				created++
			}
		}
	}

	if created != 8 {
		t.Errorf("exercised %d combinations, want 8", created)
	}
}

// docs/09 §36: an unsupported format value is a validation error, never a
// silently accepted string.
func TestNovels_CreateRejectsUnsupportedFormatValues(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	tests := map[string]map[string]any{
		"unknown story structure":     {"story_structure": "trilogy"},
		"unknown presentation format": {"presentation_format": "script"},
		"unknown content mode":        {"content_mode": "fanfic"},
		"the collapsed enum shape":    {"story_structure": "headcanon_chat_one_shot"},
		"an empty dimension":          {"content_mode": ""},
		"wrong case":                  {"story_structure": "ONE_SHOT"},
	}

	for name, overrides := range tests {
		t.Run(name, func(t *testing.T) {
			res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
				createNovelBody("Bad Format", overrides))

			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422. body: %s", res.status, res.body)
			}
			if code := errorCodeOf(t, res); code != "INVALID_FICTION_FORMAT" {
				t.Errorf("error code = %q, want INVALID_FICTION_FORMAT (docs/09 §36)", code)
			}
		})
	}
}

func TestNovels_CreateValidatesFields(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	tests := map[string]map[string]any{
		"missing title":        {"title": ""},
		"blank title":          {"title": "   "},
		"over-long title":      {"title": strings.Repeat("a", 201)},
		"control character":    {"title": "Title\x00"},
		"over-long descriptio": {"description": strings.Repeat("a", 5001)},
		"javascript cover URL": {"cover_url": "javascript:alert(1)"},
		"relative cover URL":   {"cover_url": "/covers/mine.png"},
		"unknown status":       {"status": "archived"},
		"unknown visibility":   {"visibility": "friends"},
	}

	for name, overrides := range tests {
		t.Run(name, func(t *testing.T) {
			body := createNovelBody("A Valid Title", overrides)
			res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels", body)

			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422. body: %s", res.status, res.body)
			}
		})
	}
}

// docs/08 §7.1: a draft must never be publicly reachable.
func TestNovels_CreateRefusesAPublicDraft(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		createNovelBody("Public Draft", map[string]any{
			"status": "draft", "visibility": "public",
		}))

	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; a draft cannot be public. body: %s", res.status, res.body)
	}
}

// docs/08 §35: slugs are the public URL identity and are unique platform wide.
func TestNovels_DuplicateTitlesGetDistinctSlugs(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	title := uniqueName(t, "Shared Title ")
	first := env.createNovel(t, w, createNovelBody(title, nil))
	second := env.createNovel(t, w, createNovelBody(title, nil))

	if first.Slug == second.Slug {
		t.Fatal("two fictions received the same slug")
	}
	// Both get the SAME shape - a bare token (docs/SLUGS.md) - rather than the
	// first getting a clean slug and the second a decorated one.
	if !slug.IsToken(first.Slug) || !slug.IsToken(second.Slug) {
		t.Errorf("slugs = %q and %q, want bare address tokens for both", first.Slug, second.Slug)
	}
}

// docs/07 §14: ownership comes from the authenticated session, never the body.
func TestNovels_CreateIgnoresAClientSuppliedOwner(t *testing.T) {
	env := newAuthEnv(t)
	victim := env.newWriter(t)
	attacker := env.newWriter(t)

	res := env.asOwner(t, attacker, http.MethodPost, "/api/v1/novels",
		createNovelBody("Forged Ownership", map[string]any{
			"author_id": victim.userID,
			"user_id":   victim.userID,
		}))
	if res.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", res.status, res.body)
	}

	novel := dataOf[novelBody](t, res)
	if novel.Author.ID != attacker.userID {
		t.Errorf("author = %s, want the authenticated caller %s", novel.Author.ID, attacker.userID)
	}
}

func TestNovels_CreateRequiresAuthentication(t *testing.T) {
	env := newAuthEnv(t)

	res := env.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/novels",
		body:   createNovelBody("Anonymous Fiction", nil),
	})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body: %s", res.status, res.body)
	}
}

// ---------------------------------------------------------------------------
// Guest reading and visibility
// ---------------------------------------------------------------------------

// docs/09 §6, docs/11 §12: guests read public fiction without an account.
func TestNovels_GuestCanReadPublishedFiction(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200; guests must be able to read. body: %s", res.status, res.body)
	}

	got := dataOf[novelBody](t, res)
	if got.ID != novel.ID {
		t.Error("the guest received a different fiction")
	}
	// docs/08 §1.4: owner metadata must not appear in a public response.
	if got.Visibility != nil {
		t.Error("visibility leaked to a guest")
	}
	if got.IsOwner {
		t.Error("a guest was reported as the owner")
	}
}

// docs/11 §31: a draft must never appear in a public API response, and its
// existence must not be confirmable.
func TestNovels_DraftIsInvisibleToEveryoneElse(t *testing.T) {
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	stranger := env.newWriter(t)

	draft := env.createNovel(t, owner, createNovelBody("Secret Draft", nil))

	for name, res := range map[string]apiResponse{
		"guest":    env.asGuest(t, http.MethodGet, "/api/v1/novels/"+draft.Slug),
		"stranger": env.asOwner(t, stranger, http.MethodGet, "/api/v1/novels/"+draft.Slug),
	} {
		t.Run(name, func(t *testing.T) {
			// 404, not 403: a 403 would confirm the slug exists (docs/11 §3.4).
			if res.status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404. body: %s", res.status, res.body)
			}
		})
	}

	// The owner still sees it.
	res := env.asOwner(t, owner, http.MethodGet, "/api/v1/novels/"+draft.Slug)
	if res.status != http.StatusOK {
		t.Fatalf("the owner cannot read their own draft: %d %s", res.status, res.body)
	}
}

// docs/11 §31: unlisted work is reachable by link but must not be discoverable.
func TestNovels_UnlistedIsReadableButNotListed(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	unlisted := env.publishedNovel(t, w, map[string]any{"visibility": "unlisted"})

	res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+unlisted.Slug)
	if res.status != http.StatusOK {
		t.Fatalf("an unlisted fiction should be readable by link: %d %s", res.status, res.body)
	}

	listed, _ := collectionOf[novelBody](t,
		env.asGuest(t, http.MethodGet, "/api/v1/novels?per_page=100"))
	for _, item := range listed {
		if item.ID == unlisted.ID {
			t.Error("an unlisted fiction appeared in the public listing")
		}
	}
}

func TestNovels_ListExcludesDraftsAndPrivateWork(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	draft := env.createNovel(t, w, createNovelBody(uniqueName(t, "Hidden "), nil))
	published := env.publishedNovel(t, w, nil)

	items, _ := collectionOf[novelBody](t,
		env.asGuest(t, http.MethodGet, "/api/v1/novels?per_page=100"))

	seen := map[string]bool{}
	for _, item := range items {
		seen[item.ID] = true
	}
	if seen[draft.ID] {
		t.Error("a draft appeared in the public listing (docs/11 §31)")
	}
	if !seen[published.ID] {
		t.Error("published work is missing from the public listing")
	}
}

// A writer needs to see their own unpublished work; nobody else does.
func TestNovels_AuthorScopedListingShowsOwnDraftsOnly(t *testing.T) {
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	stranger := env.newWriter(t)

	draft := env.createNovel(t, owner, createNovelBody(uniqueName(t, "My Draft "), nil))
	path := "/api/v1/novels?per_page=100&author=" + owner.username

	own, _ := collectionOf[novelBody](t, env.asOwner(t, owner, http.MethodGet, path))
	found := false
	for _, item := range own {
		if item.ID == draft.ID {
			found = true
			if item.Visibility == nil {
				t.Error("the owner's own listing should carry owner metadata")
			}
		}
	}
	if !found {
		t.Error("the writer cannot see their own draft in their own listing")
	}

	for name, res := range map[string]apiResponse{
		"guest":    env.asGuest(t, http.MethodGet, path),
		"stranger": env.asOwner(t, stranger, http.MethodGet, path),
	} {
		t.Run(name, func(t *testing.T) {
			items, _ := collectionOf[novelBody](t, res)
			for _, item := range items {
				if item.ID == draft.ID {
					t.Error("someone else's draft leaked through the author filter")
				}
			}
		})
	}
}

// An unknown author is an empty page, not a 404: a 404 would turn the listing
// into a username oracle.
func TestNovels_UnknownAuthorFilterReturnsAnEmptyPage(t *testing.T) {
	env := newAuthEnv(t)

	res := env.asGuest(t, http.MethodGet, "/api/v1/novels?author=nobody-here-12345")
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", res.status, res.body)
	}
	items, total := collectionOf[novelBody](t, res)
	if len(items) != 0 || total != 0 {
		t.Errorf("got %d items (total %d), want an empty page", len(items), total)
	}
}

// ---------------------------------------------------------------------------
// Filtering, sorting, search
// ---------------------------------------------------------------------------

// docs/09 §11: format filters are first-class and combinable.
func TestNovels_FiltersByEachFormatDimension(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	target := env.publishedNovel(t, w, map[string]any{
		"story_structure":     "one_shot",
		"presentation_format": "chat",
		"content_mode":        "headcanon",
	})
	other := env.publishedNovel(t, w, map[string]any{
		"story_structure":     "multi_chapter",
		"presentation_format": "standard",
		"content_mode":        "general",
	})

	queries := map[string]string{
		"story_structure":     "story_structure=one_shot",
		"presentation_format": "presentation_format=chat",
		"content_mode":        "content_mode=headcanon",
		"combined":            "story_structure=one_shot&presentation_format=chat&content_mode=headcanon",
	}

	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			items, _ := collectionOf[novelBody](t,
				env.asGuest(t, http.MethodGet, "/api/v1/novels?per_page=100&"+query))

			seen := map[string]bool{}
			for _, item := range items {
				seen[item.ID] = true
			}
			if !seen[target.ID] {
				t.Error("the matching fiction was filtered out")
			}
			if seen[other.ID] {
				t.Error("a non-matching fiction passed the filter")
			}
		})
	}
}

func TestNovels_ListRejectsUnsupportedFilterAndSortValues(t *testing.T) {
	env := newAuthEnv(t)

	// docs/09 §10: the server keeps an allowlist and never injects a
	// user-provided sort value into SQL.
	// "popular" was in this list until Phase 4 implemented it (docs/09 §10);
	// "trending" stands in as the still-unsupported sort value.
	queries := []string{
		"sort=" + url.QueryEscape("id; DROP TABLE novels"),
		"sort=" + url.QueryEscape("title DESC"),
		"sort=trending",
		"story_structure=trilogy",
		"presentation_format=script",
		"content_mode=fanfic",
		"status=archived",
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			res := env.asGuest(t, http.MethodGet, "/api/v1/novels?"+query)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422. body: %s", res.status, res.body)
			}
		})
	}
}

// A search term must be bound as a parameter and its wildcards neutralised, so
// "%" cannot turn a bounded search into a full scan.
func TestNovels_SearchIsInjectionSafe(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, map[string]any{"title": uniqueName(t, "Findable ")})

	hostile := []string{
		"' OR 1=1 --",
		"%",
		"_",
		"\\",
		"'; DROP TABLE novels; --",
		"100% ต้องอ่าน",
	}
	for _, term := range hostile {
		t.Run(term, func(t *testing.T) {
			res := env.asGuest(t, http.MethodGet,
				"/api/v1/novels?q="+url.QueryEscape(term))
			if res.status != http.StatusOK {
				t.Fatalf("status = %d, want 200. body: %s", res.status, res.body)
			}

			// A bare wildcard must not match everything.
			if term == "%" {
				items, _ := collectionOf[novelBody](t, res)
				for _, item := range items {
					if item.ID == novel.ID {
						t.Error(`the "%" wildcard was not escaped; it matched every title`)
					}
				}
			}
		})
	}

	// The table is still there.
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels"); res.status != http.StatusOK {
		t.Fatalf("the listing broke after the injection attempts: %d %s", res.status, res.body)
	}
}

func TestNovels_ListEnforcesThePaginationCeiling(t *testing.T) {
	env := newAuthEnv(t)

	// docs/09 §9: the maximum per_page is enforced server-side.
	var payload struct {
		Meta struct {
			Page    int `json:"page"`
			PerPage int `json:"per_page"`
		} `json:"meta"`
	}
	res := env.asGuest(t, http.MethodGet, "/api/v1/novels?per_page=100000&page=-5")
	res.json(t, &payload)

	if payload.Meta.PerPage > 100 {
		t.Errorf("per_page = %d; the server-side ceiling was not applied", payload.Meta.PerPage)
	}
	if payload.Meta.Page < 1 {
		t.Errorf("page = %d; a negative page must fall back to a safe default", payload.Meta.Page)
	}
}

// ---------------------------------------------------------------------------
// Ownership - IDOR / BOLA (docs/11 §8)
// ---------------------------------------------------------------------------

func TestNovels_WriterCannotModifyAnotherWritersFiction(t *testing.T) {
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	attacker := env.newWriter(t)

	// Published, so the attacker can SEE it - which is what makes 403 the
	// correct answer here rather than 404.
	novel := env.publishedNovel(t, owner, nil)

	attempts := map[string]apiRequest{
		"update": {
			method: http.MethodPatch, path: "/api/v1/novels/" + novel.ID,
			body: map[string]any{"title": "Stolen"},
		},
		"change format": {
			method: http.MethodPatch, path: "/api/v1/novels/" + novel.ID + "/format",
			body: map[string]any{"presentation_format": "chat"},
		},
		"delete": {
			method: http.MethodDelete, path: "/api/v1/novels/" + novel.ID,
		},
		"add a chapter": {
			method: http.MethodPost, path: "/api/v1/novels/" + novel.ID + "/chapters",
			body: map[string]any{"title": "Injected"},
		},
	}

	for name, attempt := range attempts {
		t.Run(name, func(t *testing.T) {
			attempt.cookies = attacker.authCookies()
			attempt.csrf = attacker.csrfToken

			res := env.do(t, attempt)
			if res.status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403. body: %s", res.status, res.body)
			}
		})
	}

	// Nothing changed.
	after := dataOf[novelBody](t, env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID))
	if after.Title != novel.Title {
		t.Errorf("title is now %q, want %q - the attacker's write took effect", after.Title, novel.Title)
	}
	if after.PresentationFormat != novel.PresentationFormat {
		t.Error("the attacker changed the fiction's format")
	}
}

// Someone who cannot even see the fiction gets 404, not 403: a 403 would tell
// them a private draft exists behind that id (docs/11 §3.4, §31).
func TestNovels_PrivateDraftAnswersNotFoundToAnAttacker(t *testing.T) {
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	attacker := env.newWriter(t)

	draft := env.createNovel(t, owner, createNovelBody("Unseen Draft", nil))

	res := env.asOwner(t, attacker, http.MethodPatch, "/api/v1/novels/"+draft.ID,
		map[string]any{"title": "Stolen"})
	if res.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", res.status, res.body)
	}
}

func TestNovels_GuestCannotMutateAnything(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	attempts := map[string]apiRequest{
		"update": {method: http.MethodPatch, path: "/api/v1/novels/" + novel.ID,
			body: map[string]any{"title": "Guest Edit"}},
		"change format": {method: http.MethodPatch, path: "/api/v1/novels/" + novel.ID + "/format",
			body: map[string]any{"content_mode": "headcanon"}},
		"delete": {method: http.MethodDelete, path: "/api/v1/novels/" + novel.ID},
	}

	for name, attempt := range attempts {
		t.Run(name, func(t *testing.T) {
			res := env.do(t, attempt)
			if res.status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401. body: %s", res.status, res.body)
			}
		})
	}
}

// docs/11 §22: a cookie-authenticated mutation without a CSRF token is refused.
func TestNovels_CookieMutationRequiresCSRF(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	// Same session, same cookies - only the header is missing.
	res := env.do(t, apiRequest{
		method:  http.MethodPatch,
		path:    "/api/v1/novels/" + novel.ID,
		body:    map[string]any{"title": "CSRF Attempt"},
		cookies: w.authCookies(),
	})
	if res.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", res.status, res.body)
	}
	if code := errorCodeOf(t, res); code != "CSRF_TOKEN_INVALID" {
		t.Errorf("error code = %q, want CSRF_TOKEN_INVALID", code)
	}

	// With the token it succeeds, proving the rejection was the CSRF check
	// rather than something else about the request.
	if res := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"title": "Legitimate Edit"}); res.status != http.StatusOK {
		t.Fatalf("the same request with a CSRF token failed: %d %s", res.status, res.body)
	}
}

// A Bearer token cannot be sent by a cross-site page, so the exemption from
// Phase 1 must still hold for the new endpoints (docs/11 §22).
func TestNovels_BearerMutationNeedsNoCSRF(t *testing.T) {
	env := newAuthEnv(t)
	token, _, _, _ := env.registerNative(t)

	res := env.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/novels",
		body:   createNovelBody("Written On A Phone", nil),
		bearer: token,
		origin: "-", // no browser Origin at all
	})
	if res.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", res.status, res.body)
	}
}

// ---------------------------------------------------------------------------
// Updates and lifecycle
// ---------------------------------------------------------------------------

func TestNovels_UpdateDoesNotResetUnmentionedFields(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody("Original Title", map[string]any{
		"description":     "A description worth keeping.",
		"content_warning": "Contains storms.",
	}))

	res := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"title": "Renamed"})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", res.status, res.body)
	}

	updated := dataOf[novelBody](t, res)
	if updated.Title != "Renamed" {
		t.Errorf("title = %q, want Renamed", updated.Title)
	}
	if updated.Description == nil || *updated.Description != "A description worth keeping." {
		t.Error("an unmentioned description was reset by a PATCH")
	}
	if updated.ContentWarning == nil {
		t.Error("an unmentioned content warning was reset by a PATCH")
	}
	// docs/09 §15: format changes have their own endpoint.
	if updated.PresentationFormat != novel.PresentationFormat {
		t.Error("a metadata PATCH changed the format")
	}
}

// An explicit null clears the field; that is the difference a PATCH must be able
// to express.
func TestNovels_UpdateClearsAFieldOnExplicitNull(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody("With A Description", map[string]any{
		"description": "Temporary.",
	}))

	res := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"description": nil})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", res.status, res.body)
	}
	if updated := dataOf[novelBody](t, res); updated.Description != nil {
		t.Errorf("description = %v, want nil after an explicit null", *updated.Description)
	}
}

func TestNovels_PublishingStampsThePublicationDate(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody("To Be Published", nil))
	if novel.PublishedAt != nil {
		t.Error("a draft must not carry a publication date")
	}

	// The pre-publish checklist has to be satisfied first (§13L): a fiction
	// with no synopsis, genres, tags, or cover cannot go public at all.
	env.completeChecklist(t, w, novel.ID)

	published := dataOf[novelBody](t, env.asOwner(t, w, http.MethodPatch,
		"/api/v1/novels/"+novel.ID, map[string]any{
			"status": "ongoing", "visibility": "public",
		}))
	if published.PublishedAt == nil {
		t.Fatal("publishing did not stamp published_at")
	}
	first := *published.PublishedAt

	// Unpublish and republish: the original date must survive, or a reader's
	// "published on" would jump because the author toggled a setting.
	env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"visibility": "private"})
	again := dataOf[novelBody](t, env.asOwner(t, w, http.MethodPatch,
		"/api/v1/novels/"+novel.ID, map[string]any{"visibility": "public"}))

	if again.PublishedAt == nil || *again.PublishedAt != first {
		t.Errorf("published_at moved from %v to %v on republication", first, again.PublishedAt)
	}
}

// docs/AUTHENTICATION.md §9: verification gates PUBLISHING, never reading or
// ordinary account use.
func TestNovels_UnverifiedWriterMayDraftButNotPublish(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newUnverifiedWriter(t)

	novel := env.createNovel(t, w, createNovelBody("Draft While Unverified", nil))

	// Editing a draft is unaffected.
	if res := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"title": "Still Drafting"}); res.status != http.StatusOK {
		t.Fatalf("an unverified writer could not edit their own draft: %d %s", res.status, res.body)
	}

	res := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"status": "ongoing", "visibility": "public"})
	if res.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", res.status, res.body)
	}
	if code := errorCodeOf(t, res); code != "EMAIL_VERIFICATION_REQUIRED" {
		t.Errorf("error code = %q, want EMAIL_VERIFICATION_REQUIRED", code)
	}
}

func TestNovels_DeleteIsSoftAndRemovesPublicAccess(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	if res := env.asOwner(t, w, http.MethodDelete, "/api/v1/novels/"+novel.ID,
		nil); res.status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204. body: %s", res.status, res.body)
	}

	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug); res.status != http.StatusNotFound {
		t.Errorf("a deleted fiction is still readable: %d", res.status)
	}
	// docs/08 §37: soft delete, so the row survives for moderation and recovery.
	var deletedAt *string
	if err := env.db.QueryRowContext(t.Context(),
		`SELECT deleted_at FROM novels WHERE id = $1`, novel.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("the row was hard-deleted: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at was not set")
	}

	// The slug is released, so the title can be reused.
	if _, err := env.db.ExecContext(t.Context(),
		`SELECT 1 FROM novels WHERE slug = $1 AND deleted_at IS NULL`, novel.Slug); err != nil {
		t.Fatalf("slug query failed: %v", err)
	}
}
