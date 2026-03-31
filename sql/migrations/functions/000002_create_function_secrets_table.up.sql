-- Create function_secrets table for per-function encrypted secrets
-- and environment variable storage (E-019: Per-function secrets management)
--
-- This table stores per-function secret key-value pairs where values are
-- encrypted at rest. Each secret is scoped to a specific function via the
-- function_id foreign key. When a function is deleted, all associated
-- secrets are cascade-deleted automatically.
--
-- Consumed by functions/secrets/manager.go and the Functions management API.

CREATE TABLE IF NOT EXISTS function_secrets (
    id              BIGSERIAL       PRIMARY KEY,
    function_id     BIGINT          NOT NULL REFERENCES functions(id) ON DELETE CASCADE,
    key             VARCHAR(256)    NOT NULL,
    encrypted_value TEXT            NOT NULL,
    created_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- Index on function_id for efficient lookup of all secrets for a given function
CREATE INDEX IF NOT EXISTS function_secrets_function_id_idx
    ON function_secrets (function_id);

-- Unique index on (function_id, key) to enforce one secret per key name per function
CREATE UNIQUE INDEX IF NOT EXISTS function_secrets_function_id_key_idx
    ON function_secrets (function_id, key);
