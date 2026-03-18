// Package warehouse_test provides integration tests for the warehouse backfill API (E-032).
//
// TestBackfillAPI exercises the full backfill lifecycle through the HTTP API layer:
// request validation, archiver integration, job status tracking, concurrent job limits,
// and staging file fallback resolution.
//
// Infrastructure: Each test spins up a PostgreSQL container (via dockertest + postgres.Setup),
// creates the wh_backfill_jobs table, wires the backfill handler + service + repository
// into a Chi HTTP server on a random port, and validates end-to-end HTTP request/response
// behavior.
//
// CRITICAL: All JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs
// exclusively — encoding/json is prohibited by .golangci.yml depguard rules.
package warehouse_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ory/dockertest/v3"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	kithelper "github.com/rudderlabs/rudder-go-kit/testhelper"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/minio"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"

	"github.com/rudderlabs/rudder-server/warehouse/backfill"
	sqlmw "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
)

// ---------------------------------------------------------------------------
// DTO structs for test request/response payloads
// ---------------------------------------------------------------------------

// backfillRequestPayload is the HTTP request body for POST /v1/warehouse/backfill.
// Field names use JSON tags consistent with the backfill.BackfillRequest model.
type backfillRequestPayload struct {
	SourceID      string `json:"source_id"`
	DestinationID string `json:"destination_id"`
	WorkspaceID   string `json:"workspace_id,omitempty"`
	StartDate     string `json:"start_date,omitempty"`
	EndDate       string `json:"end_date,omitempty"`
}

// backfillResponsePayload mirrors the backfill.BackfillResponse for JSON deserialization.
type backfillResponsePayload struct {
	JobID  int64  `json:"jobID"`
	Status string `json:"status"`
}

// backfillErrorResponse mirrors the handler's errorResponse for JSON deserialization.
type backfillErrorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Mock implementations for ArchiverQuerier and StagingFileQuerier
// ---------------------------------------------------------------------------

// mockArchiverQuerier implements backfill.ArchiverQuerier for integration testing.
// It records calls and returns configurable results, enabling assertion of archiver
// integration behavior in test cases 8 and 9.
type mockArchiverQuerier struct {
	mu       sync.Mutex
	calls    []archiverCall
	resultFn func(ctx context.Context, sourceID, destID string, startDate, endDate time.Time) ([]int64, error)
}

type archiverCall struct {
	SourceID string
	DestID   string
	Start    time.Time
	End      time.Time
}

func (m *mockArchiverQuerier) ListArchivedStagingFiles(
	ctx context.Context, sourceID, destID string, startDate, endDate time.Time,
) ([]int64, error) {
	m.mu.Lock()
	m.calls = append(m.calls, archiverCall{
		SourceID: sourceID,
		DestID:   destID,
		Start:    startDate,
		End:      endDate,
	})
	m.mu.Unlock()
	if m.resultFn != nil {
		return m.resultFn(ctx, sourceID, destID, startDate, endDate)
	}
	return nil, nil
}

func (m *mockArchiverQuerier) getCalls() []archiverCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]archiverCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// mockStagingFileQuerier implements backfill.StagingFileQuerier for integration testing.
// It records calls and returns configurable results for staging file fallback tests.
type mockStagingFileQuerier struct {
	mu       sync.Mutex
	calls    []stagingCall
	resultFn func(ctx context.Context, sourceID, destID string, startDate, endDate time.Time) ([]int64, error)
}

type stagingCall struct {
	SourceID string
	DestID   string
	Start    time.Time
	End      time.Time
}

func (m *mockStagingFileQuerier) GetByDateRange(
	ctx context.Context, sourceID, destID string, startDate, endDate time.Time,
) ([]int64, error) {
	m.mu.Lock()
	m.calls = append(m.calls, stagingCall{
		SourceID: sourceID,
		DestID:   destID,
		Start:    startDate,
		End:      endDate,
	})
	m.mu.Unlock()
	if m.resultFn != nil {
		return m.resultFn(ctx, sourceID, destID, startDate, endDate)
	}
	return nil, nil
}

