// Package graph — resolver.go implements the identity resolution engine (E-026).
//
// This file implements the three resolution strategies (new match, single match,
// multi match) refactored from warehouse/identity/identity.go:applyRule() for
// reuse in the real-time identity graph context. The Resolver is consumed by
// the IdentityGraph service in graph.go during real-time event processing.
package graph

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/identity/settings"
	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ResolutionStrategy identifies which resolution path was taken.
type ResolutionStrategy int

const (
	// StrategyNewMatch indicates no existing identity was found for any identifier.
	// A new segment is created. Corresponds to warehouse/identity/identity.go:112
	// (len(rudderIDs) == 0).
	StrategyNewMatch ResolutionStrategy = iota

	// StrategySingleMatch indicates exactly one existing identity was found.
	// New identifiers are added to the existing segment. Corresponds to
	// warehouse/identity/identity.go:114-116 (len(rudderIDs) == 1).
	StrategySingleMatch

	// StrategyMultiMatch indicates multiple existing identities were found.
	// Segments are merged using the lowest segment ID as the target. Corresponds to
	// warehouse/identity/identity.go:137-195 (len(rudderIDs) > 1).
	StrategyMultiMatch
)

// String returns the human-readable name of the resolution strategy.
func (s ResolutionStrategy) String() string {
	switch s {
	case StrategyNewMatch:
		return "new_match"
	case StrategySingleMatch:
		return "single_match"
	case StrategyMultiMatch:
		return "multi_match"
	default:
		return "unknown"
	}
}

// ResolutionResult contains the outcome of an identity resolution operation.
type ResolutionResult struct {
	// Strategy indicates which resolution path was taken.
	Strategy ResolutionStrategy

	// SegmentID is the ID of the resulting segment after resolution.
	SegmentID int64

	// MatchedSegments contains the IDs of all segments matched before merge.
	MatchedSegments []int64

	// NewIdentifiers contains identifiers that were newly added to the graph.
	NewIdentifiers []IdentifierPair

	// MergedSegmentIDs contains segments that were merged and removed (multi-match only).
	MergedSegmentIDs []int64
}

// resolverStats tracks metrics for the resolver operations.
// Follows the tagged measurement pattern from processor/trackingplan.go:17-21.
type resolverStats struct {
	newMatchCount    stats.Measurement
	singleMatchCount stats.Measurement
	multiMatchCount  stats.Measurement
	resolveTime      stats.Measurement
	mergeTime        stats.Measurement
	blockedIDCount   stats.Measurement
	limitExceeded    stats.Measurement
}

// Resolver implements identity resolution logic for the real-time identity graph.
// It determines how incoming identifiers from events map to existing segments
// and executes the appropriate merge/creation strategy.
//
// The resolution logic is refactored from warehouse/identity/identity.go:applyRule()
// to work in a real-time, event-driven context rather than batch warehouse uploads.
type Resolver struct {
	repo     storage.Repository
	settings *settings.ResolutionSettings
	logger   logger.Logger
	stats    resolverStats
}

// NewResolver creates a new identity Resolver.
// The repo parameter provides persistence, settings provides resolution rules,
// log provides structured logging, and statsFactory provides metrics.
func NewResolver(
	repo storage.Repository,
	s *settings.ResolutionSettings,
	log logger.Logger,
	statsFactory stats.Stats,
) *Resolver {
	if log == nil {
		log = pkgLogger
	}
	r := &Resolver{
		repo:     repo,
		settings: s,
		logger:   log.Child("resolver"),
	}
	// Initialize stats following the pattern from processor/trackingplan.go:155-159.
	if statsFactory != nil {
		tags := stats.Tags{"module": "identity", "component": "resolver"}
		r.stats.newMatchCount = statsFactory.NewTaggedStat(
			"identity_resolution_new_match", stats.CountType, tags,
		)
		r.stats.singleMatchCount = statsFactory.NewTaggedStat(
			"identity_resolution_single_match", stats.CountType, tags,
		)
		r.stats.multiMatchCount = statsFactory.NewTaggedStat(
			"identity_resolution_multi_match", stats.CountType, tags,
		)
		r.stats.resolveTime = statsFactory.NewTaggedStat(
			"identity_resolution_time", stats.TimerType, tags,
		)
		r.stats.mergeTime = statsFactory.NewTaggedStat(
			"identity_merge_time", stats.TimerType, tags,
		)
		r.stats.blockedIDCount = statsFactory.NewTaggedStat(
			"identity_blocked_id_count", stats.CountType, tags,
		)
		r.stats.limitExceeded = statsFactory.NewTaggedStat(
			"identity_limit_exceeded_count", stats.CountType, tags,
		)
	}
	return r
}

