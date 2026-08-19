package novels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/internal/taxonomy"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// Sentinel errors. The repository never builds HTTP responses; the service
// translates these.
var (
	ErrNotFound  = errors.New("novel not found")
	ErrSlugTaken = errors.New("slug already taken")
	// ErrCollaboratorUser - the username being added as a co-writer does not
	// name an account (13U).
	ErrCollaboratorUser = errors.New("collaborator user not found")
)

// Repository is the only place that reads or writes the `novels` table.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// novelColumns is shared by every SELECT so a new column cannot be added to one
// query and forgotten in another. The unqualified form is used where there is
// no join and therefore no `n` alias.
const novelColumns = `
	n.id, n.author_id, n.title, n.slug, n.description, n.tagline, n.foreword,
	n.cover_url,
	n.story_structure, n.presentation_format, n.content_mode,
	n.status, n.visibility, n.content_warning, n.content_warning_spoiler,
	n.age_rating, n.age_gate, n.origin_type, n.fandom,
	n.language, n.chapter_unit, n.author_note_start, n.author_note_end,
	n.series_name, n.series_position, n.comment_access, n.comment_approval,
	n.allow_screenshot, n.allow_translation, n.allow_derivative,
	n.allow_audio, n.require_credit, n.derivative_terms,
	n.view_count, n.like_count, n.bookmark_count,
	n.hide_counts, n.show_donate, n.theme_color, n.publish_at,
	n.published_at, n.created_at, n.updated_at, n.deleted_at`

const novelColumnsBare = `
	id, author_id, title, slug, description, tagline, foreword, cover_url,
	story_structure, presentation_format, content_mode,
	status, visibility, content_warning, content_warning_spoiler,
	age_rating, age_gate, origin_type, fandom,
	language, chapter_unit, author_note_start, author_note_end,
	series_name, series_position, comment_access, comment_approval,
	allow_screenshot, allow_translation, allow_derivative,
	allow_audio, require_credit, derivative_terms,
	view_count, like_count, bookmark_count,
	hide_counts, show_donate, theme_color, publish_at,
	published_at, created_at, updated_at, deleted_at`

// LiveChapterSQL is the predicate for "a chapter a non-owner may read", written
// against the alias `c`.
//
// It lives in this package because both domains need it: novels counts readable
// chapters for its cards, and chapters filters reads with it. Duplicating it
// would eventually let a published fiction leak an unpublished chapter through
// whichever copy drifted (docs/11 §21).
//
// Its Go twin is chapters.Chapter.Live, and an integration test asserts the two
// agree on every boundary case.
const LiveChapterSQL = `
	c.deleted_at IS NULL AND (
		(c.status = 'published' AND (c.published_at IS NULL OR c.published_at <= now()))
		OR
		(c.status = 'scheduled' AND c.scheduled_at IS NOT NULL AND c.scheduled_at <= now())
	)`

// ReadableSQL is the predicate for "a fiction ANY reader may open, account or
// not", written against the alias `n`. It is the SQL twin of Novel.Readable -
// the guest tier - exported for the same reason LiveChapterSQL is: the library
// domain filters its listings with it, and a private copy there would
// eventually drift and leak a private draft through someone's bookmark shelf
// (docs/11 §31).
//
// It is an ALLOWLIST, and that is load-bearing (§13C). It used to read
// `visibility <> 'private'`, which would have published every fiction on the
// members and followers rungs the moment the CHECK constraint widened - in five
// call sites at once, silently. Listing what is allowed rather than what is
// forbidden makes a new rung default to invisible, which is the direction a
// mistake here has to fail in.
const ReadableSQL = `
	n.deleted_at IS NULL AND n.status <> 'draft'
	AND (n.publish_at IS NULL OR n.publish_at <= now())
	AND n.visibility IN ('public', 'unlisted')`

// ReadableSQLFor is ReadableSQL for a KNOWN viewer: the same rule, plus the two
// rungs that depend on who is asking (§13C).
//
// viewer is an SQL expression for the viewer's user id - a bound placeholder, or
// a column such as `f.follower_id` where the recipient is already in the query.
// It is never caller text: every call site in the codebase passes a placeholder
// it produced itself.
func ReadableSQLFor(viewer string) string {
	return `
	n.deleted_at IS NULL AND n.status <> 'draft'
	AND (n.publish_at IS NULL OR n.publish_at <= now()) AND (
		n.visibility IN ('public', 'unlisted')
		OR (n.visibility = 'members' AND ` + viewer + ` IS NOT NULL)
		OR (n.visibility = 'followers' AND EXISTS (
			SELECT 1 FROM user_follows uf
			WHERE uf.following_id = n.author_id AND uf.follower_id = ` + viewer + `
		))
	)`
}

// PublishedSQL is a WEAKER question than ReadableSQL: has this fiction been
// published to anyone other than its owner?
//
// It exists for the notification router, where the recipient is the author or a
// participant in the thread rather than a browsing reader, and where the
// audience rungs are the wrong test - a comment on a followers-only fiction
// still has to reach the person who wrote it. Nothing reader-facing may use
// this; it does not answer "may this person open it".
const PublishedSQL = `
	n.deleted_at IS NULL AND n.status <> 'draft' AND n.visibility <> 'private'`

// listedSQL is the predicate for "may appear in a browse surface", written
// against `n`. Public always; members too, but only for a viewer who could
// actually open it - listing work to someone who would be turned away at the
// door is worse than not listing it.
func listedSQL(viewer string) string {
	return `(n.publish_at IS NULL OR n.publish_at <= now())
	AND (n.visibility = 'public' OR (n.visibility = 'members' AND ` +
		viewer + ` IS NOT NULL))`
}

// viewerArg binds a viewer id as a NULLable uuid and returns the expression to
// interpolate.
//
// uuid.Nil is bound as SQL NULL rather than as a row of zeros, which is the
// whole point: `$1 IS NOT NULL` is how both viewer-dependent predicates ask
// "is anybody signed in", and a zero uuid would answer yes for every guest on
// the platform. The explicit ::uuid cast is what lets PostgreSQL type a
// parameter whose only other appearance is a NULL test.
func viewerArg(args *argList, id uuid.UUID) string {
	return args.add(ViewerValue(id)) + "::uuid"
}

