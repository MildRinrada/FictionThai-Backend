package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Pen names (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2).
//
// Three things are being defended here, in descending order of how much damage
// getting them wrong would do:
//
//  1. DELETING AN IDENTITY DELETES NO WORK. `novels.pen_name_id` is
//     ON DELETE SET NULL, and this suite proves the fiction and every chapter
//     survive a pen name being removed. CLAUDE.md makes this a hard rule:
//     never silently modify or delete writer content.
//  2. A rename is VISIBLE for thirty days - the platform's whole answer to
//     someone taking over a name - and it records the previous name, not the
//     new one.
//  3. The endpoints are self-scoped. Another account's pen name is unreachable
//     and indistinguishable from one that never existed; a guest gets 401.

type penNameBody struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Note      *string `json:"note"`
	IsDefault bool    `json:"is_default"`
}

// penProfileBody and penNovelBody decode only the fields this suite asserts on.
//
// Deliberately local rather than extra fields on the shared profileBody and
// novelBody: a decoder that names what its own test needs cannot break a
// neighbouring suite, and the shared fixtures stay the shape their own tests
// describe.
type penProfileBody struct {
	Username    string        `json:"username"`
	PenNames    []penNameBody `json:"pen_names"`
	FormerNames []string      `json:"former_names"`
}

type penNovelBody struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	PenName   *string `json:"pen_name"`
	PenNameID *string `json:"pen_name_id"`
}

func (e *authEnv) penNames(t *testing.T, w writer) []penNameBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/me/pen-names")
	if res.status != http.StatusOK {
		t.Fatalf("list pen names status = %d. body: %s", res.status, res.body)
	}
	return dataOf[[]penNameBody](t, res)
}

func (e *authEnv) createPenName(t *testing.T, w writer, body map[string]any) penNameBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPost, "/api/v1/me/pen-names", body)
	if res.status != http.StatusCreated {
		t.Fatalf("create pen name status = %d, want 201. body: %s", res.status, res.body)
	}
	return dataOf[penNameBody](t, res)
}

// The ordinary life of the feature: add, label, rename, re-default, remove.
func TestPenNames_WriterManagesTheirOwnIdentities(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)

	// The FIRST identity becomes the default on its own. A writer with one pen
	// name and no default would have asked for a name and been given none.
	first := env.createPenName(t, author, map[string]any{
		"name": "ณัฐวรา", "note": "แนวหลัก",
	})
	if !first.IsDefault {
		t.Fatal("the first pen name did not become the default")
	}
	if first.Note == nil || *first.Note != "แนวหลัก" {
		t.Fatalf("note not saved: %+v", first.Note)
	}

	second := env.createPenName(t, author, map[string]any{"name": "N.W."})
	if second.IsDefault {
		t.Fatal("a second pen name took the default away without being asked")
	}

	// A duplicate is refused as a field error, case-insensitively - two names
	// the writer cannot tell apart in their own list are not two names.
	dup := env.asOwner(t, author, http.MethodPost, "/api/v1/me/pen-names",
		map[string]any{"name": "n.w."})
	if dup.status != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate pen name status = %d, want 422. body: %s", dup.status, dup.body)
	}

	// Renaming.
	renamed := env.asOwner(t, author, http.MethodPatch,
		"/api/v1/me/pen-names/"+second.ID, map[string]any{"name": "นวรา"})
	if renamed.status != http.StatusOK {
		t.Fatalf("rename status = %d. body: %s", renamed.status, renamed.body)
	}
	if got := dataOf[penNameBody](t, renamed).Name; got != "นวรา" {
		t.Fatalf("renamed to %q, want นวรา", got)
	}

	// Making one the default takes it off the other, in one step - two defaults
	// would leave "whose name is on this work" ambiguous.
	if res := env.asOwner(t, author, http.MethodPatch,
		"/api/v1/me/pen-names/"+second.ID,
		map[string]any{"is_default": true}); res.status != http.StatusOK {
		t.Fatalf("set default status = %d. body: %s", res.status, res.body)
	}
	list := env.penNames(t, author)
	if len(list) != 2 {
		t.Fatalf("pen name count = %d, want 2", len(list))
	}
	defaults := 0
	for _, item := range list {
		if item.IsDefault {
			defaults++
			if item.ID != second.ID {
				t.Fatalf("the default is %q, want the one just set", item.Name)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("%d pen names claim to be the default", defaults)
	}
	// The default sorts first, so a client renders the list without sorting it.
	if list[0].ID != second.ID {
		t.Fatalf("the default is not first in the list: %+v", list)
	}

	// An emptied note is a deliberate clear.
	cleared := env.asOwner(t, author, http.MethodPatch,
		"/api/v1/me/pen-names/"+first.ID, map[string]any{"note": nil})
	if cleared.status != http.StatusOK {
		t.Fatalf("clear note status = %d. body: %s", cleared.status, cleared.body)
	}
	if note := dataOf[penNameBody](t, cleared).Note; note != nil {
		t.Fatalf("note not cleared: %+v", note)
	}

	// A nameless pen name is a field error, not a row.
	if res := env.asOwner(t, author, http.MethodPost, "/api/v1/me/pen-names",
		map[string]any{"name": "   "}); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("empty name status = %d, want 422. body: %s", res.status, res.body)
	}

	if res := env.asOwner(t, author, http.MethodDelete,
		"/api/v1/me/pen-names/"+first.ID); res.status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204. body: %s", res.status, res.body)
	}
	if remaining := env.penNames(t, author); len(remaining) != 1 {
		t.Fatalf("after deleting one, %d remain", len(remaining))
	}
}

