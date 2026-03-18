// Package replay_test contains black-box HTTP handler tests for the
// warehouse replay API (E-035).
//
// All tests exercise the Handler's exported methods (TriggerReplay,
// GetReplayStatus) through net/http/httptest, following the project's
// table-driven t.Run() + testify/require conventions observed in
// warehouse/api/http_test.go.
//
// JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs
// exclusively — encoding/json must never be imported per .golangci.yml
// depguard rules.
package replay_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/warehouse/replay"
)

// ---------------------------------------------------------------------------
// Mock implementation of replay.ReplayTrigger
// ---------------------------------------------------------------------------

// mockReplayService is a configurable test double for the replay.ReplayTrigger
// interface. Each function field controls the return value of the corresponding
// service method, enabling isolated tests for every handler code path without
// a real gateway, archiver, or job store.
type mockReplayService struct {
	triggerFn   func(ctx context.Context, req replay.ReplayRequest) (*replay.ReplayResponse, error)
	getStatusFn func(ctx context.Context, jobID int64) (*replay.ReplayJob, error)
}

// Trigger delegates to the configured triggerFn. Panics if triggerFn is nil,
// surfacing missing test setup immediately.
func (m *mockReplayService) Trigger(ctx context.Context, req replay.ReplayRequest) (*replay.ReplayResponse, error) {
	if m.triggerFn != nil {
		return m.triggerFn(ctx, req)
	}
	return nil, errors.New("not implemented")
}

// GetStatus delegates to the configured getStatusFn. Panics if getStatusFn
// is nil, surfacing missing test setup immediately.
func (m *mockReplayService) GetStatus(ctx context.Context, jobID int64) (*replay.ReplayJob, error) {
	if m.getStatusFn != nil {
		return m.getStatusFn(ctx, jobID)
	}
	return nil, errors.New("not implemented")
}

// errorResponse mirrors the unexported errorResponse struct defined in
// handler.go so that test assertions can decode error JSON payloads
// returned by the handler (package replay_test cannot reference the
// unexported type directly).
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Response assertion helpers
// ---------------------------------------------------------------------------

// assertErrorResponse decodes the recorder's response body as an error JSON
// payload and asserts that:
//   - The HTTP status code matches the expected code.
//   - The JSON "status" field is "error".
//   - The JSON "message" field contains the expected substring.
func assertErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, statusCode int, message string) {
	t.Helper()
	require.Equal(t, statusCode, rec.Code)
	var resp errorResponse
	err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
	require.NoError(t, err, "failed to decode error response body")
	require.Equal(t, "error", resp.Status)
	require.Contains(t, resp.Message, message)
}

// ---------------------------------------------------------------------------
// TestHandler_TriggerReplay
// ---------------------------------------------------------------------------

