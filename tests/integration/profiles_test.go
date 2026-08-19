package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Phase 12E - the public profile (docs/PHASE-12-STORY-DEPTH.md §12E).
//
// Two things are being defended here. First, that the response is the PUBLIC
// half of an identity and nothing else - no email, no role, no account status
// (docs/10 §8). Second, that its counters count only work a stranger can
// actually open, so a profile cannot become a side channel for private drafts.

type profileBody struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name"`
	AvatarURL   *string `json:"avatar_url"`
	BannerURL   *string `json:"banner_url"`
	Bio         *string `json:"bio"`
	WebsiteURL  *string `json:"website_url"`
	JoinedAt    string  `json:"joined_at"`
	Links       []struct {
		Label string `json:"label"`
		URL   string `json:"url"`
	} `json:"links"`

	IsAuthor    bool     `json:"is_author"`
	PenName     *string  `json:"pen_name"`
	AuthorBio   *string  `json:"author_bio"`
	DonationURL *string  `json:"donation_url"`
	OpenFor     []string `json:"open_for"`
	Boundaries  *string  `json:"boundaries"`

	// WallEnabled is the profile wall's switch, published so a page knows
	// whether to ask for the wall at all (see wall_test.go).
	WallEnabled bool `json:"wall_enabled"`

	Pinned []struct {
		NovelID string  `json:"novel_id"`
		Slug    string  `json:"slug"`
		Title   string  `json:"title"`
		Note    *string `json:"note"`
	} `json:"pinned"`

	NovelCount    int64 `json:"novel_count"`
	FollowerCount int64 `json:"follower_count"`
	TotalViews    int64 `json:"total_views"`
}

func (e *authEnv) publicProfile(t *testing.T, ref string) apiResponse {
	t.Helper()
	return e.asGuest(t, http.MethodGet, "/api/v1/users/"+ref)
}

// account is the caller's own private view, used here only to learn the
// identifiers a test needs before asking for the public one.
type accountBody struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

func (e *authEnv) currentUser(t *testing.T, w writer) accountBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/auth/me")
	if res.status != http.StatusOK {
		t.Fatalf("auth/me status = %d. body: %s", res.status, res.body)
	}
	return dataOf[accountBody](t, res)
}

// The endpoint publishes an identity a stranger may see, by username or by id,
// and never anything from the private account view.
func TestProfiles_PublicReadNeverLeaksTheAccount(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	me := env.currentUser(t, author)

	res := env.publicProfile(t, me.Username)
	if res.status != http.StatusOK {
		t.Fatalf("profile status = %d, want 200. body: %s", res.status, res.body)
	}
	profile := dataOf[profileBody](t, res)

	if profile.Username != me.Username || profile.ID != me.ID {
		t.Fatalf("profile identifies the wrong person: %+v", profile)
	}
	if profile.JoinedAt == "" {
		t.Fatal("joined_at missing")
	}

	// The account half must not be reachable through this door, for anyone.
	body := string(res.body)
	for _, forbidden := range []string{
		me.Email, `"email"`, `"role"`, `"status"`, `"email_verified"`, `"password`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public profile leaks %q: %s", forbidden, body)
		}
	}

	// The same person by id, with the same answer.
	byID := env.publicProfile(t, me.ID)
	if byID.status != http.StatusOK {
		t.Fatalf("profile by id status = %d, want 200", byID.status)
	}
	if dataOf[profileBody](t, byID).Username != me.Username {
		t.Fatal("resolving by id and by username disagree")
	}

	// And the caller's own session buys no extra fields - the response is the
	// same bytes for everyone, which is what makes it cacheable.
	asSelf := env.asOwner(t, author, http.MethodGet, "/api/v1/users/"+me.Username)
	if string(asSelf.body) != body {
		t.Fatalf("the profile differs for the person themselves:\n%s\n%s", body, asSelf.body)
	}
}

