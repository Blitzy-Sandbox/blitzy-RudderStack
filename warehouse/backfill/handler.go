// Package backfill provides the HTTP handler layer for the warehouse backfill API.
//
// This file implements the HTTP request handlers for the backfill endpoints:
//   - POST /v1/warehouse/backfill   — Trigger a backfill with date range, source, and destination
//   - GET  /v1/warehouse/backfill/{jobID} — Retrieve backfill job status
//
// The handler follows the Chi router middleware pattern established in warehouse/api/http.go.
// JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs exclusively,
// as mandated by the project's .golangci.yml depguard rule — encoding/json must never be used.
package backfill

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// maxVarcharLength is the maximum allowable length for string identifier fields
// (source_id, destination_id, workspace_id) that map to VARCHAR(64) columns in
// the wh_backfill_jobs table. Pre-validating at the HTTP layer prevents
// PostgreSQL constraint violation errors from surfacing as generic 500 errors.
const maxVarcharLength = 64

// BackfillTrigger defines the service interface that the HTTP handler depends on.
// It decouples the handler from the concrete BackfillService, enabling straightforward
// mock-based unit testing following established warehouse patterns.
type BackfillTrigger interface {
	// Trigger initiates a new backfill job based on the provided request parameters.
	// It returns a BackfillResponse containing the created job ID and initial status,
	// or an error if validation fails, the feature is disabled, or the concurrent limit is reached.
	Trigger(ctx context.Context, req BackfillRequest) (*BackfillResponse, error)

	// GetStatus retrieves the current state of a backfill job identified by jobID.
	// It returns the full BackfillJob record or ErrBackfillJobNotFound if no such job exists.
	GetStatus(ctx context.Context, jobID int64) (*BackfillJob, error)
}

// Handler implements the HTTP request handlers for the backfill API.
// It delegates business logic to the BackfillTrigger service interface and
// provides consistent JSON error/success response formatting.
type Handler struct {
	log     logger.Logger
	service BackfillTrigger
}

// NewHandler creates a new Handler with the given logger and service.
// The logger is derived as a child logger with the "backfill.handler" component name,
// following the logger hierarchy pattern from warehouse/api/http.go (log.Child("api")).
func NewHandler(log logger.Logger, service BackfillTrigger) *Handler {
	return &Handler{
		log:     log.Child("backfill.handler"),
		service: service,
	}
}

// TriggerBackfill handles POST /v1/warehouse/backfill requests.
// It parses the JSON request body into a BackfillRequest, validates all required fields,
// delegates to the BackfillTrigger service, and returns the created job as JSON.
//
// Request body: {"source_id": "...", "destination_id": "...", "start_date": "RFC3339", "end_date": "RFC3339"}
// Success response (200): {"jobID": int64, "status": "pending"}
// Error responses: 400 (bad request), 403 (disabled), 429 (limit), 500 (internal)
func (h *Handler) TriggerBackfill(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	// Parse the JSON request body using jsonrs (mandated by depguard).
	var req BackfillRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.log.Warnn("invalid JSON in backfill request body", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	// Validate all required fields are present and non-zero.
	if req.SourceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "source_id is required")
		return
	}
	if req.DestinationID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "destination_id is required")
		return
	}

	// Reject null bytes (\u0000) in string fields. PostgreSQL VARCHAR columns
	// reject null bytes, causing a database-level error that would surface as an
	// opaque 500 Internal Server Error. By validating at the handler layer, we
	// return a clear 400 Bad Request with an actionable error message.
	if containsNullByte(req.SourceID) {
		h.writeErrorResponse(w, http.StatusBadRequest, "source_id contains invalid characters")
		return
	}
	if containsNullByte(req.DestinationID) {
		h.writeErrorResponse(w, http.StatusBadRequest, "destination_id contains invalid characters")
		return
	}
	if containsNullByte(req.WorkspaceID) {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspace_id contains invalid characters")
		return
	}

	// Validate field lengths to prevent PostgreSQL VARCHAR(64) overflow errors
	// at the database layer (wh_backfill_jobs table columns are VARCHAR(64)).
	// This returns a user-friendly 400 error instead of a generic 500.
	if len(req.SourceID) > maxVarcharLength {
		h.writeErrorResponse(w, http.StatusBadRequest, "source_id exceeds maximum length of 64 characters")
		return
	}
	if len(req.DestinationID) > maxVarcharLength {
		h.writeErrorResponse(w, http.StatusBadRequest, "destination_id exceeds maximum length of 64 characters")
		return
	}
	if len(req.WorkspaceID) > maxVarcharLength {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspace_id exceeds maximum length of 64 characters")
		return
	}

	if req.StartDate.IsZero() {
		h.writeErrorResponse(w, http.StatusBadRequest, "start_date is required")
		return
	}
	if req.EndDate.IsZero() {
		h.writeErrorResponse(w, http.StatusBadRequest, "end_date is required")
		return
	}

	// Validate date ordering: start must precede end.
	if req.StartDate.After(req.EndDate) {
		h.writeErrorResponse(w, http.StatusBadRequest, "start_date must be before end_date")
		return
	}

	// Delegate to the service layer for business logic validation and job creation.
	resp, err := h.service.Trigger(r.Context(), req)
	if err != nil {
		h.handleServiceError(w, err)
		return
	}

	// Return the created backfill job response with HTTP 201 Created
	// since the POST endpoint creates a new backfill job resource.
	h.writeJSONResponse(w, http.StatusCreated, resp)
}

