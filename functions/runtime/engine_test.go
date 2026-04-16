// Package runtime — engine_test.go
//
// Comprehensive unit tests for the Functions runtime engine (Sprint 4-6,
// Epics E-015, E-016, E-017). Tests cover five phases:
//
//  1. Engine Construction  — New() constructor, config fallbacks, child logger
//  2. Handler Dispatch     — Source, Destination, Insert routing via Execute()
//  3. Transformer HTTP     — POST format, timeout, unavailable service
//  4. Error Handling       — nil FunctionDef, empty event, context cancelled
//  5. Settings Propagation — merged settings forwarded to Transformer
//
// Tests use httptest.NewServer to mock the Transformer service, following the
// fakeTransformer pattern from
// processor/internal/transformer/user_transformer/user_transformer_test.go.
//
// Assertions use testify/require per repository convention for unit tests
// (NOT Ginkgo/Gomega — per AAP Rule 0.7.4).
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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"
)

// ===========================================================================
// Phase 1: Engine Construction Tests
// ===========================================================================

// TestNewEngine verifies that New() correctly initialises the Engine from
// configuration, sets up the child logger, HTTP client, and transformer URL
// with the expected fallback chain:
//
//	FUNCTIONS_TRANSFORM_URL → DEST_TRANSFORM_URL → http://localhost:9090
func TestNewEngine(t *testing.T) {
	t.Run("returns non-nil engine with defaults", func(t *testing.T) {
		conf := config.New()
		statsStore, err := memstats.New()
		require.NoError(t, err)

		engine := New(conf, logger.NOP, statsStore)
		require.NotNil(t, engine)
		require.NotNil(t, engine.httpClient)
		require.Equal(t, defaultTransformerURL, engine.transformerURL)
	})

	t.Run("uses FUNCTIONS_TRANSFORM_URL when set", func(t *testing.T) {
		conf := config.New()
		conf.Set("FUNCTIONS_TRANSFORM_URL", "http://custom-transformer:8888")
		statsStore, err := memstats.New()
		require.NoError(t, err)

		engine := New(conf, logger.NOP, statsStore)
		require.NotNil(t, engine)
		require.Equal(t, "http://custom-transformer:8888", engine.transformerURL)
	})

	t.Run("falls back to DEST_TRANSFORM_URL", func(t *testing.T) {
		conf := config.New()
		conf.Set("DEST_TRANSFORM_URL", "http://dest-transformer:7777")
		statsStore, err := memstats.New()
		require.NoError(t, err)

		engine := New(conf, logger.NOP, statsStore)
		require.NotNil(t, engine)
		require.Equal(t, "http://dest-transformer:7777", engine.transformerURL)
	})

	t.Run("FUNCTIONS_TRANSFORM_URL takes precedence over DEST_TRANSFORM_URL", func(t *testing.T) {
		conf := config.New()
		conf.Set("FUNCTIONS_TRANSFORM_URL", "http://functions:9999")
		conf.Set("DEST_TRANSFORM_URL", "http://dest:7777")
		statsStore, err := memstats.New()
		require.NoError(t, err)

		engine := New(conf, logger.NOP, statsStore)
		require.Equal(t, "http://functions:9999", engine.transformerURL)
	})

	t.Run("sets HTTP client timeout from config", func(t *testing.T) {
		conf := config.New()
		conf.Set("Functions.Runtime.timeout", 5)
		statsStore, err := memstats.New()
		require.NoError(t, err)

		engine := New(conf, logger.NOP, statsStore)
		require.NotNil(t, engine.httpClient)
		require.Equal(t, 5*time.Second, engine.httpClient.Timeout)
	})

	t.Run("defaults HTTP client timeout to 600 seconds", func(t *testing.T) {
		conf := config.New()
		statsStore, err := memstats.New()
		require.NoError(t, err)

		engine := New(conf, logger.NOP, statsStore)
		require.Equal(t, time.Duration(defaultTimeoutSeconds)*time.Second, engine.httpClient.Timeout)
	})

	t.Run("stores config and statsFactory references", func(t *testing.T) {
		conf := config.New()
		statsStore, err := memstats.New()
		require.NoError(t, err)

		engine := New(conf, logger.NOP, statsStore)
		require.NotNil(t, engine.conf)
		require.NotNil(t, engine.statsFactory)
		require.NotNil(t, engine.log)
	})
}

