package integration

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Phase 3 - Reader & Library over the real HTTP API.
//
// The properties that matter most here are the negative ones: a shelf must
// never become a probe for private work (docs/11 §31), progress must never
// attach across fictions (docs/11 §21), and nothing an author does may delete
// a reader's shelf state (docs/08 §3).

// libraryEntryBody is the decoded shape of one /me/library entry.
type libraryEntryBody struct {
	Novel        novelBody `json:"novel"`
	BookmarkedAt string    `json:"bookmarked_at"`
}

// continueReadingBody is the decoded shape of one /me/reading-progress entry.
type continueReadingBody struct {
	Novel   novelBody `json:"novel"`
	Chapter *struct {
		ID            string  `json:"id"`
		ChapterNumber int     `json:"chapter_number"`
		Slug          string  `json:"slug"`
		Title         *string `json:"title"`
	} `json:"chapter"`
	ProgressPercent float64 `json:"progress_percent"`
	LastReadAt      string  `json:"last_read_at"`
}

// progressBody is the decoded shape of a saved position.
type progressBody struct {
	NovelID         string  `json:"novel_id"`
	ChapterID       string  `json:"chapter_id"`
	ProgressPercent float64 `json:"progress_percent"`
	LastReadAt      string  `json:"last_read_at"`
}

// followedAuthorBody is the decoded shape of one /me/following entry.
type followedAuthorBody struct {
	Author struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
	FollowedAt string `json:"followed_at"`
}

// readableChapter publishes a chapter with prose so a fiction has something a
// reader can hold a position in.
func (e *authEnv) readableChapter(t *testing.T, w writer, novelID string) chapterBody {
	t.Helper()
	chapter := e.createChapter(t, w, novelID, map[string]any{
		"title":   "ตอนสำหรับอ่าน",
		"content": "ฝนตกทั้งคืน แต่เช้านี้ฟ้าเปิดแล้ว",
	})
	return e.publishChapter(t, w, novelID, chapter.ID)
}

// ---------------------------------------------------------------------------
// Bookmarks
// ---------------------------------------------------------------------------

func TestBookmarks_SaveAndListWithFormatMetadata(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, author, map[string]any{
		"story_structure":     "one_shot",
		"presentation_format": "chat",
		"content_mode":        "headcanon",
	})

	if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.ID+"/bookmark", nil); res.status != http.StatusNoContent {
		t.Fatalf("bookmark status = %d, want 204. body: %s", res.status, res.body)
	}

	status := dataOf[struct {
		IsBookmarked bool `json:"is_bookmarked"`
	}](t, env.asOwner(t, reader, http.MethodGet, "/api/v1/novels/"+novel.ID+"/bookmark"))
	if !status.IsBookmarked {
		t.Fatal("is_bookmarked = false after bookmarking")
	}

	entries, total := collectionOf[libraryEntryBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/library"))
	if total != 1 || len(entries) != 1 {
		t.Fatalf("library total = %d, entries = %d, want 1 and 1", total, len(entries))
	}

	// docs/09 §18: the library card carries the full format metadata so clients
	// can render badges without a second request.
	card := entries[0].Novel
	if card.StoryStructure != "one_shot" || card.PresentationFormat != "chat" || card.ContentMode != "headcanon" {
		t.Errorf("library card format = %s+%s+%s, want one_shot+chat+headcanon",
			card.StoryStructure, card.PresentationFormat, card.ContentMode)
	}
	if entries[0].BookmarkedAt == "" {
		t.Error("bookmarked_at is empty")
	}
	// The reader is not the owner: owner-only fields must be absent.
	if card.Visibility != nil {
		t.Error("library card leaked visibility to a non-owner")
	}
}

func TestBookmarks_RepeatIsIdempotent(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	for range 3 {
		if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.ID+"/bookmark", nil); res.status != http.StatusNoContent {
			t.Fatalf("bookmark status = %d, want 204. body: %s", res.status, res.body)
		}
	}

	_, total := collectionOf[libraryEntryBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/library"))
	if total != 1 {
		t.Fatalf("library total = %d after repeated bookmarking, want 1 (docs/09 §33)", total)
	}
}

