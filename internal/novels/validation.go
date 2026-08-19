package novels

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/slug"
)

// Field bounds (docs/09 §36 requires maximum lengths at the API boundary).
//
// These are counted in RUNES, not bytes. A Thai title is roughly three bytes
// per character, so a byte limit would silently give Thai writers a third of the
// room it gives English ones - unacceptable on a Thai-first platform.
const (
	TitleMaxLength          = 200
	DescriptionMaxLength    = 5000
	ContentWarningMaxLength = 500
	URLMaxLength            = 2048

	// TaglineMaxLength bounds คำโปรย (13S). Short by design: it is the line
	// under a cover, and one that wraps to four lines is a synopsis wearing the
	// wrong field.
	TaglineMaxLength = 200

	// ForewordMaxLength bounds บทนำ. Generous - it is prose the author wrote -
	// but not chapter-sized: a foreword long enough to be a chapter is one.
	ForewordMaxLength = 20_000

	// MaxGenresPerNovel keeps the CONTROLLED classification meaningful.
	//
	// Three was right when the vocabulary answered one question. Since 13S it
	// answers three - what the story is like, who it is about, and which AU -
	// and three across all of them would make a BL campus romance choose two of
	// its own facts. Eight is still a ceiling: a fiction in eight categories is
	// classified in none, which is what this number exists to prevent.
	MaxGenresPerNovel = 8

	// MaxTagsPerNovel bounds the flexible metadata so tagging stays curation,
	// not keyword stuffing that would degrade the ?tag= filters for everyone.
	//
	// Twenty rather than ten (§13R). Ten is a reasonable number of tags for a
	// novel and a low one for the fiction this platform is actually for: a
	// fanwork carries the fandom, the pairing, the characters, the tropes, and
	// the warnings, and readers search on every one of them. The ceiling is
	// still there - it is the difference between describing a work and
	// spamming the filters - it is just no longer below what an honest tag
	// list needs.
	MaxTagsPerNovel = 20

	// ตั้งค่าเพิ่มเติม bounds (§13K). Counted in RUNES, like every other text
	// bound here: a byte limit gives a Thai writer a third of the room.
	LanguageMaxLength        = 8
	ChapterUnitMaxLength     = 16
	AuthorNoteMaxLength      = 2_000
	SeriesNameMaxLength      = 120
	DerivativeTermsMaxLength = 280
)

// Defaults for the collapsed section, applied when a writer never opens it.
const (
	// DefaultLanguage is Thai: the platform is Thai-first (docs/05 §11), and a
	// column that defaulted to nothing would make every existing fiction
	// languageless.
	DefaultLanguage = "th"
	// DefaultChapterUnit is what most Thai fiction calls a chapter.
	DefaultChapterUnit = "ตอน"
)

// SupportedLanguages is the allowlist. It is short on purpose - a language the
// platform cannot display, search, or moderate is not a language it supports,
// and offering one would be a claim it cannot keep (docs/11 §43).
func SupportedLanguages() []string { return []string{"th", "en"} }

