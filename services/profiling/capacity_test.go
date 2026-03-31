// Package profiling – capacity_test.go provides comprehensive unit tests for
// the capacity planning report generator (capacity.go). Part of Sprint 8-10,
// E-039, these tests verify throughput measurement, bottleneck identification,
// resource utilisation metrics, report generation targeting 50,000 events/sec,
// and JSON round-trip serialisation.
package profiling

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failWriter is an http.ResponseWriter whose Write method always returns an
// error. This lets us exercise the Handler error path where JSON encoding fails.
type failWriter struct {
	header http.Header
	code   int
}

func (fw *failWriter) Header() http.Header        { return fw.header }
func (fw *failWriter) WriteHeader(statusCode int)  { fw.code = statusCode }
func (fw *failWriter) Write([]byte) (int, error) {
	if fw.code == 0 {
		fw.code = http.StatusOK
	}
	return 0, errors.New("simulated write failure")
}

// ---------------------------------------------------------------------------
// Helper: create a CapacityPlanner with a valid Profiler for testing.
// ---------------------------------------------------------------------------

func newTestCapacityPlanner(t *testing.T) *CapacityPlanner {
	t.Helper()
	profiler := NewProfiler()
	cp := NewCapacityPlanner(profiler)
	require.NotNil(t, cp, "NewCapacityPlanner must return a non-nil planner")
	return cp
}

// ---------------------------------------------------------------------------
// 2.1 TestNewCapacityPlanner
// ---------------------------------------------------------------------------

func TestNewCapacityPlanner(t *testing.T) {
	t.Run("returns_non_nil_planner", func(t *testing.T) {
		profiler := NewProfiler()
		cp := NewCapacityPlanner(profiler)
		require.NotNil(t, cp)
	})

	t.Run("default_target_throughput_is_50000", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)
		// DefaultTargetThroughput from E-039 is 50,000 events/sec.
		assert.Equal(t, DefaultTargetThroughput, cp.targetThroughput,
			"default target throughput must be %d events/sec", DefaultTargetThroughput)
		assert.Equal(t, 50000, cp.targetThroughput)
	})

	t.Run("has_profiler_reference", func(t *testing.T) {
		profiler := NewProfiler()
		cp := NewCapacityPlanner(profiler)
		assert.NotNil(t, cp.profiler, "planner must hold a reference to the Profiler")
		assert.Equal(t, profiler, cp.profiler)
	})

	t.Run("initial_state_has_no_throughput_data", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)
		cp.mu.RLock()
		defer cp.mu.RUnlock()
		assert.NotNil(t, cp.throughputData, "throughputData map must be initialised")
		assert.Equal(t, 0, len(cp.throughputData),
			"no throughput data should exist on a fresh planner")
	})
}

// ---------------------------------------------------------------------------
// 2.2 TestRecordThroughput
// ---------------------------------------------------------------------------

func TestRecordThroughput(t *testing.T) {
	t.Run("records_samples_per_stage", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)

		// Gateway samples
		cp.RecordThroughput("gateway", 45000)
		cp.RecordThroughput("gateway", 48000)
		cp.RecordThroughput("gateway", 50000)

		// Router samples
		cp.RecordThroughput("router", 42000)
		cp.RecordThroughput("router", 43000)
		cp.RecordThroughput("router", 44000)

		// Processor samples
		cp.RecordThroughput("processor", 38000)
		cp.RecordThroughput("processor", 40000)
		cp.RecordThroughput("processor", 41000)

		cp.mu.RLock()
		defer cp.mu.RUnlock()

		// Verify sample counts
		assert.Equal(t, 3, len(cp.throughputData["gateway"]))
		assert.Equal(t, 3, len(cp.throughputData["router"]))
		assert.Equal(t, 3, len(cp.throughputData["processor"]))

		// Verify values stored in order
		assert.Equal(t, []float64{45000, 48000, 50000}, cp.throughputData["gateway"])
		assert.Equal(t, []float64{42000, 43000, 44000}, cp.throughputData["router"])
		assert.Equal(t, []float64{38000, 40000, 41000}, cp.throughputData["processor"])
	})

	t.Run("different_stages_are_independent", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)

		cp.RecordThroughput("gateway", 50000)
		cp.RecordThroughput("router", 40000)

		cp.mu.RLock()
		defer cp.mu.RUnlock()

		assert.Equal(t, 1, len(cp.throughputData["gateway"]))
		assert.Equal(t, 1, len(cp.throughputData["router"]))
		assert.Equal(t, 0, len(cp.throughputData["warehouse"]))
	})
}

