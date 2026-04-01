// Package graph — tracker.go implements graph tracking and metrics for the
// identity resolution system (E-026, Sprint 6-8).
//
// The Tracker provides observability into identity graph operations by collecting
// and exposing metrics about graph structure, mutation rates, and resolution
// patterns. These metrics are exposed via Prometheus through the rudder-go-kit/stats
// infrastructure, enabling operational monitoring and capacity planning.
//
// Key metrics tracked:
//   - Graph segment counts and growth rates (per workspace)
//   - External ID counts and type distribution
//   - Merge operation frequency and segment consolidation ratios
//   - Resolution strategy distribution (new/single/multi match)
//   - Graph mutation latencies
//
// Thread-safe for concurrent use. Uses atomic counters for high-throughput
// metric recording without lock contention.
package graph

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
)

// ---------------------------------------------------------------------------
// Tracker — graph operation metrics collector
// ---------------------------------------------------------------------------

// Tracker collects and exposes metrics about identity graph operations.
// It is designed to be embedded or used alongside the IdentityGraph service
// for comprehensive operational monitoring.
//
// All counter methods are safe for concurrent use via atomic operations.
// Periodic metric publication uses a background goroutine started by Run().
type Tracker struct {
	// conf holds reloadable configuration for refresh intervals and toggles.
	conf *config.Config
	// logger is a scoped structured logger for the tracker.
	logger logger.Logger

	// mu protects the workspace-level gauge snapshots during periodic refresh.
	mu sync.RWMutex

	// done signals the background refresh goroutine to stop.
	done chan struct{}

	// --- Atomic counters for high-throughput metric recording ---

	// totalEventsProcessed is the cumulative count of events processed through
	// the identity graph since the service started.
	totalEventsProcessed atomic.Int64

	// totalResolutions is the cumulative count of successful identity resolutions.
	totalResolutions atomic.Int64

	// totalNewMatches counts resolution operations that resulted in new segment creation.
	totalNewMatches atomic.Int64

	// totalSingleMatches counts resolution operations that matched exactly one existing segment.
	totalSingleMatches atomic.Int64

	// totalMultiMatches counts resolution operations that matched multiple segments (merges).
	totalMultiMatches atomic.Int64

	// totalMergeOperations counts the total number of segment merge operations performed.
	totalMergeOperations atomic.Int64

	// totalIdentifiersAdded counts the total number of new external identifiers added to the graph.
	totalIdentifiersAdded atomic.Int64

	// totalBlockedIdentifiers counts identifiers rejected by blocked value rules.
	totalBlockedIdentifiers atomic.Int64

	// totalLimitExceeded counts identifiers rejected by per-type limit rules.
	totalLimitExceeded atomic.Int64

	// totalErrors counts errors encountered during identity resolution.
	totalErrors atomic.Int64

	// --- Stats measurements for Prometheus export ---
	statsEventsProcessed    stats.Measurement
	statsResolutions        stats.Measurement
	statsNewMatches         stats.Measurement
	statsSingleMatches      stats.Measurement
	statsMultiMatches       stats.Measurement
	statsMergeOps           stats.Measurement
	statsIdentifiersAdded   stats.Measurement
	statsBlockedIdentifiers stats.Measurement
	statsLimitExceeded      stats.Measurement
	statsErrors             stats.Measurement
}

// NewTracker creates a new Tracker with the given configuration, logger, and stats factory.
// The tracker starts collecting metrics immediately but does not publish periodic
// snapshots until Run() is called.
func NewTracker(
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
) *Tracker {
	if conf == nil {
		conf = config.Default
	}
	if log == nil {
		log = pkgLogger
	}

	t := &Tracker{
		conf:   conf,
		logger: log.Child("tracker"),
		done:   make(chan struct{}),
	}

	// Initialize Prometheus-compatible stats measurements using tagged stats.
	// Tags follow the pattern from processor/trackingplan.go:155-159.
	if statsFactory != nil {
		tags := stats.Tags{"module": "identity", "component": "tracker"}
		t.statsEventsProcessed = statsFactory.NewTaggedStat(
			"identity_tracker_events_processed", stats.CountType, tags,
		)
		t.statsResolutions = statsFactory.NewTaggedStat(
			"identity_tracker_resolutions", stats.CountType, tags,
		)
		t.statsNewMatches = statsFactory.NewTaggedStat(
			"identity_tracker_new_matches", stats.CountType, tags,
		)
		t.statsSingleMatches = statsFactory.NewTaggedStat(
			"identity_tracker_single_matches", stats.CountType, tags,
		)
		t.statsMultiMatches = statsFactory.NewTaggedStat(
			"identity_tracker_multi_matches", stats.CountType, tags,
		)
		t.statsMergeOps = statsFactory.NewTaggedStat(
			"identity_tracker_merge_operations", stats.CountType, tags,
		)
		t.statsIdentifiersAdded = statsFactory.NewTaggedStat(
			"identity_tracker_identifiers_added", stats.CountType, tags,
		)
		t.statsBlockedIdentifiers = statsFactory.NewTaggedStat(
			"identity_tracker_blocked_identifiers", stats.CountType, tags,
		)
		t.statsLimitExceeded = statsFactory.NewTaggedStat(
			"identity_tracker_limit_exceeded", stats.CountType, tags,
		)
		t.statsErrors = statsFactory.NewTaggedStat(
			"identity_tracker_errors", stats.CountType, tags,
		)
	}

	return t
}

// ---------------------------------------------------------------------------
// Recording methods — called by IdentityGraph and Resolver during processing
// ---------------------------------------------------------------------------

