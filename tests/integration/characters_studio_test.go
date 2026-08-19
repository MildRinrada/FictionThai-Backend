package integration

import (
	"net/http"
	"strings"
	"testing"
)

// The cast editor's studio round (Phase 12A follow-up): duplicate names are
// refused, the LIST carries every character's appearances (the closed-card
// count and the timeline both read it), a character portrait uploads under its
// own purpose with content-editor authorization (13U), and a stored avatar
// reference can only ever be a web URL.

// characterBody is the decoded shape of one cast member.
type characterBody struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Role      *string  `json:"role"`
	AvatarURL *string  `json:"avatar_url"`
	AppearsIn []string `json:"appears_in"`
}

func (e *authEnv) createCharacter(
	t *testing.T, w writer, novelID, name string,
) characterBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPost,
		"/api/v1/novels/"+novelID+"/characters", map[string]any{"name": name})
	if res.status != http.StatusCreated {
		t.Fatalf("create character %q status = %d, want 201. body: %s", name, res.status, res.body)
	}
	return dataOf[characterBody](t, res)
}

func TestCharacters_DuplicateNamesAreRefused(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Cast "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	first := env.createCharacter(t, w, novel.ID, "มินตรา")

	// The same name again - including a padded or case-shifted variant - is a
	// field error, not a second indistinguishable cast member.
	for _, dup := range []string{"มินตรา", "  มินตรา  "} {
		res := env.asOwner(t, w, http.MethodPost, path, map[string]any{"name": dup})
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("duplicate %q status = %d, want 422. body: %s", dup, res.status, res.body)
		}
		if !strings.Contains(string(res.body), "name") {
			t.Errorf("the error does not name the field: %s", res.body)
		}
	}
	env.createCharacter(t, w, novel.ID, "Nina")
	if res := env.asOwner(t, w, http.MethodPost, path, map[string]any{"name": "nina"}); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("case-shifted duplicate status = %d, want 422. body: %s", res.status, res.body)
	}

	// A rename INTO an existing name collides the same way...
	second := env.createCharacter(t, w, novel.ID, "วรัญ")
	res := env.asOwner(t, w, http.MethodPatch, path+"/"+second.ID,
		map[string]any{"name": "มินตรา"})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("rename-to-duplicate status = %d, want 422. body: %s", res.status, res.body)
	}
	// ...but a character keeps their own name through an unrelated edit.
	res = env.asOwner(t, w, http.MethodPatch, path+"/"+first.ID,
		map[string]any{"name": "มินตรา", "role": "ตัวเอก"})
	if res.status != http.StatusOK {
		t.Fatalf("same-name self edit status = %d, want 200. body: %s", res.status, res.body)
	}

	// The name is still free in ANOTHER fiction - the scope is one cast.
	other := env.createNovel(t, w, createNovelBody(uniqueName(t, "Other "), nil))
	env.createCharacter(t, w, other.ID, "มินตรา")
}

func TestCharacters_ListCarriesAppearances(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Timeline "), nil))
	path := "/api/v1/novels/" + novel.ID + "/characters"

	chapterOne := env.createChapter(t, w, novel.ID, map[string]any{"content": "หนึ่ง"})
	chapterTwo := env.createChapter(t, w, novel.ID, map[string]any{"content": "สอง"})
	lead := env.createCharacter(t, w, novel.ID, "ตัวเอก")
	extra := env.createCharacter(t, w, novel.ID, "ตัวประกอบ")

	// Recorded out of order on purpose: the API answers in CHAPTER order.
	res := env.asOwner(t, w, http.MethodPut, path+"/"+lead.ID+"/appearances",
		map[string]any{"chapter_ids": []string{chapterTwo.ID, chapterOne.ID}})
	if res.status != http.StatusOK {
		t.Fatalf("set appearances status = %d. body: %s", res.status, res.body)
	}

	list := dataOf[[]characterBody](t, env.asOwner(t, w, http.MethodGet, path))
	if len(list) != 2 {
		t.Fatalf("cast size = %d, want 2", len(list))
	}
	byID := map[string]characterBody{}
	for _, member := range list {
		byID[member.ID] = member
	}
	got := byID[lead.ID].AppearsIn
	if len(got) != 2 || got[0] != chapterOne.ID || got[1] != chapterTwo.ID {
		t.Fatalf("lead appears_in = %v, want [ch1 ch2] in chapter order", got)
	}
	if len(byID[extra.ID].AppearsIn) != 0 {
		t.Fatalf("extra appears_in = %v, want none", byID[extra.ID].AppearsIn)
	}
}

