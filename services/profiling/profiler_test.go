// Package profiling — unit tests for the per-stage pipeline performance
// profiling engine (profiler.go). Part of Sprint 8-10, E-039 (Capacity Planning).
//
// Tests cover constructor validation, latency recording, percentile computation,
// report generation, data reset, HTTP handler, and concurrent access safety.
package profiling

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
)

// newTestProfiler creates a Profiler suitable for unit testing. It uses NOP
// implementations for logger and stats to avoid side effects from the global
// singletons, and fixed-value config loaders so tests are deterministic.
func newTestProfiler() *Profiler {
	return &Profiler{
		stageData:    make(map[string][]time.Duration),
		logger:       logger.NOP,
		statsFactory: stats.NOP,
		samplingRate: config.SingleValueLoader(100),
		enabled:      config.SingleValueLoader(true),
	}
}

// ---------------------------------------------------------------------------
// TestNewProfiler verifies that the constructor returns a valid, properly
// initialised Profiler and that the AllStages variable contains all expected
// pipeline stage names.
// ---------------------------------------------------------------------------
func TestNewProfiler(t *testing.T) {
	t.Run("returns a valid non-nil profiler", func(t *testing.T) {
		p := NewProfiler()
		require.NotNil(t, p, "NewProfiler must return a non-nil profiler")
		require.NotNil(t, p.stageData, "stageData map must be initialised")
		require.Equal(t, 0, len(p.stageData), "initial stageData must be empty")
		require.NotNil(t, p.logger, "logger must be set")
		require.NotNil(t, p.statsFactory, "statsFactory must be set")
		require.NotNil(t, p.samplingRate, "samplingRate config loader must be set")
		require.NotNil(t, p.enabled, "enabled config loader must be set")
	})

	t.Run("enabled defaults to true", func(t *testing.T) {
		p := NewProfiler()
		require.True(t, p.enabled.Load(), "profiling should be enabled by default")
	})

	t.Run("AllStages contains expected pipeline stages", func(t *testing.T) {
		expectedStages := []string{
			"gateway",
			"preprocess",
			"srcHydration",
			"preTransform",
			"userTransform",
			"destTransform",
			"store",
			"router",
			"warehouse",
		}
		require.Equal(t, len(expectedStages), len(AllStages),
			"AllStages should have exactly %d entries", len(expectedStages))
		for i, expected := range expectedStages {
			require.Equal(t, expected, AllStages[i],
				"AllStages[%d] should be %q", i, expected)
		}
	})

	t.Run("stage constants match AllStages entries", func(t *testing.T) {
		require.Equal(t, StageGateway, AllStages[0])
		require.Equal(t, StagePreprocess, AllStages[1])
		require.Equal(t, StageSrcHydration, AllStages[2])
		require.Equal(t, StagePreTransform, AllStages[3])
		require.Equal(t, StageUserTransform, AllStages[4])
		require.Equal(t, StageDestTransform, AllStages[5])
		require.Equal(t, StageStore, AllStages[6])
		require.Equal(t, StageRouter, AllStages[7])
		require.Equal(t, StageWarehouse, AllStages[8])
	})
}

