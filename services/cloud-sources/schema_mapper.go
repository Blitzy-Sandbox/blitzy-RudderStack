package cloudsources

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
)

// cloudSourcesLibraryName is the library name embedded in every SegmentEvent
// produced by the cloud source schema mapper. This identifies the origin
// of the event in downstream analytics systems.
const cloudSourcesLibraryName = "rudder-cloud-sources"

// cloudSourcesLibraryVersion is the proof-of-concept version identifier
// for the cloud source ingestion framework (Sprint 2-3, E-009).
const cloudSourcesLibraryVersion = "0.1.0"

// userIDFieldNames defines the ordered list of common field names
// to search when extracting a user identifier from cloud source event data.
// The search is case-insensitive and returns the first non-empty match.
var userIDFieldNames = []string{
	"userId",
	"user_id",
	"id",
	"customer_id",
	"email",
}

// groupIDFieldNames defines the ordered list of common field names
// to search when extracting a group identifier from cloud source event data.
var groupIDFieldNames = []string{
	"groupId",
	"group_id",
	"organization_id",
	"org_id",
	"account_id",
	"company_id",
}

// BaseSchemaMapper provides a default implementation of the SchemaMapper interface.
// It transforms third-party cloud source events into Segment Spec events
// (identify, track, group). The mapper handles event type routing, user
// identity extraction, timestamp normalization, and Segment context metadata
// generation.
//
// This is a proof-of-concept implementation for the E-009 cloud source
// ingestion framework. It follows the adapter transformation pattern
// established by gateway/webhook/webhookTransformer.go but operates on
// cloud source Event structs rather than raw HTTP requests.
type BaseSchemaMapper struct {
	// SourceType identifies the cloud source connector producing events
	// (e.g., "stripe", "salesforce", "hubspot"). This value is embedded
	// in the context.source.type field of every produced SegmentEvent.
	SourceType string
}

// NewBaseSchemaMapper creates a new BaseSchemaMapper for the given source type.
// The sourceType parameter identifies the cloud source connector and is
// included in the context metadata of every produced SegmentEvent.
//
// Example:
//
//	mapper := NewBaseSchemaMapper("stripe")
//	events, err := mapper.MapToSegmentSpec(rawEvent)
func NewBaseSchemaMapper(sourceType string) *BaseSchemaMapper {
	return &BaseSchemaMapper{
		SourceType: sourceType,
	}
}

// MapToSegmentSpec transforms a cloud source Event into one or more Segment
// Spec-compliant SegmentEvent instances. The method routes events based on
// their Type field:
//
//   - "identify" → SegmentEvent with Type="identify", Traits from event.Data
//   - "track"    → SegmentEvent with Type="track", Properties from event.Data, Event name from event.Name
//   - "group"    → SegmentEvent with Type="group", Traits from event.Data, GroupID extracted from data
//   - default    → Falls back to "track" type for unrecognized event types
//
// Each produced SegmentEvent includes a generated UUID MessageID, RFC 3339
// timestamps, library metadata in Context, and either UserID or AnonymousID
// depending on whether a user identifier can be extracted from the event data.
//
// The method uses jsonrs for deep-copying event data maps to prevent mutation
// of the original Event.Data between caller and callee.
func (m *BaseSchemaMapper) MapToSegmentSpec(event Event) ([]SegmentEvent, error) {
	now := time.Now()

	// Deep-copy event data to prevent mutation of the caller's map.
	dataCopy, err := deepCopyData(event.Data)
	if err != nil {
		return nil, fmt.Errorf("schema mapper: failed to deep-copy event data for event %q: %w", event.ID, err)
	}

	// Resolve user identity: prefer explicit UserID from event, then extract
	// from data fields, finally generate an anonymous ID.
	userID := event.UserID
	if userID == "" {
		userID = extractUserID(dataCopy)
	}
	anonymousID := ""
	if userID == "" {
		anonymousID = uuid.New().String()
	}

	// Resolve original timestamp: use event.Timestamp if present, otherwise now.
	originalTimestamp := now
	if !event.Timestamp.IsZero() {
		originalTimestamp = event.Timestamp
	}

	// Build the base segment event with common fields.
	base := SegmentEvent{
		MessageID:         uuid.New().String(),
		UserID:            userID,
		AnonymousID:       anonymousID,
		Context:           buildContext(m.SourceType),
		Timestamp:         now.Format(time.RFC3339),
		OriginalTimestamp:  originalTimestamp.Format(time.RFC3339),
		SentAt:            now.Format(time.RFC3339),
		Integrations:      map[string]interface{}{"All": true},
	}

	// Route the event based on its type.
	eventType := strings.ToLower(strings.TrimSpace(event.Type))
	switch eventType {
	case "identify":
		base.Type = "identify"
		base.Traits = dataCopy
	case "track":
		base.Type = "track"
		base.Event = resolveEventName(event)
		base.Properties = dataCopy
	case "group":
		base.Type = "group"
		base.Traits = dataCopy
		base.GroupID = extractGroupID(dataCopy)
	default:
		// Unrecognized event types default to "track" to ensure no data is lost.
		base.Type = "track"
		base.Event = resolveEventName(event)
		base.Properties = dataCopy
	}

	return []SegmentEvent{base}, nil
}

