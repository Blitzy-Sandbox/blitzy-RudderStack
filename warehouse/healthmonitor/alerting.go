package healthmonitor

import (
	"fmt"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
)

// AlertType represents the category of alert being evaluated by the AlertingEvaluator.
// Each alert type corresponds to a specific health metric threshold check.
type AlertType string

const (
	// AlertTypeFailureRate fires when the sync error rate exceeds the configured threshold.
	// Threshold configured via Warehouse.healthMonitor.alerting.failureRateThreshold (default: 0.1 = 10%).
	AlertTypeFailureRate AlertType = "failure_rate"

	// AlertTypeDurationSpike fires when the average sync duration exceeds the configured threshold.
	// Threshold configured via Warehouse.healthMonitor.alerting.durationSpikeThresholdMs (default: 300000 = 5 min).
	AlertTypeDurationSpike AlertType = "duration_spike"

	// AlertTypeRowCountAnomaly fires when the current row count drops significantly
	// compared to the previous period's row count.
	// Threshold configured via Warehouse.healthMonitor.alerting.rowCountDropPercent (default: 50).
	AlertTypeRowCountAnomaly AlertType = "row_count_anomaly"

	// AlertTypeSchemaDrift fires when schema changes are detected during sync.
	// Can be disabled via Warehouse.healthMonitor.alerting.schemaDriftEnabled (default: true).
	AlertTypeSchemaDrift AlertType = "schema_drift"
)

// alertMetricName is the Prometheus counter metric name emitted when a health alert fires.
// Follows the warehouse metric naming convention from warehouse/router/upload_stats.go.
const alertMetricName = "warehouse_health_alert"

// AlertingEvaluator evaluates health metrics against configurable thresholds
// and emits tagged stats for external alerting integration (Prometheus/Grafana).
//
// The evaluator supports four alert types:
//   - Failure Rate: Fires when the error rate exceeds a configurable threshold
//   - Duration Spike: Fires when average sync duration exceeds a configurable threshold
//   - Row Count Anomaly: Fires when row count drops below a percentage of the previous period
//   - Schema Drift: Fires when schema changes are detected
//
// Each alert type has a per-source/destination cooldown to prevent alert flooding.
// The cooldown period is configurable via Warehouse.healthMonitor.alerting.cooldownMinutes.
//
// Thread-safety: The evaluator is safe for concurrent use. The lastAlertTime map
// is protected by a sync.Mutex.
type AlertingEvaluator struct {
	logger       logger.Logger
	statsFactory stats.Stats

	// now returns the current time. Defaults to time.Now but can be overridden for testing.
	now func() time.Time

	// mu protects lastAlertTime for concurrent access from the HealthMonitor collection loop.
	mu sync.Mutex
	// lastAlertTime tracks the last time an alert was emitted per alertKey.
	// The alertKey format is "alertType:sourceID:destID".
	lastAlertTime map[string]time.Time

	config struct {
		// failureRateThreshold is the error rate threshold (0.0 to 1.0) above which
		// a failure rate alert is emitted. Default: 0.1 (10%).
		failureRateThreshold config.ValueLoader[float64]

		// durationSpikeThresholdMs is the average duration threshold in milliseconds
		// above which a duration spike alert is emitted. Default: 300000 (5 minutes).
		durationSpikeThresholdMs config.ValueLoader[int]

		// rowCountDropPercent is the percentage drop in row count from the previous period
		// below which a row count anomaly alert is emitted. Default: 50 (50% drop).
		rowCountDropPercent config.ValueLoader[int]

		// schemaDriftEnabled controls whether schema drift alerts are emitted.
		// Default: true.
		schemaDriftEnabled config.ValueLoader[bool]

		// cooldownMinutes is the cooldown period in minutes between repeated alerts of
		// the same type for the same source/destination pair. Default: 30.
		cooldownMinutes config.ValueLoader[int]
	}
}

