-- Phase 12E - Public user profile (docs/PHASE-12-STORY-DEPTH.md §12E).
--
-- The profile shows "ผู้ติดตาม N". Nothing in the API reported that number:
-- user_follows was only ever read in one direction (`/me/following`), so a
-- profile could say who someone follows but never how many follow them.
--
-- Design, and why this is a column rather than a COUNT(*):
--
--   * user_follows_following_idx makes a single count cheap, but the number is
--     not needed one profile at a time. It belongs on every author card - the
--     following list, an author block on a fiction page, a future author
--     directory - where a per-row count becomes a scan per card. This follows
--     12C exactly: denormalise the number the cards read (docs/07 §67).
--
--   * It is maintained in the SAME statement as the row it counts, using the
--     same CTE shape bookmark_count and like_count use, so it cannot drift
--     from user_follows and a repeat follow cannot inflate it.
--
--   * It lives on `users`, not on `user_profiles`: a user_profiles row is
--     created on demand, so a counter there would be missing for exactly the
--     accounts that have not filled in a profile yet.
--
-- The doc placed this column in 12C's migration. That migration is already
-- applied, and an applied migration is never edited - hence its own file.

-- +migrate Up

ALTER TABLE users
    ADD COLUMN follower_count INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN users.follower_count IS
    'Display counter for user_follows, maintained in the same statement as the follow row.';

-- Correct from the moment the column appears, rather than after the first
-- follow or unfollow touches each account.
UPDATE users
SET follower_count = COALESCE(counts.total, 0)
FROM (
    SELECT following_id, COUNT(*) AS total FROM user_follows GROUP BY following_id
) AS counts
WHERE users.id = counts.following_id;

-- +migrate Down

ALTER TABLE users DROP COLUMN IF EXISTS follower_count;
