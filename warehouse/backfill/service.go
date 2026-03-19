// Package backfill provides the warehouse backfill service orchestrator (E-032).
//
// The BackfillService validates backfill requests, creates wh_backfill_jobs records,
// resolves archived staging files from the archiver (within its 10-day retention window)
// or from staging file storage (for data beyond the retention window), and transitions
// jobs through their lifecycle (pending → in_progress → completed/failed).
//
// Architecture:
//   - BackfillService.Trigger()  — API entry point: validates request, creates pending job
//   - BackfillService.Run()      — Background monitor loop (context-cancellable ticker)
//   - BackfillService.GetStatus() — Retrieves current job state by ID
//
// The service follows the patterns established by warehouse/archive/archiver.go (struct
// layout, reloadable config) and warehouse/archive/cron.go (background monitor loop).
//
// JSON serialization across all backfill package files uses
// github.com/rudderlabs/rudder-go-kit/jsonrs exclusively, as mandated by the project's
// .golangci.yml depguard rule. encoding/json must never be imported directly.
package backfill

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// ---------------------------------------------------------------------------
// Interface Definitions — Dependency Injection Contracts
// ---------------------------------------------------------------------------

// BackfillRepository defines the data access contract for backfill job persistence.
// The concrete implementation lives in repository.go (Repository struct). This interface
// enables dependency injection and mock-based testing of the BackfillService.
//
// All methods accept context.Context as the first parameter, supporting deadline
// propagation, cancellation, and graceful shutdown per the warehouse convention.
type BackfillRepository interface {
	// Create inserts a new backfill job and returns its auto-generated ID.
	// The job's status is always set to StatusPending by the repository,
	// regardless of what the caller provides, ensuring consistent state
	// machine entry.
	Create(ctx context.Context, job BackfillJob) (int64, error)

	// Get retrieves a backfill job by its primary key ID.
	// Returns ErrBackfillJobNotFound if no job exists with the given ID.
	Get(ctx context.Context, id int64) (BackfillJob, error)

	// UpdateStatus transitions a backfill job to the specified status
	// and updates the updated_at timestamp.
	// Returns ErrBackfillJobNotFound if no job exists with the given ID.
	UpdateStatus(ctx context.Context, id int64, status string) error

	// ListBySource returns all backfill jobs for a given source ID,
	// ordered by created_at descending (most recent first).
	// Returns an empty (non-nil) slice if no jobs exist for the source.
	ListBySource(ctx context.Context, sourceID string) ([]BackfillJob, error)

	// GetActiveCount returns the number of backfill jobs that are currently
	// in an active (non-terminal) state: StatusPending or StatusInProgress.
	// Used by the service to enforce concurrency limits.
	GetActiveCount(ctx context.Context) (int64, error)

	// CreateIfUnderLimit atomically checks that the number of active
	// (Pending or InProgress) backfill jobs is below maxConcurrent and
	// inserts a new job only if the limit is not reached. This prevents
	// the TOCTOU race condition inherent in separate GetActiveCount + Create calls.
	// Returns (0, ErrConcurrentLimitReached) if the limit is already met.
	CreateIfUnderLimit(ctx context.Context, job BackfillJob, maxConcurrent int) (int64, error)
}

// ArchiverQuerier defines the interface for querying archived staging files
// from the warehouse archiver. The archiver stores staging files for a
// configurable retention window (default 10 days per ArchiverRetentionWindowDays
// in config.go).
//
// Backfill requests targeting dates within this window resolve data from
// the archiver, which provides faster access to recently archived events.
type ArchiverQuerier interface {
	// ListArchivedStagingFiles returns the IDs of staging files archived
	// for the given source-destination pair within the specified date range.
	// Returns an empty slice (not nil) if no archived files exist for the range.
	ListArchivedStagingFiles(ctx context.Context, sourceID, destID string, startDate, endDate time.Time) ([]int64, error)
}

