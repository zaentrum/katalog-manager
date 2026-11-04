-- 055_chapter_reads.sql — what reading each title's chapters found, and the titles still to read (idempotent).
--
-- A title's chapters live in the catalog (com_nalet_katalog_itemchapters),
-- not in its package: they are read from its original, and a deletion wave
-- keeps an original until they are in the catalog. Two readers read them:
-- the analyzer, the original's header (ffprobe's chapters, nothing
-- decoded), and the catalog itself, the original's source record
-- (sources/<id>/ffprobe.json, ffprobe's whole probe of the file as the
-- library recorded it, its chapters among it, kept after the original is
-- retired). Before this migration nothing said that a read happened, of
-- which original, or what it found: a title without chapter rows could be
-- one never read, one whose read failed, or one whose file holds none, and
-- chapter rows read of an earlier original stood for the one there now.
--
-- com_nalet_katalog_chapterreads, a movie or an episode a row, its last
-- read:
--   item_id       the title
--   state         read (chapters found and written), none (the original
--                 holds no chapter with a length; found says how many marks
--                 it held), failed (the last read failed, reason says why;
--                 read again from nextattemptat), unreadable (its header read
--                 failed so many times that it is no use reading it again; it
--                 settles nothing until an admin accepts it), or pending
--                 (handed out, nothing reported yet)
--   readfrom      header (the analyzer's read of the original) or record
--                 (the catalog's read of its source record)
--   confirmed     a none that its source record agrees with (no chapter with
--                 a length in its probe, or none at all): only a confirmed
--                 none settles a title
--   sourceid      the original read (migration 040's source); a read settles
--                 only the title's original of that source
--   path, filesize the file read and its size in bytes
--   found, written the chapter marks found, and the chapters written of them
--   probe         what read it (the analyzer's ffprobe and its version, or the
--                 record's file)
--   seconds       how long the read took
--   reason        why it failed, or what was left out of a read
--   readat        when the last read was reported
--   recordsourceid, recordat, recordnote
--                 the source whose record the catalog read last, when, and
--                 why that record could not settle the title (the analyzer
--                 reads its header then)
--   claimedat, claimedby, claimtoken
--                 the backfill's claim of it, while a run reads it: when,
--                 which worker's, and the token its report names
--   lastclaimedat when the backfill last handed it out
--   lapsedtoken, lapsedclaimedat
--                 its last claim whose lease ended: its token and when it was
--                 handed out, so that its run's report, late, is told for one
--   attempts      the backfill's runs of it
--   failures      header reads in a row that ran on the original and could
--                 not read its chapters, each counted once a backoff, which
--                 make it unreadable at the fifth
--   transients    reads in a row the storage or the worker failed (no file
--                 to read, no answer in time, a lease that ended, an error
--                 of the storage), which count for nothing but the backoff
--   nextattemptat when it may be handed out again
--   acceptedat, acceptedby
--                 an admin's decision that an unreadable title is settled:
--                 its original goes without its chapters
--
-- com_nalet_katalog_chapterclaims holds a row for every title the backfill
-- handed out, for its hourly rate; rows older than two days go.
--
-- com_nalet_katalog_chaptersmissing holds the titles whose original is still
-- needed for their chapters: each movie and episode with an original present
-- on disk, of its current original (its primary asset's source while that
-- is present, else its newest present one), unless that original's record
-- says it holds no chapter at all (the compliance check's srcEssence, of a
-- record of that source: a missing record, one that is no boolean or one of
-- another source settles nothing), or a read of that original settled it:
-- one that wrote chapters that are in the catalog, a confirmed none, an
-- unreadable an admin accepted. A deletion wave keeps the original of a
-- title in it; the backfill reads them. The view needs 040 and 051. A
-- deleted item's rows go with it. Lowercase identifiers, like every
-- com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_chapterreads (
  item_id VARCHAR(36) PRIMARY KEY,
  state VARCHAR(10) NOT NULL DEFAULT 'pending'
    CHECK (state IN ('pending', 'read', 'none', 'failed', 'unreadable')),
  readfrom VARCHAR(6) CHECK (readfrom IS NULL OR readfrom IN ('header', 'record')),
  confirmed BOOLEAN NOT NULL DEFAULT false,
  sourceid VARCHAR(36),
  path VARCHAR(2048),
  filesize BIGINT,
  found INTEGER CHECK (found IS NULL OR found >= 0),
  written INTEGER CHECK (written IS NULL OR written >= 0),
  probe VARCHAR(200),
  seconds REAL,
  reason VARCHAR(500),
  readat TIMESTAMPTZ,
  recordsourceid VARCHAR(36),
  recordat TIMESTAMPTZ,
  recordnote VARCHAR(500),
  claimedat TIMESTAMPTZ,
  claimedby VARCHAR(255),
  claimtoken VARCHAR(36),
  lastclaimedat TIMESTAMPTZ,
  lapsedtoken VARCHAR(36),
  lapsedclaimedat TIMESTAMPTZ,
  attempts INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  transients INTEGER NOT NULL DEFAULT 0,
  nextattemptat TIMESTAMPTZ,
  acceptedat TIMESTAMPTZ,
  acceptedby VARCHAR(255),
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(),
  modifiedat TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS idx_chapterreads_claimed ON com_nalet_katalog_chapterreads (claimedat)
  WHERE claimedat IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_chapterreads_recordat ON com_nalet_katalog_chapterreads (recordat)
  WHERE recordat IS NOT NULL;
CREATE TABLE IF NOT EXISTS com_nalet_katalog_chapterclaims (
  id BIGSERIAL PRIMARY KEY,
  item_id VARCHAR(36) NOT NULL,
  claimedat TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS idx_chapterclaims_claimedat ON com_nalet_katalog_chapterclaims (claimedat);
CREATE OR REPLACE VIEW com_nalet_katalog_chaptersmissing AS
SELECT i.id AS item_id, cur.id AS sourceid, r.state, r.failures, r.nextattemptat, r.reason, r.readat
FROM com_nalet_katalog_items i
JOIN LATERAL (
  SELECT s.id FROM com_nalet_katalog_itemsources s
  WHERE s.item_id = i.id AND s.state = 'present'
  ORDER BY s.id IS NOT DISTINCT FROM (
             SELECT a.sourceid FROM com_nalet_katalog_playbackassets a
             WHERE a.item_id = i.id AND a.isprimary AND COALESCE(a.kind, 'primary') = 'primary'
               AND a.sourceid IS NOT NULL
             ORDER BY a.id LIMIT 1) DESC,
           s.createdat DESC, s.id
  LIMIT 1) cur ON true
LEFT JOIN com_nalet_katalog_compliance c ON c.item_id = i.id
LEFT JOIN com_nalet_katalog_chapterreads r ON r.item_id = i.id
WHERE i.type IN ('movie', 'episode')
  AND NOT COALESCE(substring(c.recordkey FROM 's:([0-9a-f-]{36})@') = cur.id
                   AND jsonb_typeof(c.records->'srcEssence'->'chapters') = 'boolean'
                   AND NOT (c.records->'srcEssence'->>'chapters')::boolean, false)
  AND NOT COALESCE(r.sourceid = cur.id AND (
        (r.state = 'read' AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemchapters h WHERE h.item_id = i.id))
     OR (r.state = 'none' AND r.confirmed)
     OR (r.state = 'unreadable' AND r.acceptedat IS NOT NULL)), false);
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_chapterreads, com_nalet_katalog_chapterclaims, com_nalet_katalog_chaptersmissing
    TO cloud_katalog_ro; END IF; END $$;
