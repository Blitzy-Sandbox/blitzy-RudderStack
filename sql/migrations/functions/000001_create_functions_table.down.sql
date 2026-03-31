-- Rollback: Remove functions table
--
-- Reverses 000001_create_functions_table.up.sql by dropping the
-- functions table. Associated indexes (functions_workspace_id_idx,
-- functions_type_idx) are automatically removed when the table is dropped.

DROP TABLE IF EXISTS functions;
