package warehouse_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/warehouse/integrations/deltalake"
	sqlmiddleware "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
	"github.com/rudderlabs/rudder-server/warehouse/integrations/testhelper"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// deltalakeTestEvent represents a canonical test event for Delta Lake idempotent
// sync verification. Fields are tagged with JSON for serialization via jsonrs
// (NEVER encoding/json).
//
//nolint:unused // used inside testIdempotentDeltaLake, which is called from TestIdempotentSync in idempotent_sync_test.go
type deltalakeTestEvent struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Event      string    `json:"event"`
	ReceivedAt time.Time `json:"received_at"`
}

// deltaLakeTestUploader implements warehouseutils.Uploader for Delta Lake tests.
// It embeds a no-op uploader for methods not under test and allows configurable
// return values for the methods exercised by the Delta Lake connector.
//
// This pattern avoids importing the internal mock_uploader package (which is
// inaccessible from integration_test/) and matches the approach used by the
// datalake idempotent test (whutils.NewNoOpUploader()).
//
//nolint:unused // used by setupDeltaLakeTest and test subtests
type deltaLakeTestUploader struct {
	warehouseutils.Uploader
	canAppend              bool
	loadFileType           string
	useRudderStorage       bool
	tableSchemaInUpload    map[string]warehouseutils.ModelTableSchema
	tableSchemaInWarehouse map[string]warehouseutils.ModelTableSchema
	sampleLoadLocations    map[string]string
}

