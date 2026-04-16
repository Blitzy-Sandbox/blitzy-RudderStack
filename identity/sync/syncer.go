// Package sync implements profile synchronization to downstream destinations
// using change-data-capture (CDC) patterns on the identity graph.
//
// The Syncer monitors the identity graph for mutations (new segments, merges,
// trait updates, new external IDs) and propagates complete profile snapshots
// to configured downstream destinations. This is the E-029 epic implementation
// from Sprint 6–8 of the Identity Resolution feature.
//
// Architecture:
//
//	ChangeListener (CDC source) → Syncer (batch + deduplicate) → DestinationSender (delivery)
//
// Key design decisions:
//   - Dual-trigger flush (size + time) for balanced throughput and latency
//   - Per-segment deduplication within batch windows to avoid redundant syncs
//   - Checkpoint-based progress tracking for resume-after-restart
//   - Exponential backoff retry for transient delivery failures
//   - Per-event error isolation to prevent single-event failures from blocking a batch
package sync

import (
	"context"
	"fmt"
	gosync "sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/identity/storage"
)

// pkgLogger is the package-level logger following the pattern from
// warehouse/identity/identity.go:30-34.
var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("identity").Child("sync")
}

// ---------------------------------------------------------------------------
// Change Event Types
// ---------------------------------------------------------------------------

// ChangeType represents the type of mutation that occurred in the identity graph.
type ChangeType int

const (
	// ChangeTypeNewSegment indicates a new identity segment was created.
	// Triggers a full profile sync for the new segment.
	ChangeTypeNewSegment ChangeType = iota

	// ChangeTypeMerge indicates two or more segments were merged.
	// This is the most important sync event — merges change identity associations.
	// The surviving segment's profile must be synced to all destinations.
	ChangeTypeMerge

	// ChangeTypeTraitUpdate indicates trait values were updated on a segment.
	// Triggers a profile sync with the updated trait data.
	ChangeTypeTraitUpdate

	// ChangeTypeNewExternalID indicates a new external identifier was added to a segment.
	// Triggers a profile sync with the updated identifier set.
	ChangeTypeNewExternalID
)

// String returns the human-readable name for a ChangeType constant.
func (ct ChangeType) String() string {
	switch ct {
	case ChangeTypeNewSegment:
		return "new_segment"
	case ChangeTypeMerge:
		return "merge"
	case ChangeTypeTraitUpdate:
		return "trait_update"
	case ChangeTypeNewExternalID:
		return "new_external_id"
	default:
		return fmt.Sprintf("unknown(%d)", int(ct))
	}
}

// ChangeEvent represents a change-data-capture event from the identity graph.
// These events are produced by identity/graph/graph.go when segments are
// created, merged, or updated, and consumed by the Syncer for downstream propagation.
type ChangeEvent struct {
	// ID is a monotonically increasing event identifier for checkpointing.
	ID int64

	// Type is the kind of graph mutation that occurred.
	Type ChangeType

	// SegmentID is the ID of the affected identity graph segment.
	SegmentID int64

	// WorkspaceID is the workspace this segment belongs to.
	WorkspaceID string

	// MergedSegmentIDs contains the IDs of segments that were merged into SegmentID.
	// Only populated for ChangeTypeMerge events.
	MergedSegmentIDs []int64

	// Timestamp when the change occurred.
	Timestamp time.Time
}

// String returns a debug-friendly representation of the ChangeEvent.
func (e ChangeEvent) String() string {
	return fmt.Sprintf("ChangeEvent{ID:%d, Type:%s, SegmentID:%d, WorkspaceID:%s, Timestamp:%s}",
		e.ID, e.Type, e.SegmentID, e.WorkspaceID, e.Timestamp.Format(time.RFC3339))
}

// ---------------------------------------------------------------------------
// Core Interfaces
// ---------------------------------------------------------------------------

