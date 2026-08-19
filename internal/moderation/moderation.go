// Package moderation implements user reports and the moderator audit trail
// (docs/08 §24 and §44 Phase 8, docs/09 §28–§29, docs/01 §21, docs/11 §38–§39).
//
// The package owns the two Phase 8 tables - reports and moderation_actions -
// and NOTHING else. It never applies a state change itself: hiding a comment,
// removing a post, or suspending a user always goes through the owning
// domain's service, which stays the single authorization boundary for its own
// rows (docs/10 §27). What this package adds is the queue in front of those
// changes and the append-only record behind them.
package moderation

import (
	"time"

	"github.com/google/uuid"
)

// TargetType names what a report or an action points at. The vocabulary is
// docs/11 §38's complete reportable list - media joined in Phase 9 when its
// table arrived - spelled exactly like the notification entity types so a
// client resolves both the same way.
type TargetType string

const (
	TargetNovel            TargetType = "novel"
	TargetChapter          TargetType = "chapter"
	TargetComment          TargetType = "comment"
	TargetCommunityPost    TargetType = "community_post"
	TargetCommunityComment TargetType = "community_comment"
	TargetUser             TargetType = "user"
	TargetMedia            TargetType = "media"
)

// TargetTypes returns the reportable target vocabulary.
func TargetTypes() []string {
	return []string{
		"novel", "chapter", "comment", "community_post", "community_comment", "user", "media",
	}
}

// ValidTargetType reports whether a value is allowlisted.
func ValidTargetType(t string) bool {
	switch TargetType(t) {
	case TargetNovel, TargetChapter, TargetComment,
		TargetCommunityPost, TargetCommunityComment, TargetUser, TargetMedia:
		return true
	}
	return false
}

// Reason is the reporter's category. The column is VARCHAR (docs/08 §24.1
// does not enumerate it); the allowlist is docs/01 §21's list of what the
// platform must handle - spam, harassment, copyright complaints, illegal
// content, abuse, and AI-generated spam.
type Reason string

const (
	ReasonSpam       Reason = "spam"
	ReasonHarassment Reason = "harassment"
	ReasonCopyright  Reason = "copyright"
	ReasonIllegal    Reason = "illegal"
	ReasonAbuse      Reason = "abuse"
	ReasonAISpam     Reason = "ai_spam"
)

// Reasons returns the report-reason vocabulary, for clients building the
// "Select reason" step of docs/02 §38.
func Reasons() []string {
	return []string{"spam", "harassment", "copyright", "illegal", "abuse", "ai_spam"}
}

// ValidReason reports whether a value is allowlisted.
func ValidReason(r string) bool {
	switch Reason(r) {
	case ReasonSpam, ReasonHarassment, ReasonCopyright,
		ReasonIllegal, ReasonAbuse, ReasonAISpam:
		return true
	}
	return false
}

// MaxDescriptionRunes bounds the optional free-text detail. Runes, not bytes -
// Thai text must not get a third of the room (the same rationale as every
// other text limit in the codebase).
const MaxDescriptionRunes = 2000

// MaxActionReasonRunes bounds a moderator's note on an action.
const MaxActionReasonRunes = 2000

// Status is the report lifecycle (docs/08 §24.1 - enumerated, so also
// CHECKed in the database).
type Status string

const (
	StatusPending   Status = "pending"
	StatusReviewing Status = "reviewing"
	StatusResolved  Status = "resolved"
	StatusRejected  Status = "rejected"
)

// Statuses returns the lifecycle vocabulary.
func Statuses() []string { return []string{"pending", "reviewing", "resolved", "rejected"} }

// ValidStatus reports whether a value is allowlisted.
func ValidStatus(s string) bool {
	switch Status(s) {
	case StatusPending, StatusReviewing, StatusResolved, StatusRejected:
		return true
	}
	return false
}

// Terminal reports whether the lifecycle ends here. Resolved and rejected
// reports never reopen - a fresh concern is a fresh report (which the
// duplicate guard permits once the old one is closed).
func (s Status) Terminal() bool { return s == StatusResolved || s == StatusRejected }

// transitions models the documented lifecycle explicitly (docs/08 §24.1:
// pending → reviewing → resolved / rejected). Skipping "reviewing" is legal -
// an obviously actionable or obviously bogus report needs no claim step - but
// nothing moves backwards and nothing leaves a terminal state.
var transitions = map[Status][]Status{
	StatusPending:   {StatusReviewing, StatusResolved, StatusRejected},
	StatusReviewing: {StatusResolved, StatusRejected},
}

// CanTransition reports whether from → to is a documented lifecycle step.
func CanTransition(from, to Status) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Action is what a moderator did (docs/08 §24.2, docs/02 §46). "No action"
// from the docs/02 flow is not an Action - it is rejecting the report, which
// lives on the report row, not in the audit table.
type Action string

const (
	ActionHide    Action = "hide"
	ActionRemove  Action = "remove"
	ActionRestore Action = "restore"
	ActionWarn    Action = "warn"
	ActionSuspend Action = "suspend"
	ActionBan     Action = "ban"
)

// actionMatrix maps each target type to the actions its EXISTING states can
// express (Phase 8 brief: exercise the states earlier phases created, never
// invent parallel ones):
//
//	comments / community content  status: visible|published / hidden / removed
//	users                         status: active / suspended / banned
//	novels / chapters             deleted_at only - they have no hidden state,
//	                              so hide is NOT offered; remove/restore toggle
//	                              the soft delete (the same capability staff
//	                              already hold through docs/09 §14.7)
var actionMatrix = map[TargetType][]Action{
	TargetNovel:            {ActionRemove, ActionRestore},
	TargetChapter:          {ActionRemove, ActionRestore},
	TargetComment:          {ActionHide, ActionRemove, ActionRestore},
	TargetCommunityPost:    {ActionHide, ActionRemove, ActionRestore},
	TargetCommunityComment: {ActionHide, ActionRemove, ActionRestore},
	TargetUser:             {ActionWarn, ActionSuspend, ActionBan, ActionRestore},
	// Media rides its deleted_at like novels and chapters - no hidden state.
	// A moderation removal keeps the stored object so restore is lossless.
	TargetMedia: {ActionRemove, ActionRestore},
}

