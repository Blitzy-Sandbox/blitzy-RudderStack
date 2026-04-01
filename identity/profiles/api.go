// Package profiles implements the Profiles REST API (E-027) for the
// RudderStack identity resolution system. It provides HTTP endpoints for
// querying identity profiles including traits, external identifiers, events,
// and metadata with sub-200ms response times backed by Redis caching.
//
// The API follows RESTful conventions using the chi/v5 router framework
// consistent with the Gateway HTTP patterns in gateway/handle_http.go.
package profiles

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/identity/graph"
	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ---------------------------------------------------------------------------
// Response Types
// ---------------------------------------------------------------------------

// ProfileResponse is the JSON envelope for a full profile lookup.
type ProfileResponse struct {
	Segment    SegmentResponse     `json:"segment"`
	ExternalIDs []ExternalIDResponse `json:"external_ids"`
	Traits     []TraitResponse     `json:"traits"`
}

// SegmentResponse represents the identity segment metadata in JSON responses.
type SegmentResponse struct {
	ID          int64  `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	SegmentID   string `json:"segment_id"`
	CreatedAt   string `json:"created_at"`
}

// ExternalIDResponse represents a single external identifier in JSON responses.
type ExternalIDResponse struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// TraitResponse represents a single key-value trait in JSON responses.
type TraitResponse struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// MetadataResponse is the JSON envelope for profile metadata.
type MetadataResponse struct {
	SegmentID   int64  `json:"segment_id"`
	WorkspaceID string `json:"workspace_id"`
	CreatedAt   string `json:"created_at"`
}

// ErrorResponse is the JSON envelope for error responses.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// Handler Stats
// ---------------------------------------------------------------------------

// handlerStats collects Prometheus metrics for the Profiles API.
type handlerStats struct {
	requestCount stats.Measurement
	cacheHit     stats.Measurement
	cacheMiss    stats.Measurement
	latency      stats.Measurement
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Handler implements the Profiles REST API endpoints. It coordinates between
// the identity graph service (for data retrieval) and the profile cache
// (for sub-200ms response times).
type Handler struct {
	graphService graph.Service
	cache        ProfileCache
	conf         *config.Config
	logger       logger.Logger
	stats        handlerStats
}

// NewHandler creates a new Profiles API handler.
//
// Parameters:
//   - graphService: the identity graph service for data retrieval (required)
//   - cache: profile cache for sub-200ms responses (may be nil for no caching)
//   - conf: runtime configuration
//   - log: structured logger
//   - statsFactory: metrics factory
//
// Returns error if graphService is nil.
func NewHandler(graphService graph.Service, cache ProfileCache, conf *config.Config, log logger.Logger, statsFactory stats.Stats) (*Handler, error) {
	if graphService == nil {
		return nil, fmt.Errorf("profiles handler: graph service is required")
	}
	if cache == nil {
		cache = &NoopCache{} // graceful degradation — no caching
	}
	if log == nil {
		log = pkgLogger
	}
	if conf == nil {
		conf = config.Default
	}

	h := &Handler{
		graphService: graphService,
		cache:        cache,
		conf:         conf,
		logger:       log.Child("profiles-api"),
	}

	if statsFactory != nil {
		tags := stats.Tags{"module": "identity", "component": "profiles-api"}
		h.stats.requestCount = statsFactory.NewTaggedStat("profiles_request_count", stats.CountType, tags)
		h.stats.cacheHit = statsFactory.NewTaggedStat("profiles_cache_hit", stats.CountType, tags)
		h.stats.cacheMiss = statsFactory.NewTaggedStat("profiles_cache_miss", stats.CountType, tags)
		h.stats.latency = statsFactory.NewTaggedStat("profiles_latency", stats.TimerType, tags)
	}

	return h, nil
}

// Routes returns a chi.Router with all Profiles API endpoints registered.
// The routes are:
//
//	GET /v1/profiles/{id}              — full profile
//	GET /v1/profiles/{id}/traits       — profile traits
//	GET /v1/profiles/{id}/events       — profile events
//	GET /v1/profiles/{id}/external_ids — external identifiers
//	GET /v1/profiles/{id}/metadata     — profile metadata
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Route("/v1/profiles", func(r chi.Router) {
		r.Get("/{id}", h.getProfile)
		r.Get("/{id}/traits", h.getProfileTraits)
		r.Get("/{id}/events", h.getProfileEvents)
		r.Get("/{id}/external_ids", h.getProfileExternalIDs)
		r.Get("/{id}/metadata", h.getProfileMetadata)
	})
	return r
}

// ---------------------------------------------------------------------------
// Endpoint Handlers
// ---------------------------------------------------------------------------

// getProfile returns the full profile (segment + external IDs + traits).
func (h *Handler) getProfile(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	defer func() {
		if h.stats.latency != nil {
			h.stats.latency.Since(startTime)
		}
	}()

	if h.stats.requestCount != nil {
		h.stats.requestCount.Increment()
	}

	segmentID, err := parseProfileID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	// Try cache first
	profileData, err := h.cache.GetProfile(r.Context(), segmentID)
	if err != nil {
		// Cache error — log and fall through to graph service
		h.logger.Warnn("Cache get error, falling back to graph",
			logger.NewIntField("segmentID", segmentID),
		)
	}

	if profileData != nil {
		// Cache hit
		if h.stats.cacheHit != nil {
			h.stats.cacheHit.Increment()
		}
		writeProfileResponse(w, profileData)
		return
	}

	// Cache miss — fetch from graph service
	if h.stats.cacheMiss != nil {
		h.stats.cacheMiss.Increment()
	}

	profileData, err = h.fetchProfileFromGraph(r.Context(), segmentID)
	if err != nil {
		h.logger.Errorn("Error fetching profile from graph",
			logger.NewIntField("segmentID", segmentID),
		)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to retrieve profile")
		return
	}

	if profileData == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
		return
	}

	// Populate cache (best-effort, errors logged but not returned)
	cacheTTL := 5 * time.Minute
	if cacheErr := h.cache.SetProfile(r.Context(), segmentID, profileData, cacheTTL); cacheErr != nil {
		h.logger.Warnn("Cache set error",
			logger.NewIntField("segmentID", segmentID),
		)
	}

	writeProfileResponse(w, profileData)
}

// getProfileTraits returns traits for a profile.
func (h *Handler) getProfileTraits(w http.ResponseWriter, r *http.Request) {
	segmentID, err := parseProfileID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	traits, err := h.graphService.GetSegmentTraits(r.Context(), segmentID)
	if err != nil {
		h.logger.Errorn("Error fetching traits",
			logger.NewIntField("segmentID", segmentID),
		)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to retrieve traits")
		return
	}

	// Check if profile exists by verifying data was returned
	if traits == nil {
		// Confirm profile existence via GetProfileData
		profileData, profileErr := h.graphService.GetProfileData(r.Context(), segmentID)
		if profileErr != nil || profileData == nil {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
			return
		}
		traits = profileData.Traits
	}

	// Build response with empty array (never null)
	traitResponses := make([]TraitResponse, 0, len(traits))
	for _, t := range traits {
		traitResponses = append(traitResponses, TraitResponse{
			Key:   t.Key,
			Value: t.Value,
		})
	}

	writeJSON(w, http.StatusOK, traitResponses)
}

// getProfileEvents returns events for a profile.
// Event storage is separate from the identity graph — this endpoint
// returns an empty array initially and will be extended when event
// storage integration is built.
func (h *Handler) getProfileEvents(w http.ResponseWriter, r *http.Request) {
	segmentID, err := parseProfileID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	// Verify profile exists
	profileData, profileErr := h.graphService.GetProfileData(r.Context(), segmentID)
	if profileErr != nil {
		h.logger.Errorn("Error checking profile existence",
			logger.NewIntField("segmentID", segmentID),
		)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to check profile")
		return
	}
	if profileData == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
		return
	}

	// Events storage is separate — return empty array for now
	writeJSON(w, http.StatusOK, []struct{}{})
}

// getProfileExternalIDs returns external identifiers for a profile.
func (h *Handler) getProfileExternalIDs(w http.ResponseWriter, r *http.Request) {
	segmentID, err := parseProfileID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	externalIDs, err := h.graphService.GetSegmentIdentifiers(r.Context(), segmentID)
	if err != nil {
		h.logger.Errorn("Error fetching external IDs",
			logger.NewIntField("segmentID", segmentID),
		)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to retrieve external IDs")
		return
	}

	// Check if profile exists
	if externalIDs == nil {
		profileData, profileErr := h.graphService.GetProfileData(r.Context(), segmentID)
		if profileErr != nil || profileData == nil {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
			return
		}
		externalIDs = profileData.ExternalIDs
	}

	// Build response with empty array (never null)
	idResponses := make([]ExternalIDResponse, 0, len(externalIDs))
	for _, eid := range externalIDs {
		idResponses = append(idResponses, ExternalIDResponse{
			Type:  eid.ExternalIDType,
			Value: eid.ExternalIDValue,
		})
	}

	writeJSON(w, http.StatusOK, idResponses)
}

// getProfileMetadata returns metadata for a profile.
func (h *Handler) getProfileMetadata(w http.ResponseWriter, r *http.Request) {
	segmentID, err := parseProfileID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	profileData, err := h.graphService.GetProfileData(r.Context(), segmentID)
	if err != nil {
		h.logger.Errorn("Error fetching profile metadata",
			logger.NewIntField("segmentID", segmentID),
		)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to retrieve metadata")
		return
	}
	if profileData == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
		return
	}

	resp := MetadataResponse{
		SegmentID:   profileData.Segment.ID,
		WorkspaceID: profileData.Segment.WorkspaceID,
		CreatedAt:   profileData.Segment.CreatedAt.Format(time.RFC3339),
	}

	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Helper Functions
// ---------------------------------------------------------------------------

// parseProfileID extracts and validates the profile ID (segment ID) from
// the chi URL parameter "{id}".
func parseProfileID(r *http.Request) (int64, error) {
	idStr := chi.URLParam(r, "id")
	if idStr == "" {
		return 0, fmt.Errorf("profile id is required")
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid profile id %q: %w", idStr, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("profile id must be positive, got %d", id)
	}
	return id, nil
}

// fetchProfileFromGraph retrieves the full profile data from the identity
// graph service. Returns nil if the profile does not exist.
func (h *Handler) fetchProfileFromGraph(ctx context.Context, segmentID int64) (*storage.ProfileData, error) {
	profileData, err := h.graphService.GetProfileData(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("fetch profile %d from graph: %w", segmentID, err)
	}
	return profileData, nil
}

// writeJSON serializes v to JSON and writes it to the response with the given status code.
func writeJSON(w http.ResponseWriter, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	data, err := jsonrs.Marshal(v)
	if err != nil {
		// Fallback: write a minimal error response
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"marshal_error","message":"failed to serialize response"}`))
		return
	}
	_, _ = w.Write(data)
}