// UpdateSettings replaces the resolution settings on the Resolver.
// Called from IdentityGraph.UpdateSettings() to propagate settings changes.
func (r *Resolver) UpdateSettings(s *settings.ResolutionSettings) {
	r.settings = s
}

// Resolve performs identity resolution for a set of identifier pairs from an event.
// It determines which resolution strategy applies and executes it atomically.
//
// The three resolution strategies mirror warehouse/identity/identity.go:applyRule():
//   - New match (len(matchedSegments) == 0): Create new segment
//   - Single match (len(matchedSegments) == 1): Add new identifiers to existing segment
//   - Multi match (len(matchedSegments) > 1): Merge all matched segments
func (r *Resolver) Resolve(ctx context.Context, workspaceID string, identifiers []IdentifierPair) (*ResolutionResult, error) {
	startTime := time.Now()
	defer func() {
		if r.stats.resolveTime != nil {
			r.stats.resolveTime.Since(startTime)
		}
	}()

	// Step 1: Filter blocked identifiers using settings.
	validIdentifiers := r.filterBlockedIdentifiers(identifiers)

	// Step 2: Deduplicate identifier pairs.
	validIdentifiers = deduplicateIdentifiers(validIdentifiers)

	// Step 3: Validate — at least one identifier must remain after filtering.
	if len(validIdentifiers) == 0 {
		return nil, fmt.Errorf("no valid identifiers after filtering")
	}

	// Step 4: Lookup — find which segments match any of the provided identifiers.
	matchedSegmentIDs, err := r.findMatchingSegments(ctx, workspaceID, validIdentifiers)
	if err != nil {
		return nil, fmt.Errorf("finding matching segments: %w", err)
	}

	// Step 5: Sort matched segments by ID ascending for deterministic merge ordering.
	// This ensures the "first" segment is always the lowest ID, consistent with
	// warehouse/identity/identity.go:139 (newID := rudderIDs[0]).
	sort.Slice(matchedSegmentIDs, func(i, j int) bool {
		return matchedSegmentIDs[i] < matchedSegmentIDs[j]
	})

	// Step 6: Dispatch to the appropriate strategy.
	var result *ResolutionResult
	switch len(matchedSegmentIDs) {
	case 0:
		result, err = r.newMatch(ctx, workspaceID, validIdentifiers)
		if err != nil {
			return nil, fmt.Errorf("new match: %w", err)
		}
		if r.stats.newMatchCount != nil {
			r.stats.newMatchCount.Increment()
		}
	case 1:
		result, err = r.singleMatch(ctx, matchedSegmentIDs[0], validIdentifiers)
		if err != nil {
			return nil, fmt.Errorf("single match: %w", err)
		}
		if r.stats.singleMatchCount != nil {
			r.stats.singleMatchCount.Increment()
		}
	default:
		result, err = r.multiMatch(ctx, matchedSegmentIDs, validIdentifiers)
		if err != nil {
			return nil, fmt.Errorf("multi match: %w", err)
		}
		if r.stats.multiMatchCount != nil {
			r.stats.multiMatchCount.Increment()
		}
	}

	r.logger.Infon("Identity resolved",
		logger.NewStringField("workspaceID", workspaceID),
		logger.NewStringField("strategy", result.Strategy.String()),
		logger.NewIntField("segmentID", result.SegmentID),
		logger.NewIntField("matchedSegmentCount", int64(len(matchedSegmentIDs))),
	)

	return result, nil
}