// GetBackfillStatus handles GET /v1/warehouse/backfill/{jobID} requests.
// It extracts the jobID path parameter, queries the service for the job's current state,
// and returns the full BackfillJob record as JSON.
//
// Success response (200): Full BackfillJob JSON
// Error responses: 400 (invalid ID), 404 (not found), 500 (internal)
func (h *Handler) GetBackfillStatus(w http.ResponseWriter, r *http.Request) {
	// Extract the jobID path parameter using chi.URLParam,
	// consistent with the Chi router pattern in warehouse/api/http.go.
	jobIDStr := chi.URLParam(r, "jobID")
	jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
	if err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	// Query the service for the backfill job status.
	job, err := h.service.GetStatus(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, ErrBackfillJobNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "backfill job not found")
			return
		}
		h.log.Errorn("failed to get backfill status", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to get backfill status")
		return
	}

	// Return the full job record.
	h.writeJSONResponse(w, http.StatusOK, job)
}

// handleServiceError maps well-known service-level sentinel errors to appropriate
// HTTP status codes. Unrecognized errors are logged and returned as 500 Internal Server Error.
//
// Error-to-status mapping:
//   - ErrBackfillDisabled       → 403 Forbidden
//   - ErrConcurrentLimitReached → 429 Too Many Requests
//   - ErrInvalidDateRange       → 400 Bad Request
//   - ErrDateRangeExceedsMax    → 400 Bad Request
//   - (unknown)                 → 500 Internal Server Error
func (h *Handler) handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrBackfillDisabled):
		h.writeErrorResponse(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrConcurrentLimitReached):
		h.writeErrorResponse(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, ErrInvalidDateRange):
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrDateRangeExceedsMax):
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
	default:
		h.log.Errorn("backfill trigger failed", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "internal server error")
	}
}

// errorResponse is the standard error response DTO, consistent with
// the warehouse API error format: {"status": "error", "message": "..."}.
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// writeErrorResponse writes a structured JSON error response to the client.
// The response body follows the format {"status": "error", "message": "..."},
// consistent with the error response pattern in warehouse/api/http.go.
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
// and encodes the data payload using jsonrs. Encoding errors are logged but
// not surfaced to the client (the status code has already been written).
func (h *Handler) writeJSONResponse(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := jsonrs.NewEncoder(w).Encode(data); err != nil {
		h.log.Errorn("failed to encode response", obskit.Error(err))
	}
}

// containsNullByte reports whether s contains a Unicode null byte (U+0000).
// PostgreSQL VARCHAR columns reject null bytes, causing a database-level error
// that would otherwise surface as a 500 Internal Server Error. This helper
// enables handler-level validation to return 400 Bad Request instead.
func containsNullByte(s string) bool {
	return strings.ContainsRune(s, 0)
}
