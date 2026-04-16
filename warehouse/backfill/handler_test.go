// Package backfill_test contains black-box HTTP handler tests for the
// warehouse backfill API.
//
// All tests exercise the Handler's exported methods (TriggerBackfill,
// GetBackfillStatus) through net/http/httptest, following the project's
// table-driven t.Run() + testify/require conventions observed in
// warehouse/api/http_test.go and warehouse/internal/repo/source_test.go.
//
// JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs
// exclusively — encoding/json must never be imported per .golangci.yml
// depguard rules.
package backfill_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/warehouse/backfill"
)

// ---------------------------------------------------------------------------
// Mock implementation of backfill.BackfillTrigger
// ---------------------------------------------------------------------------

// mockBackfillService is a configurable test double for the
// backfill.BackfillTrigger interface. Each function field controls the
// return value of the corresponding service method, enabling isolated
// tests for every handler code path without a real database or service.
type mockBackfillService struct {
	triggerFn   func(ctx context.Context, req backfill.BackfillRequest) (*backfill.BackfillResponse, error)
	getStatusFn func(ctx context.Context, id int64) (*backfill.BackfillJob, error)
}

// Trigger delegates to the configured triggerFn. If triggerFn is nil the
// method panics, surfacing missing test setup immediately.
func (m *mockBackfillService) Trigger(ctx context.Context, req backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
	return m.triggerFn(ctx, req)
}

// GetStatus delegates to the configured getStatusFn. If getStatusFn is nil
// the method panics, surfacing missing test setup immediately.
func (m *mockBackfillService) GetStatus(ctx context.Context, id int64) (*backfill.BackfillJob, error) {
	return m.getStatusFn(ctx, id)
}

// errorResponse mirrors the unexported errorResponse struct defined in
// handler.go so that test assertions can decode error JSON payloads
// returned by the handler (package backfill_test cannot reference the
// unexported type directly).
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// TestHandler_TriggerBackfill
// ---------------------------------------------------------------------------