func TestBookmarks_RequireAuthenticationAndCSRF(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	// A guest has no shelf.
	if res := env.asGuest(t, http.MethodPost, "/api/v1/novels/"+novel.ID+"/bookmark"); res.status != http.StatusUnauthorized {
		t.Errorf("guest bookmark status = %d, want 401", res.status)
	}
	if res := env.asGuest(t, http.MethodGet, "/api/v1/me/library"); res.status != http.StatusUnauthorized {
		t.Errorf("guest library status = %d, want 401", res.status)
	}

	// A cookie session mutating without the CSRF header is exactly the forged
	// cross-site request the double-submit check exists for (docs/11 §22).
	reader := env.newWriter(t)
	res := env.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/novels/" + novel.ID + "/bookmark",
		cookies: reader.authCookies(),
		// no csrf token
	})
	if res.status != http.StatusForbidden {
		t.Errorf("bookmark without CSRF = %d, want 403. body: %s", res.status, res.body)
	}
}

func TestBookmarks_CannotProbePrivateWork(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	stranger := env.newWriter(t)

	draft := env.createNovel(t, author, createNovelBody(uniqueName(t, "Draft "), nil))

	// Bookmarking an unreadable fiction answers the same 404 a missing one
	// does - the endpoint is not an existence oracle (docs/11 §3.4).
	res := env.asOwner(t, stranger, http.MethodPost, "/api/v1/novels/"+draft.ID+"/bookmark", nil)
	if res.status != http.StatusNotFound {
		t.Errorf("bookmark private draft = %d, want 404. body: %s", res.status, res.body)
	}
	if code := errorCodeOf(t, res); code != "NOVEL_NOT_FOUND" {
		t.Errorf("error code = %q, want NOVEL_NOT_FOUND", code)
	}

	// The owner may shelve their own draft, and it appears in their own library.
	if res := env.asOwner(t, author, http.MethodPost, "/api/v1/novels/"+draft.ID+"/bookmark", nil); res.status != http.StatusNoContent {
		t.Fatalf("owner bookmark own draft = %d, want 204. body: %s", res.status, res.body)
	}
	entries, _ := collectionOf[libraryEntryBody](t,
		env.asOwner(t, author, http.MethodGet, "/api/v1/me/library"))
	if len(entries) != 1 || entries[0].Novel.ID != draft.ID {
		t.Fatalf("owner's library does not contain their own draft")
	}
}

func TestBookmarks_HiddenWhenNovelBecomesPrivateButRemovalStillWorks(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.ID+"/bookmark", nil); res.status != http.StatusNoContent {
		t.Fatalf("bookmark status = %d. body: %s", res.status, res.body)
	}

	// The author withdraws the fiction.
	if res := env.asOwner(t, author, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"visibility": "private"}); res.status != http.StatusOK {
		t.Fatalf("make private status = %d. body: %s", res.status, res.body)
	}

	// The shelf no longer SHOWS it (docs/11 §31) …
	entries, total := collectionOf[libraryEntryBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/library"))
	if total != 0 || len(entries) != 0 {
		t.Fatalf("library shows a private fiction: total = %d", total)
	}

	// … but the row is retained, and removal still works (docs/01 §11).
	if res := env.asOwner(t, reader, http.MethodDelete, "/api/v1/novels/"+novel.ID+"/bookmark", nil); res.status != http.StatusNoContent {
		t.Errorf("remove bookmark of private fiction = %d, want 204. body: %s", res.status, res.body)
	}
}

func TestBookmarks_SurviveFormatChange(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.ID+"/bookmark", nil); res.status != http.StatusNoContent {
		t.Fatalf("bookmark status = %d. body: %s", res.status, res.body)
	}

	// docs/08 §3: changing a format must not delete bookmarks.
	if res := env.asOwner(t, author, http.MethodPatch, "/api/v1/novels/"+novel.ID+"/format",
		map[string]any{"presentation_format": "chat"}); res.status != http.StatusOK {
		t.Fatalf("format change status = %d. body: %s", res.status, res.body)
	}

	entries, total := collectionOf[libraryEntryBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/library"))
	if total != 1 {
		t.Fatalf("library total = %d after format change, want 1", total)
	}
	if entries[0].Novel.PresentationFormat != "chat" {
		t.Errorf("library card format = %q, want the NEW format", entries[0].Novel.PresentationFormat)
	}
}

