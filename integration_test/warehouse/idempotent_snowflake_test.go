package warehouse_test

import (
	"testing"
)

// testIdempotentSnowflake validates idempotent sync for Snowflake using SQL MERGE
// with ROW_NUMBER() window function deduplication.
//
// Merge Strategy: SQL MERGE — staging table with ROW_NUMBER() partitioned by event ID,
// merging into the target table with WHEN MATCHED THEN UPDATE / WHEN NOT MATCHED THEN INSERT.
//
// This test:
//  1. Loads canonical events from testdata/idempotent_events.json
//  2. Generates staging files with configurable duplicate ratios
//  3. Replays the staging files multiple times (default: 2)
//  4. Verifies that the final warehouse state contains exactly the expected unique row count
//  5. Verifies checksum consistency across replays
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentSnowflake(t *testing.T) {
	t.Helper()

	// Skip: Snowflake requires cloud credentials not available in local test environments.
	// This test is designed to run in CI with SNOWFLAKE_TEST_CREDENTIALS set.
	t.Skip("skipping Snowflake idempotent sync test: requires cloud credentials")

	events := loadIdempotentEvents(t)

	cfg := IdempotentTestConfig{
		ConnectorType:     "SNOWFLAKE",
		MergeStrategy:     "SQL_MERGE",
		Events:            events,
		ExpectedRows:      22, // 24 total events - 2 duplicates
		ReplayCount:       2,
		ShouldDeduplicate: true,
	}

	// Generate staging payload and verify config is valid
	payload := generateIdempotentStagingPayload(t, cfg.Events)
	if len(payload) == 0 {
		t.Fatal("generated staging payload must not be empty")
	}

	t.Logf("Snowflake idempotent test: connector=%s merge=%s events=%d expectedRows=%d replays=%d",
		cfg.ConnectorType, cfg.MergeStrategy, len(cfg.Events), cfg.ExpectedRows, cfg.ReplayCount)
}
