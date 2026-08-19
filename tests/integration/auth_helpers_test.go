package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/achievements"
	"github.com/fictionthai/fictionthai/backend/internal/ai"
	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/authors"
	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/characters"
	"github.com/fictionthai/fictionthai/backend/internal/comments"
	"github.com/fictionthai/fictionthai/backend/internal/community"
	"github.com/fictionthai/fictionthai/backend/internal/config"
	"github.com/fictionthai/fictionthai/backend/internal/desk"
	"github.com/fictionthai/fictionthai/backend/internal/insights"
	"github.com/fictionthai/fictionthai/backend/internal/library"
	"github.com/fictionthai/fictionthai/backend/internal/media"
	"github.com/fictionthai/fictionthai/backend/internal/moderation"
	"github.com/fictionthai/fictionthai/backend/internal/notifications"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/pennames"
	"github.com/fictionthai/fictionthai/backend/internal/platform/cache"
	"github.com/fictionthai/fictionthai/backend/internal/platform/database"
	"github.com/fictionthai/fictionthai/backend/internal/platform/email"
	"github.com/fictionthai/fictionthai/backend/internal/platform/storage"
	"github.com/fictionthai/fictionthai/backend/internal/profiles"
	"github.com/fictionthai/fictionthai/backend/internal/ratelimit"
	"github.com/fictionthai/fictionthai/backend/internal/server"
	"github.com/fictionthai/fictionthai/backend/internal/shelves"
	"github.com/fictionthai/fictionthai/backend/internal/subscriptions"
	"github.com/fictionthai/fictionthai/backend/internal/taxonomy"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/internal/variables"
	"github.com/fictionthai/fictionthai/backend/internal/views"
	"github.com/fictionthai/fictionthai/backend/internal/wall"
)

const testOrigin = "http://localhost:3000"

// testMediaMaxBytes keeps the oversize-upload test cheap: big enough for any
// real test image, small enough that exceeding it needs only ~¼ MiB.
const testMediaMaxBytes = 256 * 1024

// captureMailer records what would have been sent, so a test can follow a
// password-reset or verification link without a mail provider.
type captureMailer struct {
	messages []email.Message
}

func (m *captureMailer) Send(_ context.Context, msg email.Message) error {
	m.messages = append(m.messages, msg)
	return nil
}

// lastLinkContaining returns the token from the most recent message whose body
// contains the given path.
func (m *captureMailer) lastLinkToken(t *testing.T, path string) string {
	t.Helper()

	for i := len(m.messages) - 1; i >= 0; i-- {
		body := m.messages[i].Body
		idx := strings.Index(body, path+"?token=")
		if idx < 0 {
			continue
		}
		token := body[idx+len(path)+len("?token="):]
		if end := strings.IndexAny(token, "\n \t"); end >= 0 {
			token = token[:end]
		}
		return strings.TrimSpace(token)
	}
	t.Fatalf("no message contained a %s link; messages: %d", path, len(m.messages))
	return ""
}

// authEnv is a fully wired API backed by the real test database.
type authEnv struct {
	router  http.Handler
	db      *database.DB
	mailer  *captureMailer
	service *auth.Service
	cookies auth.CookieConfig
	// aiTools lets a test attach a fake consistency sidecar
	// (docs/AI-CONSISTENCY-MODEL.md) via SetModelClient.
	aiTools *ai.Tools
}

// envConfig carries the knobs an individual test may override. It exists so the
// AI phase can inject a deterministic FAKE provider (to exercise provider
// failures) or a tiny quota, without changing the many existing newAuthEnv
// callers.
type envConfig struct {
	aiProvider      ai.Provider
	aiDailyQuota    int
	aiMaxInputRunes int
	aiEnabled       bool
	aiRunWorker     bool

	subscriptionMode            string
	subscriptionPromptPayTarget string
	subscriptionDemoTier        string
	subscriptionDemoDuration    time.Duration
}

// EnvOption tunes an authEnv at construction.
type EnvOption func(*envConfig)

