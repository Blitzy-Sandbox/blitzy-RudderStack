// Package runtime — insert_functions.go
//
// Implements Insert Functions pre-destination transformation hooks (Sprint 4-6,
// Epic E-017). Insert Functions sit between user transforms and destination
// transforms in the processor pipeline (processor/pipeline_worker.go lines
// 35-36, between the usertransform and destinationtransform channels). They
// allow per-destination event transformation or filtering before the event
// reaches the destination transformer.
//
// All eight Segment-compatible typed handlers are supported per AAP Rule 0.7.2:
//
//	onTrack, onIdentify, onGroup, onPage, onScreen, onAlias, onDelete, onBatch
//
// Architecture:
//
//	Processor pipeline → ExecuteInsertFunction → Transformer /v1/functions/insert
//	                                                  → InsertFunctionResult (Event | Dropped)
//
// The execution is delegated to the external Transformer service via HTTP POST,
// following the communication pattern established by
// processor/internal/transformer/user_transformer/user_transformer.go and the
// sibling Engine methods in engine.go.
//
// Unlike the Engine's internal executeInsertFunction method (which returns the
// normalised ExecutionResult), this exported method returns the Insert-specific
// InsertFunctionResult with the transformed event and drop signal directly.
//
// Insert Functions must be no-op when no Insert Functions are configured
// (AAP Rule 0.7.6 — backward compatibility).
//
// Reference: refs/segment-docs/src/connections/functions/insert-functions.md
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
// Insert Function types
// ---------------------------------------------------------------------------

// InsertFunctionResult represents the result of executing an Insert Function.
// Insert Functions sit between user transforms and destination transforms in
// the processor pipeline and may either transform an event or signal that the
// event should be dropped entirely (e.g., for filtering, deduplication, or
// PII redaction).
//
// When Dropped is true the event must not be forwarded to the destination
// transformer; the caller should discard the event from the pipeline. When
// Dropped is false the Event field contains the (possibly modified) event
// payload that should continue through the pipeline.
type InsertFunctionResult struct {
	// Event is the transformed event payload returned by the insert function.
	// It may be the original event unmodified, or a transformed version. When
	// Dropped is true this field is nil.
	Event json.RawMessage `json:"event"`

	// Dropped indicates whether the insert function intentionally dropped the
	// event. When true, the event must not be forwarded to destination
	// transforms.
	Dropped bool `json:"dropped"`
}

// ---------------------------------------------------------------------------
// Internal request / response types
// ---------------------------------------------------------------------------

// insertFunctionRequest is the payload POSTed to the Transformer service for
// Insert Function execution. It carries the function definition, the event
// payload, the resolved handler name, and merged settings.
type insertFunctionRequest struct {
	FunctionID  string            `json:"functionId"`
	WorkspaceID string            `json:"workspaceId"`
	Code        string            `json:"code"`
	Version     int               `json:"version"`
	Handler     string            `json:"handler"`   // e.g. "onTrack"
	EventType   string            `json:"eventType"` // e.g. "track"
	Event       json.RawMessage   `json:"event"`
	Settings    map[string]string `json:"settings"`
}

