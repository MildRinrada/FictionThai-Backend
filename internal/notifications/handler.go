package notifications

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RefParam is the path parameter name for a notification id.
const RefParam = "notification"

// Handler exposes the notification endpoints (docs/09 §23). Everything here
// is the caller's own data; there is no public surface at all.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// List handles GET /me/notifications.
func (h *Handler) List(c *gin.Context) {
	views, meta, err := h.service.List(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// UnreadCount handles GET /me/notifications/unread-count - the badge read.
func (h *Handler) UnreadCount(c *gin.Context) {
	count, err := h.service.UnreadCount(c.Request.Context(), auth.IdentityFrom(c.Request.Context()))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"unread_count": count})
}

// MarkRead handles POST /notifications/:notification/read. A malformed id is
// the same 404 as a missing one.
func (h *Handler) MarkRead(c *gin.Context) {
	id, err := uuid.Parse(c.Param(RefParam))
	if err != nil {
		response.Fail(c, notFound())
		return
	}
	if err := h.service.MarkRead(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// MarkAllRead handles POST /me/notifications/read-all.
func (h *Handler) MarkAllRead(c *gin.Context) {
	if err := h.service.MarkAllRead(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context())); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}