// writeError writes a structured error JSON response.
func writeError(w http.ResponseWriter, statusCode int, errCode, message string) {
	resp := ErrorResponse{
		Error:   errCode,
		Message: message,
	}
	writeJSON(w, statusCode, resp)
}

// writeProfileResponse serializes storage.ProfileData into a ProfileResponse and writes it.
func writeProfileResponse(w http.ResponseWriter, data *storage.ProfileData) {
	externalIDs := make([]ExternalIDResponse, 0, len(data.ExternalIDs))
	for _, eid := range data.ExternalIDs {
		externalIDs = append(externalIDs, ExternalIDResponse{
			Type:  eid.ExternalIDType,
			Value: eid.ExternalIDValue,
		})
	}

	traits := make([]TraitResponse, 0, len(data.Traits))
	for _, t := range data.Traits {
		traits = append(traits, TraitResponse{
			Key:   t.Key,
			Value: t.Value,
		})
	}

	resp := ProfileResponse{
		Segment: SegmentResponse{
			ID:          data.Segment.ID,
			WorkspaceID: data.Segment.WorkspaceID,
			SegmentID:   data.Segment.SegmentID,
			CreatedAt:   data.Segment.CreatedAt.Format(time.RFC3339),
		},
		ExternalIDs: externalIDs,
		Traits:      traits,
	}

	writeJSON(w, http.StatusOK, resp)
}
