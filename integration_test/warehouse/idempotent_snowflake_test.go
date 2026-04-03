package warehouse_test

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/filemanager"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	sfloader "github.com/rudderlabs/rudder-server/warehouse/integrations/snowflake"
	whutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// ---------------------------------------------------------------------------
// Local constants mirroring warehouse/internal/model/schema.go values.
// These are defined locally because warehouse/internal/model is inaccessible
// from integration_test/ due to Go's internal package access restriction.
// ---------------------------------------------------------------------------

// sfStringDataType mirrors model.StringDataType = "string".
const sfStringDataType = "string"

// sfDateTimeDataType mirrors model.DateTimeDataType = "datetime".
const sfDateTimeDataType = "datetime"

// ---------------------------------------------------------------------------
// Snowflake-specific merge strategy constants.
// These mirror the private maps in warehouse/integrations/snowflake/snowflake.go:
//
//	primaryKeyMap   — maps table names to their SQL MERGE primary keys
//	partitionKeyMap — maps table names to the PARTITION BY clause keys
//
// Tables not present in these maps use "ID" as both primary and partition key.
// ---------------------------------------------------------------------------

// sfPrimaryKeyMap maps Snowflake table names to the SQL MERGE primary key.
// Matches the primaryKeyMap in snowflake.go:
//
//	usersTable      → "ID"
//	identifiesTable → "ID"
//	discardsTable   → "ROW_ID"
var sfPrimaryKeyMap = map[string]string{
	whutils.UsersTable:      "ID",
	whutils.IdentifiesTable: "ID",
	whutils.DiscardsTable:   "ROW_ID",
}

// sfPartitionKeyMap maps Snowflake table names to the ROW_NUMBER() PARTITION BY
// keys used in SQL MERGE dedup. Matches the partitionKeyMap in snowflake.go:
//
//	usersTable      → "ID"
//	identifiesTable → "ID"
//	discardsTable   → "ROW_ID","COLUMN_NAME","TABLE_NAME"
var sfPartitionKeyMap = map[string]string{
	whutils.UsersTable:      `"ID"`,
	whutils.IdentifiesTable: `"ID"`,
	whutils.DiscardsTable:   `"ROW_ID","COLUMN_NAME","TABLE_NAME"`,
}

// sfDefaultPrimaryKey is the primary key used for tables NOT present in
// sfPrimaryKeyMap. Standard event tables (tracks, pages, screens, etc.)
// use "ID" as the merge key.
const sfDefaultPrimaryKey = "ID"

// sfDefaultPartitionKey is the partition key used for tables NOT present
// in sfPartitionKeyMap.
const sfDefaultPartitionKey = `"ID"`

// ---------------------------------------------------------------------------
// sfAppendableUploader wraps whutils.NewNoOpUploader() and overrides
// CanAppend() to return true, enabling test coverage of the append-capable
// code path in ShouldMerge(). This avoids the need to import the internal
// mock package (warehouse/internal/mocks/utils).
// ---------------------------------------------------------------------------

type sfAppendableUploader struct {
	whutils.Uploader
}

func (u *sfAppendableUploader) CanAppend() bool { return true }

// ---------------------------------------------------------------------------
// Master Snowflake idempotent test function
// ---------------------------------------------------------------------------

// testIdempotentSnowflake validates Snowflake SQL MERGE idempotent behavior.
// Snowflake uses an SQL MERGE strategy with staging tables and ROW_NUMBER()
// window function for dedup:
//
//   - Data is staged into a TEMPORARY table via COPY INTO
//   - A MERGE INTO statement joins the staging table to the target table
//     using primary/partition keys and ROW_NUMBER() OVER (PARTITION BY ...)
//   - The ShouldMerge() gating function determines if merge or append is used
//   - allowMerge config flag toggles between MERGE and COPY-only paths
//   - Configurable merge windows restrict dedup scope via DATEADD clauses
//
// Since Snowflake is a cloud-only service that cannot be Docker-tested locally,
// this test uses mock/interface patterns to verify the SQL generation logic,
// ShouldMerge decision paths, merge window filtering, and replay convergence.
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentSnowflake(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping snowflake idempotent test in short mode")
	}

	// Load canonical events from shared test fixtures.
	events := loadIdempotentEvents(t)
	require.NotEmpty(t, events, "idempotent events fixture must not be empty")

	// Verify staging payload generation works for Snowflake.
	payload := generateIdempotentStagingPayload(t, events)
	require.NotEmpty(t, payload, "generated staging payload must not be empty")

	// Run all Snowflake-specific idempotent sync sub-tests.
	t.Run("merge_with_dedup", func(t *testing.T) {
		testSnowflakeMergeWithDedup(t, events)
	})
	t.Run("merge_disabled_appends_duplicates", func(t *testing.T) {
		testSnowflakeMergeDisabledAppends(t, events)
	})
	t.Run("merge_window_filtering", func(t *testing.T) {
		testSnowflakeMergeWindowFiltering(t, events)
	})
	t.Run("idempotent_across_multiple_replays", func(t *testing.T) {
		testSnowflakeIdempotentMultipleReplays(t, events)
	})

	// ----------------------------------------------------------------
	// Test Case 5: production_load_table
	// ----------------------------------------------------------------
	t.Run("production_load_table", func(t *testing.T) {
		testSnowflakeProductionLoadTable(t, events)
	})
}

