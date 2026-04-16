// Package runtime — destination_functions_test.go
//
// Unit tests for Destination Functions typed handler dispatch
// (Sprint 4-6, Epic E-016). Tests the Engine.ExecuteDestinationFunction
// entry point which dispatches events to typed handlers (onTrack, onIdentify,
// onGroup, onPage, onScreen, onAlias, onDelete, onBatch).
//
// All 8 Segment-compatible typed handlers are tested per AAP Rule 0.7.2:
//
//	onTrack, onIdentify, onGroup, onPage, onScreen, onAlias, onDelete, onBatch
//
// Tests use httptest.NewServer to mock the Transformer service's
// /v1/functions/destination endpoint, following the fakeTransformer pattern
// from processor/internal/transformer/user_transformer/user_transformer_test.go.
//
// Assertions use testify/require per repository convention for unit tests.
//
// NOTE: newTestEngine is defined in insert_functions_test.go and reused here
// because both files belong to the same package (runtime). This avoids
// duplicate helper definitions while ensuring consistent Engine construction
// across all function-type test suites.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
)

// ===========================================================================
// Destination Function Test Helpers
// ===========================================================================

// testDestFunctionDef returns a minimal FunctionDef configured for destination
// function testing. The Settings field contains a base key that will be merged
// with per-invocation settings in settings propagation tests.
func testDestFunctionDef() *FunctionDef {
	return &FunctionDef{
		ID:          "dest-func-001",
		WorkspaceID: "ws-test-001",
		Name:        "Test Destination Function",
		Type:        FunctionTypeDestination,
		Code:        `function onTrack(event, settings) { return { statusCode: 200, body: { ok: true } }; }`,
		Version:     1,
		Settings:    map[string]string{"base_url": "https://api.example.com"},
	}
}

// mockDestServer starts an httptest.Server that simulates the Transformer
// service's /v1/functions/destination endpoint. It decodes the request body
// into a destinationFunctionRequest and delegates to handlerFn, which is
// responsible for writing the response using writeDestResponse.
func mockDestServer(
	t *testing.T,
	handlerFn func(w http.ResponseWriter, req destinationFunctionRequest),
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/functions/destination", r.URL.Path)

		var req destinationFunctionRequest
		err := jsonrs.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)

		handlerFn(w, req)
	}))
}

// writeDestResponse encodes a destinationFunctionResponse as JSON and writes
// it to w with HTTP status 200 (Transformer-level success). Function-level
// status codes and errors are conveyed inside the JSON body. Uses jsonrs per
// project linting rules.
func writeDestResponse(t *testing.T, w http.ResponseWriter, resp destinationFunctionResponse) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	err := jsonrs.NewEncoder(w).Encode(resp)
	require.NoError(t, err)
}