// ---------------------------------------------------------------------------
// 2.3 TestCalculateStageCapacity
// ---------------------------------------------------------------------------

func TestCalculateStageCapacity(t *testing.T) {
	cp := newTestCapacityPlanner(t)

	t.Run("with_multiple_samples", func(t *testing.T) {
		// 10 samples: 10K, 20K, ..., 100K events/sec
		data := []float64{10000, 20000, 30000, 40000, 50000, 60000, 70000, 80000, 90000, 100000}
		sc := cp.calculateStageCapacity("test_stage", data)

		assert.Equal(t, 100000.0, sc.MaxThroughput,
			"MaxThroughput must be the largest sample")
		assert.Equal(t, 10, sc.SampleCount)
		// Mean of 10K..100K = 55K
		assert.InDelta(t, 55000.0, sc.AvgThroughput, 0.1)
		// p95 with linear interpolation:
		// idx = 0.95 * 9 = 8.55; sorted[8]=90000, sorted[9]=100000
		// result = 90000*0.45 + 100000*0.55 = 95500
		assert.InDelta(t, 95500.0, sc.SustainedThroughput, 1.0,
			"SustainedThroughput should be p95 of the data")
	})

	t.Run("empty_data", func(t *testing.T) {
		sc := cp.calculateStageCapacity("empty", nil)
		assert.Equal(t, 0.0, sc.MaxThroughput)
		assert.Equal(t, 0.0, sc.SustainedThroughput)
		assert.Equal(t, 0.0, sc.AvgThroughput)
		assert.Equal(t, 0, sc.SampleCount)
	})

	t.Run("single_element", func(t *testing.T) {
		data := []float64{42000}
		sc := cp.calculateStageCapacity("single", data)
		assert.Equal(t, 42000.0, sc.MaxThroughput)
		assert.Equal(t, 42000.0, sc.SustainedThroughput)
		assert.Equal(t, 42000.0, sc.AvgThroughput)
		assert.Equal(t, 1, sc.SampleCount)
	})

	t.Run("two_elements", func(t *testing.T) {
		data := []float64{30000, 50000}
		sc := cp.calculateStageCapacity("two", data)
		assert.Equal(t, 50000.0, sc.MaxThroughput)
		assert.InDelta(t, 40000.0, sc.AvgThroughput, 0.1)
		assert.Equal(t, 2, sc.SampleCount)
		assert.Greater(t, sc.SustainedThroughput, 0.0)
	})

	t.Run("all_same_values", func(t *testing.T) {
		data := []float64{50000, 50000, 50000}
		sc := cp.calculateStageCapacity("uniform", data)
		assert.Equal(t, 50000.0, sc.MaxThroughput)
		assert.InDelta(t, 50000.0, sc.SustainedThroughput, 0.1)
		assert.InDelta(t, 50000.0, sc.AvgThroughput, 0.1)
		assert.Equal(t, 3, sc.SampleCount)
	})
}

// ---------------------------------------------------------------------------
// 2.4 TestIdentifyBottlenecks
// ---------------------------------------------------------------------------

