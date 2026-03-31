// Package api provides the HTTP handler layer for the Functions Management REST API.
//
// This file implements the HTTP request handlers for function management endpoints:
//   - POST   /             — Create a new function
//   - GET    /             — List functions for a workspace
//   - GET    /{id}         — Get a function by ID
//   - PUT    /{id}         — Update a function by ID
//   - DELETE /{id}         — Delete a function by ID
//   - POST   /{id}/test    — Test invoke a function with sample payload
//
// The handler defines dependency interfaces (FunctionRepository, FunctionRuntime,
// SecretsManager) following the dependency inversion principle established in
// processor/transformer/clients.go. JSON serialization uses jsonrs exclusively
// per the project's .golangci.yml depguard rules.
//
// Reference patterns:
//   - services/rsources/http/http.go — handler struct, errors.Is sentinel checks
//   - warehouse/healthmonitor/handler.go — writeJSONResponse/writeErrorResponse, chi.URLParam
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// ---------------------------------------------------------------------------
// Sentinel Errors
// ---------------------------------------------------------------------------

// ErrFunctionNotFound is returned when a function cannot be found by its ID.
// The handler checks for this error using errors.Is() and returns HTTP 404,
// following the pattern from services/rsources/http/http.go lines 102-107.
var ErrFunctionNotFound = errors.New("function not found")

// ---------------------------------------------------------------------------
// Valid Function Types
// ---------------------------------------------------------------------------

// validFunctionTypes defines the allowed function type values for validation.
// These correspond to Source Functions (E-015), Destination Functions (E-016),
// and Insert Functions (E-017) in the Functions framework.
var validFunctionTypes = map[string]bool{
	"source":      true,
	"destination": true,
	"insert":      true,
}

// ---------------------------------------------------------------------------
// Dependency Interfaces
// ---------------------------------------------------------------------------

// FunctionRepository defines the persistence operations for function management.
// Implementations are expected to be provided by functions/storage.Repository.
// The interface follows the dependency inversion principle used in
// processor/transformer/clients.go where interfaces are defined by the consumer.
type FunctionRepository interface {
	// Create persists a new function record. The FunctionModel must have all
	// required fields populated (ID, WorkspaceID, Name, Type, Code, Version).
	Create(ctx context.Context, fn *FunctionModel) error

	// Get retrieves a function by its unique ID. Returns ErrFunctionNotFound
	// if no function exists with the given ID.
	Get(ctx context.Context, id string) (*FunctionModel, error)

	// Update modifies an existing function record. The repository is responsible
	// for incrementing the Version field and updating the UpdatedAt timestamp.
	Update(ctx context.Context, fn *FunctionModel) error

	// Delete removes a function by its unique ID. Returns ErrFunctionNotFound
	// if no function exists with the given ID.
	Delete(ctx context.Context, id string) error

	// List returns all functions for the given workspace, subject to the
	// pagination and filtering options in ListOpts.
	List(ctx context.Context, workspaceID string, opts ListOpts) ([]*FunctionModel, error)
}

// FunctionRuntime defines the function execution interface for test invocation.
// Implementations are expected to be provided by functions/runtime.Engine.
// The Execute method accepts a FunctionModel and sample event payload, returning
// the execution result as raw JSON.
type FunctionRuntime interface {
	// Execute runs the function with the given event and settings, returning
	// the result as raw JSON. An error indicates execution failure (e.g.,
	// syntax error, runtime exception, timeout).
	Execute(ctx context.Context, fn *FunctionModel, event json.RawMessage, settings map[string]string) (json.RawMessage, error)
}

// SecretsManager defines the secrets management interface for function secrets.
// Implementations are expected to be provided by functions/secrets.SecretsManager.
// Per-function secrets are encrypted at rest following the security requirements
// in AAP Rule 0.7.7.
type SecretsManager interface {
	// GetAll retrieves all secrets for the given function ID as a key-value map.
	GetAll(ctx context.Context, functionID string) (map[string]string, error)

	// Set stores or updates a single secret key-value pair for the given function.
	Set(ctx context.Context, functionID string, key string, value string) error

	// DeleteAll removes all secrets associated with the given function ID.
	DeleteAll(ctx context.Context, functionID string) error
}

