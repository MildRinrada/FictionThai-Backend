package auth_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
)

func TestGenerateToken_ProducesDistinctHighEntropyTokens(t *testing.T) {
	const iterations = 200
	seen := make(map[string]struct{}, iterations)

	for i := 0; i < iterations; i++ {
		raw, digest, err := auth.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken() error = %v", err)
		}

		if _, duplicate := seen[raw]; duplicate {
			t.Fatal("GenerateToken produced a duplicate token")
		}
		seen[raw] = struct{}{}

		// 32 random bytes, base64url without padding, is 43 characters.
		if len(raw) != 43 {
			t.Errorf("token length = %d, want 43 (32 random bytes)", len(raw))
		}
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			t.Errorf("token is not valid base64url: %v", err)
		}
		if len(decoded) != 32 {
			t.Errorf("token carries %d bytes of entropy, want 32", len(decoded))
		}

		if digest != auth.HashToken(raw) {
			t.Error("the returned digest does not match HashToken of the raw token")
		}
	}
}

// The stored digest must not be reversible to the token, and must not BE the
// token - otherwise a database leak would hand over every live session.
func TestHashToken_IsNotTheRawToken(t *testing.T) {
	raw, digest, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}

	if digest == raw {
		t.Fatal("the stored digest is the raw token")
	}
	if strings.Contains(digest, raw) {
		t.Fatal("the stored digest contains the raw token")
	}
	if len(digest) != 64 { // SHA-256, hex-encoded
		t.Errorf("digest length = %d, want 64 hex characters", len(digest))
	}
}

func TestHashToken_IsDeterministic(t *testing.T) {
	// Validation depends on this: the digest computed from a presented token
	// must equal the one stored at login.
	if auth.HashToken("some-token") != auth.HashToken("some-token") {
		t.Fatal("HashToken is not deterministic")
	}
	if auth.HashToken("token-a") == auth.HashToken("token-b") {
		t.Fatal("different tokens produced the same digest")
	}
}

func TestLifetime_ExpiryTakesTheEarlierBound(t *testing.T) {
	created := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	lifetime := auth.Lifetime{Absolute: 14 * 24 * time.Hour, Idle: 7 * 24 * time.Hour}

	t.Run("idle wins for a fresh session", func(t *testing.T) {
		// Just created: idle (created+7d) is earlier than absolute (created+14d).
		got := lifetime.Expiry(created, created)
		want := created.Add(7 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("expiry = %v, want %v", got, want)
		}
	})

	t.Run("absolute caps a continuously used session", func(t *testing.T) {
		// Used on day 13: idle would reach day 20, but the absolute cap is
		// day 14 - a session must not live forever just because it stays busy.
		lastUsed := created.Add(13 * 24 * time.Hour)
		got := lifetime.Expiry(created, lastUsed)
		want := created.Add(14 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("expiry = %v, want the absolute cap %v", got, want)
		}
	})
}

func TestSession_Active(t *testing.T) {
	now := time.Now()
	revoked := now.Add(-time.Minute)

	tests := map[string]struct {
		session auth.Session
		want    bool
	}{
		"live":    {auth.Session{ExpiresAt: now.Add(time.Hour)}, true},
		"expired": {auth.Session{ExpiresAt: now.Add(-time.Hour)}, false},
		"revoked": {auth.Session{ExpiresAt: now.Add(time.Hour), RevokedAt: &revoked}, false},
		"revoked and expired": {
			auth.Session{ExpiresAt: now.Add(-time.Hour), RevokedAt: &revoked}, false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.session.Active(now); got != tc.want {
				t.Errorf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}

// docs/11 §34 requires data minimisation: a stored address must be coarse
// enough not to identify an individual subscriber.
func TestTruncateIP(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"ipv4":             {"203.0.113.42", "203.0.113.0/24"},
		"ipv4 low octet":   {"198.51.100.1", "198.51.100.0/24"},
		"ipv6":             {"2001:db8:1234:5678::1", "2001:db8:1234::/48"},
		"ipv4 as loopback": {"127.0.0.1", "127.0.0.0/24"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := auth.TruncateIP(tc.in)
			if got == nil {
				t.Fatalf("TruncateIP(%q) = nil, want %q", tc.in, tc.want)
			}
			if *got != tc.want {
				t.Errorf("TruncateIP(%q) = %q, want %q", tc.in, *got, tc.want)
			}
			// The full address must not survive truncation.
			if *got == tc.in {
				t.Error("the full address was stored unmodified")
			}
		})
	}
}

func TestTruncateIP_RejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "not-an-ip", "999.999.999.999", "localhost"} {
		if got := auth.TruncateIP(raw); got != nil {
			t.Errorf("TruncateIP(%q) = %q, want nil", raw, *got)
		}
	}
}

