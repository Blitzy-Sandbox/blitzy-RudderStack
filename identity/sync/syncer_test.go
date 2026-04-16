// Package sync tests — Unit tests for the identity profile sync service (E-029).
//
// Tests cover CDC-based profile change detection, batching, syncer lifecycle
// (start/stop), change event processing, deduplication, retry logic, checkpoint
// tracking, configuration, metrics, and interface compliance.
//
// All dependencies are mocked — no real infrastructure (DB, Redis, Router).
// Uses testify/require per AAP 0.7.4.
package sync

import (
	"context"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ---------------------------------------------------------------------------
// Mock Dependencies (all shared fields are mutex-protected for -race safety)
// ---------------------------------------------------------------------------

// mockChangeListener simulates the identity graph emitting CDC events.
// Push events into the eventCh channel; Subscribe returns that channel.
type mockChangeListener struct {
	mu            gosync.Mutex
	eventCh       chan ChangeEvent
	checkpoints   []int64
	subscribeErr  error
	checkpointErr error
}

func newMockChangeListener(bufSize int) *mockChangeListener {
	return &mockChangeListener{
		eventCh: make(chan ChangeEvent, bufSize),
	}
}

func (m *mockChangeListener) Subscribe(_ context.Context) (<-chan ChangeEvent, error) {
	if m.subscribeErr != nil {
		return nil, m.subscribeErr
	}
	return m.eventCh, nil
}

func (m *mockChangeListener) Checkpoint(_ context.Context, eventID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.checkpointErr != nil {
		return m.checkpointErr
	}
	m.checkpoints = append(m.checkpoints, eventID)
	return nil
}

// checkpointCount returns the number of checkpoints recorded (thread-safe).
func (m *mockChangeListener) checkpointCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.checkpoints)
}

// hasCheckpoint returns true if the given event ID was checkpointed (thread-safe).
func (m *mockChangeListener) hasCheckpoint(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cp := range m.checkpoints {
		if cp == id {
			return true
		}
	}
	return false
}

// mockProfileAssembler returns assembled ProfileData for given segment IDs.
// Supports per-segment error injection and call tracking.
type mockProfileAssembler struct {
	mu        gosync.RWMutex
	profiles  map[int64]*storage.ProfileData
	errMap    map[int64]error
	callCount atomic.Int64
}

func newMockProfileAssembler() *mockProfileAssembler {
	return &mockProfileAssembler{
		profiles: make(map[int64]*storage.ProfileData),
		errMap:   make(map[int64]error),
	}
}

func (m *mockProfileAssembler) AssembleProfile(_ context.Context, segmentID int64) (*storage.ProfileData, error) {
	m.callCount.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err, ok := m.errMap[segmentID]; ok {
		return nil, err
	}
	p, ok := m.profiles[segmentID]
	if !ok {
		return nil, nil //nolint:nilnil // nil profile signals "segment not found" per syncer API contract
	}
	return p, nil
}

// setProfile registers a profile for a segment ID (thread-safe for post-Start use).
func (m *mockProfileAssembler) setProfile(id int64, p *storage.ProfileData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.profiles[id] = p
}

// mockDestinationSender tracks sent profiles and supports error injection.
type mockDestinationSender struct {
	mu           gosync.Mutex
	sentProfiles []*storage.ProfileData
	sentBatches  [][]*storage.ProfileData
	sendErr      error
	batchErr     error

	// Retry support: failCount tracks how many times to fail before succeeding.
	failCount    int32
	sendAttempts atomic.Int32
}

func newMockDestinationSender() *mockDestinationSender {
	return &mockDestinationSender{}
}

func (m *mockDestinationSender) SendProfile(_ context.Context, profile *storage.ProfileData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	m.sentProfiles = append(m.sentProfiles, profile)
	return nil
}

func (m *mockDestinationSender) SendBatch(_ context.Context, profiles []*storage.ProfileData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	attempt := m.sendAttempts.Add(1)
	if m.failCount > 0 && attempt <= m.failCount {
		return m.batchErr
	}
	if m.batchErr != nil && m.failCount == 0 {
		return m.batchErr
	}
	m.sentBatches = append(m.sentBatches, profiles)
	return nil
}

// batchCount returns the number of successful batches sent (thread-safe).
func (m *mockDestinationSender) batchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sentBatches)
}

// getBatches returns a snapshot of all batches (thread-safe).
func (m *mockDestinationSender) getBatches() [][]*storage.ProfileData {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([][]*storage.ProfileData, len(m.sentBatches))
	for i, b := range m.sentBatches {
		bc := make([]*storage.ProfileData, len(b))
		copy(bc, b)
		result[i] = bc
	}
	return result
}

// totalSent returns the total number of profiles sent across all batches (thread-safe).
func (m *mockDestinationSender) totalSent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, b := range m.sentBatches {
		total += len(b)
	}
	return total
}

