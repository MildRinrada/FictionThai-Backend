package community

import (
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Path parameter names. Posts and community comments are id-only resources
// (docs/09 §21 ":id") - no slugs.
const (
	PostRefParam    = "post"
	CommentRefParam = "comment"
)

// Handler exposes the community endpoints. It parses and shapes; every
// decision belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// contentRequest carries the single writable text field.
type contentRequest struct {
	Content string `json:"content"`
}

func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		response.Fail(c, apierror.BadRequest("The request body is not valid JSON."))
		return false
	}
	return true
}

// postIDFrom parses the post id; malformed ids are the same 404 as missing
// posts (docs/11 §3.4).
func postIDFrom(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(PostRefParam))
	if err != nil {
		response.Fail(c, postNotFound())
		return uuid.Nil, false
	}
	return id, true
}

func commentIDFrom(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(CommentRefParam))
	if err != nil {
		response.Fail(c, commentNotFound())
		return uuid.Nil, false
	}
	return id, true
}

func identityOf(c *gin.Context) *auth.Identity {
	return auth.IdentityFrom(c.Request.Context())
}

// ---------------------------------------------------------------------------
// Posts
// ---------------------------------------------------------------------------

// ListPosts handles GET /community/posts (docs/09 §21).
func (h *Handler) ListPosts(c *gin.Context) {
	views, meta, err := h.service.ListPosts(c.Request.Context(), identityOf(c), ListQuery{
		Author: c.Query("author"),
		Feed:   c.Query("feed"),
		Novel:  c.Query("novel"),
		Type:   c.Query("type"),
		Sort:   c.Query("sort"),
	}, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// SearchPosts handles GET /search/posts (docs/COMMUNITY-FEED.md). It lives
// beside the other /search/* endpoints on the Search rate tier; the operator
// syntax is the CLIENT's affordance - by the time a request reaches here it
// is plain structured parameters.
func (h *Handler) SearchPosts(c *gin.Context) {
	views, meta, err := h.service.SearchPosts(c.Request.Context(), identityOf(c), SearchQuery{
		Q:       c.Query("q"),
		From:    c.Query("from"),
		Range:   c.Query("range"),
		Has:     c.Query("has"),
		Sort:    c.Query("sort"),
		Type:    c.Query("type"),
		Author:  c.Query("author"),
		Mention: c.Query("mention"),
		Fandom:  c.Query("fandom"),
		Tag:     c.Query("tag"),
	}, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// ListTrendingTags handles GET /community/tags - the "แท็กที่กำลังพูดถึง"
// panel and the # autocomplete. ?q= optionally narrows by prefix.
func (h *Handler) ListTrendingTags(c *gin.Context) {
	items, err := h.service.ListTrendingTags(c.Request.Context(), c.Query("q"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, items)
}

// GetPost handles GET /community/posts/:post.
func (h *Handler) GetPost(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	view, err := h.service.GetPost(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// referenceRequest is the attachment a post carries
// (docs/PHASE-12-STORY-DEPTH.md §12D). Both fields accept a UUID or a slug,
// like every :ref path parameter; the names match the stored columns so the
// object a client reads back can be sent straight back in.
type referenceRequest struct {
	NovelID   string `json:"novel_id"`
	ChapterID string `json:"chapter_id"`
}

func (r *referenceRequest) input() ReferenceInput {
	if r == nil {
		return ReferenceInput{}
	}
	return ReferenceInput{NovelRef: r.NovelID, ChapterRef: r.ChapterID}
}

// createPostRequest - docs/09 §21 "Create Post".
type createPostRequest struct {
	Content    string            `json:"content"`
	Visibility string            `json:"visibility"`
	PostType   string            `json:"post_type"`
	Reference  *referenceRequest `json:"reference"`
}

// CreatePost handles POST /community/posts.
func (h *Handler) CreatePost(c *gin.Context) {
	var req createPostRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.CreatePost(c.Request.Context(), identityOf(c), CreatePostInput{
		Content:    req.Content,
		Visibility: req.Visibility,
		Type:       req.PostType,
		Reference:  req.Reference.input(),
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// updatePostRequest is a partial edit; absent fields stay untouched.
//
// `reference` is the three-case field (docs/09 §3), which is why it is raw
// JSON: absent leaves the attachment alone, `null` detaches it, and an object
// replaces it. Distinguishing the first two matters - an edit that only
// changes the text must not silently drop the fiction card.
type updatePostRequest struct {
	Content    *string         `json:"content"`
	Visibility *string         `json:"visibility"`
	PostType   *string         `json:"post_type"`
	Reference  json.RawMessage `json:"reference"`
}

// UpdatePost handles PATCH /community/posts/:post.
func (h *Handler) UpdatePost(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	var req updatePostRequest
	if !bindJSON(c, &req) {
		return
	}

	input := UpdatePostInput{Content: req.Content, Visibility: req.Visibility, Type: req.PostType}
	if len(req.Reference) > 0 {
		if string(req.Reference) == "null" {
			// Detach: an explicit, empty reference.
			input.Reference = &ReferenceInput{}
		} else {
			var reference referenceRequest
			if err := json.Unmarshal(req.Reference, &reference); err != nil {
				response.Fail(c, apierror.BadRequest("The request body is not valid JSON."))
				return
			}
			parsed := reference.input()
			input.Reference = &parsed
		}
	}

	view, err := h.service.UpdatePost(c.Request.Context(), identityOf(c), id, input)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// ListDiscussedFictions handles GET /community/discussed - the sidebar (§12D).
func (h *Handler) ListDiscussedFictions(c *gin.Context) {
	items, err := h.service.ListDiscussedFictions(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, items)
}

// DeletePost handles DELETE /community/posts/:post.
func (h *Handler) DeletePost(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	if err := h.service.DeletePost(c.Request.Context(), identityOf(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

// ListComments handles GET /community/posts/:post/comments.
func (h *Handler) ListComments(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	views, meta, err := h.service.ListComments(
		c.Request.Context(), identityOf(c), id, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// CreateComment handles POST /community/posts/:post/comments.
func (h *Handler) CreateComment(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	var req contentRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.CreateComment(c.Request.Context(), identityOf(c), id, req.Content)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// ListReplies handles GET /community/comments/:comment/replies.
func (h *Handler) ListReplies(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	views, meta, err := h.service.ListReplies(
		c.Request.Context(), identityOf(c), id, pagination.Parse(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, views, meta)
}

// Reply handles POST /community/comments/:comment/replies.
func (h *Handler) Reply(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	var req contentRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.Reply(c.Request.Context(), identityOf(c), id, req.Content)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// UpdateComment handles PATCH /community/comments/:comment.
func (h *Handler) UpdateComment(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	var req contentRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.UpdateComment(c.Request.Context(), identityOf(c), id, req.Content)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// DeleteComment handles DELETE /community/comments/:comment.
func (h *Handler) DeleteComment(c *gin.Context) {
	id, ok := commentIDFrom(c)
	if !ok {
		return
	}
	if err := h.service.DeleteComment(c.Request.Context(), identityOf(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// ---------------------------------------------------------------------------
// Bookmarks (docs/COMMUNITY-FEED.md)
// ---------------------------------------------------------------------------

// Bookmark handles POST /community/posts/:post/bookmark.
func (h *Handler) Bookmark(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	view, err := h.service.BookmarkPost(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// RemoveBookmark handles DELETE /community/posts/:post/bookmark.
func (h *Handler) RemoveBookmark(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	view, err := h.service.UnbookmarkPost(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// ---------------------------------------------------------------------------
// Reactions
// ---------------------------------------------------------------------------

// reactionRequest - docs/09 §21 "React to Post".
type reactionRequest struct {
	Type string `json:"type"`
}

// React handles POST /community/posts/:post/reactions.
func (h *Handler) React(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	var req reactionRequest
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.service.React(c.Request.Context(), identityOf(c), id, req.Type)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// RemoveReaction handles DELETE /community/posts/:post/reactions.
func (h *Handler) RemoveReaction(c *gin.Context) {
	id, ok := postIDFrom(c)
	if !ok {
		return
	}
	view, err := h.service.Unreact(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}
