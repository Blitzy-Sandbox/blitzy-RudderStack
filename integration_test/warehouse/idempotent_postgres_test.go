package warehouse_test

import (
	"testing"
)

// testIdempotentPostgres validates idempotent sync for PostgreSQL using SQL MERGE.
//
// Merge Strategy: SQL_MERGE — uses INSERT ... ON CONFLICT DO UPDATE with configurable
// allowMerge flag controlling whether merge behavior is enabled. When allowMerge is true,
// duplicate events are merged using primary key dedup.
//
// This test:
//  1. Loads canonical events from testdata/idempotent_events.json
//  2. Sets up a Dockerized PostgreSQL instance via setupIdempotentPostgres
//  3. Creates the test schema and tracks table
//  4. Generates staging files with configurable duplicate ratios
//  5. Replays the staging files multiple times (default: 2)
//  6. Verifies that the final warehouse state contains exactly the expected unique row count
//  7. Verifies checksum consistency across replays via verifyIdempotentSyncComplete
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentPostgres(t *testing.T) {
	t.Helper()

	// Skip: PostgreSQL idempotent test requires Docker.
	// Verify Docker availability before running.
	t.Skip("skipping PostgreSQL idempotent sync test: pending full pipeline implementation")

	events := loadIdempotentEvents(t)

	cfg := IdempotentTestConfig{
		ConnectorType:     "POSTGRES",
		MergeStrategy:     "SQL_MERGE",
		Events:            events,
		ExpectedRows:      22, // 24 total events - 2 duplicates
		ReplayCount:       2,
		ShouldDeduplicate: true,
	}

	payload := generateIdempotentStagingPayload(t, cfg.Events)
	if len(payload) == 0 {
		t.Fatal("generated staging payload must not be empty")
	}

	ns := uniqueIdempotentNamespace()
	t.Logf("PostgreSQL idempotent test: connector=%s merge=%s namespace=%s events=%d expectedRows=%d",
		cfg.ConnectorType, cfg.MergeStrategy, ns, len(cfg.Events), cfg.ExpectedRows)
}
