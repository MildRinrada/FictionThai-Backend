package moderation

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RefParam names the report id path parameter.
const RefParam = "report"

// Handler exposes the moderation endpoints. It parses and shapes; every
// decision belongs to the service (docs/09 §44). Staff-ness is enforced twice
// by design: middleware.RequireStaff on the admin routes, and again inside
// the service (docs/10 §27).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		response.Fail(c, apierror.BadRequest("The request body is not valid JSON."))
		return false
	}
	return true
}

// reportIDFrom parses the report id; malformed ids are the same 404 as
// missing reports (docs/11 §3.4).
func reportIDFrom(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(RefParam))
	if err != nil {
		response.Fail(c, reportNotFound())
		return uuid.Nil, false
	}
	return id, true
}

func identityOf(c *gin.Context) *auth.Identity {
	return auth.IdentityFrom(c.Request.Context())
}

// ---------------------------------------------------------------------------
// User side (docs/09 §28)
// ---------------------------------------------------------------------------

// createReportRequest - docs/09 §28 "Create Report".
type createReportRequest struct {
	TargetType  string `json:"target_type"`
	TargetID    string `json:"target_id"`
	Reason      string `json:"reason"`
	Description string `json:"description"`
}

// CreateReport handles POST /reports. A fresh report answers 201; filing
// again while an earlier report on the same target is still open answers 200
// with that existing report (the idempotent-duplicate shape of docs/09 §34).
func (h *Handler) CreateReport(c *gin.Context) {
	var req createReportRequest
	if !bindJSON(c, &req) {
		return
	}
	view, created, err := h.service.CreateReport(c.Request.Context(), identityOf(c), ReportInput{
		TargetType:  req.TargetType,
		TargetID:    req.TargetID,
		Reason:      req.Reason,
		Description: req.Description,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	if created {
		response.Created(c, view)
		return
	}
	response.OK(c, view)
}

// MyReports handles GET /me/reports (docs/09 §28).
func (h *Handler) MyReports(c *gin.Context) {
	views, meta, err := h.service.MyReports(c.Request.Context(), identityOf(c), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// ---------------------------------------------------------------------------
// Staff side (docs/09 §29)
// ---------------------------------------------------------------------------

// Queue handles GET /admin/reports.
func (h *Handler) Queue(c *gin.Context) {
	views, meta, err := h.service.Queue(c.Request.Context(), identityOf(c), QueueQuery{
		Status:     c.Query("status"),
		TargetType: c.Query("target_type"),
	}, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// GetReport handles GET /admin/reports/:report.
func (h *Handler) GetReport(c *gin.Context) {
	id, ok := reportIDFrom(c)
	if !ok {
		return
	}
	detail, err := h.service.GetReport(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, detail)
}

// updateReportRequest moves a report along the lifecycle.
type updateReportRequest struct {
	Status string `json:"status"`
}

// UpdateReport handles PATCH /admin/reports/:report.
func (h *Handler) UpdateReport(c *gin.Context) {
	id, ok := reportIDFrom(c)
	if !ok {
		return
	}
	var req updateReportRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.UpdateReport(c.Request.Context(), identityOf(c), id, req.Status)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// actionRequest - one moderation action (docs/08 §24.2, docs/02 §46).
type actionRequest struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Action     string `json:"action"`
	Reason     string `json:"reason"`
}

// PerformAction handles POST /admin/moderation/actions.
func (h *Handler) PerformAction(c *gin.Context) {
	var req actionRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.PerformAction(c.Request.Context(), identityOf(c), ActionInput{
		TargetType: req.TargetType,
		TargetID:   req.TargetID,
		Action:     req.Action,
		Reason:     req.Reason,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// ListActions handles GET /admin/moderation/actions.
func (h *Handler) ListActions(c *gin.Context) {
	views, meta, err := h.service.Actions(c.Request.Context(), identityOf(c), ActionsQuery{
		TargetType: c.Query("target_type"),
		TargetID:   c.Query("target_id"),
	}, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}
