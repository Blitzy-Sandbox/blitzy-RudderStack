-- Rollback: Remove backfill tracking
--
-- Reverses 000042_add_backfill_tracking.up.sql:
--   1. Drops the backfill_job_id FK column from wh_uploads (must happen first to release the FK constraint)
--   2. Drops the wh_backfill_jobs table (indexes are dropped automatically with the table)
--

ALTER TABLE wh_uploads DROP COLUMN IF EXISTS backfill_job_id;

DROP TABLE IF EXISTS wh_backfill_jobs;
