package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
)

// ClientKind is the transport a session was issued for.
//
// It is declared explicitly by the client at login and never inferred from the
// User-Agent: guessing wrong would either leak a raw token to a browser or
// leave a native app with a cookie it cannot store.
type ClientKind string

const (
	ClientWeb    ClientKind = "web"
	ClientMobile ClientKind = "mobile"
)

func (c ClientKind) Valid() bool { return c == ClientWeb || c == ClientMobile }

// ParseClientKind maps the API's `client` field onto a ClientKind.
//
// The API vocabulary is "web" / "native"; the stored value is "web" / "mobile".
// "native" is accepted because that is what the client declares, and "mobile"
// because that is what the database records.
func ParseClientKind(raw string) (ClientKind, bool) {
	switch raw {
	case "", "web":
		return ClientWeb, true
	case "native", "mobile":
		return ClientMobile, true
	}
	return "", false
}

// tokenBytes is the raw entropy in a session token. 32 bytes = 256 bits, which
// is far beyond guessable and is why the stored digest can be a fast hash.
const tokenBytes = 32

// Session mirrors a `user_sessions` row. It never holds the raw token - that
// value exists only in the response that created it.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  string
	ClientKind ClientKind
	ExpiresAt  time.Time
	LastUsedAt time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
	UserAgent  *string
	IPPrefix   *string
}

// Active reports whether the session is usable at instant now.
//
// This does NOT consider users.sessions_invalidated_before - that check needs
// the user record and lives in the service's single validation path.
func (s *Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// GenerateToken returns a new raw session token and its storage digest.
//
// The raw value is returned once and never persisted (docs/08 §29). SHA-256 is
// the right digest here: the input is already 256 bits of CSPRNG output, so
// there is nothing to slow down a brute force against, and a slow KDF would tax
// every authenticated request.
func GenerateToken() (raw string, digest string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate session token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashToken(raw), nil
}

// HashToken returns the storage digest for a raw token.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Lifetime is the expiry policy for one client kind.
type Lifetime struct {
	// Absolute caps total session age regardless of activity.
	Absolute time.Duration
	// Idle expires a session that has not been used recently.
	Idle time.Duration
}

// Expiry returns the effective expiry for a session last used at lastUsed.
//
// The session dies at whichever comes first: the absolute cap measured from
// creation, or the idle window measured from last use (docs/10 §15).
func (l Lifetime) Expiry(createdAt, lastUsed time.Time) time.Time {
	absolute := createdAt.Add(l.Absolute)
	idle := lastUsed.Add(l.Idle)
	if idle.Before(absolute) {
		return idle
	}
	return absolute
}

// TruncateIP reduces an address to a coarse network prefix.
//
// docs/11 §34 requires data minimisation. A /24 (IPv4) or /48 (IPv6) is enough
// to notice that a session is being replayed from a different network, without
// storing an identifier that pinpoints an individual subscriber.
// Returns nil when the address cannot be parsed, so a malformed value is simply
// not recorded.
func TruncateIP(raw string) *string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return nil
	}

	var masked net.IP
	if v4 := ip.To4(); v4 != nil {
		masked = v4.Mask(net.CIDRMask(24, 32))
		out := masked.String() + "/24"
		return &out
	}
	masked = ip.Mask(net.CIDRMask(48, 128))
	out := masked.String() + "/48"
	return &out
}

// truncateUserAgent bounds a stored User-Agent string. It is kept for a future
// device list (docs/10 §56); an unbounded header would let a client write
// arbitrarily large rows.
func truncateUserAgent(raw string) *string {
	const maxUserAgentLength = 400
	if raw == "" {
		return nil
	}
	if len(raw) > maxUserAgentLength {
		raw = raw[:maxUserAgentLength]
	}
	return &raw
}
