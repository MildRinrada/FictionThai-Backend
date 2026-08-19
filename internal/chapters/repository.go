package chapters

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/slug"
)

var (
	ErrNotFound    = errors.New("chapter not found")
	ErrSlugTaken   = errors.New("chapter slug already taken")
	ErrNumberTaken = errors.New("chapter number already taken")
)

// Repository is the only place that reads or writes chapters, their messages,
// and their revisions.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const chapterColumns = `
	c.id, c.novel_id, c.chapter_number, c.title, c.slug, c.content,
	c.content_format, c.presentation_format, c.entry_fields,
	c.status, c.published_at, c.scheduled_at, c.word_count,
	c.created_at, c.updated_at, c.deleted_at`

type scanner interface{ Scan(...any) error }

// chapterTargets is the scan destination list for chapterColumns. Both the
// single-row and the list path go through it, so a column added to one can
// never be forgotten by the other.
func chapterTargets(c *Chapter, format *sql.NullString, fields *[]byte) []any {
	return []any{
		&c.ID, &c.NovelID, &c.Number, &c.Title, &c.Slug, &c.Content,
		&c.ContentFormat, format, fields,
		&c.Status, &c.PublishedAt, &c.ScheduledAt, &c.WordCount,
		&c.CreatedAt, &c.UpdatedAt, &c.DeletedAt,
	}
}

// applyFormatAndFields decodes the two columns that need interpreting.
//
// An unreadable entry_fields value is treated as no fields rather than as a
// failed request: the labels are presentation, and refusing to serve a chapter
// because its column headings did not parse would deny a writer their own text.
func applyFormatAndFields(c *Chapter, format sql.NullString, fields []byte) {
	if format.Valid && format.String != "" {
		value := fiction.PresentationFormat(format.String)
		c.Format = &value
	}
	c.EntryFields = []string{}
	if len(fields) > 0 {
		var decoded []string
		if err := json.Unmarshal(fields, &decoded); err == nil && decoded != nil {
			c.EntryFields = decoded
		}
	}
}

func scanChapter(row scanner) (*Chapter, error) {
	var (
		c      Chapter
		format sql.NullString
		fields []byte
	)
	err := row.Scan(chapterTargets(&c, &format, &fields)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan chapter: %w", err)
	}
	applyFormatAndFields(&c, format, fields)
	return &c, nil
}

// Listing is a chapter plus which representations it has prepared.
//
// The flags come from the same query as the row: computing them per chapter with
// a follow-up query would be an N+1 on the fiction's table of contents, which is
// a reader-facing page (docs/07 §67).
type Listing struct {
	Chapter
	Presence
}

// Ref identifies a chapter in a URL: a UUID or a slug within its fiction.
type Ref struct {
	ID   uuid.UUID
	Slug string
}

func (r Ref) BySlug() bool { return r.ID == uuid.Nil }

// ParseRef interprets a path parameter.
//
// A malformed reference is ErrNotFound, not a parse error: distinguishing
// "well-formed but absent" from "malformed" is information worth denying to
// anyone probing for content (docs/11 §3.4).
func ParseRef(raw string) (Ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Ref{}, ErrNotFound
	}
	if id, err := uuid.Parse(raw); err == nil {
		return Ref{ID: id}, nil
	}
	if !slug.Valid(raw) {
		return Ref{}, ErrNotFound
	}
	return Ref{Slug: raw}, nil
}

// List returns a fiction's chapters in deterministic order.
//
// Ordering is by chapter_number, never by insertion order or by id: docs/15
// §5.3 requires chapter order to be correct, and a physical-order default would
// silently reshuffle after any row update.
//
// liveOnly applies the reader gate. It is the caller's ONLY way to widen the
// result, so a forgotten flag hides drafts rather than exposing them.
func (r *Repository) List(ctx context.Context, novelID uuid.UUID, liveOnly bool) ([]Listing, error) {
	where := "c.novel_id = $1 AND c.deleted_at IS NULL"
	if liveOnly {
		where = "c.novel_id = $1 AND " + novels.LiveChapterSQL
	}

	query := `
		SELECT ` + chapterColumns + `,
			(SELECT COUNT(*) FROM chapter_messages m WHERE m.chapter_id = c.id),
			(SELECT COUNT(*) FROM chapter_entries e WHERE e.chapter_id = c.id)
		FROM chapters c
		WHERE ` + where + `
		ORDER BY c.chapter_number ASC`

	rows, err := r.db.QueryContext(ctx, query, novelID)
	if err != nil {
		return nil, fmt.Errorf("list chapters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	listings := []Listing{}
	for rows.Next() {
		var (
			l      Listing
			format sql.NullString
			fields []byte
		)
		targets := append(chapterTargets(&l.Chapter, &format, &fields),
			&l.MessageCount, &l.EntryCount)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scan chapter listing: %w", err)
		}
		applyFormatAndFields(&l.Chapter, format, fields)
		l.HasMessages = l.MessageCount > 0
		l.HasEntries = l.EntryCount > 0
		listings = append(listings, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chapters: %w", err)
	}
	return listings, nil
}

// CountLive reports how many non-deleted chapters a fiction has, for the
// reorder completeness check.
func (r *Repository) CountLive(ctx context.Context, novelID uuid.UUID) (int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chapters WHERE novel_id = $1 AND deleted_at IS NULL`,
		novelID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count chapters: %w", err)
	}
	return total, nil
}

