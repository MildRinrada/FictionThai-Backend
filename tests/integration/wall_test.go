package integration

import (
	"net/http"
	"strings"
	"testing"
)

// The profile comment wall.
//
// What is being defended: a message can be left and read without ceremony; the
// person who wrote it can take it back; the person whose page it is can clear
// it - a switch is not enough on its own, because "live with it or close the
// wall" is not a choice; the switch itself hides the wall ENTIRELY without
// destroying anything; and there is no guest wall at all.

type wallBody struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	IsOwner   bool   `json:"is_owner"`
	CanDelete bool   `json:"can_delete"`
	Author    struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
}

func (e *authEnv) postToWall(t *testing.T, w writer, ref, body string) apiResponse {
	t.Helper()
	return e.asOwner(t, w, http.MethodPost, "/api/v1/users/"+ref+"/wall",
		map[string]any{"body": body})
}

// readWall reads a wall as a guest - no session of any kind.
func (e *authEnv) readWall(t *testing.T, ref string) ([]wallBody, apiResponse) {
	t.Helper()
	res := e.asGuest(t, http.MethodGet, "/api/v1/users/"+ref+"/wall")
	if res.status != http.StatusOK {
		return nil, res
	}
	items, _ := collectionOf[wallBody](t, res)
	return items, res
}

func (e *authEnv) setWall(t *testing.T, w writer, enabled bool) {
	t.Helper()
	res := e.asOwner(t, w, http.MethodPatch, "/api/v1/me/profile",
		map[string]any{"wall_enabled": enabled})
	if res.status != http.StatusOK {
		t.Fatalf("set wall_enabled=%v status = %d. body: %s", enabled, res.status, res.body)
	}
}

// The ordinary path: a visitor leaves a word, everyone reads it, and the person
// who wrote it can take it back.
func TestWall_PostReadAndDeleteByAuthor(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	owner := env.newWriter(t)
	page := env.currentUser(t, owner)
	visitor := env.newWriter(t)
	visitorAccount := env.currentUser(t, visitor)

	res := env.postToWall(t, visitor, page.Username, "  อ่านเรื่องล่าสุดแล้ว ชอบมากค่ะ  ")
	if res.status != http.StatusCreated {
		t.Fatalf("post to wall status = %d, want 201. body: %s", res.status, res.body)
	}
	posted := dataOf[wallBody](t, res)
	// Stored trimmed: trailing whitespace is never meaningful.
	if posted.Body != "อ่านเรื่องล่าสุดแล้ว ชอบมากค่ะ" {
		t.Fatalf("body stored as %q", posted.Body)
	}
	if posted.Author.ID != visitorAccount.ID {
		t.Fatalf("the entry is attributed to the wrong person: %+v", posted.Author)
	}

	// A guest reads it, with no session at all.
	entries, _ := env.readWall(t, page.Username)
	if len(entries) != 1 || entries[0].ID != posted.ID {
		t.Fatalf("the wall does not show the message: %+v", entries)
	}
	if entries[0].CanDelete {
		t.Fatal("a guest is offered a delete control")
	}

	// The author sees their own control.
	mine, _ := collectionOf[wallBody](t,
		env.asOwner(t, visitor, http.MethodGet, "/api/v1/users/"+page.Username+"/wall"))
	if len(mine) != 1 || !mine[0].IsOwner || !mine[0].CanDelete {
		t.Fatalf("the author of a message is not offered its controls: %+v", mine)
	}

	// An unrelated signed-in reader is not.
	other := env.newWriter(t)
	theirs, _ := collectionOf[wallBody](t,
		env.asOwner(t, other, http.MethodGet, "/api/v1/users/"+page.Username+"/wall"))
	if len(theirs) != 1 || theirs[0].CanDelete {
		t.Fatalf("a stranger is offered someone else's delete control: %+v", theirs)
	}
	if res := env.asOwner(t, other, http.MethodDelete, "/api/v1/wall/"+posted.ID); res.status != http.StatusForbidden {
		t.Fatalf("stranger delete status = %d, want 403. body: %s", res.status, res.body)
	}

	// The author takes it back, and a repeat is still a success.
	for range 2 {
		if res := env.asOwner(t, visitor, http.MethodDelete,
			"/api/v1/wall/"+posted.ID); res.status != http.StatusNoContent {
			t.Fatalf("author delete status = %d, want 204. body: %s", res.status, res.body)
		}
	}
	if entries, _ := env.readWall(t, page.Username); len(entries) != 0 {
		t.Fatalf("the message survived its author deleting it: %+v", entries)
	}
}

// The person whose page it is may clear anything on it. Without this, the
// switch would be the only remedy, and closing your wall because of one message
// is not a remedy.
func TestWall_ProfileOwnerCanClearTheirOwnWall(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	owner := env.newWriter(t)
	page := env.currentUser(t, owner)
	visitor := env.newWriter(t)

	posted := dataOf[wallBody](t, env.postToWall(t, visitor, page.Username, "ข้อความที่เจ้าของหน้าไม่อยากเก็บไว้"))

	// The owner is offered the control on somebody else's message, but is not
	// told it is theirs.
	seen, _ := collectionOf[wallBody](t,
		env.asOwner(t, owner, http.MethodGet, "/api/v1/users/"+page.Username+"/wall"))
	if len(seen) != 1 || !seen[0].CanDelete || seen[0].IsOwner {
		t.Fatalf("the profile owner's view of a visitor's message is wrong: %+v", seen)
	}

	if res := env.asOwner(t, owner, http.MethodDelete,
		"/api/v1/wall/"+posted.ID); res.status != http.StatusNoContent {
		t.Fatalf("owner delete status = %d, want 204. body: %s", res.status, res.body)
	}
	if entries, _ := env.readWall(t, page.Username); len(entries) != 0 {
		t.Fatalf("the owner could not clear their own wall: %+v", entries)
	}
}

