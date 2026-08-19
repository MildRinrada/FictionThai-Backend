// Package community implements the community layer: short posts by any
// signed-in user, their comment threads, and lightweight reactions
// (docs/08 §21 and §44 Phase 7, docs/09 §21, docs/01 §20).
//
// Community is a SEPARATE domain from fiction (docs/09 §21 "Community is a
// separate domain from novel chapters"): community_comments never touch the
// comments table or package, posts have no format system, and nothing here
// is writer-gated - the access matrix (docs/03 §27) lets any authenticated
// user post, so email verification is NOT required (it gates publishing
// fiction only, docs/AUTHENTICATION.md §9).
package community

import (
	"time"

	"github.com/google/uuid"
)

// Visibility is the AUTHOR's audience choice (docs/08 §21.1, docs/11 §37 -
// enforced by the backend, never the client).
type Visibility string

const (
	VisibilityPublic    Visibility = "public"
	VisibilityFollowers Visibility = "followers"
	VisibilityPrivate   Visibility = "private"
)

// Visibilities returns the allowlisted visibility values.
func Visibilities() []string { return []string{"public", "followers", "private"} }

// ValidVisibility reports whether a value is allowlisted.
func ValidVisibility(v string) bool {
	switch Visibility(v) {
	case VisibilityPublic, VisibilityFollowers, VisibilityPrivate:
		return true
	}
	return false
}

// PostStatus is the platform's moderation state (docs/08 §21.1) - distinct
// from both visibility and deleted_at.
type PostStatus string

const (
	PostStatusPublished PostStatus = "published"
	PostStatusHidden    PostStatus = "hidden"
	PostStatusRemoved   PostStatus = "removed"
)

// CommentStatus follows the §20.1 comment vocabulary; §21.2 leaves the values
// unenumerated and this is the same semantic family.
type CommentStatus string

const (
	CommentStatusVisible CommentStatus = "visible"
	CommentStatusHidden  CommentStatus = "hidden"
	CommentStatusRemoved CommentStatus = "removed"
)

// PostType is the AUTHOR's declared intent for a post
// (docs/COMMUNITY-FEED.md). It never gates who may read or write anything -
// it exists so the feed can be filtered by what people are looking for
// (หาเบต้า, อีเวนต์เขียน) and so the card can label itself.
type PostType string

const (
	PostTypeDiscussion   PostType = "discussion"   // พูดคุย - the default
	PostTypeAnnouncement PostType = "announcement" // ประกาศตอนใหม่
	PostTypePlotHelp     PostType = "plot_help"    // ขอความช่วยเหลือเรื่องพล็อต
	PostTypeBetaRequest  PostType = "beta_request" // หาเบต้า/นักเขียนร่วม
	PostTypeFicRequest   PostType = "fic_request"  // รับคำขอเขียน
	PostTypeEvent        PostType = "event"        // อีเวนต์เขียน
)

// PostTypeList returns the allowlisted post types. The column is VARCHAR
// without a CHECK, exactly like reaction_type: adding a type is a change
// here, never a migration.
func PostTypeList() []string {
	return []string{
		"discussion", "announcement", "plot_help",
		"beta_request", "fic_request", "event",
	}
}

// ValidPostType reports whether a post type is allowlisted.
func ValidPostType(t string) bool {
	switch PostType(t) {
	case PostTypeDiscussion, PostTypeAnnouncement, PostTypePlotHelp,
		PostTypeBetaRequest, PostTypeFicRequest, PostTypeEvent:
		return true
	}
	return false
}

// ReactionTypes is the service-level allowlist (docs/09 §21 documents "like";
// docs/01 §20.2's wider list is explicitly examples and "may support"). The
// column is VARCHAR, so adding a type later is a one-line change here, not a
// migration.
var reactionTypes = map[string]struct{}{
	"like": {},
}

// ReactionTypeList returns the allowlisted reaction types.
func ReactionTypeList() []string { return []string{"like"} }

// ValidReactionType reports whether a reaction type is allowlisted.
func ValidReactionType(t string) bool {
	_, ok := reactionTypes[t]
	return ok
}

