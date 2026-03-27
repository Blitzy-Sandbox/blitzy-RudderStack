package healthmonitor

import (
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
)

// ---------------------------------------------------------------------------
// Mock / Stub implementation of HealthRepository for handler-level unit tests.
// Every field is configurable to drive specific test scenarios without a real
// database connection. This follows the test-stub pattern observed in
// warehouse/router/upload_stats_test.go.
// ---------------------------------------------------------------------------

// mockHealthRepo is a configurable stub that satisfies the HealthRepository
// interface declared in handler.go. Each getter field pair (value + error)
// controls the return of the corresponding repository method, enabling
// isolated tests for every handler code path.
type mockHealthRepo struct {
	// GetHealthSummary return values
	healthSummary    *HealthSummaryResponse
	healthSummaryErr error

	// GetHealthBySourceDest return values
	sourceHealth    *SourceHealthResponse
	sourceHealthErr error

	// GetHealthByUpload return values
	syncHealth    *SyncHealth
	syncHealthErr error

	// PurgeOldRecords return values
	purgeCount int64
	purgeErr   error

	// RecordSyncHealth return value
	recordErr error
}

// RecordSyncHealth implements HealthRepository.
func (m *mockHealthRepo) RecordSyncHealth(_ context.Context, _ *SyncHealth) error {
	return m.recordErr
}

// GetHealthSummary implements HealthRepository.
func (m *mockHealthRepo) GetHealthSummary(_ context.Context) (*HealthSummaryResponse, error) {
	return m.healthSummary, m.healthSummaryErr
}

// GetHealthBySourceDest implements HealthRepository.
func (m *mockHealthRepo) GetHealthBySourceDest(_ context.Context, _, _ string) (*SourceHealthResponse, error) {
	return m.sourceHealth, m.sourceHealthErr
}

// GetHealthByUpload implements HealthRepository.
func (m *mockHealthRepo) GetHealthByUpload(_ context.Context, _ int64) (*SyncHealth, error) {
	return m.syncHealth, m.syncHealthErr
}

// PurgeOldRecords implements HealthRepository.
func (m *mockHealthRepo) PurgeOldRecords(_ context.Context, _ time.Time) (int64, error) {
	return m.purgeCount, m.purgeErr
}

// ---------------------------------------------------------------------------
// TestHealthHandler_GetHealthSummary validates the GET /v1/warehouse/health
// endpoint for three scenarios: a successful response with multiple sources,
// an empty state where no health data exists, and a repository error.
// ---------------------------------------------------------------------------

