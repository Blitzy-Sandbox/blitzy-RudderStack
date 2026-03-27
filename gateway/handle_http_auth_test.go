package gateway

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	gwtypes "github.com/rudderlabs/rudder-server/gateway/types"

	"github.com/rudderlabs/rudder-go-kit/config"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
)

func TestAuth(t *testing.T) {
	delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})
	statsStore, err := memstats.New()
	require.NoError(t, err)

	newGateway := func(writeKeysSourceMap, sourceIDSourceMap map[string]backendconfig.SourceT) *Handle {
		return &Handle{
			logger:             logger.NOP,
			stats:              statsStore,
			writeKeysSourceMap: writeKeysSourceMap,
			sourceIDSourceMap:  sourceIDSourceMap,
		}
	}

	newWriteKeyRequest := func(writeKey string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if writeKey != "" {
			r.SetBasicAuth(writeKey, "")
		}
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "dummy"))
		return r
	}

	newSourceIDRequest := func(sourceID string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if sourceID != "" {
			r.Header.Add("X-Rudder-Source-Id", sourceID)
		}
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "dummy"))
		return r
	}

	newRequestWithSourceIDAndDestID := func(sourceID, destinationID, reqType string, reqCtx *gwtypes.AuthRequestContext) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if len(sourceID) != 0 {
			r.Header.Add("X-Rudder-Source-Id", sourceID)
		}
		if len(destinationID) != 0 {
			r.Header.Add("X-Rudder-Destination-Id", destinationID)
		}
		if len(reqType) > 0 {
			r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, reqType))
		}
		if reqCtx != nil {
			r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamAuthRequestContext, reqCtx))
		}
		return r
	}

	t.Run("writeKeyAuth", func(t *testing.T) {
		t.Run("successful auth", func(t *testing.T) {
			writeKey := "123"
			gw := newGateway(map[string]backendconfig.SourceT{
				writeKey: {
					Enabled: true,
				},
			}, nil)
			r := newWriteKeyRequest(writeKey)
			w := httptest.NewRecorder()
			gw.writeKeyAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusOK, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "OK", string(body))
		})

		t.Run("no writeKey", func(t *testing.T) {
			gw := newGateway(nil, nil)
			r := newWriteKeyRequest("")
			w := httptest.NewRecorder()
			gw.writeKeyAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "failed to read writekey from header\n", string(body))
			require.Equal(t,
				float64(1),
				statsStore.Get(
					"gateway.write_key_requests",
					map[string]string{
						"source":        "noWriteKey",
						"sourceID":      "noWriteKey",
						"workspaceId":   "",
						"writeKey":      "noWriteKey",
						"reqType":       "dummy",
						"sourceType":    "",
						"sdkVersion":    "",
						"sourceDefName": "",
					},
				).LastValue(),
			)
		})

		t.Run("invalid writeKey", func(t *testing.T) {
			gw := newGateway(nil, nil)
			r := newWriteKeyRequest("random")
			w := httptest.NewRecorder()
			gw.writeKeyAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "invalid write key\n", string(body))
		})

		t.Run("disabled source", func(t *testing.T) {
			writeKey := "123"
			gw := newGateway(map[string]backendconfig.SourceT{
				writeKey: {
					Enabled:     false,
					ID:          "456",
					Name:        "789",
					WorkspaceID: "wrskpc",
					SourceDefinition: backendconfig.SourceDefinitionT{
						Category: "catA",
						Name:     "sourceA",
					},
					WriteKey: writeKey,
				},
			}, nil)
			r := newWriteKeyRequest(writeKey)
			w := httptest.NewRecorder()
			gw.writeKeyAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusNotFound, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "source is disabled\n", string(body))
			require.Equal(t,
				float64(1),
				statsStore.Get(
					"gateway.write_key_requests",
					map[string]string{
						"reqType":       "dummy",
						"sdkVersion":    "",
						"source":        "789_123",
						"sourceDefName": "sourcea",
						"sourceID":      "456",
						"sourceType":    "catA",
						"workspaceId":   "wrskpc",
						"writeKey":      writeKey,
					},
				).LastValue(),
			)
			require.Equal(t,
				float64(1),
				statsStore.Get(
					"gateway.write_key_failed_requests",
					map[string]string{
						"reqType":       "dummy",
						"sdkVersion":    "",
						"source":        "789_123",
						"sourceID":      "456",
						"sourceType":    "catA",
						"workspaceId":   "wrskpc",
						"writeKey":      writeKey,
						"reason":        "source is disabled",
						"sourceDefName": "sourcea",
					},
				).LastValue(),
			)
		})
	})

	t.Run("webhookAuth", func(t *testing.T) {
		t.Run("successful auth with authorization header", func(t *testing.T) {
			writeKey := "123"
			gw := newGateway(map[string]backendconfig.SourceT{
				writeKey: {
					Enabled: true,
					SourceDefinition: backendconfig.SourceDefinitionT{
						Category: "webhook",
					},
				},
			}, nil)
			r := newWriteKeyRequest(writeKey)
			w := httptest.NewRecorder()
			gw.webhookAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusOK, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "OK", string(body))
		})

		t.Run("successful auth with query param", func(t *testing.T) {
			writeKey := "123"
			gw := newGateway(map[string]backendconfig.SourceT{
				writeKey: {
					Enabled: true,
					SourceDefinition: backendconfig.SourceDefinitionT{
						Category: "webhook",
					},
				},
			}, nil)
			r := newWriteKeyRequest("")

			params := url.Values{}
			params.Add("writeKey", writeKey)
			r.URL.RawQuery = params.Encode()
			w := httptest.NewRecorder()
			gw.webhookAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusOK, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "OK", string(body))
		})

		t.Run("no writeKey", func(t *testing.T) {
			gw := newGateway(nil, nil)
			r := newWriteKeyRequest("")
			w := httptest.NewRecorder()
			gw.webhookAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "failed to read writekey from query params\n", string(body))
		})

		t.Run("invalid writeKey", func(t *testing.T) {
			gw := newGateway(nil, nil)
			r := newWriteKeyRequest("random")
			w := httptest.NewRecorder()
			gw.webhookAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "invalid write key\n", string(body))
		})

		t.Run("not a webhook source", func(t *testing.T) {
			writeKey := "123"
			gw := newGateway(map[string]backendconfig.SourceT{
				writeKey: {
					Enabled: true,
					SourceDefinition: backendconfig.SourceDefinitionT{
						Category: "other",
					},
				},
			}, nil)
			r := newWriteKeyRequest(writeKey)
			w := httptest.NewRecorder()
			gw.webhookAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "invalid write key\n", string(body))
		})

		t.Run("disabled webhook source", func(t *testing.T) {
			writeKey := "123"
			gw := newGateway(map[string]backendconfig.SourceT{
				writeKey: {
					Enabled: false,
					SourceDefinition: backendconfig.SourceDefinitionT{
						Category: "webhook",
					},
				},
			}, nil)
			r := newWriteKeyRequest(writeKey)
			w := httptest.NewRecorder()
			gw.webhookAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusNotFound, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "source is disabled\n", string(body))
		})
	})

	t.Run("sourceIDAuth", func(t *testing.T) {
		t.Run("successful auth", func(t *testing.T) {
			sourceID := "123"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: true,
				},
			})
			r := newSourceIDRequest(sourceID)
			w := httptest.NewRecorder()
			gw.sourceIDAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusOK, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "OK", string(body))
		})

		t.Run("no sourceID", func(t *testing.T) {
			gw := newGateway(nil, nil)
			r := newSourceIDRequest("")
			w := httptest.NewRecorder()
			gw.sourceIDAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "failed to read source id from header\n", string(body))
		})

		t.Run("invalid writeKey", func(t *testing.T) {
			gw := newGateway(nil, nil)
			r := newSourceIDRequest("random")
			w := httptest.NewRecorder()
			gw.sourceIDAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "invalid source id\n", string(body))
		})

		t.Run("disabled source", func(t *testing.T) {
			sourceID := "123"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: false,
				},
			})
			r := newSourceIDRequest(sourceID)
			w := httptest.NewRecorder()
			gw.sourceIDAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusNotFound, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "source is disabled\n", string(body))
		})
	})

	t.Run("replaySourceIDAuth", func(t *testing.T) {
		t.Run("replay source", func(t *testing.T) {
			sourceID := "123"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					ID:         sourceID,
					Enabled:    true,
					OriginalID: sourceID,
				},
			})
			r := newSourceIDRequest(sourceID)
			w := httptest.NewRecorder()
			gw.replaySourceIDAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusOK, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "OK", string(body))
		})

		t.Run("invalid source using replay endpoint", func(t *testing.T) {
			sourceID := "123"
			invalidSource := "345"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					ID:         sourceID,
					Enabled:    true,
					OriginalID: "",
				},
			})
			r := newSourceIDRequest(invalidSource)
			w := httptest.NewRecorder()
			gw.replaySourceIDAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "invalid source id\n", string(body))
		})

		t.Run("regular source using replay endpoint", func(t *testing.T) {
			sourceID := "123"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					ID:         sourceID,
					Enabled:    true,
					OriginalID: "",
				},
			})
			r := newSourceIDRequest(sourceID)
			w := httptest.NewRecorder()
			gw.replaySourceIDAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "invalid replay source\n", string(body))
		})
	})

	t.Run("authDestIDForSource", func(t *testing.T) {
		t.Run("successful auth with destination header", func(t *testing.T) {
			sourceID := "123"
			destinationID := "456"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: true,
					Destinations: []backendconfig.DestinationT{{
						ID: destinationID,
					}},
				},
			})
			r := newRequestWithSourceIDAndDestID(sourceID, destinationID, "dummy", &gwtypes.AuthRequestContext{
				Source: backendconfig.SourceT{
					Destinations: []backendconfig.DestinationT{{ID: destinationID, Enabled: true}},
				},
			})
			w := httptest.NewRecorder()
			gw.authDestIDForSource(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusOK, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "OK", string(body))
		})

		t.Run("auth req should be present in context", func(t *testing.T) {
			sourceID := "123"
			destinationID := "456"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: true,
					Destinations: []backendconfig.DestinationT{{
						ID: destinationID,
					}},
				},
			})
			r := newRequestWithSourceIDAndDestID(sourceID, destinationID, "dummy", nil)
			w := httptest.NewRecorder()
			gw.authDestIDForSource(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusInternalServerError, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "unable to get AuthRequest from context\n", string(body))
		})

		t.Run("req type should be present in context", func(t *testing.T) {
			sourceID := "123"
			destinationID := "456"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: true,
					Destinations: []backendconfig.DestinationT{{
						ID: destinationID,
					}},
				},
			})
			r := newRequestWithSourceIDAndDestID(sourceID, destinationID, "", &gwtypes.AuthRequestContext{
				Source: backendconfig.SourceT{
					Destinations: []backendconfig.DestinationT{{ID: destinationID, Enabled: true}},
				},
			})
			w := httptest.NewRecorder()
			gw.authDestIDForSource(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusInternalServerError, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "unable to get request type from context\n", string(body))
		})

		t.Run("successful auth without destination id in header", func(t *testing.T) {
			sourceID := "123"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: true,
				},
			})
			gw.config = config.Default
			r := newRequestWithSourceIDAndDestID(sourceID, "", "dummy", &gwtypes.AuthRequestContext{})
			w := httptest.NewRecorder()
			gw.authDestIDForSource(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusOK, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "OK", string(body))
		})

		t.Run("failed auth without destination id in header", func(t *testing.T) {
			sourceID := "123"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: true,
				},
			})
			gw.config = config.Default
			gw.config.Set("Gateway.requireDestinationIdHeader", true)
			r := newRequestWithSourceIDAndDestID(sourceID, "", "dummy", &gwtypes.AuthRequestContext{})
			w := httptest.NewRecorder()
			gw.authDestIDForSource(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusBadRequest, w.Code, "authentication should succeed")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "failed to read destination id from header\n", string(body))
		})

		t.Run("invalid destination id", func(t *testing.T) {
			sourceID := "123"
			destinationID := "456"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: true,
					Destinations: []backendconfig.DestinationT{{
						ID: destinationID,
					}},
				},
			})
			r := newRequestWithSourceIDAndDestID(sourceID, destinationID, "dummy", &gwtypes.AuthRequestContext{
				Source: backendconfig.SourceT{
					Destinations: []backendconfig.DestinationT{{ID: "invalid-dest-id"}},
				},
			})
			w := httptest.NewRecorder()
			gw.authDestIDForSource(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusBadRequest, w.Code, "authentication should fail")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "invalid destination id\n", string(body))
		})

		t.Run("destination disabled", func(t *testing.T) {
			sourceID := "123"
			destinationID := "456"
			gw := newGateway(nil, map[string]backendconfig.SourceT{
				sourceID: {
					Enabled: true,
					Destinations: []backendconfig.DestinationT{{
						ID: destinationID,
					}},
				},
			})
			r := newRequestWithSourceIDAndDestID(sourceID, destinationID, "dummy", &gwtypes.AuthRequestContext{
				Source: backendconfig.SourceT{
					Destinations: []backendconfig.DestinationT{{ID: destinationID, Enabled: false}},
				},
			})
			w := httptest.NewRecorder()
			gw.authDestIDForSource(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusNotFound, w.Code, "authentication should fail")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err, "reading response body should succeed")
			require.Equal(t, "destination is disabled\n", string(body))
		})
	})
}

