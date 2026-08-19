package subscriptions

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// PaymentRefParam names the payment id path parameter on the staff routes.
const PaymentRefParam = "payment"

// Handler exposes the subscription endpoints. It parses and shapes; every
// decision belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func identityOf(c *gin.Context) *auth.Identity {
	return auth.IdentityFrom(c.Request.Context())
}

// Plans handles GET /api/v1/subscription/plans - the public pricing payload
// (plans + mode + demo offer). Available in every mode.
func (h *Handler) Plans(c *gin.Context) {
	pricing, err := h.service.Plans(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, pricing)
}

// Overview handles GET /api/v1/subscription - the caller's tier, entitlements,
// current subscription, latest payment, and the plans on offer.
func (h *Handler) Overview(c *gin.Context) {
	view, err := h.service.Overview(c.Request.Context(), identityOf(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Checkout handles POST /api/v1/subscription/checkout - begin a Premium purchase.
func (h *Handler) Checkout(c *gin.Context) {
	var body struct {
		PlanCode string `json:"plan_code"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, apierror.BadRequest("Malformed request body."))
		return
	}
	view, err := h.service.Checkout(c.Request.Context(), identityOf(c), body.PlanCode)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// ActivateDemo handles POST /api/v1/subscription/demo - start the FREE launch
// demo. Demo mode only; creates no payment (brief §4, §11).
func (h *Handler) ActivateDemo(c *gin.Context) {
	view, err := h.service.ActivateDemo(c.Request.Context(), identityOf(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// Cancel handles POST /api/v1/subscription/cancel.
func (h *Handler) Cancel(c *gin.Context) {
	view, err := h.service.Cancel(c.Request.Context(), identityOf(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// ReviewQueue handles GET /api/v1/admin/subscription/payments - staff only.
func (h *Handler) ReviewQueue(c *gin.Context) {
	views, meta, err := h.service.ReviewQueue(c.Request.Context(), identityOf(c), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// Verify handles POST /api/v1/admin/subscription/payments/:payment/verify.
func (h *Handler) Verify(c *gin.Context) {
	id, err := uuid.Parse(c.Param(PaymentRefParam))
	if err != nil {
		response.Fail(c, paymentNotFound())
		return
	}
	result, err := h.service.Verify(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// Reject handles POST /api/v1/admin/subscription/payments/:payment/reject.
func (h *Handler) Reject(c *gin.Context) {
	id, err := uuid.Parse(c.Param(PaymentRefParam))
	if err != nil {
		response.Fail(c, paymentNotFound())
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	// A missing/empty body is fine - the reason is optional.
	_ = c.ShouldBindJSON(&body)

	view, err := h.service.Reject(c.Request.Context(), identityOf(c), id, body.Reason)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}
