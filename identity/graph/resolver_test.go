// Package graph — resolver_test.go contains comprehensive unit tests for the
// identity resolution engine (E-026). Tests cover all three resolution strategies
// (new match, single match, multi match), resolution decision logic, segment merge
// ordering, edge cases, settings integration, result types, and atomicity.
//
// Uses testify/require for fail-fast assertions and an in-memory mockRepository
// implementing storage.Repository for full test isolation.
package graph

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-server/identity/settings"
	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ---------------------------------------------------------------------------
// Mock Repository — In-memory storage.Repository for test isolation
// ---------------------------------------------------------------------------

// mockRepository is an in-memory implementation of storage.Repository used by
// resolver tests. All methods are safe for concurrent access via sync.Mutex,
// enabling -race detection. Error injection hooks allow simulating storage failures.
type mockRepository struct {
	mu             sync.Mutex
	segments       map[int64]*storage.GraphSegment  // primary key ID → segment
	externalIDs    map[int64][]storage.ExternalID    // graphID → external IDs
	traits         map[int64][]storage.Trait          // graphID → traits
	nextSegmentID  int64
	nextExternalID int64
	nextTraitID    int64

	// Error injection hooks — set non-nil to simulate storage failures.
	createSegmentErr error
	mergeSegmentsErr error
	addExternalIDErr error
	lookupErr        error
}

// newMockRepository creates an empty in-memory repository for test setup.
func newMockRepository() *mockRepository {
	return &mockRepository{
		segments:       make(map[int64]*storage.GraphSegment),
		externalIDs:    make(map[int64][]storage.ExternalID),
		traits:         make(map[int64][]storage.Trait),
		nextSegmentID:  1,
		nextExternalID: 1,
		nextTraitID:    1,
	}
}

// CreateSegment creates a new identity graph segment with an auto-generated UUID.
func (m *mockRepository) CreateSegment(ctx context.Context, workspaceID string) (int64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createSegmentErr != nil {
		return 0, m.createSegmentErr
	}
	id := m.nextSegmentID
	m.nextSegmentID++
	m.segments[id] = &storage.GraphSegment{
		ID:          id,
		WorkspaceID: workspaceID,
		SegmentID:   uuid.New().String(),
		CreatedAt:   time.Now(),
	}
	return id, nil
}

// GetSegment returns a segment by its primary key ID.
func (m *mockRepository) GetSegment(ctx context.Context, id int64) (*storage.GraphSegment, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seg, ok := m.segments[id]
	if !ok {
		return nil, nil //nolint:nilnil // matches real Repository contract: nil,nil means not found
	}
	cp := *seg
	return &cp, nil
}

// GetSegmentByWorkspace returns segments for a given workspace.
func (m *mockRepository) GetSegmentByWorkspace(ctx context.Context, workspaceID string, limit, offset int) ([]storage.GraphSegment, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []storage.GraphSegment
	for _, seg := range m.segments {
		if seg.WorkspaceID == workspaceID {
			result = append(result, *seg)
		}
	}
	if offset >= len(result) {
		return nil, nil //nolint:nilnil // matches real Repository: empty pagination returns nil,nil
	}
	result = result[offset:]
	if limit > 0 && limit < len(result) {
		result = result[:limit]
	}
	return result, nil
}

// MergeSegments moves all external IDs and traits from source segments to target,
// then removes source segments. Traits are merged with latest-value-wins for
// conflicting keys, mirroring the PostgreSQL implementation's behavior.
func (m *mockRepository) MergeSegments(ctx context.Context, targetSegmentID int64, sourceSegmentIDs []int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mergeSegmentsErr != nil {
		return m.mergeSegmentsErr
	}
	now := time.Now()
	for _, sourceID := range sourceSegmentIDs {
		// Move external IDs from source to target.
		for _, eid := range m.externalIDs[sourceID] {
			eid.GraphID = targetSegmentID
			mergedAt := now
			eid.MergedAt = &mergedAt
			mergedFrom := sourceID
			eid.MergedFrom = &mergedFrom
			m.externalIDs[targetSegmentID] = append(m.externalIDs[targetSegmentID], eid)
		}
		delete(m.externalIDs, sourceID)

		// Merge traits: build map keyed by trait name, latest UpdatedAt wins.
		targetTraits := m.traits[targetSegmentID]
		sourceTraits := m.traits[sourceID]
		traitMap := make(map[string]*storage.Trait, len(targetTraits)+len(sourceTraits))
		for i := range targetTraits {
			t := targetTraits[i]
			traitMap[t.Key] = &t
		}
		for i := range sourceTraits {
			t := sourceTraits[i]
			t.GraphID = targetSegmentID
			existing, ok := traitMap[t.Key]
			if !ok || t.UpdatedAt.After(existing.UpdatedAt) {
				copied := t
				traitMap[t.Key] = &copied
			}
		}
		merged := make([]storage.Trait, 0, len(traitMap))
		for _, t := range traitMap {
			merged = append(merged, *t)
		}
		m.traits[targetSegmentID] = merged
		delete(m.traits, sourceID)

		// Remove source segment.
		delete(m.segments, sourceID)
	}
	return nil
}

