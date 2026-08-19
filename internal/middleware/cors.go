package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/config"
)

// corsMaxAge caps how long a browser may cache a preflight result.
const corsMaxAge = 10 * 60 // seconds

var (
	corsAllowedMethods = strings.Join([]string{
		http.MethodGet, http.MethodPost, http.MethodPatch,
		http.MethodPut, http.MethodDelete, http.MethodOptions,
	}, ", ")

	corsAllowedHeaders = strings.Join([]string{
		"Accept", "Authorization", "Content-Type", HeaderRequestID, "X-CSRF-Token",
	}, ", ")

	corsExposedHeaders = strings.Join([]string{HeaderRequestID}, ", ")
)

// CORS enforces an explicit origin allowlist.
//
// docs/11 - Security & Privacy.md §23 and docs/10 §41 forbid
// `Access-Control-Allow-Origin: *` for an API carrying private user data, so
// this middleware only ever echoes an origin it recognises. A request from an
// unknown origin still reaches the handler - public reads are meant to work for
// any client - but it receives no CORS headers, so a browser will not expose
// the response cross-origin.
func CORS(cfg config.CORS) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, origin := range cfg.AllowedOrigins {
		allowed[strings.TrimRight(origin, "/")] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if _, ok := allowed[strings.TrimRight(origin, "/")]; ok {
				h := c.Writer.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Methods", corsAllowedMethods)
				h.Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				h.Set("Access-Control-Expose-Headers", corsExposedHeaders)
				h.Set("Access-Control-Max-Age", strconv.Itoa(corsMaxAge))
				// Caches must key on Origin, or one origin's response could be
				// served to another.
				h.Add("Vary", "Origin")
			}
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
