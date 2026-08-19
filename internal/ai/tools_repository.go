package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ToolsRepository owns the 13Y tables: the word bank, the mutes, the
// evolution markers, the fact books, and the two preference tiers. Its reads
// into chapters/tags stay novel-scoped and run AFTER the service authorized
// the caller as the fiction's editor.
type ToolsRepository struct {
	db *sql.DB
}

func NewToolsRepository(db *sql.DB) *ToolsRepository { return &ToolsRepository{db: db} }

// ---------------------------------------------------------------------------
// Preferences
// ---------------------------------------------------------------------------

// UserPrefs loads the account tier; nil when never saved.
func (r *ToolsRepository) UserPrefs(ctx context.Context, userID uuid.UUID) (*Prefs, error) {
	return r.prefs(ctx, `SELECT prefs FROM ai_prefs WHERE user_id = $1`, userID)
}

// NovelPrefs loads the fiction tier; nil when never saved.
func (r *ToolsRepository) NovelPrefs(ctx context.Context, novelID uuid.UUID) (*Prefs, error) {
	return r.prefs(ctx, `SELECT prefs FROM ai_novel_prefs WHERE novel_id = $1`, novelID)
}

func (r *ToolsRepository) prefs(ctx context.Context, query string, id uuid.UUID) (*Prefs, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx, query, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load prefs: %w", err)
	}
	var prefs Prefs
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return nil, fmt.Errorf("decode prefs: %w", err)
	}
	return &prefs, nil
}

// SetUserPrefs upserts the account tier.
func (r *ToolsRepository) SetUserPrefs(ctx context.Context, userID uuid.UUID, prefs Prefs) error {
	return r.setPrefs(ctx,
		`INSERT INTO ai_prefs (user_id, prefs) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET prefs = $2, updated_at = now()`,
		userID, prefs)
}

// SetNovelPrefs upserts the fiction tier.
func (r *ToolsRepository) SetNovelPrefs(ctx context.Context, novelID uuid.UUID, prefs Prefs) error {
	return r.setPrefs(ctx,
		`INSERT INTO ai_novel_prefs (novel_id, prefs) VALUES ($1, $2)
		 ON CONFLICT (novel_id) DO UPDATE SET prefs = $2, updated_at = now()`,
		novelID, prefs)
}

