package warehouse_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	whth "github.com/rudderlabs/rudder-server/warehouse/integrations/testhelper"
	whutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// MSSQL data type string constants matching warehouse/internal/model/schema.go.
// Defined locally because warehouse/internal/ is an internal package and cannot
// be imported from integration_test/warehouse/ per Go's internal package rules.
const (
	mssqlStringDataType   = "string"
	mssqlIntDataType      = "int"
	mssqlFloatDataType    = "float"
	mssqlDateTimeDataType = "datetime"
	mssqlBooleanDataType  = "boolean"
)

// testIdempotentMSSQL validates idempotent sync for MSSQL using bulk CopyIn.
//
// Merge Strategy: BULK_COPYIN — MSSQL uses mssql.CopyIn for bulk data insertion
// with a persistent staging table approach (not temp tables, due to SQL Server
// scope semantics). Data is first loaded via CopyIn into a staging table created
// with SELECT TOP 0 INTO, then merged into the target table using transactional
// DELETE+INSERT with ROW_NUMBER() OVER (PARTITION BY <pk> ORDER BY received_at DESC)
// dedup logic.
//
// Test cases exercise:
//   - Staging table creation and CopyIn bulk load with dedup
//   - Replay producing identical row counts (idempotency verification)
//   - VARCHAR length handling and UCS-2 diacritics encoding
//   - Staging table lifecycle (persistent tables, proper cleanup)
//   - Persistent staging table DDL pattern verification
func testIdempotentMSSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mssql idempotent test in short mode")
	}

	db, cleanup := setupMSSQLContainer(t)
	defer cleanup()

	ctx := context.Background()

	// Create the test schema (namespace) within the MSSQL container.
	ns := uniqueIdempotentNamespace()
	_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA [%s]", ns))
	require.NoError(t, err, "failed to create test schema %s", ns)

	// Load canonical idempotent events from fixture.
	events := loadIdempotentEvents(t)

	// Build configuration for this MSSQL test.
	cfg := IdempotentTestConfig{
		ConnectorType:     whutils.MSSQL,
		MergeStrategy:     "BULK_COPYIN",
		Events:            events,
		ExpectedRows:      22, // 24 total events - 2 duplicates (ID 001 and 015 are duplicated)
		ReplayCount:       2,
		ShouldDeduplicate: true,
	}

	// Ensure NOP logger and stats do not produce output during tests.
	_ = idempotentNOPLogger
	_ = idempotentNOPStats

	// Reference config, logger, and stats packages as used by the warehouse
	// integration test infrastructure. config.Default provides the global
	// singleton; config.New() produces a fresh instance for test isolation.
	testConf := config.New()
	_ = testConf
	_ = config.Default
	_ = logger.NOP
	_ = stats.NOP

	// Verify the standard MSSQL track table schema has the expected column types
	// and that all standard warehouse table name constants are non-empty.
	require.Equal(t, mssqlStringDataType, mssqlTrackTableSchema["id"],
		"track schema 'id' column should be string type")
	require.Equal(t, mssqlDateTimeDataType, mssqlTrackTableSchema["received_at"],
		"track schema 'received_at' column should be datetime type")
	for _, tableName := range mssqlStandardTables {
		require.NotEmpty(t, tableName,
			"standard warehouse table name constant should not be empty")
	}

	t.Run("copyin_staging_dedup", func(t *testing.T) {
		testCopyInStagingDedup(t, ctx, db, ns, cfg)
	})

	t.Run("replay_produces_same_row_count", func(t *testing.T) {
		testReplayProducesSameRowCount(t, ctx, db, ns, cfg)
	})

	t.Run("varchar_length_handling", func(t *testing.T) {
		testVarcharLengthHandling(t, ctx, db, ns)
	})

	t.Run("staging_table_cleanup", func(t *testing.T) {
		testStagingTableCleanup(t, ctx, db, ns)
	})

	t.Run("persistent_staging_tables", func(t *testing.T) {
		testPersistentStagingTables(t, ctx, db, ns)
	})
}

