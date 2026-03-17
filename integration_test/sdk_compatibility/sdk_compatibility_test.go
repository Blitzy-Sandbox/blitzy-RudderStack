// Package sdk_compatibility implements end-to-end integration tests that validate
// Segment SDK payload format compatibility across the complete RudderStack pipeline:
// Gateway → Processor → Router → Webhook destination.
//
// It exercises payloads from all major Segment SDK platforms:
//   - JavaScript (analytics.js / Analytics 2.0) — E-006
//   - iOS (analytics-ios / Swift) — E-007
//   - Android (analytics-android / Kotlin) — E-007
//   - Server-side (Node.js, Python, Go, Java, Ruby) — E-008
//
// Each SDK's payload format is sent through the Gateway HTTP API and verified at the
// webhook destination for field-level preservation, correct authentication, and
// context metadata integrity (E-005).
package sdk_compatibility

import (
	"context"
	"database/sql"
	b64 "encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/errgroup"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	kithelper "github.com/rudderlabs/rudder-go-kit/testhelper"
	pgdocker "github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"
	transformertest "github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/transformer"
	"github.com/rudderlabs/rudder-go-kit/testhelper/rand"

	"github.com/rudderlabs/rudder-server/runner"
	"github.com/rudderlabs/rudder-server/testhelper/health"
	whUtil "github.com/rudderlabs/rudder-server/testhelper/webhook"
	"github.com/rudderlabs/rudder-server/testhelper/workspaceConfig"
	"github.com/rudderlabs/rudder-server/utils/httputil"
	"github.com/rudderlabs/rudder-server/utils/types/deployment"
)

// Package-level variables shared across setup, send, and verify phases.
var (
	db                   *sql.DB
	httpPort             string
	webhookURL           string
	webhook              *whUtil.Recorder
	writeKey             string
	workspaceID          string
	postgresContainer    *pgdocker.Resource
	transformerContainer *transformertest.Resource
)

// expectedEventCount is the total number of individual webhook deliveries we expect.
//
// Breakdown:
//   - JavaScript SDK: 6 individual calls + 6 batch events + 2 beacon events = 14
//   - iOS SDK: 5 individual calls + 2 lifecycle events = 7
//   - Android SDK: 5 individual calls + 2 lifecycle events = 7
//   - Server SDKs: 5 platforms × 6 batch events each = 30
//   - Total: 14 + 7 + 7 + 30 = 58 events
const expectedEventCount = 58

// TestSDKCompatibility is the main entry point for the full-stack SDK
// compatibility integration test. It provisions Docker containers (PostgreSQL,
// Transformer), starts the RudderStack server, sends SDK-specific payloads for
// all platforms, and asserts field-level preservation at the webhook destination.
func TestSDKCompatibility(t *testing.T) {
	t.Log("=== SDK Compatibility Integration Test ===")

	var tearDownStart time.Time
	defer func() {
		if tearDownStart.IsZero() {
			t.Log("--- Teardown done (unexpected)")
		} else {
			t.Logf("--- Teardown done (%s)", time.Since(tearDownStart))
		}
	}()

	svcCtx, svcCancel := context.WithCancel(context.Background())
	svcDone := setupSDKCompatibility(svcCtx, svcCancel, t)

	sendSDKPayloads(t)
	verifySDKCompatibility(t)

	svcCancel()
	t.Log("Waiting for service to stop")
	<-svcDone

	tearDownStart = time.Now()
}