// AddExternalID adds an external identifier to the specified graph segment.
func (m *mockRepository) AddExternalID(ctx context.Context, externalID storage.ExternalID) (int64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addExternalIDErr != nil {
		return 0, m.addExternalIDErr
	}
	id := m.nextExternalID
	m.nextExternalID++
	externalID.ID = id
	if externalID.CreatedAt.IsZero() {
		externalID.CreatedAt = time.Now()
	}
	m.externalIDs[externalID.GraphID] = append(m.externalIDs[externalID.GraphID], externalID)
	return id, nil
}

// GetExternalIDsBySegment returns all external IDs linked to a segment.
func (m *mockRepository) GetExternalIDsBySegment(ctx context.Context, segmentID int64) ([]storage.ExternalID, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.externalIDs[segmentID]
	// Return a copy to prevent external mutation.
	result := make([]storage.ExternalID, len(ids))
	copy(result, ids)
	return result, nil
}

// LookupByExternalID finds a segment by workspace, identifier type, and value.
// Returns (nil, nil) when no match is found, matching PostgreSQL's sql.ErrNoRows behavior.
func (m *mockRepository) LookupByExternalID(ctx context.Context, workspaceID, externalIDType, externalIDValue string) (*storage.GraphSegment, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lookupErr != nil {
		return nil, m.lookupErr
	}
	for graphID, ids := range m.externalIDs {
		for _, eid := range ids {
			if eid.ExternalIDType == externalIDType && eid.ExternalIDValue == externalIDValue {
				seg, ok := m.segments[graphID]
				if ok && seg.WorkspaceID == workspaceID {
					cp := *seg
					return &cp, nil
				}
			}
		}
	}
	return nil, nil //nolint:nilnil // matches real Repository contract: nil,nil means not found
}

// BulkAddExternalIDs adds multiple external IDs sequentially.
func (m *mockRepository) BulkAddExternalIDs(ctx context.Context, externalIDs []storage.ExternalID) error {
	for _, eid := range externalIDs {
		if _, err := m.AddExternalID(ctx, eid); err != nil {
			return err
		}
	}
	return nil
}

// SetTrait sets or updates a trait for a graph segment (upsert by key).
func (m *mockRepository) SetTrait(ctx context.Context, graphID int64, key, value string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	traits := m.traits[graphID]
	for i, t := range traits {
		if t.Key == key {
			traits[i].Value = value
			traits[i].UpdatedAt = time.Now()
			return nil
		}
	}
	m.nextTraitID++
	m.traits[graphID] = append(m.traits[graphID], storage.Trait{
		ID:        m.nextTraitID,
		GraphID:   graphID,
		Key:       key,
		Value:     value,
		UpdatedAt: time.Now(),
	})
	return nil
}

// GetTraits returns all traits for a graph segment.
func (m *mockRepository) GetTraits(ctx context.Context, graphID int64) ([]storage.Trait, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	traits := m.traits[graphID]
	result := make([]storage.Trait, len(traits))
	copy(result, traits)
	return result, nil
}

// BulkSetTraits sets multiple traits sequentially.
func (m *mockRepository) BulkSetTraits(ctx context.Context, traits []storage.Trait) error {
	for _, t := range traits {
		if err := m.SetTrait(ctx, t.GraphID, t.Key, t.Value); err != nil {
			return err
		}
	}
	return nil
}

// GetProfileData returns the complete profile data for a segment.
func (m *mockRepository) GetProfileData(ctx context.Context, segmentID int64) (*storage.ProfileData, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seg, ok := m.segments[segmentID]
	if !ok {
		return nil, nil //nolint:nilnil // matches real Repository contract: nil,nil means not found
	}
	return &storage.ProfileData{
		Segment:     *seg,
		ExternalIDs: m.externalIDs[segmentID],
		Traits:      m.traits[segmentID],
	}, nil
}

// WithTx executes fn within a mock transaction context. In the in-memory mock,
// a real *sql.Tx is not available, so we pass nil to the callback function.
// The resolver does not call WithTx directly, so this satisfies the interface.
func (m *mockRepository) WithTx(_ context.Context, fn func(tx *sql.Tx) error) error {
	return fn(nil)
}

// Ping checks connectivity (always succeeds for in-memory mock unless context cancelled).
func (m *mockRepository) Ping(ctx context.Context) error {
	return ctx.Err()
}

