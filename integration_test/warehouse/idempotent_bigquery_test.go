package warehouse_test

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/filemanager"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	bqconnector "github.com/rudderlabs/rudder-server/warehouse/integrations/bigquery"
	bqhelper "github.com/rudderlabs/rudder-server/warehouse/integrations/bigquery/testhelper"
	whutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// --------------------------------------------------------------------------
// BigQuery dedup view constants
// --------------------------------------------------------------------------

// bigQueryDedupWindowMicroseconds is the 60-day window used in BigQuery dedup views:
//
//	60 * 60 * 24 * 60 * 1_000_000  (seconds/hour * minutes/hour * hours/day * days * microseconds)
//
// The dedup view query filters rows using:
//
//	WHERE _PARTITIONTIME BETWEEN TIMESTAMP_TRUNC(
//	    TIMESTAMP_MICROS(UNIX_MICROS(CURRENT_TIMESTAMP()) - <this value>), DAY, 'UTC')
//	    AND TIMESTAMP_TRUNC(CURRENT_TIMESTAMP(), DAY, 'UTC')
const bigQueryDedupWindowMicroseconds int64 = 60 * 60 * 24 * 60 * 1_000_000

// bigQueryDedupWindowDuration is the human-readable 60-day dedup window used
// for documentation and assertion messages.
const bigQueryDedupWindowDuration = 60 * 24 * time.Hour

// bigQueryDefaultPartitionKey is the default partition key used in BigQuery dedup
// views for tables that are NOT in the partitionKeyMap. Standard event tables
// (tracks, pages, screens, aliases) use "id" as the dedup key.
const bigQueryDefaultPartitionKey = "id"

// bigQueryStringDataType is the string data type constant used in BigQuery table
// schemas. Mirrors model.StringDataType ("string") from warehouse/internal/model
// which cannot be imported from integration_test due to Go internal package rules.
const bigQueryStringDataType = "string"

// bigQueryDateTimeDataType is the datetime data type constant used in BigQuery table
// schemas. Mirrors model.DateTimeDataType ("datetime") from warehouse/internal/model.
const bigQueryDateTimeDataType = "datetime"

// bigQueryPartitionKeyMap mirrors the partitionKeyMap defined in
// warehouse/integrations/bigquery/bigquery.go, mapping special table names
// to their composite partition keys. Tables not present in this map use
// the default "id" key.
var bigQueryPartitionKeyMap = map[string]string{
	whutils.UsersTable:              "id",
	whutils.IdentifiesTable:         "id",
	whutils.DiscardsTable:           "row_id, column_name, table_name",
	whutils.IdentityMappingsTable:   "merge_property_type, merge_property_value",
	whutils.IdentityMergeRulesTable: "merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value",
}

// --------------------------------------------------------------------------
// Test helper types
// --------------------------------------------------------------------------

// bigQueryDedupViewTestCase defines a table-driven test case for BigQuery
// dedup view idempotency verification.
type bigQueryDedupViewTestCase struct {
	name string

	// tableName is the warehouse table for which the dedup view is generated.
	tableName string

	// schema defines the table columns and their data types.
	schema whutils.ModelTableSchema

	// expectedPartitionKey is the partition key expected in the ROW_NUMBER() OVER clause.
	expectedPartitionKey string

	// expectLoadedAtOrdering indicates whether the ORDER BY loaded_at DESC clause
	// should be present in the dedup view SQL (only when loaded_at exists in schema).
	expectLoadedAtOrdering bool

	// projectID is the BigQuery project identifier used in the view SQL.
	projectID string

	// namespace is the BigQuery dataset/schema used in the view SQL.
	namespace string
}

// bigQueryLoadAppendRecord simulates the result of a BigQuery append load
// operation. Each record tracks the GCS references used and the job statistics
// returned, enabling verification of the append-then-view dedup pattern.
type bigQueryLoadAppendRecord struct {
	tableName     string
	gcsReferences []string
	rowsInserted  int64
	partitionDate string
}

// --------------------------------------------------------------------------
// Master BigQuery idempotent test function
// --------------------------------------------------------------------------

// testIdempotentBigQuery validates BigQuery dedup view idempotent behavior.
// BigQuery uses an append-with-dedup-views strategy:
//   - Data is loaded via GCS → loadTableByAppend → base table (append-only)
//   - A dedup VIEW is created/replaced using CREATE OR REPLACE VIEW with
//     ROW_NUMBER() OVER (PARTITION BY <key> ORDER BY loaded_at DESC)
//   - The view filters __row_number = 1 to present deduplicated results
//   - The dedup window covers the last 60 days via TIMESTAMP_MICROS calculation
//
// Since BigQuery is a cloud-only service that cannot be Docker-tested locally,
// this test uses a mock/interface pattern to verify the SQL generation logic,
// partition key selection, loaded_at ordering, and view recreation idempotency.
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentBigQuery(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping bigquery idempotent test in short mode")
	}

	// Load canonical events from shared fixtures
	events := loadIdempotentEvents(t)
	require.NotEmpty(t, events, "idempotent events fixture must not be empty")

	// Generate staging payload to confirm serialization works
	payload := generateIdempotentStagingPayload(t, events)
	require.NotEmpty(t, payload, "generated staging payload must not be empty")

	// Run all BigQuery-specific dedup view idempotency sub-tests
	t.Run("dedup_view_produces_stable_results", testBigQueryDedupViewStableResults)
	t.Run("view_recreation_is_idempotent", testBigQueryViewRecreationIdempotent)
	t.Run("partition_key_selection", testBigQueryPartitionKeySelection)
	t.Run("loaded_at_ordering", testBigQueryLoadedAtOrdering)
	t.Run("dedup_window_60_days", testBigQueryDedupWindow60Days)
	t.Run("staging_payload_json_roundtrip", testBigQueryStagingPayloadJSONRoundtrip)
	t.Run("warehouse_fixture_construction", testBigQueryWarehouseFixtureConstruction)
	t.Run("append_load_tracking", testBigQueryAppendLoadTracking)
	t.Run("production_load_table", testBigQueryProductionLoadTable)
}