// insertFunctionResponse is the response received from the Transformer service
// after executing an Insert Function. The Transformer either returns the
// transformed event or signals a drop.
type insertFunctionResponse struct {
	// Event is the transformed (or original) event payload.
	Event json.RawMessage `json:"event,omitempty"`
	// Dropped signals that the Insert Function intentionally dropped the event.
	Dropped bool `json:"dropped,omitempty"`
	// Error is a structured function-level error, if any. The Type field maps
	// to one of the ErrorType* constants from errors.go.
	Error *FunctionError `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// ExecuteInsertFunction
// ---------------------------------------------------------------------------

// ExecuteInsertFunction executes an Insert Function against a single event.
//
// Insert Functions support the same typed handlers as Destination Functions:
// onTrack, onIdentify, onGroup, onPage, onScreen, onAlias, onDelete, onBatch.
// The handler is resolved automatically from the event's "type" field using
// the package-level eventTypeToHandler map defined in engine.go.
//
// The function either returns the transformed event or signals that the event
// should be dropped from the pipeline. A nil FunctionDef or empty event
// returns a descriptive error (never panics).
//
// This method delegates execution to the external Transformer service at the
// configured Insert Functions endpoint (/v1/functions/insert), following the
// HTTP communication pattern from user_transformer.go.
//
// Insert Functions hook into the processor pipeline between user transforms
// and destination transforms (see processor/pipeline_worker.go).
func (e *Engine) ExecuteInsertFunction(
	ctx context.Context,
	fn *FunctionDef,
	event json.RawMessage,
	settings map[string]string,
) (*InsertFunctionResult, error) {
	// -----------------------------------------------------------------------
	// Phase 1: Input validation
	// -----------------------------------------------------------------------

	if fn == nil {
		return nil, fmt.Errorf("insert function definition must not be nil")
	}
	if fn.Type != FunctionTypeInsert {
		return nil, &ValidationError{
			Message: fmt.Sprintf("expected function type %q, got %q", FunctionTypeInsert, fn.Type),
		}
	}
	if len(event) == 0 {
		return nil, &InvalidEventPayload{
			Message: "event payload is required for insert function execution",
		}
	}

	// -----------------------------------------------------------------------
	// Phase 2: Event type extraction and handler routing
	// -----------------------------------------------------------------------

	// Extract the event type from the JSON payload and map it to the
	// Segment-compatible handler name using the package-level
	// eventTypeToHandler map (defined in engine.go). resolveHandler returns a
	// typed *InvalidEventPayload or *EventNotSupported error for invalid or
	// unknown event types.
	handler, err := e.resolveHandler(event)
	if err != nil {
		recordInsertError(e.statsFactory, fn.ID, classifyInsertError(err))
		return nil, err
	}

	// -----------------------------------------------------------------------
	// Phase 3: Prepare execution context
	// -----------------------------------------------------------------------

	// Create a scoped child logger for insert-function-specific log entries,
	// following AAP Rule 0.7.4 (rudder-go-kit/logger with scoped child
	// loggers).
	log := e.log.Child("insert_functions")

	log.Debugn("Executing insert function",
		logger.NewStringField("functionId", fn.ID),
		logger.NewStringField("functionName", fn.Name),
		logger.NewStringField("handler", handler),
	)

	// Capture the start time for execution duration metrics.
	start := time.Now()

	// -----------------------------------------------------------------------
	// Phase 4: Build and send Transformer request
	// -----------------------------------------------------------------------

	// Merge function-level settings with per-invocation overrides. Overrides
	// take precedence, allowing callers to inject per-destination settings.
	merged := mergeSettings(fn.Settings, settings)

	// Extract the raw event type string for the request payload. We need the
	// original type string (e.g. "track"), not the handler name ("onTrack"),
	// for the Transformer's eventType field. resolveHandler already validated
	// the event JSON and type field, so this unmarshal is safe.
	var typeExtractor struct {
		Type string `json:"type"`
	}
	_ = jsonrs.Unmarshal(event, &typeExtractor)

	insertReq := insertFunctionRequest{
		FunctionID:  fn.ID,
		WorkspaceID: fn.WorkspaceID,
		Code:        fn.Code,
		Version:     fn.Version,
		Handler:     handler,
		EventType:   typeExtractor.Type,
		Event:       event,
		Settings:    merged,
	}

	reqBody, err := jsonrs.Marshal(insertReq)
	if err != nil {
		recordInsertError(e.statsFactory, fn.ID, "marshal_error")
		return nil, fmt.Errorf("marshaling insert function request: %w", err)
	}

	url := e.transformerURL + insertEndpoint

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, bytes.NewReader(reqBody),
	)
	if err != nil {
		recordInsertError(e.statsFactory, fn.ID, "http_request_error")
		return nil, fmt.Errorf("creating insert function HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(httpReq)
	if err != nil {
		log.Errorn("Insert function Transformer request failed",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
			obskit.Error(err),
		)
		recordInsertError(e.statsFactory, fn.ID, "http_error")
		return nil, fmt.Errorf("executing insert function via Transformer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read the full response body for parsing. io.ReadAll is used per the
	// user_transformer.go pattern.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		recordInsertError(e.statsFactory, fn.ID, "response_read_error")
		return nil, fmt.Errorf("reading insert function response body: %w", err)
	}

	// -----------------------------------------------------------------------
	// Phase 5: Record execution metrics
	// -----------------------------------------------------------------------

	elapsed := time.Since(start)

	// Record execution time as a timer metric, tagged by function ID and the
	// resolved handler name for per-handler latency analysis.
	e.statsFactory.NewTaggedStat(
		"functions_insert_execution_time",
		stats.TimerType,
		stats.Tags{"function_id": fn.ID, "handler": handler},
	).SendTiming(elapsed)

	// Record event processed counter, tagged by event type for throughput
	// analysis per event type.
	e.statsFactory.NewTaggedStat(
		"functions_insert_events_processed",
		stats.CountType,
		stats.Tags{"eventType": typeExtractor.Type},
	).Increment()

	// -----------------------------------------------------------------------
	// Phase 6: Parse and validate response
	// -----------------------------------------------------------------------

	// Check for HTTP-level errors from the Transformer service before
	// attempting to parse the response body.
	if resp.StatusCode != http.StatusOK {
		log.Errorn("Transformer returned non-200 for insert function",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
			logger.NewIntField("statusCode", int64(resp.StatusCode)),
		)
		recordInsertError(e.statsFactory, fn.ID, "http_status_error")
		return nil, fmt.Errorf(
			"transformer returned HTTP %d for insert function %s: %s",
			resp.StatusCode, fn.ID, string(respBody),
		)
	}

	// Unmarshal the Transformer response into the insert-specific response
	// type. Uses jsonrs per project linting rules (.golangci.yml forbids
	// encoding/json Marshal/Unmarshal — use jsonrs instead).
	var funcResp insertFunctionResponse
	if err := jsonrs.Unmarshal(respBody, &funcResp); err != nil {
		recordInsertError(e.statsFactory, fn.ID, "unmarshal_error")
		return nil, fmt.Errorf("unmarshaling insert function response: %w", err)
	}

	// Check for structured function-level errors in the response. The
	// Transformer wraps function errors in the Error field with a Type
	// matching one of the ErrorType* constants from errors.go.
	if funcResp.Error != nil && funcResp.Error.Type != "" {
		mappedErr := mapFunctionError(funcResp.Error)

		// Special case: DropEvent is not a failure — it means the function
		// intentionally decided to drop the event. Return a successful result
		// with Dropped=true instead of an error.
		if IsDropError(mappedErr) {
			log.Debugn("Insert function dropped event via DropEvent error",
				logger.NewStringField("functionId", fn.ID),
				logger.NewStringField("handler", handler),
			)
			e.statsFactory.NewTaggedStat(
				"functions_insert_events_dropped",
				stats.CountType,
				stats.Tags{"function_id": fn.ID, "eventType": typeExtractor.Type},
			).Increment()
			return &InsertFunctionResult{
				Event:   nil,
				Dropped: true,
			}, nil
		}

		log.Errorn("Insert function returned error",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
			logger.NewStringField("errorType", funcResp.Error.Type),
			logger.NewStringField("errorMessage", funcResp.Error.Message),
		)
		recordInsertError(e.statsFactory, fn.ID, funcResp.Error.Type)
		return nil, mappedErr
	}

	// -----------------------------------------------------------------------
	// Phase 7: Handle drop signal and return result
	// -----------------------------------------------------------------------

	// The Transformer may signal a drop via the Dropped field without
	// returning a function-level error. This happens when the insert function
	// returns undefined/null or explicitly returns a drop instruction.
	if funcResp.Dropped {
		log.Debugn("Insert function dropped event (response flag)",
			logger.NewStringField("functionId", fn.ID),
			logger.NewStringField("handler", handler),
		)
		e.statsFactory.NewTaggedStat(
			"functions_insert_events_dropped",
			stats.CountType,
			stats.Tags{"function_id": fn.ID, "eventType": typeExtractor.Type},
		).Increment()
		return &InsertFunctionResult{
			Event:   nil,
			Dropped: true,
		}, nil
	}

	// Successful execution: the event may have been transformed by the insert
	// function. If the Transformer returned no event payload, fall back to the
	// original event as a passthrough (no-op insert function), preserving
	// backward compatibility per AAP Rule 0.7.6.
	resultEvent := funcResp.Event
	if len(resultEvent) == 0 {
		resultEvent = event
	}

	log.Debugn("Insert function executed successfully",
		logger.NewStringField("functionId", fn.ID),
		logger.NewStringField("handler", handler),
	)

	return &InsertFunctionResult{
		Event:   resultEvent,
		Dropped: false,
	}, nil
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// recordInsertError increments the insert function error counter metric with
// the given error type tag. This is a small helper to keep the main function
// body readable while ensuring consistent metric names and tag keys across all
// error paths.
//
// Metric: functions_insert_errors (counter)
// Tags:
//   - function_id: the unique identifier of the function
//   - error_type: the classification of the error (e.g. "marshal_error",
//     "http_error", "EventNotSupported", etc.)
func recordInsertError(statsFactory stats.Stats, functionID, errorType string) {
	statsFactory.NewTaggedStat(
		"functions_insert_errors",
		stats.CountType,
		stats.Tags{"function_id": functionID, "error_type": errorType},
	).Increment()
}

// classifyInsertError returns a string classification for an insert function
// error, used for metrics tagging. It checks for the typed errors defined in
// errors.go using type assertions.
//
// The returned string is used as a tag value for the "error_type" dimension
// in the functions_insert_errors counter metric.
//
// Classifications:
//   - "retryable": the error is a *RetryError
//   - "drop": the error is a *DropEvent (event should be silently discarded)
//   - "event_not_supported": the error is an *EventNotSupported
//   - "invalid_payload": the error is an *InvalidEventPayload
//   - "validation": the error is a *ValidationError
//   - "unknown": any other error type
func classifyInsertError(err error) string {
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
