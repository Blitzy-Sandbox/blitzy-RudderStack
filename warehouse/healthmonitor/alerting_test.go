package healthmonitor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"
)

// testHealthSummary creates a HealthSummaryResponse with a single source-destination
// pair for testing. The destination's fields can be customised via functional options
// applied to a zero-valued DestinationHealth before it is wrapped in the response.
func testHealthSummary(sourceID, destID string, opts ...func(*DestinationHealth)) *HealthSummaryResponse {
	dest := &DestinationHealth{
		DestID:   destID,
		DestType: "SNOWFLAKE",
	}
	for _, opt := range opts {
		opt(dest)
	}
	return &HealthSummaryResponse{
		Sources: []*SourceHealth{
			{
				SourceID:     sourceID,
				SourceType:   "test_source",
				WorkspaceID:  "test_workspace",
				Destinations: []*DestinationHealth{dest},
			},
		},
	}
}

// TestAlertingEvaluator_EvaluateThresholds validates that the AlertingEvaluator
// correctly detects threshold crossings for each of the four alert types and does
// NOT emit alerts when the metrics are within acceptable ranges.
func TestAlertingEvaluator_EvaluateThresholds(t *testing.T) {
	const (
		sourceID = "source_1"
		destID   = "dest_1"
	)

	alertTags := func(alertType AlertType) stats.Tags {
		return stats.Tags{
			"module":      "warehouse",
			"workspaceId": "test_workspace",
			"alertType":   string(alertType),
			"sourceID":    sourceID,
			"destID":      destID,
			"destType":    "SNOWFLAKE",
			"sourceType":  "test_source",
		}
	}

	tests := []struct {
		name           string
		setupDest      func(*DestinationHealth)
		expectAlerts   []AlertType
		expectNoAlerts []AlertType
	}{
		{
			name: "failure rate exceeds threshold",
			setupDest: func(d *DestinationHealth) {
				d.ErrorRate = 0.5 // > default threshold 0.1
			},
			expectAlerts:   []AlertType{AlertTypeFailureRate},
			expectNoAlerts: []AlertType{AlertTypeDurationSpike, AlertTypeRowCountAnomaly, AlertTypeSchemaDrift},
		},
		{
			name: "failure rate below threshold",
			setupDest: func(d *DestinationHealth) {
				d.ErrorRate = 0.05 // < default threshold 0.1
			},
			expectAlerts:   nil,
			expectNoAlerts: []AlertType{AlertTypeFailureRate, AlertTypeDurationSpike, AlertTypeRowCountAnomaly, AlertTypeSchemaDrift},
		},
		{
			name: "duration spike detection",
			setupDest: func(d *DestinationHealth) {
				d.SyncDuration = DurationStats{Avg: 400000} // > default threshold 300000 ms
			},
			expectAlerts:   []AlertType{AlertTypeDurationSpike},
			expectNoAlerts: []AlertType{AlertTypeFailureRate, AlertTypeRowCountAnomaly, AlertTypeSchemaDrift},
		},
		{
			name: "duration within acceptable range",
			setupDest: func(d *DestinationHealth) {
				d.SyncDuration = DurationStats{Avg: 200000} // < default threshold 300000 ms
			},
			expectAlerts:   nil,
			expectNoAlerts: []AlertType{AlertTypeFailureRate, AlertTypeDurationSpike, AlertTypeRowCountAnomaly, AlertTypeSchemaDrift},
		},
		{
			name: "row count anomaly detection",
			setupDest: func(d *DestinationHealth) {
				// Default drop threshold is 50%.
				// thresholdRows = 1000 * (1 - 50/100) = 500
				// 100 < 500 → anomaly detected.
				d.RowsSynced = 100
				d.PreviousRowsSynced = 1000
			},
			expectAlerts:   []AlertType{AlertTypeRowCountAnomaly},
			expectNoAlerts: []AlertType{AlertTypeFailureRate, AlertTypeDurationSpike, AlertTypeSchemaDrift},
		},
		{
			name: "schema drift detection",
			setupDest: func(d *DestinationHealth) {
				d.SchemaChanges = 3 // > 0 with schemaDriftEnabled=true (default)
			},
			expectAlerts:   []AlertType{AlertTypeSchemaDrift},
			expectNoAlerts: []AlertType{AlertTypeFailureRate, AlertTypeDurationSpike, AlertTypeRowCountAnomaly},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			statsStore, err := memstats.New()
			require.NoError(t, err)

			conf := config.New()
			evaluator := NewAlertingEvaluator(conf, logger.NOP, statsStore)

			summary := testHealthSummary(sourceID, destID, tc.setupDest)
			evaluator.Evaluate(summary)

			// Verify expected alerts were emitted with exactly one increment.
			for _, alertType := range tc.expectAlerts {
				m := statsStore.Get(alertMetricName, alertTags(alertType))
				require.NotNil(t, m, "expected alert %s to be emitted", alertType)
				require.EqualValues(t, 1, m.LastValue(), "expected exactly one %s alert", alertType)
			}

			// Verify alerts that should not fire are absent.
			for _, alertType := range tc.expectNoAlerts {
				m := statsStore.Get(alertMetricName, alertTags(alertType))
				require.Nil(t, m, "expected alert %s to NOT be emitted", alertType)
			}
		})
	}
}

