// Package runtime — insert_functions_test.go
//
// Unit tests for Insert Functions pre-destination transformation hooks
// (Sprint 4-6, Epic E-017). Tests the Engine.ExecuteInsertFunction entry point
// which transforms events between user transforms and destination transforms in
// the processor pipeline (processor/pipeline_worker.go lines 35-36).
//
// All 8 Segment-compatible typed handlers are tested per AAP Rule 0.7.2:
//
//	onTrack, onIdentify, onGroup, onPage, onScreen, onAlias, onDelete, onBatch
//
// Tests use httptest.NewServer to mock the Transformer service's
// /v1/functions/insert endpoint, following the fakeTransformer pattern from
// processor/internal/transformer/user_transformer/user_transformer_test.go.
//
// Assertions use testify/require per repository convention for unit tests.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestEngine creates an Engine whose Transformer URL points at serverURL.
// It uses NOP logger and in-memory stats suitable for deterministic unit tests.
// Pattern reference: user_transformer_test.go lines 153-189.
func newTestEngine(t *testing.T, serverURL string) *Engine {
	t.Helper()
	conf := config.New()
	conf.Set("FUNCTIONS_TRANSFORM_URL", serverURL)
	conf.Set("Functions.Runtime.timeout", 10) // 10-second HTTP timeout for tests
	statsStore, err := memstats.New()
	require.NoError(t, err)
	return New(conf, logger.NOP, statsStore)
}

// testInsertFunctionDef returns a minimal FunctionDef configured for insert
// function testing. The Settings field contains a base key that will be merged
// with per-invocation settings in settings propagation tests.
func testInsertFunctionDef() *FunctionDef {
	return &FunctionDef{
		ID:          "insert-fn-001",
		WorkspaceID: "ws-001",
		Name:        "Test Insert Function",
		Type:        FunctionTypeInsert,
		Code:        "function onTrack(event, settings) { return event; }",
		Version:     1,
		Settings:    map[string]string{"base_key": "base_value"},
	}
}

// mockInsertServer starts an httptest.Server that simulates the Transformer
// service's /v1/functions/insert endpoint. It decodes the request body into
// an insertFunctionRequest and delegates to handlerFn, which is responsible
// for writing the response using writeInsertResponse.
func mockInsertServer(
	t *testing.T,
	handlerFn func(w http.ResponseWriter, req insertFunctionRequest),
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/functions/insert", r.URL.Path)

		var req insertFunctionRequest
		err := jsonrs.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)

		handlerFn(w, req)
	}))
}

// writeInsertResponse encodes an insertFunctionResponse as JSON and writes it
// to w with status 200. Uses jsonrs per project linting rules.
func writeInsertResponse(t *testing.T, w http.ResponseWriter, resp insertFunctionResponse) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	err := jsonrs.NewEncoder(w).Encode(resp)
	require.NoError(t, err)
}

