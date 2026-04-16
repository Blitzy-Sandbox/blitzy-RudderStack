package anomalydetection_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-server/processor/anomalydetection"
)

// newTestTracker is a test helper that creates a Tracker with a 1-hour window and
// a high sensitivity threshold (preventing false anomaly flags during counting tests).
func newTestTracker(t *testing.T) *anomalydetection.Tracker {
	t.Helper()
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)
	return tracker
}

// ============================================================================
// Phase 1: Test Frequency Baseline Building
// ============================================================================

func TestTracker_RecordEventOccurrence(t *testing.T) {
	tracker := newTestTracker(t)

	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")

	count := tracker.GetEventCount("source-1", "Order Completed")
	assert.Equal(t, 3, count)
}

func TestTracker_RecordPropertyOccurrence(t *testing.T) {
	tracker := newTestTracker(t)

	tracker.RecordProperty("source-1", "Order Completed", "unexpectedProp")
	tracker.RecordProperty("source-1", "Order Completed", "unexpectedProp")

	count := tracker.GetPropertyCount("source-1", "Order Completed", "unexpectedProp")
	assert.Equal(t, 2, count)
}

func TestTracker_MultipleSources(t *testing.T) {
	tracker := newTestTracker(t)

	for i := 0; i < 3; i++ {
		tracker.RecordEvent("source-1", "Order Completed")
	}
	for i := 0; i < 5; i++ {
		tracker.RecordEvent("source-2", "Order Completed")
	}

	assert.Equal(t, 3, tracker.GetEventCount("source-1", "Order Completed"))
	assert.Equal(t, 5, tracker.GetEventCount("source-2", "Order Completed"))

	// Verify source isolation: source-3 has never been recorded
	assert.Zero(t, tracker.GetEventCount("source-3", "Order Completed"))
}

func TestTracker_MultipleEventNames(t *testing.T) {
	tracker := newTestTracker(t)

	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Page Viewed")
	tracker.RecordEvent("source-1", "Page Viewed")
	tracker.RecordEvent("source-1", "Page Viewed")
	tracker.RecordEvent("source-1", "Button Clicked")

	assert.Equal(t, 2, tracker.GetEventCount("source-1", "Order Completed"))
	assert.Equal(t, 3, tracker.GetEventCount("source-1", "Page Viewed"))
	assert.Equal(t, 1, tracker.GetEventCount("source-1", "Button Clicked"))
	assert.Zero(t, tracker.GetEventCount("source-1", "Unrecorded Event"))
}

func TestTracker_MultipleProperties(t *testing.T) {
	tracker := newTestTracker(t)

	tracker.RecordProperty("source-1", "Order Completed", "price")
	tracker.RecordProperty("source-1", "Order Completed", "price")
	tracker.RecordProperty("source-1", "Order Completed", "currency")
	tracker.RecordProperty("source-1", "Order Completed", "currency")
	tracker.RecordProperty("source-1", "Order Completed", "currency")

	assert.Equal(t, 2, tracker.GetPropertyCount("source-1", "Order Completed", "price"))
	assert.Equal(t, 3, tracker.GetPropertyCount("source-1", "Order Completed", "currency"))
	assert.Zero(t, tracker.GetPropertyCount("source-1", "Order Completed", "unknown"))
}

// ============================================================================
// Phase 2: Test Sliding Window Calculations
// ============================================================================

func TestTracker_SlidingWindowExpiry(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           100 * time.Millisecond,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Record 5 events
	for i := 0; i < 5; i++ {
		tracker.RecordEvent("source-1", "Order Completed")
	}
	require.Equal(t, 5, tracker.GetEventCount("source-1", "Order Completed"))

	// Wait past the 100ms window
	time.Sleep(150 * time.Millisecond)

	// Record 2 more events (old ones should be expired)
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")

	count := tracker.GetEventCount("source-1", "Order Completed")
	assert.Equal(t, 2, count, "only events within the current window should be counted")
}

func TestTracker_SlidingWindowMidExpiry(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           100 * time.Millisecond,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Record 3 events at t=0
	for i := 0; i < 3; i++ {
		tracker.RecordEvent("source-1", "Order Completed")
	}

	// Wait 60ms, then record 2 more
	time.Sleep(60 * time.Millisecond)
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")

	// At this point (~60ms), all 5 should still be in window
	assert.Equal(t, 5, tracker.GetEventCount("source-1", "Order Completed"))

	// Wait another 50ms (total ~110ms from t=0; first batch of 3 should expire)
	time.Sleep(50 * time.Millisecond)

	count := tracker.GetEventCount("source-1", "Order Completed")
	assert.Equal(t, 2, count, "only the second batch should remain within the window")
}

