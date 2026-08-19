package users_test

import (
	"strings"
	"testing"

	"github.com/fictionthai/fictionthai/backend/internal/users"
)

func TestNormalizeUsername(t *testing.T) {
	tests := map[string]string{
		"  Writer  ": "writer",
		"WRITER":     "writer",
		"WrItEr_01":  "writer_01",
	}
	for in, want := range tests {
		if got := users.NormalizeUsername(in); got != want {
			t.Errorf("NormalizeUsername(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := users.NormalizeEmail("  Reader@Example.COM "); got != "reader@example.com" {
		t.Errorf("NormalizeEmail() = %q, want reader@example.com", got)
	}

	// Dots and +tags must be preserved. Stripping them is a guess about one
	// provider's semantics and would merge two genuinely distinct accounts.
	for _, addr := range []string{"a.b@example.com", "user+tag@example.com"} {
		if got := users.NormalizeEmail(addr); got != addr {
			t.Errorf("NormalizeEmail(%q) = %q; local-part structure must be preserved", addr, got)
		}
	}
}

func TestValidateUsername_Accepts(t *testing.T) {
	valid := []string{"writer", "abc", "reader_01", "thai-writer", strings.Repeat("a", 32)}

	for _, username := range valid {
		if msg := users.ValidateUsername(username); msg != "" {
			t.Errorf("ValidateUsername(%q) = %q, want accepted", username, msg)
		}
	}
}

func TestValidateUsername_Rejects(t *testing.T) {
	tests := map[string]string{
		"empty":                "",
		"too short":            "ab",
		"too long":             strings.Repeat("a", 33),
		"space":                "two words",
		"dot":                  "writer.name",
		"at sign":              "writer@example",
		"slash breaks routing": "writer/admin",
		// docs/10 §7: prevent impersonation. A Cyrillic "а" renders exactly like
		// a Latin "a", so a Unicode username could impersonate another account.
		"cyrillic homograph": "аdmin",
		"thai script":        "นักเขียน",
		"emoji":              "writer🎉",
	}

	for name, username := range tests {
		t.Run(name, func(t *testing.T) {
			if msg := users.ValidateUsername(username); msg == "" {
				t.Errorf("ValidateUsername(%q) was accepted; it must be rejected", username)
			}
		})
	}
}

// docs/10 §7: reserved names must not be claimable.
func TestValidateUsername_RejectsReservedNames(t *testing.T) {
	reserved := []string{"admin", "Admin", "ADMIN", "moderator", "support", "api", "root", "security"}

	for _, username := range reserved {
		if !users.IsReservedUsername(username) {
			t.Errorf("%q should be reserved", username)
		}
		if msg := users.ValidateUsername(users.NormalizeUsername(username)); msg == "" {
			t.Errorf("reserved username %q was accepted", username)
		}
	}
}

// A username that collides with a top-level route would make /author/{username}
// ambiguous (docs/03).
func TestValidateUsername_RejectsRouteCollisions(t *testing.T) {
	for _, username := range []string{"login", "register", "settings", "studio", "explore", "library"} {
		if !users.IsReservedUsername(username) {
			t.Errorf("route name %q should be reserved", username)
		}
	}
}

// The rejection message must not reveal WHY a name is unavailable - "reserved"
// would tell an attacker they found a system name.
func TestValidateUsername_ReservedMessageIsNeutral(t *testing.T) {
	msg := users.ValidateUsername("admin")
	if strings.Contains(strings.ToLower(msg), "reserved") {
		t.Errorf("message %q reveals that the name is reserved", msg)
	}
}

func TestValidateEmail(t *testing.T) {
	valid := []string{"reader@example.com", "a.b+tag@sub.example.co.th", "x@y.io"}
	for _, addr := range valid {
		if msg := users.ValidateEmail(addr); msg != "" {
			t.Errorf("ValidateEmail(%q) = %q, want accepted", addr, msg)
		}
	}

	invalid := map[string]string{
		"empty":     "",
		"no at":     "reader.example.com",
		"no domain": "reader@",
		"no local":  "@example.com",
		"spaces":    "reader @example.com",
		"too long":  strings.Repeat("a", 250) + "@example.com",
	}
	for name, addr := range invalid {
		t.Run(name, func(t *testing.T) {
			if msg := users.ValidateEmail(addr); msg == "" {
				t.Errorf("ValidateEmail(%q) was accepted", addr)
			}
		})
	}
}

// docs/10 §9: prefer long and unique over forced character classes.
func TestValidatePassword(t *testing.T) {
	valid := []string{
		"correct horse battery staple",
		strings.Repeat("a", 12),
		"รหัสผ่านภาษาไทยที่ยาวพอ", // Thai passphrases must work
	}
	for _, password := range valid {
		if msg := users.ValidatePassword(password); msg != "" {
			t.Errorf("ValidatePassword(%q) = %q, want accepted", password, msg)
		}
	}

	invalid := map[string]string{
		"empty":     "",
		"too short": strings.Repeat("a", 11),
		// Bounded so a huge input cannot be used to burn Argon2id CPU.
		"too long": strings.Repeat("a", 257),
	}
	for name, password := range invalid {
		t.Run(name, func(t *testing.T) {
			if msg := users.ValidatePassword(password); msg == "" {
				t.Errorf("ValidatePassword(%s) was accepted", name)
			}
		})
	}
}

// A complex-but-short password must still be rejected: length is the rule.
func TestValidatePassword_DoesNotRewardCharacterClasses(t *testing.T) {
	if msg := users.ValidatePassword("Aa1!Bb2@"); msg == "" {
		t.Error("an 8-character password was accepted despite the 12-character minimum")
	}
}

func TestRoleAndStatus(t *testing.T) {
	if !users.RoleAdmin.IsStaff() || !users.RoleModerator.IsStaff() {
		t.Error("admin and moderator must be staff")
	}
	if users.RoleUser.IsStaff() {
		t.Error("a normal user must not be staff")
	}
	if users.Role("writer").Valid() {
		t.Error(`"writer" must not be a role - it is a capability (docs/10 §52)`)
	}

	// docs/10 §17: an unverified account may still sign in and read.
	if !users.StatusPendingVerification.CanAuthenticate() {
		t.Error("a pending-verification account must still be able to sign in")
	}
	for _, status := range []users.Status{
		users.StatusSuspended, users.StatusBanned, users.StatusDeleted,
	} {
		if status.CanAuthenticate() {
			t.Errorf("status %q must not be able to authenticate", status)
		}
	}
}