// testHandlerDispatch is the shared verification core for the 8 typed handler
// dispatch tests. It sends an event with the given eventType through the mock
// Transformer and asserts the correct handler name was dispatched.
func testHandlerDispatch(t *testing.T, eventType, expectedHandler, eventJSON string) {
	t.Helper()

	var receivedHandler string
	var receivedEventType string

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		receivedHandler = req.Handler
		receivedEventType = req.EventType
		writeInsertResponse(t, w, insertFunctionResponse{
			Event:   req.Event,
			Dropped: false,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()
	event := json.RawMessage(eventJSON)

	result, err := engine.ExecuteInsertFunction(context.Background(), fn, event, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Dropped)
	require.NotNil(t, result.Event)

	// Verify the Transformer received the correct handler name and event type
	require.Equal(t, expectedHandler, receivedHandler)
	require.Equal(t, eventType, receivedEventType)
}

// ===========================================================================
// Phase 1: Transformation Tests
// ===========================================================================

// TestExecuteInsertFunction_PassThrough verifies that when the insert function
// returns the event unmodified, the result contains the original event and
// Dropped is false.
func TestExecuteInsertFunction_PassThrough(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"track","event":"Purchase","userId":"u1","properties":{"price":42}}`)

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		// Return the event unchanged (pass-through)
		writeInsertResponse(t, w, insertFunctionResponse{
			Event:   req.Event,
			Dropped: false,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	result, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Dropped)

	// Compare as decoded maps to handle JSON key-ordering differences
	var inputMap, resultMap map[string]interface{}
	require.NoError(t, jsonrs.Unmarshal(inputEvent, &inputMap))
	require.NoError(t, jsonrs.Unmarshal(result.Event, &resultMap))
	require.Equal(t, inputMap, resultMap)
}

// TestExecuteInsertFunction_TransformEvent verifies that the insert function
// can modify the event by adding new properties. The result should contain
// the transformed event with the added property.
func TestExecuteInsertFunction_TransformEvent(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"track","event":"Purchase","properties":{"price":42}}`)
	transformedEvent := json.RawMessage(`{"type":"track","event":"Purchase","properties":{"price":42,"currency":"USD"}}`)

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		writeInsertResponse(t, w, insertFunctionResponse{
			Event:   transformedEvent,
			Dropped: false,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	result, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Dropped)

	var resultMap map[string]interface{}
	require.NoError(t, jsonrs.Unmarshal(result.Event, &resultMap))

	props, ok := resultMap["properties"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "USD", props["currency"])
	require.Equal(t, float64(42), props["price"])
}

// TestExecuteInsertFunction_AddContextField verifies that the insert function
// can enrich the event context with additional fields (e.g., campaign data).
func TestExecuteInsertFunction_AddContextField(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"track","event":"Purchase","userId":"u1","context":{"ip":"1.2.3.4"}}`)
	enrichedEvent := json.RawMessage(`{"type":"track","event":"Purchase","userId":"u1","context":{"ip":"1.2.3.4","campaign":{"source":"google","medium":"cpc","name":"spring_sale"}}}`)

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		writeInsertResponse(t, w, insertFunctionResponse{
			Event:   enrichedEvent,
			Dropped: false,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	result, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Dropped)

	var resultMap map[string]interface{}
	require.NoError(t, jsonrs.Unmarshal(result.Event, &resultMap))

	ctx, ok := resultMap["context"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "1.2.3.4", ctx["ip"])

	campaign, ok := ctx["campaign"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "google", campaign["source"])
	require.Equal(t, "cpc", campaign["medium"])
	require.Equal(t, "spring_sale", campaign["name"])
}

// ===========================================================================
// Phase 2: Event Drop Tests
// ===========================================================================

// TestExecuteInsertFunction_DropEvent verifies that the insert function can
// intentionally drop an event by returning a drop signal in the response body.
// Dropping is a valid action — no error should be returned.
func TestExecuteInsertFunction_DropEvent(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"track","event":"InternalTest","userId":"bot-1"}`)

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		writeInsertResponse(t, w, insertFunctionResponse{
			Dropped: true,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	result, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Dropped)
	require.Nil(t, result.Event)
}