func TestIdentifyBottlenecks(t *testing.T) {
	cp := newTestCapacityPlanner(t)

	t.Run("processor_is_bottleneck", func(t *testing.T) {
		stageCapacities := map[string]StageCapacity{
			"gateway": {
				MaxThroughput:       65000,
				SustainedThroughput: 60000,
				AvgThroughput:       58000,
				SampleCount:         10,
			},
			"processor": {
				MaxThroughput:       40000,
				SustainedThroughput: 35000,
				AvgThroughput:       33000,
				SampleCount:         10,
			},
			"router": {
				MaxThroughput:       58000,
				SustainedThroughput: 55000,
				AvgThroughput:       53000,
				SampleCount:         10,
			},
		}

		bottlenecks := cp.identifyBottlenecks(stageCapacities)

		// Only processor is below 50K target
		require.Equal(t, 1, len(bottlenecks),
			"exactly one bottleneck should be identified")
		assert.Equal(t, "processor", bottlenecks[0].Stage)
		assert.Equal(t, 35000.0, bottlenecks[0].SustainedThroughput)
		assert.Equal(t, 40000.0, bottlenecks[0].MaxThroughput)
		// PercentOfTarget: (35000/50000)*100 = 70%
		assert.InDelta(t, 70.0, bottlenecks[0].PercentOfTarget, 0.1)
	})

	t.Run("multiple_bottlenecks_sorted_ascending", func(t *testing.T) {
		stageCapacities := map[string]StageCapacity{
			"gateway": {
				MaxThroughput:       65000,
				SustainedThroughput: 60000,
				AvgThroughput:       58000,
				SampleCount:         10,
			},
			"processor": {
				MaxThroughput:       30000,
				SustainedThroughput: 25000,
				AvgThroughput:       23000,
				SampleCount:         10,
			},
			"router": {
				MaxThroughput:       48000,
				SustainedThroughput: 45000,
				AvgThroughput:       43000,
				SampleCount:         10,
			},
		}

		bottlenecks := cp.identifyBottlenecks(stageCapacities)

		// Both processor (25K) and router (45K) are below 50K
		require.Equal(t, 2, len(bottlenecks))
		// Sorted by sustained throughput ascending — processor first
		assert.Equal(t, "processor", bottlenecks[0].Stage)
		assert.Equal(t, "router", bottlenecks[1].Stage)
		assert.True(t, bottlenecks[0].SustainedThroughput < bottlenecks[1].SustainedThroughput,
			"bottlenecks must be sorted ascending by sustained throughput")
	})

	t.Run("no_bottlenecks_when_all_above_target", func(t *testing.T) {
		stageCapacities := map[string]StageCapacity{
			"gateway":   {SustainedThroughput: 60000},
			"processor": {SustainedThroughput: 55000},
			"router":    {SustainedThroughput: 52000},
		}

		bottlenecks := cp.identifyBottlenecks(stageCapacities)
		assert.Equal(t, 0, len(bottlenecks),
			"no bottlenecks when all stages meet or exceed target")
	})

	t.Run("empty_capacities", func(t *testing.T) {
		bottlenecks := cp.identifyBottlenecks(map[string]StageCapacity{})
		assert.Equal(t, 0, len(bottlenecks))
	})
}

// ---------------------------------------------------------------------------
// 2.5 TestGenerateCapacityReport
// ---------------------------------------------------------------------------

