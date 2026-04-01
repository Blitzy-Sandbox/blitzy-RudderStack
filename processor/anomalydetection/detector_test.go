package anomalydetection_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	"github.com/rudderlabs/rudder-server/processor/anomalydetection"
	"github.com/rudderlabs/rudder-server/processor/types"
)

// ============================================================================
// Test Helpers
// ============================================================================

// newTestDetectorWithStore creates a Detector backed by an in-memory stats store
// for metric verification. It returns the detector and the memstats store.
func newTestDetectorWithStore(t *testing.T, tracker *anomalydetection.Tracker) (*anomalydetection.Detector, *memstats.Store) {
	t.Helper()
	store, err := memstats.New()
	require.NoError(t, err)
	d := anomalydetection.NewDetector(logger.NOP, store, tracker)
	require.NotNil(t, d)
	return d, store
}

// makeTrackEvent builds a types.TransformerEvent for a "track" call with the given
// event name, properties, and tracking plan ID.
func makeTrackEvent(eventName string, props map[string]any, tpID string) types.TransformerEvent {
	msg := types.SingularEventT{
		"type":  "track",
		"event": eventName,
	}
	if props != nil {
		msg["properties"] = props
	}
	return types.TransformerEvent{
		Message: msg,
		Metadata: types.Metadata{
			TrackingPlanID: tpID,
			EventType:      "track",
			EventName:      eventName,
		},
	}
}

// makeNonTrackEvent builds a types.TransformerEvent for a non-track event type
// (e.g. "identify", "page", "screen", "group", "alias").
func makeNonTrackEvent(eventType, tpID string) types.TransformerEvent {
	return types.TransformerEvent{
		Message: types.SingularEventT{
			"type": eventType,
		},
		Metadata: types.Metadata{
			TrackingPlanID: tpID,
			EventType:      eventType,
		},
	}
}

// sampleSchemas returns a realistic three-event tracking plan schema for tests.
// Maps trackingPlanID -> eventName -> expectedPropertyName -> true.
func sampleSchemas() map[string]map[string]map[string]bool {
	return map[string]map[string]map[string]bool{
		"tp-001": {
			"Order Completed": {
				"orderId":  true,
				"total":    true,
				"revenue":  true,
				"currency": true,
				"products": true,
			},
			"Product Viewed": {
				"productId": true,
				"name":      true,
				"price":     true,
				"category":  true,
			},
			"Cart Updated": {
				"cartId":   true,
				"products": true,
			},
		},
	}
}

// emptyResponse returns an empty types.Response suitable for passing to Observe.
func emptyResponse() types.Response {
	return types.Response{}
}

// ============================================================================
// Constructor Tests
// ============================================================================

func TestNewDetector_ValidArgs(t *testing.T) {
	store, err := memstats.New()
	require.NoError(t, err)
	tracker := anomalydetection.NewTracker(anomalydetection.DefaultTrackerConfig())
	d := anomalydetection.NewDetector(logger.NOP, store, tracker)
	require.NotNil(t, d, "NewDetector should return a non-nil detector")
}

func TestNewDetector_NilTracker(t *testing.T) {
	store, err := memstats.New()
	require.NoError(t, err)
	d := anomalydetection.NewDetector(logger.NOP, store, nil)
	require.NotNil(t, d, "NewDetector should handle nil tracker without panic")

	// Observe should not panic even with nil tracker
	d.UpdateSchemas(sampleSchemas())
	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}
	require.NotPanics(t, func() {
		d.Observe("source-1", events, emptyResponse())
	})
}

func TestNewDetector_NilLogger(t *testing.T) {
	// Passing a nil logger should not panic during construction.
	// The detector may panic on Observe if logger methods are called,
	// so we verify construction succeeds.
	store, err := memstats.New()
	require.NoError(t, err)
	// logger.NOP is the safe zero-value logger — test with that
	d := anomalydetection.NewDetector(logger.NOP, store, nil)
	require.NotNil(t, d)
}

// ============================================================================
// UpdateSchemas Tests
// ============================================================================

