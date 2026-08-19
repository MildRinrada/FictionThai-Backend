package integration

import (
	"net/http"
	"net/url"
	"testing"
)

// Phase 13 - the three ladders that decide who reaches what
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13B, §13C, §13D).
//
// They are tested together because they fail together: each one replaced a
// two-value control with a graded one, and the failure mode in every case is
// the same shape - a rung that silently behaves like the widest value. The
// assertions here are therefore mostly about what a caller CANNOT reach.

// A listing row with the two fields the ladders decide. novelBody carries
// visibility as a pointer (owner-only) and no rating at all, so the browse
// assertions read their own shape rather than widening a shared one.
type ladderBody struct {
	ID         string  `json:"id"`
	Slug       string  `json:"slug"`
	AgeRating  string  `json:"age_rating"`
	AgeGate    string  `json:"age_gate"`
	Visibility *string `json:"visibility"`
	Author     struct {
		Username string `json:"username"`
	} `json:"author"`
}

type commentAccessBody struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Pending   bool   `json:"pending"`
	GuestName string `json:"guest_name"`
	Author    *struct {
		Username string `json:"username"`
	} `json:"author"`
}

// ---------------------------------------------------------------------------
// §13C - the visibility ladder
// ---------------------------------------------------------------------------

// The two new rungs are the whole point: publish, but not to the open internet.
// A guest must be turned away from both, and the SQL predicate every other
// domain filters with must agree with the Go rule.
func TestVisibility_MembersAndFollowersRungs(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	member := env.newWriter(t)
	follower := env.newWriter(t)

	authorID := env.whoAmI(t, author)
	env.asOwner(t, follower, http.MethodPost, "/api/v1/users/"+authorID+"/follow")

	members := env.publishedNovel(t, author, map[string]any{"visibility": "members"})
	followers := env.publishedNovel(t, author, map[string]any{"visibility": "followers"})

	// A guest reaches neither. Not 403: a fiction they may not open does not
	// exist for them (docs/11 §3.4).
	for label, novel := range map[string]novelBody{"members": members, "followers": followers} {
		if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug); res.status != http.StatusNotFound {
			t.Fatalf("guest reading %s-only work status = %d, want 404", label, res.status)
		}
	}

	// Any account clears the members rung.
	if res := env.asOwner(t, member, http.MethodGet,
		"/api/v1/novels/"+members.Slug); res.status != http.StatusOK {
		t.Fatalf("member reading members-only status = %d, want 200. body: %s", res.status, res.body)
	}

	// The followers rung asks for a named relationship, and an ordinary account
	// is not in it.
	if res := env.asOwner(t, member, http.MethodGet,
		"/api/v1/novels/"+followers.Slug); res.status != http.StatusNotFound {
		t.Fatalf("non-follower reading followers-only status = %d, want 404", res.status)
	}
	if res := env.asOwner(t, follower, http.MethodGet,
		"/api/v1/novels/"+followers.Slug); res.status != http.StatusOK {
		t.Fatalf("follower reading followers-only status = %d, want 200. body: %s", res.status, res.body)
	}

	// The author always reaches their own work, without following themselves.
	if res := env.asOwner(t, author, http.MethodGet,
		"/api/v1/novels/"+followers.Slug); res.status != http.StatusOK {
		t.Fatalf("author reading their own followers-only work status = %d", res.status)
	}
}

