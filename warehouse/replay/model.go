// Package replay provides domain models and types for the warehouse replay feature (E-035).
//
// The warehouse replay feature enables re-processing of archived events through the
// warehouse pipeline, bypassing real-time Router delivery. This package defines the
// request/response DTOs, job tracking models, archived event structures, sentinel
// errors, and helper functions used throughout the replay subsystem.
package replay

import (
	"errors"
	"fmt"
	"time"
)

// ReplayStatus represents the status of a warehouse replay job.
// Uses a type alias (not a distinct type) to match the model.UploadStatus pattern,
// ensuring status values are plain strings that are easily serializable and backward compatible.
type ReplayStatus = string

const (
	// StatusPending indicates the replay job has been created but not yet started.
	StatusPending ReplayStatus = "pending"

	// StatusInProgress indicates the replay job is currently processing archived events.
	StatusInProgress ReplayStatus = "in_progress"

	// StatusCompleted indicates the replay job has finished successfully.
	StatusCompleted ReplayStatus = "completed"

	// StatusFailed indicates the replay job has failed.
	StatusFailed ReplayStatus = "failed"
)

const (
	// WarehouseReplayHeader is the HTTP header used to tag events for warehouse-only routing.
	// When set to "true", the Processor bypasses real-time Router delivery and routes
	// events exclusively to the warehouse pipeline.
	WarehouseReplayHeader = "X-Warehouse-Replay"

	// WarehouseReplayHeaderValue is the expected value for the warehouse replay header.
	WarehouseReplayHeaderValue = "true"

	// DefaultReplayType is the default replay type used when no explicit type is specified
	// in the replay request. A "full" replay re-processes all archived events in the
	// given time range through the warehouse pipeline.
	DefaultReplayType = "full"
)

// Sentinel errors for replay error classification.
// These are used by the HTTP handler layer (handler.go) to map service errors
// to appropriate HTTP status codes and structured error responses.
var (
	// ErrReplayDisabled is returned when the warehouse replay feature is disabled
	// via the Warehouse.replay.enabled configuration flag.
	ErrReplayDisabled = errors.New("replay feature is disabled")

	// ErrConcurrentLimitReached is returned when the maximum number of concurrent
	// replay jobs (Warehouse.replay.maxConcurrentReplays) has been reached.
	ErrConcurrentLimitReached = errors.New("concurrent replay job limit reached")

	// ErrReplayJobNotFound is returned when a replay job with the given ID does not exist
	// in the wh_backfill_jobs or equivalent tracking table.
	ErrReplayJobNotFound = errors.New("replay job not found")

	// ErrInvalidReplayRequest is returned for invalid replay request parameters.
	// It is wrapped with specific field-level details via fmt.Errorf in Validate().
	ErrInvalidReplayRequest = errors.New("invalid replay request")

	// ErrGatewayNotConfigured is returned when the replay handler's GatewayClient
	// dependency is nil. This occurs when the gateway is not yet available during
	// application startup. Mapped to HTTP 503 Service Unavailable in the handler.
	ErrGatewayNotConfigured = errors.New("replay gateway client not configured")
)

// ReplayRequest is the DTO for triggering a warehouse replay via the HTTP API.
// It specifies the source, destination, and time range for replaying archived events.
//
// API contract: POST /v1/warehouse/replay
//
//	{
//	  "source_id": "...",
//	  "destination_id": "...",
//	  "start_time": "2024-01-01T00:00:00Z",
//	  "end_time": "2024-01-31T23:59:59Z",
//	  "replay_type": "full"
//	}
type ReplayRequest struct {
	// SourceID is the source identifier for the events to replay.
	SourceID string `json:"source_id"`

	// DestinationID is the warehouse destination to replay events to.
	DestinationID string `json:"destination_id"`

	// StartTime is the beginning of the time range for replay (inclusive).
	StartTime time.Time `json:"start_time"`

	// EndTime is the end of the time range for replay (inclusive).
	EndTime time.Time `json:"end_time"`

	// ReplayType specifies the type of replay (e.g., "full", "incremental").
	// Optional; defaults to DefaultReplayType ("full") if not specified.
	ReplayType string `json:"replay_type,omitempty"`
}

// Validate checks that the ReplayRequest has all required fields and valid values.
// It returns a wrapped ErrInvalidReplayRequest with a specific field-level message
// on validation failure, enabling error chain inspection via errors.Is().
func (r ReplayRequest) Validate() error {
	if r.SourceID == "" {
		return fmt.Errorf("%w: source_id is required", ErrInvalidReplayRequest)
	}
	if r.DestinationID == "" {
		return fmt.Errorf("%w: destination_id is required", ErrInvalidReplayRequest)
	}
	if r.StartTime.IsZero() {
		return fmt.Errorf("%w: start_time is required", ErrInvalidReplayRequest)
	}
	if r.EndTime.IsZero() {
		return fmt.Errorf("%w: end_time is required", ErrInvalidReplayRequest)
	}
	if r.StartTime.After(r.EndTime) {
		return fmt.Errorf("%w: start_time must be before end_time", ErrInvalidReplayRequest)
	}
	return nil
}

