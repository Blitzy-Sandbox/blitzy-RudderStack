package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/chiware"
	"github.com/rudderlabs/rudder-go-kit/config"
	kithttputil "github.com/rudderlabs/rudder-go-kit/httputil"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/services/notifier"
	"github.com/rudderlabs/rudder-server/utils/crash"
	"github.com/rudderlabs/rudder-server/warehouse/bcm"
	sqlmw "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
	"github.com/rudderlabs/rudder-server/warehouse/internal/api"
	ierrors "github.com/rudderlabs/rudder-server/warehouse/internal/errors"
	"github.com/rudderlabs/rudder-server/warehouse/internal/mode"
	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
	"github.com/rudderlabs/rudder-server/warehouse/internal/repo"
	"github.com/rudderlabs/rudder-server/warehouse/internal/snapshots"
	lf "github.com/rudderlabs/rudder-server/warehouse/logfield"
	"github.com/rudderlabs/rudder-server/warehouse/multitenant"
	"github.com/rudderlabs/rudder-server/warehouse/source"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

const triggerUploadQPName = "triggerUpload"

type pendingEventsRequest struct {
	SourceID  string `json:"source_id"`
	TaskRunID string `json:"task_run_id"`
}

type pendingEventsResponse struct {
	PendingEvents            bool  `json:"pending_events"`
	PendingStagingFilesCount int64 `json:"pending_staging_files"`
	PendingUploadCount       int64 `json:"pending_uploads"`
	AbortedEvents            bool  `json:"aborted_events"`
}

type fetchTablesRequest struct {
	Connections []warehouseutils.SourceIDDestinationID `json:"connections"`
}

type fetchTablesResponse struct {
	ConnectionsTables []warehouseutils.FetchTableInfo `json:"connections_tables"`
}

type triggerUploadRequest struct {
	SourceID      string `json:"source_id"`
	DestinationID string `json:"destination_id"`
}

// backfillHandler defines the interface for backfill HTTP handlers (E-032).
// Implementations are injected from the warehouse/backfill package.
type backfillHandler interface {
	TriggerBackfill(w http.ResponseWriter, r *http.Request)
	GetBackfillStatus(w http.ResponseWriter, r *http.Request)
}

// healthMonitorHandler defines the interface for health monitoring HTTP handlers (E-033).
// Named healthMonitorHandler (not healthHandler) to avoid collision with the existing
// Api.healthHandler method used for standalone mode health checks.
type healthMonitorHandler interface {
	GetHealthSummary(w http.ResponseWriter, r *http.Request)
	GetHealthBySourceDest(w http.ResponseWriter, r *http.Request)
}

// selectiveSyncHandler defines the interface for selective sync HTTP handlers (E-034).
// Implementations are injected from the warehouse/selectivesync package.
type selectiveSyncHandler interface {
	UpdateSelectiveSync(w http.ResponseWriter, r *http.Request)
	GetSelectiveSync(w http.ResponseWriter, r *http.Request)
}

// replayHandler defines the interface for warehouse replay HTTP handlers (E-035).
// Implementations are injected from the warehouse/replay package.
type replayHandler interface {
	TriggerReplay(w http.ResponseWriter, r *http.Request)
	GetReplayStatus(w http.ResponseWriter, r *http.Request)
}

type Api struct {
	mode          string
	conf          *config.Config
	logger        logger.Logger
	statsFactory  stats.Stats
	db            *sqlmw.DB
	notifier      *notifier.Notifier
	bcConfig      backendconfig.BackendConfig
	tenantManager *multitenant.Manager
	bcManager     *bcm.BackendConfigManager
	sourceManager *source.Manager
	stagingRepo   *repo.StagingFiles
	uploadRepo    *repo.Uploads
	schemaRepo    *repo.WHSchema
	triggerStore  *sync.Map

	// Sprint 7-9 feature handlers — nil-safe; routes are only registered when non-nil.
	backfillH      backfillHandler       // E-032: Backfill API
	healthMonitorH healthMonitorHandler  // E-033: Health Monitoring API
	selectiveSyncH selectiveSyncHandler  // E-034: Selective Sync API
	replayH        replayHandler         // E-035: Warehouse Replay API

	config struct {
		healthTimeout       time.Duration
		readerHeaderTimeout time.Duration
		runningMode         string
		webPort             int
		mode                string
	}
}

