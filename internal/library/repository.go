package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// ErrNotFound covers "no such row" for progress lookups. The repository never
// builds HTTP responses; the service translates.
var ErrNotFound = errors.New("library row not found")

// Repository is the only place that reads or writes bookmarks, user_follows,
// and reading_progress.
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

// visibleNovelSQL is the predicate for "a fiction this user's shelf may show",
// against alias `n`: anything this reader may open, plus their own work.
//
// It asks novels.ReadableSQLFor rather than the guest-tier constant, because
// the shelf belongs to a KNOWN person: a members-only or followers-only fiction
// they bookmarked is one they can still open, and the guest predicate would
// have made it vanish from their own shelf while the link kept working (§13C).
//
// The owner clause exists because a writer may bookmark or read their own
// private draft; without it, that entry would silently vanish too. Neither
// clause can widen anyone else's view - userID is always the authenticated
// caller (docs/11 §31).
func visibleNovelSQL(args *argList, userID uuid.UUID) string {
	viewer := args.add(novels.ViewerValue(userID)) + "::uuid"
	return `((` + novels.ReadableSQLFor(viewer) + `)
		OR (n.deleted_at IS NULL AND n.author_id = ` + viewer + `))`
}

// resumableChapterSQL is the predicate for "a chapter this user may resume at",
// against aliases `c` and `n`: live chapters, plus every non-deleted chapter of
// the user's own fiction.
func resumableChapterSQL(args *argList, userID uuid.UUID) string {
	return `((` + novels.LiveChapterSQL + `)
		OR (c.deleted_at IS NULL AND n.author_id = ` + args.add(userID) + `))`
}

// ---------------------------------------------------------------------------
// Bookmarks (docs/08 §16)
// ---------------------------------------------------------------------------

// UpsertBookmark saves a fiction to the user's shelf.
//
// ON CONFLICT DO NOTHING makes a repeat idempotent: the PRIMARY KEY is the
// duplicate guard, and a pre-flight SELECT would still race (docs/08 §34,
// docs/09 §33). The original created_at is kept - re-bookmarking is not
// re-shelving.
// The display counter is maintained in the SAME statement as the row it counts
// (docs/PHASE-12-STORY-DEPTH.md §12C), so it cannot drift from it. The CTE makes
// the increment conditional on the insert actually happening: a repeat bookmark
// inserts nothing, the subquery is NULL, and no counter moves.
func (r *Repository) UpsertBookmark(ctx context.Context, userID, novelID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		WITH inserted AS (
			INSERT INTO bookmarks (user_id, novel_id)
			VALUES ($1, $2)
			ON CONFLICT (user_id, novel_id) DO NOTHING
			RETURNING novel_id
		)
		UPDATE novels SET bookmark_count = bookmark_count + 1
		WHERE id = (SELECT novel_id FROM inserted)`, userID, novelID)
	if err != nil {
		return fmt.Errorf("upsert bookmark: %w", err)
	}
	return nil
}

// DeleteBookmark removes a fiction from the user's shelf. Deleting an absent
// row is a no-op, not an error: removal must ALWAYS work, including for a
// fiction the user can no longer read (docs/01 §11 "Users should be able to
// remove items from their library").
func (r *Repository) DeleteBookmark(ctx context.Context, userID, novelID uuid.UUID) error {
	// GREATEST clamps at zero. A counter that has somehow fallen behind reality
	// must not be able to go negative and render as "-1 บันทึกไว้".
	_, err := r.db.ExecContext(ctx, `
		WITH deleted AS (
			DELETE FROM bookmarks WHERE user_id = $1 AND novel_id = $2
			RETURNING novel_id
		)
		UPDATE novels SET bookmark_count = GREATEST(bookmark_count - 1, 0)
		WHERE id = (SELECT novel_id FROM deleted)`, userID, novelID)
	if err != nil {
		return fmt.Errorf("delete bookmark: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Fiction likes (docs/01 §20.2, docs/PHASE-12-STORY-DEPTH.md §12C)
// ---------------------------------------------------------------------------

// UpsertReaction records that the user likes this fiction.
//
// One row per user per fiction, so liking twice is idempotent and the count
// cannot be farmed by repeat submission - the primary key is the guard, and a
// pre-flight SELECT would still race.
func (r *Repository) UpsertReaction(ctx context.Context, userID, novelID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		WITH inserted AS (
			INSERT INTO novel_reactions (novel_id, user_id)
			VALUES ($2, $1)
			ON CONFLICT (novel_id, user_id) DO NOTHING
			RETURNING novel_id
		)
		UPDATE novels SET like_count = like_count + 1
		WHERE id = (SELECT novel_id FROM inserted)`, userID, novelID)
	if err != nil {
		return fmt.Errorf("upsert reaction: %w", err)
	}
	return nil
}

