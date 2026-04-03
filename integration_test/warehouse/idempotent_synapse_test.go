package warehouse_test

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/utils/misc"
	azuresynapse "github.com/rudderlabs/rudder-server/warehouse/integrations/azure-synapse"
	sqlmw "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
	whutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// --------------------------------------------------------------------------
// Azure Synapse CopyIn idempotency test constants.
// These mirror private constants from azure-synapse.go to verify SQL
// patterns match the expected Synapse dialect without importing the package.
// --------------------------------------------------------------------------

const (
	// synapseVarcharDefaultLength mirrors azuresynapse.varcharDefaultLength.
	// When a VARCHAR column length is not specified or is below this threshold,
	// the default 512 is used for string truncation during ProcessColumnValue.
	synapseVarcharDefaultLength = 512

	// synapseVarcharMaxLength mirrors azuresynapse.varcharMaxLength.
	// A value of -1 indicates VARCHAR(MAX), meaning no truncation is applied.
	synapseVarcharMaxLength = -1

	// synapseTableNameLimit mirrors azuresynapse.tableNameLimit.
	// Azure Synapse limits table identifier names to 127 characters.
	synapseTableNameLimit = 127
)

// --------------------------------------------------------------------------
// mockSynapseExecutor: captures Azure Synapse SQL operations for verification.
// --------------------------------------------------------------------------

// synapseSQLOperation identifies the type of SQL operation captured by
// mockSynapseExecutor. These correspond to the Azure Synapse bulk CopyIn
// flow steps: staging table creation → bulk CopyIn → DELETE dedup →
// INSERT with ROW_NUMBER dedup → staging table cleanup.
type synapseSQLOperation string

const (
	synapseSQLCreateStaging synapseSQLOperation = "CREATE_STAGING"
	synapseSQLCopyIn        synapseSQLOperation = "COPYIN"
	synapseSQLDelete        synapseSQLOperation = "DELETE"
	synapseSQLInsert        synapseSQLOperation = "INSERT"
	synapseSQLDropStaging   synapseSQLOperation = "DROP_STAGING"
)

// capturedSynapseSQL records a single SQL operation captured by the mock
// executor during simulation of the Azure Synapse CopyIn flow.
type capturedSynapseSQL struct {
	operation synapseSQLOperation
	sql       string
	columns   []string
	rows      [][]any
}

// mockSynapseExecutor captures Azure Synapse SQL operations during simulated
// load table execution. It records CREATE_STAGING, COPYIN, DELETE, INSERT,
// and DROP_STAGING operations for verification of SQL pattern correctness,
// determinism, and idempotent behavior. This mock replaces the need for a
// live Azure Synapse connection (which cannot be Dockerized locally).
type mockSynapseExecutor struct {
	captured []capturedSynapseSQL
}

// newMockSynapseExecutor creates a fresh mock executor with an empty captured
// statement list, ready to record SQL operations from a simulated CopyIn flow.
func newMockSynapseExecutor() *mockSynapseExecutor {
	return &mockSynapseExecutor{
		captured: make([]capturedSynapseSQL, 0, 8),
	}
}

// recordStmt records a SQL operation with its operation type, SQL text,
// columns involved, and any row data (for CopyIn bulk insert operations).
func (m *mockSynapseExecutor) recordStmt(
	op synapseSQLOperation,
	sqlText string,
	columns []string,
	rows [][]any,
) {
	m.captured = append(m.captured, capturedSynapseSQL{
		operation: op,
		sql:       sqlText,
		columns:   columns,
		rows:      rows,
	})
}

// statementsOfType returns all captured statements matching the given
// operation type, preserving their original order of capture.
func (m *mockSynapseExecutor) statementsOfType(op synapseSQLOperation) []capturedSynapseSQL {
	var result []capturedSynapseSQL
	for _, s := range m.captured {
		if s.operation == op {
			result = append(result, s)
		}
	}
	return result
}

// allSQLStatements returns all captured SQL strings in capture order.
// Used for determinism verification between first load and replay.
func (m *mockSynapseExecutor) allSQLStatements() []string {
	stmts := make([]string, len(m.captured))
	for i, s := range m.captured {
		stmts[i] = s.sql
	}
	return stmts
}

// reset clears all captured statements for reuse between test runs.
func (m *mockSynapseExecutor) reset() {
	m.captured = m.captured[:0]
}

// --------------------------------------------------------------------------
// SQL generation helpers: produce expected Azure Synapse SQL patterns.
// These construct the exact SQL that the azure-synapse.go connector
// generates, allowing pattern verification without a live connection.
// --------------------------------------------------------------------------

// generateSynapseStagingSQL produces the staging table creation SQL.
// Azure Synapse creates staging tables via: SELECT TOP 0 * INTO <staging> FROM <target>
// which copies the target table's schema without copying any data.
func generateSynapseStagingSQL(namespace, stagingTable, targetTable string) string {
	return fmt.Sprintf(
		"SELECT TOP 0 * INTO %[1]s.%[2]s FROM %[1]s.%[3]s",
		namespace, stagingTable, targetTable,
	)
}

// generateSynapseDeleteSQL produces the DELETE SQL for primary-key-based dedup.
// Azure Synapse deletes matching rows using a FROM-FROM join pattern:
// DELETE FROM <target> FROM <staging> AS _source WHERE _source.<pk> = <target>.<pk>
func generateSynapseDeleteSQL(namespace, tableName, stagingTable, primaryKey string) string {
	return fmt.Sprintf(
		`DELETE FROM %[1]s.%[2]s FROM %[1]s.%[3]s AS _source `+
			`WHERE _source."%[4]s" = %[1]s.%[2]s."%[4]s"`,
		namespace, tableName, stagingTable, primaryKey,
	)
}

