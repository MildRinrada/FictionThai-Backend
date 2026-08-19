package integration

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
)

// Phase 4 - Discovery over the real HTTP API.
//
// The properties that matter most: the two vocabularies stay distinct, tags
// can never impersonate format metadata (docs/08 §15.2), term filters compose
// with everything that already exists, and none of it ever exposes a private
// draft (docs/11 §31).

type genreBody struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Slug        string  `json:"slug"`
	Description *string `json:"description"`
}

type tagBody struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	NovelCount int64  `json:"novel_count"`
}

type termBody struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// novelWithTermsBody extends the fiction resource with its discovery metadata.
type novelWithTermsBody struct {
	novelBody
	Genres []termBody `json:"genres"`
	Tags   []termBody `json:"tags"`
}

// seededGenres loads the controlled vocabulary through the API.
func (e *authEnv) seededGenres(t *testing.T) map[string]genreBody {
	t.Helper()
	res := e.asGuest(t, http.MethodGet, "/api/v1/genres")
	if res.status != http.StatusOK {
		t.Fatalf("list genres status = %d. body: %s", res.status, res.body)
	}
	list := dataOf[[]genreBody](t, res)
	genres := make(map[string]genreBody, len(list))
	for _, genre := range list {
		genres[genre.Slug] = genre
	}
	return genres
}

// newTag creates (or resolves) a tag as the given writer.
func (e *authEnv) newTag(t *testing.T, w writer, name string) tagBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPost, "/api/v1/tags", map[string]any{"name": name})
	if res.status != http.StatusOK {
		t.Fatalf("create tag %q status = %d. body: %s", name, res.status, res.body)
	}
	return dataOf[tagBody](t, res)
}

// ---------------------------------------------------------------------------
// Vocabularies
// ---------------------------------------------------------------------------

func TestGenres_SeededVocabularyIsPublic(t *testing.T) {
	env := newAuthEnv(t)

	genres := env.seededGenres(t)

	// The documented examples (docs/08 §14.1) arrive seeded.
	for _, slug := range []string{"fantasy", "romance", "horror", "mystery", "sci-fi", "comedy", "drama"} {
		if _, ok := genres[slug]; !ok {
			t.Errorf("seeded genre %q is missing", slug)
		}
	}
}

func TestTags_CreateIsValidatedNormalizedAndIdempotent(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)

	name := uniqueName(t, "slow-burn-")

	first := env.newTag(t, writer, name)
	// The same name - and a differently-cased spelling of it - resolve to the
	// SAME row (docs/09 §33): a vocabulary must not fork on capitalisation.
	second := env.newTag(t, writer, name)
	if first.ID != second.ID {
		t.Errorf("repeat tag creation forked the vocabulary: %s vs %s", first.ID, second.ID)
	}
	cased := env.newTag(t, writer, "SLOW-BURN-"+name[len("slow-burn-"):])
	if cased.ID != first.ID {
		t.Errorf("cased spelling forked the vocabulary: %s vs %s", cased.ID, first.ID)
	}

	// A guest cannot create tags.
	if res := env.asGuest(t, http.MethodPost, "/api/v1/tags"); res.status != http.StatusUnauthorized {
		t.Errorf("guest create tag = %d, want 401", res.status)
	}

	// docs/08 §15.2: format metadata must not be duplicated as tags.
	for _, forbidden := range []string{"one-shot", "chat-fiction", "headcanon", "Chat"} {
		res := env.asOwner(t, writer, http.MethodPost, "/api/v1/tags", map[string]any{"name": forbidden})
		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("format tag %q status = %d, want 422. body: %s", forbidden, res.status, res.body)
		}
	}

	// Naming rules: empty, over-long, and control characters are rejected.
	for name, body := range map[string]map[string]any{
		"empty":    {"name": "   "},
		"too long": {"name": uniqueName(t, "ยาวเกินไปมาก-ยาวเกินไปมาก-ยาวเกินไปมาก-ยาวเกิน-")},
		"symbols":  {"name": "tag<script>"},
	} {
		res := env.asOwner(t, writer, http.MethodPost, "/api/v1/tags", body)
		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("%s tag: status = %d, want 422. body: %s", name, res.status, res.body)
		}
	}
}

