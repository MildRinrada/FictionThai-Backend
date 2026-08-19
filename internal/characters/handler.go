package characters

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RefParam is the path parameter name for a character id.
const RefParam = "character"

// Handler exposes the character endpoints. It parses and shapes; every decision
// belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

type createRequest struct {
	Name        string   `json:"name"`
	Role        *string  `json:"role"`
	Summary     *string  `json:"summary"`
	AvatarURL   *string  `json:"avatar_url"`
	Description *string  `json:"description"`
	Quote       *string  `json:"quote"`
	Traits      []string `json:"traits"`
	Details     []Detail `json:"details"`

	FirstChapterID *string `json:"first_chapter_id"`
}

// updateRequest is a partial update.
//
// The nullable text fields are json.RawMessage so a PATCH can distinguish absent
// from null; a plain *string collapses the two and would let a rename wipe a
// backstory (docs/09 §3).
type updateRequest struct {
	Name        *string         `json:"name"`
	Role        json.RawMessage `json:"role"`
	Summary     json.RawMessage `json:"summary"`
	AvatarURL   json.RawMessage `json:"avatar_url"`
	Description json.RawMessage `json:"description"`
	Quote       json.RawMessage `json:"quote"`

	Traits  *[]string `json:"traits"`
	Details *[]Detail `json:"details"`

	ChatColor       json.RawMessage `json:"chat_color"`
	ChatSide        json.RawMessage `json:"chat_side"`
	ChatDisplayName json.RawMessage `json:"chat_display_name"`

	FirstChapterID json.RawMessage `json:"first_chapter_id"`
}

type reorderRequest struct {
	CharacterIDs []string `json:"character_ids"`
}

type appearancesRequest struct {
	ChapterIDs []string `json:"chapter_ids"`
}

// nullableString interprets a raw JSON field as absent / null / value.
func nullableString(raw json.RawMessage) (**string, error) {
	if len(raw) == 0 {
		return nil, nil // absent
	}
	if string(raw) == "null" {
		var cleared *string
		return &cleared, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		var cleared *string
		return &cleared, nil
	}
	pointer := &trimmed
	return &pointer, nil
}

// nullableUUID interprets a raw JSON field as absent / null / an id.
func nullableUUID(raw json.RawMessage) (**uuid.UUID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if string(raw) == "null" {
		var cleared *uuid.UUID
		return &cleared, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	pointer := &id
	return &pointer, nil
}

func parseIDList(raw []string, field string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(raw))
	for _, value := range raw {
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			return nil, apierror.Validation(map[string][]string{
				field: {"Must be a list of valid ids."},
			})
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// List handles GET /api/v1/novels/:novel/characters. Guests are welcome for
// public fiction.
func (h *Handler) List(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}

	views, err := h.service.List(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// Not a paginated collection: a cast is a small, complete, author-ordered
	// set, and paginating it would let a page boundary hide a character.
	response.OK(c, views)
}

// Get handles GET /api/v1/novels/:novel/characters/:character.
func (h *Handler) Get(c *gin.Context) {
	ref, id, ok := refs(c)
	if !ok {
		return
	}

	view, err := h.service.Get(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Create handles POST /api/v1/novels/:novel/characters. Owner only.
func (h *Handler) Create(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}

	var req createRequest
	if !bindJSON(c, &req) {
		return
	}

	input := CreateInput{
		Name:        req.Name,
		Role:        req.Role,
		Summary:     req.Summary,
		AvatarURL:   req.AvatarURL,
		Description: req.Description,
		Quote:       req.Quote,
		Traits:      req.Traits,
		Details:     req.Details,
	}
	if req.FirstChapterID != nil {
		id, err := uuid.Parse(strings.TrimSpace(*req.FirstChapterID))
		if err != nil {
			response.Fail(c, apierror.Validation(map[string][]string{
				"first_chapter_id": {"Must be a valid id."},
			}))
			return
		}
		input.FirstChapterID = &id
	}

	view, err := h.service.Create(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, input)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// Update handles PATCH /api/v1/novels/:novel/characters/:character. Owner only.
func (h *Handler) Update(c *gin.Context) {
	ref, id, ok := refs(c)
	if !ok {
		return
	}

	var req updateRequest
	if !bindJSON(c, &req) {
		return
	}

	input := UpdateInput{Name: req.Name, Traits: req.Traits, Details: req.Details}

	for _, field := range []struct {
		name   string
		raw    json.RawMessage
		target *(**string)
	}{
		{"role", req.Role, &input.Role},
		{"summary", req.Summary, &input.Summary},
		{"avatar_url", req.AvatarURL, &input.AvatarURL},
		{"description", req.Description, &input.Description},
		{"quote", req.Quote, &input.Quote},
		{"chat_color", req.ChatColor, &input.ChatColor},
		{"chat_side", req.ChatSide, &input.ChatSide},
		{"chat_display_name", req.ChatDisplayName, &input.ChatDisplayName},
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

	firstChapter, err := nullableUUID(req.FirstChapterID)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"first_chapter_id": {"Must be a valid id or null."},
		}))
		return
	}
	input.FirstChapterID = firstChapter

	view, serviceErr := h.service.Update(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, id, input)
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, view)
}

// Delete handles DELETE /api/v1/novels/:novel/characters/:character. Owner only.
func (h *Handler) Delete(c *gin.Context) {
	ref, id, ok := refs(c)
	if !ok {
		return
	}

	if err := h.service.Delete(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Reorder handles PUT /api/v1/novels/:novel/characters/order. Owner only.
func (h *Handler) Reorder(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}

	var req reorderRequest
	if !bindJSON(c, &req) {
		return
	}

	ids, err := parseIDList(req.CharacterIDs, "character_ids")
	if err != nil {
		response.Fail(c, err)
		return
	}

	views, serviceErr := h.service.Reorder(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, ids)
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, views)
}

// SetAppearances handles PUT
// /api/v1/novels/:novel/characters/:character/appearances. Owner only.
func (h *Handler) SetAppearances(c *gin.Context) {
	ref, id, ok := refs(c)
	if !ok {
		return
	}

	var req appearancesRequest
	if !bindJSON(c, &req) {
		return
	}

	chapterIDs, err := parseIDList(req.ChapterIDs, "chapter_ids")
	if err != nil {
		response.Fail(c, err)
		return
	}

	view, serviceErr := h.service.SetAppearances(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, id, chapterIDs)
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, view)
}

func novelRef(c *gin.Context) (novels.Ref, bool) {
	ref, err := novels.ParseRef(c.Param(novels.RefParam))
	if err != nil {
		response.Fail(c, apierror.New(http.StatusNotFound, "NOVEL_NOT_FOUND", "Fiction not found."))
		return novels.Ref{}, false
	}
	return ref, true
}

func refs(c *gin.Context) (novels.Ref, uuid.UUID, bool) {
	ref, ok := novelRef(c)
	if !ok {
		return novels.Ref{}, uuid.Nil, false
	}
	id, err := ParseID(c.Param(RefParam))
	if err != nil {
		response.Fail(c, notFound())
		return novels.Ref{}, uuid.Nil, false
	}
	return ref, id, true
}

func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
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
