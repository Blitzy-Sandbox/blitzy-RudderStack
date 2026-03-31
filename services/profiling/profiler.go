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
	Timestamp        time.Time                `json:"timestamp"`
	Stages           map[string]StageProfile  `json:"stages"`
	TotalPipelineP99 time.Duration            `json:"total_pipeline_p99_ns"`
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
// Configuration keys (hot-reloadable):
//   - Profiling.enabled   – master toggle (default: true)
//   - Profiling.samplingRate – percentage of events to sample, 1-100 (default: 100)
func NewProfiler() *Profiler {
	return &Profiler{
		stageData:    make(map[string][]time.Duration),
		logger:       logger.NewLogger().Child("profiling"),
		statsFactory: stats.Default,
		samplingRate: config.GetReloadableIntVar(100, 1, "Profiling.samplingRate"),
		enabled:      config.GetReloadableBoolVar(true, "Profiling.enabled"),
	}
}

// RecordStageLatency records a single latency observation for the given
// pipeline stage. The sample is appended to the in-memory buffer and also
// emitted to Prometheus via the stats infrastructure (pipeline_stage_latency
// timer tagged with the stage name).
//
// Recording is a no-op when profiling is disabled.
func (p *Profiler) RecordStageLatency(stage string, latency time.Duration) {
	if !p.enabled.Load() {
		return
	}

	// Append to in-memory buffer under write lock.
	p.mu.Lock()
	p.stageData[stage] = append(p.stageData[stage], latency)
	p.mu.Unlock()

	// Emit to Prometheus / stats infrastructure following the tagged stat
	// pattern from processor/trackingplan.go (lines 155-159) and
	// router/handle_observability.go (line 111).
	p.statsFactory.NewTaggedStat(
		"pipeline_stage_latency",
		stats.TimerType,
		stats.Tags{"stage": stage},
	).SendTiming(latency)
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
// durations using linear interpolation. A copy of the data is sorted so the
// caller's slice is not modified.
//
// Edge cases:
//   - empty data → 0
//   - single element → that element
func computePercentile(data []time.Duration, percentile float64) time.Duration {
	n := len(data)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return data[0]
	}

	// Sort a copy to avoid mutating the caller's slice.
	sorted := make([]time.Duration, n)
	copy(sorted, data)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

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
func computeStageProfile(data []time.Duration) StageProfile {
	n := len(data)
	if n == 0 {
		return StageProfile{}
	}

	var sum time.Duration
	minVal := data[0]
	maxVal := data[0]
	for _, d := range data {
		sum += d
		if d < minVal {
			minVal = d
		}
		if d > maxVal {
			maxVal = d
		}
	}

	return StageProfile{
		P50:         computePercentile(data, 50),
		P95:         computePercentile(data, 95),
		P99:         computePercentile(data, 99),
		Mean:        sum / time.Duration(n),
		Min:         minVal,
		Max:         maxVal,
		SampleCount: n,
	}
}
