-- Create identity graph table as core node table for real-time identity resolution (E-026)
--
-- Each row represents a unified user profile (identity graph node) scoped to a workspace.
-- Referenced by identity_external_ids and identity_traits tables via foreign keys.

CREATE TABLE IF NOT EXISTS identity_graph (
    id          BIGSERIAL       PRIMARY KEY,
    workspace_id VARCHAR(64)    NOT NULL,
    segment_id  UUID            NOT NULL,
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS identity_graph_workspace_id_idx
    ON identity_graph (workspace_id);

CREATE UNIQUE INDEX IF NOT EXISTS identity_graph_workspace_segment_idx
    ON identity_graph (workspace_id, segment_id);
