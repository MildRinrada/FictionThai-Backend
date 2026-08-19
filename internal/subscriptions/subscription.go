// Package subscriptions implements FictionThai's platform-owned Premium/Pro
// subscription feature - the FIRST and ONLY monetization stream (Phase 11,
// docs/MONETIZATION.md).
//
// Scope is deliberately narrow and is the whole of it:
//
//   - Reader pays the PLATFORM for Premium/Pro. Revenue is FictionThai's.
//   - Phase 1 payment is PromptPay QR + a manually-verified slip; the frontend
//     can NEVER declare success (docs/07 §39, brief §16).
//   - Entitlements are resolved on the BACKEND (brief §11, §20); the tier is
//     never trusted from the client.
//
// It explicitly does NOT implement anything writer-facing: no donations inside
// the platform, no payouts, earnings, wallet, balance, commission, paid
// chapters, or marketplace (brief §28). Writer support is an EXTERNAL EasyDonate
// link handled by the separate authors package; FictionThai never touches that
// money.
//
// The package owns the subscription_plans / subscriptions / subscription_payments
// tables and NOTHING else. Payment-slip BYTES live in the media domain (a
// private 'payment_slip' object referenced by FK); this package stores only the
// media id.
package subscriptions

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Operating mode (demo-mode brief §3, §16, §25)
// ---------------------------------------------------------------------------

// Mode is the platform's monetization operating mode, chosen by configuration
// (SUBSCRIPTION_MODE). It controls what a NEW user may do - never what an
// existing subscription IS (brief §13): entitlement resolution reads stored
// state and is mode-independent.
type Mode string

const (
	// ModeDisabled: Premium/Pro is not publicly available. The safest default
	// (brief §16). Pricing may still be browsed ("coming soon"), but no
	// subscription - paid or demo - can be created.
	ModeDisabled Mode = "disabled"
	// ModeDemo: Premium/Pro is offered as a FREE launch demo. A user activates a
	// demo entitlement at no cost; the paid checkout path is off (brief §4, §11).
	ModeDemo Mode = "demo"
	// ModeLive: real paid subscriptions via the PromptPay + manual-verification
	// flow. The demo activation path is off (brief §12).
	ModeLive Mode = "live"
)

// Enabled reports whether ANY acquisition path is open (demo or live). Disabled
// is the only mode where the whole purchase/activation surface is off.
func (m Mode) Enabled() bool { return m == ModeDemo || m == ModeLive }

// IsDemo reports the free-demo mode. IsLive reports the paid mode.
func (m Mode) IsDemo() bool { return m == ModeDemo }
func (m Mode) IsLive() bool { return m == ModeLive }

// ---------------------------------------------------------------------------
// Tiers & entitlements (brief §11–§12, §20)
// ---------------------------------------------------------------------------

// Tier is the access level a subscription grants. free is the absence of any
// live subscription - it is never stored, only computed.
type Tier string

const (
	TierFree    Tier = "free"
	TierPremium Tier = "premium"
	TierPro     Tier = "pro"
)

// rank orders the tiers so a higher tier subsumes a lower one. An unknown tier
// ranks below free and grants nothing.
func (t Tier) rank() int {
	switch t {
	case TierFree:
		return 0
	case TierPremium:
		return 1
	case TierPro:
		return 2
	}
	return -1
}

// AtLeast reports whether t grants everything min grants (pro ⊇ premium ⊇ free).
func (t Tier) AtLeast(min Tier) bool {
	return t.rank() >= 0 && t.rank() >= min.rank()
}

// PlanTier reports whether a value is a tier a PLAN may carry (free is not a
// plan tier - it is the unsubscribed state).
func (t Tier) PlanTier() bool { return t == TierPremium || t == TierPro }

// Entitlement keys. This registry is deliberately MINIMAL - only the tier
// baselines exist today, because no concrete premium feature has been specified
// yet (docs/MONETIZATION.md §3, brief §3 "do not create fake features that do
// not exist yet"). The mechanism is what matters: when a real premium capability
// is built, it registers its own key here mapped to the tier that unlocks it -
// e.g. entitlementRegistry["premium.reader.bookmarks_unlimited"] = TierPremium -
// and every gate then asks HasEntitlement(user, key) rather than testing the
// tier string in scattered places (brief §12). Adding a key is one line.
const (
	EntitlementPremium = "premium"
	EntitlementPro     = "pro"
)

