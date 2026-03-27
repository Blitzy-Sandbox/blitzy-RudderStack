// Package selectivesync provides per-table and per-column sync filtering
// for warehouse destinations, allowing users to include or exclude specific
// tables and columns from warehouse sync operations.
package selectivesync

import (
	"github.com/rudderlabs/rudder-go-kit/config"
)

// Configuration key paths for all selective-sync-related settings.
// These keys are nested under "Warehouse.selectiveSync.*" following
// the established convention in the warehouse configuration namespace
// (see also warehouse/backfill/config.go and warehouse/archive/archiver.go).
const (
	// ConfigKeyEnabled is the configuration key for enabling/disabling selective sync.
	// When disabled, all tables and columns are included in the warehouse sync
	// (preserving pre-existing behavior).
	// Default: false (selective sync is disabled by default for backward compatibility)
	ConfigKeyEnabled = "Warehouse.selectiveSync.enabled"

	// ConfigKeyCacheRefreshMinutes is the configuration key for the interval
	// (in minutes) at which the selective sync configuration cache is refreshed
	// from the database. A shorter interval means configuration changes take
	// effect sooner; a longer interval reduces database load.
	// Default: 5 minutes
	ConfigKeyCacheRefreshMinutes = "Warehouse.selectiveSync.cacheRefreshMinutes"
)

// Default values for selective sync configuration parameters.
// These defaults ensure backward compatibility: when no configuration is
// provided, selective sync remains disabled and all tables and columns
// are synced as before.
const (
	// DefaultEnabled is the default value for the selective sync enabled flag.
	// Set to false to ensure backward compatibility — existing deployments
	// continue to sync all tables and columns until an operator explicitly
	// enables selective sync.
	DefaultEnabled = false

	// DefaultCacheRefreshMinutes is the default cache refresh interval in minutes.
	// A 5-minute interval balances responsiveness to configuration changes
	// against database query overhead.
	DefaultCacheRefreshMinutes = 5
)

// Config holds all selective sync configuration values using reloadable loaders.
// These are initialized from the rudder-go-kit config system via LoadConfig,
// enabling runtime reconfiguration without process restart. This follows
// the same reloadable pattern used in warehouse/archive/archiver.go (lines 62-70)
// and warehouse/backfill/config.go.
type Config struct {
	// Enabled controls whether selective sync filtering is active.
	// When false, all tables and columns are included in sync (default behavior).
	// When true, the selective sync service evaluates per-table and per-column
	// exclusion rules before generating load files and exporting data.
	Enabled config.ValueLoader[bool]

	// CacheRefreshMinutes controls how often (in minutes) selective sync
	// configurations are refreshed from the database. Cached configurations
	// are used for table/column exclusion decisions during load file generation
	// and schema consolidation.
	CacheRefreshMinutes config.ValueLoader[int]
}

// LoadConfig initializes all selective sync configuration from the provided
// config instance. It uses the reloadable variable pattern (GetReloadableBoolVar,
// GetReloadableIntVar) so that configuration changes take effect at the next
// Load() call without requiring a process restart.
//
// This follows the pattern established in warehouse/archive/archiver.go
// New() function (lines 92-98) and warehouse/backfill/config.go LoadConfig().
func LoadConfig(conf *config.Config) Config {
	return Config{
		Enabled: conf.GetReloadableBoolVar(
			DefaultEnabled,
			ConfigKeyEnabled,
		),
		CacheRefreshMinutes: conf.GetReloadableIntVar(
			DefaultCacheRefreshMinutes, 1,
			ConfigKeyCacheRefreshMinutes,
		),
	}
}
