// Package novels owns the Fiction entity: its lifecycle, its ownership, and its
// format metadata.
//
// The table is `novels` and the product term is "Fiction" (docs/08 §7.1,
// docs/09 §15); renaming the resource is a deliberate future API version rather
// than a casual change, so both names appear here on purpose.
//
// There is exactly ONE novel domain. One-shot, chat, and headcanon fictions are
// not subtypes - they are values of three independent format dimensions owned by
// the `fiction` package (docs/08 §7.3, §43 Rule 6). Nothing in this package
// branches on a combination of dimensions.
//
// Layering (docs/09 §44): Handler -> Service -> Repository -> PostgreSQL.
// Ownership is decided in the Service, never in HTTP middleware (docs/10 §27).
package novels

import (
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/internal/taxonomy"
)

// Status is the publication status of a fiction (docs/08 §7.1).
//
// It is independent of Visibility. Status says how far along the work is;
// Visibility says who may reach it.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusOngoing   Status = "ongoing"
	StatusCompleted Status = "completed"
	StatusHiatus    Status = "hiatus"
	StatusCancelled Status = "cancelled"
)

// DefaultStatus is what a newly created fiction receives. A fiction starts
// private and unpublished; publishing is an explicit later action (docs/11 §31).
const DefaultStatus = StatusDraft

func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusOngoing, StatusCompleted, StatusHiatus, StatusCancelled:
		return true
	}
	return false
}

// IsDraft reports whether the fiction is still unpublished work.
func (s Status) IsDraft() bool { return s == StatusDraft }

// Statuses returns every supported value, for API metadata and validation
// messages.
func Statuses() []Status {
	return []Status{StatusDraft, StatusOngoing, StatusCompleted, StatusHiatus, StatusCancelled}
}

// Visibility is who may reach the fiction (docs/08 §7.1).
//
// docs/11 §32 requires visibility to remain independent of format: "chat +
// private" stays private, and a format change never touches this field.
//
// It is a LADDER (§13C), widest to narrowest. Three values could only say
// "everyone", "anyone with the link", or "nobody"; the two rungs writers keep
// asking for are the same request - publish, but not to the open internet.
type Visibility string

const (
	// VisibilityPublic - readable and listed. ทุกคน.
	VisibilityPublic Visibility = "public"
	// VisibilityMembers - a signed-in reader. Still LISTED: the work is meant
	// to be found, and the gate belongs at the door rather than on the map.
	VisibilityMembers Visibility = "members"
	// VisibilityFollowers - readers who follow the author. NOT listed: work
	// whose audience is a named set has no business appearing in a browse
	// surface for people outside it.
	VisibilityFollowers Visibility = "followers"
	// VisibilityUnlisted - readable by anyone holding the link, but excluded
	// from listings and search.
	VisibilityUnlisted Visibility = "unlisted"
	// VisibilityPrivate - only the owner (and staff) may read it.
	VisibilityPrivate Visibility = "private"
)

// DefaultVisibility keeps new work private until the author says otherwise.
const DefaultVisibility = VisibilityPrivate

func (v Visibility) Valid() bool {
	switch v {
	case VisibilityPublic, VisibilityMembers, VisibilityFollowers,
		VisibilityUnlisted, VisibilityPrivate:
		return true
	}
	return false
}

// Listable reports whether work at this visibility may appear in a browse
// surface at all. It is the rung's own property, so a listing never has to
// enumerate the ladder for itself.
func (v Visibility) Listable() bool {
	return v == VisibilityPublic || v == VisibilityMembers
}

// Visibilities returns every supported value, widest first - the order the UI
// presents, so the ladder reads downward into narrowness.
func Visibilities() []Visibility {
	return []Visibility{
		VisibilityPublic, VisibilityMembers, VisibilityFollowers,
		VisibilityUnlisted, VisibilityPrivate,
	}
}

