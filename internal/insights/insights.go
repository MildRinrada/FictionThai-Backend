// Package insights answers the studio overview's questions about one fiction
// (Phase 13R).
//
// It is a READ-ONLY composition and owns no table of its own. Everything it
// returns already exists somewhere - daily view counters, comments, community
// posts - and the value it adds is asking for all of it under one ownership
// check, in one request, so the overview does not open four.
//
// It sits ABOVE the domains it reads, which is what keeps the fiction domain's
// rule intact: nothing in novels, chapters, or comments knows that community
// exists, and nothing here reaches into another package's tables. Each source
// is a consumer-defined interface satisfied by that domain's own repository.
//
// Two properties are deliberate and load-bearing:
//
//   - It is owner-only, decided in the SERVICE by the novels service's own
//     ForWriter - which returns the reader-identical 404 for a fiction the
//     caller may not open, so the endpoint cannot confirm a private draft.
//
//   - It reports NO per-reader anything. "ผู้อ่านสัปดาห์นี้" is a sum of daily
//     counters; the activity feed carries the display names comments and posts
//     already show publicly and nothing else. The studio's promise to writers -
//     เราไม่เก็บสถิติการอ่านรายบุคคล - stays true, and there is no query here
//     that could make it false.
package insights

import "time"

// Window is how far back "this week" reaches, in days.
const Window = 7

// ActivityKind names what happened, so a client can choose an icon and a verb
// without parsing a sentence.
type ActivityKind string

const (
	// KindComment is a reader commenting on the fiction or one of its chapters.
	KindComment ActivityKind = "comment"
	// KindPost is someone attaching the fiction to a community post.
	KindPost ActivityKind = "community_post"
)

// Activity is one line of ความเคลื่อนไหวล่าสุด.
//
// The pieces are separate rather than a pre-built sentence: the client owns the
// wording, and a Thai sentence assembled in Go is a sentence nobody can change
// without a deploy.
type Activity struct {
	Kind ActivityKind `json:"kind"`
	// Actor is a display name - never an account id, never an email.
	Actor string `json:"actor"`
	// Excerpt is the first part of what they wrote. Absent for events with no
	// text of their own.
	Excerpt string `json:"excerpt,omitempty"`
	// ChapterSlug and ChapterLabel name where it happened, when it happened
	// somewhere. A fiction-level comment has neither.
	ChapterSlug  string `json:"chapter_slug,omitempty"`
	ChapterLabel string `json:"chapter_label,omitempty"`
	// PostID lets the client link straight to the post.
	PostID    string    `json:"post_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// View is GET /novels/:ref/insights.
type View struct {
	// WeeklyViews is how many reads the fiction was credited with in the last
	// Window days. It is a COUNT of reads, de-duplicated per viewer per day by
	// the recorder - not a count of people, and never presented as one.
	WeeklyViews int64 `json:"weekly_views"`
	// WeeklyComments is how many comments arrived in the same window.
	//
	// "ตอนที่เผยแพร่" is deliberately NOT here: the overview already holds the
	// chapter list, and a second source for a number it can count itself is a
	// second thing that can disagree with what the page is showing.
	WeeklyComments int64 `json:"weekly_comments"`
	// PrevWeeklyViews and PrevWeeklyComments are the SAME sums over the window
	// before this one. They exist because a bare number answers nothing a
	// writer actually asks - "is it going up?" needs last week beside this one,
	// and shipping both is what keeps the comparison honest rather than a trend
	// invented client-side.
	PrevWeeklyViews    int64 `json:"prev_weekly_views"`
	PrevWeeklyComments int64 `json:"prev_weekly_comments"`
	// ViewsByDay and CommentsByDay are exactly Window entries, oldest first and
	// today last - the sparkline's data. Still daily COUNTERS: no reader, no
	// session, no time of day.
	ViewsByDay    []int64 `json:"views_by_day"`
	CommentsByDay []int64 `json:"comments_by_day"`
	// WindowDays is what "weekly" meant, so a client never has to assume.
	WindowDays int `json:"window_days"`
	// Activity is newest-first and may be empty. Always an array.
	Activity []Activity `json:"activity"`
}
