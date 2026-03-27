package warehouse_test

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/ClickHouse/clickhouse-go"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/utils/misc"
	chconnector "github.com/rudderlabs/rudder-server/warehouse/integrations/clickhouse"
	sqlmw "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
	whutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// clickhouseTestEvent represents a canonical test event for ClickHouse idempotent sync
// verification. Fields are tagged with JSON for serialization via jsonrs (NEVER encoding/json).
//
//nolint:unused // used inside testIdempotentClickHouse, which is called from TestIdempotentSync in idempotent_sync_test.go
type clickhouseTestEvent struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Event      string    `json:"event"`
	ReceivedAt time.Time `json:"received_at"`
}

// testIdempotentClickHouse validates ClickHouse engine-level dedup idempotency
// using ReplacingMergeTree and AggregatingMergeTree engines. It is called from
// TestIdempotentSync in idempotent_sync_test.go via t.Run("clickhouse", ...).
//
// ClickHouse achieves idempotent sync through engine-level deduplication:
//   - ReplacingMergeTree: collapses duplicate rows by ORDER BY key during merge
//   - AggregatingMergeTree: aggregates rows by ORDER BY key using SimpleAggregateFunction
//   - Plain MergeTree (staging): no dedup — staging tables use append-only semantics
//
// All assertions require OPTIMIZE TABLE ... FINAL to force part merging, because
// ClickHouse performs dedup lazily during background merges. OPTIMIZE TABLE FINAL
// triggers a synchronous, deterministic merge of all parts in a single-node setup.
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go via t.Run("clickhouse", testIdempotentClickHouse)
func testIdempotentClickHouse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping clickhouse idempotent test in short mode")
	}

	// Verify external test dependencies are available for consistency with other
	// idempotent test files in the warehouse_test package.
	_ = config.New()
	_ = logger.NOP
	_ = stats.NOP

	db, _ := setupClickHouseContainer(t) // cleanup registered via t.Cleanup in setupClickHouseContainer

	ctx := context.Background()
	namespace := "ch_idempotent_test"

	// Create the test database namespace (ClickHouse uses databases, not schemas).
	_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", namespace))
	require.NoError(t, err, "failed to create test database")

	// Define canonical test events with deterministic values for reproducible assertions.
	events := []clickhouseTestEvent{
		{ID: "evt-001", UserID: "user-1", Event: "page_view", ReceivedAt: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)},
		{ID: "evt-002", UserID: "user-2", Event: "button_click", ReceivedAt: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)},
		{ID: "evt-003", UserID: "user-1", Event: "form_submit", ReceivedAt: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)},
		{ID: "evt-004", UserID: "user-3", Event: "page_view", ReceivedAt: time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)},
		{ID: "evt-005", UserID: "user-2", Event: "checkout", ReceivedAt: time.Date(2024, 1, 15, 14, 0, 0, 0, time.UTC)},
	}

	// Verify jsonrs round-trip serialization of test events (CRITICAL: never use encoding/json).
	eventsJSON, err := jsonrs.Marshal(events)
	require.NoError(t, err, "failed to serialize test events with jsonrs")
	require.NotEmpty(t, eventsJSON, "serialized events must not be empty")

	var deserializedEvents []clickhouseTestEvent
	err = jsonrs.Unmarshal(eventsJSON, &deserializedEvents)
	require.NoError(t, err, "failed to deserialize test events with jsonrs")
	require.Equal(t, len(events), len(deserializedEvents), "deserialized event count must match")

	// insertClickHouseEvents inserts a batch of events into the specified table via a
	// database transaction. Each call creates a separate ClickHouse "part", which is
	// critical for testing merge behavior across multiple inserts.
	insertClickHouseEvents := func(t *testing.T, tableName string, evts []clickhouseTestEvent) {
		t.Helper()
		tx, txErr := db.Begin()
		require.NoError(t, txErr, "failed to begin transaction")

		stmt, stmtErr := tx.Prepare(fmt.Sprintf(
			"INSERT INTO %s.%s (id, user_id, event, received_at) VALUES (?, ?, ?, ?)",
			namespace, tableName,
		))
		require.NoError(t, stmtErr, "failed to prepare insert statement")

		for _, evt := range evts {
			_, execErr := stmt.Exec(evt.ID, evt.UserID, evt.Event, evt.ReceivedAt)
			require.NoError(t, execErr, fmt.Sprintf("failed to insert event %s", evt.ID))
		}

		require.NoError(t, tx.Commit(), "failed to commit transaction")
	}

	// getRowCount returns the number of rows in the specified table.
	getRowCount := func(t *testing.T, tableName string) int {
		t.Helper()
		var count int
		scanErr := db.QueryRowContext(ctx, fmt.Sprintf(
			"SELECT COUNT(*) FROM %s.%s", namespace, tableName,
		)).Scan(&count)
		require.NoError(t, scanErr, fmt.Sprintf("failed to count rows in %s.%s", namespace, tableName))
		return count
	}

	// optimizeTable forces ClickHouse to synchronously merge all parts for the specified
	// table. This is REQUIRED for deterministic dedup assertions because ClickHouse performs
	// ReplacingMergeTree/AggregatingMergeTree dedup lazily during background merges.
	optimizeTable := func(t *testing.T, tableName string) {
		t.Helper()
		_, optErr := db.ExecContext(ctx, fmt.Sprintf("OPTIMIZE TABLE %s.%s FINAL", namespace, tableName))
		require.NoError(t, optErr, fmt.Sprintf("failed to optimize table %s.%s", namespace, tableName))
	}

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 1: ReplacingMergeTree dedup — duplicate rows collapsed to unique set.
	//
	// ReplacingMergeTree removes duplicate rows that share the same ORDER BY key
	// (received_at, id) during background merges. After OPTIMIZE TABLE FINAL,
	// all parts are merged and duplicates are collapsed.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("replacing_merge_tree_dedup", func(t *testing.T) {
		tableName := "tracks_rmt_dedup"
		_, createErr := db.ExecContext(ctx, fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s.%s (
				id String,
				user_id String,
				event String,
				received_at DateTime
			) ENGINE = ReplacingMergeTree()
			ORDER BY (received_at, id)
			PARTITION BY toDate(received_at)
		`, namespace, tableName))
		require.NoError(t, createErr, "failed to create ReplacingMergeTree table")

		// First load: insert 5 unique events (creates Part 1).
		insertClickHouseEvents(t, tableName, events)

		// Second load (replay): insert same 5 events again (creates Part 2).
		insertClickHouseEvents(t, tableName, events)

		// Before OPTIMIZE, both parts exist and row count may be 10.
		countBeforeOptimize := getRowCount(t, tableName)
		require.Greater(t, countBeforeOptimize, 0, "should have rows before optimize")

		// OPTIMIZE TABLE FINAL forces merge, collapsing duplicates by ORDER BY key.
		optimizeTable(t, tableName)

		// After OPTIMIZE FINAL, ReplacingMergeTree deduplicates: 10 rows → 5 unique rows.
		countAfterOptimize := getRowCount(t, tableName)
		require.Equal(t, len(events), countAfterOptimize,
			"ReplacingMergeTree should collapse duplicate rows to %d unique events after OPTIMIZE FINAL, got %d",
			len(events), countAfterOptimize)
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 2: AggregatingMergeTree users dedup — aggregated state per user.
	//
	// The warehouse connector uses AggregatingMergeTree for the users table with
	// SimpleAggregateFunction(anyLast, Nullable(TYPE)) columns. This aggregates
	// duplicate user records by ORDER BY key (id), keeping the last-seen value
	// for each column via the anyLast aggregate function.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("aggregating_merge_tree_users_dedup", func(t *testing.T) {
		tableName := "users_amt_dedup"

		// Create AggregatingMergeTree table matching the warehouse connector's
		// createUsersTable() pattern (clickhouse.go lines 847-866).
		_, createErr := db.ExecContext(ctx, fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s.%s (
				id String,
				name SimpleAggregateFunction(anyLast, Nullable(String)),
				received_at DateTime
			) ENGINE = AggregatingMergeTree()
			ORDER BY (id)
			PARTITION BY toDate(received_at)
		`, namespace, tableName))
		require.NoError(t, createErr, "failed to create AggregatingMergeTree table")

		// Define user records with deterministic values.
		type userRecord struct {
			ID         string
			Name       string
			ReceivedAt time.Time
		}
		users := []userRecord{
			{ID: "user-1", Name: "Alice", ReceivedAt: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)},
			{ID: "user-2", Name: "Bob", ReceivedAt: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)},
			{ID: "user-3", Name: "Charlie", ReceivedAt: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)},
		}

		// insertUsers inserts all user records within a single transaction (one part).
		insertUsers := func(t *testing.T) {
			t.Helper()
			tx, txErr := db.Begin()
			require.NoError(t, txErr, "failed to begin user insert transaction")

			stmt, stmtErr := tx.Prepare(fmt.Sprintf(
				"INSERT INTO %s.%s (id, name, received_at) VALUES (?, ?, ?)",
				namespace, tableName,
			))
			require.NoError(t, stmtErr, "failed to prepare user insert statement")

			for _, u := range users {
				_, execErr := stmt.Exec(u.ID, u.Name, u.ReceivedAt)
				require.NoError(t, execErr, fmt.Sprintf("failed to insert user %s", u.ID))
			}

			require.NoError(t, tx.Commit(), "failed to commit user insert transaction")
		}

		// First load (Part 1).
		insertUsers(t)
		// Second load — replay with same data (Part 2).
		insertUsers(t)

		// OPTIMIZE TABLE FINAL merges parts, aggregating by ORDER BY key (id).
		optimizeTable(t, tableName)

		// After OPTIMIZE, each unique user ID should have exactly one row.
		var count int
		scanErr := db.QueryRowContext(ctx, fmt.Sprintf(
			"SELECT COUNT(*) FROM %s.%s", namespace, tableName,
		)).Scan(&count)
		require.NoError(t, scanErr, "failed to count users after optimize")
		require.Equal(t, len(users), count,
			"AggregatingMergeTree should aggregate duplicate user records to %d unique users, got %d",
			len(users), count)

		// Verify each user has the correct name via anyLast aggregation.
		for _, u := range users {
			var name string
			scanErr := db.QueryRowContext(ctx, fmt.Sprintf(
				"SELECT name FROM %s.%s WHERE id = '%s'",
				namespace, tableName, u.ID,
			)).Scan(&name)
			require.NoError(t, scanErr, fmt.Sprintf("failed to query user %s", u.ID))
			require.Equal(t, u.Name, name,
				"user %s should have name '%s' after anyLast aggregation", u.ID, u.Name)
		}
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 3: Replay produces identical final state — checksum convergence.
	//
	// This test verifies that inserting the same events, optimizing, and capturing
	// a checksum produces identical results before and after a replay. This proves
	// that ClickHouse engine-level dedup ensures convergent state regardless of
	// how many times the same data is loaded.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("insert_replay_produces_identical_final_state", func(t *testing.T) {
		tableName := "tracks_replay_checksum"
		_, createErr := db.ExecContext(ctx, fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s.%s (
				id String,
				user_id String,
				event String,
				received_at DateTime
			) ENGINE = ReplacingMergeTree()
			ORDER BY (received_at, id)
			PARTITION BY toDate(received_at)
		`, namespace, tableName))
		require.NoError(t, createErr, "failed to create table for replay checksum test")

		// First load and optimize.
		insertClickHouseEvents(t, tableName, events)
		optimizeTable(t, tableName)

		// Capture first checksum: toString(groupArray(id)) produces a deterministic string
		// representation of all IDs in sorted order. toString() is needed because
		// groupArray() returns an Array(String) which cannot be scanned into *string directly.
		var checksum1 string
		scanErr := db.QueryRowContext(ctx, fmt.Sprintf(
			"SELECT toString(groupArray(id)) FROM (SELECT id FROM %s.%s ORDER BY id)",
			namespace, tableName,
		)).Scan(&checksum1)
		require.NoError(t, scanErr, "failed to get first checksum")
		require.NotEmpty(t, checksum1, "first checksum must not be empty")

		firstRowCount := getRowCount(t, tableName)
		require.Equal(t, len(events), firstRowCount, "first load should have %d rows", len(events))

		// Replay: insert same events again (creates new part with duplicates).
		insertClickHouseEvents(t, tableName, events)
		optimizeTable(t, tableName)

		// Capture second checksum after replay and merge.
		var checksum2 string
		scanErr = db.QueryRowContext(ctx, fmt.Sprintf(
			"SELECT toString(groupArray(id)) FROM (SELECT id FROM %s.%s ORDER BY id)",
			namespace, tableName,
		)).Scan(&checksum2)
		require.NoError(t, scanErr, "failed to get second checksum")
		require.NotEmpty(t, checksum2, "second checksum must not be empty")

		// Both checksums must be identical — engine-level dedup ensures convergence.
		require.Equal(t, checksum1, checksum2,
			"ReplacingMergeTree should produce identical final state after replay — checksums must match")

		// Row count must remain at unique event count after replay.
		secondRowCount := getRowCount(t, tableName)
		require.Equal(t, len(events), secondRowCount,
			"row count should remain %d after replay, got %d", len(events), secondRowCount)
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 4: Staging table non-dedup — plain MergeTree does NOT dedup.
	//
	// Staging tables in the warehouse pipeline use plain MergeTree (not
	// ReplacingMergeTree or AggregatingMergeTree). This means they are
	// append-only and do NOT deduplicate rows, even after OPTIMIZE TABLE FINAL.
	// This test confirms the behavioral difference between staging (MergeTree)
	// and final (ReplacingMergeTree) table engines.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("production_load_table", func(t *testing.T) {
		testClickHouseProductionLoadTable(t, ctx, db, namespace)
	})

	t.Run("staging_table_non_dedup", func(t *testing.T) {
		tableName := "rudder_staging_tracks_test"

		// Create staging table with plain MergeTree (no dedup engine).
		_, createErr := db.ExecContext(ctx, fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s.%s (
				id String,
				user_id String,
				event String,
				received_at DateTime
			) ENGINE = MergeTree()
			ORDER BY (id)
			PARTITION BY toDate(received_at)
		`, namespace, tableName))
		require.NoError(t, createErr, "failed to create MergeTree staging table")

		// First load.
		insertClickHouseEvents(t, tableName, events)

		// Second load (same events — should NOT be deduplicated with plain MergeTree).
		insertClickHouseEvents(t, tableName, events)

		// OPTIMIZE TABLE FINAL with plain MergeTree merges parts but does NOT dedup.
		optimizeTable(t, tableName)

		// Row count should be doubled — MergeTree is append-only with no dedup.
		count := getRowCount(t, tableName)
		require.Equal(t, len(events)*2, count,
			"plain MergeTree staging table should NOT dedup — expected %d rows (2x%d events), got %d",
			len(events)*2, len(events), count)

		// Verify the engine type is MergeTree (not ReplacingMergeTree) via system tables.
		var engine string
		scanErr := db.QueryRowContext(ctx, fmt.Sprintf(
			"SELECT engine FROM system.tables WHERE database = '%s' AND name = '%s'",
			namespace, tableName,
		)).Scan(&engine)
		require.NoError(t, scanErr, "failed to query engine type from system.tables")
		require.Equal(t, "MergeTree", engine,
			"staging table should use plain MergeTree engine, got %s", engine)
	})
}

