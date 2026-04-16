// Package monitoring_test provides unit tests for DashboardService and
// DashboardHandler defined in services/monitoring/dashboard.go. Tests cover
// service lifecycle (Start/Stop), per-destination metric aggregation, HTTP
// handler response serialisation, destination filtering, and sliding-window
// retention eviction.
//
// This file is part of Sprint 8–10, Epic E-036 (Per-destination delivery
// monitoring). It follows the external test package pattern established in
// services/rmetrics/pending_events_test.go and uses testify/require for
// immediate-failure assertions.
package monitoring_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	"github.com/rudderlabs/rudder-server/services/monitoring"
)

// ---------------------------------------------------------------------------
// Test fixture constants — following services/rmetrics/pending_events_test.go
// pattern of declaring reproducible string constants.
// ---------------------------------------------------------------------------

const (
	testDashDestA       = "dest-A"
	testDashDestB       = "dest-B"
	testDashTypeWebhook = "WEBHOOK"
	testDashTypeS3      = "S3"
)

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// swapStatsForDashboard replaces stats.Default with a fresh memstats.Store and
// registers a t.Cleanup callback that restores the original value when the test
// or subtest completes. This is required because the package-level recording
// helpers in metrics.go call stats.Default.NewTaggedStat().
//
// Tests that call this helper MUST NOT use t.Parallel because stats.Default and
// the package-level defaultDashboard are global variables.
func swapStatsForDashboard(t *testing.T) *memstats.Store {
	t.Helper()
	store, err := memstats.New()
	require.NoError(t, err)
	original := stats.Default
	stats.Default = store
	t.Cleanup(func() {
		stats.Default = original
	})
	return store
}

// newTestService creates a DashboardService with default configuration values
// (10 s refresh interval, 86 400 s retention). It swaps stats.Default so that
// package-level recording helpers work without panicking and registers cleanup
// for the stats swap. The returned service is NOT started — callers must invoke
// Start() if the aggregation loop is needed.
func newTestService(t *testing.T) *monitoring.DashboardService {
	t.Helper()
	swapStatsForDashboard(t)
	conf := config.New()
	return monitoring.NewDashboardService(conf, logger.NOP)
}

// newTestServiceWithShortWindow creates a DashboardService with very short
// aggregation and retention periods for time-sensitive tests (sliding window,
// latency percentile derivation). The refresh interval is 100 ms and the
// retention period is 400 ms, which allows tests to observe eviction within
// roughly 600 ms of wall-clock time.
func newTestServiceWithShortWindow(t *testing.T) *monitoring.DashboardService {
	t.Helper()
	swapStatsForDashboard(t)
	conf := config.New()
	conf.Set("Monitoring.dashboard.refreshInterval", "100ms")
	conf.Set("Monitoring.dashboard.retentionPeriod", "400ms")
	return monitoring.NewDashboardService(conf, logger.NOP)
}