// buildContext constructs the standard Segment context metadata object
// containing library identification and source type information. This
// context is attached to every SegmentEvent produced by the schema mapper.
//
// The resulting context follows the Segment Spec context structure:
//
//	{
//	  "library": {"name": "rudder-cloud-sources", "version": "0.1.0"},
//	  "source":  {"type": "<sourceType>"}
//	}
func buildContext(sourceType string) map[string]interface{} {
	return map[string]interface{}{
		"library": map[string]interface{}{
			"name":    cloudSourcesLibraryName,
			"version": cloudSourcesLibraryVersion,
		},
		"source": map[string]interface{}{
			"type": sourceType,
		},
	}
}

// extractUserID searches the event data map for a user identifier using
// a prioritized list of common field names. The search checks both the
// exact field name and a case-insensitive match against the data keys.
// Returns an empty string if no user identifier is found.
//
// Checked field names (in priority order):
// "userId", "user_id", "id", "customer_id", "email"
func extractUserID(data map[string]interface{}) string {
	if data == nil {
		return ""
	}
	for _, fieldName := range userIDFieldNames {
		if val, ok := data[fieldName]; ok {
			if strVal, isStr := val.(string); isStr && strVal != "" {
				return strVal
			}
		}
	}
	// Case-insensitive fallback: iterate once through keys looking for matches.
	for key, val := range data {
		lowerKey := strings.ToLower(key)
		for _, fieldName := range userIDFieldNames {
			if lowerKey == strings.ToLower(fieldName) {
				if strVal, isStr := val.(string); isStr && strVal != "" {
					return strVal
				}
			}
		}
	}
	return ""
}

// extractGroupID searches the event data map for a group identifier using
// a prioritized list of common field names. Returns an empty string if no
// group identifier is found.
//
// Checked field names (in priority order):
// "groupId", "group_id", "organization_id", "org_id", "account_id", "company_id"
func extractGroupID(data map[string]interface{}) string {
	if data == nil {
		return ""
	}
	for _, fieldName := range groupIDFieldNames {
		if val, ok := data[fieldName]; ok {
			if strVal, isStr := val.(string); isStr && strVal != "" {
				return strVal
			}
		}
	}
	// Case-insensitive fallback.
	for key, val := range data {
		lowerKey := strings.ToLower(key)
		for _, fieldName := range groupIDFieldNames {
			if lowerKey == strings.ToLower(fieldName) {
				if strVal, isStr := val.(string); isStr && strVal != "" {
					return strVal
				}
			}
		}
	}
	return ""
}

// resolveEventName determines the event name for track-type SegmentEvents.
// It uses the Event.Name field if present; otherwise falls back to
// Event.Type, and finally to a generic "unknown_event" sentinel.
func resolveEventName(event Event) string {
	if event.Name != "" {
		return event.Name
	}
	if event.Type != "" {
		return event.Type
	}
	return "unknown_event"
}

// deepCopyData creates a deep copy of the event data map by marshalling
// to JSON and unmarshalling back into a new map. This ensures the schema
// mapper does not mutate the original Event.Data reference.
//
// Uses jsonrs (github.com/rudderlabs/rudder-go-kit/jsonrs) as mandated
// by the repository's depguard linting rules — encoding/json is banned.
func deepCopyData(data map[string]interface{}) (map[string]interface{}, error) {
	if data == nil {
		return make(map[string]interface{}), nil
	}
	if len(data) == 0 {
		return make(map[string]interface{}), nil
	}

	raw, err := jsonrs.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal event data: %w", err)
	}

	var copied map[string]interface{}
	if err := jsonrs.Unmarshal(raw, &copied); err != nil {
		return nil, fmt.Errorf("unmarshal event data: %w", err)
	}
	return copied, nil
}