// ---------------------------------------------------------------------------
// Helper Functions
// ---------------------------------------------------------------------------

// newTestSyncer creates a Syncer with all mock dependencies and test-friendly
// configuration (small batch size, short flush interval, low retries).
func newTestSyncer(t *testing.T) (*Syncer, *mockChangeListener, *mockProfileAssembler, *mockDestinationSender) { //nolint:unparam // 4th return kept for API consistency with newTestSyncerWithConfig
	t.Helper()

	cl := newMockChangeListener(100)
	pa := newMockProfileAssembler()
	ds := newMockDestinationSender()

	conf := config.New()
	conf.Set("Identity.Sync.BatchSize", 10)
	conf.Set("Identity.Sync.FlushIntervalMS", 50)
	conf.Set("Identity.Sync.MaxRetries", 3)

	s, err := New(cl, pa, ds, conf, logger.NOP, stats.NOP)
	require.NoError(t, err)
	require.NotNil(t, s)

	return s, cl, pa, ds
}

// newTestSyncerWithConfig creates a Syncer allowing custom config overrides.
func newTestSyncerWithConfig(
	t *testing.T,
	batchSize, flushIntervalMS, maxRetries int,
) (*Syncer, *mockChangeListener, *mockProfileAssembler, *mockDestinationSender) {
	t.Helper()

	cl := newMockChangeListener(200)
	pa := newMockProfileAssembler()
	ds := newMockDestinationSender()

	conf := config.New()
	conf.Set("Identity.Sync.BatchSize", batchSize)
	conf.Set("Identity.Sync.FlushIntervalMS", flushIntervalMS)
	conf.Set("Identity.Sync.MaxRetries", maxRetries)

	s, err := New(cl, pa, ds, conf, logger.NOP, stats.NOP)
	require.NoError(t, err)
	require.NotNil(t, s)

	return s, cl, pa, ds
}

// sampleChangeEvent creates a ChangeEvent for testing.
func sampleChangeEvent(changeType ChangeType, segmentID int64) ChangeEvent {
	return ChangeEvent{
		ID:          segmentID * 100, // deterministic ID based on segment
		Type:        changeType,
		SegmentID:   segmentID,
		WorkspaceID: "workspace-1",
		Timestamp:   time.Now(),
	}
}

// sampleMergeEvent creates a merge ChangeEvent with MergedSegmentIDs populated.
func sampleMergeEvent(targetSegmentID int64, mergedIDs []int64) ChangeEvent {
	return ChangeEvent{
		ID:               targetSegmentID * 100,
		Type:             ChangeTypeMerge,
		SegmentID:        targetSegmentID,
		WorkspaceID:      "workspace-1",
		MergedSegmentIDs: mergedIDs,
		Timestamp:        time.Now(),
	}
}

