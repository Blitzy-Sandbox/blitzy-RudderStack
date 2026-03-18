package selectivesync_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/warehouse/selectivesync"
)

// mockSelectiveSyncRepo implements selectivesync.SelectiveSyncRepository
// using configurable function fields for deterministic test control.
type mockSelectiveSyncRepo struct {
	upsertFn          func(ctx context.Context, cfg selectivesync.SelectiveSyncConfig) error
	getFn             func(ctx context.Context, sourceID, destID string) (*selectivesync.SelectiveSyncConfig, error)
	deleteFn          func(ctx context.Context, sourceID, destID string) error
	listByWorkspaceFn func(ctx context.Context, workspaceID string) ([]selectivesync.SelectiveSyncConfig, error)
}

func (m *mockSelectiveSyncRepo) Upsert(ctx context.Context, cfg selectivesync.SelectiveSyncConfig) error {
	if m.upsertFn != nil {
		return m.upsertFn(ctx, cfg)
	}
	return nil
}

func (m *mockSelectiveSyncRepo) Get(ctx context.Context, sourceID, destID string) (*selectivesync.SelectiveSyncConfig, error) {
	if m.getFn != nil {
		return m.getFn(ctx, sourceID, destID)
	}
	return nil, selectivesync.ErrSelectiveSyncNotFound
}

func (m *mockSelectiveSyncRepo) Delete(ctx context.Context, sourceID, destID string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, sourceID, destID)
	}
	return nil
}

func (m *mockSelectiveSyncRepo) ListByWorkspace(ctx context.Context, workspaceID string) ([]selectivesync.SelectiveSyncConfig, error) {
	if m.listByWorkspaceFn != nil {
		return m.listByWorkspaceFn(ctx, workspaceID)
	}
	return []selectivesync.SelectiveSyncConfig{}, nil
}

// newEnabledService constructs a SelectiveSyncService with the feature enabled
// and a 5-minute cache refresh for standard test scenarios.
func newEnabledService(t *testing.T, repo selectivesync.SelectiveSyncRepository) *selectivesync.SelectiveSyncService {
	t.Helper()
	conf := config.New()
	conf.Set("Warehouse.selectiveSync.enabled", true)
	conf.Set("Warehouse.selectiveSync.cacheRefreshMinutes", 5)
	return selectivesync.NewSelectiveSyncService(conf, logger.NOP, repo)
}

// newDisabledService constructs a SelectiveSyncService with the feature disabled.
func newDisabledService(t *testing.T, repo selectivesync.SelectiveSyncRepository) *selectivesync.SelectiveSyncService {
	t.Helper()
	conf := config.New()
	conf.Set("Warehouse.selectiveSync.enabled", false)
	conf.Set("Warehouse.selectiveSync.cacheRefreshMinutes", 5)
	return selectivesync.NewSelectiveSyncService(conf, logger.NOP, repo)
}

func TestSelectiveSyncService_IsTableExcluded(t *testing.T) {
	const (
		sourceID = "test-source-id"
		destID   = "test-destination-id"
	)

	t.Run("excluded table returns true", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:      sourceID,
					DestinationID: destID,
					ExcludedTables: []string{"users", "tracks"},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		result := svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.True(t, result)
	})

	t.Run("non-excluded table returns false", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:      sourceID,
					DestinationID: destID,
					ExcludedTables: []string{"users", "tracks"},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		result := svc.IsTableExcluded(context.Background(), sourceID, destID, "pages")
		require.False(t, result)
	})

	t.Run("empty excluded tables returns false", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:       sourceID,
					DestinationID:  destID,
					ExcludedTables: []string{},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		result := svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.False(t, result)
	})

	t.Run("no config found returns false (default include-all)", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return nil, selectivesync.ErrSelectiveSyncNotFound
			},
		}
		svc := newEnabledService(t, repo)

		result := svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.False(t, result)
	})

	t.Run("case sensitivity", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:       sourceID,
					DestinationID:  destID,
					ExcludedTables: []string{"users"},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		// Exact case match: "users" is excluded
		require.True(t, svc.IsTableExcluded(context.Background(), sourceID, destID, "users"))
		// Different case: "Users" is NOT the same as "users" — the service
		// performs exact string comparison, so mixed-case does not match.
		svc.InvalidateAllCache()
		require.False(t, svc.IsTableExcluded(context.Background(), sourceID, destID, "Users"))
	})

	t.Run("feature disabled returns false", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:       sourceID,
					DestinationID:  destID,
					ExcludedTables: []string{"users", "tracks"},
				}, nil
			},
		}
		svc := newDisabledService(t, repo)

		// Feature is disabled — should always return false regardless of stored config.
		result := svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.False(t, result)
	})
}

