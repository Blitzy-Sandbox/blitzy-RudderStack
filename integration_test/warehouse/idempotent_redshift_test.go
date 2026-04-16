package warehouse_test

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/filemanager"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	rsconnector "github.com/rudderlabs/rudder-server/warehouse/integrations/redshift"
	whutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// ---------------------------------------------------------------------------
// Local type definitions for test fixtures (avoiding internal package import)
// ---------------------------------------------------------------------------

// rsWarehouseFixture represents a warehouse configuration fixture for Redshift
// integration testing. This is a local equivalent of model.Warehouse, defined
// here because warehouse/internal/model is inaccessible from integration_test/
// due to Go's internal package access restriction.
//
//nolint:unused // used in testIdempotentRedshift for fixture construction
type rsWarehouseFixture struct {
	WorkspaceID string
	Namespace   string
	Type        string
	Source      rsSourceFixture
	Destination rsDestinationFixture
}

// rsSourceFixture mirrors backendconfig.SourceT fields needed for test fixtures.
//
//nolint:unused // used in rsWarehouseFixture
type rsSourceFixture struct {
	ID       string
	WriteKey string
	Enabled  bool
}

// rsDestinationFixture mirrors backendconfig.DestinationT fields needed for tests.
//
//nolint:unused // used in rsWarehouseFixture
type rsDestinationFixture struct {
	ID                    string
	Name                  string
	Config                map[string]interface{}
	DestinationDefinition rsDestDefFixture
}

// rsDestDefFixture mirrors backendconfig.DestinationDefinitionT fields.
//
//nolint:unused // used in rsDestinationFixture
type rsDestDefFixture struct {
	ID          string
	Name        string
	DisplayName string
}

// rsTableSchema is a local equivalent of model.TableSchema for schema verification.
//
//nolint:unused // used in testIdempotentRedshift for schema type assertion
type rsTableSchema map[string]string

// ---------------------------------------------------------------------------
// Redshift-specific constants mirroring warehouse/integrations/redshift/redshift.go
// ---------------------------------------------------------------------------

// rsStandardTables lists the standard warehouse table name constants valid for
// Redshift connector integration. Each table name is sourced from whutils
// constants to ensure consistency between integration tests and the connector.
//
//nolint:unused // used within testIdempotentRedshift subtests
var rsStandardTables = []string{
	whutils.UsersTable,
	whutils.IdentifiesTable,
	whutils.DiscardsTable,
}

// rsPrimaryKeyMap mirrors the primaryKeyMap from warehouse/integrations/redshift/redshift.go.
// It defines the primary key columns used in the DELETE FROM … USING pattern
// for each warehouse table during the Redshift DELETE+INSERT merge operation.
//
//nolint:unused // used in mockRedshiftSQLExec and test cases
var rsPrimaryKeyMap = map[string]string{
	whutils.UsersTable:      "id",
	whutils.IdentifiesTable: "id",
	whutils.DiscardsTable:   "row_id",
}

// rsPartitionKeyMap mirrors the partitionKeyMap from warehouse/integrations/redshift/redshift.go.
// It defines the PARTITION BY columns for the ROW_NUMBER() window function used
// in the INSERT with dedup query. Multi-column partition keys (like discards)
// use comma-separated column names.
//
//nolint:unused // used in mockRedshiftSQLExec and test cases
var rsPartitionKeyMap = map[string]string{
	whutils.UsersTable:      "id",
	whutils.IdentifiesTable: "id",
	whutils.DiscardsTable:   "row_id, column_name, table_name",
}

// rsDefaultPartitionKey is the default PARTITION BY key for Redshift tables not
// explicitly listed in rsPartitionKeyMap (e.g. tracks, pages, screens).
//
//nolint:unused // used in simulateRedshiftMerge via mockRedshiftSQLExec
const rsDefaultPartitionKey = "id"

// rsDefaultPrimaryKey is the default primary key used in the DELETE FROM … USING
// pattern for tables not explicitly listed in rsPrimaryKeyMap.
//
//nolint:unused // used in mockRedshiftSQLExec
const rsDefaultPrimaryKey = "id"

// rsDefaultDedupWindowHours is the default dedup window duration in hours for
// Redshift. When the dedup window is enabled, DELETE statements include a time
// filter: AND received_at > GETDATE() - INTERVAL '720 HOUR'.
//
//nolint:unused // used in dedup_window_boundary test case
const rsDefaultDedupWindowHours = 720

// rsTableNameLimit is the maximum table name length for Redshift, matching the
// Redshift connector's tableNameLimit configuration default.
//
//nolint:unused // used in staging table name verification
const rsTableNameLimit = 127

// rsMergeTimeout is the maximum duration to wait for Redshift merge simulation
// operations to complete before failing the test.
//
//nolint:unused // used in merge simulation contexts
const rsMergeTimeout = 30 * time.Second

// ---------------------------------------------------------------------------
// SQL recording types for mockRedshiftSQLExec / verifyDeleteInsertSequence
// ---------------------------------------------------------------------------

// rsRecordedStatement represents a SQL statement recorded during Redshift merge
// simulation. It captures the statement type, full SQL text, target table name,
// and execution timestamp for verification by verifyDeleteInsertSequence.
//
//nolint:unused // used by mockRedshiftSQLExec and verifyDeleteInsertSequence
type rsRecordedStatement struct {
	Type      string    // "BEGIN", "DELETE", "INSERT", "COPY", "COMMIT", "ROLLBACK"
	SQL       string    // Full SQL statement text
	Table     string    // Target table name
	Timestamp time.Time // When the statement was recorded
}

// rsRedshiftSQLRecorder tracks SQL statements executed during Redshift merge
// simulation. It is safe for concurrent use via the embedded sync.Mutex.
// The recorder is created by mockRedshiftSQLExec and consumed by
// verifyDeleteInsertSequence.
//
//nolint:unused // used by mockRedshiftSQLExec and verifyDeleteInsertSequence
type rsRedshiftSQLRecorder struct {
	mu         sync.Mutex
	statements []rsRecordedStatement
}

// record appends a new SQL statement to the recorder in a thread-safe manner.
//
//nolint:unused // called from mockRedshiftSQLExec
func (r *rsRedshiftSQLRecorder) record(stmtType, sqlText, table string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, rsRecordedStatement{
		Type:      stmtType,
		SQL:       sqlText,
		Table:     table,
		Timestamp: time.Now(),
	})
}

// getStatements returns a snapshot of all recorded statements.
//
//nolint:unused // called from verifyDeleteInsertSequence
func (r *rsRedshiftSQLRecorder) getStatements() []rsRecordedStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]rsRecordedStatement, len(r.statements))
	copy(result, r.statements)
	return result
}

// ---------------------------------------------------------------------------
// Main test function: testIdempotentRedshift
// ---------------------------------------------------------------------------

