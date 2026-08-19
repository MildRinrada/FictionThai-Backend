package wall

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/profiles"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// ErrNotFound covers every reason an entry or a wall cannot be reached. The
// service translates it into the single 404 all of them share.
var ErrNotFound = errors.New("wall entry not found")

// Repository is the only place that reads or writes `profile_comments`.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// visibleAccountSQL is "an account that has a public profile", against `u` -
// the same rule the public profile read applies. A wall must not outlive the
// page it belongs to.
const visibleAccountSQL = `
	u.deleted_at IS NULL AND u.status NOT IN ('deleted', 'banned')`

// Target resolves a `/users/:user` reference to the wall's owner and its
// switch.
//
// COALESCE(p.wall_enabled, TRUE) because user_profiles is LEFT JOINed: an
// account whose profile row is somehow missing has a wall that is OPEN, which
// is the documented default rather than an accidental closure.
func (r *Repository) Target(ctx context.Context, ref profiles.Ref) (Target, error) {
	var (
		where string
		arg   any
	)
	if ref.ByUsername() {
		// username is CITEXT, so this is the same case-insensitive comparison
		// registration used (docs/10 §7).
		where = `u.username = $1`
		arg = users.NormalizeUsername(ref.Username)
	} else {
		where = `u.id = $1`
		arg = ref.ID
	}

	var target Target
	err := r.db.QueryRowContext(ctx, `
		SELECT u.id, COALESCE(p.wall_enabled, TRUE)
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE `+where+` AND `+visibleAccountSQL, arg,
	).Scan(&target.UserID, &target.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, fmt.Errorf("resolve wall target: %w", err)
	}
	return target, nil
}

const entryColumns = `
	e.id, e.profile_user_id, e.author_id, e.body, e.status,
	e.created_at, e.deleted_at,
	u.id, u.username, p.display_name, p.avatar_url`

const entryFrom = `
	FROM profile_comments e
	JOIN users u ON u.id = e.author_id
	LEFT JOIN user_profiles p ON p.user_id = e.author_id`

type scanner interface{ Scan(...any) error }

func scanEntry(row scanner) (*Entry, error) {
	var entry Entry
	err := row.Scan(
		&entry.ID, &entry.ProfileUserID, &entry.AuthorID, &entry.Body,
		&entry.Status, &entry.CreatedAt, &entry.DeletedAt,
		&entry.Author.ID, &entry.Author.Username,
		&entry.Author.DisplayName, &entry.Author.AvatarURL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan wall entry: %w", err)
	}
	return &entry, nil
}

// listedSQL is the predicate for "an entry a reader sees": not withdrawn by its
// author, not acted on by the platform, and written by an account that still
// has a public profile.
//
// The account clause is why a deleted or banned writer's message leaves with
// them rather than standing on somebody else's page with a dead link under it.
const listedSQL = `
	e.deleted_at IS NULL AND e.status = 'visible' AND ` + visibleAccountSQL

// List returns one page of a wall, newest first - the order the index exists
// for (profile_comments_wall_idx).
func (r *Repository) List(
	ctx context.Context, profileUserID uuid.UUID, page pagination.Params,
) ([]Entry, int64, error) {
	where := entryFrom + ` WHERE e.profile_user_id = $1 AND ` + listedSQL

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*)`+where, profileUserID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count wall entries: %w", err)
	}
	if total == 0 {
		return []Entry{}, 0, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+entryColumns+where+
			` ORDER BY e.created_at DESC, e.id DESC LIMIT $2 OFFSET $3`,
		profileUserID, page.Limit(), page.Offset())
	if err != nil {
		return nil, 0, fmt.Errorf("list wall entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []Entry{}
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, *entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list wall entries: %w", err)
	}
	return entries, total, nil
}

// Find loads one entry with no visibility filter. The service decides who may
// act on it.
func (r *Repository) Find(ctx context.Context, entryID uuid.UUID) (*Entry, error) {
	return scanEntry(r.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+entryFrom+` WHERE e.id = $1`, entryID))
}

// Count reports how many entries one account has left on one wall, for the
// per-wall flood check the rate limiter cannot express.
func (r *Repository) CountByAuthor(
	ctx context.Context, profileUserID, authorID uuid.UUID,
) (int, error) {
	var total int
	err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM profile_comments
		WHERE profile_user_id = $1 AND author_id = $2
		  AND deleted_at IS NULL AND status = 'visible'`,
		profileUserID, authorID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count wall entries by author: %w", err)
	}
	return total, nil
}

// Create inserts one entry and returns it with its author card attached.
func (r *Repository) Create(
	ctx context.Context, profileUserID, authorID uuid.UUID, body string,
) (*Entry, error) {
	var id uuid.UUID
	if err := r.db.QueryRowContext(ctx, `
		INSERT INTO profile_comments (profile_user_id, author_id, body)
		VALUES ($1, $2, $3)
		RETURNING id`, profileUserID, authorID, body).Scan(&id); err != nil {
		return nil, fmt.Errorf("create wall entry: %w", err)
	}
	return r.Find(ctx, id)
}

// SoftDelete withdraws an entry. Soft, like a comment: the row survives, which
// keeps the trail and makes a repeat delete a no-op rather than a race.
func (r *Repository) SoftDelete(ctx context.Context, entryID uuid.UUID) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE profile_comments SET deleted_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`, entryID)
	if err != nil {
		return fmt.Errorf("delete wall entry: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete wall entry: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
