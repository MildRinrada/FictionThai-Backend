package community

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// ErrNotFound covers "no such post/comment". The service translates to HTTP.
var ErrNotFound = errors.New("community row not found")

// Repository is the only place that reads or writes community_posts,
// community_comments, and community_reactions. It also reads user_follows for
// the followers-visibility predicate - table access, not a package
// dependency, exactly like the notifications repository.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// argList builds a positional-parameter list, so every value is bound rather
// than interpolated (docs/11 §15).
type argList struct {
	args []any
}

func (a *argList) add(value any) string {
	a.args = append(a.args, value)
	return "$" + strconv.Itoa(len(a.args))
}

// escapeLike neutralises the wildcards a searcher could otherwise inject.
// Without it, a query of "%" matches every post and turns a bounded search
// into a full scan. The same guard novels, profiles, and taxonomy each carry.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// visiblePostSQL is the predicate for "a post this viewer may open", against
// alias `p` (docs/11 §37: the backend enforces visibility):
//
//	public     everyone, including guests
//	followers  the author's followers, and the author
//	private    the author alone
//
// Moderated (hidden/removed) and author-deleted posts are invisible to
// EVERYONE, the author included - the same rule Phase 6 comments follow;
// the moderation phase can refine owner transparency later.
//
// viewerID is uuid.Nil for guests, which matches nothing in the owner and
// follower clauses, so a guest sees exactly the public feed.
func visiblePostSQL(args *argList, viewerID uuid.UUID) string {
	viewer := args.add(viewerID)
	return `(p.deleted_at IS NULL AND p.status = 'published' AND (
		p.visibility = 'public'
		OR p.author_id = ` + viewer + `
		OR (p.visibility = 'followers' AND EXISTS (
			SELECT 1 FROM user_follows f
			WHERE f.following_id = p.author_id AND f.follower_id = ` + viewer + `
		))
	))`
}

// postColumns is every storage column plus the author card, the viewer-aware
// enrichments, and the resolved reference - in one query per page
// (docs/07 §67). The reference columns come from the LEFT JOINs below and are
// all NULL when the viewer may not see the fiction.
func postColumns(args *argList, viewerID uuid.UUID) string {
	return `
	p.id, p.author_id, p.content, p.visibility, p.status, p.post_type,
	p.created_at, p.updated_at, p.deleted_at,
	u.id, u.username, pr.display_name, pr.avatar_url,
	(
		SELECT count(*) FROM community_comments cc
		WHERE cc.post_id = p.id AND cc.deleted_at IS NULL AND cc.status = 'visible'
	) AS comment_count,
	(
		SELECT count(*) FROM community_reactions r WHERE r.post_id = p.id
	) AS reaction_count,
	COALESCE((
		SELECT r.reaction_type FROM community_reactions r
		WHERE r.post_id = p.id AND r.user_id = ` + args.add(viewerID) + `
	), '') AS my_reaction,
	EXISTS (
		SELECT 1 FROM community_post_bookmarks b
		WHERE b.post_id = p.id AND b.user_id = ` + args.add(viewerID) + `
	) AS bookmarked,` + referenceColumns
}

// referenceColumns is the resolved reference, against the `n` and `c` aliases
// joined by postJoins. The fiction half is shared with the discussed-fictions
// query so a fiction can never be described one way on a post card and another
// in the sidebar.
const novelReferenceColumns = `
	n.id, n.slug, n.title, n.cover_url,
	n.story_structure, n.presentation_format, n.content_mode, n.age_rating`

const referenceColumns = novelReferenceColumns + `,
	c.id, c.slug, c.chapter_number, c.title, c.word_count`

// noChapterColumns fills the chapter half for a fiction-level row. Typed NULLs
// rather than a contrived join, so the scan targets line up either way.
const noChapterColumns = `
	NULL::uuid, NULL::varchar, NULL::integer, NULL::varchar, NULL::integer`