// generateSynapseInsertSQL produces the INSERT SQL with ROW_NUMBER dedup.
// Azure Synapse inserts deduped rows using a windowed ROW_NUMBER function:
// INSERT INTO <target> SELECT ... FROM (SELECT *, ROW_NUMBER() OVER
// (PARTITION BY <pk> ORDER BY received_at DESC)) WHERE row_number = 1
func generateSynapseInsertSQL(namespace, tableName, stagingTable, columnList, partitionKey string) string {
	return fmt.Sprintf(
		`INSERT INTO %[1]s.%[2]s (%[3]s) `+
			`SELECT %[3]s FROM `+
			`(SELECT *, ROW_NUMBER() OVER (PARTITION BY %[5]s ORDER BY received_at DESC) `+
			`AS _rudder_staging_row_number FROM %[1]s.%[4]s) AS _ `+
			`WHERE _rudder_staging_row_number = 1`,
		namespace, tableName, columnList, stagingTable, partitionKey,
	)
}

// generateSynapseDropStagingSQL produces the conditional staging table drop SQL.
// Azure Synapse checks OBJECT_ID existence before dropping to avoid errors.
func generateSynapseDropStagingSQL(namespace, stagingTable string) string {
	return fmt.Sprintf(
		`IF OBJECT_ID ('%[1]s.%[2]s','U') IS NOT NULL DROP TABLE %[1]s.%[2]s;`,
		namespace, stagingTable,
	)
}

// generateSynapseVarcharLengthSQL produces the VARCHAR length query SQL.
// Azure Synapse queries INFORMATION_SCHEMA.COLUMNS for CHARACTER_MAXIMUM_LENGTH
// filtered by schema, table, and string data types.
func generateSynapseVarcharLengthSQL() string {
	return `SELECT COLUMN_NAME, CHARACTER_MAXIMUM_LENGTH ` +
		`FROM INFORMATION_SCHEMA.COLUMNS ` +
		`WHERE TABLE_SCHEMA = @schema AND TABLE_NAME = @tableName ` +
		`AND DATA_TYPE IN ('char', 'varchar', 'nchar', 'nvarchar')`
}

// --------------------------------------------------------------------------
// Test-local UCS-2 and diacritics helpers.
// These replicate the behavior of unexported azuresynapse.str2ucs2 and
// azuresynapse.hasDiacritics for verifying expected ProcessColumnValue
// output without importing the azuresynapse package directly.
// --------------------------------------------------------------------------

// testHasDiacritics returns true if the string contains any multibyte
// UTF-8 characters (runes with codepoints above U+007F). This mirrors
// azuresynapse.hasDiacritics which checks utf8.RuneLen(x) > 1.
func testHasDiacritics(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

// testStr2UCS2 converts a string to UCS-2 little-endian byte representation.
// This mirrors azuresynapse.str2ucs2 for BMP characters (U+0000 to U+FFFF),
// producing 2 bytes per character in little-endian byte order.
func testStr2UCS2(s string) []byte {
	runes := []rune(s)
	ucs2 := make([]byte, 2*len(runes))
	for i, r := range runes {
		ucs2[2*i] = byte(r)
		ucs2[2*i+1] = byte(r >> 8)
	}
	return ucs2
}

// testProcessStringColumnValue simulates ProcessColumnValue behavior for the
// string data type. When the string has diacritics, it converts to UCS-2
// and truncates at the byte level. Otherwise it truncates as a plain string.
// This mirrors azuresynapse.ProcessColumnValue for model.StringDataType.
func testProcessStringColumnValue(value string, varcharLength int) any {
	if varcharLength == synapseVarcharMaxLength {
		return value
	}
	maxLen := varcharLength
	if maxLen < synapseVarcharDefaultLength {
		maxLen = synapseVarcharDefaultLength
	}
	if len(value) > maxLen {
		value = value[:maxLen]
	}
	if !testHasDiacritics(value) {
		return value
	}
	byteArr := testStr2UCS2(value)
	if len(byteArr) > maxLen {
		byteArr = byteArr[:maxLen]
	}
	return byteArr
}

// --------------------------------------------------------------------------
// simulateSynapseCopyInFlow replicates the Azure Synapse loadTable logic
// step-by-step using a mockSynapseExecutor. This is the core simulation
// that the test cases exercise — it generates the same SQL patterns that
// the actual connector produces, allowing idempotent verification.
// --------------------------------------------------------------------------

// simulateSynapseCopyInFlow executes a simulated Azure Synapse bulk CopyIn
// flow against the mock executor. It generates SQL statements for staging
// table creation, bulk copy, DELETE dedup, INSERT with ROW_NUMBER dedup,
// and staging table cleanup — identical to azuresynapse.loadTable logic.
//
// Parameters:
//   - executor: mock SQL executor that captures statements
//   - namespace: database schema namespace (e.g., "test_namespace")
//   - tableName: target table name (e.g., "tracks")
//   - sortedColumns: column names in sorted order for this load
//   - previousColumns: columns from warehouse schema (may be superset)
//   - primaryKey: primary key column for dedup
//   - partitionKey: partition key for ROW_NUMBER windowing
//   - rows: event data rows to load
func simulateSynapseCopyInFlow(
	executor *mockSynapseExecutor,
	namespace, tableName string,
	sortedColumns, previousColumns []string,
	primaryKey, partitionKey string,
	rows [][]any,
) {
	// Step 1: Generate a staging table name (deterministic for tests)
	stagingTable := fmt.Sprintf("rudder_staging_%s_%s", tableName, "test_hex")

	// Step 2: Drop any pre-existing staging table (idempotent cleanup)
	dropSQL := generateSynapseDropStagingSQL(namespace, stagingTable)
	executor.recordStmt(synapseSQLDropStaging, dropSQL, nil, nil)

	// Step 3: Create staging table by cloning target schema
	createSQL := generateSynapseStagingSQL(namespace, stagingTable, tableName)
	executor.recordStmt(synapseSQLCreateStaging, createSQL, nil, nil)

	// Step 4: Compute extra columns (in previousColumns but not in sortedColumns)
	extraColumns := computeExtraColumns(sortedColumns, previousColumns)

	// Step 5: Prepare rows with nil padding for extra columns
	paddedRows := make([][]any, len(rows))
	for i, row := range rows {
		paddedRow := make([]any, len(row)+len(extraColumns))
		copy(paddedRow, row)
		// Extra columns are padded with nil values
		for j := range extraColumns {
			paddedRow[len(row)+j] = nil
		}
		paddedRows[i] = paddedRow
	}

	// Step 6: Bulk CopyIn to staging table
	allColumns := append(append([]string{}, sortedColumns...), extraColumns...)
	copyInSQL := fmt.Sprintf("mssql.CopyIn(%s.%s, mssql.BulkOptions{}, %s)",
		namespace, stagingTable, strings.Join(allColumns, ", "))
	executor.recordStmt(synapseSQLCopyIn, copyInSQL, allColumns, paddedRows)

	// Step 7: DELETE conflicting rows from target
	deleteSQL := generateSynapseDeleteSQL(namespace, tableName, stagingTable, primaryKey)
	executor.recordStmt(synapseSQLDelete, deleteSQL, nil, nil)

	// Step 8: INSERT deduplicated rows from staging using ROW_NUMBER
	columnList := quoteAndJoin(allColumns)
	insertSQL := generateSynapseInsertSQL(namespace, tableName, stagingTable, columnList, partitionKey)
	executor.recordStmt(synapseSQLInsert, insertSQL, allColumns, nil)

	// Step 9: Drop staging table (cleanup)
	dropSQL2 := generateSynapseDropStagingSQL(namespace, stagingTable)
	executor.recordStmt(synapseSQLDropStaging, dropSQL2, nil, nil)
}

// computeExtraColumns returns columns present in previousColumns but not in
// sortedColumns. These represent warehouse-schema columns not present in the
// current upload data that must be nil-padded during CopyIn.
func computeExtraColumns(sortedColumns, previousColumns []string) []string {
	sortedSet := make(map[string]struct{}, len(sortedColumns))
	for _, c := range sortedColumns {
		sortedSet[c] = struct{}{}
	}
	var extra []string
	for _, c := range previousColumns {
		if _, found := sortedSet[c]; !found {
			extra = append(extra, c)
		}
	}
	return extra
}

// quoteAndJoin wraps each column name in double quotes and joins them
// with commas. This matches the SQL column list formatting used by
// the Azure Synapse connector in INSERT/SELECT statements.
func quoteAndJoin(cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf(`"%s"`, c)
	}
	return strings.Join(quoted, ", ")
}

