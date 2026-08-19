package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/config"
)

// configEnvVars is every variable config.Load reads.
//
// The list exists so setEnv can CLEAR all of them. Without that, a developer
// who has exported APP_PORT or REDIS_URL - or who runs the suite through
// scripts/check.sh, which sources .env - would see these tests fail or, worse,
// pass for the wrong reason. Tests of default behaviour must not depend on the
// ambient shell.
var configEnvVars = []string{
	"APP_NAME", "APP_ENV", "APP_PORT", "LOG_LEVEL",
	"HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT", "HTTP_IDLE_TIMEOUT",
	"HTTP_SHUTDOWN_TIMEOUT", "HTTP_MAX_REQUEST_BYTES",
	"DATABASE_URL", "DATABASE_MAX_OPEN_CONNS", "DATABASE_MAX_IDLE_CONNS",
	"DATABASE_CONN_MAX_LIFETIME", "DATABASE_CONNECT_TIMEOUT",
	"REDIS_URL", "REDIS_CONNECT_TIMEOUT",
	"CORS_ORIGINS", "AUTH_SECRET",
	"HTTP_BIND_ADDRESS", "APP_URL", "EMAIL_TRANSPORT",
	"SESSION_WEB_ABSOLUTE_LIFETIME", "SESSION_WEB_IDLE_TIMEOUT",
	"SESSION_MOBILE_ABSOLUTE_LIFETIME", "SESSION_MOBILE_IDLE_TIMEOUT",
	"SESSION_TOUCH_INTERVAL", "SESSION_SECURE_COOKIES",
	"PASSWORD_RESET_TTL", "EMAIL_VERIFICATION_TTL",
	"SUBSCRIPTION_MODE", "SUBSCRIPTION_PROMPTPAY_TARGET", "SUBSCRIPTION_PROMPTPAY_NAME",
	"SUBSCRIPTION_DEMO_TIER", "SUBSCRIPTION_DEMO_DURATION_DAYS",
}

// setEnv gives one test a hermetic environment: every config variable is
// cleared, then the required minimum plus kv is applied. t.Setenv restores the
// previous values when the test ends.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()

	for _, key := range configEnvVars {
		t.Setenv(key, "")
	}

	env := map[string]string{
		"APP_ENV":      config.EnvTest,
		"DATABASE_URL": "postgres://user:pass@localhost:5432/fictionthai_test?sslmode=disable",
		"CORS_ORIGINS": "http://localhost:3000",
	}
	for k, v := range kv {
		env[k] = v
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestLoad_Defaults(t *testing.T) {
	setEnv(t, nil)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HTTP.Port != 8080 {
		t.Errorf("port = %d, want 8080", cfg.HTTP.Port)
	}
	if cfg.HTTP.MaxRequestBytes != 1<<20 {
		t.Errorf("max request bytes = %d, want %d", cfg.HTTP.MaxRequestBytes, 1<<20)
	}
	if cfg.Redis.Enabled() {
		t.Error("Redis should be disabled when REDIS_URL is unset (docs/07 §18)")
	}
}

func TestLoad_RequiresDatabaseURL(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": ""})

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected an error when DATABASE_URL is missing")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error should name the missing variable, got: %v", err)
	}
}

// docs/11 §23: a wildcard CORS origin must never be accepted.
func TestLoad_RejectsWildcardCORSOrigin(t *testing.T) {
	setEnv(t, map[string]string{"CORS_ORIGINS": "*"})

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected a wildcard CORS origin to be rejected")
	}
}

func TestLoad_RejectsMalformedCORSOrigin(t *testing.T) {
	setEnv(t, map[string]string{"CORS_ORIGINS": "localhost:3000"})

	if _, err := config.Load(); err == nil {
		t.Fatal("expected an origin without a scheme to be rejected")
	}
}

func TestLoad_ParsesMultipleCORSOrigins(t *testing.T) {
	setEnv(t, map[string]string{
		"CORS_ORIGINS": "http://localhost:3000, https://www.fictionthai.test ",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"http://localhost:3000", "https://www.fictionthai.test"}
	if len(cfg.CORS.AllowedOrigins) != len(want) {
		t.Fatalf("origins = %v, want %v", cfg.CORS.AllowedOrigins, want)
	}
	for i, origin := range want {
		if cfg.CORS.AllowedOrigins[i] != origin {
			t.Errorf("origin[%d] = %q, want %q (whitespace should be trimmed)", i, cfg.CORS.AllowedOrigins[i], origin)
		}
	}
}

func TestLoad_RequiresAuthSecretOutsideDevelopment(t *testing.T) {
	setEnv(t, map[string]string{"APP_ENV": config.EnvProduction, "AUTH_SECRET": ""})

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected AUTH_SECRET to be required in production")
	}
	if !strings.Contains(err.Error(), "AUTH_SECRET") {
		t.Errorf("error should name AUTH_SECRET, got: %v", err)
	}
}