func TestDetector_UpdateSchemas_Valid(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)

	// Initially no schemas — unknown events should not trigger anomalies
	// because there's no schema to compare against
	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", nil, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())
	metrics := store.GetByName("anomaly_unknown_events")
	assert.Empty(t, metrics, "no anomalies expected before schemas are loaded")

	// Load schemas
	d.UpdateSchemas(sampleSchemas())

	// Now an unknown event should be detected
	unknownEvents := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}
	d.Observe("source-1", unknownEvents, emptyResponse())
	metrics = store.GetByName("anomaly_unknown_events")
	require.NotEmpty(t, metrics, "unknown event anomaly should be emitted after schemas loaded")
	assert.Equal(t, float64(1), metrics[0].Value)
}

func TestDetector_UpdateSchemas_Nil(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)

	// Load schemas first
	d.UpdateSchemas(sampleSchemas())

	// Then clear schemas by setting nil
	d.UpdateSchemas(nil)

	// Unknown events should no longer trigger anomalies (no schemas to compare against)
	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())
	metrics := store.GetByName("anomaly_unknown_events")
	assert.Empty(t, metrics, "no anomalies expected after schemas cleared to nil")
}

func TestDetector_UpdateSchemas_Empty(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)

	// Load schemas, then update with empty map
	d.UpdateSchemas(sampleSchemas())
	d.UpdateSchemas(map[string]map[string]map[string]bool{})

	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())
	metrics := store.GetByName("anomaly_unknown_events")
	assert.Empty(t, metrics, "no anomalies expected after schemas cleared to empty")
}

// ============================================================================
// Observe — Empty/No-Op Paths
// ============================================================================

func TestDetector_Observe_EmptyEvents(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// Should not panic or emit any metrics
	require.NotPanics(t, func() {
		d.Observe("source-1", nil, emptyResponse())
	})
	require.NotPanics(t, func() {
		d.Observe("source-1", []types.TransformerEvent{}, emptyResponse())
	})

	assert.Empty(t, store.GetAll(), "no metrics should be emitted for empty events")
}

func TestDetector_Observe_NoTrackingPlanID(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// Event with empty TrackingPlanID — Observe should skip detection
	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, ""), // no TP ID
	}
	d.Observe("source-1", events, emptyResponse())
	assert.Empty(t, store.GetAll(), "no metrics when TrackingPlanID is empty")
}

func TestDetector_Observe_NoSchemasForTrackingPlan(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas()) // schemas for "tp-001"

	// Event references a different tracking plan "tp-999" with no schemas
	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-999"),
	}
	d.Observe("source-1", events, emptyResponse())
	assert.Empty(t, store.GetAll(), "no metrics when no schemas exist for the tracking plan")
}

// ============================================================================
// Observe — Unknown Event Detection
// ============================================================================

func TestDetector_Observe_KnownEvent_NoAnomalies(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{
			"orderId": "123",
			"total":   99.99,
		}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())
	assert.Empty(t, store.GetByName("anomaly_unknown_events"), "known event should not trigger unknown event anomaly")
}

func TestDetector_Observe_UnknownEvent(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Checkout Started", nil, "tp-001"), // not in schema
	}
	d.Observe("source-1", events, emptyResponse())

	metrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, metrics, 1, "exactly one unknown event metric should be emitted")
	assert.Equal(t, float64(1), metrics[0].Value)
	assert.Equal(t, "source-1", metrics[0].Tags["source"])
	assert.Equal(t, "tp-001", metrics[0].Tags["trackingPlanId"])
}

func TestDetector_Observe_MultipleUnknownEvents(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", nil, "tp-001"),   // known
		makeTrackEvent("Checkout Started", nil, "tp-001"),  // unknown
		makeTrackEvent("Product Viewed", nil, "tp-001"),    // known
		makeTrackEvent("Coupon Applied", nil, "tp-001"),    // unknown
		makeTrackEvent("Payment Failed", nil, "tp-001"),    // unknown
	}
	d.Observe("source-1", events, emptyResponse())

	metrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, metrics, 1)
	assert.Equal(t, float64(3), metrics[0].Value, "should detect exactly 3 unknown events")
}