// Audience is what a VIEWER brings to the visibility check (§13C).
//
// It exists so the rule stays one function: the caller resolves these two facts
// however it can - from the request identity, from a follow lookup - and the
// decision itself is made in exactly one place.
type Audience struct {
	// SignedIn is whether the viewer holds an account. Nothing more: the
	// members rung asks for an account, not for a particular one.
	SignedIn bool
	// Follows is whether the viewer follows this fiction's author. Resolved
	// only when the fiction actually asks, so the ordinary read costs nothing.
	Follows bool
}

// GuestAudience is what a reader with no account brings.
var GuestAudience = Audience{}

// AgeRating is the author's statement about their own work
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13B).
//
// It is required on create - the only field on the six-field create form that
// is not either unavoidable (a title) or immediately visible in the editor
// (structure, presentation). It is required because it decides where the work
// may appear at all, which is not a question that can be answered later without
// the answer having already mattered.
type AgeRating string

const (
	// RatingGeneral - no gate.
	RatingGeneral AgeRating = "general"
	// RatingTeen - 15+. A dismissible warning before reading, never a sign-in.
	RatingTeen AgeRating = "teen"
	// RatingMature - 18+. Gated by AgeGate, and excluded from listings.
	RatingMature AgeRating = "mature"
	// RatingExplicit - 18+ เนื้อหาทางเพศชัดเจน. Split from RatingMature because
	// one value had to serve two very different works: a fiction that is
	// violent or bleak, and one that is explicitly sexual. Treating them
	// identically means either the first is gated harder than it needs to be or
	// the second is gated softer than the platform can defend.
	//
	// A signed-in reader is required for this rating ALWAYS - a platform rule,
	// not an author setting, which is why it lives here rather than in AgeGate.
	RatingExplicit AgeRating = "explicit"
)

// DefaultAgeRating exists for rows that predate the column. New fictions must
// state a rating; the API does not fall back to this.
const DefaultAgeRating = RatingGeneral

func (r AgeRating) Valid() bool {
	switch r {
	case RatingGeneral, RatingTeen, RatingMature, RatingExplicit:
		return true
	}
	return false
}

// Adult reports whether this rating is 18+ in either of its two forms. It is
// the question almost every caller actually has - the gate, the attestation,
// the listing exclusion - so nothing outside this file enumerates the pair.
func (r AgeRating) Adult() bool { return r == RatingMature || r == RatingExplicit }

// RestrictsListing reports whether work with this rating is kept out of
// listings, search, and recommendations by default (§13B).
func (r AgeRating) RestrictsListing() bool { return r.Adult() }

// NeverListed reports whether work with this rating stays out of browse
// surfaces even for a reader who asked to see adult work.
//
// Explicit work is reachable by link and by the author's own page; it is not
// something a reader can stumble into from a listing, and there is no switch
// that makes it so (§13B).
func (r AgeRating) NeverListed() bool { return r == RatingExplicit }

// AgeRatings returns every supported value.
func AgeRatings() []AgeRating {
	return []AgeRating{RatingGeneral, RatingTeen, RatingMature, RatingExplicit}
}

// AgeGate is how 18+ work is gated - the WRITER's choice between protection and
// reach (§13B).
//
// It is stored whatever the current rating is, so moving a work to 18+ and back
// does not lose the setting, and it is consulted only when the rating is
// mature.
type AgeGate string

const (
	// GateWarning - a warning shown before every read. Guests included: this is
	// what keeps "ไม่ต้องสมัคร" reachable for work whose author wants it
	// reachable. It is honest about being a warning and nothing more.
	GateWarning AgeGate = "warning"
	// GateLogin - a signed-in reader. The rung the ladder was missing: the gap
	// between "click to continue" and "send us your ID" is enormous, and this
	// is where most 18+ work actually belongs.
	GateLogin AgeGate = "login"
	// GateVerified - only readers who completed identity verification. The
	// document itself is never retained; see §13B.
	GateVerified AgeGate = "verified"
)

// DefaultAgeGate is the widest. A writer who wants a stricter gate says so;
// nobody is silently locked out of work by a default.
const DefaultAgeGate = GateWarning

func (g AgeGate) Valid() bool {
	switch g {
	case GateWarning, GateLogin, GateVerified:
		return true
	}
	return false
}

