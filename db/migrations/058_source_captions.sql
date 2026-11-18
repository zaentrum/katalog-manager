-- 058_source_captions.sql — what a read of an original's frames found of its closed captions, window by window (idempotent).
--
-- A source record's closedCaptions comes from a probe that decodes no frame,
-- which says false of an original whose video carries A53 closed captions
-- (EIA/CEA-608/708 in a cc_data SEI with each picture): nothing in a
-- container tells, the decoder's side data does. An original that carries
-- closed captions, or whose captions are not known, is not retired. The
-- catalog's captions backfill (captions.backfill*) hands each original on
-- disk (present or superseded) to the analyzer, which reads it in windows
-- across it (its picture's start, and 10 to 90 % of its picture's span)
-- with ffprobe 7.1 and reports
-- what each window found (PUT /api/captions/sources/{sourceId});
-- katalog-manager reads no media. One row an original; an original without
-- a row is not read yet.
--   state            pending (handed out, no result yet), read
--                    (captionwindows holds what it found) or failed
--                    (reason; handed out again from nextattemptat)
--   captionwindows   the windows the read decoded, [{at, start, end, frames,
--                    captions, interlacedFrames, repeatFrames}]: where (a
--                    fraction of the picture's span), the presentation
--                    seconds covered, the frames decoded (0, a window that
--                    failed), whether they showed captions, and how many were
--                    coded as fields and how many repeat a field; a window
--                    that failed keeps what it found where it decoded frames
--   captions         whether any window showed them
--   ffprobeversion   the ffprobe that read it, as the analyzer names it
--   framesdecoded    how many video frames its windows decoded (a read that
--                    decoded none is a failed one)
--   duration         the video's length the read says (seconds; none when
--                    it read no length), which its record's is held to
--   captionstreams   how many streams of closed captions of their own the
--                    read found (an EIA-608 track, one tagged c608 or c708)
--   offlist          what of the file is off the analyzer's closed list
--   sourcesize, sourceqh1
--                    the size and quick hash of the file the read is of: a
--                    read of another file than the source is now is none
--   claimsize, claimqh1
--                    the same of the file the claim handed out
--   claimdurationms  the length its source record says (container.durationMs),
--                    handed out with the claim: a read of another length is
--                    none
--   claimspanstart, claimspanend
--                    the presentation span of its picture its records gave
--                    when it was handed out (seconds), which the read's
--                    windows are placed by; none where its record said no
--                    length
--   claimrecordedat, sourcerecordedat
--                    when the source record the claim handed out was written,
--                    and the one a finding is of
--   finding          what a read found that keeps the original for good of
--                    this file and this record: lengthMismatch (its length is
--                    not its record's), noRecordLength (its record said no
--                    length), noPlan (its records gave no plan of its
--                    picture), unscanned (its picture's captions need its
--                    packets' signature); none otherwise
--   failures         reports in a row with a failed read or window
--   claimedat, claimedby, claimtoken
--                    the claim that handed it out, while it is the reader's
--   lapsedtoken      the token of the claim whose lease ended last (counted
--                    a failure then: its report, late, counts no other)
--   lastclaimedat    when it was last handed out
--   attempts         how often it was handed out
-- com_nalet_katalog_captionhandouts keeps when each original was handed
-- out, for the last hour (the hourly rate counts hand-outs, not originals).
-- A catalog that applied an earlier shape of this file (before a release)
-- is brought to this one: its reads, of an earlier analyzer, are read again,
-- the columns it lacks are added, those it has no more are dropped, and its
-- checks are made again.
-- Lowercase identifiers, like every com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_sourcecaptions (
  source_id VARCHAR(36) PRIMARY KEY,
  item_id VARCHAR(36) NOT NULL,
  state VARCHAR(8) NOT NULL,
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(),
  modifiedat TIMESTAMPTZ NOT NULL DEFAULT now());
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema()
             AND table_name = 'com_nalet_katalog_sourcecaptions'
             AND column_name IN ('probe', 'packets', 'packageversionid')) THEN
    ALTER TABLE com_nalet_katalog_sourcecaptions
      DROP CONSTRAINT IF EXISTS com_nalet_katalog_sourcecaptions_state_check,
      DROP CONSTRAINT IF EXISTS sourcecaptions_read,
      DROP CONSTRAINT IF EXISTS sourcecaptions_package_read;
    UPDATE com_nalet_katalog_sourcecaptions SET state = 'pending', captions = NULL, readat = NULL
      WHERE state = 'read';
  END IF;
