package integration

import (
	"net/http"
	"strings"
	"testing"
)

// The achievement system end to end (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3).
//
// These drive the real HTTP API, so each assertion also exercises routing,
// CSRF, the allowlist, and the signal path from the domain choke point that
// actually fires it - which is the only place a "fire-and-forget line at a
// choke point" can be proved to have been wired at all.

// achievementEntry is the owner's view of one achievement.
type achievementEntry struct {
	Key          string  `json:"key"`
	Family       string  `json:"family"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	Count        int     `json:"count"`
	Threshold    int     `json:"threshold"`
	Unlocked     bool    `json:"unlocked"`
	UnlockedAt   *string `json:"unlocked_at"`
	Trigger      string  `json:"trigger"`
	Message      string  `json:"message"`
	Showcaseable bool    `json:"showcaseable"`
}

type eggCountBody struct {
	Unlocked int `json:"unlocked"`
	Total    int `json:"total"`
}

type ownerAchievementsBody struct {
	Enabled      bool               `json:"enabled"`
	Achievements []achievementEntry `json:"achievements"`
	Eggs         eggCountBody       `json:"eggs"`
	ShowcaseMin  int                `json:"showcase_min"`
	ShowcaseMax  int                `json:"showcase_max"`
}

type publicAchievementEntry struct {
	Key         string `json:"key"`
	Family      string `json:"family"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type publicAchievementsBody struct {
	Enabled  bool                     `json:"enabled"`
	Showcase []publicAchievementEntry `json:"showcase"`
	Unlocked int                      `json:"unlocked"`
	Total    int                      `json:"total"`
	Eggs     eggCountBody             `json:"eggs"`
}

type signalBody struct {
	Recorded bool `json:"recorded"`
	Unlocked *struct {
		Key     string `json:"key"`
		Title   string `json:"title"`
		Trigger string `json:"trigger"`
		Message string `json:"message"`
	} `json:"unlocked"`
}

// myAchievements reads the caller's own view.
func (e *authEnv) myAchievements(t *testing.T, w writer) ownerAchievementsBody {
	t.Helper()
	res := e.asOwner(t, w, http.MethodGet, "/api/v1/me/achievements")
	if res.status != http.StatusOK {
		t.Fatalf("GET /me/achievements status = %d. body: %s", res.status, res.body)
	}
	return dataOf[ownerAchievementsBody](t, res)
}

// entryFor finds one achievement in an owner view.
func entryFor(view ownerAchievementsBody, key string) (achievementEntry, bool) {
	for _, entry := range view.Achievements {
		if entry.Key == key {
			return entry, true
		}
	}
	return achievementEntry{}, false
}

// เริ่มต้น is the first step of the onboarding path, and the whole system is
// worthless if the step that fires it is not actually wired to publishing.
func TestAchievements_PublishingAFirstChapterUnlocksTheFirstStep(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	before := env.myAchievements(t, w)
	if !before.Enabled {
		t.Fatal("achievements are off for a new account; the default is on")
	}
	if entry, ok := entryFor(before, "first_chapter"); !ok || entry.Unlocked {
		t.Fatalf("เริ่มต้น is already unlocked before anything was published: %+v", entry)
	}

	novel := env.publishedNovel(t, w, nil)
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "ตอนแรก", "content": "เขาเปิดประตูออกไปโดยไม่หันกลับมามองอีกเลย",
	})
	env.publishChapter(t, w, novel.ID, chapter.ID)

	after := env.myAchievements(t, w)
	entry, ok := entryFor(after, "first_chapter")
	if !ok {
		t.Fatal("เริ่มต้น is missing from the owner view")
	}
	if !entry.Unlocked || entry.UnlockedAt == nil {
		t.Fatalf("publishing a first chapter did not unlock เริ่มต้น: %+v", entry)
	}
	if entry.Title != "เริ่มต้น" {
		t.Errorf("title = %q, want เริ่มต้น", entry.Title)
	}

	// A second publish must not move the date or re-award anything.
	second := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "ตอนที่สอง", "content": "เช้าวันถัดมาเขายังยืนอยู่ที่เดิม ราวกับไม่เคยไปไหน",
	})
	env.publishChapter(t, w, novel.ID, second.ID)

	again, _ := entryFor(env.myAchievements(t, w), "first_chapter")
	if *again.UnlockedAt != *entry.UnlockedAt {
		t.Errorf("unlocked_at moved from %s to %s on a second publish",
			*entry.UnlockedAt, *again.UnlockedAt)
	}
}