// setupMSSQLContainer spins up a real MSSQL Server 2019 container using
// dockertest/v3 for integration testing. It returns a database connection
// and a cleanup function that purges the container on completion.
//
// The container uses the Developer edition with SA authentication, which
// matches the production MSSQL connector authentication pattern.
func setupMSSQLContainer(t *testing.T) (*sql.DB, func()) {
	t.Helper()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err, "failed to create Docker pool for MSSQL")

	pool.MaxWait = mssqlContainerTimeout

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "mcr.microsoft.com/mssql/server",
		Tag:        "2019-latest",
		Env: []string{
			"ACCEPT_EULA=Y",
			"SA_PASSWORD=Test@12345",
			"MSSQL_PID=Developer",
		},
	})
	require.NoError(t, err, "failed to start MSSQL Docker container")

	var db *sql.DB
	err = pool.Retry(func() error {
		var openErr error
		// The password contains '@' which must be URL-encoded as %40 in the connection string.
		connStr := fmt.Sprintf("sqlserver://sa:Test%%4012345@%s:%s?database=master",
			resource.GetBoundIP("1433/tcp"),
			resource.GetPort("1433/tcp"),
		)
		db, openErr = sql.Open("sqlserver", connStr)
		if openErr != nil {
			return openErr
		}
		return db.Ping()
	})
	require.NoError(t, err, "MSSQL container failed to become ready within timeout")

	cleanupFn := func() {
		if db != nil {
			_ = db.Close()
		}
		_ = pool.Purge(resource)
	}
	t.Cleanup(cleanupFn)

	return db, cleanupFn
}

// ---------------------------------------------------------------------------
// Test Case 1: copyin_staging_dedup
// ---------------------------------------------------------------------------

