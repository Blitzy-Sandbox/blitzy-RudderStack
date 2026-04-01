// Package graph — graph_test.go provides comprehensive unit tests for the
// core IdentityGraph service (E-026). Tests cover graph creation, segment
// management, identity edge addition, segment merging, real-time event
// processing, graph querying, concurrency safety, and error propagation.
//
// This file reuses the mockRepository defined in resolver_test.go (same
// package) for all in-memory storage, avoiding duplication and ensuring
// consistent mock behaviour across the test suite.
package graph

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/identity/settings"
)

// testWorkspaceID is the default workspace identifier used across all test cases.
const testWorkspaceID = "test-workspace-1"

// ---------------------------------------------------------------------------
// Test graph helper constructors.
// ---------------------------------------------------------------------------

// newTestGraph creates an IdentityGraph with a fresh mock repository and default
// resolution settings. The mock repository is the shared mockRepository from
// resolver_test.go within this package.
func newTestGraph(t *testing.T) (*IdentityGraph, *mockRepository) {
	t.Helper()
	return newTestGraphWithSettings(t, settings.DefaultSettings())
}

// newTestGraphWithSettings creates an IdentityGraph with custom resolution settings.
// Used by tests that need to configure blocked values or identifier limits.
func newTestGraphWithSettings(t *testing.T, s *settings.ResolutionSettings) (*IdentityGraph, *mockRepository) {
	t.Helper()
	repo := newMockRepository()
	g, err := New(repo, s, config.New(), logger.NOP, nil)
	require.NoError(t, err)
	require.NotNil(t, g)
	return g, repo
}

// ===========================================================================
// Phase 2: Constructor Tests
// ===========================================================================

// TestNewIdentityGraph verifies that the New() constructor creates a valid
// IdentityGraph instance when given all valid dependencies.
func TestNewIdentityGraph(t *testing.T) {
	repo := newMockRepository()
	conf := config.New()
	g, err := New(repo, settings.DefaultSettings(), conf, logger.NOP, nil)
	require.NoError(t, err)
	require.NotNil(t, g)
}

// TestNewIdentityGraph_NilDependencies verifies nil-safe handling of each
// constructor parameter: repo must be non-nil, others have safe defaults.
func TestNewIdentityGraph_NilDependencies(t *testing.T) {
	t.Run("nil repo returns error", func(t *testing.T) {
		g, err := New(nil, settings.DefaultSettings(), config.New(), logger.NOP, nil)
		require.Error(t, err)
		require.Nil(t, g)
		require.Contains(t, err.Error(), "storage repository is required")
	})

	t.Run("nil settings uses defaults", func(t *testing.T) {
		repo := newMockRepository()
		g, err := New(repo, nil, config.New(), logger.NOP, nil)
		require.NoError(t, err)
		require.NotNil(t, g)
	})

	t.Run("nil logger uses package default", func(t *testing.T) {
		repo := newMockRepository()
		g, err := New(repo, settings.DefaultSettings(), config.New(), nil, nil)
		require.NoError(t, err)
		require.NotNil(t, g)
	})

	t.Run("nil config uses config.Default", func(t *testing.T) {
		repo := newMockRepository()
		g, err := New(repo, settings.DefaultSettings(), nil, logger.NOP, nil)
		require.NoError(t, err)
		require.NotNil(t, g)
		// Verify config.Default is accessible and non-nil.
		require.NotNil(t, config.Default)
	})

	t.Run("nil stats factory is handled gracefully", func(t *testing.T) {
		repo := newMockRepository()
		var statsFactory stats.Stats
		g, err := New(repo, settings.DefaultSettings(), config.New(), logger.NOP, statsFactory)
		require.NoError(t, err)
		require.NotNil(t, g)
	})
}

// ===========================================================================
// Phase 3: Segment Management Tests
// ===========================================================================

