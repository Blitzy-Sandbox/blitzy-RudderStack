-- Rollback: Remove tracking plan versions table
--
-- Reverses 000002_create_tracking_plan_versions_table.up.sql by dropping the
-- tracking_plan_versions table. Associated indexes
-- (tracking_plan_versions_tracking_plan_id_idx, tracking_plan_versions_plan_version_idx)
-- and foreign key constraints are automatically removed when the table is dropped.

DROP TABLE IF EXISTS tracking_plan_versions;
