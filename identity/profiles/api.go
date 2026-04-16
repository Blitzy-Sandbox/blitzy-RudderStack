// Package profiles implements the Profiles REST API handler for RudderStack's
// identity resolution system (E-027, Sprint 6-8). It provides read access to
// resolved identity profiles including traits, external identifiers, segment
// metadata, and profile metadata. All endpoints target sub-200ms response times
// via a Redis-backed read-through caching layer.
//
// Architecture:
//
//	Request → Handler → Cache (try) → Graph Service → Storage
//	Cache hit → JSON response (sub-1ms)
//	Cache miss → Graph query → Cache populate → JSON response
//
// This file is the central orchestration handler for the identity/profiles
// package, analogous to warehouse/api/http.go in architecture.
package profiles

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/identity/graph"
	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ---------------------------------------------------------------------------
// Response Types
// ---------------------------------------------------------------------------

// ProfileResponse represents the full profile response returned by
// GET /v1/profiles/{id}. It combines graph segment metadata, external
// identifiers, and trait key-value pairs into a single composite payload.
type ProfileResponse struct {
	Segment     SegmentResponse      `json:"segment"`
	ExternalIDs []ExternalIDResponse `json:"external_ids"`
	Traits      []TraitResponse      `json:"traits"`
}

// SegmentResponse represents the identity graph segment metadata returned
// as part of the full profile or metadata endpoints.
type SegmentResponse struct {
	ID          int64     `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	SegmentID   string    `json:"segment_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// ExternalIDResponse represents an external identifier in API responses.
// Each external identifier maps an external system's user ID (e.g. email,
// anonymous_id, ios.id) to the resolved identity segment.
type ExternalIDResponse struct {
	Type      string     `json:"type"`
	Value     string     `json:"value"`
	Source    string     `json:"source,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	MergedAt  *time.Time `json:"merged_at,omitempty"`
}

// TraitResponse represents a profile trait (key-value pair) in API responses.
// Traits are user attributes such as name, email, plan, etc., stored as
// string key-value pairs with an update timestamp.
type TraitResponse struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MetadataResponse represents profile metadata returned by
// GET /v1/profiles/{id}/metadata. It provides summary counts of identifiers
// and traits without returning the full data payloads.
type MetadataResponse struct {
	ID              int64     `json:"id"`
	WorkspaceID     string    `json:"workspace_id"`
	SegmentID       string    `json:"segment_id"`
	CreatedAt       time.Time `json:"created_at"`
	IdentifierCount int       `json:"identifier_count"`
	TraitCount      int       `json:"trait_count"`
}

// ErrorResponse represents a standard error response for all profile
// endpoints. It provides a machine-readable error code and an optional
// human-readable message.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// Handler and Stats
// ---------------------------------------------------------------------------

// handlerStats holds tagged Prometheus metrics for the Profiles API,
// following the pattern from processor/trackingplan.go and
// identity/graph/resolver.go.
type handlerStats struct {
	requestCount   stats.Measurement
	requestLatency stats.Measurement
	cacheHits      stats.Measurement
	cacheMisses    stats.Measurement
	errors         stats.Measurement
}

// Handler implements the Profiles REST API endpoints for E-027.
// It serves as the HTTP layer for the identity resolution subsystem,
// providing read access to resolved identity profiles with sub-200ms
// response times via the Redis-backed cache layer.
//
// The handler is safe for concurrent use — it holds no per-request state.
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
//   - graphService: The identity graph service providing resolution and data
//     retrieval. Must not be nil.
//   - cache: Redis-backed profile cache for sub-200ms responses. If nil, a
//     NoopCache is used for graceful degradation (queries go directly to the
//     graph service).
//   - conf: Reloadable configuration. If nil, config.Default is used.
//   - log: Structured logger. If nil, the package-level pkgLogger is used.
//   - statsFactory: Metrics factory for tagged measurements. If nil, metrics
//     are silently disabled.
//
// Returns an error if graphService is nil.
func NewHandler(
	graphService graph.Service,
	cache ProfileCache,
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
) (*Handler, error) {
	if graphService == nil {
		return nil, fmt.Errorf("profiles handler: graph service is required")
	}
	if log == nil {
		log = pkgLogger
	}
	if conf == nil {
		conf = config.Default
	}
	// Graceful degradation: use NoopCache when no cache is provided.
	if cache == nil {
		cache = &NoopCache{}
	}

	h := &Handler{
		graphService: graphService,
		cache:        cache,
		conf:         conf,
		logger:       log.Child("profiles"),
	}

	// Initialize tagged stats for Prometheus scraping.
	if statsFactory != nil {
		tags := stats.Tags{"module": "identity", "component": "profiles_api"}
		h.stats.requestCount = statsFactory.NewTaggedStat(
			"profiles_api_request_count", stats.CountType, tags,
		)
		h.stats.requestLatency = statsFactory.NewTaggedStat(
			"profiles_api_request_latency", stats.TimerType, tags,
		)
		h.stats.cacheHits = statsFactory.NewTaggedStat(
			"profiles_api_cache_hits", stats.CountType, tags,
		)
		h.stats.cacheMisses = statsFactory.NewTaggedStat(
			"profiles_api_cache_misses", stats.CountType, tags,
		)
		h.stats.errors = statsFactory.NewTaggedStat(
			"profiles_api_errors", stats.CountType, tags,
		)
	}

	return h, nil
}

// ---------------------------------------------------------------------------
// Route Registration
// ---------------------------------------------------------------------------

// Routes returns a chi.Router with all Profiles API endpoints registered.
// The router is intended to be mounted at the appropriate prefix in the
// Gateway's main HTTP server (gateway/handle_http.go). Route paths are
// relative — the caller is responsible for mounting the returned router
// under the appropriate prefix (e.g., "/v1/profiles" or "/profiles").
//
// When mounted under the Gateway's chi router at /v1/profiles (via the
// internalHttpHandlers map), chi strips the prefix before dispatching to
// this router, so routes must be defined without the /v1/profiles prefix.
//
// Endpoints (relative, mounted at /v1/profiles):
//
//	GET /{id}              — Full profile (segment + external_ids + traits)
//	GET /{id}/traits       — Profile traits only
//	GET /{id}/events       — Profile events (initial: empty array)
//	GET /{id}/external_ids — External identifiers
//	GET /{id}/metadata     — Profile metadata with summary counts
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{id}", h.getProfile)
	r.Get("/{id}/traits", h.getProfileTraits)
	r.Get("/{id}/events", h.getProfileEvents)
	r.Get("/{id}/external_ids", h.getProfileExternalIDs)
	r.Get("/{id}/metadata", h.getProfileMetadata)
	return r
}

// ---------------------------------------------------------------------------
// Public Service Methods
// ---------------------------------------------------------------------------

// Health checks the health of the underlying identity graph service.
// Returns nil if the service is healthy, or an error describing the issue.
// This delegates to graph.Service.Health() to verify connectivity to
// the storage layer (PostgreSQL).
func (h *Handler) Health(ctx context.Context) error {
	return h.graphService.Health(ctx)
}

// ResolveByExternalID resolves a profile segment from an external identifier.
// This enables lookup by email, user_id, anonymous_id, device IDs, etc.
// through the identity graph's resolution engine (graph.Service.ResolveIdentity).
//
// Parameters:
//   - workspaceID: the workspace to scope the lookup to
//   - idType: external identifier type (e.g., "user_id", "email", "anonymous_id")
//   - idValue: external identifier value to resolve
//
// Returns the resolved GraphSegment if found, nil if not found, or error.
func (h *Handler) ResolveByExternalID(ctx context.Context, workspaceID, idType, idValue string) (*storage.GraphSegment, error) {
	return h.graphService.ResolveIdentity(ctx, workspaceID, idType, idValue)
}

// ---------------------------------------------------------------------------
// Handler Methods
// ---------------------------------------------------------------------------

// getProfile handles GET /v1/profiles/{id} — returns the full profile
// including segment metadata, external identifiers, and traits.
// Uses read-through caching for sub-200ms response times.
func (h *Handler) getProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	startTime := time.Now()
	defer func() {
		if h.stats.requestLatency != nil {
			h.stats.requestLatency.Since(startTime)
		}
	}()

	if h.stats.requestCount != nil {
		h.stats.requestCount.Increment()
	}

	segmentID, err := h.parseProfileID(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	// Read-through cache: try cache first.
	cached, cacheErr := h.cache.GetProfile(ctx, segmentID)
	if cacheErr != nil {
		// Cache errors are non-critical — log at WARN and fall back to graph.
		h.logger.Warnn("Cache get error, falling back to graph",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(cacheErr),
		)
	} else if cached != nil {
		if h.stats.cacheHits != nil {
			h.stats.cacheHits.Increment()
		}
		h.writeProfileResponse(w, cached)
		return
	}

	// Cache miss — proceed to graph service.
	if h.stats.cacheMisses != nil {
		h.stats.cacheMisses.Increment()
	}

	profileData, err := h.fetchProfileFromGraph(ctx, segmentID)
	if err != nil {
		if h.stats.errors != nil {
			h.stats.errors.Increment()
		}
		h.logger.Errorn("Error fetching profile",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch profile")
		return
	}
	if profileData == nil {
		h.writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
		return
	}

	// Populate cache for future requests.
	cacheTTL := h.conf.GetDuration("Identity.Profiles.Cache.TTL", 5, time.Minute)
	if setErr := h.cache.SetProfile(ctx, segmentID, profileData, cacheTTL); setErr != nil {
		h.logger.Warnn("Cache set error",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(setErr),
		)
	}

	h.writeProfileResponse(w, profileData)
}

// getProfileTraits handles GET /v1/profiles/{id}/traits — returns traits only.
// Returns 404 if the profile segment does not exist.
// Returns an empty JSON array [] if the profile exists but has no traits.
//
// Uses GetSegmentTraits() for targeted trait retrieval. If traits are nil
// (segment may not exist), falls back to fetchProfileFromGraph for existence
// verification before returning 404.
func (h *Handler) getProfileTraits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	segmentID, err := h.parseProfileID(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	// Use targeted GetSegmentTraits for efficient single-purpose retrieval.
	traits, err := h.graphService.GetSegmentTraits(ctx, segmentID)
	if err != nil {
		h.logger.Errorn("Error fetching profile traits",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch traits")
		return
	}

	// A nil result means the segment may not exist — verify existence via
	// full profile fetch before returning 404.
	if traits == nil {
		profileData, fetchErr := h.fetchProfileFromGraph(ctx, segmentID)
		if fetchErr != nil {
			h.logger.Errorn("Error verifying profile existence for traits",
				logger.NewIntField("segmentID", segmentID),
				obskit.Error(fetchErr),
			)
			h.writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch traits")
			return
		}
		if profileData == nil {
			h.writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
			return
		}
		// Profile exists but GetSegmentTraits returned nil — use traits from profile data.
		traits = profileData.Traits
	}

	// Convert storage types to API response types.
	// Always produce [] (not null) for empty collections.
	resp := make([]TraitResponse, 0, len(traits))
	for _, t := range traits {
		resp = append(resp, TraitResponse{
			Key:       t.Key,
			Value:     t.Value,
			UpdatedAt: t.UpdatedAt,
		})
	}

	h.writeJSON(w, http.StatusOK, resp)
}

// profileEvent represents a single identity-related event in the profile's timeline.
// Events are derived from the profile's external identifiers and traits — each
// creation, merge, or trait update generates an event entry.
type profileEvent struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source,omitempty"`
	Details   any       `json:"details,omitempty"`
}

// defaultProfileEventsLimit is the default maximum number of events returned
// per page when no limit query parameter is provided.
const defaultProfileEventsLimit = 100

// getProfileEvents handles GET /v1/profiles/{id}/events — returns identity
// events for a profile with pagination support.
//
// Events are derived from the profile's identity activity:
//   - External identifier additions (type: "identify")
//   - Identity merges (type: "merge")
//   - Trait updates (type: "trait_update")
//
// Query Parameters:
//   - limit: Maximum number of events to return (default: 100)
//   - offset: Number of events to skip (default: 0)
//
// Events are ordered by timestamp descending (most recent first).
func (h *Handler) getProfileEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	segmentID, err := h.parseProfileID(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	// Verify that the profile segment exists before returning events.
	profileData, err := h.fetchProfileFromGraph(ctx, segmentID)
	if err != nil {
		h.logger.Errorn("Error checking profile existence for events",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch events")
		return
	}
	if profileData == nil {
		h.writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
		return
	}

	// Parse pagination parameters.
	limit := defaultProfileEventsLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, parseErr := strconv.Atoi(v); parseErr == nil && parsed > 0 {
			limit = parsed
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if parsed, parseErr := strconv.Atoi(v); parseErr == nil && parsed >= 0 {
			offset = parsed
		}
	}

	// Build events from the profile's identity activity.
	events := h.buildProfileEvents(profileData)

	// Sort by timestamp descending (most recent first).
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.After(events[j].Timestamp)
	})

	// Apply pagination.
	total := len(events)
	if offset >= total {
		events = []profileEvent{}
	} else {
		end := offset + limit
		if end > total {
			end = total
		}
		events = events[offset:end]
	}

	resp := struct {
		Events []profileEvent `json:"events"`
		Total  int            `json:"total"`
		Limit  int            `json:"limit"`
		Offset int            `json:"offset"`
	}{
		Events: events,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}
	// Ensure events is never nil so JSON produces [] not null.
	if resp.Events == nil {
		resp.Events = []profileEvent{}
	}
	h.writeJSON(w, http.StatusOK, resp)
}

