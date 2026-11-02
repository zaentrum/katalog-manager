-- 054_picture_areas.sql — the part of each title's frame its picture fills (idempotent).
--
-- Many sources keep a full frame with a wider picture letterboxed inside it,
-- black bars baked into the video (a 1920x1080 frame whose picture is
-- 1920x804), or a narrower one pillarboxed. The players fit the picture area
-- to the screen rather than the frame. The analyzer measures it (its pass of
-- a new title reads the original; the backfill, the titles that have none,
-- the package where there is one), and the catalog keeps it here, in pixels
-- with the frame it was measured in. The packages stay as they are.
--
-- com_nalet_katalog_pictureareas, a movie, an episode or an extra a row:
--   id            the item's id, or the extra's
--   kind          item or extra
--   item_id       the title: the item itself, or the extra's title
--   state         pending (a backfill run claimed it, nothing measured yet),
--                 measured (frame and area set; the area is the frame itself
--                 when there are no bars), unknown (the analyzer could not
--                 tell, reason says why), or failed (it could not read the
--                 file, reason says why; tried again from nextattemptat)
--   framewidth, frameheight
--                 the frame measured, as the video codes it, bars included
--   areax, areay, areawidth, areaheight
--                 the picture's area in it, pixels from its top left corner,
--                 rounded outwards to even pixels
--   path          the file measured: an original, or the video rendition of
--                 a package (its HLS playlist)
--   sourceid, versionid, packageid
--                 what that file is: the original of migration 040 it is or
--                 the package was made of, the version whose package it is,
--                 the extra's package
--   method        the measurement's version, as the analyzer names it
--   samples, informative
--                 the frames it decoded, and those that showed a picture
--   durationms, seconds, cpuseconds
--                 the length of the video measured, and what measuring it
--                 took: its time and its CPU
--   evidence      per side, the bar it found and why (the analyzer's JSON)
--   reason        why it is unknown or failed
--   measuredat    when it was measured (reported)
--   claimedat, claimedby
--                 the backfill's claim of it, while a run measures it, and
--                 which worker's
--   lastclaimedat when the backfill last handed it out, which its hourly
--                 rate counts
--   attempts, failures, nextattemptat
--                 the backfill's runs of it, those that failed (a claim
--                 whose run never reported back among them), and when it may
--                 run again after a failure
--
-- com_nalet_katalog_currentpictureareas holds the rows that are of what the
-- title plays now, and only those; a row of another file stays in the table,
-- and the title is measured again:
--   an item with a complete version: one of that version's package, or of an
--     original the version was made of (its sourceids);
--   an item without one: one of its original (the primary asset, by its path
--     or its source), or of the folder of the package it plays from before
--     the library (its packaged asset's, manifest.json);
--   an extra: one of its package (its packageid; before the library, a file
--     in its package's folder).
-- An episode another's file covers has no row of its own: it plays its
-- holder's file, and its holder's row is read. A deleted item's rows go with
-- it. Lowercase identifiers, like every com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_pictureareas (
  id VARCHAR(36) PRIMARY KEY,
  kind VARCHAR(8) NOT NULL CHECK (kind IN ('item', 'extra')),
  item_id VARCHAR(36) NOT NULL,
  state VARCHAR(10) NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'measured', 'unknown', 'failed')),
  framewidth INTEGER CHECK (framewidth > 0),
  frameheight INTEGER CHECK (frameheight > 0),
  areax INTEGER,
  areay INTEGER,
  areawidth INTEGER,
  areaheight INTEGER,
  path VARCHAR(2048),
  sourceid VARCHAR(36),
  versionid VARCHAR(36),
  packageid VARCHAR(36),
  method VARCHAR(32),
  samples INTEGER,
  informative INTEGER,
  durationms BIGINT,
  seconds REAL,
  cpuseconds REAL,
  evidence JSONB CHECK (evidence IS NULL OR jsonb_typeof(evidence) = 'object'),
  reason VARCHAR(500),
  measuredat TIMESTAMPTZ,
  claimedat TIMESTAMPTZ,
  claimedby VARCHAR(255),
  lastclaimedat TIMESTAMPTZ,
  attempts INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  nextattemptat TIMESTAMPTZ,
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(),
  modifiedat TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT pictureareas_area CHECK (state <> 'measured' OR (
    framewidth IS NOT NULL AND frameheight IS NOT NULL
    AND areax IS NOT NULL AND areay IS NOT NULL AND areawidth IS NOT NULL AND areaheight IS NOT NULL
    AND areax >= 0 AND areay >= 0 AND areawidth > 0 AND areaheight > 0
    AND areax + areawidth <= framewidth AND areay + areaheight <= frameheight)));
CREATE INDEX IF NOT EXISTS idx_pictureareas_item ON com_nalet_katalog_pictureareas (item_id);
CREATE INDEX IF NOT EXISTS idx_pictureareas_claimed ON com_nalet_katalog_pictureareas (claimedat)
  WHERE claimedat IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_pictureareas_lastclaimed ON com_nalet_katalog_pictureareas (lastclaimedat)
  WHERE lastclaimedat IS NOT NULL;
CREATE OR REPLACE VIEW com_nalet_katalog_currentpictureareas AS
SELECT pa.*
FROM com_nalet_katalog_pictureareas pa
WHERE (pa.kind = 'item' AND (
         EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
                 WHERE v.item_id = pa.id AND v.state = 'complete'
                   AND (v.id = pa.versionid OR pa.sourceid = ANY (v.sourceids)))
         OR (NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
                         WHERE v.item_id = pa.id AND v.state = 'complete')
             AND EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
                         WHERE a.item_id = pa.id
                           AND ((a.isprimary AND COALESCE(a.kind, 'primary') = 'primary'
                                 AND (a.path = pa.path OR a.sourceid = pa.sourceid))
                             OR (a.kind = 'packaged'
                                 AND starts_with(pa.path, regexp_replace(a.path, '/[^/]*$', '') || '/')))))))
   OR (pa.kind = 'extra' AND EXISTS (
         SELECT 1 FROM com_nalet_katalog_itemextras x
         WHERE x.id = pa.id AND x.removedat IS NULL
           AND ((x.packageid IS NOT NULL AND x.packageid = pa.packageid)
             OR (x.packageid IS NULL AND x.packagepath IS NOT NULL
                 AND starts_with(pa.path, rtrim(x.packagepath, '/') || '/')))));
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_pictureareas, com_nalet_katalog_currentpictureareas TO cloud_katalog_ro; END IF; END $$;
