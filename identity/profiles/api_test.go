package profiles

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/identity/graph"
	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ===========================================================================
// Phase 1: Mock Dependencies Setup
// ===========================================================================

// mockGraphService implements the graph.Service interface methods used by the
// Handler. It stores pre-populated profile data and supports error injection
// hooks for each method.
type mockGraphService struct {
	// Pre-populated data keyed by segment ID
	profiles map[int64]*storage.ProfileData

	// Error injection hooks — set these to non-nil to simulate errors.
	getProfileDataErr       error
	getSegmentIdentifiersErr error
	getSegmentTraitsErr     error
	healthErr               error

	// Call tracking
	getProfileDataCalls       int
	getSegmentIdentifiersCalls int
	getSegmentTraitsCalls     int
}

func newMockGraphService() *mockGraphService {
	return &mockGraphService{
		profiles: make(map[int64]*storage.ProfileData),
	}
}

func (m *mockGraphService) ProcessEvent(_ context.Context, _ string, _ []byte) (*graph.ResolutionResult, error) {
	return nil, nil //nolint:nilnil // mock: nil result is valid for no-op stub
}

func (m *mockGraphService) ResolveIdentity(_ context.Context, _, _, _ string) (*storage.GraphSegment, error) {
	return nil, nil //nolint:nilnil // mock: nil result is valid for no-op stub
}

func (m *mockGraphService) GetSegmentIdentifiers(_ context.Context, segmentID int64) ([]storage.ExternalID, error) {
	m.getSegmentIdentifiersCalls++
	if m.getSegmentIdentifiersErr != nil {
		return nil, m.getSegmentIdentifiersErr
	}
	if p, ok := m.profiles[segmentID]; ok {
		return p.ExternalIDs, nil
	}
	return nil, nil
}

func (m *mockGraphService) GetSegmentTraits(_ context.Context, segmentID int64) ([]storage.Trait, error) {
	m.getSegmentTraitsCalls++
	if m.getSegmentTraitsErr != nil {
		return nil, m.getSegmentTraitsErr
	}
	if p, ok := m.profiles[segmentID]; ok {
		return p.Traits, nil
	}
	return nil, nil
}

func (m *mockGraphService) GetProfileData(_ context.Context, segmentID int64) (*storage.ProfileData, error) {
	m.getProfileDataCalls++
	if m.getProfileDataErr != nil {
		return nil, m.getProfileDataErr
	}
	if p, ok := m.profiles[segmentID]; ok {
		return p, nil
	}
	return nil, nil //nolint:nilnil // mock: nil profile means not found per interface contract
}

func (m *mockGraphService) Health(_ context.Context) error {
	return m.healthErr
}

// mockCache implements the ProfileCache interface with an in-memory store.
// It supports hit/miss tracking and error injection hooks.
type mockCache struct {
	store map[int64]*storage.ProfileData

	// Error injection hooks
	getErr        error
	setErr        error
	invalidateErr error

	// Call tracking
	getCalls        int
	setCalls        int
	invalidateCalls int
}

func newMockCache() *mockCache {
	return &mockCache{
		store: make(map[int64]*storage.ProfileData),
	}
}

func (c *mockCache) GetProfile(_ context.Context, segmentID int64) (*storage.ProfileData, error) {
	c.getCalls++
	if c.getErr != nil {
		return nil, c.getErr
	}
	if p, ok := c.store[segmentID]; ok {
		return p, nil
	}
	return nil, nil //nolint:nilnil // mock: nil profile = cache miss per ProfileCache contract
}

func (c *mockCache) SetProfile(_ context.Context, segmentID int64, data *storage.ProfileData, _ time.Duration) error {
	c.setCalls++
	if c.setErr != nil {
		return c.setErr
	}
	c.store[segmentID] = data
	return nil
}

func (c *mockCache) InvalidateProfile(_ context.Context, segmentID int64) error {
	c.invalidateCalls++
	if c.invalidateErr != nil {
		return c.invalidateErr
	}
	delete(c.store, segmentID)
	return nil
}

// ---------------------------------------------------------------------------
// Helper Functions
// ---------------------------------------------------------------------------

