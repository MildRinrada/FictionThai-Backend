package comments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// ErrNotFound covers "no such comment". The service translates to HTTP.
var ErrNotFound = errors.New("comment not found")

// Repository is the only place that reads or writes the comments table.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// commentColumns is every storage column plus the joined author card and the
// visible-reply count. One query per page, like every other listing
// (docs/07 §67).
const commentColumns = `
	c.id, c.user_id, c.guest_name, c.novel_id, c.chapter_id, c.parent_id,
	c.content, c.status, c.created_at, c.updated_at, c.deleted_at,
	u.id, u.username, p.display_name, p.avatar_url,
	(
		SELECT count(*) FROM comments r
		WHERE r.parent_id = c.id AND r.deleted_at IS NULL AND r.status = 'visible'
	) AS reply_count`

// The users join is a LEFT join since §13D: a guest comment has no account row
// behind it, and an inner join would silently drop every one of them from every
// listing - the worst possible failure mode for a feature whose whole point is
// that those comments exist.
const commentFrom = `
	FROM comments c
	LEFT JOIN users u ON u.id = c.user_id
	LEFT JOIN user_profiles p ON p.user_id = c.user_id`

type scanner interface{ Scan(...any) error }

func scanComment(row scanner) (*Comment, error) {
	var c Comment
	var authorID uuid.NullUUID
	var username sql.NullString
	var displayName, avatarURL *string

	err := row.Scan(
		&c.ID, &c.UserID, &c.GuestName, &c.NovelID, &c.ChapterID, &c.ParentID,
		&c.Content, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.DeletedAt,
		&authorID, &username, &displayName, &avatarURL,
		&c.ReplyCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan comment: %w", err)
	}
	// Built only when the join found an account. A guest comment leaves Author
	// nil, so nothing downstream can render an empty card as if it were one.
	if authorID.Valid {
		c.Author = &Author{
			ID:          authorID.UUID,
			Username:    username.String,
			DisplayName: displayName,
			AvatarURL:   avatarURL,
		}
	}
	return &c, nil
}

// argList builds a positional-parameter list, so every value is bound rather
// than interpolated (docs/11 §15).
type argList struct {
	args []any
}

func (a *argList) add(value any) string {
	a.args = append(a.args, value)
	return "$" + strconv.Itoa(len(a.args))
}

// CreateParams is a validated insert. Exactly one of UserID and GuestName is
// set; the service decides which, and the database refuses anything else.
type CreateParams struct {
	UserID    *uuid.UUID
	GuestName *string
	NovelID   uuid.UUID
	ChapterID *uuid.UUID
	ParentID  *uuid.UUID
	Content   string
	// Status is the state the comment is BORN in - visible, or pending when
	// the fiction holds comments for review (§13D). The service decides it;
	// the column default is never relied on, so the rule lives in one place.
	Status Status
}

// Create inserts a comment and returns it with its author card in one round
// trip. The CTE is aliased `c`, so commentColumns applies unchanged; the
// reply-count subquery sees the pre-insert table snapshot, which is correct -
// a fresh comment has no replies.
func (r *Repository) Create(ctx context.Context, params CreateParams) (*Comment, error) {
	row := r.db.QueryRowContext(ctx, `
		WITH inserted AS (
			INSERT INTO comments
				(user_id, guest_name, novel_id, chapter_id, parent_id, content, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING *
		)
		SELECT `+commentColumns+`
		FROM inserted c
		LEFT JOIN users u ON u.id = c.user_id
		LEFT JOIN user_profiles p ON p.user_id = c.user_id`,
		params.UserID, params.GuestName, params.NovelID,
		params.ChapterID, params.ParentID, params.Content, params.Status)

	comment, err := scanComment(row)
	if err != nil {
		return nil, fmt.Errorf("create comment: %w", err)
	}
	return comment, nil
}

// CountPending returns how many comments on one fiction are waiting for its
// author. It backs the badge in the studio, so a writer who opened their thread
// to guests is never left wondering whether anything arrived (§13D).
func (r *Repository) CountPending(ctx context.Context, novelID uuid.UUID) (int64, error) {
	var total int64
	err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM comments c
		WHERE c.novel_id = $1 AND c.status = 'pending' AND c.deleted_at IS NULL`,
		novelID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count pending comments: %w", err)
	}
	return total, nil
}

// CountSince returns how many comments landed on one fiction in the last N
// days. It backs "คอมเมนต์ใหม่สัปดาห์นี้" on the studio overview (§13R).
//
// Every comment counts, whatever its moderation status: the number answers
// "what arrived", and a writer who is holding comments for review is exactly
// the writer who most needs to know that some did.
func (r *Repository) CountSince(ctx context.Context, novelID uuid.UUID, days int) (int64, error) {
	var total int64
	err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM comments c
		WHERE c.novel_id = $1
		  AND c.deleted_at IS NULL
		  AND c.created_at > now() - make_interval(days => $2)`,
		novelID, days).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count recent comments: %w", err)
	}
	return total, nil
}

