-- 042_playback_item_index.sql — a title's playback assets found by the title (idempotent).
--
-- Every reader of a title's files asks for its assets by its id: the worker
-- record, playback, katalog-api's /asset and /playback, the retire job. The
-- table had no index on item_id, so each of them read the whole table.
-- Lowercase identifiers, like every com_nalet_katalog_* table.
CREATE INDEX IF NOT EXISTS idx_playbackassets_item ON com_nalet_katalog_playbackassets (item_id);