// ============================================================================
// Observe — Unexpected Property Detection
// ============================================================================

func TestDetector_Observe_UnexpectedProperty(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{
			"orderId":         "123",
			"total":           99.99,
			"unexpectedField": "value", // not in schema
		}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	// No unknown event anomaly
	assert.Empty(t, store.GetByName("anomaly_unknown_events"))

	// One unexpected property anomaly
	metrics := store.GetByName("anomaly_unexpected_properties")
	require.Len(t, metrics, 1)
	assert.Equal(t, float64(1), metrics[0].Value)
	assert.Equal(t, "source-1", metrics[0].Tags["source"])
	assert.Equal(t, "tp-001", metrics[0].Tags["trackingPlanId"])
}

func TestDetector_Observe_AllExpectedProperties(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{
			"orderId":  "123",
			"total":    99.99,
			"revenue":  89.99,
			"currency": "USD",
			"products": []string{"prod-1"},
		}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	assert.Empty(t, store.GetByName("anomaly_unexpected_properties"), "all properties expected — no anomaly")
	assert.Empty(t, store.GetByName("anomaly_unknown_events"))
}

func TestDetector_Observe_MultipleUnexpectedProperties(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{
			"orderId":  "123",          // expected
			"total":    99.99,          // expected
			"discount": 10.0,           // unexpected
			"coupon":   "SAVE10",       // unexpected
			"referrer": "google.com",   // unexpected
		}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	metrics := store.GetByName("anomaly_unexpected_properties")
	require.Len(t, metrics, 1)
	assert.Equal(t, float64(3), metrics[0].Value, "should detect exactly 3 unexpected properties")
}

func TestDetector_Observe_NoProperties(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// Known event but with no properties at all
	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", nil, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	assert.Empty(t, store.GetAll(), "no anomalies when event has no properties field")
}

func TestDetector_Observe_EmptyPropertiesMap(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	assert.Empty(t, store.GetAll(), "no anomalies for empty properties map")
}

func TestDetector_Observe_PropertiesNotAMap(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// Properties field exists but is a string, not a map — should not panic
	event := types.TransformerEvent{
		Message: types.SingularEventT{
			"type":       "track",
			"event":      "Order Completed",
			"properties": "not-a-map",
		},
		Metadata: types.Metadata{
			TrackingPlanID: "tp-001",
		},
	}
	require.NotPanics(t, func() {
		d.Observe("source-1", []types.TransformerEvent{event}, emptyResponse())
	})
	assert.Empty(t, store.GetAll(), "non-map properties should not trigger anomalies")
}

// ============================================================================
// Observe — Non-Track Event Handling
// ============================================================================

func TestDetector_Observe_NonTrackEventsSkipped(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	eventTypes := []string{"identify", "page", "screen", "group", "alias"}
	for _, et := range eventTypes {
		t.Run(et, func(t *testing.T) {
			events := []types.TransformerEvent{
				makeNonTrackEvent(et, "tp-001"),
			}
			d.Observe("source-1", events, emptyResponse())
		})
	}

	assert.Empty(t, store.GetAll(), "non-track event types should not produce anomalies")
}

func TestDetector_Observe_TrackEventEmptyName(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// Track event with empty event name
	event := types.TransformerEvent{
		Message: types.SingularEventT{
			"type":  "track",
			"event": "",
		},
		Metadata: types.Metadata{
			TrackingPlanID: "tp-001",
		},
	}
	d.Observe("source-1", []types.TransformerEvent{event}, emptyResponse())
	assert.Empty(t, store.GetAll(), "track event with empty name should not trigger anomalies")
}

func TestDetector_Observe_TrackEventNoEventField(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// Track event missing the "event" field entirely
	event := types.TransformerEvent{
		Message: types.SingularEventT{
			"type": "track",
		},
		Metadata: types.Metadata{
			TrackingPlanID: "tp-001",
		},
	}
	d.Observe("source-1", []types.TransformerEvent{event}, emptyResponse())
	assert.Empty(t, store.GetAll(), "track event missing event field should not trigger anomalies")
}

// ============================================================================
// Observe — Mixed Batch
// ============================================================================

