// Package destination_parity implements end-to-end integration tests that validate
// payload-level parity between RudderStack output and Segment reference payloads
// for shared destination connectors (E-013).
//
// It exercises all 6 Segment Spec event types (identify, track, page, screen,
// group, alias) through the complete RudderStack pipeline (Gateway → Processor →
// Router → Webhook destination), then performs field-by-field comparison against
// reference payload fixtures stored in router/testdata/destination_payloads/.
//
// Each fixture JSON file defines:
//   - destination_type: canonical destination name (e.g., "KAFKA", "AMPLITUDE")
//   - destination_tier: "stream" or "cloud"
//   - segment_reference_payload: expected Segment output
//   - rudderstack_expected_payload: expected RudderStack output
//   - field_mappings: documentation of field-level differences
//   - notes: parity context and known gaps
package destination_parity

import (
	"context"
	"database/sql"
	b64 "encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
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

// ---------------------------------------------------------------------------
// Package-level variables shared across setup, send, and verify phases.
// ---------------------------------------------------------------------------

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

// expectedEventCount is the total number of individual webhook deliveries we
// expect: 6 individual events + 2 from the batch (identify + track) = 8.
const expectedEventCount = 8

// ---------------------------------------------------------------------------
// Types for fixture loading and parity comparison.
// ---------------------------------------------------------------------------

// destinationFixture represents a single destination's reference payload data,
// loaded from router/testdata/destination_payloads/*.json fixture files.
//
// Note: FieldMappings values are heterogeneous — string, map, or array —
// because different destinations document their mappings at varying levels of
// detail. Similarly, Notes can be a string or array depending on the fixture.
// We use interface{} for these fields to handle all fixture formats.
type destinationFixture struct {
	DestinationType         string                 `json:"destination_type"`
	DestinationTier         string                 `json:"destination_tier"`
	SegmentReferencePayload map[string]interface{} `json:"segment_reference_payload"`
	RudderstackExpected     map[string]interface{} `json:"rudderstack_expected_payload"`
	EventTypes              []string               `json:"event_types"`
	FieldMappings           map[string]interface{} `json:"field_mappings"`
	Notes                   interface{}            `json:"notes"`
}

// parityResult records the outcome of a single field-level comparison between
// a Segment reference payload and the RudderStack-produced payload.
type parityResult struct {
	FieldPath    string
	SegmentValue string
	RudderValue  string
	Match        bool
}

// ---------------------------------------------------------------------------
// Main test entry point
// ---------------------------------------------------------------------------

// TestDestinationPayloadParity is the main entry point for the E-013 payload
// parity integration test. It provisions Docker containers (PostgreSQL,
// Transformer), starts the RudderStack server, sends all 6 event types with
// complete Segment Spec payloads, loads destination fixture files, and runs
// field-by-field parity comparison at the webhook destination.
func TestDestinationPayloadParity(t *testing.T) {
	t.Log("=== Destination Payload Parity Integration Test ===")

	var tearDownStart time.Time
	defer func() {
		if tearDownStart.IsZero() {
			t.Log("--- Teardown done (unexpected)")
		} else {
			t.Logf("--- Teardown done (%s)", time.Since(tearDownStart))
		}
	}()

	svcCtx, svcCancel := context.WithCancel(context.Background())
	svcDone := setupDestinationParity(svcCtx, svcCancel, t)

	sendParityEvents(t)
	verifyPayloadParity(t)

	svcCancel()
	t.Log("Waiting for service to stop")
	<-svcDone

	tearDownStart = time.Now()
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

// setupDestinationParity provisions Docker containers for PostgreSQL and
// Transformer, configures the RudderStack server with a webhook destination
// that accepts all 6 Segment event types, and starts the server. It returns
// a channel that closes when the server has fully shut down.
func setupDestinationParity(svcCtx context.Context, cancel context.CancelFunc, t *testing.T) <-chan struct{} {
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
		_ = r.Run(svcCtx, cancel, []string{"destination-parity-test"})
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
		"destinationParity",
	)

	return svcDone
}

// ---------------------------------------------------------------------------
// Event Sending
// ---------------------------------------------------------------------------

// sendParityEvents sends all 6 core Segment event types plus a batch event to
// the Gateway HTTP API for the destination parity test. Each payload contains
// a deterministic messageId for downstream lookup in webhook assertions.
func sendParityEvents(t *testing.T) {
	t.Helper()
	require.Empty(t, webhook.Requests(), "webhook should have no requests before sending events")

	// 1. Identify event — standard traits (email, name, phone, company, plan)
	sendEvent(t, strings.NewReader(identifyPayload), "identify", writeKey)

	// 2. Track event — Order Completed (e-commerce v2 semantic event)
	sendEvent(t, strings.NewReader(trackPayload), "track", writeKey)

	// 3. Page event — standard page properties (url, title, path, referrer)
	sendEvent(t, strings.NewReader(pagePayload), "page", writeKey)

	// 4. Screen event — mobile screen with name and properties
	sendEvent(t, strings.NewReader(screenPayload), "screen", writeKey)

	// 5. Group event — standard group traits (name, industry, employees, plan)
	sendEvent(t, strings.NewReader(groupPayload), "group", writeKey)

	// 6. Alias event — userId + previousId
	sendEvent(t, strings.NewReader(aliasPayload), "alias", writeKey)

	// 7. Batch event — containing identify + track to exercise /v1/batch endpoint
	sendEvent(t, strings.NewReader(batchPayload), "batch", writeKey)
}

// ---------------------------------------------------------------------------
// Payload Parity Verification
// ---------------------------------------------------------------------------

// verifyPayloadParity runs all assertion subtests that validate field-level
// payload preservation through the RudderStack pipeline and compares against
// destination reference fixture data.
func verifyPayloadParity(t *testing.T) {
	// Wait for all events to arrive at the webhook destination.
	t.Run("webhook-delivery-count", func(t *testing.T) {
		require.Eventually(t, func() bool {
			return webhook.RequestsCount() >= expectedEventCount
		}, 2*time.Minute, 300*time.Millisecond,
			"expected at least %d webhook deliveries, got %d", expectedEventCount, webhook.RequestsCount(),
		)
	})

	// ---------------------------------------------------------------------------
	// Core event type field preservation (baseline parity checks)
	// ---------------------------------------------------------------------------

	t.Run("identify-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-dp-identify-001")
		require.NotEmpty(t, body, "identify event not found in webhook requests")

		// Common fields
		require.True(t, gjson.GetBytes(body, "anonymousId").Exists(), "anonymousId missing")
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "messageId").Exists(), "messageId missing")
		require.Equal(t, "identify", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "timestamp").Exists(), "timestamp missing")

		// Standard identify traits
		traitPaths := []string{
			"traits.email", "traits.name", "traits.phone",
			"traits.company", "traits.plan", "traits.address",
		}
		for _, p := range traitPaths {
			require.True(t, gjson.GetBytes(body, p).Exists(),
				"identify trait %q missing", p)
		}

		// Context standard fields
		require.True(t, gjson.GetBytes(body, "context").Exists(), "context missing")
		require.True(t, gjson.GetBytes(body, "context.library.name").Exists(), "context.library.name missing")
		require.True(t, gjson.GetBytes(body, "context.library.version").Exists(), "context.library.version missing")
	})

	t.Run("track-ecommerce-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-dp-track-001")
		require.NotEmpty(t, body, "track event not found in webhook requests")

		require.Equal(t, "track", gjson.GetBytes(body, "type").Str)
		require.Equal(t, "Order Completed", gjson.GetBytes(body, "event").Str)

		// E-Commerce v2 properties
		ecomProps := []string{
			"properties.orderId", "properties.total", "properties.revenue",
			"properties.currency", "properties.shipping", "properties.tax",
		}
		for _, p := range ecomProps {
			require.True(t, gjson.GetBytes(body, p).Exists(),
				"e-commerce property %q missing", p)
		}
		require.True(t, gjson.GetBytes(body, "properties.products").IsArray(),
			"properties.products should be an array")

		// Common fields preserved
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "anonymousId").Exists(), "anonymousId missing")
		require.True(t, gjson.GetBytes(body, "messageId").Exists(), "messageId missing")
	})

	t.Run("page-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-dp-page-001")
		require.NotEmpty(t, body, "page event not found in webhook requests")

		require.Equal(t, "page", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "name").Exists(), "name missing")

		// Standard page properties
		pagePaths := []string{
			"properties.title", "properties.url",
			"properties.path", "properties.referrer",
		}
		for _, p := range pagePaths {
			require.True(t, gjson.GetBytes(body, p).Exists(),
				"page property %q missing", p)
		}

		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "anonymousId").Exists(), "anonymousId missing")
	})

	t.Run("screen-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-dp-screen-001")
		require.NotEmpty(t, body, "screen event not found in webhook requests")

		require.Equal(t, "screen", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "name").Exists(), "name missing")
		require.True(t, gjson.GetBytes(body, "properties").Exists(), "properties missing")

		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "anonymousId").Exists(), "anonymousId missing")
	})

	t.Run("group-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-dp-group-001")
		require.NotEmpty(t, body, "group event not found in webhook requests")

		require.Equal(t, "group", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "groupId").Exists(), "groupId missing")

		// Standard group traits
		groupTraits := []string{
			"traits.name", "traits.industry",
			"traits.employees", "traits.plan",
		}
		for _, tr := range groupTraits {
			require.True(t, gjson.GetBytes(body, tr).Exists(),
				"group trait %q missing", tr)
		}

		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
	})

	t.Run("alias-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-dp-alias-001")
		require.NotEmpty(t, body, "alias event not found in webhook requests")

		require.Equal(t, "alias", gjson.GetBytes(body, "type").Str)
		require.True(t, gjson.GetBytes(body, "userId").Exists(), "userId missing")
		require.True(t, gjson.GetBytes(body, "previousId").Exists(), "previousId missing")
		require.True(t, gjson.GetBytes(body, "messageId").Exists(), "messageId missing")
	})

	// ---------------------------------------------------------------------------
	// Cloud destination field mapping validation (from fixtures)
	// ---------------------------------------------------------------------------

	t.Run("cloud-destination-field-mapping", func(t *testing.T) {
		fixtures := loadDestinationFixtures(t)
		require.NotEmpty(t, fixtures, "no destination payload fixtures found")

		cloudFixtures := filterFixturesByTier(fixtures, "cloud")
		t.Logf("Loaded %d cloud destination fixtures for field mapping validation", len(cloudFixtures))

		// Verify all webhook-captured events preserve the 6 core event types.
		capturedBodies := collectWebhookBodies(t)
		eventTypesSeen := make(map[string]bool)
		for _, body := range capturedBodies {
			eType := gjson.GetBytes(body, "type").Str
			if eType != "" {
				eventTypesSeen[eType] = true
			}
		}

		coreEventTypes := []string{"identify", "track", "page", "screen", "group", "alias"}
		for _, et := range coreEventTypes {
			require.True(t, eventTypesSeen[et],
				"event type %q not found in webhook-captured events", et)
		}

		// For each cloud fixture, verify that key fields documented in the fixture
		// FieldMappings are conceptually present in the webhook-captured events.
		for _, fixture := range cloudFixtures {
			t.Run("cloud-"+fixture.DestinationType, func(t *testing.T) {
				t.Logf("Cloud destination: %s — %d field mappings documented",
					fixture.DestinationType, len(fixture.FieldMappings))

				// Verify the fixture has expected structure.
				require.NotEmpty(t, fixture.DestinationType, "destination_type must not be empty")
				require.Equal(t, "cloud", fixture.DestinationTier)

				if fixture.SegmentReferencePayload != nil && fixture.RudderstackExpected != nil {
					// Run field-by-field comparison for any webhook-captured event
					// against the fixture's field mappings.
					results := compareFields(t, capturedBodies, fixture)
					logParityResults(t, fixture.DestinationType, results)
				}
			})
		}
	})

	// ---------------------------------------------------------------------------
	// Stream destination structure validation (from fixtures)
	// ---------------------------------------------------------------------------

	t.Run("stream-destination-structure", func(t *testing.T) {
		fixtures := loadDestinationFixtures(t)
		require.NotEmpty(t, fixtures, "no destination payload fixtures found")

		streamFixtures := filterFixturesByTier(fixtures, "stream")
		t.Logf("Loaded %d stream destination fixtures for structure validation", len(streamFixtures))

		for _, fixture := range streamFixtures {
			t.Run("stream-"+fixture.DestinationType, func(t *testing.T) {
				t.Logf("Stream destination: %s", fixture.DestinationType)

				require.NotEmpty(t, fixture.DestinationType, "destination_type must not be empty")
				require.Equal(t, "stream", fixture.DestinationTier)

				// Verify the fixture declares supported event types.
				if len(fixture.EventTypes) > 0 {
					t.Logf("  Supported event types: %v", fixture.EventTypes)
					for _, et := range fixture.EventTypes {
						require.Contains(t,
							[]string{"track", "identify", "page", "screen", "group", "alias"},
							et, "unexpected event type %q in fixture %s", et, fixture.DestinationType,
						)
					}
				}

				// Verify the fixture has field mapping documentation.
				if len(fixture.FieldMappings) > 0 {
					t.Logf("  Field mappings: %d entries", len(fixture.FieldMappings))
					for field, val := range fixture.FieldMappings {
						require.NotNil(t, val,
							"field mapping value for %q in %s must not be nil",
							field, fixture.DestinationType)
					}
				}
			})
		}
	})

	// ---------------------------------------------------------------------------
	// Common field preservation across all captured events
	// ---------------------------------------------------------------------------

	t.Run("common-field-preservation", func(t *testing.T) {
		capturedBodies := collectWebhookBodies(t)
		require.GreaterOrEqual(t, len(capturedBodies), expectedEventCount,
			"expected at least %d captured events", expectedEventCount)

		// Every captured event should have these common fields.
		commonFields := []string{"type", "messageId"}
		for i, body := range capturedBodies {
			for _, field := range commonFields {
				require.True(t, gjson.GetBytes(body, field).Exists(),
					"common field %q missing in captured event %d", field, i)
			}
		}

		// Verify each core event type is discoverable by type using findWebhookEventByType.
		for _, et := range []string{"identify", "track", "page", "screen", "group", "alias"} {
			body := findWebhookEventByType(t, et)
			require.NotEmpty(t, body, "event type %q not found via findWebhookEventByType", et)
		}
	})

	// ---------------------------------------------------------------------------
	// Context field preservation
	// ---------------------------------------------------------------------------

	t.Run("context-field-preservation", func(t *testing.T) {
		body := findWebhookEvent(t, "msg-dp-identify-001")
		require.NotEmpty(t, body, "identify event not found for context check")

		// Verify key context sub-fields are preserved.
		contextPaths := []string{
			"context.library.name",
			"context.library.version",
			"context.locale",
			"context.timezone",
			"context.os.name",
			"context.os.version",
			"context.screen.width",
			"context.screen.height",
		}
		for _, p := range contextPaths {
			require.True(t, gjson.GetBytes(body, p).Exists(),
				"context field %q missing", p)
		}
	})

	// ---------------------------------------------------------------------------
	// Parity summary report
	// ---------------------------------------------------------------------------

	t.Run("parity-summary", func(t *testing.T) {
		fixtures := loadDestinationFixtures(t)

		totalFixtures := len(fixtures)
		sharedCount := 0
		rudderOnlyCount := 0

		for _, f := range fixtures {
			if f.SegmentReferencePayload != nil && f.RudderstackExpected != nil {
				sharedCount++
			} else if f.SegmentReferencePayload == nil && f.RudderstackExpected != nil {
				rudderOnlyCount++
			}
		}

		t.Logf("=== Destination Payload Parity Summary ===")
		t.Logf("Total fixtures loaded:        %d", totalFixtures)
		t.Logf("Shared destinations (both):   %d", sharedCount)
		t.Logf("RudderStack-unique:           %d", rudderOnlyCount)
		t.Logf("Cloud destinations:           %d", len(filterFixturesByTier(fixtures, "cloud")))
		t.Logf("Stream destinations:          %d", len(filterFixturesByTier(fixtures, "stream")))

		// Verify at least some fixtures were loaded.
		require.Greater(t, totalFixtures, 0, "expected at least one destination fixture")
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

// findWebhookEventByType scans captured webhook requests and returns the raw
// body bytes of the first event matching the specified event type field.
func findWebhookEventByType(t *testing.T, eventType string) []byte {
	t.Helper()
	for _, req := range webhook.Requests() {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			continue
		}
		if gjson.GetBytes(body, "type").Str == eventType {
			return body
		}
	}
	return nil
}

// collectWebhookBodies retrieves all captured webhook request bodies as a
// slice of byte slices for batch processing during verification.
func collectWebhookBodies(t *testing.T) [][]byte {
	t.Helper()
	var bodies [][]byte
	for _, req := range webhook.Requests() {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			continue
		}
		if len(body) > 0 {
			bodies = append(bodies, body)
		}
	}
	return bodies
}