// ---------------------------------------------------------------------------
// Test Helper Methods
// ---------------------------------------------------------------------------

// addSegmentWithIDs pre-populates a segment with external identifiers in the mock.
// Returns the segment's primary key ID for use in test assertions.
func (m *mockRepository) addSegmentWithIDs(workspaceID string, ids ...IdentifierPair) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextSegmentID
	m.nextSegmentID++
	m.segments[id] = &storage.GraphSegment{
		ID:          id,
		WorkspaceID: workspaceID,
		SegmentID:   uuid.New().String(),
		CreatedAt:   time.Now(),
	}
	for _, p := range ids {
		m.nextExternalID++
		m.externalIDs[id] = append(m.externalIDs[id], storage.ExternalID{
			ID:              m.nextExternalID,
			GraphID:         id,
			WorkspaceID:     workspaceID,
			ExternalIDType:  p.Type,
			ExternalIDValue: p.Value,
			CreatedSource:   "test",
			CreatedAt:       time.Now(),
		})
	}
	return id
}

// addTraits pre-populates traits on a segment for merge-preservation tests.
func (m *mockRepository) addTraits(graphID int64, traitData map[string]string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range traitData {
		m.nextTraitID++
		m.traits[graphID] = append(m.traits[graphID], storage.Trait{
			ID:        m.nextTraitID,
			GraphID:   graphID,
			Key:       k,
			Value:     v,
			UpdatedAt: at,
		})
	}
}

// segmentCount returns the total number of segments in the mock repository.
func (m *mockRepository) segmentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.segments)
}

// Compile-time interface verification: mockRepository must implement storage.Repository.
var _ storage.Repository = (*mockRepository)(nil)

// ===========================================================================
// Phase 1: Resolution Strategy Tests
// ===========================================================================

// TestResolver_NewMatch_Strategy verifies that when no existing segment matches
// any provided identifier, the resolver creates a new segment with a valid UUID
// and links all identifiers to it. This mirrors warehouse/identity/identity.go
// lines 112-113: len(rudderIDs) == 0 → new UUID.
func TestResolver_NewMatch_Strategy(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-new-match"

	identifiers := []IdentifierPair{
		{Type: "user_id", Value: "user123"},
		{Type: "anonymous_id", Value: "anon-uuid-abc"},
	}

	result, err := r.Resolve(ctx, workspaceID, identifiers)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Strategy must be NewMatch.
	require.Equal(t, StrategyNewMatch, result.Strategy)

	// New segment must have a positive ID.
	require.True(t, result.SegmentID > 0, "new segment must have a positive ID")

	// Verify the segment record has a valid UUID and correct workspace.
	seg, err := repo.GetSegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.NotNil(t, seg, "segment must exist in storage")
	_, parseErr := uuid.Parse(seg.SegmentID)
	require.NoError(t, parseErr, "segment UUID must be valid RFC-4122")
	require.Equal(t, workspaceID, seg.WorkspaceID)

	// Both identifiers must be linked to the new segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.Len(t, ids, 2, "both identifiers should be linked")

	// Verify identifiers were reported as new.
	require.NotEmpty(t, result.NewIdentifiers, "new identifiers must be reported")
}

// TestResolver_SingleMatch_Strategy verifies that when exactly one existing segment
// matches an identifier, the resolver reuses that segment and adds any new identifiers.
// This mirrors identity.go lines 114-116: len(rudderIDs) == 1 → use existing.
func TestResolver_SingleMatch_Strategy(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-single-match"

	// Pre-populate: one segment with user_id:"user123".
	segID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "user_id", Value: "user123"})

	identifiers := []IdentifierPair{
		{Type: "user_id", Value: "user123"},
		{Type: "email", Value: "user@example.com"},
	}

	result, err := r.Resolve(ctx, workspaceID, identifiers)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Strategy must be SingleMatch.
	require.Equal(t, StrategySingleMatch, result.Strategy)

	// Must reuse the existing segment, not create a new one.
	require.Equal(t, segID, result.SegmentID)
	require.Equal(t, 1, repo.segmentCount(), "no new segment should be created")

	// The email identifier must have been added to the existing segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, segID)
	require.NoError(t, err)
	require.Len(t, ids, 2, "existing + new identifier")

	// Only the email should be reported as new.
	require.Len(t, result.NewIdentifiers, 1)
	require.Equal(t, "email", result.NewIdentifiers[0].Type)
	require.Equal(t, "user@example.com", result.NewIdentifiers[0].Value)
}