// The rule this feature is not allowed to break: removing an identity removes
// NO WORK. The fiction, its chapters, and every word in them survive; the work
// simply falls back to the writer's default name.
func TestPenNames_DeletingOneLeavesTheWorkIntact(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	fallback := env.createPenName(t, author, map[string]any{"name": uniqueName(t, "หลัก")})
	retired := env.createPenName(t, author, map[string]any{"name": uniqueName(t, "แยก")})

	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title":   "ตอนที่หนึ่ง",
		"content": "เนื้อความที่ต้องรอดจากการลบนามปากกา",
	})
	env.publishChapter(t, author, novel.ID, chapter.ID)

	// Publish the work under the identity that is about to be removed.
	attached := env.asOwner(t, author, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"pen_name_id": retired.ID})
	if attached.status != http.StatusOK {
		t.Fatalf("attach pen name status = %d. body: %s", attached.status, attached.body)
	}
	withPen := dataOf[penNovelBody](t, attached)
	if withPen.PenName == nil || *withPen.PenName != retired.Name {
		t.Fatalf("the work does not carry its chosen pen name: %+v", withPen.PenName)
	}
	if withPen.PenNameID == nil || *withPen.PenNameID != retired.ID {
		t.Fatalf("the owner view does not echo the choice: %+v", withPen.PenNameID)
	}

	// Attaching a pen name must not have touched the content.
	beforeChapters, beforeTotal := collectionOf[chapterBody](t,
		env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID+"/chapters"))

	if res := env.asOwner(t, author, http.MethodDelete,
		"/api/v1/me/pen-names/"+retired.ID); res.status != http.StatusNoContent {
		t.Fatalf("delete pen name status = %d, want 204. body: %s", res.status, res.body)
	}

	// The fiction is still there, still readable, still the same fiction.
	res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID)
	if res.status != http.StatusOK {
		t.Fatalf("the fiction became unreadable after a pen name was deleted: %d", res.status)
	}
	after := dataOf[penNovelBody](t, res)
	if after.ID != novel.ID || after.Title != novel.Title {
		t.Fatalf("the fiction changed: %+v", after)
	}
	// It falls back to the writer's default rather than losing its cover name.
	if after.PenName == nil || *after.PenName != fallback.Name {
		t.Fatalf("after the delete the work shows %+v, want the default %q",
			after.PenName, fallback.Name)
	}

	// And every chapter, with every word, is exactly where it was.
	afterChapters, afterTotal := collectionOf[chapterBody](t,
		env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.ID+"/chapters"))
	if afterTotal != beforeTotal || len(afterChapters) != len(beforeChapters) {
		t.Fatalf("chapter count went from %d to %d", beforeTotal, afterTotal)
	}
	read := env.asGuest(t, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID)
	if read.status != http.StatusOK {
		t.Fatalf("chapter read status = %d after the pen name was deleted", read.status)
	}
	body := dataOf[chapterBody](t, read)
	if body.Content == nil || *body.Content != "เนื้อความที่ต้องรอดจากการลบนามปากกา" {
		t.Fatalf("the chapter's text changed: %+v", body.Content)
	}
}

