package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"
)

// Cookie names.
//
// The `__Host-` prefix is not decoration: a browser refuses to accept a
// `__Host-` cookie unless it is Secure, has Path=/, and carries NO Domain
// attribute. That makes it impossible for a sibling subdomain - or an attacker
// who obtains one - to set or overwrite the session cookie. It requires the
// same-origin reverse-proxy deployment (docs/07 §58), which is the documented
// production model.
//
// The prefix cannot be used over plain HTTP, so development falls back to an
// unprefixed name. The fallback exists ONLY for localhost.
const (
	SessionCookieName    = "__Host-session"
	SessionCookieNameDev = "ft_session"
	CSRFCookieName       = "__Host-csrf"
	CSRFCookieNameDev    = "ft_csrf"
	CSRFHeaderName       = "X-CSRF-Token"
	csrfTokenBytes       = 32
)

// CookieConfig describes how authentication cookies are written.
type CookieConfig struct {
	// Secure marks cookies Secure and enables the __Host- prefix. True in
	// staging and production; false only for local HTTP development.
	Secure bool
}

// SessionCookieName returns the session cookie name for this configuration.
func (c CookieConfig) SessionCookieName() string {
	if c.Secure {
		return SessionCookieName
	}
	return SessionCookieNameDev
}

// CSRFCookieName returns the CSRF cookie name for this configuration.
func (c CookieConfig) CSRFCookieName() string {
	if c.Secure {
		return CSRFCookieName
	}
	return CSRFCookieNameDev
}

// NewSessionCookie builds the session cookie.
//
//	HttpOnly  JavaScript cannot read it, so an XSS payload cannot exfiltrate
//	          the session (docs/11 §43)
//	Secure    never sent over plain HTTP in production (docs/10 §43)
//	SameSite  Lax - not sent on cross-site POST/PATCH/DELETE, which is the
//	          first layer of CSRF defence (docs/10 §40)
//	Path=/    required by the __Host- prefix
//	no Domain required by the __Host- prefix
func (c CookieConfig) NewSessionCookie(rawToken string, expiresAt time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     c.SessionCookieName(),
		Value:    rawToken,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// NewCSRFCookie builds the CSRF cookie.
//
// Deliberately NOT HttpOnly: the frontend must read this value to echo it in
// the X-CSRF-Token header. That is safe because the CSRF token is not a
// credential - on its own it authenticates nothing. The session cookie, which
// IS the credential, stays HttpOnly.
func (c CookieConfig) NewCSRFCookie(token string, expiresAt time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     c.CSRFCookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: false,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearCookie returns a cookie that deletes the named cookie.
//
// The attributes must match those the cookie was set with, or the browser
// treats it as a different cookie and the original survives logout.
func (c CookieConfig) ClearCookie(name string) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: name == c.SessionCookieName(),
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// GenerateCSRFToken returns a fresh random CSRF token.
func GenerateCSRFToken() (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