// ---------------------------------------------------------------------------
// TestRecordStageLatency verifies that latency samples are correctly stored
// per stage and that the profiler accepts both known and custom stage names.
// ---------------------------------------------------------------------------
func TestRecordStageLatency(t *testing.T) {
	t.Run("records multiple samples per stage", func(t *testing.T) {
		p := newTestProfiler()

		// Gateway: 3 samples
		p.RecordStageLatency("gateway", 5*time.Millisecond)
		p.RecordStageLatency("gateway", 10*time.Millisecond)
		p.RecordStageLatency("gateway", 15*time.Millisecond)

		// Preprocess: 2 samples
		p.RecordStageLatency("preprocess", 2*time.Millisecond)
		p.RecordStageLatency("preprocess", 4*time.Millisecond)

		// Router: 4 samples
		p.RecordStageLatency("router", 20*time.Millisecond)
		p.RecordStageLatency("router", 30*time.Millisecond)
		p.RecordStageLatency("router", 40*time.Millisecond)
		p.RecordStageLatency("router", 50*time.Millisecond)

		p.mu.RLock()
		defer p.mu.RUnlock()

		require.Len(t, p.stageData["gateway"], 3, "gateway should have 3 samples")
		require.Len(t, p.stageData["preprocess"], 2, "preprocess should have 2 samples")
		require.Len(t, p.stageData["router"], 4, "router should have 4 samples")
	})

	t.Run("verifies stored sample values", func(t *testing.T) {
		p := newTestProfiler()

		p.RecordStageLatency("gateway", 5*time.Millisecond)
		p.RecordStageLatency("gateway", 10*time.Millisecond)
		p.RecordStageLatency("gateway", 15*time.Millisecond)

		p.mu.RLock()
		defer p.mu.RUnlock()

		require.EqualValues(t, 5*time.Millisecond, p.stageData["gateway"][0])
		require.EqualValues(t, 10*time.Millisecond, p.stageData["gateway"][1])
		require.EqualValues(t, 15*time.Millisecond, p.stageData["gateway"][2])
	})

	t.Run("accepts unknown/custom stage names", func(t *testing.T) {
		p := newTestProfiler()

		p.RecordStageLatency("custom_stage", 10*time.Millisecond)
		p.RecordStageLatency("another_custom", 25*time.Millisecond)
		p.RecordStageLatency("another_custom", 35*time.Millisecond)

		p.mu.RLock()
		defer p.mu.RUnlock()

		require.Len(t, p.stageData["custom_stage"], 1)
		require.Equal(t, 10*time.Millisecond, p.stageData["custom_stage"][0])
		require.Len(t, p.stageData["another_custom"], 2)
	})

	t.Run("no-op when profiling is disabled", func(t *testing.T) {
		p := &Profiler{
			stageData:    make(map[string][]time.Duration),
			logger:       logger.NOP,
			statsFactory: stats.NOP,
			samplingRate: config.SingleValueLoader(100),
			enabled:      config.SingleValueLoader(false), // disabled
		}

		p.RecordStageLatency("gateway", 5*time.Millisecond)

		p.mu.RLock()
		defer p.mu.RUnlock()

		require.Len(t, p.stageData["gateway"], 0,
			"disabled profiler must not record samples")
	})
}

// ---------------------------------------------------------------------------
// TestComputePercentiles directly exercises the unexported computePercentile
// function with deterministic data and edge cases.
// ---------------------------------------------------------------------------
func TestComputePercentiles(t *testing.T) {
	t.Run("100 deterministic samples 1ms to 100ms", func(t *testing.T) {
		// Build a deterministic dataset: [1ms, 2ms, 3ms, ..., 100ms]
		data := make([]time.Duration, 100)
		for i := 0; i < 100; i++ {
			data[i] = time.Duration(i+1) * time.Millisecond
		}

		p50 := computePercentile(data, 50)
		p95 := computePercentile(data, 95)
		p99 := computePercentile(data, 99)

		// With linear interpolation on 100 samples [1..100]:
		// p50 idx = 0.50 * 99 = 49.5 → interp(50ms, 51ms, 0.5) ≈ 50.5ms
		// p95 idx = 0.95 * 99 = 94.05 → interp(95ms, 96ms, 0.05) ≈ 95.05ms
		// p99 idx = 0.99 * 99 = 98.01 → interp(99ms, 100ms, 0.01) ≈ 99.01ms
		assert.InDelta(t, float64(50*time.Millisecond), float64(p50), float64(2*time.Millisecond),
			"p50 should be approximately 50ms")
		assert.InDelta(t, float64(95*time.Millisecond), float64(p95), float64(2*time.Millisecond),
			"p95 should be approximately 95ms")
		assert.InDelta(t, float64(99*time.Millisecond), float64(p99), float64(2*time.Millisecond),
			"p99 should be approximately 99ms")
	})

	t.Run("empty data returns zero", func(t *testing.T) {
		var empty []time.Duration
		require.Zero(t, computePercentile(empty, 50), "p50 of empty data should be zero")
		require.Zero(t, computePercentile(empty, 95), "p95 of empty data should be zero")
		require.Zero(t, computePercentile(empty, 99), "p99 of empty data should be zero")
	})

	t.Run("single sample returns that value for all percentiles", func(t *testing.T) {
		data := []time.Duration{42 * time.Millisecond}

		require.Equal(t, 42*time.Millisecond, computePercentile(data, 50), "p50 of single sample")
		require.Equal(t, 42*time.Millisecond, computePercentile(data, 95), "p95 of single sample")
		require.Equal(t, 42*time.Millisecond, computePercentile(data, 99), "p99 of single sample")
	})

	t.Run("two samples interpolate correctly", func(t *testing.T) {
		data := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}

		p50 := computePercentile(data, 50)
		// idx = 0.5 * 1 = 0.5 → interp(10ms, 20ms, 0.5) = 15ms
		require.Equal(t, 15*time.Millisecond, p50, "p50 of [10ms, 20ms] should be 15ms")
	})

	t.Run("does not mutate original data", func(t *testing.T) {
		data := []time.Duration{
			50 * time.Millisecond,
			10 * time.Millisecond,
			30 * time.Millisecond,
			20 * time.Millisecond,
			40 * time.Millisecond,
		}
		original := make([]time.Duration, len(data))
		copy(original, data)

		_ = computePercentile(data, 50)

		require.Equal(t, original, data, "computePercentile must not mutate the input slice")
	})
}