// setupClickHouseContainer spins up a real ClickHouse server container using dockertest/v3
// and returns a connected *sql.DB instance plus a cleanup function. The container uses
// the official clickhouse/clickhouse-server:latest image with the native TCP protocol on
// port 9000 (the port used by the clickhouse-go v1 driver).
//
// The function waits up to 2 minutes for the container to become healthy before failing.
//
//nolint:unused // called from testIdempotentClickHouse which is invoked by TestIdempotentSync in idempotent_sync_test.go
func setupClickHouseContainer(t *testing.T) (*sql.DB, func()) {
	t.Helper()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err, "failed to create dockertest pool — is Docker running?")

	pool.MaxWait = 2 * time.Minute

	// Set a known password for the default user. Newer ClickHouse images (24.x+) generate
	// a random password by default, so we must set it explicitly via CLICKHOUSE_PASSWORD.
	const clickhousePassword = "clickhouse_test_pass"
	resource, err := pool.Run("clickhouse/clickhouse-server", "latest", []string{
		fmt.Sprintf("CLICKHOUSE_PASSWORD=%s", clickhousePassword),
	})
	require.NoError(t, err, "failed to start ClickHouse container")

	var db *sql.DB

	// Retry connection until the ClickHouse container is ready to accept TCP connections.
	// The clickhouse-go v1 driver uses the native TCP protocol on port 9000.
	// DSN format: tcp://host:port?username=default&password=...&database=default
	err = pool.Retry(func() error {
		var openErr error
		dsn := fmt.Sprintf("tcp://%s?username=default&password=%s&database=default",
			resource.GetHostPort("9000/tcp"), clickhousePassword)
		db, openErr = sql.Open("clickhouse", dsn)
		if openErr != nil {
			return openErr
		}
		return db.Ping()
	})
	require.NoError(t, err, "ClickHouse container did not become ready within timeout")

	cleanup := func() {
		if purgeErr := pool.Purge(resource); purgeErr != nil {
			t.Logf("warning: failed to purge ClickHouse container: %v", purgeErr)
		}
	}
	t.Cleanup(cleanup)

	return db, cleanup
}

