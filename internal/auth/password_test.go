package auth_test

import (
	"strings"
	"testing"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
)

// testParams keep the tests fast. Production uses DefaultPasswordParams
// (64 MiB), which is deliberately expensive and would make the suite crawl.
func testParams() auth.PasswordParams {
	p := auth.DefaultPasswordParams()
	p.Memory = 8 * 1024
	p.Iterations = 1
	return p
}

func TestHashPassword_VerifiesCorrectPassword(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := auth.HashPassword(password, testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	ok, err := auth.VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if !ok {
		t.Error("the correct password did not verify")
	}
}

func TestHashPassword_RejectsWrongPassword(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery staple", testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	for _, wrong := range []string{
		"correct horse battery stapl",  // one character short
		"Correct horse battery staple", // case differs
		"",
		"totally different",
	} {
		ok, err := auth.VerifyPassword(wrong, hash)
		if err != nil {
			t.Fatalf("VerifyPassword(%q) error = %v", wrong, err)
		}
		if ok {
			t.Errorf("password %q should not have verified", wrong)
		}
	}
}

// A per-password salt is what stops an attacker who steals the database from
// spotting that two users share a password.
func TestHashPassword_UsesUniqueSaltPerCall(t *testing.T) {
	const password = "the same password twice"

	first, err := auth.HashPassword(password, testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	second, err := auth.HashPassword(password, testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	if first == second {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}

	// Both must still verify.
	for i, hash := range []string{first, second} {
		ok, err := auth.VerifyPassword(password, hash)
		if err != nil || !ok {
			t.Errorf("hash %d did not verify (ok=%v err=%v)", i, ok, err)
		}
	}
}

// The encoded hash must never contain the password itself.
func TestHashPassword_DoesNotEmbedPassword(t *testing.T) {
	const password = "supersecretpassphrase"

	hash, err := auth.HashPassword(password, testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatal("the encoded hash contains the plaintext password")
	}
}

// The PHC string must carry its parameters, or the work factor could never be
// raised without invalidating every existing password.
func TestHashPassword_EncodesParameters(t *testing.T) {
	params := testParams()

	hash, err := auth.HashPassword("a password long enough", params)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash = %q, want an $argon2id$ prefix", hash)
	}
	if parts := strings.Split(hash, "$"); len(parts) != 6 {
		t.Errorf("hash has %d segments, want 6 (PHC format)", len(parts))
	}
	if !strings.Contains(hash, "m=8192,t=1,p=2") {
		t.Errorf("hash = %q, want the cost parameters embedded", hash)
	}
}

// A hash produced with different parameters must still verify - that is what
// makes raising the work factor a non-breaking change.
func TestVerifyPassword_AcceptsOtherParameters(t *testing.T) {
	const password = "migrating between parameters"

	weak := testParams()
	strong := weak
	strong.Iterations = 2

	weakHash, err := auth.HashPassword(password, weak)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	strongHash, err := auth.HashPassword(password, strong)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	for name, hash := range map[string]string{"weak": weakHash, "strong": strongHash} {
		ok, err := auth.VerifyPassword(password, hash)
		if err != nil {
			t.Errorf("%s: VerifyPassword() error = %v", name, err)
		}
		if !ok {
			t.Errorf("%s: password did not verify", name)
		}
	}
}

func TestVerifyPassword_RejectsMalformedHash(t *testing.T) {
	tests := map[string]string{
		"empty":            "",
		"not phc":          "not-a-hash",
		"wrong algorithm":  "$bcrypt$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"missing segments": "$argon2id$v=19$m=65536,t=3,p=2",
		"bad base64":       "$argon2id$v=19$m=65536,t=3,p=2$!!!$!!!",
	}

	for name, hash := range tests {
		t.Run(name, func(t *testing.T) {
			ok, err := auth.VerifyPassword("any password", hash)
			if err == nil {
				t.Error("expected an error for a malformed hash")
			}
			if ok {
				t.Error("a malformed hash must never verify")
			}
		})
	}
}

// A stored plain SHA-256 or MD5 digest must be rejected outright rather than
// silently treated as a password hash.
func TestVerifyPassword_RejectsLegacyDigests(t *testing.T) {
	legacy := []string{
		"5f4dcc3b5aa765d61d8327deb882cf99",                                 // MD5
		"5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8", // SHA-256
	}

	for _, hash := range legacy {
		if ok, _ := auth.VerifyPassword("password", hash); ok {
			t.Errorf("legacy digest %q was accepted", hash)
		}
	}
}

// BurnPasswordTime must be safe to call with anything - it exists to equalise
// response timing for unknown accounts (docs/10 §39).
func TestBurnPasswordTime_IsSafe(t *testing.T) {
	auth.BurnPasswordTime("")
	auth.BurnPasswordTime("anything at all")
}
