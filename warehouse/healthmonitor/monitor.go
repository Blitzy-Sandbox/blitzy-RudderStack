package healthmonitor

import (
	"context"
	"fmt"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	sqlmw "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
)

// HealthMonitor aggregates per-upload metrics from the warehouse upload pipeline,
// emits Prometheus metrics, evaluates alerting thresholds, and persists health records
// to the wh_sync_health table. It runs a context-cancellable periodic collection loop
// that matches the warehouse/archive/cron.go ticker pattern.
//
// The monitor:
//   - Queries recent wh_uploads for completion rates, duration percentiles, and error categorization
//   - Emits Prometheus metrics (histograms, counters, gauges) via the stats factory
//   - Evaluates alerting thresholds for failure rates, duration spikes, row count anomalies, and schema drift
//   - Purges old health records beyond the configured retention window
//
// The HealthMonitor is goroutine-safe: Run() executes in a single goroutine,
// while RecordSyncHealth() may be called concurrently from upload pipeline goroutines.
type HealthMonitor struct {
	db           *sqlmw.DB
	conf         *config.Config
	logger       logger.Logger
	statsFactory stats.Stats

	// now returns the current time. Defaults to time.Now but can be overridden for testing.
	now func() time.Time

	repository *HealthRepo
	metrics    *HealthMetrics
	alerting   *AlertingEvaluator

	config struct {
		enabled                   config.ValueLoader[bool]
		collectionIntervalSeconds config.ValueLoader[int]
		retentionDays             config.ValueLoader[int]
	}
}

// NewHealthMonitor creates a new HealthMonitor with the given dependencies.
// Configuration keys follow the Warehouse.<featureName>.<paramName> pattern:
//   - Warehouse.healthMonitor.enabled (default: true) — master switch for health monitoring
//   - Warehouse.healthMonitor.collectionIntervalSeconds (default: 60) — metric collection frequency
//   - Warehouse.healthMonitor.retentionDays (default: 30) — health record retention period
//
// The constructor initializes three sub-components:
//   - HealthRepo: database access for wh_sync_health table
//   - HealthMetrics: Prometheus metric emission
//   - AlertingEvaluator: configurable threshold alerting
//
// All configuration values are reloadable at runtime without restart, matching the pattern
// established in warehouse/archive/archiver.go lines 92-98.
func NewHealthMonitor(
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
	db *sqlmw.DB,
) *HealthMonitor {
	m := &HealthMonitor{
		db:           db,
		conf:         conf,
		logger:       log.Child("healthmonitor"),
		statsFactory: statsFactory,
		now:          time.Now,
	}

	// Reloadable configuration using the established config pattern
	m.config.enabled = conf.GetReloadableBoolVar(
		true, "Warehouse.healthMonitor.enabled",
	)
	m.config.collectionIntervalSeconds = conf.GetReloadableIntVar(
		60, 1, "Warehouse.healthMonitor.collectionIntervalSeconds",
	)
	m.config.retentionDays = conf.GetReloadableIntVar(
		30, 1, "Warehouse.healthMonitor.retentionDays",
	)

	// Initialize sub-components with shared dependencies
	m.repository = NewHealthRepo(db, statsFactory)
	m.metrics = NewHealthMetrics(statsFactory)
	m.alerting = NewAlertingEvaluator(conf, log, statsFactory)

	return m
}

