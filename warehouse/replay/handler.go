// Package replay provides the warehouse replay feature (E-035) for re-processing
// archived events through the warehouse pipeline, bypassing real-time Router delivery.
//
// This file implements the ReplayHandler orchestrator, which coordinates the full
// replay pipeline: archived events → replay payload → Gateway injection → job tracking.
// It also provides a separate Handler type for HTTP-only handler delegation via the
// ReplayTrigger interface, enabling test isolation of HTTP concerns from business logic.
//
// Pattern references:
//   - warehouse/archive/archiver.go (struct, config loading, stats, lifecycle)
//   - warehouse/archive/cron.go (context-cancellable background processing)
//   - warehouse/api/http.go (HTTP handler patterns, Chi router, JSON responses)
//   - gateway/handle_http_replay.go (replay request routing)
package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// ---------------------------------------------------------------------------
// Interface Definitions
// ---------------------------------------------------------------------------

// ReplayTrigger defines the interface for replay service operations.
// This interface is consumed by the Handler HTTP wrapper and can be mocked
// in handler_test.go for isolated HTTP layer testing.
type ReplayTrigger interface {
	// Trigger initiates a new warehouse replay job. It validates the request,
	// creates a job record, and starts the replay pipeline asynchronously.
	Trigger(ctx context.Context, req ReplayRequest) (*ReplayResponse, error)

	// GetStatus returns the current status of a replay job by its ID.
	GetStatus(ctx context.Context, jobID int64) (*ReplayJob, error)
}

// GatewayClient sends replay event batches to the Gateway replay endpoint.
// The implementation must set the X-Warehouse-Replay: true header on all
// requests to tag events for warehouse-only routing in the Processor.
type GatewayClient interface {
	// SendReplayBatch sends a batch of events to the Gateway with the
	// X-Warehouse-Replay: true header for warehouse-only routing.
	SendReplayBatch(ctx context.Context, batch []byte) error
}

// NOTE: ArchiverQuerier interface is defined in retriever.go (same package).
// It provides QueryArchivedEvents(ctx, sourceID, startTime, endTime) for
// fetching archived gateway event batches.

// ---------------------------------------------------------------------------
// Thread-safe in-memory job store
// ---------------------------------------------------------------------------

// replayJobStore provides thread-safe in-memory storage for replay jobs.
// It uses sync.RWMutex to allow concurrent reads (GetStatus) while
// serializing writes (Create, UpdateStatus). This can be extended to
// database persistence in the future.
type replayJobStore struct {
	mu     sync.RWMutex
	jobs   map[int64]*ReplayJob
	nextID int64
}

// newReplayJobStore creates a new empty job store with ID sequencing starting at 1.
func newReplayJobStore() *replayJobStore {
	return &replayJobStore{
		jobs:   make(map[int64]*ReplayJob),
		nextID: 1,
	}
}

// Create stores a new replay job and assigns it a unique monotonic ID.
// Returns the assigned job ID.
func (s *replayJobStore) Create(job *ReplayJob) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	job.ID = s.nextID
	s.nextID++
	s.jobs[job.ID] = job
	return job.ID
}

// Get retrieves a replay job by ID. Returns ErrReplayJobNotFound if the job
// does not exist.
func (s *replayJobStore) Get(id int64) (*ReplayJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	job, ok := s.jobs[id]
	if !ok {
		return nil, ErrReplayJobNotFound
	}
	// Return a copy to prevent concurrent mutation of the stored job.
	jobCopy := *job
	return &jobCopy, nil
}

// UpdateStatus transitions a replay job to the given status and updates its
// UpdatedAt timestamp. Returns ErrReplayJobNotFound if the job does not exist.
func (s *replayJobStore) UpdateStatus(id int64, status ReplayStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return ErrReplayJobNotFound
	}
	job.Status = status
	job.UpdatedAt = time.Now()
	return nil
}

