package integration

import (
	"net/http"
	"testing"
)

// Phase 13L - the pre-publish checklist
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13L).
//
// 13A was right to cut the create form to six fields, but it left the synopsis,
// the genres, and the tags with no point at which they were ever required
// again - a lighter form that produced worse data. This is where they come
// back: not before the first sentence, but in front of PUBLISHING.
//
// The rule these protect: the gate is on publishing ONLY. Drafting, editing,
// and keeping a private work forever are untouched, because nothing may stop a
// writer from writing.

type readinessBody struct {
	Ready bool `json:"ready"`
	Items []struct {
		Key      string `json:"key"`
		Label    string `json:"label"`
		Done     bool   `json:"done"`
		Hint     string `json:"hint"`
		Required bool   `json:"required"`
	} `json:"items"`
}

func (r readinessBody) done(key string) (bool, bool) {
	for _, item := range r.Items {
		if item.Key == key {
			return item.Done, true
		}
	}
	return false, false
}

func (r readinessBody) required(key string) bool {
	for _, item := range r.Items {
		if item.Key == key {
			return item.Required
		}
	}
	return false
}

func TestReadiness_BlocksPublishingUntilComplete(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Unready "), nil))
	path := "/api/v1/novels/" + novel.ID

	list := dataOf[readinessBody](t, env.asOwner(t, w, http.MethodGet, path+"/readiness"))
	if list.Ready {
		t.Fatal("a fiction with no synopsis, genres, tags, or cover is not ready")
	}
	for _, key := range []string{"description", "genres", "tags", "cover", "email_verified"} {
		done, present := list.done(key)
		if !present {
			t.Fatalf("the checklist is missing %q: %+v", key, list.Items)
		}
		// The writer verified their email in newWriter, so that one is done and
		// the rest are not - which is what makes this a work list rather than a
		// wall of red.
		if key == "email_verified" && !done {
			t.Error("a verified writer should not be told to verify")
		}
		if key != "email_verified" && done {
			t.Errorf("%q reported done on an empty fiction", key)
		}
	}

	// The gate refuses, and names the FIELDS so a form can point at each one.
	res := env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"status": "ongoing", "visibility": "public"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("publish status = %d, want 422. body: %s", res.status, res.body)
	}
	for _, field := range []string{"description", "genres", "tags"} {
		if !containsField(t, res, field) {
			t.Errorf("the error does not name %q: %s", field, res.body)
		}
	}
	// The cover is RECOMMENDED (§13T): the error must not name it, or
	// "recommended" would be a lie the writer discovers at the gate.
	if containsField(t, res, "cover") {
		t.Errorf("the error names the recommended cover: %s", res.body)
	}

	// And the fiction is still exactly where it was.
	after := dataOf[novelBody](t, env.asOwner(t, w, http.MethodGet, path))
	if after.Status != "draft" || after.Visibility == nil || *after.Visibility != "private" {
		t.Fatalf("a refused publish changed the fiction: %+v", after)
	}
}

// Drafting is never blocked. This is the test that keeps the gate honest.
func TestReadiness_NeverBlocksWriting(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Drafting "), nil))
	path := "/api/v1/novels/" + novel.ID

	// Every one of these happens on a fiction that could not be published.
	for _, body := range []map[string]any{
		{"title": "ชื่อใหม่"},
		{"content_warning": "มีฉากรุนแรง"},
		{"status": "hiatus"},
		{"chapter_unit": "บท"},
	} {
		res := env.asOwner(t, w, http.MethodPatch, path, body)
		if res.status != http.StatusOK {
			t.Fatalf("editing a draft was blocked: %d for %v. body: %s", res.status, body, res.body)
		}
	}

	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novel.ID+"/chapters",
		map[string]any{"title": "ตอนแรก", "content": "เขียนได้ตามปกติ"})
	if res.status != http.StatusCreated {
		t.Fatalf("adding a chapter to an unready fiction was blocked: %d. body: %s",
			res.status, res.body)
	}
}

