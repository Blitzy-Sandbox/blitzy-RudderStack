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
	"github.com/rudderlabs/rudder-go-kit/stats"
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
// AlertEngine — core alerting rules engine
// ---------------------------------------------------------------------------

// AlertEngine periodically evaluates configured alert rules against current
// pipeline metrics and dispatches triggered alerts to notification channels.
//
// The engine follows a pull-based evaluation model: on each tick of the
// configurable evaluation interval, it fetches all enabled rules from the
// RuleRepository, builds a MetricSnapshot from the stats infrastructure,
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
	// the evaluation interval (Alerting.evaluationInterval).
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

	// statsFactory provides access to the shared metrics infrastructure for
	// reading current pipeline metric values during snapshot construction.
	statsFactory stats.Stats

	// evalInterval is the duration between consecutive evaluation cycles.
	// Loaded from config key "Alerting.evaluationInterval" with a default of 60s.
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
//   - statsFactory: Stats provider for building metric snapshots. May be nil if
//     metrics are provided externally via the Evaluate method.
//
// The evaluation interval is read from the config key "Alerting.evaluationInterval"
// with a default of 60 seconds. This follows the config.GetDuration pattern used
// in services/alert/pagerduty.go and services/alert/victorops.go.
func NewAlertEngine(
	conf *config.Config,
	log logger.Logger,
	channels map[string]NotificationChannel,
	ruleRepo RuleRepository,
	statsFactory stats.Stats,
) *AlertEngine {
	if log == nil {
		log = pkgLogger
	}
	if channels == nil {
		channels = make(map[string]NotificationChannel)
	}

	// Load the configurable evaluation interval following the pattern from
	// services/alert/pagerduty.go line 35: config.GetDuration("key", default, unit)
	evalInterval := conf.GetDuration("Alerting.evaluationInterval", 60, time.Second)

	return &AlertEngine{
		config:       conf,
		logger:       log,
		channels:     channels,
		ruleRepo:     ruleRepo,
		statsFactory: statsFactory,
		evalInterval: evalInterval,
	}
}

// ---------------------------------------------------------------------------
// Lifecycle Methods — Start, Stop
// ---------------------------------------------------------------------------

// Start begins the periodic evaluation loop in a background goroutine.
// The loop runs until the provided context is cancelled or Stop is called.
//
// Start creates a derived context with its own cancel function, ensuring that
// Stop can cleanly shut down the evaluation loop independently of the parent
// context's lifecycle.
//
// Returns nil on successful startup. The evaluation loop goroutine is tracked
// via sync.WaitGroup for clean shutdown synchronization in Stop.
func (e *AlertEngine) Start(ctx context.Context) error {
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

		// Rule triggered — build the alert payload.
		metricValue, _ := getMetricValue(rule.Condition, snapshot)
		alert := Alert{
			RuleID:      rule.ID,
			Condition:   rule.Condition,
			Message:     fmt.Sprintf("Alert triggered: %s metric value %.2f breached threshold %.2f", rule.Condition, metricValue, rule.Threshold),
			Value:       metricValue,
			Threshold:   rule.Threshold,
			WorkspaceID: rule.WorkspaceID,
			Timestamp:   time.Now(),
		}

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

		// Build the alert payload from the triggered rule and current metric value.
		metricValue, _ := getMetricValue(rule.Condition, snapshot)
		alert := Alert{
			RuleID:      rule.ID,
			Condition:   rule.Condition,
			Message:     fmt.Sprintf("Alert triggered: %s metric value %.2f breached threshold %.2f", rule.Condition, metricValue, rule.Threshold),
			Value:       metricValue,
			Threshold:   rule.Threshold,
			WorkspaceID: rule.WorkspaceID,
			Timestamp:   time.Now(),
		}

		triggered = append(triggered, alert)
		e.dispatchAlert(ctx, alert, rule.Channels)
	}

	return triggered
}

// ---------------------------------------------------------------------------
// Metric Snapshot Builder
// ---------------------------------------------------------------------------

// buildMetricSnapshot collects current pipeline metric values from the stats
// infrastructure and returns a populated MetricSnapshot.
//
// The stats.Stats interface (from rudder-go-kit/stats) is primarily a metrics
// recording interface (NewStat, NewTaggedStat) rather than a metrics reading
// interface. In production deployments, metric values are typically collected
// from Prometheus queries or internal gauges. This implementation returns a
// baseline snapshot with zero values as a starting point.
//
// For production use with real metric values, callers should either:
//  1. Use the public Evaluate method with a pre-populated MetricSnapshot
//  2. Extend this method to read from a metrics provider (e.g., Prometheus
//     HTTP API or internal gauge registry)
//
// The CollectedAt timestamp is always set to the current time to enable
// staleness detection by downstream consumers.
func (e *AlertEngine) buildMetricSnapshot() MetricSnapshot {
	return MetricSnapshot{
		Throughput:              0,
		ErrorRate:               0,
		DeliveryFailureCount:    0,
		WarehouseLatencySeconds: 0,
		JobsDBQueueDepth:        0,
		CollectedAt:             time.Now(),
	}
}