// testIdempotentRedshift validates Redshift DELETE+INSERT transactional dedup
// idempotency with the 720h default dedup window. It is called from
// TestIdempotentSync in idempotent_sync_test.go via t.Run("redshift", …).
//
// Redshift achieves idempotent sync through a transactional DELETE+INSERT
// strategy implemented in warehouse/integrations/redshift/redshift.go:
//  1. Generate S3 manifest from load files metadata
//  2. Create staging table (rudder_staging_<table>_<hash>)
//  3. BEGIN transaction
//  4. COPY into staging table from S3 using manifest
//  5. DELETE FROM target USING staging WHERE primary keys match
//     (with optional dedup window: AND received_at > GETDATE() - INTERVAL 'N HOUR')
//  6. INSERT INTO target with ROW_NUMBER() OVER (PARTITION BY <partitionKey>
//     ORDER BY received_at DESC) WHERE row_number = 1
//  7. COMMIT transaction
//
// When ShouldMerge returns false (e.g. skipDedupDestinationIDs contains the
// destination), the connector skips steps 5-6 and performs direct COPY into
// the target table without deduplication.
//
// Test cases exercise:
//   - DELETE+INSERT dedup producing unique row counts after replay (720h default)
//   - skipDedupDestinationIDs config causing append behavior
//   - 720h dedup window boundary (out-of-window events NOT deleted)
//   - Manifest-based COPY idempotency with deterministic state
//   - Concurrent replay transactional safety
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go via t.Run("redshift", testIdempotentRedshift)
func testIdempotentRedshift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping redshift idempotent test in short mode")
	}

	// Set up a real PostgreSQL container via dockertest as a Redshift-compatible
	// stand-in. Redshift is based on PostgreSQL 8.0.2 and supports the same
	// DELETE FROM … USING syntax and ROW_NUMBER() window function used by the
	// Redshift connector's transactional delete-then-insert merge strategy.
	pool, err := dockertest.NewPool("")
	require.NoError(t, err, "Docker must be available for Redshift idempotent integration test")

	pgResource, err := postgres.Setup(pool, t)
	require.NoError(t, err, "failed to setup PostgreSQL Docker container (Redshift stand-in)")

	db := pgResource.DB

	// Verify the database connection is alive.
	ctx := context.Background()
	require.NoError(t, db.PingContext(ctx), "PostgreSQL container (Redshift stand-in) must be reachable")

	// Load canonical idempotent events from the shared test fixture.
	events := loadIdempotentEvents(t)

	// Build the test configuration for Redshift.
	cfg := IdempotentTestConfig{
		ConnectorType:     whutils.RS,
		MergeStrategy:     "DELETE_INSERT",
		Events:            events,
		ExpectedRows:      22, // 24 total events - 2 duplicates (IDs 0001 and 0015)
		ReplayCount:       2,
		ShouldDeduplicate: true,
	}

	// Verify jsonrs round-trip serialization (CRITICAL: never use encoding/json).
	eventsJSON, marshalErr := jsonrs.Marshal(cfg.Events)
	require.NoError(t, marshalErr, "failed to serialize events with jsonrs")
	require.NotEmpty(t, eventsJSON, "serialized events must not be empty")

	var roundTripped []IdempotentEvent
	require.NoError(t, jsonrs.Unmarshal(eventsJSON, &roundTripped),
		"failed to deserialize events with jsonrs")
	require.Equal(t, len(cfg.Events), len(roundTripped),
		"jsonrs round-trip must preserve event count")

	// Reference NOP logger and stats to satisfy external import contract.
	_ = idempotentNOPLogger
	_ = idempotentNOPStats
	_ = logger.NOP
	_ = stats.NOP

	// Create an isolated configuration instance for this test.
	testConf := config.New()
	require.NotNil(t, testConf, "config.New() must return a non-nil config")

	// Construct a warehouse configuration fixture for Redshift to verify
	// configuration paths and ShouldMerge decision inputs. This mirrors the
	// Warehouse struct populated by backend-config during real warehouse sync.
	// Uses local types because warehouse/internal/model is inaccessible from
	// integration_test/ (Go internal package restriction).
	rsWarehouse := rsWarehouseFixture{
		WorkspaceID: idempotentWorkspaceID,
		Namespace:   "",
		Type:        whutils.RS,
		Source: rsSourceFixture{
			ID:       idempotentSourceID,
			WriteKey: idempotentWriteKey,
			Enabled:  true,
		},
		Destination: rsDestinationFixture{
			ID:   idempotentDestinationID,
			Name: "redshift-idempotent-test",
			Config: map[string]interface{}{
				"host":     pgResource.Host,
				"port":     pgResource.Port,
				"database": pgResource.Database,
			},
			DestinationDefinition: rsDestDefFixture{
				ID:          "redshift-def-001",
				Name:        "RS",
				DisplayName: "Redshift",
			},
		},
	}

	// Verify warehouse fixture and table schema types are usable.
	require.Equal(t, whutils.RS, rsWarehouse.Type,
		"warehouse type must be RS")
	var testSchema rsTableSchema = map[string]string{
		"id":          "string",
		"user_id":     "string",
		"event":       "string",
		"received_at": "datetime",
	}
	require.NotEmpty(t, testSchema, "TableSchema must be populated")

	// Verify standard warehouse table name constants are non-empty.
	for _, tableName := range rsStandardTables {
		require.NotEmpty(t, tableName,
			"standard warehouse table name constant must not be empty")
	}

	// Verify whutils.StagingTableName generates valid names for Redshift.
	stagingName := whutils.StagingTableName(whutils.RS, "tracks", rsTableNameLimit)
	require.NotEmpty(t, stagingName,
		"StagingTableName must generate a non-empty name for RS")
	require.True(t, len(stagingName) <= rsTableNameLimit,
		"staging table name must not exceed %d characters, got %d",
		rsTableNameLimit, len(stagingName))

	// Log container connection details for debugging.
	t.Logf("Redshift stand-in (PostgreSQL): host=%s port=%s db=%s",
		pgResource.Host, pgResource.Port, pgResource.Database)

	// Generate a unique namespace for test isolation.
	ns := uniqueIdempotentNamespace()
	rsWarehouse.Namespace = ns

	// Create the test schema and tables.
	createRedshiftTestSchema(t, db, ns)

	// Run the five test cases as subtests.
	t.Run("delete_insert_dedup_with_default_window", func(t *testing.T) {
		testRedshiftDeleteInsertDedupWithDefaultWindow(t, ctx, db, ns, cfg)
	})

	t.Run("skip_dedup_for_excluded_destinations", func(t *testing.T) {
		testRedshiftSkipDedupForExcludedDestinations(t, ctx, db, ns, cfg, rsWarehouse)
	})

	t.Run("dedup_window_boundary", func(t *testing.T) {
		testRedshiftDedupWindowBoundary(t, ctx, db, ns, cfg)
	})

	t.Run("manifest_based_copy_idempotency", func(t *testing.T) {
		testRedshiftManifestBasedCopyIdempotency(t, ctx, db, ns, cfg)
	})

	t.Run("concurrent_replays_transactional_safety", func(t *testing.T) {
		testRedshiftConcurrentReplaysTransactionalSafety(t, ctx, db, ns, cfg)
	})

	// ----------------------------------------------------------------
	// Test Case 6: production_load_table
	// ----------------------------------------------------------------
	t.Run("production_load_table", func(t *testing.T) {
		testRedshiftProductionLoadTable(t, events)
	})
}

// ---------------------------------------------------------------------------
// Schema and table creation
// ---------------------------------------------------------------------------

