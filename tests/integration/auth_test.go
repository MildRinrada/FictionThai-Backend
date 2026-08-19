package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
)

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestRegister_CreatesAccountAndSatelliteRecords(t *testing.T) {
	env := newAuthEnv(t)
	session := env.registerWeb(t)

	// Every account must have a profile and preferences; a missing preferences
	// row would break the reader on first load.
	for _, table := range []string{"user_profiles", "user_preferences"} {
		var exists bool
		err := env.db.QueryRowContext(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM "+table+" WHERE user_id = $1)", session.userID).Scan(&exists)
		if err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("registration did not create a %s row", table)
		}
	}

	// docs/08 §6.3: a user does not become a writer immediately.
	var isAuthor bool
	if err := env.db.QueryRowContext(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM author_profiles WHERE user_id = $1)", session.userID).Scan(&isAuthor); err != nil {
		t.Fatalf("check author_profiles: %v", err)
	}
	if isAuthor {
		t.Error("registration created an author profile; writer capability is opt-in")
	}
}

// The database must store only a hash - never the password, never the token.
func TestRegister_StoresOnlyHashedSecrets(t *testing.T) {
	env := newAuthEnv(t)
	session := env.registerWeb(t)

	var passwordHash string
	if err := env.db.QueryRowContext(context.Background(),
		"SELECT password_hash FROM users WHERE id = $1", session.userID).Scan(&passwordHash); err != nil {
		t.Fatalf("read password hash: %v", err)
	}

	if strings.Contains(passwordHash, session.password) {
		t.Fatal("the stored password hash contains the plaintext password")
	}
	if !strings.HasPrefix(passwordHash, "$argon2id$") {
		t.Errorf("password hash = %q, want an Argon2id hash", passwordHash)
	}

	// The session cookie carries the raw token; the database must hold only its
	// digest (docs/08 §29).
	var storedHash string
	if err := env.db.QueryRowContext(context.Background(),
		"SELECT token_hash FROM user_sessions WHERE user_id = $1", session.userID).Scan(&storedHash); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if storedHash == session.sessionCookie.Value {
		t.Fatal("the raw session token is stored in the database")
	}
	if storedHash != auth.HashToken(session.sessionCookie.Value) {
		t.Error("the stored digest does not match the issued token")
	}
}

func TestRegister_WebDoesNotReturnTokenInBody(t *testing.T) {
	env := newAuthEnv(t)

	username := uniqueName(t, "webonly")
	res := env.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/auth/register",
		body: map[string]string{
			"username": username,
			"email":    username + "@example.com",
			"password": "correct horse battery staple",
			"client":   "web",
		},
	})

	// docs/09 §4: a browser must never be handed a credential it could store in
	// JavaScript.
	if strings.Contains(string(res.body), `"token"`) {
		t.Errorf("web registration returned a token in the body: %s", res.body)
	}
	if cookie := res.cookie(env.cookies.SessionCookieName()); cookie == nil {
		t.Error("web registration must set a session cookie")
	} else if !cookie.HttpOnly {
		t.Error("the session cookie must be HttpOnly")
	}
}

func TestRegister_NativeReturnsTokenAndSetsNoCookie(t *testing.T) {
	env := newAuthEnv(t)

	token, _, _, _ := env.registerNative(t)
	if token == "" {
		t.Fatal("native registration returned no token")
	}

	// A native client has no cookie jar; setting one would be meaningless and
	// could leak into an embedded webview.
	res := env.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/auth/register",
		body: map[string]string{
			"username": uniqueName(t, "native"),
			"email":    uniqueName(t, "native") + "@example.com",
			"password": "correct horse battery staple",
			"client":   "native",
		},
	})
	if cookie := res.cookie(env.cookies.SessionCookieName()); cookie != nil {
		t.Error("native registration must not set a session cookie")
	}
}