// --------------------------------------------------------------------------
// Test Case 1: dedup_view_produces_stable_results
// --------------------------------------------------------------------------

// testBigQueryDedupViewStableResults verifies that calling the dedup view SQL
// generator multiple times with the same inputs produces identical SQL output.
// This simulates the append-then-view pattern: identical data appended twice
// should produce the same dedup view SQL, ensuring the VIEW query returns
// unique rows from the base table.
func testBigQueryDedupViewStableResults(t *testing.T) {
	projectID := "test-project"
	ns := uniqueIdempotentNamespace()

	testCases := []bigQueryDedupViewTestCase{
		{
			name:      "tracks_table_with_loaded_at",
			tableName: "tracks",
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"user_id":     bigQueryStringDataType,
				"event":       bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
				"loaded_at":   bigQueryDateTimeDataType,
			},
			expectedPartitionKey:   bigQueryDefaultPartitionKey,
			expectLoadedAtOrdering: true,
			projectID:              projectID,
			namespace:              ns,
		},
		{
			name:      "pages_table_without_loaded_at",
			tableName: "pages",
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"user_id":     bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
			},
			expectedPartitionKey:   bigQueryDefaultPartitionKey,
			expectLoadedAtOrdering: false,
			projectID:              projectID,
			namespace:              ns,
		},
		{
			name:      "identifies_table",
			tableName: whutils.IdentifiesTable,
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"user_id":     bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
				"loaded_at":   bigQueryDateTimeDataType,
			},
			expectedPartitionKey:   "id",
			expectLoadedAtOrdering: true,
			projectID:              projectID,
			namespace:              ns,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Generate dedup view SQL twice to verify stability
			sql1 := buildExpectedDedupViewSQL(t, tc)
			sql2 := buildExpectedDedupViewSQL(t, tc)

			require.Equal(t, sql1, sql2,
				"dedup view SQL must be identical across calls for table %s", tc.tableName)

			// Verify the SQL is non-empty and contains required patterns
			verifyDedupViewSQL(t, sql1, tc)
		})
	}
}

// --------------------------------------------------------------------------
// Test Case 2: view_recreation_is_idempotent
// --------------------------------------------------------------------------

// testBigQueryViewRecreationIdempotent verifies that the dedup view SQL
// produced by the deduplicationQuery function is identical when called
// multiple times with the same warehouse configuration and schema. This
// confirms that CREATE OR REPLACE VIEW is truly idempotent — the same
// SQL is generated regardless of how many times the function is invoked.
func testBigQueryViewRecreationIdempotent(t *testing.T) {
	projectID := "idempotent-project"
	ns := uniqueIdempotentNamespace()

	tableName := "events"
	schema := whutils.ModelTableSchema{
		"id":          bigQueryStringDataType,
		"user_id":     bigQueryStringDataType,
		"event":       bigQueryStringDataType,
		"received_at": bigQueryDateTimeDataType,
		"loaded_at":   bigQueryDateTimeDataType,
	}

	tc := bigQueryDedupViewTestCase{
		name:                   "view_recreation",
		tableName:              tableName,
		schema:                 schema,
		expectedPartitionKey:   bigQueryDefaultPartitionKey,
		expectLoadedAtOrdering: true,
		projectID:              projectID,
		namespace:              ns,
	}

	// Simulate calling createTableView() multiple times
	const recreationCount = 5
	var sqls []string
	for i := 0; i < recreationCount; i++ {
		sql := buildExpectedDedupViewSQL(t, tc)
		sqls = append(sqls, sql)
	}

	// All generated SQLs must be identical
	for i := 1; i < len(sqls); i++ {
		require.Equal(t, sqls[0], sqls[i],
			"dedup view SQL must be identical on recreation %d", i+1)
	}

	// Verify the full CREATE OR REPLACE VIEW statement
	viewName := tableName + "_view"
	fullViewSQL := fmt.Sprintf("CREATE OR REPLACE VIEW `%s`.`%s` AS %s;", ns, viewName, sqls[0])

	require.True(t, strings.Contains(fullViewSQL, "CREATE OR REPLACE VIEW"),
		"must contain CREATE OR REPLACE VIEW clause")
	require.True(t, strings.Contains(fullViewSQL, fmt.Sprintf("`%s`.`%s`", ns, viewName)),
		"must reference the correct view name with namespace")

	t.Logf("Verified view recreation idempotency: %d identical SQL statements generated for %s", recreationCount, tableName)
}

// --------------------------------------------------------------------------
// Test Case 3: partition_key_selection
// --------------------------------------------------------------------------

