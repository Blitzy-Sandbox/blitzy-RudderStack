// Package alerting provides the PostgreSQL-backed implementation of RuleRepository (Gap 14).
//
// This file implements the full CRUD interface for alert rules using the alert_rules
// table created by sql/migrations/alerting/000001_create_alert_rules_table.up.sql.
// The implementation follows the same database access patterns used in
// protocols/storage/repository.go and functions/storage/repository.go.
package alerting

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
)

// PostgresRuleRepository implements RuleRepository using a PostgreSQL database.
// It stores and retrieves alert rules from the alert_rules table.
//
// Thread-safe: all methods use context-scoped database queries and do not hold
// any mutable state. Concurrent calls are safe because the underlying *sql.DB
// handles connection pooling and serialization internally.
type PostgresRuleRepository struct {
	db *sql.DB
}

// NewPostgresRuleRepository creates a new PostgresRuleRepository backed by the given
// database connection pool. The pool should be the same jobsdbPool used throughout
// the embedded app handler for sprint migrations and other sprint APIs.
//
// The caller is responsible for ensuring that the alert_rules table exists (created
// by sql/migrations/alerting/000001_create_alert_rules_table.up.sql, which is run
// during startup by the sprint migration block in embeddedAppHandler.go — Gap 1).
func NewPostgresRuleRepository(db *sql.DB) *PostgresRuleRepository {
	return &PostgresRuleRepository{db: db}
}

// Create stores a new alert rule and returns the assigned unique ID.
// The rule's ID field is ignored on input; a new UUID is generated.
// CreatedAt and UpdatedAt are set to the current time.
func (r *PostgresRuleRepository) Create(ctx context.Context, rule AlertRule) (string, error) {
	id := uuid.New().String()
	now := time.Now().UTC()

	channelsJSON, err := jsonrs.Marshal(rule.Channels)
	if err != nil {
		return "", fmt.Errorf("alerting: marshal channels: %w", err)
	}

	evalIntervalSecs := int64(rule.EvaluationInterval.Seconds())

	_, err = r.db.ExecContext(ctx,
		`INSERT INTO alert_rules (id, workspace_id, condition, threshold, comparison_operator, channels, enabled, evaluation_interval_seconds, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, rule.WorkspaceID, string(rule.Condition), rule.Threshold,
		string(rule.ComparisonOperator), string(channelsJSON), rule.Enabled,
		evalIntervalSecs, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("alerting: create rule: %w", err)
	}
	return id, nil
}

// Get retrieves a single alert rule by its unique ID.
// Returns an error if the rule does not exist.
func (r *PostgresRuleRepository) Get(ctx context.Context, id string) (AlertRule, error) {
	var rule AlertRule
	var channelsJSON string
	var conditionStr, operatorStr string
	var evalIntervalSecs int64

	err := r.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, condition, threshold, comparison_operator, channels, enabled, evaluation_interval_seconds, created_at, updated_at
		 FROM alert_rules WHERE id = $1`, id,
	).Scan(
		&rule.ID, &rule.WorkspaceID, &conditionStr, &rule.Threshold,
		&operatorStr, &channelsJSON, &rule.Enabled,
		&evalIntervalSecs, &rule.CreatedAt, &rule.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return AlertRule{}, fmt.Errorf("alerting: rule %q not found", id)
	}
	if err != nil {
		return AlertRule{}, fmt.Errorf("alerting: get rule: %w", err)
	}

	rule.Condition = AlertCondition(conditionStr)
	rule.ComparisonOperator = ComparisonOperator(operatorStr)
	rule.EvaluationInterval = time.Duration(evalIntervalSecs) * time.Second

	if err := jsonrs.Unmarshal([]byte(channelsJSON), &rule.Channels); err != nil {
		return AlertRule{}, fmt.Errorf("alerting: unmarshal channels: %w", err)
	}
	return rule, nil
}

// Update modifies an existing alert rule identified by its ID field.
// UpdatedAt is refreshed. Returns an error if the rule does not exist.
func (r *PostgresRuleRepository) Update(ctx context.Context, rule AlertRule) error {
	channelsJSON, err := jsonrs.Marshal(rule.Channels)
	if err != nil {
		return fmt.Errorf("alerting: marshal channels: %w", err)
	}

	evalIntervalSecs := int64(rule.EvaluationInterval.Seconds())
	now := time.Now().UTC()

	result, err := r.db.ExecContext(ctx,
		`UPDATE alert_rules SET workspace_id = $1, condition = $2, threshold = $3, comparison_operator = $4, channels = $5, enabled = $6, evaluation_interval_seconds = $7, updated_at = $8
		 WHERE id = $9`,
		rule.WorkspaceID, string(rule.Condition), rule.Threshold,
		string(rule.ComparisonOperator), string(channelsJSON), rule.Enabled,
		evalIntervalSecs, now, rule.ID,
	)
	if err != nil {
		return fmt.Errorf("alerting: update rule: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("alerting: update rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("alerting: rule %q not found", rule.ID)
	}
	return nil
}

// Delete removes an alert rule by its unique ID.
// Returns an error if the rule does not exist.
func (r *PostgresRuleRepository) Delete(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM alert_rules WHERE id = $1`, id,
	)
	if err != nil {
		return fmt.Errorf("alerting: delete rule: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("alerting: delete rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("alerting: rule %q not found", id)
	}
	return nil
}

// List returns all alert rules belonging to the specified workspace,
// ordered by creation time (newest first). Returns an empty slice
// if no rules exist for the workspace.
func (r *PostgresRuleRepository) List(ctx context.Context, workspaceID string) ([]AlertRule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, workspace_id, condition, threshold, comparison_operator, channels, enabled, evaluation_interval_seconds, created_at, updated_at
		 FROM alert_rules WHERE workspace_id = $1 ORDER BY created_at DESC`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("alerting: list rules: %w", err)
	}
	defer rows.Close()

	return scanRules(rows)
}

// ListEnabled returns all enabled alert rules across all workspaces.
// This method is used by the alerting engine's evaluation loop to
// retrieve the set of rules that need periodic evaluation.
func (r *PostgresRuleRepository) ListEnabled(ctx context.Context) ([]AlertRule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, workspace_id, condition, threshold, comparison_operator, channels, enabled, evaluation_interval_seconds, created_at, updated_at
		 FROM alert_rules WHERE enabled = true ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("alerting: list enabled rules: %w", err)
	}
	defer rows.Close()

	return scanRules(rows)
}

// scanRules scans all rows from a query result into a slice of AlertRule.
func scanRules(rows *sql.Rows) ([]AlertRule, error) {
	var rules []AlertRule
	for rows.Next() {
		var rule AlertRule
		var channelsJSON string
		var conditionStr, operatorStr string
		var evalIntervalSecs int64

		if err := rows.Scan(
			&rule.ID, &rule.WorkspaceID, &conditionStr, &rule.Threshold,
			&operatorStr, &channelsJSON, &rule.Enabled,
			&evalIntervalSecs, &rule.CreatedAt, &rule.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("alerting: scan rule: %w", err)
		}

		rule.Condition = AlertCondition(conditionStr)
		rule.ComparisonOperator = ComparisonOperator(operatorStr)
		rule.EvaluationInterval = time.Duration(evalIntervalSecs) * time.Second

		if err := jsonrs.Unmarshal([]byte(channelsJSON), &rule.Channels); err != nil {
			return nil, fmt.Errorf("alerting: unmarshal channels: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alerting: rows iteration: %w", err)
	}
	if rules == nil {
		rules = []AlertRule{}
	}
	return rules, nil
}