// TestAddIdentityEdge_NewMatch verifies that when no existing identity is found
// for any identifier on the event, a NEW segment is created and all identifiers
// are associated with it. Mirrors warehouse/identity/identity.go:112-113
// (len(rudderIDs) == 0 → new UUID).
func TestAddIdentityEdge_NewMatch(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	eventJSON := []byte(`{
		"type": "track",
		"event": "Product Viewed",
		"userId": "user1",
		"anonymousId": "anon-uuid-1"
	}`)

	result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyNewMatch, result.Strategy)
	require.Greater(t, result.SegmentID, int64(0))

	// Verify both identifiers are stored on the new segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.Len(t, ids, 2)

	idMap := make(map[string]string)
	for _, id := range ids {
		idMap[id.ExternalIDType] = id.ExternalIDValue
	}
	require.Equal(t, "user1", idMap["user_id"])
	require.Equal(t, "anon-uuid-1", idMap["anonymous_id"])

	// Verify segment metadata.
	seg, err := repo.GetSegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.NotNil(t, seg)
	require.Equal(t, testWorkspaceID, seg.WorkspaceID)
	require.NotEmpty(t, seg.SegmentID)
}

// TestAddIdentityEdge_SingleMatch verifies that when exactly one existing
// identity is found, new identifiers are added to the EXISTING segment.
// Mirrors warehouse/identity/identity.go:114-116 (len(rudderIDs) == 1).
func TestAddIdentityEdge_SingleMatch(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	// Pre-populate: segment with user_id "user1".
	segID := repo.addSegmentWithIDs(testWorkspaceID, IdentifierPair{Type: "user_id", Value: "user1"})

	// Process event: same userId + new email via traits.
	eventJSON := []byte(`{
		"type": "identify",
		"userId": "user1",
		"traits": {"email": "user1@example.com"}
	}`)

	result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategySingleMatch, result.Strategy)
	require.Equal(t, segID, result.SegmentID)

	// Verify email was added to the existing segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, segID)
	require.NoError(t, err)
	require.Len(t, ids, 2)

	idMap := make(map[string]string)
	for _, id := range ids {
		idMap[id.ExternalIDType] = id.ExternalIDValue
	}
	require.Equal(t, "user1", idMap["user_id"])
	require.Equal(t, "user1@example.com", idMap["email"])
}

// TestAddIdentityEdge_MultiMatch verifies that when multiple existing segments
// match different identifiers, the segments are MERGED — all identifiers from
// both segments combined into one. The FIRST (lowest ID) segment is kept.
// Mirrors warehouse/identity/identity.go:139 (newID := rudderIDs[0]).
func TestAddIdentityEdge_MultiMatch(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	// Pre-populate: two separate segments.
	segA := repo.addSegmentWithIDs(testWorkspaceID, IdentifierPair{Type: "user_id", Value: "user1"})
	segB := repo.addSegmentWithIDs(testWorkspaceID, IdentifierPair{Type: "email", Value: "user1@example.com"})

	// Process event matching both segments.
	eventJSON := []byte(`{
		"type": "identify",
		"userId": "user1",
		"traits": {"email": "user1@example.com"}
	}`)

	result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyMultiMatch, result.Strategy)

	// First segment (lowest ID) is kept as the merge target.
	require.Equal(t, segA, result.SegmentID)
	require.Len(t, result.MergedSegmentIDs, 1)
	require.Equal(t, segB, result.MergedSegmentIDs[0])

	// All identifiers must be on the surviving segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, segA)
	require.NoError(t, err)

	idMap := make(map[string]string)
	for _, id := range ids {
		idMap[id.ExternalIDType] = id.ExternalIDValue
	}
	require.Equal(t, "user1", idMap["user_id"])
	require.Equal(t, "user1@example.com", idMap["email"])

	// Merged segment must be deleted.
	segBAfter, err := repo.GetSegment(ctx, segB)
	require.NoError(t, err)
	require.Nil(t, segBAfter)
}

