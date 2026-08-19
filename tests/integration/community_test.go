package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Phase 7 - Community: posts, comment threads, and reactions
// (docs/08 §21, docs/09 §21, docs/11 §37).
//
// Notification assertions ride the same FIFO-sentinel technique as the
// Phase 6 suite: a later event arriving proves everything enqueued before it
// was already processed.

// referenceBody is the decoded shape of a post's attached fiction (§12D).
type referenceBody struct {
	NovelID       string  `json:"novel_id"`
	NovelSlug     string  `json:"novel_slug"`
	NovelTitle    string  `json:"novel_title"`
	ChapterID     *string `json:"chapter_id"`
	ChapterSlug   *string `json:"chapter_slug"`
	ChapterNumber *int    `json:"chapter_number"`
	ChapterTitle  *string `json:"chapter_title"`
	WordCount     *int    `json:"word_count"`
}

// postBody is the decoded shape of a community post resource.
type postBody struct {
	ID            string         `json:"id"`
	Content       string         `json:"content"`
	Visibility    string         `json:"visibility"`
	PostType      string         `json:"post_type"`
	Edited        bool           `json:"edited"`
	CommentCount  int64          `json:"comment_count"`
	ReactionCount int64          `json:"reaction_count"`
	MyReaction    string         `json:"my_reaction"`
	Bookmarked    bool           `json:"bookmarked"`
	Reference     *referenceBody `json:"reference"`
	IsOwner       bool           `json:"is_owner"`
	Author        struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
}

// communityCommentBody is the decoded shape of a community comment.
type communityCommentBody struct {
	ID         string  `json:"id"`
	PostID     string  `json:"post_id"`
	ParentID   *string `json:"parent_id"`
	Content    string  `json:"content"`
	Edited     bool    `json:"edited"`
	ReplyCount int64   `json:"reply_count"`
	IsOwner    bool    `json:"is_owner"`
}

type reactionBody struct {
	PostID        string `json:"post_id"`
	MyReaction    string `json:"my_reaction"`
	ReactionCount int64  `json:"reaction_count"`
}

// createCommunityPost creates a post and fails the test if it does not succeed.
func (e *authEnv) createCommunityPost(t *testing.T, w writer, content, visibility string) postBody {
	t.Helper()
	body := map[string]any{"content": content}
	if visibility != "" {
		body["visibility"] = visibility
	}
	res := e.asOwner(t, w, http.MethodPost, "/api/v1/community/posts", body)
	if res.status != http.StatusCreated {
		t.Fatalf("create post status = %d, want 201. body: %s", res.status, res.body)
	}
	return dataOf[postBody](t, res)
}

// ---------------------------------------------------------------------------
// Posts
// ---------------------------------------------------------------------------