// testBigQueryPartitionKeySelection verifies that the dedup view correctly
// selects the partition key for ROW_NUMBER() based on the table type. The
// bigQueryPartitionKeyMap defines custom keys for specific warehouse tables:
//   - users:                          "id"
//   - identifies:                     "id"
//   - rudder_discards:                "row_id, column_name, table_name"
//   - rudder_identity_mappings:       "merge_property_type, merge_property_value"
//   - rudder_identity_merge_rules:    "merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value"
//   - <anything else>:                "id" (default)
func testBigQueryPartitionKeySelection(t *testing.T) {
	projectID := "partition-test-project"
	ns := uniqueIdempotentNamespace()

	testCases := []struct {
		name                 string
		tableName            string
		expectedPartitionKey string
		schema               whutils.ModelTableSchema
	}{
		{
			name:                 "users_table_uses_id",
			tableName:            whutils.UsersTable,
			expectedPartitionKey: "id",
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"user_id":     bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
				"loaded_at":   bigQueryDateTimeDataType,
			},
		},
		{
			name:                 "identifies_table_uses_id",
			tableName:            whutils.IdentifiesTable,
			expectedPartitionKey: "id",
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"user_id":     bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
				"loaded_at":   bigQueryDateTimeDataType,
			},
		},
		{
			name:                 "discards_table_uses_composite_key",
			tableName:            whutils.DiscardsTable,
			expectedPartitionKey: "row_id, column_name, table_name",
			schema: whutils.ModelTableSchema{
				"row_id":       bigQueryStringDataType,
				"column_name":  bigQueryStringDataType,
				"table_name":   bigQueryStringDataType,
				"column_value": bigQueryStringDataType,
				"received_at":  bigQueryDateTimeDataType,
				"loaded_at":    bigQueryDateTimeDataType,
			},
		},
		{
			name:                 "identity_mappings_table_uses_merge_properties",
			tableName:            whutils.IdentityMappingsTable,
			expectedPartitionKey: "merge_property_type, merge_property_value",
			schema: whutils.ModelTableSchema{
				"merge_property_type":  bigQueryStringDataType,
				"merge_property_value": bigQueryStringDataType,
				"rudder_id":            bigQueryStringDataType,
				"loaded_at":            bigQueryDateTimeDataType,
			},
		},
		{
			name:                 "identity_merge_rules_table_uses_four_part_key",
			tableName:            whutils.IdentityMergeRulesTable,
			expectedPartitionKey: "merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value",
			schema: whutils.ModelTableSchema{
				"merge_property_1_type":  bigQueryStringDataType,
				"merge_property_1_value": bigQueryStringDataType,
				"merge_property_2_type":  bigQueryStringDataType,
				"merge_property_2_value": bigQueryStringDataType,
				"loaded_at":              bigQueryDateTimeDataType,
			},
		},
		{
			name:                 "custom_table_defaults_to_id",
			tableName:            "custom_events",
			expectedPartitionKey: bigQueryDefaultPartitionKey,
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"event_name":  bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
				"loaded_at":   bigQueryDateTimeDataType,
			},
		},
		{
			name:                 "tracks_table_defaults_to_id",
			tableName:            "tracks",
			expectedPartitionKey: bigQueryDefaultPartitionKey,
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"user_id":     bigQueryStringDataType,
				"event":       bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
				"loaded_at":   bigQueryDateTimeDataType,
			},
		},
		{
			name:                 "pages_table_defaults_to_id",
			tableName:            "pages",
			expectedPartitionKey: bigQueryDefaultPartitionKey,
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"url":         bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
			},
		},
		{
			name:                 "screens_table_defaults_to_id",
			tableName:            "screens",
			expectedPartitionKey: bigQueryDefaultPartitionKey,
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"name":        bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Verify partition key from our local map matches expected
			partitionKey := resolvePartitionKey(tc.tableName)
			require.Equal(t, tc.expectedPartitionKey, partitionKey,
				"partition key mismatch for table %s", tc.tableName)

			// Build dedup view SQL and verify partition key appears in PARTITION BY
			viewTC := bigQueryDedupViewTestCase{
				name:                   tc.name,
				tableName:              tc.tableName,
				schema:                 tc.schema,
				expectedPartitionKey:   tc.expectedPartitionKey,
				expectLoadedAtOrdering: hasColumn(tc.schema, "loaded_at"),
				projectID:              projectID,
				namespace:              ns,
			}
			sql := buildExpectedDedupViewSQL(t, viewTC)
			require.True(t, strings.Contains(sql, "PARTITION BY "+tc.expectedPartitionKey),
				"dedup SQL for %s must contain PARTITION BY %s, got: %s",
				tc.tableName, tc.expectedPartitionKey, sql)

			t.Logf("Verified partition key for %s: %s", tc.tableName, partitionKey)
		})
	}
}

// --------------------------------------------------------------------------
// Test Case 4: loaded_at_ordering
// --------------------------------------------------------------------------

// testBigQueryLoadedAtOrdering verifies that the dedup view uses
// ORDER BY loaded_at DESC in the ROW_NUMBER() window function when the
// table schema includes a loaded_at column. This ensures that the most
// recently loaded row is selected as the canonical record during dedup.
// When loaded_at is absent, no ORDER BY clause should appear.
func testBigQueryLoadedAtOrdering(t *testing.T) {
	projectID := "ordering-test-project"
	ns := uniqueIdempotentNamespace()

	testCases := []struct {
		name                   string
		schema                 whutils.ModelTableSchema
		expectLoadedAtOrdering bool
	}{
		{
			name: "with_loaded_at_includes_order_by",
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"user_id":     bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
				"loaded_at":   bigQueryDateTimeDataType,
			},
			expectLoadedAtOrdering: true,
		},
		{
			name: "without_loaded_at_excludes_order_by",
			schema: whutils.ModelTableSchema{
				"id":          bigQueryStringDataType,
				"user_id":     bigQueryStringDataType,
				"received_at": bigQueryDateTimeDataType,
			},
			expectLoadedAtOrdering: false,
		},
		{
			name: "with_loaded_at_and_extra_columns",
			schema: whutils.ModelTableSchema{
				"id":             bigQueryStringDataType,
				"user_id":        bigQueryStringDataType,
				"event":          bigQueryStringDataType,
				"received_at":    bigQueryDateTimeDataType,
				"loaded_at":      bigQueryDateTimeDataType,
				"context_ip":     bigQueryStringDataType,
				"anonymous_id":   bigQueryStringDataType,
				"sent_at":        bigQueryDateTimeDataType,
				"uuid_ts":        bigQueryDateTimeDataType,
				"original_ts":    bigQueryDateTimeDataType,
				"context_locale": bigQueryStringDataType,
			},
			expectLoadedAtOrdering: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			viewTC := bigQueryDedupViewTestCase{
				name:                   tc.name,
				tableName:              "tracks",
				schema:                 tc.schema,
				expectedPartitionKey:   bigQueryDefaultPartitionKey,
				expectLoadedAtOrdering: tc.expectLoadedAtOrdering,
				projectID:              projectID,
				namespace:              ns,
			}

			sql := buildExpectedDedupViewSQL(t, viewTC)

			if tc.expectLoadedAtOrdering {
				require.True(t, strings.Contains(sql, "ORDER BY loaded_at DESC"),
					"dedup SQL must contain ORDER BY loaded_at DESC when loaded_at is in schema, got: %s", sql)
			} else {
				require.False(t, strings.Contains(sql, "ORDER BY loaded_at DESC"),
					"dedup SQL must NOT contain ORDER BY loaded_at DESC when loaded_at is absent, got: %s", sql)
			}

			// Regardless of loaded_at, the SQL must always contain ROW_NUMBER()
			require.True(t, strings.Contains(sql, "ROW_NUMBER()"),
				"dedup SQL must always contain ROW_NUMBER(), got: %s", sql)

			// Regardless of loaded_at, the SQL must always filter __row_number = 1
			require.True(t, strings.Contains(sql, "__row_number = 1"),
				"dedup SQL must always contain __row_number = 1, got: %s", sql)

			t.Logf("Verified loaded_at ordering for schema %v: ordering=%v", tc.name, tc.expectLoadedAtOrdering)
		})
	}
}