// ChangeListener provides a stream of change-data-capture events from the
// identity graph. The Syncer subscribes to this stream to detect graph mutations
// and trigger profile sync to downstream destinations.
//
// Implementations may be backed by:
//   - PostgreSQL LISTEN/NOTIFY for real-time notification
//   - A polling mechanism on an event log table
//   - An in-memory channel for testing
type ChangeListener interface {
	// Subscribe returns a channel that emits ChangeEvents.
	// The channel is closed when the context is cancelled.
	// Events are delivered in order of their ID (monotonically increasing).
	Subscribe(ctx context.Context) (<-chan ChangeEvent, error)

	// Checkpoint records the last successfully processed event ID.
	// This enables resume-from-checkpoint after restart.
	Checkpoint(ctx context.Context, eventID int64) error
}

// ProfileAssembler assembles a complete profile from the identity graph.
// It combines segment metadata, external identifiers, and traits into
// a single ProfileData struct suitable for downstream delivery.
//
// The assembler is typically backed by identity/graph.Service and
// identity/storage.Repository.
type ProfileAssembler interface {
	// AssembleProfile builds a complete profile for the given segment ID.
	// Returns the assembled profile or error.
	// Returns (nil, nil) if the segment no longer exists (was merged away).
	AssembleProfile(ctx context.Context, segmentID int64) (*storage.ProfileData, error)
}

// DestinationSender delivers profile updates to downstream destinations.
// It leverages the existing Router infrastructure for destination delivery,
// handling authentication, batching, and retry at the transport level.
type DestinationSender interface {
	// SendProfile sends a single profile update to all configured downstream destinations.
	SendProfile(ctx context.Context, profile *storage.ProfileData) error

	// SendBatch sends a batch of profile updates to downstream destinations.
	// Batching improves efficiency for high-throughput scenarios.
	SendBatch(ctx context.Context, profiles []*storage.ProfileData) error
}

// ---------------------------------------------------------------------------
// Syncer Stats
// ---------------------------------------------------------------------------

// syncerStats holds the tagged measurements for Syncer performance monitoring.
// Follows the stats pattern from processor/trackingplan.go:16-22.
type syncerStats struct {
	eventsReceived  stats.Measurement
	eventsProcessed stats.Measurement
	eventsFailed    stats.Measurement
	batchSize       stats.Measurement
	processTime     stats.Measurement
	sendTime        stats.Measurement
	retryCount      stats.Measurement
}

// ---------------------------------------------------------------------------
// Syncer
// ---------------------------------------------------------------------------