// findDestination locates a DestinationMetrics entry by ID in the response
// slice. Returns nil if not found.
func findDestination(dests []monitoring.DestinationMetrics, id string) *monitoring.DestinationMetrics {
	for i := range dests {
		if dests[i].DestinationID == id {
			return &dests[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Phase 1: DashboardService Lifecycle Tests
// ---------------------------------------------------------------------------

// TestDashboardService_StartStop verifies that the DashboardService can be
// created, started, and stopped without errors or panics. It also ensures the
// service can be started and stopped multiple times to confirm idempotent
// lifecycle behaviour.
func TestDashboardService_StartStop(t *testing.T) {
	t.Run("start and stop without error", func(t *testing.T) {
		ds := newTestService(t)
		ctx := context.Background()
		err := ds.Start(ctx)
		require.NoError(t, err)
		ds.Stop()
	})

	t.Run("start stop multiple cycles", func(t *testing.T) {
		ds := newTestService(t)
		for i := 0; i < 3; i++ {
			ctx := context.Background()
			err := ds.Start(ctx)
			require.NoError(t, err)
			ds.Stop()
		}
	})

	t.Run("stop after context cancellation", func(t *testing.T) {
		ds := newTestService(t)
		ctx, cancel := context.WithCancel(context.Background())
		err := ds.Start(ctx)
		require.NoError(t, err)
		cancel()
		// Allow the aggregation goroutine to observe the cancellation.
		time.Sleep(50 * time.Millisecond)
		ds.Stop()
	})

	t.Run("stop on fresh service is safe", func(t *testing.T) {
		ds := newTestService(t)
		// Stop without ever calling Start — should not panic.
		require.NotPanics(t, func() {
			ds.Stop()
		})
	})
}

// ---------------------------------------------------------------------------
// Phase 2: GetMetrics — Empty State Tests
// ---------------------------------------------------------------------------

// TestDashboardService_GetMetrics_Empty verifies that GetMetrics returns a
// well-formed, non-nil response when no delivery events have been recorded.
func TestDashboardService_GetMetrics_Empty(t *testing.T) {
	ds := newTestService(t)

	t.Run("returns empty destinations slice", func(t *testing.T) {
		resp := ds.GetMetrics()
		require.NotNil(t, resp)
		require.NotNil(t, resp.Destinations)
		require.Len(t, resp.Destinations, 0)
	})

	t.Run("timestamp is populated", func(t *testing.T) {
		before := time.Now()
		resp := ds.GetMetrics()
		after := time.Now()
		require.NotNil(t, resp)
		require.False(t, resp.Timestamp.IsZero(), "Timestamp should not be zero")
		require.True(t, !resp.Timestamp.Before(before) && !resp.Timestamp.After(after),
			"Timestamp should be between before and after query")
	})

	t.Run("filter for non-existent destination returns empty", func(t *testing.T) {
		resp := ds.GetMetrics("non-existent-dest")
		require.NotNil(t, resp)
		require.NotNil(t, resp.Destinations)
		require.Len(t, resp.Destinations, 0)
	})

	t.Run("empty string filter returns empty destinations", func(t *testing.T) {
		resp := ds.GetMetrics("")
		require.NotNil(t, resp)
		require.NotNil(t, resp.Destinations)
		require.Len(t, resp.Destinations, 0)
	})
}

// ---------------------------------------------------------------------------
// Phase 3: Metrics Aggregation Tests
// ---------------------------------------------------------------------------

// TestDashboardService_GetMetrics_AfterRecording verifies that the
// DashboardService correctly aggregates delivery metrics recorded through the
// package-level helper functions (RecordDelivery, RecordFailure, RecordRetry,
// RecordLatency, RecordCircuitBreakerState). Counters are checked immediately
// since they are set synchronously; latency percentiles require an aggregation
// tick so the service is started briefly.
func TestDashboardService_GetMetrics_AfterRecording(t *testing.T) {
	ds := newTestServiceWithShortWindow(t)

	// Record a mix of delivery events for dest-A via the package-level helpers.
	// These write to both Prometheus (stats.Default) and the in-memory
	// dashboard (defaultDashboard) simultaneously.
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordFailure(testDashDestA, testDashTypeWebhook)
	monitoring.RecordRetry(testDashDestA, testDashTypeWebhook)
	monitoring.RecordRetry(testDashDestA, testDashTypeWebhook)
	monitoring.RecordLatency(testDashDestA, testDashTypeWebhook, 100*time.Millisecond)
	monitoring.RecordLatency(testDashDestA, testDashTypeWebhook, 200*time.Millisecond)
	monitoring.RecordLatency(testDashDestA, testDashTypeWebhook, 300*time.Millisecond)
	monitoring.RecordCircuitBreakerState(testDashDestA, testDashTypeWebhook, 0) // closed

	// Start the service so the aggregation loop computes latency percentiles.
	ctx := context.Background()
	require.NoError(t, ds.Start(ctx))
	// Wait for at least two aggregation ticks (100 ms each) to ensure
	// computeDerivedMetrics has run.
	time.Sleep(250 * time.Millisecond)

	t.Run("success count matches recorded deliveries", func(t *testing.T) {
		resp := ds.GetMetrics(testDashDestA)
		require.Len(t, resp.Destinations, 1)
		require.Equal(t, int64(3), resp.Destinations[0].SuccessCount)
	})

	t.Run("failure count matches recorded failures", func(t *testing.T) {
		resp := ds.GetMetrics(testDashDestA)
		require.Len(t, resp.Destinations, 1)
		require.Equal(t, int64(1), resp.Destinations[0].FailureCount)
	})

	t.Run("retry count matches recorded retries", func(t *testing.T) {
		resp := ds.GetMetrics(testDashDestA)
		require.Len(t, resp.Destinations, 1)
		require.Equal(t, int64(2), resp.Destinations[0].RetryCount)
	})

	t.Run("circuit breaker state is closed", func(t *testing.T) {
		resp := ds.GetMetrics(testDashDestA)
		require.Len(t, resp.Destinations, 1)
		require.Equal(t, monitoring.CircuitBreakerClosed, resp.Destinations[0].CircuitBreakerState)
	})

	t.Run("destination ID and type are set", func(t *testing.T) {
		resp := ds.GetMetrics(testDashDestA)
		require.Len(t, resp.Destinations, 1)
		m := resp.Destinations[0]
		require.Equal(t, testDashDestA, m.DestinationID)
		require.Equal(t, testDashTypeWebhook, m.DestinationType)
	})

	t.Run("latency percentiles computed after aggregation tick", func(t *testing.T) {
		resp := ds.GetMetrics(testDashDestA)
		require.Len(t, resp.Destinations, 1)
		m := resp.Destinations[0]
		// With observations [100, 200, 300] ms, the percentiles should be:
		//   p50 ≈ 200 ms, p95 ≈ 290–300 ms, p99 ≈ 298–300 ms
		// We use a generous range to account for the nearest-rank interpolation.
		require.True(t, m.LatencyP50Ms >= 100 && m.LatencyP50Ms <= 300,
			"LatencyP50Ms should be between 100 and 300, got %f", m.LatencyP50Ms)
		require.True(t, m.LatencyP95Ms >= 200 && m.LatencyP95Ms <= 300,
			"LatencyP95Ms should be between 200 and 300, got %f", m.LatencyP95Ms)
		require.True(t, m.LatencyP99Ms >= 200 && m.LatencyP99Ms <= 300,
			"LatencyP99Ms should be between 200 and 300, got %f", m.LatencyP99Ms)
	})

	t.Run("timestamp is recent", func(t *testing.T) {
		before := time.Now()
		resp := ds.GetMetrics(testDashDestA)
		require.False(t, resp.Timestamp.IsZero())
		require.True(t, !resp.Timestamp.Before(before.Add(-1*time.Second)),
			"Timestamp should be recent")
	})

	ds.Stop()
}

// TestDashboardService_GetMetrics_PerDestination verifies that metrics are
// isolated per destination — recording for dest-A does not affect dest-B and
// vice versa — and that the variadic destinationID filter works correctly.
func TestDashboardService_GetMetrics_PerDestination(t *testing.T) {
	ds := newTestService(t)

	// Record different volumes for each destination.
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordFailure(testDashDestA, testDashTypeWebhook)

	monitoring.RecordDelivery(testDashDestB, testDashTypeS3)
	monitoring.RecordFailure(testDashDestB, testDashTypeS3)
	monitoring.RecordFailure(testDashDestB, testDashTypeS3)
	monitoring.RecordRetry(testDashDestB, testDashTypeS3)
	monitoring.RecordCircuitBreakerState(testDashDestB, testDashTypeS3, 1) // open

	t.Run("query dest-A only returns dest-A metrics", func(t *testing.T) {
		resp := ds.GetMetrics(testDashDestA)
		require.Len(t, resp.Destinations, 1)
		m := resp.Destinations[0]
		require.Equal(t, testDashDestA, m.DestinationID)
		require.Equal(t, int64(3), m.SuccessCount)
		require.Equal(t, int64(1), m.FailureCount)
		require.Equal(t, int64(0), m.RetryCount)
	})

	t.Run("query dest-B only returns dest-B metrics", func(t *testing.T) {
		resp := ds.GetMetrics(testDashDestB)
		require.Len(t, resp.Destinations, 1)
		m := resp.Destinations[0]
		require.Equal(t, testDashDestB, m.DestinationID)
		require.Equal(t, testDashTypeS3, m.DestinationType)
		require.Equal(t, int64(1), m.SuccessCount)
		require.Equal(t, int64(2), m.FailureCount)
		require.Equal(t, int64(1), m.RetryCount)
		require.Equal(t, monitoring.CircuitBreakerOpen, m.CircuitBreakerState)
	})

	t.Run("query all returns both destinations", func(t *testing.T) {
		resp := ds.GetMetrics()
		require.Len(t, resp.Destinations, 2)

		destA := findDestination(resp.Destinations, testDashDestA)
		require.NotNil(t, destA, "dest-A should be present")
		require.Equal(t, int64(3), destA.SuccessCount)

		destB := findDestination(resp.Destinations, testDashDestB)
		require.NotNil(t, destB, "dest-B should be present")
		require.Equal(t, int64(1), destB.SuccessCount)
	})
}

// ---------------------------------------------------------------------------
// Phase 4: HTTP Handler Tests
// ---------------------------------------------------------------------------

// TestDashboardHandler_Success verifies the DashboardHandler returns HTTP 200
// with a correctly shaped JSON body containing per-destination metrics.
func TestDashboardHandler_Success(t *testing.T) {
	ds := newTestService(t)

	// Pre-record some metrics for the handler to serve.
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordFailure(testDashDestA, testDashTypeWebhook)
	monitoring.RecordRetry(testDashDestA, testDashTypeWebhook)
	monitoring.RecordLatency(testDashDestA, testDashTypeWebhook, 150*time.Millisecond)
	monitoring.RecordCircuitBreakerState(testDashDestA, testDashTypeWebhook, 0)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/monitoring/dashboard", nil)

	ds.DashboardHandler(rr, req)

	t.Run("status code is 200", func(t *testing.T) {
		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("content type is application/json", func(t *testing.T) {
		require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	})

	t.Run("response body decodes to DashboardResponse", func(t *testing.T) {
		var resp monitoring.DashboardResponse
		err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
		require.NoError(t, err)
		require.NotNil(t, resp.Destinations)
		require.Len(t, resp.Destinations, 1)
		require.False(t, resp.Timestamp.IsZero())

		m := resp.Destinations[0]
		require.Equal(t, testDashDestA, m.DestinationID)
		require.Equal(t, int64(2), m.SuccessCount)
		require.Equal(t, int64(1), m.FailureCount)
		require.Equal(t, int64(1), m.RetryCount)
		require.Equal(t, monitoring.CircuitBreakerClosed, m.CircuitBreakerState)
	})
}

// TestDashboardHandler_WithDestinationFilter verifies that the handler
// respects the "destinationId" query parameter and returns only the matching
// destination's metrics.
func TestDashboardHandler_WithDestinationFilter(t *testing.T) {
	ds := newTestService(t)

	// Record metrics for two destinations.
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordDelivery(testDashDestB, testDashTypeS3)

	t.Run("filter returns only matching destination", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/monitoring/dashboard?destinationId="+testDashDestA, nil)
		ds.DashboardHandler(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)

		var resp monitoring.DashboardResponse
		err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
		require.NoError(t, err)
		require.Len(t, resp.Destinations, 1)
		require.Equal(t, testDashDestA, resp.Destinations[0].DestinationID)
		require.Equal(t, int64(2), resp.Destinations[0].SuccessCount)
	})

	t.Run("filter for dest-B returns only dest-B", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/monitoring/dashboard?destinationId="+testDashDestB, nil)
		ds.DashboardHandler(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)

		var resp monitoring.DashboardResponse
		err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
		require.NoError(t, err)
		require.Len(t, resp.Destinations, 1)
		require.Equal(t, testDashDestB, resp.Destinations[0].DestinationID)
		require.Equal(t, int64(1), resp.Destinations[0].SuccessCount)
	})

	t.Run("filter for non-existent destination returns empty", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/monitoring/dashboard?destinationId=non-existent", nil)
		ds.DashboardHandler(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)

		var resp monitoring.DashboardResponse
		err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
		require.NoError(t, err)
		require.NotNil(t, resp.Destinations)
		require.Len(t, resp.Destinations, 0)
	})
}

// TestDashboardHandler_EmptyMetrics verifies that the handler returns HTTP 200
// with an empty (but valid) JSON body when no metrics have been recorded.
func TestDashboardHandler_EmptyMetrics(t *testing.T) {
	ds := newTestService(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/monitoring/dashboard", nil)

	ds.DashboardHandler(rr, req)

	t.Run("status code is 200", func(t *testing.T) {
		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("content type is application/json", func(t *testing.T) {
		require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	})

	t.Run("response body contains empty destinations array", func(t *testing.T) {
		var resp monitoring.DashboardResponse
		err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
		require.NoError(t, err)
		require.NotNil(t, resp.Destinations)
		require.Len(t, resp.Destinations, 0)
		require.False(t, resp.Timestamp.IsZero())
	})
}

// ---------------------------------------------------------------------------
// Phase 5: Sliding Window / Retention Tests
// ---------------------------------------------------------------------------

// TestDashboardService_SlidingWindow verifies that the DashboardService evicts
// stale destination entries after the configured retention period has elapsed.
// It uses a very short retention (400 ms) and refresh interval (100 ms) to
// exercise the cleanup path within a reasonable test duration.
func TestDashboardService_SlidingWindow(t *testing.T) {
	ds := newTestServiceWithShortWindow(t)

	// Record metrics so the dashboard has at least one destination entry.
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordDelivery(testDashDestA, testDashTypeWebhook)
	monitoring.RecordFailure(testDashDestA, testDashTypeWebhook)
	monitoring.RecordLatency(testDashDestA, testDashTypeWebhook, 50*time.Millisecond)

	t.Run("metrics present before expiration", func(t *testing.T) {
		resp := ds.GetMetrics()
		require.Len(t, resp.Destinations, 1)
		require.Equal(t, testDashDestA, resp.Destinations[0].DestinationID)
		require.EqualValues(t, 2, resp.Destinations[0].SuccessCount)
		require.EqualValues(t, 1, resp.Destinations[0].FailureCount)
	})

	// Start the service so the aggregation loop begins running cleanup.
	ctx := context.Background()
	require.NoError(t, ds.Start(ctx))
	defer ds.Stop()

	t.Run("metrics evicted after retention window expires", func(t *testing.T) {
		// Wait longer than the retention period (400 ms) plus at least two
		// aggregation ticks (100 ms each) to ensure cleanup has executed.
		time.Sleep(700 * time.Millisecond)

		resp := ds.GetMetrics()
		require.NotNil(t, resp)
		require.NotNil(t, resp.Destinations)
		require.Len(t, resp.Destinations, 0,
			"stale destination should have been evicted after retention period")
	})

	t.Run("new metrics survive after old ones are evicted", func(t *testing.T) {
		// Record fresh metrics after the old ones have been evicted.
		monitoring.RecordDelivery(testDashDestB, testDashTypeS3)

		resp := ds.GetMetrics()
		require.Len(t, resp.Destinations, 1,
			"newly recorded destination should be present")
		require.Equal(t, testDashDestB, resp.Destinations[0].DestinationID)
		require.EqualValues(t, 1, resp.Destinations[0].SuccessCount)
	})
}