// ValidAction reports whether the action exists at all.
func ValidAction(a string) bool {
	switch Action(a) {
	case ActionHide, ActionRemove, ActionRestore, ActionWarn, ActionSuspend, ActionBan:
		return true
	}
	return false
}

// ActionAllowed reports whether an action applies to a target type.
func ActionAllowed(target TargetType, action Action) bool {
	for _, allowed := range actionMatrix[target] {
		if allowed == action {
			return true
		}
	}
	return false
}

// ActionsFor returns the actions available for one target type, for the
// moderator UI's action panel.
func ActionsFor(target TargetType) []string {
	actions := actionMatrix[target]
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, string(a))
	}
	return out
}

// Card is the public identity shown beside a report or an action - the same
// public-profile slice every other card uses (docs/08 §1.4). It appears only
// on STAFF-facing views; a reporter's own view never carries anyone's card.
type Card struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`
}

// Report mirrors the reports table plus the joined staff-view cards.
type Report struct {
	ID         uuid.UUID
	ReporterID uuid.UUID

	TargetType TargetType
	TargetID   uuid.UUID

	Reason      Reason
	Description *string

	Status Status

	CreatedAt  time.Time
	ResolvedAt *time.Time
	ResolvedBy *uuid.UUID

	// Reporter and Resolver are joined for moderator views; the reporter's own
	// view renders neither.
	Reporter Card
	Resolver *Card
}

// View is the REPORTER's shape of their own report (docs/09 §28 GET
// /me/reports). docs/02 §38: a simple confirmation, no internal moderation
// details - so no resolver identity, ever.
type View struct {
	ID         uuid.UUID  `json:"id"`
	TargetType TargetType `json:"target_type"`
	TargetID   uuid.UUID  `json:"target_id"`

	Reason      Reason  `json:"reason"`
	Description *string `json:"description,omitempty"`

	Status Status `json:"status"`

	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Render builds the reporter-facing view.
func (r *Report) Render() View {
	return View{
		ID:          r.ID,
		TargetType:  r.TargetType,
		TargetID:    r.TargetID,
		Reason:      r.Reason,
		Description: r.Description,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt,
		ResolvedAt:  r.ResolvedAt,
	}
}

// ModeratorView is the staff shape: the report plus who filed it and who
// closed it (docs/11 §39 - the reporter identity is necessary moderation
// information; it never leaves the staff surface).
type ModeratorView struct {
	View
	Reporter Card  `json:"reporter"`
	Resolver *Card `json:"resolver,omitempty"`
}

// RenderForModerator builds the staff-facing view.
func (r *Report) RenderForModerator() ModeratorView {
	return ModeratorView{View: r.Render(), Reporter: r.Reporter, Resolver: r.Resolver}
}

// ActionRecord mirrors one moderation_actions row plus the joined moderator
// card. Append-only: there is no update or delete path anywhere in the
// package (docs/08 §24.2).
type ActionRecord struct {
	ID          uuid.UUID
	ModeratorID uuid.UUID

	TargetType TargetType
	TargetID   uuid.UUID

	Action Action
	Reason *string

	CreatedAt time.Time

	Moderator Card
}

// ActionView is the staff-facing audit entry.
type ActionView struct {
	ID         uuid.UUID  `json:"id"`
	Moderator  Card       `json:"moderator"`
	TargetType TargetType `json:"target_type"`
	TargetID   uuid.UUID  `json:"target_id"`
	Action     Action     `json:"action"`
	Reason     *string    `json:"reason,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Render builds the audit view.
func (a *ActionRecord) Render() ActionView {
	return ActionView{
		ID:         a.ID,
		Moderator:  a.Moderator,
		TargetType: a.TargetType,
		TargetID:   a.TargetID,
		Action:     a.Action,
		Reason:     a.Reason,
		CreatedAt:  a.CreatedAt,
	}
}

// TargetSnapshot is the compact, staff-only description of what a report
// points at, fetched live for the report detail page. It carries only what a
// moderator needs to judge the report (docs/11 §39 "access only information
// necessary"):
//
//   - short-form content (comments, posts) includes an excerpt - the content
//     IS what was reported;
//   - fiction includes metadata only, never chapters.content - moderators
//     read published fiction through the normal reader, and drafts are not
//     opened by a report (docs/11 §39 "not automatically gain unrestricted
//     access to private drafts").
type TargetSnapshot struct {
	Type   TargetType `json:"type"`
	ID     uuid.UUID  `json:"id"`
	Exists bool       `json:"exists"`

	// State is the target's CURRENT domain state, so the action panel can
	// offer only sensible actions:
	//
	//	novel/chapter        active | removed
	//	comment/community    visible|published | hidden | removed | deleted
	//	user                 active | pending_verification | suspended | banned
	State string `json:"state,omitempty"`

	Title   *string `json:"title,omitempty"`
	Excerpt *string `json:"excerpt,omitempty"`
	Author  *Card   `json:"author,omitempty"`
}

// ReportDetail is the staff report page: the report, what it points at now,
// and what has already been done to that target.
type ReportDetail struct {
	Report  ModeratorView   `json:"report"`
	Target  *TargetSnapshot `json:"target,omitempty"`
	History []ActionView    `json:"history"`
	Actions []string        `json:"available_actions"`
}