// TestExecuteInsertFunction_DropEvent_ViaError verifies that a DropEvent error
// returned by the Transformer is treated as an intentional drop (not a failure).
// The result should indicate the event was dropped, with no error returned.
func TestExecuteInsertFunction_DropEvent_ViaError(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"track","event":"FilteredEvent","userId":"u1"}`)

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		writeInsertResponse(t, w, insertFunctionResponse{
			Error: &FunctionError{
				Type:    ErrorTypeDropEvent,
				Message: "event intentionally dropped by function logic",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	result, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	// DropEvent errors are NOT propagated as errors — they produce a drop result
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Dropped)
	require.Nil(t, result.Event)
}

// ===========================================================================
// Phase 3: Typed Handler Dispatch Tests (all 8 per AAP Rule 0.7.2)
// ===========================================================================

// TestExecuteInsertFunction_OnTrack verifies track events are routed to onTrack.
func TestExecuteInsertFunction_OnTrack(t *testing.T) {
	testHandlerDispatch(t, "track", "onTrack",
		`{"type":"track","event":"Purchase","userId":"u1","properties":{"price":42}}`)
}

// TestExecuteInsertFunction_OnIdentify verifies identify events are routed to onIdentify.
func TestExecuteInsertFunction_OnIdentify(t *testing.T) {
	testHandlerDispatch(t, "identify", "onIdentify",
		`{"type":"identify","userId":"u1","traits":{"email":"user@example.com","name":"John"}}`)
}

// TestExecuteInsertFunction_OnGroup verifies group events are routed to onGroup.
func TestExecuteInsertFunction_OnGroup(t *testing.T) {
	testHandlerDispatch(t, "group", "onGroup",
		`{"type":"group","userId":"u1","groupId":"g1","traits":{"name":"Acme","plan":"enterprise"}}`)
}

// TestExecuteInsertFunction_OnPage verifies page events are routed to onPage.
func TestExecuteInsertFunction_OnPage(t *testing.T) {
	testHandlerDispatch(t, "page", "onPage",
		`{"type":"page","userId":"u1","name":"Home","properties":{"url":"https://example.com"}}`)
}

// TestExecuteInsertFunction_OnScreen verifies screen events are routed to onScreen.
func TestExecuteInsertFunction_OnScreen(t *testing.T) {
	testHandlerDispatch(t, "screen", "onScreen",
		`{"type":"screen","userId":"u1","name":"Dashboard","properties":{"section":"analytics"}}`)
}

// TestExecuteInsertFunction_OnAlias verifies alias events are routed to onAlias.
func TestExecuteInsertFunction_OnAlias(t *testing.T) {
	testHandlerDispatch(t, "alias", "onAlias",
		`{"type":"alias","userId":"new_id","previousId":"old_id"}`)
}

// TestExecuteInsertFunction_OnDelete verifies delete events are routed to onDelete.
func TestExecuteInsertFunction_OnDelete(t *testing.T) {
	testHandlerDispatch(t, "delete", "onDelete",
		`{"type":"delete","userId":"u1"}`)
}

// TestExecuteInsertFunction_OnBatch verifies batch events are routed to onBatch.
func TestExecuteInsertFunction_OnBatch(t *testing.T) {
	testHandlerDispatch(t, "batch", "onBatch",
		`{"type":"batch","batch":[{"type":"track","event":"E1"},{"type":"identify","userId":"u2"}]}`)
}

// ===========================================================================
// Phase 4: Error Handling Tests
// ===========================================================================

// TestExecuteInsertFunction_RetryError verifies that a transient failure from
// the Transformer is surfaced as a *RetryError with IsRetryable() == true.
func TestExecuteInsertFunction_RetryError(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"track","event":"Test"}`)

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		writeInsertResponse(t, w, insertFunctionResponse{
			Error: &FunctionError{
				Type:    ErrorTypeRetryError,
				Message: "temporary network failure",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	_, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.Error(t, err)

	var retryErr *RetryError
	require.True(t, errors.As(err, &retryErr))
	require.True(t, retryErr.IsRetryable())
	require.Contains(t, retryErr.Message, "temporary network failure")
}

// TestExecuteInsertFunction_ValidationError verifies that a validation failure
// from the Transformer is surfaced as a *ValidationError.
func TestExecuteInsertFunction_ValidationError(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"track","event":"Test"}`)

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		writeInsertResponse(t, w, insertFunctionResponse{
			Error: &FunctionError{
				Type:    ErrorTypeValidationError,
				Message: "required field revenue missing",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	_, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.Error(t, err)

	var valErr *ValidationError
	require.True(t, errors.As(err, &valErr))
	require.False(t, valErr.IsRetryable())
	require.Contains(t, valErr.Message, "required field revenue missing")
}

// TestExecuteInsertFunction_EventNotSupported verifies that an event with an
// unrecognised type results in an *EventNotSupported error. This error occurs
// locally in resolveHandler before any HTTP call is made.
func TestExecuteInsertFunction_EventNotSupported(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"unknown","data":"test"}`)

	// The server should never be called — the error occurs before the HTTP request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not have been called for unsupported event type")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	_, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.Error(t, err)

	var ensErr *EventNotSupported
	require.True(t, errors.As(err, &ensErr))
	require.False(t, ensErr.IsRetryable())
	require.Contains(t, ensErr.Message, "unknown")
}