// UpdateError sets the error message and transitions the job to StatusFailed.
func (s *replayJobStore) UpdateError(id int64, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return ErrReplayJobNotFound
	}
	job.Status = StatusFailed
	job.Error = errMsg
	job.UpdatedAt = time.Now()
	return nil
}

// ActiveCount returns the number of jobs in a non-terminal state (Pending or InProgress).
func (s *replayJobStore) ActiveCount() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int64
	for _, job := range s.jobs {
		if IsActive(job.Status) {
			count++
		}
	}
	return count
}

// PruneTerminalJobs removes terminal (Completed/Failed) jobs that are older than
// the given retention duration. This prevents unbounded memory growth in the
// in-memory job store over the lifetime of the process.
func (s *replayJobStore) PruneTerminalJobs(retention time.Duration) int {
	cutoff := time.Now().Add(-retention)
	s.mu.Lock()
	defer s.mu.Unlock()

	pruned := 0
	for id, job := range s.jobs {
		if IsTerminal(job.Status) && job.UpdatedAt.Before(cutoff) {
			delete(s.jobs, id)
			pruned++
		}
	}
	return pruned
}

// ---------------------------------------------------------------------------
// ReplayHandler — Full orchestrator
// ---------------------------------------------------------------------------

// ReplayHandler coordinates warehouse replay operations. It implements the
// ReplayTrigger interface and also exposes HTTP handler methods directly.
//
// The handler orchestrates the full pipeline:
//  1. Accept replay requests via HTTP or direct API call
//  2. Query archived events via the ArchivedEventRetriever
//  3. Construct replay payloads and batch them
//  4. Inject batches into the Gateway replay endpoint with X-Warehouse-Replay header
//  5. Track replay job lifecycle (Pending → InProgress → Completed/Failed)
//
// Configuration is loaded via the reloadable config pattern from config.go,
// and stats counters follow the warehouse/archive/archiver.go pattern.
type ReplayHandler struct {
	conf         *config.Config
	log          logger.Logger
	statsFactory stats.Stats
	retriever    *ArchivedEventRetriever
	gateway      GatewayClient

	// shutdownCtx is derived from the server's lifecycle context, enabling
	// graceful shutdown of in-flight replay goroutines when the server stops.
	// Background replay pipelines inherit this context (with an added timeout)
	// instead of using context.Background(), ensuring they are cancelled when
	// the server shuts down.
	shutdownCtx context.Context

	// Reloadable configuration loaded from config.go via LoadConfig().
	replayCfg Config

	// In-memory job store for tracking replay job lifecycle.
	jobs *replayJobStore

	// Prometheus-compatible stats counters for replay operations.
	replayTriggered stats.Counter
	replayFailed    stats.Counter
	replayCompleted stats.Counter
	replayBatchSent stats.Counter
}

// Compile-time assertion that ReplayHandler implements ReplayTrigger.
var _ ReplayTrigger = (*ReplayHandler)(nil)

// NewReplayHandler creates a new ReplayHandler with the provided dependencies.
// Configuration is loaded using the reloadable config pattern established in
// warehouse/archive/archiver.go (lines 92-98), enabling runtime hot-reloading
// without process restart.
//
// Parameters:
//   - shutdownCtx: Server lifecycle context for graceful shutdown of in-flight replays
//   - conf: Configuration instance for loading reloadable parameters
//   - log: Structured logger for diagnostic output (Child("replay") is created)
//   - statsFactory: Prometheus-compatible stats factory for metric emission
//   - retriever: ArchivedEventRetriever for querying archived events
//   - gateway: GatewayClient for sending replay batches to Gateway
func NewReplayHandler(
	shutdownCtx context.Context,
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
	retriever *ArchivedEventRetriever,
	gateway GatewayClient,
) *ReplayHandler {
	h := &ReplayHandler{
		conf:         conf,
		log:          log.Child("replay"),
		statsFactory: statsFactory,
		retriever:    retriever,
		gateway:      gateway,
		shutdownCtx:  shutdownCtx,
		jobs:         newReplayJobStore(),
	}

	// Load reloadable configuration from config.go — follows
	// warehouse/archive/archiver.go pattern (lines 92-98).
	h.replayCfg = LoadConfig(conf)

	// Stats counters — follows warehouse/router/upload_stats.go pattern using
	// NewTaggedStat with the standard warehouse tag set for Prometheus metric
	// aggregation by module.
	warehouseTag := stats.Tags{"module": "warehouse"}
	h.replayTriggered = statsFactory.NewTaggedStat("warehouse.replay.triggered", stats.CountType, warehouseTag)
	h.replayFailed = statsFactory.NewTaggedStat("warehouse.replay.failed", stats.CountType, warehouseTag)
	h.replayCompleted = statsFactory.NewTaggedStat("warehouse.replay.completed", stats.CountType, warehouseTag)
	h.replayBatchSent = statsFactory.NewTaggedStat("warehouse.replay.batchSent", stats.CountType, warehouseTag)

	return h
}

