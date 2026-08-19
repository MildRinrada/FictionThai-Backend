package integration

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
)

// Community feed tools (docs/COMMUNITY-FEED.md): post search, post types,
// bookmarks, and trending hashtags.
//
// Every needle carries a per-test unique marker because the suite runs in
// parallel against one shared database - totals are only ever asserted inside
// a marker's scope, never globally.

// searchPosts runs GET /search/posts with the given query values.
func (e *authEnv) searchPosts(t *testing.T, w *writer, params url.Values) apiResponse {
	t.Helper()
	path := "/api/v1/search/posts?" + params.Encode()
	if w == nil {
		return e.asGuest(t, http.MethodGet, path)
	}
	return e.asOwner(t, *w, http.MethodGet, path)
}

// createTypedPost creates a post with an explicit post_type.
func (e *authEnv) createTypedPost(t *testing.T, w writer, content, postType string) postBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPost, "/api/v1/community/posts",
		map[string]any{"content": content, "post_type": postType})
	if res.status != http.StatusCreated {
		t.Fatalf("create typed post status = %d. body: %s", res.status, res.body)
	}
	return dataOf[postBody](t, res)
}

func TestCommunity_PostSearchVisibility(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newUnverifiedWriter(t)
	authorID := env.whoAmI(t, author)
	follower := env.newUnverifiedWriter(t)
	env.asOwner(t, follower, http.MethodPost, "/api/v1/users/"+authorID+"/follow")

	marker := "คราม" + uuid.NewString()[:8]
	env.createCommunityPost(t, author, "โพสต์สาธารณะเรื่อง "+marker, "public")
	env.createCommunityPost(t, author, "เล่าให้ผู้ติดตามฟังเรื่อง "+marker, "followers")
	env.createCommunityPost(t, author, "บันทึกส่วนตัวเรื่อง "+marker, "private")

	// THE SEARCH RULE: only public posts surface, even for a follower who
	// reads the followers-only post in their feed (docs/COMMUNITY-FEED.md).
	for name, caller := range map[string]*writer{
		"guest": nil, "follower": &follower, "author-as-searcher": &author,
	} {
		res := env.searchPosts(t, caller, url.Values{"q": {marker}})
		if res.status != http.StatusOK {
			t.Fatalf("%s search status = %d. body: %s", name, res.status, res.body)
		}
		if _, total := collectionOf[postBody](t, res); total != 1 {
			t.Errorf("%s search total = %d, want 1 (public only)", name, total)
		}
	}

	// from=me is the one exception: your own posts, whatever their audience.
	res := env.searchPosts(t, &author, url.Values{"q": {marker}, "from": {"me"}})
	if _, total := collectionOf[postBody](t, res); total != 3 {
		t.Errorf("from=me total = %d, want 3", total)
	}

	// from:@yourself by handle behaves the same as from=me.
	res = env.searchPosts(t, &author, url.Values{"q": {marker}, "author": {"@" + author.username}})
	if _, total := collectionOf[postBody](t, res); total != 3 {
		t.Errorf("author=self total = %d, want 3", total)
	}

	// A stranger filtering by that author's handle still gets public only.
	res = env.searchPosts(t, &follower, url.Values{"q": {marker}, "author": {author.username}})
	if _, total := collectionOf[postBody](t, res); total != 1 {
		t.Errorf("author=other total = %d, want 1", total)
	}

	// from=following scopes to followed authors' public posts.
	res = env.searchPosts(t, &follower, url.Values{"q": {marker}, "from": {"following"}})
	if _, total := collectionOf[postBody](t, res); total != 1 {
		t.Errorf("from=following total = %d, want 1", total)
	}

	// Unknown author: an empty page, never an oracle.
	res = env.searchPosts(t, nil, url.Values{"q": {marker}, "author": {"nobody" + uuid.NewString()[:8]}})
	if _, total := collectionOf[postBody](t, res); total != 0 {
		t.Errorf("unknown author total = %d, want 0", total)
	}

	// Personal scopes need an account; malformed enums are named 422s.
	if res := env.searchPosts(t, nil, url.Values{"q": {marker}, "from": {"following"}}); res.status != http.StatusUnauthorized {
		t.Errorf("guest from=following = %d, want 401", res.status)
	}
	if res := env.searchPosts(t, nil, url.Values{"q": {marker}, "from": {"me"}}); res.status != http.StatusUnauthorized {
		t.Errorf("guest from=me = %d, want 401", res.status)
	}
	if res := env.searchPosts(t, nil, url.Values{"q": {marker}, "range": {"1y"}}); res.status != http.StatusUnprocessableEntity {
		t.Errorf("bad range = %d, want 422", res.status)
	}
	if res := env.searchPosts(t, nil, url.Values{}); res.status != http.StatusUnprocessableEntity {
		t.Errorf("empty search = %d, want 422", res.status)
	}
}