// The switch hides the wall entirely - and keeps what was there, so turning it
// back on restores the page rather than starting an empty one.
func TestWall_DisappearsWhenSwitchedOff(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	owner := env.newWriter(t)
	page := env.currentUser(t, owner)
	visitor := env.newWriter(t)

	env.postToWall(t, visitor, page.Username, "ฝากไว้ก่อนปิดผนัง")

	// The public profile says the wall is open, which is how a page knows to
	// ask for it at all.
	if !dataOf[profileBody](t, env.publicProfile(t, page.Username)).WallEnabled {
		t.Fatal("wall_enabled is not true by default")
	}

	env.setWall(t, owner, false)

	if !strings.Contains(
		string(env.publicProfile(t, page.Username).body), `"wall_enabled":false`) {
		t.Fatal("the public profile does not report the wall as closed")
	}

	entries, res := env.readWall(t, page.Username)
	if res.status != http.StatusNotFound {
		t.Fatalf("closed wall read status = %d, want 404. body: %s", res.status, res.body)
	}
	if code := errorCodeOf(t, res); code != "WALL_DISABLED" {
		t.Fatalf("closed wall error code = %q, want WALL_DISABLED", code)
	}
	if len(entries) != 0 {
		t.Fatalf("a closed wall returned entries: %+v", entries)
	}

	// Nobody may post to a closed wall either - not a visitor, not the owner.
	for _, w := range []writer{visitor, owner} {
		if res := env.postToWall(t, w, page.Username, "ยังส่งได้ไหม"); res.status != http.StatusNotFound {
			t.Fatalf("post to closed wall status = %d, want 404. body: %s", res.status, res.body)
		}
	}

	// Re-opening restores what was already there. The messages were hidden, not
	// destroyed - the platform never silently deletes what someone wrote.
	env.setWall(t, owner, true)
	restored, _ := env.readWall(t, page.Username)
	if len(restored) != 1 || restored[0].Body != "ฝากไว้ก่อนปิดผนัง" {
		t.Fatalf("re-opening the wall lost its messages: %+v", restored)
	}
}

// There is no guest wall: with no fiction behind it there is no author to
// review a queue, so an account someone can hold responsible is the only rule
// that works. Reading, though, needs nothing.
func TestWall_GuestCanReadButNotPost(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	owner := env.newWriter(t)
	page := env.currentUser(t, owner)
	visitor := env.newWriter(t)
	env.postToWall(t, visitor, page.Username, "ทักทายจากผู้อ่าน")

	if entries, _ := env.readWall(t, page.Username); len(entries) != 1 {
		t.Fatalf("a guest cannot read the wall: %+v", entries)
	}

	res := env.asGuest(t, http.MethodPost, "/api/v1/users/"+page.Username+"/wall",
		map[string]any{"body": "ขอฝากโดยไม่ล็อกอิน"})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("guest post status = %d, want 401. body: %s", res.status, res.body)
	}
	if res := env.asGuest(t, http.MethodDelete, "/api/v1/wall/"+"00000000-0000-4000-8000-000000000000"); res.status != http.StatusUnauthorized {
		t.Fatalf("guest delete status = %d, want 401", res.status)
	}

	// An unknown page and a malformed one answer identically, so the wall
	// cannot be used to enumerate accounts (docs/11 §3.4).
	answers := []string{}
	for _, ref := range []string{"nobody_here_at_all", "a"} {
		res := env.asGuest(t, http.MethodGet, "/api/v1/users/"+ref+"/wall")
		if res.status != http.StatusNotFound {
			t.Fatalf("%q wall status = %d, want 404", ref, res.status)
		}
		answers = append(answers, string(res.body))
	}
	if answers[0] != answers[1] {
		t.Fatalf("404s differ and become an oracle:\n%s\n%s", answers[0], answers[1])
	}

	// An empty message is a field error, not a 500.
	if res := env.postToWall(t, visitor, page.Username, "   "); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("empty body status = %d, want 422. body: %s", res.status, res.body)
	}
}

// คำเตือน/ขอบเขตของนักเขียน rides on the same self-scoped profile PATCH, and
// lands on the public profile verbatim.
func TestWall_BoundariesAreWriterAuthoredFreeText(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	name := env.currentUser(t, author).Username

	const stated = "ไม่รับเรื่องที่มีตัวละครอายุต่ำกว่า 18 / เขียนเฉพาะ AU มหาลัย"
	if res := env.asOwner(t, author, http.MethodPatch, "/api/v1/me/profile",
		map[string]any{"boundaries": stated}); res.status != http.StatusOK {
		t.Fatalf("set boundaries status = %d. body: %s", res.status, res.body)
	}

	profile := dataOf[profileBody](t, env.publicProfile(t, name))
	if profile.Boundaries == nil || *profile.Boundaries != stated {
		t.Fatalf("boundaries not published verbatim: %+v", profile.Boundaries)
	}
	// Stating boundaries creates the author profile on demand, like the other
	// author-half fields.
	if !profile.IsAuthor {
		t.Fatal("stating boundaries did not create the author profile")
	}

	// An empty string clears it - the writer's way of withdrawing the notice.
	env.asOwner(t, author, http.MethodPatch, "/api/v1/me/profile",
		map[string]any{"boundaries": ""})
	if cleared := dataOf[profileBody](t, env.publicProfile(t, name)); cleared.Boundaries != nil {
		t.Fatalf("boundaries not cleared: %+v", cleared.Boundaries)
	}
}
