package moderation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// ErrNotFound covers "no such report". The service translates to HTTP.
var ErrNotFound = errors.New("report not found")

// Repository is the only place that reads or writes the reports and
// moderation_actions tables. It also reads compact target snapshots for the
// staff detail page - reads only; every state CHANGE goes through the owning
// domain's service.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// reportColumns is every storage column plus the joined reporter and resolver
// cards. One query per page (docs/07 §67).
const reportColumns = `
	r.id, r.reporter_id, r.target_type, r.target_id,
	r.reason, r.description, r.status,
	r.created_at, r.resolved_at, r.resolved_by,
	u.id, u.username, p.display_name, p.avatar_url,
	m.id, m.username, mp.display_name, mp.avatar_url`

const reportFrom = `
	FROM reports r
	JOIN users u ON u.id = r.reporter_id
	LEFT JOIN user_profiles p ON p.user_id = r.reporter_id
	LEFT JOIN users m ON m.id = r.resolved_by
	LEFT JOIN user_profiles mp ON mp.user_id = r.resolved_by`

type scanner interface{ Scan(...any) error }

func scanReport(row scanner) (*Report, error) {
	var (
		r        Report
		resolver Card
		resID    uuid.NullUUID
		resName  sql.NullString
	)
	err := row.Scan(
		&r.ID, &r.ReporterID, &r.TargetType, &r.TargetID,
		&r.Reason, &r.Description, &r.Status,
		&r.CreatedAt, &r.ResolvedAt, &r.ResolvedBy,
		&r.Reporter.ID, &r.Reporter.Username, &r.Reporter.DisplayName, &r.Reporter.AvatarURL,
		&resID, &resName, &resolver.DisplayName, &resolver.AvatarURL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan report: %w", err)
	}
	if resID.Valid {
		resolver.ID = resID.UUID
		resolver.Username = resName.String
		r.Resolver = &resolver
	}
	return &r, nil
}

// CreateParams is a validated report insert.
type CreateParams struct {
	ReporterID  uuid.UUID
	TargetType  TargetType
	TargetID    uuid.UUID
	Reason      Reason
	Description *string
}