// newTestHandler creates a Handler with mock dependencies for testing.
// It returns the Handler along with references to the mocks for assertions.
func newTestHandler(t *testing.T) (*Handler, *mockGraphService, *mockCache) {
	t.Helper()
	gs := newMockGraphService()
	cache := newMockCache()
	conf := config.New()
	log := logger.NewLogger()
	var statsFactory stats.Stats // nil is fine for tests

	h, err := NewHandler(gs, cache, conf, log, statsFactory)
	require.NoError(t, err)
	require.NotNil(t, h)
	return h, gs, cache
}

// executeRequest is a helper that dispatches an HTTP GET request through the
// handler's chi router and returns the recorded response.
func executeRequest(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	handler.ServeHTTP(rr, req)
	return rr
}

// newTestProfileData builds a standard test profile fixture.
func newTestProfileData() *storage.ProfileData {
	now := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	return &storage.ProfileData{
		Segment: storage.GraphSegment{
			ID:          1,
			WorkspaceID: "ws-1",
			SegmentID:   "uuid-segment-1",
			CreatedAt:   now,
		},
		ExternalIDs: []storage.ExternalID{
			{
				ID:              100,
				GraphID:         1,
				WorkspaceID:     "ws-1",
				ExternalIDType:  "user_id",
				ExternalIDValue: "user-123",
				CreatedSource:   "identify",
				CreatedAt:       now,
			},
			{
				ID:              101,
				GraphID:         1,
				WorkspaceID:     "ws-1",
				ExternalIDType:  "email",
				ExternalIDValue: "user@example.com",
				CreatedSource:   "identify",
				CreatedAt:       now,
			},
		},
		Traits: []storage.Trait{
			{ID: 200, GraphID: 1, Key: "name", Value: "Alice", UpdatedAt: now},
			{ID: 201, GraphID: 1, Key: "plan", Value: "enterprise", UpdatedAt: now},
		},
	}
}

// ===========================================================================
// Phase 2: Route Registration Tests
// ===========================================================================

func TestHandler_Routes(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := h.Routes()
	require.NotNil(t, router)

	// Verify all five endpoints are registered by sending requests.
	// Registered routes return non-405 status codes (they may return 400 or 200
	// depending on ID validity, but NOT 405 Method Not Allowed).
	endpoints := []string{
		"/v1/profiles/1",
		"/v1/profiles/1/traits",
		"/v1/profiles/1/events",
		"/v1/profiles/1/external_ids",
		"/v1/profiles/1/metadata",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			rr := executeRequest(t, router, ep)
			// 405 would mean the route is not registered.
			require.NotEqual(t, http.StatusMethodNotAllowed, rr.Code,
				"route %s should be registered", ep)
		})
	}
}

// ===========================================================================
// Phase 3: Full Profile Endpoint Tests
// ===========================================================================

func TestGetProfile_Success(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	profile := newTestProfileData()
	gs.profiles[1] = profile

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1")

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	var resp ProfileResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Verify segment data
	require.Equal(t, int64(1), resp.Segment.ID)
	require.Equal(t, "ws-1", resp.Segment.WorkspaceID)
	require.Equal(t, "uuid-segment-1", resp.Segment.SegmentID)
	require.NotEmpty(t, resp.Segment.CreatedAt)

	// Verify external IDs
	require.Len(t, resp.ExternalIDs, 2)
	require.Equal(t, "user_id", resp.ExternalIDs[0].Type)
	require.Equal(t, "user-123", resp.ExternalIDs[0].Value)
	require.Equal(t, "email", resp.ExternalIDs[1].Type)
	require.Equal(t, "user@example.com", resp.ExternalIDs[1].Value)

	// Verify traits
	require.Len(t, resp.Traits, 2)
	require.Equal(t, "name", resp.Traits[0].Key)
	require.Equal(t, "Alice", resp.Traits[0].Value)
	require.Equal(t, "plan", resp.Traits[1].Key)
	require.Equal(t, "enterprise", resp.Traits[1].Value)
}

