package library

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// The library redesign's reader-side state (library review 2026-08): removing
// a stalled progress row, the per-follow notification switch, the private
// "finished" mark with its star and note, and reading history with the
// privacy controls README demands. Everything here is scoped to the
// authenticated caller; none of it has a public read path.

const markNoteMaxRunes = 500

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

// DeleteProgress removes the caller's position in one fiction - the "เอาออก"
// and "เก็บกวาด" moves. Deleting an absent row is a no-op: removal must
// always work, including for a fiction the user can no longer read.
func (r *Repository) DeleteProgress(ctx context.Context, userID, novelID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM reading_progress WHERE user_id = $1 AND novel_id = $2`,
		userID, novelID)
	if err != nil {
		return fmt.Errorf("delete progress: %w", err)
	}
	return nil
}

// SetFollowNotify flips the per-follow notification switch. Reports whether a
// follow row existed to flip.
func (r *Repository) SetFollowNotify(
	ctx context.Context, followerID, followingID uuid.UUID, notify bool,
) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE user_follows SET notify_new_chapters = $3
		WHERE follower_id = $1 AND following_id = $2`,
		followerID, followingID, notify)
	if err != nil {
		return false, fmt.Errorf("set follow notify: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set follow notify: %w", err)
	}
	return affected > 0, nil
}

// UpsertMark records (or edits) the caller's private "finished" mark. The
// original finished_at survives an edit - re-saving a note is not re-reading
// the fiction.
func (r *Repository) UpsertMark(
	ctx context.Context, userID, novelID uuid.UUID, stars *int, note *string,
) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO novel_marks (user_id, novel_id, stars, note)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, novel_id) DO UPDATE
		SET stars = EXCLUDED.stars, note = EXCLUDED.note`,
		userID, novelID, stars, note)
	if err != nil {
		return fmt.Errorf("upsert mark: %w", err)
	}
	return nil
}

// DeleteMark removes the caller's finished mark. Idempotent.
func (r *Repository) DeleteMark(ctx context.Context, userID, novelID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM novel_marks WHERE user_id = $1 AND novel_id = $2`, userID, novelID)
	if err != nil {
		return fmt.Errorf("delete mark: %w", err)
	}
	return nil
}

type markRow struct {
	NovelID    uuid.UUID
	FinishedAt time.Time
	Stars      sql.NullInt64
	Note       sql.NullString
}

// ListMarks returns one page of the caller's finished fictions, newest first.
func (r *Repository) ListMarks(
	ctx context.Context, userID uuid.UUID, page pagination.Params,
) ([]markRow, int64, error) {
	args := &argList{}
	from := ` FROM novel_marks m JOIN novels n ON n.id = m.novel_id`
	where := ` WHERE m.user_id = ` + args.add(userID) + ` AND ` + visibleNovelSQL(args, userID)

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*)`+from+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count marks: %w", err)
	}
	if total == 0 {
		return []markRow{}, 0, nil
	}

	query := `SELECT m.novel_id, m.finished_at, m.stars, m.note` + from + where +
		` ORDER BY m.finished_at DESC, m.novel_id DESC` +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())
	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list marks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []markRow{}
	for rows.Next() {
		var row markRow
		if err := rows.Scan(&row.NovelID, &row.FinishedAt, &row.Stars, &row.Note); err != nil {
			return nil, 0, fmt.Errorf("scan mark: %w", err)
		}
		entries = append(entries, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list marks: %w", err)
	}
	return entries, total, nil
}

// RecordEvent notes that the caller opened a chapter - unless they opted out
// of history. ONE statement on the hot path: the opt-out gate is the WHERE
// on the source row, so recording costs no extra round trip.
func (r *Repository) RecordEvent(ctx context.Context, userID, novelID, chapterID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO reading_events (user_id, novel_id, chapter_id)
		SELECT $1, $2, $3
		WHERE NOT EXISTS (SELECT 1 FROM reading_history_optout WHERE user_id = $1)
		ON CONFLICT (user_id, chapter_id) DO UPDATE SET read_at = now()`,
		userID, novelID, chapterID)
	if err != nil {
		return fmt.Errorf("record reading event: %w", err)
	}
	return nil
}

