// Package alerting_test provides comprehensive unit tests for the AlertEngine
// defined in engine.go. Tests cover the core alerting rules engine lifecycle
// (Start, Stop, Evaluate) and metric-driven alert dispatch, including
// threshold-based triggering, disabled rule skipping, and periodic evaluation.
//
// This file uses the external test package pattern (alerting_test) consistent
// with services/alerta/client_test.go. All assertions use testify/require
// following the established codebase convention. Mock implementations of
// NotificationChannel, RuleRepository, and MetricCollector are provided
// inline with atomic counters for thread-safe concurrent test assertions.
package alerting_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/services/alerting"
)

// ---------------------------------------------------------------------------
// Mock types for test isolation — no external service dependencies
// ---------------------------------------------------------------------------

// mockChannel implements alerting.NotificationChannel with an atomic send
// counter for thread-safe invocation counting across concurrent test assertions.
// This follows the pattern from services/alerta/client_test.go which uses
// atomic.AddInt64 for counting HTTP handler invocations.
type mockChannel struct {
	sendCount atomic.Int64
}

// Send records an alert dispatch by incrementing the atomic counter.
// Always returns nil — test scenarios requiring error behavior should
// use a separate error-returning mock.
func (m *mockChannel) Send(_ context.Context, _ alerting.Alert) error {
	m.sendCount.Add(1)
	return nil
}

// mockRuleRepo implements alerting.RuleRepository with in-memory storage.
// Only ListEnabled is functionally implemented since the periodic evaluation
// loop calls this method. Other CRUD methods return zero values and nil
// errors to satisfy the interface contract without side effects.
type mockRuleRepo struct {
	rules []alerting.AlertRule
}

func (m *mockRuleRepo) Create(_ context.Context, _ alerting.AlertRule) (string, error) {
	return "", nil
}

func (m *mockRuleRepo) Get(_ context.Context, _ string) (alerting.AlertRule, error) {
	return alerting.AlertRule{}, nil
}

func (m *mockRuleRepo) Update(_ context.Context, _ alerting.AlertRule) error {
	return nil
}

func (m *mockRuleRepo) Delete(_ context.Context, _ string) error {
	return nil
}

func (m *mockRuleRepo) List(_ context.Context, _ string) ([]alerting.AlertRule, error) {
	return nil, nil
}

