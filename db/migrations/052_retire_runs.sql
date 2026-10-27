-- 052_retire_runs.sql — an admin's list of titles whose originals are retired, and how each fared (idempotent).
--
-- library.originals keeps every original unless it is delete-after-package.
-- An admin who wants exactly the originals of some titles gone, and no
-- policy, names them in a list (GraphQL retireOriginals): a run checks each
-- title, and retires the original of each that holds every condition through
-- the retire job's own path (its event, the trash and its grace, the source's
-- row, what the package lacks of it), one at a time; a dry run, the default,
-- only says what it would retire. One run runs at a time; a run a restart
-- interrupted goes on where it stopped. A run and its titles are kept once it
-- is done, a record of what was retired, on whose word.
--
-- com_nalet_katalog_retireruns, a run a row:
--   state        running, done, or failed (it could not go on; error says why)
--   dryrun       true: nothing is retired, each title says whether it would be
--   requestedby  who asked for it (the admin's subject)
--   startedat, finishedat
--   error        why a failed run stopped, or null
-- com_nalet_katalog_retirerunitems, a title of a run's list a row, in the
--   list's order (position, from 1):
--   item_id      the title as listed (no foreign key: a title deleted since
--                keeps its row of the run)
--   title        its name as a list names it, when the run began
--   source_id    the original the run retires or retired of it, once known
--   result       retired, skipped, failed or would-retire; null while it waits
--   reason       why it was skipped or failed, or what a dry run found
--   bytes        the size of its original
--   phase        retiring once the run began to retire its original: a run
--                that resumes goes on with that retirement
--   at           when it got its result
--
-- idx_retireruns_running holds one running run at most. Lowercase
-- identifiers, like every com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_retireruns (
  id VARCHAR(36) PRIMARY KEY,
  state VARCHAR(8) NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'done', 'failed')),
  dryrun BOOLEAN NOT NULL,
  requestedby VARCHAR(255) NOT NULL,
  startedat TIMESTAMPTZ NOT NULL DEFAULT now(),
  finishedat TIMESTAMPTZ,
  error VARCHAR(500));
CREATE UNIQUE INDEX IF NOT EXISTS idx_retireruns_running ON com_nalet_katalog_retireruns (state) WHERE state = 'running';
CREATE TABLE IF NOT EXISTS com_nalet_katalog_retirerunitems (
  run_id VARCHAR(36) NOT NULL REFERENCES com_nalet_katalog_retireruns (id) ON DELETE CASCADE,
  position INTEGER NOT NULL CHECK (position >= 1),
  item_id VARCHAR(36) NOT NULL,
  title VARCHAR(500),
  source_id VARCHAR(36),
  result VARCHAR(16) CHECK (result IN ('retired', 'skipped', 'failed', 'would-retire')),
  reason VARCHAR(1000),
  bytes BIGINT,
  phase VARCHAR(16) CHECK (phase IN ('retiring')),
  at TIMESTAMPTZ,
  PRIMARY KEY (run_id, position));
