-- 059_captions_retire_waiting.sql — which originals a retirement waits on for their closed captions alone (idempotent, after 058).
--
-- The retire job holds an original whose closed captions are not read yet
-- (058). Where that is the only thing that keeps it - its
-- title has a complete version, and every other rule of the deletion gate and
-- of the retire job lets it go - the job notes since when
-- (retirewaitingsince), and clears the note once anything else decides it.
-- The captions backfill hands these originals out first, then those of
-- titles with a complete version, then the rest; the note orders the
-- backfill and decides nothing.
-- Lowercase identifiers, like every com_nalet_katalog_* table.
ALTER TABLE com_nalet_katalog_sourcecaptions ADD COLUMN IF NOT EXISTS retirewaitingsince TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_sourcecaptions_retirewaiting ON com_nalet_katalog_sourcecaptions (source_id)
  WHERE retirewaitingsince IS NOT NULL;
