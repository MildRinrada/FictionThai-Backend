package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id password hashing - docs/10 §9, docs/11 §5.
//
// The encoded form is the standard PHC string, so the parameters travel with
// the hash. That is what makes it possible to raise the work factor later
// without invalidating existing passwords: an old hash still verifies with the
// parameters it was created with.
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>

// PasswordParams are the Argon2id cost parameters.
type PasswordParams struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultPasswordParams follows the OWASP Argon2id recommendation of 64 MiB
// with 3 iterations. docs/11 §5 requires "a sufficiently strong work factor
// appropriate for the production environment" - raise Memory on a larger
// instance rather than lowering it to make tests faster.
func DefaultPasswordParams() PasswordParams {
	return PasswordParams{
		Memory:      64 * 1024,
		Iterations:  3,
		Parallelism: 2,
		SaltLength:  16,
		KeyLength:   32,
	}
}

var (
	ErrInvalidHashFormat  = errors.New("password hash is not in the expected format")
	ErrUnsupportedVersion = errors.New("unsupported argon2 version")
)

// HashPassword derives an Argon2id hash with a fresh random salt.
func HashPassword(password string, params PasswordParams) (string, error) {
	salt := make([]byte, params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey(
		[]byte(password), salt,
		params.Iterations, params.Memory, params.Parallelism, params.KeyLength,
	)

	// The error value never contains the password - docs/11 §5 forbids it
	// appearing in logs or errors.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, params.Memory, params.Iterations, params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches encodedHash.
//
// Comparison is constant-time so that a timing difference cannot reveal how
// much of the hash matched.
func VerifyPassword(password, encodedHash string) (bool, error) {
	params, salt, want, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey(
		[]byte(password), salt,
		params.Iterations, params.Memory, params.Parallelism, uint32(len(want)),
	)

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func decodeHash(encoded string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return PasswordParams{}, nil, nil, ErrInvalidHashFormat
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidHashFormat
	}
	if version != argon2.Version {
		return PasswordParams{}, nil, nil, ErrUnsupportedVersion
	}

	var params PasswordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&params.Memory, &params.Iterations, &params.Parallelism); err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidHashFormat
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidHashFormat
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidHashFormat
	}

	params.SaltLength = uint32(len(salt))
	params.KeyLength = uint32(len(key))
	return params, salt, key, nil
}

// dummyHash is verified against when no user matches the supplied identifier.
//
// Without this, a login attempt for an unknown account would return noticeably
// faster than one for a real account with a wrong password, letting an attacker
// enumerate valid accounts by timing alone - which docs/10 §39 and docs/11 §27
// require preventing. The value is a real Argon2id hash of a random string, so
// verifying it costs the same as a genuine check.
var dummyHash string

func init() {
	// Computed once at startup with the default parameters.
	filler := make([]byte, 32)
	if _, err := rand.Read(filler); err != nil {
		panic("auth: cannot initialise dummy password hash: " + err.Error())
	}
	hash, err := HashPassword(base64.RawStdEncoding.EncodeToString(filler), DefaultPasswordParams())
	if err != nil {
		panic("auth: cannot initialise dummy password hash: " + err.Error())
	}
	dummyHash = hash
}

// BurnPasswordTime performs a throwaway verification so that a login for a
// non-existent account takes about as long as a real one.
func BurnPasswordTime(password string) {
	_, _ = VerifyPassword(password, dummyHash)
}
