package ai

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/ai/thai"
	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/characters"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/variables"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// The writing-tools round (13Y). Six tools, three cost classes - never
// promised as one system:
//
//   - live checks (spelling/grammar, polish): local rules, mode-aware,
//     filtered through the fiction's own word bank and the writer's mutes;
//   - round checks (character consistency, continuity): run when the writer
//     pauses or asks, never mid-keystroke, and every finding CITES its source;
//   - separate tools (search): user-invoked.
//
// Everything here proposes; nothing edits. The platform's standing answer to
// "is my work used to train AI" is NO - checks run in-process on platform
// rules, nothing is sent to an external service, nothing is retained beyond
// the response (docs/12 §2; restated verbatim in the settings UI).

// Field bounds.
const (
	LexiconTermMaxRunes = 120
	MuteTermMaxRunes    = 200
	MaxFactRows         = 100
	FactLabelMaxRunes   = 80
	FactValueMaxRunes   = 500
	MaxSearchResults    = 30
	SearchQueryMaxRunes = 120

	// minCheckRunes is the floor under every round check: a chapter shorter
	// than this has nothing to check (13Y §12).
	minCheckRunes = 20

	// maxIssuesPerParagraph caps live-check output per paragraph; the excess
	// is summarised, not shown (13Y §2 "หน้าที่ขีดแดงทั้งหน้า").
	maxIssuesPerParagraph = 3
)

// ---------------------------------------------------------------------------
// Consumer-defined slivers of the neighbouring domains
// ---------------------------------------------------------------------------

// NovelSource gates every tool to the fiction's EDITORS (owner or
// collaborator, 13U) and supplies the metadata the auto word-bank derives
// from. Authorization stays inside the novels service.
type NovelSource interface {
	ForEditor(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
}

// CastSource is the characters domain's own listing - the character sheets
// the consistency check cites (13Y §5).
type CastSource interface {
	List(ctx context.Context, identity *auth.Identity, ref novels.Ref) ([]characters.View, error)
}

// VariableSource supplies the fiction's reader-variable tokens (y/n, l/n…)
// for the auto word bank.
type VariableSource interface {
	List(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*variables.Result, error)
}

// ---------------------------------------------------------------------------
// Preferences (13Y §10) - two persisted tiers plus derived effective values
// ---------------------------------------------------------------------------

// Prefs are the assistant switches. Pointers so a tier only overrides what it
// SET; nil falls through to the tier below.
type Prefs struct {
	// Assistant is the account/fiction master switch. Off means every tool
	// answers empty and the editor shows "ปิดอยู่".
	Assistant *bool `json:"assistant,omitempty"`
	// Spell is ตรวจคำผิดและไวยากรณ์ (default ON).
	Spell *bool `json:"spell,omitempty"`
	// Character is ตรวจความสอดคล้องของตัวละคร (default ON).
	Character *bool `json:"character,omitempty"`
	// Continuity is ตรวจความต่อเนื่อง (default OFF - the expensive one).
	Continuity *bool `json:"continuity,omitempty"`
	// Polish is เกลาภาษา (default ON, softest level only).
	Polish *bool `json:"polish,omitempty"`
}

// EffectivePrefs is every switch resolved: defaults ← user ← novel.
type EffectivePrefs struct {
	Assistant  bool `json:"assistant"`
	Spell      bool `json:"spell"`
	Character  bool `json:"character"`
	Continuity bool `json:"continuity"`
	Polish     bool `json:"polish"`
}

func defaultPrefs() EffectivePrefs {
	return EffectivePrefs{Assistant: true, Spell: true, Character: true, Continuity: false, Polish: true}
}

func (e *EffectivePrefs) apply(p *Prefs) {
	if p == nil {
		return
	}
	if p.Assistant != nil {
		e.Assistant = *p.Assistant
	}
	if p.Spell != nil {
		e.Spell = *p.Spell
	}
	if p.Character != nil {
		e.Character = *p.Character
	}
	if p.Continuity != nil {
		e.Continuity = *p.Continuity
	}
	if p.Polish != nil {
		e.Polish = *p.Polish
	}
}

// ---------------------------------------------------------------------------
// Tools service
// ---------------------------------------------------------------------------

// Tools owns the 13Y features. A separate service from the Phase-10 request
// pipeline on purpose: these are interactive editor calls with their own
// dependencies, and the older service keeps its narrower wiring.
type Tools struct {
	repo      *ToolsRepository
	provider  Provider
	novels    NovelSource
	cast      CastSource
	variables VariableSource
	enabled   bool
	maxRunes  int
	log       *slog.Logger

	// model is the optional consistency sidecar
	// (docs/AI-CONSISTENCY-MODEL.md). nil = rules only, the default.
	model *ModelClient

	// achievements is optional and set after construction. nil records nothing.
	achievements Achiever
}

// SetAchiever attaches the achievement service after construction.
func (t *Tools) SetAchiever(achiever Achiever) { t.achievements = achiever }

func NewTools(
	repo *ToolsRepository, provider Provider,
	novelSource NovelSource, castSource CastSource, variableSource VariableSource,
	cfg Config, log *slog.Logger,
) *Tools {
	return &Tools{
		repo: repo, provider: provider,
		novels: novelSource, cast: castSource, variables: variableSource,
		enabled: cfg.Enabled, maxRunes: cfg.MaxInputRunes, log: log,
		model: NewModelClient(cfg.ModelURL),
	}
}

// SetModelClient swaps the consistency sidecar client. Tests use it to point
// the check at a fake sidecar; nil returns the check to rules only.
func (t *Tools) SetModelClient(client *ModelClient) { t.model = client }

func (t *Tools) internal(op string, err error) error {
	t.log.Error("ai tools: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

// ---------------------------------------------------------------------------
// Preferences
// ---------------------------------------------------------------------------

// PrefsView is the layered answer the settings surfaces render.
type PrefsView struct {
	User      *Prefs         `json:"user"`
	Novel     *Prefs         `json:"novel,omitempty"`
	Effective EffectivePrefs `json:"effective"`
	// Overrides names the caller's fictions whose override tier actually sets
	// something. Present on the ACCOUNT view only - it is what lets that page
	// say "ค่าเริ่มต้นของทุกเรื่อง ยกเว้นเรื่องเหล่านี้" instead of leaving the
	// writer to wonder why one story behaves differently.
	Overrides []PrefsOverride `json:"overrides,omitempty"`
}

// PrefsOverride names one fiction that overrides the account defaults.
type PrefsOverride struct {
	Title string `json:"title"`
	Slug  string `json:"slug"`
}

// GetPrefs resolves the caller's assistant switches, optionally against one
// fiction (whose override tier then applies).
func (t *Tools) GetPrefs(
	ctx context.Context, identity *auth.Identity, novelRef string,
) (PrefsView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return PrefsView{}, err
	}
	userPrefs, err := t.repo.UserPrefs(ctx, userID)
	if err != nil {
		return PrefsView{}, t.internal("load user prefs", err)
	}

	view := PrefsView{User: userPrefs}
	effective := defaultPrefs()
	effective.apply(userPrefs)

	if strings.TrimSpace(novelRef) != "" {
		novel, err := t.editorNovel(ctx, identity, novelRef)
		if err != nil {
			return PrefsView{}, err
		}
		novelPrefs, err := t.repo.NovelPrefs(ctx, novel.ID)
		if err != nil {
			return PrefsView{}, t.internal("load novel prefs", err)
		}
		view.Novel = novelPrefs
		effective.apply(novelPrefs)
	} else {
		// The account view carries the override list. Advisory: a failure to
		// load it must not take the switches down with it.
		overrides, err := t.repo.PrefsOverrides(ctx, userID)
		if err != nil {
			t.log.Warn("ai tools: list prefs overrides failed", slog.Any("error", err))
		} else {
			view.Overrides = overrides
		}
	}
	view.Effective = effective
	return view, nil
}

// SetPrefs writes one tier: the caller's own defaults, or one fiction's
// override (editor-gated).
func (t *Tools) SetPrefs(
	ctx context.Context, identity *auth.Identity, novelRef string, prefs Prefs,
) (PrefsView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return PrefsView{}, err
	}
	if strings.TrimSpace(novelRef) == "" {
		if err := t.repo.SetUserPrefs(ctx, userID, prefs); err != nil {
			return PrefsView{}, t.internal("save user prefs", err)
		}
	} else {
		novel, err := t.editorNovel(ctx, identity, novelRef)
		if err != nil {
			return PrefsView{}, err
		}
		if err := t.repo.SetNovelPrefs(ctx, novel.ID, prefs); err != nil {
			return PrefsView{}, t.internal("save novel prefs", err)
		}
	}
	return t.GetPrefs(ctx, identity, novelRef)
}

// ---------------------------------------------------------------------------
// The word bank (13Y §2)
// ---------------------------------------------------------------------------

// LexiconView is the fiction's word bank: the writer's own terms plus the
// auto part derived live from the cast, the reader variables, and the
// fiction's fandom/tags. Auto terms have no id - they are not rows and cannot
// be deleted here; they change when their source changes.
//
// Account carries the author's account-wide terms, which apply here too but
// are managed on the account settings page - shown so a writer is never left
// asking why a word is not being flagged.
type LexiconView struct {
	Custom  []LexiconTerm `json:"custom"`
	Account []LexiconTerm `json:"account"`
	Auto    []string      `json:"auto"`
}

// LexiconTerm is one writer-added term.
type LexiconTerm struct {
	ID   uuid.UUID `json:"id"`
	Term string    `json:"term"`
}

// Lexicon lists the fiction's word bank.
func (t *Tools) Lexicon(
	ctx context.Context, identity *auth.Identity, novelRef string,
) (LexiconView, error) {
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return LexiconView{}, err
	}
	custom, err := t.repo.LexiconTerms(ctx, novel.ID, novel.AuthorID, novel.Extras.SeriesName)
	if err != nil {
		return LexiconView{}, t.internal("load lexicon", err)
	}
	account, err := t.repo.UserLexiconTerms(ctx, novel.AuthorID)
	if err != nil {
		return LexiconView{}, t.internal("load user lexicon", err)
	}
	auto, err := t.autoLexicon(ctx, identity, novelRef, novel)
	if err != nil {
		return LexiconView{}, err
	}
	return LexiconView{Custom: custom, Account: account, Auto: auto}, nil
}

