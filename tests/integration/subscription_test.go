package integration

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Decoded response shapes
// ---------------------------------------------------------------------------

type subPlan struct {
	Code          string `json:"code"`
	Tier          string `json:"tier"`
	BillingPeriod string `json:"billing_period"`
	PriceMinor    int64  `json:"price_minor"`
	Currency      string `json:"currency"`
}

type subSubscription struct {
	ID               string  `json:"id"`
	PlanCode         string  `json:"plan_code"`
	Tier             string  `json:"tier"`
	Status           string  `json:"status"`
	Source           string  `json:"source"`
	CurrentPeriodEnd *string `json:"current_period_end"`
}

type subPayment struct {
	ID             string  `json:"id"`
	SubscriptionID string  `json:"subscription_id"`
	AmountMinor    int64   `json:"amount_minor"`
	Currency       string  `json:"currency"`
	Method         string  `json:"method"`
	Status         string  `json:"status"`
	HasEvidence    bool    `json:"has_evidence"`
	EvidenceURL    *string `json:"evidence_url"`
	RejectReason   *string `json:"reject_reason"`
}

type subPromptPay struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Payload     string `json:"qr_payload"`
	Available   bool   `json:"available"`
}

type subCheckout struct {
	Subscription subSubscription `json:"subscription"`
	Payment      subPayment      `json:"payment"`
	PromptPay    subPromptPay    `json:"promptpay"`
}

type subDemo struct {
	OfferedTier  string `json:"offered_tier"`
	DurationDays int    `json:"duration_days"`
	Used         bool   `json:"used"`
	Eligible     bool   `json:"eligible"`
}

type subOverview struct {
	Tier          string           `json:"tier"`
	Entitlements  []string         `json:"entitlements"`
	Subscription  *subSubscription `json:"subscription"`
	LatestPayment *subPayment      `json:"latest_payment"`
	Plans         []subPlan        `json:"plans"`
	Mode          string           `json:"mode"`
	Demo          *subDemo         `json:"demo"`
}

type subPricing struct {
	Mode  string    `json:"mode"`
	Plans []subPlan `json:"plans"`
	Demo  *subDemo  `json:"demo"`
}

