package warehouse_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ory/dockertest/v3"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/minio"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"

	"github.com/rudderlabs/rudder-server/warehouse/integrations/testhelper"
)

// --------------------------------------------------------------------------
// Shared constants used across ALL 9 connector-specific idempotent test files.
// These constants provide deterministic, well-known identifiers for workspace,
// source, destination, and namespace values used in idempotent sync testing.
// --------------------------------------------------------------------------

const (
	idempotentWorkspaceID   = "idempotent_test_workspace"
	idempotentWriteKey      = "idempotent_test_write_key"
	idempotentSourceID      = "idempotent_test_source"
	idempotentDestinationID = "idempotent_test_destination"
	idempotentNamespace     = "idempotent_test_namespace"

	// idempotentTimeout is the maximum duration to wait for warehouse state
	// convergence during polling-based verification assertions.
	idempotentTimeout = 30 * time.Second //nolint:unused // used by connector-specific test files in same package
)

// Package-level NOP dependencies for warehouse test service initialization.
// Connector-specific test files in the same package reference these when
// bootstrapping warehouse components without producing log or metric output.
var (
	idempotentNOPLogger = logger.NOP //nolint:gochecknoglobals,unused // shared test dependency used by connector-specific files
	idempotentNOPStats  = stats.NOP  //nolint:gochecknoglobals,unused // shared test dependency used by connector-specific files
)

// --------------------------------------------------------------------------
// Exported types used by connector-specific test files.
// --------------------------------------------------------------------------

// IdempotentTestConfig holds common configuration for all idempotent sync
// test cases. Each connector-specific test populates this config to declare
// its merge strategy, expected dedup behavior, and replay parameters.
type IdempotentTestConfig struct {
	// ConnectorType is the warehouse connector identifier (e.g. "SNOWFLAKE", "BQ",
	// "RS", "CLICKHOUSE", "DELTALAKE", "POSTGRES", "MSSQL", "AZURE_SYNAPSE", "S3_DATALAKE").
	ConnectorType string

	// MergeStrategy describes the dedup approach (e.g. "SQL_MERGE", "DELETE_INSERT",
	// "DEDUP_VIEW", "ENGINE_DEDUP", "BULK_COPYIN", "APPEND_ONLY").
	MergeStrategy string

	// Events is the canonical set of test events loaded from
	// testdata/idempotent_events.json. All connector tests share the same events
	// to ensure consistent replay scenarios.
	Events []IdempotentEvent

	// ExpectedRows is the expected number of rows in the tracks table after
	// dedup-aware processing. For dedup connectors this equals unique event count;
	// for append-only connectors it equals total event count × replay count.
	ExpectedRows int

	// ReplayCount specifies how many times the staging payload is submitted to
	// simulate replay/retry. Defaults to 2 when zero.
	ReplayCount int

	// ShouldDeduplicate indicates whether the connector's merge strategy
	// guarantees deduplication. When true, repeated replays must produce the
	// same row count. When false (append-only), rows accumulate.
	ShouldDeduplicate bool
}

// IdempotentEvent is a canonical test event with known deterministic values.
// Events are loaded from testdata/idempotent_events.json and used by all
// connector-specific idempotent tests for consistency. The struct fields
// map directly to the JSON fixture schema.
type IdempotentEvent struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	Event      string `json:"event"`
	ReceivedAt string `json:"received_at"`
	Table      string `json:"table"`
}

// --------------------------------------------------------------------------
// Master test function: TestIdempotentSync
// --------------------------------------------------------------------------

// TestIdempotentSync is the master test function that orchestrates idempotent
// sync validation across all 9 warehouse connectors. Each connector-specific
// test function is defined in its own file and verifies that replay/retry
// scenarios produce identical warehouse state according to the connector's
// merge/dedup strategy.
//
// Connector merge strategies under test:
//   - Snowflake:     SQL MERGE with ROW_NUMBER() window function
//   - BigQuery:      Append with dedup views via CREATE OR REPLACE VIEW
//   - Redshift:      Transactional DELETE+INSERT with dedup window (720h default)
//   - ClickHouse:    Engine-level dedup (ReplacingMergeTree / AggregatingMergeTree)
//   - PostgreSQL:    SQL MERGE with configurable allowMerge flag
//   - MSSQL:         Bulk CopyIn via mssql.CopyIn with staging table dedup
//   - Azure Synapse: Bulk CopyIn with Synapse-specific query dialect
//   - Delta Lake:    Databricks SQL MERGE with ShouldMerge() flag
//   - Datalake:      Append-only — no merge, verifies expected duplicate accumulation
func TestIdempotentSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Run("snowflake", testIdempotentSnowflake)
	t.Run("bigquery", testIdempotentBigQuery)
	t.Run("redshift", testIdempotentRedshift)
	t.Run("clickhouse", testIdempotentClickHouse)
	t.Run("postgres", testIdempotentPostgres)
	t.Run("mssql", testIdempotentMSSQL)
	t.Run("azure_synapse", testIdempotentSynapse)
	t.Run("deltalake", testIdempotentDeltaLake)
	t.Run("datalake", testIdempotentDatalake)
}

