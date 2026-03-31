// channels.go implements the NotificationChannel interface and three concrete
// channel implementations for alert delivery: WebhookChannel, EmailChannel,
// and SlackChannel. Each follows the retry/backoff pattern from
// services/alerta/client.go.
package alerting

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/utils/backoffvoid"
	"github.com/rudderlabs/rudder-server/utils/httputil"
)

// ---------------------------------------------------------------------------
// NotificationChannel — core interface for alert delivery
// ---------------------------------------------------------------------------

// NotificationChannel defines the contract for alert notification delivery.
// All channel implementations must satisfy this interface, enabling the
// AlertEngine to dispatch triggered alerts to multiple channels uniformly.
type NotificationChannel interface {
	Send(ctx context.Context, alert Alert) error
}

// ---------------------------------------------------------------------------
// Alert — triggered alert payload
// ---------------------------------------------------------------------------

// Alert represents a triggered alert ready for notification delivery.
// It contains the rule that triggered, the condition and metric values,
// and the workspace and timestamp for contextual identification.
type Alert struct {
	RuleID      string         `json:"rule_id"`
	Condition   AlertCondition `json:"condition"`
	Message     string         `json:"message"`
	Value       float64        `json:"value"`
	Threshold   float64        `json:"threshold"`
	WorkspaceID string         `json:"workspace_id"`
	Timestamp   time.Time      `json:"timestamp"`
}

// ---------------------------------------------------------------------------
// WebhookChannel — HTTP POST delivery with retry/backoff
// ---------------------------------------------------------------------------

// WebhookChannel delivers alerts via HTTP POST to a configured webhook URL.
// It uses retry with exponential backoff for transient failures, following
// the pattern from services/alerta/client.go.
type WebhookChannel struct {
	url        string
	client     *http.Client
	maxRetries int
}

// NewWebhookChannel creates a new WebhookChannel with the specified URL,
// HTTP timeout, and maximum retry count.
func NewWebhookChannel(url string, timeout time.Duration, maxRetries int) *WebhookChannel {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxRetries < 0 {
		maxRetries = 3
	}
	return &WebhookChannel{
		url: url,
		client: &http.Client{
			Timeout: timeout,
		},
		maxRetries: maxRetries,
	}
}

// Send delivers an alert to the webhook URL via HTTP POST with JSON payload.
// Follows the retry/backoff pattern from services/alerta/client.go lines 197-262.
func (w *WebhookChannel) Send(ctx context.Context, alert Alert) error {
	body, err := jsonrs.Marshal(alert)
	if err != nil {
		return fmt.Errorf("marshalling webhook alert: %w", err)
	}

	sendErr := backoffvoid.Retry(ctx, func() error {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
		if reqErr != nil {
			return fmt.Errorf("creating http request: %w", reqErr)
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, doErr := w.client.Do(req)
		if doErr != nil {
			return fmt.Errorf("http request to %q: %w", w.url, doErr)
		}
		defer func() { httputil.CloseResponse(resp) }()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		respBody, _ := io.ReadAll(resp.Body)
		respErr := fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(respBody))
		if !httputil.RetriableStatus(resp.StatusCode) {
			return backoff.Permanent(fmt.Errorf("non retriable: %w", respErr))
		}
		return respErr
	}, backoff.WithMaxTries(uint(w.maxRetries+1)))

	if sendErr != nil {
		pkgLogger.Errorn("Webhook alert delivery failed",
			logger.NewStringField("url", w.url),
			logger.NewStringField("ruleID", alert.RuleID),
			obskit.Error(sendErr))
	}
	return sendErr
}

// ---------------------------------------------------------------------------
// EmailChannel — SMTP-based email delivery
// ---------------------------------------------------------------------------

// EmailChannel delivers alerts via SMTP email. It supports optional
// authentication via smtp.PlainAuth.
type EmailChannel struct {
	smtpHost string
	smtpPort string
	from     string
	to       []string
	auth     smtp.Auth
}

// NewEmailChannel creates a new EmailChannel with the specified SMTP configuration.
// If username and password are provided, SMTP PlainAuth is configured.
func NewEmailChannel(host, port, from string, to []string, username, password string) *EmailChannel {
	var auth smtp.Auth
	if username != "" && password != "" {
		auth = smtp.PlainAuth("", username, password, host)
	}
	return &EmailChannel{
		smtpHost: host,
		smtpPort: port,
		from:     from,
		to:       to,
		auth:     auth,
	}
}