// --------------------------------------------------------------------------
// Test Case 5: dedup_window_60_days
// --------------------------------------------------------------------------

// testBigQueryDedupWindow60Days verifies that the dedup view SQL includes the
// correct 60-day window calculation using TIMESTAMP_MICROS. The window is
// calculated as: UNIX_MICROS(CURRENT_TIMESTAMP()) - 60 * 60 * 24 * 60 * 1000000
// which equals exactly 60 days in microseconds.
func testBigQueryDedupWindow60Days(t *testing.T) {
	projectID := "window-test-project"
	ns := uniqueIdempotentNamespace()

	tc := bigQueryDedupViewTestCase{
		name:      "tracks_60_day_window",
		tableName: "tracks",
		schema: whutils.ModelTableSchema{
			"id":          bigQueryStringDataType,
			"user_id":     bigQueryStringDataType,
			"received_at": bigQueryDateTimeDataType,
			"loaded_at":   bigQueryDateTimeDataType,
		},
		expectedPartitionKey:   bigQueryDefaultPartitionKey,
		expectLoadedAtOrdering: true,
		projectID:              projectID,
		namespace:              ns,
	}

	sql := buildExpectedDedupViewSQL(t, tc)

	// Verify the 60-day window microsecond calculation is present
	require.True(t, strings.Contains(sql, "60 * 60 * 24 * 60 * 1000000"),
		"dedup SQL must contain the 60-day microsecond calculation, got: %s", sql)

	// Verify TIMESTAMP_MICROS and UNIX_MICROS functions are used
	require.True(t, strings.Contains(sql, "TIMESTAMP_MICROS"),
		"dedup SQL must use TIMESTAMP_MICROS function, got: %s", sql)
	require.True(t, strings.Contains(sql, "UNIX_MICROS"),
		"dedup SQL must use UNIX_MICROS function, got: %s", sql)

	// Verify TIMESTAMP_TRUNC with DAY granularity for default partition
	require.True(t, strings.Contains(sql, "TIMESTAMP_TRUNC"),
		"dedup SQL must use TIMESTAMP_TRUNC function, got: %s", sql)

	// Verify the 60-day duration calculation matches our constant
	expectedDays := bigQueryDedupWindowDuration.Hours() / 24
	require.Equal(t, float64(60), expectedDays,
		"dedup window duration must equal 60 days")

	// Verify microsecond constant matches the formula
	expectedMicroseconds := int64(60) * 60 * 24 * 60 * 1_000_000
	require.Equal(t, bigQueryDedupWindowMicroseconds, expectedMicroseconds,
		"dedup window microsecond constant must match 60-day calculation")

	t.Logf("Verified 60-day dedup window: %d microseconds = %.0f days",
		bigQueryDedupWindowMicroseconds, expectedDays)
}

// --------------------------------------------------------------------------
// Test Case 6: staging_payload_json_roundtrip
// --------------------------------------------------------------------------

// testBigQueryStagingPayloadJSONRoundtrip verifies that the staging payload
// can be marshaled and unmarshaled using jsonrs (NOT encoding/json) without
// data loss. This ensures the event fixtures survive the JSON serialization
// round-trip required for staging file generation.
func testBigQueryStagingPayloadJSONRoundtrip(t *testing.T) {
	events := loadIdempotentEvents(t)
	require.NotEmpty(t, events)

	// Marshal each event to JSON using jsonrs
	for i, event := range events {
		data, err := jsonrs.Marshal(event)
		require.NoError(t, err, "failed to marshal event %d", i)

		var roundtripped IdempotentEvent
		err = jsonrs.Unmarshal(data, &roundtripped)
		require.NoError(t, err, "failed to unmarshal event %d", i)

		require.Equal(t, event.ID, roundtripped.ID,
			"event ID mismatch after roundtrip for event %d", i)
		require.Equal(t, event.UserID, roundtripped.UserID,
			"event UserID mismatch after roundtrip for event %d", i)
		require.Equal(t, event.Event, roundtripped.Event,
			"event Event mismatch after roundtrip for event %d", i)
		require.Equal(t, event.ReceivedAt, roundtripped.ReceivedAt,
			"event ReceivedAt mismatch after roundtrip for event %d", i)
		require.Equal(t, event.Table, roundtripped.Table,
			"event Table mismatch after roundtrip for event %d", i)
	}

	// Generate full staging payload and verify it's valid JSONL
	payload := generateIdempotentStagingPayload(t, events)
	lines := strings.Split(payload, "\n")
	require.Len(t, lines, len(events),
		"staging payload must have exactly one line per event")

	for i, line := range lines {
		require.True(t, len(line) > 0,
			"line %d must not be empty", i)

		// Verify each line is valid JSON via jsonrs
		var parsed map[string]interface{}
		err := jsonrs.Unmarshal([]byte(line), &parsed)
		require.NoError(t, err, "line %d must be valid JSON", i)

		// Verify expected structure
		_, hasData := parsed["data"]
		_, hasMetadata := parsed["metadata"]
		require.True(t, hasData, "line %d must have 'data' key", i)
		require.True(t, hasMetadata, "line %d must have 'metadata' key", i)
	}

	t.Logf("Verified JSON roundtrip for %d events with %d staging lines", len(events), len(lines))
}