func TestRegister_RejectsWeakOrInvalidInput(t *testing.T) {
	env := newAuthEnv(t)

	tests := map[string]map[string]string{
		"short password":    {"username": uniqueName(t, "a"), "email": "a@example.com", "password": "short"},
		"invalid username":  {"username": "no spaces here", "email": "b@example.com", "password": "correct horse battery staple"},
		"reserved username": {"username": "admin", "email": "c@example.com", "password": "correct horse battery staple"},
		"invalid email":     {"username": uniqueName(t, "d"), "email": "not-an-email", "password": "correct horse battery staple"},
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			res := env.do(t, apiRequest{
				method: http.MethodPost, path: "/api/v1/auth/register", body: body,
			})
			if res.status != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422. body: %s", res.status, res.body)
			}
		})
	}
}

// docs/11 §27: registration must not become an account-existence oracle.
func TestRegister_DuplicateEmailDoesNotConfirmExistence(t *testing.T) {
	env := newAuthEnv(t)
	existing := env.registerWeb(t)

	res := env.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/auth/register",
		body: map[string]string{
			"username": uniqueName(t, "other"),
			"email":    existing.email, // already registered
			"password": "correct horse battery staple",
		},
	})

	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", res.status, res.body)
	}
	body := strings.ToLower(string(res.body))
	for _, leak := range []string{"already registered", "already exists", "taken", existing.email} {
		if strings.Contains(body, strings.ToLower(leak)) {
			t.Errorf("response reveals that the email is registered (%q): %s", leak, res.body)
		}
	}
}

func TestRegister_DuplicateUsernameIsReported(t *testing.T) {
	env := newAuthEnv(t)
	existing := env.registerWeb(t)

	// A username IS public - /author/{username} already discloses it - so
	// naming the conflict leaks nothing and saves the user a guess.
	res := env.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/auth/register",
		body: map[string]string{
			"username": existing.username,
			"email":    uniqueName(t, "fresh") + "@example.com",
			"password": "correct horse battery staple",
		},
	})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422. body: %s", res.status, res.body)
	}
}

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

func TestLogin_AcceptsUsernameOrEmail(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	for name, identifier := range map[string]string{
		"username":            account.username,
		"email":               account.email,
		"username other case": strings.ToUpper(account.username),
		"email other case":    strings.ToUpper(account.email),
	} {
		t.Run(name, func(t *testing.T) {
			res := env.do(t, apiRequest{
				method: http.MethodPost,
				path:   "/api/v1/auth/login",
				body: map[string]string{
					"identifier": identifier,
					"password":   account.password,
					"client":     "web",
				},
			})
			if res.status != http.StatusOK {
				t.Errorf("status = %d, want 200. body: %s", res.status, res.body)
			}
			if res.cookie(env.cookies.SessionCookieName()) == nil {
				t.Error("login did not set a session cookie")
			}
		})
	}
}

// docs/10 §10, §39: the response must be identical whether the account exists.
func TestLogin_FailuresAreIndistinguishable(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	unknown := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": "no-such-account-here", "password": "correct horse battery staple"},
	})
	wrongPassword := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": "definitely not the password"},
	})

	if unknown.status != http.StatusUnauthorized || wrongPassword.status != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want both 401", unknown.status, wrongPassword.status)
	}
	if string(unknown.body) != string(wrongPassword.body) {
		t.Errorf("responses differ, which reveals whether the account exists:\n unknown: %s\n wrong password: %s",
			unknown.body, wrongPassword.body)
	}
}

func TestLogin_CreatesANewSessionEachTime(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	first := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": account.password},
	})
	second := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": account.password},
	})

	firstToken := first.cookie(env.cookies.SessionCookieName()).Value
	secondToken := second.cookie(env.cookies.SessionCookieName()).Value

	// A fresh session per login is what prevents session fixation
	// (docs/11 §6): an identifier known before authentication is never adopted.
	if firstToken == secondToken {
		t.Fatal("two logins produced the same session token")
	}

	// Signing in on a second device must not sign the first one out.
	me := env.do(t, apiRequest{
		method:  http.MethodGet,
		path:    "/api/v1/auth/me",
		cookies: []*http.Cookie{{Name: env.cookies.SessionCookieName(), Value: firstToken}},
	})
	if me.status != http.StatusOK {
		t.Errorf("the first session stopped working after a second login (status %d)", me.status)
	}
}