// setupSDKCompatibility provisions Docker containers for PostgreSQL and
// Transformer, configures the RudderStack server with a webhook destination
// that accepts all 6 Segment event types, and starts the server. It returns a
// channel that closes when the server has fully shut down.
func setupSDKCompatibility(svcCtx context.Context, cancel context.CancelFunc, t *testing.T) <-chan struct{} {
	setupStart := time.Now()
	if testing.Verbose() {
		t.Setenv("LOG_LEVEL", "DEBUG")
	}

	config.Reset()
	logger.Reset()

	// Create Docker pool for container orchestration.
	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	// Provision PostgreSQL and Transformer containers in parallel.
	containersGroup, _ := errgroup.WithContext(context.TODO())
	containersGroup.Go(func() (err error) {
		postgresContainer, err = pgdocker.Setup(pool, t)
		if err != nil {
			return err
		}
		db = postgresContainer.DB
		return nil
	})
	containersGroup.Go(func() (err error) {
		transformerContainer, err = transformertest.Setup(pool, t)
		return err
	})
	require.NoError(t, containersGroup.Wait())

	// Load environment file if present (not required).
	if err := godotenv.Load("../../testhelper/.env"); err != nil {
		t.Log("INFO: No .env file found, continuing with defaults.")
	}

	// Configure database connectivity for the RudderStack server.
	t.Setenv("JOBS_DB_HOST", postgresContainer.Host)
	t.Setenv("JOBS_DB_PORT", postgresContainer.Port)
	t.Setenv("JOBS_DB_USER", postgresContainer.User)
	t.Setenv("JOBS_DB_PASSWORD", postgresContainer.Password)
	t.Setenv("JOBS_DB_DB_NAME", postgresContainer.Database)
	t.Setenv("JOBS_DB_SSL_MODE", "disable")
	t.Setenv("WAREHOUSE_JOBS_DB_HOST", postgresContainer.Host)
	t.Setenv("WAREHOUSE_JOBS_DB_PORT", postgresContainer.Port)

	// Configure transformer URL and deployment type.
	t.Setenv("DEST_TRANSFORM_URL", transformerContainer.TransformerURL)
	t.Setenv("DEPLOYMENT_TYPE", string(deployment.DedicatedType))

	// Allocate a free port for the Gateway HTTP server.
	httpPortInt, err := kithelper.GetFreePort()
	require.NoError(t, err)
	httpPort = strconv.Itoa(httpPortInt)
	t.Setenv("RSERVER_GATEWAY_WEB_PORT", httpPort)

	// Disable stats collection during testing.
	t.Setenv("RSERVER_ENABLE_STATS", "false")

	// Create webhook recorder to capture destination-delivered events.
	webhook = whUtil.NewRecorder()
	t.Cleanup(webhook.Close)
	webhookURL = webhook.Server.URL

	// Generate unique identifiers for test isolation.
	writeKey = rand.String(27)
	workspaceID = rand.String(27)

	// Build workspace configuration from template with runtime values.
	mapWorkspaceConfig := map[string]any{
		"webhookUrl":  webhookURL,
		"writeKey":    writeKey,
		"workspaceId": workspaceID,
	}
	t.Logf("workspace config: %v", mapWorkspaceConfig)
	workspaceConfigPath := workspaceConfig.CreateTempFile(t,
		"testdata/workspaceConfigTemplate.json",
		mapWorkspaceConfig,
	)
	if testing.Verbose() {
		data, err := os.ReadFile(workspaceConfigPath)
		require.NoError(t, err)
		t.Logf("Workspace config: %s", string(data))
	}

	t.Log("workspace config path:", workspaceConfigPath)
	t.Setenv("RSERVER_BACKEND_CONFIG_CONFIG_JSONPATH", workspaceConfigPath)
	t.Setenv("RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE", "true")
	t.Setenv("RUDDER_TMPDIR", t.TempDir())

	t.Logf("--- Setup done (%s)", time.Since(setupStart))

	// Start the RudderStack server in a background goroutine.
	svcDone := make(chan struct{})
	go func() {
		r := runner.New(runner.ReleaseInfo{EnterpriseToken: os.Getenv("ENTERPRISE_TOKEN")})
		_ = r.Run(svcCtx, cancel, []string{"sdk-compatibility-test"})
		close(svcDone)
	}()

	// Wait until the Gateway HTTP server is healthy.
	serviceHealthEndpoint := fmt.Sprintf("http://localhost:%s/health", httpPort)
	t.Log("serviceHealthEndpoint", serviceHealthEndpoint)
	health.WaitUntilReady(
		context.Background(), t,
		serviceHealthEndpoint,
		2*time.Minute,
		time.Second,
		"sdkCompatibility",
	)

	return svcDone
}

// ---------------------------------------------------------------------------
// Payload Loading and Sending
// ---------------------------------------------------------------------------

