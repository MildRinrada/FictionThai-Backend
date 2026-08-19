// Package library owns the reader's personal shelf: bookmarks, follows, and
// reading progress (docs/08 §16–§18, docs/09 §17–§19, docs/03 §13).
//
// It is ONE domain rather than three because the rows share a shape and a
// purpose: each is per-user state pointing at someone else's content, carrying
// no authored content of its own. Nothing here is ever publicly listed - every
// read is scoped to the authenticated caller, and every response filters out
// fictions the caller can no longer read, so a novel going private never leaks
// through a shelf (docs/11 §31).
//
// The reader path constraint (docs/07 §67) shapes the queries: "Continue
// Reading" is one index walk, a progress save is one PK upsert, and a page of
// library cards costs two queries - one over the shelf table, one batch load of
// the fiction cards through the novels domain, which stays the single source of
// truth for what a card contains.
//
// Layering matches the rest of the backend (docs/09 §44): Handler -> Service ->
// Repository -> PostgreSQL, with authorization decided in the Service, never in
// HTTP middleware (docs/10 §27). The dependency is one-directional:
// library -> novels, never back.
package library

import (
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
)

// Entry is one bookmark on the shelf: the fiction card plus when it was saved
// (docs/09 §18 "My Library" - "Library responses should expose fiction format
// metadata", which the embedded novel view carries).
type Entry struct {
	Novel        novels.View `json:"novel"`
	BookmarkedAt time.Time   `json:"bookmarked_at"`
}

// Progress is the caller's saved position in one fiction - the row behind
// PUT/GET /novels/:novel/progress (docs/08 §18, docs/09 §17).
type Progress struct {
	NovelID   uuid.UUID `json:"novel_id"`
	ChapterID uuid.UUID `json:"chapter_id"`

	// ProgressPercent is how far through the CHAPTER the reader is (0–100).
	// The chapter identifies where in the novel they are, so the pair resumes
	// both one-shot and multi-chapter fiction (docs/08 §18.1).
	ProgressPercent float64   `json:"progress_percent"`
	LastReadAt      time.Time `json:"last_read_at"`
}

// ChapterRef is the slice of a chapter a Continue Reading card needs - enough
// to label and link the resume point, and nothing content-bearing. Minimal
// payloads are the point: this list renders on the library page for every
// signed-in visitor.
type ChapterRef struct {
	ID            uuid.UUID `json:"id"`
	ChapterNumber int       `json:"chapter_number"`
	Slug          string    `json:"slug"`
	Title         *string   `json:"title,omitempty"`
}

// ContinueReading is one entry of GET /me/reading-progress (docs/08 §18.1
// "Continue Reading", docs/02 §13).
//
// Chapter is nil when the chapter the reader stopped at is no longer live
// (unpublished or deleted since). The fiction is still shown - the reader's
// place in the STORY survives even when their place in a chapter cannot be
// linked, and progress is never deleted by someone else's action (docs/08 §3).
type ContinueReading struct {
	Novel           novels.View `json:"novel"`
	Chapter         *ChapterRef `json:"chapter"`
	ProgressPercent float64     `json:"progress_percent"`
	LastReadAt      time.Time   `json:"last_read_at"`

	// The three numbers the redesigned library runs on (library review
	// 2026-08): how many live chapters the fiction has, how many sit AFTER
	// the reader's stopped-at chapter, and how many were published after
	// they last read - the badge that brings readers back.
	TotalChapters int `json:"total_chapters"`
	ChaptersLeft  int `json:"chapters_left"`
	NewSinceRead  int `json:"new_since_read"`
}

// FollowedAuthor is one entry of GET /me/following (docs/03 §13 - the library's
// "Following" section).
type FollowedAuthor struct {
	Author     novels.Author `json:"author"`
	FollowedAt time.Time     `json:"followed_at"`

	// When this author last published a chapter the caller could read, and
	// how many of their public fictions are still being written - the two
	// facts the follow list groups by (library review 2026-08).
	LastPublishedAt *time.Time `json:"last_published_at,omitempty"`
	WritingCount    int        `json:"writing_count"`

	// The per-follow notification choice: following is not the same as
	// wanting an alert from everyone.
	NotifyNewChapters bool `json:"notify_new_chapters"`
}

// FinishedEntry is one entry of GET /me/finished - the reader's own record of
// having read a fiction through, with an optional PRIVATE star and note.
type FinishedEntry struct {
	Novel      novels.View `json:"novel"`
	FinishedAt time.Time   `json:"finished_at"`
	Stars      *int        `json:"stars,omitempty"`
	Note       *string     `json:"note,omitempty"`
}

// HistoryEntry is one entry of GET /me/history. Owner-only, always - README:
// "Reading history should never be exposed through public APIs."
type HistoryEntry struct {
	Novel   novels.View `json:"novel"`
	Chapter *ChapterRef `json:"chapter"`
	ReadAt  time.Time   `json:"read_at"`
}

// HistorySettings answers GET/PUT /me/history/settings.
type HistorySettings struct {
	RecordHistory bool `json:"record_history"`
}

// FollowStatus answers GET /users/:user/follow-status (docs/09 §19).
type FollowStatus struct {
	IsFollowing bool `json:"is_following"`
}