// ---------------------------------------------------------------------------
// The account word bank (assistant-settings review, 2026-08)
// ---------------------------------------------------------------------------

// UserLexiconView is the account-wide bank on its own: no auto part, because
// the auto terms are derived per fiction from that fiction's cast and tags.
type UserLexiconView struct {
	Terms []LexiconTerm `json:"terms"`
}

// UserLexicon lists the caller's account-wide word bank.
func (t *Tools) UserLexicon(
	ctx context.Context, identity *auth.Identity,
) (UserLexiconView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return UserLexiconView{}, err
	}
	terms, err := t.repo.UserLexiconTerms(ctx, userID)
	if err != nil {
		return UserLexiconView{}, t.internal("load user lexicon", err)
	}
	return UserLexiconView{Terms: terms}, nil
}

// AddUserLexiconTerm teaches every fiction the caller writes a word - the
// fandom's proper nouns, taught once instead of once per story. Same rules as
// the per-fiction bank.
func (t *Tools) AddUserLexiconTerm(
	ctx context.Context, identity *auth.Identity, term string,
) (UserLexiconView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return UserLexiconView{}, err
	}
	trimmed := strings.TrimSpace(term)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > LexiconTermMaxRunes ||
		strings.ContainsAny(trimmed, "\n\r") {
		return UserLexiconView{}, apierror.Validation(map[string][]string{
			"term": {"A term must be a single line of at most 120 characters."},
		})
	}
	if err := t.repo.AddUserLexiconTerm(ctx, userID, trimmed); err != nil {
		return UserLexiconView{}, t.internal("add user lexicon term", err)
	}
	return t.UserLexicon(ctx, identity)
}

// RemoveUserLexiconTerm forgets one account-wide term.
func (t *Tools) RemoveUserLexiconTerm(
	ctx context.Context, identity *auth.Identity, termID uuid.UUID,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	if err := t.repo.RemoveUserLexiconTerm(ctx, userID, termID); err != nil {
		return t.internal("remove user lexicon term", err)
	}
	return nil
}

// AddLexiconTerm teaches the fiction a word ("เพิ่มคำนี้ในคลังของเรื่อง").
func (t *Tools) AddLexiconTerm(
	ctx context.Context, identity *auth.Identity, novelRef, term string,
) (LexiconView, error) {
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return LexiconView{}, err
	}
	trimmed := strings.TrimSpace(term)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > LexiconTermMaxRunes ||
		strings.ContainsAny(trimmed, "\n\r") {
		return LexiconView{}, apierror.Validation(map[string][]string{
			"term": {"A term must be a single line of at most 120 characters."},
		})
	}
	if err := t.repo.AddLexiconTerm(ctx, novel.ID, trimmed); err != nil {
		return LexiconView{}, t.internal("add lexicon term", err)
	}
	return t.Lexicon(ctx, identity, novelRef)
}

// RemoveLexiconTerm forgets a custom term.
func (t *Tools) RemoveLexiconTerm(
	ctx context.Context, identity *auth.Identity, novelRef string, termID uuid.UUID,
) error {
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return err
	}
	if err := t.repo.RemoveLexiconTerm(ctx, novel.ID, termID); err != nil {
		return t.internal("remove lexicon term", err)
	}
	return nil
}