// AgeGates returns every supported value, widest first.
func AgeGates() []AgeGate { return []AgeGate{GateWarning, GateLogin, GateVerified} }

// DefaultGateFor is the gate a rating starts at when the author names none.
//
// Explicit work starts at GateLogin rather than GateWarning because the
// platform will not serve it behind a dismissible warning at all. Defaulting is
// not the same as overriding: an author who explicitly asks for warning on
// explicit work is REFUSED with a field error rather than quietly upgraded,
// because a control that is silently ignored is the dishonesty §13E rules out.
func DefaultGateFor(rating AgeRating) AgeGate {
	if rating == RatingExplicit {
		return GateLogin
	}
	return DefaultAgeGate
}

// GateSatisfies reports whether a gate meets the minimum this rating demands.
func GateSatisfies(rating AgeRating, gate AgeGate) bool {
	if rating != RatingExplicit {
		return true
	}
	return gate == GateLogin || gate == GateVerified
}

// OriginType separates the two worlds a fiction search has to keep apart
// (§13A): work written from an existing source, and work invented whole.
type OriginType string

const (
	OriginOriginal    OriginType = "original"
	OriginFanfiction  OriginType = "fanfiction"
	DefaultOriginType            = OriginOriginal
)

func (o OriginType) Valid() bool {
	switch o {
	case OriginOriginal, OriginFanfiction:
		return true
	}
	return false
}

// OriginTypes returns every supported value.
func OriginTypes() []OriginType { return []OriginType{OriginOriginal, OriginFanfiction} }

