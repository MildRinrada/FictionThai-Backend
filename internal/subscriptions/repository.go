package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// Sentinel errors. The service translates each into the right API response: a
// non-oracle 404 for ErrNotFound, a 409 for the two conflict cases.
var (
	// ErrNotFound covers "no such plan / subscription / payment".
	ErrNotFound = errors.New("subscription record not found")
	// ErrLiveSubscription is CreatePending/CreateDemo hitting the
	// one-live-per-user unique index: the caller already has a
	// pending/active/cancelled subscription.
	ErrLiveSubscription = errors.New("user already has a live subscription")
	// ErrDemoAlreadyUsed is CreateDemo hitting the one-demo-per-user unique
	// index: the caller has already spent their single free trial (brief §6).
	ErrDemoAlreadyUsed = errors.New("user has already used the demo")
	// ErrConflict is a guarded transition that matched no row - the state moved
	// under the caller (e.g. verifying a payment that is no longer pending).
	ErrConflict = errors.New("subscription state conflict")
)

// Repository is the only place that reads or writes the subscription tables.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

type scanner interface{ Scan(...any) error }

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

const planColumns = `id, code, tier, billing_period, price_minor, currency, active`

func scanPlan(row scanner) (*Plan, error) {
	var p Plan
	err := row.Scan(&p.ID, &p.Code, &p.Tier, &p.BillingPeriod, &p.PriceMinor, &p.Currency, &p.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan subscription plan: %w", err)
	}
	return &p, nil
}

// ActivePlans returns the plans on offer, cheapest first.
func (r *Repository) ActivePlans(ctx context.Context) ([]Plan, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+planColumns+` FROM subscription_plans WHERE active ORDER BY price_minor`)
	if err != nil {
		return nil, fmt.Errorf("list subscription plans: %w", err)
	}
	defer rows.Close()

	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// PlanByCode loads one ACTIVE plan by its code.
func (r *Repository) PlanByCode(ctx context.Context, code string) (*Plan, error) {
	return scanPlan(r.db.QueryRowContext(ctx,
		`SELECT `+planColumns+` FROM subscription_plans WHERE code = $1 AND active`, code))
}

// PlanByID loads one plan by id (used to reconstruct a payment's billing term).
func (r *Repository) PlanByID(ctx context.Context, id uuid.UUID) (*Plan, error) {
	return scanPlan(r.db.QueryRowContext(ctx,
		`SELECT `+planColumns+` FROM subscription_plans WHERE id = $1`, id))
}