// Syncer monitors the identity graph for changes and propagates profile updates
// to downstream destinations using change-data-capture (CDC) patterns.
//
// Architecture:
//
//	ChangeListener (CDC source) → Syncer (batch + deduplicate) → DestinationSender (delivery)
//
// The Syncer:
//  1. Subscribes to identity graph change events via ChangeListener
//  2. Batches change events within configurable windows (size + time)
//  3. Deduplicates events for the same segment within a batch window
//  4. Assembles complete profiles via ProfileAssembler
//  5. Delivers profile updates via DestinationSender
//  6. Checkpoints progress for resume-after-restart
//
// Thread-safe for concurrent use. Start/Stop manage the background goroutine.
type Syncer struct {
	mu             gosync.Mutex
	changeListener ChangeListener
	assembler      ProfileAssembler
	sender         DestinationSender
	conf           *config.Config
	logger         logger.Logger
	stats          syncerStats

	// Configurable parameters (loaded from rudder-go-kit/config)
	batchSize     int
	flushInterval time.Duration
	maxRetries    int

	// Runtime state
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// New creates a new profile Syncer.
//
// Parameters:
//   - changeListener: CDC event source from the identity graph (required)
//   - assembler: Profile assembly from graph data (required)
//   - sender: Destination delivery for profile updates (required)
//   - conf: Reloadable configuration from rudder-go-kit/config (nil uses config.Default)
//   - log: Structured logger (nil uses package default)
//   - statsFactory: Metrics factory for tagged measurements (nil disables metrics)
//
// Configuration keys (via rudder-go-kit/config):
//   - Identity.Sync.BatchSize: Max events per batch (default: 100)
//   - Identity.Sync.FlushIntervalMS: Flush interval in milliseconds (default: 1000)
//   - Identity.Sync.MaxRetries: Max retries per event (default: 3)
func New(
	changeListener ChangeListener,
	assembler ProfileAssembler,
	sender DestinationSender,
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
) (*Syncer, error) {
	if changeListener == nil {
		return nil, fmt.Errorf("identity sync: change listener is required")
	}
	if assembler == nil {
		return nil, fmt.Errorf("identity sync: profile assembler is required")
	}
	if sender == nil {
		return nil, fmt.Errorf("identity sync: destination sender is required")
	}
	if log == nil {
		log = pkgLogger
	}
	if conf == nil {
		conf = config.Default
	}

	s := &Syncer{
		changeListener: changeListener,
		assembler:      assembler,
		sender:         sender,
		conf:           conf,
		logger:         log.Child("sync"),
		batchSize:      conf.GetInt("Identity.Sync.BatchSize", 100),
		flushInterval:  time.Duration(conf.GetInt("Identity.Sync.FlushIntervalMS", 1000)) * time.Millisecond,
		maxRetries:     conf.GetInt("Identity.Sync.MaxRetries", 3),
		done:           make(chan struct{}),
	}

	// Initialize stats following processor/trackingplan.go:155-159 pattern.
	if statsFactory != nil {
		tags := stats.Tags{"module": "identity", "component": "sync"}
		s.stats.eventsReceived = statsFactory.NewTaggedStat("identity_sync_events_received", stats.CountType, tags)
		s.stats.eventsProcessed = statsFactory.NewTaggedStat("identity_sync_events_processed", stats.CountType, tags)
		s.stats.eventsFailed = statsFactory.NewTaggedStat("identity_sync_events_failed", stats.CountType, tags)
		s.stats.batchSize = statsFactory.NewTaggedStat("identity_sync_batch_size", stats.GaugeType, tags)
		s.stats.processTime = statsFactory.NewTaggedStat("identity_sync_process_time", stats.TimerType, tags)
		s.stats.sendTime = statsFactory.NewTaggedStat("identity_sync_send_time", stats.TimerType, tags)
		s.stats.retryCount = statsFactory.NewTaggedStat("identity_sync_retry_count", stats.CountType, tags)
	}

	return s, nil
}

// ---------------------------------------------------------------------------
// Lifecycle — Start / Stop / Health
// ---------------------------------------------------------------------------

// Start begins the background sync loop that:
//  1. Subscribes to identity graph change events
//  2. Batches events within configurable windows
//  3. Processes batches by assembling profiles and sending to destinations
//
// Start is non-blocking — it launches a background goroutine.
// Use Stop() or context cancellation to shut down.
//
// Returns error if the syncer is already running or if subscription fails.
func (s *Syncer) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("identity sync: already running")
	}

	// Create cancellable context for the sync loop.
	syncCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})

	// Subscribe to change events.
	eventCh, err := s.changeListener.Subscribe(syncCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("identity sync: subscribe to changes: %w", err)
	}

	s.running = true

	// Launch background sync loop. syncCtx is derived from the caller's ctx via
	// WithCancel above; cancel is stored in s.cancel and called in Stop().
	go s.syncLoop(syncCtx, eventCh) //nolint:gosec // G118: syncCtx is request-scoped (derived from ctx); cancel stored in s.cancel

	s.logger.Infon("Identity sync started",
		logger.NewIntField("batchSize", int64(s.batchSize)),
		logger.NewStringField("flushInterval", s.flushInterval.String()),
		logger.NewIntField("maxRetries", int64(s.maxRetries)),
	)

	return nil
}

// Stop gracefully shuts down the sync loop.
// It cancels the background context and waits for the sync loop to complete.
// Blocks until the loop finishes or a 30-second timeout occurs.
// Idempotent — returns nil if the syncer is not running.
func (s *Syncer) Stop() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	cancel := s.cancel
	done := s.done
	s.mu.Unlock()

	// Cancel the sync loop context.
	cancel()

	// Wait for the sync loop to finish with a timeout.
	select {
	case <-done:
		s.logger.Infon("Identity sync stopped gracefully")
	case <-time.After(30 * time.Second):
		s.logger.Warnn("Identity sync stop timed out after 30s")
	}

	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	return nil
}

