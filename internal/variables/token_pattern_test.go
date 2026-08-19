package variables

import (
	"regexp"
	"testing"
)

// The scanner's whole vocabulary: single key / single key (settings review
// round 4). The pattern runs inside PostgreSQL, but its syntax is common to
// POSIX and Go, so this test pins the rule where CI sees it.
func TestTokenPattern_SingleKeyOnly(t *testing.T) {
	pattern := regexp.MustCompile(tokenPattern)

	matches := []string{"(y/n)", "(l/n)", "(e/c)", "(p/n)", "(n/a)"}
	for _, token := range matches {
		if !pattern.MatchString(token) {
			t.Errorf("%q must be scanned - it is the genre's own convention", token)
		}
	}

	// Words are prose. The old 12/18-character allowance turned a character's
	// two spellings into a nagging false positive.
	prose := []string{
		"(Scaramouche/Wanderer)",
		"(ab/cd)",
		"(yes/no)",
		"(ไป/มา)",
		"(y/n้ๆ)",
		"(/)",
		"(y/)",
		"(//)",
	}
	for _, text := range prose {
		if pattern.MatchString(text) {
			t.Errorf("%q must NOT be scanned - a key is one character, anything longer is prose", text)
		}
	}
}