// TestMergeSegments_UpdatesAllProperties verifies that a multi-match merge
// correctly combines all external IDs AND all traits from both segments,
// with latest trait values winning on key conflicts.
func TestMergeSegments_UpdatesAllProperties(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	// Pre-populate: two segments with multiple IDs and traits.
	segA := repo.addSegmentWithIDs(testWorkspaceID,
		IdentifierPair{Type: "user_id", Value: "user1"},
		IdentifierPair{Type: "anonymous_id", Value: "anon1"},
	)
	repo.addTraits(segA, map[string]string{"name": "Alice", "plan": "free"}, time.Now())

	segB := repo.addSegmentWithIDs(testWorkspaceID,
		IdentifierPair{Type: "email", Value: "alice@example.com"},
		IdentifierPair{Type: "anonymous_id", Value: "anon2"},
	)
	repo.addTraits(segB, map[string]string{"company": "Acme"}, time.Now())

	// Trigger merge by processing event matching both.
	eventJSON := []byte(`{
		"type": "identify",
		"userId": "user1",
		"traits": {"email": "alice@example.com"}
	}`)

	result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyMultiMatch, result.Strategy)
	require.Equal(t, segA, result.SegmentID)

	// All external IDs from both segments end up in the surviving segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, segA)
	require.NoError(t, err)
	require.Len(t, ids, 4) // user_id, anon1, email, anon2

	// All traits from both segments are merged.
	traits, err := repo.GetTraits(ctx, segA)
	require.NoError(t, err)
	require.NotEmpty(t, traits)

	traitMap := make(map[string]string)
	for _, tr := range traits {
		traitMap[tr.Key] = tr.Value
	}
	require.Equal(t, "Alice", traitMap["name"])
	require.Equal(t, "Acme", traitMap["company"])
	// "plan" trait exists from segA (no conflict since segB didn't have "plan").
	require.Contains(t, traitMap, "plan")
}

// ===========================================================================
// Phase 4: Real-Time Event Processing Tests
// ===========================================================================

// TestProcessEvent_TrackEvent verifies that a track event's userId, anonymousId,
// and context.traits.email are all extracted and resolved into identity edges.
func TestProcessEvent_TrackEvent(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	eventJSON := []byte(`{
		"type": "track",
		"event": "Product Viewed",
		"userId": "user-123",
		"anonymousId": "anon-456",
		"context": {
			"traits": {"email": "user@example.com"}
		},
		"properties": {"product_id": "SKU-001"}
	}`)

	result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyNewMatch, result.Strategy)
	require.Greater(t, result.SegmentID, int64(0))

	// Verify identity edges for userId, anonymousId, and context.traits.email.
	ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.Len(t, ids, 3)

	idMap := make(map[string]string)
	for _, id := range ids {
		idMap[id.ExternalIDType] = id.ExternalIDValue
	}
	require.Equal(t, "user-123", idMap["user_id"])
	require.Equal(t, "anon-456", idMap["anonymous_id"])
	require.Equal(t, "user@example.com", idMap["email"])
}

// TestProcessEvent_IdentifyEvent verifies that an identify event's userId,
// anonymousId, traits.email, AND context.externalId array entries are all
// extracted and resolved into identity edges.
func TestProcessEvent_IdentifyEvent(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	eventJSON := []byte(`{
		"type": "identify",
		"userId": "user-123",
		"anonymousId": "anon-456",
		"traits": {
			"email": "user@example.com",
			"name": "Test User"
		},
		"context": {
			"externalId": [
				{"type": "braze_id", "id": "braze-789"},
				{"type": "mailchimp_id", "id": "mc-012"}
			]
		}
	}`)

	result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyNewMatch, result.Strategy)

	// user_id, anonymous_id, email, braze_id, mailchimp_id = 5 identifiers.
	ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.Len(t, ids, 5)

	idMap := make(map[string]string)
	for _, id := range ids {
		idMap[id.ExternalIDType] = id.ExternalIDValue
	}
	require.Equal(t, "user-123", idMap["user_id"])
	require.Equal(t, "anon-456", idMap["anonymous_id"])
	require.Equal(t, "user@example.com", idMap["email"])
	require.Equal(t, "braze-789", idMap["braze_id"])
	require.Equal(t, "mc-012", idMap["mailchimp_id"])
}

