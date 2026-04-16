// Package profiling provides per-stage pipeline performance profiling for the
// RudderStack event pipeline. It collects per-stage latency measurements across
// the full pipeline (Gateway → Processor stages → Router → Warehouse),
// aggregates timing data into performance profiles with p50/p95/p99 latency
// percentiles, and exposes an HTTP API endpoint for retrieving pipeline
// performance data. This is part of Sprint 8-10, E-039.
package profiling

import (
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// Pipeline stage constants aligned with processor/pipeline_worker.go stages
// and extended with Gateway, Router, and Warehouse stages per E-039.
const (
	StageGateway       = "gateway"
	StagePreprocess    = "preprocess"
	StageSrcHydration  = "srcHydration"
	StagePreTransform  = "preTransform"
	StageUserTransform = "userTransform"
	StageDestTransform = "destTransform"
	StageStore         = "store"
	StageRouter        = "router"
	StageWarehouse     = "warehouse"
)

// AllStages lists all known pipeline stages in execution order. The first seven
// stages represent the linear pipeline (Gateway → Store), while Router and
// Warehouse stages operate independently after events are persisted.
var AllStages = []string{
	StageGateway,
	StagePreprocess,
	StageSrcHydration,
	StagePreTransform,
	StageUserTransform,
	StageDestTransform,
	StageStore,
	StageRouter,
	StageWarehouse,
}

// ProfileReport is the primary report output containing per-stage latency
// profiles and an aggregate end-to-end pipeline P99 latency estimate. JSON
// tags use the _ns suffix to clarify that Go's time.Duration is serialised as
// nanoseconds by default.
type ProfileReport struct {
	Timestamp        time.Time               `json:"timestamp"`
	Stages           map[string]StageProfile `json:"stages"`
	TotalPipelineP99 time.Duration           `json:"total_pipeline_p99_ns"`
}

// StageProfile holds aggregated latency statistics for a single pipeline stage.
type StageProfile struct {
	P50         time.Duration `json:"p50_ns"`
	P95         time.Duration `json:"p95_ns"`
	P99         time.Duration `json:"p99_ns"`
	Mean        time.Duration `json:"mean_ns"`
	Min         time.Duration `json:"min_ns"`
	Max         time.Duration `json:"max_ns"`
	SampleCount int           `json:"sample_count"`
}

// maxSamplesPerStage limits the number of latency samples kept in memory per
// pipeline stage. When exceeded, the oldest samples are evicted (FIFO). This
// prevents unbounded memory growth under sustained load. The value 10000 gives
// sufficient statistical accuracy for percentile computations while keeping the
// per-stage buffer under ~160 KB (10K × 16 bytes per time.Duration).
const maxSamplesPerStage = 10000

// Profiler collects per-stage latency samples, computes percentile profiles,
// and exposes the data via an HTTP handler. All public methods are safe for
// concurrent use.
type Profiler struct {
	mu           sync.RWMutex
	stageData    map[string][]time.Duration // stage name → latency samples
	logger       logger.Logger
	statsFactory stats.Stats
	samplingRate config.ValueLoader[int]  // percentage of events to sample (1-100)
	enabled      config.ValueLoader[bool] // master enable toggle
}

// NewProfiler creates a Profiler initialised with default configuration.
//
// Configuration keys (hot-reloadable, matching config.yaml Monitoring section):
//   - Monitoring.profiling.enabled    – master toggle (default: true)
//   - Monitoring.profiling.sampleRate – percentage of events to sample, 1-100 (default: 100)
func NewProfiler() *Profiler {
	return &Profiler{
		stageData:    make(map[string][]time.Duration),
		logger:       logger.NewLogger().Child("profiling"),
		statsFactory: stats.Default,
		samplingRate: config.GetReloadableIntVar(100, 1, "Monitoring.profiling.sampleRate"),
		enabled:      config.GetReloadableBoolVar(true, "Monitoring.profiling.enabled"),
	}
}

// RecordStageLatency records a single latency observation for the given
// pipeline stage. The sample is appended to the in-memory buffer (subject to
// sampling rate) and also emitted to Prometheus via the stats infrastructure
// (pipeline_stage_latency timer tagged with the stage name).
//
// Recording is a no-op when profiling is disabled. Only a configurable
// percentage of events (Monitoring.profiling.sampleRate) are buffered
// in-memory; the Prometheus timer always receives all observations regardless
// of sampling.
//
// The in-memory buffer is capped at maxSamplesPerStage per stage. When the cap
// is reached, the oldest samples are evicted (FIFO) to prevent unbounded
// memory growth under sustained load.
func (p *Profiler) RecordStageLatency(stage string, latency time.Duration) {
	if !p.enabled.Load() {
		return
	}

	// Always emit to Prometheus regardless of in-memory sampling.
	p.statsFactory.NewTaggedStat(
		"pipeline_stage_latency",
		stats.TimerType,
		stats.Tags{"stage": stage},
	).SendTiming(latency)

	// Apply sampling rate: only buffer a percentage of observations in memory
	// for percentile computation. The sampling rate is hot-reloadable via config.
	rate := p.samplingRate.Load()
	if rate < 100 {
		// Use a simple deterministic modulo check for low overhead. Each latency
		// value's nanosecond portion provides sufficient entropy for sampling.
		if int(latency.Nanoseconds()%100) >= rate {
			return
		}
	}

	// Append to in-memory buffer under write lock with capacity enforcement.
	p.mu.Lock()
	buf := p.stageData[stage]
	buf = append(buf, latency)
	// FIFO eviction: drop the oldest samples when buffer exceeds capacity.
	if len(buf) > maxSamplesPerStage {
		excess := len(buf) - maxSamplesPerStage
		copy(buf, buf[excess:])
		buf = buf[:maxSamplesPerStage]
	}
	p.stageData[stage] = buf
	p.mu.Unlock()
}

// GenerateReport computes a ProfileReport from all latency samples collected
// since the last Reset. The report includes per-stage percentile profiles and
// a TotalPipelineP99 estimate computed as the sum of P99 latencies across the
// linear pipeline stages (Gateway through Store). Router and Warehouse stages
// are tracked independently.
//
// After generating the report, Prometheus gauge metrics are updated for each
// stage.
func (p *Profiler) GenerateReport() *ProfileReport {
	// Snapshot the current data under read lock, then release the lock
	// before doing any heavy computation.
	p.mu.RLock()
	snapshot := make(map[string][]time.Duration, len(p.stageData))
	for stage, samples := range p.stageData {
		cp := make([]time.Duration, len(samples))
		copy(cp, samples)
		snapshot[stage] = cp
	}
	p.mu.RUnlock()

	stages := make(map[string]StageProfile, len(snapshot))
	for stage, data := range snapshot {
		stages[stage] = computeStageProfile(data)
	}

	// TotalPipelineP99: sum of P99 across the linear pipeline stages
	// (gateway → store). Router and Warehouse are independent and excluded
	// from the sum.
	linearStages := []string{
		StageGateway,
		StagePreprocess,
		StageSrcHydration,
		StagePreTransform,
		StageUserTransform,
		StageDestTransform,
		StageStore,
	}
	var totalP99 time.Duration
	for _, s := range linearStages {
		if sp, ok := stages[s]; ok {
			totalP99 += sp.P99
		}
	}

	report := &ProfileReport{
		Timestamp:        time.Now(),
		Stages:           stages,
		TotalPipelineP99: totalP99,
	}

	// Publish gauge metrics to Prometheus for dashboarding.
	p.recordPrometheusMetrics(report)

	return report
}

// Reset discards all collected latency samples, allowing a new measurement
// window to begin.
func (p *Profiler) Reset() {
	p.mu.Lock()
	p.stageData = make(map[string][]time.Duration)
	p.mu.Unlock()

	p.logger.Debugn("profiling data reset")
}

// Handler returns an http.HandlerFunc that serialises the current
// ProfileReport as JSON. The handler sets Content-Type to application/json and
// returns HTTP 200 on success or HTTP 500 if serialisation fails.
func (p *Profiler) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		report := p.GenerateReport()

		w.Header().Set("Content-Type", "application/json")
		if err := jsonrs.NewEncoder(w).Encode(report); err != nil {
			p.logger.Errorn("failed to encode profile report", obskit.Error(err))
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
}

// recordPrometheusMetrics publishes per-stage gauge metrics to the stats
// infrastructure. Gauges are expressed in seconds (float64) for Prometheus
// compatibility, following the pattern from router/handle_observability.go.
func (p *Profiler) recordPrometheusMetrics(report *ProfileReport) {
	for stage, profile := range report.Stages {
		tags := stats.Tags{"stage": stage}

		p.statsFactory.NewTaggedStat(
			"pipeline_profiling_p50_seconds",
			stats.GaugeType,
			tags,
		).Gauge(float64(profile.P50) / float64(time.Second))

		p.statsFactory.NewTaggedStat(
			"pipeline_profiling_p95_seconds",
			stats.GaugeType,
			tags,
		).Gauge(float64(profile.P95) / float64(time.Second))

		p.statsFactory.NewTaggedStat(
			"pipeline_profiling_p99_seconds",
			stats.GaugeType,
			tags,
		).Gauge(float64(profile.P99) / float64(time.Second))
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// computePercentile calculates the given percentile (0–100) from a slice of
// durations by sorting a copy and using linear interpolation. This is a
// convenience wrapper around percentileFromSorted for standalone callers and
// tests. For batch computation of multiple percentiles on the same data, prefer
// sorting once and calling percentileFromSorted directly (as computeStageProfile
// does) to avoid redundant sorting.
func computePercentile(data []time.Duration, percentile float64) time.Duration {
	if len(data) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(data))
	copy(sorted, data)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return percentileFromSorted(sorted, percentile)
}

// percentileFromSorted computes the given percentile (0–100) from a pre-sorted
// slice of durations using linear interpolation. The caller MUST provide sorted
// input; the function performs no validation or sorting of its own.
//
// Edge cases:
//   - empty data → 0
//   - single element → that element
func percentileFromSorted(sorted []time.Duration, percentile float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}

	// Linear interpolation between floor and ceil indices.
	idx := (percentile / 100.0) * float64(n-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))

	if lower == upper || upper >= n {
		return sorted[lower]
	}

	frac := idx - float64(lower)
	interpolated := float64(sorted[lower])*(1-frac) + float64(sorted[upper])*frac
	return time.Duration(interpolated)
}

// computeStageProfile builds a StageProfile from a slice of latency samples.
// It computes P50, P95, P99, Mean, Min, Max, and SampleCount. Returns a
// zero-valued StageProfile when the input is empty.
//
// The data is sorted exactly once and all three percentiles are extracted from
// the pre-sorted slice, avoiding the previous approach of re-sorting for each
// percentile computation.
func computeStageProfile(data []time.Duration) StageProfile {
	n := len(data)
	if n == 0 {
		return StageProfile{}
	}

	// Sort a copy once for all percentile computations.
	sorted := make([]time.Duration, n)
	copy(sorted, data)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	// Min and Max are at the ends of the sorted slice; Mean via summation.
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}

	return StageProfile{
		P50:         percentileFromSorted(sorted, 50),
		P95:         percentileFromSorted(sorted, 95),
		P99:         percentileFromSorted(sorted, 99),
		Mean:        sum / time.Duration(n),
		Min:         sorted[0],
		Max:         sorted[n-1],
		SampleCount: n,
	}
}
