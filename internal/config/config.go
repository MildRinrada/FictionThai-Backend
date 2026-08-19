// Package config loads runtime configuration from environment variables.
//
// Secrets are never hard-coded and never defaulted to a usable value in
// production (docs/10 - Authentication & Authorization.md §44,
// docs/11 - Security & Privacy.md §42). Load fails loudly rather than starting
// a misconfigured server.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment names understood by the application.
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
	EnvTest        = "test"
)

// Config is the fully resolved application configuration.
type Config struct {
	App          App
	HTTP         HTTP
	Database     Database
	Redis        Redis
	CORS         CORS
	Auth         Auth
	Session      Session
	Email        Email
	Media        Media
	AI           AI
	Subscription Subscription
}

type App struct {
	Name     string
	Env      string
	LogLevel string
}

func (a App) IsProduction() bool  { return a.Env == EnvProduction }
func (a App) IsDevelopment() bool { return a.Env == EnvDevelopment }

type HTTP struct {
	Port int

	// BindAddress is the interface to listen on. Empty means all interfaces,
	// which is what a container or VM needs in order to be reachable.
	//
	// Set it to 127.0.0.1 to listen on loopback only - useful behind a
	// same-host reverse proxy, and what the tests use so that running the suite
	// does not trigger a host firewall prompt.
	BindAddress string

	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// MaxRequestBytes caps request bodies. docs/09 §37 requires request size
	// limits; media goes to object storage via signed URLs (docs/14 §19), so
	// the API itself never needs to accept large payloads.
	MaxRequestBytes int64
}

func (h HTTP) Addr() string { return h.BindAddress + ":" + strconv.Itoa(h.Port) }

type Database struct {
	URL             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
}

// Redis is optional during early development (docs/07 §18): when URL is empty
// the application runs without a cache and reports it as disabled in /ready.
type Redis struct {
	URL            string
	ConnectTimeout time.Duration
}

func (r Redis) Enabled() bool { return r.URL != "" }

// CORS holds the explicit origin allowlist. A wildcard is never permitted for
// credentialed requests (docs/11 §23).
type CORS struct {
	AllowedOrigins []string
}

// Auth holds authentication secrets.
//
// The session credential itself is an opaque random token, so no signing key is
// required to validate a session. AUTH_SECRET is retained for future signed
// artefacts (invite links, signed media URLs) and is still mandatory outside
// development so a deployment cannot reach production without one configured.
type Auth struct {
	Secret string
}

// Session configures session lifetime and cookie behaviour.
//
// Two lifetime policies, because the transports have different risk profiles: a
// browser is often a shared machine, whereas a phone is a personal device
// behind a device lock, so forcing a 14-day re-login on mobile would be
// friction without a matching security gain.
type Session struct {
	WebAbsoluteLifetime time.Duration
	WebIdleTimeout      time.Duration

	MobileAbsoluteLifetime time.Duration
	MobileIdleTimeout      time.Duration

	// TouchInterval is the minimum gap between `last_used_at` writes, so an
	// authenticated read does not become a write on every request.
	TouchInterval time.Duration

	PasswordResetTTL     time.Duration
	EmailVerificationTTL time.Duration

	// SecureCookies enables the Secure attribute and the `__Host-` cookie
	// prefix. Always true outside development - a session cookie without
	// Secure can be sent over plain HTTP (docs/10 §43).
	SecureCookies bool

	// AppURL is the public origin of the web app, used to build emailed links.
	AppURL string
}

// EmailTransport selects the outbound mail implementation.
type EmailTransport string

const (
	// EmailTransportLog writes messages to the application log instead of
	// sending them. Development only - the log line contains a single-use link.
	EmailTransportLog EmailTransport = "log"
	// EmailTransportDiscard drops messages; used by tests.
	EmailTransportDiscard EmailTransport = "discard"
)

// Email configures outbound mail. No production provider is integrated yet
// (docs/14 §64 keeps that choice replaceable).
type Email struct {
	Transport EmailTransport
}

