package alert

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	kithttputil "github.com/rudderlabs/rudder-go-kit/httputil"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// Slack implements AlertManager for Slack incoming webhook notifications.
// It posts JSON payloads to a configured Slack incoming webhook URL,
// following the same HTTP-based alert delivery pattern as PagerDuty and VictorOps.
type Slack struct {
	webhookURL   string
	instanceName string
}

// Alert sends an alert message to the configured Slack incoming webhook.
// The payload uses Slack's required "text" field, prefixed with the instance name
// for alert context identification. HTTP transport uses a configurable timeout
// and the response body is always properly drained and closed.
func (s *Slack) Alert(message string) {
	// Construct Slack webhook payload with the required "text" field.
	// The text includes the instance name for context identification.
	event := map[string]any{
		"text": fmt.Sprintf("[%s] %s", s.instanceName, message),
	}

	// Serialize the payload to JSON using the approved jsonrs package
	// (encoding/json is forbidden per .golangci.yml depguard rules).
	eventJSON, err := jsonrs.Marshal(event)
	if err != nil {
		pkgLogger.Errorn("Alert: Failed to marshal Slack payload", obskit.Error(err))
		return
	}

	// Create HTTP client with configurable timeout (default 30 seconds).
	// Config key follows the same pattern as pagerduty and victorops providers.
	client := &http.Client{Timeout: config.GetDuration("HttpClient.slack.timeout", 30, time.Second)}

	// POST the JSON payload to the Slack incoming webhook URL.
	resp, err := client.Post(s.webhookURL, "application/json", bytes.NewBuffer(eventJSON))
	if err != nil {
		pkgLogger.Errorn("Alert: Failed to alert service", obskit.Error(err))
		return
	}
	// Immediately defer response closure to ensure the body is drained and
	// closed on ALL code paths, including early returns and panics.
	defer func() { kithttputil.CloseResponse(resp) }()

	// Validate response status — Slack webhooks return 200 on success;
	// 202 is also accepted for consistency with other alert providers.
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		pkgLogger.Errorn("Alert: Got error response", logger.NewIntField("statusCode", int64(resp.StatusCode)))
	}

	// Read the response body for logging purposes.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		pkgLogger.Errorn("Alert: Failed to read response body", obskit.Error(err))
		return
	}

	pkgLogger.Infon("Alert: Successful", logger.NewStringField("body", string(body)))
}
