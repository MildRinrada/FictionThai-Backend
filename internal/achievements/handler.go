package achievements

import (
	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler parses and shapes; every decision belongs to the service
// (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func badBody() *apierror.Error { return apierror.BadRequest("Malformed request body.") }

// Mine handles GET /me/achievements - the owner's own view, with progress.
func (h *Handler) Mine(c *gin.Context) {
	view, err := h.service.Mine(c.Request.Context(), auth.IdentityFrom(c.Request.Context()))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Public handles GET /users/:user/achievements - the visitor's view: what the
// owner chose to show, plus counts. Never an egg's name.
func (h *Handler) Public(c *gin.Context) {
	view, err := h.service.Public(c.Request.Context(), c.Param(RefParam))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// SetShowcase handles PUT /me/achievements/showcase.
func (h *Handler) SetShowcase(c *gin.Context) {
	var body struct {
		Keys []string `json:"keys"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.service.SetShowcase(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), body.Keys)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// SetPrefs handles PUT /me/achievements/prefs - the global off switch.
func (h *Handler) SetPrefs(c *gin.Context) {
	var body Prefs
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.service.SetPrefs(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), body)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Signal handles POST /achievements/signal - the four cosmetic eggs only.
func (h *Handler) Signal(c *gin.Context) {
	var body struct {
		Key string `json:"key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	result, err := h.service.Signal(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), body.Key)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}