func TestLogin_RejectsUnknownClientKind(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{
			"identifier": account.username,
			"password":   account.password,
			"client":     "desktop",
		},
	})
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 for an unknown client kind", res.status)
	}
}

// ---------------------------------------------------------------------------
// Session validation
// ---------------------------------------------------------------------------

func TestMe_RequiresAuthentication(t *testing.T) {
	env := newAuthEnv(t)

	res := env.do(t, apiRequest{method: http.MethodGet, path: "/api/v1/auth/me"})
	if res.status != http.StatusUnauthorized {
		t.Errorf("guest status = %d, want 401 (docs/09 §12)", res.status)
	}
}

func TestMe_ReturnsPrivateViewWithoutSecrets(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	res := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", res.status, res.body)
	}

	var payload struct {
		Data struct {
			Username      string `json:"username"`
			Email         string `json:"email"`
			Role          string `json:"role"`
			EmailVerified bool   `json:"email_verified"`
			IsAuthor      bool   `json:"is_author"`
		} `json:"data"`
	}
	res.json(t, &payload)

	if payload.Data.Username != account.username {
		t.Errorf("username = %q, want %q", payload.Data.Username, account.username)
	}
	// A user may see their OWN email (docs/10 §29).
	if payload.Data.Email != account.email {
		t.Errorf("email = %q, want %q", payload.Data.Email, account.email)
	}
	if payload.Data.EmailVerified {
		t.Error("a newly registered account must not be marked verified")
	}
	if payload.Data.IsAuthor {
		t.Error("a newly registered account is not yet an author")
	}

	// No credential may ever appear in a response (docs/10 §9).
	for _, forbidden := range []string{"password_hash", "password", "token_hash"} {
		if strings.Contains(string(res.body), forbidden) {
			t.Errorf("response contains %q: %s", forbidden, res.body)
		}
	}
}

func TestSession_BearerTokenAuthenticates(t *testing.T) {
	env := newAuthEnv(t)
	token, username, _, _ := env.registerNative(t)

	res := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", bearer: token,
	})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", res.status, res.body)
	}

	var payload struct {
		Data struct{ Username string } `json:"data"`
	}
	res.json(t, &payload)
	if payload.Data.Username != username {
		t.Errorf("username = %q, want %q", payload.Data.Username, username)
	}
}

func TestSession_RejectsForgedAndMalformedTokens(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	forged := []string{
		"not-a-real-token",
		"",
		strings.Repeat("A", 43), // right shape, wrong value
		auth.HashToken(account.sessionCookie.Value), // the DIGEST is not a credential
	}

	for _, token := range forged {
		res := env.do(t, apiRequest{
			method:  http.MethodGet,
			path:    "/api/v1/auth/me",
			cookies: []*http.Cookie{{Name: env.cookies.SessionCookieName(), Value: token}},
		})
		if res.status != http.StatusUnauthorized {
			t.Errorf("token %q gave status %d, want 401", token, res.status)
		}
	}
}

func TestSession_ExpiredSessionIsRejected(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	// Expire it directly; waiting 14 days is not an option.
	if _, err := env.db.ExecContext(context.Background(),
		"UPDATE user_sessions SET expires_at = now() - interval '1 minute' WHERE user_id = $1",
		account.userID); err != nil {
		t.Fatalf("expire session: %v", err)
	}

	res := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	})
	if res.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for an expired session", res.status)
	}
}

func TestSession_RevokedSessionIsRejected(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	if _, err := env.db.ExecContext(context.Background(),
		"UPDATE user_sessions SET revoked_at = now() WHERE user_id = $1", account.userID); err != nil {
		t.Fatalf("revoke session: %v", err)
	}

	res := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	})
	if res.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a revoked session", res.status)
	}
}

