// Package selectivesync_test contains black-box HTTP handler tests for the
// warehouse selective sync API.
//
// All tests exercise the Handler's exported methods (UpdateSelectiveSync,
// GetSelectiveSync) through net/http/httptest, following the project's
// table-driven t.Run() + testify/require conventions observed in
// warehouse/api/http_test.go and warehouse/backfill/handler_test.go.
//
// JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs
// exclusively — encoding/json must never be imported per .golangci.yml
// depguard rules.
package selectivesync_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/warehouse/selectivesync"
)

// ---------------------------------------------------------------------------
// Mock implementation of selectivesync.SelectiveSyncConfigurer
// ---------------------------------------------------------------------------

// mockSelectiveSyncService is a configurable test double for the
// selectivesync.SelectiveSyncConfigurer interface. Each function field
// controls the return value of the corresponding service method, enabling
// isolated tests for every handler code path without a real database or
// service layer.
type mockSelectiveSyncService struct {
	getConfigFn    func(ctx context.Context, sourceID, destID string) (*selectivesync.SelectiveSyncConfig, error)
	updateConfigFn func(ctx context.Context, req selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error)
}

// GetConfig delegates to the configured getConfigFn. If getConfigFn is nil
// the method panics, surfacing missing test setup immediately.
func (m *mockSelectiveSyncService) GetConfig(ctx context.Context, sourceID, destID string) (*selectivesync.SelectiveSyncConfig, error) {
	return m.getConfigFn(ctx, sourceID, destID)
}

// UpdateConfig delegates to the configured updateConfigFn. If updateConfigFn
// is nil the method panics, surfacing missing test setup immediately.
func (m *mockSelectiveSyncService) UpdateConfig(ctx context.Context, req selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error) {
	return m.updateConfigFn(ctx, req)
}

// errorResponse mirrors the unexported errorResponse struct defined in
// handler.go so that test assertions can decode error JSON payloads
// returned by the handler (package selectivesync_test cannot reference the
// unexported type directly).
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// TestHandler_UpdateSelectiveSync
// ---------------------------------------------------------------------------

