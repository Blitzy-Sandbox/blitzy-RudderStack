-- Create identity external IDs table for external identifier mappings (E-026, E-028)
--
-- Stores external identifiers (12+ types: user_id, email, anonymous_id, ios.id,
-- android.id, ga_client_id, amp_id, etc.) linked to identity graph nodes.
-- Each external identifier value is globally unique per type.

CREATE TABLE IF NOT EXISTS identity_external_ids (
    id               BIGSERIAL       PRIMARY KEY,
    graph_id         BIGINT          NOT NULL REFERENCES identity_graph(id) ON DELETE CASCADE,
    external_id_type VARCHAR(128)    NOT NULL,
    external_id_value TEXT           NOT NULL,
    created_source   VARCHAR(128),
    created_at       TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    merged_at        TIMESTAMPTZ,
    merged_from      BIGINT
);

CREATE INDEX IF NOT EXISTS identity_external_ids_graph_id_idx
    ON identity_external_ids (graph_id);

CREATE UNIQUE INDEX IF NOT EXISTS identity_external_ids_type_value_idx
    ON identity_external_ids (external_id_type, external_id_value);
