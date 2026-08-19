package desk

import (
	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes GET /me/desk. It shapes nothing and decides nothing; the
// service owns the whole answer.
type Handler struct {
	service *Service
}

// NewHandler wires the handler to its service.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Mine handles GET /api/v1/me/desk.
//
// There is no id in the path and no query parameter, on purpose: the only
// account this endpoint can describe is the one holding the session.
func (h *Handler) Mine(c *gin.Context) {
	view, err := h.service.Mine(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Search handles GET /api/v1/me/desk/search?q= - the writer's own work.
func (h *Handler) Search(c *gin.Context) {
	hits, err := h.service.Search(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), c.Query("q"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, hits)
}
