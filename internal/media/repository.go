package media

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNotFound covers "no such media". The service translates to HTTP.
var ErrNotFound = errors.New("media not found")

// Repository is the only place that reads or writes the media table.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const mediaColumns = `
	id, owner_id, object_key, original_filename, mime_type, size_bytes,
	media_type, created_at, deleted_at`

type scanner interface{ Scan(...any) error }

func scanMedia(row scanner) (*Media, error) {
	var m Media
	err := row.Scan(
		&m.ID, &m.OwnerID, &m.ObjectKey, &m.OriginalFilename, &m.MimeType,
		&m.SizeBytes, &m.MediaType, &m.CreatedAt, &m.DeletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan media: %w", err)
	}
	return &m, nil
}

// InsertParams is a validated metadata insert - recorded AFTER the object is
// safely stored (docs/07 §23), which is why every field is already known.
type InsertParams struct {
	OwnerID          uuid.UUID
	ObjectKey        string
	OriginalFilename *string
	MimeType         string
	SizeBytes        int64
	MediaType        Type
}

// Insert records one stored object's metadata.
func (r *Repository) Insert(ctx context.Context, params InsertParams) (*Media, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO media (owner_id, object_key, original_filename, mime_type, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+mediaColumns,
		params.OwnerID, params.ObjectKey, params.OriginalFilename,
		params.MimeType, params.SizeBytes, params.MediaType)

	m, err := scanMedia(row)
	if err != nil {
		return nil, fmt.Errorf("insert media: %w", err)
	}
	return m, nil
}

// Find loads one media row by id, whatever its state - the SERVICE decides
// what a deleted row means for each operation.
func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*Media, error) {
	return scanMedia(r.db.QueryRowContext(ctx,
		`SELECT `+mediaColumns+` FROM media WHERE id = $1`, id))
}

// FindLiveByKey resolves a serve-path key to its LIVE row. The public file
// route goes through this row, never straight to storage - which is what
// makes deleted_at authoritative even if a storage delete failed behind it.
func (r *Repository) FindLiveByKey(ctx context.Context, key string) (*Media, error) {
	return scanMedia(r.db.QueryRowContext(ctx,
		`SELECT `+mediaColumns+` FROM media WHERE object_key = $1 AND deleted_at IS NULL`, key))
}

// SoftDelete marks a row deleted. Zero rows affected means it was already
// deleted (or never existed) - the caller decides what that means.
func (r *Repository) SoftDelete(ctx context.Context, id uuid.UUID) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE media SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("delete media: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete media: %w", err)
	}
	return affected == 1, nil
}

// Restore clears the soft delete - the moderation counterpart of SoftDelete
// (docs/11 §39 "Moderator restored content").
func (r *Repository) Restore(ctx context.Context, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE media SET deleted_at = NULL WHERE id = $1 AND deleted_at IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("restore media: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("restore media: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
