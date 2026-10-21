-- 049_catalog_search.sql — what katalog-api's ranked search finds titles and people by (idempotent).
--
-- katalog-api ranks a search by how a title matches it: the title is the
-- query, begins with it, holds every word of it, a series' other name holds
-- them, an episode's title does, the title is close to it (a typo), and last,
-- when nothing else matched, the plot holds them. Each of those reads a
-- title, a name or a plot as it is normalized, so that "widows bay", "widow's
-- bay" and "Widow´s Bay" are one and "leon" finds "Léon":
--
--   com_nalet_katalog_search_text(t)    lower case, accents off (unaccent's
--                                       rules), apostrophes dropped
--                                       ("Widow's" is one word, "widows"),
--                                       every other mark and space one space:
--                                       "Spider-Man: Far From Home" is
--                                       "spider man far from home"
--   com_nalet_katalog_search_joined(t)  the same, the marks inside a word
--                                       (- . · • / & + *) dropped too:
--                                       "spiderman far from home", "S.W.A.T."
--                                       "swat", "WALL·E" "walle"
--   com_nalet_katalog_search_words(t)   the words of both, as a tsvector
--                                       (configuration simple: no stemming,
--                                       no stop words)
--   com_nalet_katalog_search_plot(t)    the words of the first, for a plot
--
-- They are IMMUTABLE so that an index can hold their results. unaccent() is
-- STABLE because its dictionary could change; they name the dictionary, as
-- the catalog's immutable_unaccent does. Every name in them is schema
-- qualified: from PostgreSQL 17 on, an index's expression is computed under
-- a search_path of pg_catalog alone during CREATE INDEX, REINDEX, VACUUM and
-- ANALYZE. Their cost says they are expensive, so that the planner reaches
-- for the indexes rather than computing them row by row.
--
-- Never change what a function returns: an index holds what it returned when
-- the row was written, and a change would leave it disagreeing with the
-- function. A change of normalization makes new functions and new indexes.
--
-- The indexes, built CONCURRENTLY: a build neither blocks the catalog's
-- writes nor rewrites the table. CREATE INDEX CONCURRENTLY cannot run inside
-- a transaction, and a build that is cut short leaves its index invalid, so
-- katalog-manager runs each statement after a line "--@index <name>" on its
-- own, in the background after startup, drops an invalid index of that name
-- and builds it again (store.EnsureCatalogSearch). Everything before the
-- first "--@index" line runs as one statement, before the indexes.
--
--   idx_items_search_words     the words of a title (a GIN of
--                              com_nalet_katalog_search_words(title)): every
--                              word of a query begins a word of it
--   idx_items_search_prefix    a title by its type and its text: it is the
--                              query, it begins with it (a B-tree,
--                              text_pattern_ops, with the id and the title, so
--                              that a scan of it reads no row)
--   idx_items_search_trigrams  the trigrams of a title's text (pg_trgm): a
--                              title close to the query
--   idx_items_search_scantitle the words of the name the scanner made a series
--                              under (046's scantitle), a series' other name
--   idx_items_search_plot      the words of a plot
--   idx_people_search_words, idx_people_search_prefix,
--   idx_people_search_trigrams a person's name, as a title's
--   idx_people_search_aka      the other names a person is known by (030's
--                              alsoknownas)
--   idx_itempeople_person      a person's credits, which a person found is
--                              ranked by (and a filmography is read by)
--
-- Lowercase identifiers, like every com_nalet_katalog_* table. A large
-- catalog (78,000 items, 16,000 people) builds them in seconds.

DO $migration$
DECLARE
  catalog text := current_schema();
  accents text;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'unaccent') THEN
    CREATE EXTENSION unaccent;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
    CREATE EXTENSION pg_trgm;
  END IF;
  SELECT n.nspname INTO accents FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
  WHERE e.extname = 'unaccent';

  EXECUTE format($f$
    CREATE OR REPLACE FUNCTION %1$I.com_nalet_katalog_search_text(t text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE COST 10000 AS $body$
      SELECT pg_catalog.btrim(pg_catalog.regexp_replace(pg_catalog.regexp_replace(
        pg_catalog.lower(%2$I.unaccent(%3$L::regdictionary, t)),
        '[\U00000027\U00000060\U000000b4\U000002bc\U00002018\U00002019\U00002032\U0000ff07]+', '', 'g'),
        '[[:space:]\U00000021-\U0000002f\U0000003a-\U00000040\U0000005b-\U00000060\U0000007b-\U0000007e\U000000a0-\U000000bf\U000000d7\U000000f7\U00002000-\U0000206f\U00002e00-\U00002e7f\U00003000-\U00003003\U00003008-\U00003011\U00003014-\U0000301f\U0000fe10-\U0000fe1f\U0000fe30-\U0000fe6b\U0000ff01-\U0000ff0f\U0000ff1a-\U0000ff20\U0000ff3b-\U0000ff40\U0000ff5b-\U0000ff65]+',
        ' ', 'g'))
    $body$$f$, catalog, accents, accents || '.unaccent');

  EXECUTE format($f$
    CREATE OR REPLACE FUNCTION %1$I.com_nalet_katalog_search_joined(t text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE COST 10000 AS $body$
      SELECT pg_catalog.btrim(pg_catalog.regexp_replace(pg_catalog.regexp_replace(
        pg_catalog.lower(%2$I.unaccent(%3$L::regdictionary, t)),
        '[\U00000027\U00000060\U000000b4\U000002bc\U00002018\U00002019\U00002032\U0000ff07\U0000002e\U000000b7\U00002022\U0000002f\U00000026\U0000002b\U0000002a\U0000002d\U00002010-\U00002015\U00002212]+', '', 'g'),
        '[[:space:]\U00000021-\U0000002f\U0000003a-\U00000040\U0000005b-\U00000060\U0000007b-\U0000007e\U000000a0-\U000000bf\U000000d7\U000000f7\U00002000-\U0000206f\U00002e00-\U00002e7f\U00003000-\U00003003\U00003008-\U00003011\U00003014-\U0000301f\U0000fe10-\U0000fe1f\U0000fe30-\U0000fe6b\U0000ff01-\U0000ff0f\U0000ff1a-\U0000ff20\U0000ff3b-\U0000ff40\U0000ff5b-\U0000ff65]+',
        ' ', 'g'))
    $body$$f$, catalog, accents, accents || '.unaccent');

  EXECUTE format($f$
    CREATE OR REPLACE FUNCTION %1$I.com_nalet_katalog_search_words(t text) RETURNS tsvector
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $body$
      SELECT pg_catalog.to_tsvector('pg_catalog.simple'::regconfig,
        %1$I.com_nalet_katalog_search_text(t) || ' ' || %1$I.com_nalet_katalog_search_joined(t))
    $body$$f$, catalog);

  EXECUTE format($f$
    CREATE OR REPLACE FUNCTION %1$I.com_nalet_katalog_search_plot(t text) RETURNS tsvector
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $body$
      SELECT pg_catalog.to_tsvector('pg_catalog.simple'::regconfig, %1$I.com_nalet_katalog_search_text(t))
    $body$$f$, catalog);
END $migration$;

--@index idx_items_search_words
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_items_search_words ON com_nalet_katalog_items
  USING gin (com_nalet_katalog_search_words(title));
--@index idx_items_search_prefix
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_items_search_prefix ON com_nalet_katalog_items
  (type, com_nalet_katalog_search_text(title) text_pattern_ops) INCLUDE (id, title);
--@index idx_items_search_trigrams
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_items_search_trigrams ON com_nalet_katalog_items
  USING gin (com_nalet_katalog_search_text(title) gin_trgm_ops);
--@index idx_items_search_scantitle
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_items_search_scantitle ON com_nalet_katalog_items
  USING gin (com_nalet_katalog_search_words(scantitle)) WHERE scantitle IS NOT NULL;
--@index idx_items_search_plot
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_items_search_plot ON com_nalet_katalog_items
  USING gin (com_nalet_katalog_search_plot(description));
--@index idx_people_search_words
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_people_search_words ON com_nalet_katalog_people
  USING gin (com_nalet_katalog_search_words(name));
--@index idx_people_search_prefix
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_people_search_prefix ON com_nalet_katalog_people
  (com_nalet_katalog_search_text(name) text_pattern_ops) INCLUDE (id, name);
--@index idx_people_search_trigrams
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_people_search_trigrams ON com_nalet_katalog_people
  USING gin (com_nalet_katalog_search_text(name) gin_trgm_ops);
--@index idx_people_search_aka
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_people_search_aka ON com_nalet_katalog_people
  USING gin (com_nalet_katalog_search_words(alsoknownas::text)) WHERE alsoknownas IS NOT NULL;
--@index idx_itempeople_person
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_itempeople_person ON com_nalet_katalog_itempeople (person_id);