// ---------------------------------------------------------------------------
// ReplayHandler — Business Logic Methods
// ---------------------------------------------------------------------------

// Trigger initiates a new warehouse replay job. It validates the request,
// creates a job record, and starts the replay pipeline asynchronously in a
// background goroutine with its own timeout-bounded context.
//
// Returns ErrReplayDisabled if the replay feature is disabled.
// Returns ErrConcurrentLimitReached if the max concurrent replays limit is met.
// Returns a wrapped ErrInvalidReplayRequest if request validation fails.
func (h *ReplayHandler) Trigger(ctx context.Context, req ReplayRequest) (*ReplayResponse, error) {
	// 1. Check if replay feature is enabled via reloadable config.
	if !h.replayCfg.Enabled.Load() {
		return nil, ErrReplayDisabled
	}

	// 2. Validate request fields (source_id, destination_id, time range) BEFORE
	// checking infrastructure dependencies. This ensures invalid requests always
	// receive a proper 400 response with field-level details, even when the
	// gateway client is not yet available (e.g., during application startup).
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("invalid replay request: %w", err)
	}

	// 2a. Validate that the gateway client is configured. The GatewayClient may be nil
	// when the replay handler is instantiated during Setup() before the gateway is available.
	// This guard prevents a nil pointer panic when executeReplay() calls gateway.SendReplayBatch().
	// Returns the sentinel ErrGatewayNotConfigured, mapped to HTTP 503 by mapServiceError().
	if h.gateway == nil {
		return nil, ErrGatewayNotConfigured
	}

	// 3. Check concurrent replay limit to prevent resource exhaustion.
	activeCount := h.jobs.ActiveCount()
	maxConcurrent := int64(h.replayCfg.MaxConcurrentReplays.Load())
	if activeCount >= maxConcurrent {
		h.log.Warnn("concurrent replay limit reached",
			logger.NewIntField("activeCount", activeCount),
			logger.NewIntField("maxConcurrent", maxConcurrent),
		)
		return nil, ErrConcurrentLimitReached
	}

	// 4. Apply default replay type if not specified.
	if req.ReplayType == "" {
		req.ReplayType = DefaultReplayType
	}

	// 5. Create replay job in the in-memory store.
	now := time.Now()
	job := &ReplayJob{
		SourceID:      req.SourceID,
		DestinationID: req.DestinationID,
		StartTime:     req.StartTime,
		EndTime:       req.EndTime,
		ReplayType:    req.ReplayType,
		Status:        StatusPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	jobID := h.jobs.Create(job)

	// 6. Increment triggered counter for Prometheus.
	h.replayTriggered.Increment()

	h.log.Infon("replay job created",
		logger.NewIntField("jobID", jobID),
		logger.NewStringField("sourceID", req.SourceID),
		logger.NewStringField("destinationID", req.DestinationID),
	)

	// 7. Start replay pipeline asynchronously with the server shutdown context.
	// We use h.shutdownCtx (not r.Context()) so the pipeline is not cancelled
	// when the HTTP request ends, but IS cancelled on graceful server shutdown.
	go h.executeReplay(h.shutdownCtx, jobID, req)

	// 8. Return response with the assigned job ID and pending status.
	return &ReplayResponse{
		JobID:  jobID,
		Status: StatusPending,
	}, nil
}