// ViewerValue is the bound form of a viewer id, for the other domains that
// build their own parameter lists and filter with ReadableSQLFor.
//
// uuid.Nil becomes SQL NULL, never a row of zeros. That is the difference
// between a guest being refused members-only work and a guest being handed it:
// `$1 IS NOT NULL` is how the predicate asks "is anybody signed in", and a zero
// uuid answers yes. Callers must append `::uuid` to the placeholder so
// PostgreSQL can type a parameter that may only ever appear in a NULL test.
func ViewerValue(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// recordColumns adds the joined author and the two chapter counts.
//
// The counts are correlated subqueries rather than a second round trip: a page
// of results costs one query, which is what keeps the reader path cheap
// (docs/07 §67, docs/09 §21). Both are index scans on
// chapters_novel_status_idx.
const recordColumns = novelColumns + `,
	u.id, u.username, p.display_name, p.avatar_url, ap.donation_url,
	n.pen_name_id,
	-- The identity this work is published under
	-- (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2): the pen name the author chose
	-- for it, falling back to their default when it named none. Two correlated
	-- lookups rather than joins, so the listing COUNT query is untouched and the
	-- second one only runs when there is no explicit choice - COALESCE
	-- short-circuits. Both are single-row index lookups.
	COALESCE(
		(
			SELECT pn.name FROM pen_names pn
			WHERE pn.id = n.pen_name_id AND pn.user_id = n.author_id
		),
		(
			SELECT pd.name FROM pen_names pd
			WHERE pd.user_id = n.author_id AND pd.is_default
		)
	) AS pen_name,
	(
		SELECT count(*) FROM chapters c
		WHERE c.novel_id = n.id AND ` + LiveChapterSQL + `
	) AS published_chapters,
	(
		SELECT count(*) FROM chapters c
		WHERE c.novel_id = n.id AND c.deleted_at IS NULL
	) AS total_chapters,
	(
		-- "ผสมรูปแบบ" is an OBSERVATION about the chapters that exist, never a
		-- setting (§13J, revised). A stored flag would have been a lock the
		-- writer could not see the effect of; this is simply true or not.
		SELECT EXISTS (
			SELECT 1 FROM chapters c
			WHERE c.novel_id = n.id AND c.deleted_at IS NULL
			  AND c.presentation_format IS NOT NULL
			  AND c.presentation_format <> n.presentation_format
		)
	) AS has_mixed_formats,
	(
		-- Whether the work uses reader variables (y/n) - a fact a result card
		-- states and a filter narrows on (search review 2026-08 section B).
		-- Index scan on novel_variables' novel_id foreign key.
		SELECT EXISTS (SELECT 1 FROM novel_variables v WHERE v.novel_id = n.id)
	) AS has_reader_variables,
	(
		-- The first chapter a non-owner may read, so a card can link
		-- "อ่านตอนแรก" directly (section D6). NULL when nothing is live.
		SELECT c.slug FROM chapters c
		WHERE c.novel_id = n.id AND ` + LiveChapterSQL + `
		ORDER BY c.chapter_number ASC LIMIT 1
	) AS first_chapter_slug`

const recordFrom = `
	FROM novels n
	JOIN users u ON u.id = n.author_id
	LEFT JOIN user_profiles p ON p.user_id = n.author_id
	LEFT JOIN author_profiles ap ON ap.user_id = n.author_id`

type scanner interface{ Scan(...any) error }

func scanNovel(row scanner) (*Novel, error) {
	var n Novel
	err := row.Scan(
		&n.ID, &n.AuthorID, &n.Title, &n.Slug, &n.Description, &n.Tagline,
		&n.Foreword, &n.CoverURL,
		&n.Format.StoryStructure, &n.Format.PresentationFormat, &n.Format.ContentMode,
		&n.Status, &n.Visibility, &n.ContentWarning, &n.ContentWarningSpoiler,
		&n.AgeRating, &n.AgeGate, &n.OriginType, &n.Fandom,
		&n.Extras.Language, &n.Extras.ChapterUnit,
		&n.Extras.AuthorNoteStart, &n.Extras.AuthorNoteEnd,
		&n.Extras.SeriesName, &n.Extras.SeriesPosition,
		&n.Extras.CommentAccess, &n.Extras.CommentApproval,
		&n.Extras.Rights.AllowScreenshot, &n.Extras.Rights.AllowTranslation,
		&n.Extras.Rights.AllowDerivative, &n.Extras.Rights.AllowAudio,
		&n.Extras.Rights.RequireCredit, &n.Extras.Rights.DerivativeTerms,
		&n.ViewCount, &n.LikeCount, &n.BookmarkCount,
		&n.HideCounts, &n.ShowDonate, &n.ThemeColor, &n.PublishAt,
		&n.PublishedAt, &n.CreatedAt, &n.UpdatedAt, &n.DeletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan novel: %w", err)
	}
	return &n, nil
}

func scanRecord(row scanner) (*Record, error) {
	var r Record
	err := row.Scan(
		&r.ID, &r.AuthorID, &r.Title, &r.Slug, &r.Description, &r.Tagline,
		&r.Foreword, &r.CoverURL,
		&r.Format.StoryStructure, &r.Format.PresentationFormat, &r.Format.ContentMode,
		&r.Status, &r.Visibility, &r.ContentWarning, &r.ContentWarningSpoiler,
		&r.AgeRating, &r.AgeGate, &r.OriginType, &r.Fandom,
		&r.Extras.Language, &r.Extras.ChapterUnit,
		&r.Extras.AuthorNoteStart, &r.Extras.AuthorNoteEnd,
		&r.Extras.SeriesName, &r.Extras.SeriesPosition,
		&r.Extras.CommentAccess, &r.Extras.CommentApproval,
		&r.Extras.Rights.AllowScreenshot, &r.Extras.Rights.AllowTranslation,
		&r.Extras.Rights.AllowDerivative, &r.Extras.Rights.AllowAudio,
		&r.Extras.Rights.RequireCredit, &r.Extras.Rights.DerivativeTerms,
		&r.ViewCount, &r.LikeCount, &r.BookmarkCount,
		&r.HideCounts, &r.ShowDonate, &r.ThemeColor, &r.PublishAt,
		&r.PublishedAt, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt,
		&r.Author.ID, &r.Author.Username, &r.Author.DisplayName, &r.Author.AvatarURL, &r.Author.DonationURL,
		&r.PenNameID, &r.PenName,
		&r.PublishedChapters, &r.TotalChapters, &r.HasMixedFormats,
		&r.HasReaderVariables, &r.FirstChapterSlug,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan novel record: %w", err)
	}
	return &r, nil
}

// CreateParams carries an already-validated fiction.
type CreateParams struct {
	AuthorID uuid.UUID
	Title    string
	// PublicID is the fiction's short permanent handle. It is also the tail of
	// Slug, but it is stored on its own so a rename cannot take it away.
	PublicID       string
	Slug           string
	Description    *string
	Tagline        *string
	Foreword       *string
	CoverURL       *string
	ContentWarning *string
	Format         fiction.Format
	Status         Status
	Visibility     Visibility

	// Creation fields (Phase 13A). AgeRating is required by the service; the
	// others carry their documented defaults when the request omits them.
	AgeRating  AgeRating
	AgeGate    AgeGate
	OriginType OriginType
	Fandom     *string

	// ตั้งค่าเพิ่มเติม (§13K). Always a complete state - the service resolves
	// it against the defaults before it gets here.
	Extras Extras

	// 13U display choices, settable from birth.
	ContentWarningSpoiler bool
	HideCounts            bool
	ShowDonate            bool
	ThemeColor            *string
}

// Create inserts one fiction.
//
// A duplicate slug surfaces as ErrSlugTaken so the service can retry with a
// discriminator. The UNIQUE index is the authority - a pre-flight SELECT would
// still race (docs/09 §34).
func (r *Repository) Create(ctx context.Context, params CreateParams) (*Novel, error) {
	// The publication flag is a separate BOOLEAN parameter rather than a
	// re-use of the status and visibility placeholders. Binding one parameter
	// both as a column value and inside a comparison leaves PostgreSQL unable to
	// deduce a single type for it (SQLSTATE 42P08).
	query := `
		INSERT INTO novels (
			author_id, title, slug, description, cover_url, content_warning,
			story_structure, presentation_format, content_mode,
			status, visibility,
			age_rating, age_gate, origin_type, fandom,
			language, chapter_unit, author_note_start, author_note_end,
			series_name, series_position, comment_access, comment_approval,
			allow_screenshot, allow_translation, allow_derivative,
			allow_audio, require_credit, derivative_terms,
			tagline, foreword,
			published_at,
			content_warning_spoiler, hide_counts, show_donate, theme_color,
			public_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
			$16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29,
			$30, $31,
			CASE WHEN $32 THEN now() ELSE NULL END,
			$33, $34, $35, $36, $37)
		RETURNING ` + novelColumnsBare

	novel, err := scanNovel(r.db.QueryRowContext(ctx, query,
		params.AuthorID, params.Title, params.Slug, params.Description,
		params.CoverURL, params.ContentWarning,
		params.Format.StoryStructure, params.Format.PresentationFormat, params.Format.ContentMode,
		params.Status, params.Visibility,
		params.AgeRating, params.AgeGate, params.OriginType, params.Fandom,
		params.Extras.Language, params.Extras.ChapterUnit,
		params.Extras.AuthorNoteStart, params.Extras.AuthorNoteEnd,
		params.Extras.SeriesName, params.Extras.SeriesPosition,
		params.Extras.CommentAccess, params.Extras.CommentApproval,
		params.Extras.Rights.AllowScreenshot, params.Extras.Rights.AllowTranslation,
		params.Extras.Rights.AllowDerivative, params.Extras.Rights.AllowAudio,
		params.Extras.Rights.RequireCredit, params.Extras.Rights.DerivativeTerms,
		params.Tagline, params.Foreword,
		exposed(params.Status, params.Visibility),
		params.ContentWarningSpoiler, params.HideCounts, params.ShowDonate,
		params.ThemeColor, params.PublicID,
	))
	if err != nil {
		if isSlugConflict(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	return novel, nil
}

// Find loads a fiction by reference WITHOUT joins.
//
// This is the ownership lookup: the service calls it before deciding whether the
// caller may act, so it deliberately applies no visibility filter of its own.
// Soft-deleted rows are excluded - a deleted fiction is gone for everyone,
// including its author.
func (r *Repository) Find(ctx context.Context, ref Ref) (*Novel, error) {
	query := `SELECT ` + novelColumnsBare +
		` FROM novels WHERE deleted_at IS NULL AND `

	var novel *Novel
	var err error
	if ref.BySlug() {
		novel, err = scanNovel(r.db.QueryRowContext(ctx, query+`slug = $1`, ref.Slug))
	} else {
		novel, err = scanNovel(r.db.QueryRowContext(ctx, query+`id = $1`, ref.ID))
	}
	if err != nil {
		return nil, err
	}
	if novel.CollaboratorIDs, err = r.collaboratorIDs(ctx, novel.ID); err != nil {
		return nil, err
	}
	return novel, nil
}

// FindRecord loads a fiction with its author and chapter counts.
func (r *Repository) FindRecord(ctx context.Context, ref Ref) (*Record, error) {
	query := `SELECT ` + recordColumns + recordFrom + ` WHERE n.deleted_at IS NULL AND `

	var record *Record
	var err error
	if ref.BySlug() {
		record, err = scanRecord(r.db.QueryRowContext(ctx, query+`n.slug = $1`, ref.Slug))
	} else {
		record, err = scanRecord(r.db.QueryRowContext(ctx, query+`n.id = $1`, ref.ID))
	}
	if err != nil {
		return nil, err
	}
	if record.CollaboratorIDs, err = r.collaboratorIDs(ctx, record.ID); err != nil {
		return nil, err
	}
	return record, nil
}

// collaboratorIDs loads who may co-write one fiction (13U). It rides along with
// Find/FindRecord so every authorization decision sees the same answer.
func (r *Repository) collaboratorIDs(ctx context.Context, novelID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id FROM novel_collaborators WHERE novel_id = $1`, novelID)
	if err != nil {
		return nil, fmt.Errorf("load collaborators: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan collaborator: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load collaborators: %w", err)
	}
	return ids, nil
}

// CollaboratorCredits is the public co-writer credit for one fiction (13U):
// public profile fields and the credit wording, never an email.
func (r *Repository) CollaboratorCredits(ctx context.Context, novelID uuid.UUID) ([]CollaboratorCredit, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT u.username, NULLIF(p.display_name, ''), NULLIF(p.avatar_url, ''), nc.credit
		FROM novel_collaborators nc
		JOIN users u ON u.id = nc.user_id
		LEFT JOIN user_profiles p ON p.user_id = nc.user_id
		WHERE nc.novel_id = $1
		ORDER BY nc.created_at`, novelID)
	if err != nil {
		return nil, fmt.Errorf("list collaborator credits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	credits := []CollaboratorCredit{}
	for rows.Next() {
		var credit CollaboratorCredit
		if err := rows.Scan(&credit.Username, &credit.DisplayName,
			&credit.AvatarURL, &credit.Credit); err != nil {
			return nil, fmt.Errorf("scan collaborator credit: %w", err)
		}
		credits = append(credits, credit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list collaborator credits: %w", err)
	}
	return credits, nil
}

// AddCollaboratorByUsername attaches a co-writer, resolving the username inside
// the statement so there is no separate lookup to race. ErrCollaboratorUser
// when no such account exists.
func (r *Repository) AddCollaboratorByUsername(
	ctx context.Context, novelID uuid.UUID, username, credit string,
) error {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO novel_collaborators (novel_id, user_id, credit)
		SELECT $1, u.id, $3 FROM users u
		WHERE lower(u.username) = lower($2) AND u.deleted_at IS NULL
		ON CONFLICT (novel_id, user_id) DO UPDATE SET credit = EXCLUDED.credit`,
		novelID, username, credit)
	if err != nil {
		return fmt.Errorf("add collaborator: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("add collaborator: %w", err)
	}
	if affected == 0 {
		return ErrCollaboratorUser
	}
	return nil
}

// RemoveCollaboratorByUsername detaches a co-writer. Removing someone who was
// not on the list is not an error - the end state is what was asked for.
func (r *Repository) RemoveCollaboratorByUsername(
	ctx context.Context, novelID uuid.UUID, username string,
) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM novel_collaborators nc
		USING users u
		WHERE nc.novel_id = $1 AND nc.user_id = u.id AND lower(u.username) = lower($2)`,
		novelID, username)
	if err != nil {
		return fmt.Errorf("remove collaborator: %w", err)
	}
	return nil
}

// RecordsByIDs loads full records for a set of ids, keyed by id.
//
// It exists for domains that page over their OWN table (bookmarks, reading
// progress) and then need the fiction cards for one page of ids: reusing this
// scan keeps `novels` the single source of truth for what a card contains, so a
// new column cannot appear in listings but go missing from the library.
//
// No visibility filter is applied here - the caller decides readability per
// record, exactly as the service layer does everywhere else.
func (r *Repository) RecordsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Record, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]Record{}, nil
	}

	args := &argList{}
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, args.add(id))
	}

	query := `SELECT ` + recordColumns + recordFrom +
		` WHERE n.id IN (` + strings.Join(placeholders, ", ") + `)`

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, fmt.Errorf("load novel records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	records := make(map[uuid.UUID]Record, len(ids))
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records[record.ID] = *record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load novel records: %w", err)
	}
	return records, nil
}

