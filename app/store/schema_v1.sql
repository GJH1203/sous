-- Times are UTC in RFC 3339 ("2026-11-14T03:04:05Z"), so they sort as text.

-- A dish is what gets cooked again and again; a cook is one time it was.
CREATE TABLE dishes (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE cooks (
    id         INTEGER PRIMARY KEY,
    dish_id    INTEGER NOT NULL REFERENCES dishes (id),
    -- The day it was cooked, in the app's time zone: "yesterday" names a
    -- day, not an instant.
    cooked_on  TEXT NOT NULL
        CHECK (cooked_on GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    rating     REAL CHECK (rating >= 0 AND rating <= 10), -- NULL: not rated
    note       TEXT NOT NULL DEFAULT '',
    author_id  TEXT NOT NULL, -- the Discord user who recorded it
    source     TEXT NOT NULL, -- the message it was extracted from
    created_at TEXT NOT NULL
);
CREATE INDEX cooks_by_dish ON cooks (dish_id, cooked_on);

-- Ingredients belong to a cook, not to the dish: the same dish is sometimes
-- made with minced pork and sometimes without.
CREATE TABLE ingredients (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE -- the canonical name, after aliases
);

CREATE TABLE cook_ingredients (
    cook_id       INTEGER NOT NULL REFERENCES cooks (id) ON DELETE CASCADE,
    ingredient_id INTEGER NOT NULL REFERENCES ingredients (id),
    PRIMARY KEY (cook_id, ingredient_id)
) WITHOUT ROWID;
CREATE INDEX cook_ingredients_by_ingredient ON cook_ingredients (ingredient_id);

-- 西红柿 -> 番茄. Both lookups and inserts go through this table first.
CREATE TABLE ingredient_aliases (
    alias         TEXT PRIMARY KEY,
    ingredient_id INTEGER NOT NULL REFERENCES ingredients (id)
) WITHOUT ROWID;