func TestTracker_LongWindow(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Record many events — all should be counted since window is 1 hour
	for i := 0; i < 50; i++ {
		tracker.RecordEvent("source-1", "Order Completed")
	}

	count := tracker.GetEventCount("source-1", "Order Completed")
	assert.Equal(t, 50, count, "all events should be within the 1-hour window")
}

func TestTracker_WindowZero(t *testing.T) {
	// When TimeWindow is zero, NewTracker defaults to 1 hour
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           0,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Record events — they should be kept since the default 1-hour window applies
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")

	count := tracker.GetEventCount("source-1", "Order Completed")
	assert.Equal(t, 3, count, "with zero window defaulting to 1h, events should persist")
}

func TestTracker_PropertySlidingWindowExpiry(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           100 * time.Millisecond,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	tracker.RecordProperty("source-1", "Order Completed", "price")
	tracker.RecordProperty("source-1", "Order Completed", "price")
	tracker.RecordProperty("source-1", "Order Completed", "price")

	require.Equal(t, 3, tracker.GetPropertyCount("source-1", "Order Completed", "price"))

	time.Sleep(150 * time.Millisecond)

	tracker.RecordProperty("source-1", "Order Completed", "price")

	count := tracker.GetPropertyCount("source-1", "Order Completed", "price")
	assert.Equal(t, 1, count, "old property occurrences should be expired")
}

// ============================================================================
// Phase 3: Test Sensitivity Threshold Configuration
// ============================================================================

func TestTracker_IsAnomalous_AboveThreshold(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 5,
	})
	require.NotNil(t, tracker)

	for i := 0; i < 6; i++ {
		tracker.RecordEvent("source-1", "UnknownEvent")
	}

	// 6 occurrences >= threshold 5 → anomalous
	assert.True(t, tracker.IsAnomalous("source-1", "UnknownEvent"))
}

func TestTracker_IsAnomalous_BelowThreshold(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 5,
	})
	require.NotNil(t, tracker)

	tracker.RecordEvent("source-1", "UnknownEvent")
	tracker.RecordEvent("source-1", "UnknownEvent")

	// 2 occurrences < threshold 5 → not anomalous
	assert.False(t, tracker.IsAnomalous("source-1", "UnknownEvent"))
}

func TestTracker_IsAnomalous_AtThreshold(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 5,
	})
	require.NotNil(t, tracker)

	for i := 0; i < 5; i++ {
		tracker.RecordEvent("source-1", "UnknownEvent")
	}

	// Exactly at threshold (5 >= 5) → the implementation uses >= so this is true
	assert.True(t, tracker.IsAnomalous("source-1", "UnknownEvent"),
		"at exactly the threshold, IsAnomalous should return true (>= semantics)")
}

func TestTracker_IsPropertyAnomalous(t *testing.T) {
	t.Run("below threshold", func(t *testing.T) {
		tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
			TimeWindow:           1 * time.Hour,
			SensitivityThreshold: 3,
		})
		assert.NotNil(t, tracker)

		tracker.RecordProperty("source-1", "Order Completed", "unknownProp")
		tracker.RecordProperty("source-1", "Order Completed", "unknownProp")
		assert.False(t, tracker.IsPropertyAnomalous("source-1", "Order Completed", "unknownProp"))
	})

	t.Run("at threshold", func(t *testing.T) {
		tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
			TimeWindow:           1 * time.Hour,
			SensitivityThreshold: 3,
		})
		assert.NotNil(t, tracker)

		for i := 0; i < 3; i++ {
			tracker.RecordProperty("source-1", "Order Completed", "unknownProp")
		}
		// 3 >= 3 → true
		assert.True(t, tracker.IsPropertyAnomalous("source-1", "Order Completed", "unknownProp"))
	})

	t.Run("above threshold", func(t *testing.T) {
		tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
			TimeWindow:           1 * time.Hour,
			SensitivityThreshold: 3,
		})
		assert.NotNil(t, tracker)

		for i := 0; i < 5; i++ {
			tracker.RecordProperty("source-1", "Order Completed", "unknownProp")
		}
		assert.True(t, tracker.IsPropertyAnomalous("source-1", "Order Completed", "unknownProp"))
	})
}

