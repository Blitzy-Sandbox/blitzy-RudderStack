-- Create identity traits table for profile trait key-value pairs (E-026)

CREATE TABLE IF NOT EXISTS identity_traits (
    id          BIGSERIAL       PRIMARY KEY,
    graph_id    BIGINT          NOT NULL REFERENCES identity_graph(id) ON DELETE CASCADE,
    key         VARCHAR(256)    NOT NULL,
    value       JSONB,
    updated_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS identity_traits_graph_id_idx
    ON identity_traits (graph_id);

CREATE UNIQUE INDEX IF NOT EXISTS identity_traits_graph_key_idx
    ON identity_traits (graph_id, key);
