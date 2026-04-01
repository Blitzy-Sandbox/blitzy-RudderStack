// Package api provides the HTTP handler layer for the Tracking Plan Management REST API.
//
// This file implements the HTTP request handlers for the tracking plan endpoints:
//   - POST   /tracking-plans                — Create a new tracking plan
//   - GET    /tracking-plans                — List all tracking plans for a workspace
//   - GET    /tracking-plans/{id}           — Get a single tracking plan
//   - PUT    /tracking-plans/{id}           — Update a tracking plan (creates new version)
//   - DELETE /tracking-plans/{id}           — Delete a tracking plan
//   - GET    /tracking-plans/{id}/versions  — Get version history
//   - POST   /tracking-plans/{id}/import    — Import tracking plan from CSV
//   - GET    /tracking-plans/{id}/export    — Export tracking plan as CSV
//
// The handler follows the Chi router middleware pattern established in warehouse/backfill/handler.go.
// JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs exclusively,
// as mandated by the project's .golangci.yml depguard rule — encoding/json must never be used.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

const (
	// WorkspaceIDHeader is the HTTP header used to identify the workspace.
	// This follows the convention from gateway/handle_http_auth.go where
	// workspace information is extracted from request context and headers.
	WorkspaceIDHeader = "X-Rudder-Workspace-Id"

	// maxCSVBodySize is the maximum allowed size for CSV import request bodies (10 MB).
	// This limit prevents memory exhaustion from excessively large uploads,
	// following security best practice for unbounded body reads.
	maxCSVBodySize = 10 * 1024 * 1024 // 10 MB

	// maxProtocolsBodySize is the maximum allowed size for tracking plan management
	// JSON request bodies (10 MB). Tracking plan schemas can be large (JSON Schema
	// draft-07 with many event types) so we allow a generous but bounded limit.
	maxProtocolsBodySize int64 = 10 * 1024 * 1024 // 10 MB
)