// reorderParkBase is far above MaxChapterNumber, so parked numbers can never
// collide with a legal one.
const reorderParkBase = 1_000_000

// Reorder renumbers a fiction's chapters to 1..N following the given id order.
//
// Two passes inside one transaction: the uniqueness index on
// (novel_id, chapter_number) is not deferrable, so every chapter is first
// parked far above the legal range and then given its final number - no
// intermediate state can collide. updated_at is deliberately untouched: a
// reorder rearranges the shelf, it does not edit any chapter.
func (r *Repository) Reorder(ctx context.Context, novelID uuid.UUID, ids []uuid.UUID) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reorder chapters: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE chapters SET chapter_number = $3
			 WHERE id = $1 AND novel_id = $2 AND deleted_at IS NULL`,
			id, novelID, reorderParkBase+i+1); err != nil {
			return fmt.Errorf("reorder chapters (park): %w", err)
		}
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE chapters SET chapter_number = $3
			 WHERE id = $1 AND novel_id = $2 AND deleted_at IS NULL`,
			id, novelID, i+1); err != nil {
			return fmt.Errorf("reorder chapters: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reorder chapters: %w", err)
	}
	return nil
}

// Find loads one chapter of a fiction, applying no visibility rule of its own.
//
// The novel_id is part of the lookup rather than merely being verified
// afterwards: without it, a chapter UUID from one fiction could be read through
// another fiction's URL, which is the confused-deputy shape of an IDOR
// (docs/11 §8).
func (r *Repository) Find(ctx context.Context, novelID uuid.UUID, ref Ref) (*Chapter, error) {
	query := `SELECT ` + chapterColumns + `
		FROM chapters c
		WHERE c.novel_id = $1 AND c.deleted_at IS NULL AND `

	if ref.BySlug() {
		return scanChapter(r.db.QueryRowContext(ctx, query+`c.slug = $2`, novelID, ref.Slug))
	}
	return scanChapter(r.db.QueryRowContext(ctx, query+`c.id = $2`, novelID, ref.ID))
}