// sendSDKPayloads loads test payloads from JSON fixture files and sends them to
// the Gateway HTTP API, organized by SDK platform.
func sendSDKPayloads(t *testing.T) {
	t.Helper()
	require.Empty(t, webhook.Requests(), "webhook should have no requests before sending events")

	// Load all payload fixture files.
	jsData := loadFixture(t, "testdata/segment_js_payloads.json")
	iosData := loadFixture(t, "testdata/segment_ios_payloads.json")
	androidData := loadFixture(t, "testdata/segment_android_payloads.json")
	serverData := loadFixture(t, "testdata/segment_server_payloads.json")

	// ---------------------------------------------------------------
	// JavaScript SDK (E-006) — 6 individual + batch + beacon
	// ---------------------------------------------------------------
	t.Log("--- Sending JavaScript SDK payloads ---")

	// Individual call types.
	jsCallTypes := []string{"identify", "track", "page", "screen", "group", "alias"}
	for _, ct := range jsCallTypes {
		payload := gjson.GetBytes(jsData, ct).Raw
		require.NotEmpty(t, payload, "JS %s payload missing from fixture", ct)
		sendEvent(t, strings.NewReader(payload), ct, writeKey)
	}

	// Batch endpoint (mixed event types).
	jsBatch := gjson.GetBytes(jsData, "batch").Raw
	require.NotEmpty(t, jsBatch, "JS batch payload missing from fixture")
	sendEvent(t, strings.NewReader(jsBatch), "batch", writeKey)

	// Beacon batch endpoint (sendBeacon — writeKey in query params, text/plain).
	jsBeacon := gjson.GetBytes(jsData, "beacon_batch").Raw
	require.NotEmpty(t, jsBeacon, "JS beacon_batch payload missing from fixture")
	sendBeaconBatch(t, strings.NewReader(jsBeacon), writeKey)

	// ---------------------------------------------------------------
	// iOS SDK (E-007) — 5 individual + 2 lifecycle
	// ---------------------------------------------------------------
	t.Log("--- Sending iOS SDK payloads ---")

	iosCallTypes := []string{"identify", "track", "screen", "group", "alias"}
	for _, ct := range iosCallTypes {
		payload := gjson.GetBytes(iosData, ct).Raw
		require.NotEmpty(t, payload, "iOS %s payload missing from fixture", ct)
		sendEvent(t, strings.NewReader(payload), ct, writeKey)
	}

	// iOS lifecycle events (track type).
	iosLifecycleKeys := []string{"lifecycle_app_opened", "lifecycle_app_backgrounded"}
	for _, key := range iosLifecycleKeys {
		payload := gjson.GetBytes(iosData, key).Raw
		require.NotEmpty(t, payload, "iOS %s payload missing from fixture", key)
		sendEvent(t, strings.NewReader(payload), "track", writeKey)
	}

	// ---------------------------------------------------------------
	// Android SDK (E-007) — 5 individual + 2 lifecycle
	// ---------------------------------------------------------------
	t.Log("--- Sending Android SDK payloads ---")

	androidCallTypes := []string{"identify", "track", "screen", "group", "alias"}
	for _, ct := range androidCallTypes {
		payload := gjson.GetBytes(androidData, ct).Raw
		require.NotEmpty(t, payload, "Android %s payload missing from fixture", ct)
		sendEvent(t, strings.NewReader(payload), ct, writeKey)
	}

	// Android lifecycle events (track type).
	androidLifecycleKeys := []string{"lifecycle_app_opened", "lifecycle_app_backgrounded"}
	for _, key := range androidLifecycleKeys {
		payload := gjson.GetBytes(androidData, key).Raw
		require.NotEmpty(t, payload, "Android %s payload missing from fixture", key)
		sendEvent(t, strings.NewReader(payload), "track", writeKey)
	}

	// ---------------------------------------------------------------
	// Server-Side SDKs (E-008) — 5 platforms × batch
	// ---------------------------------------------------------------
	t.Log("--- Sending Server-Side SDK payloads ---")

	serverPlatforms := []string{"node", "python", "go", "java", "ruby"}
	for _, platform := range serverPlatforms {
		payload := gjson.GetBytes(serverData, platform).Raw
		require.NotEmpty(t, payload, "Server %s payload missing from fixture", platform)
		sendEvent(t, strings.NewReader(payload), "batch", writeKey)
	}
}

// loadFixture reads a JSON fixture file from disk and returns its raw bytes.
func loadFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "failed to load fixture file: %s", path)
	require.NotEmpty(t, data, "fixture file is empty: %s", path)
	return data
}