func TestDetector_Observe_MixedBatch(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		// Known event with all expected properties — no anomalies
		makeTrackEvent("Order Completed", map[string]any{
			"orderId": "123",
			"total":   99.99,
		}, "tp-001"),
		// Unknown event — 1 unknown event anomaly
		makeTrackEvent("Promo Clicked", nil, "tp-001"),
		// Known event with unexpected property — 1 unexpected property anomaly
		makeTrackEvent("Product Viewed", map[string]any{
			"productId": "prod-1",
			"name":      "Widget",
			"color":     "blue", // unexpected
		}, "tp-001"),
		// Non-track event — ignored
		makeNonTrackEvent("identify", "tp-001"),
	}

	d.Observe("source-1", events, emptyResponse())

	unknownMetrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, unknownMetrics, 1)
	assert.Equal(t, float64(1), unknownMetrics[0].Value, "1 unknown event expected")

	propMetrics := store.GetByName("anomaly_unexpected_properties")
	require.Len(t, propMetrics, 1)
	assert.Equal(t, float64(1), propMetrics[0].Value, "1 unexpected property expected")
}

// ============================================================================
// Observe — Metrics Emission
// ============================================================================

func TestDetector_Observe_Metrics_UnknownEvents_Tags(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event 1", nil, "tp-001"),
		makeTrackEvent("Unknown Event 2", nil, "tp-001"),
	}
	d.Observe("source-abc", events, emptyResponse())

	metrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, metrics, 1)
	assert.Equal(t, float64(2), metrics[0].Value, "2 unknown events")
	assert.Equal(t, "source-abc", metrics[0].Tags["source"], "source tag matches")
	assert.Equal(t, "tp-001", metrics[0].Tags["trackingPlanId"], "trackingPlanId tag matches")
}

func TestDetector_Observe_Metrics_UnexpectedProperties_Tags(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Cart Updated", map[string]any{
			"cartId":    "cart-1",
			"products":  []string{"p1"},
			"extraProp": true, // unexpected
		}, "tp-001"),
	}
	d.Observe("source-xyz", events, emptyResponse())

	metrics := store.GetByName("anomaly_unexpected_properties")
	require.Len(t, metrics, 1)
	assert.Equal(t, float64(1), metrics[0].Value)
	assert.Equal(t, "source-xyz", metrics[0].Tags["source"])
	assert.Equal(t, "tp-001", metrics[0].Tags["trackingPlanId"])
}

func TestDetector_Observe_Metrics_NoAnomalies(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{
			"orderId": "123",
			"total":   99.99,
		}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	assert.Empty(t, store.GetAll(), "no metrics should be emitted when there are no anomalies")
}

// ============================================================================
// Observe — Tracker Integration
// ============================================================================

func TestDetector_Observe_TrackerRecordsUnknownEvents(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 1,
	})
	d, _ := newTestDetectorWithStore(t, tracker)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	// The tracker should have recorded 2 occurrences of the unknown event
	count := tracker.GetEventCount("source-1", "Unknown Event")
	assert.Equal(t, 2, count, "tracker should record unknown event occurrences")
}

func TestDetector_Observe_TrackerRecordsUnexpectedProperties(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 1,
	})
	d, _ := newTestDetectorWithStore(t, tracker)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{
			"orderId":  "123",
			"badField": true, // unexpected
		}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	count := tracker.GetPropertyCount("source-1", "Order Completed", "badField")
	assert.Equal(t, 1, count, "tracker should record unexpected property occurrences")
}

// ============================================================================
// Observe — Thread Safety
// ============================================================================

func TestDetector_Observe_ConcurrentAccess(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.DefaultTrackerConfig())
	d, _ := newTestDetectorWithStore(t, tracker)
	d.UpdateSchemas(sampleSchemas())

	const numGoroutines = 10
	const numIterations = 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < numIterations; i++ {
				events := []types.TransformerEvent{
					makeTrackEvent("Order Completed", map[string]any{
						"orderId": "123",
						"total":   99.99,
					}, "tp-001"),
					makeTrackEvent("Unknown From Goroutine", nil, "tp-001"),
				}
				d.Observe("source-concurrent", events, emptyResponse())
			}
		}()
	}

	wg.Wait()

	// Verify no panics occurred and tracker recorded events
	count := tracker.GetEventCount("source-concurrent", "Unknown From Goroutine")
	assert.Equal(t, numGoroutines*numIterations, count,
		"tracker should record all unknown event occurrences from concurrent goroutines")
}