func TestTracker_DefaultThreshold(t *testing.T) {
	// DefaultTrackerConfig returns SensitivityThreshold=1
	cfg := anomalydetection.DefaultTrackerConfig()
	assert.Equal(t, 1, cfg.SensitivityThreshold)
	assert.Equal(t, 1*time.Hour, cfg.TimeWindow)

	tracker := anomalydetection.NewTracker(cfg)
	require.NotNil(t, tracker)

	// With threshold=1, a single occurrence makes it anomalous (1 >= 1)
	tracker.RecordEvent("source-1", "UnknownEvent")
	assert.True(t, tracker.IsAnomalous("source-1", "UnknownEvent"))

	// An event never recorded is NOT anomalous (0 < 1)
	assert.False(t, tracker.IsAnomalous("source-1", "NeverRecorded"))
}

func TestTracker_UpdateThreshold(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 10,
	})
	require.NotNil(t, tracker)

	for i := 0; i < 5; i++ {
		tracker.RecordEvent("source-1", "UnknownEvent")
	}

	// 5 < 10 → not anomalous
	assert.False(t, tracker.IsAnomalous("source-1", "UnknownEvent"))

	// Update threshold to 3
	tracker.SetSensitivityThreshold(3)

	// Now 5 >= 3 → anomalous
	assert.True(t, tracker.IsAnomalous("source-1", "UnknownEvent"))
}

func TestTracker_SetSensitivityThreshold_IgnoresInvalid(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 5,
	})
	require.NotNil(t, tracker)

	// Zero should be ignored
	tracker.SetSensitivityThreshold(0)
	for i := 0; i < 5; i++ {
		tracker.RecordEvent("source-1", "evt")
	}
	assert.True(t, tracker.IsAnomalous("source-1", "evt"),
		"threshold should still be 5 after ignoring zero")

	// Negative should be ignored
	tracker.SetSensitivityThreshold(-1)
	assert.True(t, tracker.IsAnomalous("source-1", "evt"),
		"threshold should still be 5 after ignoring negative")
}

// ============================================================================
// Phase 4: Test Time Window Expiration Handling
// ============================================================================

func TestTracker_ExpiredEntriesCleanup(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           100 * time.Millisecond,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Add many entries
	for i := 0; i < 100; i++ {
		tracker.RecordEvent("source-1", "Order Completed")
	}
	require.Equal(t, 100, tracker.GetEventCount("source-1", "Order Completed"))

	// Wait for expiry
	time.Sleep(150 * time.Millisecond)

	// Add a few new entries
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")

	// Only new entries should be counted (old 100 expired)
	count := tracker.GetEventCount("source-1", "Order Completed")
	assert.Equal(t, 2, count, "expired entries should be cleaned up")
}

func TestTracker_PartialExpiry(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           100 * time.Millisecond,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Batch 1 at t=0
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")

	time.Sleep(60 * time.Millisecond)

	// Batch 2 at t=~60ms
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")

	// At ~60ms, all 5 should be in window
	assert.Equal(t, 5, tracker.GetEventCount("source-1", "Order Completed"))

	// Wait until batch 1 expires but batch 2 is still valid
	time.Sleep(50 * time.Millisecond)

	count := tracker.GetEventCount("source-1", "Order Completed")
	assert.Equal(t, 3, count, "only batch 2 entries should remain after partial expiry")
}

func TestTracker_AllEntriesExpired(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           100 * time.Millisecond,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")
	require.Equal(t, 3, tracker.GetEventCount("source-1", "Order Completed"))

	// Wait for all to expire
	time.Sleep(150 * time.Millisecond)

	count := tracker.GetEventCount("source-1", "Order Completed")
	assert.Zero(t, count, "all entries should be expired")
}

// ============================================================================
// Phase 5: Test Constructor and Configuration
// ============================================================================

func TestNewTracker_DefaultConfig(t *testing.T) {
	cfg := anomalydetection.DefaultTrackerConfig()
	require.Equal(t, 1*time.Hour, cfg.TimeWindow)
	require.Equal(t, 1, cfg.SensitivityThreshold)

	tracker := anomalydetection.NewTracker(cfg)
	require.NotNil(t, tracker)

	// Verify tracker is usable with default config
	tracker.RecordEvent("source-1", "test-event")
	assert.Equal(t, 1, tracker.GetEventCount("source-1", "test-event"))
}

