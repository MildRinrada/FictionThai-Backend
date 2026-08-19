package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// CSRF protects cookie-authenticated state-changing requests.
//
// The threat is specific: a cookie is an AMBIENT credential, so a page on
// attacker.example can cause the browser to issue an authenticated request to
// our origin. A Bearer token cannot be sent that way - the attacker's page has
// no access to it - so Bearer requests are exempt, and forcing a CSRF token on
// mobile would be friction with no security benefit (docs/10 §40, docs/11 §22).
//
// Three layers, cheapest first:
//
//  1. Only cookie-authenticated mutations are checked at all. Guests and Bearer
//     callers pass straight through.
//  2. The Origin/Referer header must match an allowed origin. This alone stops
//     classic cross-site form posts, since a browser sets Origin on every
//     cross-origin request and script cannot forge it.
//  3. Double-submit: the X-CSRF-Token header must equal the CSRF cookie. A
//     cross-site attacker can cause the cookie to be SENT but cannot READ it,
//     so they cannot produce a matching header.
//
// SameSite=Lax on the session cookie is a fourth layer, applied by the browser.
func CSRF(cookies auth.CookieConfig, allowedOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[normalizeOrigin(origin)] = struct{}{}
	}

	return func(c *gin.Context) {
		if isSafeMethod(c.Request.Method) {
			// GET/HEAD/OPTIONS must not change state (docs/10 §40), so there is
			// nothing to protect.
			c.Next()
			return
		}

		identity := auth.IdentityFrom(c.Request.Context())
		if !identity.UsedCookieTransport() {
			// Guest, or a Bearer-authenticated native client.
			c.Next()
			return
		}

		if !originAllowed(c, allowed) {
			response.Fail(c, csrfError("The request origin is not allowed."))
			return
		}

		cookie, err := c.Request.Cookie(cookies.CSRFCookieName())
		if err != nil || cookie.Value == "" {
			response.Fail(c, csrfError("Missing CSRF token."))
			return
		}

		header := c.GetHeader(auth.CSRFHeaderName)
		if header == "" {
			response.Fail(c, csrfError("Missing CSRF token."))
			return
		}

		// Constant-time: a byte-by-byte comparison would leak how much of the
		// token was correct.
		if subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) != 1 {
			response.Fail(c, csrfError("Invalid CSRF token."))
			return
		}

		c.Next()
	}
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// originAllowed checks the Origin header, falling back to Referer.
//
// A request with neither is rejected: every browser sets Origin on a
// state-changing cross-origin request, so its absence on a cookie-authenticated
// mutation is not something a legitimate browser does.
func originAllowed(c *gin.Context, allowed map[string]struct{}) bool {
	origin := c.GetHeader("Origin")
	if origin == "" {
		origin = originOfReferer(c.GetHeader("Referer"))
	}
	if origin == "" {
		return false
	}
	_, ok := allowed[normalizeOrigin(origin)]
	return ok
}

func csrfError(message string) *apierror.Error {
	return apierror.New(http.StatusForbidden, "CSRF_TOKEN_INVALID", message)
}