// TestResolver_MultiMatch_Strategy verifies that when identifiers match multiple
// existing segments, all segments are merged into the lowest-ID segment, the other
// segments are removed, and all identifiers are consolidated. This mirrors
// identity.go line 139: newID := rudderIDs[0] (the first/lowest).
func TestResolver_MultiMatch_Strategy(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-multi-match"

	// Pre-populate: two separate segments.
	seg1ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "user_id", Value: "user123"})
	seg2ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "email", Value: "user@example.com"})

	identifiers := []IdentifierPair{
		{Type: "user_id", Value: "user123"},
		{Type: "email", Value: "user@example.com"},
	}

	result, err := r.Resolve(ctx, workspaceID, identifiers)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Strategy must be MultiMatch.
	require.Equal(t, StrategyMultiMatch, result.Strategy)

	// Only ONE segment should remain after merge.
	require.Equal(t, 1, repo.segmentCount(), "segments must be merged into one")

	// The FIRST (lowest-ID) segment survives (per identity.go:139).
	require.Equal(t, seg1ID, result.SegmentID)

	// The second segment must appear in MergedSegmentIDs.
	require.NotNil(t, result.MergedSegmentIDs)
	require.Contains(t, result.MergedSegmentIDs, seg2ID)

	// All identifiers from both segments must exist in the merged segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, seg1ID)
	require.NoError(t, err)
	require.Len(t, ids, 2, "merged segment must have all identifiers")

	// The second segment must be gone from storage.
	seg2, err := repo.GetSegment(ctx, seg2ID)
	require.NoError(t, err)
	require.Nil(t, seg2, "merged-away segment must no longer exist")
}

// ===========================================================================
// Phase 2: Resolution Strategy Constants
// ===========================================================================

// TestResolutionStrategy_String verifies that the String() method on
// ResolutionStrategy constants returns the expected human-readable labels.
func TestResolutionStrategy_String(t *testing.T) {
	require.Equal(t, "new_match", StrategyNewMatch.String())
	require.Equal(t, "single_match", StrategySingleMatch.String())
	require.Equal(t, "multi_match", StrategyMultiMatch.String())
}

// ===========================================================================
// Phase 3: Edge Case Tests
// ===========================================================================

// TestResolver_EmptyIdentifiers verifies that calling Resolve with empty or nil
// identifiers returns an error rather than creating an orphan segment.
func TestResolver_EmptyIdentifiers(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()

	t.Run("empty slice", func(t *testing.T) {
		_, err := r.Resolve(ctx, "ws-empty", []IdentifierPair{})
		require.Error(t, err, "empty identifiers must return error")
	})

	t.Run("nil slice", func(t *testing.T) {
		_, err := r.Resolve(ctx, "ws-empty", nil)
		require.Error(t, err, "nil identifiers must return error")
	})

	t.Run("all values blocked by settings", func(t *testing.T) {
		// Use settings that block the specific test values.
		blockedSettings := settings.New(nil)
		_ = blockedSettings.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
			BlockedValues: []settings.BlockedValueRule{{Type: "exact", Value: "only-blocked"}},
			Limit:         settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
			Priority:      1,
		})
		rBlocked := NewResolver(repo, blockedSettings, nil, nil)
		_, err := rBlocked.Resolve(ctx, "ws-empty", []IdentifierPair{
			{Type: "user_id", Value: "only-blocked"},
		})
		require.Error(t, err, "all identifiers blocked should return error")
	})
}

// TestResolver_SingleIdentifier_NewMatch verifies that providing a single
// identifier (only one property, no pair) still creates a new segment when
// no match exists. This matches identity.go lines 89-91 where prop2 may be NULL.
func TestResolver_SingleIdentifier_NewMatch(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()

	identifiers := []IdentifierPair{
		{Type: "anonymous_id", Value: "single-anon-uuid"},
	}

	result, err := r.Resolve(ctx, "ws-single-id", identifiers)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyNewMatch, result.Strategy)

	// Segment must have exactly one identifier.
	ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	require.Equal(t, "anonymous_id", ids[0].ExternalIDType)
	require.Equal(t, "single-anon-uuid", ids[0].ExternalIDValue)
}

// TestResolver_SingleIdentifier_ExistingMatch verifies that providing a single
// identifier that already matches an existing segment returns SingleMatch without
// modifications (no new identifier to add).
func TestResolver_SingleIdentifier_ExistingMatch(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-single-existing"

	segID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "anonymous_id", Value: "known-anon"})

	result, err := r.Resolve(ctx, workspaceID, []IdentifierPair{
		{Type: "anonymous_id", Value: "known-anon"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategySingleMatch, result.Strategy)
	require.Equal(t, segID, result.SegmentID)

	// No new identifiers should be added since the only identifier already exists.
	require.Empty(t, result.NewIdentifiers, "no new identifiers expected")

	// Segment count must remain unchanged.
	require.Equal(t, 1, repo.segmentCount())
}

// TestResolver_DuplicateIdentifiersInEvent verifies that duplicate identifiers
// in the input are deduplicated before resolution, preventing duplicate edges.
func TestResolver_DuplicateIdentifiersInEvent(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()

	identifiers := []IdentifierPair{
		{Type: "user_id", Value: "dup-user"},
		{Type: "user_id", Value: "dup-user"}, // exact duplicate
	}

	result, err := r.Resolve(ctx, "ws-dedup", identifiers)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyNewMatch, result.Strategy)

	// Only ONE external ID should be stored (deduplication).
	ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.Len(t, ids, 1, "duplicate identifiers must be deduplicated")
	require.Equal(t, "dup-user", ids[0].ExternalIDValue)
}

