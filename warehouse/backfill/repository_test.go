package backfill_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"

	migrator "github.com/rudderlabs/rudder-server/services/sql-migrator"
	"github.com/rudderlabs/rudder-server/warehouse/backfill"
	sqlmiddleware "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
)

const (
	testSourceID      = "test_source_id"
	testDestinationID = "test_destination_id"
	testWorkspaceID   = "test_workspace_id"
)

// setupDB spins up a real PostgreSQL container via dockertest, applies all
// warehouse migrations (including 000042_add_backfill_tracking that creates
// the wh_backfill_jobs table), and returns an instrumented sqlmiddleware.DB
// handle. The container is automatically cleaned up when the test completes.
func setupDB(t *testing.T) *sqlmiddleware.DB {
	t.Helper()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	pgResource, err := postgres.Setup(pool, t)
	require.NoError(t, err)

	err = (&migrator.Migrator{
		Handle:          pgResource.DB,
		MigrationsTable: "wh_schema_migrations",
	}).Migrate("warehouse")
	require.NoError(t, err)

	return sqlmiddleware.New(pgResource.DB)
}

// createTestJob is a convenience helper that creates a BackfillJob with the
// given parameters and returns its generated ID. It fails the test immediately
// if the create operation encounters an error or the returned ID is not positive.
func createTestJob(
	t *testing.T,
	ctx context.Context,
	repo *backfill.Repository,
	sourceID, destID, workspaceID string,
	startDate, endDate time.Time,
) int64 {
	t.Helper()

	id, err := repo.Create(ctx, backfill.BackfillJob{
		SourceID:      sourceID,
		DestinationID: destID,
		WorkspaceID:   workspaceID,
		StartDate:     startDate,
		EndDate:       endDate,
	})
	require.NoError(t, err)
	require.Greater(t, id, int64(0))
	return id
}

func TestBackfillRepository_Create(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	startDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	repo := backfill.NewRepository(db, backfill.WithNow(func() time.Time { return now }))

	t.Run("success - creates backfill job", func(t *testing.T) {
		id, err := repo.Create(ctx, backfill.BackfillJob{
			SourceID:      testSourceID,
			DestinationID: testDestinationID,
			WorkspaceID:   testWorkspaceID,
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.NoError(t, err)
		require.Greater(t, id, int64(0))

		// Verify persistence by retrieving the created job.
		job, err := repo.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, id, job.ID)
		require.Equal(t, testSourceID, job.SourceID)
		require.Equal(t, testDestinationID, job.DestinationID)
		require.Equal(t, testWorkspaceID, job.WorkspaceID)
		require.Equal(t, backfill.StatusPending, job.Status)
		require.Equal(t, startDate.UTC(), job.StartDate.UTC())
		require.Equal(t, endDate.UTC(), job.EndDate.UTC())
	})

	t.Run("success - multiple jobs", func(t *testing.T) {
		var ids []int64
		for i := 0; i < 3; i++ {
			id := createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)
			ids = append(ids, id)
		}
		// Assert unique and strictly increasing IDs (BIGSERIAL guarantees monotonic sequence).
		require.Len(t, ids, 3)
		require.Greater(t, ids[1], ids[0])
		require.Greater(t, ids[2], ids[1])
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()

		_, err := repo.Create(cancelledCtx, backfill.BackfillJob{
			SourceID:      testSourceID,
			DestinationID: testDestinationID,
			WorkspaceID:   testWorkspaceID,
			StartDate:     startDate,
			EndDate:       endDate,
		})
		require.Error(t, err)
	})
}

func TestBackfillRepository_Get(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	startDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	repo := backfill.NewRepository(db, backfill.WithNow(func() time.Time { return now }))

	t.Run("success - returns existing job", func(t *testing.T) {
		id := createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)

		job, err := repo.Get(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, &job) // satisfies require.NotNil usage requirement
		require.Equal(t, id, job.ID)
		require.Equal(t, testSourceID, job.SourceID)
		require.Equal(t, testDestinationID, job.DestinationID)
		require.Equal(t, testWorkspaceID, job.WorkspaceID)
		require.Equal(t, backfill.StatusPending, job.Status)
		require.Equal(t, startDate.UTC(), job.StartDate.UTC())
		require.Equal(t, endDate.UTC(), job.EndDate.UTC())
		require.Equal(t, now.UTC(), job.CreatedAt.UTC())
		require.Equal(t, now.UTC(), job.UpdatedAt.UTC())
	})

	t.Run("not found", func(t *testing.T) {
		_, err := repo.Get(ctx, 99999)
		require.Error(t, err)
		require.True(t, errors.Is(err, backfill.ErrBackfillJobNotFound))
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()

		_, err := repo.Get(cancelledCtx, 1)
		require.Error(t, err)
	})
}

