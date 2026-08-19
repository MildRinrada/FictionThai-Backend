package integration

import (
	"net/http"
	"testing"
	"time"
)

// Phase 13U - co-writers, display choices, and the scheduled first publish.
//
// The boundary these tests defend: a collaborator EDITS CONTENT - chapters,
// the shared studio - and nothing else. Ownership actions (settings,
// visibility, deleting, the collaborator list itself) stay with the author,
// and a scheduled fiction stays invisible until its moment.

type collaboratorsBody struct {
	Collaborators []struct {
		Username    string  `json:"username"`
		DisplayName *string `json:"display_name"`
		Credit      string  `json:"credit"`
	} `json:"collaborators"`
}

func TestCollaborators_CoWriterEditsChaptersButNotSettings(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	coWriter := env.newWriter(t)

	novel := env.createNovel(t, owner, createNovelBody(uniqueName(t, "CoWritten "), nil))
	path := "/api/v1/novels/" + novel.ID

	// Before being added: the private draft answers 404, exactly as it does
	// for any stranger.
	if res := env.asOwner(t, coWriter, http.MethodGet, path); res.status != http.StatusNotFound {
		t.Fatalf("a stranger reads a private draft: %d", res.status)
	}

	// The owner adds them by username.
	res := env.asOwner(t, owner, http.MethodPost, path+"/collaborators",
		map[string]any{"username": coWriter.username, "credit": "ร่วมเขียน"})
	if res.status != http.StatusOK {
		t.Fatalf("add collaborator = %d. body: %s", res.status, res.body)
	}
	list := dataOf[collaboratorsBody](t, res)
	if len(list.Collaborators) != 1 || list.Collaborators[0].Username != coWriter.username {
		t.Fatalf("collaborator list = %+v", list.Collaborators)
	}

	// Now the co-writer opens the fiction - including the working view.
	view := dataOf[novelBody](t, env.asOwner(t, coWriter, http.MethodGet, path))
	if view.IsOwner {
		t.Error("a collaborator must not be reported as the owner")
	}
	if !view.CanEdit {
		t.Error("a collaborator must get can_edit")
	}

	// And WRITES a chapter.
	res = env.asOwner(t, coWriter, http.MethodPost, path+"/chapters",
		map[string]any{"title": "ตอนของผู้เขียนร่วม", "content": "เขียนโดยคนที่สอง"})
	if res.status != http.StatusCreated {
		t.Fatalf("a collaborator cannot write a chapter: %d. body: %s", res.status, res.body)
	}

	// But not the fiction's settings, its visibility, or its life.
	if res := env.asOwner(t, coWriter, http.MethodPatch, path,
		map[string]any{"title": "ยึดเรื่อง"}); res.status != http.StatusForbidden {
		t.Errorf("a collaborator changed the fiction's settings: %d", res.status)
	}
	if res := env.asOwner(t, coWriter, http.MethodDelete, path); res.status != http.StatusForbidden {
		t.Errorf("a collaborator deleted the fiction: %d", res.status)
	}
	if res := env.asOwner(t, coWriter, http.MethodPost, path+"/collaborators",
		map[string]any{"username": owner.username}); res.status != http.StatusForbidden {
		t.Errorf("a collaborator edited the collaborator list: %d", res.status)
	}

	// Their shelf lists the co-written fiction, drafts included.
	shelf, _ := collectionOf[novelBody](t,
		env.asOwner(t, coWriter, http.MethodGet, "/api/v1/novels?co_writer=me"))
	var found bool
	for _, item := range shelf {
		if item.ID == novel.ID {
			found = true
		}
	}
	if !found {
		t.Error("the co-writer shelf does not list the co-written draft")
	}

	// Removal ends access - and destroys nothing.
	if res := env.asOwner(t, owner, http.MethodDelete,
		path+"/collaborators/"+coWriter.username); res.status != http.StatusNoContent {
		t.Fatalf("remove collaborator = %d", res.status)
	}
	if res := env.asOwner(t, coWriter, http.MethodGet, path); res.status != http.StatusNotFound {
		t.Errorf("a removed collaborator still reads the private draft: %d", res.status)
	}
	chapters, _ := collectionOf[chapterBody](t,
		env.asOwner(t, owner, http.MethodGet, path+"/chapters"))
	var kept bool
	for _, chapter := range chapters {
		if chapter.Title != nil && *chapter.Title == "ตอนของผู้เขียนร่วม" {
			kept = true
		}
	}
	if !kept {
		t.Error("removing a collaborator lost the chapter they wrote")
	}
}

