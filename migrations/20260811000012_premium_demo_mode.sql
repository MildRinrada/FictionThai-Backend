-- Phase 11 (addendum) - Premium/Pro Demo Mode
-- (docs/MONETIZATION.md §P11-DEMO; brief "Premium/Pro Demo Mode & Production
--  Monetization Toggle").
--
-- Goal: let real users experience Premium/Pro FOR FREE during a launch demo,
-- behind a single environment switch (SUBSCRIPTION_MODE = disabled | demo |
-- live), WITHOUT ever fabricating a payment. This migration is the whole of the
-- schema change: one column and one index. It does NOT add a payment row, a
-- wallet, a balance, a ledger, or a payout - a demo grant is deliberately NOT a
-- financial record (brief §2, §7, §26).
--
-- Design:
--
--   * subscriptions.source distinguishes how the entitlement was OBTAINED:
--     'paid'  - the existing PromptPay-verified flow (the default; every row
--               created before this migration is paid, and the DEFAULT backfills
--               them correctly).
--     'demo'  - a free launch-demo grant. Same lifecycle, same entitlement
--               resolution, same lazy expiry as a paid subscription - only the
--               ACQUISITION method differs (brief §7, §25 "the entitlement layer
--               is the common abstraction; only the method of obtaining it
--               changes"). A demo NEVER has a subscription_payments row.
--
--     This EXTENDS the existing entitlement model rather than adding a second,
--     competing one (brief §7). The hot path - FindLiveForUser → entitledAt - is
--     untouched: a demo is simply an active subscription whose source is 'demo'.
--
--   * The critical invariant (brief §7): demo access must NEVER be read as
--     evidence that money was paid. source='demo' is that evidence-of-absence,
--     and the staff payment-review queue reads subscription_payments (which a
--     demo never touches), so a demo user can never appear as "awaiting payment
--     verification" (brief §17).
--
--   * ONE demo per user, EVER (brief §6 "each user can receive the demo once";
--     "the database should enforce uniqueness"). A partial UNIQUE index on
--     source='demo' - for ANY status, including 'expired' - so an expired demo
--     still blocks a second free trial. This is distinct from
--     subscriptions_one_live_per_user, which frees its slot on expiry to allow a
--     fresh PAID subscription; the demo slot never frees. A user therefore gets
--     one free trial, and afterwards may still PAY (the live slot is free).
--
--   * Switching SUBSCRIPTION_MODE (demo ↔ live ↔ disabled) changes only what NEW
--     users may do; it writes NOTHING here (brief §13). Existing subscriptions -
--     paid or demo - keep their stored state and expiry, and a demo row stays a
--     demo row forever. No mode change converts a demo into a paid subscriber.

-- +migrate Up

-- source: how the entitlement was obtained. NOT NULL with a DEFAULT so existing
-- rows backfill to 'paid' (they were all paid-flow), and the paid INSERT path
-- need not mention the column.
ALTER TABLE subscriptions
    ADD COLUMN source VARCHAR(16) NOT NULL DEFAULT 'paid';

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_source_valid CHECK (source IN ('paid', 'demo'));

COMMENT ON COLUMN subscriptions.source IS
    'How the entitlement was obtained: paid (PromptPay-verified) or demo (free launch-trial grant). A demo is NEVER a payment record and must never be read as proof of payment.';

-- One free demo per user, ever - any status. Partial, so it never touches paid
-- rows. Persists through expiry (unlike subscriptions_one_live_per_user), so a
-- spent trial cannot be re-claimed by letting it lapse (brief §6).
CREATE UNIQUE INDEX subscriptions_one_demo_per_user
    ON subscriptions (user_id)
    WHERE source = 'demo';

-- +migrate Down

DROP INDEX IF EXISTS subscriptions_one_demo_per_user;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_source_valid;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS source;
