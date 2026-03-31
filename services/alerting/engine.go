// Package alerting provides a configurable alerting rules engine for monitoring
// pipeline health conditions across the RudderStack event delivery system.
//
// The AlertEngine periodically evaluates configured alert rules against current
// pipeline metrics (throughput, error rate, delivery failures, warehouse latency,
// and JobsDB queue depth) and dispatches triggered alerts to notification channels
// such as webhooks, email, and Slack.
//
// Architecture:
//   - AlertEngine: Core orchestrator running a periodic evaluation loop
//   - AlertRule / MetricSnapshot / EvaluateRule: Rule definitions and threshold evaluation (rules.go)
//   - NotificationChannel / Alert: Channel interface and alert payload (channels.go)
//
// This file implements the AlertEngine struct, its lifecycle methods (Start, Stop),
// the public Evaluate method for manual/test invocations, and the private evaluation
// loop that periodically fetches enabled rules, builds metric snapshots, evaluates
// thresholds, and dispatches triggered alerts.
package alerting

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// ---------------------------------------------------------------------------
// Package-level logger — initialized in init() following the codebase pattern
// from services/alerta/client.go (line 132) and services/alert/alertmanager.go
// (line 19). The scoped child logger name "alerting" matches this package.
// ---------------------------------------------------------------------------

var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("alerting")
}

// ---------------------------------------------------------------------------
// MetricCollector — interface for collecting pipeline metrics
// ---------------------------------------------------------------------------

// MetricCollector provides an abstraction for reading current pipeline metric
// values. Implementations may source data from in-memory aggregations (e.g.,
// the monitoring dashboard), Prometheus gauge registries, or other stats
// backends. The alerting engine uses this interface to populate MetricSnapshot
// values for threshold evaluation.
type MetricCollector interface {
	// CollectMetrics returns a snapshot of current pipeline metric values.
	// Implementations should populate all fields that are available; zero
	// values indicate that the metric is either unknown or at its zero state.
	CollectMetrics() MetricSnapshot
}

// ---------------------------------------------------------------------------
// AlertEngine — core alerting rules engine
// ---------------------------------------------------------------------------

// AlertEngine periodically evaluates configured alert rules against current
// pipeline metrics and dispatches triggered alerts to notification channels.
//
// The engine follows a pull-based evaluation model: on each tick of the
// configurable evaluation interval, it fetches all enabled rules from the
// RuleRepository, builds a MetricSnapshot from the MetricCollector,
// evaluates each rule's threshold condition, and dispatches any triggered
// alerts to the rule's configured notification channels.
//
// Lifecycle:
//   - Create with NewAlertEngine
//   - Call Start(ctx) to begin the periodic evaluation loop
//   - Call Stop() to gracefully shut down
//   - Optionally call Evaluate(ctx, rules, snapshot) for manual/test evaluation
//
// Thread Safety:
//
//	The engine is safe for concurrent use. The evaluation loop runs in its own
//	goroutine, and the channels map is protected by an RWMutex for concurrent reads.
type AlertEngine struct {
	// config provides access to reloadable configuration values, specifically
	// the evaluation interval (Monitoring.alerting.evaluationInterval).
	config *config.Config

	// logger is the scoped logger instance for this engine. Uses the package-level
	// pkgLogger as fallback if nil is provided to the constructor.
	logger logger.Logger

	// channels maps notification channel names to their implementations.
	// The map key is the channel identifier referenced by AlertRule.Channels.
	channels map[string]NotificationChannel

	// ruleRepo provides CRUD operations for alert rules. The evaluation loop
	// calls ListEnabled to fetch the set of rules to evaluate on each tick.
	ruleRepo RuleRepository

	// metricCollector provides access to current pipeline metric values for
	// building MetricSnapshot during periodic evaluation. When nil, the engine
	// returns zero-valued snapshots and logs a warning.
	metricCollector MetricCollector

	// evalInterval is the duration between consecutive evaluation cycles.
	// Loaded from config key "Monitoring.alerting.evaluationInterval" with a default of 30s.
	evalInterval time.Duration

	// cancel is the context cancellation function used to signal the evaluation
	// loop goroutine to stop during graceful shutdown.
	cancel context.CancelFunc

	// wg tracks the evaluation loop goroutine for clean shutdown synchronization.
	wg sync.WaitGroup

	// mu protects concurrent access to the engine's mutable state (channels map,
	// cancel function).
	mu sync.RWMutex
}

