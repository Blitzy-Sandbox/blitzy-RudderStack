package runner

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	_ "go.uber.org/automaxprocs"
	"golang.org/x/sync/errgroup"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/filemanager"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	_ "github.com/rudderlabs/rudder-go-kit/maxprocs"
	"github.com/rudderlabs/rudder-go-kit/profiler"
	"github.com/rudderlabs/rudder-go-kit/stats"
	svcMetric "github.com/rudderlabs/rudder-go-kit/stats/metric"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/admin"
	"github.com/rudderlabs/rudder-server/app"
	"github.com/rudderlabs/rudder-server/app/apphandlers"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	functionsruntime "github.com/rudderlabs/rudder-server/functions/runtime"
	identitygraph "github.com/rudderlabs/rudder-server/identity/graph"
	identitystorage "github.com/rudderlabs/rudder-server/identity/storage"
	"github.com/rudderlabs/rudder-server/info"
	"github.com/rudderlabs/rudder-server/router/customdestinationmanager"
	"github.com/rudderlabs/rudder-server/rruntime"
	"github.com/rudderlabs/rudder-server/services/alert"
	"github.com/rudderlabs/rudder-server/services/alerting"
	"github.com/rudderlabs/rudder-server/services/controlplane"
	"github.com/rudderlabs/rudder-server/services/diagnostics"
	"github.com/rudderlabs/rudder-server/services/monitoring"
	"github.com/rudderlabs/rudder-server/services/streammanager/kafka"
	"github.com/rudderlabs/rudder-server/utils/crash"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/utils/types/deployment"
	"github.com/rudderlabs/rudder-server/warehouse"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
	"github.com/rudderlabs/rudder-server/warehouse/validations"
)

// ReleaseInfo holds the release information
type ReleaseInfo struct {
	Version         string
	Commit          string
	BuildDate       string
	BuiltBy         string
	EnterpriseToken string
}

// Runner is responsible for running the application
type Runner struct {
	appType                   string
	application               app.App
	releaseInfo               ReleaseInfo
	warehouseMode             string
	warehouseApp              *warehouse.App
	enableSuppressUserFeature bool
	logger                    logger.Logger
	appHandler                apphandlers.AppHandler
	gracefulShutdownTimeout   time.Duration

	// New service instances for feature expansion (Sprint 4-10).
	// Each field remains nil when its feature flag is disabled, ensuring
	// zero overhead and full backward compatibility with existing behaviour.
	functionsRuntime  *functionsruntime.Engine // Functions runtime engine (E-015/E-016/E-017)
	identityService   identitygraph.Service    // Identity graph service (E-026)
	identityDB        *sql.DB                  // Database pool for identity graph — closed on shutdown
	monitoringService *monitoring.Dashboard    // Monitoring dashboard (E-036)
	alertingEngine    *alerting.Engine         // Alerting rules engine (E-037)
}

// New creates and initializes a new Runner
func New(releaseInfo ReleaseInfo) *Runner {
	return &Runner{
		appType:                   strings.ToUpper(config.GetString("APP_TYPE", app.EMBEDDED)),
		releaseInfo:               releaseInfo,
		logger:                    logger.NewLogger().Child("runner"),
		warehouseMode:             config.GetString("Warehouse.mode", "embedded"),
		enableSuppressUserFeature: config.GetBool("Gateway.enableSuppressUserFeature", true),
		gracefulShutdownTimeout:   config.GetDuration("GracefulShutdownTimeout", 15, time.Second),
	}
}

