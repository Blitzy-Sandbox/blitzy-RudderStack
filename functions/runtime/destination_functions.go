// Package runtime — destination_functions.go
//
// Implements Destination Functions with per-event typed handler dispatch
// (Sprint 4-6, Epic E-016). Destination Functions receive events and route
// them to typed handlers based on the event type. Each handler receives
// (event, settings) and returns a response or error.
//
// All eight Segment-compatible typed handlers are supported per AAP Rule 0.7.2:
//
//	onTrack, onIdentify, onGroup, onPage, onScreen, onAlias, onDelete, onBatch
//
// Architecture:
//
//	Processor pipeline → ExecuteDestinationFunction → Transformer /v1/functions/destination
//	                                                       → DestinationFunctionResult (StatusCode, Body)
//
// The execution is delegated to the external Transformer service via HTTP POST,
// following the communication pattern established by
// processor/internal/transformer/user_transformer/user_transformer.go and
// the sibling Engine methods in engine.go.
//
// Unlike the Engine's internal executeDestinationFunction method (which
// extracts the event type from the event JSON and returns the normalised
// ExecutionResult), this exported method accepts an explicit eventType
// parameter and returns the raw DestinationFunctionResult with the HTTP-style
// status code and body directly from the function execution.
//
// Reference: refs/segment-docs/src/connections/functions/destination-functions.md
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// ---------------------------------------------------------------------------
// Destination Function types
// ---------------------------------------------------------------------------

// DestinationFunctionResult represents the result of executing a Destination
// Function handler. It carries the HTTP-style status code from the function
// execution and the raw response body returned by the handler.
//
// StatusCode follows HTTP semantics:
//   - 200: success
//   - 400: invalid input (client error)
//   - 500: internal function error
//   - Other status codes as returned by the function handler
//
// Body contains the raw JSON response from the Destination Function handler.
// It may be nil if the handler returns no body (e.g. fire-and-forget delivery
// confirmations).
type DestinationFunctionResult struct {
	// StatusCode is an HTTP-style status code from function execution.
	StatusCode int `json:"statusCode"`
	// Body is the raw response body from the function handler.
	Body json.RawMessage `json:"body"`
}

// ---------------------------------------------------------------------------
// Internal request/response types for Transformer communication
// ---------------------------------------------------------------------------

// destinationFunctionRequest is the JSON payload POSTed to the Transformer
// service at the /v1/functions/destination endpoint for destination function
// execution. It carries the function definition, the resolved handler name,
// the raw event, and the merged settings map.
type destinationFunctionRequest struct {
	// FunctionID is the unique identifier of the destination function.
	FunctionID string `json:"functionId"`
	// Code is the JavaScript source code of the function.
	Code string `json:"code"`
	// Version is the function version for cache invalidation.
	Version int `json:"version"`
	// Handler is the resolved handler name (e.g. "onTrack", "onIdentify").
	Handler string `json:"handler"`
	// EventType is the original RudderStack event type string.
	EventType string `json:"eventType"`
	// Event is the raw JSON event payload.
	Event json.RawMessage `json:"event"`
	// Settings is the merged map of function-level and call-level settings.
	Settings map[string]string `json:"settings"`
}

