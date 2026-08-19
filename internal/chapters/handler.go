package chapters

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RefParam is the path parameter name for a chapter reference. It accepts a
// UUID or a slug, matching the novel reference (docs/08 §35, docs/09 §16).
const RefParam = "chapter"

// ViewCounter is the slice of the views domain this handler needs (Phase 12C).
//
// Consumer-defined and optional: with it nil - Redis disabled, or a test that
// predates the counter - reading simply counts nothing. It must never be able to
// fail a read.
type ViewCounter interface {
	Record(ctx context.Context, novelID uuid.UUID, viewer string)
}

// Handler exposes the chapter endpoints.
type Handler struct {
	service *Service
	views   ViewCounter
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// WithViewCounter attaches read counting. Separate from NewHandler so every
// existing caller keeps working and counting stays visibly optional.
func (h *Handler) WithViewCounter(counter ViewCounter) *Handler {
	h.views = counter
	return h
}

// createRequest - docs/09 §16 "Create Chapter".
//
// `messages` carries the structured chat representation, which docs/09 §16
// requires the API to support rather than forcing chat fiction into prose.
// Positions are absent by design; the server assigns them from array order.
type createRequest struct {
	Title    *string         `json:"title"`
	Content  *string         `json:"content"`
	Messages *[]MessageInput `json:"messages"`

	// The headcanon representation and this chapter's own format (§13J, 12F).
	// An absent or empty presentation_format means "follow the fiction", which
	// is what every chapter of a non-mixed work sends.
	Entries            *[]EntryInput `json:"entries"`
	EntryFields        *[]string     `json:"entry_fields"`
	PresentationFormat *string       `json:"presentation_format"`

	// How the prose renders (§13N). Absent takes the editor's own model, which
	// is safe on a chapter that has nothing in it yet to reinterpret.
	ContentFormat *string `json:"content_format"`

	// ChapterNumber is the number the writer typed (§13R). Absent means "append",
	// which is what the studio sends unless the writer changed it - a fiction
	// that starts at 0, skips to 100, or numbers its side stories separately is
	// the author's arrangement, not a gap the platform gets to close.
	ChapterNumber *int `json:"chapter_number"`

	Status      *string    `json:"status"`
	ScheduledAt *time.Time `json:"scheduled_at"`
}

// updateRequest is a partial update.
//
// json.RawMessage is used for the nullable fields because a PATCH must
// distinguish absent (leave alone) from null (clear). A plain pointer collapses
// the two, which here would mean a request touching only the status could wipe
// a manuscript.
type updateRequest struct {
	Title    json.RawMessage `json:"title"`
	Content  json.RawMessage `json:"content"`
	Messages *[]MessageInput `json:"messages"`

	// presentation_format is a plain pointer rather than RawMessage because its
	// "clear" value is the empty STRING, not null: a chapter that follows the
	// fiction is expressed by sending "" - the value a browser's select sends
	// for its "ตามที่ตั้งไว้" option - so absent and cleared are already
	// distinguishable without the three-case dance.
	Entries            *[]EntryInput `json:"entries"`
	EntryFields        *[]string     `json:"entry_fields"`
	PresentationFormat *string       `json:"presentation_format"`

	// content_format is a plain pointer for the same reason: it has two values
	// and no "clear", so absent already means "leave it alone" (§13N).
	ContentFormat *string `json:"content_format"`

	Status      *string         `json:"status"`
	ScheduledAt json.RawMessage `json:"scheduled_at"`
}

// List handles GET /api/v1/novels/:novel/chapters.
//
// Guests and readers see published chapters; the owner also sees drafts
// (docs/09 §16).
//
// The response is a collection envelope with meta, so it matches every other
// list endpoint. It is deliberately NOT paginated: docs/07 §21 bounds the reader
// at one chapter at a time, and a fiction's table of contents is the navigation
// for that - splitting it would make chapter navigation itself paginated.
func (h *Handler) List(c *gin.Context) {
	novelRef, ok := novelRefFrom(c)
	if !ok {
		return
	}

	summaries, err := h.service.List(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), novelRef)
	if err != nil {
		response.Fail(c, err)
		return
	}

	response.Collection(c, summaries, pagination.Meta{
		Page:    1,
		PerPage: len(summaries),
		Total:   int64(len(summaries)),
	})
}

type reorderRequest struct {
	ChapterIDs []string `json:"chapter_ids"`
}

// Reorder handles PUT /api/v1/novels/:novel/chapters/order. Editor only.
func (h *Handler) Reorder(c *gin.Context) {
	novelRef, ok := novelRefFrom(c)
	if !ok {
		return
	}

	var req reorderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apierror.BadRequest("The request body could not be parsed as JSON."))
		return
	}

	ids := make([]uuid.UUID, 0, len(req.ChapterIDs))
	for _, raw := range req.ChapterIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			response.Fail(c, apierror.Validation(map[string][]string{
				"chapter_ids": {"Must be a list of valid ids."},
			}))
			return
		}
		ids = append(ids, id)
	}

	summaries, err := h.service.Reorder(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), novelRef, ids)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Collection(c, summaries, pagination.Meta{
		Page:    1,
		PerPage: len(summaries),
		Total:   int64(len(summaries)),
	})
}

