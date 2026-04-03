// Package api_test provides comprehensive unit tests for the Functions management
// REST API handler (Sprint 4-6, Epic E-018). Tests cover all CRUD operations,
// test invocation endpoint, error handling, and HTTP contract compliance.
//
// Testing patterns follow services/rsources/http/http_test.go — httptest-based
// request/response testing with testify/require assertions per AAP Rule 0.7.4.
package api_test

import (
	"context"
	"encoding/json" // for json.RawMessage type only
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/functions/api"
	functionsruntime "github.com/rudderlabs/rudder-server/functions/runtime"
	functionsstorage "github.com/rudderlabs/rudder-server/functions/storage"
)

// ---------------------------------------------------------------------------
// Response Helper Types
// ---------------------------------------------------------------------------

// errorResponse mirrors the JSON error structure returned by Handler.writeErrorResponse:
//
//	{"status":"error","message":"<description>"}
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Mock Implementations
// ---------------------------------------------------------------------------

// mockFunctionRepository implements api.FunctionRepository with configurable
// function-pointer fields. Each method delegates to its corresponding field;
// unconfigured fields return a descriptive error to surface unexpected calls.
type mockFunctionRepository struct {
	createFn func(ctx context.Context, fn *functionsstorage.Function) error
	getFn    func(ctx context.Context, id string, workspaceID string) (*functionsstorage.Function, error)
	updateFn func(ctx context.Context, fn *functionsstorage.Function) error
	deleteFn func(ctx context.Context, id string, workspaceID string) error
	listFn   func(ctx context.Context, workspaceID string, opts functionsstorage.ListOptions) ([]*functionsstorage.Function, error)
}

func (m *mockFunctionRepository) Create(ctx context.Context, fn *functionsstorage.Function) error {
	if m.createFn != nil {
		return m.createFn(ctx, fn)
	}
	return nil
}

func (m *mockFunctionRepository) Get(ctx context.Context, id string, workspaceID string) (*functionsstorage.Function, error) {
	if m.getFn != nil {
		return m.getFn(ctx, id, workspaceID)
	}
	return nil, errors.New("mockFunctionRepository.Get not configured")
}

func (m *mockFunctionRepository) Update(ctx context.Context, fn *functionsstorage.Function) error {
	if m.updateFn != nil {
		return m.updateFn(ctx, fn)
	}
	return nil
}

func (m *mockFunctionRepository) Delete(ctx context.Context, id string, workspaceID string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id, workspaceID)
	}
	return nil
}

func (m *mockFunctionRepository) List(ctx context.Context, workspaceID string, opts functionsstorage.ListOptions) ([]*functionsstorage.Function, error) {
	if m.listFn != nil {
		return m.listFn(ctx, workspaceID, opts)
	}
	return []*functionsstorage.Function{}, nil
}

// mockFunctionRuntime implements api.FunctionRuntime with a configurable
// executeFn field. Unconfigured calls return an empty ExecutionResult.
type mockFunctionRuntime struct {
	executeFn func(ctx context.Context, fn *functionsruntime.FunctionDef, event json.RawMessage, settings map[string]string) (*functionsruntime.ExecutionResult, error)
}

func (m *mockFunctionRuntime) Execute(ctx context.Context, fn *functionsruntime.FunctionDef, event json.RawMessage, settings map[string]string) (*functionsruntime.ExecutionResult, error) {
	if m.executeFn != nil {
		return m.executeFn(ctx, fn, event, settings)
	}
	return &functionsruntime.ExecutionResult{}, nil
}

// mockSecretsManager implements api.SecretsManager with configurable function-
// pointer fields. Unconfigured calls return safe defaults (empty map / nil error).
type mockSecretsManager struct {
	getAllFn    func(ctx context.Context, functionID string) (map[string]string, error)
	setFn       func(ctx context.Context, functionID string, key string, value string) error
	getFn       func(ctx context.Context, functionID string, key string) (string, error)
	deleteFn    func(ctx context.Context, functionID string, key string) error
	deleteAllFn func(ctx context.Context, functionID string) error
}

func (m *mockSecretsManager) GetAll(ctx context.Context, functionID string) (map[string]string, error) {
	if m.getAllFn != nil {
		return m.getAllFn(ctx, functionID)
	}
	return make(map[string]string), nil
}

func (m *mockSecretsManager) Set(ctx context.Context, functionID string, key string, value string) error {
	if m.setFn != nil {
		return m.setFn(ctx, functionID, key, value)
	}
	return nil
}

