// Package backfill_test contains comprehensive unit tests for the BackfillService
// orchestrator (warehouse/backfill/service.go).
//
// Tests follow table-driven t.Run() subtests with testify/require assertions per
// project conventions. Mock implementations of BackfillRepository, ArchiverQuerier,
// and StagingFileQuerier are defined locally with configurable function fields.
//
// JSON serialization uses github.com/rudderlabs/rudder-go-kit/jsonrs exclusively —
// encoding/json must never be imported per .golangci.yml depguard rules.
package backfill_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/warehouse/backfill"
)

// ---------------------------------------------------------------------------
// Mock Implementations
// ---------------------------------------------------------------------------

// mockBackfillRepo is a configurable test double for the backfill.BackfillRepository
// interface. Each function field controls the return value of the corresponding
// repository method, enabling isolated tests for every service code path.
type mockBackfillRepo struct {
	createFn             func(ctx context.Context, job backfill.BackfillJob) (int64, error)
	getFn                func(ctx context.Context, id int64) (backfill.BackfillJob, error)
	updateStatusFn       func(ctx context.Context, id int64, status string) error
	listBySourceFn       func(ctx context.Context, sourceID string) ([]backfill.BackfillJob, error)
	getActiveCountFn     func(ctx context.Context) (int64, error)
	createIfUnderLimitFn func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error)
	listActiveJobsFn     func(ctx context.Context) ([]backfill.BackfillJob, error)
}

func (m *mockBackfillRepo) Create(ctx context.Context, job backfill.BackfillJob) (int64, error) {
	if m.createFn != nil {
		return m.createFn(ctx, job)
	}
	return 0, errors.New("mockBackfillRepo.Create not configured")
}

func (m *mockBackfillRepo) Get(ctx context.Context, id int64) (backfill.BackfillJob, error) {
	if m.getFn != nil {
		return m.getFn(ctx, id)
	}
	return backfill.BackfillJob{}, errors.New("mockBackfillRepo.Get not configured")
}

func (m *mockBackfillRepo) UpdateStatus(ctx context.Context, id int64, status string) error {
	if m.updateStatusFn != nil {
		return m.updateStatusFn(ctx, id, status)
	}
	return errors.New("mockBackfillRepo.UpdateStatus not configured")
}

func (m *mockBackfillRepo) ListBySource(ctx context.Context, sourceID string) ([]backfill.BackfillJob, error) {
	if m.listBySourceFn != nil {
		return m.listBySourceFn(ctx, sourceID)
	}
	return nil, errors.New("mockBackfillRepo.ListBySource not configured")
}

func (m *mockBackfillRepo) GetActiveCount(ctx context.Context) (int64, error) {
	if m.getActiveCountFn != nil {
		return m.getActiveCountFn(ctx)
	}
	return 0, errors.New("mockBackfillRepo.GetActiveCount not configured")
}

func (m *mockBackfillRepo) CreateIfUnderLimit(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
	if m.createIfUnderLimitFn != nil {
		return m.createIfUnderLimitFn(ctx, job, maxConcurrent)
	}
	return 0, errors.New("mockBackfillRepo.CreateIfUnderLimit not configured")
}

func (m *mockBackfillRepo) ListActiveJobs(ctx context.Context) ([]backfill.BackfillJob, error) {
	if m.listActiveJobsFn != nil {
		return m.listActiveJobsFn(ctx)
	}
	// Default: return empty list (no active jobs to recover on startup)
	return nil, nil
}

// mockArchiverQuerier is a configurable test double for the
// backfill.ArchiverQuerier interface. The listFn field controls the return
// value of ListArchivedStagingFiles, and the called atomic flag records
// whether the method was invoked (for assertion in resolution-path tests).
type mockArchiverQuerier struct {
	listFn func(ctx context.Context, sourceID, destID string, startDate, endDate time.Time) ([]int64, error)
	called atomic.Bool
}

func (m *mockArchiverQuerier) ListArchivedStagingFiles(
	ctx context.Context,
	sourceID, destID string,
	startDate, endDate time.Time,
) ([]int64, error) {
	m.called.Store(true)
	if m.listFn != nil {
		return m.listFn(ctx, sourceID, destID, startDate, endDate)
	}
	return nil, nil
}