// The O(1) bulk-invalidation mechanism (docs/10 §37).
func TestSession_InvalidatedBeforeCutoffIsRejected(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	// The session row itself stays untouched - only the user's cutoff moves.
	if _, err := env.db.ExecContext(context.Background(),
		"UPDATE users SET sessions_invalidated_before = now() WHERE id = $1", account.userID); err != nil {
		t.Fatalf("set cutoff: %v", err)
	}

	var revokedAt *time.Time
	if err := env.db.QueryRowContext(context.Background(),
		"SELECT revoked_at FROM user_sessions WHERE user_id = $1", account.userID).Scan(&revokedAt); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if revokedAt != nil {
		t.Fatal("the session row was revoked; this test must exercise the cutoff alone")
	}

	res := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	})
	if res.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 - the cutoff must invalidate the session without touching its row", res.status)
	}
}

func TestSession_SuspendedAccountLosesAccess(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	if _, err := env.db.ExecContext(context.Background(),
		"UPDATE users SET status = 'suspended' WHERE id = $1", account.userID); err != nil {
		t.Fatalf("suspend account: %v", err)
	}

	// An existing session must stop working immediately, not at expiry.
	res := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	})
	if res.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a suspended account", res.status)
	}

	// And they must not be able to sign in again.
	login := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": account.password},
	})
	if login.status != http.StatusForbidden {
		t.Errorf("login status = %d, want 403 for a suspended account. body: %s", login.status, login.body)
	}
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

func TestLogout_EndsOnlyTheCurrentSession(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	// A second device.
	second := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": account.password},
	})
	secondCookie := second.cookie(env.cookies.SessionCookieName())

	res := env.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/auth/logout",
		cookies: account.authCookies(),
		csrf:    account.csrfToken,
	})
	if res.status != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204. body: %s", res.status, res.body)
	}

	// This session is gone.
	if me := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	}); me.status != http.StatusUnauthorized {
		t.Errorf("status after logout = %d, want 401", me.status)
	}

	// The other device is untouched - logout is per-session.
	if other := env.do(t, apiRequest{
		method:  http.MethodGet,
		path:    "/api/v1/auth/me",
		cookies: []*http.Cookie{secondCookie},
	}); other.status != http.StatusOK {
		t.Errorf("the second device status = %d, want 200; logout must not be global", other.status)
	}
}

func TestLogout_ClearsCookies(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/logout",
		cookies: account.authCookies(), csrf: account.csrfToken,
	})

	cleared := res.cookie(env.cookies.SessionCookieName())
	if cleared == nil {
		t.Fatal("logout did not send a cookie-clearing header")
	}
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("session cookie = %q (MaxAge %d), want an immediate deletion", cleared.Value, cleared.MaxAge)
	}
}

func TestLogoutAll_EndsEverySession(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	// Three more devices, one of them native.
	var extra []*http.Cookie
	for i := 0; i < 2; i++ {
		res := env.do(t, apiRequest{
			method: http.MethodPost, path: "/api/v1/auth/login",
			body: map[string]string{"identifier": account.username, "password": account.password},
		})
		extra = append(extra, res.cookie(env.cookies.SessionCookieName()))
	}
	nativeLogin := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": account.password, "client": "native"},
	})
	var nativePayload struct {
		Data struct{ Token *string } `json:"data"`
	}
	nativeLogin.json(t, &nativePayload)

	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/logout-all",
		cookies: account.authCookies(), csrf: account.csrfToken,
	})
	if res.status != http.StatusNoContent {
		t.Fatalf("logout-all status = %d, want 204. body: %s", res.status, res.body)
	}

	for i, cookie := range append(extra, account.sessionCookie) {
		if me := env.do(t, apiRequest{
			method: http.MethodGet, path: "/api/v1/auth/me", cookies: []*http.Cookie{cookie},
		}); me.status != http.StatusUnauthorized {
			t.Errorf("web session %d status = %d, want 401 after logout-all", i, me.status)
		}
	}

	// Logout-all must cross transports: a phone stays signed in otherwise.
	if nativePayload.Data.Token != nil {
		if me := env.do(t, apiRequest{
			method: http.MethodGet, path: "/api/v1/auth/me", bearer: *nativePayload.Data.Token,
		}); me.status != http.StatusUnauthorized {
			t.Errorf("native session status = %d, want 401 after logout-all", me.status)
		}
	}
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