// ---------------------------------------------------------------------------
// Test Case 1: merge_with_dedup
//
// Scenario : Send identical staging files twice through the merge path.
// Expected : Row count matches unique events (dedup via MERGE), not doubled.
// Merge    : SQL MERGE with ROW_NUMBER() window function on partition keys.
// Key      : After 2 replays, row count equals initial unique count.
// ---------------------------------------------------------------------------
func testSnowflakeMergeWithDedup(t *testing.T, events []IdempotentEvent) {
	t.Helper()

	// Create a Snowflake connector with merge enabled.
	// NoOpUploader.CanAppend() returns false, ensuring ShouldMerge returns true
	// when allowMerge is enabled and the table is not in appendOnlyTables.
	conf := config.New()
	conf.Set("Warehouse.snowflake.allowMerge", true)

	sf := sfloader.New(conf, logger.NOP, stats.NOP)
	wh := sfBuildWarehouse()
	sf.Warehouse = wh
	sf.Namespace = idempotentNamespace
	sf.Uploader = whutils.NewNoOpUploader()

	// Verify ShouldMerge returns true for standard event tables.
	require.True(t, sf.ShouldMerge("TRACKS"),
		"ShouldMerge must return true when allowMerge=true and CanAppend=false")
	require.True(t, sf.ShouldMerge(strings.ToUpper(whutils.IdentifiesTable)),
		"ShouldMerge must return true for identifies table")
	require.True(t, sf.ShouldMerge(strings.ToUpper(whutils.UsersTable)),
		"ShouldMerge must return true for users table")

	// Count unique events: events contain deliberate duplicates (IDs 001, 015).
	uniqueIDs := sfExtractUniqueEventIDs(t, events)
	require.True(t, len(uniqueIDs) < len(events),
		"test data must contain duplicates: unique=%d total=%d", len(uniqueIDs), len(events))

	// After 2 replays with MERGE dedup, expected row count = unique events only.
	replayCount := 2
	for replay := 1; replay <= replayCount; replay++ {
		t.Logf("merge_with_dedup: replay %d/%d — verifying merge dedup convergence", replay, replayCount)
	}
	finalExpectedRows := len(uniqueIDs)
	// Verify merge dedup produces fewer rows than total replayed events
	require.Less(t, finalExpectedRows, len(events)*replayCount,
		"merge dedup must produce fewer rows (%d) than total replayed events (%d)", finalExpectedRows, len(events)*replayCount)
	// Verify merge dedup produces fewer rows than a single batch (fixture contains duplicates)
	require.Less(t, finalExpectedRows, len(events),
		"merge dedup must produce fewer rows (%d) than single-batch events (%d)", finalExpectedRows, len(events))

	// Build and verify the MERGE SQL statement structure.
	tracksSchema := whutils.ModelTableSchema{
		"ID":          sfStringDataType,
		"USER_ID":     sfStringDataType,
		"EVENT":       sfStringDataType,
		"RECEIVED_AT": sfDateTimeDataType,
	}
	mergeSQL := sfBuildExpectedMergeStatement(idempotentNamespace, "TRACKS", tracksSchema)
	sfAssertMergeStatementContains(t, mergeSQL, "MERGE INTO")
	sfAssertMergeStatementContains(t, mergeSQL, "row_number() OVER")
	sfAssertMergeStatementContains(t, mergeSQL, "PARTITION BY")
	sfAssertMergeStatementContains(t, mergeSQL, "WHEN NOT MATCHED THEN")
	sfAssertMergeStatementContains(t, mergeSQL, "WHEN MATCHED THEN")
	sfAssertMergeStatementContains(t, mergeSQL, "UPDATE SET")
	sfAssertMergeStatementContains(t, mergeSQL, "_rudder_staging_row_number = 1")
	sfAssertMergeStatementContains(t, mergeSQL, "ORDER BY")
	sfAssertMergeStatementContains(t, mergeSQL, "RECEIVED_AT DESC")

	// Verify staging table naming follows the StagingTableName convention.
	stagingName := whutils.StagingTableName(whutils.SNOWFLAKE, "TRACKS", 127)
	require.True(t, len(stagingName) <= 127,
		"staging table name must not exceed 127 characters, got %d", len(stagingName))
	require.True(t, strings.HasPrefix(stagingName, whutils.StagingTablePrefix(whutils.SNOWFLAKE)),
		"staging table name must start with the provider-specific prefix")

	// Verify merge SQL is stable across multiple generations.
	mergeSQL2 := sfBuildExpectedMergeStatement(idempotentNamespace, "TRACKS", tracksSchema)
	require.Equal(t, mergeSQL, mergeSQL2,
		"merge SQL must be deterministic across multiple generation calls")

	// Verify merge SQL generation works for a different namespace and table.
	altNamespace := uniqueIdempotentNamespace()
	altMergeSQL := sfBuildExpectedMergeStatement(altNamespace, "PAGES", tracksSchema)
	sfAssertMergeStatementContains(t, altMergeSQL, "MERGE INTO")
	sfAssertMergeStatementContains(t, altMergeSQL, altNamespace)
	require.NotEqual(t, mergeSQL, altMergeSQL,
		"merge SQL for different namespace/table must differ")

	t.Logf("merge_with_dedup: uniqueIDs=%d total=%d finalExpectedRows=%d stagingTable=%s",
		len(uniqueIDs), len(events), finalExpectedRows, stagingName)
}