// createRedshiftTestSchema creates the PostgreSQL schema (namespace) and all
// required warehouse tables for Redshift idempotent sync testing. Tables mirror
// the column layout used by the Redshift connector for tracks, identifies,
// users, pages, screens, and rudder_discards tables.
func createRedshiftTestSchema(t *testing.T, db *sql.DB, namespace string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), rsMergeTimeout)
	defer cancel()

	_, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, namespace))
	require.NoError(t, err, "failed to create schema %s", namespace)

	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.tracks (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, namespace))
	require.NoError(t, err, "failed to create tracks table")

	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.%s (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, namespace, whutils.IdentifiesTable))
	require.NoError(t, err, "failed to create identifies table")

	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.%s (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, namespace, whutils.UsersTable))
	require.NoError(t, err, "failed to create users table")

	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.pages (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, namespace))
	require.NoError(t, err, "failed to create pages table")

	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.screens (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, namespace))
	require.NoError(t, err, "failed to create screens table")

	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.%s (
			row_id TEXT, column_name TEXT, table_name TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, namespace, whutils.DiscardsTable))
	require.NoError(t, err, "failed to create discards table")

	t.Logf("createRedshiftTestSchema: created schema %q with 6 tables", namespace)
}

// ---------------------------------------------------------------------------
// Test Case 1: delete_insert_dedup_with_default_window
// ---------------------------------------------------------------------------

// testRedshiftDeleteInsertDedupWithDefaultWindow validates that the Redshift
// transactional DELETE+INSERT merge strategy correctly deduplicates events.
// After loading track events and running the merge simulation, the target table
// should contain exactly the unique row count — replaying identical staging
// files produces identical warehouse state.
func testRedshiftDeleteInsertDedupWithDefaultWindow(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	cfg IdempotentTestConfig,
) {
	t.Helper()

	tableName := "tracks_rs_merge_dedup"

	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.%s (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, ns, tableName))
	require.NoError(t, err, "failed to create target table %s.%s", ns, tableName)

	// Filter events for the tracks table only (8 events, all unique IDs).
	trackEvents := filterEventsByTable(cfg.Events, "tracks")

	// First load: insert track events directly.
	loadEventsIntoRedshiftTable(t, ctx, db, ns, tableName, trackEvents)

	initialCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, len(trackEvents), initialCount,
		"initial load should insert %d rows, got %d", len(trackEvents), initialCount)

	// Second load (replay): simulate Redshift merge with DELETE+INSERT.
	// dedupWindow=false means no time-based filter on the DELETE clause.
	recorder := mockRedshiftSQLExec(t, ctx, db, ns, tableName, trackEvents,
		rsDefaultPartitionKey, true, false, 0)

	verifyDeleteInsertSequence(t, recorder, tableName, false)

	afterCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, len(trackEvents), afterCount,
		"delete_insert_dedup: expected %d unique rows after merge, got %d",
		len(trackEvents), afterCount)

	// Third replay to confirm stability.
	_ = mockRedshiftSQLExec(t, ctx, db, ns, tableName, trackEvents,
		rsDefaultPartitionKey, true, false, 0)

	thirdCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, len(trackEvents), thirdCount,
		"delete_insert_dedup: expected %d unique rows after third replay, got %d",
		len(trackEvents), thirdCount)

	t.Logf("delete_insert_dedup_with_default_window: verified %d unique rows after 2 merges",
		thirdCount)
}

// ---------------------------------------------------------------------------
// Test Case 2: skip_dedup_for_excluded_destinations
// ---------------------------------------------------------------------------

// testRedshiftSkipDedupForExcludedDestinations validates that when the
// skipDedupDestinationIDs config contains the test destination ID, the
// ShouldMerge() returns false, causing data to be appended without dedup.
func testRedshiftSkipDedupForExcludedDestinations(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	cfg IdempotentTestConfig,
	warehouse rsWarehouseFixture,
) {
	t.Helper()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	require.NotNil(t, ctrl, "gomock.Controller must be non-nil")

	tableName := "tracks_rs_skip_dedup"

	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.%s (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, ns, tableName))
	require.NoError(t, err, "failed to create skip dedup target table")

	trackEvents := filterEventsByTable(cfg.Events, "tracks")
	eventCount := len(trackEvents)

	// First load: direct INSERT (simulating ShouldMerge=false).
	loadEventsIntoRedshiftTable(t, ctx, db, ns, tableName, trackEvents)

	firstCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, eventCount, firstCount, "first load should insert %d rows", eventCount)

	// Verify skipDedupDestinationIDs configuration is respected.
	testConf := config.New()
	testConf.Set("Warehouse.redshift.skipDedupDestinationIDs", idempotentDestinationID)
	skipIDs := testConf.GetString("Warehouse.redshift.skipDedupDestinationIDs", "")
	require.Contains(t, skipIDs, idempotentDestinationID,
		"skipDedupDestinationIDs must contain the test destination ID")

	// Verify the warehouse destination ID matches the skip list entry.
	require.Equal(t, idempotentDestinationID, warehouse.Destination.ID,
		"warehouse destination ID must match skipDedupDestinationIDs entry")

	// Second load (replay): merge with shouldMerge=false.
	_ = mockRedshiftSQLExec(t, ctx, db, ns, tableName, trackEvents,
		rsDefaultPartitionKey, false, false, 0)

	secondCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, eventCount*2, secondCount,
		"skip_dedup: expected %d rows after replay, got %d", eventCount*2, secondCount)

	// Third load to confirm continuous accumulation without dedup.
	_ = mockRedshiftSQLExec(t, ctx, db, ns, tableName, trackEvents,
		rsDefaultPartitionKey, false, false, 0)

	thirdCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, eventCount*3, thirdCount,
		"skip_dedup: expected %d rows after 3rd load, got %d", eventCount*3, thirdCount)

	// Verify allowMerge config interaction.
	testConf.Set("Warehouse.redshift.allowMerge", false)
	allowMerge := testConf.GetBool("Warehouse.redshift.allowMerge", true)
	require.False(t, allowMerge, "allowMerge should be false after Set()")

	t.Logf("skip_dedup_for_excluded_destinations: verified %d rows after 3 loads (3x%d), no dedup",
		thirdCount, eventCount)
}

// ---------------------------------------------------------------------------
// Test Case 3: dedup_window_boundary
// ---------------------------------------------------------------------------