// DeleteReaction withdraws a like. Removing an absent one is a no-op: undoing
// must always work, for the same reason removing a bookmark always does.
func (r *Repository) DeleteReaction(ctx context.Context, userID, novelID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		WITH deleted AS (
			DELETE FROM novel_reactions WHERE novel_id = $2 AND user_id = $1
			RETURNING novel_id
		)
		UPDATE novels SET like_count = GREATEST(like_count - 1, 0)
		WHERE id = (SELECT novel_id FROM deleted)`, userID, novelID)
	if err != nil {
		return fmt.Errorf("delete reaction: %w", err)
	}
	return nil
}

// HasReacted reports whether the user has liked this fiction.
func (r *Repository) HasReacted(ctx context.Context, userID, novelID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM novel_reactions WHERE novel_id = $2 AND user_id = $1
		)`, userID, novelID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check reaction: %w", err)
	}
	return exists, nil
}

// IsBookmarked reports whether the user has saved this fiction.
func (r *Repository) IsBookmarked(ctx context.Context, userID, novelID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bookmarks WHERE user_id = $1 AND novel_id = $2
		)`, userID, novelID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check bookmark: %w", err)
	}
	return exists, nil
}

// bookmarkRow is one page row of the shelf; the fiction card is loaded
// separately through the novels domain.
type bookmarkRow struct {
	NovelID   uuid.UUID
	CreatedAt sql.NullTime
}

// ListBookmarks returns one page of the user's shelf, newest first.
//
// The join applies the visibility predicate INSIDE the query so both the page
// and the total count exclude fictions the caller may no longer read - a
// bookmark of a now-private novel is retained but never shown (docs/08 §3,
// docs/11 §31).
func (r *Repository) ListBookmarks(
	ctx context.Context, userID uuid.UUID, status string, page pagination.Params,
) ([]bookmarkRow, int64, error) {
	args := &argList{}
	where := ` FROM bookmarks b
		JOIN novels n ON n.id = b.novel_id
		WHERE b.user_id = ` + args.add(userID) + ` AND ` + visibleNovelSQL(args, userID)

	// Optional section filter - the library's "Completed" shelf is
	// ?status=completed (docs/03 §13). Validated by the service against the
	// novels vocabulary, bound here.
	if status != "" {
		where += ` AND n.status = ` + args.add(status)
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*)`+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count bookmarks: %w", err)
	}
	if total == 0 {
		return []bookmarkRow{}, 0, nil
	}

	query := `SELECT b.novel_id, b.created_at` + where +
		` ORDER BY b.created_at DESC, b.novel_id DESC` +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list bookmarks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []bookmarkRow{}
	for rows.Next() {
		var row bookmarkRow
		if err := rows.Scan(&row.NovelID, &row.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan bookmark: %w", err)
		}
		entries = append(entries, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list bookmarks: %w", err)
	}
	return entries, total, nil
}

// ---------------------------------------------------------------------------
// Follows (docs/08 §17)
// ---------------------------------------------------------------------------

// UpsertFollow records that follower follows following. Idempotent, like
// bookmarks. The service rejects self-follows first; the CHECK constraint is
// the backstop.
//
// The returned bool reports whether a row was actually INSERTED - false on an
// idempotent repeat. Phase 6 hangs the new_follower notification on it, so
// hammering the follow button can never notify the author more than once
// (docs/08 §23.1, docs/07 §37).
// users.follower_count is maintained in the SAME statement as the follow row
// (Phase 12E), so the number a profile shows cannot drift from the table it
// counts. The UPDATE only fires when the INSERT actually inserted, which is
// also what keeps the returned bool - and therefore the notification - honest
// on an idempotent repeat.
func (r *Repository) UpsertFollow(ctx context.Context, followerID, followingID uuid.UUID) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		WITH inserted AS (
			INSERT INTO user_follows (follower_id, following_id)
			VALUES ($1, $2)
			ON CONFLICT (follower_id, following_id) DO NOTHING
			RETURNING following_id
		)
		UPDATE users SET follower_count = follower_count + 1
		WHERE id = (SELECT following_id FROM inserted)`, followerID, followingID)
	if err != nil {
		return false, fmt.Errorf("upsert follow: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("upsert follow result: %w", err)
	}
	return inserted > 0, nil
}

// DeleteFollow removes a follow. Idempotent - unfollowing someone already
// unfollowed changes nothing, and GREATEST keeps the counter off negative
// numbers even if a row were ever removed by another path.
func (r *Repository) DeleteFollow(ctx context.Context, followerID, followingID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		WITH deleted AS (
			DELETE FROM user_follows
			WHERE follower_id = $1 AND following_id = $2
			RETURNING following_id
		)
		UPDATE users SET follower_count = GREATEST(follower_count - 1, 0)
		WHERE id = (SELECT following_id FROM deleted)`,
		followerID, followingID)
	if err != nil {
		return fmt.Errorf("delete follow: %w", err)
	}
	return nil
}