func TestCollaborators_UnknownUsernameIsAFieldError(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	novel := env.createNovel(t, owner, createNovelBody(uniqueName(t, "Solo "), nil))

	res := env.asOwner(t, owner, http.MethodPost,
		"/api/v1/novels/"+novel.ID+"/collaborators",
		map[string]any{"username": "no-such-user-ever"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown username = %d, want 422. body: %s", res.status, res.body)
	}
	if !containsField(t, res, "username") {
		t.Errorf("the error does not name the username field: %s", res.body)
	}
}

// ซ่อนตัวเลข (13U): the numbers must not leave the server for a reader.
func TestDisplay_HiddenCountsAreZeroedForReaders(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	reader := env.newWriter(t)
	novel := env.publishedNovel(t, owner, nil)

	res := env.asOwner(t, owner, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"hide_counts": true})
	if res.status != http.StatusOK {
		t.Fatalf("set hide_counts = %d. body: %s", res.status, res.body)
	}

	got := dataOf[novelBody](t, env.asOwner(t, reader, http.MethodGet,
		"/api/v1/novels/"+novel.Slug))
	if !got.CountsHidden {
		t.Error("counts_hidden not reported to the reader")
	}
	if got.ViewCount != 0 || got.LikeCount != 0 || got.BookmarkCount != 0 {
		t.Errorf("hidden counts leaked: views=%d likes=%d bookmarks=%d",
			got.ViewCount, got.LikeCount, got.BookmarkCount)
	}
}

// ตั้งเวลาเผยแพร่ (13U): scheduled means invisible until the moment.
func TestSchedule_AScheduledFictionStaysInvisibleUntilItsTime(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)

	genres := env.seededGenres(t)
	genre := genres["fantasy"].ID
	tag := env.newTag(t, owner, uniqueName(t, "ตั้งเวลา ")).ID
	novel := env.createNovel(t, owner, createNovelBody(uniqueName(t, "Scheduled "), nil))
	path := "/api/v1/novels/" + novel.ID

	// Complete the checklist, then publish WITH a future time.
	res := env.asOwner(t, owner, http.MethodPatch, path, map[string]any{
		"description": "เรื่องย่อครบ",
		"genre_ids":   []string{genre},
		"tag_ids":     []string{tag},
	})
	if res.status != http.StatusOK {
		t.Fatalf("complete metadata = %d. body: %s", res.status, res.body)
	}

	when := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	res = env.asOwner(t, owner, http.MethodPatch, path, map[string]any{
		"status": "ongoing", "visibility": "public", "publish_at": when,
	})
	if res.status != http.StatusOK {
		t.Fatalf("schedule publish = %d. body: %s", res.status, res.body)
	}

	// Before its time: readers get the same 404 a private draft gets, and it
	// appears in no listing.
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug); res.status != http.StatusNotFound {
		t.Errorf("a scheduled fiction is readable early: %d", res.status)
	}
	listed, _ := collectionOf[novelBody](t, env.asGuest(t, http.MethodGet, "/api/v1/novels"))
	for _, item := range listed {
		if item.ID == novel.ID {
			t.Error("a scheduled fiction appears in the public listing early")
		}
	}

	// The owner sees the pending schedule on their own view.
	own := dataOf[novelBody](t, env.asOwner(t, owner, http.MethodGet, path))
	if own.PublishAt == nil {
		t.Error("the owner's view does not carry publish_at")
	}

	// A past time is refused - that is what the ordinary publish is for.
	res = env.asOwner(t, owner, http.MethodPatch, path, map[string]any{
		"publish_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("a past publish_at was accepted: %d", res.status)
	}

	// Cancelling (null + private) makes it a private work again.
	res = env.asOwner(t, owner, http.MethodPatch, path, map[string]any{
		"publish_at": nil, "visibility": "private",
	})
	if res.status != http.StatusOK {
		t.Fatalf("cancel schedule = %d. body: %s", res.status, res.body)
	}
	after := dataOf[novelBody](t, env.asOwner(t, owner, http.MethodGet, path))
	if after.PublishAt != nil {
		t.Error("cancelling did not clear publish_at")
	}
}

// The gate that guards a plain publish guards a scheduled one identically.
func TestSchedule_RunsTheReadinessGateAtSchedulingTime(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	novel := env.createNovel(t, owner, createNovelBody(uniqueName(t, "NotReady "), nil))

	when := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	res := env.asOwner(t, owner, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"status": "ongoing", "visibility": "public", "publish_at": when})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("an unready fiction was scheduled: %d. body: %s", res.status, res.body)
	}
}
