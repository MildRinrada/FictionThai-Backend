package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// Logger emits one structured line per request.
//
// docs/09 - API Specification.md §40 defines the fields; it also forbids
// logging credentials or manuscript content, so this middleware logs metadata
// only and never touches the request or response body.
func Logger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		c.Next()

		status := c.Writer.Status()
		attrs := []slog.Attr{
			slog.String("event", "http_request"),
			slog.String("request_id", RequestIDFrom(c)),
			slog.String("method", c.Request.Method),
			slog.String("path", path),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
			slog.String("client_ip", c.ClientIP()),
		}
		if query != "" {
			// The query string is logged because filters and pagination matter
			// for debugging; endpoints must therefore never accept secrets as
			// query parameters (docs/10 §13).
			attrs = append(attrs, slog.String("query", query))
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, slog.String("errors", c.Errors.String()))
		}

		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		log.LogAttrs(c.Request.Context(), level, "request completed", attrs...)
	}
}
