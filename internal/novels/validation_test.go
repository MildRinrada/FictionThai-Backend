package novels_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/novels"
)

// docs/11 §16, §17: the stored content model is plain text, so anything that
// could become executable markup or disguise text is refused at the boundary.
func TestSafeText(t *testing.T) {
	accepted := map[string]string{
		"Thai prose":           "ฝนหยุดตกแล้ว เธอเดินออกไป",
		"newlines and tabs":    "Paragraph one.\n\nParagraph two.\tIndented.",
		"emoji":                "She smiled 😊",
		"punctuation and math": `"Quotes", <angles>, & ampersands - 100% ≥ 50%`,
		"combining marks":      "égalité",
	}
	for name, value := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			if !novels.SafeText(value) {
				t.Errorf("SafeText(%q) = false; legitimate manuscript text was rejected", value)
			}
		})
	}

	// Note that `<script>` is ACCEPTED as text: it is stored verbatim and never
	// rendered as markup, so escaping it here would corrupt a fiction that
	// legitimately discusses HTML. See docs/CONTENT-MODEL.md §3.
	if !novels.SafeText("<script>alert(1)</script>") {
		t.Error("literal angle brackets are ordinary text and must be storable")
	}

	rejected := map[string]string{
		// PostgreSQL rejects a NUL in TEXT too; catching it here turns a
		// driver-level 500 into a clean 422.
		"NUL byte":        "before\x00after",
		"bell character":  "ding\a",
		"escape sequence": "\x1b[31mred\x1b[0m",
		// Cf characters can hide or visually reverse text, which is how a title
		// is made to read differently from what is actually stored.
		"zero-width joiner": "invis\u200dible",
		"bidi override":     "safe\u202etxet desrever",
		"private use area":  "gl\ue000yph",
		"C1 control":        "text\u0085more",
	}
	for name, value := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			if novels.SafeText(value) {
				t.Errorf("SafeText(%q) = true; this is not plain manuscript text", value)
			}
		})
	}
}

func TestParseRef_AcceptsUUIDsAndSlugs(t *testing.T) {
	id := uuid.New()

	ref, err := novels.ParseRef(id.String())
	if err != nil {
		t.Fatalf("ParseRef(uuid): %v", err)
	}
	if ref.BySlug() || ref.ID != id {
		t.Errorf("a UUID should resolve to an id reference, got %+v", ref)
	}

	ref, err = novels.ParseRef("my-first-fiction")
	if err != nil {
		t.Fatalf("ParseRef(slug): %v", err)
	}
	if !ref.BySlug() || ref.Slug != "my-first-fiction" {
		t.Errorf("a slug should resolve to a slug reference, got %+v", ref)
	}
}

// A malformed reference must be indistinguishable from a missing one: telling a
// caller that an identifier is well-formed but absent is information worth
// denying to anyone probing for content (docs/11 §3.4).
func TestParseRef_RejectsHostileReferences(t *testing.T) {
	hostile := []string{
		"",
		"   ",
		"../../etc/passwd",
		"' OR 1=1 --",
		"slug/with/slashes",
		"Has Spaces",
		"%2e%2e%2f",
		"<script>",
		strings.Repeat("a", 500),
	}

	for _, raw := range hostile {
		t.Run(raw, func(t *testing.T) {
			if _, err := novels.ParseRef(raw); err == nil {
				t.Errorf("ParseRef(%q) was accepted; it must be rejected as not found", raw)
			}
		})
	}
}

func TestParseRef_TrimsSurroundingWhitespace(t *testing.T) {
	ref, err := novels.ParseRef("  my-fiction  ")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if ref.Slug != "my-fiction" {
		t.Errorf("slug = %q, want the trimmed form", ref.Slug)
	}
}