// Author is the public card beside a post or comment - the same
// public-profile slice every other card uses (docs/08 §1.4).
type Author struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`
}

// PostReference is the fiction - and optionally the chapter - a post is about
// (docs/PHASE-12-STORY-DEPTH.md §12D).
//
// It is RESOLVED, never stored: every field below is read from novels and
// chapters at request time, through the same visibility predicate that decides
// whether the reader could open the fiction directly. Nothing is copied onto
// community_posts, so a fiction that goes private stops rendering a card the
// moment it does, and its title never leaks through a stranger's post.
type PostReference struct {
	NovelID    uuid.UUID `json:"novel_id"`
	NovelSlug  string    `json:"novel_slug"`
	NovelTitle string    `json:"novel_title"`
	CoverURL   *string   `json:"cover_url,omitempty"`

	// The format dimensions, so the card can label itself the way every other
	// fiction surface does rather than guessing (docs/09 §51).
	StoryStructure     string `json:"story_structure"`
	PresentationFormat string `json:"presentation_format"`
	ContentMode        string `json:"content_mode"`

	// AgeRating lets the card carry the same 18+ badge every other fiction
	// card does (docs/COMMUNITY-FEED.md): a post about a mature work must not
	// present it unbadged in a general feed.
	AgeRating string `json:"age_rating"`

	// Chapter fields are present only when the post attached a chapter.
	ChapterID     *uuid.UUID `json:"chapter_id,omitempty"`
	ChapterSlug   *string    `json:"chapter_slug,omitempty"`
	ChapterNumber *int       `json:"chapter_number,omitempty"`
	ChapterTitle  *string    `json:"chapter_title,omitempty"`
	WordCount     *int       `json:"word_count,omitempty"`
}

// Post is the storage row plus its joined author card and the viewer-aware
// enrichments a card needs.
type Post struct {
	ID       uuid.UUID
	AuthorID uuid.UUID

	Content    string
	Visibility Visibility
	Status     PostStatus
	Type       PostType

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time

	Author Author

	// CommentCount counts visible comments; ReactionCount counts reactions.
	CommentCount  int64
	ReactionCount int64

	// MyReaction is the VIEWER's reaction type, empty for none/guests.
	MyReaction string

	// Bookmarked reports whether the VIEWER saved this post; false for guests.
	Bookmarked bool

	// Reference is the attached fiction AS THIS VIEWER MAY SEE IT - nil when
	// the post attached nothing, when the fiction was deleted (the columns
	// SET NULL), or when this viewer may not open it.
	Reference *PostReference
}

// PostView is the API shape of one post (docs/09 §7 envelope `data`).
type PostView struct {
	ID      uuid.UUID `json:"id"`
	Content string    `json:"content"`

	// Visibility is included so the owner's edit UI can reflect it; it is the
	// author's own setting, not private data - the post being readable at all
	// already proves the viewer is inside its audience.
	Visibility Visibility `json:"visibility"`

	// Type is the author's declared intent (docs/COMMUNITY-FEED.md).
	Type PostType `json:"post_type"`

	Edited    bool      `json:"edited"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Author Author `json:"author"`

	CommentCount  int64 `json:"comment_count"`
	ReactionCount int64 `json:"reaction_count"`

	// MyReaction is the caller's reaction type, omitted when none.
	MyReaction string `json:"my_reaction,omitempty"`

	// Bookmarked is whether the caller saved this post; omitted when false,
	// which is also every guest's answer.
	Bookmarked bool `json:"bookmarked,omitempty"`

	// Reference is omitted entirely when there is none to show. A post whose
	// fiction the caller may not open is a post with no card - it neither
	// discloses the reference nor disappears (§12D).
	Reference *PostReference `json:"reference,omitempty"`

	IsOwner bool `json:"is_owner"`
}

// Render builds the API view.
func (p *Post) Render(viewerID uuid.UUID) PostView {
	return PostView{
		ID:            p.ID,
		Content:       p.Content,
		Visibility:    p.Visibility,
		Type:          p.Type,
		Edited:        p.UpdatedAt.After(p.CreatedAt),
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		Author:        p.Author,
		CommentCount:  p.CommentCount,
		ReactionCount: p.ReactionCount,
		MyReaction:    p.MyReaction,
		Bookmarked:    p.Bookmarked,
		Reference:     p.Reference,
		IsOwner:       viewerID != uuid.Nil && viewerID == p.AuthorID,
	}
}

// DiscussedFiction is one row of "fictions people are posting about" - the
// community sidebar. It carries the same resolved reference a post card uses,
// so the two can never describe the same fiction differently.
type DiscussedFiction struct {
	Fiction   PostReference `json:"fiction"`
	PostCount int64         `json:"post_count"`
}

// TrendingTag is one row of "แท็กที่กำลังพูดถึง" - a hashtag and how many
// recent PUBLIC posts carry it (docs/COMMUNITY-FEED.md). Like the discussed
// sidebar, it is one cacheable answer for everyone.
type TrendingTag struct {
	Tag       string `json:"tag"`
	PostCount int64  `json:"post_count"`
}

// Comment is a community comment row plus its author card.
type Comment struct {
	ID       uuid.UUID
	PostID   uuid.UUID
	AuthorID uuid.UUID
	ParentID *uuid.UUID

	Content string
	Status  CommentStatus

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time

	Author Author

	ReplyCount int64
}

// CommentView is the API shape of one community comment.
type CommentView struct {
	ID       uuid.UUID  `json:"id"`
	PostID   uuid.UUID  `json:"post_id"`
	ParentID *uuid.UUID `json:"parent_id,omitempty"`

	Content string `json:"content"`

	Edited    bool      `json:"edited"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Author Author `json:"author"`

	ReplyCount int64 `json:"reply_count"`
	IsOwner    bool  `json:"is_owner"`
}

// Render builds the API view.
func (c *Comment) Render(viewerID uuid.UUID) CommentView {
	return CommentView{
		ID:         c.ID,
		PostID:     c.PostID,
		ParentID:   c.ParentID,
		Content:    c.Content,
		Edited:     c.UpdatedAt.After(c.CreatedAt),
		CreatedAt:  c.CreatedAt,
		UpdatedAt:  c.UpdatedAt,
		Author:     c.Author,
		ReplyCount: c.ReplyCount,
		IsOwner:    viewerID != uuid.Nil && viewerID == c.AuthorID,
	}
}

// ReactionView answers the reaction endpoints with the post's new totals so
// the UI can reconcile without a second fetch.
type ReactionView struct {
	PostID        uuid.UUID `json:"post_id"`
	MyReaction    string    `json:"my_reaction,omitempty"`
	ReactionCount int64     `json:"reaction_count"`
}

// BookmarkView answers the bookmark endpoints the same way.
type BookmarkView struct {
	PostID     uuid.UUID `json:"post_id"`
	Bookmarked bool      `json:"bookmarked"`
}
