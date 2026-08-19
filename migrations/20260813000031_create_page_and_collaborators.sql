-- +migrate Up

-- Phase 13U - the create page's missing halves.
--
-- content_warning_spoiler: the author's choice to fold the warning behind a
-- reader-operated button, because a warning can itself spoil.
--
-- hide_counts: the author's choice to keep hearts/views off their fiction's
-- public face. The numbers keep counting - the writer still sees them in the
-- studio - readers simply are not shown a scoreboard.
--
-- show_donate: whether the author's support link (profile-level) is offered on
-- THIS fiction. Default true: the link exists because the author set one.
--
-- theme_color: an accent for the fiction's own page. Stored lowercase #rrggbb,
-- enforced by CHECK so a stored value is always paintable.
--
-- publish_at: a scheduled first publish. Read-time semantics, like chapter
-- schedules: the row may say public, but nothing serves it before its time.
ALTER TABLE novels
    ADD COLUMN content_warning_spoiler BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN hide_counts BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN show_donate BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN theme_color VARCHAR(7),
    ADD COLUMN publish_at TIMESTAMPTZ,
    ADD CONSTRAINT novels_theme_color_format
        CHECK (theme_color IS NULL OR theme_color ~ '^#[0-9a-f]{6}$');

-- ผู้เขียนร่วม (13U). A collaborator may write and edit the fiction's CONTENT
-- - chapters, characters, variables - and sees the studio. They are not the
-- owner: settings, visibility, publishing, deletion, and this very list stay
-- with the author. `credit` is the public wording the author chooses; empty
-- means the plain display name.
CREATE TABLE novel_collaborators (
    novel_id   UUID NOT NULL REFERENCES novels (id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credit     VARCHAR(120) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (novel_id, user_id)
);

-- "which fictions do I co-write" is the collaborator's own studio list.
CREATE INDEX idx_novel_collaborators_user ON novel_collaborators (user_id);

-- +migrate Down
DROP TABLE novel_collaborators;
ALTER TABLE novels
    DROP CONSTRAINT novels_theme_color_format,
    DROP COLUMN publish_at,
    DROP COLUMN theme_color,
    DROP COLUMN show_donate,
    DROP COLUMN hide_counts,
    DROP COLUMN content_warning_spoiler;
