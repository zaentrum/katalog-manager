-- 056_superseded_sources.sql — an original another file took the place of (idempotent).
--
-- A title given another file in place of its original (replaceSource with
-- deleteOldFile, the library's v2 layout) keeps the old original where it
-- lies, superseded, until the retire job retires it: once a version of the
-- title made without it is complete and verified, through the trash, as it
-- retires an original. com_nalet_katalog_itemsources.state gains the state:
--   present     the original is there, a source of its title
--   retiring    its deletion is claimed
--   superseded  another file took its place as the title's; it is there still
--   deleted     deleted (after packaging, or once superseded)
--   removed     no original of its item after all
-- and of a source that is or was superseded
--   supersededby  the source that took its place, set from then on, also
--                 while it is retiring and once it is deleted: a retirement
--                 of a superseded original is told by it, and one called off
--                 leaves it superseded again
--   supersededat  since when
-- replacekey is the key a replace that gave the title a file was asked with
-- (replaceSource's key, an addon's id of what it hands over), on the source
-- of that file: one key gives a title one file, however often it is asked.
-- The check on the state that does not admit superseded goes, whatever it is
-- named, and one that does is added: under the name Postgres gave 040's.
-- Lowercase identifiers, like every com_nalet_katalog_* table.
ALTER TABLE com_nalet_katalog_itemsources
  ADD COLUMN IF NOT EXISTS supersededby VARCHAR(36),
  ADD COLUMN IF NOT EXISTS supersededat TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS replacekey VARCHAR(255);
DO $$
DECLARE c record;
BEGIN
  FOR c IN SELECT conname FROM pg_constraint
      WHERE conrelid = 'com_nalet_katalog_itemsources'::regclass AND contype = 'c'
        AND pg_get_constraintdef(oid) LIKE '%state%' AND pg_get_constraintdef(oid) NOT LIKE '%''superseded''%'
  LOOP
    EXECUTE format('ALTER TABLE com_nalet_katalog_itemsources DROP CONSTRAINT %I', c.conname);
  END LOOP;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
      WHERE conrelid = 'com_nalet_katalog_itemsources'::regclass AND contype = 'c'
        AND pg_get_constraintdef(oid) LIKE '%''superseded''%') THEN
    ALTER TABLE com_nalet_katalog_itemsources ADD CONSTRAINT com_nalet_katalog_itemsources_state_check
      CHECK (state IN ('present', 'retiring', 'superseded', 'deleted', 'removed'));
  END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS idx_itemsources_replacekey ON com_nalet_katalog_itemsources (item_id, replacekey)
  WHERE replacekey IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_itemsources_superseded ON com_nalet_katalog_itemsources (supersededat, id)
  WHERE state = 'superseded';
