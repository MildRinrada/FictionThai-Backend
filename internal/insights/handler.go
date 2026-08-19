package insights

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes GET /novels/:novel/insights. It parses and shapes; every
// decision, ownership included, belongs to the service.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Get handles GET /api/v1/novels/:novel/insights - owner only.
//
// A read that is not public, so it is mounted with the authenticated writes
// rather than on the guest-first read group. A caller who may not write the
// fiction gets the same 404 a stranger gets for a private draft.
func (h *Handler) Get(c *gin.Context) {
	ref, err := novels.ParseRef(c.Param(novels.RefParam))
	if err != nil {
		// The same body the novels service returns for a fiction the caller may
		// not see, so an unparseable ref is not distinguishable from a private
		// one (docs/11 §3.4).
		response.Fail(c, apierror.New(
			http.StatusNotFound, "NOVEL_NOT_FOUND", "Fiction not found."))
		return
	}

	view, err := h.service.For(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}