// postJoins attaches the author card and RESOLVES the fiction reference
// against this viewer (docs/PHASE-12-STORY-DEPTH.md §12D).
//
// The two reference joins are LEFT JOINs whose ON clause carries the whole
// visibility rule, so a reference the viewer may not see simply produces NULLs
// rather than dropping the post from the feed: the post stays, its card does
// not appear. novels.ReadableSQL and novels.LiveChapterSQL are the shared
// predicates every other domain filters with, so this can never drift into
// exposing a private draft through someone else's post (docs/11 §31).
//
// The owner clause lets a writer see their own unpublished work in a card they
// posted themselves. It widens nothing for anyone else - viewerID is always the
// authenticated caller.
//
// `c.novel_id = n.id` is what makes the chapter belong to the referenced
// fiction: without it, a mismatched pair written by any future code path would
// render a chapter title under the wrong fiction.
func postJoins(args *argList, viewerID uuid.UUID) string {
	// Bound through novels.ViewerValue so a GUEST is SQL NULL rather than a
	// zero uuid: the members rung asks "is anybody signed in", and a zero would
	// answer yes for every visitor on the platform (§13C).
	viewer := args.add(novels.ViewerValue(viewerID)) + "::uuid"
	return `
	JOIN users u ON u.id = p.author_id
	LEFT JOIN user_profiles pr ON pr.user_id = p.author_id
	LEFT JOIN novels n ON n.id = p.novel_id AND ((` + novels.ReadableSQLFor(viewer) + `)
		OR (n.deleted_at IS NULL AND n.author_id = ` + viewer + `))
	LEFT JOIN chapters c ON c.id = p.chapter_id AND c.novel_id = n.id
		AND ((` + novels.LiveChapterSQL + `)
			OR (c.deleted_at IS NULL AND n.author_id = ` + viewer + `))`
}

const postSource = `
	FROM community_posts p`

type scanner interface{ Scan(...any) error }

// referenceScan holds the nullable reference columns for one row.
type referenceScan struct {
	novelID       uuid.NullUUID
	novelSlug     sql.NullString
	novelTitle    sql.NullString
	coverURL      sql.NullString
	structure     sql.NullString
	presentation  sql.NullString
	mode          sql.NullString
	ageRating     sql.NullString
	chapterID     uuid.NullUUID
	chapterSlug   sql.NullString
	chapterNumber sql.NullInt64
	chapterTitle  sql.NullString
	wordCount     sql.NullInt64
}

func (r *referenceScan) targets() []any {
	return []any{
		&r.novelID, &r.novelSlug, &r.novelTitle, &r.coverURL,
		&r.structure, &r.presentation, &r.mode, &r.ageRating,
		&r.chapterID, &r.chapterSlug, &r.chapterNumber, &r.chapterTitle, &r.wordCount,
	}
}

// build returns the reference, or nil when the join found nothing this viewer
// may see.
func (r *referenceScan) build() *PostReference {
	if !r.novelID.Valid {
		return nil
	}
	ref := &PostReference{
		NovelID:            r.novelID.UUID,
		NovelSlug:          r.novelSlug.String,
		NovelTitle:         r.novelTitle.String,
		StoryStructure:     r.structure.String,
		PresentationFormat: r.presentation.String,
		ContentMode:        r.mode.String,
		AgeRating:          r.ageRating.String,
	}
	if r.coverURL.Valid {
		ref.CoverURL = &r.coverURL.String
	}
	if r.chapterID.Valid {
		id := r.chapterID.UUID
		ref.ChapterID = &id
		if r.chapterSlug.Valid {
			ref.ChapterSlug = &r.chapterSlug.String
		}
		if r.chapterNumber.Valid {
			number := int(r.chapterNumber.Int64)
			ref.ChapterNumber = &number
		}
		if r.chapterTitle.Valid {
			ref.ChapterTitle = &r.chapterTitle.String
		}
		if r.wordCount.Valid {
			words := int(r.wordCount.Int64)
			ref.WordCount = &words
		}
	}
	return ref
}

func scanPost(row scanner) (*Post, error) {
	var p Post
	var ref referenceScan
	targets := append([]any{
		&p.ID, &p.AuthorID, &p.Content, &p.Visibility, &p.Status, &p.Type,
		&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt,
		&p.Author.ID, &p.Author.Username, &p.Author.DisplayName, &p.Author.AvatarURL,
		&p.CommentCount, &p.ReactionCount, &p.MyReaction, &p.Bookmarked,
	}, ref.targets()...)

	err := row.Scan(targets...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan community post: %w", err)
	}
	p.Reference = ref.build()
	return &p, nil
}

// ---------------------------------------------------------------------------
// Posts
// ---------------------------------------------------------------------------

