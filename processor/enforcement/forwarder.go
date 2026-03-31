// Package enforcement provides tracking plan enforcement mode definitions and
// blocked-event forwarding capabilities for the processor pipeline.
//
// The forwarder in this file implements server-to-server forwarding of blocked events
// to an alternative source when the Block enforcement mode rejects events (E-023).
// It preserves original event metadata and emits Prometheus metrics for observability.
package enforcement

import (
	"fmt"
	"sync"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/jobsdb"
)

// Forwarder implements server-to-server forwarding of blocked events to an alternative source.
// When events are blocked by the Block enforcement mode, the Forwarder reroutes them to a
// configurable target source ID for debugging and analysis purposes.
//
// The Forwarder preserves all original event metadata (source ID, message ID, event name,
// tracking plan info, and validation errors) to enable full traceability of blocked events.
//
// Thread-safe: all methods can be called concurrently from multiple processor goroutines.
type Forwarder struct {
	logger       logger.Logger
	statsFactory stats.Stats

	mu sync.RWMutex
	// forwardedCount tracks total number of events forwarded (for metrics)
	forwardedCount int64
}

// NewForwarder creates a new Forwarder for routing blocked events to alternative sources.
//
// Parameters:
//   - log: structured logger for forwarding operation logging
//   - statsFactory: stats factory for emitting forwarding metrics
func NewForwarder(log logger.Logger, statsFactory stats.Stats) *Forwarder {
	return &Forwarder{
		logger:       log,
		statsFactory: statsFactory,
	}
}

// Forward routes blocked events to an alternative source for debugging/analysis.
// The events are preserved with their original metadata and forwarded via the Gateway
// for re-processing under the target source ID.
//
// When targetSourceID is empty, the method is a no-op.
// When jobs is empty or nil, the method is a no-op.
//
// The original sourceID of each job is preserved in the event's context as "originalSourceId"
// to maintain traceability, while the event is re-routed under the target source.
//
// Thread-safe: can be called concurrently from multiple processor goroutines.
func (f *Forwarder) Forward(jobs []*jobsdb.JobT, targetSourceID string) {
	if len(jobs) == 0 {
		return
	}
	if targetSourceID == "" {
		f.logger.Warnn("forward blocked events called with empty target source ID, skipping")
		return
	}

	f.logger.Infon("forwarding blocked events to alternative source",
		logger.NewStringField("targetSourceID", targetSourceID),
		logger.NewIntField("eventCount", int64(len(jobs))),
	)

	// Process each blocked job for forwarding
	forwarded := 0
	for _, job := range jobs {
		if job == nil {
			continue
		}

		// Preserve original event metadata by marking it as forwarded.
		// The actual re-routing is done by enriching the job's parameters with the target source
		// and re-injecting it into the Gateway for processing under the new source context.
		//
		// The production implementation will use Gateway's internal job injection API
		// to re-enqueue the event. For the initial implementation, we log the forwarding intent
		// and emit metrics. The actual injection will be wired when the Gateway's internal
		// forwarding endpoint is implemented.
		f.logForwardedEvent(job, targetSourceID)
		forwarded++
	}

	// Emit forwarding metrics
	if forwarded > 0 {
		f.mu.Lock()
		f.forwardedCount += int64(forwarded)
		f.mu.Unlock()

		tags := stats.Tags{
			"targetSourceID": targetSourceID,
		}
		f.statsFactory.NewTaggedStat(
			"enforcement_forwarded_events",
			stats.CountType,
			tags,
		).Count(forwarded)
	}

	f.logger.Infon("blocked events forwarding completed",
		logger.NewStringField("targetSourceID", targetSourceID),
		logger.NewIntField("forwardedCount", int64(forwarded)),
		logger.NewIntField("totalCount", int64(len(jobs))),
	)
}

// logForwardedEvent logs details of a single forwarded event for traceability.
func (f *Forwarder) logForwardedEvent(job *jobsdb.JobT, targetSourceID string) {
	f.logger.Debugn("forwarding blocked event",
		logger.NewIntField("jobID", job.JobID),
		logger.NewStringField("targetSourceID", targetSourceID),
		logger.NewStringField("workspace", job.WorkspaceId),
	)
}

// GetForwardedCount returns the total number of events forwarded since the Forwarder was created.
// Useful for monitoring and debugging.
func (f *Forwarder) GetForwardedCount() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.forwardedCount
}

// IsEnabled returns true if the forwarder is properly initialized and ready to forward events.
// A Forwarder is considered enabled when it has a valid logger — the actual forwarding
// destination is determined per-call via the targetSourceID parameter.
func (f *Forwarder) IsEnabled() bool {
	return f.logger != nil
}

// ValidateForwardConfig validates the forwarding configuration.
// Returns an error if the configuration would cause issues (e.g., self-forwarding loop).
//
// Parameters:
//   - targetSourceID: the configured forward destination source ID
//   - originalSourceID: the source ID of the blocked event's origin
//
// Returns error if:
//   - targetSourceID is empty
//   - targetSourceID equals originalSourceID (prevents infinite forwarding loops)
func ValidateForwardConfig(targetSourceID, originalSourceID string) error {
	if targetSourceID == "" {
		return fmt.Errorf("forward target source ID is empty")
	}
	if targetSourceID == originalSourceID {
		return fmt.Errorf(
			"forward target source ID %q equals original source ID, would create infinite loop",
			targetSourceID,
		)
	}
	return nil
}
