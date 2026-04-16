// Package anomalydetection provides anomaly detection capabilities for the Protocols
// tracking plan enforcement pipeline (Sprint 5-7, E-021). It identifies unexpected events
// and properties not defined in tracking plan schemas and tracks their occurrence frequencies
// over configurable sliding time windows.
//
// The Detector integrates with the processor's tracking plan validation flow by observing
// validated events and comparing them against the tracking plan schema baseline. It satisfies
// the anomalyDetector interface defined in processor/processor.go:
//
//	type anomalyDetector interface {
//	    Observe(sourceID string, events []types.TransformerEvent, response types.Response)
//	}
//
// Note: This package deliberately does NOT import the processor package to avoid circular
// dependencies. The method signature uses string for sourceID rather than processor.SourceIDT.
package anomalydetection

import (
	"sync"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/processor/types"
)

// Anomaly type constants identify the kind of anomaly detected.
const (
	// AnomalyTypeUnknownEvent indicates an event name that is not defined in the tracking plan.
	// This occurs when a "track" event has an event name not present in the schema baseline.
	AnomalyTypeUnknownEvent = "unknown_event"

	// AnomalyTypeUnexpectedProperty indicates a property on a known event that is not defined
	// in the tracking plan schema for that event. The event itself is known, but one or more
	// of its properties are not in the expected property set.
	AnomalyTypeUnexpectedProperty = "unexpected_property"
)

// Anomaly represents a single detected anomaly — either an unexpected event name
// or an unexpected property on a known event. Anomalies are produced by the Detector
// during Observe and reported via structured logging and stats metrics.
type Anomaly struct {
	// Type is either AnomalyTypeUnknownEvent ("unknown_event") or
	// AnomalyTypeUnexpectedProperty ("unexpected_property").
	Type string

	// SourceID is the source that produced the anomalous event.
	SourceID string

	// EventName is the name of the event (from the "event" field in track events,
	// e.g., "Order Completed").
	EventName string

	// EventType is the RudderStack event type: "track", "identify", "page",
	// "screen", "group", or "alias".
	EventType string

	// PropertyName is the name of the unexpected property. This field is empty
	// for AnomalyTypeUnknownEvent anomalies.
	PropertyName string

	// TrackingPlanID is the tracking plan the event was validated against.
	TrackingPlanID string
}

// Detector identifies unexpected events and properties not defined in the tracking plan.
// It integrates with the processor's tracking plan validation flow by observing validated
// events and comparing them against the tracking plan schema baseline.
//
// The Detector maintains a three-level schema map:
//
//	trackingPlanID -> eventName -> set of expected property names
//
// It is designed for concurrent use: Observe may be called from multiple processor
// goroutines simultaneously while UpdateSchemas is called from the backend-config
// goroutine. Thread safety is achieved via sync.RWMutex.
//
// When no schemas are loaded or no tracking plan is configured for a source, the
// Detector is a no-op, ensuring backward compatibility with existing pipeline behavior.
type Detector struct {
	logger       logger.Logger
	statsFactory stats.Stats
	tracker      *Tracker

	// mu protects the schemas map for concurrent access.
	// RLock is held during Observe (called from processor goroutines).
	// Lock is held during UpdateSchemas (called from backend-config goroutine).
	mu sync.RWMutex

	// schemas maps tracking plan ID -> event name -> set of expected property names.
	// Updated when backend-config changes are received via UpdateSchemas.
	schemas map[string]map[string]map[string]bool
}

// NewDetector creates a new anomaly Detector.
//
// Parameters:
//   - log: structured logger (callers should use logger.NewLogger().Child("anomalydetection"))
//   - statsFactory: stats factory for emitting anomaly metrics via NewTaggedStat
//   - tracker: frequency tracker for anomaly occurrence tracking over time windows;
//     may be nil if frequency tracking is not needed
//
// The detector starts with no schemas. Call UpdateSchemas to set tracking plan schemas
// before Observe will produce meaningful results.
func NewDetector(log logger.Logger, statsFactory stats.Stats, tracker *Tracker) *Detector {
	return &Detector{
		logger:       log,
		statsFactory: statsFactory,
		tracker:      tracker,
		schemas:      make(map[string]map[string]map[string]bool),
	}
}

