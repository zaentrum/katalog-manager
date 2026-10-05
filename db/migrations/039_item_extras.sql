-- 039_item_extras.sql — a title's extras (idempotent).
--
-- An extra is bonus material of a movie or a series, never of an episode: a
-- trailer or a teaser that is a file of its own, a featurette, a making-of, a
-- deleted scene. It is no item: it has no row in com_nalet_katalog_items and
-- no playback asset, and it is packaged on its own, into the package store at
-- packages/extras/<aa>/<extraId>/, never inside its title's package.
--
-- com_nalet_katalog_itemextras, one row per extra, keyed by its id (the
-- extraId, a lower-case UUID, the name its library folder has when it is
-- recorded):
--   item_id          the movie or the series it belongs to
--   kind             featurette, behind-the-scenes, making-of, deleted-scene,
--                    interview, trailer, teaser, gag-reel, short or other
--   title            as it was taken in; localizedtitles the title in other
--                    languages (an object keyed by BCP 47 language)
--   language         the language spoken in it; NULL when unknown
--   seasonnumber     a series' extra may name the season it belongs to
--   origin           where the file came from, when a record names it; an
--                    object, informational: nothing reads anything from it
--   sourcepath       the file it is packaged from, on the library storage;
--                    sourcesize its size and sourceqh1 its quick hash
--                    (sha256 of its first and last 64 KiB and its size), which
--                    tell the file after a move
--   recordpath       its folder in the library, when it is recorded there
--   registeredby     how it was taken in: api, scanner or library
--   sortorder, hidden, label
--                    what a viewer is shown: its place in the title's list
--                    (NULL: after those that have one), whether it is hidden,
--                    and a label shown instead of the title
--   state            where its packaging stands: pending (waiting to be
--                    sent), queued (sent to the transcoder), transcoding,
--                    transcoded (waiting for the packager), packaging, ready,
--                    failed (no attempt left) or missing (its file is gone)
--   error, attempts, failures, nextretryat, dispatchedat, heartbeatat
--                    the last failure's error, the runs started, the
--                    failures in a row, when it is due next, when its trigger
--                    was last sent, and its worker's last word
--   packagepath, packagedat, durationms, videocodec, width, height,
--   peakbandwidthbps, packagesizebytes
--                    its package: where it is, when it was recorded, how long
--                    it plays, its top rendition's codec and size, the
--                    highest bandwidth its master playlist names, and how
--                    large it is
--   removedat, removedby, removalreason
--                    its removal (the extra-removed fact): who removed it,
--                    when and why; a removed row stays, and its package is
--                    deleted a day later
-- An extra plays once it is packaged, until it is removed, unless it is
-- hidden or its file is missing:
--   packagedat IS NOT NULL AND removedat IS NULL AND NOT hidden
--   AND state <> 'missing'
-- A file is an extra once while it is not removed (idx_itemextras_source);
-- idx_itemextras_due serves the sweep that sends and heals the packaging of
-- the extras that are not done.
--
-- An item's rows go with it. Lowercase identifiers, like every
-- com_nalet_katalog_* table; the timestamps carry their time zone
-- (timestamptz).
CREATE TABLE IF NOT EXISTS com_nalet_katalog_itemextras (
  id VARCHAR(36) PRIMARY KEY,                  -- the extraId
  item_id VARCHAR(36) NOT NULL,                -- a movie or a series, never an episode
  kind VARCHAR(20) NOT NULL CHECK (kind IN ('featurette','behind-the-scenes','making-of','deleted-scene',
       'interview','trailer','teaser','gag-reel','short','other')),
  title VARCHAR(255) NOT NULL CHECK (title <> ''),   -- as taken in
  localizedtitles JSONB CHECK (localizedtitles IS NULL OR jsonb_typeof(localizedtitles)='object'),
  language VARCHAR(35), seasonnumber INTEGER CHECK (seasonnumber IS NULL OR seasonnumber >= 0),
  origin JSONB CHECK (origin IS NULL OR jsonb_typeof(origin)='object'),   -- informational, never fetched
  sourcepath VARCHAR(2048), sourcesize BIGINT,
  sourceqh1 VARCHAR(71) CHECK (sourceqh1 IS NULL OR sourceqh1 ~ '^sha256:[0-9a-f]{64}$'),
  recordpath VARCHAR(2048),                    -- library/<cat>/<aa>/<item>/extras/<id>/ when recorded
  registeredby VARCHAR(10) NOT NULL CHECK (registeredby IN ('api','scanner','library')),
  sortorder INTEGER, hidden BOOLEAN NOT NULL DEFAULT false, label VARCHAR(255),   -- the projection
  state VARCHAR(12) NOT NULL DEFAULT 'pending' CHECK (state IN
       ('pending','queued','transcoding','transcoded','packaging','ready','failed','missing')),
  error VARCHAR(500), attempts INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0,
  nextretryat TIMESTAMPTZ, dispatchedat TIMESTAMPTZ, heartbeatat TIMESTAMPTZ,
  packagepath VARCHAR(2048), packagedat TIMESTAMPTZ, durationms BIGINT, videocodec VARCHAR(40),
  width INTEGER, height INTEGER, peakbandwidthbps BIGINT, packagesizebytes BIGINT,
  removedat TIMESTAMPTZ, removedby VARCHAR(255), removalreason VARCHAR(500),   -- extra-removed
  createdat TIMESTAMPTZ NOT NULL DEFAULT now(), createdby VARCHAR(255),
  modifiedat TIMESTAMPTZ NOT NULL DEFAULT now(), modifiedby VARCHAR(255));
CREATE INDEX IF NOT EXISTS idx_itemextras_item ON com_nalet_katalog_itemextras (item_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_itemextras_source ON com_nalet_katalog_itemextras (sourcepath)
  WHERE removedat IS NULL AND sourcepath IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_itemextras_due ON com_nalet_katalog_itemextras (state, nextretryat)
  WHERE removedat IS NULL AND state NOT IN ('ready','failed');
