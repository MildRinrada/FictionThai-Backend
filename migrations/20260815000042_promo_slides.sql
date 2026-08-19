-- +migrate Up

-- The home hero's curated slide queue (docs/HOME-PROMO.md). Slides are
-- CONTENT the staff schedules, not a banner network: each carries its own
-- promo art and copy, a source label, and a window. The paid-ratio and
-- labelling rules are enforced in the service at read time.
CREATE TABLE promo_slides (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    position    INTEGER      NOT NULL DEFAULT 0,

    kicker      VARCHAR(40)  NOT NULL DEFAULT '',
    headline    VARCHAR(120) NOT NULL,
    tagline     VARCHAR(160) NOT NULL DEFAULT '',
    cta_label   VARCHAR(40)  NOT NULL DEFAULT '',

    -- Internal path only ("/novel/..."), validated in the service - an
    -- external URL here would be an open redirect wearing a hero image.
    link_url    VARCHAR(500) NOT NULL,

    image_url   VARCHAR(500),
    bg_color    VARCHAR(7),
    -- Which side the copy sits on, over the banner art.
    text_side   VARCHAR(8)   NOT NULL DEFAULT 'start',

    -- editorial / paid / event (docs/HOME-PROMO.md "three sources").
    source      VARCHAR(16)  NOT NULL DEFAULT 'editorial',

    enabled     BOOLEAN      NOT NULL DEFAULT false,
    starts_at   TIMESTAMPTZ,
    ends_at     TIMESTAMPTZ,

    -- Serving counts, not billing-grade analytics (docs/HOME-PROMO.md "Stats").
    impressions BIGINT       NOT NULL DEFAULT 0,
    clicks      BIGINT       NOT NULL DEFAULT 0,

    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX promo_slides_active
    ON promo_slides (enabled, position)
    WHERE enabled;

-- +migrate Down
DROP TABLE promo_slides;