// Messages loads a chapter's conversation in order.
func (r *Repository) Messages(ctx context.Context, chapterID uuid.UUID) ([]Message, error) {
	return loadMessages(ctx, r.db, chapterID)
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func loadMessages(ctx context.Context, db queryer, chapterID uuid.UUID) ([]Message, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, position, speaker_name, speaker_avatar_url, message_type, content, metadata
		FROM chapter_messages
		WHERE chapter_id = $1
		ORDER BY position ASC`, chapterID)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	messages := []Message{}
	for rows.Next() {
		var m Message
		var metadata []byte
		if err := rows.Scan(&m.ID, &m.Position, &m.SpeakerName, &m.SpeakerAvatarURL,
			&m.Type, &m.Content, &metadata); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if len(metadata) > 0 {
			var parsed Metadata
			if err := json.Unmarshal(metadata, &parsed); err != nil {
				return nil, fmt.Errorf("decode message metadata: %w", err)
			}
			if !parsed.IsEmpty() {
				m.Metadata = &parsed
			}
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	return messages, nil
}

// Entries loads a chapter's headcanon topic in order (12F).
func (r *Repository) Entries(ctx context.Context, chapterID uuid.UUID) ([]Entry, error) {
	return loadEntries(ctx, r.db, chapterID)
}

func loadEntries(ctx context.Context, db queryer, chapterID uuid.UUID) ([]Entry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, position, character_id, name, field_values, body, image_url
		FROM chapter_entries
		WHERE chapter_id = $1
		ORDER BY position ASC`, chapterID)
	if err != nil {
		return nil, fmt.Errorf("load entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []Entry{}
	for rows.Next() {
		var (
			e      Entry
			values []byte
		)
		if err := rows.Scan(&e.ID, &e.Position, &e.CharacterID, &e.Name,
			&values, &e.Body, &e.ImageURL); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		e.Values = []string{}
		if len(values) > 0 {
			var decoded []string
			if err := json.Unmarshal(values, &decoded); err == nil && decoded != nil {
				e.Values = decoded
			}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load entries: %w", err)
	}
	return entries, nil
}

// replaceEntries makes the stored topic equal to entries.
//
// Delete-then-insert, exactly like replaceMessages and for the same reasons:
// positions are dense and reassigned from array order, so almost every row would
// move anyway, and the previous topic is already preserved in the revision
// snapshot taken earlier in this same transaction.
func replaceEntries(ctx context.Context, tx *sql.Tx, chapterID uuid.UUID, entries []Entry) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chapter_entries WHERE chapter_id = $1`, chapterID); err != nil {
		return fmt.Errorf("clear entries: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}

	for start := 0; start < len(entries); start += messageInsertBatch {
		end := start + messageInsertBatch
		if end > len(entries) {
			end = len(entries)
		}

		var (
			placeholders = make([]string, 0, end-start)
			args         = make([]any, 0, (end-start)*7)
		)
		for _, entry := range entries[start:end] {
			base := len(args)
			placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7))

			values := entry.Values
			if values == nil {
				values = []string{}
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				return fmt.Errorf("encode entry values: %w", err)
			}

			args = append(args, chapterID, entry.Position, entry.CharacterID,
				entry.Name, encoded, entry.Body, entry.ImageURL)
		}

		query := `INSERT INTO chapter_entries
			(chapter_id, position, character_id, name, field_values, body, image_url)
			VALUES ` + strings.Join(placeholders, ", ")

		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert entries: %w", err)
		}
	}
	return nil
}

// Neighbours returns the ids of the chapters either side of number.
//
// liveOnly must match the caller's visibility, or navigation would leak the
// existence of an unpublished chapter by linking to it (docs/11 §21).
func (r *Repository) Neighbours(
	ctx context.Context, novelID uuid.UUID, number int, liveOnly bool,
) (previous, next *uuid.UUID, err error) {
	visible := "c.deleted_at IS NULL"
	if liveOnly {
		visible = novels.LiveChapterSQL
	}

	query := `
		SELECT
			(SELECT c.id FROM chapters c
			 WHERE c.novel_id = $1 AND c.chapter_number < $2 AND ` + visible + `
			 ORDER BY c.chapter_number DESC LIMIT 1),
			(SELECT c.id FROM chapters c
			 WHERE c.novel_id = $1 AND c.chapter_number > $2 AND ` + visible + `
			 ORDER BY c.chapter_number ASC LIMIT 1)`

	if err := r.db.QueryRowContext(ctx, query, novelID, number).Scan(&previous, &next); err != nil {
		return nil, nil, fmt.Errorf("load chapter neighbours: %w", err)
	}
	return previous, next, nil
}

// CharactersInNovel reports which of ids are characters of novelID.
//
// The mirror of characters.Repository.ChapterBelongsTo, and it exists for the
// same reason: an entry naming a character id from someone else's fiction would
// let a writer confirm that id exists, and would render a stranger's cast on
// their page (docs/11 §8).
func (r *Repository) CharactersInNovel(
	ctx context.Context, novelID uuid.UUID, ids []uuid.UUID,
) (map[uuid.UUID]bool, error) {
	found := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return found, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM characters WHERE novel_id = $1 AND id = ANY($2)`,
		novelID, uuidArray(ids))
	if err != nil {
		return nil, fmt.Errorf("check characters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan character id: %w", err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("check characters: %w", err)
	}
	return found, nil
}

// uuidArray renders ids as a PostgreSQL uuid[] literal.
//
// The values are UUIDs already parsed by uuid.Parse, so their text form is a
// fixed 36-character hex pattern with no way to carry a quote - this cannot
// become an injection point (docs/11 §15).
func uuidArray(ids []uuid.UUID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return "{" + strings.Join(out, ",") + "}"
}

// CreateParams carries an already-validated new chapter.
type CreateParams struct {
	NovelID     uuid.UUID
	ActorID     uuid.UUID
	Title       *string
	Slug        string
	Content     *string
	Messages    []Message
	Entries     []Entry
	EntryFields []string
	Format      *fiction.PresentationFormat
	// ContentFormat is how the prose renders (§13N). Always set by the service -
	// a new chapter has nothing to reinterpret, so it gets the editor's model.
	ContentFormat ContentFormat
	Status        Status
	ScheduledAt   *time.Time

	// Number is the requested chapter number. Zero means "append".
	Number int
}

// Create inserts a chapter and its messages in one transaction.
//
// Either the whole chapter appears with its conversation intact or nothing does;
// a chat chapter that committed without its messages would look to its author
// like the platform had eaten their work.
func (r *Repository) Create(ctx context.Context, params CreateParams) (*Chapter, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create chapter: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	number := params.Number
	if number == 0 {
		// Allocate inside the transaction. The partial UNIQUE index on
		// (novel_id, chapter_number) is still the authority - two concurrent
		// creates make one of them fail, and the service retries.
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(chapter_number), 0) + 1
			FROM chapters WHERE novel_id = $1 AND deleted_at IS NULL`,
			params.NovelID).Scan(&number); err != nil {
			return nil, fmt.Errorf("allocate chapter number: %w", err)
		}
	}

	wordCount := contentWords(params.Content, params.Messages, params.Entries)

	fields, err := encodeFields(params.EntryFields)
	if err != nil {
		return nil, err
	}

	chapter, err := scanChapter(tx.QueryRowContext(ctx, `
		INSERT INTO chapters (
			novel_id, chapter_number, title, slug, content,
			content_format, presentation_format, entry_fields,
			status, scheduled_at, published_at, word_count
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			CASE WHEN $11 THEN now() ELSE NULL END, $12)
		RETURNING `+strings.ReplaceAll(chapterColumns, "c.", ""),
		params.NovelID, number, params.Title, params.Slug, params.Content,
		string(params.ContentFormat), formatValue(params.Format), fields,
		params.Status, params.ScheduledAt,
		// A separate BOOLEAN rather than a re-use of the status placeholder:
		// binding one parameter both as a column value and inside a comparison
		// leaves PostgreSQL unable to deduce a single type (SQLSTATE 42P08).
		params.Status == StatusPublished,
		wordCount,
	))
	if err != nil {
		return nil, classifyConflict(err)
	}

	if err := replaceMessages(ctx, tx, chapter.ID, params.Messages); err != nil {
		return nil, err
	}
	if err := replaceEntries(ctx, tx, chapter.ID, params.Entries); err != nil {
		return nil, err
	}

	// A chapter created with text in it is text that was written today. In the
	// same transaction as the chapter, so the day's tally can never claim words
	// that a rolled-back save never stored.
	if err := addWrittenWords(ctx, tx, params.ActorID, time.Now(), wordCount); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create chapter: %w", err)
	}
	chapter.WordCount = wordCount
	return chapter, nil
}

// UpdateParams carries a validated partial update.
//
// The double pointers distinguish "not mentioned" from "explicitly cleared",
// which a single pointer cannot express - and getting that wrong would let a
// PATCH that mentions only the status silently erase a manuscript.
type UpdateParams struct {
	ChapterID uuid.UUID
	ActorID   uuid.UUID

	Title   **string
	Content **string

	// Messages replaces the whole conversation when non-nil. A nil pointer
	// leaves the existing messages untouched; a pointer to an empty slice is an
	// explicit "remove them all". Entries follow the identical rule.
	Messages *[]Message
	Entries  *[]Entry

	// ContentFormat is how the prose renders (§13N). A single pointer, unlike
	// Format below: there is no "go back to following something" here, only two
	// values and "not mentioned".
	ContentFormat *ContentFormat

	EntryFields *[]string

	// Format is the chapter's own presentation. The double pointer separates
	// "not mentioned" from "go back to following the fiction" - a single
	// pointer could not express the second, and a chapter that could never
	// un-declare a format would be a one-way door.
	Format **fiction.PresentationFormat

	Status      *Status
	ScheduledAt **time.Time
	Slug        *string
}

// Update applies a partial update, recording a revision of what it replaces.
//
// Everything happens in one transaction (docs/09 §45): the revision, the row
// update, and the message replacement. A crash between them would otherwise lose
// the previous version, which is the one thing revisions exist to prevent.
//
// The chapter row is locked FOR UPDATE, so two concurrent edits serialise rather
// than interleaving into a chapter whose prose came from one writer and whose
// messages came from another (docs/09 §34).
func (r *Repository) Update(ctx context.Context, params UpdateParams) (*Chapter, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin update chapter: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := scanChapter(tx.QueryRowContext(ctx,
		`SELECT `+strings.ReplaceAll(chapterColumns, "c.", "")+`
		 FROM chapters WHERE id = $1 AND deleted_at IS NULL
		 FOR UPDATE`, params.ChapterID))
	if err != nil {
		return nil, err
	}

	contentChanging := params.Title != nil || params.Content != nil ||
		params.Messages != nil || params.Entries != nil || params.EntryFields != nil

	// How many words this save added, credited to the writer's day below.
	written := 0

	// The other representations as they stand. Needed both for the revision
	// snapshot and for recomputing the word count when only the prose changed.
	var (
		existing        []Message
		existingEntries []Entry
	)
	if contentChanging {
		existing, err = loadMessages(ctx, tx, current.ID)
		if err != nil {
			return nil, err
		}
		existingEntries, err = loadEntries(ctx, tx, current.ID)
		if err != nil {
			return nil, err
		}
		if err := insertRevision(ctx, tx, current, existing, existingEntries, params.ActorID); err != nil {
			return nil, err
		}
	}

	args := &argList{}
	sets := []string{"updated_at = now()"}

	if params.Title != nil {
		sets = append(sets, "title = "+args.add(*params.Title))
	}
	if params.Content != nil {
		sets = append(sets, "content = "+args.add(*params.Content))
	}
	if params.Slug != nil {
		sets = append(sets, "slug = "+args.add(*params.Slug))
	}
	if params.ContentFormat != nil {
		sets = append(sets, "content_format = "+args.add(string(*params.ContentFormat)))
	}
	if params.Format != nil {
		sets = append(sets, "presentation_format = "+args.add(formatValue(*params.Format)))
	}
	if params.EntryFields != nil {
		fields, err := encodeFields(*params.EntryFields)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "entry_fields = "+args.add(fields))
	}
	if params.ScheduledAt != nil {
		sets = append(sets, "scheduled_at = "+args.add(*params.ScheduledAt))
	}
	if params.Status != nil {
		sets = append(sets, "status = "+args.add(*params.Status))
		// Stamp the first publication and keep it. COALESCE means republishing
		// does not move a reader's "published on" date forward.
		//
		// The condition is a separate BOOLEAN parameter: reusing the status
		// placeholder inside a comparison as well as a column value leaves
		// PostgreSQL unable to deduce a single type for it (SQLSTATE 42P08).
		sets = append(sets, `published_at = CASE WHEN `+
			args.add(*params.Status == StatusPublished)+
			` THEN COALESCE(published_at, now()) ELSE published_at END`)
	}

	if contentChanging {
		finalContent := current.Content
		if params.Content != nil {
			finalContent = *params.Content
		}
		finalMessages := existing
		if params.Messages != nil {
			finalMessages = *params.Messages
		}
		finalEntries := existingEntries
		if params.Entries != nil {
			finalEntries = *params.Entries
		}
		wordCount := contentWords(finalContent, finalMessages, finalEntries)
		sets = append(sets, "word_count = "+args.add(wordCount))
		// What this save ADDED - not what the chapter now holds. Fixing one
		// typo in a 4,000-word chapter is not 4,000 words written today.
		written = wordCount - current.WordCount
	}

	updated, err := scanChapter(tx.QueryRowContext(ctx,
		`UPDATE chapters SET `+strings.Join(sets, ", ")+
			` WHERE id = `+args.add(params.ChapterID)+` AND deleted_at IS NULL`+
			` RETURNING `+strings.ReplaceAll(chapterColumns, "c.", ""),
		args.args...))
	if err != nil {
		return nil, classifyConflict(err)
	}

	if params.Messages != nil {
		if err := replaceMessages(ctx, tx, updated.ID, *params.Messages); err != nil {
			return nil, err
		}
	}
	if params.Entries != nil {
		if err := replaceEntries(ctx, tx, updated.ID, *params.Entries); err != nil {
			return nil, err
		}
	}

	if err := addWrittenWords(ctx, tx, params.ActorID, time.Now(), written); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit update chapter: %w", err)
	}
	return updated, nil
}

// insertRevision records an immutable snapshot of the chapter as it stands.
//
// docs/CONTENT-MODEL.md §5: one row captures BOTH representations, so restoring
// it restores the complete authored state regardless of which format was active
// when it was taken.
//
// The version is allocated from MAX+1 inside the caller's transaction, with
// UNIQUE(chapter_id, version) as the authority against a concurrent writer.
// Entries snapshot into COLUMNS of the same row rather than into the existing
// `messages` JSON. §5's argument is atomicity - one row commits or it does not
// - and columns keep that while leaving every revision already written exactly
// as it was written. Reshaping `messages` into an object would have silently
// given older rows a different meaning from newer ones.
func insertRevision(
	ctx context.Context, tx *sql.Tx, chapter *Chapter,
	messages []Message, entries []Entry, actorID uuid.UUID,
) error {
	var snapshot []byte
	if len(messages) > 0 {
		views := make([]MessageView, 0, len(messages))
		for _, message := range messages {
			views = append(views, message.View())
		}
		encoded, err := json.Marshal(views)
		if err != nil {
			return fmt.Errorf("encode message snapshot: %w", err)
		}
		snapshot = encoded
	}

	var entrySnapshot []byte
	if len(entries) > 0 {
		views := make([]EntryView, 0, len(entries))
		for _, entry := range entries {
			views = append(views, entry.View())
		}
		encoded, err := json.Marshal(views)
		if err != nil {
			return fmt.Errorf("encode entry snapshot: %w", err)
		}
		entrySnapshot = encoded
	}

	fields, err := encodeFields(chapter.EntryFields)
	if err != nil {
		return err
	}

	var createdBy *uuid.UUID
	if actorID != uuid.Nil {
		createdBy = &actorID
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO chapter_revisions (
			chapter_id, version, title, content, content_format, messages,
			entries, entry_fields, word_count, created_by
		)
		VALUES (
			$1,
			(SELECT COALESCE(MAX(version), 0) + 1 FROM chapter_revisions WHERE chapter_id = $1),
			$2, $3, $4, $5, $6, $7, $8, $9
		)`,
		chapter.ID, chapter.Title, chapter.Content,
		// The format the snapshotted text was WRITTEN under. Text restored under
		// the wrong one would render as something the author never wrote
		// (docs/CONTENT-MODEL.md §5).
		string(chapter.contentFormat()), snapshot,
		entrySnapshot, fields, chapter.WordCount, createdBy)
	if err != nil {
		return fmt.Errorf("record revision: %w", err)
	}
	return nil
}

// messageInsertBatch bounds one multi-row INSERT. PostgreSQL allows 65535
// parameters per statement; at seven columns a batch of 500 stays far below it
// however large MaxMessagesPerChapter later becomes.
const messageInsertBatch = 500

// replaceMessages makes the stored conversation equal to messages.
//
// Delete-then-insert rather than a per-row diff: positions are dense and
// reassigned from array order, so almost every row would move anyway, and the
// previous conversation is already preserved in the revision snapshot taken
// earlier in this same transaction.
func replaceMessages(ctx context.Context, tx *sql.Tx, chapterID uuid.UUID, messages []Message) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chapter_messages WHERE chapter_id = $1`, chapterID); err != nil {
		return fmt.Errorf("clear messages: %w", err)
	}
	if len(messages) == 0 {
		return nil
	}

	for start := 0; start < len(messages); start += messageInsertBatch {
		end := start + messageInsertBatch
		if end > len(messages) {
			end = len(messages)
		}

		var (
			placeholders = make([]string, 0, end-start)
			args         = make([]any, 0, (end-start)*7)
		)
		for _, message := range messages[start:end] {
			base := len(args)
			placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7))

			var metadata []byte
			if !message.Metadata.IsEmpty() {
				encoded, err := json.Marshal(message.Metadata)
				if err != nil {
					return fmt.Errorf("encode message metadata: %w", err)
				}
				metadata = encoded
			}

			args = append(args, chapterID, message.Position, message.SpeakerName,
				message.SpeakerAvatarURL, message.Type, message.Content, metadata)
		}

		query := `INSERT INTO chapter_messages
			(chapter_id, position, speaker_name, speaker_avatar_url, message_type, content, metadata)
			VALUES ` + strings.Join(placeholders, ", ")

		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert messages: %w", err)
		}
	}
	return nil
}

// SoftDelete marks a chapter deleted.
//
// Its messages and revisions are left in place. They are unreachable through the
// chapter, and hard-deleting an author's work on one API call is exactly the
// destructive behaviour the platform forbids (docs/08 §37).
func (r *Repository) SoftDelete(ctx context.Context, chapterID uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE chapters SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, chapterID)
	if err != nil {
		return fmt.Errorf("delete chapter: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete chapter: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// FindByIDIncludingDeleted loads a chapter by id alone, soft-deleted or not.
//
// It exists ONLY for the moderation path (docs/08 §24), which holds a bare
// chapter UUID from a report: restore has to see the removed row, and remove
// wants to answer "already removed" rather than "not found". The caller
// resolves the parent fiction itself - no visibility rule is applied here.
func (r *Repository) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*Chapter, error) {
	return scanChapter(r.db.QueryRowContext(ctx,
		`SELECT `+chapterColumns+` FROM chapters c WHERE c.id = $1`, id))
}

// FindByID is FindByIDIncludingDeleted's reader-side sibling: live rows only.
func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*Chapter, error) {
	return scanChapter(r.db.QueryRowContext(ctx,
		`SELECT `+chapterColumns+` FROM chapters c WHERE c.id = $1 AND c.deleted_at IS NULL`, id))
}

// Restore clears the soft delete - the moderator's counterpart of SoftDelete
// (docs/11 §39 "Moderator restored content").
func (r *Repository) Restore(ctx context.Context, chapterID uuid.UUID) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE chapters SET deleted_at = NULL, updated_at = now()
		WHERE id = $1 AND deleted_at IS NOT NULL`, chapterID)
	if err != nil {
		return fmt.Errorf("restore chapter: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("restore chapter: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// RevisionCount reports how many snapshots a chapter has, for tests and for the
// writer UI's "revision history" affordance (docs/01 §16).
func (r *Repository) RevisionCount(ctx context.Context, chapterID uuid.UUID) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM chapter_revisions WHERE chapter_id = $1`, chapterID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count revisions: %w", err)
	}
	return count, nil
}

// argList builds a positional-parameter list so every value is bound rather than
// interpolated (docs/11 §15).
type argList struct{ args []any }

func (a *argList) add(value any) string {
	a.args = append(a.args, value)
	return "$" + strconv.Itoa(len(a.args))
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// contentWords totals every representation the chapter holds.
//
// All three count, not only the active one: the number is what the writer is
// told they have written, and it must not drop because they switched which
// version readers see.
func contentWords(content *string, messages []Message, entries []Entry) int {
	total := CountWords(deref(content))
	for _, message := range messages {
		total += CountWords(message.Content)
	}
	for _, entry := range entries {
		total += CountWords(entry.Body)
	}
	return total
}

// formatValue renders a chapter's own format for the column: NULL when it
// follows the fiction.
func formatValue(format *fiction.PresentationFormat) any {
	if format == nil {
		return nil
	}
	return string(*format)
}

// encodeFields renders entry field labels as JSONB, never as SQL NULL - the
// column is NOT NULL and an absent list is an empty one.
func encodeFields(fields []string) ([]byte, error) {
	if fields == nil {
		fields = []string{}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode entry fields: %w", err)
	}
	return encoded, nil
}

// classifyConflict turns a UNIQUE violation into a typed error the service can
// retry, matching users.uniqueViolation's approach of naming constraints rather
// than importing driver-specific types.
func classifyConflict(err error) error {
	msg := err.Error()
	if !strings.Contains(msg, "SQLSTATE 23505") && !strings.Contains(msg, "duplicate key value") {
		return err
	}
	switch {
	case strings.Contains(msg, "chapters_novel_slug_key"):
		return ErrSlugTaken
	case strings.Contains(msg, "chapters_novel_number_key"):
		return ErrNumberTaken
	}
	return err
}