// PlanForTier returns a representative ACTIVE plan for a tier - the cheapest one,
// chosen deterministically. A demo subscription references it only to satisfy
// the plan_id FK and carry the tier; the demo's period comes from configuration,
// NOT the plan's billing period, so which of a tier's plans is picked is
// immaterial to the demo (brief §5). ErrNotFound means the tier has no active
// plan (a misconfiguration the service surfaces as an internal error).
func (r *Repository) PlanForTier(ctx context.Context, tier Tier) (*Plan, error) {
	return scanPlan(r.db.QueryRowContext(ctx,
		`SELECT `+planColumns+`
		 FROM subscription_plans WHERE tier = $1 AND active
		 ORDER BY price_minor LIMIT 1`, tier))
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

// subRawColumns are the subscriptions columns in table order; PlanCode is set
// separately by the caller (which holds the plan) after an INSERT/UPDATE.
const subRawColumns = `
	id, user_id, plan_id, tier, status, source,
	current_period_start, current_period_end, cancelled_at, created_at, updated_at`

func scanSubscriptionRaw(row scanner) (*Subscription, error) {
	var s Subscription
	err := row.Scan(
		&s.ID, &s.UserID, &s.PlanID, &s.Tier, &s.Status, &s.Source,
		&s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.CancelledAt, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan subscription: %w", err)
	}
	return &s, nil
}

// subJoinedColumns add the plan code via a join, for reads.
const subJoinedColumns = `
	s.id, s.user_id, s.plan_id, pl.code, s.tier, s.status, s.source,
	s.current_period_start, s.current_period_end, s.cancelled_at, s.created_at, s.updated_at
	FROM subscriptions s JOIN subscription_plans pl ON pl.id = s.plan_id`

func scanSubscriptionJoined(row scanner) (*Subscription, error) {
	var s Subscription
	err := row.Scan(
		&s.ID, &s.UserID, &s.PlanID, &s.PlanCode, &s.Tier, &s.Status, &s.Source,
		&s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.CancelledAt, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan subscription: %w", err)
	}
	return &s, nil
}

// CreatePending inserts a new pending subscription for a plan. The partial
// UNIQUE index rejects a second live subscription for the same user; that
// violation surfaces as ErrLiveSubscription so the service can answer 409.
func (r *Repository) CreatePending(
	ctx context.Context, userID uuid.UUID, plan *Plan,
) (*Subscription, error) {
	sub, err := scanSubscriptionRaw(r.db.QueryRowContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, tier, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING `+subRawColumns,
		userID, plan.ID, plan.Tier))
	if err != nil {
		if isLiveSubscriptionViolation(err) {
			return nil, ErrLiveSubscription
		}
		return nil, err
	}
	sub.PlanCode = plan.Code
	return sub, nil
}

// CreateDemo inserts an ACTIVE, free demo subscription (source='demo') whose
// paid period is [start, end) - the entitlement is granted immediately, with NO
// payment row anywhere (brief §2, §4). Two partial unique indexes guard it:
// one-live-per-user (the caller already has a live subscription → ErrLiveSubscription)
// and one-demo-per-user (the caller already spent their trial → ErrDemoAlreadyUsed).
// The DB is the race guard: two concurrent activations cannot both win.
func (r *Repository) CreateDemo(
	ctx context.Context, userID uuid.UUID, plan *Plan, start, end time.Time,
) (*Subscription, error) {
	sub, err := scanSubscriptionRaw(r.db.QueryRowContext(ctx, `
		INSERT INTO subscriptions
			(user_id, plan_id, tier, status, source, current_period_start, current_period_end)
		VALUES ($1, $2, $3, 'active', 'demo', $4, $5)
		RETURNING `+subRawColumns,
		userID, plan.ID, plan.Tier, start, end))
	if err != nil {
		// Order matters: a user who already demoed AND has a live sub could trip
		// either index; the demo-specific message is the more informative, so
		// check it first.
		if isDemoSubscriptionViolation(err) {
			return nil, ErrDemoAlreadyUsed
		}
		if isLiveSubscriptionViolation(err) {
			return nil, ErrLiveSubscription
		}
		return nil, err
	}
	sub.PlanCode = plan.Code
	return sub, nil
}

// HasDemoSubscription reports whether the user has EVER activated a demo, of any
// status (including expired). Backs the "one demo per user, ever" eligibility
// check (brief §6).
func (r *Repository) HasDemoSubscription(ctx context.Context, userID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM subscriptions WHERE user_id = $1 AND source = 'demo')`,
		userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check demo subscription: %w", err)
	}
	return exists, nil
}

// FindLiveForUser returns the caller's one live (pending/active/cancelled)
// subscription, or ErrNotFound. The unique index guarantees there is at most one.
func (r *Repository) FindLiveForUser(ctx context.Context, userID uuid.UUID) (*Subscription, error) {
	return scanSubscriptionJoined(r.db.QueryRowContext(ctx,
		`SELECT `+subJoinedColumns+`
		 WHERE s.user_id = $1 AND s.status IN ('pending', 'active', 'cancelled')`, userID))
}

// FindSubscription loads one subscription by id.
func (r *Repository) FindSubscription(ctx context.Context, id uuid.UUID) (*Subscription, error) {
	return scanSubscriptionJoined(r.db.QueryRowContext(ctx,
		`SELECT `+subJoinedColumns+` WHERE s.id = $1`, id))
}

// rowChanged runs a guarded UPDATE and reports whether exactly one row matched.
func (r *Repository) rowChanged(ctx context.Context, op, query string, args ...any) (bool, error) {
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	return n == 1, nil
}

// MarkCancelled cancels an ACTIVE subscription. Entitlement continues until the
// period end (the service does not clear the period), then lazy expiry finishes.
func (r *Repository) MarkCancelled(ctx context.Context, id uuid.UUID) (bool, error) {
	return r.rowChanged(ctx, "cancel subscription", `
		UPDATE subscriptions SET status = 'cancelled', cancelled_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'active'`, id)
}

// AbandonPending expires a PENDING subscription (the reader gave up before
// paying), freeing the one-live slot for a fresh attempt.
func (r *Repository) AbandonPending(ctx context.Context, id uuid.UUID) (bool, error) {
	return r.rowChanged(ctx, "abandon subscription", `
		UPDATE subscriptions SET status = 'expired', updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id)
}

// ExpireIfPast marks an active/cancelled subscription expired once its paid
// period has ended. It is the lazy-expiry step run on read; it is idempotent and
// safe to call on any subscription.
func (r *Repository) ExpireIfPast(ctx context.Context, id uuid.UUID) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'expired', updated_at = now()
		WHERE id = $1 AND status IN ('active', 'cancelled')
		  AND current_period_end IS NOT NULL AND current_period_end <= now()`, id)
	if err != nil {
		return false, fmt.Errorf("expire subscription: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("expire subscription: %w", err)
	}
	return n == 1, nil
}

// ---------------------------------------------------------------------------
// Payments
// ---------------------------------------------------------------------------

const paymentColumns = `
	id, subscription_id, user_id, plan_id, amount_minor, currency, method, status,
	payment_slip_media_id, evidence_submitted_at, reviewed_by, reviewed_at,
	reject_reason, created_at, updated_at`

func scanPayment(row scanner) (*Payment, error) {
	var p Payment
	err := row.Scan(
		&p.ID, &p.SubscriptionID, &p.UserID, &p.PlanID, &p.AmountMinor, &p.Currency,
		&p.Method, &p.Status, &p.PaymentSlipMediaID, &p.EvidenceSubmittedAt,
		&p.ReviewedBy, &p.ReviewedAt, &p.RejectReason, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan subscription payment: %w", err)
	}
	return &p, nil
}

// CreatePendingPayment records a pending payment attempt for a subscription,
// freezing the plan price as the amount owed.
func (r *Repository) CreatePendingPayment(
	ctx context.Context, subscriptionID, userID uuid.UUID, plan *Plan,
) (*Payment, error) {
	return scanPayment(r.db.QueryRowContext(ctx, `
		INSERT INTO subscription_payments
			(subscription_id, user_id, plan_id, amount_minor, currency, method, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending_verification')
		RETURNING `+paymentColumns,
		subscriptionID, userID, plan.ID, plan.PriceMinor, plan.Currency, MethodPromptPay))
}

// FindPayment loads one payment by id; the service decides authorization.
func (r *Repository) FindPayment(ctx context.Context, id uuid.UUID) (*Payment, error) {
	return scanPayment(r.db.QueryRowContext(ctx,
		`SELECT `+paymentColumns+` FROM subscription_payments WHERE id = $1`, id))
}

// FindLatestPaymentForUser returns the caller's most recent payment, or
// ErrNotFound if they have none.
func (r *Repository) FindLatestPaymentForUser(ctx context.Context, userID uuid.UUID) (*Payment, error) {
	return scanPayment(r.db.QueryRowContext(ctx,
		`SELECT `+paymentColumns+`
		 FROM subscription_payments WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT 1`, userID))
}

// AttachEvidence links an uploaded slip (a media id) to a PENDING payment owned
// by userID that does not already have one. Returns false if the payment is not
// the caller's, not pending, or already carries evidence.
func (r *Repository) AttachEvidence(
	ctx context.Context, paymentID, userID, mediaID uuid.UUID,
) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE subscription_payments
		SET payment_slip_media_id = $3, evidence_submitted_at = now(), updated_at = now()
		WHERE id = $1 AND user_id = $2
		  AND status = 'pending_verification' AND payment_slip_media_id IS NULL`,
		paymentID, userID, mediaID)
	if err != nil {
		return false, fmt.Errorf("attach payment evidence: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("attach payment evidence: %w", err)
	}
	return n == 1, nil
}

// PendingReviewQueue returns payments awaiting verification that have a slip
// attached (only those are reviewable), newest first, for staff.
func (r *Repository) PendingReviewQueue(
	ctx context.Context, page pagination.Params,
) ([]Payment, int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM subscription_payments
		WHERE status = 'pending_verification' AND payment_slip_media_id IS NOT NULL`).
		Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count review queue: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT `+paymentColumns+`
		FROM subscription_payments
		WHERE status = 'pending_verification' AND payment_slip_media_id IS NOT NULL
		ORDER BY created_at, id
		LIMIT $1 OFFSET $2`, page.Limit(), page.Offset())
	if err != nil {
		return nil, 0, fmt.Errorf("list review queue: %w", err)
	}
	defer rows.Close()

	var out []Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *p)
	}
	return out, total, rows.Err()
}

// Verify confirms a payment and activates its subscription in ONE transaction.
// Both updates are guarded: the payment must still be pending_verification WITH a
// slip, and the subscription must still be pending. If either guard fails, the
// whole transaction rolls back and ErrConflict is returned - Premium is never
// half-activated, and a duplicate confirmation cannot double-activate.
func (r *Repository) Verify(
	ctx context.Context, paymentID, reviewerID uuid.UUID,
	periodStart, periodEnd time.Time,
) (*Payment, *Subscription, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin verify: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	payment, err := scanPayment(tx.QueryRowContext(ctx, `
		UPDATE subscription_payments
		SET status = 'verified', reviewed_by = $2, reviewed_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'pending_verification' AND payment_slip_media_id IS NOT NULL
		RETURNING `+paymentColumns, paymentID, reviewerID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil, ErrConflict
	}
	if err != nil {
		return nil, nil, err
	}

	sub, err := scanSubscriptionRaw(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET status = 'active', current_period_start = $2, current_period_end = $3, updated_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING `+subRawColumns, payment.SubscriptionID, periodStart, periodEnd))
	if errors.Is(err, ErrNotFound) {
		// The subscription was not pending - do not verify the payment either.
		return nil, nil, ErrConflict
	}
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit verify: %w", err)
	}
	return payment, sub, nil
}