// ===========================================================================
// Phase 2: Handler Dispatch Tests
// ===========================================================================

// TestEngine_Execute_DispatchesSourceFunction verifies that Execute routes
// source function types to executeSourceFunction and hits the correct
// Transformer endpoint (/v1/functions/source).
func TestEngine_Execute_DispatchesSourceFunction(t *testing.T) {
	var receivedPath string
	var receivedReq transformerRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		err := jsonrs.NewDecoder(r.Body).Decode(&receivedReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := transformerResponse{
			Events: []json.RawMessage{
				json.RawMessage(`{"type":"track","event":"Webhook Received","userId":"user-1"}`),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:          "src-001",
		WorkspaceID: "ws-001",
		Name:        "Test Source Fn",
		Type:        FunctionTypeSource,
		Code:        "function onRequest(req, settings) { return []; }",
		Version:     1,
		Settings:    map[string]string{"key": "val"},
	}

	// Source functions pass the event as the "request" payload.
	requestPayload := json.RawMessage(`{"method":"POST","url":"/webhook","body":{"data":1}}`)

	result, err := engine.Execute(context.Background(), fn, requestPayload, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify the Transformer received the request at the source endpoint.
	require.Equal(t, "/v1/functions/source", receivedPath)

	// Verify the transformerRequest carries the correct handler and request
	// field (not the event field — source functions use the Request key).
	require.Equal(t, "onRequest", receivedReq.Handler)
	require.Equal(t, fn.ID, receivedReq.FunctionID)
	require.Equal(t, fn.WorkspaceID, receivedReq.WorkspaceID)
	require.Equal(t, fn.Code, receivedReq.Code)
	require.Equal(t, fn.Version, receivedReq.Version)
	require.NotNil(t, receivedReq.Request)
	require.Contains(t, string(receivedReq.Request), `"method":"POST"`)

	// Verify the result contains the events from the mock response.
	require.Len(t, result.Events, 1)
	require.Contains(t, string(result.Events[0]), `"Webhook Received"`)
}

// TestEngine_Execute_DispatchesDestinationFunction verifies that Execute routes
// destination function types to executeDestinationFunction with the correct
// handler derived from the event type.
func TestEngine_Execute_DispatchesDestinationFunction(t *testing.T) {
	var receivedPath string
	var receivedReq transformerRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		err := jsonrs.NewDecoder(r.Body).Decode(&receivedReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Return a destination-style response with statusCode and body.
		resp := transformerResponse{
			StatusCode: http.StatusOK,
			Body:       json.RawMessage(`{"delivered":true}`),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:          "dest-001",
		WorkspaceID: "ws-001",
		Name:        "Test Dest Fn",
		Type:        FunctionTypeDestination,
		Code:        "function onTrack(event, settings) { return { statusCode: 200 }; }",
		Version:     1,
	}

	trackEvent := json.RawMessage(`{"type":"track","event":"Order Completed","userId":"user-1","properties":{"revenue":50.00}}`)

	result, err := engine.Execute(context.Background(), fn, trackEvent, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify the Transformer received the request at the destination endpoint.
	require.Equal(t, "/v1/functions/destination", receivedPath)

	// Verify the handler was resolved from the event type.
	require.Equal(t, "onTrack", receivedReq.Handler)
	require.Equal(t, fn.ID, receivedReq.FunctionID)
	require.NotNil(t, receivedReq.Event)
	require.Contains(t, string(receivedReq.Event), `"Order Completed"`)
}

// TestEngine_Execute_DispatchesInsertFunction verifies that Execute routes
// insert function types to executeInsertFunction with the correct handler
// derived from the event type.
func TestEngine_Execute_DispatchesInsertFunction(t *testing.T) {
	var receivedPath string
	var receivedReq transformerRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		err := jsonrs.NewDecoder(r.Body).Decode(&receivedReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := transformerResponse{
			Event:   json.RawMessage(`{"type":"identify","userId":"user-1","traits":{"email":"test@example.com"}}`),
			Dropped: false,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:          "insert-001",
		WorkspaceID: "ws-001",
		Name:        "Test Insert Fn",
		Type:        FunctionTypeInsert,
		Code:        "function onIdentify(event, settings) { return event; }",
		Version:     1,
	}

	identifyEvent := json.RawMessage(`{"type":"identify","userId":"user-1","traits":{"name":"Alice"}}`)

	result, err := engine.Execute(context.Background(), fn, identifyEvent, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify the Transformer received the request at the insert endpoint.
	require.Equal(t, "/v1/functions/insert", receivedPath)

	// Verify the handler was resolved from the event type.
	require.Equal(t, "onIdentify", receivedReq.Handler)

	// Verify the result contains the single event from the mock response.
	require.Len(t, result.Events, 1)
	require.Contains(t, string(result.Events[0]), `"user-1"`)
}

// TestEngine_Execute_UnknownFunctionType verifies that Execute returns an error
// (not a panic) for unknown function types.
func TestEngine_Execute_UnknownFunctionType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// This handler should never be reached.
		t.Fatal("unexpected Transformer request for unknown function type")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:   "bad-001",
		Type: "nonexistent",
		Code: "function foo() {}",
	}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{}`), nil)
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "unsupported function type")
}

// ===========================================================================
// Phase 3: Transformer HTTP Communication Tests
// ===========================================================================

// TestEngine_TransformerHTTPPost verifies the HTTP POST format: Content-Type
// header, request body containing function code, event data, and settings,
// and correct response parsing.
func TestEngine_TransformerHTTPPost(t *testing.T) {
	var receivedContentType string
	var receivedMethod string
	var receivedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		receivedMethod = r.Method

		// Read and store actual body for assertion.
		buf := make([]byte, r.ContentLength+1024)
		n, _ := r.Body.Read(buf)
		receivedBody = buf[:n]

		resp := transformerResponse{
			Events: []json.RawMessage{
				json.RawMessage(`{"type":"track","event":"Page Viewed"}`),
			},
			Logs: []string{"function executed successfully"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:          "http-test-001",
		WorkspaceID: "ws-001",
		Name:        "HTTP Post Test Fn",
		Type:        FunctionTypeSource,
		Code:        "function onRequest(req) { return []; }",
		Version:     3,
		Settings:    map[string]string{"api_key": "secret123"},
	}

	event := json.RawMessage(`{"method":"GET","url":"/health"}`)

	result, err := engine.Execute(context.Background(), fn, event, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify HTTP method and Content-Type header.
	require.Equal(t, http.MethodPost, receivedMethod)
	require.Equal(t, "application/json", receivedContentType)

	// Verify the request body contains expected fields.
	require.True(t, len(receivedBody) > 0, "request body must not be empty")
	bodyStr := string(receivedBody)
	require.Contains(t, bodyStr, `"functionId"`)
	require.Contains(t, bodyStr, `"http-test-001"`)
	require.Contains(t, bodyStr, `"code"`)
	require.Contains(t, bodyStr, `"onRequest"`)
	require.Contains(t, bodyStr, `"api_key"`)
	require.Contains(t, bodyStr, `"secret123"`)

	// Verify the response was correctly parsed.
	require.Len(t, result.Events, 1)
	require.Contains(t, string(result.Events[0]), "Page Viewed")
	require.Equal(t, []string{"function executed successfully"}, result.Logs)
}

// TestEngine_TransformerTimeout verifies that when the Transformer service
// takes too long, the engine respects the context deadline and returns a
// context deadline exceeded error.
func TestEngine_TransformerTimeout(t *testing.T) {
	// handlerDone is used to signal when the handler has exited so that we
	// can close the server cleanly without a long wait.
	handlerDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(handlerDone)
		// Block until the server is closed. The client will time out well
		// before the server shuts down.
		time.Sleep(500 * time.Millisecond)
		resp := transformerResponse{
			Events: []json.RawMessage{json.RawMessage(`{"type":"track"}`)},
		}
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer func() {
		srv.Close()
		<-handlerDone
	}()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:   "timeout-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { return []; }",
	}

	// Use a very short timeout to trigger deadline exceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	result, err := engine.Execute(ctx, fn, json.RawMessage(`{"method":"GET"}`), nil)
	require.Error(t, err)
	require.Nil(t, result)
	// The error should be related to context deadline/cancellation.
	require.True(t,
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			containsAny(err.Error(), "deadline exceeded", "context canceled", "Client.Timeout"),
		"expected context or timeout error, got: %v", err,
	)
}

// TestEngine_TransformerUnavailable verifies that when the Transformer service
// is unreachable, the engine returns a meaningful connection error.
func TestEngine_TransformerUnavailable(t *testing.T) {
	// Create a server and immediately close it to get an invalid URL.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	engine := newTestEngine(t, closedURL)

	fn := &FunctionDef{
		ID:   "unavail-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { return []; }",
	}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{"method":"GET"}`), nil)
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "posting to transformer")
}

// TestEngine_TransformerNon200 verifies that non-200 HTTP responses from the
// Transformer are treated as errors with the status code in the message.
func TestEngine_TransformerNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:   "non200-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { return []; }",
	}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{"method":"GET"}`), nil)
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "500")
}

// ===========================================================================
// Phase 4: Error Handling Tests
// ===========================================================================

// TestEngine_Execute_NilFunctionDef verifies that passing a nil FunctionDef
// to Execute returns an error rather than panicking.
func TestEngine_Execute_NilFunctionDef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected Transformer request for nil function def")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	result, err := engine.Execute(context.Background(), nil, json.RawMessage(`{}`), nil)
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "function definition is required")
}

// TestEngine_Execute_EmptyEvent verifies that passing an empty/nil event JSON
// is handled gracefully. For source functions (which put the event into the
// Request field), an empty event should still succeed. For destination/insert
// functions, resolveHandler requires a type field, so an empty event returns
// an InvalidEventPayload error.
func TestEngine_Execute_EmptyEvent(t *testing.T) {
	t.Run("source function with nil event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := transformerResponse{Events: []json.RawMessage{}}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = jsonrs.NewEncoder(w).Encode(resp)
		}))
		defer srv.Close()

		engine := newTestEngine(t, srv.URL)
		fn := &FunctionDef{
			ID:   "empty-src-001",
			Type: FunctionTypeSource,
			Code: "function onRequest(req) { return []; }",
		}

		// Source functions accept nil events (the request may have no body).
		result, err := engine.Execute(context.Background(), fn, nil, nil)
		require.NoError(t, err)
		require.NotNil(t, result)
	})

	t.Run("destination function with empty event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("unexpected Transformer request for empty destination event")
		}))
		defer srv.Close()

		engine := newTestEngine(t, srv.URL)
		fn := &FunctionDef{
			ID:   "empty-dest-001",
			Type: FunctionTypeDestination,
			Code: "function onTrack(event) { return {}; }",
		}

		// Destination functions require an event with a "type" field.
		result, err := engine.Execute(context.Background(), fn, json.RawMessage(``), nil)
		require.Error(t, err)
		require.Nil(t, result)

		// Should be an InvalidEventPayload typed error from resolveHandler.
		var invalidEvt *InvalidEventPayload
		require.True(t, errors.As(err, &invalidEvt))
	})

	t.Run("insert function with nil event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("unexpected Transformer request for nil insert event")
		}))
		defer srv.Close()

		engine := newTestEngine(t, srv.URL)
		fn := &FunctionDef{
			ID:   "empty-insert-001",
			Type: FunctionTypeInsert,
			Code: "function onTrack(event) { return event; }",
		}

		result, err := engine.Execute(context.Background(), fn, nil, nil)
		require.Error(t, err)
		require.Nil(t, result)

		var invalidEvt *InvalidEventPayload
		require.True(t, errors.As(err, &invalidEvt))
	})
}

// TestEngine_Execute_ContextCancelled verifies that a cancelled context
// results in a prompt cancellation error without completing the HTTP request.
func TestEngine_Execute_ContextCancelled(t *testing.T) {
	requestReceived := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestReceived <- struct{}{}
		// Block long enough for context to cancel first.
		time.Sleep(5 * time.Second)
		resp := transformerResponse{Events: []json.RawMessage{json.RawMessage(`{}`)}}
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:   "cancel-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { return []; }",
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately to ensure the request is aborted.
	cancel()

	result, err := engine.Execute(ctx, fn, json.RawMessage(`{"method":"GET"}`), nil)
	require.Error(t, err)
	require.Nil(t, result)
	require.True(t,
		errors.Is(err, context.Canceled) ||
			containsAny(err.Error(), "context canceled", "operation was canceled"),
		"expected context cancelled error, got: %v", err,
	)
}

// TestEngine_Execute_TransformerReturnsTypedError verifies that typed errors
// from the Transformer response are correctly mapped to Go typed errors.
func TestEngine_Execute_TransformerReturnsTypedError(t *testing.T) {
	testCases := []struct {
		name      string
		errorType string
		checkFn   func(t *testing.T, err error)
	}{
		{
			name:      "EventNotSupported error",
			errorType: ErrorTypeEventNotSupported,
			checkFn: func(t *testing.T, err error) {
				t.Helper()
				var typedErr *EventNotSupported
				require.True(t, errors.As(err, &typedErr))
			},
		},
		{
			name:      "InvalidEventPayload error",
			errorType: ErrorTypeInvalidEventPayload,
			checkFn: func(t *testing.T, err error) {
				t.Helper()
				var typedErr *InvalidEventPayload
				require.True(t, errors.As(err, &typedErr))
			},
		},
		{
			name:      "ValidationError",
			errorType: ErrorTypeValidationError,
			checkFn: func(t *testing.T, err error) {
				t.Helper()
				var typedErr *ValidationError
				require.True(t, errors.As(err, &typedErr))
			},
		},
		{
			name:      "RetryError",
			errorType: ErrorTypeRetryError,
			checkFn: func(t *testing.T, err error) {
				t.Helper()
				var typedErr *RetryError
				require.True(t, errors.As(err, &typedErr))
				require.True(t, typedErr.IsRetryable())
			},
		},
		{
			name:      "DropEvent error",
			errorType: ErrorTypeDropEvent,
			checkFn: func(t *testing.T, err error) {
				t.Helper()
				var typedErr *DropEvent
				require.True(t, errors.As(err, &typedErr))
				require.True(t, IsDropError(err))
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				resp := transformerResponse{
					Error: &FunctionError{
						Type:    tc.errorType,
						Message: "test error for " + tc.errorType,
					},
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = jsonrs.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			engine := newTestEngine(t, srv.URL)
			fn := &FunctionDef{
				ID:   "typed-err-001",
				Type: FunctionTypeSource,
				Code: "function onRequest(req) { throw new Error('test'); }",
			}

			result, err := engine.Execute(
				context.Background(), fn,
				json.RawMessage(`{"method":"POST"}`), nil,
			)
			require.Error(t, err)
			require.Nil(t, result)
			tc.checkFn(t, err)
		})
	}
}

// TestEngine_Execute_DestinationUnsupportedEventType verifies that an event
// with an unknown type results in an EventNotSupported typed error.
func TestEngine_Execute_DestinationUnsupportedEventType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected Transformer request for unsupported event type")
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := &FunctionDef{
		ID:   "unsupported-type-001",
		Type: FunctionTypeDestination,
		Code: "function onCustom(event) { return {}; }",
	}

	event := json.RawMessage(`{"type":"custom_event","userId":"u1"}`)
	result, err := engine.Execute(context.Background(), fn, event, nil)
	require.Error(t, err)
	require.Nil(t, result)

	var notSupported *EventNotSupported
	require.True(t, errors.As(err, &notSupported))
	require.Contains(t, notSupported.Error(), "custom_event")
}

// ===========================================================================
// Phase 5: Settings Propagation Tests
// ===========================================================================

// TestEngine_Execute_SettingsPassedToTransformer verifies that the merged
// settings map (FunctionDef.Settings + call-level overrides) is sent to the
// Transformer in the request body.
func TestEngine_Execute_SettingsPassedToTransformer(t *testing.T) {
	var receivedSettings map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req transformerRequest
		err := jsonrs.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		receivedSettings = req.Settings

		resp := transformerResponse{
			Events: []json.RawMessage{json.RawMessage(`{"type":"track","event":"Test"}`)},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)

	fn := &FunctionDef{
		ID:   "settings-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req, settings) { return []; }",
		Settings: map[string]string{
			"api_key":  "secret123",
			"endpoint": "https://api.example.com",
			"shared":   "base_value",
		},
	}

	callSettings := map[string]string{
		"request_id": "req-abc",
		"shared":     "override_value", // Should override the base setting.
	}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{"method":"POST"}`), callSettings)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify all settings were merged and sent.
	require.Equal(t, "secret123", receivedSettings["api_key"])
	require.Equal(t, "https://api.example.com", receivedSettings["endpoint"])
	require.Equal(t, "req-abc", receivedSettings["request_id"])
	// Call-level settings take precedence over FunctionDef.Settings.
	require.Equal(t, "override_value", receivedSettings["shared"])
}

