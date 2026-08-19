package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Phase 6 - Interaction: comments and notifications (docs/08 §20, §23;
// docs/09 §20, §23).
//
// Notification tests exercise the REAL asynchronous pipeline: the API
// enqueues, the in-process worker delivers, and assertions poll the recipient
// API briefly. Negative assertions ("nobody was notified") ride the queue's
// FIFO order: a later sentinel event arriving proves everything enqueued
// before it was already processed.

// commentBody is the decoded shape of a comment resource.
type commentBody struct {
	ID         string  `json:"id"`
	NovelID    string  `json:"novel_id"`
	ChapterID  *string `json:"chapter_id"`
	ParentID   *string `json:"parent_id"`
	Content    string  `json:"content"`
	Edited     bool    `json:"edited"`
	ReplyCount int64   `json:"reply_count"`
	IsOwner    bool    `json:"is_owner"`
	Author     struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
}

// notificationBody is the decoded shape of a notification resource.
type notificationBody struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	EntityType *string `json:"entity_type"`
	EntityID   *string `json:"entity_id"`
	Read       bool    `json:"read"`
	Actor      *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"actor"`
}

// awaitNotifications polls the recipient's list until it holds at least want
// entries. The worker runs in-process, so this resolves in milliseconds; the
// deadline only bounds a genuine failure.
func (e *authEnv) awaitNotifications(t *testing.T, w writer, want int) []notificationBody {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var items []notificationBody
	for time.Now().Before(deadline) {
		res := e.asOwner(t, w, http.MethodGet, "/api/v1/me/notifications")
		if res.status != http.StatusOK {
			t.Fatalf("list notifications status = %d. body: %s", res.status, res.body)
		}
		items, _ = collectionOf[notificationBody](t, res)
		if len(items) >= want {
			return items
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d notifications; have %d", want, len(items))
	return nil
}

func typesOf(items []notificationBody) []string {
	types := make([]string, 0, len(items))
	for _, item := range items {
		types = append(types, item.Type)
	}
	return types
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func TestComments_NovelThreadLifecycle(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	// Commenting requires an account but NOT a verified email - verification
	// gates publishing fiction, never ordinary account use.
	reader := env.newUnverifiedWriter(t)

	res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.Slug+"/comments",
		map[string]any{"content": "  สนุกมากเลยค่ะ  "})
	if res.status != http.StatusCreated {
		t.Fatalf("create comment status = %d, want 201. body: %s", res.status, res.body)
	}
	comment := dataOf[commentBody](t, res)
	if comment.Content != "สนุกมากเลยค่ะ" {
		t.Fatalf("content not trimmed/preserved: %q", comment.Content)
	}
	if !comment.IsOwner || comment.ChapterID != nil || comment.ParentID != nil {
		t.Fatalf("unexpected comment shape: %+v", comment)
	}

	// Guests read the discussion without any authentication.
	res = env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug+"/comments")
	if res.status != http.StatusOK {
		t.Fatalf("guest list status = %d. body: %s", res.status, res.body)
	}
	listed, total := collectionOf[commentBody](t, res)
	if total != 1 || len(listed) != 1 {
		t.Fatalf("guest list total = %d, len = %d, want 1", total, len(listed))
	}
	if listed[0].IsOwner {
		t.Fatal("a guest must never see is_owner=true")
	}

	// Guests cannot join it: this fiction is on the default "เฉพาะสมาชิก"
	// level, and the 401 is what the UI turns into a sign-in offer. A body is
	// sent because the check now happens in the SERVICE - it has the fiction in
	// hand and can see which of the three levels applies (§13D).
	res = env.asGuest(t, http.MethodPost, "/api/v1/novels/"+novel.Slug+"/comments",
		map[string]any{"content": "ขอคอมเมนต์แบบไม่ล็อกอิน"})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("guest create status = %d, want 401", res.status)
	}

	// Edit: author of the comment only.
	res = env.asOwner(t, reader, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		map[string]any{"content": "แก้ไขแล้วนะคะ"})
	if res.status != http.StatusOK {
		t.Fatalf("edit status = %d. body: %s", res.status, res.body)
	}
	edited := dataOf[commentBody](t, res)
	if !edited.Edited {
		t.Fatal("edited flag must be set after a content change")
	}

	stranger := env.newUnverifiedWriter(t)
	res = env.asOwner(t, stranger, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		map[string]any{"content": "ยึดคอมเมนต์"})
	if res.status != http.StatusForbidden {
		t.Fatalf("stranger edit status = %d, want 403", res.status)
	}
	res = env.asOwner(t, stranger, http.MethodDelete, "/api/v1/comments/"+comment.ID)
	if res.status != http.StatusForbidden {
		t.Fatalf("stranger delete status = %d, want 403", res.status)
	}

	// Delete: soft, idempotent, and gone from every listing.
	res = env.asOwner(t, reader, http.MethodDelete, "/api/v1/comments/"+comment.ID)
	if res.status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", res.status)
	}
	res = env.asOwner(t, reader, http.MethodDelete, "/api/v1/comments/"+comment.ID)
	if res.status != http.StatusNoContent {
		t.Fatalf("repeat delete status = %d, want 204 (idempotent)", res.status)
	}
	res = env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug+"/comments")
	if _, total := collectionOf[commentBody](t, res); total != 0 {
		t.Fatalf("deleted comment still listed; total = %d", total)
	}
}

func TestComments_ChapterAndNovelThreadsAreSeparate(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title": "บทที่หนึ่ง", "content": strings.Repeat("เนื้อเรื่อง ", 20), "status": "published",
	})

	reader := env.newUnverifiedWriter(t)
	res := env.asOwner(t, reader, http.MethodPost,
		"/api/v1/novels/"+novel.Slug+"/chapters/"+chapter.Slug+"/comments",
		map[string]any{"content": "ตอนนี้พีคมาก"})
	if res.status != http.StatusCreated {
		t.Fatalf("chapter comment status = %d. body: %s", res.status, res.body)
	}
	chapterComment := dataOf[commentBody](t, res)
	if chapterComment.ChapterID == nil || *chapterComment.ChapterID != chapter.ID {
		t.Fatalf("chapter comment not anchored to the chapter: %+v", chapterComment)
	}

	res = env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.Slug+"/comments",
		map[string]any{"content": "รอติดตามเรื่องนี้"})
	if res.status != http.StatusCreated {
		t.Fatalf("novel comment status = %d. body: %s", res.status, res.body)
	}

	// The fiction page thread holds only the fiction-level comment…
	res = env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug+"/comments")
	novelThread, total := collectionOf[commentBody](t, res)
	if total != 1 || novelThread[0].Content != "รอติดตามเรื่องนี้" {
		t.Fatalf("novel thread wrong: total=%d %+v", total, novelThread)
	}

	// …and the chapter thread only the chapter's.
	res = env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.Slug+"/chapters/"+chapter.Slug+"/comments")
	chapterThread, total := collectionOf[commentBody](t, res)
	if total != 1 || chapterThread[0].Content != "ตอนนี้พีคมาก" {
		t.Fatalf("chapter thread wrong: total=%d %+v", total, chapterThread)
	}
}

func TestComments_RepliesNestThreeLevels(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	reader := env.newUnverifiedWriter(t)

	res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.Slug+"/comments",
		map[string]any{"content": "คำถามถึงนักเขียนค่ะ"})
	parent := dataOf[commentBody](t, res)

	// The fiction's author replies (level two).
	res = env.asOwner(t, author, http.MethodPost, "/api/v1/comments/"+parent.ID+"/replies",
		map[string]any{"content": "ขอบคุณที่ติดตามนะครับ"})
	if res.status != http.StatusCreated {
		t.Fatalf("reply status = %d. body: %s", res.status, res.body)
	}
	reply := dataOf[commentBody](t, res)
	if reply.ParentID == nil || *reply.ParentID != parent.ID {
		t.Fatalf("reply not attached to parent: %+v", reply)
	}

	// A reply to the reply nests (level three) - the comment design review
	// 2026-08 depth.
	res = env.asOwner(t, reader, http.MethodPost, "/api/v1/comments/"+reply.ID+"/replies",
		map[string]any{"content": "ตอบกลับซ้อนค่ะ"})
	if res.status != http.StatusCreated {
		t.Fatalf("nested reply status = %d, want 201. body: %s", res.status, res.body)
	}
	nested := dataOf[commentBody](t, res)
	if nested.ParentID == nil || *nested.ParentID != reply.ID {
		t.Fatalf("nested reply not attached to the reply: %+v", nested)
	}

	// The third level is the floor: replying THERE joins the same level,
	// beside what it answers, never deeper.
	res = env.asOwner(t, author, http.MethodPost, "/api/v1/comments/"+nested.ID+"/replies",
		map[string]any{"content": "ตอบต่อจากชั้นสุดท้าย"})
	if res.status != http.StatusCreated {
		t.Fatalf("floor reply status = %d, want 201. body: %s", res.status, res.body)
	}
	floor := dataOf[commentBody](t, res)
	if floor.ParentID == nil || *floor.ParentID != reply.ID {
		t.Fatalf("floor reply should attach beside, under %s: %+v", reply.ID, floor)
	}

	// The top-level listing counts only its DIRECT replies.
	res = env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug+"/comments")
	topLevel, total := collectionOf[commentBody](t, res)
	if total != 1 || topLevel[0].ReplyCount != 1 {
		t.Fatalf("top-level thread: total=%d replyCount=%d, want 1/1", total, topLevel[0].ReplyCount)
	}

	// The reply's own thread holds the nested reply and the floor one.
	res = env.asGuest(t, http.MethodGet, "/api/v1/comments/"+reply.ID+"/replies")
	replies, total := collectionOf[commentBody](t, res)
	if total != 2 || len(replies) != 2 {
		t.Fatalf("nested thread wrong: total=%d %+v", total, replies)
	}

	// Deleting the parent closes its thread: replies of an invisible comment
	// are the same 404 as a missing one.
	res = env.asOwner(t, reader, http.MethodDelete, "/api/v1/comments/"+parent.ID)
	if res.status != http.StatusNoContent {
		t.Fatalf("delete parent status = %d", res.status)
	}
	res = env.asGuest(t, http.MethodGet, "/api/v1/comments/"+parent.ID+"/replies")
	if res.status != http.StatusNotFound {
		t.Fatalf("replies of deleted parent status = %d, want 404", res.status)
	}
}

func TestComments_ClosedOnUnreadableContent(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	stranger := env.newUnverifiedWriter(t)

	// A draft fiction does not exist for anyone else - listing or commenting.
	draft := env.createNovel(t, author, createNovelBody(uniqueName(t, "Draft "), nil))
	res := env.asOwner(t, stranger, http.MethodGet, "/api/v1/novels/"+draft.Slug+"/comments")
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger list on draft = %d, want 404", res.status)
	}
	res = env.asOwner(t, stranger, http.MethodPost, "/api/v1/novels/"+draft.Slug+"/comments",
		map[string]any{"content": "เห็นดราฟต์"})
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger comment on draft = %d, want 404", res.status)
	}

	// An unpublished chapter of a PUBLIC fiction is equally closed.
	novel := env.publishedNovel(t, author, nil)
	draftChapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title": "ตอนร่าง", "content": "ยังไม่เผยแพร่",
	})
	res = env.asOwner(t, stranger, http.MethodPost,
		"/api/v1/novels/"+novel.Slug+"/chapters/"+draftChapter.Slug+"/comments",
		map[string]any{"content": "เห็นตอนร่าง"})
	if res.status != http.StatusNotFound {
		t.Fatalf("comment on draft chapter = %d, want 404", res.status)
	}
}

func TestComments_Validation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	reader := env.newUnverifiedWriter(t)
	path := "/api/v1/novels/" + novel.Slug + "/comments"

	for name, content := range map[string]string{
		"empty":      "",
		"whitespace": "   \n\t",
		"too long":   strings.Repeat("ก", 5001),
	} {
		res := env.asOwner(t, reader, http.MethodPost, path, map[string]any{"content": content})
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("%s content status = %d, want 422", name, res.status)
		}
		if code := errorCodeOf(t, res); code != "VALIDATION_ERROR" {
			t.Fatalf("%s content code = %s", name, code)
		}
	}

	// Exactly at the limit passes - the bound counts runes, not bytes.
	res := env.asOwner(t, reader, http.MethodPost, path,
		map[string]any{"content": strings.Repeat("ก", 5000)})
	if res.status != http.StatusCreated {
		t.Fatalf("5000-rune Thai comment status = %d, want 201. body: %s", res.status, res.body)
	}
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

func TestNotifications_RequireAuthentication(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	for _, path := range []string{"/api/v1/me/notifications", "/api/v1/me/notifications/unread-count"} {
		if res := env.asGuest(t, http.MethodGet, path); res.status != http.StatusUnauthorized {
			t.Fatalf("guest GET %s = %d, want 401", path, res.status)
		}
	}
}

func TestNotifications_FollowDelivery(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	authorSession := env.newWriter(t)
	authorID := env.whoAmI(t, authorSession)
	follower := env.newUnverifiedWriter(t)

	res := env.asOwner(t, follower, http.MethodPost, "/api/v1/users/"+authorID+"/follow")
	if res.status != http.StatusNoContent {
		t.Fatalf("follow status = %d. body: %s", res.status, res.body)
	}

	items := env.awaitNotifications(t, authorSession, 1)
	got := items[0]
	if got.Type != "new_follower" || got.Actor == nil || got.Read {
		t.Fatalf("unexpected notification: %+v", got)
	}

	// The idempotent repeat inserts nothing, so it notifies nothing. Prove it
	// with a FIFO sentinel: a fresh follower's event lands AFTER any repeat
	// event would have, so 2 total means the repeat delivered nothing.
	res = env.asOwner(t, follower, http.MethodPost, "/api/v1/users/"+authorID+"/follow")
	if res.status != http.StatusNoContent {
		t.Fatalf("repeat follow status = %d", res.status)
	}
	second := env.newUnverifiedWriter(t)
	env.asOwner(t, second, http.MethodPost, "/api/v1/users/"+authorID+"/follow")

	items = env.awaitNotifications(t, authorSession, 2)
	if len(items) != 2 {
		t.Fatalf("repeat follow re-notified: %v", typesOf(items))
	}

	// Unread badge, single read, read-all - and recipient isolation.
	res = env.asOwner(t, authorSession, http.MethodGet, "/api/v1/me/notifications/unread-count")
	var badge struct {
		Data struct {
			UnreadCount int64 `json:"unread_count"`
		} `json:"data"`
	}
	res.json(t, &badge)
	if badge.Data.UnreadCount != 2 {
		t.Fatalf("unread_count = %d, want 2", badge.Data.UnreadCount)
	}

	res = env.asOwner(t, follower, http.MethodPost, "/api/v1/notifications/"+items[0].ID+"/read")
	if res.status != http.StatusNotFound {
		t.Fatalf("marking someone else's notification = %d, want 404", res.status)
	}

	res = env.asOwner(t, authorSession, http.MethodPost, "/api/v1/notifications/"+items[0].ID+"/read")
	if res.status != http.StatusNoContent {
		t.Fatalf("mark read status = %d", res.status)
	}
	res = env.asOwner(t, authorSession, http.MethodPost, "/api/v1/notifications/"+items[0].ID+"/read")
	if res.status != http.StatusNoContent {
		t.Fatalf("repeat mark read status = %d, want 204 (idempotent)", res.status)
	}

	res = env.asOwner(t, authorSession, http.MethodPost, "/api/v1/me/notifications/read-all")
	if res.status != http.StatusNoContent {
		t.Fatalf("read-all status = %d", res.status)
	}
	res = env.asOwner(t, authorSession, http.MethodGet, "/api/v1/me/notifications/unread-count")
	res.json(t, &badge)
	if badge.Data.UnreadCount != 0 {
		t.Fatalf("unread_count after read-all = %d, want 0", badge.Data.UnreadCount)
	}
}

func TestNotifications_CommentAndReplyDelivery(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	reader := env.newUnverifiedWriter(t)

	// Reader comments → the fiction's author hears about it.
	res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.Slug+"/comments",
		map[string]any{"content": "ชอบบทนี้มาก"})
	comment := dataOf[commentBody](t, res)

	items := env.awaitNotifications(t, author, 1)
	if items[0].Type != "new_comment" {
		t.Fatalf("author notification = %v, want new_comment", typesOf(items))
	}
	if items[0].EntityID == nil || *items[0].EntityID != comment.ID {
		t.Fatalf("notification does not reference the comment: %+v", items[0])
	}

	// The author replies → the reader gets comment_reply; the author, who is
	// both replier and fiction owner, must NOT be notified about their own
	// reply.
	env.asOwner(t, author, http.MethodPost, "/api/v1/comments/"+comment.ID+"/replies",
		map[string]any{"content": "ขอบคุณครับ"})

	readerItems := env.awaitNotifications(t, reader, 1)
	if readerItems[0].Type != "comment_reply" {
		t.Fatalf("reader notification = %v, want comment_reply", typesOf(readerItems))
	}

	// Sentinel proves the author's queue settled at exactly one entry.
	other := env.newUnverifiedWriter(t)
	env.asOwner(t, other, http.MethodPost, "/api/v1/novels/"+novel.Slug+"/comments",
		map[string]any{"content": "ตามมาอ่านค่ะ"})
	items = env.awaitNotifications(t, author, 2)
	for _, item := range items {
		if item.Type == "comment_reply" {
			t.Fatalf("author was notified of their own reply: %v", typesOf(items))
		}
	}
}

func TestNotifications_ChapterPublishFanOut(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	authorID := env.whoAmI(t, author)
	follower := env.newUnverifiedWriter(t)
	nonFollower := env.newUnverifiedWriter(t)

	env.asOwner(t, follower, http.MethodPost, "/api/v1/users/"+authorID+"/follow")

	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title": "ตอนแรก", "content": "จุดเริ่มต้นของเรื่อง",
	})
	env.publishChapter(t, author, novel.ID, chapter.ID)

	// new_follower + novel_update, newest first.
	items := env.awaitNotifications(t, follower, 1)
	if items[0].Type != "novel_update" {
		t.Fatalf("follower notifications = %v, want novel_update first", typesOf(items))
	}
	if items[0].EntityID == nil || *items[0].EntityID != chapter.ID {
		t.Fatalf("notification does not reference the chapter: %+v", items[0])
	}

	// Unpublish and republish must NOT re-notify: published_at survives the
	// retraction, so the transition is not a first publish.
	res := env.asOwner(t, author, http.MethodPost,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID+"/unpublish", nil)
	if res.status != http.StatusOK {
		t.Fatalf("unpublish status = %d. body: %s", res.status, res.body)
	}
	env.publishChapter(t, author, novel.ID, chapter.ID)

	// A genuinely new chapter DOES notify - and, as the FIFO sentinel, proves
	// the republish above delivered nothing.
	second := env.createChapter(t, author, novel.ID, map[string]any{
		"title": "ตอนสอง", "content": "เรื่องดำเนินต่อ", "status": "published",
	})
	items = env.awaitNotifications(t, follower, 2)
	if len(items) != 2 {
		t.Fatalf("republish re-notified: %v", typesOf(items))
	}
	if items[0].EntityID == nil || *items[0].EntityID != second.ID {
		t.Fatalf("newest notification should reference chapter two: %+v", items[0])
	}

	// The author never hears about their own publish - their only entry is
	// the follow itself; a non-follower hears nothing at all.
	res = env.asOwner(t, author, http.MethodGet, "/api/v1/me/notifications")
	authorItems, _ := collectionOf[notificationBody](t, res)
	for _, item := range authorItems {
		if item.Type == "novel_update" {
			t.Fatalf("author self-notified about their own publish: %v", typesOf(authorItems))
		}
	}
	res = env.asOwner(t, nonFollower, http.MethodGet, "/api/v1/me/notifications")
	if _, total := collectionOf[notificationBody](t, res); total != 0 {
		t.Fatalf("non-follower notified; total = %d", total)
	}
}

func TestNotifications_PrivateFictionPublishesQuietly(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	authorID := env.whoAmI(t, author)
	follower := env.newUnverifiedWriter(t)
	env.asOwner(t, follower, http.MethodPost, "/api/v1/users/"+authorID+"/follow")
	env.awaitNotifications(t, author, 1) // the follow itself

	private := env.createNovel(t, author, createNovelBody(uniqueName(t, "Private "),
		map[string]any{"status": "ongoing", "visibility": "private"}))
	chapter := env.createChapter(t, author, private.ID, map[string]any{
		"title": "ลับเฉพาะ", "content": "เนื้อหาส่วนตัว",
	})
	env.publishChapter(t, author, private.ID, chapter.ID)

	// FIFO sentinel: a follow event queued AFTER the publish arrives, so the
	// publish was processed - and delivered nothing.
	second := env.newUnverifiedWriter(t)
	env.asOwner(t, second, http.MethodPost, "/api/v1/users/"+authorID+"/follow")
	env.awaitNotifications(t, author, 2)

	res := env.asOwner(t, follower, http.MethodGet, "/api/v1/me/notifications")
	if items, total := collectionOf[notificationBody](t, res); total != 0 {
		t.Fatalf("private publish notified a follower: %v", typesOf(items))
	}
}

// whoAmI reads the caller's own user id through the real endpoint.
func (e *authEnv) whoAmI(t *testing.T, w writer) string {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/auth/me")
	if res.status != http.StatusOK {
		t.Fatalf("auth/me status = %d. body: %s", res.status, res.body)
	}
	me := dataOf[struct {
		ID string `json:"id"`
	}](t, res)
	if me.ID == "" {
		t.Fatalf("auth/me returned no user id: %s", res.body)
	}
	return me.ID
}
