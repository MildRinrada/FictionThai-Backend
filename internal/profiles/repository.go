package profiles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/pennames"
	"github.com/fictionthai/fictionthai/backend/internal/users"
)

// ErrNotFound covers every reason a profile cannot be shown. The service
// translates it into the single 404 all of them share.
var ErrNotFound = errors.New("profile not found")

// Repository reads the identity tables and the work counters in one query.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// visibleAccountSQL is the predicate for "an account that has a public
// profile", against alias `u`.
//
// Deleted accounts are gone. A BANNED account goes with them: its work is
// already unreachable, and leaving the profile standing would publish the
// outcome of a moderation decision (docs/11 §37).
//
// A SUSPENDED account stays visible on purpose. A suspension stops someone
// acting, not existing; their published fictions remain readable, so a profile
// that vanished would strand every link pointing at it.
const visibleAccountSQL = `
	u.deleted_at IS NULL AND u.status NOT IN ('deleted', 'banned')`

// ByRef loads one person's public profile, or ErrNotFound for anything a
// stranger may not see.
//
// The two novel aggregates are correlated subqueries over novels(author_id),
// the same shape every other card count uses (docs/07 §67). Both filter with
// novels.ReadableSQL - the shared predicate, not a private copy, because a
// profile that counted work by its own rule would eventually count a private
// draft (docs/11 §31).
func (r *Repository) ByRef(ctx context.Context, ref Ref) (*Profile, error) {
	var (
		where string
		arg   any
	)
	if ref.ByUsername() {
		// username is CITEXT, so this is the same case-insensitive comparison
		// registration used (docs/10 §7).
		where = `u.username = $1`
		arg = users.NormalizeUsername(ref.Username)
	} else {
		where = `u.id = $1`
		arg = ref.ID
	}

	query := `
		SELECT
			u.id, u.username, u.created_at, u.follower_count,
			p.display_name, p.bio, p.avatar_url, p.banner_url, p.website_url,
			COALESCE(p.links, '[]'::jsonb),
			(ap.user_id IS NOT NULL) AS is_author,
			ap.pen_name, ap.author_bio, ap.donation_url,
			COALESCE(ap.open_for, '[]'::jsonb),
			ap.boundaries,
			-- The wall's switch. COALESCE because user_profiles is LEFT JOINed:
			-- an account whose profile row is somehow missing has an OPEN wall,
			-- which is the documented default rather than a silent closure.
			COALESCE(p.wall_enabled, TRUE) AS wall_enabled,
			-- The rankings opt-out (docs/WRITER-SPOTLIGHT.md). Defaults the
			-- other way from the wall: a missing profile row is LISTED.
			COALESCE(p.hide_from_rankings, FALSE) AS hide_from_rankings,
			(
				SELECT count(*) FROM novels n
				WHERE n.author_id = u.id AND ` + novels.ReadableSQL + `
			) AS novel_count,
			-- จบแล้ว is the number a READER decides by (profile review
			-- 2026-08): a finished story is a promise kept.
			(
				SELECT count(*) FROM novels n
				WHERE n.author_id = u.id AND n.status = 'completed'
				  AND ` + novels.ReadableSQL + `
			) AS completed_count,
			(
				SELECT COALESCE(sum(n.view_count), 0) FROM novels n
				WHERE n.author_id = u.id AND ` + novels.ReadableSQL + `
			) AS total_views,
			-- The writer's identities and their recent renames, aggregated in
			-- the SAME round trip (docs/07 §67 - the profile is a reader-facing
			-- page and must stay one query). Both are viewer-INDEPENDENT: a pen
			-- name is printed on the covers, so it is the same answer for a
			-- guest, a stranger, and the person themselves, and the response
			-- stays cacheable (docs/14 §7).
			(
				SELECT COALESCE(jsonb_agg(jsonb_build_object(
					'id',         pn.id,
					'name',       pn.name,
					'note',       pn.note,
					'is_default', pn.is_default
				) ORDER BY pn.is_default DESC, pn.created_at, pn.id), '[]'::jsonb)
				FROM pen_names pn
				WHERE pn.user_id = u.id
			) AS pen_names,
			(
				-- «เคยใช้ชื่อ …»: names used inside the window and no longer
				-- used. A name the writer took back is excluded - renaming
				-- A → B → A must not tell readers this person "used to be" the
				-- name currently on their work.
				SELECT COALESCE(jsonb_agg(f.previous_name ORDER BY f.changed_at DESC),
				                '[]'::jsonb)
				FROM (
					SELECT DISTINCT ON (lower(h.previous_name))
						h.previous_name, h.changed_at
					FROM pen_name_history h
					WHERE h.user_id = u.id
					  AND h.changed_at > now() - $2::interval
					  AND NOT EXISTS (
						SELECT 1 FROM pen_names p
						WHERE p.user_id = h.user_id
						  AND lower(p.name) = lower(h.previous_name)
					  )
					ORDER BY lower(h.previous_name), h.changed_at DESC
				) f
			) AS former_names,
			-- ชั้นวางเรื่องที่ปักหมุด: the owner's own three, re-checked for
			-- readability HERE rather than trusted from the stored JSON, so a
			-- work that has since become private simply stops rendering
			-- instead of leaking its title from a cached pin.
			(
				SELECT COALESCE(jsonb_agg(jsonb_build_object(
					'novel_id', n.id,
					'slug',     n.slug,
					'title',    n.title,
					'note',     pin.note
				) ORDER BY pin.ord), '[]'::jsonb)
				-- ROWS FROM(...) because WITH ORDINALITY cannot take a column
				-- definition list directly; the ordinal preserves the owner's
				-- chosen order.
				FROM ROWS FROM (
					jsonb_to_recordset(COALESCE(p.pinned, '[]'::jsonb))
						AS (novel_id uuid, note text)
				) WITH ORDINALITY AS pin(novel_id, note, ord)
				JOIN novels n ON n.id = pin.novel_id
				WHERE n.author_id = u.id AND ` + novels.ReadableSQL + `
			) AS pinned
		FROM users u
		LEFT JOIN user_profiles p    ON p.user_id = u.id
		LEFT JOIN author_profiles ap ON ap.user_id = u.id
		WHERE ` + where + ` AND ` + visibleAccountSQL

	var (
		profile         Profile
		linksJSON       []byte
		openForJSON     []byte
		penNamesJSON    []byte
		formerNamesJSON []byte
		pinnedJSON      []byte
	)
	err := r.db.QueryRowContext(ctx, query, arg, pennames.HistoryInterval()).Scan(
		&profile.ID, &profile.Username, &profile.JoinedAt, &profile.FollowerCount,
		&profile.DisplayName, &profile.Bio, &profile.AvatarURL, &profile.BannerURL,
		&profile.WebsiteURL, &linksJSON,
		&profile.IsAuthor,
		&profile.PenName, &profile.AuthorBio, &profile.DonationURL, &openForJSON,
		&profile.Boundaries, &profile.WallEnabled, &profile.HideFromRankings,
		&profile.NovelCount, &profile.CompletedCount, &profile.TotalViews,
		&penNamesJSON, &formerNamesJSON, &pinnedJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load public profile: %w", err)
	}
	profile.Links = decodeLinks(linksJSON)
	profile.OpenFor = decodeStrings(openForJSON)
	profile.PenNames = decodePenNames(penNamesJSON)
	profile.FormerNames = decodeStrings(formerNamesJSON)
	profile.Pinned = decodePinned(pinnedJSON)
	return &profile, nil
}