func TestBookmarks_StatusFilterForLibrarySections(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	ongoing := env.publishedNovel(t, author, nil)
	completed := env.publishedNovel(t, author, map[string]any{"status": "completed"})

	for _, id := range []string{ongoing.ID, completed.ID} {
		if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+id+"/bookmark", nil); res.status != http.StatusNoContent {
			t.Fatalf("bookmark status = %d. body: %s", res.status, res.body)
		}
	}

	// The "Completed" shelf (docs/03 §13).
	entries, total := collectionOf[libraryEntryBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/library?status=completed"))
	if total != 1 || len(entries) != 1 || entries[0].Novel.ID != completed.ID {
		t.Fatalf("status=completed returned %d entries, want just the completed fiction", len(entries))
	}

	// An unsupported filter is a clean 422, not a silently empty page.
	if res := env.asOwner(t, reader, http.MethodGet, "/api/v1/me/library?status=finished"); res.status != http.StatusUnprocessableEntity {
		t.Errorf("unknown status filter = %d, want 422", res.status)
	}
}

// ---------------------------------------------------------------------------
// Follows
// ---------------------------------------------------------------------------

func TestFollows_FollowStatusAndUnfollow(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	authorID := novel.Author.ID

	// Repeated follows are one row (docs/09 §33).
	for range 2 {
		if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/users/"+authorID+"/follow", nil); res.status != http.StatusNoContent {
			t.Fatalf("follow status = %d, want 204. body: %s", res.status, res.body)
		}
	}

	status := dataOf[struct {
		IsFollowing bool `json:"is_following"`
	}](t, env.asOwner(t, reader, http.MethodGet, "/api/v1/users/"+authorID+"/follow-status"))
	if !status.IsFollowing {
		t.Fatal("is_following = false after following")
	}

	following, total := collectionOf[followedAuthorBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/following"))
	if total != 1 || len(following) != 1 || following[0].Author.ID != authorID {
		t.Fatalf("/me/following total = %d, want the one followed author", total)
	}

	if res := env.asOwner(t, reader, http.MethodDelete, "/api/v1/users/"+authorID+"/follow", nil); res.status != http.StatusNoContent {
		t.Fatalf("unfollow status = %d, want 204. body: %s", res.status, res.body)
	}
	status = dataOf[struct {
		IsFollowing bool `json:"is_following"`
	}](t, env.asOwner(t, reader, http.MethodGet, "/api/v1/users/"+authorID+"/follow-status"))
	if status.IsFollowing {
		t.Fatal("is_following = true after unfollowing")
	}
}

func TestFollows_RejectSelfAndUnknownTargets(t *testing.T) {
	env := newAuthEnv(t)
	reader := env.newWriter(t)

	me := dataOf[struct {
		ID string `json:"id"`
	}](t, env.asOwner(t, reader, http.MethodGet, "/api/v1/auth/me"))

	// docs/08 §17.1: users cannot follow themselves.
	res := env.asOwner(t, reader, http.MethodPost, "/api/v1/users/"+me.ID+"/follow", nil)
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("self-follow status = %d, want 422. body: %s", res.status, res.body)
	}

	// An unknown user and a malformed id answer identically, so the parameter
	// is not an oracle.
	unknown := uuid.NewString()
	for _, target := range []string{unknown, "not-a-uuid"} {
		res := env.asOwner(t, reader, http.MethodPost, "/api/v1/users/"+target+"/follow", nil)
		if res.status != http.StatusNotFound {
			t.Errorf("follow %q status = %d, want 404", target, res.status)
		}
		if code := errorCodeOf(t, res); code != "USER_NOT_FOUND" {
			t.Errorf("follow %q error code = %q, want USER_NOT_FOUND", target, code)
		}
	}

	// A guest cannot follow.
	if res := env.asGuest(t, http.MethodPost, "/api/v1/users/"+unknown+"/follow"); res.status != http.StatusUnauthorized {
		t.Errorf("guest follow status = %d, want 401", res.status)
	}
}

