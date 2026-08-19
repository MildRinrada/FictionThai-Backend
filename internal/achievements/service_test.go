package achievements

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/users"
)

// identityFor is a signed-in caller with the given id and nothing else - these
// endpoints are all self-scoped, so a role would only be noise.
func identityFor(userID uuid.UUID) *auth.Identity {
	return &auth.Identity{User: &users.User{ID: userID}}
}

// The rules this package exists to hold, tested against an in-memory store.
//
// They are worth testing in isolation rather than only through the API because
// each one is a product decision that is hard to undo: a threshold that fires
// early awards something nobody earned, a cooldown that does not bite turns
// the set into a farming game, and an unlock that is not idempotent moves the
// date somebody earned their first medal.

// memoryStore is a Store that keeps everything in maps. It is deliberately
// dumb: every decision under test belongs to the service, so a store that
// enforced anything would be testing itself.
type memoryStore struct {
	prefs    map[uuid.UUID]Prefs
	progress map[string]Progress
	awards   map[string]Award
	accounts map[uuid.UUID]time.Time
	users    map[string]uuid.UUID

	unlockCalls int
	bumpCalls   int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		prefs:    map[uuid.UUID]Prefs{},
		progress: map[string]Progress{},
		awards:   map[string]Award{},
		accounts: map[uuid.UUID]time.Time{},
		users:    map[string]uuid.UUID{},
	}
}

func rowKey(userID uuid.UUID, key string) string { return userID.String() + "\x00" + key }

func (m *memoryStore) Prefs(_ context.Context, userID uuid.UUID) (*Prefs, error) {
	prefs, ok := m.prefs[userID]
	if !ok {
		return nil, nil
	}
	return &prefs, nil
}

func (m *memoryStore) SetPrefs(_ context.Context, userID uuid.UUID, prefs Prefs) error {
	m.prefs[userID] = prefs
	return nil
}

func (m *memoryStore) Progress(_ context.Context, userID uuid.UUID, key string) (Progress, error) {
	return m.progress[rowKey(userID, key)], nil
}

func (m *memoryStore) AllProgress(
	_ context.Context, userID uuid.UUID,
) (map[string]Progress, error) {
	out := map[string]Progress{}
	for _, definition := range Catalog {
		if progress, ok := m.progress[rowKey(userID, definition.Key)]; ok {
			out[definition.Key] = progress
		}
	}
	return out, nil
}

func (m *memoryStore) Bump(
	_ context.Context, userID uuid.UUID, key string, actor uuid.UUID, at time.Time,
) (int, error) {
	m.bumpCalls++
	id := rowKey(userID, key)
	progress := m.progress[id]
	progress.Count++
	when := at
	progress.LastAt = &when
	if actor != uuid.Nil {
		progress.Actors = append(progress.Actors, actor.String())
	}
	m.progress[id] = progress
	return progress.Count, nil
}

func (m *memoryStore) Unlock(
	_ context.Context, userID uuid.UUID, key string, at time.Time,
) (bool, error) {
	m.unlockCalls++
	id := rowKey(userID, key)
	if _, exists := m.awards[id]; exists {
		return false, nil
	}
	m.awards[id] = Award{Key: key, UnlockedAt: at}
	return true, nil
}

func (m *memoryStore) Awards(_ context.Context, userID uuid.UUID) (map[string]Award, error) {
	out := map[string]Award{}
	for _, definition := range Catalog {
		if award, ok := m.awards[rowKey(userID, definition.Key)]; ok {
			out[definition.Key] = award
		}
	}
	return out, nil
}

func (m *memoryStore) SetShowcase(_ context.Context, userID uuid.UUID, keys []string) error {
	for id, award := range m.awards {
		award.ShowcaseOrder = nil
		m.awards[id] = award
	}
	for position, key := range keys {
		id := rowKey(userID, key)
		award, ok := m.awards[id]
		if !ok {
			continue
		}
		slot := position
		award.ShowcaseOrder = &slot
		m.awards[id] = award
	}
	return nil
}

func (m *memoryStore) MarkSeen(_ context.Context, userID uuid.UUID, at time.Time) error {
	for id, award := range m.awards {
		if award.SeenAt == nil {
			when := at
			award.SeenAt = &when
			m.awards[id] = award
		}
	}
	return nil
}

