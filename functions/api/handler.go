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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	functionsruntime "github.com/rudderlabs/rudder-server/functions/runtime"
	functionssecrets "github.com/rudderlabs/rudder-server/functions/secrets"
	functionsstorage "github.com/rudderlabs/rudder-server/functions/storage"
)

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
// Method signatures match functions/storage.Repository exactly so that the
// storage implementation satisfies this interface without adapter layers.
// All mutating and read operations are scoped by workspace_id for
// defense-in-depth multi-tenant isolation.
type FunctionRepository interface {
	// Create persists a new function record. The Function must have all
	// required fields populated (ID, WorkspaceID, Name, Type, Code, Version).
	Create(ctx context.Context, fn *functionsstorage.Function) error

	// Get retrieves a function by its unique ID scoped to a workspace.
	// Returns functionsstorage.ErrFunctionNotFound if no row matches.
	Get(ctx context.Context, id string, workspaceID string) (*functionsstorage.Function, error)

	// Update modifies an existing function record. The repository is responsible
	// for incrementing the Version field and updating the UpdatedAt timestamp.
	// Workspace scoping is derived from fn.WorkspaceID.
	Update(ctx context.Context, fn *functionsstorage.Function) error

	// Delete removes a function by its unique ID scoped to a workspace.
	// Returns functionsstorage.ErrFunctionNotFound if no row matches.
	Delete(ctx context.Context, id string, workspaceID string) error

	// List returns all functions for the given workspace, subject to the
	// pagination and filtering options in ListOptions.
	List(ctx context.Context, workspaceID string, opts functionsstorage.ListOptions) ([]*functionsstorage.Function, error)
}

// FunctionRuntime defines the function execution interface for test invocation.
// Method signature matches functions/runtime.Engine.Execute exactly so that the
// Engine satisfies this interface without adapter layers.
type FunctionRuntime interface {
	// Execute runs the function with the given event and settings, returning
	// an ExecutionResult. An error indicates execution failure (e.g.,
	// syntax error, runtime exception, timeout).
	Execute(ctx context.Context, fn *functionsruntime.FunctionDef, event json.RawMessage, settings map[string]string) (*functionsruntime.ExecutionResult, error)
}

// SecretsManager defines the secrets management interface for function secrets.
// Implementations are expected to be provided by functions/secrets.SecretsManager.
// Per-function secrets are encrypted at rest following the security requirements
// in AAP Rule 0.7.7.
type SecretsManager interface {
	// Get retrieves a single decrypted secret value for the given function and key.
	Get(ctx context.Context, functionID string, key string) (string, error)

	// GetAll retrieves all secrets for the given function ID as a key-value map.
	GetAll(ctx context.Context, functionID string) (map[string]string, error)

	// Set stores or updates a single secret key-value pair for the given function.
	Set(ctx context.Context, functionID string, key string, value string) error

	// Delete removes a single secret by function ID and key.
	Delete(ctx context.Context, functionID string, key string) error

	// DeleteAll removes all secrets associated with the given function ID.
	DeleteAll(ctx context.Context, functionID string) error
}

// ---------------------------------------------------------------------------
// Data Transfer Types
// ---------------------------------------------------------------------------

// FunctionModel is the API-layer Data Transfer Object for function responses.
// It exists separately from functionsstorage.Function to provide a clean
// API boundary: Settings are represented as map[string]string (loaded from
// the encrypted secrets manager) rather than json.RawMessage (DB JSONB column).
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

// storageToModel converts a storage-layer Function to the API response DTO.
// The Settings field is NOT copied because API-layer settings come from the
// encrypted SecretsManager, not from the storage JSONB column.
func storageToModel(fn *functionsstorage.Function) *FunctionModel {
	return &FunctionModel{
		ID:          fn.ID,
		WorkspaceID: fn.WorkspaceID,
		Name:        fn.Name,
		Type:        fn.Type,
		Code:        fn.Code,
		Version:     fn.Version,
		CreatedAt:   fn.CreatedAt,
		UpdatedAt:   fn.UpdatedAt,
	}
}