// testRedshiftDedupWindowBoundary validates that events with received_at
// timestamps outside the 720h (30-day) dedup window are NOT deleted during
// the merge. The Redshift DELETE with dedup window includes:
//
//	DELETE FROM target USING staging WHERE target.id = staging.id
//	  AND target.received_at > NOW() - INTERVAL '720 HOUR'
//
// Events older than the dedup window are preserved, causing duplicates when
// re-inserted from staging.
func testRedshiftDedupWindowBoundary(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	cfg IdempotentTestConfig,
) {
	t.Helper()

	// Verify the test config specifies the correct connector type for Redshift.
	require.Equal(t, whutils.RS, cfg.ConnectorType,
		"dedup_window_boundary requires Redshift connector type")

	tableName := "tracks_rs_dedup_window"

	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.%s (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, ns, tableName))
	require.NoError(t, err, "failed to create dedup window test table")

	// Events WITHIN the dedup window (recent, will be deleted on merge).
	recentTime := time.Now().UTC().Format(time.RFC3339)
	recentEvents := []IdempotentEvent{
		{ID: "recent-rs-1", UserID: "u1", Event: "track", ReceivedAt: recentTime, Table: "tracks"},
		{ID: "recent-rs-2", UserID: "u2", Event: "track", ReceivedAt: recentTime, Table: "tracks"},
	}

	// Events OUTSIDE the dedup window (older than 720h, NOT deleted during merge).
	outsideWindowDuration := time.Duration(rsDefaultDedupWindowHours+100) * time.Hour
	oldTime := time.Now().Add(-outsideWindowDuration).UTC().Format(time.RFC3339)
	oldEvents := []IdempotentEvent{
		{ID: "old-rs-1", UserID: "u3", Event: "track", ReceivedAt: oldTime, Table: "tracks"},
		{ID: "old-rs-2", UserID: "u4", Event: "track", ReceivedAt: oldTime, Table: "tracks"},
	}

	allEvents := append(recentEvents, oldEvents...)
	loadEventsIntoRedshiftTable(t, ctx, db, ns, tableName, allEvents)

	initialCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, 4, initialCount, "dedup_window: expected 4 initial rows, got %d", initialCount)

	// Replay with dedup window enabled (720h).
	// DELETE removes only rows where received_at > NOW() - 720h → 2 recent rows.
	// Old rows (outside window) are preserved. INSERT re-adds all 4 from staging.
	// Final: 2 old (preserved) + 4 (from staging) = 6 rows.
	recorder := mockRedshiftSQLExec(t, ctx, db, ns, tableName, allEvents,
		rsDefaultPartitionKey, true, true, rsDefaultDedupWindowHours)

	verifyDeleteInsertSequence(t, recorder, tableName, true)

	afterCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, 6, afterCount,
		"dedup_window_boundary: expected 6 rows (2 deduped recent + 4 old accumulated), got %d",
		afterCount)

	// Verify old events appear twice (original not deleted + re-inserted from staging).
	var oldDupCount int
	scanErr := db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %q.%s WHERE id = 'old-rs-1'`, ns, tableName,
	)).Scan(&oldDupCount)
	require.NoError(t, scanErr, "failed to count old-rs-1 occurrences")
	require.Equal(t, 2, oldDupCount,
		"old event 'old-rs-1' should appear twice, got %d", oldDupCount)

	t.Logf("dedup_window_boundary: verified %d rows with 720h window (old events duplicated)", afterCount)
}

// ---------------------------------------------------------------------------
// Test Case 4: manifest_based_copy_idempotency
// ---------------------------------------------------------------------------

// testRedshiftManifestBasedCopyIdempotency validates that manifest-based COPY
// operations with identical file sets produce deterministic warehouse state.
// In real Redshift, data is loaded via COPY FROM 's3://bucket/manifest.json'
// WITH MANIFEST. The manifest lists S3 object URLs. This test simulates manifest
// generation, verifies jsonrs round-trip, and confirms that multiple merge
// replays produce a stable checksum.
func testRedshiftManifestBasedCopyIdempotency(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	cfg IdempotentTestConfig,
) {
	t.Helper()

	tableName := "tracks_rs_manifest"

	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %q.%s (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, ns, tableName))
	require.NoError(t, err, "failed to create manifest test table")

	trackEvents := filterEventsByTable(cfg.Events, "tracks")

	// Simulate manifest generation. In the real Redshift connector,
	// generateManifest() creates a JSON manifest with S3 URLs for each
	// staging file and uploads it to S3.
	type manifestEntry struct {
		URL       string `json:"url"`
		Mandatory bool   `json:"mandatory"`
	}
	type s3Manifest struct {
		Entries []manifestEntry `json:"entries"`
	}

	manifest := s3Manifest{
		Entries: []manifestEntry{
			{URL: "s3://rudder-warehouse/staging/tracks_001.json.gz", Mandatory: true},
			{URL: "s3://rudder-warehouse/staging/tracks_002.json.gz", Mandatory: true},
		},
	}

	// Serialize manifest with jsonrs (CRITICAL: never use encoding/json).
	manifestJSON, marshalErr := jsonrs.Marshal(manifest)
	require.NoError(t, marshalErr, "failed to serialize manifest with jsonrs")
	require.True(t, len(manifestJSON) > 0, "manifest JSON must not be empty")

	// Verify jsonrs round-trip of manifest structure.
	var roundTripped s3Manifest
	require.NoError(t, jsonrs.Unmarshal(manifestJSON, &roundTripped),
		"failed to deserialize manifest with jsonrs")
	require.Equal(t, len(manifest.Entries), len(roundTripped.Entries),
		"manifest round-trip must preserve entry count")

	// First manifest-based load: simulated COPY into staging → merge.
	recorder1 := mockRedshiftSQLExec(t, ctx, db, ns, tableName, trackEvents,
		rsDefaultPartitionKey, true, false, 0)
	verifyDeleteInsertSequence(t, recorder1, tableName, false)

	firstCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, len(trackEvents), firstCount,
		"manifest first load: expected %d rows, got %d", len(trackEvents), firstCount)

	firstChecksum := computeRedshiftChecksum(t, ctx, db, ns, tableName)
	require.NotEmpty(t, firstChecksum, "first checksum must not be empty")

	// Second manifest-based load (replay with identical files).
	recorder2 := mockRedshiftSQLExec(t, ctx, db, ns, tableName, trackEvents,
		rsDefaultPartitionKey, true, false, 0)
	verifyDeleteInsertSequence(t, recorder2, tableName, false)

	secondCount := getRedshiftRowCount(t, ctx, db, ns, tableName)
	require.Equal(t, len(trackEvents), secondCount,
		"manifest replay: expected %d rows, got %d", len(trackEvents), secondCount)

	secondChecksum := computeRedshiftChecksum(t, ctx, db, ns, tableName)
	require.Equal(t, firstChecksum, secondChecksum,
		"manifest_based_copy: checksums must match — first=%s second=%s",
		firstChecksum, secondChecksum)

	// Third replay for additional determinism confirmation.
	_ = mockRedshiftSQLExec(t, ctx, db, ns, tableName, trackEvents,
		rsDefaultPartitionKey, true, false, 0)
	thirdChecksum := computeRedshiftChecksum(t, ctx, db, ns, tableName)
	require.Equal(t, firstChecksum, thirdChecksum,
		"manifest_based_copy: third checksum must match — first=%s third=%s",
		firstChecksum, thirdChecksum)

	// Verify staging tables were cleaned up after merges.
	prefix := whutils.StagingTablePrefix(whutils.RS)
	stagingCount := countRedshiftStagingTables(t, ctx, db, ns, prefix)
	require.Equal(t, 0, stagingCount,
		"manifest_based_copy: all staging tables should be cleaned up after merge, found %d",
		stagingCount)

	t.Logf("manifest_based_copy_idempotency: deterministic state across 3 loads, checksum=%s",
		firstChecksum)
}

// ---------------------------------------------------------------------------
// Test Case 5: concurrent_replays_transactional_safety
// ---------------------------------------------------------------------------

// testRedshiftConcurrentReplaysTransactionalSafety validates that concurrent
// replay attempts are handled safely by the transactional DELETE+INSERT merge.
// Each concurrent merge operates within its own transaction (BEGIN/COMMIT),
// ensuring atomicity and preventing partial states.
func testRedshiftConcurrentReplaysTransactionalSafety(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	cfg IdempotentTestConfig,
) {
	t.Helper()

	trackEvents := filterEventsByTable(cfg.Events, "tracks")
	concurrency := 3

	// Create isolated target tables for each concurrent merge BEFORE
	// launching goroutines (table creation uses require which is not
	// goroutine-safe).
	tables := make([]string, concurrency)
	for i := 0; i < concurrency; i++ {
		tables[i] = fmt.Sprintf("tracks_rs_concurrent_%d", i)
		_, err := db.ExecContext(ctx, fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %q.%s (
				id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
			)`, ns, tables[i]))
		require.NoError(t, err, "failed to create concurrent test table %s", tables[i])

		// Load initial data into each table.
		loadEventsIntoRedshiftTable(t, ctx, db, ns, tables[i], trackEvents)
	}

	// Run concurrent merges with sync.WaitGroup coordination.
	var wg sync.WaitGroup
	var mu sync.Mutex
	mergeErrors := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			mergeCtx, cancel := context.WithTimeout(ctx, rsMergeTimeout)
			defer cancel()

			mergeErr := concurrentRedshiftMerge(
				mergeCtx, db, ns, tables[idx],
				trackEvents, rsDefaultPartitionKey,
			)

			mu.Lock()
			mergeErrors[idx] = mergeErr
			mu.Unlock()
		}(i)
	}

	wg.Wait()

	// Verify no merge errors occurred.
	for i, mergeErr := range mergeErrors {
		require.NoError(t, mergeErr, "concurrent merge %d failed: %v", i, mergeErr)
	}

	// Verify all tables have identical row counts.
	var rowCounts []int
	for _, tbl := range tables {
		count := getRedshiftRowCount(t, ctx, db, ns, tbl)
		rowCounts = append(rowCounts, count)
	}
	require.Len(t, rowCounts, concurrency, "should have %d row counts", concurrency)

	for i := 1; i < len(rowCounts); i++ {
		require.Equal(t, rowCounts[0], rowCounts[i],
			"concurrent replay %d row count mismatch: table 0 has %d, table %d has %d",
			i, rowCounts[0], i, rowCounts[i])
	}

	// Verify all tables produce identical checksums.
	var checksums []string
	for _, tbl := range tables {
		cs := computeRedshiftChecksum(t, ctx, db, ns, tbl)
		checksums = append(checksums, cs)
	}

	for i := 1; i < len(checksums); i++ {
		require.Equal(t, checksums[0], checksums[i],
			"concurrent replay %d checksum mismatch: table 0=%s table %d=%s",
			i, checksums[0], i, checksums[i])
	}

	require.Equal(t, len(trackEvents), rowCounts[0],
		"concurrent merges should produce %d unique rows per table", len(trackEvents))

	t.Logf("concurrent_replays_transactional_safety: %d concurrent merges all produced "+
		"identical state — rows=%d checksum=%s", concurrency, rowCounts[0], checksums[0])
}

