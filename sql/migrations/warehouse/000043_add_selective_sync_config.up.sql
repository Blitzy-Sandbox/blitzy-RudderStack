-- Add selective sync configuration table for per-table and per-column exclusion rules
--
-- This table stores per-source/destination selective sync configuration
-- that controls which tables and columns are included or excluded from
-- warehouse sync. The excluded_tables column holds a JSON array of table
-- names (e.g. ["users", "tracks"]), and excluded_columns holds a JSON
-- object keyed by table name with arrays of column names
-- (e.g. {"users": ["email", "phone"], "tracks": ["ip"]}).
--
-- Consumed by warehouse/selectivesync/repository.go and supports the
-- selective sync API endpoints (PUT/GET /v1/warehouse/selective-sync).

CREATE TABLE IF NOT EXISTS wh_selective_sync (
    id              BIGSERIAL       PRIMARY KEY,
    source_id       VARCHAR(64)     NOT NULL,
    destination_id  VARCHAR(64)     NOT NULL,
    workspace_id    VARCHAR(64)     NOT NULL,
    excluded_tables JSONB,
    excluded_columns JSONB,
    created_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ,
    UNIQUE (source_id, destination_id)
);

-- Index on workspace_id for efficient per-workspace queries
CREATE INDEX IF NOT EXISTS wh_selective_sync_workspace_id_idx
    ON wh_selective_sync (workspace_id);