// TestEngine_Execute_NilSettings verifies that nil settings maps (both at
// FunctionDef level and call level) are handled gracefully.
func TestEngine_Execute_NilSettings(t *testing.T) {
	var receivedSettings map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req transformerRequest
		err := jsonrs.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		receivedSettings = req.Settings

		resp := transformerResponse{
			Events: []json.RawMessage{json.RawMessage(`{"type":"track"}`)},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := &FunctionDef{
		ID:   "nil-settings-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { return []; }",
		// Settings is nil — no base settings.
	}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{"method":"GET"}`), nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	// With both nil, mergeSettings returns nil.
	require.Nil(t, receivedSettings)
}

// TestEngine_Execute_OnlyCallLevelSettings verifies that when FunctionDef has
// no settings but the caller provides settings, they are sent correctly.
func TestEngine_Execute_OnlyCallLevelSettings(t *testing.T) {
	var receivedSettings map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req transformerRequest
		err := jsonrs.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		receivedSettings = req.Settings

		resp := transformerResponse{
			Events: []json.RawMessage{json.RawMessage(`{"type":"track"}`)},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := &FunctionDef{
		ID:   "call-only-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { return []; }",
	}

	callSettings := map[string]string{"runtime_key": "runtime_val"}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{"method":"GET"}`), callSettings)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, "runtime_val", receivedSettings["runtime_key"])
}

