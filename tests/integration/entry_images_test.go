package integration

import (
	"net/http"
	"strings"
	"testing"
)

// Phase 13M - a picture on a headcanon entry
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13M, extends 12F).
//
// The image is a fourth, OPTIONAL thing on an entry, and these tests are the
// two halves of that claim: it survives every round trip the other three
// representations already survive, and it never becomes required. The upload
// itself is authorized against the fiction before a byte is stored, which is
// the property that keeps a stranger from making the platform write objects.

// A picture attaches, comes back, and survives a format change - the same
// guarantee prose, messages, and the entry body already have.
func TestEntryImage_AttachesAndSurvivesFormatChanges(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, map[string]any{
		"presentation_format": "headcanon",
		"content_mode":        "headcanon",
	})

	// The bytes go through the real media endpoint, so what lands on the entry
	// is the URL the API minted rather than a string this test invented.
	res := env.uploadMedia(t, w, "entry_image", novel.ID, "arin.png", pngBytes)
	if res.status != http.StatusCreated {
		t.Fatalf("entry_image upload status = %d, want 201. body: %s", res.status, res.body)
	}
	uploaded := dataOf[mediaBody](t, res)
	if uploaded.MediaType != "entry_image" {
		t.Fatalf("media_type = %q, want entry_image", uploaded.MediaType)
	}
	if !strings.Contains(uploaded.URL, "/media/entry_image/") {
		t.Fatalf("serve URL = %q, want it under the entry_image prefix", uploaded.URL)
	}

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "เปอร์เซ็นต์ที่จีบติด",
		"entries": []map[string]any{
			{"name": "อาริน", "body": "อบอุ่นและเป็นมิตร", "image_url": uploaded.URL},
			{"name": "เธียร", "body": "พกความเงียบติดตัว"},
		},
		"entry_fields": []string{"เปอร์เซ็นต์ที่จีบติด"},
	})

	if len(chapter.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(chapter.Entries))
	}
	if chapter.Entries[0].ImageURL == nil || *chapter.Entries[0].ImageURL != uploaded.URL {
		t.Fatalf("first entry image = %v, want the uploaded URL", chapter.Entries[0].ImageURL)
	}
	// An entry without one is complete, not incomplete.
	if chapter.Entries[1].ImageURL != nil {
		t.Fatalf("second entry gained an image it never had: %v", chapter.Entries[1].ImageURL)
	}

	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	// The picture is content, so it survives the trips a chapter can still
	// take: an edit of the topic beside it, and a FICTION-level format change
	// (a chapter's own mode is fixed at creation since 13P).
	for _, edit := range []map[string]any{
		{"title": "ชื่อใหม่"},
		{"entry_fields": []string{"เปอร์เซ็นต์", "ราศี"}},
	} {
		if r := env.asOwner(t, w, http.MethodPatch, path, edit); r.status != http.StatusOK {
			t.Fatalf("edit %v status = %d. body: %s", edit, r.status, r.body)
		}
	}
	if r := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID+"/format",
		map[string]any{"presentation_format": "chat", "content_mode": "general"},
	); r.status != http.StatusOK {
		t.Fatalf("fiction format change status = %d. body: %s", r.status, r.body)
	}

	got := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, path))
	if len(got.Entries) != 2 {
		t.Fatalf("entries lost: %d", len(got.Entries))
	}
	if got.Entries[0].ImageURL == nil || *got.Entries[0].ImageURL != uploaded.URL {
		t.Fatalf("image lost: %v", got.Entries[0].ImageURL)
	}

	// And the file itself is still served - none of that touches a byte.
	if file := env.fetchFile(t, servePath(t, uploaded.URL)); file.status != http.StatusOK {
		t.Fatalf("uploaded entry image status = %d, want 200", file.status)
	}
}

// Removing the picture is an edit of the ENTRY, not a deletion of the file. An
// author who reverts to an earlier revision has to find the image still there
// (docs/CONTENT-MODEL.md §5).
func TestEntryImage_RemovingTheReferenceKeepsTheFile(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, map[string]any{"presentation_format": "headcanon"})
	uploaded := dataOf[mediaBody](t,
		env.uploadMedia(t, w, "entry_image", novel.ID, "arin.png", pngBytes))

	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"entries": []map[string]any{
			{"name": "อาริน", "body": "อบอุ่น", "image_url": uploaded.URL},
		},
	})
	path := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	res := env.asOwner(t, w, http.MethodPatch, path, map[string]any{
		"entries": []map[string]any{
			{"name": "อาริน", "body": "อบอุ่น", "image_url": ""},
		},
	})
	if res.status != http.StatusOK {
		t.Fatalf("clear image status = %d. body: %s", res.status, res.body)
	}

	got := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, path))
	if len(got.Entries) != 1 || got.Entries[0].ImageURL != nil {
		t.Fatalf("image reference survived the clear: %+v", got.Entries)
	}
	// The author's paragraph is untouched by removing a picture beside it.
	if got.Entries[0].Body != "อบอุ่น" {
		t.Fatalf("body changed when the image was removed: %q", got.Entries[0].Body)
	}
	if file := env.fetchFile(t, servePath(t, uploaded.URL)); file.status != http.StatusOK {
		t.Fatalf("file status after clearing the reference = %d, want 200", file.status)
	}
}

// The upload is authorized against the FICTION before any byte is stored, by
// the novels service's own ownership rule - so a stranger cannot make the
// platform write objects, and a private fiction stays unconfirmed (docs/11 §21).
func TestEntryImage_UploadRefusesSomeoneElsesFiction(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	stranger := env.newWriter(t)

	novel := env.publishedNovel(t, owner, map[string]any{"presentation_format": "headcanon"})

	res := env.uploadMedia(t, stranger, "entry_image", novel.ID, "x.png", pngBytes)
	if res.status != http.StatusForbidden && res.status != http.StatusNotFound {
		t.Fatalf("stranger upload status = %d, want 403 or 404. body: %s", res.status, res.body)
	}

	// And the purpose is not a way around the fiction reference either.
	missing := env.uploadMedia(t, stranger, "entry_image", "", "x.png", pngBytes)
	if missing.status != http.StatusUnprocessableEntity {
		t.Fatalf("upload without a novel status = %d, want 422. body: %s",
			missing.status, missing.body)
	}
}
