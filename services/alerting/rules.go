// Package alerting provides configurable alerting rules for monitoring pipeline health
// conditions across the RudderStack event delivery system. It defines alert rule types,
// metric snapshot structures, threshold evaluation logic, and a repository interface
// for persistent rule management.
//
// Alert rules support five pipeline health conditions: throughput drops, error rate spikes,
// delivery failures, warehouse latency, and JobsDB queue depth. Rules are evaluated against
// current metric snapshots using configurable comparison operators and thresholds.
package alerting

import (
	"context"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// AlertCondition — typed string enum for pipeline health conditions
// ---------------------------------------------------------------------------

// AlertCondition represents the type of pipeline health condition to monitor.
// Each condition maps to a specific metric in MetricSnapshot, enabling the
// EvaluateRule function to extract the appropriate metric value for comparison.
type AlertCondition string

const (
	// ThroughputDrop triggers when event throughput falls below the configured
	// threshold (events per second). This condition is typically paired with
	// a LessThan comparison operator to detect pipeline slowdowns.
	ThroughputDrop AlertCondition = "throughput_drop"

	// ErrorRateSpike triggers when the error rate exceeds the configured
	// threshold percentage (0-100). This condition is typically paired with
	// a GreaterThan comparison operator to detect quality degradation.
	ErrorRateSpike AlertCondition = "error_rate_spike"

	// DeliveryFailures triggers when the delivery failure count exceeds
	// the configured threshold (absolute count). Useful for detecting
	// destination-level outages or persistent delivery problems.
	DeliveryFailures AlertCondition = "delivery_failures"

	// WarehouseLatency triggers when warehouse upload latency exceeds
	// the configured threshold in seconds. Monitors the warehouse upload
	// state machine for excessive delays.
	WarehouseLatency AlertCondition = "warehouse_latency"

	// JobsDBQueueDepth triggers when the pending job count exceeds the
	// configured threshold. Monitors JobsDB backpressure that can indicate
	// pipeline congestion or slow consumers.
	JobsDBQueueDepth AlertCondition = "jobsdb_queue_depth"
)

// ---------------------------------------------------------------------------
// ComparisonOperator — typed string enum for threshold comparison modes
// ---------------------------------------------------------------------------

// ComparisonOperator defines how to compare a metric value against a threshold.
// The operator is applied as: metricValue <operator> threshold, where the
// operator determines the comparison semantics.
type ComparisonOperator string

const (
	// LessThan evaluates true when the metric value is strictly less than the threshold.
	LessThan ComparisonOperator = "lt"

	// GreaterThan evaluates true when the metric value is strictly greater than the threshold.
	GreaterThan ComparisonOperator = "gt"

	// LessThanOrEqual evaluates true when the metric value is less than or equal to the threshold.
	LessThanOrEqual ComparisonOperator = "lte"

	// GreaterThanOrEqual evaluates true when the metric value is greater than or equal to the threshold.
	GreaterThanOrEqual ComparisonOperator = "gte"

	// Equal evaluates true when the metric value is exactly equal to the threshold.
	Equal ComparisonOperator = "eq"
)

// ---------------------------------------------------------------------------
// AlertRule — configurable alert rule with threshold evaluation parameters
// ---------------------------------------------------------------------------

// AlertRule defines a configurable alert rule with threshold evaluation parameters.
// Each rule specifies a pipeline health condition, a numeric threshold, a comparison
// operator, and a list of notification channels. Rules can be enabled or disabled
// independently, and each rule has a configurable evaluation interval that determines
// how frequently the rule should be checked against current metrics.
//
// AlertRule instances are persisted via the RuleRepository interface and evaluated
// by the EvaluateRule function against MetricSnapshot values.
type AlertRule struct {
	// ID is the unique identifier for this alert rule, assigned by the repository
	// on creation. Typically a UUID string.
	ID string `json:"id"`

	// WorkspaceID identifies the workspace this rule belongs to, enabling
	// multi-tenant alert isolation.
	WorkspaceID string `json:"workspace_id"`

	// Condition specifies which pipeline health metric to evaluate.
	Condition AlertCondition `json:"condition"`

	// Threshold is the numeric boundary for the comparison. The semantics
	// depend on the Condition type (e.g., events/sec for ThroughputDrop,
	// percentage for ErrorRateSpike, seconds for WarehouseLatency).
	Threshold float64 `json:"threshold"`

	// ComparisonOperator determines how the metric value is compared against
	// the Threshold (e.g., LessThan, GreaterThan).
	ComparisonOperator ComparisonOperator `json:"comparison_operator"`

	// Channels lists the notification channel identifiers to alert when the
	// rule is triggered. Each string references a configured channel (e.g.,
	// "webhook:https://...", "slack:#alerts", "email:ops@example.com").
	Channels []string `json:"channels"`

	// Enabled determines whether this rule is actively evaluated. Disabled
	// rules are skipped by EvaluateRule without error.
	Enabled bool `json:"enabled"`

	// EvaluationInterval specifies how frequently this rule should be
	// evaluated against current metrics. A zero value indicates the
	// default evaluation interval should be used.
	EvaluationInterval time.Duration `json:"evaluation_interval"`

	// CreatedAt records when this rule was first created.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt records the last modification time for this rule.
	UpdatedAt time.Time `json:"updated_at"`
}

// validConditions enumerates all recognized AlertCondition values for
// validation purposes. A rule whose Condition is not in this set is rejected.
var validConditions = map[AlertCondition]struct{}{
	ThroughputDrop:   {},
	ErrorRateSpike:   {},
	DeliveryFailures: {},
	WarehouseLatency: {},
	JobsDBQueueDepth: {},
}

// validOperators enumerates all recognized ComparisonOperator values for
// validation purposes. A rule whose ComparisonOperator is not in this set
// is rejected.
var validOperators = map[ComparisonOperator]struct{}{
	LessThan:           {},
	GreaterThan:        {},
	LessThanOrEqual:    {},
	GreaterThanOrEqual: {},
	Equal:              {},
}

// Validate checks all field constraints of the AlertRule and returns an error
// describing the first validation failure encountered, or nil if the rule is
// valid. Callers should invoke Validate before persisting or evaluating a rule.
//
// Checked constraints:
//   - Condition must be one of the recognized AlertCondition values.
//   - Threshold must be non-negative.
//   - ComparisonOperator must be one of the recognized operator values.
//   - Channels must contain at least one entry (otherwise the alert has no
//     notification target).
func (r AlertRule) Validate() error {
	if r.Condition == "" {
		return fmt.Errorf("alert rule validation: condition is required")
	}
	if _, ok := validConditions[r.Condition]; !ok {
		return fmt.Errorf("alert rule validation: unknown condition %q", r.Condition)
	}
	if r.Threshold < 0 {
		return fmt.Errorf("alert rule validation: threshold must be non-negative, got %f", r.Threshold)
	}
	if r.ComparisonOperator == "" {
		return fmt.Errorf("alert rule validation: comparison operator is required")
	}
	if _, ok := validOperators[r.ComparisonOperator]; !ok {
		return fmt.Errorf("alert rule validation: unknown comparison operator %q", r.ComparisonOperator)
	}
	if len(r.Channels) == 0 {
		return fmt.Errorf("alert rule validation: at least one notification channel is required")
	}
	return nil
}

// ---------------------------------------------------------------------------
// MetricSnapshot — current pipeline metric values for rule evaluation
// ---------------------------------------------------------------------------

// MetricSnapshot holds current pipeline metric values collected from Prometheus
// counters, gauges, and histograms. Each field corresponds to one AlertCondition
// type, enabling the EvaluateRule function to extract the appropriate value
// for threshold comparison.
//
// All numeric metric fields use float64 for uniform comparison logic, even for
// naturally integer-valued metrics like queue depth and failure counts. This
// avoids type conversion overhead during evaluation.
type MetricSnapshot struct {
	// Throughput is the current event processing rate in events per second.
	// Maps to the ThroughputDrop alert condition.
	Throughput float64 `json:"throughput"`

	// ErrorRate is the current error rate as a percentage (0-100).
	// Maps to the ErrorRateSpike alert condition.
	ErrorRate float64 `json:"error_rate"`

	// DeliveryFailureCount is the current count of failed event deliveries.
	// Maps to the DeliveryFailures alert condition.
	DeliveryFailureCount float64 `json:"delivery_failure_count"`

	// WarehouseLatencySeconds is the current warehouse upload latency in seconds.
	// Maps to the WarehouseLatency alert condition.
	WarehouseLatencySeconds float64 `json:"warehouse_latency_seconds"`

	// JobsDBQueueDepth is the current number of pending jobs in the JobsDB.
	// Maps to the JobsDBQueueDepth alert condition.
	JobsDBQueueDepth float64 `json:"jobsdb_queue_depth"`

	// CollectedAt records when this snapshot was captured, enabling staleness
	// detection for metric freshness validation.
	CollectedAt time.Time `json:"collected_at"`
}

// ---------------------------------------------------------------------------
// Helper functions — private metric extraction and threshold comparison
// ---------------------------------------------------------------------------

// getMetricValue extracts the metric value from a MetricSnapshot that corresponds
// to the given AlertCondition. Returns an error for unrecognized condition types.
func getMetricValue(condition AlertCondition, snapshot MetricSnapshot) (float64, error) {
	switch condition {
	case ThroughputDrop:
		return snapshot.Throughput, nil
	case ErrorRateSpike:
		return snapshot.ErrorRate, nil
	case DeliveryFailures:
		return snapshot.DeliveryFailureCount, nil
	case WarehouseLatency:
		return snapshot.WarehouseLatencySeconds, nil
	case JobsDBQueueDepth:
		return snapshot.JobsDBQueueDepth, nil
	default:
		return 0, fmt.Errorf("unknown alert condition: %s", condition)
	}
}

// compareThreshold evaluates whether the given value satisfies the comparison
// operator against the threshold. The comparison is: value <op> threshold.
// Returns an error for unrecognized comparison operators.
func compareThreshold(value float64, op ComparisonOperator, threshold float64) (bool, error) {
	switch op {
	case LessThan:
		return value < threshold, nil
	case GreaterThan:
		return value > threshold, nil
	case LessThanOrEqual:
		return value <= threshold, nil
	case GreaterThanOrEqual:
		return value >= threshold, nil
	case Equal:
		return value == threshold, nil
	default:
		return false, fmt.Errorf("unknown comparison operator: %s", op)
	}
}

// ---------------------------------------------------------------------------
// EvaluateRule — threshold-based rule evaluation
// ---------------------------------------------------------------------------

// EvaluateRule evaluates an alert rule against the current pipeline metric snapshot.
//
// The evaluation follows these steps:
//  1. If the rule is disabled (Enabled == false), return (false, nil) immediately
//     without performing any evaluation.
//  2. Extract the metric value from the snapshot that corresponds to the rule's
//     Condition type using the condition-to-field mapping.
//  3. Compare the extracted metric value against the rule's Threshold using the
//     configured ComparisonOperator.
//
// Returns:
//   - (true, nil) if the rule is triggered (the metric breaches the threshold).
//   - (false, nil) if the rule is not triggered, or if the rule is disabled.
//   - (false, error) if the condition type or comparison operator is unrecognized.
func EvaluateRule(rule AlertRule, snapshot MetricSnapshot) (bool, error) {
	// Step 1: Skip disabled rules without error.
	if !rule.Enabled {
		return false, nil
	}

	// Step 2: Extract the metric value for the rule's condition type.
	metricValue, err := getMetricValue(rule.Condition, snapshot)
	if err != nil {
		return false, err
	}

	// Step 3: Compare the extracted metric against the threshold.
	triggered, err := compareThreshold(metricValue, rule.ComparisonOperator, rule.Threshold)
	if err != nil {
		return false, err
	}

	return triggered, nil
}

// ---------------------------------------------------------------------------
// RuleRepository — CRUD interface for persistent alert rule management
// ---------------------------------------------------------------------------

// RuleRepository provides CRUD operations for alert rules. Implementations
// are expected to be backed by PostgreSQL, following the same persistence
// patterns used by other repository interfaces in the RudderStack codebase
// (e.g., tracking plan storage, functions storage).
//
// All methods accept a context.Context as their first parameter to support
// cancellation, deadline propagation, and tracing across database operations.
type RuleRepository interface {
	// Create stores a new alert rule and returns the assigned unique ID.
	// The rule's ID field is ignored on input; the repository generates
	// a new identifier. CreatedAt and UpdatedAt are set by the repository.
	Create(ctx context.Context, rule AlertRule) (string, error)

	// Get retrieves a single alert rule by its unique ID. Returns an error
	// if the rule does not exist.
	Get(ctx context.Context, id string) (AlertRule, error)

	// Update modifies an existing alert rule. The rule is identified by its
	// ID field. UpdatedAt is refreshed by the repository. Returns an error
	// if the rule does not exist.
	Update(ctx context.Context, rule AlertRule) error

	// Delete removes an alert rule by its unique ID. Returns an error if
	// the rule does not exist.
	Delete(ctx context.Context, id string) error

	// List returns all alert rules belonging to the specified workspace,
	// ordered by creation time (newest first). Returns an empty slice
	// if no rules exist for the workspace.
	List(ctx context.Context, workspaceID string) ([]AlertRule, error)

	// ListEnabled returns all enabled alert rules across all workspaces.
	// This method is used by the alerting engine's evaluation loop to
	// retrieve the set of rules that need periodic evaluation. Returns
	// an empty slice if no enabled rules exist.
	ListEnabled(ctx context.Context) ([]AlertRule, error)
}
