// Package backfill provides configurable warehouse backfill functionality,
// allowing historical data sync for a specified date range, source, and
// warehouse destination.
package backfill

import (
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
)

// Configuration key paths for all backfill-related settings.
// These keys are nested under "Warehouse.backfill.*" following the
// established convention in the warehouse configuration namespace.
const (
	// ConfigKeyEnabled is the configuration key for enabling/disabling
	// the backfill feature. When disabled, backfill API requests are
	// rejected and the background monitor does not run.
	// Default: false (backfill is disabled by default for backward compatibility)
	ConfigKeyEnabled = "Warehouse.backfill.enabled"

	// ConfigKeyMaxDateRangeDays is the configuration key for the maximum
	// number of days allowed in a single backfill date range request.
	// Requests exceeding this range are rejected with a validation error.
	// Default: 90 days
	ConfigKeyMaxDateRangeDays = "Warehouse.backfill.maxDateRangeDays"

	// ConfigKeyMaxConcurrentJobs is the configuration key for the maximum
	// number of backfill jobs that can run concurrently across all sources
	// and destinations. Additional requests are queued until a slot opens.
	// Default: 3
	ConfigKeyMaxConcurrentJobs = "Warehouse.backfill.maxConcurrentJobs"

	// ConfigKeyMonitorInterval is the configuration key for the interval
	// (in seconds) between consecutive background monitor iterations.
	// The monitor checks for stalled backfill jobs and updates status.
	// Default: 60 seconds
	ConfigKeyMonitorInterval = "Warehouse.backfill.monitorIntervalSeconds"
)

// Default values for backfill configuration parameters.
// These defaults ensure backward compatibility: when no configuration is
// provided, the backfill feature remains disabled and all operational
// parameters use conservative values.
const (
	// DefaultEnabled is the default value for the backfill enabled flag.
	// Set to false to ensure backward compatibility — existing deployments
	// are unaffected until the operator explicitly enables backfill.
	DefaultEnabled = false

	// DefaultMaxDateRangeDays is the default maximum date range in days
	// that a single backfill request may span. 90 days balances utility
	// with resource consumption.
	DefaultMaxDateRangeDays = 90

	// DefaultMaxConcurrentJobs is the default maximum number of backfill
	// jobs that may execute concurrently. Limiting concurrency prevents
	// backfill operations from overwhelming the warehouse or the staging
	// file storage backend.
	DefaultMaxConcurrentJobs = 3

	// DefaultMonitorIntervalSeconds is the default interval in seconds
	// between background monitor check iterations. The monitor detects
	// stalled or timed-out backfill jobs and transitions them to a
	// terminal state.
	DefaultMonitorIntervalSeconds = 60

	// ArchiverRetentionWindowDays is the archiver's default retention
	// window in days. Backfill requests targeting dates within this
	// window resolve data from the archiver's stored events. Requests
	// for dates beyond this window fall back to staging files stored
	// in object storage.
	ArchiverRetentionWindowDays = 10
)

// Config holds all backfill configuration values using reloadable loaders.
// Fields are initialized from the rudder-go-kit config system via LoadConfig,
// enabling runtime reconfiguration without process restart. This follows the
// same reloadable pattern used in warehouse/archive/archiver.go.
type Config struct {
	// Enabled controls whether the backfill feature is active. When false,
	// the backfill HTTP/gRPC endpoints return a 503 Service Unavailable
	// response and the background monitor goroutine is not started.
	Enabled config.ValueLoader[bool]

	// MaxDateRangeDays is the upper bound on the number of days a single
	// backfill request may span. The backfill handler validates that
	// (endDate - startDate) <= MaxDateRangeDays before accepting a request.
	MaxDateRangeDays config.ValueLoader[int]

	// MaxConcurrentJobs limits how many backfill jobs may execute in
	// parallel. The backfill service checks the count of active jobs
	// before starting a new one, queuing excess requests.
	MaxConcurrentJobs config.ValueLoader[int]

	// MonitorInterval is the time duration between consecutive iterations
	// of the background monitor loop that checks for stalled or timed-out
	// backfill jobs.
	MonitorInterval config.ValueLoader[time.Duration]
}

// LoadConfig initializes all backfill configuration from the provided config
// instance. It uses the reloadable variable pattern (GetReloadableBoolVar,
// GetReloadableIntVar, GetReloadableDurationVar) so that configuration
// changes take effect at the next Load() call without requiring a restart.
//
// This follows the pattern established in warehouse/archive/archiver.go
// New() function (lines 92-98).
func LoadConfig(conf *config.Config) Config {
	return Config{
		Enabled: conf.GetReloadableBoolVar(
			DefaultEnabled,
			ConfigKeyEnabled,
		),
		MaxDateRangeDays: conf.GetReloadableIntVar(
			DefaultMaxDateRangeDays, 1,
			ConfigKeyMaxDateRangeDays,
		),
		MaxConcurrentJobs: conf.GetReloadableIntVar(
			DefaultMaxConcurrentJobs, 1,
			ConfigKeyMaxConcurrentJobs,
		),
		MonitorInterval: conf.GetReloadableDurationVar(
			DefaultMonitorIntervalSeconds, time.Second,
			ConfigKeyMonitorInterval,
		),
	}
}