// Media configures uploads and the storage backend (docs/08 §22, docs/11
// §28–§29). Only the local filesystem backend exists so far; an S3-compatible
// backend slots in behind the same storage.Store interface later
// (docs/07 §22 "The exact provider can change later").
type Media struct {
	// StoragePath is the local backend's base directory. Never a web-served
	// source directory (docs/11 §29).
	StoragePath string

	// MaxUploadBytes caps one uploaded file. The GLOBAL request-body limit
	// stays small; only the upload route raises it to this value.
	MaxUploadBytes int64

	// PublicBaseURL is the origin under which /media/{key} is reachable -
	// the URL written into avatar_url / cover_url. Defaults to the API's own
	// localhost origin; a deployment sets its public host (later, a CDN).
	PublicBaseURL string
}

// AI configures the optional AI / Thai NLP assistance (docs/12 §36, Phase 10).
// Everything AI is optional infrastructure: when disabled, or when a provider
// is unreachable, the rest of the platform keeps working (docs/12 §31).
type AI struct {
	// Enabled is the platform master switch. When false, AI endpoints return a
	// clean 503 and nothing else changes (docs/12 §31).
	Enabled bool

	// Provider selects the language backend. Only the local, dependency-free
	// Thai rule engine exists so far; a self-hosted model or an external LLM
	// slots in behind the same ai.Provider interface later (docs/12 §7, §26).
	Provider string

	// MaxInputRunes caps how much text is analyzed per request - a cost and
	// abuse control (docs/12 §29, docs/11 §53). Counted in runes: Thai per
	// character, never per byte.
	MaxInputRunes int

	// DailyQuota is the per-user budget of persisted AI requests per 24h
	// (docs/12 §29–§30). Enforced through the shared atomic rate limiter, so it
	// is race-safe; it is separate from the per-minute AI rate tier.
	DailyQuota int

	// ModelURL is the local consistency-model sidecar
	// (docs/AI-CONSISTENCY-MODEL.md). Empty (the default) runs the character
	// check on deterministic rules only; the sidecar being down degrades to
	// the same, never to an error.
	ModelURL string
}

// Subscription mode constants (SUBSCRIPTION_MODE). Kept here as plain strings so
// the config package stays free of a dependency on the subscriptions domain; the
// server wiring maps them onto subscriptions.Mode.
const (
	// SubscriptionModeDisabled: Premium/Pro not publicly available. The safe
	// default (demo-mode brief §16).
	SubscriptionModeDisabled = "disabled"
	// SubscriptionModeDemo: Premium/Pro offered as a free launch demo.
	SubscriptionModeDemo = "demo"
	// SubscriptionModeLive: real paid PromptPay subscriptions.
	SubscriptionModeLive = "live"
)

// Subscription configures the platform-owned Premium subscription feature
// (docs/MONETIZATION.md, Phase 11; demo-mode brief). A single environment switch,
// SUBSCRIPTION_MODE, moves the platform between three states without a code
// change (brief §3, §25):
//
//	disabled  Premium/Pro is off. Pricing may still be browsed ("coming soon");
//	          nothing is purchasable or activatable. The SAFE default (brief §16).
//	demo      Premium/Pro is offered as a FREE launch trial - a demo entitlement,
//	          no payment, no slip, no verification (brief §2, §4).
//	live      real paid subscriptions via PromptPay + manual slip verification.
//
// Prices live in the database (subscription_plans), never here. NO payment-provider
// secret, card, or bank field belongs in this struct - Phase 1 is PromptPay +
// manual verification, and a Phase 2 gateway keeps its secret in its own config
// when actually selected (payment_provider: OPEN).
type Subscription struct {
	// Mode is disabled | demo | live. Invalid values fail startup (brief §16).
	Mode string

	// PromptPayTarget is the platform's PromptPay id (a phone number, national
	// id, or e-wallet id) that the checkout QR pays into. This is the PLATFORM's
	// receiving account - never a user's. When empty, live checkout still works
	// but returns payment instructions without a generated QR payload, so the app
	// runs in development without a real PromptPay id configured. Live mode only.
	PromptPayTarget string

	// PromptPayName is the merchant display name embedded in the QR (EMVCo tag
	// 59). Cosmetic.
	PromptPayName string

	// DemoTier is the tier a free demo grants: "premium" or "pro" (brief §5).
	// Demo mode only.
	DemoTier string

	// DemoDurationDays is how long a free demo lasts, in days (brief §5). Demo
	// mode only.
	DemoDurationDays int
}