func TestCommunity_PostLifecycle(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	// Community posting needs an account but NOT a verified email - the
	// access matrix (docs/03 §27) lets any signed-in user post.
	author := env.newUnverifiedWriter(t)

	post := env.createCommunityPost(t, author, "  อัปเดตงานเขียนสัปดาห์นี้ค่ะ  ", "")
	if post.Content != "อัปเดตงานเขียนสัปดาห์นี้ค่ะ" {
		t.Fatalf("content not trimmed/preserved: %q", post.Content)
	}
	if post.Visibility != "public" || !post.IsOwner {
		t.Fatalf("unexpected post shape: %+v", post)
	}

	// Guests read the public feed and the post itself.
	res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts")
	if res.status != http.StatusOK {
		t.Fatalf("guest feed status = %d", res.status)
	}
	feed, total := collectionOf[postBody](t, res)
	if total < 1 || len(feed) == 0 {
		t.Fatalf("guest feed empty after posting; total = %d", total)
	}
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	if res.status != http.StatusOK {
		t.Fatalf("guest get post status = %d", res.status)
	}
	if got := dataOf[postBody](t, res); got.IsOwner {
		t.Fatal("a guest must never see is_owner=true")
	}

	// Guests cannot write.
	res = env.asGuest(t, http.MethodPost, "/api/v1/community/posts")
	if res.status != http.StatusUnauthorized {
		t.Fatalf("guest create status = %d, want 401", res.status)
	}

	// Edit: content and visibility patch independently; edited flag appears.
	res = env.asOwner(t, author, http.MethodPatch, "/api/v1/community/posts/"+post.ID,
		map[string]any{"content": "แก้ไขอัปเดตแล้ว"})
	if res.status != http.StatusOK {
		t.Fatalf("edit status = %d. body: %s", res.status, res.body)
	}
	edited := dataOf[postBody](t, res)
	if !edited.Edited || edited.Content != "แก้ไขอัปเดตแล้ว" || edited.Visibility != "public" {
		t.Fatalf("edit result wrong: %+v", edited)
	}

	// IDOR: a stranger can neither edit nor delete a public post.
	stranger := env.newUnverifiedWriter(t)
	res = env.asOwner(t, stranger, http.MethodPatch, "/api/v1/community/posts/"+post.ID,
		map[string]any{"content": "ยึดโพสต์"})
	if res.status != http.StatusForbidden {
		t.Fatalf("stranger edit status = %d, want 403", res.status)
	}
	res = env.asOwner(t, stranger, http.MethodDelete, "/api/v1/community/posts/"+post.ID)
	if res.status != http.StatusForbidden {
		t.Fatalf("stranger delete status = %d, want 403", res.status)
	}

	// Delete: soft, idempotent, gone from the feed and the detail read.
	res = env.asOwner(t, author, http.MethodDelete, "/api/v1/community/posts/"+post.ID)
	if res.status != http.StatusNoContent {
		t.Fatalf("delete status = %d", res.status)
	}
	res = env.asOwner(t, author, http.MethodDelete, "/api/v1/community/posts/"+post.ID)
	if res.status != http.StatusNoContent {
		t.Fatalf("repeat delete status = %d, want 204 (idempotent)", res.status)
	}
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	if res.status != http.StatusNotFound {
		t.Fatalf("deleted post read status = %d, want 404", res.status)
	}
}

func TestCommunity_VisibilityMatrix(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	authorID := env.whoAmI(t, author)
	follower := env.newUnverifiedWriter(t)
	stranger := env.newUnverifiedWriter(t)
	env.asOwner(t, follower, http.MethodPost, "/api/v1/users/"+authorID+"/follow")

	public := env.createCommunityPost(t, author, "โพสต์สาธารณะ", "public")
	followersOnly := env.createCommunityPost(t, author, "เฉพาะผู้ติดตาม", "followers")
	private := env.createCommunityPost(t, author, "บันทึกส่วนตัว", "private")

	// docs/11 §37: the backend enforces visibility, non-oracle.
	cases := []struct {
		name   string
		postID string
		guest  int
		strngr int
		follow int
		owner  int
	}{
		{"public", public.ID, 200, 200, 200, 200},
		{"followers", followersOnly.ID, 404, 404, 200, 200},
		{"private", private.ID, 404, 404, 404, 200},
	}
	for _, tc := range cases {
		path := "/api/v1/community/posts/" + tc.postID
		if res := env.asGuest(t, http.MethodGet, path); res.status != tc.guest {
			t.Errorf("%s: guest = %d, want %d", tc.name, res.status, tc.guest)
		}
		if res := env.asOwner(t, stranger, http.MethodGet, path); res.status != tc.strngr {
			t.Errorf("%s: stranger = %d, want %d", tc.name, res.status, tc.strngr)
		}
		if res := env.asOwner(t, follower, http.MethodGet, path); res.status != tc.follow {
			t.Errorf("%s: follower = %d, want %d", tc.name, res.status, tc.follow)
		}
		if res := env.asOwner(t, author, http.MethodGet, path); res.status != tc.owner {
			t.Errorf("%s: owner = %d, want %d", tc.name, res.status, tc.owner)
		}
	}

	// The feeds apply the same predicate: author filter shows the guest one
	// post, the follower two, the owner all three.
	author001 := env.asGuest(t, http.MethodGet, "/api/v1/community/posts?author="+public.Author.Username)
	if _, total := collectionOf[postBody](t, author001); total != 1 {
		t.Errorf("guest author feed total = %d, want 1", total)
	}
	res := env.asOwner(t, follower, http.MethodGet, "/api/v1/community/posts?author="+public.Author.Username)
	if _, total := collectionOf[postBody](t, res); total != 2 {
		t.Errorf("follower author feed total = %d, want 2", total)
	}
	res = env.asOwner(t, author, http.MethodGet, "/api/v1/community/posts?author="+public.Author.Username)
	if _, total := collectionOf[postBody](t, res); total != 3 {
		t.Errorf("owner author feed total = %d, want 3", total)
	}

	// A stranger cannot even 403-probe someone's private post: same 404.
	res = env.asOwner(t, stranger, http.MethodPatch, "/api/v1/community/posts/"+private.ID,
		map[string]any{"content": "x"})
	if res.status != http.StatusNotFound {
		t.Errorf("stranger edit of private post = %d, want 404 (non-oracle)", res.status)
	}

	// ?feed=following narrows to followed authors and requires an account.
	res = env.asOwner(t, follower, http.MethodGet, "/api/v1/community/posts?feed=following")
	if _, total := collectionOf[postBody](t, res); total != 2 {
		t.Errorf("following feed total = %d, want 2", total)
	}
	if res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts?feed=following"); res.status != http.StatusUnauthorized {
		t.Errorf("guest following feed = %d, want 401", res.status)
	}
	if res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts?feed=trending"); res.status != http.StatusUnprocessableEntity {
		t.Errorf("unknown feed = %d, want 422", res.status)
	}

	// Unknown author: an empty page, not an oracle.
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts?author=nobody"+uuid.NewString()[:8])
	if _, total := collectionOf[postBody](t, res); total != 0 {
		t.Errorf("unknown author total = %d, want 0", total)
	}
}