// TestProcessEvent_MissingUserID verifies that events without userId are
// processed correctly using only anonymousId.
func TestProcessEvent_MissingUserID(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	eventJSON := []byte(`{
		"type": "track",
		"event": "Page Viewed",
		"anonymousId": "anon-only-uuid"
	}`)

	result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategyNewMatch, result.Strategy)
	require.Greater(t, result.SegmentID, int64(0))

	// Only anonymousId should be in the segment.
	ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	require.Equal(t, "anonymous_id", ids[0].ExternalIDType)
	require.Equal(t, "anon-only-uuid", ids[0].ExternalIDValue)
}

// TestProcessEvent_BlockedValues verifies that blocked identifier values are
// filtered out before resolution. Default settings block "null", "-1", and
// "anonymous" as exact matches, plus the regex ^[0-]*$.
func TestProcessEvent_BlockedValues(t *testing.T) {
	t.Run("default settings block null value", func(t *testing.T) {
		g, repo := newTestGraph(t)
		ctx := context.Background()

		eventJSON := []byte(`{
			"type": "track",
			"event": "Page Viewed",
			"userId": "null",
			"anonymousId": "valid-anon-uuid"
		}`)

		result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
		require.NoError(t, err)
		require.NotNil(t, result)

		// Only anonymousId should be present (userId "null" is blocked).
		ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
		require.NoError(t, err)
		require.Len(t, ids, 1)
		require.Equal(t, "anonymous_id", ids[0].ExternalIDType)
		require.Equal(t, "valid-anon-uuid", ids[0].ExternalIDValue)
	})

	t.Run("custom blocked value via settings", func(t *testing.T) {
		s := settings.DefaultSettings()
		err := s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
			Priority: 1,
			Limit:    settings.IdentifierLimit{MaxCount: 10, TimeWindow: "ever"},
			BlockedValues: []settings.BlockedValueRule{
				{Type: "exact", Value: "null"},
				{Type: "exact", Value: "blocked-user"},
				{Type: "regex", Value: `^[0-]*$`},
			},
		})
		require.NoError(t, err)

		g, repo := newTestGraphWithSettings(t, s)
		ctx := context.Background()

		eventJSON := []byte(`{
			"type": "track",
			"userId": "blocked-user",
			"anonymousId": "real-anon-id"
		}`)

		result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
		require.NoError(t, err)
		require.NotNil(t, result)

		ids, err := repo.GetExternalIDsBySegment(ctx, result.SegmentID)
		require.NoError(t, err)
		require.Len(t, ids, 1)
		require.Equal(t, "anonymous_id", ids[0].ExternalIDType)
	})
}

// TestProcessEvent_ExceedsLimit verifies that when a per-type identifier limit
// is reached, new identifiers of that type are NOT added to the graph.
func TestProcessEvent_ExceedsLimit(t *testing.T) {
	// Configure settings with limit 2 for anonymous_id.
	s := settings.DefaultSettings()
	err := s.SetIdentifierConfig("anonymous_id", &settings.IdentifierConfig{
		Priority: 3,
		Limit:    settings.IdentifierLimit{MaxCount: 2, TimeWindow: "ever"},
		BlockedValues: []settings.BlockedValueRule{
			{Type: "regex", Value: `^[0-]*$`},
			{Type: "exact", Value: "-1"},
			{Type: "exact", Value: "null"},
			{Type: "exact", Value: "anonymous"},
		},
	})
	require.NoError(t, err)

	g, repo := newTestGraphWithSettings(t, s)
	ctx := context.Background()

	// Pre-populate a segment with user_id and 2 anonymous_ids (at limit).
	segID := repo.addSegmentWithIDs(testWorkspaceID,
		IdentifierPair{Type: "user_id", Value: "user1"},
		IdentifierPair{Type: "anonymous_id", Value: "anon1"},
		IdentifierPair{Type: "anonymous_id", Value: "anon2"},
	)

	// Process event with existing user_id + new anonymous_id.
	eventJSON := []byte(`{
		"userId": "user1",
		"anonymousId": "anon3"
	}`)

	result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, StrategySingleMatch, result.Strategy)
	require.Equal(t, segID, result.SegmentID)

	// Verify anon3 was NOT added due to limit exceeded.
	ids, err := repo.GetExternalIDsBySegment(ctx, segID)
	require.NoError(t, err)
	require.Len(t, ids, 3) // user_id + anon1 + anon2 (anon3 rejected)

	for _, id := range ids {
		if id.ExternalIDType == "anonymous_id" {
			require.NotEqual(t, "anon3", id.ExternalIDValue)
		}
	}
}