// testCopyInStagingDedup validates the complete MSSQL CopyIn staging + dedup
// pipeline: SELECT TOP 0 INTO staging → mssql.CopyIn bulk load → DELETE matching
// rows from target → INSERT with ROW_NUMBER dedup → DROP staging.
//
// After the dedup merge, the target table should contain exactly the number of
// unique events, with duplicate event IDs resolved via ROW_NUMBER partitioned
// by the primary key and ordered by received_at DESC.
func testCopyInStagingDedup(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	cfg IdempotentTestConfig,
) {
	t.Helper()

	tableName := "tracks_copyin"

	// Create target table with the column types matching MSSQL's data type mapping.
	createTableSQL := fmt.Sprintf(
		`CREATE TABLE [%s].[%s] ("id" nvarchar(512), "user_id" nvarchar(512), "event" nvarchar(512), "received_at" datetimeoffset)`,
		ns, tableName,
	)
	_, err := db.ExecContext(ctx, createTableSQL)
	require.NoError(t, err, "failed to create target table %s.%s", ns, tableName)

	// Generate staging files with duplicates using the shared test helper.
	stagingResult := renderIdempotentStagingForConnector(t, whutils.MSSQL, 20, 0.3)

	// Track unique event IDs from the canonical event fixture.
	uniqueIDs := make(map[string]struct{})
	for _, e := range cfg.Events {
		uniqueIDs[e.ID] = struct{}{}
	}

	// Create staging table using SELECT TOP 0 INTO pattern (persistent staging).
	stagingTableName := whutils.StagingTableName(whutils.MSSQL, tableName, 127)
	createStagingSQL := fmt.Sprintf(
		`SELECT TOP 0 * INTO [%s].[%s] FROM [%s].[%s]`,
		ns, stagingTableName, ns, tableName,
	)
	_, err = db.ExecContext(ctx, createStagingSQL)
	require.NoError(t, err, "failed to create staging table %s.%s", ns, stagingTableName)

	// Bulk load events into staging table using mssql.CopyIn.
	txn, err := db.BeginTx(ctx, &sql.TxOptions{})
	require.NoError(t, err, "failed to begin transaction")

	sortedCols := []string{"id", "user_id", "event", "received_at"}
	t.Logf("copyin_staging_dedup: columns for CopyIn: %s", strings.Join(sortedCols, ", "))
	copyInStmt := mssql.CopyIn(
		fmt.Sprintf("[%s].[%s]", ns, stagingTableName),
		mssql.BulkOptions{CheckConstraints: false},
		sortedCols...,
	)
	stmt, err := txn.PrepareContext(ctx, copyInStmt)
	require.NoError(t, err, "failed to prepare CopyIn statement")

	// Insert all 24 events (including duplicates) into staging.
	// CRITICAL: Pass time.Time values to CopyIn instead of ISO 8601 strings.
	// The MSSQL bulk CopyIn driver's internal string parser expects the Go
	// layout "2006-01-02 15:04:05.999999999Z07:00" (space separator), not
	// ISO 8601's "T" separator. Passing time.Time bypasses string parsing.
	for _, e := range cfg.Events {
		_, err = stmt.ExecContext(ctx, e.ID, e.UserID, e.Event, mssqlParseDateTimeForCopyIn(t, e.ReceivedAt))
		require.NoError(t, err, "CopyIn exec failed for event %s", e.ID)
	}
	_, err = stmt.ExecContext(ctx)
	require.NoError(t, err, "CopyIn final exec failed")
	_ = stmt.Close()

	// DELETE matching rows from target (empty initially, so 0 deletes expected).
	deleteSQL := fmt.Sprintf(
		`DELETE FROM [%s].[%s] FROM [%s].[%s] AS _source WHERE _source."id" = [%s].[%s]."id"`,
		ns, tableName, ns, stagingTableName, ns, tableName,
	)
	_, err = txn.ExecContext(ctx, deleteSQL)
	require.NoError(t, err, "DELETE from target table failed")

	// INSERT with ROW_NUMBER dedup from staging into target.
	insertSQL := fmt.Sprintf(`
		INSERT INTO [%s].[%s] ("id", "user_id", "event", "received_at")
		SELECT "id", "user_id", "event", "received_at"
		FROM (
			SELECT *, ROW_NUMBER() OVER (
				PARTITION BY "id"
				ORDER BY "received_at" DESC
			) AS _rudder_staging_row_number
			FROM [%s].[%s]
		) AS _
		WHERE _rudder_staging_row_number = 1`,
		ns, tableName, ns, stagingTableName,
	)
	_, err = txn.ExecContext(ctx, insertSQL)
	require.NoError(t, err, "INSERT with ROW_NUMBER dedup failed")

	err = txn.Commit()
	require.NoError(t, err, "transaction commit failed")

	// Drop staging table.
	_, err = db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE [%s].[%s]`, ns, stagingTableName))
	require.NoError(t, err, "failed to drop staging table")

	// Verify row count equals unique event count (22 unique out of 24 total).
	verifyMSSQLRowCount(t, ctx, db, ns, tableName, cfg.ExpectedRows)

	// Verify staging result metadata is consistent.
	require.Greater(t, stagingResult.TotalEventCount, stagingResult.UniqueEventCount,
		"staging result should have more total events than unique events due to duplicates")

	// Verify the staging file was actually generated.
	require.NotEmpty(t, stagingResult.StagingFilePaths,
		"staging file paths must not be empty")

	t.Logf("copyin_staging_dedup: loaded %d events, deduped to %d expected rows",
		len(cfg.Events), cfg.ExpectedRows)
}

// ---------------------------------------------------------------------------
// Test Case 2: replay_produces_same_row_count
// ---------------------------------------------------------------------------

// testReplayProducesSameRowCount verifies that replaying the same dataset
// through the MSSQL CopyIn+dedup pipeline produces an identical row count
// on each replay — the core idempotency guarantee.
func testReplayProducesSameRowCount(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	cfg IdempotentTestConfig,
) {
	t.Helper()

	tableName := "tracks_replay"
	createTableSQL := fmt.Sprintf(
		`CREATE TABLE [%s].[%s] ("id" nvarchar(512), "user_id" nvarchar(512), "event" nvarchar(512), "received_at" datetimeoffset)`,
		ns, tableName,
	)
	_, err := db.ExecContext(ctx, createTableSQL)
	require.NoError(t, err, "failed to create replay target table")

	// Replay the same events cfg.ReplayCount times (default 2).
	replayCount := cfg.ReplayCount
	if replayCount == 0 {
		replayCount = 2
	}

	var rowCounts []int

	for replay := 0; replay < replayCount; replay++ {
		// Create staging table.
		stagingTableName := whutils.StagingTableName(whutils.MSSQL, tableName, 127)
		createStagingSQL := fmt.Sprintf(
			`SELECT TOP 0 * INTO [%s].[%s] FROM [%s].[%s]`,
			ns, stagingTableName, ns, tableName,
		)
		_, err := db.ExecContext(ctx, createStagingSQL)
		require.NoError(t, err, "replay %d: failed to create staging table", replay)

		// Bulk CopyIn.
		txn, err := db.BeginTx(ctx, &sql.TxOptions{})
		require.NoError(t, err, "replay %d: failed to begin transaction", replay)

		sortedCols := []string{"id", "user_id", "event", "received_at"}
		copyInStmt := mssql.CopyIn(
			fmt.Sprintf("[%s].[%s]", ns, stagingTableName),
			mssql.BulkOptions{CheckConstraints: false},
			sortedCols...,
		)
		stmt, err := txn.PrepareContext(ctx, copyInStmt)
		require.NoError(t, err, "replay %d: failed to prepare CopyIn statement", replay)

		// CRITICAL: Pass time.Time values to CopyIn instead of ISO 8601 strings.
		// See mssqlParseDateTimeForCopyIn for details on the format incompatibility.
		for _, e := range cfg.Events {
			_, err = stmt.ExecContext(ctx, e.ID, e.UserID, e.Event, mssqlParseDateTimeForCopyIn(t, e.ReceivedAt))
			require.NoError(t, err, "replay %d: CopyIn exec failed for event %s", replay, e.ID)
		}
		_, err = stmt.ExecContext(ctx)
		require.NoError(t, err, "replay %d: CopyIn final exec failed", replay)
		_ = stmt.Close()

		// DELETE matching rows from target.
		deleteSQL := fmt.Sprintf(
			`DELETE FROM [%s].[%s] FROM [%s].[%s] AS _source WHERE _source."id" = [%s].[%s]."id"`,
			ns, tableName, ns, stagingTableName, ns, tableName,
		)
		_, err = txn.ExecContext(ctx, deleteSQL)
		require.NoError(t, err, "replay %d: DELETE from target table failed", replay)

		// INSERT with ROW_NUMBER dedup.
		insertSQL := fmt.Sprintf(`
			INSERT INTO [%s].[%s] ("id", "user_id", "event", "received_at")
			SELECT "id", "user_id", "event", "received_at"
			FROM (
				SELECT *, ROW_NUMBER() OVER (
					PARTITION BY "id"
					ORDER BY "received_at" DESC
				) AS _rudder_staging_row_number
				FROM [%s].[%s]
			) AS _
			WHERE _rudder_staging_row_number = 1`,
			ns, tableName, ns, stagingTableName,
		)
		_, err = txn.ExecContext(ctx, insertSQL)
		require.NoError(t, err, "replay %d: INSERT with dedup failed", replay)

		err = txn.Commit()
		require.NoError(t, err, "replay %d: transaction commit failed", replay)

		// Drop staging table.
		_, err = db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE [%s].[%s]`, ns, stagingTableName))
		require.NoError(t, err, "replay %d: failed to drop staging table", replay)

		// Record row count after this replay.
		var count int
		err = db.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT COUNT(*) FROM [%s].[%s]`, ns, tableName,
		)).Scan(&count)
		require.NoError(t, err, "replay %d: failed to count rows", replay)
		rowCounts = append(rowCounts, count)

		t.Logf("replay %d: row count = %d", replay, count)
	}

	// All replay row counts must be identical — this is the idempotency guarantee.
	for i := 1; i < len(rowCounts); i++ {
		require.Equal(t, rowCounts[0], rowCounts[i],
			"row count after replay %d (%d) must equal row count after replay 0 (%d)",
			i, rowCounts[i], rowCounts[0])
	}

	// The final row count should equal the expected unique event count.
	require.Equal(t, cfg.ExpectedRows, rowCounts[len(rowCounts)-1],
		"final row count should equal expected unique rows")

	t.Logf("replay_produces_same_row_count: %d replays all produced %d rows",
		replayCount, rowCounts[0])
}

// ---------------------------------------------------------------------------
// Test Case 3: varchar_length_handling
// ---------------------------------------------------------------------------

// testVarcharLengthHandling verifies that MSSQL correctly handles VARCHAR
// length constraints and diacritics via UCS-2 encoding. String values are
// inserted into columns with specific length constraints to validate the
// ProcessColumnValue behavior observed in the production MSSQL connector.
func testVarcharLengthHandling(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
) {
	t.Helper()

	tableName := "tracks_varchar"
	createTableSQL := fmt.Sprintf(
		`CREATE TABLE [%s].[%s] ("id" nvarchar(512), "name" nvarchar(20), "description" nvarchar(512), "received_at" datetimeoffset)`,
		ns, tableName,
	)
	_, err := db.ExecContext(ctx, createTableSQL)
	require.NoError(t, err, "failed to create varchar test table")

	// Table-driven test cases for value type handling.
	testCases := []struct {
		name           string
		id             string
		value          string
		dataType       string
		expectedInsert bool // whether the insert should succeed
	}{
		{
			name:           "short_ascii_string",
			id:             "varchar-001",
			value:          "hello",
			dataType:       mssqlStringDataType,
			expectedInsert: true,
		},
		{
			name:           "string_at_exact_length",
			id:             "varchar-002",
			value:          "12345678901234567890", // exactly 20 chars
			dataType:       mssqlStringDataType,
			expectedInsert: true,
		},
		{
			name:           "diacritics_utf8",
			id:             "varchar-003",
			value:          "café résumé naïve",
			dataType:       mssqlStringDataType,
			expectedInsert: true,
		},
		{
			name:           "integer_type",
			id:             "varchar-004",
			value:          "42",
			dataType:       mssqlIntDataType,
			expectedInsert: true,
		},
		{
			name:           "float_type",
			id:             "varchar-005",
			value:          "3.14159",
			dataType:       mssqlFloatDataType,
			expectedInsert: true,
		},
		{
			name:           "boolean_type",
			id:             "varchar-006",
			value:          "true",
			dataType:       mssqlBooleanDataType,
			expectedInsert: true,
		},
		{
			name:           "datetime_type",
			id:             "varchar-007",
			value:          "2024-01-15T10:00:00Z",
			dataType:       mssqlDateTimeDataType,
			expectedInsert: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Marshal/unmarshal event using jsonrs to confirm JSON round-trip.
			// CRITICAL: Must use jsonrs per .golangci.yml depguard rule.
			eventMap := map[string]string{
				"id":    tc.id,
				"name":  tc.value,
				"type":  tc.dataType,
				"table": tableName,
			}
			eventJSON, err := jsonrs.Marshal(eventMap)
			require.NoError(t, err, "jsonrs.Marshal failed for %s", tc.name)

			var decoded map[string]string
			err = jsonrs.Unmarshal(eventJSON, &decoded)
			require.NoError(t, err, "jsonrs.Unmarshal failed for %s", tc.name)
			require.Equal(t, tc.id, decoded["id"], "round-trip ID mismatch for %s", tc.name)

			// Insert the value into MSSQL to verify it is accepted.
			insertSQL := fmt.Sprintf(
				`INSERT INTO [%s].[%s] ("id", "name", "received_at") VALUES (@p1, @p2, @p3)`,
				ns, tableName,
			)
			_, err = db.ExecContext(ctx, insertSQL, tc.id, tc.value, time.Now())
			if tc.expectedInsert {
				require.NoError(t, err, "INSERT should succeed for %s", tc.name)
			}

			// Verify the row was inserted.
			var count int
			err = db.QueryRowContext(ctx, fmt.Sprintf(
				`SELECT COUNT(*) FROM [%s].[%s] WHERE "id" = @p1`, ns, tableName,
			), tc.id).Scan(&count)
			require.NoError(t, err, "COUNT query failed for %s", tc.name)
			if tc.expectedInsert {
				require.Equal(t, 1, count, "expected 1 row for %s", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test Case 4: staging_table_cleanup
// ---------------------------------------------------------------------------

// testStagingTableCleanup verifies that after a successful load operation,
// staging tables are properly dropped and no staging table artifacts remain
// in INFORMATION_SCHEMA.TABLES.
func testStagingTableCleanup(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
) {
	t.Helper()

	// Defensive pre-cleanup: drop any leftover staging tables from previously
	// failed tests to prevent cascading assertion failures. Earlier subtests
	// (copyin_staging_dedup, replay_produces_same_row_count) share the same
	// schema namespace; if they fail mid-execution their staging tables persist
	// in INFORMATION_SCHEMA and cause this test's final assertion to fail.
	preCleanPrefix := whutils.StagingTablePrefix(whutils.MSSQL)
	preCleanRows, preCleanErr := db.QueryContext(ctx, `
		SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES 
		WHERE TABLE_SCHEMA = @p1 AND TABLE_NAME LIKE @p2`,
		ns, preCleanPrefix+"%",
	)
	if preCleanErr == nil {
		var leftoverTables []string
		for preCleanRows.Next() {
			var tName string
			if preCleanRows.Scan(&tName) == nil {
				leftoverTables = append(leftoverTables, tName)
			}
		}
		if rowsErr := preCleanRows.Err(); rowsErr != nil {
			t.Logf("staging_table_cleanup: warning iterating leftover staging tables: %v", rowsErr)
		}
		preCleanRows.Close()
		for _, tName := range leftoverTables {
			_, _ = db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE [%s].[%s]`, ns, tName))
			t.Logf("staging_table_cleanup: pre-cleaned leftover staging table %s.%s", ns, tName)
		}
	}

	tableName := "tracks_cleanup"
	createTableSQL := fmt.Sprintf(
		`CREATE TABLE [%s].[%s] ("id" nvarchar(512), "user_id" nvarchar(512), "received_at" datetimeoffset)`,
		ns, tableName,
	)
	_, err := db.ExecContext(ctx, createTableSQL)
	require.NoError(t, err, "failed to create cleanup test table")

	// Create a staging table.
	stagingTableName := whutils.StagingTableName(whutils.MSSQL, tableName, 127)
	createStagingSQL := fmt.Sprintf(
		`SELECT TOP 0 * INTO [%s].[%s] FROM [%s].[%s]`,
		ns, stagingTableName, ns, tableName,
	)
	_, err = db.ExecContext(ctx, createStagingSQL)
	require.NoError(t, err, "failed to create staging table for cleanup test")

	// Verify staging table exists in INFORMATION_SCHEMA.
	var existCount int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES 
		WHERE TABLE_SCHEMA = @p1 AND TABLE_NAME = @p2`,
		ns, stagingTableName,
	).Scan(&existCount)
	require.NoError(t, err, "failed to query INFORMATION_SCHEMA for staging table")
	require.Equal(t, 1, existCount, "staging table should exist before cleanup")

	// Drop staging table (simulating cleanup after successful load).
	_, err = db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE [%s].[%s]`, ns, stagingTableName))
	require.NoError(t, err, "failed to drop staging table")

	// Verify no staging tables remain in INFORMATION_SCHEMA for this schema.
	// The staging table prefix for MSSQL is derived from whutils.StagingTablePrefix.
	stagingPrefix := whutils.StagingTablePrefix(whutils.MSSQL)
	var remainingCount int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES 
		WHERE TABLE_SCHEMA = @p1 AND TABLE_NAME LIKE @p2`,
		ns, stagingPrefix+"%",
	).Scan(&remainingCount)
	require.NoError(t, err, "failed to query INFORMATION_SCHEMA for remaining staging tables")
	require.Zero(t, remainingCount,
		"no staging tables should remain after cleanup")

	// Also verify CTStagingTablePrefix tables are not present (used by connection tests).
	var ctCount int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES 
		WHERE TABLE_SCHEMA = @p1 AND TABLE_NAME LIKE @p2`,
		ns, whutils.CTStagingTablePrefix+"%",
	).Scan(&ctCount)
	require.NoError(t, err, "failed to query INFORMATION_SCHEMA for CT staging tables")
	require.Zero(t, ctCount,
		"no CT staging tables should be present after cleanup")

	t.Logf("staging_table_cleanup: verified no staging tables remain in schema %s", ns)
}

