package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RequireAuth rejects requests that carry no valid identity.
//
// It must run AFTER Authenticate, which does the actual credential validation.
// This middleware only asks "is someone signed in?" - it answers authentication,
// never authorization (docs/10 §3).
//
// 401 means "not authenticated". 403 - which this never returns - means
// "authenticated but not allowed" (docs/10 §48).
func RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := auth.IdentityFrom(c.Request.Context())
		if !identity.Authenticated() {
			response.Fail(c, apierror.Unauthorized("Authentication required."))
			return
		}
		c.Next()
	}
}

// RequireVerifiedEmail rejects a signed-in user whose address is unconfirmed.
//
// Phase 1 decision, from docs/10 §17: verification gates PUBLISHING, never
// reading or ordinary account use. Apply this only to publishing routes when
// the writer domain lands - never to a read path.
//
// It returns 403, not 401: the caller IS authenticated, they simply lack a
// required attribute.
func RequireVerifiedEmail() gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := auth.IdentityFrom(c.Request.Context())
		if !identity.Authenticated() {
			response.Fail(c, apierror.Unauthorized("Authentication required."))
			return
		}
		if !identity.EmailVerified() {
			response.Fail(c, apierror.New(403, "EMAIL_VERIFICATION_REQUIRED",
				"Please verify your email address before publishing."))
			return
		}
		c.Next()
	}
}
