package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var ErrSessionNotFound = errors.New("session not found")

// SessionRepository is the only place `user_sessions` is read or written.
type SessionRepository struct {
	db *sql.DB
}

func NewSessionRepository(db *sql.DB) *SessionRepository {
	return &SessionRepository{db: db}
}

const sessionColumns = `
	id, user_id, token_hash, client_kind, expires_at, last_used_at,
	revoked_at, created_at, user_agent, host(ip_prefix) || '/' || masklen(ip_prefix)`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var s Session
	var ipPrefix sql.NullString

	err := row.Scan(
		&s.ID, &s.UserID, &s.TokenHash, &s.ClientKind, &s.ExpiresAt, &s.LastUsedAt,
		&s.RevokedAt, &s.CreatedAt, &s.UserAgent, &ipPrefix,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}
	if ipPrefix.Valid {
		s.IPPrefix = &ipPrefix.String
	}
	return &s, nil
}

// CreateSessionParams describes a new session.
type CreateSessionParams struct {
	UserID     uuid.UUID
	TokenHash  string
	ClientKind ClientKind
	ExpiresAt  time.Time
	UserAgent  string
	IP         string
}

// Create inserts a session row.
func (r *SessionRepository) Create(ctx context.Context, params CreateSessionParams) (*Session, error) {
	query := `
		INSERT INTO user_sessions
			(user_id, token_hash, client_kind, expires_at, user_agent, ip_prefix)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + sessionColumns

	return scanSession(r.db.QueryRowContext(ctx, query,
		params.UserID,
		params.TokenHash,
		params.ClientKind,
		params.ExpiresAt,
		truncateUserAgent(params.UserAgent),
		TruncateIP(params.IP),
	))
}

// FindByTokenHash loads a session by its stored digest.
//
// The caller passes a digest, never a raw token - the raw value never reaches
// the repository layer at all.
func (r *SessionRepository) FindByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM user_sessions WHERE token_hash = $1`
	return scanSession(r.db.QueryRowContext(ctx, query, tokenHash))
}

// Touch records that a session was used and slides its idle window forward.
//
// Only written when the new expiry is meaningfully later than the stored one:
// updating on every single request would turn each authenticated read into a
// write and add pointless WAL traffic on the hot path.
func (r *SessionRepository) Touch(ctx context.Context, id uuid.UUID, usedAt, expiresAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE user_sessions
		SET last_used_at = $2, expires_at = $3
		WHERE id = $1 AND revoked_at IS NULL`, id, usedAt, expiresAt)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// Revoke ends one session. Already-revoked sessions are left untouched so the
// original revocation time is preserved for audit.
func (r *SessionRepository) Revoke(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE user_sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeAllForUser ends every live session belonging to a user.
//
// This complements users.sessions_invalidated_before rather than replacing it:
// the cutoff makes validation reject old sessions in O(1), while this marks the
// rows so a future device list shows them as ended rather than merely absent.
func (r *SessionRepository) RevokeAllForUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`UPDATE user_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	return affected, nil
}

// ActiveCountForUser reports how many usable sessions a user currently has.
func (r *SessionRepository) ActiveCountForUser(ctx context.Context, userID uuid.UUID) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM user_sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()`, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count sessions: %w", err)
	}
	return count, nil
}

// DeleteExpired hard-deletes sessions that ended before cutoff.
//
// docs/08 §37 hard-deletes expired sessions rather than soft-deleting them:
// there is no audit value in retaining a dead credential, and retaining it
// conflicts with the retention policy in docs/08 §39.
func (r *SessionRepository) DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM user_sessions WHERE expires_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return affected, nil
}
