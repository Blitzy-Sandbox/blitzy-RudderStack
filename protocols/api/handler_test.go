// Package api_test provides comprehensive black-box HTTP handler tests for the
// Tracking Plan Management REST API (E-024).
//
// All tests exercise the Handler's exported methods through net/http/httptest,
// following the project's table-driven t.Run() + testify/require conventions
// observed in warehouse/backfill/handler_test.go.
//
// JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs
// exclusively — encoding/json must never be imported per .golangci.yml
// depguard rules.
package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/protocols/api"
)

// ---------------------------------------------------------------------------
// Mock implementation of api.TrackingPlanService
// ---------------------------------------------------------------------------

// mockTrackingPlanService is a configurable test double for the
// api.TrackingPlanService interface. Each function field controls the
// return value of the corresponding service method, enabling isolated
// tests for every handler code path without a real database or service.
// Following the warehouse/backfill/handler_test.go mock pattern.
type mockTrackingPlanService struct {
	createFn      func(ctx context.Context, workspaceID string, req api.CreateTrackingPlanRequest) (string, error)
	getFn         func(ctx context.Context, workspaceID, id string) (*api.TrackingPlanResponse, error)
	updateFn      func(ctx context.Context, workspaceID, id string, req api.UpdateTrackingPlanRequest) error
	deleteFn      func(ctx context.Context, workspaceID, id string) error
	listFn        func(ctx context.Context, workspaceID string) ([]api.TrackingPlanResponse, error)
	getVersionsFn func(ctx context.Context, workspaceID, trackingPlanID string) ([]api.TrackingPlanVersionResponse, error)
	importCSVFn   func(ctx context.Context, workspaceID, trackingPlanID string, csvData []byte) error
	exportCSVFn   func(ctx context.Context, workspaceID, trackingPlanID string) ([]byte, error)
}

// Create delegates to the configured createFn. If createFn is nil, the method
// panics, surfacing missing test setup immediately.
func (m *mockTrackingPlanService) Create(ctx context.Context, workspaceID string, req api.CreateTrackingPlanRequest) (string, error) {
	return m.createFn(ctx, workspaceID, req)
}

// Get delegates to the configured getFn.
func (m *mockTrackingPlanService) Get(ctx context.Context, workspaceID, id string) (*api.TrackingPlanResponse, error) {
	return m.getFn(ctx, workspaceID, id)
}

// Update delegates to the configured updateFn.
func (m *mockTrackingPlanService) Update(ctx context.Context, workspaceID, id string, req api.UpdateTrackingPlanRequest) error {
	return m.updateFn(ctx, workspaceID, id, req)
}

// Delete delegates to the configured deleteFn.
func (m *mockTrackingPlanService) Delete(ctx context.Context, workspaceID, id string) error {
	return m.deleteFn(ctx, workspaceID, id)
}

// List delegates to the configured listFn.
func (m *mockTrackingPlanService) List(ctx context.Context, workspaceID string) ([]api.TrackingPlanResponse, error) {
	return m.listFn(ctx, workspaceID)
}

// GetVersions delegates to the configured getVersionsFn.
func (m *mockTrackingPlanService) GetVersions(ctx context.Context, workspaceID, trackingPlanID string) ([]api.TrackingPlanVersionResponse, error) {
	return m.getVersionsFn(ctx, workspaceID, trackingPlanID)
}

// ImportCSV delegates to the configured importCSVFn.
func (m *mockTrackingPlanService) ImportCSV(ctx context.Context, workspaceID, trackingPlanID string, csvData []byte) error {
	return m.importCSVFn(ctx, workspaceID, trackingPlanID, csvData)
}

// ExportCSV delegates to the configured exportCSVFn.
func (m *mockTrackingPlanService) ExportCSV(ctx context.Context, workspaceID, trackingPlanID string) ([]byte, error) {
	return m.exportCSVFn(ctx, workspaceID, trackingPlanID)
}

// ---------------------------------------------------------------------------
// Response types for test assertions
// ---------------------------------------------------------------------------