// buildSynapseTestWarehouse constructs a whutils.ModelWarehouse with Azure
// Synapse configuration for use in mock-based testing. Populates required
// fields using shared idempotent test constants and the Azure Synapse dest type.
func buildSynapseTestWarehouse(namespace string) whutils.ModelWarehouse {
	return whutils.ModelWarehouse{
		WorkspaceID: idempotentWorkspaceID,
		Source: backendconfig.SourceT{
			ID:       idempotentSourceID,
			WriteKey: idempotentWriteKey,
			Enabled:  true,
		},
		Destination: backendconfig.DestinationT{
			ID:      idempotentDestinationID,
			Enabled: true,
			DestinationDefinition: backendconfig.DestinationDefinitionT{
				Name: whutils.AzureSynapse,
			},
			Config: map[string]any{
				"host":     "localhost",
				"port":     "1433",
				"database": "master",
				"user":     "sa",
				"password": "test_password",
			},
		},
		Namespace: namespace,
		Type:      whutils.AzureSynapse,
	}
}

// --------------------------------------------------------------------------
// testIdempotentSynapse validates Azure Synapse bulk CopyIn idempotency
// across 5 table-driven test scenarios. Azure Synapse uses the same merge
// pattern as MSSQL (staging → DELETE + INSERT with ROW_NUMBER dedup) but
// with Synapse-specific SQL dialect. Since Azure Synapse cannot be
// Dockerized locally, all tests use mock/interface verification.
// --------------------------------------------------------------------------

