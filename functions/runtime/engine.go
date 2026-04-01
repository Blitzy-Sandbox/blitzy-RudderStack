// Package runtime provides the Functions runtime engine for the RudderStack
// pipeline, supporting Source Functions, Destination Functions, and Insert
// Functions (Sprint 4-6, Epics E-015, E-016, E-017).
//
// The Engine delegates JavaScript function execution to the external
// Transformer service (by default http://localhost:9090) via HTTP POST
// requests, following the same communication pattern established by
// processor/internal/transformer/user_transformer.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// ---------------------------------------------------------------------------
// Function type constants
// ---------------------------------------------------------------------------

const (
	// FunctionTypeSource identifies a Source Function that receives an HTTP
	// request and produces RudderStack events via its onRequest handler (E-015).
	FunctionTypeSource = "source"

	// FunctionTypeDestination identifies a Destination Function that receives
	// events and routes them to typed handlers such as onTrack, onIdentify,
	// etc. (E-016).
	FunctionTypeDestination = "destination"

	// FunctionTypeInsert identifies an Insert Function that sits between user
	// transforms and destination transforms in the processor pipeline, allowing
	// per-destination event transformation or filtering (E-017).
	FunctionTypeInsert = "insert"
)

// Internal constants for Transformer endpoint paths and configuration defaults.
const (
	sourceEndpoint      = "/v1/functions/source"
	destinationEndpoint = "/v1/functions/destination"
	insertEndpoint      = "/v1/functions/insert"

	defaultTransformerURL = "http://localhost:9090"
	defaultTimeoutSeconds = 600
)

// eventTypeToHandler maps RudderStack event type strings to the corresponding
// Segment-compatible typed handler names for Destination Functions (E-016) and
// Insert Functions (E-017). All 8 handlers per AAP Rule 0.7.2 are included.
var eventTypeToHandler = map[string]string{
	"track":    "onTrack",
	"identify": "onIdentify",
	"group":    "onGroup",
	"page":     "onPage",
	"screen":   "onScreen",
	"alias":    "onAlias",
	"delete":   "onDelete",
	"batch":    "onBatch",
}

// ---------------------------------------------------------------------------
// Exported types
// ---------------------------------------------------------------------------

// FunctionDef represents a user-defined function definition that is stored in
// the functions management layer and passed to the runtime engine for execution.
type FunctionDef struct {
	// ID is the unique identifier for this function.
	ID string `json:"id"`
	// WorkspaceID is the workspace this function belongs to.
	WorkspaceID string `json:"workspaceId"`
	// Name is the human-readable function name.
	Name string `json:"name"`
	// Type is one of FunctionTypeSource, FunctionTypeDestination, or FunctionTypeInsert.
	Type string `json:"type"`
	// Code contains the JavaScript function code.
	Code string `json:"code"`
	// Version is the monotonically increasing version number.
	Version int `json:"version"`
	// Settings contains function-level settings and encrypted secrets.
	Settings map[string]string `json:"settings"`
}

// ExecutionResult represents the result of a generic function execution,
// normalised across all three function types.
type ExecutionResult struct {
	// Events contains the output events. For Source Functions these are the
	// generated RudderStack events; for Insert Functions the (possibly
	// transformed) event; for Destination Functions the response body.
	Events []json.RawMessage `json:"events"`
	// Errors contains structured execution errors returned by the Transformer.
	Errors []FunctionError `json:"errors"`
	// Logs contains console output captured from the function runtime.
	Logs []string `json:"logs"`
}

