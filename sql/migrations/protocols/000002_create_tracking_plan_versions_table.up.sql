-- Create tracking plan versions table for version history tracking
--
-- This table stores historical versions of tracking plan schemas, enabling
-- version comparison, rollback, and audit trail. Each row represents a
-- point-in-time snapshot of a tracking plan's schema. Used by the Protocols
-- management API (E-024) for tracking plan versioning and CSV import/export.
--
-- The tracking_plan_id foreign key uses ON DELETE CASCADE so that when a
-- tracking plan is deleted, all its version history is automatically cleaned up.

CREATE TABLE IF NOT EXISTS tracking_plan_versions (
    id                BIGSERIAL   PRIMARY KEY,
    tracking_plan_id  BIGINT      NOT NULL REFERENCES tracking_plans(id) ON DELETE CASCADE,
    version           INTEGER     NOT NULL,
    schema            JSONB       NOT NULL,
    changelog         TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index for efficient lookups by tracking plan ID
CREATE INDEX IF NOT EXISTS tracking_plan_versions_tracking_plan_id_idx
    ON tracking_plan_versions (tracking_plan_id);

-- Unique index ensuring each (tracking_plan_id, version) pair is unique
CREATE UNIQUE INDEX IF NOT EXISTS tracking_plan_versions_plan_version_idx
    ON tracking_plan_versions (tracking_plan_id, version);
