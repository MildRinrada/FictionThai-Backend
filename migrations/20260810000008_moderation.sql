-- Phase 8 - Moderation
-- (docs/08 - Database Design.md §24 and §44 Phase 8; docs/01 §21; docs/11 §38–§39).
--
-- Creates reports and moderation_actions. Nothing speculative: no appeals,
-- no report categories table, no per-target report tables - docs/08 §24.1
-- models ONE polymorphic reports table, and docs/03's admin "Appeals" node is
-- a later product decision, not a Phase 8 table.
--
-- Design notes:
--
--   * reports.status is CHECKed because §24.1 enumerates the lifecycle
--     (pending / reviewing / resolved / rejected). reason, target_type, and
--     moderation_actions.action stay VARCHAR without a CHECK for the same
--     reason notifications.type does: their documented lists are examples or
--     live in other documents (docs/01 §21, docs/11 §38, docs/02 §46), and
--     growing a vocabulary must not need a migration. The service enforces
--     the current allowlists.
--
--   * The partial unique index is the duplicate-report guard. The docs are
--     silent on duplicates, but docs/01 §21 requires the platform to handle
--     spam and abuse - and an unguarded report endpoint is itself a spam
--     vector aimed at the moderator queue. One OPEN report per
--     (reporter, target); filing again while it is open idempotently returns
--     the existing report, and re-reporting after resolution stays possible
--     because resolved/rejected rows leave the partial index.
--
--   * reports has no updated_at on purpose - §24.1 defines created_at and
--     resolved_at only; the lifecycle is the update.
--
--   * moderation_actions is APPEND-ONLY (§24.2): no updated_at, no
--     deleted_at, no UPDATE path in the repository. The audit trail is part
--     of the security model (docs/11 §39 "Sensitive operations should be
--     logged").
--
--   * reporter_id / moderator_id use ON DELETE CASCADE like every other
--     user-owned row (docs/08 §38 hard-delete is the only path that fires
--     it); resolved_by uses SET NULL so resolving staff leaving the platform
--     does not erase the report itself.
--
--   * target_id is deliberately NOT a foreign key: the target is polymorphic
--     across six tables (docs/11 §38). Existence and visibility are verified
--     by the owning domain's service at report time, and re-verified at
--     moderation time - the same live re-check discipline the notification
--     worker uses.

-- +migrate Up

-- ---------------------------------------------------------------------------
-- reports - docs/08 §24.1
-- ---------------------------------------------------------------------------
CREATE TABLE reports (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    reporter_id UUID         NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    target_type VARCHAR(32)  NOT NULL,
    target_id   UUID         NOT NULL,

    reason      VARCHAR(32)  NOT NULL,
    description TEXT,

    status      VARCHAR(16)  NOT NULL DEFAULT 'pending',

    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    resolved_by UUID         REFERENCES users (id) ON DELETE SET NULL,

    CONSTRAINT reports_status_check
        CHECK (status IN ('pending', 'reviewing', 'resolved', 'rejected')),
    CONSTRAINT reports_reason_not_blank CHECK (btrim(reason) <> ''),
    CONSTRAINT reports_target_type_not_blank CHECK (btrim(target_type) <> '')
);

-- The documented queue index (docs/08 §34: reports(status)).
CREATE INDEX reports_status_idx ON reports (status);

-- "My reports" (docs/09 §28 GET /me/reports) walks a reporter's history.
CREATE INDEX reports_reporter_idx ON reports (reporter_id);

-- One OPEN report per reporter per target - see the header note.
CREATE UNIQUE INDEX reports_open_dedup_idx
    ON reports (reporter_id, target_type, target_id)
    WHERE status IN ('pending', 'reviewing');

-- ---------------------------------------------------------------------------
-- moderation_actions - docs/08 §24.2
-- ---------------------------------------------------------------------------
CREATE TABLE moderation_actions (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    moderator_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    target_type  VARCHAR(32) NOT NULL,
    target_id    UUID        NOT NULL,

    action       VARCHAR(32) NOT NULL,
    reason       TEXT,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT moderation_actions_action_not_blank CHECK (btrim(action) <> ''),
    CONSTRAINT moderation_actions_target_type_not_blank
        CHECK (btrim(target_type) <> '')
);

-- "What has been done to this object" - the report-detail history panel and
-- the moderator's target lookup (docs/11 §39 audit examples).
CREATE INDEX moderation_actions_target_idx
    ON moderation_actions (target_type, target_id, created_at DESC);

-- The global audit listing, newest first.
CREATE INDEX moderation_actions_created_idx
    ON moderation_actions (created_at DESC);

-- +migrate Down

DROP TABLE IF EXISTS moderation_actions;
DROP TABLE IF EXISTS reports;
