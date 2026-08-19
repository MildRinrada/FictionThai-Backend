// Package authors owns the author_profiles WRITE surface (Phase 11).
//
// Repository fact (addendum §2): before Phase 11 the author_profiles table had
// NO application write path at all - it was only ever read, through
// users.HasAuthorProfile / the is_author flag. This package adds the first
// write, and it is deliberately narrow: the single thing Phase 11 needs is the
// EXTERNAL writer-support link (author_profiles.donation_url), which readers see
// as a "support this writer" CTA that opens the writer's own EasyDonate page.
// FictionThai never processes that money (brief §6, §15).
//
// Documented gap (addendum §4): there is still no broader author-registration /
// writer-onboarding workflow, and inventing one is out of scope. Setting a
// donation URL CREATES the author_profiles row on demand (an explicit user
// action on a writer-settings surface - not a silent side effect), which is also
// what flips is_author to true for that user. A full writer-capability flow
// remains a pre-existing gap, recorded in docs/MONETIZATION.md, not solved here.
package authors

import (
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// donationURLMaxLength matches the platform-wide URL limit used by novels and
// chapters (validateOptionalURL), so the contract is consistent.
const donationURLMaxLength = 2048

// Profile mirrors an author_profiles row.
type Profile struct {
	UserID      uuid.UUID
	PenName     *string
	AuthorBio   *string
	IsFeatured  bool
	DonationURL *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ProfileView is the API shape a writer sees for their own author profile. Phase
// 11 exposes only the donation URL; pen name / bio remain read-only elsewhere
// until an author-profile editor is actually specified.
type ProfileView struct {
	DonationURL *string `json:"donation_url,omitempty"`
}

// View renders a profile for the API.
func (p *Profile) View() ProfileView {
	return ProfileView{DonationURL: p.DonationURL}
}

// validateDonationURL enforces the donation-URL contract (brief §6): a nullable,
// absolute, HTTPS-only URL within the standard length limit. It returns a
// cleaned value (nil to CLEAR) and a validation error using the standard 422
// envelope. http, javascript, data, file, ftp, and anything without a host are
// rejected - a stricter scheme rule than the http-or-https validateOptionalURL,
// because a donation link is a destination readers are sent to.
func validateDonationURL(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	// An empty (or whitespace-only) string CLEARS the link - the field is nullable.
	raw := strings.TrimSpace(*value)
	if raw == "" {
		return nil, nil
	}

	if utf8.RuneCountInString(raw) > donationURLMaxLength {
		return nil, fieldError("The donation link is too long.")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return nil, fieldError("The donation link must be an absolute https URL.")
	}
	return &raw, nil
}

func fieldError(msg string) error {
	return apierror.Validation(map[string][]string{"donation_url": {msg}})
}
