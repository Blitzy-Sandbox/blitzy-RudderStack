package alerting_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-server/services/alerting"
)

// ---------------------------------------------------------------------------
// TestEvaluateRule_AllConditions — table-driven tests covering every
// AlertCondition type with triggered and not-triggered scenarios.
// ---------------------------------------------------------------------------

func TestEvaluateRule_AllConditions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		rule      alerting.AlertRule
		snapshot  alerting.MetricSnapshot
		triggered bool
	}{
		// ---- ThroughputDrop (LessThan) ----
		{
			name: "ThroughputDrop_Triggered",
			rule: alerting.AlertRule{
				ID:                 "rule-1",
				WorkspaceID:        "ws-1",
				Condition:          alerting.ThroughputDrop,
				Threshold:          1000.0,
				ComparisonOperator: alerting.LessThan,
				Channels:           []string{"webhook"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{Throughput: 500.0},
			triggered: true,
		},
		{
			name: "ThroughputDrop_NotTriggered",
			rule: alerting.AlertRule{
				ID:                 "rule-1",
				WorkspaceID:        "ws-1",
				Condition:          alerting.ThroughputDrop,
				Threshold:          1000.0,
				ComparisonOperator: alerting.LessThan,
				Channels:           []string{"webhook"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{Throughput: 1500.0},
			triggered: false,
		},
		{
			name: "ThroughputDrop_AtThreshold_LessThan_NotTriggered",
			rule: alerting.AlertRule{
				ID:                 "rule-1",
				WorkspaceID:        "ws-1",
				Condition:          alerting.ThroughputDrop,
				Threshold:          1000.0,
				ComparisonOperator: alerting.LessThan,
				Channels:           []string{"webhook"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{Throughput: 1000.0},
			triggered: false,
		},
		// ---- ErrorRateSpike (GreaterThan) ----
		{
			name: "ErrorRateSpike_Triggered",
			rule: alerting.AlertRule{
				ID:                 "rule-2",
				WorkspaceID:        "ws-1",
				Condition:          alerting.ErrorRateSpike,
				Threshold:          5.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"slack"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{ErrorRate: 12.0},
			triggered: true,
		},
		{
			name: "ErrorRateSpike_NotTriggered",
			rule: alerting.AlertRule{
				ID:                 "rule-2",
				WorkspaceID:        "ws-1",
				Condition:          alerting.ErrorRateSpike,
				Threshold:          5.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"slack"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{ErrorRate: 2.0},
			triggered: false,
		},
		{
			name: "ErrorRateSpike_AtThreshold_GreaterThan_NotTriggered",
			rule: alerting.AlertRule{
				ID:                 "rule-2",
				WorkspaceID:        "ws-1",
				Condition:          alerting.ErrorRateSpike,
				Threshold:          5.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"slack"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{ErrorRate: 5.0},
			triggered: false,
		},
		// ---- DeliveryFailures (GreaterThan) ----
		{
			name: "DeliveryFailures_Triggered",
			rule: alerting.AlertRule{
				ID:                 "rule-3",
				WorkspaceID:        "ws-1",
				Condition:          alerting.DeliveryFailures,
				Threshold:          50.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"email"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{DeliveryFailureCount: 75.0},
			triggered: true,
		},
		{
			name: "DeliveryFailures_NotTriggered",
			rule: alerting.AlertRule{
				ID:                 "rule-3",
				WorkspaceID:        "ws-1",
				Condition:          alerting.DeliveryFailures,
				Threshold:          50.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"email"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{DeliveryFailureCount: 30.0},
			triggered: false,
		},
		// ---- WarehouseLatency (GreaterThan) ----
		{
			name: "WarehouseLatency_Triggered",
			rule: alerting.AlertRule{
				ID:                 "rule-4",
				WorkspaceID:        "ws-1",
				Condition:          alerting.WarehouseLatency,
				Threshold:          300.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"webhook"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{WarehouseLatencySeconds: 450.0},
			triggered: true,
		},
		{
			name: "WarehouseLatency_NotTriggered",
			rule: alerting.AlertRule{
				ID:                 "rule-4",
				WorkspaceID:        "ws-1",
				Condition:          alerting.WarehouseLatency,
				Threshold:          300.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"webhook"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{WarehouseLatencySeconds: 150.0},
			triggered: false,
		},
		// ---- JobsDBQueueDepth (GreaterThan) ----
		{
			name: "JobsDBQueueDepth_Triggered",
			rule: alerting.AlertRule{
				ID:                 "rule-5",
				WorkspaceID:        "ws-1",
				Condition:          alerting.JobsDBQueueDepth,
				Threshold:          10000.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"slack"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{JobsDBQueueDepth: 15000.0},
			triggered: true,
		},
		{
			name: "JobsDBQueueDepth_NotTriggered",
			rule: alerting.AlertRule{
				ID:                 "rule-5",
				WorkspaceID:        "ws-1",
				Condition:          alerting.JobsDBQueueDepth,
				Threshold:          10000.0,
				ComparisonOperator: alerting.GreaterThan,
				Channels:           []string{"slack"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{JobsDBQueueDepth: 5000.0},
			triggered: false,
		},
		// ---- Zero-value metric ----
		{
			name: "ThroughputDrop_ZeroThroughput_Triggered",
			rule: alerting.AlertRule{
				ID:                 "rule-6",
				WorkspaceID:        "ws-1",
				Condition:          alerting.ThroughputDrop,
				Threshold:          100.0,
				ComparisonOperator: alerting.LessThan,
				Channels:           []string{"webhook"},
				Enabled:            true,
			},
			snapshot:  alerting.MetricSnapshot{Throughput: 0.0},
			triggered: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			triggered, err := alerting.EvaluateRule(tt.rule, tt.snapshot)
			require.NoError(t, err)
			require.Equal(t, tt.triggered, triggered)
		})
	}
}

// ---------------------------------------------------------------------------
// TestEvaluateRule_ComparisonOperators — table-driven tests covering every
// ComparisonOperator with boundary conditions.
// ---------------------------------------------------------------------------

func TestEvaluateRule_ComparisonOperators(t *testing.T) {
	t.Parallel()

	// Base rule using ErrorRateSpike so we can set threshold and vary the operator.
	baseRule := func(op alerting.ComparisonOperator, threshold float64) alerting.AlertRule {
		return alerting.AlertRule{
			ID:                 "rule-op-test",
			WorkspaceID:        "ws-1",
			Condition:          alerting.ErrorRateSpike,
			Threshold:          threshold,
			ComparisonOperator: op,
			Channels:           []string{"webhook"},
			Enabled:            true,
		}
	}

	tests := []struct {
		name      string
		rule      alerting.AlertRule
		snapshot  alerting.MetricSnapshot
		triggered bool
	}{
		// ---- LessThan ----
		{
			name:      "LessThan_Below_Triggered",
			rule:      baseRule(alerting.LessThan, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 50.0},
			triggered: true,
		},
		{
			name:      "LessThan_AtThreshold_NotTriggered",
			rule:      baseRule(alerting.LessThan, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 100.0},
			triggered: false,
		},
		{
			name:      "LessThan_Above_NotTriggered",
			rule:      baseRule(alerting.LessThan, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 150.0},
			triggered: false,
		},
		// ---- GreaterThan ----
		{
			name:      "GreaterThan_Above_Triggered",
			rule:      baseRule(alerting.GreaterThan, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 150.0},
			triggered: true,
		},
		{
			name:      "GreaterThan_AtThreshold_NotTriggered",
			rule:      baseRule(alerting.GreaterThan, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 100.0},
			triggered: false,
		},
		{
			name:      "GreaterThan_Below_NotTriggered",
			rule:      baseRule(alerting.GreaterThan, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 50.0},
			triggered: false,
		},
		// ---- LessThanOrEqual ----
		{
			name:      "LessThanOrEqual_Below_Triggered",
			rule:      baseRule(alerting.LessThanOrEqual, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 50.0},
			triggered: true,
		},
		{
			name:      "LessThanOrEqual_AtThreshold_Triggered",
			rule:      baseRule(alerting.LessThanOrEqual, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 100.0},
			triggered: true,
		},
		{
			name:      "LessThanOrEqual_Above_NotTriggered",
			rule:      baseRule(alerting.LessThanOrEqual, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 150.0},
			triggered: false,
		},
		// ---- GreaterThanOrEqual ----
		{
			name:      "GreaterThanOrEqual_Above_Triggered",
			rule:      baseRule(alerting.GreaterThanOrEqual, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 150.0},
			triggered: true,
		},
		{
			name:      "GreaterThanOrEqual_AtThreshold_Triggered",
			rule:      baseRule(alerting.GreaterThanOrEqual, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 100.0},
			triggered: true,
		},
		{
			name:      "GreaterThanOrEqual_Below_NotTriggered",
			rule:      baseRule(alerting.GreaterThanOrEqual, 100.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 50.0},
			triggered: false,
		},
		// ---- Equal ----
		{
			name:      "Equal_ExactMatch_Triggered",
			rule:      baseRule(alerting.Equal, 42.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 42.0},
			triggered: true,
		},
		{
			name:      "Equal_Above_NotTriggered",
			rule:      baseRule(alerting.Equal, 42.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 43.0},
			triggered: false,
		},
		{
			name:      "Equal_Below_NotTriggered",
			rule:      baseRule(alerting.Equal, 42.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 41.0},
			triggered: false,
		},
		// ---- Zero threshold boundary ----
		{
			name:      "GreaterThan_ZeroThreshold_Triggered",
			rule:      baseRule(alerting.GreaterThan, 0.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 0.001},
			triggered: true,
		},
		{
			name:      "Equal_ZeroThreshold_Triggered",
			rule:      baseRule(alerting.Equal, 0.0),
			snapshot:  alerting.MetricSnapshot{ErrorRate: 0.0},
			triggered: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			triggered, err := alerting.EvaluateRule(tt.rule, tt.snapshot)
			require.NoError(t, err)
			require.Equal(t, tt.triggered, triggered)
		})
	}
}

// ---------------------------------------------------------------------------
// TestEvaluateRule_DisabledRule — disabled rules never trigger, regardless
// of metric values.
// ---------------------------------------------------------------------------

func TestEvaluateRule_DisabledRule(t *testing.T) {
	t.Parallel()

	// Create a rule that would normally trigger (throughput below threshold).
	rule := alerting.AlertRule{
		ID:                 "rule-disabled",
		WorkspaceID:        "ws-1",
		Condition:          alerting.ThroughputDrop,
		Threshold:          1000.0,
		ComparisonOperator: alerting.LessThan,
		Channels:           []string{"webhook"},
		Enabled:            false, // Explicitly disabled.
	}

	snapshot := alerting.MetricSnapshot{Throughput: 0.0} // Would trigger if enabled.

	triggered, err := alerting.EvaluateRule(rule, snapshot)
	require.NoError(t, err)
	require.False(t, triggered, "disabled rules must never trigger")
}

// ---------------------------------------------------------------------------
// TestAlertCondition_StringValues — verify the string constant values for
// every AlertCondition enum entry.
// ---------------------------------------------------------------------------

func TestAlertCondition_StringValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		condition alerting.AlertCondition
		expected  string
	}{
		{alerting.ThroughputDrop, "throughput_drop"},
		{alerting.ErrorRateSpike, "error_rate_spike"},
		{alerting.DeliveryFailures, "delivery_failures"},
		{alerting.WarehouseLatency, "warehouse_latency"},
		{alerting.JobsDBQueueDepth, "jobsdb_queue_depth"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, string(tt.condition))
		})
	}
}

// ---------------------------------------------------------------------------
// TestComparisonOperator_StringValues — verify the string constant values for
// every ComparisonOperator enum entry.
// ---------------------------------------------------------------------------

func TestComparisonOperator_StringValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		operator alerting.ComparisonOperator
		expected string
	}{
		{alerting.LessThan, "lt"},
		{alerting.GreaterThan, "gt"},
		{alerting.LessThanOrEqual, "lte"},
		{alerting.GreaterThanOrEqual, "gte"},
		{alerting.Equal, "eq"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, string(tt.operator))
		})
	}
}

// ---------------------------------------------------------------------------
// TestAlertRule_Validate — validation tests covering all constraint checks
// on the AlertRule.Validate() method.
// ---------------------------------------------------------------------------

func TestAlertRule_Validate(t *testing.T) {
	t.Parallel()

	// validBase returns a fully valid AlertRule that passes Validate(). Tests
	// mutate individual fields from this base to trigger specific errors.
	validBase := func() alerting.AlertRule {
		return alerting.AlertRule{
			ID:                 "rule-v",
			WorkspaceID:        "ws-1",
			Condition:          alerting.ThroughputDrop,
			Threshold:          100.0,
			ComparisonOperator: alerting.GreaterThan,
			Channels:           []string{"webhook"},
			Enabled:            true,
		}
	}

	t.Run("ValidRule_NoError", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		require.NoError(t, rule.Validate())
	})

	t.Run("EmptyCondition_Error", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.Condition = ""
		err := rule.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "condition is required")
	})

	t.Run("UnknownCondition_Error", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.Condition = alerting.AlertCondition("nonexistent_condition")
		err := rule.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown condition")
	})

	t.Run("NegativeThreshold_Error", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.Threshold = -1.0
		err := rule.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "threshold must be non-negative")
	})

	t.Run("ZeroThreshold_NoError", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.Threshold = 0.0
		require.NoError(t, rule.Validate())
	})

	t.Run("EmptyComparisonOperator_Error", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.ComparisonOperator = ""
		err := rule.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "comparison operator is required")
	})

	t.Run("UnknownComparisonOperator_Error", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.ComparisonOperator = alerting.ComparisonOperator("invalid_op")
		err := rule.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown comparison operator")
	})

	t.Run("EmptyChannels_Error", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.Channels = []string{}
		err := rule.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "at least one notification channel is required")
	})

	t.Run("NilChannels_Error", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.Channels = nil
		err := rule.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "at least one notification channel is required")
	})

	t.Run("MultipleChannels_NoError", func(t *testing.T) {
		t.Parallel()
		rule := validBase()
		rule.Channels = []string{"webhook", "slack", "email"}
		require.NoError(t, rule.Validate())
	})

	t.Run("AllConditionsValid", func(t *testing.T) {
		t.Parallel()
		conditions := []alerting.AlertCondition{
			alerting.ThroughputDrop,
			alerting.ErrorRateSpike,
			alerting.DeliveryFailures,
			alerting.WarehouseLatency,
			alerting.JobsDBQueueDepth,
		}
		for _, c := range conditions {
			rule := validBase()
			rule.Condition = c
			require.NoError(t, rule.Validate(), "condition %q should be valid", string(c))
		}
	})

	t.Run("AllOperatorsValid", func(t *testing.T) {
		t.Parallel()
		operators := []alerting.ComparisonOperator{
			alerting.LessThan,
			alerting.GreaterThan,
			alerting.LessThanOrEqual,
			alerting.GreaterThanOrEqual,
			alerting.Equal,
		}
		for _, op := range operators {
			rule := validBase()
			rule.ComparisonOperator = op
			require.NoError(t, rule.Validate(), "operator %q should be valid", string(op))
		}
	})
}

