package characters

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNotFound reports that no character matched.
var ErrNotFound = errors.New("character not found")

// Repository is the only place that reads or writes characters and their
// appearances.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const columns = `id, novel_id, name, role, summary, avatar_url, description, quote,
	traits, details, chat_color, chat_side, chat_display_name,
	position, first_chapter_id, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanCharacter(row scanner) (*Character, error) {
	var (
		c       Character
		traits  []byte
		details []byte
	)
	err := row.Scan(&c.ID, &c.NovelID, &c.Name, &c.Role, &c.Summary, &c.AvatarURL,
		&c.Description, &c.Quote, &traits, &details,
		&c.ChatColor, &c.ChatSide, &c.ChatDisplayName,
		&c.Position, &c.FirstChapterID, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan character: %w", err)
	}

	if err := json.Unmarshal(traits, &c.Traits); err != nil {
		return nil, fmt.Errorf("decode character traits: %w", err)
	}
	if err := json.Unmarshal(details, &c.Details); err != nil {
		return nil, fmt.Errorf("decode character details: %w", err)
	}
	return &c, nil
}

// encode turns the two JSONB payloads into bytes, normalising nil to an empty
// array so the column never holds SQL NULL or the JSON literal `null`.
func encode(traits []string, details []Detail) ([]byte, []byte, error) {
	if traits == nil {
		traits = []string{}
	}
	if details == nil {
		details = []Detail{}
	}
	encodedTraits, err := json.Marshal(traits)
	if err != nil {
		return nil, nil, fmt.Errorf("encode character traits: %w", err)
	}
	encodedDetails, err := json.Marshal(details)
	if err != nil {
		return nil, nil, fmt.Errorf("encode character details: %w", err)
	}
	return encodedTraits, encodedDetails, nil
}

// ListForNovel returns a fiction's cast in the author's order.
func (r *Repository) ListForNovel(ctx context.Context, novelID uuid.UUID) ([]Character, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+columns+` FROM characters WHERE novel_id = $1 ORDER BY position, created_at`,
		novelID)
	if err != nil {
		return nil, fmt.Errorf("list characters: %w", err)
	}
	defer rows.Close()

	list := []Character{}
	for rows.Next() {
		character, err := scanCharacter(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *character)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list characters: %w", err)
	}
	return list, nil
}