// newMatch creates a new identity segment when no existing identity is found.
// Mirrors warehouse/identity/identity.go:112-113 (rudderID = misc.FastUUID().String()).
func (r *Resolver) newMatch(ctx context.Context, workspaceID string, identifiers []IdentifierPair) (*ResolutionResult, error) {
	// Create a new segment via the repository — the repo generates the UUID internally.
	// CreateSegment returns the segment's primary key ID (int64).
	segmentID, err := r.repo.CreateSegment(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("creating segment: %w", err)
	}

	newIdentifiers := make([]IdentifierPair, 0, len(identifiers))

	// Track identifiers added within this invocation so that per-type limit
	// checks account for all additions — not just what exists in the DB.
	// For a brand-new segment the DB starts empty, so this local counter is
	// the sole source of truth for the limit check.
	addedPerType := make(map[string]int, len(identifiers))

	// Add each identifier as an external ID, respecting per-type limits.
	for _, id := range identifiers {
		// Count = identifiers of this type already added in this invocation.
		typeCount := addedPerType[id.Type]
		// ExceedsLimit uses count >= MaxCount semantics, so pass the current count:
		// if we're already at or over the limit, don't add more.
		if r.settings != nil && r.settings.ExceedsLimit(id.Type, typeCount) {
			r.logger.Debugn("Identifier limit exceeded, skipping",
				logger.NewStringField("identifierType", id.Type),
				logger.NewIntField("currentCount", int64(typeCount)),
			)
			if r.stats.limitExceeded != nil {
				r.stats.limitExceeded.Increment()
			}
			continue
		}

		// AddExternalID takes a storage.ExternalID struct per the Repository interface.
		_, addErr := r.repo.AddExternalID(ctx, storage.ExternalID{
			GraphID:         segmentID,
			WorkspaceID:     workspaceID,
			ExternalIDType:  id.Type,
			ExternalIDValue: id.Value,
			CreatedSource:   "event",
		})
		if addErr != nil {
			r.logger.Errorn("Error adding external ID to new segment",
				logger.NewStringField("identifierType", id.Type),
				obskit.Error(addErr),
			)
			continue
		}
		addedPerType[id.Type]++
		newIdentifiers = append(newIdentifiers, id)
	}

	return &ResolutionResult{
		Strategy:       StrategyNewMatch,
		SegmentID:      segmentID,
		NewIdentifiers: newIdentifiers,
	}, nil
}