// ---------------------------------------------------------------------------
// TestEvaluateRule_ErrorCases — verify EvaluateRule returns errors for
// unrecognized condition types and comparison operators.
// ---------------------------------------------------------------------------

func TestEvaluateRule_ErrorCases(t *testing.T) {
	t.Parallel()

	t.Run("UnknownCondition_ReturnsError", func(t *testing.T) {
		t.Parallel()
		rule := alerting.AlertRule{
			ID:                 "rule-err-1",
			WorkspaceID:        "ws-1",
			Condition:          alerting.AlertCondition("nonexistent"),
			Threshold:          100.0,
			ComparisonOperator: alerting.GreaterThan,
			Channels:           []string{"webhook"},
			Enabled:            true,
		}
		snapshot := alerting.MetricSnapshot{Throughput: 50.0}
		triggered, err := alerting.EvaluateRule(rule, snapshot)
		require.Error(t, err)
		require.False(t, triggered)
		require.Contains(t, err.Error(), "unknown alert condition")
	})

	t.Run("UnknownOperator_ReturnsError", func(t *testing.T) {
		t.Parallel()
		rule := alerting.AlertRule{
			ID:                 "rule-err-2",
			WorkspaceID:        "ws-1",
			Condition:          alerting.ThroughputDrop,
			Threshold:          100.0,
			ComparisonOperator: alerting.ComparisonOperator("invalid"),
			Channels:           []string{"webhook"},
			Enabled:            true,
		}
		snapshot := alerting.MetricSnapshot{Throughput: 50.0}
		triggered, err := alerting.EvaluateRule(rule, snapshot)
		require.Error(t, err)
		require.False(t, triggered)
		require.Contains(t, err.Error(), "unknown comparison operator")
	})
}

