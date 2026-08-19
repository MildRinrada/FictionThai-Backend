package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Phase 8 - Moderation: reports and the audit trail (docs/08 §24, docs/09
// §28–§29, docs/11 §38–§39).
//
// The properties that matter most are adversarial: a report must never become
// an existence oracle for private work (docs/11 §21), a normal user must
// never reach a staff surface (docs/09 §29), the reporter's view must never
// leak internal moderation detail (docs/02 §38), and every action must leave
// an append-only audit row (docs/08 §24.2).

// reportBody is the reporter-facing decoded shape.
type reportBody struct {
	ID          string  `json:"id"`
	TargetType  string  `json:"target_type"`
	TargetID    string  `json:"target_id"`
	Reason      string  `json:"reason"`
	Description *string `json:"description"`
	Status      string  `json:"status"`
	ResolvedAt  *string `json:"resolved_at"`
}

// moderatorReportBody adds the staff-only fields.
type moderatorReportBody struct {
	reportBody
	Reporter *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"reporter"`
	Resolver *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"resolver"`
}

// actionBody is one audit entry.
type actionBody struct {
	ID         string  `json:"id"`
	TargetType string  `json:"target_type"`
	TargetID   string  `json:"target_id"`
	Action     string  `json:"action"`
	Reason     *string `json:"reason"`
	Moderator  struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"moderator"`
}

// reportDetailBody is the staff report page.
type reportDetailBody struct {
	Report moderatorReportBody `json:"report"`
	Target *struct {
		Type    string  `json:"type"`
		ID      string  `json:"id"`
		Exists  bool    `json:"exists"`
		State   string  `json:"state"`
		Title   *string `json:"title"`
		Excerpt *string `json:"excerpt"`
		Author  *struct {
			Username string `json:"username"`
		} `json:"author"`
	} `json:"target"`
	History          []actionBody `json:"history"`
	AvailableActions []string     `json:"available_actions"`
}

// promote flips an account's role directly in the database. Role changes are
// deliberately NOT an API (docs/08 §6.1 "Do not create dozens of roles
// prematurely"; role assignment is operational), so the test does what an
// operator would.
func (e *authEnv) promote(t *testing.T, userID, role string) {
	t.Helper()
	result, err := e.db.DB.Exec(`UPDATE users SET role = $2 WHERE id = $1`, userID, role)
	if err != nil {
		t.Fatalf("promote user: %v", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		t.Fatalf("promote user affected %d rows, want 1", affected)
	}
}

// newModerator registers a fresh account and promotes it. The session loads
// the user on every request, so the new role is live immediately.
func (e *authEnv) newModerator(t *testing.T) writer {
	t.Helper()
	session := e.registerWeb(t)
	e.promote(t, session.userID, "moderator")
	return writer{webSession: session}
}

// newAdmin is newModerator with the admin role.
func (e *authEnv) newAdmin(t *testing.T) writer {
	t.Helper()
	session := e.registerWeb(t)
	e.promote(t, session.userID, "admin")
	return writer{webSession: session}
}

// fileReport files a report and fails the test unless it answers 201.
func (e *authEnv) fileReport(
	t *testing.T, w writer, targetType, targetID, reason string,
) reportBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPost, "/api/v1/reports", map[string]string{
		"target_type": targetType,
		"target_id":   targetID,
		"reason":      reason,
	})
	if res.status != http.StatusCreated {
		t.Fatalf("create report status = %d, want 201. body: %s", res.status, res.body)
	}
	return dataOf[reportBody](t, res)
}

// performAction executes a moderation action and fails the test unless it
// answers 201.
func (e *authEnv) performAction(
	t *testing.T, staff writer, targetType, targetID, action string,
) actionBody {
	t.Helper()
	res := e.asOwner(t, staff, http.MethodPost, "/api/v1/admin/moderation/actions", map[string]string{
		"target_type": targetType,
		"target_id":   targetID,
		"action":      action,
		"reason":      "integration test",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("%s %s status = %d, want 201. body: %s", action, targetType, res.status, res.body)
	}
	return dataOf[actionBody](t, res)
}

// ---------------------------------------------------------------------------
// Report lifecycle
// ---------------------------------------------------------------------------

func TestReports_LifecycleAndDuplicateGuard(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	reporter := writer{webSession: env.registerWeb(t)}
	moderator := env.newModerator(t)

	report := env.fileReport(t, reporter, "novel", novel.ID, "spam")
	if report.Status != "pending" {
		t.Fatalf("fresh report status = %q, want pending", report.Status)
	}

	// Filing again while open is idempotent: 200 and the SAME report.
	res := env.asOwner(t, reporter, http.MethodPost, "/api/v1/reports", map[string]string{
		"target_type": "novel", "target_id": novel.ID, "reason": "abuse",
	})
	if res.status != http.StatusOK {
		t.Fatalf("duplicate report status = %d, want 200. body: %s", res.status, res.body)
	}
	if dup := dataOf[reportBody](t, res); dup.ID != report.ID || dup.Reason != "spam" {
		t.Fatalf("duplicate returned %+v, want the original open report unchanged", dup)
	}

	// The moderator's queue holds it; the reporter card is present there.
	//
	// Paged, not assumed to be on the first page. The pending queue is shared
	// state that every other test in this package adds to, so a single-page
	// lookup fails for a reason that has nothing to do with reports - it fails
	// because the suite grew.
	var queued *moderatorReportBody
	for page := 1; queued == nil && page <= 20; page++ {
		res = env.asOwner(t, moderator, http.MethodGet, fmt.Sprintf(
			"/api/v1/admin/reports?status=pending&per_page=100&page=%d", page))
		queue, total := collectionOf[moderatorReportBody](t, res)
		for i := range queue {
			if queue[i].ID == report.ID {
				queued = &queue[i]
			}
		}
		if len(queue) == 0 || int64(page*100) >= total {
			break
		}
	}
	if queued == nil {
		t.Fatalf("report %s is not anywhere in the pending queue", report.ID)
	}
	if queued.Reporter == nil || queued.Reporter.Username != reporter.username {
		t.Fatalf("queue entry missing reporter card: %+v", queued)
	}

	// pending → reviewing → resolved, each step through the real endpoint.
	res = env.asOwner(t, moderator, http.MethodPatch, "/api/v1/admin/reports/"+report.ID,
		map[string]string{"status": "reviewing"})
	if res.status != http.StatusOK {
		t.Fatalf("to reviewing status = %d. body: %s", res.status, res.body)
	}

	// Still open - the duplicate guard still answers with the same report.
	res = env.asOwner(t, reporter, http.MethodPost, "/api/v1/reports", map[string]string{
		"target_type": "novel", "target_id": novel.ID, "reason": "spam",
	})
	if res.status != http.StatusOK {
		t.Fatalf("duplicate during review status = %d, want 200", res.status)
	}

	res = env.asOwner(t, moderator, http.MethodPatch, "/api/v1/admin/reports/"+report.ID,
		map[string]string{"status": "resolved"})
	if res.status != http.StatusOK {
		t.Fatalf("to resolved status = %d. body: %s", res.status, res.body)
	}
	resolved := dataOf[moderatorReportBody](t, res)
	if resolved.ResolvedAt == nil || resolved.Resolver == nil ||
		resolved.Resolver.Username != moderator.username {
		t.Fatalf("resolution missing its stamp: %+v", resolved)
	}

	// Terminal means terminal.
	for _, illegal := range []string{"reviewing", "pending", "rejected"} {
		res = env.asOwner(t, moderator, http.MethodPatch, "/api/v1/admin/reports/"+report.ID,
			map[string]string{"status": illegal})
		if res.status != http.StatusConflict {
			t.Errorf("resolved → %s status = %d, want 409", illegal, res.status)
		}
	}

	// Closed report → the target can be reported afresh.
	fresh := env.fileReport(t, reporter, "novel", novel.ID, "abuse")
	if fresh.ID == report.ID {
		t.Fatal("re-report after resolution returned the closed report")
	}

	// The reporter's own history shows both, newest first, and NEVER exposes
	// who resolved anything (docs/02 §38).
	res = env.asOwner(t, reporter, http.MethodGet, "/api/v1/me/reports")
	mine, total := collectionOf[reportBody](t, res)
	if total != 2 || len(mine) != 2 {
		t.Fatalf("my reports = %d entries (total %d), want 2", len(mine), total)
	}
	if mine[0].ID != fresh.ID {
		t.Fatalf("my reports not newest first: %+v", mine)
	}
	if strings.Contains(string(res.body), "resolver") ||
		strings.Contains(string(res.body), moderator.username) {
		t.Fatalf("reporter view leaks moderation internals: %s", res.body)
	}
}

func TestReports_Validation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	reporter := writer{webSession: env.registerWeb(t)}

	cases := []struct {
		name string
		body map[string]string
	}{
		{"unknown target type", map[string]string{
			"target_type": "fanfic", "target_id": novel.ID, "reason": "spam"}},
		{"malformed target id", map[string]string{
			"target_type": "novel", "target_id": "not-a-uuid", "reason": "spam"}},
		{"unknown reason", map[string]string{
			"target_type": "novel", "target_id": novel.ID, "reason": "boring"}},
		{"overlong description", map[string]string{
			"target_type": "novel", "target_id": novel.ID, "reason": "spam",
			"description": strings.Repeat("ยาว", 700)}},
	}
	for _, tc := range cases {
		res := env.asOwner(t, reporter, http.MethodPost, "/api/v1/reports", tc.body)
		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422. body: %s", tc.name, res.status, res.body)
		}
	}

	// A guest cannot report at all (docs/01 §21: the reporter is part of the
	// report) - and the public reading path is untouched by that.
	res := env.do(t, apiRequest{method: http.MethodPost, path: "/api/v1/reports",
		body: map[string]string{"target_type": "novel", "target_id": novel.ID, "reason": "spam"}})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("guest report status = %d, want 401", res.status)
	}
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID); res.status != http.StatusOK {
		t.Fatalf("guest read status = %d, want 200 - reporting must not break guest reading", res.status)
	}

	// A cookie session without its CSRF token is rejected (docs/11 §22).
	res = env.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/reports",
		body:    map[string]string{"target_type": "novel", "target_id": novel.ID, "reason": "spam"},
		cookies: reporter.authCookies(),
	})
	if res.status != http.StatusForbidden {
		t.Fatalf("report without CSRF status = %d, want 403", res.status)
	}
}

// A report must never confirm the existence of something its reporter cannot
// see (docs/11 §21, §31).
func TestReports_NoVisibilityOracle(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	stranger := writer{webSession: env.registerWeb(t)}

	// A private draft: reporting it must 404 exactly like reading it.
	private := env.createNovel(t, author, createNovelBody(uniqueName(t, "Secret "), nil))
	res := env.asOwner(t, stranger, http.MethodPost, "/api/v1/reports", map[string]string{
		"target_type": "novel", "target_id": private.ID, "reason": "spam",
	})
	if res.status != http.StatusNotFound {
		t.Fatalf("report private novel status = %d, want 404", res.status)
	}

	// The owner CAN see it, so the owner can report it - the check is
	// visibility, not a blanket wall.
	if r := env.fileReport(t, author, "novel", private.ID, "spam"); r.Status != "pending" {
		t.Fatalf("owner report on own private novel = %+v", r)
	}

	// A private community post is invisible to a stranger's report.
	poster := writer{webSession: env.registerWeb(t)}
	res = env.asOwner(t, poster, http.MethodPost, "/api/v1/community/posts",
		map[string]string{"content": "โพสต์ส่วนตัว", "visibility": "private"})
	if res.status != http.StatusCreated {
		t.Fatalf("create private post status = %d. body: %s", res.status, res.body)
	}
	post := dataOf[struct {
		ID string `json:"id"`
	}](t, res)
	res = env.asOwner(t, stranger, http.MethodPost, "/api/v1/reports", map[string]string{
		"target_type": "community_post", "target_id": post.ID, "reason": "spam",
	})
	if res.status != http.StatusNotFound {
		t.Fatalf("report private post status = %d, want 404", res.status)
	}

	// Nonexistent targets of every type answer the same 404.
	const ghost = "3f1f8de1-0000-4000-8000-000000000000"
	for _, targetType := range []string{
		"novel", "chapter", "comment", "community_post", "community_comment", "user",
	} {
		res := env.asOwner(t, stranger, http.MethodPost, "/api/v1/reports", map[string]string{
			"target_type": targetType, "target_id": ghost, "reason": "spam",
		})
		if res.status != http.StatusNotFound {
			t.Errorf("report ghost %s status = %d, want 404", targetType, res.status)
		}
	}

	// An unpublished chapter of a public fiction is not reportable by a
	// reader (docs/11 §21: publishing the fiction does not publish drafts).
	public := env.publishedNovel(t, author, nil)
	draft := env.createChapter(t, author, public.ID, map[string]any{
		"title": "Draft", "content": "ยังไม่เผยแพร่",
	})
	res = env.asOwner(t, stranger, http.MethodPost, "/api/v1/reports", map[string]string{
		"target_type": "chapter", "target_id": draft.ID, "reason": "spam",
	})
	if res.status != http.StatusNotFound {
		t.Fatalf("report draft chapter status = %d, want 404", res.status)
	}

	// Reporting a user works for any live account.
	if r := env.fileReport(t, stranger, "user", author.userID, "harassment"); r.Status != "pending" {
		t.Fatalf("report user = %+v", r)
	}
}

// ---------------------------------------------------------------------------
// Authorization - the IDOR matrix (Phase 8 brief §15)
// ---------------------------------------------------------------------------

func TestModeration_StaffSurfaceIsClosed(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	normal := writer{webSession: env.registerWeb(t)}
	other := writer{webSession: env.registerWeb(t)}
	report := env.fileReport(t, normal, "novel", novel.ID, "spam")

	// Every staff route answers 403 to a signed-in normal user…
	staffCalls := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/admin/reports", nil},
		{http.MethodGet, "/api/v1/admin/reports/" + report.ID, nil},
		{http.MethodPatch, "/api/v1/admin/reports/" + report.ID, map[string]string{"status": "resolved"}},
		{http.MethodGet, "/api/v1/admin/moderation/actions", nil},
		{http.MethodPost, "/api/v1/admin/moderation/actions", map[string]string{
			"target_type": "novel", "target_id": novel.ID, "action": "remove"}},
	}
	for _, call := range staffCalls {
		res := env.asOwner(t, normal, call.method, call.path, call.body)
		if res.status != http.StatusForbidden {
			t.Errorf("normal user %s %s = %d, want 403", call.method, call.path, res.status)
		}
		// …and 401 to a guest.
		res = env.do(t, apiRequest{method: call.method, path: call.path, body: call.body})
		if res.status != http.StatusUnauthorized {
			t.Errorf("guest %s %s = %d, want 401", call.method, call.path, res.status)
		}
	}

	// /me/reports is scoped: another user's listing never contains my report.
	res := env.asOwner(t, other, http.MethodGet, "/api/v1/me/reports")
	theirs, total := collectionOf[reportBody](t, res)
	if total != 0 || len(theirs) != 0 {
		t.Fatalf("another user's /me/reports = %d entries, want 0", len(theirs))
	}

	// The role gate names no required role (docs/10 §48 - no oracle about
	// which role would have passed).
	res = env.asOwner(t, normal, http.MethodGet, "/api/v1/admin/reports")
	if strings.Contains(strings.ToLower(string(res.body)), "moderator") ||
		strings.Contains(strings.ToLower(string(res.body)), "admin") {
		t.Fatalf("403 body names the required role: %s", res.body)
	}

	// A moderator passes the same doors.
	moderator := env.newModerator(t)
	res = env.asOwner(t, moderator, http.MethodGet, "/api/v1/admin/reports")
	if res.status != http.StatusOK {
		t.Fatalf("moderator queue status = %d, want 200. body: %s", res.status, res.body)
	}
}

// ---------------------------------------------------------------------------
// Actions per target - exercising the EXISTING states of earlier phases
// ---------------------------------------------------------------------------

func TestModeration_CommentActions(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	commenter := writer{webSession: env.registerWeb(t)}
	moderator := env.newModerator(t)

	res := env.asOwner(t, commenter, http.MethodPost, "/api/v1/novels/"+novel.ID+"/comments",
		map[string]string{"content": "ความคิดเห็นที่จะถูกซ่อน"})
	if res.status != http.StatusCreated {
		t.Fatalf("create comment status = %d. body: %s", res.status, res.body)
	}
	comment := dataOf[commentBody](t, res)

	threadLen := func() int {
		res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID+"/comments")
		items, _ := collectionOf[commentBody](t, res)
		return len(items)
	}
	if threadLen() != 1 {
		t.Fatal("comment did not appear in the public thread")
	}

	// Hide: the platform axis flips, the public thread empties.
	action := env.performAction(t, moderator, "comment", comment.ID, "hide")
	if action.Action != "hide" || action.Moderator.Username != moderator.username {
		t.Fatalf("audit row = %+v", action)
	}
	if threadLen() != 0 {
		t.Fatal("hidden comment still visible to the public")
	}

	// Hiding again is a state conflict, not a duplicate audit row.
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "comment", "target_id": comment.ID, "action": "hide"})
	if res.status != http.StatusConflict {
		t.Fatalf("re-hide status = %d, want 409", res.status)
	}

	// Restore brings it back.
	env.performAction(t, moderator, "comment", comment.ID, "restore")
	if threadLen() != 1 {
		t.Fatal("restored comment did not reappear")
	}

	// Remove takes it away again; the author's own deletion axis is untouched
	// throughout, and the audit trail holds every step.
	env.performAction(t, moderator, "comment", comment.ID, "remove")
	if threadLen() != 0 {
		t.Fatal("removed comment still visible")
	}

	res = env.asOwner(t, moderator, http.MethodGet,
		"/api/v1/admin/moderation/actions?target_type=comment&target_id="+comment.ID)
	history, total := collectionOf[actionBody](t, res)
	if total != 3 || len(history) != 3 {
		t.Fatalf("audit trail has %d entries (total %d), want 3", len(history), total)
	}
	if history[0].Action != "remove" || history[2].Action != "hide" {
		t.Fatalf("audit trail out of order: %+v", history)
	}

	// The commenter was told, without being told WHO (docs/11 §39): the
	// moderation notifications carry no actor.
	items := env.awaitNotifications(t, commenter, 3)
	moderationCount := 0
	for _, item := range items {
		if item.Type != "moderation" {
			continue
		}
		moderationCount++
		if item.Actor != nil {
			t.Fatalf("moderation notification exposes an actor: %+v", item)
		}
		if item.EntityType == nil || *item.EntityType != "comment" {
			t.Fatalf("moderation notification entity = %+v", item)
		}
	}
	if moderationCount != 3 {
		t.Fatalf("commenter holds %d moderation notifications, want 3 (types: %v)",
			moderationCount, typesOf(items))
	}

	// An author-deleted comment is beyond moderation: nothing left to act on.
	res = env.asOwner(t, commenter, http.MethodPost, "/api/v1/novels/"+novel.ID+"/comments",
		map[string]string{"content": "จะลบเอง"})
	own := dataOf[commentBody](t, res)
	if res := env.asOwner(t, commenter, http.MethodDelete, "/api/v1/comments/"+own.ID); res.status != http.StatusNoContent {
		t.Fatalf("self-delete status = %d", res.status)
	}
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "comment", "target_id": own.ID, "action": "hide"})
	if res.status != http.StatusNotFound {
		t.Fatalf("moderating author-deleted comment status = %d, want 404", res.status)
	}
}

func TestModeration_CommunityActions(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	poster := writer{webSession: env.registerWeb(t)}
	moderator := env.newModerator(t)

	res := env.asOwner(t, poster, http.MethodPost, "/api/v1/community/posts",
		map[string]string{"content": "โพสต์สาธารณะ", "visibility": "public"})
	post := dataOf[struct {
		ID string `json:"id"`
	}](t, res)

	res = env.asOwner(t, poster, http.MethodPost, "/api/v1/community/posts/"+post.ID+"/comments",
		map[string]string{"content": "คอมเมนต์ในโพสต์"})
	comment := dataOf[struct {
		ID string `json:"id"`
	}](t, res)

	// Hide the post: it vanishes for everyone, its thread goes with it.
	env.performAction(t, moderator, "community_post", post.ID, "hide")
	if res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID); res.status != http.StatusNotFound {
		t.Fatalf("hidden post GET status = %d, want 404", res.status)
	}
	// Even the owner loses it while moderated (the Phase 6/7 precedent).
	if res := env.asOwner(t, poster, http.MethodGet, "/api/v1/community/posts/"+post.ID); res.status != http.StatusNotFound {
		t.Fatalf("hidden post owner GET status = %d, want 404", res.status)
	}

	// Restore, then act on the community comment alone.
	env.performAction(t, moderator, "community_post", post.ID, "restore")
	if res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID); res.status != http.StatusOK {
		t.Fatalf("restored post GET status = %d, want 200", res.status)
	}

	env.performAction(t, moderator, "community_comment", comment.ID, "remove")
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID+"/comments")
	items, _ := collectionOf[json.RawMessage](t, res)
	if len(items) != 0 {
		t.Fatalf("removed community comment still listed (%d entries)", len(items))
	}

	// Wrong action for the target type is a validation error, not a 500.
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "community_post", "target_id": post.ID, "action": "ban"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("ban a post status = %d, want 422", res.status)
	}
}

func TestModeration_FictionActions(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title": "บทที่หนึ่ง", "content": "เนื้อหา", "status": "published",
	})
	moderator := env.newModerator(t)

	// Remove the chapter: it disappears from the public fiction.
	env.performAction(t, moderator, "chapter", chapter.ID, "remove")
	if res := env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID); res.status != http.StatusNotFound {
		t.Fatalf("removed chapter GET status = %d, want 404", res.status)
	}

	// Restore it; readers get it back exactly as it was.
	env.performAction(t, moderator, "chapter", chapter.ID, "restore")
	res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID)
	if res.status != http.StatusOK {
		t.Fatalf("restored chapter GET status = %d, want 200", res.status)
	}
	restored := dataOf[chapterBody](t, res)
	if restored.Content == nil || *restored.Content != "เนื้อหา" {
		t.Fatalf("restored chapter lost its content: %+v", restored)
	}

	// Remove the whole fiction.
	env.performAction(t, moderator, "novel", novel.ID, "remove")
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID); res.status != http.StatusNotFound {
		t.Fatalf("removed novel GET status = %d, want 404", res.status)
	}
	// Removing again is a state conflict.
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "novel", "target_id": novel.ID, "action": "remove"})
	if res.status != http.StatusConflict {
		t.Fatalf("re-remove novel status = %d, want 409", res.status)
	}
	// While the novel is removed, its chapters are not independently
	// moderatable - the fiction level owns that state.
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "chapter", "target_id": chapter.ID, "action": "remove"})
	if res.status != http.StatusNotFound {
		t.Fatalf("chapter action under removed novel status = %d, want 404", res.status)
	}

	// Restore the fiction: everything comes back, chapters included.
	env.performAction(t, moderator, "novel", novel.ID, "restore")
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID); res.status != http.StatusOK {
		t.Fatalf("restored novel GET status = %d, want 200", res.status)
	}
	if res := env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID); res.status != http.StatusOK {
		t.Fatalf("chapter after novel restore GET status = %d, want 200", res.status)
	}
	// Restoring a live novel is a state conflict.
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "novel", "target_id": novel.ID, "action": "restore"})
	if res.status != http.StatusConflict {
		t.Fatalf("restore live novel status = %d, want 409", res.status)
	}
	// Hide is not in the fiction matrix (no hidden state exists to move to).
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "novel", "target_id": novel.ID, "action": "hide"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("hide novel status = %d, want 422", res.status)
	}
}

func TestModeration_UserActions(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	target := writer{webSession: env.registerWeb(t)}
	moderator := env.newModerator(t)
	admin := env.newAdmin(t)

	me := func(w writer) int {
		return env.asOwner(t, w, http.MethodGet, "/api/v1/auth/me").status
	}

	// Warn: audit + notification, account untouched.
	env.performAction(t, moderator, "user", target.userID, "warn")
	if me(target) != http.StatusOK {
		t.Fatal("a warning must not touch the account")
	}

	// Suspend: the target's live session dies on its next request.
	env.performAction(t, moderator, "user", target.userID, "suspend")
	if me(target) != http.StatusUnauthorized {
		t.Fatal("suspended account still holds a live session")
	}
	// And signing in again is refused - the Phase 1 contract answers 403
	// ACCOUNT_UNAVAILABLE, checked only after the password matched so the
	// state is not an oracle for password guessing.
	res := env.do(t, apiRequest{method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{
			"identifier": target.username, "password": target.password, "client": "web"}})
	if res.status != http.StatusForbidden {
		t.Fatalf("suspended login status = %d, want 403", res.status)
	}

	// Suspending again is a state conflict.
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "user", "target_id": target.userID, "action": "suspend"})
	if res.status != http.StatusConflict {
		t.Fatalf("re-suspend status = %d, want 409", res.status)
	}

	// Restore: the account can sign in again (sessions stay cut - docs/10 §37).
	env.performAction(t, moderator, "user", target.userID, "restore")
	res = env.do(t, apiRequest{method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{
			"identifier": target.username, "password": target.password, "client": "web"}})
	if res.status != http.StatusOK {
		t.Fatalf("restored login status = %d, want 200. body: %s", res.status, res.body)
	}

	// Ban, then verify restore-from-ban also works.
	env.performAction(t, moderator, "user", target.userID, "ban")
	res = env.do(t, apiRequest{method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{
			"identifier": target.username, "password": target.password, "client": "web"}})
	if res.status != http.StatusForbidden {
		t.Fatalf("banned login status = %d, want 403", res.status)
	}
	env.performAction(t, admin, "user", target.userID, "restore")

	// The rank guard (role-escalation prevention): a moderator may not touch
	// staff; an admin may touch a moderator but never another admin.
	otherModerator := env.newModerator(t)
	otherAdmin := env.newAdmin(t)
	forbidden := []struct {
		actor  writer
		victim string
		label  string
	}{
		{moderator, otherModerator.userID, "moderator → moderator"},
		{moderator, admin.userID, "moderator → admin"},
		{admin, otherAdmin.userID, "admin → admin"},
	}
	for _, tc := range forbidden {
		res := env.asOwner(t, tc.actor, http.MethodPost, "/api/v1/admin/moderation/actions",
			map[string]string{"target_type": "user", "target_id": tc.victim, "action": "suspend"})
		if res.status != http.StatusForbidden {
			t.Errorf("%s suspend status = %d, want 403", tc.label, res.status)
		}
	}
	// An admin CAN suspend a moderator.
	env.performAction(t, admin, "user", otherModerator.userID, "suspend")
	if me(otherModerator) != http.StatusUnauthorized {
		t.Fatal("suspended moderator still holds a live session")
	}
}

// ---------------------------------------------------------------------------
// The staff report detail - snapshot, history, reporter privacy
// ---------------------------------------------------------------------------

func TestModeration_ReportDetailForStaff(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	poster := writer{webSession: env.registerWeb(t)}
	reporter := writer{webSession: env.registerWeb(t)}
	moderator := env.newModerator(t)

	content := "โพสต์ที่ถูกรายงาน " + uniqueName(t, "x")
	res := env.asOwner(t, poster, http.MethodPost, "/api/v1/community/posts",
		map[string]string{"content": content, "visibility": "public"})
	post := dataOf[struct {
		ID string `json:"id"`
	}](t, res)

	report := env.fileReport(t, reporter, "community_post", post.ID, "harassment")
	env.performAction(t, moderator, "community_post", post.ID, "hide")

	res = env.asOwner(t, moderator, http.MethodGet, "/api/v1/admin/reports/"+report.ID)
	if res.status != http.StatusOK {
		t.Fatalf("report detail status = %d. body: %s", res.status, res.body)
	}
	detail := dataOf[reportDetailBody](t, res)

	if detail.Report.Reporter == nil || detail.Report.Reporter.Username != reporter.username {
		t.Fatalf("detail missing reporter card: %+v", detail.Report)
	}
	if detail.Target == nil || !detail.Target.Exists {
		t.Fatalf("detail missing target snapshot: %+v", detail.Target)
	}
	if detail.Target.State != "hidden" {
		t.Fatalf("snapshot state = %q, want hidden (live re-read)", detail.Target.State)
	}
	if detail.Target.Excerpt == nil || !strings.Contains(*detail.Target.Excerpt, "โพสต์ที่ถูกรายงาน") {
		t.Fatalf("snapshot missing the reported content excerpt: %+v", detail.Target)
	}
	if detail.Target.Author == nil || detail.Target.Author.Username != poster.username {
		t.Fatalf("snapshot missing author card: %+v", detail.Target)
	}
	if len(detail.History) != 1 || detail.History[0].Action != "hide" {
		t.Fatalf("detail history = %+v, want the hide action", detail.History)
	}
	if fmt.Sprintf("%v", detail.AvailableActions) != "[hide remove restore]" {
		t.Fatalf("available actions = %v", detail.AvailableActions)
	}

	// A report on a NOVEL never carries manuscript content in its snapshot
	// (docs/11 §39): metadata only.
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title": "ลับสุดยอด", "content": "MANUSCRIPT-CONTENT-MUST-NOT-LEAK", "status": "published",
	})
	novelReport := env.fileReport(t, reporter, "chapter", chapter.ID, "copyright")
	res = env.asOwner(t, moderator, http.MethodGet, "/api/v1/admin/reports/"+novelReport.ID)
	if strings.Contains(string(res.body), "MANUSCRIPT-CONTENT-MUST-NOT-LEAK") {
		t.Fatal("chapter snapshot leaked manuscript content")
	}
	chapterDetail := dataOf[reportDetailBody](t, res)
	if chapterDetail.Target == nil || chapterDetail.Target.Title == nil ||
		*chapterDetail.Target.Title != "ลับสุดยอด" {
		t.Fatalf("chapter snapshot metadata = %+v", chapterDetail.Target)
	}
}