// Novel is the fiction record; it mirrors the `novels` table.
type Novel struct {
	ID       uuid.UUID
	AuthorID uuid.UUID

	Title       string
	Slug        string
	Description *string
	/*
	 * Tagline and Foreword (13S).
	 *
	 * คำโปรย is the one line that has to work on a card, under a cover, in a
	 * listing. บทนำ is what the author says before the story starts - content
	 * notes, a dedication, where an AU diverges. Neither is the synopsis, and
	 * both were being written INTO the synopsis or into chapter one because
	 * there was nowhere else for them.
	 */
	Tagline  *string
	Foreword *string
	CoverURL *string

	// Format is the fiction's format state: the three independent dimensions
	// plus whether chapters may declare their own presentation (§13J). Stored
	// as separate columns and never collapsed into one enum.
	Format fiction.Format

	Status         Status
	Visibility     Visibility
	ContentWarning *string
	// ContentWarningSpoiler folds the warning behind a reader-operated button
	// (13U): a warning names what happens in the story, and for some stories
	// that IS the spoiler. The author decides; the reader still gets the
	// warning, one click away, before any text.
	ContentWarningSpoiler bool

	// Creation fields (Phase 13A). AgeRating is stated by the author and
	// decides discoverability; AgeGate is their choice of how 18+ work is
	// gated and matters only when the rating is mature; OriginType and Fandom
	// separate fanfiction from original work in search.
	AgeRating  AgeRating
	AgeGate    AgeGate
	OriginType OriginType
	Fandom     *string

	// ตั้งค่าเพิ่มเติม (Phase 13K): language, chapter unit, author notes,
	// series, the comment switch, and the author's stated permissions.
	Extras Extras

	// y/n reader-insert (Phase 12B, docs/PHASE-12-STORY-DEPTH.md §12B).
	//
	// Reader variables live in their own table since 13H - see
	// internal/variables. They were novels.yn_enabled/yn_token in 12B; the
	// migration carried every existing declaration across.

	// Engagement counters (Phase 12C, docs/PHASE-12-STORY-DEPTH.md §12C).
	//
	// Denormalised DISPLAY numbers, maintained by the events that produce them:
	// bookmarks and likes in the same statement as the row they count, views by a
	// buffered writer. They are not an analytics record and there is no per-reader
	// row behind them - docs/11 §34 does not permit building a reading history.
	ViewCount     int64
	LikeCount     int
	BookmarkCount int

	// HideCounts keeps the hearts/views scoreboard off the fiction's public
	// face (13U). The counters keep counting - the writer still sees them in
	// the studio - readers simply are not shown numbers the author does not
	// want their story judged by.
	HideCounts bool

	// ShowDonate is whether the author's support link (a profile-level
	// setting) is offered on THIS fiction. Default true.
	ShowDonate bool

	// ThemeColor is the fiction page's accent, lowercase #rrggbb or nil (13U).
	ThemeColor *string

	// PublishAt is a scheduled first publish (13U). Read-time semantics, the
	// same shape as chapter schedules: the row may already say public, but
	// ReadableBy and every listing predicate refuse it before its time. The
	// readiness and verification gates ran when the schedule was SET - the
	// moment of decision - not at the moment of appearance.
	PublishAt *time.Time

	// CollaboratorIDs is who may edit this fiction's CONTENT besides the
	// author (13U). Loaded by the repository's Find/FindRecord, so every
	// authorization decision sees it. Collaborators are never owners: settings,
	// visibility, publishing, deletion, and the collaborator list itself stay
	// with the author.
	CollaboratorIDs []uuid.UUID

	PublishedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Rights is the author's stated permissions (§13E).
//
// DECLARATIONS, not enforcement. Preventing a screenshot is not technically
// possible, so these are rendered to readers as "what the author allows" and
// the platform never claims to be stopping anything (docs/11 §43). A control
// that cannot be enforced is a declaration, and must say so.
type Rights struct {
	AllowScreenshot  bool `json:"allow_screenshot"`
	AllowTranslation bool `json:"allow_translation"`
	AllowDerivative  bool `json:"allow_derivative"`
	AllowAudio       bool `json:"allow_audio"`
	RequireCredit    bool `json:"require_credit"`
	// DerivativeTerms is the author's own condition beside AllowDerivative.
	DerivativeTerms *string `json:"derivative_terms,omitempty"`
}

// Extras groups the ตั้งค่าเพิ่มเติม fields (§13K) - the collapsed section the
// create form was always specified to have.
//
// Grouped rather than spread across the struct because they share one property:
// every one of them is answerable later, and none of them blocks writing.
type Extras struct {
	Language    string `json:"language"`
	ChapterUnit string `json:"chapter_unit"`

	AuthorNoteStart *string `json:"author_note_start,omitempty"`
	AuthorNoteEnd   *string `json:"author_note_end,omitempty"`

	SeriesName     *string `json:"series_name,omitempty"`
	SeriesPosition *int    `json:"series_position,omitempty"`

	// CommentAccess is the three-level switch (§13D); CommentApproval holds
	// member comments for review. Guest comments are held whatever it says -
	// see CommentAccess.
	CommentAccess   CommentAccess `json:"comment_access"`
	CommentApproval bool          `json:"comment_approval"`

	Rights Rights `json:"rights"`
}

// CommentAccess is who may add to a fiction's thread (§13D).
//
// It replaced a single boolean, because one boolean could not express the thing
// this platform is for. "ไม่ต้องสมัครก็อ่านได้" is a promise about READING, and
// the thread under a fiction is exactly where a guest most wants to say
// something - an account requirement there is where they leave.
type CommentAccess string

const (
	// CommentsEveryone - guests may comment, with a name they type.
	//
	// Their comments are held for the author's approval ALWAYS, whatever
	// CommentApproval says: a guest comment carries no account to warn,
	// suspend, or hold responsible. That is not a hedge, it is what makes the
	// level survivable - a door opened once, met by a drive-by, and closed
	// permanently would be worse than never opening it.
	CommentsEveryone CommentAccess = "everyone"
	// CommentsMembers - a signed-in account is required. What the old boolean
	// meant by `true`; it never meant guests could.
	CommentsMembers CommentAccess = "members"
	// CommentsOff - nobody may add to the thread. Existing comments stay: the
	// switch closes the door, it does not empty the room.
	CommentsOff CommentAccess = "off"
)

// DefaultCommentAccess matches what every existing fiction already had.
const DefaultCommentAccess = CommentsMembers

func (a CommentAccess) Valid() bool {
	switch a {
	case CommentsEveryone, CommentsMembers, CommentsOff:
		return true
	}
	return false
}

// CommentAccesses returns every supported value, widest first.
func CommentAccesses() []CommentAccess {
	return []CommentAccess{CommentsEveryone, CommentsMembers, CommentsOff}
}

// AllowsGuests reports whether a reader with no account may post here.
func (a CommentAccess) AllowsGuests() bool { return a == CommentsEveryone }

// Open reports whether the thread accepts anything at all.
func (a CommentAccess) Open() bool { return a != CommentsOff }

// DefaultExtras is what a fiction gets when the writer never opens the section.
func DefaultExtras() Extras {
	return Extras{
		Language:      DefaultLanguage,
		ChapterUnit:   DefaultChapterUnit,
		CommentAccess: DefaultCommentAccess,
		Rights: Rights{
			// The permissive defaults are the ones that match what readers
			// already do and what most writers already want: quoting is how a
			// fiction is shared, and credit is the ask that costs nothing.
			AllowScreenshot: true,
			RequireCredit:   true,
		},
	}
}

// ReadableBy reports whether someone who is NOT the owner may open this
// fiction.
//
// A draft is never readable this way, whatever its visibility - docs/08 §7.1:
// "A draft should never become publicly readable simply because the chapter has
// a public URL."
//
// The switch is an ALLOWLIST, and that is the point: a visibility value this
// build does not recognise falls to the default and is refused. The predicate
// it replaced was `visibility != 'private'`, which would have published every
// new rung the moment one was added to the column (§13C).
func (n *Novel) ReadableBy(audience Audience) bool {
	if n.DeletedAt != nil || n.Status.IsDraft() {
		return false
	}
	// A scheduled first publish has not happened yet (13U). The owner and
	// their collaborators still reach it through canManage/EditableBy - this
	// predicate only ever answers for everyone else.
	if n.PublishAt != nil && n.PublishAt.After(time.Now()) {
		return false
	}
	switch n.Visibility {
	case VisibilityPublic, VisibilityUnlisted:
		return true
	case VisibilityMembers:
		return audience.SignedIn
	case VisibilityFollowers:
		return audience.Follows
	default:
		return false
	}
}

// Readable is the GUEST tier of ReadableBy: reachable with no account at all.
//
// It is the right question for anything that has no viewer to speak of - a
// public count, a shared card - and the wrong one wherever an identity is in
// hand, which is why the service resolves an Audience instead.
func (n *Novel) Readable() bool { return n.ReadableBy(GuestAudience) }

// NeedsFollowCheck reports whether deciding readability for this fiction
// requires a follow lookup. It keeps the query off every ordinary read.
func (n *Novel) NeedsFollowCheck() bool {
	return n.DeletedAt == nil && !n.Status.IsDraft() && n.Visibility == VisibilityFollowers
}

// Listed reports whether this fiction may appear in a listing or search result.
// Unlisted work is reachable by link but must not be discoverable
// (docs/11 §31); followers-only work is narrower still.
func (n *Novel) Listed() bool {
	if n.PublishAt != nil && n.PublishAt.After(time.Now()) {
		return false
	}
	return n.DeletedAt == nil && !n.Status.IsDraft() && n.Visibility.Listable()
}

// OwnedBy reports whether the given user is the author.
//
// This is the ownership predicate; it is used by the service, which is the
// authorization boundary (docs/07 §14, docs/11 §8).
func (n *Novel) OwnedBy(userID uuid.UUID) bool {
	return userID != uuid.Nil && n.AuthorID == userID
}

// EditableBy reports whether the given user may edit this fiction's CONTENT:
// the author, or a collaborator (13U).
//
// It is deliberately NOT the ownership predicate. Settings, visibility,
// publishing, deletion, and the collaborator list answer to OwnedBy; this
// answers for chapters, characters, and variables - the work of co-writing.
func (n *Novel) EditableBy(userID uuid.UUID) bool {
	if n.OwnedBy(userID) {
		return true
	}
	if userID == uuid.Nil {
		return false
	}
	for _, id := range n.CollaboratorIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// Author is the public identity shown alongside a fiction.
//
// It carries only public profile fields; an email address must never appear
// here (docs/08 §1.4, docs/10 §8).
type Author struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`
	// DonationURL is the author's EXTERNAL writer-support link (e.g. EasyDonate),
	// shown to readers as a "support this writer" CTA (Phase 11, brief §6, §15).
	// It is public by design and entirely external - FictionThai never processes
	// this money. Omitted when the author has not set one.
	DonationURL *string `json:"donation_url,omitempty"`
}

// Record is a novel loaded together with the joined data every view needs.
//
// The author and the chapter counts come from the same query as the novel, so
// rendering a page of results costs one round trip rather than one per row
// (docs/07 §67 - the reader is the priority path).
type Record struct {
	Novel
	Author Author

	// PublishedChapters counts only chapters a non-owner may read. Exposing a
	// total that included drafts would leak how much unpublished work exists
	// (docs/11 §31).
	PublishedChapters int

	// HasMixedFormats reports whether any chapter renders as something other
	// than the fiction's own format - derived from the chapters that exist, so
	// the badge can never claim a mix the reader will not find (§13J).
	HasMixedFormats bool

	// TotalChapters includes drafts and is populated for the owner only.
	TotalChapters int

	// HasReaderVariables reports that the work carries at least one reader
	// variable (y/n). Derived from novel_variables with the record, so a search
	// card can badge it - and a reader can filter for it - without a request
	// per row (search review 2026-08 section B).
	HasReaderVariables bool

	// FirstChapterSlug addresses the first chapter a non-owner may read, so a
	// result card can offer "อ่านตอนแรก" as one click instead of three
	// (search review section D6). Nil when nothing is published.
	FirstChapterSlug *string

	// PenNameID is the identity the author explicitly chose for this work, and
	// PenName is the RESOLVED name readers see - that choice, or the author's
	// default when the work named none (docs/PROFILE-AND-ACHIEVEMENTS.md
	// Part 2). Both are loaded with the record, so no page of cards costs a
	// query per row to learn whose name is on it.
	//
	// A work whose author keeps no pen names simply has none, and the author's
	// display name stands as it always did.
	PenNameID *uuid.UUID
	PenName   *string

	// Genres and Tags are the fiction's discovery metadata (docs/08 §14, §15),
	// attached by the service after the row loads. Distinct from the three
	// format dimensions, which are first-class columns, never terms
	// (docs/08 §15.2).
	Genres []taxonomy.Term
	Tags   []taxonomy.Term
}

// View is the API representation of a fiction (docs/09 §14.5).
//
// The format fields are flattened onto the resource by embedding
// fiction.Format anonymously, so clients read `story_structure` directly rather
// than reaching into a nested object.
//
// Owner-only fields are pointers so they are omitted entirely for a reader,
// rather than being sent as a zero value that a client might misread.
type View struct {
	ID   uuid.UUID `json:"id"`
	Slug string    `json:"slug"`

	Title       string  `json:"title"`
	Description *string `json:"description,omitempty"`
	// คำโปรย and บทนำ (13S). Sent to every reader: the tagline is what a CARD
	// shows, and the foreword is part of the work.
	Tagline  *string `json:"tagline,omitempty"`
	Foreword *string `json:"foreword,omitempty"`
	CoverURL *string `json:"cover_url,omitempty"`

	fiction.Format

	Status         Status  `json:"status"`
	ContentWarning *string `json:"content_warning,omitempty"`
	// ContentWarningSpoiler tells the reader UI to fold the warning behind a
	// button (13U). Public: it changes how every reader is shown the warning.
	ContentWarningSpoiler bool `json:"content_warning_spoiler,omitempty"`

	// ThemeColor is the fiction page's accent, when the author chose one (13U).
	ThemeColor *string `json:"theme_color,omitempty"`

	// CountsHidden reports that the author keeps the scoreboard off this
	// fiction (13U). When true for a non-owner, the counter fields below are
	// zeroed - the flag exists so a client renders NOTHING rather than "0".
	CountsHidden bool `json:"counts_hidden,omitempty"`

	// Collaborators is the fiction's public co-writer credit (13U). Attached
	// on the single-fiction view; empty on cards.
	Collaborators []CollaboratorCredit `json:"collaborators,omitempty"`

	// Creation fields (docs/PHASE-13-CREATION-AND-CONTROL.md §13A). Always
	// sent: a card has to badge the rating, and a reader deciding whether to
	// open a work should not need a second request to learn it is 18+.
	//
	// AgeGate is sent too, so a client knows which gate to present before the
	// reader commits to opening anything.
	AgeRating  AgeRating  `json:"age_rating"`
	AgeGate    AgeGate    `json:"age_gate"`
	OriginType OriginType `json:"origin_type"`

	// ตั้งค่าเพิ่มเติม (§13K). Flattened onto the resource so a client reads
	// `chapter_unit` directly, the same shape the format dimensions use.
	Extras
	Fandom *string `json:"fandom,omitempty"`

	// Engagement counters (docs/PHASE-12-STORY-DEPTH.md §12C). Always sent so a
	// card never has to decide whether a missing number means zero.
	ViewCount     int64 `json:"view_count"`
	LikeCount     int   `json:"like_count"`
	BookmarkCount int   `json:"bookmark_count"`

	// Genres and Tags are always arrays (possibly empty), so clients can map
	// over them without a null check. They are discovery metadata; the format
	// dimensions above remain separate first-class fields (docs/08 §15.2).
	Genres []taxonomy.Term `json:"genres"`
	Tags   []taxonomy.Term `json:"tags"`

	Author Author `json:"author"`

	// PenName is the identity this work is published under - the author's
	// choice for this fiction, or their default (Part 2 of
	// docs/PROFILE-AND-ACHIEVEMENTS.md). Public, because it is the name printed
	// on the cover; absent when the writer keeps no pen names, and a client then
	// falls back to the author's display name exactly as before.
	PenName *string `json:"pen_name,omitempty"`

	// ChapterCount is what the viewer may actually reach: published chapters
	// for a reader, every chapter for the owner.
	ChapterCount int `json:"chapter_count"`

	// UsesChapterNavigation lets a client pick the right presentation without
	// re-implementing the rule (docs/09 §51 - clients must not invent their own
	// interpretation of format values).
	UsesChapterNavigation bool `json:"uses_chapter_navigation"`

	// HasMixedFormats is derived from the chapters that exist (§13J). The badge
	// reads "ผสมรูปแบบ" only when a reader would actually meet more than one.
	HasMixedFormats bool `json:"has_mixed_formats"`

	// HasReaderVariables is whether the work uses reader variables (y/n) -
	// derived from novel_variables, so a card can say so and the search page
	// can filter on it (search review 2026-08 section B).
	HasReaderVariables bool `json:"has_reader_variables"`

	// FirstChapterSlug is the first PUBLISHED chapter, for a one-click
	// "อ่านตอนแรก" on result cards (section D6). Absent when none is live.
	FirstChapterSlug *string `json:"first_chapter_slug,omitempty"`

	PublishedAt *time.Time `json:"published_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// --- owner-only ---------------------------------------------------------
	IsOwner bool `json:"is_owner"`
	// CanEdit is whether the VIEWER may edit this fiction's content: the
	// owner, or a collaborator (13U). The studio opens on it; ownership-only
	// surfaces (settings, publishing, deletion) still key off IsOwner.
	CanEdit    bool        `json:"can_edit,omitempty"`
	Visibility *Visibility `json:"visibility,omitempty"`
	// DraftChapterCount is the number of chapters the public cannot see.
	DraftChapterCount *int `json:"draft_chapter_count,omitempty"`
	// PublishAt is the scheduled first publish, for the studio (13U).
	PublishAt *time.Time `json:"publish_at,omitempty"`
	// The author's own display settings, echoed so the settings form can
	// render its switches without a second request (13U).
	HideCounts *bool `json:"hide_counts,omitempty"`
	ShowDonate *bool `json:"show_donate,omitempty"`
	// PenNameID is the author's EXPLICIT choice for this work, so the settings
	// form can show which identity is selected and tell "chosen" apart from
	// "falling back to my default". Owner-only: the choice is the writer's
	// business, the resulting name is everyone's.
	PenNameID *string `json:"pen_name_id,omitempty"`
}

// CollaboratorCredit is a co-writer as the public sees them (13U): a public
// profile identity plus the credit wording the author chose. Never an email,
// never an account id beyond the public username.
type CollaboratorCredit struct {
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
	Credit      string  `json:"credit,omitempty"`
}

// ViewFor renders the record for a particular viewer.
//
// This method is the single place that decides what a reader may see about a
// fiction, so a new column cannot leak by being added to Novel (docs/08 §1.4).
func (r Record) ViewFor(isOwner bool) View {
	view := View{
		ID:                    r.ID,
		Slug:                  r.Slug,
		Title:                 r.Title,
		Description:           r.Description,
		Tagline:               r.Tagline,
		Foreword:              r.Foreword,
		CoverURL:              r.CoverURL,
		Format:                r.Format,
		Status:                r.Status,
		ContentWarning:        r.ContentWarning,
		ContentWarningSpoiler: r.ContentWarningSpoiler,
		ThemeColor:            r.ThemeColor,
		AgeRating:             r.AgeRating,
		AgeGate:               r.AgeGate,
		OriginType:            r.OriginType,
		Extras:                r.Extras,
		HasMixedFormats:       r.HasMixedFormats,
		HasReaderVariables:    r.HasReaderVariables,
		FirstChapterSlug:      r.FirstChapterSlug,
		Fandom:                r.Fandom,
		ViewCount:             r.ViewCount,
		LikeCount:             r.LikeCount,
		BookmarkCount:         r.BookmarkCount,
		Genres:                orEmpty(r.Genres),
		Tags:                  orEmpty(r.Tags),
		Author:                r.Author,
		PenName:               r.PenName,
		ChapterCount:          r.PublishedChapters,
		UsesChapterNavigation: r.Format.UsesChapterNavigation(),
		PublishedAt:           r.PublishedAt,
		CreatedAt:             r.CreatedAt,
		UpdatedAt:             r.UpdatedAt,
		IsOwner:               isOwner,
		CanEdit:               isOwner,
	}

	// The author's support link is offered only where the author wants it
	// (13U). Blanked for every viewer including the owner, so the owner's
	// preview shows what readers actually get.
	if !r.ShowDonate {
		view.Author.DonationURL = nil
	}

	if isOwner {
		visibility := r.Visibility
		view.Visibility = &visibility
		view.ChapterCount = r.TotalChapters

		drafts := r.TotalChapters - r.PublishedChapters
		view.DraftChapterCount = &drafts

		view.PublishAt = r.PublishAt
		hideCounts, showDonate := r.HideCounts, r.ShowDonate
		view.HideCounts = &hideCounts
		view.ShowDonate = &showDonate

		if r.PenNameID != nil {
			id := r.PenNameID.String()
			view.PenNameID = &id
		}
	} else if r.HideCounts {
		// ซ่อนตัวเลข (13U): the numbers must not leave the server for a
		// reader, and the flag is what lets a client show nothing at all
		// rather than a row of zeros.
		view.ViewCount, view.LikeCount, view.BookmarkCount = 0, 0, 0
		view.CountsHidden = true
	}
	return view
}

// orEmpty turns a nil slice into an empty one so the JSON is always an array.
func orEmpty(terms []taxonomy.Term) []taxonomy.Term {
	if terms == nil {
		return []taxonomy.Term{}
	}
	return terms
}

// FormatView is the response body of PATCH /novels/:id/format (docs/09 §14.8).
//
// It returns the resulting format state and nothing else - the endpoint changes
// only metadata, so echoing the whole fiction would suggest otherwise.
type FormatView struct {
	ID uuid.UUID `json:"id"`
	fiction.Format

	// NeedsChatSetup warns that the fiction now presents as chat while its
	// chapters have no chat content prepared. It is a WARNING for the author
	// (docs/08 §11) and must never trigger an automatic conversion.
	NeedsChatSetup bool `json:"needs_chat_setup"`
}