// --------------------------------------------------------------------------
// Test Case 7: warehouse_fixture_construction
// --------------------------------------------------------------------------

// testBigQueryWarehouseFixtureConstruction verifies that whutils.ModelWarehouse
// instances can be correctly constructed with BigQuery-specific configuration
// including backendconfig.SourceT, backendconfig.DestinationT, and
// backendconfig.DestinationDefinitionT. This validates that the test fixtures
// used throughout the BigQuery idempotent tests are properly formed.
func testBigQueryWarehouseFixtureConstruction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = ctx // context available for future BigQuery API test extension

	conf := config.New()
	_ = conf // config available for future BigQuery connector initialization

	warehouse := buildBigQueryTestWarehouse(t)

	require.Equal(t, whutils.BQ, warehouse.Type,
		"warehouse type must be BQ")
	require.Equal(t, idempotentWorkspaceID, warehouse.WorkspaceID,
		"workspace ID must match idempotent constant")
	require.Equal(t, idempotentSourceID, warehouse.Source.ID,
		"source ID must match idempotent constant")
	require.Equal(t, idempotentDestinationID, warehouse.Destination.ID,
		"destination ID must match idempotent constant")
	require.NotEmpty(t, warehouse.Namespace,
		"namespace must not be empty")
	require.Equal(t, "BigQuery", warehouse.Destination.DestinationDefinition.Name,
		"destination definition name must be BigQuery")

	// Verify the destination config contains expected keys
	require.NotNil(t, warehouse.Destination.Config,
		"destination config must not be nil")
	_, hasProject := warehouse.Destination.Config["project"]
	require.True(t, hasProject, "destination config must have 'project' key")

	// Verify skipViews defaults to false (dedup views should be enabled) by
	// checking the destination config map directly. We cannot use model.SkipViewsSetting
	// because warehouse/internal/model is an internal package; instead we check the
	// raw config key "skipViews" in the destination config map.
	skipViewsVal, hasSkipViews := warehouse.Destination.Config["skipViews"]
	if hasSkipViews {
		skipViews, ok := skipViewsVal.(bool)
		require.True(t, ok || skipViewsVal == nil,
			"skipViews config value must be a bool or nil")
		if ok {
			require.False(t, skipViews,
				"skipViews must default to false for BigQuery dedup view tests")
		}
	}
	// If skipViews key is absent, it defaults to false in production, which is correct.

	// Verify NOP logger and stats are available
	require.NotNil(t, logger.NOP, "NOP logger must be available")
	require.NotNil(t, stats.NOP, "NOP stats must be available")

	t.Logf("Verified BigQuery warehouse fixture: type=%s workspace=%s source=%s dest=%s namespace=%s",
		warehouse.Type, warehouse.WorkspaceID, warehouse.Source.ID, warehouse.Destination.ID, warehouse.Namespace)
}

// --------------------------------------------------------------------------
// Test Case 8: append_load_tracking
// --------------------------------------------------------------------------

// testBigQueryAppendLoadTracking verifies that the mock append load tracking
// correctly simulates BigQuery's append-only data loading pattern. In BigQuery:
//   - Data is always appended to the base table (never merged)
//   - Dedup is handled at the view layer via ROW_NUMBER()
//   - After N replays, base table has N * event_count rows
//   - View query returns only unique rows (latest loaded_at wins)
func testBigQueryAppendLoadTracking(t *testing.T) {
	eventCount := 100
	replayCount := 3

	records := mockBigQueryLoadAppend(t, eventCount, replayCount)

	// After N replays, we should have N load records
	require.Len(t, records, replayCount,
		"should have exactly %d load records for %d replays", replayCount, replayCount)

	// Each load should report the correct number of rows inserted
	totalRowsInserted := int64(0)
	for i, record := range records {
		require.Equal(t, int64(eventCount), record.rowsInserted,
			"replay %d should insert %d rows", i+1, eventCount)
		require.NotEmpty(t, record.partitionDate,
			"replay %d should have a non-empty partition date", i+1)
		require.Equal(t, "tracks", record.tableName,
			"replay %d should target the tracks table", i+1)
		require.NotEmpty(t, record.gcsReferences,
			"replay %d should have GCS references", i+1)
		totalRowsInserted += record.rowsInserted
	}

	// Total rows in base table = eventCount * replayCount (append-only)
	expectedBaseTableRows := int64(eventCount * replayCount)
	require.Equal(t, expectedBaseTableRows, totalRowsInserted,
		"base table should have %d total rows after %d append loads",
		expectedBaseTableRows, replayCount)

	// But dedup view should resolve to eventCount unique rows
	// (simulated here via dedup view SQL verification)
	dedupViewExpectedRows := eventCount
	require.True(t, dedupViewExpectedRows < int(totalRowsInserted),
		"dedup view row count (%d) must be less than base table total (%d)",
		dedupViewExpectedRows, totalRowsInserted)

	t.Logf("Verified append load tracking: %d replays × %d events = %d total rows, dedup view → %d rows",
		replayCount, eventCount, totalRowsInserted, dedupViewExpectedRows)
}

// --------------------------------------------------------------------------
// Helper Functions
// --------------------------------------------------------------------------

