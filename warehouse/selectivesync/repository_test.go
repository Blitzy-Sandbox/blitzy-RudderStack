// Package selectivesync_test provides comprehensive integration tests for the
// selective sync CRUD repository. Tests follow the established pattern from
// warehouse/internal/repo/staging_test.go — using a real (Dockerized) PostgreSQL
// database with warehouse migrations, t.Run() subtests, and testify/require
// assertions.
package selectivesync_test

import (
	"context"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"

	migrator "github.com/rudderlabs/rudder-server/services/sql-migrator"
	sqlmiddleware "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
	"github.com/rudderlabs/rudder-server/warehouse/selectivesync"
)

// Test data constants for consistent, deterministic test assertions.
const (
	testSourceID      = "test_source_id"
	testDestinationID = "test_destination_id"
	testWorkspaceID   = "test_workspace_id"
)

// setupDB spins up a Dockerized PostgreSQL instance, runs all warehouse migrations
// (including 000043_add_selective_sync_config), and returns a wrapped DB handle.
// The container and database are automatically cleaned up when the test finishes.
//
// This follows the established pattern from warehouse/internal/repo/staging_test.go.
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

// newTestConfig creates a SelectiveSyncConfig with common test data and the provided
// exclusion rules. It uses fixed values for source, destination, and workspace IDs.
func newTestConfig(excludedTables []string, excludedColumns map[string][]string) selectivesync.SelectiveSyncConfig {
	return selectivesync.SelectiveSyncConfig{
		SourceID:        testSourceID,
		DestinationID:   testDestinationID,
		WorkspaceID:     testWorkspaceID,
		ExcludedTables:  excludedTables,
		ExcludedColumns: excludedColumns,
	}
}

