package ai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// ErrNotFound covers "no such request/suggestion". The service translates it to
// the appropriate non-oracle 404 (docs/11 §31).
var ErrNotFound = errors.New("ai record not found")

// Repository is the only place that reads or writes ai_requests and
// ai_suggestions.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const requestColumns = `
	id, user_id, chapter_id, feature, provider, model, status, error_code,
	created_at, started_at, completed_at`

const suggestionColumns = `
	id, request_id, chapter_id, type, original_text, suggested_text,
	explanation, status, created_at`

// suggestionColumnsS is suggestionColumns aliased to `s`, for the ownership
// JOIN in FindSuggestionForUser where id/chapter_id would otherwise be
// ambiguous against ai_requests.
const suggestionColumnsS = `
	s.id, s.request_id, s.chapter_id, s.type, s.original_text, s.suggested_text,
	s.explanation, s.status, s.created_at`

type scanner interface{ Scan(...any) error }

func scanRequest(row scanner) (*Request, error) {
	var r Request
	err := row.Scan(
		&r.ID, &r.UserID, &r.ChapterID, &r.Feature, &r.Provider, &r.Model,
		&r.Status, &r.ErrorCode, &r.CreatedAt, &r.StartedAt, &r.CompletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan ai request: %w", err)
	}
	return &r, nil
}

func scanSuggestion(row scanner) (*Suggestion, error) {
	var s Suggestion
	err := row.Scan(
		&s.ID, &s.RequestID, &s.ChapterID, &s.Type, &s.OriginalText,
		&s.SuggestedText, &s.Explanation, &s.Status, &s.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan ai suggestion: %w", err)
	}
	return &s, nil
}

// NewSuggestion is a validated suggestion ready to persist.
type NewSuggestion struct {
	ChapterID     uuid.UUID
	Type          string
	OriginalText  string
	SuggestedText *string
	Explanation   *string
}

// InsertCompletedParams records a SYNCHRONOUS request that already ran, with its
// suggestions, in one transaction (docs/12 §27 sync path).
type InsertCompletedParams struct {
	UserID      uuid.UUID
	ChapterID   *uuid.UUID
	Feature     Feature
	Provider    string
	Model       *string
	Suggestions []NewSuggestion
}

// InsertCompleted writes a completed request and its suggestions atomically.
func (r *Repository) InsertCompleted(
	ctx context.Context, params InsertCompletedParams,
) (*Request, []Suggestion, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin ai insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	req, err := scanRequest(tx.QueryRowContext(ctx, `
		INSERT INTO ai_requests
			(user_id, chapter_id, feature, provider, model, status, started_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, 'completed', now(), now())
		RETURNING `+requestColumns,
		params.UserID, params.ChapterID, params.Feature, params.Provider, params.Model))
	if err != nil {
		return nil, nil, fmt.Errorf("insert ai request: %w", err)
	}

	suggestions, err := insertSuggestions(ctx, tx, req.ID, params.Suggestions)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit ai insert: %w", err)
	}
	return req, suggestions, nil
}

// InsertFailedParams records a synchronous request whose provider failed. Only
// the safe error CLASSIFICATION is stored, never provider detail (docs/12 §36).
type InsertFailedParams struct {
	UserID    uuid.UUID
	ChapterID *uuid.UUID
	Feature   Feature
	Provider  string
	ErrorCode string
}

// InsertFailed records a failed synchronous request.
func (r *Repository) InsertFailed(ctx context.Context, params InsertFailedParams) (*Request, error) {
	return scanRequest(r.db.QueryRowContext(ctx, `
		INSERT INTO ai_requests
			(user_id, chapter_id, feature, provider, status, error_code, started_at, completed_at)
		VALUES ($1, $2, $3, $4, 'failed', $5, now(), now())
		RETURNING `+requestColumns,
		params.UserID, params.ChapterID, params.Feature, params.Provider, params.ErrorCode))
}

// Enqueue records an ASYNCHRONOUS request in the queued state (docs/12 §27
// async path). The worker claims it later.
func (r *Repository) Enqueue(
	ctx context.Context, userID uuid.UUID, chapterID *uuid.UUID, feature Feature, provider string,
) (*Request, error) {
	return scanRequest(r.db.QueryRowContext(ctx, `
		INSERT INTO ai_requests (user_id, chapter_id, feature, provider, status)
		VALUES ($1, $2, $3, $4, 'queued')
		RETURNING `+requestColumns,
		userID, chapterID, feature, provider))
}

// Find loads one request by id, any status. The service decides authorization.
func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*Request, error) {
	return scanRequest(r.db.QueryRowContext(ctx,
		`SELECT `+requestColumns+` FROM ai_requests WHERE id = $1`, id))
}

// Suggestions loads a request's suggestions, oldest first.
func (r *Repository) Suggestions(ctx context.Context, requestID uuid.UUID) ([]Suggestion, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+suggestionColumns+` FROM ai_suggestions WHERE request_id = $1 ORDER BY created_at, id`,
		requestID)
	if err != nil {
		return nil, fmt.Errorf("list ai suggestions: %w", err)
	}
	defer rows.Close()

	var out []Suggestion
	for rows.Next() {
		s, err := scanSuggestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ListForUser returns one page of the caller's requests, newest first, WITHOUT
// suggestions (the history list is metadata only, docs/07 §21).
func (r *Repository) ListForUser(
	ctx context.Context, userID uuid.UUID, page pagination.Params,
) ([]Request, int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM ai_requests WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count ai requests: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+requestColumns+`
		 FROM ai_requests WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2 OFFSET $3`,
		userID, page.Limit(), page.Offset())
	if err != nil {
		return nil, 0, fmt.Errorf("list ai requests: %w", err)
	}
	defer rows.Close()

	var out []Request
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *req)
	}
	return out, total, rows.Err()
}

// FindSuggestionForUser loads a suggestion only if it belongs to a request owned
// by userID. A suggestion the caller does not own is ErrNotFound - never
// distinguishable from a missing one (docs/11 §31).
func (r *Repository) FindSuggestionForUser(
	ctx context.Context, id, userID uuid.UUID,
) (*Suggestion, error) {
	return scanSuggestion(r.db.QueryRowContext(ctx, `
		SELECT `+suggestionColumnsS+`
		FROM ai_suggestions s
		JOIN ai_requests req ON req.id = s.request_id
		WHERE s.id = $1 AND req.user_id = $2`, id, userID))
}

// SetSuggestionStatus moves a PENDING suggestion to a decision. It returns false
// if the row was not pending (already decided) - the service turns that into a
// conflict, distinct from the not-found the ownership check already ruled out.
func (r *Repository) SetSuggestionStatus(
	ctx context.Context, id uuid.UUID, status SuggestionStatus,
) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE ai_suggestions SET status = $2 WHERE id = $1 AND status = 'pending'`,
		id, status)
	if err != nil {
		return false, fmt.Errorf("decide ai suggestion: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("decide ai suggestion: %w", err)
	}
	return n == 1, nil
}

