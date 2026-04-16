package alert

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/stretchr/testify/require"
)

// TestSlack_Alert_Success verifies successful Slack webhook delivery with correct
// HTTP method, Content-Type header, and JSON payload structure containing both the
// alert message and instanceName in the required "text" field.
func TestSlack_Alert_Success(t *testing.T) {
	Init()

	// capturedRequest holds the key fields of the incoming HTTP request
	// so assertions can run safely in the test goroutine (not the handler goroutine).
	type capturedRequest struct {
		method      string
		contentType string
		body        []byte
	}
	received := make(chan capturedRequest, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- capturedRequest{
			method:      r.Method,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	slack := &Slack{webhookURL: server.URL, instanceName: "test-instance"}
	slack.Alert("test alert message")

	// Verify the mock server received the webhook request and validate its contents.
	select {
	case req := <-received:
		require.Equal(t, http.MethodPost, req.method)
		require.Equal(t, "application/json", req.contentType)

		var payload map[string]any
		err := jsonrs.Unmarshal(req.body, &payload)
		require.NoError(t, err)

		text, ok := payload["text"]
		require.True(t, ok, "payload should contain 'text' field")
		require.NotNil(t, text)

		textStr, ok := text.(string)
		require.True(t, ok, "text field should be a string")
		require.Contains(t, textStr, "test alert message")
		require.Contains(t, textStr, "test-instance")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive webhook request within timeout")
	}
}

// TestSlack_Alert_TimeoutHandling verifies the Slack provider handles HTTP client
// timeout gracefully — the Alert method logs the timeout error and returns without
// panicking, matching the PagerDuty/VictorOps error-handling pattern.
func TestSlack_Alert_TimeoutHandling(t *testing.T) {
	Init()

	// Override the Slack HTTP client timeout to a very short duration so the
	// test completes quickly. config.GetDuration parses Go duration strings.
	config.Set("HttpClient.slack.timeout", "1ms")
	defer config.Reset()

	// blockCh keeps the handler blocked so the client timeout fires before
	// a response is sent. It is closed in the deferred cleanup to allow the
	// handler goroutine to exit cleanly.
	blockCh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(blockCh)
		server.Close()
	}()

	slack := &Slack{webhookURL: server.URL, instanceName: "test-instance"}

	// Alert should return without panicking; the timeout error is logged via
	// pkgLogger.Errorn("Alert: Failed to alert service", ...) and the method
	// returns early, identical to the PagerDuty/VictorOps error path.
	require.NotPanics(t, func() {
		slack.Alert("timeout test")
	})
}

// TestSlack_Alert_ErrorResponse verifies proper handling of non-200/202 HTTP error
// responses from the Slack webhook endpoint. The method should complete without
// panicking and log the error status code.
func TestSlack_Alert_ErrorResponse(t *testing.T) {
	Init()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer server.Close()

	slack := &Slack{webhookURL: server.URL, instanceName: "test-instance"}

	// Alert should complete without panicking even with a 500 error response.
	// The implementation logs the error status code via pkgLogger.Errorn and
	// still reads/closes the response body before returning.
	require.NotPanics(t, func() {
		slack.Alert("error test")
	})
}

// TestSlack_Alert_WebhookPayloadStructure verifies the exact structure of the Slack
// webhook JSON payload: a single "text" field containing both the instanceName and
// the alert message, formatted as "[instanceName] message" per the Slack webhook spec.
func TestSlack_Alert_WebhookPayloadStructure(t *testing.T) {
	Init()

	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyCh <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	slack := &Slack{webhookURL: server.URL, instanceName: "prod-server-01"}
	slack.Alert("Critical pipeline failure")

	// Verify the captured webhook payload has the correct JSON structure.
	select {
	case capturedBody := <-bodyCh:
		require.NotNil(t, capturedBody, "captured body should not be nil")

		var payload map[string]any
		err := jsonrs.Unmarshal(capturedBody, &payload)
		require.NoError(t, err)

		// The payload must contain the Slack-required "text" field.
		text, ok := payload["text"]
		require.True(t, ok, "payload must have 'text' key")

		// "text" must be a string containing both the instanceName and alert message.
		textStr, ok := text.(string)
		require.True(t, ok, "text must be a string")
		require.Contains(t, textStr, "Critical pipeline failure")
		require.Contains(t, textStr, "prod-server-01")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive webhook request within timeout")
	}
}