func (m *mockStagingFileQuerier) getCalls() []stagingCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]stagingCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// ---------------------------------------------------------------------------
// Test infrastructure: server setup and helpers
// ---------------------------------------------------------------------------

// backfillTestEnv holds all resources for a running backfill test server.
type backfillTestEnv struct {
	serverURL      string
	db             *sqlmw.DB
	conf           *config.Config
	minioResource  *minio.Resource
	archiver       *mockArchiverQuerier
	stagingQuerier *mockStagingFileQuerier
	service        *backfill.BackfillService
	cancel         context.CancelFunc
}

// setupBackfillTestServer spins up a PostgreSQL container, creates the
// wh_backfill_jobs table, wires the backfill handler + service + repository
// into a Chi HTTP server on a random port, and returns a backfillTestEnv.
//
// The server is automatically torn down when the test finishes via t.Cleanup.
// The monitor goroutine (service.Run) is started so that async archiver/staging
// integration tests can observe status transitions.
func setupBackfillTestServer(
	t *testing.T,
	configOverrides ...lo.Tuple2[string, any],
) *backfillTestEnv {
	t.Helper()

	// 1. Docker pool + PostgreSQL container
	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	pgResource, err := postgres.Setup(pool, t)
	require.NoError(t, err)

	// 1b. MinIO container (S3-compatible object storage for staging file tests).
	minioResource, err := minio.Setup(pool, t)
	require.NoError(t, err, "failed to setup minio container")

	// 2. Create wh_backfill_jobs table (mirrors 000042 migration).
	//    We also need the wh_uploads table stub so the FK is valid.
	_, err = pgResource.DB.Exec(createBackfillTablesSQL)
	require.NoError(t, err, "failed to create wh_backfill_jobs table")

	db := sqlmw.New(pgResource.DB)

	// 3. Configuration
	conf := config.New()
	// Enable backfill feature for tests by default.
	conf.Set(backfill.ConfigKeyEnabled, true)
	// Speed up the monitor loop for integration tests.
	conf.Set(backfill.ConfigKeyMonitorInterval, 1)
	// Apply caller-provided overrides.
	for _, override := range configOverrides {
		conf.Set(override.A, override.B)
	}

	// 4. Mock external dependencies
	archMock := &mockArchiverQuerier{}
	stagingMock := &mockStagingFileQuerier{}

	// 5. Real repository + service + handler
	repo := backfill.NewRepository(db)
	svc := backfill.NewBackfillService(
		conf,
		logger.NewLogger(),
		stats.NOP,
		repo,
		archMock,
		stagingMock,
	)
	handler := backfill.NewHandler(logger.NewLogger(), svc)

	// 6. Chi router matching the API pattern from warehouse/api/http.go.
	mux := chi.NewRouter()
	mux.Post("/v1/warehouse/backfill", handler.TriggerBackfill)
	mux.Get("/v1/warehouse/backfill/{jobID}", handler.GetBackfillStatus)

	// 7. Start HTTP server on a random port.
	port, err := kithelper.GetFreePort()
	require.NoError(t, err)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start the monitor loop so that async job processing triggers.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = svc.Run(ctx) }()

	go func() {
		if srvErr := srv.ListenAndServe(); srvErr != nil && srvErr != http.ErrServerClosed {
			t.Logf("backfill test server exited: %v", srvErr)
		}
	}()

	// Wait for the server to be reachable.
	serverURL := fmt.Sprintf("http://localhost:%d", port)
	require.Eventuallyf(t, func() bool {
		resp, httpErr := http.Get(serverURL + "/v1/warehouse/backfill/0")
		if httpErr != nil {
			return false
		}
		_ = resp.Body.Close()
		// Any non-connection-refused response means the server is up.
		return true
	}, 10*time.Second, 100*time.Millisecond, "backfill test server did not start in time")

	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})

	return &backfillTestEnv{
		serverURL:      serverURL,
		db:             db,
		conf:           conf,
		minioResource:  minioResource,
		archiver:       archMock,
		stagingQuerier: stagingMock,
		service:        svc,
		cancel:         cancel,
	}
}