// Reject marks a pending payment rejected and expires its still-pending
// subscription (freeing the slot so the reader can retry) in one transaction.
// Returns ErrConflict if the payment was no longer pending.
func (r *Repository) Reject(
	ctx context.Context, paymentID, reviewerID uuid.UUID, reason string,
) (*Payment, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin reject: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	payment, err := scanPayment(tx.QueryRowContext(ctx, `
		UPDATE subscription_payments
		SET status = 'rejected', reviewed_by = $2, reviewed_at = now(),
		    reject_reason = $3, updated_at = now()
		WHERE id = $1 AND status = 'pending_verification'
		RETURNING `+paymentColumns, paymentID, reviewerID, reasonArg))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}

	// Expire the pending subscription so the reader can start over. If it is not
	// pending (unexpected), leave it - the payment rejection still stands.
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'expired', updated_at = now()
		WHERE id = $1 AND status = 'pending'`, payment.SubscriptionID); err != nil {
		return nil, fmt.Errorf("expire subscription on reject: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reject: %w", err)
	}
	return payment, nil
}

// isLiveSubscriptionViolation matches the one-live-per-user unique index by
// name, the same string-match approach the other repositories use rather than
// importing the driver's error type.
func isLiveSubscriptionViolation(err error) bool {
	return isUniqueViolationOn(err, "subscriptions_one_live_per_user")
}

// isDemoSubscriptionViolation matches the one-demo-per-user unique index by name
// (brief §6 - one free trial per user, ever).
func isDemoSubscriptionViolation(err error) bool {
	return isUniqueViolationOn(err, "subscriptions_one_demo_per_user")
}

// isUniqueViolationOn reports whether err is a Postgres unique-violation naming
// the given constraint/index.
func isUniqueViolationOn(err error, constraint string) bool {
	msg := err.Error()
	if !strings.Contains(msg, "SQLSTATE 23505") && !strings.Contains(msg, "duplicate key value") {
		return false
	}
	return strings.Contains(msg, constraint)
}