// buildProfileEvents constructs a chronological event timeline from the
// profile's external identifiers and traits. Each external identifier addition
// generates an "identify" event, each merge generates a "merge" event, and
// each trait generates a "trait_update" event using its updated_at timestamp.
func (h *Handler) buildProfileEvents(profile *storage.ProfileData) []profileEvent {
	events := make([]profileEvent, 0, len(profile.ExternalIDs)+len(profile.Traits))

	for _, eid := range profile.ExternalIDs {
		events = append(events, profileEvent{
			Type:      "identify",
			Timestamp: eid.CreatedAt,
			Source:    eid.CreatedSource,
			Details: map[string]string{
				"external_id_type":  eid.ExternalIDType,
				"external_id_value": eid.ExternalIDValue,
			},
		})
		// If the external ID was merged from another segment, add a merge event.
		if eid.MergedAt != nil {
			events = append(events, profileEvent{
				Type:      "merge",
				Timestamp: *eid.MergedAt,
				Source:    eid.CreatedSource,
				Details: map[string]string{
					"external_id_type":  eid.ExternalIDType,
					"external_id_value": eid.ExternalIDValue,
					"action":            "merged",
				},
			})
		}
	}

	for _, trait := range profile.Traits {
		events = append(events, profileEvent{
			Type:      "trait_update",
			Timestamp: trait.UpdatedAt,
			Details: map[string]string{
				"key":   trait.Key,
				"value": trait.Value,
			},
		})
	}

	return events
}