func TestNewTracker_CustomWindow(t *testing.T) {
	t.Run("30 minute window", func(t *testing.T) {
		tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
			TimeWindow:           30 * time.Minute,
			SensitivityThreshold: 5,
		})
		require.NotNil(t, tracker)

		// Record events; with a 30-minute window, all should be within window
		for i := 0; i < 10; i++ {
			tracker.RecordEvent("source-1", "evt")
		}
		assert.Equal(t, 10, tracker.GetEventCount("source-1", "evt"))
	})

	t.Run("5 minute window", func(t *testing.T) {
		tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
			TimeWindow:           5 * time.Minute,
			SensitivityThreshold: 5,
		})
		require.NotNil(t, tracker)

		for i := 0; i < 3; i++ {
			tracker.RecordEvent("source-1", "evt")
		}
		assert.Equal(t, 3, tracker.GetEventCount("source-1", "evt"))
	})
}

func TestNewTracker_CustomThreshold(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 7,
	})
	require.NotNil(t, tracker)

	// 6 occurrences < 7 → not anomalous
	for i := 0; i < 6; i++ {
		tracker.RecordEvent("source-1", "evt")
	}
	assert.False(t, tracker.IsAnomalous("source-1", "evt"))

	// 7th occurrence → 7 >= 7 → anomalous
	tracker.RecordEvent("source-1", "evt")
	assert.True(t, tracker.IsAnomalous("source-1", "evt"))
}

func TestNewTracker_ZeroValuesDefaulted(t *testing.T) {
	// Both zero values should be replaced by NewTracker
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           0,
		SensitivityThreshold: 0,
	})
	require.NotNil(t, tracker)

	// With defaulted threshold=1, a single occurrence should be anomalous
	tracker.RecordEvent("source-1", "evt")
	assert.True(t, tracker.IsAnomalous("source-1", "evt"),
		"defaulted threshold should be 1, so 1 >= 1 is true")

	// With defaulted 1-hour window, events should persist
	assert.Equal(t, 1, tracker.GetEventCount("source-1", "evt"))
}

// ============================================================================
// Phase 6: Test Thread Safety
// ============================================================================

func TestTracker_ConcurrentRecordEvent(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	const goroutines = 10
	const eventsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerGoroutine; i++ {
				tracker.RecordEvent("source-1", "concurrent-event")
			}
		}()
	}

	wg.Wait()

	count := tracker.GetEventCount("source-1", "concurrent-event")
	assert.Equal(t, goroutines*eventsPerGoroutine, count,
		"total count should equal goroutines * eventsPerGoroutine")
}

func TestTracker_ConcurrentRecordAndRead(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	const goroutines = 10
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2) // writers + readers

	// Writers
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				tracker.RecordEvent("source-1", "concurrent-rw-event")
			}
		}()
	}

	// Readers
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				_ = tracker.GetEventCount("source-1", "concurrent-rw-event")
			}
		}()
	}

	wg.Wait()

	// Final count should be goroutines * opsPerGoroutine
	count := tracker.GetEventCount("source-1", "concurrent-rw-event")
	assert.Equal(t, goroutines*opsPerGoroutine, count)
}

func TestTracker_ConcurrentMultipleSources(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	const sources = 5
	const eventsPerSource = 200

	var wg sync.WaitGroup
	wg.Add(sources)

	for s := 0; s < sources; s++ {
		go func(sourceIdx int) {
			defer wg.Done()
			sourceID := "source-" + strings.Repeat("x", sourceIdx+1) // unique per goroutine
			for i := 0; i < eventsPerSource; i++ {
				tracker.RecordEvent(sourceID, "evt")
			}
		}(s)
	}

	wg.Wait()

	// Verify each source has correct independent count
	for s := 0; s < sources; s++ {
		sourceID := "source-" + strings.Repeat("x", s+1)
		count := tracker.GetEventCount(sourceID, "evt")
		assert.Equal(t, eventsPerSource, count,
			"source %s should have exactly %d events", sourceID, eventsPerSource)
	}
}

func TestTracker_ConcurrentIsAnomalous(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 5,
	})
	require.NotNil(t, tracker)

	// Pre-populate with enough events to be anomalous
	for i := 0; i < 10; i++ {
		tracker.RecordEvent("source-1", "anomalous-event")
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				result := tracker.IsAnomalous("source-1", "anomalous-event")
				assert.True(t, result)
			}
		}()
	}

	wg.Wait()
}