func TestParseClientKind(t *testing.T) {
	tests := map[string]struct {
		in    string
		want  auth.ClientKind
		valid bool
	}{
		"empty defaults to web": {"", auth.ClientWeb, true},
		"web":                   {"web", auth.ClientWeb, true},
		"native":                {"native", auth.ClientMobile, true},
		"mobile":                {"mobile", auth.ClientMobile, true},
		"unknown":               {"desktop", "", false},
		// The value is declared explicitly, so a User-Agent string is not a
		// valid client kind.
		"user agent": {"Mozilla/5.0", "", false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := auth.ParseClientKind(tc.in)
			if ok != tc.valid {
				t.Fatalf("ParseClientKind(%q) ok = %v, want %v", tc.in, ok, tc.valid)
			}
			if ok && got != tc.want {
				t.Errorf("ParseClientKind(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGenerateCSRFToken(t *testing.T) {
	first, err := auth.GenerateCSRFToken()
	if err != nil {
		t.Fatalf("GenerateCSRFToken() error = %v", err)
	}
	second, err := auth.GenerateCSRFToken()
	if err != nil {
		t.Fatalf("GenerateCSRFToken() error = %v", err)
	}

	if first == second {
		t.Fatal("two CSRF tokens are identical")
	}
	if len(first) < 40 {
		t.Errorf("CSRF token length = %d, want at least 40 characters", len(first))
	}
}

// The __Host- prefix only works with Secure; falling back in development is
// deliberate, but production must always get the prefixed name.
func TestCookieConfig_NamesByEnvironment(t *testing.T) {
	secure := auth.CookieConfig{Secure: true}
	insecure := auth.CookieConfig{Secure: false}

	if got := secure.SessionCookieName(); got != auth.SessionCookieName {
		t.Errorf("secure session cookie = %q, want %q", got, auth.SessionCookieName)
	}
	if !strings.HasPrefix(secure.SessionCookieName(), "__Host-") {
		t.Error("the production session cookie must use the __Host- prefix")
	}
	if strings.HasPrefix(insecure.SessionCookieName(), "__Host-") {
		t.Error("the __Host- prefix cannot be used without Secure; development must fall back")
	}
}

func TestCookieConfig_SessionCookieAttributes(t *testing.T) {
	cookies := auth.CookieConfig{Secure: true}
	expiry := time.Now().Add(14 * 24 * time.Hour)

	cookie := cookies.NewSessionCookie("raw-token", expiry)

	if !cookie.HttpOnly {
		t.Error("the session cookie must be HttpOnly so XSS cannot read it")
	}
	if !cookie.Secure {
		t.Error("the session cookie must be Secure in production")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want / (required by __Host-)", cookie.Path)
	}
	if cookie.Domain != "" {
		t.Errorf("Domain = %q, want empty (required by __Host-)", cookie.Domain)
	}
}

func TestCookieConfig_CSRFCookieIsReadable(t *testing.T) {
	cookies := auth.CookieConfig{Secure: true}

	cookie := cookies.NewCSRFCookie("csrf-token", time.Now().Add(time.Hour))

	// Deliberately readable: the frontend has to echo it in a header. It is not
	// a credential on its own.
	if cookie.HttpOnly {
		t.Error("the CSRF cookie must be readable by JavaScript for double-submit")
	}
	if !cookie.Secure {
		t.Error("the CSRF cookie should still be Secure in production")
	}
}

func TestCookieConfig_ClearCookieExpiresImmediately(t *testing.T) {
	cookies := auth.CookieConfig{Secure: true}

	cookie := cookies.ClearCookie(cookies.SessionCookieName())

	if cookie.Value != "" {
		t.Errorf("cleared cookie value = %q, want empty", cookie.Value)
	}
	if cookie.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative so the browser deletes it", cookie.MaxAge)
	}
	// Attributes must match the original or the browser keeps the old cookie.
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want / to match the cookie being cleared", cookie.Path)
	}
}