// validateExtras resolves the collapsed section as a whole.
//
// Returned rather than mutated so a rejected request cannot leave a caller
// holding a half-applied state, the same shape validateOrigin uses.
func validateExtras(errs validationErrors, input ExtrasInput, current Extras) Extras {
	next := current

	if input.Language != nil {
		value := strings.ToLower(strings.TrimSpace(*input.Language))
		if !slices.Contains(SupportedLanguages(), value) {
			errs.add("language", fmt.Sprintf("Must be one of: %s.",
				strings.Join(SupportedLanguages(), ", ")))
		} else {
			next.Language = value
		}
	}

	if input.ChapterUnit != nil {
		value := strings.TrimSpace(*input.ChapterUnit)
		switch {
		case value == "":
			// Clearing it restores the default rather than erroring: an empty
			// unit would render chapters as "ที่ 3" with no noun at all.
			next.ChapterUnit = DefaultChapterUnit
		case utf8.RuneCountInString(value) > ChapterUnitMaxLength:
			errs.add("chapter_unit", fmt.Sprintf(
				"Must be at most %d characters.", ChapterUnitMaxLength))
		case !singleLine(value):
			errs.add("chapter_unit", "Contains characters that are not allowed.")
		default:
			next.ChapterUnit = value
		}
	}

	if input.AuthorNoteStart != nil {
		next.AuthorNoteStart = validateNote(errs, "author_note_start", *input.AuthorNoteStart)
	}
	if input.AuthorNoteEnd != nil {
		next.AuthorNoteEnd = validateNote(errs, "author_note_end", *input.AuthorNoteEnd)
	}

	if input.SeriesName != nil {
		value := strings.TrimSpace(*input.SeriesName)
		switch {
		case value == "":
			// Leaving the series clears the position with it - the database
			// CHECK requires it, and a position about no series is a number
			// about nothing.
			next.SeriesName = nil
			next.SeriesPosition = nil
		case utf8.RuneCountInString(value) > SeriesNameMaxLength:
			errs.add("series_name", fmt.Sprintf(
				"Must be at most %d characters.", SeriesNameMaxLength))
		case !singleLine(value):
			errs.add("series_name", "Contains characters that are not allowed.")
		default:
			next.SeriesName = &value
		}
	}
	if input.SeriesPosition != nil && next.SeriesName != nil {
		if *input.SeriesPosition < 1 {
			errs.add("series_position", "Must be 1 or greater.")
		} else {
			position := *input.SeriesPosition
			next.SeriesPosition = &position
		}
	}

	if input.CommentAccess != nil {
		value := CommentAccess(strings.TrimSpace(*input.CommentAccess))
		if !value.Valid() {
			errs.add("comment_access", fmt.Sprintf("Must be one of: %s.",
				joinValues(CommentAccesses())))
		} else {
			next.CommentAccess = value
		}
	}
	if input.CommentApproval != nil {
		next.CommentApproval = *input.CommentApproval
	}

	next.Rights = validateRights(errs, input, current.Rights)
	return next
}

// validateRights resolves the author's stated permissions.
//
// It validates SHAPE only. There is nothing to enforce here and nothing
// downstream enforces it either: these are declarations rendered to readers,
// and the platform never advertises protection it does not have (§13E).
func validateRights(errs validationErrors, input ExtrasInput, current Rights) Rights {
	next := current

	if input.AllowScreenshot != nil {
		next.AllowScreenshot = *input.AllowScreenshot
	}
	if input.AllowTranslation != nil {
		next.AllowTranslation = *input.AllowTranslation
	}
	if input.AllowDerivative != nil {
		next.AllowDerivative = *input.AllowDerivative
	}
	if input.AllowAudio != nil {
		next.AllowAudio = *input.AllowAudio
	}
	if input.RequireCredit != nil {
		next.RequireCredit = *input.RequireCredit
	}

	if input.DerivativeTerms != nil {
		value := strings.TrimSpace(*input.DerivativeTerms)
		switch {
		case value == "":
			next.DerivativeTerms = nil
		case utf8.RuneCountInString(value) > DerivativeTermsMaxLength:
			errs.add("derivative_terms", fmt.Sprintf(
				"Must be at most %d characters.", DerivativeTermsMaxLength))
		case !SafeText(value):
			errs.add("derivative_terms", "Contains characters that are not allowed.")
		default:
			next.DerivativeTerms = &value
		}
	}

	// A condition attached to a permission that is off would be shown to
	// nobody, so it goes with it rather than lingering.
	if !next.AllowDerivative {
		next.DerivativeTerms = nil
	}
	return next
}

func validateNote(errs validationErrors, field, value string) *string {
	trimmed := strings.TrimSpace(value)
	switch {
	case trimmed == "":
		return nil
	case utf8.RuneCountInString(trimmed) > AuthorNoteMaxLength:
		errs.add(field, fmt.Sprintf("Must be at most %d characters.", AuthorNoteMaxLength))
	case !SafeText(trimmed):
		errs.add(field, "Contains characters that are not allowed.")
	default:
		return &trimmed
	}
	return nil
}

// ExtrasInput is the collapsed section as submitted. Every field is a pointer:
// an absent one keeps whatever the fiction already has.
type ExtrasInput struct {
	Language        *string
	ChapterUnit     *string
	AuthorNoteStart *string
	AuthorNoteEnd   *string
	SeriesName      *string
	SeriesPosition  *int
	CommentAccess   *string
	CommentApproval *bool

	AllowScreenshot  *bool
	AllowTranslation *bool
	AllowDerivative  *bool
	AllowAudio       *bool
	RequireCredit    *bool
	DerivativeTerms  *string
}

// validationErrors accumulates per-field messages so one request reports every
// problem at once rather than making the writer resubmit repeatedly.
type validationErrors map[string][]string

