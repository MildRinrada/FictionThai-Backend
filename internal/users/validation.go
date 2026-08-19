package users

import (
	_ "embed"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Username rules (docs/10 §7).
const (
	UsernameMinLength = 3
	UsernameMaxLength = 32
)

// usernamePattern restricts usernames to a URL-safe ASCII set.
//
// The platform is Thai-first, but the USERNAME is a handle that appears in
// /author/{username} and identifies an account. Allowing arbitrary Unicode
// there would open homograph impersonation - Cyrillic "а" rendering as Latin
// "a" - which docs/10 §7 explicitly requires preventing. Thai names belong in
// user_profiles.display_name, which is free-form and never used as an
// identifier or in a URL.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// reservedUsernames is loaded from a data file rather than being hard-coded.
//
// docs/10 §7: "The exact reserved-word list should be maintained separately
// from application code where practical." Keeping it in a text file means the
// list can be reviewed and extended without a code change.
//
//go:embed reserved_usernames.txt
var reservedUsernamesData string

var reservedUsernames = loadReservedUsernames(reservedUsernamesData)

func loadReservedUsernames(data string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[strings.ToLower(line)] = struct{}{}
	}
	return out
}

// NormalizeUsername produces the canonical form used for storage and lookup.
//
// The column is CITEXT so the database compares case-insensitively regardless,
// but normalising here keeps what we store predictable (docs/10 §7).
func NormalizeUsername(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// NormalizeEmail trims and lower-cases an address.
//
// Only the whole address is lower-cased; the local part is NOT otherwise
// rewritten. Stripping dots or +tags would be a guess about a provider's
// semantics and could merge two genuinely different accounts.
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// ValidateUsername reports why a username is unacceptable, or "" if it is fine.
func ValidateUsername(username string) string {
	switch {
	case username == "":
		return "Username is required."
	case utf8.RuneCountInString(username) < UsernameMinLength:
		return "Username must be at least 3 characters."
	case utf8.RuneCountInString(username) > UsernameMaxLength:
		return "Username must be at most 32 characters."
	case !usernamePattern.MatchString(username):
		return "Username may contain only letters, numbers, hyphens, and underscores."
	case IsReservedUsername(username):
		// docs/10 §7: prevent impersonation of system accounts.
		return "This username is not available."
	}
	return ""
}

// IsReservedUsername reports whether the name is reserved for the platform.
func IsReservedUsername(username string) bool {
	_, reserved := reservedUsernames[NormalizeUsername(username)]
	return reserved
}

// ValidateEmail reports why an address is unacceptable, or "" if it is fine.
func ValidateEmail(email string) string {
	if email == "" {
		return "Email is required."
	}
	if len(email) > 254 { // RFC 5321 maximum path length
		return "Email is too long."
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return "Email is not a valid address."
	}
	return ""
}

// Password rules (docs/10 §9).
//
// The guidance is explicit: prefer LONG and unique over forced character-class
// combinations, which push users toward predictable substitutions. So the only
// rule is length, plus an upper bound because Argon2id hashes the whole input
// and an unbounded password is a cheap way to burn CPU.
const (
	PasswordMinLength = 12
	PasswordMaxLength = 256
)

// ValidatePassword reports why a password is unacceptable, or "" if it is fine.
func ValidatePassword(password string) string {
	switch {
	case password == "":
		return "Password is required."
	case utf8.RuneCountInString(password) < PasswordMinLength:
		return "Password must be at least 12 characters."
	case utf8.RuneCountInString(password) > PasswordMaxLength:
		return "Password must be at most 256 characters."
	}
	return ""
}
