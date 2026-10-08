-- 045_multi_episode_files.sql — one file of several episodes, and what a scan passed over (idempotent).
--
-- A file may hold more than one episode (a double-length finale, named
-- S05E15-E16, S05E15E16 or S05E15-16), and it is never split: it belongs to
-- the first episode it covers, its holder, as any episode's file belongs to
-- its episode. Its source, its versions, its package, its pipeline run and
-- its retire are the holder's. Every other episode it covers keeps an item of
-- its own (its title, its overview, its images), has no file of its own and
-- runs nothing; it plays its holder's version (com_nalet_katalog_items):
--   coveredby  the holder's item id, on a covered episode alone; null on every
--              other item. The holder covers its own id and every item that
--              names it, in episode order, as its source record's covers
--              lists them.
-- idx_items_coveredby finds the episodes a holder's file covers.
--
-- A scan passes over what the library takes no file of (a disc image, an .iso
-- or .img: it is converted to a single file first, or stays out), and leaves
-- an episode a file's name covers alone when it has a file of its own (its
-- own file wins). Its job says so (com_nalet_katalog_scanjobs):
--   report  [{"kind", "path", "itemId", "reason"}], kind unsupported (a file
--           taken in as nothing) or not-linked (an episode left alone); null
--           for a scan that passed over nothing, and for a job older than
--           this migration.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table. Adding the
-- columns rewrites nothing (no default); the index holds the covered
-- episodes alone.
ALTER TABLE com_nalet_katalog_items ADD COLUMN IF NOT EXISTS coveredby VARCHAR(36);
CREATE INDEX IF NOT EXISTS idx_items_coveredby ON com_nalet_katalog_items (coveredby) WHERE coveredby IS NOT NULL;
ALTER TABLE com_nalet_katalog_scanjobs ADD COLUMN IF NOT EXISTS report JSONB;
