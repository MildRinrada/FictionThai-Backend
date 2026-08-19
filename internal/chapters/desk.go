package chapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The writer's desk: the few facts the shell asks for on every page.
//
// They live here because they are questions about CHAPTERS - how much is
// unfinished, what was written today, where the writer left off - and the rule
// this codebase keeps is that a domain's tables are read by that domain. The
// composition that serves them to the client is in package desk, above this
// one.

// writerZone is the day boundary every "วันนี้" is measured against.
//
// A Thai platform's day is Thailand's day. Deriving it from the server's clock
// would move a writer's midnight when the application is deployed somewhere
// else, and a word counter that resets at 07:00 is a bug the writer cannot
// explain.
var writerZone = time.FixedZone("ICT", 7*60*60)

// WriterDay is the calendar day `at` falls on for a Thai writer.
func WriterDay(at time.Time) time.Time {
	local := at.In(writerZone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, writerZone)
}

// addWrittenWords credits a day's tally with the words a save ADDED.
//
// Called inside the saving transaction, so the tally cannot drift from the
// manuscript: either the chapter and the day's total both move or neither
// does. Non-positive deltas are dropped rather than subtracted - see the
// migration - and dropping them here also means the common case (a save that
// changed nothing) writes no row at all.
func addWrittenWords(
	ctx context.Context, tx *sql.Tx, userID uuid.UUID, at time.Time, delta int,
) error {
	if delta <= 0 || userID == uuid.Nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO writing_days (user_id, day, words)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, day) DO UPDATE
		SET words = writing_days.words + EXCLUDED.words`,
		userID, WriterDay(at), delta,
	); err != nil {
		return fmt.Errorf("record written words: %w", err)
	}
	return nil
}

// WordsWrittenOn totals what one writer added on one day.
func (r *Repository) WordsWrittenOn(
	ctx context.Context, userID uuid.UUID, day time.Time,
) (int, error) {
	var words int
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(words, 0) FROM writing_days
		WHERE user_id = $1 AND day = $2`,
		userID, WriterDay(day),
	).Scan(&words)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read written words: %w", err)
	}
	return words, nil
}

// UnfinishedForAuthor counts the writer's drafts that have something in them.
//
// "ร่างที่ค้าง" means work with words in it that nobody can read yet. An empty
// chapter someone opened and abandoned is not a task - counting it would put a
// number on the studio link that the writer can never clear without deleting
// something, which teaches them to ignore the number.
//
// Scheduled chapters are excluded too: they are finished work with a date on
// them, and nothing is owed.
func (r *Repository) UnfinishedForAuthor(ctx context.Context, userID uuid.UUID) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM chapters c
		JOIN novels n ON n.id = c.novel_id
		WHERE n.author_id = $1
		  AND n.deleted_at IS NULL
		  AND c.deleted_at IS NULL
		  AND c.status = $2
		  AND c.word_count > 0`,
		userID, string(StatusDraft),
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unfinished chapters: %w", err)
	}
	return count, nil
}

// Resume is where a writer was last working.
type Resume struct {
	NovelSlug    string
	NovelTitle   string
	ChapterSlug  string
	ChapterLabel string
	// UpdatedAt is when it was last touched, so the client can say "42 นาทีที่แล้ว".
	UpdatedAt time.Time
}

// LastEditedForAuthor finds the chapter the writer touched most recently.
//
// Their own fictions only. A collaborator's chapter is somebody else's desk,
// and "เขียนต่อจากที่ค้าง" jumping into another person's work-in-progress is
// not a shortcut anyone asked for.
func (r *Repository) LastEditedForAuthor(
	ctx context.Context, userID uuid.UUID,
) (*Resume, error) {
	var (
		resume Resume
		title  sql.NullString
		number int
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT n.slug, n.title, c.slug, c.title, c.chapter_number, c.updated_at
		FROM chapters c
		JOIN novels n ON n.id = c.novel_id
		WHERE n.author_id = $1
		  AND n.deleted_at IS NULL
		  AND c.deleted_at IS NULL
		ORDER BY c.updated_at DESC
		LIMIT 1`,
		userID,
	).Scan(
		&resume.NovelSlug, &resume.NovelTitle,
		&resume.ChapterSlug, &title, &number, &resume.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last edited chapter: %w", err)
	}
	resume.ChapterLabel = title.String
	if resume.ChapterLabel == "" {
		resume.ChapterLabel = fmt.Sprintf("ตอนที่ %d", number)
	}
	return &resume, nil
}

