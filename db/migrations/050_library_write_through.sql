-- 050_library_write_through.sql — every change of what a projection shows marks it (idempotent).
--
-- Migration 041 marks an item, or a person, whose projection is behind when
-- a statement changes what it shows, whoever writes the rows. A projection
-- shows more than 041 knew of:
--   - an item's marks, its chapters and its intro, recap and credits ranges,
--     which metadata.json shows (marks): a statement that inserts, changes or
--     deletes any marks each item it touched once, with 041's function. A
--     version's version.json keeps the marks the version was made with,
--     written once; a detector run again, or a person, changes them later,
--     and only the projection can follow;
--   - an extra inserted or deleted recorded, and not removed, marks its title
--     (041 marks one an update shows otherwise, removes or records);
--   - a genre renamed marks the items it is a genre of;
--   - a change of a projected column of a person (a name, the dates and the
--     place, the biography, the ids, the department, the locks and the
--     origins) marks them unless the statement sets modifiedat itself. When
--     the catalog last asked TMDB about them is bookkeeping, and marks
--     nothing: a refresh that finds nothing new moves only that.
-- The version that plays (library.primaryVersionId) and the file that
-- covers an episode (library.coveredBy, numbering.episodeEnd) are marked by
-- their writers, not here: packaging-complete marks the title and the
-- episodes its file covers in the transaction that makes a version complete,
-- and Link and Unlink mark an episode and its holders; an adopt writes the
-- versions and the links its staged projections show already, so a trigger
-- would mark every title it records, and the episodes covered by those
-- adopted before them, which were projected.
-- The person's trigger is made only where migration 030's columns are.
-- Lowercase identifiers, like every com_nalet_katalog_* table.

-- An item's marks: each item a statement touched, once (041's function).
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['com_nalet_katalog_mediasegments', 'com_nalet_katalog_itemchapters'] LOOP
    EXECUTE format('DROP TRIGGER IF EXISTS library_insert ON %I', t);
    EXECUTE format('CREATE TRIGGER library_insert AFTER INSERT ON %I REFERENCING NEW TABLE AS library_new '
      'FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_items_touched()', t);
    EXECUTE format('DROP TRIGGER IF EXISTS library_update ON %I', t);
    EXECUTE format('CREATE TRIGGER library_update AFTER UPDATE ON %I REFERENCING OLD TABLE AS library_old '
      'NEW TABLE AS library_new FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_items_touched()', t);
    EXECUTE format('DROP TRIGGER IF EXISTS library_delete ON %I', t);
    EXECUTE format('CREATE TRIGGER library_delete AFTER DELETE ON %I REFERENCING OLD TABLE AS library_old '
      'FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_items_touched()', t);
  END LOOP;
END $$;

-- The titles of the extras a statement inserted or deleted recorded and not
-- removed: the decisions about how they are shown are their projection's.
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_extras_listed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    UPDATE com_nalet_katalog_items SET modifiedat = now()
    WHERE id IN (SELECT item_id FROM library_new WHERE recordedat IS NOT NULL AND removedat IS NULL);
  ELSE
    UPDATE com_nalet_katalog_items SET modifiedat = now()
    WHERE id IN (SELECT item_id FROM library_old WHERE recordedat IS NOT NULL AND removedat IS NULL);
  END IF;
  RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS library_insert ON com_nalet_katalog_itemextras;
CREATE TRIGGER library_insert AFTER INSERT ON com_nalet_katalog_itemextras
  REFERENCING NEW TABLE AS library_new
  FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_extras_listed();
DROP TRIGGER IF EXISTS library_delete ON com_nalet_katalog_itemextras;
CREATE TRIGGER library_delete AFTER DELETE ON com_nalet_katalog_itemextras
  REFERENCING OLD TABLE AS library_old
  FOR EACH STATEMENT EXECUTE FUNCTION com_nalet_katalog_library_extras_listed();

-- The items of a genre renamed: their genres show its name.
CREATE OR REPLACE FUNCTION com_nalet_katalog_library_genre_renamed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  UPDATE com_nalet_katalog_items SET modifiedat = now()
  WHERE id IN (SELECT item_id FROM com_nalet_katalog_itemgenres WHERE genre_id = NEW.id);
  RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS library_renamed ON com_nalet_katalog_genres;
CREATE TRIGGER library_renamed AFTER UPDATE ON com_nalet_katalog_genres
  FOR EACH ROW WHEN (OLD.name IS DISTINCT FROM NEW.name)
  EXECUTE FUNCTION com_nalet_katalog_library_genre_renamed();

-- A person whose projected columns changed, unless the statement says when.
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('com_nalet_katalog_people') AND attnum > 0
      AND NOT attisdropped AND attname IN ('sortname', 'alsoknownas', 'birthdate', 'deathdate', 'birthplace',
        'biography', 'tmdbpersonid', 'imdbid', 'knownfordepartment', 'metadatalocked', 'lockedfields', 'fieldorigins',
        'modifiedat')) = 13 THEN
    EXECUTE $f$
      CREATE OR REPLACE FUNCTION com_nalet_katalog_library_person_changed() RETURNS trigger
      LANGUAGE plpgsql AS $b$
      BEGIN
        IF NEW.modifiedat IS NOT DISTINCT FROM OLD.modifiedat AND (
             NEW.name IS DISTINCT FROM OLD.name OR NEW.sortname IS DISTINCT FROM OLD.sortname
          OR NEW.alsoknownas IS DISTINCT FROM OLD.alsoknownas OR NEW.birthdate IS DISTINCT FROM OLD.birthdate
          OR NEW.deathdate IS DISTINCT FROM OLD.deathdate OR NEW.birthplace IS DISTINCT FROM OLD.birthplace
          OR NEW.biography IS DISTINCT FROM OLD.biography OR NEW.tmdbpersonid IS DISTINCT FROM OLD.tmdbpersonid
          OR NEW.imdbid IS DISTINCT FROM OLD.imdbid OR NEW.knownfordepartment IS DISTINCT FROM OLD.knownfordepartment
          OR NEW.metadatalocked IS DISTINCT FROM OLD.metadatalocked OR NEW.lockedfields IS DISTINCT FROM OLD.lockedfields
          OR NEW.fieldorigins IS DISTINCT FROM OLD.fieldorigins) THEN
          NEW.modifiedat := now();
        END IF;
        RETURN NEW;
      END $b$
    $f$;
    DROP TRIGGER IF EXISTS library_changed ON com_nalet_katalog_people;
    CREATE TRIGGER library_changed BEFORE UPDATE ON com_nalet_katalog_people
      FOR EACH ROW EXECUTE FUNCTION com_nalet_katalog_library_person_changed();
  END IF;
END $$;
