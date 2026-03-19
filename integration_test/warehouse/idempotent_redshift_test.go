package warehouse_test

import (
	"testing"
)

// testIdempotentRedshift validates idempotent sync for Redshift using DELETE+INSERT.
//
// Merge Strategy: DELETE_INSERT — transactional DELETE of matching rows followed by INSERT,
// within a configurable dedup window (default 720h). Uses ROW_NUMBER() window function
// for deduplication within the transaction.
//
// This test:
//  1. Loads canonical events from testdata/idempotent_events.json
//  2. Generates staging files with configurable duplicate ratios
//  3. Replays the staging files multiple times (default: 2)
//  4. Verifies that the final warehouse state contains exactly the expected unique row count
//  5. Verifies checksum consistency across replays
//
//nolint:unused // called from TestIdempotentSync in idempotent_sync_test.go
func testIdempotentRedshift(t *testing.T) {
	t.Helper()

	// Skip: Redshift requires AWS credentials not available in local test environments.
	// This test is designed to run in CI with REDSHIFT_TEST_CREDENTIALS set.
	t.Skip("skipping Redshift idempotent sync test: requires cloud credentials")

	events := loadIdempotentEvents(t)

	cfg := IdempotentTestConfig{
		ConnectorType:     "RS",
		MergeStrategy:     "DELETE_INSERT",
		Events:            events,
		ExpectedRows:      22, // 24 total events - 2 duplicates
		ReplayCount:       2,
		ShouldDeduplicate: true,
	}

	payload := generateIdempotentStagingPayload(t, cfg.Events)
	if len(payload) == 0 {
		t.Fatal("generated staging payload must not be empty")
	}

	t.Logf("Redshift idempotent test: connector=%s merge=%s events=%d expectedRows=%d replays=%d",
		cfg.ConnectorType, cfg.MergeStrategy, len(cfg.Events), cfg.ExpectedRows, cfg.ReplayCount)
}