// UpdateSchemas updates the tracking plan schemas used for anomaly baseline comparison.
// This is called when backend-config publishes updated tracking plan configurations.
//
// The schemas map structure is:
//
//	trackingPlanID -> eventName -> set of expected property names (propertyName -> true)
//
// Passing nil or an empty map effectively disables anomaly detection until non-empty
// schemas are provided.
//
// Thread-safe: can be called concurrently with Observe.
func (d *Detector) UpdateSchemas(schemas map[string]map[string]map[string]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if schemas == nil {
		schemas = make(map[string]map[string]map[string]bool)
	}
	d.schemas = schemas
}

// Observe analyzes events after tracking plan validation and detects anomalies:
//  1. Unknown events: event names not defined in the tracking plan (track events only)
//  2. Unexpected properties: properties on known events not in the tracking plan schema
//
// Called by processor/trackingplan.go after validateEvents() completes. Reports anomalies
// via structured logging and stats metrics (anomaly_unknown_events, anomaly_unexpected_properties).
//
// Observe is a no-op when:
//   - events is empty
//   - the first event has no TrackingPlanID in its Metadata
//   - no schemas are loaded for the tracking plan
//
// Parameters:
//   - sourceID: the source ID that produced the events
//   - events: the list of events that were validated against the tracking plan
//   - response: the validation response from the tracking plan validator (retained for
//     future use in correlating anomalies with validation failures)
//
// Thread-safe: can be called concurrently from multiple processor goroutines.
func (d *Detector) Observe(sourceID string, events []types.TransformerEvent, response types.Response) {
	if len(events) == 0 {
		return
	}

	// Get the tracking plan ID from the first event's metadata.
	// All events in a batch share the same tracking plan context.
	trackingPlanID := events[0].Metadata.TrackingPlanID
	if trackingPlanID == "" {
		return // No tracking plan configured — nothing to compare against
	}

	// Read-lock the schemas for the duration of detection.
	d.mu.RLock()
	tpSchemas, hasTp := d.schemas[trackingPlanID]
	d.mu.RUnlock()

	if !hasTp {
		// No schema loaded for this tracking plan — skip detection.
		// This can happen if the backend-config has not yet published schemas
		// for this tracking plan.
		return
	}

	var anomalies []Anomaly

	for i := range events {
		eventAnomalies := d.detectEventAnomalies(sourceID, trackingPlanID, events[i], tpSchemas)
		anomalies = append(anomalies, eventAnomalies...)
	}

	// Report anomalies via logging and metrics
	d.reportAnomalies(sourceID, trackingPlanID, anomalies)
}

// detectEventAnomalies checks a single event for anomalies against the tracking plan schema.
// It inspects track events for:
//   - Unknown event names: the event name is not in the tracking plan schema
//   - Unexpected properties: properties on known events not in the expected set
//
// Non-track event types (identify, page, screen, group, alias) are not checked for
// event name anomalies because they don't have named events in tracking plans.
func (d *Detector) detectEventAnomalies(
	sourceID, trackingPlanID string,
	event types.TransformerEvent,
	tpSchemas map[string]map[string]bool,
) []Anomaly {
	var anomalies []Anomaly

	eventType := getEventType(event.Message)
	eventName := getEventName(event.Message)

	// Only check track events for event name anomalies.
	// Identify, page, screen, group, and alias events don't have named events
	// in tracking plans — they are schema-validated by their properties only.
	if eventType != "track" || eventName == "" {
		return anomalies
	}

	expectedProps, knownEvent := tpSchemas[eventName]
	if !knownEvent {
		// Unknown event: event name is not defined in the tracking plan
		anomalies = append(anomalies, Anomaly{
			Type:           AnomalyTypeUnknownEvent,
			SourceID:       sourceID,
			EventName:      eventName,
			EventType:      eventType,
			TrackingPlanID: trackingPlanID,
		})
		// Record in tracker for frequency-based analysis
		if d.tracker != nil {
			d.tracker.RecordEvent(sourceID, eventName)
		}
		return anomalies
	}

	// Known event — check for unexpected properties
	propertyAnomalies := d.detectUnexpectedProperties(
		sourceID, trackingPlanID, eventName, eventType,
		event.Message, expectedProps,
	)
	anomalies = append(anomalies, propertyAnomalies...)

	return anomalies
}