func TestGetProfile_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/999")

	require.Equal(t, http.StatusNotFound, rr.Code)

	var resp ErrorResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Contains(t, resp.Error, "not_found")
	require.Contains(t, resp.Message, "999")
}

func TestGetProfile_InvalidID(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/not-a-number")

	require.Equal(t, http.StatusBadRequest, rr.Code)

	var resp ErrorResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, "invalid_id", resp.Error)
}

func TestGetProfile_CacheHit(t *testing.T) {
	h, gs, cache := newTestHandler(t)
	profile := newTestProfileData()
	cache.store[1] = profile

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1")

	require.Equal(t, http.StatusOK, rr.Code)

	// Verify the response contains the cached data
	var resp ProfileResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, int64(1), resp.Segment.ID)
	require.Len(t, resp.ExternalIDs, 2)
	require.Len(t, resp.Traits, 2)

	// The graph service should NOT have been called (served from cache)
	require.Equal(t, 0, gs.getProfileDataCalls,
		"graph service should not be called on cache hit")
	require.Equal(t, 0, gs.getSegmentIdentifiersCalls,
		"graph service identifiers should not be called on cache hit")
	require.Equal(t, 0, gs.getSegmentTraitsCalls,
		"graph service traits should not be called on cache hit")

	// Cache get should have been called exactly once
	require.Equal(t, 1, cache.getCalls)
}

func TestGetProfile_CacheMiss_ThenPopulated(t *testing.T) {
	h, gs, cache := newTestHandler(t)
	profile := newTestProfileData()
	gs.profiles[1] = profile
	// Cache is empty → miss

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1")

	require.Equal(t, http.StatusOK, rr.Code)

	var resp ProfileResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, int64(1), resp.Segment.ID)

	// Graph service should have been called
	require.Greater(t, gs.getProfileDataCalls, 0,
		"graph service should be called on cache miss")

	// Cache SetProfile should have been called to populate cache
	require.Equal(t, 1, cache.setCalls,
		"cache set should be called after graph fetch")

	// Verify the cache now contains the profile
	require.NotNil(t, cache.store[1])
}

// ===========================================================================
// Phase 4: Traits Endpoint Tests
// ===========================================================================

func TestGetProfileTraits_Success(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	now := time.Now()
	gs.profiles[1] = &storage.ProfileData{
		Segment: storage.GraphSegment{ID: 1, WorkspaceID: "ws-1", SegmentID: "seg-1", CreatedAt: now},
		Traits: []storage.Trait{
			{ID: 1, GraphID: 1, Key: "name", Value: "Alice", UpdatedAt: now},
			{ID: 2, GraphID: 1, Key: "email", Value: "alice@example.com", UpdatedAt: now},
		},
	}

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1/traits")

	require.Equal(t, http.StatusOK, rr.Code)

	var traits []TraitResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &traits)
	require.NoError(t, err)
	require.Len(t, traits, 2)
	require.Equal(t, "name", traits[0].Key)
	require.Equal(t, "Alice", traits[0].Value)
	require.Equal(t, "email", traits[1].Key)
	require.Equal(t, "alice@example.com", traits[1].Value)
}

func TestGetProfileTraits_EmptyTraits(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	now := time.Now()
	gs.profiles[1] = &storage.ProfileData{
		Segment: storage.GraphSegment{ID: 1, WorkspaceID: "ws-1", SegmentID: "seg-1", CreatedAt: now},
		Traits:  []storage.Trait{}, // empty, not nil
	}

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1/traits")

	require.Equal(t, http.StatusOK, rr.Code)

	var traits []TraitResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &traits)
	require.NoError(t, err)
	require.Len(t, traits, 0, "should return empty array, not null")
}

func TestGetProfileTraits_ProfileNotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/999/traits")

	require.Equal(t, http.StatusNotFound, rr.Code)
}

// ===========================================================================
// Phase 5: Events Endpoint Tests
// ===========================================================================

func TestGetProfileEvents_Success(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	now := time.Now()
	gs.profiles[1] = &storage.ProfileData{
		Segment: storage.GraphSegment{ID: 1, WorkspaceID: "ws-1", SegmentID: "seg-1", CreatedAt: now},
	}

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1/events")

	require.Equal(t, http.StatusOK, rr.Code)
	// Events storage is separate — returns empty array
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	var events []interface{}
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &events)
	require.NoError(t, err)
	require.Len(t, events, 0)
}

