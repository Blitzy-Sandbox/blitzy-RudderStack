package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
)

func TestBeaconInterceptor(t *testing.T) {
	refTime := time.Now().UTC()

	delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})

	newGateway := func() *Handle {
		return &Handle{
			logger: logger.NOP,
			stats:  stats.Default,
			now:    func() time.Time { return refTime },
		}
	}
	newBeaconRequest := func(values url.Values) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if values != nil {
			r.URL.RawQuery = values.Encode()
		}
		return r
	}

	t.Run("valid request", func(t *testing.T) {
		gw := newGateway()
		r := newBeaconRequest(url.Values{
			"writeKey": []string{"123"},
		})
		w := httptest.NewRecorder()
		gw.beaconInterceptor(delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "request should succeed")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, "OK", string(gif))
	})

	t.Run("no writeKey", func(t *testing.T) {
		gw := newGateway()
		r := newBeaconRequest(url.Values{})
		w := httptest.NewRecorder()
		gw.beaconInterceptor(delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusUnauthorized, w.Code, "authentication should not succeed")
		body, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, "failed to read writekey from query params\n", string(body))
	})
}

// TestBeaconAnalytics20Compatibility validates that the beacon interceptor correctly handles
// navigator.sendBeacon() payloads from the Segment Analytics 2.0 JavaScript SDK.
// sendBeacon() may send payloads with Content-Type values other than application/json
// (e.g., text/plain or application/x-www-form-urlencoded depending on the browser).
// The interceptor must extract the writeKey from query params, set Basic Auth, and
// delegate to the batch handler regardless of the content type.
func TestBeaconAnalytics20Compatibility(t *testing.T) {
	refTime := time.Now().UTC()

	newGateway := func() *Handle {
		return &Handle{
			logger: logger.NOP,
			stats:  stats.Default,
			now:    func() time.Time { return refTime },
		}
	}

	tests := []struct {
		name         string
		queryParams  url.Values
		contentType  string
		body         string
		wantStatus   int
		wantDelegate bool
		wantUsername  string
	}{
		{
			name:        "sendBeacon text/plain content type",
			queryParams: url.Values{"writeKey": []string{"beacon-text-key-001"}},
			contentType: "text/plain",
			body: `{"batch":[{"type":"track","event":"Button Clicked",` +
				`"properties":{"label":"Sign Up"},"anonymousId":"anon-001",` +
				`"messageId":"msg-001"}]}`,
			wantStatus:   http.StatusOK,
			wantDelegate: true,
			wantUsername:  "beacon-text-key-001",
		},
		{
			name:        "sendBeacon application/x-www-form-urlencoded content type",
			queryParams: url.Values{"writeKey": []string{"beacon-form-key-002"}},
			contentType: "application/x-www-form-urlencoded",
			body: `{"batch":[{"type":"identify","userId":"user-002",` +
				`"traits":{"email":"synthetic@example.com","name":"Test User"},` +
				`"anonymousId":"anon-002","messageId":"msg-002"}]}`,
			wantStatus:   http.StatusOK,
			wantDelegate: true,
			wantUsername:  "beacon-form-key-002",
		},
		{
			name:        "writeKey extraction from query params with batch payload",
			queryParams: url.Values{"writeKey": []string{"batch-mixed-key-003"}},
			contentType: "application/json",
			body: `{"batch":[` +
				`{"type":"identify","userId":"user-003","traits":{"plan":"premium"},` +
				`"anonymousId":"anon-003","messageId":"msg-003a"},` +
				`{"type":"track","event":"Order Completed",` +
				`"properties":{"orderId":"order-789","total":99.99},` +
				`"userId":"user-003","anonymousId":"anon-003","messageId":"msg-003b"}` +
				`],"sentAt":"2024-01-15T10:30:00.000Z"}`,
			wantStatus:   http.StatusOK,
			wantDelegate: true,
			wantUsername:  "batch-mixed-key-003",
		},
		{
			name: "multiple query params with writeKey",
			queryParams: url.Values{
				"writeKey": []string{"multi-param-key-004"},
				"extra":    []string{"value"},
				"another":  []string{"param"},
			},
			contentType: "text/plain",
			body: `{"batch":[{"type":"track","event":"Page Loaded",` +
				`"anonymousId":"anon-004","messageId":"msg-004"}]}`,
			wantStatus:   http.StatusOK,
			wantDelegate: true,
			wantUsername:  "multi-param-key-004",
		},
		{
			name:         "empty writeKey in query params",
			queryParams:  url.Values{"writeKey": []string{""}},
			contentType:  "text/plain",
			body:         `{"batch":[{"type":"track","event":"Test"}]}`,
			wantStatus:   http.StatusUnauthorized,
			wantDelegate: false,
		},
		{
			name:        "Analytics 2.0 batch payload via sendBeacon",
			queryParams: url.Values{"writeKey": []string{"analytics20-key-006"}},
			contentType: "text/plain",
			body: `{"batch":[{"type":"track","event":"CTA Clicked",` +
				`"properties":{"buttonId":"hero-signup","variant":"blue"},` +
				`"context":{` +
				`"library":{"name":"analytics.js","version":"2.0.0"},` +
				`"page":{"url":"https://example.com/pricing","path":"/pricing",` +
				`"referrer":"https://example.com","title":"Pricing","search":""},` +
				`"userAgent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"},` +
				`"anonymousId":"anon-006","messageId":"ajs-2.0-msg-006",` +
				`"timestamp":"2024-01-15T10:30:00.000Z"}],` +
				`"sentAt":"2024-01-15T10:30:01.000Z"}`,
			wantStatus:   http.StatusOK,
			wantDelegate: true,
			wantUsername:  "analytics20-key-006",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := newGateway()

			// Track delegate invocation and capture auth/body details
			var delegateCalled bool
			var capturedUsername, capturedPassword string
			var capturedAuthOk bool
			var capturedBody string

			delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				delegateCalled = true
				capturedUsername, capturedPassword, capturedAuthOk = r.BasicAuth()
				bodyBytes, readErr := io.ReadAll(r.Body)
				require.NoError(t, readErr, "reading delegate request body should succeed")
				capturedBody = string(bodyBytes)
				_, _ = w.Write([]byte("OK"))
			})

			// Build POST request with body (sendBeacon uses POST)
			var bodyReader io.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			}
			r := httptest.NewRequest("POST", "/", bodyReader)
			r.URL.RawQuery = tc.queryParams.Encode()
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}

			w := httptest.NewRecorder()
			gw.beaconInterceptor(delegate).ServeHTTP(w, r)

			require.Equal(t, tc.wantStatus, w.Code, "unexpected HTTP status code")

			if tc.wantDelegate {
				require.True(t, delegateCalled,
					"delegate handler should have been called for valid writeKey")
				require.True(t, capturedAuthOk,
					"Basic Auth header should be present on the delegated request")
				require.Equal(t, tc.wantUsername, capturedUsername,
					"writeKey should be set as the Basic Auth username")
				require.Empty(t, capturedPassword,
					"Basic Auth password should be empty (Segment SDK convention)")

				// Verify the request body is passed through unmodified to the delegate
				if tc.body != "" {
					require.Equal(t, tc.body, capturedBody,
						"request body should be preserved intact for the delegate handler")
				}
			} else {
				require.False(t, delegateCalled,
					"delegate handler should not have been called for invalid/empty writeKey")

				// Verify the error response body for unauthorized requests
				respBody, err := io.ReadAll(w.Body)
				require.NoError(t, err, "reading error response body should succeed")
				require.Equal(t, "failed to read writekey from query params\n", string(respBody),
					"error response should indicate missing writeKey in query params")
			}
		})
	}
}