func (v validationErrors) add(field, message string) {
	v[field] = append(v[field], message)
}

// err returns a 422 carrying the collected fields, or nil if there are none.
func (v validationErrors) err() error {
	if len(v) == 0 {
		return nil
	}
	return apierror.Validation(map[string][]string(v))
}

// SafeText rejects text that is not plain, human-readable content.
//
// docs/11 §17 requires an explicitly allowed content model rather than
// arbitrary markup. For this phase that model is plain text: printable
// characters plus the whitespace a manuscript genuinely needs.
//
// Control characters are refused because they are never authored deliberately
// and are a classic vector for smuggling - a NUL would also be rejected by
// PostgreSQL itself, and returning a clean 422 beats a 500 from the driver.
func SafeText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		switch r {
		case '\n', '\r', '\t':
			continue
		}
		// Cc = C0/C1 controls, Cf = format characters such as the bidi
		// overrides used to disguise text, Co = private use, Cs = surrogates.
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}

// singleLine additionally rejects line breaks, for fields that are rendered as
// one line (a title in a card, a speaker name in a chat bubble). A newline there
// would let an author break the surrounding layout.
func singleLine(value string) bool {
	return SafeText(value) && !strings.ContainsAny(value, "\n\r")
}

func runeLen(value string) int { return utf8.RuneCountInString(value) }

// validateTitle checks a required, single-line title.
func validateTitle(errs validationErrors, field, value string) {
	trimmed := strings.TrimSpace(value)
	switch {
	case trimmed == "":
		errs.add(field, "Title is required.")
	case runeLen(trimmed) > TitleMaxLength:
		errs.add(field, fmt.Sprintf("Title must be at most %d characters.", TitleMaxLength))
	case !singleLine(trimmed):
		errs.add(field, "Title contains characters that are not allowed.")
	}
}

// validateOptionalText checks a nullable free-text field.
func validateOptionalText(errs validationErrors, field string, value *string, max int) {
	if value == nil {
		return
	}
	switch {
	case runeLen(*value) > max:
		errs.add(field, fmt.Sprintf("Must be at most %d characters.", max))
	case !SafeText(*value):
		errs.add(field, "Contains characters that are not allowed.")
	}
}

// validateOptionalURL checks a nullable absolute http(s) URL.
//
// The scheme allowlist matters: without it, `javascript:` would be accepted here
// and become an XSS vector the moment a client rendered it as a link
// (docs/11 §16).
func validateOptionalURL(errs validationErrors, field string, value *string) {
	if value == nil || *value == "" {
		return
	}
	raw := *value
	if runeLen(raw) > URLMaxLength {
		errs.add(field, fmt.Sprintf("Must be at most %d characters.", URLMaxLength))
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		errs.add(field, "Must be an absolute http or https URL.")
	}
}

// validateThemeColor checks and normalises a fiction's accent colour (13U).
// Lowercase #rrggbb, matching the CHECK constraint, so a value that validates
// here can never bounce off the database.
func validateThemeColor(errs validationErrors, raw string) string {
	color := strings.ToLower(strings.TrimSpace(raw))
	if len(color) != 7 || color[0] != '#' {
		errs.add("theme_color", "Must be a #rrggbb colour.")
		return color
	}
	for _, r := range color[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			errs.add("theme_color", "Must be a #rrggbb colour.")
			return color
		}
	}
	return color
}

// CreateInput is a validated request to create a fiction.
//
// The format dimensions are optional pointers: an omitted dimension takes its
// documented default (docs/09 §15) rather than being rejected.
type CreateInput struct {
	Title          string
	Description    *string
	Tagline        *string
	Foreword       *string
	CoverURL       *string
	ContentWarning *string

	StoryStructure     *string
	PresentationFormat *string
	ContentMode        *string

	Status     *string
	Visibility *string

	// Creation fields (docs/PHASE-13-CREATION-AND-CONTROL.md §13A).
	//
	// AgeRating is the one REQUIRED field here: an empty value is a validation
	// error rather than a silent default, because a rating the author never
	// chose is a claim they never made.
	AgeRating  string
	AgeGate    string
	OriginType string
	Fandom     *string

	// Extras is the collapsed ตั้งค่าเพิ่มเติม section (§13K). Every field is
	// optional; an unopened section produces the documented defaults.
	Extras ExtrasInput

	// 13U display choices. Pointers so an omitted field takes the column
	// default (show_donate in particular defaults to TRUE).
	ContentWarningSpoiler *bool
	HideCounts            *bool
	ShowDonate            *bool
	ThemeColor            *string

	// GenreIDs and TagIDs assign discovery metadata at creation
	// (docs/09 §15's genre_ids / tag_ids). Nil means none.
	GenreIDs []uuid.UUID
	TagIDs   []uuid.UUID
}