// StagingFileQuerier defines the interface for querying staging files from
// the staging file repository (object storage). This is the fallback path
// for backfill requests that target dates beyond the archiver's retention window.
type StagingFileQuerier interface {
	// GetByDateRange returns the IDs of staging files for the given
	// source-destination pair within the specified date range.
	// Returns an empty slice (not nil) if no staging files exist for the range.
	GetByDateRange(ctx context.Context, sourceID, destID string, startDate, endDate time.Time) ([]int64, error)
}

// ---------------------------------------------------------------------------
// BackfillService — Core Orchestrator
// ---------------------------------------------------------------------------

// BackfillService is the central orchestrator for warehouse backfill operations.
// It manages the complete lifecycle of backfill jobs:
//
//  1. Trigger — validates the request, checks concurrency limits, creates the job record
//  2. Monitor — periodically checks tracked jobs, resolves staging files for pending jobs,
//     and detects completed or failed jobs
//  3. Status — retrieves the current state of any backfill job
//
// The service follows the context-cancellable background loop pattern established by
// warehouse/archive/cron.go (CronArchiver). Configuration uses reloadable variables
// via the Config struct (config.go) so that settings take effect without restart.
//
// BackfillService satisfies the BackfillTrigger interface defined in handler.go,
// enabling the HTTP handler to delegate business logic through a decoupled interface.
type BackfillService struct {
	// conf is the raw configuration instance, retained for potential future
	// configuration needs beyond what the reloadable Config provides.
	conf *config.Config

	// log is the structured logger scoped to the "backfill" component.
	// Created via log.Child("backfill") in the constructor, following
	// the logger hierarchy pattern from warehouse/archive/archiver.go.
	log logger.Logger

	// statsFactory provides Prometheus-compatible metric creation following
	// the warehouse/router/upload_stats.go instrumentation pattern.
	statsFactory stats.Stats

	// repo is the data access layer for backfill job CRUD operations.
	// Injected via constructor for testability and decoupling from the
	// concrete Repository implementation in repository.go.
	repo BackfillRepository

	// archiver queries archived staging files within the retention window.
	// Used by processPendingJob to resolve data sources for recent backfills.
	archiver ArchiverQuerier

	// stagingRepo queries staging files from object storage for date ranges
	// beyond the archiver's retention window. Used by processPendingJob as
	// the fallback data source path.
	stagingRepo StagingFileQuerier

	// cfg holds all reloadable configuration values loaded via LoadConfig
	// (config.go). Changes to configuration keys at runtime take effect at
	// the next Load() call without requiring a process restart.
	cfg Config

	// Prometheus-compatible metric counters for backfill operations.
	// Initialized in NewBackfillService using statsFactory.NewStat().
	// stats.Measurement (returned by NewStat) satisfies stats.Counter.
	backfillTriggered stats.Counter
	backfillFailed    stats.Counter
	backfillCompleted stats.Counter

	// now is an injectable clock function for testability.
	// Defaults to time.Now in the constructor; can be overridden for tests.
	now func() time.Time

	// mu protects concurrent access to trackedJobs from the Trigger method
	// (API goroutine) and the monitorJobs method (background goroutine).
	mu sync.Mutex

	// trackedJobs holds the set of backfill job IDs created by this service
	// instance that require monitoring. Jobs are added by Trigger() and
	// removed by handleTerminalJob() when they reach a terminal state
	// (StatusCompleted or StatusFailed). Using a map for O(1) add/remove.
	trackedJobs map[int64]struct{}
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

// NewBackfillService creates a new BackfillService with all dependencies injected.
//
// The constructor follows the pattern established in warehouse/archive/archiver.go New():
//   - Derives a child logger for structured component-scoped logging
//   - Initializes reloadable configuration via LoadConfig (config.go)
//   - Creates Prometheus-compatible metric counters for operational visibility
//   - Initializes the tracked jobs map for background monitoring
//
// Parameters:
//   - conf: Config instance for loading reloadable configuration values
//   - log: Parent logger; a child logger scoped to "backfill" is derived
//   - statsFactory: Stats instance for creating Prometheus-compatible metrics
//   - repo: Repository for backfill job CRUD operations
//   - archiver: Querier for archived staging files (within retention window)
//   - stagingRepo: Querier for staging files in object storage (beyond retention)
func NewBackfillService(
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
	repo BackfillRepository,
	archiver ArchiverQuerier,
	stagingRepo StagingFileQuerier,
) *BackfillService {
	s := &BackfillService{
		conf:         conf,
		log:          log.Child("backfill"),
		statsFactory: statsFactory,
		repo:         repo,
		archiver:     archiver,
		stagingRepo:  stagingRepo,
		cfg:          LoadConfig(conf),
		now:          time.Now,
		trackedJobs:  make(map[int64]struct{}),
	}

	// Initialize Prometheus-compatible metric counters following the
	// statsFactory.NewStat() pattern from warehouse/router/upload_stats.go.
	// stats.NewStat returns stats.Measurement which satisfies stats.Counter.
	s.backfillTriggered = statsFactory.NewStat("warehouse.backfill.triggered", stats.CountType)
	s.backfillFailed = statsFactory.NewStat("warehouse.backfill.failed", stats.CountType)
	s.backfillCompleted = statsFactory.NewStat("warehouse.backfill.completed", stats.CountType)

	return s
}

// ---------------------------------------------------------------------------
// Public API Methods (satisfy BackfillTrigger interface from handler.go)
// ---------------------------------------------------------------------------

// Trigger initiates a new backfill job. It performs multi-stage validation before
// creating the job record in the repository:
//
//  1. Feature gate — verifies backfill is enabled via Warehouse.backfill.enabled
//  2. Input validation — checks sourceID, destinationID, and date range validity
//  3. Concurrency check — ensures the active job count is below maxConcurrentJobs
//  4. Job creation — persists the job with StatusPending via the repository
//  5. Tracking — adds the job ID to the monitored set for background processing
//
// On success, returns a BackfillResponse containing the new job ID and StatusPending.
// On failure, returns a typed sentinel error that the HTTP handler maps to an
// appropriate HTTP status code (see handler.go handleServiceError):
//   - ErrBackfillDisabled       → 403 Forbidden
//   - ErrMissingSourceID        → 400 Bad Request (caller typically validates first)
//   - ErrMissingDestinationID   → 400 Bad Request
//   - ErrInvalidDateRange       → 400 Bad Request
//   - ErrDateRangeExceedsMax    → 400 Bad Request
//   - ErrConcurrentLimitReached → 429 Too Many Requests
//
// This method satisfies the BackfillTrigger.Trigger contract defined in handler.go.
func (s *BackfillService) Trigger(ctx context.Context, req BackfillRequest) (*BackfillResponse, error) {
	// Step 1: Feature gate — reject early if backfill is disabled.
	// This is the default state (DefaultEnabled = false in config.go)
	// to ensure backward compatibility with existing deployments.
	if !s.cfg.Enabled.Load() {
		return nil, ErrBackfillDisabled
	}

	// Step 2: Input validation — sourceID, destinationID, date range.
	if req.SourceID == "" {
		return nil, ErrMissingSourceID
	}
	if req.DestinationID == "" {
		return nil, ErrMissingDestinationID
	}
	if err := validateDateRange(req.StartDate, req.EndDate, s.cfg.MaxDateRangeDays.Load()); err != nil {
		return nil, err
	}

	// Step 3+4 (atomic): Create the backfill job with an atomic concurrency
	// limit check. CreateIfUnderLimit uses a single SQL CTE that counts active
	// jobs and inserts only if the count is below the configured maximum.
	// This eliminates the TOCTOU race condition that existed when GetActiveCount
	// and Create were separate operations — concurrent requests can no longer
	// all pass the count check before any of them persist.
	job := BackfillJob{
		SourceID:      req.SourceID,
		DestinationID: req.DestinationID,
		WorkspaceID:   req.WorkspaceID,
		StartDate:     req.StartDate,
		EndDate:       req.EndDate,
		Status:        StatusPending,
	}
	id, err := s.repo.CreateIfUnderLimit(ctx, job, s.cfg.MaxConcurrentJobs.Load())
	if err != nil {
		s.backfillFailed.Increment()
		return nil, fmt.Errorf("creating backfill job: %w", err)
	}

	// Step 5: Emit metrics and track the job for background monitoring.
	s.backfillTriggered.Increment()
	s.trackJob(id)

	s.log.Infon("backfill job created",
		logger.NewIntField("jobID", id),
		obskit.SourceID(req.SourceID),
		obskit.DestinationID(req.DestinationID),
	)

	return &BackfillResponse{JobID: id, Status: StatusPending}, nil
}

// GetStatus retrieves the current state of a backfill job identified by jobID.
// Returns the full BackfillJob record including all metadata and timestamps,
// or ErrBackfillJobNotFound if no such job exists.
//
// This method satisfies the BackfillTrigger.GetStatus contract defined in handler.go.
func (s *BackfillService) GetStatus(ctx context.Context, jobID int64) (*BackfillJob, error) {
	job, err := s.repo.Get(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// ---------------------------------------------------------------------------
// Background Monitor — Run Loop
// ---------------------------------------------------------------------------

// Run starts the background monitor loop that periodically checks tracked backfill
// jobs for status changes. It follows the context-cancellable ticker pattern
// established by warehouse/archive/cron.go (CronArchiver).
//
// The monitor runs at the interval specified by Warehouse.backfill.monitorIntervalSeconds
// (default 60s, reloadable via cfg.MonitorInterval). On each tick, it calls monitorJobs
// to process all tracked jobs.
//
// Run blocks until the context is cancelled. When the backfill feature is disabled,
// Run returns immediately without starting the monitor loop, ensuring zero resource
// consumption for disabled features.
//
// Typical usage from warehouse/app.go:
//
//	go backfillService.Run(ctx)
func (s *BackfillService) Run(ctx context.Context) error {
	s.log.Infon("starting backfill background monitor")

	for {
		select {
		case <-ctx.Done():
			s.log.Infon("context cancelled, stopping backfill monitor")
			return nil
		case <-time.After(s.cfg.MonitorInterval.Load()):
			// Check enabled flag per-tick so that runtime config toggling
			// (via reloadable config) takes effect without restarting the monitor.
			if !s.cfg.Enabled.Load() {
				continue
			}
			s.monitorJobs(ctx)
		}
	}
}

// ---------------------------------------------------------------------------
// Internal Monitor Methods
// ---------------------------------------------------------------------------

// monitorJobs iterates over all tracked backfill jobs and performs lifecycle management:
//
//   - For terminal jobs (StatusCompleted/StatusFailed): emit the appropriate metric
//     counter and remove the job from tracking.
//   - For pending jobs (StatusPending): attempt to resolve staging files from the
//     archiver or staging repository, then transition the job to StatusInProgress.
//   - For in-progress jobs (StatusInProgress): log their status for operational
//     visibility. (Upload pipeline status checking is handled by the upload state machine.)
//
// This method is called by the Run loop at the configured monitor interval.
// It respects context cancellation for graceful shutdown between job iterations.
func (s *BackfillService) monitorJobs(ctx context.Context) {
	// Snapshot tracked job IDs under the lock to minimize lock hold time.
	// This prevents the lock from being held during potentially slow
	// repository queries.
	s.mu.Lock()
	jobIDs := make([]int64, 0, len(s.trackedJobs))
	for id := range s.trackedJobs {
		jobIDs = append(jobIDs, id)
	}
	s.mu.Unlock()

	if len(jobIDs) == 0 {
		return
	}

	s.log.Infon("monitoring backfill jobs",
		logger.NewIntField("trackedJobCount", int64(len(jobIDs))),
	)

	for _, id := range jobIDs {
		// Respect context cancellation between iterations for graceful shutdown.
		if ctx.Err() != nil {
			return
		}

		job, err := s.repo.Get(ctx, id)
		if err != nil {
			s.log.Errorn("failed to get backfill job during monitoring",
				obskit.Error(err),
				logger.NewIntField("jobID", id),
			)
			continue
		}

		switch {
		case IsTerminal(job.Status):
			// Job has reached a terminal state (completed or failed).
			// Emit the appropriate metric and remove from tracking.
			s.handleTerminalJob(id, job.Status)

		case job.Status == StatusPending:
			// Job is pending — attempt to resolve staging files and
			// transition to in_progress.
			s.processPendingJob(ctx, job)

		case job.Status == StatusInProgress:
			// Job is in progress — log for operational visibility.
			// The actual upload pipeline manages the upload lifecycle;
			// the backfill monitor provides observability.
			s.log.Infon("backfill job in progress",
				logger.NewIntField("jobID", id),
				obskit.SourceID(job.SourceID),
				obskit.DestinationID(job.DestinationID),
			)
		}
	}
}

// handleTerminalJob processes a backfill job that has reached a terminal state.
// It emits the appropriate metric counter (completed or failed), logs the terminal
// event for operational visibility, and removes the job from the tracked set so
// subsequent monitor iterations skip it.
func (s *BackfillService) handleTerminalJob(jobID int64, status BackfillStatus) {
	if status == StatusCompleted {
		s.backfillCompleted.Increment()
		s.log.Infon("backfill job completed",
			logger.NewIntField("jobID", jobID),
		)
	} else {
		s.backfillFailed.Increment()
		s.log.Infon("backfill job reached terminal failure state",
			logger.NewIntField("jobID", jobID),
		)
	}
	s.untrackJob(jobID)
}

// processPendingJob resolves staging files for a pending backfill job and
// transitions it to StatusInProgress if files are found. The resolution strategy
// depends on the date range relative to the archiver's retention window
// (ArchiverRetentionWindowDays, default 10 days, defined in config.go):
//
//   - Within retention: queries the ArchiverQuerier for recently archived
//     staging files, providing faster access to recent data.
//   - Beyond retention: falls back to the StagingFileQuerier which reads
//     from object storage for historical data.
//
// If no staging files are found for the date range or the query fails,
// the job is marked as StatusFailed via failJob.
func (s *BackfillService) processPendingJob(ctx context.Context, job BackfillJob) {
	// Determine the archiver retention cutoff. Dates after this cutoff
	// are within the archiver's retention window and can be resolved
	// from the archiver's stored events.
	retentionCutoff := s.now().AddDate(0, 0, -ArchiverRetentionWindowDays)

	var fileIDs []int64
	var err error

	if job.StartDate.After(retentionCutoff) {
		// Within archiver retention window — resolve from archiver.
		fileIDs, err = s.archiver.ListArchivedStagingFiles(
			ctx, job.SourceID, job.DestinationID, job.StartDate, job.EndDate,
		)
		if err != nil {
			s.log.Errorn("failed to query archiver for staging files",
				obskit.Error(err),
				logger.NewIntField("jobID", job.ID),
				obskit.SourceID(job.SourceID),
				obskit.DestinationID(job.DestinationID),
			)
			s.failJob(ctx, job.ID, "archiver query failed")
			return
		}
	} else {
		// Beyond archiver retention — fall back to staging file repository.
		fileIDs, err = s.stagingRepo.GetByDateRange(
			ctx, job.SourceID, job.DestinationID, job.StartDate, job.EndDate,
		)
		if err != nil {
			s.log.Errorn("failed to query staging files for backfill",
				obskit.Error(err),
				logger.NewIntField("jobID", job.ID),
				obskit.SourceID(job.SourceID),
				obskit.DestinationID(job.DestinationID),
			)
			s.failJob(ctx, job.ID, "staging file query failed")
			return
		}
	}

	// If no staging files were found for the date range, the backfill
	// cannot proceed — mark the job as failed with a descriptive reason.
	if len(fileIDs) == 0 {
		s.log.Warnn("no staging files found for backfill date range, marking job as failed",
			logger.NewIntField("jobID", job.ID),
			obskit.SourceID(job.SourceID),
			obskit.DestinationID(job.DestinationID),
		)
		s.failJob(ctx, job.ID, "no staging files found for the specified date range")
		return
	}

	// Staging files resolved successfully — transition job to in_progress.
	// The upload pipeline will pick up the in-progress job and create
	// warehouse uploads with the backfill_job_id foreign key set.
	if err := s.repo.UpdateStatus(ctx, job.ID, StatusInProgress); err != nil {
		s.log.Errorn("failed to transition backfill job to in_progress",
			obskit.Error(err),
			logger.NewIntField("jobID", job.ID),
		)
		return
	}

	s.log.Infon("backfill job transitioned to in_progress",
		logger.NewIntField("jobID", job.ID),
		logger.NewIntField("stagingFileCount", int64(len(fileIDs))),
		obskit.SourceID(job.SourceID),
		obskit.DestinationID(job.DestinationID),
	)
}

// failJob marks a backfill job as StatusFailed, emits the failure metric counter,
// and logs the reason. If the status update itself fails, the error is logged but
// not propagated — the job will be retried on the next monitor iteration and
// eventually cleaned up.
func (s *BackfillService) failJob(ctx context.Context, jobID int64, reason string) {
	if err := s.repo.UpdateStatus(ctx, jobID, StatusFailed); err != nil {
		s.log.Errorn("failed to mark backfill job as failed",
			obskit.Error(err),
			logger.NewIntField("jobID", jobID),
		)
	}
	s.backfillFailed.Increment()
	s.log.Infon("backfill job marked as failed",
		logger.NewIntField("jobID", jobID),
		logger.NewStringField("reason", reason),
	)
}

// ---------------------------------------------------------------------------
// Job Tracking Helpers (thread-safe)
// ---------------------------------------------------------------------------

// trackJob adds a backfill job ID to the set of jobs monitored by this
// service instance. Called from Trigger after successful job creation.
// Thread-safe via mutex.
func (s *BackfillService) trackJob(id int64) {
	s.mu.Lock()
	s.trackedJobs[id] = struct{}{}
	s.mu.Unlock()
}

// untrackJob removes a backfill job ID from the monitored set.
// Called from handleTerminalJob when a job reaches StatusCompleted or StatusFailed.
// Thread-safe via mutex.
func (s *BackfillService) untrackJob(id int64) {
	s.mu.Lock()
	delete(s.trackedJobs, id)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Validation Helpers
// ---------------------------------------------------------------------------

// validateDateRange verifies that the backfill date range is valid:
//   - Both dates must be non-zero (zero time.Time indicates missing input)
//   - startDate must be strictly before endDate
//   - The range must not exceed maxDays (Warehouse.backfill.maxDateRangeDays)
//
// Returns a wrapped sentinel error (ErrInvalidDateRange or ErrDateRangeExceedsMax)
// that the HTTP handler can classify via errors.Is() for appropriate HTTP status codes.
// The wrapping preserves the sentinel error chain while adding descriptive context.
func validateDateRange(startDate, endDate time.Time, maxDays int) error {
	if startDate.IsZero() || endDate.IsZero() {
		return fmt.Errorf("%w: start_date and end_date must not be zero-valued", ErrInvalidDateRange)
	}
	if startDate.After(endDate) {
		return fmt.Errorf("%w: start_date must be before end_date", ErrInvalidDateRange)
	}
	rangeDays := endDate.Sub(startDate).Hours() / 24
	if rangeDays > float64(maxDays) {
		return fmt.Errorf(
			"%w: range of %.0f days exceeds maximum of %d days",
			ErrDateRangeExceedsMax, rangeDays, maxDays,
		)
	}
	return nil
}
