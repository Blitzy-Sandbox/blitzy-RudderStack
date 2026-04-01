// Package graph implements the real-time identity graph service (E-026).
//
// This package provides the foundational identity resolution service that
// processes events in real-time (not batch-only during warehouse uploads),
// extending beyond the existing warehouse/identity/identity.go batch model.
//
// The Service interface defines the public contract consumed by the processor
// pipeline, the Profiles API (identity/profiles/), and other internal services.
package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/identity/storage"
)

var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("identity").Child("graph")
}

// Service defines the public contract for the real-time identity graph.
// It is designed to be integrated into the processor pipeline for real-time
// identity resolution as events flow through the system.
//
// Unlike warehouse/identity/identity.go which only resolves during batch
// warehouse uploads, this service processes identity in real-time.
type Service interface {
	// ProcessEvent extracts identifiers from an event and resolves identity in real-time.
	// This is called from the processor pipeline (processor/processor.go) for each event.
	// Returns the resolution result or error.
	ProcessEvent(ctx context.Context, workspaceID string, eventJSON []byte) (*ResolutionResult, error)

	// ResolveIdentity looks up the identity segment for a specific external identifier.
	// Returns the segment if found, nil if not found, or error.
	ResolveIdentity(ctx context.Context, workspaceID, externalIDType, externalIDValue string) (*storage.GraphSegment, error)

	// GetSegmentIdentifiers returns all external identifiers associated with a segment.
	GetSegmentIdentifiers(ctx context.Context, segmentID int64) ([]storage.ExternalID, error)

	// GetSegmentTraits returns all traits associated with an identity segment.
	GetSegmentTraits(ctx context.Context, segmentID int64) ([]storage.Trait, error)

	// GetProfileData returns the full profile data (segment, external IDs, traits)
	// for a given segment ID. Used by the Profiles API for efficient profile retrieval.
	GetProfileData(ctx context.Context, segmentID int64) (*storage.ProfileData, error)

	// Health returns nil if the service is healthy, error otherwise.
	Health(ctx context.Context) error
}

// ResolutionStrategy identifies which resolution path was taken.
type ResolutionStrategy int

const (
	// StrategyNewMatch indicates no existing identity was found for any identifier.
	// A new segment is created with a fresh UUID.
	StrategyNewMatch ResolutionStrategy = iota

	// StrategySingleMatch indicates exactly one existing identity was found.
	// New identifiers are added to the existing segment.
	StrategySingleMatch

	// StrategyMultiMatch indicates multiple existing identities were found.
	// Segments are merged using the first rudder_id.
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
	// Strategy indicates which resolution path was taken
	Strategy ResolutionStrategy

	// SegmentID is the ID of the resulting segment after resolution
	SegmentID int64

	// MatchedSegments contains the IDs of all segments matched before merge
	MatchedSegments []int64

	// NewIdentifiers contains identifiers that were newly added to the graph
	NewIdentifiers []IdentifierPair

	// MergedSegmentIDs contains segments that were merged and removed (multi-match only)
	MergedSegmentIDs []int64
}

// IdentifierPair represents a single external identifier association.
// Type is the identifier type (e.g., "user_id", "email", "ios.idfa")
// and Value is the identifier value.
type IdentifierPair struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// graphStats tracks performance and behaviour metrics for the identity graph.
type graphStats struct {
	eventsProcessed stats.Measurement
	eventsErrored   stats.Measurement
	processTime     stats.Measurement
	noIdentifiers   stats.Measurement
}

// IdentityGraph implements the Service interface for real-time identity resolution.
// It is the core service that manages the identity graph, coordinating between
// the storage.Repository (persistence) and resolution settings.
//
// Thread-safe for concurrent use from multiple pipeline workers.
type IdentityGraph struct {
	repo   storage.Repository
	conf   *config.Config
	logger logger.Logger
	stats  graphStats
}