// PostFilter selects one feed (docs/09 §21 List Posts).
type PostFilter struct {
	// AuthorID scopes to one author's posts (?author=, resolved by the
	// service). Visibility still applies - a stranger browsing an author page
	// sees only what they are allowed to.
	AuthorID uuid.UUID

	// FollowingOnly narrows to authors the viewer follows (?feed=following).
	// Meaningless for guests; the service requires authentication first.
	FollowingOnly bool

	// WithReferenceOnly narrows to posts that attached a fiction
	// (?feed=attached). It filters on the COLUMN, not on the resolved
	// reference: a post whose fiction this viewer may not open still belongs to
	// the feed, and still renders without a card - filtering on visibility here
	// would turn the page count into an oracle (§12D).
	WithReferenceOnly bool

	// NovelID narrows to posts attached to one fiction (§13R) - the studio's
	// "โพสต์ชุมชนที่พูดถึงเรื่องนี้". It composes with the visibility predicate
	// above rather than replacing it: an author reading their own fiction's
	// posts still sees only the posts they would see in the community itself,
	// which is what stops this from becoming a way around a hidden post.
	NovelID uuid.UUID

	// --- Feed tools (docs/COMMUNITY-FEED.md) -------------------------------

	// Type narrows to one declared post type (?type=beta_request).
	Type string

	// BookmarkedBy narrows to posts this user saved (?feed=saved) and orders
	// them by when they were saved. The visibility predicate still applies: a
	// saved post whose author narrowed its audience afterwards stays saved
	// but stops listing, rather than the bookmark becoming a keyhole.
	BookmarkedBy uuid.UUID

	// PublicOnly narrows the audience predicate to public posts. Search sets
	// it: search never surfaces followers-only or private posts, even to
	// viewers inside their audience, so it cannot be used to trawl a person's
	// narrower posts. The one exception - searching your own posts - is
	// expressed by the service leaving this false (docs/COMMUNITY-FEED.md).
	PublicOnly bool

	// Query is the trimmed free-text needle; empty means no text search.
	Query string

	// Mention narrows to posts whose text mentions @<handle> (to:@handle).
	// A text convention, not a relation - posts have no recipient column.
	Mention string

	// Tag narrows to posts carrying one extracted hashtag.
	Tag string

	// Fandom narrows to posts whose attached fiction declares this fandom
	// (fandom:"..." - docs/FANDOM.md free text, matched broadly).
	Fandom string

	// Since drops posts created before this instant (the time-range chips).
	Since *time.Time

	// HasChapter narrows to posts that attached a chapter (มีตอนแนบ);
	// TextOnly to posts that attached nothing (ข้อความล้วน).
	HasChapter bool
	TextOnly   bool

	// SortTop orders by engagement (reactions + visible comments) instead of
	// recency - "มีปฏิสัมพันธ์มากสุด".
	SortTop bool
}