// sampleProfileData creates sample ProfileData for a given segment ID.
func sampleProfileData(segmentID int64) *storage.ProfileData {
	now := time.Now()
	return &storage.ProfileData{
		Segment: storage.GraphSegment{
			ID:          segmentID,
			WorkspaceID: "workspace-1",
			SegmentID:   fmt.Sprintf("seg-%d", segmentID),
			CreatedAt:   now,
		},
		ExternalIDs: []storage.ExternalID{
			{
				ID:              segmentID*10 + 1,
				GraphID:         segmentID,
				WorkspaceID:     "workspace-1",
				ExternalIDType:  "user_id",
				ExternalIDValue: fmt.Sprintf("user-%d", segmentID),
				CreatedSource:   "web",
				CreatedAt:       now,
			},
			{
				ID:              segmentID*10 + 2,
				GraphID:         segmentID,
				WorkspaceID:     "workspace-1",
				ExternalIDType:  "email",
				ExternalIDValue: fmt.Sprintf("user%d@example.com", segmentID),
				CreatedSource:   "web",
				CreatedAt:       now,
			},
		},
		Traits: []storage.Trait{
			{
				ID:        segmentID*10 + 1,
				GraphID:   segmentID,
				Key:       "name",
				Value:     fmt.Sprintf("User %d", segmentID),
				UpdatedAt: now,
			},
			{
				ID:        segmentID*10 + 2,
				GraphID:   segmentID,
				Key:       "plan",
				Value:     "free",
				UpdatedAt: now,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Phase 2: Constructor and Lifecycle Tests
// ---------------------------------------------------------------------------

func TestNewSyncer(t *testing.T) {
	cl := newMockChangeListener(10)
	pa := newMockProfileAssembler()
	ds := newMockDestinationSender()

	t.Run("valid_construction", func(t *testing.T) {
		s, err := New(cl, pa, ds, config.New(), logger.NOP, stats.NOP)
		require.NoError(t, err)
		require.NotNil(t, s)
	})

	t.Run("nil_logger_uses_package_default", func(t *testing.T) {
		s, err := New(cl, pa, ds, config.New(), nil, stats.NOP)
		require.NoError(t, err)
		require.NotNil(t, s)
	})

	t.Run("nil_config_uses_default", func(t *testing.T) {
		s, err := New(cl, pa, ds, nil, logger.NOP, stats.NOP)
		require.NoError(t, err)
		require.NotNil(t, s)
	})

	t.Run("nil_stats_disables_metrics", func(t *testing.T) {
		s, err := New(cl, pa, ds, config.New(), logger.NOP, nil)
		require.NoError(t, err)
		require.NotNil(t, s)
		// stats fields should be nil when statsFactory is nil
		require.Nil(t, s.stats.eventsReceived)
		require.Nil(t, s.stats.eventsProcessed)
		require.Nil(t, s.stats.eventsFailed)
	})
}

func TestNewSyncer_NilChangeListener(t *testing.T) {
	pa := newMockProfileAssembler()
	ds := newMockDestinationSender()

	s, err := New(nil, pa, ds, config.New(), logger.NOP, stats.NOP)
	require.Error(t, err)
	require.Nil(t, s)
	require.Contains(t, err.Error(), "change listener is required")
}

func TestNewSyncer_NilProfileAssembler(t *testing.T) {
	cl := newMockChangeListener(10)
	ds := newMockDestinationSender()

	s, err := New(cl, nil, ds, config.New(), logger.NOP, stats.NOP)
	require.Error(t, err)
	require.Nil(t, s)
	require.Contains(t, err.Error(), "profile assembler is required")
}

func TestNewSyncer_NilDestinationSender(t *testing.T) {
	cl := newMockChangeListener(10)
	pa := newMockProfileAssembler()

	s, err := New(cl, pa, nil, config.New(), logger.NOP, stats.NOP)
	require.Error(t, err)
	require.Nil(t, s)
	require.Contains(t, err.Error(), "destination sender is required")
}

func TestSyncer_StartStop(t *testing.T) {
	s, cl, _, _ := newTestSyncer(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Verify healthy after start.
	require.NoError(t, s.Health(context.Background()))

	// Close the event channel to signal end of events.
	close(cl.eventCh)

	// Stop should complete gracefully within a timeout.
	errCh := make(chan error, 1)
	go func() { errCh <- s.Stop() }()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete within 5 seconds")
	}

	// Verify not healthy after stop.
	require.Error(t, s.Health(context.Background()))
}

func TestSyncer_StartStop_AlreadyStarted(t *testing.T) {
	s, cl, _, _ := newTestSyncer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		close(cl.eventCh)
		cancel()
		_ = s.Stop()
	})

	err := s.Start(ctx)
	require.NoError(t, err)

	// Second Start should return error.
	err = s.Start(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already running")
}

func TestSyncer_ContextCancellation(t *testing.T) {
	s, _, pa, ds := newTestSyncer(t)

	// Add a profile so in-flight events get processed.
	pa.profiles[1] = sampleProfileData(1)

	ctx, cancel := context.WithCancel(context.Background())

	err := s.Start(ctx)
	require.NoError(t, err)

	// Cancel the context to trigger graceful shutdown.
	cancel()

	// Stop must be called to set running=false; cancelling context alone
	// only stops the syncLoop goroutine but doesn't flip the running flag.
	err = s.Stop()
	require.NoError(t, err)

	// Verify not healthy after stop.
	require.Error(t, s.Health(context.Background()))

	// Verify Stop is idempotent after context cancellation.
	err = s.Stop()
	require.NoError(t, err)

	// Verify no crash — the sender may or may not have been called depending on timing.
	_ = ds
}

func TestSyncer_StopIdempotent(t *testing.T) {
	s, cl, _, _ := newTestSyncer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	close(cl.eventCh)

	// First Stop.
	err = s.Stop()
	require.NoError(t, err)

	// Second Stop should also return nil (idempotent).
	err = s.Stop()
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Phase 3: Change Event Processing Tests
// ---------------------------------------------------------------------------

func TestSyncer_ProcessNewSegmentEvent(t *testing.T) {
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 3)

	profile := sampleProfileData(1)
	pa.profiles[1] = profile

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Emit a new segment event.
	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 1)

	// Batch size is 1 so it should flush immediately.
	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Stop the syncer so no more concurrent writes occur.
	close(cl.eventCh)
	_ = s.Stop()

	batches := ds.getBatches()
	require.Len(t, batches, 1)
	require.Len(t, batches[0], 1)
	require.Equal(t, profile.Segment.ID, batches[0][0].Segment.ID)
	require.Equal(t, profile.Segment.WorkspaceID, batches[0][0].Segment.WorkspaceID)
}

func TestSyncer_ProcessMergeEvent(t *testing.T) {
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 3)

	// After merge, segment 1 has identifiers from segments 2 and 3.
	mergedProfile := sampleProfileData(1)
	mergedProfile.ExternalIDs = append(mergedProfile.ExternalIDs,
		storage.ExternalID{
			ID:              21,
			GraphID:         1,
			WorkspaceID:     "workspace-1",
			ExternalIDType:  "anonymous_id",
			ExternalIDValue: "anon-from-segment-2",
			CreatedSource:   "web",
			CreatedAt:       time.Now(),
		},
		storage.ExternalID{
			ID:              31,
			GraphID:         1,
			WorkspaceID:     "workspace-1",
			ExternalIDType:  "anonymous_id",
			ExternalIDValue: "anon-from-segment-3",
			CreatedSource:   "mobile",
			CreatedAt:       time.Now(),
		},
	)
	pa.profiles[1] = mergedProfile

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Emit a merge event.
	cl.eventCh <- sampleMergeEvent(1, []int64{2, 3})

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	batches := ds.getBatches()
	require.Len(t, batches[0], 1)
	sentProfile := batches[0][0]
	// Profile should include identifiers from all merged segments.
	require.Len(t, sentProfile.ExternalIDs, 4) // 2 original + 2 from merged segments
}

func TestSyncer_ProcessTraitUpdateEvent(t *testing.T) {
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 3)

	profile := sampleProfileData(1)
	// Update trait: plan changed from "free" to "enterprise".
	profile.Traits[1] = storage.Trait{
		ID:        12,
		GraphID:   1,
		Key:       "plan",
		Value:     "enterprise",
		UpdatedAt: time.Now(),
	}
	pa.profiles[1] = profile

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- sampleChangeEvent(ChangeTypeTraitUpdate, 1)

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	batches := ds.getBatches()
	sentProfile := batches[0][0]
	// Verify the "plan" trait has the updated value.
	var foundPlan bool
	for _, trait := range sentProfile.Traits {
		if trait.Key == "plan" {
			require.Equal(t, "enterprise", trait.Value)
			foundPlan = true
		}
	}
	require.True(t, foundPlan, "plan trait should exist in sent profile")
}