type eventRow struct {
	NovelID uuid.UUID
	ReadAt  time.Time

	ChapterID     uuid.NullUUID
	ChapterNumber sql.NullInt64
	ChapterSlug   sql.NullString
	ChapterTitle  sql.NullString
}

// ListEvents returns one page of the caller's history, newest first. The
// chapter joins on the resumable predicate like Continue Reading does: a
// retracted chapter drops the link, never the reader's own record.
func (r *Repository) ListEvents(
	ctx context.Context, userID uuid.UUID, page pagination.Params,
) ([]eventRow, int64, error) {
	args := &argList{}
	from := ` FROM reading_events e JOIN novels n ON n.id = e.novel_id`
	where := ` WHERE e.user_id = ` + args.add(userID) + ` AND ` + visibleNovelSQL(args, userID)

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*)`+from+where, args.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count history: %w", err)
	}
	if total == 0 {
		return []eventRow{}, 0, nil
	}

	query := `SELECT e.novel_id, e.read_at, c.id, c.chapter_number, c.slug, c.title` +
		from +
		` LEFT JOIN chapters c ON c.id = e.chapter_id AND ` + resumableChapterSQL(args, userID) +
		where +
		` ORDER BY e.read_at DESC, e.chapter_id DESC` +
		` LIMIT ` + args.add(page.Limit()) + ` OFFSET ` + args.add(page.Offset())
	rows, err := r.db.QueryContext(ctx, query, args.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []eventRow{}
	for rows.Next() {
		var row eventRow
		if err := rows.Scan(&row.NovelID, &row.ReadAt,
			&row.ChapterID, &row.ChapterNumber, &row.ChapterSlug, &row.ChapterTitle); err != nil {
			return nil, 0, fmt.Errorf("scan history: %w", err)
		}
		entries = append(entries, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list history: %w", err)
	}
	return entries, total, nil
}

// ClearEvents erases the caller's whole history - README's "Remove reading
// history", as one statement.
func (r *Repository) ClearEvents(ctx context.Context, userID uuid.UUID) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM reading_events WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("clear history: %w", err)
	}
	return nil
}

// HistoryOptedOut reports whether the caller turned history recording off.
func (r *Repository) HistoryOptedOut(ctx context.Context, userID uuid.UUID) (bool, error) {
	var optedOut bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM reading_history_optout WHERE user_id = $1)`,
		userID).Scan(&optedOut)
	if err != nil {
		return false, fmt.Errorf("check history optout: %w", err)
	}
	return optedOut, nil
}

