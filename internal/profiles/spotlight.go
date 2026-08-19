package profiles

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// อันดับนักเขียน - the home page's writer band (docs/WRITER-SPOTLIGHT.md).
//
// Three rankings take turns, one per ISO week, so the same six names cannot
// hold the front page forever:
//
//   - rising      ถูกเพิ่มเข้าชั้นหนังสือมากที่สุดเดือนนี้. Bookshelf adds,
//     NOT views: a fifty-novel backlist wins a view ranking permanently, and
//     views are the easiest number on the platform to inflate. Putting a book
//     on your own shelf is the cheapest honest signal of intent we store.
//   - newcomer    first published within the last 90 days. Its own ranking on
//     purpose - inside any all-time ladder a new writer starts at the bottom
//     of a hill the incumbents finished climbing years ago.
//   - consistent  a live chapter in consecutive weeks. The one no other
//     platform runs, and the one that matches this platform's stance: it
//     rewards the habit of showing up, which every writer controls, rather
//     than an audience, which none of them do.
//
// Two rules bound the whole feature:
//
//   - hide_from_rankings excludes a writer from every query here. A ranking
//     of PEOPLE presses harder than a ranking of stories, so staying out of
//     it is the writer's call (the hide_counts principle, one level up).
//   - No exact numbers leave the API. Counts are coarsened to bands ("10+",
//     "50+", "100+") before they are serialized; the streak's week count is
//     the only precise figure, because it measures the writer's own habit
//     and not the size of their audience.

// SpotlightKind names one of the rotating rankings.
type SpotlightKind string

const (
	SpotlightRising     SpotlightKind = "rising"
	SpotlightNewcomer   SpotlightKind = "newcomer"
	SpotlightConsistent SpotlightKind = "consistent"
)

const (
	// spotlightSize is the band's width: the review asked for 5-6 people.
	spotlightSize = 6
	// spotlightMin is the fewest writers worth showing. Below this the week's
	// kind is skipped for the next in rotation - two lonely cards read as a
	// platform with two writers, which serves neither of them.
	spotlightMin = 3
	// consistentMinWeeks keeps the streak ranking meaningful: everyone who
	// published anything this week has a "streak" of one.
	consistentMinWeeks = 3
)

// SpotlightWriter is one person on the band. Deliberately narrow: no follower
// count, no view total - the card links to the profile for anyone who wants
// the person, and the band must not become a scoreboard.
type SpotlightWriter struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	PenName     *string   `json:"pen_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`

	// Band is the coarsened metric ("10+", "50+", "100+"), empty below the
	// first threshold. Never an exact count - docs/WRITER-SPOTLIGHT.md.
	Band string `json:"band,omitempty"`
	// StreakWeeks is set only for the consistent ranking.
	StreakWeeks int64 `json:"streak_weeks,omitempty"`
}

// Spotlight is the week's ranking, as `GET /writers/spotlight` returns it.
type Spotlight struct {
	Kind    SpotlightKind     `json:"kind"`
	Writers []SpotlightWriter `json:"writers"`
}

// spotlightRotation is the preference order for a given ISO week: the week's
// own kind first, then the others as fallbacks for a thin week. Pure, so the
// rotation is testable without a clock.
func spotlightRotation(week int) []SpotlightKind {
	kinds := []SpotlightKind{SpotlightRising, SpotlightNewcomer, SpotlightConsistent}
	start := week % len(kinds)
	if start < 0 {
		start += len(kinds)
	}
	ordered := make([]SpotlightKind, 0, len(kinds))
	for i := range kinds {
		ordered = append(ordered, kinds[(start+i)%len(kinds)])
	}
	return ordered
}

// band coarsens a count into the only resolution the API publishes.
func band(n int64) string {
	switch {
	case n >= 100:
		return "100+"
	case n >= 50:
		return "50+"
	case n >= 10:
		return "10+"
	default:
		return ""
	}
}

// spotlightExcludedSQL is the writer-control predicate every ranking shares,
// against aliases `u` and `p`. COALESCE because user_profiles is LEFT JOINed:
// a missing profile row means the default, which is "listed".
const spotlightExcludedSQL = `
	NOT COALESCE(p.hide_from_rankings, FALSE)`

// spotlightRisingSQL ranks by bookshelf adds this calendar month. Only adds
// pointing at work a stranger can open count - a bookmark of a private draft
// is real to its owner but must not move a public ranking.
const spotlightRisingSQL = `
	SELECT u.id, u.username, p.display_name, p.avatar_url, ap.pen_name,
	       count(*) AS metric
	FROM bookmarks b
	JOIN novels n ON n.id = b.novel_id AND ` + novels.ReadableSQL + `
	JOIN users u ON u.id = n.author_id
	LEFT JOIN user_profiles p    ON p.user_id = u.id
	LEFT JOIN author_profiles ap ON ap.user_id = u.id
	WHERE b.created_at >= date_trunc('month', now())
	  AND ` + visibleAccountSQL + `
	  AND ` + spotlightExcludedSQL + `
	GROUP BY u.id, u.username, p.display_name, p.avatar_url, ap.pen_name,
	         u.follower_count
	ORDER BY count(*) DESC, u.follower_count DESC, u.id
	LIMIT $1`

