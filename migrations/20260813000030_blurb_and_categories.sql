-- Phase 13S - คำโปรย, บทนำ, and a taxonomy that asks three questions
--
-- Three changes, all additive, none of which touches a word anybody wrote.
--
-- 1. novels.tagline - คำโปรย.
--
--    A synopsis and a tagline are not the same thing and never were. A synopsis
--    is a paragraph a reader opens the fiction page to read; a tagline is the
--    one line that has to work on a CARD, in a listing, under a cover. The card
--    had been truncating the synopsis to stand in for it, which gives every
--    fiction on a browse page the first sentence of its plot summary rather
--    than the line its author would have chosen.
--
-- 2. novels.foreword - บทนำ.
--
--    What an author says BEFORE the story: content notes, a dedication, who the
--    cast are, where an AU diverges. It was going into chapter one, which meant
--    a reader coming back for chapter twelve had to scroll past it, and a
--    reader who wanted it after chapter twelve could not find it.
--
--    Deliberately separate from author_note_start (13K), which is the note
--    attached to a CHAPTER. This one belongs to the fiction.
--
-- 3. genres.kind - the controlled vocabulary answers three questions, not one.
--
--    "Romance" and "Boy's Love" are not alternatives; a fiction is routinely
--    both, and a reader browsing for one is not browsing for the other. Same
--    for AU: whether a fiction is an alternate universe, and which one, is a
--    thing readers filter on and a thing the flat list could not express.
--
--    A `kind` column rather than three tables: they are all controlled
--    vocabulary, they all attach through novel_genres, and every query that
--    already reads them keeps working unchanged. What changes is that a picker
--    can ask three questions and a filter can name which one it means.
--
--    The seven seeded rows keep their ids, slugs, and every assignment made to
--    them - they are simply labelled 'content' and given the Thai names writers
--    actually use. Renaming a genre changes what it is CALLED, never which
--    fictions are in it.

-- +migrate Up

ALTER TABLE novels
    ADD COLUMN tagline  VARCHAR(200) NULL,
    ADD COLUMN foreword TEXT         NULL;

COMMENT ON COLUMN novels.tagline IS
    'คำโปรย (13S) - the one line under a cover. Distinct from description, '
    'which is the synopsis a reader opens the fiction page for.';

COMMENT ON COLUMN novels.foreword IS
    'บทนำ (13S) - what the author says before the story begins. Belongs to the '
    'fiction; author_note_start belongs to a chapter.';

ALTER TABLE genres
    ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'content',
    ADD CONSTRAINT genres_kind_valid
        CHECK (kind IN ('content', 'relationship', 'au'));

COMMENT ON COLUMN genres.kind IS
    'Which question this term answers (13S): content = what the story is like, '
    'relationship = who it is about, au = which alternate universe. All three '
    'attach through novel_genres.';

-- The Thai names writers actually use. Ids, slugs, and every existing
-- assignment are untouched - this renames, it does not re-classify.
UPDATE genres SET name = 'โรแมนติก'      WHERE slug = 'romance';
UPDATE genres SET name = 'ดราม่าปวดตับ'  WHERE slug = 'drama';
UPDATE genres SET name = 'ตลก'           WHERE slug = 'comedy';
UPDATE genres SET name = 'แฟนตาซี'       WHERE slug = 'fantasy';
UPDATE genres SET name = 'สยองขวัญ'      WHERE slug = 'horror';
UPDATE genres SET name = 'สืบสวนสอบสวน'  WHERE slug = 'mystery';
UPDATE genres SET name = 'ไซไฟ'          WHERE slug = 'sci-fi';

INSERT INTO genres (name, slug, kind, description) VALUES
    ('รักหวานแหวว',   'fluff',          'content', 'อบอุ่น เบา ๆ อ่านแล้วยิ้ม'),
    ('เจ็บปวด',        'angst',          'content', 'เข้มข้น กดดัน สะเทือนใจ'),
    ('ผจญภัย',        'adventure',      'content', 'ออกเดินทาง ภารกิจ และโลกกว้าง'),
    ('ชีวิตประจำวัน',  'slice-of-life',  'content', 'เรื่องเล็ก ๆ ในชีวิตที่เดินไปเรื่อย ๆ'),

    ('Boy''s Love (BL)',  'bl',      'relationship', 'ความสัมพันธ์ชายกับชาย'),
    ('Girl''s Love (GL)', 'gl',      'relationship', 'ความสัมพันธ์หญิงกับหญิง'),
    ('ชาย-หญิง',          'het',     'relationship', 'ความสัมพันธ์เพศตรงข้าม'),
    ('Reader',            'reader',  'relationship', 'ผู้อ่านเป็นตัวละครในเรื่อง'),
    ('OC',                'oc',      'relationship', 'ตัวละครที่ผู้เขียนสร้างขึ้นเอง'),

    ('AU ไทย',      'au-thai',      'au', 'ย้ายฉากมาไว้ในบริบทไทย'),
    ('AU มหาลัย',   'au-campus',    'au', 'ชีวิตในมหาวิทยาลัย'),
    ('AU คาเฟ่',    'au-cafe',      'au', 'ร้านกาแฟ ร้านขนม และชีวิตในร้าน'),
    ('AU มัธยม',    'au-highschool','au', 'ชีวิตวัยมัธยม'),
    ('AU ออฟฟิศ',   'au-office',    'au', 'ที่ทำงานและชีวิตวัยทำงาน'),
    ('AU โอเมก้า',  'au-omegaverse','au', 'โลกแบบโอเมก้าเวิร์ส'),
    ('AU แฟนตาซี',  'au-fantasy',   'au', 'ย้ายฉากไปโลกเวทมนตร์'),
    ('AU ย้อนยุค',  'au-historical','au', 'ฉากในอดีตหรือย้อนยุค')
ON CONFLICT (slug) DO NOTHING;

-- +migrate Down

DELETE FROM genres
WHERE kind IN ('relationship', 'au')
  AND NOT EXISTS (SELECT 1 FROM novel_genres ng WHERE ng.genre_id = genres.id);

ALTER TABLE genres
    DROP CONSTRAINT IF EXISTS genres_kind_valid;
ALTER TABLE genres
    DROP COLUMN IF EXISTS kind;

-- The English names the vocabulary shipped with.
UPDATE genres SET name = 'Romance' WHERE slug = 'romance';
UPDATE genres SET name = 'Drama'   WHERE slug = 'drama';
UPDATE genres SET name = 'Comedy'  WHERE slug = 'comedy';
UPDATE genres SET name = 'Fantasy' WHERE slug = 'fantasy';
UPDATE genres SET name = 'Horror'  WHERE slug = 'horror';
UPDATE genres SET name = 'Mystery' WHERE slug = 'mystery';
UPDATE genres SET name = 'Sci-Fi'  WHERE slug = 'sci-fi';

ALTER TABLE novels
    DROP COLUMN IF EXISTS foreword,
    DROP COLUMN IF EXISTS tagline;
