-- 031_item_locked_fields.sql — the fields of a title automation leaves alone (idempotent).
--
-- A title's credits follow TMDB: whenever its TMDB credits are read, they
-- replace the title's credits. metadatalocked keeps a title's metadata, credits
-- included, as someone set it. lockedfields names single fields to keep
-- without locking the rest, as a person's does (migration 030), with the field
-- names of the library record's metadata.json; today "credits" (or "people")
-- is the one automation honours: TMDB then neither adds a credit to the title
-- nor drops one.
ALTER TABLE com_nalet_katalog_items
  ADD COLUMN IF NOT EXISTS lockedfields JSONB CHECK (jsonb_typeof(lockedfields) = 'array');
