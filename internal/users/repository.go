package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. Callers translate these into API errors; the repository
// never constructs HTTP responses.
var (
	ErrNotFound        = errors.New("user not found")
	ErrUsernameTaken   = errors.New("username already taken")
	ErrEmailRegistered = errors.New("email already registered")
)

// Repository is the only place that reads or writes identity tables
// (backend/internal/README.md layering rules).
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// userColumns is shared by every SELECT so a new column cannot be added to one
// query and forgotten in another.
const userColumns = `
	id, username, email, password_hash, role, status,
	email_verified_at, adult_attested_at, sessions_invalidated_before,
	created_at, updated_at, deleted_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(
		&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Role, &u.Status,
		&u.EmailVerifiedAt, &u.AdultAttestedAt, &u.SessionsInvalidatedBefore,
		&u.CreatedAt, &u.UpdatedAt, &u.DeletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return &u, nil
}

// CreateParams carries everything needed to register an account.
type CreateParams struct {
	Username     string
	Email        string
	PasswordHash string
}

// Create inserts the user together with the profile and preference rows that
// every account must have.
//
// All four inserts share one transaction: an account without preferences would
// break the reader on first load, so a partial registration must not be
// possible (docs/09 §45).
//
// The author_profile row is deliberately NOT created - docs/08 §6.3 says a user
// does not become a writer immediately.
func (r *Repository) Create(ctx context.Context, params CreateParams) (*User, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	insert := `
		INSERT INTO users (username, email, password_hash, status)
		VALUES ($1, $2, $3, $4)
		RETURNING ` + userColumns

	user, err := scanUser(tx.QueryRowContext(ctx, insert,
		params.Username, params.Email, params.PasswordHash, StatusPendingVerification))
	if err != nil {
		// Translate the UNIQUE violations into typed errors. The database is
		// the authority here: a pre-flight SELECT would still race, so the
		// constraint is what actually prevents duplicates (docs/09 §34).
		if taken := uniqueViolation(err); taken != "" {
			return nil, taken2Error(taken)
		}
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_profiles (user_id) VALUES ($1)`, user.ID); err != nil {
		return nil, fmt.Errorf("create profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id) VALUES ($1)`, user.ID); err != nil {
		return nil, fmt.Errorf("create preferences: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit registration: %w", err)
	}
	return user, nil
}

// FindByID loads a user by primary key. Soft-deleted accounts are excluded.
func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*User, error) {
	query := `SELECT ` + userColumns + ` FROM users WHERE id = $1 AND deleted_at IS NULL`
	return scanUser(r.db.QueryRowContext(ctx, query, id))
}

// FindByIdentifier loads a user by username OR email.
//
// docs/10 §10 allows signing in with either. Both columns are CITEXT, so the
// comparison is case-insensitive in the database.
func (r *Repository) FindByIdentifier(ctx context.Context, identifier string) (*User, error) {
	query := `
		SELECT ` + userColumns + `
		FROM users
		WHERE (username = $1 OR email = $1) AND deleted_at IS NULL`
	return scanUser(r.db.QueryRowContext(ctx, query, strings.TrimSpace(identifier)))
}

// FindByEmail loads a user by email address.
func (r *Repository) FindByEmail(ctx context.Context, email string) (*User, error) {
	query := `SELECT ` + userColumns + ` FROM users WHERE email = $1 AND deleted_at IS NULL`
	return scanUser(r.db.QueryRowContext(ctx, query, NormalizeEmail(email)))
}

// FindByUsername loads a user by username.
func (r *Repository) FindByUsername(ctx context.Context, username string) (*User, error) {
	query := `SELECT ` + userColumns + ` FROM users WHERE username = $1 AND deleted_at IS NULL`
	return scanUser(r.db.QueryRowContext(ctx, query, NormalizeUsername(username)))
}

// Profile loads the public profile. A missing row yields ErrNotFound.
func (r *Repository) Profile(ctx context.Context, userID uuid.UUID) (*Profile, error) {
	query := `
		SELECT user_id, display_name, bio, avatar_url, website_url, created_at, updated_at
		FROM user_profiles WHERE user_id = $1`

	var p Profile
	err := r.db.QueryRowContext(ctx, query, userID).Scan(
		&p.UserID, &p.DisplayName, &p.Bio, &p.AvatarURL, &p.WebsiteURL,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load profile: %w", err)
	}
	return &p, nil
}