func TestCommunity_PostSearchFiltersAndSort(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newUnverifiedWriter(t)
	reader := env.newUnverifiedWriter(t)
	marker := "นิล" + uuid.NewString()[:8]

	quiet := env.createCommunityPost(t, author, "โพสต์เงียบ ๆ เรื่อง "+marker, "public")
	loud := env.createCommunityPost(t, author, "โพสต์ดัง เรื่อง "+marker, "public")
	res := env.asOwner(t, reader, http.MethodPost,
		"/api/v1/community/posts/"+loud.ID+"/reactions", map[string]any{"type": "like"})
	if res.status != http.StatusOK {
		t.Fatalf("react status = %d. body: %s", res.status, res.body)
	}

	// sort=top puts the reacted post first; sort=new the newest.
	res = env.searchPosts(t, nil, url.Values{"q": {marker}, "sort": {"top"}})
	if items, _ := collectionOf[postBody](t, res); len(items) != 2 || items[0].ID != loud.ID {
		t.Errorf("sort=top order wrong (want %s first): %+v", loud.ID, ids(items))
	}
	res = env.searchPosts(t, nil, url.Values{"q": {marker}, "sort": {"new"}})
	if items, _ := collectionOf[postBody](t, res); len(items) != 2 || items[0].ID != loud.ID {
		// loud was created second, so it is also the newest.
		t.Errorf("sort=new order wrong: %+v", ids(items))
	}
	_ = quiet

	// has=none matches these unattached posts; has=chapter matches neither.
	res = env.searchPosts(t, nil, url.Values{"q": {marker}, "has": {"none"}})
	if _, total := collectionOf[postBody](t, res); total != 2 {
		t.Errorf("has=none total = %d, want 2", total)
	}
	res = env.searchPosts(t, nil, url.Values{"q": {marker}, "has": {"chapter"}})
	if _, total := collectionOf[postBody](t, res); total != 0 {
		t.Errorf("has=chapter total = %d, want 0", total)
	}

	// mention= finds posts whose text names the handle.
	mentionMarker := "พูดถึง" + uuid.NewString()[:8]
	env.createCommunityPost(t, author,
		"ชวน @"+reader.username+" มาอ่านเรื่อง "+mentionMarker, "public")
	res = env.searchPosts(t, nil, url.Values{"mention": {reader.username}, "q": {mentionMarker}})
	if _, total := collectionOf[postBody](t, res); total != 1 {
		t.Errorf("mention total = %d, want 1", total)
	}

	// ILIKE wildcards are data, not syntax: "%" finds nothing rather than
	// everything.
	res = env.searchPosts(t, nil, url.Values{"q": {"%" + uuid.NewString()}})
	if _, total := collectionOf[postBody](t, res); total != 0 {
		t.Errorf("wildcard needle total = %d, want 0", total)
	}
}

func ids(items []postBody) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ID
	}
	return out
}