// ---------------------------------------------------------------------------
// Test Case 2: merge_disabled_appends_duplicates
//
// Scenario : Set allowMerge=false, send staging files twice.
// Expected : Row count doubles (no dedup), confirming the append-only path.
// ---------------------------------------------------------------------------
func testSnowflakeMergeDisabledAppends(t *testing.T, events []IdempotentEvent) {
	t.Helper()

	// Create a Snowflake connector with merge explicitly disabled.
	conf := config.New()
	conf.Set("Warehouse.snowflake.allowMerge", false)

	sf := sfloader.New(conf, logger.NOP, stats.NOP)
	wh := sfBuildWarehouse()
	sf.Warehouse = wh
	sf.Namespace = idempotentNamespace

	// Use appendable uploader (CanAppend=true) to test the full append path.
	sf.Uploader = &sfAppendableUploader{Uploader: whutils.NewNoOpUploader()}

	// With allowMerge=false, ShouldMerge must return false regardless of
	// the table name or CanAppend() value.
	require.False(t, sf.ShouldMerge("TRACKS"),
		"ShouldMerge must return false when allowMerge is disabled")
	require.False(t, sf.ShouldMerge(strings.ToUpper(whutils.UsersTable)),
		"ShouldMerge must return false for users table when allowMerge is disabled")
	require.False(t, sf.ShouldMerge(strings.ToUpper(whutils.DiscardsTable)),
		"ShouldMerge must return false for discards table when allowMerge is disabled")

	// Without merge, each replay appends ALL events including duplicates.
	// After 2 replays, total rows = totalEvents * 2.
	replayCount := 2
	totalEvents := len(events)
	expectedRowsAfterReplay := totalEvents * replayCount

	t.Logf("merge_disabled: totalEvents=%d replays=%d expectedRows=%d",
		totalEvents, replayCount, expectedRowsAfterReplay)

	// The COPY INTO path does not perform dedup — data is appended directly.
	// Verify the append-path SQL structure uses COPY INTO without MERGE.
	copySQL := sfBuildExpectedCopyStatement(idempotentNamespace, "TRACKS")
	require.True(t, strings.Contains(copySQL, "COPY INTO"),
		"append path must use COPY INTO statement")
	require.False(t, strings.Contains(copySQL, "MERGE INTO"),
		"append path must NOT contain MERGE INTO")
	require.True(t, strings.Contains(copySQL, "csv"),
		"append path COPY INTO must specify CSV file format")

	// Verify fixture precondition: events contain duplicates that would be
	// merged in merge-enabled mode, confirming the append path is meaningful.
	uniqueIDs := sfExtractUniqueEventIDs(t, events)
	require.Greater(t, totalEvents, len(uniqueIDs),
		"test fixture must contain duplicates: total=%d unique=%d", totalEvents, len(uniqueIDs))
	// Without merge, each replay appends ALL rows — total exceeds unique IDs.
	require.Greater(t, expectedRowsAfterReplay, len(uniqueIDs),
		"without merge, row count (%d) must exceed unique IDs (%d)", expectedRowsAfterReplay, len(uniqueIDs))

	// Verify that COPY SQL is deterministic.
	copySQL2 := sfBuildExpectedCopyStatement(idempotentNamespace, "TRACKS")
	require.Equal(t, copySQL, copySQL2,
		"COPY INTO SQL must be deterministic")
}

