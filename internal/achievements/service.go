package achievements

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Store is the persistence port. The concrete *Repository implements it; the
// interface exists because the rules worth defending in this package - the
// threshold, the cooldown, the idempotent unlock - are DECISIONS, and a
// decision that can only be tested against a live database is a decision that
// stops being tested.
type Store interface {
	Prefs(ctx context.Context, userID uuid.UUID) (*Prefs, error)
	SetPrefs(ctx context.Context, userID uuid.UUID, prefs Prefs) error

	Progress(ctx context.Context, userID uuid.UUID, key string) (Progress, error)
	AllProgress(ctx context.Context, userID uuid.UUID) (map[string]Progress, error)
	// Bump adds one to a key's tally at `at`, recording actor in the distinct
	// set when it is not uuid.Nil. It returns the resulting count.
	Bump(ctx context.Context, userID uuid.UUID, key string, actor uuid.UUID, at time.Time) (int, error)

	// Unlock writes the award. It reports false when the row already existed,
	// which is what makes a second unlock a no-op rather than a new date.
	Unlock(ctx context.Context, userID uuid.UUID, key string, at time.Time) (bool, error)
	Awards(ctx context.Context, userID uuid.UUID) (map[string]Award, error)
	SetShowcase(ctx context.Context, userID uuid.UUID, keys []string) error
	MarkSeen(ctx context.Context, userID uuid.UUID, at time.Time) error

	// AccountOlderThan answers the reader-driven rule: an account younger than
	// this cannot earn anybody an achievement.
	AccountOlderThan(ctx context.Context, userID uuid.UUID, age time.Duration) (bool, error)
	// ResolveUser turns a public reference into an id, or ErrNotFound for
	// anything a stranger may not see.
	ResolveUser(ctx context.Context, ref Ref) (uuid.UUID, error)
}

// Options carry the extra facts a signal may bring.
type Options struct {
	// ActorID is the OTHER account whose action caused this - the reader who
	// opened the chapter, the person who commented. Used only by
	// reader-driven achievements, where it is the whole anti-farming
	// mechanism: distinct accounts, each older than 7 days.
	ActorID uuid.UUID
}

// Service owns the achievement rules and is the authorization boundary for
// everything under /me/achievements and /users/:user/achievements.
type Service struct {
	store Store
	log   *slog.Logger

	// now is injectable so the cooldown can be tested without sleeping.
	now func() time.Time
}

func NewService(store Store, log *slog.Logger) *Service {
	return &Service{store: store, log: log, now: time.Now}
}

// SetClock replaces the service's clock. Tests only.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "USER_NOT_FOUND", "User not found.")
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("achievements service failure", slog.String("op", op), slog.Any("error", err))
	return apierror.Internal()
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// ---------------------------------------------------------------------------
// The signal side
// ---------------------------------------------------------------------------

// Record is how a domain reports that something happened.
//
// FIRE-AND-FORGET BY CONTRACT, exactly like notifications.emit: by the time it
// is called the writer's action has already committed, and no failure here may
// turn a successful publish, edit, or character sheet into an error they see.
// It therefore returns nothing at all - there is no error for a caller to
// mishandle - and every failure is logged instead.
//
// The request context may be cancelled the moment the response is written; the
// award must not be lost with it, so the work runs under context.WithoutCancel.
func (s *Service) Record(ctx context.Context, userID uuid.UUID, key string, opts Options) {
	if _, err := s.record(context.WithoutCancel(ctx), userID, key, opts); err != nil {
		s.log.Error("could not record achievement signal",
			slog.String("key", key),
			slog.Any("error", err),
		)
	}
}

// record is the rule, shared by the domain signals and the browser endpoint.
//
// Order matters, and each step is a documented product rule:
//
//  1. the global switch - off means nothing is counted at all;
//  2. an unknown key is dropped, never stored;
//  3. an achievement already unlocked stops here, which is what makes the
//     unlock idempotent and bounds the distinct-actor set;
//  4. the per-key cooldown, so ten create-delete cycles in a minute count once;
//  5. for reader-driven keys, a DISTINCT account older than 7 days;
//  6. the tally, and the unlock when the threshold is met.
func (s *Service) record(
	ctx context.Context, userID uuid.UUID, key string, opts Options,
) (SignalResult, error) {
	if userID == uuid.Nil {
		return SignalResult{}, nil
	}
	definition, known := Lookup(key)
	if !known {
		return SignalResult{}, nil
	}

	enabled, err := s.enabled(ctx, userID)
	if err != nil {
		return SignalResult{}, err
	}
	if !enabled {
		return SignalResult{}, nil
	}

	awards, err := s.store.Awards(ctx, userID)
	if err != nil {
		return SignalResult{}, err
	}
	if _, already := awards[key]; already {
		return SignalResult{}, nil
	}

	progress, err := s.store.Progress(ctx, userID, key)
	if err != nil {
		return SignalResult{}, err
	}

	now := s.now()
	if definition.DistinctActors {
		// A reader-driven achievement counts PEOPLE, so a second signal from
		// the same person adds nothing, and a brand-new account adds nothing
		// at all. Someone applauding their own work with a fresh account is
		// the farm this rule exists to stop.
		if opts.ActorID == uuid.Nil || opts.ActorID == userID || progress.counted(opts.ActorID) {
			return SignalResult{}, nil
		}
		old, err := s.store.AccountOlderThan(ctx, opts.ActorID, distinctActorMinAge)
		if err != nil {
			return SignalResult{}, err
		}
		if !old {
			return SignalResult{}, nil
		}
	} else if progress.LastAt != nil && now.Sub(*progress.LastAt) < definition.Cooldown {
		return SignalResult{}, nil
	}

	count, err := s.store.Bump(ctx, userID, key, opts.ActorID, now)
	if err != nil {
		return SignalResult{}, err
	}
	if !definition.Unlocked(count) {
		return SignalResult{Recorded: true}, nil
	}

	fresh, err := s.store.Unlock(ctx, userID, key, now)
	if err != nil {
		return SignalResult{}, err
	}
	result := SignalResult{Recorded: true}
	if fresh {
		result.Unlocked = &UnlockedView{
			Key: definition.Key, Family: definition.Family, Title: definition.Title,
			Trigger: definition.Trigger, Message: definition.Message,
		}
	}
	return result, nil
}