// ---------------------------------------------------------------------------
// Worker-side lifecycle (concurrency-safe)
// ---------------------------------------------------------------------------

// Claim atomically moves a request from queued to processing. The condition
// `status = 'queued'` is the whole concurrency guarantee: two workers racing
// for the same id, only one UPDATE matches, so a job is processed at most once
// (Phase 10 brief "Two workers must not accidentally process the same
// request"). A cancelled or already-claimed row matches nothing → claimed=false.
func (r *Repository) Claim(ctx context.Context, id uuid.UUID) (*Request, bool, error) {
	req, err := scanRequest(r.db.QueryRowContext(ctx, `
		UPDATE ai_requests SET status = 'processing', started_at = now()
		WHERE id = $1 AND status = 'queued'
		RETURNING `+requestColumns, id))
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return req, true, nil
}

// Complete finishes a processing request and writes its suggestions atomically.
// The `status = 'processing'` guard means a completion only lands on the row
// this worker actually claimed.
func (r *Repository) Complete(
	ctx context.Context, id uuid.UUID, model string, suggestions []NewSuggestion,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ai complete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE ai_requests SET status = 'completed', model = $2, completed_at = now()
		WHERE id = $1 AND status = 'processing'`, id, model)
	if err != nil {
		return fmt.Errorf("complete ai request: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// The row was cancelled or re-claimed underneath us; drop the result
		// rather than attach suggestions to a row that moved on.
		return nil
	}
	if _, err := insertSuggestions(ctx, tx, id, suggestions); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ai complete: %w", err)
	}
	return nil
}

// Fail marks a processing request failed with a safe classification.
func (r *Repository) Fail(ctx context.Context, id uuid.UUID, errorCode string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE ai_requests SET status = 'failed', error_code = $2, completed_at = now()
		WHERE id = $1 AND status = 'processing'`, id, errorCode)
	if err != nil {
		return fmt.Errorf("fail ai request: %w", err)
	}
	return nil
}

