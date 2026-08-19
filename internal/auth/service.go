package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/platform/email"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Errors the service returns to its handler. They carry no detail about which
// part of a credential was wrong (docs/10 §10, §39).
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrAccountUnavailable = errors.New("account unavailable")
)

// Config holds the service's tunables. Everything here is configuration rather
// than a constant, per the Phase 1 requirement that lifetimes not be magic
// numbers.
type Config struct {
	WebLifetime    Lifetime
	MobileLifetime Lifetime

	PasswordParams PasswordParams

	PasswordResetTTL     time.Duration
	EmailVerificationTTL time.Duration

	// AppURL is the public origin of the web app, used to build the links sent
	// by email.
	AppURL string

	// TouchInterval is the minimum time between session `last_used_at` writes.
	// Without it every authenticated read would also be a write.
	TouchInterval time.Duration
}

// Lifetime returns the policy for a client kind.
func (c Config) lifetimeFor(kind ClientKind) Lifetime {
	if kind == ClientMobile {
		return c.MobileLifetime
	}
	return c.WebLifetime
}

// Service owns authentication business rules.
//
// Handlers call it; it calls repositories. Ownership checks for *content* do
// not live here - they belong to the service that owns the resource
// (docs/10 §27).
type Service struct {
	users    *users.Repository
	sessions *SessionRepository
	tokens   *TokenRepository
	mailer   email.Sender
	log      *slog.Logger
	cfg      Config
}

func NewService(
	userRepo *users.Repository,
	sessionRepo *SessionRepository,
	tokenRepo *TokenRepository,
	mailer email.Sender,
	log *slog.Logger,
	cfg Config,
) *Service {
	return &Service{
		users: userRepo, sessions: sessionRepo, tokens: tokenRepo,
		mailer: mailer, log: log, cfg: cfg,
	}
}

// Credentials describe a login or registration attempt.
type RegisterParams struct {
	Username   string
	Email      string
	Password   string
	ClientKind ClientKind
	UserAgent  string
	IP         string
}

type LoginParams struct {
	Identifier string
	Password   string
	ClientKind ClientKind
	UserAgent  string
	IP         string
}

// Authentication is the result of a successful register or login.
//
// RawToken is the only moment the token exists outside the client. The handler
// decides how to deliver it: a cookie for web, the response body for native.
type Authentication struct {
	User     *users.User
	Session  *Session
	RawToken string
}

// Register creates an account and signs the new user in.
func (s *Service) Register(ctx context.Context, params RegisterParams) (*Authentication, error) {
	username := users.NormalizeUsername(params.Username)
	emailAddr := users.NormalizeEmail(params.Email)

	fields := map[string][]string{}
	if msg := users.ValidateUsername(username); msg != "" {
		fields["username"] = []string{msg}
	}
	if msg := users.ValidateEmail(emailAddr); msg != "" {
		fields["email"] = []string{msg}
	}
	if msg := users.ValidatePassword(params.Password); msg != "" {
		fields["password"] = []string{msg}
	}
	if len(fields) > 0 {
		return nil, apierror.Validation(fields)
	}

	hash, err := HashPassword(params.Password, s.cfg.PasswordParams)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.users.Create(ctx, users.CreateParams{
		Username:     username,
		Email:        emailAddr,
		PasswordHash: hash,
	})
	if err != nil {
		switch {
		case errors.Is(err, users.ErrUsernameTaken):
			// A username is public - /author/{username} already reveals whether
			// it is taken - so naming the conflict costs nothing and saves the
			// user a guess.
			return nil, apierror.Validation(map[string][]string{
				"username": {"This username is not available."},
			})
		case errors.Is(err, users.ErrEmailRegistered):
			// An email address is NOT public. Saying "already registered" would
			// turn registration into an account-existence oracle, which
			// docs/11 §27 requires preventing. The caller gets the same generic
			// conflict either way, and the real owner is told by email.
			s.sendEmailAlreadyRegisteredNotice(ctx, emailAddr)
			return nil, apierror.New(409, apierror.CodeConflict,
				"We could not complete registration with those details.")
		}
		return nil, err
	}

	s.issueEmailVerification(ctx, user)

	auth, err := s.startSession(ctx, user, params.ClientKind, params.UserAgent, params.IP)
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "user registered",
		slog.String("event", "auth_register"),
		slog.String("user_id", user.ID.String()),
		slog.String("client_kind", string(params.ClientKind)),
	)
	return auth, nil
}