// FunctionError represents a structured error from function execution, as
// returned by the Transformer service. The Type field matches one of the
// ErrorType* constants defined in errors.go.
type FunctionError struct {
	// Type is the error category (e.g. "EventNotSupported", "RetryError").
	Type string `json:"type"`
	// Message is a human-readable description of the error.
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// Engine is the core Functions runtime engine that dispatches function
// execution requests to the external Transformer service via HTTP.
// It is safe for concurrent use by multiple goroutines.
type Engine struct {
	conf           *config.Config
	log            logger.Logger
	statsFactory   stats.Stats
	httpClient     *http.Client
	transformerURL string

	// Lifecycle management fields.
	mu      sync.RWMutex
	running bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a new Functions runtime engine. The engine communicates with the
// Transformer service at the configured URL (defaults to http://localhost:9090
// matching the existing Transformer service).
//
// Configuration keys:
//   - FUNCTIONS_TRANSFORM_URL: Primary URL for Functions runtime (falls back to DEST_TRANSFORM_URL)
//   - DEST_TRANSFORM_URL: Fallback transformer URL
//   - Functions.Runtime.timeout: HTTP client timeout in seconds (default: 600)
//
// Pattern reference: processor/internal/transformer/user_transformer/user_transformer.go:42-48
func New(conf *config.Config, log logger.Logger, statsFactory stats.Stats) *Engine {
	if conf == nil {
		conf = config.Default
	}
	if log == nil {
		log = logger.NewLogger().Child("functions")
	}
	childLog := log.Child("functions_runtime")

	// Read the Transformer URL from configuration, following the two-level
	// fallback pattern from user_transformer.go.
	transformerURL := conf.GetString(
		"FUNCTIONS_TRANSFORM_URL",
		conf.GetString("DEST_TRANSFORM_URL", defaultTransformerURL),
	)

	// Read the HTTP client timeout in seconds (default 600s = 10 minutes),
	// matching the timeout used by the user transformer.
	timeout := conf.GetDuration(
		"Functions.Runtime.timeout",
		defaultTimeoutSeconds,
		time.Second,
	)

	httpClient := &http.Client{
		Timeout: timeout,
	}

	childLog.Infon("Functions runtime engine initialised",
		logger.NewStringField("transformerURL", transformerURL),
	)

	return &Engine{
		conf:           conf,
		log:            childLog,
		statsFactory:   statsFactory,
		httpClient:     httpClient,
		transformerURL: transformerURL,
	}
}

// ---------------------------------------------------------------------------
// Lifecycle: Run / Stop
// ---------------------------------------------------------------------------

// NewEngine is an alias for New, providing a longer-form constructor name
// for use in contexts where the package alias makes the shorter name ambiguous
// (e.g., runner/runner.go uses functionsruntime.NewEngine).
func NewEngine(conf *config.Config, log logger.Logger, statsFactory stats.Stats) *Engine {
	return New(conf, log, statsFactory)
}

// Run starts the engine's lifecycle. It blocks until the provided context is
// cancelled or Stop is called. When no functions are configured, the engine
// acts as a no-op (AAP Rule 0.7.6 — backward compatibility).
// Returns nil on clean shutdown or the context error on cancellation.
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return nil
	}
	childCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.running = true
	e.mu.Unlock()

	e.log.Infon("Functions runtime engine started")

	// Block until shutdown is requested via context cancellation or Stop().
	<-childCtx.Done()

	e.mu.Lock()
	e.running = false
	e.mu.Unlock()

	// Wait for any in-flight work tracked via e.wg.
	e.wg.Wait()

	e.log.Infon("Functions runtime engine stopped")
	return nil
}

// Stop gracefully shuts down the engine, cancelling any background work and
// waiting for in-flight operations to complete.
func (e *Engine) Stop() {
	e.mu.RLock()
	cancel := e.cancel
	running := e.running
	e.mu.RUnlock()

	if !running || cancel == nil {
		return
	}
	cancel()
	e.wg.Wait()
}

// ---------------------------------------------------------------------------
// Execute — main entry point
// ---------------------------------------------------------------------------

// Execute is the main entry point for function execution. It dispatches to the
// appropriate internal handler based on the function type:
//
//   - FunctionTypeSource      → executeSourceFunction   → POST /v1/functions/source
//   - FunctionTypeDestination → executeDestinationFunction → POST /v1/functions/destination
//   - FunctionTypeInsert      → executeInsertFunction   → POST /v1/functions/insert
//
// The method is safe for concurrent use.
func (e *Engine) Execute(
	ctx context.Context,
	fn *FunctionDef,
	event json.RawMessage,
	settings map[string]string,
) (*ExecutionResult, error) {
	if fn == nil {
		return nil, fmt.Errorf("function definition is required")
	}

	start := time.Now()
	fnType := fn.Type

	// Merge function-level settings with call-level settings. Call-level
	// settings take precedence so that callers can override per invocation.
	mergedSettings := mergeSettings(fn.Settings, settings)

	var (
		result *ExecutionResult
		err    error
	)

	switch fnType {
	case FunctionTypeSource:
		result, err = e.executeSourceFunction(ctx, fn, event, mergedSettings)
	case FunctionTypeDestination:
		result, err = e.executeDestinationFunction(ctx, fn, event, mergedSettings)
	case FunctionTypeInsert:
		result, err = e.executeInsertFunction(ctx, fn, event, mergedSettings)
	default:
		return nil, fmt.Errorf("unsupported function type: %s", fnType)
	}

	// Record execution metrics.
	elapsed := time.Since(start)
	status := "success"
	if err != nil {
		status = "error"
	}

	e.statsFactory.NewTaggedStat(
		"functions_execution_time",
		stats.TimerType,
		stats.Tags{"function_type": fnType, "function_id": fn.ID},
	).SendTiming(elapsed)

	e.statsFactory.NewTaggedStat(
		"functions_executions_total",
		stats.CountType,
		stats.Tags{"function_type": fnType, "status": status},
	).Increment()

	return result, err
}