// sendEvent sends a single event to the Gateway HTTP API using the specified
// call type (identify, track, page, screen, group, alias, batch) with Basic
// Auth credentials derived from the provided write key.
func sendEvent(t *testing.T, payload *strings.Reader, callType, wk string) {
	t.Helper()
	t.Logf("Sending %s Event", callType)

	var (
		httpClient = &http.Client{}
		method     = "POST"
		url        = fmt.Sprintf("http://localhost:%s/v1/%s", httpPort, callType)
	)

	req, err := http.NewRequest(method, url, payload)
	require.NoError(t, err, "failed to create HTTP request for %s event", callType)

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", b64.StdEncoding.EncodeToString(
		fmt.Appendf(nil, "%s:", wk),
	)))

	res, err := httpClient.Do(req)
	require.NoError(t, err, "failed to send %s event", callType)
	defer func() { httputil.CloseResponse(res) }()

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err, "failed to read response body for %s event", callType)
	require.Equal(t, "200 OK", res.Status,
		"expected 200 OK for %s event, got %s: %s", callType, res.Status, string(body))

	t.Logf("Event Sent Successfully: (%s)", body)
}

// sendBeaconBatch sends a beacon batch payload to /beacon/v1/batch with the
// writeKey in query params (not in Authorization header), using text/plain
// Content-Type to match the navigator.sendBeacon() browser API behavior.
func sendBeaconBatch(t *testing.T, payload *strings.Reader, wk string) {
	t.Helper()
	t.Log("Sending beacon batch event")

	httpClient := &http.Client{}
	url := fmt.Sprintf("http://localhost:%s/beacon/v1/batch?writeKey=%s", httpPort, wk)

	req, err := http.NewRequest("POST", url, payload)
	require.NoError(t, err, "failed to create HTTP request for beacon batch")

	// sendBeacon() sends with text/plain content type; the Gateway beacon
	// interceptor reads the writeKey from query params and delegates to the
	// batch handler which parses the body as JSON regardless of content type.
	req.Header.Add("Content-Type", "text/plain")

	res, err := httpClient.Do(req)
	require.NoError(t, err, "failed to send beacon batch event")
	defer func() { httputil.CloseResponse(res) }()

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err, "failed to read response body for beacon batch")
	require.Equal(t, "200 OK", res.Status,
		"expected 200 OK for beacon batch, got %s: %s", res.Status, string(body))

	t.Logf("Beacon batch sent successfully: (%s)", body)
}

// ---------------------------------------------------------------------------
// Verification Subtests
// ---------------------------------------------------------------------------