// Login verifies credentials and starts a session.
func (s *Service) Login(ctx context.Context, params LoginParams) (*Authentication, error) {
	user, err := s.users.FindByIdentifier(ctx, params.Identifier)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			// Spend the same time as a real verification so response timing
			// cannot be used to enumerate accounts (docs/10 §39).
			BurnPasswordTime(params.Password)
			s.logFailedLogin(ctx, "unknown_identifier", params.IP)
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	match, err := VerifyPassword(params.Password, user.PasswordHash)
	if err != nil {
		// A malformed stored hash is an operational fault, not a user error.
		s.log.ErrorContext(ctx, "stored password hash could not be parsed",
			slog.String("user_id", user.ID.String()), slog.Any("error", err))
		return nil, ErrInvalidCredentials
	}
	if !match {
		s.logFailedLogin(ctx, "bad_password", params.IP)
		return nil, ErrInvalidCredentials
	}

	// Checked AFTER the password so a suspended account is not distinguishable
	// from a wrong password by an attacker who does not know the password.
	if !user.Active() {
		s.logFailedLogin(ctx, "account_unavailable", params.IP)
		return nil, ErrAccountUnavailable
	}

	auth, err := s.startSession(ctx, user, params.ClientKind, params.UserAgent, params.IP)
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "login succeeded",
		slog.String("event", "auth_login"),
		slog.String("user_id", user.ID.String()),
		slog.String("client_kind", string(params.ClientKind)),
	)
	return auth, nil
}

// startSession mints a token and persists the session.
//
// A fresh session row per login is what protects against session fixation
// (docs/11 §6): an identifier supplied before authentication is never adopted.
func (s *Service) startSession(
	ctx context.Context,
	user *users.User,
	kind ClientKind,
	userAgent, ip string,
) (*Authentication, error) {
	raw, digest, err := GenerateToken()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	lifetime := s.cfg.lifetimeFor(kind)

	session, err := s.sessions.Create(ctx, CreateSessionParams{
		UserID:     user.ID,
		TokenHash:  digest,
		ClientKind: kind,
		ExpiresAt:  lifetime.Expiry(now, now),
		UserAgent:  userAgent,
		IP:         ip,
	})
	if err != nil {
		return nil, err
	}

	return &Authentication{User: user, Session: session, RawToken: raw}, nil
}

// Authenticate resolves a raw token into an Identity.
//
// This is the SINGLE validation path required by the Phase 1 brief - the
// middleware is its only caller, and no handler re-implements any part of it.
//
//	token → digest → session → revoked? → expired? → invalidated-before?
//	      → user → account status → Identity
//
// It returns (nil, nil) for every rejection. A caller cannot distinguish an
// expired session from a forged token, and must not: both simply mean "not
// authenticated".
func (s *Service) Authenticate(ctx context.Context, rawToken string) (*Identity, error) {
	if rawToken == "" {
		return nil, nil
	}

	session, err := s.sessions.FindByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return nil, nil
		}
		return nil, err
	}

	now := time.Now()
	if !session.Active(now) {
		return nil, nil
	}

	user, err := s.users.FindByID(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	// Bulk invalidation: a session created at or before the cutoff is dead,
	// without having had to touch its row (docs/10 §37).
	if cutoff := user.SessionsInvalidatedBefore; cutoff != nil && !session.CreatedAt.After(*cutoff) {
		return nil, nil
	}

	if !user.Active() {
		// Suspended, banned, or deleted since the session was issued.
		return nil, nil
	}

	s.touch(ctx, session, now)

	return &Identity{User: user, Session: session}, nil
}

// touch slides the idle window, but only when enough time has passed to make
// the write worthwhile.
func (s *Service) touch(ctx context.Context, session *Session, now time.Time) {
	if now.Sub(session.LastUsedAt) < s.cfg.TouchInterval {
		return
	}

	lifetime := s.cfg.lifetimeFor(session.ClientKind)
	expiry := lifetime.Expiry(session.CreatedAt, now)

	if err := s.sessions.Touch(ctx, session.ID, now, expiry); err != nil {
		// Never fail a request because the activity timestamp could not be
		// updated - the session is still valid.
		s.log.WarnContext(ctx, "could not touch session", slog.Any("error", err))
		return
	}
	session.LastUsedAt = now
	session.ExpiresAt = expiry
}

// Logout ends one session, leaving the user's other devices signed in.
func (s *Service) Logout(ctx context.Context, session *Session) error {
	if session == nil {
		return nil
	}
	if err := s.sessions.Revoke(ctx, session.ID); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "logout",
		slog.String("event", "auth_logout"),
		slog.String("user_id", session.UserID.String()),
	)
	return nil
}

