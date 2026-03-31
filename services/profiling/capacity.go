// Package profiling – capacity.go provides the capacity planning report
// generator for the RudderStack event pipeline. It is part of Sprint 8-10,
// E-039 and targets 50,000 events/sec throughput measurement and validation.
//
// The CapacityPlanner collects per-stage throughput observations, calculates
// sustained (p95) and peak capacities, identifies bottleneck stages, gathers
// runtime resource utilisation metrics, and emits scaling recommendations.
// Reports are exposed via an HTTP JSON API endpoint.
package profiling

import (
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

// DefaultTargetThroughput is the baseline target throughput in events/sec as
// specified in E-039. This value is used when no override is provided via the
// "Profiling.targetThroughput" configuration key.
const DefaultTargetThroughput = 50000

// ---------------------------------------------------------------------------
// Report types
// ---------------------------------------------------------------------------

// CapacityReport is the primary output of the capacity planning system.
// It aggregates per-stage throughput capacities, identifies bottleneck stages,
// collects resource utilisation metrics, and provides scaling recommendations.
type CapacityReport struct {
	// Timestamp records when the report was generated.
	Timestamp time.Time `json:"timestamp"`
	// TargetThroughput is the desired pipeline throughput in events/sec.
	TargetThroughput int `json:"target_throughput"`
	// ActualThroughput is the minimum sustained throughput across all stages,
	// representing the effective pipeline capacity (limited by slowest stage).
	ActualThroughput float64 `json:"actual_throughput"`
	// MeetsTarget indicates whether ActualThroughput meets or exceeds the target.
	MeetsTarget bool `json:"meets_target"`
	// Bottlenecks lists stages whose sustained throughput falls below the target,
	// sorted by sustained throughput ascending (worst first).
	Bottlenecks []BottleneckInfo `json:"bottlenecks"`
	// StageCapacities maps each observed pipeline stage to its capacity metrics.
	StageCapacities map[string]StageCapacity `json:"stage_capacities"`
	// ResourceUtilization contains runtime resource metrics at report generation time.
	ResourceUtilization ResourceUtilization `json:"resource_utilization"`
	// Recommendations contains scaling suggestions for bottleneck stages.
	Recommendations []ScalingRecommendation `json:"recommendations"`
}

// BottleneckInfo describes a pipeline stage that cannot sustain the target
// throughput.
type BottleneckInfo struct {
	// Stage is the pipeline stage name (e.g. "gateway", "router").
	Stage string `json:"stage"`
	// MaxThroughput is the peak observed throughput for this stage in events/sec.
	MaxThroughput float64 `json:"max_throughput"`
	// SustainedThroughput is the p95 observed throughput for this stage in events/sec.
	SustainedThroughput float64 `json:"sustained_throughput"`
	// PercentOfTarget expresses SustainedThroughput as a percentage of the target.
	PercentOfTarget float64 `json:"percent_of_target"`
}

// StageCapacity contains throughput capacity metrics for a single pipeline stage.
type StageCapacity struct {
	// MaxThroughput is the peak observed throughput in events/sec.
	MaxThroughput float64 `json:"max_throughput"`
	// SustainedThroughput is the p95 percentile of observed throughput samples.
	SustainedThroughput float64 `json:"sustained_throughput"`
	// AvgThroughput is the arithmetic mean of observed throughput samples.
	AvgThroughput float64 `json:"avg_throughput"`
	// SampleCount is the number of throughput observations recorded for this stage.
	SampleCount int `json:"sample_count"`
}

// ResourceUtilization captures Go runtime resource metrics at the time the
// capacity report is generated. Memory values are expressed in megabytes.
type ResourceUtilization struct {
	// CPUCount is the number of logical CPUs available (runtime.NumCPU).
	CPUCount int `json:"cpu_count"`
	// GOMAXPROCS is the current GOMAXPROCS setting.
	GOMAXPROCS int `json:"gomaxprocs"`
	// GoroutineCount is the current number of goroutines.
	GoroutineCount int `json:"goroutine_count"`
	// MemoryAllocMB is the current heap allocation in megabytes.
	MemoryAllocMB float64 `json:"memory_alloc_mb"`
	// MemoryTotalAllocMB is the cumulative heap allocation in megabytes.
	MemoryTotalAllocMB float64 `json:"memory_total_alloc_mb"`
	// MemorySysMB is the total memory obtained from the OS in megabytes.
	MemorySysMB float64 `json:"memory_sys_mb"`
	// NumGC is the number of completed GC cycles.
	NumGC uint32 `json:"num_gc"`
}

// ScalingRecommendation provides an actionable suggestion for a bottleneck stage.
type ScalingRecommendation struct {
	// Stage is the pipeline stage this recommendation targets.
	Stage string `json:"stage"`
	// Action describes the recommended action (e.g. "scale_horizontal", "optimize").
	Action string `json:"action"`
	// Reason provides a human-readable explanation including current vs target throughput.
	Reason string `json:"reason"`
	// ScaleFactor is the ratio of target to current sustained throughput, indicating
	// how much scaling is needed. Omitted from JSON when zero.
	ScaleFactor float64 `json:"scale_factor,omitempty"`
}

// ---------------------------------------------------------------------------
// CapacityPlanner
// ---------------------------------------------------------------------------

// CapacityPlanner records per-stage throughput observations and generates
// capacity planning reports with bottleneck identification and scaling
// recommendations. All public methods are safe for concurrent use.
type CapacityPlanner struct {
	mu               sync.RWMutex
	profiler         *Profiler
	targetThroughput int
	throughputData   map[string][]float64 // stage → throughput samples (events/sec)
	logger           logger.Logger
}

// NewCapacityPlanner creates a CapacityPlanner wired to the given Profiler.
// The target throughput is loaded from the "Profiling.targetThroughput"
// configuration key (default: DefaultTargetThroughput = 50,000 events/sec).
func NewCapacityPlanner(profiler *Profiler) *CapacityPlanner {
	return &CapacityPlanner{
		profiler:         profiler,
		targetThroughput: config.GetIntVar(DefaultTargetThroughput, 1, "Profiling.targetThroughput"),
		throughputData:   make(map[string][]float64),
		logger:           logger.NewLogger().Child("profiling.capacity"),
	}
}

// ---------------------------------------------------------------------------
// Public methods
// ---------------------------------------------------------------------------

// RecordThroughput appends a throughput observation (events/sec) for the named
// pipeline stage. This method is thread-safe and is intended to be called by
// external callers (e.g. pipeline instrumentation) to feed data into the
// capacity planner.
func (cp *CapacityPlanner) RecordThroughput(stage string, eventsPerSec float64) {
	cp.mu.Lock()
	cp.throughputData[stage] = append(cp.throughputData[stage], eventsPerSec)
	cp.mu.Unlock()
}

// GenerateReport produces a complete CapacityReport from all throughput data
// recorded since the last Reset. The report includes per-stage capacities,
// bottleneck identification, resource utilisation, and scaling recommendations.
//
// The actual pipeline throughput is determined by the minimum sustained
// throughput across all recorded stages, since the pipeline is limited by its
// slowest stage. If no data has been recorded, a zero-valued report is returned
// with MeetsTarget set to false.
func (cp *CapacityPlanner) GenerateReport() *CapacityReport {
	// Snapshot throughput data under read lock and release immediately so
	// computation does not block writers.
	cp.mu.RLock()
	snapshot := make(map[string][]float64, len(cp.throughputData))
	for stage, samples := range cp.throughputData {
		c := make([]float64, len(samples))
		copy(c, samples)
		snapshot[stage] = c
	}
	cp.mu.RUnlock()

	// Calculate per-stage capacities.
	stageCapacities := make(map[string]StageCapacity, len(snapshot))
	for stage, data := range snapshot {
		stageCapacities[stage] = cp.calculateStageCapacity(stage, data)
	}

	// Identify bottleneck stages below target throughput.
	bottlenecks := cp.identifyBottlenecks(stageCapacities)

	// Determine actual throughput as the minimum sustained throughput across
	// all stages — the pipeline is limited by its slowest stage.
	var actualThroughput float64
	if len(stageCapacities) > 0 {
		first := true
		for _, sc := range stageCapacities {
			if first || sc.SustainedThroughput < actualThroughput {
				actualThroughput = sc.SustainedThroughput
				first = false
			}
		}
	}

	return &CapacityReport{
		Timestamp:           time.Now(),
		TargetThroughput:    cp.targetThroughput,
		ActualThroughput:    actualThroughput,
		MeetsTarget:         actualThroughput >= float64(cp.targetThroughput),
		Bottlenecks:         bottlenecks,
		StageCapacities:     stageCapacities,
		ResourceUtilization: cp.collectResourceUtilization(),
		Recommendations:     cp.generateRecommendations(bottlenecks),
	}
}

// Reset discards all recorded throughput data, allowing a new measurement
// window to begin. This method is thread-safe.
func (cp *CapacityPlanner) Reset() {
	cp.mu.Lock()
	cp.throughputData = make(map[string][]float64)
	cp.mu.Unlock()
}

// Handler returns an http.HandlerFunc that generates a capacity report and
// serialises it as JSON. The handler sets Content-Type to application/json
// and returns HTTP 200 on success or HTTP 500 if serialisation fails.
func (cp *CapacityPlanner) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		report := cp.GenerateReport()

		w.Header().Set("Content-Type", "application/json")
		if err := jsonrs.NewEncoder(w).Encode(report); err != nil {
			cp.logger.Errorn("failed to encode capacity report", obskit.Error(err))
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// calculateStageCapacity computes throughput capacity metrics for a single
// stage from its recorded throughput samples:
//   - MaxThroughput: the peak observed value
//   - SustainedThroughput: the p95 percentile (representative of sustained load)
//   - AvgThroughput: the arithmetic mean
//   - SampleCount: number of observations
//
// Returns a zero-valued StageCapacity when data is empty.
func (cp *CapacityPlanner) calculateStageCapacity(_ string, data []float64) StageCapacity {
	n := len(data)
	if n == 0 {
		return StageCapacity{}
	}

	maxVal := data[0]
	var sum float64
	for _, v := range data {
		sum += v
		if v > maxVal {
			maxVal = v
		}
	}

	return StageCapacity{
		MaxThroughput:       maxVal,
		SustainedThroughput: computeFloat64Percentile(data, 95),
		AvgThroughput:       sum / float64(n),
		SampleCount:         n,
	}
}

// identifyBottlenecks returns a list of stages whose sustained throughput
// falls below the configured target. The result is sorted by sustained
// throughput ascending so the worst bottleneck appears first.
func (cp *CapacityPlanner) identifyBottlenecks(stageCapacities map[string]StageCapacity) []BottleneckInfo {
	target := float64(cp.targetThroughput)
	var bottlenecks []BottleneckInfo

	for stage, sc := range stageCapacities {
		if sc.SustainedThroughput < target {
			var pct float64
			if target > 0 {
				pct = (sc.SustainedThroughput / target) * 100
			}
			bottlenecks = append(bottlenecks, BottleneckInfo{
				Stage:               stage,
				MaxThroughput:       sc.MaxThroughput,
				SustainedThroughput: sc.SustainedThroughput,
				PercentOfTarget:     pct,
			})
		}
	}

	// Sort by SustainedThroughput ascending — worst bottleneck first.
	sort.Slice(bottlenecks, func(i, j int) bool {
		return bottlenecks[i].SustainedThroughput < bottlenecks[j].SustainedThroughput
	})

	return bottlenecks
}

// collectResourceUtilization gathers Go runtime metrics at the current point in
// time. Memory figures are converted to megabytes for human readability.
func (cp *CapacityPlanner) collectResourceUtilization() ResourceUtilization {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	const bytesPerMB = 1024 * 1024

	return ResourceUtilization{
		CPUCount:           runtime.NumCPU(),
		GOMAXPROCS:         runtime.GOMAXPROCS(0), // 0 = query without setting
		GoroutineCount:     runtime.NumGoroutine(),
		MemoryAllocMB:      float64(memStats.Alloc) / bytesPerMB,
		MemoryTotalAllocMB: float64(memStats.TotalAlloc) / bytesPerMB,
		MemorySysMB:        float64(memStats.Sys) / bytesPerMB,
		NumGC:              memStats.NumGC,
	}
}

// generateRecommendations produces scaling recommendations for each bottleneck
// stage. The ScaleFactor is calculated as target / sustained throughput.
// Stages requiring more than 2× scaling are recommended for horizontal scaling;
// stages closer to the target receive optimisation recommendations.
//
// Returns nil when there are no bottlenecks (system is meeting targets).
func (cp *CapacityPlanner) generateRecommendations(bottlenecks []BottleneckInfo) []ScalingRecommendation {
	if len(bottlenecks) == 0 {
		return nil
	}

	recommendations := make([]ScalingRecommendation, 0, len(bottlenecks))
	for _, bn := range bottlenecks {
		if bn.PercentOfTarget >= 100 {
			continue
		}

		// Guard against division by zero when sustained throughput is zero.
		var scaleFactor float64
		if bn.SustainedThroughput > 0 {
			scaleFactor = float64(cp.targetThroughput) / bn.SustainedThroughput
		}

		action := "optimize"
		if scaleFactor > 2 {
			action = "scale_horizontal"
		}

		recommendations = append(recommendations, ScalingRecommendation{
			Stage:  bn.Stage,
			Action: action,
			Reason: fmt.Sprintf(
				"stage %q sustained throughput %.0f events/sec is below target %d events/sec (%.1f%%)",
				bn.Stage, bn.SustainedThroughput, cp.targetThroughput, bn.PercentOfTarget,
			),
			ScaleFactor: scaleFactor,
		})
	}

	return recommendations
}

// computeFloat64Percentile calculates the given percentile (0–100) from a
// slice of float64 values using linear interpolation between floor and ceil
// indices. A copy of the data is sorted so the caller's slice is not modified.
//
// Edge cases:
//   - empty data → 0
//   - single element → that element
func computeFloat64Percentile(data []float64, percentile float64) float64 {
	n := len(data)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return data[0]
	}

	// Sort a copy to avoid mutating the caller's slice.
	sorted := make([]float64, n)
	copy(sorted, data)
	sort.Float64s(sorted)

	// Linear interpolation between floor and ceil indices.
	idx := (percentile / 100.0) * float64(n-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))

	if lower == upper || upper >= n {
		return sorted[lower]
	}

	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}
