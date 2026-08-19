package profiles

import (
	"context"
	"fmt"
	"strings"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
)

// Author search - "หานักเขียนคนนี้", the half of search that did not exist.
//
// Fiction and tags were both searchable; people were not, so the only way to
// reach a writer was through something they had written. On a platform whose
// readers follow AUTHORS as much as stories, that is a missing door rather
// than a missing filter.

// SearchLimit caps a suggestion list. The navbar shows a handful per group;
// anyone who wants the rest goes to the search page.
const SearchLimit = 5

// Author is one person, as a search suggestion.
type Author struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	// IsAuthor separates "has published fiction" from "has an account", so the
	// client can say so rather than implying everyone writes.
	IsAuthor bool `json:"is_author"`
}

// SearchAuthors finds accounts whose handle or chosen name contains the query.
//
// Only accounts a stranger may see (`visibleAccountSQL`) - the same predicate
// the profile read uses, not a copy of its logic, so a banned account cannot
// reappear here after being removed from there.
//
// Writers first. Someone typing a name into a fiction site is looking for the
// person who wrote something far more often than for a reader with a similar
// handle, and ordering by "has published" costs one boolean.
func (r *Repository) SearchAuthors(
	ctx context.Context, query string, limit int,
) ([]Author, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return []Author{}, nil
	}
	if limit <= 0 || limit > SearchLimit*4 {
		limit = SearchLimit
	}

	// LIKE with the wildcards bound as DATA, never concatenated into the SQL.
	pattern := "%" + escapeLike(trimmed) + "%"

	rows, err := r.db.QueryContext(ctx, `
		SELECT
			u.username,
			COALESCE(p.display_name, ''),
			COALESCE(p.avatar_url, ''),
			EXISTS (
				SELECT 1 FROM novels n
				WHERE n.author_id = u.id AND `+novels.ReadableSQL+`
			) AS is_author
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE `+visibleAccountSQL+`
		  AND (u.username ILIKE $1 ESCAPE '\' OR p.display_name ILIKE $1 ESCAPE '\')
		ORDER BY is_author DESC, length(u.username), u.username
		LIMIT $2`,
		pattern, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("search authors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Author{}
	for rows.Next() {
		var author Author
		if err := rows.Scan(
			&author.Username, &author.DisplayName, &author.AvatarURL, &author.IsAuthor,
		); err != nil {
			return nil, fmt.Errorf("scan author: %w", err)
		}
		out = append(out, author)
	}
	return out, rows.Err()
}

// escapeLike neutralises the wildcards a searcher typed.
//
// Without this, a query of "%" matches every account on the platform in one
// request - a cheap way to enumerate the user table, which is exactly what an
// endpoint that lists people must not offer.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}
