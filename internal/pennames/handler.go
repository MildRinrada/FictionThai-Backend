package pennames

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes the self-scoped pen-name endpoints. It parses and shapes;
// every decision belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

type createRequest struct {
	Name      string  `json:"name"`
	Note      *string `json:"note"`
	IsDefault bool    `json:"is_default"`
}

// updateRequest is a partial update.
//
// note is json.RawMessage so a PATCH can distinguish absent from null; a plain
// *string collapses the two and would let a rename wipe the writer's own label
// for what the identity is for (docs/09 §3).
type updateRequest struct {
	Name      *string         `json:"name"`
	Note      json.RawMessage `json:"note"`
	IsDefault *bool           `json:"is_default"`
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
		// An emptied box in a form is a deliberate clear; storing "" and NULL as
		// different states would be a distinction without a difference.
		var cleared *string
		return &cleared, nil
	}
	pointer := &trimmed
	return &pointer, nil
}

func identityOf(c *gin.Context) *auth.Identity {
	return auth.IdentityFrom(c.Request.Context())
}

func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		response.Fail(c, apierror.BadRequest("The request body could not be parsed as JSON."))
		return false
	}
	return true
}

// ref reads the pen name id from the path. A malformed id is the same 404 an
// unknown one gets - the endpoint must not distinguish the two.
func ref(c *gin.Context) (uuid.UUID, bool) {
	id, err := ParseID(c.Param(RefParam))
	if err != nil {
		response.Fail(c, notFound())
		return uuid.Nil, false
	}
	return id, true
}

// List handles GET /api/v1/me/pen-names - the caller's own identities.
func (h *Handler) List(c *gin.Context) {
	views, err := h.service.List(c.Request.Context(), identityOf(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, views)
}

// Create handles POST /api/v1/me/pen-names.
func (h *Handler) Create(c *gin.Context) {
	var req createRequest
	if !bindJSON(c, &req) {
		return
	}

	view, err := h.service.Create(c.Request.Context(), identityOf(c), CreateInput{
		Name:      req.Name,
		Note:      req.Note,
		IsDefault: req.IsDefault,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// Update handles PATCH /api/v1/me/pen-names/:pen_name - rename, re-label, or
// make this identity the default.
func (h *Handler) Update(c *gin.Context) {
	id, ok := ref(c)
	if !ok {
		return
	}

	var req updateRequest
	if !bindJSON(c, &req) {
		return
	}

	note, err := nullableString(req.Note)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"note": {"Must be a string or null."},
		}))
		return
	}

	view, serviceErr := h.service.Update(c.Request.Context(), identityOf(c), id, UpdateInput{
		Name:      req.Name,
		Note:      note,
		IsDefault: req.IsDefault,
	})
	if serviceErr != nil {
		response.Fail(c, serviceErr)
		return
	}
	response.OK(c, view)
}

// Delete handles DELETE /api/v1/me/pen-names/:pen_name.
//
// It removes an IDENTITY, never a work: the fictions published under this name
// keep every word and fall back to the writer's default name. 204, because
// there is nothing meaningful to return about a thing that no longer exists.
func (h *Handler) Delete(c *gin.Context) {
	id, ok := ref(c)
	if !ok {
		return
	}

	if err := h.service.Delete(c.Request.Context(), identityOf(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}
