// Package selectivesync provides per-table and per-column sync filtering
// for warehouse destinations, allowing users to include or exclude specific
// tables and columns from warehouse sync operations.
//
// The SelectiveSyncService evaluates table/column inclusion against the sync
// configuration. It is the primary entry point for the selective sync feature
// (E-034). Configuration is loaded from the database via a repository layer
// and cached in memory with a configurable TTL for fast predicate evaluation.
package selectivesync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// SelectiveSyncRepository defines the persistence interface for selective sync
// configurations. It is implemented by the concrete Repository type (repository.go)
// and can be mocked in tests.
//
// All methods accept a context.Context for deadline propagation and cancellation.
type SelectiveSyncRepository interface {
	// Upsert inserts or updates a selective sync configuration for a
	// source/destination pair. The unique constraint on (source_id, destination_id)
	// ensures atomic upsert semantics.
	Upsert(ctx context.Context, cfg SelectiveSyncConfig) error

	// Get retrieves the selective sync configuration for a source/destination pair.
	// Returns ErrSelectiveSyncNotFound if no configuration exists.
	Get(ctx context.Context, sourceID, destID string) (*SelectiveSyncConfig, error)

	// Delete removes the selective sync configuration for a source/destination pair.
	// The operation is idempotent — deleting a non-existent config does not error.
	Delete(ctx context.Context, sourceID, destID string) error

	// ListByWorkspace retrieves all selective sync configurations for a workspace.
	// Returns an empty slice (not nil) if no configurations exist.
	ListByWorkspace(ctx context.Context, workspaceID string) ([]SelectiveSyncConfig, error)
}

// SelectiveSyncService evaluates table/column inclusion against selective sync
// configuration. It caches configurations with a configurable TTL to minimize
// database queries during the hot path of load file generation and schema
// consolidation.
//
// The service is thread-safe and all methods can be called concurrently from
// multiple goroutines.
//
// Default behavior when the feature is disabled (Warehouse.selectiveSync.enabled=false):
// all predicates return false (include everything), preserving backward compatibility.
//
// Default behavior on error (config lookup failure): predicates return false
// (fail open, not fail closed), ensuring that a transient database error never
// silently drops tables or columns from sync.
type SelectiveSyncService struct {
	log  logger.Logger
	repo SelectiveSyncRepository

	config struct {
		enabled             config.ValueLoader[bool]
		cacheRefreshMinutes config.ValueLoader[int]
	}

	// cacheMu protects the cache map for concurrent read/write access.
	// Read operations (getConfig cache lookup) acquire RLock.
	// Write operations (cache update, invalidation) acquire Lock.
	cacheMu sync.RWMutex
	// cache maps "sourceID:destID" to a cached configuration entry with expiry.
	cache map[string]*cachedConfig
}

// cachedConfig holds a selective sync configuration together with its cache
// expiry time. When time.Now() is after expiresAt, the entry is considered stale
// and will be refreshed from the repository on the next access.
type cachedConfig struct {
	config    *SelectiveSyncConfig
	expiresAt time.Time
}

// NewSelectiveSyncService creates a new SelectiveSyncService with the given
// dependencies.
//
// Configuration keys:
//   - Warehouse.selectiveSync.enabled (bool, default false)
//   - Warehouse.selectiveSync.cacheRefreshMinutes (int, default 5, min 1)
//
// The constructor follows the config pattern established in
// warehouse/archive/archiver.go and warehouse/schema/schema.go.
func NewSelectiveSyncService(
	conf *config.Config,
	log logger.Logger,
	repo SelectiveSyncRepository,
) *SelectiveSyncService {
	s := &SelectiveSyncService{
		log:   log.Child("selectivesync"),
		repo:  repo,
		cache: make(map[string]*cachedConfig),
	}
	s.config.enabled = conf.GetReloadableBoolVar(
		DefaultEnabled,
		ConfigKeyEnabled,
	)
	s.config.cacheRefreshMinutes = conf.GetReloadableIntVar(
		DefaultCacheRefreshMinutes, 1,
		ConfigKeyCacheRefreshMinutes,
	)
	return s
}