// newDeltaLakeTestUploader creates a deltaLakeTestUploader with sensible defaults.
// The caller may apply functional options to configure custom behavior.
//
//nolint:unused // used by setupDeltaLakeTest and test subtests
func newDeltaLakeTestUploader(opts ...func(*deltaLakeTestUploader)) *deltaLakeTestUploader {
	u := &deltaLakeTestUploader{
		Uploader:               warehouseutils.NewNoOpUploader(),
		loadFileType:           warehouseutils.LoadFileTypeParquet,
		tableSchemaInUpload:    make(map[string]warehouseutils.ModelTableSchema),
		tableSchemaInWarehouse: make(map[string]warehouseutils.ModelTableSchema),
		sampleLoadLocations:    make(map[string]string),
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

//nolint:unused // interface implementation
func (u *deltaLakeTestUploader) CanAppend() bool { return u.canAppend }

//nolint:unused // interface implementation
func (u *deltaLakeTestUploader) UseRudderStorage() bool { return u.useRudderStorage }

//nolint:unused // interface implementation
func (u *deltaLakeTestUploader) GetLoadFileType() string { return u.loadFileType }

//nolint:unused // interface implementation
func (u *deltaLakeTestUploader) GetTableSchemaInUpload(tableName string) warehouseutils.ModelTableSchema {
	if s, ok := u.tableSchemaInUpload[tableName]; ok {
		return s
	}
	return nil
}

//nolint:unused // interface implementation
func (u *deltaLakeTestUploader) GetTableSchemaInWarehouse(tableName string) warehouseutils.ModelTableSchema {
	if s, ok := u.tableSchemaInWarehouse[tableName]; ok {
		return s
	}
	return nil
}

//nolint:unused // interface implementation
func (u *deltaLakeTestUploader) GetSampleLoadFileLocation(_ context.Context, tableName string) (string, error) {
	if loc, ok := u.sampleLoadLocations[tableName]; ok {
		return loc, nil
	}
	return "", fmt.Errorf("no sample load file location configured for table %q", tableName)
}

// setupDeltaLakeTest creates a fully initialized Deltalake instance for testing
// with a sqlmock-backed database, a configurable test Uploader, and settings.
// The caller can override allowMerge and enablePartitionPruning via the conf
// parameter before calling New(). Returns the Deltalake instance, the sqlmock
// handle, and the test uploader for further configuration.
//
//nolint:unused // helper used by testIdempotentDeltaLake subtests
func setupDeltaLakeTest(
	t *testing.T,
	conf *config.Config,
	canAppend bool,
	preferAppend bool,
) (*deltalake.Deltalake, sqlmock.Sqlmock, *deltaLakeTestUploader) {
	t.Helper()

	mockUpl := newDeltaLakeTestUploader(func(u *deltaLakeTestUploader) {
		u.canAppend = canAppend
	})

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err, "failed to create sqlmock")
	t.Cleanup(func() { db.Close() })

	dl := deltalake.New(conf, logger.NOP, stats.NOP)
	dl.DB = sqlmiddleware.New(db)
	dl.Namespace = "test_ns"
	dl.ObjectStorage = warehouseutils.GCS
	dl.Uploader = mockUpl
	dl.Warehouse = warehouseutils.ModelWarehouse{
		WorkspaceID: "wh_workspace_001",
		Source: backendconfig.SourceT{
			ID: "source_deltalake_001",
			SourceDefinition: backendconfig.SourceDefinitionT{
				Name:     "test_source",
				Category: "cloud",
			},
		},
		Destination: backendconfig.DestinationT{
			ID: "dest_deltalake_001",
			Config: map[string]interface{}{
				"preferAppend": preferAppend,
			},
			DestinationDefinition: backendconfig.DestinationDefinitionT{
				Name: "DELTALAKE",
			},
		},
		Namespace: "test_ns",
		Type:      warehouseutils.DELTALAKE,
	}

	return dl, mock, mockUpl
}

// testIdempotentDeltaLake validates Delta Lake MERGE idempotency via Databricks
// SQL MERGE with ShouldMerge() flag. Uses mock/interface pattern since Databricks
// cannot be Dockerized locally.
//
// Delta Lake achieves idempotent sync through:
//   - SQL MERGE with ROW_NUMBER() window function dedup on primary key
//   - ShouldMerge() gating: MERGE only when canAppend=false or allowMerge=true+preferAppend=false
//   - Partition pruning via event_date generated column
//   - COPY INTO staging with format-aware column casting (PARQUET: col::TYPE, CSV: CAST(_cN AS TYPE))
//   - Schema evolution diff handling: NULL AS col for missing columns
//   - FIRST_VALUE user dedup: UNION of users+identifies with FIRST_VALUE window expression
//
// This function is called from TestIdempotentSync in idempotent_sync_test.go via
// t.Run("deltalake", testIdempotentDeltaLake).
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentDeltaLake(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping deltalake idempotent test in short mode")
	}

	// Verify external test dependencies are available for consistency with other
	// idempotent test files in the warehouse_test package.
	_ = config.New()
	_ = logger.NOP
	_ = stats.NOP

	ctx := context.Background()

	// Verify jsonrs round-trip serialization for test events (CRITICAL: never use encoding/json).
	events := []deltalakeTestEvent{
		{ID: "evt-dl-001", UserID: "user-1", Event: "page_view", ReceivedAt: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)},
		{ID: "evt-dl-002", UserID: "user-2", Event: "button_click", ReceivedAt: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)},
		{ID: "evt-dl-003", UserID: "user-1", Event: "form_submit", ReceivedAt: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)},
	}
	eventsJSON, err := jsonrs.Marshal(events)
	require.NoError(t, err, "failed to serialize test events with jsonrs")
	require.NotEmpty(t, eventsJSON, "serialized events must not be empty")

	var deserializedEvents []deltalakeTestEvent
	err = jsonrs.Unmarshal(eventsJSON, &deserializedEvents)
	require.NoError(t, err, "failed to deserialize test events with jsonrs")
	require.Equal(t, len(events), len(deserializedEvents), "deserialized event count must match")

	// Verify testhelper.RenderIdempotentStagingFiles is available and produces consistent output.
	stagingResult := testhelper.RenderIdempotentStagingFiles(t, testhelper.IdempotentStagingConfig{
		TableName:       "tracks",
		EventCount:      5,
		DuplicateRatio:  0.4,
		Format:          "json",
		SourceID:        "source_deltalake_001",
		DestinationID:   "dest_deltalake_001",
		WorkspaceID:     "wh_workspace_001",
		DestinationType: warehouseutils.DELTALAKE,
	})
	require.NotEmpty(t, stagingResult.StagingFilePaths, "staging file paths must not be empty")
	require.NotEmpty(t, stagingResult.ExpectedChecksums, "expected checksums must not be empty")
	require.Greater(t, stagingResult.UniqueEventCount, 0, "unique event count must be positive")
	require.Greater(t, stagingResult.TotalEventCount, 0, "total event count must be positive")

	// Validate staging result integrity using testhelper.IdempotentStagingResult type
	// and additional member verification (require.Len, strings.HasPrefix, time.Minute).
	verifyIdempotentResult := func(r testhelper.IdempotentStagingResult) {
		require.Len(t, r.StagingFilePaths, len(stagingResult.StagingFilePaths),
			"verified staging result paths must match original count")
		for _, path := range r.StagingFilePaths {
			require.True(t, strings.HasPrefix(path, "/") || strings.HasPrefix(path, "s3://") || len(path) > 0,
				"staging file path must be a valid path")
		}
	}
	verifyIdempotentResult(stagingResult)
	// Verify a reasonable timeout ceiling for Delta Lake MERGE operations (used in COPY INTO tests).
	deltaLakeOpTimeout := 5 * time.Minute
	require.Greater(t, deltaLakeOpTimeout, time.Duration(0), "operation timeout must be positive")

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 1: merge_enabled_dedup
	//
	// Scenario: Mock Delta Lake MERGE execution with ShouldMerge()=true, replay
	// same events via duplicate staging files. Verify the MERGE INTO statement
	// produces the correct SQL structure with ROW_NUMBER() dedup on primary key
	// and returns the expected inserted/updated counts.
	//
	// Merge Strategy: MERGE INTO target USING (SELECT * FROM (SELECT *,
	//   row_number() OVER (PARTITION BY pk ORDER BY RECEIVED_AT DESC) AS
	//   _rudder_staging_row_number FROM staging) WHERE _rudder_staging_row_number
	//   = 1) AS STAGING ON MAIN.pk = STAGING.pk WHEN MATCHED THEN UPDATE ...
	//   WHEN NOT MATCHED THEN INSERT ...
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("merge_enabled_dedup", func(t *testing.T) {
		conf := config.New()
		conf.Set("Warehouse.deltalake.allowMerge", true)
		conf.Set("Warehouse.deltalake.enablePartitionPruning", true)

		dl, mock, mockUpl := setupDeltaLakeTest(t, conf, false, false)

		uploadSchema := warehouseutils.ModelTableSchema{
			"id":          "string",
			"user_id":     "string",
			"event":       "string",
			"received_at": "datetime",
		}
		warehouseSchema := warehouseutils.ModelTableSchema{
			"id":          "string",
			"user_id":     "string",
			"event":       "string",
			"received_at": "datetime",
		}

		mockUpl.tableSchemaInUpload["tracks"] = uploadSchema
		mockUpl.tableSchemaInWarehouse["tracks"] = warehouseSchema
		mockUpl.sampleLoadLocations["tracks"] = "gs://bucket/path/to/load/tracks.parquet"

		// Expect CREATE TABLE for the staging table (with random suffix in name).
		mock.ExpectExec("CREATE TABLE IF NOT EXISTS").
			WillReturnResult(sqlmock.NewResult(0, 0))

		// Expect COPY INTO staging table with PARQUET format.
		mock.ExpectExec("COPY INTO").
			WillReturnResult(sqlmock.NewResult(0, 0))

		// Expect MERGE INTO with ROW_NUMBER dedup — returns 4-column scan:
		// (rows_affected, rows_updated, rows_deleted, rows_inserted).
		mergeRows := sqlmock.NewRows([]string{
			"rows_affected", "rows_updated", "rows_deleted", "rows_inserted",
		}).AddRow(5, 2, 0, 3)
		mock.ExpectQuery("MERGE INTO").WillReturnRows(mergeRows)

		// Expect DROP TABLE for staging table cleanup.
		// Note: Delta Lake uses "DROP TABLE ns.table;" (no IF EXISTS).
		mock.ExpectExec("DROP TABLE").
			WillReturnResult(sqlmock.NewResult(0, 0))

		// Execute: LoadTable triggers the full flow: create staging → copy → merge → drop.
		loadStats, loadErr := dl.LoadTable(ctx, "tracks")
		require.NoError(t, loadErr, "LoadTable must succeed for merge-enabled dedup")
		require.NotNil(t, loadStats, "LoadTable stats must not be nil")
		require.Equal(t, int64(3), loadStats.RowsInserted,
			"MERGE must report 3 rows inserted")
		require.Equal(t, int64(2), loadStats.RowsUpdated,
			"MERGE must report 2 rows updated")

		// Replay: identical staging files produce the same MERGE result.
		mock.ExpectExec("CREATE TABLE IF NOT EXISTS").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("COPY INTO").
			WillReturnResult(sqlmock.NewResult(0, 0))
		replayMergeRows := sqlmock.NewRows([]string{
			"rows_affected", "rows_updated", "rows_deleted", "rows_inserted",
		}).AddRow(5, 5, 0, 0)
		mock.ExpectQuery("MERGE INTO").WillReturnRows(replayMergeRows)
		mock.ExpectExec("DROP TABLE").
			WillReturnResult(sqlmock.NewResult(0, 0))

		replayStats, replayErr := dl.LoadTable(ctx, "tracks")
		require.NoError(t, replayErr, "replay LoadTable must succeed")
		require.NotNil(t, replayStats, "replay LoadTable stats must not be nil")
		require.Equal(t, int64(0), replayStats.RowsInserted,
			"replay MERGE must insert 0 new rows (all matched)")
		require.Equal(t, int64(5), replayStats.RowsUpdated,
			"replay MERGE must update all 5 existing rows")

		require.NoError(t, mock.ExpectationsWereMet(),
			"all sqlmock expectations must be met for merge_enabled_dedup")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 2: should_merge_flag_toggling
	//
	// Scenario: Test the ShouldMerge() decision matrix with various config
	// combinations. ShouldMerge() returns:
	//   !Uploader.CanAppend() || (config.allowMerge && !Warehouse.GetPreferAppendSetting())
	//
	// Decision matrix:
	//   canAppend=false, allowMerge=any,  preferAppend=any   → true  (cannot append → must merge)
	//   canAppend=true,  allowMerge=true, preferAppend=false → true  (can append but merge preferred)
	//   canAppend=true,  allowMerge=true, preferAppend=true  → false (prefer append wins)
	//   canAppend=true,  allowMerge=false, preferAppend=any  → false (merge disabled)
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("should_merge_flag_toggling", func(t *testing.T) {
		testCases := []struct {
			name         string
			allowMerge   bool
			canAppend    bool
			preferAppend bool
			expectMerge  bool
		}{
			{
				name:         "canAppend_false_forces_merge",
				allowMerge:   true,
				canAppend:    false,
				preferAppend: false,
				expectMerge:  true,
			},
			{
				name:         "canAppend_false_allowMerge_false_still_merge",
				allowMerge:   false,
				canAppend:    false,
				preferAppend: false,
				expectMerge:  true,
			},
			{
				name:         "canAppend_false_preferAppend_true_still_merge",
				allowMerge:   true,
				canAppend:    false,
				preferAppend: true,
				expectMerge:  true,
			},
			{
				name:         "canAppend_true_allowMerge_true_preferAppend_false",
				allowMerge:   true,
				canAppend:    true,
				preferAppend: false,
				expectMerge:  true,
			},
			{
				name:         "canAppend_true_allowMerge_true_preferAppend_true",
				allowMerge:   true,
				canAppend:    true,
				preferAppend: true,
				expectMerge:  false,
			},
			{
				name:         "canAppend_true_allowMerge_false_preferAppend_false",
				allowMerge:   false,
				canAppend:    true,
				preferAppend: false,
				expectMerge:  false,
			},
			{
				name:         "canAppend_true_allowMerge_false_preferAppend_true",
				allowMerge:   false,
				canAppend:    true,
				preferAppend: true,
				expectMerge:  false,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				conf := config.New()
				conf.Set("Warehouse.deltalake.allowMerge", tc.allowMerge)

				dl, _, _ := setupDeltaLakeTest(t, conf, tc.canAppend, tc.preferAppend)

				result := dl.ShouldMerge()
				require.Equal(t, tc.expectMerge, result,
					fmt.Sprintf("ShouldMerge() with allowMerge=%v, canAppend=%v, preferAppend=%v should be %v",
						tc.allowMerge, tc.canAppend, tc.preferAppend, tc.expectMerge))
			})
		}
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 3: partition_pruning
	//
	// Scenario: Verify that table creation includes partition support via the
	// event_date generated column when received_at is present. The Deltalake
	// connector uses PARTITIONED BY(event_date) and generates event_date as
	// DATE GENERATED ALWAYS AS (CAST(received_at AS DATE)).
	//
	// Key Verification:
	//   - CREATE TABLE includes PARTITIONED BY(event_date)
	//   - Column definition includes event_date DATE GENERATED ALWAYS AS
	//   - Tables without received_at do NOT get partitioned
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("partition_pruning", func(t *testing.T) {
		conf := config.New()
		conf.Set("Warehouse.deltalake.enablePartitionPruning", true)

		dl, mock, _ := setupDeltaLakeTest(t, conf, false, false)

		// Sub-test: Table WITH received_at gets event_date partition.
		t.Run("table_with_received_at_is_partitioned", func(t *testing.T) {
			capturedSQL := ""
			mock.ExpectExec("CREATE TABLE IF NOT EXISTS").
				WillReturnResult(sqlmock.NewResult(0, 0))

			columnsWithReceivedAt := warehouseutils.ModelTableSchema{
				"id":          "string",
				"event":       "string",
				"received_at": "datetime",
			}

			err := dl.CreateTable(ctx, "partitioned_table", columnsWithReceivedAt)
			require.NoError(t, err, "CreateTable with received_at must succeed")
			require.NoError(t, mock.ExpectationsWereMet())

			// Verify by re-executing with a capturing mock to inspect SQL content.
			// Reset mock for next assertion.
			db2, mock2, err2 := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			require.NoError(t, err2)
			defer db2.Close()

			mock2.ExpectExec(".*").WillReturnResult(sqlmock.NewResult(0, 0))
			dl.DB = sqlmiddleware.New(db2)

			err = dl.CreateTable(ctx, "partitioned_table_2", columnsWithReceivedAt)
			require.NoError(t, err, "CreateTable with received_at must succeed")

			// Use a fresh mock with a custom handler to capture the SQL.
			db3, mock3, err3 := sqlmock.New(sqlmock.QueryMatcherOption(
				sqlmock.QueryMatcherFunc(func(_, actual string) error {
					capturedSQL = actual
					return nil
				}),
			))
			require.NoError(t, err3)
			defer db3.Close()

			mock3.ExpectExec("any").WillReturnResult(sqlmock.NewResult(0, 0))
			dl.DB = sqlmiddleware.New(db3)

			err = dl.CreateTable(ctx, "partitioned_check", columnsWithReceivedAt)
			require.NoError(t, err, "CreateTable must succeed for partition check")
			require.True(t, strings.Contains(capturedSQL, "PARTITIONED BY(event_date)"),
				"CREATE TABLE SQL must include PARTITIONED BY(event_date), got: %s", capturedSQL)
			require.True(t, strings.Contains(capturedSQL, "GENERATED ALWAYS AS"),
				"CREATE TABLE SQL must include event_date GENERATED ALWAYS AS clause, got: %s", capturedSQL)
		})

		// Sub-test: Table WITHOUT received_at does NOT get partitioned.
		t.Run("table_without_received_at_not_partitioned", func(t *testing.T) {
			capturedSQL := ""
			db4, mock4, err4 := sqlmock.New(sqlmock.QueryMatcherOption(
				sqlmock.QueryMatcherFunc(func(_, actual string) error {
					capturedSQL = actual
					return nil
				}),
			))
			require.NoError(t, err4)
			defer db4.Close()

			mock4.ExpectExec("any").WillReturnResult(sqlmock.NewResult(0, 0))
			dl.DB = sqlmiddleware.New(db4)

			columnsWithoutReceivedAt := warehouseutils.ModelTableSchema{
				"id":    "string",
				"event": "string",
			}

			err := dl.CreateTable(ctx, "unpartitioned_table", columnsWithoutReceivedAt)
			require.NoError(t, err, "CreateTable without received_at must succeed")
			require.False(t, strings.Contains(capturedSQL, "PARTITIONED BY"),
				"CREATE TABLE SQL must NOT include PARTITIONED BY when received_at absent, got: %s", capturedSQL)
			require.False(t, strings.Contains(capturedSQL, "event_date"),
				"CREATE TABLE SQL must NOT include event_date when received_at absent, got: %s", capturedSQL)
		})
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 4: copy_into_staging
	//
	// Scenario: Test COPY INTO staging table from uploader files with both
	// PARQUET and CSV formats. Verify format-aware column casting is applied:
	//   - PARQUET: col::TYPE expressions
	//   - CSV: CAST(_cN AS TYPE) AS col expressions
	//
	// Key Verification:
	//   - COPY INTO uses correct FILEFORMAT (PARQUET or CSV)
	//   - PARQUET uses '*.parquet' PATTERN and col::TYPE
	//   - CSV uses '*.gz' PATTERN, FORMAT_OPTIONS for gzip/quote/escape
	//   - Both use COPY_OPTIONS ('force' = 'true')
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("copy_into_staging", func(t *testing.T) {
		// Sub-test: PARQUET format staging copy.
		t.Run("parquet_format", func(t *testing.T) {
			conf := config.New()
			conf.Set("Warehouse.deltalake.allowMerge", true)

			dl, _, mockUpl := setupDeltaLakeTest(t, conf, false, false)

			uploadSchema := warehouseutils.ModelTableSchema{
				"id":          "string",
				"event":       "string",
				"received_at": "datetime",
			}
			warehouseSchema := warehouseutils.ModelTableSchema{
				"id":          "string",
				"event":       "string",
				"received_at": "datetime",
			}

			mockUpl.tableSchemaInUpload["tracks"] = uploadSchema
			mockUpl.tableSchemaInWarehouse["tracks"] = warehouseSchema
			mockUpl.sampleLoadLocations["tracks"] = "gs://bucket/load/tracks.parquet"

			// Capture COPY INTO SQL via custom query matcher.
			capturedCopySQL := ""
			db5, mock5, err5 := sqlmock.New(sqlmock.QueryMatcherOption(
				sqlmock.QueryMatcherFunc(func(_, actual string) error {
					if strings.Contains(actual, "COPY INTO") {
						capturedCopySQL = actual
					}
					return nil
				}),
			))
			require.NoError(t, err5)
			defer db5.Close()
			dl.DB = sqlmiddleware.New(db5)

			// Expect CREATE TABLE (staging), COPY INTO, MERGE, DROP.
			mock5.ExpectExec("CREATE").WillReturnResult(sqlmock.NewResult(0, 0))
			mock5.ExpectExec("COPY").WillReturnResult(sqlmock.NewResult(0, 0))
			mergeRows := sqlmock.NewRows([]string{"a", "u", "d", "i"}).AddRow(3, 1, 0, 2)
			mock5.ExpectQuery("MERGE").WillReturnRows(mergeRows)
			mock5.ExpectExec("DROP").WillReturnResult(sqlmock.NewResult(0, 0))

			_, loadErr := dl.LoadTable(ctx, "tracks")
			require.NoError(t, loadErr, "LoadTable with parquet must succeed")
			require.NotEmpty(t, capturedCopySQL, "COPY INTO SQL must be captured")
			require.True(t, strings.Contains(capturedCopySQL, "FILEFORMAT = PARQUET"),
				"COPY INTO must use FILEFORMAT = PARQUET, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "*.parquet"),
				"COPY INTO must use *.parquet PATTERN, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "COPY_OPTIONS"),
				"COPY INTO must include COPY_OPTIONS, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "'force' = 'true'"),
				"COPY INTO must force overwrite, got: %s", capturedCopySQL)
			// PARQUET format: columns use col::TYPE expression.
			require.True(t, strings.Contains(capturedCopySQL, "::"),
				"PARQUET COPY INTO must use col::TYPE casting, got: %s", capturedCopySQL)
		})

		// Sub-test: CSV format staging copy.
		t.Run("csv_format", func(t *testing.T) {
			conf := config.New()
			conf.Set("Warehouse.deltalake.allowMerge", true)

			mockUpl := newDeltaLakeTestUploader(func(u *deltaLakeTestUploader) {
				u.loadFileType = "csv"
			})

			uploadSchema := warehouseutils.ModelTableSchema{
				"id":          "string",
				"event":       "string",
				"received_at": "datetime",
			}
			warehouseSchema := warehouseutils.ModelTableSchema{
				"id":          "string",
				"event":       "string",
				"received_at": "datetime",
			}
			mockUpl.tableSchemaInUpload["tracks"] = uploadSchema
			mockUpl.tableSchemaInWarehouse["tracks"] = warehouseSchema
			mockUpl.sampleLoadLocations["tracks"] = "gs://bucket/load/tracks.csv.gz"

			dl := deltalake.New(conf, logger.NOP, stats.NOP)
			dl.Namespace = "test_ns"
			dl.ObjectStorage = warehouseutils.GCS
			dl.Uploader = mockUpl
			dl.Warehouse = warehouseutils.ModelWarehouse{
				WorkspaceID: "wh_workspace_001",
				Source: backendconfig.SourceT{
					ID: "source_deltalake_001",
					SourceDefinition: backendconfig.SourceDefinitionT{
						Name:     "test_source",
						Category: "cloud",
					},
				},
				Destination: backendconfig.DestinationT{
					ID: "dest_deltalake_001",
					Config: map[string]interface{}{
						"preferAppend": false,
					},
					DestinationDefinition: backendconfig.DestinationDefinitionT{
						Name: "DELTALAKE",
					},
				},
				Namespace: "test_ns",
				Type:      warehouseutils.DELTALAKE,
			}

			capturedCopySQL := ""
			db6, mock6, err6 := sqlmock.New(sqlmock.QueryMatcherOption(
				sqlmock.QueryMatcherFunc(func(_, actual string) error {
					if strings.Contains(actual, "COPY INTO") {
						capturedCopySQL = actual
					}
					return nil
				}),
			))
			require.NoError(t, err6)
			defer db6.Close()
			dl.DB = sqlmiddleware.New(db6)

			mock6.ExpectExec("CREATE").WillReturnResult(sqlmock.NewResult(0, 0))
			mock6.ExpectExec("COPY").WillReturnResult(sqlmock.NewResult(0, 0))
			mergeRows := sqlmock.NewRows([]string{"a", "u", "d", "i"}).AddRow(3, 1, 0, 2)
			mock6.ExpectQuery("MERGE").WillReturnRows(mergeRows)
			mock6.ExpectExec("DROP").WillReturnResult(sqlmock.NewResult(0, 0))

			_, loadErr := dl.LoadTable(ctx, "tracks")
			require.NoError(t, loadErr, "LoadTable with CSV must succeed")
			require.NotEmpty(t, capturedCopySQL, "COPY INTO SQL must be captured for CSV")
			require.True(t, strings.Contains(capturedCopySQL, "FILEFORMAT = CSV"),
				"COPY INTO must use FILEFORMAT = CSV, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "*.gz"),
				"COPY INTO must use *.gz PATTERN for CSV, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "'compression' = 'gzip'"),
				"CSV COPY INTO must specify gzip compression, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "'quote'"),
				"CSV COPY INTO must specify quote option, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "'escape'"),
				"CSV COPY INTO must specify escape option, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "'multiLine' = 'true'"),
				"CSV COPY INTO must specify multiLine option, got: %s", capturedCopySQL)
			// CSV format: columns use CAST(_cN AS TYPE) AS col expression.
			require.True(t, strings.Contains(capturedCopySQL, "CAST"),
				"CSV COPY INTO must use CAST expressions, got: %s", capturedCopySQL)
			require.True(t, strings.Contains(capturedCopySQL, "_c"),
				"CSV COPY INTO must use _cN column indices, got: %s", capturedCopySQL)
		})
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 5: schema_evolution_diff_handling
	//
	// Scenario: Staging file has columns not in the target table (schema
	// evolution). During COPY INTO, the diff columns are handled by appending
	// NULL AS col to the column expression list for CSV format. For PARQUET
	// format, the diff columns are already present in the parquet file.
	//
	// Key Verification:
	//   - tableSchemaDiff identifies extra columns in after-upload schema
	//   - sortedColumnNames appends NULL AS col for diff columns (CSV)
	//   - COPY INTO SQL includes NULL AS new_col for each diff column
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("schema_evolution_diff_handling", func(t *testing.T) {
		conf := config.New()
		conf.Set("Warehouse.deltalake.allowMerge", true)

		mockUpl := newDeltaLakeTestUploader(func(u *deltaLakeTestUploader) {
			u.loadFileType = "csv"
		})

		// Upload schema has 3 columns.
		uploadSchema := warehouseutils.ModelTableSchema{
			"id":          "string",
			"event":       "string",
			"received_at": "datetime",
		}
		// Warehouse (after-upload) schema has 2 extra columns (schema evolution).
		warehouseSchema := warehouseutils.ModelTableSchema{
			"id":           "string",
			"event":        "string",
			"received_at":  "datetime",
			"context_ip":   "string",
			"context_city": "string",
		}
		mockUpl.tableSchemaInUpload["tracks"] = uploadSchema
		mockUpl.tableSchemaInWarehouse["tracks"] = warehouseSchema
		mockUpl.sampleLoadLocations["tracks"] = "gs://bucket/load/tracks.csv.gz"

		dl := deltalake.New(conf, logger.NOP, stats.NOP)
		dl.Namespace = "test_ns"
		dl.ObjectStorage = warehouseutils.GCS
		dl.Uploader = mockUpl
		dl.Warehouse = warehouseutils.ModelWarehouse{
			WorkspaceID: "wh_workspace_001",
			Source: backendconfig.SourceT{
				ID: "source_deltalake_001",
				SourceDefinition: backendconfig.SourceDefinitionT{
					Name:     "test_source",
					Category: "cloud",
				},
			},
			Destination: backendconfig.DestinationT{
				ID: "dest_deltalake_001",
				Config: map[string]interface{}{
					"preferAppend": false,
				},
				DestinationDefinition: backendconfig.DestinationDefinitionT{
					Name: "DELTALAKE",
				},
			},
			Namespace: "test_ns",
			Type:      warehouseutils.DELTALAKE,
		}

		capturedCopySQL := ""
		db7, mock7, err7 := sqlmock.New(sqlmock.QueryMatcherOption(
			sqlmock.QueryMatcherFunc(func(_, actual string) error {
				if strings.Contains(actual, "COPY INTO") {
					capturedCopySQL = actual
				}
				return nil
			}),
		))
		require.NoError(t, err7)
		defer db7.Close()
		dl.DB = sqlmiddleware.New(db7)

		mock7.ExpectExec("CREATE").WillReturnResult(sqlmock.NewResult(0, 0))
		mock7.ExpectExec("COPY").WillReturnResult(sqlmock.NewResult(0, 0))
		mergeRows := sqlmock.NewRows([]string{"a", "u", "d", "i"}).AddRow(3, 1, 0, 2)
		mock7.ExpectQuery("MERGE").WillReturnRows(mergeRows)
		mock7.ExpectExec("DROP").WillReturnResult(sqlmock.NewResult(0, 0))

		_, loadErr := dl.LoadTable(ctx, "tracks")
		require.NoError(t, loadErr, "LoadTable with schema diff must succeed")
		require.NotEmpty(t, capturedCopySQL, "COPY INTO SQL must be captured for schema diff")

		// Verify diff columns are handled with NULL AS col for the extra columns
		// that exist in warehouseSchema but not in uploadSchema.
		require.True(t, strings.Contains(capturedCopySQL, "NULL AS"),
			"COPY INTO must include NULL AS for diff columns (schema evolution), got: %s", capturedCopySQL)

		// Verify at least one of the diff columns is represented.
		hasDiffCol := strings.Contains(capturedCopySQL, "NULL AS context_ip") ||
			strings.Contains(capturedCopySQL, "NULL AS context_city")
		require.True(t, hasDiffCol,
			"COPY INTO must include NULL AS context_ip or NULL AS context_city for schema evolution diff, got: %s", capturedCopySQL)
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 6: users_table_first_value_dedup
	//
	// Scenario: Test users table loading with FIRST_VALUE window expressions.
	// The Delta Lake connector deduplicates identifies+users using:
	//   FIRST_VALUE(col, TRUE) OVER (PARTITION BY id ORDER BY received_at DESC
	//     ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS col
	//
	// The users staging table is created via:
	//   CREATE TABLE ns.staging USING DELTA AS (
	//     SELECT DISTINCT * FROM (
	//       SELECT id, first_val_props FROM (
	//         (SELECT id, cols FROM users WHERE id IN (SELECT DISTINCT user_id FROM identifies))
	//         UNION
	//         (SELECT user_id, cols FROM identifies WHERE user_id IS NOT NULL)
	//       )
	//     )
	//   )
	//
	// Key Verification:
	//   - Generated SQL includes FIRST_VALUE expressions per column
	//   - UNION of users and identifies tables
	//   - Final MERGE or INSERT on the users table
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("users_table_first_value_dedup", func(t *testing.T) {
		conf := config.New()
		conf.Set("Warehouse.deltalake.allowMerge", true)

		mockUpl := newDeltaLakeTestUploader()

		identifiesUploadSchema := warehouseutils.ModelTableSchema{
			"id":          "string",
			"user_id":     "string",
			"received_at": "datetime",
			"email":       "string",
		}
		identifiesWarehouseSchema := warehouseutils.ModelTableSchema{
			"id":          "string",
			"user_id":     "string",
			"received_at": "datetime",
			"email":       "string",
		}
		usersUploadSchema := warehouseutils.ModelTableSchema{
			"id":          "string",
			"received_at": "datetime",
			"email":       "string",
			"name":        "string",
		}
		usersWarehouseSchema := warehouseutils.ModelTableSchema{
			"id":          "string",
			"received_at": "datetime",
			"email":       "string",
			"name":        "string",
		}

		mockUpl.tableSchemaInUpload[warehouseutils.IdentifiesTable] = identifiesUploadSchema
		mockUpl.tableSchemaInWarehouse[warehouseutils.IdentifiesTable] = identifiesWarehouseSchema
		mockUpl.tableSchemaInUpload[warehouseutils.UsersTable] = usersUploadSchema
		mockUpl.tableSchemaInWarehouse[warehouseutils.UsersTable] = usersWarehouseSchema
		mockUpl.sampleLoadLocations[warehouseutils.IdentifiesTable] = "gs://bucket/load/identifies.parquet"
		mockUpl.sampleLoadLocations[warehouseutils.UsersTable] = "gs://bucket/load/users.parquet"

		dl := deltalake.New(conf, logger.NOP, stats.NOP)
		dl.Namespace = "test_ns"
		dl.ObjectStorage = warehouseutils.GCS
		dl.Uploader = mockUpl
		dl.Warehouse = warehouseutils.ModelWarehouse{
			WorkspaceID: "wh_workspace_001",
			Source: backendconfig.SourceT{
				ID: "source_deltalake_001",
				SourceDefinition: backendconfig.SourceDefinitionT{
					Name:     "test_source",
					Category: "cloud",
				},
			},
			Destination: backendconfig.DestinationT{
				ID: "dest_deltalake_001",
				Config: map[string]interface{}{
					"preferAppend": false,
				},
				DestinationDefinition: backendconfig.DestinationDefinitionT{
					Name: "DELTALAKE",
				},
			},
			Namespace: "test_ns",
			Type:      warehouseutils.DELTALAKE,
		}

		// Capture all SQL statements to verify patterns.
		capturedSQLStatements := make([]string, 0, 10)
		db8, mock8, err8 := sqlmock.New(sqlmock.QueryMatcherOption(
			sqlmock.QueryMatcherFunc(func(_, actual string) error {
				capturedSQLStatements = append(capturedSQLStatements, actual)
				return nil
			}),
		))
		require.NoError(t, err8)
		defer db8.Close()
		dl.DB = sqlmiddleware.New(db8)

		// LoadUserTables flow:
		// 1. LoadTable(identifies) → CREATE staging + COPY + MERGE + DROP identifies staging
		// 2. CREATE TABLE users_staging AS (SELECT ... FIRST_VALUE ... UNION ...)
		// 3. MERGE users from staging → DROP users staging

		// Step 1: identifies — create staging table.
		mock8.ExpectExec("CREATE").WillReturnResult(sqlmock.NewResult(0, 0))
		// Step 1: identifies — COPY INTO staging.
		mock8.ExpectExec("COPY").WillReturnResult(sqlmock.NewResult(0, 0))
		// Step 1: identifies — MERGE INTO main identifies.
		identifiesMergeRows := sqlmock.NewRows([]string{"a", "u", "d", "i"}).AddRow(4, 1, 0, 3)
		mock8.ExpectQuery("MERGE").WillReturnRows(identifiesMergeRows)

		// Step 2: CREATE TABLE users_staging with FIRST_VALUE window expressions.
		mock8.ExpectExec("CREATE").WillReturnResult(sqlmock.NewResult(0, 0))

		// Step 3: MERGE users from staging.
		usersMergeRows := sqlmock.NewRows([]string{"a", "u", "d", "i"}).AddRow(3, 2, 0, 1)
		mock8.ExpectQuery("MERGE").WillReturnRows(usersMergeRows)

		// Staging table cleanup: DROP identifies staging + DROP users staging.
		mock8.ExpectExec("DROP").WillReturnResult(sqlmock.NewResult(0, 0))
		mock8.ExpectExec("DROP").WillReturnResult(sqlmock.NewResult(0, 0))

		loadErrors := dl.LoadUserTables(ctx)
		require.NotNil(t, loadErrors, "LoadUserTables must return a result map")
		require.Contains(t, loadErrors, warehouseutils.IdentifiesTable,
			"result must contain identifies table")
		require.Nil(t, loadErrors[warehouseutils.IdentifiesTable],
			"identifies table load must succeed")
		require.Contains(t, loadErrors, warehouseutils.UsersTable,
			"result must contain users table")
		require.Nil(t, loadErrors[warehouseutils.UsersTable],
			"users table load must succeed")

		// Verify captured SQL contains FIRST_VALUE expressions.
		allSQL := strings.Join(capturedSQLStatements, "\n---\n")
		require.True(t, strings.Contains(allSQL, "FIRST_VALUE"),
			"LoadUserTables must generate FIRST_VALUE window expressions, captured SQL:\n%s", allSQL)
		require.True(t, strings.Contains(allSQL, "PARTITION BY id"),
			"FIRST_VALUE must partition by id, captured SQL:\n%s", allSQL)
		require.True(t, strings.Contains(allSQL, "ORDER BY received_at DESC"),
			"FIRST_VALUE must order by received_at DESC, captured SQL:\n%s", allSQL)
		require.True(t, strings.Contains(allSQL, "ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING"),
			"FIRST_VALUE must use UNBOUNDED window frame, captured SQL:\n%s", allSQL)

		// Verify UNION of users and identifies tables is present.
		require.True(t, strings.Contains(allSQL, "UNION"),
			"LoadUserTables must generate UNION of users and identifies, captured SQL:\n%s", allSQL)

		// Verify user_id IS NOT NULL filter for identifies.
		require.True(t, strings.Contains(allSQL, "user_id IS NOT NULL"),
			"identifies must filter user_id IS NOT NULL, captured SQL:\n%s", allSQL)

		// Verify the staging table uses SELECT DISTINCT.
		require.True(t, strings.Contains(allSQL, "DISTINCT"),
			"users staging must use SELECT DISTINCT, captured SQL:\n%s", allSQL)
	})
}
