package notifications

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// ErrNotFound covers "no such notification for this recipient". The service
// translates; the same 404 answers "absent" and "someone else's"
// (docs/11 §31).
var ErrNotFound = errors.New("notification not found")

// Repository is the only place that reads or writes the notifications table.
// It also owns the DELIVERY queries the worker runs; those read other domains'
// tables (user_follows, novels, chapters, comments) through the same exported
// visibility predicates the rest of the system uses, so a notification can
// never be generated for content its recipient could not open.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// ---------------------------------------------------------------------------
// Recipient-facing reads and writes (docs/09 §23)
// ---------------------------------------------------------------------------

// List returns one page of the recipient's notifications, newest first -
// riding notifications_recipient_recency_idx (docs/08 §37).
func (r *Repository) List(
	ctx context.Context, recipientID uuid.UUID, page pagination.Params,
) ([]Notification, int64, error) {
	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM notifications WHERE recipient_id = $1`,
		recipientID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count notifications: %w", err)
	}
	if total == 0 {
		return []Notification{}, 0, nil
	}

	// The actor join is a LEFT JOIN twice over: actor_id may be NULL (system
	// notifications, hard-deleted actors) and a soft-deleted actor keeps the
	// notification but drops the card.
	rows, err := r.db.QueryContext(ctx, `
		SELECT nt.id, nt.recipient_id, nt.type, nt.entity_type, nt.entity_id,
		       nt.read_at, nt.created_at,
		       u.id, u.username, p.display_name, p.avatar_url
		FROM notifications nt
		LEFT JOIN users u ON u.id = nt.actor_id AND u.deleted_at IS NULL
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE nt.recipient_id = $1
		ORDER BY nt.created_at DESC, nt.id DESC
		LIMIT $2 OFFSET $3`, recipientID, page.Limit(), page.Offset())
	if err != nil {
		return nil, 0, fmt.Errorf("list notifications: %w", err)
	}
	defer func() { _ = rows.Close() }()

	notifications := []Notification{}
	for rows.Next() {
		var (
			n           Notification
			actorID     uuid.NullUUID
			username    sql.NullString
			displayName *string
			avatarURL   *string
		)
		if err := rows.Scan(
			&n.ID, &n.RecipientID, &n.Type, &n.EntityType, &n.EntityID,
			&n.ReadAt, &n.CreatedAt,
			&actorID, &username, &displayName, &avatarURL,
		); err != nil {
			return nil, 0, fmt.Errorf("scan notification: %w", err)
		}
		if actorID.Valid {
			n.Actor = &Actor{
				ID:          actorID.UUID,
				Username:    username.String,
				DisplayName: displayName,
				AvatarURL:   avatarURL,
			}
		}
		notifications = append(notifications, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list notifications: %w", err)
	}
	return notifications, total, nil
}

// UnreadCount answers the badge - an index-only walk of
// notifications_recipient_read_idx (docs/08 §37).
func (r *Repository) UnreadCount(ctx context.Context, recipientID uuid.UUID) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM notifications
		WHERE recipient_id = $1 AND read_at IS NULL`, recipientID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return count, nil
}

// MarkRead stamps one notification read. COALESCE makes a repeat idempotent
// while still matching the row, so "already read" is a success and only
// "absent or not yours" is ErrNotFound.
func (r *Repository) MarkRead(ctx context.Context, recipientID, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE notifications SET read_at = COALESCE(read_at, now())
		WHERE id = $1 AND recipient_id = $2`, id, recipientID)
	if err != nil {
		return fmt.Errorf("mark notification read: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark notification read result: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead stamps every unread notification and reports how many.
func (r *Repository) MarkAllRead(ctx context.Context, recipientID uuid.UUID) (int64, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE notifications SET read_at = now()
		WHERE recipient_id = $1 AND read_at IS NULL`, recipientID)
	if err != nil {
		return 0, fmt.Errorf("mark all notifications read: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mark all notifications read result: %w", err)
	}
	return affected, nil
}

// ---------------------------------------------------------------------------
// Delivery (worker side)
// ---------------------------------------------------------------------------

// Insert writes one notification row.
func (r *Repository) Insert(
	ctx context.Context, recipientID, actorID uuid.UUID,
	typ Type, entityType string, entityID uuid.UUID,
) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notifications (recipient_id, actor_id, type, entity_type, entity_id)
		VALUES ($1, $2, $3, $4, $5)`,
		recipientID, actorID, typ, entityType, entityID)
	if err != nil {
		return fmt.Errorf("insert notification: %w", err)
	}
	return nil
}

// InsertSystem writes a notification with NO actor - the moderation and
// system types, where naming the acting individual would itself be a leak
// (docs/11 §39).
func (r *Repository) InsertSystem(
	ctx context.Context, recipientID uuid.UUID,
	typ Type, entityType string, entityID uuid.UUID,
) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notifications (recipient_id, actor_id, type, entity_type, entity_id)
		VALUES ($1, NULL, $2, $3, $4)`,
		recipientID, typ, entityType, entityID)
	if err != nil {
		return fmt.Errorf("insert system notification: %w", err)
	}
	return nil
}

