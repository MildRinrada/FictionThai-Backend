// Package integration exercises the API against real PostgreSQL and Redis.
//
// docs/15 - Testing Strategy.md §3 places these between unit and E2E tests:
// they verify API + database and API + Redis wiring, not business rules in
// isolation.
//
// The whole suite SKIPS when TEST_DATABASE_URL is unset, so `go test ./...`
// stays runnable on a laptop with no infrastructure. CI sets the variable.
//
//	docker compose -f infrastructure/docker/docker-compose.yml up -d
//	export TEST_DATABASE_URL="postgres://fictionthai:fictionthai_dev@localhost:55432/fictionthai_test?sslmode=disable"
//	export TEST_REDIS_URL="redis://localhost:55379/1"
//	go test ./tests/integration/...
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/config"
	"github.com/fictionthai/fictionthai/backend/internal/platform/cache"
	"github.com/fictionthai/fictionthai/backend/internal/platform/database"
	"github.com/fictionthai/fictionthai/backend/internal/ratelimit"
	"github.com/fictionthai/fictionthai/backend/internal/server"
)

func databaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping integration tests")
	}
	return url
}

func redisURL() string { return os.Getenv("TEST_REDIS_URL") }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func connectDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Connect(context.Background(), config.Database{
		URL:             databaseURL(t),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnectTimeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPostgres_ConnectsAndResponds(t *testing.T) {
	db := connectDB(t)

	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	var one int
	if err := db.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 returned %d", one)
	}
}

// The connection pool must honour its configured ceiling; an unbounded pool
// would exhaust a small managed database (docs/14 §13).
func TestPostgres_RespectsPoolLimits(t *testing.T) {
	db := connectDB(t)

	if got := db.Stats().MaxOpenConnections; got != 5 {
		t.Errorf("MaxOpenConnections = %d, want 5", got)
	}
}

// Migrations must be idempotent: running `up` twice is what a redeploy does.
func TestMigrations_ApplyAndAreIdempotent(t *testing.T) {
	ctx := context.Background()

	db, err := database.ConnectForMigrations(ctx, config.Database{
		URL:             databaseURL(t),
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnectTimeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect for migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	migrator, err := database.NewMigrator(db.DB)
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}

	if _, err := migrator.Up(ctx); err != nil {
		t.Fatalf("first up: %v", err)
	}

	version, err := migrator.Version(ctx)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version == 0 {
		t.Fatal("schema version is still 0 after migrating up")
	}

	// Second run: nothing should be applied and the version must not move.
	applied, err := migrator.Up(ctx)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("re-running up applied %d migrations; it must be a no-op", len(applied))
	}

	after, err := migrator.Version(ctx)
	if err != nil {
		t.Fatalf("read version again: %v", err)
	}
	if after != version {
		t.Errorf("schema version moved from %d to %d on a repeat run", version, after)
	}

	pending, err := migrator.Pending(ctx)
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d migrations still pending after up", pending)
	}
}

// The baseline migration installs the extensions the documented schema needs.
func TestMigrations_InstallRequiredExtensions(t *testing.T) {
	ctx := context.Background()
	db := connectDB(t)

	migrator, err := database.NewMigrator(db.DB)
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	for _, extension := range []string{"pgcrypto", "citext", "pg_trgm"} {
		var present bool
		err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", extension,
		).Scan(&present)
		if err != nil {
			t.Fatalf("check extension %s: %v", extension, err)
		}
		if !present {
			t.Errorf("extension %q was not installed", extension)
		}
	}
}

func TestRedis_ConnectsAndResponds(t *testing.T) {
	url := redisURL()
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set; skipping Redis integration test")
	}

	client, err := cache.Connect(context.Background(), config.Redis{
		URL:            url,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect to redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if !client.Enabled() {
		t.Fatal("client reports disabled despite a configured URL")
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

// The Redis-backed limiter shares one budget across API instances, so it must
// behave identically to the in-memory one from a caller's point of view.
func TestRedisRateLimiter_EnforcesPolicy(t *testing.T) {
	url := redisURL()
	if url == "" {
		t.Skip("TEST_REDIS_URL is not set; skipping Redis rate-limit test")
	}
	ctx := context.Background()

	client, err := cache.Connect(ctx, config.Redis{URL: url, ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect to redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	limiter := ratelimit.NewRedisLimiter(client.Redis())
	policy := ratelimit.Policy{Name: "integration_test", Limit: 3, Window: 2 * time.Second}
	// A unique key per run keeps repeated `go test` invocations independent.
	key := "key-" + time.Now().Format("150405.000000")

	for i := 1; i <= policy.Limit; i++ {
		if res := limiter.Allow(ctx, key, policy); !res.Allowed {
			t.Fatalf("request %d should have been allowed", i)
		}
	}
	res := limiter.Allow(ctx, key, policy)
	if res.Allowed {
		t.Fatal("the request past the limit should have been denied")
	}
	if res.RetryAfter <= 0 {
		t.Error("a denied result must carry a retry hint")
	}
}

// /ready must report every dependency as healthy once both are running - this
// is the check a deployment gate and an orchestrator rely on (docs/14 §45).
func TestReadiness_ReportsHealthyDependencies(t *testing.T) {
	ctx := context.Background()

	db := connectDB(t)

	redisClient, err := cache.Connect(ctx, config.Redis{
		URL:            redisURL(),
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect to redis: %v", err)
	}
	t.Cleanup(func() { _ = redisClient.Close() })

	limiter := ratelimit.NewMemoryLimiter()
	t.Cleanup(func() { _ = limiter.Close() })

	router := server.NewRouter(server.Dependencies{
		Config: &config.Config{
			App:  config.App{Name: "fictionthai-api", Env: config.EnvTest, LogLevel: "error"},
			HTTP: config.HTTP{Port: 8080, MaxRequestBytes: 1 << 20},
			CORS: config.CORS{AllowedOrigins: []string{"http://localhost:3000"}},
		},
		Logger:  testLogger(),
		DB:      db,
		Cache:   redisClient,
		Limiter: limiter,
		Version: "integration",
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/ready status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if got := body.Checks["postgres"]; got != "ok" {
		t.Errorf("postgres check = %q, want ok", got)
	}
	if redisURL() != "" {
		if got := body.Checks["redis"]; got != "ok" {
			t.Errorf("redis check = %q, want ok", got)
		}
	}
}