// Health returns nil if the syncer is healthy, error otherwise.
// The syncer is considered healthy when it is running.
func (s *Syncer) Health(ctx context.Context) error {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()

	if !running {
		return fmt.Errorf("identity sync: not running")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sync Loop — Core Event Processing
// ---------------------------------------------------------------------------

// syncLoop is the main background processing loop that batches and processes
// identity graph change events. It runs until the context is cancelled.
//
// The loop uses a dual-trigger flush mechanism:
//   - Size trigger: Flush when batch reaches batchSize
//   - Time trigger: Flush when flushInterval elapses (even with partial batch)
//
// This pattern ensures both throughput (large batches) and latency
// (time-bounded delivery) are optimized.
func (s *Syncer) syncLoop(ctx context.Context, eventCh <-chan ChangeEvent) {
	defer close(s.done)

	batch := make([]ChangeEvent, 0, s.batchSize)
	flushTimer := time.NewTimer(s.flushInterval)
	defer flushTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			// Context cancelled — flush remaining batch and exit.
			if len(batch) > 0 {
				s.processBatch(context.Background(), batch)
			}
			return

		case event, ok := <-eventCh:
			if !ok {
				// Channel closed — flush remaining and exit.
				if len(batch) > 0 {
					s.processBatch(ctx, batch)
				}
				return
			}

			// Record metric.
			if s.stats.eventsReceived != nil {
				s.stats.eventsReceived.Increment()
			}

			batch = append(batch, event)

			// Size-triggered flush.
			if len(batch) >= s.batchSize {
				s.processBatch(ctx, batch)
				batch = make([]ChangeEvent, 0, s.batchSize)
				flushTimer.Reset(s.flushInterval)
			}

		case <-flushTimer.C:
			// Time-triggered flush.
			if len(batch) > 0 {
				s.processBatch(ctx, batch)
				batch = make([]ChangeEvent, 0, s.batchSize)
			}
			flushTimer.Reset(s.flushInterval)
		}
	}
}

// ---------------------------------------------------------------------------
// Batch Processing
// ---------------------------------------------------------------------------

// processBatch processes a batch of change events by:
//  1. Deduplicating events for the same segment (latest event wins)
//  2. Assembling profiles for each unique segment
//  3. Sending profiles to downstream destinations
//  4. Checkpointing the last processed event ID
func (s *Syncer) processBatch(ctx context.Context, batch []ChangeEvent) {
	startTime := time.Now()
	defer func() {
		if s.stats.processTime != nil {
			s.stats.processTime.Since(startTime)
		}
	}()

	// Step 1: Deduplicate — keep only the latest event per segment.
	deduped := s.deduplicateEvents(batch)

	// Record batch size metric.
	if s.stats.batchSize != nil {
		s.stats.batchSize.Gauge(len(deduped))
	}

	// Step 2: Assemble profiles for each unique segment.
	profiles := make([]*storage.ProfileData, 0, len(deduped))
	for _, event := range deduped {
		profile, err := s.assembler.AssembleProfile(ctx, event.SegmentID)
		if err != nil {
			s.logger.Errorn("Error assembling profile for sync",
				logger.NewIntField("segmentID", event.SegmentID),
				logger.NewStringField("changeType", event.Type.String()),
				obskit.Error(err),
			)
			if s.stats.eventsFailed != nil {
				s.stats.eventsFailed.Increment()
			}
			continue // Skip this event, continue processing others.
		}
		if profile == nil {
			// Segment no longer exists (was merged away).
			s.logger.Debugn("Segment no longer exists, skipping sync",
				logger.NewIntField("segmentID", event.SegmentID),
			)
			continue
		}
		profiles = append(profiles, profile)
	}

	if len(profiles) == 0 {
		return
	}

	// Step 3: Send profiles to downstream destinations with retry.
	if sendErr := s.sendWithRetry(ctx, profiles); sendErr != nil {
		// Delivery failed after all retries — do NOT advance the checkpoint.
		// The failed batch will be re-fetched and retried on the next sync cycle
		// or after service restart, preventing silent data loss.
		s.logger.Errorn("Skipping checkpoint due to send failure — batch will be retried",
			logger.NewIntField("profileCount", int64(len(profiles))),
			obskit.Error(sendErr),
		)
		return
	}

	// Step 4: Checkpoint the last processed event ID only on successful delivery.
	lastEventID := batch[len(batch)-1].ID
	if err := s.changeListener.Checkpoint(ctx, lastEventID); err != nil {
		s.logger.Errorn("Error checkpointing sync progress",
			logger.NewIntField("lastEventID", lastEventID),
			obskit.Error(err),
		)
	}

	// Record success metrics.
	if s.stats.eventsProcessed != nil {
		for range profiles {
			s.stats.eventsProcessed.Increment()
		}
	}
}

