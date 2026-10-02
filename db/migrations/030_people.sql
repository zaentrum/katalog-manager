-- 030_people.sql — people who keep their TMDB identity and details (idempotent).
--
-- Until now a person was an id and a name. Enrichment saved each credited
-- person by name and dropped the TMDB person id the credit carried, so nothing
-- about a person could ever be fetched again. This adds what the catalog keeps
-- about a person, their images, and the cursors of TMDB's change lists, which
-- keep both people and titles fresh without crawling TMDB.
--
-- com_nalet_katalog_people
--   tmdbpersonid   the TMDB person id, unique where set: a credit finds its
--                  person by it. A row without one (from before this migration)
--                  is matched by name once, and then carries it.
--   lockedfields   the fields automation leaves alone, metadatalocked all of
--                  them. fieldorigins says where each field that is set came
--                  from ('tmdb', 'manual'). Both name the fields as the library
--                  record's person.json does: name, sortName, alsoKnownAs,
--                  birthDate, deathDate, birthPlace, biography, externalIds,
--                  images, plus knownForDepartment.
--   alsoknownas    a JSON array of names; biography a JSON object with one text
--                  per language, {"en": "...", "de": "..."}.
--   tmdbfetchedat  when TMDB was last read for the person; tmdbchangedat the
--                  last day TMDB's change list named them; modifiedat when their
--                  data last changed. createdat is unknown (NULL) for a person
--                  older than this migration.
-- com_nalet_katalog_personartwork
--   a person's images, kind 'profile': the bytes, their sha256 (lower-case hex),
--   size in pixels, the TMDB file path they came from; at most one primary per
--   person and kind.
-- com_nalet_katalog_referencesync
--   one row per TMDB change list (person, movie, tv): cursor is the day the
--   next run reads from (it re-reads that day, which may have changed since),
--   and the lastrun* columns say what the last run did.
--
-- Every new timestamp carries its time zone (timestamptz). Lowercase
-- identifiers, like every com_nalet_katalog_* table. Columns are added with
-- their constraints only when missing, so running this again changes nothing.
ALTER TABLE com_nalet_katalog_people
  ADD COLUMN IF NOT EXISTS sortname           VARCHAR(255),
  ADD COLUMN IF NOT EXISTS alsoknownas        JSONB CHECK (jsonb_typeof(alsoknownas) = 'array'),
  ADD COLUMN IF NOT EXISTS birthdate          DATE,
  ADD COLUMN IF NOT EXISTS deathdate          DATE,
  ADD COLUMN IF NOT EXISTS birthplace         TEXT,
  ADD COLUMN IF NOT EXISTS biography          JSONB CHECK (jsonb_typeof(biography) = 'object'),
  ADD COLUMN IF NOT EXISTS tmdbpersonid       TEXT CHECK (tmdbpersonid ~ '^[0-9]+$'),
  ADD COLUMN IF NOT EXISTS imdbid             TEXT CHECK (imdbid ~ '^nm[0-9]+$'),
  ADD COLUMN IF NOT EXISTS knownfordepartment TEXT,
  ADD COLUMN IF NOT EXISTS metadatalocked     BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS lockedfields       JSONB CHECK (jsonb_typeof(lockedfields) = 'array'),
  ADD COLUMN IF NOT EXISTS fieldorigins       JSONB CHECK (jsonb_typeof(fieldorigins) = 'object'),
  ADD COLUMN IF NOT EXISTS tmdbfetchedat      TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS tmdbchangedat      DATE,
  ADD COLUMN IF NOT EXISTS createdat          TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS modifiedat         TIMESTAMPTZ;
-- A default given with ADD COLUMN would stamp every existing person with the
-- moment of this migration; set apart, it applies to people created from now on.
ALTER TABLE com_nalet_katalog_people ALTER COLUMN createdat SET DEFAULT now();
CREATE UNIQUE INDEX IF NOT EXISTS idx_people_tmdbpersonid
  ON com_nalet_katalog_people (tmdbpersonid) WHERE tmdbpersonid IS NOT NULL;

CREATE TABLE IF NOT EXISTS com_nalet_katalog_personartwork (
  id          VARCHAR(36)  PRIMARY KEY,
  person_id   VARCHAR(36)  NOT NULL,
  kind        VARCHAR(20)  NOT NULL DEFAULT 'profile' CHECK (kind IN ('profile')),
  contenttype VARCHAR(80)  NOT NULL,
  bytes       BYTEA        NOT NULL,
  sha256      CHAR(64)     NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  width       INTEGER,
  height      INTEGER,
  isprimary   BOOLEAN      NOT NULL DEFAULT false,
  sourcepath  VARCHAR(2048),
  fetchedat   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_personartwork_person
  ON com_nalet_katalog_personartwork (person_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_personartwork_primary
  ON com_nalet_katalog_personartwork (person_id, kind) WHERE isprimary;

CREATE TABLE IF NOT EXISTS com_nalet_katalog_referencesync (
  kind             TEXT        PRIMARY KEY CHECK (kind IN ('person', 'movie', 'tv')),
  cursor           DATE        NOT NULL,
  lastrunat        TIMESTAMPTZ,
  lastrunchanges   INTEGER,
  lastrunmatched   INTEGER,
  lastrunrefreshed INTEGER,
  lastrunskipped   INTEGER,
  lastrunfailed    INTEGER,
  lastrunerror     TEXT
);
