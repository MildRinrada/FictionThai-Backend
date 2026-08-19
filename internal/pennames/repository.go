package pennames

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Sentinel errors. The repository never builds HTTP responses; the service
// translates these.
var (
	// ErrNotFound reports that no pen name of THIS user matched.
	ErrNotFound = errors.New("pen name not found")
	// ErrNameTaken reports the per-account uniqueness index refusing a name.
	ErrNameTaken = errors.New("pen name already used by this account")
)

// Repository is the only place that reads or writes pen_names and
// pen_name_history.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const columns = `id, user_id, name, note, is_default, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanPenName(row scanner) (*PenName, error) {
	var p PenName
	err := row.Scan(&p.ID, &p.UserID, &p.Name, &p.Note, &p.IsDefault,
		&p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan pen name: %w", err)
	}
	return &p, nil
}

// isUniqueViolation reports the per-account name index refusing a write.
//
// The index is the authority rather than a pre-flight SELECT, which would still
// race two tabs against each other (the novels slug precedent, docs/09 §34).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// The index NAME is matched rather than a typed driver code, so this package
	// does not take a dependency on pq/pgx internals for one branch - and so a
	// violation of some OTHER constraint can never be reported to the writer as
	// "you already use this name".
	return strings.Contains(err.Error(), "pen_names_user_name_key")
}

// ListForUser returns the account's identities, the default first and then in
// the order they were created - the order the writer built the list in.
func (r *Repository) ListForUser(ctx context.Context, userID uuid.UUID) ([]PenName, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+columns+` FROM pen_names WHERE user_id = $1
		 ORDER BY is_default DESC, created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list pen names: %w", err)
	}
	defer rows.Close()

	list := []PenName{}
	for rows.Next() {
		penName, err := scanPenName(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *penName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pen names: %w", err)
	}
	return list, nil
}

// CountForUser reports how many identities an account holds, for the cap.
func (r *Repository) CountForUser(ctx context.Context, userID uuid.UUID) (int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pen_names WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count pen names: %w", err)
	}
	return total, nil
}

// Find loads one identity. The user id is part of the predicate, so an id
// belonging to another account can never be reached through this path - it
// simply does not match, which is what makes the endpoint's 404 honest.
func (r *Repository) Find(ctx context.Context, userID, id uuid.UUID) (*PenName, error) {
	return scanPenName(r.db.QueryRowContext(ctx,
		`SELECT `+columns+` FROM pen_names WHERE id = $1 AND user_id = $2`, id, userID))
}

// Create adds one identity.
//
// The first pen name an account creates becomes its default: a writer with one
// identity and no default would have asked for a name and been given none. Any
// other default is cleared in the same transaction, so the partial unique index
// can never see two.
func (r *Repository) Create(
	ctx context.Context, userID uuid.UUID, name string, note *string, isDefault bool,
) (*PenName, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create pen name: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if isDefault {
		if err := clearDefault(ctx, tx, userID, uuid.Nil); err != nil {
			return nil, err
		}
	}

	penName, err := scanPenName(tx.QueryRowContext(ctx, `
		INSERT INTO pen_names (user_id, name, note, is_default)
		VALUES ($1, $2, $3,
			$4 OR NOT EXISTS (SELECT 1 FROM pen_names x WHERE x.user_id = $1))
		RETURNING `+columns,
		userID, name, note, isDefault))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create pen name: %w", err)
	}
	return penName, nil
}

// execer is the slice of *sql.Tx the default helpers need.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// clearDefault takes the flag off every identity of this account except one.
func clearDefault(ctx context.Context, tx execer, userID, keep uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE pen_names SET is_default = FALSE, updated_at = now()
		WHERE user_id = $1 AND is_default AND id <> $2`, userID, keep); err != nil {
		return fmt.Errorf("clear default pen name: %w", err)
	}
	return nil
}

// Update writes the fields the caller actually supplied, and records a rename.
//
// The whole operation is ONE transaction: the history row that makes a rename
// visible must not be able to exist without the rename, nor the rename without
// it. A rename records the PREVIOUS name - the new one is already on the row,
// so the old one is the entire point of the record.
func (r *Repository) Update(
	ctx context.Context, userID, id uuid.UUID, input UpdateInput,
) (*PenName, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update pen name: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read the row first, inside the transaction, so the previous name recorded
	// in the history is the one this update actually replaced.
	current, err := scanPenName(tx.QueryRowContext(ctx,
		`SELECT `+columns+` FROM pen_names WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		id, userID))
	if err != nil {
		return nil, err
	}

	// The default is resolved before the write, because clearing the previous
	// holder and setting the new one has to be indivisible - the partial unique
	// index would otherwise reject a perfectly ordinary "make this one default".
	if input.IsDefault != nil && *input.IsDefault {
		if err := clearDefault(ctx, tx, userID, id); err != nil {
			return nil, err
		}
	}

	updated, err := scanPenName(tx.QueryRowContext(ctx, `
		UPDATE pen_names SET
			name       = COALESCE($3, name),
			note       = CASE WHEN $4 THEN $5 ELSE note END,
			is_default = COALESCE($6::boolean, is_default),
			updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING `+columns,
		id, userID,
		input.Name,
		input.Note != nil, derefNote(input.Note),
		input.IsDefault,
	))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}

	if updated.Name != current.Name {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pen_name_history (user_id, previous_name) VALUES ($1, $2)`,
			userID, current.Name); err != nil {
			return nil, fmt.Errorf("record pen name history: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update pen name: %w", err)
	}
	return updated, nil
}

func derefNote(value **string) *string {
	if value == nil {
		return nil
	}
	return *value
}

// Delete removes ONE identity of this account.
//
// It touches no fiction. `novels.pen_name_id` is ON DELETE SET NULL, so every
// work published under this name keeps its text, its chapters, and its history,
// and falls back to the writer's default name (CLAUDE.md - never silently
// modify or delete writer content).
func (r *Repository) Delete(ctx context.Context, userID, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM pen_names WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete pen name: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete pen name: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Recent returns the names this account used recently and no longer uses,
// newest first - the «เคยใช้ชื่อ …» line.
//
// A name the writer has since taken back is filtered out: renaming A → B → A
// must not tell readers the writer "used to be" the name currently on their
// work. Anything older than the window is simply not selected; the row stays,
// because pruning history is a scheduled job's business, not a read's.
func (r *Repository) Recent(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT ON (lower(h.previous_name)) h.previous_name
		FROM pen_name_history h
		WHERE h.user_id = $1
		  AND h.changed_at > now() - $2::interval
		  AND NOT EXISTS (
			SELECT 1 FROM pen_names p
			WHERE p.user_id = h.user_id AND lower(p.name) = lower(h.previous_name)
		  )
		ORDER BY lower(h.previous_name), h.changed_at DESC`,
		userID, HistoryInterval())
	if err != nil {
		return nil, fmt.Errorf("list former pen names: %w", err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan former pen name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list former pen names: %w", err)
	}
	return names, nil
}
