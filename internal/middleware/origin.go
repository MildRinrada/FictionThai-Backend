package middleware

import (
	"net/url"
	"strings"
)

// normalizeOrigin reduces an origin to scheme://host[:port] with no trailing
// slash, so that "https://example.com/" and "https://example.com" compare equal.
func normalizeOrigin(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// originOfReferer extracts the origin from a Referer header.
//
// Referer is only a fallback for Origin. It is less reliable - a referrer
// policy can strip or shorten it - which is why the CSRF check treats a missing
// value as a failure rather than a pass.
func originOfReferer(referer string) string {
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
