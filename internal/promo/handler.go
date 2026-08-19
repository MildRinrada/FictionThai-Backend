package promo

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// SlideRefParam is the route parameter naming one slide.
const SlideRefParam = "slide"

// Handler shapes the promo endpoints. Every decision is the service's.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func identityOf(c *gin.Context) *auth.Identity {
	return auth.IdentityFrom(c.Request.Context())
}

func (h *Handler) slideID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(SlideRefParam))
	if err != nil {
		response.Fail(c, notFound())
		return uuid.Nil, false
	}
	return id, true
}

// Active handles GET /api/v1/promo/slides - the public queue.
func (h *Handler) Active(c *gin.Context) {
	views, err := h.service.Active(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"slides": views})
}

// Click handles POST /api/v1/promo/slides/:slide/click - the public counter.
func (h *Handler) Click(c *gin.Context) {
	id, ok := h.slideID(c)
	if !ok {
		return
	}
	if err := h.service.Click(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Queue handles GET /api/v1/admin/promo/slides.
func (h *Handler) Queue(c *gin.Context) {
	views, err := h.service.Queue(c.Request.Context(), identityOf(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"slides": views})
}

// Create handles POST /api/v1/admin/promo/slides.
func (h *Handler) Create(c *gin.Context) {
	var body Input
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.service.Create(c.Request.Context(), identityOf(c), body)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// Update handles PATCH /api/v1/admin/promo/slides/:slide.
func (h *Handler) Update(c *gin.Context) {
	id, ok := h.slideID(c)
	if !ok {
		return
	}
	var body Input
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.service.Update(c.Request.Context(), identityOf(c), id, body)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Delete handles DELETE /api/v1/admin/promo/slides/:slide.
func (h *Handler) Delete(c *gin.Context) {
	id, ok := h.slideID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), identityOf(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Reorder handles PUT /api/v1/admin/promo/slides/order.
func (h *Handler) Reorder(c *gin.Context) {
	var body struct {
		IDs []uuid.UUID `json:"ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	views, err := h.service.Reorder(c.Request.Context(), identityOf(c), body.IDs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"slides": views})
}

func badBody() *apierror.Error {
	return apierror.Validation(map[string][]string{
		"body": {"A valid JSON body is required."},
	})
}