// UpdateInput is a validated partial update. A nil pointer means "leave alone";
// this is what stops a PATCH from silently resetting fields the client did not
// mention (docs/09 §3).
type UpdateInput struct {
	Title          *string
	Description    **string
	Tagline        **string
	Foreword       **string
	CoverURL       **string
	ContentWarning **string

	Status     *string
	Visibility *string

	// Creation fields (§13A). On update the rating is optional like everything
	// else - it was already required once, at creation, and a PATCH that does
	// not mention it must not reset it.
	AgeRating  *string
	AgeGate    *string
	OriginType *string
	Fandom     **string

	// Extras (§13K). Absent fields keep whatever the fiction already has.
	Extras ExtrasInput

	// 13U display choices. ThemeColor's outer pointer is presence, inner is
	// the three-case PATCH: absent / null (clear) / value.
	ContentWarningSpoiler *bool
	HideCounts            *bool
	ShowDonate            *bool
	ThemeColor            **string

	// PublishAt schedules the first publish (13U): absent leaves it, null
	// clears it, a time sets it. Only meaningful alongside an exposed
	// status/visibility - the service enforces the pairing.
	PublishAt **time.Time

	// PenNameID is which of the author's identities this work is published
	// under (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2): absent leaves it, null
	// returns the work to the author's default, an id names one. The service
	// checks the id belongs to the author before it is written.
	PenNameID **uuid.UUID

	// A non-nil slice REPLACES the whole set (an empty one clears it); nil
	// leaves the assignments untouched, like every other omitted PATCH field.
	GenreIDs *[]uuid.UUID
	TagIDs   *[]uuid.UUID
}

// dedupeIDs drops duplicates while preserving order, so sending the same
// genre twice is not a way around the per-fiction limit.
func dedupeIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// validateStatus checks a status value against the documented enumeration.
func validateStatus(errs validationErrors, raw string) Status {
	status := Status(raw)
	if !status.Valid() {
		errs.add("status", fmt.Sprintf("Must be one of: %s.", joinValues(Statuses())))
	}
	return status
}

func validateVisibility(errs validationErrors, raw string) Visibility {
	visibility := Visibility(raw)
	if !visibility.Valid() {
		errs.add("visibility", fmt.Sprintf("Must be one of: %s.", joinValues(Visibilities())))
	}
	return visibility
}

// FandomMaxLength bounds the name of the source a fanfiction is written from.
const FandomMaxLength = 120

// validateAgeRating checks the required rating (§13A, §13B).
//
// Required is the point: it is the one create-form field that decides where the
// work may appear, and a fiction that quietly defaulted to ทั่วไป because the
// author skipped a dropdown is a claim the author never made.
func validateAgeRating(errs validationErrors, raw string, required bool) AgeRating {
	if raw == "" {
		if required {
			errs.add("age_rating", fmt.Sprintf(
				"An age rating is required. Choose one of: %s.", joinValues(AgeRatings())))
		}
		return DefaultAgeRating
	}
	rating := AgeRating(raw)
	if !rating.Valid() {
		errs.add("age_rating", fmt.Sprintf("Must be one of: %s.", joinValues(AgeRatings())))
	}
	return rating
}

// validateAgeGate checks the writer's choice of how adult work is gated,
// against the rating it will be stored beside.
//
// The value is accepted whatever the rating currently is - it only takes effect
// at 18+, and keeping it means moving a work to 18+ and back does not silently
// lose the setting (§13B) - with ONE exception. Explicit work is never served
// behind a dismissible warning, so that pair is refused with a field error
// rather than quietly upgraded: a control the platform overrides in silence is
// worse than a control that says no.
//
// An OMITTED gate takes the rating's own default, which is why an explicit
// fiction created without naming one is not rejected for a field the writer
// never touched.
func validateAgeGate(errs validationErrors, raw string, rating AgeRating) AgeGate {
	if raw == "" {
		return DefaultGateFor(rating)
	}
	gate := AgeGate(raw)
	if !gate.Valid() {
		errs.add("age_gate", fmt.Sprintf("Must be one of: %s.", joinValues(AgeGates())))
		return gate
	}
	if !GateSatisfies(rating, gate) {
		errs.add("age_gate",
			"งาน 18+ เนื้อหาทางเพศชัดเจน ต้องให้ผู้อ่านล็อกอินก่อนเสมอ - เลือกล็อกอิน หรือยืนยันตัวตน")
	}
	return gate
}

