package novels

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/fiction"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RefParam is the path parameter name for a fiction reference.
//
// One name serves the whole /novels subtree because Gin requires a single
// wildcard name per path position. It accepts a slug or a UUID (see ParseRef),
// which reconciles docs/09 §15 reading by slug with writing by id.
const RefParam = "novel"

// Handler exposes the fiction endpoints. It parses and shapes; every decision
// belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// createRequest - docs/09 §15 "Create Novel".
//
// The format dimensions are pointers so an omitted one takes its documented
// default instead of arriving as an empty string that fails validation.
type createRequest struct {
	Title          string  `json:"title"`
	Description    *string `json:"description"`
	Tagline        *string `json:"tagline"`
	Foreword       *string `json:"foreword"`
	CoverURL       *string `json:"cover_url"`
	ContentWarning *string `json:"content_warning"`

	StoryStructure     *string `json:"story_structure"`
	PresentationFormat *string `json:"presentation_format"`
	ContentMode        *string `json:"content_mode"`

	Status     *string `json:"status"`
	Visibility *string `json:"visibility"`

	// Creation fields (docs/PHASE-13-CREATION-AND-CONTROL.md §13A).
	// age_rating is a plain string rather than a pointer because it is
	// REQUIRED: an absent field and an empty one are the same mistake here,
	// and both get the same field error.
	AgeRating  string  `json:"age_rating"`
	AgeGate    string  `json:"age_gate"`
	OriginType string  `json:"origin_type"`
	Fandom     *string `json:"fandom"`

	// ตั้งค่าเพิ่มเติม (§13K). Flat on the body, not nested, so the form sends
	// one object and the fields read the same way they do on the response.
	extrasRequest

	// 13U display choices.
	ContentWarningSpoiler *bool   `json:"content_warning_spoiler"`
	HideCounts            *bool   `json:"hide_counts"`
	ShowDonate            *bool   `json:"show_donate"`
	ThemeColor            *string `json:"theme_color"`

	// Discovery metadata (docs/09 §15's genre_ids / tag_ids).
	GenreIDs []string `json:"genre_ids"`
	TagIDs   []string `json:"tag_ids"`
}

// extrasRequest is the collapsed ตั้งค่าเพิ่มเติม section (§13K).
//
// Every field is a pointer so an absent one keeps whatever the fiction already
// has - the section is optional by design, and a writer who never opens it must
// not have it answered for them.
type extrasRequest struct {
	Language        *string `json:"language"`
	ChapterUnit     *string `json:"chapter_unit"`
	AuthorNoteStart *string `json:"author_note_start"`
	AuthorNoteEnd   *string `json:"author_note_end"`
	SeriesName      *string `json:"series_name"`
	SeriesPosition  *int    `json:"series_position"`

	// The three-level comment switch and its review queue (§13D). Together,
	// because shipping the levels without the queue produces the outcome the
	// levels exist to prevent.
	CommentAccess   *string `json:"comment_access"`
	CommentApproval *bool   `json:"comment_approval"`

	// The author's stated permissions (§13E). Declarations, never enforcement.
	AllowScreenshot  *bool   `json:"allow_screenshot"`
	AllowTranslation *bool   `json:"allow_translation"`
	AllowDerivative  *bool   `json:"allow_derivative"`
	AllowAudio       *bool   `json:"allow_audio"`
	RequireCredit    *bool   `json:"require_credit"`
	DerivativeTerms  *string `json:"derivative_terms"`
}

func (r extrasRequest) input() ExtrasInput {
	return ExtrasInput{
		Language:         r.Language,
		ChapterUnit:      r.ChapterUnit,
		AuthorNoteStart:  r.AuthorNoteStart,
		AuthorNoteEnd:    r.AuthorNoteEnd,
		SeriesName:       r.SeriesName,
		SeriesPosition:   r.SeriesPosition,
		CommentAccess:    r.CommentAccess,
		CommentApproval:  r.CommentApproval,
		AllowScreenshot:  r.AllowScreenshot,
		AllowTranslation: r.AllowTranslation,
		AllowDerivative:  r.AllowDerivative,
		AllowAudio:       r.AllowAudio,
		RequireCredit:    r.RequireCredit,
		DerivativeTerms:  r.DerivativeTerms,
	}
}

// parseIDList turns a JSON array of id strings into UUIDs, reporting one field
// error for any malformed entry.
func parseIDList(errs map[string][]string, field string, raw []string) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(raw))
	for _, value := range raw {
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			errs[field] = append(errs[field], "Must be a list of valid ids.")
			return nil
		}
		ids = append(ids, id)
	}
	return ids
}