// Load reads configuration from the process environment and validates it.
func Load() (*Config, error) {
	// Resolved first: several defaults below are environment-dependent, and a
	// setting that silently weakens security in production because APP_ENV was
	// read too late would be a nasty failure mode.
	resetParseErrors()

	env := getString("APP_ENV", EnvDevelopment)
	isLocal := env == EnvDevelopment || env == EnvTest

	cfg := &Config{
		App: App{
			Name:     getString("APP_NAME", "fictionthai-api"),
			Env:      env,
			LogLevel: getString("LOG_LEVEL", "info"),
		},
		HTTP: HTTP{
			Port:            getInt("APP_PORT", 8080),
			BindAddress:     getString("HTTP_BIND_ADDRESS", ""),
			ReadTimeout:     getDuration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    getDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:     getDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: getDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
			MaxRequestBytes: int64(getInt("HTTP_MAX_REQUEST_BYTES", 1<<20)), // 1 MiB
		},
		Database: Database{
			URL:             getString("DATABASE_URL", ""),
			MaxOpenConns:    getInt("DATABASE_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    getInt("DATABASE_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: getDuration("DATABASE_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnectTimeout:  getDuration("DATABASE_CONNECT_TIMEOUT", 10*time.Second),
		},
		Redis: Redis{
			URL:            getString("REDIS_URL", ""),
			ConnectTimeout: getDuration("REDIS_CONNECT_TIMEOUT", 5*time.Second),
		},
		CORS: CORS{
			AllowedOrigins: getCSV("CORS_ORIGINS", []string{"http://localhost:3000"}),
		},
		Auth: Auth{
			Secret: getString("AUTH_SECRET", ""),
		},
		Session: Session{
			WebAbsoluteLifetime:    getDuration("SESSION_WEB_ABSOLUTE_LIFETIME", 14*24*time.Hour),
			WebIdleTimeout:         getDuration("SESSION_WEB_IDLE_TIMEOUT", 7*24*time.Hour),
			MobileAbsoluteLifetime: getDuration("SESSION_MOBILE_ABSOLUTE_LIFETIME", 90*24*time.Hour),
			MobileIdleTimeout:      getDuration("SESSION_MOBILE_IDLE_TIMEOUT", 30*24*time.Hour),
			TouchInterval:          getDuration("SESSION_TOUCH_INTERVAL", time.Hour),
			PasswordResetTTL:       getDuration("PASSWORD_RESET_TTL", time.Hour),
			EmailVerificationTTL:   getDuration("EMAIL_VERIFICATION_TTL", 48*time.Hour),
			// Secure by default everywhere except local development, where
			// there is no TLS to carry the cookie.
			SecureCookies: getBool("SESSION_SECURE_COOKIES", !isLocal),
			AppURL:        getString("APP_URL", "http://localhost:3000"),
		},
		Email: Email{
			Transport: EmailTransport(getString("EMAIL_TRANSPORT", string(EmailTransportLog))),
		},
	}

	cfg.Media = Media{
		StoragePath:    getString("MEDIA_STORAGE_PATH", "./data/media"),
		MaxUploadBytes: int64(getInt("MEDIA_MAX_UPLOAD_BYTES", 5<<20)), // 5 MiB
		PublicBaseURL: getString("MEDIA_PUBLIC_BASE_URL",
			fmt.Sprintf("http://localhost:%d", cfg.HTTP.Port)),
	}

	cfg.AI = AI{
		Enabled:       getBool("AI_ENABLED", true),
		Provider:      getString("AI_PROVIDER", "local"),
		MaxInputRunes: getInt("AI_MAX_INPUT_RUNES", 20000),
		DailyQuota:    getInt("AI_DAILY_REQUEST_QUOTA", 50),
		ModelURL:      getString("AI_MODEL_URL", ""),
	}

	cfg.Subscription = Subscription{
		// Default disabled: the safest production posture (brief §16). A demo or
		// live deployment sets SUBSCRIPTION_MODE explicitly.
		Mode:             getString("SUBSCRIPTION_MODE", SubscriptionModeDisabled),
		PromptPayTarget:  getString("SUBSCRIPTION_PROMPTPAY_TARGET", ""),
		PromptPayName:    getString("SUBSCRIPTION_PROMPTPAY_NAME", "FICTIONTHAI"),
		DemoTier:         getString("SUBSCRIPTION_DEMO_TIER", "pro"),
		DemoDurationDays: getInt("SUBSCRIPTION_DEMO_DURATION_DAYS", 30),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	// Malformed values first: they explain why a later check may look wrong.
	problems := takeParseErrors()

	switch c.App.Env {
	case EnvDevelopment, EnvStaging, EnvProduction, EnvTest:
	default:
		problems = append(problems, fmt.Sprintf(
			"APP_ENV %q is not one of: %s, %s, %s, %s",
			c.App.Env, EnvDevelopment, EnvStaging, EnvProduction, EnvTest))
	}

	if c.HTTP.Port < 1 || c.HTTP.Port > 65535 {
		problems = append(problems, fmt.Sprintf("APP_PORT %d is out of range", c.HTTP.Port))
	}
	if c.HTTP.MaxRequestBytes < 1024 {
		problems = append(problems, "HTTP_MAX_REQUEST_BYTES must be at least 1024")
	}

	if c.Database.URL == "" {
		problems = append(problems, "DATABASE_URL is required")
	} else if _, err := url.Parse(c.Database.URL); err != nil {
		problems = append(problems, "DATABASE_URL is not a valid URL")
	}
	if c.Database.MaxIdleConns > c.Database.MaxOpenConns {
		problems = append(problems, "DATABASE_MAX_IDLE_CONNS must not exceed DATABASE_MAX_OPEN_CONNS")
	}

	if c.Redis.Enabled() {
		if _, err := url.Parse(c.Redis.URL); err != nil {
			problems = append(problems, "REDIS_URL is not a valid URL")
		}
	}

	if len(c.CORS.AllowedOrigins) == 0 {
		problems = append(problems, "CORS_ORIGINS must list at least one trusted origin")
	}
	for _, origin := range c.CORS.AllowedOrigins {
		// docs/11 §23: a wildcard origin is not acceptable for an API that
		// carries private user data.
		if origin == "*" {
			problems = append(problems, `CORS_ORIGINS must not contain "*"`)
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" {
			problems = append(problems, fmt.Sprintf("CORS origin %q must be an absolute URL such as https://example.com", origin))
		}
	}

	// Secrets are only mandatory outside development, so a fresh clone can boot
	// with `docker compose up` and no secret ceremony.
	isLocal := c.App.IsDevelopment() || c.App.Env == EnvTest
	if !isLocal && c.Auth.Secret == "" {
		problems = append(problems, "AUTH_SECRET is required outside development")
	}

	problems = append(problems, c.validateSession(isLocal)...)
	problems = append(problems, c.validateEmail(isLocal)...)

	if c.Media.StoragePath == "" {
		problems = append(problems, "MEDIA_STORAGE_PATH is required")
	}
	if c.Media.MaxUploadBytes < 1024 {
		problems = append(problems, "MEDIA_MAX_UPLOAD_BYTES must be at least 1024")
	}
	if u, err := url.Parse(c.Media.PublicBaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		problems = append(problems, "MEDIA_PUBLIC_BASE_URL must be an absolute URL")
	}

	// AI settings only need to be valid when AI is switched on - a deployment
	// that turns AI off must not be blocked by AI configuration (docs/12 §31).
	if c.AI.Enabled {
		if c.AI.Provider != "local" {
			problems = append(problems,
				`AI_PROVIDER must be "local" - the only AI provider implemented so far`)
		}
		if c.AI.MaxInputRunes < 100 {
			problems = append(problems, "AI_MAX_INPUT_RUNES must be at least 100")
		}
		if c.AI.DailyQuota < 1 {
			problems = append(problems, "AI_DAILY_REQUEST_QUOTA must be at least 1")
		}
	}

	problems = append(problems, c.validateSubscription()...)

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// validateSession checks session lifetime and cookie settings.
func (c *Config) validateSession(isLocal bool) []string {
	var problems []string
	s := c.Session

	for name, d := range map[string]time.Duration{
		"SESSION_WEB_ABSOLUTE_LIFETIME":    s.WebAbsoluteLifetime,
		"SESSION_WEB_IDLE_TIMEOUT":         s.WebIdleTimeout,
		"SESSION_MOBILE_ABSOLUTE_LIFETIME": s.MobileAbsoluteLifetime,
		"SESSION_MOBILE_IDLE_TIMEOUT":      s.MobileIdleTimeout,
		"PASSWORD_RESET_TTL":               s.PasswordResetTTL,
		"EMAIL_VERIFICATION_TTL":           s.EmailVerificationTTL,
	} {
		if d <= 0 {
			problems = append(problems, name+" must be greater than zero")
		}
	}

	// An idle window longer than the absolute cap can never fire, which would
	// silently disable idle expiry.
	if s.WebIdleTimeout > s.WebAbsoluteLifetime {
		problems = append(problems,
			"SESSION_WEB_IDLE_TIMEOUT must not exceed SESSION_WEB_ABSOLUTE_LIFETIME")
	}
	if s.MobileIdleTimeout > s.MobileAbsoluteLifetime {
		problems = append(problems,
			"SESSION_MOBILE_IDLE_TIMEOUT must not exceed SESSION_MOBILE_ABSOLUTE_LIFETIME")
	}

	// A password-reset link is a bearer credential sitting in an inbox; a long
	// TTL widens the window in which a leaked mailbox yields an account.
	if s.PasswordResetTTL > 24*time.Hour {
		problems = append(problems, "PASSWORD_RESET_TTL must not exceed 24h")
	}

	// docs/10 §43: authentication cookies must be Secure in production. Allowing
	// this to be switched off outside development would let a deployment ship a
	// session cookie that travels over plain HTTP.
	if !isLocal && !s.SecureCookies {
		problems = append(problems,
			"SESSION_SECURE_COOKIES must not be disabled outside development")
	}

	if u, err := url.Parse(s.AppURL); err != nil || u.Scheme == "" || u.Host == "" {
		problems = append(problems,
			"APP_URL must be an absolute URL such as https://fictionthai.com")
	}

	return problems
}

// validateEmail checks the outbound mail configuration.
func (c *Config) validateEmail(isLocal bool) []string {
	var problems []string

	switch c.Email.Transport {
	case EmailTransportLog, EmailTransportDiscard:
	default:
		problems = append(problems, fmt.Sprintf(
			"EMAIL_TRANSPORT %q is not one of: %s, %s",
			c.Email.Transport, EmailTransportLog, EmailTransportDiscard))
	}

	// The log transport writes single-use reset links into the application log.
	// That is the point in development and unacceptable anywhere else, so the
	// application refuses to start rather than quietly logging credentials.
	if !isLocal && c.Email.Transport == EmailTransportLog {
		problems = append(problems,
			"EMAIL_TRANSPORT=log writes password reset links to the log and must not be used outside development; "+
				"configure a real provider before deploying")
	}

	return problems
}

// validateSubscription checks the Premium mode configuration. An invalid
// SUBSCRIPTION_MODE fails startup (demo-mode brief §16), and the mode-specific
// settings are validated only when that mode is active, mirroring AI: a
// deployment that leaves Premium off must not be blocked by demo/live settings.
func (c *Config) validateSubscription() []string {
	var problems []string
	s := c.Subscription

	switch s.Mode {
	case SubscriptionModeDisabled, SubscriptionModeDemo, SubscriptionModeLive:
	default:
		problems = append(problems, fmt.Sprintf(
			"SUBSCRIPTION_MODE %q is not one of: %s, %s, %s",
			s.Mode, SubscriptionModeDisabled, SubscriptionModeDemo, SubscriptionModeLive))
		// The remaining checks assume a known mode; the shape checks below still
		// run and are harmless, but bail on the demo/live branches.
	}

	enabled := s.Mode == SubscriptionModeDemo || s.Mode == SubscriptionModeLive
	if enabled {
		// The merchant name rides in EMVCo QR tag 59, bounded at 25 characters.
		// An over-long name would produce an invalid QR.
		if len(s.PromptPayName) > 25 {
			problems = append(problems, "SUBSCRIPTION_PROMPTPAY_NAME must be at most 25 characters")
		}
		// The target is optional (empty → no QR payload), but when set it must be
		// digits only: a PromptPay id is a phone number, national id, or e-wallet
		// id. A shape check, not provider selection (payment_provider stays OPEN -
		// docs/MONETIZATION.md §6).
		if t := s.PromptPayTarget; t != "" {
			for _, r := range t {
				if r < '0' || r > '9' {
					problems = append(problems,
						"SUBSCRIPTION_PROMPTPAY_TARGET must contain only digits (a phone number, national id, or e-wallet id)")
					break
				}
			}
		}
	}

	// Demo settings only matter in demo mode.
	if s.Mode == SubscriptionModeDemo {
		if s.DemoTier != "premium" && s.DemoTier != "pro" {
			problems = append(problems, `SUBSCRIPTION_DEMO_TIER must be "premium" or "pro"`)
		}
		if s.DemoDurationDays < 1 {
			problems = append(problems, "SUBSCRIPTION_DEMO_DURATION_DAYS must be at least 1")
		}
		// A guard against a fat-fingered value granting a decade-long "trial".
		if s.DemoDurationDays > 3650 {
			problems = append(problems, "SUBSCRIPTION_DEMO_DURATION_DAYS must be at most 3650")
		}
	}

	return problems
}

// Redacted returns a summary safe to log at startup. It deliberately omits
// credentials - docs/07 §48 forbids logging secrets.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"app_name":          c.App.Name,
		"env":               c.App.Env,
		"log_level":         c.App.LogLevel,
		"port":              c.HTTP.Port,
		"database":          redactURL(c.Database.URL),
		"redis":             redactURL(c.Redis.URL),
		"redis_enabled":     c.Redis.Enabled(),
		"cors_origins":      c.CORS.AllowedOrigins,
		"max_request_bytes": c.HTTP.MaxRequestBytes,
		// The monetization mode is operationally useful and not a secret. The
		// PromptPay target (the platform's receiving id) is deliberately NOT
		// logged.
		"subscription_mode": c.Subscription.Mode,
	}
}

// redactURL strips userinfo (and therefore the password) from a connection
// string so it can be logged.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.Redacted()
}

var errEmpty = errors.New("empty")

func getString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) int {
	v, err := lookup(key)
	if err != nil {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		recordParseError(key, v, "an integer")
		return fallback
	}
	return n
}

