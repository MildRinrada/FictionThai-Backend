package integration

import (
	"net/http"
	"strings"
	"testing"
)

// Public bookshelves - the OPT-IN half of README "Bookmarks & Personal
// Library".
//
// Three things are being defended, and they are the three ways this feature
// could go wrong:
//
//  1. a shelf nobody opted into publishing stays invisible, to everyone,
//     including through its own id;
//  2. a PUBLIC shelf still cannot publish a fiction its author did not - the
//     items go through novels.ReadableSQL, the same shared predicate the
//     profile counters use;
//  3. none of this touches bookmarks. The private shelf is private forever
//     (backend/internal/library package doc), and no switch here changes that.

type shelfItemBody struct {
	Novel novelBody `json:"novel"`
	Note  *string   `json:"note"`
}

type shelfBody struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Note      *string         `json:"note"`
	IsPublic  bool            `json:"is_public"`
	Position  int             `json:"position"`
	ItemCount int64           `json:"item_count"`
	Items     []shelfItemBody `json:"items"`
}

// createShelf makes one shelf through the real endpoint.
func (e *authEnv) createShelf(t *testing.T, w writer, body map[string]any) shelfBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPost, "/api/v1/me/shelves", body)
	if res.status != http.StatusCreated {
		t.Fatalf("create shelf status = %d, want 201. body: %s", res.status, res.body)
	}
	return dataOf[shelfBody](t, res)
}

// shelveNovel puts one fiction on a shelf.
func (e *authEnv) shelveNovel(t *testing.T, w writer, shelfID, novelID string, body ...any) apiResponse {
	t.Helper()
	return e.asOwner(t, w, http.MethodPost,
		"/api/v1/me/shelves/"+shelfID+"/items/"+novelID, body...)
}

// publicShelves reads someone's shelves as a stranger with no session at all.
func (e *authEnv) publicShelves(t *testing.T, ref string) []shelfBody {
	t.Helper()
	res := e.asGuest(t, http.MethodGet, "/api/v1/users/"+ref+"/shelves")
	if res.status != http.StatusOK {
		t.Fatalf("public shelves status = %d, want 200. body: %s", res.status, res.body)
	}
	items, _ := collectionOf[shelfBody](t, res)
	return items
}

func (e *authEnv) myShelves(t *testing.T, w writer) []shelfBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/me/shelves")
	if res.status != http.StatusOK {
		t.Fatalf("my shelves status = %d, want 200. body: %s", res.status, res.body)
	}
	items, _ := collectionOf[shelfBody](t, res)
	return items
}

func findShelf(shelves []shelfBody, id string) *shelfBody {
	for i := range shelves {
		if shelves[i].ID == id {
			return &shelves[i]
		}
	}
	return nil
}

// A shelf is private until its owner says otherwise, and a private one is
// invisible to a stranger by every route into it: the listing, the id, and the
// mutations.
func TestShelves_PrivateShelfIsInvisibleToAStranger(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	owner := env.newWriter(t)
	me := env.currentUser(t, owner)
	stranger := env.newWriter(t)

	novel := env.publishedNovel(t, owner, nil)

	// No is_public in the body at all: the DEFAULT has to be private, because a
	// client that forgets the field must get the safe answer.
	shelf := env.createShelf(t, owner, map[string]any{"name": "อ่านจบแล้วยังคิดถึง"})
	if shelf.IsPublic {
		t.Fatal("a new shelf is public by default; it must be private")
	}
	if res := env.shelveNovel(t, owner, shelf.ID, novel.ID); res.status != http.StatusOK {
		t.Fatalf("shelve status = %d. body: %s", res.status, res.body)
	}

	// The owner sees it.
	if mine := findShelf(env.myShelves(t, owner), shelf.ID); mine == nil {
		t.Fatal("the owner cannot see their own shelf")
	} else if mine.ItemCount != 1 || len(mine.Items) != 1 {
		t.Fatalf("owner shelf holds %d items (count %d), want 1", len(mine.Items), mine.ItemCount)
	}

	// A stranger sees nothing - not the shelf, not its name, not the fiction on
	// it appearing by way of it.
	public := env.publicShelves(t, me.Username)
	if findShelf(public, shelf.ID) != nil {
		t.Fatalf("a private shelf appeared to a stranger: %+v", public)
	}
	body := string(env.asGuest(t, http.MethodGet, "/api/v1/users/"+me.Username+"/shelves").body)
	if strings.Contains(body, "อ่านจบแล้วยังคิดถึง") {
		t.Fatalf("a private shelf's name leaked: %s", body)
	}

	// Nor can a stranger reach it by id. A 404, not a 403: confirming that the
	// id names a real shelf would make the endpoint an oracle for other
	// people's private collections (docs/11 §3.4).
	for _, call := range []struct {
		method string
		body   []any
	}{
		{http.MethodPatch, []any{map[string]any{"is_public": true}}},
		{http.MethodDelete, nil},
	} {
		res := env.asOwner(t, stranger, call.method, "/api/v1/me/shelves/"+shelf.ID, call.body...)
		if res.status != http.StatusNotFound {
			t.Fatalf("stranger %s status = %d, want 404. body: %s", call.method, res.status, res.body)
		}
	}

	// The owner publishing it is one explicit act on one shelf.
	res := env.asOwner(t, owner, http.MethodPatch,
		"/api/v1/me/shelves/"+shelf.ID, map[string]any{"is_public": true})
	if res.status != http.StatusOK {
		t.Fatalf("publish shelf status = %d. body: %s", res.status, res.body)
	}
	published := findShelf(env.publicShelves(t, me.Username), shelf.ID)
	if published == nil {
		t.Fatal("a shelf the owner published is still invisible")
	}
	if len(published.Items) != 1 || published.Items[0].Novel.ID != novel.ID {
		t.Fatalf("published shelf does not carry its fiction: %+v", published.Items)
	}

	// And turning it back off hides it again without touching the items.
	env.asOwner(t, owner, http.MethodPatch,
		"/api/v1/me/shelves/"+shelf.ID, map[string]any{"is_public": false})
	if findShelf(env.publicShelves(t, me.Username), shelf.ID) != nil {
		t.Fatal("un-publishing a shelf left it visible")
	}
	if mine := findShelf(env.myShelves(t, owner), shelf.ID); mine == nil || mine.ItemCount != 1 {
		t.Fatal("un-publishing a shelf lost its items")
	}
}