// ---------------------------------------------------------------------------
// Internal dispatch methods
// ---------------------------------------------------------------------------

// executeSourceFunction handles Source Function execution (E-015).
// Source Functions expose a single onRequest(request, settings) handler that
// receives an HTTP request representation and returns an array of RudderStack
// events.
func (e *Engine) executeSourceFunction(
	ctx context.Context,
	fn *FunctionDef,
	event json.RawMessage,
	settings map[string]string,
) (*ExecutionResult, error) {
	req := transformerRequest{
		FunctionID:  fn.ID,
		WorkspaceID: fn.WorkspaceID,
		Code:        fn.Code,
		Version:     fn.Version,
		Handler:     "onRequest", // Source Functions always use onRequest
		Request:     event,       // For source functions the event carries the HTTP request
		Settings:    settings,
	}

	respBody, err := e.postToTransformer(ctx, sourceEndpoint, &req)
	if err != nil {
		e.log.Errorn("Source function execution failed",
			logger.NewStringField("functionId", fn.ID),
			obskit.Error(err),
		)
		return nil, err
	}

	return e.parseTransformerResponse(respBody, fn)
}

// executeDestinationFunction handles Destination Function execution (E-016).
// It routes the event to the appropriate typed handler (onTrack, onIdentify,
// onGroup, onPage, onScreen, onAlias, onDelete, onBatch) based on the event's
// "type" field.
func (e *Engine) executeDestinationFunction(
	ctx context.Context,
	fn *FunctionDef,
	event json.RawMessage,
	settings map[string]string,
) (*ExecutionResult, error) {
	// Extract the event type to determine the handler name.
	handler, err := e.resolveHandler(event)
	if err != nil {
		return nil, err
	}

	req := transformerRequest{
		FunctionID:  fn.ID,
		WorkspaceID: fn.WorkspaceID,
		Code:        fn.Code,
		Version:     fn.Version,
		Handler:     handler,
		Event:       event,
		Settings:    settings,
	}

	respBody, err := e.postToTransformer(ctx, destinationEndpoint, &req)
	if err != nil {
		e.log.Errorn("Destination function execution failed",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
			obskit.Error(err),
		)
		return nil, err
	}

	return e.parseTransformerResponse(respBody, fn)
}

// executeInsertFunction handles Insert Function execution (E-017).
// Insert Functions hook between user transforms and destination transforms in
// the processor pipeline. They can transform events or drop them entirely.
func (e *Engine) executeInsertFunction(
	ctx context.Context,
	fn *FunctionDef,
	event json.RawMessage,
	settings map[string]string,
) (*ExecutionResult, error) {
	handler, err := e.resolveHandler(event)
	if err != nil {
		return nil, err
	}

	req := transformerRequest{
		FunctionID:  fn.ID,
		WorkspaceID: fn.WorkspaceID,
		Code:        fn.Code,
		Version:     fn.Version,
		Handler:     handler,
		Event:       event,
		Settings:    settings,
	}

	respBody, err := e.postToTransformer(ctx, insertEndpoint, &req)
	if err != nil {
		e.log.Errorn("Insert function execution failed",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
			obskit.Error(err),
		)
		return nil, err
	}

	return e.parseTransformerResponse(respBody, fn)
}

// ---------------------------------------------------------------------------
// HTTP communication helper
// ---------------------------------------------------------------------------

// postToTransformer posts the given payload to the Transformer service at the
// specified endpoint path and returns the raw response body. It follows the
// HTTP communication pattern from user_transformer.go's sendBatch/doPost.
func (e *Engine) postToTransformer(
	ctx context.Context,
	endpoint string,
	payload any,
) (json.RawMessage, error) {
	start := time.Now()

	body, err := jsonrs.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling function request: %w", err)
	}

	url := e.transformerURL + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("posting to transformer (%s): %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading transformer response: %w", err)
	}

	// Record HTTP round-trip metric.
	e.statsFactory.NewTaggedStat(
		"functions_transformer_request_time",
		stats.TimerType,
		stats.Tags{"endpoint": endpoint},
	).SendTiming(time.Since(start))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"transformer returned HTTP %d for %s: %s",
			resp.StatusCode, endpoint, string(respBody),
		)
	}

	return json.RawMessage(respBody), nil
}

// ---------------------------------------------------------------------------
// Response parsing
// ---------------------------------------------------------------------------

