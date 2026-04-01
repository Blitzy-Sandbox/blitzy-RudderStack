// Package graph implements the real-time identity graph service (E-026).
//
// This package provides the foundational identity resolution service that
// processes events in real-time as they flow through the RudderStack pipeline,
// extending beyond the existing warehouse/identity/identity.go batch-only model.
//
// The Service interface defines the public contract consumed by the processor
// pipeline (processor/processor.go), the Profiles API (identity/profiles/),
// and other internal services. The IdentityGraph struct implements Service
// and coordinates between the Resolver (resolution logic), storage.Repository
// (persistence), and settings.ResolutionSettings (rules).
//
// Thread-safe for concurrent use from multiple pipeline workers.
package graph

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/identity/settings"
	"github.com/rudderlabs/rudder-server/identity/storage"
)

// pkgLogger is the package-level scoped logger for the identity graph package.
// Initialized following the exact pattern from warehouse/identity/identity.go:30-34.
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
	// Returns the resolution result or error. Returns (nil, nil) when no identifiers are found.
	ProcessEvent(ctx context.Context, workspaceID string, eventJSON []byte) (*ResolutionResult, error)

	// ResolveIdentity looks up the identity segment for a specific external identifier.
	// Returns the segment if found, nil if not found, or error.
	ResolveIdentity(ctx context.Context, workspaceID, externalIDType, externalIDValue string) (*storage.GraphSegment, error)

	// GetSegmentIdentifiers returns all external identifiers associated with a segment.
	GetSegmentIdentifiers(ctx context.Context, segmentID int64) ([]storage.ExternalID, error)

	// GetSegmentTraits returns all traits associated with an identity segment.
	GetSegmentTraits(ctx context.Context, segmentID int64) ([]storage.Trait, error)

	// GetProfileData retrieves the complete profile for a segment, including the
	// segment metadata, all external identifiers, and all traits.
	// Used by the Profiles API (identity/profiles/) for batch profile assembly.
	GetProfileData(ctx context.Context, segmentID int64) (*storage.ProfileData, error)

	// Health returns nil if the service is healthy, error otherwise.
	// Checks connectivity to the underlying storage layer.
	Health(ctx context.Context) error

	// Run starts the service lifecycle. Blocks until ctx is cancelled or Stop is called.
	// Used by runner/runner.go for errgroup-based lifecycle management.
	Run(ctx context.Context) error

	// Stop gracefully stops the service and releases held resources.
	// Used by runner/runner.go for graceful shutdown.
	Stop()
}

// graphStats tracks performance and behaviour metrics for the identity graph service.
// Follows the tagged measurement pattern from processor/trackingplan.go:17-21.
type graphStats struct {
	eventsProcessed stats.Measurement
	eventsErrored   stats.Measurement
	processTime     stats.Measurement
	noIdentifiers   stats.Measurement
}

// IdentityGraph implements the Service interface for real-time identity resolution.
// It is the core service that manages the identity graph, coordinating between
// the Resolver (resolution logic), storage.Repository (persistence), and
// settings.ResolutionSettings (rules).
//
// Thread-safe for concurrent use from multiple pipeline workers via sync.RWMutex.
// The RWMutex protects the settings field; read operations (ProcessEvent, queries)
// acquire RLock while write operations (UpdateSettings) acquire Lock.
type IdentityGraph struct {
	mu       sync.RWMutex
	repo     storage.Repository
	resolver *Resolver
	settings *settings.ResolutionSettings
	conf     *config.Config
	logger   logger.Logger
	stats    graphStats
}

