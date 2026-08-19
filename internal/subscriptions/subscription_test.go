package subscriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTierRankAndAtLeast(t *testing.T) {
	if !TierPro.AtLeast(TierPremium) {
		t.Error("pro should subsume premium")
	}
	if !TierPro.AtLeast(TierPro) {
		t.Error("pro should satisfy pro")
	}
	if TierPremium.AtLeast(TierPro) {
		t.Error("premium must NOT satisfy pro")
	}
	if TierFree.AtLeast(TierPremium) {
		t.Error("free must NOT satisfy premium")
	}
	if Tier("garbage").AtLeast(TierFree) {
		t.Error("an unknown tier must satisfy nothing, not even free")
	}
	if !TierPremium.PlanTier() || !TierPro.PlanTier() || TierFree.PlanTier() {
		t.Error("only premium and pro are plan tiers")
	}
}

func TestEntitlementRegistry(t *testing.T) {
	// Premium grants premium but NOT pro.
	if !Allows(TierPremium, EntitlementPremium) {
		t.Error("premium should grant the premium entitlement")
	}
	if Allows(TierPremium, EntitlementPro) {
		t.Error("premium must NOT grant the pro entitlement")
	}
	// Pro grants both.
	if !Allows(TierPro, EntitlementPremium) || !Allows(TierPro, EntitlementPro) {
		t.Error("pro should grant both entitlements")
	}
	// Free grants nothing; unknown keys are denied (fail closed).
	if Allows(TierFree, EntitlementPremium) {
		t.Error("free should grant nothing")
	}
	if Allows(TierPro, "premium.unregistered.feature") {
		t.Error("an unregistered key must be denied by default")
	}

	if got := Grants(TierFree); len(got) != 0 {
		t.Errorf("free grants = %v, want empty", got)
	}
	if got := Grants(TierPremium); len(got) != 1 || got[0] != EntitlementPremium {
		t.Errorf("premium grants = %v, want [premium]", got)
	}
	if got := Grants(TierPro); len(got) != 2 {
		t.Errorf("pro grants = %v, want two keys", got)
	}
}

func TestModeHelpers(t *testing.T) {
	// Only demo and live are "enabled"; disabled turns the whole acquisition
	// surface off (demo-mode brief §4, §16).
	if ModeDisabled.Enabled() {
		t.Error("disabled must not be enabled")
	}
	if !ModeDemo.Enabled() || !ModeLive.Enabled() {
		t.Error("demo and live must both be enabled")
	}
	if !ModeDemo.IsDemo() || ModeDemo.IsLive() {
		t.Error("demo mode classification is wrong")
	}
	if !ModeLive.IsLive() || ModeLive.IsDemo() {
		t.Error("live mode classification is wrong")
	}
	if ModeDisabled.IsDemo() || ModeDisabled.IsLive() {
		t.Error("disabled is neither demo nor live")
	}
}

func TestConfigDemoDurationDays(t *testing.T) {
	c := Config{DemoDuration: 30 * 24 * time.Hour}
	if got := c.demoDurationDays(); got != 30 {
		t.Errorf("demoDurationDays = %d, want 30", got)
	}
	if got := (Config{DemoDuration: 24 * time.Hour}).demoDurationDays(); got != 1 {
		t.Errorf("demoDurationDays(1 day) = %d, want 1", got)
	}
}

func TestBillingPeriodAdvance(t *testing.T) {
	from := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if got := PeriodMonthly.advance(from); !got.Equal(from.AddDate(0, 1, 0)) {
		t.Errorf("monthly advance = %v", got)
	}
	if got := PeriodYearly.advance(from); !got.Equal(from.AddDate(1, 0, 0)) {
		t.Errorf("yearly advance = %v", got)
	}
}

func TestSubscriptionEntitledAt(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	past := now.Add(-24 * time.Hour)

	active := &Subscription{Status: StatusActive, CurrentPeriodEnd: &future}
	if !active.entitledAt(now) {
		t.Error("an active subscription within its period should be entitled")
	}

	// A cancelled subscription KEEPS entitlement until the period end (brief §10).
	cancelled := &Subscription{Status: StatusCancelled, CurrentPeriodEnd: &future}
	if !cancelled.entitledAt(now) {
		t.Error("a cancelled subscription within its period should still be entitled")
	}
	if !cancelled.pastPeriod(now.Add(48 * time.Hour)) {
		t.Error("a cancelled subscription past its end should be flagged for expiry")
	}

	// Past the period → not entitled, flagged for lazy expiry.
	expiredish := &Subscription{Status: StatusActive, CurrentPeriodEnd: &past}
	if expiredish.entitledAt(now) {
		t.Error("an active subscription past its period must NOT be entitled")
	}
	if !expiredish.pastPeriod(now) {
		t.Error("an active subscription past its period should be flagged for expiry")
	}

	// Pending never grants (no period yet).
	pending := &Subscription{Status: StatusPending}
	if pending.entitledAt(now) {
		t.Error("a pending subscription must never be entitled")
	}
	if pending.pastPeriod(now) {
		t.Error("a pending subscription is never 'past period'")
	}
}

func TestPaymentViewHidesStorageKey(t *testing.T) {
	mediaID := uuid.New()
	p := &Payment{
		ID:                 uuid.New(),
		SubscriptionID:     uuid.New(),
		AmountMinor:        9900,
		Currency:           CurrencyTHB,
		Method:             MethodPromptPay,
		Status:             PaymentPending,
		PaymentSlipMediaID: &mediaID,
	}
	v := p.View()
	if !v.HasEvidence {
		t.Error("a payment with a slip should report HasEvidence")
	}
	if v.EvidenceURL == nil {
		t.Fatal("expected a private evidence URL")
	}
	// The URL carries the media id, never a storage key/path (brief §14).
	if *v.EvidenceURL != "/api/v1/media/"+mediaID.String()+"/private" {
		t.Errorf("evidence URL = %q", *v.EvidenceURL)
	}

	none := &Payment{Status: PaymentPending}
	if nv := none.View(); nv.HasEvidence || nv.EvidenceURL != nil {
		t.Error("a payment without a slip must not expose evidence")
	}
}