// TestResolver_CrossWorkspaceIsolation verifies that segments in different
// workspaces with identical identifiers do not interfere with each other.
// Each workspace has its own identity namespace.
func TestResolver_CrossWorkspaceIsolation(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()

	ws1 := "ws-1-" + uuid.New().String()
	ws2 := "ws-2-" + uuid.New().String()

	// Same user_id in both workspaces.
	seg1ID := repo.addSegmentWithIDs(ws1, IdentifierPair{Type: "user_id", Value: "shared-user"})
	_ = repo.addSegmentWithIDs(ws2, IdentifierPair{Type: "user_id", Value: "shared-user"})

	// Resolve in workspace 1 — should only match ws-1's segment.
	result, err := r.Resolve(ctx, ws1, []IdentifierPair{
		{Type: "user_id", Value: "shared-user"},
		{Type: "email", Value: "ws1-only@example.com"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, StrategySingleMatch, result.Strategy)
	require.Equal(t, seg1ID, result.SegmentID)

	// Both workspace segments must still exist — ws-2 segment is untouched.
	require.Equal(t, 2, repo.segmentCount(), "cross-workspace segments must be isolated")
}

// TestResolver_MergePreservesAllTraits verifies that when a multi-match merge
// occurs, traits from all merged segments are preserved in the surviving segment.
// For conflicting keys, the latest (most recent UpdatedAt) value wins.
func TestResolver_MergePreservesAllTraits(t *testing.T) {
	repo := newMockRepository()
	s := settings.DefaultSettings()
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-trait-merge"

	// Create two segments.
	seg1ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "user_id", Value: "user-trait"})
	seg2ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "email", Value: "trait@example.com"})

	// Segment A traits (older timestamp).
	olderTime := time.Now().Add(-1 * time.Hour)
	repo.addTraits(seg1ID, map[string]string{
		"name":  "Alice",
		"email": "alice@old.com",
	}, olderTime)

	// Segment B traits (newer timestamp) — "email" conflicts, should win.
	newerTime := time.Now()
	repo.addTraits(seg2ID, map[string]string{
		"email": "alice@new.com",
		"phone": "+1234567890",
	}, newerTime)

	// Trigger multi-match merge.
	result, err := r.Resolve(ctx, workspaceID, []IdentifierPair{
		{Type: "user_id", Value: "user-trait"},
		{Type: "email", Value: "trait@example.com"},
	})
	require.NoError(t, err)
	require.Equal(t, StrategyMultiMatch, result.Strategy)
	require.Equal(t, seg1ID, result.SegmentID)

	// Verify merged traits on the surviving segment.
	traits, err := repo.GetTraits(ctx, seg1ID)
	require.NoError(t, err)

	traitMap := make(map[string]string, len(traits))
	for _, tr := range traits {
		traitMap[tr.Key] = tr.Value
	}

	require.Equal(t, "Alice", traitMap["name"], "unique trait from seg A preserved")
	require.Equal(t, "alice@new.com", traitMap["email"], "newer email value wins on conflict")
	require.Equal(t, "+1234567890", traitMap["phone"], "unique trait from seg B preserved")

	// Verify the merged-away segment's traits are gone.
	seg2Traits, err := repo.GetTraits(ctx, seg2ID)
	require.NoError(t, err)
	require.Empty(t, seg2Traits, "merged-away segment must have no traits")
}

