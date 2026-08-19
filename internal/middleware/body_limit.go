package middleware

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// rawBodyKey stores the pre-limit body so RaiseBodyLimit can re-wrap it.
const rawBodyKey = "middleware.rawBody"

// BodyLimit caps the number of bytes a handler will read from a request body
// (docs/09 - API Specification.md §37 "Request size limits").
//
// It uses http.MaxBytesReader rather than checking Content-Length, because a
// chunked request can lie about or omit its length. Oversized bodies fail when
// the handler reads them, and MaxBytesError is translated into a 413 by
// TranslateBodyLimitError below.
//
// The small global cap is correct for every JSON endpoint. The ONE route that
// legitimately accepts a file - the media upload (docs/08 §22) - composes
// RaiseBodyLimit after this to swap in the configured media cap; a limit
// always applies, only its size changes.
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Set(rawBodyKey, c.Request.Body)
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

// RaiseBodyLimit replaces the global body cap with a larger one for a single
// route. It re-wraps the ORIGINAL body (stashed by BodyLimit) - wrapping the
// already-limited reader would keep the small cap in force.
func RaiseBodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if raw, ok := c.Get(rawBodyKey); ok {
			if body, ok := raw.(io.ReadCloser); ok {
				c.Request.Body = http.MaxBytesReader(c.Writer, body, maxBytes)
			}
		}
		c.Next()
	}
}

// PayloadTooLarge is the error handlers should return when a body exceeds the
// configured limit.
func PayloadTooLarge() *apierror.Error {
	return apierror.New(
		http.StatusRequestEntityTooLarge,
		apierror.CodePayloadTooLarge,
		"The request body is too large.",
	)
}

// FailPayloadTooLarge renders the 413 response.
func FailPayloadTooLarge(c *gin.Context) { response.Fail(c, PayloadTooLarge()) }
