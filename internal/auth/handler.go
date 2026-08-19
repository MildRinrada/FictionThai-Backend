package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/response"
)

// Handler exposes the authentication endpoints (docs/09 §12, docs/10 §49).
//
// It parses and validates request shape, then delegates every decision to the
// service. No business rule lives here.
type Handler struct {
	service *Service
	cookies CookieConfig
}

func NewHandler(service *Service, cookies CookieConfig) *Handler {
	return &Handler{service: service, cookies: cookies}
}

// registerRequest - docs/09 §12.
type registerRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Client   string `json:"client"`
}

type loginRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
	Client     string `json:"client"`
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

type resetPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

type verifyEmailRequest struct {
	Token string `json:"token"`
}

// authResponse is returned by register and login.
//
// Token is populated ONLY for native clients. For web it stays empty and the
// credential travels in a Set-Cookie header instead, so it never reaches
// JavaScript (docs/09 §4).
type authResponse struct {
	User  any     `json:"user"`
	Token *string `json:"token,omitempty"`
	// CSRFToken is returned for web clients as a convenience; it is also set as
	// a readable cookie.
	CSRFToken *string `json:"csrf_token,omitempty"`
}

// Register handles POST /api/v1/auth/register.
func (h *Handler) Register(c *gin.Context) {
	var req registerRequest
	if !bindJSON(c, &req) {
		return
	}

	kind, ok := ParseClientKind(req.Client)
	if !ok {
		response.Fail(c, invalidClientError())
		return
	}

	auth, err := h.service.Register(c.Request.Context(), RegisterParams{
		Username:   req.Username,
		Email:      req.Email,
		Password:   req.Password,
		ClientKind: kind,
		UserAgent:  c.Request.UserAgent(),
		IP:         c.ClientIP(),
	})
	if err != nil {
		response.Fail(c, err)
		return
	}

	h.respondWithAuthentication(c, http.StatusCreated, auth, kind)
}

// Login handles POST /api/v1/auth/login.
func (h *Handler) Login(c *gin.Context) {
	var req loginRequest
	if !bindJSON(c, &req) {
		return
	}

	if strings.TrimSpace(req.Identifier) == "" || req.Password == "" {
		// Same generic failure as a wrong password: an empty field must not be
		// distinguishable from a bad credential.
		response.Fail(c, invalidCredentialsError())
		return
	}

	kind, ok := ParseClientKind(req.Client)
	if !ok {
		response.Fail(c, invalidClientError())
		return
	}

	auth, err := h.service.Login(c.Request.Context(), LoginParams{
		Identifier: req.Identifier,
		Password:   req.Password,
		ClientKind: kind,
		UserAgent:  c.Request.UserAgent(),
		IP:         c.ClientIP(),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			response.Fail(c, invalidCredentialsError())
		case errors.Is(err, ErrAccountUnavailable):
			response.Fail(c, apierror.New(http.StatusForbidden, "ACCOUNT_UNAVAILABLE",
				"This account is not available. Please contact support."))
		default:
			response.Fail(c, err)
		}
		return
	}

	h.respondWithAuthentication(c, http.StatusOK, auth, kind)
}

// Logout handles POST /api/v1/auth/logout. Requires authentication.
func (h *Handler) Logout(c *gin.Context) {
	identity := IdentityFrom(c.Request.Context())
	if !identity.Authenticated() {
		response.Fail(c, apierror.Unauthorized("Authentication required."))
		return
	}

	if err := h.service.Logout(c.Request.Context(), identity.Session); err != nil {
		response.Fail(c, err)
		return
	}

	// Clear the cookies regardless of transport: a native client simply has
	// none to clear, and clearing is harmless.
	h.clearAuthCookies(c)
	response.NoContent(c)
}

// LogoutAll handles POST /api/v1/auth/logout-all (docs/10 §37).
func (h *Handler) LogoutAll(c *gin.Context) {
	identity := IdentityFrom(c.Request.Context())
	if !identity.Authenticated() {
		response.Fail(c, apierror.Unauthorized("Authentication required."))
		return
	}

	if _, err := h.service.LogoutAll(c.Request.Context(), identity.User.ID); err != nil {
		response.Fail(c, err)
		return
	}

	h.clearAuthCookies(c)
	response.NoContent(c)
}