func (r *ToolsRepository) setPrefs(ctx context.Context, query string, id uuid.UUID, prefs Prefs) error {
	encoded, err := json.Marshal(prefs)
	if err != nil {
		return fmt.Errorf("encode prefs: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, query, id, encoded); err != nil {
		return fmt.Errorf("save prefs: %w", err)
	}
	return nil
}

// PrefsOverrides names the caller's fictions that carry an override tier with
// anything actually set in it - what the account settings page lists so
// "ค่าเริ่มต้นของทุกเรื่อง" can say which stories do not follow it.
func (r *ToolsRepository) PrefsOverrides(
	ctx context.Context, authorID uuid.UUID,
) ([]PrefsOverride, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT n.title, n.slug FROM ai_novel_prefs p
		JOIN novels n ON n.id = p.novel_id
		WHERE n.author_id = $1 AND n.deleted_at IS NULL AND p.prefs <> '{}'::jsonb
		ORDER BY n.title`, authorID)
	if err != nil {
		return nil, fmt.Errorf("list prefs overrides: %w", err)
	}
	defer rows.Close()
	overrides := []PrefsOverride{}
	for rows.Next() {
		var override PrefsOverride
		if err := rows.Scan(&override.Title, &override.Slug); err != nil {
			return nil, fmt.Errorf("scan prefs override: %w", err)
		}
		overrides = append(overrides, override)
	}
	return overrides, rows.Err()
}

// ---------------------------------------------------------------------------
// The word bank
// ---------------------------------------------------------------------------

// LexiconTerms returns the fiction's custom terms - plus, when the fiction is
// part of a series, every sibling's terms (13Y §2: one fandom, taught once).
func (r *ToolsRepository) LexiconTerms(
	ctx context.Context, novelID, authorID uuid.UUID, seriesName *string,
) ([]LexiconTerm, error) {
	query := `SELECT id, term FROM novel_lexicon WHERE novel_id = $1 ORDER BY lower(term)`
	args := []any{novelID}
	if seriesName != nil && strings.TrimSpace(*seriesName) != "" {
		query = `
			SELECT l.id, l.term FROM novel_lexicon l
			WHERE l.novel_id = $1
			   OR l.novel_id IN (
					SELECT id FROM novels
					WHERE author_id = $2 AND series_name = $3 AND deleted_at IS NULL)
			ORDER BY lower(l.term)`
		args = []any{novelID, authorID, *seriesName}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list lexicon: %w", err)
	}
	defer rows.Close()

	terms := []LexiconTerm{}
	seen := map[string]bool{}
	for rows.Next() {
		var term LexiconTerm
		if err := rows.Scan(&term.ID, &term.Term); err != nil {
			return nil, fmt.Errorf("scan lexicon term: %w", err)
		}
		key := strings.ToLower(term.Term)
		if seen[key] {
			continue
		}
		seen[key] = true
		terms = append(terms, term)
	}
	return terms, rows.Err()
}

// AddLexiconTerm inserts a term; adding one that exists is a no-op.
func (r *ToolsRepository) AddLexiconTerm(ctx context.Context, novelID uuid.UUID, term string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO novel_lexicon (novel_id, term) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, novelID, term)
	if err != nil {
		return fmt.Errorf("add lexicon term: %w", err)
	}
	return nil
}

// RemoveLexiconTerm deletes one of THIS fiction's terms (a sibling's term is
// removed on the sibling).
func (r *ToolsRepository) RemoveLexiconTerm(ctx context.Context, novelID, termID uuid.UUID) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM novel_lexicon WHERE id = $1 AND novel_id = $2`, termID, novelID); err != nil {
		return fmt.Errorf("remove lexicon term: %w", err)
	}
	return nil
}

// UserLexiconTerms returns the writer's account-wide bank - the terms that
// apply in every fiction they write.
func (r *ToolsRepository) UserLexiconTerms(
	ctx context.Context, userID uuid.UUID,
) ([]LexiconTerm, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, term FROM user_lexicon WHERE user_id = $1 ORDER BY lower(term)`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user lexicon: %w", err)
	}
	defer rows.Close()
	terms := []LexiconTerm{}
	for rows.Next() {
		var term LexiconTerm
		if err := rows.Scan(&term.ID, &term.Term); err != nil {
			return nil, fmt.Errorf("scan user lexicon term: %w", err)
		}
		terms = append(terms, term)
	}
	return terms, rows.Err()
}

// AddUserLexiconTerm inserts an account-wide term; adding one that exists is a
// no-op.
func (r *ToolsRepository) AddUserLexiconTerm(ctx context.Context, userID uuid.UUID, term string) error {
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO user_lexicon (user_id, term) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, userID, term); err != nil {
		return fmt.Errorf("add user lexicon term: %w", err)
	}
	return nil
}

// RemoveUserLexiconTerm deletes one of the caller's own account-wide terms.
func (r *ToolsRepository) RemoveUserLexiconTerm(ctx context.Context, userID, termID uuid.UUID) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM user_lexicon WHERE id = $1 AND user_id = $2`, termID, userID); err != nil {
		return fmt.Errorf("remove user lexicon term: %w", err)
	}
	return nil
}