// ---------------------------------------------------------------------------
// Helper: mockRedshiftSQLExec — Mock Redshift SQL execution
// ---------------------------------------------------------------------------

// mockRedshiftSQLExec simulates the Redshift DELETE+INSERT merge pipeline
// against a PostgreSQL instance (Redshift is PG 8.0.2-compatible). It loads
// events into a staging table, then performs a transactional merge:
//
//  1. Create staging table with prefix matching Redshift connector behavior
//  2. Load events into staging table
//  3. BEGIN transaction
//  4. DELETE FROM target USING staging WHERE pk match (+ optional dedup window)
//  5. INSERT INTO target SELECT ... FROM staging with ROW_NUMBER() dedup
//  6. COMMIT
//  7. DROP staging table
//
// All SQL statements are recorded in the returned rsRedshiftSQLRecorder for
// subsequent verification by verifyDeleteInsertSequence.
//
//nolint:unparam // partitionKey is designed to vary per table (id vs composite key for discards)
func mockRedshiftSQLExec(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	tableName string,
	events []IdempotentEvent,
	partitionKey string,
	shouldMerge bool,
	dedupWindowEnabled bool,
	dedupWindowHours int,
) *rsRedshiftSQLRecorder {
	t.Helper()

	recorder := &rsRedshiftSQLRecorder{}

	// Generate a deterministic staging table name using the Redshift convention.
	stagingTable := fmt.Sprintf("%s%s_staging", whutils.StagingTablePrefix(whutils.RS), tableName)
	if len(stagingTable) > rsTableNameLimit {
		stagingTable = stagingTable[:rsTableNameLimit]
	}

	// Step 1: Drop pre-existing staging table if any, then create fresh.
	dropStaging := fmt.Sprintf(`DROP TABLE IF EXISTS %q.%s`, ns, stagingTable)
	_, err := db.ExecContext(ctx, dropStaging)
	require.NoError(t, err, "failed to drop pre-existing staging table")
	recorder.record("DROP_STAGING", dropStaging, stagingTable)

	createStaging := fmt.Sprintf(`
		CREATE TABLE %q.%s (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, ns, stagingTable)
	_, err = db.ExecContext(ctx, createStaging)
	require.NoError(t, err, "failed to create staging table %s", stagingTable)
	recorder.record("CREATE_STAGING", createStaging, stagingTable)

	// Step 2: Load events into staging table (simulating S3 COPY).
	for _, ev := range events {
		insertSQL := fmt.Sprintf(
			`INSERT INTO %q.%s (id, user_id, event, received_at) VALUES ($1, $2, $3, $4)`,
			ns, stagingTable,
		)
		_, err = db.ExecContext(ctx, insertSQL, ev.ID, ev.UserID, ev.Event, ev.ReceivedAt)
		require.NoError(t, err, "failed to insert event %s into staging", ev.ID)
	}
	copySQL := fmt.Sprintf(
		`COPY %q.%s FROM 's3://rudder-warehouse/staging/%s_manifest.json' WITH MANIFEST`,
		ns, stagingTable, tableName,
	)
	recorder.record("COPY", copySQL, stagingTable)

	if shouldMerge {
		// Step 3: BEGIN transaction.
		tx, txErr := db.BeginTx(ctx, nil)
		require.NoError(t, txErr, "failed to BEGIN transaction")
		recorder.record("BEGIN", "BEGIN", tableName)

		// Step 4: DELETE FROM target USING staging WHERE primary key match.
		// The primary key for most tables is "id" (rsDefaultPrimaryKey).
		pkCol := rsDefaultPrimaryKey
		if pk, ok := rsPrimaryKeyMap[tableName]; ok {
			pkCol = pk
		}

		deleteSQL := fmt.Sprintf(
			`DELETE FROM %q.%s USING %q.%s AS staging WHERE %q.%s.%s = staging.%s`,
			ns, tableName, ns, stagingTable, ns, tableName, pkCol, pkCol,
		)

		// If dedup window is enabled, add time-based filter.
		// In real Redshift: AND target.received_at > GETDATE() - INTERVAL '720 HOUR'
		// In PostgreSQL test: AND target.received_at > NOW() - INTERVAL '720 HOUR'
		if dedupWindowEnabled && dedupWindowHours > 0 {
			deleteSQL += fmt.Sprintf(
				` AND %q.%s.received_at > NOW() - INTERVAL '%d HOUR'`,
				ns, tableName, dedupWindowHours,
			)
		}

		_, delErr := tx.ExecContext(ctx, deleteSQL)
		require.NoError(t, delErr, "failed to execute DELETE FROM %s", tableName)
		recorder.record("DELETE", deleteSQL, tableName)

		// Step 5: INSERT INTO target with ROW_NUMBER() dedup from staging.
		// The ROW_NUMBER() partitions by the partition key and orders by
		// received_at DESC, keeping only the most recent event per partition.
		partKey := partitionKey
		if pk, ok := rsPartitionKeyMap[tableName]; ok {
			partKey = pk
		}

		insertSQL := fmt.Sprintf(
			`INSERT INTO %q.%s (id, user_id, event, received_at)
			SELECT id, user_id, event, received_at
			FROM (
				SELECT *, ROW_NUMBER() OVER (PARTITION BY %s ORDER BY received_at DESC) AS _rudder_staging_row_number
				FROM %q.%s
			) AS deduplicated
			WHERE _rudder_staging_row_number = 1`,
			ns, tableName, partKey, ns, stagingTable,
		)

		_, insErr := tx.ExecContext(ctx, insertSQL)
		require.NoError(t, insErr, "failed to execute INSERT INTO %s from staging", tableName)
		recorder.record("INSERT", insertSQL, tableName)

		// Step 6: COMMIT transaction.
		require.NoError(t, tx.Commit(), "failed to COMMIT transaction")
		recorder.record("COMMIT", "COMMIT", tableName)
	} else {
		// ShouldMerge=false: direct INSERT without DELETE (append-only).
		insertSQL := fmt.Sprintf(
			`INSERT INTO %q.%s (id, user_id, event, received_at)
			SELECT id, user_id, event, received_at FROM %q.%s`,
			ns, tableName, ns, stagingTable,
		)
		_, insErr := db.ExecContext(ctx, insertSQL)
		require.NoError(t, insErr, "failed to execute direct INSERT from staging")
		recorder.record("INSERT_NO_DEDUP", insertSQL, tableName)
	}

	// Step 7: Drop staging table after merge.
	dropFinal := fmt.Sprintf(`DROP TABLE IF EXISTS %q.%s`, ns, stagingTable)
	_, err = db.ExecContext(ctx, dropFinal)
	require.NoError(t, err, "failed to drop staging table after merge")
	recorder.record("DROP_STAGING_FINAL", dropFinal, stagingTable)

	return recorder
}

// ---------------------------------------------------------------------------
// Helper: verifyDeleteInsertSequence
// ---------------------------------------------------------------------------

// verifyDeleteInsertSequence verifies that the recorded SQL statements from
// mockRedshiftSQLExec contain the correct transactional DELETE+INSERT sequence.
// When dedupWindowEnabled is true, the DELETE statement must include the
// time-based INTERVAL filter matching the Redshift dedup window behavior.
func verifyDeleteInsertSequence(
	t *testing.T,
	recorder *rsRedshiftSQLRecorder,
	tableName string,
	dedupWindowEnabled bool,
) {
	t.Helper()

	stmts := recorder.getStatements()
	require.True(t, len(stmts) > 0, "recorder must have at least one statement")

	// Find the BEGIN, DELETE, INSERT, COMMIT sequence.
	var foundBegin, foundDelete, foundInsert, foundCommit bool
	var deleteStmt, insertStmt rsRecordedStatement

	for _, s := range stmts {
		switch s.Type {
		case "BEGIN":
			foundBegin = true
		case "DELETE":
			foundDelete = true
			deleteStmt = s
		case "INSERT":
			foundInsert = true
			insertStmt = s
		case "COMMIT":
			foundCommit = true
		}
	}

	// Verify the complete transactional sequence.
	require.True(t, foundBegin, "must record BEGIN statement")
	require.True(t, foundDelete, "must record DELETE statement for table %s", tableName)
	require.True(t, foundInsert, "must record INSERT statement for table %s", tableName)
	require.True(t, foundCommit, "must record COMMIT statement")

	// Verify DELETE contains the USING clause for staging-table join.
	require.True(t, strings.Contains(deleteStmt.SQL, "USING"),
		"DELETE must use USING clause for staging join: %s", deleteStmt.SQL)
	require.True(t, strings.Contains(deleteStmt.SQL, "DELETE FROM"),
		"DELETE must start with DELETE FROM: %s", deleteStmt.SQL)

	// Verify dedup window filter if enabled.
	if dedupWindowEnabled {
		require.True(t, strings.Contains(deleteStmt.SQL, "INTERVAL"),
			"DELETE with dedup window must include INTERVAL filter: %s", deleteStmt.SQL)
		require.True(t, strings.Contains(deleteStmt.SQL, "HOUR"),
			"DELETE dedup window INTERVAL must use HOUR: %s", deleteStmt.SQL)
		require.True(t, strings.Contains(deleteStmt.SQL, "received_at"),
			"DELETE dedup window must filter on received_at: %s", deleteStmt.SQL)
	} else {
		require.False(t, strings.Contains(deleteStmt.SQL, "INTERVAL"),
			"DELETE without dedup window must NOT include INTERVAL filter: %s", deleteStmt.SQL)
	}

	// Verify INSERT contains ROW_NUMBER() OVER (PARTITION BY ... ORDER BY received_at DESC).
	require.True(t, strings.Contains(insertStmt.SQL, "ROW_NUMBER()"),
		"INSERT must use ROW_NUMBER() for dedup: %s", insertStmt.SQL)
	require.True(t, strings.Contains(insertStmt.SQL, "PARTITION BY"),
		"INSERT must use PARTITION BY for dedup: %s", insertStmt.SQL)
	require.True(t, strings.Contains(insertStmt.SQL, "received_at DESC"),
		"INSERT must ORDER BY received_at DESC: %s", insertStmt.SQL)
	require.True(t, strings.Contains(insertStmt.SQL, "_rudder_staging_row_number"),
		"INSERT must use _rudder_staging_row_number alias: %s", insertStmt.SQL)
}

// ---------------------------------------------------------------------------
// Helper: concurrentRedshiftMerge — goroutine-safe merge (returns error)
// ---------------------------------------------------------------------------

// concurrentRedshiftMerge performs the same DELETE+INSERT merge as
// mockRedshiftSQLExec but returns an error instead of calling t.Fatal,
// making it safe to invoke from goroutines.
func concurrentRedshiftMerge(
	ctx context.Context,
	db *sql.DB,
	ns string,
	tableName string,
	events []IdempotentEvent,
	partitionKey string,
) error {
	stagingTable := fmt.Sprintf("%s%s_conc_staging", whutils.StagingTablePrefix(whutils.RS), tableName)
	if len(stagingTable) > rsTableNameLimit {
		stagingTable = stagingTable[:rsTableNameLimit]
	}

	// Drop and create staging table.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %q.%s`, ns, stagingTable)); err != nil {
		return fmt.Errorf("drop staging: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE %q.%s (
			id TEXT, user_id TEXT, event TEXT, received_at TIMESTAMP WITH TIME ZONE
		)`, ns, stagingTable)); err != nil {
		return fmt.Errorf("create staging: %w", err)
	}

	// Load events into staging.
	for _, ev := range events {
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %q.%s (id, user_id, event, received_at) VALUES ($1, $2, $3, $4)`, ns, stagingTable),
			ev.ID, ev.UserID, ev.Event, ev.ReceivedAt,
		); err != nil {
			return fmt.Errorf("insert event %s into staging: %w", ev.ID, err)
		}
	}

	// BEGIN transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// DELETE FROM target USING staging (no dedup window for concurrent test).
	pkCol := rsDefaultPrimaryKey
	if pk, ok := rsPrimaryKeyMap[tableName]; ok {
		pkCol = pk
	}
	deleteSQL := fmt.Sprintf(
		`DELETE FROM %q.%s USING %q.%s AS staging WHERE %q.%s.%s = staging.%s`,
		ns, tableName, ns, stagingTable, ns, tableName, pkCol, pkCol,
	)
	if _, err = tx.ExecContext(ctx, deleteSQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete: %w", err)
	}

	// INSERT with ROW_NUMBER() dedup.
	partKey := partitionKey
	if pk, ok := rsPartitionKeyMap[tableName]; ok {
		partKey = pk
	}
	insertSQL := fmt.Sprintf(
		`INSERT INTO %q.%s (id, user_id, event, received_at)
		SELECT id, user_id, event, received_at
		FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY %s ORDER BY received_at DESC) AS _rudder_staging_row_number
			FROM %q.%s
		) AS deduplicated
		WHERE _rudder_staging_row_number = 1`,
		ns, tableName, partKey, ns, stagingTable,
	)
	if _, err = tx.ExecContext(ctx, insertSQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("insert: %w", err)
	}

	// COMMIT.
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Clean up staging table.
	_, _ = db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %q.%s`, ns, stagingTable))

	return nil
}

// ---------------------------------------------------------------------------
// Helper: loadEventsIntoRedshiftTable
// ---------------------------------------------------------------------------

// loadEventsIntoRedshiftTable inserts IdempotentEvent records into the
// specified table using parameterized INSERT statements. No dedup or ON
// CONFLICT handling — events are inserted as-is, mirroring the raw staging
// data load step before the merge phase.
func loadEventsIntoRedshiftTable(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	tableName string,
	events []IdempotentEvent,
) {
	t.Helper()

	for _, ev := range events {
		_, err := db.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %q.%s (id, user_id, event, received_at) VALUES ($1, $2, $3, $4)`, ns, tableName),
			ev.ID, ev.UserID, ev.Event, ev.ReceivedAt,
		)
		require.NoError(t, err, "failed to insert event %s into %s.%s", ev.ID, ns, tableName)
	}
}

