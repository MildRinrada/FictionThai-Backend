package achievements

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/users"
)

// Repository is the SQL implementation of Store.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// Compile-time proof that the production store satisfies the port the service
// depends on.
var _ Store = (*Repository)(nil)

// visibleAccountSQL is "an account a stranger may look at", against alias `u`.
// The same predicate the profile read uses, and for the same reasons: a
// deleted or banned account has no public anything, while a suspended one
// keeps its published work and therefore its page.
const visibleAccountSQL = `
	u.deleted_at IS NULL AND u.status NOT IN ('deleted', 'banned')`

// ---------------------------------------------------------------------------
// Preferences - the ai_prefs shape
// ---------------------------------------------------------------------------

// Prefs loads the account's switches; nil when never saved.
func (r *Repository) Prefs(ctx context.Context, userID uuid.UUID) (*Prefs, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT prefs FROM achievement_prefs WHERE user_id = $1`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load achievement prefs: %w", err)
	}
	var prefs Prefs
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return nil, fmt.Errorf("decode achievement prefs: %w", err)
	}
	return &prefs, nil
}

// SetPrefs upserts the account's switches.
func (r *Repository) SetPrefs(ctx context.Context, userID uuid.UUID, prefs Prefs) error {
	encoded, err := json.Marshal(prefs)
	if err != nil {
		return fmt.Errorf("encode achievement prefs: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO achievement_prefs (user_id, prefs) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET prefs = $2, updated_at = now()`,
		userID, encoded)
	if err != nil {
		return fmt.Errorf("save achievement prefs: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The tally
// ---------------------------------------------------------------------------

// progressMeta is the per-key working state stored in the meta column.
type progressMeta struct {
	// Actors is the distinct-account set for reader-driven keys. It is bounded
	// by the service, which stops recording a key the moment it unlocks.
	Actors []string `json:"actors,omitempty"`
}

// Progress reads one tally row. A key with no row yet is a zero tally, not an
// error - most keys never have a row until something happens.
func (r *Repository) Progress(ctx context.Context, userID uuid.UUID, key string) (Progress, error) {
	var (
		progress Progress
		raw      []byte
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT count, last_at, meta FROM achievement_progress
		WHERE user_id = $1 AND key = $2`, userID, key).
		Scan(&progress.Count, &progress.LastAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, nil
	}
	if err != nil {
		return Progress{}, fmt.Errorf("load achievement progress: %w", err)
	}
	var meta progressMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Progress{}, fmt.Errorf("decode achievement progress meta: %w", err)
	}
	progress.Actors = meta.Actors
	return progress, nil
}

// AllProgress reads every tally the account holds, keyed by achievement.
func (r *Repository) AllProgress(
	ctx context.Context, userID uuid.UUID,
) (map[string]Progress, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT key, count, last_at FROM achievement_progress WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("list achievement progress: %w", err)
	}
	defer rows.Close()

	// The actor set is deliberately NOT read here: it is anti-farming state,
	// not something any surface displays, and a profile read has no business
	// loading a list of who read somebody's work.
	out := map[string]Progress{}
	for rows.Next() {
		var (
			key      string
			progress Progress
		)
		if err := rows.Scan(&key, &progress.Count, &progress.LastAt); err != nil {
			return nil, fmt.Errorf("scan achievement progress: %w", err)
		}
		out[key] = progress
	}
	return out, rows.Err()
}

// Bump adds one to a tally and returns the resulting count.
//
// One statement, so two concurrent signals cannot both read 9 and both write
// 10. The actor - when there is one - is appended to the distinct set in the
// same statement, and `- to_jsonb(...)` removes any existing copy first so the
// set stays a set even if two signals for the same reader race.
func (r *Repository) Bump(
	ctx context.Context, userID uuid.UUID, key string, actor uuid.UUID, at time.Time,
) (int, error) {
	var count int
	if actor == uuid.Nil {
		err := r.db.QueryRowContext(ctx, `
			INSERT INTO achievement_progress (user_id, key, count, last_at)
			VALUES ($1, $2, 1, $3)
			ON CONFLICT (user_id, key) DO UPDATE
			SET count = achievement_progress.count + 1, last_at = $3
			RETURNING count`, userID, key, at).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("bump achievement progress: %w", err)
		}
		return count, nil
	}

	err := r.db.QueryRowContext(ctx, `
		INSERT INTO achievement_progress (user_id, key, count, last_at, meta)
		VALUES ($1, $2, 1, $3, jsonb_build_object('actors', jsonb_build_array($4::text)))
		ON CONFLICT (user_id, key) DO UPDATE
		SET count   = achievement_progress.count + 1,
		    last_at = $3,
		    meta    = jsonb_set(
		        achievement_progress.meta,
		        '{actors}',
		        (COALESCE(achievement_progress.meta->'actors', '[]'::jsonb) - $4::text)
		            || jsonb_build_array($4::text))
		RETURNING count`, userID, key, at, actor.String()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("bump achievement progress with actor: %w", err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// The award
// ---------------------------------------------------------------------------

// Unlock writes the award, reporting whether THIS call created it.
//
// DO NOTHING rather than DO UPDATE: an award already held keeps its original
// date. A second unlock must never move the day somebody earned something.
func (r *Repository) Unlock(
	ctx context.Context, userID uuid.UUID, key string, at time.Time,
) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO achievements (user_id, key, unlocked_at) VALUES ($1, $2, $3)
		ON CONFLICT (user_id, key) DO NOTHING`, userID, key, at)
	if err != nil {
		return false, fmt.Errorf("unlock achievement: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("unlock achievement: %w", err)
	}
	return affected == 1, nil
}

// Awards reads every award the account holds, keyed by achievement.
func (r *Repository) Awards(ctx context.Context, userID uuid.UUID) (map[string]Award, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT key, unlocked_at, seen_at, showcase_order
		FROM achievements WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("list achievements: %w", err)
	}
	defer rows.Close()

	out := map[string]Award{}
	for rows.Next() {
		var (
			award Award
			order sql.NullInt16
		)
		if err := rows.Scan(&award.Key, &award.UnlockedAt, &award.SeenAt, &order); err != nil {
			return nil, fmt.Errorf("scan achievement: %w", err)
		}
		if order.Valid {
			position := int(order.Int16)
			award.ShowcaseOrder = &position
		}
		out[award.Key] = award
	}
	return out, rows.Err()
}

// SetShowcase replaces the owner's selection in one transaction: clear
// everything, then number what they chose. Two statements rather than a diff,
// because the selection is small and an ORDER that half-applied would put two
// medals in one slot.
func (r *Repository) SetShowcase(ctx context.Context, userID uuid.UUID, keys []string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin showcase update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE achievements SET showcase_order = NULL
		WHERE user_id = $1 AND showcase_order IS NOT NULL`, userID); err != nil {
		return fmt.Errorf("clear showcase: %w", err)
	}
	for position, key := range keys {
		if _, err := tx.ExecContext(ctx, `
			UPDATE achievements SET showcase_order = $3
			WHERE user_id = $1 AND key = $2`, userID, key, position); err != nil {
			return fmt.Errorf("set showcase position: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit showcase update: %w", err)
	}
	return nil
}

// MarkSeen stamps every award the owner has now been shown. Only the unseen
// rows are touched, so the column keeps saying when they FIRST saw it.
func (r *Repository) MarkSeen(ctx context.Context, userID uuid.UUID, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE achievements SET seen_at = $2
		WHERE user_id = $1 AND seen_at IS NULL`, userID, at)
	if err != nil {
		return fmt.Errorf("mark achievements seen: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

// AccountOlderThan answers the reader-driven rule. A missing account answers
// false, which is the safe direction: it counts for nobody.
func (r *Repository) AccountOlderThan(
	ctx context.Context, userID uuid.UUID, age time.Duration,
) (bool, error) {
	var older bool
	err := r.db.QueryRowContext(ctx, `
		SELECT created_at < now() - make_interval(secs => $2)
		FROM users WHERE id = $1`, userID, age.Seconds()).Scan(&older)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check account age: %w", err)
	}
	return older, nil
}

// ResolveUser turns a public reference into an id.
func (r *Repository) ResolveUser(ctx context.Context, ref Ref) (uuid.UUID, error) {
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

	var id uuid.UUID
	err := r.db.QueryRowContext(ctx,
		`SELECT u.id FROM users u WHERE `+where+` AND `+visibleAccountSQL, arg).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve user: %w", err)
	}
	return id, nil
}