// updateRequest is a partial update.
//
// json.RawMessage is used for the nullable text fields because a PATCH must
// distinguish three cases a plain *string cannot: absent (leave alone), null
// (clear it), and a value. Collapsing them would let a PATCH that mentions only
// the title silently wipe the description (docs/09 §3).
type updateRequest struct {
	Title          *string         `json:"title"`
	Description    json.RawMessage `json:"description"`
	Tagline        json.RawMessage `json:"tagline"`
	Foreword       json.RawMessage `json:"foreword"`
	CoverURL       json.RawMessage `json:"cover_url"`
	ContentWarning json.RawMessage `json:"content_warning"`
	Status         *string         `json:"status"`
	Visibility     *string         `json:"visibility"`

	// Creation fields (§13A). fandom is raw JSON like the other nullable text:
	// a work that stops being fanfiction clears it, which null expresses and a
	// *string cannot.
	AgeRating  *string         `json:"age_rating"`
	AgeGate    *string         `json:"age_gate"`
	OriginType *string         `json:"origin_type"`
	Fandom     json.RawMessage `json:"fandom"`

	// ตั้งค่าเพิ่มเติม (§13K).
	extrasRequest

	// 13U display choices. theme_color and publish_at are raw JSON for the
	// same three-case reason as the nullable text fields.
	ContentWarningSpoiler *bool           `json:"content_warning_spoiler"`
	HideCounts            *bool           `json:"hide_counts"`
	ShowDonate            *bool           `json:"show_donate"`
	ThemeColor            json.RawMessage `json:"theme_color"`
	PublishAt             json.RawMessage `json:"publish_at"`

	// นามปากกา (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2). Raw JSON for the same
	// three-case reason: absent leaves the choice alone, null returns the work
	// to the author's default, an id names one of their identities.
	PenNameID json.RawMessage `json:"pen_name_id"`

	// Pointers so an omitted list leaves the assignments untouched while a
	// present one (including []) replaces the whole set (docs/09 §3).
	GenreIDs *[]string `json:"genre_ids"`
	TagIDs   *[]string `json:"tag_ids"`
}

// nullableString interprets a raw JSON field as absent / null / value.
func nullableString(raw json.RawMessage) (**string, error) {
	if len(raw) == 0 {
		return nil, nil // absent
	}
	if string(raw) == "null" {
		var cleared *string
		return &cleared, nil // explicit null
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		// An empty string means the writer cleared the field in a form. Storing
		// "" and NULL as different states would be a distinction without a
		// difference for free text.
		var cleared *string
		return &cleared, nil
	}
	pointer := &trimmed
	return &pointer, nil
}

// listQueryFrom collects the shared listing parameters (docs/09 §10, §11).
func listQueryFrom(c *gin.Context) ListQuery {
	return ListQuery{
		Query:              c.Query("q"),
		StoryStructure:     c.Query("story_structure"),
		PresentationFormat: c.Query("presentation_format"),
		ContentMode:        c.Query("content_mode"),
		Status:             c.Query("status"),
		Sort:               c.Query("sort"),
		Genre:              c.Query("genre"),
		Tag:                c.Query("tag"),
		Author:             c.Query("author"),
		// "ซ่อนเนื้อหา 18+" turned off by the reader (§13B). A plain query
		// parameter is safe because the SERVICE ignores it for a guest and for
		// explicit work - it is a request, not a decision.
		IncludeAdult: c.Query("adult") == "1",
		// The caller's own co-written shelf (13U). Also a plain parameter: it
		// resolves against the identity only, and a guest gets an empty page.
		CoWriter: c.Query("co_writer") == "me",

		// The reader-side dimensions of the 2026-08 search rework. All plain
		// parameters; the service validates every one.
		Rating:         c.Query("rating"),
		Origin:         c.Query("origin"),
		Fandom:         c.Query("fandom"),
		Character:      c.Query("character"),
		ExcludeTag:     c.Query("exclude_tag"),
		ExcludeWarning: c.Query("exclude_warning"),
		MinChapters:    c.Query("min_chapters"),
		MaxChapters:    c.Query("max_chapters"),
		UpdatedWithin:  c.Query("updated_within"),
		HasVariables:   c.Query("variables") == "1",
	}
}

