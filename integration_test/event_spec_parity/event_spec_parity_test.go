// Package event_spec_parity implements end-to-end integration tests that validate
// Segment Event Spec parity across the complete RudderStack pipeline:
// Gateway → Processor → Router → Webhook destination.
//
// It exercises all 6 Segment Spec event types (identify, track, page, screen,
// group, alias) and validates:
//   - Client Hints pass-through (ES-001)
//   - Semantic event routing (ES-002)
//   - Reserved trait handling (ES-003)
//   - Channel field behavior (ES-007)
//   - Field-level preservation for all common fields (E-001, E-003)
package event_spec_parity

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
// 9 individual events + 2 from the batch = 11 total events routed to the webhook.
const expectedEventCount = 11

// TestEventSpecParity is the main entry point for the full-stack Segment Event Spec
// parity integration test. It provisions Docker containers (PostgreSQL, Transformer),
// starts the RudderStack server, sends all 6 event types with complete Segment Spec
// payloads, and asserts field-level preservation at the webhook destination.
func TestEventSpecParity(t *testing.T) {
	t.Log("=== Event Spec Parity Integration Test ===")

	var tearDownStart time.Time
	defer func() {
		if tearDownStart.IsZero() {
			t.Log("--- Teardown done (unexpected)")
		} else {
			t.Logf("--- Teardown done (%s)", time.Since(tearDownStart))
		}
	}()

	svcCtx, svcCancel := context.WithCancel(context.Background())
	svcDone := setupEventSpecParity(svcCtx, svcCancel, t)

	sendSegmentSpecEvents(t)
	verifyParity(t)

	svcCancel()
	t.Log("Waiting for service to stop")
	<-svcDone

	tearDownStart = time.Now()
}

// setupEventSpecParity provisions Docker containers for PostgreSQL and Transformer,
// configures the RudderStack server with a webhook destination that accepts all 6
// Segment event types, and starts the server. It returns a channel that closes when
// the server has fully shut down.
func setupEventSpecParity(svcCtx context.Context, cancel context.CancelFunc, t *testing.T) <-chan struct{} {
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
	t.Setenv("RUDDER_TMPDIR", t.TempDir())

	t.Logf("--- Setup done (%s)", time.Since(setupStart))

	// Start the RudderStack server in a background goroutine.
	svcDone := make(chan struct{})
	go func() {
		r := runner.New(runner.ReleaseInfo{EnterpriseToken: os.Getenv("ENTERPRISE_TOKEN")})
		_ = r.Run(svcCtx, cancel, []string{"event-spec-parity-test"})
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
		"eventSpecParity",
	)

	return svcDone
}

// sendSegmentSpecEvents sends all 6 Segment Spec core event types plus semantic
// events (E-Commerce v2, Video, Mobile lifecycle) to the Gateway HTTP API. Each
// payload includes the full set of Segment Spec fields: common fields, context
// with all 18 standard sub-fields, Client Hints (userAgentData), channel field,
// integrations object, and event-type-specific fields with reserved traits.
func sendSegmentSpecEvents(t *testing.T) {
	t.Helper()
	require.Empty(t, webhook.Requests(), "webhook should have no requests before sending events")

	// 1. Identify event — all 17 reserved identify traits + Client Hints + channel
	sendEvent(t, strings.NewReader(identifyPayload), "identify", writeKey)

	// 2. Track — Order Completed (E-Commerce v2 semantic event)
	sendEvent(t, strings.NewReader(trackOrderCompletedPayload), "track", writeKey)

	// 3. Track — Product Viewed (E-Commerce v2 semantic event)
	sendEvent(t, strings.NewReader(trackProductViewedPayload), "track", writeKey)

	// 4. Track — Video Playback Started (Video semantic event)
	sendEvent(t, strings.NewReader(trackVideoPlaybackStartedPayload), "track", writeKey)

	// 5. Track — Application Opened (Mobile lifecycle semantic event)
	sendEvent(t, strings.NewReader(trackApplicationOpenedPayload), "track", writeKey)

	// 6. Page event — with full page properties
	sendEvent(t, strings.NewReader(pagePayload), "page", writeKey)

	// 7. Screen event — with mobile context
	sendEvent(t, strings.NewReader(screenPayload), "screen", writeKey)

	// 8. Group event — all 12 reserved group traits
	sendEvent(t, strings.NewReader(groupPayload), "group", writeKey)

	// 9. Alias event — userId + previousId
	sendEvent(t, strings.NewReader(aliasPayload), "alias", writeKey)

	// 10. Batch event — containing identify + track to exercise /v1/batch endpoint
	sendEvent(t, strings.NewReader(batchPayload), "batch", writeKey)
}