func withAIProvider(p ai.Provider) EnvOption { return func(c *envConfig) { c.aiProvider = p } }
func withAIDailyQuota(n int) EnvOption       { return func(c *envConfig) { c.aiDailyQuota = n } }
func withAIMaxInputRunes(n int) EnvOption    { return func(c *envConfig) { c.aiMaxInputRunes = n } }
func withAIDisabled() EnvOption              { return func(c *envConfig) { c.aiEnabled = false } }

// withAIWorkerDisabled keeps AI enabled but does NOT start the async worker, so
// a queued request stays queued - letting a test exercise the cancel transition
// deterministically instead of racing the worker.
func withAIWorkerDisabled() EnvOption { return func(c *envConfig) { c.aiRunWorker = false } }

// withSubscriptionDisabled puts Premium in disabled mode, so a test can assert
// the acquisition surface is off while pricing/overview stay available.
func withSubscriptionDisabled() EnvOption {
	return func(c *envConfig) { c.subscriptionMode = "disabled" }
}

// withSubscriptionDemo puts Premium in free-demo mode.
func withSubscriptionDemo() EnvOption {
	return func(c *envConfig) { c.subscriptionMode = "demo" }
}

// withSubscriptionDemoTier overrides which tier the demo grants (default pro).
func withSubscriptionDemoTier(tier string) EnvOption {
	return func(c *envConfig) { c.subscriptionDemoTier = tier }
}

