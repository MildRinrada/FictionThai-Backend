package variables

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes the reader-variable endpoints. It parses and shapes; every
// decision belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// replaceRequest carries the WHOLE declaration list.
//
// A PUT rather than a PATCH, and a list rather than per-row routes, because
// order is the order a reader is asked in: a partial update could leave two
// variables claiming one position, and the reader would be asked a question
// twice.
type replaceRequest struct {
	Variables []Input `json:"variables"`
}

func novelRefFrom(c *gin.Context) (novels.Ref, bool) {
	ref, err := novels.ParseRef(c.Param(novels.RefParam))
	if err != nil {
		// A malformed reference is the SAME 404 an absent fiction gets:
		// distinguishing them is information worth denying (docs/11 §3.4).
		response.Fail(c, apierror.New(http.StatusNotFound, "NOVEL_NOT_FOUND", "Fiction not found."))
		return novels.Ref{}, false
	}
	return ref, true
}

// List handles GET /api/v1/novels/:novel/variables. Public, under the fiction's
// own gate - a guest reading a reader-insert fiction needs the questions.
func (h *Handler) List(c *gin.Context) {
	ref, ok := novelRefFrom(c)
	if !ok {
		return
	}

	result, err := h.service.List(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// Replace handles PUT /api/v1/novels/:novel/variables. Owner or staff only.
func (h *Handler) Replace(c *gin.Context) {
	ref, ok := novelRefFrom(c)
	if !ok {
		return
	}

	var req replaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apierror.New(http.StatusBadRequest, apierror.CodeBadRequest,
			"The request body could not be read."))
		return
	}

	result, err := h.service.Replace(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, req.Variables)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}
