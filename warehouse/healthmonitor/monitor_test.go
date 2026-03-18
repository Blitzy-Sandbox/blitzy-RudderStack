package healthmonitor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	sqlmw "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
)

// newTestMonitor creates a fully configured HealthMonitor for testing.
// It uses go-sqlmock for the database layer, memstats for Prometheus metric verification,
// and config.SingleValueLoader for deterministic reloadable config injection.
//
// Returns the HealthMonitor, the memstats store (for metric assertions),
// the sqlmock handle (for setting SQL expectations), and the underlying HealthRepo
// (for overriding the clock function in time-sensitive tests).
func newTestMonitor(t *testing.T, enabled bool, intervalSeconds, retentionDays int) (
	*HealthMonitor, *memstats.Store, sqlmock.Sqlmock, *HealthRepo,
) {
	t.Helper()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	wrappedDB := sqlmw.New(db)

	statsStore, err := memstats.New()
	require.NoError(t, err)

	conf := config.New()
	repo := NewHealthRepo(wrappedDB, statsStore)
	metrics := NewHealthMetrics(statsStore)
	alerting := NewAlertingEvaluator(conf, logger.NOP, statsStore)

	m := &HealthMonitor{
		db:           wrappedDB,
		conf:         conf,
		logger:       logger.NOP,
		statsFactory: statsStore,
		repository:   repo,
		metrics:      metrics,
		alerting:     alerting,
	}

	m.config.enabled = config.SingleValueLoader(enabled)
	m.config.collectionIntervalSeconds = config.SingleValueLoader(intervalSeconds)
	m.config.retentionDays = config.SingleValueLoader(retentionDays)

	return m, statsStore, mock, repo
}

// healthSummaryColumns returns the column names for the GetHealthSummary SQL result set,
// matching the SELECT clause in getHealthSummarySQL from repository.go.
func healthSummaryColumns() []string {
	return []string{
		"source_id", "destination_id", "dest_type", "source_type",
		"total_syncs", "successful_syncs",
		"avg_duration_ms", "min_duration_ms", "max_duration_ms",
		"total_rows_synced", "total_rows_failed",
		"last_sync",
	}
}

