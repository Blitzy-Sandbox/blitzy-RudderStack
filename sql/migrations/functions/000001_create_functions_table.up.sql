-- Create functions table: primary function store for Source, Destination,
-- and Insert functions (E-018: Functions management CRUD API, Sprint 4-6).
--
-- Stores function definitions including JavaScript source code, type,
-- version, and workspace association. The three supported function types
-- are 'source' (custom webhook ingestion via onRequest), 'destination'
-- (per-event typed handlers like onTrack, onIdentify), and 'insert'
-- (pre-destination transformation hooks).
--
-- Consumed by functions/storage/repository.go and the Functions
-- management REST API (functions/api/handler.go).

CREATE TABLE IF NOT EXISTS functions (
    id              BIGSERIAL       PRIMARY KEY,
    workspace_id    VARCHAR(64)     NOT NULL,
    name            VARCHAR(256)    NOT NULL,
    type            VARCHAR(64)     NOT NULL,
    code            TEXT            NOT NULL,
    version         INTEGER         NOT NULL DEFAULT 1,
    settings        JSONB,
    created_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- Index on workspace_id for efficient per-workspace function listing
CREATE INDEX IF NOT EXISTS functions_workspace_id_idx
    ON functions (workspace_id);

-- Index on type for efficient filtering by function type
CREATE INDEX IF NOT EXISTS functions_type_idx
    ON functions (type);
