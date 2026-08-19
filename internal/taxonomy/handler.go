package taxonomy

import (
	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes the vocabulary endpoints.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Genres handles GET /api/v1/genres. Public: the whole controlled vocabulary,
// for pickers and filter chips (docs/03 §9).
func (h *Handler) Genres(c *gin.Context) {
	genres, err := h.service.Genres(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, genres)
}

// Tags handles GET /api/v1/tags. Public browse and typeahead, most-used first
// (docs/01 §6).
func (h *Handler) Tags(c *gin.Context) {
	tags, meta, err := h.service.Tags(c.Request.Context(), c.Query("q"), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, tags, meta)
}

type createTagRequest struct {
	Name string `json:"name"`
}

// CreateTag handles POST /api/v1/tags. Authenticated, idempotent get-or-create
// - the path a writer's free-form tag enters the vocabulary through.
func (h *Handler) CreateTag(c *gin.Context) {
	var req createTagRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apierror.BadRequest("The request body could not be parsed as JSON."))
		return
	}

	tag, err := h.service.CreateTag(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), req.Name)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, tag)
}