// NewAlertEngine creates a new AlertEngine with the provided dependencies.
//
// Parameters:
//   - conf: Configuration provider for reading the evaluation interval. Must not be nil.
//   - log: Scoped logger instance. If nil, the package-level pkgLogger is used.
//   - channels: Map of channel name to NotificationChannel implementation. If nil,
//     an empty map is used (alerts will be evaluated but not dispatched).
//   - ruleRepo: Repository for fetching alert rules. Must not be nil for the
//     periodic evaluation loop to function.
//   - metricCollector: Provider for current pipeline metric values. May be nil if
//     metrics are provided externally via the Evaluate method; in that case,
//     the periodic evaluation loop logs warnings and uses zero-valued snapshots.
//
// The evaluation interval is read from the config key "Monitoring.alerting.evaluationInterval"
// with a default of 30 seconds, matching the config.yaml path under the Monitoring section.
// This follows the config.GetDuration pattern used in services/alert/pagerduty.go.
func NewAlertEngine(
	conf *config.Config,
	log logger.Logger,
	channels map[string]NotificationChannel,
	ruleRepo RuleRepository,
	metricCollector MetricCollector,
) *AlertEngine {
	if log == nil {
		log = pkgLogger
	}
	if channels == nil {
		channels = make(map[string]NotificationChannel)
	}

	// Load the configurable evaluation interval from the Monitoring.alerting section
	// of config.yaml. Default is 30s to match the config.yaml definition.
	evalInterval := conf.GetDuration("Monitoring.alerting.evaluationInterval", 30, time.Second)

	return &AlertEngine{
		config:          conf,
		logger:          log,
		channels:        channels,
		ruleRepo:        ruleRepo,
		metricCollector: metricCollector,
		evalInterval:    evalInterval,
	}
}

// ---------------------------------------------------------------------------
// Lifecycle Methods — Start, Stop
// ---------------------------------------------------------------------------

// Start begins the periodic evaluation loop in a background goroutine.
// The loop runs until the provided context is cancelled or Stop is called.
//
// Start checks the "Monitoring.alerting.enabled" configuration flag before
// launching the evaluation goroutine. If alerting is disabled (the default),
// Start returns nil immediately without starting any background work. This
// config-gating ensures the engine imposes zero overhead when not needed.
//
// Start creates a derived context with its own cancel function, ensuring that
// Stop can cleanly shut down the evaluation loop independently of the parent
// context's lifecycle.
//
// Returns nil on successful startup. The evaluation loop goroutine is tracked
// via sync.WaitGroup for clean shutdown synchronization in Stop.
func (e *AlertEngine) Start(ctx context.Context) error {
	// Config-gated: alerting is disabled by default per config.yaml.
	if !e.config.GetBool("Monitoring.alerting.enabled", false) {
		e.logger.Infon("Alerting engine disabled via Monitoring.alerting.enabled config")
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)

	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()

	e.wg.Add(1)
	go e.evaluationLoop(ctx)

	e.logger.Infon("Alerting engine started",
		logger.NewStringField("evalInterval", e.evalInterval.String()))
	return nil
}

// Stop gracefully shuts down the alerting engine by cancelling the evaluation
// loop's context and waiting for the goroutine to complete. Stop blocks until
// the evaluation loop has fully exited.
//
// It is safe to call Stop multiple times; subsequent calls are no-ops if the
// cancel function has already been invoked.
func (e *AlertEngine) Stop() {
	e.mu.RLock()
	cancel := e.cancel
	e.mu.RUnlock()

	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
	e.logger.Infon("Alerting engine stopped")
}

// ---------------------------------------------------------------------------
// Evaluation Loop — periodic rule evaluation
// ---------------------------------------------------------------------------

// evaluationLoop runs the periodic evaluation cycle. It ticks at the configured
// evalInterval, fetching enabled rules and evaluating them against current metrics
// on each tick. The loop exits cleanly when the context is cancelled.
func (e *AlertEngine) evaluationLoop(ctx context.Context) {
	defer e.wg.Done()

	ticker := time.NewTicker(e.evalInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			e.logger.Infon("Alerting engine evaluation loop stopped")
			return
		case <-ticker.C:
			e.evaluateAll(ctx)
		}
	}
}

// evaluateAll fetches all enabled rules from the repository, builds a metric
// snapshot, evaluates each rule against the snapshot, and dispatches triggered
// alerts to the configured notification channels.
//
// This method is called on each tick of the evaluation loop. Errors in fetching
// rules or evaluating individual rules are logged but do not halt the evaluation
// of remaining rules — the engine follows a best-effort approach to maximize
// alert coverage even when individual rules encounter transient issues.
func (e *AlertEngine) evaluateAll(ctx context.Context) {
	// Fetch all enabled rules from the repository.
	rules, err := e.ruleRepo.ListEnabled(ctx)
	if err != nil {
		e.logger.Errorn("Failed to fetch enabled rules", obskit.Error(err))
		return
	}

	if len(rules) == 0 {
		return
	}

	// Build a snapshot of current pipeline metrics.
	snapshot := e.buildMetricSnapshot()

	// Evaluate each rule against the snapshot.
	for i := range rules {
		rule := rules[i]

		triggered, evalErr := EvaluateRule(rule, snapshot)
		if evalErr != nil {
			e.logger.Errorn("Failed to evaluate rule",
				logger.NewStringField("ruleID", rule.ID),
				logger.NewStringField("condition", string(rule.Condition)),
				obskit.Error(evalErr))
			continue
		}

		if !triggered {
			continue
		}

		alert := buildAlertFromRule(rule, snapshot)

		e.logger.Warnn("Alert triggered",
			logger.NewStringField("ruleID", rule.ID),
			logger.NewStringField("condition", string(rule.Condition)))

		e.dispatchAlert(ctx, alert, rule.Channels)
	}
}