// ---------------------------------------------------------------------------
// Data Transfer Types
// ---------------------------------------------------------------------------

// FunctionModel represents a function in API requests and responses.
// This type is used for JSON serialization/deserialization at the API boundary
// and is shared with the storage layer to minimize type conversion boilerplate.
//
// Supported function types:
//   - "source"      — Source Functions with onRequest handler (E-015)
//   - "destination"  — Destination Functions with typed handlers (E-016)
//   - "insert"       — Insert Functions as pre-destination hooks (E-017)
type FunctionModel struct {
	ID          string            `json:"id"`
	WorkspaceID string            `json:"workspaceId"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Code        string            `json:"code"`
	Version     int               `json:"version"`
	Settings    map[string]string `json:"settings,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

// ListOpts configures filtering and pagination for function listing.
// Default values are applied by the listFunctions handler when query
// parameters are omitted.
type ListOpts struct {
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
	TypeFilter string `json:"typeFilter"`
}

// ---------------------------------------------------------------------------
// Request Types (unexported — internal to handler)
// ---------------------------------------------------------------------------

// createFunctionRequest is the JSON body for POST /v1/functions.
type createFunctionRequest struct {
	Name        string            `json:"name"`
	WorkspaceID string            `json:"workspaceId"`
	Type        string            `json:"type"`
	Code        string            `json:"code"`
	Settings    map[string]string `json:"settings,omitempty"`
}

// updateFunctionRequest is the JSON body for PUT /v1/functions/{id}.
type updateFunctionRequest struct {
	Name     string            `json:"name,omitempty"`
	Type     string            `json:"type,omitempty"`
	Code     string            `json:"code,omitempty"`
	Settings map[string]string `json:"settings,omitempty"`
}

// testFunctionRequest is the JSON body for POST /v1/functions/{id}/test.
type testFunctionRequest struct {
	Event json.RawMessage `json:"event"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Handler implements the Functions management REST API endpoints.
// It delegates persistence to FunctionRepository, execution to FunctionRuntime,
// and secrets management to SecretsManager.
//
// The handler follows the patterns established by:
//   - services/rsources/http/http.go (handler struct with service dependency)
//   - warehouse/healthmonitor/handler.go (HealthHandler with repository dependency)
type Handler struct {
	log     logger.Logger
	repo    FunctionRepository
	runtime FunctionRuntime
	secrets SecretsManager
}

// NewHandler creates a new Functions API handler with the given dependencies.
// The logger is wrapped with a child namespace "functions.api" for structured
// log filtering, following the warehouse package logging convention.
//
// Reference: warehouse/healthmonitor/handler.go lines 59-63 (NewHealthHandler)
// Reference: services/rsources/http/http.go lines 24-27 (handler construction)
func NewHandler(log logger.Logger, repo FunctionRepository, rt FunctionRuntime, secrets SecretsManager) *Handler {
	return &Handler{
		log:     log.Child("functions.api"),
		repo:    repo,
		runtime: rt,
		secrets: secrets,
	}
}

// ---------------------------------------------------------------------------
// Create Function Handler
// ---------------------------------------------------------------------------

// createFunction handles POST /v1/functions — creates a new function.
// Validates the request body, checks for required fields, validates function type,
// generates a UUID, persists the function, and stores associated secrets if provided.
//
// Response: HTTP 201 Created with the created function JSON body.
// Errors: HTTP 400 (invalid input), HTTP 500 (server error).
func (h *Handler) createFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createFunctionRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if err := validateCreateRequest(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	now := time.Now()
	fn := &FunctionModel{
		ID:          uuid.New().String(),
		WorkspaceID: req.WorkspaceID,
		Name:        req.Name,
		Type:        req.Type,
		Code:        req.Code,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := h.repo.Create(ctx, fn); err != nil {
		h.log.Errorn("failed to create function", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to create function")
		return
	}

	// Store settings as secrets if provided. Secrets storage is best-effort;
	// the function is already persisted, so we log errors but do not fail.
	for key, value := range req.Settings {
		if err := h.secrets.Set(ctx, fn.ID, key, value); err != nil {
			h.log.Errorn("failed to set function secret", obskit.Error(err))
		}
	}
	fn.Settings = req.Settings

	h.writeJSONResponse(w, http.StatusCreated, fn)
}

// ---------------------------------------------------------------------------
// Get Function Handler
// ---------------------------------------------------------------------------

// getFunction handles GET /v1/functions/{id} — retrieves a function by ID.
//
// Response: HTTP 200 OK with function JSON body.
// Errors: HTTP 400 (missing ID), HTTP 404 (not found), HTTP 500 (server error).
func (h *Handler) getFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "function id is required")
		return
	}

	fn, err := h.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrFunctionNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "function not found")
			return
		}
		h.log.Errorn("failed to get function", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to retrieve function")
		return
	}

	h.writeJSONResponse(w, http.StatusOK, fn)
}

// ---------------------------------------------------------------------------
// Update Function Handler
// ---------------------------------------------------------------------------

// updateFunction handles PUT /v1/functions/{id} — updates an existing function.
// Only non-empty fields from the request body are applied to the existing function.
// The version is automatically incremented by the storage layer upon update.
//
// Response: HTTP 200 OK with the updated function JSON body.
// Errors: HTTP 400 (invalid input), HTTP 404 (not found), HTTP 500 (server error).
func (h *Handler) updateFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "function id is required")
		return
	}

	var req updateFunctionRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	// Retrieve the existing function to apply partial updates.
	fn, err := h.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrFunctionNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "function not found")
			return
		}
		h.log.Errorn("failed to get function for update", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to retrieve function")
		return
	}

	// Apply non-empty fields from the request to the existing function model.
	if req.Name != "" {
		fn.Name = req.Name
	}
	if req.Type != "" {
		if !validFunctionTypes[req.Type] {
			h.writeErrorResponse(w, http.StatusBadRequest,
				fmt.Sprintf("invalid function type %q: must be one of source, destination, insert", req.Type))
			return
		}
		fn.Type = req.Type
	}
	if req.Code != "" {
		fn.Code = req.Code
	}

	// Increment version and update timestamp.
	fn.Version++
	fn.UpdatedAt = time.Now()

	if err := h.repo.Update(ctx, fn); err != nil {
		h.log.Errorn("failed to update function", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to update function")
		return
	}

	// Update secrets if provided. Uses Set for upsert behavior on each key.
	if len(req.Settings) > 0 {
		for key, value := range req.Settings {
			if err := h.secrets.Set(ctx, fn.ID, key, value); err != nil {
				h.log.Errorn("failed to update function secret", obskit.Error(err))
			}
		}
		fn.Settings = req.Settings
	}

	h.writeJSONResponse(w, http.StatusOK, fn)
}

// ---------------------------------------------------------------------------
// Delete Function Handler
// ---------------------------------------------------------------------------

// deleteFunction handles DELETE /v1/functions/{id} — deletes a function.
// Also deletes all associated secrets via the SecretsManager. Secrets cleanup
// is best-effort: errors are logged but do not prevent function deletion.
//
// Response: HTTP 204 No Content.
// Errors: HTTP 400 (missing ID), HTTP 404 (not found), HTTP 500 (server error).
func (h *Handler) deleteFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "function id is required")
		return
	}

	// Best-effort secrets cleanup before function deletion.
	if err := h.secrets.DeleteAll(ctx, id); err != nil {
		h.log.Errorn("failed to delete function secrets", obskit.Error(err))
	}

	if err := h.repo.Delete(ctx, id); err != nil {
		if errors.Is(err, ErrFunctionNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "function not found")
			return
		}
		h.log.Errorn("failed to delete function", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to delete function")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// List Functions Handler
// ---------------------------------------------------------------------------

// listFunctions handles GET /v1/functions — lists functions for a workspace.
// Query parameters:
//   - workspaceId (required): Filter by workspace ID
//   - type (optional): Filter by function type ("source", "destination", "insert")
//   - limit (optional): Maximum number of results (default: 100)
//   - offset (optional): Number of results to skip (default: 0)
//
// Response: HTTP 200 OK with JSON array of functions.
// Errors: HTTP 400 (missing workspaceId), HTTP 500 (server error).
func (h *Handler) listFunctions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	typeFilter := r.URL.Query().Get("type")

	// Parse pagination parameters with sensible defaults.
	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	offset := 0
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if parsed, err := strconv.Atoi(offsetStr); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	opts := ListOpts{
		Limit:      limit,
		Offset:     offset,
		TypeFilter: typeFilter,
	}

	functions, err := h.repo.List(ctx, workspaceID, opts)
	if err != nil {
		h.log.Errorn("failed to list functions", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to list functions")
		return
	}

	// Ensure non-nil slice so JSON serialization produces [] instead of null.
	// Follows pattern from warehouse/healthmonitor/handler.go lines 97-99.
	if functions == nil {
		functions = make([]*FunctionModel, 0)
	}

	h.writeJSONResponse(w, http.StatusOK, functions)
}

// ---------------------------------------------------------------------------
// Test Function Handler
// ---------------------------------------------------------------------------

// testFunction handles POST /v1/functions/{id}/test — test invokes a function.
// Retrieves the function, loads its secrets, and executes it with the provided
// sample event payload using the FunctionRuntime.
//
// Response: HTTP 200 OK with execution result JSON body.
// Errors: HTTP 400 (invalid payload), HTTP 404 (function not found),
//
//	HTTP 422 (execution error), HTTP 500 (server error).
func (h *Handler) testFunction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "function id is required")
		return
	}

	var req testFunctionRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if len(req.Event) == 0 {
		h.writeErrorResponse(w, http.StatusBadRequest, "event payload is required")
		return
	}

	// Retrieve the function definition.
	fn, err := h.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrFunctionNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "function not found")
			return
		}
		h.log.Errorn("failed to get function for test invocation", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to retrieve function")
		return
	}

	// Load function secrets for the test execution context.
	settings, err := h.secrets.GetAll(ctx, id)
	if err != nil {
		h.log.Errorn("failed to get function secrets for test invocation", obskit.Error(err))
		// Proceed with empty settings rather than failing the test invocation.
		settings = make(map[string]string)
	}

	// Execute the function with the sample event payload.
	result, err := h.runtime.Execute(ctx, fn, req.Event, settings)
	if err != nil {
		h.log.Errorn("function test execution failed", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusUnprocessableEntity, "function execution failed: "+err.Error())
		return
	}

	h.writeJSONResponse(w, http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// Response Helpers
// ---------------------------------------------------------------------------

// writeJSONResponse writes a JSON response with the given status code and payload.
// Uses jsonrs for serialization matching the repository convention in
// warehouse/healthmonitor/handler.go lines 148-152.
func (h *Handler) writeJSONResponse(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := jsonrs.NewEncoder(w).Encode(payload); err != nil {
		h.log.Errorn("error writing JSON response", obskit.Error(err))
	}
}

// writeErrorResponse writes a structured JSON error response.
// Follows the error response pattern from warehouse/healthmonitor/handler.go
// lines 160-167: {"status":"error","message":"<description>"}.
func (h *Handler) writeErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = jsonrs.NewEncoder(w).Encode(map[string]string{
		"status":  "error",
		"message": message,
	})
}

// ---------------------------------------------------------------------------
// Validation Helpers
// ---------------------------------------------------------------------------

// validateCreateRequest validates the create function request body.
// Returns a descriptive error for the first validation failure encountered.
func validateCreateRequest(req *createFunctionRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	if req.WorkspaceID == "" {
		return fmt.Errorf("workspaceId is required")
	}
	if req.Type == "" {
		return fmt.Errorf("type is required")
	}
	if !validFunctionTypes[req.Type] {
		return fmt.Errorf("invalid function type %q: must be one of source, destination, insert", req.Type)
	}
	if req.Code == "" {
		return fmt.Errorf("code is required")
	}
	return nil
}