// mockStagingFileQuerier is a configurable test double for the
// backfill.StagingFileQuerier interface. The getByDateRangeFn field controls
// the return value, and the called atomic flag records invocation.
type mockStagingFileQuerier struct {
	getByDateRangeFn func(ctx context.Context, sourceID, destID string, startDate, endDate time.Time) ([]int64, error)
	called           atomic.Bool
}

func (m *mockStagingFileQuerier) GetByDateRange(
	ctx context.Context,
	sourceID, destID string,
	startDate, endDate time.Time,
) ([]int64, error) {
	m.called.Store(true)
	if m.getByDateRangeFn != nil {
		return m.getByDateRangeFn(ctx, sourceID, destID, startDate, endDate)
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Helper — creates a default enabled config for tests
// ---------------------------------------------------------------------------

// newTestConfig creates a config.Config with backfill defaults suitable
// for unit testing: enabled=true, maxDateRangeDays=90, maxConcurrentJobs=3,
// monitorIntervalSeconds=1 (fast ticks for Run tests).
func newTestConfig() *config.Config {
	conf := config.New()
	conf.Set(backfill.ConfigKeyEnabled, true)
	conf.Set(backfill.ConfigKeyMaxDateRangeDays, 90)
	conf.Set(backfill.ConfigKeyMaxConcurrentJobs, 3)
	conf.Set(backfill.ConfigKeyMonitorInterval, 1) // 1 second for fast test ticks
	return conf
}

// newTestService creates a BackfillService wired with the provided mocks and
// a test-friendly configuration. If conf is nil, newTestConfig() is used.
func newTestService(
	conf *config.Config,
	repo backfill.BackfillRepository,
	archiver backfill.ArchiverQuerier,
	staging backfill.StagingFileQuerier,
) *backfill.BackfillService {
	if conf == nil {
		conf = newTestConfig()
	}
	return backfill.NewBackfillService(conf, logger.NOP, stats.NOP, repo, archiver, staging)
}

// ---------------------------------------------------------------------------
// TestBackfillService_Trigger
// ---------------------------------------------------------------------------

func TestBackfillService_Trigger(t *testing.T) {
	// Deterministic dates used across subtests for repeatable assertions.
	startDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	t.Run("valid request creates backfill job", func(t *testing.T) {
		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				require.Equal(t, "test_source", job.SourceID)
				require.Equal(t, "test_dest", job.DestinationID)
				require.Equal(t, "test_workspace", job.WorkspaceID)
				require.Equal(t, startDate, job.StartDate)
				require.Equal(t, endDate, job.EndDate)
				require.Equal(t, backfill.StatusPending, job.Status)
				require.Equal(t, 3, maxConcurrent) // default MaxConcurrentJobs
				return 42, nil
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Equal(t, int64(42), resp.JobID)
		require.Equal(t, backfill.StatusPending, resp.Status)
	})

	t.Run("missing source ID returns error", func(t *testing.T) {
		svc := newTestService(nil, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrMissingSourceID)
	})

	t.Run("missing destination ID returns error", func(t *testing.T) {
		svc := newTestService(nil, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrMissingDestinationID)
	})

	t.Run("invalid date range - start after end", func(t *testing.T) {
		svc := newTestService(nil, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		// startDate is after endDate
		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     endDate,   // 2024-01-15
			EndDate:       startDate, // 2024-01-01
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrInvalidDateRange)
	})

	t.Run("date range exceeds max days", func(t *testing.T) {
		svc := newTestService(nil, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		// Range of 120 days exceeds the default maxDateRangeDays of 90
		longEnd := startDate.Add(120 * 24 * time.Hour)
		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       longEnd,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrDateRangeExceedsMax)
	})

	t.Run("backfill disabled returns error", func(t *testing.T) {
		conf := config.New()
		conf.Set(backfill.ConfigKeyEnabled, false)
		conf.Set(backfill.ConfigKeyMaxDateRangeDays, 90)
		conf.Set(backfill.ConfigKeyMaxConcurrentJobs, 3)

		svc := newTestService(conf, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrBackfillDisabled)
	})

	t.Run("concurrent job limit exceeded", func(t *testing.T) {
		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				// Simulate the atomic limit check returning ErrConcurrentLimitReached
				// when the active count is at or above the configured maximum.
				return 0, backfill.ErrConcurrentLimitReached
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrConcurrentLimitReached)
	})

	t.Run("archiver integration - within retention window", func(t *testing.T) {
		// Date range within the 10-day archiver retention window
		// (relative to time.Now() used by the service's default clock).
		now := time.Now()
		recentStart := now.Add(-5 * 24 * time.Hour) // 5 days ago
		recentEnd := now.Add(-2 * 24 * time.Hour)   // 2 days ago

		archiverMock := &mockArchiverQuerier{
			listFn: func(ctx context.Context, sourceID, destID string, sd, ed time.Time) ([]int64, error) {
				return []int64{10, 20, 30}, nil
			},
		}
		stagingMock := &mockStagingFileQuerier{
			getByDateRangeFn: func(ctx context.Context, sourceID, destID string, sd, ed time.Time) ([]int64, error) {
				return nil, nil
			},
		}

		var jobStatusAfterMonitor string
		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				return 1, nil
			},
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				return backfill.BackfillJob{
					ID:            1,
					SourceID:      "test_source",
					DestinationID: "test_dest",
					WorkspaceID:   "test_workspace",
					StartDate:     recentStart,
					EndDate:       recentEnd,
					Status:        backfill.StatusPending,
				}, nil
			},
			updateStatusFn: func(ctx context.Context, id int64, status string) error {
				jobStatusAfterMonitor = status
				return nil
			},
		}

		svc := newTestService(nil, repo, archiverMock, stagingMock)

		// Trigger the backfill job
		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     recentStart,
			EndDate:       recentEnd,
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Equal(t, int64(1), resp.JobID)

		// Start the background monitor to process the pending job.
		// Use a short-lived context so Run exits after the monitor fires.
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = svc.Run(ctx) }()

		// Wait for the archiver mock to be called (resolution path).
		require.Eventually(t, func() bool {
			return archiverMock.called.Load()
		}, 5*time.Second, 50*time.Millisecond,
			"expected archiver to be called for dates within retention window",
		)
		cancel()

		// Verify the archiver was used (not the staging file querier).
		require.True(t, archiverMock.called.Load(), "archiver should have been called")
		require.False(t, stagingMock.called.Load(), "staging file querier should NOT have been called")
		// The job should have been transitioned to in_progress.
		require.Equal(t, backfill.StatusInProgress, jobStatusAfterMonitor)
	})

	t.Run("staging file resolution - beyond retention window", func(t *testing.T) {
		// Date range beyond the 10-day archiver retention window.
		now := time.Now()
		oldStart := now.Add(-30 * 24 * time.Hour) // 30 days ago
		oldEnd := now.Add(-25 * 24 * time.Hour)   // 25 days ago

		archiverMock := &mockArchiverQuerier{
			listFn: func(ctx context.Context, sourceID, destID string, sd, ed time.Time) ([]int64, error) {
				return nil, nil
			},
		}
		stagingMock := &mockStagingFileQuerier{
			getByDateRangeFn: func(ctx context.Context, sourceID, destID string, sd, ed time.Time) ([]int64, error) {
				return []int64{100, 200}, nil
			},
		}

		var jobStatusAfterMonitor string
		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				return 2, nil
			},
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				return backfill.BackfillJob{
					ID:            2,
					SourceID:      "test_source",
					DestinationID: "test_dest",
					WorkspaceID:   "test_workspace",
					StartDate:     oldStart,
					EndDate:       oldEnd,
					Status:        backfill.StatusPending,
				}, nil
			},
			updateStatusFn: func(ctx context.Context, id int64, status string) error {
				jobStatusAfterMonitor = status
				return nil
			},
		}

		svc := newTestService(nil, repo, archiverMock, stagingMock)

		// Trigger the backfill job
		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     oldStart,
			EndDate:       oldEnd,
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Equal(t, int64(2), resp.JobID)

		// Start the background monitor to process the pending job.
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = svc.Run(ctx) }()

		// Wait for the staging file querier mock to be called.
		require.Eventually(t, func() bool {
			return stagingMock.called.Load()
		}, 5*time.Second, 50*time.Millisecond,
			"expected staging file querier to be called for dates beyond retention window",
		)
		cancel()

		// Verify staging was used (not the archiver).
		require.True(t, stagingMock.called.Load(), "staging file querier should have been called")
		require.False(t, archiverMock.called.Load(), "archiver should NOT have been called for old dates")
		// The job should have been transitioned to in_progress.
		require.Equal(t, backfill.StatusInProgress, jobStatusAfterMonitor)
	})

	t.Run("context cancellation", func(t *testing.T) {
		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				// Propagate context error to simulate a cancelled DB call.
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				return 1, nil
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		resp, err := svc.Trigger(ctx, backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("zero-valued start date returns error", func(t *testing.T) {
		svc := newTestService(nil, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     time.Time{},
			EndDate:       endDate,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrInvalidDateRange)
	})

	t.Run("zero-valued end date returns error", func(t *testing.T) {
		svc := newTestService(nil, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       time.Time{},
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrInvalidDateRange)
	})

	t.Run("repository create failure returns wrapped error", func(t *testing.T) {
		repoErr := errors.New("database connection lost")
		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				return 0, repoErr
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, repoErr)
	})

	t.Run("atomic limit check query failure returns wrapped error", func(t *testing.T) {
		repoErr := errors.New("query timeout")
		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				return 0, repoErr
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, repoErr)
	})

	t.Run("custom max date range days config", func(t *testing.T) {
		// Set a very small maxDateRangeDays to verify config takes effect.
		conf := config.New()
		conf.Set(backfill.ConfigKeyEnabled, true)
		conf.Set(backfill.ConfigKeyMaxDateRangeDays, 7)
		conf.Set(backfill.ConfigKeyMaxConcurrentJobs, 3)

		svc := newTestService(conf, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		// 14-day range exceeds the 7-day limit
		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			StartDate:     startDate,
			EndDate:       endDate, // 14 days from startDate
		})
		require.Nil(t, resp)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrDateRangeExceedsMax)
	})

	t.Run("concurrent jobs at limit minus one allows creation", func(t *testing.T) {
		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				// Simulate the atomic check succeeding: 2 active jobs < 3 limit
				return 99, nil
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		resp, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Equal(t, int64(99), resp.JobID)
		require.Equal(t, backfill.StatusPending, resp.Status)
	})
}