// SetHistoryOptOut flips the recording switch. Both directions idempotent.
func (r *Repository) SetHistoryOptOut(ctx context.Context, userID uuid.UUID, optOut bool) error {
	var err error
	if optOut {
		_, err = r.db.ExecContext(ctx, `
			INSERT INTO reading_history_optout (user_id) VALUES ($1)
			ON CONFLICT (user_id) DO NOTHING`, userID)
	} else {
		_, err = r.db.ExecContext(ctx,
			`DELETE FROM reading_history_optout WHERE user_id = $1`, userID)
	}
	if err != nil {
		return fmt.Errorf("set history optout: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// DeleteProgress removes the caller's saved position in one fiction.
//
// The ref resolves through ForReader when possible, but an unreadable fiction
// must STILL be removable - the row is the caller's own, and being unable to
// clean a dead entry off one's own shelf would be absurd. So an access
// failure falls back to resolving the bare id.
func (s *Service) DeleteProgress(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	novel, novelErr := s.novels.ForReader(ctx, identity, ref)
	if novelErr == nil {
		return s.deleteProgressRow(ctx, userID, novel.ID)
	}
	if !ref.BySlug() {
		return s.deleteProgressRow(ctx, userID, ref.ID)
	}
	return novelErr
}

func (s *Service) deleteProgressRow(ctx context.Context, userID, novelID uuid.UUID) error {
	if err := s.repo.DeleteProgress(ctx, userID, novelID); err != nil {
		return s.internal("delete progress", err)
	}
	return nil
}

// SetFollowNotify flips the caller's per-follow notification switch.
func (s *Service) SetFollowNotify(
	ctx context.Context, identity *auth.Identity, targetID uuid.UUID, notify bool,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	updated, err := s.repo.SetFollowNotify(ctx, userID, targetID, notify)
	if err != nil {
		return s.internal("set follow notify", err)
	}
	if !updated {
		// No follow row - same 404 an unknown user gets (docs/11 §3.4).
		return userNotFound()
	}
	return nil
}

// MarkInput is a validated finished mark.
type MarkInput struct {
	Stars *int
	Note  *string
}

// MarkFinished records (or edits) the caller's private finished mark.
func (s *Service) MarkFinished(
	ctx context.Context, identity *auth.Identity, ref novels.Ref, input MarkInput,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}

	errs := map[string][]string{}
	if input.Stars != nil && (*input.Stars < 1 || *input.Stars > 5) {
		errs["stars"] = []string{"Must be between 1 and 5."}
	}
	if input.Note != nil {
		trimmed := strings.TrimSpace(*input.Note)
		if trimmed == "" {
			input.Note = nil
		} else if utf8.RuneCountInString(trimmed) > markNoteMaxRunes {
			errs["note"] = []string{"The note is too long."}
		} else {
			input.Note = &trimmed
		}
	}
	if len(errs) > 0 {
		return apierror.Validation(errs)
	}

	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return err
	}
	if err := s.repo.UpsertMark(ctx, userID, novel.ID, input.Stars, input.Note); err != nil {
		return s.internal("mark finished", err)
	}
	return nil
}

// UnmarkFinished removes the caller's finished mark. Like every removal on
// the shelf it works even when the fiction is gone.
func (s *Service) UnmarkFinished(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	novel, novelErr := s.novels.ForReader(ctx, identity, ref)
	if novelErr == nil {
		if err := s.repo.DeleteMark(ctx, userID, novel.ID); err != nil {
			return s.internal("unmark finished", err)
		}
		return nil
	}
	if !ref.BySlug() {
		if err := s.repo.DeleteMark(ctx, userID, ref.ID); err != nil {
			return s.internal("unmark finished", err)
		}
		return nil
	}
	return novelErr
}

// Finished returns one page of the caller's finished fictions.
func (s *Service) Finished(
	ctx context.Context, identity *auth.Identity, page pagination.Params,
) ([]FinishedEntry, pagination.Meta, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	rows, total, err := s.repo.ListMarks(ctx, userID, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list marks", err)
	}
	records, err := s.novelCards(ctx, rowIDs(rows, func(r markRow) uuid.UUID { return r.NovelID }))
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	entries := make([]FinishedEntry, 0, len(rows))
	for _, row := range rows {
		record, ok := records[row.NovelID]
		if !ok {
			continue
		}
		entry := FinishedEntry{
			Novel:      record.ViewFor(isOwner(identity, &record.Novel)),
			FinishedAt: row.FinishedAt,
		}
		if row.Stars.Valid {
			stars := int(row.Stars.Int64)
			entry.Stars = &stars
		}
		if row.Note.Valid {
			note := row.Note.String
			entry.Note = &note
		}
		entries = append(entries, entry)
	}
	return entries, page.MetaFor(total), nil
}

// History returns one page of the caller's reading history. Owner-only by
// construction - there is no other identity to ask for.
func (s *Service) History(
	ctx context.Context, identity *auth.Identity, page pagination.Params,
) ([]HistoryEntry, pagination.Meta, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	rows, total, err := s.repo.ListEvents(ctx, userID, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list history", err)
	}
	records, err := s.novelCards(ctx, rowIDs(rows, func(r eventRow) uuid.UUID { return r.NovelID }))
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	entries := make([]HistoryEntry, 0, len(rows))
	for _, row := range rows {
		record, ok := records[row.NovelID]
		if !ok {
			continue
		}
		entry := HistoryEntry{
			Novel:  record.ViewFor(isOwner(identity, &record.Novel)),
			ReadAt: row.ReadAt,
		}
		if row.ChapterID.Valid {
			chapter := ChapterRef{
				ID:            row.ChapterID.UUID,
				ChapterNumber: int(row.ChapterNumber.Int64),
				Slug:          row.ChapterSlug.String,
			}
			if row.ChapterTitle.Valid {
				title := row.ChapterTitle.String
				chapter.Title = &title
			}
			entry.Chapter = &chapter
		}
		entries = append(entries, entry)
	}
	return entries, page.MetaFor(total), nil
}

// ClearHistory erases the caller's whole reading history.
func (s *Service) ClearHistory(ctx context.Context, identity *auth.Identity) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	if err := s.repo.ClearEvents(ctx, userID); err != nil {
		return s.internal("clear history", err)
	}
	return nil
}