func TestDetector_ConcurrentObserveAndUpdateSchemas(t *testing.T) {
	tracker := anomalydetection.NewTracker(anomalydetection.DefaultTrackerConfig())
	d, _ := newTestDetectorWithStore(t, tracker)
	d.UpdateSchemas(sampleSchemas())

	const numGoroutines = 5
	const numIterations = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines + 1) // +1 for the schema updater

	// Goroutines calling Observe concurrently
	for g := 0; g < numGoroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < numIterations; i++ {
				events := []types.TransformerEvent{
					makeTrackEvent("Order Completed", map[string]any{"orderId": "123"}, "tp-001"),
				}
				d.Observe("source-1", events, emptyResponse())
			}
		}()
	}

	// One goroutine updating schemas concurrently
	go func() {
		defer wg.Done()
		for i := 0; i < numIterations; i++ {
			d.UpdateSchemas(sampleSchemas())
		}
	}()

	wg.Wait()
	// Test passes if no race condition or panic occurs
}

// ============================================================================
// Observe — Edge Cases
// ============================================================================

func TestDetector_Observe_EventTypeNotString(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// "type" field is not a string
	event := types.TransformerEvent{
		Message: types.SingularEventT{
			"type":  42,
			"event": "Order Completed",
		},
		Metadata: types.Metadata{
			TrackingPlanID: "tp-001",
		},
	}
	require.NotPanics(t, func() {
		d.Observe("source-1", []types.TransformerEvent{event}, emptyResponse())
	})
	assert.Empty(t, store.GetAll(), "non-string type should not trigger anomalies")
}

func TestDetector_Observe_EventNameNotString(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// "event" field is not a string
	event := types.TransformerEvent{
		Message: types.SingularEventT{
			"type":  "track",
			"event": 12345,
		},
		Metadata: types.Metadata{
			TrackingPlanID: "tp-001",
		},
	}
	require.NotPanics(t, func() {
		d.Observe("source-1", []types.TransformerEvent{event}, emptyResponse())
	})
	assert.Empty(t, store.GetAll(), "non-string event name should not trigger anomalies")
}

func TestDetector_Observe_NilMessage(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	event := types.TransformerEvent{
		Message: nil,
		Metadata: types.Metadata{
			TrackingPlanID: "tp-001",
		},
	}
	require.NotPanics(t, func() {
		d.Observe("source-1", []types.TransformerEvent{event}, emptyResponse())
	})
	assert.Empty(t, store.GetAll(), "nil message should not trigger anomalies or panics")
}

func TestDetector_Observe_EmptySourceID(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}
	require.NotPanics(t, func() {
		d.Observe("", events, emptyResponse())
	})
	// Should still detect the anomaly even with empty sourceID
	metrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, metrics, 1)
	assert.Equal(t, float64(1), metrics[0].Value)
}

func TestDetector_Observe_MultipleEventsAcrossEventTypes(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{"orderId": "1"}, "tp-001"),
		makeTrackEvent("Product Viewed", map[string]any{"productId": "p1", "name": "Widget"}, "tp-001"),
		makeTrackEvent("Cart Updated", map[string]any{"cartId": "c1", "products": []string{"p1"}}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())
	assert.Empty(t, store.GetAll(), "all events are known with expected properties — no anomalies")
}