func NewApi(
	mode string,
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
	bcConfig backendconfig.BackendConfig,
	db *sqlmw.DB,
	notifier *notifier.Notifier,
	tenantManager *multitenant.Manager,
	bcManager *bcm.BackendConfigManager,
	sourceManager *source.Manager,
	triggerStore *sync.Map,
	// Sprint 7-9 handler dependencies — pass nil when the feature is disabled.
	backfillH backfillHandler,
	healthMonitorH healthMonitorHandler,
	selectiveSyncH selectiveSyncHandler,
	replayH replayHandler,
) *Api {
	a := &Api{
		mode:           mode,
		conf:           conf,
		logger:         log.Child("api"),
		db:             db,
		notifier:       notifier,
		bcConfig:       bcConfig,
		statsFactory:   statsFactory,
		tenantManager:  tenantManager,
		bcManager:      bcManager,
		sourceManager:  sourceManager,
		triggerStore:   triggerStore,
		backfillH:      backfillH,
		healthMonitorH: healthMonitorH,
		selectiveSyncH: selectiveSyncH,
		replayH:        replayH,
		stagingRepo:    repo.NewStagingFiles(db, conf, repo.WithStats(statsFactory)),
		uploadRepo:     repo.NewUploads(db, repo.WithStats(statsFactory)),
		schemaRepo:     repo.NewWHSchemas(db, conf, log, repo.WithStats(statsFactory)),
	}
	a.config.healthTimeout = conf.GetDuration("Warehouse.healthTimeout", 10, time.Second)
	a.config.readerHeaderTimeout = conf.GetDuration("Warehouse.readerHeaderTimeout", 3, time.Second)
	a.config.runningMode = conf.GetString("Warehouse.runningMode", "")
	a.config.webPort = conf.GetInt("Warehouse.webPort", 8082)
	return a
}

func (a *Api) Start(ctx context.Context) error {
	srvMux := chi.NewRouter()
	srvMux.Use(
		chiware.StatMiddleware(ctx, a.statsFactory, "warehouse"),
	)

	if mode.IsStandAlone(a.mode) {
		srvMux.Get("/health", a.healthHandler)
	}
	if !mode.IsDegraded(a.config.runningMode) {
		if mode.IsMaster(a.mode) {
			a.addMasterEndpoints(ctx, srvMux)

			a.logger.Infon("Starting warehouse master service",
				logger.NewIntField("port", int64(a.config.webPort)),
			)
		} else {
			a.logger.Infon("Starting warehouse slave service",
				logger.NewIntField("port", int64(a.config.webPort)),
			)
		}
	}

	srv := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(a.config.webPort)),
		Handler:           crash.Handler(srvMux),
		ReadHeaderTimeout: a.config.readerHeaderTimeout,
	}
	return kithttputil.ListenAndServe(ctx, srv)
}

