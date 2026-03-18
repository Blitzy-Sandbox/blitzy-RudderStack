// Package selectivesync provides per-table and per-column sync filtering
// for warehouse destinations, allowing users to include or exclude specific
// tables and columns from warehouse sync operations.
//
// This file implements the HTTP request handlers for the selective sync API
// endpoints:
//
//   - PUT  /v1/warehouse/selective-sync                        — Update selective sync configuration
//   - GET  /v1/warehouse/selective-sync/{sourceID}/{destID}    — Retrieve selective sync configuration
//
// The handler follows the Chi router middleware pattern established in
// warehouse/api/http.go (lines 161-205) and mirrors the handler architecture
// from warehouse/backfill/handler.go.
//
// JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs exclusively,
// as mandated by the project's .golangci.yml depguard rule — encoding/json must
// never be used.
package selectivesync

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// SelectiveSyncConfigurer is the service interface that the HTTP handler depends on
// for selective sync operations. It decouples the handler from the concrete
// SelectiveSyncService, enabling straightforward mock-based unit testing
// following the established warehouse handler patterns.
//
// The SelectiveSyncService (service.go) implements this interface.
type SelectiveSyncConfigurer interface {
	// GetConfig retrieves the selective sync configuration for a source/destination pair.
	// Returns ErrSelectiveSyncDisabled if the feature is disabled.
	// Returns ErrSelectiveSyncNotFound if no configuration exists.
	GetConfig(ctx context.Context, sourceID, destID string) (*SelectiveSyncConfig, error)

	// UpdateConfig creates or updates the selective sync configuration for a
	// source/destination pair. Returns a SelectiveSyncResponse with the updated
	// status, or an error if the feature is disabled or validation fails.
	UpdateConfig(ctx context.Context, req SelectiveSyncRequest) (*SelectiveSyncResponse, error)
}

// Handler implements the HTTP request handlers for the selective sync API.
// It delegates all business logic to the SelectiveSyncConfigurer service interface
// and provides consistent JSON error/success response formatting following the
// warehouse API conventions.
type Handler struct {
	log     logger.Logger
	service SelectiveSyncConfigurer
}

// NewHandler creates a new selective sync HTTP handler with the given logger
// and service. The logger is derived as a child logger with the
// "selectivesync.handler" component name, following the logger hierarchy
// pattern from warehouse/api/http.go (log.Child("api")) and
// warehouse/backfill/handler.go (log.Child("backfill.handler")).
func NewHandler(log logger.Logger, service SelectiveSyncConfigurer) *Handler {
	return &Handler{
		log:     log.Child("selectivesync.handler"),
		service: service,
	}
}

// UpdateSelectiveSync handles PUT /v1/warehouse/selective-sync requests.
// It parses the JSON request body into a SelectiveSyncRequest, validates all
// required fields (source_id, destination_id), delegates to the
// SelectiveSyncConfigurer service, and returns the result as JSON.
//
// Request body (JSON):
//
//	{
//	  "source_id": "...",
//	  "destination_id": "...",
//	  "workspace_id": "...",           // optional
//	  "excluded_tables": ["..."],      // optional
//	  "excluded_columns": {"t": [...]} // optional
//	}
//
// Success response (HTTP 200):
//
//	{"status": "updated", "sourceID": "...", "destID": "..."}
//
// Error responses:
//
//	400 Bad Request    — invalid JSON, missing source_id or destination_id
//	403 Forbidden      — selective sync feature disabled
//	500 Internal Error — unexpected service failure
func (h *Handler) UpdateSelectiveSync(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	// Parse the JSON request body using jsonrs (mandated by depguard).
	var req SelectiveSyncRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.log.Warnn("invalid JSON in selective sync request body", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	// Validate required fields are present and non-empty.
	if req.SourceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "source_id is required")
		return
	}
	if req.DestinationID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "destination_id is required")
		return
	}

	// Delegate to the service layer for business logic validation and upsert.
	resp, err := h.service.UpdateConfig(r.Context(), req)
	if err != nil {
		h.handleServiceError(w, err)
		return
	}

	// Return the successful update response.
	h.writeJSONResponse(w, http.StatusOK, resp)
}

// GetSelectiveSync handles GET /v1/warehouse/selective-sync/{sourceID}/{destID}
// requests. It extracts the sourceID and destID path parameters using
// chi.URLParam, queries the service for the configuration, and returns the
// full SelectiveSyncConfig as JSON.
//
// Success response (HTTP 200):
//
//	Full SelectiveSyncConfig JSON with id, source_id, destination_id,
//	workspace_id, excluded_tables, excluded_columns, created_at, updated_at
//
// Error responses:
//
//	400 Bad Request    — missing sourceID or destID path parameters
//	403 Forbidden      — selective sync feature disabled
//	404 Not Found      — no configuration exists for the pair
//	500 Internal Error — unexpected service failure
func (h *Handler) GetSelectiveSync(w http.ResponseWriter, r *http.Request) {
	// Extract path parameters using chi.URLParam, consistent with the Chi
	// router pattern in warehouse/api/http.go and warehouse/backfill/handler.go.
	sourceID := chi.URLParam(r, "sourceID")
	destID := chi.URLParam(r, "destID")

	if sourceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "sourceID is required")
		return
	}
	if destID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "destID is required")
		return
	}

	// Query the service for the selective sync configuration.
	cfg, err := h.service.GetConfig(r.Context(), sourceID, destID)
	if err != nil {
		if errors.Is(err, ErrSelectiveSyncNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "selective sync configuration not found")
			return
		}
		h.handleServiceError(w, err)
		return
	}

	// Return the full configuration.
	h.writeJSONResponse(w, http.StatusOK, cfg)
}

// handleServiceError maps well-known service-level sentinel errors to appropriate
// HTTP status codes and writes a structured JSON error response. Unrecognized
// errors are logged at error level and returned as 500 Internal Server Error.
//
// Error-to-status mapping:
//   - ErrSelectiveSyncDisabled → 403 Forbidden
//   - ErrSelectiveSyncNotFound → 404 Not Found
//   - (unknown)                → 500 Internal Server Error
func (h *Handler) handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSelectiveSyncDisabled):
		h.writeErrorResponse(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrSelectiveSyncNotFound):
		h.writeErrorResponse(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrMissingSourceID), errors.Is(err, ErrMissingDestinationID):
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
	default:
		h.log.Errorn("selective sync operation failed", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "internal server error")
	}
}

// errorResponse is the standard error response DTO, consistent with the
// warehouse API error format: {"status": "error", "message": "..."}.
// This format is used by all warehouse HTTP endpoints for error reporting.
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// writeErrorResponse writes a structured JSON error response to the client.
// The response body follows the format {"status": "error", "message": "..."},
// consistent with the error response pattern established across the warehouse
// API layer (warehouse/api/http.go, warehouse/backfill/handler.go).
//
// The Content-Type header is set to application/json before writing the status
// code to ensure correct header ordering per the HTTP spec.
func (h *Handler) writeErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = jsonrs.NewEncoder(w).Encode(errorResponse{
		Status:  "error",
		Message: message,
	})
}

// writeJSONResponse writes a successful JSON response to the client.
// It sets the Content-Type header to application/json, writes the status code,
// and encodes the data payload using jsonrs (per the project's depguard rule).
//
// Encoding errors are logged at error level but not surfaced to the client
// because the status code has already been written to the response at that point
// — re-writing headers would panic in the Go HTTP server.
func (h *Handler) writeJSONResponse(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := jsonrs.NewEncoder(w).Encode(data); err != nil {
		h.log.Errorn("failed to encode response", obskit.Error(err))
	}
}
