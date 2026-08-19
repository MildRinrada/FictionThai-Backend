package profiles

import (
	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes the public profile read. It parses and shapes; every
// decision belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Get handles GET /users/:user - a username or an id
// (docs/PHASE-12-STORY-DEPTH.md §12E).
func (h *Handler) Get(c *gin.Context) {
	profile, err := h.service.Get(c.Request.Context(), c.Param(RefParam))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, profile)
}

// UpdateMine handles PATCH /me/profile - the caller edits their own profile
// (docs/PROFILE-AND-ACHIEVEMENTS.md Part 1). The response is the PUBLIC view,
// so the settings screen can show the writer what everyone else will see.
func (h *Handler) UpdateMine(c *gin.Context) {
	var edit Edit
	if err := c.ShouldBindJSON(&edit); err != nil {
		response.Fail(c, apierror.BadRequest("Malformed request body."))
		return
	}
	profile, err := h.service.Update(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), &edit)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, profile)
}

// Spotlight handles GET /writers/spotlight - the home page's writer band
// (docs/WRITER-SPOTLIGHT.md). Public, identity-free, and identical for every
// viewer, like Get.
func (h *Handler) Spotlight(c *gin.Context) {
	view, err := h.service.Spotlight(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// SearchAuthors handles GET /search/authors?q= - the people half of search.
func (h *Handler) SearchAuthors(c *gin.Context) {
	authors, err := h.service.SearchAuthors(c.Request.Context(), c.Query("q"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, authors)
}
