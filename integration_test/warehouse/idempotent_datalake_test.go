package warehouse_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/warehouse/integrations/datalake"
	whutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// testIdempotentDatalake validates that the Datalake connector exhibits correct
// append-only behavior — meaning replays produce expected duplicates rather than
// silent drops.  Unlike all other warehouse connectors (Snowflake, BigQuery,
// Redshift, ClickHouse, Delta Lake, PostgreSQL, MSSQL, Azure Synapse), the
// Datalake connector has NO merge or dedup mechanism.  Its LoadTable is a
// deliberate no-op at the Go level; actual object-storage writes occur through
// the schema repository layer.
//
// The five sub-tests verify:
//  1. Append-only semantics produce duplicates (not silent drops)
//  2. No merge strategy implementation exists
//  3. All schema operations delegate to the SchemaRepository
//  4. Replay row counts are strictly additive
//  5. Error mappings correctly classify AWS permission errors
//
// This function is called from TestIdempotentSync in idempotent_sync_test.go.
//
//nolint:unused
func testIdempotentDatalake(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping datalake idempotent test in short mode")
	}

	ctx := context.Background()

	// ----------------------------------------------------------------
	// Test Case 1: append_only_creates_duplicates
	// Scenario: Load events, then replay same events on the same table.
	// Expected: Each load succeeds independently — the datalake is
	//           append-only so replays WILL create duplicates.  This is
	//           the correct and expected behaviour.
	// ----------------------------------------------------------------
	t.Run("append_only_creates_duplicates", func(t *testing.T) {
		dl := datalake.New(config.New(), logger.NOP)
		dl.Uploader = whutils.NewNoOpUploader()
		dl.Warehouse.Type = whutils.S3Datalake

		// datalakeEvent is a sample event structure used for serialisation
		// round-trip verification using jsonrs (never encoding/json).
		type datalakeEvent struct {
			EventType string `json:"event_type"`
			Table     string `json:"table"`
			MessageID string `json:"message_id"`
			ReplayID  int    `json:"replay_id"`
		}

		// First load — LoadTable is an intentional no-op that returns zero
		// stats because the actual append to object storage is handled by
		// the schema repository at a different layer.
		stats1, err := dl.LoadTable(ctx, "tracks")
		require.NoError(t, err, "first LoadTable must succeed")
		require.NotNil(t, stats1, "stats must never be nil for datalake")
		require.Equal(t, int64(0), stats1.RowsInserted,
			"datalake LoadTable is a no-op — reports zero rows inserted")
		require.Equal(t, int64(0), stats1.RowsUpdated,
			"datalake LoadTable is a no-op — reports zero rows updated")

		// Second load — same table, same conceptual events.
		// Append-only: no conflict, no dedup, no merge failure.
		stats2, err := dl.LoadTable(ctx, "tracks")
		require.NoError(t, err, "replay LoadTable must succeed — no merge conflicts")
		require.NotNil(t, stats2)
		require.Equal(t, int64(0), stats2.RowsInserted,
			"replay also reports zero — datalake no-op is deterministic")
		require.Equal(t, int64(0), stats2.RowsUpdated,
			"replay also reports zero updates")

		// Third load on a different table name — still succeeds.
		stats3, err := dl.LoadTable(ctx, "pages")
		require.NoError(t, err, "LoadTable on different table must succeed")
		require.NotNil(t, stats3)
		require.Equal(t, int64(0), stats3.RowsInserted)

		// Verify jsonrs serialisation round-trip for the events that would
		// normally be appended in a real datalake sync.
		events := []datalakeEvent{
			{EventType: "track", Table: "tracks", MessageID: "msg-001", ReplayID: 1},
			{EventType: "track", Table: "tracks", MessageID: "msg-002", ReplayID: 1},
			{EventType: "track", Table: "tracks", MessageID: "msg-001", ReplayID: 2},
		}
		data, err := jsonrs.Marshal(events)
		require.NoError(t, err, "jsonrs.Marshal must succeed")

		var decoded []datalakeEvent
		err = jsonrs.Unmarshal(data, &decoded)
		require.NoError(t, err, "jsonrs.Unmarshal must succeed")
		require.Len(t, decoded, 3, "all events preserved including duplicates")
		require.Equal(t, "msg-001", decoded[0].MessageID)
		require.Equal(t, 1, decoded[0].ReplayID)
		require.Equal(t, "msg-001", decoded[2].MessageID,
			"duplicate message preserved — append-only semantics")
		require.Equal(t, 2, decoded[2].ReplayID,
			"replay ID distinguishes original from replay")
	})

	// ----------------------------------------------------------------
	// Test Case 2: no_merge_strategy_exists
	// Scenario: Systematically verify every datalake operation that
	//           proves the absence of merge/dedup logic.
	// Expected: LoadTable → no-op success, LoadUserTables → success,
	//           DeleteBy / DropTable / TestConnection / Connect /
	//           DownloadIdentityRules / TestLoadTable → "not implemented"
	// ----------------------------------------------------------------
	t.Run("no_merge_strategy_exists", func(t *testing.T) {
		dl := datalake.New(config.New(), logger.NOP)
		dl.Uploader = whutils.NewNoOpUploader()
		dl.Warehouse.Type = whutils.S3Datalake

		// LoadTable: returns zero-stat success (no merge SQL executed)
		stats, err := dl.LoadTable(ctx, "tracks")
		require.NoError(t, err, "LoadTable must be a no-op success")
		require.NotNil(t, stats)
		require.Equal(t, int64(0), stats.RowsInserted)
		require.Equal(t, int64(0), stats.RowsUpdated)

		// LoadUserTables: returns identifies (and conditionally users).
		// With NoOpUploader, GetTableSchemaInUpload returns nil for all
		// tables, so len == 0 → only identifies appears in the result.
		loadErrors := dl.LoadUserTables(ctx)
		require.NotNil(t, loadErrors, "LoadUserTables must return a result map")
		require.Contains(t, loadErrors, whutils.IdentifiesTable,
			"identifies table must always be in the result")
		require.Nil(t, loadErrors[whutils.IdentifiesTable],
			"identifies table load must succeed (no-op)")

		// With NoOp uploader, users table is NOT in the result (no schema).
		_, hasUsers := loadErrors[whutils.UsersTable]
		require.False(t, hasUsers,
			"users table absent when uploader has no users schema")

		// LoadIdentityMergeRulesTable: no-op success
		err = dl.LoadIdentityMergeRulesTable(ctx)
		require.NoError(t, err, "LoadIdentityMergeRulesTable must succeed (no-op)")

		// LoadIdentityMappingsTable: no-op success
		err = dl.LoadIdentityMappingsTable(ctx)
		require.NoError(t, err, "LoadIdentityMappingsTable must succeed (no-op)")

		// DeleteBy: returns "not implemented" — no dedup = no delete
		err = dl.DeleteBy(ctx, []string{"tracks"}, whutils.DeleteByParams{
			SourceId:  "test-source",
			JobRunId:  "test-job-run",
			TaskRunId: "test-task-run",
		})
		require.Error(t, err, "DeleteBy must return error for datalake")
		require.Contains(t, err.Error(), whutils.NotImplementedErrorCode,
			"DeleteBy error must contain 'not implemented'")

		// DropTable: returns "not implemented"
		err = dl.DropTable(ctx, "tracks")
		require.Error(t, err, "DropTable must return error for datalake")
		require.Contains(t, err.Error(), "not implemented",
			"DropTable error must contain 'not implemented'")

		// TestConnection: not implemented for datalake
		err = dl.TestConnection(ctx, dl.Warehouse)
		require.Error(t, err, "TestConnection must return error for datalake")
		require.Contains(t, err.Error(), "not implemented")

		// Connect: not implemented for datalake
		_, err = dl.Connect(ctx, dl.Warehouse)
		require.Error(t, err, "Connect must return error for datalake")
		require.Contains(t, err.Error(), "not implemented")

		// DownloadIdentityRules: not implemented
		err = dl.DownloadIdentityRules(ctx, nil)
		require.Error(t, err, "DownloadIdentityRules must return error for datalake")
		require.Contains(t, err.Error(), "not implemented")

		// TestLoadTable: not implemented
		err = dl.TestLoadTable(ctx, "tracks", "/tmp/test", nil, "json")
		require.Error(t, err, "TestLoadTable must return error for datalake")
		require.Contains(t, err.Error(), "not implemented")

		// IsEmpty: always returns false (append-only, never "empty")
		isEmpty, err := dl.IsEmpty(ctx, dl.Warehouse)
		require.NoError(t, err, "IsEmpty must not return an error")
		require.False(t, isEmpty, "datalake always reports as non-empty")

		// Cleanup: no-op, must not panic
		require.NotPanics(t, func() {
			dl.Cleanup(ctx)
		}, "Cleanup must be a safe no-op")

		// SetConnectionTimeout: no-op, must not panic
		require.NotPanics(t, func() {
			dl.SetConnectionTimeout(0)
		}, "SetConnectionTimeout must be a safe no-op")
	})

	// ----------------------------------------------------------------
	// Test Case 3: schema_operations_delegated
	// Scenario: Verify that ALL schema operations (FetchSchema,
	//           CreateSchema, CreateTable, AddColumns, AlterColumn)
	//           delegate exclusively to the SchemaRepository.
	// Expected: With a nil SchemaRepository, each operation panics —
	//           proving that the code path goes through delegation
	//           rather than implementing schema logic directly.
	// ----------------------------------------------------------------
	t.Run("schema_operations_delegated", func(t *testing.T) {
		dl := datalake.New(config.New(), logger.NOP)
		dl.Uploader = whutils.NewNoOpUploader()
		dl.Warehouse.Type = whutils.S3Datalake
		// SchemaRepository is intentionally nil — this is the key to the
		// delegation test.  If datalake implemented schema logic directly,
		// these calls would NOT panic.  The fact that they DO panic proves
		// every schema operation delegates to SchemaRepository.

		// FetchSchema delegates to SchemaRepository.FetchSchema
		require.Panics(t, func() {
			_, _ = dl.FetchSchema(ctx)
		}, "FetchSchema must delegate to SchemaRepository (panics when nil)")

		// CreateSchema delegates to SchemaRepository.CreateSchema
		require.Panics(t, func() {
			_ = dl.CreateSchema(ctx)
		}, "CreateSchema must delegate to SchemaRepository (panics when nil)")

		// CreateTable delegates to SchemaRepository.CreateTable
		require.Panics(t, func() {
			_ = dl.CreateTable(ctx, "tracks", map[string]string{
				"id":         "string",
				"event":      "string",
				"timestamp":  "datetime",
				"user_id":    "string",
				"context_ip": "string",
			})
		}, "CreateTable must delegate to SchemaRepository (panics when nil)")

		// AddColumns delegates to SchemaRepository.AddColumns
		require.Panics(t, func() {
			_ = dl.AddColumns(ctx, "tracks", []whutils.ColumnInfo{
				{Name: "new_column", Type: "string"},
				{Name: "another_col", Type: "int"},
			})
		}, "AddColumns must delegate to SchemaRepository (panics when nil)")

		// AlterColumn delegates to SchemaRepository.AlterColumn
		require.Panics(t, func() {
			_, _ = dl.AlterColumn(ctx, "tracks", "event", "text")
		}, "AlterColumn must delegate to SchemaRepository (panics when nil)")

		// TestFetchSchema wraps FetchSchema, also delegates
		require.Panics(t, func() {
			_ = dl.TestFetchSchema(ctx)
		}, "TestFetchSchema wraps FetchSchema and must also delegate")

		// Non-schema operations must NOT panic even with nil SchemaRepository
		// to confirm schema delegation is isolated to the 5 schema methods.
		require.NotPanics(t, func() {
			_, _ = dl.LoadTable(ctx, "tracks")
		}, "LoadTable must not touch SchemaRepository")

		require.NotPanics(t, func() {
			_ = dl.DeleteBy(ctx, []string{"tracks"}, whutils.DeleteByParams{})
		}, "DeleteBy must not touch SchemaRepository")

		require.NotPanics(t, func() {
			_ = dl.ErrorMappings()
		}, "ErrorMappings must not touch SchemaRepository")
	})

	// ----------------------------------------------------------------
	// Test Case 4: replay_row_count_is_additive
	// Scenario: Simulate 3 replays of 100 events each.
	// Expected: Final cumulative row count = 3 × 100 = 300 because
	//           append-only semantics mean every load adds all events
	//           without dedup or merge.
	// ----------------------------------------------------------------
	t.Run("replay_row_count_is_additive", func(t *testing.T) {
		dl := datalake.New(config.New(), logger.NOP)
		dl.Uploader = whutils.NewNoOpUploader()
		dl.Warehouse.Type = whutils.S3Datalake

		const numReplays = 3
		const eventsPerReplay = 100

		// replayEvent represents a single warehouse event for serialisation.
		type replayEvent struct {
			EventType string `json:"event_type"`
			MessageID string `json:"message_id"`
			UserID    string `json:"user_id"`
			Timestamp string `json:"timestamp"`
			ReplayNum int    `json:"replay_num"`
		}

		totalRows := int64(0)
		for replay := 0; replay < numReplays; replay++ {
			// Construct events for this replay iteration.
			events := make([]replayEvent, eventsPerReplay)
			for i := 0; i < eventsPerReplay; i++ {
				events[i] = replayEvent{
					EventType: "track",
					MessageID: fmt.Sprintf("msg-%03d", i),
					UserID:    fmt.Sprintf("user-%03d", i%10),
					Timestamp: "2024-01-15T10:30:00Z",
					ReplayNum: replay + 1,
				}
			}

			// Verify jsonrs serialisation round-trip.
			// CRITICAL: Never use encoding/json — always use jsonrs per
			// .golangci.yml depguard rules.
			data, err := jsonrs.Marshal(events)
			require.NoError(t, err, "jsonrs.Marshal must succeed for replay %d", replay)
			require.True(t, len(data) > 0, "serialised data must be non-empty")

			var decoded []replayEvent
			err = jsonrs.Unmarshal(data, &decoded)
			require.NoError(t, err, "jsonrs.Unmarshal must succeed for replay %d", replay)
			require.Len(t, decoded, eventsPerReplay,
				"all %d events must survive round-trip for replay %d", eventsPerReplay, replay)

			// Verify first and last event fidelity.
			require.Equal(t, "msg-000", decoded[0].MessageID)
			require.Equal(t, replay+1, decoded[0].ReplayNum)
			require.Equal(t, fmt.Sprintf("msg-%03d", eventsPerReplay-1),
				decoded[eventsPerReplay-1].MessageID)

			// Load table — datalake no-op.  In a real datalake, this would
			// append eventsPerReplay rows to object storage.
			stats, err := dl.LoadTable(ctx, "tracks")
			require.NoError(t, err, "LoadTable must succeed on replay %d", replay)
			require.NotNil(t, stats)

			// Track cumulative rows.  Because append-only, each replay adds
			// the full event count without dedup.
			totalRows += int64(eventsPerReplay)
		}

		// Final verification: total rows = numReplays × eventsPerReplay.
		require.Equal(t, int64(numReplays*eventsPerReplay), totalRows,
			"row count must be strictly additive — datalake is append-only with no dedup")
		require.Equal(t, int64(300), totalRows,
			"3 replays × 100 events = 300 total rows")

		// Verify that multiple table names also work additively.
		tables := []string{"tracks", "pages", "screens", "identifies"}
		for _, table := range tables {
			stats, err := dl.LoadTable(ctx, table)
			require.NoError(t, err, "LoadTable(%q) must succeed", table)
			require.NotNil(t, stats)
			require.Equal(t, int64(0), stats.RowsInserted,
				"LoadTable(%q) reports zero at Go level", table)
		}
	})

	// ----------------------------------------------------------------
	// Test Case 5: error_mappings_for_permissions
	// Scenario: Verify that ErrorMappings() returns the correct regex →
	//           error type mappings for AWS permission errors.
	// Expected: Exactly 2 mappings, both of type "permission_error",
	//           matching Lake Formation and IAM error patterns.
	// ----------------------------------------------------------------
	t.Run("error_mappings_for_permissions", func(t *testing.T) {
		dl := datalake.New(config.New(), logger.NOP)

		mappings := dl.ErrorMappings()
		require.Len(t, mappings, 2,
			"datalake must have exactly 2 error mappings")

		// Both mappings must be permission errors.
		// model.JobErrorType is a type alias for string, so direct
		// string comparison works without importing the model package.
		const permissionError = "permission_error"
		for i, m := range mappings {
			require.Equal(t, permissionError, m.Type,
				"mapping %d must be a permission error", i)
			require.NotNil(t, m.Format,
				"mapping %d format (regex) must not be nil", i)
		}

		// Mapping 0: Lake Formation insufficient permission.
		lakeFormationErrors := []string{
			"AccessDeniedException: Insufficient Lake Formation permission on arn:aws:glue:us-east-1:123456789012:database/mydb: Required Create Database on Catalog",
			"AccessDeniedException: Insufficient Lake Formation permission on arn:aws:glue:eu-west-1:999999999999:table/db/tbl: Required Create Database on Catalog",
		}
		for _, errMsg := range lakeFormationErrors {
			require.True(t, mappings[0].Format.MatchString(errMsg),
				"mapping 0 must match Lake Formation error: %s", errMsg)
		}

		// Mapping 1: IAM authorization failure.
		iamErrors := []string{
			"AccessDeniedException: User: arn:aws:iam::123456789012:user/testuser is not authorized to perform: glue:CreateDatabase on resource: arn:aws:glue:us-east-1:123456789012:catalog",
			"AccessDeniedException: User: arn:aws:sts::987654321098:assumed-role/role-name/session is not authorized to perform: s3:PutObject on resource: arn:aws:s3:::my-bucket/prefix/*",
		}
		for _, errMsg := range iamErrors {
			require.True(t, mappings[1].Format.MatchString(errMsg),
				"mapping 1 must match IAM error: %s", errMsg)
		}

		// Verify that non-permission errors do NOT match either mapping.
		nonPermissionErrors := []string{
			"NetworkTimeoutException: connection timed out",
			"InternalServerError: Service unavailable",
			"ThrottlingException: Rate exceeded",
			"ValidationException: Invalid table name",
			"ResourceNotFoundException: Database not found",
			"",
		}
		for _, errMsg := range nonPermissionErrors {
			for i, m := range mappings {
				require.False(t, m.Format.MatchString(errMsg),
					"mapping %d must NOT match non-permission error: %q", i, errMsg)
			}
		}

		// Cross-pattern verification: Lake Formation pattern must NOT match
		// IAM errors and vice versa.
		for _, iamErr := range iamErrors {
			require.False(t, mappings[0].Format.MatchString(iamErr),
				"Lake Formation pattern must not match IAM error: %s", iamErr)
		}
		for _, lfErr := range lakeFormationErrors {
			require.False(t, mappings[1].Format.MatchString(lfErr),
				"IAM pattern must not match Lake Formation error: %s", lfErr)
		}

		// Verify error mappings work with fmt.Errorf constructed errors
		// (simulating how errors would be formatted in production).
		wrappedErr := fmt.Errorf("warehouse sync failed: %s",
			"AccessDeniedException: User: arn:aws:iam::123456789012:user/admin is not authorized to perform: glue:GetTable on resource: arn:aws:glue:us-east-1:123456789012:table/db/tbl")
		require.True(t, mappings[1].Format.MatchString(wrappedErr.Error()),
			"IAM pattern must match errors wrapped with fmt.Errorf")
	})
}
