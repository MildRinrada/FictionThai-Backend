package integration

import (
	"net/http"
	"testing"
)

// authorProfileView decodes GET/PUT /me/author-profile.
type authorProfileView struct {
	DonationURL *string `json:"donation_url"`
}

// novelAuthorView decodes just the author block of a fiction, for the donation
// CTA data a reader receives.
type novelAuthorView struct {
	Author struct {
		Username    string  `json:"username"`
		DonationURL *string `json:"donation_url"`
	} `json:"author"`
}

func (e *authEnv) setDonationURL(t *testing.T, w writer, body any) apiResponse {
	t.Helper()
	return e.asOwner(t, w, http.MethodPut, "/api/v1/me/author-profile", body)
}

func (e *authEnv) countRows(t *testing.T, query, userID string) int {
	t.Helper()
	var n int
	if err := e.db.DB.QueryRow(query, userID).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func TestAuthors_DonationURLLifecycleAndValidation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	// A guest cannot set a donation URL.
	if g := env.asGuest(t, http.MethodPut, "/api/v1/me/author-profile"); g.status != http.StatusUnauthorized {
		t.Fatalf("guest set status = %d, want 401", g.status)
	}

	// A valid https URL is accepted and read back.
	const url = "https://easydonate.example/my-writer"
	if res := env.setDonationURL(t, w, map[string]any{"donation_url": url}); res.status != http.StatusOK {
		t.Fatalf("set https status = %d, want 200. body: %s", res.status, res.body)
	}
	got := dataOf[authorProfileView](t, env.asOwner(t, w, http.MethodGet, "/api/v1/me/author-profile"))
	if got.DonationURL == nil || *got.DonationURL != url {
		t.Fatalf("read-back donation url = %v, want %q", got.DonationURL, url)
	}

	// Unsafe / non-https schemes are rejected with 422.
	for _, bad := range []string{"http://insecure.example", "javascript:alert(1)", "data:text/html,x", "file:///etc/passwd"} {
		if res := env.setDonationURL(t, w, map[string]any{"donation_url": bad}); res.status != http.StatusUnprocessableEntity {
			t.Fatalf("set %q status = %d, want 422", bad, res.status)
		}
	}
	// The rejected updates did not overwrite the good value.
	got = dataOf[authorProfileView](t, env.asOwner(t, w, http.MethodGet, "/api/v1/me/author-profile"))
	if got.DonationURL == nil || *got.DonationURL != url {
		t.Fatalf("donation url changed by a rejected update: %v", got.DonationURL)
	}

	// Clearing (null) is supported - the field is nullable.
	if res := env.setDonationURL(t, w, map[string]any{"donation_url": nil}); res.status != http.StatusOK {
		t.Fatalf("clear status = %d, want 200", res.status)
	}
	cleared := dataOf[authorProfileView](t, env.asOwner(t, w, http.MethodGet, "/api/v1/me/author-profile"))
	if cleared.DonationURL != nil {
		t.Fatalf("donation url after clear = %v, want nil", cleared.DonationURL)
	}
}

func TestAuthors_DonationLinkVisibleToReaders(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	const url = "https://ko-fi.example/thai-writer"
	if res := env.setDonationURL(t, w, map[string]any{"donation_url": url}); res.status != http.StatusOK {
		t.Fatalf("set donation url status = %d", res.status)
	}

	novel := env.publishedNovel(t, w, nil)

	// 13V: money is opt-in PER FICTION. A new fiction starts with the donate
	// switch off, so the link is withheld until the writer turns it on.
	before := dataOf[novelAuthorView](t, env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug))
	if before.Author.DonationURL != nil {
		t.Fatalf("donation url shown before the writer opted in: %v", before.Author.DonationURL)
	}
	if res := env.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"show_donate": true}); res.status != http.StatusOK {
		t.Fatalf("enable donate = %d. body: %s", res.status, res.body)
	}

	// A guest reading the fiction now receives the author's external donation
	// link, which the frontend renders as a "support this writer" CTA.
	res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug)
	if res.status != http.StatusOK {
		t.Fatalf("read novel status = %d. body: %s", res.status, res.body)
	}
	view := dataOf[novelAuthorView](t, res)
	if view.Author.DonationURL == nil || *view.Author.DonationURL != url {
		t.Fatalf("novel author donation url = %v, want %q", view.Author.DonationURL, url)
	}

	// A writer who has set NO link exposes none (omitted).
	other := env.newWriter(t)
	otherNovel := env.publishedNovel(t, other, nil)
	otherView := dataOf[novelAuthorView](t, env.asGuest(t, http.MethodGet, "/api/v1/novels/"+otherNovel.Slug))
	if otherView.Author.DonationURL != nil {
		t.Fatalf("author with no link exposed %v", otherView.Author.DonationURL)
	}
}

// TestAuthors_DonationSeparationCreatesNoPlatformPayment is the core business
// rule (addendum §27): configuring an external EasyDonate link and a reader
// following it creates NO FictionThai payment, subscription, or any platform
// financial record. FictionThai only stores and displays the URL.
func TestAuthors_DonationSeparationCreatesNoPlatformPayment(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)
	reader := env.registerWeb(t)

	if res := env.setDonationURL(t, w, map[string]any{"donation_url": "https://easydonate.example/sep"}); res.status != http.StatusOK {
		t.Fatalf("set donation url status = %d", res.status)
	}
	novel := env.publishedNovel(t, w, nil)

	// The reader reads the fiction and would click the external CTA (which leaves
	// FictionThai entirely - nothing is posted back here).
	if res := env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug); res.status != http.StatusOK {
		t.Fatalf("reader read status = %d", res.status)
	}

	// Neither the writer nor the reader gained any platform financial row.
	for _, id := range []string{w.userID, reader.userID} {
		if n := env.countRows(t, `SELECT count(*) FROM subscriptions WHERE user_id = $1`, id); n != 0 {
			t.Errorf("user %s has %d subscriptions from a DONATION flow, want 0", id, n)
		}
		if n := env.countRows(t, `SELECT count(*) FROM subscription_payments WHERE user_id = $1`, id); n != 0 {
			t.Errorf("user %s has %d payments from a DONATION flow, want 0", id, n)
		}
	}
}