// TestSelectiveSyncRepository_Upsert validates the upsert semantics of the
// Repository.Upsert method, covering insert, update, empty exclusions, unique
// constraint enforcement, and context cancellation.
func TestSelectiveSyncRepository_Upsert(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	repo := selectivesync.NewRepository(
		db,
		selectivesync.WithStats(stats.NOP),
		selectivesync.WithNow(func() time.Time { return now }),
	)

	t.Run("insert new config", func(t *testing.T) {
		cfg := newTestConfig(
			[]string{"users", "tracks"},
			map[string][]string{
				"users":  {"email", "phone"},
				"tracks": {"ip"},
			},
		)

		err := repo.Upsert(ctx, cfg)
		require.NoError(t, err)

		// Verify the config was stored correctly by retrieving it.
		got, err := repo.Get(ctx, testSourceID, testDestinationID)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, testSourceID, got.SourceID)
		require.Equal(t, testDestinationID, got.DestinationID)
		require.Equal(t, testWorkspaceID, got.WorkspaceID)
		require.Equal(t, []string{"users", "tracks"}, got.ExcludedTables)
		require.Equal(t, map[string][]string{
			"users":  {"email", "phone"},
			"tracks": {"ip"},
		}, got.ExcludedColumns)
		require.Equal(t, now.UTC(), got.CreatedAt.UTC())
		require.Equal(t, now.UTC(), got.UpdatedAt.UTC())
	})

	t.Run("update existing config", func(t *testing.T) {
		// First, insert a fresh config with unique IDs for this subtest.
		sourceID := "upsert_update_source"
		destID := "upsert_update_dest"

		initial := selectivesync.SelectiveSyncConfig{
			SourceID:        sourceID,
			DestinationID:   destID,
			WorkspaceID:     testWorkspaceID,
			ExcludedTables:  []string{"old_table"},
			ExcludedColumns: map[string][]string{"old_table": {"col_a"}},
		}
		err := repo.Upsert(ctx, initial)
		require.NoError(t, err)

		// Now upsert with the same source/dest but different exclusions.
		updatedNow := now.Add(time.Hour)
		repoUpdated := selectivesync.NewRepository(
			db,
			selectivesync.WithStats(stats.NOP),
			selectivesync.WithNow(func() time.Time { return updatedNow }),
		)

		updated := selectivesync.SelectiveSyncConfig{
			SourceID:        sourceID,
			DestinationID:   destID,
			WorkspaceID:     testWorkspaceID,
			ExcludedTables:  []string{"new_table_1", "new_table_2"},
			ExcludedColumns: map[string][]string{"new_table_1": {"col_x"}},
		}
		err = repoUpdated.Upsert(ctx, updated)
		require.NoError(t, err)

		// Verify the updated values.
		got, err := repo.Get(ctx, sourceID, destID)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, []string{"new_table_1", "new_table_2"}, got.ExcludedTables)
		require.Equal(t, map[string][]string{"new_table_1": {"col_x"}}, got.ExcludedColumns)
		// updated_at should reflect the updated time.
		require.Equal(t, updatedNow.UTC(), got.UpdatedAt.UTC())
	})

	t.Run("upsert with empty exclusion lists", func(t *testing.T) {
		sourceID := "upsert_empty_source"
		destID := "upsert_empty_dest"

		cfg := selectivesync.SelectiveSyncConfig{
			SourceID:        sourceID,
			DestinationID:   destID,
			WorkspaceID:     testWorkspaceID,
			ExcludedTables:  []string{},
			ExcludedColumns: map[string][]string{},
		}
		err := repo.Upsert(ctx, cfg)
		require.NoError(t, err)

		got, err := repo.Get(ctx, sourceID, destID)
		require.NoError(t, err)
		require.NotNil(t, got)
		// Empty arrays and maps should be stored and returned properly.
		require.Equal(t, []string{}, got.ExcludedTables)
		require.Equal(t, map[string][]string{}, got.ExcludedColumns)
	})

	t.Run("upsert with nil exclusion lists normalizes to empty", func(t *testing.T) {
		sourceID := "upsert_nil_source"
		destID := "upsert_nil_dest"

		cfg := selectivesync.SelectiveSyncConfig{
			SourceID:      sourceID,
			DestinationID: destID,
			WorkspaceID:   testWorkspaceID,
			// ExcludedTables and ExcludedColumns intentionally left nil.
		}
		err := repo.Upsert(ctx, cfg)
		require.NoError(t, err)

		got, err := repo.Get(ctx, sourceID, destID)
		require.NoError(t, err)
		require.NotNil(t, got)
		// The repository normalizes nil to empty slices/maps for JSONB.
		require.Equal(t, []string{}, got.ExcludedTables)
		require.Equal(t, map[string][]string{}, got.ExcludedColumns)
	})

	t.Run("unique constraint on source_id + destination_id", func(t *testing.T) {
		sourceID := "upsert_unique_source"
		destID := "upsert_unique_dest"

		cfg1 := selectivesync.SelectiveSyncConfig{
			SourceID:       sourceID,
			DestinationID:  destID,
			WorkspaceID:    testWorkspaceID,
			ExcludedTables: []string{"table_a"},
		}
		err := repo.Upsert(ctx, cfg1)
		require.NoError(t, err)

		cfg2 := selectivesync.SelectiveSyncConfig{
			SourceID:       sourceID,
			DestinationID:  destID,
			WorkspaceID:    testWorkspaceID,
			ExcludedTables: []string{"table_b"},
		}
		err = repo.Upsert(ctx, cfg2)
		require.NoError(t, err)

		// Verify only one record exists by listing all for this workspace.
		all, err := repo.ListByWorkspace(ctx, testWorkspaceID)
		require.NoError(t, err)

		// Count records matching this specific source/dest pair.
		matchCount := 0
		for _, c := range all {
			if c.SourceID == sourceID && c.DestinationID == destID {
				matchCount++
			}
		}
		require.Equal(t, 1, matchCount, "expected exactly one record for the source/dest pair after two upserts")

		// Verify it has the second upsert's data.
		got, err := repo.Get(ctx, sourceID, destID)
		require.NoError(t, err)
		require.Equal(t, []string{"table_b"}, got.ExcludedTables)
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		cfg := newTestConfig([]string{"any"}, nil)
		err := repo.Upsert(cancelCtx, cfg)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestSelectiveSyncRepository_Get validates the Repository.Get method, covering
// successful retrieval, not-found handling, multi-record disambiguation, and
// context cancellation.
func TestSelectiveSyncRepository_Get(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	repo := selectivesync.NewRepository(
		db,
		selectivesync.WithStats(stats.NOP),
		selectivesync.WithNow(func() time.Time { return now }),
	)

	t.Run("existing config returned", func(t *testing.T) {
		sourceID := "get_existing_source"
		destID := "get_existing_dest"

		cfg := selectivesync.SelectiveSyncConfig{
			SourceID:      sourceID,
			DestinationID: destID,
			WorkspaceID:   testWorkspaceID,
			ExcludedTables: []string{"users", "identifies"},
			ExcludedColumns: map[string][]string{
				"users":      {"email", "phone"},
				"identifies": {"anonymous_id"},
			},
		}
		err := repo.Upsert(ctx, cfg)
		require.NoError(t, err)

		got, err := repo.Get(ctx, sourceID, destID)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, sourceID, got.SourceID)
		require.Equal(t, destID, got.DestinationID)
		require.Equal(t, testWorkspaceID, got.WorkspaceID)
		require.Equal(t, []string{"users", "identifies"}, got.ExcludedTables)
		require.Equal(t, map[string][]string{
			"users":      {"email", "phone"},
			"identifies": {"anonymous_id"},
		}, got.ExcludedColumns)
		require.Equal(t, now.UTC(), got.CreatedAt.UTC())
		require.Equal(t, now.UTC(), got.UpdatedAt.UTC())

		// Verify JSONB roundtrip: marshal the stored values and compare.
		tablesJSON, err := jsonrs.Marshal(got.ExcludedTables)
		require.NoError(t, err)
		require.NotNil(t, tablesJSON)

		colsJSON, err := jsonrs.Marshal(got.ExcludedColumns)
		require.NoError(t, err)
		require.NotNil(t, colsJSON)
	})

	t.Run("not found returns error", func(t *testing.T) {
		got, err := repo.Get(ctx, "nonexistent_source", "nonexistent_dest")
		require.ErrorIs(t, err, selectivesync.ErrSelectiveSyncNotFound)
		require.Nil(t, got)
	})

	t.Run("correct config returned with multiple records", func(t *testing.T) {
		// Insert three distinct configs.
		configs := []selectivesync.SelectiveSyncConfig{
			{
				SourceID:       "multi_src_1",
				DestinationID:  "multi_dest_1",
				WorkspaceID:    testWorkspaceID,
				ExcludedTables: []string{"table_1"},
			},
			{
				SourceID:       "multi_src_2",
				DestinationID:  "multi_dest_2",
				WorkspaceID:    testWorkspaceID,
				ExcludedTables: []string{"table_2"},
			},
			{
				SourceID:       "multi_src_3",
				DestinationID:  "multi_dest_3",
				WorkspaceID:    testWorkspaceID,
				ExcludedTables: []string{"table_3"},
			},
		}
		for _, c := range configs {
			err := repo.Upsert(ctx, c)
			require.NoError(t, err)
		}

		// Get the second config and verify it is the correct one.
		got, err := repo.Get(ctx, "multi_src_2", "multi_dest_2")
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, "multi_src_2", got.SourceID)
		require.Equal(t, "multi_dest_2", got.DestinationID)
		require.Equal(t, []string{"table_2"}, got.ExcludedTables)
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		got, err := repo.Get(cancelCtx, testSourceID, testDestinationID)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, got)
	})
}

