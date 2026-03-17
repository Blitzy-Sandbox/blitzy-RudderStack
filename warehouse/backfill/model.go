// Package backfill provides domain models and types for warehouse backfill operations.
//
// Backfill allows historical data sync for a specified date range, source, and warehouse
// destination. It integrates with the archiver (within its retention window) and
// staging files in object storage to re-process past events into the warehouse pipeline.
package backfill

import (
	"errors"
	"time"
)

// BackfillStatus represents the status of a backfill job.
// It follows the type-alias pattern used by model.UploadStatus for consistency
// and backward-compatible JSON serialization.
type BackfillStatus = string

const (
	// StatusPending indicates the backfill job has been created but not yet started.
	StatusPending BackfillStatus = "pending"

	// StatusInProgress indicates the backfill job is currently processing.
	StatusInProgress BackfillStatus = "in_progress"

	// StatusCompleted indicates the backfill job has finished successfully.
	StatusCompleted BackfillStatus = "completed"

	// StatusFailed indicates the backfill job has failed.
	StatusFailed BackfillStatus = "failed"
)

// BackfillJob represents a backfill operation record in the wh_backfill_jobs table.
// It stores all metadata required for tracking and orchestrating a backfill,
// including the date range to re-sync, ownership identifiers, and lifecycle timestamps.
type BackfillJob struct {
	// ID is the unique identifier for the backfill job (wh_backfill_jobs.id).
	ID int64 `json:"id"`

	// SourceID identifies the event source whose data is being backfilled.
	SourceID string `json:"source_id"`

	// DestinationID identifies the warehouse destination receiving the backfilled data.
	DestinationID string `json:"destination_id"`

	// WorkspaceID identifies the workspace owning this backfill job.
	WorkspaceID string `json:"workspace_id"`

	// StartDate is the inclusive start of the backfill date range.
	StartDate time.Time `json:"start_date"`

	// EndDate is the inclusive end of the backfill date range.
	EndDate time.Time `json:"end_date"`

	// Status tracks the current lifecycle state of the backfill job.
	Status BackfillStatus `json:"status"`

	// Metadata holds optional JSON-encoded metadata associated with the backfill job.
	// Using []byte instead of json.RawMessage to avoid importing encoding/json;
	// the two types are binary-compatible (json.RawMessage is defined as []byte).
	Metadata []byte `json:"metadata,omitempty"`

	// CreatedAt records when the backfill job was created.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt records when the backfill job was last updated.
	UpdatedAt time.Time `json:"updated_at"`
}

// BackfillRequest is the DTO for triggering a backfill via the HTTP API.
// It captures the required parameters: source, destination, and date range.
// JSON tags follow the jsonrs marshaling convention (wire-compatible with encoding/json tags).
type BackfillRequest struct {
	// SourceID identifies the event source to backfill from.
	SourceID string `json:"source_id"`

	// DestinationID identifies the warehouse destination to backfill into.
	DestinationID string `json:"destination_id"`

	// StartDate is the inclusive start of the backfill date range.
	StartDate time.Time `json:"start_date"`

	// EndDate is the inclusive end of the backfill date range.
	EndDate time.Time `json:"end_date"`
}

// BackfillResponse is the DTO returned after triggering a backfill.
// API contract: { "jobID": int64, "status": "Pending" }
type BackfillResponse struct {
	// JobID is the unique identifier of the created backfill job.
	JobID int64 `json:"jobID"`

	// Status is the initial status of the backfill job (typically StatusPending).
	Status BackfillStatus `json:"status"`
}

// Sentinel errors for backfill operations.
// These enable structured error classification in HTTP handlers and service layers.
var (
	// ErrBackfillDisabled is returned when the backfill feature is disabled
	// via the Warehouse.backfill.enabled configuration flag.
	ErrBackfillDisabled = errors.New("backfill feature is disabled")

	// ErrConcurrentLimitReached is returned when the maximum number of concurrent
	// backfill jobs (Warehouse.backfill.maxConcurrentJobs) has been reached.
	ErrConcurrentLimitReached = errors.New("concurrent backfill job limit reached")

	// ErrBackfillJobNotFound is returned when a backfill job with the given ID
	// does not exist in the wh_backfill_jobs table.
	ErrBackfillJobNotFound = errors.New("backfill job not found")

	// ErrInvalidDateRange is returned when the provided date range is invalid,
	// such as when start_date is after end_date or dates are zero-valued.
	ErrInvalidDateRange = errors.New("invalid date range")

	// ErrMissingSourceID is returned when source_id is not provided in the request.
	ErrMissingSourceID = errors.New("source_id is required")

	// ErrMissingDestinationID is returned when destination_id is not provided in the request.
	ErrMissingDestinationID = errors.New("destination_id is required")

	// ErrDateRangeExceedsMax is returned when the requested date range exceeds
	// the configured maximum (Warehouse.backfill.maxDateRangeDays, default 90 days).
	ErrDateRangeExceedsMax = errors.New("date range exceeds maximum allowed days")
)

// IsTerminal returns true if the backfill status is a terminal state.
// Terminal states are StatusCompleted and StatusFailed, meaning the job
// has finished processing and will not transition to any further state.
func IsTerminal(status BackfillStatus) bool {
	return status == StatusCompleted || status == StatusFailed
}

// IsActive returns true if the backfill status is an active (non-terminal) state.
// Active states are StatusPending and StatusInProgress, meaning the job
// is either awaiting processing or currently being executed.
func IsActive(status BackfillStatus) bool {
	return status == StatusPending || status == StatusInProgress
}
