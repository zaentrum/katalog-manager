-- 044_library_takein.sql — a version taken in: its original, and no package (idempotent).
--
-- A title's original goes into the library, as versions/<versionId>/
-- original.<ext>, with the first version of it: when its first package is
-- built, or, when the title gets no package now, without one (its transcode
-- was refused, its transcode or its package failed with no attempt left, an
-- admin took it in, or the migration staged it unpackaged). A version of the
-- second kind is taken: its folder holds its version.json and its original,
-- and no package; the title plays from that original as a title without a
-- package does, and the version is complete once a package is added to its
-- folder. com_nalet_katalog_itemversions.state gains the state:
--   building    the pipeline works on it, its folder not written yet
--   taken       its folder holds its original and no package
--   complete    its package is recorded and plays (one per item at most)
--   superseded  a newer one took over
--   removed     gone from the item
-- The check on the state that does not admit taken goes, whatever it is
-- named, and one that does is added: under the name Postgres gave 040's.
-- Lowercase identifiers, like every com_nalet_katalog_* table.
DO $$
DECLARE c record;
BEGIN
  FOR c IN SELECT conname FROM pg_constraint
      WHERE conrelid = 'com_nalet_katalog_itemversions'::regclass AND contype = 'c'
        AND pg_get_constraintdef(oid) LIKE '%state%' AND pg_get_constraintdef(oid) NOT LIKE '%''taken''%'
  LOOP
    EXECUTE format('ALTER TABLE com_nalet_katalog_itemversions DROP CONSTRAINT %I', c.conname);
  END LOOP;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
      WHERE conrelid = 'com_nalet_katalog_itemversions'::regclass AND contype = 'c'
        AND pg_get_constraintdef(oid) LIKE '%''taken''%') THEN
    ALTER TABLE com_nalet_katalog_itemversions ADD CONSTRAINT com_nalet_katalog_itemversions_state_check
      CHECK (state IN ('building', 'taken', 'complete', 'superseded', 'removed'));
  END IF;
END $$;