// newAuthEnv builds the environment and gives each test an isolated username
// namespace, so tests can run in any order against a shared database.
func newAuthEnv(t *testing.T, opts ...EnvOption) *authEnv {
	t.Helper()

	envCfg := envConfig{
		aiProvider:      ai.NewLocalProvider(),
		aiDailyQuota:    1000, // generous: individual quota tests set their own
		aiMaxInputRunes: 20000,
		aiEnabled:       true,
		aiRunWorker:     true,

		// Default to LIVE so the existing paid-flow suite exercises real checkout.
		// Demo/disabled tests opt in with withSubscriptionDemo/Disabled.
		subscriptionMode: "live",
		// A real-shaped Thai mobile so the PromptPay QR payload is exercised.
		subscriptionPromptPayTarget: "0812345678",
		subscriptionDemoTier:        "pro",
		subscriptionDemoDuration:    30 * 24 * time.Hour,
	}
	for _, o := range opts {
		o(&envCfg)
	}

	ctx := context.Background()
	db := connectDB(t)

	// The identity schema must exist before these tests can run.
	migrator, err := database.NewMigrator(db.DB)
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	mailer := &captureMailer{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Fast Argon2id parameters: the suite runs many registrations, and the
	// production work factor would make it unusably slow.
	passwordParams := auth.DefaultPasswordParams()
	passwordParams.Memory = 8 * 1024
	passwordParams.Iterations = 1

	service := auth.NewService(
		users.NewRepository(db.DB),
		auth.NewSessionRepository(db.DB),
		auth.NewTokenRepository(db.DB),
		mailer,
		log,
		auth.Config{
			WebLifetime:          auth.Lifetime{Absolute: 14 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
			MobileLifetime:       auth.Lifetime{Absolute: 90 * 24 * time.Hour, Idle: 30 * 24 * time.Hour},
			PasswordParams:       passwordParams,
			PasswordResetTTL:     time.Hour,
			EmailVerificationTTL: 48 * time.Hour,
			AppURL:               testOrigin,
			TouchInterval:        time.Hour,
		},
	)

	limiter := ratelimit.NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })

	cookies := auth.CookieConfig{Secure: false}

	// Interaction infrastructure - the REAL asynchronous path: an in-process
	// queue plus a running worker, so tests exercise the same enqueue → worker
	// → rows flow production uses rather than a synchronous shortcut. Tests
	// that assert on notifications poll briefly (awaitNotifications).
	notificationQueue := notifications.NewMemoryQueue()
	notificationRepo := notifications.NewRepository(db.DB)
	notificationService := notifications.NewService(notificationRepo, notificationQueue, log)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	worker := notifications.NewWorker(notificationQueue, notificationRepo, log)
	waitForWorker := worker.Start(workerCtx)
	t.Cleanup(func() {
		stopWorker()
		waitForWorker()
	})

	// Publishing core. Chapters depend on novels for ownership, never the
	// reverse (docs/08 §10.2).
	userRepo := users.NewRepository(db.DB)
	novelRepo := novels.NewRepository(db.DB)
	taxonomyRepo := taxonomy.NewRepository(db.DB)
	taxonomyService := taxonomy.NewService(taxonomyRepo, log)
	libraryRepo := library.NewRepository(db.DB)
	novelService := novels.NewService(novelRepo, userRepo, taxonomyRepo, libraryRepo, log)
	chapterRepo := chapters.NewRepository(db.DB)
	chapterService := chapters.NewService(chapterRepo, novelService, notificationService, log)
	libraryService := library.NewService(libraryRepo, novelService, novelRepo, userRepo, notificationService, log)
	commentRepo := comments.NewRepository(db.DB)
	commentService := comments.NewService(commentRepo, novelService, chapterService, notificationService, log)
	// Public profiles (Phase 12E): a read-only composition of identity and
	// published work.
	profileService := profiles.NewService(profiles.NewRepository(db.DB), log)
	// Pen names (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2): self-scoped, so the
	// only wiring it needs is its own repository.
	penNameService := pennames.NewService(pennames.NewRepository(db.DB), log)
	// Public bookshelves and the profile wall: the real repositories, so the
	// integration tests exercise the actual visibility SQL rather than a
	// stand-in.
	shelfService := shelves.NewService(
		shelves.NewRepository(db.DB), novelService, novelRepo, log)
	wallService := wall.NewService(wall.NewRepository(db.DB), log)

	communityRepo := community.NewRepository(db.DB)
	communityService := community.NewService(
		communityRepo, userRepo,
		novelService, chapterService,
		notificationService, log,
	)

	// The studio overview (§13R). The real repositories behind it, so the
	// integration tests exercise the actual SQL rather than a stand-in.
	insightsService := insights.NewService(
		novelService, views.NewRepository(db.DB), commentRepo, communityRepo, log,
	)

	// Media uses a real filesystem backend under the test's temp directory,
	// so the full upload → store → serve → delete lifecycle is exercised
	// without touching a developer's data directory.
	objectStore, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("initialise media storage: %v", err)
	}
	// Premium subscription layer (Phase 11): built before media because a
	// payment-slip upload attaches through it (media.PaymentSlipTarget).
	subscriptionService := subscriptions.NewService(
		subscriptions.NewRepository(db.DB), notificationService,
		subscriptions.Config{
			Mode:            subscriptions.Mode(envCfg.subscriptionMode),
			PromptPayTarget: envCfg.subscriptionPromptPayTarget,
			PromptPayName:   "FICTIONTHAI",
			DemoTier:        subscriptions.Tier(envCfg.subscriptionDemoTier),
			DemoDuration:    envCfg.subscriptionDemoDuration,
		},
		log,
	)
	authorService := authors.NewService(authors.NewRepository(db.DB), log)
	characterService := characters.NewService(characters.NewRepository(db.DB), novelService, log)
	variableService := variables.NewService(variables.NewRepository(db.DB), novelService, log)

	mediaService := media.NewService(
		media.NewRepository(db.DB), objectStore, novelService, userRepo, subscriptionService,
		media.Config{
			MaxUploadBytes: testMediaMaxBytes,
			PublicBaseURL:  "http://localhost:8080",
		},
		log,
	)

	moderationService := moderation.NewService(
		moderation.NewRepository(db.DB),
		novelService, chapterService, commentService, communityService,
		userRepo, mediaService, notificationService, log,
	)

	// AI layer: a REAL asynchronous worker over an in-process queue, so tests
	// exercise the same enqueue → claim → complete → notify flow production
	// uses. The provider is deterministic (the local rule engine by default, or
	// a fake for failure tests), so no network or model is involved.
	aiQueue := ai.NewMemoryQueue()
	aiService := ai.NewService(
		ai.NewRepository(db.DB), envCfg.aiProvider, chapterService, notificationService,
		aiQueue, limiter,
		ai.Config{
			Enabled:       envCfg.aiEnabled,
			MaxInputRunes: envCfg.aiMaxInputRunes,
			DailyQuota:    envCfg.aiDailyQuota,
		},
		log,
	)
	if envCfg.aiEnabled && envCfg.aiRunWorker {
		aiWorkerCtx, stopAIWorker := context.WithCancel(context.Background())
		aiWorker := ai.NewWorker(aiQueue, aiService, log)
		waitForAIWorker := aiWorker.Start(aiWorkerCtx)
		t.Cleanup(func() {
			stopAIWorker()
			waitForAIWorker()
		})
	}

	aiTools := ai.NewTools(
		ai.NewToolsRepository(db.DB), ai.NewLocalProvider(),
		novelService, characterService, variableService,
		ai.Config{
			Enabled:       envCfg.aiEnabled,
			MaxInputRunes: envCfg.aiMaxInputRunes,
			DailyQuota:    envCfg.aiDailyQuota,
		},
		log,
	)

	// Achievements (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3), wired the way
	// production wires it: built after the domains, attached by setter, so the
	// integration suite exercises the real signal path from the real choke
	// points rather than calling Record directly.
	achievementService := achievements.NewService(achievements.NewRepository(db.DB), log)
	chapterService.SetAchiever(achievementService)
	novelService.SetAchiever(achievementService)
	characterService.SetAchiever(achievementService)
	aiService.SetAchiever(achievementService)
	aiTools.SetAchiever(achievementService)

	router := server.NewRouter(server.Dependencies{
		Config: &config.Config{
			App:     config.App{Name: "fictionthai-api", Env: config.EnvTest, LogLevel: "error"},
			HTTP:    config.HTTP{Port: 8080, MaxRequestBytes: 1 << 20},
			CORS:    config.CORS{AllowedOrigins: []string{testOrigin}},
			Session: config.Session{SecureCookies: false},
		},
		Logger:        log,
		DB:            db,
		Cache:         &cache.Client{},
		Limiter:       limiter,
		Version:       "integration",
		Auth:          service,
		Novels:        novelService,
		Chapters:      chapterService,
		Insights:      insightsService,
		Desk:          desk.NewService(chapterRepo, novelRepo),
		Characters:    characterService,
		Variables:     variableService,
		Library:       libraryService,
		Taxonomy:      taxonomyService,
		Comments:      commentService,
		Notifications: notificationService,
		Community:     communityService,
		Moderation:    moderationService,
		Media:         mediaService,
		AI:            aiService,
		AITools:       aiTools,
		Subscription:  subscriptionService,
		Authors:       authorService,
		Profiles:      profileService,
		PenNames:      penNameService,
		Achievements:  achievementService,
		Shelves:       shelfService,
		Wall:          wallService,
	})

	return &authEnv{
		router: router, db: db, mailer: mailer, service: service,
		cookies: cookies, aiTools: aiTools,
	}
}