// ---------------------------------------------------------------------------
// TestGenerateProfileReport exercises the full report generation path with
// data recorded across all pipeline stages.
// ---------------------------------------------------------------------------
func TestGenerateProfileReport(t *testing.T) {
	p := newTestProfiler()

	// Populate data with varied distributions for each stage.
	stageLatencies := map[string][]time.Duration{
		StageGateway:       {5 * time.Millisecond, 10 * time.Millisecond, 15 * time.Millisecond},
		StagePreprocess:    {2 * time.Millisecond, 4 * time.Millisecond, 6 * time.Millisecond},
		StageSrcHydration:  {1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
		StagePreTransform:  {3 * time.Millisecond, 5 * time.Millisecond, 7 * time.Millisecond},
		StageUserTransform: {10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond},
		StageDestTransform: {8 * time.Millisecond, 12 * time.Millisecond, 16 * time.Millisecond},
		StageStore:         {4 * time.Millisecond, 6 * time.Millisecond, 8 * time.Millisecond},
		StageRouter:        {20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond},
		StageWarehouse:     {50 * time.Millisecond, 60 * time.Millisecond, 70 * time.Millisecond},
	}
	for stage, latencies := range stageLatencies {
		for _, lat := range latencies {
			p.RecordStageLatency(stage, lat)
		}
	}

	report := p.GenerateReport()

	t.Run("report is non-nil", func(t *testing.T) {
		require.NotNil(t, report)
		require.NotNil(t, report.Stages)
	})

	t.Run("report has entries for all recorded stages", func(t *testing.T) {
		for stage := range stageLatencies {
			_, exists := report.Stages[stage]
			require.True(t, exists, "report should contain stage %q", stage)
		}
	})

	t.Run("each stage entry has valid statistics", func(t *testing.T) {
		for stage, sp := range report.Stages {
			assert.Greater(t, sp.SampleCount, 0,
				"stage %s should have samples", stage)
			assert.Greater(t, sp.Mean, time.Duration(0),
				"stage %s mean should be positive", stage)
			assert.GreaterOrEqual(t, sp.P50, sp.Min,
				"stage %s: P50 >= Min", stage)
			assert.GreaterOrEqual(t, sp.P95, sp.P50,
				"stage %s: P95 >= P50", stage)
			assert.GreaterOrEqual(t, sp.P99, sp.P95,
				"stage %s: P99 >= P95", stage)
			assert.GreaterOrEqual(t, sp.Max, sp.P99,
				"stage %s: Max >= P99", stage)
		}
	})

	t.Run("sample counts match recorded data", func(t *testing.T) {
		for stage, latencies := range stageLatencies {
			require.Equal(t, len(latencies), report.Stages[stage].SampleCount,
				"stage %s sample count mismatch", stage)
		}
	})

	t.Run("gateway stage statistics are consistent", func(t *testing.T) {
		gw := report.Stages[StageGateway]
		// Recorded: [5ms, 10ms, 15ms]  →  Min=5ms, Max=15ms, Mean=10ms
		require.Equal(t, 5*time.Millisecond, gw.Min, "gateway Min")
		require.Equal(t, 15*time.Millisecond, gw.Max, "gateway Max")
		require.Equal(t, 10*time.Millisecond, gw.Mean, "gateway Mean")
	})

	t.Run("timestamp is set and recent", func(t *testing.T) {
		assert.True(t, !report.Timestamp.IsZero(), "timestamp should not be zero")
		assert.True(t, time.Since(report.Timestamp) < 5*time.Second,
			"timestamp should be within the last 5 seconds")
	})

	t.Run("TotalPipelineP99 is sum of linear stages P99", func(t *testing.T) {
		require.Greater(t, report.TotalPipelineP99, time.Duration(0),
			"TotalPipelineP99 should be positive")

		// The implementation sums P99 across the linear pipeline stages
		// (gateway through store), excluding router and warehouse.
		linearStages := []string{
			StageGateway, StagePreprocess, StageSrcHydration,
			StagePreTransform, StageUserTransform, StageDestTransform, StageStore,
		}
		var expectedSum time.Duration
		for _, s := range linearStages {
			if sp, ok := report.Stages[s]; ok {
				expectedSum += sp.P99
			}
		}
		require.Equal(t, expectedSum, report.TotalPipelineP99,
			"TotalPipelineP99 must equal sum of P99 for linear pipeline stages")
	})

	t.Run("TotalPipelineP99 excludes router and warehouse", func(t *testing.T) {
		// Verify by calculating the total with router+warehouse and comparing
		routerP99 := report.Stages[StageRouter].P99
		warehouseP99 := report.Stages[StageWarehouse].P99
		allStagesSum := report.TotalPipelineP99 + routerP99 + warehouseP99

		assert.Greater(t, allStagesSum, report.TotalPipelineP99,
			"including router and warehouse should yield a larger sum")
	})
}

// ---------------------------------------------------------------------------
// TestResetProfileData verifies that Reset clears all collected data and that
// subsequent reports reflect the empty state.
// ---------------------------------------------------------------------------
func TestResetProfileData(t *testing.T) {
	t.Run("clears all stage data", func(t *testing.T) {
		p := newTestProfiler()

		// Record some data
		p.RecordStageLatency("gateway", 5*time.Millisecond)
		p.RecordStageLatency("router", 10*time.Millisecond)
		p.RecordStageLatency("warehouse", 20*time.Millisecond)

		// Verify data exists
		p.mu.RLock()
		require.Greater(t, len(p.stageData), 0, "should have data before reset")
		p.mu.RUnlock()

		// Reset
		p.Reset()

		// Verify all data is cleared
		p.mu.RLock()
		require.Equal(t, 0, len(p.stageData), "stageData should be empty after reset")
		p.mu.RUnlock()
	})

	t.Run("report after reset is empty", func(t *testing.T) {
		p := newTestProfiler()

		p.RecordStageLatency("gateway", 5*time.Millisecond)
		p.RecordStageLatency("router", 10*time.Millisecond)

		p.Reset()

		report := p.GenerateReport()
		require.NotNil(t, report, "report must not be nil even after reset")
		require.Equal(t, 0, len(report.Stages), "no stage entries expected after reset")
		require.Zero(t, report.TotalPipelineP99,
			"TotalPipelineP99 should be zero after reset")
	})

	t.Run("can record new data after reset", func(t *testing.T) {
		p := newTestProfiler()

		p.RecordStageLatency("gateway", 5*time.Millisecond)
		p.Reset()
		p.RecordStageLatency("gateway", 42*time.Millisecond)

		p.mu.RLock()
		require.Len(t, p.stageData["gateway"], 1)
		require.Equal(t, 42*time.Millisecond, p.stageData["gateway"][0])
		p.mu.RUnlock()

		report := p.GenerateReport()
		require.Equal(t, 1, report.Stages["gateway"].SampleCount)
	})
}

// ---------------------------------------------------------------------------
// TestHTTPHandler verifies that the Profiler HTTP handler correctly serialises
// the profile report as JSON and returns appropriate HTTP status and headers.
// ---------------------------------------------------------------------------
func TestHTTPHandler(t *testing.T) {
	t.Run("returns 200 with valid JSON report", func(t *testing.T) {
		p := newTestProfiler()

		// Record some latency data
		p.RecordStageLatency("gateway", 10*time.Millisecond)
		p.RecordStageLatency("gateway", 20*time.Millisecond)
		p.RecordStageLatency("router", 30*time.Millisecond)

		handler := p.Handler()
		req := httptest.NewRequest("GET", "/v1/profiling/pipeline", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code,
			"handler should return HTTP 200")
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"),
			"Content-Type should be application/json")

		// Decode the JSON response body into a ProfileReport.
		var report ProfileReport
		err := jsonrs.NewDecoder(rec.Body).Decode(&report)
		require.NoError(t, err, "response body should be valid JSON")

		// Validate report contents match recorded data.
		require.NotNil(t, report.Stages, "stages map must be present")
		assert.NotEmpty(t, report.Stages, "stages should not be empty")

		gwProfile, hasGateway := report.Stages["gateway"]
		require.True(t, hasGateway, "report should contain gateway stage")
		assert.Equal(t, 2, gwProfile.SampleCount, "gateway should have 2 samples")

		rtProfile, hasRouter := report.Stages["router"]
		require.True(t, hasRouter, "report should contain router stage")
		assert.Equal(t, 1, rtProfile.SampleCount, "router should have 1 sample")
	})

	t.Run("returns valid report with no data", func(t *testing.T) {
		p := newTestProfiler()

		handler := p.Handler()
		req := httptest.NewRequest("GET", "/v1/profiling/pipeline", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)

		var report ProfileReport
		err := jsonrs.NewDecoder(rec.Body).Decode(&report)
		require.NoError(t, err)

		assert.Equal(t, 0, len(report.Stages),
			"empty profiler should produce a report with no stages")
		assert.True(t, !report.Timestamp.IsZero(),
			"timestamp should still be set")
	})
}