// Filter is a validated listing query (docs/09 §10, §11).
type Filter struct {
	// Query is a title substring search. With SearchAll it widens to the
	// documented search scope instead.
	Query string

	// SearchAll widens Query to the docs/01 §7 search scope: title,
	// description, author, and genre/tag names. Set only by the search
	// endpoint (docs/09 §22) - plain listings keep the cheaper title match.
	SearchAll bool

	// GenreSlug and TagSlug narrow to fictions carrying the term
	// (docs/09 §11: ?genre=fantasy, ?tag=romance - one of each, by slug).
	// An unknown slug matches nothing and yields an empty page, exactly like
	// an unknown author.
	GenreSlug string
	TagSlug   string

	// GenreSlugs / TagSlugs are the multi-term forms (search review 2026-08
	// section B): every named term must be present - AND, because a reader
	// stacking filters is narrowing, not widening. ExcludeTagSlugs refuses
	// works carrying ANY of the named tags - the "ไม่เอา" half of tag
	// filtering, which fic readers use more than the "ต้องมี" half.
	GenreSlugs      []string
	TagSlugs        []string
	ExcludeTagSlugs []string

	// Rating narrows to one age rating exactly. It NARROWS within the §13B
	// exclusion, never past it: asking for `mature` without the 18+ opt-in
	// simply matches nothing.
	Rating string

	// Origin is the reader's ประเภทงาน filter: "original", "fanfiction" (any),
	// "crossover" (a fandom joined with " × ", per docs/FANDOM.md the label is
	// DERIVED from the field, never stored), or "single" (fanfiction of one
	// fandom only).
	Origin string

	// Fandom is a substring match on the free-text fandom field. Free text in,
	// free text matched - docs/FANDOM.md keeps the platform out of the
	// vocabulary business, so there is no canonical form to equal against.
	Fandom string

	// Character is a substring match on the fiction's cast names, so "จงหลี่
	// ทุกเรื่อง" is answerable without the reader guessing which tag spelling
	// the writer used.
	Character string

	// ExcludeWarnings refuses works whose content warning mentions ANY of the
	// given words. The warning is the writer's own free text, so this is a
	// substring test per word - the reader filtering out what they cannot
	// read is the point of the field existing (search review section B).
	ExcludeWarnings []string

	// MinChapters / MaxChapters bound the LIVE chapter count. Zero means
	// unbounded on that side.
	MinChapters int
	MaxChapters int

	// UpdatedWithinDays keeps only works updated in the last N days. Zero is
	// off.
	UpdatedWithinDays int

	// HasVariables keeps only works with reader variables (y/n).
	HasVariables bool

	// Format filters. An empty value means "do not filter on this dimension";
	// each is independent, exactly like the columns (docs/09 §11).
	StoryStructure     string
	PresentationFormat string
	ContentMode        string

	Status string

	// AuthorID scopes the listing to one writer.
	AuthorID uuid.UUID

	// CoWriterID scopes the listing to fictions this user CO-WRITES (13U).
	// Like IncludeUnpublished it is set by the SERVICE from the authenticated
	// identity only - a request cannot ask for someone else's shelf.
	CoWriterID uuid.UUID

	// Viewer is the authenticated caller, or uuid.Nil for a guest. It decides
	// the two visibility rungs that depend on who is asking (§13C) and nothing
	// else; it is set by the SERVICE from the request identity, never from a
	// parameter, so a request cannot claim to be someone.
	Viewer uuid.UUID

	// IncludeMature lifts the default exclusion of 18+ work from listings,
	// search, and recommendations (§13B). Like IncludeUnpublished it is set by
	// the SERVICE and never taken from a query parameter directly - a guest can
	// never reach it, whatever they ask for. Explicit work stays out regardless:
	// there is no switch that puts it in a browse surface.
	IncludeMature bool

	// IncludeUnpublished lifts the public visibility filter. The service sets it
	// ONLY when the listing is scoped to the authenticated user's own work, so
	// it can never widen a guest's view.
	IncludeUnpublished bool

	Sort string
}

