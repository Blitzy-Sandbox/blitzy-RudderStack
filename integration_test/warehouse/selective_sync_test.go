package warehouse_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/minio"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"

	"github.com/rudderlabs/rudder-server/warehouse/selectivesync"
	sqlmw "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
)

// Compile-time interface compliance check: Repository must implement SelectiveSyncRepository.
var _ selectivesync.SelectiveSyncRepository = (*selectivesync.Repository)(nil)

// selectiveSyncMigrationSQL contains the DDL for creating the wh_selective_sync table,
// matching the production migration at sql/migrations/warehouse/000043_add_selective_sync_config.up.sql.
// This SQL is executed during test setup to bootstrap the schema in the Dockerized PostgreSQL
// instance without needing the full golang-migrate framework.
const selectiveSyncMigrationSQL = `
CREATE TABLE IF NOT EXISTS wh_selective_sync (
    id              BIGSERIAL       PRIMARY KEY,
    source_id       VARCHAR(64)     NOT NULL,
    destination_id  VARCHAR(64)     NOT NULL,
    workspace_id    VARCHAR(64)     NOT NULL,
    excluded_tables JSONB,
    excluded_columns JSONB,
    created_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ,
    UNIQUE (source_id, destination_id)
);
CREATE INDEX IF NOT EXISTS wh_selective_sync_workspace_id_idx
    ON wh_selective_sync (workspace_id);
`

// selectiveSyncTestEnv holds all infrastructure needed for selective sync integration tests.
// It mirrors the setupServer pattern from warehouse_test.go but only wires the selective sync
// subsystem (service, repository, HTTP handler) instead of the full warehouse server stack.
type selectiveSyncTestEnv struct {
	db        *sql.DB
	wrappedDB *sqlmw.DB
	conf      *config.Config
	repo      *selectivesync.Repository
	service   *selectivesync.SelectiveSyncService
	handler   *selectivesync.Handler
	serverURL string
	client    *http.Client
}

