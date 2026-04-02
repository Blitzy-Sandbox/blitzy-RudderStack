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
	"github.com/rudderlabs/rudder-server/archiver"
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
	"github.com/rudderlabs/rudder-server/internal/pulsar"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/jobsdb/bench"
	"github.com/rudderlabs/rudder-server/processor"
	protocolsapi "github.com/rudderlabs/rudder-server/protocols/api"
	protocolsstorage "github.com/rudderlabs/rudder-server/protocols/storage"
	"github.com/rudderlabs/rudder-server/router"
	"github.com/rudderlabs/rudder-server/router/batchrouter"
	routerManager "github.com/rudderlabs/rudder-server/router/manager"
	rtThrottler "github.com/rudderlabs/rudder-server/router/throttler"
	schema_forwarder "github.com/rudderlabs/rudder-server/schema-forwarder"
	destinationdebugger "github.com/rudderlabs/rudder-server/services/debugger/destination"
	sourcedebugger "github.com/rudderlabs/rudder-server/services/debugger/source"
	transformationdebugger "github.com/rudderlabs/rudder-server/services/debugger/transformation"
	"github.com/rudderlabs/rudder-server/services/alerting"
	"github.com/rudderlabs/rudder-server/services/fileuploader"
	"github.com/rudderlabs/rudder-server/services/monitoring"
	"github.com/rudderlabs/rudder-server/services/profiling"
	"github.com/rudderlabs/rudder-server/services/rmetrics"
	"github.com/rudderlabs/rudder-server/services/transformer"
	"github.com/rudderlabs/rudder-server/services/transientsource"
	"github.com/rudderlabs/rudder-server/utils/crash"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/utils/payload"
	"github.com/rudderlabs/rudder-server/utils/types"
	"github.com/rudderlabs/rudder-server/utils/types/deployment"

	"github.com/redis/go-redis/v9"
)

// embeddedApp is the type for embedded type implementation
type embeddedApp struct {
	setupDone      bool
	app            app.App
	versionHandler func(w http.ResponseWriter, r *http.Request)
	log            logger.Logger
	config         struct {
		eschDSLimit    config.ValueLoader[int]
		arcDSLimit     config.ValueLoader[int]
		rtDSLimit      config.ValueLoader[int]
		batchrtDSLimit config.ValueLoader[int]
		gwDSLimit      config.ValueLoader[int]
	}
}

func (a *embeddedApp) Setup() error {
	a.config.gwDSLimit = config.GetReloadableIntVar(0, 1, "JobsDB.gw.dsLimit", "Gateway.jobsDB.dsLimit", "JobsDB.dsLimit")
	a.config.rtDSLimit = config.GetReloadableIntVar(0, 1, "JobsDB.rt.dsLimit", "Router.jobsDB.dsLimit", "JobsDB.dsLimit")
	a.config.batchrtDSLimit = config.GetReloadableIntVar(0, 1, "JobsDB.batch_rt.dsLimit", "BatchRouter.jobsDB.dsLimit", "JobsDB.dsLimit")
	a.config.eschDSLimit = config.GetReloadableIntVar(0, 1, "JobsDB.esch.dsLimit", "Processor.jobsDB.dsLimit", "JobsDB.dsLimit")
	a.config.arcDSLimit = config.GetReloadableIntVar(0, 1, "JobsDB.arc.dsLimit", "Processor.jobsDB.dsLimit", "JobsDB.dsLimit")
	if err := rudderCoreDBValidator(); err != nil {
		return err
	}
	if err := rudderCoreNodeSetup(); err != nil {
		return err
	}
	a.setupDone = true
	return nil
}