// TestHandler_TriggerReplay exercises POST /v1/warehouse/replay via
// table-driven subtests covering: valid requests, malformed JSON, validation
// failures for every required field, sentinel service errors (disabled,
// concurrent limit, gateway not configured), and generic internal errors.
func TestHandler_TriggerReplay(t *testing.T) {
	// Deterministic dates used across subtests for repeatable assertions.
	startTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endTime := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	// A valid request body reused (or overridden) in many subtests.
	validReq := replay.ReplayRequest{
		SourceID:      "test_source",
		DestinationID: "test_dest",
		StartTime:     startTime,
		EndTime:       endTime,
		ReplayType:    "full",
	}

	tests := []struct {
		name       string
		body       func() []byte  // returns raw request body
		mockSvc    *mockReplayService
		wantStatus int
		wantErrMsg string // non-empty ⇒ error path; empty ⇒ success path
		wantJobID  int64  // expected job ID for success path
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
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return &replay.ReplayResponse{
						JobID:  1,
						Status: replay.StatusPending,
					}, nil
				},
			},
			wantStatus: http.StatusCreated,
			wantJobID:  1,
		},

		// -----------------------------------------------------------------
		// 2. Invalid JSON body returns 400
		// -----------------------------------------------------------------
		{
			name: "invalid JSON body returns 400",
			body: func() []byte {
				return []byte(`{not valid json`)
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					t.Fatal("Trigger should not be called for invalid JSON body")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "invalid JSON request body",
		},

		// -----------------------------------------------------------------
		// 3. Missing source_id returns 400 (via ErrInvalidReplayRequest)
		// -----------------------------------------------------------------
		{
			name: "missing source_id returns 400",
			body: func() []byte {
				req := replay.ReplayRequest{
					DestinationID: "test_dest",
					StartTime:     startTime,
					EndTime:       endTime,
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return nil, fmt.Errorf("%w: source_id is required", replay.ErrInvalidReplayRequest)
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
				req := replay.ReplayRequest{
					SourceID:  "test_source",
					StartTime: startTime,
					EndTime:   endTime,
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return nil, fmt.Errorf("%w: destination_id is required", replay.ErrInvalidReplayRequest)
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "destination_id is required",
		},

		// -----------------------------------------------------------------
		// 5. Missing start_time returns 400
		// -----------------------------------------------------------------
		{
			name: "missing start_time returns 400",
			body: func() []byte {
				req := replay.ReplayRequest{
					SourceID:      "test_source",
					DestinationID: "test_dest",
					EndTime:       endTime,
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return nil, fmt.Errorf("%w: start_time is required", replay.ErrInvalidReplayRequest)
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "start_time is required",
		},

		// -----------------------------------------------------------------
		// 6. Missing end_time returns 400
		// -----------------------------------------------------------------
		{
			name: "missing end_time returns 400",
			body: func() []byte {
				req := replay.ReplayRequest{
					SourceID:      "test_source",
					DestinationID: "test_dest",
					StartTime:     startTime,
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return nil, fmt.Errorf("%w: end_time is required", replay.ErrInvalidReplayRequest)
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "end_time is required",
		},

		// -----------------------------------------------------------------
		// 7. start_time after end_time returns 400
		// -----------------------------------------------------------------
		{
			name: "start_time after end_time returns 400",
			body: func() []byte {
				req := replay.ReplayRequest{
					SourceID:      "test_source",
					DestinationID: "test_dest",
					StartTime:     endTime,   // intentionally reversed
					EndTime:       startTime, // intentionally reversed
				}
				b, _ := jsonrs.Marshal(req)
				return b
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return nil, fmt.Errorf("%w: start_time must be before end_time", replay.ErrInvalidReplayRequest)
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "start_time must be before end_time",
		},

		// -----------------------------------------------------------------
		// 8. Replay disabled returns 403
		// -----------------------------------------------------------------
		{
			name: "replay disabled returns 403",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return nil, replay.ErrReplayDisabled
				},
			},
			wantStatus: http.StatusForbidden,
			wantErrMsg: "replay feature is disabled",
		},

		// -----------------------------------------------------------------
		// 9. Concurrent limit reached returns 429
		// -----------------------------------------------------------------
		{
			name: "concurrent limit reached returns 429",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return nil, replay.ErrConcurrentLimitReached
				},
			},
			wantStatus: http.StatusTooManyRequests,
			wantErrMsg: "concurrent replay job limit reached",
		},

		// -----------------------------------------------------------------
		// 10. Generic service error returns 500
		// -----------------------------------------------------------------
		{
			name: "service internal error returns 500",
			body: func() []byte {
				b, _ := jsonrs.Marshal(validReq)
				return b
			},
			mockSvc: &mockReplayService{
				triggerFn: func(_ context.Context, _ replay.ReplayRequest) (*replay.ReplayResponse, error) {
					return nil, errors.New("unexpected internal error")
				},
			},
			wantStatus: http.StatusInternalServerError,
			wantErrMsg: "internal server error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create handler with the per-test mock service and NOP logger.
			h := replay.NewHandler(tc.mockSvc, logger.NOP)

			// Build the HTTP test request.
			body := tc.body()
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/replay", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			// Invoke the handler.
			h.TriggerReplay(rec, req)

			// Assert HTTP status code.
			require.Equal(t, tc.wantStatus, rec.Code)

			// Assert response body.
			if tc.wantErrMsg != "" {
				// Error path — use helper.
				var errResp errorResponse
				err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
				require.NoError(t, err, "failed to decode error response body")
				require.Equal(t, "error", errResp.Status)
				require.Contains(t, errResp.Message, tc.wantErrMsg)
			} else {
				// Success path — decode replay response and verify fields.
				var resp replay.ReplayResponse
				err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
				require.NoError(t, err, "failed to decode success response body")
				require.Equal(t, tc.wantJobID, resp.JobID)
				require.Equal(t, replay.StatusPending, resp.Status)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestHandler_GetReplayStatus
// ---------------------------------------------------------------------------

// TestHandler_GetReplayStatus exercises GET /v1/warehouse/replay/{jobID}
// via table-driven subtests covering: valid retrieval (pending and in-progress
// statuses), invalid job ID format, non-existent job, and generic errors.
// Uses chi.NewRouter() with registered routes to exercise the full Chi routing
// stack for path parameter extraction.
func TestHandler_GetReplayStatus(t *testing.T) {
	createdAt := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2024, 1, 1, 12, 5, 0, 0, time.UTC)

	tests := []struct {
		name       string
		jobIDParam string
		mockSvc    *mockReplayService
		wantStatus int
		wantErrMsg string
		wantJob    *replay.ReplayJob // populated for success assertions
	}{
		// -----------------------------------------------------------------
		// 1. Valid job ID returns 200 with full job details (pending)
		// -----------------------------------------------------------------
		{
			name:       "valid job ID returns 200 with job details",
			jobIDParam: "1",
			mockSvc: &mockReplayService{
				getStatusFn: func(_ context.Context, id int64) (*replay.ReplayJob, error) {
					require.Equal(t, int64(1), id)
					return &replay.ReplayJob{
						ID:            1,
						SourceID:      "test_source",
						DestinationID: "test_dest",
						StartTime:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
						EndTime:       time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
						ReplayType:    "full",
						Status:        replay.StatusPending,
						CreatedAt:     createdAt,
						UpdatedAt:     updatedAt,
					}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantJob: &replay.ReplayJob{
				ID:            1,
				SourceID:      "test_source",
				DestinationID: "test_dest",
				Status:        replay.StatusPending,
			},
		},

		// -----------------------------------------------------------------
		// 2. Valid in-progress job returns 200
		// -----------------------------------------------------------------
		{
			name:       "in-progress job returns 200",
			jobIDParam: "42",
			mockSvc: &mockReplayService{
				getStatusFn: func(_ context.Context, id int64) (*replay.ReplayJob, error) {
					require.Equal(t, int64(42), id)
					return &replay.ReplayJob{
						ID:            42,
						SourceID:      "src_42",
						DestinationID: "dst_42",
						StartTime:     time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
						EndTime:       time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC),
						ReplayType:    "full",
						Status:        replay.StatusInProgress,
						TotalEvents:   500,
						TotalBatches:  5,
						CreatedAt:     createdAt,
						UpdatedAt:     updatedAt,
					}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantJob: &replay.ReplayJob{
				ID:            42,
				SourceID:      "src_42",
				DestinationID: "dst_42",
				Status:        replay.StatusInProgress,
			},
		},

		// -----------------------------------------------------------------
		// 3. Invalid job ID format returns 400
		// -----------------------------------------------------------------
		{
			name:       "invalid job ID format returns 400",
			jobIDParam: "abc",
			mockSvc: &mockReplayService{
				getStatusFn: func(_ context.Context, _ int64) (*replay.ReplayJob, error) {
					t.Fatal("GetStatus should not be called for invalid job ID format")
					return nil, errors.New("unreachable")
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrMsg: "invalid job ID",
		},

		// -----------------------------------------------------------------
		// 4. Non-existent job ID returns 404
		// -----------------------------------------------------------------
		{
			name:       "non-existent job ID returns 404",
			jobIDParam: "999999",
			mockSvc: &mockReplayService{
				getStatusFn: func(_ context.Context, _ int64) (*replay.ReplayJob, error) {
					return nil, replay.ErrReplayJobNotFound
				},
			},
			wantStatus: http.StatusNotFound,
			wantErrMsg: "replay job not found",
		},

		// -----------------------------------------------------------------
		// 5. Generic service error returns 500
		// -----------------------------------------------------------------
		{
			name:       "generic service error returns 500",
			jobIDParam: "1",
			mockSvc: &mockReplayService{
				getStatusFn: func(_ context.Context, _ int64) (*replay.ReplayJob, error) {
					return nil, errors.New("database connection lost")
				},
			},
			wantStatus: http.StatusInternalServerError,
			wantErrMsg: "failed to get replay status",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := replay.NewHandler(tc.mockSvc, logger.NOP)

			// Register the handler on a Chi router so that chi.URLParam()
			// correctly extracts the {jobID} path parameter.
			r := chi.NewRouter()
			r.Get("/v1/warehouse/replay/{jobID}", h.GetReplayStatus)

			req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/replay/"+tc.jobIDParam, nil)
			rec := httptest.NewRecorder()

			// Serve through the Chi router to populate URL params.
			r.ServeHTTP(rec, req)

			// Assert HTTP status code.
			require.Equal(t, tc.wantStatus, rec.Code)

			if tc.wantErrMsg != "" {
				assertErrorResponse(t, rec, tc.wantStatus, tc.wantErrMsg)
			} else {
				var job replay.ReplayJob
				err := jsonrs.NewDecoder(rec.Body).Decode(&job)
				require.NoError(t, err, "failed to decode job response body")
				require.Equal(t, tc.wantJob.ID, job.ID)
				require.Equal(t, tc.wantJob.SourceID, job.SourceID)
				require.Equal(t, tc.wantJob.DestinationID, job.DestinationID)
				require.Equal(t, tc.wantJob.Status, job.Status)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestReplayRequest_Validate
// ---------------------------------------------------------------------------

// TestReplayRequest_Validate exercises the Validate method on ReplayRequest
// to verify field-level validation produces expected sentinel errors for
// every required field and the time-range constraint.
func TestReplayRequest_Validate(t *testing.T) {
	startTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endTime := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		req     replay.ReplayRequest
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid request passes validation",
			req: replay.ReplayRequest{
				SourceID:      "src1",
				DestinationID: "dst1",
				StartTime:     startTime,
				EndTime:       endTime,
			},
			wantErr: false,
		},
		{
			name: "missing source_id fails",
			req: replay.ReplayRequest{
				DestinationID: "dst1",
				StartTime:     startTime,
				EndTime:       endTime,
			},
			wantErr: true,
			errMsg:  "source_id is required",
		},
		{
			name: "missing destination_id fails",
			req: replay.ReplayRequest{
				SourceID:  "src1",
				StartTime: startTime,
				EndTime:   endTime,
			},
			wantErr: true,
			errMsg:  "destination_id is required",
		},
		{
			name: "missing start_time fails",
			req: replay.ReplayRequest{
				SourceID:      "src1",
				DestinationID: "dst1",
				EndTime:       endTime,
			},
			wantErr: true,
			errMsg:  "start_time is required",
		},
		{
			name: "missing end_time fails",
			req: replay.ReplayRequest{
				SourceID:      "src1",
				DestinationID: "dst1",
				StartTime:     startTime,
			},
			wantErr: true,
			errMsg:  "end_time is required",
		},
		{
			name: "start_time after end_time fails",
			req: replay.ReplayRequest{
				SourceID:      "src1",
				DestinationID: "dst1",
				StartTime:     endTime,
				EndTime:       startTime,
			},
			wantErr: true,
			errMsg:  "start_time must be before end_time",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, replay.ErrInvalidReplayRequest)
				require.Contains(t, err.Error(), tc.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestReplayStatus_IsActive
// ---------------------------------------------------------------------------

// TestReplayStatus_IsActive verifies the IsActive helper correctly classifies
// all replay job statuses as active or non-active.
func TestReplayStatus_IsActive(t *testing.T) {
	tests := []struct {
		status replay.ReplayStatus
		want   bool
	}{
		{replay.StatusPending, true},
		{replay.StatusInProgress, true},
		{replay.StatusCompleted, false},
		{replay.StatusFailed, false},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			require.Equal(t, tc.want, replay.IsActive(tc.status))
		})
	}
}

// ---------------------------------------------------------------------------
// TestReplayStatus_IsTerminal
// ---------------------------------------------------------------------------

// TestReplayStatus_IsTerminal verifies the IsTerminal helper correctly
// classifies all replay job statuses as terminal or non-terminal.
func TestReplayStatus_IsTerminal(t *testing.T) {
	tests := []struct {
		status replay.ReplayStatus
		want   bool
	}{
		{replay.StatusPending, false},
		{replay.StatusInProgress, false},
		{replay.StatusCompleted, true},
		{replay.StatusFailed, true},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			require.Equal(t, tc.want, replay.IsTerminal(tc.status))
		})
	}
}
