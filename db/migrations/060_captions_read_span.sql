-- 060_captions_read_span.sql — what a read of an original's frames says of the span of its picture, and the hold of a picture whose captions windows alone don't tell (idempotent, after 058).
--
-- A read is held to the presentation span of the original's picture its
-- records give, within a frame step and half a second: the claim notes that
-- step (claimspanstep, seconds), and a read kept notes the span it read
-- (readspanstart and readspanend, seconds of the picture's clock), each end
-- of which the deletion gate holds to the records' again, and the
-- longest run between two keyframes of its picture's first packets it
-- found (readgop, seconds), by which its windows lie at their places. A read
-- that says the picture's captions can't be told by windows (an HEVC
-- picture whose packets weren't read whole for a caption signature) is kept
-- as the finding unscanned: final for that file and that record, handed out
-- again once the record is written again (with its caption signature).
-- Lowercase identifiers, like every com_nalet_katalog_* table.
ALTER TABLE com_nalet_katalog_sourcecaptions
  ADD COLUMN IF NOT EXISTS claimspanstep DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS readspanstart DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS readspanend DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS readgop DOUBLE PRECISION,
  DROP CONSTRAINT IF EXISTS sourcecaptions_finding;
ALTER TABLE com_nalet_katalog_sourcecaptions
  ADD CONSTRAINT sourcecaptions_finding CHECK (finding IS NULL
    OR finding IN ('lengthMismatch', 'noRecordLength', 'noPlan', 'unscanned'));
CREATE INDEX IF NOT EXISTS idx_sourcecaptions_unscanned ON com_nalet_katalog_sourcecaptions (source_id)
  WHERE finding = 'unscanned';