// Get handles GET /api/v1/novels/:novel/chapters/:chapter.
func (h *Handler) Get(c *gin.Context) {
	novelRef, chapterRef, ok := refsFrom(c)
	if !ok {
		return
	}

	identity := auth.IdentityFrom(c.Request.Context())

	view, err := h.service.Get(c.Request.Context(), identity, novelRef, chapterRef)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// Counting happens HERE rather than in the service: the viewer key needs the
	// request's client address for a guest, which is a transport detail the
	// service has no business knowing. It runs after the read has already
	// succeeded, is fire-and-forget, and an owner previewing their own work is
	// deliberately not counted as a reader of it.
	if h.views != nil && !view.IsOwner {
		h.views.Record(c.Request.Context(), view.NovelID, viewerKey(c, identity))
	}

	response.OK(c, view)
}

// viewerKey identifies "the same person" for one day of de-duplication.
//
// A member is keyed by their user id and a guest by their client address. Both
// go through the recorder's salted one-way hash and expire within the day -
// nothing here is stored, and neither value reaches PostgreSQL (docs/11 §34).
func viewerKey(c *gin.Context, identity *auth.Identity) string {
	if identity.Authenticated() {
		return "u:" + identity.UserID().String()
	}
	return "a:" + c.ClientIP()
}

// Create handles POST /api/v1/novels/:novel/chapters. Owner or staff only.
func (h *Handler) Create(c *gin.Context) {
	novelRef, ok := novelRefFrom(c)
	if !ok {
		return
	}

	var req createRequest
	if !bindJSON(c, &req) {
		return
	}

	view, err := h.service.Create(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), novelRef, CreateInput{
			Title:              req.Title,
			Content:            req.Content,
			Messages:           req.Messages,
			Entries:            req.Entries,
			EntryFields:        req.EntryFields,
			PresentationFormat: req.PresentationFormat,
			ContentFormat:      req.ContentFormat,
			Number:             req.ChapterNumber,
			Status:             req.Status,
			ScheduledAt:        req.ScheduledAt,
		})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// Update handles PATCH /api/v1/novels/:novel/chapters/:chapter.
func (h *Handler) Update(c *gin.Context) {
	novelRef, chapterRef, ok := refsFrom(c)
	if !ok {
		return
	}

	var req updateRequest
	if !bindJSON(c, &req) {
		return
	}

	input := UpdateInput{
		Messages:           req.Messages,
		Entries:            req.Entries,
		EntryFields:        req.EntryFields,
		PresentationFormat: req.PresentationFormat,
		ContentFormat:      req.ContentFormat,
		Status:             req.Status,
	}

	title, err := nullableString(req.Title)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"title": {"Must be a string or null."},
		}))
		return
	}
	input.Title = title

	content, err := nullableString(req.Content)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"content": {"Must be a string or null."},
		}))
		return
	}
	input.Content = content

	scheduled, err := nullableTime(req.ScheduledAt)
	if err != nil {
		response.Fail(c, apierror.Validation(map[string][]string{
			"scheduled_at": {"Must be an RFC 3339 timestamp or null."},
		}))
		return
	}
	input.ScheduledAt = scheduled

	view, updateErr := h.service.Update(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), novelRef, chapterRef, input)
	if updateErr != nil {
		response.Fail(c, updateErr)
		return
	}
	response.OK(c, view)
}

// Publish handles POST /api/v1/novels/:novel/chapters/:chapter/publish.
func (h *Handler) Publish(c *gin.Context) { h.setStatus(c, StatusPublished) }

// Unpublish handles POST /api/v1/novels/:novel/chapters/:chapter/unpublish.
func (h *Handler) Unpublish(c *gin.Context) { h.setStatus(c, StatusUnpublished) }

func (h *Handler) setStatus(c *gin.Context, status Status) {
	novelRef, chapterRef, ok := refsFrom(c)
	if !ok {
		return
	}

	view, err := h.service.SetStatus(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), novelRef, chapterRef, status)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Delete handles DELETE /api/v1/novels/:novel/chapters/:chapter.
func (h *Handler) Delete(c *gin.Context) {
	novelRef, chapterRef, ok := refsFrom(c)
	if !ok {
		return
	}

	if err := h.service.Delete(c.Request.Context(),
		auth.IdentityFrom(c.Request.Context()), novelRef, chapterRef); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
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

func refsFrom(c *gin.Context) (novels.Ref, Ref, bool) {
	novelRef, ok := novelRefFrom(c)
	if !ok {
		return novels.Ref{}, Ref{}, false
	}
	chapterRef, err := ParseRef(c.Param(RefParam))
	if err != nil {
		response.Fail(c, notFound())
		return novels.Ref{}, Ref{}, false
	}
	return novelRef, chapterRef, true
}

// nullableString interprets a raw JSON field as absent / null / value.
func nullableString(raw json.RawMessage) (**string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if string(raw) == "null" {
		var cleared *string
		return &cleared, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	pointer := &value
	return &pointer, nil
}

func nullableTime(raw json.RawMessage) (**time.Time, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if string(raw) == "null" {
		var cleared *time.Time
		return &cleared, nil
	}
	var value time.Time
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	pointer := &value
	return &pointer, nil
}

func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			response.Fail(c, apierror.New(http.StatusRequestEntityTooLarge,
				apierror.CodePayloadTooLarge, "The request body is too large."))
			return false
		}
		response.Fail(c, apierror.BadRequest("The request body could not be parsed as JSON."))
		return false
	}
	return true
}