// Absent, malformed, and never-registered all answer identically, so the
// endpoint cannot be used to enumerate accounts (docs/11 §3.4).
func TestProfiles_UnknownUsersAreIndistinguishable(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	answers := make([]string, 0, 4)
	for _, ref := range []string{
		"nobody_here_at_all",
		uuid.NewString(),
		"a", // too short to have ever been registered
		"admin",
	} {
		res := env.publicProfile(t, ref)
		if res.status != http.StatusNotFound {
			t.Fatalf("%q status = %d, want 404. body: %s", ref, res.status, res.body)
		}
		answers = append(answers, string(res.body))
	}
	for i := 1; i < len(answers); i++ {
		if answers[i] != answers[0] {
			t.Fatalf("404s differ and become an oracle:\n%s\n%s", answers[0], answers[i])
		}
	}
}

// The counters describe the work a stranger can open - never the drafts.
func TestProfiles_CountersExcludeUnreadableWork(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	me := env.currentUser(t, author)

	env.publishedNovel(t, author, nil)
	env.publishedNovel(t, author, nil)
	private := env.createNovel(t, author, createNovelBody(uniqueName(t, "Private "),
		map[string]any{"visibility": "private", "status": "ongoing"}))

	profile := dataOf[profileBody](t, env.publicProfile(t, me.Username))
	if profile.NovelCount != 2 {
		t.Fatalf("novel_count = %d, want 2 (the private one must not count)", profile.NovelCount)
	}
	if strings.Contains(string(env.publicProfile(t, me.Username).body), private.ID) {
		t.Fatal("the profile mentions a private fiction")
	}

	// The owner looking at their own profile sees the same number: a count that
	// changed with the viewer would not be the same claim.
	asSelf := dataOf[profileBody](t,
		env.asOwner(t, author, http.MethodGet, "/api/v1/users/"+me.Username))
	if asSelf.NovelCount != 2 {
		t.Fatalf("owner novel_count = %d, want 2", asSelf.NovelCount)
	}
}

// follower_count is maintained in the same statement as the follow row, so it
// tracks user_follows exactly - including through an idempotent repeat.
func TestProfiles_FollowerCountTracksTheFollowTable(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	me := env.currentUser(t, author)
	reader := env.newUnverifiedWriter(t)

	if got := dataOf[profileBody](t, env.publicProfile(t, me.Username)).FollowerCount; got != 0 {
		t.Fatalf("follower_count starts at %d, want 0", got)
	}

	followPath := "/api/v1/users/" + me.ID + "/follow"
	if res := env.asOwner(t, reader, http.MethodPost, followPath, nil); res.status >= 400 {
		t.Fatalf("follow status = %d. body: %s", res.status, res.body)
	}
	if got := dataOf[profileBody](t, env.publicProfile(t, me.Username)).FollowerCount; got != 1 {
		t.Fatalf("follower_count after follow = %d, want 1", got)
	}

	// Idempotent: following again is still one follower.
	env.asOwner(t, reader, http.MethodPost, followPath, nil)
	if got := dataOf[profileBody](t, env.publicProfile(t, me.Username)).FollowerCount; got != 1 {
		t.Fatalf("a repeated follow inflated the counter to %d", got)
	}

	env.asOwner(t, reader, http.MethodDelete, followPath)
	if got := dataOf[profileBody](t, env.publicProfile(t, me.Username)).FollowerCount; got != 0 {
		t.Fatalf("follower_count after unfollow = %d, want 0", got)
	}

	// And unfollowing again cannot drive it negative.
	env.asOwner(t, reader, http.MethodDelete, followPath)
	if got := dataOf[profileBody](t, env.publicProfile(t, me.Username)).FollowerCount; got != 0 {
		t.Fatalf("a repeated unfollow drove the counter to %d", got)
	}
}

// The writer's own external support link is part of the public profile - that
// is the whole point of it (docs/MONETIZATION.md §6).
func TestProfiles_AuthorHalfAppearsOnlyForAuthors(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	reader := env.newUnverifiedWriter(t)
	readerName := env.currentUser(t, reader).Username
	plain := dataOf[profileBody](t, env.publicProfile(t, readerName))
	if plain.IsAuthor {
		t.Fatal("someone with no author profile is reported as an author")
	}
	if plain.DonationURL != nil {
		t.Fatalf("donation link on a non-author: %+v", plain.DonationURL)
	}

	author := env.newWriter(t)
	authorName := env.currentUser(t, author).Username
	res := env.asOwner(t, author, http.MethodPut, "/api/v1/me/author-profile",
		map[string]any{"donation_url": "https://easydonate.example/nattavara"})
	if res.status != http.StatusOK {
		t.Fatalf("set donation url status = %d. body: %s", res.status, res.body)
	}

	withAuthor := dataOf[profileBody](t, env.publicProfile(t, authorName))
	if !withAuthor.IsAuthor {
		t.Fatal("an account with an author profile is not reported as an author")
	}
	if withAuthor.DonationURL == nil ||
		*withAuthor.DonationURL != "https://easydonate.example/nattavara" {
		t.Fatalf("donation link missing from the public profile: %+v", withAuthor)
	}
}