func TestLoad_RejectsUnknownEnvironment(t *testing.T) {
	setEnv(t, map[string]string{"APP_ENV": "prod"})

	if _, err := config.Load(); err == nil {
		t.Fatal(`expected APP_ENV="prod" to be rejected`)
	}
}

// A value that is SET but unparseable must fail loudly.
//
// Silently falling back is dangerous for a security setting: an operator who
// writes SESSION_WEB_IDLE_TIMEOUT=30d - natural, but not valid Go duration
// syntax - would otherwise get the 7-day default and no warning that their
// configuration was ignored.
func TestLoad_RejectsMalformedValues(t *testing.T) {
	tests := map[string]struct {
		key, value string
	}{
		"duration without a unit":  {"SESSION_WEB_IDLE_TIMEOUT", "30"},
		"duration with a day unit": {"SESSION_WEB_IDLE_TIMEOUT", "30d"},
		"non-numeric port":         {"APP_PORT", "eighty"},
		"non-boolean flag":         {"SESSION_SECURE_COOKIES", "yes-please"},
		"non-numeric byte limit":   {"HTTP_MAX_REQUEST_BYTES", "1MB"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setEnv(t, map[string]string{tc.key: tc.value})

			_, err := config.Load()
			if err == nil {
				t.Fatalf("%s=%q was silently ignored; it must fail loudly", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error should name %s, got: %v", tc.key, err)
			}
		})
	}
}

// An UNSET value must still fall back to its documented default.
func TestLoad_UnsetValuesUseDefaults(t *testing.T) {
	setEnv(t, nil)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Session.WebIdleTimeout != 7*24*time.Hour {
		t.Errorf("web idle timeout = %v, want 168h", cfg.Session.WebIdleTimeout)
	}
	if cfg.Session.WebAbsoluteLifetime != 14*24*time.Hour {
		t.Errorf("web absolute lifetime = %v, want 336h", cfg.Session.WebAbsoluteLifetime)
	}
	if cfg.Session.MobileAbsoluteLifetime != 90*24*time.Hour {
		t.Errorf("mobile absolute lifetime = %v, want 2160h", cfg.Session.MobileAbsoluteLifetime)
	}
}

// docs/10 §43: an authentication cookie must be Secure in production.
func TestLoad_RefusesInsecureCookiesOutsideDevelopment(t *testing.T) {
	setEnv(t, map[string]string{
		"APP_ENV":                config.EnvProduction,
		"AUTH_SECRET":            "a-secret",
		"EMAIL_TRANSPORT":        "discard",
		"APP_URL":                "https://fictionthai.test",
		"CORS_ORIGINS":           "https://fictionthai.test",
		"SESSION_SECURE_COOKIES": "false",
	})

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected insecure cookies to be rejected in production")
	}
	if !strings.Contains(err.Error(), "SESSION_SECURE_COOKIES") {
		t.Errorf("error should name the setting, got: %v", err)
	}
}

// The development mail transport writes single-use links into the log, so it
// must never be selectable outside development.
func TestLoad_RefusesLogEmailTransportOutsideDevelopment(t *testing.T) {
	setEnv(t, map[string]string{
		"APP_ENV":         config.EnvProduction,
		"AUTH_SECRET":     "a-secret",
		"APP_URL":         "https://fictionthai.test",
		"CORS_ORIGINS":    "https://fictionthai.test",
		"EMAIL_TRANSPORT": "log",
	})

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected EMAIL_TRANSPORT=log to be rejected in production")
	}
	if !strings.Contains(err.Error(), "EMAIL_TRANSPORT") {
		t.Errorf("error should name the setting, got: %v", err)
	}
}

// An idle window longer than the absolute cap can never fire, silently
// disabling idle expiry.
func TestLoad_RejectsIdleTimeoutBeyondAbsoluteLifetime(t *testing.T) {
	setEnv(t, map[string]string{"SESSION_WEB_IDLE_TIMEOUT": "720h"}) // 30 days > 14

	if _, err := config.Load(); err == nil {
		t.Fatal("expected an idle timeout beyond the absolute lifetime to be rejected")
	}
}

// A reset link is a bearer credential sitting in an inbox.
func TestLoad_RejectsExcessivePasswordResetTTL(t *testing.T) {
	setEnv(t, map[string]string{"PASSWORD_RESET_TTL": "72h"})

	if _, err := config.Load(); err == nil {
		t.Fatal("expected a password reset TTL beyond 24h to be rejected")
	}
}