func TestTags_BrowseCountsOnlyPublicFiction(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)

	tag := env.newTag(t, writer, uniqueName(t, "แท็กลับ-"))

	// The tag is used ONLY on a private draft.
	env.createNovel(t, writer, createNovelBody(uniqueName(t, "Draft "), map[string]any{
		"tag_ids": []string{tag.ID},
	}))

	res := env.asGuest(t, http.MethodGet, "/api/v1/tags?q="+url.QueryEscape(tag.Name))
	if res.status != http.StatusOK {
		t.Fatalf("browse tags status = %d. body: %s", res.status, res.body)
	}
	tags, _ := collectionOf[tagBody](t, res)
	for _, found := range tags {
		if found.ID == tag.ID && found.NovelCount != 0 {
			// The tag itself may appear - it is vocabulary - but its count must
			// not advertise unpublished work (docs/11 §31).
			t.Errorf("tag on a private draft reports novel_count = %d, want 0", found.NovelCount)
		}
	}
}

// ---------------------------------------------------------------------------
// Assignment on the fiction
// ---------------------------------------------------------------------------

func TestNovels_CreateWithGenresAndTags(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)
	genres := env.seededGenres(t)
	tag := env.newTag(t, writer, uniqueName(t, "found-family-"))

	// A DRAFT: this test is about term assignment, and creating the fiction
	// already published would drag in the pre-publish checklist (§13L), which
	// is a different rule tested elsewhere.
	res := env.asOwner(t, writer, http.MethodPost, "/api/v1/novels", createNovelBody(
		uniqueName(t, "Fiction "), map[string]any{
			// A duplicate id must not count twice against the limit.
			"genre_ids": []string{genres["fantasy"].ID, genres["romance"].ID, genres["fantasy"].ID},
			"tag_ids":   []string{tag.ID},
		}))
	if res.status != http.StatusCreated {
		t.Fatalf("create status = %d. body: %s", res.status, res.body)
	}
	novel := dataOf[novelWithTermsBody](t, res)

	if len(novel.Genres) != 2 {
		t.Fatalf("genres = %d, want 2 (deduplicated)", len(novel.Genres))
	}
	if len(novel.Tags) != 1 || novel.Tags[0].ID != tag.ID {
		t.Fatalf("tags = %+v, want the one assigned tag", novel.Tags)
	}

	// The reader's view carries the same terms. The owner asks, because the
	// fiction is still a private draft - a guest reading it would be the leak
	// docs/11 §31 forbids, not a passing assertion.
	read := env.asOwner(t, writer, http.MethodGet, "/api/v1/novels/"+novel.ID)
	if read.status != http.StatusOK {
		t.Fatalf("read status = %d", read.status)
	}
	readNovel := dataOf[novelWithTermsBody](t, read)
	if len(readNovel.Genres) != 2 || len(readNovel.Tags) != 1 {
		t.Errorf("reader view genres/tags = %d/%d, want 2/1", len(readNovel.Genres), len(readNovel.Tags))
	}
}

func TestNovels_TermAssignmentValidation(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)
	genres := env.seededGenres(t)

	// Over the genre limit.
	tooMany := []string{}
	for _, genre := range genres {
		tooMany = append(tooMany, genre.ID)
	}
	res := env.asOwner(t, writer, http.MethodPost, "/api/v1/novels", createNovelBody(
		uniqueName(t, "Fiction "), map[string]any{"genre_ids": tooMany}))
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("over genre limit = %d, want 422. body: %s", res.status, res.body)
	}

	// An unknown genre id is a field error, not a 500 from a constraint.
	res = env.asOwner(t, writer, http.MethodPost, "/api/v1/novels", createNovelBody(
		uniqueName(t, "Fiction "), map[string]any{"genre_ids": []string{uuid.NewString()}}))
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("unknown genre id = %d, want 422. body: %s", res.status, res.body)
	}

	// Malformed ids are rejected at the boundary.
	res = env.asOwner(t, writer, http.MethodPost, "/api/v1/novels", createNovelBody(
		uniqueName(t, "Fiction "), map[string]any{"tag_ids": []string{"not-a-uuid"}}))
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("malformed tag id = %d, want 422. body: %s", res.status, res.body)
	}
}

