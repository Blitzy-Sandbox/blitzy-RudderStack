// Package monitoring provides per-destination delivery metrics aggregation and
// an HTTP API handler for the operational tooling sprint (E-036). The
// DashboardService maintains a thread-safe in-memory map of per-destination
// delivery metrics, computes sliding-window throughput and approximate latency
// percentiles, and serves a JSON API for operational dashboards.
package monitoring

import (
	"context"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// ---------------------------------------------------------------------------
// Circuit-breaker state constants
// ---------------------------------------------------------------------------

const (
	// CircuitBreakerClosed indicates a healthy destination accepting traffic.
	CircuitBreakerClosed = 0
	// CircuitBreakerOpen indicates an unhealthy destination blocking traffic.
	CircuitBreakerOpen = 1
	// CircuitBreakerHalfOpen indicates a recovery-probe phase allowing limited traffic.
	CircuitBreakerHalfOpen = 2
)

// defaultLatencyCapacity is the maximum number of latency observations kept per
// destination in the sliding window. Once this limit is reached the oldest
// observations are evicted to make room for new ones. 10 000 observations are
// sufficient to derive accurate percentiles for a single aggregation interval.
const defaultLatencyCapacity = 10_000

// ---------------------------------------------------------------------------
// DestinationMetrics is the per-destination metrics snapshot returned in API
// responses. All fields are exported for JSON serialisation.
// ---------------------------------------------------------------------------

// DestinationMetrics holds aggregated delivery metrics for a single
// destination. Field names are stable and part of the public HTTP API.
type DestinationMetrics struct {
	DestinationID       string    `json:"destination_id"`
	DestinationType     string    `json:"destination_type"`
	SuccessCount        int64     `json:"success_count"`
	FailureCount        int64     `json:"failure_count"`
	LatencyP50Ms        float64   `json:"latency_p50_ms"`
	LatencyP95Ms        float64   `json:"latency_p95_ms"`
	LatencyP99Ms        float64   `json:"latency_p99_ms"`
	ThroughputPerSec    float64   `json:"throughput_per_sec"`
	RetryCount          int64     `json:"retry_count"`
	CircuitBreakerState int       `json:"circuit_breaker_state"`
	LastUpdated         time.Time `json:"last_updated"`
}

// DashboardResponse is the top-level API response returned by the
// DashboardHandler. It wraps a list of per-destination metric snapshots with
// the server timestamp at query time.
type DashboardResponse struct {
	Destinations []DestinationMetrics `json:"destinations"`
	Timestamp    time.Time            `json:"timestamp"`
}

// ---------------------------------------------------------------------------
// Internal sliding-window latency tracker
// ---------------------------------------------------------------------------

// latencyWindow maintains a bounded, append-only ring of latency observations
// for a single destination. It is NOT thread-safe; the caller must hold
// DashboardService.mu.
type latencyWindow struct {
	values []float64 // millisecond durations
	pos    int       // next write position (ring)
	full   bool      // whether the ring has wrapped at least once
	cap    int       // maximum number of observations
}

// newLatencyWindow returns a latencyWindow with the given capacity.
func newLatencyWindow(cap int) *latencyWindow {
	return &latencyWindow{
		values: make([]float64, cap),
		cap:    cap,
	}
}

// record adds a latency observation (in milliseconds) to the window.
func (lw *latencyWindow) record(ms float64) {
	lw.values[lw.pos] = ms
	lw.pos++
	if lw.pos >= lw.cap {
		lw.pos = 0
		lw.full = true
	}
}

// percentile returns the p-th percentile (0 ≤ p ≤ 1) of the observations
// currently in the window. If the window is empty it returns 0.
func (lw *latencyWindow) percentile(p float64) float64 {
	n := lw.size()
	if n == 0 {
		return 0
	}

	// Copy the active slice so sorting does not disturb the ring buffer.
	active := make([]float64, n)
	if lw.full {
		copy(active, lw.values)
	} else {
		copy(active, lw.values[:lw.pos])
	}
	sort.Float64s(active)

	// Nearest-rank method for percentile calculation.
	rank := p * float64(n-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	if lower == upper || upper >= n {
		return active[lower]
	}
	frac := rank - float64(lower)
	return active[lower]*(1-frac) + active[upper]*frac
}

// size returns the number of valid observations.
func (lw *latencyWindow) size() int {
	if lw.full {
		return lw.cap
	}
	return lw.pos
}

// ---------------------------------------------------------------------------
// internalDestinationState holds the mutable state for a single destination.
// ---------------------------------------------------------------------------

type internalDestinationState struct {
	metrics  DestinationMetrics
	latency  *latencyWindow
	prevTotal int64 // previous (success + failure) used for throughput delta
}

// ---------------------------------------------------------------------------
// DashboardService aggregates per-destination delivery metrics in memory and
// exposes them via an HTTP API for operational dashboards.
// ---------------------------------------------------------------------------

// DashboardService is the core monitoring service. It maintains a map of
// per-destination delivery metrics, periodically computes derived values
// (throughput, latency percentiles), cleans up stale entries, and serves a
// JSON API.
type DashboardService struct {
	mu                sync.RWMutex
	logger            logger.Logger
	destinations      map[string]*internalDestinationState
	aggregationWindow time.Duration
	retentionPeriod   time.Duration
	ticker            *time.Ticker
	cancel            context.CancelFunc
}

// NewDashboardService creates a new DashboardService. Configuration is read
// from the provided config.Config following the rudder-go-kit configuration
// pattern (see router/handle.go):
//
//   - Monitoring.aggregationWindowSeconds — aggregation tick interval (default 60s)
//   - Monitoring.retentionPeriodSeconds — metric retention before eviction (default 3600s / 1 hour)
//
// A scoped child logger is created via log.Child("monitoring") following the
// pattern in services/alert/alertmanager.go.
func NewDashboardService(conf *config.Config, log logger.Logger) *DashboardService {
	aggregationWindow := conf.GetDurationVar(
		60, time.Second,
		"Monitoring.aggregationWindowSeconds",
	)
	retentionPeriod := conf.GetDurationVar(
		3600, time.Second,
		"Monitoring.retentionPeriodSeconds",
	)

	return &DashboardService{
		logger:            log.Child("monitoring"),
		destinations:      make(map[string]*internalDestinationState),
		aggregationWindow: aggregationWindow,
		retentionPeriod:   retentionPeriod,
	}
}

// ---------------------------------------------------------------------------
// Lifecycle — Start / Stop
// ---------------------------------------------------------------------------

// Start launches a background goroutine that periodically:
//   - computes throughput (events/sec) per destination
//   - recalculates latency percentiles (p50, p95, p99)
//   - removes metrics older than the retention period
//
// The goroutine respects context cancellation and the Stop signal.
func (ds *DashboardService) Start(ctx context.Context) error {
	derivedCtx, cancel := context.WithCancel(ctx)
	ds.cancel = cancel
	ds.ticker = time.NewTicker(ds.aggregationWindow)

	go ds.aggregationLoop(derivedCtx)

	ds.logger.Infon("Dashboard service started")
	return nil
}

// Stop gracefully shuts down the background aggregation goroutine and releases
// ticker resources.
func (ds *DashboardService) Stop() {
	if ds.cancel != nil {
		ds.cancel()
	}
	if ds.ticker != nil {
		ds.ticker.Stop()
	}
	ds.logger.Infon("Dashboard service stopped")
}

// aggregationLoop runs in its own goroutine, triggered by the ticker. Each
// tick recomputes derived metrics and evicts stale destinations.
func (ds *DashboardService) aggregationLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ds.ticker.C:
			ds.computeDerivedMetrics()
			ds.cleanup()
		}
	}
}