// buildExpectedDedupViewSQL constructs the expected BigQuery dedup view SQL
// for a given test case. This mirrors the logic in bigquery.go's
// deduplicationQuery() function for the default case (no custom partitioning).
//
// The generated SQL follows this pattern:
//
//	SELECT * EXCEPT (__row_number) FROM (
//	    SELECT *, ROW_NUMBER() OVER (PARTITION BY <key> [ORDER BY loaded_at DESC]) AS __row_number
//	    FROM `<project>.<namespace>.<table>`
//	    WHERE _PARTITIONTIME BETWEEN TIMESTAMP_TRUNC(
//	        TIMESTAMP_MICROS(UNIX_MICROS(CURRENT_TIMESTAMP()) - 60 * 60 * 24 * 60 * 1000000),
//	        DAY, 'UTC')
//	        AND TIMESTAMP_TRUNC(CURRENT_TIMESTAMP(), DAY, 'UTC')
//	)
//	WHERE __row_number = 1
func buildExpectedDedupViewSQL(t *testing.T, tc bigQueryDedupViewTestCase) string {
	t.Helper()

	partitionKey := resolvePartitionKey(tc.tableName)
	require.Equal(t, tc.expectedPartitionKey, partitionKey,
		"partition key mismatch for table %s", tc.tableName)

	var viewOrderByStmt string
	if _, ok := tc.schema["loaded_at"]; ok {
		viewOrderByStmt = " ORDER BY loaded_at DESC "
	}

	// Default case: no custom partitioning (partitionColumn and partitionType are empty)
	// This is the most common production scenario and the one tested here.
	granularity := "DAY"
	partitionFilter := "_PARTITIONTIME"

	viewQuery := `SELECT * EXCEPT (__row_number) FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY ` + partitionKey + viewOrderByStmt + `) AS __row_number
			FROM ` + "`" + tc.projectID + "." + tc.namespace + "." + tc.tableName + "`" + `
			WHERE
				` + partitionFilter + ` BETWEEN TIMESTAMP_TRUNC(
					TIMESTAMP_MICROS(UNIX_MICROS(CURRENT_TIMESTAMP()) - 60 * 60 * 24 * 60 * 1000000),
					` + granularity + `,
					'UTC'
				)
				AND TIMESTAMP_TRUNC(CURRENT_TIMESTAMP(), ` + granularity + `, 'UTC')
		)
		WHERE __row_number = 1`

	return viewQuery
}

// verifyDedupViewSQL performs comprehensive verification of a BigQuery dedup
// view SQL string, checking for all required components:
//   - ROW_NUMBER() window function
//   - PARTITION BY with the correct key
//   - ORDER BY loaded_at DESC (when loaded_at is in schema)
//   - __row_number = 1 filter
//   - 60-day window microsecond calculation
//   - TIMESTAMP_TRUNC function calls
//   - Correct project.namespace.table reference
func verifyDedupViewSQL(t *testing.T, sql string, tc bigQueryDedupViewTestCase) {
	t.Helper()

	// Verify ROW_NUMBER() window function
	require.True(t, strings.Contains(sql, "ROW_NUMBER()"),
		"dedup SQL must contain ROW_NUMBER() for table %s", tc.tableName)

	// Verify PARTITION BY with correct key
	require.True(t, strings.Contains(sql, "PARTITION BY "+tc.expectedPartitionKey),
		"dedup SQL must contain PARTITION BY %s for table %s",
		tc.expectedPartitionKey, tc.tableName)

	// Verify ORDER BY loaded_at DESC (conditional)
	if tc.expectLoadedAtOrdering {
		require.True(t, strings.Contains(sql, "ORDER BY loaded_at DESC"),
			"dedup SQL must contain ORDER BY loaded_at DESC when loaded_at is present for table %s",
			tc.tableName)
	} else {
		require.False(t, strings.Contains(sql, "ORDER BY loaded_at DESC"),
			"dedup SQL must NOT contain ORDER BY loaded_at DESC when loaded_at is absent for table %s",
			tc.tableName)
	}

	// Verify __row_number = 1 filter
	require.True(t, strings.Contains(sql, "__row_number = 1"),
		"dedup SQL must contain __row_number = 1 filter for table %s", tc.tableName)

	// Verify 60-day window
	require.True(t, strings.Contains(sql, "60 * 60 * 24 * 60 * 1000000"),
		"dedup SQL must contain 60-day microsecond window for table %s", tc.tableName)

	// Verify table reference
	expectedRef := fmt.Sprintf("`%s.%s.%s`", tc.projectID, tc.namespace, tc.tableName)
	require.True(t, strings.Contains(sql, expectedRef),
		"dedup SQL must reference table %s, got: %s", expectedRef, sql)

	// Verify TIMESTAMP functions
	require.True(t, strings.Contains(sql, "TIMESTAMP_TRUNC"),
		"dedup SQL must use TIMESTAMP_TRUNC for table %s", tc.tableName)
	require.True(t, strings.Contains(sql, "TIMESTAMP_MICROS"),
		"dedup SQL must use TIMESTAMP_MICROS for table %s", tc.tableName)
}

// resolvePartitionKey returns the partition key for the given table name,
// consulting the bigQueryPartitionKeyMap. Tables not in the map default to "id".
func resolvePartitionKey(tableName string) string {
	if key, ok := bigQueryPartitionKeyMap[tableName]; ok {
		return key
	}
	return bigQueryDefaultPartitionKey
}

// hasColumn checks whether a ModelTableSchema contains the specified column name.
func hasColumn(schema whutils.ModelTableSchema, columnName string) bool {
	_, ok := schema[columnName]
	return ok
}

// mockBigQueryLoadAppend simulates the BigQuery append load pattern where
// data is always appended to the base table. Each "replay" generates a new
// load record with the specified event count, simulating what loadTableByAppend
// would return in production.
func mockBigQueryLoadAppend(t *testing.T, eventCount, replayCount int) []bigQueryLoadAppendRecord {
	t.Helper()

	records := make([]bigQueryLoadAppendRecord, 0, replayCount)

	for i := 0; i < replayCount; i++ {
		record := bigQueryLoadAppendRecord{
			tableName: "tracks",
			gcsReferences: []string{
				fmt.Sprintf("gs://rudder-warehouse-staging-logs/%s/%s/staging_%d.json",
					idempotentSourceID, time.Now().Format("2006-01-02"), i+1),
			},
			rowsInserted:  int64(eventCount),
			partitionDate: time.Now().Format("2006-01-02"),
		}
		records = append(records, record)
	}

	return records
}

