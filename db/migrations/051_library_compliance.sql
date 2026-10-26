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
--                   the size of its original and of its package, and what
--                   its package is to be after its rework at most
--   unchecked       why a rule was not checked of it (a record that cannot
--                   be read), or null
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
  ruleshash VARCHAR(64) NOT NULL,
  staleat TIMESTAMPTZ,
  evaluatedat TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS idx_compliance_report ON com_nalet_katalog_compliance
  (kind, (COALESCE(packagebytes, -1)) DESC, (COALESCE(originalbytes, -1)) DESC, item_id)
  WHERE rules <> '{}';
CREATE INDEX IF NOT EXISTS idx_compliance_stale ON com_nalet_katalog_compliance (item_id) WHERE staleat IS NOT NULL;