func (a *Api) addMasterEndpoints(ctx context.Context, r chi.Router) {
	a.logger.Infon("waiting for BackendConfig before starting",
		logger.NewIntField("port", int64(a.config.webPort)),
	)

	a.bcConfig.WaitForConfig(ctx)

	stagingFileSchemaSnapshotTTL := a.conf.GetDurationVar(3, time.Hour, "Warehouse.stagingFileSchemaSnapshotTTL")
	stagingFileSchemaSnapshots := snapshots.NewStagingFileSchema(
		a.conf,
		repo.NewStagingFileSchemaSnapshots(a.db, repo.WithStats(a.statsFactory)),
		snapshots.NewStagingFileSchemaTimeBasedExpiryStrategy(stagingFileSchemaSnapshotTTL),
	)
	schemaSnapshotHandler := &api.StagingFileSchemaSnapshotHandler{
		Snapshots: stagingFileSchemaSnapshots,
		PatchGen:  warehouseutils.GenerateJSONPatch,
	}

	r.Handle("/v1/process", (&api.WarehouseAPI{
		Logger:                a.logger,
		Stats:                 a.statsFactory,
		Repo:                  a.stagingRepo,
		Multitenant:           a.tenantManager,
		SchemaSnapshotHandler: schemaSnapshotHandler,
	}).Handler())

	r.Route("/v1", func(r chi.Router) {
		r.Route("/warehouse", func(r chi.Router) {
			r.Post("/pending-events", a.logMiddleware(a.pendingEventsHandler))
			r.Post("/trigger-upload", a.logMiddleware(a.triggerUploadHandler))

			r.Post("/jobs", a.logMiddleware(a.sourceManager.InsertJobHandler))       // TODO: add degraded mode
			r.Get("/jobs/status", a.logMiddleware(a.sourceManager.StatusJobHandler)) // TODO: add degraded mode

			r.Get("/fetch-tables", a.logMiddleware(a.fetchTablesHandler)) // TODO: Remove this endpoint once sources change is released

			// E-032: Backfill endpoints — trigger historical data sync for a date range.
			if a.backfillH != nil {
				r.Post("/backfill", a.logMiddleware(a.backfillH.TriggerBackfill))
				r.Get("/backfill/{jobID}", a.logMiddleware(a.backfillH.GetBackfillStatus))
			}

			// E-033: Health monitoring endpoints — per-upload and aggregate sync health.
			if a.healthMonitorH != nil {
				r.Get("/health", a.logMiddleware(a.healthMonitorH.GetHealthSummary))
				r.Get("/health/{sourceID}/{destID}", a.logMiddleware(a.healthMonitorH.GetHealthBySourceDest))
			}

			// E-034: Selective sync endpoints — per-table/per-column inclusion/exclusion.
			if a.selectiveSyncH != nil {
				r.Put("/selective-sync", a.logMiddleware(a.selectiveSyncH.UpdateSelectiveSync))
				r.Get("/selective-sync/{sourceID}/{destID}", a.logMiddleware(a.selectiveSyncH.GetSelectiveSync))
			}

			// E-035: Warehouse replay endpoints — replay archived events to warehouse.
			if a.replayH != nil {
				r.Post("/replay", a.logMiddleware(a.replayH.TriggerReplay))
				r.Get("/replay/{jobID}", a.logMiddleware(a.replayH.GetReplayStatus))
			}
		})
	})
	r.Route("/internal", func(r chi.Router) {
		r.Route("/v1", func(r chi.Router) {
			r.Route("/warehouse", func(r chi.Router) {
				r.Get("/fetch-tables", a.logMiddleware(a.fetchTablesHandler))
			})
		})
	})
}

func (a *Api) healthHandler(w http.ResponseWriter, r *http.Request) {
	var dbService, notifierService string

	ctx, cancel := context.WithTimeout(r.Context(), a.config.healthTimeout)
	defer cancel()

	if !mode.IsDegraded(a.config.runningMode) {
		if !a.notifier.CheckHealth(ctx) {
			a.logger.Warnn("notifier service is not healthy")
			http.Error(w, "Cannot connect to notifierService", http.StatusInternalServerError)
			return
		}
		notifierService = "UP"
	}

	if mode.IsMaster(a.mode) {
		if !checkHealth(ctx, a.db.DB) {
			a.logger.Warnn("dbService is not healthy")
			http.Error(w, "Cannot connect to dbService", http.StatusInternalServerError)
			return
		}
		dbService = "UP"
	}

	healthVal := fmt.Sprintf(`{
		"server": "UP",
		"db": %q,
		"notifier": %q,
		"acceptingEvents": "TRUE",
		"warehouseMode": %q
	}`,
		dbService,
		notifierService,
		strings.ToUpper(a.mode),
	)

	_, _ = w.Write([]byte(healthVal))
}

func checkHealth(ctx context.Context, db *sql.DB) bool {
	if db == nil {
		return false
	}

	healthCheckMsg := "Rudder Warehouse DB Health Check"
	msg := ""

	err := db.QueryRowContext(ctx, `SELECT '`+healthCheckMsg+`'::text as message;`).Scan(&msg)
	if err != nil {
		return false
	}

	return healthCheckMsg == msg
}