// validateTrackingPlanID validates that the provided ID string is a valid positive
// integer, suitable for use as a BIGSERIAL primary key. This prevents SQL-level
// type errors and potential information leakage when non-numeric strings are
// passed as IDs to PostgreSQL BIGINT columns.
func validateTrackingPlanID(id string) error {
	if id == "" {
		return fmt.Errorf("tracking plan ID is required")
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid tracking plan ID format: must be a numeric identifier")
	}
	if n <= 0 {
		return fmt.Errorf("invalid tracking plan ID: must be a positive integer")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Request and Response Types
// ---------------------------------------------------------------------------

// CreateTrackingPlanRequest represents the JSON body for creating a new tracking plan.
type CreateTrackingPlanRequest struct {
	Name              string          `json:"name"`
	Schema            json.RawMessage `json:"schema"`             // JSON Schema draft-07 definition
	EnforcementConfig json.RawMessage `json:"enforcement_config"` // Block/Omit/Allow per source per call type
}

// UpdateTrackingPlanRequest represents the JSON body for updating an existing tracking plan.
type UpdateTrackingPlanRequest struct {
	Name              string          `json:"name,omitempty"`
	Schema            json.RawMessage `json:"schema,omitempty"`
	EnforcementConfig json.RawMessage `json:"enforcement_config,omitempty"`
	Changelog         string `json:"changelog,omitempty"` // Human-readable description of changes for version history
}

// TrackingPlanResponse represents a tracking plan in API responses.
type TrackingPlanResponse struct {
	ID                string          `json:"id"`
	WorkspaceID       string          `json:"workspace_id"`
	Name              string          `json:"name"`
	Schema            json.RawMessage `json:"schema"`
	Version           int             `json:"version"`
	EnforcementConfig json.RawMessage `json:"enforcement_config"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// TrackingPlanVersionResponse represents a tracking plan version in API responses.
type TrackingPlanVersionResponse struct {
	ID             string          `json:"id"`
	TrackingPlanID string          `json:"tracking_plan_id"`
	Version        int             `json:"version"`
	Schema         json.RawMessage `json:"schema"`
	Changelog      string          `json:"changelog"`
	CreatedAt      time.Time       `json:"created_at"`
}

// CreateResponse is the response returned after successfully creating a tracking plan.
type CreateResponse struct {
	ID string `json:"id"`
}

// ---------------------------------------------------------------------------
// Sentinel Errors
// ---------------------------------------------------------------------------

var (
	// ErrTrackingPlanNotFound is returned when a tracking plan is not found.
	ErrTrackingPlanNotFound = errors.New("tracking plan not found")

	// ErrVersionNotFound is returned when a specific version is not found.
	ErrVersionNotFound = errors.New("tracking plan version not found")

	// ErrInvalidSchema is returned when the tracking plan schema is invalid JSON Schema.
	ErrInvalidSchema = errors.New("invalid tracking plan schema")

	// ErrInvalidCSV is returned when the CSV import data is malformed.
	ErrInvalidCSV = errors.New("invalid CSV data")
)

// ---------------------------------------------------------------------------
// TrackingPlanService Interface
// ---------------------------------------------------------------------------

// TrackingPlanService defines the service interface that the HTTP handler depends on.
// It decouples the handler from the concrete storage implementation, enabling
// straightforward mock-based unit testing following the BackfillTrigger pattern
// in warehouse/backfill/handler.go:35-44.
type TrackingPlanService interface {
	// Create creates a new tracking plan and returns its generated ID.
	// The workspaceID parameter is required for multi-tenant isolation.
	Create(ctx context.Context, workspaceID string, req CreateTrackingPlanRequest) (string, error)

	// Get retrieves a single tracking plan by workspace and ID.
	Get(ctx context.Context, workspaceID, id string) (*TrackingPlanResponse, error)

	// Update updates an existing tracking plan (creates a new version).
	Update(ctx context.Context, workspaceID, id string, req UpdateTrackingPlanRequest) error

	// Delete removes a tracking plan and all its versions.
	Delete(ctx context.Context, workspaceID, id string) error

	// List returns all tracking plans for a workspace.
	List(ctx context.Context, workspaceID string) ([]TrackingPlanResponse, error)

	// GetVersions returns the version history for a tracking plan.
	GetVersions(ctx context.Context, workspaceID, trackingPlanID string) ([]TrackingPlanVersionResponse, error)

	// ImportCSV imports a tracking plan schema from CSV data.
	ImportCSV(ctx context.Context, workspaceID, trackingPlanID string, csvData []byte) error

	// ExportCSV exports a tracking plan schema as CSV data.
	ExportCSV(ctx context.Context, workspaceID, trackingPlanID string) ([]byte, error)
}

// ---------------------------------------------------------------------------
// Handler Struct and Constructor
// ---------------------------------------------------------------------------

// Handler implements the HTTP request handlers for the tracking plan management API.
// It delegates business logic to the TrackingPlanService interface and provides
// consistent JSON error/success response formatting.
// Following the Handler pattern from warehouse/backfill/handler.go:46-52.
type Handler struct {
	log     logger.Logger
	service TrackingPlanService
}

// NewHandler creates a new Handler with the given logger and service.
// The logger is derived as a child logger with the "protocols.api" component name,
// following the logger hierarchy pattern from warehouse/backfill/handler.go:57-61.
func NewHandler(log logger.Logger, service TrackingPlanService) *Handler {
	return &Handler{
		log:     log.Child("protocols.api"),
		service: service,
	}
}

// ---------------------------------------------------------------------------
// CRUD Handler Methods
// ---------------------------------------------------------------------------

// CreateTrackingPlan handles POST /tracking-plans requests.
// It parses the JSON request body into a CreateTrackingPlanRequest, validates required fields,
// delegates to the TrackingPlanService, and returns the created tracking plan ID as JSON.
//
// Request body: {"name": "...", "schema": {...}, "enforcement_config": {...}}
// Success response (201): {"id": "<generated>"}
// Error responses: 400 (bad request), 500 (internal)
func (h *Handler) CreateTrackingPlan(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	// Extract and validate workspace ID from the request header.
	workspaceID, ok := extractWorkspaceID(r)
	if !ok {
		h.writeErrorResponse(w, http.StatusBadRequest, "missing workspace ID header")
		return
	}

	// Apply body size limit to prevent memory exhaustion from oversized payloads.
	r.Body = http.MaxBytesReader(w, r.Body, maxProtocolsBodySize)

	// Parse the JSON request body using jsonrs (mandated by depguard).
	var req CreateTrackingPlanRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON request body: %s", err.Error()))
		return
	}

	// Validate that the tracking plan name is present.
	if req.Name == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "name is required")
		return
	}

	// Delegate to the service layer for business logic and persistence.
	// The workspace ID is passed directly to enforce multi-tenant isolation
	// at the storage layer, ensuring tracking plans are scoped to their workspace.
	id, err := h.service.Create(r.Context(), workspaceID, req)
	if err != nil {
		h.handleServiceError(w, err)
		return
	}

	// Return the created tracking plan ID with HTTP 201 Created,
	// following warehouse/backfill/handler.go:149.
	h.writeJSONResponse(w, http.StatusCreated, CreateResponse{ID: id})
}

// GetTrackingPlan handles GET /tracking-plans/{id} requests.
// It extracts the tracking plan ID from the URL path parameter, queries the service,
// and returns the full tracking plan record as JSON.
//
// Success response (200): Full TrackingPlanResponse JSON
// Error responses: 400 (missing workspace), 404 (not found), 500 (internal)
func (h *Handler) GetTrackingPlan(w http.ResponseWriter, r *http.Request) {
	// Extract and validate the tracking plan ID from the chi URL parameter.
	// Numeric validation prevents SQL type errors and potential info leakage.
	id := chi.URLParam(r, "id")
	if err := validateTrackingPlanID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Extract and validate workspace ID.
	workspaceID, ok := extractWorkspaceID(r)
	if !ok {
		h.writeErrorResponse(w, http.StatusBadRequest, "missing workspace ID header")
		return
	}

	// Query the service for the tracking plan.
	tp, err := h.service.Get(r.Context(), workspaceID, id)
	if err != nil {
		h.handleServiceError(w, err)
		return
	}

	// Return the full tracking plan record.
	h.writeJSONResponse(w, http.StatusOK, tp)
}

// ListTrackingPlans handles GET /tracking-plans requests.
// It returns all tracking plans for the workspace identified by the workspace ID header.
//
// Success response (200): JSON array of TrackingPlanResponse (empty [] if none)
// Error responses: 400 (missing workspace), 500 (internal)
func (h *Handler) ListTrackingPlans(w http.ResponseWriter, r *http.Request) {
	// Extract and validate workspace ID.
	workspaceID, ok := extractWorkspaceID(r)
	if !ok {
		h.writeErrorResponse(w, http.StatusBadRequest, "missing workspace ID header")
		return
	}

	// Query the service for all tracking plans in this workspace.
	plans, err := h.service.List(r.Context(), workspaceID)
	if err != nil {
		h.handleServiceError(w, err)
		return
	}

	// Ensure the slice is never nil so JSON serialization produces
	// an empty array [] instead of null when no tracking plans exist.
	// Following warehouse/healthmonitor/handler.go:97 pattern.
	if plans == nil {
		plans = []TrackingPlanResponse{}
	}

	h.writeJSONResponse(w, http.StatusOK, plans)
}

// UpdateTrackingPlan handles PUT /tracking-plans/{id} requests.
// It parses the JSON update body, delegates to the service (which creates a new version),
// and returns a success confirmation.
//
// Request body: {"name": "...", "schema": {...}, "enforcement_config": {...}, "changelog": "..."}
// Success response (200): {"status": "ok"}
// Error responses: 400 (bad request/invalid schema), 404 (not found), 500 (internal)
func (h *Handler) UpdateTrackingPlan(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	// Extract and validate tracking plan ID.
	id := chi.URLParam(r, "id")
	if err := validateTrackingPlanID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID, ok := extractWorkspaceID(r)
	if !ok {
		h.writeErrorResponse(w, http.StatusBadRequest, "missing workspace ID header")
		return
	}

	// Apply body size limit to prevent memory exhaustion from oversized payloads.
	r.Body = http.MaxBytesReader(w, r.Body, maxProtocolsBodySize)

	// Parse the JSON request body.
	var req UpdateTrackingPlanRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON request body: %s", err.Error()))
		return
	}

	// Delegate to service layer for update and versioning.
	if err := h.service.Update(r.Context(), workspaceID, id, req); err != nil {
		h.handleServiceError(w, err)
		return
	}

	h.writeJSONResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// DeleteTrackingPlan handles DELETE /tracking-plans/{id} requests.
// It removes the tracking plan and all associated versions.
//
// Success response: 204 No Content (no body)
// Error responses: 400 (missing workspace), 404 (not found), 500 (internal)
func (h *Handler) DeleteTrackingPlan(w http.ResponseWriter, r *http.Request) {
	// Extract and validate tracking plan ID.
	id := chi.URLParam(r, "id")
	if err := validateTrackingPlanID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID, ok := extractWorkspaceID(r)
	if !ok {
		h.writeErrorResponse(w, http.StatusBadRequest, "missing workspace ID header")
		return
	}

	// Delegate to service layer for deletion.
	if err := h.service.Delete(r.Context(), workspaceID, id); err != nil {
		h.handleServiceError(w, err)
		return
	}

	// Return 204 No Content following services/rsources/http/http.go:80 pattern.
	w.WriteHeader(http.StatusNoContent)
}

// GetVersionHistory handles GET /tracking-plans/{id}/versions requests.
// It returns the version history for a specific tracking plan.
//
// Success response (200): JSON array of TrackingPlanVersionResponse
// Error responses: 400 (missing workspace), 404 (not found), 500 (internal)
func (h *Handler) GetVersionHistory(w http.ResponseWriter, r *http.Request) {
	// Extract and validate tracking plan ID.
	id := chi.URLParam(r, "id")
	if err := validateTrackingPlanID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID, ok := extractWorkspaceID(r)
	if !ok {
		h.writeErrorResponse(w, http.StatusBadRequest, "missing workspace ID header")
		return
	}

	// Query the service for version history.
	versions, err := h.service.GetVersions(r.Context(), workspaceID, id)
	if err != nil {
		h.handleServiceError(w, err)
		return
	}

	// Ensure the slice is never nil for consistent JSON array serialization.
	if versions == nil {
		versions = []TrackingPlanVersionResponse{}
	}

	h.writeJSONResponse(w, http.StatusOK, versions)
}

// ImportCSV handles POST /tracking-plans/{id}/import requests.
// It reads the full CSV request body and passes it to the service for import.
//
// Request body: Raw CSV data
// Success response (200): {"status": "ok"}
// Error responses: 400 (invalid CSV/missing workspace), 404 (not found), 500 (internal)
func (h *Handler) ImportCSV(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	// Extract and validate tracking plan ID.
	id := chi.URLParam(r, "id")
	if err := validateTrackingPlanID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID, ok := extractWorkspaceID(r)
	if !ok {
		h.writeErrorResponse(w, http.StatusBadRequest, "missing workspace ID header")
		return
	}

	// Wrap the request body with a size limit to prevent memory exhaustion
	// from excessively large CSV uploads (defense against DoS via large payloads).
	r.Body = http.MaxBytesReader(w, r.Body, maxCSVBodySize)

	// Read the full CSV request body using io.ReadAll (now size-limited).
	csvData, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to read request body: %s", err.Error()))
		return
	}

	// Validate that the CSV body is non-empty.
	if len(csvData) == 0 {
		h.writeErrorResponse(w, http.StatusBadRequest, "request body is empty")
		return
	}

	// Delegate to service layer for CSV import and schema update.
	if err := h.service.ImportCSV(r.Context(), workspaceID, id, csvData); err != nil {
		h.handleServiceError(w, err)
		return
	}

	h.writeJSONResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ExportCSV handles GET /tracking-plans/{id}/export requests.
// It exports the tracking plan schema as CSV data for download.
//
// Success response (200): CSV file download
// Error responses: 400 (missing workspace), 404 (not found), 500 (internal)
func (h *Handler) ExportCSV(w http.ResponseWriter, r *http.Request) {
	// Extract and validate tracking plan ID.
	id := chi.URLParam(r, "id")
	if err := validateTrackingPlanID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID, ok := extractWorkspaceID(r)
	if !ok {
		h.writeErrorResponse(w, http.StatusBadRequest, "missing workspace ID header")
		return
	}

	// Delegate to service layer for CSV export.
	csvData, err := h.service.ExportCSV(r.Context(), workspaceID, id)
	if err != nil {
		h.handleServiceError(w, err)
		return
	}

	// Set appropriate headers for CSV file download.
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=tracking-plan.csv")
	w.WriteHeader(http.StatusOK)

	// Write the CSV data to the response body.
	if _, err := w.Write(csvData); err != nil {
		h.log.Errorn("failed to write CSV response", obskit.Error(err))
	}
}

// ---------------------------------------------------------------------------
// Error-to-Status Mapping
// ---------------------------------------------------------------------------

// handleServiceError maps well-known service-level sentinel errors to appropriate
// HTTP status codes. Unrecognized errors are logged and returned as 500 Internal Server Error.
//
// Error-to-status mapping:
//   - ErrTrackingPlanNotFound → 404 Not Found
//   - ErrVersionNotFound      → 404 Not Found
//   - ErrInvalidSchema        → 400 Bad Request
//   - ErrInvalidCSV           → 400 Bad Request
//   - (unknown)               → 500 Internal Server Error
//
// Following the pattern from warehouse/backfill/handler.go:193-207.
func (h *Handler) handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTrackingPlanNotFound):
		h.writeErrorResponse(w, http.StatusNotFound, "tracking plan not found")
	case errors.Is(err, ErrVersionNotFound):
		h.writeErrorResponse(w, http.StatusNotFound, "tracking plan version not found")
	case errors.Is(err, ErrInvalidSchema):
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrInvalidCSV):
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
	default:
		h.log.Errorn("tracking plan service error", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "internal server error")
	}
}

// ---------------------------------------------------------------------------
// Response Helpers
// ---------------------------------------------------------------------------

// errorResponse is the standard error response DTO, consistent with
// the warehouse API error format: {"status": "error", "message": "..."}.
// Following warehouse/backfill/handler.go:209-214.
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// writeErrorResponse writes a structured JSON error response to the client.
// The response body follows the format {"status": "error", "message": "..."},
// consistent with the error response pattern in warehouse/backfill/handler.go:219-226.
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
// Following warehouse/backfill/handler.go:232-238.
func (h *Handler) writeJSONResponse(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := jsonrs.NewEncoder(w).Encode(data); err != nil {
		h.log.Errorn("failed to encode response", obskit.Error(err))
	}
}

// ---------------------------------------------------------------------------
// Workspace ID Extraction Helper
// ---------------------------------------------------------------------------

// extractWorkspaceID extracts the workspace ID from the X-Rudder-Workspace-Id header.
// Returns the workspace ID and true if present, or empty string and false if missing.
func extractWorkspaceID(r *http.Request) (string, bool) {
	workspaceID := r.Header.Get(WorkspaceIDHeader)
	return workspaceID, workspaceID != ""
}