// singleMatch adds new identifiers to an existing matched segment.
// Mirrors warehouse/identity/identity.go:114-116 (rudderID = rudderIDs[0]).
func (r *Resolver) singleMatch(ctx context.Context, segmentID int64, identifiers []IdentifierPair) (*ResolutionResult, error) {
	// Get existing external IDs for the segment to avoid duplicates.
	existingIDs, err := r.repo.GetExternalIDsBySegment(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("getting existing identifiers: %w", err)
	}

	// Build a set of existing {type:value} pairs for O(1) lookup.
	existingSet := make(map[string]struct{}, len(existingIDs))
	for _, eid := range existingIDs {
		existingSet[eid.ExternalIDType+":"+eid.ExternalIDValue] = struct{}{}
	}

	newIdentifiers := make([]IdentifierPair, 0)

	// Track identifiers added within this invocation so that per-type limit
	// checks account for all additions during this Resolve() call — not just
	// the count from the initial DB snapshot.
	addedPerType := make(map[string]int, len(identifiers))

	// Only add identifiers that don't already exist in the segment.
	for _, id := range identifiers {
		key := id.Type + ":" + id.Value
		if _, exists := existingSet[key]; exists {
			continue // Already associated with this segment.
		}

		// Check limits: DB count + count added within this invocation.
		typeCount := countIDsOfType(existingIDs, id.Type) + addedPerType[id.Type]
		if r.settings != nil && r.settings.ExceedsLimit(id.Type, typeCount) {
			r.logger.Debugn("Identifier limit exceeded, skipping",
				logger.NewStringField("identifierType", id.Type),
				logger.NewIntField("currentCount", int64(typeCount)),
			)
			if r.stats.limitExceeded != nil {
				r.stats.limitExceeded.Increment()
			}
			continue
		}

		// AddExternalID takes a storage.ExternalID struct per the Repository interface.
		// WorkspaceID is left empty for singleMatch since the segment already has
		// workspace-scoped data; the repository implementation can derive it.
		_, addErr := r.repo.AddExternalID(ctx, storage.ExternalID{
			GraphID:         segmentID,
			ExternalIDType:  id.Type,
			ExternalIDValue: id.Value,
			CreatedSource:   "event",
		})
		if addErr != nil {
			r.logger.Errorn("Error adding external ID to existing segment",
				logger.NewStringField("identifierType", id.Type),
				obskit.Error(addErr),
			)
			continue
		}
		addedPerType[id.Type]++
		newIdentifiers = append(newIdentifiers, id)
	}

	return &ResolutionResult{
		Strategy:        StrategySingleMatch,
		SegmentID:       segmentID,
		MatchedSegments: []int64{segmentID},
		NewIdentifiers:  newIdentifiers,
	}, nil
}

// multiMatch merges multiple matched segments, keeping the lowest segment ID.
// Mirrors warehouse/identity/identity.go:137-195:
//   - newID := rudderIDs[0] (keep first/lowest)
//   - Update all other rudderIDs to newID
//   - Insert mapping rows for the merge properties
func (r *Resolver) multiMatch(ctx context.Context, matchedSegmentIDs []int64, identifiers []IdentifierPair) (*ResolutionResult, error) {
	mergeStart := time.Now()

	// Sort segment IDs to ensure deterministic "first" (lowest ID wins).
	sort.Slice(matchedSegmentIDs, func(i, j int) bool {
		return matchedSegmentIDs[i] < matchedSegmentIDs[j]
	})

	// Target segment = lowest ID (mirrors identity.go:139 newID := rudderIDs[0]).
	targetSegmentID := matchedSegmentIDs[0]
	sourceSegmentIDs := matchedSegmentIDs[1:]

	// Merge all source segments into the target atomically.
	// MergeSegments moves all external IDs and traits from source to target.
	if err := r.repo.MergeSegments(ctx, targetSegmentID, sourceSegmentIDs); err != nil {
		return nil, fmt.Errorf("merging segments: %w", err)
	}

	if r.stats.mergeTime != nil {
		r.stats.mergeTime.Since(mergeStart)
	}

	// After merge, add any new event identifiers not yet in the merged segment.
	existingIDs, err := r.repo.GetExternalIDsBySegment(ctx, targetSegmentID)
	if err != nil {
		r.logger.Errorn("Error getting identifiers after merge",
			logger.NewIntField("segmentID", targetSegmentID),
			obskit.Error(err),
		)
		// Non-fatal — the merge succeeded, just couldn't add new identifiers.
		return &ResolutionResult{
			Strategy:         StrategyMultiMatch,
			SegmentID:        targetSegmentID,
			MatchedSegments:  matchedSegmentIDs,
			MergedSegmentIDs: sourceSegmentIDs,
		}, nil
	}

	existingSet := make(map[string]struct{}, len(existingIDs))
	for _, eid := range existingIDs {
		existingSet[eid.ExternalIDType+":"+eid.ExternalIDValue] = struct{}{}
	}

	newIdentifiers := make([]IdentifierPair, 0)
	// Track identifiers added within this invocation for accurate limit checks.
	addedPerType := make(map[string]int, len(identifiers))
	for _, id := range identifiers {
		key := id.Type + ":" + id.Value
		if _, exists := existingSet[key]; exists {
			continue
		}

		// DB count + count added within this invocation.
		typeCount := countIDsOfType(existingIDs, id.Type) + addedPerType[id.Type]
		if r.settings != nil && r.settings.ExceedsLimit(id.Type, typeCount) {
			if r.stats.limitExceeded != nil {
				r.stats.limitExceeded.Increment()
			}
			continue
		}

		_, addErr := r.repo.AddExternalID(ctx, storage.ExternalID{
			GraphID:         targetSegmentID,
			ExternalIDType:  id.Type,
			ExternalIDValue: id.Value,
			CreatedSource:   "event",
		})
		if addErr != nil {
			r.logger.Errorn("Error adding external ID after merge",
				logger.NewStringField("identifierType", id.Type),
				obskit.Error(addErr),
			)
			continue
		}
		addedPerType[id.Type]++
		newIdentifiers = append(newIdentifiers, id)
	}

	return &ResolutionResult{
		Strategy:         StrategyMultiMatch,
		SegmentID:        targetSegmentID,
		MatchedSegments:  matchedSegmentIDs,
		NewIdentifiers:   newIdentifiers,
		MergedSegmentIDs: sourceSegmentIDs,
	}, nil
}