// HasAuthorProfile reports whether the user has taken on writer capability.
func (r *Repository) HasAuthorProfile(ctx context.Context, userID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM author_profiles WHERE user_id = $1)`, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check author profile: %w", err)
	}
	return exists, nil
}

// UpdatePassword sets a new hash and, in the same transaction, moves the
// account's session invalidation cutoff forward.
//
// These two writes must be atomic: a password change that did not invalidate
// existing sessions would leave an attacker signed in on another device, which
// is precisely the scenario docs/10 §37 requires handling.
func (r *Repository) UpdatePassword(ctx context.Context, userID uuid.UUID, passwordHash string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET password_hash = $2,
		    sessions_invalidated_before = now(),
		    updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, userID, passwordHash)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	return expectOneRow(result, "update password")
}

// InvalidateSessionsBefore moves the bulk-invalidation cutoff to now.
//
// This is how "log out of all devices" stays O(1) - one UPDATE on one row,
// rather than loading and revoking every session (docs/10 §37).
func (r *Repository) InvalidateSessionsBefore(ctx context.Context, userID uuid.UUID, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE users SET sessions_invalidated_before = $2, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, userID, at)
	if err != nil {
		return fmt.Errorf("invalidate sessions: %w", err)
	}
	return expectOneRow(result, "invalidate sessions")
}

// SetAvatarURL points the profile at a newly uploaded avatar (docs/08 §6.2,
// Phase 9). The media domain calls this for the uploader's OWN row only; nil
// clears the avatar.
func (r *Repository) SetAvatarURL(ctx context.Context, userID uuid.UUID, avatarURL *string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE user_profiles SET avatar_url = $2, updated_at = now()
		WHERE user_id = $1`, userID, avatarURL)
	if err != nil {
		return fmt.Errorf("set avatar url: %w", err)
	}
	return expectOneRow(result, "set avatar url")
}

// SetBannerURL points the profile at a newly uploaded cover image
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 1). Same contract as SetAvatarURL:
// the uploader's OWN row only; nil clears the banner and restores the
// typographic placeholder.
func (r *Repository) SetBannerURL(ctx context.Context, userID uuid.UUID, bannerURL *string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE user_profiles SET banner_url = $2, updated_at = now()
		WHERE user_id = $1`, userID, bannerURL)
	if err != nil {
		return fmt.Errorf("set banner url: %w", err)
	}
	return expectOneRow(result, "set banner url")
}

// UpdateStatus moves the account lifecycle state (docs/10 §18) - the
// moderation path for suspend, ban, and restore (docs/08 §24.2). The
// per-request status check in session validation makes the change take
// effect on the target's very next request.
func (r *Repository) UpdateStatus(ctx context.Context, userID uuid.UUID, status Status) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE users SET status = $2, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, userID, status)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return expectOneRow(result, "update status")
}

// MarkEmailVerified records that the address has been confirmed and promotes a
// pending account to active.
func (r *Repository) MarkEmailVerified(ctx context.Context, userID uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET email_verified_at = COALESCE(email_verified_at, now()),
		    status = CASE WHEN status = 'pending_verification' THEN 'active' ELSE status END,
		    updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, userID)
	if err != nil {
		return fmt.Errorf("mark email verified: %w", err)
	}
	return expectOneRow(result, "mark email verified")
}

// MarkAdultAttested records the account's one-time statement that it belongs to
// an adult (§13B).
//
// COALESCE keeps the FIRST time it was made: re-attesting is a no-op, not a
// fresh date, because the fact being recorded is "this account said so", and it
// said so once.
func (r *Repository) MarkAdultAttested(ctx context.Context, userID uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE users
		SET adult_attested_at = COALESCE(adult_attested_at, now()),
		    updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, userID)
	if err != nil {
		return fmt.Errorf("mark adult attested: %w", err)
	}
	return expectOneRow(result, "mark adult attested")
}

func expectOneRow(result sql.Result, op string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// uniqueViolation returns the constraint name when err is a PostgreSQL unique
// violation, or "" otherwise.
//
// The driver error is matched by string rather than by importing pgconn for its
// SQLSTATE type: the constraint names are ours and stable, and this keeps the
// repository free of driver-specific types.
func uniqueViolation(err error) string {
	msg := err.Error()
	if !strings.Contains(msg, "SQLSTATE 23505") && !strings.Contains(msg, "duplicate key value") {
		return ""
	}
	switch {
	case strings.Contains(msg, "users_username_key"):
		return "username"
	case strings.Contains(msg, "users_email_key"):
		return "email"
	}
	return "unknown"
}

func taken2Error(field string) error {
	switch field {
	case "username":
		return ErrUsernameTaken
	case "email":
		return ErrEmailRegistered
	default:
		return ErrEmailRegistered
	}
}