// ListPosts returns one page of the feed, newest first - riding
// community_posts_created_idx (docs/08 §37).
func (r *Repository) ListPosts(
	ctx context.Context, viewerID uuid.UUID, filter PostFilter, page pagination.Params,
) ([]Post, int64, error) {
	args := &argList{}

	where := ` WHERE ` + visiblePostSQL(args, viewerID)
	if filter.AuthorID != uuid.Nil {
		where += ` AND p.author_id = ` + args.add(filter.AuthorID)
	}
	if filter.FollowingOnly {
		where += ` AND EXISTS (
			SELECT 1 FROM user_follows f
			WHERE f.following_id = p.author_id AND f.follower_id = ` + args.add(viewerID) + `
		)`
	}
	if filter.WithReferenceOnly {
		where += ` AND p.novel_id IS NOT NULL`
	}
	if filter.NovelID != uuid.Nil {
		where += ` AND p.novel_id = ` + args.add(filter.NovelID)
	}
	if filter.PublicOnly {
		where += ` AND p.visibility = 'public'`
	}
	if filter.Type != "" {
		where += ` AND p.post_type = ` + args.add(filter.Type)
	}
	if filter.BookmarkedBy != uuid.Nil {
		where += ` AND EXISTS (
			SELECT 1 FROM community_post_bookmarks b
			WHERE b.post_id = p.id AND b.user_id = ` + args.add(filter.BookmarkedBy) + `
		)`
	}
	if filter.Query != "" {
		where += ` AND p.content ILIKE ` + args.add("%"+escapeLike(filter.Query)+"%")
	}
	if filter.Mention != "" {
		where += ` AND p.content ILIKE ` + args.add("%@"+escapeLike(filter.Mention)+"%")
	}
	if filter.Tag != "" {
		where += ` AND EXISTS (
			SELECT 1 FROM community_post_hashtags h
			WHERE h.post_id = p.id AND h.tag = ` + args.add(strings.ToLower(filter.Tag)) + `
		)`
	}
	if filter.Fandom != "" {
		// Behind the same public-readability predicate every fiction listing
		// uses: a fandom filter must not confirm what a private work is about.
		where += ` AND EXISTS (
			SELECT 1 FROM novels n
			WHERE n.id = p.novel_id AND (` + novels.ReadableSQL + `)
			  AND n.fandom ILIKE ` + args.add("%"+escapeLike(filter.Fandom)+"%") + `
		)`
	}
	if filter.HasChapter {
		where += ` AND p.chapter_id IS NOT NULL`
	}
	if filter.TextOnly {
		where += ` AND p.novel_id IS NULL`
	}
	if filter.Since != nil {
		where += ` AND p.created_at >= ` + args.add(*filter.Since)
	}

	// The count runs BEFORE the select columns and joins add their parameters,
	// so it is bound to exactly the arguments its own predicate uses.
	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM community_posts p`+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count community posts: %w", err)
	}
	if total == 0 {
		return []Post{}, 0, nil
	}

	columns := postColumns(args, viewerID)
	joins := postJoins(args, viewerID)

	order := ` ORDER BY p.created_at DESC, p.id DESC`
	switch {
	case filter.BookmarkedBy != uuid.Nil:
		// The saved feed reads in the order things were saved, not written.
		order = ` ORDER BY (
			SELECT b.created_at FROM community_post_bookmarks b
			WHERE b.post_id = p.id AND b.user_id = ` + args.add(filter.BookmarkedBy) + `
		) DESC, p.id DESC`
	case filter.SortTop:
		// Engagement mirrors the two counters the card shows. The subqueries
		// repeat the SELECT's counters because ORDER BY may not reference an
		// expression over output aliases; the planner runs them once per row
		// either way.
		order = ` ORDER BY (
			(SELECT count(*) FROM community_reactions tr WHERE tr.post_id = p.id)
			+ (SELECT count(*) FROM community_comments tc
				WHERE tc.post_id = p.id AND tc.deleted_at IS NULL AND tc.status = 'visible')
		) DESC, p.created_at DESC, p.id DESC`
	}

	query := `SELECT ` + columns + postSource + joins + where + order +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list community posts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	posts := []Post{}
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, 0, err
		}
		posts = append(posts, *post)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list community posts: %w", err)
	}
	return posts, total, nil
}

// RecentPost is one post reduced to what a studio activity line shows (§13R).
type RecentPost struct {
	ID         uuid.UUID
	AuthorName string
	Excerpt    string
	CreatedAt  time.Time
}

// RecentPostsForNovel lists the newest posts attached to one fiction, as its
// AUTHOR would see them in the community.
//
// The same visibility predicate the feed uses, with the same viewer. A fiction's
// author is not automatically an audience for a post about it - a followers-only
// post from someone they do not follow is not theirs to read - and quietly
// widening the rule here would turn a studio panel into a way around it.
func (r *Repository) RecentPostsForNovel(
	ctx context.Context, viewerID, novelID uuid.UUID, limit int,
) ([]RecentPost, error) {
	args := &argList{}
	where := ` WHERE ` + visiblePostSQL(args, viewerID) +
		` AND p.novel_id = ` + args.add(novelID)

	rows, err := r.db.QueryContext(ctx, `
		SELECT p.id,
		       COALESCE(NULLIF(pr.display_name, ''), u.username, 'สมาชิก'),
		       left(p.content, 120),
		       p.created_at
		FROM community_posts p
		LEFT JOIN users u ON u.id = p.author_id
		LEFT JOIN user_profiles pr ON pr.user_id = p.author_id`+where+`
		ORDER BY p.created_at DESC, p.id DESC
		LIMIT `+args.add(limit), args.args...)
	if err != nil {
		return nil, fmt.Errorf("list recent posts for novel: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := []RecentPost{}
	for rows.Next() {
		var item RecentPost
		if err := rows.Scan(&item.ID, &item.AuthorName, &item.Excerpt,
			&item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan recent post: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recent posts for novel: %w", err)
	}
	return items, nil
}

// FindVisiblePost loads one post the viewer may open; anything else - absent,
// deleted, moderated, outside the audience - is the same ErrNotFound
// (docs/11 §3.4).
func (r *Repository) FindVisiblePost(
	ctx context.Context, viewerID uuid.UUID, id uuid.UUID,
) (*Post, error) {
	args := &argList{}
	columns := postColumns(args, viewerID)
	joins := postJoins(args, viewerID)
	query := `SELECT ` + columns + postSource + joins +
		` WHERE p.id = ` + args.add(id) + ` AND ` + visiblePostSQL(args, viewerID)
	return scanPost(r.db.QueryRowContext(ctx, query, args.args...))
}

// FindPost loads one post regardless of visibility or state - for OWNERSHIP
// operations only; the service decides what each state means. The reference is
// resolved as a guest would see it, because no viewer is known here; callers
// that render the result re-read it through a viewer-aware path.
func (r *Repository) FindPost(ctx context.Context, id uuid.UUID) (*Post, error) {
	args := &argList{}
	columns := postColumns(args, uuid.Nil)
	joins := postJoins(args, uuid.Nil)
	query := `SELECT ` + columns + postSource + joins +
		` WHERE p.id = ` + args.add(id)
	return scanPost(r.db.QueryRowContext(ctx, query, args.args...))
}

// CreatePostParams carries everything a new post stores. The reference ids are
// already validated by the service against the AUTHOR's own read access.
type CreatePostParams struct {
	AuthorID   uuid.UUID
	Content    string
	Visibility Visibility
	Type       PostType

	NovelID   *uuid.UUID
	ChapterID *uuid.UUID
}

// CreatePost inserts a post and returns it with its author card, resolved for
// the author - who is, at this moment, the only viewer.
//
// The insert and the derived hashtag rows commit together: a post must never
// exist whose tags describe some other version of its text.
func (r *Repository) CreatePost(ctx context.Context, params CreatePostParams) (*Post, error) {
	args := &argList{}
	values := args.add(params.AuthorID) + `, ` + args.add(params.Content) + `, ` +
		args.add(params.Visibility) + `, ` + args.add(params.Type) + `, ` +
		args.add(params.NovelID) + `, ` + args.add(params.ChapterID)

	columns := postColumns(args, params.AuthorID)
	joins := postJoins(args, params.AuthorID)
	insert := `WITH inserted AS (
			INSERT INTO community_posts (author_id, content, visibility, post_type, novel_id, chapter_id)
			VALUES (` + values + `)
			RETURNING *
		)
		SELECT ` + columns + `
		FROM inserted p` + joins

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create community post: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	post, err := scanPost(tx.QueryRowContext(ctx, insert, args.args...))
	if err != nil {
		return nil, fmt.Errorf("create community post: %w", err)
	}
	if err := replaceHashtags(ctx, tx, post.ID, ExtractHashtags(params.Content)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create community post: %w", err)
	}
	return post, nil
}

// replaceHashtags rewrites a post's derived hashtag rows to match its
// content. Tags are derived data (docs/COMMUNITY-FEED.md): delete-and-insert
// keeps them exactly what extraction says, with nothing stale surviving an
// edit.
func replaceHashtags(ctx context.Context, tx *sql.Tx, postID uuid.UUID, tags []string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM community_post_hashtags WHERE post_id = $1`, postID); err != nil {
		return fmt.Errorf("clear post hashtags: %w", err)
	}
	if len(tags) == 0 {
		return nil
	}

	args := &argList{}
	post := args.add(postID)
	values := make([]string, 0, len(tags))
	for _, tag := range tags {
		values = append(values, "("+post+", "+args.add(tag)+")")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO community_post_hashtags (post_id, tag) VALUES `+
			strings.Join(values, ", ")+` ON CONFLICT DO NOTHING`,
		args.args...); err != nil {
		return fmt.Errorf("write post hashtags: %w", err)
	}
	return nil
}

// UpdatePostParams is a partial edit; nil fields stay untouched.
type UpdatePostParams struct {
	Content    *string
	Visibility *Visibility
	Type       *PostType

	// SetReference distinguishes "the caller sent no reference field" from
	// "the caller sent null" (docs/09 §3). Only when it is true are the two
	// columns written - with the ids below, which may both be nil to detach.
	SetReference bool
	NovelID      *uuid.UUID
	ChapterID    *uuid.UUID
}

// UpdatePost applies a partial edit and stamps updated_at. viewerID resolves
// the reference in the returned row. A content edit re-extracts the derived
// hashtags in the same transaction.
func (r *Repository) UpdatePost(
	ctx context.Context, id, viewerID uuid.UUID, params UpdatePostParams,
) (*Post, error) {
	args := &argList{}
	sets := "updated_at = now()"
	if params.Content != nil {
		sets += ", content = " + args.add(*params.Content)
	}
	if params.Visibility != nil {
		sets += ", visibility = " + args.add(*params.Visibility)
	}
	if params.Type != nil {
		sets += ", post_type = " + args.add(*params.Type)
	}
	if params.SetReference {
		// Written together, always: a detach must clear both columns, and an
		// attach must never leave a chapter from the previous fiction behind.
		sets += ", novel_id = " + args.add(params.NovelID) +
			", chapter_id = " + args.add(params.ChapterID)
	}

	columns := postColumns(args, viewerID)
	joins := postJoins(args, viewerID)
	query := `WITH updated AS (
			UPDATE community_posts SET ` + sets + `
			WHERE id = ` + args.add(id) + ` AND deleted_at IS NULL
			RETURNING *
		)
		SELECT ` + columns + `
		FROM updated p` + joins

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update community post: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	post, err := scanPost(tx.QueryRowContext(ctx, query, args.args...))
	if err != nil {
		return nil, err
	}
	if params.Content != nil {
		if err := replaceHashtags(ctx, tx, post.ID, ExtractHashtags(*params.Content)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update community post: %w", err)
	}
	return post, nil
}

// ---------------------------------------------------------------------------
// Discussed fictions (the community sidebar)
// ---------------------------------------------------------------------------

// DiscussedWindow is how far back "being talked about" reaches. A day is too
// narrow to ever fill the panel on a young platform; a week keeps it honest
// without needing the statistics phase.
const DiscussedWindow = "7 days"

// ListDiscussedFictions returns the fictions PUBLIC posts referenced most in
// the recent window.
//
// Deliberately viewer-independent: only public posts count, and only publicly
// readable fictions qualify - no owner clause. That keeps the panel a
// discovery surface with one cacheable answer for everyone, and means it can
// never surface a private fiction to the one person who can see it while
// silently changing rank for everyone else.
func (r *Repository) ListDiscussedFictions(
	ctx context.Context, limit int,
) ([]DiscussedFiction, error) {
	query := `
		SELECT ` + novelReferenceColumns + `, ` + noChapterColumns + `, count(*) AS post_count
		FROM community_posts p
		JOIN novels n ON n.id = p.novel_id AND (` + novels.ReadableSQL + `)
		WHERE p.deleted_at IS NULL
		  AND p.status = 'published'
		  AND p.visibility = 'public'
		  AND p.created_at >= now() - $1::interval
		GROUP BY n.id, n.slug, n.title, n.cover_url,
		         n.story_structure, n.presentation_format, n.content_mode, n.age_rating
		ORDER BY post_count DESC, n.title ASC
		LIMIT $2`

	rows, err := r.db.QueryContext(ctx, query, DiscussedWindow, limit)
	if err != nil {
		return nil, fmt.Errorf("list discussed fictions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := []DiscussedFiction{}
	for rows.Next() {
		var ref referenceScan
		var count int64
		if err := rows.Scan(append(ref.targets(), &count)...); err != nil {
			return nil, fmt.Errorf("scan discussed fiction: %w", err)
		}
		fiction := ref.build()
		if fiction == nil {
			continue
		}
		items = append(items, DiscussedFiction{Fiction: *fiction, PostCount: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list discussed fictions: %w", err)
	}
	return items, nil
}

// SoftDeletePost marks a post taken back by its author. The row survives for
// integrity (replies keep a parent, moderation keeps a trail); it never lists
// again.
// SetPostStatus moves the platform's moderation axis (docs/08 §21.1). It
// never touches visibility or deleted_at - those belong to the author.
func (r *Repository) SetPostStatus(ctx context.Context, id uuid.UUID, status PostStatus) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE community_posts SET status = $2 WHERE id = $1 AND deleted_at IS NULL`, id, status)
	if err != nil {
		return fmt.Errorf("set post status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set post status: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCommentStatus is SetPostStatus's community-comment counterpart.
func (r *Repository) SetCommentStatus(ctx context.Context, id uuid.UUID, status CommentStatus) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE community_comments SET status = $2 WHERE id = $1 AND deleted_at IS NULL`, id, status)
	if err != nil {
		return fmt.Errorf("set community comment status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set community comment status: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) SoftDeletePost(ctx context.Context, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE community_posts SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("delete community post: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete community post result: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

const visibleCommentSQL = `c.deleted_at IS NULL AND c.status = 'visible'`

const commentColumns = `
	c.id, c.post_id, c.author_id, c.parent_id,
	c.content, c.status, c.created_at, c.updated_at, c.deleted_at,
	u.id, u.username, pr.display_name, pr.avatar_url,
	(
		SELECT count(*) FROM community_comments rc
		WHERE rc.parent_id = c.id AND rc.deleted_at IS NULL AND rc.status = 'visible'
	) AS reply_count`

const commentFrom = `
	FROM community_comments c
	JOIN users u ON u.id = c.author_id
	LEFT JOIN user_profiles pr ON pr.user_id = c.author_id`

func scanComment(row scanner) (*Comment, error) {
	var c Comment
	err := row.Scan(
		&c.ID, &c.PostID, &c.AuthorID, &c.ParentID,
		&c.Content, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.DeletedAt,
		&c.Author.ID, &c.Author.Username, &c.Author.DisplayName, &c.Author.AvatarURL,
		&c.ReplyCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan community comment: %w", err)
	}
	return &c, nil
}

// CommentFilter selects one listing scope.
type CommentFilter struct {
	// PostID lists the post's top-level thread.
	PostID uuid.UUID

	// ParentID lists one comment's replies instead; PostID is ignored.
	ParentID *uuid.UUID
}

// ListComments returns one page of visible comments - top-level newest first,
// replies oldest first, the same reading order as fiction threads.
func (r *Repository) ListComments(
	ctx context.Context, filter CommentFilter, page pagination.Params,
) ([]Comment, int64, error) {
	args := &argList{}

	where := ` WHERE ` + visibleCommentSQL
	order := ` ORDER BY c.created_at DESC, c.id DESC`
	if filter.ParentID != nil {
		where += ` AND c.parent_id = ` + args.add(*filter.ParentID)
		order = ` ORDER BY c.created_at ASC, c.id ASC`
	} else {
		where += ` AND c.post_id = ` + args.add(filter.PostID) + ` AND c.parent_id IS NULL`
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM community_comments c`+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count community comments: %w", err)
	}
	if total == 0 {
		return []Comment{}, 0, nil
	}

	query := `SELECT ` + commentColumns + commentFrom + where + order +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list community comments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	comments := []Comment{}
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			return nil, 0, err
		}
		comments = append(comments, *comment)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list community comments: %w", err)
	}
	return comments, total, nil
}