// Run runs the application and returns the exit code
func (r *Runner) Run(ctx context.Context, shutdownFn func(), args []string) int {
	// Start stats
	deploymentType, err := deployment.GetFromEnv()
	if err != nil {
		r.logger.Errorn("failed to get deployment type", obskit.Error(err))
		return 1
	}

	path, err := config.Default.ConfigFileUsed()
	if err != nil {
		r.logger.Warnn("Config: Failed to parse config file from path, using default values",
			logger.NewStringField("path", path),
			obskit.Error(err))
	} else {
		r.logger.Infon("Config: Using config file",
			logger.NewStringField("path", path))
	}

	if err := config.Default.DotEnvLoaded(); err != nil {
		r.logger.Infon("Config: No .env file loaded", obskit.Error(err))
	} else {
		r.logger.Infon("Config: Loaded .env file")
	}

	// TODO: remove as soon as we update the configuration with statsExcludedTags where necessary
	if !config.IsSet("statsExcludedTags") && deploymentType == deployment.MultiTenantType &&
		(!config.IsSet("WORKSPACE_NAMESPACE") || strings.Contains(config.GetString("WORKSPACE_NAMESPACE", ""), "free")) {
		config.Set("statsExcludedTags", []string{"workspaceId", "sourceID", "destId"})
	}
	statsOptions := []stats.Option{
		stats.WithServiceName(r.appType),
		stats.WithServiceVersion(r.releaseInfo.Version),
	}
	if r.canStartWarehouse() {
		statsOptions = append(statsOptions, stats.WithDefaultHistogramBuckets(defaultWarehouseHistogramBuckets))
		for histogramName, buckets := range customBucketsWarehouse {
			statsOptions = append(statsOptions, stats.WithHistogramBuckets(histogramName, buckets))
		}
	} else {
		statsOptions = append(statsOptions, stats.WithDefaultHistogramBuckets(defaultHistogramBuckets))
		for histogramName, buckets := range customBucketsServer {
			statsOptions = append(statsOptions, stats.WithHistogramBuckets(histogramName, buckets))
		}
	}
	for histogramName, buckets := range customBuckets {
		statsOptions = append(statsOptions, stats.WithHistogramBuckets(histogramName, buckets))
	}
	stats.Default = stats.NewStats(config.Default, logger.Default, svcMetric.Instance, statsOptions...)
	if err := stats.Default.Start(ctx, rruntime.GoRoutineFactory); err != nil {
		r.logger.Errorn("Failed to start stats", obskit.Error(err))
		return 1
	}

	runAllInit()

	options := app.LoadOptions(args)
	if options.VersionFlag {
		r.printVersion()
		return 0
	}

	options.EnterpriseToken = r.releaseInfo.EnterpriseToken

	r.application = app.New(options)

	// application & backend setup should be done before starting any new goroutines.
	r.application.Setup()

	r.appHandler, err = apphandlers.GetAppHandler(r.application, r.appType, r.versionHandler)
	if err != nil {
		r.logger.Errorn("Failed to get app handler", obskit.Error(err))
		return 1
	}

	crash.Configure(r.logger, crash.PanicWrapperOpts{
		ReleaseStage: config.GetString("GO_ENV", "development"),
		AppType:      fmt.Sprintf("rudder-server-%s", r.appType),
		AppVersion:   r.releaseInfo.Version,
	})
	defer crash.Notify("Core")()

	stats.Default.NewTaggedStat("rudder_server_config",
		stats.GaugeType,
		stats.Tags{
			"version":   r.releaseInfo.Version,
			"commit":    r.releaseInfo.Commit,
			"buildDate": r.releaseInfo.BuildDate,
			"builtBy":   r.releaseInfo.BuiltBy,
		}).Gauge(1)

	configEnvHandler := r.application.Features().ConfigEnv.Setup()

	if err := backendconfig.Setup(configEnvHandler); err != nil {
		r.logger.Errorn("Unable to setup backend config", obskit.Error(err))
		return 1
	}
	backendconfig.DefaultBackendConfig.StartWithIDs(ctx, "")

	// Initialize new services before database preparation.
	// CRITICAL: These services must initialize BEFORE the Processor starts
	// to ensure pipeline hooks are available (AAP Section 0.4.1).
	// Each service is gated by canStartServer() AND its own config flag,
	// defaulting to disabled for full backward compatibility (AAP Section 0.7.6).

	// Initialize Functions runtime (E-015: Source Functions, E-016: Destination Functions, E-017: Insert Functions)
	if r.canStartServer() && config.GetBool("Functions.enabled", false) {
		r.functionsRuntime = functionsruntime.NewEngine(
			config.Default,
			logger.NewLogger().Child("functions"),
			stats.Default,
		)
		r.logger.Infon("Functions runtime initialized")
	}

	// Initialize Identity service (E-026: Real-time identity graph).
	// sql.Open creates a lazy database pool — no TCP connection is established
	// until the first query, which happens in Run() after r.appHandler.Setup()
	// has completed schema migrations. This avoids the timing issue where
	// NewService is called before database setup.
	if r.canStartServer() && config.GetBool("Identity.enabled", false) {
		identityDSN := misc.GetConnectionString(config.Default, "identity")
		identityDB, err := sql.Open("postgres", identityDSN)
		if err != nil {
			r.logger.Errorn("Failed to create identity database pool", obskit.Error(err))
		} else {
			r.identityDB = identityDB
			identityRepo := identitystorage.NewPostgresRepository(
				identityDB,
				logger.NewLogger().Child("identity.storage"),
			)
			r.identityService = identitygraph.NewService(
				identityRepo,
				config.Default,
				logger.NewLogger().Child("identity"),
				stats.Default,
			)
			r.logger.Infon("Identity service initialized with PostgreSQL repository")
		}
	}

	// Initialize Monitoring dashboard (E-036: Per-destination delivery metrics).
	// Gated by Monitoring.dashboard.enabled matching config.yaml keys.
	if r.canStartServer() && config.GetBool("Monitoring.dashboard.enabled", false) {
		r.monitoringService = monitoring.NewDashboard(
			config.Default,
			logger.NewLogger().Child("monitoring"),
			stats.Default,
		)
		r.logger.Infon("Monitoring dashboard initialized")
	}

	// Initialize Alerting engine (E-037: Configurable alerting rules).
	// Independently gated by Monitoring.alerting.enabled so alerting can be
	// enabled/disabled without affecting the monitoring dashboard.
	if r.canStartServer() && config.GetBool("Monitoring.alerting.enabled", false) {
		r.alertingEngine = alerting.NewEngine(
			config.Default,
			logger.NewLogger().Child("alerting"),
			stats.Default,
		)
		r.logger.Infon("Alerting engine initialized")
	}

	// Prepare databases in sequential order, so that failure in one doesn't affect others (leaving dirty schema migration state)
	if r.canStartServer() {
		if err := r.appHandler.Setup(); err != nil {
			r.logger.Errorn("Unable to prepare rudder-core database", obskit.Error(err))
			return 1
		}
	}
	if r.canStartWarehouse() {
		r.warehouseApp = warehouse.New(
			r.application,
			config.Default,
			r.logger,
			stats.Default,
			backendconfig.DefaultBackendConfig,
			filemanager.New,
		)

		if err := r.warehouseApp.Setup(ctx); err != nil {
			r.logger.Errorn("Unable to prepare warehouse database", obskit.Error(err))
			return 1
		}
	}
	g, ctx := errgroup.WithContext(ctx)

	// Start admin server
	if config.GetBool("AdminServer.enabled", true) {
		g.Go(func() error {
			if err := admin.StartServer(ctx); err != nil {
				return fmt.Errorf("admin server routine: %w", err)
			}
			return nil
		})
	}

	if config.GetBool("Profiler.Enabled", true) {
		g.Go(func() error {
			return profiler.StartServer(ctx, config.GetInt("Profiler.Port", 7777))
		})
	}

	// Start rudder core
	if r.canStartServer() {
		g.Go(crash.Wrapper(func() (err error) {
			if err := r.appHandler.StartRudderCore(ctx, shutdownFn, options); err != nil {
				return fmt.Errorf("rudder core: %w", err)
			}
			return nil
		}))
		g.Go(crash.Wrapper(func() error {
			backendconfig.DefaultBackendConfig.WaitForConfig(ctx)

			c := controlplane.NewClient(
				config.GetString("CONFIG_BACKEND_URL", "https://api.rudderstack.com"),
				backendconfig.DefaultBackendConfig.Identity(),
			)

			err := c.SendFeatures(ctx, info.ServerComponent.Name, info.ServerComponent.Features)
			if err != nil {
				r.logger.Errorn("error sending server features", obskit.Error(err))
			}

			// we don't want to exit if we can't send server features
			return nil
		}))
	}

	// Start new services in errgroup (Sprint 4-10).
	// Each service is nil-guarded: only started when its respective config flag was
	// enabled and canStartServer() was true during initialization above.

	// Start Functions runtime (E-015: Source Functions, E-016: Destination Functions, E-017: Insert Functions)
	if r.functionsRuntime != nil {
		g.Go(crash.Wrapper(func() error {
			if err := r.functionsRuntime.Run(ctx); err != nil {
				return fmt.Errorf("functions runtime: %w", err)
			}
			return nil
		}))
	}

	// Start Identity service (E-026: Real-time identity graph)
	if r.identityService != nil {
		g.Go(crash.Wrapper(func() error {
			if err := r.identityService.Run(ctx); err != nil {
				return fmt.Errorf("identity service: %w", err)
			}
			return nil
		}))
	}

	// Start Monitoring dashboard (E-036: Per-destination delivery metrics)
	if r.monitoringService != nil {
		g.Go(crash.Wrapper(func() error {
			if err := r.monitoringService.Run(ctx); err != nil {
				return fmt.Errorf("monitoring dashboard: %w", err)
			}
			return nil
		}))
	}

	// Start Alerting engine (E-037: Configurable alerting rules)
	if r.alertingEngine != nil {
		g.Go(crash.Wrapper(func() error {
			if err := r.alertingEngine.Run(ctx); err != nil {
				return fmt.Errorf("alerting engine: %w", err)
			}
			return nil
		}))
	}

	// Start warehouse
	// initialize warehouse service after core to handle non-normal recovery modes
	if r.canStartWarehouse() {
		g.Go(crash.NotifyWarehouse(func() error {
			if err := r.warehouseApp.Run(ctx); err != nil {
				return fmt.Errorf("warehouse service routine: %w", err)
			}
			return nil
		}))
	}

	shutdownDone := make(chan struct{})
	go func() {
		err := g.Wait()
		if err != nil {
			r.logger.Errorn("Terminal error", obskit.Error(err))
		}

		r.logger.Infon("Attempting to shutdown gracefully")
		backendconfig.DefaultBackendConfig.Stop()

		// Stop new services (Sprint 4-10) in reverse initialization order.
		// Each stop is nil-guarded to be safe when the service was not configured.
		if r.alertingEngine != nil {
			r.alertingEngine.Stop()
		}
		if r.monitoringService != nil {
			r.monitoringService.Stop()
		}
		if r.identityService != nil {
			r.identityService.Stop()
		}
		if r.identityDB != nil {
			_ = r.identityDB.Close()
		}
		if r.functionsRuntime != nil {
			r.functionsRuntime.Stop()
		}

		close(shutdownDone)
	}()

	<-ctx.Done()
	ctxDoneTime := time.Now()

	select {
	case <-shutdownDone:
		r.application.Stop()
		r.logger.Infon("Graceful termination after, with go-routines",
			logger.NewDurationField("duration", time.Since(ctxDoneTime)),
			logger.NewIntField("goroutines", int64(runtime.NumGoroutine())),
		)
		// clearing zap Log buffer to std output
		logger.Sync()
		stats.Default.Stop()
	case <-time.After(r.gracefulShutdownTimeout):
		// Assume graceful shutdown failed, log remain goroutines and force kill
		r.logger.Errorn("Graceful termination failed after, goroutine dump:",
			logger.NewDurationField("duration", time.Since(ctxDoneTime)),
		)

		fmt.Print("\n\n")
		_ = pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
		fmt.Print("\n\n")

		r.application.Stop()
		logger.Sync()
		stats.Default.Stop()
		if config.GetBool("RUDDER_GRACEFUL_SHUTDOWN_TIMEOUT_EXIT", true) {
			return 1
		}
	}

	return 0
}

