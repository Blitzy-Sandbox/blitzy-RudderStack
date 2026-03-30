// Package schema – common_schema.go
//
// This file defines the common JSON Schema draft-07 definition that is applied
// to ALL events from connected sources before any event-specific or tracking
// plan schemas are evaluated. It ensures every incoming event has the minimum
// required structure regardless of its type (track, identify, page, etc.).
//
// The schema is lazily compiled into a singleton via sync.Once and cached for
// concurrent access across the processing pipeline. Convenience functions are
// provided for single-event and batch validation.
package schema

import (
	"fmt"
	"sync"
)

// ---------------------------------------------------------------------------
// Common Event Schema Definition (JSON Schema draft-07)
// ---------------------------------------------------------------------------

// CommonEventSchema defines the base JSON Schema draft-07 that is applied to
// ALL events from connected sources before any event-specific or tracking plan
// schemas are evaluated.
//
// It enforces the minimum required structure for valid RudderStack events:
//   - messageId (string, required): unique message identifier
//   - type (string, required, enum): one of the standard event types
//   - timestamp (string, required, date-time): ISO 8601 / RFC 3339 timestamp
//
// Design decisions:
//   - required: ["messageId", "type", "timestamp"] — these three fields are
//     universally required for all RudderStack events, derived from
//     processor/types.SingularEventT and TrackingPlanValidationEvent metadata.
//   - type.enum restricts to standard event types matching the
//     RudderStack/Segment event specification, ensuring only recognized event
//     types flow through the pipeline.
//   - additionalProperties: true — CRITICAL: the common schema must NOT reject
//     events with extra properties. Events carry varied payloads depending on
//     source, SDK, and tracking plan configuration. The common schema only
//     validates the minimum base structure.
//   - format: "date-time" enforces ISO 8601 / RFC 3339 format for all
//     timestamp fields, matching the processor's timestamp handling.
var CommonEventSchema = []byte(`{
	"$schema": "http://json-schema.org/draft-07/schema#",
	"type": "object",
	"required": ["messageId", "type", "timestamp"],
	"properties": {
		"messageId": {
			"type": "string",
			"description": "Unique message identifier for the event"
		},
		"type": {
			"type": "string",
			"enum": ["track", "identify", "page", "screen", "group", "alias"],
			"description": "Event type following the RudderStack/Segment event specification"
		},
		"timestamp": {
			"type": "string",
			"format": "date-time",
			"description": "ISO 8601 / RFC 3339 timestamp when the event occurred"
		},
		"anonymousId": {
			"type": "string",
			"description": "Anonymous user identifier"
		},
		"userId": {
			"type": "string",
			"description": "Known user identifier"
		},
		"context": {
			"type": "object",
			"description": "Context metadata about the event (device, library, OS, etc.)"
		},
		"properties": {
			"type": "object",
			"description": "Event properties (primarily for track events)"
		},
		"traits": {
			"type": "object",
			"description": "User traits (primarily for identify events)"
		},
		"integrations": {
			"type": "object",
			"description": "Per-destination integration flags"
		},
		"originalTimestamp": {
			"type": "string",
			"format": "date-time",
			"description": "Original event timestamp from the client SDK"
		},
		"sentAt": {
			"type": "string",
			"format": "date-time",
			"description": "Timestamp when the event was sent from the SDK"
		},
		"receivedAt": {
			"type": "string",
			"format": "date-time",
			"description": "Timestamp when the server received the event"
		},
		"event": {
			"type": "string",
			"description": "Event name (for track events)"
		},
		"channel": {
			"type": "string",
			"description": "Source channel (web, mobile, server, etc.)"
		}
	},
	"additionalProperties": true
}`)

// ---------------------------------------------------------------------------
// Compiled Common Schema Singleton
// ---------------------------------------------------------------------------

var (
	compiledCommonSchema     *CompiledSchema
	compiledCommonSchemaOnce sync.Once
	compiledCommonSchemaErr  error
)

// GetCompiledCommonSchema returns a pre-compiled version of the common event
// schema. It is lazily compiled on first call and cached for all subsequent
// calls. Thread-safe via sync.Once.
//
// The compiled schema can be passed directly to ValidateWithCompiled and
// ValidateBatchWithCompiled for efficient repeated validation. Returns a
// non-nil error if the common schema definition contains invalid JSON or
// violates the JSON Schema draft-07 meta-schema.
func GetCompiledCommonSchema() (*CompiledSchema, error) {
	compiledCommonSchemaOnce.Do(func() {
		compiledCommonSchema, compiledCommonSchemaErr = CompileSchema(CommonEventSchema)
		if compiledCommonSchemaErr != nil {
			pkgLogger.Errorn("failed to compile common event schema: " + compiledCommonSchemaErr.Error())
		}
	})
	return compiledCommonSchema, compiledCommonSchemaErr
}

// ---------------------------------------------------------------------------
// Convenience Validation Functions
// ---------------------------------------------------------------------------

// ValidateCommonSchema validates a single event against the common event
// schema. This should be called BEFORE event-specific tracking plan validation
// to ensure the event has the minimum required structure.
//
// Returns a slice of ValidationError for each constraint violation, or nil if
// the event is fully valid. Returns a non-nil error only if the common schema
// compilation fails (programming error, never expected at runtime after
// initial successful compilation).
func ValidateCommonSchema(event map[string]any) ([]ValidationError, error) {
	compiled, err := GetCompiledCommonSchema()
	if err != nil {
		return nil, fmt.Errorf("common schema compilation failed: %w", err)
	}
	return ValidateWithCompiled(compiled, event)
}

// ValidateCommonSchemaBatch validates a batch of events against the common
// event schema. Returns per-event validation errors — one slice per event —
// and a non-nil error only if the common schema compilation fails.
//
// Each event's errors are independent. A nil or empty inner slice means that
// event passes the common schema. This is the preferred entry point for
// pipeline batch processing where multiple events flow together.
func ValidateCommonSchemaBatch(events []map[string]any) ([][]ValidationError, error) {
	compiled, err := GetCompiledCommonSchema()
	if err != nil {
		return nil, fmt.Errorf("common schema compilation failed: %w", err)
	}
	return ValidateBatchWithCompiled(compiled, events)
}
