-- Add health monitoring table for tracking per-upload sync metrics

CREATE TABLE IF NOT EXISTS wh_sync_health (
    id BIGSERIAL PRIMARY KEY,
    upload_id BIGINT REFERENCES wh_uploads(id),
    source_id VARCHAR(64) NOT NULL,
    destination_id VARCHAR(64) NOT NULL,
    dest_type VARCHAR(64) NOT NULL DEFAULT '',
    source_type VARCHAR(64) NOT NULL DEFAULT '',
    workspace_id VARCHAR(64) NOT NULL DEFAULT '',
    source_name VARCHAR(256) NOT NULL DEFAULT '',
    dest_name VARCHAR(256) NOT NULL DEFAULT '',
    status VARCHAR(64) NOT NULL,
    duration_ms BIGINT,
    rows_synced BIGINT,
    rows_failed BIGINT,
    error_category VARCHAR(64),
    schema_changes JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS wh_sync_health_source_dest_created_at_idx
    ON wh_sync_health (source_id, destination_id, created_at);

CREATE INDEX IF NOT EXISTS wh_sync_health_upload_id_idx
    ON wh_sync_health (upload_id);
