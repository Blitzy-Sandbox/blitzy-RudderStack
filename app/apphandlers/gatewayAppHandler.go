package apphandlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rudderlabs/rudder-schemas/go/stream"

	"golang.org/x/sync/errgroup"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/app"
	"github.com/rudderlabs/rudder-server/app/cluster"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	functionsapi "github.com/rudderlabs/rudder-server/functions/api"
	functionsruntime "github.com/rudderlabs/rudder-server/functions/runtime"
	functionssecrets "github.com/rudderlabs/rudder-server/functions/secrets"
	functionsstorage "github.com/rudderlabs/rudder-server/functions/storage"
	"github.com/rudderlabs/rudder-server/gateway"
	gwThrottler "github.com/rudderlabs/rudder-server/gateway/throttler"
	identitygraph "github.com/rudderlabs/rudder-server/identity/graph"
	identityprofiles "github.com/rudderlabs/rudder-server/identity/profiles"
	identitystorage "github.com/rudderlabs/rudder-server/identity/storage"
	drain_config "github.com/rudderlabs/rudder-server/internal/drain-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	protocolsapi "github.com/rudderlabs/rudder-server/protocols/api"
	protocolsstorage "github.com/rudderlabs/rudder-server/protocols/storage"
	sourcedebugger "github.com/rudderlabs/rudder-server/services/debugger/source"
	"github.com/rudderlabs/rudder-server/services/monitoring"
	"github.com/rudderlabs/rudder-server/services/transformer"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/utils/types/deployment"
)

// gatewayApp is the type for Gateway type implementation
type gatewayApp struct {
	setupDone      bool
	app            app.App
	versionHandler func(w http.ResponseWriter, r *http.Request)
	log            logger.Logger
}

func (a *gatewayApp) Setup() error {
	if err := rudderCoreDBValidator(); err != nil {
		return err
	}
	if err := rudderCoreNodeSetup(); err != nil {
		return err
	}
	a.setupDone = true
	return nil
}