// errorResponse mirrors the unexported errorResponse struct defined in
// handler.go so that test assertions can decode error JSON payloads
// returned by the handler (package api_test cannot reference the
// unexported type directly).
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// okResponse mirrors the success response format {"status": "ok"}
// used by UpdateTrackingPlan and ImportCSV handlers.
type okResponse struct {
	Status string `json:"status"`
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// testWorkspaceID is a deterministic workspace identifier used across all
// subtests for repeatable assertions and workspace isolation verification.
const testWorkspaceID = "ws-test-001"

// newTestHandler constructs a Handler with a NOP logger and the provided
// mock service, suppressing log output during test runs while satisfying
// the Handler's logger.Logger dependency.
// Following warehouse/backfill/handler_test.go:377 pattern.
func newTestHandler(svc api.TrackingPlanService) *api.Handler {
	return api.NewHandler(logger.NOP, svc)
}

// newTestRouter creates a chi router with all tracking plan routes mounted,
// enabling full HTTP routing integration tests including chi URL parameter
// extraction for {id} path parameters.
func newTestRouter(h *api.Handler) http.Handler {
	return api.NewRouter(h)
}

// doRequest is a helper that builds an HTTP test request, sends it through
// the router, and returns the response recorder. Setting workspaceID to ""
// omits the X-Rudder-Workspace-Id header (for testing missing workspace
// header scenarios).
// Following warehouse/backfill/handler_test.go:381-383 pattern.
func doRequest(t *testing.T, router http.Handler, method, path string, body []byte, workspaceID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if workspaceID != "" {
		req.Header.Set(api.WorkspaceIDHeader, workspaceID)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Test_CreateTrackingPlan
// ---------------------------------------------------------------------------

// Test_CreateTrackingPlan exercises POST /tracking-plans via subtests
// covering: valid creation, invalid JSON, missing required name, service
// error, invalid schema error, and missing workspace header.
func Test_CreateTrackingPlan(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			createFn: func(_ context.Context, workspaceID string, req api.CreateTrackingPlanRequest) (string, error) {
				require.Equal(t, testWorkspaceID, workspaceID)
				require.Equal(t, "My Tracking Plan", req.Name)
				return "tp-001", nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, err := jsonrs.Marshal(api.CreateTrackingPlanRequest{
			Name:              "My Tracking Plan",
			Schema:            []byte(`{"type":"object"}`),
			EnforcementConfig: []byte(`{"mode":"block"}`),
		})
		require.NoError(t, err)

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans", body, testWorkspaceID)

		require.Equal(t, http.StatusCreated, rec.Code)

		var resp api.CreateResponse
		err = jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, "tp-001", resp.ID)
	})

	t.Run("InvalidJSON", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			createFn: func(_ context.Context, _ string, _ api.CreateTrackingPlanRequest) (string, error) {
				t.Fatal("Create should not be called for invalid JSON body")
				return "", errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans", []byte(`{not valid json`), testWorkspaceID)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp.Status)
		require.Contains(t, errResp.Message, "invalid JSON request body")
	})

	t.Run("MissingName", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			createFn: func(_ context.Context, _ string, _ api.CreateTrackingPlanRequest) (string, error) {
				t.Fatal("Create should not be called when name is missing")
				return "", errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, _ := jsonrs.Marshal(api.CreateTrackingPlanRequest{
			Schema: []byte(`{"type":"object"}`),
		})

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans", body, testWorkspaceID)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp.Status)
		require.Contains(t, errResp.Message, "name is required")
	})

	t.Run("ServiceError", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			createFn: func(_ context.Context, _ string, _ api.CreateTrackingPlanRequest) (string, error) {
				return "", errors.New("unexpected database timeout")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, _ := jsonrs.Marshal(api.CreateTrackingPlanRequest{
			Name:   "My Tracking Plan",
			Schema: []byte(`{"type":"object"}`),
		})

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans", body, testWorkspaceID)

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp.Status)
		assert.Contains(t, errResp.Message, "internal server error")
	})

	t.Run("InvalidSchemaError", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			createFn: func(_ context.Context, _ string, _ api.CreateTrackingPlanRequest) (string, error) {
				return "", api.ErrInvalidSchema
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, _ := jsonrs.Marshal(api.CreateTrackingPlanRequest{
			Name:   "Bad Schema Plan",
			Schema: []byte(`not-valid-schema`),
		})

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans", body, testWorkspaceID)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Contains(t, errResp.Message, "invalid tracking plan schema")
	})

	t.Run("MissingWorkspaceHeader", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			createFn: func(_ context.Context, _ string, _ api.CreateTrackingPlanRequest) (string, error) {
				t.Fatal("Create should not be called when workspace header is missing")
				return "", errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, _ := jsonrs.Marshal(api.CreateTrackingPlanRequest{
			Name:   "My Tracking Plan",
			Schema: []byte(`{"type":"object"}`),
		})

		// Pass empty workspace ID to omit the header.
		rec := doRequest(t, router, http.MethodPost, "/tracking-plans", body, "")

		require.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp.Status)
		require.Contains(t, errResp.Message, "missing workspace ID header")
	})
}

