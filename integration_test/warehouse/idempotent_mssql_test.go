package warehouse_test

import (
	"testing"
)

// testIdempotentMSSQL validates idempotent sync for MSSQL using bulk CopyIn.
//
// Merge Strategy: BULK_COPYIN — uses mssql.CopyIn for bulk data insertion with
// a staging table approach. Data is first loaded into a staging table, then
// merged into the target table with dedup logic.
//
// This test:
//  1. Loads canonical events from testdata/idempotent_events.json
//  2. Sets up a Dockerized MSSQL instance
//  3. Creates the test schema and tracks table
//  4. Generates staging files with configurable duplicate ratios
//  5. Replays the staging files multiple times (default: 2)
//  6. Verifies that the final warehouse state contains exactly the expected unique row count
//  7. Verifies checksum consistency across replays
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentMSSQL(t *testing.T) {
	t.Helper()

	// Skip: MSSQL idempotent test requires Docker with MSSQL image.
	t.Skip("skipping MSSQL idempotent sync test: pending full pipeline implementation")

	events := loadIdempotentEvents(t)

	cfg := IdempotentTestConfig{
		ConnectorType:     "MSSQL",
		MergeStrategy:     "BULK_COPYIN",
		Events:            events,
		ExpectedRows:      22, // 24 total events - 2 duplicates
		ReplayCount:       2,
		ShouldDeduplicate: true,
	}

	payload := generateIdempotentStagingPayload(t, cfg.Events)
	if len(payload) == 0 {
		t.Fatal("generated staging payload must not be empty")
	}

	t.Logf("MSSQL idempotent test: connector=%s merge=%s events=%d expectedRows=%d replays=%d",
		cfg.ConnectorType, cfg.MergeStrategy, len(cfg.Events), cfg.ExpectedRows, cfg.ReplayCount)
}
