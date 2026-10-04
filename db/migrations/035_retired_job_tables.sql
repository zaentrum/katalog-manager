-- 035_retired_job_tables.sql — drops the job tables of an integration the core
-- no longer carries, each only while it is empty (idempotent).
--
-- The core once carried an integration that fetched files from outside the
-- catalog (see 028). Files enter the catalog through the scanner and POST
-- /api/ingest only, and nothing reads or writes what that integration kept:
--   com_nalet_katalog_trailerjobs   its job table, which 028 created;
--   com_nalet_katalog_downloadjobs  the base schema's read model of its job
--                                   events, with the view the base schema
--                                   reads it through,
--                                   katalogservice_downloadjobs, and the
--                                   unique index 028 put on it,
--                                   idx_downloadjobs_client.
-- A table is dropped only while it holds no row, its view and index with it.
-- One that holds a row is left as it is, view and index too: its rows are a
-- record nobody asked to lose, and the service says at startup that it kept
-- it. Nothing is dropped with CASCADE: an object of anyone else's that hangs
-- off a table fails the migration, and nothing goes.
--
-- A deployment that applies the whole base schema at every start gets
-- com_nalet_katalog_downloadjobs and its view back, empty, and the next start
-- drops them again.
--
-- One run at a time: the lock is held until the run commits, so instances
-- that start together do not drop at once.
DO $$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtext('com_nalet_katalog: 035_retired_job_tables'));
  IF to_regclass('com_nalet_katalog_trailerjobs') IS NOT NULL THEN
    LOCK TABLE com_nalet_katalog_trailerjobs IN ACCESS EXCLUSIVE MODE;
    IF NOT EXISTS (SELECT 1 FROM com_nalet_katalog_trailerjobs) THEN
      DROP TABLE com_nalet_katalog_trailerjobs;
    END IF;
  END IF;
  IF to_regclass('com_nalet_katalog_downloadjobs') IS NOT NULL THEN
    LOCK TABLE com_nalet_katalog_downloadjobs IN ACCESS EXCLUSIVE MODE;
    IF NOT EXISTS (SELECT 1 FROM com_nalet_katalog_downloadjobs) THEN
      DROP VIEW IF EXISTS katalogservice_downloadjobs;
      DROP TABLE com_nalet_katalog_downloadjobs;
    END IF;
  END IF;
END
$$;