// TestAlertingEvaluator_Cooldown validates that the evaluator respects its
// configurable cooldown period:
//   - Repeated evaluations within the cooldown window must NOT re-emit alerts.
//   - After the cooldown expires, the same alert can fire again.
//   - Different alert types maintain independent cooldown timers.
func TestAlertingEvaluator_Cooldown(t *testing.T) {
	const (
		sourceID = "source_cooldown"
		destID   = "dest_cooldown"
	)

	failureRateTags := stats.Tags{
		"module":      "warehouse",
		"workspaceId": "test_workspace",
		"alertType":   string(AlertTypeFailureRate),
		"sourceID":    sourceID,
		"destID":      destID,
		"destType":    "SNOWFLAKE",
		"sourceType":  "test_source",
	}

	t.Run("alert suppressed during cooldown period", func(t *testing.T) {
		statsStore, err := memstats.New()
		require.NoError(t, err)

		conf := config.New()
		conf.Set("Warehouse.healthMonitor.alerting.cooldownMinutes", 5)

		evaluator := NewAlertingEvaluator(conf, logger.NOP, statsStore)

		// Inject deterministic time — stays fixed at noon.
		currentTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		evaluator.now = func() time.Time { return currentTime }

		summary := testHealthSummary(sourceID, destID, func(d *DestinationHealth) {
			d.ErrorRate = 0.5 // exceeds default threshold 0.1
		})

		// First evaluation — no prior alert, should fire.
		evaluator.Evaluate(summary)
		m := statsStore.Get(alertMetricName, failureRateTags)
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())

		// Second evaluation — same time (within cooldown), must NOT re-fire.
		evaluator.Evaluate(summary)
		m = statsStore.Get(alertMetricName, failureRateTags)
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue(), "alert should be suppressed during cooldown")
	})

	t.Run("alert re-fires after cooldown expires", func(t *testing.T) {
		statsStore, err := memstats.New()
		require.NoError(t, err)

		conf := config.New()
		conf.Set("Warehouse.healthMonitor.alerting.cooldownMinutes", 5)

		evaluator := NewAlertingEvaluator(conf, logger.NOP, statsStore)

		currentTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		evaluator.now = func() time.Time { return currentTime }

		summary := testHealthSummary(sourceID, destID, func(d *DestinationHealth) {
			d.ErrorRate = 0.5
		})

		// First evaluation — fires.
		evaluator.Evaluate(summary)
		m := statsStore.Get(alertMetricName, failureRateTags)
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())

		// Advance time past the 5-minute cooldown.
		currentTime = currentTime.Add(5*time.Minute + time.Second)

		// Second evaluation — cooldown expired, must re-fire.
		evaluator.Evaluate(summary)
		m = statsStore.Get(alertMetricName, failureRateTags)
		require.NotNil(t, m)
		require.EqualValues(t, 2, m.LastValue(), "alert should re-fire after cooldown expires")
	})

	t.Run("different alert types have independent cooldowns", func(t *testing.T) {
		statsStore, err := memstats.New()
		require.NoError(t, err)

		conf := config.New()
		conf.Set("Warehouse.healthMonitor.alerting.cooldownMinutes", 5)

		evaluator := NewAlertingEvaluator(conf, logger.NOP, statsStore)

		currentTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		evaluator.now = func() time.Time { return currentTime }

		// First evaluation: trigger failure_rate only.
		summary1 := testHealthSummary(sourceID, destID, func(d *DestinationHealth) {
			d.ErrorRate = 0.5 // exceeds threshold
		})
		evaluator.Evaluate(summary1)
		m := statsStore.Get(alertMetricName, failureRateTags)
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())

		// Second evaluation (same time): trigger failure_rate (should be in cooldown)
		// AND duration_spike (should fire independently — different alert key).
		summary2 := testHealthSummary(sourceID, destID, func(d *DestinationHealth) {
			d.ErrorRate = 0.5                           // still above threshold
			d.SyncDuration = DurationStats{Avg: 400000} // exceeds threshold
		})
		evaluator.Evaluate(summary2)

		durationSpikeTags := stats.Tags{
			"module":      "warehouse",
			"workspaceId": "test_workspace",
			"alertType":   string(AlertTypeDurationSpike),
			"sourceID":    sourceID,
			"destID":      destID,
			"destType":    "SNOWFLAKE",
			"sourceType":  "test_source",
		}

		// Failure rate stays at 1 (cooldown suppressed the second emission).
		m = statsStore.Get(alertMetricName, failureRateTags)
		require.EqualValues(t, 1, m.LastValue(), "failure_rate should remain suppressed by cooldown")

		// Duration spike fires independently.
		m2 := statsStore.Get(alertMetricName, durationSpikeTags)
		require.NotNil(t, m2, "duration_spike should fire independently of failure_rate cooldown")
		require.EqualValues(t, 1, m2.LastValue())
	})
}

