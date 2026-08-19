package views

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// Repository applies buffered view counts to the fiction rows.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// AddViews applies a whole batch in one statement.
//
// Written as an UPDATE ... FROM over two unnested arrays rather than a statement
// per fiction: a busy flush covers hundreds of fictions, and this keeps it to a
// single round trip. The ids arrive as text and are cast, because that is the
// array type the driver encodes a []string as.
func (r *Repository) AddViews(ctx context.Context, counts map[uuid.UUID]int64) error {
	if len(counts) == 0 {
		return nil
	}

	ids := make([]string, 0, len(counts))
	deltas := make([]int64, 0, len(counts))
	for id, delta := range counts {
		ids = append(ids, id.String())
		deltas = append(deltas, delta)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin add views: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE novels
		SET view_count = novels.view_count + batch.delta
		FROM (
			SELECT UNNEST($1::text[])::uuid AS id, UNNEST($2::bigint[]) AS delta
		) AS batch
		WHERE novels.id = batch.id`, ids, deltas); err != nil {
		return fmt.Errorf("add views: %w", err)
	}

	// The same batch, per day (13R). Today's row is the one every flush in the
	// next 24 hours lands on, so this is an upsert rather than an insert.
	//
	// The join back to `novels` is what keeps a delete honest: a fiction the
	// batch names but the table no longer has contributes nothing, instead of
	// failing the whole flush on a foreign key.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO novel_view_days (novel_id, day, view_count)
		SELECT n.id, (now() AT TIME ZONE 'utc')::date, batch.delta
		FROM (
			SELECT UNNEST($1::text[])::uuid AS id, UNNEST($2::bigint[]) AS delta
		) AS batch
		JOIN novels n ON n.id = batch.id
		ON CONFLICT (novel_id, day)
		DO UPDATE SET view_count = novel_view_days.view_count + EXCLUDED.view_count`,
		ids, deltas); err != nil {
		return fmt.Errorf("add daily views: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit add views: %w", err)
	}
	return nil
}

// ViewsSince totals the daily counters from a date forward.
//
// The date rather than an instant, because that is the resolution the table
// keeps: asking for "the last seven days" is asking for seven rows.
func (r *Repository) ViewsSince(ctx context.Context, novelID uuid.UUID, days int) (int64, error) {
	var total int64
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(view_count), 0)
		FROM novel_view_days
		WHERE novel_id = $1
		  AND day > (now() AT TIME ZONE 'utc')::date - $2::int`,
		novelID, days).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("sum views since: %w", err)
	}
	return total, nil
}

// ViewsByDay returns one entry per day - oldest first, today last, zeros for
// days without a row. Zeros are filled HERE rather than by the caller because
// the table only stores days that saw a read, and a sparkline handed a sparse
// series would draw a lie.
func (r *Repository) ViewsByDay(ctx context.Context, novelID uuid.UUID, days int) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT (now() AT TIME ZONE 'utc')::date - day, view_count
		FROM novel_view_days
		WHERE novel_id = $1
		  AND day > (now() AT TIME ZONE 'utc')::date - $2::int`,
		novelID, days)
	if err != nil {
		return nil, fmt.Errorf("daily views: %w", err)
	}
	defer func() { _ = rows.Close() }()

	series := make([]int64, days)
	for rows.Next() {
		var ago int
		var views int64
		if err := rows.Scan(&ago, &views); err != nil {
			return nil, fmt.Errorf("scan daily views: %w", err)
		}
		if index := days - 1 - ago; index >= 0 && index < days {
			series[index] = views
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("daily views: %w", err)
	}
	return series, nil
}