// ---------------------------------------------------------------------------
// TestBackfillService_GetStatus
// ---------------------------------------------------------------------------

func TestBackfillService_GetStatus(t *testing.T) {
	t.Run("existing job returns status", func(t *testing.T) {
		expectedJob := backfill.BackfillJob{
			ID:            1,
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			EndDate:       time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
			Status:        backfill.StatusInProgress,
			CreatedAt:     time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt:     time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		}

		repo := &mockBackfillRepo{
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				require.Equal(t, int64(1), id)
				return expectedJob, nil
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		job, err := svc.GetStatus(context.Background(), 1)
		require.NoError(t, err)
		require.NotNil(t, job)
		require.Equal(t, expectedJob.ID, job.ID)
		require.Equal(t, expectedJob.SourceID, job.SourceID)
		require.Equal(t, expectedJob.DestinationID, job.DestinationID)
		require.Equal(t, expectedJob.WorkspaceID, job.WorkspaceID)
		require.Equal(t, expectedJob.Status, job.Status)
		require.Equal(t, expectedJob.StartDate, job.StartDate)
		require.Equal(t, expectedJob.EndDate, job.EndDate)
		require.Equal(t, expectedJob.CreatedAt, job.CreatedAt)
		require.Equal(t, expectedJob.UpdatedAt, job.UpdatedAt)
	})

	t.Run("non-existent job returns error", func(t *testing.T) {
		repo := &mockBackfillRepo{
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				return backfill.BackfillJob{}, backfill.ErrBackfillJobNotFound
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		job, err := svc.GetStatus(context.Background(), 999)
		require.Nil(t, job)
		require.Error(t, err)
		require.ErrorIs(t, err, backfill.ErrBackfillJobNotFound)
	})

	t.Run("repository error is propagated", func(t *testing.T) {
		repoErr := errors.New("database timeout")
		repo := &mockBackfillRepo{
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				return backfill.BackfillJob{}, repoErr
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		job, err := svc.GetStatus(context.Background(), 1)
		require.Nil(t, job)
		require.Error(t, err)
		require.ErrorIs(t, err, repoErr)
	})

	t.Run("completed job returns completed status", func(t *testing.T) {
		repo := &mockBackfillRepo{
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				return backfill.BackfillJob{
					ID:     5,
					Status: backfill.StatusCompleted,
				}, nil
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		job, err := svc.GetStatus(context.Background(), 5)
		require.NoError(t, err)
		require.NotNil(t, job)
		require.Equal(t, backfill.StatusCompleted, job.Status)
	})

	t.Run("failed job returns failed status", func(t *testing.T) {
		repo := &mockBackfillRepo{
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				return backfill.BackfillJob{
					ID:     6,
					Status: backfill.StatusFailed,
				}, nil
			},
		}

		svc := newTestService(nil, repo, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		job, err := svc.GetStatus(context.Background(), 6)
		require.NoError(t, err)
		require.NotNil(t, job)
		require.Equal(t, backfill.StatusFailed, job.Status)
	})
}

// ---------------------------------------------------------------------------
// TestBackfillService_Run
// ---------------------------------------------------------------------------

func TestBackfillService_Run(t *testing.T) {
	t.Run("context cancellation stops background monitor", func(t *testing.T) {
		svc := newTestService(nil, &mockBackfillRepo{}, &mockArchiverQuerier{}, &mockStagingFileQuerier{})

		ctx, cancel := context.WithCancel(context.Background())

		errCh := make(chan error, 1)
		go func() {
			errCh <- svc.Run(ctx)
		}()

		// Cancel the context to signal the monitor to stop.
		cancel()

		// Run should return nil on context cancellation.
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return within timeout after context cancellation")
		}
	})

	t.Run("background monitor updates in-progress jobs", func(t *testing.T) {
		// Track how many times the repo.Get was called, indicating the
		// monitor actually ran and checked job statuses.
		var getCallCount atomic.Int64

		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				return 1, nil
			},
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				getCallCount.Add(1)
				return backfill.BackfillJob{
					ID:            1,
					SourceID:      "test_source",
					DestinationID: "test_dest",
					WorkspaceID:   "test_workspace",
					StartDate:     time.Now().Add(-5 * 24 * time.Hour),
					EndDate:       time.Now().Add(-2 * 24 * time.Hour),
					Status:        backfill.StatusInProgress,
				}, nil
			},
			updateStatusFn: func(ctx context.Context, id int64, status string) error {
				return nil
			},
		}

		archiverMock := &mockArchiverQuerier{
			listFn: func(ctx context.Context, sourceID, destID string, sd, ed time.Time) ([]int64, error) {
				return []int64{1, 2, 3}, nil
			},
		}

		svc := newTestService(nil, repo, archiverMock, &mockStagingFileQuerier{})

		// First trigger a job so it gets tracked.
		now := time.Now()
		_, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     now.Add(-5 * 24 * time.Hour),
			EndDate:       now.Add(-2 * 24 * time.Hour),
		})
		require.NoError(t, err)

		// Start the background monitor.
		ctx, cancel := context.WithCancel(context.Background())

		go func() { _ = svc.Run(ctx) }()

		// Wait until the monitor has checked the job at least once.
		require.Eventually(t, func() bool {
			return getCallCount.Load() >= 1
		}, 5*time.Second, 50*time.Millisecond,
			"expected monitor to check job statuses at least once",
		)

		cancel()
	})

	t.Run("monitor handles completed job by untracking", func(t *testing.T) {
		// Verify that a job reaching StatusCompleted during monitoring is
		// properly untracked (subsequent monitor iterations skip it).
		var getCallCount atomic.Int64

		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				return 1, nil
			},
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				count := getCallCount.Add(1)
				if count == 1 {
					// First call: job is still pending (will be processed)
					return backfill.BackfillJob{
						ID:            1,
						SourceID:      "test_source",
						DestinationID: "test_dest",
						StartDate:     time.Now().Add(-5 * 24 * time.Hour),
						EndDate:       time.Now().Add(-2 * 24 * time.Hour),
						Status:        backfill.StatusPending,
					}, nil
				}
				// Subsequent calls: job is completed
				return backfill.BackfillJob{
					ID:     1,
					Status: backfill.StatusCompleted,
				}, nil
			},
			updateStatusFn: func(ctx context.Context, id int64, status string) error {
				return nil
			},
		}

		archiverMock := &mockArchiverQuerier{
			listFn: func(ctx context.Context, sourceID, destID string, sd, ed time.Time) ([]int64, error) {
				return []int64{1}, nil
			},
		}

		svc := newTestService(nil, repo, archiverMock, &mockStagingFileQuerier{})

		now := time.Now()
		_, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     now.Add(-5 * 24 * time.Hour),
			EndDate:       now.Add(-2 * 24 * time.Hour),
		})
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = svc.Run(ctx) }()

		// Wait for at least two Get calls (first: pending, second: completed).
		require.Eventually(t, func() bool {
			return getCallCount.Load() >= 2
		}, 5*time.Second, 50*time.Millisecond,
			"expected monitor to check job at least twice",
		)

		cancel()
	})

	t.Run("monitor processes failed job from archiver error", func(t *testing.T) {
		// Verify that when the archiver query fails, the job is marked failed.
		var updateStatusCalled atomic.Bool
		var failedStatus string

		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				return 1, nil
			},
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				return backfill.BackfillJob{
					ID:            1,
					SourceID:      "test_source",
					DestinationID: "test_dest",
					StartDate:     time.Now().Add(-3 * 24 * time.Hour),
					EndDate:       time.Now().Add(-1 * 24 * time.Hour),
					Status:        backfill.StatusPending,
				}, nil
			},
			updateStatusFn: func(ctx context.Context, id int64, status string) error {
				updateStatusCalled.Store(true)
				failedStatus = status
				return nil
			},
		}

		archiverMock := &mockArchiverQuerier{
			listFn: func(ctx context.Context, sourceID, destID string, sd, ed time.Time) ([]int64, error) {
				return nil, errors.New("archiver connection failed")
			},
		}

		svc := newTestService(nil, repo, archiverMock, &mockStagingFileQuerier{})

		now := time.Now()
		_, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     now.Add(-3 * 24 * time.Hour),
			EndDate:       now.Add(-1 * 24 * time.Hour),
		})
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = svc.Run(ctx) }()

		// Wait for the status update to be called.
		require.Eventually(t, func() bool {
			return updateStatusCalled.Load()
		}, 5*time.Second, 50*time.Millisecond,
			"expected job to be marked as failed",
		)

		cancel()
		require.Equal(t, backfill.StatusFailed, failedStatus)
	})

	t.Run("monitor marks job failed when no staging files found", func(t *testing.T) {
		// Verify that when the archiver returns zero staging files, the
		// job is marked as failed.
		var failedStatus string
		var updateCalled atomic.Bool

		repo := &mockBackfillRepo{
			createIfUnderLimitFn: func(ctx context.Context, job backfill.BackfillJob, maxConcurrent int) (int64, error) {
				return 1, nil
			},
			getFn: func(ctx context.Context, id int64) (backfill.BackfillJob, error) {
				return backfill.BackfillJob{
					ID:            1,
					SourceID:      "test_source",
					DestinationID: "test_dest",
					StartDate:     time.Now().Add(-3 * 24 * time.Hour),
					EndDate:       time.Now().Add(-1 * 24 * time.Hour),
					Status:        backfill.StatusPending,
				}, nil
			},
			updateStatusFn: func(ctx context.Context, id int64, status string) error {
				failedStatus = status
				updateCalled.Store(true)
				return nil
			},
		}

		// Archiver returns empty — no staging files found.
		archiverMock := &mockArchiverQuerier{
			listFn: func(ctx context.Context, sourceID, destID string, sd, ed time.Time) ([]int64, error) {
				return []int64{}, nil // empty, no files
			},
		}

		svc := newTestService(nil, repo, archiverMock, &mockStagingFileQuerier{})

		now := time.Now()
		_, err := svc.Trigger(context.Background(), backfill.BackfillRequest{
			SourceID:      "test_source",
			DestinationID: "test_dest",
			WorkspaceID:   "test_workspace",
			StartDate:     now.Add(-3 * 24 * time.Hour),
			EndDate:       now.Add(-1 * 24 * time.Hour),
		})
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = svc.Run(ctx) }()

		require.Eventually(t, func() bool {
			return updateCalled.Load()
		}, 5*time.Second, 50*time.Millisecond,
			"expected job to be marked as failed when no staging files found",
		)

		cancel()
		require.Equal(t, backfill.StatusFailed, failedStatus)
	})
}