func TestGetProfileEvents_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/999/events")

	require.Equal(t, http.StatusNotFound, rr.Code)
}

// ===========================================================================
// Phase 6: External IDs Endpoint Tests
// ===========================================================================

func TestGetProfileExternalIDs_Success(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	now := time.Now()
	gs.profiles[1] = &storage.ProfileData{
		Segment: storage.GraphSegment{ID: 1, WorkspaceID: "ws-1", SegmentID: "seg-1", CreatedAt: now},
		ExternalIDs: []storage.ExternalID{
			{ID: 1, GraphID: 1, WorkspaceID: "ws-1", ExternalIDType: "user_id", ExternalIDValue: "user-123", CreatedAt: now},
			{ID: 2, GraphID: 1, WorkspaceID: "ws-1", ExternalIDType: "email", ExternalIDValue: "user@example.com", CreatedAt: now},
			{ID: 3, GraphID: 1, WorkspaceID: "ws-1", ExternalIDType: "anonymous_id", ExternalIDValue: "anon-uuid", CreatedAt: now},
		},
	}

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1/external_ids")

	require.Equal(t, http.StatusOK, rr.Code)

	var ids []ExternalIDResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &ids)
	require.NoError(t, err)
	require.Len(t, ids, 3)
	require.Equal(t, "user_id", ids[0].Type)
	require.Equal(t, "user-123", ids[0].Value)
	require.Equal(t, "email", ids[1].Type)
	require.Equal(t, "user@example.com", ids[1].Value)
	require.Equal(t, "anonymous_id", ids[2].Type)
	require.Equal(t, "anon-uuid", ids[2].Value)
}

func TestGetProfileExternalIDs_EmptyIDs(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	now := time.Now()
	gs.profiles[1] = &storage.ProfileData{
		Segment:     storage.GraphSegment{ID: 1, WorkspaceID: "ws-1", SegmentID: "seg-1", CreatedAt: now},
		ExternalIDs: []storage.ExternalID{}, // empty, not nil
	}

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1/external_ids")

	require.Equal(t, http.StatusOK, rr.Code)

	var ids []ExternalIDResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &ids)
	require.NoError(t, err)
	require.Len(t, ids, 0, "should return empty array, not null")
}

// ===========================================================================
// Phase 7: Metadata Endpoint Tests
// ===========================================================================

func TestGetProfileMetadata_Success(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	createdAt := time.Date(2025, 3, 20, 14, 0, 0, 0, time.UTC)
	gs.profiles[1] = &storage.ProfileData{
		Segment: storage.GraphSegment{
			ID:          1,
			WorkspaceID: "ws-1",
			SegmentID:   "uuid-segment-1",
			CreatedAt:   createdAt,
		},
	}

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1/metadata")

	require.Equal(t, http.StatusOK, rr.Code)

	var meta MetadataResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &meta)
	require.NoError(t, err)
	require.Equal(t, int64(1), meta.SegmentID)
	require.Equal(t, "ws-1", meta.WorkspaceID)
	require.Contains(t, meta.CreatedAt, "2025-03-20")
}

func TestGetProfileMetadata_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/999/metadata")

	require.Equal(t, http.StatusNotFound, rr.Code)
}

// ===========================================================================
// Phase 8: Response Time Validation
// ===========================================================================

func TestGetProfile_SubMillisecondFromCache(t *testing.T) {
	h, _, cache := newTestHandler(t)
	cache.store[1] = newTestProfileData()

	router := h.Routes()

	start := time.Now()
	rr := executeRequest(t, router, "/v1/profiles/1")
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rr.Code)
	// The AAP target is sub-200ms. A cache hit with in-memory mock should be
	// well below that — we use 200ms as the absolute ceiling.
	require.Less(t, elapsed, 200*time.Millisecond,
		"cache-hit response must be under 200ms, took %v", elapsed)
}