// sortClauses is an allowlist. docs/09 §10: never inject a user-provided sort
// value into SQL. The map key is the public name; the value is a fixed fragment
// that no request can influence.
//
// "popular" ranks by bookmark count, computed live from the bookmarks table -
// which docs/08 §28.1 names as the source of truth. When the statistics phase
// lands novel_stats, only this fragment changes; the API value is stable.
var sortClauses = map[string]string{
	"latest":  "n.published_at DESC NULLS LAST, n.created_at DESC",
	"updated": "n.updated_at DESC",
	"title":   "n.title ASC",
	"created": "n.created_at DESC",
	"popular": "(SELECT count(*) FROM bookmarks b WHERE b.novel_id = n.id) DESC, " +
		"n.published_at DESC NULLS LAST",
	// "shelved" ranks by how many reader shelves hold the work - a slower,
	// more deliberate signal than a bookmark (search review 2026-08: never
	// rank by raw view count alone).
	"shelved": "(SELECT count(*) FROM shelf_items si WHERE si.novel_id = n.id) DESC, " +
		"n.published_at DESC NULLS LAST",
	// "relevance" is replaced with a query-aware fragment in List when a text
	// query is present; without one it degrades to this, the default order.
	"relevance": "n.published_at DESC NULLS LAST, n.created_at DESC",
}