// TestSelectiveSyncRepository_Delete validates the Repository.Delete method,
// covering successful deletion, idempotent deletion of non-existent records,
// and context cancellation.
func TestSelectiveSyncRepository_Delete(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	repo := selectivesync.NewRepository(
		db,
		selectivesync.WithStats(stats.NOP),
		selectivesync.WithNow(func() time.Time { return now }),
	)

	t.Run("delete existing config", func(t *testing.T) {
		sourceID := "delete_existing_source"
		destID := "delete_existing_dest"

		cfg := selectivesync.SelectiveSyncConfig{
			SourceID:       sourceID,
			DestinationID:  destID,
			WorkspaceID:    testWorkspaceID,
			ExcludedTables: []string{"table_to_delete"},
		}
		err := repo.Upsert(ctx, cfg)
		require.NoError(t, err)

		// Verify config exists before deletion.
		got, err := repo.Get(ctx, sourceID, destID)
		require.NoError(t, err)
		require.NotNil(t, got)

		// Delete the config.
		err = repo.Delete(ctx, sourceID, destID)
		require.NoError(t, err)

		// Verify config no longer exists.
		got, err = repo.Get(ctx, sourceID, destID)
		require.ErrorIs(t, err, selectivesync.ErrSelectiveSyncNotFound)
		require.Nil(t, got)
	})

	t.Run("delete non-existent returns no error", func(t *testing.T) {
		// Deleting a config that doesn't exist should be idempotent.
		err := repo.Delete(ctx, "nonexistent_source", "nonexistent_dest")
		require.NoError(t, err)
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		err := repo.Delete(cancelCtx, testSourceID, testDestinationID)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestSelectiveSyncRepository_ListByWorkspace validates the Repository.ListByWorkspace
// method, covering correct workspace-scoped retrieval, empty results, and context
// cancellation.
func TestSelectiveSyncRepository_ListByWorkspace(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	repo := selectivesync.NewRepository(
		db,
		selectivesync.WithStats(stats.NOP),
		selectivesync.WithNow(func() time.Time { return now }),
	)

	t.Run("returns all configs for workspace", func(t *testing.T) {
		targetWorkspace := "list_workspace_target"
		otherWorkspace := "list_workspace_other"

		// Insert 3 configs for the target workspace.
		for i := 1; i <= 3; i++ {
			cfg := selectivesync.SelectiveSyncConfig{
				SourceID:       "list_src_" + string(rune('0'+i)),
				DestinationID:  "list_dest_" + string(rune('0'+i)),
				WorkspaceID:    targetWorkspace,
				ExcludedTables: []string{"table_" + string(rune('0'+i))},
			}
			err := repo.Upsert(ctx, cfg)
			require.NoError(t, err)
		}

		// Insert 2 configs for a different workspace.
		for i := 1; i <= 2; i++ {
			cfg := selectivesync.SelectiveSyncConfig{
				SourceID:       "other_src_" + string(rune('0'+i)),
				DestinationID:  "other_dest_" + string(rune('0'+i)),
				WorkspaceID:    otherWorkspace,
				ExcludedTables: []string{"other_table"},
			}
			err := repo.Upsert(ctx, cfg)
			require.NoError(t, err)
		}

		// List by target workspace — should return exactly 3.
		results, err := repo.ListByWorkspace(ctx, targetWorkspace)
		require.NoError(t, err)
		require.Len(t, results, 3)

		// Verify all returned configs belong to the target workspace.
		for _, r := range results {
			require.Equal(t, targetWorkspace, r.WorkspaceID)
			require.NotNil(t, r.ExcludedTables)
		}

		// List by other workspace — should return exactly 2.
		otherResults, err := repo.ListByWorkspace(ctx, otherWorkspace)
		require.NoError(t, err)
		require.Len(t, otherResults, 2)
	})

	t.Run("empty result for workspace", func(t *testing.T) {
		results, err := repo.ListByWorkspace(ctx, "nonexistent_workspace_xyz")
		require.NoError(t, err)
		require.NotNil(t, results, "expected empty slice, not nil")
		require.Len(t, results, 0)
	})

	t.Run("context cancelled", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		results, err := repo.ListByWorkspace(cancelCtx, testWorkspaceID)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, results)
	})
}

// TestSelectiveSyncRepository_JSONBRoundtrip verifies that complex JSONB structures
// survive serialization roundtrips through the PostgreSQL JSONB columns correctly.
// This test exercises the jsonrs.Marshal/Unmarshal path used in the repository.
func TestSelectiveSyncRepository_JSONBRoundtrip(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	repo := selectivesync.NewRepository(
		db,
		selectivesync.WithStats(stats.NOP),
		selectivesync.WithNow(func() time.Time { return now }),
	)

	t.Run("complex excluded_columns roundtrip", func(t *testing.T) {
		sourceID := "jsonb_roundtrip_source"
		destID := "jsonb_roundtrip_dest"

		originalTables := []string{"users", "tracks", "identifies", "pages", "screens"}
		originalColumns := map[string][]string{
			"users":      {"email", "phone", "address", "ip"},
			"tracks":     {"context_ip", "context_user_agent"},
			"identifies": {"anonymous_id", "context_traits_email"},
			"pages":      {},
		}

		cfg := selectivesync.SelectiveSyncConfig{
			SourceID:        sourceID,
			DestinationID:   destID,
			WorkspaceID:     testWorkspaceID,
			ExcludedTables:  originalTables,
			ExcludedColumns: originalColumns,
		}
		err := repo.Upsert(ctx, cfg)
		require.NoError(t, err)

		// Retrieve and verify roundtrip.
		got, err := repo.Get(ctx, sourceID, destID)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, originalTables, got.ExcludedTables)
		require.Equal(t, originalColumns, got.ExcludedColumns)

		// Verify jsonrs.Marshal produces valid JSON for these structures.
		tablesJSON, err := jsonrs.Marshal(got.ExcludedTables)
		require.NoError(t, err)
		require.NotNil(t, tablesJSON)

		colsJSON, err := jsonrs.Marshal(got.ExcludedColumns)
		require.NoError(t, err)
		require.NotNil(t, colsJSON)

		// Unmarshal back and verify equality.
		var roundtripTables []string
		err = jsonrs.Unmarshal(tablesJSON, &roundtripTables)
		require.NoError(t, err)
		require.Equal(t, originalTables, roundtripTables)

		var roundtripCols map[string][]string
		err = jsonrs.Unmarshal(colsJSON, &roundtripCols)
		require.NoError(t, err)
		require.Equal(t, originalColumns, roundtripCols)
	})

	t.Run("single table single column roundtrip", func(t *testing.T) {
		sourceID := "jsonb_single_source"
		destID := "jsonb_single_dest"

		cfg := selectivesync.SelectiveSyncConfig{
			SourceID:        sourceID,
			DestinationID:   destID,
			WorkspaceID:     testWorkspaceID,
			ExcludedTables:  []string{"one_table"},
			ExcludedColumns: map[string][]string{"one_table": {"one_column"}},
		}
		err := repo.Upsert(ctx, cfg)
		require.NoError(t, err)

		got, err := repo.Get(ctx, sourceID, destID)
		require.NoError(t, err)
		require.Equal(t, []string{"one_table"}, got.ExcludedTables)
		require.Equal(t, map[string][]string{"one_table": {"one_column"}}, got.ExcludedColumns)
	})
}
