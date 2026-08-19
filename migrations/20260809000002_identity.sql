-- Phase 1 - Identity (docs/08 - Database Design.md §44).
--
-- Creates the identity core: users, their three satellite records, revocable
-- sessions, and the two short-lived token tables.
--
-- Design notes:
--
--   * Authentication data is separated from profile data (docs/08 §6.2, §29).
--     `users` holds credentials; everything public lives in `user_profiles`.
--
--   * `username` and `email` are CITEXT. docs/10 §7 requires that a username
--     cannot be used to impersonate another account, and two accounts differing
--     only by letter case would do exactly that. CITEXT makes the UNIQUE
--     constraint itself case-insensitive, so the database enforces it rather
--     than relying on every call site remembering to lower-case first.
--
--   * CHECK constraints mirror the documented enumerations. docs/08 §43 Rule 1:
--     database constraints protect integrity, application logic protects
--     ownership - use both.

-- +migrate Up

-- ---------------------------------------------------------------------------
-- users - docs/08 §6.1
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id                          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    username                    CITEXT      NOT NULL UNIQUE,
    email                       CITEXT      NOT NULL UNIQUE,
    password_hash               TEXT        NOT NULL,
    role                        VARCHAR(32) NOT NULL DEFAULT 'user',
    status                      VARCHAR(32) NOT NULL DEFAULT 'active',
    email_verified_at           TIMESTAMPTZ NULL,

    -- Cutoff for bulk session invalidation. Any session created at or before
    -- this instant is rejected at validation time, which makes "log out of all
    -- devices", a password change, and an admin suspension a single UPDATE
    -- rather than a scan over every session row (docs/10 §37).
    sessions_invalidated_before TIMESTAMPTZ NULL,

    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at                  TIMESTAMPTZ NULL,

    -- docs/08 §6.1: initial roles. Writer is a CAPABILITY, not a role
    -- (docs/10 §52) - a normal user becomes a writer by creating a fiction, so
    -- there is deliberately no 'writer' value here.
    CONSTRAINT users_role_valid CHECK (role IN ('user', 'moderator', 'admin')),

    -- docs/10 §18 account states.
    CONSTRAINT users_status_valid CHECK (
        status IN ('active', 'pending_verification', 'suspended', 'banned', 'deleted')
    ),

    -- docs/10 §7: a bounded length and a URL-safe character set. Thai display
    -- names live in user_profiles.display_name; the username is the handle that
    -- appears in /author/{username}, so it stays ASCII to avoid homograph
    -- impersonation.
    CONSTRAINT users_username_format CHECK (username ~ '^[A-Za-z0-9_-]{3,32}$'),

    CONSTRAINT users_email_shape CHECK (position('@' IN email) > 1)
);

-- docs/08 §34. The UNIQUE constraints above already create indexes for
-- username and email lookups, so no duplicates are declared here.
CREATE INDEX users_status_idx ON users (status) WHERE deleted_at IS NULL;

COMMENT ON COLUMN users.sessions_invalidated_before IS
    'Sessions created at or before this instant are invalid. Used for logout-all, password change, and suspension.';

