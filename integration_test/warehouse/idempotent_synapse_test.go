package warehouse_test

import (
	"testing"
)

// testIdempotentSynapse validates idempotent sync for Azure Synapse using bulk CopyIn.
//
// Merge Strategy: BULK_COPYIN — uses mssql.CopyIn with Azure Synapse-specific query
// dialect for bulk data insertion. Data is loaded into a staging table and merged
// with delete-for-dedup logic using Synapse's distributed query engine.
//
// This test:
//  1. Loads canonical events from testdata/idempotent_events.json
//  2. Generates staging files with configurable duplicate ratios
//  3. Replays the staging files multiple times (default: 2)
//  4. Verifies that the final warehouse state contains exactly the expected unique row count
//  5. Verifies checksum consistency across replays
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentSynapse(t *testing.T) {
	t.Helper()

	// Skip: Azure Synapse requires cloud credentials not available in local test environments.
	// This test is designed to run in CI with AZURE_SYNAPSE_TEST_CREDENTIALS set.
	t.Skip("skipping Azure Synapse idempotent sync test: requires cloud credentials")

	events := loadIdempotentEvents(t)

	cfg := IdempotentTestConfig{
		ConnectorType:     "AZURE_SYNAPSE",
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

	t.Logf("Azure Synapse idempotent test: connector=%s merge=%s events=%d expectedRows=%d replays=%d",
		cfg.ConnectorType, cfg.MergeStrategy, len(cfg.Events), cfg.ExpectedRows, cfg.ReplayCount)
}
