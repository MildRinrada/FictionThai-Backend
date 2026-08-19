package shelves

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes the shelf endpoints. It parses and shapes; every decision
// belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// createRequest is the create body. is_public is a plain bool because absent
// means false here - a new shelf is private, and a client that forgets the
// field must get the safe answer.
type createRequest struct {
	Name     string `json:"name"`
	Note     string `json:"note"`
	IsPublic bool   `json:"is_public"`
}

// updateRequest is the PATCH body: every field is a pointer, so an absent one
// is untouched and an empty note clears.
type updateRequest struct {
	Name     *string `json:"name"`
	Note     *string `json:"note"`
	IsPublic *bool   `json:"is_public"`
	Position *int    `json:"position"`
}

// itemRequest carries the reader's own line about one fiction.
type itemRequest struct {
	Note string `json:"note"`
}

func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		response.Fail(c, apierror.BadRequest("The request body is not valid JSON."))
		return false
	}
	return true
}

// shelfIDFrom parses the shelf id. A malformed id is the same 404 a missing
// shelf gets (docs/11 §3.4).
func shelfIDFrom(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(RefParam))
	if err != nil {
		response.Fail(c, notFound())
		return uuid.Nil, false
	}
	return id, true
}

func novelRefFrom(c *gin.Context) (novels.Ref, bool) {
	ref, err := novels.ParseRef(c.Param(novels.RefParam))
	if err != nil {
		response.Fail(c, apierror.New(http.StatusNotFound, "NOVEL_NOT_FOUND", "Fiction not found."))
		return novels.Ref{}, false
	}
	return ref, true
}

// ListPublic handles GET /users/:user/shelves - PUBLIC shelves only.
//
// The collection envelope carries meta for consistency with every other list,
// but the response is not paged: a person has at most MaxShelves shelves, and
// the items inside each one are what is capped.
func (h *Handler) ListPublic(c *gin.Context) {
	views, err := h.service.ListPublic(c.Request.Context(), c.Param(UserRefParam))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, pagination.Params{
		Page: 1, PerPage: MaxShelves,
	}.MetaFor(int64(len(views))))
}

// ListMine handles GET /me/shelves - the owner's own, public and private.
func (h *Handler) ListMine(c *gin.Context) {
	views, err := h.service.Mine(c.Request.Context(), auth.IdentityFrom(c.Request.Context()))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, pagination.Params{
		Page: 1, PerPage: MaxShelves,
	}.MetaFor(int64(len(views))))
}

// Create handles POST /me/shelves.
func (h *Handler) Create(c *gin.Context) {
	var req createRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.Create(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()),
		Input{Name: req.Name, Note: req.Note, IsPublic: req.IsPublic})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// Update handles PATCH /me/shelves/:shelf, including the public/private switch.
func (h *Handler) Update(c *gin.Context) {
	id, ok := shelfIDFrom(c)
	if !ok {
		return
	}
	var req updateRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.Update(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id,
		Edit{Name: req.Name, Note: req.Note, IsPublic: req.IsPublic, Position: req.Position})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Delete handles DELETE /me/shelves/:shelf.
func (h *Handler) Delete(c *gin.Context) {
	id, ok := shelfIDFrom(c)
	if !ok {
		return
	}
	if err := h.service.Delete(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// AddItem handles POST /me/shelves/:shelf/items/:novel.
//
// It answers with the whole shelf rather than the item, because that is what
// the manager re-renders - and because an idempotent repeat then looks exactly
// like the first call.
func (h *Handler) AddItem(c *gin.Context) {
	id, ok := shelfIDFrom(c)
	if !ok {
		return
	}
	ref, ok := novelRefFrom(c)
	if !ok {
		return
	}
	// The note is optional, so a body-less POST is valid: only decode when the
	// client actually sent something.
	var req itemRequest
	if c.Request.ContentLength > 0 {
		if !bindJSON(c, &req) {
			return
		}
	}
	view, err := h.service.AddItem(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id, ref, req.Note)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// RemoveItem handles DELETE /me/shelves/:shelf/items/:novel.
func (h *Handler) RemoveItem(c *gin.Context) {
	id, ok := shelfIDFrom(c)
	if !ok {
		return
	}
	ref, ok := novelRefFrom(c)
	if !ok {
		return
	}
	if err := h.service.RemoveItem(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id, ref); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}