func TestCommunity_PostValidation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newUnverifiedWriter(t)

	for name, body := range map[string]map[string]any{
		"empty content":      {"content": ""},
		"whitespace content": {"content": " \n\t "},
		"too long":           {"content": strings.Repeat("ก", 10001)},
		"bad visibility":     {"content": "โพสต์", "visibility": "friends"},
	} {
		res := env.asOwner(t, author, http.MethodPost, "/api/v1/community/posts", body)
		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("%s status = %d, want 422", name, res.status)
		}
	}

	// Exactly at the limit passes - runes, not bytes.
	post := env.createCommunityPost(t, author, strings.Repeat("ก", 10000), "")
	if post.ID == "" {
		t.Fatal("10000-rune Thai post rejected")
	}
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func TestCommunity_CommentThread(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newUnverifiedWriter(t)
	reader := env.newUnverifiedWriter(t)
	post := env.createCommunityPost(t, author, "ขอคำแนะนำปกนิยายหน่อยค่ะ", "")

	res := env.asOwner(t, reader, http.MethodPost,
		"/api/v1/community/posts/"+post.ID+"/comments",
		map[string]any{"content": "ลองโทนสีพาสเทลไหมคะ"})
	if res.status != http.StatusCreated {
		t.Fatalf("comment status = %d. body: %s", res.status, res.body)
	}
	comment := dataOf[communityCommentBody](t, res)

	// The author replies; single-level threading holds.
	res = env.asOwner(t, author, http.MethodPost,
		"/api/v1/community/comments/"+comment.ID+"/replies",
		map[string]any{"content": "ขอบคุณค่ะ เดี๋ยวลองดู"})
	if res.status != http.StatusCreated {
		t.Fatalf("reply status = %d. body: %s", res.status, res.body)
	}
	reply := dataOf[communityCommentBody](t, res)
	if reply.ParentID == nil || *reply.ParentID != comment.ID {
		t.Fatalf("reply not attached: %+v", reply)
	}
	res = env.asOwner(t, reader, http.MethodPost,
		"/api/v1/community/comments/"+reply.ID+"/replies",
		map[string]any{"content": "ซ้อน"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("nested reply status = %d, want 422", res.status)
	}

	// Guests read the thread; replies list separately, oldest first.
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID+"/comments")
	thread, total := collectionOf[communityCommentBody](t, res)
	if total != 1 || thread[0].ReplyCount != 1 {
		t.Fatalf("thread total = %d, replyCount = %d, want 1/1", total, thread[0].ReplyCount)
	}
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/comments/"+comment.ID+"/replies")
	replies, total := collectionOf[communityCommentBody](t, res)
	if total != 1 || replies[0].ID != reply.ID {
		t.Fatalf("replies wrong: total=%d %+v", total, replies)
	}

	// The post card counts visible comments.
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	if got := dataOf[postBody](t, res); got.CommentCount != 2 {
		t.Fatalf("comment_count = %d, want 2", got.CommentCount)
	}

	// Edit: owner only; IDOR is a 403 on public-post comments.
	res = env.asOwner(t, reader, http.MethodPatch, "/api/v1/community/comments/"+comment.ID,
		map[string]any{"content": "แก้ไขคำแนะนำ"})
	if res.status != http.StatusOK || !dataOf[communityCommentBody](t, res).Edited {
		t.Fatalf("edit failed: %d %s", res.status, res.body)
	}
	res = env.asOwner(t, author, http.MethodPatch, "/api/v1/community/comments/"+comment.ID,
		map[string]any{"content": "ยึด"})
	if res.status != http.StatusForbidden {
		t.Fatalf("non-owner comment edit = %d, want 403", res.status)
	}

	// Delete closes the thread: replies of a deleted comment are 404.
	res = env.asOwner(t, reader, http.MethodDelete, "/api/v1/community/comments/"+comment.ID)
	if res.status != http.StatusNoContent {
		t.Fatalf("delete comment status = %d", res.status)
	}
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/comments/"+comment.ID+"/replies")
	if res.status != http.StatusNotFound {
		t.Fatalf("replies of deleted comment = %d, want 404", res.status)
	}
}