// LogoutAll ends every session for a user, on every device.
//
// Both steps matter: the cutoff makes validation reject old sessions in O(1),
// and revoking the rows means a future device list shows them as ended rather
// than silently still present (docs/10 §37).
func (s *Service) LogoutAll(ctx context.Context, userID uuid.UUID) (int64, error) {
	if err := s.users.InvalidateSessionsBefore(ctx, userID, time.Now()); err != nil {
		return 0, err
	}
	revoked, err := s.sessions.RevokeAllForUser(ctx, userID)
	if err != nil {
		return 0, err
	}

	s.log.InfoContext(ctx, "logout all devices",
		slog.String("event", "auth_logout_all"),
		slog.String("user_id", userID.String()),
		slog.Int64("sessions_revoked", revoked),
	)
	return revoked, nil
}

// RequestPasswordReset issues a reset link.
//
// It ALWAYS reports success. Revealing whether an address is registered would
// turn this endpoint into an account-existence oracle (docs/10 §16, §39,
// docs/11 §26, §27).
func (s *Service) RequestPasswordReset(ctx context.Context, emailAddr string) {
	normalized := users.NormalizeEmail(emailAddr)
	if msg := users.ValidateEmail(normalized); msg != "" {
		return
	}

	user, err := s.users.FindByEmail(ctx, normalized)
	if err != nil {
		if !errors.Is(err, users.ErrNotFound) {
			s.log.ErrorContext(ctx, "password reset lookup failed", slog.Any("error", err))
		}
		return
	}

	raw, digest, err := GenerateToken()
	if err != nil {
		s.log.ErrorContext(ctx, "could not generate reset token", slog.Any("error", err))
		return
	}

	if _, err := s.tokens.Create(ctx, PurposePasswordReset, user.ID, digest,
		time.Now().Add(s.cfg.PasswordResetTTL)); err != nil {
		s.log.ErrorContext(ctx, "could not store reset token", slog.Any("error", err))
		return
	}

	// The RAW token goes only into the email. It is never logged and never
	// returned by the API (docs/10 §16).
	s.send(ctx, email.Message{
		To:      user.Email,
		Subject: "รีเซ็ตรหัสผ่าน FictionThai",
		Body: fmt.Sprintf(
			"เปิดลิงก์นี้เพื่อตั้งรหัสผ่านใหม่ (ลิงก์หมดอายุใน %s):\n\n%s\n\n"+
				"หากคุณไม่ได้ขอรีเซ็ตรหัสผ่าน คุณไม่ต้องดำเนินการใด ๆ",
			s.cfg.PasswordResetTTL, s.link("/reset-password", raw)),
	})

	s.log.InfoContext(ctx, "password reset requested",
		slog.String("event", "auth_password_reset_requested"),
		slog.String("user_id", user.ID.String()),
	)
}

// ResetPassword redeems a reset token and sets a new password.
//
// Every existing session is invalidated: a reset is the remedy for a
// compromised account, so leaving the attacker's session alive would defeat it
// (docs/10 §16, §37).
func (s *Service) ResetPassword(ctx context.Context, rawToken, newPassword string) error {
	if msg := users.ValidatePassword(newPassword); msg != "" {
		return apierror.Validation(map[string][]string{"password": {msg}})
	}

	token, err := s.tokens.Consume(ctx, PurposePasswordReset, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			return apierror.BadRequest("This reset link is invalid or has expired.")
		}
		return err
	}

	hash, err := HashPassword(newPassword, s.cfg.PasswordParams)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	// UpdatePassword also advances sessions_invalidated_before in the same
	// statement, so the two can never diverge.
	if err := s.users.UpdatePassword(ctx, token.UserID, hash); err != nil {
		return err
	}
	if _, err := s.sessions.RevokeAllForUser(ctx, token.UserID); err != nil {
		return err
	}

	s.log.InfoContext(ctx, "password reset completed",
		slog.String("event", "auth_password_reset"),
		slog.String("user_id", token.UserID.String()),
	)
	return nil
}

// VerifyEmail redeems an email-verification token.
func (s *Service) VerifyEmail(ctx context.Context, rawToken string) error {
	token, err := s.tokens.Consume(ctx, PurposeEmailVerification, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			return apierror.BadRequest("This verification link is invalid or has expired.")
		}
		return err
	}

	if err := s.users.MarkEmailVerified(ctx, token.UserID); err != nil {
		return err
	}

	s.log.InfoContext(ctx, "email verified",
		slog.String("event", "auth_email_verified"),
		slog.String("user_id", token.UserID.String()),
	)
	return nil
}

