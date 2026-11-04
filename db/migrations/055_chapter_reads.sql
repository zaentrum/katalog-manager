-- 055_chapter_reads.sql — what reading each title's chapters from its original found (idempotent).
--
-- A title's chapters live in the catalog (com_nalet_katalog_itemchapters),
-- not in its package: the analyzer reads them from the header of the
-- original (ffprobe's chapters, nothing decoded) and writes them. Before this
-- migration nothing said that a read had happened when it found none, or that
-- it could not read the file: a title whose original has chapters (its source
-- record's essence.chapters) and no chapter rows could be one never read, one
-- whose read failed, or one whose file holds no chapter with a length, and a
-- deletion wave that keeps an original until its chapters are in the catalog
-- kept all three for good. Here the catalog keeps the last read of each
-- title's original: what it found, what it wrote, or why it failed; the
-- backfill (the settings chapters.backfill*) reads the titles again that
-- still miss theirs.
--
-- com_nalet_katalog_chapterreads, a movie or an episode a row:
--   item_id       the title
--   state         read (chapters found and written), none (the header holds
--                 no chapter with a length; found says how many marks it
--                 held), failed (the last read failed, reason says why; read
--                 again from nextattemptat), unreadable (it failed so many
--                 times in a row that it is settled: not read again while the
--                 original is the same), or pending (the backfill handed it
--                 out, nothing reported yet)
--   sourceid      the original read (migration 040's source)
--   path, filesize
--                 the file read, and its size in bytes
--   found, written
--                 the chapter marks the header held, and the chapters
--                 written of them (marks without a length are left out)
--   probe         what read it (the analyzer's ffprobe, its version)
--   seconds       how long the read took
--   reason        why it failed, or what was left out of a read
--   readat        when the last read was reported
--   claimedat, claimedby, claimtoken
--                 the backfill's claim of it, while a run reads it: when,
--                 which worker's, and the token its report names (a report of
--                 another claim's leaves this one as it is)
--   lastclaimedat when the backfill last handed it out, which its hourly rate
--                 counts
--   attempts, failures, nextattemptat
--                 the backfill's runs of it, the failed reads in a row (a
--                 claim whose run never reported back among them), and when
--                 it may be read again after a failure
--   lapsedtoken   the token of its last claim whose lease ended, counted a
--                 failure then: that run's own failure, reported late, is not
--                 counted again
--
-- com_nalet_katalog_chaptersmissing holds the titles whose original is still
-- needed for their chapters, a present original a row: its record says it has
-- chapters (the compliance check's srcEssence, of that source), the catalog
-- has no chapter of the title, and no read of that original settled it (none
-- or unreadable). A deletion wave keeps such an original; the backfill reads
-- these titles. The view needs 040 and 051. A deleted item's row goes with it.
-- Lowercase identifiers, like every com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_chapterreads (
  item_id VARCHAR(36) PRIMARY KEY,
  state VARCHAR(10) NOT NULL DEFAULT 'pending'
    CHECK (state IN ('pending', 'read', 'none', 'failed', 'unreadable')),
  sourceid VARCHAR(36),
  path VARCHAR(2048),
  filesize BIGINT,
  found INTEGER CHECK (found IS NULL OR found >= 0),
  written INTEGER CHECK (written IS NULL OR written >= 0),
  probe VARCHAR(200),
  seconds REAL,
  reason VARCHAR(500),
  readat TIMESTAMPTZ,
  claimedat TIMESTAMPTZ,
  claimedby VARCHAR(255),
  claimtoken VARCHAR(36),
  lastclaimedat TIMESTAMPTZ,
  attempts INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  nextattemptat TIMESTAMPTZ,
  lapsedtoken VARCHAR(36),
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(),
  modifiedat TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS idx_chapterreads_claimed ON com_nalet_katalog_chapterreads (claimedat)
  WHERE claimedat IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_chapterreads_lastclaimed ON com_nalet_katalog_chapterreads (lastclaimedat)
  WHERE lastclaimedat IS NOT NULL;
CREATE OR REPLACE VIEW com_nalet_katalog_chaptersmissing AS
SELECT c.item_id, s.id AS sourceid, r.state, r.failures, r.nextattemptat, r.reason, r.readat
FROM com_nalet_katalog_compliance c
JOIN com_nalet_katalog_itemsources s ON s.item_id = c.item_id AND s.state = 'present'
LEFT JOIN com_nalet_katalog_chapterreads r ON r.item_id = c.item_id
WHERE jsonb_typeof(c.records->'srcEssence'->'chapters') = 'boolean'
  AND (c.records->'srcEssence'->>'chapters')::boolean
  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemchapters h WHERE h.item_id = c.item_id)
  AND NOT COALESCE(r.state IN ('none', 'unreadable') AND r.sourceid = s.id, false);
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_chapterreads, com_nalet_katalog_chaptersmissing TO cloud_katalog_ro; END IF; END $$;