func TestCommunity_ThreadFollowsPostVisibility(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newUnverifiedWriter(t)
	stranger := env.newUnverifiedWriter(t)
	private := env.createCommunityPost(t, author, "โพสต์ส่วนตัว", "private")

	// The discussion of an invisible post is equally invisible - list and
	// create alike, same 404.
	res := env.asOwner(t, stranger, http.MethodGet,
		"/api/v1/community/posts/"+private.ID+"/comments")
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger list on private post = %d, want 404", res.status)
	}
	res = env.asOwner(t, stranger, http.MethodPost,
		"/api/v1/community/posts/"+private.ID+"/comments",
		map[string]any{"content": "เห็นโพสต์ลับ"})
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger comment on private post = %d, want 404", res.status)
	}

	// The owner's own thread works.
	res = env.asOwner(t, author, http.MethodPost,
		"/api/v1/community/posts/"+private.ID+"/comments",
		map[string]any{"content": "โน้ตถึงตัวเอง"})
	if res.status != http.StatusCreated {
		t.Fatalf("owner comment on own private post = %d. body: %s", res.status, res.body)
	}
}

// ---------------------------------------------------------------------------
// Reactions
// ---------------------------------------------------------------------------

func TestCommunity_Reactions(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newUnverifiedWriter(t)
	reader := env.newUnverifiedWriter(t)
	second := env.newUnverifiedWriter(t)
	post := env.createCommunityPost(t, author, "จบเล่มแรกแล้ว!", "")

	react := func(w writer, body map[string]any) apiResponse {
		return env.asOwner(t, w, http.MethodPost,
			"/api/v1/community/posts/"+post.ID+"/reactions", body)
	}

	// Guests cannot react; unknown types are rejected by the allowlist.
	if res := env.asGuest(t, http.MethodPost, "/api/v1/community/posts/"+post.ID+"/reactions"); res.status != http.StatusUnauthorized {
		t.Fatalf("guest react = %d, want 401", res.status)
	}
	if res := react(reader, map[string]any{"type": "angry"}); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown reaction type = %d, want 422", res.status)
	}

	// React, then react again: one row per (post, user), never two.
	res := react(reader, map[string]any{"type": "like"})
	if res.status != http.StatusOK {
		t.Fatalf("react status = %d. body: %s", res.status, res.body)
	}
	state := dataOf[reactionBody](t, res)
	if state.MyReaction != "like" || state.ReactionCount != 1 {
		t.Fatalf("state after react: %+v", state)
	}
	res = react(reader, map[string]any{"type": "like"})
	if state = dataOf[reactionBody](t, res); state.ReactionCount != 1 {
		t.Fatalf("duplicate reaction accumulated: %+v", state)
	}

	// A second user's reaction counts separately; the first user's view of
	// the post carries their own my_reaction.
	react(second, map[string]any{"type": "like"})
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	got := dataOf[postBody](t, res)
	if got.ReactionCount != 2 || got.MyReaction != "like" {
		t.Fatalf("post reaction state: %+v", got)
	}
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	if got = dataOf[postBody](t, res); got.MyReaction != "" {
		t.Fatalf("guest sees a my_reaction: %+v", got)
	}

	// Removal is idempotent and safe to repeat.
	for i := 0; i < 2; i++ {
		res = env.asOwner(t, reader, http.MethodDelete,
			"/api/v1/community/posts/"+post.ID+"/reactions")
		if res.status != http.StatusOK {
			t.Fatalf("unreact #%d status = %d", i+1, res.status)
		}
	}
	if state = dataOf[reactionBody](t, res); state.MyReaction != "" || state.ReactionCount != 1 {
		t.Fatalf("state after unreact: %+v", state)
	}
}

