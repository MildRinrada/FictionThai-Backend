package slug_test

import (
	"strings"
	"testing"

	"github.com/fictionthai/fictionthai/backend/pkg/slug"
)

// The 2026-08 address review (docs/SLUGS.md): a NEW address is a bare random
// token, so a rename can never leave a URL asserting a stale title.
func TestNewToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		token, err := slug.NewToken()
		if err != nil {
			t.Fatalf("NewToken() error: %v", err)
		}
		if !slug.IsToken(token) {
			t.Fatalf("NewToken() = %q, which IsToken rejects", token)
		}
		if !slug.Valid(token) {
			t.Fatalf("NewToken() = %q, which the validator rejects", token)
		}
		if seen[token] {
			t.Fatalf("NewToken() repeated %q within 50 draws", token)
		}
		seen[token] = true
	}
}

func TestIsToken_RejectsLegacyShapes(t *testing.T) {
	for _, legacy := range []string{
		"",
		"test-headcanon",                 // title-based chapter address
		"one-shot",                       // short title-based address
		"785834039-kgb6hd",               // title + 6-char handle
		"b7k2m9",                         // handle alone: too short
		"aeiou234",                       // right length, vowels not in alphabet
		"b7k2m9x4p",                      // one character too long
		"genshin-impact-x-reader-b7k2m9", // full legacy fiction address
	} {
		if slug.IsToken(legacy) {
			t.Errorf("IsToken(%q) = true, want false", legacy)
		}
	}
}