// TagNames returns the fiction's tag names for the auto bank.
func (r *ToolsRepository) TagNames(ctx context.Context, novelID uuid.UUID) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT t.name FROM novel_tags nt JOIN tags t ON t.id = nt.tag_id
		WHERE nt.novel_id = $1`, novelID)
	if err != nil {
		return nil, fmt.Errorf("list tag names: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tag name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// ---------------------------------------------------------------------------
// Mutes
// ---------------------------------------------------------------------------

// AddMute records a taught silence; teaching the same one twice is a no-op.
func (r *ToolsRepository) AddMute(
	ctx context.Context, userID uuid.UUID, novelID *uuid.UUID, kind, term string,
) error {
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO ai_mutes (user_id, novel_id, kind, term) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, userID, novelID, kind, term); err != nil {
		return fmt.Errorf("add mute: %w", err)
	}
	return nil
}

// ListMutes returns the caller's mutes.
//
// With a fiction named: the ones that APPLY there - global plus that
// fiction's, which is what the check path folds into its filter. With none:
// EVERY mute the caller has taught anywhere, each carrying its fiction's name
// - the account settings page's "กฎที่ปิดไว้" list, where a silence taught
// with one click months ago can finally be seen and un-taught.
func (r *ToolsRepository) ListMutes(
	ctx context.Context, userID uuid.UUID, novelID *uuid.UUID,
) ([]MuteView, error) {
	query := `
		SELECT m.id, m.kind, m.term, m.novel_id, n.title, n.slug
		FROM ai_mutes m
		LEFT JOIN novels n ON n.id = m.novel_id
		WHERE m.user_id = $1
		ORDER BY m.created_at DESC`
	args := []any{userID}
	if novelID != nil {
		query = `
			SELECT m.id, m.kind, m.term, m.novel_id, n.title, n.slug
			FROM ai_mutes m
			LEFT JOIN novels n ON n.id = m.novel_id
			WHERE m.user_id = $1 AND (m.novel_id IS NULL OR m.novel_id = $2)
			ORDER BY m.created_at DESC`
		args = append(args, novelID)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list mutes: %w", err)
	}
	defer rows.Close()
	mutes := []MuteView{}
	for rows.Next() {
		var mute MuteView
		if err := rows.Scan(&mute.ID, &mute.Kind, &mute.Term, &mute.Novel,
			&mute.NovelTitle, &mute.NovelSlug); err != nil {
			return nil, fmt.Errorf("scan mute: %w", err)
		}
		mutes = append(mutes, mute)
	}
	return mutes, rows.Err()
}

// MuteSet folds the caller's applicable mutes into a lookup set.
func (r *ToolsRepository) MuteSet(
	ctx context.Context, userID, novelID uuid.UUID,
) (map[string]bool, error) {
	id := novelID
	mutes, err := r.ListMutes(ctx, userID, &id)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, mute := range mutes {
		set[muteKey(mute.Kind, mute.Term)] = true
	}
	return set, nil
}

// RemoveMute deletes one of the caller's own mutes. Someone else's id removes
// nothing.
func (r *ToolsRepository) RemoveMute(ctx context.Context, userID, muteID uuid.UUID) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM ai_mutes WHERE id = $1 AND user_id = $2`, muteID, userID); err != nil {
		return fmt.Errorf("remove mute: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Evolution markers
// ---------------------------------------------------------------------------

// Evolutions returns the fiction's markers, keyed by character.
func (r *ToolsRepository) Evolutions(
	ctx context.Context, novelID uuid.UUID,
) (map[uuid.UUID]int, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT character_id, from_chapter_number FROM character_evolution
		WHERE novel_id = $1`, novelID)
	if err != nil {
		return nil, fmt.Errorf("list evolutions: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int{}
	for rows.Next() {
		var id uuid.UUID
		var from int
		if err := rows.Scan(&id, &from); err != nil {
			return nil, fmt.Errorf("scan evolution: %w", err)
		}
		out[id] = from
	}
	return out, rows.Err()
}

// SetEvolution upserts (from > 0) or clears (from == 0) a marker. The insert
// verifies the character belongs to this fiction; false = no such character.
func (r *ToolsRepository) SetEvolution(
	ctx context.Context, novelID, characterID uuid.UUID, from int,
) (bool, error) {
	if from == 0 {
		if _, err := r.db.ExecContext(ctx, `
			DELETE FROM character_evolution WHERE character_id = $1 AND novel_id = $2`,
			characterID, novelID); err != nil {
			return false, fmt.Errorf("clear evolution: %w", err)
		}
		return true, nil
	}
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO character_evolution (character_id, novel_id, from_chapter_number)
		SELECT c.id, c.novel_id, $3 FROM characters c
		WHERE c.id = $1 AND c.novel_id = $2
		ON CONFLICT (character_id) DO UPDATE SET from_chapter_number = $3`,
		characterID, novelID, from)
	if err != nil {
		return false, fmt.Errorf("set evolution: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set evolution: %w", err)
	}
	return affected > 0, nil
}

// ---------------------------------------------------------------------------
// Fact books
// ---------------------------------------------------------------------------

// Facts loads a chapter's fact book. ok=false when the chapter is not this
// fiction's (the caller answers 404).
func (r *ToolsRepository) Facts(
	ctx context.Context, novelID, chapterID uuid.UUID,
) ([]Fact, bool, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(f.facts, '[]'::jsonb)
		FROM chapters c
		LEFT JOIN chapter_facts f ON f.chapter_id = c.id
		WHERE c.id = $1 AND c.novel_id = $2 AND c.deleted_at IS NULL`,
		chapterID, novelID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load facts: %w", err)
	}
	facts := []Fact{}
	if err := json.Unmarshal(raw, &facts); err != nil {
		return nil, false, fmt.Errorf("decode facts: %w", err)
	}
	return facts, true, nil
}

// SetFacts replaces a chapter's fact book. ok=false when the chapter is not
// this fiction's.
func (r *ToolsRepository) SetFacts(
	ctx context.Context, novelID, chapterID uuid.UUID, facts []Fact,
) (bool, error) {
	encoded, err := json.Marshal(facts)
	if err != nil {
		return false, fmt.Errorf("encode facts: %w", err)
	}
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO chapter_facts (chapter_id, facts)
		SELECT c.id, $3::jsonb FROM chapters c
		WHERE c.id = $1 AND c.novel_id = $2 AND c.deleted_at IS NULL
		ON CONFLICT (chapter_id) DO UPDATE SET facts = $3::jsonb, updated_at = now()`,
		chapterID, novelID, encoded)
	if err != nil {
		return false, fmt.Errorf("save facts: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("save facts: %w", err)
	}
	return affected > 0, nil
}

// earlierFact is one fact from a previous chapter, with where it came from.
type earlierFact struct {
	Value   string
	Chapter int
}

// PreviousFacts merges the fact books of every chapter BEFORE the given one
// (by chapter number), latest statement winning, headcanon chapters excluded
// (13Y §6). novelFormat resolves chapters that follow the fiction.
func (r *ToolsRepository) PreviousFacts(
	ctx context.Context, novelID, chapterID uuid.UUID, novelFormat string,
) (map[string]earlierFact, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.chapter_number, f.facts
		FROM chapters c
		JOIN chapter_facts f ON f.chapter_id = c.id
		WHERE c.novel_id = $1 AND c.deleted_at IS NULL
		  AND COALESCE(c.presentation_format, $3) <> 'headcanon'
		  AND c.chapter_number < (
			SELECT chapter_number FROM chapters WHERE id = $2)
		ORDER BY c.chapter_number ASC`, novelID, chapterID, novelFormat)
	if err != nil {
		return nil, fmt.Errorf("load previous facts: %w", err)
	}
	defer rows.Close()

	merged := map[string]earlierFact{}
	for rows.Next() {
		var number int
		var raw []byte
		if err := rows.Scan(&number, &raw); err != nil {
			return nil, fmt.Errorf("scan previous facts: %w", err)
		}
		var facts []Fact
		if err := json.Unmarshal(raw, &facts); err != nil {
			continue
		}
		for _, fact := range facts {
			merged[strings.ToLower(strings.TrimSpace(fact.Label))] = earlierFact{
				Value: fact.Value, Chapter: number,
			}
		}
	}
	return merged, rows.Err()
}

// ---------------------------------------------------------------------------
// Chapter text for the precheck
// ---------------------------------------------------------------------------

// ChapterText is what the precheck analyzes: the chapter's prose plus its
// chat lines, its resolved mode, and its number.
type chapterText struct {
	Text   string
	Mode   string
	Number int
}

// ChapterText loads one chapter's analyzable text (novel-scoped; the service
// authorized the caller first).
func (r *ToolsRepository) ChapterText(
	ctx context.Context, novelID, chapterID uuid.UUID,
) (chapterText, bool, error) {
	var out chapterText
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(c.content, ''),
		       COALESCE(c.presentation_format, n.presentation_format),
		       c.chapter_number
		FROM chapters c JOIN novels n ON n.id = c.novel_id
		WHERE c.id = $1 AND c.novel_id = $2 AND c.deleted_at IS NULL`,
		chapterID, novelID).Scan(&out.Text, &out.Mode, &out.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return chapterText{}, false, nil
	}
	if err != nil {
		return chapterText{}, false, fmt.Errorf("load chapter text: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT content FROM chapter_messages WHERE chapter_id = $1 ORDER BY position`,
		chapterID)
	if err != nil {
		return chapterText{}, false, fmt.Errorf("load chapter messages: %w", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return chapterText{}, false, fmt.Errorf("scan message: %w", err)
		}
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if err := rows.Err(); err != nil {
		return chapterText{}, false, err
	}
	if len(lines) > 0 {
		if out.Text != "" {
			out.Text += "\n"
		}
		out.Text += strings.Join(lines, "\n")
	}
	return out, true, nil
}

// ---------------------------------------------------------------------------
// Search (13Y §8)
// ---------------------------------------------------------------------------

// Search scans the fiction's prose, chat lines, entry bodies, and titles for
// a literal query, drafts included.
func (r *ToolsRepository) Search(
	ctx context.Context, novelID uuid.UUID, query string, limit int,
) ([]SearchHit, error) {
	pattern := "%" + escapeLike(query) + "%"

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, slug, chapter_number, title, status, 'title', COALESCE(title, '')
		FROM chapters
		WHERE novel_id = $1 AND deleted_at IS NULL AND title ILIKE $2 ESCAPE '\'

		UNION ALL
		SELECT id, slug, chapter_number, title, status, 'prose', COALESCE(content, '')
		FROM chapters
		WHERE novel_id = $1 AND deleted_at IS NULL AND content ILIKE $2 ESCAPE '\'

		UNION ALL
		SELECT c.id, c.slug, c.chapter_number, c.title, c.status, 'chat', m.content
		FROM chapter_messages m JOIN chapters c ON c.id = m.chapter_id
		WHERE c.novel_id = $1 AND c.deleted_at IS NULL AND m.content ILIKE $2 ESCAPE '\'

		UNION ALL
		SELECT c.id, c.slug, c.chapter_number, c.title, c.status, 'entry',
		       e.name || ' - ' || e.body
		FROM chapter_entries e JOIN chapters c ON c.id = e.chapter_id
		WHERE c.novel_id = $1 AND c.deleted_at IS NULL
		  AND (e.body ILIKE $2 ESCAPE '\' OR e.name ILIKE $2 ESCAPE '\')

		LIMIT $3`, novelID, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("search chapters: %w", err)
	}
	defer rows.Close()

	hits := []SearchHit{}
	for rows.Next() {
		var hit SearchHit
		var haystack string
		if err := rows.Scan(&hit.ChapterID, &hit.Slug, &hit.ChapterNumber,
			&hit.Title, &hit.Status, &hit.Where, &haystack); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		hit.Snippet = snippetAround(haystack, query, 60)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// escapeLike neutralises LIKE metacharacters in user input.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// snippetAround extracts ±window runes around the first case-insensitive
// occurrence of query.
func snippetAround(text, query string, window int) string {
	lower := strings.ToLower(text)
	at := strings.Index(lower, strings.ToLower(query))
	if at < 0 {
		return truncateRunes(strings.TrimSpace(text), window*2)
	}
	runes := []rune(text)
	// Convert the byte offset to a rune offset.
	runeAt := len([]rune(text[:at]))
	start := runeAt - window
	if start < 0 {
		start = 0
	}
	end := runeAt + len([]rune(query)) + window
	if end > len(runes) {
		end = len(runes)
	}
	snippet := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return snippet
}