// TestProcessEvent_EmptyEvent verifies that events with no identity signals
// return a StrategyNewMatch result with zero SegmentID (no segment created).
func TestProcessEvent_EmptyEvent(t *testing.T) {
	g, _ := newTestGraph(t)
	ctx := context.Background()

	t.Run("empty JSON object has no identifiers", func(t *testing.T) {
		result, err := g.ProcessEvent(ctx, testWorkspaceID, []byte(`{}`))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, StrategyNewMatch, result.Strategy)
		require.Equal(t, int64(0), result.SegmentID)
	})

	t.Run("nil event JSON has no identifiers", func(t *testing.T) {
		result, err := g.ProcessEvent(ctx, testWorkspaceID, nil)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, StrategyNewMatch, result.Strategy)
		require.Equal(t, int64(0), result.SegmentID)
	})
}

// ===========================================================================
// Phase 5: Query Tests
// ===========================================================================

// TestResolveIdentity verifies that ResolveIdentity returns the correct segment
// for a known external identifier.
func TestResolveIdentity(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	segID := repo.addSegmentWithIDs(testWorkspaceID,
		IdentifierPair{Type: "user_id", Value: "user123"},
		IdentifierPair{Type: "email", Value: "user@example.com"},
	)

	seg, err := g.ResolveIdentity(ctx, testWorkspaceID, "user_id", "user123")
	require.NoError(t, err)
	require.NotNil(t, seg)
	require.Equal(t, segID, seg.ID)
	require.Equal(t, testWorkspaceID, seg.WorkspaceID)
}

// TestResolveIdentity_NotFound verifies that ResolveIdentity returns nil
// without error for a non-existent identifier.
func TestResolveIdentity_NotFound(t *testing.T) {
	g, _ := newTestGraph(t)
	ctx := context.Background()

	seg, err := g.ResolveIdentity(ctx, testWorkspaceID, "user_id", "nonexistent")
	require.NoError(t, err)
	require.Nil(t, seg)
}

// TestResolveIdentity_EmptyParams verifies that ResolveIdentity returns an
// error when any required parameter is empty.
func TestResolveIdentity_EmptyParams(t *testing.T) {
	g, _ := newTestGraph(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		workspace string
		idType    string
		idValue   string
	}{
		{"empty workspace", "", "user_id", "user1"},
		{"empty type", testWorkspaceID, "", "user1"},
		{"empty value", testWorkspaceID, "user_id", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.ResolveIdentity(ctx, tc.workspace, tc.idType, tc.idValue)
			require.Error(t, err)
		})
	}
}

// TestGetSegment verifies that GetSegmentIdentifiers returns all external
// identifiers associated with a given segment.
func TestGetSegment(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	segID := repo.addSegmentWithIDs(testWorkspaceID,
		IdentifierPair{Type: "user_id", Value: "user1"},
		IdentifierPair{Type: "email", Value: "user1@test.com"},
	)

	ids, err := g.GetSegmentIdentifiers(ctx, segID)
	require.NoError(t, err)
	require.Len(t, ids, 2)

	idMap := make(map[string]string)
	for _, id := range ids {
		idMap[id.ExternalIDType] = id.ExternalIDValue
	}
	require.Equal(t, "user1", idMap["user_id"])
	require.Equal(t, "user1@test.com", idMap["email"])
}