// List handles GET /api/v1/novels. Guests are welcome (docs/09 §6).
func (h *Handler) List(c *gin.Context) {
	identity := auth.IdentityFrom(c.Request.Context())

	views, meta, err := h.service.List(c.Request.Context(), identity,
		listQueryFrom(c), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// Search handles GET /api/v1/search/novels (docs/09 §22). Guests are welcome;
// the widened match scope is docs/01 §7.
func (h *Handler) Search(c *gin.Context) {
	identity := auth.IdentityFrom(c.Request.Context())

	views, meta, err := h.service.Search(c.Request.Context(), identity,
		listQueryFrom(c), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// SearchFacets handles GET /api/v1/search/facets (search review 2026-08
// section A): per-option match counts for the filter panel, under the same
// parameters the search takes. Guests are welcome.
func (h *Handler) SearchFacets(c *gin.Context) {
	identity := auth.IdentityFrom(c.Request.Context())

	facets, err := h.service.SearchFacets(c.Request.Context(), identity, listQueryFrom(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, facets)
}

// Get handles GET /api/v1/novels/:novel. Guests are welcome for public work.
func (h *Handler) Get(c *gin.Context) {
	ref, ok := h.ref(c)
	if !ok {
		return
	}

	view, err := h.service.Get(c.Request.Context(), auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Create handles POST /api/v1/novels. Authentication required.
func (h *Handler) Create(c *gin.Context) {
	var req createRequest
	if !bindJSON(c, &req) {
		return
	}

	idErrs := map[string][]string{}
	genreIDs := parseIDList(idErrs, "genre_ids", req.GenreIDs)
	tagIDs := parseIDList(idErrs, "tag_ids", req.TagIDs)
	if len(idErrs) > 0 {
		response.Fail(c, apierror.Validation(idErrs))
		return
	}

	record, err := h.service.Create(c.Request.Context(), auth.IdentityFrom(c.Request.Context()),
		CreateInput{
			Title:                 req.Title,
			Description:           req.Description,
			Tagline:               req.Tagline,
			Foreword:              req.Foreword,
			CoverURL:              req.CoverURL,
			ContentWarning:        req.ContentWarning,
			StoryStructure:        req.StoryStructure,
			PresentationFormat:    req.PresentationFormat,
			ContentMode:           req.ContentMode,
			Extras:                req.extrasRequest.input(),
			Status:                req.Status,
			Visibility:            req.Visibility,
			AgeRating:             req.AgeRating,
			AgeGate:               req.AgeGate,
			OriginType:            req.OriginType,
			Fandom:                req.Fandom,
			ContentWarningSpoiler: req.ContentWarningSpoiler,
			HideCounts:            req.HideCounts,
			ShowDonate:            req.ShowDonate,
			ThemeColor:            req.ThemeColor,
			GenreIDs:              genreIDs,
			TagIDs:                tagIDs,
		})
	if err != nil {
		response.Fail(c, err)
		return
	}

	// The creator is by definition the owner, so they get the owner view.
	response.Created(c, record.ViewFor(true))
}

// Update handles PATCH /api/v1/novels/:novel. Owner or staff only.
func (h *Handler) Update(c *gin.Context) {
	ref, ok := h.ref(c)
	if !ok {
		return
	}

	var req updateRequest
	if !bindJSON(c, &req) {
		return
	}

	input := UpdateInput{
		Title:                 req.Title,
		Status:                req.Status,
		Visibility:            req.Visibility,
		AgeRating:             req.AgeRating,
		AgeGate:               req.AgeGate,
		OriginType:            req.OriginType,
		Extras:                req.extrasRequest.input(),
		ContentWarningSpoiler: req.ContentWarningSpoiler,
		HideCounts:            req.HideCounts,
		ShowDonate:            req.ShowDonate,
	}

	// theme_color: absent / null / value (docs/09 §3).
	themeColor, err := nullableString(req.ThemeColor)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"theme_color": {"Must be a string or null."},
		}))
		return
	}
	input.ThemeColor = themeColor

	// publish_at: absent / null (cancel the schedule) / RFC 3339 time (13U).
	if len(req.PublishAt) > 0 {
		if string(req.PublishAt) == "null" {
			var cleared *time.Time
			input.PublishAt = &cleared
		} else {
			var when time.Time
			if err := json.Unmarshal(req.PublishAt, &when); err != nil {
				response.Fail(c, apierror.Validation(map[string][]string{
					"publish_at": {"Must be an RFC 3339 timestamp or null."},
				}))
				return
			}
			pointer := &when
			input.PublishAt = &pointer
		}
	}

	// pen_name_id: absent / null (back to the default) / an id.
	if len(req.PenNameID) > 0 {
		if string(req.PenNameID) == "null" {
			var cleared *uuid.UUID
			input.PenNameID = &cleared
		} else {
			var raw string
			if err := json.Unmarshal(req.PenNameID, &raw); err != nil {
				response.Fail(c, apierror.Validation(map[string][]string{
					"pen_name_id": {"Must be a valid id or null."},
				}))
				return
			}
			id, err := uuid.Parse(strings.TrimSpace(raw))
			if err != nil {
				response.Fail(c, apierror.Validation(map[string][]string{
					"pen_name_id": {"Must be a valid id or null."},
				}))
				return
			}
			pointer := &id
			input.PenNameID = &pointer
		}
	}

	idErrs := map[string][]string{}
	if req.GenreIDs != nil {
		ids := parseIDList(idErrs, "genre_ids", *req.GenreIDs)
		input.GenreIDs = &ids
	}
	if req.TagIDs != nil {
		ids := parseIDList(idErrs, "tag_ids", *req.TagIDs)
		input.TagIDs = &ids
	}
	if len(idErrs) > 0 {
		response.Fail(c, apierror.Validation(idErrs))
		return
	}

	for _, field := range []struct {
		name   string
		raw    json.RawMessage
		target *(**string)
	}{
		{"description", req.Description, &input.Description},
		{"tagline", req.Tagline, &input.Tagline},
		{"foreword", req.Foreword, &input.Foreword},
		{"cover_url", req.CoverURL, &input.CoverURL},
		{"content_warning", req.ContentWarning, &input.ContentWarning},
		{"fandom", req.Fandom, &input.Fandom},
	} {
		value, err := nullableString(field.raw)
		if err != nil {
			response.Fail(c, apierror.Validation(map[string][]string{
				field.name: {"Must be a string or null."},
			}))
			return
		}
		*field.target = value
	}

	view, err := h.service.Update(c.Request.Context(), auth.IdentityFrom(c.Request.Context()), ref, input)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// UpdateFormat handles PATCH /api/v1/novels/:novel/format (docs/09 §14.7).
//
// Every dimension is optional; an omitted one keeps its current value.
// Readiness handles GET /api/v1/novels/:novel/readiness (§13L).
//
// Owner-only, and a pure read: it is the work list for the person doing the
// work, and looking at it must never change anything.
func (h *Handler) Readiness(c *gin.Context) {
	ref, ok := h.ref(c)
	if !ok {
		return
	}

	readiness, err := h.service.Readiness(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, readiness)
}

func (h *Handler) UpdateFormat(c *gin.Context) {
	ref, ok := h.ref(c)
	if !ok {
		return
	}

	var patch fiction.Patch
	if !bindJSON(c, &patch) {
		return
	}

	view, err := h.service.UpdateFormat(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, patch)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Delete handles DELETE /api/v1/novels/:novel. Owner or staff only.
func (h *Handler) Delete(c *gin.Context) {
	ref, ok := h.ref(c)
	if !ok {
		return
	}

	if err := h.service.Delete(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// ListCollaborators handles GET /api/v1/novels/:novel/collaborators (13U).
func (h *Handler) ListCollaborators(c *gin.Context) {
	ref, ok := h.ref(c)
	if !ok {
		return
	}

	credits, err := h.service.ListCollaborators(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"collaborators": credits})
}

// AddCollaborator handles POST /api/v1/novels/:novel/collaborators (13U).
// Owner only. Returns the resulting list, so the settings page never has to
// guess what the write produced.
func (h *Handler) AddCollaborator(c *gin.Context) {
	ref, ok := h.ref(c)
	if !ok {
		return
	}

	var req struct {
		Username string `json:"username"`
		Credit   string `json:"credit"`
	}
	if !bindJSON(c, &req) {
		return
	}

	credits, err := h.service.AddCollaborator(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, req.Username, req.Credit)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"collaborators": credits})
}

// RemoveCollaborator handles DELETE /api/v1/novels/:novel/collaborators/:username.
func (h *Handler) RemoveCollaborator(c *gin.Context) {
	ref, ok := h.ref(c)
	if !ok {
		return
	}

	if err := h.service.RemoveCollaborator(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, c.Param("collaborator")); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// ref parses the path parameter, writing a 404 and reporting false on failure.
func (h *Handler) ref(c *gin.Context) (Ref, bool) {
	ref, err := ParseRef(c.Param(RefParam))
	if err != nil {
		response.Fail(c, notFound())
		return Ref{}, false
	}
	return ref, true
}

// bindJSON decodes a request body, reporting a clean error on malformed input.
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		// An oversized body surfaces here, because the limit is enforced when
		// the body is read (middleware.BodyLimit).
		if strings.Contains(err.Error(), "http: request body too large") {
			response.Fail(c, apierror.New(http.StatusRequestEntityTooLarge,
				apierror.CodePayloadTooLarge, "The request body is too large."))
			return false
		}
		response.Fail(c, apierror.BadRequest("The request body could not be parsed as JSON."))
		return false
	}
	return true
}