func TestGenerateCapacityReport(t *testing.T) {
	t.Run("full_report_with_realistic_data", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)

		// Populate all pipeline stages with realistic samples
		gatewayData := []float64{55000, 58000, 60000, 62000, 65000, 55000, 57000, 59000, 61000, 63000}
		processorData := []float64{38000, 40000, 42000, 44000, 46000, 39000, 41000, 43000, 45000, 47000}
		routerData := []float64{50000, 52000, 54000, 56000, 58000, 51000, 53000, 55000, 57000, 59000}

		for _, v := range gatewayData {
			cp.RecordThroughput(StageGateway, v)
		}
		for _, v := range processorData {
			cp.RecordThroughput("processor", v)
		}
		for _, v := range routerData {
			cp.RecordThroughput(StageRouter, v)
		}

		before := time.Now()
		report := cp.GenerateReport()
		require.NotNil(t, report)

		// TargetThroughput
		assert.Equal(t, DefaultTargetThroughput, report.TargetThroughput,
			"TargetThroughput must equal DefaultTargetThroughput (50000)")

		// Timestamp must be recent
		assert.False(t, report.Timestamp.IsZero(), "Timestamp must be set")
		assert.True(t, !report.Timestamp.Before(before),
			"Timestamp must be at or after report generation start")
		assert.True(t, time.Since(report.Timestamp) < 5*time.Second,
			"Timestamp must be recent")

		// StageCapacities must contain all recorded stages
		assert.Equal(t, 3, len(report.StageCapacities))
		_, hasGW := report.StageCapacities[StageGateway]
		_, hasProc := report.StageCapacities["processor"]
		_, hasRT := report.StageCapacities[StageRouter]
		assert.True(t, hasGW, "StageCapacities must include gateway")
		assert.True(t, hasProc, "StageCapacities must include processor")
		assert.True(t, hasRT, "StageCapacities must include router")

		// Actual throughput is the minimum sustained throughput across stages
		// (pipeline limited by slowest stage). Processor's p95 should be the lowest.
		assert.Greater(t, report.ActualThroughput, 0.0,
			"ActualThroughput must be positive when data is recorded")

		// Processor sustained throughput is below 50K, so MeetsTarget should be false
		assert.False(t, report.MeetsTarget,
			"pipeline should NOT meet target when processor is the bottleneck")

		// Bottlenecks must include the processor stage
		assert.NotEmpty(t, report.Bottlenecks,
			"Bottlenecks list must be non-empty when a stage is below target")
		foundProcessor := false
		for _, bn := range report.Bottlenecks {
			if bn.Stage == "processor" {
				foundProcessor = true
				assert.Greater(t, bn.SustainedThroughput, 0.0)
				assert.Greater(t, bn.MaxThroughput, 0.0)
				assert.Greater(t, bn.PercentOfTarget, 0.0)
			}
		}
		assert.True(t, foundProcessor, "processor must be identified as a bottleneck")

		// ResourceUtilization must contain sensible runtime values
		assert.Greater(t, report.ResourceUtilization.CPUCount, 0)
		assert.Greater(t, report.ResourceUtilization.GOMAXPROCS, 0)
		assert.Greater(t, report.ResourceUtilization.GoroutineCount, 0)
		assert.GreaterOrEqual(t, report.ResourceUtilization.MemoryAllocMB, 0.0)
		assert.GreaterOrEqual(t, report.ResourceUtilization.MemoryTotalAllocMB, 0.0)

		// Recommendations must be present for bottleneck stages
		assert.NotEmpty(t, report.Recommendations,
			"Recommendations must be generated for bottleneck stages")
	})

	t.Run("meets_target_when_all_above", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)

		// All stages above 50K
		for i := 0; i < 10; i++ {
			cp.RecordThroughput(StageGateway, 60000+float64(i*1000))
			cp.RecordThroughput(StageRouter, 55000+float64(i*1000))
			cp.RecordThroughput("processor", 52000+float64(i*1000))
		}

		report := cp.GenerateReport()
		require.NotNil(t, report)
		assert.True(t, report.MeetsTarget,
			"pipeline should meet target when all stages are above 50K")
		assert.GreaterOrEqual(t, report.ActualThroughput, float64(DefaultTargetThroughput))
		assert.Equal(t, 0, len(report.Bottlenecks),
			"no bottlenecks when all stages meet target")
	})
}

// ---------------------------------------------------------------------------
// 2.6 TestResourceUtilization
// ---------------------------------------------------------------------------