// A rename is visible as «เคยใช้ชื่อ …» on the public profile, and the record
// is of the PREVIOUS name - the new one is already on the page.
func TestPenNames_ARenameShowsAsAFormerNameOnTheProfile(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	name := env.currentUser(t, author).Username

	before := uniqueName(t, "ชื่อเดิม")
	pen := env.createPenName(t, author, map[string]any{"name": before})

	fresh := dataOf[penProfileBody](t, env.publicProfile(t, name))
	if len(fresh.FormerNames) != 0 {
		t.Fatalf("a profile with no rename already lists former names: %+v", fresh.FormerNames)
	}
	if len(fresh.PenNames) != 1 || fresh.PenNames[0].Name != before {
		t.Fatalf("the profile does not publish the writer's identities: %+v", fresh.PenNames)
	}
	if !fresh.PenNames[0].IsDefault {
		t.Fatal("the ค่าเริ่มต้น chip is missing from the only pen name")
	}

	after := uniqueName(t, "ชื่อใหม่")
	if res := env.asOwner(t, author, http.MethodPatch, "/api/v1/me/pen-names/"+pen.ID,
		map[string]any{"name": after}); res.status != http.StatusOK {
		t.Fatalf("rename status = %d. body: %s", res.status, res.body)
	}

	renamed := dataOf[penProfileBody](t, env.publicProfile(t, name))
	if len(renamed.PenNames) != 1 || renamed.PenNames[0].Name != after {
		t.Fatalf("the profile still shows the old identity: %+v", renamed.PenNames)
	}
	if len(renamed.FormerNames) != 1 || renamed.FormerNames[0] != before {
		t.Fatalf("former_names = %+v, want [%q] - the PREVIOUS name",
			renamed.FormerNames, before)
	}

	// Taking the name back removes it from the line: renaming A → B → A must
	// not tell readers this person "used to be" the name they are using now.
	if res := env.asOwner(t, author, http.MethodPatch, "/api/v1/me/pen-names/"+pen.ID,
		map[string]any{"name": before}); res.status != http.StatusOK {
		t.Fatalf("rename back status = %d. body: %s", res.status, res.body)
	}
	back := dataOf[penProfileBody](t, env.publicProfile(t, name))
	for _, former := range back.FormerNames {
		if former == before {
			t.Fatalf("a name the writer uses again is still listed as former: %+v",
				back.FormerNames)
		}
	}

	// The read stays viewer-independent: the person themselves gets the same
	// bytes a guest gets, which is what keeps it cacheable (docs/14 §7).
	asGuest := env.publicProfile(t, name)
	asSelf := env.asOwner(t, author, http.MethodGet, "/api/v1/users/"+name)
	if string(asSelf.body) != string(asGuest.body) {
		t.Fatalf("the profile differs for its owner:\n%s\n%s", asGuest.body, asSelf.body)
	}
}