func getBool(key string, fallback bool) bool {
	v, err := lookup(key)
	if err != nil {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		recordParseError(key, v, "a boolean (true or false)")
		return fallback
	}
	return parsed
}

func getDuration(key string, fallback time.Duration) time.Duration {
	v, err := lookup(key)
	if err != nil {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Go durations have no day unit, so "30d" is a natural thing to write
		// and a silent fallback would hand the operator 7 days while they
		// believed they had configured 30.
		recordParseError(key, v, `a duration such as 30s, 15m, 24h, or 720h (Go has no "d" unit)`)
		return fallback
	}
	return d
}

func getCSV(key string, fallback []string) []string {
	v, err := lookup(key)
	if err != nil {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// parseErrors collects malformed environment values seen during one Load.
//
// A SET-but-unparseable value must never fall back silently: an operator who
// writes SESSION_WEB_IDLE_TIMEOUT=30d would otherwise get the 7-day default and
// no indication that their setting was ignored. An UNSET value still falls back
// to its documented default, which is the intended behaviour.
//
// Load is called once at startup, so a package-level accumulator is adequate;
// resetParseErrors clears it at the start of each Load for tests.
var parseErrors []string

func recordParseError(key, value, expected string) {
	parseErrors = append(parseErrors,
		fmt.Sprintf("%s=%q is not %s", key, value, expected))
}

func resetParseErrors() { parseErrors = nil }

func takeParseErrors() []string {
	out := parseErrors
	parseErrors = nil
	return out
}

func lookup(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return "", errEmpty
	}
	return v, nil
}
