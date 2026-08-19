package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fictionthai/fictionthai/backend/internal/config"
	"github.com/fictionthai/fictionthai/backend/internal/middleware"
	"github.com/fictionthai/fictionthai/backend/internal/platform/cache"
	"github.com/fictionthai/fictionthai/backend/internal/ratelimit"
	"github.com/fictionthai/fictionthai/backend/internal/server"
)

// newTestRouter builds a router with no live dependencies. Nothing here touches
// PostgreSQL or Redis, so these tests run in CI without infrastructure; the
// probes that DO need a database live in tests/integration.
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()

	limiter := ratelimit.NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })

	return server.NewRouter(server.Dependencies{
		Config: &config.Config{
			App:  config.App{Name: "fictionthai-api", Env: config.EnvTest, LogLevel: "error"},
			HTTP: config.HTTP{Port: 8080, MaxRequestBytes: 1 << 20},
			CORS: config.CORS{AllowedOrigins: []string{"http://localhost:3000"}},
		},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cache:   &cache.Client{},
		Limiter: limiter,
		Version: "test",
	})
}

func do(t *testing.T, router http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestHealth_ReportsOK(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf(`status = %q, want "ok"`, body["status"])
	}
}

// /health must answer "is the process alive?" without touching dependencies,
// so it stays green while /ready reports a degraded database (docs/14 §45).
func TestHealth_DoesNotDependOnDatabase(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("liveness returned %d with no database configured; it must not probe dependencies", rec.Code)
	}
}

func TestReady_ReportsDisabledDependencies(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/ready", nil)

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}

	// An unconfigured dependency is "disabled", not a failure - Redis is
	// optional (docs/07 §18).
	if got := body.Checks["redis"]; got != "disabled" {
		t.Errorf(`redis check = %q, want "disabled"`, got)
	}
	if _, present := body.Checks["postgres"]; !present {
		t.Error("readiness must report on postgres")
	}
}

func TestRoot_DescribesTheService(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body["api_version"] != server.APIVersion {
		t.Errorf("api_version = %v, want %q", body["api_version"], server.APIVersion)
	}
}

// The Fiction Format System vocabulary must be readable without an account:
// guests filter by format (docs/09 §6, §11).
func TestFictionFormats_AreAvailableToGuests(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/api/v1/fiction-formats", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an unauthenticated request", rec.Code)
	}

	var body struct {
		Data struct {
			StoryStructures     []string `json:"story_structures"`
			PresentationFormats []string `json:"presentation_formats"`
			ContentModes        []string `json:"content_modes"`
			Defaults            struct {
				StoryStructure     string `json:"story_structure"`
				PresentationFormat string `json:"presentation_format"`
				ContentMode        string `json:"content_mode"`
			} `json:"defaults"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}

	// The three dimensions must stay separate on the wire - docs/08 §43 Rule 6.
	assertContains(t, "story_structures", body.Data.StoryStructures, "one_shot", "multi_chapter")
	assertContains(t, "presentation_formats", body.Data.PresentationFormats, "standard", "chat")
	assertContains(t, "content_modes", body.Data.ContentModes, "general", "headcanon")

	if body.Data.Defaults.StoryStructure != "multi_chapter" {
		t.Errorf("default story_structure = %q, want multi_chapter", body.Data.Defaults.StoryStructure)
	}
}

func assertContains(t *testing.T, field string, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, v := range got {
		set[v] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("%s = %v, missing %q", field, got, w)
		}
	}
}

func TestUnknownRoute_UsesTheErrorEnvelope(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/api/v1/does-not-exist", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	// Clients branch on code, never on message (docs/09 §7).
	if body.Error.Code != "NOT_FOUND" {
		t.Errorf("error code = %q, want NOT_FOUND", body.Error.Code)
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/health", nil)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, expected := range want {
		if got := rec.Header().Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
	if rec.Header().Get("Permissions-Policy") == "" {
		t.Error("Permissions-Policy should be set")
	}
	// HSTS is meaningless (and misleading) over plain HTTP in dev/test.
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS should not be asserted outside production, got %q", got)
	}
}

func TestRequestID_IsGeneratedAndEchoed(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/health", nil)

	id := rec.Header().Get(middleware.HeaderRequestID)
	if id == "" {
		t.Fatal("every response must carry a request ID (docs/07 §49)")
	}
	if !strings.HasPrefix(id, "req_") {
		t.Errorf("generated request ID = %q, want a req_ prefix", id)
	}
}

func TestRequestID_ReusesTrustedInboundValue(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/health", map[string]string{
		middleware.HeaderRequestID: "edge-abc123",
	})

	if got := rec.Header().Get(middleware.HeaderRequestID); got != "edge-abc123" {
		t.Errorf("request ID = %q, want the inbound value to be reused for correlation", got)
	}
}

// A header-injected control character must never reach the logs.
func TestRequestID_RejectsMaliciousInboundValue(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/health", map[string]string{
		middleware.HeaderRequestID: "abc\tdef ghi",
	})

	got := rec.Header().Get(middleware.HeaderRequestID)
	if strings.Contains(got, "\t") || strings.Contains(got, " ") {
		t.Errorf("request ID = %q, want the unsafe inbound value to be replaced", got)
	}
	if !strings.HasPrefix(got, "req_") {
		t.Errorf("request ID = %q, want a freshly generated ID", got)
	}
}

func TestCORS_EchoesAllowedOrigin(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/health", map[string]string{
		"Origin": "http://localhost:3000",
	})

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the allowlisted origin", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, must include Origin so caches do not cross origins", got)
	}
}

// docs/11 §23: an unknown origin must never receive CORS headers, and the API
// must never answer with a wildcard.
func TestCORS_RejectsUnknownOrigin(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/health", map[string]string{
		"Origin": "https://evil.example",
	})

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no CORS headers for an unknown origin", got)
	}
}

func TestCORS_PreflightSucceedsForAllowedOrigin(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodOptions, "/api/v1/fiction-formats", map[string]string{
		"Origin":                        "http://localhost:3000",
		"Access-Control-Request-Method": "GET",
	})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("preflight must advertise the allowed methods")
	}
}

func TestMethodNotAllowed_UsesTheErrorEnvelope(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodDelete, "/health", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Error.Code != "METHOD_NOT_ALLOWED" {
		t.Errorf("error code = %q, want METHOD_NOT_ALLOWED", body.Error.Code)
	}
}

func TestRateLimit_HeadersArePresentOnAPIRoutes(t *testing.T) {
	rec := do(t, newTestRouter(t), http.MethodGet, "/api/v1/fiction-formats", nil)

	if rec.Header().Get("RateLimit-Limit") == "" {
		t.Error("rate-limited routes should advertise their limit to clients")
	}
}

func TestRateLimit_DeniesOnceThePolicyIsExhausted(t *testing.T) {
	router := newTestRouter(t)

	// The public-read policy is deliberately generous, so exhausting it takes
	// more requests than a normal reader would ever make.
	var last *httptest.ResponseRecorder
	for i := 0; i <= ratelimit.PublicRead.Limit; i++ {
		last = do(t, router, http.MethodGet, "/api/v1/fiction-formats", nil)
	}

	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status after %d requests = %d, want 429", ratelimit.PublicRead.Limit+1, last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("a 429 must tell the client when it may retry")
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(last.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Error.Code != "RATE_LIMIT_EXCEEDED" {
		t.Errorf("error code = %q, want RATE_LIMIT_EXCEEDED (docs/09 §26)", body.Error.Code)
	}
}
