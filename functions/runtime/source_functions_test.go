// Package runtime — source_functions_test.go
//
// Unit tests for Source Functions onRequest(request, settings) handler
// (Sprint 4-6, Epic E-015). Tests the Engine.ExecuteSourceFunction entry
// point which receives an HTTP webhook request representation and produces
// one or more RudderStack events (track, identify, group, page, screen).
//
// Test phases:
//
//  1. Happy Path   — single event, multiple events, settings propagation
//  2. Request Handling — GET request, headers preserved, empty body
//  3. Error Handling   — typed errors, nil request, unavailable transformer
//  4. Edge Cases       — empty events, large payload, context cancellation
//
// Tests use httptest.NewServer to mock the Transformer service's
// /v1/functions/source endpoint, following the fakeTransformer pattern from
// processor/internal/transformer/user_transformer/user_transformer_test.go.
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
)

// ===========================================================================
// Source Function Test Helpers
// ===========================================================================

// testSourceFunctionDef returns a minimal FunctionDef configured for source
// function testing. The Settings field contains a base key that will be merged
// with per-invocation settings in settings propagation tests.
func testSourceFunctionDef() *FunctionDef {
	return &FunctionDef{
		ID:          "src-func-001",
		WorkspaceID: "ws-test-001",
		Name:        "Test Source Function",
		Type:        FunctionTypeSource,
		Code:        `function onRequest(request, settings) { return []; }`,
		Version:     1,
		Settings:    map[string]string{"base_key": "base_value"},
	}
}

// mockSourceServer starts an httptest.Server that simulates the Transformer
// service's /v1/functions/source endpoint. It decodes the request body into
// a transformerRequest and delegates to handlerFn, which is responsible for
// writing the response using writeSourceResponse.
//
// The mock verifies that:
//   - The HTTP method is POST
//   - The URL path is /v1/functions/source
//   - The request body decodes into a valid transformerRequest
func mockSourceServer(
	t *testing.T,
	handlerFn func(w http.ResponseWriter, req transformerRequest),
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/functions/source", r.URL.Path)

		var req transformerRequest
		err := jsonrs.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)

		handlerFn(w, req)
	}))
}

// writeSourceResponse encodes a transformerResponse as JSON and writes it to
// w with HTTP status 200 (Transformer-level success). Function-level errors
// are conveyed inside the JSON body via the Error field. Uses jsonrs per
// project linting rules (.golangci.yml forbids encoding/json functions).
func writeSourceResponse(t *testing.T, w http.ResponseWriter, resp transformerResponse) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	err := jsonrs.NewEncoder(w).Encode(resp)
	require.NoError(t, err)
}

// makeTrackEvent builds a minimal RudderStack track event JSON suitable for
// use in Source Function test results.
func makeTrackEvent(userID, eventName string) json.RawMessage {
	evt := map[string]interface{}{
		"type":   "track",
		"userId": userID,
		"event":  eventName,
		"properties": map[string]interface{}{
			"source": "source_function_test",
		},
	}
	data, _ := jsonrs.Marshal(evt)
	return json.RawMessage(data)
}

// makeIdentifyEvent builds a minimal RudderStack identify event JSON.
func makeIdentifyEvent(userID, email string) json.RawMessage {
	evt := map[string]interface{}{
		"type":   "identify",
		"userId": userID,
		"traits": map[string]interface{}{
			"email": email,
		},
	}
	data, _ := jsonrs.Marshal(evt)
	return json.RawMessage(data)
}

// makeGroupEvent builds a minimal RudderStack group event JSON.
func makeGroupEvent(userID, groupID string) json.RawMessage {
	evt := map[string]interface{}{
		"type":    "group",
		"userId":  userID,
		"groupId": groupID,
		"traits": map[string]interface{}{
			"name": "Acme Corp",
		},
	}
	data, _ := jsonrs.Marshal(evt)
	return json.RawMessage(data)
}

// ===========================================================================
// Phase 1: Happy Path Tests
// ===========================================================================

