-- 036_item_ratings.sql — a title's age rating (idempotent).
--
-- A kid's account is capped at an age, the max_rating claim of its access
-- token: katalog-api leaves every title rated above the cap out of what it
-- serves the kid, and chino-api answers 404 for one asked for by id. A title's
-- rating comes from TMDB. Enrichment (and backfillRatings) reads the
-- certifications TMDB gives it, a film's theatrical and digital releases
-- (release_dates, types 3 and 4) and a series' content ratings, in the
-- countries the setting ratings.countries names, in its order (CH,DE,US by
-- default): the first of them with a certification the catalog's table rates
-- (internal/ratings) wins, and of a country's certifications the strictest.
-- Per title (com_nalet_katalog_items):
--   certification             the certification as TMDB gives it in that
--                             country ("12", "PG-13", "TV-MA"); NULL: no
--                             country of the list rates the title
--   certification_country     that country (ISO 3166-1 alpha-2, "DE")
--   min_age                   the minimum age, in years, the certification
--                             means; set with certification, or neither
--   min_age_override          an admin's rating of the title, in years: it
--                             wins over min_age; NULL: none
--   certification_fetched_at  when TMDB's certifications of the title were
--                             last read; NULL: never (backfillRatings reads
--                             such titles first)
-- An episode carries no certification of its own: it is rated as its series
-- (its own min_age_override still wins), which a reader takes through
-- parent_id. The age a viewer is held to is then the item's override, else
-- its parent's override, else its parent's min_age, else its own min_age;
-- NULL is unrated.
--
-- idx_items_rated_age indexes that age for a title without a parent (a film,
-- a series), which a capped viewer's lists of them filter on.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table; the new
-- timestamp carries its time zone (timestamptz). Adding the columns rewrites
-- nothing: none has a default.
ALTER TABLE com_nalet_katalog_items
  ADD COLUMN IF NOT EXISTS certification            VARCHAR(40),
  ADD COLUMN IF NOT EXISTS certification_country    VARCHAR(2) CHECK (certification_country ~ '^[A-Z]{2}$'),
  ADD COLUMN IF NOT EXISTS min_age                  SMALLINT CHECK (min_age BETWEEN 0 AND 21),
  ADD COLUMN IF NOT EXISTS min_age_override         SMALLINT CHECK (min_age_override BETWEEN 0 AND 21),
  ADD COLUMN IF NOT EXISTS certification_fetched_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_items_rated_age
  ON com_nalet_katalog_items ((COALESCE(min_age_override, min_age)));