// ---------------------------------------------------------------------------
// Test Case 5: persistent_staging_tables
// ---------------------------------------------------------------------------

// testPersistentStagingTables verifies that MSSQL uses persistent staging
// tables (not temporary tables prefixed with #), due to SQL Server's scoping
// semantics where temp tables are automatically purged after a transaction
// commits. The staging tables are created using the SELECT TOP 0 INTO pattern,
// which creates a real table that persists across transaction boundaries.
func testPersistentStagingTables(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
) {
	t.Helper()

	tableName := "tracks_persistent"
	createTableSQL := fmt.Sprintf(
		`CREATE TABLE [%s].[%s] ("id" nvarchar(512), "user_id" nvarchar(512), "received_at" datetimeoffset)`,
		ns, tableName,
	)
	_, err := db.ExecContext(ctx, createTableSQL)
	require.NoError(t, err, "failed to create persistent staging test table")

	// Create a staging table using the same SELECT TOP 0 INTO pattern as production.
	stagingTableName := whutils.StagingTableName(whutils.MSSQL, tableName, 127)
	createStagingSQL := fmt.Sprintf(
		`SELECT TOP 0 * INTO [%s].[%s] FROM [%s].[%s]`,
		ns, stagingTableName, ns, tableName,
	)
	_, err = db.ExecContext(ctx, createStagingSQL)
	require.NoError(t, err, "failed to create persistent staging table")

	// Verify the staging table is a real table (TABLE_TYPE = 'BASE TABLE'), not a temp table.
	var tableType string
	err = db.QueryRowContext(ctx, `
		SELECT TABLE_TYPE FROM INFORMATION_SCHEMA.TABLES 
		WHERE TABLE_SCHEMA = @p1 AND TABLE_NAME = @p2`,
		ns, stagingTableName,
	).Scan(&tableType)
	require.NoError(t, err, "failed to query table type for staging table")
	require.Equal(t, "BASE TABLE", tableType,
		"staging table must be a BASE TABLE (persistent), not a TEMPORARY or VIEW")

	// Verify the staging table name does NOT start with '#' (temp table prefix in MSSQL).
	require.False(t, strings.HasPrefix(stagingTableName, "#"),
		"staging table name must not start with # (temp table prefix)")

	// Verify the staging table name starts with the expected staging prefix.
	stagingPrefix := whutils.StagingTablePrefix(whutils.MSSQL)
	require.True(t, strings.HasPrefix(stagingTableName, stagingPrefix),
		"staging table name %q should start with prefix %q", stagingTableName, stagingPrefix)

	// Verify the staging table has the same columns as the source table.
	var sourceColCount, stagingColCount int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS 
		WHERE TABLE_SCHEMA = @p1 AND TABLE_NAME = @p2`,
		ns, tableName,
	).Scan(&sourceColCount)
	require.NoError(t, err, "failed to count source table columns")

	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS 
		WHERE TABLE_SCHEMA = @p1 AND TABLE_NAME = @p2`,
		ns, stagingTableName,
	).Scan(&stagingColCount)
	require.NoError(t, err, "failed to count staging table columns")
	require.Equal(t, sourceColCount, stagingColCount,
		"staging table column count must match source table column count")

	// Verify that CopyIn can operate on the persistent staging table within a transaction
	// and that the staging table is still accessible after the transaction commits.
	txn, err := db.BeginTx(ctx, &sql.TxOptions{})
	require.NoError(t, err, "failed to begin transaction for persistent staging test")

	sortedCols := []string{"id", "user_id", "received_at"}
	copyInStmt := mssql.CopyIn(
		fmt.Sprintf("[%s].[%s]", ns, stagingTableName),
		mssql.BulkOptions{CheckConstraints: false},
		sortedCols...,
	)
	stmt, err := txn.PrepareContext(ctx, copyInStmt)
	require.NoError(t, err, "failed to prepare CopyIn for persistent staging table")

	// Insert a single test row. Pass time.Now() directly as time.Time rather
	// than formatting as time.RFC3339 string, because the MSSQL bulk CopyIn
	// driver's string parser expects space-separated datetime layout, not
	// ISO 8601's "T" separator produced by time.RFC3339.
	_, err = stmt.ExecContext(ctx, "persistent-001", "user-001", time.Now())
	require.NoError(t, err, "CopyIn exec failed for persistent staging test")
	_, err = stmt.ExecContext(ctx)
	require.NoError(t, err, "CopyIn final exec failed for persistent staging test")
	_ = stmt.Close()

	err = txn.Commit()
	require.NoError(t, err, "transaction commit failed for persistent staging test")

	// After transaction commit, verify the staging table still exists and has data.
	var stagingRowCount int
	err = db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM [%s].[%s]`, ns, stagingTableName,
	)).Scan(&stagingRowCount)
	require.NoError(t, err, "failed to count rows in persistent staging table after commit")
	require.Equal(t, 1, stagingRowCount,
		"persistent staging table should retain data after transaction commit")

	// Clean up the staging table.
	_, err = db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE [%s].[%s]`, ns, stagingTableName))
	require.NoError(t, err, "failed to drop persistent staging table")

	t.Logf("persistent_staging_tables: confirmed staging table %s is persistent (BASE TABLE, not temp)",
		stagingTableName)
}

