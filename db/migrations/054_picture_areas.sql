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
--   measuredon    original or package: what the analyzer read
--   filesize      the size of the original measured, in bytes
--   method        the measurement's version, as the analyzer names it
--   samples, informative
--                 the frames it decoded, and those that showed a picture
--   durationms, seconds, cpuseconds, bytesread
--                 the length of the video measured, and what measuring it
--                 took: its time, its CPU and the bytes it read
--   evidence      per side, the bar it found and why (the analyzer's JSON)
--   sampling      how it sampled the video (the analyzer's JSON: how, its
--                 samples' largest gap, its budget), what the video is (its
--                 codec, its pixel aspect) and the decoder's version
--   reason        why it is unknown or failed
--   measuredat    when it was measured (reported)
--   claimedat, claimedby, claimtoken
--                 the backfill's claim of it, while a run measures it: when,
--                 which worker's, and the token its run reports with (a
--                 report of another claim's leaves this one as it is)
--   lastclaimedat when the backfill last handed it out, which its hourly
--                 rate counts
--   attempts, failures, nextattemptat
--                 the backfill's runs of it, those that failed (a claim
--                 whose run never reported back among them), and when it may
--                 run again after a failure or a release
--   lapsedtoken   the token of its last claim whose lease ended, counted a
--                 failure then: that run's own failure, reported late, is
--                 not counted again
--   releases      the runs in a row that gave it back unmeasured (their
--                 deadline came first); each waits longer before the next
--
-- com_nalet_katalog_currentpictureareas holds the rows that are of what the
-- title plays now, and only those; a row of another file stays in the table,
-- and the title is measured again:
--   an item with a complete version: one of that version's package, or of an
--     original the version was made of (its sourceids) whose picture has the
--     shape of the version's package: the frame's display aspect (its size
--     and the pixel aspect the measurement read, sampling.video.sar; square
--     without one) and the packaged asset's resolution within 1 %, so an
--     anamorphic original's area holds for its package of square pixels,
--     and one of another shape (a crop) is measured again;
--   an item without one: one of its original (the primary asset: of its
--     source where both name one, else of its path), of the size the asset
--     has where both say one, or of the folder of the package it plays from
--     before the library (its packaged asset's, manifest.json);
--   an extra: one of its package (its packageid; before the library, a file
--     in its package's folder, measured since it was packaged).
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
  measuredon VARCHAR(8) CHECK (measuredon IS NULL OR measuredon IN ('original', 'package')),
  filesize BIGINT,
  method VARCHAR(32),
  samples INTEGER,
  informative INTEGER,
  durationms BIGINT,
  seconds REAL,
  cpuseconds REAL,
  bytesread BIGINT,
  evidence JSONB CHECK (evidence IS NULL OR jsonb_typeof(evidence) = 'object'),
  sampling JSONB CHECK (sampling IS NULL OR jsonb_typeof(sampling) = 'object'),
  reason VARCHAR(500),
  measuredat TIMESTAMPTZ,
  claimedat TIMESTAMPTZ,
  claimedby VARCHAR(255),
  claimtoken VARCHAR(36),
  lastclaimedat TIMESTAMPTZ,
  attempts INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  nextattemptat TIMESTAMPTZ,
  lapsedtoken VARCHAR(36),
  releases INTEGER NOT NULL DEFAULT 0,
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
                   AND (v.id = pa.versionid
                     OR (pa.sourceid = ANY (v.sourceids) AND EXISTS (
                           SELECT 1 FROM com_nalet_katalog_playbackassets k
                           WHERE k.item_id = pa.id AND k.kind = 'packaged' AND k.versionid = v.id
                             AND k.resolution ~ '^[1-9][0-9]*x[1-9][0-9]*$'
                             AND abs(split_part(k.resolution, 'x', 1)::numeric * pa.frameheight
                                     * CASE WHEN pa.sampling->'video'->>'sar' ~ '^[1-9][0-9]*:[1-9][0-9]*$'
                                            THEN split_part(pa.sampling->'video'->>'sar', ':', 2)::numeric
                                                 / split_part(pa.sampling->'video'->>'sar', ':', 1)::numeric
                                            ELSE 1 END
                                     / (split_part(k.resolution, 'x', 2)::numeric * pa.framewidth) - 1) <= 0.01))))
         OR (NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
                         WHERE v.item_id = pa.id AND v.state = 'complete')
             AND EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
                         WHERE a.item_id = pa.id
                           AND ((a.isprimary AND COALESCE(a.kind, 'primary') = 'primary'
                                 AND CASE WHEN a.sourceid IS NOT NULL AND pa.sourceid IS NOT NULL
                                          THEN a.sourceid = pa.sourceid ELSE a.path = pa.path END
                                 AND (a.sizebytes IS NULL OR pa.filesize IS NULL OR a.sizebytes = pa.filesize))
                             OR (a.kind = 'packaged'
                                 AND starts_with(pa.path, regexp_replace(a.path, '/[^/]*$', '') || '/')))))))
   OR (pa.kind = 'extra' AND EXISTS (
         SELECT 1 FROM com_nalet_katalog_itemextras x
         WHERE x.id = pa.id AND x.removedat IS NULL
           AND ((x.packageid IS NOT NULL AND x.packageid = pa.packageid)
             OR (x.packageid IS NULL AND x.packagepath IS NOT NULL
                 AND starts_with(pa.path, rtrim(x.packagepath, '/') || '/')
                 AND (x.packagedat IS NULL OR x.packagedat <= COALESCE(pa.measuredat, pa.modifiedat))))));
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_pictureareas, com_nalet_katalog_currentpictureareas TO cloud_katalog_ro; END IF; END $$;
