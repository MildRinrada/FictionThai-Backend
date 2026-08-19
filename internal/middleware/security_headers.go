package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/config"
)

// SecurityHeaders sets the response headers appropriate for a JSON API
// (docs/10 - Authentication & Authorization.md §42, docs/11 §44).
//
// Content-Security-Policy is deliberately NOT set here. docs/11 §44 says a CSP
// must be designed around the actual frontend architecture rather than copied
// generically - it belongs on the Next.js responses that actually render HTML.
// This API returns JSON, which a CSP does not protect.
func SecurityHeaders(app config.App) gin.HandlerFunc {
	// HSTS is only meaningful over TLS and is dangerous to assert from a
	// plain-HTTP dev server, so it is production/staging only (docs/11 §45:
	// "Enable HSTS after validating the deployment").
	enableHSTS := !app.IsDevelopment() && app.Env != config.EnvTest

	return func(c *gin.Context) {
		h := c.Writer.Header()

		// Never let a browser MIME-sniff a JSON response into something else.
		h.Set("X-Content-Type-Options", "nosniff")
		// An API response has no legitimate reason to be framed.
		h.Set("X-Frame-Options", "DENY")
		// Do not leak API paths (which contain slugs and IDs) to third parties.
		h.Set("Referrer-Policy", "no-referrer")
		// The API needs none of these browser capabilities.
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		// Authenticated API responses must not be stored by shared caches.
		// Public, cacheable reads opt back in explicitly per endpoint.
		h.Set("Cache-Control", "no-store")

		if enableHSTS {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		c.Next()
	}
}