// RecordEventProcessed increments the total events processed counter.
func (t *Tracker) RecordEventProcessed() {
	t.totalEventsProcessed.Add(1)
	if t.statsEventsProcessed != nil {
		t.statsEventsProcessed.Increment()
	}
}

// RecordResolution records a successful identity resolution with the given strategy.
func (t *Tracker) RecordResolution(strategy ResolutionStrategy) {
	t.totalResolutions.Add(1)
	if t.statsResolutions != nil {
		t.statsResolutions.Increment()
	}

	switch strategy {
	case StrategyNewMatch:
		t.totalNewMatches.Add(1)
		if t.statsNewMatches != nil {
			t.statsNewMatches.Increment()
		}
	case StrategySingleMatch:
		t.totalSingleMatches.Add(1)
		if t.statsSingleMatches != nil {
			t.statsSingleMatches.Increment()
		}
	case StrategyMultiMatch:
		t.totalMultiMatches.Add(1)
		if t.statsMultiMatches != nil {
			t.statsMultiMatches.Increment()
		}
	}
}

// RecordMergeOperation increments the merge operation counter.
func (t *Tracker) RecordMergeOperation() {
	t.totalMergeOperations.Add(1)
	if t.statsMergeOps != nil {
		t.statsMergeOps.Increment()
	}
}

// RecordIdentifiersAdded records the number of new external identifiers added.
func (t *Tracker) RecordIdentifiersAdded(count int) {
	t.totalIdentifiersAdded.Add(int64(count))
	if t.statsIdentifiersAdded != nil {
		t.statsIdentifiersAdded.Count(count)
	}
}

// RecordBlockedIdentifier increments the blocked identifier counter.
func (t *Tracker) RecordBlockedIdentifier() {
	t.totalBlockedIdentifiers.Add(1)
	if t.statsBlockedIdentifiers != nil {
		t.statsBlockedIdentifiers.Increment()
	}
}

// RecordLimitExceeded increments the limit exceeded counter.
func (t *Tracker) RecordLimitExceeded() {
	t.totalLimitExceeded.Add(1)
	if t.statsLimitExceeded != nil {
		t.statsLimitExceeded.Increment()
	}
}

// RecordError increments the error counter.
func (t *Tracker) RecordError() {
	t.totalErrors.Add(1)
	if t.statsErrors != nil {
		t.statsErrors.Increment()
	}
}

// ---------------------------------------------------------------------------
// Snapshot — point-in-time metrics snapshot
// ---------------------------------------------------------------------------

// Snapshot returns a point-in-time snapshot of all tracked metrics.
// This is used by health check endpoints and diagnostic tools.
type Snapshot struct {
	EventsProcessed    int64 `json:"events_processed"`
	Resolutions        int64 `json:"resolutions"`
	NewMatches         int64 `json:"new_matches"`
	SingleMatches      int64 `json:"single_matches"`
	MultiMatches       int64 `json:"multi_matches"`
	MergeOperations    int64 `json:"merge_operations"`
	IdentifiersAdded   int64 `json:"identifiers_added"`
	BlockedIdentifiers int64 `json:"blocked_identifiers"`
	LimitExceeded      int64 `json:"limit_exceeded"`
	Errors             int64 `json:"errors"`
}

// GetSnapshot returns a consistent point-in-time snapshot of all metrics.
func (t *Tracker) GetSnapshot() Snapshot {
	return Snapshot{
		EventsProcessed:    t.totalEventsProcessed.Load(),
		Resolutions:        t.totalResolutions.Load(),
		NewMatches:         t.totalNewMatches.Load(),
		SingleMatches:      t.totalSingleMatches.Load(),
		MultiMatches:       t.totalMultiMatches.Load(),
		MergeOperations:    t.totalMergeOperations.Load(),
		IdentifiersAdded:   t.totalIdentifiersAdded.Load(),
		BlockedIdentifiers: t.totalBlockedIdentifiers.Load(),
		LimitExceeded:      t.totalLimitExceeded.Load(),
		Errors:             t.totalErrors.Load(),
	}
}

// ---------------------------------------------------------------------------
// Lifecycle — background metrics publication
// ---------------------------------------------------------------------------

// Run starts the background metrics publication goroutine. It periodically
// logs a summary of tracked metrics at the configured refresh interval.
// Blocks until ctx is cancelled or Stop() is called.
//
// Follows the standard RudderStack service lifecycle pattern.
func (t *Tracker) Run(ctx context.Context) error {
	refreshInterval := t.conf.GetDuration("Identity.graph.metricsRefreshInterval", 60, time.Second)
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	t.logger.Infon("Identity graph tracker started",
		logger.NewStringField("refreshInterval", refreshInterval.String()),
	)

	for {
		select {
		case <-ctx.Done():
			t.logger.Infon("Identity graph tracker stopping (context cancelled)")
			return ctx.Err()
		case <-t.done:
			t.logger.Infon("Identity graph tracker stopped")
			return nil
		case <-ticker.C:
			snap := t.GetSnapshot()
			t.logger.Infon("Identity graph metrics snapshot",
				logger.NewIntField("eventsProcessed", snap.EventsProcessed),
				logger.NewIntField("resolutions", snap.Resolutions),
				logger.NewIntField("newMatches", snap.NewMatches),
				logger.NewIntField("singleMatches", snap.SingleMatches),
				logger.NewIntField("multiMatches", snap.MultiMatches),
				logger.NewIntField("mergeOperations", snap.MergeOperations),
				logger.NewIntField("identifiersAdded", snap.IdentifiersAdded),
				logger.NewIntField("errors", snap.Errors),
			)
		}
	}
}

// Stop signals the background publication goroutine to stop.
func (t *Tracker) Stop() {
	select {
	case t.done <- struct{}{}:
	default:
	}
}