// ---------------------------------------------------------------------------
// Reading progress
// ---------------------------------------------------------------------------

func TestProgress_SaveGetAndUpsert(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, author, nil)
	first := env.readableChapter(t, author, novel.ID)
	second := env.readableChapter(t, author, novel.ID)

	// First save.
	saved := dataOf[progressBody](t, env.asOwner(t, reader, http.MethodPut,
		"/api/v1/novels/"+novel.ID+"/progress",
		map[string]any{"chapter_id": first.ID, "progress_percent": 42.5}))
	if saved.ChapterID != first.ID || saved.ProgressPercent != 42.5 {
		t.Fatalf("saved progress = %+v, want chapter %s at 42.5", saved, first.ID)
	}

	// A later save REPLACES the position - one row per (user, novel)
	// (docs/08 §18.1).
	dataOf[progressBody](t, env.asOwner(t, reader, http.MethodPut,
		"/api/v1/novels/"+novel.ID+"/progress",
		map[string]any{"chapter_id": second.ID, "progress_percent": 10}))

	got := dataOf[progressBody](t, env.asOwner(t, reader, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/progress"))
	if got.ChapterID != second.ID || got.ProgressPercent != 10 {
		t.Fatalf("progress after second save = %+v, want chapter %s at 10", got, second.ID)
	}

	entries, total := collectionOf[continueReadingBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/reading-progress"))
	if total != 1 || len(entries) != 1 {
		t.Fatalf("continue reading total = %d, want exactly 1 (upsert, not append)", total)
	}
	if entries[0].Chapter == nil || entries[0].Chapter.ID != second.ID {
		t.Fatalf("continue reading points at the wrong chapter")
	}
	if entries[0].Novel.Title != novel.Title {
		t.Errorf("continue reading card lacks the fiction")
	}
}

func TestProgress_ContinueReadingOrdersByRecency(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	older := env.publishedNovel(t, author, nil)
	newer := env.publishedNovel(t, author, nil)
	olderChapter := env.readableChapter(t, author, older.ID)
	newerChapter := env.readableChapter(t, author, newer.ID)

	for _, save := range []struct{ novelID, chapterID string }{
		{older.ID, olderChapter.ID},
		{newer.ID, newerChapter.ID},
	} {
		res := env.asOwner(t, reader, http.MethodPut, "/api/v1/novels/"+save.novelID+"/progress",
			map[string]any{"chapter_id": save.chapterID, "progress_percent": 50})
		if res.status != http.StatusOK {
			t.Fatalf("save progress status = %d. body: %s", res.status, res.body)
		}
	}

	entries, _ := collectionOf[continueReadingBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/reading-progress"))
	if len(entries) != 2 {
		t.Fatalf("continue reading entries = %d, want 2", len(entries))
	}
	// Most recently read first - that is what "Continue Reading" means.
	if entries[0].Novel.ID != newer.ID || entries[1].Novel.ID != older.ID {
		t.Errorf("continue reading order = [%s, %s], want most recent first",
			entries[0].Novel.Title, entries[1].Novel.Title)
	}
}

func TestProgress_RejectsForeignAndUnpublishedChapters(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, author, nil)
	env.readableChapter(t, author, novel.ID)

	otherNovel := env.publishedNovel(t, author, nil)
	foreignChapter := env.readableChapter(t, author, otherNovel.ID)

	// A chapter of ANOTHER fiction can never be attached here (IDOR,
	// docs/11 §21).
	res := env.asOwner(t, reader, http.MethodPut, "/api/v1/novels/"+novel.ID+"/progress",
		map[string]any{"chapter_id": foreignChapter.ID, "progress_percent": 5})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("foreign chapter status = %d, want 422. body: %s", res.status, res.body)
	}

	// A draft chapter is invisible to a reader - saving a position in it must
	// answer exactly like a chapter that does not exist.
	draft := env.createChapter(t, author, novel.ID, map[string]any{"content": "ฉบับร่าง"})
	res = env.asOwner(t, reader, http.MethodPut, "/api/v1/novels/"+novel.ID+"/progress",
		map[string]any{"chapter_id": draft.ID, "progress_percent": 5})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("draft chapter status = %d, want 422. body: %s", res.status, res.body)
	}

	// The OWNER may hold a position in their own draft (previewing).
	res = env.asOwner(t, author, http.MethodPut, "/api/v1/novels/"+novel.ID+"/progress",
		map[string]any{"chapter_id": draft.ID, "progress_percent": 5})
	if res.status != http.StatusOK {
		t.Errorf("owner draft progress status = %d, want 200. body: %s", res.status, res.body)
	}
}

