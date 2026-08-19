package taxonomy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ErrNotFound covers "no such row" lookups.
var ErrNotFound = errors.New("taxonomy term not found")

// Repository is the only place that reads or writes genres, tags, and their
// assignment tables.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// ListGenres returns the whole controlled vocabulary, alphabetically.
//
// No pagination: this is a curated list of a handful of rows (docs/08 §14.1),
// and clients need all of it to render a picker.
func (r *Repository) ListGenres(ctx context.Context) ([]Genre, error) {
	// Ordered by KIND first (13S), so a picker rendering the list top to bottom
	// gets its three questions in the order the form asks them without having
	// to know the vocabulary.
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, slug, kind, description, created_at
		FROM genres
		ORDER BY CASE kind
			WHEN 'content' THEN 0
			WHEN 'relationship' THEN 1
			ELSE 2
		END, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list genres: %w", err)
	}
	defer func() { _ = rows.Close() }()

	genres := []Genre{}
	for rows.Next() {
		var g Genre
		if err := rows.Scan(&g.ID, &g.Name, &g.Slug, &g.Kind, &g.Description, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan genre: %w", err)
		}
		genres = append(genres, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list genres: %w", err)
	}
	return genres, nil
}

// GenreIDs returns which of the given ids exist. The novels service uses the
// result to reject an assignment referencing a genre that does not exist,
// field-by-field rather than with a bare constraint error.
func (r *Repository) GenreIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	return r.existingIDs(ctx, "genres", ids)
}

// TagIDs returns which of the given ids exist.
func (r *Repository) TagIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	return r.existingIDs(ctx, "tags", ids)
}

func (r *Repository) existingIDs(
	ctx context.Context, table string, ids []uuid.UUID,
) (map[uuid.UUID]bool, error) {
	found := make(map[uuid.UUID]bool, len(ids))
	if len(ids) == 0 {
		return found, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}

	// table is one of two package-internal constants, never caller input.
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM `+table+` WHERE id IN (`+strings.Join(placeholders, ", ")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("check %s ids: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s id: %w", table, err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("check %s ids: %w", table, err)
	}
	return found, nil
}

// ListTags returns tags for browsing, most-used first (docs/01 §6 "Browse
// tags"). query optionally narrows by substring; limit is enforced by the
// caller through pagination.
//
// The usage count considers only LISTED fictions: a tag used solely on private
// drafts must not advertise their existence (docs/11 §31).
func (r *Repository) ListTags(ctx context.Context, query string, limit, offset int) ([]Tag, int64, error) {
	args := []any{}
	where := ""
	if query != "" {
		args = append(args, "%"+escapeLike(query)+"%")
		where = ` WHERE t.name ILIKE $1`
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM tags t`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count tags: %w", err)
	}
	if total == 0 {
		return []Tag{}, 0, nil
	}

	limitArg := "$" + strconv.Itoa(len(args)+1)
	offsetArg := "$" + strconv.Itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, `
		SELECT t.id, t.name, t.slug, t.created_at,
			(
				SELECT count(*) FROM novel_tags nt
				JOIN novels n ON n.id = nt.novel_id
				WHERE nt.tag_id = t.id
				  AND n.deleted_at IS NULL AND n.status <> 'draft' AND n.visibility = 'public'
			) AS novel_count
		FROM tags t`+where+`
		ORDER BY novel_count DESC, t.name ASC
		LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tags := []Tag{}
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.CreatedAt, &t.NovelCount); err != nil {
			return nil, 0, fmt.Errorf("scan tag: %w", err)
		}
		tags = append(tags, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list tags: %w", err)
	}
	return tags, total, nil
}

// FindOrCreateTag resolves a normalized name to its tag row, creating it if
// new. ON CONFLICT makes concurrent creation of the same tag converge on one
// row instead of erroring (docs/09 §33).
//
// The conflict target is the SLUG, not the name: the slug is derived from the
// name, so a name collision always implies a slug collision - and two names
// that flatten to the same slug ("slow burn", "slow-burn") are one tag as far
// as a filter URL is concerned, so they converge on whichever spelling arrived
// first.
func (r *Repository) FindOrCreateTag(ctx context.Context, name, tagSlug string) (*Tag, error) {
	// DO UPDATE (a no-op write) rather than DO NOTHING so RETURNING always
	// yields the row, whichever writer got there first.
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO tags (name, slug)
		VALUES ($1, $2)
		ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id, name, slug, created_at`, name, tagSlug)

	var t Tag
	if err := row.Scan(&t.ID, &t.Name, &t.Slug, &t.CreatedAt); err != nil {
		return nil, fmt.Errorf("find or create tag: %w", err)
	}
	return &t, nil
}

// escapeLike neutralises wildcards in a search value.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}