// cacheKey builds the map key used to index cached configurations.
// Format: "sourceID:destID"
func cacheKey(sourceID, destID string) string {
	return sourceID + ":" + destID
}

// getConfig retrieves the selective sync configuration for a source/destination
// pair. It first checks the in-memory cache under a read lock; on a cache miss
// or expiry, it fetches from the repository and updates the cache under a write
// lock.
//
// The cache TTL is controlled by Warehouse.selectiveSync.cacheRefreshMinutes
// and is re-evaluated on every Load() call, enabling runtime reconfiguration
// without restart.
func (s *SelectiveSyncService) getConfig(ctx context.Context, sourceID, destID string) (*SelectiveSyncConfig, error) {
	key := cacheKey(sourceID, destID)

	// Fast path: check cache under read lock.
	s.cacheMu.RLock()
	if cached, ok := s.cache[key]; ok && time.Now().Before(cached.expiresAt) {
		s.cacheMu.RUnlock()
		return cached.config, nil
	}
	s.cacheMu.RUnlock()

	// Slow path: cache miss or expired — fetch from repository.
	cfg, err := s.repo.Get(ctx, sourceID, destID)
	if err != nil {
		return nil, err
	}

	// Update cache under write lock.
	ttl := time.Duration(s.config.cacheRefreshMinutes.Load()) * time.Minute
	s.cacheMu.Lock()
	s.cache[key] = &cachedConfig{
		config:    cfg,
		expiresAt: time.Now().Add(ttl),
	}
	s.cacheMu.Unlock()

	return cfg, nil
}

// IsTableExcluded returns true if the given table is excluded from sync for the
// specified source/destination pair.
//
// Returns false (include table) in any of the following cases:
//   - Selective sync feature is disabled (Warehouse.selectiveSync.enabled=false)
//   - No configuration exists for the source/destination pair
//   - Any error occurs during configuration lookup (fail open)
//   - The table is not in the excluded tables list
//
// This method is thread-safe and can be called concurrently. Callers should
// pass a request or upload context to enable deadline propagation and
// cancellation during the repository lookup on cache miss.
func (s *SelectiveSyncService) IsTableExcluded(ctx context.Context, sourceID, destID, table string) bool {
	if !s.config.enabled.Load() {
		return false
	}

	cfg, err := s.getConfig(ctx, sourceID, destID)
	if err != nil {
		// Fail open: on error, default to including the table.
		// Log at debug level to avoid log spam on every predicate call.
		s.log.Warnn("selective sync config lookup failed, defaulting to include",
			obskit.Error(err),
			logger.NewStringField("sourceID", sourceID),
			logger.NewStringField("destID", destID),
			logger.NewStringField("table", table),
		)
		return false
	}

	for _, excluded := range cfg.ExcludedTables {
		if excluded == table {
			return true
		}
	}
	return false
}

// IsColumnExcluded returns true if the given column in the given table is excluded
// from sync for the specified source/destination pair.
//
// Returns false (include column) in any of the following cases:
//   - Selective sync feature is disabled
//   - No configuration exists for the source/destination pair
//   - Any error occurs during configuration lookup (fail open)
//   - The table has no column-level exclusions
//   - The column is not in the excluded columns list for the table
//
// This method is thread-safe and can be called concurrently. Callers should
// pass a request or upload context to enable deadline propagation and
// cancellation during the repository lookup on cache miss.
func (s *SelectiveSyncService) IsColumnExcluded(ctx context.Context, sourceID, destID, table, column string) bool {
	if !s.config.enabled.Load() {
		return false
	}

	cfg, err := s.getConfig(ctx, sourceID, destID)
	if err != nil {
		// Fail open: on error, default to including the column.
		s.log.Warnn("selective sync config lookup failed, defaulting to include",
			obskit.Error(err),
			logger.NewStringField("sourceID", sourceID),
			logger.NewStringField("destID", destID),
			logger.NewStringField("table", table),
			logger.NewStringField("column", column),
		)
		return false
	}

	excludedCols, ok := cfg.ExcludedColumns[table]
	if !ok {
		return false
	}
	for _, excluded := range excludedCols {
		if excluded == column {
			return true
		}
	}
	return false
}

