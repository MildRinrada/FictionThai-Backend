package middleware

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/pkg/logger"
)

// HeaderRequestID is the correlation header exchanged with clients and the
// reverse proxy (docs/07 - System Architecture.md §49).
const HeaderRequestID = "X-Request-ID"

// ContextRequestID is the gin context key holding the current request ID.
const ContextRequestID = "request_id"

// maxInboundRequestIDLen bounds a client-supplied ID so it cannot be used to
// bloat every log line for a request.
const maxInboundRequestIDLen = 64

// RequestID attaches a correlation ID to every request, reusing the inbound
// header when the proxy or frontend already set one.
//
// The ID is placed on the gin context AND on the request context, so services
// and repositories can log with it without depending on gin.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if !validRequestID(id) {
			id = newRequestID()
		}

		c.Set(ContextRequestID, id)
		c.Request = c.Request.WithContext(logger.WithRequestID(c.Request.Context(), id))
		c.Header(HeaderRequestID, id)

		c.Next()
	}
}

// RequestIDFrom returns the request ID for the current request.
func RequestIDFrom(c *gin.Context) string {
	id, _ := c.Get(ContextRequestID)
	s, _ := id.(string)
	return s
}

// validRequestID accepts only short, printable-ASCII IDs. Anything else is
// replaced, which keeps header-injected control characters out of the logs.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxInboundRequestIDLen {
		return false
	}
	for _, r := range id {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !isAllowed {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; if it ever did, an empty ID
		// would be worse than a constant marker for debugging.
		return "req-unavailable"
	}
	return "req_" + hex.EncodeToString(b[:])
}