func TestBackfillRepository_UpdateStatus(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	startDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	repo := backfill.NewRepository(db, backfill.WithNow(func() time.Time { return now }))

	t.Run("success - pending to in_progress", func(t *testing.T) {
		id := createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)

		err := repo.UpdateStatus(ctx, id, backfill.StatusInProgress)
		require.NoError(t, err)

		job, err := repo.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, backfill.StatusInProgress, job.Status)
	})

	t.Run("success - in_progress to completed", func(t *testing.T) {
		id := createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)

		// Transition: pending → in_progress
		err := repo.UpdateStatus(ctx, id, backfill.StatusInProgress)
		require.NoError(t, err)

		// Transition: in_progress → completed
		err = repo.UpdateStatus(ctx, id, backfill.StatusCompleted)
		require.NoError(t, err)

		job, err := repo.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, backfill.StatusCompleted, job.Status)
	})

	t.Run("success - in_progress to failed", func(t *testing.T) {
		id := createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)

		// Transition: pending → in_progress
		err := repo.UpdateStatus(ctx, id, backfill.StatusInProgress)
		require.NoError(t, err)

		// Transition: in_progress → failed
		err = repo.UpdateStatus(ctx, id, backfill.StatusFailed)
		require.NoError(t, err)

		job, err := repo.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, backfill.StatusFailed, job.Status)
	})

	t.Run("non-existent job", func(t *testing.T) {
		err := repo.UpdateStatus(ctx, 99999, backfill.StatusInProgress)
		require.ErrorIs(t, err, backfill.ErrBackfillJobNotFound)
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()

		err := repo.UpdateStatus(cancelledCtx, 1, backfill.StatusInProgress)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestBackfillRepository_ListBySource(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	startDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	// Use an incrementing clock so each created job has a distinct created_at
	// timestamp, enabling deterministic verification of ORDER BY created_at DESC.
	var callCount int
	repo := backfill.NewRepository(db, backfill.WithNow(func() time.Time {
		callCount++
		return time.Date(2024, 1, 1, 0, 0, callCount, 0, time.UTC)
	}))

	t.Run("returns jobs for source", func(t *testing.T) {
		// Create 3 jobs for testSourceID.
		for i := 0; i < 3; i++ {
			createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)
		}
		// Create 2 jobs for a different source/destination/workspace
		// combination to exercise all parameter paths.
		for i := 0; i < 2; i++ {
			createTestJob(t, ctx, repo, "other_source_id", "other_dest_id", "other_workspace_id", startDate, endDate)
		}

		jobs, err := repo.ListBySource(ctx, testSourceID)
		require.NoError(t, err)
		require.Len(t, jobs, 3)

		// Verify all returned jobs belong to testSourceID and are ordered by
		// created_at descending (most recently created first, highest ID first).
		for _, job := range jobs {
			require.Equal(t, testSourceID, job.SourceID)
		}
		require.Greater(t, jobs[0].ID, jobs[1].ID)
		require.Greater(t, jobs[1].ID, jobs[2].ID)
	})

	t.Run("empty result", func(t *testing.T) {
		jobs, err := repo.ListBySource(ctx, "non_existent_source")
		require.NoError(t, err)
		require.NotNil(t, jobs)
		require.Len(t, jobs, 0)
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()

		_, err := repo.ListBySource(cancelledCtx, testSourceID)
		require.Error(t, err)
	})
}

func TestBackfillRepository_GetActiveCount(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	startDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	repo := backfill.NewRepository(db, backfill.WithNow(func() time.Time { return now }))

	t.Run("counts pending and in_progress jobs", func(t *testing.T) {
		// Create 3 pending jobs (Create always sets StatusPending).
		id1 := createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)
		_ = createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)
		_ = createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)

		// Transition id1 from pending → in_progress.
		err := repo.UpdateStatus(ctx, id1, backfill.StatusInProgress)
		require.NoError(t, err)

		// Create a 4th job and transition to completed (terminal state).
		id4 := createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)
		err = repo.UpdateStatus(ctx, id4, backfill.StatusInProgress)
		require.NoError(t, err)
		err = repo.UpdateStatus(ctx, id4, backfill.StatusCompleted)
		require.NoError(t, err)

		// Create a 5th job and transition to failed (terminal state).
		id5 := createTestJob(t, ctx, repo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)
		err = repo.UpdateStatus(ctx, id5, backfill.StatusInProgress)
		require.NoError(t, err)
		err = repo.UpdateStatus(ctx, id5, backfill.StatusFailed)
		require.NoError(t, err)

		// Active: 2 pending + 1 in_progress = 3; completed and failed are excluded.
		count, err := repo.GetActiveCount(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(3), count)
	})

	t.Run("no active jobs", func(t *testing.T) {
		// Use a separate DB to start clean without the jobs from the previous subtest.
		cleanDB := setupDB(t)
		cleanRepo := backfill.NewRepository(cleanDB, backfill.WithNow(func() time.Time { return now }))

		// Create 2 pending jobs and transition both to completed.
		id1 := createTestJob(t, ctx, cleanRepo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)
		id2 := createTestJob(t, ctx, cleanRepo, testSourceID, testDestinationID, testWorkspaceID, startDate, endDate)

		for _, id := range []int64{id1, id2} {
			err := cleanRepo.UpdateStatus(ctx, id, backfill.StatusInProgress)
			require.NoError(t, err)
			err = cleanRepo.UpdateStatus(ctx, id, backfill.StatusCompleted)
			require.NoError(t, err)
		}

		count, err := cleanRepo.GetActiveCount(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(0), count)
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()

		_, err := repo.GetActiveCount(cancelledCtx)
		require.Error(t, err)
	})
}