// ListEnabled returns all rules configured in the mock that have Enabled=true.
// This is the primary method used by the AlertEngine's periodic evaluation loop.
func (m *mockRuleRepo) ListEnabled(_ context.Context) ([]alerting.AlertRule, error) {
	var enabled []alerting.AlertRule
	for _, r := range m.rules {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	return enabled, nil
}

// mockCollector implements alerting.MetricCollector, returning a fixed
// MetricSnapshot for predictable test assertions. The periodic evaluation
// loop calls CollectMetrics on each tick to obtain current pipeline metrics.
type mockCollector struct {
	snapshot alerting.MetricSnapshot
}

// CollectMetrics returns the preconfigured snapshot for testing.
func (m *mockCollector) CollectMetrics() alerting.MetricSnapshot {
	return m.snapshot
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestLogger creates a scoped logger for test isolation, matching the
// pattern from services/alerta/client.go line 132 and services/alert/alertmanager.go line 19.
func newTestLogger() logger.Logger {
	return logger.NewLogger().Child("alerting-test")
}

// ---------------------------------------------------------------------------
// Test 1: TestAlertEngine_StartStop
// ---------------------------------------------------------------------------

// TestAlertEngine_StartStop verifies the AlertEngine lifecycle: creating an
// engine, starting the periodic evaluation loop, and stopping it cleanly
// without errors or goroutine leaks. A context.WithTimeout guard prevents
// the test from hanging if shutdown fails.
func TestAlertEngine_StartStop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := config.New()
	// Enable the alerting engine so Start actually launches the evaluation loop
	c.Set("Monitoring.alerting.enabled", true)
	// Use a long evaluation interval to avoid actual evaluations during this lifecycle test
	c.Set("Monitoring.alerting.evaluationInterval", "10s")

	ch := &mockChannel{}
	channels := map[string]alerting.NotificationChannel{
		"noop": ch,
	}

	repo := &mockRuleRepo{}
	engine := alerting.NewAlertEngine(c, newTestLogger(), channels, repo, nil)

	// Start should succeed without error
	err := engine.Start(ctx)
	require.NoError(t, err)

	// Stop should complete without hanging or panicking within the timeout
	engine.Stop()

	// Verify no alerts were dispatched during the lifecycle test
	require.Equal(t, int64(0), ch.sendCount.Load())
}

// ---------------------------------------------------------------------------
// Test 2: TestAlertEngine_EvaluateTriggersAlert
// ---------------------------------------------------------------------------

// TestAlertEngine_EvaluateTriggersAlert verifies that when a rule's threshold
// condition is met (Throughput < 100 with actual value 50), the engine
// dispatches exactly one alert to the notification channel and returns the
// correct alert payload with rule ID, condition, and metric value.
func TestAlertEngine_EvaluateTriggersAlert(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	c := config.New()
	log := newTestLogger()

	ch := &mockChannel{}
	channels := map[string]alerting.NotificationChannel{
		"webhook": ch,
	}

	engine := alerting.NewAlertEngine(c, log, channels, &mockRuleRepo{}, nil)

	rules := []alerting.AlertRule{
		{
			ID:                 "rule-trigger-1",
			WorkspaceID:        "ws-test",
			Condition:          alerting.ThroughputDrop,
			Threshold:          100.0,
			ComparisonOperator: alerting.LessThan,
			Enabled:            true,
			Channels:           []string{"webhook"},
		},
	}

	// Throughput 50 < Threshold 100 with LessThan operator → triggers alert
	snapshot := alerting.MetricSnapshot{
		Throughput:  50.0,
		CollectedAt: time.Now(),
	}

	alerts := engine.Evaluate(ctx, rules, snapshot)

	// Verify the notification channel's Send was invoked exactly once
	require.Equal(t, int64(1), ch.sendCount.Load())

	// Verify one alert was returned
	require.Equal(t, 1, len(alerts))

	// Verify the alert payload contains the rule ID, condition, and current metric value
	require.Equal(t, "rule-trigger-1", alerts[0].RuleID)
	require.Equal(t, alerting.ThroughputDrop, alerts[0].Condition)
	require.Equal(t, 50.0, alerts[0].Value)
	require.Equal(t, 100.0, alerts[0].Threshold)
	require.Equal(t, "ws-test", alerts[0].WorkspaceID)
	require.False(t, alerts[0].Timestamp.IsZero())
}

// ---------------------------------------------------------------------------
// Test 3: TestAlertEngine_EvaluateSkipsDisabledRule
// ---------------------------------------------------------------------------

// TestAlertEngine_EvaluateSkipsDisabledRule verifies that disabled rules
// (Enabled=false) are completely skipped during evaluation — no threshold
// comparison is performed and no alerts are dispatched.
func TestAlertEngine_EvaluateSkipsDisabledRule(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	c := config.New()
	log := newTestLogger()

	ch := &mockChannel{}
	channels := map[string]alerting.NotificationChannel{
		"webhook": ch,
	}

	engine := alerting.NewAlertEngine(c, log, channels, &mockRuleRepo{}, nil)

	rules := []alerting.AlertRule{
		{
			ID:                 "rule-disabled-1",
			WorkspaceID:        "ws-test",
			Condition:          alerting.ThroughputDrop,
			Threshold:          100.0,
			ComparisonOperator: alerting.LessThan,
			Enabled:            false, // Disabled — must be skipped
			Channels:           []string{"webhook"},
		},
	}

	// Even though throughput 50 < threshold 100 would trigger, the rule is disabled
	snapshot := alerting.MetricSnapshot{
		Throughput:  50.0,
		CollectedAt: time.Now(),
	}

	alerts := engine.Evaluate(ctx, rules, snapshot)

	// Verify Send was NOT invoked
	require.Equal(t, int64(0), ch.sendCount.Load())

	// Verify no alerts were returned
	require.Equal(t, 0, len(alerts))
}

// ---------------------------------------------------------------------------
// Test 4: TestAlertEngine_EvaluateNoAlertBelowThreshold
// ---------------------------------------------------------------------------

// TestAlertEngine_EvaluateNoAlertBelowThreshold verifies that when the
// metric value does NOT satisfy the threshold condition (Throughput 200
// is NOT less than 100), no alert is triggered and no notification is sent.
func TestAlertEngine_EvaluateNoAlertBelowThreshold(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	c := config.New()
	log := newTestLogger()

	ch := &mockChannel{}
	channels := map[string]alerting.NotificationChannel{
		"webhook": ch,
	}

	engine := alerting.NewAlertEngine(c, log, channels, &mockRuleRepo{}, nil)

	rules := []alerting.AlertRule{
		{
			ID:                 "rule-no-trigger-1",
			WorkspaceID:        "ws-test",
			Condition:          alerting.ThroughputDrop,
			Threshold:          100.0,
			ComparisonOperator: alerting.LessThan,
			Enabled:            true,
			Channels:           []string{"webhook"},
		},
	}

	// Throughput 200 is NOT < 100 → does NOT trigger LessThan condition
	snapshot := alerting.MetricSnapshot{
		Throughput:  200.0,
		CollectedAt: time.Now(),
	}

	alerts := engine.Evaluate(ctx, rules, snapshot)

	// Verify Send was NOT invoked
	require.Equal(t, int64(0), ch.sendCount.Load())

	// Verify no alerts were returned
	require.Equal(t, 0, len(alerts))
}

// ---------------------------------------------------------------------------
// Test 5: TestAlertEngine_PeriodicEvaluation
// ---------------------------------------------------------------------------

// TestAlertEngine_PeriodicEvaluation verifies that the engine's background
// evaluation loop periodically evaluates rules and dispatches alerts. The
// test configures a very short evaluation interval (50ms), starts the engine
// with a rule that always triggers, waits 200ms, stops the engine, and
// verifies the notification channel was called multiple times (at least 2).
// Uses atomic.Int64 for thread-safe invocation counting across the
// evaluation goroutine and the test assertion goroutine.
func TestAlertEngine_PeriodicEvaluation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := config.New()
	// Enable the alerting engine to start the periodic evaluation loop
	c.Set("Monitoring.alerting.enabled", true)
	// Set a short evaluation interval (50ms) for fast test execution.
	// rudder-go-kit's GetDuration supports parsing Go duration strings
	// via time.ParseDuration, so "50ms" is interpreted directly.
	c.Set("Monitoring.alerting.evaluationInterval", "50ms")

	ch := &mockChannel{}
	channels := map[string]alerting.NotificationChannel{
		"webhook": ch,
	}

	// Configure a rule that always triggers: ThroughputDrop with threshold 100
	// and a metric collector returning throughput=50 (50 < 100 → always triggers)
	repo := &mockRuleRepo{
		rules: []alerting.AlertRule{
			{
				ID:                 "rule-periodic-1",
				WorkspaceID:        "ws-test",
				Condition:          alerting.ThroughputDrop,
				Threshold:          100.0,
				ComparisonOperator: alerting.LessThan,
				Enabled:            true,
				Channels:           []string{"webhook"},
			},
		},
	}

	collector := &mockCollector{
		snapshot: alerting.MetricSnapshot{
			Throughput:  50.0,
			CollectedAt: time.Now(),
		},
	}

	engine := alerting.NewAlertEngine(c, newTestLogger(), channels, repo, collector)

	err := engine.Start(ctx)
	require.NoError(t, err)

	// Wait 200ms to allow at least 2-3 evaluation cycles at 50ms intervals
	time.Sleep(200 * time.Millisecond)

	engine.Stop()

	// Verify the notification channel was called multiple times (at least 2)
	sendCount := ch.sendCount.Load()
	require.GreaterOrEqual(t, sendCount, int64(2),
		"expected at least 2 alert dispatches during periodic evaluation, got %d", sendCount)
}