// spotlightNewcomerSQL ranks writers whose FIRST readable work went live
// within the last 90 days, by this month's bookshelf adds. The window is on
// the first publication, not the account age: someone who lurked for a year
// and published last month is exactly who this ranking is for.
const spotlightNewcomerSQL = `
	WITH firsts AS (
		SELECT n.author_id, min(n.published_at) AS first_published
		FROM novels n
		WHERE ` + novels.ReadableSQL + ` AND n.published_at IS NOT NULL
		GROUP BY n.author_id
	)
	SELECT u.id, u.username, p.display_name, p.avatar_url, ap.pen_name,
	       (
			SELECT count(*)
			FROM bookmarks b
			JOIN novels n ON n.id = b.novel_id AND ` + novels.ReadableSQL + `
			WHERE n.author_id = u.id
			  AND b.created_at >= date_trunc('month', now())
	       ) AS metric
	FROM firsts f
	JOIN users u ON u.id = f.author_id
	LEFT JOIN user_profiles p    ON p.user_id = u.id
	LEFT JOIN author_profiles ap ON ap.user_id = u.id
	WHERE f.first_published >= now() - interval '90 days'
	  AND ` + visibleAccountSQL + `
	  AND ` + spotlightExcludedSQL + `
	ORDER BY metric DESC, u.follower_count DESC, f.first_published DESC, u.id
	LIMIT $1`

// spotlightConsistentSQL ranks by consecutive weeks with at least one live
// chapter, anchored at this week or the one before (a streak must not die on
// Monday morning because this week's chapter is not up yet).
//
// The prefix trick in `streaks`: rows are numbered newest-first per author,
// so a row belongs to the unbroken run exactly when its week equals the
// latest week minus (row number - 1) weeks. The first gap breaks the
// equality for every later row - weeks fall behind row numbers and never
// catch up - so count(*) is the streak length.
const spotlightConsistentSQL = `
	WITH weeks AS (
		SELECT DISTINCT n.author_id,
		       date_trunc('week',
		           COALESCE(c.published_at, c.scheduled_at, c.created_at)) AS wk
		FROM chapters c
		JOIN novels n ON n.id = c.novel_id AND ` + novels.ReadableSQL + `
		WHERE ` + novels.LiveChapterSQL + `
		  AND COALESCE(c.published_at, c.scheduled_at, c.created_at)
		      >= now() - interval '26 weeks'
	),
	runs AS (
		SELECT author_id, wk,
		       max(wk) OVER (PARTITION BY author_id) AS latest,
		       row_number() OVER (PARTITION BY author_id ORDER BY wk DESC) AS rn
		FROM weeks
	),
	streaks AS (
		SELECT author_id, count(*) AS metric
		FROM runs
		WHERE latest >= date_trunc('week', now()) - interval '7 days'
		  AND wk = latest - ((rn - 1) * interval '7 days')
		GROUP BY author_id
		HAVING count(*) >= $2
	)
	SELECT u.id, u.username, p.display_name, p.avatar_url, ap.pen_name, s.metric
	FROM streaks s
	JOIN users u ON u.id = s.author_id
	LEFT JOIN user_profiles p    ON p.user_id = u.id
	LEFT JOIN author_profiles ap ON ap.user_id = u.id
	WHERE ` + visibleAccountSQL + `
	  AND ` + spotlightExcludedSQL + `
	ORDER BY s.metric DESC, u.follower_count DESC, u.id
	LIMIT $1`

// SpotlightWriters runs one ranking. The raw metric stays inside this method:
// what leaves is the band (or the streak's week count), never the count
// itself.
func (r *Repository) SpotlightWriters(
	ctx context.Context, kind SpotlightKind,
) ([]SpotlightWriter, error) {
	var (
		query string
		args  []any
	)
	switch kind {
	case SpotlightNewcomer:
		query, args = spotlightNewcomerSQL, []any{spotlightSize}
	case SpotlightConsistent:
		query, args = spotlightConsistentSQL, []any{spotlightSize, consistentMinWeeks}
	default:
		query, args = spotlightRisingSQL, []any{spotlightSize}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("spotlight %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	out := []SpotlightWriter{}
	for rows.Next() {
		var (
			writer SpotlightWriter
			metric int64
		)
		if err := rows.Scan(
			&writer.ID, &writer.Username, &writer.DisplayName,
			&writer.AvatarURL, &writer.PenName, &metric,
		); err != nil {
			return nil, fmt.Errorf("scan spotlight writer: %w", err)
		}
		if kind == SpotlightConsistent {
			writer.StreakWeeks = metric
		} else {
			writer.Band = band(metric)
		}
		out = append(out, writer)
	}
	return out, rows.Err()
}

// Spotlight returns this week's ranking, falling through the rotation when
// the preferred one is too thin to stand. Public and identical for every
// viewer, so the response is cacheable like the rest of this package.
func (s *Service) Spotlight(ctx context.Context) (*Spotlight, error) {
	_, week := time.Now().ISOWeek()

	var best *Spotlight
	for _, kind := range spotlightRotation(week) {
		writers, err := s.repo.SpotlightWriters(ctx, kind)
		if err != nil {
			s.log.Error("profile service failure",
				slog.String("op", "writer spotlight"), slog.Any("error", err))
			return nil, apierror.Internal()
		}
		view := &Spotlight{Kind: kind, Writers: writers}
		if len(writers) >= spotlightMin {
			return view, nil
		}
		if best == nil || len(writers) > len(best.Writers) {
			best = view
		}
	}
	return best, nil
}