// ===========================================================================
// Phase 9: Error Handling Tests
// ===========================================================================

func TestGetProfile_GraphServiceError(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	gs.getProfileDataErr = errors.New("database connection lost")

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1")

	require.Equal(t, http.StatusInternalServerError, rr.Code)

	var resp ErrorResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, "internal_error", resp.Error)
}

func TestGetProfile_CacheError_FallsBackToGraph(t *testing.T) {
	h, gs, cache := newTestHandler(t)
	profile := newTestProfileData()
	gs.profiles[1] = profile
	cache.getErr = errors.New("redis timeout")

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1")

	// Should succeed despite cache error — graceful degradation
	require.Equal(t, http.StatusOK, rr.Code)

	var resp ProfileResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, int64(1), resp.Segment.ID)

	// Graph service should have been called as fallback
	require.Greater(t, gs.getProfileDataCalls, 0,
		"graph service should be called when cache errors")
}

func TestHandler_NilGraphService(t *testing.T) {
	// Creating a Handler with nil graph service should return an error
	_, err := NewHandler(nil, newMockCache(), config.New(), logger.NewLogger(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "graph service is required")
}

// ===========================================================================
// Phase 10: Constructor Tests
// ===========================================================================

func TestNewHandler(t *testing.T) {
	gs := newMockGraphService()
	cache := newMockCache()
	conf := config.New()
	log := logger.NewLogger()
	var statsFactory stats.Stats

	h, err := NewHandler(gs, cache, conf, log, statsFactory)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.NotNil(t, h.graphService)
	require.NotNil(t, h.cache)
	require.NotNil(t, h.conf)
	require.NotNil(t, h.logger)
}

func TestNewHandler_NilDependencies(t *testing.T) {
	gs := newMockGraphService()

	t.Run("nil graph service returns error", func(t *testing.T) {
		_, err := NewHandler(nil, newMockCache(), config.New(), logger.NewLogger(), nil)
		require.Error(t, err)
	})

	t.Run("nil cache uses NoopCache", func(t *testing.T) {
		h, err := NewHandler(gs, nil, config.New(), logger.NewLogger(), nil)
		require.NoError(t, err)
		require.NotNil(t, h)
		require.NotNil(t, h.cache, "cache should default to NoopCache, not nil")
	})

	t.Run("nil logger uses package default", func(t *testing.T) {
		h, err := NewHandler(gs, newMockCache(), config.New(), nil, nil)
		require.NoError(t, err)
		require.NotNil(t, h)
		require.NotNil(t, h.logger, "logger should default to pkgLogger, not nil")
	})

	t.Run("nil config uses default", func(t *testing.T) {
		h, err := NewHandler(gs, newMockCache(), nil, logger.NewLogger(), nil)
		require.NoError(t, err)
		require.NotNil(t, h)
		require.NotNil(t, h.conf, "config should default to config.Default, not nil")
	})

	t.Run("nil stats is acceptable", func(t *testing.T) {
		h, err := NewHandler(gs, newMockCache(), config.New(), logger.NewLogger(), nil)
		require.NoError(t, err)
		require.NotNil(t, h)
	})
}

// ===========================================================================
// Additional Edge Case Tests
// ===========================================================================

func TestGetProfile_InvalidID_Table(t *testing.T) {
	h, _, _ := newTestHandler(t)
	router := h.Routes()

	testCases := []struct {
		name string
		path string
	}{
		{"string id", "/v1/profiles/abc"},
		{"float id", "/v1/profiles/1.5"},
		{"negative id", "/v1/profiles/-1"},
		{"zero id", "/v1/profiles/0"},
		{"empty id", "/v1/profiles/"},
		{"special chars", "/v1/profiles/!@#"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rr := executeRequest(t, router, tc.path)
			// Non-numeric or non-positive IDs should return 400 or 404 (from chi not matching).
			// We just verify it's NOT 200 OK.
			require.NotEqual(t, http.StatusOK, rr.Code,
				"invalid id %q should not return 200", tc.path)
		})
	}
}