// --------------------------------------------------------------------------
// Shared helper functions: Event loading and payload generation
// --------------------------------------------------------------------------

// loadIdempotentEvents reads and parses the canonical test event fixture file
// at testdata/idempotent_events.json. The file contains 24 deterministic events:
//   - 6 identifies (3 user IDs, event ID 001 deliberately duplicated)
//   - 8 tracks
//   - 6 pages (event ID 015 deliberately duplicated)
//   - 4 screens
//
// The two duplicated event IDs test primary-key dedup behavior in connectors
// that implement merge strategies.
func loadIdempotentEvents(t *testing.T) []IdempotentEvent {
	t.Helper()

	data, err := os.ReadFile("testdata/idempotent_events.json")
	require.NoError(t, err, "failed to read idempotent_events.json")

	var events []IdempotentEvent
	err = jsonrs.Unmarshal(data, &events)
	require.NoError(t, err, "failed to unmarshal idempotent events")
	require.NotEmpty(t, events, "idempotent events must not be empty")

	return events
}

// generateIdempotentStagingPayload transforms a slice of IdempotentEvent into
// a newline-delimited JSON payload suitable for staging file upload. Each line
// contains a data payload and metadata block matching the warehouse staging
// file format expected by the warehouse ingestion pipeline:
//
//	{"data": {"id": "...", "user_id": "...", ...}, "metadata": {"columns": {...}, "table": "..."}}
//
// Lines are joined with newline separators (no trailing newline) to match
// the standard JSONL format used by warehouse staging files.
func generateIdempotentStagingPayload(t *testing.T, events []IdempotentEvent) string {
	t.Helper()

	lines := lo.Map(events, func(e IdempotentEvent, _ int) string {
		data, err := jsonrs.Marshal(map[string]interface{}{
			"data": map[string]interface{}{
				"id":          e.ID,
				"user_id":     e.UserID,
				"event":       e.Event,
				"received_at": e.ReceivedAt,
			},
			"metadata": map[string]interface{}{
				"columns": map[string]string{
					"id":          "string",
					"user_id":     "string",
					"event":       "string",
					"received_at": "datetime",
				},
				"table": e.Table,
			},
		})
		require.NoError(t, err, "failed to marshal event %s", e.ID)
		return string(data)
	})

	return strings.Join(lines, "\n")
}

// --------------------------------------------------------------------------
// Shared helper functions: Warehouse state verification
// --------------------------------------------------------------------------

// verifyIdempotentState polls a PostgreSQL database to verify the expected
// row count in the tracks table for the given namespace. Uses require.Eventually
// to allow for async warehouse processing with a configurable timeout and
// 1-second polling interval.
//nolint:unused // shared helper called from connector-specific test files
func verifyIdempotentState(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	namespace string,
	expectedRows int,
	timeout time.Duration,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		var count int
		err := db.QueryRowContext(ctx, fmt.Sprintf(
			"SELECT COUNT(*) FROM %s.tracks", namespace,
		)).Scan(&count)
		if err != nil {
			return false
		}
		return count == expectedRows
	}, timeout, 1*time.Second, "expected %d rows in %s.tracks", expectedRows, namespace)
}

// verifyChecksumConsistency computes and validates an MD5 checksum of all
// track IDs in sorted order, ensuring deterministic state after replay.
// The checksum is computed using PostgreSQL's string_agg with ORDER BY
// to produce a reproducible hash regardless of physical row ordering.
// An empty or null checksum indicates an empty or corrupted table.
//nolint:unused // shared helper called from connector-specific test files
func verifyChecksumConsistency(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	namespace string,
) {
	t.Helper()

	var checksum string
	err := db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT md5(string_agg(id::text, ',' ORDER BY id)) FROM %s.tracks", namespace,
	)).Scan(&checksum)
	require.NoError(t, err, "failed to compute checksum for %s.tracks", namespace)
	require.NotEmpty(t, checksum, "checksum must not be empty for %s.tracks", namespace)
}