// parseTransformerResponse converts the raw Transformer response body into an
// ExecutionResult. If the response carries an error payload, it maps the error
// to the corresponding typed Go error from errors.go.
func (e *Engine) parseTransformerResponse(
	body json.RawMessage,
	fn *FunctionDef,
) (*ExecutionResult, error) {
	var resp transformerResponse
	if err := jsonrs.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshaling transformer response: %w", err)
	}

	// If the Transformer returned a top-level error, map it to a typed Go error.
	if resp.Error != nil && resp.Error.Type != "" {
		mappedErr := mapFunctionError(resp.Error)
		e.log.Errorn("Function execution returned error",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("errorType", resp.Error.Type),
			logger.NewStringField("errorMessage", resp.Error.Message),
		)
		return nil, mappedErr
	}

	result := &ExecutionResult{
		Errors: resp.Errors,
		Logs:   resp.Logs,
	}

	// Normalise the events field: some function types return a single event
	// in the "event" key, while others return an array in "events".
	switch {
	case len(resp.Events) > 0:
		result.Events = resp.Events
	case len(resp.Event) > 0 && !resp.Dropped:
		result.Events = []json.RawMessage{resp.Event}
	}

	// For Insert Functions: if the Transformer indicated the event was dropped,
	// return an empty events list (which signals a drop to the caller).
	if resp.Dropped {
		result.Events = nil
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Handler resolution
// ---------------------------------------------------------------------------

// resolveHandler extracts the "type" field from an event JSON payload and maps
// it to the corresponding Segment-compatible handler name. It returns a typed
// error if the event type is missing or not supported.
func (e *Engine) resolveHandler(event json.RawMessage) (string, error) {
	if len(event) == 0 {
		return "", &InvalidEventPayload{Message: "event payload is required"}
	}

	var extractor struct {
		Type string `json:"type"`
	}
	if err := jsonrs.Unmarshal(event, &extractor); err != nil {
		return "", &InvalidEventPayload{
			Message: fmt.Sprintf("failed to extract event type: %s", err.Error()),
		}
	}

	if extractor.Type == "" {
		return "", &InvalidEventPayload{Message: "event type is required"}
	}

	handler, ok := eventTypeToHandler[extractor.Type]
	if !ok {
		return "", &EventNotSupported{
			Message: fmt.Sprintf("no handler for event type: %s", extractor.Type),
		}
	}

	return handler, nil
}

// ---------------------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------------------

// mapFunctionError maps a structured FunctionError (as returned by the
// Transformer service) to the corresponding typed Go error defined in
// errors.go. The mapping uses the ErrorType* constants.
func mapFunctionError(fe *FunctionError) error {
	if fe == nil {
		return nil
	}
	switch fe.Type {
	case ErrorTypeEventNotSupported:
		return &EventNotSupported{Message: fe.Message}
	case ErrorTypeInvalidEventPayload:
		return &InvalidEventPayload{Message: fe.Message}
	case ErrorTypeValidationError:
		return &ValidationError{Message: fe.Message}
	case ErrorTypeRetryError:
		return &RetryError{Message: fe.Message}
	case ErrorTypeDropEvent:
		return &DropEvent{Message: fe.Message}
	default:
		return fmt.Errorf("function error: %s - %s", fe.Type, fe.Message)
	}
}

// ---------------------------------------------------------------------------
// Internal types
// ---------------------------------------------------------------------------

// transformerRequest is the payload sent to the Transformer service for
// function execution. It is used for all three function types; only the
// relevant fields are populated per type.
type transformerRequest struct {
	FunctionID  string            `json:"functionId"`
	WorkspaceID string            `json:"workspaceId"`
	Code        string            `json:"code"`
	Version     int               `json:"version"`
	Handler     string            `json:"handler"`
	Event       json.RawMessage   `json:"event,omitempty"`   // Destination / Insert functions
	Request     json.RawMessage   `json:"request,omitempty"` // Source function HTTP request
	Settings    map[string]string `json:"settings"`
}

// transformerResponse is the normalised response payload from the Transformer
// service. Different function types populate different subsets of fields.
type transformerResponse struct {
	// Source functions return an array of generated events.
	Events []json.RawMessage `json:"events,omitempty"`
	// Insert functions may return a single transformed event.
	Event json.RawMessage `json:"event,omitempty"`
	// Insert functions may signal an event drop.
	Dropped bool `json:"dropped,omitempty"`
	// Destination functions return an HTTP-style status code.
	StatusCode int `json:"statusCode,omitempty"`
	// Destination functions return a response body.
	Body json.RawMessage `json:"body,omitempty"`
	// Top-level error from function execution.
	Error *FunctionError `json:"error,omitempty"`
	// Per-event errors (for batch processing).
	Errors []FunctionError `json:"errors,omitempty"`
	// Console output from the function runtime.
	Logs []string `json:"logs,omitempty"`
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// mergeSettings merges base settings with override settings. Override values
// take precedence so that callers can provide per-invocation overrides. Both
// maps may be nil.
func mergeSettings(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}
