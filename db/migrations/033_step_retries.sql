-- 033_step_retries.sql — what the service keeps to retry a processing step (idempotent).
--
-- A step that fails is retried: the event that triggers its worker is sent
-- again, after a backoff that grows with every failure in a row, a bounded
-- number of times. A step in progress whose worker has been silent for longer
-- than the step's timeout is taken for a failed run, and so retried too. Per
-- step (com_nalet_katalog_itemprocessingsteps):
--   failures      its failed runs in a row: 1 after a first failure, one more
--                 for each failure after a retry or a new run, 0 again once
--                 it is done, skipped or not applicable, or an admin retries
--                 it. A run cut short by the step's timeout counts.
--   lasterror     the error of its last failed run (at most 500 characters,
--                 credentials redacted), kept while it is retried and after,
--                 until another failure replaces it.
--   nextretryat   when the service retries it by itself; NULL unless it is
--                 failed with an attempt left (an admin may still retry it).
--   dispatchedat  when the service last sent its trigger event again, while
--                 no worker has reported on it since; NULL once one has.
-- A step older than this migration has no failures and no retry scheduled:
-- the service retries what fails after it, and an admin what failed before.
-- skipped is terminal: nothing retries a skipped step.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table; the new
-- timestamps carry their time zone (timestamptz). Adding the columns rewrites
-- nothing (a constant default).
ALTER TABLE com_nalet_katalog_itemprocessingsteps
  ADD COLUMN IF NOT EXISTS failures     INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS lasterror    VARCHAR(500),
  ADD COLUMN IF NOT EXISTS nextretryat  TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS dispatchedat TIMESTAMPTZ;

-- The retries due, the failed steps, the steps in progress and the steps
-- sent again: what the retries, the reaper and the processing overview read.
CREATE INDEX IF NOT EXISTS idx_processingsteps_retry
  ON com_nalet_katalog_itemprocessingsteps (nextretryat)
  WHERE status = 'failed' AND nextretryat IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_processingsteps_failed
  ON com_nalet_katalog_itemprocessingsteps (step)
  WHERE status = 'failed';
CREATE INDEX IF NOT EXISTS idx_processingsteps_running
  ON com_nalet_katalog_itemprocessingsteps (modifiedat)
  WHERE status = 'in_progress';
CREATE INDEX IF NOT EXISTS idx_processingsteps_dispatched
  ON com_nalet_katalog_itemprocessingsteps (dispatchedat)
  WHERE status = 'pending' AND dispatchedat IS NOT NULL;
