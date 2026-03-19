// Package replay provides the warehouse replay feature (E-035) that enables
// re-processing of archived events through the warehouse pipeline.
// This file defines configuration keys, default values, and a reloadable
// configuration struct for all replay-related settings.
package replay

import (
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
)

// Configuration key constants define the paths used to look up replay settings
// in the rudder-go-kit config system. These follow the Warehouse.<featureName>.<paramName>
// convention established throughout the codebase (see warehouse/archive/archiver.go).
const (
	// ConfigKeyEnabled is the configuration key for enabling/disabling warehouse replay.
	// When false (the default), all replay operations are rejected with ErrReplayDisabled.
	ConfigKeyEnabled = "Warehouse.replay.enabled"

	// ConfigKeyMaxConcurrentReplays is the configuration key for the maximum number
	// of concurrent replay jobs that can run simultaneously.
	ConfigKeyMaxConcurrentReplays = "Warehouse.replay.maxConcurrentReplays"

	// ConfigKeyBatchSize is the configuration key for the number of events per batch
	// sent to the Gateway replay endpoint during replay processing.
	ConfigKeyBatchSize = "Warehouse.replay.batchSize"

	// ConfigKeyTimeoutMinutes is the configuration key for the maximum duration
	// (in minutes) allowed for a single replay job before it is considered timed out.
	ConfigKeyTimeoutMinutes = "Warehouse.replay.timeoutMinutes"

	// ConfigKeyPruneIntervalSeconds is the configuration key for how frequently
	// (in seconds) terminal replay jobs are pruned from the in-memory job store.
	ConfigKeyPruneIntervalSeconds = "Warehouse.replay.pruneIntervalSeconds"

	// ConfigKeyPruneRetentionMinutes is the configuration key for how long
	// (in minutes) terminal replay jobs are retained in the in-memory store
	// before being eligible for pruning.
	ConfigKeyPruneRetentionMinutes = "Warehouse.replay.pruneRetentionMinutes"
)

// Default value constants provide safe fallback values for all replay configuration
// parameters. These defaults maintain backward compatibility — replay is disabled
// by default and does nothing when configuration keys are unset.
const (
	// DefaultEnabled is the default value for the replay enabled flag.
	// Set to false for backward compatibility: when unset, replay is inactive
	// and all replay API requests are rejected gracefully.
	DefaultEnabled = false

	// DefaultMaxConcurrentReplays is the default maximum number of concurrent
	// replay jobs. Limits resource consumption from parallel replay operations.
	DefaultMaxConcurrentReplays = 2

	// DefaultBatchSize is the default number of events per replay batch sent
	// to the Gateway. Balances throughput with memory usage per batch.
	DefaultBatchSize = 1000

	// DefaultTimeoutMinutes is the default replay job timeout in minutes.
	// A value of 60 translates to 1 hour (60 * time.Minute) when used with
	// GetReloadableDurationVar's time.Minute timescale.
	DefaultTimeoutMinutes = 60

	// DefaultPruneIntervalSeconds is the default interval between pruning
	// cycles for terminal replay jobs. Set to 300 seconds (5 minutes) to
	// balance memory cleanup with minimal CPU overhead.
	DefaultPruneIntervalSeconds = 300

	// DefaultPruneRetentionMinutes is the default retention period for terminal
	// replay jobs. Jobs in Completed or Failed state older than this duration
	// are removed from the in-memory store. Set to 60 minutes (1 hour) to
	// give clients sufficient time to query final job status.
	DefaultPruneRetentionMinutes = 60
)

// Config holds all replay configuration values using reloadable loaders.
// These are initialized from the rudder-go-kit config system via LoadConfig,
// enabling runtime reconfiguration without process restart.
//
// Each field is a config.ValueLoader[T] that supports hot-reloading: callers
// invoke .Load() at the point of use to retrieve the current value, ensuring
// configuration changes propagate without requiring service restarts.
//
// Pattern reference: warehouse/archive/archiver.go lines 62-70.
type Config struct {
	// Enabled controls whether warehouse replay is active. When false,
	// all replay trigger requests are rejected with ErrReplayDisabled.
	Enabled config.ValueLoader[bool]

	// MaxConcurrentReplays limits the number of simultaneous replay jobs
	// that can execute at any given time. Requests exceeding this limit
	// are rejected with ErrConcurrentLimitReached.
	MaxConcurrentReplays config.ValueLoader[int]

	// BatchSize controls how many archived events are grouped into a single
	// batch when sent to the Gateway replay endpoint. Larger values improve
	// throughput but increase per-batch memory consumption.
	BatchSize config.ValueLoader[int]

	// TimeoutMinutes sets the maximum duration for a single replay job.
	// Jobs exceeding this timeout are cancelled via context cancellation
	// and marked as failed. The value is loaded as a time.Duration with
	// minute-level granularity.
	TimeoutMinutes config.ValueLoader[time.Duration]

	// PruneInterval controls how frequently the background pruning loop
	// removes terminal (Completed/Failed) replay jobs from the in-memory
	// job store. Loaded as a time.Duration with second-level granularity.
	PruneInterval config.ValueLoader[time.Duration]

	// PruneRetention controls how long terminal replay jobs are kept in
	// the in-memory store before becoming eligible for pruning. Jobs older
	// than this duration are removed during the next prune cycle.
	// Loaded as a time.Duration with minute-level granularity.
	PruneRetention config.ValueLoader[time.Duration]
}

// LoadConfig initializes all replay configuration from the provided config instance.
// It uses reloadable value loaders that pick up runtime changes without restart.
//
// The function follows the configuration loading pattern established in
// warehouse/archive/archiver.go (lines 92-98), using:
//   - GetReloadableBoolVar for boolean flags
//   - GetReloadableIntVar for integer values (with valueScale=1 for pass-through)
//   - GetReloadableDurationVar for duration values (with time.Minute timescale)
//
// All returned ValueLoader fields support hot-reloading: configuration changes
// take effect on the next .Load() call without requiring a process restart.
func LoadConfig(conf *config.Config) Config {
	return Config{
		Enabled: conf.GetReloadableBoolVar(
			DefaultEnabled,
			ConfigKeyEnabled,
		),
		MaxConcurrentReplays: conf.GetReloadableIntVar(
			DefaultMaxConcurrentReplays, 1,
			ConfigKeyMaxConcurrentReplays,
		),
		BatchSize: conf.GetReloadableIntVar(
			DefaultBatchSize, 1,
			ConfigKeyBatchSize,
		),
		TimeoutMinutes: conf.GetReloadableDurationVar(
			DefaultTimeoutMinutes, time.Minute,
			ConfigKeyTimeoutMinutes,
		),
		PruneInterval: conf.GetReloadableDurationVar(
			DefaultPruneIntervalSeconds, time.Second,
			ConfigKeyPruneIntervalSeconds,
		),
		PruneRetention: conf.GetReloadableDurationVar(
			DefaultPruneRetentionMinutes, time.Minute,
			ConfigKeyPruneRetentionMinutes,
		),
	}
}