func TestHealthHandler_GetHealthSummary(t *testing.T) {
	t.Run("successful health summary", func(t *testing.T) {
		repo := &mockHealthRepo{
			healthSummary: &HealthSummaryResponse{
				Sources: []*SourceHealth{
					{
						SourceID:   "source-1",
						SourceType: "web",
						Destinations: []*DestinationHealth{
							{
								DestID:   "dest-1",
								DestType: "SNOWFLAKE",
								SyncDuration: DurationStats{
									Min: 100,
									Max: 5000,
									Avg: 2500,
									P95: 4500,
								},
								RowsSynced:    12345,
								ErrorRate:     0.02,
								ErrorCount:    1,
								ErrorCategory: "permission_error",
								LastSync:      "2024-01-15T10:30:00Z",
								SchemaChanges: 3,
							},
						},
					},
					{
						SourceID:   "source-2",
						SourceType: "android",
						Destinations: []*DestinationHealth{
							{
								DestID:   "dest-2",
								DestType: "BQ",
								SyncDuration: DurationStats{
									Min: 200,
									Max: 3000,
									Avg: 1500,
									P95: 2800,
								},
								RowsSynced: 67890,
								ErrorRate:  0.0,
								ErrorCount: 0,
								LastSync:   "2024-01-15T11:00:00Z",
							},
						},
					},
				},
			},
		}

		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health", nil)
		rec := httptest.NewRecorder()

		handler.GetHealthSummary(rec, req)

		// Verify HTTP 200 and Content-Type header.
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		// Parse the JSON body with jsonrs (never encoding/json).
		var resp HealthSummaryResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)

		// Validate the top-level sources array.
		require.NotNil(t, resp.Sources)
		require.Len(t, resp.Sources, 2)

		// Validate the first source and its destination.
		src1 := resp.Sources[0]
		require.Equal(t, "source-1", src1.SourceID)
		require.Equal(t, "web", src1.SourceType)
		require.Len(t, src1.Destinations, 1)

		dest1 := src1.Destinations[0]
		require.Equal(t, "dest-1", dest1.DestID)
		require.Equal(t, "SNOWFLAKE", dest1.DestType)
		require.Equal(t, int64(12345), dest1.RowsSynced)
		require.Equal(t, 0.02, dest1.ErrorRate)
		require.Equal(t, int64(1), dest1.ErrorCount)
		require.Equal(t, "permission_error", dest1.ErrorCategory)
		require.Equal(t, "2024-01-15T10:30:00Z", dest1.LastSync)
		require.Equal(t, int64(3), dest1.SchemaChanges)

		// Validate duration stats for the first destination.
		require.Equal(t, int64(100), dest1.SyncDuration.Min)
		require.Equal(t, int64(5000), dest1.SyncDuration.Max)
		require.Equal(t, int64(2500), dest1.SyncDuration.Avg)
		require.Equal(t, int64(4500), dest1.SyncDuration.P95)

		// Validate the second source.
		src2 := resp.Sources[1]
		require.Equal(t, "source-2", src2.SourceID)
		require.Equal(t, "android", src2.SourceType)
		require.Len(t, src2.Destinations, 1)
		require.Equal(t, "dest-2", src2.Destinations[0].DestID)
		require.Equal(t, int64(67890), src2.Destinations[0].RowsSynced)
	})

	t.Run("empty state", func(t *testing.T) {
		// When no health data exists, the repository returns a valid
		// HealthSummaryResponse with nil Sources. The handler converts
		// this to an empty slice so JSON serialization produces []
		// instead of null.
		repo := &mockHealthRepo{
			healthSummary: &HealthSummaryResponse{
				Sources: nil,
			},
		}

		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health", nil)
		rec := httptest.NewRecorder()

		handler.GetHealthSummary(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var resp HealthSummaryResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)

		// Handler must convert nil → empty slice, producing {"sources":[]}.
		require.NotNil(t, resp.Sources)
		require.Len(t, resp.Sources, 0)
	})

	t.Run("internal error", func(t *testing.T) {
		repo := &mockHealthRepo{
			healthSummaryErr: errors.New("database connection failed"),
		}

		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health", nil)
		rec := httptest.NewRecorder()

		handler.GetHealthSummary(rec, req)

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var errResp map[string]string
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp["status"])
		require.Contains(t, errResp["message"], "failed to retrieve health summary")
	})
}

// ---------------------------------------------------------------------------
// TestHealthHandler_GetHealthBySourceDest validates the
// GET /v1/warehouse/health/{sourceID}/{destID} endpoint for three scenarios:
// a valid source-destination pair returning health data, an unknown source
// returning an empty result, and missing URL parameters returning HTTP 400.
// ---------------------------------------------------------------------------

