// Package cloudsources implements the cloud source ingestion framework
// for the RudderStack data plane. It provides interface definitions and
// base implementations for both polling-based (REST API) and webhook-based
// (event-driven) cloud source connectors.
//
// This package is a proof-of-concept (Sprint 2-3, E-009) designed to
// address the 140 cloud app source gap identified in the source catalog
// parity analysis. It defines the connector interface pattern and provides
// a reference implementation with the Stripe webhook connector.
//
// Architecture:
//   - CloudSource: Top-level lifecycle interface (Start/Stop/Status)
//   - Poller: Polling-based ingestion with cursor pagination
//   - WebhookReceiver: Webhook-based ingestion with HMAC validation
//   - SchemaMapper: Third-party API → Segment Spec event transformation
//   - Registry: Thread-safe connector plugin registration
package cloudsources

import (
	"context"
	"net/http"
	"time"
)

// CloudSource is the top-level interface for cloud source connectors.
// Each connector implements this interface to participate in the
// cloud source ingestion framework. It provides lifecycle management
// (Start/Stop) and status reporting.
//
// Connectors may implement additional interfaces (Poller, WebhookReceiver)
// to indicate their ingestion mode. The framework uses these interfaces
// for mode-specific initialization and orchestration.
type CloudSource interface {
	// Start initializes the cloud source connector and begins ingestion.
	// For polling sources, this starts the background polling loop.
	// For webhook sources, this registers the webhook endpoint.
	// The context controls the lifecycle — cancelling it triggers shutdown.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the cloud source connector.
	// It should complete any in-flight operations before returning.
	// The context provides a deadline for the shutdown procedure;
	// implementations should respect ctx.Done() to avoid blocking indefinitely.
	Stop(ctx context.Context) error

	// Status returns the current operational status of the connector.
	// This is a non-blocking call that returns the most recently known state.
	Status() SourceStatus
}

// Poller defines the interface for polling-based cloud source connectors.
// Polling sources periodically fetch data from third-party REST APIs
// using cursor-based pagination (e.g., Salesforce, HubSpot).
//
// Implementations must be safe for concurrent use; the framework may call
// GetCursor from monitoring goroutines while Poll is executing.
type Poller interface {
	// Poll executes a single polling cycle, fetching events from the source API.
	// Returns the fetched events and any error encountered.
	// The context should be checked for cancellation before making external API calls.
	Poll(ctx context.Context) ([]Event, error)

	// SetCursor stores the pagination cursor for the next polling cycle.
	// The cursor format is source-specific (e.g., timestamp, offset, page token).
	// Implementations must ensure thread-safe access to the cursor state.
	SetCursor(cursor string)

	// GetCursor retrieves the current pagination cursor.
	// Returns an empty string if no cursor has been set.
	// Implementations must ensure thread-safe access to the cursor state.
	GetCursor() string
}

// WebhookReceiver defines the interface for webhook-based cloud source connectors.
// Webhook sources receive real-time events via HTTP callbacks from third-party
// services (e.g., Stripe webhooks, SendGrid event hooks).
//
// Implementations must validate webhook authenticity (typically via HMAC signatures)
// before processing the payload. The Validate and Transform methods may be called
// concurrently from multiple HTTP handler goroutines.
type WebhookReceiver interface {
	// Validate verifies the authenticity of an incoming webhook request.
	// Typically checks HMAC signatures to prevent webhook spoofing.
	// Returns true if the request is authentic, false otherwise.
	// Implementations should use constant-time comparison for signature checks.
	// The request body must be preserved (read and restored) for subsequent Transform calls.
	Validate(r *http.Request) (bool, error)

	// Transform converts a validated webhook request into Segment Spec events.
	// The request body is parsed and mapped to one or more SegmentEvent instances.
	// A single webhook payload may produce multiple Segment events
	// (e.g., a Stripe charge event may produce both a track and an identify event).
	Transform(r *http.Request) ([]SegmentEvent, error)
}

// SchemaMapper defines the interface for transforming third-party API responses
// into Segment Spec events. Each cloud source connector provides its own
// schema mapping implementation to handle source-specific data formats.
//
// The schema mapper is the core transformation layer that normalizes
// heterogeneous cloud source data into a unified Segment Spec event stream.
type SchemaMapper interface {
	// MapToSegmentSpec transforms a cloud source event into one or more
	// Segment Spec events (identify, track, group). A single source event
	// may produce multiple Segment events (e.g., a Stripe charge event
	// produces both a track event for the charge and an identify for the customer).
	// Returns an empty slice (not nil) when no mapping applies.
	MapToSegmentSpec(event Event) ([]SegmentEvent, error)
}

