-- Create alert rules table for configurable pipeline alerting (E-037)

CREATE TABLE IF NOT EXISTS alert_rules (
    id          BIGSERIAL       PRIMARY KEY,
    workspace_id VARCHAR(64)    NOT NULL,
    condition   JSONB           NOT NULL,
    threshold   JSONB           NOT NULL,
    channels    JSONB           NOT NULL,
    enabled     BOOLEAN         NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS alert_rules_workspace_id_idx
    ON alert_rules (workspace_id);

CREATE INDEX IF NOT EXISTS alert_rules_enabled_idx
    ON alert_rules (enabled) WHERE enabled = true;