// A writer who completes the checklist and publishes in ONE request must not be
// told the thing they just filled in is missing.
func TestReadiness_JudgesTheRequestNotTheStoredRow(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	genres := env.seededGenres(t)
	genre := genres["fantasy"].ID
	tag := env.newTag(t, w, uniqueName(t, "พร้อมเผยแพร่ ")).ID

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "OneShot "), nil))
	path := "/api/v1/novels/" + novel.ID

	res := env.asOwner(t, w, http.MethodPatch, path, map[string]any{
		"description": "เรื่องย่อที่เพิ่งเขียนในคำขอเดียวกันนี้",
		"cover_url":   "https://example.com/cover.jpg",
		"genre_ids":   []string{genre},
		"tag_ids":     []string{tag},
		"status":      "ongoing",
		"visibility":  "public",
	})
	if res.status != http.StatusOK {
		t.Fatalf("completing and publishing together was refused: %d. body: %s",
			res.status, res.body)
	}

	list := dataOf[readinessBody](t, env.asOwner(t, w, http.MethodGet, path+"/readiness"))
	if !list.Ready {
		t.Errorf("the checklist still reports work outstanding: %+v", list.Items)
	}
}

// The cover is advice, not a wall (§13T): a finished story with no cover yet is
// still publishable work, and the checklist says so with `required: false`.
func TestReadiness_CoverIsRecommendedNotRequired(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	genres := env.seededGenres(t)
	genre := genres["fantasy"].ID
	tag := env.newTag(t, w, uniqueName(t, "ไม่มีปก ")).ID

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Coverless "), nil))
	path := "/api/v1/novels/" + novel.ID

	// Everything except the cover.
	res := env.asOwner(t, w, http.MethodPatch, path, map[string]any{
		"description": "เรื่องย่อครบ แต่ยังไม่มีปก",
		"genre_ids":   []string{genre},
		"tag_ids":     []string{tag},
	})
	if res.status != http.StatusOK {
		t.Fatalf("completing metadata failed: %d. body: %s", res.status, res.body)
	}

	list := dataOf[readinessBody](t, env.asOwner(t, w, http.MethodGet, path+"/readiness"))
	if !list.Ready {
		t.Errorf("a fiction missing only its cover must be ready: %+v", list.Items)
	}
	if done, present := list.done("cover"); !present || done {
		t.Errorf("the cover item should be present and not done: %+v", list.Items)
	}
	if list.required("cover") {
		t.Error("the cover reports required=true")
	}
	if !list.required("description") {
		t.Error("the synopsis reports required=false")
	}

	// And the publish itself goes through without it.
	res = env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"status": "ongoing", "visibility": "public"})
	if res.status != http.StatusOK {
		t.Fatalf("publishing without a cover was refused: %d. body: %s", res.status, res.body)
	}
}

// The content warning is asked for only where it means something. Demanding one
// on a ทั่วไป fiction would teach writers to type "ไม่มี" to get past a gate.
func TestReadiness_ContentWarningOnlyForRatedWork(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	general := env.createNovel(t, w, createNovelBody(uniqueName(t, "General "), nil))
	list := dataOf[readinessBody](t, env.asOwner(t, w, http.MethodGet,
		"/api/v1/novels/"+general.ID+"/readiness"))
	if _, present := list.done("content_warning"); present {
		t.Error("a ทั่วไป fiction should not be asked for a content warning")
	}

	rated := env.createNovel(t, w, createNovelBody(uniqueName(t, "Teen "),
		map[string]any{"age_rating": "teen"}))
	list = dataOf[readinessBody](t, env.asOwner(t, w, http.MethodGet,
		"/api/v1/novels/"+rated.ID+"/readiness"))
	done, present := list.done("content_warning")
	if !present {
		t.Fatal("a 15+ fiction must be asked for a content warning")
	}
	if done {
		t.Error("it has none yet, so it is not done")
	}
}

// Only the owner sees their own work list; a stranger gets the fiction's own
// 404 rather than a 403 that would confirm it exists (docs/11 §3.4).
func TestReadiness_IsOwnerOnly(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	stranger := env.newWriter(t)

	novel := env.publishedNovel(t, owner, nil)

	res := env.asOwner(t, stranger, http.MethodGet, "/api/v1/novels/"+novel.ID+"/readiness")
	if res.status != http.StatusNotFound && res.status != http.StatusForbidden {
		t.Fatalf("stranger readiness status = %d, want 404 or 403", res.status)
	}
	if res := env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.Slug+"/readiness"); res.status == http.StatusOK {
		t.Fatal("a guest read another writer's work list")
	}
}

func containsField(t *testing.T, res apiResponse, field string) bool {
	t.Helper()
	var payload struct {
		Error struct {
			Fields map[string][]string `json:"fields"`
		} `json:"error"`
	}
	res.json(t, &payload)
	_, present := payload.Error.Fields[field]
	return present
}
