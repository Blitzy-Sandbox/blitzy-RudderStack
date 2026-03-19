-- Rollback: Remove columns added by migration 000045.
-- These columns were originally part of the updated 000044 migration but needed
-- a separate migration for environments where 000044 was already applied.

ALTER TABLE wh_sync_health DROP COLUMN IF EXISTS dest_name;
ALTER TABLE wh_sync_health DROP COLUMN IF EXISTS source_name;
ALTER TABLE wh_sync_health DROP COLUMN IF EXISTS workspace_id;
ALTER TABLE wh_sync_health DROP COLUMN IF EXISTS source_type;
ALTER TABLE wh_sync_health DROP COLUMN IF EXISTS dest_type;