func (m *mockSecretsManager) Get(ctx context.Context, functionID string, key string) (string, error) {
	if m.getFn != nil {
		return m.getFn(ctx, functionID, key)
	}
	return "", nil
}

func (m *mockSecretsManager) Delete(ctx context.Context, functionID string, key string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, functionID, key)
	}
	return nil
}

func (m *mockSecretsManager) DeleteAll(ctx context.Context, functionID string) error {
	if m.deleteAllFn != nil {
		return m.deleteAllFn(ctx, functionID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// newTestHandler creates a complete HTTP handler for the Functions API with
// mock dependencies. The handler is mounted at /v1/functions to match the
// Gateway mount point, enabling test URLs like /v1/functions/123.
func newTestHandler(t *testing.T, repo api.FunctionRepository, rt api.FunctionRuntime, secrets api.SecretsManager) http.Handler {
	t.Helper()
	h := api.NewHandler(logger.NOP, repo, rt, secrets)
	mux := chi.NewRouter()
	mux.Mount("/v1/functions", api.Routes(h))
	return mux
}

// doRequest sends an HTTP request to the given handler and returns the
// response recorder. The body parameter is sent as application/json when
// non-empty. Pattern follows services/rsources/http/http_test.go.
func doRequest(t *testing.T, handler http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Create Function Tests
// ---------------------------------------------------------------------------

// TestHandler_CreateFunction verifies successful function creation via
// POST /v1/functions with a valid JSON body.
func TestHandler_CreateFunction(t *testing.T) {
	repo := &mockFunctionRepository{
		createFn: func(_ context.Context, fn *functionsstorage.Function) error {
			// Simulate DB-generated BIGSERIAL primary key (repository.Create sets fn.ID)
			fn.ID = "42"
			return nil
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	body := `{"name":"My Source Function","type":"source","code":"function onRequest(request, settings) { return []; }","workspaceId":"ws-123"}`
	rec := doRequest(t, handler, http.MethodPost, "/v1/functions", body)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var result api.FunctionModel
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&result))
	require.NotEmpty(t, result.ID, "ID should be set by repository (DB-generated BIGSERIAL)")
	require.Equal(t, "42", result.ID)
	require.Equal(t, "ws-123", result.WorkspaceID)
	require.Equal(t, "My Source Function", result.Name)
	require.Equal(t, "source", result.Type)
	require.Equal(t, "function onRequest(request, settings) { return []; }", result.Code)
	require.Equal(t, 1, result.Version)
	require.False(t, result.CreatedAt.IsZero(), "CreatedAt must not be zero")
	require.False(t, result.UpdatedAt.IsZero(), "UpdatedAt must not be zero")
}

// TestHandler_CreateFunction_MissingFields verifies that POST /v1/functions
// with an empty JSON body returns HTTP 400 Bad Request.
func TestHandler_CreateFunction_MissingFields(t *testing.T) {
	handler := newTestHandler(t, &mockFunctionRepository{}, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodPost, "/v1/functions", `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "name is required")
}

// TestHandler_CreateFunction_InvalidType verifies that POST /v1/functions
// with an invalid function type returns HTTP 400 Bad Request.
func TestHandler_CreateFunction_InvalidType(t *testing.T) {
	handler := newTestHandler(t, &mockFunctionRepository{}, &mockFunctionRuntime{}, &mockSecretsManager{})

	body := `{"name":"test","type":"invalid","code":"test code","workspaceId":"ws-123"}`
	rec := doRequest(t, handler, http.MethodPost, "/v1/functions", body)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "invalid function type")
}

// TestHandler_CreateFunction_RepoError verifies that a repository error
// during function creation returns HTTP 500 Internal Server Error.
func TestHandler_CreateFunction_RepoError(t *testing.T) {
	repo := &mockFunctionRepository{
		createFn: func(_ context.Context, _ *functionsstorage.Function) error {
			return fmt.Errorf("database connection refused")
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	body := `{"name":"test","type":"source","code":"test code","workspaceId":"ws-123"}`
	rec := doRequest(t, handler, http.MethodPost, "/v1/functions", body)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "failed to create function")
}

// ---------------------------------------------------------------------------
// Get Function Tests
// ---------------------------------------------------------------------------

// TestHandler_GetFunction verifies successful function retrieval via
// GET /v1/functions/{id} with a valid function ID and workspaceId.
func TestHandler_GetFunction(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, id string, workspaceID string) (*functionsstorage.Function, error) {
			require.Equal(t, "123", id)
			require.Equal(t, "ws-123", workspaceID)
			return &functionsstorage.Function{
				ID:          "123",
				WorkspaceID: "ws-123",
				Name:        "My Function",
				Type:        "source",
				Code:        "function onRequest() {}",
				Version:     1,
				CreatedAt:   now,
				UpdatedAt:   now,
			}, nil
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodGet, "/v1/functions/123?workspaceId=ws-123", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var result api.FunctionModel
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&result))
	require.Equal(t, "123", result.ID)
	require.Equal(t, "ws-123", result.WorkspaceID)
	require.Equal(t, "My Function", result.Name)
	require.Equal(t, "source", result.Type)
	require.Equal(t, "function onRequest() {}", result.Code)
	require.Equal(t, 1, result.Version)
}

// TestHandler_GetFunction_NotFound verifies that GET /v1/functions/{id}
// returns HTTP 404 when the function does not exist.
func TestHandler_GetFunction_NotFound(t *testing.T) {
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, _ string, _ string) (*functionsstorage.Function, error) {
			return nil, functionsstorage.ErrFunctionNotFound
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodGet, "/v1/functions/999999?workspaceId=ws-123", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "function not found")
}

// TestHandler_GetFunction_RepoError verifies that a repository error
// during function retrieval returns HTTP 500 Internal Server Error.
func TestHandler_GetFunction_RepoError(t *testing.T) {
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, _ string, _ string) (*functionsstorage.Function, error) {
			return nil, fmt.Errorf("database timeout")
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodGet, "/v1/functions/123?workspaceId=ws-123", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "failed to retrieve function")
}

// ---------------------------------------------------------------------------
// Update Function Tests
// ---------------------------------------------------------------------------

// TestHandler_UpdateFunction verifies successful function update via
// PUT /v1/functions/{id}. The storage layer is expected to increment
// the version and update the timestamp atomically.
func TestHandler_UpdateFunction(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, _ string, _ string) (*functionsstorage.Function, error) {
			return &functionsstorage.Function{
				ID:          "123",
				WorkspaceID: "ws-123",
				Name:        "Original Name",
				Type:        "source",
				Code:        "old code",
				Version:     1,
				CreatedAt:   now.Add(-time.Hour),
				UpdatedAt:   now.Add(-time.Hour),
			}, nil
		},
		updateFn: func(_ context.Context, fn *functionsstorage.Function) error {
			// Simulate the storage layer's atomic version increment via
			// UPDATE ... SET version=version+1 ... RETURNING version, updated_at
			fn.Version = fn.Version + 1
			fn.UpdatedAt = time.Now().UTC()
			return nil
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	body := `{"name":"Updated Name","code":"new code"}`
	rec := doRequest(t, handler, http.MethodPut, "/v1/functions/123?workspaceId=ws-123", body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var result api.FunctionModel
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&result))
	require.Equal(t, "123", result.ID)
	require.Equal(t, "Updated Name", result.Name)
	require.Equal(t, "new code", result.Code)
	require.Equal(t, 2, result.Version, "version should be incremented")
}

// TestHandler_UpdateFunction_NotFound verifies that PUT /v1/functions/{id}
// returns HTTP 404 when the function does not exist. The handler issues a
// Get before Update — if Get returns ErrFunctionNotFound, the handler
// short-circuits to 404 without calling Update.
func TestHandler_UpdateFunction_NotFound(t *testing.T) {
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, _ string, _ string) (*functionsstorage.Function, error) {
			return nil, functionsstorage.ErrFunctionNotFound
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	body := `{"name":"Updated Name"}`
	rec := doRequest(t, handler, http.MethodPut, "/v1/functions/999999?workspaceId=ws-123", body)
	require.Equal(t, http.StatusNotFound, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "function not found")
}

// TestHandler_UpdateFunction_InvalidBody verifies that PUT /v1/functions/{id}
// with malformed JSON returns HTTP 400 Bad Request.
func TestHandler_UpdateFunction_InvalidBody(t *testing.T) {
	handler := newTestHandler(t, &mockFunctionRepository{}, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodPut, "/v1/functions/123?workspaceId=ws-123", `{invalid json`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "invalid request body")
}

// ---------------------------------------------------------------------------
// Delete Function Tests
// ---------------------------------------------------------------------------

// TestHandler_DeleteFunction verifies successful function deletion via
// DELETE /v1/functions/{id}. The handler first deletes the function
// from the repository, then makes a best-effort attempt to clean up
// associated secrets.
func TestHandler_DeleteFunction(t *testing.T) {
	deleteCalled := false
	secretsDeleteCalled := false
	repo := &mockFunctionRepository{
		deleteFn: func(_ context.Context, id string, workspaceID string) error {
			require.Equal(t, "123", id)
			require.Equal(t, "ws-123", workspaceID)
			deleteCalled = true
			return nil
		},
	}
	secrets := &mockSecretsManager{
		deleteAllFn: func(_ context.Context, functionID string) error {
			require.Equal(t, "123", functionID)
			secretsDeleteCalled = true
			return nil
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, secrets)

	rec := doRequest(t, handler, http.MethodDelete, "/v1/functions/123?workspaceId=ws-123", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.True(t, deleteCalled, "repository delete should be called")
	require.True(t, secretsDeleteCalled, "secrets deleteAll should be called")
}

// TestHandler_DeleteFunction_NotFound verifies that DELETE /v1/functions/{id}
// returns HTTP 404 when the function does not exist.
func TestHandler_DeleteFunction_NotFound(t *testing.T) {
	repo := &mockFunctionRepository{
		deleteFn: func(_ context.Context, _ string, _ string) error {
			return functionsstorage.ErrFunctionNotFound
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodDelete, "/v1/functions/999999?workspaceId=ws-123", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "function not found")
}

// ---------------------------------------------------------------------------
// List Functions Tests
// ---------------------------------------------------------------------------

// TestHandler_ListFunctions verifies successful function listing via
// GET /v1/functions?workspaceId=ws-123.
func TestHandler_ListFunctions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := &mockFunctionRepository{
		listFn: func(_ context.Context, workspaceID string, opts functionsstorage.ListOptions) ([]*functionsstorage.Function, error) {
			require.Equal(t, "ws-123", workspaceID)
			return []*functionsstorage.Function{
				{
					ID:          "func-1",
					WorkspaceID: "ws-123",
					Name:        "Function A",
					Type:        "source",
					Code:        "a code",
					Version:     1,
					CreatedAt:   now,
					UpdatedAt:   now,
				},
				{
					ID:          "func-2",
					WorkspaceID: "ws-123",
					Name:        "Function B",
					Type:        "destination",
					Code:        "b code",
					Version:     2,
					CreatedAt:   now,
					UpdatedAt:   now,
				},
				{
					ID:          "func-3",
					WorkspaceID: "ws-123",
					Name:        "Function C",
					Type:        "insert",
					Code:        "c code",
					Version:     1,
					CreatedAt:   now,
					UpdatedAt:   now,
				},
			}, nil
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodGet, "/v1/functions?workspaceId=ws-123", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var results []api.FunctionModel
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&results))
	require.Len(t, results, 3)
	require.Equal(t, "func-1", results[0].ID)
	require.Equal(t, "func-2", results[1].ID)
	require.Equal(t, "func-3", results[2].ID)
}

// TestHandler_ListFunctions_Empty verifies that an empty workspace returns
// an empty JSON array rather than null.
func TestHandler_ListFunctions_Empty(t *testing.T) {
	repo := &mockFunctionRepository{
		listFn: func(_ context.Context, _ string, _ functionsstorage.ListOptions) ([]*functionsstorage.Function, error) {
			return []*functionsstorage.Function{}, nil
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodGet, "/v1/functions?workspaceId=ws-999", "")
	require.Equal(t, http.StatusOK, rec.Code)

	body := strings.TrimSpace(rec.Body.String())
	require.Equal(t, "[]", body, "empty list should return [] not null")
}

// TestHandler_ListFunctions_WithTypeFilter verifies that the type query
// parameter is forwarded to the repository as a filter.
func TestHandler_ListFunctions_WithTypeFilter(t *testing.T) {
	repo := &mockFunctionRepository{
		listFn: func(_ context.Context, workspaceID string, opts functionsstorage.ListOptions) ([]*functionsstorage.Function, error) {
			require.Equal(t, "ws-123", workspaceID)
			require.Equal(t, "source", opts.TypeFilter)
			return []*functionsstorage.Function{}, nil
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodGet, "/v1/functions?workspaceId=ws-123&type=source", "")
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestHandler_ListFunctions_MissingWorkspaceID verifies that a missing
// workspaceId query parameter returns HTTP 400.
func TestHandler_ListFunctions_MissingWorkspaceID(t *testing.T) {
	handler := newTestHandler(t, &mockFunctionRepository{}, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodGet, "/v1/functions", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "workspaceId")
}

// ---------------------------------------------------------------------------
// Test Invocation Tests
// ---------------------------------------------------------------------------

// TestHandler_TestFunction verifies successful function test invocation via
// POST /v1/functions/{id}/test with a valid event payload.
func TestHandler_TestFunction(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, id string, _ string) (*functionsstorage.Function, error) {
			return &functionsstorage.Function{
				ID:          id,
				WorkspaceID: "ws-123",
				Name:        "Test Func",
				Type:        "source",
				Code:        `function onRequest(request, settings) { return []; }`,
				Version:     1,
				CreatedAt:   now,
				UpdatedAt:   now,
			}, nil
		},
	}
	secrets := &mockSecretsManager{
		getAllFn: func(_ context.Context, functionID string) (map[string]string, error) {
			require.Equal(t, "123", functionID)
			return map[string]string{"API_KEY": "test-key"}, nil
		},
	}
	rt := &mockFunctionRuntime{
		executeFn: func(_ context.Context, fn *functionsruntime.FunctionDef, event json.RawMessage, settings map[string]string) (*functionsruntime.ExecutionResult, error) {
			require.Equal(t, "123", fn.ID)
			require.Equal(t, "test-key", settings["API_KEY"])
			return &functionsruntime.ExecutionResult{
				Events: []json.RawMessage{
					json.RawMessage(`{"type":"track","event":"test"}`),
				},
				Logs: []string{"execution complete"},
			}, nil
		},
	}
	handler := newTestHandler(t, repo, rt, secrets)

	body := `{"event":{"type":"track","event":"Purchase","userId":"u1","properties":{"price":42}}}`
	rec := doRequest(t, handler, http.MethodPost, "/v1/functions/123/test?workspaceId=ws-123", body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	// Validate that the response contains result data.
	require.NotEmpty(t, rec.Body.Bytes())
}

// TestHandler_TestFunction_NotFound verifies that POST /v1/functions/{id}/test
// returns HTTP 404 when the function does not exist.
func TestHandler_TestFunction_NotFound(t *testing.T) {
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, _ string, _ string) (*functionsstorage.Function, error) {
			return nil, functionsstorage.ErrFunctionNotFound
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	body := `{"event":{"type":"track"}}`
	rec := doRequest(t, handler, http.MethodPost, "/v1/functions/999999/test?workspaceId=ws-123", body)
	require.Equal(t, http.StatusNotFound, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "function not found")
}

// TestHandler_TestFunction_RuntimeError verifies that an execution failure
// from the Functions runtime returns HTTP 422 Unprocessable Entity.
func TestHandler_TestFunction_RuntimeError(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, id string, _ string) (*functionsstorage.Function, error) {
			return &functionsstorage.Function{
				ID:          id,
				WorkspaceID: "ws-123",
				Name:        "Test Func",
				Type:        "source",
				Code:        "bad code",
				Version:     1,
				CreatedAt:   now,
				UpdatedAt:   now,
			}, nil
		},
	}
	secrets := &mockSecretsManager{
		getAllFn: func(_ context.Context, _ string) (map[string]string, error) {
			return make(map[string]string), nil
		},
	}
	rt := &mockFunctionRuntime{
		executeFn: func(_ context.Context, _ *functionsruntime.FunctionDef, _ json.RawMessage, _ map[string]string) (*functionsruntime.ExecutionResult, error) {
			return nil, errors.New("runtime: execution timeout after 10s")
		},
	}
	handler := newTestHandler(t, repo, rt, secrets)

	body := `{"event":{"type":"track"}}`
	rec := doRequest(t, handler, http.MethodPost, "/v1/functions/123/test?workspaceId=ws-123", body)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "execution")
}

// TestHandler_TestFunction_InvalidPayload verifies that malformed JSON in
// the test invocation request body returns HTTP 400.
func TestHandler_TestFunction_InvalidPayload(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := &mockFunctionRepository{
		getFn: func(_ context.Context, id string, _ string) (*functionsstorage.Function, error) {
			return &functionsstorage.Function{
				ID:          id,
				WorkspaceID: "ws-123",
				Name:        "Test Func",
				Type:        "source",
				Code:        "code",
				Version:     1,
				CreatedAt:   now,
				UpdatedAt:   now,
			}, nil
		},
	}
	handler := newTestHandler(t, repo, &mockFunctionRuntime{}, &mockSecretsManager{})

	rec := doRequest(t, handler, http.MethodPost, "/v1/functions/123/test?workspaceId=ws-123", `{invalid json`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp errorResponse
	require.NoError(t, jsonrs.NewDecoder(rec.Body).Decode(&errResp))
	require.Equal(t, "error", errResp.Status)
	require.Contains(t, errResp.Message, "invalid request body")
}