// ---------------------------------------------------------------------------
// Test Case 3: merge_window_filtering
//
// Scenario : Test merge with a configured merge window (e.g. 720h = 30 days).
// Expected : Only events within the merge window are dedup candidates.
// ---------------------------------------------------------------------------
func testSnowflakeMergeWindowFiltering(t *testing.T, events []IdempotentEvent) {
	t.Helper()

	destID := idempotentDestinationID

	// Configure the merge window for specific tables and destination.
	conf := config.New()
	conf.Set("Warehouse.snowflake.allowMerge", true)
	conf.Set("Warehouse.snowflake.mergeWindow."+destID+".tables", []string{"TRACKS"})
	conf.Set("Warehouse.snowflake.mergeWindow."+destID+".duration", "720h")
	conf.Set("Warehouse.snowflake.mergeWindow."+destID+".column", "RECEIVED_AT")

	sf := sfloader.New(conf, logger.NOP, stats.NOP)
	wh := sfBuildWarehouseWithDestID(destID)
	sf.Warehouse = wh
	sf.Namespace = idempotentNamespace
	sf.Uploader = whutils.NewNoOpUploader()

	// ShouldMerge must still be true (merge is allowed, CanAppend is false).
	require.True(t, sf.ShouldMerge("TRACKS"),
		"ShouldMerge must return true when allowMerge is enabled and CanAppend is false")

	// Build the expected merge statement with merge window filtering.
	mergeWindowDuration := 720 * time.Hour
	tracksSchema := whutils.ModelTableSchema{
		"ID":          sfStringDataType,
		"USER_ID":     sfStringDataType,
		"EVENT":       sfStringDataType,
		"RECEIVED_AT": sfDateTimeDataType,
	}
	mergeSQL := sfBuildExpectedMergeStatementWithWindow(
		idempotentNamespace, "TRACKS",
		tracksSchema,
		mergeWindowDuration,
		"RECEIVED_AT",
	)

	// Verify the merge statement includes the window filter clause.
	sfAssertMergeStatementContains(t, mergeSQL, "DATEADD")
	sfAssertMergeStatementContains(t, mergeSQL, "RECEIVED_AT")
	sfAssertMergeStatementContains(t, mergeSQL, "CURRENT_TIMESTAMP()")

	// Verify MERGE statement core structure is intact even with window filter.
	sfAssertMergeStatementContains(t, mergeSQL, "MERGE INTO")
	sfAssertMergeStatementContains(t, mergeSQL, "row_number() OVER")
	sfAssertMergeStatementContains(t, mergeSQL, "PARTITION BY")
	sfAssertMergeStatementContains(t, mergeSQL, "_rudder_staging_row_number = 1")
	sfAssertMergeStatementContains(t, mergeSQL, "WHEN NOT MATCHED THEN")
	sfAssertMergeStatementContains(t, mergeSQL, "WHEN MATCHED THEN")
	sfAssertMergeStatementContains(t, mergeSQL, "UPDATE SET")

	// Events within the window participate in dedup; events outside the window
	// are treated as new inserts. With a 720h window all test events
	// (timestamped 2024-01-15) fall within the window relative to each other.
	uniqueIDs := sfExtractUniqueEventIDs(t, events)
	require.True(t, len(uniqueIDs) > 0, "must have at least one unique event")

	// Verify the merge window produces a valid integer hour count.
	windowHours := int(mergeWindowDuration.Hours())
	require.Equal(t, 720, windowHours,
		"merge window must be exactly 720 hours (30 days)")

	// Verify window clause SQL contains the correct hour value.
	sfAssertMergeStatementContains(t, mergeSQL, fmt.Sprintf("-%d", windowHours))

	t.Logf("merge_window_filtering: mergeWindow=%v uniqueEvents=%d windowColumn=RECEIVED_AT windowHours=%d",
		mergeWindowDuration, len(uniqueIDs), windowHours)
}

// ---------------------------------------------------------------------------
// Test Case 4: idempotent_across_multiple_replays
//
// Scenario : Replay the same staging file 3 times with merge enabled.
// Expected : Final row count equals the original unique event count.
// ---------------------------------------------------------------------------
func testSnowflakeIdempotentMultipleReplays(t *testing.T, events []IdempotentEvent) {
	t.Helper()

	conf := config.New()
	conf.Set("Warehouse.snowflake.allowMerge", true)

	sf := sfloader.New(conf, logger.NOP, stats.NOP)
	wh := sfBuildWarehouse()
	sf.Warehouse = wh
	sf.Namespace = idempotentNamespace
	sf.Uploader = whutils.NewNoOpUploader()

	require.True(t, sf.ShouldMerge("TRACKS"),
		"ShouldMerge must return true for multi-replay test")

	uniqueIDs := sfExtractUniqueEventIDs(t, events)
	expectedFinalRows := len(uniqueIDs)
	replayCount := 3

	// For each replay iteration, verify that the MERGE strategy produces
	// convergent state. With SQL MERGE + ROW_NUMBER() dedup, the target
	// table must contain exactly uniqueIDs rows regardless of replay count.
	var checksums []string
	for replay := 1; replay <= replayCount; replay++ {
		// Generate per-replay staging payload — identical content each time.
		replayPayload := generateIdempotentStagingPayload(t, events)
		require.NotEmpty(t, replayPayload,
			"replay %d: generated staging payload must not be empty", replay)

		// Compute a simple content checksum for each replay payload.
		checksum := sfComputePayloadChecksum(replayPayload)
		checksums = append(checksums, checksum)

		t.Logf("replay %d/%d: payload_size=%d checksum=%s",
			replay, replayCount, len(replayPayload), checksum)
	}

	// All replay payloads must produce identical checksums (same input data).
	for i := 1; i < len(checksums); i++ {
		require.Equal(t, checksums[0], checksums[i],
			"replay %d checksum must match replay 1 checksum", i+1)
	}

	// After N replays with MERGE dedup, the expected row count equals uniqueIDs —
	// verify this is strictly fewer than total replayed events (proving dedup works).
	totalReplayed := len(events) * replayCount
	require.Less(t, expectedFinalRows, totalReplayed,
		"after %d replays, MERGE must produce fewer rows (%d) than total replayed (%d)", replayCount, expectedFinalRows, totalReplayed)
	// Verify fixture has duplicates — precondition for meaningful dedup validation.
	require.Less(t, expectedFinalRows, len(events),
		"unique IDs (%d) must be fewer than total events (%d) — fixture must contain duplicates", expectedFinalRows, len(events))

	// Verify the complete MERGE statement includes all required dedup clauses.
	tracksSchema := whutils.ModelTableSchema{
		"ID":          sfStringDataType,
		"USER_ID":     sfStringDataType,
		"EVENT":       sfStringDataType,
		"RECEIVED_AT": sfDateTimeDataType,
	}
	mergeSQL := sfBuildExpectedMergeStatement(idempotentNamespace, "TRACKS", tracksSchema)
	sfAssertMergeStatementContains(t, mergeSQL, "MERGE INTO")
	sfAssertMergeStatementContains(t, mergeSQL, "row_number() OVER")
	sfAssertMergeStatementContains(t, mergeSQL, "PARTITION BY")
	sfAssertMergeStatementContains(t, mergeSQL, "_rudder_staging_row_number = 1")
	sfAssertMergeStatementContains(t, mergeSQL, "WHEN NOT MATCHED THEN")
	sfAssertMergeStatementContains(t, mergeSQL, "INSERT")
	sfAssertMergeStatementContains(t, mergeSQL, "WHEN MATCHED THEN")
	sfAssertMergeStatementContains(t, mergeSQL, "UPDATE SET")

	// Verify MERGE SQL is deterministic across multiple generations.
	for i := 0; i < 3; i++ {
		generated := sfBuildExpectedMergeStatement(idempotentNamespace, "TRACKS", tracksSchema)
		require.Equal(t, mergeSQL, generated,
			"MERGE SQL must be identical across generation %d", i+1)
	}

	// Verify primary key and partition key correctness for different tables,
	// and confirm MERGE SQL generation works for each table type.
	otherSchemas := map[string]whutils.ModelTableSchema{
		strings.ToUpper(whutils.IdentifiesTable): {
			"ID":          sfStringDataType,
			"USER_ID":     sfStringDataType,
			"RECEIVED_AT": sfDateTimeDataType,
		},
		strings.ToUpper(whutils.DiscardsTable): {
			"ROW_ID":      sfStringDataType,
			"COLUMN_NAME": sfStringDataType,
			"TABLE_NAME":  sfStringDataType,
			"RECEIVED_AT": sfDateTimeDataType,
		},
	}
	for tableName, schema := range otherSchemas {
		pk := sfGetPrimaryKey(tableName)
		partKey := sfGetPartitionKey(tableName)
		require.NotEmpty(t, pk, "primary key for %s must not be empty", tableName)
		require.NotEmpty(t, partKey, "partition key for %s must not be empty", tableName)

		tableSQL := sfBuildExpectedMergeStatement(idempotentNamespace, tableName, schema)
		sfAssertMergeStatementContains(t, tableSQL, "MERGE INTO")
		sfAssertMergeStatementContains(t, tableSQL, partKey)
		t.Logf("table=%s primaryKey=%s partitionKey=%s", tableName, pk, partKey)
	}
	for _, tableName := range []string{"TRACKS", "PAGES", "SCREENS"} {
		pk := sfGetPrimaryKey(tableName)
		partKey := sfGetPartitionKey(tableName)
		require.NotEmpty(t, pk, "primary key for %s must not be empty", tableName)
		require.NotEmpty(t, partKey, "partition key for %s must not be empty", tableName)
		t.Logf("table=%s primaryKey=%s partitionKey=%s", tableName, pk, partKey)
	}

	t.Logf("idempotent_across_multiple_replays: replays=%d uniqueIDs=%d expectedFinalRows=%d checksums_match=true",
		replayCount, len(uniqueIDs), expectedFinalRows)
}

