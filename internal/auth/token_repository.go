package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var ErrTokenNotFound = errors.New("token not found")

// TokenPurpose selects which single-use token table to operate on.
//
// The two tables are kept separate rather than sharing one table with a
// `purpose` column so that a password-reset token can never be redeemed as an
// email-verification token, or vice versa, through a missing WHERE clause.
type TokenPurpose string

const (
	PurposePasswordReset     TokenPurpose = "password_reset"
	PurposeEmailVerification TokenPurpose = "email_verification"
)

func (p TokenPurpose) table() (string, error) {
	switch p {
	case PurposePasswordReset:
		return "password_reset_tokens", nil
	case PurposeEmailVerification:
		return "email_verification_tokens", nil
	}
	return "", fmt.Errorf("unknown token purpose %q", p)
}

// SingleUseToken is a stored, hashed, expiring token.
type SingleUseToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// Usable reports whether the token may still be redeemed.
func (t *SingleUseToken) Usable(now time.Time) bool {
	return t.UsedAt == nil && now.Before(t.ExpiresAt)
}

// TokenRepository manages password-reset and email-verification tokens.
type TokenRepository struct {
	db *sql.DB
}

func NewTokenRepository(db *sql.DB) *TokenRepository { return &TokenRepository{db: db} }

// Create issues a token, invalidating any earlier unused token for the same
// user and purpose.
//
// Superseding is deliberate (docs/10 §16): if a user requests three reset links
// in a row, only the newest should work, so an older link sitting in an inbox
// or a mail-server log cannot be redeemed later.
func (r *TokenRepository) Create(
	ctx context.Context,
	purpose TokenPurpose,
	userID uuid.UUID,
	tokenHash string,
	expiresAt time.Time,
) (*SingleUseToken, error) {
	table, err := purpose.table()
	if err != nil {
		return nil, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin token issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// #nosec G201 -- `table` comes from the closed TokenPurpose set above, never
	// from user input.
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET used_at = now() WHERE user_id = $1 AND used_at IS NULL`, table),
		userID); err != nil {
		return nil, fmt.Errorf("supersede tokens: %w", err)
	}

	// #nosec G201 -- see above.
	query := fmt.Sprintf(`
		INSERT INTO %s (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, expires_at, used_at, created_at`, table)

	var token SingleUseToken
	if err := tx.QueryRowContext(ctx, query, userID, tokenHash, expiresAt).Scan(
		&token.ID, &token.UserID, &token.ExpiresAt, &token.UsedAt, &token.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit token issue: %w", err)
	}
	return &token, nil
}

// Consume atomically redeems a token and returns it.
//
// The single-use guarantee lives in the WHERE clause: `used_at IS NULL` means
// two concurrent redemptions of the same link cannot both succeed, because the
// second UPDATE matches no row (docs/10 §16, docs/11 §26). Checking then
// updating in two statements would be a race.
func (r *TokenRepository) Consume(
	ctx context.Context,
	purpose TokenPurpose,
	tokenHash string,
) (*SingleUseToken, error) {
	table, err := purpose.table()
	if err != nil {
		return nil, err
	}

	// #nosec G201 -- `table` comes from the closed TokenPurpose set.
	query := fmt.Sprintf(`
		UPDATE %s
		SET used_at = now()
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING id, user_id, expires_at, used_at, created_at`, table)

	var token SingleUseToken
	err = r.db.QueryRowContext(ctx, query, tokenHash).Scan(
		&token.ID, &token.UserID, &token.ExpiresAt, &token.UsedAt, &token.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Expired, already used, or never existed - the caller must not be able
		// to tell which (docs/10 §16).
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("consume token: %w", err)
	}
	return &token, nil
}

// DeleteExpired removes spent and expired tokens (docs/08 §37, §39).
func (r *TokenRepository) DeleteExpired(ctx context.Context, purpose TokenPurpose, cutoff time.Time) (int64, error) {
	table, err := purpose.table()
	if err != nil {
		return 0, err
	}

	// #nosec G201 -- `table` comes from the closed TokenPurpose set.
	result, err := r.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE expires_at < $1`, table), cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete expired tokens: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired tokens: %w", err)
	}
	return affected, nil
}
