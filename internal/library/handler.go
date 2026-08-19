package library

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

// UserRefParam is the path parameter name for a follow target.
const UserRefParam = "user"

// Handler exposes the shelf endpoints. It parses and shapes; every decision
// belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Bookmark handles POST /api/v1/novels/:novel/bookmark (docs/09 §18).
func (h *Handler) Bookmark(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	if err := h.service.Bookmark(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Unbookmark handles DELETE /api/v1/novels/:novel/bookmark (docs/09 §18).
func (h *Handler) Unbookmark(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	if err := h.service.Unbookmark(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// BookmarkStatus handles GET /api/v1/novels/:novel/bookmark.
func (h *Handler) BookmarkStatus(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	saved, err := h.service.BookmarkStatus(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"is_bookmarked": saved})
}

// Like handles POST /api/v1/novels/:novel/reaction
// (docs/01 §20.2, docs/PHASE-12-STORY-DEPTH.md §12C).
func (h *Handler) Like(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	if err := h.service.Like(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Unlike handles DELETE /api/v1/novels/:novel/reaction.
func (h *Handler) Unlike(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	if err := h.service.Unlike(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// LikeStatus handles GET /api/v1/novels/:novel/reaction.
func (h *Handler) LikeStatus(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	liked, err := h.service.LikeStatus(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"is_liked": liked})
}

// Library handles GET /api/v1/me/library (docs/09 §18).
func (h *Handler) Library(c *gin.Context) {
	entries, meta, err := h.service.Library(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), c.Query("status"), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, entries, meta)
}

// Follow handles POST /api/v1/users/:user/follow (docs/09 §19).
func (h *Handler) Follow(c *gin.Context) {
	target, ok := userRef(c)
	if !ok {
		return
	}
	if err := h.service.Follow(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), target); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Unfollow handles DELETE /api/v1/users/:user/follow (docs/09 §19).
func (h *Handler) Unfollow(c *gin.Context) {
	target, ok := userRef(c)
	if !ok {
		return
	}
	if err := h.service.Unfollow(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), target); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// FollowStatus handles GET /api/v1/users/:user/follow-status (docs/09 §19).
func (h *Handler) FollowStatus(c *gin.Context) {
	target, ok := userRef(c)
	if !ok {
		return
	}
	status, err := h.service.FollowStatusFor(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), target)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, status)
}

// Following handles GET /api/v1/me/following (docs/03 §13).
func (h *Handler) Following(c *gin.Context) {
	entries, meta, err := h.service.Following(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, entries, meta)
}

// progressRequest - docs/09 §17 "Update Reading Progress".
//
// The wire field is progress_percent, matching what docs/08 §18 stores. The
// float pointer distinguishes "absent" from an explicit 0 so a missing field
// fails validation instead of silently meaning "start of chapter".
type progressRequest struct {
	ChapterID       string   `json:"chapter_id"`
	ProgressPercent *float64 `json:"progress_percent"`
}

// SaveProgress handles PUT /api/v1/novels/:novel/progress (docs/09 §17).
func (h *Handler) SaveProgress(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}

	var req progressRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apierror.BadRequest("The request body could not be parsed as JSON."))
		return
	}

	input := ProgressInput{}
	errs := map[string][]string{}
	if req.ChapterID == "" {
		errs["chapter_id"] = []string{"A chapter is required."}
	} else {
		id, err := uuid.Parse(req.ChapterID)
		if err != nil {
			errs["chapter_id"] = []string{"Must be a valid chapter id."}
		} else {
			input.ChapterID = id
		}
	}
	if req.ProgressPercent == nil {
		errs["progress_percent"] = []string{"A progress percentage is required."}
	} else {
		input.ProgressPercent = *req.ProgressPercent
	}
	if len(errs) > 0 {
		response.Fail(c, apierror.Validation(errs))
		return
	}

	progress, err := h.service.SaveProgress(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref, input)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, progress)
}

// GetProgress handles GET /api/v1/novels/:novel/progress (docs/09 §17).
func (h *Handler) GetProgress(c *gin.Context) {
	ref, ok := novelRef(c)
	if !ok {
		return
	}
	progress, err := h.service.GetProgress(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), ref)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, progress)
}

// ContinueReading handles GET /api/v1/me/reading-progress (docs/09 §17).
func (h *Handler) ContinueReading(c *gin.Context) {
	entries, meta, err := h.service.ContinueReading(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, entries, meta)
}

// novelRef parses the fiction path parameter, reusing the novels domain's
// parser so slug-or-id behaves identically across the whole /novels subtree.
func novelRef(c *gin.Context) (novels.Ref, bool) {
	ref, err := novels.ParseRef(c.Param(novels.RefParam))
	if err != nil {
		response.Fail(c, apierror.New(http.StatusNotFound, "NOVEL_NOT_FOUND", "Fiction not found."))
		return novels.Ref{}, false
	}
	return ref, true
}

// userRef parses the follow-target path parameter. Follow targets are ids only
// (docs/09 §19 /users/:user_id/...); a malformed id is the same 404 an unknown
// user gets, so the parameter is not an oracle.
func userRef(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(UserRefParam))
	if err != nil {
		response.Fail(c, userNotFound())
		return uuid.Nil, false
	}
	return id, true
}