// createBackfillRequest is a helper that POSTs a JSON-encoded backfill request
// to the test server and returns the HTTP status code and body bytes.
// It uses jsonrs.Marshal per the CRITICAL depguard rule.
func createBackfillRequest(t *testing.T, serverURL string, payload backfillRequestPayload) (int, []byte) {
	t.Helper()

	body, err := jsonrs.Marshal(payload)
	require.NoError(t, err, "jsonrs.Marshal of backfill request failed")

	resp, err := http.Post(
		serverURL+"/v1/warehouse/backfill",
		"application/json",
		bytes.NewReader(body),
	)
	require.NoError(t, err, "HTTP POST to backfill endpoint failed")

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "reading backfill response body failed")
	_ = resp.Body.Close()

	return resp.StatusCode, respBody
}

// getBackfillStatus is a helper that GETs the status of a backfill job by ID.
// It uses http.NewRequest with http.MethodGet for explicit method specification,
// consistent with the warehouse API testing pattern.
func getBackfillStatus(t *testing.T, serverURL string, jobID int64) (int, []byte) {
	t.Helper()

	url := fmt.Sprintf("%s/v1/warehouse/backfill/%d", serverURL, jobID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err, "failed to create GET request for backfill status")

	client := &http.Client{Timeout: time.Duration(10) * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err, "HTTP GET to backfill status endpoint failed")

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "reading backfill status response body failed")
	_ = resp.Body.Close()

	return resp.StatusCode, respBody
}

// createBackfillTablesSQL contains the DDL for the wh_backfill_jobs table
// and a minimal wh_uploads stub with the backfill_job_id FK column.
// This mirrors the 000042_add_backfill_tracking migration without requiring
// the full migration runner or all previous migrations.
const createBackfillTablesSQL = `
CREATE TABLE IF NOT EXISTS wh_uploads (
    id BIGSERIAL PRIMARY KEY,
    source_id VARCHAR(64),
    destination_id VARCHAR(64),
    namespace VARCHAR(64),
    status VARCHAR(64),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS wh_backfill_jobs (
    id BIGSERIAL PRIMARY KEY,
    source_id VARCHAR(64) NOT NULL,
    destination_id VARCHAR(64) NOT NULL,
    workspace_id VARCHAR(64) NOT NULL,
    start_date TIMESTAMPTZ NOT NULL,
    end_date TIMESTAMPTZ NOT NULL,
    status VARCHAR(64) NOT NULL DEFAULT 'Pending',
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS wh_backfill_jobs_source_dest_status_idx
    ON wh_backfill_jobs (source_id, destination_id, status);

ALTER TABLE wh_uploads ADD COLUMN IF NOT EXISTS backfill_job_id BIGINT REFERENCES wh_backfill_jobs(id);
`

// ---------------------------------------------------------------------------
// TestBackfillAPI — Main integration test function
// ---------------------------------------------------------------------------