// ---------------------------------------------------------------------------
// Alert Dispatch — fan-out to notification channels
// ---------------------------------------------------------------------------

// dispatchAlert sends the given alert to all specified notification channels.
// Each channel is attempted independently — a failure in one channel does not
// prevent delivery to other channels. Errors are logged with structured fields
// following the obskit.Error pattern from services/alert/pagerduty.go.
//
// If a channel name is not found in the engine's channels map, a warning is
// logged and the channel is skipped without error.
func (e *AlertEngine) dispatchAlert(ctx context.Context, alert Alert, channelNames []string) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, name := range channelNames {
		ch, ok := e.channels[name]
		if !ok {
			e.logger.Warnn("Notification channel not found",
				logger.NewStringField("channel", name),
				logger.NewStringField("ruleID", alert.RuleID))
			continue
		}

		if err := ch.Send(ctx, alert); err != nil {
			e.logger.Errorn("Failed to send alert",
				logger.NewStringField("channel", name),
				logger.NewStringField("ruleID", alert.RuleID),
				obskit.Error(err))
			// Continue to next channel — do NOT fail fast.
		}
	}
}

// ---------------------------------------------------------------------------
// Public Evaluate — manual/test rule evaluation
// ---------------------------------------------------------------------------

// Evaluate evaluates the given rules against the provided metric snapshot and
// dispatches triggered alerts to the engine's configured notification channels.
//
// This method provides a synchronous, non-periodic evaluation path suitable for:
//   - Unit and integration testing without starting the periodic loop
//   - Manual one-shot evaluation triggered by an API endpoint
//   - External callers that manage their own scheduling
//
// Parameters:
//   - ctx: Context for cancellation and deadline propagation during dispatch.
//   - rules: The set of alert rules to evaluate. Rules with Enabled=false are skipped.
//   - snapshot: The current pipeline metric values to evaluate rules against.
//
// Returns a slice of Alert structs for all rules that were triggered.
// Evaluation errors for individual rules are logged but do not prevent
// evaluation of remaining rules.
func (e *AlertEngine) Evaluate(ctx context.Context, rules []AlertRule, snapshot MetricSnapshot) []Alert {
	var triggered []Alert

	for i := range rules {
		rule := rules[i]

		isTriggered, err := EvaluateRule(rule, snapshot)
		if err != nil {
			e.logger.Errorn("Failed to evaluate rule",
				logger.NewStringField("ruleID", rule.ID),
				logger.NewStringField("condition", string(rule.Condition)),
				obskit.Error(err))
			continue
		}

		if !isTriggered {
			continue
		}

		alert := buildAlertFromRule(rule, snapshot)
		triggered = append(triggered, alert)
		e.dispatchAlert(ctx, alert, rule.Channels)
	}

	return triggered
}

// ---------------------------------------------------------------------------
// Alert Builder Helper
// ---------------------------------------------------------------------------

// buildAlertFromRule constructs an Alert payload from a triggered rule and
// the metric snapshot that caused the trigger. This centralises the alert
// construction logic that is shared by both evaluateAll (periodic) and
// Evaluate (manual/test) code paths.
func buildAlertFromRule(rule AlertRule, snapshot MetricSnapshot) Alert {
	metricValue, _ := getMetricValue(rule.Condition, snapshot)
	return Alert{
		RuleID:      rule.ID,
		Condition:   rule.Condition,
		Message:     fmt.Sprintf("Alert triggered: %s metric value %.2f breached threshold %.2f", rule.Condition, metricValue, rule.Threshold),
		Value:       metricValue,
		Threshold:   rule.Threshold,
		WorkspaceID: rule.WorkspaceID,
		Timestamp:   time.Now(),
	}
}

// ---------------------------------------------------------------------------
// Metric Snapshot Builder
// ---------------------------------------------------------------------------

// buildMetricSnapshot collects current pipeline metric values from the
// configured MetricCollector and returns a populated MetricSnapshot.
//
// When a MetricCollector is provided (via NewAlertEngine), it delegates
// to the collector to obtain real-time metric values from the monitoring
// dashboard or stats infrastructure. When no collector is configured, the
// method returns a zero-valued snapshot and logs a warning to aid debugging.
//
// The CollectedAt timestamp is always set to the current time to enable
// staleness detection by downstream consumers.
func (e *AlertEngine) buildMetricSnapshot() MetricSnapshot {
	if e.metricCollector != nil {
		snapshot := e.metricCollector.CollectMetrics()
		// Ensure CollectedAt is always fresh regardless of collector implementation.
		snapshot.CollectedAt = time.Now()
		return snapshot
	}

	e.logger.Warnn("No metric collector configured — returning zero-valued snapshot; alerts may not fire correctly")
	return MetricSnapshot{
		Throughput:              0,
		ErrorRate:               0,
		DeliveryFailureCount:    0,
		WarehouseLatencySeconds: 0,
		JobsDBQueueDepth:        0,
		CollectedAt:             time.Now(),
	}
}