// DefaultSort orders by most recently published.
const DefaultSort = "latest"

// SortOptions returns the allowlisted sort values.
func SortOptions() []string {
	return []string{"latest", "updated", "title", "created", "popular", "shelved", "relevance"}
}

// ValidSort reports whether a sort value is allowlisted.
func ValidSort(sort string) bool { _, ok := sortClauses[sort]; return ok }

// argList builds a positional-parameter list, so every value is bound rather
// than interpolated (docs/11 §15).
type argList struct {
	args []any
}

func (a *argList) add(value any) string {
	a.args = append(a.args, value)
	return "$" + strconv.Itoa(len(a.args))
}

// where builds the shared predicate for List and Count.
func (f Filter) where(args *argList) string {
	clauses := []string{"n.deleted_at IS NULL"}

	// IncludeUnpublished requires a specific author. Without that guard, a bug
	// that set the flag without the ID would widen a guest's listing to every
	// draft on the platform; here it degrades to the public predicate instead.
	if f.IncludeUnpublished && f.CoWriterID != uuid.Nil {
		// The co-writer's shelf (13U): fictions this user collaborates on,
		// every status and visibility - a co-written private draft is exactly
		// what they need to reach. The service guarantees CoWriterID is the
		// authenticated user before setting this.
		clauses = append(clauses,
			"EXISTS (SELECT 1 FROM novel_collaborators nc WHERE nc.novel_id = n.id AND nc.user_id = "+
				args.add(f.CoWriterID)+")")
	} else if f.IncludeUnpublished && f.AuthorID != uuid.Nil {
		// The writer's own shelf: every status and visibility. The service
		// guarantees AuthorID is the authenticated user before setting this.
		clauses = append(clauses, "n.author_id = "+args.add(f.AuthorID))
	} else {
		// Everyone else sees only listed work. docs/11 §31: a draft must never
		// appear in a public API response, and unlisted work is reachable by
		// link but must not be discoverable. Members-only work IS listed, but
		// only to a viewer who could open it - offering a card that leads to a
		// closed door is worse than not offering it (§13C).
		clauses = append(clauses, listedSQL(viewerArg(args, f.Viewer)), "n.status <> 'draft'")
		if f.AuthorID != uuid.Nil {
			clauses = append(clauses, "n.author_id = "+args.add(f.AuthorID))
		}

		// A fiction with nothing to read does not appear in a browse surface
		// (home review, 2026-08): a card that says "0 ตอน" leads to an empty
		// book, and a front page of them reads as a broken site. This filters
		// DISCOVERY only - the fiction stays reachable by direct link, and the
		// writer's own shelf (the branch above) lists it regardless. An index
		// scan on chapters_novel_status_idx, same as the card's own count.
		clauses = append(clauses, `EXISTS (
			SELECT 1 FROM chapters c WHERE c.novel_id = n.id AND `+LiveChapterSQL+`)`)

		// 18+ work is excluded from every browse surface by default (§13B).
		// It stays reachable by direct link - this hides it from discovery,
		// which is a different thing from gating it, and the gate itself is
		// 13B's job.
		//
		// A signed-in reader may turn the exclusion off for `mature`. Explicit
		// work is never listed either way: it is reachable by link and from the
		// author's own page, and there is no setting that makes it something a
		// reader can stumble into.
		if f.IncludeMature {
			clauses = append(clauses, "n.age_rating <> "+args.add(string(RatingExplicit)))
		} else {
			clauses = append(clauses, "n.age_rating NOT IN ("+
				args.add(string(RatingMature))+", "+args.add(string(RatingExplicit))+")")
		}
	}

	if f.Query != "" {
		pattern := args.add("%" + escapeLike(f.Query) + "%")
		if f.SearchAll {
			// The documented search scope (docs/01 §7): title, author,
			// description, and the names of assigned genres and tags - plus the
			// tagline (it is what a card shows) and the fandom field, which
			// docs/FANDOM.md names as feeding search. One bound pattern, reused
			// by position.
			clauses = append(clauses, `(
				n.title ILIKE `+pattern+`
				OR n.description ILIKE `+pattern+`
				OR n.tagline ILIKE `+pattern+`
				OR n.fandom ILIKE `+pattern+`
				OR u.username ILIKE `+pattern+`
				OR p.display_name ILIKE `+pattern+`
				OR EXISTS (
					SELECT 1 FROM novel_genres ng JOIN genres g ON g.id = ng.genre_id
					WHERE ng.novel_id = n.id AND g.name ILIKE `+pattern+`
				)
				OR EXISTS (
					SELECT 1 FROM novel_tags nt JOIN tags t ON t.id = nt.tag_id
					WHERE nt.novel_id = n.id AND t.name ILIKE `+pattern+`
				)
			)`)
		} else {
			clauses = append(clauses, "n.title ILIKE "+pattern)
		}
	}

	// Term filters (docs/09 §11). EXISTS over the assignment tables rides
	// novel_genres_genre_idx / novel_tags_tag_idx.
	if f.GenreSlug != "" {
		clauses = append(clauses, `EXISTS (
			SELECT 1 FROM novel_genres ng JOIN genres g ON g.id = ng.genre_id
			WHERE ng.novel_id = n.id AND g.slug = `+args.add(f.GenreSlug)+`
		)`)
	}
	if f.TagSlug != "" {
		clauses = append(clauses, `EXISTS (
			SELECT 1 FROM novel_tags nt JOIN tags t ON t.id = nt.tag_id
			WHERE nt.novel_id = n.id AND t.slug = `+args.add(f.TagSlug)+`
		)`)
	}
	// The multi-term forms (search review 2026-08). Include is AND - stacking
	// filters narrows; exclude is NOT EXISTS per slug.
	for _, slug := range f.GenreSlugs {
		clauses = append(clauses, `EXISTS (
			SELECT 1 FROM novel_genres ng JOIN genres g ON g.id = ng.genre_id
			WHERE ng.novel_id = n.id AND g.slug = `+args.add(slug)+`
		)`)
	}
	for _, slug := range f.TagSlugs {
		clauses = append(clauses, `EXISTS (
			SELECT 1 FROM novel_tags nt JOIN tags t ON t.id = nt.tag_id
			WHERE nt.novel_id = n.id AND t.slug = `+args.add(slug)+`
		)`)
	}
	for _, slug := range f.ExcludeTagSlugs {
		clauses = append(clauses, `NOT EXISTS (
			SELECT 1 FROM novel_tags nt JOIN tags t ON t.id = nt.tag_id
			WHERE nt.novel_id = n.id AND t.slug = `+args.add(slug)+`
		)`)
	}

	if f.Rating != "" {
		clauses = append(clauses, "n.age_rating = "+args.add(f.Rating))
	}

	// ประเภทงาน. "crossover" and "single" are DERIVED from the fandom field
	// per docs/FANDOM.md: names joined with " × " are a crossover, and that
	// convention is the only place the distinction lives.
	switch f.Origin {
	case "original", "fanfiction":
		clauses = append(clauses, "n.origin_type = "+args.add(f.Origin))
	case "crossover":
		clauses = append(clauses, "n.origin_type = 'fanfiction'",
			"n.fandom LIKE "+args.add("% × %"))
	case "single":
		clauses = append(clauses, "n.origin_type = 'fanfiction'",
			"(n.fandom IS NULL OR n.fandom NOT LIKE "+args.add("% × %")+")")
	}

	if f.Fandom != "" {
		clauses = append(clauses,
			"n.fandom ILIKE "+args.add("%"+escapeLike(f.Fandom)+"%"))
	}
	if f.Character != "" {
		clauses = append(clauses, `EXISTS (
			SELECT 1 FROM characters ch
			WHERE ch.novel_id = n.id AND ch.name ILIKE `+
			args.add("%"+escapeLike(f.Character)+"%")+`
		)`)
	}
	for _, word := range f.ExcludeWarnings {
		clauses = append(clauses,
			"(n.content_warning IS NULL OR n.content_warning NOT ILIKE "+
				args.add("%"+escapeLike(word)+"%")+")")
	}

	// Length bounds run against the LIVE count - the same subquery the card's
	// own chapter_count uses, so the filter and the number can never disagree.
	liveCount := `(SELECT count(*) FROM chapters c WHERE c.novel_id = n.id AND ` +
		LiveChapterSQL + `)`
	if f.MinChapters > 0 {
		clauses = append(clauses, liveCount+" >= "+args.add(f.MinChapters))
	}
	if f.MaxChapters > 0 {
		clauses = append(clauses, liveCount+" <= "+args.add(f.MaxChapters))
	}

	if f.UpdatedWithinDays > 0 {
		clauses = append(clauses,
			"n.updated_at >= now() - ("+args.add(f.UpdatedWithinDays)+" * interval '1 day')")
	}
	if f.HasVariables {
		clauses = append(clauses,
			"EXISTS (SELECT 1 FROM novel_variables v WHERE v.novel_id = n.id)")
	}

	if f.StoryStructure != "" {
		clauses = append(clauses, "n.story_structure = "+args.add(f.StoryStructure))
	}
	if f.PresentationFormat != "" {
		clauses = append(clauses, "n.presentation_format = "+args.add(f.PresentationFormat))
	}
	if f.ContentMode != "" {
		clauses = append(clauses, "n.content_mode = "+args.add(f.ContentMode))
	}
	if f.Status != "" {
		clauses = append(clauses, "n.status = "+args.add(f.Status))
	}

	return " WHERE " + strings.Join(clauses, " AND ")
}