func TestResourceUtilization(t *testing.T) {
	cp := newTestCapacityPlanner(t)

	ru := cp.collectResourceUtilization()

	t.Run("cpu_count_positive", func(t *testing.T) {
		assert.Greater(t, ru.CPUCount, 0, "CPUCount must be > 0 (runtime.NumCPU)")
	})

	t.Run("gomaxprocs_positive", func(t *testing.T) {
		assert.Greater(t, ru.GOMAXPROCS, 0, "GOMAXPROCS must be > 0")
	})

	t.Run("goroutine_count_positive", func(t *testing.T) {
		assert.Greater(t, ru.GoroutineCount, 0,
			"GoroutineCount must be > 0 (runtime.NumGoroutine)")
	})

	t.Run("memory_alloc_non_negative", func(t *testing.T) {
		assert.GreaterOrEqual(t, ru.MemoryAllocMB, 0.0,
			"MemoryAllocMB must be >= 0")
	})

	t.Run("memory_total_alloc_non_negative", func(t *testing.T) {
		assert.GreaterOrEqual(t, ru.MemoryTotalAllocMB, 0.0,
			"MemoryTotalAllocMB must be >= 0")
	})

	t.Run("memory_sys_non_negative", func(t *testing.T) {
		assert.GreaterOrEqual(t, ru.MemorySysMB, 0.0,
			"MemorySysMB must be >= 0")
	})

	t.Run("values_are_reasonable", func(t *testing.T) {
		// Sanity: CPU count should be between 1 and 1024
		assert.True(t, ru.CPUCount >= 1 && ru.CPUCount <= 1024,
			"CPUCount %d should be reasonable (1–1024)", ru.CPUCount)
		// Sanity: goroutines should be between 1 and 1000000
		assert.True(t, ru.GoroutineCount >= 1 && ru.GoroutineCount <= 1000000,
			"GoroutineCount %d should be reasonable", ru.GoroutineCount)
		// Sanity: memory alloc in MB should be less than 100GB
		assert.True(t, ru.MemoryAllocMB < 100*1024,
			"MemoryAllocMB %.2f should be < 100GB", ru.MemoryAllocMB)
	})
}

// ---------------------------------------------------------------------------
// 2.7 TestScalingRecommendations
// ---------------------------------------------------------------------------

