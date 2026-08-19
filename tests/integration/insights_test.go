package integration

import (
	"net/http"
	"testing"
)

// Phase 13R - the studio overview's numbers and activity feed.
//
// Three properties, and each test below is one of them:
//
//   - It is OWNER-ONLY, with the reader-identical 404, so the endpoint cannot
//     be used to learn that someone's private draft exists.
//   - The numbers are real. "คอมเมนต์ใหม่" counts comments that actually
//     arrived, whatever their moderation status - a writer holding comments for
//     review is exactly the writer who needs to know some are waiting.
//   - The activity feed carries display names and places, and nothing that
//     could become a per-reader record. There is no reader id in the response
//     and no query behind it that could produce one.

type insightsBody struct {
	WeeklyViews        int64   `json:"weekly_views"`
	WeeklyComments     int64   `json:"weekly_comments"`
	PrevWeeklyViews    int64   `json:"prev_weekly_views"`
	PrevWeeklyComments int64   `json:"prev_weekly_comments"`
	ViewsByDay         []int64 `json:"views_by_day"`
	CommentsByDay      []int64 `json:"comments_by_day"`
	WindowDays         int     `json:"window_days"`
	Activity           []struct {
		Kind         string `json:"kind"`
		Actor        string `json:"actor"`
		Excerpt      string `json:"excerpt"`
		ChapterSlug  string `json:"chapter_slug"`
		ChapterLabel string `json:"chapter_label"`
		PostID       string `json:"post_id"`
		CreatedAt    string `json:"created_at"`
	} `json:"activity"`
}

func (e *authEnv) insights(t *testing.T, w writer, novelRef string) insightsBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/novels/"+novelRef+"/insights")
	if res.status != http.StatusOK {
		t.Fatalf("insights status = %d, want 200. body: %s", res.status, res.body)
	}
	return dataOf[insightsBody](t, res)
}

// A fiction with nothing happening reports zeros and an EMPTY array - never a
// null the client has to defend against, and never an error page in front of a
// writer's own studio.
func TestInsights_AnUntouchedFictionReportsZeros(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	got := env.insights(t, w, novel.Slug)
	if got.WeeklyViews != 0 || got.WeeklyComments != 0 {
		t.Fatalf("a fiction nobody has touched reports %+v", got)
	}
	if got.PrevWeeklyViews != 0 || got.PrevWeeklyComments != 0 {
		t.Fatalf("last week is zero too: %+v", got)
	}
	if got.WindowDays != 7 {
		t.Fatalf("window_days = %d, want 7", got.WindowDays)
	}
	// The daily series is always exactly the window, zero-filled (§13T) - a
	// sparkline handed a sparse array would draw a lie.
	if len(got.ViewsByDay) != got.WindowDays {
		t.Errorf("views_by_day has %d entries, want %d", len(got.ViewsByDay), got.WindowDays)
	}
	if len(got.CommentsByDay) != got.WindowDays {
		t.Errorf("comments_by_day has %d entries, want %d", len(got.CommentsByDay), got.WindowDays)
	}
	for i, views := range got.ViewsByDay {
		if views != 0 {
			t.Errorf("views_by_day[%d] = %d on an untouched fiction", i, views)
		}
	}
	if got.Activity == nil {
		t.Error("activity must be an empty array, not null")
	}
}

// Comments count, and each one becomes a line naming the chapter it landed on.
func TestInsights_CountsCommentsAndNamesWhereTheyLanded(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, w, nil)
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title":   "น้ำขึ้นตอนตีสาม",
		"content": "ฝนตกทั้งคืน",
	})
	env.publishChapter(t, w, novel.ID, chapter.ID)

	res := env.asOwner(t, reader, http.MethodPost,
		"/api/v1/novels/"+novel.Slug+"/chapters/"+chapter.Slug+"/comments",
		map[string]any{"content": "อ่านแล้วร้องเลย"})
	if res.status != http.StatusCreated {
		t.Fatalf("comment status = %d, want 201. body: %s", res.status, res.body)
	}

	got := env.insights(t, w, novel.Slug)
	if got.WeeklyComments != 1 {
		t.Fatalf("weekly_comments = %d, want 1", got.WeeklyComments)
	}
	// The daily series agrees with the weekly sum, and today is the LAST entry.
	var daySum int64
	for _, comments := range got.CommentsByDay {
		daySum += comments
	}
	if daySum != got.WeeklyComments {
		t.Errorf("comments_by_day sums to %d, weekly_comments says %d", daySum, got.WeeklyComments)
	}
	if last := got.CommentsByDay[len(got.CommentsByDay)-1]; last != 1 {
		t.Errorf("today (last entry) = %d, want 1: %v", last, got.CommentsByDay)
	}
	if len(got.Activity) != 1 {
		t.Fatalf("activity = %d lines, want 1: %+v", len(got.Activity), got.Activity)
	}

	line := got.Activity[0]
	if line.Kind != "comment" {
		t.Errorf("kind = %q, want comment", line.Kind)
	}
	if line.Actor == "" {
		t.Error("a line with no actor is a line nobody can read")
	}
	if line.ChapterSlug != chapter.Slug {
		t.Errorf("chapter_slug = %q, want %q", line.ChapterSlug, chapter.Slug)
	}
	if line.ChapterLabel != "น้ำขึ้นตอนตีสาม" {
		t.Errorf("chapter_label = %q, want the chapter's title", line.ChapterLabel)
	}
	if line.Excerpt == "" {
		t.Error("the excerpt is what makes the line worth reading")
	}
}