// New creates a new IdentityGraph service.
//
// Parameters:
//   - repo: PostgreSQL-backed persistence layer from identity/storage/
//   - conf: Reloadable configuration from rudder-go-kit/config
//   - log: Structured logger (if nil, uses package default)
//   - statsFactory: Metrics factory for tagged measurements
func New(repo storage.Repository, conf *config.Config, log logger.Logger, statsFactory stats.Stats) (*IdentityGraph, error) {
	if repo == nil {
		return nil, fmt.Errorf("identity graph: storage repository is required")
	}
	if log == nil {
		log = pkgLogger
	}
	if conf == nil {
		conf = config.Default
	}

	g := &IdentityGraph{
		repo:   repo,
		conf:   conf,
		logger: log.Child("graph"),
	}

	// Initialize stats following processor/trackingplan.go pattern
	if statsFactory != nil {
		tags := stats.Tags{"module": "identity", "component": "graph"}
		g.stats.eventsProcessed = statsFactory.NewTaggedStat("identity_events_processed", stats.CountType, tags)
		g.stats.eventsErrored = statsFactory.NewTaggedStat("identity_events_errored", stats.CountType, tags)
		g.stats.processTime = statsFactory.NewTaggedStat("identity_process_time", stats.TimerType, tags)
		g.stats.noIdentifiers = statsFactory.NewTaggedStat("identity_no_identifiers", stats.CountType, tags)
	}

	return g, nil
}

// ProcessEvent extracts identifiers from an incoming event and performs
// real-time identity resolution. This is a placeholder for the full
// implementation which will be provided by the resolver module.
func (g *IdentityGraph) ProcessEvent(ctx context.Context, workspaceID string, eventJSON []byte) (*ResolutionResult, error) {
	startTime := time.Now()
	defer func() {
		if g.stats.processTime != nil {
			g.stats.processTime.Since(startTime)
		}
	}()

	if g.stats.eventsProcessed != nil {
		g.stats.eventsProcessed.Increment()
	}

	g.logger.Debugn("ProcessEvent called",
		logger.NewStringField("workspaceID", workspaceID),
	)

	return &ResolutionResult{Strategy: StrategyNewMatch}, nil
}

// ResolveIdentity looks up the identity segment for a specific external identifier.
func (g *IdentityGraph) ResolveIdentity(ctx context.Context, workspaceID, externalIDType, externalIDValue string) (*storage.GraphSegment, error) {
	if workspaceID == "" || externalIDType == "" || externalIDValue == "" {
		return nil, fmt.Errorf("identity graph: workspaceID, externalIDType, and externalIDValue are required")
	}
	segment, err := g.repo.LookupByExternalID(ctx, workspaceID, externalIDType, externalIDValue)
	if err != nil {
		g.logger.Errorn("Error looking up identity",
			logger.NewStringField("workspaceID", workspaceID),
			logger.NewStringField("externalIDType", externalIDType),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("identity lookup: %w", err)
	}
	return segment, nil
}

// GetSegmentIdentifiers returns all external identifiers associated with an identity segment.
func (g *IdentityGraph) GetSegmentIdentifiers(ctx context.Context, segmentID int64) ([]storage.ExternalID, error) {
	ids, err := g.repo.GetExternalIDsBySegment(ctx, segmentID)
	if err != nil {
		g.logger.Errorn("Error getting segment identifiers",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("get segment identifiers: %w", err)
	}
	return ids, nil
}

// GetSegmentTraits returns all traits (key-value attributes) for an identity segment.
func (g *IdentityGraph) GetSegmentTraits(ctx context.Context, segmentID int64) ([]storage.Trait, error) {
	traits, err := g.repo.GetTraits(ctx, segmentID)
	if err != nil {
		g.logger.Errorn("Error getting segment traits",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("get segment traits: %w", err)
	}
	return traits, nil
}

// GetProfileData returns the full profile data (segment, external IDs, traits)
// for a given segment ID. Used by the Profiles API for efficient profile retrieval.
func (g *IdentityGraph) GetProfileData(ctx context.Context, segmentID int64) (*storage.ProfileData, error) {
	profile, err := g.repo.GetProfileData(ctx, segmentID)
	if err != nil {
		g.logger.Errorn("Error getting profile data",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("get profile data: %w", err)
	}
	return profile, nil
}

// Health checks if the identity graph service is healthy by pinging the database.
func (g *IdentityGraph) Health(ctx context.Context) error {
	return g.repo.Ping(ctx)
}

// compile-time interface check
var _ Service = (*IdentityGraph)(nil)
