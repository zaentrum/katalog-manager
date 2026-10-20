-- 048_attached_subtitles.sql — subtitles attached to a title after its package completed (idempotent).
--
-- A title's subtitles are its package's and the files beside its source. A
-- subtitle can also be attached to it later, through POST
-- /api/library/items/{itemId}/subtitles: made from the title's audio or
-- another of its subtitles, or found elsewhere. Its file is written into the
-- folder of the version that plays, as subs/attached-<sha256>.<language>.vtt,
-- and its row says how it came to be (com_nalet_katalog_subtitleassets):
--   origin      how the subtitle was made: source (it came with the title's
--               file), provider (found elsewhere), transcribed (from the
--               title's audio), translated (from another of its subtitles) or
--               converted (another script of one); null for a subtitle of the
--               title's own, its package's or a file beside its source;
--   issdh       whether it is for deaf and hard-of-hearing viewers (sound
--               cues, speakers' names); false for the title's own, whose label
--               says so where it is;
--   versionid   the version it was attached to, whose timing it follows;
--   sha256      the hash of its WebVTT file, which names the file;
--   attachedby  who attached it: the client of the caller's token;
--   attachedat  when.
-- Only an attached subtitle can be removed through the API, and a new
-- version of the same file carries it over; a version of another file does
-- not.
--
-- Lowercase identifiers, like every com_nalet_katalog_* table. Adding the
-- columns rewrites nothing (no default but a constant one).
ALTER TABLE com_nalet_katalog_subtitleassets
  ADD COLUMN IF NOT EXISTS origin VARCHAR(16),
  ADD COLUMN IF NOT EXISTS issdh BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS versionid VARCHAR(36),
  ADD COLUMN IF NOT EXISTS sha256 VARCHAR(64),
  ADD COLUMN IF NOT EXISTS attachedby VARCHAR(255),
  ADD COLUMN IF NOT EXISTS attachedat TIMESTAMPTZ;