// The timeline is the existing audience-filtered community listing; a private
// post must not appear on its author's public profile.
func TestProfiles_TimelineRespectsPostVisibility(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	name := env.currentUser(t, author).Username

	open := env.createCommunityPost(t, author, uniqueName(t, "โพสต์สาธารณะ "), "public")
	secret := env.createCommunityPost(t, author, uniqueName(t, "โพสต์ส่วนตัว "), "private")

	res := env.asGuest(t, http.MethodGet,
		"/api/v1/community/posts?author="+name+"&per_page=100")
	items, _ := collectionOf[postBody](t, res)

	var sawOpen, sawSecret bool
	for _, item := range items {
		switch item.ID {
		case open.ID:
			sawOpen = true
		case secret.ID:
			sawSecret = true
		}
	}
	if !sawOpen {
		t.Fatal("the timeline dropped the author's public post")
	}
	if sawSecret {
		t.Fatal("a private post appeared on a public timeline")
	}
}

// The profile WRITE path (docs/PROFILE-AND-ACHIEVEMENTS.md Part 1). Before it
// existed, display_name / bio / website_url were readable everywhere and
// writable nowhere. What is defended here: a person edits their OWN row, the
// response is the public view, a partial patch leaves untouched fields alone,
// an empty string clears, and a stranger's row is unreachable.
func TestProfiles_OwnerEditsTheirOwnProfile(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	person := env.newWriter(t)
	name := env.currentUser(t, person).Username

	patch := func(body map[string]any) apiResponse {
		return env.asOwner(t, person, http.MethodPatch, "/api/v1/me/profile", body)
	}

	res := patch(map[string]any{
		"display_name": "ชื่อที่แสดง",
		"bio":          "เขียนฟิคเป็นงานอดิเรก",
		"website_url":  "https://example.com/me",
		"links": []map[string]string{
			{"label": "X", "url": "https://x.com/someone"},
			{"label": "", "url": "https://www.pixiv.net/users/1"},
		},
		"open_for": []string{"commission", "beta"},
	})
	if res.status != http.StatusOK {
		t.Fatalf("patch profile status = %d. body: %s", res.status, res.body)
	}
	saved := dataOf[profileBody](t, res)
	if saved.DisplayName == nil || *saved.DisplayName != "ชื่อที่แสดง" {
		t.Fatalf("display name not saved: %+v", saved.DisplayName)
	}
	if len(saved.Links) != 2 || saved.Links[0].Label != "X" {
		t.Fatalf("links not saved: %+v", saved.Links)
	}
	// A link with no label is named by its host rather than refused.
	if saved.Links[1].Label != "pixiv.net" {
		t.Fatalf("unlabelled link = %q, want its host", saved.Links[1].Label)
	}
	if len(saved.OpenFor) != 2 {
		t.Fatalf("open_for not saved: %+v", saved.OpenFor)
	}
	// Setting availability creates the author profile on demand.
	if !saved.IsAuthor {
		t.Fatal("declaring availability did not create the author profile")
	}

	// A partial patch touches only what it names.
	if res := patch(map[string]any{"bio": "แก้เฉพาะแนะนำตัว"}); res.status != http.StatusOK {
		t.Fatalf("partial patch status = %d. body: %s", res.status, res.body)
	}
	after := dataOf[profileBody](t, env.publicProfile(t, name))
	if after.DisplayName == nil || *after.DisplayName != "ชื่อที่แสดง" {
		t.Fatalf("a partial patch erased the display name: %+v", after.DisplayName)
	}
	if after.Bio == nil || *after.Bio != "แก้เฉพาะแนะนำตัว" {
		t.Fatalf("bio not updated: %+v", after.Bio)
	}

	// An empty string clears the field - the writer's way of removing it.
	if res := patch(map[string]any{"display_name": ""}); res.status != http.StatusOK {
		t.Fatalf("clear patch status = %d. body: %s", res.status, res.body)
	}
	cleared := dataOf[profileBody](t, env.publicProfile(t, name))
	if cleared.DisplayName != nil {
		t.Fatalf("display name not cleared: %+v", cleared.DisplayName)
	}

	// Rubbish is refused with a field error, not a 500.
	if res := patch(map[string]any{"website_url": "javascript:alert(1)"}); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("javascript: url status = %d, want 422. body: %s", res.status, res.body)
	}
	if res := patch(map[string]any{"open_for": []string{"anything"}}); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown availability status = %d, want 422. body: %s", res.status, res.body)
	}

	// A guest has no profile to edit.
	if res := env.asGuest(t, http.MethodPatch, "/api/v1/me/profile"); res.status != http.StatusUnauthorized {
		t.Fatalf("guest patch status = %d, want 401", res.status)
	}
}

