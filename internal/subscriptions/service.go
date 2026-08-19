package subscriptions

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// maxRejectReasonRunes bounds the free-text staff rejection reason. It is a
// short classification shown back to the reader, never PII.
const maxRejectReasonRunes = 64

// Notifier is the sliver of notifications this service needs: telling a reader
// their Premium payment was verified or rejected (the only two subscription
// lifecycle events with an implemented, staff-triggered transition - brief §17).
// Fire-and-forget: a notification failure never changes a payment's outcome.
type Notifier interface {
	SubscriptionActivated(ctx context.Context, recipientID, subscriptionID uuid.UUID)
	SubscriptionPaymentRejected(ctx context.Context, recipientID, subscriptionID uuid.UUID)
}

// Config is the Premium runtime configuration (docs/MONETIZATION.md, Phase 11;
// demo-mode brief §3).
type Config struct {
	// Mode is the monetization operating mode (disabled | demo | live). It gates
	// only the ACQUISITION surface - checkout in live, demo activation in demo.
	// Entitlement resolution and the caller's own overview are mode-independent
	// so an existing subscription always reads truthfully (brief §8, §13).
	Mode Mode
	// PromptPayTarget is the platform's receiving PromptPay id; empty → the QR
	// payload is omitted and only manual instructions are shown. Live mode only.
	PromptPayTarget string
	// PromptPayName is the merchant display name embedded in the QR.
	PromptPayName string
	// DemoTier is the tier a free demo grants (premium | pro). Demo mode only.
	DemoTier Tier
	// DemoDuration is how long a free demo lasts. Demo mode only.
	DemoDuration time.Duration
}

// demoDurationDays renders the demo length in whole days for the API/UI.
func (c Config) demoDurationDays() int {
	return int(c.DemoDuration / (24 * time.Hour))
}

// Service owns Premium business rules and is the authorization boundary for
// every subscription endpoint (docs/10 §27). It is also the entitlement
// authority the rest of the platform consults (brief §11, §20).
type Service struct {
	repo     *Repository
	notifier Notifier
	cfg      Config
	log      *slog.Logger

	// now is the clock, injectable so tests can exercise expiry deterministically
	// without sleeping. Defaults to time.Now.
	now func() time.Time
}

// NewService wires the subscription service. notifier may be nil (lifecycle
// transitions then simply notify nobody).
func NewService(repo *Repository, notifier Notifier, cfg Config, log *slog.Logger) *Service {
	return &Service{repo: repo, notifier: notifier, cfg: cfg, log: log, now: time.Now}
}

// PaymentAdminView is a payment enriched with its owner, for the staff review
// queue. Staff need to know whose payment they are verifying.
type PaymentAdminView struct {
	PaymentView
	UserID uuid.UUID `json:"user_id"`
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

func unavailable() *apierror.Error {
	return apierror.New(http.StatusServiceUnavailable, apierror.CodeUnavailable,
		"Premium subscriptions are currently unavailable.")
}

func subscriptionNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "SUBSCRIPTION_NOT_FOUND", "No subscription found.")
}

func paymentNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "PAYMENT_NOT_FOUND", "Payment not found.")
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("subscriptions: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

func requireStaff(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	if !identity.IsStaff() {
		return uuid.Nil, apierror.Forbidden("You do not have permission to do that.")
	}
	return identity.UserID(), nil
}

// ---------------------------------------------------------------------------
// Reader-facing: plans, overview, checkout, cancel
// ---------------------------------------------------------------------------

// Plans returns the public pricing payload: the plans, the current mode, and -
// in demo mode - the demo offer. Public and available in EVERY mode (brief §4,
// §19 "GET pricing → available"): a guest browsing the pricing page needs no
// account, and even in disabled mode the page renders "coming soon" with real
// prices. Prices come from the database (brief §9). The demo block here carries
// only the offer (tier + duration); per-user eligibility needs the caller and is
// resolved in Overview.
func (s *Service) Plans(ctx context.Context) (PricingView, error) {
	plans, err := s.repo.ActivePlans(ctx)
	if err != nil {
		return PricingView{}, s.internal("list plans", err)
	}
	views := make([]PlanView, 0, len(plans))
	for i := range plans {
		views = append(views, plans[i].View())
	}
	out := PricingView{Mode: s.cfg.Mode, Plans: views}
	if s.cfg.Mode.IsDemo() {
		out.Demo = &DemoView{
			OfferedTier:  s.cfg.DemoTier,
			DurationDays: s.cfg.demoDurationDays(),
		}
	}
	return out, nil
}

// Overview returns the caller's subscription summary: tier, entitlements, the
// current subscription (or null), their latest payment, the plans on offer, the
// mode, and - in demo mode - their demo standing.
//
// It is NOT gated by mode (brief §8, §13): the caller's real state must read
// truthfully in every mode, so an existing subscription survives a mode switch
// and a leftover subscription can always be seen and cancelled. Only the
// acquisition actions (checkout, demo activation) are mode-gated.
func (s *Service) Overview(ctx context.Context, identity *auth.Identity) (OverviewView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return OverviewView{}, err
	}

	tier, sub, err := s.currentTier(ctx, userID)
	if err != nil {
		return OverviewView{}, err
	}

	plans, err := s.repo.ActivePlans(ctx)
	if err != nil {
		return OverviewView{}, s.internal("list plans", err)
	}
	planViews := make([]PlanView, 0, len(plans))
	for i := range plans {
		planViews = append(planViews, plans[i].View())
	}

	out := OverviewView{
		Tier:         tier,
		Entitlements: Grants(tier),
		Plans:        planViews,
		Mode:         s.cfg.Mode,
	}
	// Show the subscription only while it is still live; a just-expired row reads
	// as "no subscription" so the reader can resubscribe (or, for a demo, so the
	// UI shows the "trial expired" state).
	hasLiveSub := sub != nil && sub.Status != StatusExpired
	if hasLiveSub {
		v := sub.View()
		out.Subscription = &v
	}

	payment, err := s.repo.FindLatestPaymentForUser(ctx, userID)
	if err == nil {
		v := payment.View()
		out.LatestPayment = &v
	} else if !errors.Is(err, ErrNotFound) {
		return OverviewView{}, s.internal("load latest payment", err)
	}

	// Demo standing (demo mode only): whether the caller has ever used their one
	// free trial, and whether they may activate one right now. Server-computed so
	// the client never decides eligibility (brief §6, §8).
	if s.cfg.Mode.IsDemo() {
		used, err := s.repo.HasDemoSubscription(ctx, userID)
		if err != nil {
			return OverviewView{}, s.internal("check demo standing", err)
		}
		out.Demo = &DemoView{
			OfferedTier:  s.cfg.DemoTier,
			DurationDays: s.cfg.demoDurationDays(),
			Used:         used,
			Eligible:     !used && !hasLiveSub,
		}
	}

	return out, nil
}

// Checkout begins a Premium purchase: it creates a pending subscription and a
// pending payment, and returns how to pay (PromptPay). It NEVER activates
// anything - activation is a staff-verified backend transition (brief §16).
func (s *Service) Checkout(
	ctx context.Context, identity *auth.Identity, planCode string,
) (CheckoutView, error) {
	// Paid checkout is LIVE-only. In demo mode the free trial is the acquisition
	// path (brief §12: demo endpoint disabled in live, and vice versa); in
	// disabled mode nothing is purchasable (brief §4). This is the backend guard
	// that makes "the frontend can never create a paid subscription" true
	// regardless of what the client attempts.
	if !s.cfg.Mode.IsLive() {
		return CheckoutView{}, unavailable()
	}
	userID, err := requireUser(identity)
	if err != nil {
		return CheckoutView{}, err
	}

	code := strings.TrimSpace(planCode)
	if code == "" {
		return CheckoutView{}, apierror.Validation(map[string][]string{
			"plan_code": {"A plan is required."},
		})
	}
	plan, err := s.repo.PlanByCode(ctx, code)
	if errors.Is(err, ErrNotFound) {
		return CheckoutView{}, apierror.Validation(map[string][]string{
			"plan_code": {"Unknown plan."},
		})
	}
	if err != nil {
		return CheckoutView{}, s.internal("load plan", err)
	}

	sub, err := s.repo.CreatePending(ctx, userID, plan)
	if errors.Is(err, ErrLiveSubscription) {
		return CheckoutView{}, apierror.Conflict(
			"You already have an active or pending subscription.")
	}
	if err != nil {
		return CheckoutView{}, s.internal("create subscription", err)
	}

	payment, err := s.repo.CreatePendingPayment(ctx, sub.ID, userID, plan)
	if err != nil {
		return CheckoutView{}, s.internal("create payment", err)
	}

	s.log.Info("premium checkout started",
		slog.String("subscription_id", sub.ID.String()),
		slog.String("user_id", userID.String()),
		slog.String("plan", plan.Code))

	return CheckoutView{
		Subscription: sub.View(),
		Payment:      payment.View(),
		PromptPay:    s.promptPayInstructions(plan),
	}, nil
}

