// Package selectivesync provides domain models, configuration, and filtering logic
// for the warehouse selective sync feature (E-034).
//
// Selective sync allows users to include or exclude specific tables and columns
// from warehouse sync. Configuration is delivered via backend-config and persisted
// in the wh_selective_sync table, with runtime filtering applied at the load file
// generation stage of the upload state machine.
package selectivesync

import (
	"errors"
	"time"
)

// SelectiveSyncConfig represents a selective sync configuration for a source/destination pair.
// It defines which tables and columns should be excluded from warehouse sync.
// Stored in the wh_selective_sync table with JSONB columns for exclusion lists.
//
// An empty ExcludedTables slice and an empty ExcludedColumns map both represent the default
// behavior: all tables and all columns are included in the sync.
type SelectiveSyncConfig struct {
	// ID is the auto-generated primary key (wh_selective_sync.id).
	ID int64 `json:"id"`

	// SourceID identifies the RudderStack source.
	SourceID string `json:"source_id"`

	// DestinationID identifies the warehouse destination.
	DestinationID string `json:"destination_id"`

	// WorkspaceID identifies the workspace this config belongs to.
	WorkspaceID string `json:"workspace_id"`

	// ExcludedTables is the list of table names to exclude from sync.
	// An empty list means all tables are included.
	ExcludedTables []string `json:"excluded_tables"`

	// ExcludedColumns is a map of table name to a list of column names to exclude.
	// An empty map means all columns are included for all tables.
	// Example: {"users": ["email", "phone"], "tracks": ["ip"]}
	ExcludedColumns map[string][]string `json:"excluded_columns"`

	// CreatedAt is the timestamp when this config was first created.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the timestamp of the last update to this config.
	UpdatedAt time.Time `json:"updated_at"`
}

// SelectiveSyncRequest is the DTO for creating or updating selective sync configuration
// via the HTTP API (PUT /v1/warehouse/selective-sync).
// JSON tags use the standard format compatible with jsonrs.
type SelectiveSyncRequest struct {
	// SourceID identifies the RudderStack source (required).
	SourceID string `json:"source_id"`

	// DestinationID identifies the warehouse destination (required).
	DestinationID string `json:"destination_id"`

	// WorkspaceID identifies the workspace (optional — derived from auth context if not provided).
	WorkspaceID string `json:"workspace_id,omitempty"`

	// ExcludedTables is the list of table names to exclude from sync.
	ExcludedTables []string `json:"excluded_tables,omitempty"`

	// ExcludedColumns is a map of table name to column names to exclude.
	ExcludedColumns map[string][]string `json:"excluded_columns,omitempty"`
}

// SelectiveSyncResponse is the DTO returned after updating selective sync configuration.
// API contract: {"status": "updated", "sourceID": "...", "destID": "..."}
type SelectiveSyncResponse struct {
	// Status indicates the result of the operation (e.g., "updated").
	Status string `json:"status"`

	// SourceID echoes back the source ID from the request.
	SourceID string `json:"sourceID"`

	// DestID echoes back the destination ID from the request.
	DestID string `json:"destID"`
}

// Sentinel errors for selective sync operations.
// These enable structured error classification in HTTP handlers and service layers,
// following the pattern established by warehouse/internal/model/upload.go and
// warehouse/backfill/model.go.
var (
	// ErrSelectiveSyncDisabled is returned when the selective sync feature is disabled
	// via the Warehouse.selectiveSync.enabled configuration flag.
	ErrSelectiveSyncDisabled = errors.New("selective sync feature is disabled")

	// ErrSelectiveSyncNotFound is returned when no selective sync configuration exists
	// for the given source/destination pair.
	ErrSelectiveSyncNotFound = errors.New("selective sync configuration not found")

	// ErrMissingSourceID is returned when source_id is not provided in a request.
	ErrMissingSourceID = errors.New("source_id is required")

	// ErrMissingDestinationID is returned when destination_id is not provided in a request.
	ErrMissingDestinationID = errors.New("destination_id is required")
)
