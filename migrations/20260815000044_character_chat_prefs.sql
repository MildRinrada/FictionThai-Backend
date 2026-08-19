-- +migrate Up

-- Chat presentation preferences (chat-editor review 2026-08, items 1-2).
--
-- The colour that identifies a character, the side their bubbles sit on, and
-- the short name the speaker strip displays. They belong to the CHARACTER -
-- not to a chapter, not to a composer session - so the strip a writer arranges
-- in one chapter is the strip every chapter shows, and the character page
-- remains the single place the cast is defined.
ALTER TABLE characters
    ADD COLUMN chat_color        text,
    ADD COLUMN chat_side         text,
    ADD COLUMN chat_display_name text;

ALTER TABLE characters
    ADD CONSTRAINT characters_chat_side_check
    CHECK (chat_side IS NULL OR chat_side IN ('left', 'right'));

-- +migrate Down

ALTER TABLE characters
    DROP CONSTRAINT characters_chat_side_check;

ALTER TABLE characters
    DROP COLUMN chat_color,
    DROP COLUMN chat_side,
    DROP COLUMN chat_display_name;
