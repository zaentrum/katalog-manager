-- 057_superseded_decisions.sql — what holds an original another file took the place of for an admin, and what an admin decides of it (idempotent).
--
-- The retire job retires a superseded original (056) only when what deleting
-- it gives up against its title's master — the deletion gate of the master's
-- package for the original's record — lies within library.superseded.accept.
-- One that gives up more waits for an admin, and an admin decides it:
--   supersededhold       why it waits: what deleting it gives up, and what of
--                        that is not accepted
--   supersededholdfor    the master it waits against: once another version of
--                        the title is its master, it is looked at again
--   supersededlosses     what deleting it gives up against that master (the
--                        gate), null when no record tells
--   supersededdecision   accept (retired, giving up what supersededaccepted
--                        names, or anything when no record tells) or keep
--                        (never retired by the job)
--   supersededaccepted   what the admin accepted it gives up
--   supersededdecidedby, supersededdecidedat
-- Lowercase identifiers, like every com_nalet_katalog_* table.
ALTER TABLE com_nalet_katalog_itemsources
  ADD COLUMN IF NOT EXISTS supersededhold VARCHAR(1000),
  ADD COLUMN IF NOT EXISTS supersededholdfor VARCHAR(36),
  ADD COLUMN IF NOT EXISTS supersededlosses JSONB,
  ADD COLUMN IF NOT EXISTS supersededdecision VARCHAR(8),
  ADD COLUMN IF NOT EXISTS supersededaccepted JSONB,
  ADD COLUMN IF NOT EXISTS supersededdecidedby VARCHAR(255),
  ADD COLUMN IF NOT EXISTS supersededdecidedat TIMESTAMPTZ;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
      WHERE conrelid = 'com_nalet_katalog_itemsources'::regclass AND conname = 'com_nalet_katalog_itemsources_supersededdecision_check') THEN
    ALTER TABLE com_nalet_katalog_itemsources ADD CONSTRAINT com_nalet_katalog_itemsources_supersededdecision_check
      CHECK (supersededdecision IS NULL OR supersededdecision IN ('accept', 'keep'));
  END IF;
END $$;
CREATE INDEX IF NOT EXISTS idx_itemsources_supersededhold ON com_nalet_katalog_itemsources (supersededat, id)
  WHERE state = 'superseded' AND supersededhold IS NOT NULL;