type subMediaView struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	MediaType string `json:"media_type"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (e *authEnv) checkout(t *testing.T, s webSession, planCode string) apiResponse {
	t.Helper()
	return e.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/subscription/checkout",
		body:    map[string]string{"plan_code": planCode},
		cookies: s.authCookies(),
		csrf:    s.csrfToken,
	})
}

func (e *authEnv) checkoutOK(t *testing.T, s webSession, planCode string) subCheckout {
	t.Helper()
	res := e.checkout(t, s, planCode)
	if res.status != http.StatusCreated {
		t.Fatalf("checkout %s status = %d, want 201. body: %s", planCode, res.status, res.body)
	}
	var wrap struct {
		Data subCheckout `json:"data"`
	}
	res.json(t, &wrap)
	return wrap.Data
}

// uploadPaymentSlip submits a PromptPay slip through the MEDIA endpoint with
// purpose=payment_slip and the payment id (addendum §12).
func (e *authEnv) uploadPaymentSlip(
	t *testing.T, s webSession, paymentID string, contents []byte,
) apiResponse {
	t.Helper()

	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	_ = form.WriteField("purpose", "payment_slip")
	if paymentID != "" {
		_ = form.WriteField("payment", paymentID)
	}
	if contents != nil {
		part, err := form.CreateFormFile("file", "slip.png")
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(contents); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	_ = form.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/v1/media", &buf)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Origin", testOrigin)
	for _, c := range s.authCookies() {
		r.AddCookie(c)
	}
	if s.csrfToken != "" {
		r.Header.Set("X-CSRF-Token", s.csrfToken)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, r)
	return apiResponse{status: rec.Code, body: rec.Body.Bytes(), header: rec.Header()}
}

func (e *authEnv) overview(t *testing.T, s webSession) subOverview {
	t.Helper()
	res := e.do(t, apiRequest{
		method:  http.MethodGet,
		path:    "/api/v1/subscription",
		cookies: s.authCookies(),
	})
	if res.status != http.StatusOK {
		t.Fatalf("overview status = %d, want 200. body: %s", res.status, res.body)
	}
	var wrap struct {
		Data subOverview `json:"data"`
	}
	res.json(t, &wrap)
	return wrap.Data
}

func (e *authEnv) verifyPayment(t *testing.T, staff webSession, paymentID string) apiResponse {
	t.Helper()
	return e.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/admin/subscription/payments/" + paymentID + "/verify",
		cookies: staff.authCookies(),
		csrf:    staff.csrfToken,
	})
}

// staffSession registers a user and promotes them to admin.
func (e *authEnv) staffSession(t *testing.T) webSession {
	t.Helper()
	s := e.registerWeb(t)
	e.promote(t, s.userID, "admin")
	return s
}

// awaitNotificationType polls the caller's notifications for a given type.
func (e *authEnv) awaitNotificationType(t *testing.T, s webSession, typ string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res := e.do(t, apiRequest{
			method:  http.MethodGet,
			path:    "/api/v1/me/notifications",
			cookies: s.authCookies(),
		})
		var wrap struct {
			Data []struct {
				Type string `json:"type"`
			} `json:"data"`
		}
		res.json(t, &wrap)
		for _, n := range wrap.Data {
			if n.Type == typ {
				return true
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	return false
}

// backdatePeriodEnd forces a subscription's paid period to have already ended,
// so lazy expiry can be exercised without waiting a month.
func (e *authEnv) backdatePeriodEnd(t *testing.T, subscriptionID string) {
	t.Helper()
	if _, err := e.db.DB.Exec(
		`UPDATE subscriptions SET current_period_end = now() - interval '1 hour' WHERE id = $1`,
		subscriptionID); err != nil {
		t.Fatalf("backdate period end: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Plans & pricing
// ---------------------------------------------------------------------------

func TestSubscription_PlansPublicAndPricing(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	// No authentication - a guest may browse pricing.
	res := env.do(t, apiRequest{method: http.MethodGet, path: "/api/v1/subscription/plans"})
	if res.status != http.StatusOK {
		t.Fatalf("plans status = %d, want 200. body: %s", res.status, res.body)
	}
	var wrap struct {
		Data struct {
			Plans []subPlan `json:"plans"`
		} `json:"data"`
	}
	res.json(t, &wrap)

	want := map[string]struct {
		tier   string
		period string
		price  int64
	}{
		"premium_monthly": {"premium", "monthly", 9900},
		"premium_yearly":  {"premium", "yearly", 99000},
		"pro_monthly":     {"pro", "monthly", 19900},
	}
	if len(wrap.Data.Plans) != len(want) {
		t.Fatalf("got %d plans, want %d: %+v", len(wrap.Data.Plans), len(want), wrap.Data.Plans)
	}
	for _, p := range wrap.Data.Plans {
		w, ok := want[p.Code]
		if !ok {
			t.Fatalf("unexpected plan %q", p.Code)
		}
		if p.Tier != w.tier || p.BillingPeriod != w.period || p.PriceMinor != w.price || p.Currency != "THB" {
			t.Errorf("plan %q = %+v, want tier=%s period=%s price=%d THB", p.Code, p, w.tier, w.period, w.price)
		}
	}
}

func TestSubscription_OverviewRequiresAuth(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	res := env.do(t, apiRequest{method: http.MethodGet, path: "/api/v1/subscription"})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("guest overview status = %d, want 401", res.status)
	}
}

// ---------------------------------------------------------------------------
// Full PromptPay lifecycle
// ---------------------------------------------------------------------------

func TestSubscription_FullPromptPayLifecycle(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	reader := env.registerWeb(t)
	staff := env.staffSession(t)

	// 1. Checkout: a pending subscription + pending payment + a PromptPay QR.
	co := env.checkoutOK(t, reader, "premium_monthly")
	if co.Subscription.Status != "pending" || co.Payment.Status != "pending_verification" {
		t.Fatalf("checkout state = sub:%s pay:%s", co.Subscription.Status, co.Payment.Status)
	}
	if co.Payment.AmountMinor != 9900 || co.Payment.Currency != "THB" || co.Payment.Method != "promptpay" {
		t.Fatalf("payment = %+v", co.Payment)
	}
	if !co.PromptPay.Available || co.PromptPay.Payload == "" || co.PromptPay.AmountMinor != 9900 {
		t.Fatalf("promptpay = %+v", co.PromptPay)
	}

	// 2. Before verification the reader has NO entitlement (frontend cannot
	//    activate - activation is a backend transition only).
	before := env.overview(t, reader)
	if before.Tier != "free" || len(before.Entitlements) != 0 {
		t.Fatalf("pre-verify tier = %s entitlements = %v, want free/none", before.Tier, before.Entitlements)
	}

	// 3. Submit the PromptPay slip via the media endpoint.
	slipRes := env.uploadPaymentSlip(t, reader, co.Payment.ID, pngBytes)
	if slipRes.status != http.StatusCreated {
		t.Fatalf("slip upload status = %d, want 201. body: %s", slipRes.status, slipRes.body)
	}
	var slipWrap struct {
		Data subMediaView `json:"data"`
	}
	slipRes.json(t, &slipWrap)
	if slipWrap.Data.MediaType != "payment_slip" {
		t.Fatalf("slip media type = %q", slipWrap.Data.MediaType)
	}
	// The upload response exposes the PRIVATE path, never a public /media/key URL.
	if slipWrap.Data.URL != "/api/v1/media/"+slipWrap.Data.ID+"/private" {
		t.Fatalf("slip URL = %q, want private path", slipWrap.Data.URL)
	}

	// A second slip on the same payment conflicts (evidence already submitted).
	if dup := env.uploadPaymentSlip(t, reader, co.Payment.ID, pngBytes); dup.status != http.StatusConflict {
		t.Fatalf("second slip status = %d, want 409", dup.status)
	}

	// 4. Staff verifies → subscription active, reader entitled.
	vres := env.verifyPayment(t, staff, co.Payment.ID)
	if vres.status != http.StatusOK {
		t.Fatalf("verify status = %d, want 200. body: %s", vres.status, vres.body)
	}

	after := env.overview(t, reader)
	if after.Tier != "premium" {
		t.Fatalf("post-verify tier = %s, want premium", after.Tier)
	}
	if len(after.Entitlements) != 1 || after.Entitlements[0] != "premium" {
		t.Fatalf("entitlements = %v, want [premium]", after.Entitlements)
	}
	if after.Subscription == nil || after.Subscription.Status != "active" {
		t.Fatalf("subscription = %+v, want active", after.Subscription)
	}

	// 5. The reader is notified their Premium is active.
	if !env.awaitNotificationType(t, reader, "subscription_active") {
		t.Error("expected a subscription_active notification")
	}

	// 6. Verifying again does NOT double-activate - the payment is no longer pending.
	if again := env.verifyPayment(t, staff, co.Payment.ID); again.status != http.StatusConflict {
		t.Fatalf("second verify status = %d, want 409", again.status)
	}
}

func TestSubscription_DuplicateLiveCheckoutConflict(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	reader := env.registerWeb(t)

	env.checkoutOK(t, reader, "premium_monthly")
	// A second checkout while one is live is a conflict (one live per user).
	if res := env.checkout(t, reader, "pro_monthly"); res.status != http.StatusConflict {
		t.Fatalf("duplicate checkout status = %d, want 409. body: %s", res.status, res.body)
	}
}

func TestSubscription_RejectExpiresAndAllowsRetry(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	reader := env.registerWeb(t)
	staff := env.staffSession(t)

	co := env.checkoutOK(t, reader, "premium_monthly")
	if slip := env.uploadPaymentSlip(t, reader, co.Payment.ID, pngBytes); slip.status != http.StatusCreated {
		t.Fatalf("slip upload status = %d", slip.status)
	}

	// Staff rejects.
	rej := env.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/admin/subscription/payments/" + co.Payment.ID + "/reject",
		body:    map[string]string{"reason": "amount mismatch"},
		cookies: staff.authCookies(),
		csrf:    staff.csrfToken,
	})
	if rej.status != http.StatusOK {
		t.Fatalf("reject status = %d, want 200. body: %s", rej.status, rej.body)
	}

	// No entitlement, and the reader may now start a fresh checkout.
	ov := env.overview(t, reader)
	if ov.Tier != "free" {
		t.Fatalf("post-reject tier = %s, want free", ov.Tier)
	}
	if ov.LatestPayment == nil || ov.LatestPayment.Status != "rejected" {
		t.Fatalf("latest payment = %+v, want rejected", ov.LatestPayment)
	}
	if retry := env.checkout(t, reader, "premium_monthly"); retry.status != http.StatusCreated {
		t.Fatalf("retry checkout status = %d, want 201 (slot should be freed)", retry.status)
	}
	if !env.awaitNotificationType(t, reader, "subscription_payment_failed") {
		t.Error("expected a subscription_payment_failed notification")
	}
}

func TestSubscription_CancelKeepsAccessThenExpires(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	reader := env.registerWeb(t)
	staff := env.staffSession(t)

	co := env.checkoutOK(t, reader, "premium_monthly")
	env.uploadPaymentSlip(t, reader, co.Payment.ID, pngBytes)
	if v := env.verifyPayment(t, staff, co.Payment.ID); v.status != http.StatusOK {
		t.Fatalf("verify status = %d", v.status)
	}

	// Cancel: status becomes cancelled but access is NOT revoked (brief §10).
	cancel := env.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/subscription/cancel",
		cookies: reader.authCookies(),
		csrf:    reader.csrfToken,
	})
	if cancel.status != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200. body: %s", cancel.status, cancel.body)
	}
	stillOn := env.overview(t, reader)
	if stillOn.Tier != "premium" {
		t.Fatalf("cancelled-but-in-period tier = %s, want premium (access not revoked)", stillOn.Tier)
	}
	if stillOn.Subscription == nil || stillOn.Subscription.Status != "cancelled" {
		t.Fatalf("subscription = %+v, want cancelled", stillOn.Subscription)
	}

	// Once the paid period ends, entitlement is gone (lazy expiry).
	env.backdatePeriodEnd(t, co.Subscription.ID)
	expired := env.overview(t, reader)
	if expired.Tier != "free" {
		t.Fatalf("post-period tier = %s, want free", expired.Tier)
	}
	if expired.Subscription != nil {
		t.Fatalf("expired subscription should read as none, got %+v", expired.Subscription)
	}
}

func TestSubscription_ExpiredLosesEntitlement(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	reader := env.registerWeb(t)
	staff := env.staffSession(t)

	co := env.checkoutOK(t, reader, "pro_monthly")
	env.uploadPaymentSlip(t, reader, co.Payment.ID, pngBytes)
	if v := env.verifyPayment(t, staff, co.Payment.ID); v.status != http.StatusOK {
		t.Fatalf("verify status = %d", v.status)
	}
	if ov := env.overview(t, reader); ov.Tier != "pro" {
		t.Fatalf("active tier = %s, want pro", ov.Tier)
	}

	env.backdatePeriodEnd(t, co.Subscription.ID)
	if ov := env.overview(t, reader); ov.Tier != "free" || len(ov.Entitlements) != 0 {
		t.Fatalf("expired tier = %s entitlements = %v, want free/none", ov.Tier, ov.Entitlements)
	}
}

// ---------------------------------------------------------------------------
// Private payment-slip access (addendum §9–§11, §25)
// ---------------------------------------------------------------------------

func TestSubscription_PrivatePaymentSlipAccess(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.registerWeb(t)
	stranger := env.registerWeb(t)
	staff := env.staffSession(t)

	co := env.checkoutOK(t, owner, "premium_monthly")
	slipRes := env.uploadPaymentSlip(t, owner, co.Payment.ID, pngBytes)
	if slipRes.status != http.StatusCreated {
		t.Fatalf("slip upload status = %d", slipRes.status)
	}
	var slip struct {
		Data subMediaView `json:"data"`
	}
	slipRes.json(t, &slip)
	privatePath := "/api/v1/media/" + slip.Data.ID + "/private"

	// Owner can fetch their own slip, byte-identical, with a PRIVATE cache policy.
	ownerFetch := env.do(t, apiRequest{method: http.MethodGet, path: privatePath, cookies: owner.authCookies()})
	if ownerFetch.status != http.StatusOK || !bytes.Equal(ownerFetch.body, pngBytes) {
		t.Fatalf("owner private fetch status = %d, %d bytes", ownerFetch.status, len(ownerFetch.body))
	}
	if cc := ownerFetch.header.Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("private slip cache-control = %q, want private, no-store", cc)
	}

	// A stranger gets the non-oracle 404 - never confirmation the slip exists.
	if s := env.do(t, apiRequest{method: http.MethodGet, path: privatePath, cookies: stranger.authCookies()}); s.status != http.StatusNotFound {
		t.Fatalf("stranger private fetch status = %d, want 404", s.status)
	}
	// A guest is unauthenticated.
	if g := env.do(t, apiRequest{method: http.MethodGet, path: privatePath}); g.status != http.StatusUnauthorized {
		t.Fatalf("guest private fetch status = %d, want 401", g.status)
	}
	// Staff may view it (verification work).
	if st := env.do(t, apiRequest{method: http.MethodGet, path: privatePath, cookies: staff.authCookies()}); st.status != http.StatusOK {
		t.Fatalf("staff private fetch status = %d, want 200", st.status)
	}

	// The PUBLIC /media/*key route must REFUSE the slip, even with its real key.
	var objectKey string
	if err := env.db.DB.QueryRow(`SELECT object_key FROM media WHERE id = $1`, slip.Data.ID).Scan(&objectKey); err != nil {
		t.Fatalf("read slip object key: %v", err)
	}
	if pub := env.fetchFile(t, "/media/"+objectKey); pub.status != http.StatusNotFound {
		t.Fatalf("public serve of a payment slip status = %d, want 404 (must not be public)", pub.status)
	}
}

func TestSubscription_PaymentSlipUploadAuthorization(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.registerWeb(t)
	stranger := env.registerWeb(t)

	co := env.checkoutOK(t, owner, "premium_monthly")

	// A guest cannot upload.
	if g := env.uploadPaymentSlip(t, webSession{}, co.Payment.ID, pngBytes); g.status != http.StatusUnauthorized {
		t.Fatalf("guest slip upload = %d, want 401", g.status)
	}
	// Missing payment ref → 422.
	if m := env.uploadPaymentSlip(t, owner, "", pngBytes); m.status != http.StatusUnprocessableEntity {
		t.Fatalf("missing payment ref = %d, want 422", m.status)
	}
	// A stranger cannot attach a slip to someone else's payment → non-oracle 404.
	if s := env.uploadPaymentSlip(t, stranger, co.Payment.ID, pngBytes); s.status != http.StatusNotFound {
		t.Fatalf("cross-user slip upload = %d, want 404", s.status)
	}
	// A non-image file is rejected by the media validator (422).
	if bad := env.uploadPaymentSlip(t, owner, co.Payment.ID, []byte("this is not an image")); bad.status != http.StatusUnprocessableEntity {
		t.Fatalf("non-image slip = %d, want 422", bad.status)
	}
}

func TestSubscription_NonStaffCannotVerify(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.registerWeb(t)
	other := env.registerWeb(t)

	co := env.checkoutOK(t, owner, "premium_monthly")
	env.uploadPaymentSlip(t, owner, co.Payment.ID, pngBytes)

	// A regular user cannot reach the staff verification surface.
	if res := env.verifyPayment(t, other, co.Payment.ID); res.status != http.StatusForbidden {
		t.Fatalf("non-staff verify status = %d, want 403", res.status)
	}
	// And the owner cannot self-verify either.
	if res := env.verifyPayment(t, owner, co.Payment.ID); res.status != http.StatusForbidden {
		t.Fatalf("self verify status = %d, want 403", res.status)
	}
}

// In DISABLED mode the ACQUISITION surface is off, but pricing and the caller's
// own overview stay available (demo-mode brief §4, §19: "GET pricing →
// available but purchase/demo activation disabled"). Nobody becomes premium.
func TestSubscription_DisabledMode(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t, withSubscriptionDisabled())
	reader := env.registerWeb(t)

	// Pricing is available (a guest may still see "coming soon" with real prices),
	// and reports the disabled mode.
	pres := env.do(t, apiRequest{method: http.MethodGet, path: "/api/v1/subscription/plans"})
	if pres.status != http.StatusOK {
		t.Fatalf("plans while disabled = %d, want 200 (available)", pres.status)
	}
	var pwrap struct {
		Data subPricing `json:"data"`
	}
	pres.json(t, &pwrap)
	if pwrap.Data.Mode != "disabled" {
		t.Errorf("pricing mode = %q, want disabled", pwrap.Data.Mode)
	}
	if pwrap.Data.Demo != nil {
		t.Errorf("disabled mode must offer no demo, got %+v", pwrap.Data.Demo)
	}

	// Overview is available and reports the user as free with no demo block.
	ov := env.overview(t, reader)
	if ov.Tier != "free" || ov.Mode != "disabled" || ov.Demo != nil {
		t.Fatalf("disabled overview = tier:%s mode:%s demo:%+v, want free/disabled/nil", ov.Tier, ov.Mode, ov.Demo)
	}

	// Paid checkout and demo activation are BOTH off.
	if res := env.checkout(t, reader, "premium_monthly"); res.status != http.StatusServiceUnavailable {
		t.Fatalf("checkout while disabled = %d, want 503", res.status)
	}
	if res := env.activateDemo(t, reader); res.status != http.StatusServiceUnavailable {
		t.Fatalf("demo activation while disabled = %d, want 503", res.status)
	}
}
