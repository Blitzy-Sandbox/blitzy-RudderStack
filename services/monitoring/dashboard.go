// Package monitoring provides per-destination delivery metrics aggregation and
// an HTTP API handler for the operational tooling sprint (E-036). The
// DashboardService maintains a thread-safe in-memory map of per-destination
// delivery metrics, computes sliding-window throughput and approximate latency
// percentiles, and serves a JSON API for operational dashboards.
package monitoring

import (
	"bytes"
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

// percentiles computes multiple percentiles (0 ≤ p ≤ 1) in a single pass by
// copying and sorting the active observations once. This avoids the O(N log N)
// cost per percentile that occurs when calling percentile() individually.
func (lw *latencyWindow) percentiles(ps ...float64) []float64 {
	n := lw.size()
	results := make([]float64, len(ps))
	if n == 0 {
		return results
	}

	// Copy the active slice so sorting does not disturb the ring buffer.
	active := make([]float64, n)
	if lw.full {
		copy(active, lw.values)
	} else {
		copy(active, lw.values[:lw.pos])
	}
	sort.Float64s(active)

	for i, p := range ps {
		// Nearest-rank method for percentile calculation.
		rank := p * float64(n-1)
		lower := int(math.Floor(rank))
		upper := int(math.Ceil(rank))
		if lower == upper || upper >= n {
			results[i] = active[lower]
		} else {
			frac := rank - float64(lower)
			results[i] = active[lower]*(1-frac) + active[upper]*frac
		}
	}
	return results
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
	done              chan struct{} // closed when aggregationLoop exits
}

// Dashboard is a type alias for DashboardService. It is exported for use by
// runner/runner.go where the Runner struct field is typed as *monitoring.Dashboard.
// This alias avoids a naming break while maintaining the descriptive DashboardService
// name in the monitoring package's own code.
type Dashboard = DashboardService

// NewDashboardService creates a new DashboardService. Configuration is read
// from the provided config.Config following the rudder-go-kit configuration
// pattern. Config keys match the Monitoring section in config.yaml:
//
//   - Monitoring.dashboard.refreshInterval — aggregation tick interval (default 10s)
//   - Monitoring.dashboard.retentionPeriod — metric retention before eviction (default 86400s / 24 hours)
//
// A scoped child logger is created via log.Child("monitoring") following the
// pattern in services/alert/alertmanager.go.
//
// The service registers itself as the package-level default dashboard via
// RegisterDashboard so that recording helpers in metrics.go automatically
// bridge Prometheus writes into the in-memory dashboard state.
func NewDashboardService(conf *config.Config, log logger.Logger) *DashboardService {
	if conf == nil {
		conf = config.Default
	}
	if log == nil {
		log = pkgLogger
	}
	aggregationWindow := conf.GetDurationVar(
		10, time.Second,
		"Monitoring.dashboard.refreshInterval",
	)
	retentionPeriod := conf.GetDurationVar(
		86400, time.Second,
		"Monitoring.dashboard.retentionPeriod",
	)

	ds := &DashboardService{
		logger:            log.Child("monitoring"),
		destinations:      make(map[string]*internalDestinationState),
		aggregationWindow: aggregationWindow,
		retentionPeriod:   retentionPeriod,
	}

	// Bridge: register this dashboard so package-level recording helpers
	// (RecordDelivery, RecordFailure, etc.) also populate the in-memory state.
	RegisterDashboard(ds)

	return ds
}

// NewDashboard creates a new Dashboard (alias for DashboardService) with the
// standard 3-argument constructor signature expected by runner/runner.go (E-036).
// The stats parameter is accepted for API uniformity but not used — the
// DashboardService bridges Prometheus metrics to in-memory state internally
// via RegisterDashboard / metrics.go helpers. If stats is needed in the future,
// it can be wired here without changing the runner.
func NewDashboard(conf *config.Config, log logger.Logger, _ interface{}) *Dashboard {
	return NewDashboardService(conf, log)
}

// ---------------------------------------------------------------------------
// Lifecycle — Start / Stop / Run
// ---------------------------------------------------------------------------

// Run starts the dashboard service and blocks until ctx is cancelled.
// This is the lifecycle method called by runner/runner.go's errgroup to
// start the monitoring dashboard as a managed goroutine alongside the
// existing Gateway, Processor, Router, and Warehouse services.
func (ds *DashboardService) Run(ctx context.Context) error {
	if err := ds.Start(ctx); err != nil {
		return err
	}
	// Block until context cancellation — mirrors the pattern in
	// identity/graph/graph.go:Run.
	<-ctx.Done()
	return ctx.Err()
}

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
	ds.done = make(chan struct{})

	go ds.aggregationLoop(derivedCtx)

	ds.logger.Infon("Dashboard service started")
	return nil
}