END $$;
ALTER TABLE com_nalet_katalog_sourcecaptions
  ADD COLUMN IF NOT EXISTS captionwindows JSONB,
  ADD COLUMN IF NOT EXISTS captions BOOLEAN,
  ADD COLUMN IF NOT EXISTS ffprobeversion VARCHAR(100),
  ADD COLUMN IF NOT EXISTS framesdecoded INTEGER,
  ADD COLUMN IF NOT EXISTS duration DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS captionstreams INTEGER,
  ADD COLUMN IF NOT EXISTS offlist JSONB,
  ADD COLUMN IF NOT EXISTS sourcesize BIGINT,
  ADD COLUMN IF NOT EXISTS sourceqh1 VARCHAR(100),
  ADD COLUMN IF NOT EXISTS claimsize BIGINT,
  ADD COLUMN IF NOT EXISTS claimqh1 VARCHAR(100),
  ADD COLUMN IF NOT EXISTS claimdurationms BIGINT,
  ADD COLUMN IF NOT EXISTS claimspanstart DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS claimspanend DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS claimrecordedat TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS sourcerecordedat TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS finding VARCHAR(32),
  ADD COLUMN IF NOT EXISTS reason VARCHAR(500),
  ADD COLUMN IF NOT EXISTS readat TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS failures INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS nextattemptat TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS claimedat TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS claimedby VARCHAR(255),
  ADD COLUMN IF NOT EXISTS claimtoken VARCHAR(36),
  ADD COLUMN IF NOT EXISTS lapsedtoken VARCHAR(36),
  ADD COLUMN IF NOT EXISTS lastclaimedat TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 0,
  DROP CONSTRAINT IF EXISTS com_nalet_katalog_sourcecaptions_state_check,
  DROP CONSTRAINT IF EXISTS com_nalet_katalog_sourcecaptions_packets_check,
  DROP CONSTRAINT IF EXISTS com_nalet_katalog_sourcecaptions_framesdecoded_check,
  DROP CONSTRAINT IF EXISTS com_nalet_katalog_sourcecaptions_packagestate_check,
  DROP CONSTRAINT IF EXISTS com_nalet_katalog_sourcecaptions_packageframesdecoded_check,
  DROP CONSTRAINT IF EXISTS sourcecaptions_read,
  DROP CONSTRAINT IF EXISTS sourcecaptions_package_read,
  DROP CONSTRAINT IF EXISTS sourcecaptions_state,
  DROP CONSTRAINT IF EXISTS sourcecaptions_frames,
  DROP CONSTRAINT IF EXISTS sourcecaptions_finding,
  DROP COLUMN IF EXISTS probe,
  DROP COLUMN IF EXISTS packets,
  DROP COLUMN IF EXISTS packageversionid,
  DROP COLUMN IF EXISTS packagestate,
  DROP COLUMN IF EXISTS packagecaptionwindows,
  DROP COLUMN IF EXISTS packagecaptions,
  DROP COLUMN IF EXISTS packageffprobeversion,
  DROP COLUMN IF EXISTS packageframesdecoded,
  DROP COLUMN IF EXISTS packagereason,
  DROP COLUMN IF EXISTS packagereadat;
UPDATE com_nalet_katalog_sourcecaptions SET state = 'pending', captions = NULL, readat = NULL
  WHERE state = 'read' AND (captionwindows IS NULL OR ffprobeversion IS NULL OR COALESCE(framesdecoded, 0) <= 0);
ALTER TABLE com_nalet_katalog_sourcecaptions
  ADD CONSTRAINT sourcecaptions_state CHECK (state IN ('pending', 'read', 'failed')),
  ADD CONSTRAINT sourcecaptions_frames CHECK (framesdecoded IS NULL OR framesdecoded >= 0),
  ADD CONSTRAINT sourcecaptions_finding CHECK (finding IS NULL OR finding IN ('lengthMismatch', 'noRecordLength', 'noPlan',
    'unscanned')),
  ADD CONSTRAINT sourcecaptions_read CHECK (state <> 'read'
    OR (captionwindows IS NOT NULL AND captions IS NOT NULL AND ffprobeversion IS NOT NULL AND framesdecoded > 0));
CREATE TABLE IF NOT EXISTS com_nalet_katalog_captionhandouts (
  source_id VARCHAR(36) NOT NULL,
  handedoutat TIMESTAMPTZ NOT NULL);
CREATE INDEX IF NOT EXISTS idx_captionhandouts_at ON com_nalet_katalog_captionhandouts (handedoutat);
CREATE INDEX IF NOT EXISTS idx_sourcecaptions_item ON com_nalet_katalog_sourcecaptions (item_id);
CREATE INDEX IF NOT EXISTS idx_sourcecaptions_claimed ON com_nalet_katalog_sourcecaptions (claimedat)
  WHERE claimedat IS NOT NULL;
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_sourcecaptions TO cloud_katalog_ro; END IF; END $$;