func TestMake(t *testing.T) {
	tests := map[string]struct{ title, want string }{
		"lower-cases and hyphenates":  {"My First Fiction", "my-first-fiction"},
		"collapses punctuation runs":  {"Hello --- World!!!", "hello-world"},
		"trims leading and trailing":  {"  ...Spaced...  ", "spaced"},
		"keeps digits":                {"Chapter 42", "chapter-42"},
		"drops emoji":                 {"Sunrise 🌅 Story", "sunrise-story"},
		"yields nothing from symbols": {"!!! ???", ""},

		// A slug is ASCII. A Thai slug looks fine in the address bar and
		// becomes a wall of %E0%B8 the moment anyone pastes it into a chat;
		// romanising it instead produced addresses the author could not
		// recognise as their own title. So Thai is dropped, the ASCII words in
		// the title carry the address, and a title with no ASCII in it falls
		// back to a short generated one (see TestCandidate).
		"drops Thai":            {"นิยายของฉัน", ""},
		"keeps the ASCII words": {"หนุ่ม ๆ Genshin Impact x Reader", "genshin-impact-x-reader"},
		"mixes Thai and ASCII":  {"บทที่ 1 Prologue", "1-prologue"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := slug.Make(tc.title); got != tc.want {
				t.Errorf("Make(%q) = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}

// A generated slug must always satisfy the validator, or a fiction could be
// created at a URL its own routing then rejects.
func TestMake_AlwaysProducesAValidSlug(t *testing.T) {
	titles := []string{
		"My First Fiction", "นิยายของฉัน", "Chapter 42",
		strings.Repeat("very long title ", 40),
		"บทที่ 1 Prologue", "a",
	}

	for _, title := range titles {
		got := slug.Make(title)
		if got == "" {
			continue // the caller substitutes Fallback; see TestCandidate
		}
		if !slug.Valid(got) {
			t.Errorf("Make(%q) produced %q, which Valid rejects", title, got)
		}
	}
}

func TestMake_TruncatesToTheColumnLimit(t *testing.T) {
	got := slug.Make(strings.Repeat("word ", 200))

	if runes := len([]rune(got)); runes > slug.MaxLength {
		t.Errorf("slug is %d runes, want at most %d", runes, slug.MaxLength)
	}
	if strings.HasSuffix(got, "-") {
		t.Error("truncation left a trailing hyphen")
	}
}

func TestValid(t *testing.T) {
	valid := []string{"my-fiction", "chapter-1", "นิยาย", "a", "123"}
	for _, s := range valid {
		if !slug.Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}

	// These are the shapes a hostile path parameter would take. Rejecting them
	// by shape means they never reach a query at all.
	invalid := []string{
		"", "Has Spaces", "UPPER", "semi;colon", "quo'te",
		"../etc/passwd", "slash/es", "percent%20", "null\x00byte",
		strings.Repeat("a", slug.MaxLength+1),
	}
	for _, s := range invalid {
		if slug.Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

func TestCandidate_FirstAttemptIsTheCleanSlug(t *testing.T) {
	got, err := slug.Candidate("my-fiction", 0)
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if got != "my-fiction" {
		t.Errorf("attempt 0 = %q, want the unmodified slug", got)
	}
}

func TestCandidate_LaterAttemptsAddDistinctSuffixes(t *testing.T) {
	seen := map[string]bool{}

	for attempt := 1; attempt <= 20; attempt++ {
		got, err := slug.Candidate("my-fiction", attempt)
		if err != nil {
			t.Fatalf("Candidate: %v", err)
		}
		if !strings.HasPrefix(got, "my-fiction-") {
			t.Errorf("attempt %d = %q, want the base slug plus a suffix", attempt, got)
		}
		if !slug.Valid(got) {
			t.Errorf("attempt %d produced %q, which Valid rejects", attempt, got)
		}
		seen[got] = true
	}

	// Randomness matters: a counter would let anyone enumerate how many
	// fictions share a title, including private drafts.
	if len(seen) < 15 {
		t.Errorf("only %d distinct suffixes in 20 attempts; they look predictable", len(seen))
	}
}

func TestCandidate_SubstitutesTheFallbackForAnEmptyBase(t *testing.T) {
	got, err := slug.Candidate("", 0)
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if got != slug.Fallback {
		t.Errorf("Candidate(\"\", 0) = %q, want %q", got, slug.Fallback)
	}
}

// A long base plus a suffix must still fit the column.
func TestCandidate_KeepsSuffixedSlugsWithinTheLimit(t *testing.T) {
	base := slug.Make(strings.Repeat("word ", 200))

	got, err := slug.Candidate(base, 1)
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if runes := len([]rune(got)); runes > slug.MaxLength {
		t.Errorf("suffixed slug is %d runes, want at most %d", runes, slug.MaxLength)
	}
	if !slug.Valid(got) {
		t.Errorf("suffixed slug %q is not valid", got)
	}
}

// Every fiction address ends with its own handle, so two writers who choose the
// same title get addresses that differ in the last six characters instead of
// one of them getting a bare slug and the other a decorated one.
func TestWithPublicID(t *testing.T) {
	tests := map[string]struct{ title, id, want string }{
		"appends the handle": {
			"Genshin Impact x Reader", "b7k2m9", "genshin-impact-x-reader-b7k2m9",
		},
		"same title, different address": {
			"test", "x4n2pq", "test-x4n2pq",
		},
		"a title with no ASCII becomes the handle alone": {
			"นิยายของฉัน", "m3kd7f", "fiction-m3kd7f",
		},
		"drops the Thai and keeps the rest": {
			"หนุ่ม ๆ Genshin Impact", "q8w2rt", "genshin-impact-q8w2rt",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := slug.WithPublicID(tc.title, tc.id); got != tc.want {
				t.Errorf("WithPublicID(%q, %q) = %q, want %q", tc.title, tc.id, got, tc.want)
			}
		})
	}
}

func TestWithPublicID_StaysWithinTheColumn(t *testing.T) {
	got := slug.WithPublicID(strings.Repeat("word ", 200), "b7k2m9")

	if runes := len([]rune(got)); runes > slug.MaxLength {
		t.Errorf("slug is %d runes, want at most %d", runes, slug.MaxLength)
	}
	if !strings.HasSuffix(got, "-b7k2m9") {
		t.Errorf("truncation lost the handle: %q", got)
	}
	if !slug.Valid(got) {
		t.Errorf("%q is not a valid slug", got)
	}
}

// A handle must not spell anything, and must not repeat.
func TestNewPublicID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id, err := slug.NewPublicID()
		if err != nil {
			t.Fatalf("NewPublicID: %v", err)
		}
		if len([]rune(id)) != slug.PublicIDLength {
			t.Fatalf("handle %q is not %d characters", id, slug.PublicIDLength)
		}
		if strings.ContainsAny(id, "aeiou") {
			t.Errorf("handle %q contains a vowel; it could spell a word", id)
		}
		if !slug.Valid(id) {
			t.Errorf("handle %q is not a valid slug on its own", id)
		}
		seen[id] = true
	}
	if len(seen) < 45 {
		t.Errorf("only %d distinct handles in 50 draws; they look predictable", len(seen))
	}
}