func TestScalingRecommendations(t *testing.T) {
	cp := newTestCapacityPlanner(t)

	t.Run("optimize_action_for_moderate_gap", func(t *testing.T) {
		// Router at 30K → scaleFactor = 50000/30000 ≈ 1.667 (< 2 → "optimize")
		bottlenecks := []BottleneckInfo{
			{
				Stage:               "router",
				MaxThroughput:       35000,
				SustainedThroughput: 30000,
				PercentOfTarget:     60,
			},
		}

		recs := cp.generateRecommendations(bottlenecks)
		require.NotEmpty(t, recs)
		assert.Equal(t, "router", recs[0].Stage)
		assert.Equal(t, "optimize", recs[0].Action,
			"action should be 'optimize' when scaleFactor < 2")
		assert.Contains(t, recs[0].Reason, "router")
		assert.InDelta(t, 50000.0/30000.0, recs[0].ScaleFactor, 0.01)
		assert.Greater(t, recs[0].ScaleFactor, 0.0)
	})

	t.Run("scale_horizontal_for_large_gap", func(t *testing.T) {
		// Processor at 20K → scaleFactor = 50000/20000 = 2.5 (> 2 → "scale_horizontal")
		bottlenecks := []BottleneckInfo{
			{
				Stage:               "processor",
				MaxThroughput:       25000,
				SustainedThroughput: 20000,
				PercentOfTarget:     40,
			},
		}

		recs := cp.generateRecommendations(bottlenecks)
		require.NotEmpty(t, recs)
		assert.Equal(t, "processor", recs[0].Stage)
		assert.Equal(t, "scale_horizontal", recs[0].Action,
			"action should be 'scale_horizontal' when scaleFactor > 2")
		assert.InDelta(t, 2.5, recs[0].ScaleFactor, 0.01)
	})

	t.Run("no_recommendations_when_no_bottlenecks", func(t *testing.T) {
		recs := cp.generateRecommendations(nil)
		assert.Equal(t, 0, len(recs),
			"no recommendations when bottlenecks is nil")

		recs = cp.generateRecommendations([]BottleneckInfo{})
		assert.Equal(t, 0, len(recs),
			"no recommendations when bottlenecks is empty")
	})

	t.Run("multiple_bottleneck_recommendations", func(t *testing.T) {
		bottlenecks := []BottleneckInfo{
			{Stage: "processor", SustainedThroughput: 15000, PercentOfTarget: 30},
			{Stage: "router", SustainedThroughput: 40000, PercentOfTarget: 80},
		}

		recs := cp.generateRecommendations(bottlenecks)
		require.Equal(t, 2, len(recs),
			"one recommendation per bottleneck stage")
		assert.Equal(t, "processor", recs[0].Stage)
		assert.Equal(t, "router", recs[1].Stage)
	})

	t.Run("skips_bottleneck_at_100_percent", func(t *testing.T) {
		// A bottleneck with PercentOfTarget >= 100 should be skipped
		// (this can happen due to rounding or exact target match).
		bottlenecks := []BottleneckInfo{
			{Stage: "exact", SustainedThroughput: 50000, PercentOfTarget: 100},
		}

		recs := cp.generateRecommendations(bottlenecks)
		assert.Equal(t, 0, len(recs),
			"bottleneck at exactly 100%% of target should produce no recommendation")
	})

	t.Run("zero_sustained_throughput_no_panic", func(t *testing.T) {
		bottlenecks := []BottleneckInfo{
			{Stage: "stalled", SustainedThroughput: 0, PercentOfTarget: 0},
		}

		recs := cp.generateRecommendations(bottlenecks)
		require.NotEmpty(t, recs)
		// ScaleFactor should be 0 (guarded against division by zero)
		assert.Equal(t, 0.0, recs[0].ScaleFactor,
			"ScaleFactor must be 0 when sustained throughput is 0")
	})
}

// ---------------------------------------------------------------------------
// 2.8 TestCapacityReportWithEmptyData
// ---------------------------------------------------------------------------

func TestCapacityReportWithEmptyData(t *testing.T) {
	cp := newTestCapacityPlanner(t)

	// No data recorded — must not panic and must produce a valid report.
	report := cp.GenerateReport()
	require.NotNil(t, report, "GenerateReport must not return nil even with empty data")

	t.Run("target_throughput_is_set", func(t *testing.T) {
		assert.Equal(t, DefaultTargetThroughput, report.TargetThroughput)
	})

	t.Run("actual_throughput_is_zero", func(t *testing.T) {
		assert.Equal(t, 0.0, report.ActualThroughput,
			"actual throughput must be 0 with no data")
	})

	t.Run("does_not_meet_target", func(t *testing.T) {
		assert.False(t, report.MeetsTarget,
			"MeetsTarget must be false with no data")
	})

	t.Run("empty_bottlenecks_and_capacities", func(t *testing.T) {
		assert.Equal(t, 0, len(report.Bottlenecks))
		assert.Equal(t, 0, len(report.StageCapacities))
	})

	t.Run("resource_utilization_is_populated", func(t *testing.T) {
		// Even without throughput data, resource utilisation should be collected.
		assert.Greater(t, report.ResourceUtilization.CPUCount, 0)
		assert.Greater(t, report.ResourceUtilization.GOMAXPROCS, 0)
		assert.Greater(t, report.ResourceUtilization.GoroutineCount, 0)
	})

	t.Run("timestamp_is_recent", func(t *testing.T) {
		assert.False(t, report.Timestamp.IsZero())
		assert.True(t, time.Since(report.Timestamp) < 5*time.Second)
	})
}

// ---------------------------------------------------------------------------
// 2.9 TestCapacityReportJSONSerialization
// ---------------------------------------------------------------------------