// Me handles GET /api/v1/auth/me.
//
// docs/09 §12: a guest receives 401 here. That is not a guest-access violation -
// this endpoint is *about* the current account, unlike the public reading
// endpoints, which stay open.
func (h *Handler) Me(c *gin.Context) {
	identity := IdentityFrom(c.Request.Context())
	if !identity.Authenticated() {
		response.Fail(c, apierror.Unauthorized("Authentication required."))
		return
	}

	view, err := h.service.CurrentUserView(c.Request.Context(), identity.User)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// AttestAdult handles POST /api/v1/auth/adult-attestation (§13B).
//
// It takes no body. The request IS the statement, and a payload would invite
// exactly the extra fields - a birth date, a document - that this deliberately
// does not collect.
func (h *Handler) AttestAdult(c *gin.Context) {
	identity := IdentityFrom(c.Request.Context())
	if !identity.Authenticated() {
		response.Fail(c, apierror.Unauthorized("Authentication required."))
		return
	}

	if err := h.service.AttestAdult(c.Request.Context(), identity.UserID()); err != nil {
		response.Fail(c, err)
		return
	}

	view, err := h.service.CurrentUserView(c.Request.Context(), identity.User)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// ForgotPassword handles POST /api/v1/auth/password/forgot.
//
// Always 202, whatever happens. Any variation - status, body, or timing - would
// reveal whether the address is registered (docs/10 §16, docs/11 §27).
func (h *Handler) ForgotPassword(c *gin.Context) {
	var req forgotPasswordRequest
	if !bindJSON(c, &req) {
		return
	}

	h.service.RequestPasswordReset(c.Request.Context(), req.Email)

	response.Data(c, http.StatusAccepted, gin.H{
		"message": "If an account exists for this email, password reset instructions will be sent.",
	})
}

// ResetPassword handles POST /api/v1/auth/password/reset.
func (h *Handler) ResetPassword(c *gin.Context) {
	var req resetPasswordRequest
	if !bindJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		response.Fail(c, apierror.BadRequest("This reset link is invalid or has expired."))
		return
	}

	if err := h.service.ResetPassword(c.Request.Context(), req.Token, req.Password); err != nil {
		response.Fail(c, err)
		return
	}

	// The user's sessions are gone, including this browser's.
	h.clearAuthCookies(c)
	response.Data(c, http.StatusOK, gin.H{
		"message": "Your password has been changed. Please sign in again.",
	})
}

// VerifyEmail handles POST /api/v1/auth/verify-email.
func (h *Handler) VerifyEmail(c *gin.Context) {
	var req verifyEmailRequest
	if !bindJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		response.Fail(c, apierror.BadRequest("This verification link is invalid or has expired."))
		return
	}

	if err := h.service.VerifyEmail(c.Request.Context(), req.Token); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "Your email address has been verified."})
}

// respondWithAuthentication delivers the session over the transport the client
// asked for.
func (h *Handler) respondWithAuthentication(
	c *gin.Context,
	status int,
	auth *Authentication,
	kind ClientKind,
) {
	view, err := h.service.CurrentUserView(c.Request.Context(), auth.User)
	if err != nil {
		response.Fail(c, err)
		return
	}

	body := authResponse{User: view}

	if kind == ClientMobile {
		// Native: the raw token goes in the body for secure device storage. No
		// cookie is set - a native client has no cookie jar to protect.
		token := auth.RawToken
		body.Token = &token
		response.Data(c, status, body)
		return
	}

	// Web: the token goes in an HttpOnly cookie and never appears in the body.
	csrfToken, err := GenerateCSRFToken()
	if err != nil {
		response.Fail(c, err)
		return
	}

	http.SetCookie(c.Writer, h.cookies.NewSessionCookie(auth.RawToken, auth.Session.ExpiresAt))
	http.SetCookie(c.Writer, h.cookies.NewCSRFCookie(csrfToken, auth.Session.ExpiresAt))

	body.CSRFToken = &csrfToken
	response.Data(c, status, body)
}

func (h *Handler) clearAuthCookies(c *gin.Context) {
	http.SetCookie(c.Writer, h.cookies.ClearCookie(h.cookies.SessionCookieName()))
	http.SetCookie(c.Writer, h.cookies.ClearCookie(h.cookies.CSRFCookieName()))
}

// bindJSON decodes the request body, reporting a clean error on malformed input.
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		// An oversized body surfaces here, because the limit is enforced when
		// the body is read (middleware.BodyLimit).
		if strings.Contains(err.Error(), "http: request body too large") {
			response.Fail(c, apierror.New(http.StatusRequestEntityTooLarge,
				apierror.CodePayloadTooLarge, "The request body is too large."))
			return false
		}
		response.Fail(c, apierror.BadRequest("The request body could not be parsed as JSON."))
		return false
	}
	return true
}

// invalidCredentialsError is the ONE response for every failed sign-in, whatever
// the underlying cause (docs/10 §10, §47).
func invalidCredentialsError() *apierror.Error {
	return apierror.New(http.StatusUnauthorized, "INVALID_CREDENTIALS",
		"Invalid username or password.")
}

func invalidClientError() *apierror.Error {
	return apierror.Validation(map[string][]string{
		"client": {`Must be "web" or "native".`},
	})
}