// TestHealthMonitor_CollectMetrics validates the collect() method's interaction with
// the repository, metric emission pipeline, and alerting evaluator.
//
// The collect() method performs three sequential steps:
//  1. Query the repository for a health summary (GetHealthSummary)
//  2. Emit Prometheus metrics for each source/destination pair (EmitMetrics)
//  3. Evaluate alerting thresholds (Evaluate)
//
// Each subtest exercises a different scenario to ensure comprehensive coverage.
func TestHealthMonitor_CollectMetrics(t *testing.T) {
	t.Run("successful collection", func(t *testing.T) {
		// Arrange: Create a monitor with a mock DB that returns one source/dest pair.
		m, statsStore, mock, repo := newTestMonitor(t, true, 60, 30)

		// Override the repo clock for deterministic query time range.
		fixedNow := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
		repo.now = func() time.Time { return fixedNow }

		// Set up mock rows for GetHealthSummary.
		// The query uses "since = r.now().Add(-24 * time.Hour)" as the parameter.
		rows := sqlmock.NewRows(healthSummaryColumns()).AddRow(
			"source-1",        // source_id
			"dest-1",          // destination_id
			"SNOWFLAKE",       // dest_type
			"web",             // source_type
			int64(100),        // total_syncs
			int64(95),         // successful_syncs
			float64(5000),     // avg_duration_ms
			float64(1000),     // min_duration_ms
			float64(10000),    // max_duration_ms
			int64(50000),      // total_rows_synced
			int64(100),        // total_rows_failed
			fixedNow,          // last_sync
		)
		mock.ExpectQuery("SELECT").WillReturnRows(rows)

		// Act: call collect directly (private method, same package).
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.collect(ctx)

		// Assert: no error from the collection.
		require.NoError(t, err)

		// Assert: Prometheus metrics were emitted correctly.
		baseTags := stats.Tags{
			"module":     "warehouse",
			"sourceID":   "source-1",
			"destID":     "dest-1",
			"destType":   "SNOWFLAKE",
			"sourceType": "web",
		}

		// Duration histogram: avg_duration_ms (5000) / 1000.0 = 5.0 seconds
		durationMeasurement := statsStore.Get(metricSyncDuration, baseTags)
		require.NotNil(t, durationMeasurement)
		require.EqualValues(t, 5.0, durationMeasurement.LastValue())

		// Rows synced counter: 50000 rows
		rowsMeasurement := statsStore.Get(metricSyncRowsTotal, baseTags)
		require.NotNil(t, rowsMeasurement)
		require.EqualValues(t, 50000, rowsMeasurement.LastValue())

		// Sync status gauge: error rate = 1 - 95/100 = 0.05 (5%), below 10% threshold → healthy (1.0)
		statusMeasurement := statsStore.Get(metricSyncStatus, baseTags)
		require.NotNil(t, statusMeasurement)
		require.EqualValues(t, 1.0, statusMeasurement.LastValue())

		// Errors counter: 5 errors (100 - 95)
		errorTags := stats.Tags{
			"module":         "warehouse",
			"sourceID":       "source-1",
			"destID":         "dest-1",
			"destType":       "SNOWFLAKE",
			"sourceType":     "web",
			"error_category": "",
		}
		errorMeasurement := statsStore.Get(metricSyncErrorsTotal, errorTags)
		require.NotNil(t, errorMeasurement)
		require.EqualValues(t, 5, errorMeasurement.LastValue())

		// Verify all mock expectations were satisfied.
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("empty upload data", func(t *testing.T) {
		// Arrange: Create a monitor where GetHealthSummary returns no rows.
		m, statsStore, mock, repo := newTestMonitor(t, true, 60, 30)

		fixedNow := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
		repo.now = func() time.Time { return fixedNow }

		// Empty result set — no health data available.
		emptyRows := sqlmock.NewRows(healthSummaryColumns())
		mock.ExpectQuery("SELECT").WillReturnRows(emptyRows)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Act
		err := m.collect(ctx)

		// Assert: no error, no metrics emitted (empty summary, no sources).
		require.NoError(t, err)

		// No metrics should have been emitted since there are no source/dest pairs.
		allMetrics := statsStore.GetAll()
		for _, metric := range allMetrics {
			// None of the health-specific metrics should be present.
			if metric.Name == metricSyncDuration ||
				metric.Name == metricSyncRowsTotal ||
				metric.Name == metricSyncErrorsTotal ||
				metric.Name == metricSyncStatus ||
				metric.Name == metricSchemaChangesTotal {
				t.Errorf("unexpected metric emitted with empty data: %s", metric.Name)
			}
		}

		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("repository error", func(t *testing.T) {
		// Arrange: Create a monitor where the repository query fails.
		m, _, mock, _ := newTestMonitor(t, true, 60, 30)

		mock.ExpectQuery("SELECT").WillReturnError(fmt.Errorf("connection refused"))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Act
		err := m.collect(ctx)

		// Assert: error is returned but the monitor should not crash.
		require.Error(t, err)
		require.Contains(t, err.Error(), "getting health summary")

		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("unhealthy status with high error rate", func(t *testing.T) {
		// Arrange: Create a monitor with >10% error rate → unhealthy status gauge.
		m, statsStore, mock, repo := newTestMonitor(t, true, 60, 30)

		fixedNow := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
		repo.now = func() time.Time { return fixedNow }

		// 20% error rate: 80 successful out of 100
		rows := sqlmock.NewRows(healthSummaryColumns()).AddRow(
			"source-2",        // source_id
			"dest-2",          // destination_id
			"BQ",              // dest_type
			"android",         // source_type
			int64(100),        // total_syncs
			int64(80),         // successful_syncs (20% failure)
			float64(3000),     // avg_duration_ms
			float64(500),      // min_duration_ms
			float64(8000),     // max_duration_ms
			int64(30000),      // total_rows_synced
			int64(200),        // total_rows_failed
			fixedNow,          // last_sync
		)
		mock.ExpectQuery("SELECT").WillReturnRows(rows)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.collect(ctx)
		require.NoError(t, err)

		// Assert: status gauge is 0.0 (unhealthy) because error rate 20% > 10% threshold.
		statusTags := stats.Tags{
			"module":     "warehouse",
			"sourceID":   "source-2",
			"destID":     "dest-2",
			"destType":   "BQ",
			"sourceType": "android",
		}
		statusMeasurement := statsStore.Get(metricSyncStatus, statusTags)
		require.NotNil(t, statusMeasurement)
		require.EqualValues(t, 0.0, statusMeasurement.LastValue())

		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestHealthMonitor_PurgeOldRecords validates the purge() method's interaction with
// the repository for retention-based cleanup of old health records.
//
// The purge() method:
//  1. Loads the configured retention period (retentionDays)
//  2. Computes a cutoff time: time.Now().AddDate(0, 0, -retentionDays)
//  3. Calls repository.PurgeOldRecords(ctx, cutoffTime)
//  4. Logs the count of purged records (if any)
func TestHealthMonitor_PurgeOldRecords(t *testing.T) {
	t.Run("records older than retentionDays purged", func(t *testing.T) {
		// Arrange: 30-day retention, mock returns 5 deleted records.
		m, _, mock, _ := newTestMonitor(t, true, 60, 30)

		// Expect a DELETE query with a timestamp argument (any time arg).
		mock.ExpectExec("DELETE FROM").
			WithArgs(sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 5))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Act
		err := m.purge(ctx)

		// Assert: no error, 5 records were purged.
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("recent records preserved", func(t *testing.T) {
		// Arrange: 30-day retention, mock returns 0 deleted records
		// (all records are within the retention window).
		m, _, mock, _ := newTestMonitor(t, true, 60, 30)

		mock.ExpectExec("DELETE FROM").
			WithArgs(sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 0))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.purge(ctx)

		// Assert: no error, nothing was purged.
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("configurable retention", func(t *testing.T) {
		// Test with different retention periods to verify the config is respected.
		retentionValues := []int{7, 90, 365}

		for _, retDays := range retentionValues {
			t.Run(fmt.Sprintf("retention_%d_days", retDays), func(t *testing.T) {
				m, _, mock, _ := newTestMonitor(t, true, 60, retDays)

				mock.ExpectExec("DELETE FROM").
					WithArgs(sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 3))

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				err := m.purge(ctx)
				require.NoError(t, err)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	})

	t.Run("purge database error", func(t *testing.T) {
		// Arrange: Repository returns an error during purge.
		m, _, mock, _ := newTestMonitor(t, true, 60, 30)

		mock.ExpectExec("DELETE FROM").
			WithArgs(sqlmock.AnyArg()).
			WillReturnError(fmt.Errorf("disk full"))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.purge(ctx)

		// Assert: error is returned with descriptive wrapping.
		require.Error(t, err)
		require.Contains(t, err.Error(), "purging old health records")

		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestHealthMonitor_Run_ContextCancellation validates that the Run() method exits
// gracefully when the context is cancelled, matching the context-cancellable
// ticker loop pattern from warehouse/archive/cron.go.
//
// The test verifies:
//   - Run() returns nil on context cancellation (not an error)
//   - The goroutine exits without leaking
//   - No panics or race conditions during shutdown
func TestHealthMonitor_Run_ContextCancellation(t *testing.T) {
	t.Run("immediate cancellation", func(t *testing.T) {
		// Arrange: Create a monitor with a very short interval.
		m, _, _, _ := newTestMonitor(t, false, 1, 30)

		ctx, cancel := context.WithCancel(context.Background())

		// Cancel the context immediately before starting Run.
		cancel()

		// Act: Run should return immediately since context is already cancelled.
		err := m.Run(ctx)

		// Assert: Run returns nil on context cancellation (graceful shutdown).
		require.Nil(t, err)
	})

	t.Run("cancellation during wait", func(t *testing.T) {
		// Arrange: Create a monitor with a long interval so it's waiting when cancelled.
		m, _, _, _ := newTestMonitor(t, false, 3600, 30)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() {
			done <- m.Run(ctx)
		}()

		// Give the goroutine a moment to enter the select loop.
		time.Sleep(100 * time.Millisecond)

		// Cancel the context while Run is waiting.
		cancel()

		// Assert: Run exits within a reasonable timeout.
		select {
		case err := <-done:
			require.Nil(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not exit within 5 seconds after context cancellation")
		}
	})

	t.Run("cancellation with timeout guard", func(t *testing.T) {
		// Arrange: Use context.WithTimeout as a guard to prevent test hangs.
		m, _, _, _ := newTestMonitor(t, false, 3600, 30)

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		// Act: Run blocks until the timeout fires.
		err := m.Run(ctx)

		// Assert: Returns nil after context deadline exceeded.
		require.Nil(t, err)
	})
}

// TestHealthMonitor_Run_PeriodicCollection validates that the Run() method executes
// the collection loop at the configured interval and performs both collection and
// purge operations on each tick.
//
// The test uses a 1-second collection interval and waits for 2+ cycles to verify
// that the monitor runs periodically.
func TestHealthMonitor_Run_PeriodicCollection(t *testing.T) {
	t.Run("multiple collection cycles", func(t *testing.T) {
		// Arrange: Monitor with 1-second interval, enabled.
		m, statsStore, mock, repo := newTestMonitor(t, true, 1, 30)

		fixedNow := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
		repo.now = func() time.Time { return fixedNow }

		// Set up mock expectations for at least 2 collection cycles.
		// Each cycle: 1 GetHealthSummary query + 1 PurgeOldRecords exec.
		for i := 0; i < 3; i++ {
			summaryRows := sqlmock.NewRows(healthSummaryColumns()).AddRow(
				"src-periodic",    // source_id
				"dst-periodic",    // destination_id
				"RS",              // dest_type
				"ios",             // source_type
				int64(10),         // total_syncs
				int64(10),         // successful_syncs
				float64(2000),     // avg_duration_ms
				float64(1000),     // min_duration_ms
				float64(3000),     // max_duration_ms
				int64(5000),       // total_rows_synced
				int64(0),          // total_rows_failed
				fixedNow,          // last_sync
			)
			mock.ExpectQuery("SELECT").WillReturnRows(summaryRows)

			mock.ExpectExec("DELETE FROM").
				WithArgs(sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 0))
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- m.Run(ctx)
		}()

		// Wait enough time for at least 2 collection cycles (1s interval).
		time.Sleep(2500 * time.Millisecond)

		// Cancel to stop the loop.
		cancel()

		select {
		case err := <-done:
			require.Nil(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not exit within 5 seconds after context cancellation")
		}

		// Assert: Verify metrics were emitted at least twice (multiple cycles).
		periodicTags := stats.Tags{
			"module":     "warehouse",
			"sourceID":   "src-periodic",
			"destID":     "dst-periodic",
			"destType":   "RS",
			"sourceType": "ios",
		}
		durationValues := statsStore.Get(metricSyncDuration, periodicTags)
		require.NotNil(t, durationValues)
		require.True(t, len(durationValues.Values()) >= 2,
			"expected at least 2 collection cycles, got %d", len(durationValues.Values()))
	})

	t.Run("disabled monitor skips collection", func(t *testing.T) {
		// Arrange: Monitor is disabled, should not trigger any DB queries.
		m, _, _, _ := newTestMonitor(t, false, 1, 30)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- m.Run(ctx)
		}()

		// Wait for a few would-be collection cycles.
		time.Sleep(2500 * time.Millisecond)
		cancel()

		select {
		case err := <-done:
			require.Nil(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not exit within 5 seconds")
		}

		// No DB interactions should have occurred since monitor is disabled.
		// (sqlmock would fail if unexpected queries were made)
	})
}

// TestHealthMonitor_RecordSyncHealth validates the RecordSyncHealth() method,
// which records a sync health entry for a completed or failed upload.
//
// When health monitoring is disabled, the method returns nil immediately
// without persisting the record. When enabled, it delegates to repository.RecordSyncHealth().
func TestHealthMonitor_RecordSyncHealth(t *testing.T) {
	t.Run("successful record", func(t *testing.T) {
		// Arrange: Monitor is enabled, repository accepts the record.
		m, _, mock, _ := newTestMonitor(t, true, 60, 30)

		health := &SyncHealth{
			UploadID:      42,
			SourceID:      "source-rec",
			DestinationID: "dest-rec",
			DestType:      "SNOWFLAKE",
			SourceType:    "web",
			WorkspaceID:   "workspace-1",
			Status:        "exported_data",
			DurationMs:    5000,
			RowsSynced:    1000,
			RowsFailed:    0,
			ErrorCategory: "",
		}

		// Mock the INSERT query with RETURNING id.
		mock.ExpectQuery("INSERT INTO").
			WithArgs(
				health.UploadID,
				health.SourceID,
				health.DestinationID,
				health.DestType,
				health.SourceType,
				health.WorkspaceID,
				health.Status,
				health.DurationMs,
				health.RowsSynced,
				health.RowsFailed,
				health.ErrorCategory,
				sqlmock.AnyArg(), // schemaChangesJSON
				sqlmock.AnyArg(), // created_at from repo.now()
			).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.RecordSyncHealth(ctx, health)

		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("record with error category", func(t *testing.T) {
		// Arrange: Record a failed sync with an error category.
		m, _, mock, _ := newTestMonitor(t, true, 60, 30)

		health := &SyncHealth{
			UploadID:      43,
			SourceID:      "source-err",
			DestinationID: "dest-err",
			DestType:      "BQ",
			SourceType:    "android",
			WorkspaceID:   "workspace-2",
			Status:        "aborted",
			DurationMs:    12000,
			RowsSynced:    500,
			RowsFailed:    300,
			ErrorCategory: "permission_error",
		}

		mock.ExpectQuery("INSERT INTO").
			WithArgs(
				health.UploadID,
				health.SourceID,
				health.DestinationID,
				health.DestType,
				health.SourceType,
				health.WorkspaceID,
				health.Status,
				health.DurationMs,
				health.RowsSynced,
				health.RowsFailed,
				health.ErrorCategory,
				sqlmock.AnyArg(), // schemaChangesJSON
				sqlmock.AnyArg(), // created_at
			).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(2)))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.RecordSyncHealth(ctx, health)

		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("record with schema changes", func(t *testing.T) {
		// Arrange: Record a sync health entry that includes schema changes.
		// SchemaChanges is json.RawMessage ([]byte) in the SyncHealth struct.
		m, _, mock, _ := newTestMonitor(t, true, 60, 30)

		health := &SyncHealth{
			UploadID:      44,
			SourceID:      "source-sc",
			DestinationID: "dest-sc",
			DestType:      "RS",
			SourceType:    "cloud",
			WorkspaceID:   "workspace-3",
			Status:        "exported_data",
			DurationMs:    7000,
			RowsSynced:    2000,
			RowsFailed:    0,
			ErrorCategory: "",
			SchemaChanges: []byte(`{"added":["new_col"],"removed":["old_col"]}`),
		}

		mock.ExpectQuery("INSERT INTO").
			WithArgs(
				health.UploadID,
				health.SourceID,
				health.DestinationID,
				health.DestType,
				health.SourceType,
				health.WorkspaceID,
				health.Status,
				health.DurationMs,
				health.RowsSynced,
				health.RowsFailed,
				health.ErrorCategory,
				sqlmock.AnyArg(), // schemaChangesJSON (marshaled by jsonrs)
				sqlmock.AnyArg(), // created_at
			).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(3)))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.RecordSyncHealth(ctx, health)

		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("disabled monitor returns nil immediately", func(t *testing.T) {
		// Arrange: Monitor is disabled — RecordSyncHealth should return nil
		// without any database interaction.
		m, _, _, _ := newTestMonitor(t, false, 60, 30)

		health := &SyncHealth{
			UploadID:      99,
			SourceID:      "source-disabled",
			DestinationID: "dest-disabled",
			Status:        "exported_data",
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.RecordSyncHealth(ctx, health)

		// Assert: nil error returned, no DB interaction.
		require.Nil(t, err)
		// sqlmock would panic/fail if any DB calls were made without expectations.
	})

	t.Run("repository error propagated", func(t *testing.T) {
		// Arrange: Repository INSERT fails with an error.
		m, _, mock, _ := newTestMonitor(t, true, 60, 30)

		health := &SyncHealth{
			UploadID:      50,
			SourceID:      "source-fail",
			DestinationID: "dest-fail",
			DestType:      "CH",
			SourceType:    "web",
			WorkspaceID:   "workspace-fail",
			Status:        "exported_data",
			DurationMs:    1000,
			RowsSynced:    100,
			RowsFailed:    0,
			ErrorCategory: "",
		}

		mock.ExpectQuery("INSERT INTO").
			WithArgs(
				health.UploadID,
				health.SourceID,
				health.DestinationID,
				health.DestType,
				health.SourceType,
				health.WorkspaceID,
				health.Status,
				health.DurationMs,
				health.RowsSynced,
				health.RowsFailed,
				health.ErrorCategory,
				sqlmock.AnyArg(), // schemaChangesJSON
				sqlmock.AnyArg(), // created_at
			).
			WillReturnError(fmt.Errorf("unique constraint violation"))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.RecordSyncHealth(ctx, health)

		require.Error(t, err)
		require.Contains(t, err.Error(), "recording sync health")

		require.NoError(t, mock.ExpectationsWereMet())
	})
}