// storageToFunctionDef converts a storage-layer Function to a runtime
// FunctionDef for execution via FunctionRuntime.Execute.
func storageToFunctionDef(fn *functionsstorage.Function) *functionsruntime.FunctionDef {
	return &functionsruntime.FunctionDef{
		ID:          fn.ID,
		WorkspaceID: fn.WorkspaceID,
		Name:        fn.Name,
		Type:        fn.Type,
		Code:        fn.Code,
		Version:     fn.Version,
	}
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

	// Apply body size limit to prevent memory exhaustion from oversized payloads.
	r.Body = http.MaxBytesReader(w, r.Body, maxManagementBodySize)

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
	storageFn := &functionsstorage.Function{
		// ID is omitted — the database generates a BIGSERIAL primary key.
		// The storage layer sets fn.ID after INSERT via RETURNING id.
		WorkspaceID: req.WorkspaceID,
		Name:        req.Name,
		Type:        req.Type,
		Code:        req.Code,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := h.repo.Create(ctx, storageFn); err != nil {
		h.log.Errorn("failed to create function", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to create function")
		return
	}

	// Store settings as secrets if provided. Secrets storage is best-effort;
	// the function is already persisted, so we log errors but do not fail.
	for key, value := range req.Settings {
		if err := h.secrets.Set(ctx, storageFn.ID, key, value); err != nil {
			h.log.Errorn("failed to set function secret", obskit.Error(err))
		}
	}

	// Convert storage entity to API response DTO, enriching with secrets.
	resp := storageToModel(storageFn)
	resp.Settings = req.Settings

	h.writeJSONResponse(w, http.StatusCreated, resp)
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
	if err := validateNumericID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	fn, err := h.repo.Get(ctx, id, workspaceID)
	if err != nil {
		if errors.Is(err, functionsstorage.ErrFunctionNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "function not found")
			return
		}
		h.log.Errorn("failed to get function", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to retrieve function")
		return
	}

	h.writeJSONResponse(w, http.StatusOK, storageToModel(fn))
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
	if err := validateNumericID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	// Apply body size limit to prevent memory exhaustion from oversized payloads.
	r.Body = http.MaxBytesReader(w, r.Body, maxManagementBodySize)

	var req updateFunctionRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	// Retrieve the existing function to apply partial updates.
	fn, err := h.repo.Get(ctx, id, workspaceID)
	if err != nil {
		if errors.Is(err, functionsstorage.ErrFunctionNotFound) {
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

	// Version increment and UpdatedAt are managed atomically by the storage
	// layer's UPDATE ... SET version=version+1, updated_at=NOW() RETURNING
	// clause — no handler-side mutation needed to avoid double-increment.
	if err := h.repo.Update(ctx, fn); err != nil {
		h.log.Errorn("failed to update function", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to update function")
		return
	}

	// Convert to response DTO. The storage layer already reflected the new
	// version and updated_at back onto fn via the RETURNING clause.
	resp := storageToModel(fn)

	// Update secrets if provided. Uses Set for upsert behavior on each key.
	if len(req.Settings) > 0 {
		for key, value := range req.Settings {
			if err := h.secrets.Set(ctx, fn.ID, key, value); err != nil {
				h.log.Errorn("failed to update function secret", obskit.Error(err))
			}
		}
		resp.Settings = req.Settings
	}

	h.writeJSONResponse(w, http.StatusOK, resp)
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
	if err := validateNumericID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	// Delete the function entity first (primary operation). If this fails,
	// secrets remain intact and no data is lost — a retry will succeed
	// cleanly. Reversing this order (secrets-first) would risk orphaning
	// the function record if secret deletion succeeds but function deletion
	// fails.
	if err := h.repo.Delete(ctx, id, workspaceID); err != nil {
		if errors.Is(err, functionsstorage.ErrFunctionNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "function not found")
			return
		}
		h.log.Errorn("failed to delete function", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to delete function")
		return
	}

	// Best-effort orphaned secrets cleanup after function deletion.
	if err := h.secrets.DeleteAll(ctx, id); err != nil {
		h.log.Errorn("failed to delete function secrets", obskit.Error(err))
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

	opts := functionsstorage.ListOptions{
		Limit:      limit,
		Offset:     offset,
		TypeFilter: typeFilter,
	}

	storageFunctions, err := h.repo.List(ctx, workspaceID, opts)
	if err != nil {
		h.log.Errorn("failed to list functions", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to list functions")
		return
	}

	// Convert storage entities to API response DTOs.
	// Ensure non-nil slice so JSON serialization produces [] instead of null.
	// Follows pattern from warehouse/healthmonitor/handler.go lines 97-99.
	models := make([]*FunctionModel, 0, len(storageFunctions))
	for _, fn := range storageFunctions {
		models = append(models, storageToModel(fn))
	}

	h.writeJSONResponse(w, http.StatusOK, models)
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
	if err := validateNumericID(id); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Apply body size limit to prevent memory exhaustion from oversized test payloads.
	r.Body = http.MaxBytesReader(w, r.Body, maxManagementBodySize)

	var req testFunctionRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if len(req.Event) == 0 {
		h.writeErrorResponse(w, http.StatusBadRequest, "event payload is required")
		return
	}

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	// Retrieve the function definition from storage.
	fn, err := h.repo.Get(ctx, id, workspaceID)
	if err != nil {
		if errors.Is(err, functionsstorage.ErrFunctionNotFound) {
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

	// Convert the storage entity to a runtime FunctionDef for execution.
	fnDef := storageToFunctionDef(fn)

	// Execute the function with the sample event payload.
	result, err := h.runtime.Execute(ctx, fnDef, req.Event, settings)
	if err != nil {
		h.log.Errorn("function test execution failed", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusUnprocessableEntity, "function execution failed: "+err.Error())
		return
	}

	h.writeJSONResponse(w, http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// Secrets Handlers (E-019)
// ---------------------------------------------------------------------------

// setSecretRequest is the JSON body for PUT /v1/functions/{id}/secrets.
type setSecretRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// verifyFunctionWorkspaceOwnership checks that the function identified by functionID
// belongs to the specified workspace. This is a critical security check for secrets
// endpoints to prevent cross-workspace data access. The function record is retrieved
// from the repository and its workspace_id is compared against the provided workspace.
//
// Returns the function if ownership is verified, or writes an error response and returns nil.
func (h *Handler) verifyFunctionWorkspaceOwnership(w http.ResponseWriter, r *http.Request, functionID, workspaceID string) *functionsstorage.Function {
	fn, err := h.repo.Get(r.Context(), functionID, workspaceID)
	if err != nil {
		if errors.Is(err, functionsstorage.ErrFunctionNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "function not found or not accessible in this workspace")
			return nil
		}
		h.log.Errorn("failed to verify function ownership", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to verify function access")
		return nil
	}
	return fn
}

// maskSecretValue returns a masked representation of a secret value for safe display
// in API responses. Only the last 4 characters are shown (if the value is long enough),
// preceded by asterisks. This prevents exposure of full secret values while allowing
// operators to identify which secret is configured.
func maskSecretValue(value string) string {
	if len(value) <= 4 {
		return "****"
	}
	return "****" + value[len(value)-4:]
}

// setSecret handles PUT /v1/functions/{id}/secrets — create or update a secret.
// Validates the request body and verifies workspace ownership of the function
// before delegating to SecretsManager.Set.
//
// Response: HTTP 204 No Content on success.
// Errors: HTTP 400 (invalid input), HTTP 404 (function not found), HTTP 500 (server error).
func (h *Handler) setSecret(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	functionID := chi.URLParam(r, "id")
	if err := validateNumericID(functionID); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	// Verify the function belongs to the requesting workspace before allowing
	// secrets operations. This prevents cross-workspace secret manipulation.
	if fn := h.verifyFunctionWorkspaceOwnership(w, r, functionID, workspaceID); fn == nil {
		return
	}

	// Apply body size limit to prevent memory exhaustion from oversized payloads.
	r.Body = http.MaxBytesReader(w, r.Body, maxManagementBodySize)

	var req setSecretRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Key == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "key is required")
		return
	}
	if req.Value == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "value is required")
		return
	}

	if err := h.secrets.Set(ctx, functionID, req.Key, req.Value); err != nil {
		h.log.Errorn("failed to set secret", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to set secret")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getSecret handles GET /v1/functions/{id}/secrets/{key} — retrieve a single secret.
// Returns masked secret values (e.g., "****5678") to prevent plaintext exposure.
// Verifies workspace ownership before allowing access.
//
// Response: HTTP 200 with JSON {"key": "...", "value": "****XXXX"}.
// Errors: HTTP 400 (invalid input), HTTP 404 (secret or function not found), HTTP 500 (server error).
func (h *Handler) getSecret(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	functionID := chi.URLParam(r, "id")
	if err := validateNumericID(functionID); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	key := chi.URLParam(r, "key")

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	// Verify workspace ownership before allowing secrets access.
	if fn := h.verifyFunctionWorkspaceOwnership(w, r, functionID, workspaceID); fn == nil {
		return
	}

	value, err := h.secrets.Get(ctx, functionID, key)
	if err != nil {
		if errors.Is(err, functionssecrets.ErrSecretNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "secret not found")
			return
		}
		h.log.Errorn("failed to get secret", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to get secret")
		return
	}
	// Return masked value — plaintext secrets are only available internally
	// to the Functions runtime during execution, never via external API.
	h.writeJSONResponse(w, http.StatusOK, map[string]string{"key": key, "value": maskSecretValue(value)})
}

// getAllSecrets handles GET /v1/functions/{id}/secrets — list all secrets for a function.
// Returns only secret key names with masked values to prevent plaintext exposure.
// Verifies workspace ownership before allowing access.
//
// Response: HTTP 200 with JSON object mapping key names to masked values.
// Errors: HTTP 400 (invalid input), HTTP 404 (function not found), HTTP 500 (server error).
func (h *Handler) getAllSecrets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	functionID := chi.URLParam(r, "id")
	if err := validateNumericID(functionID); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	// Verify workspace ownership before allowing secrets access.
	if fn := h.verifyFunctionWorkspaceOwnership(w, r, functionID, workspaceID); fn == nil {
		return
	}

	secrets, err := h.secrets.GetAll(ctx, functionID)
	if err != nil {
		h.log.Errorn("failed to list secrets", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to list secrets")
		return
	}
	if secrets == nil {
		secrets = make(map[string]string)
	}
	// Mask all secret values — plaintext secrets are only available internally
	// to the Functions runtime during execution, never via external API.
	maskedSecrets := make(map[string]string, len(secrets))
	for k, v := range secrets {
		maskedSecrets[k] = maskSecretValue(v)
	}
	h.writeJSONResponse(w, http.StatusOK, maskedSecrets)
}

// deleteSecret handles DELETE /v1/functions/{id}/secrets/{key} — delete a single secret.
// Verifies workspace ownership before allowing deletion.
//
// Response: HTTP 204 No Content.
// Errors: HTTP 400 (invalid input), HTTP 404 (function or secret not found), HTTP 500 (server error).
func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	functionID := chi.URLParam(r, "id")
	if err := validateNumericID(functionID); err != nil {
		h.writeErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	key := chi.URLParam(r, "key")

	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "workspaceId query parameter is required")
		return
	}

	// Verify workspace ownership before allowing secret deletion.
	if fn := h.verifyFunctionWorkspaceOwnership(w, r, functionID, workspaceID); fn == nil {
		return
	}

	if err := h.secrets.Delete(ctx, functionID, key); err != nil {
		if errors.Is(err, functionssecrets.ErrSecretNotFound) {
			h.writeErrorResponse(w, http.StatusNotFound, "secret not found")
			return
		}
		h.log.Errorn("failed to delete secret", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to delete secret")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// maxManagementBodySize is the maximum allowed request body size for management API
// endpoints (Functions CRUD). This prevents memory exhaustion from oversized payloads.
// Functions code bodies are limited to 1MB, which is generous for transformation code.
const maxManagementBodySize = 1 * 1024 * 1024 // 1 MB

// validateCreateRequest validates the create function request body.
// Returns a descriptive error for the first validation failure encountered.
// Includes null byte detection to prevent PostgreSQL text column errors.
func validateCreateRequest(req *createFunctionRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	if containsNullByte(req.Name) {
		return fmt.Errorf("name contains invalid characters (null bytes)")
	}
	if req.WorkspaceID == "" {
		return fmt.Errorf("workspaceId is required")
	}
	if containsNullByte(req.WorkspaceID) {
		return fmt.Errorf("workspaceId contains invalid characters (null bytes)")
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
	if containsNullByte(req.Code) {
		return fmt.Errorf("code contains invalid characters (null bytes)")
	}
	return nil
}

// validateNumericID checks that the given ID string is a valid positive numeric
// identifier. Returns a descriptive error if the ID is empty, non-numeric, or
// not a valid positive integer. This prevents SQL injection via path parameters
// and ensures PostgreSQL BIGINT columns receive valid input.
func validateNumericID(id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	parsed, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id format: must be a numeric value")
	}
	if parsed <= 0 {
		return fmt.Errorf("invalid id: must be a positive integer")
	}
	return nil
}

// containsNullByte checks if a string contains null bytes (\x00) which are
// rejected by PostgreSQL text columns. Detecting null bytes in the validation
// layer prevents unhandled 500 errors from the database driver.
func containsNullByte(s string) bool {
	return strings.Contains(s, "\x00")
}