// Signal is POST /achievements/signal: the four cosmetic eggs, and nothing
// else ever.
//
// The allowlist is the catalogue's own ClientTriggerable flag, so a key can
// never become browser-reachable by being added to a list someone forgot to
// review. A key outside it is refused rather than silently ignored, because a
// client sending one is a client with a bug.
func (s *Service) Signal(
	ctx context.Context, identity *auth.Identity, key string,
) (SignalResult, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return SignalResult{}, err
	}
	if !ClientTriggerable(key) {
		return SignalResult{}, apierror.Validation(map[string][]string{
			"key": {"This signal cannot be sent from a browser."},
		})
	}
	result, err := s.record(ctx, userID, key, Options{})
	if err != nil {
		return SignalResult{}, s.internal("record signal", err)
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// Mine is GET /me/achievements: everything the owner has, with progress.
//
// A LOCKED egg does not appear as an entry, only in the count. That holds even
// here: an egg the owner has not found yet is one they can still find, and
// telling them the name would spend it.
func (s *Service) Mine(ctx context.Context, identity *auth.Identity) (*OwnerView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	view := &OwnerView{
		Achievements: []OwnerEntry{},
		Eggs:         EggCount{Total: EggTotal()},
		ShowcaseMin:  ShowcaseMin,
		ShowcaseMax:  ShowcaseMax,
	}

	enabled, err := s.enabled(ctx, userID)
	if err != nil {
		return nil, s.internal("load prefs", err)
	}
	view.Enabled = enabled
	if !enabled {
		// Off means off. Nothing counted, nothing shown - not even the egg
		// tally, which would still be a surface.
		view.Eggs.Total = 0
		return view, nil
	}

	awards, err := s.store.Awards(ctx, userID)
	if err != nil {
		return nil, s.internal("load awards", err)
	}
	progress, err := s.store.AllProgress(ctx, userID)
	if err != nil {
		return nil, s.internal("load progress", err)
	}

	for _, definition := range Catalog {
		award, unlocked := awards[definition.Key]
		if definition.Family == FamilyEgg {
			if !unlocked {
				continue
			}
			view.Eggs.Unlocked++
		}

		entry := OwnerEntry{
			Key:          definition.Key,
			Family:       definition.Family,
			Title:        definition.Title,
			Description:  definition.Description,
			Count:        progress[definition.Key].Count,
			Threshold:    definition.Threshold,
			Unlocked:     unlocked,
			Showcaseable: definition.Family != FamilyEgg,
		}
		if unlocked {
			// A tally that stopped short of its own threshold because the
			// award arrived first would read as a bug to the person looking at
			// it; the achievement is theirs, so the bar is full.
			if entry.Count < entry.Threshold {
				entry.Count = entry.Threshold
			}
			at := award.UnlockedAt
			entry.UnlockedAt = &at
			entry.SeenAt = award.SeenAt
			entry.ShowcaseOrder = award.ShowcaseOrder
			// Only now, and only to this person: the name, the trigger, and
			// the message, so they can tell somebody.
			entry.Trigger = definition.Trigger
			entry.Message = definition.Message
		}
		view.Achievements = append(view.Achievements, entry)
	}

	// The owner has now been shown everything they hold, so the strip owes
	// them nothing further. Best-effort: failing to stamp it would show one
	// unlock twice, which is not worth failing a read over.
	if err := s.store.MarkSeen(context.WithoutCancel(ctx), userID, s.now()); err != nil {
		s.log.Warn("could not stamp achievements as seen", slog.Any("error", err))
	}
	return view, nil
}

// Public is GET /users/:user/achievements: what the owner chose to show, plus
// counts.
//
// It takes no identity, like the profile read it sits beside: the answer is
// the same for a guest, a stranger, and the person themselves, so one cached
// response serves everybody.
//
// An egg is never named here. Not "unless showcased", not "if the owner
// agrees" - never, which is why SetShowcase refuses an egg key as well. Two
// guards for one rule, because the rule cannot be undone once broken.
func (s *Service) Public(ctx context.Context, raw string) (*PublicView, error) {
	ref, err := ParseRef(raw)
	if err != nil {
		return nil, notFound()
	}
	userID, err := s.store.ResolveUser(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, s.internal("resolve user", err)
	}

	view := &PublicView{
		Showcase: []PublicEntry{},
		Total:    ListedTotal(),
		Eggs:     EggCount{Total: EggTotal()},
	}

	enabled, err := s.enabled(ctx, userID)
	if err != nil {
		return nil, s.internal("load prefs", err)
	}
	view.Enabled = enabled
	if !enabled {
		view.Total = 0
		view.Eggs.Total = 0
		return view, nil
	}

	awards, err := s.store.Awards(ctx, userID)
	if err != nil {
		return nil, s.internal("load awards", err)
	}

	type showcased struct {
		order int
		entry PublicEntry
	}
	var chosen []showcased
	for _, definition := range Catalog {
		award, unlocked := awards[definition.Key]
		if !unlocked {
			continue
		}
		if definition.Family == FamilyEgg {
			view.Eggs.Unlocked++
			continue
		}
		view.Unlocked++
		if award.ShowcaseOrder == nil {
			continue
		}
		chosen = append(chosen, showcased{
			order: *award.ShowcaseOrder,
			entry: PublicEntry{
				Key: definition.Key, Family: definition.Family,
				Title: definition.Title, Description: definition.Description,
				UnlockedAt: award.UnlockedAt,
			},
		})
	}
	sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].order < chosen[j].order })
	for _, item := range chosen {
		view.Showcase = append(view.Showcase, item.entry)
	}
	return view, nil
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// SetShowcase chooses what the profile shows.
//
// The rules: at most ShowcaseMax; only achievements the caller has actually
// unlocked; never an egg. Fewer than ShowcaseMin is accepted only when the
// caller does not yet HAVE that many - refusing to display two medals because
// the design prefers three would be the product arguing with the person.
func (s *Service) SetShowcase(
	ctx context.Context, identity *auth.Identity, keys []string,
) (*OwnerView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	awards, err := s.store.Awards(ctx, userID)
	if err != nil {
		return nil, s.internal("load awards", err)
	}

	available := 0
	for key := range awards {
		if definition, known := Lookup(key); known && definition.Family != FamilyEgg {
			available++
		}
	}

	clean := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true

		definition, known := Lookup(key)
		if !known {
			return nil, apierror.Validation(map[string][]string{
				"keys": {"เลือกได้เฉพาะเหรียญที่มีอยู่จริง"},
			})
		}
		if definition.Family == FamilyEgg {
			// Refused, not silently dropped: showing an egg on a profile
			// would name it to every visitor, and that is the one thing that
			// cannot be taken back.
			return nil, apierror.Validation(map[string][]string{
				"keys": {"เหรียญที่ซ่อนอยู่แสดงบนโปรไฟล์ไม่ได้"},
			})
		}
		if _, unlocked := awards[key]; !unlocked {
			return nil, apierror.Validation(map[string][]string{
				"keys": {"เลือกได้เฉพาะเหรียญที่ปลดล็อกแล้ว"},
			})
		}
		clean = append(clean, key)
	}

	if len(clean) > ShowcaseMax {
		return nil, apierror.Validation(map[string][]string{
			"keys": {"เลือกแสดงได้ไม่เกิน 5 เหรียญ"},
		})
	}
	if len(clean) > 0 && len(clean) < ShowcaseMin && available >= ShowcaseMin {
		return nil, apierror.Validation(map[string][]string{
			"keys": {"เลือกแสดงอย่างน้อย 3 เหรียญ หรือไม่เลือกเลยก็ได้"},
		})
	}

	if err := s.store.SetShowcase(ctx, userID, clean); err != nil {
		return nil, s.internal("set showcase", err)
	}
	return s.Mine(ctx, identity)
}

// SetPrefs writes the global switch.
//
// Turning it off keeps every existing row. Nothing a writer earned is deleted
// for having changed their mind about wanting to see it, and turning it back
// on resumes exactly where they left off.
func (s *Service) SetPrefs(
	ctx context.Context, identity *auth.Identity, prefs Prefs,
) (*OwnerView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetPrefs(ctx, userID, prefs); err != nil {
		return nil, s.internal("save prefs", err)
	}
	return s.Mine(ctx, identity)
}

// enabled resolves the global switch for one account.
func (s *Service) enabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	stored, err := s.store.Prefs(ctx, userID)
	if err != nil {
		return false, err
	}
	effective := defaultPrefs()
	effective.apply(stored)
	return effective.Enabled, nil
}