func (m *memoryStore) AccountOlderThan(
	_ context.Context, userID uuid.UUID, age time.Duration,
) (bool, error) {
	created, known := m.accounts[userID]
	if !known {
		return false, nil
	}
	return time.Since(created) > age, nil
}

func (m *memoryStore) ResolveUser(_ context.Context, ref Ref) (uuid.UUID, error) {
	if !ref.ByUsername() {
		return ref.ID, nil
	}
	id, ok := m.users[ref.Username]
	if !ok {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

// testService builds a service over an in-memory store with a clock the test
// controls, so the cooldown is exercised without sleeping through it.
func testService(t *testing.T) (*Service, *memoryStore, *time.Time) {
	t.Helper()
	store := newMemoryStore()
	clock := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	service := NewService(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.SetClock(func() time.Time { return clock })
	return service, store, &clock
}

func unlocked(t *testing.T, store *memoryStore, userID uuid.UUID, key string) bool {
	t.Helper()
	awards, err := store.Awards(context.Background(), userID)
	if err != nil {
		t.Fatalf("read awards: %v", err)
	}
	_, ok := awards[key]
	return ok
}

func countOf(t *testing.T, store *memoryStore, userID uuid.UUID, key string) int {
	t.Helper()
	progress, err := store.Progress(context.Background(), userID, key)
	if err != nil {
		t.Fatalf("read progress: %v", err)
	}
	return progress.Count
}

// The threshold is the whole promise of a counted achievement: it must not
// award early, and it must award exactly when the count arrives.
func TestRecord_UnlocksOnlyAtTheThreshold(t *testing.T) {
	service, store, clock := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	definition, _ := Lookup(KeyWorldbuilder)
	for i := 1; i < definition.Threshold; i++ {
		service.Record(ctx, writer, KeyWorldbuilder, Options{})
		// Past the cooldown each time, so this test measures the threshold and
		// nothing else.
		*clock = clock.Add(definition.Cooldown + time.Second)
	}

	if unlocked(t, store, writer, KeyWorldbuilder) {
		t.Fatalf("นักสร้างโลก unlocked at %d, one short of its threshold of %d",
			definition.Threshold-1, definition.Threshold)
	}
	if got := countOf(t, store, writer, KeyWorldbuilder); got != definition.Threshold-1 {
		t.Fatalf("count = %d, want %d", got, definition.Threshold-1)
	}

	service.Record(ctx, writer, KeyWorldbuilder, Options{})
	if !unlocked(t, store, writer, KeyWorldbuilder) {
		t.Fatal("นักสร้างโลก did not unlock when its threshold was reached")
	}
}

// "Ten create-delete cycles in a minute count once"
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3, anti-farming).
func TestRecord_CooldownRejectsARapidSecondSignal(t *testing.T) {
	service, store, clock := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	definition, _ := Lookup(KeyWorldbuilder)

	service.Record(ctx, writer, KeyWorldbuilder, Options{})
	for i := 0; i < 9; i++ {
		*clock = clock.Add(definition.Cooldown / 20)
		service.Record(ctx, writer, KeyWorldbuilder, Options{})
	}

	if got := countOf(t, store, writer, KeyWorldbuilder); got != 1 {
		t.Fatalf("ten signals inside one cooldown counted %d times, want 1", got)
	}

	// And the cooldown opens again once it has actually elapsed - a rule that
	// never lets go would make the achievement unearnable.
	*clock = clock.Add(definition.Cooldown + time.Second)
	service.Record(ctx, writer, KeyWorldbuilder, Options{})
	if got := countOf(t, store, writer, KeyWorldbuilder); got != 2 {
		t.Fatalf("count after the cooldown elapsed = %d, want 2", got)
	}
}

// An award is permanent and dated once. A later signal for something already
// held must change nothing at all - not the date, not the tally.
func TestRecord_UnlockIsIdempotent(t *testing.T) {
	service, store, clock := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	service.Record(ctx, writer, KeyFirstChapter, Options{})
	first := store.awards[rowKey(writer, KeyFirstChapter)].UnlockedAt
	countAfterUnlock := countOf(t, store, writer, KeyFirstChapter)
	unlockCalls := store.unlockCalls

	// A year later, and well past every cooldown.
	*clock = clock.AddDate(1, 0, 0)
	for i := 0; i < 5; i++ {
		service.Record(ctx, writer, KeyFirstChapter, Options{})
	}

	award := store.awards[rowKey(writer, KeyFirstChapter)]
	if !award.UnlockedAt.Equal(first) {
		t.Fatalf("unlocked_at moved from %s to %s; the day someone earned "+
			"something must not change", first, award.UnlockedAt)
	}
	if got := countOf(t, store, writer, KeyFirstChapter); got != countAfterUnlock {
		t.Fatalf("count moved from %d to %d after the award was already held",
			countAfterUnlock, got)
	}
	if store.unlockCalls != unlockCalls {
		t.Fatalf("the store was asked to unlock %d more times after the award "+
			"was already held", store.unlockCalls-unlockCalls)
	}
}

// The global off switch: off means no counting at all, not merely a hidden
// section. A writer who finds this cheapens the work gets no tally kept on
// them in the meantime.
func TestRecord_OffSwitchSilencesEverything(t *testing.T) {
	service, store, _ := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	off := false
	if _, err := service.SetPrefs(ctx, identityFor(writer), Prefs{Enabled: &off}); err != nil {
		t.Fatalf("set prefs: %v", err)
	}

	service.Record(ctx, writer, KeyFirstChapter, Options{})

	if store.bumpCalls != 0 {
		t.Fatalf("the tally was bumped %d times with the switch off", store.bumpCalls)
	}
	if unlocked(t, store, writer, KeyFirstChapter) {
		t.Fatal("an achievement unlocked with the switch off")
	}

	view, err := service.Mine(ctx, identityFor(writer))
	if err != nil {
		t.Fatalf("owner view: %v", err)
	}
	if view.Enabled {
		t.Fatal("the owner view reports the system on after it was switched off")
	}
	if len(view.Achievements) != 0 || view.Eggs.Total != 0 {
		t.Fatalf("the owner view still shows %d achievements and %d egg slots "+
			"with the switch off", len(view.Achievements), view.Eggs.Total)
	}

	// Switching it back on must find everything exactly where it was - nothing
	// is deleted for having changed one's mind.
	on := true
	if _, err := service.SetPrefs(ctx, identityFor(writer), Prefs{Enabled: &on}); err != nil {
		t.Fatalf("set prefs back on: %v", err)
	}
	service.Record(ctx, writer, KeyFirstChapter, Options{})
	if !unlocked(t, store, writer, KeyFirstChapter) {
		t.Fatal("counting did not resume after the switch was turned back on")
	}
}

// A reader-driven achievement counts PEOPLE: the same reader twice is one
// reader, and an account made this morning is nobody.
func TestRecord_ReaderDrivenCountsDistinctOldAccounts(t *testing.T) {
	service, store, _ := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	fresh := uuid.New()
	store.accounts[fresh] = time.Now().Add(-2 * 24 * time.Hour)
	service.Record(ctx, writer, KeyFirstReader, Options{ActorID: fresh})
	if countOf(t, store, writer, KeyFirstReader) != 0 {
		t.Fatal("a two-day-old account earned somebody an achievement")
	}

	// The writer applauding themselves is not a reader either.
	store.accounts[writer] = time.Now().Add(-400 * 24 * time.Hour)
	service.Record(ctx, writer, KeyFirstReader, Options{ActorID: writer})
	if countOf(t, store, writer, KeyFirstReader) != 0 {
		t.Fatal("the writer's own read counted as a reader")
	}

	reader := uuid.New()
	store.accounts[reader] = time.Now().Add(-30 * 24 * time.Hour)
	service.Record(ctx, writer, KeyFirstReader, Options{ActorID: reader})
	if !unlocked(t, store, writer, KeyFirstReader) {
		t.Fatal("a month-old reader did not count")
	}
}

// The distinct-account rule has to hold for a threshold above one too, or the
// set would only ever be tested where a single signal ends it.
func TestRecord_ReaderDrivenIgnoresARepeatVisitor(t *testing.T) {
	service, store, _ := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	reader := uuid.New()
	store.accounts[reader] = time.Now().Add(-30 * 24 * time.Hour)

	// Pre-load a tally with a threshold that one signal cannot finish, so the
	// second signal from the same person has somewhere to show up if the rule
	// leaks. KeyOneIsEnough is reader-driven; raise the bar locally by
	// pretending the award is not yet held and the actor already counted.
	store.progress[rowKey(writer, KeyOneIsEnough)] = Progress{
		Count: 0, Actors: []string{reader.String()},
	}
	service.Record(ctx, writer, KeyOneIsEnough, Options{ActorID: reader})

	if got := countOf(t, store, writer, KeyOneIsEnough); got != 0 {
		t.Fatalf("a reader already counted was counted again (count = %d)", got)
	}
}

// The easter-egg rule, on the public surface: an egg is a number and never a
// name, whatever the owner would like.
func TestPublic_NeverNamesAnEgg(t *testing.T) {
	service, store, _ := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	service.Record(ctx, writer, KeyEggDevTools, Options{})
	service.Record(ctx, writer, KeyFirstChapter, Options{})
	if _, err := service.SetShowcase(ctx, identityFor(writer), []string{KeyFirstChapter}); err != nil {
		t.Fatalf("showcase: %v", err)
	}

	view, err := service.Public(ctx, writer.String())
	if err != nil {
		t.Fatalf("public view: %v", err)
	}
	if view.Eggs.Unlocked != 1 || view.Eggs.Total != EggTotal() {
		t.Fatalf("egg count = %d/%d, want 1/%d",
			view.Eggs.Unlocked, view.Eggs.Total, EggTotal())
	}
	egg, _ := Lookup(KeyEggDevTools)
	for _, entry := range view.Showcase {
		if entry.Family == FamilyEgg || entry.Key == egg.Key || entry.Title == egg.Title {
			t.Fatalf("the public view named an easter egg: %+v", entry)
		}
	}
	if len(view.Showcase) != 1 || view.Showcase[0].Key != KeyFirstChapter {
		t.Fatalf("showcase = %+v, want only เริ่มต้น", view.Showcase)
	}
	_ = store
}

// The second guard on the same rule: an egg cannot even be CHOSEN for display,
// so the public view is not the only thing standing between an egg and a
// stranger.
func TestSetShowcase_RefusesAnEgg(t *testing.T) {
	service, _, _ := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	service.Record(ctx, writer, KeyEggDevTools, Options{})
	if _, err := service.SetShowcase(ctx, identityFor(writer), []string{KeyEggDevTools}); err == nil {
		t.Fatal("an easter egg was accepted for the public showcase")
	}
}

// A locked egg is not listed even to the person who might still find it.
func TestMine_HidesLockedEggsAndNamesFoundOnes(t *testing.T) {
	service, _, _ := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	service.Record(ctx, writer, KeyEggCtrlS, Options{})

	view, err := service.Mine(ctx, identityFor(writer))
	if err != nil {
		t.Fatalf("owner view: %v", err)
	}

	named := map[string]OwnerEntry{}
	for _, entry := range view.Achievements {
		named[entry.Key] = entry
	}
	for _, definition := range Catalog {
		if definition.Family != FamilyEgg {
			continue
		}
		entry, listed := named[definition.Key]
		if definition.Key == KeyEggCtrlS {
			if !listed {
				t.Fatal("an egg the owner found is missing from their own view")
			}
			if entry.Trigger == "" || entry.Message == "" {
				t.Fatal("a found egg does not tell its owner how they found it")
			}
			if entry.Showcaseable {
				t.Fatal("a found egg is offered for the public showcase")
			}
			continue
		}
		if listed {
			t.Fatalf("locked egg %q was named to its owner", definition.Key)
		}
	}
	if view.Eggs.Unlocked != 1 || view.Eggs.Total != EggTotal() {
		t.Fatalf("owner egg count = %d/%d", view.Eggs.Unlocked, view.Eggs.Total)
	}
}

// The client allowlist is the catalogue's own flag, so nothing that implies
// real work can be unlocked from a browser.
func TestSignal_AllowsOnlyClientTriggerableKeys(t *testing.T) {
	service, store, _ := testService(t)
	ctx := context.Background()
	writer := uuid.New()

	if _, err := service.Signal(ctx, identityFor(writer), KeyFirstChapter); err == nil {
		t.Fatal("a browser was allowed to signal เริ่มต้น")
	}
	if unlocked(t, store, writer, KeyFirstChapter) {
		t.Fatal("a refused signal still unlocked something")
	}

	result, err := service.Signal(ctx, identityFor(writer), KeyEggAdminPath)
	if err != nil {
		t.Fatalf("signal: %v", err)
	}
	if result.Unlocked == nil || result.Unlocked.Key != KeyEggAdminPath {
		t.Fatalf("an allowlisted egg did not unlock: %+v", result)
	}
	if result.Unlocked.Message == "" {
		t.Fatal("the unlock carries no message for the strip to show")
	}
}
