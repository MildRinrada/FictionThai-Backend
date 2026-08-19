package achievements

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/users"
)

// ErrNotFound covers every reason a person's achievements cannot be shown -
// absent, deleted, banned. The service turns all of them into one 404, so the
// endpoint cannot be used to enumerate accounts (docs/11 §3.4).
var ErrNotFound = errors.New("user not found")

// RefParam is the path parameter name for a user reference. It matches the
// profile and follow endpoints' parameter, because Gin allows one wildcard
// name per path position and all three share `/users/:user`.
const RefParam = "user"

// Ref identifies a user in a URL: a UUID or a username. The same rule the
// profile read uses, for the same reason - public URLs read better with the
// name and both resolve to one row.
type Ref struct {
	ID       uuid.UUID
	Username string
}

// ByUsername reports whether the reference is a name rather than a UUID.
func (r Ref) ByUsername() bool { return r.ID == uuid.Nil }

// ParseRef interprets a path parameter. A malformed reference is ErrNotFound
// rather than a parse error: the difference between "well-formed but absent"
// and "malformed" is worth denying to anyone enumerating accounts.
func ParseRef(raw string) (Ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Ref{}, ErrNotFound
	}
	if id, err := uuid.Parse(raw); err == nil {
		return Ref{ID: id}, nil
	}
	if users.ValidateUsername(raw) != "" {
		return Ref{}, ErrNotFound
	}
	return Ref{Username: raw}, nil
}

// ---------------------------------------------------------------------------
// Stored shapes
// ---------------------------------------------------------------------------

// Award is one unlocked achievement as stored.
type Award struct {
	Key           string
	UnlockedAt    time.Time
	SeenAt        *time.Time
	ShowcaseOrder *int
}

// Progress is one tally row as stored. Actors is the distinct-account set for
// reader-driven keys, and is empty for everything else.
type Progress struct {
	Count  int
	LastAt *time.Time
	Actors []string
}

// counted reports whether this actor has already been counted for the key.
func (p Progress) counted(actor uuid.UUID) bool {
	needle := actor.String()
	for _, id := range p.Actors {
		if id == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Preferences - the ai_prefs pattern (Part 3 "A global off switch")
// ---------------------------------------------------------------------------

// Prefs are the stored switches. A pointer so an unset field falls through to
// the default rather than silently meaning false.
type Prefs struct {
	// Enabled is the global off switch. Off means no counting, no strip, and
	// no profile section - some writers find this sort of thing cheapens the
	// work, and that view is respected in full.
	Enabled *bool `json:"enabled,omitempty"`
}

// EffectivePrefs is every switch resolved: defaults ← user.
type EffectivePrefs struct {
	Enabled bool `json:"enabled"`
}

func defaultPrefs() EffectivePrefs { return EffectivePrefs{Enabled: true} }

func (e *EffectivePrefs) apply(p *Prefs) {
	if p == nil {
		return
	}
	if p.Enabled != nil {
		e.Enabled = *p.Enabled
	}
}

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

// EggCount is the ONLY thing anyone is ever told about eggs they have not
// found: how many, out of how many. "ปลดล็อกแล้ว 3 / ??" - naming one, or
// describing how to get it, kills it instantly.
type EggCount struct {
	Unlocked int `json:"unlocked"`
	Total    int `json:"total"`
}

// OwnerEntry is one achievement as its owner sees it: the copy, the tally, and
// - for an egg they have actually found - the trigger and the message, so they
// can tell somebody.
type OwnerEntry struct {
	Key         string `json:"key"`
	Family      Family `json:"family"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`

	// Count and Threshold are the progress line. Never a score: they are
	// per-achievement and there is no total anywhere in this response.
	Count     int `json:"count"`
	Threshold int `json:"threshold"`

	Unlocked      bool       `json:"unlocked"`
	UnlockedAt    *time.Time `json:"unlocked_at,omitempty"`
	SeenAt        *time.Time `json:"seen_at,omitempty"`
	ShowcaseOrder *int       `json:"showcase_order,omitempty"`

	// Trigger and Message are present only for an UNLOCKED egg.
	Trigger string `json:"trigger,omitempty"`
	Message string `json:"message,omitempty"`

	// Showcaseable says whether this may be chosen for the public profile.
	// False for every egg, because a visitor must never see one named.
	Showcaseable bool `json:"showcaseable"`
}

// OwnerView is GET /me/achievements: everything, with progress.
type OwnerView struct {
	// Enabled is the global switch. False means the rest is deliberately
	// empty - nothing was counted and nothing is shown.
	Enabled bool `json:"enabled"`

	// Achievements is every path and identity achievement, plus the eggs the
	// owner has already found. A LOCKED egg is not here at all: found, never
	// announced.
	Achievements []OwnerEntry `json:"achievements"`

	// Eggs is the blank-slot count, the only hint a locked egg ever gives.
	Eggs EggCount `json:"eggs"`

	// ShowcaseMin and ShowcaseMax bound the owner's choice, so the manager
	// does not have to hard-code the rule.
	ShowcaseMin int `json:"showcase_min"`
	ShowcaseMax int `json:"showcase_max"`
}

// PublicEntry is one achievement as a visitor sees it. There is no progress
// here: how close somebody is to something is their own business.
type PublicEntry struct {
	Key         string    `json:"key"`
	Family      Family    `json:"family"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	UnlockedAt  time.Time `json:"unlocked_at"`
}

// PublicView is GET /users/:user/achievements: what the owner chose to show,
// plus counts. Never an egg's name, whatever the owner chose.
type PublicView struct {
	// Enabled false means the owner turned the whole system off; the profile
	// then shows no achievement section at all.
	Enabled bool `json:"enabled"`

	// Showcase is the 3-5 the owner picked, in their order.
	Showcase []PublicEntry `json:"showcase"`

	// Unlocked and Total are the medal grid's numerator and denominator over
	// the LISTED families. Locked slots are rendered from the difference.
	Unlocked int `json:"unlocked"`
	Total    int `json:"total"`

	// Eggs is a count and nothing else, always.
	Eggs EggCount `json:"eggs"`
}

// SignalResult answers POST /achievements/signal. It carries the unlock so the
// browser can show its strip without a second request - and so the strip can
// wait for a moment that is not mid-sentence.
type SignalResult struct {
	// Recorded is false when the switch is off or the cooldown rejected it.
	Recorded bool `json:"recorded"`
	// Unlocked is the achievement this signal completed, or nil.
	Unlocked *UnlockedView `json:"unlocked,omitempty"`
}

// UnlockedView is what the dismissible strip renders.
type UnlockedView struct {
	Key     string `json:"key"`
	Family  Family `json:"family"`
	Title   string `json:"title"`
	Trigger string `json:"trigger,omitempty"`
	Message string `json:"message,omitempty"`
}

// Showcase bounds (Part 3 "The profile showcases 3-5").
const (
	ShowcaseMin = 3
	ShowcaseMax = 5
)