// FindComment loads one comment whatever its state; the service decides.
func (r *Repository) FindComment(ctx context.Context, id uuid.UUID) (*Comment, error) {
	return scanComment(r.db.QueryRowContext(ctx,
		`SELECT `+commentColumns+commentFrom+` WHERE c.id = $1`, id))
}

// CreateComment inserts a comment with its author card in one round trip.
func (r *Repository) CreateComment(
	ctx context.Context, postID, authorID uuid.UUID, parentID *uuid.UUID, content string,
) (*Comment, error) {
	row := r.db.QueryRowContext(ctx, `
		WITH inserted AS (
			INSERT INTO community_comments (post_id, author_id, parent_id, content)
			VALUES ($1, $2, $3, $4)
			RETURNING *
		)
		SELECT `+commentColumns+`
		FROM inserted c
		JOIN users u ON u.id = c.author_id
		LEFT JOIN user_profiles pr ON pr.user_id = c.author_id`,
		postID, authorID, parentID, content)

	comment, err := scanComment(row)
	if err != nil {
		return nil, fmt.Errorf("create community comment: %w", err)
	}
	return comment, nil
}

// UpdateCommentContent replaces the text and stamps updated_at.
func (r *Repository) UpdateCommentContent(
	ctx context.Context, id uuid.UUID, content string,
) (*Comment, error) {
	return scanComment(r.db.QueryRowContext(ctx, `
		WITH updated AS (
			UPDATE community_comments SET content = $2, updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL
			RETURNING *
		)
		SELECT `+commentColumns+`
		FROM updated c
		JOIN users u ON u.id = c.author_id
		LEFT JOIN user_profiles pr ON pr.user_id = c.author_id`, id, content))
}