func TestSelectiveSyncService_IsColumnExcluded(t *testing.T) {
	const (
		sourceID = "test-source-id"
		destID   = "test-destination-id"
	)

	t.Run("excluded column returns true", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:      sourceID,
					DestinationID: destID,
					ExcludedColumns: map[string][]string{
						"users": {"email", "phone"},
					},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		result := svc.IsColumnExcluded(context.Background(), sourceID, destID, "users", "email")
		require.True(t, result)
	})

	t.Run("non-excluded column returns false", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:      sourceID,
					DestinationID: destID,
					ExcludedColumns: map[string][]string{
						"users": {"email", "phone"},
					},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		result := svc.IsColumnExcluded(context.Background(), sourceID, destID, "users", "name")
		require.False(t, result)
	})

	t.Run("column from non-excluded table returns false", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:      sourceID,
					DestinationID: destID,
					ExcludedColumns: map[string][]string{
						"users": {"email", "phone"},
					},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		// "tracks" table has no column-level exclusions configured.
		result := svc.IsColumnExcluded(context.Background(), sourceID, destID, "tracks", "email")
		require.False(t, result)
	})

	t.Run("empty excluded columns returns false", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:        sourceID,
					DestinationID:   destID,
					ExcludedColumns: map[string][]string{},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		result := svc.IsColumnExcluded(context.Background(), sourceID, destID, "users", "email")
		require.False(t, result)
	})

	t.Run("no config found returns false", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return nil, selectivesync.ErrSelectiveSyncNotFound
			},
		}
		svc := newEnabledService(t, repo)

		result := svc.IsColumnExcluded(context.Background(), sourceID, destID, "users", "email")
		require.False(t, result)
	})

	t.Run("feature disabled returns false", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return &selectivesync.SelectiveSyncConfig{
					SourceID:      sourceID,
					DestinationID: destID,
					ExcludedColumns: map[string][]string{
						"users": {"email", "phone"},
					},
				}, nil
			},
		}
		svc := newDisabledService(t, repo)

		result := svc.IsColumnExcluded(context.Background(), sourceID, destID, "users", "email")
		require.False(t, result)
	})
}

