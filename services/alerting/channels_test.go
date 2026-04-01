package alerting_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"

	"github.com/rudderlabs/rudder-server/services/alerting"
)

// testAlert returns a fully populated Alert test fixture with deterministic values.
// Using a fixed timestamp ensures reproducible assertions on the serialized payload.
func testAlert() alerting.Alert {
	return alerting.Alert{
		RuleID:      "test-rule-1",
		Condition:   alerting.ThroughputDrop,
		Message:     "throughput below threshold",
		Value:       500.0,
		Threshold:   1000.0,
		WorkspaceID: "ws-test-1",
		Timestamp:   time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}
}

// ---------------------------------------------------------------------------
// WebhookChannel tests
// ---------------------------------------------------------------------------

// TestWebhookChannel_Success validates that WebhookChannel sends a properly
// formatted JSON POST request to the configured webhook URL and reports no
// error when the server returns HTTP 200 OK.
func TestWebhookChannel_Success(t *testing.T) {
	t.Parallel()

	alert := testAlert()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request method and content type header.
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/json; charset=utf-8", r.Header.Get("Content-Type"))

		// Read and unmarshal the JSON body.
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var received alerting.Alert
		err = jsonrs.Unmarshal(body, &received)
		require.NoError(t, err)

		// Verify all alert fields are delivered correctly.
		require.Equal(t, alert.RuleID, received.RuleID)
		require.Equal(t, alert.Condition, received.Condition)
		require.Equal(t, alert.Message, received.Message)
		require.Equal(t, alert.Value, received.Value)
		require.Equal(t, alert.Threshold, received.Threshold)
		require.Equal(t, alert.WorkspaceID, received.WorkspaceID)
		require.Equal(t, alert.Timestamp.UTC(), received.Timestamp.UTC())

		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()

	ctx := context.Background()
	channel := alerting.NewWebhookChannel(s.URL, 5*time.Second, 1)
	err := channel.Send(ctx, alert)
	require.NoError(t, err)
}

// TestWebhookChannel_Retry validates that WebhookChannel retries on retriable
// HTTP status codes (5xx) and eventually succeeds when the server recovers.
// Pattern reference: services/alerta/client_test.go "unexpected retriable status".
func TestWebhookChannel_Retry(t *testing.T) {
	t.Parallel()

	const maxRetries = 2
	var count int64

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&count, 1)
		if n <= int64(maxRetries) {
			// Return retriable 500 for the first maxRetries attempts.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Succeed on the final attempt.
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()

	ctx := context.Background()
	channel := alerting.NewWebhookChannel(s.URL, 5*time.Second, maxRetries)

	alert := testAlert()
	err := channel.Send(ctx, alert)
	require.NoError(t, err)

	// Total requests = maxRetries (failed) + 1 (success)
	require.Equalf(t, int64(maxRetries+1), atomic.LoadInt64(&count),
		"expected %d total requests (retries=%d)", maxRetries+1, maxRetries)
}

// TestWebhookChannel_RetryExhausted validates that when the server always
// returns a retriable 5xx status, the channel exhausts all retries and
// returns an error with the expected status code message.
func TestWebhookChannel_RetryExhausted(t *testing.T) {
	t.Parallel()

	const maxRetries = 2
	var count int64

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer s.Close()

	ctx := context.Background()
	channel := alerting.NewWebhookChannel(s.URL, 5*time.Second, maxRetries)

	alert := testAlert()
	err := channel.Send(ctx, alert)
	require.EqualError(t, err, "unexpected status code 500: ")

	// Should have tried maxRetries+1 total times.
	require.Equalf(t, int64(maxRetries+1), atomic.LoadInt64(&count),
		"retry %d times", maxRetries)
}

// TestWebhookChannel_NonRetriableFailure validates that non-retriable HTTP
// status codes (4xx except 408, 429) cause immediate failure without retries.
// Pattern reference: services/alerta/client_test.go "unexpected non-retriable status".
func TestWebhookChannel_NonRetriableFailure(t *testing.T) {
	t.Parallel()

	const maxRetries = 2
	var count int64

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer s.Close()

	ctx := context.Background()
	channel := alerting.NewWebhookChannel(s.URL, 5*time.Second, maxRetries)

	alert := testAlert()
	err := channel.Send(ctx, alert)
	require.EqualError(t, err, "non retriable: unexpected status code 400: bad request")

	// Non-retriable errors must not be retried — exactly one request.
	require.Equalf(t, int64(1), atomic.LoadInt64(&count),
		"should not retry non-retriable errors")
}

// TestWebhookChannel_Timeout validates that WebhookChannel properly
// respects its configured timeout when the server does not respond.
// Pattern reference: services/alerta/client_test.go "timeout".
func TestWebhookChannel_Timeout(t *testing.T) {
	t.Parallel()

	blocker := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocker
	}))
	defer s.Close()
	defer close(blocker)

	ctx := context.Background()
	// Very short timeout to trigger deadline exceeded quickly.
	channel := alerting.NewWebhookChannel(s.URL, time.Millisecond, 1)

	alert := testAlert()
	err := channel.Send(ctx, alert)
	require.Error(t, err, "deadline exceeded")
}

// ---------------------------------------------------------------------------
// SlackChannel tests
// ---------------------------------------------------------------------------

