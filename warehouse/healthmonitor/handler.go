package healthmonitor

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// HealthRepository defines the data access interface for health monitoring.
// Implementations must be safe for concurrent access from multiple goroutines.
// The interface is consumed primarily by HealthHandler for serving HTTP API responses,
// and by HealthMonitor for periodic metric collection and record management.
type HealthRepository interface {
	// RecordSyncHealth persists a sync health record to the wh_sync_health table.
	// The health parameter must have all required fields populated except ID,
	// which is set by the database on insert.
	RecordSyncHealth(ctx context.Context, health *SyncHealth) error

	// GetHealthSummary returns an aggregated health summary across all source/destination
	// pairs within a recent time window. The returned HealthSummaryResponse groups
	// destinations by source for dashboard consumption.
	GetHealthSummary(ctx context.Context) (*HealthSummaryResponse, error)

	// GetHealthBySourceDest returns health data filtered by a specific source-destination
	// pair. Returns nil with no error if no health records exist for the given pair.
	GetHealthBySourceDest(ctx context.Context, sourceID, destID string) (*SourceHealthResponse, error)

	// GetHealthByUpload returns the sync health record for a specific upload ID.
	// Returns ErrHealthNotFound if no record exists for the given upload.
	GetHealthByUpload(ctx context.Context, uploadID int64) (*SyncHealth, error)

	// PurgeOldRecords deletes health records created before the given cutoff time.
	// Returns the number of records deleted. Used by the health monitor's periodic
	// retention cleanup to prevent unbounded table growth.
	PurgeOldRecords(ctx context.Context, before time.Time) (int64, error)
}

// HealthHandler handles HTTP requests for warehouse health monitoring endpoints.
// It serves the GET /v1/warehouse/health and GET /v1/warehouse/health/{sourceID}/{destID}
// endpoints, delegating data retrieval to the HealthRepository interface.
//
// The handler follows the Chi HTTP handler pattern established in warehouse/api/http.go,
// using jsonrs for all JSON serialization (never encoding/json), structured logging
// with obskit error labels, and standard HTTP status codes for error responses.
type HealthHandler struct {
	logger     logger.Logger
	repository HealthRepository
}

// NewHealthHandler creates a new HealthHandler with the given dependencies.
// The logger is wrapped with a child namespace "healthmonitor.handler" for
// structured log filtering, following the warehouse package logging convention.
func NewHealthHandler(log logger.Logger, repo HealthRepository) *HealthHandler {
	return &HealthHandler{
		logger:     log.Child("healthmonitor.handler"),
		repository: repo,
	}
}

// GetHealthSummary handles GET /v1/warehouse/health requests.
// It returns an aggregated health summary across all source/destination pairs
// in the JSON format specified by the AAP:
//
//	{
//	    "sources": [{
//	        "sourceID": "...",
//	        "destinations": [{
//	            "destID": "...",
//	            "syncDuration": {"min": 0, "max": 0, "avg": 0, "p95": 0},
//	            "rowsSynced": 12345,
//	            "errorRate": 0.02,
//	            "lastSync": "2024-01-01T00:00:00Z"
//	        }]
//	    }]
//	}
//
// On success, returns HTTP 200 with the JSON health summary.
// On repository error, returns HTTP 500 with a structured error response.
func (h *HealthHandler) GetHealthSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	summary, err := h.repository.GetHealthSummary(ctx)
	if err != nil {
		h.logger.Errorn("failed to get health summary", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to retrieve health summary")
		return
	}

	// Ensure the sources slice is never nil so JSON serialization produces
	// an empty array [] instead of null when no health data is available.
	if summary != nil && summary.Sources == nil {
		summary.Sources = make([]*SourceHealth, 0)
	}

	h.writeJSONResponse(w, http.StatusOK, summary)
}

// GetHealthBySourceDest handles GET /v1/warehouse/health/{sourceID}/{destID} requests.
// It returns health data filtered for a specific source-destination pair.
//
// URL Parameters:
//   - sourceID: The RudderStack source identifier (required, non-empty)
//   - destID: The warehouse destination identifier (required, non-empty)
//
// On success, returns HTTP 200 with the filtered health data.
// Returns HTTP 400 if sourceID or destID are missing/empty.
// Returns HTTP 500 on repository errors.
func (h *HealthHandler) GetHealthBySourceDest(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "sourceID")
	destID := chi.URLParam(r, "destID")

	if sourceID == "" || destID == "" {
		h.writeErrorResponse(w, http.StatusBadRequest, "sourceID and destID are required")
		return
	}

	ctx := r.Context()

	health, err := h.repository.GetHealthBySourceDest(ctx, sourceID, destID)
	if err != nil {
		h.logger.Errorn("failed to get health by source/dest", obskit.Error(err))
		h.writeErrorResponse(w, http.StatusInternalServerError, "failed to retrieve health data")
		return
	}

	// Return an empty response with zero-value fields instead of null JSON body
	// when no health data exists, matching GetHealthSummary's empty-array convention.
	if health == nil {
		health = &SourceHealthResponse{
			SourceID:     sourceID,
			Destinations: make([]*DestinationHealth, 0),
		}
	}

	h.writeJSONResponse(w, http.StatusOK, health)
}

// writeJSONResponse writes a JSON response with the given HTTP status code and payload.
// It sets the Content-Type header to application/json before writing the status code
// to ensure correct header ordering per HTTP specification.
// Uses jsonrs.NewEncoder for all JSON serialization per .golangci.yml depguard rules.
func (h *HealthHandler) writeJSONResponse(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = jsonrs.NewEncoder(w).Encode(payload)
}

// writeErrorResponse writes a structured JSON error response with the given HTTP status
// code and error message. The response follows the standard warehouse API error format:
//
//	{"status": "error", "message": "<error description>"}
//
// This pattern is consistent with error responses in warehouse/api/http.go.
func (h *HealthHandler) writeErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = jsonrs.NewEncoder(w).Encode(map[string]string{
		"status":  "error",
		"message": message,
	})
}