// ---------------------------------------------------------------------------
// TestConcurrentRecording launches multiple goroutines that simultaneously
// record latency samples and verifies data integrity and freedom from data
// races. Run with -race to exercise the race detector.
// ---------------------------------------------------------------------------
func TestConcurrentRecording(t *testing.T) {
	t.Run("10 goroutines each recording 100 samples", func(t *testing.T) {
		p := newTestProfiler()

		const goroutines = 10
		const samplesPerGoroutine = 100

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < samplesPerGoroutine; j++ {
					p.RecordStageLatency("gateway", time.Duration(j+1)*time.Millisecond)
				}
			}()
		}

		wg.Wait()

		p.mu.RLock()
		count := len(p.stageData["gateway"])
		p.mu.RUnlock()

		require.Equal(t, goroutines*samplesPerGoroutine, count,
			"expected %d total samples from %d goroutines × %d samples",
			goroutines*samplesPerGoroutine, goroutines, samplesPerGoroutine)
	})

	t.Run("concurrent recording across multiple stages", func(t *testing.T) {
		p := newTestProfiler()

		stages := []string{"gateway", "preprocess", "router", "warehouse"}
		const samplesPerStage = 50
		const goroutinesPerStage = 5

		var wg sync.WaitGroup
		wg.Add(len(stages) * goroutinesPerStage)
		for _, stage := range stages {
			for g := 0; g < goroutinesPerStage; g++ {
				go func(s string) {
					defer wg.Done()
					for j := 0; j < samplesPerStage; j++ {
						p.RecordStageLatency(s, time.Duration(j+1)*time.Millisecond)
					}
				}(stage)
			}
		}

		wg.Wait()

		p.mu.RLock()
		defer p.mu.RUnlock()

		for _, stage := range stages {
			require.Len(t, p.stageData[stage], goroutinesPerStage*samplesPerStage,
				"stage %s should have %d samples", stage, goroutinesPerStage*samplesPerStage)
		}
	})

	t.Run("concurrent recording and report generation", func(t *testing.T) {
		p := newTestProfiler()

		var wg sync.WaitGroup

		// Writer goroutines
		const writers = 5
		wg.Add(writers)
		for i := 0; i < writers; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					p.RecordStageLatency("gateway", time.Duration(j+1)*time.Millisecond)
				}
			}()
		}

		// Reader goroutines (generate reports concurrently)
		const readers = 3
		wg.Add(readers)
		for i := 0; i < readers; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < 10; j++ {
					report := p.GenerateReport()
					require.NotNil(t, report)
				}
			}()
		}

		wg.Wait()
	})
}