// TestAlertingEvaluator_AlertSuppression verifies that configurable suppression
// flags correctly prevent specific alert types from firing while other types
// remain unaffected.
func TestAlertingEvaluator_AlertSuppression(t *testing.T) {
	const (
		sourceID = "source_suppress"
		destID   = "dest_suppress"
	)

	alertTags := func(alertType AlertType) stats.Tags {
		return stats.Tags{
			"module":      "warehouse",
			"workspaceId": "test_workspace",
			"alertType":   string(alertType),
			"sourceID":    sourceID,
			"destID":      destID,
			"destType":    "SNOWFLAKE",
			"sourceType":  "test_source",
		}
	}

	t.Run("schema drift suppression does not affect other alerts", func(t *testing.T) {
		statsStore, err := memstats.New()
		require.NoError(t, err)

		conf := config.New()
		conf.Set("Warehouse.healthMonitor.alerting.schemaDriftEnabled", false)

		evaluator := NewAlertingEvaluator(conf, logger.NOP, statsStore)

		// Build summary that breaches ALL four thresholds.
		summary := testHealthSummary(sourceID, destID, func(d *DestinationHealth) {
			d.ErrorRate = 0.5                           // failure rate breach
			d.SyncDuration = DurationStats{Avg: 400000} // duration spike breach
			d.RowsSynced = 100                          // row count anomaly (90% drop)
			d.PreviousRowsSynced = 1000                 // baseline for anomaly calc
			d.SchemaChanges = 5                         // schema drift condition
		})

		evaluator.Evaluate(summary)

		// Failure rate, duration spike, and row count anomaly should fire.
		for _, alertType := range []AlertType{
			AlertTypeFailureRate,
			AlertTypeDurationSpike,
			AlertTypeRowCountAnomaly,
		} {
			m := statsStore.Get(alertMetricName, alertTags(alertType))
			require.NotNil(t, m, "expected alert %s to be emitted", alertType)
			require.EqualValues(t, 1, m.LastValue(), "expected exactly one %s alert", alertType)
		}

		// Schema drift must be suppressed.
		m := statsStore.Get(alertMetricName, alertTags(AlertTypeSchemaDrift))
		require.Nil(t, m, "schema_drift alert should be suppressed when schemaDriftEnabled is false")
	})

	t.Run("schema drift fires when enabled", func(t *testing.T) {
		statsStore, err := memstats.New()
		require.NoError(t, err)

		conf := config.New()
		// schemaDriftEnabled defaults to true — set explicitly for clarity.
		conf.Set("Warehouse.healthMonitor.alerting.schemaDriftEnabled", true)

		evaluator := NewAlertingEvaluator(conf, logger.NOP, statsStore)

		summary := testHealthSummary(sourceID, destID, func(d *DestinationHealth) {
			d.SchemaChanges = 3
		})

		evaluator.Evaluate(summary)

		m := statsStore.Get(alertMetricName, alertTags(AlertTypeSchemaDrift))
		require.NotNil(t, m, "schema_drift alert should fire when schemaDriftEnabled is true")
		require.EqualValues(t, 1, m.LastValue())
	})

	t.Run("no alerts emitted for nil summary", func(t *testing.T) {
		statsStore, err := memstats.New()
		require.NoError(t, err)

		conf := config.New()
		evaluator := NewAlertingEvaluator(conf, logger.NOP, statsStore)

		evaluator.Evaluate(nil)

		// No metrics should be recorded when the summary is nil.
		for _, alertType := range []AlertType{
			AlertTypeFailureRate,
			AlertTypeDurationSpike,
			AlertTypeRowCountAnomaly,
			AlertTypeSchemaDrift,
		} {
			m := statsStore.Get(alertMetricName, alertTags(alertType))
			require.Nil(t, m, "no %s alert should be emitted for nil summary", alertType)
		}
	})

	t.Run("row count anomaly skipped when no previous baseline", func(t *testing.T) {
		statsStore, err := memstats.New()
		require.NoError(t, err)

		conf := config.New()
		evaluator := NewAlertingEvaluator(conf, logger.NOP, statsStore)

		summary := testHealthSummary(sourceID, destID, func(d *DestinationHealth) {
			d.RowsSynced = 100
			d.PreviousRowsSynced = 0 // no baseline — anomaly check must be skipped
		})

		evaluator.Evaluate(summary)

		m := statsStore.Get(alertMetricName, alertTags(AlertTypeRowCountAnomaly))
		require.Nil(t, m, "row_count_anomaly should not fire without previous baseline")
	})
}