// verifySDKCompatibility runs all assertion subtests that validate SDK payload
// compatibility through the complete RudderStack pipeline.
func verifySDKCompatibility(t *testing.T) {
	// Wait for all events to arrive at the webhook destination.
	t.Run("webhook-delivery-count", func(t *testing.T) {
		require.Eventually(t, func() bool {
			return webhook.RequestsCount() >= expectedEventCount
		}, 2*time.Minute, 300*time.Millisecond,
			"expected at least %d webhook deliveries, got %d", expectedEventCount, webhook.RequestsCount(),
		)
	})

	// ---------------------------------------------------------------
	// E-005: Gateway API Surface Validation
	// ---------------------------------------------------------------

	t.Run("write-key-basic-auth", func(t *testing.T) {
		// All events were accepted with 200 OK during the send phase.
		// Validate at least one event arrived with correct auth by checking
		// the total delivery count is at or above our expected count.
		require.True(t, webhook.RequestsCount() >= expectedEventCount,
			"write key basic auth: expected at least %d deliveries, got %d",
			expectedEventCount, webhook.RequestsCount())
	})

	t.Run("all-endpoints-accessible", func(t *testing.T) {
		// Verify that events arrived from all endpoint types by checking
		// that at least one event per call type was delivered.
		for _, expected := range []string{"identify", "track", "page", "screen", "group", "alias"} {
			body := findWebhookEventByField(t, "type", expected)
			require.NotEmpty(t, body,
				"event type %q not found in webhook deliveries", expected)
		}
	})

	// ---------------------------------------------------------------
	// E-006: JavaScript SDK Compatibility
	// ---------------------------------------------------------------

	t.Run("js-sdk/identify", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-js-sdk-identify-001")
		require.NotEmpty(t, body, "JS identify event not found in webhook requests")

		require.Equal(t, "identify", gjson.GetBytes(body, "type").Str)
		require.Equal(t, "user-js-sdk-test-001", gjson.GetBytes(body, "userId").Str)
		require.True(t, gjson.GetBytes(body, "traits.email").Exists(), "traits.email missing")
		require.True(t, gjson.GetBytes(body, "traits.firstName").Exists(), "traits.firstName missing")

		// context.library must be analytics.js
		require.Equal(t, "analytics.js", gjson.GetBytes(body, "context.library.name").Str)
		require.True(t, gjson.GetBytes(body, "context.library.version").Exists(), "context.library.version missing")

		// context.page auto-collected fields (JS SDK specific)
		for _, field := range []string{"path", "referrer", "title", "url"} {
			require.True(t, gjson.GetBytes(body, "context.page."+field).Exists(),
				"context.page.%s missing on JS identify", field)
		}
	})

	t.Run("js-sdk/track", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-js-sdk-track-001")
		require.NotEmpty(t, body, "JS track event not found in webhook requests")

		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "event").Exists(), "event name missing")
		require.True(t, gjson.GetBytes(body, "properties").Exists(), "properties missing")
		require.Equal(t, "analytics.js", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("js-sdk/page", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-js-sdk-page-001")
		require.NotEmpty(t, body, "JS page event not found in webhook requests")

		require.Equal(t, "page", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "name").Exists(), "page name missing")
		require.True(t, gjson.GetBytes(body, "properties.title").Exists(), "properties.title missing")
		require.True(t, gjson.GetBytes(body, "properties.url").Exists(), "properties.url missing")
		require.True(t, gjson.GetBytes(body, "properties.path").Exists(), "properties.path missing")
		require.True(t, gjson.GetBytes(body, "properties.referrer").Exists(), "properties.referrer missing")
		require.Equal(t, "analytics.js", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("js-sdk/screen", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-js-sdk-screen-001")
		require.NotEmpty(t, body, "JS screen event not found in webhook requests")

		require.Equal(t, "screen", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "name").Exists(), "screen name missing")
		require.True(t, gjson.GetBytes(body, "properties").Exists(), "properties missing")
		require.Equal(t, "analytics.js", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("js-sdk/group", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-js-sdk-group-001")
		require.NotEmpty(t, body, "JS group event not found in webhook requests")

		require.Equal(t, "group", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "groupId").Exists(), "groupId missing")
		require.True(t, gjson.GetBytes(body, "traits").Exists(), "traits missing")
		require.Equal(t, "analytics.js", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("js-sdk/alias", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-js-sdk-alias-001")
		require.NotEmpty(t, body, "JS alias event not found in webhook requests")

		require.Equal(t, "alias", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "previousId").Exists(), "previousId missing")
		require.Equal(t, "analytics.js", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("js-sdk/batch", func(t *testing.T) {
		// Verify that all 6 individual events from the batch were split and delivered.
		batchMessageIDs := []string{
			"msg-js-sdk-batch-identify-001",
			"msg-js-sdk-batch-track-001",
			"msg-js-sdk-batch-page-001",
			"msg-js-sdk-batch-screen-001",
			"msg-js-sdk-batch-group-001",
			"msg-js-sdk-batch-alias-001",
		}
		for _, msgID := range batchMessageIDs {
			body := findWebhookEvent(t, msgID)
			require.NotEmpty(t, body, "JS batch event %s not found in webhook requests", msgID)
		}
	})

	t.Run("js-sdk/beacon", func(t *testing.T) {
		// Verify beacon batch events were accepted and delivered.
		beaconMessageIDs := []string{
			"msg-js-sdk-beacon-track-001",
			"msg-js-sdk-beacon-page-001",
		}
		for _, msgID := range beaconMessageIDs {
			body := findWebhookEvent(t, msgID)
			require.NotEmpty(t, body, "JS beacon event %s not found in webhook requests", msgID)
		}
	})

	// ---------------------------------------------------------------
	// E-007: iOS SDK Compatibility
	// ---------------------------------------------------------------

	t.Run("ios-sdk/identify", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-ios-sdk-identify-001")
		require.NotEmpty(t, body, "iOS identify event not found in webhook requests")

		require.Equal(t, "identify", gjson.GetBytes(body, "type").Str)
		require.Equal(t, "analytics-ios", gjson.GetBytes(body, "context.library.name").Str)
		require.True(t, gjson.GetBytes(body, "context.library.version").Exists(), "context.library.version missing")
		require.True(t, gjson.GetBytes(body, "traits.email").Exists(), "traits.email missing")
	})

	t.Run("ios-sdk/track", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-ios-sdk-track-001")
		require.NotEmpty(t, body, "iOS track event not found in webhook requests")

		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "event").Exists(), "event name missing")
		require.True(t, gjson.GetBytes(body, "properties").Exists(), "properties missing")
		require.Equal(t, "analytics-ios", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("ios-sdk/screen", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-ios-sdk-screen-001")
		require.NotEmpty(t, body, "iOS screen event not found in webhook requests")

		require.Equal(t, "screen", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "name").Exists(), "screen name missing")
		require.True(t, gjson.GetBytes(body, "properties").Exists(), "properties missing")
		require.Equal(t, "analytics-ios", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("ios-sdk/lifecycle-app-opened", func(t *testing.T) {
		body := findWebhookEventByLibraryAndName(t, "analytics-ios", "Application Opened")
		require.NotEmpty(t, body, "iOS Application Opened event not found in webhook requests")

		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)
		require.Equal(t, "Application Opened", gjson.GetBytes(body, "event").Str)
		require.True(t, gjson.GetBytes(body, "properties.version").Exists(), "properties.version missing")
		require.Equal(t, "analytics-ios", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("ios-sdk/lifecycle-app-backgrounded", func(t *testing.T) {
		body := findWebhookEventByLibraryAndName(t, "analytics-ios", "Application Backgrounded")
		require.NotEmpty(t, body, "iOS Application Backgrounded event not found in webhook requests")

		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)
		require.Equal(t, "Application Backgrounded", gjson.GetBytes(body, "event").Str)
		require.Equal(t, "analytics-ios", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("ios-sdk/context-device", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-ios-sdk-identify-001")
		require.NotEmpty(t, body, "iOS identify event not found for device context check")

		for _, field := range []string{"id", "manufacturer", "model", "name", "type"} {
			require.True(t, gjson.GetBytes(body, "context.device."+field).Exists(),
				"context.device.%s missing on iOS event", field)
		}
		require.Equal(t, "Apple", gjson.GetBytes(body, "context.device.manufacturer").Str)
		require.Equal(t, "ios", gjson.GetBytes(body, "context.device.type").Str)
	})

	t.Run("ios-sdk/context-os", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-ios-sdk-identify-001")
		require.NotEmpty(t, body, "iOS identify event not found for OS context check")

		require.Equal(t, "iOS", gjson.GetBytes(body, "context.os.name").Str)
		require.True(t, gjson.GetBytes(body, "context.os.version").Exists(), "context.os.version missing")
	})

	t.Run("ios-sdk/context-app", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-ios-sdk-identify-001")
		require.NotEmpty(t, body, "iOS identify event not found for app context check")

		for _, field := range []string{"name", "version", "build", "namespace"} {
			require.True(t, gjson.GetBytes(body, "context.app."+field).Exists(),
				"context.app.%s missing on iOS event", field)
		}
	})

	t.Run("ios-sdk/context-network", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-ios-sdk-identify-001")
		require.NotEmpty(t, body, "iOS identify event not found for network context check")

		for _, field := range []string{"carrier", "cellular", "wifi"} {
			require.True(t, gjson.GetBytes(body, "context.network."+field).Exists(),
				"context.network.%s missing on iOS event", field)
		}
	})

	t.Run("ios-sdk/context-screen", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-ios-sdk-identify-001")
		require.NotEmpty(t, body, "iOS identify event not found for screen context check")

		for _, field := range []string{"height", "width"} {
			require.True(t, gjson.GetBytes(body, "context.screen."+field).Exists(),
				"context.screen.%s missing on iOS event", field)
		}
	})

	// ---------------------------------------------------------------
	// E-007: Android SDK Compatibility
	// ---------------------------------------------------------------

	t.Run("android-sdk/identify", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-android-sdk-identify-001")
		require.NotEmpty(t, body, "Android identify event not found in webhook requests")

		require.Equal(t, "identify", gjson.GetBytes(body, "type").Str)
		require.Equal(t, "analytics-android", gjson.GetBytes(body, "context.library.name").Str)
		require.True(t, gjson.GetBytes(body, "context.library.version").Exists(), "context.library.version missing")
		require.True(t, gjson.GetBytes(body, "traits.email").Exists(), "traits.email missing")
	})

	t.Run("android-sdk/track", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-android-sdk-track-001")
		require.NotEmpty(t, body, "Android track event not found in webhook requests")

		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "event").Exists(), "event name missing")
		require.True(t, gjson.GetBytes(body, "properties").Exists(), "properties missing")
		require.Equal(t, "analytics-android", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("android-sdk/screen", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-android-sdk-screen-001")
		require.NotEmpty(t, body, "Android screen event not found in webhook requests")

		require.Equal(t, "screen", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "name").Exists(), "screen name missing")
		require.True(t, gjson.GetBytes(body, "properties").Exists(), "properties missing")
		require.Equal(t, "analytics-android", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("android-sdk/lifecycle-app-opened", func(t *testing.T) {
		body := findWebhookEventByLibraryAndName(t, "analytics-android", "Application Opened")
		require.NotEmpty(t, body, "Android Application Opened event not found in webhook requests")

		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)
		require.Equal(t, "Application Opened", gjson.GetBytes(body, "event").Str)
		require.True(t, gjson.GetBytes(body, "properties.version").Exists(), "properties.version missing")
		require.Equal(t, "analytics-android", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("android-sdk/lifecycle-app-backgrounded", func(t *testing.T) {
		body := findWebhookEventByLibraryAndName(t, "analytics-android", "Application Backgrounded")
		require.NotEmpty(t, body, "Android Application Backgrounded event not found in webhook requests")

		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)
		require.Equal(t, "Application Backgrounded", gjson.GetBytes(body, "event").Str)
		require.Equal(t, "analytics-android", gjson.GetBytes(body, "context.library.name").Str)
	})

	t.Run("android-sdk/context-device", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-android-sdk-identify-001")
		require.NotEmpty(t, body, "Android identify event not found for device context check")

		for _, field := range []string{"id", "manufacturer", "model", "name", "type"} {
			require.True(t, gjson.GetBytes(body, "context.device."+field).Exists(),
				"context.device.%s missing on Android event", field)
		}
		require.Equal(t, "Samsung", gjson.GetBytes(body, "context.device.manufacturer").Str)
		require.Equal(t, "android", gjson.GetBytes(body, "context.device.type").Str)
	})

	t.Run("android-sdk/context-os", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-android-sdk-identify-001")
		require.NotEmpty(t, body, "Android identify event not found for OS context check")

		require.Equal(t, "Android", gjson.GetBytes(body, "context.os.name").Str)
		require.True(t, gjson.GetBytes(body, "context.os.version").Exists(), "context.os.version missing")
	})

	// ---------------------------------------------------------------
	// E-008: Server-Side SDK Compatibility
	// ---------------------------------------------------------------

	// serverSDKs maps platform name to library name and a representative messageId.
	serverSDKs := []struct {
		platform    string
		libraryName string
		identifyID  string
		trackID     string
	}{
		{"node", "analytics-node", "msg-node-sdk-identify-001", "msg-node-sdk-track-001"},
		{"python", "analytics-python", "msg-python-sdk-identify-001", "msg-python-sdk-track-001"},
		{"go", "analytics-go", "msg-go-sdk-identify-001", "msg-go-sdk-track-001"},
		{"java", "analytics-java", "msg-java-sdk-identify-001", "msg-java-sdk-track-001"},
		{"ruby", "analytics-ruby", "msg-ruby-sdk-identify-001", "msg-ruby-sdk-track-001"},
	}

	for _, sdk := range serverSDKs {
		sdk := sdk // capture range variable

		t.Run(sdk.platform+"-sdk/batch-delivery", func(t *testing.T) {
			// Verify at least one event from this platform's batch was delivered.
			body := findWebhookEvent(t, sdk.identifyID)
			require.NotEmpty(t, body,
				"%s batch identify event not found in webhook requests", sdk.platform)
		})

		t.Run(sdk.platform+"-sdk/context-library", func(t *testing.T) {
			body := findWebhookEvent(t, sdk.identifyID)
			require.NotEmpty(t, body,
				"%s identify event not found for library check", sdk.platform)
			require.Equal(t, sdk.libraryName, gjson.GetBytes(body, "context.library.name").Str,
				"%s identify should have context.library.name=%s", sdk.platform, sdk.libraryName)
		})

		t.Run(sdk.platform+"-sdk/mixed-event-types", func(t *testing.T) {
			// Verify that multiple event types from this platform's batch were delivered.
			batchMsgIDs := []string{
				fmt.Sprintf("msg-%s-sdk-identify-001", sdk.platform),
				fmt.Sprintf("msg-%s-sdk-track-001", sdk.platform),
				fmt.Sprintf("msg-%s-sdk-page-001", sdk.platform),
				fmt.Sprintf("msg-%s-sdk-screen-001", sdk.platform),
				fmt.Sprintf("msg-%s-sdk-group-001", sdk.platform),
				fmt.Sprintf("msg-%s-sdk-alias-001", sdk.platform),
			}
			deliveredTypes := map[string]bool{}
			for _, msgID := range batchMsgIDs {
				body := findWebhookEvent(t, msgID)
				if len(body) > 0 {
					eType := gjson.GetBytes(body, "type").Str
					deliveredTypes[eType] = true
				}
			}
			for _, expected := range []string{"identify", "track", "page", "screen", "group", "alias"} {
				require.True(t, deliveredTypes[expected],
					"%s batch: event type %q not found in deliveries", sdk.platform, expected)
			}
		})
	}

	// ---------------------------------------------------------------
	// Cross-SDK Context Field Preservation
	// ---------------------------------------------------------------

	t.Run("context-field-preservation", func(t *testing.T) {
		// Verify representative events from different SDKs preserve key context fields.
		representativeEvents := []struct {
			name      string
			messageID string
			fields    []string
		}{
			{
				name:      "JS SDK (full browser context)",
				messageID: "msg-js-sdk-identify-001",
				fields: []string{
					"context.library.name",
					"context.library.version",
					"context.page.path",
					"context.page.url",
					"context.userAgent",
					"context.locale",
				},
			},
			{
				name:      "iOS SDK (mobile context)",
				messageID: "msg-ios-sdk-identify-001",
				fields: []string{
					"context.library.name",
					"context.library.version",
					"context.device.id",
					"context.device.manufacturer",
					"context.device.model",
					"context.os.name",
					"context.os.version",
					"context.app.name",
					"context.app.version",
					"context.network.carrier",
					"context.screen.height",
					"context.screen.width",
				},
			},
			{
				name:      "Android SDK (mobile context)",
				messageID: "msg-android-sdk-identify-001",
				fields: []string{
					"context.library.name",
					"context.library.version",
					"context.device.id",
					"context.device.manufacturer",
					"context.device.model",
					"context.os.name",
					"context.os.version",
					"context.app.name",
					"context.app.version",
					"context.network.carrier",
					"context.screen.height",
					"context.screen.width",
				},
			},
			{
				name:      "Node.js SDK (server context)",
				messageID: "msg-node-sdk-identify-001",
				fields: []string{
					"context.library.name",
					"context.library.version",
				},
			},
		}

		for _, re := range representativeEvents {
			re := re // capture range variable
			t.Run(re.name, func(t *testing.T) {
				body := findWebhookEvent(t, re.messageID)
				require.NotEmpty(t, body, "%s event %s not found in webhook requests",
					re.name, re.messageID)

				for _, field := range re.fields {
					require.True(t, gjson.GetBytes(body, field).Exists(),
						"%s: context field %q missing", re.name, field)
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Webhook Event Finder Helpers
// ---------------------------------------------------------------------------

// findWebhookEvent scans captured webhook requests and returns the raw body
// bytes of the first request matching the specified messageId. Using messageId
// for lookup ensures deterministic matching when multiple events of the same
// type are delivered.
func findWebhookEvent(t *testing.T, messageID string) []byte {
	t.Helper()
	for _, req := range webhook.Requests() {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			continue
		}
		if gjson.GetBytes(body, "messageId").Str == messageID {
			return body
		}
	}
	return nil
}

// findWebhookEventByField scans captured webhook requests and returns the raw
// body bytes of the first request where the given JSON path equals the given
// value. Useful for locating events by event name, type, or any single field.
func findWebhookEventByField(t *testing.T, jsonPath, expected string) []byte {
	t.Helper()
	for _, req := range webhook.Requests() {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			continue
		}
		if gjson.GetBytes(body, jsonPath).Str == expected {
			return body
		}
	}
	return nil
}

// findWebhookEventByLibraryAndName scans captured webhook requests and returns
// the raw body bytes of the first event matching both the library name and the
// event name. This helper is needed to disambiguate lifecycle events (e.g.,
// "Application Opened") that are sent by both iOS and Android SDKs.
func findWebhookEventByLibraryAndName(t *testing.T, libraryName, eventName string) []byte {
	t.Helper()
	for _, req := range webhook.Requests() {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			continue
		}
		if gjson.GetBytes(body, "context.library.name").Str == libraryName &&
			gjson.GetBytes(body, "event").Str == eventName {
			return body
		}
	}
	return nil
}