// Send delivers an alert via SMTP email with RFC 822 formatted headers and body.
func (e *EmailChannel) Send(ctx context.Context, alert Alert) error {
	if e.smtpHost == "" {
		return fmt.Errorf("email channel misconfigured: empty SMTP host")
	}
	if e.from == "" {
		return fmt.Errorf("email channel misconfigured: empty from address")
	}
	if len(e.to) == 0 {
		return fmt.Errorf("email channel misconfigured: no recipients")
	}

	subject := fmt.Sprintf("[Alert] %s: %s", alert.Condition, alert.Message)
	body := fmt.Sprintf(
		"Alert: %s\r\nCondition: %s\r\nValue: %.2f\r\nThreshold: %.2f\r\nWorkspace: %s\r\nTime: %s\r\n",
		alert.RuleID, alert.Condition, alert.Value, alert.Threshold,
		alert.WorkspaceID, alert.Timestamp.Format(time.RFC3339),
	)

	msg := fmt.Sprintf(
		"Subject: %s\r\nFrom: %s\r\nTo: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=\"utf-8\"\r\n\r\n%s",
		subject, e.from, strings.Join(e.to, ","), body,
	)

	addr := net.JoinHostPort(e.smtpHost, e.smtpPort)
	if err := smtp.SendMail(addr, e.auth, e.from, e.to, []byte(msg)); err != nil {
		sendErr := fmt.Errorf("sending email alert: %w", err)
		pkgLogger.Errorn("Email alert delivery failed",
			logger.NewStringField("ruleID", alert.RuleID),
			obskit.Error(sendErr))
		return sendErr
	}
	return nil
}

// ---------------------------------------------------------------------------
// SlackChannel — Slack incoming webhook delivery with retry/backoff
// ---------------------------------------------------------------------------

// SlackChannel delivers alerts via Slack incoming webhook URL. It formats
// alerts as Slack message payloads with structured text blocks.
type SlackChannel struct {
	webhookURL string
	client     *http.Client
	maxRetries int
}

// NewSlackChannel creates a new SlackChannel with the specified webhook URL,
// HTTP timeout, and maximum retry count.
func NewSlackChannel(webhookURL string, timeout time.Duration, maxRetries int) *SlackChannel {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxRetries < 0 {
		maxRetries = 3
	}
	return &SlackChannel{
		webhookURL: webhookURL,
		client: &http.Client{
			Timeout: timeout,
		},
		maxRetries: maxRetries,
	}
}

// Send delivers an alert to the Slack webhook URL with a formatted message payload.
// Follows the same retry/backoff pattern as WebhookChannel.
func (s *SlackChannel) Send(ctx context.Context, alert Alert) error {
	slackPayload := map[string]any{
		"text": fmt.Sprintf(
			":rotating_light: *Alert Triggered*\n*Rule:* %s\n*Condition:* %s\n*Value:* %.2f (Threshold: %.2f)\n*Workspace:* %s\n*Time:* %s",
			alert.RuleID, alert.Condition, alert.Value, alert.Threshold,
			alert.WorkspaceID, alert.Timestamp.Format(time.RFC3339),
		),
	}

	body, err := jsonrs.Marshal(slackPayload)
	if err != nil {
		return fmt.Errorf("marshalling slack payload: %w", err)
	}

	sendErr := backoffvoid.Retry(ctx, func() error {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
		if reqErr != nil {
			return fmt.Errorf("creating http request: %w", reqErr)
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, doErr := s.client.Do(req)
		if doErr != nil {
			return fmt.Errorf("http request to slack: %w", doErr)
		}
		defer func() { httputil.CloseResponse(resp) }()

		if resp.StatusCode == http.StatusOK {
			return nil
		}

		respBody, _ := io.ReadAll(resp.Body)
		respErr := fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(respBody))
		if !httputil.RetriableStatus(resp.StatusCode) {
			return backoff.Permanent(fmt.Errorf("non retriable: %w", respErr))
		}
		return respErr
	}, backoff.WithMaxTries(uint(s.maxRetries+1)))

	if sendErr != nil {
		pkgLogger.Errorn("Slack alert delivery failed",
			logger.NewStringField("webhookURL", s.webhookURL),
			logger.NewStringField("ruleID", alert.RuleID),
			obskit.Error(sendErr))
	}
	return sendErr
}