func runAllInit() {
	admin.Init()
	misc.Init()
	diagnostics.Init()
	backendconfig.Init()
	warehouseutils.Init()
	validations.Init()
	kafka.Init()
	customdestinationmanager.Init()
	alert.Init()
}

func (r *Runner) versionInfo() map[string]any {
	return map[string]any{
		"Version":   r.releaseInfo.Version,
		"Commit":    r.releaseInfo.Commit,
		"BuildDate": r.releaseInfo.BuildDate,
		"BuiltBy":   r.releaseInfo.BuiltBy,
		"Features":  info.ServerComponent.Features,
	}
}

func (r *Runner) versionHandler(w http.ResponseWriter, _ *http.Request) {
	version := r.versionInfo()
	versionFormatted, _ := jsonrs.Marshal(&version)
	_, _ = w.Write(versionFormatted)
}

func (r *Runner) printVersion() {
	version := r.versionInfo()
	versionFormatted, _ := jsonrs.MarshalIndent(&version, "", " ")
	fmt.Printf("Version Info %s\n", versionFormatted)
}

func (r *Runner) canStartServer() bool {
	r.logger.Infon("warehousemode",
		logger.NewStringField("mode", r.warehouseMode))
	return r.warehouseMode == config.EmbeddedMode || r.warehouseMode == config.OffMode || r.warehouseMode == config.EmbeddedMasterMode
}

func (r *Runner) canStartWarehouse() bool {
	return r.appType != app.GATEWAY && r.warehouseMode != config.OffMode
}