// TestResolver_MergeOrderDeterminism verifies that when 3+ segments match, the
// FIRST (lowest ID) segment is always kept as the target. This is critical per
// identity.go:139 — newID := rudderIDs[0] — ensuring deterministic merge outcomes.
func TestResolver_MergeOrderDeterminism(t *testing.T) {
	repo := newMockRepository()
	// Use New(nil) for bare settings without restrictive per-type limits.
	s := settings.New(nil)
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-determinism"

	// Create 3 segments with different identifier types.
	seg1ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "ga_client_id", Value: "ga-001"})
	seg2ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "braze_id", Value: "braze-001"})
	seg3ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "ios_idfa", Value: "idfa-001"})

	// Provide all three identifiers to trigger multi-match with 3 segments.
	result, err := r.Resolve(ctx, workspaceID, []IdentifierPair{
		{Type: "ga_client_id", Value: "ga-001"},
		{Type: "braze_id", Value: "braze-001"},
		{Type: "ios_idfa", Value: "idfa-001"},
	})
	require.NoError(t, err)
	require.Equal(t, StrategyMultiMatch, result.Strategy)

	// The FIRST (lowest ID) segment must survive — deterministic ordering.
	require.Equal(t, seg1ID, result.SegmentID, "lowest-ID segment must be the merge target")

	// The other two must be merged away.
	require.Len(t, result.MergedSegmentIDs, 2)
	require.Contains(t, result.MergedSegmentIDs, seg2ID)
	require.Contains(t, result.MergedSegmentIDs, seg3ID)

	// Only one segment remains.
	require.Equal(t, 1, repo.segmentCount())

	// Verify all identifiers consolidated into the surviving segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, seg1ID)
	require.NoError(t, err)
	require.True(t, len(ids) >= 3, "merged segment must have all identifiers")
}

// ===========================================================================
// Phase 4: Settings Integration Tests
// ===========================================================================

// TestResolver_BlockedValueSkipped verifies that identifiers matching blocked
// value rules in settings are not used for resolution lookup. Only non-blocked
// identifiers participate in segment matching.
func TestResolver_BlockedValueSkipped(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)
	// Configure user_id to block the exact value "null".
	err := s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
		BlockedValues: []settings.BlockedValueRule{
			{Type: "exact", Value: "null"},
		},
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		Priority: 1,
	})
	require.NoError(t, err)

	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-blocked"

	result, err := r.Resolve(ctx, workspaceID, []IdentifierPair{
		{Type: "user_id", Value: "null"},         // blocked — should be filtered out
		{Type: "email", Value: "real@email.com"},  // not blocked — should be used
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyNewMatch, result.Strategy)

	// Verify only the email identifier was stored (user_id:"null" was blocked).
	ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.Len(t, ids, 1, "blocked identifier must not be stored")
	require.Equal(t, "email", ids[0].ExternalIDType)
	require.Equal(t, "real@email.com", ids[0].ExternalIDValue)
}

// TestResolver_BlockedValueRegex verifies regex-based blocked value rules work
// correctly. The pattern "^[0-]*$" should block values like "0000" and "0-0-0".
func TestResolver_BlockedValueRegex(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)
	err := s.SetIdentifierConfig("anonymous_id", &settings.IdentifierConfig{
		BlockedValues: []settings.BlockedValueRule{
			{Type: "regex", Value: "^[0-]*$"},
		},
		Limit:    settings.IdentifierLimit{MaxCount: 10, TimeWindow: "monthly"},
		Priority: 3,
	})
	require.NoError(t, err)

	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()

	t.Run("regex blocked value", func(t *testing.T) {
		result, err := r.Resolve(ctx, "ws-regex", []IdentifierPair{
			{Type: "anonymous_id", Value: "0000"},
			{Type: "email", Value: "valid@test.com"},
		})
		require.NoError(t, err)
		require.NotNil(t, result)

		ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
		require.NoError(t, err)
		// Only email should be stored; anonymous_id "0000" matches ^[0-]*$.
		require.Len(t, ids, 1)
		require.Equal(t, "email", ids[0].ExternalIDType)
	})

	t.Run("non-matching value passes", func(t *testing.T) {
		result, err := r.Resolve(ctx, "ws-regex2", []IdentifierPair{
			{Type: "anonymous_id", Value: "abc-123"},
		})
		require.NoError(t, err)
		require.NotNil(t, result)

		ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
		require.NoError(t, err)
		require.Len(t, ids, 1, "non-matching value should pass through")
	})
}