// ============================================================================
// Phase 7: Edge Cases
// ============================================================================

func TestTracker_EmptySourceID(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Empty source ID should be accepted (the tracker uses string keys)
	tracker.RecordEvent("", "Order Completed")
	tracker.RecordEvent("", "Order Completed")

	count := tracker.GetEventCount("", "Order Completed")
	assert.Equal(t, 2, count, "empty source ID should work as a valid key")

	// Ensure it does not interfere with a non-empty source
	assert.Zero(t, tracker.GetEventCount("source-1", "Order Completed"))
}

func TestTracker_EmptyEventName(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	tracker.RecordEvent("source-1", "")
	tracker.RecordEvent("source-1", "")
	tracker.RecordEvent("source-1", "")

	count := tracker.GetEventCount("source-1", "")
	assert.Equal(t, 3, count, "empty event name should work as a valid key")

	// Ensure isolation from non-empty event name
	assert.Zero(t, tracker.GetEventCount("source-1", "Order Completed"))
}

func TestTracker_EmptyPropertyName(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	tracker.RecordProperty("source-1", "Order Completed", "")
	tracker.RecordProperty("source-1", "Order Completed", "")

	count := tracker.GetPropertyCount("source-1", "Order Completed", "")
	assert.Equal(t, 2, count, "empty property name should work as a valid key")

	// Implementation detail: RecordProperty with empty propertyName and RecordEvent share
	// the same internal tracking key (both use propertyName=""), so GetEventCount will
	// also return these recordings. This is expected behavior of the key structure.
	assert.Equal(t, 2, tracker.GetEventCount("source-1", "Order Completed"),
		"empty property name shares the same key as event-level tracking")
}

func TestTracker_VeryLongStrings(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	longSourceID := strings.Repeat("s", 10000)
	longEventName := strings.Repeat("e", 10000)
	longPropName := strings.Repeat("p", 10000)

	// Should not panic with very long strings
	tracker.RecordEvent(longSourceID, longEventName)
	tracker.RecordEvent(longSourceID, longEventName)
	assert.Equal(t, 2, tracker.GetEventCount(longSourceID, longEventName))

	tracker.RecordProperty(longSourceID, longEventName, longPropName)
	assert.Equal(t, 1, tracker.GetPropertyCount(longSourceID, longEventName, longPropName))
}

func TestTracker_HighVolume(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100000,
	})
	require.NotNil(t, tracker)

	const totalEvents = 100000
	for i := 0; i < totalEvents; i++ {
		tracker.RecordEvent("source-1", "high-volume-event")
	}

	count := tracker.GetEventCount("source-1", "high-volume-event")
	assert.Equal(t, totalEvents, count, "tracker should handle high volume without data loss")
}

func TestTracker_UnrecordedEventCount(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Querying for events that were never recorded should return zero
	assert.Zero(t, tracker.GetEventCount("source-1", "never-recorded"))
	assert.Zero(t, tracker.GetPropertyCount("source-1", "never-recorded", "prop"))
	assert.False(t, tracker.IsAnomalous("source-1", "never-recorded"))
	assert.False(t, tracker.IsPropertyAnomalous("source-1", "never-recorded", "prop"))
}

// ============================================================================
// Phase 8: Test Reset/Clear Operations
// ============================================================================

func TestTracker_Reset(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 5,
	})
	require.NotNil(t, tracker)

	// Populate with data
	for i := 0; i < 10; i++ {
		tracker.RecordEvent("source-1", "Order Completed")
	}
	tracker.RecordProperty("source-1", "Order Completed", "price")
	tracker.RecordProperty("source-1", "Order Completed", "price")
	tracker.RecordEvent("source-2", "Page Viewed")

	require.Equal(t, 10, tracker.GetEventCount("source-1", "Order Completed"))
	require.Equal(t, 2, tracker.GetPropertyCount("source-1", "Order Completed", "price"))
	require.Equal(t, 1, tracker.GetEventCount("source-2", "Page Viewed"))

	// Reset clears all tracked data
	tracker.Reset()

	assert.Zero(t, tracker.GetEventCount("source-1", "Order Completed"))
	assert.Zero(t, tracker.GetPropertyCount("source-1", "Order Completed", "price"))
	assert.Zero(t, tracker.GetEventCount("source-2", "Page Viewed"))
	assert.False(t, tracker.IsAnomalous("source-1", "Order Completed"))
}

