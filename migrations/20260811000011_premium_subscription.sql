-- Phase 11 - Premium Subscription (platform-owned)
-- (docs/08 - Database Design.md §32/§44 Phase 11; docs/MONETIZATION.md;
--  docs/01 §30; docs/07 §39; docs/11 payment protection).
--
-- This is the FIRST and ONLY monetization feature: PLATFORM-owned Premium/Pro
-- subscriptions. Revenue belongs to FictionThai. It is deliberately NARROW -
-- three tables and one column - and does NOT create any of the writer-facing
-- money entities docs/08 §32 lists as future work (no creator_plans, orders,
-- entitlements table, donations, payouts, wallet, or balance). Writer support is
-- an EXTERNAL EasyDonate link (author_profiles.donation_url below); FictionThai
-- never receives, holds, splits, or distributes that money.
--
-- Design notes:
--
--   * MONEY is stored as an INTEGER minor unit (satang) plus an explicit ISO
--     currency - never floating point (docs/MONETIZATION.md §5, brief §9).
--     99 THB = 9900, 990 THB = 99000, 199 THB = 19900.
--
--   * subscription_plans is REFERENCE data, seeded here with exactly the three
--     confirmed plans. Prices live in the database, the single source of truth
--     (brief §9) - never hard-coded in the service. tier/billing_period carry a
--     CHECK because both are FIXED, closed sets; a value outside them is a bug.
--
--   * subscriptions.status is a FIXED lifecycle (pending → active → cancelled /
--     expired), so it carries a CHECK. A partial UNIQUE index enforces AT MOST
--     ONE live (pending/active/cancelled) subscription per user - this is the
--     database half of "prevent duplicate active subscription" (brief §21); the
--     service enforces the transitions.
--
--   * payment.method stays VARCHAR WITHOUT a CHECK, the same reasoning as
--     ai_requests.provider and media.media_type: Phase 1 is 'promptpay', and a
--     Phase 2 payment gateway must be able to add a method identifier without a
--     migration (docs/MONETIZATION.md §6, brief §5). payment.status DOES carry a
--     CHECK: verification is a closed set (pending_verification/verified/
--     rejected), SEPARATE from subscription status (brief §10).
--
--   * PAYMENT EVIDENCE (the PromptPay slip) reuses the Phase 9 MEDIA domain for
--     its bytes, validation, and lifecycle (addendum §12): the slip is uploaded
--     as a media object of the new PRIVATE 'payment_slip' type and referenced
--     here by a normal FK - payment_slip_media_id → media (addendum §13, "prefer
--     a normal FK over polymorphic ownership"). The bytes never enter PostgreSQL;
--     the media row holds a GENERATED key (docs/07 §22). UNLIKE avatars/covers,
--     a payment_slip is PRIVATE: the public /media/*key route refuses it and it
--     is served only through an owner-or-staff-authorized route (addendum §9–§11,
--     §14). payment_slip_media_id is UNIQUE - one slip per attempt, and a media
--     object can back at most one payment (a reuse guard; addendum §13).
--
--   * NO card numbers, bank credentials, or payment-provider secrets are stored
--     anywhere (brief §13, docs/11). The platform holds none of that - PromptPay
--     settles directly, and Phase 2's gateway keeps secrets in config, not rows.
--
--   * reviewed_by is the STAFF verifier, kept for audit (brief §13 "audit who
--     verified/rejected"). ON DELETE SET NULL preserves the payment row if that
--     staff account is later removed.
--
--   * user_id / subscription_id CASCADE like every user-owned row (docs/08 §38):
--     deleting a user takes their subscription and payment history with them.
--     plan_id does NOT cascade - a plan can never vanish out from under the
--     subscriptions that reference it (it is deactivated, never deleted).

-- +migrate Up

-- ---------------------------------------------------------------------------
-- subscription_plans - the three confirmed Premium/Pro plans (reference data)
-- ---------------------------------------------------------------------------
CREATE TABLE subscription_plans (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    code           VARCHAR(32) NOT NULL UNIQUE,
    tier           VARCHAR(16) NOT NULL,
    billing_period VARCHAR(16) NOT NULL,
    price_minor    BIGINT      NOT NULL,
    currency       VARCHAR(3)  NOT NULL DEFAULT 'THB',
    active         BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT subscription_plans_tier_valid     CHECK (tier IN ('premium', 'pro')),
    CONSTRAINT subscription_plans_period_valid   CHECK (billing_period IN ('monthly', 'yearly')),
    CONSTRAINT subscription_plans_price_positive CHECK (price_minor > 0),
    CONSTRAINT subscription_plans_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT subscription_plans_code_not_blank CHECK (btrim(code) <> '')
);

-- The three confirmed plans (docs/MONETIZATION.md §3, brief §2). Seeded once;
-- ON CONFLICT keeps a re-run harmless.
INSERT INTO subscription_plans (code, tier, billing_period, price_minor, currency) VALUES
    ('premium_monthly', 'premium', 'monthly',  9900, 'THB'),
    ('premium_yearly',  'premium', 'yearly',  99000, 'THB'),
    ('pro_monthly',     'pro',     'monthly', 19900, 'THB')
ON CONFLICT (code) DO NOTHING;

-- ---------------------------------------------------------------------------
-- subscriptions - one live subscription per user
-- ---------------------------------------------------------------------------
CREATE TABLE subscriptions (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    plan_id              UUID        NOT NULL REFERENCES subscription_plans (id),

    -- Denormalised snapshot of the plan's tier, so the hot entitlement check is
    -- one indexed row read and never a join. The plan a subscription points at
    -- never changes tier (a new tier is a new plan), so this cannot drift.
    tier                 VARCHAR(16) NOT NULL,
    status               VARCHAR(16) NOT NULL DEFAULT 'pending',

    current_period_start TIMESTAMPTZ,
    current_period_end   TIMESTAMPTZ,
    cancelled_at         TIMESTAMPTZ,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT subscriptions_tier_valid   CHECK (tier IN ('premium', 'pro')),
    CONSTRAINT subscriptions_status_valid CHECK (
        status IN ('pending', 'active', 'cancelled', 'expired')
    )
);

-- AT MOST ONE live subscription per user (brief §21 "unique constraint
-- preventing duplicate active subscriptions"). 'cancelled' is still live because
-- it retains already-paid entitlement until current_period_end (brief §10 - do
-- not revoke paid access); only 'expired' (period ended, or payment rejected
-- before activation) frees the slot for a fresh subscription.
CREATE UNIQUE INDEX subscriptions_one_live_per_user
    ON subscriptions (user_id)
    WHERE status IN ('pending', 'active', 'cancelled');

-- The lazy-expiry sweep and the user's own lookup both hit rows by user.
CREATE INDEX subscriptions_user_idx ON subscriptions (user_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- subscription_payments - one row per PromptPay payment attempt + its evidence
-- ---------------------------------------------------------------------------
CREATE TABLE subscription_payments (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id       UUID        NOT NULL REFERENCES subscriptions (id) ON DELETE CASCADE,
    user_id               UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    plan_id               UUID        NOT NULL REFERENCES subscription_plans (id),

    -- Snapshot of what was owed at attempt time - the price is read from the
    -- plan and frozen here so later plan-price edits never rewrite history.
    amount_minor          BIGINT      NOT NULL,
    currency              VARCHAR(3)  NOT NULL,
    method                VARCHAR(16) NOT NULL DEFAULT 'promptpay',
    status                VARCHAR(24) NOT NULL DEFAULT 'pending_verification',

    -- The uploaded PromptPay slip, referenced in the MEDIA table as a private
    -- 'payment_slip' object (addendum §13). NULL until the reader submits one.
    -- ON DELETE SET NULL: if the media row is ever hard-deleted the payment
    -- audit row survives. UNIQUE: one slip per attempt, and a given media object
    -- backs at most one payment.
    payment_slip_media_id UUID        REFERENCES media (id) ON DELETE SET NULL,
    evidence_submitted_at TIMESTAMPTZ,

    -- Manual verification audit trail (Phase 1). Replaced by a gateway webhook in
    -- Phase 2 (docs/MONETIZATION.md §6) without touching these columns.
    reviewed_by           UUID        REFERENCES users (id) ON DELETE SET NULL,
    reviewed_at           TIMESTAMPTZ,
    reject_reason         VARCHAR(64),

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT subscription_payments_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT subscription_payments_currency_valid  CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT subscription_payments_status_valid     CHECK (
        status IN ('pending_verification', 'verified', 'rejected')
    ),
    CONSTRAINT subscription_payments_method_not_blank CHECK (btrim(method) <> '')
);

-- One media object backs at most one payment (a reuse guard; addendum §13).
-- Partial, so many NULLs (unsubmitted attempts) do not collide.
CREATE UNIQUE INDEX subscription_payments_slip_media_idx
    ON subscription_payments (payment_slip_media_id)
    WHERE payment_slip_media_id IS NOT NULL;

-- The caller's own payment history, newest first.
CREATE INDEX subscription_payments_user_idx
    ON subscription_payments (user_id, created_at DESC);

-- The staff review queue: only attempts still awaiting verification.
CREATE INDEX subscription_payments_pending_idx
    ON subscription_payments (created_at)
    WHERE status = 'pending_verification';

CREATE INDEX subscription_payments_subscription_idx
    ON subscription_payments (subscription_id);

-- ---------------------------------------------------------------------------
-- author_profiles.donation_url - the EXTERNAL writer-support link (EasyDonate)
--
-- This is the ONLY platform responsibility for writer support: store and present
-- the writer's own external donation destination (docs/MONETIZATION.md §8.2,
-- brief §6, §15). FictionThai does NOT process, hold, or account for this money.
-- It is deliberately NOT a donations table, a transaction, a balance, or a
-- payout - it is a URL. Validated https-only in the service.
-- ---------------------------------------------------------------------------
ALTER TABLE author_profiles ADD COLUMN donation_url TEXT;

COMMENT ON COLUMN author_profiles.donation_url IS
    'External writer-support link (e.g. EasyDonate). FictionThai stores and displays it only; it never processes this money. Not a platform donation record.';

-- +migrate Down

ALTER TABLE author_profiles DROP COLUMN IF EXISTS donation_url;
DROP TABLE IF EXISTS subscription_payments;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS subscription_plans;
