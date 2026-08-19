-- Achievements: the award, the tally, and the switch that turns the whole
-- thing off (docs/PROFILE-AND-ACHIEVEMENTS.md Part 3).
--
-- Three tables because they have three different lifetimes. The AWARD is
-- durable and permanent - it is a fact about someone's practice, and a badge
-- that could be un-awarded is a badge that can shame a writer. The TALLY is
-- working state the service rewrites constantly and may reset without loss.
-- The SWITCH is a preference, and preferences here follow the ai_prefs
-- precedent so that adding a toggle is never a migration.
--
-- What is deliberately absent: points, a total score, and anything a
-- leaderboard could be built from. Comparison turns writing into hunting, so
-- there is no column to sort people by (Part 3 "System rules").

-- +migrate Up

-- The award. One row per unlocked achievement, written by the achievement
-- service itself and never by the notification pipeline - notifications
-- explicitly drop what they cannot deliver, so an award that depended on one
-- would be lost exactly when it mattered.
--
-- `key` is a catalogue key, not a foreign key: the catalogue lives in Go
-- (internal/achievements) because each entry carries a threshold, a cooldown
-- and Thai copy that belong with the code that enforces them. A key retired
-- from the catalogue leaves its rows standing rather than deleting somebody's
-- history.
CREATE TABLE achievements (
    user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key            TEXT        NOT NULL,
    unlocked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    seen_at        TIMESTAMPTZ NULL,
    showcase_order SMALLINT    NULL,
    PRIMARY KEY (user_id, key)
);

COMMENT ON COLUMN achievements.key IS
    'Catalogue key (internal/achievements). Not a foreign key: the catalogue carries thresholds, cooldowns and Thai copy, which belong beside the code that enforces them.';

COMMENT ON COLUMN achievements.unlocked_at IS
    'When the threshold was met. The award is permanent - nothing in the product revokes one, because a badge that can be taken back can be used to shame a writer.';

COMMENT ON COLUMN achievements.seen_at IS
    'When the owner was actually shown the unlock. NULL means the dismissible strip still owes them the news; it is why an unlock never has to interrupt writing to be delivered (Part 3 "Never interrupt writing").';

COMMENT ON COLUMN achievements.showcase_order IS
    'Position on the public profile, or NULL for "not showcased". The profile shows 3-5 chosen by the owner, not everything - so this is the owner''s editorial choice, not a ranking.';

-- Showcase lookups are per person and tiny; the partial index keeps the
-- profile read from touching the rows the owner did not choose.
CREATE INDEX achievements_showcase
    ON achievements (user_id, showcase_order)
    WHERE showcase_order IS NOT NULL;

-- The tally. Progress towards an award, plus the anti-farming state that keeps
-- it honest (Part 3 "Anti-farming").
CREATE TABLE achievement_progress (
    user_id  UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key      TEXT        NOT NULL,
    count    INTEGER     NOT NULL DEFAULT 0,
    last_at  TIMESTAMPTZ NULL,
    meta     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (user_id, key)
);

COMMENT ON COLUMN achievement_progress.count IS
    'Signals accepted so far. Never a score and never displayed as one: no achievement rewards posting volume, because that would invite the platform to fill with rubbish.';

COMMENT ON COLUMN achievement_progress.last_at IS
    'When a signal was last ACCEPTED. The per-key cooldown is measured from here, so ten create-delete cycles in a minute count once.';

COMMENT ON COLUMN achievement_progress.meta IS
    'Per-key working state. Reader-driven achievements keep {"actors": [user_id, ...]} here so they count DISTINCT accounts older than 7 days rather than repeat visits from one person - or from an account made to applaud its owner.';

-- The switch. Some writers find this sort of thing cheapens the work, and that
-- view is respected in full: off means no counting, no strip, no profile
-- section. Existing rows are kept, never deleted - turning it back on resumes
-- exactly where the writer left off, and nothing about them is destroyed for
-- having changed their mind.
--
-- JSONB for the ai_prefs reason: the next toggle should not be a migration.
CREATE TABLE achievement_prefs (
    user_id    UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    prefs      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON COLUMN achievement_prefs.prefs IS
    'Opaque-but-validated switches, currently {"enabled": bool}. Absent means the default (on). The ai_prefs precedent: adding a toggle is a code change, not a migration.';

-- +migrate Down

DROP TABLE achievement_prefs;
DROP TABLE achievement_progress;
DROP TABLE achievements;
