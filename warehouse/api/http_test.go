package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"golang.org/x/sync/errgroup"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	kithelper "github.com/rudderlabs/rudder-go-kit/testhelper"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	mocksBackendConfig "github.com/rudderlabs/rudder-server/mocks/backend-config"
	"github.com/rudderlabs/rudder-server/services/notifier"
	migrator "github.com/rudderlabs/rudder-server/services/sql-migrator"
	"github.com/rudderlabs/rudder-server/testhelper/health"
	"github.com/rudderlabs/rudder-server/utils/httputil"
	"github.com/rudderlabs/rudder-server/utils/pubsub"
	"github.com/rudderlabs/rudder-server/warehouse/bcm"
	sqlmiddleware "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
	"github.com/rudderlabs/rudder-server/warehouse/internal/mode"
	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
	"github.com/rudderlabs/rudder-server/warehouse/internal/repo"
	"github.com/rudderlabs/rudder-server/warehouse/multitenant"
	"github.com/rudderlabs/rudder-server/warehouse/source"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// mockBackfillH implements backfillHandler for testing E-032 backfill endpoints.
type mockBackfillH struct {
	triggerFn   func(w http.ResponseWriter, r *http.Request)
	getStatusFn func(w http.ResponseWriter, r *http.Request)
}

func (m *mockBackfillH) TriggerBackfill(w http.ResponseWriter, r *http.Request) {
	if m.triggerFn != nil {
		m.triggerFn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

func (m *mockBackfillH) GetBackfillStatus(w http.ResponseWriter, r *http.Request) {
	if m.getStatusFn != nil {
		m.getStatusFn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

// mockHealthMonitorH implements healthMonitorHandler for testing E-033 health monitoring endpoints.
type mockHealthMonitorH struct {
	getSummaryFn      func(w http.ResponseWriter, r *http.Request)
	getBySourceDestFn func(w http.ResponseWriter, r *http.Request)
}

func (m *mockHealthMonitorH) GetHealthSummary(w http.ResponseWriter, r *http.Request) {
	if m.getSummaryFn != nil {
		m.getSummaryFn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

func (m *mockHealthMonitorH) GetHealthBySourceDest(w http.ResponseWriter, r *http.Request) {
	if m.getBySourceDestFn != nil {
		m.getBySourceDestFn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

// mockSelectiveSyncH implements selectiveSyncHandler for testing E-034 selective sync endpoints.
type mockSelectiveSyncH struct {
	updateFn func(w http.ResponseWriter, r *http.Request)
	getFn    func(w http.ResponseWriter, r *http.Request)
}

func (m *mockSelectiveSyncH) UpdateSelectiveSync(w http.ResponseWriter, r *http.Request) {
	if m.updateFn != nil {
		m.updateFn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

func (m *mockSelectiveSyncH) GetSelectiveSync(w http.ResponseWriter, r *http.Request) {
	if m.getFn != nil {
		m.getFn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

// mockReplayH implements replayHandler for testing E-035 warehouse replay endpoints.
type mockReplayH struct {
	triggerFn   func(w http.ResponseWriter, r *http.Request)
	getStatusFn func(w http.ResponseWriter, r *http.Request)
}

func (m *mockReplayH) TriggerReplay(w http.ResponseWriter, r *http.Request) {
	if m.triggerFn != nil {
		m.triggerFn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

func (m *mockReplayH) GetReplayStatus(w http.ResponseWriter, r *http.Request) {
	if m.getStatusFn != nil {
		m.getStatusFn(w, r)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

func TestHTTPApi(t *testing.T) {
	const (
		workspaceID              = "test_workspace_id"
		sourceID                 = "test_source_id"
		destinationID            = "test_destination_id"
		degradedWorkspaceID      = "degraded_test_workspace_id"
		degradedSourceID         = "degraded_test_source_id"
		degradedDestinationID    = "degraded_test_destination_id"
		unusedWorkspaceID        = "unused_test_workspace_id"
		unusedSourceID           = "unused_test_source_id"
		unusedDestinationID      = "unused_test_destination_id"
		unsupportedWorkspaceID   = "unsupported_test_workspace_id"
		unsupportedSourceID      = "unsupported_test_source_id"
		unsupportedDestinationID = "unsupported_test_destination_id"
		workspaceIdentifier      = "test_workspace-identifier"
		namespace                = "test_namespace"
		destinationType          = "test_destination_type"
		sourceTaskRunID          = "test_source_task_run_id"
		sourceJobID              = "test_source_job_id"
		sourceJobRunID           = "test_source_job_run_id"
	)

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	pgResource, err := postgres.Setup(pool, t)
	require.NoError(t, err)

	t.Log("db:", pgResource.DBDsn)

	err = (&migrator.Migrator{
		Handle:          pgResource.DB,
		MigrationsTable: "wh_schema_migrations",
	}).Migrate("warehouse")
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	mockBackendConfig := mocksBackendConfig.NewMockBackendConfig(ctrl)
	mockBackendConfig.EXPECT().WaitForConfig(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		return nil
	}).AnyTimes()
	mockBackendConfig.EXPECT().Subscribe(gomock.Any(), backendconfig.TopicBackendConfig).DoAndReturn(func(ctx context.Context, topic backendconfig.Topic) pubsub.DataChannel {
		ch := make(chan pubsub.DataEvent, 1)
		ch <- pubsub.DataEvent{
			Data: map[string]backendconfig.ConfigT{
				workspaceID: {
					WorkspaceID: workspaceID,
					Sources: []backendconfig.SourceT{
						{
							ID:      sourceID,
							Enabled: true,
							Destinations: []backendconfig.DestinationT{
								{
									ID:      destinationID,
									Enabled: true,
									DestinationDefinition: backendconfig.DestinationDefinitionT{
										Name: warehouseutils.POSTGRES,
									},
								},
							},
						},
					},
				},
				degradedWorkspaceID: {
					WorkspaceID: degradedWorkspaceID,
					Sources: []backendconfig.SourceT{
						{
							ID:      degradedSourceID,
							Enabled: true,
							Destinations: []backendconfig.DestinationT{
								{
									ID:      degradedDestinationID,
									Enabled: true,
									DestinationDefinition: backendconfig.DestinationDefinitionT{
										Name: warehouseutils.POSTGRES,
									},
								},
							},
						},
					},
				},
				unsupportedWorkspaceID: {
					WorkspaceID: unsupportedWorkspaceID,
					Sources: []backendconfig.SourceT{
						{
							ID:      unsupportedSourceID,
							Enabled: true,
							Destinations: []backendconfig.DestinationT{
								{
									ID:      unsupportedDestinationID,
									Enabled: true,
									DestinationDefinition: backendconfig.DestinationDefinitionT{
										Name: "unknown_destination_type",
									},
								},
							},
						},
					},
				},
				unusedWorkspaceID: {
					WorkspaceID: unusedWorkspaceID,
					Sources: []backendconfig.SourceT{
						{
							ID:      unusedSourceID,
							Enabled: true,
							Destinations: []backendconfig.DestinationT{
								{
									ID:      unusedDestinationID,
									Enabled: true,
									DestinationDefinition: backendconfig.DestinationDefinitionT{
										Name: warehouseutils.POSTGRES,
									},
								},
							},
						},
					},
				},
			},
			Topic: string(backendconfig.TopicBackendConfig),
		}
		close(ch)
		return ch
	}).AnyTimes()

	c := config.New()
	c.Set("Warehouse.degradedWorkspaceIDs", []string{degradedWorkspaceID})

	db := sqlmiddleware.New(pgResource.DB)

	tenantManager := multitenant.New(c, mockBackendConfig)

	bcManager := bcm.New(config.New(), db, tenantManager, logger.NOP, stats.NOP)

	triggerStore := &sync.Map{}

	ctx, stopTest := context.WithCancel(context.Background())

	n := notifier.New(config.New(), logger.NOP, stats.NOP, workspaceIdentifier, true)
	err = n.Setup(ctx, pgResource.DBDsn)
	require.NoError(t, err)

	sourcesManager := source.New(
		c,
		logger.NOP,
		stats.NOP,
		db,
		n,
	)

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		tenantManager.Run(gCtx)
		return nil
	})
	g.Go(func() error {
		bcManager.Start(gCtx)
		return nil
	})
	g.Go(func() error {
		return sourcesManager.Run(gCtx)
	})

	setupCh := make(chan struct{})
	go func() {
		require.NoError(t, g.Wait())
		close(setupCh)
	}()

	now := time.Now().Truncate(time.Second).UTC()
	stagingRepo := repo.NewStagingFiles(db, c, repo.WithNow(func() time.Time {
		return now
	}))
	uploadsRepo := repo.NewUploads(db, repo.WithNow(func() time.Time {
		return now
	}))
	tableUploadsRepo := repo.NewTableUploads(db, c, repo.WithNow(func() time.Time {
		return now
	}))

	stagingFile := model.StagingFile{
		WorkspaceID:           workspaceID,
		Location:              "s3://bucket/path/to/file",
		SourceID:              sourceID,
		DestinationID:         destinationID,
		Status:                warehouseutils.StagingFileWaitingState,
		Error:                 fmt.Errorf("dummy error"),
		FirstEventAt:          now.Add(time.Second),
		UseRudderStorage:      true,
		DestinationRevisionID: "destination_revision_id",
		TotalEvents:           100,
		SourceTaskRunID:       sourceTaskRunID,
		SourceJobID:           sourceJobID,
		SourceJobRunID:        sourceJobRunID,
		TimeWindow:            time.Date(1993, 8, 1, 3, 0, 0, 0, time.UTC),
	}.WithSchema([]byte(`{"type": "object"}`))

	failedStagingID, err := stagingRepo.Insert(ctx, &stagingFile)
	require.NoError(t, err)
	pendingStagingID, err := stagingRepo.Insert(ctx, &stagingFile)
	require.NoError(t, err)

	_, err = uploadsRepo.CreateWithStagingFiles(ctx, model.Upload{
		WorkspaceID:     workspaceID,
		Namespace:       namespace,
		SourceID:        sourceID,
		DestinationID:   destinationID,
		DestinationType: destinationType,
		Status:          model.Aborted,
		SourceJobRunID:  sourceJobRunID,
		SourceTaskRunID: sourceTaskRunID,
	}, []*model.StagingFile{{
		ID:              failedStagingID,
		SourceID:        sourceID,
		DestinationID:   destinationID,
		SourceJobRunID:  sourceJobRunID,
		SourceTaskRunID: sourceTaskRunID,
	}})
	require.NoError(t, err)
	uploadID, err := uploadsRepo.CreateWithStagingFiles(ctx, model.Upload{
		WorkspaceID:     workspaceID,
		Namespace:       namespace,
		SourceID:        sourceID,
		DestinationID:   destinationID,
		DestinationType: destinationType,
		Status:          model.Waiting,
		SourceJobRunID:  sourceJobRunID,
		SourceTaskRunID: sourceTaskRunID,
	}, []*model.StagingFile{{
		ID:              pendingStagingID,
		SourceID:        sourceID,
		DestinationID:   destinationID,
		SourceJobRunID:  sourceJobRunID,
		SourceTaskRunID: sourceTaskRunID,
	}})
	require.NoError(t, err)

	err = tableUploadsRepo.Insert(ctx, uploadID, []string{
		"test_table_1",
		"test_table_2",
		"test_table_3",
		"test_table_4",
		"test_table_5",

		"rudder_discards",
		"rudder_identity_mappings",
		"rudder_identity_merge_rules",
	})
	require.NoError(t, err)

	for range 5 {
		_, err = stagingRepo.Insert(ctx, &stagingFile)
		require.NoError(t, err)
	}

	schemaRepo := repo.NewWHSchemas(db, c, logger.NOP)
	err = schemaRepo.Insert(ctx,
		&model.WHSchema{
			SourceID:        sourceID,
			Namespace:       namespace,
			DestinationID:   destinationID,
			DestinationType: destinationType,
			Schema: model.Schema{
				"test_table": {
					"test_column": "test_data_type",
				},
			},
			CreatedAt: now,
			UpdatedAt: now,
			ExpiresAt: now.Add(1 * time.Hour),
		},
	)
	require.NoError(t, err)

	t.Run("health handler", func(t *testing.T) {
		testCases := []struct {
			name        string
			mode        string
			runningMode string
			response    map[string]string
		}{
			{
				name: "embedded",
				mode: config.EmbeddedMode,
				response: map[string]string{
					"acceptingEvents": "TRUE",
					"db":              "UP",
					"notifier":        "UP",
					"server":          "UP",
					"warehouseMode":   "EMBEDDED",
				},
			},
			{
				name: "master",
				mode: config.MasterMode,
				response: map[string]string{
					"acceptingEvents": "TRUE",
					"db":              "UP",
					"notifier":        "UP",
					"server":          "UP",
					"warehouseMode":   "MASTER",
				},
			},
			{
				name:        "degraded master",
				mode:        config.MasterMode,
				runningMode: "degraded",
				response: map[string]string{
					"acceptingEvents": "TRUE",
					"db":              "UP",
					"notifier":        "",
					"server":          "UP",
					"warehouseMode":   "MASTER",
				},
			},
			{
				name: "master and slave",
				mode: config.MasterSlaveMode,
				response: map[string]string{
					"acceptingEvents": "TRUE",
					"db":              "UP",
					"notifier":        "UP",
					"server":          "UP",
					"warehouseMode":   "MASTER_AND_SLAVE",
				},
			},
			{
				name: "embedded master",
				mode: config.EmbeddedMasterMode,
				response: map[string]string{
					"acceptingEvents": "TRUE",
					"db":              "UP",
					"notifier":        "UP",
					"server":          "UP",
					"warehouseMode":   "EMBEDDED_MASTER",
				},
			},
			{
				name: "slave",
				mode: config.SlaveMode,
				response: map[string]string{
					"acceptingEvents": "TRUE",
					"db":              "",
					"notifier":        "UP",
					"server":          "UP",
					"warehouseMode":   "SLAVE",
				},
			},
			{
				name: "off",
				mode: config.OffMode,
				response: map[string]string{
					"acceptingEvents": "TRUE",
					"db":              "",
					"notifier":        "UP",
					"server":          "UP",
					"warehouseMode":   "OFF",
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/health", nil)
				resp := httptest.NewRecorder()

				c := config.New()
				c.Set("Warehouse.runningMode", tc.runningMode)

				a := NewApi(tc.mode, c, logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
				a.healthHandler(resp, req)

				var healthBody map[string]string
				err = jsonrs.NewDecoder(resp.Body).Decode(&healthBody)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.Code)

				require.EqualValues(t, healthBody, tc.response)
			})
		}
	})

	t.Run("pending events handler", func(t *testing.T) {
		t.Run("invalid payload", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/pending-events", bytes.NewReader([]byte(`"Invalid payload"`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.pendingEventsHandler(resp, req)
			require.Equal(t, http.StatusBadRequest, resp.Code)

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "invalid JSON in request body\n", string(b))
		})

		t.Run("empty source id or task run id", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/pending-events", bytes.NewReader([]byte(`
				{
				  "source_id": "",
				  "task_run_id": ""
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.pendingEventsHandler(resp, req)
			require.Equal(t, http.StatusBadRequest, resp.Code)

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "empty source or task run id\n", string(b))
		})

		t.Run("workspace not found", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/pending-events", bytes.NewReader([]byte(`
				{
				  "source_id": "unknown_source_id",
				  "task_run_id": "unknown_task_run_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.pendingEventsHandler(resp, req)
			require.Equal(t, http.StatusBadRequest, resp.Code)

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "workspace from source not found\n", string(b))
		})

		t.Run("degraded workspace", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/pending-events", bytes.NewReader([]byte(`
				{
				  "source_id": "degraded_test_source_id",
				  "task_run_id": "degraded_task_run_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.pendingEventsHandler(resp, req)
			require.Equal(t, http.StatusServiceUnavailable, resp.Code)

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "workspace is degraded\n", string(b))
		})

		t.Run("pending events available with without trigger uploads", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/pending-events?triggerUpload=false", bytes.NewReader([]byte(`
				{
				  "source_id": "test_source_id",
				  "task_run_id": "test_source_task_run_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.pendingEventsHandler(resp, req)
			require.Equal(t, http.StatusOK, resp.Code)

			var pendingEventsResponse pendingEventsResponse
			err := jsonrs.NewDecoder(resp.Body).Decode(&pendingEventsResponse)
			require.NoError(t, err)

			require.EqualValues(t, pendingEventsResponse.PendingEvents, true)
			require.EqualValues(t, pendingEventsResponse.PendingUploadCount, 1)
			require.EqualValues(t, pendingEventsResponse.PendingStagingFilesCount, 5)
			require.EqualValues(t, pendingEventsResponse.AbortedEvents, true)

			_, isTriggered := triggerStore.Load("POSTGRES:test_source_id:test_destination_id")
			require.False(t, isTriggered)
		})

		t.Run("pending events available with trigger uploads", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/pending-events?triggerUpload=true", bytes.NewReader([]byte(`
				{
				  "source_id": "test_source_id",
				  "task_run_id": "test_source_task_run_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.pendingEventsHandler(resp, req)
			require.Equal(t, http.StatusOK, resp.Code)

			var pendingEventsResponse pendingEventsResponse
			err := jsonrs.NewDecoder(resp.Body).Decode(&pendingEventsResponse)
			require.NoError(t, err)

			defer func() {
				triggerStore.Delete("POSTGRES:test_source_id:test_destination_id")
			}()

			require.EqualValues(t, pendingEventsResponse.PendingEvents, true)
			require.EqualValues(t, pendingEventsResponse.PendingUploadCount, 1)
			require.EqualValues(t, pendingEventsResponse.PendingStagingFilesCount, 5)
			require.EqualValues(t, pendingEventsResponse.AbortedEvents, true)

			_, isTriggered := triggerStore.Load("POSTGRES:test_source_id:test_destination_id")
			require.True(t, isTriggered)
		})

		t.Run("no pending events available", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/pending-events?triggerUpload=true", bytes.NewReader([]byte(`
				{
				  "source_id": "unused_test_source_id",
				  "task_run_id": "unused_test_source_task_run_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.pendingEventsHandler(resp, req)
			require.Equal(t, http.StatusOK, resp.Code)

			var pendingEventsResponse pendingEventsResponse
			err := jsonrs.NewDecoder(resp.Body).Decode(&pendingEventsResponse)
			require.NoError(t, err)
			require.EqualValues(t, pendingEventsResponse.PendingEvents, false)
			require.EqualValues(t, pendingEventsResponse.PendingUploadCount, 0)
			require.EqualValues(t, pendingEventsResponse.PendingStagingFilesCount, 0)
			require.EqualValues(t, pendingEventsResponse.AbortedEvents, false)

			_, isTriggered := triggerStore.Load("POSTGRES:test_source_id:test_destination_id")
			require.False(t, isTriggered)
		})
	})

	t.Run("fetch tables handler", func(t *testing.T) {
		t.Run("invalid payload", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/v1/warehouse/fetch-tables", bytes.NewReader([]byte(`"Invalid payload"`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.fetchTablesHandler(resp, req)
			require.Equal(t, http.StatusBadRequest, resp.Code)

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "invalid JSON in request body\n", string(b))
		})

		t.Run("empty connections", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/v1/warehouse/fetch-tables", bytes.NewReader([]byte(`
				{
				  "connections": []
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.fetchTablesHandler(resp, req)
			require.Equal(t, http.StatusInternalServerError, resp.Code)

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "can't fetch tables\n", string(b))
		})

		t.Run("succeed", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal/v1/warehouse/fetch-tables", bytes.NewReader([]byte(`
				{
				  "connections": [
					{
					  "source_id": "test_source_id",
					  "destination_id": "test_destination_id"
					}
				  ]
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.fetchTablesHandler(resp, req)
			require.Equal(t, http.StatusOK, resp.Code)

			var ftr fetchTablesResponse
			err = jsonrs.NewDecoder(resp.Body).Decode(&ftr)
			require.NoError(t, err)
			require.EqualValues(t, ftr.ConnectionsTables, []warehouseutils.FetchTableInfo{
				{
					SourceID:      sourceID,
					DestinationID: destinationID,
					Namespace:     namespace,
					Tables:        []string{"test_table"},
				},
			})
		})
	})

	t.Run("trigger uploads handler", func(t *testing.T) {
		t.Run("invalid payload", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/trigger-upload", bytes.NewReader([]byte(`"Invalid payload"`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.triggerUploadHandler(resp, req)
			require.Equal(t, http.StatusBadRequest, resp.Code)

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "invalid JSON in request body\n", string(b))
		})

		t.Run("workspace not found", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/trigger-upload", bytes.NewReader([]byte(`
				{
				  "source_id": "unknown_source_id",
				  "destination_id": "unknown_destination_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.triggerUploadHandler(resp, req)
			require.Equal(t, http.StatusBadRequest, resp.Code)

			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "workspace from source not found\n", string(b))
		})

		t.Run("degraded workspaces", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/trigger-upload", bytes.NewReader([]byte(`
				{
				  "source_id": "degraded_test_source_id",
				  "destination_id": "degraded_test_destination_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.triggerUploadHandler(resp, req)

			require.Equal(t, http.StatusServiceUnavailable, resp.Code)
			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "workspace is degraded\n", string(b))
		})

		t.Run("no warehouses", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/trigger-upload", bytes.NewReader([]byte(`
				{
				  "source_id": "unsupported_test_source_id",
				  "destination_id": "unsupported_test_destination_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.triggerUploadHandler(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code)

			_, isTriggered := triggerStore.Load("POSTGRES:unsupported_test_source_id:unsupported_test_destination_id")
			require.False(t, isTriggered)
		})

		t.Run("without destination id", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/trigger-upload", bytes.NewReader([]byte(`
				{
				  "source_id": "test_source_id",
				  "destination_id": ""
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.triggerUploadHandler(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			defer func() {
				triggerStore.Delete("POSTGRES:test_source_id:test_destination_id")
			}()

			_, isTriggered := triggerStore.Load("POSTGRES:test_source_id:test_destination_id")
			require.True(t, isTriggered)
		})

		t.Run("with destination id", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/trigger-upload", bytes.NewReader([]byte(`
				{
				  "source_id": "test_source_id",
				  "destination_id": "test_destination_id"
				}
			`)))
			resp := httptest.NewRecorder()

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)
			a.triggerUploadHandler(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			defer func() {
				triggerStore.Delete("POSTGRES:test_source_id:test_destination_id")
			}()

			_, isTriggered := triggerStore.Load("POSTGRES:test_source_id:test_destination_id")
			require.True(t, isTriggered)
		})
	})

	t.Run("backfill handler", func(t *testing.T) {
		backfillMock := &mockBackfillH{
			triggerFn: func(w http.ResponseWriter, r *http.Request) {
				defer func() { _ = r.Body.Close() }()

				type bfReq struct {
					SourceID      string `json:"source_id"`
					DestinationID string `json:"destination_id"`
					StartDate     string `json:"start_date"`
					EndDate       string `json:"end_date"`
				}

				var payload bfReq
				if err := jsonrs.NewDecoder(r.Body).Decode(&payload); err != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":"error","message":"invalid JSON in request body"}`))
					return
				}
				if payload.SourceID == "" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":"error","message":"missing source_id"}`))
					return
				}
				if payload.DestinationID == "" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":"error","message":"missing destination_id"}`))
					return
				}

				startDate, sErr := time.Parse(time.RFC3339, payload.StartDate)
				endDate, eErr := time.Parse(time.RFC3339, payload.EndDate)
				if sErr != nil || eErr != nil || !startDate.Before(endDate) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":"error","message":"invalid date range"}`))
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_ = jsonrs.NewEncoder(w).Encode(map[string]interface{}{
					"jobID":  1,
					"status": "Pending",
				})
			},
		}

		a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, backfillMock, nil, nil, nil)

		t.Run("POST /v1/warehouse/backfill - valid request", func(t *testing.T) {
			body := bytes.NewReader([]byte(`{
				"source_id": "test_source_id",
				"destination_id": "test_destination_id",
				"start_date": "2024-01-01T00:00:00Z",
				"end_date": "2024-01-15T00:00:00Z"
			}`))
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/backfill", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.backfillH.TriggerBackfill(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			var result map[string]interface{}
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&result)
			require.NoError(t, decErr)
			require.Equal(t, float64(1), result["jobID"])
			require.Equal(t, "Pending", result["status"])
		})

		t.Run("POST /v1/warehouse/backfill - invalid JSON", func(t *testing.T) {
			body := bytes.NewReader([]byte(`not valid json`))
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/backfill", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.backfillH.TriggerBackfill(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code)

			var errResp map[string]string
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&errResp)
			require.NoError(t, decErr)
			require.Equal(t, "error", errResp["status"])
			require.Contains(t, errResp["message"], "invalid JSON")
		})

		t.Run("POST /v1/warehouse/backfill - missing source_id", func(t *testing.T) {
			body := bytes.NewReader([]byte(`{
				"destination_id": "test_destination_id",
				"start_date": "2024-01-01T00:00:00Z",
				"end_date": "2024-01-15T00:00:00Z"
			}`))
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/backfill", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.backfillH.TriggerBackfill(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code)

			var errResp map[string]string
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&errResp)
			require.NoError(t, decErr)
			require.Equal(t, "error", errResp["status"])
			require.Contains(t, errResp["message"], "source_id")
		})

		t.Run("POST /v1/warehouse/backfill - missing destination_id", func(t *testing.T) {
			body := bytes.NewReader([]byte(`{
				"source_id": "test_source_id",
				"start_date": "2024-01-01T00:00:00Z",
				"end_date": "2024-01-15T00:00:00Z"
			}`))
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/backfill", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.backfillH.TriggerBackfill(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code)

			var errResp map[string]string
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&errResp)
			require.NoError(t, decErr)
			require.Equal(t, "error", errResp["status"])
			require.Contains(t, errResp["message"], "destination_id")
		})

		t.Run("POST /v1/warehouse/backfill - invalid date range", func(t *testing.T) {
			body := bytes.NewReader([]byte(`{
				"source_id": "test_source_id",
				"destination_id": "test_destination_id",
				"start_date": "2024-01-15T00:00:00Z",
				"end_date": "2024-01-01T00:00:00Z"
			}`))
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/backfill", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.backfillH.TriggerBackfill(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code)

			var errResp map[string]string
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&errResp)
			require.NoError(t, decErr)
			require.Equal(t, "error", errResp["status"])
			require.Contains(t, errResp["message"], "date range")
		})
	})

	t.Run("health monitoring handler", func(t *testing.T) {
		t.Run("GET /v1/warehouse/health - success", func(t *testing.T) {
			healthMock := &mockHealthMonitorH{
				getSummaryFn: func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = jsonrs.NewEncoder(w).Encode(map[string]interface{}{
						"sources": []interface{}{
							map[string]interface{}{
								"sourceID": sourceID,
								"destinations": []interface{}{
									map[string]interface{}{
										"destID":       destinationID,
										"syncDuration": map[string]interface{}{"p50": 120, "p95": 300},
										"rowsSynced":   int64(1000),
										"errorRate":    0.01,
										"lastSync":     "2024-01-15T00:00:00Z",
									},
								},
							},
						},
					})
				},
			}

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, healthMock, nil, nil)

			req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health", nil)
			resp := httptest.NewRecorder()

			a.healthMonitorH.GetHealthSummary(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			var result map[string]interface{}
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&result)
			require.NoError(t, decErr)
			require.Contains(t, result, "sources")
			sources, ok := result["sources"].([]interface{})
			require.True(t, ok)
			require.Len(t, sources, 1)
		})

		t.Run("GET /v1/warehouse/health - empty state", func(t *testing.T) {
			healthMock := &mockHealthMonitorH{
				getSummaryFn: func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = jsonrs.NewEncoder(w).Encode(map[string]interface{}{
						"sources": []interface{}{},
					})
				},
			}

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, healthMock, nil, nil)

			req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health", nil)
			resp := httptest.NewRecorder()

			a.healthMonitorH.GetHealthSummary(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			var result map[string]interface{}
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&result)
			require.NoError(t, decErr)
			sources, ok := result["sources"].([]interface{})
			require.True(t, ok)
			require.Empty(t, sources)
		})

		t.Run("GET /v1/warehouse/health/{sourceID}/{destID} - success", func(t *testing.T) {
			healthMock := &mockHealthMonitorH{
				getBySourceDestFn: func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = jsonrs.NewEncoder(w).Encode(map[string]interface{}{
						"sourceID":     sourceID,
						"destID":       destinationID,
						"syncDuration": map[string]interface{}{"p50": 120, "p95": 300},
						"rowsSynced":   int64(1000),
						"errorRate":    0.01,
						"lastSync":     "2024-01-15T00:00:00Z",
					})
				},
			}

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, healthMock, nil, nil)

			req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health/test_source_id/test_destination_id", nil)
			resp := httptest.NewRecorder()

			a.healthMonitorH.GetHealthBySourceDest(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			var result map[string]interface{}
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&result)
			require.NoError(t, decErr)
			require.Equal(t, sourceID, result["sourceID"])
			require.Equal(t, destinationID, result["destID"])
		})

		t.Run("GET /v1/warehouse/health/{sourceID}/{destID} - not found", func(t *testing.T) {
			healthMock := &mockHealthMonitorH{
				getBySourceDestFn: func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"status":"error","message":"no health data found"}`))
				},
			}

			a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, healthMock, nil, nil)

			req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/health/unknown_source/unknown_dest", nil)
			resp := httptest.NewRecorder()

			a.healthMonitorH.GetHealthBySourceDest(resp, req)

			require.Equal(t, http.StatusNotFound, resp.Code)
		})
	})

	t.Run("selective sync handler", func(t *testing.T) {
		selectiveSyncMock := &mockSelectiveSyncH{
			updateFn: func(w http.ResponseWriter, r *http.Request) {
				defer func() { _ = r.Body.Close() }()

				type ssReq struct {
					SourceID        string              `json:"source_id"`
					DestinationID   string              `json:"destination_id"`
					ExcludedTables  []string            `json:"excluded_tables"`
					ExcludedColumns map[string][]string `json:"excluded_columns"`
				}

				var payload ssReq
				if err := jsonrs.NewDecoder(r.Body).Decode(&payload); err != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":"error","message":"invalid JSON in request body"}`))
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_ = jsonrs.NewEncoder(w).Encode(map[string]interface{}{
					"status":   "updated",
					"sourceID": payload.SourceID,
					"destID":   payload.DestinationID,
				})
			},
			getFn: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = jsonrs.NewEncoder(w).Encode(map[string]interface{}{
					"source_id":       sourceID,
					"destination_id":  destinationID,
					"excluded_tables": []string{"table_a"},
					"excluded_columns": map[string][]string{
						"table_b": {"col_x"},
					},
				})
			},
		}

		a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, selectiveSyncMock, nil)

		t.Run("PUT /v1/warehouse/selective-sync - valid config", func(t *testing.T) {
			body := bytes.NewReader([]byte(`{
				"source_id": "test_source_id",
				"destination_id": "test_destination_id",
				"excluded_tables": ["table_a"],
				"excluded_columns": {"table_b": ["col_x"]}
			}`))
			req := httptest.NewRequest(http.MethodPut, "/v1/warehouse/selective-sync", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.selectiveSyncH.UpdateSelectiveSync(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			var result map[string]interface{}
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&result)
			require.NoError(t, decErr)
			require.Equal(t, "updated", result["status"])
			require.Equal(t, sourceID, result["sourceID"])
			require.Equal(t, destinationID, result["destID"])
		})

		t.Run("PUT /v1/warehouse/selective-sync - invalid JSON", func(t *testing.T) {
			body := bytes.NewReader([]byte(`not valid json`))
			req := httptest.NewRequest(http.MethodPut, "/v1/warehouse/selective-sync", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.selectiveSyncH.UpdateSelectiveSync(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code)
		})

		t.Run("GET /v1/warehouse/selective-sync/{sourceID}/{destID} - success", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/selective-sync/test_source_id/test_destination_id", nil)
			resp := httptest.NewRecorder()

			a.selectiveSyncH.GetSelectiveSync(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			var result map[string]interface{}
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&result)
			require.NoError(t, decErr)
			require.Equal(t, sourceID, result["source_id"])
			require.Equal(t, destinationID, result["destination_id"])
		})
	})

	t.Run("replay handler", func(t *testing.T) {
		replayMock := &mockReplayH{
			triggerFn: func(w http.ResponseWriter, r *http.Request) {
				defer func() { _ = r.Body.Close() }()

				type rpReq struct {
					SourceID      string `json:"source_id"`
					DestinationID string `json:"destination_id"`
					StartTime     string `json:"start_time"`
					EndTime       string `json:"end_time"`
				}

				var payload rpReq
				if err := jsonrs.NewDecoder(r.Body).Decode(&payload); err != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":"error","message":"invalid JSON in request body"}`))
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_ = jsonrs.NewEncoder(w).Encode(map[string]interface{}{
					"jobID":  1,
					"status": "Pending",
				})
			},
			getStatusFn: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = jsonrs.NewEncoder(w).Encode(map[string]interface{}{
					"jobID":  1,
					"status": "InProgress",
				})
			},
		}

		a := NewApi(config.MasterMode, config.New(), logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, replayMock)

		t.Run("POST /v1/warehouse/replay - valid request", func(t *testing.T) {
			body := bytes.NewReader([]byte(`{
				"source_id": "test_source_id",
				"destination_id": "test_destination_id",
				"start_time": "2024-01-01T00:00:00Z",
				"end_time": "2024-01-15T00:00:00Z"
			}`))
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/replay", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.replayH.TriggerReplay(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			var result map[string]interface{}
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&result)
			require.NoError(t, decErr)
			require.Equal(t, float64(1), result["jobID"])
			require.Equal(t, "Pending", result["status"])
		})

		t.Run("POST /v1/warehouse/replay - invalid JSON", func(t *testing.T) {
			body := bytes.NewReader([]byte(`not valid json`))
			req := httptest.NewRequest(http.MethodPost, "/v1/warehouse/replay", body)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			a.replayH.TriggerReplay(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code)
		})

		t.Run("GET /v1/warehouse/replay/{jobID} - success", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/warehouse/replay/1", nil)
			resp := httptest.NewRecorder()

			a.replayH.GetReplayStatus(resp, req)

			require.Equal(t, http.StatusOK, resp.Code)

			var result map[string]interface{}
			decErr := jsonrs.NewDecoder(resp.Body).Decode(&result)
			require.NoError(t, decErr)
			require.Equal(t, float64(1), result["jobID"])
			require.Equal(t, "InProgress", result["status"])
		})
	})

	t.Run("endpoints", func(t *testing.T) {
		t.Run("normal mode", func(t *testing.T) {
			webPort, err := kithelper.GetFreePort()
			require.NoError(t, err)

			c := config.New()
			c.Set("Warehouse.webPort", webPort)

			srvCtx, stopServer := context.WithCancel(ctx)

			// Provide non-nil mock handlers so that the Chi router registers the Sprint 7-9 routes.
			integrationBackfillH := &mockBackfillH{
				triggerFn: func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"jobID":1,"status":"Pending"}`))
				},
				getStatusFn: func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"jobID":1,"status":"Pending"}`))
				},
			}
			integrationHealthH := &mockHealthMonitorH{
				getSummaryFn: func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"sources":[]}`))
				},
				getBySourceDestFn: func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{}`))
				},
			}
			integrationSSH := &mockSelectiveSyncH{
				updateFn: func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"status":"updated"}`))
				},
				getFn: func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{}`))
				},
			}
			integrationReplayH := &mockReplayH{
				triggerFn: func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"jobID":1,"status":"Pending"}`))
				},
				getStatusFn: func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"jobID":1,"status":"Pending"}`))
				},
			}

			a := NewApi(config.MasterMode, c, logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, integrationBackfillH, integrationHealthH, integrationSSH, integrationReplayH)

			serverSetupCh := make(chan struct{})
			go func() {
				require.NoError(t, a.Start(srvCtx))

				close(serverSetupCh)
			}()

			serverURL := fmt.Sprintf("http://localhost:%d", webPort)

			t.Run("health", func(t *testing.T) {
				health.WaitUntilReady(ctx, t, fmt.Sprintf("%s/health", serverURL), time.Second*10, time.Millisecond*100, t.Name())
			})

			t.Run("process", func(t *testing.T) {
				pendingEventsURL := fmt.Sprintf("%s/v1/process", serverURL)
				req, err := http.NewRequest(http.MethodPost, pendingEventsURL, bytes.NewReader([]byte(`
				{
				  "WorkspaceID": "test_workspace_id",
				  "Schema": {
					"test_table": {
					  "test_column": "test_data_type"
					}
				  },
				  "BatchDestination": {
					"Source": {
					  "ID": "test_source_id"
					},
					"Destination": {
					  "ID": "test_destination_id"
					}
				  },
				  "Location": "rudder-warehouse-staging-logs/279L3gEKqwruBoKGsXZtSVX7vIy/2022-11-08/1667913810.279L3gEKqwruBoKGsXZtSVX7vIy.7a6e7785-7a75-4345-8d3c-d7a1ce49a43f.json.gz",
				  "FirstEventAt": "2022-11-08T13:23:07Z",
				  "LastEventAt": "2022-11-08T13:23:07Z",
				  "TotalEvents": 2,
				  "TotalBytes": 2000,
				  "UseRudderStorage": false,
				  "DestinationRevisionID": "2H1cLBvL3v0prRBNzpe8D34XTzU",
				  "SourceTaskRunID": "test_source_task_run_id",
				  "SourceJobID": "test_source_job_id",
				  "SourceJobRunID": "test_source_job_run_id",
				  "TimeWindow": "0001-01-01T00:40:00Z"
				}
			`)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			t.Run("pending events", func(t *testing.T) {
				pendingEventsURL := fmt.Sprintf("%s/v1/warehouse/pending-events?triggerUpload=true", serverURL)
				req, err := http.NewRequest(http.MethodPost, pendingEventsURL, bytes.NewReader([]byte(`
				{
				  "source_id": "test_source_id",
				  "task_run_id": "test_source_task_run_id"
				}
			`)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			t.Run("trigger upload", func(t *testing.T) {
				triggerUploadURL := fmt.Sprintf("%s/v1/warehouse/trigger-upload", serverURL)
				req, err := http.NewRequest(http.MethodPost, triggerUploadURL, bytes.NewReader([]byte(`
				{
				  "source_id": "test_source_id",
				  "destination_id": "test_destination_id"
				}
			`)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			t.Run("fetch tables", func(t *testing.T) {
				for _, u := range []string{
					fmt.Sprintf("%s/v1/warehouse/fetch-tables", serverURL),
					fmt.Sprintf("%s/internal/v1/warehouse/fetch-tables", serverURL),
				} {
					req, err := http.NewRequest(http.MethodGet, u, bytes.NewReader([]byte(`
				{
				  "connections": [
					{
					  "source_id": "test_source_id",
					  "destination_id": "test_destination_id"
					}
				  ]
				}
			`)))
					require.NoError(t, err)
					req.Header.Set("Content-Type", "application/json")

					resp, err := (&http.Client{}).Do(req)
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, resp.StatusCode)

					t.Cleanup(func() {
						httputil.CloseResponse(resp)
					})
				}
			})

			t.Run("jobs", func(t *testing.T) {
				jobsURL := fmt.Sprintf("%s/v1/warehouse/jobs", serverURL)
				req, err := http.NewRequest(http.MethodPost, jobsURL, bytes.NewReader([]byte(`
				{
				  "source_id": "test_source_id",
				  "destination_id": "test_destination_id",
				  "job_run_id": "test_source_job_run_id",
				  "task_run_id": "test_source_task_run_id",
				  "async_job_type": "deletebyjobrunid"
				}
			`)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			t.Run("jobs status", func(t *testing.T) {
				qp := url.Values{}
				qp.Add("task_run_id", sourceTaskRunID)
				qp.Add("job_run_id", sourceJobRunID)
				qp.Add("source_id", sourceID)
				qp.Add("destination_id", destinationID)
				qp.Add("workspace_id", workspaceID)

				jobsStatusURL := fmt.Sprintf("%s/v1/warehouse/jobs/status?"+qp.Encode(), serverURL)
				req, err := http.NewRequest(http.MethodGet, jobsStatusURL, nil)
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			t.Run("backfill", func(t *testing.T) {
				backfillURL := fmt.Sprintf("%s/v1/warehouse/backfill", serverURL)
				req, err := http.NewRequest(http.MethodPost, backfillURL, bytes.NewReader([]byte(`{
					"source_id": "test_source_id",
					"destination_id": "test_destination_id",
					"start_date": "2024-01-01T00:00:00Z",
					"end_date": "2024-01-15T00:00:00Z"
				}`)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			t.Run("health summary", func(t *testing.T) {
				healthURL := fmt.Sprintf("%s/v1/warehouse/health", serverURL)
				req, err := http.NewRequest(http.MethodGet, healthURL, nil)
				require.NoError(t, err)

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			t.Run("selective sync update", func(t *testing.T) {
				ssURL := fmt.Sprintf("%s/v1/warehouse/selective-sync", serverURL)
				req, err := http.NewRequest(http.MethodPut, ssURL, bytes.NewReader([]byte(`{
					"source_id": "test_source_id",
					"destination_id": "test_destination_id",
					"excluded_tables": ["table_a"]
				}`)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			t.Run("replay", func(t *testing.T) {
				replayURL := fmt.Sprintf("%s/v1/warehouse/replay", serverURL)
				req, err := http.NewRequest(http.MethodPost, replayURL, bytes.NewReader([]byte(`{
					"source_id": "test_source_id",
					"destination_id": "test_destination_id",
					"start_time": "2024-01-01T00:00:00Z",
					"end_time": "2024-01-15T00:00:00Z"
				}`)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				resp, err := (&http.Client{}).Do(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)

				defer func() {
					httputil.CloseResponse(resp)
				}()
			})

			stopServer()

			<-serverSetupCh
		})

		t.Run("degraded mode", func(t *testing.T) {
			webPort, err := kithelper.GetFreePort()
			require.NoError(t, err)

			c := config.New()
			c.Set("Warehouse.webPort", webPort)
			c.Set("Warehouse.runningMode", mode.DegradedMode)

			srvCtx, stopServer := context.WithCancel(ctx)

			a := NewApi(config.MasterMode, c, logger.NOP, stats.NOP, mockBackendConfig, db, n, tenantManager, bcManager, sourcesManager, triggerStore, nil, nil, nil, nil)

			serverSetupCh := make(chan struct{})
			go func() {
				require.NoError(t, a.Start(srvCtx))

				close(serverSetupCh)
			}()

			serverURL := fmt.Sprintf("http://localhost:%d", webPort)

			t.Run("health endpoint should work", func(t *testing.T) {
				health.WaitUntilReady(ctx, t, fmt.Sprintf("%s/health", serverURL), time.Second*10, time.Millisecond*100, t.Name())
			})

			t.Run("other endpoints should fail", func(t *testing.T) {
				testCases := []struct {
					name   string
					url    string
					method string
					body   io.Reader
				}{
					{
						name:   "process",
						url:    fmt.Sprintf("%s/v1/process", serverURL),
						method: http.MethodPost,
						body:   bytes.NewReader([]byte(`{}`)),
					},
					{
						name:   "pending events",
						url:    fmt.Sprintf("%s/v1/warehouse/pending-events", serverURL),
						method: http.MethodPost,
						body:   bytes.NewReader([]byte(`{}`)),
					},
					{
						name:   "trigger upload",
						url:    fmt.Sprintf("%s/v1/warehouse/trigger-upload", serverURL),
						method: http.MethodPost,
						body:   bytes.NewReader([]byte(`{}`)),
					},
					{
						name:   "jobs",
						url:    fmt.Sprintf("%s/v1/warehouse/jobs", serverURL),
						method: http.MethodPost,
						body:   bytes.NewReader([]byte(`{}`)),
					},
					{
						name:   "jobs status",
						url:    fmt.Sprintf("%s/v1/warehouse/jobs/status", serverURL),
						method: http.MethodGet,
						body:   nil,
					},
					{
						name:   "fetch tables",
						url:    fmt.Sprintf("%s/v1/warehouse/fetch-tables", serverURL),
						method: http.MethodGet,
						body:   nil,
					},
					{
						name:   "internal fetch tables",
						url:    fmt.Sprintf("%s/internal/v1/warehouse/fetch-tables", serverURL),
						method: http.MethodGet,
						body:   nil,
					},
					{
						name:   "backfill",
						url:    fmt.Sprintf("%s/v1/warehouse/backfill", serverURL),
						method: http.MethodPost,
						body:   bytes.NewReader([]byte(`{}`)),
					},
					{
						name:   "health summary",
						url:    fmt.Sprintf("%s/v1/warehouse/health", serverURL),
						method: http.MethodGet,
						body:   nil,
					},
					{
						name:   "selective sync update",
						url:    fmt.Sprintf("%s/v1/warehouse/selective-sync", serverURL),
						method: http.MethodPut,
						body:   bytes.NewReader([]byte(`{}`)),
					},
					{
						name:   "replay",
						url:    fmt.Sprintf("%s/v1/warehouse/replay", serverURL),
						method: http.MethodPost,
						body:   bytes.NewReader([]byte(`{}`)),
					},
				}

				for _, tc := range testCases {
					t.Run(tc.name, func(t *testing.T) {
						req, err := http.NewRequest(tc.method, tc.url, tc.body)
						require.NoError(t, err)

						resp, err := (&http.Client{}).Do(req)
						require.NoError(t, err)
						require.Equal(t, http.StatusNotFound, resp.StatusCode)

						defer func() {
							httputil.CloseResponse(resp)
						}()
					})
				}
			})

			stopServer()

			<-serverSetupCh
		})
	})

	stopTest()

	<-setupCh
}
