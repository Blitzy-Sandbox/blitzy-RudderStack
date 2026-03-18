// Package healthmonitor provides warehouse sync health monitoring with per-upload
// metrics collection, Prometheus metric emission, alerting thresholds, and a
// dedicated HTTP API for dashboard consumption.
//
// Configuration is delivered via the standard Warehouse configuration hierarchy:
//   - Warehouse.healthMonitor.enabled: Enable/disable health monitoring (default: true)
//   - Warehouse.healthMonitor.collectionIntervalSeconds: Metric collection interval (default: 60)
//   - Warehouse.healthMonitor.retentionDays: Health record retention period (default: 30)
//   - Warehouse.healthMonitor.alerting.failureRateThreshold: Failure rate alert threshold (default: 0.1)
//   - Warehouse.healthMonitor.alerting.durationSpikeThresholdMs: Duration spike threshold (default: 300000)
//   - Warehouse.healthMonitor.alerting.rowCountDropPercent: Row count drop alert threshold (default: 50)
//   - Warehouse.healthMonitor.alerting.schemaDriftEnabled: Enable schema drift alerts (default: true)
//   - Warehouse.healthMonitor.alerting.cooldownMinutes: Alert cooldown period (default: 30)
//
// These keys are consumed by other files in this package via the reloadable config
// variable pattern established in warehouse/archive/archiver.go, using methods such as
// config.GetReloadableBoolVar(), config.GetReloadableIntVar(), and
// config.GetReloadableFloat64Var() from github.com/rudderlabs/rudder-go-kit/config.
package healthmonitor

// Configuration key constants define the paths used to look up health monitoring
// settings from the RudderStack configuration system. All keys are nested under
// the Warehouse.healthMonitor.* hierarchy, following the convention established
// throughout the warehouse package (see warehouse/archive/archiver.go lines 92-98).
const (
	// ConfigKeyEnabled is the configuration key for enabling health monitoring.
	// When set to false, no health metrics are collected and no alerting is evaluated.
	// Consumed via config.GetReloadableBoolVar().
	ConfigKeyEnabled = "Warehouse.healthMonitor.enabled"

	// ConfigKeyCollectionInterval is the configuration key for the metric collection
	// interval in seconds. Controls how frequently the health monitor queries recent
	// upload records to compute aggregate health statistics.
	// Consumed via config.GetReloadableIntVar().
	ConfigKeyCollectionInterval = "Warehouse.healthMonitor.collectionIntervalSeconds"

	// ConfigKeyRetentionDays is the configuration key for the health record retention
	// period in days. Records older than this threshold are purged during periodic
	// cleanup to prevent unbounded table growth.
	// Consumed via config.GetReloadableIntVar().
	ConfigKeyRetentionDays = "Warehouse.healthMonitor.retentionDays"

	// ConfigKeyFailureRateThreshold is the configuration key for the failure rate
	// alerting threshold, expressed as a float between 0.0 and 1.0. When the ratio
	// of failed uploads to total uploads exceeds this value within the evaluation
	// window, an alert is emitted via the stats subsystem.
	// Consumed via config.GetReloadableFloat64Var().
	ConfigKeyFailureRateThreshold = "Warehouse.healthMonitor.alerting.failureRateThreshold"

	// ConfigKeyDurationSpikeThreshold is the configuration key for the duration spike
	// detection threshold in milliseconds. When a sync operation's duration exceeds
	// this value, it is flagged as a latency spike and triggers an alert.
	// Consumed via config.GetReloadableIntVar().
	ConfigKeyDurationSpikeThreshold = "Warehouse.healthMonitor.alerting.durationSpikeThresholdMs"

	// ConfigKeyRowCountDropPercent is the configuration key for row count anomaly
	// detection, expressed as a percentage (0–100). When the row count for a sync
	// drops by more than this percentage compared to the rolling average, an anomaly
	// alert is raised.
	// Consumed via config.GetReloadableIntVar().
	ConfigKeyRowCountDropPercent = "Warehouse.healthMonitor.alerting.rowCountDropPercent"

	// ConfigKeySchemaDriftEnabled is the configuration key for enabling schema drift
	// detection. When enabled, the health monitor compares each sync's schema against
	// the previously recorded schema and reports any added, removed, or type-changed
	// columns as schema drift events.
	// Consumed via config.GetReloadableBoolVar().
	ConfigKeySchemaDriftEnabled = "Warehouse.healthMonitor.alerting.schemaDriftEnabled"

	// ConfigKeyCooldownMinutes is the configuration key for the alert cooldown period
	// in minutes. After an alert is emitted for a given source/destination pair, no
	// further alerts of the same category are emitted until the cooldown expires.
	// This prevents alert flooding during prolonged outages or degradation periods.
	// Consumed via config.GetReloadableIntVar().
	ConfigKeyCooldownMinutes = "Warehouse.healthMonitor.alerting.cooldownMinutes"
)

// Default value constants provide safe fallbacks when configuration keys are not
// explicitly set. These defaults are designed to be non-disruptive: health monitoring
// is enabled by default (it is additive and does not alter existing sync behavior),
// and alerting thresholds are set to reasonable values that avoid false positives
// in typical production environments.
const (
	// DefaultEnabled is the default value for health monitoring enabled flag.
	// Health monitoring is enabled by default because it is purely additive —
	// it observes and records metrics without altering the sync pipeline.
	DefaultEnabled = true

	// DefaultCollectionIntervalSeconds is the default collection interval in seconds.
	// A 60-second interval provides near-real-time visibility while keeping the
	// query load on the warehouse metadata database minimal.
	DefaultCollectionIntervalSeconds = 60

	// DefaultRetentionDays is the default retention period for health records in days.
	// Thirty days provides sufficient history for trend analysis and root-cause
	// investigation while keeping storage requirements bounded.
	DefaultRetentionDays = 30

	// DefaultFailureRateThreshold is the default failure rate alerting threshold.
	// A threshold of 0.1 (10%) means alerts fire only when more than one in ten
	// syncs are failing, avoiding noise from transient single-sync failures.
	DefaultFailureRateThreshold = 0.1

	// DefaultDurationSpikeThresholdMs is the default duration spike threshold in
	// milliseconds. The value of 300000 (5 minutes) represents a generous upper bound
	// that accommodates large warehouses while still catching significant regressions.
	DefaultDurationSpikeThresholdMs = 300000

	// DefaultRowCountDropPercent is the default row count drop percentage for anomaly
	// detection. A 50% drop from the rolling average is a strong signal of an
	// upstream data issue or pipeline misconfiguration.
	DefaultRowCountDropPercent = 50

	// DefaultSchemaDriftEnabled is the default value for schema drift detection.
	// Schema drift alerting is enabled by default to provide immediate visibility
	// into unexpected schema changes that could indicate upstream instrumentation
	// modifications or data quality regressions.
	DefaultSchemaDriftEnabled = true

	// DefaultCooldownMinutes is the default alert cooldown period in minutes.
	// A 30-minute cooldown prevents alert fatigue during sustained degradation
	// while still ensuring operators are notified of recurring issues within
	// a reasonable timeframe.
	DefaultCooldownMinutes = 30
)
