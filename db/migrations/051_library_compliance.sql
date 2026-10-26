-- 051_library_compliance.sql — the rules the workers run with, and how each title fares against them (idempotent).
--
-- The transcoder and the packager report the rules they run with, at their
-- start and every ten minutes (PUT /api/analyze/rules/{worker}): the
-- transcoder its caps, its 2160 bucket, its encoder and its ladders, the
-- packager its 5.1 companion, its audio bit rates and the image subtitles it
-- carries. The library's compliance check holds every movie and episode
-- against the rules reported, and keeps what it found here, so that the
-- report reads a table and never the library's records.
--
-- com_nalet_katalog_workerrules, a worker a row:
--   worker      transcoder or packager
--   version     the worker's image (its commit or release), as it says it
--   rules       its rules as it reported them, a JSON object
--   ruleshash   the hash of the rules the check reads of them: the same
--               rules reported again, by another version too, keep it
--   reportedat  its last report
--   changedat   when its rules last changed (ruleshash)
-- com_nalet_katalog_compliance, a title a row, the movies and the episodes
--   with a file or a package (an episode another's file covers is its
--   holder's):
--   kind            movie or episode
--   rules           the rules it breaks, empty when it breaks none
--   findings        [{rule, text, measured, limit}], one a rule it breaks
--   originalbytes, packagebytes, estimatedbytes
--                   the size of its original and of its package, and an
--                   estimate of its package once it is processed again
--   unchecked       why a rule was not checked of it (a record that cannot
--                   be read), or null
--   blocked         why processing it again can't help, as its original's
--                   record or its failed step tells (an original without
--                   video, a Dolby Vision profile the transcoder leaves as it
--                   is): a kind, or null
--   resolution, videocodec, videobitrate, bitrateestimated, bitratecap,
--   sizecap, durationseconds, packageaudio, originalaudio
--                   what the report's columns show of it, as the check
--                   measured it: its video's size and codec, its video's bit
--                   rate (bit/s) and whether that was estimated, the caps
--                   that hold for it (bit/s, bytes), its duration (s), and
--                   its package's and its original's audio
--   recordkey, records
--                   the facts the check read of its records (its version's
--                   package.json, its original's source.json), as JSON, and
--                   what they were read of (the version and the original,
--                   with when their rows last changed): a pass reads a title's
--                   records again only once the key changes, and checks it
--                   against other rules from these
--   ruleshash       the rules it was checked against (both workers' hashes,
--                   the library's layout, the check's own version): a row
--                   whose hash is no longer the rules' is checked again
--   staleat         when it was marked to be checked again (its package
--                   completed, its pipeline ended, an admin asked), null when
--                   it is not: the next pass checks it
--   evaluatedat     when it was checked
-- A deleted item's row goes with it. Lowercase identifiers, like every
-- com_nalet_katalog_* table.
CREATE TABLE IF NOT EXISTS com_nalet_katalog_workerrules (
  worker VARCHAR(16) PRIMARY KEY CHECK (worker IN ('transcoder', 'packager')),
  version VARCHAR(255),
  rules JSONB NOT NULL CHECK (jsonb_typeof(rules) = 'object'),
  ruleshash VARCHAR(64) NOT NULL,
  reportedat TIMESTAMPTZ NOT NULL DEFAULT now(),
  changedat TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS com_nalet_katalog_compliance (
  item_id VARCHAR(36) PRIMARY KEY,
  kind VARCHAR(8) NOT NULL CHECK (kind IN ('movie', 'episode')),
  rules VARCHAR(32)[] NOT NULL DEFAULT '{}',
  findings JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(findings) = 'array'),
  originalbytes BIGINT,
  packagebytes BIGINT,
  estimatedbytes BIGINT,
  unchecked VARCHAR(500),
  blocked VARCHAR(64),
  resolution VARCHAR(32),
  videocodec VARCHAR(32),
  videobitrate DOUBLE PRECISION,
  bitrateestimated BOOLEAN NOT NULL DEFAULT false,
  bitratecap DOUBLE PRECISION,
  sizecap BIGINT,
  durationseconds DOUBLE PRECISION,
  packageaudio VARCHAR(255),
  originalaudio VARCHAR(255),
  recordkey VARCHAR(400) NOT NULL DEFAULT '',
  records JSONB,
  ruleshash VARCHAR(64) NOT NULL,
  staleat TIMESTAMPTZ,
  evaluatedat TIMESTAMPTZ NOT NULL DEFAULT now());
-- a table an earlier build of this migration created gains the columns
ALTER TABLE com_nalet_katalog_compliance ADD COLUMN IF NOT EXISTS blocked VARCHAR(64),
  ADD COLUMN IF NOT EXISTS resolution VARCHAR(32), ADD COLUMN IF NOT EXISTS videocodec VARCHAR(32),
  ADD COLUMN IF NOT EXISTS videobitrate DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS bitrateestimated BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS bitratecap DOUBLE PRECISION, ADD COLUMN IF NOT EXISTS sizecap BIGINT,
  ADD COLUMN IF NOT EXISTS durationseconds DOUBLE PRECISION, ADD COLUMN IF NOT EXISTS packageaudio VARCHAR(255),
  ADD COLUMN IF NOT EXISTS originalaudio VARCHAR(255), ADD COLUMN IF NOT EXISTS recordkey VARCHAR(400) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS records JSONB;
-- the report sorts a filtered join, which no index of the table serves: an
-- earlier build's goes
DROP INDEX IF EXISTS idx_compliance_report;
CREATE INDEX IF NOT EXISTS idx_compliance_stale ON com_nalet_katalog_compliance (item_id) WHERE staleat IS NOT NULL;