// ---------------------------------------------------------------------------
// Deduplication
// ---------------------------------------------------------------------------

// deduplicateEvents removes duplicate events for the same segment within a batch.
// When multiple events target the same segment, only the LATEST event is kept
// (last occurrence in the batch wins). This prevents redundant profile assembly
// and delivery.
//
// The function preserves the first-seen order of segments for deterministic
// processing and operates in O(n) time using a map.
func (s *Syncer) deduplicateEvents(batch []ChangeEvent) []ChangeEvent {
	// Map of segmentID to latest event.
	latest := make(map[int64]ChangeEvent, len(batch))
	order := make([]int64, 0, len(batch)) // Preserve first-seen order.

	for _, event := range batch {
		if _, exists := latest[event.SegmentID]; !exists {
			order = append(order, event.SegmentID)
		}
		latest[event.SegmentID] = event // Last event wins.
	}

	// Build deduplicated slice preserving order.
	result := make([]ChangeEvent, 0, len(latest))
	for _, segID := range order {
		result = append(result, latest[segID])
	}

	if len(batch) != len(result) {
		s.logger.Debugn("Deduplicated batch events",
			logger.NewIntField("originalCount", int64(len(batch))),
			logger.NewIntField("deduplicatedCount", int64(len(result))),
		)
	}

	return result
}

// ---------------------------------------------------------------------------
// Send with Retry
// ---------------------------------------------------------------------------

// sendWithRetry attempts to send profiles to downstream destinations with
// configurable retry logic using exponential backoff (1s, 2s, 4s, ...).
// Respects context cancellation during backoff waits.
//
// Returns nil on success. Returns an error after all retries are exhausted,
// enabling the caller to make checkpointing decisions based on delivery outcome.
// This prevents data loss by ensuring failed batches are not checkpointed past.
func (s *Syncer) sendWithRetry(ctx context.Context, profiles []*storage.ProfileData) error {
	sendStart := time.Now()
	defer func() {
		if s.stats.sendTime != nil {
			s.stats.sendTime.Since(sendStart)
		}
	}()

	var err error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s, ...
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}

			if s.stats.retryCount != nil {
				s.stats.retryCount.Increment()
			}

			s.logger.Warnn("Retrying profile sync",
				logger.NewIntField("attempt", int64(attempt+1)),
				logger.NewIntField("maxRetries", int64(s.maxRetries)),
				logger.NewIntField("profileCount", int64(len(profiles))),
				obskit.Error(err),
			)
		}

		err = s.sender.SendBatch(ctx, profiles)
		if err == nil {
			return nil // Success.
		}
	}

	// All retries exhausted — return error so caller does NOT advance checkpoint.
	s.logger.Errorn("Profile sync failed after all retries",
		logger.NewIntField("maxRetries", int64(s.maxRetries)),
		logger.NewIntField("profileCount", int64(len(profiles))),
		obskit.Error(err),
	)
	if s.stats.eventsFailed != nil {
		for range profiles {
			s.stats.eventsFailed.Increment()
		}
	}
	return fmt.Errorf("profile sync failed after %d retries: %w", s.maxRetries, err)
}