// Create files a report. The partial unique index is the duplicate guard: if
// the reporter already has an OPEN report on this target, nothing is inserted
// and the existing report is returned with created=false - the same
// idempotent-duplicate shape bookmarks and follows use (docs/09 §34).
func (r *Repository) Create(ctx context.Context, params CreateParams) (*Report, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		WITH inserted AS (
			INSERT INTO reports (reporter_id, target_type, target_id, reason, description)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (reporter_id, target_type, target_id)
				WHERE status IN ('pending', 'reviewing')
				DO NOTHING
			RETURNING *
		)
		SELECT `+reportColumns+`
		FROM inserted r
		JOIN users u ON u.id = r.reporter_id
		LEFT JOIN user_profiles p ON p.user_id = r.reporter_id
		LEFT JOIN users m ON m.id = r.resolved_by
		LEFT JOIN user_profiles mp ON mp.user_id = r.resolved_by`,
		params.ReporterID, params.TargetType, params.TargetID,
		params.Reason, params.Description)

	report, err := scanReport(row)
	if err == nil {
		return report, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, fmt.Errorf("create report: %w", err)
	}

	// Conflict path: return the open report that blocked the insert.
	existing, err := r.findOpen(ctx, params.ReporterID, params.TargetType, params.TargetID)
	if err != nil {
		return nil, false, fmt.Errorf("load existing report: %w", err)
	}
	return existing, false, nil
}

func (r *Repository) findOpen(
	ctx context.Context, reporterID uuid.UUID, targetType TargetType, targetID uuid.UUID,
) (*Report, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+reportColumns+reportFrom+`
		WHERE r.reporter_id = $1 AND r.target_type = $2 AND r.target_id = $3
		  AND r.status IN ('pending', 'reviewing')`,
		reporterID, targetType, targetID)
	return scanReport(row)
}

// Find loads one report by id, whatever its state.
func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*Report, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+reportColumns+reportFrom+` WHERE r.id = $1`, id)
	return scanReport(row)
}

// ListForReporter returns one page of the caller's own reports, newest first -
// their filing history (docs/09 §28).
func (r *Repository) ListForReporter(
	ctx context.Context, reporterID uuid.UUID, page pagination.Params,
) ([]Report, int64, error) {
	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM reports WHERE reporter_id = $1`, reporterID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reports: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT `+reportColumns+reportFrom+`
		WHERE r.reporter_id = $1
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT $2 OFFSET $3`,
		reporterID, page.Limit(), page.Offset())
	if err != nil {
		return nil, 0, fmt.Errorf("list reports: %w", err)
	}
	defer rows.Close()
	return collectReports(rows, total)
}

// AdminFilter narrows the staff queue.
type AdminFilter struct {
	// Status narrows to one lifecycle state; empty lists every state.
	Status Status
	// TargetType narrows to one target type; empty lists every type.
	TargetType TargetType
}

// AdminList returns one page of the moderation queue, OLDEST first - a queue
// is worked in arrival order (docs/02 §38 "Moderator queue").
func (r *Repository) AdminList(
	ctx context.Context, filter AdminFilter, page pagination.Params,
) ([]Report, int64, error) {
	where := "TRUE"
	args := argList{}
	if filter.Status != "" {
		where += " AND r.status = " + args.add(filter.Status)
	}
	if filter.TargetType != "" {
		where += " AND r.target_type = " + args.add(filter.TargetType)
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM reports r WHERE `+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reports: %w", err)
	}

	query := `
		SELECT ` + reportColumns + reportFrom + `
		WHERE ` + where + `
		ORDER BY r.created_at ASC, r.id ASC
		LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list reports: %w", err)
	}
	defer rows.Close()
	return collectReports(rows, total)
}

func collectReports(rows *sql.Rows, total int64) ([]Report, int64, error) {
	reports := []Report{}
	for rows.Next() {
		report, err := scanReport(rows)
		if err != nil {
			return nil, 0, err
		}
		reports = append(reports, *report)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate reports: %w", err)
	}
	return reports, total, nil
}

// Transition moves a report along the lifecycle, guarded in SQL so two
// concurrent moderators cannot both win: the UPDATE applies only when the row
// is still in one of the states the transition is legal FROM. Zero rows means
// "gone or already moved on" - the service disambiguates.
//
// Resolved and rejected stamp resolved_at/resolved_by (docs/08 §24.1);
// reviewing stamps neither because nothing is closed yet.
func (r *Repository) Transition(
	ctx context.Context, id uuid.UUID, to Status, from []Status, actorID uuid.UUID,
) (bool, error) {
	args := argList{}
	idParam := args.add(id)

	set := "status = " + args.add(to)
	if to.Terminal() {
		set += ", resolved_at = now(), resolved_by = " + args.add(actorID)
	}

	fromParams := make([]string, 0, len(from))
	for _, s := range from {
		fromParams = append(fromParams, args.add(s))
	}

	result, err := r.db.ExecContext(ctx,
		`UPDATE reports SET `+set+
			` WHERE id = `+idParam+` AND status IN (`+strings.Join(fromParams, ", ")+`)`,
		args.args...)
	if err != nil {
		return false, fmt.Errorf("transition report: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("transition report: %w", err)
	}
	return affected == 1, nil
}

// ---------------------------------------------------------------------------
// moderation_actions - append-only (docs/08 §24.2)
// ---------------------------------------------------------------------------

const actionColumns = `
	a.id, a.moderator_id, a.target_type, a.target_id,
	a.action, a.reason, a.created_at,
	u.id, u.username, p.display_name, p.avatar_url`

const actionFrom = `
	FROM moderation_actions a
	JOIN users u ON u.id = a.moderator_id
	LEFT JOIN user_profiles p ON p.user_id = a.moderator_id`

func scanAction(row scanner) (*ActionRecord, error) {
	var a ActionRecord
	err := row.Scan(
		&a.ID, &a.ModeratorID, &a.TargetType, &a.TargetID,
		&a.Action, &a.Reason, &a.CreatedAt,
		&a.Moderator.ID, &a.Moderator.Username,
		&a.Moderator.DisplayName, &a.Moderator.AvatarURL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan moderation action: %w", err)
	}
	return &a, nil
}

// ActionParams is a validated audit insert.
type ActionParams struct {
	ModeratorID uuid.UUID
	TargetType  TargetType
	TargetID    uuid.UUID
	Action      Action
	Reason      *string
}

// InsertAction appends one audit row. There is deliberately no update or
// delete counterpart anywhere in this package.
func (r *Repository) InsertAction(ctx context.Context, params ActionParams) (*ActionRecord, error) {
	row := r.db.QueryRowContext(ctx, `
		WITH inserted AS (
			INSERT INTO moderation_actions (moderator_id, target_type, target_id, action, reason)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING *
		)
		SELECT `+actionColumns+`
		FROM inserted a
		JOIN users u ON u.id = a.moderator_id
		LEFT JOIN user_profiles p ON p.user_id = a.moderator_id`,
		params.ModeratorID, params.TargetType, params.TargetID, params.Action, params.Reason)

	action, err := scanAction(row)
	if err != nil {
		return nil, fmt.Errorf("insert moderation action: %w", err)
	}
	return action, nil
}

// ActionFilter narrows the audit listing.
type ActionFilter struct {
	// TargetType with TargetID lists one object's history; both empty lists
	// the global trail.
	TargetType TargetType
	TargetID   uuid.UUID
}

// ListActions returns one page of the audit trail, newest first.
func (r *Repository) ListActions(
	ctx context.Context, filter ActionFilter, page pagination.Params,
) ([]ActionRecord, int64, error) {
	where := "TRUE"
	args := argList{}
	if filter.TargetType != "" {
		where += " AND a.target_type = " + args.add(filter.TargetType)
	}
	if filter.TargetID != uuid.Nil {
		where += " AND a.target_id = " + args.add(filter.TargetID)
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM moderation_actions a WHERE `+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count moderation actions: %w", err)
	}

	query := `
		SELECT ` + actionColumns + actionFrom + `
		WHERE ` + where + `
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())

	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list moderation actions: %w", err)
	}
	defer rows.Close()

	actions := []ActionRecord{}
	for rows.Next() {
		action, err := scanAction(rows)
		if err != nil {
			return nil, 0, err
		}
		actions = append(actions, *action)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate moderation actions: %w", err)
	}
	return actions, total, nil
}

// ---------------------------------------------------------------------------
// Target snapshots - staff-only READS for the report detail page
// ---------------------------------------------------------------------------

// Snapshot describes what a report points at RIGHT NOW. Reads only; it never
// exposes chapters.content or any draft manuscript (docs/11 §39) - long-form
// fiction is judged through the normal reader, short-form content through the
// excerpt that IS the reported material.
func (r *Repository) Snapshot(
	ctx context.Context, targetType TargetType, id uuid.UUID,
) (*TargetSnapshot, error) {
	snap := &TargetSnapshot{Type: targetType, ID: id}

	var query string
	switch targetType {
	case TargetNovel:
		query = `
			SELECT n.title, NULL::text,
			       CASE WHEN n.deleted_at IS NULL THEN 'active' ELSE 'removed' END,
			       u.id, u.username, p.display_name, p.avatar_url
			FROM novels n
			JOIN users u ON u.id = n.author_id
			LEFT JOIN user_profiles p ON p.user_id = n.author_id
			WHERE n.id = $1`
	case TargetChapter:
		query = `
			SELECT COALESCE(c.title, n.title), NULL::text,
			       CASE WHEN c.deleted_at IS NULL THEN 'active' ELSE 'removed' END,
			       u.id, u.username, p.display_name, p.avatar_url
			FROM chapters c
			JOIN novels n ON n.id = c.novel_id
			JOIN users u ON u.id = n.author_id
			LEFT JOIN user_profiles p ON p.user_id = n.author_id
			WHERE c.id = $1`
	case TargetComment:
		query = `
			SELECT NULL::text, left(c.content, 300),
			       CASE WHEN c.deleted_at IS NOT NULL THEN 'deleted' ELSE c.status END,
			       u.id, u.username, p.display_name, p.avatar_url
			FROM comments c
			JOIN users u ON u.id = c.user_id
			LEFT JOIN user_profiles p ON p.user_id = c.user_id
			WHERE c.id = $1`
	case TargetCommunityPost:
		query = `
			SELECT NULL::text, left(c.content, 300),
			       CASE WHEN c.deleted_at IS NOT NULL THEN 'deleted' ELSE c.status END,
			       u.id, u.username, p.display_name, p.avatar_url
			FROM community_posts c
			JOIN users u ON u.id = c.author_id
			LEFT JOIN user_profiles p ON p.user_id = c.author_id
			WHERE c.id = $1`
	case TargetCommunityComment:
		query = `
			SELECT NULL::text, left(c.content, 300),
			       CASE WHEN c.deleted_at IS NOT NULL THEN 'deleted' ELSE c.status END,
			       u.id, u.username, p.display_name, p.avatar_url
			FROM community_comments c
			JOIN users u ON u.id = c.author_id
			LEFT JOIN user_profiles p ON p.user_id = c.author_id
			WHERE c.id = $1`
	case TargetUser:
		query = `
			SELECT NULL::text, NULL::text,
			       CASE WHEN u.deleted_at IS NOT NULL THEN 'deleted' ELSE u.status END,
			       u.id, u.username, p.display_name, p.avatar_url
			FROM users u
			LEFT JOIN user_profiles p ON p.user_id = u.id
			WHERE u.id = $1`
	case TargetMedia:
		// The moderator judges the file through its public URL; the snapshot
		// carries the metadata (title = the uploader's original filename).
		query = `
			SELECT m.original_filename, m.media_type || ' · ' || m.mime_type,
			       CASE WHEN m.deleted_at IS NULL THEN 'active' ELSE 'removed' END,
			       u.id, u.username, p.display_name, p.avatar_url
			FROM media m
			JOIN users u ON u.id = m.owner_id
			LEFT JOIN user_profiles p ON p.user_id = m.owner_id
			WHERE m.id = $1`
	default:
		return snap, nil
	}

	var author Card
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&snap.Title, &snap.Excerpt, &snap.State,
		&author.ID, &author.Username, &author.DisplayName, &author.AvatarURL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Hard-deleted since the report was filed. The report keeps its row;
		// the snapshot says the object is gone.
		return snap, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", targetType, err)
	}
	snap.Exists = true
	snap.Author = &author
	return snap, nil
}

// argList builds a positional-parameter list, so every value is bound rather
// than interpolated (docs/11 §15).
type argList struct {
	args []any
}

func (a *argList) add(value any) string {
	a.args = append(a.args, value)
	return "$" + strconv.Itoa(len(a.args))
}