// TestResolver_IdentifierLimitEnforced verifies that per-identifier-type limits
// are enforced during resolution. When a segment already has the maximum number
// of identifiers of a given type, additional identifiers of that type are rejected
// while other types with remaining capacity are still added.
func TestResolver_IdentifierLimitEnforced(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)
	// anonymous_id: limit of 3
	err := s.SetIdentifierConfig("anonymous_id", &settings.IdentifierConfig{
		Limit:    settings.IdentifierLimit{MaxCount: 3, TimeWindow: "monthly"},
		Priority: 3,
	})
	require.NoError(t, err)
	// user_id: limit of 5 (higher capacity)
	err = s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		Priority: 1,
	})
	require.NoError(t, err)

	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-limit"

	// Pre-populate segment with 3 anonymous_ids (at limit).
	segID := repo.addSegmentWithIDs(workspaceID,
		IdentifierPair{Type: "anonymous_id", Value: "anon-1"},
		IdentifierPair{Type: "anonymous_id", Value: "anon-2"},
		IdentifierPair{Type: "anonymous_id", Value: "anon-3"},
	)

	// Resolve: anon-1 triggers single match, attempt to add anon-4 (blocked by limit)
	// and user-new (allowed, different type).
	result, err := r.Resolve(ctx, workspaceID, []IdentifierPair{
		{Type: "anonymous_id", Value: "anon-1"},  // existing — triggers single match
		{Type: "anonymous_id", Value: "anon-4"},  // new but limit reached → rejected
		{Type: "user_id", Value: "user-new"},      // new, different type → added
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategySingleMatch, result.Strategy)
	require.Equal(t, segID, result.SegmentID)

	// Verify the 4th anonymous_id was NOT added (limit = 3, count already 3).
	ids, err := repo.GetExternalIDsBySegment(ctx, segID)
	require.NoError(t, err)

	anonCount := 0
	userFound := false
	for _, id := range ids {
		if id.ExternalIDType == "anonymous_id" {
			anonCount++
		}
		if id.ExternalIDType == "user_id" && id.ExternalIDValue == "user-new" {
			userFound = true
		}
	}
	require.Equal(t, 3, anonCount, "anonymous_id count must stay at 3 (limit enforced)")
	require.True(t, userFound, "user_id must be added (different type, within limit)")
	require.False(t, anonCount > 3, "anonymous_id must not exceed configured limit")
}

// TestResolver_PriorityOrdering verifies that multi-match merges produce
// deterministic results. The resolver sorts matched segment IDs ascending and
// always keeps the lowest-ID segment as the merge target, regardless of
// identifier priority configuration. Priority affects identifier filtering
// and ordering but not the merge target selection.
func TestResolver_PriorityOrdering(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)

	// Configure priorities: user_id highest, email middle, anonymous_id lowest.
	err := s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
		Priority: 1,
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	})
	require.NoError(t, err)
	err = s.SetIdentifierConfig("email", &settings.IdentifierConfig{
		Priority: 2,
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	})
	require.NoError(t, err)
	err = s.SetIdentifierConfig("anonymous_id", &settings.IdentifierConfig{
		Priority: 3,
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	})
	require.NoError(t, err)

	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-priority"

	// Create segments: seg1 (email, priority 2), seg2 (anonymous_id, priority 3).
	// seg1 has the lowest ID, so it becomes the target regardless of priority.
	seg1ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "email", Value: "priority@test.com"})
	seg2ID := repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "anonymous_id", Value: "priority-anon"})

	result, err := r.Resolve(ctx, workspaceID, []IdentifierPair{
		{Type: "email", Value: "priority@test.com"},
		{Type: "anonymous_id", Value: "priority-anon"},
	})
	require.NoError(t, err)
	require.Equal(t, StrategyMultiMatch, result.Strategy)

	// Lowest-ID segment (seg1) is the target — priority does not override ID ordering.
	require.Equal(t, seg1ID, result.SegmentID,
		"merge target must be lowest-ID segment regardless of identifier priority")
	require.Contains(t, result.MergedSegmentIDs, seg2ID)

	// All identifiers consolidated in merged segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, seg1ID)
	require.NoError(t, err)
	require.True(t, len(ids) >= 2, "merged segment must have identifiers from both segments")
}

// ===========================================================================
// Phase 5: Result Type Tests
// ===========================================================================

// TestResolutionResult_Fields verifies that the ResolutionResult struct contains
// all expected fields and they are populated correctly for each strategy.
func TestResolutionResult_Fields(t *testing.T) {
	t.Run("MultiMatch result has all fields", func(t *testing.T) {
		result := &ResolutionResult{
			Strategy:         StrategyMultiMatch,
			SegmentID:        42,
			MatchedSegments:  []int64{42, 99},
			NewIdentifiers:   []IdentifierPair{{Type: "email", Value: "test@example.com"}},
			MergedSegmentIDs: []int64{99},
		}

		require.Equal(t, StrategyMultiMatch, result.Strategy)
		require.Equal(t, int64(42), result.SegmentID)
		require.Len(t, result.MatchedSegments, 2)
		require.Len(t, result.NewIdentifiers, 1)
		require.Len(t, result.MergedSegmentIDs, 1)
		require.Equal(t, "email", result.NewIdentifiers[0].Type)
	})

	t.Run("NewMatch result has minimal fields", func(t *testing.T) {
		result := &ResolutionResult{
			Strategy:  StrategyNewMatch,
			SegmentID: 1,
		}

		require.Equal(t, StrategyNewMatch, result.Strategy)
		require.Equal(t, int64(1), result.SegmentID)
		require.Nil(t, result.MatchedSegments, "new match should have no matched segments")
		require.Nil(t, result.MergedSegmentIDs, "new match should have no merged segments")
	})

	t.Run("SingleMatch result has matched segments", func(t *testing.T) {
		result := &ResolutionResult{
			Strategy:        StrategySingleMatch,
			SegmentID:       7,
			MatchedSegments: []int64{7},
			NewIdentifiers:  []IdentifierPair{{Type: "phone", Value: "+1234"}},
		}

		require.Equal(t, StrategySingleMatch, result.Strategy)
		require.Len(t, result.MatchedSegments, 1)
		require.NotNil(t, result.NewIdentifiers)
		require.Nil(t, result.MergedSegmentIDs, "single match should have no merged segments")
	})
}