// SUBSCRIPTION_MODE defaults to the safe "disabled" (demo-mode brief §16).
func TestLoad_SubscriptionModeDefaultsDisabled(t *testing.T) {
	setEnv(t, nil)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Subscription.Mode != config.SubscriptionModeDisabled {
		t.Errorf("subscription mode default = %q, want %q", cfg.Subscription.Mode, config.SubscriptionModeDisabled)
	}
}

// An unknown SUBSCRIPTION_MODE fails startup (demo-mode brief §16 - reject
// "true", "false", "production", anything not in the closed set).
func TestLoad_RejectsUnknownSubscriptionMode(t *testing.T) {
	for _, bad := range []string{"true", "false", "enabled", "production", "testing"} {
		t.Run(bad, func(t *testing.T) {
			setEnv(t, map[string]string{"SUBSCRIPTION_MODE": bad})
			_, err := config.Load()
			if err == nil {
				t.Fatalf("SUBSCRIPTION_MODE=%q must be rejected", bad)
			}
			if !strings.Contains(err.Error(), "SUBSCRIPTION_MODE") {
				t.Errorf("error should name SUBSCRIPTION_MODE, got: %v", err)
			}
		})
	}
}

// The three valid modes load cleanly.
func TestLoad_AcceptsValidSubscriptionModes(t *testing.T) {
	for _, mode := range []string{
		config.SubscriptionModeDisabled, config.SubscriptionModeDemo, config.SubscriptionModeLive,
	} {
		t.Run(mode, func(t *testing.T) {
			setEnv(t, map[string]string{"SUBSCRIPTION_MODE": mode})
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("SUBSCRIPTION_MODE=%q should load, got: %v", mode, err)
			}
			if cfg.Subscription.Mode != mode {
				t.Errorf("mode = %q, want %q", cfg.Subscription.Mode, mode)
			}
		})
	}
}

// Demo settings are validated only in demo mode.
func TestLoad_ValidatesDemoSettings(t *testing.T) {
	// A bad demo tier is rejected in demo mode.
	setEnv(t, map[string]string{
		"SUBSCRIPTION_MODE":      config.SubscriptionModeDemo,
		"SUBSCRIPTION_DEMO_TIER": "vip",
	})
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "SUBSCRIPTION_DEMO_TIER") {
		t.Fatalf("expected SUBSCRIPTION_DEMO_TIER=vip to be rejected in demo mode, got: %v", err)
	}

	// A zero duration is rejected in demo mode.
	setEnv(t, map[string]string{
		"SUBSCRIPTION_MODE":               config.SubscriptionModeDemo,
		"SUBSCRIPTION_DEMO_DURATION_DAYS": "0",
	})
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "SUBSCRIPTION_DEMO_DURATION_DAYS") {
		t.Fatalf("expected a zero demo duration to be rejected, got: %v", err)
	}

	// The SAME bad demo tier is ignored when NOT in demo mode (live) - demo
	// settings must not block a live deployment.
	setEnv(t, map[string]string{
		"SUBSCRIPTION_MODE":      config.SubscriptionModeLive,
		"SUBSCRIPTION_DEMO_TIER": "vip",
	})
	if _, err := config.Load(); err != nil {
		t.Fatalf("demo settings must not be validated outside demo mode, got: %v", err)
	}

	// A valid demo config loads and parses the duration.
	setEnv(t, map[string]string{
		"SUBSCRIPTION_MODE":               config.SubscriptionModeDemo,
		"SUBSCRIPTION_DEMO_TIER":          "premium",
		"SUBSCRIPTION_DEMO_DURATION_DAYS": "14",
	})
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("valid demo config should load, got: %v", err)
	}
	if cfg.Subscription.DemoTier != "premium" || cfg.Subscription.DemoDurationDays != 14 {
		t.Errorf("demo config = %q/%d, want premium/14", cfg.Subscription.DemoTier, cfg.Subscription.DemoDurationDays)
	}
}

// A startup log line must never expose the database password.
func TestRedacted_HidesCredentials(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL": "postgres://fictionthai:sup3rs3cret@db:5432/fictionthai",
		"REDIS_URL":    "redis://:red1ssecret@cache:6379/0",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	for key, value := range cfg.Redacted() {
		s, ok := value.(string)
		if !ok {
			continue
		}
		if strings.Contains(s, "sup3rs3cret") || strings.Contains(s, "red1ssecret") {
			t.Errorf("Redacted()[%q] leaked a credential: %q", key, s)
		}
	}
	if _, present := cfg.Redacted()["auth_secret"]; present {
		t.Error("Redacted() must not include the auth secret at all")
	}
}
