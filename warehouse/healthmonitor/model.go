package healthmonitor

import (
	"errors"
	"time"
)

// ErrHealthNotFound is returned when no health record is found for the given criteria.
// Follows the sentinel error pattern established in warehouse/internal/model/upload.go
// (e.g., ErrUploadNotFound, ErrLoadFileNotFound).
var ErrHealthNotFound = errors.New("health record not found")

// SyncHealth represents a single sync health record for a warehouse upload.
// Each record captures the outcome of one upload cycle including duration, row counts,
// error categorization, and schema changes.
//
// Persisted to the wh_sync_health table created by migration 000044.
// Table columns map directly to struct fields:
//
//	id            -> ID
//	upload_id     -> UploadID
//	source_id     -> SourceID
//	destination_id -> DestinationID
//	dest_type     -> DestType
//	source_type   -> SourceType
//	workspace_id  -> WorkspaceID
//	status        -> Status
//	duration_ms   -> DurationMs
//	rows_synced   -> RowsSynced
//	rows_failed   -> RowsFailed
//	error_category -> ErrorCategory
//	schema_changes -> SchemaChanges
//	created_at    -> CreatedAt
type SyncHealth struct {
	// ID is the auto-generated primary key from the wh_sync_health table.
	ID int64 `json:"id"`

	// UploadID references the wh_uploads.id for the upload this health record belongs to.
	UploadID int64 `json:"uploadID"`

	// SourceID is the RudderStack source identifier that produced the events.
	SourceID string `json:"sourceID"`

	// DestinationID is the warehouse destination identifier where events were synced.
	DestinationID string `json:"destinationID"`

	// DestType is the warehouse destination type (e.g., "SNOWFLAKE", "BQ", "RS").
	DestType string `json:"destType"`

	// SourceType is the source type (e.g., "web", "android", "ios", "cloud").
	SourceType string `json:"sourceType"`

	// WorkspaceID is the workspace that owns this source-destination pair.
	WorkspaceID string `json:"workspaceID"`

	// SourceName is the human-readable name of the RudderStack source.
	// Used to compute the warehouseID composite tag for Prometheus metric emission,
	// matching the tag computation in warehouse/router/upload_stats.go warehouseTagName().
	SourceName string `json:"sourceName"`

	// DestName is the human-readable name of the warehouse destination.
	// Used to compute the warehouseID composite tag for Prometheus metric emission,
	// matching the tag computation in warehouse/router/upload_stats.go warehouseTagName().
	DestName string `json:"destName"`

	// Status is the final upload status (e.g., "exported_data", "aborted", "failed").
	// Maps to warehouse/internal/model/upload.go status constants.
	Status string `json:"status"`

	// DurationMs is the total sync duration in milliseconds from upload start to completion.
	DurationMs int64 `json:"durationMs"`

	// RowsSynced is the total number of rows successfully synced to the warehouse.
	RowsSynced int64 `json:"rowsSynced"`

	// RowsFailed is the total number of rows that failed during sync.
	RowsFailed int64 `json:"rowsFailed"`

	// ErrorCategory classifies the error type if the sync failed.
	// Maps to warehouse/internal/model/upload.go JobErrorType constants
	// (e.g., "permission_error", "column_count_error", "uncategorised").
	ErrorCategory string `json:"errorCategory"`

	// SchemaChanges stores arbitrary JSON data describing schema changes detected
	// during this sync cycle. Stored as JSONB in the wh_sync_health table.
	// Uses []byte to avoid importing encoding/json, consistent with the backfill
	// package's approach for JSONB data and the project's depguard rules that
	// restrict encoding/json usage. All Marshal/Unmarshal operations use jsonrs.
	SchemaChanges []byte `json:"schemaChanges"`

	// CreatedAt records when this health record was persisted to the wh_sync_health table.
	CreatedAt time.Time `json:"createdAt"`
}

// DurationStats provides aggregated duration statistics for sync operations
// within a reporting window. All values are in milliseconds.
type DurationStats struct {
	// Min is the minimum sync duration in milliseconds across all uploads in the window.
	Min int64 `json:"min"`

	// Max is the maximum sync duration in milliseconds across all uploads in the window.
	Max int64 `json:"max"`

	// Avg is the average sync duration in milliseconds across all uploads in the window.
	Avg int64 `json:"avg"`

	// P95 is the 95th percentile sync duration in milliseconds across all uploads in the window.
	P95 int64 `json:"p95"`
}

