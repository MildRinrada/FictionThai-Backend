-- Phase 13 - แยก 18+ ทั่วไป ออกจาก 18+ เนื้อหาทางเพศชัดเจน
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13B, revised).
--
-- 13B shipped one 18+ value, and one value has to serve two very different
-- works: a fiction that is violent, bleak, or about addiction, and a fiction
-- that is explicitly sexual. Treating them identically means either the first is
-- gated far harder than it needs to be, or the second is gated far softer than
-- the platform can defend. Splitting them is the only way both answers can be
-- right.
--
--   general   no gate
--   teen      15+  a dismissible warning
--   mature    18+  the author picks the gate
--   explicit  18+ เนื้อหาทางเพศชัดเจน  a signed-in reader ALWAYS
--
-- The last one is a PLATFORM rule, not an author setting, which is why it lives
-- in the rating rather than in age_gate: an author choosing "warning" on
-- explicit work would be choosing something the platform will not honour, and
-- offering a control that is silently overridden is the dishonesty §13E rules
-- out. The API therefore refuses the combination outright.
--
-- age_gate gains 'login' between 'warning' and 'verified', because the ladder
-- had a missing rung: the gap between "click to continue" and "send us your ID"
-- is enormous, and "you must be signed in" is where most 18+ work actually
-- belongs. It is also the rung the explicit rating forces.
--
-- users.adult_attested_at is the author's own statement, made ONCE, that they
-- are an adult. It is a timestamp rather than a boolean so the record says when,
-- which is the only part of it worth keeping. No date of birth, no document, no
-- third party: docs/11 §34 does not permit collecting more than the question
-- needs, and the question is yes or no.

-- +migrate Up

ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_age_rating_valid,
    DROP CONSTRAINT IF EXISTS novels_age_gate_valid;

ALTER TABLE novels
    ADD CONSTRAINT novels_age_rating_valid
        CHECK (age_rating IN ('general', 'teen', 'mature', 'explicit')),
    ADD CONSTRAINT novels_age_gate_valid
        CHECK (age_gate IN ('warning', 'login', 'verified')),
    -- Explicit work is never reachable behind a dismissible warning. The
    -- service refuses the pair with a field error; this is the backstop that
    -- makes the rule true of the data rather than only of the code path.
    ADD CONSTRAINT novels_explicit_needs_account
        CHECK (age_rating <> 'explicit' OR age_gate <> 'warning');

COMMENT ON COLUMN novels.age_rating IS
    'Author''s rating: general / teen / mature / explicit (§13B). Decides discoverability.';
COMMENT ON COLUMN novels.age_gate IS
    'How adult work is gated: warning / login / verified. Consulted at mature and explicit.';

ALTER TABLE users
    ADD COLUMN adult_attested_at TIMESTAMPTZ;

COMMENT ON COLUMN users.adult_attested_at IS
    'When the account stated it belongs to an adult. Required once before publishing 18+ work (§13B).';

-- +migrate Down

ALTER TABLE users DROP COLUMN IF EXISTS adult_attested_at;

ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_explicit_needs_account,
    DROP CONSTRAINT IF EXISTS novels_age_rating_valid,
    DROP CONSTRAINT IF EXISTS novels_age_gate_valid;

-- The narrower schema has one 18+ value, so explicit work folds back into it.
-- The stricter gate is kept: coming back down must never widen who can reach a
-- work that was published as explicitly sexual.
UPDATE novels SET age_rating = 'mature' WHERE age_rating = 'explicit';
UPDATE novels SET age_gate   = 'verified' WHERE age_gate = 'login';

ALTER TABLE novels
    ADD CONSTRAINT novels_age_rating_valid
        CHECK (age_rating IN ('general', 'teen', 'mature')),
    ADD CONSTRAINT novels_age_gate_valid
        CHECK (age_gate IN ('warning', 'verified'));
