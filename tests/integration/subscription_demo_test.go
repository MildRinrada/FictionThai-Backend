package integration

import (
	"net/http"
	"testing"
)

// Premium/Pro Demo Mode (demo-mode brief §19–§21).
//
// A demo grants a REAL entitlement for free: same tier ladder, same lazy
// expiry, same cancel path as a paid subscription - only the acquisition
// differs. The invariant these tests defend is that a demo is NEVER a financial
// record: no subscription_payments row, never in the staff review queue, never
// evidence of payment (brief §2, §7, §17).

// activateDemo posts to the demo activation endpoint.
func (e *authEnv) activateDemo(t *testing.T, s webSession) apiResponse {
	t.Helper()
	return e.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/subscription/demo",
		cookies: s.authCookies(),
		csrf:    s.csrfToken,
	})
}

// paymentRowCount returns how many subscription_payments rows a user owns.
func (e *authEnv) paymentRowCount(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := e.db.DB.QueryRow(
		`SELECT count(*) FROM subscription_payments WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	return n
}

// demoRowState returns the source/status of a user's single demo row.
func (e *authEnv) demoRowState(t *testing.T, userID string) (source, status string) {
	t.Helper()
	if err := e.db.DB.QueryRow(
		`SELECT source, status FROM subscriptions WHERE user_id = $1 AND source = 'demo'`,
		userID).Scan(&source, &status); err != nil {
		t.Fatalf("read demo row: %v", err)
	}
	return source, status
}

// ---------------------------------------------------------------------------
// Activation grants the configured tier and creates NO payment (brief §19, §21)
// ---------------------------------------------------------------------------

func TestDemo_ActivationGrantsTierWithoutPayment(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, withSubscriptionDemo()) // default demo tier = pro
	reader := env.registerWeb(t)
	staff := env.staffSession(t)

	// Pricing advertises the demo offer, not a "subscribe" CTA.
	pres := env.do(t, apiRequest{method: http.MethodGet, path: "/api/v1/subscription/plans"})
	var pwrap struct {
		Data subPricing `json:"data"`
	}
	pres.json(t, &pwrap)
	if pwrap.Data.Mode != "demo" || pwrap.Data.Demo == nil {
		t.Fatalf("pricing = %+v, want demo mode with a demo offer", pwrap.Data)
	}
	if pwrap.Data.Demo.OfferedTier != "pro" || pwrap.Data.Demo.DurationDays != 30 {
		t.Fatalf("demo offer = %+v, want pro/30", pwrap.Data.Demo)
	}

	// Before activation: free, and eligible for a demo.
	before := env.overview(t, reader)
	if before.Tier != "free" || before.Mode != "demo" {
		t.Fatalf("pre-activation = tier:%s mode:%s", before.Tier, before.Mode)
	}
	if before.Demo == nil || !before.Demo.Eligible || before.Demo.Used {
		t.Fatalf("pre-activation demo standing = %+v, want eligible/unused", before.Demo)
	}

	// A guest cannot activate - the API enforces auth independently (brief §11).
	if g := env.activateDemo(t, webSession{}); g.status != http.StatusUnauthorized {
		t.Fatalf("guest demo activation = %d, want 401", g.status)
	}

	// Activate: an ACTIVE, source=demo subscription at the configured tier.
	res := env.activateDemo(t, reader)
	if res.status != http.StatusCreated {
		t.Fatalf("demo activation = %d, want 201. body: %s", res.status, res.body)
	}
	var awrap struct {
		Data subSubscription `json:"data"`
	}
	res.json(t, &awrap)
	if awrap.Data.Tier != "pro" || awrap.Data.Status != "active" || awrap.Data.Source != "demo" {
		t.Fatalf("demo subscription = %+v, want pro/active/demo", awrap.Data)
	}

	// The entitlement is real and backend-resolved.
	after := env.overview(t, reader)
	if after.Tier != "pro" {
		t.Fatalf("post-activation tier = %s, want pro", after.Tier)
	}
	if len(after.Entitlements) != 2 {
		t.Fatalf("entitlements = %v, want premium+pro", after.Entitlements)
	}
	if after.Subscription == nil || after.Subscription.Source != "demo" || after.Subscription.Status != "active" {
		t.Fatalf("post-activation subscription = %+v, want active demo", after.Subscription)
	}
	if after.Demo == nil || after.Demo.Eligible || !after.Demo.Used {
		t.Fatalf("post-activation demo standing = %+v, want used/not-eligible", after.Demo)
	}

	// THE CRITICAL INVARIANT: a demo creates NO payment record (brief §2).
	if n := env.paymentRowCount(t, reader.userID); n != 0 {
		t.Fatalf("demo created %d payment rows, want 0 (a demo is never a payment)", n)
	}
	if after.LatestPayment != nil {
		t.Fatalf("demo user has a latest payment %+v, want none", after.LatestPayment)
	}

	// And the stored row is unmistakably a demo.
	if source, status := env.demoRowState(t, reader.userID); source != "demo" || status != "active" {
		t.Fatalf("stored demo row = %s/%s, want demo/active", source, status)
	}

	// A demo user must NEVER appear in the staff payment-review queue (brief §17).
	q := env.do(t, apiRequest{
		method:  http.MethodGet,
		path:    "/api/v1/admin/subscription/payments",
		cookies: staff.authCookies(),
	})
	if q.status != http.StatusOK {
		t.Fatalf("review queue status = %d, want 200", q.status)
	}
	var qwrap struct {
		Data []struct {
			UserID string `json:"user_id"`
		} `json:"data"`
	}
	q.json(t, &qwrap)
	for _, p := range qwrap.Data {
		if p.UserID == reader.userID {
			t.Fatalf("demo user appeared in the payment review queue - must never happen")
		}
	}
}

// ---------------------------------------------------------------------------
// One demo per user, ever (brief §6, §19 duplicate)
// ---------------------------------------------------------------------------

func TestDemo_DuplicateActivationRejected(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, withSubscriptionDemo())
	reader := env.registerWeb(t)

	if res := env.activateDemo(t, reader); res.status != http.StatusCreated {
		t.Fatalf("first demo activation = %d, want 201", res.status)
	}
	// A second activation while the demo is live is a conflict.
	if res := env.activateDemo(t, reader); res.status != http.StatusConflict {
		t.Fatalf("second demo activation = %d, want 409", res.status)
	}
}

// ---------------------------------------------------------------------------
// Expiry drops entitlement AND still blocks a re-trial (brief §6, §19 expiration)
// ---------------------------------------------------------------------------

func TestDemo_ExpiredLosesEntitlementAndCannotRetry(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, withSubscriptionDemo())
	reader := env.registerWeb(t)

	res := env.activateDemo(t, reader)
	if res.status != http.StatusCreated {
		t.Fatalf("demo activation = %d, want 201", res.status)
	}
	var awrap struct {
		Data subSubscription `json:"data"`
	}
	res.json(t, &awrap)

	// Force the demo period to have ended, then read: entitlement is gone.
	env.backdatePeriodEnd(t, awrap.Data.ID)
	expired := env.overview(t, reader)
	if expired.Tier != "free" || len(expired.Entitlements) != 0 {
		t.Fatalf("expired demo = tier:%s ents:%v, want free/none", expired.Tier, expired.Entitlements)
	}
	// The trial is spent: used=true, and NOT eligible again (one per user, ever).
	if expired.Demo == nil || expired.Demo.Eligible || !expired.Demo.Used {
		t.Fatalf("expired demo standing = %+v, want used/not-eligible", expired.Demo)
	}
	// The endpoint refuses a second trial even after expiry.
	if again := env.activateDemo(t, reader); again.status != http.StatusConflict {
		t.Fatalf("re-activation after expiry = %d, want 409", again.status)
	}
}

// ---------------------------------------------------------------------------
// The demo tier is configuration-driven (brief §5)
// ---------------------------------------------------------------------------

func TestDemo_TierIsConfigurable(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, withSubscriptionDemo(), withSubscriptionDemoTier("premium"))
	reader := env.registerWeb(t)

	res := env.activateDemo(t, reader)
	if res.status != http.StatusCreated {
		t.Fatalf("demo activation = %d, want 201", res.status)
	}
	ov := env.overview(t, reader)
	if ov.Tier != "premium" {
		t.Fatalf("configured demo tier = %s, want premium", ov.Tier)
	}
	// Premium grants premium only, not pro (the ladder still holds).
	if len(ov.Entitlements) != 1 || ov.Entitlements[0] != "premium" {
		t.Fatalf("entitlements = %v, want [premium]", ov.Entitlements)
	}
}

// ---------------------------------------------------------------------------
// Live mode disables the demo path but keeps real checkout (brief §12, §19 live)
// ---------------------------------------------------------------------------

func TestDemo_LiveModeDisablesDemoKeepsCheckout(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t) // default = live
	reader := env.registerWeb(t)

	// Demo activation is off in live mode.
	if res := env.activateDemo(t, reader); res.status != http.StatusServiceUnavailable {
		t.Fatalf("demo activation in live mode = %d, want 503", res.status)
	}
	// The real paid flow still works.
	co := env.checkoutOK(t, reader, "premium_monthly")
	if co.Subscription.Status != "pending" || co.Subscription.Source != "paid" {
		t.Fatalf("live checkout subscription = %+v, want pending/paid", co.Subscription)
	}
	// Overview in live mode carries no demo block.
	ov := env.overview(t, reader)
	if ov.Mode != "live" || ov.Demo != nil {
		t.Fatalf("live overview = mode:%s demo:%+v, want live/nil", ov.Mode, ov.Demo)
	}
}

// ---------------------------------------------------------------------------
// A mode switch does not convert a demo into a paid subscriber (brief §13)
// ---------------------------------------------------------------------------

func TestDemo_ModeSwitchPreservesDemoRecord(t *testing.T) {
	t.Parallel()
	demoEnv := newAuthEnv(t, withSubscriptionDemo())
	reader := demoEnv.registerWeb(t)
	if res := demoEnv.activateDemo(t, reader); res.status != http.StatusCreated {
		t.Fatalf("demo activation = %d, want 201", res.status)
	}

	// A second wiring in LIVE mode over the SAME database (users/sessions are
	// shared) - simulating the operator flipping SUBSCRIPTION_MODE to live.
	liveEnv := newAuthEnv(t) // default = live, same test DB

	// The demo entitlement is untouched: still a live, source=demo subscription
	// granting pro. The switch changed config, not stored rows.
	ov := liveEnv.overview(t, reader)
	if ov.Tier != "pro" {
		t.Fatalf("after switch to live, tier = %s, want pro (demo preserved)", ov.Tier)
	}
	if ov.Subscription == nil || ov.Subscription.Source != "demo" || ov.Subscription.Status != "active" {
		t.Fatalf("after switch, subscription = %+v, want active demo (not converted)", ov.Subscription)
	}
	// The still-active demo occupies the one-live slot, so a paid checkout in
	// live mode conflicts rather than silently stacking.
	if res := liveEnv.checkout(t, reader, "premium_monthly"); res.status != http.StatusConflict {
		t.Fatalf("checkout over an active demo = %d, want 409", res.status)
	}
}