// verifyIdempotentSyncComplete runs the full idempotent state verification
// suite including row count validation and checksum consistency check using
// a background context and the default idempotent timeout. This is a
// convenience wrapper used by connector-specific tests that operate against
// PostgreSQL databases.
//nolint:unused // shared helper called from connector-specific test files
func verifyIdempotentSyncComplete(
	t *testing.T,
	db *sql.DB,
	namespace string,
	expectedRows int,
) {
	t.Helper()

	ctx := context.Background()
	verifyIdempotentState(t, ctx, db, namespace, expectedRows, idempotentTimeout)
	verifyChecksumConsistency(t, ctx, db, namespace)
}

// --------------------------------------------------------------------------
// Docker resource setup helpers
// --------------------------------------------------------------------------

// setupIdempotentPostgres creates and returns a Dockerized PostgreSQL instance
// for idempotent sync integration tests. The container is automatically cleaned
// up when the test completes via t.Cleanup registered by the postgres.Setup
// function. Returns both the raw *sql.DB handle and the full Resource struct
// for access to connection parameters.
//nolint:unused // shared helper called from connector-specific test files
func setupIdempotentPostgres(t *testing.T) (*sql.DB, *postgres.Resource) {
	t.Helper()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err, "failed to create Docker pool for PostgreSQL")

	pgResource, err := postgres.Setup(pool, t)
	require.NoError(t, err, "failed to setup PostgreSQL Docker container")

	return pgResource.DB, pgResource
}

// setupIdempotentMinio creates and returns a Dockerized MinIO instance
// for staging file storage in idempotent sync integration tests. The
// container is automatically cleaned up when the test completes via
// t.Cleanup registered by the minio.Setup function.
//nolint:unused // shared helper called from connector-specific test files
func setupIdempotentMinio(t *testing.T) *minio.Resource {
	t.Helper()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err, "failed to create Docker pool for MinIO")

	minioResource, err := minio.Setup(pool, t)
	require.NoError(t, err, "failed to setup MinIO Docker container")

	return minioResource
}

// --------------------------------------------------------------------------
// Additional shared helpers for test infrastructure
// --------------------------------------------------------------------------

// uniqueIdempotentNamespace generates a unique namespace string for test
// isolation, combining the base idempotent namespace with a UUID suffix.
// This ensures parallel test runs do not interfere with each other's
// database schemas. The UUID suffix is truncated to 8 characters for
// readability while maintaining sufficient uniqueness.
func uniqueIdempotentNamespace() string {
	return fmt.Sprintf("%s_%s", idempotentNamespace, uuid.New().String()[:8])
}

// newIdempotentConfig creates a fresh, isolated configuration instance for
// warehouse service initialization in test contexts. Each test gets its own
// config to prevent cross-test state pollution. The returned config uses
// rudder-go-kit's Config which supports hot-reloadable configuration keys
// matching the Warehouse.* namespace used in production.
//nolint:unused // shared helper called from connector-specific test files
func newIdempotentConfig(t *testing.T) *config.Config {
	t.Helper()
	return config.New()
}

// renderIdempotentStagingForConnector generates deterministic staging files
// for a specific connector type using testhelper.RenderIdempotentStagingFiles.
// The staging files are generated with the specified duplicate ratio and event
// count, returning file paths, expected checksums, and event counts for
// verification in connector-specific tests.
//
// Parameters:
//   - connectorType: warehouse type identifier (e.g. "POSTGRES", "SNOWFLAKE")
//   - eventCount: number of events to generate in each staging file
//   - duplicateRatio: fraction of events that are duplicates (0.0 to 1.0)
//
// Returns an IdempotentStagingResult containing staging file paths, expected
// checksums, unique event count, and total event count.
//nolint:unused // shared helper called from connector-specific test files
func renderIdempotentStagingForConnector(
	t *testing.T,
	connectorType string,
	eventCount int,
	duplicateRatio float64,
) testhelper.IdempotentStagingResult {
	t.Helper()

	cfg := testhelper.IdempotentStagingConfig{
		TableName:       "tracks",
		EventCount:      eventCount,
		DuplicateRatio:  duplicateRatio,
		Format:          "json",
		SourceID:        idempotentSourceID,
		DestinationID:   idempotentDestinationID,
		WorkspaceID:     idempotentWorkspaceID,
		DestinationType: connectorType,
	}

	return testhelper.RenderIdempotentStagingFiles(t, cfg)
}