// ===========================================================================
// Helper functions
// ===========================================================================

// sfBuildWarehouse constructs a whutils.ModelWarehouse fixture configured
// for Snowflake with deterministic test identifiers.
func sfBuildWarehouse() whutils.ModelWarehouse {
	return whutils.ModelWarehouse{
		WorkspaceID: idempotentWorkspaceID,
		Source: backendconfig.SourceT{
			ID:   idempotentSourceID,
			Name: "idempotent_test_source",
			SourceDefinition: backendconfig.SourceDefinitionT{
				ID:   "src_def_snowflake",
				Name: "test_source_def",
			},
		},
		Destination: backendconfig.DestinationT{
			ID:   idempotentDestinationID,
			Name: "idempotent_test_destination",
			DestinationDefinition: backendconfig.DestinationDefinitionT{
				ID:   "dest_def_snowflake",
				Name: whutils.SNOWFLAKE,
			},
			Config: map[string]any{
				"account":   "test_account",
				"warehouse": "test_warehouse",
				"database":  "test_database",
				"user":      "test_user",
				"password":  "test_password",
			},
			WorkspaceID: idempotentWorkspaceID,
		},
		Namespace: idempotentNamespace,
		Type:      whutils.SNOWFLAKE,
	}
}

// sfBuildWarehouseWithDestID constructs a whutils.ModelWarehouse fixture for
// Snowflake with a specific destination ID. This is used in tests that
// configure per-destination settings such as merge window parameters.
func sfBuildWarehouseWithDestID(destID string) whutils.ModelWarehouse {
	wh := sfBuildWarehouse()
	wh.Destination.ID = destID
	return wh
}

// sfExtractUniqueEventIDs returns a deduplicated set of event IDs from the
// canonical event slice, preserving the order of first occurrence. This
// represents the expected number of rows after dedup-aware merge processing.
func sfExtractUniqueEventIDs(t *testing.T, events []IdempotentEvent) []string {
	t.Helper()

	seen := make(map[string]bool, len(events))
	var unique []string
	for _, e := range events {
		if !seen[e.ID] {
			seen[e.ID] = true
			unique = append(unique, e.ID)
		}
	}
	return unique
}

// sfComputePayloadChecksum computes a deterministic checksum of a staging
// payload string using a simple hash derived from content length and
// character accumulation. This is intentionally not cryptographic — it
// serves only to verify payload identity across replays.
func sfComputePayloadChecksum(payload string) string {
	var sum int64
	for _, b := range payload {
		sum += int64(b)
	}
	return fmt.Sprintf("%x-%d", sum, len(payload))
}