// detectUnexpectedProperties checks if an event has properties not defined in the
// tracking plan schema for that event. It extracts the "properties" map from the
// event message and compares each property name against the expected set.
//
// Properties not in the expectedProps set are reported as AnomalyTypeUnexpectedProperty.
// If the event has no "properties" field or the field is not a map, no anomalies are reported.
func (d *Detector) detectUnexpectedProperties(
	sourceID, trackingPlanID, eventName, eventType string,
	message types.SingularEventT,
	expectedProps map[string]bool,
) []Anomaly {
	var anomalies []Anomaly

	// Get the "properties" object from the event message.
	// Track events carry their custom data in message["properties"].
	propsRaw, ok := message["properties"]
	if !ok || propsRaw == nil {
		return nil
	}

	props, ok := propsRaw.(map[string]any)
	if !ok {
		// Properties field exists but is not a map — skip property-level detection.
		return nil
	}

	for propName := range props {
		if !expectedProps[propName] {
			anomalies = append(anomalies, Anomaly{
				Type:           AnomalyTypeUnexpectedProperty,
				SourceID:       sourceID,
				EventName:      eventName,
				EventType:      eventType,
				PropertyName:   propName,
				TrackingPlanID: trackingPlanID,
			})
			// Record in tracker for frequency-based analysis
			if d.tracker != nil {
				d.tracker.RecordProperty(sourceID, eventName, propName)
			}
		}
	}

	return anomalies
}

// reportAnomalies logs and emits metrics for detected anomalies.
// Each anomaly is logged individually with structured fields for observability.
// Aggregate counts are emitted as Prometheus metrics:
//   - anomaly_unknown_events: count of unknown event anomalies
//   - anomaly_unexpected_properties: count of unexpected property anomalies
//
// Both metrics carry "source" and "trackingPlanId" tags for dimensional aggregation.
func (d *Detector) reportAnomalies(sourceID, trackingPlanID string, anomalies []Anomaly) {
	if len(anomalies) == 0 {
		return
	}

	unknownEventCount := 0
	unexpectedPropCount := 0

	for i := range anomalies {
		a := &anomalies[i]
		switch a.Type {
		case AnomalyTypeUnknownEvent:
			unknownEventCount++
			d.logger.Infon("anomaly detected: unknown event",
				logger.NewStringField("sourceID", a.SourceID),
				logger.NewStringField("eventName", a.EventName),
				logger.NewStringField("trackingPlanID", a.TrackingPlanID),
			)
		case AnomalyTypeUnexpectedProperty:
			unexpectedPropCount++
			d.logger.Infon("anomaly detected: unexpected property",
				logger.NewStringField("sourceID", a.SourceID),
				logger.NewStringField("eventName", a.EventName),
				logger.NewStringField("propertyName", a.PropertyName),
				logger.NewStringField("trackingPlanID", a.TrackingPlanID),
			)
		}
	}

	// Emit metrics following the pattern from processor/trackingplan.go lines 155-159.
	tags := stats.Tags{
		"source":         sourceID,
		"trackingPlanId": trackingPlanID,
	}

	if unknownEventCount > 0 {
		d.statsFactory.NewTaggedStat("anomaly_unknown_events", stats.CountType, tags).Count(unknownEventCount)
	}
	if unexpectedPropCount > 0 {
		d.statsFactory.NewTaggedStat("anomaly_unexpected_properties", stats.CountType, tags).Count(unexpectedPropCount)
	}
}

// getEventType extracts the event type from a RudderStack event message.
// Returns "track", "identify", "page", "screen", "group", "alias", or empty string
// if the "type" field is missing or not a string.
//
// This helper uses simple type assertion rather than gjson because SingularEventT
// is already a map[string]any. Compare with types.GetRudderEventVal in
// processor/types/types.go lines 30-36 for the same pattern.
func getEventType(message types.SingularEventT) string {
	v, ok := message["type"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// getEventName extracts the event name from a RudderStack event message.
// This is the "event" field in track events (e.g., "Order Completed", "Product Viewed").
// Returns empty string if the "event" field is missing or not a string.
//
// Non-track event types (identify, page, screen, group, alias) typically do not have
// an "event" field; they are identified by their "type" field instead.
func getEventName(message types.SingularEventT) string {
	v, ok := message["event"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