// ---------------------------------------------------------------------------
// Derived metric computation
// ---------------------------------------------------------------------------

// computeDerivedMetrics recalculates throughput and latency percentiles for
// every destination.
func (ds *DashboardService) computeDerivedMetrics() {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	windowSec := ds.aggregationWindow.Seconds()
	if windowSec <= 0 {
		windowSec = 1
	}

	for _, state := range ds.destinations {
		currentTotal := state.metrics.SuccessCount + state.metrics.FailureCount
		delta := currentTotal - state.prevTotal
		if delta < 0 {
			delta = 0
		}
		state.metrics.ThroughputPerSec = float64(delta) / windowSec
		state.prevTotal = currentTotal

		// Latency percentiles from the sliding window.
		if state.latency != nil && state.latency.size() > 0 {
			state.metrics.LatencyP50Ms = state.latency.percentile(0.50)
			state.metrics.LatencyP95Ms = state.latency.percentile(0.95)
			state.metrics.LatencyP99Ms = state.latency.percentile(0.99)
		}
	}
}

// ---------------------------------------------------------------------------
// Cleanup — retention-based eviction
// ---------------------------------------------------------------------------

// cleanup removes destination entries whose LastUpdated timestamp is older
// than the configured retentionPeriod.
func (ds *DashboardService) cleanup() {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	now := time.Now()
	for id, state := range ds.destinations {
		if time.Since(state.metrics.LastUpdated) > ds.retentionPeriod {
			delete(ds.destinations, id)
			ds.logger.Debugn("Evicted stale destination metrics",
				obskit.DestinationID(id),
			)
		}
	}
	_ = now // suppress unused-variable warning for time.Now used via time.Since
}

// ---------------------------------------------------------------------------
// Public query API
// ---------------------------------------------------------------------------

