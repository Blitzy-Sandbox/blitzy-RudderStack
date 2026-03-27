-- Rollback: Remove selective sync configuration
--
-- Reverses 000043_add_selective_sync_config.up.sql by dropping the
-- wh_selective_sync table. Associated indexes (wh_selective_sync_workspace_id_idx)
-- and constraints (UNIQUE on source_id, destination_id) are automatically
-- removed when the table is dropped.

DROP TABLE IF EXISTS wh_selective_sync;
