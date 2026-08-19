-- +migrate Up

-- 13Y - the writing tools round: the word bank, taught silences, character
-- evolution markers, the fact book, and per-writer assistant preferences.

-- The fiction's own vocabulary (13Y §2). Custom terms the writer taught the
-- spellchecker; the AUTO part of the bank (cast names, reader-variable tokens,
-- fandom, tags) is derived live from its owning tables and never duplicated
-- here. Terms are per fiction and shared across a series at read time.
CREATE TABLE novel_lexicon (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    novel_id   UUID         NOT NULL REFERENCES novels(id) ON DELETE CASCADE,
    term       VARCHAR(120) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX novel_lexicon_unique_term
    ON novel_lexicon (novel_id, lower(term));

-- "ไม่เตือนแบบนี้อีก" (13Y §4). A mute silences one rule family for one term.
-- novel_id NULL = the writer muted it everywhere (a general rule); set = only
-- in that fiction. The mute belongs to the WRITER - it teaches their
-- assistant, never anyone else's.
CREATE TABLE ai_mutes (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    novel_id   UUID         REFERENCES novels(id) ON DELETE CASCADE,
    kind       VARCHAR(40)  NOT NULL,
    term       VARCHAR(200) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ai_mutes_unique
    ON ai_mutes (user_id, COALESCE(novel_id, '00000000-0000-0000-0000-000000000000'::uuid), kind, lower(term));
CREATE INDEX ai_mutes_by_user ON ai_mutes (user_id);

-- "ตัวละครเปลี่ยนไปตั้งแต่ตอนนี้" (13Y §5): deliberate character development.
-- From this chapter number on, the consistency check stops comparing the
-- character against their sheet - the sheet describes who they WERE.
CREATE TABLE character_evolution (
    character_id        UUID        PRIMARY KEY REFERENCES characters(id) ON DELETE CASCADE,
    novel_id            UUID        NOT NULL REFERENCES novels(id) ON DELETE CASCADE,
    from_chapter_number INTEGER     NOT NULL CHECK (from_chapter_number > 0),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The fact book (13Y §6): writer-owned facts per chapter, the substrate the
-- continuity check compares against - never raw manuscript re-reads. JSONB of
-- [{"label": "...", "value": "..."}]; the writer sees and edits every row.
CREATE TABLE chapter_facts (
    chapter_id UUID        PRIMARY KEY REFERENCES chapters(id) ON DELETE CASCADE,
    facts      JSONB       NOT NULL DEFAULT '[]'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Assistant preferences (13Y §10), two of the three tiers: the account-wide
-- defaults and the per-fiction override. (The third tier - the quick switches
-- in the editor - is session UI, not storage.) Opaque-but-validated JSON, so
-- adding a toggle is not a migration.
CREATE TABLE ai_prefs (
    user_id    UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    prefs      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE ai_novel_prefs (
    novel_id   UUID        PRIMARY KEY REFERENCES novels(id) ON DELETE CASCADE,
    prefs      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +migrate Down
DROP TABLE ai_novel_prefs;
DROP TABLE ai_prefs;
DROP TABLE chapter_facts;
DROP TABLE character_evolution;
DROP TABLE ai_mutes;
DROP TABLE novel_lexicon;
