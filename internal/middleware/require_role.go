package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RequireRole rejects a signed-in caller who holds none of the given roles.
//
// This is a COARSE gate for whole route groups such as /admin (docs/10 §25,
// §32). It is not, and must never become, the ownership check: a moderator role
// says nothing about whether a specific fiction belongs to a specific writer.
// Per-resource ownership is verified inside the owning service (docs/10 §27,
// docs/11 §8) - putting it here would let a second endpoint calling the same
// service bypass it entirely.
//
// Returns 401 when nobody is signed in and 403 when someone is but lacks the
// role - the distinction docs/10 §48 requires.
func RequireRole(roles ...users.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := auth.IdentityFrom(c.Request.Context())

		if !identity.Authenticated() {
			response.Fail(c, apierror.Unauthorized("Authentication required."))
			return
		}
		if !identity.HasRole(roles...) {
			// The message never names the required role - that would tell an
			// attacker which privilege to target.
			response.Fail(c, apierror.Forbidden("You do not have permission to perform this action."))
			return
		}
		c.Next()
	}
}

// RequireStaff admits moderators and admins.
func RequireStaff() gin.HandlerFunc {
	return RequireRole(users.RoleModerator, users.RoleAdmin)
}

// RequireAdmin admits admins only.
func RequireAdmin() gin.HandlerFunc {
	return RequireRole(users.RoleAdmin)
}
