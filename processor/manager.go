package processor

import (
	"context"
	"sync"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/enterprise/trackedusers"
	functionsruntime "github.com/rudderlabs/rudder-server/functions/runtime"
	"github.com/rudderlabs/rudder-server/internal/enricher"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/processor/transformer"
	destinationdebugger "github.com/rudderlabs/rudder-server/services/debugger/destination"
	transformationdebugger "github.com/rudderlabs/rudder-server/services/debugger/transformation"
	"github.com/rudderlabs/rudder-server/services/fileuploader"
	"github.com/rudderlabs/rudder-server/services/rmetrics"
	"github.com/rudderlabs/rudder-server/services/rsources"
	transformerFeaturesService "github.com/rudderlabs/rudder-server/services/transformer"
	"github.com/rudderlabs/rudder-server/services/transientsource"
	"github.com/rudderlabs/rudder-server/utils/types"
)

type LifecycleManager struct {
	Handle                     *Handle
	mainCtx                    context.Context
	currentCancel              context.CancelFunc
	waitGroup                  interface{ Wait() }
	gatewayDB                  jobsdb.JobsDB
	routerDB                   jobsdb.JobsDB
	batchRouterDB              jobsdb.JobsDB
	esDB                       jobsdb.JobsDB
	arcDB                      jobsdb.JobsDB
	clearDB                    *bool
	ReportingI                 types.Reporting // need not initialize again
	BackendConfig              backendconfig.BackendConfig
	TransformerClients         *transformer.Clients
	transientSources           transientsource.Service
	fileuploader               fileuploader.Provider
	rsourcesService            rsources.JobService
	transformerFeaturesService transformerFeaturesService.FeaturesService
	destDebugger               destinationdebugger.DestinationDebugger
	transDebugger              transformationdebugger.TransformationDebugger
	enrichers                  []enricher.PipelineEnricher
	trackedUsersReporter       trackedusers.UsersReporter
	pendingEventsRegistry      rmetrics.PendingEventsRegistry
}

// Start starts a processor, this is not a blocking call.
// If the processor is not completely started and the data started coming then also it will not be problematic as we
// are assuming that the DBs will be up.
func (proc *LifecycleManager) Start() error {
	if proc.TransformerClients != nil {
		proc.Handle.transformerClients = proc.TransformerClients
	}
	currentCtx, cancel := context.WithCancel(context.Background())
	if err := proc.Handle.Setup(
		currentCtx,
		proc.BackendConfig,
		proc.gatewayDB,
		proc.routerDB,
		proc.batchRouterDB,
		proc.esDB,
		proc.arcDB,
		proc.ReportingI,
		proc.transientSources,
		proc.fileuploader,
		proc.rsourcesService,
		proc.transformerFeaturesService,
		proc.destDebugger,
		proc.transDebugger,
		proc.enrichers,
		proc.trackedUsersReporter,
		proc.pendingEventsRegistry,
	); err != nil {
		cancel()
		return err
	}

	proc.currentCancel = cancel
	var wg sync.WaitGroup
	proc.waitGroup = &wg

	wg.Go(func() {
		if err := proc.Handle.countPendingEvents(currentCtx); err != nil {
			proc.Handle.logger.Errorn("Error counting pending events", obskit.Error(err))
		}
	})

	wg.Go(func() {
		if err := proc.Handle.Start(currentCtx); err != nil {
			proc.Handle.logger.Errorn("Error starting processor", obskit.Error(err))
		}
	})
	return nil
}

// Stop stops the processor, this is a blocking call.
func (proc *LifecycleManager) Stop() {
	proc.currentCancel()
	proc.waitGroup.Wait()
	proc.Handle.Shutdown()
}

// New creates a new Processor instance
func New(
	ctx context.Context,
	clearDb *bool,
	gwDb, rtDb, brtDb, esDB, arcDB jobsdb.JobsDB,
	reporting types.Reporting,
	transientSources transientsource.Service,
	fileuploader fileuploader.Provider,
	rsourcesService rsources.JobService,
	transformerFeaturesService transformerFeaturesService.FeaturesService,
	destDebugger destinationdebugger.DestinationDebugger,
	transDebugger transformationdebugger.TransformationDebugger,
	enrichers []enricher.PipelineEnricher,
	trackedUsersReporter trackedusers.UsersReporter,
	pendingEventsRegistry rmetrics.PendingEventsRegistry,
	opts ...Opts,
) *LifecycleManager {
	proc := &LifecycleManager{
		Handle: NewHandle(
			config.Default,
			transformer.NewClients(
				config.Default,
				logger.NewLogger().Child("processor"),
				stats.Default,
				transformer.WithFeatureService(transformerFeaturesService),
			),
		),
		mainCtx:                    ctx,
		gatewayDB:                  gwDb,
		routerDB:                   rtDb,
		batchRouterDB:              brtDb,
		esDB:                       esDB,
		arcDB:                      arcDB,
		clearDB:                    clearDb,
		BackendConfig:              backendconfig.DefaultBackendConfig,
		ReportingI:                 reporting,
		transientSources:           transientSources,
		fileuploader:               fileuploader,
		rsourcesService:            rsourcesService,
		transformerFeaturesService: transformerFeaturesService,
		destDebugger:               destDebugger,
		transDebugger:              transDebugger,
		enrichers:                  enrichers,
		trackedUsersReporter:       trackedUsersReporter,
		pendingEventsRegistry:      pendingEventsRegistry,
	}
	for _, opt := range opts {
		opt(proc)
	}
	return proc
}

