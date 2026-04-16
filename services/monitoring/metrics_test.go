// Package monitoring_test provides unit tests for the per-destination delivery
// monitoring metrics defined in services/monitoring/metrics.go. Tests cover
// metric registration (idempotency), all five recording helpers (RecordDelivery,
// RecordFailure, RecordLatency, RecordRetry, RecordCircuitBreakerState), tag
// isolation across destinations, and concurrent access safety.
//
// The test approach uses memstats.Store from rudder-go-kit to capture metrics
// in memory, following the stats.Default swap pattern established in
// router/batchrouter/handle_observability_test.go.
package monitoring_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	"github.com/rudderlabs/rudder-server/services/monitoring"
)

// Test fixture constants following the services/rmetrics/pending_events_test.go
// pattern of declaring string constants for reproducible test data.
const (
	testDestID1   = "dest-001"
	testDestID2   = "dest-002"
	testDestType1 = "WEBHOOK"
	testDestType2 = "S3"
)

// swapStats replaces stats.Default with a fresh memstats.Store and registers a
// t.Cleanup callback that restores the original value when the test or subtest
// completes. The returned store captures every stat recorded via stats.Default
// so tests can inspect metric values, tags, and durations.
//
// Tests that call swapStats MUST NOT use t.Parallel because stats.Default is a
// package-level global variable.
func swapStats(t *testing.T) *memstats.Store {
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

// ---------------------------------------------------------------------------
// Phase 1: Metric Registration Tests
// ---------------------------------------------------------------------------

// TestRegisterMetrics verifies that RegisterMetrics completes without error and
// is safe to call multiple times (idempotent via sync.Once).
func TestRegisterMetrics(t *testing.T) {
	t.Run("first call completes without panic", func(t *testing.T) {
		require.NotPanics(t, func() {
			monitoring.RegisterMetrics()
		})
	})

	t.Run("idempotent second call", func(t *testing.T) {
		require.NotPanics(t, func() {
			monitoring.RegisterMetrics()
		})
	})

	t.Run("idempotent third call", func(t *testing.T) {
		require.NotPanics(t, func() {
			monitoring.RegisterMetrics()
		})
	})
}

// ---------------------------------------------------------------------------
// Phase 2: Recording Helper Function Tests
// ---------------------------------------------------------------------------

// TestRecordDelivery verifies that RecordDelivery correctly increments the
// delivery_success_total counter with per-destination tags.
func TestRecordDelivery(t *testing.T) {
	t.Run("single delivery increments counter", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordDelivery(testDestID1, testDestType1)

		m := store.Get(monitoring.DeliverySuccessTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())
	})

	t.Run("multiple deliveries accumulate", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordDelivery(testDestID1, testDestType1)
		monitoring.RecordDelivery(testDestID1, testDestType1)
		monitoring.RecordDelivery(testDestID1, testDestType1)

		m := store.Get(monitoring.DeliverySuccessTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 3, m.LastValue())
	})

	t.Run("different destination IDs are isolated", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordDelivery(testDestID1, testDestType1)
		monitoring.RecordDelivery(testDestID2, testDestType2)

		m1 := store.Get(monitoring.DeliverySuccessTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		m2 := store.Get(monitoring.DeliverySuccessTotal, stats.Tags{
			"destType":      testDestType2,
			"destinationId": testDestID2,
		})
		require.NotNil(t, m1)
		require.NotNil(t, m2)
		require.EqualValues(t, 1, m1.LastValue())
		require.EqualValues(t, 1, m2.LastValue())
	})

	t.Run("empty destination ID sanitized to unknown", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordDelivery("", testDestType1)

		m := store.Get(monitoring.DeliverySuccessTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": "unknown",
		})
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())
	})

	t.Run("empty destination type sanitized to unknown", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordDelivery(testDestID1, "")

		m := store.Get(monitoring.DeliverySuccessTotal, stats.Tags{
			"destType":      "unknown",
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())
	})
}

// TestRecordFailure verifies that RecordFailure correctly increments the
// delivery_failure_total counter with per-destination tags.
func TestRecordFailure(t *testing.T) {
	t.Run("single failure increments counter", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordFailure(testDestID1, testDestType1)

		m := store.Get(monitoring.DeliveryFailureTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())
	})

	t.Run("multiple failures accumulate", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordFailure(testDestID1, testDestType1)
		monitoring.RecordFailure(testDestID1, testDestType1)

		m := store.Get(monitoring.DeliveryFailureTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 2, m.LastValue())
	})

	t.Run("different destinations isolated", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordFailure(testDestID1, testDestType1)
		monitoring.RecordFailure(testDestID2, testDestType2)

		m1 := store.Get(monitoring.DeliveryFailureTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		m2 := store.Get(monitoring.DeliveryFailureTotal, stats.Tags{
			"destType":      testDestType2,
			"destinationId": testDestID2,
		})
		require.NotNil(t, m1)
		require.NotNil(t, m2)
		require.EqualValues(t, 1, m1.LastValue())
		require.EqualValues(t, 1, m2.LastValue())
	})

	t.Run("empty tags sanitized to unknown", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordFailure("", "")

		m := store.Get(monitoring.DeliveryFailureTotal, stats.Tags{
			"destType":      "unknown",
			"destinationId": "unknown",
		})
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())
	})
}

