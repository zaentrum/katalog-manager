-- 037_track_languages.sql — the languages of a title's audio and subtitle tracks (idempotent).
--
-- A source file carries audio and subtitle streams, each tagged with a
-- language or not, and not always rightly: a film without dialogue tagged
-- und, English tagged und. A track is named by its kind (audio or subtitle)
-- and its ordinal, its place among the source's streams of that kind in
-- ffprobe's order, 0 first: audio 0 is the first audio stream. Languages are
-- ISO 639-2 codes, three lowercase letters, in the B ("ger") or the T form
-- ("deu"); zxx is no linguistic content (a film without dialogue), und
-- undetermined.
--
-- com_nalet_katalog_itemtracks
--   the source's tracks as the packager last read them, one row per track:
--   language   the language the source tags it with; NULL when the packager
--              said none, or when the only package that reported the track
--              said the language an admin set, which hides the source's tag
--   title      its title tag ("Commentary", "AC3 5.1 @ 640 Kbps")
--   format     a subtitle's format as packaged: webvtt, pgs, vobsub, dvb
--   forced     whether the source marks a subtitle forced
--   updatedat  when a package last reported it
--   packaging-complete writes them from the package's manifest, and
--   backfillSourceTracks from the manifests of the packages on disk; a track
--   a package no longer reports goes.
-- com_nalet_katalog_itemtracklanguages
--   an admin's language of a track (setTrackLanguage), keyed as a track is.
--   It wins over the source's tag, and the packager labels the track with
--   it. It is kept when no package reports the track: the packager passes
--   over a track the source does not have.
-- A track plays as its admin's language, else its source's tag, else und.
--
-- Both are keyed by the item, the kind and the ordinal, and an item's rows
-- go with it. Lowercase identifiers, like every com_nalet_katalog_* table;
-- the timestamps carry their time zone (timestamptz).
CREATE TABLE IF NOT EXISTS com_nalet_katalog_itemtracks (
  item_id   VARCHAR(36)  NOT NULL,
  kind      VARCHAR(10)  NOT NULL CHECK (kind IN ('audio', 'subtitle')),
  ordinal   INTEGER      NOT NULL CHECK (ordinal >= 0),
  language  VARCHAR(35),
  title     VARCHAR(255),
  format    VARCHAR(20),
  forced    BOOLEAN      NOT NULL DEFAULT false,
  updatedat TIMESTAMPTZ  NOT NULL DEFAULT now(),
  PRIMARY KEY (item_id, kind, ordinal)
);

CREATE TABLE IF NOT EXISTS com_nalet_katalog_itemtracklanguages (
  item_id    VARCHAR(36)  NOT NULL,
  kind       VARCHAR(10)  NOT NULL CHECK (kind IN ('audio', 'subtitle')),
  ordinal    INTEGER      NOT NULL CHECK (ordinal >= 0),
  language   VARCHAR(3)   NOT NULL CHECK (language ~ '^[a-z]{3}$'),
  modifiedat TIMESTAMPTZ  NOT NULL DEFAULT now(),
  modifiedby VARCHAR(255),
  PRIMARY KEY (item_id, kind, ordinal)
);
