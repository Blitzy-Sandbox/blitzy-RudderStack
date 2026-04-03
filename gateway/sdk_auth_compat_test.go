package gateway

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/gateway/response"
	gwtypes "github.com/rudderlabs/rudder-server/gateway/types"
)

// TestWriteKeyBasicAuthCompatibility validates that all Segment SDK authentication patterns
// work correctly with the RudderStack Gateway's writeKeyAuth middleware.
//
// All Segment SDKs authenticate via HTTP Basic Auth using:
//
//	Authorization: Basic base64(writeKey:)
//
// where the username is the writeKey and the password is an empty string.
// This test suite exercises the exact auth patterns produced by each SDK platform
// (JavaScript, iOS, Android, Node.js, Python, Go, Java, Ruby), verifies edge-case
// behavior (missing colon, special characters, case sensitivity, empty credentials),
// and validates the beacon query-parameter auth flow.
func TestWriteKeyBasicAuthCompatibility(t *testing.T) {
	// delegate is the inner handler that runs only when authentication succeeds.
	delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})

	// newAuthCompatGateway creates a minimal *Handle with the supplied writeKey→source
	// mapping, a no-op logger, and an in-memory stats store — just enough to exercise
	// the writeKeyAuth and beaconInterceptor middleware without external dependencies.
	newAuthCompatGateway := func(t *testing.T, writeKeys map[string]backendconfig.SourceT) *Handle {
		t.Helper()
		statsStore, err := memstats.New()
		require.NoError(t, err)
		return &Handle{
			logger:             logger.NOP,
			stats:              statsStore,
			writeKeysSourceMap: writeKeys,
		}
	}

	// newBasicAuthRequest creates an HTTP POST request with the given Basic Auth
	// credentials and the mandatory CtxParamCallType context value that writeKeyAuth
	// reads via r.Context().Value(...).
	newBasicAuthRequest := func(writeKey, password string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/identify", nil)
		r.SetBasicAuth(writeKey, password)
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "identify"))
		return r
	}

	// newManualAuthRequest creates an HTTP request with a hand-crafted Authorization
	// header value, useful for testing edge cases such as base64-encoded writeKeys
	// that lack the trailing colon separator.
	newManualAuthRequest := func(authHeader string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/identify", nil)
		if authHeader != "" {
			r.Header.Set("Authorization", authHeader)
		}
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "identify"))
		return r
	}

	// noAuthRequest creates a request with NO Authorization header at all.
	noAuthRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/identify", nil)
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "identify"))
		return r
	}

	// enabledSource returns a SourceT that is enabled and has realistic metadata,
	// keyed by the given writeKey string.
	enabledSource := func(writeKey string) backendconfig.SourceT {
		return backendconfig.SourceT{
			ID:          "source-" + writeKey,
			WriteKey:    writeKey,
			Enabled:     true,
			Name:        "Test Source",
			WorkspaceID: "workspace-test",
			SourceDefinition: backendconfig.SourceDefinitionT{
				Name:     "javascript",
				Category: "cloud",
			},
		}
	}

	type authTestCase struct {
		name           string
		setupWriteKeys map[string]backendconfig.SourceT
		buildRequest   func() *http.Request
		expectedStatus int
		expectedBody   string
	}

	testCases := []authTestCase{
		// ---------------------------------------------------------------
		// Standard SDK Auth Patterns (cases 1–9)
		// All Segment SDKs use: Authorization: Basic base64(writeKey:)
		// Go's r.SetBasicAuth(writeKey, "") produces this exact header.
		// ---------------------------------------------------------------
		{
			name: "standard Basic Auth with empty password (Segment SDK pattern)",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"test-write-key-123": enabledSource("test-write-key-123"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("test-write-key-123", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Basic Auth from analytics.js",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"test-write-key-123": enabledSource("test-write-key-123"),
			},
			buildRequest: func() *http.Request {
				// analytics.js constructs the header exactly as:
				//   Authorization: Basic <base64(writeKey + ":")>
				encoded := base64.StdEncoding.EncodeToString([]byte("test-write-key-123:"))
				return newManualAuthRequest("Basic " + encoded)
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Basic Auth from analytics-ios",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"ios-write-key-456": enabledSource("ios-write-key-456"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("ios-write-key-456", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Basic Auth from analytics-android",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"android-write-key-789": enabledSource("android-write-key-789"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("android-write-key-789", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Basic Auth from analytics-node",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"node-write-key-abc": enabledSource("node-write-key-abc"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("node-write-key-abc", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Basic Auth from analytics-python",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"python-write-key-def": enabledSource("python-write-key-def"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("python-write-key-def", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Basic Auth from analytics-go",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"go-write-key-ghi": enabledSource("go-write-key-ghi"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("go-write-key-ghi", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Basic Auth from analytics-java",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"java-write-key-jkl": enabledSource("java-write-key-jkl"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("java-write-key-jkl", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Basic Auth from analytics-ruby",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"ruby-write-key-mno": enabledSource("ruby-write-key-mno"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("ruby-write-key-mno", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},

		// ---------------------------------------------------------------
		// Edge Cases (cases 10–17)
		// ---------------------------------------------------------------
		{
			name: "Basic Auth without trailing colon in base64",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"test-write-key-123": enabledSource("test-write-key-123"),
			},
			buildRequest: func() *http.Request {
				// Encode writeKey WITHOUT the trailing colon.
				// Go's r.BasicAuth() splits on the first ":" — when the decoded
				// string has no colon, BasicAuth() returns ok=false.
				// This documents the critical interop requirement: SDKs MUST
				// include the trailing colon before base64-encoding.
				encoded := base64.StdEncoding.EncodeToString([]byte("test-write-key-123"))
				return newManualAuthRequest("Basic " + encoded)
			},
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   response.NoWriteKeyInBasicAuth + "\n",
		},
		{
			name: "Write Key with special characters",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"wk+special/chars==": enabledSource("wk+special/chars=="),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("wk+special/chars==", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Write Key case sensitivity - exact match succeeds",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"CaseSensitiveKey": enabledSource("CaseSensitiveKey"),
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("CaseSensitiveKey", "")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
		{
			name: "Write Key case sensitivity - wrong case rejected",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"CaseSensitiveKey": enabledSource("CaseSensitiveKey"),
			},
			buildRequest: func() *http.Request {
				// Lowercase variant of the registered key must NOT match.
				return newBasicAuthRequest("casesensitivekey", "")
			},
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   response.InvalidWriteKey + "\n",
		},
		{
			name: "Empty Authorization header",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"test-write-key-123": enabledSource("test-write-key-123"),
			},
			buildRequest: func() *http.Request {
				// No Authorization header at all.
				return noAuthRequest()
			},
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   response.NoWriteKeyInBasicAuth + "\n",
		},
		{
			name:           "Invalid write key not registered",
			setupWriteKeys: map[string]backendconfig.SourceT{},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("nonexistent-write-key", "")
			},
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   response.InvalidWriteKey + "\n",
		},
		{
			name: "Disabled source write key",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"disabled-write-key": {
					ID:          "source-disabled",
					WriteKey:    "disabled-write-key",
					Enabled:     false,
					Name:        "Disabled Source",
					WorkspaceID: "workspace-disabled",
					SourceDefinition: backendconfig.SourceDefinitionT{
						Name:     "javascript",
						Category: "cloud",
					},
				},
			},
			buildRequest: func() *http.Request {
				return newBasicAuthRequest("disabled-write-key", "")
			},
			expectedStatus: http.StatusNotFound,
			expectedBody:   response.SourceDisabled + "\n",
		},
		{
			name:           "Empty write key string",
			setupWriteKeys: map[string]backendconfig.SourceT{},
			buildRequest: func() *http.Request {
				// SetBasicAuth("", "") encodes base64(":") = "Og==".
				// Go's BasicAuth() decodes ":" → username="", ok=true.
				// writeKeyAuth rejects because writeKey == "".
				return newBasicAuthRequest("", "")
			},
			expectedStatus: http.StatusUnauthorized,
			expectedBody:   response.NoWriteKeyInBasicAuth + "\n",
		},
		{
			name: "Basic Auth with non-empty password",
			setupWriteKeys: map[string]backendconfig.SourceT{
				"test-write-key-123": enabledSource("test-write-key-123"),
			},
			buildRequest: func() *http.Request {
				// Gateway only inspects the username (writeKey) from BasicAuth.
				// A non-empty password is ignored, matching Segment's behavior.
				return newBasicAuthRequest("test-write-key-123", "some-password")
			},
			expectedStatus: http.StatusOK,
			expectedBody:   "OK",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newAuthCompatGateway(t, tc.setupWriteKeys)
			handler := gw.writeKeyAuth(delegate)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, tc.buildRequest())

			require.Equal(t, tc.expectedStatus, w.Code)
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err)
			require.Equal(t, tc.expectedBody, string(body))
		})
	}

	// ---------------------------------------------------------------
	// Query Parameter Auth — Beacon pattern (case 18)
	// ---------------------------------------------------------------
	// The beaconInterceptor extracts the writeKey from the ?writeKey= query
	// parameter, converts it to a standard Basic Auth header, and delegates
	// to the inner handler chain (writeKeyAuth → delegate). This test
	// validates the full beacon auth flow end-to-end.
	t.Run("writeKey in query params (beacon pattern)", func(t *testing.T) {
		writeKey := "beacon-write-key"
		gw := newAuthCompatGateway(t, map[string]backendconfig.SourceT{
			writeKey: enabledSource(writeKey),
		})

		// Chain: beaconInterceptor → writeKeyAuth → delegate
		handler := gw.beaconInterceptor(gw.writeKeyAuth(delegate))

		params := url.Values{}
		params.Set("writeKey", writeKey)
		r := httptest.NewRequest(http.MethodPost, "/beacon/v1/batch?"+params.Encode(), nil)
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "batch"))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code)
		body, err := io.ReadAll(w.Body)
		require.NoError(t, err)
		require.Equal(t, "OK", string(body))
	})
}