func TestCSRF_CookieAuthenticatedLogoutRequiresToken(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	// Forcing a victim to log out is a real CSRF target.
	res := env.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/auth/logout",
		cookies: account.authCookies(),
		// no CSRF header
	})
	if res.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 without a CSRF token", res.status)
	}

	// The session must survive the rejected attempt.
	if me := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	}); me.status != http.StatusOK {
		t.Error("the rejected CSRF attempt still ended the session")
	}
}

func TestCSRF_RejectsWrongToken(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/logout",
		cookies: account.authCookies(), csrf: "a-token-the-attacker-guessed",
	})
	if res.status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a mismatched CSRF token", res.status)
	}
}

func TestCSRF_RejectsCrossSiteOrigin(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/logout",
		cookies: account.authCookies(), csrf: account.csrfToken,
		origin: "https://evil.example",
	})
	if res.status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a cross-site origin", res.status)
	}
}

func TestCSRF_NotRequiredForBearer(t *testing.T) {
	env := newAuthEnv(t)
	token, _, _, _ := env.registerNative(t)

	// Exactly how a native client calls: no Origin, no cookie, no CSRF header.
	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/logout",
		bearer: token, origin: " ",
	})
	if res.status != http.StatusNoContent {
		t.Errorf("status = %d, want 204 - Bearer requests must not need CSRF. body: %s", res.status, res.body)
	}
}

func TestCSRF_NotRequiredForLoginOrRegister(t *testing.T) {
	env := newAuthEnv(t)

	// A first-time visitor has no CSRF token yet; requiring one would make the
	// forms unusable.
	username := uniqueName(t, "firsttime")
	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/register",
		body: map[string]string{
			"username": username,
			"email":    username + "@example.com",
			"password": "correct horse battery staple",
		},
	})
	if res.status != http.StatusCreated {
		t.Errorf("register status = %d, want 201 without a CSRF token. body: %s", res.status, res.body)
	}
}

// ---------------------------------------------------------------------------
// Password reset
// ---------------------------------------------------------------------------

func TestPasswordReset_FullFlow(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	forgot := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/forgot",
		body: map[string]string{"email": account.email},
	})
	if forgot.status != http.StatusAccepted {
		t.Fatalf("forgot status = %d, want 202. body: %s", forgot.status, forgot.body)
	}

	token := env.mailer.lastLinkToken(t, "/reset-password")
	const newPassword = "an entirely different passphrase"

	reset := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/reset",
		body: map[string]string{"token": token, "password": newPassword},
	})
	if reset.status != http.StatusOK {
		t.Fatalf("reset status = %d, want 200. body: %s", reset.status, reset.body)
	}

	// The old password must no longer work.
	if old := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": account.password},
	}); old.status != http.StatusUnauthorized {
		t.Errorf("the old password still works (status %d)", old.status)
	}

	// The new one must.
	if fresh := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": newPassword},
	}); fresh.status != http.StatusOK {
		t.Errorf("the new password does not work (status %d)", fresh.status)
	}
}

// A reset is the remedy for a compromised account, so it must evict the
// attacker (docs/10 §16, §37).
func TestPasswordReset_InvalidatesExistingSessions(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/forgot",
		body: map[string]string{"email": account.email},
	})
	token := env.mailer.lastLinkToken(t, "/reset-password")

	env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/reset",
		body: map[string]string{"token": token, "password": "an entirely different passphrase"},
	})

	if me := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	}); me.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 - a reset must end every existing session", me.status)
	}
}

func TestPasswordReset_TokenIsSingleUse(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/forgot",
		body: map[string]string{"email": account.email},
	})
	token := env.mailer.lastLinkToken(t, "/reset-password")

	first := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/reset",
		body: map[string]string{"token": token, "password": "the first replacement passphrase"},
	})
	if first.status != http.StatusOK {
		t.Fatalf("first reset status = %d, want 200", first.status)
	}

	second := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/reset",
		body: map[string]string{"token": token, "password": "the second replacement passphrase"},
	})
	if second.status == http.StatusOK {
		t.Error("the reset token was accepted twice; it must be single-use")
	}
}