// getProfileExternalIDs handles GET /v1/profiles/{id}/external_ids — returns
// all external identifiers linked to the identity segment.
// Returns 404 if the profile does not exist (per AAP E-027 requirement:
// "Verify proper error responses (404 for missing profile)").
func (h *Handler) getProfileExternalIDs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	segmentID, err := h.parseProfileID(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	// Verify that the profile segment exists before returning external IDs.
	profileData, fetchErr := h.fetchProfileFromGraph(ctx, segmentID)
	if fetchErr != nil {
		h.logger.Errorn("Error checking profile existence for external IDs",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(fetchErr),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch external IDs")
		return
	}
	if profileData == nil {
		h.writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
		return
	}

	externalIDs, err := h.graphService.GetSegmentIdentifiers(ctx, segmentID)
	if err != nil {
		h.logger.Errorn("Error fetching external IDs",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch external IDs")
		return
	}

	// Convert storage types to API response types.
	resp := make([]ExternalIDResponse, 0, len(externalIDs))
	for _, eid := range externalIDs {
		resp = append(resp, ExternalIDResponse{
			Type:      eid.ExternalIDType,
			Value:     eid.ExternalIDValue,
			Source:    eid.CreatedSource,
			CreatedAt: eid.CreatedAt,
			MergedAt:  eid.MergedAt,
		})
	}

	h.writeJSON(w, http.StatusOK, resp)
}

// getProfileMetadata handles GET /v1/profiles/{id}/metadata — returns profile
// metadata including summary counts of identifiers and traits.
func (h *Handler) getProfileMetadata(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	segmentID, err := h.parseProfileID(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
		return
	}

	// Retrieve the full profile to compute identifier and trait counts.
	profileData, err := h.fetchProfileFromGraph(ctx, segmentID)
	if err != nil {
		h.logger.Errorn("Error fetching profile metadata",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch metadata")
		return
	}
	if profileData == nil {
		h.writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("profile %d not found", segmentID))
		return
	}

	resp := MetadataResponse{
		ID:              profileData.Segment.ID,
		WorkspaceID:     profileData.Segment.WorkspaceID,
		SegmentID:       profileData.Segment.SegmentID,
		CreatedAt:       profileData.Segment.CreatedAt,
		IdentifierCount: len(profileData.ExternalIDs),
		TraitCount:      len(profileData.Traits),
	}

	h.writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Helper Methods
// ---------------------------------------------------------------------------

// parseProfileID extracts the profile (segment) ID from the chi URL parameter
// and validates it. Returns the parsed int64 segment ID or an error if the ID
// is missing, non-numeric, or non-positive.
func (h *Handler) parseProfileID(r *http.Request) (int64, error) {
	idStr := chi.URLParam(r, "id")
	if idStr == "" {
		return 0, fmt.Errorf("missing profile ID")
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid profile ID %q: must be a numeric segment ID", idStr)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid profile ID %d: must be positive", id)
	}
	return id, nil
}

// fetchProfileFromGraph queries the graph service to assemble a complete
// profile. It delegates to graph.Service.GetProfileData which retrieves the
// segment, external IDs, and traits in a single composite call.
//
// Returns (nil, nil) if the profile segment does not exist.
// Returns (nil, error) on service failures.
func (h *Handler) fetchProfileFromGraph(ctx context.Context, segmentID int64) (*storage.ProfileData, error) {
	profileData, err := h.graphService.GetProfileData(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("graph service GetProfileData: %w", err)
	}
	return profileData, nil
}

// writeJSON writes a JSON response with the given status code and payload.
// Uses jsonrs (not encoding/json) per codebase linting rules in .golangci.yml.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := jsonrs.NewEncoder(w).Encode(data); err != nil {
		h.logger.Errorn("Error encoding JSON response", obskit.Error(err))
	}
}