func (a *gatewayApp) StartRudderCore(ctx context.Context, _ func(), options *app.Options) error {
	config := config.Default
	statsFactory := stats.Default
	if !a.setupDone {
		return fmt.Errorf("gateway cannot start, database is not setup")
	}
	a.log.Infon("Gateway starting")

	deploymentType, err := deployment.GetFromEnv()
	if err != nil {
		return fmt.Errorf("failed to get deployment type: %v", err)
	}

	a.log.Infon("Configured deployment type", logger.NewStringField("deploymentType", string(deploymentType)))
	a.log.Infon("Clearing DB", logger.NewBoolField("clearDB", options.ClearDB))

	sourceHandle, err := sourcedebugger.NewHandle(backendconfig.DefaultBackendConfig)
	if err != nil {
		return err
	}
	defer sourceHandle.Stop()

	var jobsdbPool *sql.DB
	if config.GetBoolVar(true, "DB.gateway.Pool.enabled", "DB.Pool.enabled") {
		jobsdbPool, err = misc.NewDatabaseConnectionPool(ctx, "gateway", misc.DatabaseConnectionPoolConfig{
			MaxOpenConns:    config.GetReloadableIntVar(20, 1, "DB.gateway.Pool.maxOpenConnections", "DB.Pool.maxOpenConnections"),
			MaxIdleConns:    config.GetReloadableIntVar(5, 1, "DB.gateway.Pool.maxIdleConnections", "DB.Pool.maxIdleConnections"),
			ConnMaxIdleTime: config.GetReloadableDurationVar(15, time.Minute, "DB.gateway.Pool.maxIdleTime", "DB.Pool.maxIdleTime"),
			ConnMaxLifetime: config.GetReloadableDurationVar(0, time.Second, "DB.gateway.Pool.maxConnLifetime", "DB.Pool.maxConnLifetime"),
			UpdateInterval:  config.GetDurationVar(60, time.Second, "DB.gateway.Pool.updateInterval", "DB.Pool.updateInterval"),
		}, config, statsFactory)
		if err != nil {
			return err
		}
		defer jobsdbPool.Close()
	}
	partitionCount := config.GetIntVar(0, 1, "JobsDB.partitionCount")

	gwWOHandle := jobsdb.NewForWrite(
		"gw",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithSkipMaintenanceErr(config.GetBool("Gateway.jobsDB.skipMaintenanceError", true)),
		jobsdb.WithStats(statsFactory),
		jobsdb.WithDBHandle(jobsdbPool),
		jobsdb.WithNumPartitions(partitionCount),
	)
	defer gwWOHandle.Close()
	var gwWODB jobsdb.JobsDB = gwWOHandle

	if err := gwWODB.Start(); err != nil {
		return fmt.Errorf("could not start gatewayDB: %w", err)
	}
	defer func() {
		// wrapping Stop call in an anonymous function
		// so that we can decorate gwWODB later for partition migrations
		// and call Stop on the decorated instance
		gwWODB.Stop()
	}()

	g, ctx := errgroup.WithContext(ctx)

	modeProvider, err := resolveModeProvider(a.log, deploymentType)
	if err != nil {
		return fmt.Errorf("resolving mode provider: %w", err)
	}
	partitionMigrator, gwDB, err := setupGatewayPartitionMigrator(ctx, jobsdbPool, config, statsFactory, gwWODB, modeProvider.EtcdClient)
	if err != nil {
		return fmt.Errorf("setting up partition migrator: %w", err)
	}
	gwWODB = gwDB
	if err := partitionMigrator.Start(); err != nil {
		return fmt.Errorf("starting partition migrator: %w", err)
	}
	defer partitionMigrator.Stop()

	dm := cluster.Dynamic{Provider: modeProvider, GatewayComponent: true}
	g.Go(func() error {
		return dm.Run(ctx)
	})

	var gw gateway.Handle
	rateLimiter, err := gwThrottler.New(statsFactory)
	if err != nil {
		return fmt.Errorf("failed to create rate limiter: %w", err)
	}
	rsourcesService, err := NewRsourcesService(deploymentType, false, statsFactory)
	if err != nil {
		return err
	}
	transformerFeaturesService := transformer.NewFeaturesService(ctx, config, transformer.FeaturesServiceOptions{
		PollInterval:             config.GetDuration("Transformer.pollInterval", 10, time.Second),
		TransformerURL:           config.GetString("DEST_TRANSFORM_URL", "http://localhost:9090"),
		FeaturesRetryMaxAttempts: 10,
	})
	drainConfigManager, err := drain_config.NewDrainConfigManager(config, a.log.Child("drain-config"), statsFactory)
	if err != nil {
		a.log.Errorn("drain config manager setup failed while starting gateway", obskit.Error(err))
	}

	drainConfigHttpHandler := drain_config.ErrorResponder("unable to start drain config http handler")
	if drainConfigManager != nil {
		defer drainConfigManager.Stop()
		drainConfigHttpHandler = drainConfigManager.DrainConfigHttpHandler()
	}
	streamMsgValidator := stream.NewMessageValidator()

	// Create and start the monitoring dashboard service (E-036).
	// Only requires config and logger — no database dependency.
	monitoringDashboard := monitoring.NewDashboardService(config, a.log)
	if err := monitoringDashboard.Start(ctx); err != nil {
		return fmt.Errorf("monitoring dashboard start: %v", err)
	}
	defer monitoringDashboard.Stop()
	// Use a chi.Router instead of http.ServeMux for the monitoring sub-router.
	// chi.Mount strips the route prefix via rctx.RoutePath, but http.ServeMux
	// reads r.URL.Path directly (which retains the full path), causing a 404.
	// A chi.Router correctly reads rctx.RoutePath for prefix-stripped matching.
	monitoringRouter := chi.NewRouter()
	monitoringRouter.Get("/dashboard", monitoringDashboard.DashboardHandler)

	// Build the internal HTTP handlers map with all feature API routers.
	internalHandlers := map[string]http.Handler{
		"/drain":         drainConfigHttpHandler,
		"/v1/monitoring": monitoringRouter,
	}

	// Wire Functions management CRUD API (E-018) and Functions Secrets API (E-019).
	// Requires a database connection for persistence. The Functions runtime engine
	// handles test-invocation of user-defined functions via the /test endpoint.
	if jobsdbPool != nil {
		gwLog := a.log.Child("functions")
		fnRepo := functionsstorage.New(jobsdbPool, gwLog)
		fnRuntime := functionsruntime.New(config, gwLog, statsFactory)
		fnSecrets := functionssecrets.New(config, gwLog, jobsdbPool)
		internalHandlers["/v1/functions"] = functionsapi.NewRouter(gwLog, fnRepo, fnRuntime, fnSecrets)
		a.log.Infon("Functions API wired into gateway internal handlers")
	}

	// Wire Protocols / Tracking Plan management API (E-024).
	// The protocolsapi.Service adapts the storage repository to the
	// TrackingPlanService interface expected by the HTTP handler.
	if jobsdbPool != nil {
		tpRepo := protocolsstorage.NewRepository(jobsdbPool)
		tpService := protocolsapi.NewService(tpRepo)
		tpHandler := protocolsapi.NewHandler(a.log.Child("protocols"), tpService)
		internalHandlers["/v1/protocols"] = protocolsapi.NewRouter(tpHandler)
		a.log.Infon("Protocols API wired into gateway internal handlers")
	}

	// Wire Profiles REST API (E-027). Requires identity graph service backed
	// by PostgreSQL. Cache is nil (NoopCache) — direct graph queries are used.
	if jobsdbPool != nil {
		idRepo := identitystorage.NewPostgresRepository(jobsdbPool, a.log.Child("identity-storage"))
		graphSvc := identitygraph.NewService(idRepo, config, a.log.Child("identity-graph"), statsFactory)
		profilesHandler, profilesErr := identityprofiles.NewHandler(graphSvc, nil, config, a.log.Child("profiles"), statsFactory)
		if profilesErr != nil {
			a.log.Warnn("Failed to create profiles handler — Profiles API will not be available", obskit.Error(profilesErr))
		} else {
			internalHandlers["/v1/profiles"] = profilesHandler.Routes()
			a.log.Infon("Profiles API wired into gateway internal handlers")
		}
	}

	err = gw.Setup(ctx, config, logger.NewLogger().Child("gateway"), statsFactory, a.app, backendconfig.DefaultBackendConfig,
		gwWODB, rateLimiter, a.versionHandler, rsourcesService, transformerFeaturesService, sourceHandle,
		streamMsgValidator, gateway.WithInternalHttpHandlers(internalHandlers))
	if err != nil {
		return fmt.Errorf("failed to setup gateway: %w", err)
	}
	defer func() {
		if err := gw.Shutdown(); err != nil {
			a.log.Warnn("Gateway shutdown error", obskit.Error(err))
		}
	}()

	g.Go(func() error {
		return gw.StartWebHandler(ctx)
	})
	return g.Wait()
}
