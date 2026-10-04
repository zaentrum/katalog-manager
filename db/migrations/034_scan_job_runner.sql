-- 034_scan_job_runner.sql — who runs a scan job, and its last word (idempotent).
--
-- A scan runs in the process of the service that started it, which writes the
-- scan's end into its job when it ends. A process that stops while it scans (a
-- restart, a crash) never does, so the job said running for ever. Per scan job
-- (com_nalet_katalog_scanjobs):
--   runner       the process that runs it, "<host>/<boot>": the host it runs
--                on (a pod's name) and a tag the process drew when it
--                started. At startup the service fails, interrupted, every job
--                still running that a process of its host with another tag
--                ran, and every one that names no runner: their processes are
--                gone. A job another host runs is left to the reaper.
--   heartbeatat  its scanner's last word: when it started and, while it walks
--                the library, every 30 seconds at most. A job running without
--                a word for longer than the scan's timeout (the scan step's,
--                15 minutes by default) is failed, timed out, by the reaper.
-- A job older than this migration names no runner and has said nothing since
-- it started.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table; the new
-- timestamp carries its time zone (timestamptz). Adding the columns rewrites
-- nothing.
ALTER TABLE com_nalet_katalog_scanjobs
  ADD COLUMN IF NOT EXISTS runner      VARCHAR(255),
  ADD COLUMN IF NOT EXISTS heartbeatat TIMESTAMPTZ;