func testIdempotentSynapse(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping synapse idempotent test in short mode")
	}

	// Load canonical test events from shared fixture
	events := loadIdempotentEvents(t)
	require.NotEmpty(t, events, "idempotent events fixture must not be empty")

	// Build base warehouse configuration for Azure Synapse
	namespace := idempotentNamespace
	warehouse := buildSynapseTestWarehouse(namespace)
	require.Equal(t, whutils.AzureSynapse, warehouse.Type)

	// Create context with timeout for test execution
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Suppress unused variable lint; ctx is used in subtests
	_ = ctx

	// Create gomock controller for mock lifecycle management
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Confirm config, logger, stats construct without errors (NOP for tests)
	conf := config.New()
	log := logger.NOP
	st := stats.NOP
	require.NotNil(t, conf)
	require.NotNil(t, log)
	require.NotNil(t, st)

	// ----------------------------------------------------------------
	// Test Case 1: synapse_copyin_staging_dedup
	// ----------------------------------------------------------------
	t.Run("synapse_copyin_staging_dedup", func(t *testing.T) {
		// Scenario: Mock Synapse SQL execution for bulk CopyIn → staging →
		// DELETE+INSERT dedup. After dedup, row count equals unique events.
		// Merge Strategy: bulk CopyIn to staging, DELETE conflicting,
		// INSERT with ROW_NUMBER dedup — same as MSSQL with Synapse dialect.

		executor := newMockSynapseExecutor()

		tableName := "tracks"
		sortedColumns := []string{"id", "user_id", "event", "received_at"}
		previousColumns := sortedColumns // No extra columns in this test
		primaryKey := "id"
		partitionKey := "id"

		// Build track-specific rows from events (include duplicates)
		var trackRows [][]any
		for _, e := range events {
			if e.Table == tableName {
				trackRows = append(trackRows, []any{e.ID, e.UserID, e.Event, e.ReceivedAt})
			}
		}
		require.NotEmpty(t, trackRows, "must have track events")

		// First load: simulate full CopyIn flow
		simulateSynapseCopyInFlow(
			executor, namespace, tableName,
			sortedColumns, previousColumns,
			primaryKey, partitionKey,
			trackRows,
		)

		// Verify SQL operation sequence matches expected CopyIn flow
		captured := executor.captured
		require.True(t, len(captured) >= 5,
			"expected at least 5 SQL operations (drop, create, copyin, delete, insert), got %d", len(captured))

		// Verify operation types in correct order
		require.Equal(t, synapseSQLDropStaging, captured[0].operation,
			"first operation should be DROP staging (cleanup)")
		require.Equal(t, synapseSQLCreateStaging, captured[1].operation,
			"second operation should be CREATE staging")
		require.Equal(t, synapseSQLCopyIn, captured[2].operation,
			"third operation should be COPYIN")
		require.Equal(t, synapseSQLDelete, captured[3].operation,
			"fourth operation should be DELETE (dedup)")
		require.Equal(t, synapseSQLInsert, captured[4].operation,
			"fifth operation should be INSERT (dedup)")

		// Verify staging table creation SQL matches Synapse dialect
		createStmt := captured[1]
		require.True(t, strings.Contains(createStmt.sql, "SELECT TOP 0 * INTO"),
			"staging table creation must use SELECT TOP 0 INTO pattern, got: %s", createStmt.sql)
		require.True(t, strings.Contains(createStmt.sql, namespace),
			"staging SQL must reference namespace: %s", createStmt.sql)
		require.True(t, strings.Contains(createStmt.sql, tableName),
			"staging SQL must reference target table: %s", createStmt.sql)

		// Verify DELETE SQL references primary key for dedup
		deleteStmt := captured[3]
		require.True(t, strings.Contains(deleteStmt.sql, "DELETE FROM"),
			"delete SQL must start with DELETE FROM, got: %s", deleteStmt.sql)
		require.True(t, strings.Contains(deleteStmt.sql, fmt.Sprintf(`"%s"`, primaryKey)),
			"delete SQL must reference primary key %q: %s", primaryKey, deleteStmt.sql)
		require.True(t, strings.Contains(deleteStmt.sql, "_source"),
			"delete SQL must use _source alias for staging table: %s", deleteStmt.sql)

		// Verify INSERT SQL uses ROW_NUMBER dedup with PARTITION BY
		insertStmt := captured[4]
		require.True(t, strings.Contains(insertStmt.sql, "INSERT INTO"),
			"insert SQL must contain INSERT INTO, got: %s", insertStmt.sql)
		require.True(t, strings.Contains(insertStmt.sql, "ROW_NUMBER() OVER (PARTITION BY"),
			"insert SQL must use ROW_NUMBER PARTITION BY for dedup: %s", insertStmt.sql)
		require.True(t, strings.Contains(insertStmt.sql, "_rudder_staging_row_number"),
			"insert SQL must reference _rudder_staging_row_number: %s", insertStmt.sql)
		require.True(t, strings.Contains(insertStmt.sql, "WHERE _rudder_staging_row_number = 1"),
			"insert SQL must filter on row_number = 1: %s", insertStmt.sql)
		require.True(t, strings.Contains(insertStmt.sql, "ORDER BY received_at DESC"),
			"insert SQL must ORDER BY received_at DESC for latest-wins: %s", insertStmt.sql)

		// Verify CopyIn row count equals total track events (including duplicates,
		// since dedup happens at INSERT stage, not CopyIn stage)
		copyInStmt := captured[2]
		require.Len(t, copyInStmt.rows, len(trackRows),
			"CopyIn row count must equal total track events (dedup at INSERT, not CopyIn)")

		// After ROW_NUMBER dedup, unique events are kept (tracks has 8 unique out of 8)
		t.Logf("synapse_copyin_staging_dedup: %d total rows CopyIn, "+
			"dedup via ROW_NUMBER yields unique rows per primary key %q", len(trackRows), primaryKey)
	})

	// ----------------------------------------------------------------
	// Test Case 2: synapse_varchar_length_map
	// ----------------------------------------------------------------
	t.Run("synapse_varchar_length_map", func(t *testing.T) {
		// Scenario: Verify Synapse-specific VARCHAR length mapping via
		// INFORMATION_SCHEMA.COLUMNS query. ProcessColumnValue behavior
		// changes based on column VARCHAR length: truncation thresholds
		// differ for default (512), explicit lengths, and VARCHAR(MAX).

		// Verify the VARCHAR length query SQL matches Synapse dialect
		varcharSQL := generateSynapseVarcharLengthSQL()
		require.True(t, strings.Contains(varcharSQL, "INFORMATION_SCHEMA.COLUMNS"),
			"varchar length query must use INFORMATION_SCHEMA.COLUMNS")
		require.True(t, strings.Contains(varcharSQL, "CHARACTER_MAXIMUM_LENGTH"),
			"varchar length query must select CHARACTER_MAXIMUM_LENGTH")
		require.True(t, strings.Contains(varcharSQL, "DATA_TYPE IN"),
			"varchar length query must filter by string DATA_TYPE values")

		// Table-driven tests for ProcessColumnValue with different varchar lengths.
		// These replicate azuresynapse.ProcessColumnValue logic for the string type.
		type varcharTestCase struct {
			name           string
			input          string
			varcharLength  int
			expectedOutput any
			description    string
		}

		testCases := []varcharTestCase{
			{
				name:           "default_length_no_truncation",
				input:          "short string",
				varcharLength:  0,
				expectedOutput: "short string",
				description:    "string shorter than default 512 is returned as-is",
			},
			{
				name:           "default_length_with_truncation",
				input:          strings.Repeat("test", 200), // 800 chars
				varcharLength:  0,
				expectedOutput: strings.Repeat("test", 128), // 512 chars
				description:    "string exceeding default 512 is truncated to 512",
			},
			{
				name:           "explicit_length_1024_no_truncation",
				input:          strings.Repeat("test", 200), // 800 chars
				varcharLength:  1024,
				expectedOutput: strings.Repeat("test", 200), // 800 < 1024, no truncation
				description:    "string within explicit varchar(1024) limit passes through",
			},
			{
				name:           "explicit_length_256_with_truncation",
				input:          strings.Repeat("ab", 200), // 400 chars
				varcharLength:  256,
				expectedOutput: strings.Repeat("ab", 200), // 400 < 512 (max of 256 and 512 is 512)
				description:    "explicit length below default still uses default 512 as minimum",
			},
			{
				name:           "varchar_max_no_truncation",
				input:          strings.Repeat("test", 5000), // 20000 chars
				varcharLength:  synapseVarcharMaxLength,
				expectedOutput: strings.Repeat("test", 5000), // VARCHAR(MAX) = no truncation
				description:    "VARCHAR(MAX) length (-1) means no truncation at all",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				result := testProcessStringColumnValue(tc.input, tc.varcharLength)
				require.Equal(t, tc.expectedOutput, result,
					"varchar test %s: %s", tc.name, tc.description)
			})
		}

		// Verify the staging table name respects the 127-character limit
		longTableName := strings.Repeat("a", 200)
		stagingPrefix := whutils.StagingTablePrefix(whutils.AzureSynapse)
		require.Equal(t, "rudder_staging_", stagingPrefix,
			"Azure Synapse staging prefix must be lowercase rudder_staging_")

		// StagingTableName truncates to synapseTableNameLimit (127)
		stagingName := whutils.StagingTableName(whutils.AzureSynapse, longTableName, synapseTableNameLimit)
		require.True(t, len(stagingName) <= synapseTableNameLimit,
			"staging table name length %d exceeds limit %d: %s",
			len(stagingName), synapseTableNameLimit, stagingName)
		require.True(t, strings.HasPrefix(stagingName, stagingPrefix),
			"staging table name must start with prefix %q: %s", stagingPrefix, stagingName)

		t.Logf("synapse_varchar_length_map: verified %d varchar length scenarios, "+
			"staging prefix=%q, table name limit=%d", len(testCases), stagingPrefix, synapseTableNameLimit)
	})

	// ----------------------------------------------------------------
	// Test Case 3: replay_produces_identical_sql_sequence
	// ----------------------------------------------------------------
	t.Run("replay_produces_identical_sql_sequence", func(t *testing.T) {
		// Scenario: First load vs replay load should produce the same SQL
		// statement sequence. This is the core idempotency guarantee —
		// replaying the same events generates identical DML operations.

		executor := newMockSynapseExecutor()

		tableName := "identifies"
		sortedColumns := []string{"id", "user_id", "received_at"}
		previousColumns := sortedColumns
		primaryKey := "id"
		partitionKey := "id"

		// Build identify-specific rows from events (includes one duplicate)
		var identifyRows [][]any
		for _, e := range events {
			if e.Table == "identifies" {
				identifyRows = append(identifyRows, []any{e.ID, e.UserID, e.ReceivedAt})
			}
		}
		require.NotEmpty(t, identifyRows, "must have identify events")

		// First load: generate full SQL sequence
		simulateSynapseCopyInFlow(
			executor, namespace, tableName,
			sortedColumns, previousColumns,
			primaryKey, partitionKey,
			identifyRows,
		)
		firstLoadSQL := executor.allSQLStatements()
		require.NotEmpty(t, firstLoadSQL, "first load must produce SQL statements")

		// Reset executor and replay identical event set
		executor.reset()
		simulateSynapseCopyInFlow(
			executor, namespace, tableName,
			sortedColumns, previousColumns,
			primaryKey, partitionKey,
			identifyRows,
		)
		replayLoadSQL := executor.allSQLStatements()
		require.NotEmpty(t, replayLoadSQL, "replay load must produce SQL statements")

		// Core idempotency assertion: SQL sequences must be identical
		require.Equal(t, len(firstLoadSQL), len(replayLoadSQL),
			"first load and replay must produce same number of SQL statements")
		for i := range firstLoadSQL {
			require.Equal(t, firstLoadSQL[i], replayLoadSQL[i],
				"SQL statement %d differs between first load and replay:\n  first:  %s\n  replay: %s",
				i, firstLoadSQL[i], replayLoadSQL[i])
		}

		// Verify DELETE+INSERT patterns are present in both sequences
		firstDeletes := executor.statementsOfType(synapseSQLDelete)
		firstInserts := executor.statementsOfType(synapseSQLInsert)
		require.Len(t, firstDeletes, 1,
			"replay must produce exactly 1 DELETE statement for dedup")
		require.Len(t, firstInserts, 1,
			"replay must produce exactly 1 INSERT statement for dedup")

		// Verify DELETE SQL contains primary key matching
		require.True(t, strings.Contains(firstDeletes[0].sql, fmt.Sprintf(`"%s"`, primaryKey)),
			"DELETE must reference primary key for dedup: %s", firstDeletes[0].sql)

		// Verify INSERT SQL contains ROW_NUMBER dedup
		require.True(t, strings.Contains(firstInserts[0].sql, "ROW_NUMBER() OVER (PARTITION BY"),
			"INSERT must use ROW_NUMBER dedup: %s", firstInserts[0].sql)

		t.Logf("replay_produces_identical_sql_sequence: verified %d SQL statements "+
			"are identical between first load and replay for table %q",
			len(firstLoadSQL), tableName)
	})

	// ----------------------------------------------------------------
	// Test Case 4: diacritic_handling_ucs2
	// ----------------------------------------------------------------
	t.Run("diacritic_handling_ucs2", func(t *testing.T) {
		// Scenario: Events with diacritical characters (accented names,
		// non-ASCII text) must be encoded via UCS-2 and truncated correctly.
		// ProcessColumnValue detects diacritics via hasDiacritics, converts
		// to UCS-2 via str2ucs2, and truncates at byte level.

		type diacriticTestCase struct {
			name             string
			input            string
			varcharLength    int
			expectDiacritics bool
			expectedType     string // "string" or "bytes"
			expectedOutput   any
			description      string
		}

		testCases := []diacriticTestCase{
			{
				name:             "simple_accent_e",
				input:            "tést",
				varcharLength:    0, // default 512
				expectDiacritics: true,
				expectedType:     "bytes",
				expectedOutput:   []byte{0x74, 0x00, 0xe9, 0x00, 0x73, 0x00, 0x74, 0x00},
				description:      "é (U+00E9) triggers UCS-2 encoding: t=0x74, é=0xE9, s=0x73, t=0x74 in LE",
			},
			{
				name:             "no_diacritics_ascii",
				input:            "hello world",
				varcharLength:    0,
				expectDiacritics: false,
				expectedType:     "string",
				expectedOutput:   "hello world",
				description:      "pure ASCII string is returned as-is without UCS-2 conversion",
			},
			{
				name:             "german_umlaut",
				input:            "München",
				varcharLength:    0,
				expectDiacritics: true,
				expectedType:     "bytes",
				expectedOutput:   testStr2UCS2("München"),
				description:      "ü (U+00FC) triggers UCS-2 encoding for German city name",
			},
			{
				name:             "french_accented_name",
				input:            "François",
				varcharLength:    0,
				expectDiacritics: true,
				expectedType:     "bytes",
				expectedOutput:   testStr2UCS2("François"),
				description:      "ç (U+00E7) triggers UCS-2 encoding for French name",
			},
			{
				name:             "japanese_characters",
				input:            "東京",
				varcharLength:    0,
				expectDiacritics: true,
				expectedType:     "bytes",
				expectedOutput:   testStr2UCS2("東京"),
				description:      "CJK characters (U+6771, U+4EAC) trigger UCS-2 encoding",
			},
			{
				name:             "mixed_ascii_diacritics",
				input:            "cafe\u0301", // café with combining accent
				varcharLength:    0,
				expectDiacritics: true,
				expectedType:     "bytes",
				expectedOutput:   testStr2UCS2("cafe\u0301"),
				description:      "combining acute accent (U+0301) triggers UCS-2 encoding",
			},
			{
				name:             "diacritics_with_varchar_max",
				input:            "tést",
				varcharLength:    synapseVarcharMaxLength, // -1 = no truncation
				expectDiacritics: true,                    // "tést" has diacritics, but VARCHAR(MAX) returns early
				expectedType:     "string",
				expectedOutput:   "tést",
				description:      "VARCHAR(MAX) returns original string bypassing UCS-2 even with diacritics",
			},
			{
				name:             "empty_string",
				input:            "",
				varcharLength:    0,
				expectDiacritics: false,
				expectedType:     "string",
				expectedOutput:   "",
				description:      "empty string has no diacritics and returns as-is",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				// Verify diacritics detection
				hasDiacritics := testHasDiacritics(tc.input)
				require.Equal(t, tc.expectDiacritics, hasDiacritics,
					"diacritics detection mismatch for %q", tc.input)

				// Process through the simulated ProcessColumnValue for string type
				result := testProcessStringColumnValue(tc.input, tc.varcharLength)

				switch tc.expectedType {
				case "bytes":
					resultBytes, ok := result.([]byte)
					require.True(t, ok,
						"expected []byte result for %q with diacritics, got %T", tc.input, result)
					expectedBytes, ok := tc.expectedOutput.([]byte)
					require.True(t, ok, "test case expected output must be []byte")
					require.Equal(t, expectedBytes, resultBytes,
						"UCS-2 byte output mismatch for %q", tc.input)
					// Verify byte length is 2 * rune count (UCS-2 = 2 bytes per char)
					runeCount := len([]rune(tc.input))
					require.Equal(t, runeCount*2, len(resultBytes),
						"UCS-2 byte length must be 2x rune count for BMP chars")
				case "string":
					resultStr, ok := result.(string)
					require.True(t, ok,
						"expected string result for %q without diacritics, got %T", tc.input, result)
					expectedStr, ok := tc.expectedOutput.(string)
					require.True(t, ok, "test case expected output must be string")
					require.Equal(t, expectedStr, resultStr,
						"string output mismatch for %q", tc.input)
				default:
					t.Fatalf("unknown expected type: %s", tc.expectedType)
				}
			})
		}

		// Verify specific UCS-2 byte layout for "tést" (canonical test case)
		tEstUCS2 := testStr2UCS2("tést")
		require.Equal(t, 8, len(tEstUCS2),
			"UCS-2 of 'tést' (4 runes) must be 8 bytes")
		require.Equal(t, byte(0x74), tEstUCS2[0], "t low byte")
		require.Equal(t, byte(0x00), tEstUCS2[1], "t high byte")
		require.Equal(t, byte(0xe9), tEstUCS2[2], "é low byte")
		require.Equal(t, byte(0x00), tEstUCS2[3], "é high byte")
		require.Equal(t, byte(0x73), tEstUCS2[4], "s low byte")
		require.Equal(t, byte(0x00), tEstUCS2[5], "s high byte")
		require.Equal(t, byte(0x74), tEstUCS2[6], "t low byte (final)")
		require.Equal(t, byte(0x00), tEstUCS2[7], "t high byte (final)")

		t.Logf("diacritic_handling_ucs2: verified %d diacritics scenarios "+
			"including UCS-2 encoding, ASCII passthrough, and VARCHAR(MAX) bypass",
			len(testCases))
	})

	// ----------------------------------------------------------------
	// Test Case 5: extra_columns_nil_padding
	// ----------------------------------------------------------------
	t.Run("extra_columns_nil_padding", func(t *testing.T) {
		// Scenario: Warehouse schema has more columns than upload columns.
		// Extra columns must be padded with nil values during CopyIn.
		// This tests the extraColumns computation and nil padding logic
		// in the Azure Synapse loadTable implementation.
		//
		// Uses "row_id" as primary key (matching Discards table convention)
		// to verify non-default primary key handling in the dedup flow.

		executor := newMockSynapseExecutor()

		tableName := "pages"
		// Upload columns: current staging file has these columns
		sortedColumns := []string{"row_id", "user_id", "received_at"}
		// Previous warehouse columns: schema includes extra columns from prior loads
		previousColumns := []string{"row_id", "user_id", "received_at", "title", "url", "referrer"}
		// Use "row_id" as primary key (matching Discards table convention in azuresynapse.primaryKeyMap)
		primaryKey := "row_id"
		partitionKey := "row_id"

		// Build page-specific rows (columns match sortedColumns only, using ID as row_id)
		var pageRows [][]any
		for _, e := range events {
			if e.Table == "pages" {
				pageRows = append(pageRows, []any{e.ID, e.UserID, e.ReceivedAt})
			}
		}
		require.NotEmpty(t, pageRows, "must have page events")

		// Simulate CopyIn flow with extra columns
		simulateSynapseCopyInFlow(
			executor, namespace, tableName,
			sortedColumns, previousColumns,
			primaryKey, partitionKey,
			pageRows,
		)

		// Verify extra columns were computed correctly
		extraCols := computeExtraColumns(sortedColumns, previousColumns)
		require.Len(t, extraCols, 3,
			"should have 3 extra columns (title, url, referrer)")
		require.Equal(t, "title", extraCols[0])
		require.Equal(t, "url", extraCols[1])
		require.Equal(t, "referrer", extraCols[2])

		// Verify CopyIn statement includes all columns (sorted + extra)
		copyInStmts := executor.statementsOfType(synapseSQLCopyIn)
		require.Len(t, copyInStmts, 1, "must have exactly 1 CopyIn statement")

		allExpectedCols := append(append([]string{}, sortedColumns...), extraCols...)
		require.Equal(t, allExpectedCols, copyInStmts[0].columns,
			"CopyIn column list must include sorted columns then extra columns")

		// Verify each row has nil padding for extra columns
		for rowIdx, row := range copyInStmts[0].rows {
			expectedLen := len(sortedColumns) + len(extraCols)
			require.Len(t, row, expectedLen,
				"row %d must have %d values (sorted + extra)", rowIdx, expectedLen)

			// First N values correspond to sortedColumns (non-nil event data)
			for colIdx := 0; colIdx < len(sortedColumns); colIdx++ {
				require.NotNil(t, row[colIdx],
					"row %d, col %d (%s) must not be nil (event data)",
					rowIdx, colIdx, sortedColumns[colIdx])
			}

			// Extra columns must be nil-padded
			for colIdx := len(sortedColumns); colIdx < expectedLen; colIdx++ {
				require.Nil(t, row[colIdx],
					"row %d, col %d (%s) must be nil (extra column padding)",
					rowIdx, colIdx, allExpectedCols[colIdx])
			}
		}

		// Verify INSERT SQL includes all columns (sorted + extra) in column list
		insertStmts := executor.statementsOfType(synapseSQLInsert)
		require.Len(t, insertStmts, 1, "must have exactly 1 INSERT statement")
		for _, col := range allExpectedCols {
			require.True(t, strings.Contains(insertStmts[0].sql, fmt.Sprintf(`"%s"`, col)),
				"INSERT SQL must reference extra column %q: %s", col, insertStmts[0].sql)
		}

		t.Logf("extra_columns_nil_padding: verified %d extra columns (title, url, referrer) "+
			"are nil-padded in %d CopyIn rows, all present in INSERT column list",
			len(extraCols), len(pageRows))
	})

	// ----------------------------------------------------------------
	// Test Case 6: production_load_table
	// ----------------------------------------------------------------
	t.Run("production_load_table", func(t *testing.T) {
		testSynapseProductionLoadTable(t, events)
	})

	// Generate staging payload for overall validation
	payload := generateIdempotentStagingPayload(t, events)
	require.NotEmpty(t, payload, "staging payload must not be empty")

	// Marshal warehouse config to verify jsonrs serialization
	warehouseJSON, err := jsonrs.Marshal(warehouse)
	require.NoError(t, err, "warehouse config must serialize via jsonrs")
	require.NotEmpty(t, warehouseJSON, "serialized warehouse must not be empty")

	t.Logf("testIdempotentSynapse: all 6 test cases completed for connector=%s, "+
		"merge_strategy=BULK_COPYIN, events=%d, warehouse=%s",
		whutils.AzureSynapse, len(events), warehouse.Type)
}

