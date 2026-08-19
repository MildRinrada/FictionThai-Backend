-- Profile identity: the banner, the writer-controlled contact fields, and the
-- one thing a public profile page needs that the schema could not hold
-- (docs/PROFILE-AND-ACHIEVEMENTS.md Part 1).
--
-- Background: user_profiles has been readable everywhere and writable nowhere
-- since Phase 1 - the only mutation in the whole backend was SetAvatarURL. The
-- profile page therefore showed a typographic placeholder where the design
-- (design handoff §12) asks for a 180px cover with an owner-only
-- "เปลี่ยนภาพปก" control, and had no way to say who the person is beyond a
-- display name.
--
-- Nothing here collects anything new about a person. banner_url and links are
-- things the account CHOOSES to publish; there is still no real name, no
-- birthdate, no location - the do-not-collect list is unchanged.

-- +migrate Up

ALTER TABLE user_profiles
    ADD COLUMN banner_url TEXT NULL,
    ADD COLUMN links      JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN user_profiles.banner_url IS
    'Object-storage URL of the profile cover image, chosen by the account owner. NULL renders the typographic placeholder.';

COMMENT ON COLUMN user_profiles.links IS
    'Writer-published contact links: [{"label":"X","url":"https://…"}]. Opaque-but-validated JSON so adding a service is not a migration (the ai_prefs precedent). Never an email address.';

-- open_for is the writer''s own answer to "are you taking work right now" -
-- commissions, fic requests, beta reading, or nothing. A status, not a
-- marketplace: the platform never brokers, prices, or processes any of it,
-- exactly as it never processes donations (docs/MONETIZATION.md §6).
ALTER TABLE author_profiles
    ADD COLUMN open_for JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN author_profiles.open_for IS
    'What the writer is currently open to: subset of ["commission","request","beta"]. Writer-controlled, cleared by the writer alone.';

-- +migrate Down

ALTER TABLE author_profiles DROP COLUMN IF EXISTS open_for;
ALTER TABLE user_profiles   DROP COLUMN IF EXISTS links;
ALTER TABLE user_profiles   DROP COLUMN IF EXISTS banner_url;