// Listing is a SEPARATE question from reading. Members-only work is meant to be
// found - the gate is at the door, not on the map - while followers-only and
// unlisted work must not appear in a browse surface at all.
func TestVisibility_ListingFollowsTheRung(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	member := env.newWriter(t)

	env.publishedNovel(t, author, map[string]any{"visibility": "members"})
	env.publishedNovel(t, author, map[string]any{"visibility": "followers"})
	env.publishedNovel(t, author, map[string]any{"visibility": "unlisted"})
	public := env.publishedNovel(t, author, nil)

	scope := "&author=" + url.QueryEscape(public.Author.Username)

	// A guest sees only the public one - including the members rung, which they
	// could not open. Offering a card that leads to a closed door is worse than
	// not offering it.
	guest := env.asGuest(t, http.MethodGet, "/api/v1/novels?per_page=50"+scope)
	listed, _ := collectionOf[ladderBody](t, guest)
	if len(listed) != 1 || listed[0].ID != public.ID {
		t.Fatalf("guest listing = %d fictions, want just the public one", len(listed))
	}

	// A signed-in reader sees the members one too, and still not the other two.
	res := env.asOwner(t, member, http.MethodGet, "/api/v1/novels?per_page=50"+scope)
	listed, _ = collectionOf[ladderBody](t, res)
	if len(listed) != 2 {
		t.Fatalf("member listing = %d fictions, want the public and the members one", len(listed))
	}
}

// ---------------------------------------------------------------------------
// §13B - 18+ split into two, and the reader's own switch
// ---------------------------------------------------------------------------

// Explicit work is never served behind a dismissible warning. The pair is
// refused with a field error rather than quietly upgraded: a control the
// platform overrides in silence is worse than a control that says no.
func TestAdult_ExplicitRefusesTheWarningGate(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels", createNovelBody(
		uniqueName(t, "Explicit "), map[string]any{
			"age_rating": "explicit",
			"age_gate":   "warning",
		}))
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("explicit + warning status = %d, want 422. body: %s", res.status, res.body)
	}

	// An OMITTED gate is not an error: it takes the rating's own default, so a
	// writer is never rejected for a field they never touched.
	created := dataOf[ratingBody](t, env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		createNovelBody(uniqueName(t, "Explicit "), map[string]any{"age_rating": "explicit"})))
	if created.AgeGate != "login" {
		t.Fatalf("age_gate = %q, want login by default on explicit work", created.AgeGate)
	}
}

// The author states once, at their account, that they are an adult. Until they
// do, 18+ work does not publish - and the checklist says so rather than letting
// the publish fail with no explanation.
func TestAdult_AttestationGatesPublishing(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Mature "),
		map[string]any{"age_rating": "mature"}))
	env.completeChecklist(t, w, novel.ID)

	// completeChecklist attests, so take the statement back out of the picture
	// by using a second writer who has not made it.
	other := env.newWriter(t)
	fresh := env.createNovel(t, other, createNovelBody(uniqueName(t, "Mature "),
		map[string]any{"age_rating": "mature"}))

	publish := map[string]any{
		"description":     "เรื่องย่อ",
		"cover_url":       "https://example.com/c.jpg",
		"content_warning": "18+",
		"genre_ids":       []string{env.seededGenres(t)["fantasy"].ID},
		"tag_ids":         []string{env.newTag(t, other, uniqueName(t, "แท็ก ")).ID},
		"status":          "ongoing",
		"visibility":      "public",
	}

	res := env.asOwner(t, other, http.MethodPatch, "/api/v1/novels/"+fresh.ID, publish)
	if res.status != http.StatusForbidden {
		t.Fatalf("publishing 18+ without attesting status = %d, want 403. body: %s", res.status, res.body)
	}
	if code := errorCodeOf(t, res); code != "ADULT_ATTESTATION_REQUIRED" {
		t.Fatalf("error code = %q, want ADULT_ATTESTATION_REQUIRED", code)
	}

	// A refused publish writes NOTHING: the same request has to be resent, and
	// resending it after attesting is what proves the gate was the only thing
	// standing in the way.
	if res := env.asOwner(t, other, http.MethodPost,
		"/api/v1/auth/adult-attestation", nil); res.status != http.StatusOK {
		t.Fatalf("attestation status = %d. body: %s", res.status, res.body)
	}
	res = env.asOwner(t, other, http.MethodPatch, "/api/v1/novels/"+fresh.ID, publish)
	if res.status != http.StatusOK {
		t.Fatalf("publishing after attesting status = %d, want 200. body: %s", res.status, res.body)
	}
}