// NewAlertingEvaluator creates a new AlertingEvaluator with configurable thresholds.
// All configuration keys are hot-reloadable and follow the Warehouse.<featureName>.<paramName>
// pattern established in warehouse/archive/archiver.go.
//
// Configuration keys:
//   - Warehouse.healthMonitor.alerting.failureRateThreshold (default: 0.1 = 10%)
//   - Warehouse.healthMonitor.alerting.durationSpikeThresholdMs (default: 300000 = 5 min)
//   - Warehouse.healthMonitor.alerting.rowCountDropPercent (default: 50)
//   - Warehouse.healthMonitor.alerting.schemaDriftEnabled (default: true)
//   - Warehouse.healthMonitor.alerting.cooldownMinutes (default: 30)
func NewAlertingEvaluator(
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
) *AlertingEvaluator {
	a := &AlertingEvaluator{
		logger:        log.Child("healthmonitor.alerting"),
		statsFactory:  statsFactory,
		now:           time.Now,
		lastAlertTime: make(map[string]time.Time),
	}

	a.config.failureRateThreshold = conf.GetReloadableFloat64Var(
		0.1, "Warehouse.healthMonitor.alerting.failureRateThreshold",
	)
	a.config.durationSpikeThresholdMs = conf.GetReloadableIntVar(
		300000, 1, "Warehouse.healthMonitor.alerting.durationSpikeThresholdMs",
	)
	a.config.rowCountDropPercent = conf.GetReloadableIntVar(
		50, 1, "Warehouse.healthMonitor.alerting.rowCountDropPercent",
	)
	a.config.schemaDriftEnabled = conf.GetReloadableBoolVar(
		true, "Warehouse.healthMonitor.alerting.schemaDriftEnabled",
	)
	a.config.cooldownMinutes = conf.GetReloadableIntVar(
		30, 1, "Warehouse.healthMonitor.alerting.cooldownMinutes",
	)

	return a
}

// Evaluate checks the health summary against configured thresholds and emits
// tagged alerting stats when thresholds are breached. Implements cooldown to prevent
// alert flooding.
//
// For each source-destination pair in the summary, the evaluator checks:
//   - Failure rate against the configured threshold
//   - Average sync duration against the duration spike threshold
//   - Row count drop against the previous period's row count
//   - Schema changes (if schema drift detection is enabled)
//
// If any threshold is breached and the cooldown period has elapsed for that
// specific alertType:sourceID:destID combination, a warehouse_health_alert
// counter metric is emitted with appropriate tags.
//
// Safe for concurrent use from multiple goroutines.
func (a *AlertingEvaluator) Evaluate(summary *HealthSummaryResponse) {
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
			a.evaluateFailureRate(source.WorkspaceID, source.SourceID, dest.DestID, dest.DestType, source.SourceType, dest.ErrorRate)
			a.evaluateDurationSpike(source.WorkspaceID, source.SourceID, dest.DestID, dest.DestType, source.SourceType, dest.SyncDuration.Avg)
			a.evaluateRowCountAnomaly(source.WorkspaceID, source.SourceID, dest.DestID, dest.DestType, source.SourceType, dest.RowsSynced, dest.PreviousRowsSynced)
			a.evaluateSchemaDrift(source.WorkspaceID, source.SourceID, dest.DestID, dest.DestType, source.SourceType, dest.SchemaChanges)
		}
	}
}

// evaluateFailureRate checks if the error rate exceeds the configured threshold.
// If exceeded and the cooldown period has elapsed, emits a warehouse_health_alert
// counter with alertType=failure_rate.
//
// The error rate is a float64 value between 0.0 and 1.0, where 0.0 means no errors
// and 1.0 means all syncs failed.
func (a *AlertingEvaluator) evaluateFailureRate(workspaceID, sourceID, destID, destType, sourceType string, errorRate float64) {
	threshold := a.config.failureRateThreshold.Load()
	if errorRate <= threshold {
		return
	}

	alertKey := a.buildAlertKey(AlertTypeFailureRate, sourceID, destID)
	if !a.canAlert(alertKey) {
		return
	}

	a.logger.Warnn("failure rate threshold exceeded",
		logger.NewStringField("sourceID", sourceID),
		logger.NewStringField("destID", destID),
		logger.NewFloatField("errorRate", errorRate),
		logger.NewFloatField("threshold", threshold),
	)

	a.emitAlert(AlertTypeFailureRate, workspaceID, sourceID, destID, destType, sourceType)
	a.recordAlert(alertKey)
}

