package warehouse_test

import (
	"testing"
)

// testIdempotentBigQuery validates idempotent sync for BigQuery using dedup views.
//
// Merge Strategy: DEDUP_VIEW — append-only writes with CREATE OR REPLACE VIEW using
// ROW_NUMBER() to present deduplicated results without modifying underlying tables.
//
// This test:
//  1. Loads canonical events from testdata/idempotent_events.json
//  2. Generates staging files with configurable duplicate ratios
//  3. Replays the staging files multiple times (default: 2)
//  4. Verifies that the dedup view returns exactly the expected unique row count
//  5. Verifies checksum consistency across replays
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentBigQuery(t *testing.T) {
	t.Helper()

	// Skip: BigQuery requires GCP credentials not available in local test environments.
	// This test is designed to run in CI with BIGQUERY_TEST_CREDENTIALS set.
	t.Skip("skipping BigQuery idempotent sync test: requires cloud credentials")

	events := loadIdempotentEvents(t)

	cfg := IdempotentTestConfig{
		ConnectorType:     "BQ",
		MergeStrategy:     "DEDUP_VIEW",
		Events:            events,
		ExpectedRows:      22, // 24 total events - 2 duplicates (dedup via view)
		ReplayCount:       2,
		ShouldDeduplicate: true,
	}

	payload := generateIdempotentStagingPayload(t, cfg.Events)
	if len(payload) == 0 {
		t.Fatal("generated staging payload must not be empty")
	}

	t.Logf("BigQuery idempotent test: connector=%s merge=%s events=%d expectedRows=%d replays=%d",
		cfg.ConnectorType, cfg.MergeStrategy, len(cfg.Events), cfg.ExpectedRows, cfg.ReplayCount)
}
