package wall

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes the wall endpoints. It parses and shapes; every decision
// belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// entryRequest is the one writable field.
type entryRequest struct {
	Body string `json:"body"`
}

// List handles GET /users/:user/wall - public, paginated, and absent entirely
// when the owner has the wall switched off.
func (h *Handler) List(c *gin.Context) {
	views, meta, err := h.service.List(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()),
		c.Param(UserRefParam), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// Post handles POST /users/:user/wall.
func (h *Handler) Post(c *gin.Context) {
	var req entryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apierror.BadRequest("The request body is not valid JSON."))
		return
	}
	view, err := h.service.Post(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()),
		c.Param(UserRefParam), req.Body)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// Delete handles DELETE /wall/:entry - the author or the profile owner.
func (h *Handler) Delete(c *gin.Context) {
	id, err := uuid.Parse(c.Param(RefParam))
	if err != nil {
		// A malformed id is the same 404 a missing entry gets (docs/11 §3.4).
		response.Fail(c, entryNotFound())
		return
	}
	if err := h.service.Delete(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}