// createClickHouseTestSchema creates a test database and tables matching the warehouse
// schema patterns used by the ClickHouse connector (warehouse/integrations/clickhouse/clickhouse.go):
//
//   - tracks table: ReplacingMergeTree — engine-level dedup by (received_at, id) sort key,
//     partitioned by toDate(received_at). Matches CreateTable() at clickhouse.go line 884.
//
//   - users table: AggregatingMergeTree — SimpleAggregateFunction(anyLast, Nullable(TYPE))
//     columns for non-key fields, ORDER BY (id), partitioned by toDate(received_at).
//     Matches createUsersTable() at clickhouse.go line 847.
//
//   - staging table: plain MergeTree — append-only with ORDER BY (id), no dedup. Used for
//     temporary staging data before merge into final tables.
//
//nolint:unused // reusable schema helper for ClickHouse tests, called from idempotent_sync_test.go suite
func createClickHouseTestSchema(t *testing.T, db *sql.DB, namespace string) {
	t.Helper()
	ctx := context.Background()

	// Create the database namespace (ClickHouse equivalent of a schema).
	_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", namespace))
	require.NoError(t, err, fmt.Sprintf("failed to create test database %s", namespace))

	// Create tracks table with ReplacingMergeTree.
	// ORDER BY (received_at, id) matches the warehouse connector's sortKeyFields for regular tables.
	// PARTITION BY toDate(received_at) matches the partitionField constant.
	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.tracks (
			id String,
			user_id String,
			event String,
			received_at DateTime
		) ENGINE = ReplacingMergeTree()
		ORDER BY (received_at, id)
		PARTITION BY toDate(received_at)
	`, namespace))
	require.NoError(t, err, "failed to create tracks table with ReplacingMergeTree")

	// Create users table with AggregatingMergeTree.
	// Uses SimpleAggregateFunction(anyLast, Nullable(TYPE)) for non-key columns,
	// matching the warehouse connector's getClickHouseColumnTypeForSpecificTable() logic
	// for the UsersTable which wraps types in SimpleAggregateFunction(anyLast, Nullable(...)).
	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.users (
			id String,
			name SimpleAggregateFunction(anyLast, Nullable(String)),
			received_at DateTime
		) ENGINE = AggregatingMergeTree()
		ORDER BY (id)
		PARTITION BY toDate(received_at)
	`, namespace))
	require.NoError(t, err, "failed to create users table with AggregatingMergeTree")

	// Create staging table with plain MergeTree (no dedup).
	// Staging tables are used for temporary data before merge into final tables.
	// ORDER BY (id) matches the staging sortKeyFields from CreateTable().
	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.rudder_staging_tracks (
			id String,
			user_id String,
			event String,
			received_at DateTime
		) ENGINE = MergeTree()
		ORDER BY (id)
	`, namespace))
	require.NoError(t, err, "failed to create staging table with MergeTree")
}

// ---------------------------------------------------------------------------
// Production LoadTable Sub-test — ClickHouse
// ---------------------------------------------------------------------------

// chTestDownloader implements the Downloader interface for ClickHouse production
// LoadTable tests, returning pre-created CSV file paths.
//
//nolint:unused // used by testClickHouseProductionLoadTable
type chTestDownloader struct {
	files map[string][]string
}

//nolint:unused // implements Downloader
func (d *chTestDownloader) Download(_ context.Context, tableName string) ([]string, error) {
	return d.files[tableName], nil
}

// chTestUploader implements whutils.Uploader for ClickHouse production LoadTable tests.
//
//nolint:unused // used by testClickHouseProductionLoadTable
type chTestUploader struct {
	whutils.Uploader
	schemaInUpload    map[string]whutils.ModelTableSchema
	schemaInWarehouse map[string]whutils.ModelTableSchema
}

//nolint:unused
func (u *chTestUploader) GetTableSchemaInUpload(tableName string) whutils.ModelTableSchema {
	return u.schemaInUpload[tableName]
}

//nolint:unused
func (u *chTestUploader) GetTableSchemaInWarehouse(tableName string) whutils.ModelTableSchema {
	return u.schemaInWarehouse[tableName]
}

//nolint:unused
func (u *chTestUploader) UseRudderStorage() bool { return false }

//nolint:unused
func (u *chTestUploader) CanAppend() bool { return false }

//nolint:unused
func (u *chTestUploader) GetLoadFileType() string { return whutils.LoadFileTypeCsv }

//nolint:unused
func (u *chTestUploader) ShouldOnDedupUseNewRecord() bool { return false }

//nolint:unused
func (u *chTestUploader) IsWarehouseSchemaEmpty() bool { return false }

//nolint:unused
func (u *chTestUploader) GetLoadFilesMetadata(_ context.Context, _ whutils.GetLoadFilesOptions) ([]whutils.LoadFile, error) {
	return nil, nil
}

//nolint:unused
func (u *chTestUploader) GetSampleLoadFileLocation(_ context.Context, _ string) (string, error) {
	return "", nil
}

//nolint:unused
func (u *chTestUploader) GetSingleLoadFile(_ context.Context, _ string) (whutils.LoadFile, error) {
	return whutils.LoadFile{}, nil
}

// createChTestLoadFile creates a gzipped CSV file from clickhouseTestEvent entries.
// Column order is alphabetical (event, id, received_at, user_id) to match the
// warehouseutils.SortColumnKeysFromColumnMap ordering.
//
//nolint:unused // used by testClickHouseProductionLoadTable
func createChTestLoadFile(t *testing.T, events []clickhouseTestEvent) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "ch_load_*.csv.gz")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(tmpFile.Name()) })

	gw := gzip.NewWriter(tmpFile)
	for _, evt := range events {
		// Column order: event, id, received_at, user_id (alphabetical)
		// ClickHouse typecastDataFromType expects time.RFC3339 format for datetime columns.
		line := fmt.Sprintf("%s,%s,%s,%s\n",
			evt.Event, evt.ID, evt.ReceivedAt.Format(time.RFC3339), evt.UserID)
		_, writeErr := gw.Write([]byte(line))
		require.NoError(t, writeErr)
	}
	require.NoError(t, gw.Close())
	require.NoError(t, tmpFile.Close())
	return tmpFile.Name()
}

// testClickHouseProductionLoadTable validates the production ClickHouse connector's
// LoadTable() method by exercising the REAL code path in
// warehouse/integrations/clickhouse/clickhouse.go — CSV download, transaction,
// INSERT with type casting — rather than test-local DDL helpers.
//
//nolint:unused // called from testIdempotentClickHouse via t.Run
func testClickHouseProductionLoadTable(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	namespace string,
) {
	t.Helper()

	// Initialize misc package to ensure pkgLogger is non-nil — production
	// loadTable defers misc.RemoveFilePaths which needs the logger.
	misc.Init()

	tableName := "tracks_prod_load"

	// Create a ReplacingMergeTree table for the production LoadTable test.
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.%s (
			id String,
			user_id String,
			event String,
			received_at DateTime
		) ENGINE = ReplacingMergeTree()
		ORDER BY (received_at, id)
		PARTITION BY toDate(received_at)
	`, namespace, tableName))
	require.NoError(t, err, "failed to create production load table")

	// Define test events — include duplicates for dedup validation.
	events := []clickhouseTestEvent{
		{ID: "prod-001", UserID: "user-1", Event: "page_view", ReceivedAt: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)},
		{ID: "prod-002", UserID: "user-2", Event: "button_click", ReceivedAt: time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)},
		{ID: "prod-003", UserID: "user-1", Event: "form_submit", ReceivedAt: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)},
		{ID: "prod-001", UserID: "user-1", Event: "page_view", ReceivedAt: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)}, // duplicate of prod-001
	}
	uniqueCount := 3

	// Build the table schema matching the ClickHouse column types.
	tracksSchema := whutils.ModelTableSchema{
		"id":          "string",
		"user_id":     "string",
		"event":       "string",
		"received_at": "datetime",
	}

	mockUploader := &chTestUploader{
		Uploader: whutils.NewNoOpUploader(),
		schemaInUpload: map[string]whutils.ModelTableSchema{
			tableName: tracksSchema,
		},
		schemaInWarehouse: map[string]whutils.ModelTableSchema{
			tableName: tracksSchema,
		},
	}

	loadFilePath := createChTestLoadFile(t, events)
	mockDownloader := &chTestDownloader{
		files: map[string][]string{
			tableName: {loadFilePath},
		},
	}

	// Create the production ClickHouse connector.
	testConf := config.New()
	ch := chconnector.New(testConf, logger.NOP, stats.NOP)

	// Inject test dependencies.
	ch.DB = sqlmw.New(db)
	ch.Namespace = namespace
	ch.Uploader = mockUploader
	ch.LoadFileDownloader = mockDownloader
	ch.Warehouse = whutils.ModelWarehouse{
		WorkspaceID: "test_workspace_ch",
		Source: backendconfig.SourceT{
			ID:               "source_ch_prod_test",
			SourceDefinition: backendconfig.SourceDefinitionT{Name: "test_source"},
		},
		Destination: backendconfig.DestinationT{
			ID:                    "dest_ch_prod_test",
			Config:                map[string]interface{}{},
			DestinationDefinition: backendconfig.DestinationDefinitionT{Name: "CLICKHOUSE"},
		},
		Namespace: namespace,
		Type:      whutils.CLICKHOUSE,
	}

	// First load: production LoadTable inserts events via transaction + INSERT.
	loadStats, loadErr := ch.LoadTable(ctx, tableName)
	require.NoError(t, loadErr, "production LoadTable must succeed for first load")
	require.NotNil(t, loadStats, "LoadTable stats must not be nil")

	t.Logf("ch production_load_table: first load — RowsInserted=%d", loadStats.RowsInserted)

	// Force merge to collapse duplicates (ReplacingMergeTree dedup is lazy).
	_, err = db.ExecContext(ctx, fmt.Sprintf("OPTIMIZE TABLE %s.%s FINAL", namespace, tableName))
	require.NoError(t, err, "failed to optimize table after first load")

	var firstLoadCount int
	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s.%s", namespace, tableName)).Scan(&firstLoadCount)
	require.NoError(t, err)
	require.Equal(t, uniqueCount, firstLoadCount,
		"after first load + OPTIMIZE, ReplacingMergeTree must produce %d unique rows from %d events",
		uniqueCount, len(events))

	// Replay: production LoadTable again with the same events.
	replayPath := createChTestLoadFile(t, events)
	mockDownloader.files[tableName] = []string{replayPath}

	replayStats, replayErr := ch.LoadTable(ctx, tableName)
	require.NoError(t, replayErr, "production LoadTable replay must succeed")
	require.NotNil(t, replayStats)

	t.Logf("ch production_load_table: replay — RowsInserted=%d", replayStats.RowsInserted)

	// Force merge again after replay.
	_, err = db.ExecContext(ctx, fmt.Sprintf("OPTIMIZE TABLE %s.%s FINAL", namespace, tableName))
	require.NoError(t, err, "failed to optimize table after replay")

	var replayRowCount int
	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s.%s", namespace, tableName)).Scan(&replayRowCount)
	require.NoError(t, err)
	require.Equal(t, uniqueCount, replayRowCount,
		"after replay + OPTIMIZE, ReplacingMergeTree must maintain %d unique rows", uniqueCount)

	t.Logf("ch production_load_table: PASSED — %d rows after replay (idempotent via ReplacingMergeTree)", replayRowCount)
}