// testDestHandlerDispatch is the shared verification core for the 8 typed
// handler dispatch tests. It sends an event with the given eventType through
// the mock Transformer and asserts the correct handler name was dispatched,
// and that the result is successful with a 200 status code.
func testDestHandlerDispatch(t *testing.T, eventType, expectedHandler string, eventJSON string) {
	t.Helper()

	var receivedHandler string
	var receivedEventType string
	var receivedFunctionID string

	responseBody := json.RawMessage(`{"delivered":true}`)

	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		receivedHandler = req.Handler
		receivedEventType = req.EventType
		receivedFunctionID = req.FunctionID
		writeDestResponse(t, w, destinationFunctionResponse{
			StatusCode: http.StatusOK,
			Body:       responseBody,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(eventJSON)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, eventType, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, result.StatusCode)
	require.NotNil(t, result.Body)

	// Verify the Transformer received the correct handler name and event type
	require.Equal(t, expectedHandler, receivedHandler)
	require.Equal(t, eventType, receivedEventType)
	require.Equal(t, fn.ID, receivedFunctionID)

	// Verify the result body matches the mock response
	var bodyMap map[string]interface{}
	require.NoError(t, jsonrs.Unmarshal(result.Body, &bodyMap))
	require.Equal(t, true, bodyMap["delivered"])
}

// ===========================================================================
// Phase 1: Typed Handler Dispatch Tests — ALL 8 handlers (AAP Rule 0.7.2)
// ===========================================================================

// TestExecuteDestinationFunction_OnTrack verifies that a track event with
// realistic payload (Order Completed with properties) is dispatched to the
// onTrack handler. Track events are the most common event type and carry
// event name plus arbitrary properties.
func TestExecuteDestinationFunction_OnTrack(t *testing.T) {
	testDestHandlerDispatch(t, "track", "onTrack",
		`{"type":"track","event":"Order Completed","userId":"user123","properties":{"orderId":"ORD-001","revenue":99.99}}`)
}

// TestExecuteDestinationFunction_OnIdentify verifies that an identify event
// with traits (email, name) is dispatched to the onIdentify handler.
// Identify events update user traits in downstream destinations.
func TestExecuteDestinationFunction_OnIdentify(t *testing.T) {
	testDestHandlerDispatch(t, "identify", "onIdentify",
		`{"type":"identify","userId":"user123","traits":{"email":"user@example.com","name":"John Doe"}}`)
}

// TestExecuteDestinationFunction_OnGroup verifies that a group event with
// groupId and traits is dispatched to the onGroup handler. Group events
// associate users with organisations or teams.
func TestExecuteDestinationFunction_OnGroup(t *testing.T) {
	testDestHandlerDispatch(t, "group", "onGroup",
		`{"type":"group","userId":"user123","groupId":"group456","traits":{"name":"Acme Corp","plan":"enterprise"}}`)
}

// TestExecuteDestinationFunction_OnPage verifies that a page event is
// dispatched to the onPage handler. Page events track website page views
// and carry page name and URL properties.
func TestExecuteDestinationFunction_OnPage(t *testing.T) {
	testDestHandlerDispatch(t, "page", "onPage",
		`{"type":"page","userId":"user123","name":"Home","properties":{"url":"https://example.com"}}`)
}

// TestExecuteDestinationFunction_OnScreen verifies that a screen event is
// dispatched to the onScreen handler. Screen events are the mobile
// equivalent of page events, tracking app screen views.
func TestExecuteDestinationFunction_OnScreen(t *testing.T) {
	testDestHandlerDispatch(t, "screen", "onScreen",
		`{"type":"screen","userId":"user123","name":"Dashboard","properties":{"section":"analytics"}}`)
}

// TestExecuteDestinationFunction_OnAlias verifies that an alias event
// linking a new user ID to a previous ID is dispatched to the onAlias
// handler. Alias events merge user identities in downstream systems.
func TestExecuteDestinationFunction_OnAlias(t *testing.T) {
	testDestHandlerDispatch(t, "alias", "onAlias",
		`{"type":"alias","userId":"new_id","previousId":"old_id"}`)
}

// TestExecuteDestinationFunction_OnDelete verifies that a delete event
// for GDPR/CCPA compliance is dispatched to the onDelete handler.
// Delete events instruct destinations to purge user data.
func TestExecuteDestinationFunction_OnDelete(t *testing.T) {
	testDestHandlerDispatch(t, "delete", "onDelete",
		`{"type":"delete","userId":"user123"}`)
}

// TestExecuteDestinationFunction_OnBatch verifies that a batch event
// containing multiple sub-events is dispatched to the onBatch handler.
// Batch events allow bulk delivery to destinations.
func TestExecuteDestinationFunction_OnBatch(t *testing.T) {
	testDestHandlerDispatch(t, "batch", "onBatch",
		`{"type":"batch","batch":[{"type":"track","event":"Event1"},{"type":"identify","userId":"u2"}]}`)
}

// ===========================================================================
// Phase 2: Error Handling Tests
// ===========================================================================

// TestExecuteDestinationFunction_EventNotSupported verifies that an
// unrecognised event type (not in the eventTypeToHandler map) returns an
// *EventNotSupported error without making any HTTP call to the Transformer.
// This validates local input validation before network communication.
func TestExecuteDestinationFunction_EventNotSupported(t *testing.T) {
	// The server should never be called — the error occurs before the HTTP
	// request is created, during handler lookup in the eventTypeToHandler map.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not have been called for unsupported event type")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"unknown_type","data":"test"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "unknown_type", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var ensErr *EventNotSupported
	require.True(t, errors.As(err, &ensErr))
	require.False(t, ensErr.IsRetryable())
	require.Contains(t, ensErr.Message, "unknown_type")
}

// TestExecuteDestinationFunction_InvalidEventPayload verifies that an empty
// event payload (zero-length json.RawMessage) returns an *InvalidEventPayload
// error. The production code checks len(event) == 0 before any processing.
func TestExecuteDestinationFunction_InvalidEventPayload(t *testing.T) {
	// The server should never be called — the error occurs during input
	// validation before the HTTP request is built.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not have been called for invalid event payload")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	// Empty RawMessage — len(event) == 0 triggers InvalidEventPayload
	event := json.RawMessage{}

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var ipErr *InvalidEventPayload
	require.True(t, errors.As(err, &ipErr))
	require.False(t, ipErr.IsRetryable())
	require.Contains(t, ipErr.Message, "event payload is required")
}

// TestExecuteDestinationFunction_DropEvent verifies that when the Transformer
// returns a DropEvent function error, it is surfaced as a *DropEvent Go error.
// Dropping is an intentional action — the function chose to discard the event.
// IsRetryable must return false (do not retry drops).
func TestExecuteDestinationFunction_DropEvent(t *testing.T) {
	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		writeDestResponse(t, w, destinationFunctionResponse{
			Error: &FunctionError{
				Type:    ErrorTypeDropEvent,
				Message: "event intentionally dropped by function logic",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"InternalTest","userId":"bot-1"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var dropErr *DropEvent
	require.True(t, errors.As(err, &dropErr))
	require.False(t, dropErr.IsRetryable())
	require.Contains(t, dropErr.Message, "event intentionally dropped")
}

// TestExecuteDestinationFunction_RetryError verifies that a transient failure
// from the Transformer is surfaced as a *RetryError with IsRetryable() == true.
// This allows the pipeline to retry delivery to the destination function.
func TestExecuteDestinationFunction_RetryError(t *testing.T) {
	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		writeDestResponse(t, w, destinationFunctionResponse{
			Error: &FunctionError{
				Type:    ErrorTypeRetryError,
				Message: "temporary destination unavailable (HTTP 429)",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Purchase","userId":"u1"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var retryErr *RetryError
	require.True(t, errors.As(err, &retryErr))
	require.True(t, retryErr.IsRetryable())
	require.Contains(t, retryErr.Message, "temporary destination unavailable")
}

// TestExecuteDestinationFunction_ValidationError verifies that when the
// Transformer returns a ValidationError, it is surfaced as a *ValidationError
// Go error. Validation errors indicate the event failed schema or business
// rule checks inside the destination function. IsRetryable must be false.
func TestExecuteDestinationFunction_ValidationError(t *testing.T) {
	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		writeDestResponse(t, w, destinationFunctionResponse{
			Error: &FunctionError{
				Type:    ErrorTypeValidationError,
				Message: "required field 'email' missing in traits",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"identify","userId":"u1","traits":{"name":"Jane"}}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "identify", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var valErr *ValidationError
	require.True(t, errors.As(err, &valErr))
	require.False(t, valErr.IsRetryable())
	require.Contains(t, valErr.Message, "required field 'email' missing")
}

// ===========================================================================
// Phase 3: Settings and Context Tests
// ===========================================================================

// TestExecuteDestinationFunction_SettingsPassedThrough verifies that both
// function-level settings (from FunctionDef.Settings) and per-invocation
// call-level settings are merged and propagated to the Transformer service.
// Per-invocation settings override function-level settings for matching keys.
func TestExecuteDestinationFunction_SettingsPassedThrough(t *testing.T) {
	var receivedSettings map[string]string

	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		receivedSettings = req.Settings
		writeDestResponse(t, w, destinationFunctionResponse{
			StatusCode: http.StatusOK,
			Body:       json.RawMessage(`{"ok":true}`),
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef() // Has Settings: {"base_url": "https://api.example.com"}

	event := json.RawMessage(`{"type":"track","event":"Test","userId":"u1"}`)
	callSettings := map[string]string{
		"api_endpoint": "https://api.dest.com",
		"auth_token":   "bearer_xyz",
	}

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", callSettings,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, result.StatusCode)

	// Verify function-level settings from FunctionDef are present
	require.Equal(t, "https://api.example.com", receivedSettings["base_url"])
	// Verify call-level settings are present
	require.Equal(t, "https://api.dest.com", receivedSettings["api_endpoint"])
	require.Equal(t, "bearer_xyz", receivedSettings["auth_token"])
}

// TestExecuteDestinationFunction_ContextCancellation verifies that a
// cancelled context is propagated through the HTTP request, resulting in a
// context error. This ensures the function respects pipeline shutdown
// signals and does not hang indefinitely.
func TestExecuteDestinationFunction_ContextCancellation(t *testing.T) {
	// Create a mock server that blocks until the request context is done.
	// This simulates a slow Transformer response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Test","userId":"u1"}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately — the HTTP call will fail with context.Canceled

	result, err := engine.ExecuteDestinationFunction(
		ctx, fn, event, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)
}

// TestExecuteDestinationFunction_NilEvent verifies that passing a nil
// json.RawMessage as the event returns an *InvalidEventPayload error and
// does not panic. A nil slice has len 0, triggering the same validation
// path as an empty slice.
func TestExecuteDestinationFunction_NilEvent(t *testing.T) {
	// The server should never be called — nil event is caught during
	// input validation before any HTTP call is made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not have been called for nil event")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()

	// Explicitly pass nil — must not panic, must return error
	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, nil, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var ipErr *InvalidEventPayload
	require.True(t, errors.As(err, &ipErr))
	require.Contains(t, ipErr.Message, "event payload is required")
}

// ===========================================================================
// Phase 4: Response Handling Tests
// ===========================================================================

// TestExecuteDestinationFunction_NonOKStatusCode verifies that when the
// Transformer service itself returns a non-200 HTTP status (e.g. 500
// Internal Server Error), the error is propagated with the status code and
// response body included in the error message.
func TestExecuteDestinationFunction_NonOKStatusCode(t *testing.T) {
	// Bypass mockDestServer — we need to control the HTTP status code
	// directly. The production code checks resp.StatusCode != http.StatusOK
	// before attempting to parse the JSON body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"transformer internal error"}`))
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Test","userId":"u1"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
}

// TestExecuteDestinationFunction_EmptyBody verifies that when the destination
// function returns a 200 status with an empty body, the result reflects
// statusCode=200 and Body is empty/nil. This covers fire-and-forget
// delivery patterns where the destination confirms receipt without a payload.
func TestExecuteDestinationFunction_EmptyBody(t *testing.T) {
	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		writeDestResponse(t, w, destinationFunctionResponse{
			StatusCode: http.StatusOK,
			Body:       nil, // Empty body — function returned no payload
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Notification Sent","userId":"u1"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, result.StatusCode)
	// Body should be nil or empty — no payload returned by the function.
	// In Go, len(nil) == 0, so a single length check covers both nil and
	// empty slices. The "null" JSON literal is also valid for absent bodies.
	require.True(t, len(result.Body) == 0 || string(result.Body) == "null",
		"expected empty/nil/null body, got: %s", string(result.Body))
}

// ===========================================================================
// Additional Edge Case Tests
// ===========================================================================

// TestExecuteDestinationFunction_NilFunctionDef verifies that passing a nil
// FunctionDef returns an error describing the missing function definition.
// This guard prevents nil pointer dereferences in the execution path.
func TestExecuteDestinationFunction_NilFunctionDef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not have been called for nil function def")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	event := json.RawMessage(`{"type":"track","event":"Test"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), nil, event, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "function definition is required")
}

// TestExecuteDestinationFunction_EmptyEventType verifies that passing an
// empty eventType string returns an *InvalidEventPayload error. The
// eventType is required for handler dispatch.
func TestExecuteDestinationFunction_EmptyEventType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not have been called for empty event type")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Test"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var ipErr *InvalidEventPayload
	require.True(t, errors.As(err, &ipErr))
	require.Contains(t, ipErr.Message, "event type is required")
}

// TestExecuteDestinationFunction_SettingsOverride verifies that call-level
// settings override function-level settings when keys overlap. The
// mergeSettings function gives precedence to the second map (call-level).
func TestExecuteDestinationFunction_SettingsOverride(t *testing.T) {
	var receivedSettings map[string]string

	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		receivedSettings = req.Settings
		writeDestResponse(t, w, destinationFunctionResponse{
			StatusCode: http.StatusOK,
			Body:       json.RawMessage(`{"ok":true}`),
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef() // Settings: {"base_url": "https://api.example.com"}

	event := json.RawMessage(`{"type":"track","event":"Test"}`)
	// Override base_url from function-level settings
	callSettings := map[string]string{
		"base_url": "https://override.example.com",
	}

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", callSettings,
	)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Call-level "base_url" should override function-level "base_url"
	require.Equal(t, "https://override.example.com", receivedSettings["base_url"])
}

// TestExecuteDestinationFunction_FunctionCodePassedThrough verifies that the
// function code from FunctionDef is correctly propagated to the Transformer
// service in the request payload.
func TestExecuteDestinationFunction_FunctionCodePassedThrough(t *testing.T) {
	var receivedCode string
	var receivedVersion int

	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		receivedCode = req.Code
		receivedVersion = req.Version
		writeDestResponse(t, w, destinationFunctionResponse{
			StatusCode: http.StatusOK,
			Body:       json.RawMessage(`{"ok":true}`),
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Test"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, fn.Code, receivedCode)
	require.Equal(t, fn.Version, receivedVersion)
}

// TestExecuteDestinationFunction_TransformerUnavailable verifies that a
// connection failure to the Transformer service is surfaced as an error.
func TestExecuteDestinationFunction_TransformerUnavailable(t *testing.T) {
	// Start and immediately close a server to get a valid URL that is no
	// longer accepting connections.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	engine := newTestEngine(t, closedURL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Test"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "posting destination function to transformer")
}

// TestExecuteDestinationFunction_InvalidEventPayloadError_FromTransformer
// verifies that when the Transformer returns an InvalidEventPayload function
// error in the response body, it is correctly mapped to a *InvalidEventPayload
// Go error. This tests the error mapping path through mapFunctionError.
func TestExecuteDestinationFunction_InvalidEventPayloadError_FromTransformer(t *testing.T) {
	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		writeDestResponse(t, w, destinationFunctionResponse{
			Error: &FunctionError{
				Type:    ErrorTypeInvalidEventPayload,
				Message: "event missing required 'userId' field",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Test","properties":{}}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var ipErr *InvalidEventPayload
	require.True(t, errors.As(err, &ipErr))
	require.False(t, ipErr.IsRetryable())
	require.Contains(t, ipErr.Message, "event missing required 'userId' field")
}

// TestExecuteDestinationFunction_NonOKFunctionStatusCode verifies that when
// the destination function itself returns a non-200 status code (e.g. 400)
// but the Transformer HTTP layer is healthy (200), the result correctly
// reflects the function-level status code and body.
func TestExecuteDestinationFunction_NonOKFunctionStatusCode(t *testing.T) {
	errorBody := json.RawMessage(`{"error":"bad request from destination API","code":"INVALID_PARAM"}`)

	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		writeDestResponse(t, w, destinationFunctionResponse{
			StatusCode: http.StatusBadRequest,
			Body:       errorBody,
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef()
	event := json.RawMessage(`{"type":"track","event":"Test","userId":"u1"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusBadRequest, result.StatusCode)

	var bodyMap map[string]interface{}
	require.NoError(t, jsonrs.Unmarshal(result.Body, &bodyMap))
	require.Equal(t, "bad request from destination API", bodyMap["error"])
	require.Equal(t, "INVALID_PARAM", bodyMap["code"])
}

// TestExecuteDestinationFunction_NilCallSettings verifies that passing nil
// for the call-level settings map still correctly propagates the function-
// level settings from the FunctionDef. The mergeSettings function must
// handle nil gracefully.
func TestExecuteDestinationFunction_NilCallSettings(t *testing.T) {
	var receivedSettings map[string]string

	srv := mockDestServer(t, func(w http.ResponseWriter, req destinationFunctionRequest) {
		receivedSettings = req.Settings
		writeDestResponse(t, w, destinationFunctionResponse{
			StatusCode: http.StatusOK,
			Body:       json.RawMessage(`{"ok":true}`),
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testDestFunctionDef() // Settings: {"base_url": "https://api.example.com"}
	event := json.RawMessage(`{"type":"track","event":"Test"}`)

	result, err := engine.ExecuteDestinationFunction(
		context.Background(), fn, event, "track", nil, // nil call settings
	)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Function-level settings should be present even with nil call settings
	require.Equal(t, "https://api.example.com", receivedSettings["base_url"])
}
