-- 040_library_v2.sql — the catalog keeps the library record's paths (idempotent).
--
-- With the setting library.layout=v2 the share's root is the library:
-- movies/, series/ and people/ hold the record, and .work/ everything that is
-- not the record (arrivals, the workers' handoffs, the trash). The catalog
-- decides every path in it, and keeps here what it decided and what became of
-- each file. Nothing reads these columns and tables while library.layout is
-- legacy, so this changes nothing for a catalog that has not moved.
--
-- com_nalet_katalog_items
--   recordedat          when the item's item.json was written: from then on
--                       its type and its parent are its identity, kept
--   libraryprojectedat  the modifiedat its last metadata.json reflects: the
--                       projector writes it again once modifiedat is newer
--   retirehold          its originals are kept whatever library.originals says
-- com_nalet_katalog_people
--   libraryprojectedat  the modifiedat their last person.json reflects
-- com_nalet_katalog_itemsources, one row per original a title was given (the
--   sourceId, the name of its folder sources/<sourceId>/): where it arrived
--   (arrivalpath, absolute, while it exists; librarypath relative to
--   ARRIVALS_ROOT), its size and quick hash, and what became of it: present,
--   retiring (its deletion claimed), deleted (after packaging: its record and
--   its package are what is left) or removed; when its record was written
--   (recordedat, recorddir), how the packager mapped its subtitle files
--   (sidecars), and its deletion: the event that records it, the trash it went
--   to, who deleted it and what the package does not carry of it (lost).
-- com_nalet_katalog_itemversions, one row per package run of a title (the
--   versionId, the name of its folder versions/<versionId>/): building while
--   the pipeline works on it, complete once its package is recorded (one per
--   item at most), superseded by the next one, and removed after a grace;
--   where it is, when it was verified and how far, and the events that record
--   its supersession and its removal.
-- com_nalet_katalog_playbackassets: the source (sourceid) and the version
--   (versionid) a row is of. A retired original's row is kind 'original'.
-- com_nalet_katalog_itemextras: an extra's package (packageid), when its
--   folder was recorded, and when its original was deleted.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table. The read-only
-- role of the catalog's readers (katalog-api) may read the new tables.
ALTER TABLE com_nalet_katalog_items
  ADD COLUMN IF NOT EXISTS recordedat TIMESTAMPTZ,            -- item.json written
  ADD COLUMN IF NOT EXISTS libraryprojectedat TIMESTAMP,      -- the modifiedat the last metadata.json reflects
  ADD COLUMN IF NOT EXISTS retirehold BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE com_nalet_katalog_people
  ADD COLUMN IF NOT EXISTS libraryprojectedat TIMESTAMPTZ;
CREATE TABLE IF NOT EXISTS com_nalet_katalog_itemsources (
  id VARCHAR(36) PRIMARY KEY,                    -- the sourceId: sources/<id>/
  item_id VARCHAR(36) NOT NULL,
  filename VARCHAR(1024) NOT NULL,               -- source.json file.name
  arrivalpath VARCHAR(2048),                     -- absolute, while the original exists; NULL after retire
  librarypath VARCHAR(2048),                     -- relative to ARRIVALS_ROOT (source.json origin.libraryPath)
  sizebytes BIGINT NOT NULL,
  qh1 VARCHAR(71) CHECK (qh1 IS NULL OR qh1 ~ '^sha256:[0-9a-f]{64}$'),
  state VARCHAR(12) NOT NULL DEFAULT 'present' CHECK (state IN ('present','retiring','deleted','removed')),
  recordedat TIMESTAMPTZ, recorddir VARCHAR(2048),
  sidecars JSONB CHECK (sidecars IS NULL OR jsonb_typeof(sidecars)='array'),  -- [{subtitleAssetId, rendition, path}]
  retireeventid VARCHAR(36), retireeventat TIMESTAMPTZ, trashpath VARCHAR(2048),
  deletedat TIMESTAMPTZ, deletedby VARCHAR(255), lost JSONB, error VARCHAR(500),
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(), modifiedat TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS idx_itemsources_item ON com_nalet_katalog_itemsources (item_id);
CREATE INDEX IF NOT EXISTS idx_itemsources_fixity ON com_nalet_katalog_itemsources (sizebytes, qh1);
CREATE UNIQUE INDEX IF NOT EXISTS idx_itemsources_arrival ON com_nalet_katalog_itemsources (arrivalpath) WHERE arrivalpath IS NOT NULL;
CREATE TABLE IF NOT EXISTS com_nalet_katalog_itemversions (
  id VARCHAR(36) PRIMARY KEY,                    -- the versionId: versions/<id>/
  item_id VARCHAR(36) NOT NULL,
  sourceids VARCHAR(36)[] NOT NULL DEFAULT '{}',
  state VARCHAR(12) NOT NULL CHECK (state IN ('building','complete','superseded','removed')),
  packageid VARCHAR(36), dir VARCHAR(2048),
  completedat TIMESTAMPTZ, verifiedat TIMESTAMPTZ,
  verifiedlevel VARCHAR(8) CHECK (verifiedlevel IS NULL OR verifiedlevel IN ('chain','full')),
  supersededby VARCHAR(36), supersededat TIMESTAMPTZ, supersedeeventid VARCHAR(36),
  removedat TIMESTAMPTZ, removeeventid VARCHAR(36),
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(), modifiedat TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS idx_itemversions_item ON com_nalet_katalog_itemversions (item_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_itemversions_building ON com_nalet_katalog_itemversions (item_id) WHERE state='building';
CREATE UNIQUE INDEX IF NOT EXISTS idx_itemversions_current  ON com_nalet_katalog_itemversions (item_id) WHERE state='complete';
ALTER TABLE com_nalet_katalog_playbackassets
  ADD COLUMN IF NOT EXISTS sourceid VARCHAR(36), ADD COLUMN IF NOT EXISTS versionid VARCHAR(36);
ALTER TABLE com_nalet_katalog_itemextras
  ADD COLUMN IF NOT EXISTS packageid VARCHAR(36), ADD COLUMN IF NOT EXISTS recordedat TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS sourcedeletedat TIMESTAMPTZ;
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_itemsources, com_nalet_katalog_itemversions TO cloud_katalog_ro; END IF; END $$;