// Run starts the health monitoring periodic collection loop.
// It runs until the context is cancelled, collecting metrics at the configured interval.
//
// The loop performs the following on each tick:
//  1. Checks if health monitoring is enabled (reloadable at runtime)
//  2. Collects aggregated metrics from recent uploads via the repository
//  3. Emits Prometheus metrics for each source/destination pair
//  4. Evaluates alerting thresholds for failure rate, duration spikes, row count anomalies, and schema drift
//  5. Purges old health records beyond the configured retention window
//
// Errors during collection or purging are logged but do not crash the loop — the monitor
// continues to the next tick, matching the resilient pattern from warehouse/archive/cron.go.
//
// Run returns nil when the context is cancelled (graceful shutdown).
func (m *HealthMonitor) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			m.logger.Infon("context is cancelled, stopped running health monitor")
			return nil
		case <-time.After(time.Duration(m.config.collectionIntervalSeconds.Load()) * time.Second):
			if !m.config.enabled.Load() {
				continue
			}

			if err := m.collect(ctx); err != nil {
				m.logger.Errorn("error collecting health metrics", obskit.Error(err))
			}

			if err := m.purge(ctx); err != nil {
				m.logger.Errorn("error purging old health records", obskit.Error(err))
			}

			// Prune expired cooldown entries from the alerting evaluator to prevent
			// unbounded growth of the lastAlertTime map.
			m.alerting.PruneCooldowns()
		}
	}
}

// collect queries recent uploads, aggregates metrics, emits Prometheus stats,
// and evaluates alerting thresholds. This is the core periodic work performed
// on each collection tick.
//
// The method delegates to three sub-components in order:
//  1. HealthRepo.GetHealthSummary — aggregate upload data from wh_sync_health
//  2. HealthMetrics.EmitMetrics — emit Prometheus counters, gauges, and histograms
//  3. AlertingEvaluator.Evaluate — check thresholds and emit alerting stats
func (m *HealthMonitor) collect(ctx context.Context) error {
	summary, err := m.repository.GetHealthSummary(ctx)
	if err != nil {
		return fmt.Errorf("getting health summary: %w", err)
	}

	// Emit Prometheus metrics for each source/destination pair
	m.metrics.EmitMetrics(summary)

	// Evaluate alerting thresholds
	m.alerting.Evaluate(summary)

	return nil
}

// purge removes health records older than the configured retention window.
// The retention period is controlled by the Warehouse.healthMonitor.retentionDays
// configuration key (default: 30 days).
//
// Purge is called after each collection cycle to keep the wh_sync_health table bounded.
// If records are purged, an info-level log is emitted with the count and retention period.
func (m *HealthMonitor) purge(ctx context.Context) error {
	retentionDays := m.config.retentionDays.Load()
	before := m.now().AddDate(0, 0, -retentionDays)

	count, err := m.repository.PurgeOldRecords(ctx, before)
	if err != nil {
		return fmt.Errorf("purging old health records: %w", err)
	}

	if count > 0 {
		m.logger.Infon("purged old health records",
			logger.NewIntField("count", count),
			logger.NewIntField("retentionDays", int64(retentionDays)),
		)
	}

	return nil
}

// RecordSyncHealth records a sync health entry for a completed or failed upload.
// This method is called from the upload pipeline's success/failure metric emission
// paths (warehouse/router/upload_stats.go) at the end of generateUploadSuccessMetrics()
// and in error/abort paths.
//
// When health monitoring is disabled via the Warehouse.healthMonitor.enabled config,
// this method returns nil immediately without persisting the record, ensuring zero
// overhead when the feature is turned off.
//
// The method is safe for concurrent calls from multiple upload job goroutines,
// as the underlying repository handles database-level concurrency.
// RecordSyncHealth records a sync health entry. The health parameter is typed as
// interface{} to satisfy the syncHealthRecorder local interface in warehouse/router/upload.go,
// avoiding a direct import cycle. Callers must pass a *SyncHealth value; any other type
// will return an error.
func (m *HealthMonitor) RecordSyncHealth(ctx context.Context, health interface{}) error {
	if !m.config.enabled.Load() {
		return nil
	}

	h, ok := health.(*SyncHealth)
	if !ok {
		return fmt.Errorf("RecordSyncHealth: expected *SyncHealth, got %T", health)
	}

	if err := m.repository.RecordSyncHealth(ctx, h); err != nil {
		return fmt.Errorf("recording sync health: %w", err)
	}

	// Emit real-time per-upload Prometheus metrics. This provides immediate metric
	// visibility for individual uploads, complementing the periodic aggregate emission
	// from the HealthMetrics.EmitMetrics() path in the collect() loop.
	m.metrics.EmitUploadMetrics(h)

	return nil
}
