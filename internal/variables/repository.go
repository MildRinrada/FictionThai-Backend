package variables

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Repository is the only place that reads or writes novel_variables.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const columns = `
	id, novel_id, position, token, label, default_value, kind, options,
	created_at, updated_at`

// List returns a fiction's variables in declaration order.
func (r *Repository) List(ctx context.Context, novelID uuid.UUID) ([]Variable, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+columns+` FROM novel_variables WHERE novel_id = $1 ORDER BY position ASC`,
		novelID)
	if err != nil {
		return nil, fmt.Errorf("list variables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	list := []Variable{}
	for rows.Next() {
		var (
			v       Variable
			options []byte
		)
		if err := rows.Scan(&v.ID, &v.NovelID, &v.Position, &v.Token, &v.Label,
			&v.DefaultValue, &v.Kind, &options, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan variable: %w", err)
		}
		if len(options) > 0 {
			var decoded Options
			// An unreadable options blob degrades to "no options" rather than
			// failing the read: the declaration is still usable as plain text,
			// and refusing to serve it would take the writer's own page away.
			if err := json.Unmarshal(options, &decoded); err == nil && !decoded.IsEmpty() {
				v.Options = &decoded
			}
		}
		list = append(list, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list variables: %w", err)
	}
	return list, nil
}

// Replace makes the stored declarations equal to variables, in one transaction.
//
// Delete-then-insert rather than a per-row diff, for the same reason chat
// messages use it: positions are dense and reassigned from array order, so
// almost every row would move anyway. Nothing here touches chapter content -
// a variable's whole point is that the text keeps the token.
func (r *Repository) Replace(
	ctx context.Context, novelID uuid.UUID, list []Variable,
) ([]Variable, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin replace variables: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM novel_variables WHERE novel_id = $1`, novelID); err != nil {
		return nil, fmt.Errorf("clear variables: %w", err)
	}

	for i := range list {
		var options []byte
		if !list[i].Options.IsEmpty() {
			encoded, err := json.Marshal(list[i].Options)
			if err != nil {
				return nil, fmt.Errorf("encode variable options: %w", err)
			}
			options = encoded
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO novel_variables
				(novel_id, position, token, label, default_value, kind, options)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			novelID, list[i].Position, list[i].Token, list[i].Label,
			list[i].DefaultValue, list[i].Kind, options); err != nil {
			return nil, fmt.Errorf("insert variable: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit replace variables: %w", err)
	}
	return r.List(ctx, novelID)
}

// CastNames returns the fiction's declared character names, for telling a
// styled character mention apart from an undeclared reader variable.
func (r *Repository) CastNames(ctx context.Context, novelID uuid.UUID) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT name FROM characters WHERE novel_id = $1`, novelID)
	if err != nil {
		return nil, fmt.Errorf("list cast names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan cast name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// tokenPattern finds token-SHAPED strings in a fiction's text.
//
// ONE character, a slash, one character, in brackets - (y/n), (l/n), (e/c) -
// which is the whole of the genre's convention (settings review round 4). The
// pattern used to allow words up to 12/18 characters per side, and the first
// "(Scaramouche/Wanderer)" in someone's prose - two spellings of a character,
// a perfectly ordinary thing to write - became a nagging false positive. A
// key is a KEY: anything long enough to be a word is prose, and prose is none
// of this scanner's business.
const tokenPattern = `\([^()\s/]/[^()\s/]\)`

// Usage reports which declared tokens are never used, and which token-shaped
// strings in the text are never declared.
//
// Both are ADVISORY (see Usage). The scan runs in PostgreSQL rather than by
// loading every chapter into Go: a fiction's full text can be megabytes, and
// this runs on an ordinary save.
func (r *Repository) Usage(
	ctx context.Context, novelID uuid.UUID, declared []string,
) (Usage, error) {
	usage := emptyUsage()

	// Every place a writer can type: prose, chat messages, and headcanon entry
	// bodies. Missing one would report a token as unused while it is on screen.
	// Each source carries its chapter's identity, so an undeclared token can be
	// reported WITH the chapters it appears in (§13T follow-up) - a warning that
	// names a token but not where it is sends the writer hunting through drafts.
	const sourcesCTE = `
		WITH sources AS (
			SELECT c.chapter_number, c.title, c.slug, c.content AS text FROM chapters c
			WHERE c.novel_id = $1 AND c.deleted_at IS NULL AND c.content IS NOT NULL
			UNION ALL
			SELECT c.chapter_number, c.title, c.slug, m.content FROM chapter_messages m
			JOIN chapters c ON c.id = m.chapter_id
			WHERE c.novel_id = $1 AND c.deleted_at IS NULL
			UNION ALL
			SELECT c.chapter_number, c.title, c.slug, e.body FROM chapter_entries e
			JOIN chapters c ON c.id = e.chapter_id
			WHERE c.novel_id = $1 AND c.deleted_at IS NULL
		)`

	// DISTINCT collapses a token repeated within one chapter to one row; the
	// LIMIT bounds pathological texts (it caps token-chapter PAIRS, which is why
	// it is larger than the old 50-token cap).
	rows, err := r.db.QueryContext(ctx, sourcesCTE+`
		SELECT DISTINCT match[1], s.chapter_number, s.title, s.slug
		FROM sources s, regexp_matches(s.text, $2, 'g') AS match
		ORDER BY match[1], s.chapter_number
		LIMIT 200`, novelID, tokenPattern)
	if err != nil {
		return usage, fmt.Errorf("scan tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	known := make(map[string]struct{}, len(declared))
	for _, token := range declared {
		known[token] = struct{}{}
	}

	for rows.Next() {
		var (
			found   string
			chapter ChapterRef
		)
		if err := rows.Scan(&found, &chapter.Number, &chapter.Title, &chapter.Slug); err != nil {
			return usage, fmt.Errorf("scan token: %w", err)
		}
		if _, ok := known[found]; ok {
			continue
		}
		// Rows arrive grouped by token (ORDER BY), so appending to the last
		// entry is enough - no map, and the order is stable for the client.
		if n := len(usage.UndeclaredUses); n > 0 && usage.UndeclaredUses[n-1].Token == found {
			usage.UndeclaredUses[n-1].Chapters = append(
				usage.UndeclaredUses[n-1].Chapters, chapter)
			continue
		}
		usage.Undeclared = append(usage.Undeclared, found)
		usage.UndeclaredUses = append(usage.UndeclaredUses, TokenUse{
			Token: found, Chapters: []ChapterRef{chapter},
		})
	}
	if err := rows.Err(); err != nil {
		return usage, fmt.Errorf("scan tokens: %w", err)
	}

	if len(declared) == 0 {
		return usage, nil
	}

	// Declared-but-unused, matched LITERALLY with strpos rather than as a
	// pattern, so a token containing regex metacharacters is still found.
	placeholders := make([]string, 0, len(declared))
	args := []any{novelID}
	for _, token := range declared {
		args = append(args, token)
		placeholders = append(placeholders, fmt.Sprintf("($%d)", len(args)))
	}

	rows, err = r.db.QueryContext(ctx, sourcesCTE+`
		SELECT d.token
		FROM (VALUES `+strings.Join(placeholders, ", ")+`) AS d(token)
		WHERE NOT EXISTS (
			SELECT 1 FROM sources WHERE strpos(sources.text, d.token) > 0
		)`, args...)
	if err != nil {
		return usage, fmt.Errorf("check unused tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return usage, fmt.Errorf("scan unused token: %w", err)
		}
		usage.Unused = append(usage.Unused, token)
	}
	if err := rows.Err(); err != nil {
		return usage, fmt.Errorf("check unused tokens: %w", err)
	}
	return usage, nil
}
