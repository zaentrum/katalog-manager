-- 043_reencode_queue.sql — the titles queued to be encoded again (idempotent).
--
-- A migration's CI job, or an admin, queues titles to be packaged again under
-- the pipeline's current rules (POST /api/library/reencode); the retry sweep
-- sends them to the transcoder as reencodeItem sends one, at the pace the
-- settings library.reencode.rate (titles a pass), library.reencode.inflight
-- (titles sent and not done at once) and library.reencode.window (when) allow.
-- A title waits queued (a busy one, whose transcode or package runs, waits
-- on), is sent, then done once its package completes, or failed when its
-- transcode or its package fails with no attempt left. A title is in the
-- queue once while it waits or is sent (idx_reencodequeue_live); the rows of
-- a deleted item go with it.
--
-- com_nalet_katalog_reencodequeue
--   seq          the queue's order: the sweep sends the titles first queued
--                first, a request's in the order it named them
--   item_id      the movie or the episode
--   state        queued, sent, done or failed
--   selection    what queued it: items (named, a series its episodes), held
--                (its retire held for its surround) or all (every packaged
--                movie and episode)
--   enqueuedat, enqueuedby   when, and the caller (a token's subject)
--   sentat, doneat           when it was sent, and when it was done or failed
--   reason       why it waits (busy), or why it failed
CREATE TABLE IF NOT EXISTS com_nalet_katalog_reencodequeue (
  id VARCHAR(36) PRIMARY KEY,
  seq BIGINT GENERATED ALWAYS AS IDENTITY,
  item_id VARCHAR(36) NOT NULL,
  state VARCHAR(8) NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','sent','done','failed')),
  selection VARCHAR(8) NOT NULL DEFAULT 'items' CHECK (selection IN ('items','held','all')),
  enqueuedat TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  enqueuedby VARCHAR(255),
  sentat TIMESTAMPTZ,
  doneat TIMESTAMPTZ,
  reason VARCHAR(500));
CREATE UNIQUE INDEX IF NOT EXISTS idx_reencodequeue_live ON com_nalet_katalog_reencodequeue (item_id)
  WHERE state IN ('queued','sent');
CREATE INDEX IF NOT EXISTS idx_reencodequeue_state ON com_nalet_katalog_reencodequeue (state, seq);
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_reencodequeue TO cloud_katalog_ro; END IF; END $$;