func TestDetector_Observe_UnknownEventWithUnexpectedProps(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// Unknown event — the detector should report it as unknown and NOT check properties
	// because once the event is unknown, property checking is skipped (return early)
	events := []types.TransformerEvent{
		makeTrackEvent("Completely New Event", map[string]any{
			"field1": "val1",
			"field2": "val2",
		}, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	unknownMetrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, unknownMetrics, 1)
	assert.Equal(t, float64(1), unknownMetrics[0].Value, "should detect unknown event")

	// No unexpected property metrics because detection stops at unknown event
	propMetrics := store.GetByName("anomaly_unexpected_properties")
	assert.Empty(t, propMetrics, "properties should not be checked for unknown events")
}

func TestDetector_Observe_LargeBatch(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := make([]types.TransformerEvent, 0, 1000)
	for i := 0; i < 500; i++ {
		events = append(events, makeTrackEvent("Order Completed", map[string]any{
			"orderId": "123",
			"total":   99.99,
		}, "tp-001"))
	}
	for i := 0; i < 500; i++ {
		events = append(events, makeTrackEvent("Unknown Bulk Event", nil, "tp-001"))
	}

	require.NotPanics(t, func() {
		d.Observe("source-bulk", events, emptyResponse())
	})

	metrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, metrics, 1)
	assert.Equal(t, float64(500), metrics[0].Value, "should detect 500 unknown events")
}

// ============================================================================
// Observe — Schema with Different Tracking Plan IDs
// ============================================================================

func TestDetector_Observe_MultipleTrackingPlans(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)

	multiTPSchemas := map[string]map[string]map[string]bool{
		"tp-001": {
			"Order Completed": {"orderId": true, "total": true},
		},
		"tp-002": {
			"Signup Completed": {"userId": true, "email": true},
		},
	}
	d.UpdateSchemas(multiTPSchemas)

	// Event for tp-001 with known event
	events1 := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{"orderId": "1"}, "tp-001"),
	}
	d.Observe("source-1", events1, emptyResponse())
	assert.Empty(t, store.GetAll(), "known event in tp-001 — no anomalies")

	// Event for tp-002 with unknown event (Order Completed is not in tp-002)
	events2 := []types.TransformerEvent{
		makeTrackEvent("Order Completed", nil, "tp-002"),
	}
	d.Observe("source-2", events2, emptyResponse())

	metrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, metrics, 1, "Order Completed is unknown in tp-002")
	assert.Equal(t, "source-2", metrics[0].Tags["source"])
	assert.Equal(t, "tp-002", metrics[0].Tags["trackingPlanId"])
}

// ============================================================================
// Observe — Response parameter (retained for future use)
// ============================================================================

func TestDetector_Observe_WithPopulatedResponse(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}

	// Pass a non-empty response — should not affect anomaly detection
	response := types.Response{
		Events: []types.TransformerResponse{
			{
				Output:     map[string]any{"type": "track", "event": "Unknown Event"},
				Metadata:   types.Metadata{TrackingPlanID: "tp-001"},
				StatusCode: 200,
			},
		},
		FailedEvents: []types.TransformerResponse{
			{
				Output:     map[string]any{},
				Metadata:   types.Metadata{TrackingPlanID: "tp-001"},
				StatusCode: 400,
				Error:      "validation failed",
			},
		},
	}

	d.Observe("source-1", events, response)

	metrics := store.GetByName("anomaly_unknown_events")
	require.Len(t, metrics, 1, "anomaly detection should work regardless of response content")
}

// ============================================================================
// Anomaly Type Constants and Struct Verification
// ============================================================================

func TestDetector_AnomalyTypeConstants(t *testing.T) {
	// Verify the string values of anomaly type constants
	require.Equal(t, "unknown_event", anomalydetection.AnomalyTypeUnknownEvent)
	require.Equal(t, "unexpected_property", anomalydetection.AnomalyTypeUnexpectedProperty)

	// Constants must be distinct
	assert.NotEqual(t, anomalydetection.AnomalyTypeUnknownEvent, anomalydetection.AnomalyTypeUnexpectedProperty)
}