// escapeLike neutralises the wildcards a searcher could otherwise inject.
//
// Without this, a query of "%" matches every fiction and turns a bounded search
// into a full scan.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// List returns one page of fictions together with the total match count.
func (r *Repository) List(
	ctx context.Context, filter Filter, page pagination.Params,
) ([]Record, int64, error) {
	sort := filter.Sort
	if !ValidSort(sort) {
		sort = DefaultSort
	}

	countArgs := &argList{}
	countQuery := `SELECT count(*)` + recordFrom + filter.where(countArgs)

	var total int64
	if err := r.db.QueryRowContext(ctx, countQuery, countArgs.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count novels: %w", err)
	}
	if total == 0 {
		return []Record{}, 0, nil
	}

	args := &argList{}
	where := filter.where(args)

	orderBy := sortClauses[sort]
	if sort == "relevance" && filter.Query != "" {
		// Relevance tiering (search review 2026-08): a title hit outranks a
		// match buried in the description or a tag, then pg_trgm similarity
		// orders within a tier - trigram, not FTS, because FTS cannot segment
		// Thai. The args land AFTER the predicate's, so positions stay stable.
		pattern := args.add("%" + escapeLike(filter.Query) + "%")
		raw := args.add(filter.Query)
		orderBy = "(n.title ILIKE " + pattern + ") DESC, " +
			"similarity(n.title, " + raw + ") DESC, n.updated_at DESC"
	}

	query := `SELECT ` + recordColumns + recordFrom + where +
		// n.id is the tiebreaker: without it, rows sharing a sort value could
		// swap between pages and a reader would see one twice or not at all.
		` ORDER BY ` + orderBy + `, n.id DESC` +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list novels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	records := []Record{}
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list novels: %w", err)
	}
	return records, total, nil
}

// UpdateParams carries a validated partial update.
//
// Every field is a pointer: nil means "leave this column alone". The double
// pointers let a caller distinguish "not mentioned" from "explicitly cleared to
// NULL", which a single pointer cannot express.
type UpdateParams struct {
	Title       *string
	Description **string
	// Tagline and Foreword (13S). คำโปรย is the line under a cover; บทนำ is
	// what the author says before the story. Both are the author's words and
	// both are clearable, so both are double pointers.
	Tagline        **string
	Foreword       **string
	CoverURL       **string
	ContentWarning **string
	Status         *Status
	Visibility     *Visibility

	// y/n (docs/PHASE-12-STORY-DEPTH.md §12B). The pair is always written
	// together - the CHECK constraint requires a token whenever the switch is
	// on, so the service resolves both before calling and never sends a half
	// state that the database would have to reject.

	// Creation fields (Phase 13A). Fandom is a double pointer because clearing
	// it is a real edit - a work that stops being fanfiction has no source to
	// name, and the CHECK constraint requires the pair to stay coherent, so the
	// service resolves both together and never sends a half state.
	AgeRating  *AgeRating
	AgeGate    *AgeGate
	OriginType *OriginType
	Fandom     **string

	// Extras is written as a COMPLETE state or not at all (§13K). The service
	// resolves it against the row it read, so the update never has to express
	// thirteen independent presences - and series_name/series_position, which
	// a CHECK ties together, can never disagree.
	Extras *Extras

	// 13U display choices. ThemeColor is clearable, so it is a double pointer.
	ContentWarningSpoiler *bool
	HideCounts            *bool
	ShowDonate            *bool
	ThemeColor            **string

	// PublishAt schedules or clears the first publish (13U). A double pointer:
	// nil leaves it, a pointer to nil clears it, a pointer to a time sets it.
	PublishAt **time.Time

	// PenNameID is which of the author's identities this work is published
	// under (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2). A double pointer: nil
	// leaves it, a pointer to nil returns the work to the author's default, a
	// pointer to an id names one.
	//
	// This is a METADATA change and nothing else, by the same rule a format
	// change follows: it writes one column on `novels` and cannot reach
	// `chapters`, `chapter_messages`, or `chapter_revisions` from here.
	PenNameID **uuid.UUID

	// Exposed is whether the RESULTING fiction is reachable by someone other
	// than its owner. The service computes it from the status and visibility the
	// row will hold after this update, which is information the SQL cannot
	// recover for itself.
	Exposed bool

	// ExposedAt is the moment exposure counts from when Exposed stamps
	// published_at: the scheduled publish time when there is one, else nil for
	// now(). A scheduled fiction sorted by "latest" should rank from when
	// readers could first open it, not from when its author pressed the button.
	ExposedAt *time.Time
}

