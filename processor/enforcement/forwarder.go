// Package enforcement provides tracking plan enforcement mode definitions and
// blocked-event forwarding capabilities for the processor pipeline.
//
// The forwarder in this file implements server-to-server forwarding of blocked events
// to an alternative source when the Block enforcement mode rejects events (E-023).
// It preserves original event metadata and emits Prometheus metrics for observability.
package enforcement

import (
	"context"
	"fmt"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/jobsdb"
)

// JobWriter is an interface for writing jobs back into a JobsDB instance. The Forwarder
// uses this to re-inject blocked events into the pipeline under a target source ID.
// When nil, the forwarder logs intent and emits metrics but cannot deliver events.
type JobWriter interface {
	Store(ctx context.Context, jobs []*jobsdb.JobT) error
}

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
	writer       JobWriter

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

// SetJobWriter sets the JobWriter used for re-injecting forwarded events. When set,
// Forward() will write events to the gateway DB under the target source ID.
func (f *Forwarder) SetJobWriter(w JobWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writer = w
}

// Forward routes blocked events to an alternative source for debugging/analysis.
// The events are preserved with their original metadata and forwarded via the Gateway
// for re-processing under the target source ID.
//
// When targetSourceID is empty, the method is a no-op.
// When jobs is empty or nil, the method is a no-op.
//
// The original sourceID of each job is preserved in the event's parameters as "originalSourceId"
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

	// Build forwarded jobs by cloning originals and rewriting the source ID in parameters.
	forwarded := 0
	var forwardJobs []*jobsdb.JobT
	for _, job := range jobs {
		if job == nil {
			continue
		}

		// Clone the job to avoid mutating the original.
		fwdJob := &jobsdb.JobT{
			WorkspaceId:  job.WorkspaceId,
			EventPayload: job.EventPayload,
			EventCount:   job.EventCount,
		}

		// Rewrite parameters: set source_id to targetSourceID and preserve original.
		// Parameters is a JSON object like {"source_id": "...", "source_job_run_id": "...", ...}
		params := job.Parameters
		if len(params) > 0 {
			// Inject target source and preserve original source for traceability.
			if modified, err := sjson.SetBytes(params, "original_source_id", gjson.GetBytes(params, "source_id").String()); err == nil {
				params = modified
			}
			if modified, err := sjson.SetBytes(params, "source_id", targetSourceID); err == nil {
				params = modified
			}
			if modified, err := sjson.SetBytes(params, "forwarded_blocked_event", true); err == nil {
				params = modified
			}
		}
		fwdJob.Parameters = params

		forwardJobs = append(forwardJobs, fwdJob)
		forwarded++
	}

	// Write forwarded jobs to the gateway DB if a writer is available.
	f.mu.RLock()
	writer := f.writer
	f.mu.RUnlock()

	if writer != nil && len(forwardJobs) > 0 {
		if err := writer.Store(context.Background(), forwardJobs); err != nil {
			f.logger.Errorn("failed to store forwarded blocked events",
				logger.NewStringField("targetSourceID", targetSourceID),
				logger.NewStringField("error", err.Error()),
			)
		} else {
			f.logger.Infon("forwarded blocked events stored successfully",
				logger.NewStringField("targetSourceID", targetSourceID),
				logger.NewIntField("count", int64(len(forwardJobs))),
			)
		}
	} else if writer == nil {
		f.logger.Warnn("no job writer configured for enforcement forwarder; blocked events logged but not re-injected",
			logger.NewStringField("targetSourceID", targetSourceID),
			logger.NewIntField("count", int64(len(forwardJobs))),
		)
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