func TestCharacters_AvatarUploadIsContentWork(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	coWriter := env.newWriter(t)
	stranger := writer{webSession: env.registerWeb(t)}

	novel := env.createNovel(t, owner, createNovelBody(uniqueName(t, "Portrait "), nil))
	path := "/api/v1/novels/" + novel.ID

	if res := env.asOwner(t, owner, http.MethodPost, path+"/collaborators",
		map[string]any{"username": coWriter.username}); res.status != http.StatusOK {
		t.Fatalf("add collaborator = %d. body: %s", res.status, res.body)
	}

	// A collaborator uploads a portrait: characters are CONTENT work (13U),
	// so the editor gate - not the owner gate - decides.
	res := env.uploadMedia(t, coWriter, "character_avatar", novel.ID, "หน้า.png", pngBytes)
	if res.status != http.StatusCreated {
		t.Fatalf("collaborator portrait upload status = %d, want 201. body: %s", res.status, res.body)
	}
	portrait := dataOf[mediaBody](t, res)
	if portrait.MediaType != "character_avatar" ||
		!strings.Contains(portrait.URL, "/media/character_avatar/") {
		t.Fatalf("portrait view = %+v", portrait)
	}

	// The URL lands on the character through the characters PATCH - the same
	// attach-later contract entry and chapter images use.
	member := env.createCharacter(t, owner, novel.ID, "คนมีรูป")
	res = env.asOwner(t, coWriter, http.MethodPatch,
		path+"/characters/"+member.ID, map[string]any{"avatar_url": portrait.URL})
	if res.status != http.StatusOK {
		t.Fatalf("attach portrait status = %d. body: %s", res.status, res.body)
	}
	updated := dataOf[characterBody](t, res)
	if updated.AvatarURL == nil || *updated.AvatarURL != portrait.URL {
		t.Fatalf("avatar_url = %v, want %q", updated.AvatarURL, portrait.URL)
	}

	// A stranger gets the reader-identical 404 for the private fiction -
	// before any byte is stored.
	res = env.uploadMedia(t, stranger, "character_avatar", novel.ID, "evil.png", pngBytes)
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger portrait upload status = %d, want 404", res.status)
	}
	// And without a fiction the purpose is unusable at all.
	res = env.uploadMedia(t, owner, "character_avatar", "", "x.png", pngBytes)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("portrait without novel status = %d, want 422", res.status)
	}
}

func TestCharacters_AvatarReferenceMustBeAWebURL(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "AvatarURL "), nil))
	member := env.createCharacter(t, w, novel.ID, "คนถูกทดสอบ")
	path := "/api/v1/novels/" + novel.ID + "/characters/" + member.ID

	// The value is rendered as an <img src> on reader pages, so only http(s)
	// passes - a script or data reference is refused before it is stored.
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,x", "ftp://host/a.png", "not a url"} {
		res := env.asOwner(t, w, http.MethodPatch, path, map[string]any{"avatar_url": bad})
		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("avatar_url %q status = %d, want 422. body: %s", bad, res.status, res.body)
		}
	}

	res := env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"avatar_url": "https://example.com/a.png"})
	if res.status != http.StatusOK {
		t.Fatalf("https avatar status = %d. body: %s", res.status, res.body)
	}
	// Clearing stays a clear, not a validation target.
	res = env.asOwner(t, w, http.MethodPatch, path, map[string]any{"avatar_url": nil})
	if res.status != http.StatusOK {
		t.Fatalf("clear avatar status = %d. body: %s", res.status, res.body)
	}
	if cleared := dataOf[characterBody](t, res); cleared.AvatarURL != nil {
		t.Fatalf("avatar_url after clear = %v, want nil", cleared.AvatarURL)
	}
}