func TestSelectiveSyncService_GetConfig(t *testing.T) {
	const (
		sourceID = "test-source-id"
		destID   = "test-destination-id"
	)

	t.Run("existing config returned", func(t *testing.T) {
		expected := &selectivesync.SelectiveSyncConfig{
			SourceID:       sourceID,
			DestinationID:  destID,
			WorkspaceID:    "ws-1",
			ExcludedTables: []string{"users"},
			ExcludedColumns: map[string][]string{
				"tracks": {"ip"},
			},
		}
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, sid, did string) (*selectivesync.SelectiveSyncConfig, error) {
				require.Equal(t, sourceID, sid)
				require.Equal(t, destID, did)
				return expected, nil
			},
		}
		svc := newEnabledService(t, repo)

		cfg, err := svc.GetConfig(context.Background(), sourceID, destID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		require.Equal(t, expected.SourceID, cfg.SourceID)
		require.Equal(t, expected.DestinationID, cfg.DestinationID)
		require.Equal(t, expected.WorkspaceID, cfg.WorkspaceID)
		require.Equal(t, expected.ExcludedTables, cfg.ExcludedTables)
		require.Equal(t, expected.ExcludedColumns, cfg.ExcludedColumns)
	})

	t.Run("not found returns error", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return nil, selectivesync.ErrSelectiveSyncNotFound
			},
		}
		svc := newEnabledService(t, repo)

		cfg, err := svc.GetConfig(context.Background(), sourceID, destID)
		require.ErrorIs(t, err, selectivesync.ErrSelectiveSyncNotFound)
		require.Nil(t, cfg)
	})

	t.Run("disabled feature returns error", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{}
		svc := newDisabledService(t, repo)

		cfg, err := svc.GetConfig(context.Background(), sourceID, destID)
		require.ErrorIs(t, err, selectivesync.ErrSelectiveSyncDisabled)
		require.Nil(t, cfg)
	})

	t.Run("context cancelled", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			getFn: func(ctx context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				return nil, ctx.Err()
			},
		}
		svc := newEnabledService(t, repo)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		cfg, err := svc.GetConfig(ctx, sourceID, destID)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, cfg)
	})
}

func TestSelectiveSyncService_UpdateConfig(t *testing.T) {
	const (
		sourceID = "test-source-id"
		destID   = "test-destination-id"
	)

	t.Run("valid config updates successfully", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			upsertFn: func(_ context.Context, cfg selectivesync.SelectiveSyncConfig) error {
				require.Equal(t, sourceID, cfg.SourceID)
				require.Equal(t, destID, cfg.DestinationID)
				return nil
			},
		}
		svc := newEnabledService(t, repo)

		resp, err := svc.UpdateConfig(context.Background(), selectivesync.SelectiveSyncRequest{
			SourceID:       sourceID,
			DestinationID:  destID,
			WorkspaceID:    "ws-1",
			ExcludedTables: []string{"users"},
			ExcludedColumns: map[string][]string{
				"tracks": {"ip"},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Equal(t, "updated", resp.Status)
		require.Equal(t, sourceID, resp.SourceID)
		require.Equal(t, destID, resp.DestID)
	})

	t.Run("missing sourceID returns error", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{}
		svc := newEnabledService(t, repo)

		resp, err := svc.UpdateConfig(context.Background(), selectivesync.SelectiveSyncRequest{
			SourceID:      "",
			DestinationID: destID,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, selectivesync.ErrMissingSourceID)
		require.Nil(t, resp)
	})

	t.Run("missing destinationID returns error", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{}
		svc := newEnabledService(t, repo)

		resp, err := svc.UpdateConfig(context.Background(), selectivesync.SelectiveSyncRequest{
			SourceID:      sourceID,
			DestinationID: "",
		})
		require.Error(t, err)
		require.ErrorIs(t, err, selectivesync.ErrMissingDestinationID)
		require.Nil(t, resp)
	})

	t.Run("disabled feature returns error", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{}
		svc := newDisabledService(t, repo)

		resp, err := svc.UpdateConfig(context.Background(), selectivesync.SelectiveSyncRequest{
			SourceID:      sourceID,
			DestinationID: destID,
		})
		require.ErrorIs(t, err, selectivesync.ErrSelectiveSyncDisabled)
		require.Nil(t, resp)
	})

	t.Run("context cancelled", func(t *testing.T) {
		repo := &mockSelectiveSyncRepo{
			upsertFn: func(ctx context.Context, _ selectivesync.SelectiveSyncConfig) error {
				return ctx.Err()
			},
		}
		svc := newEnabledService(t, repo)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		resp, err := svc.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			SourceID:      sourceID,
			DestinationID: destID,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, resp)
	})
}

