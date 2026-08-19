package ai

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Path parameter names for the AI routes.
const (
	RequestRefParam    = "request"
	SuggestionRefParam = "suggestion"
)

// Handler exposes the AI endpoints. It parses and shapes; every decision belongs
// to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func identityOf(c *gin.Context) *auth.Identity {
	return auth.IdentityFrom(c.Request.Context())
}

func badBody() *apierror.Error { return apierror.BadRequest("Malformed request body.") }

// SpellCheck handles POST /api/v1/ai/spell-check - the stateless, synchronous
// analysis of raw text (docs/09 §24). Nothing is persisted.
func (h *Handler) SpellCheck(c *gin.Context) {
	var body struct {
		Text string `json:"text"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	suggestions, err := h.service.Analyze(c.Request.Context(), identityOf(c), body.Text)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"suggestions": suggestions})
}

// CreateRequest handles POST /api/v1/ai/requests - a persisted request against a
// chapter the caller owns. Queued (async) work answers 202; completed or failed
// synchronous work answers 201.
func (h *Handler) CreateRequest(c *gin.Context) {
	var body struct {
		Feature   string `json:"feature"`
		ChapterID string `json:"chapter_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.service.CreateRequest(c.Request.Context(), identityOf(c), CreateRequestInput{
		Feature:   body.Feature,
		ChapterID: body.ChapterID,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	if view.Status == StatusQueued {
		response.Data(c, http.StatusAccepted, view)
		return
	}
	response.Created(c, view)
}

// ListRequests handles GET /api/v1/ai/requests - the caller's request history.
func (h *Handler) ListRequests(c *gin.Context) {
	views, meta, err := h.service.ListRequests(c.Request.Context(), identityOf(c), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// Usage handles GET /api/v1/ai/usage - the caller's daily-quota standing.
// A read that spends nothing: the limiter is peeked, never hit.
func (h *Handler) Usage(c *gin.Context) {
	view, err := h.service.Usage(c.Request.Context(), identityOf(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// GetRequest handles GET /api/v1/ai/requests/:request - poll one request.
func (h *Handler) GetRequest(c *gin.Context) {
	id, err := uuid.Parse(c.Param(RequestRefParam))
	if err != nil {
		response.Fail(c, requestNotFound())
		return
	}
	view, err := h.service.GetRequest(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Retry handles POST /api/v1/ai/requests/:request/retry.
func (h *Handler) Retry(c *gin.Context) {
	id, err := uuid.Parse(c.Param(RequestRefParam))
	if err != nil {
		response.Fail(c, requestNotFound())
		return
	}
	view, err := h.service.RetryRequest(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Data(c, http.StatusAccepted, view)
}

// Cancel handles POST /api/v1/ai/requests/:request/cancel.
func (h *Handler) Cancel(c *gin.Context) {
	id, err := uuid.Parse(c.Param(RequestRefParam))
	if err != nil {
		response.Fail(c, requestNotFound())
		return
	}
	view, err := h.service.CancelRequest(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// DecideSuggestion handles POST /api/v1/ai/suggestions/:suggestion/decision -
// the writer accepts, rejects, or dismisses a suggestion (docs/12 §14). It never
// edits the manuscript (docs/12 §15).
func (h *Handler) DecideSuggestion(c *gin.Context) {
	id, err := uuid.Parse(c.Param(SuggestionRefParam))
	if err != nil {
		response.Fail(c, suggestionNotFound())
		return
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badBody())
		return
	}
	view, err := h.service.DecideSuggestion(c.Request.Context(), identityOf(c), id, body.Decision)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}