// docs/10 §16: the response must not reveal whether an address is registered.
func TestPasswordReset_DoesNotRevealAccountExistence(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	known := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/forgot",
		body: map[string]string{"email": account.email},
	})
	unknown := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/forgot",
		body: map[string]string{"email": "definitely-nobody@example.com"},
	})

	if known.status != unknown.status {
		t.Errorf("statuses differ (%d vs %d), revealing account existence", known.status, unknown.status)
	}
	if string(known.body) != string(unknown.body) {
		t.Errorf("bodies differ, revealing account existence:\n known: %s\n unknown: %s", known.body, unknown.body)
	}
}

func TestPasswordReset_RejectsInvalidToken(t *testing.T) {
	env := newAuthEnv(t)

	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/reset",
		body: map[string]string{"token": "not-a-real-token", "password": "a perfectly fine passphrase"},
	})
	if res.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid token", res.status)
	}
}

func TestPasswordReset_StoresTokenHashedOnly(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/password/forgot",
		body: map[string]string{"email": account.email},
	})
	token := env.mailer.lastLinkToken(t, "/reset-password")

	var stored string
	if err := env.db.QueryRowContext(context.Background(),
		"SELECT token_hash FROM password_reset_tokens WHERE user_id = $1", account.userID).Scan(&stored); err != nil {
		t.Fatalf("read reset token: %v", err)
	}
	if stored == token {
		t.Fatal("the raw reset token is stored in the database")
	}
	if stored != auth.HashToken(token) {
		t.Error("the stored digest does not match the issued token")
	}
}

// ---------------------------------------------------------------------------
// Email verification
// ---------------------------------------------------------------------------

func TestEmailVerification_FullFlow(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	token := env.mailer.lastLinkToken(t, "/verify-email")

	res := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/verify-email",
		body: map[string]string{"token": token},
	})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", res.status, res.body)
	}

	me := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	})
	var payload struct {
		Data struct {
			EmailVerified bool   `json:"email_verified"`
			Status        string `json:"status"`
		} `json:"data"`
	}
	me.json(t, &payload)

	if !payload.Data.EmailVerified {
		t.Error("the account is still unverified after redeeming the token")
	}
	if payload.Data.Status != "active" {
		t.Errorf("status = %q, want active after verification", payload.Data.Status)
	}
}

// docs/10 §17 and the Phase 1 decision: verification must NOT gate reading or
// ordinary account use.
func TestEmailVerification_IsNotRequiredForAccountUse(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	if me := env.do(t, apiRequest{
		method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
	}); me.status != http.StatusOK {
		t.Errorf("an unverified account cannot use /me (status %d); verification gates publishing only", me.status)
	}

	if login := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/login",
		body: map[string]string{"identifier": account.username, "password": account.password},
	}); login.status != http.StatusOK {
		t.Errorf("an unverified account cannot sign in (status %d)", login.status)
	}
}

func TestEmailVerification_TokenIsSingleUse(t *testing.T) {
	env := newAuthEnv(t)
	env.registerWeb(t)

	token := env.mailer.lastLinkToken(t, "/verify-email")

	if first := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/verify-email",
		body: map[string]string{"token": token},
	}); first.status != http.StatusOK {
		t.Fatalf("first verification status = %d, want 200", first.status)
	}

	if second := env.do(t, apiRequest{
		method: http.MethodPost, path: "/api/v1/auth/verify-email",
		body: map[string]string{"token": token},
	}); second.status == http.StatusOK {
		t.Error("the verification token was accepted twice")
	}
}

// ---------------------------------------------------------------------------
// Guest access - must not regress (docs/11 §12)
// ---------------------------------------------------------------------------

