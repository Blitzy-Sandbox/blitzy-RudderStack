-- Add backfill job tracking table and link to wh_uploads

CREATE TABLE IF NOT EXISTS wh_backfill_jobs (
    id BIGSERIAL PRIMARY KEY,
    source_id VARCHAR(64) NOT NULL,
    destination_id VARCHAR(64) NOT NULL,
    workspace_id VARCHAR(64) NOT NULL,
    start_date TIMESTAMPTZ NOT NULL,
    end_date TIMESTAMPTZ NOT NULL,
    status VARCHAR(64) NOT NULL DEFAULT 'pending',
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS wh_backfill_jobs_source_dest_status_idx
    ON wh_backfill_jobs (source_id, destination_id, status);

ALTER TABLE wh_uploads ADD COLUMN IF NOT EXISTS backfill_job_id BIGINT REFERENCES wh_backfill_jobs(id);