// SoftDeleteComment marks a comment taken back by its author.
func (r *Repository) SoftDeleteComment(ctx context.Context, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE community_comments SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("delete community comment: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete community comment result: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Trending hashtags (docs/COMMUNITY-FEED.md)
// ---------------------------------------------------------------------------

// ListTrendingTags counts recent PUBLIC posts per extracted hashtag, most
// used first. prefix optionally narrows to tags starting with it - the `#`
// autocomplete - matched against the stored lowercase form.
//
// Viewer-independent for the same reason ListDiscussedFictions is: one
// cacheable answer for everyone, counting only posts everyone may read.
func (r *Repository) ListTrendingTags(
	ctx context.Context, prefix string, limit int,
) ([]TrendingTag, error) {
	args := &argList{}
	where := `
		WHERE p.deleted_at IS NULL
		  AND p.status = 'published'
		  AND p.visibility = 'public'
		  AND p.created_at >= now() - ` + args.add(DiscussedWindow) + `::interval`
	if prefix != "" {
		where += ` AND h.tag LIKE ` + args.add(escapeLike(strings.ToLower(prefix))+"%")
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT h.tag, count(*) AS post_count
		FROM community_post_hashtags h
		JOIN community_posts p ON p.id = h.post_id`+where+`
		GROUP BY h.tag
		ORDER BY post_count DESC, h.tag ASC
		LIMIT `+args.add(limit), args.args...)
	if err != nil {
		return nil, fmt.Errorf("list trending tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := []TrendingTag{}
	for rows.Next() {
		var item TrendingTag
		if err := rows.Scan(&item.Tag, &item.PostCount); err != nil {
			return nil, fmt.Errorf("scan trending tag: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trending tags: %w", err)
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// Bookmarks (docs/COMMUNITY-FEED.md)
// ---------------------------------------------------------------------------

// UpsertBookmark saves a post for the viewer. The composite PK is the
// duplicate guard: saving twice is a no-op, never an error (docs/09 §33).
func (r *Repository) UpsertBookmark(ctx context.Context, postID, userID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO community_post_bookmarks (user_id, post_id)
		VALUES ($1, $2)
		ON CONFLICT (user_id, post_id) DO NOTHING`, userID, postID)
	if err != nil {
		return fmt.Errorf("upsert bookmark: %w", err)
	}
	return nil
}

// DeleteBookmark removes the viewer's bookmark. Idempotent, and never gated
// on the post still being visible - taking a bookmark back must always work,
// exactly like removing a reaction.
func (r *Repository) DeleteBookmark(ctx context.Context, postID, userID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM community_post_bookmarks WHERE post_id = $1 AND user_id = $2`,
		postID, userID)
	if err != nil {
		return fmt.Errorf("delete bookmark: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reactions
// ---------------------------------------------------------------------------

// UpsertReaction records the viewer's reaction. The composite PK is the
// duplicate guard (docs/09 §34); reacting again with another type REPLACES
// the reaction rather than accumulating.
//
// The returned bool reports whether a row was actually INSERTED - false when
// an existing reaction was updated or unchanged. The notification hangs on
// it, so re-reacting can never re-notify by itself (the worker dedupes as the
// second guard).
func (r *Repository) UpsertReaction(
	ctx context.Context, postID, userID uuid.UUID, reactionType string,
) (bool, error) {
	var inserted bool
	// xmax = 0 is the standard PostgreSQL idiom for "this row was inserted,
	// not updated, by this statement".
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO community_reactions (post_id, user_id, reaction_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (post_id, user_id) DO UPDATE SET reaction_type = EXCLUDED.reaction_type
		RETURNING (xmax = 0)`, postID, userID, reactionType).Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("upsert reaction: %w", err)
	}
	return inserted, nil
}

// DeleteReaction removes the viewer's reaction. Idempotent: removing an
// absent reaction is a no-op, and removal always works - even on a post the
// viewer can no longer open, exactly like removing a bookmark.
func (r *Repository) DeleteReaction(ctx context.Context, postID, userID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM community_reactions WHERE post_id = $1 AND user_id = $2`, postID, userID)
	if err != nil {
		return fmt.Errorf("delete reaction: %w", err)
	}
	return nil
}

// ReactionState answers the reaction endpoints with the post's new totals.
func (r *Repository) ReactionState(
	ctx context.Context, postID, userID uuid.UUID,
) (myReaction string, total int64, err error) {
	err = r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE((SELECT reaction_type FROM community_reactions WHERE post_id = $1 AND user_id = $2), ''),
			(SELECT count(*) FROM community_reactions WHERE post_id = $1)`,
		postID, userID).Scan(&myReaction, &total)
	if err != nil {
		return "", 0, fmt.Errorf("reaction state: %w", err)
	}
	return myReaction, total, nil
}