// DeliverChapterPublished fans a first publish out to the author's followers
// in ONE set-based statement - no per-follower round trips (docs/09 §23
// "New chapter → Queue → Notification Worker → Followers receive
// notification").
//
// The EXISTS guards re-check CURRENT state: if the fiction went private or
// the chapter was retracted between enqueue and delivery, nobody is notified
// of something they cannot open.
func (r *Repository) DeliverChapterPublished(
	ctx context.Context, actorID, novelID, chapterID uuid.UUID,
) (int64, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO notifications (recipient_id, actor_id, type, entity_type, entity_id)
		SELECT f.follower_id, $1, $4, $5, $3
		FROM user_follows f
		WHERE f.following_id = $1
		  -- The per-follow switch (library review 2026-08): following is not
		  -- the same as wanting an alert from everyone.
		  AND f.notify_new_chapters
		  AND EXISTS (
			SELECT 1 FROM novels n
			WHERE n.id = $2 AND n.author_id = $1
			  AND `+novels.ReadableSQLFor("f.follower_id")+`
		  )
		  AND EXISTS (
			SELECT 1 FROM chapters c
			WHERE c.id = $3 AND c.novel_id = $2 AND `+novels.LiveChapterSQL+`
		  )`,
		actorID, novelID, chapterID, TypeNovelUpdate, EntityChapter)
	if err != nil {
		return 0, fmt.Errorf("deliver chapter published: %w", err)
	}
	delivered, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("deliver chapter published result: %w", err)
	}
	return delivered, nil
}

// CommentDelivery is what the worker needs to route one comment's
// notifications.
type CommentDelivery struct {
	AuthorID      uuid.UUID // who wrote the comment
	NovelAuthorID uuid.UUID // who owns the fiction
	ParentAuthor  uuid.NullUUID
	Live          bool // still visible and not deleted
	Readable      bool // fiction still published to someone other than its owner
	// Anonymous marks a comment with no account behind it (§13D). Always false
	// for community comments, which require an account.
	Anonymous bool
}

// CommunityCommentInfo loads the routing facts for one community comment.
// Same shape as CommentDelivery: the comment's author, the POST's author in
// place of the fiction's, and liveness checks against the community tables.
// "Readable" here means the post is still published and not deleted - the
// recipient is always its author, who is inside every visibility audience.
func (r *Repository) CommunityCommentInfo(
	ctx context.Context, commentID uuid.UUID,
) (*CommentDelivery, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT c.author_id, p.author_id, parent.author_id,
		       (c.deleted_at IS NULL AND c.status = 'visible'),
		       (p.deleted_at IS NULL AND p.status = 'published')
		FROM community_comments c
		JOIN community_posts p ON p.id = c.post_id
		LEFT JOIN community_comments parent ON parent.id = c.parent_id
		WHERE c.id = $1`, commentID)

	var info CommentDelivery
	err := row.Scan(
		&info.AuthorID, &info.NovelAuthorID, &info.ParentAuthor,
		&info.Live, &info.Readable,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load community comment delivery info: %w", err)
	}
	return &info, nil
}

// ReactionDelivery is what the worker needs to route one reaction.
type ReactionDelivery struct {
	PostAuthorID uuid.UUID
	Live         bool // post still published and not deleted
	Reacted      bool // the actor's reaction still exists at delivery time
}

// ReactionInfo loads the routing facts for one (post, actor) reaction.
func (r *Repository) ReactionInfo(
	ctx context.Context, postID, actorID uuid.UUID,
) (*ReactionDelivery, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT p.author_id,
		       (p.deleted_at IS NULL AND p.status = 'published'),
		       EXISTS (
			SELECT 1 FROM community_reactions r
			WHERE r.post_id = p.id AND r.user_id = $2
		       )
		FROM community_posts p
		WHERE p.id = $1`, postID, actorID)

	var info ReactionDelivery
	err := row.Scan(&info.PostAuthorID, &info.Live, &info.Reacted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load reaction delivery info: %w", err)
	}
	return &info, nil
}

// HasNotification reports whether a notification with this exact routing
// already exists. The worker's spam guard: a react → unreact → react cycle
// delivers at most one community_reaction notification per (author, post)
// pair, ever (docs/01 §20.2 "should not encourage spam").
func (r *Repository) HasNotification(
	ctx context.Context, recipientID, actorID uuid.UUID, typ Type, entityType string, entityID uuid.UUID,
) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM notifications
			WHERE recipient_id = $1 AND actor_id = $2
			  AND type = $3 AND entity_type = $4 AND entity_id = $5
		)`, recipientID, actorID, typ, entityType, entityID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check notification exists: %w", err)
	}
	return exists, nil
}

// CommentInfo loads the routing facts for one comment.
//
// `Readable` here asks novels.PublishedSQL rather than ReadableSQL, because the
// recipients are the fiction's author and the person who was replied to - not a
// browsing reader. A comment on members-only or followers-only work still has
// to reach them, and asking the audience question would silently drop it.
func (r *Repository) CommentInfo(ctx context.Context, commentID uuid.UUID) (*CommentDelivery, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT c.user_id, n.author_id, parent.user_id,
		       (c.deleted_at IS NULL AND c.status = 'visible'),
		       (`+novels.PublishedSQL+`)
		FROM comments c
		JOIN novels n ON n.id = c.novel_id
		LEFT JOIN comments parent ON parent.id = c.parent_id
		WHERE c.id = $1`, commentID)

	var info CommentDelivery
	var author uuid.NullUUID
	err := row.Scan(
		&author, &info.NovelAuthorID, &info.ParentAuthor,
		&info.Live, &info.Readable,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load comment delivery info: %w", err)
	}
	// A guest comment has no account behind it (§13D), so there is no actor to
	// attribute the notification to. Anonymous tells the worker to drop the
	// delivery rather than invent one - the author meets that comment in their
	// review queue, which is where a held comment belongs anyway.
	info.Anonymous = !author.Valid
	info.AuthorID = author.UUID
	return &info, nil
}
