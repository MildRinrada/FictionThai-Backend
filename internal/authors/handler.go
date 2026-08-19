package authors

import (
	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes the self-scoped author-profile endpoints.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func identityOf(c *gin.Context) *auth.Identity {
	return auth.IdentityFrom(c.Request.Context())
}

// GetMine handles GET /api/v1/me/author-profile - the caller's own profile
// (currently just the donation link).
func (h *Handler) GetMine(c *gin.Context) {
	view, err := h.service.GetMine(c.Request.Context(), identityOf(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Update handles PUT /api/v1/me/author-profile - set or clear the caller's
// external donation URL. An absent or null donation_url clears it.
func (h *Handler) Update(c *gin.Context) {
	var body struct {
		DonationURL *string `json:"donation_url"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, apierror.BadRequest("Malformed request body."))
		return
	}
	view, err := h.service.SetDonationURL(c.Request.Context(), identityOf(c), body.DonationURL)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}
