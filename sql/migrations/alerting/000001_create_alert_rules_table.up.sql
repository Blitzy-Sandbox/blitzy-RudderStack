-- Create alert rules table for configurable pipeline alerting (E-037).
--
-- Stores alert rule definitions including condition type, threshold, comparison
-- operator, notification channel list, and evaluation interval. Rules are
-- evaluated periodically by the AlertEngine (services/alerting/engine.go).
--
-- Consumed by services/alerting/postgres_repository.go which implements the
-- RuleRepository interface (Gap 14).

CREATE TABLE IF NOT EXISTS alert_rules (
    id                          TEXT            PRIMARY KEY,
    workspace_id                VARCHAR(64)     NOT NULL,
    condition                   VARCHAR(64)     NOT NULL,
    threshold                   DOUBLE PRECISION NOT NULL DEFAULT 0,
    comparison_operator         VARCHAR(16)     NOT NULL DEFAULT 'gt',
    channels                    JSONB           NOT NULL DEFAULT '[]'::jsonb,
    enabled                     BOOLEAN         NOT NULL DEFAULT true,
    evaluation_interval_seconds BIGINT          NOT NULL DEFAULT 0,
    created_at                  TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS alert_rules_workspace_id_idx
    ON alert_rules (workspace_id);

CREATE INDEX IF NOT EXISTS alert_rules_enabled_idx
    ON alert_rules (enabled) WHERE enabled = true;
