-- +migrate Up

-- The library redesign (library review 2026-08): the reader-side state the
-- new ชั้นหนังสือของฉัน needs and the schema never had.

-- Per-follow notification choice (README: following ≠ wanting every alert).
-- Default TRUE keeps every existing follow behaving exactly as before.
ALTER TABLE user_follows
    ADD COLUMN notify_new_chapters BOOLEAN NOT NULL DEFAULT TRUE;

-- The reader's own "finished" mark, with an optional private star and note.
-- PRIVATE state about someone else's content - never exposed publicly, never
-- aggregated into the fiction's public numbers (it is not a rating system).
CREATE TABLE novel_marks (
    user_id     UUID         NOT NULL REFERENCES users (id)  ON DELETE CASCADE,
    novel_id    UUID         NOT NULL REFERENCES novels (id) ON DELETE CASCADE,
    finished_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    stars       SMALLINT     NULL CHECK (stars BETWEEN 1 AND 5),
    note        VARCHAR(500) NULL,
    PRIMARY KEY (user_id, novel_id)
);
CREATE INDEX novel_marks_user_recency_idx ON novel_marks (user_id, finished_at DESC);

-- Reading history (README §"Reading history is private by default"): one row
-- per (user, chapter); a revisit bumps read_at rather than growing a log.
-- Never exposed through any public API.
CREATE TABLE reading_events (
    user_id    UUID        NOT NULL REFERENCES users (id)    ON DELETE CASCADE,
    novel_id   UUID        NOT NULL REFERENCES novels (id)   ON DELETE CASCADE,
    chapter_id UUID        NOT NULL REFERENCES chapters (id) ON DELETE CASCADE,
    read_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, chapter_id)
);
CREATE INDEX reading_events_user_recency_idx ON reading_events (user_id, read_at DESC);

-- "Control whether history is retained" (README): presence = recording OFF.
-- A separate table rather than a preferences column so the library domain
-- owns its own privacy switch and one INSERT..WHERE NOT EXISTS can gate the
-- hot-path write without a second query.
CREATE TABLE reading_history_optout (
    user_id    UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +migrate Down

DROP TABLE reading_history_optout;
DROP TABLE reading_events;
DROP TABLE novel_marks;

ALTER TABLE user_follows
    DROP COLUMN notify_new_chapters;