// UnfinishedByNovel counts unfinished chapters per fiction, for the ids given.
//
// One query for the whole list rather than one per fiction: the create menu
// shows three works and the studio badge is on every page, so this is on the
// shell's path.
func (r *Repository) UnfinishedByNovel(
	ctx context.Context, novelIDs []uuid.UUID,
) (map[uuid.UUID]int, error) {
	counts := map[uuid.UUID]int{}
	if len(novelIDs) == 0 {
		return counts, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT novel_id, COUNT(*)
		FROM chapters
		WHERE novel_id = ANY($1)
		  AND deleted_at IS NULL
		  AND status = $2
		  AND word_count > 0
		GROUP BY novel_id`,
		uuidArray(novelIDs), string(StatusDraft),
	)
	if err != nil {
		return nil, fmt.Errorf("count unfinished by novel: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id    uuid.UUID
			count int
		)
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("scan unfinished by novel: %w", err)
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// DeskHit is one of the writer's own pieces of work, found by name.
type DeskHit struct {
	NovelSlug    string
	NovelTitle   string
	ChapterSlug  string
	ChapterLabel string
	// Draft marks work nobody else can read yet, which is most of what a
	// writer is looking for when they search their own desk.
	Draft bool
}

// SearchOwn finds the caller's own fictions and chapters BY TITLE.
//
// Drafts included, and that is the entire point: a writer typing a chapter
// name wants the editor, and public search cannot show them their own
// unpublished work by definition. Scoped to author_id, so this can only ever
// return the caller's own manuscripts.
//
// Titles only, not prose. Full-text search inside a manuscript already exists
// per fiction (13Y §8); what a header suggestion box needs is a way to jump,
// and jumping is by name.
func (r *Repository) SearchOwn(
	ctx context.Context, userID uuid.UUID, query string, limit int,
) ([]DeskHit, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" || userID == uuid.Nil {
		return []DeskHit{}, nil
	}
	if limit <= 0 {
		limit = 5
	}
	// Wildcards bound as DATA and escaped, so "%" is a character the writer
	// typed rather than a request for everything they own.
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	pattern := "%" + replacer.Replace(trimmed) + "%"

	rows, err := r.db.QueryContext(ctx, `
		SELECT n.slug, n.title, c.slug, COALESCE(c.title, ''), c.chapter_number,
		       c.status = $3 AS draft
		FROM chapters c
		JOIN novels n ON n.id = c.novel_id
		WHERE n.author_id = $1
		  AND n.deleted_at IS NULL
		  AND c.deleted_at IS NULL
		  AND (c.title ILIKE $2 ESCAPE '\' OR n.title ILIKE $2 ESCAPE '\')
		ORDER BY c.updated_at DESC
		LIMIT $4`,
		userID, pattern, string(StatusDraft), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("search own work: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []DeskHit{}
	for rows.Next() {
		var (
			hit    DeskHit
			title  string
			number int
		)
		if err := rows.Scan(
			&hit.NovelSlug, &hit.NovelTitle, &hit.ChapterSlug, &title, &number, &hit.Draft,
		); err != nil {
			return nil, fmt.Errorf("scan own work: %w", err)
		}
		hit.ChapterLabel = title
		if hit.ChapterLabel == "" {
			hit.ChapterLabel = fmt.Sprintf("ตอนที่ %d", number)
		}
		out = append(out, hit)
	}
	return out, rows.Err()
}