// pendingEventsHandler check whether there are any pending staging files or uploads for the given source id
func (a *Api) pendingEventsHandler(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	var payload pendingEventsRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&payload); err != nil {
		a.logger.Warnn("invalid JSON in request body for pending events", obskit.Error(err))
		http.Error(w, ierrors.ErrInvalidJSONRequestBody.Error(), http.StatusBadRequest)
		return
	}

	sourceID, taskRunID := payload.SourceID, payload.TaskRunID
	if sourceID == "" || taskRunID == "" {
		a.logger.Warnn("empty source or task run id for pending events",
			logger.NewStringField(lf.SourceID, payload.SourceID),
			logger.NewStringField(lf.TaskRunID, payload.TaskRunID),
		)
		http.Error(w, "empty source or task run id", http.StatusBadRequest)
		return
	}

	workspaceID, err := a.tenantManager.SourceToWorkspace(r.Context(), sourceID)
	if err != nil {
		a.logger.Warnn("workspace from source not found for pending events", logger.NewStringField(lf.SourceID, payload.SourceID))
		http.Error(w, ierrors.ErrWorkspaceFromSourceNotFound.Error(), http.StatusBadRequest)
		return
	}

	if a.tenantManager.DegradedWorkspace(workspaceID) {
		a.logger.Infon("workspace is degraded for pending events", logger.NewStringField(lf.WorkspaceID, workspaceID))
		http.Error(w, ierrors.ErrWorkspaceDegraded.Error(), http.StatusServiceUnavailable)
		return
	}

	pendingStagingFileCount, err := a.stagingRepo.CountPendingForSource(r.Context(), sourceID)
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			http.Error(w, ierrors.ErrRequestCancelled.Error(), http.StatusBadRequest)
			return
		}
		a.logger.Errorn("counting pending staging files", obskit.Error(err))
		http.Error(w, "can't get pending staging files count", http.StatusInternalServerError)
		return
	}

	filters := []repo.FilterBy{
		{Key: "source_id", Value: sourceID},
		{Key: "metadata->>'source_task_run_id'", Value: taskRunID},
		{Key: "status", NotEquals: true, Value: model.ExportedData},
		{Key: "status", NotEquals: true, Value: model.Aborted},
	}
	pendingUploadCount, err := a.uploadRepo.Count(r.Context(), filters...)
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			http.Error(w, ierrors.ErrRequestCancelled.Error(), http.StatusBadRequest)
			return
		}
		a.logger.Errorn("counting pending uploads", obskit.Error(err))
		http.Error(w, "can't get pending uploads count", http.StatusInternalServerError)
		return
	}

	filters = []repo.FilterBy{
		{Key: "source_id", Value: sourceID},
		{Key: "metadata->>'source_task_run_id'", Value: payload.TaskRunID},
		{Key: "status", Value: "aborted"},
	}
	abortedUploadCount, err := a.uploadRepo.Count(r.Context(), filters...)
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			http.Error(w, ierrors.ErrRequestCancelled.Error(), http.StatusBadRequest)
			return
		}
		a.logger.Errorn("counting aborted uploads", obskit.Error(err))
		http.Error(w, "can't get aborted uploads count", http.StatusInternalServerError)
		return
	}

	pendingEventsAvailable := (pendingStagingFileCount + pendingUploadCount) > 0
	triggerPendingUpload, _ := strconv.ParseBool(r.URL.Query().Get(triggerUploadQPName))

	if pendingEventsAvailable && triggerPendingUpload {
		a.logger.Infon("triggering upload for all destinations connected to source",
			logger.NewStringField(lf.WorkspaceID, workspaceID),
			logger.NewStringField(lf.SourceID, payload.SourceID),
		)

		wh := a.bcManager.WarehousesBySourceID(sourceID)
		if len(wh) == 0 {
			a.logger.Warnn("no warehouse found for pending events",
				logger.NewStringField(lf.WorkspaceID, workspaceID),
				logger.NewStringField(lf.SourceID, payload.SourceID),
			)
			http.Error(w, ierrors.ErrNoWarehouseFound.Error(), http.StatusBadRequest)
			return
		}

		for _, warehouse := range wh {
			a.triggerStore.Store(warehouse.Identifier, struct{}{})
		}
	}

	resBody, err := jsonrs.Marshal(pendingEventsResponse{
		PendingEvents:            pendingEventsAvailable,
		PendingStagingFilesCount: pendingStagingFileCount,
		PendingUploadCount:       pendingUploadCount,
		AbortedEvents:            abortedUploadCount > 0,
	})
	if err != nil {
		a.logger.Errorn("marshalling response for pending events", obskit.Error(err))
		http.Error(w, ierrors.ErrMarshallResponse.Error(), http.StatusInternalServerError)
		return
	}

	_, _ = w.Write(resBody)
}