// HistorySettingsFor reads the caller's recording switch.
func (s *Service) HistorySettingsFor(
	ctx context.Context, identity *auth.Identity,
) (*HistorySettings, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	optedOut, err := s.repo.HistoryOptedOut(ctx, userID)
	if err != nil {
		return nil, s.internal("read history settings", err)
	}
	return &HistorySettings{RecordHistory: !optedOut}, nil
}

// SetHistorySettings flips the recording switch. Turning recording OFF also
// stops the CURRENT session's saves from being recorded - the gate lives in
// the recording statement itself, so there is no window where "off" still
// writes.
func (s *Service) SetHistorySettings(
	ctx context.Context, identity *auth.Identity, record bool,
) (*HistorySettings, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	if err := s.repo.SetHistoryOptOut(ctx, userID, !record); err != nil {
		return nil, s.internal("set history settings", err)
	}
	return &HistorySettings{RecordHistory: record}, nil
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// DeleteProgress handles DELETE /api/v1/novels/:novel/progress.
func (h *Handler) DeleteProgress(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	if err := h.service.DeleteProgress(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

type followNotifyRequest struct {
	NotifyNewChapters *bool `json:"notify_new_chapters"`
}

// SetFollowNotify handles PATCH /api/v1/users/:user/follow.
func (h *Handler) SetFollowNotify(c *gin.Context) {
	target, ok := userRef(c)
	if !ok {
		return
	}
	var req followNotifyRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.NotifyNewChapters == nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"notify_new_chapters": {"Must be true or false."},
		}))
		return
	}
	if err := h.service.SetFollowNotify(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), target, *req.NotifyNewChapters); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

type markRequest struct {
	Stars *int    `json:"stars"`
	Note  *string `json:"note"`
}

// MarkFinished handles PUT /api/v1/novels/:novel/finished.
func (h *Handler) MarkFinished(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	var req markRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apierror.BadRequest("The request body could not be parsed as JSON."))
		return
	}
	if err := h.service.MarkFinished(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref,
		MarkInput{Stars: req.Stars, Note: req.Note}); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// UnmarkFinished handles DELETE /api/v1/novels/:novel/finished.
func (h *Handler) UnmarkFinished(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	if err := h.service.UnmarkFinished(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Finished handles GET /api/v1/me/finished.
func (h *Handler) Finished(c *gin.Context) {
	entries, meta, err := h.service.Finished(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, entries, meta)
}

// History handles GET /api/v1/me/history.
func (h *Handler) History(c *gin.Context) {
	entries, meta, err := h.service.History(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, entries, meta)
}

// ClearHistory handles DELETE /api/v1/me/history.
func (h *Handler) ClearHistory(c *gin.Context) {
	if err := h.service.ClearHistory(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context())); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// HistorySettings handles GET /api/v1/me/history/settings.
func (h *Handler) HistorySettings(c *gin.Context) {
	settings, err := h.service.HistorySettingsFor(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, settings)
}

type historySettingsRequest struct {
	RecordHistory *bool `json:"record_history"`
}

// SetHistorySettings handles PUT /api/v1/me/history/settings.
func (h *Handler) SetHistorySettings(c *gin.Context) {
	var req historySettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.RecordHistory == nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"record_history": {"Must be true or false."},
		}))
		return
	}
	settings, err := h.service.SetHistorySettings(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), *req.RecordHistory)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, settings)
}