// TestExecuteSourceFunction_SingleEvent verifies that a Source Function
// returning a single track event produces a SourceFunctionResult with exactly
// one event. This is the most common happy-path scenario: a webhook triggers
// a single RudderStack event.
func TestExecuteSourceFunction_SingleEvent(t *testing.T) {
	expectedEvent := makeTrackEvent("user123", "purchase")

	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		// Verify the Transformer received the correct function metadata.
		require.Equal(t, "src-func-001", req.FunctionID)
		require.Equal(t, "ws-test-001", req.WorkspaceID)
		require.Equal(t, "onRequest", req.Handler)
		require.NotNil(t, req.Request)

		// Verify the request payload contains the webhook data.
		var srcReq SourceFunctionRequest
		err := jsonrs.Unmarshal(req.Request, &srcReq)
		require.NoError(t, err)
		require.Equal(t, "POST", srcReq.Method)
		require.Equal(t, "/webhook/custom", srcReq.URL)

		writeSourceResponse(t, w, transformerResponse{
			Events: []json.RawMessage{expectedEvent},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method:  "POST",
		URL:     "/webhook/custom",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    json.RawMessage(`{"userId":"user123","event":"purchase"}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Events, 1)

	// Verify the returned event is valid JSON with the expected structure.
	var eventMap map[string]interface{}
	require.NoError(t, jsonrs.Unmarshal(result.Events[0], &eventMap))
	require.Equal(t, "track", eventMap["type"])
	require.Equal(t, "user123", eventMap["userId"])
	require.Equal(t, "purchase", eventMap["event"])
}

// TestExecuteSourceFunction_MultipleEvents verifies that a Source Function
// can produce multiple events from a single webhook request. The onRequest
// handler returns 3 events: a track, an identify, and a group call.
func TestExecuteSourceFunction_MultipleEvents(t *testing.T) {
	trackEvt := makeTrackEvent("user456", "signup")
	identifyEvt := makeIdentifyEvent("user456", "user@example.com")
	groupEvt := makeGroupEvent("user456", "grp-789")

	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		require.Equal(t, "onRequest", req.Handler)
		writeSourceResponse(t, w, transformerResponse{
			Events: []json.RawMessage{trackEvt, identifyEvt, groupEvt},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method:  "POST",
		URL:     "/webhook/stripe",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    json.RawMessage(`{"type":"customer.created","data":{"id":"cus_123"}}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Events, 3)

	// Verify each event is properly formed JSON with the expected type.
	eventTypes := make([]string, 3)
	for i, evt := range result.Events {
		var m map[string]interface{}
		require.NoError(t, jsonrs.Unmarshal(evt, &m))
		require.NotNil(t, m["type"])
		eventTypes[i] = m["type"].(string)
	}
	require.Equal(t, "track", eventTypes[0])
	require.Equal(t, "identify", eventTypes[1])
	require.Equal(t, "group", eventTypes[2])
}

// TestExecuteSourceFunction_WithSettings verifies that both function-level
// settings (from FunctionDef.Settings) and call-level settings are merged and
// passed to the Transformer. Call-level settings take precedence.
func TestExecuteSourceFunction_WithSettings(t *testing.T) {
	var receivedSettings map[string]string

	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		receivedSettings = req.Settings
		writeSourceResponse(t, w, transformerResponse{
			Events: []json.RawMessage{makeTrackEvent("u1", "test")},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()
	// fn.Settings already has {"base_key": "base_value"}

	callSettings := map[string]string{
		"api_key":        "sk_test_123",
		"webhook_secret": "whsec_abc",
	}

	request := &SourceFunctionRequest{
		Method: "POST",
		URL:    "/webhook/custom",
		Body:   json.RawMessage(`{"data":"test"}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, callSettings,
	)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify that both function-level and call-level settings were merged.
	require.NotNil(t, receivedSettings)
	require.Equal(t, "base_value", receivedSettings["base_key"])
	require.Equal(t, "sk_test_123", receivedSettings["api_key"])
	require.Equal(t, "whsec_abc", receivedSettings["webhook_secret"])
}

// ===========================================================================
// Phase 2: Request Handling Tests
// ===========================================================================

// TestExecuteSourceFunction_GETRequest verifies that Source Functions correctly
// handle GET webhooks. GET requests have no body but carry query parameters.
// Some webhook providers use GET for notification-style callbacks.
func TestExecuteSourceFunction_GETRequest(t *testing.T) {
	var receivedReq SourceFunctionRequest

	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		// Decode the embedded request to verify GET handling.
		err := jsonrs.Unmarshal(req.Request, &receivedReq)
		require.NoError(t, err)
		writeSourceResponse(t, w, transformerResponse{
			Events: []json.RawMessage{makeTrackEvent("u1", "signup")},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method: "GET",
		URL:    "/webhook?event=signup",
		QueryParams: map[string][]string{
			"event": {"signup"},
			"user":  {"u1"},
		},
		// Body is nil for GET requests — Source Functions must tolerate this.
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Events, 1)

	// Verify the Transformer received the correct method and query params.
	require.Equal(t, "GET", receivedReq.Method)
	require.Equal(t, "/webhook?event=signup", receivedReq.URL)
	// Body was nil in the original request; after JSON marshal (nil → null)
	// and unmarshal (null → json.RawMessage("null")), the body contains the
	// JSON null literal rather than Go nil.
	require.True(t,
		receivedReq.Body == nil || string(receivedReq.Body) == "null",
		"expected nil or JSON null body, got: %s", string(receivedReq.Body),
	)
	require.Equal(t, []string{"signup"}, receivedReq.QueryParams["event"])
	require.Equal(t, []string{"u1"}, receivedReq.QueryParams["user"])
}

// TestExecuteSourceFunction_HeadersPreserved verifies that custom HTTP headers
// from the incoming webhook are faithfully preserved and passed to the
// Transformer. Headers like X-Webhook-Signature, X-Request-ID, and
// Authorization are critical for webhook signature validation.
func TestExecuteSourceFunction_HeadersPreserved(t *testing.T) {
	var receivedReq SourceFunctionRequest

	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		err := jsonrs.Unmarshal(req.Request, &receivedReq)
		require.NoError(t, err)
		writeSourceResponse(t, w, transformerResponse{
			Events: []json.RawMessage{makeTrackEvent("u1", "hook")},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	customHeaders := map[string][]string{
		"Content-Type":        {"application/json"},
		"X-Webhook-Signature": {"sha256=abc123def456"},
		"X-Request-ID":        {"req-id-789"},
		"Authorization":       {"Bearer token_xyz"},
	}

	request := &SourceFunctionRequest{
		Method:  "POST",
		URL:     "/webhook/github",
		Headers: customHeaders,
		Body:    json.RawMessage(`{"action":"push"}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.TODO(), fn, request, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify all custom headers were preserved unmodified.
	require.Equal(t, []string{"application/json"}, receivedReq.Headers["Content-Type"])
	require.Equal(t, []string{"sha256=abc123def456"}, receivedReq.Headers["X-Webhook-Signature"])
	require.Equal(t, []string{"req-id-789"}, receivedReq.Headers["X-Request-ID"])
	require.Equal(t, []string{"Bearer token_xyz"}, receivedReq.Headers["Authorization"])
}

// TestExecuteSourceFunction_EmptyBody verifies that Source Functions handle
// webhooks without a body (e.g., notification-only POST requests or health
// check pings). Body is nil but execution should succeed.
func TestExecuteSourceFunction_EmptyBody(t *testing.T) {
	var receivedReq SourceFunctionRequest

	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		err := jsonrs.Unmarshal(req.Request, &receivedReq)
		require.NoError(t, err)
		writeSourceResponse(t, w, transformerResponse{
			Events: []json.RawMessage{makeTrackEvent("u1", "ping")},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method:  "POST",
		URL:     "/webhook/healthcheck",
		Headers: map[string][]string{"Content-Type": {"text/plain"}},
		Body:    nil, // Explicitly nil body
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Events, 1)

	// Verify the nil body was serialised correctly (becomes JSON null).
	// After JSON round-trip, nil Body becomes json.RawMessage("null").
	require.True(t,
		receivedReq.Body == nil || string(receivedReq.Body) == "null",
		"expected nil or JSON null body, got: %s", string(receivedReq.Body),
	)
}

// ===========================================================================
// Phase 3: Error Handling Tests
// ===========================================================================

// TestExecuteSourceFunction_FunctionError_EventNotSupported verifies that
// when the Transformer returns an EventNotSupported error (e.g. the Source
// Function's onRequest handler is not implemented), the returned error wraps
// *EventNotSupported and IsRetryable() returns false.
func TestExecuteSourceFunction_FunctionError_EventNotSupported(t *testing.T) {
	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		writeSourceResponse(t, w, transformerResponse{
			Error: &FunctionError{
				Type:    ErrorTypeEventNotSupported,
				Message: "onRequest handler not found in function code",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method: "POST",
		URL:    "/webhook/test",
		Body:   json.RawMessage(`{"data":"test"}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var ensErr *EventNotSupported
	require.True(t, errors.As(err, &ensErr))
	require.False(t, ensErr.IsRetryable())
	require.Contains(t, ensErr.Message, "onRequest handler not found")
}

// TestExecuteSourceFunction_FunctionError_RetryError verifies that when
// the Transformer returns a RetryError (transient failure), the returned
// error wraps *RetryError and IsRetryable() returns true, signalling that
// the caller should schedule a retry with backoff.
func TestExecuteSourceFunction_FunctionError_RetryError(t *testing.T) {
	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		writeSourceResponse(t, w, transformerResponse{
			Error: &FunctionError{
				Type:    ErrorTypeRetryError,
				Message: "temporary network failure calling external API",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method: "POST",
		URL:    "/webhook/test",
		Body:   json.RawMessage(`{"data":"test"}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var retryErr *RetryError
	require.True(t, errors.As(err, &retryErr))
	require.True(t, retryErr.IsRetryable())
	require.Contains(t, retryErr.Message, "temporary network failure")
}

// TestExecuteSourceFunction_FunctionError_ValidationError verifies that when
// the Transformer returns a ValidationError (invalid request), the error
// wraps *ValidationError and IsRetryable() returns false.
func TestExecuteSourceFunction_FunctionError_ValidationError(t *testing.T) {
	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		writeSourceResponse(t, w, transformerResponse{
			Error: &FunctionError{
				Type:    ErrorTypeValidationError,
				Message: "missing required field: userId",
			},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method: "POST",
		URL:    "/webhook/test",
		Body:   json.RawMessage(`{"incomplete":"data"}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.Nil(t, result)
	require.Error(t, err)

	var valErr *ValidationError
	require.True(t, errors.As(err, &valErr))
	require.False(t, valErr.IsRetryable())
	require.Contains(t, valErr.Message, "missing required field")
}

// TestExecuteSourceFunction_NilRequest verifies that passing a nil
// SourceFunctionRequest returns a descriptive error (InvalidEventPayload)
// and does not panic. The production code checks request == nil at the
// top of ExecuteSourceFunction.
func TestExecuteSourceFunction_NilRequest(t *testing.T) {
	// The mock server should never be called when request is nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mock server should not have been called for nil request")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, nil, nil,
	)
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "request is required")
}

// TestExecuteSourceFunction_TransformerUnavailable verifies that when the
// Transformer service is not reachable (e.g. service down or wrong URL),
// the error is propagated with a descriptive message containing the
// connection failure details.
func TestExecuteSourceFunction_TransformerUnavailable(t *testing.T) {
	// Start and immediately close a server to get an unreachable URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	badURL := srv.URL
	srv.Close() // Server is now unreachable.

	engine := newTestEngine(t, badURL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method: "POST",
		URL:    "/webhook/test",
		Body:   json.RawMessage(`{"data":"test"}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.Nil(t, result)
	require.Error(t, err)
	// The error should indicate a connection failure.
	require.True(t,
		strings.Contains(err.Error(), "connection refused") ||
			strings.Contains(err.Error(), "dial") ||
			strings.Contains(err.Error(), "posting source function") ||
			strings.Contains(err.Error(), "transformer"),
		"expected connection error, got: %s", err.Error(),
	)
}

// ===========================================================================
// Phase 4: Edge Cases
// ===========================================================================

// TestExecuteSourceFunction_EmptyEventsReturned verifies that when a Source
// Function returns no events (empty array), the result has an empty Events
// slice (length 0) and no error is returned. This is valid for Source
// Functions that filter out requests (e.g. health-check pings or duplicates).
func TestExecuteSourceFunction_EmptyEventsReturned(t *testing.T) {
	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		writeSourceResponse(t, w, transformerResponse{
			Events: []json.RawMessage{},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method: "POST",
		URL:    "/webhook/ping",
		Body:   json.RawMessage(`{"type":"ping"}`),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	// An empty events array is valid — the Source Function filtered out the
	// request. After JSON round-trip with omitempty, the Events field may be
	// nil (empty array omitted) or an empty slice. Either way, length must be 0.
	require.Len(t, result.Events, 0)
}

// TestExecuteSourceFunction_LargePayload verifies that Source Functions
// handle large webhook payloads (100KB+) without issues. Large payloads
// are common for bulk webhook deliveries or data-rich event notifications.
func TestExecuteSourceFunction_LargePayload(t *testing.T) {
	srv := mockSourceServer(t, func(w http.ResponseWriter, req transformerRequest) {
		// Verify we received a large request payload.
		require.True(t, len(req.Request) > 100_000, "expected large request payload")
		writeSourceResponse(t, w, transformerResponse{
			Events: []json.RawMessage{makeTrackEvent("u1", "bulk_import")},
		})
	})
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	// Build a large JSON payload (>100KB) with realistic structure.
	items := make([]map[string]interface{}, 500)
	for i := range items {
		items[i] = map[string]interface{}{
			"id":          i,
			"name":        strings.Repeat("item-data-payload-", 10),
			"description": strings.Repeat("A", 200),
		}
	}
	largeBody, err := jsonrs.Marshal(map[string]interface{}{
		"type":  "bulk_import",
		"items": items,
	})
	require.NoError(t, err)
	require.True(t, len(largeBody) > 100_000, "test payload must be >100KB, got %d bytes", len(largeBody))

	request := &SourceFunctionRequest{
		Method:  "POST",
		URL:     "/webhook/bulk",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    json.RawMessage(largeBody),
	}

	result, err := engine.ExecuteSourceFunction(
		context.Background(), fn, request, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Events, 1)
}

// TestExecuteSourceFunction_ContextCancellation verifies that when the
// context is cancelled while the Transformer request is in flight, the
// operation returns a context.Canceled error. Uses context.WithCancel to
// create a cancellable context and cancels it before the mock responds.
func TestExecuteSourceFunction_ContextCancellation(t *testing.T) {
	// Use a WaitGroup and channel to synchronise between the test goroutine
	// and the mock server handler.
	var wg sync.WaitGroup
	serverReached := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Signal that the server received the request.
		close(serverReached)
		// Block until the test is done — the context cancellation should
		// abort the client before this goroutine writes a response.
		wg.Wait()
		// Write a response in case cancellation didn't abort quickly enough
		// (prevents a broken pipe log in the test).
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := testSourceFunctionDef()

	request := &SourceFunctionRequest{
		Method: "POST",
		URL:    "/webhook/slow",
		Body:   json.RawMessage(`{"data":"test"}`),
	}

	ctx, cancel := context.WithCancel(context.Background())

	wg.Add(1)
	var execErr error
	var execResult *SourceFunctionResult
	go func() {
		defer wg.Done()
		execResult, execErr = engine.ExecuteSourceFunction(ctx, fn, request, nil)
	}()

	// Wait for the mock server to receive the request, then cancel.
	select {
	case <-serverReached:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for server to receive request")
	}

	wg.Wait()
	require.Nil(t, execResult)
	require.Error(t, execErr)
	require.True(t,
		errors.Is(execErr, context.Canceled) ||
			strings.Contains(execErr.Error(), "context canceled") ||
			strings.Contains(execErr.Error(), "request canceled"),
		"expected context cancellation error, got: %s", execErr.Error(),
	)
}
