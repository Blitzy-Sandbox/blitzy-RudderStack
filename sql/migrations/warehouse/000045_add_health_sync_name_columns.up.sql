-- Add columns that were retroactively added to migration 000044 after it had
-- already been applied in some environments. CREATE TABLE IF NOT EXISTS does not
-- alter existing tables, so environments that ran the original 000044 will be
-- missing these columns. ADD COLUMN IF NOT EXISTS ensures idempotency: fresh
-- installs (where 000044 already includes these columns) will safely skip them,
-- while existing installs will gain the missing columns.

ALTER TABLE wh_sync_health ADD COLUMN IF NOT EXISTS dest_type VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE wh_sync_health ADD COLUMN IF NOT EXISTS source_type VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE wh_sync_health ADD COLUMN IF NOT EXISTS workspace_id VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE wh_sync_health ADD COLUMN IF NOT EXISTS source_name VARCHAR(256) NOT NULL DEFAULT '';
ALTER TABLE wh_sync_health ADD COLUMN IF NOT EXISTS dest_name VARCHAR(256) NOT NULL DEFAULT '';