// TestBackfillAPI exercises the warehouse backfill API through 9 table-driven
// subtests covering: valid requests, validation errors, feature gating,
// concurrent job limits, status tracking, archiver integration, and staging
// file fallback.
func TestBackfillAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping backfill integration test in short mode")
	}

	// -------------------------------------------------------------------
	// Test Case 1: valid_backfill_request
	// -------------------------------------------------------------------
	t.Run("valid_backfill_request", func(t *testing.T) {
		env := setupBackfillTestServer(t)

		now := time.Now().UTC()
		startDate := now.AddDate(0, 0, -5).Truncate(time.Second)
		endDate := now.Truncate(time.Second)

		statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
			SourceID:      sourceID,
			DestinationID: destinationID,
			WorkspaceID:   workspaceID,
			StartDate:     startDate.Format(time.RFC3339),
			EndDate:       endDate.Format(time.RFC3339),
		})

		require.Equal(t, http.StatusCreated, statusCode,
			"expected 201 Created, got %d; body: %s", statusCode, string(body))

		var result backfillResponsePayload
		require.NoError(t, jsonrs.Unmarshal(body, &result),
			"failed to unmarshal backfill response: %s", string(body))
		require.Greater(t, result.JobID, int64(0), "jobID must be positive")
		require.Equal(t, backfill.StatusPending, result.Status,
			"new job status must be %s", backfill.StatusPending)

		// Verify the record exists in the database.
		var dbStatus string
		err := env.db.QueryRow(
			"SELECT status FROM wh_backfill_jobs WHERE id = $1", result.JobID,
		).Scan(&dbStatus)
		require.NoError(t, err, "expected job record in wh_backfill_jobs")
		require.Equal(t, backfill.StatusPending, dbStatus)
	})

	// -------------------------------------------------------------------
	// Test Case 2: invalid_date_range_start_after_end
	// -------------------------------------------------------------------
	t.Run("invalid_date_range_start_after_end", func(t *testing.T) {
		env := setupBackfillTestServer(t)

		now := time.Now().UTC()
		startDate := now.Truncate(time.Second)
		endDate := now.AddDate(0, 0, -5).Truncate(time.Second)

		statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
			SourceID:      sourceID,
			DestinationID: destinationID,
			StartDate:     startDate.Format(time.RFC3339),
			EndDate:       endDate.Format(time.RFC3339),
		})

		require.Equal(t, http.StatusBadRequest, statusCode,
			"expected 400 Bad Request for reversed date range; body: %s", string(body))

		var errResp backfillErrorResponse
		require.NoError(t, jsonrs.Unmarshal(body, &errResp))
		require.Contains(t, errResp.Message, "start_date must be before end_date",
			"error message should indicate invalid date ordering")

		// Verify the ErrInvalidDateRange sentinel error text is consistent
		// with the error hierarchy used by the service layer.
		require.Equal(t, "invalid date range", backfill.ErrInvalidDateRange.Error(),
			"ErrInvalidDateRange sentinel should have expected text")
	})

	// -------------------------------------------------------------------
	// Test Case 3: date_range_exceeds_max_days
	// -------------------------------------------------------------------
	t.Run("date_range_exceeds_max_days", func(t *testing.T) {
		env := setupBackfillTestServer(t,
			lo.Tuple2[string, any]{A: backfill.ConfigKeyMaxDateRangeDays, B: 30},
		)

		now := time.Now().UTC()
		startDate := now.AddDate(0, 0, -60).Truncate(time.Second)
		endDate := now.Truncate(time.Second)

		statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
			SourceID:      sourceID,
			DestinationID: destinationID,
			StartDate:     startDate.Format(time.RFC3339),
			EndDate:       endDate.Format(time.RFC3339),
		})

		require.Equal(t, http.StatusBadRequest, statusCode,
			"expected 400 for date range exceeding max; body: %s", string(body))

		var errResp backfillErrorResponse
		require.NoError(t, jsonrs.Unmarshal(body, &errResp))
		require.Contains(t, errResp.Message, backfill.ErrDateRangeExceedsMax.Error(),
			"error message should reference date range exceeding max")
	})

	// -------------------------------------------------------------------
	// Test Case 4: missing_required_fields
	// -------------------------------------------------------------------
	t.Run("missing_required_fields", func(t *testing.T) {
		env := setupBackfillTestServer(t)

		now := time.Now().UTC()
		startDate := now.AddDate(0, 0, -5).Truncate(time.Second).Format(time.RFC3339)
		endDate := now.Truncate(time.Second).Format(time.RFC3339)

		subtests := []struct {
			name    string
			payload backfillRequestPayload
			errMsg  string
		}{
			{
				name: "missing_source_id",
				payload: backfillRequestPayload{
					SourceID:      "",
					DestinationID: destinationID,
					StartDate:     startDate,
					EndDate:       endDate,
				},
				errMsg: "source_id is required",
			},
			{
				name: "missing_destination_id",
				payload: backfillRequestPayload{
					SourceID:      sourceID,
					DestinationID: "",
					StartDate:     startDate,
					EndDate:       endDate,
				},
				errMsg: "destination_id is required",
			},
			{
				name: "missing_start_date",
				payload: backfillRequestPayload{
					SourceID:      sourceID,
					DestinationID: destinationID,
					StartDate:     "",
					EndDate:       endDate,
				},
				errMsg: "start_date is required",
			},
			{
				name: "missing_end_date",
				payload: backfillRequestPayload{
					SourceID:      sourceID,
					DestinationID: destinationID,
					StartDate:     startDate,
					EndDate:       "",
				},
				errMsg: "end_date is required",
			},
		}

		for _, tc := range subtests {
			t.Run(tc.name, func(t *testing.T) {
				statusCode, body := createBackfillRequest(t, env.serverURL, tc.payload)
				require.Equal(t, http.StatusBadRequest, statusCode,
					"expected 400 for %s; body: %s", tc.name, string(body))

				var errResp backfillErrorResponse
				require.NoError(t, jsonrs.Unmarshal(body, &errResp))
				require.Contains(t, errResp.Message, tc.errMsg,
					"error message for %s should contain %q", tc.name, tc.errMsg)
			})
		}
	})

	// -------------------------------------------------------------------
	// Test Case 5: backfill_disabled
	// -------------------------------------------------------------------
	t.Run("backfill_disabled", func(t *testing.T) {
		env := setupBackfillTestServer(t,
			lo.Tuple2[string, any]{A: backfill.ConfigKeyEnabled, B: false},
		)

		// Schema compliance: verify config.ConfigKeyToEnv + config.DefaultEnvPrefix.
		envKey := config.ConfigKeyToEnv(config.DefaultEnvPrefix, backfill.ConfigKeyEnabled)
		require.Contains(t, envKey, "RSERVER_",
			"config key should convert to RSERVER_ prefixed env var")

		now := time.Now().UTC()
		startDate := now.AddDate(0, 0, -3).Truncate(time.Second)
		endDate := now.Truncate(time.Second)

		statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
			SourceID:      sourceID,
			DestinationID: destinationID,
			StartDate:     startDate.Format(time.RFC3339),
			EndDate:       endDate.Format(time.RFC3339),
		})

		require.Equal(t, http.StatusForbidden, statusCode,
			"expected 403 Forbidden when backfill is disabled; body: %s", string(body))

		var errResp backfillErrorResponse
		require.NoError(t, jsonrs.Unmarshal(body, &errResp))
		require.Contains(t, errResp.Message, backfill.ErrBackfillDisabled.Error(),
			"error message should reference backfill disabled")
	})

	// -------------------------------------------------------------------
	// Test Case 6: backfill_job_status_tracking
	// -------------------------------------------------------------------
	t.Run("backfill_job_status_tracking", func(t *testing.T) {
		env := setupBackfillTestServer(t)

		now := time.Now().UTC()
		startDate := now.AddDate(0, 0, -3).Truncate(time.Second)
		endDate := now.Truncate(time.Second)

		statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
			SourceID:      sourceID,
			DestinationID: destinationID,
			WorkspaceID:   workspaceID,
			StartDate:     startDate.Format(time.RFC3339),
			EndDate:       endDate.Format(time.RFC3339),
		})
		require.Equal(t, http.StatusCreated, statusCode,
			"setup: create job failed; body: %s", string(body))

		var createResp backfillResponsePayload
		require.NoError(t, jsonrs.Unmarshal(body, &createResp))
		require.Greater(t, createResp.JobID, int64(0))

		// GET the job status — initial Pending.
		statusCode2, statusBody := getBackfillStatus(t, env.serverURL, createResp.JobID)
		require.Equal(t, http.StatusOK, statusCode2,
			"expected 200 OK for job status; body: %s", string(statusBody))

		var jobRecord map[string]any
		require.NoError(t, jsonrs.Unmarshal(statusBody, &jobRecord))
		require.Equal(t, float64(createResp.JobID), jobRecord["id"])
		require.Equal(t, backfill.StatusPending, jobRecord["status"])

		// Non-existent job -> 404.
		nfStatusCode, notFoundBody := getBackfillStatus(t, env.serverURL, 999999)
		require.Equal(t, http.StatusNotFound, nfStatusCode,
			"expected 404 for non-existent job; body: %s", string(notFoundBody))

		// Simulate transition -> InProgress via DB.
		_, dbErr := env.db.Exec(
			"UPDATE wh_backfill_jobs SET status = $1, updated_at = NOW() WHERE id = $2",
			backfill.StatusInProgress, createResp.JobID,
		)
		require.NoError(t, dbErr)

		statusCode3, statusBody2 := getBackfillStatus(t, env.serverURL, createResp.JobID)
		require.Equal(t, http.StatusOK, statusCode3)
		var jobRecord2 map[string]any
		require.NoError(t, jsonrs.Unmarshal(statusBody2, &jobRecord2))
		require.Equal(t, backfill.StatusInProgress, jobRecord2["status"])

		// Transition -> Completed.
		_, dbErr = env.db.Exec(
			"UPDATE wh_backfill_jobs SET status = $1, updated_at = NOW() WHERE id = $2",
			backfill.StatusCompleted, createResp.JobID,
		)
		require.NoError(t, dbErr)

		statusCode4, statusBody3 := getBackfillStatus(t, env.serverURL, createResp.JobID)
		require.Equal(t, http.StatusOK, statusCode4)
		var jobRecord3 map[string]any
		require.NoError(t, jsonrs.Unmarshal(statusBody3, &jobRecord3))
		require.Equal(t, backfill.StatusCompleted, jobRecord3["status"])

		// Transition -> Failed.
		_, dbErr = env.db.Exec(
			"UPDATE wh_backfill_jobs SET status = $1, updated_at = NOW() WHERE id = $2",
			backfill.StatusFailed, createResp.JobID,
		)
		require.NoError(t, dbErr)

		statusCode5, statusBody4 := getBackfillStatus(t, env.serverURL, createResp.JobID)
		require.Equal(t, http.StatusOK, statusCode5)
		var jobRecord4 map[string]any
		require.NoError(t, jsonrs.Unmarshal(statusBody4, &jobRecord4))
		require.Equal(t, backfill.StatusFailed, jobRecord4["status"])

		// Verify sql.ErrNoRows for non-existent job.
		var dummy string
		scanErr := env.db.QueryRow(
			"SELECT status FROM wh_backfill_jobs WHERE id = $1", 999999,
		).Scan(&dummy)
		require.ErrorIs(t, scanErr, sql.ErrNoRows,
			"scanning non-existent job should return sql.ErrNoRows")
	})

	// -------------------------------------------------------------------
	// Test Case 7: concurrent_job_limits
	// -------------------------------------------------------------------
	t.Run("concurrent_job_limits", func(t *testing.T) {
		maxJobs := 2
		env := setupBackfillTestServer(t,
			lo.Tuple2[string, any]{A: backfill.ConfigKeyMaxConcurrentJobs, B: maxJobs},
		)

		now := time.Now().UTC()
		startDate := now.AddDate(0, 0, -3).Truncate(time.Second)
		endDate := now.Truncate(time.Second)

		// Create maxJobs jobs — all should succeed.
		for i := 0; i < maxJobs; i++ {
			statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
				SourceID:      fmt.Sprintf("%s_%d", sourceID, i),
				DestinationID: fmt.Sprintf("%s_%d", destinationID, i),
				WorkspaceID:   workspaceID,
				StartDate:     startDate.Format(time.RFC3339),
				EndDate:       endDate.Format(time.RFC3339),
			})
			require.Equal(t, http.StatusCreated, statusCode,
				"job %d/%d should be created; body: %s", i+1, maxJobs, string(body))
		}

		// The (maxJobs+1)th job should be rejected.
		statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
			SourceID:      sourceID,
			DestinationID: destinationID,
			WorkspaceID:   workspaceID,
			StartDate:     startDate.Format(time.RFC3339),
			EndDate:       endDate.Format(time.RFC3339),
		})
		require.Equal(t, http.StatusTooManyRequests, statusCode,
			"excess job should be rejected with 429; body: %s", string(body))

		var errResp backfillErrorResponse
		require.NoError(t, jsonrs.Unmarshal(body, &errResp))
		require.Contains(t, errResp.Message, backfill.ErrConcurrentLimitReached.Error())

		// Verify active job count in DB.
		var activeCount int
		scanErr := env.db.QueryRow(
			"SELECT COUNT(*) FROM wh_backfill_jobs WHERE status IN ($1, $2)",
			backfill.StatusPending, backfill.StatusInProgress,
		).Scan(&activeCount)
		require.NoError(t, scanErr)
		require.Equal(t, maxJobs, activeCount)
	})

	// -------------------------------------------------------------------
	// Test Case 8: archiver_integration
	// -------------------------------------------------------------------
	t.Run("archiver_integration", func(t *testing.T) {
		env := setupBackfillTestServer(t)

		env.archiver.resultFn = func(
			_ context.Context, _, _ string, _, _ time.Time,
		) ([]int64, error) {
			return []int64{101, 102, 103}, nil
		}

		now := time.Now().UTC()
		retentionDays := backfill.ArchiverRetentionWindowDays
		startDate := now.AddDate(0, 0, -(retentionDays - 2)).Truncate(time.Second)
		endDate := now.AddDate(0, 0, -1).Truncate(time.Second)

		statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
			SourceID:      sourceID,
			DestinationID: destinationID,
			WorkspaceID:   workspaceID,
			StartDate:     startDate.Format(time.RFC3339),
			EndDate:       endDate.Format(time.RFC3339),
		})
		require.Equal(t, http.StatusCreated, statusCode,
			"archiver integration backfill should succeed; body: %s", string(body))

		var result backfillResponsePayload
		require.NoError(t, jsonrs.Unmarshal(body, &result))
		require.Greater(t, result.JobID, int64(0))

		// Wait for the background monitor to invoke the archiver.
		require.Eventuallyf(t, func() bool {
			return len(env.archiver.getCalls()) > 0
		}, 15*time.Second, 200*time.Millisecond,
			"archiver.ListArchivedStagingFiles should be called by the monitor loop")

		calls := env.archiver.getCalls()
		require.Greater(t, len(calls), 0)
		lastCall := calls[len(calls)-1]
		require.Equal(t, sourceID, lastCall.SourceID)
		require.Equal(t, destinationID, lastCall.DestID)
	})

	// -------------------------------------------------------------------
	// Test Case 9: staging_file_source_resolution
	// -------------------------------------------------------------------
	t.Run("staging_file_source_resolution", func(t *testing.T) {
		env := setupBackfillTestServer(t)

		// Archiver returns empty (no files within retention window).
		env.archiver.resultFn = func(
			_ context.Context, _, _ string, _, _ time.Time,
		) ([]int64, error) {
			return nil, nil
		}

		// Staging querier returns IDs (fallback path).
		env.stagingQuerier.resultFn = func(
			_ context.Context, _, _ string, _, _ time.Time,
		) ([]int64, error) {
			return []int64{201, 202}, nil
		}

		now := time.Now().UTC()
		retentionDays := backfill.ArchiverRetentionWindowDays
		startDate := now.AddDate(0, 0, -(retentionDays + 30)).Truncate(time.Second)
		endDate := now.AddDate(0, 0, -(retentionDays + 10)).Truncate(time.Second)

		statusCode, body := createBackfillRequest(t, env.serverURL, backfillRequestPayload{
			SourceID:      sourceID,
			DestinationID: destinationID,
			WorkspaceID:   workspaceID,
			StartDate:     startDate.Format(time.RFC3339),
			EndDate:       endDate.Format(time.RFC3339),
		})
		require.Equal(t, http.StatusCreated, statusCode,
			"staging file fallback should succeed; body: %s", string(body))

		var result backfillResponsePayload
		require.NoError(t, jsonrs.Unmarshal(body, &result))
		require.Greater(t, result.JobID, int64(0))

		// Wait for monitor to invoke staging querier.
		require.Eventuallyf(t, func() bool {
			return len(env.stagingQuerier.getCalls()) > 0
		}, 15*time.Second, 200*time.Millisecond,
			"stagingQuerier.GetByDateRange should be called for dates beyond retention")

		calls := env.stagingQuerier.getCalls()
		require.Greater(t, len(calls), 0)
		lastCall := calls[len(calls)-1]
		require.Equal(t, sourceID, lastCall.SourceID)
		require.Equal(t, destinationID, lastCall.DestID)
	})
}