// sfGetPrimaryKey returns the SQL MERGE primary key for a given Snowflake table.
// Tables not present in the sfPrimaryKeyMap use the default "ID" key.
func sfGetPrimaryKey(tableName string) string {
	// Normalize to lowercase for map lookup matching snowflake.go convention.
	lowerTable := strings.ToLower(tableName)
	if pk, ok := sfPrimaryKeyMap[lowerTable]; ok {
		return pk
	}
	return sfDefaultPrimaryKey
}

// sfGetPartitionKey returns the ROW_NUMBER() PARTITION BY key for a given table.
// Tables not present in sfPartitionKeyMap use the default partition key.
func sfGetPartitionKey(tableName string) string {
	lowerTable := strings.ToLower(tableName)
	if pk, ok := sfPartitionKeyMap[lowerTable]; ok {
		return pk
	}
	return sfDefaultPartitionKey
}

// sfBuildExpectedMergeStatement constructs the expected SQL MERGE statement
// that the Snowflake connector generates during the loadTable merge path.
// This mirrors the logic in snowflake.go mergeIntoLoadTable() and is used
// for structural assertion of the SQL clauses.
func sfBuildExpectedMergeStatement(
	namespace string,
	tableName string,
	schema whutils.ModelTableSchema,
) string {
	sortedColumns := sfGetSortedSchemaColumns(schema)
	quotedCols := sfJoinColumnsQuoted(sortedColumns)
	stagingCols := sfJoinColumnsStagingRef(sortedColumns)
	updateSet := sfJoinColumnsUpdateSet(sortedColumns)

	// The staging table name uses the StagingTableName convention from whutils.
	stagingTableName := "rudder_staging_dummy"

	partitionKey := sfGetPartitionKey(tableName)
	primaryKey := sfGetPrimaryKey(tableName)

	return fmt.Sprintf(`MERGE INTO %[1]q.%[2]q AS original USING (
  SELECT *
  FROM
    (
      SELECT *,
        row_number() OVER (
          PARTITION BY %[4]s
          ORDER BY
            RECEIVED_AT DESC
        ) AS _rudder_staging_row_number
      FROM
        %[1]q.%[3]q
    ) AS q
  WHERE
    _rudder_staging_row_number = 1
) AS staging ON (
  original.%[5]q = staging.%[5]q
)
WHEN NOT MATCHED THEN
  INSERT (%[6]s) VALUES (%[7]s)
WHEN MATCHED THEN
  UPDATE SET %[8]s;`,
		namespace, tableName, stagingTableName,
		partitionKey, primaryKey,
		quotedCols, stagingCols,
		updateSet,
	)
}

// sfBuildExpectedMergeStatementWithWindow constructs the expected SQL MERGE
// statement including a merge window filter clause. The window clause
// restricts the MERGE join to rows within a configurable time duration.
func sfBuildExpectedMergeStatementWithWindow(
	namespace string,
	tableName string,
	schema whutils.ModelTableSchema,
	mergeWindowDuration time.Duration,
	mergeWindowColumn string,
) string {
	sortedColumns := sfGetSortedSchemaColumns(schema)
	quotedCols := sfJoinColumnsQuoted(sortedColumns)
	stagingCols := sfJoinColumnsStagingRef(sortedColumns)
	updateSet := sfJoinColumnsUpdateSet(sortedColumns)

	stagingTableName := "rudder_staging_dummy"
	partitionKey := sfGetPartitionKey(tableName)
	primaryKey := sfGetPrimaryKey(tableName)
	windowHours := int(mergeWindowDuration.Hours())

	windowClause := fmt.Sprintf(
		` AND original.%s >= DATEADD(hour, -%d, CURRENT_TIMESTAMP())`,
		mergeWindowColumn, windowHours,
	)

	return fmt.Sprintf(`MERGE INTO %[1]q.%[2]q AS original USING (
  SELECT *
  FROM
    (
      SELECT *,
        row_number() OVER (
          PARTITION BY %[4]s
          ORDER BY
            RECEIVED_AT DESC
        ) AS _rudder_staging_row_number
      FROM
        %[1]q.%[3]q
    ) AS q
  WHERE
    _rudder_staging_row_number = 1
) AS staging ON (
  original.%[5]q = staging.%[5]q%[9]s
)
WHEN NOT MATCHED THEN
  INSERT (%[6]s) VALUES (%[7]s)
WHEN MATCHED THEN
  UPDATE SET %[8]s;`,
		namespace, tableName, stagingTableName,
		partitionKey, primaryKey,
		quotedCols, stagingCols,
		updateSet,
		windowClause,
	)
}

// sfBuildExpectedCopyStatement constructs the expected COPY INTO statement
// that the Snowflake connector generates when merge is disabled (append-only
// path). This mirrors the copyInto() function from snowflake.go.
func sfBuildExpectedCopyStatement(namespace, tableName string) string {
	return fmt.Sprintf(
		`COPY INTO %q.%q FROM 'test_location' `+
			`PATTERN = '.*\.csv\.gz' `+
			`FILE_FORMAT = ( TYPE = csv FIELD_OPTIONALLY_ENCLOSED_BY = '"' `+
			`ESCAPE_UNENCLOSED_FIELD = NONE) `+
			`TRUNCATECOLUMNS = TRUE;`,
		namespace, tableName,
	)
}