// Event represents a raw event from a cloud source connector.
// This is the intermediate representation between the source-specific
// API response and the Segment Spec event format. Cloud source connectors
// produce Event instances, which are then transformed into SegmentEvent
// instances by the SchemaMapper.
type Event struct {
	// ID is the unique identifier for this event from the source system.
	// Used for deduplication and replay protection.
	ID string `json:"id"`

	// Type is the event classification (e.g., "track", "identify", "group").
	// This guides the SchemaMapper in producing the correct SegmentEvent type.
	Type string `json:"type"`

	// Name is the event name (e.g., "charge.succeeded", "customer.created").
	// For track events, this becomes the SegmentEvent.Event field.
	Name string `json:"name"`

	// SourceType identifies the cloud source (e.g., "stripe", "salesforce").
	// Used for routing, logging, and context metadata.
	SourceType string `json:"sourceType"`

	// Timestamp is when the event occurred in the source system.
	// This becomes the OriginalTimestamp in the produced SegmentEvent.
	Timestamp time.Time `json:"timestamp"`

	// Data contains the raw event payload as key-value pairs.
	// The SchemaMapper extracts properties, traits, and user identifiers from this map.
	Data map[string]interface{} `json:"data"`

	// UserID is the user identifier extracted from the event, if available.
	// When present, this is propagated as the SegmentEvent.UserID field.
	UserID string `json:"userId,omitempty"`
}

// SegmentEvent represents a Segment Spec-compliant event ready for
// ingestion into the RudderStack pipeline. This is the output format
// of the schema mapping layer. All fields follow the Segment HTTP
// Tracking API specification for maximum SDK compatibility.
type SegmentEvent struct {
	// Type is the Segment Spec event type: "identify", "track", "page", "screen", "group", "alias".
	Type string `json:"type"`

	// MessageID is the unique identifier for this event.
	// Generated by the schema mapper using UUID v4.
	MessageID string `json:"messageId"`

	// UserID is the known user identifier.
	// Populated from the source event's user ID field when available.
	UserID string `json:"userId,omitempty"`

	// AnonymousID is the anonymous user identifier.
	// Generated when no UserID is available from the source event.
	AnonymousID string `json:"anonymousId,omitempty"`

	// Event is the event name (for track events only).
	// Derived from the source Event.Name field.
	Event string `json:"event,omitempty"`

	// Properties contains event properties (for track events).
	// Populated from the source event data by the SchemaMapper.
	Properties map[string]interface{} `json:"properties,omitempty"`

	// Traits contains user traits (for identify events) or group traits (for group events).
	// Populated from the source event data by the SchemaMapper.
	Traits map[string]interface{} `json:"traits,omitempty"`

	// GroupID is the group identifier (for group events).
	// Extracted from the source event data when the event type is "group".
	GroupID string `json:"groupId,omitempty"`

	// Context contains contextual metadata (library info, source info).
	// Always includes library.name ("rudder-cloud-sources") and library.version.
	Context map[string]interface{} `json:"context"`

	// Timestamp is the event timestamp in RFC 3339 format.
	// Set to the current time when the event is processed by the schema mapper.
	Timestamp string `json:"timestamp"`

	// OriginalTimestamp is the timestamp from the source system in RFC 3339 format.
	// Preserves the original event time for accurate temporal analysis.
	OriginalTimestamp string `json:"originalTimestamp,omitempty"`

	// SentAt is when the event was sent to the pipeline in RFC 3339 format.
	// Set to the current time when the event enters the RudderStack pipeline.
	SentAt string `json:"sentAt,omitempty"`

	// Integrations controls destination-specific routing.
	// Allows selective forwarding to specific destinations.
	Integrations map[string]interface{} `json:"integrations,omitempty"`
}

// SourceStatus reports the operational status of a cloud source connector.
// This struct is returned by CloudSource.Status() to provide visibility
// into connector health and throughput metrics.
type SourceStatus struct {
	// Name is the connector name/type (e.g., "stripe", "salesforce").
	Name string `json:"name"`

	// Healthy indicates whether the connector is operating normally.
	// A connector is healthy when it can successfully reach the source API
	// and process events without persistent errors.
	Healthy bool `json:"healthy"`

	// Message provides additional status information or error details.
	// When Healthy is false, this should contain a human-readable error description.
	Message string `json:"message,omitempty"`

	// LastPollTime is the timestamp of the last successful poll (for polling sources).
	// Nil for webhook-based sources or sources that have not yet completed a poll cycle.
	LastPollTime *time.Time `json:"lastPollTime,omitempty"`

	// EventsIngested is the total number of events processed since the connector started.
	// This counter is monotonically increasing and resets on connector restart.
	EventsIngested int64 `json:"eventsIngested"`
}
