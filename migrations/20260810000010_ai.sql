-- Phase 10 - AI / Thai NLP
-- (docs/08 - Database Design.md §25–§26 and §44 Phase 10; docs/12 whole;
-- docs/09 §24; docs/11 §50–§54).
--
-- Two tables, exactly the Phase 10 scope of docs/08 §44 - no speculative AI
-- tables. AI assistance is OPTIONAL and must never modify a manuscript
-- silently (docs/12 §15, §43): these tables record REQUESTS and SUGGESTIONS,
-- and a suggestion is a proposal the writer accepts or rejects. Accepting one
-- never writes here - the writer applies it through the normal, revisioned
-- chapter-edit path.
--
-- Design notes:
--
--   * feature / provider stay VARCHAR WITHOUT a CHECK, for the same reason
--     notifications.type and media.media_type do: the vocabulary is
--     service-level and open (docs/12 §5 lists many features across rollout
--     phases), so adding one must not need a migration. The service is the
--     single authority on which features may actually be requested today.
--
--   * status DOES carry a CHECK: unlike feature, the lifecycle is a FIXED,
--     closed set (docs/12 §28). A row outside it is a bug, not a new feature,
--     so the database refuses it.
--
--   * error_code is a short CLASSIFICATION only (e.g. provider_timeout,
--     invalid_output) - never provider detail, a prompt, or manuscript content
--     (docs/12 §36 "log ... error_type", docs/11 §67). It reconciles §25.1
--     (which lists no error column) with §28's conceptual `error` field and the
--     phase's "failure information" requirement.
--
--   * started_at supports latency metrics (docs/12 §36–§37) and marks the
--     worker's claim instant; it reconciles §25.1 with §28's `started_at`.
--
--   * NO prompt/response/manuscript content is stored on ai_requests
--     (docs/12 §25.1 "AI Privacy Rule", docs/11 §54). The only user text that
--     persists is the minimal SUGGESTION SPAN on ai_suggestions - the writer's
--     own words, needed for the accept/reject workflow (docs/08 §26.1), under
--     their control and cascade-deleted with them.
--
--   * user_id / chapter_id CASCADE like every user-owned row (docs/08 §38):
--     deleting a user or hard-deleting a chapter takes its AI history with it.

-- +migrate Up

CREATE TABLE ai_requests (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users (id)    ON DELETE CASCADE,
    chapter_id   UUID                 REFERENCES chapters (id) ON DELETE CASCADE,

    feature      VARCHAR(32) NOT NULL,
    provider     VARCHAR(32) NOT NULL,
    model        VARCHAR(64),
    status       VARCHAR(16) NOT NULL,
    error_code   VARCHAR(48),

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,

    CONSTRAINT ai_requests_status_valid CHECK (
        status IN ('queued', 'processing', 'completed', 'failed', 'cancelled')
    ),
    CONSTRAINT ai_requests_feature_not_blank  CHECK (btrim(feature)  <> ''),
    CONSTRAINT ai_requests_provider_not_blank CHECK (btrim(provider) <> '')
);

-- The caller's own history, newest first (docs/11 §1461 "AI processing
-- history"), and the range the per-user DAILY quota counts over. One index
-- serves both.
CREATE INDEX ai_requests_user_idx ON ai_requests (user_id, created_at DESC);

-- The worker's hot set: only rows still in flight. A partial index keeps it
-- tiny - completed history never enters it (docs/12 §27 async worker).
CREATE INDEX ai_requests_pending_idx ON ai_requests (created_at)
    WHERE status IN ('queued', 'processing');

CREATE TABLE ai_suggestions (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id     UUID        NOT NULL REFERENCES ai_requests (id) ON DELETE CASCADE,
    chapter_id     UUID        NOT NULL REFERENCES chapters (id)    ON DELETE CASCADE,

    type           VARCHAR(32) NOT NULL,
    original_text  TEXT        NOT NULL,
    suggested_text TEXT,
    explanation    TEXT,
    status         VARCHAR(16) NOT NULL DEFAULT 'pending',

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ai_suggestions_status_valid CHECK (
        status IN ('pending', 'accepted', 'rejected', 'dismissed')
    ),
    CONSTRAINT ai_suggestions_type_not_blank CHECK (btrim(type) <> '')
);

-- Suggestions are always loaded by their parent request.
CREATE INDEX ai_suggestions_request_idx ON ai_suggestions (request_id);

-- +migrate Down

DROP TABLE IF EXISTS ai_suggestions;
DROP TABLE IF EXISTS ai_requests;
