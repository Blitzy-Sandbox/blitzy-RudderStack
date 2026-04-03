package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-server/gateway/response"
)

func TestPixelInterceptor(t *testing.T) {
	refTime := time.Now().UTC()

	newGateway := func() *Handle {
		return &Handle{
			logger: logger.NOP,
			stats:  stats.Default,
			now:    func() time.Time { return refTime },
		}
	}
	newPixelRequest := func(values url.Values) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if values != nil {
			r.URL.RawQuery = values.Encode()
		}
		return r
	}

	t.Run("valid request without extra fields", func(t *testing.T) {
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
			w.WriteHeader(http.StatusOK)
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey": []string{"123"},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("pixel", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "request should succeed")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif))

		require.NotNil(t, payload)
		require.Equal(t, fmt.Sprintf(`{"channel": "web","integrations": {"All": true},"originalTimestamp":"%[1]s","sentAt":"%[1]s","type":"pixel"}`, refTime.Format(time.RFC3339Nano)), string(payload))
	})

	t.Run("valid request with extra fields", func(t *testing.T) {
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey": []string{"123"},
			"random":   []string{"random"},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("page", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "request should succeed")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif))

		require.NotNil(t, payload)
		require.Equal(t, fmt.Sprintf(`{"channel": "web","integrations": {"All": true},"originalTimestamp":"%[1]s","sentAt":"%[1]s","random":"random","type":"page"}`, refTime.Format(time.RFC3339Nano)), string(payload))
	})

	t.Run("no writeKey", func(t *testing.T) {
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("pixel", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "request should succeed")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif))

		require.Nil(t, payload, "request should not have been forwarded")
	})

	t.Run("track without event", func(t *testing.T) {
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey": []string{"123"},
			"event":    []string{""},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("track", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "request should succeed")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif))

		require.Nil(t, payload, "request should not have been forwarded")
	})
}