// Every endpoint is self-scoped. Another account's pen name is unreachable and
// indistinguishable from one that never existed, and a guest has none at all.
func TestPenNames_AreUnreachableFromAnotherAccount(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	stranger := env.newWriter(t)

	mine := env.createPenName(t, author, map[string]any{"name": uniqueName(t, "ของฉัน")})

	// A stranger sees none of it in their own list.
	if list := env.penNames(t, stranger); len(list) != 0 {
		t.Fatalf("a stranger's list contains %d pen names", len(list))
	}

	path := "/api/v1/me/pen-names/" + mine.ID
	for _, attempt := range []struct {
		method string
		body   any
	}{
		{http.MethodPatch, map[string]any{"name": "ยึดชื่อ"}},
		{http.MethodPatch, map[string]any{"is_default": true}},
		{http.MethodDelete, nil},
	} {
		res := env.asOwner(t, stranger, attempt.method, path, attempt.body)
		if res.status != http.StatusNotFound {
			t.Fatalf("%s by a stranger = %d, want 404. body: %s",
				attempt.method, res.status, res.body)
		}
	}

	// An id that never existed answers identically, so the endpoint cannot be
	// used to discover whether someone else holds a given pen name.
	absent := env.asOwner(t, stranger, http.MethodDelete,
		"/api/v1/me/pen-names/"+uuid.NewString())
	stolen := env.asOwner(t, stranger, http.MethodDelete, path)
	if string(absent.body) != string(stolen.body) {
		t.Fatalf("the 404s differ and become an oracle:\n%s\n%s", absent.body, stolen.body)
	}

	// The pen name is untouched by every one of those attempts.
	still := env.penNames(t, author)
	if len(still) != 1 || still[0].Name != mine.Name {
		t.Fatalf("a stranger changed the owner's list: %+v", still)
	}

	// A guest has no identities to read or to write.
	for _, attempt := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/me/pen-names"},
		{http.MethodPost, "/api/v1/me/pen-names"},
		{http.MethodPatch, path},
		{http.MethodDelete, path},
	} {
		res := env.asGuest(t, attempt.method, attempt.path, map[string]any{"name": "x"})
		if res.status != http.StatusUnauthorized {
			t.Fatalf("guest %s %s = %d, want 401. body: %s",
				attempt.method, attempt.path, res.status, res.body)
		}
	}
}

// A work can only ever be published under one of its OWN author's identities,
// and changing that choice writes no content.
func TestPenNames_AWorkCannotBorrowAnotherWritersName(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	stranger := env.newWriter(t)
	theirs := env.createPenName(t, stranger, map[string]any{"name": uniqueName(t, "คนอื่น")})

	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title": "ตอนเดียว", "content": "ข้อความที่ต้องไม่เปลี่ยน",
	})

	res := env.asOwner(t, author, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"pen_name_id": theirs.ID})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("borrowing another writer's pen name = %d, want 422. body: %s",
			res.status, res.body)
	}
	if code := errorCodeOf(t, res); code != "VALIDATION_ERROR" {
		t.Fatalf("error code = %q, want a field error", code)
	}

	// A malformed id is a field error too, never a 500.
	if res := env.asOwner(t, author, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"pen_name_id": "not-an-id"}); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("malformed pen_name_id = %d, want 422. body: %s", res.status, res.body)
	}

	// Choosing one, then clearing it, is a metadata change and nothing else.
	mine := env.createPenName(t, author, map[string]any{"name": uniqueName(t, "ของฉัน")})
	for _, choice := range []any{mine.ID, nil} {
		if res := env.asOwner(t, author, http.MethodPatch, "/api/v1/novels/"+novel.ID,
			map[string]any{"pen_name_id": choice}); res.status != http.StatusOK {
			t.Fatalf("set pen_name_id=%v status = %d. body: %s", choice, res.status, res.body)
		}
	}

	read := env.asOwner(t, author, http.MethodGet,
		"/api/v1/novels/"+novel.ID+"/chapters/"+chapter.ID)
	if read.status != http.StatusOK {
		t.Fatalf("chapter read status = %d", read.status)
	}
	if body := dataOf[chapterBody](t, read); body.Content == nil ||
		*body.Content != "ข้อความที่ต้องไม่เปลี่ยน" {
		t.Fatalf("choosing a pen name rewrote the chapter: %+v", body.Content)
	}
}