// apiRequest describes one call to the test API.
type apiRequest struct {
	method  string
	path    string
	body    any
	cookies []*http.Cookie
	bearer  string
	origin  string
	csrf    string
	headers map[string]string
}

type apiResponse struct {
	status  int
	body    []byte
	cookies []*http.Cookie
	header  http.Header
}

// json decodes the response body into target.
func (r apiResponse) json(t *testing.T, target any) {
	t.Helper()
	if err := json.Unmarshal(r.body, target); err != nil {
		t.Fatalf("response is not JSON (%d): %s", r.status, string(r.body))
	}
}

// cookie returns the named cookie set by the response, or nil.
func (r apiResponse) cookie(name string) *http.Cookie {
	for _, c := range r.cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (e *authEnv) do(t *testing.T, req apiRequest) apiResponse {
	t.Helper()

	var bodyReader io.Reader
	if req.body != nil {
		encoded, err := json.Marshal(req.body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		bodyReader = strings.NewReader(string(encoded))
	}

	r := httptest.NewRequest(req.method, req.path, bodyReader)
	r.Header.Set("Content-Type", "application/json")

	origin := req.origin
	if origin == "" {
		origin = testOrigin
	}
	r.Header.Set("Origin", origin)

	for _, c := range req.cookies {
		r.AddCookie(c)
	}
	if req.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+req.bearer)
	}
	if req.csrf != "" {
		r.Header.Set(auth.CSRFHeaderName, req.csrf)
	}
	for k, v := range req.headers {
		r.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, r)

	return apiResponse{
		status:  rec.Code,
		body:    rec.Body.Bytes(),
		cookies: rec.Result().Cookies(),
		header:  rec.Header(),
	}
}

