package middleware

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Recovery converts a panic into a generic 500 in the standard error envelope.
//
// The stack trace goes to the logs only; docs/09 §39 and docs/11 §67 forbid
// exposing stack traces, SQL errors, or filesystem paths to clients.
func Recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			// A client that hung up mid-write is not a server fault and there is
			// no connection left to write a response to.
			if isBrokenPipe(rec) {
				log.WarnContext(c.Request.Context(), "client connection closed",
					slog.String("request_id", RequestIDFrom(c)),
					slog.String("path", c.Request.URL.Path),
				)
				c.Abort()
				return
			}

			log.ErrorContext(c.Request.Context(), "panic recovered",
				slog.String("event", "panic"),
				slog.String("request_id", RequestIDFrom(c)),
				slog.String("method", c.Request.Method),
				slog.String("path", c.Request.URL.Path),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
			)

			if c.Writer.Written() {
				// Headers are already on the wire; the best we can do is stop.
				c.Abort()
				return
			}
			response.Fail(c, apierror.Internal())
		}()

		c.Next()
	}
}

// MethodNotAllowed and NoRoute handlers keep 404/405 responses in the same
// envelope as every other error, so clients need only one parser.
func NoRoute() gin.HandlerFunc {
	return func(c *gin.Context) {
		response.Fail(c, apierror.NotFound("The requested endpoint does not exist."))
	}
}

func NoMethod() gin.HandlerFunc {
	return func(c *gin.Context) {
		response.Fail(c, apierror.New(
			http.StatusMethodNotAllowed,
			"METHOD_NOT_ALLOWED",
			"This method is not allowed for the requested endpoint.",
		))
	}
}

func isBrokenPipe(rec any) bool {
	err, ok := rec.(error)
	if !ok {
		return false
	}
	var netErr *net.OpError
	if !errors.As(err, &netErr) {
		return false
	}
	var sysErr *os.SyscallError
	if !errors.As(netErr, &sysErr) {
		return false
	}
	msg := strings.ToLower(sysErr.Error())
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset by peer")
}