func TestSelectiveSyncService_CacheBehavior(t *testing.T) {
	const (
		sourceID = "cache-source-id"
		destID   = "cache-destination-id"
	)

	t.Run("cache hit avoids repo call", func(t *testing.T) {
		var callCount atomic.Int32

		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				callCount.Add(1)
				return &selectivesync.SelectiveSyncConfig{
					SourceID:       sourceID,
					DestinationID:  destID,
					ExcludedTables: []string{"users"},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		// First call populates the cache.
		result1 := svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.True(t, result1)

		// Second call with the same source/dest should serve from cache
		// and NOT invoke repo.Get again.
		result2 := svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.True(t, result2)

		require.Equal(t, int32(1), callCount.Load(),
			"repo.Get should be called exactly once; second call should use cached config")
	})

	t.Run("cache refresh after invalidation", func(t *testing.T) {
		var callCount atomic.Int32

		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				callCount.Add(1)
				return &selectivesync.SelectiveSyncConfig{
					SourceID:       sourceID,
					DestinationID:  destID,
					ExcludedTables: []string{"users"},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		// First call: cache miss → repo.Get called.
		result1 := svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.True(t, result1)
		require.Equal(t, int32(1), callCount.Load())

		// Invalidate the cache entry (simulates cache expiry).
		svc.InvalidateCache(sourceID, destID)

		// Third call: cache invalidated → repo.Get called again.
		result2 := svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.True(t, result2)
		require.Equal(t, int32(2), callCount.Load(),
			"repo.Get should be called again after cache invalidation")
	})

	t.Run("different source-dest pairs use separate cache entries", func(t *testing.T) {
		var callCount atomic.Int32

		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, sid, did string) (*selectivesync.SelectiveSyncConfig, error) {
				callCount.Add(1)
				return &selectivesync.SelectiveSyncConfig{
					SourceID:       sid,
					DestinationID:  did,
					ExcludedTables: []string{"users"},
				}, nil
			},
		}
		svc := newEnabledService(t, repo)

		// Call with pair A — cache miss.
		svc.IsTableExcluded(context.Background(), "srcA", "dstA", "users")
		require.Equal(t, int32(1), callCount.Load())

		// Call with pair B — different key, cache miss.
		svc.IsTableExcluded(context.Background(), "srcB", "dstB", "users")
		require.Equal(t, int32(2), callCount.Load())

		// Call pair A again — should be a cache hit.
		svc.IsTableExcluded(context.Background(), "srcA", "dstA", "users")
		require.Equal(t, int32(2), callCount.Load(),
			"second call for srcA/dstA should use cached config")
	})

	t.Run("update config invalidates cache", func(t *testing.T) {
		var getCount atomic.Int32

		repo := &mockSelectiveSyncRepo{
			getFn: func(_ context.Context, _, _ string) (*selectivesync.SelectiveSyncConfig, error) {
				getCount.Add(1)
				return &selectivesync.SelectiveSyncConfig{
					SourceID:       sourceID,
					DestinationID:  destID,
					ExcludedTables: []string{"users"},
				}, nil
			},
			upsertFn: func(_ context.Context, _ selectivesync.SelectiveSyncConfig) error {
				return nil
			},
		}
		svc := newEnabledService(t, repo)

		// Populate cache.
		svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.Equal(t, int32(1), getCount.Load())

		// UpdateConfig should invalidate cache for this source/dest pair.
		_, err := svc.UpdateConfig(context.Background(), selectivesync.SelectiveSyncRequest{
			SourceID:       sourceID,
			DestinationID:  destID,
			ExcludedTables: []string{"tracks"},
		})
		require.NoError(t, err)

		// Next call should trigger a fresh repo.Get.
		svc.IsTableExcluded(context.Background(), sourceID, destID, "users")
		require.Equal(t, int32(2), getCount.Load(),
			"repo.Get should be called again after UpdateConfig invalidated cache")
	})
}