type Opts func(l *LifecycleManager)

func WithAdaptiveLimit(adaptiveLimitFunction func(int64) int64) Opts {
	return func(l *LifecycleManager) {
		l.Handle.adaptiveLimit = adaptiveLimitFunction
	}
}

func WithStats(stats stats.Stats) Opts {
	return func(l *LifecycleManager) {
		l.Handle.statsFactory = stats
	}
}

func WithTransformerClients(transformerClients transformer.TransformerClients) Opts {
	return func(l *LifecycleManager) {
		l.Handle.transformerClients = transformerClients
	}
}

// WithAnomalyDetector injects the anomaly detector into the processor (E-021).
func WithAnomalyDetector(d anomalyDetector) Opts {
	return func(l *LifecycleManager) {
		l.Handle.anomalyDetector = d
	}
}

// WithEnforcementForwarder injects the enforcement forwarder into the processor (E-023).
func WithEnforcementForwarder(f enforcementForwarder) Opts {
	return func(l *LifecycleManager) {
		l.Handle.enforcementForwarder = f
	}
}

// WithIdentityResolver injects the identity resolver into the processor (E-026).
func WithIdentityResolver(r identityResolver) Opts {
	return func(l *LifecycleManager) {
		l.Handle.identityResolver = r
	}
}

// WithPipelineProfiler injects the pipeline profiler into the processor (E-039).
// The profiler records per-stage latencies for performance profiling.
func WithPipelineProfiler(p pipelineProfiler) Opts {
	return func(l *LifecycleManager) {
		l.Handle.pipelineProfiler = p
	}
}

// WithFunctionsRuntime injects the Functions runtime engine into the processor,
// enabling Source Functions (E-015), Destination Functions (E-016), and Insert
// Functions (E-017) pipeline stages.
//
// This option performs three critical wirings:
//
//  1. Sets Handle.functionsEnabled = true so the functionsEnabled guard in
//     processor.go (line ~3699 and ~4328) allows Function pipeline stages to execute.
//
//  2. Creates an insertFunctionEngineAdapter wrapping the engine and assigns it to
//     Handle.insertFnExecutor so the Insert Functions stage (line ~4366) has a
//     non-nil executor.
//
//  3. Rebuilds the transformer clients to include a functionsClientAdapter as the
//     FunctionsClient, so proc.transformerClients.Functions() (line ~3710) returns
//     a non-nil client for Destination Functions execution.
//
// The option must be applied during processor.New via the opts vararg. Because the
// transformer clients are constructed inside New before opts are applied, this option
// replaces the transformer clients with a new instance that includes both the original
// FeatureService and the Functions client adapter. The FeatureService is obtained from
// LifecycleManager.transformerFeaturesService which is populated before opts run.
func WithFunctionsRuntime(engine *functionsruntime.Engine) Opts {
	return func(l *LifecycleManager) {
		// Failure 1 fix: enable the Functions pipeline stages.
		l.Handle.functionsEnabled = true

		// Failure 2 fix: wire the Insert Functions executor with type-bridge adapter.
		l.Handle.insertFnExecutor = &insertFunctionEngineAdapter{engine: engine}

		// Failure 3 fix: rebuild transformer clients to include the Functions client.
		// The functionsClientAdapter satisfies transformer.FunctionsClient by delegating
		// to the Engine's ExecuteSourceFunction, ExecuteDestinationFunction, and
		// ExecuteInsertFunction methods with appropriate type conversions.
		fnClientAdapter := &functionsClientAdapter{engine: engine}
		l.Handle.transformerClients = transformer.NewClients(
			config.Default,
			logger.NewLogger().Child("processor"),
			stats.Default,
			transformer.WithFeatureService(l.transformerFeaturesService),
			transformer.WithFunctionsClient(fnClientAdapter),
		)
	}
}