// IsFollowing answers the follow-status read (docs/09 §19).
func (r *Repository) IsFollowing(ctx context.Context, followerID, followingID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_follows WHERE follower_id = $1 AND following_id = $2
		)`, followerID, followingID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check follow: %w", err)
	}
	return exists, nil
}

// ListFollowing returns one page of the authors the user follows, newest first.
//
// The author card carries only the same public profile fields a fiction card
// does - never an email address (docs/08 §1.4, docs/10 §8). Soft-deleted users
// are excluded the same way everywhere else excludes them.
func (r *Repository) ListFollowing(
	ctx context.Context, userID uuid.UUID, page pagination.Params,
) ([]FollowedAuthor, int64, error) {
	args := &argList{}
	where := ` FROM user_follows f
		JOIN users u ON u.id = f.following_id AND u.deleted_at IS NULL
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE f.follower_id = ` + args.add(userID)

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*)`+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count following: %w", err)
	}
	if total == 0 {
		return []FollowedAuthor{}, 0, nil
	}

	// The two correlated subqueries (library review 2026-08) reuse the
	// guest-tier predicates verbatim: the outer query owns no n or c alias,
	// so the shared constants apply unchanged and cannot drift. Guest tier on
	// purpose - "ลงตอนใหม่ล่าสุด" describes the author's PUBLIC activity.
	query := `SELECT u.id, u.username, p.display_name, p.avatar_url, f.created_at,
			f.notify_new_chapters,
			(SELECT max(COALESCE(c.published_at, c.scheduled_at))
			 FROM novels n JOIN chapters c ON c.novel_id = n.id
			 WHERE n.author_id = u.id AND ` + novels.ReadableSQL + `
			   AND ` + novels.LiveChapterSQL + `) AS last_published_at,
			(SELECT count(*) FROM novels n
			 WHERE n.author_id = u.id AND n.status = 'ongoing'
			   AND ` + novels.ReadableSQL + `) AS writing_count` +
		where +
		` ORDER BY f.created_at DESC, f.following_id DESC` +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list following: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []FollowedAuthor{}
	for rows.Next() {
		var entry FollowedAuthor
		var lastPublished sql.NullTime
		if err := rows.Scan(
			&entry.Author.ID, &entry.Author.Username,
			&entry.Author.DisplayName, &entry.Author.AvatarURL,
			&entry.FollowedAt, &entry.NotifyNewChapters,
			&lastPublished, &entry.WritingCount,
		); err != nil {
			return nil, 0, fmt.Errorf("scan following: %w", err)
		}
		if lastPublished.Valid {
			at := lastPublished.Time
			entry.LastPublishedAt = &at
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list following: %w", err)
	}
	return entries, total, nil
}

// ---------------------------------------------------------------------------
// Reading progress (docs/08 §18)
// ---------------------------------------------------------------------------

// ChapterResumable reports whether the chapter belongs to the novel and may be
// a resume point for this user.
//
// The novel_id check is the anti-IDOR guard: a chapter id from another fiction
// can never be attached to this novel's progress row (docs/11 §21). Liveness
// uses the same predicate reads use, so progress can never point a client at a
// chapter it could not fetch.
func (r *Repository) ChapterResumable(
	ctx context.Context, userID, novelID, chapterID uuid.UUID,
) (bool, error) {
	args := &argList{}
	query := `SELECT EXISTS (
		SELECT 1 FROM chapters c
		JOIN novels n ON n.id = c.novel_id
		WHERE c.id = ` + args.add(chapterID) + `
		  AND c.novel_id = ` + args.add(novelID) + `
		  AND ` + resumableChapterSQL(args, userID) + `
	)`

	var exists bool
	if err := r.db.QueryRowContext(ctx, query, args.args...).Scan(&exists); err != nil {
		return false, fmt.Errorf("check chapter resumable: %w", err)
	}
	return exists, nil
}