// destinationFunctionResponse is the JSON shape returned by the Transformer
// service for destination function execution. It contains the function
// execution result and any structured error.
type destinationFunctionResponse struct {
	// StatusCode is the HTTP-style status code from the function handler.
	StatusCode int `json:"statusCode"`
	// Body is the raw response body from the function handler.
	Body json.RawMessage `json:"body"`
	// Error contains the structured error if the function execution failed.
	// It is nil on success.
	Error *FunctionError `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// ExecuteDestinationFunction — public API
// ---------------------------------------------------------------------------

// ExecuteDestinationFunction executes a Destination Function against an event.
// It dispatches to the appropriate typed handler based on the eventType
// parameter: onTrack, onIdentify, onGroup, onPage, onScreen, onAlias,
// onDelete, onBatch. Each handler receives (event, settings) and returns a
// response or error.
//
// The function communicates with the external Transformer service via HTTP
// POST to the /v1/functions/destination endpoint, following the pattern
// established by user_transformer.go and the Engine's postToTransformer
// helper.
//
// Input validation:
//   - fn must not be nil
//   - event must not be empty
//   - eventType must not be empty
//   - eventType must map to one of the 8 supported handlers
//
// Error handling:
//   - Returns *InvalidEventPayload for empty event or empty eventType
//   - Returns *EventNotSupported for unrecognised event types
//   - Returns typed errors (*RetryError, *DropEvent, *ValidationError, etc.)
//     from the function execution
//   - Returns wrapped errors for HTTP communication failures
//
// Metrics (rudder-go-kit/stats):
//   - functions_destination_execution_time: timer tagged by function_id, handler
//   - functions_destination_events_processed: counter tagged by eventType
//   - functions_destination_errors: counter tagged by function_id, error_type
//
// Reference: Segment Destination Functions documentation at
// refs/segment-docs/src/connections/functions/destination-functions.md
func (e *Engine) ExecuteDestinationFunction(
	ctx context.Context,
	fn *FunctionDef,
	event json.RawMessage,
	eventType string,
	settings map[string]string,
) (*DestinationFunctionResult, error) {
	// -----------------------------------------------------------------------
	// Phase 1: Input validation
	// -----------------------------------------------------------------------

	if fn == nil {
		return nil, fmt.Errorf("function definition is required")
	}
	if len(event) == 0 {
		return nil, &InvalidEventPayload{Message: "event payload is required"}
	}
	if eventType == "" {
		return nil, &InvalidEventPayload{Message: "event type is required"}
	}

	// -----------------------------------------------------------------------
	// Phase 2: Handler routing — all 8 handlers per AAP Rule 0.7.2
	// -----------------------------------------------------------------------

	// Look up the handler name from the package-level eventTypeToHandler map
	// (defined in engine.go). The map covers all 8 required handlers:
	// track→onTrack, identify→onIdentify, group→onGroup, page→onPage,
	// screen→onScreen, alias→onAlias, delete→onDelete, batch→onBatch.
	handler, ok := eventTypeToHandler[eventType]
	if !ok {
		return nil, &EventNotSupported{
			Message: fmt.Sprintf("no handler for event type: %s", eventType),
		}
	}

	// -----------------------------------------------------------------------
	// Phase 3: Prepare execution context
	// -----------------------------------------------------------------------

	// Create a scoped child logger for destination function execution. The
	// Engine's base logger is already scoped to "functions_runtime"; this
	// creates a nested "functions_runtime.destination_functions" scope.
	log := e.log.Child("destination_functions")
	log.Debugn("Executing destination function",
		logger.NewStringField("functionId", fn.ID),
		logger.NewStringField("eventType", eventType),
		logger.NewStringField("handler", handler),
	)

	start := time.Now()

	// Merge function-level settings from FunctionDef with call-level settings.
	// Call-level settings take precedence, allowing per-invocation overrides
	// of function defaults while preserving base configuration.
	mergedSettings := mergeSettings(fn.Settings, settings)

	// -----------------------------------------------------------------------
	// Phase 4: Build and send Transformer request
	// -----------------------------------------------------------------------

	// Build the request payload for the Transformer service. Unlike the
	// Engine's internal transformerRequest, this uses the destination-specific
	// schema that includes EventType for explicit handler context.
	req := destinationFunctionRequest{
		FunctionID: fn.ID,
		Code:       fn.Code,
		Version:    fn.Version,
		Handler:    handler,
		EventType:  eventType,
		Event:      event,
		Settings:   mergedSettings,
	}

	// Marshal the request payload using jsonrs (the project-standard JSON
	// library required by .golangci.yml linting rules instead of encoding/json).
	reqBody, err := jsonrs.Marshal(&req)
	if err != nil {
		recordDestinationError(e.statsFactory, fn.ID, "marshal_error")
		return nil, fmt.Errorf("marshaling destination function request: %w", err)
	}

	// POST to the Transformer service at the /v1/functions/destination endpoint.
	// The URL is composed from the Engine's configured transformer URL and the
	// destinationEndpoint constant defined in engine.go.
	url := e.transformerURL + destinationEndpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		recordDestinationError(e.statsFactory, fn.ID, "request_create_error")
		return nil, fmt.Errorf("creating destination function HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Execute the HTTP request using the Engine's shared HTTP client, which
	// has the configured timeout from FUNCTIONS_TRANSFORM_TIMEOUT or the
	// default 600s timeout.
	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		log.Errorn("Destination function transformer request failed",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
			obskit.Error(err),
		)
		recordDestinationError(e.statsFactory, fn.ID, classifyDestinationError(err))
		return nil, fmt.Errorf("posting destination function to transformer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read the full response body for parsing. io.ReadAll is used per the
	// user_transformer.go pattern.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		recordDestinationError(e.statsFactory, fn.ID, "response_read_error")
		return nil, fmt.Errorf("reading destination function response body: %w", err)
	}

	// -----------------------------------------------------------------------
	// Phase 5: Record execution metrics
	// -----------------------------------------------------------------------

	elapsed := time.Since(start)

	// Record execution time as a timer metric, tagged by function ID and the
	// resolved handler name for per-handler latency analysis.
	e.statsFactory.NewTaggedStat(
		"functions_destination_execution_time",
		stats.TimerType,
		stats.Tags{"function_id": fn.ID, "handler": handler},
	).SendTiming(elapsed)

	// Record event processed counter, tagged by event type for throughput
	// analysis per event type.
	e.statsFactory.NewTaggedStat(
		"functions_destination_events_processed",
		stats.CountType,
		stats.Tags{"eventType": eventType},
	).Increment()

	// -----------------------------------------------------------------------
	// Phase 6: Parse and validate response
	// -----------------------------------------------------------------------

	// Check for HTTP-level errors from the Transformer service before
	// attempting to parse the response body.
	if resp.StatusCode != http.StatusOK {
		log.Errorn("Transformer returned non-200 for destination function",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
			logger.NewIntField("statusCode", int64(resp.StatusCode)),
		)
		recordDestinationError(e.statsFactory, fn.ID, "http_status_error")
		return nil, fmt.Errorf(
			"transformer returned HTTP %d for destination function %s: %s",
			resp.StatusCode, fn.ID, string(respBody),
		)
	}

	// Unmarshal the Transformer response into the destination-specific
	// response type. Uses jsonrs per project linting rules.
	var funcResp destinationFunctionResponse
	if err := jsonrs.Unmarshal(respBody, &funcResp); err != nil {
		recordDestinationError(e.statsFactory, fn.ID, "unmarshal_error")
		return nil, fmt.Errorf("unmarshaling destination function response: %w", err)
	}

	// Check for structured function-level errors in the response. The
	// Transformer wraps function errors in the Error field with a Type
	// matching one of the ErrorType* constants from errors.go.
	if funcResp.Error != nil && funcResp.Error.Type != "" {
		mappedErr := mapFunctionError(funcResp.Error)
		log.Errorn("Destination function returned error",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
			logger.NewStringField("errorType", funcResp.Error.Type),
			logger.NewStringField("errorMessage", funcResp.Error.Message),
		)
		recordDestinationError(e.statsFactory, fn.ID, funcResp.Error.Type)
		return nil, mappedErr
	}

	// -----------------------------------------------------------------------
	// Phase 7: Return successful result
	// -----------------------------------------------------------------------

	log.Debugn("Destination function executed successfully",
		logger.NewStringField("functionId", fn.ID),
		logger.NewStringField("handler", handler),
		logger.NewIntField("statusCode", int64(funcResp.StatusCode)),
	)

	return &DestinationFunctionResult{
		StatusCode: funcResp.StatusCode,
		Body:       funcResp.Body,
	}, nil
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// recordDestinationError increments the destination function error counter
// metric with the given error type tag. This is a small helper to keep the
// main function body readable while ensuring consistent metric names and tag
// keys across all error paths.
//
// Metric: functions_destination_errors (counter)
// Tags:
//   - function_id: the unique identifier of the function
//   - error_type: the classification of the error (e.g. "marshal_error",
//     "http_error", "EventNotSupported", etc.)
func recordDestinationError(statsFactory stats.Stats, functionID, errorType string) {
	statsFactory.NewTaggedStat(
		"functions_destination_errors",
		stats.CountType,
		stats.Tags{"function_id": functionID, "error_type": errorType},
	).Increment()
}

// classifyDestinationError returns a string classification for a destination
// function error, used for metrics tagging. It checks for the typed errors
// defined in errors.go using the Retryable and Drop interfaces.
//
// The returned string is used as a tag value for the "error_type" dimension
// in the functions_destination_errors counter metric.
//
// Classifications:
//   - "retryable": the error implements Retryable and IsRetryable() returns true
//   - "drop": the error is a *DropEvent (event should be silently discarded)
//   - "event_not_supported": the error is an *EventNotSupported
//   - "invalid_payload": the error is an *InvalidEventPayload
//   - "validation": the error is a *ValidationError
//   - "unknown": any other error type
func classifyDestinationError(err error) string {
	if err == nil {
		return "none"
	}
	switch err.(type) {
	case *RetryError:
		return "retryable"
	case *DropEvent:
		return "drop"
	case *EventNotSupported:
		return "event_not_supported"
	case *InvalidEventPayload:
		return "invalid_payload"
	case *ValidationError:
		return "validation"
	default:
		return "unknown"
	}
}