func TestNovels_PatchReplacesAndClearsTerms(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)
	genres := env.seededGenres(t)
	tag := env.newTag(t, writer, uniqueName(t, "isekai-"))

	novel := env.createNovel(t, writer, createNovelBody(uniqueName(t, "Fiction "), map[string]any{
		"genre_ids": []string{genres["fantasy"].ID},
		"tag_ids":   []string{tag.ID},
	}))

	// A PATCH that does not mention terms leaves them untouched.
	res := env.asOwner(t, writer, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"description": "อัปเดตเรื่องย่อ"})
	if res.status != http.StatusOK {
		t.Fatalf("patch status = %d. body: %s", res.status, res.body)
	}
	updated := dataOf[novelWithTermsBody](t, res)
	if len(updated.Genres) != 1 || len(updated.Tags) != 1 {
		t.Fatalf("terms changed by an unrelated PATCH: genres=%d tags=%d", len(updated.Genres), len(updated.Tags))
	}

	// A present list REPLACES the set; an empty list clears it.
	res = env.asOwner(t, writer, http.MethodPatch, "/api/v1/novels/"+novel.ID, map[string]any{
		"genre_ids": []string{genres["horror"].ID},
		"tag_ids":   []string{},
	})
	if res.status != http.StatusOK {
		t.Fatalf("patch terms status = %d. body: %s", res.status, res.body)
	}
	updated = dataOf[novelWithTermsBody](t, res)
	if len(updated.Genres) != 1 || updated.Genres[0].Slug != "horror" {
		t.Errorf("genres after replace = %+v, want [horror]", updated.Genres)
	}
	if len(updated.Tags) != 0 {
		t.Errorf("tags after clearing = %+v, want none", updated.Tags)
	}
}

// ---------------------------------------------------------------------------
// Filtering, sorting, search
// ---------------------------------------------------------------------------

func TestNovels_FilterByGenreAndTag(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)
	genres := env.seededGenres(t)
	tag := env.newTag(t, writer, uniqueName(t, "school-life-"))

	fantasy := env.publishedNovel(t, writer, map[string]any{
		"genre_ids": []string{genres["fantasy"].ID},
		"tag_ids":   []string{tag.ID},
	})
	env.publishedNovel(t, writer, map[string]any{
		"genre_ids": []string{genres["horror"].ID},
	})
	// A DRAFT in the same genre must never surface (docs/11 §31).
	env.createNovel(t, writer, createNovelBody(uniqueName(t, "Draft "), map[string]any{
		"genre_ids": []string{genres["fantasy"].ID},
	}))

	author := fantasy.Author.Username

	// Genre filter, scoped to this test's author for isolation.
	list := env.asGuest(t, http.MethodGet, "/api/v1/novels?genre=fantasy&author="+url.QueryEscape(author))
	novels, total := collectionOf[novelWithTermsBody](t, list)
	if total != 1 || len(novels) != 1 || novels[0].ID != fantasy.ID {
		t.Fatalf("?genre=fantasy returned %d results, want just the published fantasy fiction", total)
	}

	// Tag filter composes with a format filter (docs/09 §11).
	list = env.asGuest(t, http.MethodGet,
		"/api/v1/novels?tag="+url.QueryEscape(tag.Slug)+"&presentation_format=standard&author="+url.QueryEscape(author))
	_, total = collectionOf[novelWithTermsBody](t, list)
	if total != 1 {
		t.Errorf("?tag=&presentation_format= total = %d, want 1", total)
	}

	// An unknown slug is an empty page, not an oracle.
	list = env.asGuest(t, http.MethodGet, "/api/v1/novels?genre=no-such-genre&author="+url.QueryEscape(author))
	if list.status != http.StatusOK {
		t.Fatalf("unknown genre status = %d, want 200", list.status)
	}
	if _, total = collectionOf[novelWithTermsBody](t, list); total != 0 {
		t.Errorf("unknown genre total = %d, want 0", total)
	}

	// A malformed slug is a clean 422 (docs/09 §36).
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels?genre=%3Cscript%3E"); res.status != http.StatusUnprocessableEntity {
		t.Errorf("malformed genre = %d, want 422", res.status)
	}
}