func TestProgress_ValidatesInputAndVisibility(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, author, nil)
	chapter := env.readableChapter(t, author, novel.ID)

	for name, body := range map[string]map[string]any{
		"missing chapter":  {"progress_percent": 10},
		"missing percent":  {"chapter_id": chapter.ID},
		"percent over 100": {"chapter_id": chapter.ID, "progress_percent": 150},
		"negative percent": {"chapter_id": chapter.ID, "progress_percent": -1},
		"malformed id":     {"chapter_id": "not-a-uuid", "progress_percent": 10},
	} {
		res := env.asOwner(t, reader, http.MethodPut, "/api/v1/novels/"+novel.ID+"/progress", body)
		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422. body: %s", name, res.status, res.body)
		}
	}

	// A private draft is unreachable, so its progress endpoints answer 404.
	draft := env.createNovel(t, author, createNovelBody(uniqueName(t, "Draft "), nil))
	res := env.asOwner(t, reader, http.MethodPut, "/api/v1/novels/"+draft.ID+"/progress",
		map[string]any{"chapter_id": chapter.ID, "progress_percent": 10})
	if res.status != http.StatusNotFound {
		t.Errorf("progress on private draft = %d, want 404", res.status)
	}

	// No position yet is its own clean answer.
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/novels/"+novel.ID+"/progress")
	if res.status != http.StatusNotFound {
		t.Errorf("unstarted progress = %d, want 404. body: %s", res.status, res.body)
	}
	if code := errorCodeOf(t, res); code != "READING_PROGRESS_NOT_FOUND" {
		t.Errorf("error code = %q, want READING_PROGRESS_NOT_FOUND", code)
	}

	// Guests keep progress on their device, never on the server (docs/03 §11).
	if res := env.asGuest(t, http.MethodGet, "/api/v1/me/reading-progress"); res.status != http.StatusUnauthorized {
		t.Errorf("guest reading-progress = %d, want 401", res.status)
	}
}

func TestProgress_SurvivesChapterUnpublish(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, author, nil)
	chapter := env.readableChapter(t, author, novel.ID)

	if res := env.asOwner(t, reader, http.MethodPut, "/api/v1/novels/"+novel.ID+"/progress",
		map[string]any{"chapter_id": chapter.ID, "progress_percent": 80}); res.status != http.StatusOK {
		t.Fatalf("save progress status = %d. body: %s", res.status, res.body)
	}

	// The author retracts the chapter.
	if res := env.asOwner(t, author, http.MethodPost,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID+"/unpublish", nil); res.status != http.StatusOK {
		t.Fatalf("unpublish status = %d. body: %s", res.status, res.body)
	}

	// The entry SURVIVES (docs/08 §3 - nothing an author does deletes a
	// reader's progress); only the resume link is gone.
	entries, total := collectionOf[continueReadingBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/reading-progress"))
	if total != 1 || len(entries) != 1 {
		t.Fatalf("continue reading total = %d after unpublish, want 1", total)
	}
	if entries[0].Chapter != nil {
		t.Errorf("chapter ref should be null for an unpublished chapter")
	}
	if entries[0].ProgressPercent != 80 {
		t.Errorf("progress_percent = %v, want the saved 80", entries[0].ProgressPercent)
	}
}