// GetStatus returns the current status of a replay job by its ID.
// Returns ErrReplayJobNotFound if no job exists with the given ID.
func (h *ReplayHandler) GetStatus(_ context.Context, jobID int64) (*ReplayJob, error) {
	return h.jobs.Get(jobID)
}

// ---------------------------------------------------------------------------
// ReplayHandler — Background Pipeline Execution
// ---------------------------------------------------------------------------

// executeReplay runs the full replay pipeline for a single job. It is invoked
// as a goroutine from Trigger() and executes with a timeout-bounded context.
//
// Pipeline stages:
//  1. Set job status to InProgress
//  2. Query archived events via the ArchivedEventRetriever
//  3. Batch events per configured batch size
//  4. Send each batch to the Gateway with X-Warehouse-Replay: true header
//  5. Mark job as Completed or Failed
func (h *ReplayHandler) executeReplay(ctx context.Context, jobID int64, req ReplayRequest) {
	// Apply timeout from reloadable configuration.
	timeout := h.replayCfg.TimeoutMinutes.Load()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Transition job to InProgress. If this fails, explicitly fail the job
	// to prevent it from being stuck in Pending state permanently.
	if err := h.jobs.UpdateStatus(jobID, StatusInProgress); err != nil {
		h.log.Errorn("failed to update replay job status to in_progress",
			obskit.Error(err),
			logger.NewIntField("jobID", jobID),
		)
		_ = h.jobs.UpdateError(jobID, fmt.Sprintf("failed to transition to in_progress: %v", err))
		h.replayFailed.Increment()
		return
	}

	h.log.Infon("starting replay event retrieval",
		logger.NewIntField("jobID", jobID),
		logger.NewStringField("sourceID", req.SourceID),
		logger.NewStringField("destinationID", req.DestinationID),
	)

	// Stage 1: Retrieve archived events via the retriever.
	events, err := h.retriever.Retrieve(ctx, req.SourceID, req.StartTime, req.EndTime)
	if err != nil {
		h.log.Errorn("failed to retrieve archived events for replay",
			obskit.Error(err),
			logger.NewIntField("jobID", jobID),
		)
		_ = h.jobs.UpdateError(jobID, fmt.Sprintf("event retrieval failed: %v", err))
		h.replayFailed.Increment()
		return
	}

	// Handle empty result — no events in the requested range.
	if len(events) == 0 {
		h.log.Infon("no archived events found for replay, marking completed",
			logger.NewIntField("jobID", jobID),
		)
		_ = h.jobs.UpdateStatus(jobID, StatusCompleted)
		h.replayCompleted.Increment()
		return
	}

	// Stage 2: Batch events per configured batch size.
	batchSize := h.replayCfg.BatchSize.Load()
	batches := batchEvents(events, batchSize)
	totalBatches := len(batches)

	h.log.Infon("sending replay batches to gateway",
		logger.NewIntField("jobID", jobID),
		logger.NewIntField("totalEvents", int64(len(events))),
		logger.NewIntField("totalBatches", int64(totalBatches)),
		logger.NewIntField("batchSize", int64(batchSize)),
	)

	// Stage 3: Send each batch to the Gateway with the replay header.
	for i, batch := range batches {
		// Check for context cancellation between batch iterations.
		select {
		case <-ctx.Done():
			h.log.Warnn("replay context cancelled during batch sending",
				logger.NewIntField("jobID", jobID),
				logger.NewIntField("batchIndex", int64(i)),
				logger.NewIntField("totalBatches", int64(totalBatches)),
			)
			_ = h.jobs.UpdateError(jobID, fmt.Sprintf("context cancelled at batch %d/%d: %v", i+1, totalBatches, ctx.Err()))
			h.replayFailed.Increment()
			return
		default:
		}

		// Marshal the event batch to JSON using jsonrs (CRITICAL: not encoding/json).
		batchPayload, marshalErr := jsonrs.Marshal(batch)
		if marshalErr != nil {
			h.log.Errorn("failed to marshal replay batch",
				obskit.Error(marshalErr),
				logger.NewIntField("jobID", jobID),
				logger.NewIntField("batchIndex", int64(i)),
			)
			_ = h.jobs.UpdateError(jobID, fmt.Sprintf("batch marshal failed at %d/%d: %v", i+1, totalBatches, marshalErr))
			h.replayFailed.Increment()
			return
		}

		// Send the marshaled batch to the Gateway replay endpoint.
		if sendErr := h.gateway.SendReplayBatch(ctx, batchPayload); sendErr != nil {
			h.log.Errorn("failed to send replay batch to gateway",
				obskit.Error(sendErr),
				logger.NewIntField("jobID", jobID),
				logger.NewIntField("batchIndex", int64(i)),
				logger.NewIntField("totalBatches", int64(totalBatches)),
			)
			_ = h.jobs.UpdateError(jobID, fmt.Sprintf("gateway send failed at batch %d/%d: %v", i+1, totalBatches, sendErr))
			h.replayFailed.Increment()
			return
		}

		h.replayBatchSent.Increment()
	}

	// Stage 4: Mark job as completed.
	_ = h.jobs.UpdateStatus(jobID, StatusCompleted)
	h.replayCompleted.Increment()

	h.log.Infon("replay completed successfully",
		logger.NewIntField("jobID", jobID),
		logger.NewIntField("totalBatches", int64(totalBatches)),
		logger.NewIntField("totalEvents", int64(len(events))),
	)
}