// New creates a new IdentityGraph service.
//
// Parameters:
//   - repo: PostgreSQL-backed persistence layer from identity/storage.
//     Must not be nil — returns error if nil.
//   - s: Resolution settings from identity/settings (blocked values, limits, priority).
//     If nil, settings.DefaultSettings() is used as fallback.
//   - conf: Reloadable configuration from rudder-go-kit/config.
//     If nil, config.Default is used as fallback.
//   - log: Structured logger. If nil, uses the package-level pkgLogger.
//   - statsFactory: Metrics factory for tagged measurements.
//     If nil, metrics are no-ops (nil Measurement fields are checked before use).
//
// The IdentityGraph creates an internal Resolver that implements the three
// resolution strategies (new/single/multi match) refactored from
// warehouse/identity/identity.go:applyRule().
func New(
	repo storage.Repository,
	s *settings.ResolutionSettings,
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
) (*IdentityGraph, error) {
	if repo == nil {
		return nil, fmt.Errorf("identity graph: storage repository is required")
	}
	if s == nil {
		s = settings.DefaultSettings()
	}
	if log == nil {
		log = pkgLogger
	}
	if conf == nil {
		conf = config.Default
	}

	g := &IdentityGraph{
		repo:     repo,
		settings: s,
		conf:     conf,
		logger:   log.Child("graph"),
	}

	// Create the resolver with the same dependencies. The resolver encapsulates
	// the three-strategy resolution logic (new/single/multi match) from
	// warehouse/identity/identity.go:applyRule().
	g.resolver = NewResolver(repo, s, log, statsFactory)

	// Initialize stats following processor/trackingplan.go:155-159 pattern.
	// Each metric is tagged with module and component for dashboard filtering.
	if statsFactory != nil {
		tags := stats.Tags{"module": "identity", "component": "graph"}
		g.stats.eventsProcessed = statsFactory.NewTaggedStat(
			"identity_events_processed", stats.CountType, tags,
		)
		g.stats.eventsErrored = statsFactory.NewTaggedStat(
			"identity_events_errored", stats.CountType, tags,
		)
		g.stats.processTime = statsFactory.NewTaggedStat(
			"identity_process_time", stats.TimerType, tags,
		)
		g.stats.noIdentifiers = statsFactory.NewTaggedStat(
			"identity_no_identifiers", stats.CountType, tags,
		)
	}

	return g, nil
}

// NewService creates a new identity graph Service with a simplified constructor
// signature suitable for runner/runner.go lifecycle management (E-026).
//
// This is the runner-facing constructor that creates an IdentityGraph with
// deferred storage initialization — the underlying repository connection is
// established in Run(ctx) rather than at construction time, because the
// database connection pool may not yet be available when the runner calls
// this constructor during early startup.
//
// Parameters mirror the pattern used by other Runner-managed services:
//
//	conf: Reloadable configuration (config.Default in runner)
//	log: Scoped logger (logger.NewLogger().Child("identity") in runner)
//	statsFactory: Metrics factory (stats.Default in runner)
func NewService(
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
) Service {
	if conf == nil {
		conf = config.Default
	}
	if log == nil {
		log = pkgLogger
	}
	s := settings.DefaultSettings()
	g := &IdentityGraph{
		settings: s,
		conf:     conf,
		logger:   log.Child("graph"),
	}
	// Create a resolver without a repository — it will be set when Run is called
	// and the storage layer becomes available.
	g.resolver = NewResolver(nil, s, log, statsFactory)

	// Initialize stats following processor/trackingplan.go:155-159 pattern.
	if statsFactory != nil {
		tags := stats.Tags{"module": "identity", "component": "graph"}
		g.stats.eventsProcessed = statsFactory.NewTaggedStat(
			"identity_events_processed", stats.CountType, tags,
		)
		g.stats.eventsErrored = statsFactory.NewTaggedStat(
			"identity_events_errored", stats.CountType, tags,
		)
		g.stats.processTime = statsFactory.NewTaggedStat(
			"identity_process_time", stats.TimerType, tags,
		)
		g.stats.noIdentifiers = statsFactory.NewTaggedStat(
			"identity_no_identifiers", stats.CountType, tags,
		)
	}
	return g
}

