package authors

import (
	"strings"
	"testing"
)

func ptr(s string) *string { return &s }

func TestValidateDonationURL_Accepts(t *testing.T) {
	clean, err := validateDonationURL(ptr("https://easydonate.example/somewriter"))
	if err != nil {
		t.Fatalf("expected a valid https URL to pass, got %v", err)
	}
	if clean == nil || *clean != "https://easydonate.example/somewriter" {
		t.Errorf("clean = %v", clean)
	}
	// Surrounding whitespace is trimmed.
	clean, err = validateDonationURL(ptr("  https://ko-fi.example/writer  "))
	if err != nil || clean == nil || *clean != "https://ko-fi.example/writer" {
		t.Errorf("trimmed URL not accepted: %v / %v", clean, err)
	}
}

func TestValidateDonationURL_Clears(t *testing.T) {
	// nil, empty, and whitespace-only all CLEAR the link (nullable field).
	for _, in := range []*string{nil, ptr(""), ptr("   ")} {
		clean, err := validateDonationURL(in)
		if err != nil {
			t.Errorf("clearing value %v should not error: %v", in, err)
		}
		if clean != nil {
			t.Errorf("clearing value %v should yield nil, got %q", in, *clean)
		}
	}
}

func TestValidateDonationURL_Rejects(t *testing.T) {
	bad := []string{
		"http://insecure.example", // http rejected - https only
		"javascript:alert(1)",     // scheme injection
		"data:text/html,<script>", // data URL
		"file:///etc/passwd",      // file scheme
		"ftp://host/x",            // ftp scheme
		"https://",                // no host
		"not a url at all",        // no scheme/host
	}
	for _, in := range bad {
		if _, err := validateDonationURL(ptr(in)); err == nil {
			t.Errorf("expected %q to be rejected", in)
		}
	}
	// Over-long URL rejected.
	long := "https://x.example/" + strings.Repeat("a", donationURLMaxLength)
	if _, err := validateDonationURL(ptr(long)); err == nil {
		t.Error("expected an over-long URL to be rejected")
	}
}
