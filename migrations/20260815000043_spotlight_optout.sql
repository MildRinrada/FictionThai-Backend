-- +migrate Up

-- อันดับนักเขียน opt-out (docs/WRITER-SPOTLIGHT.md).
--
-- A PERSON-level switch, beside wall_enabled: a ranking of people creates
-- pressure on the person, not on any one work, so the choice to stay out of
-- it belongs to user_profiles - the same continuation of writer control that
-- novels.hide_counts gives per fiction.
ALTER TABLE user_profiles
    ADD COLUMN hide_from_rankings BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN user_profiles.hide_from_rankings IS
    'Writer opted out of the home-page writer rankings (docs/WRITER-SPOTLIGHT.md).';

-- +migrate Down

ALTER TABLE user_profiles DROP COLUMN IF EXISTS hide_from_rankings;