// CountsByDay returns one entry per UTC day - oldest first, today last, zeros
// filled. The same shape as the view counters' ViewsByDay, so the overview's
// two sparklines cannot disagree about what a "day" is.
func (r *Repository) CountsByDay(ctx context.Context, novelID uuid.UUID, days int) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT (now() AT TIME ZONE 'utc')::date - (c.created_at AT TIME ZONE 'utc')::date,
		       count(*)
		FROM comments c
		WHERE c.novel_id = $1
		  AND c.deleted_at IS NULL
		  AND (c.created_at AT TIME ZONE 'utc')::date
		      > (now() AT TIME ZONE 'utc')::date - $2::int
		GROUP BY 1`,
		novelID, days)
	if err != nil {
		return nil, fmt.Errorf("daily comments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	series := make([]int64, days)
	for rows.Next() {
		var ago int
		var total int64
		if err := rows.Scan(&ago, &total); err != nil {
			return nil, fmt.Errorf("scan daily comments: %w", err)
		}
		if index := days - 1 - ago; index >= 0 && index < days {
			series[index] = total
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("daily comments: %w", err)
	}
	return series, nil
}

// RecentComment is one comment reduced to what an activity line shows (§13R).
//
// A NAME and a place, never an account. The studio's promise to writers is that
// the platform keeps no per-reader reading record, and a feed that carried user
// ids beside chapters would be one - so the id stays in the database and what
// comes out is the display name the comment already shows publicly.
type RecentComment struct {
	ID            uuid.UUID
	AuthorName    string
	ChapterSlug   *string
	ChapterTitle  *string
	ChapterNumber *int
	Excerpt       string
	CreatedAt     time.Time
}

// RecentForNovel lists the newest comments on one fiction, for its author.
func (r *Repository) RecentForNovel(
	ctx context.Context, novelID uuid.UUID, limit int,
) ([]RecentComment, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id,
		       COALESCE(NULLIF(p.display_name, ''), u.username, c.guest_name, 'ผู้อ่าน'),
		       ch.slug, ch.title, ch.chapter_number,
		       left(c.content, 120),
		       c.created_at
		FROM comments c
		LEFT JOIN users u ON u.id = c.user_id
		LEFT JOIN user_profiles p ON p.user_id = c.user_id
		LEFT JOIN chapters ch ON ch.id = c.chapter_id
		WHERE c.novel_id = $1 AND c.deleted_at IS NULL
		ORDER BY c.created_at DESC, c.id DESC
		LIMIT $2`, novelID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent comments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := []RecentComment{}
	for rows.Next() {
		var item RecentComment
		if err := rows.Scan(&item.ID, &item.AuthorName, &item.ChapterSlug,
			&item.ChapterTitle, &item.ChapterNumber, &item.Excerpt,
			&item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan recent comment: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recent comments: %w", err)
	}
	return items, nil
}

// SetStatus moves the platform's moderation axis (docs/08 §20.1). It never
// touches deleted_at - that column belongs to the author.
func (r *Repository) SetStatus(ctx context.Context, id uuid.UUID, status Status) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE comments SET status = $2 WHERE id = $1 AND deleted_at IS NULL`, id, status)
	if err != nil {
		return fmt.Errorf("set comment status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set comment status: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Find loads one comment by id, whatever its state - the SERVICE decides what
// a deleted or moderated row means for each operation.
func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*Comment, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+commentColumns+commentFrom+` WHERE c.id = $1`, id)
	return scanComment(row)
}

// Filter selects one listing scope.
type Filter struct {
	// NovelID with a nil ChapterID lists the fiction-level thread
	// (chapter_id IS NULL); with ChapterID set it lists that chapter's thread.
	NovelID   uuid.UUID
	ChapterID *uuid.UUID

	// ParentID lists the replies of one comment instead; NovelID is ignored.
	ParentID *uuid.UUID

	// Status selects the moderation state. Empty means visible - the reader
	// listing - so a caller that forgets to set it gets the safe answer rather
	// than someone's unreviewed comment.
	Status Status

	// AllLevels lists everything on one fiction regardless of chapter or thread
	// depth. It exists for the author's review queue, where a pending reply is
	// exactly as much waiting as a pending top-level comment.
	AllLevels bool
}

// List returns one page of visible comments.
//
// Top-level threads are newest first - readers come for the latest reaction.
// Replies are oldest first: a reply thread is a conversation and reads
// top-to-bottom. Both orders tie-break on id for a stable page walk.
func (r *Repository) List(
	ctx context.Context, filter Filter, page pagination.Params,
) ([]Comment, int64, error) {
	args := &argList{}

	status := filter.Status
	if status == "" {
		status = StatusVisible
	}
	where := ` WHERE c.deleted_at IS NULL AND c.status = ` + args.add(status)
	order := ` ORDER BY c.created_at DESC, c.id DESC`

	switch {
	case filter.AllLevels:
		// The review queue: every level, newest first, riding
		// comments_pending_idx.
		where += ` AND c.novel_id = ` + args.add(filter.NovelID)
	case filter.ParentID != nil:
		where += ` AND c.parent_id = ` + args.add(*filter.ParentID)
		order = ` ORDER BY c.created_at ASC, c.id ASC`
	case filter.ChapterID != nil:
		// Rides comments_chapter_idx (docs/08 §37).
		where += ` AND c.chapter_id = ` + args.add(*filter.ChapterID) + ` AND c.parent_id IS NULL`
	default:
		// Rides comments_novel_idx (docs/08 §37).
		where += ` AND c.novel_id = ` + args.add(filter.NovelID) +
			` AND c.chapter_id IS NULL AND c.parent_id IS NULL`
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM comments c`+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count comments: %w", err)
	}
	if total == 0 {
		return []Comment{}, 0, nil
	}

	query := `SELECT ` + commentColumns + commentFrom + where + order +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list comments: %w", err)
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
		return nil, 0, fmt.Errorf("list comments: %w", err)
	}
	return comments, total, nil
}