func TestSyncer_ProcessNewExternalIDEvent(t *testing.T) {
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 3)

	profile := sampleProfileData(1)
	// Add a new email external ID.
	profile.ExternalIDs = append(profile.ExternalIDs, storage.ExternalID{
		ID:              13,
		GraphID:         1,
		WorkspaceID:     "workspace-1",
		ExternalIDType:  "email",
		ExternalIDValue: "newemail@example.com",
		CreatedSource:   "api",
		CreatedAt:       time.Now(),
	})
	pa.profiles[1] = profile

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- sampleChangeEvent(ChangeTypeNewExternalID, 1)

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	batches := ds.getBatches()
	sentProfile := batches[0][0]
	require.Len(t, sentProfile.ExternalIDs, 3) // 2 original + 1 new
}

func TestSyncer_ProcessMultipleEvents(t *testing.T) {
	s, cl, pa, ds := newTestSyncerWithConfig(t, 5, 50, 3)

	// Register profiles for 5 different segments.
	for i := int64(1); i <= 5; i++ {
		pa.profiles[i] = sampleProfileData(i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Emit 5 events for different segments.
	for i := int64(1); i <= 5; i++ {
		cl.eventCh <- ChangeEvent{
			ID:          i,
			Type:        ChangeTypeNewSegment,
			SegmentID:   i,
			WorkspaceID: "workspace-1",
			Timestamp:   time.Now(),
		}
	}

	// Wait for all to be processed (batch size=5 triggers immediate flush).
	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	// All 5 profiles should have been sent in the batch.
	require.Equal(t, 5, ds.totalSent())
}

// ---------------------------------------------------------------------------
// Phase 4: Batching Tests
// ---------------------------------------------------------------------------

func TestSyncer_BatchesChanges(t *testing.T) {
	// Batch size=5, long flush interval so only size triggers flush.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 5, 10000, 3)

	for i := int64(1); i <= 10; i++ {
		pa.profiles[i] = sampleProfileData(i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Emit 10 events rapidly — should trigger 2 batches of 5.
	for i := int64(1); i <= 10; i++ {
		cl.eventCh <- ChangeEvent{
			ID:          i,
			Type:        ChangeTypeNewSegment,
			SegmentID:   i,
			WorkspaceID: "workspace-1",
			Timestamp:   time.Now(),
		}
	}

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	// Should have exactly 2 batches of 5 each.
	batches := ds.getBatches()
	require.Len(t, batches, 2)
	require.Len(t, batches[0], 5)
	require.Len(t, batches[1], 5)
}

func TestSyncer_FlushOnInterval(t *testing.T) {
	// Large batch size, short flush interval to test time-based flush.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 100, 50, 3)

	for i := int64(1); i <= 3; i++ {
		pa.profiles[i] = sampleProfileData(i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Emit 3 events — batch size is 100 so size trigger won't fire.
	for i := int64(1); i <= 3; i++ {
		cl.eventCh <- ChangeEvent{
			ID:          i,
			Type:        ChangeTypeNewSegment,
			SegmentID:   i,
			WorkspaceID: "workspace-1",
			Timestamp:   time.Now(),
		}
	}

	// Wait for flush interval to trigger (50ms + buffer).
	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	// Should have flushed the 3 events in a single batch.
	require.Equal(t, 3, ds.totalSent())
}

func TestSyncer_DeduplicateSegmentUpdates(t *testing.T) {
	// Batch size large, short flush interval.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 100, 50, 3)

	profile := sampleProfileData(1)
	pa.profiles[1] = profile

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Emit 3 events for the SAME segment within one batch window.
	cl.eventCh <- ChangeEvent{
		ID: 1, Type: ChangeTypeTraitUpdate, SegmentID: 1,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}
	cl.eventCh <- ChangeEvent{
		ID: 2, Type: ChangeTypeNewExternalID, SegmentID: 1,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}
	cl.eventCh <- ChangeEvent{
		ID: 3, Type: ChangeTypeTraitUpdate, SegmentID: 1,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}

	// Wait for time-triggered flush.
	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	// Should deduplicate: only 1 profile should be assembled and sent.
	batches := ds.getBatches()
	require.Len(t, batches[0], 1)
	require.Equal(t, int64(1), batches[0][0].Segment.ID)

	// Profile assembler should only have been called once for this segment
	// (deduplication reduces 3 events to 1).
	require.Equal(t, int64(1), pa.callCount.Load())
}

// ---------------------------------------------------------------------------
// Phase 5: Error Handling Tests
// ---------------------------------------------------------------------------

func TestSyncer_ProfileAssemblyError(t *testing.T) {
	s, cl, pa, ds := newTestSyncerWithConfig(t, 2, 50, 3)

	// Segment 1 returns error, segment 2 succeeds.
	pa.errMap[1] = fmt.Errorf("database connection lost")
	pa.profiles[2] = sampleProfileData(2)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Emit events for both segments.
	cl.eventCh <- ChangeEvent{
		ID: 1, Type: ChangeTypeNewSegment, SegmentID: 1,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}
	cl.eventCh <- ChangeEvent{
		ID: 2, Type: ChangeTypeNewSegment, SegmentID: 2,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}

	// Batch of 2 triggers flush; segment 1 fails, segment 2 should succeed.
	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	// Only segment 2's profile should have been sent.
	batches := ds.getBatches()
	require.Len(t, batches[0], 1)
	require.Equal(t, int64(2), batches[0][0].Segment.ID)
}

func TestSyncer_DestinationSendError(t *testing.T) {
	// maxRetries=0 so it fails immediately without retrying.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 0)

	pa.profiles[1] = sampleProfileData(1)
	ds.batchErr = fmt.Errorf("destination unreachable")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 1)

	// Wait for the event to be processed (even though send fails).
	require.Eventually(t, func() bool {
		return ds.sendAttempts.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Syncer should still be healthy — doesn't crash on send errors.
	require.NoError(t, s.Health(context.Background()))

	// Now send a second event to verify syncer continues processing.
	// Use thread-safe setProfile since syncer goroutine reads the map.
	pa.setProfile(2, sampleProfileData(2))
	cl.eventCh <- ChangeEvent{
		ID: 2, Type: ChangeTypeNewSegment, SegmentID: 2,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}

	require.Eventually(t, func() bool {
		return ds.sendAttempts.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()
}

func TestSyncer_RetryOnFailure(t *testing.T) {
	// Batch size=1, maxRetries=3.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 3)

	pa.profiles[1] = sampleProfileData(1)

	// Fail first 2 times, succeed on 3rd.
	ds.failCount = 2
	ds.batchErr = fmt.Errorf("temporary network error")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 1)

	// Wait for successful delivery (3rd attempt succeeds).
	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 30*time.Second, 50*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	// Verify at least 3 send attempts were made (2 failures + 1 success).
	require.True(t, ds.sendAttempts.Load() >= 3)

	// The profile should have been sent successfully.
	batches := ds.getBatches()
	require.Len(t, batches[0], 1)
	require.Equal(t, int64(1), batches[0][0].Segment.ID)
}

func TestSyncer_RetryExhausted(t *testing.T) {
	// Batch size=1, maxRetries=2 — will attempt 3 times (initial + 2 retries).
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 2)

	pa.profiles[1] = sampleProfileData(1)

	// Always fail.
	ds.batchErr = fmt.Errorf("persistent failure")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 1)

	// Wait for all retries to be exhausted (initial + 2 retries = 3 attempts).
	require.Eventually(t, func() bool {
		return ds.sendAttempts.Load() >= 3
	}, 30*time.Second, 50*time.Millisecond)

	// No successful sends should have occurred.
	require.Equal(t, 0, ds.batchCount())

	// Syncer should continue running — doesn't crash on exhausted retries.
	require.NoError(t, s.Health(context.Background()))

	// Verify it still processes new events.
	// Use thread-safe setProfile since syncer goroutine reads the map.
	pa.setProfile(2, sampleProfileData(2))
	cl.eventCh <- ChangeEvent{
		ID: 200, Type: ChangeTypeNewSegment, SegmentID: 2,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}

	require.Eventually(t, func() bool {
		return ds.sendAttempts.Load() >= 4
	}, 30*time.Second, 50*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()
}

// ---------------------------------------------------------------------------
// Phase 6: Checkpoint / Sync State Tests
// ---------------------------------------------------------------------------

func TestSyncer_TracksSyncCheckpoint(t *testing.T) {
	s, cl, pa, ds := newTestSyncerWithConfig(t, 2, 50, 3)
	_ = ds

	pa.profiles[1] = sampleProfileData(1)
	pa.profiles[2] = sampleProfileData(2)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Emit events with known IDs.
	cl.eventCh <- ChangeEvent{
		ID: 10, Type: ChangeTypeNewSegment, SegmentID: 1,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}
	cl.eventCh <- ChangeEvent{
		ID: 20, Type: ChangeTypeNewSegment, SegmentID: 2,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}

	// Wait for processing (batch size=2 triggers immediate flush).
	require.Eventually(t, func() bool {
		return cl.checkpointCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	// The checkpoint should be the ID of the LAST event in the batch.
	require.True(t, cl.hasCheckpoint(20))
}

func TestSyncer_ResumeFromCheckpoint(t *testing.T) {
	// This tests that events arriving after the syncer starts are processed,
	// simulating resume from a checkpoint (the ChangeListener implementation
	// would filter events prior to the checkpoint).
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 3)

	// Only events with ID > 50 should arrive (simulated by mock).
	pa.profiles[51] = sampleProfileData(51)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Event with ID 51 (after the checkpoint).
	cl.eventCh <- ChangeEvent{
		ID: 51, Type: ChangeTypeNewSegment, SegmentID: 51,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Wait for checkpoint to be written.
	require.Eventually(t, func() bool {
		return cl.checkpointCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	batches := ds.getBatches()
	require.Len(t, batches[0], 1)
	require.Equal(t, int64(51), batches[0][0].Segment.ID)

	// Checkpoint should have been updated to event ID 51.
	require.True(t, cl.hasCheckpoint(51))
}

// ---------------------------------------------------------------------------
// Phase 7: Configuration Tests
// ---------------------------------------------------------------------------

func TestSyncer_ConfigurableBatchSize(t *testing.T) {
	// Custom batch size of 3.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 3, 10000, 3)

	for i := int64(1); i <= 6; i++ {
		pa.profiles[i] = sampleProfileData(i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	require.Equal(t, 3, s.batchSize)

	// Emit 6 events — should produce 2 batches of 3.
	for i := int64(1); i <= 6; i++ {
		cl.eventCh <- ChangeEvent{
			ID: i, Type: ChangeTypeNewSegment, SegmentID: i,
			WorkspaceID: "workspace-1", Timestamp: time.Now(),
		}
	}

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	batches := ds.getBatches()
	require.Len(t, batches, 2)
	require.Len(t, batches[0], 3)
	require.Len(t, batches[1], 3)
}

func TestSyncer_ConfigurableFlushInterval(t *testing.T) {
	// Short flush interval of 30ms.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1000, 30, 3)

	pa.profiles[1] = sampleProfileData(1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	require.Equal(t, 30*time.Millisecond, s.flushInterval)

	// Emit 1 event — batch size is 1000 so only flush interval triggers.
	cl.eventCh <- ChangeEvent{
		ID: 1, Type: ChangeTypeNewSegment, SegmentID: 1,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	batches := ds.getBatches()
	require.Len(t, batches[0], 1)
}

func TestSyncer_ConfigurableMaxRetries(t *testing.T) {
	// maxRetries=1 means initial attempt + 1 retry = 2 total attempts.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 1)

	pa.profiles[1] = sampleProfileData(1)
	ds.batchErr = fmt.Errorf("always fail")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	require.Equal(t, 1, s.maxRetries)

	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 1)

	// Wait for retries to be exhausted (initial + 1 = 2 attempts).
	require.Eventually(t, func() bool {
		return ds.sendAttempts.Load() >= 2
	}, 30*time.Second, 50*time.Millisecond)

	// Give a small buffer to ensure no extra attempts are made.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, int32(2), ds.sendAttempts.Load())

	close(cl.eventCh)
	_ = s.Stop()
}

func TestSyncer_DefaultConfigValues(t *testing.T) {
	cl := newMockChangeListener(10)
	pa := newMockProfileAssembler()
	ds := newMockDestinationSender()

	// Use empty config — should fall back to defaults.
	conf := config.New()
	s, err := New(cl, pa, ds, conf, logger.NOP, stats.NOP)
	require.NoError(t, err)

	// Verify default values from syncer.go constructor.
	require.Equal(t, 100, s.batchSize)
	require.Equal(t, 1000*time.Millisecond, s.flushInterval)
	require.Equal(t, 3, s.maxRetries)
}

// ---------------------------------------------------------------------------
// Phase 8: Metrics Tests
// ---------------------------------------------------------------------------

func TestSyncer_RecordsMetrics(t *testing.T) {
	cl := newMockChangeListener(10)
	pa := newMockProfileAssembler()
	ds := newMockDestinationSender()

	pa.profiles[1] = sampleProfileData(1)

	conf := config.New()
	conf.Set("Identity.Sync.BatchSize", 1)
	conf.Set("Identity.Sync.FlushIntervalMS", 50)
	conf.Set("Identity.Sync.MaxRetries", 3)

	// Use stats.NOP — it creates nop measurements (not nil).
	s, err := New(cl, pa, ds, conf, logger.NOP, stats.NOP)
	require.NoError(t, err)

	// Verify stats are initialized when a non-nil statsFactory is provided.
	require.NotNil(t, s.stats.eventsReceived)
	require.NotNil(t, s.stats.eventsProcessed)
	require.NotNil(t, s.stats.eventsFailed)
	require.NotNil(t, s.stats.batchSize)
	require.NotNil(t, s.stats.processTime)
	require.NotNil(t, s.stats.sendTime)
	require.NotNil(t, s.stats.retryCount)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err = s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 1)

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// The stats.NOP measurements are no-ops so we can't inspect values,
	// but we've verified they're non-nil and the code paths that call them
	// execute without panics (the event was processed successfully).
	close(cl.eventCh)
	_ = s.Stop()
}

func TestSyncer_NilStatsNoMetrics(t *testing.T) {
	cl := newMockChangeListener(10)
	pa := newMockProfileAssembler()
	ds := newMockDestinationSender()

	pa.profiles[1] = sampleProfileData(1)

	conf := config.New()
	conf.Set("Identity.Sync.BatchSize", 1)
	conf.Set("Identity.Sync.FlushIntervalMS", 50)
	conf.Set("Identity.Sync.MaxRetries", 3)

	// nil statsFactory — metrics should be nil.
	s, err := New(cl, pa, ds, conf, logger.NOP, nil)
	require.NoError(t, err)

	require.Nil(t, s.stats.eventsReceived)
	require.Nil(t, s.stats.eventsProcessed)
	require.Nil(t, s.stats.eventsFailed)
	require.Nil(t, s.stats.batchSize)
	require.Nil(t, s.stats.processTime)
	require.Nil(t, s.stats.sendTime)
	require.Nil(t, s.stats.retryCount)

	// Should still process events without panicking despite nil metrics.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err = s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 1)

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()
}

// ---------------------------------------------------------------------------
// Phase 9: ChangeEvent Type Tests
// ---------------------------------------------------------------------------

func TestChangeEvent_Types(t *testing.T) {
	testCases := []struct {
		name     string
		ct       ChangeType
		expected string
	}{
		{"new_segment", ChangeTypeNewSegment, "new_segment"},
		{"merge", ChangeTypeMerge, "merge"},
		{"trait_update", ChangeTypeTraitUpdate, "trait_update"},
		{"new_external_id", ChangeTypeNewExternalID, "new_external_id"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, tc.ct.String())
		})
	}

	// Unknown type should produce a descriptive string.
	unknown := ChangeType(99)
	require.Contains(t, unknown.String(), "unknown")
}

func TestChangeEvent_Fields(t *testing.T) {
	now := time.Now()
	event := ChangeEvent{
		ID:               42,
		Type:             ChangeTypeMerge,
		SegmentID:        10,
		WorkspaceID:      "ws-abc",
		MergedSegmentIDs: []int64{20, 30},
		Timestamp:        now,
	}

	require.Equal(t, int64(42), event.ID)
	require.Equal(t, ChangeTypeMerge, event.Type)
	require.Equal(t, int64(10), event.SegmentID)
	require.Equal(t, "ws-abc", event.WorkspaceID)
	require.Equal(t, []int64{20, 30}, event.MergedSegmentIDs)
	require.False(t, event.Timestamp.IsZero())
}

func TestChangeEvent_String(t *testing.T) {
	now := time.Now()
	event := ChangeEvent{
		ID:          42,
		Type:        ChangeTypeMerge,
		SegmentID:   10,
		WorkspaceID: "ws-abc",
		Timestamp:   now,
	}

	s := event.String()
	require.Contains(t, s, "ChangeEvent")
	require.Contains(t, s, "ID:42")
	require.Contains(t, s, "merge")
	require.Contains(t, s, "SegmentID:10")
	require.Contains(t, s, "ws-abc")
}

// ---------------------------------------------------------------------------
// Phase 10: Interface Compliance Tests
// ---------------------------------------------------------------------------

func TestSyncer_InterfaceCompliance(t *testing.T) {
	s, cl, _, _ := newTestSyncer(t)
	t.Cleanup(func() { close(cl.eventCh) })

	// Verify Start method exists with correct signature.
	ctx := context.Background()
	startErr := s.Start(ctx)
	require.NoError(t, startErr)

	// Verify Health method exists with correct signature.
	healthErr := s.Health(ctx)
	require.NoError(t, healthErr)

	// Verify Stop method exists with correct signature.
	stopErr := s.Stop()
	require.NoError(t, stopErr)
}

func TestChangeListener_Interface(t *testing.T) {
	// Verify our mock satisfies the ChangeListener interface.
	var cl ChangeListener = newMockChangeListener(1)
	require.NotNil(t, cl)

	ctx := context.Background()
	ch, err := cl.Subscribe(ctx)
	require.NoError(t, err)
	require.NotNil(t, ch)

	err = cl.Checkpoint(ctx, 1)
	require.NoError(t, err)
}

func TestProfileAssembler_Interface(t *testing.T) {
	// Verify our mock satisfies the ProfileAssembler interface.
	var pa ProfileAssembler = newMockProfileAssembler()
	require.NotNil(t, pa)

	ctx := context.Background()
	profile, err := pa.AssembleProfile(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, profile) // no profile registered
}

func TestDestinationSender_Interface(t *testing.T) {
	// Verify our mock satisfies the DestinationSender interface.
	var ds DestinationSender = newMockDestinationSender()
	require.NotNil(t, ds)

	ctx := context.Background()
	profile := sampleProfileData(1)

	err := ds.SendProfile(ctx, profile)
	require.NoError(t, err)

	err = ds.SendBatch(ctx, []*storage.ProfileData{profile})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Additional Edge Case Tests
// ---------------------------------------------------------------------------

func TestSyncer_SubscribeError(t *testing.T) {
	cl := newMockChangeListener(1)
	cl.subscribeErr = fmt.Errorf("subscription failed")
	pa := newMockProfileAssembler()
	ds := newMockDestinationSender()

	conf := config.New()
	s, err := New(cl, pa, ds, conf, logger.NOP, stats.NOP)
	require.NoError(t, err)

	// Start should fail because Subscribe returns an error.
	err = s.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "subscribe to changes")
}

func TestSyncer_ChannelClosedMidBatch(t *testing.T) {
	// Use batch size 1 and short flush interval to ensure the event is
	// processed before the channel close is detected by the syncLoop.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 50, 3)

	pa.profiles[1] = sampleProfileData(1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	// Send one event (batch size=1 triggers immediate flush).
	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 1)

	// Wait for the event to be processed and sent.
	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Now close the channel — the syncLoop should detect it and exit.
	close(cl.eventCh)

	// Call Stop to clean up and set running=false.
	err = s.Stop()
	require.NoError(t, err)

	// Now the syncer should report as not running.
	require.Error(t, s.Health(context.Background()))

	// The single event should have been flushed and sent.
	require.Equal(t, 1, ds.totalSent())
}

func TestSyncer_EmptyProfileSkipped(t *testing.T) {
	// When ProfileAssembler returns nil (segment was merged away), skip it.
	s, cl, pa, ds := newTestSyncerWithConfig(t, 2, 50, 3)

	// Segment 1 no longer exists (returns nil profile).
	// Segment 2 has a valid profile.
	pa.profiles[2] = sampleProfileData(2)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- ChangeEvent{
		ID: 1, Type: ChangeTypeNewSegment, SegmentID: 1,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}
	cl.eventCh <- ChangeEvent{
		ID: 2, Type: ChangeTypeNewSegment, SegmentID: 2,
		WorkspaceID: "workspace-1", Timestamp: time.Now(),
	}

	require.Eventually(t, func() bool {
		return ds.batchCount() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	close(cl.eventCh)
	_ = s.Stop()

	// Only segment 2 should have been sent.
	batches := ds.getBatches()
	require.Len(t, batches[0], 1)
	require.Equal(t, int64(2), batches[0][0].Segment.ID)
}

func TestSyncer_CheckpointError(t *testing.T) {
	s, cl, pa, ds := newTestSyncerWithConfig(t, 1, 5000, 3)
	_ = ds

	pa.profiles[7] = sampleProfileData(7)
	cl.checkpointErr = fmt.Errorf("checkpoint write failed")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := s.Start(ctx)
	require.NoError(t, err)

	cl.eventCh <- sampleChangeEvent(ChangeTypeNewSegment, 7)

	// Even though checkpointing fails, the syncer should continue running.
	require.Eventually(t, func() bool {
		return ds.sendAttempts.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, s.Health(context.Background()))

	close(cl.eventCh)
	_ = s.Stop()
}

func TestSyncer_HealthNotRunning(t *testing.T) {
	s, _, _, _ := newTestSyncer(t)
	// Before starting, Health should return error.
	err := s.Health(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not running")
}
