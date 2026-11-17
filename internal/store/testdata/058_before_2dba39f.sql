-- 058_source_captions.sql — what a probe that decoded an original's first frames found of its closed captions (idempotent).
--
-- A source record's closedCaptions comes from a probe that decodes no frame,
-- which says false of an original whose video carries A53 closed captions
-- (EIA/CEA-608/708 in a cc_data SEI with each picture): nothing in a
-- container tells, the decoder's side data does. The catalog's captions
-- record (library.captions.record) reads each original on disk (present or
-- superseded) once, decoding its first 48 video packets (ffprobe
-- -read_intervals %+#48 -show_frames: "ATSC A53 Part 4 Closed Captions" side
-- data, or a stream's closed_captions), and keeps what it found here, one row
-- an original; an original without a row is not read yet. The deletion gate
-- keeps an original whose package was encoded while its captions are not
-- read; a copied package is the original's own stream, SEI included.
--   state          read (captions holds what the probe found) or failed
--                  (reason; read again from nextattemptat)
--   captions       whether its video carries closed captions
--   probe          what read it (ffprobe and its version)
--   packets        how many video packets it decoded
--   failures       reads in a row that failed
-- Lowercase identifiers, like every
-- com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_sourcecaptions (
  source_id VARCHAR(36) PRIMARY KEY,
  item_id VARCHAR(36) NOT NULL,
  state VARCHAR(8) NOT NULL CHECK (state IN ('read', 'failed')),
  captions BOOLEAN,
  probe VARCHAR(200),
  packets INTEGER CHECK (packets IS NULL OR packets >= 0),
  reason VARCHAR(500),
  failures INTEGER NOT NULL DEFAULT 0,
  nextattemptat TIMESTAMPTZ,
  readat TIMESTAMPTZ,
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(),
  modifiedat TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT sourcecaptions_read CHECK (state <> 'read' OR captions IS NOT NULL));
CREATE INDEX IF NOT EXISTS idx_sourcecaptions_item ON com_nalet_katalog_sourcecaptions (item_id);
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_sourcecaptions TO cloud_katalog_ro; END IF; END $$;