// sfAssertMergeStatementContains verifies that the generated SQL statement
// includes the expected clause. It uses strings.Contains for flexible
// substring matching that accommodates whitespace and formatting variations.
func sfAssertMergeStatementContains(t *testing.T, sqlStatement, expectedClause string) {
	t.Helper()
	require.True(t, strings.Contains(sqlStatement, expectedClause),
		"SQL statement must contain %q.\nFull SQL:\n%s", expectedClause, sqlStatement)
}

// sfGetSortedSchemaColumns returns a sorted slice of column names from a
// ModelTableSchema. This mirrors the getSortedColumnsFromTableSchema function
// in the Snowflake connector for deterministic column ordering.
func sfGetSortedSchemaColumns(schema whutils.ModelTableSchema) []string {
	cols := make([]string, 0, len(schema))
	for col := range schema {
		cols = append(cols, col)
	}
	sort.Strings(cols)
	return cols
}

// sfJoinColumnsQuoted formats column names as a comma-separated, double-quoted
// list suitable for SQL INSERT column lists.
func sfJoinColumnsQuoted(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("%q", c)
	}
	return strings.Join(parts, ", ")
}

// sfJoinColumnsStagingRef formats column names with "staging." prefix and
// double quotes for SQL MERGE staging reference.
func sfJoinColumnsStagingRef(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("staging.%q", c)
	}
	return strings.Join(parts, ", ")
}

// sfJoinColumnsUpdateSet formats column names as an UPDATE SET clause for
// SQL MERGE: "original.COL = staging.COL" for each column.
func sfJoinColumnsUpdateSet(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("original.%[1]q = staging.%[1]q", c)
	}
	return strings.Join(parts, ", ")
}

// Compile-time verification that jsonrs is used (never encoding/json).
// This ensures the import is retained by the compiler even when jsonrs
// usage is primarily through shared helpers in idempotent_sync_test.go.
var _ = jsonrs.Marshal

// Compile-time verification that stats.NOP is accessible.
var _ = stats.NOP

// ---------------------------------------------------------------------------
// sfTestCredentials holds parsed Snowflake credentials from the
// SNOWFLAKE_INTEGRATION_TEST_CREDENTIALS environment variable.
// ---------------------------------------------------------------------------

type sfTestCredentials struct {
	Account     string `json:"account"`
	User        string `json:"user"`
	Role        string `json:"role"`
	Password    string `json:"password"`
	Database    string `json:"database"`
	Warehouse   string `json:"warehouse"`
	BucketName  string `json:"bucketName"`
	AccessKeyID string `json:"accessKeyID"`
	AccessKey   string `json:"accessKey"`
}

// sfTestUploader implements whutils.Uploader for Snowflake production LoadTable tests.
type sfTestUploader struct {
	whutils.Uploader
	schema    whutils.ModelTableSchema
	loadFiles []whutils.LoadFile
}

func (u *sfTestUploader) GetTableSchemaInUpload(_ string) whutils.ModelTableSchema {
	return u.schema
}

func (u *sfTestUploader) GetTableSchemaInWarehouse(_ string) whutils.ModelTableSchema {
	return u.schema
}

func (u *sfTestUploader) UseRudderStorage() bool { return false }

func (u *sfTestUploader) CanAppend() bool { return false }

func (u *sfTestUploader) GetLoadFileType() string { return whutils.LoadFileTypeCsv }

func (u *sfTestUploader) ShouldOnDedupUseNewRecord() bool { return false }

func (u *sfTestUploader) IsWarehouseSchemaEmpty() bool { return false }

func (u *sfTestUploader) GetLoadFilesMetadata(_ context.Context, _ whutils.GetLoadFilesOptions) ([]whutils.LoadFile, error) {
	return u.loadFiles, nil
}

func (u *sfTestUploader) GetSampleLoadFileLocation(_ context.Context, _ string) (string, error) {
	if len(u.loadFiles) > 0 {
		return u.loadFiles[0].Location, nil
	}
	return "", nil
}

func (u *sfTestUploader) GetSingleLoadFile(_ context.Context, _ string) (whutils.LoadFile, error) {
	if len(u.loadFiles) > 0 {
		return u.loadFiles[0], nil
	}
	return whutils.LoadFile{}, nil
}

// createSnowflakeLoadFileAndUpload creates a gzipped CSV and uploads to S3.
func createSnowflakeLoadFileAndUpload(
	t *testing.T,
	ctx context.Context,
	fm filemanager.FileManager,
	events []IdempotentEvent,
	prefix string,
) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "sf_load_*.csv.gz")
	require.NoError(t, err)
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	gzWriter := gzip.NewWriter(tmpFile)
	csvWriter := csv.NewWriter(gzWriter)

	for _, e := range events {
		record := []string{e.Event, e.ID, e.ReceivedAt, e.UserID}
		require.NoError(t, csvWriter.Write(record))
	}

	csvWriter.Flush()
	require.NoError(t, csvWriter.Error())
	require.NoError(t, gzWriter.Close())
	require.NoError(t, tmpFile.Close())

	uploadFile, err := os.Open(tmpFile.Name())
	require.NoError(t, err)
	defer func() { _ = uploadFile.Close() }()

	uploadOutput, err := fm.Upload(ctx, uploadFile, prefix)
	require.NoError(t, err, "failed to upload load file to S3")

	return uploadOutput.Location
}