func TestCommunity_PostTypes(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newUnverifiedWriter(t)
	marker := "เบต้า" + uuid.NewString()[:8]

	// Default is discussion; a declared type round-trips.
	plain := env.createCommunityPost(t, author, "โพสต์ธรรมดา "+marker, "")
	if plain.PostType != "discussion" {
		t.Errorf("default post_type = %q, want discussion", plain.PostType)
	}
	beta := env.createTypedPost(t, author, "หาเบต้าแนวแฟนตาซี "+marker, "beta_request")
	if beta.PostType != "beta_request" {
		t.Errorf("post_type = %q, want beta_request", beta.PostType)
	}

	// Unknown types are a 422, on create and on edit.
	res := env.asOwner(t, author, http.MethodPost, "/api/v1/community/posts",
		map[string]any{"content": "โพลล์?", "post_type": "poll"})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("create poll type = %d, want 422", res.status)
	}
	res = env.asOwner(t, author, http.MethodPatch, "/api/v1/community/posts/"+beta.ID,
		map[string]any{"post_type": "poll"})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("patch poll type = %d, want 422", res.status)
	}

	// A type change is a metadata edit: content untouched.
	res = env.asOwner(t, author, http.MethodPatch, "/api/v1/community/posts/"+beta.ID,
		map[string]any{"post_type": "plot_help"})
	if res.status != http.StatusOK {
		t.Fatalf("patch type status = %d. body: %s", res.status, res.body)
	}
	patched := dataOf[postBody](t, res)
	if patched.PostType != "plot_help" || patched.Content != beta.Content {
		t.Errorf("type patch mangled the post: %+v", patched)
	}

	// The feed and search both filter by type; the search still needs the
	// marker to stay inside this test's scope.
	res = env.asGuest(t, http.MethodGet,
		"/api/v1/community/posts?type=beta_request&author="+author.username)
	if _, total := collectionOf[postBody](t, res); total != 0 {
		t.Errorf("feed type=beta_request after patch total = %d, want 0", total)
	}
	res = env.searchPosts(t, nil, url.Values{"q": {marker}, "type": {"plot_help"}})
	if _, total := collectionOf[postBody](t, res); total != 1 {
		t.Errorf("search type=plot_help total = %d, want 1", total)
	}
	if res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts?type=poll"); res.status != http.StatusUnprocessableEntity {
		t.Errorf("feed type=poll = %d, want 422", res.status)
	}
}

func TestCommunity_Bookmarks(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newUnverifiedWriter(t)
	reader := env.newUnverifiedWriter(t)

	first := env.createCommunityPost(t, author, "โพสต์แรกไว้บันทึก", "public")
	second := env.createCommunityPost(t, author, "โพสต์สองไว้บันทึก", "public")

	// Guests have no saved feed and cannot save.
	if res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts?feed=saved"); res.status != http.StatusUnauthorized {
		t.Errorf("guest saved feed = %d, want 401", res.status)
	}
	if res := env.asGuest(t, http.MethodPost, "/api/v1/community/posts/"+first.ID+"/bookmark"); res.status != http.StatusUnauthorized {
		t.Errorf("guest bookmark = %d, want 401", res.status)
	}

	// Save both; saving twice is the same bookmark (docs/09 §33).
	for _, id := range []string{first.ID, first.ID, second.ID} {
		res := env.asOwner(t, reader, http.MethodPost, "/api/v1/community/posts/"+id+"/bookmark")
		if res.status != http.StatusOK {
			t.Fatalf("bookmark status = %d. body: %s", res.status, res.body)
		}
	}

	// The saved feed reads in save order, newest save first, and the cards
	// carry bookmarked=true for their owner.
	res := env.asOwner(t, reader, http.MethodGet, "/api/v1/community/posts?feed=saved")
	items, total := collectionOf[postBody](t, res)
	if total != 2 || len(items) != 2 {
		t.Fatalf("saved feed total = %d (%d items), want 2", total, len(items))
	}
	if items[0].ID != second.ID || items[1].ID != first.ID {
		t.Errorf("saved order = %v, want [%s %s]", ids(items), second.ID, first.ID)
	}
	if !items[0].Bookmarked || !items[1].Bookmarked {
		t.Errorf("saved feed cards must carry bookmarked=true: %+v", items)
	}

	// Someone else's feed shows bookmarked=false for the same posts.
	res = env.asOwner(t, author, http.MethodGet, "/api/v1/community/posts?author="+author.username)
	authorItems, _ := collectionOf[postBody](t, res)
	for _, item := range authorItems {
		if item.Bookmarked {
			t.Errorf("author sees a stranger's bookmark on %s", item.ID)
		}
	}

	// A post whose audience narrows stays saved but stops listing - the
	// bookmark must never become a keyhole (docs/COMMUNITY-FEED.md).
	res = env.asOwner(t, author, http.MethodPatch, "/api/v1/community/posts/"+second.ID,
		map[string]any{"visibility": "private"})
	if res.status != http.StatusOK {
		t.Fatalf("narrow visibility status = %d", res.status)
	}
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/community/posts?feed=saved")
	if items, _ := collectionOf[postBody](t, res); len(items) != 1 || items[0].ID != first.ID {
		t.Errorf("saved feed after narrowing = %v, want [%s]", ids(items), first.ID)
	}

	// Taking a bookmark back always works - even on the now-private post -
	// and is idempotent.
	for i := 0; i < 2; i++ {
		res = env.asOwner(t, reader, http.MethodDelete, "/api/v1/community/posts/"+second.ID+"/bookmark")
		if res.status != http.StatusOK {
			t.Errorf("unbookmark #%d status = %d", i+1, res.status)
		}
	}

	// Saving a post outside your audience is the same 404 as a missing one.
	if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/community/posts/"+second.ID+"/bookmark"); res.status != http.StatusNotFound {
		t.Errorf("bookmark private post = %d, want 404", res.status)
	}
	if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/community/posts/"+uuid.NewString()+"/bookmark"); res.status != http.StatusNotFound {
		t.Errorf("bookmark unknown post = %d, want 404", res.status)
	}

	// feed=mine shows the author everything of their own.
	res = env.asOwner(t, author, http.MethodGet, "/api/v1/community/posts?feed=mine")
	if _, total := collectionOf[postBody](t, res); total != 2 {
		t.Errorf("feed=mine total = %d, want 2", total)
	}
}