func (a *Api) triggerUploadHandler(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	var payload triggerUploadRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&payload); err != nil {
		a.logger.Warnn("invalid JSON in request body for triggering upload", obskit.Error(err))
		http.Error(w, ierrors.ErrInvalidJSONRequestBody.Error(), http.StatusBadRequest)
		return
	}

	workspaceID, err := a.tenantManager.SourceToWorkspace(r.Context(), payload.SourceID)
	if err != nil {
		a.logger.Warnn("workspace from source not found for triggering upload", logger.NewStringField(lf.SourceID, payload.SourceID))
		http.Error(w, ierrors.ErrWorkspaceFromSourceNotFound.Error(), http.StatusBadRequest)
		return
	}

	if a.tenantManager.DegradedWorkspace(workspaceID) {
		a.logger.Infon("workspace is degraded for triggering upload", logger.NewStringField(lf.WorkspaceID, workspaceID))
		http.Error(w, ierrors.ErrWorkspaceDegraded.Error(), http.StatusServiceUnavailable)
		return
	}

	var wh []model.Warehouse
	if payload.SourceID != "" && payload.DestinationID == "" {
		wh = a.bcManager.WarehousesBySourceID(payload.SourceID)
	} else if payload.DestinationID != "" {
		wh = a.bcManager.WarehousesByDestID(payload.DestinationID)
	}
	if len(wh) == 0 {
		a.logger.Warnn("no warehouse found for triggering upload",
			logger.NewStringField(lf.WorkspaceID, workspaceID),
			logger.NewStringField(lf.SourceID, payload.SourceID),
			logger.NewStringField(lf.DestinationID, payload.DestinationID),
		)
		http.Error(w, ierrors.ErrNoWarehouseFound.Error(), http.StatusBadRequest)
		return
	}

	for _, warehouse := range wh {
		a.triggerStore.Store(warehouse.Identifier, struct{}{})
	}

	w.WriteHeader(http.StatusOK)
}

func (a *Api) fetchTablesHandler(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	var payload fetchTablesRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&payload); err != nil {
		a.logger.Warnn("invalid JSON in request body for fetching tables", obskit.Error(err))
		http.Error(w, ierrors.ErrInvalidJSONRequestBody.Error(), http.StatusBadRequest)
		return
	}

	tables, err := a.schemaRepo.GetTablesForConnection(r.Context(), payload.Connections)
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			http.Error(w, ierrors.ErrRequestCancelled.Error(), http.StatusBadRequest)
			return
		}
		a.logger.Errorn("fetching tables", obskit.Error(err))
		http.Error(w, "can't fetch tables", http.StatusInternalServerError)
		return
	}

	resBody, err := jsonrs.Marshal(fetchTablesResponse{
		ConnectionsTables: tables,
	})
	if err != nil {
		a.logger.Errorn("marshalling response for fetching tables", obskit.Error(err))
		http.Error(w, ierrors.ErrMarshallResponse.Error(), http.StatusInternalServerError)
		return
	}

	_, _ = w.Write(resBody)
}

func (a *Api) logMiddleware(delegate http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.logger.LogRequest(r)
		delegate.ServeHTTP(w, r)
	}
}