// The easter-egg rule: the public endpoint shows a count and never a name, no
// matter what the owner did or would like. Naming one kills it for everybody.
func TestAchievements_PublicViewNeverNamesAnEgg(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	res := env.asOwner(t, w, http.MethodPost, "/api/v1/achievements/signal",
		map[string]any{"key": "egg_devtools"})
	if res.status != http.StatusOK {
		t.Fatalf("signal status = %d. body: %s", res.status, res.body)
	}
	signal := dataOf[signalBody](t, res)
	if signal.Unlocked == nil {
		t.Fatalf("the egg did not unlock: %s", res.body)
	}
	// The finder gets the name, the trigger and the message - so they can tell
	// somebody, which is the entire point of an egg.
	if signal.Unlocked.Trigger == "" || signal.Unlocked.Message == "" {
		t.Errorf("the finder was told nothing about what they found: %+v", signal.Unlocked)
	}
	eggTitle := signal.Unlocked.Title

	public := env.asGuest(t, http.MethodGet, "/api/v1/users/"+w.username+"/achievements")
	if public.status != http.StatusOK {
		t.Fatalf("public status = %d. body: %s", public.status, public.body)
	}
	if body := string(public.body); namesAnything(body, eggTitle, "egg_devtools", "DevTools") {
		t.Fatalf("the public achievements response named an easter egg: %s", body)
	}

	view := dataOf[publicAchievementsBody](t, public)
	if view.Eggs.Unlocked != 1 {
		t.Errorf("public egg count = %d/%d, want 1 unlocked",
			view.Eggs.Unlocked, view.Eggs.Total)
	}
	if view.Eggs.Total < 1 {
		t.Error("the public view gives no egg denominator, so ปลดล็อกแล้ว 1 / ?? cannot render")
	}
	for _, entry := range view.Showcase {
		if entry.Family == "egg" {
			t.Fatalf("an egg reached the public showcase: %+v", entry)
		}
	}

	// And the owner may not put one there either - two guards, one rule.
	refused := env.asOwner(t, w, http.MethodPut, "/api/v1/me/achievements/showcase",
		map[string]any{"keys": []string{"egg_devtools"}})
	if refused.status != http.StatusUnprocessableEntity {
		t.Fatalf("showcasing an egg returned %d, want 422. body: %s",
			refused.status, refused.body)
	}
}

// The signal allowlist. A browser may report the four cosmetic eggs and
// nothing else: no signal from a browser may unlock anything that implies real
// work.
func TestAchievements_SignalRefusesAKeyOffTheAllowlist(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	for _, key := range []string{"first_chapter", "completed", "worldbuilder", "not_a_key"} {
		res := env.asOwner(t, w, http.MethodPost, "/api/v1/achievements/signal",
			map[string]any{"key": key})
		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("signal %q returned %d, want 422. body: %s", key, res.status, res.body)
		}
	}

	view := env.myAchievements(t, w)
	for _, key := range []string{"first_chapter", "completed", "worldbuilder"} {
		if entry, ok := entryFor(view, key); ok && entry.Unlocked {
			t.Errorf("%q was unlocked from a browser", key)
		}
	}

	// A guest has no achievements to sign for.
	guest := env.asGuest(t, http.MethodPost, "/api/v1/achievements/signal",
		map[string]any{"key": "egg_devtools"})
	if guest.status != http.StatusUnauthorized {
		t.Errorf("a guest signal returned %d, want 401. body: %s", guest.status, guest.body)
	}
}

// The global off switch, which is a promise rather than a display preference:
// off means nothing is counted, nothing is shown, and the profile has no
// achievement section at all.
func TestAchievements_OffSwitchSilencesEverything(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	off := env.asOwner(t, w, http.MethodPut, "/api/v1/me/achievements/prefs",
		map[string]any{"enabled": false})
	if off.status != http.StatusOK {
		t.Fatalf("switch off status = %d. body: %s", off.status, off.body)
	}
	if view := dataOf[ownerAchievementsBody](t, off); view.Enabled {
		t.Fatal("the response to switching off still reports the system on")
	}

	// Do the things that would otherwise award something.
	novel := env.publishedNovel(t, w, nil)
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "ตอนแรก", "content": "ลมหนาวพัดผ่านหน้าต่างที่ไม่มีใครปิดมาหลายวันแล้ว",
	})
	env.publishChapter(t, w, novel.ID, chapter.ID)
	env.asOwner(t, w, http.MethodPost, "/api/v1/achievements/signal",
		map[string]any{"key": "egg_devtools"})

	silent := env.myAchievements(t, w)
	if silent.Enabled {
		t.Fatal("the switch turned itself back on")
	}
	if len(silent.Achievements) != 0 {
		t.Errorf("%d achievements are still listed with the switch off",
			len(silent.Achievements))
	}
	if silent.Eggs.Total != 0 {
		t.Errorf("the egg slots are still shown with the switch off: %d", silent.Eggs.Total)
	}

	public := env.asGuest(t, http.MethodGet, "/api/v1/users/"+w.username+"/achievements")
	if public.status != http.StatusOK {
		t.Fatalf("public status = %d. body: %s", public.status, public.body)
	}
	publicView := dataOf[publicAchievementsBody](t, public)
	if publicView.Enabled {
		t.Error("the public view offers an achievement section for someone who switched it off")
	}
	if len(publicView.Showcase) != 0 || publicView.Unlocked != 0 || publicView.Total != 0 {
		t.Errorf("the public view still shows medals with the switch off: %+v", publicView)
	}

	// Turning it back on must find the account exactly as it was: nothing a
	// writer earned is deleted for having changed their mind, and nothing that
	// happened while it was off is retro-awarded either.
	on := env.asOwner(t, w, http.MethodPut, "/api/v1/me/achievements/prefs",
		map[string]any{"enabled": true})
	if on.status != http.StatusOK {
		t.Fatalf("switch on status = %d. body: %s", on.status, on.body)
	}
	back := dataOf[ownerAchievementsBody](t, on)
	if !back.Enabled {
		t.Fatal("the switch did not come back on")
	}
	if entry, ok := entryFor(back, "first_chapter"); !ok || entry.Unlocked {
		t.Errorf("something published while the switch was off was awarded later: %+v", entry)
	}
}