// ActivateDemo grants the caller a FREE launch-demo entitlement (brief §4, §11).
// It is the demo-mode counterpart of Checkout, and is deliberately as different
// from it as the requirements demand:
//
//   - It creates an ACTIVE subscription immediately, source='demo'.
//   - It creates NO payment record, requires NO slip, and needs NO staff
//     verification (brief §2). A demo is never a financial transaction.
//   - The tier and duration come from configuration, never from the client - the
//     client cannot pick a tier or extend the period (brief §5, §8).
//   - One per user, ever: the database rejects a second (brief §6).
//
// It is available ONLY in demo mode; the paid checkout path stays the sole
// acquisition route in live mode (brief §12).
func (s *Service) ActivateDemo(ctx context.Context, identity *auth.Identity) (SubscriptionView, error) {
	if !s.cfg.Mode.IsDemo() {
		return SubscriptionView{}, unavailable()
	}
	userID, err := requireUser(identity)
	if err != nil {
		return SubscriptionView{}, err
	}

	// The demo grants the configured tier; a representative plan of that tier
	// satisfies the plan_id FK and carries the tier. The period is the configured
	// demo length, NOT the plan's billing period.
	plan, err := s.repo.PlanForTier(ctx, s.cfg.DemoTier)
	if errors.Is(err, ErrNotFound) {
		// No active plan for the configured demo tier - a misconfiguration, not a
		// user error.
		return SubscriptionView{}, s.internal("resolve demo plan",
			errors.New("no active plan for demo tier "+string(s.cfg.DemoTier)))
	}
	if err != nil {
		return SubscriptionView{}, s.internal("resolve demo plan", err)
	}

	start := s.now()
	end := start.Add(s.cfg.DemoDuration)

	sub, err := s.repo.CreateDemo(ctx, userID, plan, start, end)
	if errors.Is(err, ErrDemoAlreadyUsed) {
		return SubscriptionView{}, apierror.Conflict("You have already used your free trial.")
	}
	if errors.Is(err, ErrLiveSubscription) {
		return SubscriptionView{}, apierror.Conflict("You already have an active subscription.")
	}
	if err != nil {
		return SubscriptionView{}, s.internal("activate demo", err)
	}

	// Deliberately NO notification: the account page reflects the active demo
	// immediately, and adding a notification type only for the demo is
	// unnecessary - and any "your subscription is active" message risks reading
	// as a payment confirmation, which a demo must never imply (brief §18).
	s.log.Info("premium demo activated",
		slog.String("subscription_id", sub.ID.String()),
		slog.String("user_id", userID.String()),
		slog.String("tier", string(sub.Tier)))

	return sub.View(), nil
}

// Cancel stops the caller's subscription. An ACTIVE subscription becomes
// cancelled but KEEPS entitlement until its period end (brief §10 - already-paid
// access is not revoked). A PENDING one (never paid) is simply abandoned.
func (s *Service) Cancel(ctx context.Context, identity *auth.Identity) (SubscriptionView, error) {
	// Ungated by mode: cancelling only ever acts on a subscription the caller
	// already has, so a leftover subscription can always be stopped even after a
	// mode switch (brief §13). A demo cancels exactly like a paid subscription -
	// the shared lifecycle, no special case.
	userID, err := requireUser(identity)
	if err != nil {
		return SubscriptionView{}, err
	}

	sub, err := s.repo.FindLiveForUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return SubscriptionView{}, subscriptionNotFound()
	}
	if err != nil {
		return SubscriptionView{}, s.internal("load subscription", err)
	}

	switch sub.Status {
	case StatusActive:
		ok, err := s.repo.MarkCancelled(ctx, sub.ID)
		if err != nil {
			return SubscriptionView{}, s.internal("cancel subscription", err)
		}
		if !ok {
			return SubscriptionView{}, apierror.Conflict("The subscription could not be cancelled.")
		}
	case StatusPending:
		ok, err := s.repo.AbandonPending(ctx, sub.ID)
		if err != nil {
			return SubscriptionView{}, s.internal("abandon subscription", err)
		}
		if !ok {
			return SubscriptionView{}, apierror.Conflict("The subscription could not be cancelled.")
		}
	case StatusCancelled:
		return SubscriptionView{}, apierror.Conflict("This subscription is already cancelled.")
	default:
		return SubscriptionView{}, subscriptionNotFound()
	}

	reloaded, err := s.repo.FindSubscription(ctx, sub.ID)
	if err != nil {
		return SubscriptionView{}, s.internal("reload subscription", err)
	}
	s.log.Info("premium subscription cancelled",
		slog.String("subscription_id", sub.ID.String()),
		slog.String("user_id", userID.String()),
		slog.String("status", string(reloaded.Status)))
	return reloaded.View(), nil
}