// TestSlackChannel_Success validates that SlackChannel sends a properly
// formatted Slack message payload via HTTP POST and succeeds when the
// server returns HTTP 200 OK.
func TestSlackChannel_Success(t *testing.T) {
	t.Parallel()

	alert := testAlert()

	// Build the expected Slack text payload matching the SlackChannel.Send format.
	expectedText := fmt.Sprintf(
		":rotating_light: *Alert Triggered*\n*Rule:* %s\n*Condition:* %s\n*Value:* %.2f (Threshold: %.2f)\n*Workspace:* %s\n*Time:* %s",
		alert.RuleID, alert.Condition, alert.Value, alert.Threshold,
		alert.WorkspaceID, alert.Timestamp.Format(time.RFC3339),
	)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request method and content type.
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/json; charset=utf-8", r.Header.Get("Content-Type"))

		// Read and unmarshal the JSON body.
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var payload map[string]any
		err = jsonrs.Unmarshal(body, &payload)
		require.NoError(t, err)

		// Verify the "text" field matches the expected Slack message format.
		require.Equal(t, expectedText, payload["text"])

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer s.Close()

	ctx := context.Background()
	channel := alerting.NewSlackChannel(s.URL, 5*time.Second, 1)
	err := channel.Send(ctx, alert)
	require.NoError(t, err)
}

// TestSlackChannel_FailureRetry validates that SlackChannel retries on
// retriable HTTP 5xx status codes and eventually succeeds on recovery.
func TestSlackChannel_FailureRetry(t *testing.T) {
	t.Parallel()

	const maxRetries = 1
	var count int64

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&count, 1)
		if n <= 1 {
			// First request fails with retriable 500.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Second request succeeds.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer s.Close()

	ctx := context.Background()
	channel := alerting.NewSlackChannel(s.URL, 5*time.Second, maxRetries)

	alert := testAlert()
	err := channel.Send(ctx, alert)
	require.NoError(t, err)

	// Total requests = 1 (failed) + 1 (success) = maxRetries + 1
	require.Equalf(t, int64(maxRetries+1), atomic.LoadInt64(&count),
		"expected %d total requests", maxRetries+1)
}

// TestSlackChannel_NonRetriableFailure validates that SlackChannel does not
// retry on non-retriable HTTP 4xx status codes.
func TestSlackChannel_NonRetriableFailure(t *testing.T) {
	t.Parallel()

	const maxRetries = 2
	var count int64

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid_payload"))
	}))
	defer s.Close()

	ctx := context.Background()
	channel := alerting.NewSlackChannel(s.URL, 5*time.Second, maxRetries)

	alert := testAlert()
	err := channel.Send(ctx, alert)
	require.EqualError(t, err, "non retriable: unexpected status code 400: invalid_payload")

	// Should not retry on 4xx — exactly one request.
	require.Equalf(t, int64(1), atomic.LoadInt64(&count),
		"should not retry non-retriable errors")
}

// ---------------------------------------------------------------------------
// EmailChannel tests
// ---------------------------------------------------------------------------

// TestEmailChannel_ConfigValidation validates that EmailChannel.Send returns
// a descriptive error for each type of configuration violation: empty SMTP
// host, empty from address, and empty recipient list.
func TestEmailChannel_ConfigValidation(t *testing.T) {
	t.Parallel()

	alert := testAlert()
	ctx := context.Background()

	t.Run("missing SMTP host", func(t *testing.T) {
		t.Parallel()
		channel := alerting.NewEmailChannel("", "587", "alerts@example.com",
			[]string{"ops@example.com"}, "", "")
		err := channel.Send(ctx, alert)
		require.EqualError(t, err, "email channel misconfigured: empty SMTP host")
	})

	t.Run("missing from address", func(t *testing.T) {
		t.Parallel()
		channel := alerting.NewEmailChannel("smtp.example.com", "587", "",
			[]string{"ops@example.com"}, "", "")
		err := channel.Send(ctx, alert)
		require.EqualError(t, err, "email channel misconfigured: empty from address")
	})

	t.Run("no recipients", func(t *testing.T) {
		t.Parallel()
		channel := alerting.NewEmailChannel("smtp.example.com", "587",
			"alerts@example.com", nil, "", "")
		err := channel.Send(ctx, alert)
		require.EqualError(t, err, "email channel misconfigured: no recipients")
	})

	t.Run("empty recipients slice", func(t *testing.T) {
		t.Parallel()
		channel := alerting.NewEmailChannel("smtp.example.com", "587",
			"alerts@example.com", []string{}, "", "")
		err := channel.Send(ctx, alert)
		require.EqualError(t, err, "email channel misconfigured: no recipients")
	})
}

// TestEmailChannel_FormatMessage validates that when provided with a valid
// configuration, the EmailChannel formats the message and attempts SMTP
// delivery. Because there is no real SMTP server, the call fails with a
// connection error — the test verifies that the function progressed past
// configuration validation and message formatting into the smtp.SendMail
// call, indicated by the "sending email alert" error wrapper.
func TestEmailChannel_FormatMessage(t *testing.T) {
	t.Parallel()

	alert := testAlert()
	ctx := context.Background()

	// Use localhost port 0 so config validation passes but smtp.SendMail
	// fails with a connection error — proving that formatting completed.
	channel := alerting.NewEmailChannel("127.0.0.1", "0", "alerts@example.com",
		[]string{"ops@example.com"}, "", "")
	err := channel.Send(ctx, alert)

	// An error must be returned (SMTP server unreachable).
	require.Error(t, err)
	// The error must be wrapped with "sending email alert:", proving that
	// the function executed the message formatting code and reached the
	// smtp.SendMail call — not stopped by config validation.
	require.ErrorContains(t, err, "sending email alert")
}