// UpdateContent replaces a comment's text and stamps updated_at, which is what
// makes the public `edited` flag honest.
// LikeStats returns, for one page of comments, how many hearts each carries
// and which of them the viewer already pressed. Two queries per PAGE, never
// per row (docs/07 §67). A nil viewer skips the second.
func (r *Repository) LikeStats(
	ctx context.Context, commentIDs []uuid.UUID, viewer uuid.UUID,
) (map[uuid.UUID]int64, map[uuid.UUID]bool, error) {
	counts := map[uuid.UUID]int64{}
	mine := map[uuid.UUID]bool{}
	if len(commentIDs) == 0 {
		return counts, mine, nil
	}

	countArgs := &argList{}
	holes := make([]string, 0, len(commentIDs))
	for _, id := range commentIDs {
		holes = append(holes, countArgs.add(id))
	}
	in := strings.Join(holes, ", ")

	rows, err := r.db.QueryContext(ctx,
		`SELECT comment_id, count(*) FROM comment_likes
		 WHERE comment_id IN (`+in+`) GROUP BY comment_id`, countArgs.args...)
	if err != nil {
		return nil, nil, fmt.Errorf("count likes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id uuid.UUID
		var count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, nil, fmt.Errorf("scan like count: %w", err)
		}
		counts[id] = count
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if viewer == uuid.Nil {
		return counts, mine, nil
	}
	mineArgs := &argList{}
	holes = holes[:0]
	for _, id := range commentIDs {
		holes = append(holes, mineArgs.add(id))
	}
	viewerHole := mineArgs.add(viewer)
	liked, err := r.db.QueryContext(ctx,
		`SELECT comment_id FROM comment_likes
		 WHERE comment_id IN (`+strings.Join(holes, ", ")+`) AND user_id = `+viewerHole,
		mineArgs.args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read viewer likes: %w", err)
	}
	defer func() { _ = liked.Close() }()
	for liked.Next() {
		var id uuid.UUID
		if err := liked.Scan(&id); err != nil {
			return nil, nil, fmt.Errorf("scan viewer like: %w", err)
		}
		mine[id] = true
	}
	return counts, mine, liked.Err()
}

// LikeCount is one comment's current total - what a like/unlike returns.
func (r *Repository) LikeCount(ctx context.Context, commentID uuid.UUID) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM comment_likes WHERE comment_id = $1`, commentID,
	).Scan(&count)
	return count, err
}

// Like records one reader's heart. Idempotent: pressing twice is one heart
// (docs/09 §33's shape for likes).
func (r *Repository) Like(ctx context.Context, commentID, userID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO comment_likes (comment_id, user_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, commentID, userID)
	return err
}

// Unlike takes the heart back. Idempotent for the same reason.
func (r *Repository) Unlike(ctx context.Context, commentID, userID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM comment_likes WHERE comment_id = $1 AND user_id = $2`,
		commentID, userID)
	return err
}

func (r *Repository) UpdateContent(ctx context.Context, id uuid.UUID, content string) (*Comment, error) {
	row := r.db.QueryRowContext(ctx, `
		WITH updated AS (
			UPDATE comments SET content = $2, updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL
			RETURNING *
		)
		SELECT `+commentColumns+`
		FROM updated c
		JOIN users u ON u.id = c.user_id
		LEFT JOIN user_profiles p ON p.user_id = c.user_id`, id, content)

	return scanComment(row)
}

// SoftDelete marks a comment taken back by its author. The row survives so
// existing replies keep a parent and moderation keeps an audit trail
// (docs/08 §37); it simply never lists again.
func (r *Repository) SoftDelete(ctx context.Context, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE comments SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete comment result: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