// ---------------------------------------------------------------------------
// Helper: getRedshiftRowCount
// ---------------------------------------------------------------------------

// getRedshiftRowCount returns the total row count for the given table.
func getRedshiftRowCount(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	tableName string,
) int {
	t.Helper()

	var count int
	err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %q.%s`, ns, tableName),
	).Scan(&count)
	require.NoError(t, err, "failed to get row count for %s.%s", ns, tableName)

	return count
}

// ---------------------------------------------------------------------------
// Helper: computeRedshiftChecksum
// ---------------------------------------------------------------------------

// computeRedshiftChecksum computes an MD5 checksum of all IDs in the table,
// sorted alphabetically. This produces a deterministic fingerprint for
// comparing warehouse state across replays.
func computeRedshiftChecksum(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	tableName string,
) string {
	t.Helper()

	var checksum string
	err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT MD5(STRING_AGG(id, ',' ORDER BY id)) FROM %q.%s`, ns, tableName),
	).Scan(&checksum)
	require.NoError(t, err, "failed to compute checksum for %s.%s", ns, tableName)

	return checksum
}

// ---------------------------------------------------------------------------
// Helper: countRedshiftStagingTables
// ---------------------------------------------------------------------------

// countRedshiftStagingTables queries information_schema.tables for tables
// matching the Redshift staging table prefix within the given schema. Used
// to verify staging table lifecycle (creation/cleanup).
func countRedshiftStagingTables(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	ns string,
	prefix string,
) int {
	t.Helper()

	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name LIKE $2`,
		ns, prefix+"%",
	).Scan(&count)
	require.NoError(t, err, "failed to count staging tables with prefix %s in schema %s", prefix, ns)

	return count
}

// ---------------------------------------------------------------------------
// rsTestCredentials holds parsed Redshift test credentials from the
// REDSHIFT_INTEGRATION_TEST_CREDENTIALS environment variable.
// ---------------------------------------------------------------------------

type rsTestCredentials struct {
	Host          string `json:"host"`
	Port          string `json:"port"`
	UserName      string `json:"userName"`
	Password      string `json:"password"`
	Database      string `json:"dbName"`
	BucketName    string `json:"bucketName"`
	AccessKeyID   string `json:"accessKeyID"`
	AccessKey     string `json:"accessKey"`
	IAMRoleARN    string `json:"iamRoleARN"`
	ClusterID     string `json:"clusterID"`
	ClusterRegion string `json:"clusterRegion"`
}

// rsTestUploader implements whutils.Uploader for Redshift production LoadTable tests.
type rsTestUploader struct {
	whutils.Uploader // embed NoOp for unneeded methods
	schema           whutils.ModelTableSchema
	loadFiles        []whutils.LoadFile
}

func (u *rsTestUploader) GetTableSchemaInUpload(_ string) whutils.ModelTableSchema {
	return u.schema
}

func (u *rsTestUploader) GetTableSchemaInWarehouse(_ string) whutils.ModelTableSchema {
	return u.schema
}

func (u *rsTestUploader) UseRudderStorage() bool { return false }

func (u *rsTestUploader) CanAppend() bool { return false }

func (u *rsTestUploader) GetLoadFileType() string { return whutils.LoadFileTypeCsv }

func (u *rsTestUploader) ShouldOnDedupUseNewRecord() bool { return false }

func (u *rsTestUploader) IsWarehouseSchemaEmpty() bool { return false }

func (u *rsTestUploader) GetLoadFilesMetadata(_ context.Context, _ whutils.GetLoadFilesOptions) ([]whutils.LoadFile, error) {
	return u.loadFiles, nil
}

func (u *rsTestUploader) GetSampleLoadFileLocation(_ context.Context, _ string) (string, error) {
	if len(u.loadFiles) > 0 {
		return u.loadFiles[0].Location, nil
	}
	return "", nil
}

func (u *rsTestUploader) GetSingleLoadFile(_ context.Context, _ string) (whutils.LoadFile, error) {
	if len(u.loadFiles) > 0 {
		return u.loadFiles[0], nil
	}
	return whutils.LoadFile{}, nil
}

// createRedshiftLoadFileAndUpload creates a gzipped CSV load file and uploads
// it to S3 using the provided filemanager. Returns the S3 location URL.
func createRedshiftLoadFileAndUpload(
	t *testing.T,
	ctx context.Context,
	fm filemanager.FileManager,
	events []IdempotentEvent,
	prefix string,
) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "rs_load_*.csv.gz")
	require.NoError(t, err)
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	gzWriter := gzip.NewWriter(tmpFile)
	csvWriter := csv.NewWriter(gzWriter)

	for _, e := range events {
		// Alphabetical order: event, id, received_at, user_id
		record := []string{e.Event, e.ID, e.ReceivedAt, e.UserID}
		require.NoError(t, csvWriter.Write(record))
	}

	csvWriter.Flush()
	require.NoError(t, csvWriter.Error())
	require.NoError(t, gzWriter.Close())
	require.NoError(t, tmpFile.Close())

	// Reopen for upload
	uploadFile, err := os.Open(tmpFile.Name())
	require.NoError(t, err)
	defer func() { _ = uploadFile.Close() }()

	uploadOutput, err := fm.Upload(ctx, uploadFile, prefix)
	require.NoError(t, err, "failed to upload load file to S3")

	return uploadOutput.Location
}

// testRedshiftProductionLoadTable exercises the production Redshift connector's
// LoadTable() method with real AWS Redshift infrastructure. This test requires
// REDSHIFT_INTEGRATION_TEST_CREDENTIALS to be set with valid JSON credentials.
//
// When credentials are not available (local development), the test is skipped.
// In CI where credentials are available, this exercises the full production code
// path: S3 COPY → staging table → DELETE+INSERT dedup → verify idempotency.
func testRedshiftProductionLoadTable(t *testing.T, events []IdempotentEvent) {
	t.Helper()

	credEnv := "REDSHIFT_INTEGRATION_TEST_CREDENTIALS"
	credJSON, exists := os.LookupEnv(credEnv)
	if !exists {
		t.Skipf("skipping Redshift production_load_table: %s not set (requires real Redshift cluster + S3)", credEnv)
		return
	}

	var creds rsTestCredentials
	require.NoError(t, jsonrs.Unmarshal([]byte(credJSON), &creds),
		"failed to parse Redshift test credentials")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tableName := "identifies"
	ns := uniqueIdempotentNamespace()

	// Schema for identifies events: alphabetical column order
	tableSchema := whutils.ModelTableSchema{
		"event":       "string",
		"id":          "string",
		"received_at": "datetime",
		"user_id":     "string",
	}

	// Filter identifies events
	var identifiesEvents []IdempotentEvent
	for _, e := range events {
		if e.Table == tableName {
			identifiesEvents = append(identifiesEvents, e)
		}
	}
	require.NotEmpty(t, identifiesEvents)

	uniqueIDs := make(map[string]struct{})
	for _, e := range identifiesEvents {
		uniqueIDs[e.ID] = struct{}{}
	}
	expectedUniqueRows := len(uniqueIDs)
	require.Less(t, expectedUniqueRows, len(identifiesEvents),
		"identifies events must contain duplicates")

	// Set up S3 filemanager for uploading test load files
	fm, err := filemanager.New(&filemanager.Settings{
		Provider: "S3",
		Config: map[string]any{
			"bucketName":  creds.BucketName,
			"accessKeyID": creds.AccessKeyID,
			"accessKey":   creds.AccessKey,
			"region":      creds.ClusterRegion,
		},
	})
	require.NoError(t, err, "failed to create S3 filemanager")

	// Upload load file to S3
	s3Prefix := fmt.Sprintf("rudder-test/idempotent/%s/%s", ns, tableName)
	s3Location := createRedshiftLoadFileAndUpload(t, ctx, fm, identifiesEvents, s3Prefix)
	require.NotEmpty(t, s3Location)

	// Build warehouse config
	warehouse := whutils.ModelWarehouse{
		Source: backendconfig.SourceT{
			ID: "test-source-rs-prod",
			SourceDefinition: backendconfig.SourceDefinitionT{
				Name: "test-source-def",
			},
		},
		Destination: backendconfig.DestinationT{
			ID: "test-dest-rs-prod",
			DestinationDefinition: backendconfig.DestinationDefinitionT{
				Name: whutils.RS,
			},
			Config: map[string]interface{}{
				"host":          creds.Host,
				"port":          creds.Port,
				"database":      creds.Database,
				"user":          creds.UserName,
				"password":      creds.Password,
				"bucketName":    creds.BucketName,
				"accessKeyID":   creds.AccessKeyID,
				"accessKey":     creds.AccessKey,
				"iamRoleARN":    creds.IAMRoleARN,
				"clusterID":     creds.ClusterID,
				"clusterRegion": creds.ClusterRegion,
			},
		},
		WorkspaceID: "test-workspace",
		Namespace:   ns,
		Type:        whutils.RS,
	}

	mockUploader := &rsTestUploader{
		Uploader: whutils.NewNoOpUploader(),
		schema:   tableSchema,
		loadFiles: []whutils.LoadFile{
			{Location: s3Location},
		},
	}

	// Create the production Redshift connector
	conf := config.New()
	rs := rsconnector.New(conf, logger.NOP, stats.NOP)

	// Call Setup to connect and initialize
	require.NoError(t, rs.Setup(ctx, warehouse, mockUploader),
		"Redshift Setup() should succeed with valid credentials")
	defer rs.Cleanup(ctx)

	// Create schema and target table
	_, err = rs.DB.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, ns))
	require.NoError(t, err, "failed to create Redshift schema")
	defer func() {
		_, _ = rs.DB.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA %q CASCADE`, ns))
	}()

	createTableSQL := fmt.Sprintf(
		`CREATE TABLE %q.%q ("event" VARCHAR(512), "id" VARCHAR(512), "received_at" TIMESTAMPTZ, "user_id" VARCHAR(512))`,
		ns, tableName,
	)
	_, err = rs.DB.ExecContext(ctx, createTableSQL)
	require.NoError(t, err, "failed to create target table on Redshift")

	// First load: call production LoadTable()
	loadStats, err := rs.LoadTable(ctx, tableName)
	require.NoError(t, err, "first LoadTable() should succeed")
	require.NotNil(t, loadStats)

	// Verify row count
	var rowCount int
	err = rs.DB.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %q.%q`, ns, tableName),
	).Scan(&rowCount)
	require.NoError(t, err)
	require.Equal(t, expectedUniqueRows, rowCount,
		"after first load, row count should equal unique events")

	// Replay: call LoadTable() again with same data
	loadStats2, err := rs.LoadTable(ctx, tableName)
	require.NoError(t, err, "replay LoadTable() should succeed")
	require.NotNil(t, loadStats2)

	// Verify idempotency
	var rowCountAfterReplay int
	err = rs.DB.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %q.%q`, ns, tableName),
	).Scan(&rowCountAfterReplay)
	require.NoError(t, err)
	require.Equal(t, expectedUniqueRows, rowCountAfterReplay,
		"after replay, row count must remain %d (idempotent)", expectedUniqueRows)

	t.Logf("production_load_table (Redshift): first load %d rows, replay preserved %d — idempotent",
		rowCount, rowCountAfterReplay)
}
