package promo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is a missing or already-deleted slide.
var ErrNotFound = errors.New("promo slide not found")

// Repository is the only place that reads or writes promo_slides.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const columns = `
	id, position, kicker, headline, tagline, cta_label, link_url,
	image_url, bg_color, text_side, source, enabled, starts_at, ends_at,
	impressions, clicks, created_at, updated_at`

func scan(row interface{ Scan(...any) error }) (Slide, error) {
	var s Slide
	err := row.Scan(
		&s.ID, &s.Position, &s.Kicker, &s.Headline, &s.Tagline, &s.CTALabel,
		&s.LinkURL, &s.ImageURL, &s.BgColor, &s.TextSide, &s.Source,
		&s.Enabled, &s.StartsAt, &s.EndsAt,
		&s.Impressions, &s.Clicks, &s.CreatedAt, &s.UpdatedAt,
	)
	return s, err
}

func (r *Repository) list(ctx context.Context, query string, args ...any) ([]Slide, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list promo slides: %w", err)
	}
	defer rows.Close()

	slides := []Slide{}
	for rows.Next() {
		slide, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan promo slide: %w", err)
		}
		slides = append(slides, slide)
	}
	return slides, rows.Err()
}

// All returns the whole queue, position order - the admin page's read.
func (r *Repository) All(ctx context.Context) ([]Slide, error) {
	return r.list(ctx, `SELECT `+columns+` FROM promo_slides
		ORDER BY position ASC, created_at ASC`)
}

// Live returns the slides whose window covers now, position order. The deck
// rules (cap, paid ratio) are applied by the service, not here.
func (r *Repository) Live(ctx context.Context, now time.Time) ([]Slide, error) {
	return r.list(ctx, `SELECT `+columns+` FROM promo_slides
		WHERE enabled
		  AND (starts_at IS NULL OR starts_at <= $1)
		  AND (ends_at IS NULL OR ends_at > $1)
		ORDER BY position ASC, created_at ASC`, now)
}

// Create appends a slide at the end of the queue.
func (r *Repository) Create(ctx context.Context, s Slide) (Slide, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO promo_slides
			(position, kicker, headline, tagline, cta_label, link_url,
			 image_url, bg_color, text_side, source, enabled, starts_at, ends_at)
		VALUES (
			COALESCE((SELECT max(position) + 1 FROM promo_slides), 0),
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+columns,
		s.Kicker, s.Headline, s.Tagline, s.CTALabel, s.LinkURL,
		s.ImageURL, s.BgColor, s.TextSide, s.Source, s.Enabled, s.StartsAt, s.EndsAt)
	created, err := scan(row)
	if err != nil {
		return Slide{}, fmt.Errorf("create promo slide: %w", err)
	}
	return created, nil
}

// Update replaces a slide's editable fields.
func (r *Repository) Update(ctx context.Context, id uuid.UUID, s Slide) (Slide, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE promo_slides SET
			kicker = $2, headline = $3, tagline = $4, cta_label = $5,
			link_url = $6, image_url = $7, bg_color = $8, text_side = $9,
			source = $10, enabled = $11, starts_at = $12, ends_at = $13,
			updated_at = now()
		WHERE id = $1
		RETURNING `+columns,
		id, s.Kicker, s.Headline, s.Tagline, s.CTALabel, s.LinkURL,
		s.ImageURL, s.BgColor, s.TextSide, s.Source, s.Enabled, s.StartsAt, s.EndsAt)
	updated, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Slide{}, ErrNotFound
	}
	if err != nil {
		return Slide{}, fmt.Errorf("update promo slide: %w", err)
	}
	return updated, nil
}

// Delete removes a slide outright - a queue is not an archive.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM promo_slides WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete promo slide: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetOrder rewrites positions from the given id order, one transaction.
// Ids not in the list keep their rows but sort after the ordered ones.
func (r *Repository) SetOrder(ctx context.Context, ids []uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reorder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE promo_slides SET position = $2, updated_at = now() WHERE id = $1`,
			id, i); err != nil {
			return fmt.Errorf("reorder promo slide: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reorder: %w", err)
	}
	return nil
}

// CountImpressions adds one serving to each slide, in one statement. Advisory
// counters (docs/HOME-PROMO.md "Stats") - a failure is logged by the caller
// and never fails the read that triggered it.
func (r *Repository) CountImpressions(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE promo_slides SET impressions = impressions + 1
		WHERE id = ANY($1::uuid[])`, uuidArray(ids)); err != nil {
		return fmt.Errorf("count impressions: %w", err)
	}
	return nil
}

// uuidArray renders ids as a Postgres array literal - the same shape the
// chapters repository uses for its ANY() reads.
func uuidArray(ids []uuid.UUID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return "{" + strings.Join(out, ",") + "}"
}

// CountClick adds one click. Counting a slide that meanwhile disappeared is a
// no-op, not an error.
func (r *Repository) CountClick(ctx context.Context, id uuid.UUID) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE promo_slides SET clicks = clicks + 1 WHERE id = $1`, id); err != nil {
		return fmt.Errorf("count click: %w", err)
	}
	return nil
}