// ชั้นวางเรื่องที่ปักหมุด: three works the writer puts at the top of their own
// page, each with one line of their own. What is defended: the pins survive a
// round trip, a work that is not readable is absent rather than leaked, and
// another writer's fiction can never be pinned onto your profile.
func TestProfiles_PinnedWorksLeadTheProfile(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	w := env.newWriter(t)
	name := env.currentUser(t, w).Username
	published := env.publishedNovel(t, w, nil)
	draft := env.createNovel(t, w, createNovelBody(uniqueName(t, "Draft "),
		map[string]any{"visibility": "private", "status": "draft"}))

	stranger := env.newWriter(t)
	theirs := env.publishedNovel(t, stranger, nil)

	res := env.asOwner(t, w, http.MethodPatch, "/api/v1/me/profile", map[string]any{
		"pinned": []map[string]string{
			{"novel_id": published.ID, "note": "เริ่มที่เรื่องนี้"},
			{"novel_id": draft.ID, "note": "ยังไม่เสร็จ"},
			{"novel_id": theirs.ID, "note": "ของคนอื่น"},
		},
	})
	if res.status != http.StatusOK {
		t.Fatalf("pin status = %d. body: %s", res.status, res.body)
	}

	profile := dataOf[profileBody](t, env.publicProfile(t, name))
	if len(profile.Pinned) != 1 {
		t.Fatalf("pinned = %+v, want only the readable one of this writer's own", profile.Pinned)
	}
	if profile.Pinned[0].NovelID != published.ID {
		t.Fatalf("pinned the wrong work: %+v", profile.Pinned[0])
	}
	if profile.Pinned[0].Note == nil || *profile.Pinned[0].Note != "เริ่มที่เรื่องนี้" {
		t.Fatalf("the writer's own line was lost: %+v", profile.Pinned[0].Note)
	}
	// The unpublished pin is absent - not its title, not its slug.
	if body := string(env.publicProfile(t, name).body); strings.Contains(body, draft.ID) {
		t.Fatalf("an unpublished pin leaked onto the profile: %s", body)
	}

	// Sending an empty list clears the shelf.
	if res := env.asOwner(t, w, http.MethodPatch, "/api/v1/me/profile",
		map[string]any{"pinned": []map[string]string{}}); res.status != http.StatusOK {
		t.Fatalf("clear status = %d. body: %s", res.status, res.body)
	}
	if got := dataOf[profileBody](t, env.publicProfile(t, name)).Pinned; len(got) != 0 {
		t.Fatalf("pins survived a clear: %+v", got)
	}

	// More than three is refused with a field error, not silently truncated.
	four := make([]map[string]string, 0, 4)
	for i := 0; i < 4; i++ {
		four = append(four, map[string]string{"novel_id": env.publishedNovel(t, w, nil).ID})
	}
	if res := env.asOwner(t, w, http.MethodPatch, "/api/v1/me/profile",
		map[string]any{"pinned": four}); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("four pins = %d, want 422. body: %s", res.status, res.body)
	}
}
