-- 053_styled_subtitles.sql — a subtitle's styled original, and the fonts a title's package carries (idempotent).
--
-- The packager keeps a styled subtitle (ASS/SSA) as it is, its script beside its WebVTT copy, and takes
-- the fonts its original carried into the package: package.json (and the package store's manifest.json)
-- name the script of a subtitle (subtitles[].styled: {path, format}) and the fonts (fonts[]: {path,
-- format, names, sourceStreamIndex}). Every client plays the WebVTT file as before; a player that draws
-- styles (libass) reads the script and loads the fonts. packaging-complete keeps them with the package's
-- other rows, and they go with them.
--
-- A subtitle of a package (com_nalet_katalog_subtitleassets):
--   styledpath    the script beside the subtitle's file, its path; null for a subtitle that has none
--   styledformat  the script's format: ass (an ASS or an SSA script); null without one
--
-- com_nalet_katalog_fontassets, a font file of the package a title plays a row:
--   id                 a name-based id: the same font of the same package keeps its id, so its URL stays
--   item_id            the title
--   versionid          the version whose package holds it; null for the package store's
--   path               the font file's path
--   format             ttf, otf, ttc, woff, woff2, or null for bytes that are none of them
--   names              the names a style or an override tag asks for it by (a JSON list of strings)
--   ordinal            its place among the package's fonts, from 0
--   sourcestreamindex  the original's attachment stream it was taken from, or null
--
-- Lowercase identifiers, like every com_nalet_katalog_* table. Adding the columns rewrites nothing
-- (no default); the table is new, and the read-only role of the catalog's readers (katalog-api) may
-- read it, as it reads the subtitles' columns.
ALTER TABLE com_nalet_katalog_subtitleassets
  ADD COLUMN IF NOT EXISTS styledpath VARCHAR(2048),
  ADD COLUMN IF NOT EXISTS styledformat VARCHAR(16);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_fontassets (
  id VARCHAR(36) NOT NULL PRIMARY KEY,
  item_id VARCHAR(36) NOT NULL,
  versionid VARCHAR(36),
  path VARCHAR(2048) NOT NULL,
  format VARCHAR(16),
  names JSONB NOT NULL DEFAULT '[]'::jsonb,
  ordinal INTEGER NOT NULL,
  sourcestreamindex INTEGER,
  createdat TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_fontassets_item ON com_nalet_katalog_fontassets (item_id, ordinal);
DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='cloud_katalog_ro') THEN
  GRANT SELECT ON com_nalet_katalog_fontassets TO cloud_katalog_ro; END IF; END $$;