func TestHealthHandler_GetHealthBySourceDest(t *testing.T) {
	t.Run("valid sourceID and destID", func(t *testing.T) {
		repo := &mockHealthRepo{
			sourceHealth: &SourceHealthResponse{
				SourceID:   "source-1",
				SourceType: "web",
				Destinations: []*DestinationHealth{
					{
						DestID:   "dest-1",
						DestType: "SNOWFLAKE",
						SyncDuration: DurationStats{
							Min: 100,
							Max: 5000,
							Avg: 2500,
							P95: 4500,
						},
						RowsSynced:    12345,
						ErrorRate:     0.02,
						ErrorCount:    1,
						ErrorCategory: "permission_error",
						LastSync:      "2024-01-15T10:30:00Z",
						SchemaChanges: 2,
					},
				},
			},
		}

		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health/source-1/dest-1", nil)

		// Inject chi route context with URL parameters so chi.URLParam()
		// can extract sourceID and destID inside the handler.
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("sourceID", "source-1")
		rctx.URLParams.Add("destID", "dest-1")
		req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))

		rec := httptest.NewRecorder()
		handler.GetHealthBySourceDest(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var resp SourceHealthResponse
		err := jsonrs.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)

		require.Equal(t, "source-1", resp.SourceID)
		require.Equal(t, "web", resp.SourceType)
		require.NotNil(t, resp.Destinations)
		require.Len(t, resp.Destinations, 1)

		dest := resp.Destinations[0]
		require.Equal(t, "dest-1", dest.DestID)
		require.Equal(t, "SNOWFLAKE", dest.DestType)
		require.Equal(t, int64(12345), dest.RowsSynced)
		require.Equal(t, 0.02, dest.ErrorRate)
		require.Equal(t, int64(1), dest.ErrorCount)
		require.Equal(t, "2024-01-15T10:30:00Z", dest.LastSync)
		require.Equal(t, int64(2), dest.SchemaChanges)
	})

	t.Run("unknown sourceID", func(t *testing.T) {
		// When the repository returns nil for an unknown source/dest pair
		// (no error), the handler responds with HTTP 200 and an empty
		// SourceHealthResponse with an empty Destinations slice (not null),
		// ensuring consistent JSON array responses for dashboard consumption.
		repo := &mockHealthRepo{
			sourceHealth:    nil,
			sourceHealthErr: nil,
		}

		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health/unknown-source/dest-1", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("sourceID", "unknown-source")
		rctx.URLParams.Add("destID", "dest-1")
		req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))

		rec := httptest.NewRecorder()
		handler.GetHealthBySourceDest(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		// The response body should contain the sourceID and an empty destinations
		// array, matching GetHealthSummary's empty-array convention.
		require.Contains(t, rec.Body.String(), "unknown-source")
		require.Contains(t, rec.Body.String(), "destinations")
	})

	t.Run("missing parameters", func(t *testing.T) {
		repo := &mockHealthRepo{}
		handler := NewHealthHandler(logger.NOP, repo)

		// Inject a chi route context without any URL parameters.
		// chi.URLParam will return "" for both sourceID and destID.
		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health//", nil)
		rctx := chi.NewRouteContext()
		req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))

		rec := httptest.NewRecorder()
		handler.GetHealthBySourceDest(rec, req)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var errResp map[string]string
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp["status"])
		require.Contains(t, errResp["message"], "sourceID and destID are required")
	})

	t.Run("missing destID only", func(t *testing.T) {
		repo := &mockHealthRepo{}
		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health/source-1/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("sourceID", "source-1")
		// destID intentionally omitted → chi.URLParam returns ""
		req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))

		rec := httptest.NewRecorder()
		handler.GetHealthBySourceDest(rec, req)

		require.Equal(t, http.StatusBadRequest, rec.Code)

		var errResp map[string]string
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp["status"])
	})

	t.Run("internal error", func(t *testing.T) {
		repo := &mockHealthRepo{
			sourceHealthErr: errors.New("query execution failed"),
		}

		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health/source-1/dest-1", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("sourceID", "source-1")
		rctx.URLParams.Add("destID", "dest-1")
		req = req.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))

		rec := httptest.NewRecorder()
		handler.GetHealthBySourceDest(rec, req)

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var errResp map[string]string
		err := jsonrs.NewDecoder(rec.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, "error", errResp["status"])
		require.Contains(t, errResp["message"], "failed to retrieve health data")
	})
}

// ---------------------------------------------------------------------------
// TestHealthHandler_ResponseFormat validates the exact JSON response structure
// produced by the handler. It creates known HealthSummary data with specific
// values and verifies that all fields are present, correctly serialized, and
// that timestamps follow ISO 8601 (RFC 3339) format.
// ---------------------------------------------------------------------------

