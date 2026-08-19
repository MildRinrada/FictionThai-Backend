-- Baseline migration.
--
-- This establishes the PostgreSQL capabilities the documented schema depends on
-- and nothing else. Application tables are NOT created here: docs/08 - Database
-- Design.md leaves the chapter revision model (§12) and the coexistence of the
-- standard and chat content representations (§40) explicitly open, and both
-- decisions must be settled before production migrations exist.
--
-- The implementation order for the real schema is docs/08 §44:
--   Phase 1  identity        users, user_profiles, author_profiles, user_preferences
--   Phase 2  publishing      novels, chapters, chapter_revisions
--   Phase 3  fiction format  novels.story_structure / presentation_format /
--                            content_mode, chapter_messages
--   Phase 4+ discovery, reader, interaction, community, moderation, media, AI

-- +migrate Up

-- gen_random_uuid() for the UUID primary keys required by docs/08 §36.
-- Built in from PostgreSQL 13, but declaring it keeps the migration honest
-- about what the schema relies on.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Case-insensitive text for users.username and users.email. docs/08 §34 indexes
-- both for lookup and §7 makes username the public identity, so two accounts
-- differing only by case would be an impersonation vector (docs/10 §7).
CREATE EXTENSION IF NOT EXISTS "citext";

-- Trigram indexes for the PostgreSQL-based search that docs/07 §25 specifies as
-- the starting point, before any dedicated search engine is introduced.
CREATE EXTENSION IF NOT EXISTS "pg_trgm";

-- +migrate Down

-- Extensions are intentionally not dropped. DROP EXTENSION cascades to every
-- column and index that depends on it, which would destroy data rather than
-- reverse a schema change (docs/14 §48: prefer backward-compatible migrations).
-- This statement makes the rollback explicit and successful without being
-- destructive.
SELECT 1;