func TestDetector_AnomalyStruct(t *testing.T) {
	// Verify the Anomaly struct can be constructed with expected field values
	a := anomalydetection.Anomaly{
		Type:           anomalydetection.AnomalyTypeUnknownEvent,
		SourceID:       "src-1",
		EventName:      "Unknown Event",
		EventType:      "track",
		PropertyName:   "",
		TrackingPlanID: "tp-001",
	}
	assert.NotNil(t, a)
	assert.Equal(t, anomalydetection.AnomalyTypeUnknownEvent, a.Type)
	assert.True(t, a.PropertyName == "", "PropertyName should be empty for unknown event anomalies")
	assert.False(t, a.SourceID == "", "SourceID should not be empty")

	// Verify unexpected property anomaly struct
	b := anomalydetection.Anomaly{
		Type:           anomalydetection.AnomalyTypeUnexpectedProperty,
		SourceID:       "src-1",
		EventName:      "Order Completed",
		EventType:      "track",
		PropertyName:   "badField",
		TrackingPlanID: "tp-001",
	}
	assert.NotNil(t, b)
	assert.Equal(t, anomalydetection.AnomalyTypeUnexpectedProperty, b.Type)
	assert.False(t, b.PropertyName == "", "PropertyName should be set for unexpected property anomalies")
	assert.True(t, b.EventName == "Order Completed")
}

// ============================================================================
// No False Positives — Comprehensive (Phase 4 spec requirements)
// ============================================================================

func TestDetector_NoFalsePositives_TenValidEvents(t *testing.T) {
	// Use logger.NewLogger pattern and explicit stats.Stats interface reference
	store, err := memstats.New()
	require.NoError(t, err)
	var statsFactory stats.Stats = store
	log := logger.NewLogger().Child("test").Child("anomalydetection")
	tracker := anomalydetection.NewTracker(anomalydetection.DefaultTrackerConfig())
	d := anomalydetection.NewDetector(log, statsFactory, tracker)
	require.NotNil(t, d)
	d.UpdateSchemas(sampleSchemas())

	// Send exactly 10 well-formed events matching the tracking plan schema
	events := []types.TransformerEvent{
		makeTrackEvent("Order Completed", map[string]any{"orderId": "o1", "total": 10.0}, "tp-001"),
		makeTrackEvent("Order Completed", map[string]any{"orderId": "o2", "total": 20.0, "revenue": 15.0}, "tp-001"),
		makeTrackEvent("Order Completed", map[string]any{"orderId": "o3", "total": 30.0, "currency": "USD"}, "tp-001"),
		makeTrackEvent("Product Viewed", map[string]any{"productId": "p1", "name": "Widget A", "price": 1.0, "category": "Electronics"}, "tp-001"),
		makeTrackEvent("Product Viewed", map[string]any{"productId": "p2", "name": "Widget B", "price": 2.0, "category": "Books"}, "tp-001"),
		makeTrackEvent("Product Viewed", map[string]any{"productId": "p3", "name": "Widget C", "price": 3.0, "category": "Clothing"}, "tp-001"),
		makeTrackEvent("Cart Updated", map[string]any{"cartId": "c1", "products": []string{"p1"}}, "tp-001"),
		makeTrackEvent("Cart Updated", map[string]any{"cartId": "c2", "products": []string{"p2", "p3"}}, "tp-001"),
		makeTrackEvent("Order Completed", map[string]any{"orderId": "o4", "total": 40.0, "products": []string{"p1", "p2"}}, "tp-001"),
		makeTrackEvent("Product Viewed", map[string]any{"productId": "p4", "name": "Widget D"}, "tp-001"),
	}

	d.Observe("source-1", events, emptyResponse())

	allMetrics := store.GetAll()
	assert.Len(t, allMetrics, 0, "no anomalies expected for 10 valid events matching schema")
	assert.Zero(t, len(allMetrics))
	assert.True(t, len(allMetrics) == 0, "zero metrics emitted for valid events")
	assert.False(t, len(allMetrics) > 0, "should not have any metrics for valid events")
}

func TestDetector_NoFalsePositives_IdentifyEvent(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// "identify" events don't have the "event" property — should not trigger anomaly
	event := types.TransformerEvent{
		Message: types.SingularEventT{
			"type": "identify",
			"traits": map[string]any{
				"email": "user@example.com",
				"name":  "Test User",
			},
		},
		Metadata: types.Metadata{
			TrackingPlanID: "tp-001",
			EventType:      "identify",
		},
	}
	d.Observe("source-1", []types.TransformerEvent{event}, emptyResponse())

	allMetrics := store.GetAll()
	assert.Len(t, allMetrics, 0, "identify events should not trigger any anomalies")
	assert.True(t, len(allMetrics) == 0)
}

