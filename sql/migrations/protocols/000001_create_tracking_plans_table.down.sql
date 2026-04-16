-- Rollback: Remove tracking plans table
--
-- Reverses 000001_create_tracking_plans_table.up.sql by dropping the
-- tracking_plans table. Associated indexes (tracking_plans_workspace_id_idx)
-- are automatically removed when the table is dropped.
--
-- Note: tracking_plan_versions (which references this table via FK) must be
-- dropped first. The migration runner handles this automatically by running
-- 000002 down before 000001 down.

DROP TABLE IF EXISTS tracking_plans;