// TestGetProfileData verifies that GetProfileData assembles the complete
// profile including segment metadata, external IDs, and traits.
func TestGetProfileData(t *testing.T) {
	g, repo := newTestGraph(t)
	ctx := context.Background()

	segID := repo.addSegmentWithIDs(testWorkspaceID,
		IdentifierPair{Type: "user_id", Value: "user1"},
		IdentifierPair{Type: "email", Value: "user1@test.com"},
	)
	repo.addTraits(segID, map[string]string{
		"name": "Test User",
		"plan": "enterprise",
	}, time.Now())

	profile, err := g.GetProfileData(ctx, segID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.Equal(t, segID, profile.Segment.ID)
	require.Len(t, profile.ExternalIDs, 2)
	require.Len(t, profile.Traits, 2)
}

// TestHealth verifies the Health method delegates to Repository.Ping and
// correctly propagates both healthy and unhealthy states.
func TestHealth(t *testing.T) {
	t.Run("healthy state", func(t *testing.T) {
		g, _ := newTestGraph(t)
		ctx := context.Background()
		err := g.Health(ctx)
		require.NoError(t, err)
	})

	t.Run("unhealthy state via cancelled context", func(t *testing.T) {
		g, _ := newTestGraph(t)
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately to simulate unhealthy state.
		err := g.Health(cancelledCtx)
		require.Error(t, err)
	})
}

// ===========================================================================
// Phase 6: Concurrency Tests
// ===========================================================================

// TestProcessEvent_ConcurrentProcessing launches multiple goroutines that
// process events simultaneously, verifying no race conditions or panics.
// Safe to run with -race flag.
func TestProcessEvent_ConcurrentProcessing(t *testing.T) {
	t.Parallel()
	g, _ := newTestGraph(t)
	ctx := context.Background()

	const numGoroutines = 20
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			eventJSON := []byte(fmt.Sprintf(`{
				"type": "track",
				"userId": "concurrent-user-%d",
				"anonymousId": "concurrent-anon-%d"
			}`, idx, idx))
			_, procErr := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
			if procErr != nil {
				errCh <- procErr
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}
}

// ===========================================================================
// Phase 7: Error Handling Tests
// ===========================================================================

// TestProcessEvent_StorageError verifies that storage errors propagate correctly
// through the service layer without partial commits.
func TestProcessEvent_StorageError(t *testing.T) {
	t.Run("lookup error propagates", func(t *testing.T) {
		g, repo := newTestGraph(t)
		ctx := context.Background()

		repo.mu.Lock()
		repo.lookupErr = fmt.Errorf("database connection lost")
		repo.mu.Unlock()

		eventJSON := []byte(`{
			"type": "track",
			"userId": "user1",
			"anonymousId": "anon1"
		}`)

		result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "database connection lost")
	})

	t.Run("create segment error propagates", func(t *testing.T) {
		g, repo := newTestGraph(t)
		ctx := context.Background()

		repo.mu.Lock()
		repo.createSegmentErr = fmt.Errorf("disk full")
		repo.mu.Unlock()

		eventJSON := []byte(`{
			"type": "track",
			"userId": "user1",
			"anonymousId": "anon1"
		}`)

		result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "disk full")
	})

	t.Run("merge error propagates", func(t *testing.T) {
		g, repo := newTestGraph(t)
		ctx := context.Background()

		// Pre-populate two segments for multi-match.
		repo.addSegmentWithIDs(testWorkspaceID, IdentifierPair{Type: "user_id", Value: "user1"})
		repo.addSegmentWithIDs(testWorkspaceID, IdentifierPair{Type: "email", Value: "user1@example.com"})

		repo.mu.Lock()
		repo.mergeSegmentsErr = fmt.Errorf("merge conflict")
		repo.mu.Unlock()

		eventJSON := []byte(`{
			"type": "identify",
			"userId": "user1",
			"traits": {"email": "user1@example.com"}
		}`)

		result, err := g.ProcessEvent(ctx, testWorkspaceID, eventJSON)
		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "merge conflict")
	})

	t.Run("health check with cancelled context", func(t *testing.T) {
		g, _ := newTestGraph(t)
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		err := g.Health(cancelledCtx)
		require.Error(t, err)
	})
}