// ---------------------------------------------------------------------------
// Test_GetTrackingPlan
// ---------------------------------------------------------------------------

// Test_GetTrackingPlan exercises GET /tracking-plans/{id} via subtests
// covering: successful retrieval, not found, nil optional fields, and
// service error.
func Test_GetTrackingPlan(t *testing.T) {
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	t.Run("Success", func(t *testing.T) {
		expectedTP := &api.TrackingPlanResponse{
			ID:                "tp-001",
			WorkspaceID:       testWorkspaceID,
			Name:              "My Tracking Plan",
			Schema:            []byte(`{"type":"object"}`),
			Version:           3,
			EnforcementConfig: []byte(`{"mode":"block"}`),
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		svc := &mockTrackingPlanService{
			getFn: func(_ context.Context, workspaceID, id string) (*api.TrackingPlanResponse, error) {
				require.Equal(t, testWorkspaceID, workspaceID)
				require.Equal(t, "tp-001", id)
				return expectedTP, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		var resp api.TrackingPlanResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, "tp-001", resp.ID)
		assert.Equal(t, testWorkspaceID, resp.WorkspaceID)
		assert.Equal(t, "My Tracking Plan", resp.Name)
		assert.Equal(t, 3, resp.Version)
		require.NotNil(t, resp.Schema)
	})

	t.Run("NilOptionalFields", func(t *testing.T) {
		// Verify that a response with nil Schema and EnforcementConfig
		// round-trips correctly through JSON serialization.
		svc := &mockTrackingPlanService{
			getFn: func(_ context.Context, _, _ string) (*api.TrackingPlanResponse, error) {
				return &api.TrackingPlanResponse{
					ID:          "tp-003",
					WorkspaceID: testWorkspaceID,
					Name:        "Minimal Plan",
					Version:     1,
					CreatedAt:   now,
					UpdatedAt:   now,
					// Schema and EnforcementConfig intentionally nil.
				}, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-003", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		var resp api.TrackingPlanResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, "tp-003", resp.ID)
		require.Nil(t, resp.Schema)
		require.Nil(t, resp.EnforcementConfig)
	})

	t.Run("NotFound", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			getFn: func(_ context.Context, _, _ string) (*api.TrackingPlanResponse, error) {
				return nil, api.ErrTrackingPlanNotFound
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-999", nil, testWorkspaceID)

		require.Equal(t, http.StatusNotFound, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp.Status)
		require.Contains(t, errResp.Message, "tracking plan not found")
	})

	t.Run("ServiceError", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			getFn: func(_ context.Context, _, _ string) (*api.TrackingPlanResponse, error) {
				return nil, errors.New("database connection lost")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001", nil, testWorkspaceID)

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp.Status)
		require.Contains(t, errResp.Message, "internal server error")
	})

	t.Run("MissingWorkspaceHeader", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			getFn: func(_ context.Context, _, _ string) (*api.TrackingPlanResponse, error) {
				t.Fatal("Get should not be called when workspace header is missing")
				return nil, errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001", nil, "")

		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// Test_ListTrackingPlans
// ---------------------------------------------------------------------------

// Test_ListTrackingPlans exercises GET /tracking-plans via subtests
// covering: successful list, empty list (nil-coalesced), missing workspace,
// and service error.
func Test_ListTrackingPlans(t *testing.T) {
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	t.Run("Success", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			listFn: func(_ context.Context, workspaceID string) ([]api.TrackingPlanResponse, error) {
				require.Equal(t, testWorkspaceID, workspaceID)
				return []api.TrackingPlanResponse{
					{
						ID:          "tp-001",
						WorkspaceID: testWorkspaceID,
						Name:        "Plan A",
						Version:     1,
						CreatedAt:   now,
						UpdatedAt:   now,
					},
					{
						ID:          "tp-002",
						WorkspaceID: testWorkspaceID,
						Name:        "Plan B",
						Version:     2,
						CreatedAt:   now,
						UpdatedAt:   now,
					},
				}, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		var resp []api.TrackingPlanResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Len(t, resp, 2)
		assert.Equal(t, "Plan A", resp[0].Name)
		assert.Equal(t, "Plan B", resp[1].Name)
	})

	t.Run("Empty", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			listFn: func(_ context.Context, _ string) ([]api.TrackingPlanResponse, error) {
				// Return nil; handler should nil-coalesce to empty slice
				// to ensure JSON serializes as [] not null.
				return nil, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		var resp []api.TrackingPlanResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Empty(t, resp)
	})

	t.Run("MissingWorkspaceHeader", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			listFn: func(_ context.Context, _ string) ([]api.TrackingPlanResponse, error) {
				t.Fatal("List should not be called when workspace header is missing")
				return nil, errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans", nil, "")

		require.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Contains(t, errResp.Message, "missing workspace ID header")
	})

	t.Run("ServiceError", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			listFn: func(_ context.Context, _ string) ([]api.TrackingPlanResponse, error) {
				return nil, errors.New("unexpected error")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans", nil, testWorkspaceID)

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp.Status)
	})
}

// ---------------------------------------------------------------------------
// Test_UpdateTrackingPlan
// ---------------------------------------------------------------------------

// Test_UpdateTrackingPlan exercises PUT /tracking-plans/{id} via subtests
// covering: successful update, not found, invalid JSON, missing workspace,
// and invalid schema from service.
func Test_UpdateTrackingPlan(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			updateFn: func(_ context.Context, workspaceID, id string, req api.UpdateTrackingPlanRequest) error {
				require.Equal(t, testWorkspaceID, workspaceID)
				require.Equal(t, "tp-001", id)
				require.Equal(t, "Updated Plan", req.Name)
				return nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, err := jsonrs.Marshal(api.UpdateTrackingPlanRequest{
			Name:      "Updated Plan",
			Schema:    []byte(`{"type":"object","required":["event"]}`),
			Changelog: "Added required event field",
		})
		require.NoError(t, err)

		rec := doRequest(t, router, http.MethodPut, "/tracking-plans/tp-001", body, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		var resp okResponse
		err = jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Status)
	})

	t.Run("NotFound", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			updateFn: func(_ context.Context, _, _ string, _ api.UpdateTrackingPlanRequest) error {
				return api.ErrTrackingPlanNotFound
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, _ := jsonrs.Marshal(api.UpdateTrackingPlanRequest{Name: "Updated"})

		rec := doRequest(t, router, http.MethodPut, "/tracking-plans/tp-999", body, testWorkspaceID)

		require.Equal(t, http.StatusNotFound, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Contains(t, errResp.Message, "tracking plan not found")
	})

	t.Run("InvalidJSON", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			updateFn: func(_ context.Context, _, _ string, _ api.UpdateTrackingPlanRequest) error {
				t.Fatal("Update should not be called for invalid JSON body")
				return errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodPut, "/tracking-plans/tp-001", []byte(`{broken`), testWorkspaceID)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Contains(t, errResp.Message, "invalid JSON request body")
	})

	t.Run("MissingWorkspaceHeader", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			updateFn: func(_ context.Context, _, _ string, _ api.UpdateTrackingPlanRequest) error {
				t.Fatal("Update should not be called when workspace header is missing")
				return errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, _ := jsonrs.Marshal(api.UpdateTrackingPlanRequest{Name: "Test"})

		rec := doRequest(t, router, http.MethodPut, "/tracking-plans/tp-001", body, "")

		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("InvalidSchemaFromService", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			updateFn: func(_ context.Context, _, _ string, _ api.UpdateTrackingPlanRequest) error {
				return api.ErrInvalidSchema
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, _ := jsonrs.Marshal(api.UpdateTrackingPlanRequest{
			Name:   "Plan With Bad Schema",
			Schema: []byte(`invalid-schema`),
		})

		rec := doRequest(t, router, http.MethodPut, "/tracking-plans/tp-001", body, testWorkspaceID)

		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// Test_DeleteTrackingPlan
// ---------------------------------------------------------------------------

// Test_DeleteTrackingPlan exercises DELETE /tracking-plans/{id} via subtests
// covering: successful deletion (204 No Content), not found, service error,
// and missing workspace.
func Test_DeleteTrackingPlan(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			deleteFn: func(_ context.Context, workspaceID, id string) error {
				require.Equal(t, testWorkspaceID, workspaceID)
				require.Equal(t, "tp-001", id)
				return nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodDelete, "/tracking-plans/tp-001", nil, testWorkspaceID)

		require.Equal(t, http.StatusNoContent, rec.Code)
		// 204 No Content should have no body.
		require.Empty(t, rec.Body.String())
	})

	t.Run("NotFound", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			deleteFn: func(_ context.Context, _, _ string) error {
				return api.ErrTrackingPlanNotFound
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodDelete, "/tracking-plans/tp-999", nil, testWorkspaceID)

		require.Equal(t, http.StatusNotFound, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Contains(t, errResp.Message, "tracking plan not found")
	})

	t.Run("ServiceError", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			deleteFn: func(_ context.Context, _, _ string) error {
				return errors.New("connection refused")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodDelete, "/tracking-plans/tp-001", nil, testWorkspaceID)

		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("MissingWorkspaceHeader", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			deleteFn: func(_ context.Context, _, _ string) error {
				t.Fatal("Delete should not be called when workspace header is missing")
				return errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodDelete, "/tracking-plans/tp-001", nil, "")

		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// Test_GetVersionHistory
// ---------------------------------------------------------------------------

// Test_GetVersionHistory exercises GET /tracking-plans/{id}/versions via
// subtests covering: successful version list, empty versions, version not
// found, and missing workspace.
func Test_GetVersionHistory(t *testing.T) {
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	t.Run("Success", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			getVersionsFn: func(_ context.Context, workspaceID, trackingPlanID string) ([]api.TrackingPlanVersionResponse, error) {
				require.Equal(t, testWorkspaceID, workspaceID)
				require.Equal(t, "tp-001", trackingPlanID)
				return []api.TrackingPlanVersionResponse{
					{
						ID:             "v-001",
						TrackingPlanID: "tp-001",
						Version:        1,
						Schema:         []byte(`{"type":"object"}`),
						Changelog:      "Initial version",
						CreatedAt:      now,
					},
					{
						ID:             "v-002",
						TrackingPlanID: "tp-001",
						Version:        2,
						Schema:         []byte(`{"type":"object","required":["event"]}`),
						Changelog:      "Added required event field",
						CreatedAt:      now.Add(time.Hour),
					},
				}, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/versions", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		var resp []api.TrackingPlanVersionResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		require.Len(t, resp, 2)
		assert.Equal(t, 1, resp[0].Version)
		assert.Equal(t, 2, resp[1].Version)
		assert.Equal(t, "Initial version", resp[0].Changelog)
	})

	t.Run("Empty", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			getVersionsFn: func(_ context.Context, _, _ string) ([]api.TrackingPlanVersionResponse, error) {
				// Return nil; handler should nil-coalesce to empty slice.
				return nil, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/versions", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		var resp []api.TrackingPlanVersionResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Empty(t, resp)
	})

	t.Run("VersionNotFound", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			getVersionsFn: func(_ context.Context, _, _ string) ([]api.TrackingPlanVersionResponse, error) {
				return nil, api.ErrVersionNotFound
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/versions", nil, testWorkspaceID)

		require.Equal(t, http.StatusNotFound, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		assert.Contains(t, errResp.Message, "tracking plan version not found")
	})

	t.Run("MissingWorkspaceHeader", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			getVersionsFn: func(_ context.Context, _, _ string) ([]api.TrackingPlanVersionResponse, error) {
				t.Fatal("GetVersions should not be called when workspace header is missing")
				return nil, errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/versions", nil, "")

		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// Test_CSVImport
// ---------------------------------------------------------------------------

// Test_CSVImport exercises POST /tracking-plans/{id}/import via subtests
// covering: successful import, invalid CSV data, empty body, service error,
// and missing workspace header.
func Test_CSVImport(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		csvData := []byte("event_name,property,type\nPage Viewed,url,string\nButton Clicked,label,string")
		svc := &mockTrackingPlanService{
			importCSVFn: func(_ context.Context, workspaceID, trackingPlanID string, data []byte) error {
				require.Equal(t, testWorkspaceID, workspaceID)
				require.Equal(t, "tp-001", trackingPlanID)
				require.Equal(t, csvData, data)
				return nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans/tp-001/import", csvData, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		var resp okResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Status)
	})

	t.Run("InvalidData", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			importCSVFn: func(_ context.Context, _, _ string, _ []byte) error {
				return api.ErrInvalidCSV
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans/tp-001/import", []byte("bad,csv\ndata"), testWorkspaceID)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Contains(t, errResp.Message, "invalid CSV data")
	})

	t.Run("EmptyBody", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			importCSVFn: func(_ context.Context, _, _ string, _ []byte) error {
				t.Fatal("ImportCSV should not be called for empty body")
				return errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans/tp-001/import", nil, testWorkspaceID)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		var errResp errorResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Contains(t, errResp.Message, "request body is empty")
	})

	t.Run("ServiceError", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			importCSVFn: func(_ context.Context, _, _ string, _ []byte) error {
				return errors.New("storage unavailable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans/tp-001/import", []byte("col1,col2\nval1,val2"), testWorkspaceID)

		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("MissingWorkspaceHeader", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			importCSVFn: func(_ context.Context, _, _ string, _ []byte) error {
				t.Fatal("ImportCSV should not be called when workspace header is missing")
				return errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodPost, "/tracking-plans/tp-001/import", []byte("some,csv"), "")

		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// Test_CSVExport
// ---------------------------------------------------------------------------

// Test_CSVExport exercises GET /tracking-plans/{id}/export via subtests
// covering: successful export with correct content type and content
// disposition headers, not found, service error, and missing workspace.
func Test_CSVExport(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		expectedCSV := []byte("event_name,property,type\nPage Viewed,url,string\n")
		svc := &mockTrackingPlanService{
			exportCSVFn: func(_ context.Context, workspaceID, trackingPlanID string) ([]byte, error) {
				require.Equal(t, testWorkspaceID, workspaceID)
				require.Equal(t, "tp-001", trackingPlanID)
				return expectedCSV, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/export", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/csv", rec.Header().Get("Content-Type"))
		assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
		assert.Equal(t, string(expectedCSV), rec.Body.String())
	})

	t.Run("NotFound", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			exportCSVFn: func(_ context.Context, _, _ string) ([]byte, error) {
				return nil, api.ErrTrackingPlanNotFound
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/export", nil, testWorkspaceID)

		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("ServiceError", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			exportCSVFn: func(_ context.Context, _, _ string) ([]byte, error) {
				return nil, errors.New("storage failure")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/export", nil, testWorkspaceID)

		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("MissingWorkspaceHeader", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			exportCSVFn: func(_ context.Context, _, _ string) ([]byte, error) {
				t.Fatal("ExportCSV should not be called when workspace header is missing")
				return nil, errors.New("unreachable")
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/export", nil, "")

		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// Test_WorkspaceIsolation
// ---------------------------------------------------------------------------

// Test_WorkspaceIsolation verifies that the handler extracts workspace ID from
// the X-Rudder-Workspace-Id header and passes it correctly to the service
// layer for all operations, ensuring multi-tenant workspace isolation.
func Test_WorkspaceIsolation(t *testing.T) {
	t.Run("CreatePassesWorkspaceID", func(t *testing.T) {
		var capturedWsID string
		svc := &mockTrackingPlanService{
			createFn: func(_ context.Context, workspaceID string, _ api.CreateTrackingPlanRequest) (string, error) {
				capturedWsID = workspaceID
				return "tp-001", nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		body, _ := jsonrs.Marshal(api.CreateTrackingPlanRequest{Name: "test"})
		rec := doRequest(t, router, http.MethodPost, "/tracking-plans", body, "ws-custom-abc")

		require.Equal(t, http.StatusCreated, rec.Code)
		require.Equal(t, "ws-custom-abc", capturedWsID)
	})

	t.Run("GetPassesWorkspaceID_DirectHandlerCall", func(t *testing.T) {
		// Direct handler call with chi context injection, following the
		// warehouse/backfill/handler_test.go:511-513 pattern.
		var capturedWsID, capturedID string
		svc := &mockTrackingPlanService{
			getFn: func(_ context.Context, workspaceID, id string) (*api.TrackingPlanResponse, error) {
				capturedWsID = workspaceID
				capturedID = id
				return &api.TrackingPlanResponse{ID: id, WorkspaceID: workspaceID}, nil
			},
		}
		h := newTestHandler(svc)

		// Build request with chi route context injected directly.
		req := httptest.NewRequest(http.MethodGet, "/tracking-plans/tp-direct", nil)
		req.Header.Set(api.WorkspaceIDHeader, "ws-direct-test")

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "tp-direct")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		rec := httptest.NewRecorder()
		h.GetTrackingPlan(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "ws-direct-test", capturedWsID)
		require.Equal(t, "tp-direct", capturedID)
	})

	t.Run("ListPassesWorkspaceID", func(t *testing.T) {
		var capturedWsID string
		svc := &mockTrackingPlanService{
			listFn: func(_ context.Context, workspaceID string) ([]api.TrackingPlanResponse, error) {
				capturedWsID = workspaceID
				return []api.TrackingPlanResponse{}, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans", nil, "ws-list-test")

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "ws-list-test", capturedWsID)
	})

	t.Run("DeletePassesWorkspaceID", func(t *testing.T) {
		var capturedWsID string
		svc := &mockTrackingPlanService{
			deleteFn: func(_ context.Context, workspaceID, _ string) error {
				capturedWsID = workspaceID
				return nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodDelete, "/tracking-plans/tp-001", nil, "ws-delete-test")

		require.Equal(t, http.StatusNoContent, rec.Code)
		require.Equal(t, "ws-delete-test", capturedWsID)
	})
}

// ---------------------------------------------------------------------------
// Test_ErrorResponseFormat
// ---------------------------------------------------------------------------

// Test_ErrorResponseFormat verifies error responses follow the standard
// format: {"status": "error", "message": "..."} — matching the pattern from
// warehouse/backfill/handler.go:209-214.
func Test_ErrorResponseFormat(t *testing.T) {
	svc := &mockTrackingPlanService{
		getFn: func(_ context.Context, _, _ string) (*api.TrackingPlanResponse, error) {
			return nil, api.ErrTrackingPlanNotFound
		},
	}
	h := newTestHandler(svc)
	router := newTestRouter(h)

	rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001", nil, testWorkspaceID)

	require.Equal(t, http.StatusNotFound, rec.Code)

	var errResp errorResponse
	err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
	require.NoError(t, err)
	require.Equal(t, "error", errResp.Status)
	require.NotNil(t, errResp.Message)
	require.Contains(t, errResp.Message, "tracking plan not found")

	// Verify content type is application/json even for error responses.
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

// ---------------------------------------------------------------------------
// Test_SuccessResponseContentType
// ---------------------------------------------------------------------------

// Test_SuccessResponseContentType verifies that JSON success responses have
// Content-Type: application/json header, and that CSV export uses text/csv.
func Test_SuccessResponseContentType(t *testing.T) {
	t.Run("JSONEndpoint", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			listFn: func(_ context.Context, _ string) ([]api.TrackingPlanResponse, error) {
				return []api.TrackingPlanResponse{}, nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	})

	t.Run("CSVExportEndpoint", func(t *testing.T) {
		svc := &mockTrackingPlanService{
			exportCSVFn: func(_ context.Context, _, _ string) ([]byte, error) {
				return []byte("col1,col2\nval1,val2\n"), nil
			},
		}
		h := newTestHandler(svc)
		router := newTestRouter(h)

		rec := doRequest(t, router, http.MethodGet, "/tracking-plans/tp-001/export", nil, testWorkspaceID)

		require.Equal(t, http.StatusOK, rec.Code)
		// CSV export should use text/csv, not application/json.
		assert.Equal(t, "text/csv", rec.Header().Get("Content-Type"))
	})
}