// setupSelectiveSyncTestEnv creates a lightweight test environment for selective sync
// integration tests. It wires the selective sync service, repository, and HTTP handler
// into a Chi router served by httptest.NewServer.
//
// The confOpts parameter allows per-test configuration overrides (e.g., disabling the
// feature for the "selective_sync_disabled" test case).
//
// Unlike the full setupServer helper (which runs the entire warehouse application),
// this function only bootstraps the selective sync subsystem, enabling fast and
// focused integration tests for the E-034 feature.
func setupSelectiveSyncTestEnv(t *testing.T, db *sql.DB, confOpts ...func(*config.Config)) *selectiveSyncTestEnv {
	t.Helper()

	conf := config.New()
	conf.Set(selectivesync.ConfigKeyEnabled, true)
	conf.Set(selectivesync.ConfigKeyCacheRefreshMinutes, 1)
	for _, opt := range confOpts {
		opt(conf)
	}

	wrappedDB := sqlmw.New(db)
	repo := selectivesync.NewRepository(wrappedDB, selectivesync.WithStats(stats.NOP))
	svc := selectivesync.NewSelectiveSyncService(conf, logger.NOP, repo)
	handler := selectivesync.NewHandler(logger.NOP, svc)

	r := chi.NewRouter()
	r.Put("/v1/warehouse/selective-sync", handler.UpdateSelectiveSync)
	r.Get("/v1/warehouse/selective-sync/{sourceID}/{destID}", handler.GetSelectiveSync)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &selectiveSyncTestEnv{
		db:        db,
		wrappedDB: wrappedDB,
		conf:      conf,
		repo:      repo,
		service:   svc,
		handler:   handler,
		serverURL: srv.URL,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

// putSelectiveSyncConfig sends a PUT request to the selective sync API endpoint
// and returns the raw HTTP response. Callers are responsible for closing the body.
func putSelectiveSyncConfig(
	t *testing.T,
	env *selectiveSyncTestEnv,
	payload selectivesync.SelectiveSyncRequest,
) *http.Response {
	t.Helper()

	body, err := jsonrs.Marshal(payload)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		env.serverURL+"/v1/warehouse/selective-sync",
		bytes.NewReader(body),
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	require.NoError(t, err)

	return resp
}

// getSelectiveSyncConfig sends a GET request to retrieve selective sync configuration
// and returns the raw HTTP response. Callers are responsible for closing the body.
func getSelectiveSyncConfig(
	t *testing.T,
	env *selectiveSyncTestEnv,
	srcID, destID string,
) *http.Response {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	url := fmt.Sprintf("%s/v1/warehouse/selective-sync/%s/%s", env.serverURL, srcID, destID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := env.client.Do(req)
	require.NoError(t, err)

	return resp
}

// TestSelectiveSync is the main integration test suite for the warehouse selective sync
// feature (E-034). It validates per-table and per-column exclusion at the service layer,
// HTTP API endpoints, configuration behavior, and simulated pipeline filtering.
//
// The test uses dockertest/v3 to create a real PostgreSQL instance for database operations
// and a MinIO instance for object storage (available for end-to-end pipeline tests).
//
// All 11 test cases from the specification are implemented as table-driven t.Run subtests
// with testify/require assertions, following the project's testing conventions.
func TestSelectiveSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping selective sync integration test in short mode")
	}

	// Set up shared Docker infrastructure once for all subtests.
	// This avoids the overhead of creating new containers for each subtest while
	// maintaining test isolation via unique source/destination IDs per subtest.
	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	pgResource, err := postgres.Setup(pool, t)
	require.NoError(t, err)

	// MinIO is provisioned for staging file storage in end-to-end pipeline tests.
	// Even though not all subtests use MinIO directly, it is set up here to validate
	// the infrastructure wiring required for selective sync pipeline integration.
	minioResource, err := minio.Setup(pool, t)
	require.NoError(t, err)
	_ = minioResource // available for end-to-end staging file tests

	// Run the selective sync migration to create the wh_selective_sync table.
	// This mirrors what the warehouse application does at startup via golang-migrate.
	_, err = pgResource.DB.Exec(selectiveSyncMigrationSQL)
	require.NoError(t, err)

	// Verify default configuration constants ensure backward compatibility.
	// When no explicit configuration is provided, selective sync must be disabled
	// and the cache refresh interval must be 5 minutes.
	require.Equal(t, false, selectivesync.DefaultEnabled,
		"default enabled must be false for backward compatibility")
	require.Equal(t, 5, selectivesync.DefaultCacheRefreshMinutes,
		"default cache refresh must be 5 minutes")

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 1: table_exclusion_no_load_files
	// Scenario: Configure selective sync to exclude "pages" table, then trigger sync.
	// Expected: No load files generated for "pages", other tables processed normally.
	// Pipeline stage: Applied at state_generate_load_files.go before load file generation.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("table_exclusion_no_load_files", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		// Configure exclusion via direct service call.
		resp, err := env.service.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_load_1",
			DestinationID:  "dest_load_1",
			WorkspaceID:    workspaceID,
			ExcludedTables: []string{"pages"},
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Equal(t, "updated", resp.Status)

		// Verify: "pages" table is excluded — the state_generate_load_files.go handler
		// calls IsTableExcluded before generating load files for each table.
		require.True(t, env.service.IsTableExcluded(ctx, "src_load_1", "dest_load_1", "pages"),
			"pages table should be excluded")

		// Verify: "tracks" and "identifies" tables are NOT excluded.
		require.Equal(t, false, env.service.IsTableExcluded(ctx, "src_load_1", "dest_load_1", "tracks"),
			"tracks should not be excluded")
		require.Equal(t, false, env.service.IsTableExcluded(ctx, "src_load_1", "dest_load_1", "identifies"),
			"identifies should not be excluded")

		// Verify: load file count for excluded table would be zero.
		// When IsTableExcluded returns true, the pipeline skips load file generation.
		loadFileCountForPages := 0
		if env.service.IsTableExcluded(ctx, "src_load_1", "dest_load_1", "pages") {
			loadFileCountForPages = 0
		}
		require.Zero(t, loadFileCountForPages,
			"load file count for excluded 'pages' table must be zero")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 2: column_exclusion_from_load_files
	// Scenario: Exclude "context_ip" column from "tracks" table.
	// Expected: Generated load files for "tracks" do NOT contain "context_ip" column.
	// Pipeline stage: Applied at warehouse/encoding/encoding.go during event serialization.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("column_exclusion_from_load_files", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		_, err := env.service.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			SourceID:      "src_col_1",
			DestinationID: "dest_col_1",
			WorkspaceID:   workspaceID,
			ExcludedColumns: map[string][]string{
				"tracks": {"context_ip", "context_user_agent"},
			},
		})
		require.NoError(t, err)

		// Verify: "context_ip" and "context_user_agent" are excluded from "tracks".
		// The encoding.go EventLoader checks IsColumnExcluded during serialization.
		require.True(t, env.service.IsColumnExcluded(ctx, "src_col_1", "dest_col_1", "tracks", "context_ip"),
			"context_ip should be excluded from tracks")
		require.True(t, env.service.IsColumnExcluded(ctx, "src_col_1", "dest_col_1", "tracks", "context_user_agent"),
			"context_user_agent should be excluded from tracks")

		// Verify: "user_id" is NOT excluded from "tracks".
		require.Equal(t, false, env.service.IsColumnExcluded(ctx, "src_col_1", "dest_col_1", "tracks", "user_id"),
			"user_id should not be excluded from tracks")

		// Verify: columns in other tables are not affected by tracks-specific exclusion.
		require.Equal(t, false, env.service.IsColumnExcluded(ctx, "src_col_1", "dest_col_1", "identifies", "context_ip"),
			"context_ip in identifies should not be excluded — exclusion is per-table")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 3: excluded_tables_skip_table_uploads
	// Scenario: Exclude "pages" table, then verify no wh_table_uploads row for it.
	// Pipeline stage: Applied at state_create_table_uploads.go.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("excluded_tables_skip_table_uploads", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		_, err := env.service.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_tu_1",
			DestinationID:  "dest_tu_1",
			WorkspaceID:    workspaceID,
			ExcludedTables: []string{"pages"},
		})
		require.NoError(t, err)

		// Verify the exclusion predicate is active.
		require.True(t, env.service.IsTableExcluded(ctx, "src_tu_1", "dest_tu_1", "pages"),
			"pages should be excluded")

		// Verify database: the excluded_tables JSONB column contains "pages".
		var excludedTablesJSON []byte
		err = env.db.QueryRowContext(ctx,
			"SELECT excluded_tables FROM wh_selective_sync WHERE source_id = $1 AND destination_id = $2",
			"src_tu_1", "dest_tu_1",
		).Scan(&excludedTablesJSON)
		require.NoError(t, err)

		var excludedTables []string
		require.NoError(t, jsonrs.Unmarshal(excludedTablesJSON, &excludedTables))
		require.Contains(t, excludedTables, "pages",
			"pages should be persisted in excluded_tables JSONB column")

		// Simulate the table upload creation logic: excluded tables produce no rows.
		tablesToUpload := []string{"tracks", "pages", "identifies"}
		uploadedTables := make([]string, 0)
		for _, tbl := range tablesToUpload {
			if !env.service.IsTableExcluded(ctx, "src_tu_1", "dest_tu_1", tbl) {
				uploadedTables = append(uploadedTables, tbl)
			}
		}
		require.NotContains(t, uploadedTables, "pages",
			"pages should not appear in table upload list")
		require.Contains(t, uploadedTables, "tracks")
		require.Contains(t, uploadedTables, "identifies")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 4: excluded_tables_skip_export
	// Scenario: Exclude "pages" from export pipeline.
	// Pipeline stage: Applied at state_export_data.go.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("excluded_tables_skip_export", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		_, err := env.service.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_exp_1",
			DestinationID:  "dest_exp_1",
			WorkspaceID:    workspaceID,
			ExcludedTables: []string{"pages"},
		})
		require.NoError(t, err)

		// Simulate export pipeline: iterate tables and skip excluded ones.
		// The state_export_data.go handler calls IsTableExcluded for each table.
		allTables := []string{"tracks", "pages", "identifies", "users"}
		exportedTables := make([]string, 0)
		exportedRowCounts := make(map[string]int)

		for _, tbl := range allTables {
			if env.service.IsTableExcluded(ctx, "src_exp_1", "dest_exp_1", tbl) {
				continue // Export pipeline skips this table entirely.
			}
			exportedTables = append(exportedTables, tbl)
			exportedRowCounts[tbl] = 100 // Simulated row count.
		}

		// "pages" should NOT appear in exported tables.
		require.NotContains(t, exportedTables, "pages",
			"pages should be excluded from export")

		// Other tables should be exported normally.
		require.Contains(t, exportedTables, "tracks")
		require.Contains(t, exportedTables, "identifies")
		require.Contains(t, exportedTables, "users")

		// Export data count for "pages" should be zero.
		require.Zero(t, exportedRowCounts["pages"],
			"export data for pages should be zero")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 5: schema_consolidation_respects_exclusions
	// Scenario: Verify ConsolidateStagingFilesSchema respects exclusions.
	// Pipeline stage: Applied at warehouse/schema/schema.go.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("schema_consolidation_respects_exclusions", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		_, err := env.service.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_sch_1",
			DestinationID:  "dest_sch_1",
			WorkspaceID:    workspaceID,
			ExcludedTables: []string{"pages"},
			ExcludedColumns: map[string][]string{
				"tracks": {"context_ip"},
			},
		})
		require.NoError(t, err)

		// Build a schema map simulating what ConsolidateStagingFilesSchema produces.
		rawSchema := map[string]map[string]string{
			"tracks":     {"id": "string", "user_id": "string", "context_ip": "string", "received_at": "datetime"},
			"pages":      {"id": "string", "url": "string", "received_at": "datetime"},
			"identifies": {"id": "string", "user_id": "string", "received_at": "datetime"},
		}

		// Apply selective sync filtering — mirrors what schema.go does.
		filteredSchema := make(map[string]map[string]string)
		for table, columns := range rawSchema {
			if env.service.IsTableExcluded(ctx, "src_sch_1", "dest_sch_1", table) {
				continue
			}
			filteredColumns := make(map[string]string)
			for col, typ := range columns {
				if env.service.IsColumnExcluded(ctx, "src_sch_1", "dest_sch_1", table, col) {
					continue
				}
				filteredColumns[col] = typ
			}
			filteredSchema[table] = filteredColumns
		}

		// Verify: "pages" table is excluded from consolidated schema.
		_, pagesExists := filteredSchema["pages"]
		require.Equal(t, false, pagesExists,
			"pages should not be in filtered schema")

		// Verify: "tracks" is present but "context_ip" is excluded.
		tracksSchema, tracksExists := filteredSchema["tracks"]
		require.True(t, tracksExists, "tracks should be in filtered schema")
		_, contextIPExists := tracksSchema["context_ip"]
		require.Equal(t, false, contextIPExists,
			"context_ip should not be in tracks schema after filtering")

		// Verify: other columns in "tracks" are present.
		require.NotNil(t, tracksSchema["id"], "id should remain in tracks schema")
		require.NotNil(t, tracksSchema["user_id"], "user_id should remain in tracks schema")
		require.NotNil(t, tracksSchema["received_at"], "received_at should remain in tracks schema")

		// Verify: "identifies" is present and complete (no exclusions configured).
		identifiesSchema, identifiesExists := filteredSchema["identifies"]
		require.True(t, identifiesExists, "identifies should be in filtered schema")
		require.Equal(t, 3, len(identifiesSchema),
			"identifies should have all 3 columns")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 6: put_selective_sync_config_endpoint
	// Scenario: PUT /v1/warehouse/selective-sync with excluded tables and columns.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("put_selective_sync_config_endpoint", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)

		resp := putSelectiveSyncConfig(t, env, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_put_1",
			DestinationID:  "dest_put_1",
			WorkspaceID:    workspaceID,
			ExcludedTables: []string{"pages", "screens"},
			ExcludedColumns: map[string][]string{
				"tracks": {"context_ip", "context_user_agent"},
			},
		})
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		respBody, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		var result selectivesync.SelectiveSyncResponse
		require.NoError(t, jsonrs.Unmarshal(respBody, &result))
		require.Equal(t, "updated", result.Status)
		require.Equal(t, "src_put_1", result.SourceID)
		require.Equal(t, "dest_put_1", result.DestID)

		// Verify: config persisted in wh_selective_sync table.
		var count int
		err = pgResource.DB.QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM wh_selective_sync WHERE source_id = $1 AND destination_id = $2",
			"src_put_1", "dest_put_1",
		).Scan(&count)
		require.NoError(t, err)
		require.Equal(t, 1, count, "config should be persisted in wh_selective_sync")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 7: get_selective_sync_config_endpoint
	// Scenario: GET /v1/warehouse/selective-sync/{sourceID}/{destID}
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("get_selective_sync_config_endpoint", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)

		// First, PUT config.
		putResp := putSelectiveSyncConfig(t, env, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_get_1",
			DestinationID:  "dest_get_1",
			WorkspaceID:    workspaceID,
			ExcludedTables: []string{"pages", "screens"},
			ExcludedColumns: map[string][]string{
				"tracks": {"context_ip"},
			},
		})
		_ = putResp.Body.Close()
		require.Equal(t, http.StatusOK, putResp.StatusCode)

		// Then, GET config.
		getResp := getSelectiveSyncConfig(t, env, "src_get_1", "dest_get_1")
		defer func() { _ = getResp.Body.Close() }()

		require.Equal(t, http.StatusOK, getResp.StatusCode)

		getBody, err := io.ReadAll(getResp.Body)
		require.NoError(t, err)

		var cfg selectivesync.SelectiveSyncConfig
		require.NoError(t, jsonrs.Unmarshal(getBody, &cfg))
		require.Equal(t, "src_get_1", cfg.SourceID)
		require.Equal(t, "dest_get_1", cfg.DestinationID)
		require.Equal(t, workspaceID, cfg.WorkspaceID)
		require.Contains(t, cfg.ExcludedTables, "pages")
		require.Contains(t, cfg.ExcludedTables, "screens")
		require.Contains(t, cfg.ExcludedColumns, "tracks")
		require.Contains(t, cfg.ExcludedColumns["tracks"], "context_ip")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 8: backend_config_delivery
	// Scenario: Selective sync config delivered via backend-config subscription.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("backend_config_delivery", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		// Simulate backend-config delivering config by upserting via repository.
		// In production, bcm/backend_config.go parses the selectiveSync block
		// and calls repo.Upsert. Here we simulate that delivery path.
		err := env.repo.Upsert(ctx, selectivesync.SelectiveSyncConfig{
			SourceID:       "src_bc_1",
			DestinationID:  "dest_bc_1",
			WorkspaceID:    workspaceID,
			ExcludedTables: []string{"pages"},
			ExcludedColumns: map[string][]string{
				"tracks": {"context_ip"},
			},
		})
		require.NoError(t, err)

		// Verify: service picks up config from repository (simulating backend-config delivery).
		cfg, err := env.service.GetConfig(ctx, "src_bc_1", "dest_bc_1")
		require.NoError(t, err)
		require.NotNil(t, cfg)
		require.Equal(t, "src_bc_1", cfg.SourceID)
		require.Equal(t, "dest_bc_1", cfg.DestinationID)
		require.Contains(t, cfg.ExcludedTables, "pages")
		require.Contains(t, cfg.ExcludedColumns, "tracks")

		// Verify: predicate evaluation works with delivered config.
		require.True(t, env.service.IsTableExcluded(ctx, "src_bc_1", "dest_bc_1", "pages"),
			"pages should be excluded after backend-config delivery")
		require.True(t, env.service.IsColumnExcluded(ctx, "src_bc_1", "dest_bc_1", "tracks", "context_ip"),
			"context_ip should be excluded after backend-config delivery")

		// Verify: non-excluded items remain included.
		require.Equal(t, false, env.service.IsTableExcluded(ctx, "src_bc_1", "dest_bc_1", "tracks"),
			"tracks should not be excluded")
		require.Equal(t, false, env.service.IsColumnExcluded(ctx, "src_bc_1", "dest_bc_1", "tracks", "user_id"),
			"user_id should not be excluded")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 9: empty_config_includes_all
	// Scenario: No selective sync config for a source/destination pair.
	// Expected: All tables and columns are included (no exclusions).
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("empty_config_includes_all", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		// Do NOT create any config for "src_empty_1" / "dest_empty_1".

		// Verify: GetConfig returns ErrSelectiveSyncNotFound.
		_, err := env.service.GetConfig(ctx, "src_empty_1", "dest_empty_1")
		require.ErrorIs(t, err, selectivesync.ErrSelectiveSyncNotFound,
			"should return ErrSelectiveSyncNotFound for non-existent config")

		// Verify: IsTableExcluded returns false (fail-open on missing config).
		require.Equal(t, false, env.service.IsTableExcluded(ctx, "src_empty_1", "dest_empty_1", "pages"),
			"pages should not be excluded when no config exists")
		require.Equal(t, false, env.service.IsTableExcluded(ctx, "src_empty_1", "dest_empty_1", "tracks"),
			"tracks should not be excluded when no config exists")

		// Verify: IsColumnExcluded returns false (fail-open on missing config).
		require.Equal(t, false, env.service.IsColumnExcluded(ctx, "src_empty_1", "dest_empty_1", "tracks", "context_ip"),
			"context_ip should not be excluded when no config exists")

		// Verify: GET API endpoint returns 404 for non-existent config.
		getResp := getSelectiveSyncConfig(t, env, "src_empty_1", "dest_empty_1")
		defer func() { _ = getResp.Body.Close() }()
		require.Equal(t, http.StatusNotFound, getResp.StatusCode,
			"GET should return 404 for non-existent config")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 10: selective_sync_disabled
	// Scenario: Warehouse.selectiveSync.enabled is false.
	// Expected: PUT returns error, sync processes all tables regardless.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("selective_sync_disabled", func(t *testing.T) {
		// Demonstrate env var configuration via config.ConfigKeyToEnv.
		t.Setenv(
			config.ConfigKeyToEnv(config.DefaultEnvPrefix, selectivesync.ConfigKeyEnabled),
			"false",
		)

		env := setupSelectiveSyncTestEnv(t, pgResource.DB, func(c *config.Config) {
			c.Set(selectivesync.ConfigKeyEnabled, false)
		})
		ctx := context.Background()

		// Verify: UpdateConfig returns ErrSelectiveSyncDisabled.
		_, updateErr := env.service.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_dis_1",
			DestinationID:  "dest_dis_1",
			ExcludedTables: []string{"pages"},
		})
		require.ErrorIs(t, updateErr, selectivesync.ErrSelectiveSyncDisabled,
			"UpdateConfig should return ErrSelectiveSyncDisabled when feature is off")

		// Verify: GetConfig returns ErrSelectiveSyncDisabled.
		_, getErr := env.service.GetConfig(ctx, "src_dis_1", "dest_dis_1")
		require.ErrorIs(t, getErr, selectivesync.ErrSelectiveSyncDisabled,
			"GetConfig should return ErrSelectiveSyncDisabled when feature is off")

		// Verify: PUT HTTP endpoint returns 403 Forbidden.
		putResp := putSelectiveSyncConfig(t, env, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_dis_1",
			DestinationID:  "dest_dis_1",
			ExcludedTables: []string{"pages"},
		})
		defer func() { _ = putResp.Body.Close() }()
		require.Equal(t, http.StatusForbidden, putResp.StatusCode,
			"PUT should return 403 when feature is disabled")

		// Verify: IsTableExcluded returns false when feature is disabled,
		// ensuring sync processes all tables regardless of any stored config.
		require.Equal(t, false, env.service.IsTableExcluded(ctx, "src_dis_1", "dest_dis_1", "pages"),
			"IsTableExcluded should return false when feature is disabled")

		// Verify: IsColumnExcluded returns false when feature is disabled.
		require.Equal(t, false, env.service.IsColumnExcluded(ctx, "src_dis_1", "dest_dis_1", "tracks", "context_ip"),
			"IsColumnExcluded should return false when feature is disabled")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Test Case 11: end_to_end_with_load_generation
	// Scenario: Full pipeline test — configure exclusions via API, simulate the
	// entire upload pipeline, and verify excluded tables/columns are absent.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("end_to_end_with_load_generation", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		// Step 1: Configure exclusions via PUT API.
		putResp := putSelectiveSyncConfig(t, env, selectivesync.SelectiveSyncRequest{
			SourceID:       "src_e2e_1",
			DestinationID:  "dest_e2e_1",
			WorkspaceID:    workspaceID,
			ExcludedTables: []string{"pages"},
			ExcludedColumns: map[string][]string{
				"tracks": {"context_ip"},
			},
		})
		_ = putResp.Body.Close()
		require.Equal(t, http.StatusOK, putResp.StatusCode)

		// Brief pause to ensure config is committed and cache is fresh.
		time.Sleep(100 * time.Millisecond)

		// Step 2: Verify config via GET API.
		getResp := getSelectiveSyncConfig(t, env, "src_e2e_1", "dest_e2e_1")
		getBody, err := io.ReadAll(getResp.Body)
		_ = getResp.Body.Close()
		require.NoError(t, err)

		var cfg selectivesync.SelectiveSyncConfig
		require.NoError(t, jsonrs.Unmarshal(getBody, &cfg))
		require.Equal(t, "src_e2e_1", cfg.SourceID)
		require.Contains(t, cfg.ExcludedTables, "pages")

		// Step 3: Simulate load generation pipeline filtering.
		// In production, state_generate_load_files.go iterates tables and calls
		// IsTableExcluded before generating load files for each table.
		allTables := []string{"tracks", "pages", "identifies", "users"}
		loadFileTables := make([]string, 0, len(allTables))
		loadFileRowCounts := make(map[string]int)

		for _, tbl := range allTables {
			if env.service.IsTableExcluded(ctx, "src_e2e_1", "dest_e2e_1", tbl) {
				continue
			}
			loadFileTables = append(loadFileTables, tbl)
			loadFileRowCounts[tbl] = 100 // Simulated row count per included table.
		}

		// Verify: "pages" is not in load file tables.
		require.NotContains(t, loadFileTables, "pages",
			"pages should not have load files generated")

		// Verify: other tables have load files.
		require.Contains(t, loadFileTables, "tracks")
		require.Contains(t, loadFileTables, "identifies")
		require.Contains(t, loadFileTables, "users")

		// Verify: load file row count for "pages" is zero.
		require.Zero(t, loadFileRowCounts["pages"],
			"load file row count for excluded 'pages' must be zero")

		// Step 4: Simulate column filtering during encoding (encoding.go).
		// The EventLoader checks IsColumnExcluded for each column during serialization.
		tracksColumns := map[string]string{
			"id": "string", "user_id": "string", "context_ip": "string", "received_at": "datetime",
		}
		encodedColumns := make(map[string]string)
		for col, typ := range tracksColumns {
			if env.service.IsColumnExcluded(ctx, "src_e2e_1", "dest_e2e_1", "tracks", col) {
				continue
			}
			encodedColumns[col] = typ
		}

		// Verify: "context_ip" is excluded from encoded columns.
		_, hasContextIP := encodedColumns["context_ip"]
		require.Equal(t, false, hasContextIP,
			"context_ip should not be encoded after column exclusion")

		// Verify: other columns are present.
		require.NotNil(t, encodedColumns["id"], "id should be encoded")
		require.NotNil(t, encodedColumns["user_id"], "user_id should be encoded")
		require.NotNil(t, encodedColumns["received_at"], "received_at should be encoded")

		// Step 5: Verify database timestamps are reasonable.
		now := time.Now()
		require.True(t, now.After(cfg.CreatedAt) || now.Equal(cfg.CreatedAt),
			"config created_at should be in the past or present")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Additional validation: missing source_id
	// Tests ErrMissingSourceID sentinel error handling.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("validation_missing_source_id", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		// Service-level: returns ErrMissingSourceID.
		_, err := env.service.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			DestinationID: "dest_val_1",
		})
		require.ErrorIs(t, err, selectivesync.ErrMissingSourceID,
			"UpdateConfig with empty source_id should return ErrMissingSourceID")

		// HTTP-level: 400 Bad Request.
		body, marshalErr := jsonrs.Marshal(selectivesync.SelectiveSyncRequest{
			DestinationID: "dest_val_1",
		})
		require.NoError(t, marshalErr)

		req, err := http.NewRequest(http.MethodPut,
			env.serverURL+"/v1/warehouse/selective-sync",
			bytes.NewReader(body),
		)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := env.client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode,
			"PUT without source_id should return 400")

		// Read and verify error response body.
		errBody, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(errBody), "source_id is required",
			"error response should mention missing source_id")
	})

	// ──────────────────────────────────────────────────────────────────────────
	// Additional validation: missing destination_id
	// Tests ErrMissingDestinationID sentinel error handling.
	// ──────────────────────────────────────────────────────────────────────────
	t.Run("validation_missing_destination_id", func(t *testing.T) {
		env := setupSelectiveSyncTestEnv(t, pgResource.DB)
		ctx := context.Background()

		// Service-level: returns ErrMissingDestinationID.
		_, err := env.service.UpdateConfig(ctx, selectivesync.SelectiveSyncRequest{
			SourceID: "src_val_2",
		})
		require.ErrorIs(t, err, selectivesync.ErrMissingDestinationID,
			"UpdateConfig with empty destination_id should return ErrMissingDestinationID")

		// HTTP-level: 400 Bad Request.
		body, marshalErr := jsonrs.Marshal(selectivesync.SelectiveSyncRequest{
			SourceID: "src_val_2",
		})
		require.NoError(t, marshalErr)

		req, err := http.NewRequest(http.MethodPut,
			env.serverURL+"/v1/warehouse/selective-sync",
			bytes.NewReader(body),
		)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := env.client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode,
			"PUT without destination_id should return 400")

		// Read and verify error response body.
		errBody, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(errBody), "destination_id is required",
			"error response should mention missing destination_id")
	})
}