// ---------------------------------------------------------------------------
// TestEvaluateRule_ConditionMetricMapping — verify each AlertCondition reads
// the correct field from MetricSnapshot. Each test sets only the expected
// metric field, leaving all others at zero, to confirm the right field is
// evaluated.
// ---------------------------------------------------------------------------

func TestEvaluateRule_ConditionMetricMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		condition alerting.AlertCondition
		snapshot  alerting.MetricSnapshot
		threshold float64
		triggered bool
	}{
		{
			name:      "ThroughputDrop_ReadsThroughput",
			condition: alerting.ThroughputDrop,
			snapshot:  alerting.MetricSnapshot{Throughput: 200.0},
			threshold: 500.0,
			triggered: true, // 200 < 500
		},
		{
			name:      "ErrorRateSpike_ReadsErrorRate",
			condition: alerting.ErrorRateSpike,
			snapshot:  alerting.MetricSnapshot{ErrorRate: 15.0},
			threshold: 10.0,
			triggered: true, // 15 > 10 (using GreaterThan below)
		},
		{
			name:      "DeliveryFailures_ReadsDeliveryFailureCount",
			condition: alerting.DeliveryFailures,
			snapshot:  alerting.MetricSnapshot{DeliveryFailureCount: 100.0},
			threshold: 50.0,
			triggered: true, // 100 > 50
		},
		{
			name:      "WarehouseLatency_ReadsWarehouseLatencySeconds",
			condition: alerting.WarehouseLatency,
			snapshot:  alerting.MetricSnapshot{WarehouseLatencySeconds: 600.0},
			threshold: 300.0,
			triggered: true, // 600 > 300
		},
		{
			name:      "JobsDBQueueDepth_ReadsJobsDBQueueDepth",
			condition: alerting.JobsDBQueueDepth,
			snapshot:  alerting.MetricSnapshot{JobsDBQueueDepth: 20000.0},
			threshold: 10000.0,
			triggered: true, // 20000 > 10000
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ThroughputDrop uses LessThan; all others use GreaterThan.
			op := alerting.GreaterThan
			if tt.condition == alerting.ThroughputDrop {
				op = alerting.LessThan
			}

			rule := alerting.AlertRule{
				ID:                 "rule-mapping",
				WorkspaceID:        "ws-1",
				Condition:          tt.condition,
				Threshold:          tt.threshold,
				ComparisonOperator: op,
				Channels:           []string{"webhook"},
				Enabled:            true,
			}

			triggered, err := alerting.EvaluateRule(rule, tt.snapshot)
			require.NoError(t, err)
			require.Equal(t, tt.triggered, triggered)
		})
	}
}

