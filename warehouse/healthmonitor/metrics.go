package healthmonitor

import (
	"github.com/rudderlabs/rudder-go-kit/stats"
)

// Prometheus metric name constants matching the AAP specification for
// warehouse health monitoring. These names are emitted as tagged stats
// via statsFactory.NewTaggedStat() following the exact instrumentation
// pattern established in warehouse/router/upload_stats.go.
const (
	// metricSyncDuration is a histogram tracking warehouse sync duration in seconds.
	// Durations are converted from milliseconds (as stored in wh_sync_health) to
	// seconds before observation, matching Prometheus histogram conventions.
	metricSyncDuration = "warehouse_sync_duration_seconds"

	// metricSyncRowsTotal is a counter tracking total rows synced across all uploads.
	// Incremented by the number of rows successfully synced for each upload.
	metricSyncRowsTotal = "warehouse_sync_rows_total"

	// metricSyncErrorsTotal is a counter tracking total sync errors, labeled by
	// error_category. The error_category tag classifies the failure type, mapping
	// to the JobErrorType constants from warehouse/internal/model/upload.go
	// (e.g., "permission_error", "column_count_error", "uncategorised").
	metricSyncErrorsTotal = "warehouse_sync_errors_total"

	// metricSyncStatus is a gauge indicating the current sync status per
	// source/destination pair. Values: 1.0 = healthy, 0.0 = unhealthy.
	// A source/destination pair is considered unhealthy when its error rate
	// exceeds 10% (0.1 threshold).
	metricSyncStatus = "warehouse_sync_status"

	// metricSchemaChangesTotal is a counter tracking the number of schema changes
	// detected during warehouse sync operations. Incremented when schema drift
	// is detected between consecutive syncs.
	metricSchemaChangesTotal = "warehouse_schema_changes_total"
)

// unhealthyErrorRateThreshold defines the error rate above which a
// source/destination pair is considered unhealthy for the sync status gauge.
// A value of 0.1 means >10% error rate triggers unhealthy status.
const unhealthyErrorRateThreshold = 0.1

// HealthMetrics handles Prometheus metric emission for warehouse health monitoring.
// It provides two emission entry points:
//
//   - EmitMetrics: called periodically by the HealthMonitor's collection loop to
//     emit aggregate metrics from the health summary across all source/destination pairs.
//   - EmitUploadMetrics: called per-upload from the warehouse upload pipeline's
//     success/failure paths to emit real-time metrics for individual sync completions.
//
// Both methods follow the stats instrumentation pattern from warehouse/router/upload_stats.go,
// using statsFactory.NewTaggedStat() with the standard warehouse tag set.
type HealthMetrics struct {
	statsFactory stats.Stats
}

// NewHealthMetrics creates a new HealthMetrics instance with the given stats factory.
// The stats factory is used to create tagged stat measurements for Prometheus metric
// emission. Pass the same stats.Stats instance used by the warehouse router to ensure
// consistent metric collection.
func NewHealthMetrics(statsFactory stats.Stats) *HealthMetrics {
	return &HealthMetrics{
		statsFactory: statsFactory,
	}
}

// buildTags creates the standard tag set for health monitoring metrics.
// Tags follow the pattern established in warehouse/router/upload_stats.go buildTags():
//
//	module      — always "warehouse"
//	sourceID    — RudderStack source identifier
//	destID      — warehouse destination identifier
//	destType    — warehouse destination type (e.g., "SNOWFLAKE", "BQ", "RS")
//	sourceType  — source type (e.g., "web", "android", "ios", "cloud")
//
// Additional tags can be provided via the variadic extraTags parameter.
// Extra tags are merged into the base tag set, with later values overwriting
// earlier ones for duplicate keys.
func (m *HealthMetrics) buildTags(workspaceID, sourceID, destID, destType, sourceType string, extraTags ...stats.Tags) stats.Tags {
	tags := stats.Tags{
		"module":      "warehouse",
		"workspaceId": workspaceID,
		"sourceID":    sourceID,
		"destID":      destID,
		"destType":    destType,
		"sourceType":  sourceType,
	}
	for _, extra := range extraTags {
		for k, v := range extra {
			tags[k] = v
		}
	}
	return tags
}