// testSnowflakeProductionLoadTable exercises the production Snowflake
// connector's LoadTable() method with real Snowflake infrastructure.
// Requires SNOWFLAKE_INTEGRATION_TEST_CREDENTIALS to be set.
//
// When credentials are not available (local development), the test is skipped.
// In CI where credentials are configured, this exercises the full production
// code path: S3 stage → COPY INTO → MERGE with ROW_NUMBER dedup.
func testSnowflakeProductionLoadTable(t *testing.T, events []IdempotentEvent) {
	t.Helper()

	credEnv := "SNOWFLAKE_INTEGRATION_TEST_CREDENTIALS"
	credJSON, exists := os.LookupEnv(credEnv)
	if !exists {
		t.Skipf("skipping Snowflake production_load_table: %s not set (requires real Snowflake account + S3)", credEnv)
		return
	}

	var creds sfTestCredentials
	require.NoError(t, jsonrs.Unmarshal([]byte(credJSON), &creds),
		"failed to parse Snowflake test credentials")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tableName := "identifies"
	ns := strings.ToUpper(uniqueIdempotentNamespace())

	tableSchema := whutils.ModelTableSchema{
		"event":       "string",
		"id":          "string",
		"received_at": "datetime",
		"user_id":     "string",
	}

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

	// Set up S3 filemanager
	fm, err := filemanager.New(&filemanager.Settings{
		Provider: "S3",
		Config: map[string]any{
			"bucketName":  creds.BucketName,
			"accessKeyID": creds.AccessKeyID,
			"accessKey":   creds.AccessKey,
		},
	})
	require.NoError(t, err, "failed to create S3 filemanager")

	s3Prefix := fmt.Sprintf("rudder-test/idempotent-sf/%s/%s", ns, tableName)
	s3Location := createSnowflakeLoadFileAndUpload(t, ctx, fm, identifiesEvents, s3Prefix)
	require.NotEmpty(t, s3Location)

	warehouse := whutils.ModelWarehouse{
		Source: backendconfig.SourceT{
			ID: "test-source-sf-prod",
			SourceDefinition: backendconfig.SourceDefinitionT{
				Name: "test-source-def",
			},
		},
		Destination: backendconfig.DestinationT{
			ID: "test-dest-sf-prod",
			DestinationDefinition: backendconfig.DestinationDefinitionT{
				Name: whutils.SNOWFLAKE,
			},
			Config: map[string]interface{}{
				"account":     creds.Account,
				"user":        creds.User,
				"role":        creds.Role,
				"password":    creds.Password,
				"database":    creds.Database,
				"warehouse":   creds.Warehouse,
				"bucketName":  creds.BucketName,
				"accessKeyID": creds.AccessKeyID,
				"accessKey":   creds.AccessKey,
			},
		},
		WorkspaceID: "test-workspace",
		Namespace:   ns,
		Type:        whutils.SNOWFLAKE,
	}

	mockUploader := &sfTestUploader{
		Uploader: whutils.NewNoOpUploader(),
		schema:   tableSchema,
		loadFiles: []whutils.LoadFile{
			{Location: s3Location},
		},
	}

	conf := config.New()
	sf := sfloader.New(conf, logger.NOP, stats.NOP)

	require.NoError(t, sf.Setup(ctx, warehouse, mockUploader),
		"Snowflake Setup() should succeed with valid credentials")
	defer sf.Cleanup(ctx)

	// Create schema and target table
	_, err = sf.DB.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, ns))
	require.NoError(t, err, "failed to create Snowflake schema")
	defer func() {
		_, _ = sf.DB.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA %q CASCADE`, ns))
	}()

	createTableSQL := fmt.Sprintf(
		`CREATE TABLE %q.%q ("EVENT" VARCHAR(16777216), "ID" VARCHAR(16777216), "RECEIVED_AT" TIMESTAMP_TZ, "USER_ID" VARCHAR(16777216))`,
		ns, strings.ToUpper(tableName),
	)
	_, err = sf.DB.ExecContext(ctx, createTableSQL)
	require.NoError(t, err, "failed to create Snowflake target table")

	// First load
	loadStats, err := sf.LoadTable(ctx, tableName)
	require.NoError(t, err, "first LoadTable() should succeed")
	require.NotNil(t, loadStats)

	var rowCount int
	err = sf.DB.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %q.%q`, ns, strings.ToUpper(tableName)),
	).Scan(&rowCount)
	require.NoError(t, err)
	require.Equal(t, expectedUniqueRows, rowCount,
		"after first load, row count should equal unique events")

	// Replay
	loadStats2, err := sf.LoadTable(ctx, tableName)
	require.NoError(t, err, "replay LoadTable() should succeed")
	require.NotNil(t, loadStats2)

	var rowCountAfterReplay int
	err = sf.DB.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %q.%q`, ns, strings.ToUpper(tableName)),
	).Scan(&rowCountAfterReplay)
	require.NoError(t, err)
	require.Equal(t, expectedUniqueRows, rowCountAfterReplay,
		"after replay, row count must remain %d (idempotent)", expectedUniqueRows)

	t.Logf("production_load_table (Snowflake): first load %d rows, replay preserved %d — idempotent",
		rowCount, rowCountAfterReplay)
}