func TestCapacityReportJSONSerialization(t *testing.T) {
	t.Run("round_trip_with_full_report", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)

		// Populate with representative data across multiple stages
		for i := 0; i < 20; i++ {
			cp.RecordThroughput(StageGateway, 55000+float64(i*500))
			cp.RecordThroughput("processor", 38000+float64(i*500))
			cp.RecordThroughput(StageRouter, 48000+float64(i*500))
			cp.RecordThroughput(StageWarehouse, 30000+float64(i*200))
		}

		original := cp.GenerateReport()
		require.NotNil(t, original)

		// Marshal to JSON
		data, err := jsonrs.Marshal(original)
		require.NoError(t, err, "JSON marshal must not fail")
		assert.NotEmpty(t, data, "serialised JSON must be non-empty")

		// Unmarshal back
		var decoded CapacityReport
		err = jsonrs.Unmarshal(data, &decoded)
		require.NoError(t, err, "JSON unmarshal must not fail")

		// Verify round-trip equivalence of key fields
		assert.Equal(t, original.TargetThroughput, decoded.TargetThroughput)
		assert.InDelta(t, original.ActualThroughput, decoded.ActualThroughput, 0.01)
		assert.Equal(t, original.MeetsTarget, decoded.MeetsTarget)
		assert.Equal(t, len(original.Bottlenecks), len(decoded.Bottlenecks))
		assert.Equal(t, len(original.StageCapacities), len(decoded.StageCapacities))
		assert.Equal(t, len(original.Recommendations), len(decoded.Recommendations))

		// Verify resource utilisation round-trip
		assert.Equal(t, original.ResourceUtilization.CPUCount, decoded.ResourceUtilization.CPUCount)
		assert.Equal(t, original.ResourceUtilization.GOMAXPROCS, decoded.ResourceUtilization.GOMAXPROCS)
		assert.Equal(t, original.ResourceUtilization.GoroutineCount, decoded.ResourceUtilization.GoroutineCount)
		assert.InDelta(t, original.ResourceUtilization.MemoryAllocMB,
			decoded.ResourceUtilization.MemoryAllocMB, 0.01)
		assert.InDelta(t, original.ResourceUtilization.MemoryTotalAllocMB,
			decoded.ResourceUtilization.MemoryTotalAllocMB, 0.01)

		// Verify stage capacities round-trip for a known stage
		if origSC, ok := original.StageCapacities[StageGateway]; ok {
			decSC, decOK := decoded.StageCapacities[StageGateway]
			require.True(t, decOK, "decoded report must include gateway stage")
			assert.InDelta(t, origSC.MaxThroughput, decSC.MaxThroughput, 0.01)
			assert.InDelta(t, origSC.SustainedThroughput, decSC.SustainedThroughput, 0.01)
			assert.InDelta(t, origSC.AvgThroughput, decSC.AvgThroughput, 0.01)
			assert.Equal(t, origSC.SampleCount, decSC.SampleCount)
		}
	})

	t.Run("round_trip_with_empty_report", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)
		original := cp.GenerateReport()

		data, err := jsonrs.Marshal(original)
		require.NoError(t, err)

		var decoded CapacityReport
		err = jsonrs.Unmarshal(data, &decoded)
		require.NoError(t, err)

		assert.Equal(t, original.TargetThroughput, decoded.TargetThroughput)
		assert.Equal(t, original.MeetsTarget, decoded.MeetsTarget)
		assert.Equal(t, original.ActualThroughput, decoded.ActualThroughput)
	})
}

// ---------------------------------------------------------------------------
// Additional: TestCapacityPlannerHandler (HTTP endpoint)
// ---------------------------------------------------------------------------