// 18+ work stays out of browse surfaces by default. A signed-in reader may lift
// that for `mature`; explicit work is never listed either way, and a guest
// asking for it changes nothing.
func TestAdult_ListingExclusionAndTheReaderSwitch(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	mature := env.publishedNovel(t, author, map[string]any{"age_rating": "mature"})
	env.publishedNovel(t, author, map[string]any{"age_rating": "explicit", "age_gate": "login"})
	general := env.publishedNovel(t, author, nil)

	scope := "&author=" + url.QueryEscape(general.Author.Username)

	// Default: neither 18+ fiction is discoverable.
	res := env.asGuest(t, http.MethodGet, "/api/v1/novels?per_page=50"+scope)
	listed, _ := collectionOf[ladderBody](t, res)
	if len(listed) != 1 || listed[0].ID != general.ID {
		t.Fatalf("default listing = %d fictions, want just the general one", len(listed))
	}

	// A guest cannot lift it by asking.
	res = env.asGuest(t, http.MethodGet, "/api/v1/novels?adult=1&per_page=50"+scope)
	listed, _ = collectionOf[ladderBody](t, res)
	if len(listed) != 1 {
		t.Fatalf("a guest asking for adult work got %d fictions, want 1", len(listed))
	}

	// A signed-in reader turning "ซ่อนเนื้อหา 18+" off sees mature work -
	// and still not the explicit one.
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/novels?adult=1&per_page=50"+scope)
	listed, _ = collectionOf[ladderBody](t, res)
	if len(listed) != 2 {
		t.Fatalf("reader with adult=1 got %d fictions, want the general and the mature one", len(listed))
	}
	foundMature := false
	for _, novel := range listed {
		if novel.ID == mature.ID {
			foundMature = true
		}
		if novel.AgeRating == "explicit" {
			t.Fatal("explicit work appeared in a listing; it is reachable by link only")
		}
	}
	if !foundMature {
		t.Fatal("mature work stayed hidden from a reader who asked for it")
	}
}

// ---------------------------------------------------------------------------
// §13D - comments in three levels, and the queue that makes them survivable
// ---------------------------------------------------------------------------