// A locked egg is never named, not even to the person who might still find it,
// and the owner's own view is the one place where naming it would be most
// tempting.
func TestAchievements_OwnerViewHidesLockedEggs(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	view := env.myAchievements(t, w)
	for _, entry := range view.Achievements {
		if entry.Family == "egg" {
			t.Errorf("a locked egg is named in its own owner's view: %+v", entry)
		}
	}
	if view.Eggs.Total < 1 {
		t.Error("the owner is not told how many eggs exist, so the blank slots cannot render")
	}
	if view.Eggs.Unlocked != 0 {
		t.Errorf("a new account already holds %d eggs", view.Eggs.Unlocked)
	}
	if view.ShowcaseMin != 3 || view.ShowcaseMax != 5 {
		t.Errorf("showcase bounds = %d-%d, want 3-5", view.ShowcaseMin, view.ShowcaseMax)
	}

	// Every listed achievement carries its own progress line and nothing that
	// could be summed into a score.
	for _, entry := range view.Achievements {
		if entry.Threshold < 1 {
			t.Errorf("%q has no threshold to make progress against", entry.Key)
		}
		if entry.Count > entry.Threshold {
			t.Errorf("%q counts %d past its threshold of %d",
				entry.Key, entry.Count, entry.Threshold)
		}
	}
}

// The showcase is the owner's editorial choice and is bounded by it: at most
// five, only what they hold, and the public read renders exactly that.
func TestAchievements_ShowcaseIsBoundedAndOwnerChosen(t *testing.T) {
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, nil)
	chapter := env.createChapter(t, w, novel.ID, map[string]any{
		"title": "ตอนแรก", "content": "เธอวางจดหมายไว้บนโต๊ะแล้วเดินออกไปโดยไม่บอกลา",
	})
	env.publishChapter(t, w, novel.ID, chapter.ID)

	// Something the writer does not hold cannot be displayed.
	notHeld := env.asOwner(t, w, http.MethodPut, "/api/v1/me/achievements/showcase",
		map[string]any{"keys": []string{"native_speaker"}})
	if notHeld.status != http.StatusUnprocessableEntity {
		t.Fatalf("showcasing a locked achievement returned %d, want 422. body: %s",
			notHeld.status, notHeld.body)
	}

	chosen := env.asOwner(t, w, http.MethodPut, "/api/v1/me/achievements/showcase",
		map[string]any{"keys": []string{"first_chapter"}})
	if chosen.status != http.StatusOK {
		t.Fatalf("showcase status = %d. body: %s", chosen.status, chosen.body)
	}

	public := env.asGuest(t, http.MethodGet, "/api/v1/users/"+w.username+"/achievements")
	view := dataOf[publicAchievementsBody](t, public)
	if len(view.Showcase) != 1 || view.Showcase[0].Key != "first_chapter" {
		t.Fatalf("public showcase = %+v, want เริ่มต้น alone", view.Showcase)
	}
	if view.Unlocked != 1 {
		t.Errorf("public unlocked count = %d, want 1", view.Unlocked)
	}
	if view.Total <= view.Unlocked {
		t.Errorf("total (%d) leaves no locked slots to render", view.Total)
	}

	// An empty list clears it - a writer may always choose to show nothing.
	cleared := env.asOwner(t, w, http.MethodPut, "/api/v1/me/achievements/showcase",
		map[string]any{"keys": []string{}})
	if cleared.status != http.StatusOK {
		t.Fatalf("clearing the showcase returned %d. body: %s", cleared.status, cleared.body)
	}
	after := dataOf[publicAchievementsBody](t,
		env.asGuest(t, http.MethodGet, "/api/v1/users/"+w.username+"/achievements"))
	if len(after.Showcase) != 0 {
		t.Errorf("the showcase did not clear: %+v", after.Showcase)
	}
}

// namesAnything reports whether the response body mentions any of these
// strings. Empty needles are skipped so a field the test failed to read cannot
// make the check trivially pass.
func namesAnything(body string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(body, needle) {
			return true
		}
	}
	return false
}