// TestPixelWebSDKCompatibility validates pixel tracking endpoints for JavaScript web SDK
// (analytics.js / Analytics 2.0) compatibility as part of E-006 SDK compatibility testing.
// It verifies that GET-based pixel requests with query parameters are correctly converted to
// JSON POST bodies, that writeKey is extracted and set as Basic Auth, and that a 1x1 GIF is
// always returned regardless of the delegate handler's response.
func TestPixelWebSDKCompatibility(t *testing.T) {
	refTime := time.Now().UTC()

	newGateway := func() *Handle {
		return &Handle{
			logger: logger.NOP,
			stats:  stats.Default,
			now:    func() time.Time { return refTime },
		}
	}
	newPixelRequest := func(values url.Values) *http.Request {
		r := httptest.NewRequest("GET", "/pixel/v1/track", nil)
		if values != nil {
			r.URL.RawQuery = values.Encode()
		}
		return r
	}

	t.Run("track pixel with event name in query param", func(t *testing.T) {
		// Validates that the pixel interceptor correctly handles track-type pixel requests
		// with an event name and nested properties passed as query params, as produced by
		// analytics.js image tag tracking (e.g., <img src="/pixel/v1/track?writeKey=...&event=...">)
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey":            []string{"abc"},
			"event":               []string{"Button Clicked"},
			"properties.category": []string{"signup"},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("track", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "should return 200 OK")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif), "should return 1x1 transparent GIF")

		require.NotNil(t, payload, "delegate should have received the request")
		payloadStr := string(payload)
		require.Contains(t, payloadStr, `"type":"track"`, "payload should contain type track")
		require.Contains(t, payloadStr, `"event":"Button Clicked"`, "payload should contain event name")
		require.Contains(t, payloadStr, `"category":"signup"`, "payload should contain properties.category via sjson dot-path nesting")
		require.Contains(t, payloadStr, `"channel": "web"`, "payload should contain web channel")
		require.Contains(t, payloadStr, `"integrations": {"All": true}`, "payload should contain default integrations")
	})

	t.Run("page pixel with optional name param", func(t *testing.T) {
		// Validates that the pixel interceptor correctly handles page-type pixel requests
		// with an optional name query parameter, producing a JSON body with the page name
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey": []string{"abc"},
			"name":     []string{"Home"},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("page", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "should return 200 OK")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif), "should return 1x1 transparent GIF")

		require.NotNil(t, payload, "delegate should have received the request")
		payloadStr := string(payload)
		require.Contains(t, payloadStr, `"name":"Home"`, "payload should contain page name")
		require.Contains(t, payloadStr, `"type":"page"`, "payload should contain type page")
		require.Contains(t, payloadStr, `"channel": "web"`, "payload should contain web channel")
	})

	t.Run("page pixel without name param", func(t *testing.T) {
		// Validates that the pixel interceptor handles page-type requests without the optional
		// name parameter — the request should still be forwarded with a valid JSON body
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey": []string{"abc"},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("page", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "should return 200 OK")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif), "should return 1x1 transparent GIF")

		require.NotNil(t, payload, "delegate should have received the request")
		payloadStr := string(payload)
		require.Contains(t, payloadStr, `"type":"page"`, "payload should contain type page")
		require.Contains(t, payloadStr, `"channel": "web"`, "payload should contain web channel")
		require.NotContains(t, payloadStr, `"name":`, "payload should not contain name field when not provided")
	})

	t.Run("GIF response regardless of delegate error", func(t *testing.T) {
		// Validates that the pixel interceptor always returns a 1x1 GIF to the client, even
		// when the downstream delegate handler returns an error status code. This is critical
		// for web SDK pixel tracking where the browser expects a valid image response.
		delegateCalled := false
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			delegateCalled = true
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal server error"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey": []string{"abc"},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("pixel", delegate).ServeHTTP(w, r)

		require.True(t, delegateCalled, "delegate should have been called")
		// The client-facing response must always be a 200 OK with a GIF, because the pixel
		// interceptor uses a separate pixelHttpWriter for the delegate and defers writing
		// the GIF to the original ResponseWriter
		require.Equal(t, http.StatusOK, w.Code, "should return 200 to client regardless of delegate error")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif), "should return 1x1 transparent GIF regardless of delegate error")
	})

	t.Run("query param to JSON conversion with nested properties", func(t *testing.T) {
		// Validates that the pixel interceptor correctly uses sjson dot-path notation to convert
		// flat query params like "properties.product.name=Widget" into nested JSON structures:
		// {"properties":{"product":{"name":"Widget"}}}. This is how web SDK image tag tracking
		// passes complex event properties.
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey":                 []string{"abc"},
			"event":                    []string{"Product Viewed"},
			"properties.product.name":  []string{"Widget"},
			"properties.product.price": []string{"9.99"},
			"properties.category":      []string{"Electronics"},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("track", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "should return 200 OK")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif), "should return 1x1 transparent GIF")

		require.NotNil(t, payload, "delegate should have received the request")
		payloadStr := string(payload)
		require.Contains(t, payloadStr, `"type":"track"`, "payload should contain type track")
		require.Contains(t, payloadStr, `"event":"Product Viewed"`, "payload should contain event name")
		// sjson interprets dots as path separators, so query params with dots produce nested JSON
		require.Contains(t, payloadStr, `"properties"`, "payload should contain properties object from dot-path conversion")
		require.Contains(t, payloadStr, `"name":"Widget"`, "payload should contain nested product name")
		require.Contains(t, payloadStr, `"price":"9.99"`, "payload should contain nested product price as string")
		require.Contains(t, payloadStr, `"category":"Electronics"`, "payload should contain nested category")
	})

	t.Run("pixel track with Segment analytics.js user-agent", func(t *testing.T) {
		// Validates that pixel tracking requests with a standard browser User-Agent header
		// (as sent by analytics.js when using image tag fallback) are processed correctly.
		// The pixel interceptor should not reject or alter behavior based on User-Agent.
		var payload []byte
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey": []string{"abc"},
			"event":    []string{"Page View"},
		})
		// Set a typical browser User-Agent header that analytics.js pixel tracking would send
		r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		w := httptest.NewRecorder()
		gw.pixelInterceptor("track", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "should return 200 OK with analytics.js user-agent")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif), "should return 1x1 transparent GIF")

		require.NotNil(t, payload, "delegate should have received the request with analytics.js user-agent")
		payloadStr := string(payload)
		require.Contains(t, payloadStr, `"type":"track"`, "payload should contain type track")
		require.Contains(t, payloadStr, `"event":"Page View"`, "payload should contain event name")
		require.Contains(t, payloadStr, `"channel": "web"`, "payload should contain web channel")
	})

	t.Run("pixel request preserves writeKey extraction", func(t *testing.T) {
		// Validates that the pixel interceptor correctly extracts the writeKey from query params
		// and sets it as Basic Auth credentials (username=writeKey, password="") on the delegated
		// request. This matches Segment's pixel tracking authentication pattern where the writeKey
		// is passed as a query parameter instead of an Authorization header.
		var capturedUser, capturedPass string
		var authOk bool
		delegate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedUser, capturedPass, authOk = r.BasicAuth()
			_, _ = w.Write([]byte("OK"))
		})
		gw := newGateway()
		r := newPixelRequest(url.Values{
			"writeKey": []string{"test-write-key-12345"},
		})
		w := httptest.NewRecorder()
		gw.pixelInterceptor("pixel", delegate).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code, "should return 200 OK")
		gif, err := io.ReadAll(w.Body)
		require.NoError(t, err, "reading response body should succeed")
		require.Equal(t, response.GetPixelResponse(), string(gif), "should return 1x1 transparent GIF")

		// Verify the writeKey was correctly extracted from query params and set as Basic Auth
		require.True(t, authOk, "delegate request should have Basic Auth credentials set")
		require.Equal(t, "test-write-key-12345", capturedUser, "writeKey should be set as Basic Auth username")
		require.Equal(t, "", capturedPass, "Basic Auth password should be empty (matching Segment SDK pattern)")
	})
}