// exposed reports whether a status/visibility pair makes work reachable by
// someone other than its owner.
func exposed(status Status, visibility Visibility) bool {
	return !status.IsDraft() && visibility != VisibilityPrivate
}

// Update applies a partial update and returns the resulting row.
//
// It deliberately cannot change author_id or the format columns. Ownership
// transfer is not a feature, and the format has its own endpoint so that a
// format change is separately validated and auditable (docs/09 §15).
func (r *Repository) Update(ctx context.Context, id uuid.UUID, params UpdateParams) (*Novel, error) {
	args := &argList{}
	sets := []string{"updated_at = now()"}

	if params.Title != nil {
		sets = append(sets, "title = "+args.add(*params.Title))
	}
	if params.Description != nil {
		sets = append(sets, "description = "+args.add(*params.Description))
	}
	if params.Tagline != nil {
		sets = append(sets, "tagline = "+args.add(*params.Tagline))
	}
	if params.Foreword != nil {
		sets = append(sets, "foreword = "+args.add(*params.Foreword))
	}
	if params.CoverURL != nil {
		sets = append(sets, "cover_url = "+args.add(*params.CoverURL))
	}
	if params.ContentWarning != nil {
		sets = append(sets, "content_warning = "+args.add(*params.ContentWarning))
	}

	if params.AgeRating != nil {
		sets = append(sets, "age_rating = "+args.add(*params.AgeRating))
	}
	if params.AgeGate != nil {
		sets = append(sets, "age_gate = "+args.add(*params.AgeGate))
	}
	if params.OriginType != nil {
		sets = append(sets, "origin_type = "+args.add(*params.OriginType))
	}
	if params.Fandom != nil {
		sets = append(sets, "fandom = "+args.add(*params.Fandom))
	}
	if params.Extras != nil {
		extras := *params.Extras
		sets = append(sets,
			"language = "+args.add(extras.Language),
			"chapter_unit = "+args.add(extras.ChapterUnit),
			"author_note_start = "+args.add(extras.AuthorNoteStart),
			"author_note_end = "+args.add(extras.AuthorNoteEnd),
			"series_name = "+args.add(extras.SeriesName),
			"series_position = "+args.add(extras.SeriesPosition),
			"comment_access = "+args.add(extras.CommentAccess),
			"comment_approval = "+args.add(extras.CommentApproval),
			"allow_screenshot = "+args.add(extras.Rights.AllowScreenshot),
			"allow_translation = "+args.add(extras.Rights.AllowTranslation),
			"allow_derivative = "+args.add(extras.Rights.AllowDerivative),
			"allow_audio = "+args.add(extras.Rights.AllowAudio),
			"require_credit = "+args.add(extras.Rights.RequireCredit),
			"derivative_terms = "+args.add(extras.Rights.DerivativeTerms),
		)
	}

	if params.Status != nil {
		sets = append(sets, "status = "+args.add(*params.Status))
	}
	if params.Visibility != nil {
		sets = append(sets, "visibility = "+args.add(*params.Visibility))
	}

	// Stamp the first publication. COALESCE keeps the original date when work is
	// unpublished and published again, so a reader's "published on" does not
	// jump forward because the author toggled a setting.
	//
	// Whether the fiction ends up exposed is decided by the service and passed
	// as a BOOLEAN. It cannot be derived from the columns here: every SET
	// expression in one UPDATE sees the OLD row, so `status` would be the value
	// being replaced rather than the incoming one.
	if params.ContentWarningSpoiler != nil {
		sets = append(sets, "content_warning_spoiler = "+args.add(*params.ContentWarningSpoiler))
	}
	if params.HideCounts != nil {
		sets = append(sets, "hide_counts = "+args.add(*params.HideCounts))
	}
	if params.ShowDonate != nil {
		sets = append(sets, "show_donate = "+args.add(*params.ShowDonate))
	}
	if params.ThemeColor != nil {
		sets = append(sets, "theme_color = "+args.add(*params.ThemeColor))
	}
	if params.PublishAt != nil {
		sets = append(sets, "publish_at = "+args.add(*params.PublishAt))
	}
	if params.PenNameID != nil {
		sets = append(sets, "pen_name_id = "+args.add(*params.PenNameID))
	}

	sets = append(sets, `published_at = CASE WHEN `+args.add(params.Exposed)+
		` THEN COALESCE(published_at, COALESCE(`+args.add(params.ExposedAt)+
		`::timestamptz, now())) ELSE published_at END`)

	query := `UPDATE novels SET ` + strings.Join(sets, ", ") +
		` WHERE id = ` + args.add(id) + ` AND deleted_at IS NULL` +
		` RETURNING ` + novelColumnsBare

	return scanNovel(r.db.QueryRowContext(ctx, query, args.args...))
}

