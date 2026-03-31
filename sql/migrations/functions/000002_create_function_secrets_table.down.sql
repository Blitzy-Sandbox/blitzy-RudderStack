-- Rollback: Remove function secrets storage
--
-- Reverses 000002_create_function_secrets_table.up.sql by dropping the
-- function_secrets table. Associated indexes (function_secrets_function_id_idx,
-- function_secrets_function_id_key_idx) and the foreign key constraint
-- referencing functions(id) are automatically removed when the table is dropped.

DROP TABLE IF EXISTS function_secrets;
