package auth

import (
	"context"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/users"
)

// Transport is how the credential reached the server on THIS request.
//
// It is distinct from Session.ClientKind, which records how the session was
// originally issued. CSRF applies based on how the credential arrived right
// now, not on what it was minted for.
type Transport string

const (
	TransportNone   Transport = ""
	TransportCookie Transport = "cookie"
	TransportBearer Transport = "bearer"
)

// Identity is the authenticated caller, resolved once per request.
//
// A nil *Identity means "guest", which is a fully valid state - guest reading
// is a product requirement, not an error condition (docs/10 §2.1, docs/11 §12).
type Identity struct {
	User    *users.User
	Session *Session

	// Transport records how this request presented its credential.
	Transport Transport
}

// UserID returns the caller's ID, or uuid.Nil for a guest.
func (i *Identity) UserID() uuid.UUID {
	if i == nil || i.User == nil {
		return uuid.Nil
	}
	return i.User.ID
}

// Authenticated reports whether a caller is signed in.
func (i *Identity) Authenticated() bool { return i != nil && i.User != nil }

// HasRole reports whether the caller holds one of the given roles.
//
// Role alone is never sufficient to authorise access to a specific resource;
// ownership is checked separately in the owning service (docs/10 §19, §27).
func (i *Identity) HasRole(roles ...users.Role) bool {
	if !i.Authenticated() {
		return false
	}
	for _, role := range roles {
		if i.User.Role == role {
			return true
		}
	}
	return false
}

// IsStaff reports whether the caller is a moderator or admin.
func (i *Identity) IsStaff() bool { return i.Authenticated() && i.User.Role.IsStaff() }

// EmailVerified reports whether the caller has confirmed their address.
//
// Phase 1 decision: verification gates PUBLISHING, never reading or ordinary
// account use (docs/10 §17).
func (i *Identity) EmailVerified() bool { return i.Authenticated() && i.User.EmailVerified() }

// UsedCookieTransport reports whether the credential arrived as a cookie.
//
// This is what decides whether CSRF protection applies. A cookie is an AMBIENT
// credential - the browser attaches it to any request to the origin, including
// one triggered by an attacker's page. A Bearer token has to be set deliberately
// by the client, so a cross-site page cannot cause it to be sent, which is why
// Bearer requests are exempt (docs/10 §40, docs/11 §22).
func (i *Identity) UsedCookieTransport() bool {
	return i.Authenticated() && i.Transport == TransportCookie
}

// identityKey is unexported so no other package can write an identity into the
// context and impersonate a user.
type identityKey struct{}

// WithIdentity stores the resolved identity on a context.
func WithIdentity(ctx context.Context, identity *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFrom returns the identity carried by ctx, or nil for a guest.
func IdentityFrom(ctx context.Context) *Identity {
	identity, _ := ctx.Value(identityKey{}).(*Identity)
	return identity
}