// validateOrigin checks the origin/fandom pair together.
//
// The two are resolved as one because the database CHECK requires them to stay
// coherent: only a fanfiction may name a source. Original work that arrives
// with a fandom has its source DROPPED rather than being rejected - the author
// said "this is my own", and that is the answer to honour.
func validateOrigin(errs validationErrors, rawOrigin string, fandom *string) (OriginType, *string) {
	origin := OriginType(rawOrigin)
	if rawOrigin == "" {
		origin = DefaultOriginType
	} else if !origin.Valid() {
		errs.add("origin_type", fmt.Sprintf("Must be one of: %s.", joinValues(OriginTypes())))
		return origin, nil
	}

	if origin != OriginFanfiction {
		return origin, nil
	}
	if fandom == nil {
		return origin, nil
	}

	trimmed := strings.TrimSpace(*fandom)
	if trimmed == "" {
		return origin, nil
	}
	switch {
	case runeLen(trimmed) > FandomMaxLength:
		errs.add("fandom", fmt.Sprintf("Must be at most %d characters.", FandomMaxLength))
	case !singleLine(trimmed):
		errs.add("fandom", "Contains characters that are not allowed.")
	}
	return origin, &trimmed
}

func joinValues[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

// Ref identifies a fiction in a URL.
//
// docs/08 §35 uses slugs in public URLs and UUIDs internally, while docs/09 §15
// shows reads by slug and writes by id. Both resolve to the same row, so one
// path parameter accepts either form and the service decides authorization the
// same way regardless of which was used.
type Ref struct {
	ID   uuid.UUID
	Slug string
}

// BySlug reports whether the reference is a slug rather than a UUID.
func (r Ref) BySlug() bool { return r.ID == uuid.Nil }

// ParseRef interprets a path parameter as a UUID or a slug.
//
// A malformed reference is a 404, not a 400: telling a caller that an
// identifier is well-formed but absent, versus malformed, is a distinction
// worth denying to anyone probing for content (docs/11 §3.4).
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

// applyOrigin resolves the origin/fandom pair together on update.
//
// They cannot be written independently: the CHECK constraint allows a fandom
// only on a fanfiction. Resolving both here means the service never sends a
// half state for PostgreSQL to reject, and it settles the two cases a writer
// actually hits - switching a work to "original" clears the source it no
// longer has, and naming a source on a work that is already original is
// refused with a field error rather than silently stored.
func applyOrigin(errs validationErrors, novel *Novel, input UpdateInput, params *UpdateParams) {
	if input.OriginType == nil && input.Fandom == nil {
		return
	}

	origin := novel.OriginType
	if input.OriginType != nil {
		origin = OriginType(*input.OriginType)
		if !origin.Valid() {
			errs.add("origin_type", fmt.Sprintf("Must be one of: %s.", joinValues(OriginTypes())))
			return
		}
		params.OriginType = &origin
	}

	fandom := novel.Fandom
	if input.Fandom != nil {
		fandom = *input.Fandom
	}

	// Original work names no source. Clearing it as a consequence of the
	// writer's own choice is correct; being told to set one is not.
	if origin != OriginFanfiction {
		if input.Fandom != nil && fandom != nil && strings.TrimSpace(*fandom) != "" {
			errs.add("fandom", "Only a fanfiction can name a source.")
			return
		}
		if novel.Fandom != nil || input.Fandom != nil {
			var cleared *string
			params.Fandom = &cleared
		}
		return
	}

	if input.Fandom == nil {
		return
	}
	if fandom == nil {
		params.Fandom = &fandom
		return
	}

	trimmed := strings.TrimSpace(*fandom)
	if trimmed == "" {
		var cleared *string
		params.Fandom = &cleared
		return
	}
	switch {
	case runeLen(trimmed) > FandomMaxLength:
		errs.add("fandom", fmt.Sprintf("Must be at most %d characters.", FandomMaxLength))
		return
	case !singleLine(trimmed):
		errs.add("fandom", "Contains characters that are not allowed.")
		return
	}
	value := &trimmed
	params.Fandom = &value
}