func TestTracker_ResetThenReuse(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 3,
	})
	require.NotNil(t, tracker)

	// First usage
	for i := 0; i < 5; i++ {
		tracker.RecordEvent("source-1", "evt")
	}
	require.Equal(t, 5, tracker.GetEventCount("source-1", "evt"))
	require.True(t, tracker.IsAnomalous("source-1", "evt"))

	// Reset
	tracker.Reset()

	// Verify clean state
	assert.Zero(t, tracker.GetEventCount("source-1", "evt"))
	assert.False(t, tracker.IsAnomalous("source-1", "evt"))

	// Reuse after reset
	tracker.RecordEvent("source-1", "evt")
	tracker.RecordEvent("source-1", "evt")
	assert.Equal(t, 2, tracker.GetEventCount("source-1", "evt"))
	assert.False(t, tracker.IsAnomalous("source-1", "evt"),
		"2 < threshold 3, so should not be anomalous after reset and reuse")
}

func TestTracker_ResetClearsAllSources(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	tracker.RecordEvent("source-1", "evt-a")
	tracker.RecordEvent("source-2", "evt-b")
	tracker.RecordEvent("source-3", "evt-c")
	tracker.RecordProperty("source-1", "evt-a", "prop-x")

	require.Equal(t, 1, tracker.GetEventCount("source-1", "evt-a"))
	require.Equal(t, 1, tracker.GetEventCount("source-2", "evt-b"))
	require.Equal(t, 1, tracker.GetEventCount("source-3", "evt-c"))
	require.Equal(t, 1, tracker.GetPropertyCount("source-1", "evt-a", "prop-x"))

	tracker.Reset()

	assert.Zero(t, tracker.GetEventCount("source-1", "evt-a"))
	assert.Zero(t, tracker.GetEventCount("source-2", "evt-b"))
	assert.Zero(t, tracker.GetEventCount("source-3", "evt-c"))
	assert.Zero(t, tracker.GetPropertyCount("source-1", "evt-a", "prop-x"))
}

// ============================================================================
// Integration: Combined Behavior Tests
// ============================================================================

func TestTracker_AnomalyDetectionWithSlidingWindow(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           100 * time.Millisecond,
		SensitivityThreshold: 3,
	})
	require.NotNil(t, tracker)

	// Record 4 events → above threshold → anomalous
	for i := 0; i < 4; i++ {
		tracker.RecordEvent("source-1", "evt")
	}
	assert.True(t, tracker.IsAnomalous("source-1", "evt"))

	// Wait for expiry
	time.Sleep(150 * time.Millisecond)

	// After expiry, count is 0 → below threshold → not anomalous
	assert.False(t, tracker.IsAnomalous("source-1", "evt"))

	// Record 2 events (below threshold)
	tracker.RecordEvent("source-1", "evt")
	tracker.RecordEvent("source-1", "evt")
	assert.False(t, tracker.IsAnomalous("source-1", "evt"))

	// Record 1 more → exactly at threshold (3 >= 3)
	tracker.RecordEvent("source-1", "evt")
	assert.True(t, tracker.IsAnomalous("source-1", "evt"))
}

func TestTracker_ThresholdUpdateWithExistingData(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 10,
	})
	require.NotNil(t, tracker)

	// Record 5 events
	for i := 0; i < 5; i++ {
		tracker.RecordEvent("source-1", "evt")
	}

	// Not anomalous (5 < 10)
	assert.False(t, tracker.IsAnomalous("source-1", "evt"))

	// Lower threshold to 5 → now 5 >= 5 → anomalous
	tracker.SetSensitivityThreshold(5)
	assert.True(t, tracker.IsAnomalous("source-1", "evt"))

	// Raise threshold to 6 → now 5 < 6 → not anomalous
	tracker.SetSensitivityThreshold(6)
	assert.False(t, tracker.IsAnomalous("source-1", "evt"))
}

func TestTracker_EventAndPropertyIndependence(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 100,
	})
	require.NotNil(t, tracker)

	// Record events and properties for the same event name
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordEvent("source-1", "Order Completed")
	tracker.RecordProperty("source-1", "Order Completed", "price")

	// Event count should not include property recordings
	assert.Equal(t, 2, tracker.GetEventCount("source-1", "Order Completed"))

	// Property count should only reflect property recordings
	assert.Equal(t, 1, tracker.GetPropertyCount("source-1", "Order Completed", "price"))
}