// verifyParity runs all assertion subtests that validate field-level preservation
// of Segment Spec events through the complete RudderStack pipeline.
func verifyParity(t *testing.T) {
	// Wait for all events to arrive at the webhook destination.
	t.Run("webhook-delivery-count", func(t *testing.T) {
		require.Eventually(t, func() bool {
			return webhook.RequestsCount() >= expectedEventCount
		}, 2*time.Minute, 300*time.Millisecond,
			"expected at least %d webhook deliveries", expectedEventCount,
		)
	})

	t.Run("identify-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-identify-parity-001")
		require.NotEmpty(t, body, "identify event not found in webhook requests")

		// Common fields
		require.True(t, gjson.GetBytes(body, "anonymousId").Exists(), "anonymousId missing")
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "messageId").Exists(), "messageId missing")
		require.Equal(t, "identify", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "timestamp").Exists(), "timestamp missing")
		require.True(t, gjson.GetBytes(body, "sentAt").Exists(), "sentAt missing")
		require.True(t, gjson.GetBytes(body, "originalTimestamp").Exists(), "originalTimestamp missing")

		// All 17 reserved identify traits
		reservedIdentifyTraits := []string{
			"address", "age", "avatar", "birthday", "company", "createdAt",
			"description", "email", "firstName", "gender", "id", "lastName",
			"name", "phone", "title", "username", "website",
		}
		for _, trait := range reservedIdentifyTraits {
			path := "traits." + trait
			require.True(t, gjson.GetBytes(body, path).Exists(),
				"reserved identify trait %q missing at path %q", trait, path)
		}

		// Context standard fields
		require.True(t, gjson.GetBytes(body, "context").Exists(), "context missing")
		require.True(t, gjson.GetBytes(body, "context.library.name").Exists(), "context.library.name missing")
		require.True(t, gjson.GetBytes(body, "context.library.version").Exists(), "context.library.version missing")

		// Client Hints (userAgentData)
		require.True(t, gjson.GetBytes(body, "context.userAgentData").Exists(), "context.userAgentData missing")
		require.True(t, gjson.GetBytes(body, "context.userAgentData.brands").IsArray(), "context.userAgentData.brands should be an array")
		require.True(t, gjson.GetBytes(body, "context.userAgentData.platform").Exists(), "context.userAgentData.platform missing")

		// Channel field
		require.True(t, gjson.GetBytes(body, "context.channel").Exists(), "context.channel missing")
		require.Equal(t, "server", gjson.GetBytes(body, "context.channel").Str)

		// Integrations
		require.True(t, gjson.GetBytes(body, "integrations.All").Bool(), "integrations.All should be true")
	})

	t.Run("track-ecommerce-semantic", func(t *testing.T) {
		body := findWebhookEventByName(t, "Order Completed")
		require.NotEmpty(t, body, "Order Completed track event not found in webhook requests")

		require.Equal(t, "Order Completed", gjson.GetBytes(body, "event").Str)
		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)

		// E-Commerce v2 properties
		require.True(t, gjson.GetBytes(body, "properties.orderId").Exists(), "properties.orderId missing")
		require.True(t, gjson.GetBytes(body, "properties.total").Exists(), "properties.total missing")
		require.True(t, gjson.GetBytes(body, "properties.revenue").Exists(), "properties.revenue missing")
		require.True(t, gjson.GetBytes(body, "properties.products").IsArray(), "properties.products should be an array")
		require.True(t, gjson.GetBytes(body, "properties.currency").Exists(), "properties.currency missing")
		require.True(t, gjson.GetBytes(body, "properties.shipping").Exists(), "properties.shipping missing")
		require.True(t, gjson.GetBytes(body, "properties.tax").Exists(), "properties.tax missing")

		// Context should be present
		require.True(t, gjson.GetBytes(body, "context.channel").Exists(), "context.channel missing")
	})

	t.Run("track-product-viewed-semantic", func(t *testing.T) {
		body := findWebhookEventByName(t, "Product Viewed")
		require.NotEmpty(t, body, "Product Viewed track event not found in webhook requests")

		require.Equal(t, "Product Viewed", gjson.GetBytes(body, "event").Str)
		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)

		// Product Viewed properties
		require.True(t, gjson.GetBytes(body, "properties.product_id").Exists(), "properties.product_id missing")
		require.True(t, gjson.GetBytes(body, "properties.sku").Exists(), "properties.sku missing")
		require.True(t, gjson.GetBytes(body, "properties.category").Exists(), "properties.category missing")
		require.True(t, gjson.GetBytes(body, "properties.name").Exists(), "properties.name missing")
		require.True(t, gjson.GetBytes(body, "properties.price").Exists(), "properties.price missing")
	})

	t.Run("track-video-semantic", func(t *testing.T) {
		body := findWebhookEventByName(t, "Video Playback Started")
		require.NotEmpty(t, body, "Video Playback Started track event not found in webhook requests")

		require.Equal(t, "Video Playback Started", gjson.GetBytes(body, "event").Str)
		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)

		// Video semantic properties
		require.True(t, gjson.GetBytes(body, "properties.session_id").Exists(), "properties.session_id missing")
		require.True(t, gjson.GetBytes(body, "properties.content_asset_id").Exists(), "properties.content_asset_id missing")
		require.True(t, gjson.GetBytes(body, "properties.video_player").Exists(), "properties.video_player missing")
		require.True(t, gjson.GetBytes(body, "properties.total_length").Exists(), "properties.total_length missing")
		require.True(t, gjson.GetBytes(body, "properties.full_screen").Exists(), "properties.full_screen missing")
	})

	t.Run("track-mobile-lifecycle", func(t *testing.T) {
		body := findWebhookEventByName(t, "Application Opened")
		require.NotEmpty(t, body, "Application Opened track event not found in webhook requests")

		require.Equal(t, "Application Opened", gjson.GetBytes(body, "event").Str)
		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)

		// Mobile lifecycle properties
		require.True(t, gjson.GetBytes(body, "properties.version").Exists(), "properties.version missing")

		// Channel field for mobile
		require.True(t, gjson.GetBytes(body, "context.channel").Exists(), "context.channel missing")
		require.Equal(t, "mobile", gjson.GetBytes(body, "context.channel").Str)
	})

	t.Run("page-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-page-parity-001")
		require.NotEmpty(t, body, "page event not found in webhook requests")

		require.Equal(t, "page", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "name").Exists(), "name missing")
		require.True(t, gjson.GetBytes(body, "properties.title").Exists(), "properties.title missing")
		require.True(t, gjson.GetBytes(body, "properties.url").Exists(), "properties.url missing")
		require.True(t, gjson.GetBytes(body, "properties.path").Exists(), "properties.path missing")
		require.True(t, gjson.GetBytes(body, "properties.referrer").Exists(), "properties.referrer missing")

		// Common fields
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "anonymousId").Exists(), "anonymousId missing")
		require.True(t, gjson.GetBytes(body, "messageId").Exists(), "messageId missing")

		// Channel field for browser
		require.True(t, gjson.GetBytes(body, "context.channel").Exists(), "context.channel missing")
		require.Equal(t, "browser", gjson.GetBytes(body, "context.channel").Str)
	})

	t.Run("screen-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-screen-parity-001")
		require.NotEmpty(t, body, "screen event not found in webhook requests")

		require.Equal(t, "screen", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "name").Exists(), "name missing")
		require.True(t, gjson.GetBytes(body, "properties").Exists(), "properties missing")

		// Common fields
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "anonymousId").Exists(), "anonymousId missing")
		require.True(t, gjson.GetBytes(body, "messageId").Exists(), "messageId missing")

		// Channel field for mobile
		require.True(t, gjson.GetBytes(body, "context.channel").Exists(), "context.channel missing")
		require.Equal(t, "mobile", gjson.GetBytes(body, "context.channel").Str)
	})

	t.Run("group-reserved-traits", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-group-parity-001")
		require.NotEmpty(t, body, "group event not found in webhook requests")

		require.Equal(t, "group", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "groupId").Exists(), "groupId missing")

		// All 12 reserved group traits
		reservedGroupTraits := []string{
			"address", "avatar", "createdAt", "description", "email",
			"employees", "id", "industry", "name", "phone", "website", "plan",
		}
		for _, trait := range reservedGroupTraits {
			path := "traits." + trait
			require.True(t, gjson.GetBytes(body, path).Exists(),
				"reserved group trait %q missing at path %q", trait, path)
		}

		// Common fields
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "anonymousId").Exists(), "anonymousId missing")
	})

	t.Run("alias-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-alias-parity-001")
		require.NotEmpty(t, body, "alias event not found in webhook requests")

		require.Equal(t, "alias", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "previousId").Exists(), "previousId missing")
		require.True(t, gjson.GetBytes(body, "messageId").Exists(), "messageId missing")

		// Channel field for server
		require.True(t, gjson.GetBytes(body, "context.channel").Exists(), "context.channel missing")
		require.Equal(t, "server", gjson.GetBytes(body, "context.channel").Str)
	})

	t.Run("client-hints-passthrough", func(t *testing.T) {
		// Verify Client Hints pass-through on the identify event which
		// includes the full userAgentData object.
		body := findWebhookEvent(t, "msg-identify-parity-001")
		require.NotEmpty(t, body, "identify event not found for Client Hints check")

		uad := gjson.GetBytes(body, "context.userAgentData")
		require.True(t, uad.Exists(), "context.userAgentData missing")

		// Brands array
		brands := gjson.GetBytes(body, "context.userAgentData.brands")
		require.True(t, brands.IsArray(), "context.userAgentData.brands should be array")
		require.Greater(t, len(brands.Array()), 0, "context.userAgentData.brands should have at least one entry")

		// Verify first brand has brand and version fields
		firstBrand := brands.Array()[0]
		require.True(t, firstBrand.Get("brand").Exists(), "brand field missing in first brand entry")
		require.True(t, firstBrand.Get("version").Exists(), "version field missing in first brand entry")

		// Mobile field (boolean)
		require.True(t, gjson.GetBytes(body, "context.userAgentData.mobile").Exists(), "context.userAgentData.mobile missing")

		// Platform field (string)
		platform := gjson.GetBytes(body, "context.userAgentData.platform")
		require.True(t, platform.Exists(), "context.userAgentData.platform missing")
		require.NotEmpty(t, platform.Str, "context.userAgentData.platform should be non-empty string")

		// Optional high-entropy fields preserved if sent
		require.True(t, gjson.GetBytes(body, "context.userAgentData.bitness").Exists(), "context.userAgentData.bitness missing")
		require.True(t, gjson.GetBytes(body, "context.userAgentData.model").Exists(), "context.userAgentData.model missing")
		require.True(t, gjson.GetBytes(body, "context.userAgentData.platformVersion").Exists(), "context.userAgentData.platformVersion missing")
		require.True(t, gjson.GetBytes(body, "context.userAgentData.uaFullVersion").Exists(), "context.userAgentData.uaFullVersion missing")
		require.True(t, gjson.GetBytes(body, "context.userAgentData.fullVersionList").IsArray(), "context.userAgentData.fullVersionList should be array")
		require.True(t, gjson.GetBytes(body, "context.userAgentData.wow64").Exists(), "context.userAgentData.wow64 missing")
	})

	t.Run("channel-field-values", func(t *testing.T) {
		// Verify that different channel values propagate correctly per event type.

		// Identify event → context.channel: "server"
		identifyBody := findWebhookEvent(t, "msg-identify-parity-001")
		require.NotEmpty(t, identifyBody, "identify event not found for channel check")
		require.Equal(t, "server", gjson.GetBytes(identifyBody, "context.channel").Str,
			"identify event should have context.channel=server")

		// Page event → context.channel: "browser"
		pageBody := findWebhookEvent(t, "msg-page-parity-001")
		require.NotEmpty(t, pageBody, "page event not found for channel check")
		require.Equal(t, "browser", gjson.GetBytes(pageBody, "context.channel").Str,
			"page event should have context.channel=browser")

		// Screen event → context.channel: "mobile"
		screenBody := findWebhookEvent(t, "msg-screen-parity-001")
		require.NotEmpty(t, screenBody, "screen event not found for channel check")
		require.Equal(t, "mobile", gjson.GetBytes(screenBody, "context.channel").Str,
			"screen event should have context.channel=mobile")

		// Alias event → context.channel: "server"
		aliasBody := findWebhookEvent(t, "msg-alias-parity-001")
		require.NotEmpty(t, aliasBody, "alias event not found for channel check")
		require.Equal(t, "server", gjson.GetBytes(aliasBody, "context.channel").Str,
			"alias event should have context.channel=server")
	})

	t.Run("context-standard-fields", func(t *testing.T) {
		// Verify all 18 standard context sub-fields are preserved on the identify event.
		body := findWebhookEvent(t, "msg-identify-parity-001")
		require.NotEmpty(t, body, "identify event not found for context fields check")

		contextPaths := []string{
			"context.app.name",
			"context.app.version",
			"context.app.build",
			"context.app.namespace",
			"context.campaign.name",
			"context.campaign.source",
			"context.campaign.medium",
			"context.campaign.term",
			"context.campaign.content",
			"context.device.id",
			"context.device.manufacturer",
			"context.device.model",
			"context.device.name",
			"context.device.type",
			"context.library.name",
			"context.library.version",
			"context.locale",
			"context.network.carrier",
			"context.network.cellular",
			"context.network.wifi",
			"context.os.name",
			"context.os.version",
			"context.page.path",
			"context.page.referrer",
			"context.page.title",
			"context.page.url",
			"context.referrer.type",
			"context.referrer.name",
			"context.referrer.url",
			"context.screen.width",
			"context.screen.height",
			"context.screen.density",
			"context.timezone",
			"context.userAgent",
			"context.userAgentData",
			"context.channel",
		}
		for _, p := range contextPaths {
			require.True(t, gjson.GetBytes(body, p).Exists(),
				"context field %q missing", p)
		}
	})
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

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