// ---------------------------------------------------------------------------
// Notifications (docs/08 §23.1 community_reaction; docs/01 §20.2 no spam)
// ---------------------------------------------------------------------------

func TestCommunity_NotificationDelivery(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newUnverifiedWriter(t)
	reader := env.newUnverifiedWriter(t)
	post := env.createCommunityPost(t, author, "โพสต์รอปฏิกิริยา", "")

	// A reaction notifies the post's author, once.
	res := env.asOwner(t, reader, http.MethodPost,
		"/api/v1/community/posts/"+post.ID+"/reactions", map[string]any{"type": "like"})
	if res.status != http.StatusOK {
		t.Fatalf("react status = %d", res.status)
	}

	items := env.awaitNotifications(t, author, 1)
	if items[0].Type != "community_reaction" {
		t.Fatalf("author notification = %v, want community_reaction", typesOf(items))
	}
	if items[0].EntityType == nil || *items[0].EntityType != "community_post" ||
		items[0].EntityID == nil || *items[0].EntityID != post.ID {
		t.Fatalf("reaction notification entity wrong: %+v", items[0])
	}

	// Toggle churn never re-notifies: unreact, react, unreact, react …
	for i := 0; i < 2; i++ {
		env.asOwner(t, reader, http.MethodDelete, "/api/v1/community/posts/"+post.ID+"/reactions")
		env.asOwner(t, reader, http.MethodPost,
			"/api/v1/community/posts/"+post.ID+"/reactions", map[string]any{"type": "like"})
	}

	// A comment notifies the author too - and, arriving after the toggles,
	// proves (FIFO) that none of them delivered anything.
	res = env.asOwner(t, reader, http.MethodPost,
		"/api/v1/community/posts/"+post.ID+"/comments",
		map[string]any{"content": "ยินดีด้วยค่ะ"})
	if res.status != http.StatusCreated {
		t.Fatalf("comment status = %d", res.status)
	}
	comment := dataOf[communityCommentBody](t, res)

	items = env.awaitNotifications(t, author, 2)
	if len(items) != 2 {
		t.Fatalf("toggle churn re-notified: %v", typesOf(items))
	}
	if items[0].Type != "new_comment" ||
		items[0].EntityType == nil || *items[0].EntityType != "community_comment" {
		t.Fatalf("comment notification wrong: %+v", items[0])
	}

	// The author replies to the reader: the reader gets comment_reply; the
	// author (post owner AND replier) hears nothing about their own reply.
	env.asOwner(t, author, http.MethodPost,
		"/api/v1/community/comments/"+comment.ID+"/replies",
		map[string]any{"content": "ขอบคุณค่ะ"})

	readerItems := env.awaitNotifications(t, reader, 1)
	if readerItems[0].Type != "comment_reply" {
		t.Fatalf("reader notification = %v, want comment_reply", typesOf(readerItems))
	}
	res = env.asOwner(t, author, http.MethodGet, "/api/v1/me/notifications")
	authorItems, _ := collectionOf[notificationBody](t, res)
	if len(authorItems) != 2 {
		t.Fatalf("author self-notified: %v", typesOf(authorItems))
	}

	// The author reacting to their OWN post notifies nobody. Sentinel: a
	// fresh reaction from a third user arrives after it.
	env.asOwner(t, author, http.MethodPost,
		"/api/v1/community/posts/"+post.ID+"/reactions", map[string]any{"type": "like"})
	third := env.newUnverifiedWriter(t)
	env.asOwner(t, third, http.MethodPost,
		"/api/v1/community/posts/"+post.ID+"/reactions", map[string]any{"type": "like"})

	items = env.awaitNotifications(t, author, 3)
	for _, item := range items {
		if item.Actor != nil && item.Actor.ID == env.whoAmI(t, author) {
			t.Fatalf("author notified of their own action: %+v", item)
		}
	}
}
