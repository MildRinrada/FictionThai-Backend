package middleware

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/internal/ratelimit"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// RateLimit applies a policy to the routes it is attached to.
//
// Attaching per route group rather than globally is what implements the tiered
// model in docs/09 §31: public reads, search, auth, writes, and AI each get
// their own budget.
//
// The key is the client IP for now. Once authentication lands, an authenticated
// request should be keyed by user ID instead, so one user behind a shared NAT
// cannot exhaust everyone else's budget - see docs/11 §24, which lists IP, user,
// endpoint, and auth state as inputs.
func RateLimit(limiter ratelimit.Limiter, policy ratelimit.Policy) gin.HandlerFunc {
	return func(c *gin.Context) {
		res := limiter.Allow(c.Request.Context(), rateLimitKey(c), policy)

		h := c.Writer.Header()
		h.Set("RateLimit-Limit", strconv.Itoa(res.Limit))
		h.Set("RateLimit-Remaining", strconv.Itoa(res.Remaining))

		if !res.Allowed {
			retryAfter := int(res.RetryAfter.Seconds())
			if retryAfter < 1 {
				retryAfter = 1
			}
			h.Set("Retry-After", strconv.Itoa(retryAfter))

			response.Fail(c, apierror.New(
				http.StatusTooManyRequests,
				apierror.CodeRateLimited,
				"Too many requests.",
			))
			return
		}

		c.Next()
	}
}

// rateLimitKey identifies the caller.
//
// An authenticated request is keyed by user ID so that many readers behind one
// NAT or campus network do not share - and exhaust - a single budget. A guest
// is keyed by IP, which is the only stable signal available.
//
// docs/11 §24 lists IP, user, endpoint, and authentication state as the inputs;
// the endpoint dimension comes from the policy name, which is part of the
// counter key inside the limiter.
//
// IP-based protection is NOT removed by this: an attacker cannot obtain a user
// key without first authenticating, and the unauthenticated endpoints they
// would attack - login, registration, password reset - have no identity to key
// on and therefore remain IP-limited.
//
// gin's ClientIP honours the engine's trusted-proxy configuration, so a spoofed
// X-Forwarded-For from an untrusted hop cannot be used to dodge the limit.
func rateLimitKey(c *gin.Context) string {
	if userID := UserIDFrom(c); userID != "" {
		return "user:" + userID
	}
	return "ip:" + c.ClientIP()
}