// Stop gracefully shuts down the background aggregation goroutine and releases
// ticker resources. It blocks until the aggregation goroutine has fully exited,
// ensuring safe re-use of the service via a subsequent Start call.
func (ds *DashboardService) Stop() {
	if ds.cancel != nil {
		ds.cancel()
	}
	if ds.done != nil {
		<-ds.done // wait for aggregationLoop goroutine to exit
	}
	if ds.ticker != nil {
		ds.ticker.Stop()
	}
	ds.logger.Infon("Dashboard service stopped")
}

// aggregationLoop runs in its own goroutine, triggered by the ticker. Each
// tick recomputes derived metrics and evicts stale destinations.
func (ds *DashboardService) aggregationLoop(ctx context.Context) {
	defer close(ds.done)
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

		// Latency percentiles from the sliding window — computed in a single
		// pass to avoid sorting the observation slice three times.
		if state.latency != nil && state.latency.size() > 0 {
			pcts := state.latency.percentiles(0.50, 0.95, 0.99)
			state.metrics.LatencyP50Ms = pcts[0]
			state.metrics.LatencyP95Ms = pcts[1]
			state.metrics.LatencyP99Ms = pcts[2]
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
		if now.Sub(state.metrics.LastUpdated) > ds.retentionPeriod {
			delete(ds.destinations, id)
			ds.logger.Debugn("Evicted stale destination metrics",
				obskit.DestinationID(id),
			)
		}
	}
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

// RecordSuccess increments the success counter for a destination. It is safe
// for concurrent use. Exported so that external packages (e.g., router) can
// call it directly or via the monitoring.RecordDelivery helper.
func (ds *DashboardService) RecordSuccess(destinationType, destinationID string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	state := ds.getOrCreateState(destinationID, destinationType)
	state.metrics.SuccessCount++
	state.metrics.LastUpdated = time.Now()
}

// RecordFailure increments the failure counter for a destination.
// Exported so that external packages can call it directly or via the
// monitoring.RecordFailure helper.
func (ds *DashboardService) RecordFailure(destinationType, destinationID string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	state := ds.getOrCreateState(destinationID, destinationType)
	state.metrics.FailureCount++
	state.metrics.LastUpdated = time.Now()
}

// RecordLatency records a single latency observation for a destination. The
// observation is appended to the per-destination sliding window from which
// percentiles are derived during the next aggregation tick.
// Exported so that external packages can call it directly or via the
// monitoring.RecordLatency helper.
func (ds *DashboardService) RecordLatency(destinationType, destinationID string, latency time.Duration) {
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

// RecordRetry increments the retry counter for a destination.
// Exported so that external packages can call it directly or via the
// monitoring.RecordRetry helper.
func (ds *DashboardService) RecordRetry(destinationType, destinationID string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	state := ds.getOrCreateState(destinationID, destinationType)
	state.metrics.RetryCount++
	state.metrics.LastUpdated = time.Now()
}

// RecordCircuitBreakerState sets the circuit-breaker gauge for a destination.
// The stateStr parameter is one of "closed", "open", or "half-open".
// Exported so that external packages can call it directly or via the
// monitoring.RecordCircuitBreakerState helper.
func (ds *DashboardService) RecordCircuitBreakerState(destinationType, destinationID string, stateStr string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	s := ds.getOrCreateState(destinationID, destinationType)
	switch stateStr {
	case "open":
		s.metrics.CircuitBreakerState = CircuitBreakerOpen
	case "half-open":
		s.metrics.CircuitBreakerState = CircuitBreakerHalfOpen
	default:
		s.metrics.CircuitBreakerState = CircuitBreakerClosed
	}
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

	// Buffer the JSON response so that a partial write does not produce a
	// corrupted response followed by an error body on the same connection.
	var buf bytes.Buffer
	if err := jsonrs.NewEncoder(&buf).Encode(resp); err != nil {
		ds.logger.Errorn("Failed to encode dashboard response", obskit.Error(err))
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}

// RegisterRoutes mounts the monitoring dashboard API endpoint on the provided
// chi.Router. The route follows the gateway /v1/* path convention:
//
//	GET /v1/monitoring/dashboard[?destinationId=<id>]
func (ds *DashboardService) RegisterRoutes(r chi.Router) {
	r.Get("/v1/monitoring/dashboard", ds.DashboardHandler)
}