// ===========================================================================
// Additional Edge Case Tests
// ===========================================================================

// TestEngine_Execute_AllDestinationHandlers verifies that all 8 typed handlers
// are correctly resolved by Execute for destination function types.
func TestEngine_Execute_AllDestinationHandlers(t *testing.T) {
	handlerCases := []struct {
		eventType       string
		expectedHandler string
	}{
		{"track", "onTrack"},
		{"identify", "onIdentify"},
		{"group", "onGroup"},
		{"page", "onPage"},
		{"screen", "onScreen"},
		{"alias", "onAlias"},
		{"delete", "onDelete"},
		{"batch", "onBatch"},
	}

	for _, tc := range handlerCases {
		t.Run(tc.eventType, func(t *testing.T) {
			var receivedHandler string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req transformerRequest
				if err := jsonrs.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				receivedHandler = req.Handler

				resp := transformerResponse{
					StatusCode: http.StatusOK,
					Body:       json.RawMessage(`{"ok":true}`),
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = jsonrs.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			engine := newTestEngine(t, srv.URL)
			fn := &FunctionDef{
				ID:   "handler-" + tc.eventType,
				Type: FunctionTypeDestination,
				Code: "function " + tc.expectedHandler + "(event) { return {}; }",
			}

			event := json.RawMessage(`{"type":"` + tc.eventType + `","userId":"u1"}`)
			result, err := engine.Execute(context.Background(), fn, event, nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tc.expectedHandler, receivedHandler)
		})
	}
}

// TestEngine_Execute_InsertFunctionDrop verifies that when the Transformer
// returns dropped=true for an insert function, the result has nil Events.
func TestEngine_Execute_InsertFunctionDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := transformerResponse{
			Dropped: true,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := &FunctionDef{
		ID:   "drop-001",
		Type: FunctionTypeInsert,
		Code: "function onTrack(event) { throw new DropEvent('filtered'); }",
	}

	event := json.RawMessage(`{"type":"track","event":"Filtered Event","userId":"u1"}`)
	result, err := engine.Execute(context.Background(), fn, event, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.Events, "dropped insert function should have nil Events")
}

// TestEngine_Execute_ResponseWithLogs verifies that console logs from the
// function runtime are captured in the ExecutionResult.
func TestEngine_Execute_ResponseWithLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := transformerResponse{
			Events: []json.RawMessage{json.RawMessage(`{"type":"track"}`)},
			Logs:   []string{"log line 1", "log line 2", "debug: processing webhook"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := &FunctionDef{
		ID:   "logs-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { console.log('test'); return []; }",
	}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{"method":"GET"}`), nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Logs, 3)
	require.Equal(t, "log line 1", result.Logs[0])
	require.Equal(t, "log line 2", result.Logs[1])
	require.Contains(t, result.Logs[2], "debug")
}

// TestEngine_Execute_SourceReturnsMultipleEvents verifies that source functions
// returning multiple events are correctly parsed.
func TestEngine_Execute_SourceReturnsMultipleEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := transformerResponse{
			Events: []json.RawMessage{
				json.RawMessage(`{"type":"track","event":"Event 1","userId":"u1"}`),
				json.RawMessage(`{"type":"identify","userId":"u1","traits":{"email":"a@b.com"}}`),
				json.RawMessage(`{"type":"page","userId":"u1","name":"Home"}`),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := &FunctionDef{
		ID:   "multi-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { return [{}, {}, {}]; }",
	}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{"method":"POST"}`), nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Events, 3)
	require.Contains(t, string(result.Events[0]), "Event 1")
	require.Contains(t, string(result.Events[1]), "identify")
	require.Contains(t, string(result.Events[2]), "Home")
}