// entitlementRegistry maps a capability key to the MINIMUM tier that grants it.
var entitlementRegistry = map[string]Tier{
	EntitlementPremium: TierPremium,
	EntitlementPro:     TierPro,
}

// entitlementOrder is the deterministic order Grants returns keys in.
var entitlementOrder = []string{EntitlementPremium, EntitlementPro}

// Allows reports whether tier t grants capability key. An unknown key is denied
// by default (fail closed) - a feature must register its key to be grantable.
func Allows(t Tier, key string) bool {
	req, ok := entitlementRegistry[key]
	return ok && t.AtLeast(req)
}

// Grants returns the capability keys a tier is granted, in a stable order.
func Grants(t Tier) []string {
	out := make([]string, 0, len(entitlementOrder))
	for _, key := range entitlementOrder {
		if Allows(t, key) {
			out = append(out, key)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Lifecycle (brief §10)
// ---------------------------------------------------------------------------

// Status is the subscription lifecycle state (a fixed, closed set - the DB
// enforces it too).
type Status string

const (
	// StatusPending: created, awaiting first payment verification. No entitlement.
	StatusPending Status = "pending"
	// StatusActive: verified and within the paid period. Grants its tier.
	StatusActive Status = "active"
	// StatusCancelled: the user cancelled. Access is NOT revoked - it continues
	// until current_period_end (brief §10: do not revoke already-paid access),
	// then the row expires.
	StatusCancelled Status = "cancelled"
	// StatusExpired: terminal. Reached when the paid period ends, or when the
	// first payment is rejected before activation. No entitlement; frees the
	// per-user "one live subscription" slot.
	StatusExpired Status = "expired"
)

// entitledAt reports whether this subscription grants its tier at instant now.
// A cancelled subscription still grants until its period end - that access was
// already paid for (brief §10).
func (s *Subscription) entitledAt(now time.Time) bool {
	if s.Status != StatusActive && s.Status != StatusCancelled {
		return false
	}
	return s.CurrentPeriodEnd != nil && now.Before(*s.CurrentPeriodEnd)
}

// pastPeriod reports whether an active/cancelled subscription has run out - the
// signal for lazy expiry.
func (s *Subscription) pastPeriod(now time.Time) bool {
	if s.Status != StatusActive && s.Status != StatusCancelled {
		return false
	}
	return s.CurrentPeriodEnd != nil && !now.Before(*s.CurrentPeriodEnd)
}

// ---------------------------------------------------------------------------
// Plans (reference data; prices come from the DB, brief §9)
// ---------------------------------------------------------------------------

// BillingPeriod is how long one paid term lasts.
type BillingPeriod string

const (
	PeriodMonthly BillingPeriod = "monthly"
	PeriodYearly  BillingPeriod = "yearly"
)

// advance returns the period end for a term that starts at from.
func (p BillingPeriod) advance(from time.Time) time.Time {
	switch p {
	case PeriodYearly:
		return from.AddDate(1, 0, 0)
	default:
		return from.AddDate(0, 1, 0)
	}
}

// Plan codes - exactly the three confirmed plans (docs/MONETIZATION.md §3). No
// others are invented (brief §19).
const (
	PlanPremiumMonthly = "premium_monthly"
	PlanPremiumYearly  = "premium_yearly"
	PlanProMonthly     = "pro_monthly"
)

// CurrencyTHB is the sole currency of record (docs/MONETIZATION.md §5).
const CurrencyTHB = "THB"

// MethodPromptPay is the Phase 1 payment method (brief §4).
const MethodPromptPay = "promptpay"

// Plan mirrors a subscription_plans row.
type Plan struct {
	ID            uuid.UUID
	Code          string
	Tier          Tier
	BillingPeriod BillingPeriod
	PriceMinor    int64
	Currency      string
	Active        bool
}

// PlanView is the public shape of a plan. The internal id is deliberately
// omitted - clients reference a plan by its stable code.
type PlanView struct {
	Code          string        `json:"code"`
	Tier          Tier          `json:"tier"`
	BillingPeriod BillingPeriod `json:"billing_period"`
	PriceMinor    int64         `json:"price_minor"`
	Currency      string        `json:"currency"`
}

// View renders a plan for the API.
func (p *Plan) View() PlanView {
	return PlanView{
		Code:          p.Code,
		Tier:          p.Tier,
		BillingPeriod: p.BillingPeriod,
		PriceMinor:    p.PriceMinor,
		Currency:      p.Currency,
	}
}

// ---------------------------------------------------------------------------
// Subscription rows & views
// ---------------------------------------------------------------------------

// Source is HOW an entitlement was obtained (demo-mode brief §7). It is NOT the
// lifecycle (that is Status): a demo and a paid subscription share every
// lifecycle state and every entitlement rule - only the acquisition differs.
// The invariant: SourceDemo must never be read as evidence that money was paid.
type Source string

const (
	// SourcePaid: the PromptPay-verified flow. The default for every row.
	SourcePaid Source = "paid"
	// SourceDemo: a free launch-demo grant. Never has a payment (brief §2).
	SourceDemo Source = "demo"
)

// Subscription mirrors a subscriptions row, with the plan code joined in for
// rendering.
type Subscription struct {
	ID                 uuid.UUID
	UserID             uuid.UUID
	PlanID             uuid.UUID
	PlanCode           string
	Tier               Tier
	Status             Status
	Source             Source
	CurrentPeriodStart *time.Time
	CurrentPeriodEnd   *time.Time
	CancelledAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// SubscriptionView is the API shape of a subscription. Source lets the client
// render a demo distinctly from a paid subscription (brief §10, §17) - but it is
// display only; the BACKEND remains the entitlement authority (brief §8).
type SubscriptionView struct {
	ID                 uuid.UUID  `json:"id"`
	PlanCode           string     `json:"plan_code"`
	Tier               Tier       `json:"tier"`
	Status             Status     `json:"status"`
	Source             Source     `json:"source"`
	CurrentPeriodStart *time.Time `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   *time.Time `json:"current_period_end,omitempty"`
	CancelledAt        *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

// View renders a subscription for the API.
func (s *Subscription) View() SubscriptionView {
	return SubscriptionView{
		ID:                 s.ID,
		PlanCode:           s.PlanCode,
		Tier:               s.Tier,
		Status:             s.Status,
		Source:             s.Source,
		CurrentPeriodStart: s.CurrentPeriodStart,
		CurrentPeriodEnd:   s.CurrentPeriodEnd,
		CancelledAt:        s.CancelledAt,
		CreatedAt:          s.CreatedAt,
	}
}

// ---------------------------------------------------------------------------
// Payment rows & views
// ---------------------------------------------------------------------------

// PaymentStatus is the verification state of one payment attempt - SEPARATE from
// the subscription lifecycle (brief §10). A closed set; the DB enforces it.
type PaymentStatus string

const (
	// PaymentPending: the reader has (usually) submitted a slip and is awaiting
	// verification. Never grants Premium.
	PaymentPending PaymentStatus = "pending_verification"
	// PaymentVerified: an authorized verifier confirmed it; the subscription was
	// activated as part of the same transition.
	PaymentVerified PaymentStatus = "verified"
	// PaymentRejected: an authorized verifier rejected it. The subscription is
	// expired so the reader can try again.
	PaymentRejected PaymentStatus = "rejected"
)

// Payment mirrors a subscription_payments row.
type Payment struct {
	ID                  uuid.UUID
	SubscriptionID      uuid.UUID
	UserID              uuid.UUID
	PlanID              uuid.UUID
	AmountMinor         int64
	Currency            string
	Method              string
	Status              PaymentStatus
	PaymentSlipMediaID  *uuid.UUID
	EvidenceSubmittedAt *time.Time
	ReviewedBy          *uuid.UUID
	ReviewedAt          *time.Time
	RejectReason        *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// PaymentView is the API shape of a payment attempt. It NEVER exposes a storage
// key or path (brief §11, §14): the slip is reachable only through the private,
// authorized media route, referenced here by a relative URL built from the
// media id.
type PaymentView struct {
	ID                  uuid.UUID     `json:"id"`
	SubscriptionID      uuid.UUID     `json:"subscription_id"`
	AmountMinor         int64         `json:"amount_minor"`
	Currency            string        `json:"currency"`
	Method              string        `json:"method"`
	Status              PaymentStatus `json:"status"`
	HasEvidence         bool          `json:"has_evidence"`
	EvidenceURL         *string       `json:"evidence_url,omitempty"`
	EvidenceSubmittedAt *time.Time    `json:"evidence_submitted_at,omitempty"`
	RejectReason        *string       `json:"reject_reason,omitempty"`
	ReviewedAt          *time.Time    `json:"reviewed_at,omitempty"`
	CreatedAt           time.Time     `json:"created_at"`
}

// View renders a payment for the API.
func (p *Payment) View() PaymentView {
	v := PaymentView{
		ID:                  p.ID,
		SubscriptionID:      p.SubscriptionID,
		AmountMinor:         p.AmountMinor,
		Currency:            p.Currency,
		Method:              p.Method,
		Status:              p.Status,
		EvidenceSubmittedAt: p.EvidenceSubmittedAt,
		RejectReason:        p.RejectReason,
		ReviewedAt:          p.ReviewedAt,
		CreatedAt:           p.CreatedAt,
	}
	if p.PaymentSlipMediaID != nil {
		v.HasEvidence = true
		url := PrivateEvidenceURL(*p.PaymentSlipMediaID)
		v.EvidenceURL = &url
	}
	return v
}

// PrivateEvidenceURL is the authorized, owner/staff-only path that serves a
// payment slip. It carries the media id, never the storage key (brief §14).
func PrivateEvidenceURL(mediaID uuid.UUID) string {
	return "/api/v1/media/" + mediaID.String() + "/private"
}

// ---------------------------------------------------------------------------
// Composite views
// ---------------------------------------------------------------------------

// PromptPayInstructions is the checkout QR + how to pay it. The QR pays the
// PLATFORM's PromptPay account; it is NOT the user's payment evidence (brief
// §15). When the platform target is unconfigured, Payload is empty and Available
// is false - the amount and target still render as manual instructions.
type PromptPayInstructions struct {
	Target      string `json:"target,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Payload     string `json:"qr_payload,omitempty"`
	Available   bool   `json:"available"`
}

// CheckoutView is returned by checkout: the pending subscription, its pending
// payment, and how to pay.
type CheckoutView struct {
	Subscription SubscriptionView      `json:"subscription"`
	Payment      PaymentView           `json:"payment"`
	PromptPay    PromptPayInstructions `json:"promptpay"`
}

// DemoView describes the free launch-demo offer and, in an authenticated
// overview, the caller's standing against it (demo-mode brief §5, §6, §11). It
// is present only in demo mode.
type DemoView struct {
	// OfferedTier is the tier a demo grants (SUBSCRIPTION_DEMO_TIER).
	OfferedTier Tier `json:"offered_tier"`
	// DurationDays is how long a demo lasts (SUBSCRIPTION_DEMO_DURATION_DAYS).
	DurationDays int `json:"duration_days"`
	// Used reports whether the caller has EVER activated a demo (one per user,
	// ever - brief §6). Meaningful only in an authenticated overview.
	Used bool `json:"used"`
	// Eligible reports whether the caller may activate a demo right now: demo
	// mode, never used, and no live subscription. Server-computed so the client
	// never decides entitlement (brief §8).
	Eligible bool `json:"eligible"`
}

// PricingView is the PUBLIC pricing payload: the plans, the current mode (so a
// guest page can render "coming soon" / "try free" / "subscribe"), and - in
// demo mode - the demo offer. Available in every mode; prices are the database's
// (brief §9).
type PricingView struct {
	Mode  Mode       `json:"mode"`
	Plans []PlanView `json:"plans"`
	Demo  *DemoView  `json:"demo,omitempty"`
}

// OverviewView is the caller's subscription summary: their tier, the capability
// keys it grants, the current subscription (or null), the latest payment (for a
// "waiting for verification" / "rejected" hint), the plans on offer, the mode,
// and - in demo mode - their demo standing.
type OverviewView struct {
	Tier          Tier              `json:"tier"`
	Entitlements  []string          `json:"entitlements"`
	Subscription  *SubscriptionView `json:"subscription"`
	LatestPayment *PaymentView      `json:"latest_payment,omitempty"`
	Plans         []PlanView        `json:"plans"`
	Mode          Mode              `json:"mode"`
	Demo          *DemoView         `json:"demo,omitempty"`
}
