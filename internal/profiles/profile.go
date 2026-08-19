// Package profiles owns the PUBLIC read of a person: who they are, and what
// their work adds up to (Phase 12E, docs/PHASE-12-STORY-DEPTH.md §12E).
//
// It is its own package rather than a method on users because it composes two
// domains - identity (users, user_profiles, author_profiles, user_follows) and
// published work (novels) - and `novels` already depends on `users`. Putting
// the composition here keeps that dependency running one way, and keeps the
// identity package free of anything that knows what a fiction is.
//
// Everything it publishes is deliberately viewer-INDEPENDENT: the same bytes
// for a guest, a stranger, and the person themselves. That is what lets one
// cached response serve every visitor (docs/14 §7). Whether the caller follows
// this person is a separate, personal read (`/users/:user/follow-status`).
package profiles

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/users"
)

// RefParam is the path parameter name for a user reference. It matches the
// follow endpoints' parameter so both can share the `/users/:user` segment.
const RefParam = "user"

// Ref identifies a user in a URL: a UUID or a username.
//
// docs/08 §35's rule for fictions applies here for the same reason - public
// URLs read better with the name, internal links already hold the id, and both
// resolve to the same row.
type Ref struct {
	ID       uuid.UUID
	Username string
}

// ByUsername reports whether the reference is a name rather than a UUID.
func (r Ref) ByUsername() bool { return r.ID == uuid.Nil }

// ParseRef interprets a path parameter.
//
// A malformed reference is ErrNotFound rather than a parse error: telling a
// caller that an identifier is well-formed but absent, versus malformed, is a
// distinction worth denying to anyone enumerating accounts (docs/11 §3.4).
func ParseRef(raw string) (Ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Ref{}, ErrNotFound
	}
	if id, err := uuid.Parse(raw); err == nil {
		return Ref{ID: id}, nil
	}
	// A name that could never have been registered - wrong shape, wrong
	// length, or reserved - cannot name an account, so it gets the same 404 an
	// unknown name gets rather than a different, more informative error.
	if users.ValidateUsername(raw) != "" {
		return Ref{}, ErrNotFound
	}
	return Ref{Username: raw}, nil
}

// Profile is one person as everyone else may see them.
//
// It is deliberately a SEPARATE type from users.PrivateUser rather than a
// subset of it: a field added to the account view cannot appear here by
// accident, which is the same reason users.PublicUser exists at all
// (docs/08 §1.4, docs/10 §8). No email, no role, no account status, no
// verification state.
type Profile struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`
	BannerURL   *string   `json:"banner_url,omitempty"`
	Bio         *string   `json:"bio,omitempty"`
	WebsiteURL  *string   `json:"website_url,omitempty"`

	// Links are the writer's own published contacts. Always an array, never
	// null, so a client can render it without a guard.
	Links []Link `json:"links"`

	// JoinedAt is the account's creation date. It is already implied by every
	// piece of work the person has published, so it discloses nothing new.
	JoinedAt time.Time `json:"joined_at"`

	// IsAuthor reports that an author profile exists. PenName and AuthorBio are
	// that profile's public half; DonationURL is the writer's own external
	// support link (docs/MONETIZATION.md §6).
	//
	// There is exactly ONE pen name per user - author_profiles is keyed by
	// user_id (docs/08 §6.3) - so this is an attribute of the person, not a
	// collection to page through.
	IsAuthor    bool    `json:"is_author"`
	PenName     *string `json:"pen_name,omitempty"`
	AuthorBio   *string `json:"author_bio,omitempty"`
	DonationURL *string `json:"donation_url,omitempty"`

	// OpenFor is what this writer says they are taking right now. A status the
	// writer sets and clears; the platform brokers none of it.
	OpenFor []string `json:"open_for"`

	// Boundaries is คำเตือน/ขอบเขตของนักเขียน - what this writer will and will
	// not write, in their own words. Displayed verbatim and never parsed: the
	// platform maintains no list of what a person may decline, and any list it
	// did maintain would be wrong for somebody.
	Boundaries *string `json:"boundaries,omitempty"`

	// WallEnabled is whether this person accepts messages on their profile.
	// Public because the page has to know whether to ask for the wall at all -
	// and because a visitor deserves to see that it is closed rather than that
	// it is broken.
	WallEnabled bool `json:"wall_enabled"`

	// HideFromRankings is the writer's choice to stay out of the home page's
	// writer rankings (docs/WRITER-SPOTLIGHT.md). On the public view for the
	// same reason novels.hide_counts is: it is the absence other pages must
	// respect, and the settings screen edits exactly what this read publishes.
	HideFromRankings bool `json:"hide_from_rankings"`

	// PenNames are the identities this writer publishes under
	// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2). Always an array, never null.
	//
	// Distinct from PenName above, which is the single author_profiles field:
	// that one is the heading of the page, these are the several names a writer
	// may put on different work. Both are public - they are printed on the
	// covers - and both are the same for every viewer.
	PenNames []PenNameView `json:"pen_names"`

	// FormerNames are the names this person used within the last thirty days
	// and no longer uses - the «เคยใช้ชื่อ …» line.
	//
	// A pen name is changeable and a handle is not; this window is what makes a
	// change VISIBLE for a while, so a name being taken over can be noticed. It
	// is deliberately not a permanent record.
	FormerNames []string `json:"former_names"`

	// Pinned is the writer's own shelf of up to three works, in their order.
	// Re-checked for readability at read time, so an unpublished pin is simply
	// absent rather than a leaked title.
	Pinned []PinnedWork `json:"pinned"`

	NovelCount    int64 `json:"novel_count"`
	// CompletedCount is how many of those are finished stories - the number a
	// reader decides by (profile review 2026-08).
	CompletedCount int64 `json:"completed_count"`
	FollowerCount int64 `json:"follower_count"`

	// TotalViews sums the readership of the work a stranger can actually open.
	// A private draft has no readers, so it contributes nothing here - for the
	// owner too, because a number that changed depending on who was looking
	// would not be the same claim (§12C).
	TotalViews int64 `json:"total_views"`
}

// PenNameView is one of a writer's identities as everyone else sees it.
//
// Declared here rather than reused from the pennames package for the same
// reason Profile is not a subset of users.PrivateUser: this is the PUBLIC half,
// and a field added to the owner's editor view must not be able to appear on a
// stranger's page by accident (docs/08 §1.4). Timestamps in particular are the
// writer's own business and stay off it.
type PenNameView struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Note *string `json:"note,omitempty"`
	// IsDefault is the ค่าเริ่มต้น chip: which name a work that named none of
	// its own is published under.
	IsDefault bool `json:"is_default"`
}

// PinnedWork is one of the three works a writer chose to put at the top of
// their own profile, with their one line about it
// (docs/PROFILE-AND-ACHIEVEMENTS.md).
//
// A profile ordered only by recency answers "what did you write last"; a new
// reader is asking "where do I start", and only the writer can answer that.
type PinnedWork struct {
	NovelID string  `json:"novel_id"`
	Slug    string  `json:"slug"`
	Title   string  `json:"title"`
	Note    *string `json:"note,omitempty"`
}