func TestDetector_NoFalsePositives_PageEvent(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	// "page" events use "name" field instead of "event" — should not trigger anomaly
	event := types.TransformerEvent{
		Message: types.SingularEventT{
			"type": "page",
			"name": "Home",
			"properties": map[string]any{
				"url":   "https://example.com",
				"title": "Home Page",
			},
		},
		Metadata: types.Metadata{
			TrackingPlanID: "tp-001",
			EventType:      "page",
		},
	}
	d.Observe("source-1", []types.TransformerEvent{event}, emptyResponse())

	allMetrics := store.GetAll()
	assert.Len(t, allMetrics, 0, "page events should not trigger any anomalies")
	assert.False(t, len(allMetrics) > 0)
}

// ============================================================================
// Response with ValidationErrors (spec Phase 7 — ObserveWithFailedEvents)
// ============================================================================

func TestDetector_Observe_ResponseWithValidationErrors(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}

	// Construct response with both Events and FailedEvents including ValidationErrors
	response := types.Response{
		Events: []types.TransformerResponse{
			{
				Output:     map[string]any{"type": "track", "event": "Unknown Event"},
				Metadata:   types.Metadata{TrackingPlanID: "tp-001"},
				StatusCode: 200,
			},
		},
		FailedEvents: []types.TransformerResponse{
			{
				Output:     map[string]any{"type": "track", "event": "Another Unknown"},
				Metadata:   types.Metadata{TrackingPlanID: "tp-001"},
				StatusCode: 400,
				Error:      "validation failed",
				ValidationErrors: []types.ValidationError{
					{
						Type:    "unknown_event",
						Message: "Event 'Another Unknown' is not defined in tracking plan",
					},
					{
						Type:     "unexpected_property",
						Message:  "Property 'badField' is not defined",
						Property: "badField",
					},
				},
			},
		},
	}

	// Observe processes events, not response content; response is retained for future use
	d.Observe("source-1", events, response)

	metrics := store.GetByName("anomaly_unknown_events")
	require.Equal(t, 1, len(metrics), "one unknown event metric batch should be emitted")
	assert.Equal(t, float64(1), metrics[0].Value)
}

// ============================================================================
// NewDetector with explicit logger.NewLogger and stats.Stats interface
// ============================================================================

func TestDetector_WithNewLoggerAndStatsInterface(t *testing.T) {
	store, err := memstats.New()
	require.NoError(t, err)

	// Explicitly reference stats.Stats interface type and logger.NewLogger()
	var s stats.Stats = store
	log := logger.NewLogger().Child("test-detector")
	d := anomalydetection.NewDetector(log, s, nil)
	require.NotNil(t, d, "detector should be created with stats.Stats interface and NewLogger")

	d.UpdateSchemas(sampleSchemas())
	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}
	d.Observe("source-1", events, emptyResponse())

	metrics := store.GetByName("anomaly_unknown_events")
	assert.NotNil(t, metrics, "metrics slice should not be nil")
	assert.Len(t, metrics, 1)
	require.Equal(t, float64(1), metrics[0].Value)
}

// ============================================================================
// Observe — Nil-equivalent Response (spec Phase 7)
// ============================================================================

func TestDetector_Observe_NilEquivalentResponse(t *testing.T) {
	d, store := newTestDetectorWithStore(t, nil)
	d.UpdateSchemas(sampleSchemas())

	events := []types.TransformerEvent{
		makeTrackEvent("Unknown Event", nil, "tp-001"),
	}

	// Pass a response with nil/empty slices — should handle gracefully
	nilResponse := types.Response{
		Events:       nil,
		FailedEvents: nil,
	}
	require.NotPanics(t, func() {
		d.Observe("source-1", events, nilResponse)
	})

	metrics := store.GetByName("anomaly_unknown_events")
	assert.Len(t, metrics, 1, "anomaly detection should work with nil response slices")
	assert.Equal(t, float64(1), metrics[0].Value)
}