// A public shelf publishes the SHELF, never the fiction on it: every item goes
// through the shared readability predicate, so the owner cannot put anything on
// a public page that its own author has not published.
func TestShelves_PublicShelfNeverListsUnreadableFiction(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	owner := env.newWriter(t)
	me := env.currentUser(t, owner)

	open := env.publishedNovel(t, owner, nil)
	// The owner's own private draft. They can read it, so they can shelve it -
	// which is exactly the case that must not reach a stranger.
	draft := env.createNovel(t, owner, createNovelBody(uniqueName(t, "ร่าง "),
		map[string]any{"visibility": "private", "status": "ongoing"}))
	// 18+ เนื้อหาทางเพศชัดเจน work is reachable by link and never LISTED
	// (§13B). A shelf is a listing.
	explicit := env.publishedNovel(t, owner, map[string]any{"age_rating": "explicit"})

	shelf := env.createShelf(t, owner,
		map[string]any{"name": "ชั้นเปิด", "is_public": true})
	for _, id := range []string{open.ID, draft.ID, explicit.ID} {
		if res := env.shelveNovel(t, owner, shelf.ID, id); res.status != http.StatusOK {
			t.Fatalf("shelve %s status = %d. body: %s", id, res.status, res.body)
		}
	}

	// The owner sees all three - they put them there.
	mine := findShelf(env.myShelves(t, owner), shelf.ID)
	if mine == nil || mine.ItemCount != 3 {
		t.Fatalf("the owner's own view lost an item: %+v", mine)
	}

	public := findShelf(env.publicShelves(t, me.Username), shelf.ID)
	if public == nil {
		t.Fatal("the public shelf vanished")
	}
	if len(public.Items) != 1 || public.Items[0].Novel.ID != open.ID {
		t.Fatalf("the public shelf lists %d items, want only the published one: %+v",
			len(public.Items), public.Items)
	}
	// The count must agree with the rows. A shelf that said 3 while showing 1
	// would be telling a stranger that two fictions they may not see exist.
	if public.ItemCount != 1 {
		t.Fatalf("public item_count = %d, want 1 - it must count what it shows", public.ItemCount)
	}

	body := string(env.asGuest(t, http.MethodGet, "/api/v1/users/"+me.Username+"/shelves").body)
	for label, id := range map[string]string{"private draft": draft.ID, "explicit fiction": explicit.ID} {
		if strings.Contains(body, id) {
			t.Fatalf("the public shelf leaked a %s: %s", label, body)
		}
	}
}

