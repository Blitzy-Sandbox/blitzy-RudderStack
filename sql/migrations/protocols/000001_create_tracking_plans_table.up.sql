-- Create tracking plans table for Protocols management
--
-- This table is the primary store for tracking plan definitions used by the
-- Protocols enforcement system (Sprint 5-7, E-020 to E-025). Each tracking plan
-- contains a JSON Schema draft-07 document that defines the expected event schema
-- for connected sources.
--
-- The enforcement_config column stores per-source, per-call-type enforcement mode
-- settings (Block Event, Omit Properties, Allow) as defined in E-022. When NULL,
-- the system falls back to the default enforcement behavior (equivalent to the
-- existing propagateValidationErrors toggle).
--
-- Consumed by protocols/storage/repository.go and the Protocols management API
-- endpoints (E-024).

CREATE TABLE IF NOT EXISTS tracking_plans (
    id                 BIGSERIAL    PRIMARY KEY,
    workspace_id       VARCHAR(64)  NOT NULL,
    name               VARCHAR(256) NOT NULL,
    schema             JSONB        NOT NULL,
    version            INTEGER      NOT NULL DEFAULT 1,
    enforcement_config JSONB,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Index on workspace_id for efficient per-workspace queries
CREATE INDEX IF NOT EXISTS tracking_plans_workspace_id_idx
    ON tracking_plans (workspace_id);
