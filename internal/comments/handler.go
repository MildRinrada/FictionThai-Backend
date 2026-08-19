package comments

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RefParam is the path parameter name for a comment id. Unlike novels and
// chapters, comments have no slugs - the id is the only reference
// (docs/09 §20 ":comment_id").
const RefParam = "comment"

// Handler exposes the comment endpoints. It parses and shapes; every decision
// belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// commentRequest carries the writable fields (docs/09 §20, §13D).
//
// GuestName is read only when the caller has no session: a signed-in reader
// posting a "name" is ignored rather than honoured, so nobody can wear one
// while logged in as someone else.
type commentRequest struct {
	Content   string `json:"content"`
	GuestName string `json:"guest_name"`
}

// bindJSON decodes a request body, reporting a clean error on malformed input.
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		response.Fail(c, apierror.BadRequest("The request body is not valid JSON."))
		return false
	}
	return true
}

// novelRefFrom parses the fiction reference, writing a 404 on failure.
func novelRefFrom(c *gin.Context) (novels.Ref, bool) {
	ref, err := novels.ParseRef(c.Param(novels.RefParam))
	if err != nil {
		response.Fail(c, apierror.New(http.StatusNotFound, "NOVEL_NOT_FOUND", "Fiction not found."))
		return novels.Ref{}, false
	}
	return ref, true
}

// chapterRefFrom parses the chapter reference, writing a 404 on failure.
func chapterRefFrom(c *gin.Context) (chapters.Ref, bool) {
	ref, err := chapters.ParseRef(c.Param(chapters.RefParam))
	if err != nil {
		response.Fail(c, apierror.New(http.StatusNotFound, "CHAPTER_NOT_FOUND", "Chapter not found."))
		return chapters.Ref{}, false
	}
	return ref, true
}

// commentIDFrom parses the comment id. A malformed id is the same 404 as a
// missing comment (docs/11 §3.4).
func commentIDFrom(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(RefParam))
	if err != nil {
		response.Fail(c, notFound())
		return uuid.Nil, false
	}
	return id, true
}

// Like handles POST /comments/:comment/like (comment design review 2026-08).
func (h *Handler) Like(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	view, err := h.service.Like(c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Unlike handles DELETE /comments/:comment/like.
func (h *Handler) Unlike(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	view, err := h.service.Unlike(c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// ListForNovel handles GET /novels/:novel/comments (docs/09 §20).
func (h *Handler) ListForNovel(c *gin.Context) {
	ref, ok := novelRefFrom(c)
	if !ok {
		return
	}
	views, meta, err := h.service.ListForNovel(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), ref, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// CreateForNovel handles POST /novels/:novel/comments.
func (h *Handler) CreateForNovel(c *gin.Context) {
	ref, ok := novelRefFrom(c)
	if !ok {
		return
	}
	var req commentRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.CreateForNovel(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()),
		ref, req.Content, req.GuestName)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// ListForChapter handles GET /novels/:novel/chapters/:chapter/comments.
func (h *Handler) ListForChapter(c *gin.Context) {
	novelRef, ok := novelRefFrom(c)
	if !ok {
		return
	}
	chapterRef, ok := chapterRefFrom(c)
	if !ok {
		return
	}
	views, meta, err := h.service.ListForChapter(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), novelRef, chapterRef, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// CreateForChapter handles POST /novels/:novel/chapters/:chapter/comments.
func (h *Handler) CreateForChapter(c *gin.Context) {
	novelRef, ok := novelRefFrom(c)
	if !ok {
		return
	}
	chapterRef, ok := chapterRefFrom(c)
	if !ok {
		return
	}
	var req commentRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.CreateForChapter(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()),
		novelRef, chapterRef, req.Content, req.GuestName)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// ListReplies handles GET /comments/:comment/replies.
func (h *Handler) ListReplies(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	views, meta, err := h.service.ListReplies(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// Reply handles POST /comments/:comment/replies (docs/09 §20).
func (h *Handler) Reply(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	var req commentRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.Reply(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()),
		id, req.Content, req.GuestName)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// Update handles PATCH /comments/:comment (docs/09 §20).
func (h *Handler) Update(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	var req commentRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.Update(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id, req.Content)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Delete handles DELETE /comments/:comment (docs/09 §20).
func (h *Handler) Delete(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// ListPending handles GET /novels/:novel/comments/pending (§13D).
//
// Owner-only. It is the author's review queue, so it lives under the fiction
// rather than under /me: what is being reviewed belongs to one work.
func (h *Handler) ListPending(c *gin.Context) {
	ref, ok := novelRefFrom(c)
	if !ok {
		return
	}
	views, meta, err := h.service.Pending(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), ref, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// Approve handles POST /comments/:comment/approve (§13D).
func (h *Handler) Approve(c *gin.Context) { h.decide(c, true) }

// Reject handles POST /comments/:comment/reject (§13D).
func (h *Handler) Reject(c *gin.Context) { h.decide(c, false) }

func (h *Handler) decide(c *gin.Context, approve bool) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	view, err := h.service.Decide(
		c.Request.Context(), auth.IdentityFrom(c.Request.Context()), id, approve)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}