// GetConfig retrieves the selective sync configuration for a source/destination
// pair. This is a public API method intended for use by the HTTP handler.
//
// Returns ErrSelectiveSyncDisabled if the feature is disabled.
// Returns ErrSelectiveSyncNotFound (propagated from the repository) if no
// configuration exists.
//
// Implements the SelectiveSyncConfigurer interface (defined in handler.go).
func (s *SelectiveSyncService) GetConfig(ctx context.Context, sourceID, destID string) (*SelectiveSyncConfig, error) {
	if !s.config.enabled.Load() {
		return nil, ErrSelectiveSyncDisabled
	}
	return s.getConfig(ctx, sourceID, destID)
}

// UpdateConfig updates (or creates) the selective sync configuration for a
// source/destination pair. This is a public API method intended for use by the
// HTTP handler.
//
// Validation:
//   - Returns ErrSelectiveSyncDisabled if the feature is disabled
//   - Returns an error if SourceID is empty
//   - Returns an error if DestinationID is empty
//
// On successful upsert, the cache entry for the source/destination pair is
// invalidated to ensure subsequent predicate calls pick up the new configuration
// (either from the repository on next access, or from a new cache entry).
//
// Implements the SelectiveSyncConfigurer interface (defined in handler.go).
func (s *SelectiveSyncService) UpdateConfig(ctx context.Context, req SelectiveSyncRequest) (*SelectiveSyncResponse, error) {
	if !s.config.enabled.Load() {
		return nil, ErrSelectiveSyncDisabled
	}

	if req.SourceID == "" {
		return nil, ErrMissingSourceID
	}
	if req.DestinationID == "" {
		return nil, ErrMissingDestinationID
	}

	cfg := SelectiveSyncConfig{
		SourceID:        req.SourceID,
		DestinationID:   req.DestinationID,
		WorkspaceID:     req.WorkspaceID,
		ExcludedTables:  req.ExcludedTables,
		ExcludedColumns: req.ExcludedColumns,
	}

	if err := s.repo.Upsert(ctx, cfg); err != nil {
		return nil, fmt.Errorf("upserting selective sync config: %w", err)
	}

	// Invalidate the cache for this source/destination pair so the next
	// predicate call fetches the updated configuration.
	key := cacheKey(req.SourceID, req.DestinationID)
	s.cacheMu.Lock()
	delete(s.cache, key)
	s.cacheMu.Unlock()

	return &SelectiveSyncResponse{
		Status:   "updated",
		SourceID: req.SourceID,
		DestID:   req.DestinationID,
	}, nil
}

// InvalidateCache removes a specific source/destination config entry from the
// in-memory cache. This method is called by the backend-config manager when
// configuration updates arrive via the pub/sub subscription, ensuring that
// stale cache entries are evicted promptly.
//
// Thread-safe: acquires a write lock on the cache mutex.
func (s *SelectiveSyncService) InvalidateCache(sourceID, destID string) {
	key := cacheKey(sourceID, destID)
	s.cacheMu.Lock()
	delete(s.cache, key)
	s.cacheMu.Unlock()
}

// InvalidateAllCache clears the entire in-memory configuration cache.
// This is useful during full config refreshes or when the backend-config
// manager reports a complete configuration reload.
//
// Thread-safe: acquires a write lock on the cache mutex.
func (s *SelectiveSyncService) InvalidateAllCache() {
	s.cacheMu.Lock()
	s.cache = make(map[string]*cachedConfig)
	s.cacheMu.Unlock()
}
