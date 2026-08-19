package shelves

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Public-shelf search - the fourth tab of /search (search review 2026-08
// section F). A reader's public shelf is a curated recommendation list, and
// "ชั้นหนังสือสาธารณะ" makes those findable by name.
//
// Only shelves whose owner opted in are searchable, the owner must still have
// a public profile, and the item count is the PUBLIC count - the same
// predicates the shelf page itself applies, so search can never surface a
// shelf (or a count) its own page would not show.

// SearchLimit caps shelf search results. Like author search it is a bounded
// panel, not a paged listing.
const SearchLimit = 8

// SearchOwner is the shelf owner as a search result shows them.
type SearchOwner struct {
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
}

// SearchHit is one public shelf matching a search.
type SearchHit struct {
	ID        uuid.UUID   `json:"id"`
	Name      string      `json:"name"`
	Note      *string     `json:"note,omitempty"`
	ItemCount int64       `json:"item_count"`
	Owner     SearchOwner `json:"owner"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// escapeLike neutralises the wildcards a searcher could otherwise inject,
// exactly as the novels repository does.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// SearchPublic returns public shelves whose name or note matches.
func (r *Repository) SearchPublic(
	ctx context.Context, query string, limit int,
) ([]SearchHit, error) {
	args := &argList{}
	pattern := args.add("%" + escapeLike(query) + "%")
	itemFilter := publicItemSQL(args)

	// The readable-item count appears twice - once to report, once to refuse
	// empty shelves. A public shelf with nothing a guest may read is not a
	// result, it is a dead end wearing a matching name.
	countSQL := `(
		SELECT count(*) FROM shelf_items si
		JOIN novels n ON n.id = si.novel_id
		WHERE si.shelf_id = s.id AND ` + itemFilter + `
	)`

	sqlText := `SELECT s.id, s.name, s.note, s.updated_at,
			u.username, p.display_name, p.avatar_url,
			` + countSQL + ` AS item_count
		FROM shelves s
		JOIN users u ON u.id = s.user_id
		LEFT JOIN user_profiles p ON p.user_id = s.user_id
		WHERE s.is_public AND ` + visibleAccountSQL + `
		  AND (s.name ILIKE ` + pattern + ` OR s.note ILIKE ` + pattern + `)
		  AND ` + countSQL + ` > 0
		ORDER BY item_count DESC, s.updated_at DESC
		LIMIT ` + args.add(limit)

	rows, err := r.db.QueryContext(ctx, sqlText, args.args...)
	if err != nil {
		return nil, fmt.Errorf("search shelves: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hits := []SearchHit{}
	for rows.Next() {
		var hit SearchHit
		if err := rows.Scan(&hit.ID, &hit.Name, &hit.Note, &hit.UpdatedAt,
			&hit.Owner.Username, &hit.Owner.DisplayName, &hit.Owner.AvatarURL,
			&hit.ItemCount); err != nil {
			return nil, fmt.Errorf("scan shelf hit: %w", err)
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// SearchPublic answers the shelf tab of search. An empty query is an empty
// list, mirroring author search - the endpoint is for suggestions panels and
// tabs, both of which handle "" locally.
func (s *Service) SearchPublic(ctx context.Context, query string) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []SearchHit{}, nil
	}
	if runes := []rune(query); len(runes) > 100 {
		// Cut by RUNES - a byte cut could split a Thai character and hand
		// PostgreSQL invalid UTF-8.
		query = string(runes[:100])
	}

	hits, err := s.repo.SearchPublic(ctx, query, SearchLimit)
	if err != nil {
		return nil, s.internal("search shelves", err)
	}
	return hits, nil
}

// SearchPublic handles GET /api/v1/search/shelves. Guests are welcome, like
// every other kind of discovery.
func (h *Handler) SearchPublic(c *gin.Context) {
	hits, err := h.service.SearchPublic(c.Request.Context(), c.Query("q"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, hits)
}