// TestExecuteInsertFunction_TransformerUnavailable verifies that a connection
// error is returned when the Transformer service is not reachable.
func TestExecuteInsertFunction_TransformerUnavailable(t *testing.T) {
	// Start and immediately close a server to get a valid URL that is no
	// longer accepting connections.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	engine := newTestEngine(t, closedURL)
	fn := testInsertFunctionDef()
	inputEvent := json.RawMessage(`{"type":"track","event":"Test"}`)

	_, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "executing insert function via Transformer")
}

// TestExecuteInsertFunction_ContextCancelled verifies that a cancelled context
// is propagated through the HTTP request, resulting in a context error.
func TestExecuteInsertFunction_ContextCancelled(t *testing.T) {
	// Create a mock server that blocks until the request context is cancelled.
	// This prevents goroutine leaks when the client aborts the request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()
	inputEvent := json.RawMessage(`{"type":"track","event":"Test"}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately — the HTTP call will fail with context.Canceled

	_, err := engine.ExecuteInsertFunction(ctx, fn, inputEvent, nil)
	require.Error(t, err)
}

// ===========================================================================
// Phase 5: Pipeline Integration Context Tests
// ===========================================================================

// TestExecuteInsertFunction_PreservesMetadata verifies that system metadata
// fields (messageId, timestamp, userId, type) are preserved through the
// transformation. Insert Functions should transform event data/properties but
// preserve system metadata.
func TestExecuteInsertFunction_PreservesMetadata(t *testing.T) {
	inputEvent := json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-abc-123","timestamp":"2024-01-15T10:30:00Z","userId":"u1","properties":{"price":42}}`)

	// The mock transformer modifies properties but preserves all metadata fields
	transformedEvent := json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-abc-123","timestamp":"2024-01-15T10:30:00Z","userId":"u1","properties":{"price":42,"currency":"USD"}}`)

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		writeInsertResponse(t, w, insertFunctionResponse{
			Event:   transformedEvent,
			Dropped: false,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef()

	result, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Dropped)

	var resultMap map[string]interface{}
	require.NoError(t, jsonrs.Unmarshal(result.Event, &resultMap))

	// Verify system metadata fields are preserved
	require.Equal(t, "msg-abc-123", resultMap["messageId"])
	require.Equal(t, "2024-01-15T10:30:00Z", resultMap["timestamp"])
	require.Equal(t, "u1", resultMap["userId"])
	require.Equal(t, "track", resultMap["type"])

	// Verify transformation was applied
	props, ok := resultMap["properties"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "USD", props["currency"])
	require.Equal(t, float64(42), props["price"])
}

// TestExecuteInsertFunction_SettingsPassedThrough verifies that both
// function-level settings (from FunctionDef.Settings) and per-invocation
// settings are merged and propagated to the Transformer service. Per-invocation
// settings override function-level settings for matching keys.
func TestExecuteInsertFunction_SettingsPassedThrough(t *testing.T) {
	var receivedSettings map[string]string

	srv := mockInsertServer(t, func(w http.ResponseWriter, req insertFunctionRequest) {
		receivedSettings = req.Settings
		writeInsertResponse(t, w, insertFunctionResponse{
			Event:   req.Event,
			Dropped: false,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testInsertFunctionDef() // Has Settings: {"base_key": "base_value"}
	inputEvent := json.RawMessage(`{"type":"track","event":"Test","userId":"u1"}`)
	callSettings := map[string]string{"enrichment_api": "https://enrich.example.com"}

	result, err := engine.ExecuteInsertFunction(context.Background(), fn, inputEvent, callSettings)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify merged settings were received by the Transformer
	require.Equal(t, "base_value", receivedSettings["base_key"])                        // From FunctionDef.Settings
	require.Equal(t, "https://enrich.example.com", receivedSettings["enrichment_api"]) // From call-level settings
}