func TestGuestAccess_PublicEndpointsRemainOpen(t *testing.T) {
	env := newAuthEnv(t)

	public := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/health"},
		{http.MethodGet, "/ready"},
		{http.MethodGet, "/api/v1/fiction-formats"},
	}

	for _, endpoint := range public {
		res := env.do(t, apiRequest{method: endpoint.method, path: endpoint.path})
		if res.status == http.StatusUnauthorized || res.status == http.StatusForbidden {
			t.Errorf("%s %s returned %d; adding authentication must not gate public content",
				endpoint.method, endpoint.path, res.status)
		}
	}
}

func TestGuestAccess_UnauthenticatedRequestIsNotRejectedGlobally(t *testing.T) {
	env := newAuthEnv(t)

	// The Authenticate middleware runs on every request. A guest must pass
	// through it, not be rejected by it.
	res := env.do(t, apiRequest{method: http.MethodGet, path: "/api/v1/fiction-formats"})
	if res.status != http.StatusOK {
		t.Fatalf("guest status = %d, want 200. body: %s", res.status, res.body)
	}
}

func TestGuestAccess_GarbageCredentialStillReadsPublicContent(t *testing.T) {
	env := newAuthEnv(t)

	// A stale or corrupt cookie must degrade to guest, not to an error page -
	// otherwise a reader with an old cookie would be locked out of reading.
	res := env.do(t, apiRequest{
		method:  http.MethodGet,
		path:    "/api/v1/fiction-formats",
		cookies: []*http.Cookie{{Name: env.cookies.SessionCookieName(), Value: "stale-garbage"}},
	})
	if res.status != http.StatusOK {
		t.Errorf("status = %d, want 200 - an invalid credential must degrade to guest", res.status)
	}
}

// ---------------------------------------------------------------------------
// Credential leakage
// ---------------------------------------------------------------------------

func TestCredentials_NeverAppearInResponses(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)

	responses := map[string]apiResponse{
		"me": env.do(t, apiRequest{
			method: http.MethodGet, path: "/api/v1/auth/me", cookies: account.authCookies(),
		}),
		"login": env.do(t, apiRequest{
			method: http.MethodPost, path: "/api/v1/auth/login",
			body: map[string]string{"identifier": account.username, "password": account.password},
		}),
	}

	for name, res := range responses {
		body := string(res.body)
		for _, secret := range []string{account.password, "password_hash", "$argon2id$", "token_hash"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s response contains %q: %s", name, secret, body)
			}
		}
	}
}

func TestSessionRepository_DeleteExpiredRemovesDeadRows(t *testing.T) {
	env := newAuthEnv(t)
	account := env.registerWeb(t)
	ctx := context.Background()

	if _, err := env.db.ExecContext(ctx,
		"UPDATE user_sessions SET expires_at = now() - interval '1 day' WHERE user_id = $1",
		account.userID); err != nil {
		t.Fatalf("expire session: %v", err)
	}

	repo := auth.NewSessionRepository(env.db.DB)
	if _, err := repo.DeleteExpired(ctx, time.Now()); err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}

	var remaining int
	if err := env.db.QueryRowContext(ctx,
		"SELECT count(*) FROM user_sessions WHERE user_id = $1", account.userID).Scan(&remaining); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	// docs/08 §37 hard-deletes expired sessions: a dead credential has no audit
	// value and retaining it conflicts with the retention policy.
	if remaining != 0 {
		t.Errorf("%d expired sessions remain, want 0", remaining)
	}
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

func TestRateLimit_AuthEndpointsUseTheStrictTier(t *testing.T) {
	env := newAuthEnv(t)

	// The auth tier is far stricter than public reading (docs/10 §38).
	var last apiResponse
	for i := 0; i <= 12; i++ {
		last = env.do(t, apiRequest{
			method: http.MethodPost, path: "/api/v1/auth/login",
			body: map[string]string{"identifier": "someone", "password": "wrong password entirely"},
		})
	}

	if last.status != http.StatusTooManyRequests {
		t.Errorf("status after 13 login attempts = %d, want 429 (brute-force protection)", last.status)
	}
}