// ---------------------------------------------------------------------------
// Entitlements - the reusable Premium check the rest of the platform consults
// (brief §11, §12, §20). The BACKEND is the source of truth; the tier is never
// trusted from the client.
// ---------------------------------------------------------------------------

// Entitlements resolves a user's current tier and the capability keys it grants.
func (s *Service) Entitlements(ctx context.Context, userID uuid.UUID) (Tier, []string, error) {
	tier, _, err := s.currentTier(ctx, userID)
	if err != nil {
		return TierFree, nil, err
	}
	return tier, Grants(tier), nil
}

// HasEntitlement reports whether a user currently holds a capability. Unknown
// keys are denied (fail closed). This is the single call gates should use rather
// than testing the tier string in scattered places (brief §12).
func (s *Service) HasEntitlement(ctx context.Context, userID uuid.UUID, key string) (bool, error) {
	tier, _, err := s.currentTier(ctx, userID)
	if err != nil {
		return false, err
	}
	return Allows(tier, key), nil
}

// currentTier resolves the caller's live tier, applying lazy expiry: a live
// subscription past its paid period is expired here, on read, so entitlement is
// always truthful without a background sweeper. Returns the (possibly
// just-expired) subscription for display.
func (s *Service) currentTier(ctx context.Context, userID uuid.UUID) (Tier, *Subscription, error) {
	sub, err := s.repo.FindLiveForUser(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return TierFree, nil, nil
	}
	if err != nil {
		return TierFree, nil, s.internal("resolve tier", err)
	}

	now := s.now()
	if sub.pastPeriod(now) {
		if _, err := s.repo.ExpireIfPast(ctx, sub.ID); err != nil {
			return TierFree, nil, s.internal("expire subscription", err)
		}
		sub.Status = StatusExpired
		return TierFree, sub, nil
	}
	if sub.entitledAt(now) {
		return sub.Tier, sub, nil
	}
	// Pending (awaiting verification) grants nothing.
	return TierFree, sub, nil
}

// ---------------------------------------------------------------------------
// Payment-slip attachment - called by the MEDIA domain (brief §12–§13).
//
// The slip's bytes live in media as a private 'payment_slip' object; media calls
// these two methods so THIS domain, which owns payment authorization, decides
// whether the caller may attach, and records the reference.
// ---------------------------------------------------------------------------

// AuthorizePaymentSlip runs BEFORE the media object is stored: it confirms the
// caller owns a pending payment that is still awaiting evidence. A payment that
// is not the caller's is the same non-oracle 404 as a missing one (brief §11).
func (s *Service) AuthorizePaymentSlip(ctx context.Context, ownerID, paymentID uuid.UUID) error {
	if !s.cfg.Mode.Enabled() {
		return unavailable()
	}
	payment, err := s.repo.FindPayment(ctx, paymentID)
	if errors.Is(err, ErrNotFound) {
		return paymentNotFound()
	}
	if err != nil {
		return s.internal("load payment", err)
	}
	if payment.UserID != ownerID {
		return paymentNotFound()
	}
	if payment.Status != PaymentPending {
		return apierror.Conflict("This payment is not awaiting evidence.")
	}
	if payment.PaymentSlipMediaID != nil {
		return apierror.Conflict("Payment evidence has already been submitted.")
	}
	return nil
}

// AttachPaymentSlip records the stored slip's media id against the caller's
// pending payment. The guarded update is the real race protection: a second
// concurrent attach finds no pending, slip-less row and conflicts.
func (s *Service) AttachPaymentSlip(ctx context.Context, ownerID, paymentID, mediaID uuid.UUID) error {
	ok, err := s.repo.AttachEvidence(ctx, paymentID, ownerID, mediaID)
	if err != nil {
		return s.internal("attach payment evidence", err)
	}
	if !ok {
		return apierror.Conflict("Payment evidence has already been submitted.")
	}
	s.log.Info("premium payment evidence attached",
		slog.String("payment_id", paymentID.String()),
		slog.String("user_id", ownerID.String()))
	return nil
}

// ---------------------------------------------------------------------------
// Staff-facing: review queue, verify, reject (brief §16 verification model)
// ---------------------------------------------------------------------------

