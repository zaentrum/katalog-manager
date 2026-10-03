-- 032_credit_details.sql — what a credit says besides its role (idempotent).
--
-- A credit links a person to a title in a role. The role is a lowercase word
-- (actor, creator, director, writer, producer, composer, cinematographer,
-- editor, or another), and a credit is the title, the person and the role:
-- one per person and role. What TMDB says of a credit beyond that are its
-- attributes, updated in place when TMDB changes them:
--   job           the person's jobs in the role, as TMDB names them, joined
--                 with ", " ("Writer, Co-Writer"); "Creator" for the creator
--                 of a series; NULL for an actor
--   charactername whom an actor plays; for a series every character, the most
--                 episodes first, joined with " / "
--   ordinal       the credit's place among the title's credits in its role,
--                 0 first: a film's billing order, otherwise the rank
--   episodecount  the episodes of a series the person is credited in, in the
--                 role; NULL for a film
-- NULL is unknown, as for every credit older than this migration.
ALTER TABLE com_nalet_katalog_itempeople
  ADD COLUMN IF NOT EXISTS job           VARCHAR(255),
  ADD COLUMN IF NOT EXISTS charactername TEXT,
  ADD COLUMN IF NOT EXISTS ordinal       INTEGER,
  ADD COLUMN IF NOT EXISTS episodecount  INTEGER;
