package novels

import (
	"context"
	"fmt"
)

// Facet counts for the search filter panel (search review 2026-08 section A):
// the panel shows every option WITH how many results choosing it would leave,
// so a reader never opens a dropdown to discover an option that matches
// nothing.
//
// Each dimension is counted with every OTHER filter applied but its own
// dimension released - the standard faceting rule. Counting a dimension under
// its own filter would zero every unselected option the moment one is picked,
// which reads as "nothing else exists" when it means "you filtered it out".
type Facets struct {
	// Total is the match count under the full filter - the number the result
	// header states.
	Total int64 `json:"total"`

	Status             map[string]int64 `json:"status"`
	PresentationFormat map[string]int64 `json:"presentation_format"`
	StoryStructure     map[string]int64 `json:"story_structure"`

	// Origin buckets are original / fanfiction / crossover - crossover DERIVED
	// from the fandom field's " × " convention (docs/FANDOM.md), because that
	// convention is the only place the distinction lives.
	Origin map[string]int64 `json:"origin"`

	Rating map[string]int64 `json:"rating"`

	// Relationship counts per relationship-kind genre slug (BL, GL, ชาย-หญิง,
	// Reader, OC) - the คู่ชิป row of the panel.
	Relationship map[string]int64 `json:"relationship"`

	// HasVariables is how many matches carry reader variables (y/n).
	HasVariables int64 `json:"has_variables"`
}

// groupCount runs one grouped count under the given filter.
func (r *Repository) groupCount(
	ctx context.Context, filter Filter, expr string,
) (map[string]int64, error) {
	args := &argList{}
	query := `SELECT ` + expr + ` AS facet_key, count(*)` + recordFrom +
		filter.where(args) + ` GROUP BY 1`

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, fmt.Errorf("facet count %s: %w", expr, err)
	}
	defer func() { _ = rows.Close() }()

	counts := map[string]int64{}
	for rows.Next() {
		var key string
		var total int64
		if err := rows.Scan(&key, &total); err != nil {
			return nil, fmt.Errorf("scan facet count: %w", err)
		}
		counts[key] = total
	}
	return counts, rows.Err()
}

// scalarCount runs one plain count under the given filter, with an optional
// extra predicate appended.
func (r *Repository) scalarCount(
	ctx context.Context, filter Filter, extra string,
) (int64, error) {
	args := &argList{}
	query := `SELECT count(*)` + recordFrom + filter.where(args) + extra

	var total int64
	if err := r.db.QueryRowContext(ctx, query, args.args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("facet total: %w", err)
	}
	return total, nil
}

// RelationshipSlugs returns every relationship-kind genre slug. The list is a
// handful of controlled terms (BL, GL, ...), fetched so the service can tell
// which selected genre filters belong to the คู่ชิป dimension.
func (r *Repository) RelationshipSlugs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT slug FROM genres WHERE kind = 'relationship' ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list relationship slugs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	slugs := []string{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("scan relationship slug: %w", err)
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// Facets computes the filter-panel counts.
//
// relationshipFilter is the full filter with the relationship-kind genre
// selections released (its own dimension); the SERVICE builds it because
// telling relationship slugs from content slugs takes the taxonomy, which the
// repository's Filter deliberately does not know about.
func (r *Repository) Facets(
	ctx context.Context, filter, relationshipFilter Filter,
) (*Facets, error) {
	facets := &Facets{}
	var err error

	if facets.Total, err = r.scalarCount(ctx, filter, ""); err != nil {
		return nil, err
	}

	released := filter
	released.Status = ""
	if facets.Status, err = r.groupCount(ctx, released, "n.status"); err != nil {
		return nil, err
	}

	released = filter
	released.PresentationFormat = ""
	if facets.PresentationFormat, err = r.groupCount(ctx, released, "n.presentation_format"); err != nil {
		return nil, err
	}

	released = filter
	released.StoryStructure = ""
	if facets.StoryStructure, err = r.groupCount(ctx, released, "n.story_structure"); err != nil {
		return nil, err
	}

	released = filter
	released.Origin = ""
	if facets.Origin, err = r.groupCount(ctx, released, `CASE
		WHEN n.origin_type = 'original' THEN 'original'
		WHEN n.fandom LIKE '% × %' THEN 'crossover'
		ELSE 'fanfiction' END`); err != nil {
		return nil, err
	}

	released = filter
	released.Rating = ""
	if facets.Rating, err = r.groupCount(ctx, released, "n.age_rating"); err != nil {
		return nil, err
	}

	// The คู่ชิป row: one row per relationship genre the matching works carry.
	// DISTINCT n.id because a work carrying two ships must count once per
	// ship, not twice in either.
	args := &argList{}
	query := `SELECT g.slug, count(DISTINCT n.id)` + recordFrom + `
		JOIN novel_genres ng ON ng.novel_id = n.id
		JOIN genres g ON g.id = ng.genre_id AND g.kind = 'relationship'` +
		relationshipFilter.where(args) + ` GROUP BY g.slug`
	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, fmt.Errorf("facet relationship: %w", err)
	}
	defer func() { _ = rows.Close() }()
	facets.Relationship = map[string]int64{}
	for rows.Next() {
		var slug string
		var total int64
		if err := rows.Scan(&slug, &total); err != nil {
			return nil, fmt.Errorf("scan facet relationship: %w", err)
		}
		facets.Relationship[slug] = total
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	released = filter
	released.HasVariables = false
	if facets.HasVariables, err = r.scalarCount(ctx, released,
		` AND EXISTS (SELECT 1 FROM novel_variables v WHERE v.novel_id = n.id)`); err != nil {
		return nil, err
	}

	return facets, nil
}
