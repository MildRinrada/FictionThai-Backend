package novels

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The writer's desk: the fictions their shell offers them, in the order they
// last touched them.
//
// A separate, deliberately narrow read rather than a call into List: the create
// menu wants three rows of (id, slug, title) and nothing else, on every page
// load, for a writer who may own hundreds of fictions. List assembles genres,
// tags, counts and a pen name for each row - correct for a listing page, waste
// for a menu.

// Desk is one of the writer's own fictions, as a menu row.
type Desk struct {
	ID    uuid.UUID `json:"-"`
	Slug  string    `json:"slug"`
	Title string    `json:"title"`
	// UpdatedAt is what the order is by, and what a client shows as "แก้ล่าสุด".
	UpdatedAt time.Time `json:"updated_at"`
}

// RecentForAuthor lists the fictions this writer touched most recently.
//
// Their own only - a collaborator's fiction is somebody else's work to add a
// chapter to, and it appearing under "เพิ่มตอนในเรื่องที่ค้างอยู่" would invite
// the wrong kind of accident.
func (r *Repository) RecentForAuthor(
	ctx context.Context, authorID uuid.UUID, limit int,
) ([]Desk, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, slug, title, updated_at
		FROM novels
		WHERE author_id = $1 AND deleted_at IS NULL
		ORDER BY updated_at DESC
		LIMIT $2`,
		authorID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list recent fictions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Desk
	for rows.Next() {
		var item Desk
		if err := rows.Scan(&item.ID, &item.Slug, &item.Title, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan recent fiction: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