// ReplayResponse is the DTO returned after triggering a warehouse replay.
//
// API contract: POST /v1/warehouse/replay → { "jobID": int64, "status": "pending" }
type ReplayResponse struct {
	// JobID is the unique identifier of the created replay job.
	JobID int64 `json:"jobID"`

	// Status is the initial status of the replay job (always StatusPending on creation).
	Status ReplayStatus `json:"status"`
}

// ReplayJob represents a warehouse replay operation with its full lifecycle state.
// This is the primary domain entity for tracking replay job execution from creation
// through completion or failure. It maps to the replay job tracking storage.
type ReplayJob struct {
	// ID is the unique identifier of the replay job.
	ID int64 `json:"id"`

	// SourceID is the source identifier for the replayed events.
	SourceID string `json:"source_id"`

	// DestinationID is the warehouse destination for the replayed events.
	DestinationID string `json:"destination_id"`

	// StartTime is the beginning of the replay time range.
	StartTime time.Time `json:"start_time"`

	// EndTime is the end of the replay time range.
	EndTime time.Time `json:"end_time"`

	// ReplayType specifies the type of replay (e.g., "full", "incremental").
	ReplayType string `json:"replay_type"`

	// Status is the current status of the replay job.
	Status ReplayStatus `json:"status"`

	// TotalEvents is the total number of events processed in this replay.
	TotalEvents int64 `json:"total_events"`

	// TotalBatches is the total number of batches sent to the Gateway.
	TotalBatches int64 `json:"total_batches"`

	// Error contains error details if the replay job failed.
	// Empty string when the job is in a non-failed state.
	Error string `json:"error,omitempty"`

	// CreatedAt is the timestamp when the replay job was created.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the timestamp when the replay job was last updated.
	UpdatedAt time.Time `json:"updated_at"`
}

// ArchivedEvent represents a single gateway event deserialized from the archived
// gzip JSONL format produced by the archiver. The Payload field contains the
// complete raw event JSON for re-injection into the Gateway replay endpoint.
type ArchivedEvent struct {
	// MessageID is the unique message identifier for the event.
	MessageID string `json:"messageId"`

	// Type is the event type (track, identify, page, screen, group, alias).
	Type string `json:"type"`

	// Event is the event name (for track events). Empty for non-track events.
	Event string `json:"event,omitempty"`

	// UserID is the user identifier associated with the event.
	UserID string `json:"userId,omitempty"`

	// AnonymousID is the anonymous user identifier.
	AnonymousID string `json:"anonymousId,omitempty"`

	// ReceivedAt is the timestamp when the event was originally received by the Gateway.
	ReceivedAt time.Time `json:"receivedAt"`

	// Payload is the complete raw event JSON payload for re-injection into the Gateway.
	// Uses []byte instead of json.RawMessage to avoid encoding/json import dependency.
	// This is binary-compatible since json.RawMessage is defined as type RawMessage []byte.
	Payload []byte `json:"payload,omitempty"`
}

// ArchivedEventBatch represents a batch of archived events retrieved from the archiver,
// typically stored as gzipped JSONL. This model is used by the ArchivedEventRetriever
// to deliver batches of events to the ReplayHandler for Gateway injection.
type ArchivedEventBatch struct {
	// SourceID is the source that generated these events.
	SourceID string `json:"source_id"`

	// Data contains the gzip-compressed JSONL event data.
	Data []byte `json:"data"`

	// StartTime is the earliest event timestamp in this batch.
	StartTime time.Time `json:"start_time"`

	// EndTime is the latest event timestamp in this batch.
	EndTime time.Time `json:"end_time"`

	// EventCount is the number of events in this batch.
	EventCount int64 `json:"event_count"`
}

// IsTerminal returns true if the given replay status is a terminal state.
// Terminal states are StatusCompleted and StatusFailed — once a job reaches
// a terminal state, it will not transition to any other state.
func IsTerminal(status ReplayStatus) bool {
	return status == StatusCompleted || status == StatusFailed
}

// IsActive returns true if the given replay status is an active (non-terminal) state.
// Active states are StatusPending and StatusInProgress — the job is still in progress
// and may transition to other states.
func IsActive(status ReplayStatus) bool {
	return status == StatusPending || status == StatusInProgress
}