// TestEngine_Execute_ResponseWithPerEventErrors verifies that per-event errors
// from batch processing are captured in ExecutionResult.Errors.
func TestEngine_Execute_ResponseWithPerEventErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := transformerResponse{
			Events: []json.RawMessage{json.RawMessage(`{"type":"track"}`)},
			Errors: []FunctionError{
				{Type: ErrorTypeValidationError, Message: "missing required field: userId"},
				{Type: ErrorTypeEventNotSupported, Message: "custom type not supported"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonrs.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine := newTestEngine(t, srv.URL)
	fn := &FunctionDef{
		ID:   "per-event-err-001",
		Type: FunctionTypeSource,
		Code: "function onRequest(req) { return []; }",
	}

	result, err := engine.Execute(context.Background(), fn, json.RawMessage(`{"method":"POST"}`), nil)
	require.NoError(t, err) // Top-level error is nil when per-event errors exist.
	require.NotNil(t, result)
	require.Len(t, result.Errors, 2)
	require.Equal(t, ErrorTypeValidationError, result.Errors[0].Type)
	require.Equal(t, ErrorTypeEventNotSupported, result.Errors[1].Type)
}

// ===========================================================================
// Test utility helpers
// ===========================================================================

// containsAny returns true if s contains any of the provided substrings.
func containsAny(s string, substrings ...string) bool {
	for _, sub := range substrings {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