func TestLibrary_NoVerificationGateOnShelfActions(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	chapter := env.readableChapter(t, author, novel.ID)

	// Email verification gates PUBLISHING, never ordinary account use
	// (docs/AUTHENTICATION.md §9) - an unverified reader's shelf works fully.
	unverified := env.newUnverifiedWriter(t)

	if res := env.asOwner(t, unverified, http.MethodPost, "/api/v1/novels/"+novel.ID+"/bookmark", nil); res.status != http.StatusNoContent {
		t.Errorf("unverified bookmark = %d, want 204. body: %s", res.status, res.body)
	}
	if res := env.asOwner(t, unverified, http.MethodPost, "/api/v1/users/"+novel.Author.ID+"/follow", nil); res.status != http.StatusNoContent {
		t.Errorf("unverified follow = %d, want 204. body: %s", res.status, res.body)
	}
	if res := env.asOwner(t, unverified, http.MethodPut, "/api/v1/novels/"+novel.ID+"/progress",
		map[string]any{"chapter_id": chapter.ID, "progress_percent": 30}); res.status != http.StatusOK {
		t.Errorf("unverified progress = %d, want 200. body: %s", res.status, res.body)
	}
}

func TestLibrary_ShelvesAreIsolatedPerUser(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	alice := env.newWriter(t)
	bob := env.newWriter(t)

	novel := env.publishedNovel(t, author, nil)
	chapter := env.readableChapter(t, author, novel.ID)

	if res := env.asOwner(t, alice, http.MethodPost, "/api/v1/novels/"+novel.ID+"/bookmark", nil); res.status != http.StatusNoContent {
		t.Fatalf("bookmark status = %d. body: %s", res.status, res.body)
	}
	if res := env.asOwner(t, alice, http.MethodPut, "/api/v1/novels/"+novel.ID+"/progress",
		map[string]any{"chapter_id": chapter.ID, "progress_percent": 60}); res.status != http.StatusOK {
		t.Fatalf("save progress status = %d. body: %s", res.status, res.body)
	}

	// Bob sees none of Alice's shelf - every /me read is scoped to its caller
	// (docs/10 §27).
	if _, total := collectionOf[libraryEntryBody](t,
		env.asOwner(t, bob, http.MethodGet, "/api/v1/me/library")); total != 0 {
		t.Errorf("bob's library total = %d, want 0", total)
	}
	if _, total := collectionOf[continueReadingBody](t,
		env.asOwner(t, bob, http.MethodGet, "/api/v1/me/reading-progress")); total != 0 {
		t.Errorf("bob's reading progress total = %d, want 0", total)
	}
	if res := env.asOwner(t, bob, http.MethodGet, "/api/v1/novels/"+novel.ID+"/progress"); res.status != http.StatusNotFound {
		t.Errorf("bob reading alice's progress = %d, want 404", res.status)
	}
}

func TestLibrary_PaginationIsBounded(t *testing.T) {
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	for i := range 3 {
		novel := env.publishedNovel(t, author, map[string]any{
			"title": uniqueName(t, fmt.Sprintf("Shelf %d ", i)),
		})
		if res := env.asOwner(t, reader, http.MethodPost, "/api/v1/novels/"+novel.ID+"/bookmark", nil); res.status != http.StatusNoContent {
			t.Fatalf("bookmark status = %d. body: %s", res.status, res.body)
		}
	}

	entries, total := collectionOf[libraryEntryBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/library?per_page=2"))
	if total != 3 || len(entries) != 2 {
		t.Fatalf("page 1: total = %d entries = %d, want 3 and 2", total, len(entries))
	}

	second, _ := collectionOf[libraryEntryBody](t,
		env.asOwner(t, reader, http.MethodGet, "/api/v1/me/library?per_page=2&page=2"))
	if len(second) != 1 {
		t.Fatalf("page 2 entries = %d, want 1", len(second))
	}
	// Newest first, no overlap between pages.
	if second[0].Novel.ID == entries[0].Novel.ID || second[0].Novel.ID == entries[1].Novel.ID {
		t.Error("page 2 repeats an entry from page 1")
	}
}
