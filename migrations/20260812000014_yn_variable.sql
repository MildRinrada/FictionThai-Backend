-- Phase 12B - y/n reader variable (docs/PHASE-12-STORY-DEPTH.md §12B).
--
-- Thai reader-insert fiction substitutes the reader's own name for a placeholder
-- the author types into the text. This migration is the whole of the schema
-- change: a per-fiction switch and the token to look for.
--
-- What is deliberately NOT here:
--
--   * The reader's chosen name. It is stored in the BROWSER, never on the server.
--     Recording what name a reader inserts into a romance would create a per-user
--     record of a sensitive preference for no product benefit, which docs/11's
--     data-minimisation position rules out. Keeping it client-side also means a
--     guest gets the feature, which docs/10 §2.1 requires.
--
--   * Any rewriting of chapter content. Substitution happens at RENDER time in
--     the reader. The stored text always contains the token, so turning y/n off
--     restores the author's text exactly, and two readers of the same cached
--     chapter receive identical bytes. Nothing here can modify writer content
--     (CLAUDE.md: never silently modify writer content).
--
-- yn_token is matched LITERALLY by the reader, never compiled as a regular
-- expression, so an author cannot write a token that turns into a catastrophic
-- pattern. The service defaults it to '{y/n}' when the switch is turned on.

-- +migrate Up

ALTER TABLE novels
    ADD COLUMN yn_enabled BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN yn_token   VARCHAR(32) NULL;

-- A token is meaningless when the feature is off, and required when it is on.
ALTER TABLE novels
    ADD CONSTRAINT novels_yn_token_present
    CHECK (NOT yn_enabled OR yn_token IS NOT NULL);

COMMENT ON COLUMN novels.yn_token IS
    'Literal placeholder the reader replaces with their own name, client-side only. Never used as a regular expression, and never substituted into stored content.';

-- +migrate Down

ALTER TABLE novels DROP CONSTRAINT IF EXISTS novels_yn_token_present;
ALTER TABLE novels DROP COLUMN IF EXISTS yn_token;
ALTER TABLE novels DROP COLUMN IF EXISTS yn_enabled;