// PenNameBelongsTo reports whether a pen name is one of this author's own
// identities (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2).
//
// Read here rather than through the pennames service for the same reason the
// author's display name is joined in rather than fetched: it is one indexed
// lookup on a row this query already knows the owner of. What matters is that
// the USER ID is part of the predicate - a pen name belonging to anyone else
// simply does not match, so no request can publish a work under a stranger's
// name.
func (r *Repository) PenNameBelongsTo(
	ctx context.Context, userID, penNameID uuid.UUID,
) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pen_names WHERE id = $1 AND user_id = $2
		)`, penNameID, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check pen name ownership: %w", err)
	}
	return exists, nil
}

// UpdateFormat writes the three format columns and nothing else.
//
// This is the whole of a format change (docs/08 §3.1, docs/09 §14.6): one
// statement, so it is atomic by construction, and it touches no chapter,
// message, or revision row. The WHERE clause carries the expected current
// format, which makes the write a compare-and-set: two concurrent format
// changes cannot interleave into a state neither writer validated
// (docs/09 §34, docs/15 §5.15).
func (r *Repository) UpdateFormat(
	ctx context.Context, id uuid.UUID, expected, next fiction.Format,
) (*Novel, error) {
	query := `
		UPDATE novels
		SET story_structure = $2, presentation_format = $3, content_mode = $4,
		    updated_at = now()
		WHERE id = $1
		  AND deleted_at IS NULL
		  AND story_structure = $5 AND presentation_format = $6 AND content_mode = $7
		RETURNING ` + novelColumnsBare

	return scanNovel(r.db.QueryRowContext(ctx, query, id,
		next.StoryStructure, next.PresentationFormat, next.ContentMode,
		expected.StoryStructure, expected.PresentationFormat, expected.ContentMode,
	))
}

// SoftDelete marks a fiction deleted.
//
// docs/08 §37 soft-deletes novels so moderation and recovery remain possible.
// Chapters and messages are left untouched: they are unreachable through the
// novel, and hard-deleting an author's manuscript on a single API call is
// exactly the destructive behaviour the platform forbids.
func (r *Repository) SoftDelete(ctx context.Context, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE novels SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("delete novel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete novel: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// FindIncludingDeleted loads a fiction by id even when it is soft-deleted.
//
// It exists ONLY for the moderation path (docs/08 §24): restore has to see
// the removed row, and remove wants to answer "already removed" rather than
// "not found". Every reader and writer path keeps using Find, which excludes
// deleted rows.
func (r *Repository) FindIncludingDeleted(ctx context.Context, id uuid.UUID) (*Novel, error) {
	return scanNovel(r.db.QueryRowContext(ctx,
		`SELECT `+novelColumnsBare+` FROM novels WHERE id = $1`, id))
}

// Restore clears the soft delete - the moderator's counterpart of SoftDelete
// (docs/11 §39 "Moderator restored content"). Nothing else changes: status,
// visibility, and content come back exactly as they were.
func (r *Repository) Restore(ctx context.Context, id uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE novels SET deleted_at = NULL, updated_at = now()
		WHERE id = $1 AND deleted_at IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("restore novel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("restore novel: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// HasChatContent reports whether ANY live chapter of this fiction has chat
// messages prepared.
//
// It answers the warning in docs/08 §11 - "chat formatting is not prepared for
// this chapter" - and nothing else. It never causes a conversion.
func (r *Repository) HasChatContent(ctx context.Context, novelID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM chapter_messages m
			JOIN chapters c ON c.id = m.chapter_id
			WHERE c.novel_id = $1 AND c.deleted_at IS NULL
		)`, novelID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check chat content: %w", err)
	}
	return exists, nil
}

// ReplaceGenres sets a fiction's genre assignments to exactly ids.
//
// The edge rows are fiction metadata, owned and authorized through the fiction
// (docs/08 §14.2) - which is why this lives here and not in taxonomy. One
// transaction so a concurrent reader never observes a half-replaced set
// (docs/09 §33).
func (r *Repository) ReplaceGenres(ctx context.Context, novelID uuid.UUID, ids []uuid.UUID) error {
	return r.replaceAssignments(ctx, "novel_genres", "genre_id", novelID, ids)
}

// ReplaceTags sets a fiction's tag assignments to exactly ids.
func (r *Repository) ReplaceTags(ctx context.Context, novelID uuid.UUID, ids []uuid.UUID) error {
	return r.replaceAssignments(ctx, "novel_tags", "tag_id", novelID, ids)
}

// replaceAssignments swaps the whole edge set for one fiction. table and
// column are package-internal constants, never caller input.
func (r *Repository) replaceAssignments(
	ctx context.Context, table, column string, novelID uuid.UUID, ids []uuid.UUID,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace %s: begin: %w", table, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE novel_id = $1`, novelID); err != nil {
		return fmt.Errorf("replace %s: clear: %w", table, err)
	}

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+table+` (novel_id, `+column+`) VALUES ($1, $2)
			 ON CONFLICT DO NOTHING`, novelID, id); err != nil {
			return fmt.Errorf("replace %s: insert: %w", table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace %s: commit: %w", table, err)
	}
	return nil
}

// TermsForNovels batch-loads the genres and tags for a page of fictions,
// keyed by novel id. One query per vocabulary for the whole page, never one
// per row (docs/07 §67).
func (r *Repository) TermsForNovels(
	ctx context.Context, ids []uuid.UUID,
) (genres, tags map[uuid.UUID][]taxonomy.Term, err error) {
	genres, err = r.termsFor(ctx, ids, `
		SELECT ng.novel_id, g.id, g.name, g.slug
		FROM novel_genres ng JOIN genres g ON g.id = ng.genre_id
		WHERE ng.novel_id IN (%s) ORDER BY g.name ASC`)
	if err != nil {
		return nil, nil, err
	}
	tags, err = r.termsFor(ctx, ids, `
		SELECT nt.novel_id, t.id, t.name, t.slug
		FROM novel_tags nt JOIN tags t ON t.id = nt.tag_id
		WHERE nt.novel_id IN (%s) ORDER BY t.name ASC`)
	if err != nil {
		return nil, nil, err
	}
	return genres, tags, nil
}

func (r *Repository) termsFor(
	ctx context.Context, ids []uuid.UUID, queryTemplate string,
) (map[uuid.UUID][]taxonomy.Term, error) {
	terms := make(map[uuid.UUID][]taxonomy.Term, len(ids))
	if len(ids) == 0 {
		return terms, nil
	}

	args := &argList{}
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = args.add(id)
	}

	rows, err := r.db.QueryContext(ctx,
		fmt.Sprintf(queryTemplate, strings.Join(placeholders, ", ")), args.args...)
	if err != nil {
		return nil, fmt.Errorf("load terms: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var novelID uuid.UUID
		var term taxonomy.Term
		if err := rows.Scan(&novelID, &term.ID, &term.Name, &term.Slug); err != nil {
			return nil, fmt.Errorf("scan term: %w", err)
		}
		terms[novelID] = append(terms[novelID], term)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load terms: %w", err)
	}
	return terms, nil
}

// isSlugConflict reports whether err is the slug UNIQUE violation.
//
// Matched by constraint name rather than by importing pgconn for its SQLSTATE
// type, consistent with users.uniqueViolation.
func isSlugConflict(err error) bool {
	msg := err.Error()
	if !strings.Contains(msg, "SQLSTATE 23505") && !strings.Contains(msg, "duplicate key value") {
		return false
	}
	return strings.Contains(msg, "novels_slug_key")
}