// The private shelf stays private. Nothing about shelves reads, exposes, or can
// be made to expose `bookmarks` - which is the whole reason they are separate
// tables (README; backend/internal/library package doc).
func TestShelves_BookmarksAreNeverExposed(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	owner := env.newWriter(t)
	me := env.currentUser(t, owner)
	author := env.newWriter(t)

	saved := env.publishedNovel(t, author, nil)
	shelved := env.publishedNovel(t, author, nil)

	// A private bookmark, made the ordinary way.
	if res := env.asOwner(t, owner, http.MethodPost,
		"/api/v1/novels/"+saved.ID+"/bookmark", nil); res.status >= 400 {
		t.Fatalf("bookmark status = %d. body: %s", res.status, res.body)
	}

	// A public shelf holding a DIFFERENT fiction, so anything bookmarked that
	// shows up here arrived by leak rather than by choice.
	shelf := env.createShelf(t, owner,
		map[string]any{"name": "ที่ฉันอ่าน", "is_public": true})
	if res := env.shelveNovel(t, owner, shelf.ID, shelved.ID); res.status != http.StatusOK {
		t.Fatalf("shelve status = %d. body: %s", res.status, res.body)
	}

	body := string(env.asGuest(t, http.MethodGet, "/api/v1/users/"+me.Username+"/shelves").body)
	if strings.Contains(body, saved.ID) {
		t.Fatalf("a bookmarked fiction appeared on a public shelf listing: %s", body)
	}
	if !strings.Contains(body, shelved.ID) {
		t.Fatal("the shelf lost the fiction that was actually put on it")
	}

	// The bookmark itself is still where it was, and still needs a session.
	if res := env.asGuest(t, http.MethodGet, "/api/v1/me/library"); res.status != http.StatusUnauthorized {
		t.Fatalf("guest library status = %d, want 401", res.status)
	}
	library, _ := collectionOf[struct {
		Novel novelBody `json:"novel"`
	}](t, env.asOwner(t, owner, http.MethodGet, "/api/v1/me/library"))
	if len(library) != 1 || library[0].Novel.ID != saved.ID {
		t.Fatalf("the bookmark did not survive the shelf: %+v", library)
	}
	// And nothing on a shelf became a bookmark either - the two are separate
	// acts, in both directions.
	if strings.Contains(string(env.asOwner(t, owner, http.MethodGet, "/api/v1/me/library").body), shelved.ID) {
		t.Fatal("shelving a fiction silently bookmarked it")
	}
}

// Shelf management is ordinary CRUD, and the parts a person will actually hit
// are the ones defended here: adding twice, removing, renaming, and deleting a
// shelf without deleting anything else.
func TestShelves_OwnerManagesTheirOwnShelves(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	owner := env.newWriter(t)
	me := env.currentUser(t, owner)
	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	shelf := env.createShelf(t, owner, map[string]any{
		"name": "ชั้นแรก", "note": "เรื่องที่อ่านซ้ำได้เรื่อย ๆ", "is_public": true,
	})
	if shelf.Note == nil || *shelf.Note != "เรื่องที่อ่านซ้ำได้เรื่อย ๆ" {
		t.Fatalf("note not saved: %+v", shelf.Note)
	}

	// Adding twice is idempotent, and the second call may carry a note.
	env.shelveNovel(t, owner, shelf.ID, novel.ID)
	res := env.shelveNovel(t, owner, shelf.ID, novel.ID, map[string]any{"note": "ตอนจบดีมาก"})
	if res.status != http.StatusOK {
		t.Fatalf("re-shelve status = %d. body: %s", res.status, res.body)
	}
	after := dataOf[shelfBody](t, res)
	if after.ItemCount != 1 {
		t.Fatalf("adding the same fiction twice made %d items", after.ItemCount)
	}
	if len(after.Items) != 1 || after.Items[0].Note == nil || *after.Items[0].Note != "ตอนจบดีมาก" {
		t.Fatalf("the item note was not saved: %+v", after.Items)
	}

	// A blank name is refused with a field error, not a 500.
	if res := env.asOwner(t, owner, http.MethodPost, "/api/v1/me/shelves",
		map[string]any{"name": "   "}); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("blank name status = %d, want 422. body: %s", res.status, res.body)
	}

	// Renaming touches only what it names; an empty note clears.
	if res := env.asOwner(t, owner, http.MethodPatch, "/api/v1/me/shelves/"+shelf.ID,
		map[string]any{"note": ""}); res.status != http.StatusOK {
		t.Fatalf("clear note status = %d. body: %s", res.status, res.body)
	}
	cleared := findShelf(env.myShelves(t, owner), shelf.ID)
	if cleared == nil || cleared.Note != nil {
		t.Fatalf("the note was not cleared: %+v", cleared)
	}
	if cleared.Name != "ชั้นแรก" {
		t.Fatalf("clearing the note renamed the shelf to %q", cleared.Name)
	}

	// Removing an item, then the shelf. Neither may touch the fiction.
	if res := env.asOwner(t, owner, http.MethodDelete,
		"/api/v1/me/shelves/"+shelf.ID+"/items/"+novel.ID); res.status != http.StatusNoContent {
		t.Fatalf("unshelve status = %d, want 204. body: %s", res.status, res.body)
	}
	if res := env.asOwner(t, owner, http.MethodDelete,
		"/api/v1/me/shelves/"+shelf.ID); res.status != http.StatusNoContent {
		t.Fatalf("delete shelf status = %d, want 204. body: %s", res.status, res.body)
	}
	if len(env.publicShelves(t, me.Username)) != 0 {
		t.Fatal("a deleted shelf is still listed")
	}
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug); res.status != http.StatusOK {
		t.Fatalf("deleting a shelf affected the fiction on it: %d", res.status)
	}

	// A guest has no shelves to manage.
	if res := env.asGuest(t, http.MethodGet, "/api/v1/me/shelves"); res.status != http.StatusUnauthorized {
		t.Fatalf("guest /me/shelves status = %d, want 401", res.status)
	}
}