// ---------------------------------------------------------------------------
// ReplayHandler — Background Pruning Loop
// ---------------------------------------------------------------------------

// Run starts a background loop that periodically prunes terminal replay jobs
// from the in-memory job store, preventing unbounded memory growth over the
// lifetime of the process.
//
// The loop follows the same context-cancellable select{} pattern established
// in warehouse/healthmonitor/monitor.go Run() and warehouse/backfill/service.go
// Run(): each tick uses time.After (matching warehouse/archive/cron.go convention),
// checks the reloadable enabled flag, and performs the pruning work.
//
// This method is designed to be started via errgroup in warehouse/app.go:
//
//	g.Go(crash.NotifyWarehouse(func() error {
//	    return replayHandler.Run(gCtx)
//	}))
func (h *ReplayHandler) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			h.log.Infon("context is cancelled, stopped running replay pruning loop")
			return nil
		case <-time.After(h.replayCfg.PruneInterval.Load()):
			if !h.replayCfg.Enabled.Load() {
				continue
			}

			retention := h.replayCfg.PruneRetention.Load()
			pruned := h.jobs.PruneTerminalJobs(retention)
			if pruned > 0 {
				h.log.Infon("pruned terminal replay jobs from in-memory store",
					logger.NewIntField("prunedCount", int64(pruned)),
				)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Handler — HTTP handler wrapper
// ---------------------------------------------------------------------------

// Handler provides HTTP handler methods that delegate business logic to a
// ReplayTrigger interface. This separation allows testing the HTTP layer
// independently by mocking the ReplayTrigger interface.
//
// Use NewHandler to create an instance, then mount TriggerReplay and
// GetReplayStatus on the desired Chi router paths.
type Handler struct {
	service ReplayTrigger
	log     logger.Logger
}

// NewHandler creates a new Handler that delegates replay operations to the
// provided ReplayTrigger implementation.
//
// Parameters:
//   - service: Business logic implementation (typically *ReplayHandler)
//   - log: Structured logger for HTTP-layer diagnostics
func NewHandler(service ReplayTrigger, log logger.Logger) *Handler {
	return &Handler{
		service: service,
		log:     log.Child("replay.handler"),
	}
}

// TriggerReplay handles POST /v1/warehouse/replay requests.
// It parses the JSON request body, delegates to the ReplayTrigger.Trigger()
// method, and returns a structured JSON response with HTTP 201 Created
// status for the newly created replay job resource.
func (h *Handler) TriggerReplay(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	var req ReplayRequest
	if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
		h.log.Warnn("invalid JSON in replay request body", obskit.Error(err))
		writeErrorResp(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	resp, err := h.service.Trigger(r.Context(), req)
	if err != nil {
		mapServiceError(w, h.log, err)
		return
	}

	writeJSONResp(w, http.StatusCreated, resp)
}

// GetReplayStatus handles GET /v1/warehouse/replay/{jobID} requests.
// It extracts the job ID from the URL path, delegates to
// ReplayTrigger.GetStatus(), and returns a structured JSON response.
func (h *Handler) GetReplayStatus(w http.ResponseWriter, r *http.Request) {
	jobIDStr := chi.URLParam(r, "jobID")
	jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
	if err != nil {
		writeErrorResp(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	job, err := h.service.GetStatus(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, ErrReplayJobNotFound) {
			writeErrorResp(w, http.StatusNotFound, "replay job not found")
			return
		}
		h.log.Errorn("failed to get replay status",
			obskit.Error(err),
			logger.NewIntField("jobID", jobID),
		)
		writeErrorResp(w, http.StatusInternalServerError, "failed to get replay status")
		return
	}

	writeJSONResp(w, http.StatusOK, job)
}

// ---------------------------------------------------------------------------
// Shared HTTP Response Helpers
// ---------------------------------------------------------------------------

// errorResponse is the structured JSON error payload returned by all HTTP
// handler methods. It follows the warehouse/api/http.go error response
// convention: {"status": "error", "message": "..."}.
type errorResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// writeErrorResp writes a structured JSON error response with the given HTTP
// status code and error message. Uses jsonrs for JSON encoding (CRITICAL:
// never encoding/json per .golangci.yml depguard rules).
func writeErrorResp(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = jsonrs.NewEncoder(w).Encode(errorResponse{
		Status:  "error",
		Message: message,
	})
}

// writeJSONResp writes a structured JSON success response with the given HTTP
// status code and data payload. Uses jsonrs for JSON encoding.
//
//nolint:unparam // statusCode is parameterised for flexibility with future status codes
func writeJSONResp(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = jsonrs.NewEncoder(w).Encode(data)
}

// mapServiceError maps service-level sentinel errors to appropriate HTTP
// status codes and writes a structured error response. This is shared by
// both ReplayHandler and Handler HTTP methods.
//
// Error mapping:
//   - ErrReplayDisabled        → 403 Forbidden
//   - ErrConcurrentLimitReached → 429 Too Many Requests
//   - ErrInvalidReplayRequest  → 400 Bad Request
//   - ErrGatewayNotConfigured  → 503 Service Unavailable
//   - Other errors             → 500 Internal Server Error
func mapServiceError(w http.ResponseWriter, log logger.Logger, err error) {
	switch {
	case errors.Is(err, ErrReplayDisabled):
		writeErrorResp(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrConcurrentLimitReached):
		writeErrorResp(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, ErrInvalidReplayRequest):
		writeErrorResp(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrGatewayNotConfigured):
		writeErrorResp(w, http.StatusServiceUnavailable, err.Error())
	default:
		log.Errorn("replay trigger failed", obskit.Error(err))
		writeErrorResp(w, http.StatusInternalServerError, "internal server error")
	}
}

// ---------------------------------------------------------------------------
// Batch Helper
// ---------------------------------------------------------------------------

// batchEvents splits a slice of archived events into batches of the specified
// size. If batchSize is <= 0, a fallback default of 1000 is used.
//
// This function is used by executeReplay to partition events before sending
// them to the Gateway, controlling per-batch memory consumption and payload size.
func batchEvents(events []ArchivedEvent, batchSize int) [][]ArchivedEvent {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	var batches [][]ArchivedEvent
	for i := 0; i < len(events); i += batchSize {
		end := i + batchSize
		if end > len(events) {
			end = len(events)
		}
		batches = append(batches, events[i:end])
	}
	return batches
}