func (a *embeddedApp) StartRudderCore(ctx context.Context, shutdownFn func(), options *app.Options) error {
	config := config.Default
	statsFactory := stats.Default

	if !a.setupDone {
		return fmt.Errorf("embedded rudder core cannot start, database is not setup")
	}
	a.log.Infon("Embedded mode: Starting Rudder Core")
	g, ctx := errgroup.WithContext(ctx)
	terminalErrFn := terminalErrorFunction(ctx, g)

	deploymentType, err := deployment.GetFromEnv()
	if err != nil {
		return fmt.Errorf("failed to get deployment type: %w", err)
	}
	a.log.Infon("Configured deployment type", logger.NewStringField("deploymentType", string(deploymentType)))

	trackedUsersReporter, err := a.app.Features().TrackedUsers.Setup(config)
	if err != nil {
		return fmt.Errorf("could not setup tracked users: %w", err)
	}
	err = trackedUsersReporter.MigrateDatabase(misc.GetConnectionString(config, "tracked_users"), config)
	if err != nil {
		return fmt.Errorf("could not run tracked users database migration: %w", err)
	}
	reporting := a.app.Features().Reporting.Setup(ctx, config, backendconfig.DefaultBackendConfig)
	defer reporting.Stop()
	syncer := reporting.DatabaseSyncer(types.SyncerConfig{ConnInfo: misc.GetConnectionString(config, "reporting")})
	g.Go(func() error {
		syncer()
		return nil
	})

	a.log.Infon("Clearing DB", logger.NewBoolField("clearDB", options.ClearDB))

	transformationhandle, err := transformationdebugger.NewHandle(backendconfig.DefaultBackendConfig)
	if err != nil {
		return err
	}
	defer transformationhandle.Stop()
	destinationHandle, err := destinationdebugger.NewHandle(backendconfig.DefaultBackendConfig)
	if err != nil {
		return err
	}
	defer destinationHandle.Stop()
	sourceHandle, err := sourcedebugger.NewHandle(backendconfig.DefaultBackendConfig)
	if err != nil {
		return err
	}
	defer sourceHandle.Stop()

	transientSources := transientsource.NewService(ctx, backendconfig.DefaultBackendConfig)

	fileUploaderProvider := fileuploader.NewProvider(ctx, backendconfig.DefaultBackendConfig)

	rsourcesService, err := NewRsourcesService(deploymentType, true, statsFactory)
	if err != nil {
		return err
	}

	transformerFeaturesService := transformer.NewFeaturesService(ctx, config, transformer.FeaturesServiceOptions{
		PollInterval:             config.GetDuration("Transformer.pollInterval", 10, time.Second),
		TransformerURL:           config.GetString("DEST_TRANSFORM_URL", "http://localhost:9090"),
		FeaturesRetryMaxAttempts: 10,
	})

	var (
		jobsdbPool   *sql.DB
		priorityPool *sql.DB
	)
	if config.GetBoolVar(true, "DB.embedded.Pool.enabled", "DB.Pool.enabled") {
		jobsdbPool, err = misc.NewDatabaseConnectionPool(ctx, "embedded", misc.DatabaseConnectionPoolConfig{
			MaxOpenConns:    config.GetReloadableIntVar(80, 1, "DB.embedded.Pool.maxOpenConnections", "DB.Pool.maxOpenConnections"),
			MaxIdleConns:    config.GetReloadableIntVar(10, 1, "DB.embedded.Pool.maxIdleConnections", "DB.Pool.maxIdleConnections"),
			ConnMaxIdleTime: config.GetReloadableDurationVar(15, time.Minute, "DB.embedded.Pool.maxIdleTime", "DB.Pool.maxIdleTime"),
			ConnMaxLifetime: config.GetReloadableDurationVar(0, time.Second, "DB.embedded.Pool.maxConnLifetime", "DB.Pool.maxConnLifetime"),
			UpdateInterval:  config.GetDurationVar(60, time.Second, "DB.embedded.Pool.updateInterval", "DB.Pool.updateInterval"),
		}, config, statsFactory)
		if err != nil {
			return err
		}
		defer jobsdbPool.Close()
	}
	if config.GetBoolVar(false, "DB.embedded.PriorityPool.enabled", "DB.PriorityPool.enabled", "PartitionMigration.enabled") {
		priorityPool, err = misc.NewDatabaseConnectionPool(ctx, "ep", misc.DatabaseConnectionPoolConfig{
			MaxOpenConns:    config.GetReloadableIntVar(10, 1, "DB.embedded.PriorityPool.maxOpenConnections", "DB.PriorityPool.maxOpenConnections"),
			MaxIdleConns:    config.GetReloadableIntVar(1, 1, "DB.embedded.PriorityPool.maxIdleConnections", "DB.PriorityPool.maxIdleConnections"),
			ConnMaxIdleTime: config.GetReloadableDurationVar(15, time.Minute, "DB.embedded.PriorityPool.maxIdleTime", "DB.PriorityPool.maxIdleTime"),
			ConnMaxLifetime: config.GetReloadableDurationVar(0, time.Second, "DB.embedded.PriorityPool.maxConnLifetime", "DB.PriorityPool.maxConnLifetime"),
			UpdateInterval:  config.GetDurationVar(60, time.Second, "DB.embedded.PriorityPool.updateInterval", "DB.PriorityPool.updateInterval"),
		}, config, statsFactory)
		if err != nil {
			return err
		}
		defer priorityPool.Close()
	}
	partitionCount := config.GetIntVar(0, 1, "JobsDB.partitionCount")

	pendingEventsRegistry := rmetrics.NewPendingEventsRegistry()

	// This separate gateway db is created just to be used with gateway because in case of degraded mode,
	// the earlier created gwDb (which was created to be used mainly with processor) will not be running, and it
	// will cause issues for gateway because gateway is supposed to receive jobs even in degraded mode.
	gwWOHandle := jobsdb.NewForWrite(
		"gw",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithStats(statsFactory),
		jobsdb.WithDBHandle(jobsdbPool),
		jobsdb.WithNumPartitions(partitionCount),
		jobsdb.WithPriorityPoolDB(priorityPool),
	)
	defer gwWOHandle.Close()
	if err = gwWOHandle.Start(); err != nil {
		return fmt.Errorf("could not start gateway: %w", err)
	}
	defer gwWOHandle.Stop()
	var gwWODB jobsdb.JobsDB = gwWOHandle

	gwROHandle := jobsdb.NewForRead(
		"gw",
		jobsdb.WithDSLimit(a.config.gwDSLimit),
		jobsdb.WithSkipMaintenanceErr(config.GetBool("Gateway.jobsDB.skipMaintenanceError", true)),
		jobsdb.WithStats(statsFactory),
		jobsdb.WithDBHandle(jobsdbPool),
		jobsdb.WithPriorityPoolDB(priorityPool),
		jobsdb.WithNumPartitions(partitionCount),
	)
	defer gwROHandle.Close()
	var gwRODB jobsdb.JobsDB = gwROHandle

	rtRWHandle := jobsdb.NewForReadWrite(
		"rt",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithDSLimit(a.config.rtDSLimit),
		jobsdb.WithSkipMaintenanceErr(config.GetBool("Router.jobsDB.skipMaintenanceError", false)),
		jobsdb.WithStats(statsFactory),
		jobsdb.WithDBHandle(jobsdbPool),
		jobsdb.WithPriorityPoolDB(priorityPool),
		jobsdb.WithNumPartitions(partitionCount),
	)
	defer rtRWHandle.Close()
	rtRWDB := jobsdb.NewPendingEventsJobsDB(rtRWHandle, pendingEventsRegistry)

	brtRWHandle := jobsdb.NewForReadWrite(
		"batch_rt",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithDSLimit(a.config.batchrtDSLimit),
		jobsdb.WithSkipMaintenanceErr(config.GetBool("BatchRouter.jobsDB.skipMaintenanceError", false)),
		jobsdb.WithStats(statsFactory),
		jobsdb.WithDBHandle(jobsdbPool),
		jobsdb.WithPriorityPoolDB(priorityPool),
		jobsdb.WithNumPartitions(partitionCount),
	)
	defer brtRWHandle.Close()
	brtRWDB := jobsdb.NewPendingEventsJobsDB(brtRWHandle, pendingEventsRegistry)

	eschRWDB := jobsdb.NewForReadWrite(
		"esch",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithDSLimit(a.config.eschDSLimit),
		jobsdb.WithSkipMaintenanceErr(config.GetBool("Processor.jobsDB.skipMaintenanceError", false)),
		jobsdb.WithStats(statsFactory),
		jobsdb.WithDBHandle(jobsdbPool),
	)
	defer eschRWDB.Close()

	arcRWDB := jobsdb.NewForReadWrite(
		"arc",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithDSLimit(a.config.arcDSLimit),
		jobsdb.WithSkipMaintenanceErr(config.GetBool("Processor.jobsDB.skipMaintenanceError", false)),
		jobsdb.WithStats(statsFactory),
		jobsdb.WithJobMaxAge(config.GetReloadableDurationVar(24, time.Hour, "archival.jobRetention")),
		jobsdb.WithDBHandle(jobsdbPool),
	)
	defer arcRWDB.Close()

	var schemaForwarder schema_forwarder.Forwarder
	if config.GetBool("EventSchemas2.enabled", false) {
		client, err := pulsar.NewClient(config)
		if err != nil {
			return err
		}
		defer client.Close()
		schemaForwarder = schema_forwarder.NewForwarder(terminalErrFn, eschRWDB, &client, backendconfig.DefaultBackendConfig, logger.NewLogger().Child("jobs_forwarder"), config, statsFactory)
	} else {
		schemaForwarder = schema_forwarder.NewAbortingForwarder(terminalErrFn, eschRWDB, logger.NewLogger().Child("jobs_forwarder"), config, statsFactory)
	}

	modeProvider, err := resolveModeProvider(a.log, deploymentType)
	if err != nil {
		return err
	}

	// setup partition migrator
	ppmSetup, err := setupProcessorPartitionMigrator(ctx, shutdownFn, jobsdbPool, priorityPool,
		config, statsFactory,
		gwRODB, gwWODB,
		rtRWDB, brtRWDB,
		modeProvider.EtcdClient,
	)
	defer ppmSetup.Finally() // always run finally to clean up resources regardless of error
	if err != nil {
		return fmt.Errorf("setting up partition migrator: %w", err)
	}
	partitionMigrator := ppmSetup.PartitionMigrator
	gwWODB = ppmSetup.GwDB
	rtRWDB = ppmSetup.RtDB
	brtRWDB = ppmSetup.BrtDB

	adaptiveLimit := payload.SetupAdaptiveLimiter(ctx, g)

	enrichers, err := setupPipelineEnrichers(config, a.log, statsFactory)
	if err != nil {
		return fmt.Errorf("setting up pipeline enrichers: %w", err)
	}

	defer func() {
		for _, enricher := range enrichers {
			_ = enricher.Close()
		}
	}()

	proc := processor.New(
		ctx,
		&options.ClearDB,
		gwRODB,
		rtRWDB,
		brtRWDB,
		eschRWDB,
		arcRWDB,
		reporting,
		transientSources,
		fileUploaderProvider,
		rsourcesService,
		transformerFeaturesService,
		destinationHandle,
		transformationhandle,
		enrichers,
		trackedUsersReporter,
		pendingEventsRegistry,
		processor.WithAdaptiveLimit(adaptiveLimit),
	)
	routerLogger := logger.NewLogger().Child("router")
	throttlerFactory, err := rtThrottler.NewFactory(config, statsFactory, routerLogger.Child("throttler"))
	if err != nil {
		return fmt.Errorf("failed to create rt throttler factory: %w", err)
	}
	rtFactory := &router.Factory{
		Logger:        routerLogger,
		Reporting:     reporting,
		BackendConfig: backendconfig.DefaultBackendConfig,
		RouterDB: jobsdb.NewCachingDistinctParameterValuesJobsdb( // using a cache so that multiple routers can share the same cache and not hit the DB every time
			config.GetReloadableDurationVar(1, time.Second, "JobsDB.rt.parameterValuesCacheTtl", "JobsDB.parameterValuesCacheTtl"),
			rtRWDB,
		),
		TransientSources:           transientSources,
		RsourcesService:            rsourcesService,
		TransformerFeaturesService: transformerFeaturesService,
		ThrottlerFactory:           throttlerFactory,
		Debugger:                   destinationHandle,
		AdaptiveLimit:              adaptiveLimit,
	}
	brtFactory := &batchrouter.Factory{
		Reporting:     reporting,
		BackendConfig: backendconfig.DefaultBackendConfig,
		RouterDB: jobsdb.NewCachingDistinctParameterValuesJobsdb( // using a cache so that multiple batch routers can share the same cache and not hit the DB every time
			config.GetReloadableDurationVar(1, time.Second, "JobsDB.rt.parameterValuesCacheTtl", "JobsDB.parameterValuesCacheTtl"),
			brtRWDB,
		),
		TransientSources: transientSources,
		RsourcesService:  rsourcesService,
		Debugger:         destinationHandle,
		AdaptiveLimit:    adaptiveLimit,
	}
	rt := routerManager.New(rtFactory, brtFactory, backendconfig.DefaultBackendConfig, logger.NewLogger())

	dm := cluster.Dynamic{
		Provider:          modeProvider,
		GatewayDB:         gwRODB,
		RouterDB:          rtRWDB,
		BatchRouterDB:     brtRWDB,
		EventSchemaDB:     eschRWDB,
		ArchivalDB:        arcRWDB,
		PartitionMigrator: partitionMigrator,
		Processor:         proc,
		Router:            rt,
		SchemaForwarder:   schemaForwarder,
		Archiver: archiver.New(
			arcRWDB,
			fileUploaderProvider,
			config,
			statsFactory,
			archiver.WithAdaptiveLimit(adaptiveLimit),
		),
	}

	rateLimiter, err := gwThrottler.New(statsFactory)
	if err != nil {
		return fmt.Errorf("failed to create gw rate limiter: %w", err)
	}
	drainConfigManager, err := drain_config.NewDrainConfigManager(config, a.log.Child("drain-config"), statsFactory)
	if err != nil {
		return fmt.Errorf("drain config manager setup: %v", err)
	}
	defer drainConfigManager.Stop()
	g.Go(crash.Wrapper(func() (err error) {
		return drainConfigManager.DrainConfigRoutine(ctx)
	}))
	g.Go(crash.Wrapper(func() (err error) {
		return drainConfigManager.CleanupRoutine(ctx)
	}))
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
		"/drain":         drainConfigManager.DrainConfigHttpHandler(),
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

	// Wire Profiles REST and gRPC APIs (E-027). Requires identity graph service
	// backed by PostgreSQL with Redis caching for sub-200ms responses under
	// production load. The gRPC server is started alongside the REST API to
	// provide high-performance inter-service communication.
	var profilesGRPCSrv *identityprofiles.GRPCServer
	if jobsdbPool != nil {
		idRepo := identitystorage.NewPostgresRepository(jobsdbPool, a.log.Child("identity-storage"))
		graphSvc := identitygraph.NewService(idRepo, config, a.log.Child("identity-graph"), statsFactory)

		// Create Redis client for profile caching (E-027).
		// Redis address is read from Identity.redis.address config key with
		// fallback to localhost:6379 matching docker-compose.yml.
		var profileCache identityprofiles.ProfileCache
		redisAddr := config.GetString("Identity.redis.address", "localhost:6379")
		redisDB := config.GetInt("Identity.redis.db", 0)
		redisPassword := config.GetString("Identity.redis.password", "")
		redisPoolSize := config.GetInt("Identity.redis.poolSize", 10)
		redisClient := redis.NewClient(&redis.Options{
			Addr:     redisAddr,
			Password: redisPassword,
			DB:       redisDB,
			PoolSize: redisPoolSize,
		})
		profileCache = identityprofiles.NewRedisProfileCache(redisClient, config, a.log.Child("profiles-cache"))
		a.log.Infon("Redis profile cache created",
			logger.NewStringField("addr", redisAddr),
			logger.NewIntField("db", int64(redisDB)),
		)

		profilesHandler, profilesErr := identityprofiles.NewHandler(graphSvc, profileCache, config, a.log.Child("profiles"), statsFactory)
		if profilesErr != nil {
			a.log.Warnn("Failed to create profiles handler — Profiles API will not be available", obskit.Error(profilesErr))
		} else {
			internalHandlers["/v1/profiles"] = profilesHandler.Routes()
			a.log.Infon("Profiles API wired into gateway internal handlers")
		}

		// Wire Profiles gRPC server (E-027) for high-performance inter-service
		// communication. Uses the same graph.Service as the REST handler.
		var grpcErr error
		profilesGRPCSrv, grpcErr = identityprofiles.NewGRPCServer(graphSvc, config, a.log.Child("profiles"))
		if grpcErr != nil {
			a.log.Warnn("Failed to create profiles gRPC server — gRPC Profiles API will not be available", obskit.Error(grpcErr))
			profilesGRPCSrv = nil
		}
	}

	// Wire Pipeline Profiling API (E-039). Exposes /pipeline and /capacity
	// sub-endpoints for runtime pipeline performance profiling and capacity planning.
	{
		profilingProfiler := profiling.NewProfiler()
		profilingCapacity := profiling.NewCapacityPlanner(profilingProfiler)
		profilingRouter := chi.NewRouter()
		profilingRouter.Get("/pipeline", profilingProfiler.Handler())
		profilingRouter.Get("/capacity", profilingCapacity.Handler())
		internalHandlers["/v1/profiling"] = profilingRouter
		a.log.Infon("Profiling API wired into gateway internal handlers")
	}

	// Wire Alerting Rules API (E-037). Exposes /rules CRUD sub-endpoints for
	// alert rule management. The AlertEngine is created with config and logger
	// only — the rule repository, metric collector, and notification channels
	// are wired separately. When no rule repository is available, CRUD endpoints
	// return 503 Service Unavailable gracefully.
	{
		alertEngine := alerting.NewAlertEngine(config, a.log.Child("alerting"), nil, nil, nil)
		internalHandlers["/v1/alerts"] = alertEngine.Handler()
		a.log.Infon("Alerting API wired into gateway internal handlers")
	}

	gw := gateway.Handle{}
	err = gw.Setup(ctx, config, logger.NewLogger().Child("gateway"), statsFactory, a.app, backendconfig.DefaultBackendConfig,
		gwWODB, rateLimiter, a.versionHandler, rsourcesService, transformerFeaturesService, sourceHandle,
		streamMsgValidator, gateway.WithInternalHttpHandlers(internalHandlers))
	if err != nil {
		return fmt.Errorf("could not setup gateway: %w", err)
	}
	defer func() {
		if err := gw.Shutdown(); err != nil {
			a.log.Warnn("Gateway shutdown error", obskit.Error(err))
		}
	}()

	g.Go(func() error {
		return gw.StartWebHandler(ctx)
	})

	// Start Profiles gRPC server (E-027) for high-performance inter-service
	// communication. Runs alongside the REST API on a separate TCP port
	// (default 50051, configurable via Identity.Profiles.gRPC.port).
	if profilesGRPCSrv != nil {
		g.Go(func() error {
			return profilesGRPCSrv.Start(ctx)
		})
	}

	g.Go(func() error {
		// This should happen only after setupDatabaseTables() is called and journal table migrations are done
		// because if this start before that then there might be a case when ReadDB will try to read the owner table
		// which gets created after either Write or ReadWrite DB is created.
		return dm.Run(ctx)
	})

	g.Go(func() error {
		return rsourcesService.CleanupLoop(ctx)
	})

	g.Go(func() error {
		replicationLagStat := statsFactory.NewStat("rsources_log_replication_lag", stats.GaugeType)
		replicationSlotStat := statsFactory.NewStat("rsources_log_replication_slot", stats.GaugeType)
		rsourcesService.Monitor(ctx, replicationLagStat, replicationSlotStat)
		return nil
	})

	if config.GetBool("JobsDB.Bench.enabled", false) {
		g.Go(func() error {
			b, err := bench.New(config, statsFactory, a.log.Child("jobsdb.benchmark"), jobsdbPool)
			if err != nil {
				return fmt.Errorf("creating jobsdb benchmarker: %w", err)
			}
			return b.Run(ctx)
		})
	}
	return g.Wait()
}
