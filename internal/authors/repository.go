package authors

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNotFound reports that a user has no author_profiles row yet.
var ErrNotFound = errors.New("author profile not found")

// Repository is the only place that reads or writes author_profiles.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const columns = `user_id, pen_name, author_bio, is_featured, donation_url, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanProfile(row scanner) (*Profile, error) {
	var p Profile
	err := row.Scan(&p.UserID, &p.PenName, &p.AuthorBio, &p.IsFeatured, &p.DonationURL, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan author profile: %w", err)
	}
	return &p, nil
}

// FindByUserID loads a user's author profile, or ErrNotFound if they have none.
func (r *Repository) FindByUserID(ctx context.Context, userID uuid.UUID) (*Profile, error) {
	return scanProfile(r.db.QueryRowContext(ctx,
		`SELECT `+columns+` FROM author_profiles WHERE user_id = $1`, userID))
}

// UpsertDonationURL sets (or clears) the caller's OWN donation URL, creating the
// author_profiles row on demand - the first author-profile write path in the
// codebase (addendum §2, §4). It only ever touches the row keyed by userID:
// there is no cross-user write. Nothing but donation_url is changed on an
// existing row.
func (r *Repository) UpsertDonationURL(
	ctx context.Context, userID uuid.UUID, donationURL *string,
) (*Profile, error) {
	return scanProfile(r.db.QueryRowContext(ctx, `
		INSERT INTO author_profiles (user_id, donation_url)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE
			SET donation_url = EXCLUDED.donation_url, updated_at = now()
		RETURNING `+columns, userID, donationURL))
}