// autoLexicon derives the terms the fiction already declared elsewhere:
// character names (and each word of them), reader-variable tokens, the
// fandom, and tag names. Fanfiction proper nouns stop being "typos" without
// the writer teaching anything (13Y §2).
func (t *Tools) autoLexicon(
	ctx context.Context, identity *auth.Identity, novelRef string, novel *novels.Novel,
) ([]string, error) {
	seen := map[string]bool{}
	// An EMPTY slice, never a nil one: a nil slice marshals to JSON `null`, and
	// a brand-new fiction has no cast, no variables and no tags - so the word
	// bank of every fiction on its first day was `"auto": null`, which the
	// settings page then read `.length` from and crashed on.
	out := []string{}
	add := func(term string) {
		trimmed := strings.TrimSpace(term)
		// A word that arrived WRAPPED in brackets - the "(Zhongli)" half of a
		// name like "จงหลี่ (Zhongli)" after splitting on spaces - is the word
		// inside them. Only balanced outer pairs are stripped, so a full name
		// with brackets in the middle stays exactly as the writer spelled it.
		for len(trimmed) >= 2 &&
			((trimmed[0] == '(' && trimmed[len(trimmed)-1] == ')') ||
				(trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']')) {
			trimmed = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		}
		if trimmed == "" || utf8.RuneCountInString(trimmed) < 2 {
			return
		}
		key := strings.ToLower(trimmed)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, trimmed)
	}

	ref, err := novels.ParseRef(novelRef)
	if err == nil {
		if cast, err := t.cast.List(ctx, identity, ref); err == nil {
			for _, member := range cast {
				add(member.Name)
				for _, word := range strings.Fields(member.Name) {
					add(word)
				}
			}
		}
		if vars, err := t.variables.List(ctx, identity, ref); err == nil && vars != nil {
			for _, v := range vars.Variables {
				add(v.Token)
			}
		}
	}
	if novel.Fandom != nil {
		add(*novel.Fandom)
		for _, word := range strings.Fields(*novel.Fandom) {
			add(word)
		}
	}
	// Tags are attached by the novels service to its VIEWS, not the row this
	// gate returns - read the names directly, novel-scoped.
	if names, err := t.repo.TagNames(ctx, novel.ID); err == nil {
		for _, name := range names {
			add(name)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mutes - "ไม่เตือนแบบนี้อีก" (13Y §4)
// ---------------------------------------------------------------------------

// MuteView is one taught silence. The novel fields are set when the silence
// is scoped to one fiction, so the account-wide list can say WHERE each one
// applies rather than making the writer decode a UUID.
type MuteView struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	Term       string     `json:"term"`
	Novel      *uuid.UUID `json:"novel_id,omitempty"`
	NovelTitle *string    `json:"novel_title,omitempty"`
	NovelSlug  *string    `json:"novel_slug,omitempty"`
}

// AddMute silences one rule family for one term - for this fiction, or (with
// no fiction) everywhere the caller writes.
func (t *Tools) AddMute(
	ctx context.Context, identity *auth.Identity, novelRef, kind, term string,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	kind = strings.TrimSpace(kind)
	if !isKnownInlineType(kind) {
		return apierror.Validation(map[string][]string{"kind": {"Unknown suggestion kind."}})
	}
	trimmed := strings.TrimSpace(term)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > MuteTermMaxRunes {
		return apierror.Validation(map[string][]string{"term": {"A term is required (at most 200 characters)."}})
	}
	var novelID *uuid.UUID
	if strings.TrimSpace(novelRef) != "" {
		novel, err := t.editorNovel(ctx, identity, novelRef)
		if err != nil {
			return err
		}
		novelID = &novel.ID
	}
	if err := t.repo.AddMute(ctx, userID, novelID, kind, trimmed); err != nil {
		return t.internal("add mute", err)
	}
	// ไม่เชื่อ AI (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3).
	if t.achievements != nil {
		t.achievements.SuggestionMuted(ctx, userID)
	}
	return nil
}

// ListMutes returns the caller's silences (global + this fiction's).
func (t *Tools) ListMutes(
	ctx context.Context, identity *auth.Identity, novelRef string,
) ([]MuteView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	var novelID *uuid.UUID
	if strings.TrimSpace(novelRef) != "" {
		novel, err := t.editorNovel(ctx, identity, novelRef)
		if err != nil {
			return nil, err
		}
		novelID = &novel.ID
	}
	mutes, err := t.repo.ListMutes(ctx, userID, novelID)
	if err != nil {
		return nil, t.internal("list mutes", err)
	}
	return mutes, nil
}

// RemoveMute un-teaches one silence.
func (t *Tools) RemoveMute(
	ctx context.Context, identity *auth.Identity, muteID uuid.UUID,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	if err := t.repo.RemoveMute(ctx, userID, muteID); err != nil {
		return t.internal("remove mute", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The live check (13Y §1-§4): mode-aware, lexicon-filtered, capped
// ---------------------------------------------------------------------------

// CheckInput is one live-check call from the editor.
type CheckInput struct {
	NovelRef string
	// Mode is the chapter's ACTIVE format: standard / chat / headcanon. The
	// rules change per mode (13Y §2) - chat keeps only confident spelling,
	// headcanon skips repetition/polish (bullet style is not a fault).
	Mode string
	Text string
}

// ParagraphOverflow says a paragraph's issues were collapsed, not that they
// do not exist (no silent caps).
type ParagraphOverflow struct {
	Paragraph int `json:"paragraph"`
	Hidden    int `json:"hidden"`
}

// CheckResult is the live check's answer.
type CheckResult struct {
	// Disabled reports the master switch is off - said explicitly, so the
	// editor can show "ปิดอยู่" rather than an eternal spinner.
	Disabled    bool                `json:"disabled,omitempty"`
	Suggestions []InlineSuggestion  `json:"suggestions"`
	Overflow    []ParagraphOverflow `json:"overflow,omitempty"`
}

// Check runs the live rules over editor text with the fiction's word bank,
// the writer's mutes, and the chapter mode applied. Nothing is persisted.
func (t *Tools) Check(
	ctx context.Context, identity *auth.Identity, input CheckInput,
) (CheckResult, error) {
	if !t.enabled {
		return CheckResult{}, unavailable()
	}
	userID, err := requireUser(identity)
	if err != nil {
		return CheckResult{}, err
	}
	novel, err := t.editorNovel(ctx, identity, input.NovelRef)
	if err != nil {
		return CheckResult{}, err
	}

	prefs, err := t.effectiveFor(ctx, userID, novel.ID)
	if err != nil {
		return CheckResult{}, err
	}
	if !prefs.Assistant {
		return CheckResult{Disabled: true, Suggestions: []InlineSuggestion{}}, nil
	}

	text := input.Text
	if utf8.RuneCountInString(text) < minCheckRunes {
		return CheckResult{Suggestions: []InlineSuggestion{}}, nil
	}
	if len([]rune(text)) > t.maxRunes {
		text = string([]rune(text)[:t.maxRunes])
	}

	kinds := kindsFor(input.Mode, prefs)
	if len(kinds) == 0 {
		return CheckResult{Suggestions: []InlineSuggestion{}}, nil
	}
	result, err := t.provider.Analyze(ctx, AnalyzeInput{Text: text, Kinds: kinds})
	if err != nil {
		t.log.Warn("ai tools: provider failure", slog.String("op", "check"))
		return CheckResult{}, unavailable()
	}

	lexicon, err := t.lexiconSet(ctx, identity, input.NovelRef, novel)
	if err != nil {
		return CheckResult{}, err
	}
	mutes, err := t.repo.MuteSet(ctx, userID, novel.ID)
	if err != nil {
		return CheckResult{}, t.internal("load mutes", err)
	}

	kept := make([]InlineSuggestion, 0, len(result.Suggestions))
	for _, sug := range result.Suggestions {
		if sug.End > len([]rune(text)) || sug.Start < 0 || sug.End < sug.Start {
			continue
		}
		// Chat dialogue keeps only what the rules are SURE of: intentional
		// misspelling is characterisation there (13Y §2).
		if input.Mode == "chat" && sug.Severity != string(thai.High) {
			continue
		}
		term := strings.ToLower(strings.TrimSpace(sug.Original))
		if sug.Type == SuggestionSpelling && lexicon[term] {
			continue
		}
		if mutes[muteKey(sug.Type, term)] {
			continue
		}
		kept = append(kept, sug)
	}

	capped, overflow := capPerParagraph(text, kept)
	return CheckResult{Suggestions: capped, Overflow: overflow}, nil
}

// kindsFor maps the chapter mode and the writer's switches onto rule kinds.
func kindsFor(mode string, prefs EffectivePrefs) []string {
	var kinds []string
	switch mode {
	case "chat":
		// Spoken register: no punctuation rules, no repetition, NO polish -
		// a character's voice is not an error (13Y §2, §7).
		if prefs.Spell {
			kinds = append(kinds, SuggestionSpelling)
		}
	case "headcanon":
		// Phrase/bullet style: spelling and punctuation only.
		if prefs.Spell {
			kinds = append(kinds, SuggestionSpelling, SuggestionPunctuation)
		}
	default:
		if prefs.Spell {
			kinds = append(kinds, SuggestionSpelling, SuggestionPunctuation)
		}
		if prefs.Polish {
			kinds = append(kinds, SuggestionRepetition, SuggestionPolish)
		}
	}
	return kinds
}

// capPerParagraph keeps at most maxIssuesPerParagraph issues per paragraph
// and reports what it collapsed.
func capPerParagraph(text string, in []InlineSuggestion) ([]InlineSuggestion, []ParagraphOverflow) {
	// Paragraph index per rune offset.
	paragraphAt := make([]int, 0, 64)
	index := 0
	starts := []int{0}
	for i, r := range []rune(text) {
		if r == '\n' {
			index++
			starts = append(starts, i+1)
		}
		_ = i
	}
	_ = paragraphAt

	paragraphOf := func(offset int) int {
		at := 0
		for i, s := range starts {
			if offset >= s {
				at = i
			}
		}
		return at
	}

	counts := map[int]int{}
	hidden := map[int]int{}
	out := make([]InlineSuggestion, 0, len(in))
	for _, sug := range in {
		p := paragraphOf(sug.Start)
		if counts[p] >= maxIssuesPerParagraph {
			hidden[p]++
			continue
		}
		counts[p]++
		out = append(out, sug)
	}

	var overflow []ParagraphOverflow
	for p, n := range hidden {
		overflow = append(overflow, ParagraphOverflow{Paragraph: p, Hidden: n})
	}
	return out, overflow
}

// lexiconSet folds the custom + account + auto bank into a lookup set.
//
// The account bank is the AUTHOR's, not the caller's: a collaborator editing
// someone's fiction writes under that fiction's vocabulary, and the author's
// taught terms must not flag as typos depending on who is typing.
func (t *Tools) lexiconSet(
	ctx context.Context, identity *auth.Identity, novelRef string, novel *novels.Novel,
) (map[string]bool, error) {
	custom, err := t.repo.LexiconTerms(ctx, novel.ID, novel.AuthorID, novel.Extras.SeriesName)
	if err != nil {
		return nil, t.internal("load lexicon", err)
	}
	account, err := t.repo.UserLexiconTerms(ctx, novel.AuthorID)
	if err != nil {
		return nil, t.internal("load user lexicon", err)
	}
	auto, err := t.autoLexicon(ctx, identity, novelRef, novel)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, term := range custom {
		set[strings.ToLower(term.Term)] = true
	}
	for _, term := range account {
		set[strings.ToLower(term.Term)] = true
	}
	for _, term := range auto {
		set[strings.ToLower(term)] = true
	}
	return set, nil
}

func muteKey(kind, term string) string { return kind + "\x00" + strings.ToLower(term) }

// effectiveFor resolves prefs by ids (no re-authorization - callers did that).
func (t *Tools) effectiveFor(
	ctx context.Context, userID, novelID uuid.UUID,
) (EffectivePrefs, error) {
	userPrefs, err := t.repo.UserPrefs(ctx, userID)
	if err != nil {
		return EffectivePrefs{}, t.internal("load user prefs", err)
	}
	novelPrefs, err := t.repo.NovelPrefs(ctx, novelID)
	if err != nil {
		return EffectivePrefs{}, t.internal("load novel prefs", err)
	}
	effective := defaultPrefs()
	effective.apply(userPrefs)
	effective.apply(novelPrefs)
	return effective, nil
}

func (t *Tools) editorNovel(
	ctx context.Context, identity *auth.Identity, novelRef string,
) (*novels.Novel, error) {
	ref, err := novels.ParseRef(strings.TrimSpace(novelRef))
	if err != nil {
		return nil, apierror.Validation(map[string][]string{
			"novel": {"A novel id or slug is required."},
		})
	}
	return t.novels.ForEditor(ctx, identity, ref)
}

// ---------------------------------------------------------------------------
// Character consistency (13Y §5)
// ---------------------------------------------------------------------------

// CharacterIssue is one possible inconsistency, WITH its source - a finding
// that cannot cite the sheet it contradicts is not shown at all.
type CharacterIssue struct {
	CharacterID   uuid.UUID `json:"character_id"`
	CharacterName string    `json:"character_name"`
	// Field and FieldValue quote the character sheet ("ลักษณะนิสัย: เก็บความรู้สึก").
	Field      string `json:"field"`
	FieldValue string `json:"field_value"`
	// Quote is the manuscript line in question.
	Quote string `json:"quote"`
	// Explanation asks, never judges: "อาจไม่ตรงกับ…".
	Explanation string `json:"explanation"`
	Severity    string `json:"severity"`
}

// SkippedCharacter names a cast member the check could NOT examine, and why -
// which is also the nudge back to the character page (13Y §5).
type SkippedCharacter struct {
	CharacterID uuid.UUID `json:"character_id"`
	Name        string    `json:"name"`
	Reason      string    `json:"reason"`
}

// CharacterCheckResult is the whole answer: findings plus honest coverage.
type CharacterCheckResult struct {
	Total     int                `json:"total"`
	Checkable int                `json:"checkable"`
	Skipped   []SkippedCharacter `json:"skipped"`
	Issues    []CharacterIssue   `json:"issues"`
	// ModelPending is how many attributed lines the model sidecar has queued
	// but not scored yet (it scores asynchronously on writer hardware). The
	// panel keeps following up while this is non-zero, so a writer who pastes
	// a long scene and stops typing still gets the late findings.
	ModelPending int `json:"model_pending"`
}

// traitContradictions maps a sheet trait (matched as a substring of the trait
// list) to manuscript words that plausibly contradict it. Deliberately small
// and one-directional; every hit is phrased as a question and cites both
// sides. This is the rule-based floor - a model provider can replace the
// mechanism behind the same response shape.
var traitContradictions = map[string][]string{
	"เก็บความรู้สึก": {"ตะโกน", "โวยวาย", "กรีดร้อง", "ระเบิดอารมณ์"},
	"เงียบขรึม":      {"ตะโกน", "โวยวาย", "พูดไม่หยุด", "เจื้อยแจ้ว"},
	"ใจเย็น":         {"โมโหจัด", "ตวาด", "เหวี่ยง", "ระเบิดอารมณ์"},
	"สุขุม":          {"ตะโกน", "โวยวาย", "ตีโพยตีพาย", "ลนลาน", "กรี๊ด", "แผดเสียง"},
	"เยือกเย็น":      {"โวยวาย", "ลนลาน", "สติแตก"},
	"สุภาพ":          {"ด่า", "ตวาด", "หยาบคาย", "ตะคอก", "แหกปาก"},
	// «ตบ» on its own is not violence - ตบไหล่ / ตบบ่า / ตบมือ are friendly.
	// Only the unambiguous forms belong here.
	"อ่อนโยน":   {"ตบหน้า", "ตบตี", "กระชาก", "ทุบ", "ขว้างปา", "กระทืบ"},
	"ขี้อาย":    {"โอ้อวด", "เรียกร้องความสนใจ"},
	"พูดน้อย":   {"พูดไม่หยุด", "ร่ายยาว", "เจื้อยแจ้ว"},
	"ซื่อสัตย์": {"โกหก", "หลอกลวง", "ต้มตุ๋น"},
	"ใจดี":      {"รังแก", "เหยียดหยาม", "ทำร้าย"},
	"หยิ่ง":     {"อ้อนวอน", "ก้มหัวให้", "ประจบ"},
	"กล้าหาญ":   {"ขี้ขลาด", "หนีเอาตัวรอด"},
	"เย็นชา":    {"ออดอ้อน", "หยอกล้อ"},
}

// composedTraits are the reserved/dignified traits whose REGISTER a bubbly
// end-particle contradicts: a สุขุม character closing an utterance with «จ้า»
// is worth a question even though no single "opposite word" appears.
// Substring-matched against the trait, so ขรึม catches เงียบขรึม too.
var composedTraits = []string{"สุขุม", "ขรึม", "เย็นชา", "เป็นทางการ", "ผู้ดี", "ใจนิ่ง"}

// cutesyParticles are utterance-final particles in a distinctly informal,
// bubbly register. Matched only at a phrase boundary (see hasEndParticle), so
// เจ้า and จ้าง can never trip them.
var cutesyParticles = []string{"จ้า", "จ้ะ", "จ๊ะ", "งับ", "ครัช", "ค้าบ"}

// hasEndParticle reports whether particle appears in line AT AN UTTERANCE
// BOUNDARY: followed by nothing, whitespace, punctuation, or a closing quote.
// A leading เ before จ-particles is rejected so เจ้า (the pronoun) never
// matches จ้า (the particle).
func hasEndParticle(line, particle string) bool {
	runes := []rune(line)
	p := []rune(particle)
	for i := 0; i+len(p) <= len(runes); i++ {
		match := true
		for j := range p {
			if runes[i+j] != p[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if next := i + len(p); next < len(runes) {
			switch runes[next] {
			case ' ', '\t', '"', '\'', '”', '»', ')', '!', '?', '~', '.', '…', ',':
			default:
				continue
			}
		}
		if i > 0 && p[0] == 'จ' && runes[i-1] == 'เ' {
			continue
		}
		return true
	}
	return false
}

// traitSources collects every trait-like statement about a character: the
// ลักษณะนิสัย chips verbatim, plus known trait words found inside the summary
// and description ("เขาเป็นคนสุขุม" counts even though it never became a
// chip). A negated mention (ไม่สุขุม) is not a trait.
func traitSources(member characters.View) []traitSource {
	sources := make([]traitSource, 0, len(member.Traits))
	for _, trait := range member.Traits {
		sources = append(sources, traitSource{Field: "ลักษณะนิสัย", Value: trait, Trait: trait})
	}
	freeText := func(field string, value *string) {
		if value == nil {
			return
		}
		known := make([]string, 0, len(traitContradictions)+len(composedTraits))
		for key := range traitContradictions {
			known = append(known, key)
		}
		known = append(known, composedTraits...)
		for _, key := range known {
			if !strings.Contains(*value, key) || strings.Contains(*value, "ไม่"+key) {
				continue
			}
			sources = append(sources, traitSource{Field: field, Value: key, Trait: key})
		}
	}
	freeText("บทบาท", member.Role)
	freeText("คำอธิบายสั้น", member.Summary)
	freeText("ภูมิหลัง", member.Description)
	return sources
}

// traitSource is one citable statement: which sheet field said it, verbatim.
type traitSource struct {
	Field string
	Value string
	Trait string
}

// CharacterCheck compares chapter text against the cast's own sheets.
func (t *Tools) CharacterCheck(
	ctx context.Context, identity *auth.Identity, novelRef string,
	chapterNumber int, text string,
) (CharacterCheckResult, error) {
	if !t.enabled {
		return CharacterCheckResult{}, unavailable()
	}
	userID, err := requireUser(identity)
	if err != nil {
		return CharacterCheckResult{}, err
	}
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return CharacterCheckResult{}, err
	}
	prefs, err := t.effectiveFor(ctx, userID, novel.ID)
	if err != nil {
		return CharacterCheckResult{}, err
	}
	result := CharacterCheckResult{Skipped: []SkippedCharacter{}, Issues: []CharacterIssue{}}
	if !prefs.Assistant || !prefs.Character {
		return result, nil
	}

	ref, _ := novels.ParseRef(novelRef)
	cast, err := t.cast.List(ctx, identity, ref)
	if err != nil {
		return CharacterCheckResult{}, err
	}
	result.Total = len(cast)

	evolutions, err := t.repo.Evolutions(ctx, novel.ID)
	if err != nil {
		return CharacterCheckResult{}, t.internal("load evolutions", err)
	}

	if utf8.RuneCountInString(text) < minCheckRunes {
		for _, member := range cast {
			result.Skipped = append(result.Skipped, SkippedCharacter{
				CharacterID: mustID(member.ID), Name: member.Name, Reason: "ตอนนี้สั้นเกินกว่าจะตรวจ",
			})
		}
		return result, nil
	}

	lines := strings.Split(text, "\n")

	// What the model tier consumes: every checkable character's verbatim
	// profile and their attributed lines, each remembering its line number.
	// Filled while the rule pass walks, scored after it, NEWEST LINES FIRST -
	// when the pair cap bites on a long chapter, it must drop the opening,
	// not the scene being written right now (docs/AI-CONSISTENCY-MODEL.md).
	type indexedPair struct {
		pair ModelPair
		// source is the manuscript line the evidence was taken from - what
		// the finding quotes back to the writer.
		source string
		line   int
	}
	var modelPairs []indexedPair
	memberName := map[string]string{}
	citedLine := map[string]bool{}

	// Who acted on each line. Built from the WHOLE cast - including members
	// this check will skip - so a line belonging to one character can never
	// leak onto the neighbour whose name happens to sit above it.
	castNames := make(map[string][]string, len(cast))
	for _, member := range cast {
		castNames[member.ID] = nameVariants(member.Name)
	}
	actors := attributeLines(lines, castNames)
	narrations := make([]string, len(lines))
	dialogues := make([]string, len(lines))
	for i, line := range lines {
		narrations[i], dialogues[i] = splitQuoted(line)
	}

	for _, member := range cast {
		id := mustID(member.ID)
		if from, moved := evolutions[id]; moved && chapterNumber >= from {
			result.Skipped = append(result.Skipped, SkippedCharacter{
				CharacterID: id, Name: member.Name,
				Reason: "ปิดการเทียบตั้งแต่ตอนที่ " + itoa(from) + " (ตัวละครเปลี่ยนไปโดยเจตนา)",
			})
			continue
		}
		if !checkableCharacter(member) {
			result.Skipped = append(result.Skipped, SkippedCharacter{
				CharacterID: id, Name: member.Name,
				Reason: "ยังไม่มีข้อมูลพอในหน้าตัวละคร - เพิ่มนิสัยหรือภูมิหลังเพื่อให้ตรวจได้",
			})
			continue
		}
		result.Checkable++
		memberName[id.String()] = member.Name
		variants := castNames[member.ID]

		if t.model != nil {
			profile := characterProfile(member)
			for li, line := range lines {
				if strings.TrimSpace(line) == "" || actors[li].actor != member.ID {
					continue
				}
				evidence := evidenceFor(line, narrations[li], actors[li].own)
				// Too short to carry evidence. An exclamation ("แคว่ก!",
				// "«หืม?»") shares no wording with any profile, which the
				// model reads as conflict - a finding with nothing behind it.
				if utf8.RuneCountInString(evidence) < modelMinRunes {
					continue
				}
				modelPairs = append(modelPairs, indexedPair{line: li, source: line, pair: ModelPair{
					// The primary variant, not the annotated sheet name - the
					// NLI premise reads it as prose («จงหลี่มีนิสัยสุขุม…»).
					CharacterID: id.String(), Name: variants[0],
					Profile: profile, Line: evidence,
				}})
			}
		}

		// Attribution stays a guess even at its best - a pronoun can point
		// the wrong way - which is why every finding is phrased as a question.
		seenIssue := map[string]bool{}
		cite := func(src traitSource, line, cited, explanation string) {
			key := line + "\x00" + cited
			if seenIssue[key] {
				return
			}
			seenIssue[key] = true
			citedLine[id.String()+"\x00"+line] = true
			result.Issues = append(result.Issues, CharacterIssue{
				CharacterID:   id,
				CharacterName: member.Name,
				Field:         src.Field,
				// A sentence-length trait is cited by its head, not in full -
				// the citation must fit on a card.
				FieldValue:  truncateRunes(src.Value, 60),
				Quote:       truncateRunes(strings.TrimSpace(line), 160),
				Explanation: explanation,
				Severity:    string(thai.Medium),
			})
		}

		for _, src := range dedupTraitSources(traitSources(member)) {
			for key, opposites := range traitContradictions {
				if !strings.Contains(src.Trait, key) {
					continue
				}
				for li, line := range lines {
					if actors[li].actor != member.ID {
						continue
					}
					for _, opposite := range opposites {
						// Narration only: a behaviour word inside quotes is
						// something the character SAID, not something they did.
						if !strings.Contains(narrations[li], opposite) ||
							actedUpon(narrations[li], opposite) {
							continue
						}
						// The explanation cites the trait WORD the rule stands
						// on - a sentence-length sheet entry is named by its
						// matched key, not quoted whole.
						cite(src, line, opposite,
							"«"+member.Name+"» ระบุ"+src.Field+" «"+key+
								"» - บรรทัดนี้มีคำว่า «"+opposite+"» อาจไม่ตรงกับที่ตั้งไว้ หรือเป็นการพัฒนาตัวละครโดยเจตนา")
					}
				}
			}
			for _, key := range composedTraits {
				if !strings.Contains(src.Trait, key) {
					continue
				}
				for li, line := range lines {
					if actors[li].actor != member.ID {
						continue
					}
					// Register is a property of SPEECH: read the quoted part
					// when there is one, the whole line otherwise (chat
					// fiction carries no quote marks).
					utterance := dialogues[li]
					if strings.TrimSpace(utterance) == "" {
						utterance = line
					}
					for _, particle := range cutesyParticles {
						if !hasEndParticle(utterance, particle) {
							continue
						}
						cite(src, line, particle,
							"«"+member.Name+"» ระบุ"+src.Field+" «"+key+
								"» - บรรทัดนี้ลงท้ายด้วย «"+particle+"» น้ำเสียงอาจเป็นกันเองกว่าที่ตั้งไว้ หรือเป็นความตั้งใจ")
					}
				}
				break
			}
		}
	}

	// The model tier (docs/AI-CONSISTENCY-MODEL.md): semantic contradiction
	// over the same attributed lines. Fail-quiet by contract - the rule
	// findings above already answered, so a sidecar problem is only a log
	// line, never an error the writer sees.
	if t.model != nil && len(modelPairs) > 0 {
		sort.SliceStable(modelPairs, func(i, j int) bool {
			return modelPairs[i].line > modelPairs[j].line
		})
		// Newest lines first, but every checkable character gets a share of
		// the cap before anyone gets a second helping: a chapter that ends in
		// one character's long scene must not push the rest out of the model
		// tier entirely. Leftover capacity then goes to the newest remainder.
		share := modelMaxPairs
		if result.Checkable > 1 {
			share = max(1, modelMaxPairs/result.Checkable)
		}
		taken := map[string]int{}
		pairs := make([]ModelPair, 0, modelMaxPairs)
		spill := make([]ModelPair, 0, len(modelPairs))
		// The model is shown the evidence; the writer is shown the LINE it
		// came from. Without this the card would quote narration stripped of
		// its dialogue, which matches nothing in the manuscript - and the
		// underline, which finds the quote by text, would never appear.
		sourceLine := make(map[string]string, len(modelPairs))
		for _, ip := range modelPairs {
			sourceLine[ip.pair.CharacterID+"\x00"+ip.pair.Line] = ip.source
			if taken[ip.pair.CharacterID] < share && len(pairs) < modelMaxPairs {
				taken[ip.pair.CharacterID]++
				pairs = append(pairs, ip.pair)
				continue
			}
			spill = append(spill, ip.pair)
		}
		for _, pair := range spill {
			if len(pairs) >= modelMaxPairs {
				break
			}
			pairs = append(pairs, pair)
		}
		scored, pending, err := t.model.Consistency(ctx, pairs)
		result.ModelPending = pending
		if err != nil {
			t.log.Warn("ai tools: consistency sidecar unavailable, rules only",
				slog.Any("error", err))
		} else {
			// Contradiction saturates near 1.00 on a whole chapter, so the
			// order within the ties decides what the writer actually sees:
			// findings the model can NAME a tone for come first, because a
			// card that says why is worth more than one that only scores.
			sort.SliceStable(scored, func(i, j int) bool {
				if scored[i].Contradiction != scored[j].Contradiction {
					return scored[i].Contradiction > scored[j].Contradiction
				}
				return len(scored[i].Labels) > len(scored[j].Labels)
			})
			// One question per character before anyone gets a second: three
			// cards about one character crowd out the scene that actually
			// went off the rails elsewhere in the chapter.
			byCharacter := map[string][]ModelResult{}
			castOrder := make([]string, 0, len(scored))
			for _, hit := range scored {
				if _, seen := byCharacter[hit.CharacterID]; !seen {
					castOrder = append(castOrder, hit.CharacterID)
				}
				byCharacter[hit.CharacterID] = append(byCharacter[hit.CharacterID], hit)
			}
			ordered := make([]ModelResult, 0, len(scored))
			for round := 0; round < maxModelFindings; round++ {
				for _, characterID := range castOrder {
					if round < len(byCharacter[characterID]) {
						ordered = append(ordered, byCharacter[characterID][round])
					}
				}
			}
			added := 0
			for _, hit := range ordered {
				if added >= maxModelFindings {
					break
				}
				// Skipped rather than stopped: the round-robin interleaves
				// characters, so a weak hit here can still be followed by a
				// strong one belonging to somebody else.
				if hit.Contradiction < conflictThreshold {
					continue
				}
				// The manuscript line the evidence came from - what the card
				// quotes and what the underline searches for.
				quoted := sourceLine[hit.CharacterID+"\x00"+hit.Line]
				if quoted == "" {
					quoted = hit.Line
				}
				// A line the rules already cited for this character needs no
				// second card saying the same thing.
				if citedLine[hit.CharacterID+"\x00"+quoted] {
					continue
				}
				name := memberName[hit.CharacterID]
				if name == "" {
					continue
				}
				id, err := uuid.Parse(hit.CharacterID)
				if err != nil {
					continue
				}
				// Both sides must be readable and they must actually clash.
				// A high contradiction score with no nameable clash is the
				// class of finding writers reject: «จริงจัง» flagged on a
				// สุขุม character is the tone their sheet asked for.
				sheetTone, lineTone, clash := clashingTones(hit.ProfileLabels, hit.Labels)
				if !clash {
					continue
				}
				// The line's tone carries its confidence - the part of a
				// finding a writer argues with, offered as a reading rather
				// than a verdict.
				confidence := 0
				if len(hit.Labels) > 0 {
					confidence = int(hit.Labels[0].Score * 100)
				}
				result.Issues = append(result.Issues, CharacterIssue{
					CharacterID:   id,
					CharacterName: name,
					Field:         "โปรไฟล์ตัวละคร",
					FieldValue:    truncateRunes(profileFor(cast, hit.CharacterID), 60),
					Quote:         truncateRunes(strings.TrimSpace(quoted), 160),
					// The card names BOTH readings, so the writer can disagree
					// with a specific claim instead of a bare score.
					Explanation: "«" + name + "» - นิสัยที่ตั้งไว้อ่านได้เป็นโทน «" + sheetTone +
						"» แต่บรรทัดนี้อ่านได้เป็นโทน «" + lineTone + "» (" + itoa(confidence) +
						"%) โมเดลภาษาให้ความขัดแย้ง " + itoa(int(hit.Contradiction*100)) +
						"% อาจไม่ตรงกับที่ตั้งไว้ หรือเป็นการพัฒนาตัวละครโดยเจตนา",
					Severity: string(thai.Medium),
				})
				added++
			}
		}
	}

	return result, nil
}

// characterProfile is the sheet as the writer wrote it, concatenated
// verbatim - the Character Profile box of the design.
func characterProfile(member characters.View) string {
	parts := make([]string, 0, 6)
	if len(member.Traits) > 0 {
		parts = append(parts, strings.Join(member.Traits, " "))
	}
	for _, field := range []*string{member.Role, member.Summary, member.Description} {
		if field != nil && *field != "" {
			parts = append(parts, *field)
		}
	}
	return strings.Join(parts, " ")
}

// profileFor re-derives the cited profile head for a model finding.
func profileFor(cast []characters.View, characterID string) string {
	for _, member := range cast {
		if member.ID == characterID {
			return characterProfile(member)
		}
	}
	return ""
}

// nameVariants lists the ways a sheet's name may appear in the manuscript.
// Writers annotate sheet names - «จงหลี่ (Zhongli)», «มายา / Maya» - and the
// prose then uses just one part, so requiring the full sheet name verbatim
// makes the whole check blind to that character. The name outside any
// parentheses comes first (it is the primary, used in premises and prose),
// then each parenthesized or slash-separated alias. Variants shorter than
// two runes are dropped - a single letter would match everywhere.
func nameVariants(name string) []string {
	var outside, inside []string
	var out, in strings.Builder
	depth := 0
	for _, r := range name {
		switch {
		case r == '(' || r == '（':
			if depth == 0 && in.Len() > 0 {
				in.Reset()
			}
			depth++
		case r == ')' || r == '）':
			if depth > 0 {
				depth--
				if depth == 0 {
					inside = append(inside, in.String())
					in.Reset()
				}
			}
		case depth > 0:
			in.WriteRune(r)
		default:
			out.WriteRune(r)
		}
	}
	for _, part := range strings.FieldsFunc(out.String(), func(r rune) bool {
		return r == '/' || r == ',' || r == '|'
	}) {
		outside = append(outside, part)
	}
	seen := map[string]bool{}
	variants := make([]string, 0, len(outside)+len(inside)+1)
	for _, candidate := range append(outside, inside...) {
		candidate = strings.TrimSpace(candidate)
		if utf8.RuneCountInString(candidate) < 2 || seen[candidate] {
			continue
		}
		seen[candidate] = true
		variants = append(variants, candidate)
	}
	if len(variants) == 0 {
		variants = append(variants, strings.TrimSpace(name))
	}
	return variants
}

// ---------------------------------------------------------------------------
// Actor attribution - the Character Detection box of docs/AI-CONSISTENCY-MODEL.md
//
// A finding is about what a character DID. Merely having their name nearby is
// not enough: in «"นายเขินเหรอ?" คุณแกล้งเดินไปตบไหล่เขา» the one doing the
// slapping is the reader, not the character being slapped. Every finding -
// rule or model - hangs off the resolved actor, so a line the character did
// not act in can never become a finding about them.
// ---------------------------------------------------------------------------

// readerActors are the narrator's own subjects: the reader-insert «คุณ» of
// second-person fiction and the first-person pronouns. A line with one of
// these as its subject belongs to the narrator, never to the cast.
var readerActors = []string{"คุณ", "ฉัน", "ผม", "ดิฉัน", "หนู", "ข้า", "เรา", "กู"}

// anaphora are third-person subjects standing in for the character named
// most recently: «"ก็ได้ ๆ" เขาหันหลังกลับ» is still that character acting.
var anaphora = []string{"เขา", "หล่อน", "เธอ", "มัน", "ท่าน"}

// speechVerbs tie a narration clause to the utterance beside it. With one of
// these the character is the SPEAKER and their words are theirs to answer
// for; without it the narration only says what they did, and the quoted line
// may well be somebody else's ("«…!» เสียงแหลมของคู่หูดังลั่น ขณะที่เอเธอร์ยืนค้าง").
var speechVerbs = []string{
	"พูด", "ตอบ", "บอก", "ถาม", "ตะโกน", "กระซิบ", "เอ่ย", "พึมพำ", "ทัก",
	"เตือน", "สั่ง", "ตวาด", "ร้อง", "หัวเราะ", "ยิ้ม", "บ่น", "ครวญ",
	"เล่า", "ประกาศ", "แย้ง", "ย้ำ",
}

type actorKind int

const (
	actorNone actorKind = iota
	actorCharacter
	actorReader
	actorAnaphora
)

// attributedLine is one line's resolved actor.
type attributedLine struct {
	// actor is the character ID, "" for the narrator, the reader, or nobody
	// identifiable.
	actor string
	// own records that the actor came from THIS line's narration subject
	// rather than being inherited from the line above.
	own bool
}

// splitQuoted separates a line into narration and speech. Thai manuscripts
// mark utterances with “…”, «…», "…" or 「…」; what is inside them is what
// somebody SAID, what is outside is what somebody DID - and a behaviour rule
// must only ever read the second.
func splitQuoted(line string) (narration, dialogue string) {
	var nar, dia strings.Builder
	depth := 0
	straight := false
	for _, r := range line {
		switch r {
		case '"':
			straight = !straight
			continue
		case '“', '‘', '«', '„', '「':
			depth++
			continue
		case '”', '’', '»', '」':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth > 0 || straight {
			dia.WriteRune(r)
		} else {
			nar.WriteRune(r)
		}
	}
	return nar.String(), dia.String()
}

// firstActor finds the SUBJECT of a narration span. Thai narration leads with
// its actor ("จงหลี่หันกลับมา", "คุณยกแขนขึ้น"), so the earliest name or
// pronoun is who acted - everything after it is what they did, or who they
// did it to.
func firstActor(narration string, names map[string][]string) (string, actorKind) {
	best, bestLen := -1, 0
	bestID, kind := "", actorNone
	consider := func(at, length int, id string, k actorKind) {
		if at < 0 {
			return
		}
		if best < 0 || at < best || (at == best && length > bestLen) {
			best, bestLen, bestID, kind = at, length, id, k
		}
	}
	for id, variants := range names {
		for _, variant := range variants {
			consider(strings.Index(narration, variant), len(variant), id, actorCharacter)
		}
	}
	for _, pronoun := range readerActors {
		consider(strings.Index(narration, pronoun), len(pronoun), "", actorReader)
	}
	for _, pronoun := range anaphora {
		consider(strings.Index(narration, pronoun), len(pronoun), "", actorAnaphora)
	}
	return bestID, kind
}

// attributeLines resolves the actor of every line.
func attributeLines(lines []string, names map[string][]string) []attributedLine {
	actors := make([]attributedLine, len(lines))
	for i, line := range lines {
		narration, dialogue := splitQuoted(line)
		id, kind := firstActor(narration, names)
		switch kind {
		case actorCharacter:
			actors[i] = attributedLine{actor: id, own: true}
		case actorReader:
			// The narrator acted. No cast member owns this line.
			actors[i] = attributedLine{own: true}
		case actorAnaphora:
			actors[i] = attributedLine{actor: lastAttributed(actors, i), own: true}
		default:
			// Bare speech takes its speaker from the line DIRECTLY above, and
			// only when that line is pure narration that named its own actor
			// (the "จงหลี่หันมา" / «บทพูด» shape). If the line above already
			// held speech, this quote is the REPLY - the speaker has
			// alternated - and the line stays unattributed rather than
			// pinning one character's words on another.
			if strings.TrimSpace(narration) != "" || strings.TrimSpace(dialogue) == "" {
				continue
			}
			for j := i - 1; j >= 0; j-- {
				if strings.TrimSpace(lines[j]) == "" {
					continue
				}
				if _, spoke := splitQuoted(lines[j]); actors[j].own && strings.TrimSpace(spoke) == "" {
					actors[i].actor = actors[j].actor
				}
				break
			}
		}
		// A speaker rarely says their own name. When the speech ADDRESSES the
		// character the attribution landed on - «เวนติ! เลิกแกล้งแล้วไปหายาแก้มา!» -
		// somebody is talking TO them, so the line is not theirs. Only
		// attributions inherited from elsewhere are second-guessed this way;
		// a line whose own narration names the actor keeps them.
		if id := actors[i].actor; id != "" && kind != actorCharacter {
			for _, variant := range names[id] {
				if strings.Contains(dialogue, variant) {
					actors[i].actor = ""
					break
				}
			}
		}
	}
	return actors
}

// lastAttributed is the nearest character resolved above line i - the
// antecedent a third-person pronoun refers back to.
func lastAttributed(actors []attributedLine, i int) string {
	for j := i - 1; j >= 0; j-- {
		if actors[j].actor != "" {
			return actors[j].actor
		}
	}
	return ""
}

// evidenceFor is the part of a line the model should judge the character on.
// A narration subject answers for what they DID; their words come with it
// only when a speech verb ties them to the utterance. Bare dialogue is the
// whole evidence there is.
func evidenceFor(line, narration string, own bool) string {
	if !own {
		return line
	}
	for _, verb := range speechVerbs {
		if strings.Contains(narration, verb) {
			return line
		}
	}
	return strings.TrimSpace(narration)
}

// actedUpon reports whether the behaviour word is preceded by a Thai passive
// marker: «เอเธอร์ถูกตบหน้า» is done TO the character, not BY them.
func actedUpon(narration, word string) bool {
	at := strings.Index(narration, word)
	if at < 0 {
		return false
	}
	before := []rune(narration[:at])
	if len(before) > 24 {
		before = before[len(before)-24:]
	}
	lead := string(before)
	return strings.Contains(lead, "ถูก") || strings.Contains(lead, "โดน")
}

// dedupTraitSources keeps one source per trait word - a chip and a
// description mentioning the same trait must not double every finding.
func dedupTraitSources(sources []traitSource) []traitSource {
	seen := map[string]bool{}
	out := make([]traitSource, 0, len(sources))
	for _, src := range sources {
		if seen[src.Trait] {
			continue
		}
		seen[src.Trait] = true
		out = append(out, src)
	}
	return out
}

// SetEvolution records "ตัวละครเปลี่ยนไปตั้งแต่ตอนนี้": from that chapter
// number on, the sheet stops being compared. fromChapter 0 clears the marker.
func (t *Tools) SetEvolution(
	ctx context.Context, identity *auth.Identity, novelRef string,
	characterID uuid.UUID, fromChapter int,
) error {
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return err
	}
	if fromChapter < 0 || fromChapter > 100_000 {
		return apierror.Validation(map[string][]string{
			"from_chapter_number": {"Must be a chapter number (or 0 to clear)."},
		})
	}
	ok, err := t.repo.SetEvolution(ctx, novel.ID, characterID, fromChapter)
	if err != nil {
		return t.internal("set evolution", err)
	}
	if !ok {
		return apierror.New(404, "CHARACTER_NOT_FOUND", "Character not found.")
	}
	return nil
}

func checkableCharacter(member characters.View) bool {
	// Anything traitSources can read counts - a known trait word in the role
	// ("ที่ปรึกษาผู้สุขุม") is as checkable as a chip.
	if len(traitSources(member)) > 0 || len(member.Details) > 0 {
		return true
	}
	if member.Description != nil && utf8.RuneCountInString(*member.Description) >= 20 {
		return true
	}
	return member.Quote != nil && *member.Quote != ""
}

// ---------------------------------------------------------------------------
// The fact book + continuity (13Y §6)
// ---------------------------------------------------------------------------

// Fact is one writer-owned statement about the story's state as of a chapter.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Facts returns a chapter's fact book (editor-gated; empty if never written).
func (t *Tools) Facts(
	ctx context.Context, identity *auth.Identity, novelRef string, chapterID uuid.UUID,
) ([]Fact, error) {
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}
	facts, ok, err := t.repo.Facts(ctx, novel.ID, chapterID)
	if err != nil {
		return nil, t.internal("load facts", err)
	}
	if !ok {
		return nil, apierror.New(404, "CHAPTER_NOT_FOUND", "Chapter not found.")
	}
	return facts, nil
}

// SetFacts replaces a chapter's fact book.
func (t *Tools) SetFacts(
	ctx context.Context, identity *auth.Identity, novelRef string,
	chapterID uuid.UUID, facts []Fact,
) ([]Fact, error) {
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}
	if len(facts) > MaxFactRows {
		return nil, apierror.Validation(map[string][]string{
			"facts": {"At most 100 facts per chapter."},
		})
	}
	cleaned := make([]Fact, 0, len(facts))
	for _, fact := range facts {
		label := strings.TrimSpace(fact.Label)
		value := strings.TrimSpace(fact.Value)
		if label == "" {
			continue
		}
		if utf8.RuneCountInString(label) > FactLabelMaxRunes ||
			utf8.RuneCountInString(value) > FactValueMaxRunes ||
			strings.ContainsAny(label, "\n\r") {
			return nil, apierror.Validation(map[string][]string{
				"facts": {"A fact is too long."},
			})
		}
		cleaned = append(cleaned, Fact{Label: label, Value: value})
	}
	ok, err := t.repo.SetFacts(ctx, novel.ID, chapterID, cleaned)
	if err != nil {
		return nil, t.internal("save facts", err)
	}
	if !ok {
		return nil, apierror.New(404, "CHAPTER_NOT_FOUND", "Chapter not found.")
	}
	return cleaned, nil
}

// ContinuityIssue is one fact that changed without a chapter in between
// saying so - shown with BOTH sides and where the earlier one came from.
type ContinuityIssue struct {
	Label           string `json:"label"`
	ThisValue       string `json:"this_value"`
	PreviousValue   string `json:"previous_value"`
	PreviousChapter int    `json:"previous_chapter"`
	Explanation     string `json:"explanation"`
}

// ContinuityResult is the round check's answer.
type ContinuityResult struct {
	// Checked is false when this chapter has no fact book yet - the check has
	// nothing to compare, and says so instead of pretending it passed.
	Checked bool              `json:"checked"`
	Issues  []ContinuityIssue `json:"issues"`
}

// ContinuityCheck compares a chapter's facts against the merged facts of the
// chapters BEFORE it (13Y §6): later chapters are not contradictions, and
// headcanon chapters are excluded - a headcanon is a hypothesis, not canon.
func (t *Tools) ContinuityCheck(
	ctx context.Context, identity *auth.Identity, novelRef string, chapterID uuid.UUID,
) (ContinuityResult, error) {
	if !t.enabled {
		return ContinuityResult{}, unavailable()
	}
	userID, err := requireUser(identity)
	if err != nil {
		return ContinuityResult{}, err
	}
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return ContinuityResult{}, err
	}
	prefs, err := t.effectiveFor(ctx, userID, novel.ID)
	if err != nil {
		return ContinuityResult{}, err
	}
	result := ContinuityResult{Issues: []ContinuityIssue{}}
	if !prefs.Assistant || !prefs.Continuity {
		return result, nil
	}

	current, ok, err := t.repo.Facts(ctx, novel.ID, chapterID)
	if err != nil {
		return ContinuityResult{}, t.internal("load facts", err)
	}
	if !ok {
		return ContinuityResult{}, apierror.New(404, "CHAPTER_NOT_FOUND", "Chapter not found.")
	}
	if len(current) == 0 {
		return result, nil
	}
	result.Checked = true

	previous, err := t.repo.PreviousFacts(ctx, novel.ID, chapterID, string(novel.Format.PresentationFormat))
	if err != nil {
		return ContinuityResult{}, t.internal("load previous facts", err)
	}

	for _, fact := range current {
		key := strings.ToLower(fact.Label)
		if earlier, ok := previous[key]; ok &&
			!strings.EqualFold(strings.TrimSpace(earlier.Value), strings.TrimSpace(fact.Value)) {
			result.Issues = append(result.Issues, ContinuityIssue{
				Label:           fact.Label,
				ThisValue:       fact.Value,
				PreviousValue:   earlier.Value,
				PreviousChapter: earlier.Chapter,
				Explanation: "«" + fact.Label + "» ตอนนี้ระบุ «" + fact.Value +
					"» แต่ตอนที่ " + itoa(earlier.Chapter) + " ระบุ «" + earlier.Value +
					"» - อาจตั้งใจเปลี่ยน หรืออาจหลุด",
			})
		}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Search (13Y §8) - the command palette's backend
// ---------------------------------------------------------------------------

// SearchHit is one place the query appears - chapter, where in it, and a
// snippet around the match. Drafts included: that is why writers search.
type SearchHit struct {
	ChapterID     uuid.UUID `json:"chapter_id"`
	Slug          string    `json:"slug"`
	ChapterNumber int       `json:"chapter_number"`
	Title         *string   `json:"title,omitempty"`
	Status        string    `json:"status"`
	Where         string    `json:"where"` // prose | chat | entry | title
	Snippet       string    `json:"snippet"`
}

// Search finds literal text across the fiction's chapters - prose, chat
// messages, and headcanon entries, drafts included (editor-gated).
func (t *Tools) Search(
	ctx context.Context, identity *auth.Identity, novelRef, query string,
) ([]SearchHit, error) {
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > SearchQueryMaxRunes {
		return nil, apierror.Validation(map[string][]string{
			"q": {"A search needs 1-120 characters."},
		})
	}
	hits, err := t.repo.Search(ctx, novel.ID, trimmed, MaxSearchResults)
	if err != nil {
		return nil, t.internal("search", err)
	}
	return hits, nil
}

// ---------------------------------------------------------------------------
// The pre-publish check (13Y §11) - the round where waiting is acceptable
// ---------------------------------------------------------------------------

// PrecheckResult bundles every round check for the publish confirmation.
type PrecheckResult struct {
	// Skipped is set when the chapter is too short to check (13Y §12).
	Skipped     bool                 `json:"skipped,omitempty"`
	Spell       []InlineSuggestion   `json:"spell"`
	Character   CharacterCheckResult `json:"character"`
	Continuity  ContinuityResult     `json:"continuity"`
	SpellCount  int                  `json:"spell_count"`
	IssueCount  int                  `json:"issue_count"`
	CheckedText int                  `json:"checked_runes"`
}

// Precheck runs the full round over one chapter: the live rules, the
// character comparison, and (when enabled) continuity.
func (t *Tools) Precheck(
	ctx context.Context, identity *auth.Identity, novelRef string,
	chapterID uuid.UUID,
) (PrecheckResult, error) {
	if !t.enabled {
		return PrecheckResult{}, unavailable()
	}
	if _, err := requireUser(identity); err != nil {
		return PrecheckResult{}, err
	}
	novel, err := t.editorNovel(ctx, identity, novelRef)
	if err != nil {
		return PrecheckResult{}, err
	}

	content, ok, err := t.repo.ChapterText(ctx, novel.ID, chapterID)
	if err != nil {
		return PrecheckResult{}, t.internal("load chapter text", err)
	}
	if !ok {
		return PrecheckResult{}, apierror.New(404, "CHAPTER_NOT_FOUND", "Chapter not found.")
	}

	result := PrecheckResult{
		Spell:       []InlineSuggestion{},
		CheckedText: utf8.RuneCountInString(content.Text),
	}
	if result.CheckedText < minCheckRunes {
		result.Skipped = true
		return result, nil
	}

	check, err := t.Check(ctx, identity, CheckInput{
		NovelRef: novelRef, Mode: content.Mode, Text: content.Text,
	})
	if err != nil {
		return PrecheckResult{}, err
	}
	result.Spell = check.Suggestions
	result.SpellCount = len(check.Suggestions)

	character, err := t.CharacterCheck(ctx, identity, novelRef, content.Number, content.Text)
	if err != nil {
		return PrecheckResult{}, err
	}
	result.Character = character

	continuity, err := t.ContinuityCheck(ctx, identity, novelRef, chapterID)
	if err != nil {
		return PrecheckResult{}, err
	}
	result.Continuity = continuity

	result.IssueCount = result.SpellCount + len(character.Issues) + len(continuity.Issues)
	return result, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func mustID(raw string) uuid.UUID {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// Compile-time assurance the real neighbouring services satisfy the slivers.
var (
	_ CastSource     = (*characters.Service)(nil)
	_ VariableSource = (*variables.Service)(nil)
)