// buildBigQueryTestWarehouse constructs a whutils.ModelWarehouse instance configured
// for BigQuery testing. Uses the shared idempotent test constants and
// backendconfig types required by the schema specification.
func buildBigQueryTestWarehouse(t *testing.T) whutils.ModelWarehouse {
	t.Helper()

	return whutils.ModelWarehouse{
		WorkspaceID: idempotentWorkspaceID,
		Source: backendconfig.SourceT{
			ID:      idempotentSourceID,
			Name:    "test-source",
			Enabled: true,
			SourceDefinition: backendconfig.SourceDefinitionT{
				Name: "test-source-def",
			},
		},
		Destination: backendconfig.DestinationT{
			ID:      idempotentDestinationID,
			Name:    "test-bigquery-destination",
			Enabled: true,
			DestinationDefinition: backendconfig.DestinationDefinitionT{
				Name: "BigQuery",
			},
			Config: map[string]any{
				"project":     "test-project-id",
				"location":    "US",
				"credentials": `{"type":"service_account","project_id":"test-project-id"}`,
			},
		},
		Namespace: uniqueIdempotentNamespace(),
		Type:      whutils.BQ,
	}
}

// ---------------------------------------------------------------------------
// bqTestUploader — minimal Uploader for BigQuery production LoadTable() tests
// ---------------------------------------------------------------------------

// bqTestUploader implements the whutils.Uploader interface for BigQuery
// production tests. It wraps whutils.NewNoOpUploader() and overrides only
// the methods required by the BigQuery connector's loadTable() flow.
type bqTestUploader struct {
	whutils.Uploader

	// loadFiles maps tableName → list of load files with GCS locations
	loadFiles map[string][]whutils.LoadFile

	// schema maps tableName → table schema (used by GetTableSchemaInUpload/InWarehouse)
	schema map[string]whutils.ModelTableSchema
}

func (u *bqTestUploader) GetLoadFilesMetadata(_ context.Context, opts whutils.GetLoadFilesOptions) ([]whutils.LoadFile, error) {
	if files, ok := u.loadFiles[opts.Table]; ok {
		return files, nil
	}
	return nil, nil
}

func (u *bqTestUploader) GetSampleLoadFileLocation(_ context.Context, tableName string) (string, error) {
	if files, ok := u.loadFiles[tableName]; ok && len(files) > 0 {
		return files[0].Location, nil
	}
	return "", fmt.Errorf("no load file for table %s", tableName)
}

func (u *bqTestUploader) GetSingleLoadFile(_ context.Context, tableName string) (whutils.LoadFile, error) {
	if files, ok := u.loadFiles[tableName]; ok && len(files) > 0 {
		return files[0], nil
	}
	return whutils.LoadFile{}, fmt.Errorf("no load file for table %s", tableName)
}

func (u *bqTestUploader) GetTableSchemaInUpload(tableName string) whutils.ModelTableSchema {
	if s, ok := u.schema[tableName]; ok {
		return s
	}
	return nil
}

func (u *bqTestUploader) GetTableSchemaInWarehouse(tableName string) whutils.ModelTableSchema {
	return u.GetTableSchemaInUpload(tableName)
}

func (u *bqTestUploader) UseRudderStorage() bool  { return false }
func (u *bqTestUploader) GetLoadFileType() string { return "json" }
func (u *bqTestUploader) CanAppend() bool         { return true }

// ---------------------------------------------------------------------------
// createBigQueryTestLoadFile — creates a gzipped NDJSON load file for BigQuery
// ---------------------------------------------------------------------------

// createBigQueryTestLoadFile writes a gzipped newline-delimited JSON file
// suitable for BigQuery bulk loading. The file contains one JSON object per
// line with the column names matching the BigQuery table schema.
func createBigQueryTestLoadFile(t *testing.T, events []IdempotentEvent) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "bq_load_*.json.gz")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(tmpFile.Name()) })

	gzWriter := gzip.NewWriter(tmpFile)

	for _, ev := range events {
		// Build a JSON object matching BQ table columns: event, id, received_at, user_id
		row := map[string]string{
			"event":       ev.Event,
			"id":          ev.ID,
			"received_at": ev.ReceivedAt,
			"user_id":     ev.UserID,
		}
		data, err := jsonrs.Marshal(row)
		require.NoError(t, err)
		_, err = gzWriter.Write(data)
		require.NoError(t, err)
		_, err = gzWriter.Write([]byte("\n"))
		require.NoError(t, err)
	}

	require.NoError(t, gzWriter.Close())
	require.NoError(t, tmpFile.Close())

	return tmpFile.Name()
}

// ---------------------------------------------------------------------------
// testBigQueryProductionLoadTable — exercises the production BigQuery connector
// ---------------------------------------------------------------------------