func TestNovels_SortByPopular(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)

	quiet := env.publishedNovel(t, writer, nil)
	popular := env.publishedNovel(t, writer, nil)

	// Two readers bookmark one fiction; nobody bookmarks the other.
	for range 2 {
		reader := env.newWriter(t)
		if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+popular.ID+"/bookmark", nil); res.status != http.StatusNoContent {
			t.Fatalf("bookmark status = %d. body: %s", res.status, res.body)
		}
	}

	list := env.asGuest(t, http.MethodGet,
		"/api/v1/novels?sort=popular&author="+url.QueryEscape(quiet.Author.Username))
	novels, _ := collectionOf[novelWithTermsBody](t, list)
	if len(novels) != 2 {
		t.Fatalf("results = %d, want 2", len(novels))
	}
	// docs/09 §10 sort=popular: the bookmarked fiction ranks first.
	if novels[0].ID != popular.ID {
		t.Errorf("sort=popular put %q first, want the bookmarked fiction", novels[0].Title)
	}
}

func TestSearch_MatchesDocumentedScope(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)
	genres := env.seededGenres(t)
	tag := env.newTag(t, writer, uniqueName(t, "เจ้าหญิงนักดาบ-"))

	novel := env.publishedNovel(t, writer, map[string]any{
		"title":       uniqueName(t, "มหากาพย์ดวงดาว "),
		"description": "การเดินทางของยานสำรวจลำสุดท้าย " + uniqueName(t, "คำพิเศษ"),
		"genre_ids":   []string{genres["sci-fi"].ID},
		"tag_ids":     []string{tag.ID},
	})

	// docs/01 §7: title, author, tags, genre - plus description - all match.
	for scope, q := range map[string]string{
		"title":       novel.Title,
		"author":      novel.Author.Username,
		"tag name":    tag.Name,
		"description": "การเดินทางของยานสำรวจ",
	} {
		res := env.asGuest(t, http.MethodGet, "/api/v1/search/novels?q="+url.QueryEscape(q))
		if res.status != http.StatusOK {
			t.Fatalf("search by %s status = %d. body: %s", scope, res.status, res.body)
		}
		results, _ := collectionOf[novelWithTermsBody](t, res)
		found := false
		for _, result := range results {
			if result.ID == novel.ID {
				found = true
			}
		}
		if !found {
			t.Errorf("search by %s (%q) did not find the fiction", scope, q)
		}
	}

	// A query is required - search is not an unbounded listing (docs/09 §22).
	if res := env.asGuest(t, http.MethodGet, "/api/v1/search/novels"); res.status != http.StatusUnprocessableEntity {
		t.Errorf("empty search query = %d, want 422", res.status)
	}
}

func TestSearch_NeverExposesUnpublishedWork(t *testing.T) {
	env := newAuthEnv(t)
	writer := env.newWriter(t)

	secret := uniqueName(t, "ความลับสุดยอด-")
	draft := env.createNovel(t, writer, createNovelBody(secret, nil))

	// Not for a guest…
	res := env.asGuest(t, http.MethodGet, "/api/v1/search/novels?q="+url.QueryEscape(secret))
	if _, total := collectionOf[novelWithTermsBody](t, res); total != 0 {
		t.Errorf("guest search found a private draft (total = %d)", total)
	}

	// …and not even for its OWNER: search is a public surface, and the
	// author's shelf is ?author= on the listing, not search (docs/11 §31).
	res = env.asOwner(t, writer, http.MethodGet, "/api/v1/search/novels?q="+url.QueryEscape(secret))
	if _, total := collectionOf[novelWithTermsBody](t, res); total != 0 {
		t.Errorf("owner search surfaced their private draft %q (total != 0)", draft.Slug)
	}
}