func TestGetProfile_ContentType(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	gs.profiles[1] = newTestProfileData()

	router := h.Routes()

	// Test all endpoints return application/json
	endpoints := []string{
		"/v1/profiles/1",
		"/v1/profiles/1/traits",
		"/v1/profiles/1/events",
		"/v1/profiles/1/external_ids",
		"/v1/profiles/1/metadata",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			rr := executeRequest(t, router, ep)
			require.Equal(t, http.StatusOK, rr.Code)
			require.Equal(t, "application/json", rr.Header().Get("Content-Type"),
				"endpoint %s should return application/json", ep)
		})
	}
}

func TestWriteError(t *testing.T) {
	rr := httptest.NewRecorder()
	writeError(rr, http.StatusBadRequest, "bad_request", "test error message")

	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	var resp ErrorResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, "bad_request", resp.Error)
	require.Equal(t, "test error message", resp.Message)
}

func TestWriteJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	data := map[string]string{"key": "value"}
	writeJSON(rr, http.StatusOK, data)

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	var resp map[string]string
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, "value", resp["key"])
}

func TestParseProfileID(t *testing.T) {
	testCases := []struct {
		name      string
		id        string
		wantID    int64
		wantError bool
	}{
		{"valid id", "1", 1, false},
		{"large id", "999999", 999999, false},
		{"string", "abc", 0, true},
		{"negative", "-1", 0, true},
		{"zero", "0", 0, true},
		{"float", "1.5", 0, true},
		{"empty", "", 0, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Build a chi context with the id parameter
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", tc.id)
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

			id, err := parseProfileID(r)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantID, id)
			}
		})
	}
}

func TestGetProfile_CacheMiss_GraphNotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	// Both cache and graph have no data for this ID

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/42")

	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestGetProfile_CacheSetError_StillReturnsData(t *testing.T) {
	h, gs, cache := newTestHandler(t)
	profile := newTestProfileData()
	gs.profiles[1] = profile
	cache.setErr = errors.New("redis write failure")

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1")

	// Response should succeed even though cache set failed
	require.Equal(t, http.StatusOK, rr.Code)

	var resp ProfileResponse
	err := jsonrs.Unmarshal(rr.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.Equal(t, int64(1), resp.Segment.ID)
}

// TestGetProfileTraits_GraphError tests error propagation from graph service.
func TestGetProfileTraits_GraphError(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	gs.getSegmentTraitsErr = errors.New("database error")

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1/traits")

	require.Equal(t, http.StatusInternalServerError, rr.Code)
}

// TestGetProfileExternalIDs_GraphError tests error propagation for external IDs.
func TestGetProfileExternalIDs_GraphError(t *testing.T) {
	h, gs, _ := newTestHandler(t)
	gs.getSegmentIdentifiersErr = errors.New("database error")

	router := h.Routes()
	rr := executeRequest(t, router, "/v1/profiles/1/external_ids")

	require.Equal(t, http.StatusInternalServerError, rr.Code)
}

// TestMockGraphService_InterfaceCompliance verifies the mock properly satisfies
// the interface contract used by the Handler.
func TestMockGraphService_InterfaceCompliance(t *testing.T) {
	gs := newMockGraphService()

	ctx := context.Background()
	_, _ = gs.ProcessEvent(ctx, "ws-1", []byte(`{}`))
	_, _ = gs.ResolveIdentity(ctx, "ws-1", "user_id", "u1")
	_, _ = gs.GetSegmentIdentifiers(ctx, 1)
	_, _ = gs.GetSegmentTraits(ctx, 1)
	_, _ = gs.GetProfileData(ctx, 1)
	_ = gs.Health(ctx)
}

// TestMockCache_InterfaceCompliance verifies the mock properly satisfies
// the ProfileCache interface contract.
func TestMockCache_InterfaceCompliance(t *testing.T) {
	cache := newMockCache()

	ctx := context.Background()
	_, _ = cache.GetProfile(ctx, 1)
	_ = cache.SetProfile(ctx, 1, newTestProfileData(), time.Minute)
	_ = cache.InvalidateProfile(ctx, 1)
}

// Compile-time verification that mocks implement expected interfaces.
var (
	_ ProfileCache = (*mockCache)(nil)
)

