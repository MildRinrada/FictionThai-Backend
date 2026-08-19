// Package comments implements reader discussion on fictions and chapters
// (docs/08 §20, docs/09 §20 and §44 Phase 6 - the first half of the
// Interaction phase).
//
// Comments are user-generated content with explicit ownership (docs/08 §1.3
// "Comment → author"). They are NOT part of the fiction: a comment thread
// survives every format change untouched (docs/08 §3 "Changing a format must
// not delete comments") precisely because nothing here references formats.
//
// Community posts and their comments are a separate domain (§44 Phase 7) and
// are deliberately absent.
package comments

import (
	"time"

	"github.com/google/uuid"
)

// Status is the moderation state of a comment (docs/08 §20.1). It is distinct
// from soft deletion: deleted_at is the comment's author taking it back,
// status is the platform acting on it.
type Status string

const (
	StatusVisible Status = "visible"
	// StatusPending is "not decided yet" - waiting for the fiction's author
	// (§13D). It sits BESIDE hidden and removed rather than replacing either:
	// those are decisions, this is the absence of one.
	StatusPending Status = "pending"
	StatusHidden  Status = "hidden"
	StatusRemoved Status = "removed"
)

// Author is the public card shown beside a comment - the same public-profile
// slice every other card uses, never an email address (docs/08 §1.4).
type Author struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`
}

// Comment is the storage row plus its joined author card.
type Comment struct {
	ID uuid.UUID

	// UserID is nil for a GUEST comment (§13D). Exactly one of UserID and
	// GuestName is set, which the database enforces - a comment can never be
	// both and never neither.
	UserID *uuid.UUID
	// GuestName is the name a guest typed. It is a display string and nothing
	// more: it identifies nobody, it is not unique, and no token, email, or
	// address is stored beside it (docs/11 §34). The cost is that a guest
	// cannot edit or delete what they posted, and the form says so first.
	GuestName *string

	NovelID uuid.UUID

	// ChapterID is nil for a comment on the fiction itself (docs/08 §20.1
	// "Comments can belong to a novel or chapter").
	ChapterID *uuid.UUID

	// ParentID is nil for a top-level comment (docs/08 §20.1 threaded replies).
	ParentID *uuid.UUID

	// Content is PLAIN TEXT. It is stored raw and escaped at render time,
	// never interpreted as markup (docs/11 §16).
	Content string

	Status Status

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time

	// Author is nil for a guest comment - there is no account behind it and
	// therefore no card to show.
	Author *Author

	// ReplyCount counts visible replies; populated on top-level listings so the
	// UI can offer "ดูการตอบกลับ" without a second query per comment.
	ReplyCount int64

	// LikeCount and IsLiked are enriched per PAGE by the service (comment
	// design review 2026-08) - never per row.
	LikeCount int64
	IsLiked   bool
}

// WrittenBy reports whether this comment belongs to the given account. It is
// false for every guest comment, whoever is asking: an unauthenticated post
// carries no claim of authorship that the platform could honour later.
func (c *Comment) WrittenBy(userID uuid.UUID) bool {
	return userID != uuid.Nil && c.UserID != nil && *c.UserID == userID
}

// View is the API shape of one comment (docs/09 §7 envelope `data`).
type View struct {
	ID        uuid.UUID  `json:"id"`
	NovelID   uuid.UUID  `json:"novel_id"`
	ChapterID *uuid.UUID `json:"chapter_id,omitempty"`
	ParentID  *uuid.UUID `json:"parent_id,omitempty"`

	Content string `json:"content"`

	// Edited tells readers the text changed after posting - transparency
	// without exposing revision history.
	Edited bool `json:"edited"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Author is omitted for a guest comment, and GuestName is present instead.
	// Two fields rather than one synthesised card, so a client cannot render a
	// guest as if they were an account - there is no profile to link to, and a
	// name anybody may type must never look like an identity.
	Author    *Author `json:"author,omitempty"`
	GuestName *string `json:"guest_name,omitempty"`

	ReplyCount int64 `json:"reply_count"`

	// The heart (comment design review 2026-08): how many, and whether the
	// caller's own is among them.
	LikeCount int64 `json:"like_count"`
	IsLiked   bool  `json:"is_liked"`

	// IsOwner marks the caller's own comments so the UI can offer edit/delete
	// without re-deriving ownership client-side.
	IsOwner bool `json:"is_owner"`

	// Pending tells the poster their comment is waiting for the author's
	// review (§13D). It appears on the CREATE response and in the author's
	// queue; a reader listing never contains pending comments at all, so it is
	// omitted rather than sent as false on every ordinary row.
	Pending bool `json:"pending,omitempty"`
}

// Render builds the API view. Only listed (visible, non-deleted) comments are
// ever rendered to readers, so status appears only as the `pending` flag - the
// one state the poster and the fiction's author both need to see.
func (c *Comment) Render(viewerID uuid.UUID) View {
	return View{
		ID:         c.ID,
		NovelID:    c.NovelID,
		ChapterID:  c.ChapterID,
		ParentID:   c.ParentID,
		Content:    c.Content,
		Edited:     c.UpdatedAt.After(c.CreatedAt),
		CreatedAt:  c.CreatedAt,
		UpdatedAt:  c.UpdatedAt,
		Author:     c.Author,
		GuestName:  c.GuestName,
		ReplyCount: c.ReplyCount,
		LikeCount:  c.LikeCount,
		IsLiked:    c.IsLiked,
		IsOwner:    c.WrittenBy(viewerID),
		Pending:    c.Status == StatusPending,
	}
}