func TestHealthHandler_ResponseFormat(t *testing.T) {
	t.Run("all fields present and correctly typed", func(t *testing.T) {
		// Construct test data with every field populated to a known value.
		now := time.Now().UTC()
		lastSyncStr := now.Format(time.RFC3339)

		repo := &mockHealthRepo{
			healthSummary: &HealthSummaryResponse{
				Sources: []*SourceHealth{
					{
						SourceID:   "src-format-test",
						SourceType: "ios",
						Destinations: []*DestinationHealth{
							{
								DestID:   "dest-format-test",
								DestType: "REDSHIFT",
								SyncDuration: DurationStats{
									Min: 50,
									Max: 10000,
									Avg: 3000,
									P95: 8500,
								},
								RowsSynced:         99999,
								ErrorRate:          0.015,
								ErrorCount:         3,
								ErrorCategory:      "network_error",
								LastSync:           lastSyncStr,
								SchemaChanges:      5,
								PreviousRowsSynced: 88888,
							},
						},
					},
				},
			},
		}

		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health", nil)
		rec := httptest.NewRecorder()

		handler.GetHealthSummary(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		// Decode into a raw map first to verify the exact JSON structure
		// including field names and value types.
		var rawResp map[string]any
		err := jsonrs.NewDecoder(rec.Body).Decode(&rawResp)
		require.NoError(t, err)

		// Top level must have "sources" key.
		sourcesRaw, ok := rawResp["sources"]
		require.Equal(t, true, ok, "response must contain 'sources' key")

		sources, ok := sourcesRaw.([]any)
		require.Equal(t, true, ok, "'sources' must be an array")
		require.Len(t, sources, 1)

		// Validate source-level fields.
		source, ok := sources[0].(map[string]any)
		require.Equal(t, true, ok)
		require.Equal(t, "src-format-test", source["sourceID"])
		require.Equal(t, "ios", source["sourceType"])

		destinations, ok := source["destinations"].([]any)
		require.Equal(t, true, ok)
		require.Len(t, destinations, 1)

		// Validate all destination-level fields.
		dest, ok := destinations[0].(map[string]any)
		require.Equal(t, true, ok)

		require.Equal(t, "dest-format-test", dest["destID"])
		require.Equal(t, "REDSHIFT", dest["destType"])

		// Verify numeric fields are the correct JSON types.
		// JSON numbers decode to float64 in Go's any/interface{}.
		require.Equal(t, float64(99999), dest["rowsSynced"])
		require.Equal(t, 0.015, dest["errorRate"])
		require.Equal(t, float64(3), dest["errorCount"])
		require.Equal(t, "network_error", dest["errorCategory"])
		require.Equal(t, float64(5), dest["schemaChanges"])
		require.Equal(t, float64(88888), dest["previousRowsSynced"])

		// Verify timestamp is in ISO 8601 / RFC 3339 format.
		lastSyncValue, ok := dest["lastSync"].(string)
		require.Equal(t, true, ok, "lastSync must be a string")
		require.Equal(t, lastSyncStr, lastSyncValue)

		// Parse the timestamp to confirm it is valid RFC 3339.
		_, parseErr := time.Parse(time.RFC3339, lastSyncValue)
		require.NoError(t, parseErr, "lastSync must be valid RFC 3339 timestamp")

		// Validate syncDuration sub-object structure.
		syncDuration, ok := dest["syncDuration"].(map[string]any)
		require.Equal(t, true, ok, "'syncDuration' must be an object")
		require.Equal(t, float64(50), syncDuration["min"])
		require.Equal(t, float64(10000), syncDuration["max"])
		require.Equal(t, float64(3000), syncDuration["avg"])
		require.Equal(t, float64(8500), syncDuration["p95"])
	})

	t.Run("omitempty fields absent when zero", func(t *testing.T) {
		// Verify that fields marked with json:"...,omitempty" are absent
		// from the JSON output when their values are zero/empty.
		repo := &mockHealthRepo{
			healthSummary: &HealthSummaryResponse{
				Sources: []*SourceHealth{
					{
						SourceID:   "src-omit",
						SourceType: "web",
						Destinations: []*DestinationHealth{
							{
								DestID:   "dest-omit",
								DestType: "POSTGRES",
								SyncDuration: DurationStats{
									Min: 10,
									Max: 20,
									Avg: 15,
								},
								RowsSynced: 100,
								ErrorRate:  0.0,
								ErrorCount: 0,
								LastSync:   "2024-06-01T00:00:00Z",
								// ErrorCategory is "" → omitempty should omit it
								// PreviousRowsSynced is 0 → omitempty should omit it
							},
						},
					},
				},
			},
		}

		handler := NewHealthHandler(logger.NOP, repo)

		req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health", nil)
		rec := httptest.NewRecorder()

		handler.GetHealthSummary(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)

		var rawResp map[string]any
		err := jsonrs.NewDecoder(rec.Body).Decode(&rawResp)
		require.NoError(t, err)

		sources := rawResp["sources"].([]any)
		source := sources[0].(map[string]any)
		destinations := source["destinations"].([]any)
		dest := destinations[0].(map[string]any)

		// Fields with omitempty should be absent when zero-valued.
		_, hasErrorCategory := dest["errorCategory"]
		require.Equal(t, false, hasErrorCategory, "errorCategory should be omitted when empty")

		_, hasPrevRows := dest["previousRowsSynced"]
		require.Equal(t, false, hasPrevRows, "previousRowsSynced should be omitted when zero")
	})
}