// SaveProgress upserts the user's position in one fiction - the single-row
// write behind PUT /novels/:novel/progress.
//
// This is the highest-frequency write on the platform, which is exactly why it
// is ONE statement against the primary key: no transaction, no read-modify-
// write, no fan-out (docs/09 §17 "The server should avoid excessive database
// writes").
func (r *Repository) SaveProgress(
	ctx context.Context, userID, novelID, chapterID uuid.UUID, percent float64,
) (*Progress, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO reading_progress (user_id, novel_id, chapter_id, progress_percent, last_read_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id, novel_id) DO UPDATE
		SET chapter_id = EXCLUDED.chapter_id,
		    progress_percent = EXCLUDED.progress_percent,
		    last_read_at = now()
		RETURNING novel_id, chapter_id, progress_percent, last_read_at`,
		userID, novelID, chapterID, percent)

	var progress Progress
	if err := row.Scan(
		&progress.NovelID, &progress.ChapterID,
		&progress.ProgressPercent, &progress.LastReadAt,
	); err != nil {
		return nil, fmt.Errorf("save progress: %w", err)
	}
	return &progress, nil
}

// GetProgress loads the user's position in one fiction.
func (r *Repository) GetProgress(
	ctx context.Context, userID, novelID uuid.UUID,
) (*Progress, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT novel_id, chapter_id, progress_percent, last_read_at
		FROM reading_progress
		WHERE user_id = $1 AND novel_id = $2`, userID, novelID)

	var progress Progress
	err := row.Scan(
		&progress.NovelID, &progress.ChapterID,
		&progress.ProgressPercent, &progress.LastReadAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get progress: %w", err)
	}
	return &progress, nil
}

// progressRow is one page row of Continue Reading; the fiction card is loaded
// separately through the novels domain. The chapter fields are NULL when the
// chapter the reader stopped at is no longer reachable.
type progressRow struct {
	NovelID         uuid.UUID
	ProgressPercent float64
	LastReadAt      sql.NullTime

	ChapterID     uuid.NullUUID
	ChapterNumber sql.NullInt64
	ChapterSlug   sql.NullString
	ChapterTitle  sql.NullString

	TotalChapters int
	ChaptersLeft  int
	NewSinceRead  int
}

// liveChapterLcSQL is novels.LiveChapterSQL re-aliased for the correlated
// subqueries inside ListProgress, whose outer query already owns `c`.
const liveChapterLcSQL = `lc.deleted_at IS NULL AND (
	(lc.status = 'published' AND (lc.published_at IS NULL OR lc.published_at <= now()))
	OR
	(lc.status = 'scheduled' AND lc.scheduled_at IS NOT NULL AND lc.scheduled_at <= now())
)`

// ListProgress returns one page of Continue Reading, most recent first.
//
// The ORDER BY rides reading_progress_user_recency_idx - this is the query the
// index exists for. The chapter join is a LEFT JOIN on the resumable predicate:
// an unpublished chapter drops the resume LINK, never the entry (docs/08 §3 -
// nothing an author does deletes a reader's progress).
func (r *Repository) ListProgress(
	ctx context.Context, userID uuid.UUID, page pagination.Params,
) ([]progressRow, int64, error) {
	args := &argList{}
	from := ` FROM reading_progress rp
		JOIN novels n ON n.id = rp.novel_id`
	where := ` WHERE rp.user_id = ` + args.add(userID) + ` AND ` + visibleNovelSQL(args, userID)

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*)`+from+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count progress: %w", err)
	}
	if total == 0 {
		return []progressRow{}, 0, nil
	}

	// Positional parameters are numbered by the order they were ADDED, not by
	// where they appear in the text, so extending the same argList for the
	// chapter join and the page bounds is safe.
	// The three counts ride as correlated subqueries over
	// chapters_novel_status_idx - one query per page, not one per card
	// (docs/07 §67). A vanished resume chapter (c.* NULL) counts everything
	// as "left", which is the honest answer when the place was lost.
	query := `SELECT rp.novel_id, rp.progress_percent, rp.last_read_at,
			c.id, c.chapter_number, c.slug, c.title,
			(SELECT count(*) FROM chapters lc
			 WHERE lc.novel_id = rp.novel_id AND ` + liveChapterLcSQL + `) AS total_chapters,
			(SELECT count(*) FROM chapters lc
			 WHERE lc.novel_id = rp.novel_id AND ` + liveChapterLcSQL + `
			   AND lc.chapter_number > COALESCE(c.chapter_number, 0)) AS chapters_left,
			(SELECT count(*) FROM chapters lc
			 WHERE lc.novel_id = rp.novel_id AND ` + liveChapterLcSQL + `
			   AND COALESCE(lc.published_at, lc.scheduled_at) > rp.last_read_at) AS new_since_read` +
		from +
		` LEFT JOIN chapters c ON c.id = rp.chapter_id AND ` + resumableChapterSQL(args, userID) +
		where +
		` ORDER BY rp.last_read_at DESC, rp.novel_id DESC` +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list progress: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []progressRow{}
	for rows.Next() {
		var row progressRow
		if err := rows.Scan(
			&row.NovelID, &row.ProgressPercent, &row.LastReadAt,
			&row.ChapterID, &row.ChapterNumber, &row.ChapterSlug, &row.ChapterTitle,
			&row.TotalChapters, &row.ChaptersLeft, &row.NewSinceRead,
		); err != nil {
			return nil, 0, fmt.Errorf("scan progress: %w", err)
		}
		entries = append(entries, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list progress: %w", err)
	}
	return entries, total, nil
}
