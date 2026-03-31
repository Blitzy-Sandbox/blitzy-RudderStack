// Package monitoring provides Prometheus metric definitions and registration
// for per-destination delivery monitoring. It is the foundational metrics
// package for the operational tooling sprint (E-036), exposing tagged counters,
// histograms, and gauges that the Router (router/handle.go) calls to instrument
// every delivery attempt.
//
// All metrics follow the rudder-go-kit/stats tagged measurement pattern used
// throughout the codebase (see router/handle_observability.go for reference).
// Metrics are created on-demand via stats.Default.NewTaggedStat() and tagged
// per destination using "destType" and "destinationId" keys matching the
// existing conventions in services/rmetrics/pending_events.go.
package monitoring

import (
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
)

// ---------------------------------------------------------------------------
// Metric name constants
// ---------------------------------------------------------------------------

const (
	// DeliverySuccessTotal is the counter metric name for successful deliveries
	// per destination. Incremented each time the Router confirms a successful
	// event delivery to a downstream destination.
	DeliverySuccessTotal = "delivery_success_total"

	// DeliveryFailureTotal is the counter metric name for failed deliveries per
	// destination. Incremented when the Router exhausts retries or receives a
	// non-retryable error from the destination.
	DeliveryFailureTotal = "delivery_failure_total"

	// DeliveryLatencySeconds is the histogram/timer metric name for delivery
	// latency per destination. Records the wall-clock duration of each delivery
	// attempt so that p50, p95, and p99 percentiles can be derived at the
	// Prometheus query layer.
	DeliveryLatencySeconds = "delivery_latency_seconds"

	// DeliveryThroughputEventsPerSecond is the gauge metric name for delivery
	// throughput per destination. Updated periodically to reflect the current
	// event delivery rate (events/sec) for capacity planning dashboards.
	DeliveryThroughputEventsPerSecond = "delivery_throughput_events_per_second"

	// DeliveryRetryTotal is the counter metric name for delivery retries per
	// destination. Incremented each time the Router schedules a retry attempt
	// for a failed delivery.
	DeliveryRetryTotal = "delivery_retry_total"

	// CircuitBreakerState is the gauge metric name for circuit breaker state per
	// destination. Possible values:
	//   0 = closed  (healthy — traffic flowing normally)
	//   1 = open    (tripped — destination is unhealthy, traffic blocked)
	//   2 = half-open (testing recovery — limited traffic allowed)
	CircuitBreakerState = "circuit_breaker_state"
)

// ---------------------------------------------------------------------------
// Package-level logger (follows services/alert/alertmanager.go pattern)
// ---------------------------------------------------------------------------

var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("monitoring")
}

// ---------------------------------------------------------------------------
// Idempotent metric registration
// ---------------------------------------------------------------------------

// registerOnce ensures RegisterMetrics body executes exactly once regardless of
// how many times it is called. This protects against double-registration when
// multiple subsystems initialise concurrently.
var registerOnce sync.Once

// RegisterMetrics is the package-level initialization entry point. It MUST be
// called at least once during server startup (typically from main.go or
// runner.go) to prepare the monitoring subsystem. The function is safe to call
// multiple times; subsequent calls are no-ops.
//
// Actual metric instances are created lazily via stats.Default.NewTaggedStat()
// inside the recording helper functions, following the on-demand pattern used
// in router/handle_observability.go. RegisterMetrics therefore only logs an
// informational message confirming readiness.
func RegisterMetrics() {
	registerOnce.Do(func() {
		pkgLogger.Infon("Delivery monitoring metrics registered")
	})
}

// ---------------------------------------------------------------------------
// Recording helper functions
// ---------------------------------------------------------------------------
// These functions constitute the public API that router/handle.go and other
// subsystems invoke to instrument delivery events. Each helper creates (or
// retrieves from the stats singleton) a tagged measurement and records a data
// point. The tag keys "destType" and "destinationId" follow the conventions
// established in services/rmetrics/pending_events.go (line 143) and
// router/handle_observability.go (line 138).

// RecordDelivery increments the delivery-success counter for the specified
// destination. Call this after the Router confirms a successful event delivery.
//
// Example usage in router/handle.go:
//
//	monitoring.RecordDelivery(dest.ID, dest.DestinationDefinition.Name)
func RecordDelivery(destinationID, destinationType string) {
	stats.Default.NewTaggedStat(
		DeliverySuccessTotal,
		stats.CountType,
		stats.Tags{
			"destType":      destinationType,
			"destinationId": destinationID,
		},
	).Count(1)
}

// RecordFailure increments the delivery-failure counter for the specified
// destination. Call this when the Router determines a delivery has permanently
// failed (retries exhausted or non-retryable error).
func RecordFailure(destinationID, destinationType string) {
	stats.Default.NewTaggedStat(
		DeliveryFailureTotal,
		stats.CountType,
		stats.Tags{
			"destType":      destinationType,
			"destinationId": destinationID,
		},
	).Count(1)
}

// RecordLatency records the duration of a single delivery attempt for the
// specified destination. The latency is emitted as a timer measurement so that
// the stats backend can compute histogram percentiles (p50, p95, p99).
//
// Pass the wall-clock duration measured between the start of the HTTP request
// to the destination and receipt of the response (or timeout).
func RecordLatency(destinationID, destinationType string, latency time.Duration) {
	stats.Default.NewTaggedStat(
		DeliveryLatencySeconds,
		stats.TimerType,
		stats.Tags{
			"destType":      destinationType,
			"destinationId": destinationID,
		},
	).SendTiming(latency)
}

// RecordRetry increments the delivery-retry counter for the specified
// destination. Call this each time the Router schedules a retry for a
// previously failed delivery attempt.
func RecordRetry(destinationID, destinationType string) {
	stats.Default.NewTaggedStat(
		DeliveryRetryTotal,
		stats.CountType,
		stats.Tags{
			"destType":      destinationType,
			"destinationId": destinationID,
		},
	).Count(1)
}

// RecordCircuitBreakerState sets the circuit breaker gauge for the specified
// destination. The state parameter encodes the breaker's current phase:
//
//	0 = closed    — destination healthy, traffic flowing normally
//	1 = open      — destination unhealthy, all traffic blocked
//	2 = half-open — recovery probe in progress, limited traffic allowed
//
// Call this whenever the circuit breaker transitions between states so the
// operational dashboard reflects the current health posture.
func RecordCircuitBreakerState(destinationID, destinationType string, state int) {
	stats.Default.NewTaggedStat(
		CircuitBreakerState,
		stats.GaugeType,
		stats.Tags{
			"destType":      destinationType,
			"destinationId": destinationID,
		},
	).Gauge(state)
}
