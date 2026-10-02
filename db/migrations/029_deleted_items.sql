-- 029_deleted_items.sql — the deletion log: what the catalog deleted (idempotent).
--
-- The library tree on storage is a written-once record that can rebuild the
-- catalog. When an item leaves the catalog but its folder survives on storage, a
-- verification must tell whether the catalog lost the item (restore it) or
-- deleted it (sweep the folder). This table is how it tells.
--
-- Every path that deletes an item writes its row here IN THE SAME TRANSACTION as
-- the delete: an item never leaves without a row, and a delete that fails leaves
-- none. A delete that cannot write its row fails.
--
-- One row per deleted id, holding its latest deletion: deleting an id that is
-- already here (re-created with the same id, then deleted again) replaces the
-- row. Re-creating an item does not touch this table, so an id can be here AND
-- in com_nalet_katalog_items. Readers treat that id as present: the item that
-- exists wins, and the row only says what happened to an earlier life of the id.
--
-- deletedat is UTC whatever the session's TimeZone (now() AT TIME ZONE 'utc').
-- deletedby is the authenticated principal's subject for a delete made through
-- the API, or the service that deleted, e.g. katalog-manager/scanner.
-- Lowercase identifiers, like every com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_deleteditems (
  id        VARCHAR(36)  PRIMARY KEY,
  type      VARCHAR(20)  NOT NULL,
  title     VARCHAR(255) NOT NULL,
  deletedat TIMESTAMP    NOT NULL DEFAULT (now() AT TIME ZONE 'utc'),
  deletedby VARCHAR(255) NOT NULL,
  reason    VARCHAR(500)
);
CREATE INDEX IF NOT EXISTS idx_deleteditems_deletedat
  ON com_nalet_katalog_deleteditems (deletedat);