// decodeLinks turns the stored JSON into the API shape. Stored JSON that no
// longer parses is treated as absent rather than as a failed profile load: a
// malformed link list must never take a person's page down.
func decodeLinks(raw []byte) []Link {
	links := []Link{}
	if len(raw) == 0 {
		return links
	}
	if err := json.Unmarshal(raw, &links); err != nil {
		return []Link{}
	}
	return links
}

// decodePinned reads the pinned shelf, forgiving unreadable stored JSON the
// same way the other decoders do.
func decodePinned(raw []byte) []PinnedWork {
	pinned := []PinnedWork{}
	if len(raw) == 0 {
		return pinned
	}
	if err := json.Unmarshal(raw, &pinned); err != nil {
		return []PinnedWork{}
	}
	return pinned
}

// decodePenNames turns the aggregated JSON into the API shape, with the same
// forgiveness decodeLinks shows: unreadable stored JSON is treated as an empty
// list rather than as a failed profile load. A writer's page must not go down
// because of one malformed row.
func decodePenNames(raw []byte) []PenNameView {
	views := []PenNameView{}
	if len(raw) == 0 {
		return views
	}
	if err := json.Unmarshal(raw, &views); err != nil {
		return []PenNameView{}
	}
	return views
}

func decodeStrings(raw []byte) []string {
	values := []string{}
	if len(raw) == 0 {
		return values
	}
	if err := json.Unmarshal(raw, &values); err != nil {
		return []string{}
	}
	return values
}