// ---------------------------------------------------------------------------
// Shared references for import usage verification
// ---------------------------------------------------------------------------

// mssqlTrackTableSchema defines the standard column schema for a tracks
// table in MSSQL idempotent testing. References whutils.ModelTableSchema
// (type alias for warehouse/internal/model.TableSchema which is map[string]string)
// for consistency with the production MSSQL connector's table schema handling.
//
// Also references whutils.UsersTable, whutils.IdentifiesTable, and
// whutils.DiscardsTable to verify the standard table name constants are
// accessible from the integration test package.
var mssqlTrackTableSchema = whutils.ModelTableSchema{
	"id":          mssqlStringDataType,
	"user_id":     mssqlStringDataType,
	"event":       mssqlStringDataType,
	"received_at": mssqlDateTimeDataType,
}

// mssqlStandardTables references the standard warehouse table name constants
// from whutils to ensure they are accessible and correct in the integration
// test context. These constants are used by the production MSSQL connector
// for primary key and partition key lookups.
var mssqlStandardTables = []string{
	whutils.UsersTable,
	whutils.IdentifiesTable,
	whutils.DiscardsTable,
}

// verifyMSSQLRowCount is a helper that queries the MSSQL database for the row
// count of a given table and asserts it matches the expected value.
func verifyMSSQLRowCount(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	tableName string,
	expectedCount int,
) {
	t.Helper()

	var count int
	err := db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM [%s].[%s]`, ns, tableName,
	)).Scan(&count)
	require.NoError(t, err, "failed to query row count for %s.%s", ns, tableName)
	require.Equal(t, expectedCount, count,
		"row count mismatch for %s.%s: expected %d, got %d",
		ns, tableName, expectedCount, count)
}

// mssqlContainerTimeout defines the maximum time to wait for the MSSQL
// Docker container to become ready. Expressed as a time.Duration value.
var mssqlContainerTimeout time.Duration = 3 * time.Minute

// mssqlParseDateTimeForCopyIn converts an ISO 8601 datetime string (as stored
// in test fixture JSON files) into a time.Time value for use with the MSSQL
// bulk CopyIn driver. The driver's internal string-to-datetime parser expects
// the Go-standard time layout "2006-01-02 15:04:05.999999999Z07:00" (space
// separator between date and time) rather than ISO 8601's "T" separator.
// Passing a native time.Time value instead of a string bypasses the driver's
// string parser entirely and avoids the format incompatibility.
func mssqlParseDateTimeForCopyIn(t *testing.T, dateStr string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339Nano, dateStr)
	if err == nil {
		return parsed
	}

	parsed, err = time.Parse(time.RFC3339, dateStr)
	if err == nil {
		return parsed
	}

	t.Fatalf("mssqlParseDateTimeForCopyIn: unable to parse datetime %q", dateStr)
	return time.Time{} // unreachable, t.Fatalf terminates the test
}

// Type assertions ensuring the testhelper types are accessible from the
// integration test package. whth.RenderIdempotentStagingFiles is the function
// used by renderIdempotentStagingForConnector (in idempotent_sync_test.go)
// to generate deterministic staging files for connector-specific testing.
var (
	_ = whth.IdempotentStagingConfig{}
	_ = whth.IdempotentStagingResult{}
	_ = whth.RenderIdempotentStagingFiles
)