// TestSegmentSDKAuthCompatibility validates that the Gateway's Write Key Basic Auth
// implementation is fully compatible with all Segment SDK authentication patterns.
// Each Segment SDK (JavaScript, iOS, Android, Node.js, Python, Go, Java, Ruby) sends
// Authorization: Basic base64(writeKey:) where the password is an empty string after
// the colon separator. This test suite validates standard auth, edge cases, error
// responses, and the beacon query-param-to-Basic-Auth conversion pathway.
func TestSegmentSDKAuthCompatibility(t *testing.T) {
	// Standard delegate that responds OK on successful authentication.
	delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})
	statsStore, err := memstats.New()
	require.NoError(t, err)

	// newGateway creates a Handle with the given writeKey→source map,
	// mirroring the helper pattern established in TestAuth.
	newGateway := func(writeKeysSourceMap map[string]backendconfig.SourceT) *Handle {
		return &Handle{
			logger:             logger.NOP,
			stats:              statsStore,
			writeKeysSourceMap: writeKeysSourceMap,
		}
	}

	t.Run("Segment SDK Basic Auth with empty password", func(t *testing.T) {
		// All Segment SDKs send Authorization: Basic base64(writeKey:) where
		// the password field is an empty string. This is the canonical auth
		// pattern used by analytics.js, analytics-ios, analytics-android,
		// analytics-node, analytics-python, analytics-go, analytics-java, and
		// analytics-ruby. r.SetBasicAuth(writeKey, "") produces exactly this
		// header format.
		writeKey := "2RVlfpFJ2sPJG4MjxMvqsQ9FjhS"
		gw := newGateway(map[string]backendconfig.SourceT{
			writeKey: {Enabled: true},
		})
		r := httptest.NewRequest("POST", "/v1/track", nil)
		r.SetBasicAuth(writeKey, "")
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "track"))
		w := httptest.NewRecorder()
		gw.writeKeyAuth(delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "Segment SDK standard Basic Auth with empty password should succeed")
		body, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, "OK", string(body))
	})

	t.Run("Basic Auth without trailing colon", func(t *testing.T) {
		// Edge case: some HTTP clients may send Authorization: Basic base64(writeKey)
		// without the trailing colon that separates username from password.
		// Go's net/http.Request.BasicAuth() requires the "user:password" format
		// and returns ok=false when no colon is present, causing the Gateway to
		// reject the request with NoWriteKeyInBasicAuth. This behavior is correct
		// because the HTTP Basic Auth spec (RFC 7617) mandates the colon separator.
		writeKey := "2RVlfpFJ2sPJG4MjxMvqsQ9FjhS"
		gw := newGateway(map[string]backendconfig.SourceT{
			writeKey: {Enabled: true},
		})
		r := httptest.NewRequest("POST", "/v1/track", nil)
		// Manually construct the Authorization header WITHOUT the colon separator.
		// This produces base64("writeKey") instead of base64("writeKey:").
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(writeKey)))
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "track"))
		w := httptest.NewRecorder()
		gw.writeKeyAuth(delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusUnauthorized, w.Code,
			"Basic Auth without colon separator should be rejected per RFC 7617")
		body, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, "failed to read writekey from header\n", string(body),
			"response should indicate missing writeKey in Basic Auth header")
	})

	t.Run("Write Key with special characters", func(t *testing.T) {
		// Verify that writeKeys containing URL-sensitive and base64-special
		// characters (+, /, =) are correctly handled through the Basic Auth
		// base64 encoding/decoding cycle. Segment write keys can contain
		// alphanumeric characters and some special characters; the auth
		// middleware must preserve them exactly.
		specialKeys := []struct {
			name     string
			writeKey string
		}{
			{name: "plus sign", writeKey: "write+Key+123"},
			{name: "forward slash", writeKey: "write/Key/456"},
			{name: "equals sign", writeKey: "writeKey==789"},
			{name: "mixed special chars", writeKey: "a+b/c=d"},
		}
		for _, tc := range specialKeys {
			t.Run(tc.name, func(t *testing.T) {
				gw := newGateway(map[string]backendconfig.SourceT{
					tc.writeKey: {Enabled: true},
				})
				r := httptest.NewRequest("POST", "/v1/identify", nil)
				r.SetBasicAuth(tc.writeKey, "")
				r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "identify"))
				w := httptest.NewRecorder()
				gw.writeKeyAuth(delegate).ServeHTTP(w, r)

				require.Equal(t, http.StatusOK, w.Code,
					"writeKey with special character %q should authenticate successfully", tc.name)
				body, err := io.ReadAll(w.Body)
				require.NoError(t, err, "reading response body should succeed")
				require.Equal(t, "OK", string(body))
			})
		}
	})

	t.Run("Write Key case sensitivity", func(t *testing.T) {
		// Verify that write key lookup is case-sensitive. The writeKeysSourceMap
		// uses exact string matching, so "AbCdEf" and "abcdef" must be treated
		// as different keys. This is important because Segment write keys are
		// case-sensitive identifiers and must not be normalized.
		registeredKey := "AbCdEf123XyZ"
		gw := newGateway(map[string]backendconfig.SourceT{
			registeredKey: {Enabled: true},
		})

		// The registered key (exact case) should authenticate successfully.
		t.Run("exact case matches", func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/track", nil)
			r.SetBasicAuth(registeredKey, "")
			r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "track"))
			w := httptest.NewRecorder()
			gw.writeKeyAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusOK, w.Code,
				"exact case writeKey should authenticate successfully")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err)
			require.Equal(t, "OK", string(body))
		})

		// A lowercased version of the same key should fail — keys are case-sensitive.
		t.Run("lowercase variant rejected", func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/track", nil)
			r.SetBasicAuth("abcdef123xyz", "")
			r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "track"))
			w := httptest.NewRecorder()
			gw.writeKeyAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code,
				"lowercased writeKey should fail authentication (case-sensitive)")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err)
			require.Equal(t, "invalid write key\n", string(body))
		})

		// An uppercased version of the same key should also fail.
		t.Run("uppercase variant rejected", func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/track", nil)
			r.SetBasicAuth("ABCDEF123XYZ", "")
			r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "track"))
			w := httptest.NewRecorder()
			gw.writeKeyAuth(delegate).ServeHTTP(w, r)

			require.Equal(t, http.StatusUnauthorized, w.Code,
				"uppercased writeKey should fail authentication (case-sensitive)")
			body, err := io.ReadAll(w.Body)
			require.NoError(t, err)
			require.Equal(t, "invalid write key\n", string(body))
		})
	})

	t.Run("Rejection of invalid writeKey returns 401", func(t *testing.T) {
		// Verify the exact HTTP 401 Unauthorized response when a writeKey is
		// provided that does not exist in the source map. Segment SDKs expect
		// a 401 response for invalid credentials, not 403 or other status codes.
		gw := newGateway(map[string]backendconfig.SourceT{
			"validWriteKey123": {Enabled: true},
		})
		r := httptest.NewRequest("POST", "/v1/track", nil)
		r.SetBasicAuth("nonExistentWriteKey999", "")
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "track"))
		w := httptest.NewRecorder()
		gw.writeKeyAuth(delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusUnauthorized, w.Code,
			"invalid writeKey should return HTTP 401 Unauthorized")
		body, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, "invalid write key\n", string(body),
			"response body should match response.InvalidWriteKey")
	})

	t.Run("Rejection of disabled source returns 404", func(t *testing.T) {
		// Verify the exact HTTP 404 Not Found response when a valid writeKey
		// maps to a disabled source. This matches the Segment API behavior
		// where a disabled source returns 404, distinguishing it from
		// authentication failures (401).
		writeKey := "disabledSourceKey456"
		gw := newGateway(map[string]backendconfig.SourceT{
			writeKey: {
				Enabled:     false,
				ID:          "source-id-001",
				Name:        "disabled-test-source",
				WorkspaceID: "workspace-001",
				WriteKey:    writeKey,
				SourceDefinition: backendconfig.SourceDefinitionT{
					Category: "eventStream",
					Name:     "javascript",
				},
			},
		})
		r := httptest.NewRequest("POST", "/v1/track", nil)
		r.SetBasicAuth(writeKey, "")
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "track"))
		w := httptest.NewRecorder()
		gw.writeKeyAuth(delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusNotFound, w.Code,
			"disabled source should return HTTP 404 Not Found")
		body, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, "source is disabled\n", string(body),
			"response body should match response.SourceDisabled")
	})

	t.Run("Empty Authorization header", func(t *testing.T) {
		// Verify the exact HTTP 401 Unauthorized response when no Authorization
		// header is present at all. Segment SDKs always include the Authorization
		// header, but this tests the defensive behavior when a raw HTTP client
		// omits credentials entirely.
		gw := newGateway(map[string]backendconfig.SourceT{
			"someKey": {Enabled: true},
		})
		r := httptest.NewRequest("POST", "/v1/track", nil)
		// Deliberately do NOT set any Authorization header.
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "track"))
		w := httptest.NewRecorder()
		gw.writeKeyAuth(delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusUnauthorized, w.Code,
			"missing Authorization header should return HTTP 401 Unauthorized")
		body, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, "failed to read writekey from header\n", string(body),
			"response body should match response.NoWriteKeyInBasicAuth")
	})

	t.Run("Query param writeKey for beacon endpoints", func(t *testing.T) {
		// Validate the beacon-style authentication where writeKey is passed as
		// a URL query parameter instead of the Basic Auth header. The
		// beaconInterceptor reads the writeKey from query params, converts it
		// to a Basic Auth header via r.SetBasicAuth(writeKey, ""), and then
		// delegates to the downstream handler which runs writeKeyAuth.
		// This is the pattern used by analytics.js navigator.sendBeacon() calls.
		writeKey := "beaconWriteKey789"
		gw := newGateway(map[string]backendconfig.SourceT{
			writeKey: {Enabled: true},
		})
		r := httptest.NewRequest("POST", "/beacon/v1/batch", nil)
		// Set writeKey as a query parameter, not as a Basic Auth header.
		params := url.Values{}
		params.Add("writeKey", writeKey)
		r.URL.RawQuery = params.Encode()
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, "batch"))
		w := httptest.NewRecorder()

		// Chain the beaconInterceptor with writeKeyAuth to validate the full
		// query-param → Basic Auth → writeKeyAuth authentication flow.
		gw.beaconInterceptor(gw.writeKeyAuth(delegate)).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code,
			"beacon query param writeKey should authenticate through beaconInterceptor → writeKeyAuth")
		body, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, "OK", string(body))
	})
}