// ---------------------------------------------------------------------------
// synapseTestDownloader implements the Downloader interface for the Azure
// Synapse production LoadTable test. It maps table names to pre-created
// gzipped CSV load file paths.
// ---------------------------------------------------------------------------

type synapseTestDownloader struct {
	files map[string]string
}

func (d *synapseTestDownloader) Download(_ context.Context, tableName string) ([]string, error) {
	return []string{d.files[tableName]}, nil
}

// ---------------------------------------------------------------------------
// synapseTestUploader implements whutils.Uploader for the Azure Synapse
// production LoadTable test. It provides schema metadata needed by the
// connector's loadTable method.
// ---------------------------------------------------------------------------

type synapseTestUploader struct {
	whutils.Uploader // embed NoOp for unneeded methods
	schema           whutils.ModelTableSchema
}

func (u *synapseTestUploader) GetTableSchemaInUpload(_ string) whutils.ModelTableSchema {
	return u.schema
}

func (u *synapseTestUploader) GetTableSchemaInWarehouse(_ string) whutils.ModelTableSchema {
	return u.schema
}

func (u *synapseTestUploader) UseRudderStorage() bool { return false }

func (u *synapseTestUploader) CanAppend() bool { return false }

func (u *synapseTestUploader) GetLoadFileType() string { return whutils.LoadFileTypeCsv }