// Update writes the caller's own profile row, then re-reads the public view so
// the client renders exactly what the next visitor will see.
//
// The two tables are written in one transaction: user_profiles always exists
// (registration creates it), author_profiles is created on demand the same way
// the donation link creates it - an explicit user action on a settings
// surface, never a silent side effect.
func (r *Repository) Update(ctx context.Context, userID uuid.UUID, edit *Edit) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin profile update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// COALESCE keeps an untouched field untouched: the parameter is NULL only
	// when the client did not send that key at all.
	var links any
	if edit.Links != nil {
		encoded, err := json.Marshal(*edit.Links)
		if err != nil {
			return fmt.Errorf("encode links: %w", err)
		}
		links = encoded
	}
	var pinned any
	if edit.Pinned != nil {
		encoded, err := json.Marshal(*edit.Pinned)
		if err != nil {
			return fmt.Errorf("encode pinned: %w", err)
		}
		pinned = encoded
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_profiles SET
			display_name = CASE WHEN $2::text IS NULL THEN display_name
			                    WHEN $2 = '' THEN NULL ELSE $2 END,
			bio          = CASE WHEN $3::text IS NULL THEN bio
			                    WHEN $3 = '' THEN NULL ELSE $3 END,
			website_url  = CASE WHEN $4::text IS NULL THEN website_url
			                    WHEN $4 = '' THEN NULL ELSE $4 END,
			links        = COALESCE($5::jsonb, links),
			wall_enabled = COALESCE($6::boolean, wall_enabled),
			pinned       = COALESCE($7::jsonb, pinned),
			hide_from_rankings = COALESCE($8::boolean, hide_from_rankings),
			updated_at   = now()
		WHERE user_id = $1`,
		userID, edit.DisplayName, edit.Bio, edit.WebsiteURL, links, edit.WallEnabled,
		pinned, edit.HideFromRankings,
	); err != nil {
		return fmt.Errorf("update user profile: %w", err)
	}

	// The author half. One statement for both fields rather than two, so a save
	// that sets availability and boundaries together creates the row once.
	if edit.OpenFor != nil || edit.Boundaries != nil {
		var openFor any
		if edit.OpenFor != nil {
			encoded, err := json.Marshal(*edit.OpenFor)
			if err != nil {
				return fmt.Errorf("encode open_for: %w", err)
			}
			openFor = encoded
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO author_profiles (user_id, open_for, boundaries)
			VALUES (
				$1,
				COALESCE($2::jsonb, '[]'::jsonb),
				CASE WHEN $3::text IS NULL OR $3 = '' THEN NULL ELSE $3 END
			)
			ON CONFLICT (user_id) DO UPDATE SET
				open_for   = COALESCE($2::jsonb, author_profiles.open_for),
				boundaries = CASE WHEN $3::text IS NULL THEN author_profiles.boundaries
				                  WHEN $3 = '' THEN NULL ELSE $3 END,
				updated_at = now()`,
			userID, openFor, edit.Boundaries,
		); err != nil {
			return fmt.Errorf("update author availability: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit profile update: %w", err)
	}
	return nil
}