// issueEmailVerification sends a verification link. Failure is logged but never
// blocks registration - the account exists and can read immediately
// (docs/10 §17).
func (s *Service) issueEmailVerification(ctx context.Context, user *users.User) {
	raw, digest, err := GenerateToken()
	if err != nil {
		s.log.ErrorContext(ctx, "could not generate verification token", slog.Any("error", err))
		return
	}

	if _, err := s.tokens.Create(ctx, PurposeEmailVerification, user.ID, digest,
		time.Now().Add(s.cfg.EmailVerificationTTL)); err != nil {
		s.log.ErrorContext(ctx, "could not store verification token", slog.Any("error", err))
		return
	}

	s.send(ctx, email.Message{
		To:      user.Email,
		Subject: "ยืนยันอีเมล FictionThai",
		Body: fmt.Sprintf(
			"ยินดีต้อนรับสู่ FictionThai\n\nเปิดลิงก์นี้เพื่อยืนยันอีเมลของคุณ:\n\n%s\n\n"+
				"คุณสามารถอ่านนิยายได้ทันทีโดยไม่ต้องยืนยันอีเมล "+
				"การยืนยันจำเป็นเมื่อคุณต้องการเผยแพร่ผลงาน",
			s.link("/verify-email", raw)),
	})
}

// sendEmailAlreadyRegisteredNotice tells the real account owner that someone
// tried to register with their address.
//
// This is how the flow stays useful without leaking: the person at the keyboard
// learns nothing, while the actual owner - who already knows they have an
// account - gets a security notice.
func (s *Service) sendEmailAlreadyRegisteredNotice(ctx context.Context, emailAddr string) {
	s.send(ctx, email.Message{
		To:      emailAddr,
		Subject: "มีความพยายามสมัครสมาชิกด้วยอีเมลของคุณ",
		Body: "มีผู้พยายามสมัครสมาชิก FictionThai ด้วยอีเมลนี้ ซึ่งมีบัญชีอยู่แล้ว\n\n" +
			"หากเป็นคุณ กรุณาเข้าสู่ระบบตามปกติ หรือใช้ลิงก์ลืมรหัสผ่าน\n" +
			"หากไม่ใช่คุณ คุณไม่ต้องดำเนินการใด ๆ",
	})
}

func (s *Service) send(ctx context.Context, msg email.Message) {
	// Delivery must not fail the caller's operation: the response for a reset
	// request has to look identical whether or not an account exists.
	if err := s.mailer.Send(ctx, msg); err != nil {
		s.log.ErrorContext(ctx, "email delivery failed",
			slog.String("subject", msg.Subject), slog.Any("error", err))
	}
}

// link builds an absolute URL carrying a single-use token.
func (s *Service) link(path, token string) string {
	return fmt.Sprintf("%s%s?token=%s", s.cfg.AppURL, path, url.QueryEscape(token))
}

// logFailedLogin records an attempt without revealing which account was
// targeted or what was supplied (docs/10 §45).
func (s *Service) logFailedLogin(ctx context.Context, reason, ip string) {
	attrs := []any{
		slog.String("event", "auth_login_failed"),
		slog.String("reason", reason),
	}
	if prefix := TruncateIP(ip); prefix != nil {
		attrs = append(attrs, slog.String("ip_prefix", *prefix))
	}
	s.log.WarnContext(ctx, "login failed", attrs...)
}

// AttestAdult records the account's one-time statement that it belongs to an
// adult (docs/PHASE-13-CREATION-AND-CONTROL.md §13B).
//
// It is asked ONCE, at the profile, and is a precondition for publishing 18+
// work - never for reading it, and never for anything else an account does.
// What is stored is a timestamp: no date of birth, no document, no third party
// (docs/11 §34). Re-attesting keeps the first date rather than moving it.
//
// There is deliberately no way to un-attest through the API. "I am not an
// adult" is not an edit to a profile field; an account that says so belongs in
// support, not in a toggle that could be flipped back a second later.
func (s *Service) AttestAdult(ctx context.Context, userID uuid.UUID) error {
	if err := s.users.MarkAdultAttested(ctx, userID); err != nil {
		s.log.ErrorContext(ctx, "adult attestation failed", slog.Any("error", err))
		return apierror.Internal()
	}
	s.log.InfoContext(ctx, "adult attested",
		slog.String("event", "auth_adult_attested"),
		slog.String("user_id", userID.String()),
	)
	return nil
}

// CurrentUserView assembles the authenticated user's own account view.
func (s *Service) CurrentUserView(ctx context.Context, user *users.User) (users.PrivateUser, error) {
	profile, err := s.users.Profile(ctx, user.ID)
	if err != nil && !errors.Is(err, users.ErrNotFound) {
		return users.PrivateUser{}, err
	}

	isAuthor, err := s.users.HasAuthorProfile(ctx, user.ID)
	if err != nil {
		return users.PrivateUser{}, err
	}

	return user.Private(profile, isAuthor), nil
}
