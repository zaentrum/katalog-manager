-- 047_identity_indexes.sql — a title found by its identity, an item's ids and its children found by the item (idempotent).
--
-- A title is found by an id of a reference database (ADR-0012): POST
-- /api/ingest finds the series of an episode and a movie by one, and
-- katalog-api's holdings answer what the library holds of one, season by
-- season. Each of those reads went through the whole of a table:
--   idx_itemexternalids_identity  the items that hold a source's id
--                                 (com_nalet_katalog_itemexternalids: source,
--                                 externalid);
--   idx_itemexternalids_item      the ids an item holds (item_id), as every
--                                 reader of a title's ids asks for them;
--   idx_items_parent              an item's children (com_nalet_katalog_items:
--                                 parent_id): a series' seasons and episodes,
--                                 a season's episodes.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table. A large
-- catalog holds some 20,000 ids and 80,000 items, so each index is built at
-- once, not concurrently.
CREATE INDEX IF NOT EXISTS idx_itemexternalids_identity ON com_nalet_katalog_itemexternalids (source, externalid);
CREATE INDEX IF NOT EXISTS idx_itemexternalids_item ON com_nalet_katalog_itemexternalids (item_id);
CREATE INDEX IF NOT EXISTS idx_items_parent ON com_nalet_katalog_items (parent_id);