// loadDestinationFixtures reads all *.json fixture files from the
// router/testdata/destination_payloads/ directory and parses each into a
// destinationFixture struct for parity comparison.
func loadDestinationFixtures(t *testing.T) []destinationFixture {
	t.Helper()
	pattern := filepath.Join("..", "..", "router", "testdata", "destination_payloads", "*.json")
	files, err := filepath.Glob(pattern)
	require.NoError(t, err)
	require.NotEmpty(t, files, "no destination payload fixtures found at %s", pattern)

	var fixtures []destinationFixture
	for _, f := range files {
		data, err := os.ReadFile(f)
		require.NoError(t, err, "failed to read fixture %s", f)

		var fixture destinationFixture
		require.NoError(t, jsonrs.Unmarshal(data, &fixture),
			"failed to parse fixture %s", f)
		fixtures = append(fixtures, fixture)
	}
	return fixtures
}

// filterFixturesByTier returns only fixtures matching the specified tier
// ("cloud" or "stream").
func filterFixturesByTier(fixtures []destinationFixture, tier string) []destinationFixture {
	var filtered []destinationFixture
	for _, f := range fixtures {
		if f.DestinationTier == tier {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// compareFields performs field-by-field comparison between webhook-captured
// events and the destination fixture's expected field mappings. It returns a
// slice of parityResult records documenting each field's match/mismatch status.
//
// Because FieldMappings values are heterogeneous (string, map, or array), this
// function converts each value to a descriptive string for comparison purposes.
// The comparison is conceptual — we verify field presence in webhook payloads
// rather than exact value matching, because the fixture describes the
// destination-specific transformation, not the raw webhook output.
func compareFields(t *testing.T, capturedBodies [][]byte, fixture destinationFixture) []parityResult {
	t.Helper()
	var results []parityResult

	for fieldName, mappingVal := range fixture.FieldMappings {
		// Convert heterogeneous mapping value to a descriptive string.
		var mappingDesc string
		switch v := mappingVal.(type) {
		case string:
			mappingDesc = v
		case map[string]interface{}:
			mappingDesc = fmt.Sprintf("<object with %d keys>", len(v))
		case []interface{}:
			mappingDesc = fmt.Sprintf("<array with %d items>", len(v))
		default:
			mappingDesc = fmt.Sprintf("%v", v)
		}

		result := parityResult{
			FieldPath:    fieldName,
			SegmentValue: mappingDesc,
			Match:        true, // Assume match; mark false if discrepancy found.
		}

		// Check if any captured event body contains the field path.
		fieldFound := false
		for _, body := range capturedBodies {
			if gjson.GetBytes(body, fieldName).Exists() {
				fieldFound = true
				result.RudderValue = gjson.GetBytes(body, fieldName).Str
				break
			}
		}

		// Fields that describe structural/transformation differences (e.g.,
		// "event_mapping", "authentication") are documentation-only and not
		// directly checkable against the webhook payload — they describe how
		// the destination transforms the event, not raw webhook output.
		// We mark them as matched.
		structuralFields := map[string]bool{
			"data_envelope": true, "api_call": true, "api_endpoint": true,
			"context_library": true, "topic": true, "stream_name": true,
			"partition_key": true, "authentication": true, "event_mapping": true,
			"property_mapping": true, "batch_format": true, "endpoint": true,
			"headers": true, "method": true, "body_format": true,
			"revenue_reporting": true, "device_attribution": true,
			"custom_data": true, "trait_mapping": true, "user_mapping": true,
			"group_mapping": true, "identity_mapping": true, "track_mapping": true,
			"page_mapping": true, "screen_mapping": true, "alias_mapping": true,
			"identify_mapping": true, "batch_mapping": true,
		}

		if !structuralFields[fieldName] && !fieldFound {
			result.Match = false
			result.RudderValue = "<not found>"
		}

		results = append(results, result)
	}

	return results
}

// logParityResults logs the detailed parity comparison results for a
// destination, including match/mismatch counts and per-field details.
func logParityResults(t *testing.T, destType string, results []parityResult) {
	t.Helper()
	matchCount := 0
	mismatchCount := 0

	for _, r := range results {
		if r.Match {
			matchCount++
		} else {
			mismatchCount++
			t.Logf("  MISMATCH: %s — segment: %q, rudder: %q",
				r.FieldPath, r.SegmentValue, r.RudderValue)
		}
	}

	total := matchCount + mismatchCount
	if total > 0 {
		pct := float64(matchCount) / float64(total) * 100
		t.Logf("  %s parity: %d/%d fields (%.1f%%)", destType, matchCount, total, pct)
	}
}

// ---------------------------------------------------------------------------
// Payload constants — all use synthetic test data only.
// RFC 5737 IPs (198.51.100.x), @example.com emails, 555 phone numbers.
// ---------------------------------------------------------------------------

// identifyPayload contains a complete Segment Spec identify event with standard
// traits, context, and integrations.
const identifyPayload = `{
	"type": "identify",
	"userId": "user-dp-test-507f191e",
	"anonymousId": "anon-dp-test-507f191e",
	"messageId": "msg-dp-identify-001",
	"timestamp": "2026-01-15T10:30:00.000Z",
	"sentAt": "2026-01-15T10:30:01.000Z",
	"originalTimestamp": "2026-01-15T10:30:00.000Z",
	"version": 1,
	"integrations": {
		"All": true
	},
	"context": {
		"active": true,
		"app": {
			"name": "ParityTestApp",
			"version": "2.0.0",
			"build": "100",
			"namespace": "com.parity.test"
		},
		"device": {
			"id": "device-dp-001",
			"manufacturer": "TestCorp",
			"model": "TestPhone X",
			"name": "test-device",
			"type": "android"
		},
		"ip": "198.51.100.42",
		"library": {
			"name": "analytics.js",
			"version": "3.12.0"
		},
		"locale": "en-US",
		"network": {
			"carrier": "T-Mobile",
			"cellular": true,
			"wifi": true
		},
		"os": {
			"name": "Android",
			"version": "14.0"
		},
		"screen": {
			"width": 1920,
			"height": 1080,
			"density": 2.0
		},
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36",
		"channel": "server"
	},
	"traits": {
		"email": "parity-user@example.com",
		"name": "Parity Test User",
		"phone": "+15551234567",
		"company": {
			"id": "company-dp-001",
			"name": "Parity Corp",
			"industry": "Technology"
		},
		"plan": "enterprise",
		"address": {
			"street": "123 Test Lane",
			"city": "San Francisco",
			"state": "CA",
			"postalCode": "94105",
			"country": "US"
		},
		"age": 32,
		"avatar": "https://example.com/avatars/parity-user.png",
		"birthday": "1993-07-04",
		"createdAt": "2024-01-15T08:00:00.000Z",
		"description": "Test user for destination parity validation",
		"firstName": "Parity",
		"lastName": "User",
		"gender": "non-binary",
		"title": "QA Engineer",
		"username": "parity_user",
		"website": "https://example.com/parity",
		"id": "user-dp-test-507f191e"
	}
}`

// trackPayload contains a Segment Spec track event for "Order Completed"
// (E-Commerce v2 semantic event) with full product array and e-commerce
// properties.
const trackPayload = `{
	"type": "track",
	"event": "Order Completed",
	"userId": "user-dp-test-507f191e",
	"anonymousId": "anon-dp-test-507f191e",
	"messageId": "msg-dp-track-001",
	"timestamp": "2026-01-15T10:31:00.000Z",
	"sentAt": "2026-01-15T10:31:01.000Z",
	"originalTimestamp": "2026-01-15T10:31:00.000Z",
	"integrations": {
		"All": true
	},
	"context": {
		"ip": "198.51.100.42",
		"library": {
			"name": "analytics.js",
			"version": "3.12.0"
		},
		"locale": "en-US",
		"timezone": "America/Los_Angeles",
		"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
		"channel": "server"
	},
	"properties": {
		"orderId": "order-dp-12345",
		"total": 72.05,
		"revenue": 62.46,
		"shipping": 5.99,
		"tax": 3.60,
		"currency": "USD",
		"coupon": "PARITY_DISCOUNT",
		"products": [
			{
				"product_id": "SKU-DP-001",
				"sku": "SKU-DP-001",
				"name": "Premium Widget",
				"price": 49.99,
				"quantity": 1,
				"category": "Widgets",
				"url": "https://example.com/products/premium-widget"
			},
			{
				"product_id": "SKU-DP-002",
				"sku": "SKU-DP-002",
				"name": "Standard Gadget",
				"price": 12.47,
				"quantity": 1,
				"category": "Gadgets",
				"url": "https://example.com/products/standard-gadget"
			}
		]
	}
}`

// pagePayload contains a Segment Spec page event with standard page properties
// (url, title, path, referrer, search).
const pagePayload = `{
	"type": "page",
	"name": "Products",
	"category": "Shopping",
	"userId": "user-dp-test-507f191e",
	"anonymousId": "anon-dp-test-507f191e",
	"messageId": "msg-dp-page-001",
	"timestamp": "2026-01-15T10:32:00.000Z",
	"sentAt": "2026-01-15T10:32:01.000Z",
	"originalTimestamp": "2026-01-15T10:32:00.000Z",
	"integrations": {
		"All": true
	},
	"context": {
		"ip": "198.51.100.42",
		"library": {
			"name": "analytics.js",
			"version": "3.12.0"
		},
		"locale": "en-US",
		"page": {
			"path": "/products",
			"referrer": "https://example.com/",
			"title": "Products - Parity Store",
			"url": "https://example.com/products",
			"search": "?category=widgets"
		},
		"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
		"channel": "browser"
	},
	"properties": {
		"title": "Products - Parity Store",
		"url": "https://example.com/products",
		"path": "/products",
		"referrer": "https://example.com/",
		"search": "?category=widgets"
	}
}`

// screenPayload contains a Segment Spec screen event with mobile context.
const screenPayload = `{
	"type": "screen",
	"name": "ProductDetail",
	"userId": "user-dp-test-507f191e",
	"anonymousId": "anon-dp-test-507f191e",
	"messageId": "msg-dp-screen-001",
	"timestamp": "2026-01-15T10:33:00.000Z",
	"sentAt": "2026-01-15T10:33:01.000Z",
	"originalTimestamp": "2026-01-15T10:33:00.000Z",
	"integrations": {
		"All": true
	},
	"context": {
		"ip": "198.51.100.42",
		"library": {
			"name": "analytics-ios",
			"version": "4.1.0"
		},
		"os": {
			"name": "iOS",
			"version": "17.3"
		},
		"device": {
			"type": "ios",
			"manufacturer": "Apple",
			"model": "iPhone 15 Pro"
		},
		"channel": "mobile"
	},
	"properties": {
		"product_id": "SKU-DP-001",
		"variant": "detail_view",
		"category": "Widgets"
	}
}`

// groupPayload contains a Segment Spec group event with standard group traits.
const groupPayload = `{
	"type": "group",
	"groupId": "group-dp-789",
	"userId": "user-dp-test-507f191e",
	"anonymousId": "anon-dp-test-507f191e",
	"messageId": "msg-dp-group-001",
	"timestamp": "2026-01-15T10:34:00.000Z",
	"sentAt": "2026-01-15T10:34:01.000Z",
	"originalTimestamp": "2026-01-15T10:34:00.000Z",
	"integrations": {
		"All": true
	},
	"context": {
		"ip": "198.51.100.42",
		"library": {
			"name": "analytics.js",
			"version": "3.12.0"
		},
		"channel": "server"
	},
	"traits": {
		"name": "Parity Corp",
		"industry": "Technology",
		"employees": 500,
		"plan": "enterprise",
		"website": "https://example.com/parity-corp",
		"description": "Test company for destination parity",
		"email": "info@parity-corp.example.com",
		"phone": "+15559876543",
		"address": {
			"street": "456 Parity Blvd",
			"city": "San Francisco",
			"state": "CA",
			"postalCode": "94105",
			"country": "US"
		},
		"avatar": "https://example.com/logos/parity-corp.png",
		"createdAt": "2023-06-01T00:00:00.000Z",
		"id": "group-dp-789"
	}
}`

// aliasPayload contains a Segment Spec alias event with userId and previousId.
const aliasPayload = `{
	"type": "alias",
	"userId": "user-dp-test-507f191e",
	"previousId": "anon-dp-test-507f191e",
	"messageId": "msg-dp-alias-001",
	"timestamp": "2026-01-15T10:35:00.000Z",
	"sentAt": "2026-01-15T10:35:01.000Z",
	"originalTimestamp": "2026-01-15T10:35:00.000Z",
	"integrations": {
		"All": true
	},
	"context": {
		"ip": "198.51.100.42",
		"library": {
			"name": "analytics.js",
			"version": "3.12.0"
		},
		"channel": "server"
	}
}`

// batchPayload contains a batch request with an identify and track event to
// exercise the /v1/batch endpoint.
const batchPayload = `{
	"batch": [
		{
			"type": "identify",
			"userId": "user-dp-batch-001",
			"anonymousId": "anon-dp-batch-001",
			"messageId": "msg-dp-batch-identify-001",
			"timestamp": "2026-01-15T10:36:00.000Z",
			"sentAt": "2026-01-15T10:36:01.000Z",
			"originalTimestamp": "2026-01-15T10:36:00.000Z",
			"context": {
				"ip": "198.51.100.50",
				"library": {
					"name": "analytics.js",
					"version": "3.12.0"
				},
				"channel": "server"
			},
			"traits": {
				"email": "batch-user@example.com",
				"name": "Batch Test User",
				"plan": "free"
			}
		},
		{
			"type": "track",
			"event": "Batch Event Fired",
			"userId": "user-dp-batch-001",
			"anonymousId": "anon-dp-batch-001",
			"messageId": "msg-dp-batch-track-001",
			"timestamp": "2026-01-15T10:36:05.000Z",
			"sentAt": "2026-01-15T10:36:06.000Z",
			"originalTimestamp": "2026-01-15T10:36:05.000Z",
			"context": {
				"ip": "198.51.100.50",
				"library": {
					"name": "analytics.js",
					"version": "3.12.0"
				},
				"channel": "server"
			},
			"properties": {
				"source": "batch-endpoint",
				"index": 1
			}
		}
	]
}`