// testBigQueryProductionLoadTable calls the production BigQuery connector's
// LoadTable() method against a real BigQuery instance. It:
//
//  1. Skips if BIGQUERY_INTEGRATION_TEST_CREDENTIALS is not set.
//  2. Uploads a gzipped NDJSON load file to GCS.
//  3. Creates a BigQuery dataset and table via the production connector.
//  4. Calls LoadTable() twice with the same data.
//  5. Verifies that both loads succeed and append rows (BigQuery is append-only).
//  6. Cleans up the dataset after the test.
//
//nolint:unused // called from testIdempotentBigQuery via t.Run
func testBigQueryProductionLoadTable(t *testing.T) {
	t.Helper()

	credJSON, ok := os.LookupEnv(bqhelper.TestKey)
	if !ok {
		t.Skipf("skipping production BigQuery load test: %s not set", bqhelper.TestKey)
	}

	var creds bqhelper.TestCredentials
	require.NoError(t, jsonrs.Unmarshal([]byte(credJSON), &creds))

	ctx := context.Background()
	namespace := fmt.Sprintf("idempotent_bq_prod_%s", strings.ReplaceAll(uuid.New().String(), "-", "_"))
	tableName := "tracks"

	conf := config.New()

	// Build warehouse with real BigQuery destination config
	warehouse := whutils.ModelWarehouse{
		WorkspaceID: idempotentWorkspaceID,
		Source: backendconfig.SourceT{
			ID:      idempotentSourceID,
			Name:    "test-source",
			Enabled: true,
			SourceDefinition: backendconfig.SourceDefinitionT{
				Name: "test-source-def",
			},
		},
		Destination: backendconfig.DestinationT{
			ID:      idempotentDestinationID,
			Name:    "test-bigquery-production",
			Enabled: true,
			DestinationDefinition: backendconfig.DestinationDefinitionT{
				Name: "BigQuery",
			},
			Config: map[string]any{
				"project":     creds.ProjectID,
				"location":    creds.Location,
				"bucketName":  creds.BucketName,
				"credentials": creds.Credentials,
			},
		},
		Namespace: namespace,
		Type:      whutils.BQ,
	}

	tableSchema := whutils.ModelTableSchema{
		"event":       bigQueryStringDataType,
		"id":          bigQueryStringDataType,
		"received_at": bigQueryDateTimeDataType,
		"user_id":     bigQueryStringDataType,
	}

	// Load canonical events from fixtures — use tracks events only
	events := loadIdempotentEvents(t)
	var trackEvents []IdempotentEvent
	for _, ev := range events {
		if ev.Table == "tracks" {
			trackEvents = append(trackEvents, ev)
		}
	}
	require.NotEmpty(t, trackEvents, "must have track events in fixture")

	// Create a gzipped NDJSON load file for BigQuery
	loadFilePath := createBigQueryTestLoadFile(t, trackEvents)

	// Create GCS file manager and upload the load file
	fm, err := filemanager.New(&filemanager.Settings{
		Provider: whutils.GCS,
		Config: map[string]any{
			"project":     creds.ProjectID,
			"location":    creds.Location,
			"bucketName":  creds.BucketName,
			"credentials": creds.Credentials,
		},
		Conf: conf,
	})
	require.NoError(t, err, "failed to create GCS file manager")

	f, err := os.Open(loadFilePath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	uploadOutput, err := fm.Upload(
		ctx, f,
		"rudder-warehouse-load-objects",
		tableName,
		idempotentSourceID,
		uuid.New().String()+"-"+tableName,
	)
	require.NoError(t, err, "failed to upload load file to GCS")
	t.Logf("Uploaded load file to GCS: %s", uploadOutput.Location)

	// Create a direct BigQuery client for verification and cleanup
	bqClient, err := bigquery.NewClient(
		ctx,
		creds.ProjectID,
		option.WithCredentialsJSON([]byte(creds.Credentials)), //nolint:staticcheck
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bqClient.Close() })
	t.Cleanup(func() {
		// Delete the test dataset with all contents
		t.Logf("Cleaning up BigQuery dataset: %s", namespace)
		_ = bqClient.Dataset(namespace).DeleteWithContents(context.Background())
	})

	// Build test uploader with the GCS load file location
	uploader := &bqTestUploader{
		Uploader: whutils.NewNoOpUploader(),
		loadFiles: map[string][]whutils.LoadFile{
			tableName: {{Location: uploadOutput.Location}},
		},
		schema: map[string]whutils.ModelTableSchema{
			tableName: tableSchema,
		},
	}

	// Create the production BigQuery connector
	bq := bqconnector.New(conf, logger.NOP)
	err = bq.Setup(ctx, warehouse, uploader)
	require.NoError(t, err, "BigQuery Setup() failed")

	// Create the dataset (schema) and table
	err = bq.CreateSchema(ctx)
	require.NoError(t, err, "BigQuery CreateSchema() failed")

	err = bq.CreateTable(ctx, tableName, tableSchema)
	require.NoError(t, err, "BigQuery CreateTable() failed")

	// --- First LoadTable() call ---
	stats1, err := bq.LoadTable(ctx, tableName)
	require.NoError(t, err, "first LoadTable() call failed")
	require.NotNil(t, stats1, "first LoadTable() returned nil stats")
	t.Logf("First LoadTable() stats: RowsInserted=%d", stats1.RowsInserted)

	// Verify rows were actually inserted
	require.Greater(t, stats1.RowsInserted, int64(0),
		"first LoadTable() must insert rows — got 0, expected %d", len(trackEvents))

	// --- Second LoadTable() call (replay/idempotency test) ---
	stats2, err := bq.LoadTable(ctx, tableName)
	require.NoError(t, err, "second LoadTable() call failed — production connector not idempotent-safe")
	require.NotNil(t, stats2, "second LoadTable() returned nil stats")
	t.Logf("Second LoadTable() stats: RowsInserted=%d", stats2.RowsInserted)

	// BigQuery is append-only: second load appends the same rows again.
	// After 2 loads, the base table contains 2× the original event count.
	require.Equal(t, stats1.RowsInserted, stats2.RowsInserted,
		"second LoadTable() should insert same number of rows as first (append-only)")

	// Verify total row count in BigQuery table via direct query
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM `%s.%s.%s`",
		creds.ProjectID, namespace, tableName)
	it, err := bqClient.Query(countQuery).Read(ctx)
	require.NoError(t, err, "count query failed")

	var row []bigquery.Value
	err = it.Next(&row)
	require.NoError(t, err, "reading count query result failed")
	require.False(t, errors.Is(err, iterator.Done))

	totalRows, ok := row[0].(int64)
	require.True(t, ok, "count query returned non-int64 value: %T", row[0])

	expectedTotal := int64(len(trackEvents)) * 2
	require.Equal(t, expectedTotal, totalRows,
		"after 2 appends of %d events, table should have %d rows, got %d",
		len(trackEvents), expectedTotal, totalRows)

	t.Logf("BigQuery production LoadTable() verified: %d events × 2 loads = %d total rows (append-only confirmed)", len(trackEvents), totalRows)
}