-- ---------------------------------------------------------------------------
-- user_profiles - docs/08 §6.2 (public profile information)
-- ---------------------------------------------------------------------------
CREATE TABLE user_profiles (
    user_id      UUID         PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    display_name VARCHAR(64)  NULL,
    bio          TEXT         NULL,
    avatar_url   TEXT         NULL,
    website_url  TEXT         NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

COMMENT ON COLUMN user_profiles.display_name IS
    'Free-form and may contain Thai. Unlike username it is not an identifier and is never used in a URL.';

-- ---------------------------------------------------------------------------
-- author_profiles - docs/08 §6.3
--
-- A user does not need to become a writer immediately (docs/08 §6.3), so this
-- row is created on demand, not at registration.
-- ---------------------------------------------------------------------------
CREATE TABLE author_profiles (
    user_id     UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    pen_name    VARCHAR(64) NULL,
    author_bio  TEXT        NULL,
    is_featured BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- user_preferences - docs/08 §6.4
-- ---------------------------------------------------------------------------
CREATE TABLE user_preferences (
    user_id       UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    theme         VARCHAR(16) NOT NULL DEFAULT 'system',
    font_size     VARCHAR(16) NOT NULL DEFAULT 'medium',
    reading_width VARCHAR(16) NOT NULL DEFAULT 'comfortable',
    line_spacing  VARCHAR(16) NOT NULL DEFAULT 'normal',
    language      VARCHAR(8)  NOT NULL DEFAULT 'th',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT user_preferences_theme_valid CHECK (theme IN ('system', 'light', 'dark')),
    CONSTRAINT user_preferences_font_size_valid CHECK (font_size IN ('small', 'medium', 'large')),
    CONSTRAINT user_preferences_reading_width_valid CHECK (reading_width IN ('narrow', 'comfortable', 'wide')),
    CONSTRAINT user_preferences_line_spacing_valid CHECK (line_spacing IN ('tight', 'normal', 'relaxed'))
);

-- ---------------------------------------------------------------------------
-- user_sessions - docs/08 §29
--
-- One row per active login. Opaque, revocable, server-side; the same row backs
-- both the web cookie and the mobile Bearer token, so revoking it logs that
-- device out regardless of transport.
-- ---------------------------------------------------------------------------
CREATE TABLE user_sessions (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- SHA-256 of the raw token, hex-encoded. docs/08 §29: never store raw
    -- session tokens. A fast hash is correct here - the token is 256 bits of
    -- CSPRNG output, so it is not guessable, and a slow KDF would add latency
    -- to every authenticated request.
    token_hash   TEXT        NOT NULL UNIQUE,

    -- 'web' cookies and 'mobile' Bearer tokens have different lifetimes and
    -- different CSRF requirements, so the transport is recorded.
    client_kind  VARCHAR(16) NOT NULL,

    expires_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    user_agent   TEXT        NULL,

    -- TRUNCATED network prefix, never a full address: docs/11 §34 requires data
    -- minimisation, and a coarse prefix is enough to spot session abuse.
    ip_prefix    INET        NULL,

    CONSTRAINT user_sessions_client_kind_valid CHECK (client_kind IN ('web', 'mobile'))
);

-- Listing and revoking a user's live sessions (docs/10 §37, §56).
CREATE INDEX user_sessions_user_active_idx ON user_sessions (user_id, revoked_at);
-- Periodic cleanup of expired rows; docs/08 §37 hard-deletes expired sessions.
CREATE INDEX user_sessions_expires_at_idx ON user_sessions (expires_at);

COMMENT ON COLUMN user_sessions.ip_prefix IS
    'Truncated network prefix (IPv4 /24, IPv6 /48), never a full address - docs/11 §34 data minimisation.';

-- ---------------------------------------------------------------------------
-- password_reset_tokens - docs/08 §29, docs/10 §16, docs/11 §26
--
-- Single-use, expiring, hashed at rest.
-- ---------------------------------------------------------------------------
CREATE TABLE password_reset_tokens (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash TEXT        NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX password_reset_tokens_user_idx ON password_reset_tokens (user_id);
CREATE INDEX password_reset_tokens_expires_at_idx ON password_reset_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- email_verification_tokens - docs/08 §29, docs/10 §17
-- ---------------------------------------------------------------------------
CREATE TABLE email_verification_tokens (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash TEXT        NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX email_verification_tokens_user_idx ON email_verification_tokens (user_id);
CREATE INDEX email_verification_tokens_expires_at_idx ON email_verification_tokens (expires_at);

-- +migrate Down

DROP TABLE IF EXISTS email_verification_tokens;
DROP TABLE IF EXISTS password_reset_tokens;
DROP TABLE IF EXISTS user_sessions;
DROP TABLE IF EXISTS user_preferences;
DROP TABLE IF EXISTS author_profiles;
DROP TABLE IF EXISTS user_profiles;
DROP TABLE IF EXISTS users;