// TestRecordLatency verifies that RecordLatency correctly records timer
// durations for the delivery_latency_seconds metric.
func TestRecordLatency(t *testing.T) {
	t.Run("normal duration recorded", func(t *testing.T) {
		store := swapStats(t)
		latency := 150 * time.Millisecond
		monitoring.RecordLatency(testDestID1, testDestType1, latency)

		m := store.Get(monitoring.DeliveryLatencySeconds, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		durations := m.Durations()
		require.Equal(t, 1, len(durations))
		require.Equal(t, latency, durations[0])
	})

	t.Run("zero duration edge case", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordLatency(testDestID1, testDestType1, 0)

		m := store.Get(monitoring.DeliveryLatencySeconds, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.Equal(t, time.Duration(0), m.LastDuration())
	})

	t.Run("very large duration edge case", func(t *testing.T) {
		store := swapStats(t)
		latency := 300 * time.Second
		monitoring.RecordLatency(testDestID1, testDestType1, latency)

		m := store.Get(monitoring.DeliveryLatencySeconds, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.Equal(t, latency, m.LastDuration())
	})

	t.Run("multiple latencies recorded in order", func(t *testing.T) {
		store := swapStats(t)
		expected := []time.Duration{
			10 * time.Millisecond,
			200 * time.Millisecond,
			3 * time.Second,
		}
		for _, d := range expected {
			monitoring.RecordLatency(testDestID1, testDestType1, d)
		}

		m := store.Get(monitoring.DeliveryLatencySeconds, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.Equal(t, expected, m.Durations())
	})
}

// TestRecordRetry verifies that RecordRetry correctly increments the
// delivery_retry_total counter with per-destination tags.
func TestRecordRetry(t *testing.T) {
	t.Run("single retry increments counter", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordRetry(testDestID1, testDestType1)

		m := store.Get(monitoring.DeliveryRetryTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())
	})

	t.Run("multiple retries accumulate", func(t *testing.T) {
		store := swapStats(t)
		for i := 0; i < 5; i++ {
			monitoring.RecordRetry(testDestID1, testDestType1)
		}

		m := store.Get(monitoring.DeliveryRetryTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 5, m.LastValue())
	})

	t.Run("per-destination isolation", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordRetry(testDestID1, testDestType1)
		monitoring.RecordRetry(testDestID1, testDestType1)
		monitoring.RecordRetry(testDestID2, testDestType2)

		m1 := store.Get(monitoring.DeliveryRetryTotal, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		m2 := store.Get(monitoring.DeliveryRetryTotal, stats.Tags{
			"destType":      testDestType2,
			"destinationId": testDestID2,
		})
		require.NotNil(t, m1)
		require.NotNil(t, m2)
		require.EqualValues(t, 2, m1.LastValue())
		require.EqualValues(t, 1, m2.LastValue())
	})
}

// TestRecordCircuitBreakerState verifies that RecordCircuitBreakerState
// correctly sets the circuit_breaker_state gauge for each destination.
func TestRecordCircuitBreakerState(t *testing.T) {
	t.Run("closed state (0)", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 0)

		m := store.Get(monitoring.CircuitBreakerState, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 0, m.LastValue())
	})

	t.Run("open state (1)", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 1)

		m := store.Get(monitoring.CircuitBreakerState, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 1, m.LastValue())
	})

	t.Run("half-open state (2)", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 2)

		m := store.Get(monitoring.CircuitBreakerState, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		require.EqualValues(t, 2, m.LastValue())
	})

	t.Run("state transitions overwrite previous value (gauge behavior)", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 0) // closed
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 1) // open
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 2) // half-open
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 0) // back to closed

		m := store.Get(monitoring.CircuitBreakerState, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		require.NotNil(t, m)
		// Gauge always reflects the most recently set value
		require.EqualValues(t, 0, m.LastValue())
	})

	t.Run("different destinations have independent states", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 1) // open
		monitoring.RecordCircuitBreakerState(testDestID2, testDestType2, 0) // closed

		m1 := store.Get(monitoring.CircuitBreakerState, stats.Tags{
			"destType":      testDestType1,
			"destinationId": testDestID1,
		})
		m2 := store.Get(monitoring.CircuitBreakerState, stats.Tags{
			"destType":      testDestType2,
			"destinationId": testDestID2,
		})
		require.NotNil(t, m1)
		require.NotNil(t, m2)
		require.EqualValues(t, 1, m1.LastValue())
		require.EqualValues(t, 0, m2.LastValue())
	})
}

// ---------------------------------------------------------------------------
// Phase 3: Metric Tagging Tests
// ---------------------------------------------------------------------------