func TestCapacityPlannerHandler(t *testing.T) {
	t.Run("returns_json_report", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)
		cp.RecordThroughput(StageGateway, 55000)
		cp.RecordThroughput(StageRouter, 45000)

		handler := cp.Handler()
		require.NotNil(t, handler)

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/profiling/capacity", nil)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, rr.Header().Get("Content-Type"), "application/json")

		var report CapacityReport
		err := jsonrs.Unmarshal(rr.Body.Bytes(), &report)
		require.NoError(t, err, "response body must be valid CapacityReport JSON")
		assert.Equal(t, DefaultTargetThroughput, report.TargetThroughput)
		assert.Equal(t, 2, len(report.StageCapacities))
	})

	t.Run("works_with_empty_data", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/profiling/capacity", nil)
		cp.Handler().ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)

		var report CapacityReport
		err := jsonrs.Unmarshal(rr.Body.Bytes(), &report)
		require.NoError(t, err)
		assert.False(t, report.MeetsTarget)
	})

	t.Run("handles_write_error", func(t *testing.T) {
		cp := newTestCapacityPlanner(t)
		cp.RecordThroughput(StageGateway, 55000)

		req := httptest.NewRequest(http.MethodGet, "/v1/profiling/capacity", nil)
		fw := &failWriter{header: http.Header{}}
		cp.Handler().ServeHTTP(fw, req)

		// The handler should attempt http.Error which also calls Write —
		// we simply verify no panic occurred and the status was set to 500.
		assert.Equal(t, http.StatusInternalServerError, fw.code)
	})
}

// ---------------------------------------------------------------------------
// Additional: TestReset
// ---------------------------------------------------------------------------

func TestCapacityPlannerReset(t *testing.T) {
	cp := newTestCapacityPlanner(t)

	// Record some data
	cp.RecordThroughput(StageGateway, 50000)
	cp.RecordThroughput(StageRouter, 40000)

	cp.mu.RLock()
	assert.Equal(t, 2, len(cp.throughputData))
	cp.mu.RUnlock()

	// Reset
	cp.Reset()

	cp.mu.RLock()
	assert.Equal(t, 0, len(cp.throughputData),
		"throughput data must be cleared after Reset")
	cp.mu.RUnlock()

	// Report after reset should be empty
	report := cp.GenerateReport()
	require.NotNil(t, report)
	assert.Equal(t, 0.0, report.ActualThroughput)
	assert.False(t, report.MeetsTarget)
	assert.Equal(t, 0, len(report.StageCapacities))
}

// ---------------------------------------------------------------------------
// Additional: TestComputeFloat64Percentile
// ---------------------------------------------------------------------------

func TestComputeFloat64Percentile(t *testing.T) {
	t.Run("p95_of_sequential_data", func(t *testing.T) {
		data := make([]float64, 100)
		for i := range data {
			data[i] = float64(i + 1) // 1, 2, ..., 100
		}
		// p95: idx = 0.95 * 99 = 94.05
		// sorted[94]=95, sorted[95]=96
		// result = 95*0.95 + 96*0.05 = 90.25 + 4.80 = 95.05
		p95 := computeFloat64Percentile(data, 95)
		assert.InDelta(t, 95.05, p95, 0.1)
	})

	t.Run("p50_median", func(t *testing.T) {
		data := []float64{10, 20, 30, 40, 50}
		p50 := computeFloat64Percentile(data, 50)
		assert.InDelta(t, 30.0, p50, 0.1)
	})

	t.Run("empty_data_returns_zero", func(t *testing.T) {
		p := computeFloat64Percentile(nil, 95)
		assert.Equal(t, 0.0, p)
	})

	t.Run("single_element", func(t *testing.T) {
		p := computeFloat64Percentile([]float64{42.0}, 95)
		assert.Equal(t, 42.0, p)
	})

	t.Run("does_not_mutate_input", func(t *testing.T) {
		data := []float64{50, 10, 30, 20, 40}
		original := make([]float64, len(data))
		copy(original, data)

		computeFloat64Percentile(data, 95)
		assert.Equal(t, original, data,
			"computeFloat64Percentile must not mutate the input slice")
	})
}