// webSession is an authenticated browser: session cookie plus CSRF token.
type webSession struct {
	sessionCookie *http.Cookie
	csrfCookie    *http.Cookie
	csrfToken     string
	userID        string
	username      string
	email         string
	password      string
}

// authCookies returns the cookies a browser would send.
func (s webSession) authCookies() []*http.Cookie {
	out := []*http.Cookie{}
	if s.sessionCookie != nil {
		out = append(out, s.sessionCookie)
	}
	if s.csrfCookie != nil {
		out = append(out, s.csrfCookie)
	}
	return out
}

// nameCounter disambiguates names created in the same clock tick - parallel
// tests can register simultaneously, and the Windows clock is coarse enough
// for two UnixNano reads to collide.
var nameCounter atomic.Int64

// uniqueName produces an identifier unique to this test run, so a shared
// database does not make tests order-dependent. Well under the 32-character
// username limit: prefix + 9 timestamp digits + up to 4 counter digits.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	// Test names contain characters the username rules reject; the timestamp
	// keeps RUNS distinct, the counter keeps names within a run distinct.
	return fmt.Sprintf("%s%d%d",
		prefix, time.Now().UnixNano()%1_000_000_000, nameCounter.Add(1)%10_000)
}

// registerWeb creates an account through the API and returns the browser state.
func (e *authEnv) registerWeb(t *testing.T) webSession {
	t.Helper()

	username := uniqueName(t, "writer")
	emailAddr := username + "@example.com"
	const password = "correct horse battery staple"

	res := e.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/auth/register",
		body: map[string]string{
			"username": username,
			"email":    emailAddr,
			"password": password,
			"client":   "web",
		},
	})
	if res.status != http.StatusCreated {
		t.Fatalf("register status = %d, want 201. body: %s", res.status, res.body)
	}

	var payload struct {
		Data struct {
			User      struct{ ID string } `json:"user"`
			Token     *string             `json:"token"`
			CSRFToken *string             `json:"csrf_token"`
		} `json:"data"`
	}
	res.json(t, &payload)

	session := res.cookie(e.cookies.SessionCookieName())
	if session == nil {
		t.Fatal("registration did not set a session cookie")
	}

	out := webSession{
		sessionCookie: session,
		csrfCookie:    res.cookie(e.cookies.CSRFCookieName()),
		userID:        payload.Data.User.ID,
		username:      username,
		email:         emailAddr,
		password:      password,
	}
	if payload.Data.CSRFToken != nil {
		out.csrfToken = *payload.Data.CSRFToken
	}
	return out
}

// registerNative creates an account as a mobile client and returns its token.
func (e *authEnv) registerNative(t *testing.T) (token, username, emailAddr, password string) {
	t.Helper()

	username = uniqueName(t, "mobile")
	emailAddr = username + "@example.com"
	password = "correct horse battery staple"

	res := e.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/auth/register",
		body: map[string]string{
			"username": username,
			"email":    emailAddr,
			"password": password,
			"client":   "native",
		},
	})
	if res.status != http.StatusCreated {
		t.Fatalf("native register status = %d, want 201. body: %s", res.status, res.body)
	}

	var payload struct {
		Data struct {
			Token *string `json:"token"`
		} `json:"data"`
	}
	res.json(t, &payload)
	if payload.Data.Token == nil || *payload.Data.Token == "" {
		t.Fatal("native registration returned no token")
	}
	return *payload.Data.Token, username, emailAddr, password
}
