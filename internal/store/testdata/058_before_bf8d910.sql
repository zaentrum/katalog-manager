-- 058_source_captions.sql — what reads that decoded frames found of the closed captions of an original and of the package it is gated against, window by window (idempotent).
--
-- A source record's closedCaptions comes from a probe that decodes no frame,
-- which says false of an original whose video carries A53 closed captions
-- (EIA/CEA-608/708 in a cc_data SEI with each picture): nothing in a
-- container tells, the decoder's side data does. A package's record says it
-- carries them only since the packager reads them (its top video
-- rendition's closedCaptions: true), and names its encoder only since it
-- does too, so an older package's record can't tell. The catalog's captions
-- backfill (captions.backfill*) hands each original on disk (present or
-- superseded) to the analyzer with the package of its title's complete
-- version; the analyzer reads each in windows across it, decoding video
-- frames with ffprobe 7.1 or later from a keyframe, and reports what each
-- window found (PUT /api/captions/sources/{sourceId}); katalog-manager reads
-- no media. One row an original; an original without a row is not read yet.
-- The deletion gate compares the windows: every window of the original that
-- shows captions needs a window of the package at the same presentation time
-- that shows them, and what can't be told keeps the original.
--   state            the original's read: pending (handed out, no result
--                    yet), read (captions holds what it found) or failed
--                    (reason; handed out again from nextattemptat)
--   captionwindows   the windows the read decoded, [{at, start, end, frames,
--                    captions}]: where (a fraction of the video), the
--                    presentation seconds covered, the frames decoded (0, a
--                    window that failed) and whether they showed captions
--   captions         whether any window showed them
--   ffprobeversion   the ffprobe that read it, as the analyzer names it
--   framesdecoded    how many video frames its windows decoded (a read that
--                    decoded none is a failed one)
--   package*         the same of the package of the version packageversionid
--                    (packagestate read or failed): its top video rendition
--   failures         reports in a row with a failed read
--   claimedat, claimedby, claimtoken
--                    the claim that handed it out, while it is the reader's
--   lapsedtoken      the token of the claim whose lease ended last (counted
--                    a failure then: its report, late, counts no other)
--   lastclaimedat    when it was last handed out (the hourly rate)
--   attempts         how often it was handed out
-- Lowercase identifiers, like every com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_sourcecaptions (
  source_id VARCHAR(36) PRIMARY KEY,
  item_id VARCHAR(36) NOT NULL,
  state VARCHAR(8) NOT NULL CHECK (state IN ('pending', 'read', 'failed')),
  captionwindows JSONB,
  captions BOOLEAN,
  ffprobeversion VARCHAR(100),
  framesdecoded INTEGER CHECK (framesdecoded IS NULL OR framesdecoded >= 0),
  reason VARCHAR(500),
  readat TIMESTAMPTZ,
  packageversionid VARCHAR(36),
  packagestate VARCHAR(8) CHECK (packagestate IS NULL OR packagestate IN ('read', 'failed')),
  packagecaptionwindows JSONB,
  packagecaptions BOOLEAN,
  packageffprobeversion VARCHAR(100),
  packageframesdecoded INTEGER CHECK (packageframesdecoded IS NULL OR packageframesdecoded >= 0),
  packagereason VARCHAR(500),
  packagereadat TIMESTAMPTZ,
  failures INTEGER NOT NULL DEFAULT 0,
  nextattemptat TIMESTAMPTZ,
  claimedat TIMESTAMPTZ,
  claimedby VARCHAR(255),
  claimtoken VARCHAR(36),
  lapsedtoken VARCHAR(36),
  lastclaimedat TIMESTAMPTZ,
  attempts INTEGER NOT NULL DEFAULT 0,
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(),
  modifiedat TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT sourcecaptions_read CHECK (state <> 'read'
    OR (captionwindows IS NOT NULL AND captions IS NOT NULL AND ffprobeversion IS NOT NULL AND framesdecoded > 0)),
  CONSTRAINT sourcecaptions_package_read CHECK (packagestate IS DISTINCT FROM 'read'
    OR (packageversionid IS NOT NULL AND packagecaptionwindows IS NOT NULL AND packagecaptions IS NOT NULL
        AND packageffprobeversion IS NOT NULL
        AND packageframesdecoded > 0)));
CREATE INDEX IF NOT EXISTS idx_sourcecaptions_item ON com_nalet_katalog_sourcecaptions (item_id);
CREATE INDEX IF NOT EXISTS idx_sourcecaptions_claimed ON com_nalet_katalog_sourcecaptions (claimedat)
  WHERE claimedat IS NOT NULL;
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_sourcecaptions TO cloud_katalog_ro; END IF; END $$;