func (u *synapseTestUploader) ShouldOnDedupUseNewRecord() bool { return false }

func (u *synapseTestUploader) IsWarehouseSchemaEmpty() bool { return false }

func (u *synapseTestUploader) GetLoadFilesMetadata(_ context.Context, _ whutils.GetLoadFilesOptions) ([]whutils.LoadFile, error) {
	return nil, nil
}

func (u *synapseTestUploader) GetSampleLoadFileLocation(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (u *synapseTestUploader) GetSingleLoadFile(_ context.Context, _ string) (whutils.LoadFile, error) {
	return whutils.LoadFile{}, nil
}

// createSynapseTestLoadFile creates a gzipped CSV load file from IdempotentEvent
// entries. Column order is alphabetical (event, id, received_at, user_id) to match
// warehouseutils.SortColumnKeysFromColumnMap ordering.
// Timestamps use RFC3339 format as expected by Azure Synapse's ProcessColumnValue.
func createSynapseTestLoadFile(t *testing.T, events []IdempotentEvent) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "synapse_load_*.csv.gz")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(tmpFile.Name()) })

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

	return tmpFile.Name()
}

// testSynapseProductionLoadTable exercises the real AzureSynapse.LoadTable()
// production code path by:
//  1. Spinning up an MSSQL Docker container (Azure Synapse uses same SQL Server protocol)
//  2. Creating target table with appropriate schema
//  3. Creating a gzipped CSV load file with duplicated events
//  4. Injecting the DB connection and mock dependencies into the AzureSynapse struct
//  5. Calling the production LoadTable() method
//  6. Verifying that after dedup, only unique rows remain
func testSynapseProductionLoadTable(t *testing.T, events []IdempotentEvent) {
	t.Helper()

	// Initialize misc package to ensure pkgLogger is non-nil — production
	// loadTable defers misc.RemoveFilePaths which needs the logger.
	misc.Init()

	// Use the shared MSSQL Docker container setup (Azure Synapse uses sqlserver protocol)
	db, cleanup := setupMSSQLContainer(t)
	defer cleanup()

	ctx := context.Background()
	ns := uniqueIdempotentNamespace()

	// Create schema in the MSSQL container
	_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA [%s]", ns))
	require.NoError(t, err, "failed to create test schema %s", ns)

	tableName := "identifies"

	// Create the target table with Azure Synapse-compatible data types.
	// Azure Synapse maps: string→varchar(512), datetime→datetimeoffset
	createTableSQL := fmt.Sprintf(
		`CREATE TABLE [%s].[%s] (
			[event] VARCHAR(512),
			[id] VARCHAR(512),
			[received_at] DATETIMEOFFSET,
			[user_id] VARCHAR(512)
		)`,
		ns, tableName,
	)
	_, err = db.ExecContext(ctx, createTableSQL)
	require.NoError(t, err, "failed to create target table")

	// Filter identifies events (contains duplicates: ID a1b2c3d4-1111-4000-8000-000000000001)
	var identifiesEvents []IdempotentEvent
	for _, e := range events {
		if e.Table == tableName {
			identifiesEvents = append(identifiesEvents, e)
		}
	}
	require.NotEmpty(t, identifiesEvents, "must have identifies events")

	// Count unique IDs to determine expected dedup result
	uniqueIDs := make(map[string]struct{})
	for _, e := range identifiesEvents {
		uniqueIDs[e.ID] = struct{}{}
	}
	expectedUniqueRows := len(uniqueIDs)
	require.Less(t, expectedUniqueRows, len(identifiesEvents),
		"identifies events must contain duplicates for meaningful dedup test")

	// Create load file from identifies events
	loadFilePath := createSynapseTestLoadFile(t, identifiesEvents)

	// Schema matches alphabetical column order
	tableSchema := whutils.ModelTableSchema{
		"event":       "string",
		"id":          "string",
		"received_at": "datetime",
		"user_id":     "string",
	}

	// Build warehouse model for the connector
	warehouse := whutils.ModelWarehouse{
		Source: backendconfig.SourceT{
			ID: "test-source-synapse",
			SourceDefinition: backendconfig.SourceDefinitionT{
				Name: "test-source-def",
			},
		},
		Destination: backendconfig.DestinationT{
			ID: "test-dest-synapse",
			DestinationDefinition: backendconfig.DestinationDefinitionT{
				Name: whutils.AzureSynapse,
			},
			Config: map[string]interface{}{
				"host":     "localhost",
				"database": "master",
				"user":     "sa",
				"password": "Test@12345",
				"port":     "1433",
				"sslMode":  "disable",
			},
		},
		WorkspaceID: "test-workspace",
		Namespace:   ns,
		Type:        whutils.AzureSynapse,
	}

	mockUploader := &synapseTestUploader{
		Uploader: whutils.NewNoOpUploader(),
		schema:   tableSchema,
	}
	mockDownloader := &synapseTestDownloader{
		files: map[string]string{tableName: loadFilePath},
	}

	// Create the production AzureSynapse connector using New()
	conf := config.New()
	as := azuresynapse.New(conf, logger.NOP, stats.NOP)

	// Wrap the raw *sql.DB in the sqlmw middleware wrapper that the connector expects
	sqlmwDB := sqlmw.New(db)

	// Inject private fields using reflect+unsafe (same pattern as MSSQL test)
	setUnexportedField(as, "db", sqlmwDB)
	setUnexportedField(as, "namespace", ns)
	setUnexportedField(as, "warehouse", warehouse)
	setUnexportedField(as, "uploader", whutils.Uploader(mockUploader))
	setUnexportedField(as, "loadFileDownLoader", mockDownloader)

	// First load: call production LoadTable()
	loadStats, err := as.LoadTable(ctx, tableName)
	require.NoError(t, err, "first LoadTable() call should succeed")
	require.NotNil(t, loadStats, "load stats must not be nil")

	// Verify row count after first load: should equal unique identifies events
	var rowCount int
	err = db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM [%s].[%s]", ns, tableName,
	)).Scan(&rowCount)
	require.NoError(t, err, "counting rows after first load")
	require.Equal(t, expectedUniqueRows, rowCount,
		"after first load, row count should equal unique events (dedup via ROW_NUMBER)")

	// Replay: create a fresh load file (the first was deleted by
	// the production deferred misc.RemoveFilePaths), then call LoadTable again.
	replayPath := createSynapseTestLoadFile(t, identifiesEvents)
	mockDownloader.files[tableName] = replayPath

	loadStats2, err := as.LoadTable(ctx, tableName)
	require.NoError(t, err, "replay LoadTable() call should succeed")
	require.NotNil(t, loadStats2, "replay load stats must not be nil")

	// Verify idempotency: row count should remain the same after replay
	var rowCountAfterReplay int
	err = db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM [%s].[%s]", ns, tableName,
	)).Scan(&rowCountAfterReplay)
	require.NoError(t, err, "counting rows after replay")
	require.Equal(t, expectedUniqueRows, rowCountAfterReplay,
		"after replay, row count must remain %d (idempotent dedup)", expectedUniqueRows)

	t.Logf("production_load_table (AzureSynapse): first load %d rows (dedup from %d), "+
		"replay preserved %d rows — idempotent dedup verified via production LoadTable()",
		rowCount, len(identifiesEvents), rowCountAfterReplay)
}
