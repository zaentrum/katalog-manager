-- 038_subtitle_forced.sql — whether a subtitle is forced (idempotent).
--
-- A forced subtitle carries only what a viewer must read to follow a title in
-- the language they hear (signs, a line in another language), and a player
-- shows it by itself. The packager says of each subtitle of a package whether
-- its source marks it forced, and packaging-complete keeps it, per subtitle
-- (com_nalet_katalog_subtitleassets):
--   isforced  whether the subtitle is forced; false for a subtitle file the
--             scanner records beside a source, and for a subtitle older than
--             this migration until its title is packaged again.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table. Adding the
-- column rewrites nothing (a constant default).
ALTER TABLE com_nalet_katalog_subtitleassets
  ADD COLUMN IF NOT EXISTS isforced BOOLEAN NOT NULL DEFAULT false;