func TestCommunity_TrendingTags(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newUnverifiedWriter(t)

	// A unique tag scopes this test against the shared database.
	tag := "แท็ก" + uuid.NewString()[:8]

	post := env.createCommunityPost(t, author, "ชวนคุยเรื่องใหม่ #"+tag+" กันค่ะ", "public")
	env.createCommunityPost(t, author, "โน้ตส่วนตัว #"+tag, "private")

	// Only the public post counts, for everyone, without an account.
	res := env.asGuest(t, http.MethodGet, "/api/v1/community/tags?q="+url.QueryEscape(tag))
	if res.status != http.StatusOK {
		t.Fatalf("tags status = %d. body: %s", res.status, res.body)
	}
	tags := dataOf[[]struct {
		Tag       string `json:"tag"`
		PostCount int64  `json:"post_count"`
	}](t, res)
	if len(tags) != 1 || tags[0].Tag != tag || tags[0].PostCount != 1 {
		t.Fatalf("trending tags = %+v, want [{%s 1}]", tags, tag)
	}

	// Searching "#tag" matches by the extracted tag, exactly.
	res = env.searchPosts(t, nil, url.Values{"q": {"#" + tag}})
	if items, total := collectionOf[postBody](t, res); total != 1 || items[0].ID != post.ID {
		t.Errorf("#tag search = %v (total %d), want the public post", ids(items), total)
	}

	// Editing the tag out of the content re-extracts: the tag stops counting
	// and stops matching, because the rows are derived from the text.
	res = env.asOwner(t, author, http.MethodPatch, "/api/v1/community/posts/"+post.ID,
		map[string]any{"content": "แก้ไขแล้ว ไม่มีแท็กแล้วนะ"})
	if res.status != http.StatusOK {
		t.Fatalf("edit status = %d", res.status)
	}
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/tags?q="+url.QueryEscape(tag))
	if tags := dataOf[[]struct {
		Tag string `json:"tag"`
	}](t, res); len(tags) != 0 {
		t.Errorf("tag survives a content edit: %+v", tags)
	}
	res = env.searchPosts(t, nil, url.Values{"q": {"#" + tag}})
	if _, total := collectionOf[postBody](t, res); total != 0 {
		t.Errorf("#tag search after edit total = %d, want 0", total)
	}
}