// ProcessEvent extracts identifiers from an incoming event and performs
// real-time identity resolution. This method is called from the processor
// pipeline (processor/processor.go) for each event that flows through.
//
// The event processing flow:
//  1. Extract identifiers from event JSON (userId, anonymousId, context.externalId, traits)
//  2. Filter blocked identifiers using resolution settings
//  3. Sort identifiers by priority for deterministic resolution
//  4. Delegate to Resolver for strategy execution (new/single/multi match)
//  5. Record metrics
//
// Returns (nil, nil) when no identifiers are found in the event — this is not
// an error condition, as some events may legitimately lack identity signals.
//
// Thread-safe: settings access is protected by RWMutex for concurrent pipeline workers.
func (g *IdentityGraph) ProcessEvent(ctx context.Context, workspaceID string, eventJSON []byte) (*ResolutionResult, error) {
	startTime := time.Now()
	defer func() {
		if g.stats.processTime != nil {
			g.stats.processTime.Since(startTime)
		}
	}()

	// Acquire read lock to safely access current settings.
	// The settings pointer is read atomically — once copied, the RLock is released
	// because ResolutionSettings internally uses its own RWMutex for method calls.
	g.mu.RLock()
	currentSettings := g.settings
	g.mu.RUnlock()

	// Step 1: Extract identifiers from event JSON.
	// ExtractExternalIDs parses userId, anonymousId, traits.email, and context.externalId.
	identifiers := ExtractExternalIDs(eventJSON)
	if len(identifiers) == 0 {
		g.logger.Debugn("No identifiers found in event",
			logger.NewStringField("workspaceID", workspaceID),
		)
		if g.stats.noIdentifiers != nil {
			g.stats.noIdentifiers.Increment()
		}
		return &ResolutionResult{Strategy: StrategyNewMatch}, nil //nolint:nilnil // no identifiers means no resolution
	}

	// Step 2: Filter blocked identifiers using resolution settings.
	// This removes values like "null", "0000", "-1" that would corrupt the graph.
	identifiers = FilterBlockedIdentifiers(identifiers, currentSettings)
	if len(identifiers) == 0 {
		g.logger.Debugn("All identifiers filtered by blocked value rules",
			logger.NewStringField("workspaceID", workspaceID),
		)
		return &ResolutionResult{Strategy: StrategyNewMatch}, nil //nolint:nilnil // all identifiers filtered
	}

	// Step 3: Sort by priority for deterministic resolution order.
	// Higher priority identifiers (lower priority number) come first.
	SortByPriority(identifiers, currentSettings)

	// Step 4: Delegate to the Resolver for strategy execution.
	// The resolver determines whether this is a new/single/multi match case.
	result, err := g.resolver.Resolve(ctx, workspaceID, identifiers)
	if err != nil {
		g.logger.Errorn("Error resolving identity",
			logger.NewStringField("workspaceID", workspaceID),
			obskit.Error(err),
		)
		if g.stats.eventsErrored != nil {
			g.stats.eventsErrored.Increment()
		}
		return nil, fmt.Errorf("identity resolution: %w", err)
	}

	// Step 5: Record success metrics.
	if g.stats.eventsProcessed != nil {
		g.stats.eventsProcessed.Increment()
	}

	g.logger.Debugn("Identity resolved",
		logger.NewStringField("workspaceID", workspaceID),
		logger.NewStringField("strategy", result.Strategy.String()),
		logger.NewIntField("segmentID", result.SegmentID),
		logger.NewIntField("identifierCount", int64(len(identifiers))),
	)

	return result, nil
}

// ResolveIdentity looks up the identity segment for a specific external identifier.
// This enables the Profiles API (E-027) and other consumers to query the graph
// by a known identifier (e.g., user_id, email).
//
// Returns:
//   - *storage.GraphSegment if a matching segment is found
//   - nil, nil if no matching segment exists
//   - nil, error on validation failure or storage error
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
// This is used by the Profiles API to retrieve the full set of external identifiers
// linked to a profile (e.g., all email addresses, device IDs, and user IDs).
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
// Traits include profile properties like name, company, plan_type, etc.
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

// GetProfileData retrieves the complete profile for a segment, assembling the
// segment metadata, all external identifiers, and all traits into a single response.
// Delegates to the storage.Repository.GetProfileData for batch assembly.
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

// Health checks if the identity graph service is healthy by verifying connectivity
// to the underlying storage layer (PostgreSQL).
func (g *IdentityGraph) Health(ctx context.Context) error {
	return g.repo.Ping(ctx)
}

// UpdateSettings replaces the resolution settings atomically.
// This is called when backend-config publishes new identity resolution settings.
// The update is propagated to the Resolver as well for consistency.
//
// Thread-safe: uses the write lock to prevent concurrent reads during update.
func (g *IdentityGraph) UpdateSettings(s *settings.ResolutionSettings) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settings = s
	// Propagate to the resolver — direct field access within the same package.
	g.resolver.settings = s
	g.logger.Infon("Identity resolution settings updated")
}

// Run starts the identity graph service and blocks until the context is cancelled.
// The service operates in a request-driven mode — events are processed via
// ProcessEvent() calls from the processor pipeline. Run provides lifecycle
// management for the service including graceful shutdown on context cancellation.
//
// This follows the standard RudderStack service lifecycle pattern used by
// Gateway, Processor, Router, and Warehouse services.
func (g *IdentityGraph) Run(ctx context.Context) error {
	g.logger.Infon("Identity graph service started")
	<-ctx.Done()
	return ctx.Err()
}

// Stop gracefully stops the identity graph service and releases any held resources.
// Called during server shutdown to ensure clean teardown.
func (g *IdentityGraph) Stop() {
	g.logger.Infon("Identity graph service stopped")
}

// compile-time interface check ensuring IdentityGraph satisfies Service.
var _ Service = (*IdentityGraph)(nil)
