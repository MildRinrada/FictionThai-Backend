package media

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RefParam names the media id path parameter; KeyParam names the serve-path
// wildcard.
const (
	RefParam = "media"
	KeyParam = "key"
)

// Handler exposes the media endpoints. It parses and shapes; every decision
// belongs to the service (docs/09 §44).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func identityOf(c *gin.Context) *auth.Identity {
	return auth.IdentityFrom(c.Request.Context())
}

// Upload handles POST /api/v1/media - multipart/form-data with fields
//
//	file     the image
//	purpose  avatar | novel_cover | entry_image | chapter_image | character_avatar
//	novel    the fiction id or slug (required for the fiction-scoped purposes)
//
// The route carries the raised body limit; everything about the FILE itself
// is validated in the service from the actual bytes (docs/11 §28).
func (h *Handler) Upload(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		if isBodyTooLarge(err) {
			response.Fail(c, apierror.New(http.StatusRequestEntityTooLarge,
				apierror.CodePayloadTooLarge, "The file is too large."))
			return
		}
		response.Fail(c, apierror.Validation(map[string][]string{
			"file": {"A file is required."},
		}))
		return
	}
	defer file.Close()

	filename := ""
	if header != nil {
		filename = header.Filename
	}

	view, err := h.service.Upload(c.Request.Context(), identityOf(c), UploadInput{
		Purpose:    c.PostForm("purpose"),
		NovelRef:   c.PostForm("novel"),
		PaymentRef: c.PostForm("payment"),
		Filename:   filename,
		File:       file,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, view)
}

// isBodyTooLarge recognises the MaxBytesReader failure surfacing through the
// multipart parser.
func isBodyTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

// Delete handles DELETE /api/v1/media/:media.
func (h *Handler) Delete(c *gin.Context) {
	id, err := uuid.Parse(c.Param(RefParam))
	if err != nil {
		// Malformed ids are the same 404 as missing media (docs/11 §3.4).
		response.Fail(c, notFound())
		return
	}
	if err := h.service.Delete(c.Request.Context(), identityOf(c), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Serve handles GET /media/*key - the public file route (the CDN-origin
// shape of docs/07 §24; in development the API streams the bytes itself).
func (h *Handler) Serve(c *gin.Context) {
	key := strings.TrimPrefix(c.Param(KeyParam), "/")

	record, reader, err := h.service.Open(c.Request.Context(), key)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer reader.Close()

	// Keys are unique per object and never reused, so the response is
	// immutable - a replaced avatar is a NEW key, not new bytes here.
	c.Header("Content-Type", record.MimeType)
	c.Header("Content-Length", strconv.FormatInt(record.SizeBytes, 10))
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		// The response is already streaming; nothing to send but a log.
		_ = c.Error(err)
	}
}

// ServePrivate handles GET /api/v1/media/:media/private - the authenticated,
// owner/staff-only serve route for PRIVATE objects such as payment slips. Unlike
// the public route it sets a PRIVATE, no-store cache policy so financial
// evidence is never cached by a shared proxy or CDN (addendum §14).
func (h *Handler) ServePrivate(c *gin.Context) {
	id, err := uuid.Parse(c.Param(RefParam))
	if err != nil {
		// Malformed ids are the same 404 as missing media (docs/11 §3.4).
		response.Fail(c, notFound())
		return
	}

	record, reader, err := h.service.OpenPrivate(c.Request.Context(), identityOf(c), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer reader.Close()

	c.Header("Content-Type", record.MimeType)
	c.Header("Content-Length", strconv.FormatInt(record.SizeBytes, 10))
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		_ = c.Error(err)
	}
}