// findWebhookEvent scans captured webhook requests and returns the raw body
// bytes of the first request matching the specified messageId. Using messageId
// for lookup ensures deterministic matching when multiple events of the same
// type are delivered (e.g., individual + batch identify events).
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

// findWebhookEventByName scans captured webhook requests and returns the raw
// body bytes of the first track event matching the specified event name (e.g.,
// "Order Completed", "Video Playback Started").
func findWebhookEventByName(t *testing.T, eventName string) []byte {
	t.Helper()
	for _, req := range webhook.Requests() {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			continue
		}
		if gjson.GetBytes(body, "event").Str == eventName {
			return body
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Payload constants — all use synthetic test data only (RFC 5737 IPs, @example.com emails, 555 phone numbers).
// ---------------------------------------------------------------------------

// identifyPayload contains a complete Segment Spec identify event with all 17
// reserved identify traits, full context object including Client Hints
// (userAgentData), and channel field set to "server".
const identifyPayload = `{
	"type": "identify",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"messageId": "msg-identify-parity-001",
	"timestamp": "2025-01-15T10:30:00.000Z",
	"sentAt": "2025-01-15T10:30:01.000Z",
	"originalTimestamp": "2025-01-15T10:30:00.000Z",
	"version": 1,
	"integrations": {
		"All": true,
		"Mixpanel": false,
		"Salesforce": false
	},
	"context": {
		"active": true,
		"app": {
			"name": "TestApp",
			"version": "1.5.0",
			"build": "250",
			"namespace": "com.test.app"
		},
		"campaign": {
			"name": "Parity Sprint",
			"source": "google",
			"medium": "cpc",
			"term": "event spec",
			"content": "banner-42"
		},
		"device": {
			"id": "device-test-001",
			"advertisingId": "adid-test-001",
			"manufacturer": "TestCorp",
			"model": "TestPhone X",
			"name": "test-device",
			"type": "android",
			"token": "push-token-test-001"
		},
		"ip": "198.51.100.42",
		"library": {
			"name": "analytics.js",
			"version": "3.12.0"
		},
		"locale": "en-US",
		"network": {
			"bluetooth": false,
			"carrier": "T-Mobile",
			"cellular": true,
			"wifi": true
		},
		"os": {
			"name": "Android",
			"version": "14.0"
		},
		"page": {
			"path": "/academy",
			"referrer": "https://example.com/pricing",
			"search": "?plan=enterprise",
			"title": "Academy",
			"url": "https://example.com/academy"
		},
		"referrer": {
			"type": "search",
			"name": "google",
			"url": "https://www.google.com/search?q=rudderstack",
			"link": "https://www.google.com"
		},
		"screen": {
			"width": 1920,
			"height": 1080,
			"density": 2.0,
			"innerWidth": 1920,
			"innerHeight": 969
		},
		"timezone": "America/Los_Angeles",
		"groupId": "grp-parity-test-001",
		"traits": {
			"email": "ctx.user@example.com",
			"name": "Context User"
		},
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"userAgentData": {
			"brands": [
				{"brand": "Chromium", "version": "120"},
				{"brand": "Google Chrome", "version": "120"},
				{"brand": "Not_A Brand", "version": "8"}
			],
			"mobile": true,
			"platform": "Android",
			"bitness": "64",
			"model": "TestPhone X",
			"platformVersion": "14.0.0",
			"uaFullVersion": "120.0.6099.230",
			"fullVersionList": [
				{"brand": "Chromium", "version": "120.0.6099.230"},
				{"brand": "Google Chrome", "version": "120.0.6099.230"},
				{"brand": "Not_A Brand", "version": "8.0.0.0"}
			],
			"wow64": false
		},
		"channel": "server"
	},
	"traits": {
		"address": {
			"street": "123 Test Lane",
			"city": "San Francisco",
			"state": "CA",
			"postalCode": "94105",
			"country": "US"
		},
		"age": 32,
		"avatar": "https://example.com/avatars/test-user.png",
		"birthday": "1993-06-15",
		"company": {
			"id": "comp-test-001",
			"name": "TestCorp Inc.",
			"industry": "Technology",
			"employee_count": 500,
			"plan": "enterprise"
		},
		"createdAt": "2023-01-10T08:00:00.000Z",
		"description": "Test user for event spec parity validation",
		"email": "test.user@example.com",
		"firstName": "Test",
		"gender": "non-binary",
		"id": "user-parity-test-507f191e",
		"lastName": "Parity",
		"name": "Test Parity",
		"phone": "+1-555-867-5309",
		"title": "QA Engineer",
		"username": "testparity",
		"website": "https://example.com/users/testparity"
	}
}`

// trackOrderCompletedPayload is an E-Commerce v2 semantic track event for
// "Order Completed" with all standard e-commerce properties.
const trackOrderCompletedPayload = `{
	"type": "track",
	"event": "Order Completed",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"messageId": "msg-track-order-completed-parity-001",
	"timestamp": "2025-01-15T10:31:00.000Z",
	"sentAt": "2025-01-15T10:31:01.000Z",
	"originalTimestamp": "2025-01-15T10:31:00.000Z",
	"version": 1,
	"integrations": {"All": true},
	"context": {
		"active": true,
		"app": {"name": "TestApp", "version": "1.5.0", "build": "250", "namespace": "com.test.app"},
		"campaign": {"name": "Parity Sprint", "source": "google", "medium": "cpc", "term": "event spec", "content": "banner-42"},
		"device": {"id": "device-test-001", "manufacturer": "TestCorp", "model": "TestPhone X", "name": "test-device", "type": "android"},
		"ip": "198.51.100.42",
		"library": {"name": "analytics.js", "version": "3.12.0"},
		"locale": "en-US",
		"network": {"bluetooth": false, "carrier": "T-Mobile", "cellular": true, "wifi": true},
		"os": {"name": "Android", "version": "14.0"},
		"page": {"path": "/checkout/complete", "referrer": "https://example.com/cart", "title": "Order Complete", "url": "https://example.com/checkout/complete"},
		"screen": {"width": 1920, "height": 1080, "density": 2.0},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"userAgentData": {
			"brands": [{"brand": "Chromium", "version": "120"}, {"brand": "Google Chrome", "version": "120"}],
			"mobile": true,
			"platform": "Android"
		},
		"channel": "browser"
	},
	"properties": {
		"orderId": "order-parity-50314b8e",
		"affiliation": "Test Store",
		"total": 219.99,
		"revenue": 199.99,
		"shipping": 10.00,
		"tax": 15.99,
		"discount": 5.99,
		"coupon": "PARITY20",
		"currency": "USD",
		"products": [
			{
				"product_id": "prod-001",
				"sku": "SKU-PARITY-001",
				"category": "Electronics",
				"name": "Test Widget Pro",
				"brand": "TestBrand",
				"variant": "Blue",
				"price": 99.99,
				"quantity": 2,
				"coupon": "WIDGET10",
				"position": 1,
				"url": "https://example.com/products/widget-pro",
				"image_url": "https://example.com/images/widget-pro.png"
			}
		]
	}
}`

// trackProductViewedPayload is an E-Commerce v2 semantic track event for
// "Product Viewed" with all standard product properties.
const trackProductViewedPayload = `{
	"type": "track",
	"event": "Product Viewed",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"messageId": "msg-track-product-viewed-parity-001",
	"timestamp": "2025-01-15T10:32:00.000Z",
	"sentAt": "2025-01-15T10:32:01.000Z",
	"originalTimestamp": "2025-01-15T10:32:00.000Z",
	"version": 1,
	"integrations": {"All": true},
	"context": {
		"active": true,
		"app": {"name": "TestApp", "version": "1.5.0", "build": "250", "namespace": "com.test.app"},
		"device": {"id": "device-test-001", "manufacturer": "TestCorp", "model": "TestPhone X", "type": "android"},
		"ip": "198.51.100.42",
		"library": {"name": "analytics.js", "version": "3.12.0"},
		"locale": "en-US",
		"os": {"name": "Android", "version": "14.0"},
		"page": {"path": "/products/jacket", "title": "Test Jacket", "url": "https://example.com/products/jacket"},
		"screen": {"width": 1920, "height": 1080, "density": 2.0},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"channel": "browser"
	},
	"properties": {
		"product_id": "prod-002",
		"sku": "SKU-PARITY-002",
		"category": "Apparel",
		"name": "Test Jacket",
		"brand": "TestBrand",
		"variant": "Red / Large",
		"price": 149.50,
		"quantity": 1,
		"coupon": "JACKET5",
		"currency": "USD",
		"position": 3,
		"url": "https://example.com/products/jacket",
		"image_url": "https://example.com/images/jacket-red.png"
	}
}`

// trackVideoPlaybackStartedPayload is a Video semantic track event for
// "Video Playback Started" with all standard video properties.
const trackVideoPlaybackStartedPayload = `{
	"type": "track",
	"event": "Video Playback Started",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"messageId": "msg-track-video-playback-parity-001",
	"timestamp": "2025-01-15T10:33:00.000Z",
	"sentAt": "2025-01-15T10:33:01.000Z",
	"originalTimestamp": "2025-01-15T10:33:00.000Z",
	"version": 1,
	"integrations": {"All": true},
	"context": {
		"active": true,
		"app": {"name": "TestApp", "version": "1.5.0", "build": "250", "namespace": "com.test.app"},
		"device": {"id": "device-test-001", "manufacturer": "TestCorp", "model": "TestPhone X", "type": "android"},
		"ip": "198.51.100.42",
		"library": {"name": "analytics.js", "version": "3.12.0"},
		"locale": "en-US",
		"os": {"name": "Android", "version": "14.0"},
		"screen": {"width": 1920, "height": 1080, "density": 2.0},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"channel": "browser"
	},
	"properties": {
		"session_id": "vsess-parity-001",
		"content_asset_id": "content-asset-001",
		"content_pod_id": "content-pod-001",
		"ad_asset_id": "ad-asset-001",
		"ad_pod_id": "ad-pod-001",
		"ad_type": "pre-roll",
		"position": 0,
		"total_length": 360,
		"bitrate": 2500,
		"framerate": 30,
		"video_player": "TestPlayer v3.0",
		"sound": 100,
		"full_screen": true,
		"ad_enabled": true,
		"quality": "1080p",
		"livestream": false
	}
}`

// trackApplicationOpenedPayload is a Mobile lifecycle semantic track event
// for "Application Opened" with context.channel set to "mobile".
const trackApplicationOpenedPayload = `{
	"type": "track",
	"event": "Application Opened",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"messageId": "msg-track-app-opened-parity-001",
	"timestamp": "2025-01-15T10:34:00.000Z",
	"sentAt": "2025-01-15T10:34:01.000Z",
	"originalTimestamp": "2025-01-15T10:34:00.000Z",
	"version": 1,
	"integrations": {"All": true},
	"context": {
		"active": true,
		"app": {"name": "TestApp", "version": "1.5.0", "build": "250", "namespace": "com.test.app"},
		"device": {"id": "device-test-001", "manufacturer": "TestCorp", "model": "TestPhone X", "type": "android"},
		"ip": "198.51.100.42",
		"library": {"name": "analytics.js", "version": "3.12.0"},
		"locale": "en-US",
		"os": {"name": "Android", "version": "14.0"},
		"screen": {"width": 1920, "height": 1080, "density": 2.0},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"channel": "mobile"
	},
	"properties": {
		"from_background": false,
		"referring_application": "com.test.referrer",
		"url": "testapp://open?source=push",
		"version": "1.5.0"
	}
}`

// pagePayload contains a complete Segment Spec page event with full page
// properties and context.channel set to "browser".
const pagePayload = `{
	"type": "page",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"messageId": "msg-page-parity-001",
	"name": "Home",
	"category": "Landing",
	"timestamp": "2025-01-15T10:35:00.000Z",
	"sentAt": "2025-01-15T10:35:01.000Z",
	"originalTimestamp": "2025-01-15T10:35:00.000Z",
	"version": 1,
	"integrations": {"All": true},
	"context": {
		"active": true,
		"app": {"name": "TestApp", "version": "1.5.0", "build": "250", "namespace": "com.test.app"},
		"campaign": {"name": "Parity Sprint", "source": "google", "medium": "cpc", "term": "event spec", "content": "banner-42"},
		"device": {"id": "device-test-001", "manufacturer": "TestCorp", "model": "TestPhone X", "name": "test-device", "type": "android"},
		"ip": "198.51.100.42",
		"library": {"name": "analytics.js", "version": "3.12.0"},
		"locale": "en-US",
		"network": {"bluetooth": false, "carrier": "T-Mobile", "cellular": true, "wifi": true},
		"os": {"name": "Android", "version": "14.0"},
		"page": {"path": "/", "referrer": "https://www.google.com/search?q=testapp", "search": "", "title": "Home - TestApp", "url": "https://example.com/"},
		"referrer": {"type": "search", "name": "google", "url": "https://www.google.com/search?q=rudderstack", "link": "https://www.google.com"},
		"screen": {"width": 1920, "height": 1080, "density": 2.0, "innerWidth": 1920, "innerHeight": 969},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"userAgentData": {
			"brands": [{"brand": "Chromium", "version": "120"}, {"brand": "Google Chrome", "version": "120"}],
			"mobile": true,
			"platform": "Android"
		},
		"channel": "browser"
	},
	"properties": {
		"title": "Home - TestApp",
		"url": "https://example.com/",
		"path": "/",
		"referrer": "https://www.google.com/search?q=testapp",
		"search": "",
		"keywords": ["test", "parity", "analytics"]
	}
}`

// screenPayload contains a complete Segment Spec screen event with mobile
// context and context.channel set to "mobile".
const screenPayload = `{
	"type": "screen",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"messageId": "msg-screen-parity-001",
	"name": "Dashboard",
	"category": "Main",
	"timestamp": "2025-01-15T10:36:00.000Z",
	"sentAt": "2025-01-15T10:36:01.000Z",
	"originalTimestamp": "2025-01-15T10:36:00.000Z",
	"version": 1,
	"integrations": {"All": true},
	"context": {
		"active": true,
		"app": {"name": "TestApp", "version": "1.5.0", "build": "250", "namespace": "com.test.app"},
		"device": {"id": "device-test-001", "manufacturer": "TestCorp", "model": "TestPhone X", "name": "test-device", "type": "android"},
		"ip": "198.51.100.42",
		"library": {"name": "analytics.js", "version": "3.12.0"},
		"locale": "en-US",
		"network": {"bluetooth": false, "carrier": "T-Mobile", "cellular": true, "wifi": true},
		"os": {"name": "Android", "version": "14.0"},
		"screen": {"width": 1920, "height": 1080, "density": 2.0, "innerWidth": 1920, "innerHeight": 969},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"userAgentData": {
			"brands": [{"brand": "Chromium", "version": "120"}, {"brand": "Google Chrome", "version": "120"}],
			"mobile": true,
			"platform": "Android"
		},
		"channel": "mobile"
	},
	"properties": {
		"name": "Dashboard",
		"variation": "dark-mode"
	}
}`

// groupPayload contains a complete Segment Spec group event with all 12
// reserved group traits and context.channel set to "server".
const groupPayload = `{
	"type": "group",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"groupId": "grp-parity-test-001",
	"messageId": "msg-group-parity-001",
	"timestamp": "2025-01-15T10:37:00.000Z",
	"sentAt": "2025-01-15T10:37:01.000Z",
	"originalTimestamp": "2025-01-15T10:37:00.000Z",
	"version": 1,
	"integrations": {"All": true},
	"context": {
		"active": true,
		"app": {"name": "TestApp", "version": "1.5.0", "build": "250", "namespace": "com.test.app"},
		"campaign": {"name": "Parity Sprint", "source": "google", "medium": "cpc"},
		"device": {"id": "device-test-001", "manufacturer": "TestCorp", "model": "TestPhone X", "name": "test-device", "type": "android"},
		"ip": "198.51.100.42",
		"library": {"name": "analytics.js", "version": "3.12.0"},
		"locale": "en-US",
		"network": {"bluetooth": false, "carrier": "T-Mobile", "cellular": true, "wifi": true},
		"os": {"name": "Android", "version": "14.0"},
		"screen": {"width": 1920, "height": 1080, "density": 2.0},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"channel": "server"
	},
	"traits": {
		"address": {
			"street": "456 Enterprise Blvd",
			"city": "San Jose",
			"state": "CA",
			"postalCode": "95113",
			"country": "US"
		},
		"avatar": "https://example.com/orgs/testcorp-logo.png",
		"createdAt": "2020-03-15T12:00:00.000Z",
		"description": "Test organization for event spec parity validation",
		"email": "admin@testcorp.example.com",
		"employees": "500",
		"id": "grp-parity-test-001",
		"industry": "Technology",
		"name": "TestCorp Inc.",
		"phone": "+1-555-123-4567",
		"website": "https://testcorp.example.com",
		"plan": "enterprise"
	}
}`

// aliasPayload contains a complete Segment Spec alias event linking a new
// userId to a previousId with context.channel set to "server".
const aliasPayload = `{
	"type": "alias",
	"userId": "user-parity-test-507f191e",
	"anonymousId": "anon-parity-test-507f191e",
	"previousId": "anon-parity-test-507f191e",
	"messageId": "msg-alias-parity-001",
	"timestamp": "2025-01-15T10:38:00.000Z",
	"sentAt": "2025-01-15T10:38:01.000Z",
	"originalTimestamp": "2025-01-15T10:38:00.000Z",
	"version": 1,
	"integrations": {"All": true},
	"context": {
		"active": true,
		"app": {"name": "TestApp", "version": "1.5.0", "build": "250", "namespace": "com.test.app"},
		"device": {"id": "device-test-001", "manufacturer": "TestCorp", "model": "TestPhone X", "type": "android"},
		"ip": "198.51.100.42",
		"library": {"name": "analytics.js", "version": "3.12.0"},
		"locale": "en-US",
		"os": {"name": "Android", "version": "14.0"},
		"screen": {"width": 1920, "height": 1080, "density": 2.0},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.230 Mobile Safari/537.36",
		"channel": "server"
	}
}`

// batchPayload exercises the /v1/batch endpoint with a batch containing an
// identify and a track event, verifying that batched events are individually
// routed to the webhook destination.
const batchPayload = `{
	"batch": [
		{
			"type": "identify",
			"userId": "user-parity-batch-001",
			"anonymousId": "anon-parity-batch-001",
			"messageId": "msg-batch-identify-001",
			"timestamp": "2025-01-15T10:39:00.000Z",
			"sentAt": "2025-01-15T10:39:01.000Z",
			"originalTimestamp": "2025-01-15T10:39:00.000Z",
			"context": {
				"library": {"name": "analytics.js", "version": "3.12.0"},
				"ip": "198.51.100.42",
				"channel": "server"
			},
			"traits": {
				"email": "batch.user@example.com",
				"name": "Batch User"
			}
		},
		{
			"type": "track",
			"event": "Batch Test Event",
			"userId": "user-parity-batch-001",
			"anonymousId": "anon-parity-batch-001",
			"messageId": "msg-batch-track-001",
			"timestamp": "2025-01-15T10:39:02.000Z",
			"sentAt": "2025-01-15T10:39:03.000Z",
			"originalTimestamp": "2025-01-15T10:39:02.000Z",
			"context": {
				"library": {"name": "analytics.js", "version": "3.12.0"},
				"ip": "198.51.100.42",
				"channel": "server"
			},
			"properties": {
				"testKey": "testValue"
			}
		}
	]
}`