// writeError writes a standardized error JSON response using ErrorResponse.
func (h *Handler) writeError(w http.ResponseWriter, status int, errorType, message string) {
	h.writeJSON(w, status, ErrorResponse{
		Error:   errorType,
		Message: message,
	})
}

// writeProfileResponse converts storage.ProfileData to the API response
// format and writes it as JSON. Initializes slices with make() to produce
// [] (not null) in JSON output for empty collections.
func (h *Handler) writeProfileResponse(w http.ResponseWriter, data *storage.ProfileData) {
	resp := ProfileResponse{
		Segment: SegmentResponse{
			ID:          data.Segment.ID,
			WorkspaceID: data.Segment.WorkspaceID,
			SegmentID:   data.Segment.SegmentID,
			CreatedAt:   data.Segment.CreatedAt,
		},
		ExternalIDs: make([]ExternalIDResponse, 0, len(data.ExternalIDs)),
		Traits:      make([]TraitResponse, 0, len(data.Traits)),
	}
	for _, eid := range data.ExternalIDs {
		resp.ExternalIDs = append(resp.ExternalIDs, ExternalIDResponse{
			Type:      eid.ExternalIDType,
			Value:     eid.ExternalIDValue,
			Source:    eid.CreatedSource,
			CreatedAt: eid.CreatedAt,
			MergedAt:  eid.MergedAt,
		})
	}
	for _, trait := range data.Traits {
		resp.Traits = append(resp.Traits, TraitResponse{
			Key:       trait.Key,
			Value:     trait.Value,
			UpdatedAt: trait.UpdatedAt,
		})
	}
	h.writeJSON(w, http.StatusOK, resp)
}