// findMatchingSegments looks up which segments match any of the provided identifiers.
// Returns a deduplicated, sorted slice of segment IDs.
func (r *Resolver) findMatchingSegments(ctx context.Context, workspaceID string, identifiers []IdentifierPair) ([]int64, error) {
	segmentIDSet := make(map[int64]struct{})
	for _, id := range identifiers {
		segment, err := r.repo.LookupByExternalID(ctx, workspaceID, id.Type, id.Value)
		if err != nil {
			r.logger.Errorn("Error looking up external ID",
				logger.NewStringField("identifierType", id.Type),
				obskit.Error(err),
			)
			return nil, fmt.Errorf("lookup for %s: %w", id.Type, err)
		}
		if segment != nil {
			segmentIDSet[segment.ID] = struct{}{}
		}
	}

	result := make([]int64, 0, len(segmentIDSet))
	for id := range segmentIDSet {
		result = append(result, id)
	}

	// Sort for deterministic ordering.
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})

	return result, nil
}

// filterBlockedIdentifiers removes identifiers whose values are blocked by settings.
func (r *Resolver) filterBlockedIdentifiers(identifiers []IdentifierPair) []IdentifierPair {
	if r.settings == nil {
		return identifiers
	}
	result := make([]IdentifierPair, 0, len(identifiers))
	for _, id := range identifiers {
		if r.settings.IsBlocked(id.Type, id.Value) {
			r.logger.Debugn("Identifier blocked by settings",
				logger.NewStringField("identifierType", id.Type),
			)
			if r.stats.blockedIDCount != nil {
				r.stats.blockedIDCount.Increment()
			}
			continue
		}
		result = append(result, id)
	}
	return result
}

// deduplicateIdentifiers removes duplicate {type, value} pairs.
// Preserves original order — first occurrence is kept.
func deduplicateIdentifiers(identifiers []IdentifierPair) []IdentifierPair {
	seen := make(map[string]struct{}, len(identifiers))
	result := make([]IdentifierPair, 0, len(identifiers))
	for _, id := range identifiers {
		key := id.Key()
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, id)
		}
	}
	return result
}

// countIDsOfType counts how many external IDs of a given type exist in a slice.
func countIDsOfType(ids []storage.ExternalID, idType string) int {
	count := 0
	for _, id := range ids {
		if id.ExternalIDType == idType {
			count++
		}
	}
	return count
}