// Requeue resets a caller's FAILED request back to queued for a fresh attempt,
// clearing the previous outcome and any stale suggestions. Returns false if the
// row was not a failed request owned by the caller.
func (r *Repository) Requeue(ctx context.Context, id, userID uuid.UUID) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin ai requeue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE ai_requests
		SET status = 'queued', error_code = NULL, started_at = NULL, completed_at = NULL
		WHERE id = $1 AND user_id = $2 AND status = 'failed'`, id, userID)
	if err != nil {
		return false, fmt.Errorf("requeue ai request: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_suggestions WHERE request_id = $1`, id); err != nil {
		return false, fmt.Errorf("clear ai suggestions on requeue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit ai requeue: %w", err)
	}
	return true, nil
}

// Cancel moves a caller's still-queued request to cancelled. Returns false if it
// was already claimed or terminal - the worker's queued-guard then guarantees it
// is never processed after a successful cancel.
func (r *Repository) Cancel(ctx context.Context, id, userID uuid.UUID) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE ai_requests SET status = 'cancelled', completed_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'queued'`, id, userID)
	if err != nil {
		return false, fmt.Errorf("cancel ai request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cancel ai request: %w", err)
	}
	return n == 1, nil
}

// RecoverQueued resets any orphaned 'processing' rows (a crash left them
// mid-flight - nothing is legitimately processing at startup) back to queued,
// then returns every queued id so the worker can re-enqueue them. This heals the
// in-memory queue across a restart and recovers crash orphans (docs/12 §28
// lifecycle). It assumes a single worker/instance, the current deployment model;
// multi-instance recovery would need per-claim leases.
func (r *Repository) RecoverQueued(ctx context.Context) ([]uuid.UUID, error) {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE ai_requests SET status = 'queued', started_at = NULL WHERE status = 'processing'`); err != nil {
		return nil, fmt.Errorf("reset stale processing: %w", err)
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM ai_requests WHERE status = 'queued' ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list queued ai requests: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan queued id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// insertSuggestions writes suggestion rows for a request within a transaction.
func insertSuggestions(
	ctx context.Context, tx *sql.Tx, requestID uuid.UUID, suggestions []NewSuggestion,
) ([]Suggestion, error) {
	out := make([]Suggestion, 0, len(suggestions))
	for _, s := range suggestions {
		row, err := scanSuggestion(tx.QueryRowContext(ctx, `
			INSERT INTO ai_suggestions
				(request_id, chapter_id, type, original_text, suggested_text, explanation)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+suggestionColumns,
			requestID, s.ChapterID, s.Type, s.OriginalText, s.SuggestedText, s.Explanation))
		if err != nil {
			return nil, fmt.Errorf("insert ai suggestion: %w", err)
		}
		out = append(out, *row)
	}
	return out, nil
}