// TestHandler_UpdateSelectiveSync exercises PUT /v1/warehouse/selective-sync
// via table-driven subtests covering: valid requests, validation failures
// (invalid JSON, missing fields), empty exclusion lists, sentinel service
// errors, and generic service errors.
func TestHandler_UpdateSelectiveSync(t *testing.T) {
	// A valid request body reused (or overridden) in many subtests.
	validReq := selectivesync.SelectiveSyncRequest{
		SourceID:      "test_source_id",
		DestinationID: "test_destination_id",
		WorkspaceID:   "test_workspace_id",
		ExcludedTables: []string{"users", "events"},
		ExcludedColumns: map[string][]string{
			"tracks": {"ip", "user_agent"},
		},
	}

	tests := []struct {
		name       string
		body       func() []byte
		mockSvc    *mockSelectiveSyncService
		wantStatus int
		wantErrMsg string // non-empty for error paths (substring match)
		wantResp   *selectivesync.SelectiveSyncResponse
	}{
		// -----------------------------------------------------------------
		// 1. Happy path: valid request returns 200 with updated config
		// -----------------------------------------------------------------
		{
			name: "valid request returns 200 with updated config",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockSelectiveSyncService{
				updateConfigFn: func(_ context.Context, req selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error) {
					return &selectivesync.SelectiveSyncResponse{
						Status:   "updated",
						SourceID: req.SourceID,
						DestID:   req.DestinationID,
					}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantResp: &selectivesync.SelectiveSyncResponse{
				Status:   "updated",
				SourceID: "test_source_id",
				DestID:   "test_destination_id",
			},
		},

		// -----------------------------------------------------------------
		// 2. Invalid JSON body returns 400
		// -----------------------------------------------------------------
		{
			name: "invalid JSON body returns 400",
			body: func() []byte {
				return []byte(`{not valid json`)
			},
			mockSvc: &mockSelectiveSyncService{
				updateConfigFn: func(_ context.Context, _ selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error) {
					t.Fatal("UpdateConfig should not be called for invalid JSON body")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "invalid JSON request body",
		},

		// -----------------------------------------------------------------
		// 3. Missing source_id returns 400
		// -----------------------------------------------------------------
		{
			name: "missing source_id returns 400",
			body: func() []byte {
				req := selectivesync.SelectiveSyncRequest{
					DestinationID:  "test_destination_id",
					WorkspaceID:    "test_workspace_id",
					ExcludedTables: []string{"users"},
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockSelectiveSyncService{
				updateConfigFn: func(_ context.Context, _ selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error) {
					t.Fatal("UpdateConfig should not be called when source_id is missing")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "source_id is required",
		},

		// -----------------------------------------------------------------
		// 4. Missing destination_id returns 400
		// -----------------------------------------------------------------
		{
			name: "missing destination_id returns 400",
			body: func() []byte {
				req := selectivesync.SelectiveSyncRequest{
					SourceID:       "test_source_id",
					WorkspaceID:    "test_workspace_id",
					ExcludedTables: []string{"users"},
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockSelectiveSyncService{
				updateConfigFn: func(_ context.Context, _ selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error) {
					t.Fatal("UpdateConfig should not be called when destination_id is missing")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "destination_id is required",
		},

		// -----------------------------------------------------------------
		// 5. Empty excludedTables and excludedColumns accepted (valid)
		//    An empty exclusion config means "no filtering" — all tables
		//    and columns are included. This is a valid configuration.
		// -----------------------------------------------------------------
		{
			name: "empty excludedTables and excludedColumns accepted",
			body: func() []byte {
				req := selectivesync.SelectiveSyncRequest{
					SourceID:      "test_source_id",
					DestinationID: "test_destination_id",
					WorkspaceID:   "test_workspace_id",
					// ExcludedTables and ExcludedColumns intentionally omitted
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockSelectiveSyncService{
				updateConfigFn: func(_ context.Context, req selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error) {
					return &selectivesync.SelectiveSyncResponse{
						Status:   "updated",
						SourceID: req.SourceID,
						DestID:   req.DestinationID,
					}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantResp: &selectivesync.SelectiveSyncResponse{
				Status:   "updated",
				SourceID: "test_source_id",
				DestID:   "test_destination_id",
			},
		},

		// -----------------------------------------------------------------
		// 6. Service returns generic error returns 500
		// -----------------------------------------------------------------
		{
			name: "service returns error returns 500",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockSelectiveSyncService{
				updateConfigFn: func(_ context.Context, _ selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error) {
					return nil, errors.New("unexpected database timeout")
				},
			},
			wantStatus: http.StatusInternalServerError,
			wantErrMsg: "internal server error",
		},

		// -----------------------------------------------------------------
		// 7. Selective sync disabled returns 403
		// -----------------------------------------------------------------
		{
			name: "selective sync disabled returns 403",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockSelectiveSyncService{
				updateConfigFn: func(_ context.Context, _ selectivesync.SelectiveSyncRequest) (*selectivesync.SelectiveSyncResponse, error) {
					return nil, selectivesync.ErrSelectiveSyncDisabled
				},
			},
			wantStatus: http.StatusForbidden,
			wantErrMsg: "selective sync feature is disabled",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create handler with NOP logger and the per-test mock service.
			h := selectivesync.NewHandler(logger.NOP, tc.mockSvc)

			// Build the HTTP test request.
			body := tc.body()
			req := httptest.NewRequest(http.MethodPut, "/v1/warehouse/selective-sync", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			// Invoke the handler directly (no chi routing needed for PUT).
			h.UpdateSelectiveSync(rec, req)

			// Assert HTTP status code.
			require.Equal(t, tc.wantStatus, rec.Code)

			// Assert Content-Type header is application/json.
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

			// Assert response body.
			if tc.wantErrMsg != "" {
				// Error path — decode error response and verify message.
				var errResp errorResponse
				err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
				require.NoError(t, err, "failed to decode error response body")
				require.Equal(t, "error", errResp.Status)
				require.Contains(t, errResp.Message, tc.wantErrMsg)
			} else {
				// Success path — decode selective sync response and verify fields.
				var resp selectivesync.SelectiveSyncResponse
				err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
				require.NoError(t, err, "failed to decode success response body")
				require.Equal(t, tc.wantResp.Status, resp.Status)
				require.Equal(t, tc.wantResp.SourceID, resp.SourceID)
				require.Equal(t, tc.wantResp.DestID, resp.DestID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestHandler_GetSelectiveSync
// ---------------------------------------------------------------------------

// TestHandler_GetSelectiveSync exercises
// GET /v1/warehouse/selective-sync/{sourceID}/{destID} via table-driven
// subtests covering: valid retrieval, not-found, empty path parameters,
// and generic service errors.
func TestHandler_GetSelectiveSync(t *testing.T) {
	tests := []struct {
		name       string
		sourceID   string
		destID     string
		useRouter  bool // true → full chi.Router; false → direct route context injection
		mockSvc    *mockSelectiveSyncService
		wantStatus int
		wantErrMsg string // non-empty for error paths (substring match)
		wantConfig *selectivesync.SelectiveSyncConfig
	}{
		// -----------------------------------------------------------------
		// 1. Valid source and dest returns config
		// -----------------------------------------------------------------
		{
			name:      "valid source and dest returns config",
			sourceID:  "src123",
			destID:    "dest456",
			useRouter: true,
			mockSvc: &mockSelectiveSyncService{
				getConfigFn: func(_ context.Context, sourceID, destID string) (*selectivesync.SelectiveSyncConfig, error) {
					return &selectivesync.SelectiveSyncConfig{
						ID:            1,
						SourceID:      sourceID,
						DestinationID: destID,
						WorkspaceID:   "test_workspace_id",
						ExcludedTables: []string{"users", "events"},
						ExcludedColumns: map[string][]string{
							"tracks": {"ip", "user_agent"},
						},
					}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantConfig: &selectivesync.SelectiveSyncConfig{
				ID:            1,
				SourceID:      "src123",
				DestinationID: "dest456",
				WorkspaceID:   "test_workspace_id",
				ExcludedTables: []string{"users", "events"},
				ExcludedColumns: map[string][]string{
					"tracks": {"ip", "user_agent"},
				},
			},
		},

		// -----------------------------------------------------------------
		// 2. Config not found returns 404
		// -----------------------------------------------------------------
		{
			name:      "config not found returns 404",
			sourceID:  "nonexistent_src",
			destID:    "nonexistent_dest",
			useRouter: true,
			mockSvc: &mockSelectiveSyncService{
				getConfigFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
					return nil, selectivesync.ErrSelectiveSyncNotFound
				},
			},
			wantStatus: http.StatusNotFound,
			wantErrMsg: "selective sync configuration not found",
		},

		// -----------------------------------------------------------------
		// 3. Empty sourceID returns 400
		//    Chi router cannot match an empty path segment, so we use
		//    direct route context injection to test the handler's
		//    validation of empty sourceID.
		// -----------------------------------------------------------------
		{
			name:      "empty sourceID returns 400",
			sourceID:  "",
			destID:    "dest456",
			useRouter: false,
			mockSvc: &mockSelectiveSyncService{
				getConfigFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
					t.Fatal("GetConfig should not be called when sourceID is empty")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "sourceID is required",
		},

		// -----------------------------------------------------------------
		// 4. Empty destID returns 400
		//    Same direct injection approach as test 3 for the destID param.
		// -----------------------------------------------------------------
		{
			name:      "empty destID returns 400",
			sourceID:  "src123",
			destID:    "",
			useRouter: false,
			mockSvc: &mockSelectiveSyncService{
				getConfigFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
					t.Fatal("GetConfig should not be called when destID is empty")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "destID is required",
		},

		// -----------------------------------------------------------------
		// 5. Service error returns 500
		// -----------------------------------------------------------------
		{
			name:      "service error returns 500",
			sourceID:  "src123",
			destID:    "dest456",
			useRouter: true,
			mockSvc: &mockSelectiveSyncService{
				getConfigFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
					return nil, errors.New("unexpected database timeout")
				},
			},
			wantStatus: http.StatusInternalServerError,
			wantErrMsg: "internal server error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create handler with NOP logger and the per-test mock service.
			h := selectivesync.NewHandler(logger.NOP, tc.mockSvc)
			rec := httptest.NewRecorder()

			if tc.useRouter {
				// Use a full chi.Router so that URL path parameters
				// ({sourceID}, {destID}) are properly extracted by
				// chi.URLParam() inside the handler.
				r := chi.NewRouter()
				r.Get("/v1/warehouse/selective-sync/{sourceID}/{destID}", h.GetSelectiveSync)

				req := httptest.NewRequest(
					http.MethodGet,
					"/v1/warehouse/selective-sync/"+tc.sourceID+"/"+tc.destID,
					nil,
				)
				r.ServeHTTP(rec, req)
			} else {
				// Direct route context injection for empty parameter
				// edge cases that cannot be expressed in a Chi route
				// (empty path segments don't match {param} patterns).
				rctx := chi.NewRouteContext()
				rctx.URLParams.Add("sourceID", tc.sourceID)
				rctx.URLParams.Add("destID", tc.destID)

				req := httptest.NewRequest(
					http.MethodGet,
					"/v1/warehouse/selective-sync",
					nil,
				)
				req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
				h.GetSelectiveSync(rec, req)
			}

			// Assert HTTP status code.
			require.Equal(t, tc.wantStatus, rec.Code)

			// Assert Content-Type header is application/json.
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

			// Assert response body.
			if tc.wantErrMsg != "" {
				// Error path — decode error response and verify message.
				var errResp errorResponse
				err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
				require.NoError(t, err, "failed to decode error response body")
				require.Equal(t, "error", errResp.Status)
				require.Contains(t, errResp.Message, tc.wantErrMsg)
			} else {
				// Success path — decode the full SelectiveSyncConfig.
				var cfg selectivesync.SelectiveSyncConfig
				err := jsonrs.NewDecoder(rec.Body).Decode(&cfg)
				require.NoError(t, err, "failed to decode success response body")
				require.Equal(t, tc.wantConfig.ID, cfg.ID)
				require.Equal(t, tc.wantConfig.SourceID, cfg.SourceID)
				require.Equal(t, tc.wantConfig.DestinationID, cfg.DestinationID)
				require.Equal(t, tc.wantConfig.WorkspaceID, cfg.WorkspaceID)
				require.Equal(t, tc.wantConfig.ExcludedTables, cfg.ExcludedTables)
				require.Equal(t, tc.wantConfig.ExcludedColumns, cfg.ExcludedColumns)
			}
		})
	}
}