// EmitMetrics emits Prometheus metrics for each source/destination health data point
// in the given summary. This method is called by the HealthMonitor's periodic collection
// loop (monitor.go collect()) to emit aggregate metrics from the health summary.
//
// For each source/destination pair in the summary, the following metrics are emitted:
//   - warehouse_sync_duration_seconds (histogram): average sync duration converted from ms to seconds
//   - warehouse_sync_rows_total (counter): total rows synced
//   - warehouse_sync_errors_total (counter): total errors with error_category label (only if errors exist)
//   - warehouse_sync_status (gauge): 1.0 for healthy, 0.0 for unhealthy (>10% error rate)
//   - warehouse_schema_changes_total (counter): number of schema changes (only if changes detected)
//
// A nil summary is a no-op, allowing safe invocation when no health data is available.
func (m *HealthMetrics) EmitMetrics(summary *HealthSummaryResponse) {
	if summary == nil {
		return
	}

	for _, source := range summary.Sources {
		if source == nil {
			continue
		}
		for _, dest := range source.Destinations {
			if dest == nil {
				continue
			}

			tags := m.buildTags(source.WorkspaceID, source.SourceID, dest.DestID, dest.DestType, source.SourceType)

			// Emit sync duration histogram: convert average duration from ms to seconds.
			// Uses the aggregate average from the health summary, providing a representative
			// duration metric across all uploads in the reporting window.
			m.statsFactory.NewTaggedStat(metricSyncDuration, stats.HistogramType, tags).
				Observe(float64(dest.SyncDuration.Avg) / 1000.0)

			// Emit rows synced counter: total rows successfully synced in the reporting window.
			m.statsFactory.NewTaggedStat(metricSyncRowsTotal, stats.CountType, tags).
				Count(int(dest.RowsSynced))

			// Emit sync errors counter with error_category label for error categorization.
			// Only emitted when there are actual errors to avoid zero-value noise.
			if dest.ErrorCount > 0 {
				errorTags := m.buildTags(
					source.WorkspaceID, source.SourceID, dest.DestID, dest.DestType, source.SourceType,
					stats.Tags{"error_category": dest.ErrorCategory},
				)
				m.statsFactory.NewTaggedStat(metricSyncErrorsTotal, stats.CountType, errorTags).
					Count(int(dest.ErrorCount))
			}

			// Emit sync status gauge: 1.0 = healthy, 0.0 = unhealthy.
			// A source/destination pair is considered unhealthy when the error rate
			// exceeds the unhealthyErrorRateThreshold (10%).
			statusValue := 1.0
			if dest.ErrorRate > unhealthyErrorRateThreshold {
				statusValue = 0.0
			}
			m.statsFactory.NewTaggedStat(metricSyncStatus, stats.GaugeType, tags).
				Gauge(statusValue)

			// Emit schema changes counter: only when changes are detected to avoid noise.
			if dest.SchemaChanges > 0 {
				m.statsFactory.NewTaggedStat(metricSchemaChangesTotal, stats.CountType, tags).
					Count(int(dest.SchemaChanges))
			}
		}
	}
}

// EmitUploadMetrics emits Prometheus metrics for a single completed upload.
// This method is called from the warehouse upload pipeline's success and failure
// metric emission paths (invoked from warehouse/router/upload_stats.go), providing
// real-time per-upload metric emission complementing the periodic aggregate emission
// from EmitMetrics.
//
// For the given SyncHealth record, the following metrics are emitted:
//   - warehouse_sync_duration_seconds (histogram): upload duration converted from ms to seconds
//   - warehouse_sync_rows_total (counter): rows successfully synced
//   - warehouse_sync_errors_total (counter): rows failed with error_category label (only if failures exist)
//   - warehouse_schema_changes_total (counter): schema change event (only if changes detected)
//
// A nil health record is a no-op, allowing safe invocation when health recording is disabled.
func (m *HealthMetrics) EmitUploadMetrics(health *SyncHealth) {
	if health == nil {
		return
	}

	tags := m.buildTags(health.WorkspaceID, health.SourceID, health.DestinationID, health.DestType, health.SourceType)

	// Duration histogram: convert upload duration from ms to seconds.
	m.statsFactory.NewTaggedStat(metricSyncDuration, stats.HistogramType, tags).
		Observe(float64(health.DurationMs) / 1000.0)

	// Rows synced counter: total rows successfully synced in this upload.
	m.statsFactory.NewTaggedStat(metricSyncRowsTotal, stats.CountType, tags).
		Count(int(health.RowsSynced))

	// Rows failed counter with error_category label: only emitted when rows fail,
	// providing error classification for alerting and dashboards.
	if health.RowsFailed > 0 {
		errorTags := m.buildTags(
			health.WorkspaceID, health.SourceID, health.DestinationID, health.DestType, health.SourceType,
			stats.Tags{"error_category": health.ErrorCategory},
		)
		m.statsFactory.NewTaggedStat(metricSyncErrorsTotal, stats.CountType, errorTags).
			Count(int(health.RowsFailed))
	}

	// Schema changes counter: emitted as a single increment when the upload detected
	// schema changes. The SchemaChanges field is []byte (JSONB), so we check for
	// non-nil and non-empty to determine if schema changes occurred.
	if len(health.SchemaChanges) > 0 {
		m.statsFactory.NewTaggedStat(metricSchemaChangesTotal, stats.CountType, tags).
			Count(1)
	}
}