// ===========================================================================
// Phase 6: Atomicity and Error Tests
// ===========================================================================

// TestResolver_StorageError_MergeFailure verifies that when MergeSegments()
// fails, the error is propagated and no partial changes are committed.
func TestResolver_StorageError_MergeFailure(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()
	workspaceID := "ws-merge-error"

	// Create two segments that will trigger multi-match.
	repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "user_id", Value: "error-user"})
	repo.addSegmentWithIDs(workspaceID, IdentifierPair{Type: "email", Value: "error@example.com"})

	// Inject a MergeSegments error to simulate storage failure.
	repo.mergeSegmentsErr = fmt.Errorf("simulated merge failure")

	_, err := r.Resolve(ctx, workspaceID, []IdentifierPair{
		{Type: "user_id", Value: "error-user"},
		{Type: "email", Value: "error@example.com"},
	})
	require.Error(t, err, "merge failure must propagate as error")
	require.Contains(t, err.Error(), "merging segments", "error should indicate merge context")

	// Both segments must still exist — no partial merge occurred.
	require.Equal(t, 2, repo.segmentCount(), "segments must not be modified on merge failure")
}

// TestResolver_StorageError_CreateSegmentFailure verifies that CreateSegment
// errors during new-match are properly propagated.
func TestResolver_StorageError_CreateSegmentFailure(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()

	// Inject CreateSegment error.
	repo.createSegmentErr = fmt.Errorf("simulated create failure")

	_, err := r.Resolve(ctx, "ws-create-error", []IdentifierPair{
		{Type: "user_id", Value: "new-user"},
	})
	require.Error(t, err, "create failure must propagate as error")
	require.Contains(t, err.Error(), "creating segment", "error should indicate creation context")

	// No segments should exist.
	require.Equal(t, 0, repo.segmentCount(), "no segment should be created on error")
}

// TestResolver_StorageError_LookupFailure verifies that LookupByExternalID
// errors during segment matching are properly propagated.
func TestResolver_StorageError_LookupFailure(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)
	r := NewResolver(repo, s, nil, nil)
	ctx := context.Background()

	// Inject LookupByExternalID error.
	repo.lookupErr = fmt.Errorf("simulated lookup failure")

	_, err := r.Resolve(ctx, "ws-lookup-error", []IdentifierPair{
		{Type: "user_id", Value: "lookup-user"},
	})
	require.Error(t, err, "lookup failure must propagate as error")
}

// TestResolver_ContextCancellation verifies that the resolver respects context
// cancellation and returns context.Canceled when the context is cancelled
// before or during resolution.
func TestResolver_ContextCancellation(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)
	r := NewResolver(repo, s, nil, nil)

	// Cancel the context immediately before resolution starts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := r.Resolve(ctx, "ws-cancel", []IdentifierPair{
		{Type: "user_id", Value: "cancel-user"},
	})
	require.Error(t, err, "cancelled context must cause error")
	require.ErrorIs(t, err, context.Canceled, "error chain must contain context.Canceled")
}

// TestResolver_AllBlockedIdentifiers verifies that when all provided identifiers
// are blocked by settings, the resolver returns an error (no valid identifiers).
func TestResolver_AllBlockedIdentifiers(t *testing.T) {
	repo := newMockRepository()
	s := settings.New(nil)
	err := s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
		BlockedValues: []settings.BlockedValueRule{
			{Type: "exact", Value: "blocked-val"},
		},
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		Priority: 1,
	})
	require.NoError(t, err)
	err = s.SetIdentifierConfig("email", &settings.IdentifierConfig{
		BlockedValues: []settings.BlockedValueRule{
			{Type: "exact", Value: "blocked@test.com"},
		},
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		Priority: 2,
	})
	require.NoError(t, err)

	r := NewResolver(repo, s, nil, nil)

	_, err = r.Resolve(context.Background(), "ws-all-blocked", []IdentifierPair{
		{Type: "user_id", Value: "blocked-val"},
		{Type: "email", Value: "blocked@test.com"},
	})
	require.Error(t, err, "all identifiers blocked must return error")
}
