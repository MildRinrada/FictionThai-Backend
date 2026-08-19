package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/middleware"
	"github.com/fictionthai/fictionthai/backend/internal/users"
)

const testOrigin = "http://localhost:3000"

var testCookies = auth.CookieConfig{Secure: false}

// csrfRouter builds a router whose identity is injected directly, so the CSRF
// rules can be exercised without a database.
func csrfRouter(identity *auth.Identity) http.Handler {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	r.Use(func(c *gin.Context) {
		if identity != nil {
			c.Request = c.Request.WithContext(auth.WithIdentity(c.Request.Context(), identity))
		}
		c.Next()
	})
	r.Use(middleware.CSRF(testCookies, []string{testOrigin}))

	handler := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }
	r.GET("/resource", handler)
	r.POST("/resource", handler)
	r.PATCH("/resource", handler)
	r.DELETE("/resource", handler)

	return r
}

func cookieIdentity() *auth.Identity {
	return &auth.Identity{
		User:      &users.User{ID: uuid.New(), Status: users.StatusActive},
		Session:   &auth.Session{ID: uuid.New(), ClientKind: auth.ClientWeb},
		Transport: auth.TransportCookie,
	}
}

func bearerIdentity() *auth.Identity {
	return &auth.Identity{
		User:      &users.User{ID: uuid.New(), Status: users.StatusActive},
		Session:   &auth.Session{ID: uuid.New(), ClientKind: auth.ClientMobile},
		Transport: auth.TransportBearer,
	}
}

type csrfRequest struct {
	method     string
	origin     string
	cookieVal  string
	headerVal  string
	omitCookie bool
}

func doCSRF(t *testing.T, router http.Handler, req csrfRequest) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(req.method, "/resource", nil)
	if req.origin != "" {
		r.Header.Set("Origin", req.origin)
	}
	if !req.omitCookie && req.cookieVal != "" {
		r.AddCookie(&http.Cookie{Name: testCookies.CSRFCookieName(), Value: req.cookieVal})
	}
	if req.headerVal != "" {
		r.Header.Set(auth.CSRFHeaderName, req.headerVal)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// Reads never mutate state, so there is nothing for CSRF to protect
// (docs/10 §40).
func TestCSRF_AllowsSafeMethods(t *testing.T) {
	router := csrfRouter(cookieIdentity())

	rec := doCSRF(t, router, csrfRequest{method: http.MethodGet, origin: testOrigin})
	if rec.Code != http.StatusOK {
		t.Errorf("GET status = %d, want 200 - safe methods need no CSRF token", rec.Code)
	}
}

// A guest has no ambient credential to abuse, so login and registration must
// not require a token a first-time visitor cannot have.
func TestCSRF_AllowsUnauthenticatedRequests(t *testing.T) {
	router := csrfRouter(nil)

	rec := doCSRF(t, router, csrfRequest{method: http.MethodPost, origin: testOrigin})
	if rec.Code != http.StatusOK {
		t.Errorf("guest POST status = %d, want 200", rec.Code)
	}
}

// The core requirement: a cookie-authenticated mutation with no token fails.
func TestCSRF_RejectsCookieAuthMutationWithoutToken(t *testing.T) {
	router := csrfRouter(cookieIdentity())

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := doCSRF(t, router, csrfRequest{method: method, origin: testOrigin})
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s status = %d, want 403", method, rec.Code)
			}
		})
	}
}

func TestCSRF_RejectsMismatchedToken(t *testing.T) {
	router := csrfRouter(cookieIdentity())

	rec := doCSRF(t, router, csrfRequest{
		method:    http.MethodPost,
		origin:    testOrigin,
		cookieVal: "the-real-token",
		headerVal: "a-different-token",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a mismatched token", rec.Code)
	}
}

func TestCSRF_RejectsHeaderWithoutCookie(t *testing.T) {
	router := csrfRouter(cookieIdentity())

	// An attacker who can guess or set a header still cannot read the cookie.
	rec := doCSRF(t, router, csrfRequest{
		method:     http.MethodPost,
		origin:     testOrigin,
		headerVal:  "guessed-token",
		omitCookie: true,
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when the CSRF cookie is absent", rec.Code)
	}
}

func TestCSRF_AcceptsMatchingToken(t *testing.T) {
	router := csrfRouter(cookieIdentity())

	rec := doCSRF(t, router, csrfRequest{
		method:    http.MethodPost,
		origin:    testOrigin,
		cookieVal: "matching-token",
		headerVal: "matching-token",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a valid double-submit token. body: %s",
			rec.Code, rec.Body.String())
	}
}

// Even with a correct token, a request from an unknown origin is refused -
// defence in depth against a token that leaked some other way.
func TestCSRF_RejectsUntrustedOrigin(t *testing.T) {
	router := csrfRouter(cookieIdentity())

	rec := doCSRF(t, router, csrfRequest{
		method:    http.MethodPost,
		origin:    "https://evil.example",
		cookieVal: "matching-token",
		headerVal: "matching-token",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an untrusted origin", rec.Code)
	}
}

// Every browser sets Origin on a cross-origin state-changing request, so its
// absence on a cookie-authenticated mutation is not legitimate browser traffic.
func TestCSRF_RejectsMissingOrigin(t *testing.T) {
	router := csrfRouter(cookieIdentity())

	rec := doCSRF(t, router, csrfRequest{
		method:    http.MethodPost,
		cookieVal: "matching-token",
		headerVal: "matching-token",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when Origin is absent", rec.Code)
	}
}

// A Bearer token is not ambient: an attacker's page cannot cause it to be sent,
// so requiring CSRF on mobile would be friction with no benefit.
func TestCSRF_ExemptsBearerAuthentication(t *testing.T) {
	router := csrfRouter(bearerIdentity())

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			// No Origin, no cookie, no CSRF header - exactly how a native
			// client calls the API.
			rec := doCSRF(t, router, csrfRequest{method: method})
			if rec.Code != http.StatusOK {
				t.Errorf("%s status = %d, want 200 for Bearer authentication. body: %s",
					method, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCSRF_ErrorUsesStableCode(t *testing.T) {
	router := csrfRouter(cookieIdentity())

	rec := doCSRF(t, router, csrfRequest{method: http.MethodPost, origin: testOrigin})

	if body := rec.Body.String(); !containsAll(body, `"code"`, "CSRF_TOKEN_INVALID") {
		t.Errorf("body = %s, want a stable CSRF_TOKEN_INVALID code", body)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !contains(haystack, needle) {
			return false
		}
	}
	return true
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
