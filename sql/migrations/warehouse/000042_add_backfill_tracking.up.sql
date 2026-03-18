-- Add backfill tracking support for warehouse backfill feature (E-032)

-- Create wh_backfill_jobs table for tracking backfill job metadata
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
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_wh_backfill_jobs_src_dest
    ON wh_backfill_jobs (source_id, destination_id);

CREATE INDEX IF NOT EXISTS idx_wh_backfill_jobs_status
    ON wh_backfill_jobs (status);

CREATE INDEX IF NOT EXISTS idx_wh_backfill_jobs_workspace
    ON wh_backfill_jobs (workspace_id);

-- Add backfill_job_id foreign key column to wh_uploads
ALTER TABLE wh_uploads
    ADD COLUMN IF NOT EXISTS backfill_job_id BIGINT REFERENCES wh_backfill_jobs(id);

CREATE INDEX IF NOT EXISTS idx_wh_uploads_backfill_job_id
    ON wh_uploads (backfill_job_id);