// TestMetricTags verifies that each recording helper produces metrics with the
// expected tag keys ("destType" and "destinationId") and that metrics for
// different destinations are stored separately.
func TestMetricTags(t *testing.T) {
	t.Run("delivery success tags include destType and destinationId", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordDelivery(testDestID1, testDestType1)

		metrics := store.GetByName(monitoring.DeliverySuccessTotal)
		require.Equal(t, 1, len(metrics))
		require.Equal(t, testDestType1, metrics[0].Tags["destType"])
		require.Equal(t, testDestID1, metrics[0].Tags["destinationId"])
	})

	t.Run("delivery failure tags include destType and destinationId", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordFailure(testDestID1, testDestType1)

		metrics := store.GetByName(monitoring.DeliveryFailureTotal)
		require.Equal(t, 1, len(metrics))
		require.Equal(t, testDestType1, metrics[0].Tags["destType"])
		require.Equal(t, testDestID1, metrics[0].Tags["destinationId"])
	})

	t.Run("latency tags include destType and destinationId", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordLatency(testDestID1, testDestType1, 100*time.Millisecond)

		metrics := store.GetByName(monitoring.DeliveryLatencySeconds)
		require.Equal(t, 1, len(metrics))
		require.Equal(t, testDestType1, metrics[0].Tags["destType"])
		require.Equal(t, testDestID1, metrics[0].Tags["destinationId"])
	})

	t.Run("retry tags include destType and destinationId", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordRetry(testDestID1, testDestType1)

		metrics := store.GetByName(monitoring.DeliveryRetryTotal)
		require.Equal(t, 1, len(metrics))
		require.Equal(t, testDestType1, metrics[0].Tags["destType"])
		require.Equal(t, testDestID1, metrics[0].Tags["destinationId"])
	})

	t.Run("circuit breaker tags include destType and destinationId", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordCircuitBreakerState(testDestID1, testDestType1, 0)

		metrics := store.GetByName(monitoring.CircuitBreakerState)
		require.Equal(t, 1, len(metrics))
		require.Equal(t, testDestType1, metrics[0].Tags["destType"])
		require.Equal(t, testDestID1, metrics[0].Tags["destinationId"])
	})

	t.Run("multiple destinations produce separate tag sets", func(t *testing.T) {
		store := swapStats(t)
		monitoring.RecordDelivery(testDestID1, testDestType1)
		monitoring.RecordDelivery(testDestID2, testDestType2)

		metrics := store.GetByName(monitoring.DeliverySuccessTotal)
		require.Equal(t, 2, len(metrics))

		// Build a map from the tag combination to verify both destinations
		found := make(map[string]bool)
		for _, m := range metrics {
			key := m.Tags["destType"] + ":" + m.Tags["destinationId"]
			found[key] = true
		}
		require.Equal(t, true, found[testDestType1+":"+testDestID1])
		require.Equal(t, true, found[testDestType2+":"+testDestID2])
	})
}

// ---------------------------------------------------------------------------
// Phase 4: Concurrent Access Tests
// ---------------------------------------------------------------------------

// TestConcurrentRecording launches multiple goroutines that concurrently call
// RecordDelivery, RecordFailure, RecordLatency, and RecordRetry for the same
// destination. It verifies no race conditions (should pass with -race flag) and
// that final metric values are consistent with total expected counts.
func TestConcurrentRecording(t *testing.T) {
	store := swapStats(t)

	const goroutines = 10
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 4) // 4 metric types recorded concurrently

	// Concurrent RecordDelivery calls
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				monitoring.RecordDelivery(testDestID1, testDestType1)
			}
		}()
	}

	// Concurrent RecordFailure calls
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				monitoring.RecordFailure(testDestID1, testDestType1)
			}
		}()
	}

	// Concurrent RecordLatency calls
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				monitoring.RecordLatency(testDestID1, testDestType1, time.Duration(i)*time.Millisecond)
			}
		}()
	}

	// Concurrent RecordRetry calls
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				monitoring.RecordRetry(testDestID1, testDestType1)
			}
		}()
	}

	wg.Wait()

	// Verify final metric values match expected totals
	expectedCount := float64(goroutines * iterations)

	deliveryM := store.Get(monitoring.DeliverySuccessTotal, stats.Tags{
		"destType":      testDestType1,
		"destinationId": testDestID1,
	})
	require.NotNil(t, deliveryM)
	require.EqualValues(t, expectedCount, deliveryM.LastValue())

	failureM := store.Get(monitoring.DeliveryFailureTotal, stats.Tags{
		"destType":      testDestType1,
		"destinationId": testDestID1,
	})
	require.NotNil(t, failureM)
	require.EqualValues(t, expectedCount, failureM.LastValue())

	retryM := store.Get(monitoring.DeliveryRetryTotal, stats.Tags{
		"destType":      testDestType1,
		"destinationId": testDestID1,
	})
	require.NotNil(t, retryM)
	require.EqualValues(t, expectedCount, retryM.LastValue())

	latencyM := store.Get(monitoring.DeliveryLatencySeconds, stats.Tags{
		"destType":      testDestType1,
		"destinationId": testDestID1,
	})
	require.NotNil(t, latencyM)
	require.Equal(t, goroutines*iterations, len(latencyM.Durations()))
}