// TestHandler_TriggerBackfill exercises POST /v1/warehouse/backfill via
// table-driven subtests covering: valid requests, validation failures,
// sentinel service errors, and generic service errors.
func TestHandler_TriggerBackfill(t *testing.T) {
	// Deterministic dates used across subtests for repeatable assertions.
	startDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	// A valid request body reused (or overridden) in many subtests.
	validReq := backfill.BackfillRequest{
		SourceID:      "test_source_id",
		DestinationID: "test_destination_id",
		WorkspaceID:   "test_workspace_id",
		StartDate:     startDate,
		EndDate:       endDate,
	}

	tests := []struct {
		name           string
		body           func() []byte        // request body factory
		mockSvc        *mockBackfillService // mock service for this subtest
		wantStatus     int                  // expected HTTP status code
		wantErrMsg     string               // expected error message (substring match); empty for success paths
		wantJobID      int64                // expected job ID for success paths
		wantBackfillSt string               // expected backfill status string for success paths
	}{
		// -----------------------------------------------------------------
		// 1. Happy path: valid request returns 201 with job ID
		// -----------------------------------------------------------------
		{
			name: "valid request returns 201 with job ID",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					return &backfill.BackfillResponse{
						JobID:  1,
						Status: backfill.StatusPending,
					}, nil
				},
			},
			wantStatus:     http.StatusCreated,
			wantJobID:      1,
			wantBackfillSt: backfill.StatusPending,
		},

		// -----------------------------------------------------------------
		// 2. Invalid JSON body returns 400
		// -----------------------------------------------------------------
		{
			name: "invalid JSON body returns 400",
			body: func() []byte {
				return []byte(`{not valid json`)
			},
			mockSvc: &mockBackfillService{
				// triggerFn should never be called for this test case.
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					t.Fatal("Trigger should not be called for invalid JSON body")
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
				req := backfill.BackfillRequest{
					DestinationID: "test_destination_id",
					WorkspaceID:   "test_workspace_id",
					StartDate:     startDate,
					EndDate:       endDate,
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					t.Fatal("Trigger should not be called when source_id is missing")
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
				req := backfill.BackfillRequest{
					SourceID:    "test_source_id",
					WorkspaceID: "test_workspace_id",
					StartDate:   startDate,
					EndDate:     endDate,
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					t.Fatal("Trigger should not be called when destination_id is missing")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "destination_id is required",
		},

		// -----------------------------------------------------------------
		// 5. Missing start_date returns 400
		// -----------------------------------------------------------------
		{
			name: "missing start_date returns 400",
			body: func() []byte {
				req := backfill.BackfillRequest{
					SourceID:      "test_source_id",
					DestinationID: "test_destination_id",
					WorkspaceID:   "test_workspace_id",
					EndDate:       endDate,
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					t.Fatal("Trigger should not be called when start_date is missing")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "start_date is required",
		},

		// -----------------------------------------------------------------
		// 6. Missing end_date returns 400
		// -----------------------------------------------------------------
		{
			name: "missing end_date returns 400",
			body: func() []byte {
				req := backfill.BackfillRequest{
					SourceID:      "test_source_id",
					DestinationID: "test_destination_id",
					WorkspaceID:   "test_workspace_id",
					StartDate:     startDate,
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					t.Fatal("Trigger should not be called when end_date is missing")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "end_date is required",
		},

		// -----------------------------------------------------------------
		// 7. Invalid date format returns 400
		//    Non-RFC3339 strings cause jsonrs.Decode to fail, producing
		//    the "invalid JSON request body" handler-level error.
		// -----------------------------------------------------------------
		{
			name: "invalid date format returns 400",
			body: func() []byte {
				// Raw JSON with non-RFC3339 date strings — jsonrs cannot
				// unmarshal these into time.Time fields.
				return []byte(`{
					"source_id": "test_source_id",
					"destination_id": "test_destination_id",
					"workspace_id": "test_workspace_id",
					"start_date": "2024-01-01",
					"end_date": "not-a-date"
				}`)
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					t.Fatal("Trigger should not be called for invalid date format")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "invalid JSON request body",
		},

		// -----------------------------------------------------------------
		// 8. start_date after end_date returns 400
		// -----------------------------------------------------------------
		{
			name: "start_date after end_date returns 400",
			body: func() []byte {
				req := backfill.BackfillRequest{
					SourceID:      "test_source_id",
					DestinationID: "test_destination_id",
					WorkspaceID:   "test_workspace_id",
					StartDate:     endDate,   // intentionally swapped
					EndDate:       startDate, // intentionally swapped
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					t.Fatal("Trigger should not be called when start_date > end_date")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "start_date must be before end_date",
		},

		// -----------------------------------------------------------------
		// 9. Date range exceeds maximum returns 400
		//    The handler delegates date-range-max validation to the service;
		//    the service returns ErrDateRangeExceedsMax which the handler
		//    maps to HTTP 400 via handleServiceError.
		// -----------------------------------------------------------------
		{
			name: "date range exceeds maximum returns 400",
			body: func() []byte {
				req := backfill.BackfillRequest{
					SourceID:      "test_source_id",
					DestinationID: "test_destination_id",
					WorkspaceID:   "test_workspace_id",
					StartDate:     startDate,
					EndDate:       startDate.AddDate(0, 0, 120), // 120 days > 90 max
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					return nil, backfill.ErrDateRangeExceedsMax
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "date range exceeds maximum allowed days",
		},

		// -----------------------------------------------------------------
		// 10. Service returns generic error returns 500
		// -----------------------------------------------------------------
		{
			name: "service returns error returns 500",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					return nil, errors.New("unexpected database timeout")
				},
			},
			wantStatus: http.StatusInternalServerError,
			wantErrMsg: "internal server error",
		},

		// -----------------------------------------------------------------
		// 11. Backfill disabled returns 403
		// -----------------------------------------------------------------
		{
			name: "backfill disabled returns 403",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					return nil, backfill.ErrBackfillDisabled
				},
			},
			wantStatus: http.StatusForbidden,
			wantErrMsg: "backfill feature is disabled",
		},

		// -----------------------------------------------------------------
		// 12. Concurrent limit reached returns 429
		// -----------------------------------------------------------------
		{
			name: "concurrent limit reached returns 429",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockBackfillService{
				triggerFn: func(_ context.Context, _ backfill.BackfillRequest) (*backfill.BackfillResponse, error) {
					return nil, backfill.ErrConcurrentLimitReached
				},
			},
			wantStatus: http.StatusTooManyRequests,
			wantErrMsg: "concurrent backfill job limit reached",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create handler with NOP logger and the per-test mock service.
			h := backfill.NewHandler(logger.NOP, tc.mockSvc)

			// Build the HTTP test request.
			body := tc.body()
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/backfill", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			// Invoke the handler.
			h.TriggerBackfill(rec, req)

			// Assert HTTP status code.
			require.Equal(t, tc.wantStatus, rec.Code)

			// Assert response body.
			if tc.wantErrMsg != "" {
				// Error path — decode error response and verify message.
				var errResp errorResponse
				err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
				require.NoError(t, err, "failed to decode error response body")
				require.Equal(t, "error", errResp.Status)
				require.Contains(t, errResp.Message, tc.wantErrMsg)
			} else {
				// Success path — decode backfill response and verify fields.
				var resp backfill.BackfillResponse
				err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
				require.NoError(t, err, "failed to decode success response body")
				require.Equal(t, tc.wantJobID, resp.JobID)
				require.Equal(t, tc.wantBackfillSt, resp.Status)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestHandler_GetBackfillStatus
// ---------------------------------------------------------------------------

// TestHandler_GetBackfillStatus exercises GET /v1/warehouse/backfill/{jobID}
// via table-driven subtests covering: valid retrieval, invalid job ID format,
// and non-existent job ID.
func TestHandler_GetBackfillStatus(t *testing.T) {
	// Deterministic timestamps for assertions.
	createdAt := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2024, 1, 1, 12, 5, 0, 0, time.UTC)

	tests := []struct {
		name       string
		jobIDParam string // value injected as chi URL param
		mockSvc    *mockBackfillService
		wantStatus int
		wantErrMsg string                // non-empty for error paths
		wantJob    *backfill.BackfillJob // expected job for success path
	}{
		// -----------------------------------------------------------------
		// 1. Valid job ID returns 200 with job details
		// -----------------------------------------------------------------
		{
			name:       "valid job ID returns status",
			jobIDParam: "1",
			mockSvc: &mockBackfillService{
				getStatusFn: func(_ context.Context, id int64) (*backfill.BackfillJob, error) {
					require.Equal(t, int64(1), id)
					return &backfill.BackfillJob{
						ID:            1,
						SourceID:      "test_source_id",
						DestinationID: "test_destination_id",
						WorkspaceID:   "test_workspace_id",
						StartDate:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
						EndDate:       time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
						Status:        backfill.StatusPending,
						CreatedAt:     createdAt,
						UpdatedAt:     updatedAt,
					}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantJob: &backfill.BackfillJob{
				ID:            1,
				SourceID:      "test_source_id",
				DestinationID: "test_destination_id",
				WorkspaceID:   "test_workspace_id",
				StartDate:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				EndDate:       time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
				Status:        backfill.StatusPending,
				CreatedAt:     createdAt,
				UpdatedAt:     updatedAt,
			},
		},

		// -----------------------------------------------------------------
		// 2. Invalid (non-numeric) job ID returns 400
		// -----------------------------------------------------------------
		{
			name:       "invalid job ID returns 400",
			jobIDParam: "not-a-number",
			mockSvc: &mockBackfillService{
				getStatusFn: func(_ context.Context, _ int64) (*backfill.BackfillJob, error) {
					t.Fatal("GetStatus should not be called for invalid job ID")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "invalid job ID",
		},

		// -----------------------------------------------------------------
		// 3. Non-existent job returns 404
		// -----------------------------------------------------------------
		{
			name:       "non-existent job returns 404",
			jobIDParam: "999",
			mockSvc: &mockBackfillService{
				getStatusFn: func(_ context.Context, id int64) (*backfill.BackfillJob, error) {
					require.Equal(t, int64(999), id)
					return nil, backfill.ErrBackfillJobNotFound
				},
			},
			wantStatus: http.StatusNotFound,
			wantErrMsg: "backfill job not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create handler with NOP logger and the per-test mock service.
			h := backfill.NewHandler(logger.NOP, tc.mockSvc)

			// Build the HTTP test request.
			req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/backfill/"+tc.jobIDParam, nil)

			// Inject chi route context with URL parameters so chi.URLParam()
			// can extract jobID inside the handler. This mirrors the pattern
			// used in warehouse/healthmonitor/handler_test.go.
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("jobID", tc.jobIDParam)
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

			rec := httptest.NewRecorder()

			// Invoke the handler.
			h.GetBackfillStatus(rec, req)

			// Assert HTTP status code.
			require.Equal(t, tc.wantStatus, rec.Code)

			// Assert response body.
			if tc.wantErrMsg != "" {
				// Error path — decode error response and verify message.
				var errResp errorResponse
				err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
				require.NoError(t, err, "failed to decode error response body")
				require.Equal(t, "error", errResp.Status)
				require.Contains(t, errResp.Message, tc.wantErrMsg)
			} else {
				// Success path — decode BackfillJob and verify fields.
				var job backfill.BackfillJob
				err := jsonrs.NewDecoder(rec.Body).Decode(&job)
				require.NoError(t, err, "failed to decode success response body")
				require.Equal(t, tc.wantJob.ID, job.ID)
				require.Equal(t, tc.wantJob.SourceID, job.SourceID)
				require.Equal(t, tc.wantJob.DestinationID, job.DestinationID)
				require.Equal(t, tc.wantJob.WorkspaceID, job.WorkspaceID)
				require.Equal(t, tc.wantJob.Status, job.Status)
				require.Equal(t, tc.wantJob.StartDate.UTC(), job.StartDate.UTC())
				require.Equal(t, tc.wantJob.EndDate.UTC(), job.EndDate.UTC())
				require.Equal(t, tc.wantJob.CreatedAt.UTC(), job.CreatedAt.UTC())
				require.Equal(t, tc.wantJob.UpdatedAt.UTC(), job.UpdatedAt.UTC())
			}
		})
	}
}