// The level that this platform exists for: a reader with no account says
// something. It arrives, it waits, and the author decides.
func TestComments_GuestsPostAndWaitForApproval(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)

	novel := env.publishedNovel(t, author, map[string]any{"comment_access": "everyone"})
	path := "/api/v1/novels/" + novel.Slug + "/comments"

	// A name is required. A thread of identical anonymous rows is unreadable.
	res := env.asGuest(t, http.MethodPost, path, map[string]any{"content": "อ่านแล้วชอบมาก"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("guest comment with no name status = %d, want 422. body: %s", res.status, res.body)
	}

	res = env.asGuest(t, http.MethodPost, path, map[string]any{
		"content": "อ่านแล้วชอบมาก", "guest_name": "คนอ่านผ่านมา",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("guest comment status = %d, want 201. body: %s", res.status, res.body)
	}
	posted := dataOf[commentAccessBody](t, res)
	if !posted.Pending {
		t.Fatal("a guest comment must be held for review, and must say so to the person who wrote it")
	}
	if posted.Author != nil {
		t.Fatal("a guest comment must carry no author card")
	}
	if posted.GuestName != "คนอ่านผ่านมา" {
		t.Fatalf("guest_name = %q", posted.GuestName)
	}

	// Nobody sees it yet - not a guest, not the author's own reader listing.
	listed, total := collectionOf[commentAccessBody](t, env.asGuest(t, http.MethodGet, path))
	if total != 0 || len(listed) != 0 {
		t.Fatalf("a pending comment appeared in the public thread (%d)", total)
	}

	// The author finds it in the queue.
	queue := env.asOwner(t, author, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/comments/pending")
	waiting, waitingTotal := collectionOf[commentAccessBody](t, queue)
	if waitingTotal != 1 || len(waiting) != 1 || waiting[0].ID != posted.ID {
		t.Fatalf("review queue = %d comments, want the one that arrived. body: %s", waitingTotal, queue.body)
	}

	// A stranger cannot look at somebody else's queue.
	stranger := env.newWriter(t)
	if res := env.asOwner(t, stranger, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/comments/pending"); res.status == http.StatusOK {
		t.Fatal("a stranger read another author's review queue")
	}

	// Approving publishes it.
	if res := env.asOwner(t, author, http.MethodPost,
		"/api/v1/comments/"+posted.ID+"/approve", nil); res.status != http.StatusOK {
		t.Fatalf("approve status = %d. body: %s", res.status, res.body)
	}
	listed, total = collectionOf[commentAccessBody](t, env.asGuest(t, http.MethodGet, path))
	if total != 1 || len(listed) != 1 || listed[0].ID != posted.ID {
		t.Fatalf("an approved comment did not join the thread (%d)", total)
	}

	// And it cannot be decided twice.
	if res := env.asOwner(t, author, http.MethodPost,
		"/api/v1/comments/"+posted.ID+"/approve", nil); res.status != http.StatusConflict {
		t.Fatalf("second decision status = %d, want 409", res.status)
	}
}

// The middle rung, which is what every existing fiction is on: an account is
// required, and a guest gets the 401 the UI turns into a sign-in offer.
func TestComments_MembersRungRefusesGuests(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, author, nil) // members is the default
	path := "/api/v1/novels/" + novel.Slug + "/comments"

	res := env.asGuest(t, http.MethodPost, path, map[string]any{
		"content": "ขอคอมเมนต์", "guest_name": "ไม่ได้ล็อกอิน",
	})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("guest on the members rung status = %d, want 401. body: %s", res.status, res.body)
	}

	// A member's comment appears immediately when approval is off.
	res = env.asOwner(t, reader, http.MethodPost, path, map[string]any{"content": "สนุกมาก"})
	if res.status != http.StatusCreated {
		t.Fatalf("member comment status = %d. body: %s", res.status, res.body)
	}
	if dataOf[commentAccessBody](t, res).Pending {
		t.Fatal("a member comment was held although the author never asked for review")
	}
}

// ตรวจก่อนโพสต์ applies to members too when the author turns it on - and never
// to the author's own comments, since approving yourself is not review.
func TestComments_ApprovalHoldsMembersButNotTheAuthor(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, author, map[string]any{"comment_approval": true})
	path := "/api/v1/novels/" + novel.Slug + "/comments"

	res := env.asOwner(t, reader, http.MethodPost, path, map[string]any{"content": "รอตรวจ"})
	if res.status != http.StatusCreated {
		t.Fatalf("member comment status = %d. body: %s", res.status, res.body)
	}
	held := dataOf[commentAccessBody](t, res)
	if !held.Pending {
		t.Fatal("approval is on, so a member comment must wait")
	}

	res = env.asOwner(t, author, http.MethodPost, path, map[string]any{"content": "ขอบคุณที่อ่านนะคะ"})
	if dataOf[commentAccessBody](t, res).Pending {
		t.Fatal("the author's own comment was queued for the author to approve")
	}

	// Rejecting removes it from the queue without deleting the row.
	if res := env.asOwner(t, author, http.MethodPost,
		"/api/v1/comments/"+held.ID+"/reject", nil); res.status != http.StatusOK {
		t.Fatalf("reject status = %d. body: %s", res.status, res.body)
	}
	_, waiting := collectionOf[commentAccessBody](t, env.asOwner(t, author, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/comments/pending"))
	if waiting != 0 {
		t.Fatalf("queue still holds %d comments after a decision", waiting)
	}
	_, total := collectionOf[commentAccessBody](t, env.asGuest(t, http.MethodGet, path))
	if total != 1 {
		t.Fatalf("public thread = %d comments, want only the author's own", total)
	}
}