// ReviewQueue returns payments awaiting verification (with a slip), for staff.
func (s *Service) ReviewQueue(
	ctx context.Context, identity *auth.Identity, page pagination.Params,
) ([]PaymentAdminView, pagination.Meta, error) {
	if !s.cfg.Mode.Enabled() {
		return nil, pagination.Meta{}, unavailable()
	}
	if _, err := requireStaff(identity); err != nil {
		return nil, pagination.Meta{}, err
	}
	payments, total, err := s.repo.PendingReviewQueue(ctx, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("review queue", err)
	}
	views := make([]PaymentAdminView, 0, len(payments))
	for i := range payments {
		views = append(views, PaymentAdminView{PaymentView: payments[i].View(), UserID: payments[i].UserID})
	}
	return views, page.MetaFor(int64(total)), nil
}

// VerifyResult is what a successful verification returns.
type VerifyResult struct {
	Payment      PaymentView      `json:"payment"`
	Subscription SubscriptionView `json:"subscription"`
}

// Verify confirms a payment and activates its subscription (staff only). The
// paid period runs from the verification instant. Idempotent by construction:
// the repository's guarded transaction refuses a second confirmation, so a
// duplicate verify cannot double-activate (brief §10).
func (s *Service) Verify(
	ctx context.Context, identity *auth.Identity, paymentID uuid.UUID,
) (VerifyResult, error) {
	if !s.cfg.Mode.Enabled() {
		return VerifyResult{}, unavailable()
	}
	reviewerID, err := requireStaff(identity)
	if err != nil {
		return VerifyResult{}, err
	}

	payment, err := s.repo.FindPayment(ctx, paymentID)
	if errors.Is(err, ErrNotFound) {
		return VerifyResult{}, paymentNotFound()
	}
	if err != nil {
		return VerifyResult{}, s.internal("load payment", err)
	}
	if payment.Status != PaymentPending {
		return VerifyResult{}, apierror.Conflict("This payment is not awaiting verification.")
	}
	if payment.PaymentSlipMediaID == nil {
		return VerifyResult{}, apierror.Conflict("This payment has no evidence to verify.")
	}

	plan, err := s.repo.PlanByID(ctx, payment.PlanID)
	if err != nil {
		return VerifyResult{}, s.internal("load plan", err)
	}
	start := s.now()
	end := plan.BillingPeriod.advance(start)

	verified, sub, err := s.repo.Verify(ctx, paymentID, reviewerID, start, end)
	if errors.Is(err, ErrConflict) {
		return VerifyResult{}, apierror.Conflict("This payment is no longer awaiting verification.")
	}
	if err != nil {
		return VerifyResult{}, s.internal("verify payment", err)
	}
	sub.PlanCode = plan.Code

	if s.notifier != nil {
		s.notifier.SubscriptionActivated(ctx, sub.UserID, sub.ID)
	}
	s.log.Info("premium payment verified",
		slog.String("payment_id", paymentID.String()),
		slog.String("subscription_id", sub.ID.String()),
		slog.String("reviewer_id", reviewerID.String()),
		slog.String("tier", string(sub.Tier)))

	return VerifyResult{Payment: verified.View(), Subscription: sub.View()}, nil
}

// Reject declines a payment (staff only) and frees the subscription so the
// reader can try again. The reason is a short classification shown to the reader.
func (s *Service) Reject(
	ctx context.Context, identity *auth.Identity, paymentID uuid.UUID, reason string,
) (PaymentView, error) {
	if !s.cfg.Mode.Enabled() {
		return PaymentView{}, unavailable()
	}
	reviewerID, err := requireStaff(identity)
	if err != nil {
		return PaymentView{}, err
	}

	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > maxRejectReasonRunes {
		return PaymentView{}, apierror.Validation(map[string][]string{
			"reason": {"The reason is too long."},
		})
	}

	payment, err := s.repo.FindPayment(ctx, paymentID)
	if errors.Is(err, ErrNotFound) {
		return PaymentView{}, paymentNotFound()
	}
	if err != nil {
		return PaymentView{}, s.internal("load payment", err)
	}
	if payment.Status != PaymentPending {
		return PaymentView{}, apierror.Conflict("This payment is not awaiting verification.")
	}

	rejected, err := s.repo.Reject(ctx, paymentID, reviewerID, reason)
	if errors.Is(err, ErrConflict) {
		return PaymentView{}, apierror.Conflict("This payment is no longer awaiting verification.")
	}
	if err != nil {
		return PaymentView{}, s.internal("reject payment", err)
	}

	if s.notifier != nil {
		s.notifier.SubscriptionPaymentRejected(ctx, rejected.UserID, rejected.SubscriptionID)
	}
	s.log.Info("premium payment rejected",
		slog.String("payment_id", paymentID.String()),
		slog.String("reviewer_id", reviewerID.String()))

	return rejected.View(), nil
}