// evaluateDurationSpike checks if the average sync duration exceeds the configured threshold.
// If exceeded and the cooldown period has elapsed, emits a warehouse_health_alert
// counter with alertType=duration_spike.
//
// The avgDurationMs parameter is the average sync duration in milliseconds.
func (a *AlertingEvaluator) evaluateDurationSpike(workspaceID, sourceID, destID, destType, sourceType string, avgDurationMs int64) {
	threshold := int64(a.config.durationSpikeThresholdMs.Load())
	if avgDurationMs <= threshold {
		return
	}

	alertKey := a.buildAlertKey(AlertTypeDurationSpike, sourceID, destID)
	if !a.canAlert(alertKey) {
		return
	}

	a.logger.Warnn("duration spike threshold exceeded",
		logger.NewStringField("sourceID", sourceID),
		logger.NewStringField("destID", destID),
		logger.NewIntField("avgDurationMs", avgDurationMs),
		logger.NewIntField("thresholdMs", threshold),
	)

	a.emitAlert(AlertTypeDurationSpike, workspaceID, sourceID, destID, destType, sourceType)
	a.recordAlert(alertKey)
}

// evaluateRowCountAnomaly checks if the current row count has dropped significantly
// compared to the previous period. A drop is detected when:
//
//	current < previous * (1 - rowCountDropPercent/100)
//
// If the previous row count is zero or negative, no anomaly check is performed because
// there is no baseline to compare against.
//
// If the drop exceeds the threshold and the cooldown period has elapsed, emits a
// warehouse_health_alert counter with alertType=row_count_anomaly.
func (a *AlertingEvaluator) evaluateRowCountAnomaly(workspaceID, sourceID, destID, destType, sourceType string, rowsSynced, previousRowsSynced int64) {
	// No baseline to compare against — skip anomaly detection.
	if previousRowsSynced <= 0 {
		return
	}

	dropPercent := a.config.rowCountDropPercent.Load()
	// Calculate the minimum acceptable row count:
	// threshold = previous * (1 - dropPercent/100)
	// If current rows are below this threshold, it's an anomaly.
	thresholdRows := float64(previousRowsSynced) * (1.0 - float64(dropPercent)/100.0)
	if float64(rowsSynced) >= thresholdRows {
		return
	}

	alertKey := a.buildAlertKey(AlertTypeRowCountAnomaly, sourceID, destID)
	if !a.canAlert(alertKey) {
		return
	}

	a.logger.Warnn("row count anomaly detected",
		logger.NewStringField("sourceID", sourceID),
		logger.NewStringField("destID", destID),
		logger.NewIntField("rowsSynced", rowsSynced),
		logger.NewIntField("previousRowsSynced", previousRowsSynced),
		logger.NewIntField("dropPercentThreshold", int64(dropPercent)),
	)

	a.emitAlert(AlertTypeRowCountAnomaly, workspaceID, sourceID, destID, destType, sourceType)
	a.recordAlert(alertKey)
}