// NameTaken reports whether another character of the SAME fiction already uses
// this name (case-insensitively). The exclude id lets a rename check skip the
// character being renamed; uuid.Nil excludes nothing.
func (r *Repository) NameTaken(
	ctx context.Context, novelID uuid.UUID, name string, exclude uuid.UUID,
) (bool, error) {
	var taken bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM characters
			WHERE novel_id = $1 AND lower(name) = lower($2) AND id <> $3
		)`, novelID, name, exclude).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("check character name: %w", err)
	}
	return taken, nil
}

// Find loads one character. The novel id is part of the predicate so a character
// id from another fiction can never be reached through this fiction's path.
func (r *Repository) Find(
	ctx context.Context, novelID, characterID uuid.UUID,
) (*Character, error) {
	return scanCharacter(r.db.QueryRowContext(ctx,
		`SELECT `+columns+` FROM characters WHERE id = $1 AND novel_id = $2`,
		characterID, novelID))
}

// Create appends a character to the end of the fiction's cast.
//
// The position is computed inside the INSERT so two concurrent creates cannot
// read the same "next" value and collide on the uniqueness constraint.
func (r *Repository) Create(ctx context.Context, input *Character) (*Character, error) {
	traits, details, err := encode(input.Traits, input.Details)
	if err != nil {
		return nil, err
	}

	return scanCharacter(r.db.QueryRowContext(ctx, `
		INSERT INTO characters
			(novel_id, name, role, summary, avatar_url, description, quote,
			 traits, details, position, first_chapter_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
			COALESCE((SELECT MAX(position) + 1 FROM characters WHERE novel_id = $1), 0),
			$10)
		RETURNING `+columns,
		input.NovelID, input.Name, input.Role, input.Summary, input.AvatarURL,
		input.Description, input.Quote, traits, details, input.FirstChapterID))
}

// Update writes the fields the caller actually supplied.
//
// Every value is passed as a pair - the new value and a boolean saying whether to
// apply it - so an absent field keeps its stored value and an explicit null
// clears it, without the repository needing to know which case the HTTP layer
// saw (docs/09 §3).
func (r *Repository) Update(
	ctx context.Context, novelID, characterID uuid.UUID, input UpdateInput,
) (*Character, error) {
	traits, details, err := encode(derefSlice(input.Traits), derefDetails(input.Details))
	if err != nil {
		return nil, err
	}

	return scanCharacter(r.db.QueryRowContext(ctx, `
		UPDATE characters SET
			name             = COALESCE($3, name),
			role             = CASE WHEN $4  THEN $5  ELSE role             END,
			summary          = CASE WHEN $6  THEN $7  ELSE summary          END,
			avatar_url       = CASE WHEN $8  THEN $9  ELSE avatar_url       END,
			description      = CASE WHEN $10 THEN $11 ELSE description      END,
			quote            = CASE WHEN $12 THEN $13 ELSE quote            END,
			traits           = CASE WHEN $14 THEN $15 ELSE traits           END,
			details          = CASE WHEN $16 THEN $17 ELSE details          END,
			first_chapter_id = CASE WHEN $18 THEN $19 ELSE first_chapter_id END,
			chat_color        = CASE WHEN $20 THEN $21 ELSE chat_color        END,
			chat_side         = CASE WHEN $22 THEN $23 ELSE chat_side         END,
			chat_display_name = CASE WHEN $24 THEN $25 ELSE chat_display_name END,
			updated_at       = now()
		WHERE id = $1 AND novel_id = $2
		RETURNING `+columns,
		characterID, novelID,
		input.Name,
		input.Role != nil, deref(input.Role),
		input.Summary != nil, deref(input.Summary),
		input.AvatarURL != nil, deref(input.AvatarURL),
		input.Description != nil, deref(input.Description),
		input.Quote != nil, deref(input.Quote),
		input.Traits != nil, traits,
		input.Details != nil, details,
		input.FirstChapterID != nil, derefUUID(input.FirstChapterID),
		input.ChatColor != nil, deref(input.ChatColor),
		input.ChatSide != nil, deref(input.ChatSide),
		input.ChatDisplayName != nil, deref(input.ChatDisplayName),
	))
}

func deref(value **string) *string {
	if value == nil {
		return nil
	}
	return *value
}

func derefUUID(value **uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	return *value
}

func derefSlice(value *[]string) []string {
	if value == nil {
		return nil
	}
	return *value
}

func derefDetails(value *[]Detail) []Detail {
	if value == nil {
		return nil
	}
	return *value
}

// Delete removes one character. Its appearances go with it (ON DELETE CASCADE);
// the chapters it appeared in are untouched.
func (r *Repository) Delete(ctx context.Context, novelID, characterID uuid.UUID) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM characters WHERE id = $1 AND novel_id = $2`, characterID, novelID)
	if err != nil {
		return fmt.Errorf("delete character: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete character: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Reorder rewrites the whole cast order in one transaction.
//
// The uniqueness constraint is DEFERRABLE, so the new sequence can be written
// directly without shuffling every row through a temporary gap first. Only ids
// belonging to this fiction are touched, so a foreign id in the list changes
// nothing rather than moving someone else's character.
func (r *Repository) Reorder(ctx context.Context, novelID uuid.UUID, ids []uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reorder characters: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for position, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE characters SET position = $3, updated_at = now()
			 WHERE id = $1 AND novel_id = $2`,
			id, novelID, position); err != nil {
			return fmt.Errorf("reorder characters: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reorder characters: %w", err)
	}
	return nil
}

// CountForNovel reports how many characters a fiction has, for the reorder
// completeness check.
func (r *Repository) CountForNovel(ctx context.Context, novelID uuid.UUID) (int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM characters WHERE novel_id = $1`, novelID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count characters: %w", err)
	}
	return total, nil
}

// AppearancesForNovel returns every cast member's appearances in ONE query,
// keyed by character, each list in chapter order - so the list endpoint can
// carry appearances without a query per card.
func (r *Repository) AppearancesForNovel(
	ctx context.Context, novelID uuid.UUID,
) (map[uuid.UUID][]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.character_id, a.chapter_id
		FROM character_appearances a
		JOIN characters ch ON ch.id = a.character_id AND ch.novel_id = $1
		JOIN chapters c ON c.id = a.chapter_id AND c.deleted_at IS NULL
		ORDER BY c.chapter_number`, novelID)
	if err != nil {
		return nil, fmt.Errorf("list novel appearances: %w", err)
	}
	defer rows.Close()

	byCharacter := map[uuid.UUID][]uuid.UUID{}
	for rows.Next() {
		var characterID, chapterID uuid.UUID
		if err := rows.Scan(&characterID, &chapterID); err != nil {
			return nil, fmt.Errorf("scan novel appearance: %w", err)
		}
		byCharacter[characterID] = append(byCharacter[characterID], chapterID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list novel appearances: %w", err)
	}
	return byCharacter, nil
}

// AppearancesFor returns the chapters a character appears in, in chapter order.
//
// The join filters to live-in-this-fiction chapters; a chapter the caller may not
// read is filtered by the service, which knows the caller.
func (r *Repository) AppearancesFor(
	ctx context.Context, characterID uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.chapter_id
		FROM character_appearances a
		JOIN chapters c ON c.id = a.chapter_id AND c.deleted_at IS NULL
		WHERE a.character_id = $1
		ORDER BY c.chapter_number`, characterID)
	if err != nil {
		return nil, fmt.Errorf("list appearances: %w", err)
	}
	defer rows.Close()

	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan appearance: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list appearances: %w", err)
	}
	return ids, nil
}

// SetAppearances replaces the chapters a character appears in.
//
// Only chapters of the SAME fiction are accepted - the INSERT filters through a
// subquery rather than trusting the ids, so a request naming a chapter of
// another writer's work silently records nothing instead of linking across
// fictions.
func (r *Repository) SetAppearances(
	ctx context.Context, novelID, characterID uuid.UUID, chapterIDs []uuid.UUID,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set appearances: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM character_appearances WHERE character_id = $1`, characterID); err != nil {
		return fmt.Errorf("set appearances: %w", err)
	}

	for _, chapterID := range chapterIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO character_appearances (character_id, chapter_id)
			SELECT $1, c.id FROM chapters c
			WHERE c.id = $2 AND c.novel_id = $3 AND c.deleted_at IS NULL
			ON CONFLICT DO NOTHING`,
			characterID, chapterID, novelID); err != nil {
			return fmt.Errorf("set appearances: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set appearances: %w", err)
	}
	return nil
}

// ChapterBelongsTo reports whether a chapter is part of this fiction, so
// first_chapter_id can never point outside it.
func (r *Repository) ChapterBelongsTo(
	ctx context.Context, novelID, chapterID uuid.UUID,
) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM chapters
			WHERE id = $1 AND novel_id = $2 AND deleted_at IS NULL
		)`, chapterID, novelID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check chapter ownership: %w", err)
	}
	return exists, nil
}