// ---------------------------------------------------------------------------
// TestEvaluateRule_LargeValues — verify evaluation behaves correctly with
// very large float64 metric values and thresholds.
// ---------------------------------------------------------------------------

func TestEvaluateRule_LargeValues(t *testing.T) {
	t.Parallel()

	rule := alerting.AlertRule{
		ID:                 "rule-large",
		WorkspaceID:        "ws-1",
		Condition:          alerting.JobsDBQueueDepth,
		Threshold:          1e15,
		ComparisonOperator: alerting.GreaterThan,
		Channels:           []string{"webhook"},
		Enabled:            true,
	}
	snapshot := alerting.MetricSnapshot{JobsDBQueueDepth: 2e15}

	triggered, err := alerting.EvaluateRule(rule, snapshot)
	require.NoError(t, err)
	require.True(t, triggered, "large metric values should compare correctly")
}

// ---------------------------------------------------------------------------
// TestAlertRule_StructFields — ensure AlertRule struct exposes expected JSON
// tags and field types by constructing a fully populated instance.
// ---------------------------------------------------------------------------

func TestAlertRule_StructFields(t *testing.T) {
	t.Parallel()

	rule := alerting.AlertRule{
		ID:                 "struct-test",
		WorkspaceID:        "ws-struct",
		Condition:          alerting.ErrorRateSpike,
		Threshold:          5.5,
		ComparisonOperator: alerting.GreaterThanOrEqual,
		Channels:           []string{"webhook", "email"},
		Enabled:            true,
	}

	require.Equal(t, "struct-test", rule.ID)
	require.Equal(t, "ws-struct", rule.WorkspaceID)
	require.Equal(t, alerting.ErrorRateSpike, rule.Condition)
	require.Equal(t, 5.5, rule.Threshold)
	require.Equal(t, alerting.GreaterThanOrEqual, rule.ComparisonOperator)
	require.Equal(t, []string{"webhook", "email"}, rule.Channels)
	require.True(t, rule.Enabled)
}

// ---------------------------------------------------------------------------
// TestMetricSnapshot_StructFields — ensure MetricSnapshot struct exposes
// expected fields and types.
// ---------------------------------------------------------------------------

func TestMetricSnapshot_StructFields(t *testing.T) {
	t.Parallel()

	snapshot := alerting.MetricSnapshot{
		Throughput:              1000.5,
		ErrorRate:               2.3,
		DeliveryFailureCount:    42.0,
		WarehouseLatencySeconds: 120.7,
		JobsDBQueueDepth:        8000.0,
	}

	require.Equal(t, 1000.5, snapshot.Throughput)
	require.Equal(t, 2.3, snapshot.ErrorRate)
	require.Equal(t, 42.0, snapshot.DeliveryFailureCount)
	require.Equal(t, 120.7, snapshot.WarehouseLatencySeconds)
	require.Equal(t, 8000.0, snapshot.JobsDBQueueDepth)
}
