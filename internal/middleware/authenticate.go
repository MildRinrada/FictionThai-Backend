package middleware

import (
	"context"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
)

// Authenticator is the single credential-validation path (auth.Service).
type Authenticator interface {
	Authenticate(ctx context.Context, rawToken string) (*auth.Identity, error)
}

// Authenticate resolves the caller's identity, if any.
//
// It is OPTIONAL authentication and never rejects a request. A guest continues
// with a nil identity, because guest reading is a first-class product feature -
// docs/10 §2.1 and docs/11 §12 - and a global 401 here would break it.
// Endpoints that need a signed-in user compose RequireAuth on top.
//
// Two transports are accepted, in this order:
//
//	Authorization: Bearer <token>   native clients (docs/10 §12)
//	Session cookie                  browsers (docs/10 §11)
//
// Bearer is checked first so that a native client with a stale cookie in a
// shared webview cannot have its explicit credential silently overridden.
func Authenticate(authenticator Authenticator, cookies auth.CookieConfig, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawToken, transport := extractCredential(c, cookies)
		if rawToken == "" {
			c.Next()
			return
		}

		identity, err := authenticator.Authenticate(c.Request.Context(), rawToken)
		if err != nil {
			// An infrastructure failure must not be reported as "not
			// authenticated" - that would silently downgrade a signed-in user
			// to a guest during a database blip. Log and continue as a guest;
			// protected routes then return 401, which is the safe direction.
			log.ErrorContext(c.Request.Context(), "session validation failed",
				slog.String("request_id", RequestIDFrom(c)),
				slog.Any("error", err),
			)
			c.Next()
			return
		}
		if identity == nil {
			// Expired, revoked, invalidated, or forged. Deliberately
			// indistinguishable from one another.
			c.Next()
			return
		}

		// Record how the credential arrived: the CSRF middleware protects
		// cookie-authenticated requests only.
		identity.Transport = transport

		c.Request = c.Request.WithContext(auth.WithIdentity(c.Request.Context(), identity))
		c.Set(ContextUserID, identity.User.ID.String())

		c.Next()
	}
}

// ContextUserID is the gin key holding the authenticated user's ID. It is set
// for logging and rate limiting; authorization always reads the full identity.
const ContextUserID = "user_id"

// UserIDFrom returns the authenticated user's ID, or "" for a guest.
func UserIDFrom(c *gin.Context) string {
	id, _ := c.Get(ContextUserID)
	s, _ := id.(string)
	return s
}

// extractCredential pulls the raw token out of the request.
func extractCredential(c *gin.Context, cookies auth.CookieConfig) (string, auth.Transport) {
	if header := c.GetHeader("Authorization"); header != "" {
		const prefix = "Bearer "
		if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
			if token := strings.TrimSpace(header[len(prefix):]); token != "" {
				return token, auth.TransportBearer
			}
		}
	}

	// Accept both the production __Host- name and the development name, so a
	// developer switching APP_ENV is not silently logged out.
	for _, name := range []string{cookies.SessionCookieName(), auth.SessionCookieNameDev} {
		if cookie, err := c.Request.Cookie(name); err == nil && cookie.Value != "" {
			return cookie.Value, auth.TransportCookie
		}
	}

	return "", auth.TransportNone
}