// A community post that attached the fiction lands in the same timeline, so a
// writer sees both ways someone talked about their work in one place.
func TestInsights_IncludesCommunityPostsAboutTheFiction(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.publishedNovel(t, w, nil)

	post := env.attachedPost(t, w, "ตอนใหม่ลงแล้วนะ", novel.ID, "")

	got := env.insights(t, w, novel.Slug)
	found := false
	for _, line := range got.Activity {
		if line.Kind == "community_post" && line.PostID == post.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the post about this fiction is not in the feed: %+v", got.Activity)
	}
}

// Someone else's studio is not readable, and the refusal follows the novels
// service's own rule rather than a second one invented here: a fiction the
// stranger can already OPEN is 403 (there is nothing left to leak), and a
// private one is the same 404 they would get for a slug that does not exist
// (docs/11 §3.4).
func TestInsights_RefusesSomeoneElsesFiction(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	stranger := env.newWriter(t)

	published := env.publishedNovel(t, owner, nil)
	res := env.asOwner(t, stranger, http.MethodGet,
		"/api/v1/novels/"+published.Slug+"/insights")
	if res.status != http.StatusForbidden {
		t.Fatalf("stranger insights on a public fiction status = %d, want 403. body: %s",
			res.status, res.body)
	}

	// The one that matters: a private draft must not be confirmed to exist.
	private := env.createNovel(t, owner, map[string]any{
		"title":      "ฉบับร่างที่ยังไม่ให้ใครอ่าน",
		"age_rating": "general",
	})
	hidden := env.asOwner(t, stranger, http.MethodGet,
		"/api/v1/novels/"+private.Slug+"/insights")
	if hidden.status != http.StatusNotFound {
		t.Fatalf("stranger insights on a private draft status = %d, want 404. body: %s",
			hidden.status, hidden.body)
	}

	guest := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+published.Slug+"/insights")
	if guest.status != http.StatusUnauthorized && guest.status != http.StatusNotFound {
		t.Fatalf("guest insights status = %d, want 401 or 404. body: %s",
			guest.status, guest.body)
	}
}

// The community filter behind "โพสต์ชุมชนที่พูดถึงเรื่องนี้" (§13R).
//
// It narrows the ordinary feed and does not widen anything: the same visibility
// rule applies, so this cannot become a way to read a post whose author chose a
// narrower audience.
func TestCommunity_ListPostsFilteredByFiction(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)

	novel := env.publishedNovel(t, author, nil)
	other := env.publishedNovel(t, author, nil)

	wanted := env.attachedPost(t, author, "โพสต์ถึงเรื่องนี้", novel.ID, "")
	env.attachedPost(t, author, "โพสต์ถึงอีกเรื่อง", other.ID, "")
	env.createCommunityPost(t, author, "โพสต์ที่ไม่แนบอะไรเลย", "public")

	listed, total := collectionOf[postBody](t, env.asOwner(t, author, http.MethodGet,
		"/api/v1/community/posts?novel="+novel.Slug))
	if total != 1 || len(listed) != 1 {
		t.Fatalf("filtered feed = %d posts (total %d), want 1", len(listed), total)
	}
	if listed[0].ID != wanted.ID {
		t.Fatalf("filtered feed returned the wrong post: %+v", listed[0])
	}

	// An unknown fiction is an empty page rather than an error - a filter must
	// not become a way to learn which slugs exist.
	_, missing := collectionOf[postBody](t, env.asOwner(t, author, http.MethodGet,
		"/api/v1/community/posts?novel=ไม่มีเรื่องนี้"))
	if missing != 0 {
		t.Fatalf("unknown fiction filter returned %d posts, want 0", missing)
	}
}