// HealthSummaryResponse is the top-level response for the GET /v1/warehouse/health endpoint.
// It aggregates health data grouped by source, with each source containing per-destination metrics.
//
// Response format:
//
//	{
//	    "sources": [{
//	        "sourceID": "...",
//	        "sourceType": "...",
//	        "destinations": [{
//	            "destID": "...",
//	            "destType": "...",
//	            "syncDuration": {"min": 0, "max": 0, "avg": 0, "p95": 0},
//	            "rowsSynced": 12345,
//	            "errorRate": 0.02,
//	            "lastSync": "2024-01-01T00:00:00Z"
//	        }]
//	    }]
//	}
type HealthSummaryResponse struct {
	// Sources contains health data for each source, with nested destination metrics.
	Sources []*SourceHealth `json:"sources"`
}

// SourceHealth contains health data for all destinations of a single source.
// Groups destination-level health metrics under a common source identifier.
type SourceHealth struct {
	// SourceID is the RudderStack source identifier.
	SourceID string `json:"sourceID"`

	// SourceType is the source type (e.g., "web", "android", "ios", "cloud").
	SourceType string `json:"sourceType"`

	// WorkspaceID is the workspace that owns this source. Required for tenant isolation
	// and Prometheus metric tagging per AAP standard warehouse tag set.
	WorkspaceID string `json:"workspaceID"`

	// SourceName is the human-readable name of the source, used for computing the
	// warehouseID composite tag in Prometheus metric emission.
	SourceName string `json:"sourceName"`

	// Destinations contains health metrics for each destination connected to this source.
	Destinations []*DestinationHealth `json:"destinations"`
}

// DestinationHealth contains health metrics for a specific source-destination pair.
// This is the core response element matching the AAP specification for the health API.
type DestinationHealth struct {
	// DestID is the warehouse destination identifier.
	DestID string `json:"destID"`

	// DestType is the warehouse destination type (e.g., "SNOWFLAKE", "BQ", "RS").
	DestType string `json:"destType"`

	// DestName is the human-readable name of the warehouse destination, used for
	// computing the warehouseID composite tag in Prometheus metric emission.
	DestName string `json:"destName"`

	// SyncDuration provides aggregated duration statistics (min/max/avg/p95) in milliseconds.
	SyncDuration DurationStats `json:"syncDuration"`

	// RowsSynced is the total number of rows successfully synced in the reporting window.
	RowsSynced int64 `json:"rowsSynced"`

	// ErrorRate is the ratio of failed syncs to total syncs (0.0 to 1.0).
	// For example, 0.02 means 2% of syncs failed.
	ErrorRate float64 `json:"errorRate"`

	// ErrorCount is the absolute number of failed syncs in the reporting window.
	ErrorCount int64 `json:"errorCount"`

	// ErrorCategory is the most common error category for failed syncs, if any.
	// Omitted from JSON when empty.
	ErrorCategory string `json:"errorCategory,omitempty"`

	// LastSync is the ISO 8601 (RFC 3339) timestamp of the most recent sync completion.
	LastSync string `json:"lastSync"`

	// SchemaChanges is the number of schema changes detected in the reporting window.
	SchemaChanges int64 `json:"schemaChanges"`

	// PreviousRowsSynced is the total rows synced in the previous reporting window.
	// Used for row count anomaly detection in alerting. Omitted from JSON when zero.
	PreviousRowsSynced int64 `json:"previousRowsSynced,omitempty"`
}

// SourceHealthResponse is the response for the filtered health endpoint
// GET /v1/warehouse/health/{sourceID}/{destID}.
// Contains health data for a specific source with its destinations.
type SourceHealthResponse struct {
	// SourceID is the RudderStack source identifier.
	SourceID string `json:"sourceID"`

	// SourceType is the source type (e.g., "web", "android", "ios", "cloud").
	SourceType string `json:"sourceType"`

	// WorkspaceID is the workspace that owns this source.
	WorkspaceID string `json:"workspaceID"`

	// SourceName is the human-readable name of the source, used for computing the
	// warehouseID composite tag in Prometheus metric emission.
	SourceName string `json:"sourceName"`

	// Destinations contains health metrics for the filtered destinations.
	Destinations []*DestinationHealth `json:"destinations"`
}

// HealthStatus represents the overall health state of a source-destination sync.
// Used by the alerting evaluator to classify sync health into discrete states.
type HealthStatus string

const (
	// HealthStatusHealthy indicates all syncs are completing successfully
	// within expected duration and row count thresholds.
	HealthStatusHealthy HealthStatus = "healthy"

	// HealthStatusDegraded indicates some syncs are failing or exceeding
	// duration thresholds, but the system is still partially operational.
	HealthStatusDegraded HealthStatus = "degraded"

	// HealthStatusUnhealthy indicates a critical failure state where
	// syncs are consistently failing or severely degraded.
	HealthStatusUnhealthy HealthStatus = "unhealthy"
)