// GetMetrics returns a DashboardResponse containing the current per-destination
// metrics. If one or more destinationID filters are provided, only matching
// destinations are included. When no filters are given, all destinations are
// returned.
func (ds *DashboardService) GetMetrics(destinationID ...string) *DashboardResponse {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	resp := &DashboardResponse{
		Timestamp: time.Now(),
	}

	if len(destinationID) > 0 && destinationID[0] != "" {
		// Build a set for efficient lookup.
		filter := make(map[string]struct{}, len(destinationID))
		for _, id := range destinationID {
			if id != "" {
				filter[id] = struct{}{}
			}
		}
		for id, state := range ds.destinations {
			if _, ok := filter[id]; ok {
				resp.Destinations = append(resp.Destinations, state.metrics)
			}
		}
	} else {
		resp.Destinations = make([]DestinationMetrics, 0, len(ds.destinations))
		for _, state := range ds.destinations {
			resp.Destinations = append(resp.Destinations, state.metrics)
		}
	}

	if resp.Destinations == nil {
		resp.Destinations = []DestinationMetrics{}
	}
	return resp
}

// ---------------------------------------------------------------------------
// Recording methods — called by metrics.go helpers and Router integration
// ---------------------------------------------------------------------------

// recordSuccess increments the success counter for a destination. It is safe
// for concurrent use.
func (ds *DashboardService) recordSuccess(destinationID, destinationType string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	state := ds.getOrCreateState(destinationID, destinationType)
	state.metrics.SuccessCount++
	state.metrics.LastUpdated = time.Now()
}

// recordFailure increments the failure counter for a destination.
func (ds *DashboardService) recordFailure(destinationID, destinationType string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	state := ds.getOrCreateState(destinationID, destinationType)
	state.metrics.FailureCount++
	state.metrics.LastUpdated = time.Now()
}

// recordLatency records a single latency observation for a destination. The
// observation is appended to the per-destination sliding window from which
// percentiles are derived during the next aggregation tick.
func (ds *DashboardService) recordLatency(destinationID, destinationType string, latency time.Duration) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	state := ds.getOrCreateState(destinationID, destinationType)
	ms := float64(latency.Milliseconds())
	if ms < 0 {
		ms = 0
	}
	state.latency.record(ms)
	state.metrics.LastUpdated = time.Now()
}

// recordRetry increments the retry counter for a destination.
func (ds *DashboardService) recordRetry(destinationID, destinationType string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	state := ds.getOrCreateState(destinationID, destinationType)
	state.metrics.RetryCount++
	state.metrics.LastUpdated = time.Now()
}

// recordCircuitBreakerState sets the circuit-breaker gauge for a destination.
//
//	0 = closed (healthy)
//	1 = open (unhealthy)
//	2 = half-open (recovery probe)
func (ds *DashboardService) recordCircuitBreakerState(destinationID, destinationType string, state int) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	s := ds.getOrCreateState(destinationID, destinationType)
	s.metrics.CircuitBreakerState = state
	s.metrics.LastUpdated = time.Now()
}

// getOrCreateState returns the internal state entry for the given destination,
// creating it if it does not yet exist. The caller MUST hold ds.mu (write).
func (ds *DashboardService) getOrCreateState(destinationID, destinationType string) *internalDestinationState {
	state, ok := ds.destinations[destinationID]
	if !ok {
		state = &internalDestinationState{
			metrics: DestinationMetrics{
				DestinationID:   destinationID,
				DestinationType: destinationType,
			},
			latency: newLatencyWindow(defaultLatencyCapacity),
		}
		ds.destinations[destinationID] = state
	}
	// Always keep the destination type current.
	state.metrics.DestinationType = destinationType
	return state
}

// ---------------------------------------------------------------------------
// HTTP Handler
// ---------------------------------------------------------------------------

// DashboardHandler serves the monitoring dashboard JSON API. An optional
// "destinationId" query parameter may be provided to filter results to a
// single destination.
//
// Response Content-Type: application/json
// Response body: DashboardResponse (see struct definition)
//
// On encoding errors the handler returns HTTP 500 with an error body and logs
// the error with structured fields via obskit.Error.
func (ds *DashboardService) DashboardHandler(w http.ResponseWriter, r *http.Request) {
	destIDFilter := r.URL.Query().Get("destinationId")

	var resp *DashboardResponse
	if destIDFilter != "" {
		resp = ds.GetMetrics(destIDFilter)
	} else {
		resp = ds.GetMetrics()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := jsonrs.NewEncoder(w).Encode(resp); err != nil {
		ds.logger.Errorn("Failed to encode dashboard response", obskit.Error(err))
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
}

// RegisterRoutes mounts the monitoring dashboard API endpoint on the provided
// chi.Router. The route follows the gateway /v1/* path convention:
//
//	GET /v1/monitoring/dashboard[?destinationId=<id>]
func (ds *DashboardService) RegisterRoutes(r chi.Router) {
	r.Get("/v1/monitoring/dashboard", ds.DashboardHandler)
}