// evaluateSchemaDrift checks if schema changes have been detected.
// Schema drift alerting can be disabled via the schemaDriftEnabled configuration key.
//
// If schema changes are detected (count > 0) and the cooldown period has elapsed,
// emits a warehouse_health_alert counter with alertType=schema_drift.
func (a *AlertingEvaluator) evaluateSchemaDrift(workspaceID, sourceID, destID, destType, sourceType string, schemaChanges int64) {
	if !a.config.schemaDriftEnabled.Load() {
		return
	}

	if schemaChanges <= 0 {
		return
	}

	alertKey := a.buildAlertKey(AlertTypeSchemaDrift, sourceID, destID)
	if !a.canAlert(alertKey) {
		return
	}

	a.logger.Warnn("schema drift detected",
		logger.NewStringField("sourceID", sourceID),
		logger.NewStringField("destID", destID),
		logger.NewIntField("schemaChanges", schemaChanges),
	)

	a.emitAlert(AlertTypeSchemaDrift, workspaceID, sourceID, destID, destType, sourceType)
	a.recordAlert(alertKey)
}

// buildAlertKey constructs a composite key for cooldown tracking in the format
// "alertType:sourceID:destID". This ensures each unique combination of alert type,
// source, and destination has its own independent cooldown timer.
func (a *AlertingEvaluator) buildAlertKey(alertType AlertType, sourceID, destID string) string {
	return fmt.Sprintf("%s:%s:%s", alertType, sourceID, destID)
}

// canAlert checks if an alert can be emitted for the given key, respecting the
// configured cooldown period. Returns true if no alert has been emitted for this key,
// or if the time since the last alert exceeds the cooldown period.
//
// Thread-safe: protected by sync.Mutex.
func (a *AlertingEvaluator) canAlert(alertKey string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	lastTime, exists := a.lastAlertTime[alertKey]
	if !exists {
		return true
	}

	cooldown := time.Duration(a.config.cooldownMinutes.Load()) * time.Minute
	elapsed := a.now().Sub(lastTime)
	return elapsed >= cooldown
}

// recordAlert records the timestamp of an alert emission for cooldown tracking.
// The timestamp is used by canAlert to determine if the cooldown period has elapsed.
//
// Thread-safe: protected by sync.Mutex.
func (a *AlertingEvaluator) recordAlert(alertKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastAlertTime[alertKey] = a.now()
}

// emitAlert emits a warehouse_health_alert counter metric with the standard tag set.
// Tags follow the pattern established in warehouse/router/upload_stats.go:
//   - module: "warehouse" (constant, identifies the subsystem)
//   - workspaceId: the workspace owning the source-destination pair
//   - alertType: the type of alert (failure_rate, duration_spike, row_count_anomaly, schema_drift)
//   - sourceID: the RudderStack source identifier
//   - destID: the warehouse destination identifier
//   - destType: the warehouse destination type (e.g., "SNOWFLAKE", "BQ", "RS")
//   - sourceType: the source type (e.g., "web", "android", "ios", "cloud")
func (a *AlertingEvaluator) emitAlert(alertType AlertType, workspaceID, sourceID, destID, destType, sourceType string) {
	tags := stats.Tags{
		"module":      "warehouse",
		"workspaceId": workspaceID,
		"alertType":   string(alertType),
		"sourceID":    sourceID,
		"destID":      destID,
		"destType":    destType,
		"sourceType":  sourceType,
	}
	a.statsFactory.NewTaggedStat(alertMetricName, stats.CountType, tags).Increment()
}

// PruneCooldowns removes expired cooldown entries from the lastAlertTime map.
// An entry is expired when the time since its last alert exceeds twice the cooldown period.
// This prevents unbounded growth of the map when source/destination pairs are removed
// or change over time.
//
// Thread-safe: protected by sync.Mutex.
func (a *AlertingEvaluator) PruneCooldowns() {
	a.mu.Lock()
	defer a.mu.Unlock()

	cooldown := time.Duration(a.config.cooldownMinutes.Load()) * time.Minute
	expiry := 2 * cooldown // Keep entries for 2x the cooldown period before pruning.
	now := a.now()

	for key, lastTime := range a.lastAlertTime {
		if now.Sub(lastTime) > expiry {
			delete(a.lastAlertTime, key)
		}
	}
}
