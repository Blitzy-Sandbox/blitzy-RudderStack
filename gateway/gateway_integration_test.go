package gateway_test

import (
	"bytes"
	"context"
	"database/sql"
	b64 "encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	kithttputil "github.com/rudderlabs/rudder-go-kit/httputil"
	kithelper "github.com/rudderlabs/rudder-go-kit/testhelper"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"
	transformertest "github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/transformer"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/runner"
	migrator "github.com/rudderlabs/rudder-server/services/sql-migrator"
	"github.com/rudderlabs/rudder-server/testhelper/backendconfigtest"
	"github.com/rudderlabs/rudder-server/testhelper/health"
)

func TestWebhook(t *testing.T) {
	bcServer := backendconfigtest.NewBuilder().
		WithWorkspaceConfig(
			backendconfigtest.NewConfigBuilder().
				WithSource(
					backendconfigtest.NewSourceBuilder().
						WithID("source-1").
						WithWriteKey("writekey-1").
						WithSourceCategory("webhook").
						WithSourceType("SeGment").
						Build()).
				Build()).
		Build()
	defer bcServer.Close()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	postgresContainer, err := postgres.Setup(pool, t)
	require.NoError(t, err)
	transformerContainer, err := transformertest.Setup(pool, t)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gwPort, err := kithelper.GetFreePort()
	require.NoError(t, err)

	wg, ctx := errgroup.WithContext(ctx)
	wg.Go(func() error {
		err := runGateway(ctx, cancel, gwPort, postgresContainer, bcServer.URL, transformerContainer.TransformerURL, t.TempDir())
		if err != nil {
			t.Logf("rudder-server exited with error: %v", err)
		}
		return err
	})

	url := fmt.Sprintf("http://localhost:%d", gwPort)
	health.WaitUntilReady(ctx, t, url+"/health", 60*time.Second, 10*time.Millisecond, t.Name())
	// send an event
	req, err := http.NewRequest(http.MethodPost, url+"/v1/webhook", bytes.NewReader([]byte(`{"userId": "user-1", "type": "identity"}`)))
	require.NoError(t, err)
	req.SetBasicAuth("writekey-1", "password")
	resp, err := (&http.Client{}).Do(req)
	require.NoError(t, err, "it should be able to send a webhook event to gateway")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	func() { kithttputil.CloseResponse(resp) }()
	require.NoError(t, err, "it should be able to send a webhook event to gateway")

	// check that the event is stored in jobsdb
	requireJobsCount(t, postgresContainer.DB, "gw", jobsdb.Unprocessed.State, 1)
}

func TestDocsEndpoint(t *testing.T) {
	bcServer := backendconfigtest.NewBuilder().
		WithWorkspaceConfig(
			backendconfigtest.NewConfigBuilder().
				WithSource(
					backendconfigtest.NewSourceBuilder().
						WithID("source-1").
						WithWriteKey("writekey-1").
						WithSourceCategory("webhook").
						WithSourceType("my_source_type").
						Build()).
				Build()).
		Build()
	defer bcServer.Close()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	postgresContainer, err := postgres.Setup(pool, t)
	require.NoError(t, err)
	transformerContainer, err := transformertest.Setup(pool, t)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gwPort, err := kithelper.GetFreePort()
	require.NoError(t, err)

	wg, ctx := errgroup.WithContext(ctx)
	wg.Go(func() error {
		err := runGateway(ctx, cancel, gwPort, postgresContainer, bcServer.URL, transformerContainer.TransformerURL, t.TempDir())
		if err != nil {
			t.Logf("rudder-server exited with error: %v", err)
		}
		return err
	})

	url := fmt.Sprintf("http://localhost:%d", gwPort)
	health.WaitUntilReady(ctx, t, url+"/health", 60*time.Second, 10*time.Millisecond, t.Name())
	resp, err := http.Get(url + "/docs")
	require.NoError(t, err, "it should be able to get the docs")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer func() { kithttputil.CloseResponse(resp) }()
	require.Equal(t, resp.Header.Get("Content-Type"), "text/html; charset=utf-8")
	all, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Greater(t, len(all), 0)
}

// TestMultiSDKIntegration validates that SDK-specific payloads from multiple
// Segment SDK platforms (JavaScript, iOS, Android, Node.js) are correctly
// accepted through the full Gateway pipeline. It provisions Docker containers
// for PostgreSQL and Transformer, starts a real Gateway instance, and verifies
// that each SDK's payload format passes Write Key Basic Auth, is accepted via
// the correct /v1/{type} endpoint, and is persisted to JobsDB in the expected
// Unprocessed state.
func TestMultiSDKIntegration(t *testing.T) {
	// Setup: event stream source (default category "eventStream") with a
	// dedicated writeKey for SDK compatibility testing.
	const writeKey = "sdk-test-writekey"
	bcServer := backendconfigtest.NewBuilder().
		WithWorkspaceConfig(
			backendconfigtest.NewConfigBuilder().
				WithSource(
					backendconfigtest.NewSourceBuilder().
						WithID("sdk-source-1").
						WithWriteKey(writeKey).
						Build()).
				Build()).
		Build()
	defer bcServer.Close()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	postgresContainer, err := postgres.Setup(pool, t)
	require.NoError(t, err)
	transformerContainer, err := transformertest.Setup(pool, t)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gwPort, err := kithelper.GetFreePort()
	require.NoError(t, err)

	wg, ctx := errgroup.WithContext(ctx)
	wg.Go(func() error {
		err := runGateway(ctx, cancel, gwPort, postgresContainer, bcServer.URL, transformerContainer.TransformerURL, t.TempDir())
		if err != nil {
			t.Logf("rudder-server exited with error: %v", err)
		}
		return err
	})

	gwURL := fmt.Sprintf("http://localhost:%d", gwPort)
	health.WaitUntilReady(ctx, t, gwURL+"/health", 60*time.Second, 10*time.Millisecond, t.Name())

	// Construct the Segment-compatible Basic Auth header: base64(writeKey:)
	// The trailing colon is required because Basic Auth format is
	// username:password with an empty password field.
	authHeader := fmt.Sprintf("Basic %s", b64.StdEncoding.EncodeToString(
		fmt.Appendf(nil, "%s:", writeKey),
	))

	// sendSDKEvent is a helper that sends a JSON payload to the specified
	// Gateway endpoint with Write Key Basic Auth and validates a 200 OK
	// response. It mirrors the sendEvent() pattern from integration_test.go.
	sendSDKEvent := func(t *testing.T, endpoint, payload string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gwURL+endpoint, bytes.NewReader([]byte(payload)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", authHeader)
		resp, err := (&http.Client{}).Do(req)
		require.NoError(t, err, "should be able to send event to %s", endpoint)
		require.Equal(t, http.StatusOK, resp.StatusCode, "expected 200 OK from %s", endpoint)
		func() { kithttputil.CloseResponse(resp) }()
	}

	// Track cumulative expected jobs across sequential subtests since all
	// subtests share the same Gateway and PostgreSQL instance. The Gateway
	// runs in APP_TYPE=gateway mode (no processor), so jobs remain in
	// Unprocessed state indefinitely.
	totalExpectedJobs := 0

	// Subtest 1: JavaScript SDK identify via /v1/identify
	// Validates that an analytics.js identify payload with standard web SDK
	// context fields (library, page, userAgent) is accepted and persisted.
	t.Run("JavaScript SDK identify via /v1/identify", func(t *testing.T) {
		payload := `{
			"userId": "js-user-1",
			"anonymousId": "js-anon-1",
			"type": "identify",
			"traits": {
				"email": "js-user@example.com",
				"name": "JS User"
			},
			"context": {
				"library": {"name": "analytics.js", "version": "1.68.0"},
				"page": {
					"url": "https://example.com",
					"path": "/home",
					"title": "Home",
					"referrer": "https://google.com"
				},
				"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"
			},
			"messageId": "js-identify-msg-1",
			"timestamp": "2024-06-01T12:00:00.000Z"
		}`
		sendSDKEvent(t, "/v1/identify", payload)
		totalExpectedJobs++
		requireJobsCount(t, postgresContainer.DB, "gw", jobsdb.Unprocessed.State, totalExpectedJobs)
	})

	// Subtest 2: JavaScript SDK track via /v1/track
	// Validates that an analytics.js track event with properties and page
	// context is accepted and persisted.
	t.Run("JavaScript SDK track via /v1/track", func(t *testing.T) {
		payload := `{
			"userId": "js-user-1",
			"anonymousId": "js-anon-1",
			"type": "track",
			"event": "Button Clicked",
			"properties": {
				"button_name": "Sign Up",
				"page": "Landing"
			},
			"context": {
				"library": {"name": "analytics.js", "version": "1.68.0"},
				"page": {
					"url": "https://example.com/signup",
					"path": "/signup",
					"title": "Sign Up"
				},
				"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"
			},
			"messageId": "js-track-msg-1",
			"timestamp": "2024-06-01T12:01:00.000Z"
		}`
		sendSDKEvent(t, "/v1/track", payload)
		totalExpectedJobs++
		requireJobsCount(t, postgresContainer.DB, "gw", jobsdb.Unprocessed.State, totalExpectedJobs)
	})

	// Subtest 3: Server SDK batch via /v1/batch
	// Validates that a Node.js SDK batch payload containing 3 mixed event
	// types (identify + track + page) is accepted as a single HTTP request
	// and results in 3 separate jobs in JobsDB (one per event).
	t.Run("Server SDK batch via /v1/batch", func(t *testing.T) {
		payload := `{
			"batch": [
				{
					"userId": "node-user-1",
					"anonymousId": "node-anon-1",
					"type": "identify",
					"traits": {"email": "node-user@example.com"},
					"context": {"library": {"name": "analytics-node", "version": "6.2.0"}},
					"messageId": "node-identify-1",
					"timestamp": "2024-06-01T12:02:00.000Z"
				},
				{
					"userId": "node-user-1",
					"anonymousId": "node-anon-1",
					"type": "track",
					"event": "Order Completed",
					"properties": {"orderId": "12345", "total": 99.99},
					"context": {"library": {"name": "analytics-node", "version": "6.2.0"}},
					"messageId": "node-track-1",
					"timestamp": "2024-06-01T12:02:01.000Z"
				},
				{
					"userId": "node-user-1",
					"anonymousId": "node-anon-1",
					"type": "page",
					"name": "Checkout",
					"properties": {"url": "https://example.com/checkout"},
					"context": {"library": {"name": "analytics-node", "version": "6.2.0"}},
					"messageId": "node-page-1",
					"timestamp": "2024-06-01T12:02:02.000Z"
				}
			],
			"sentAt": "2024-06-01T12:02:03.000Z"
		}`
		sendSDKEvent(t, "/v1/batch", payload)
		totalExpectedJobs += 3
		requireJobsCount(t, postgresContainer.DB, "gw", jobsdb.Unprocessed.State, totalExpectedJobs)
	})

	// Subtest 4: Mobile SDK screen via /v1/screen
	// Validates that an analytics-ios screen payload with full mobile context
	// auto-collection fields (device, os, app, network, screen) is accepted.
	// These context fields must pass through the Gateway without modification.
	t.Run("Mobile SDK screen via /v1/screen", func(t *testing.T) {
		payload := `{
			"userId": "ios-user-1",
			"anonymousId": "ios-anon-1",
			"type": "screen",
			"name": "Home Screen",
			"category": "Main",
			"properties": {"variation": "blue"},
			"context": {
				"library": {"name": "analytics-ios", "version": "4.1.0"},
				"device": {
					"id": "B5372DB0-C21E-11E4-8DFC-AA07A5B093DB",
					"manufacturer": "Apple",
					"model": "iPhone14,5",
					"name": "iPhone 13",
					"type": "ios"
				},
				"os": {"name": "iOS", "version": "17.4.1"},
				"app": {
					"name": "MyApp",
					"version": "2.1.0",
					"build": "100",
					"namespace": "com.example.myapp"
				},
				"network": {
					"bluetooth": false,
					"carrier": "T-Mobile",
					"cellular": true,
					"wifi": true
				},
				"screen": {"density": 3, "height": 844, "width": 390}
			},
			"messageId": "ios-screen-msg-1",
			"timestamp": "2024-06-01T12:03:00.000Z"
		}`
		sendSDKEvent(t, "/v1/screen", payload)
		totalExpectedJobs++
		requireJobsCount(t, postgresContainer.DB, "gw", jobsdb.Unprocessed.State, totalExpectedJobs)
	})

	// Subtest 5: All 6 event types via individual endpoints
	// Validates that every Segment Spec event type (identify, track, page,
	// screen, group, alias) is accepted via its respective /v1/{type}
	// endpoint and all 6 jobs are persisted to JobsDB.
	t.Run("All 6 event types via individual endpoints", func(t *testing.T) {
		eventTypes := []struct {
			endpoint string
			payload  string
		}{
			{
				endpoint: "/v1/identify",
				payload:  `{"userId":"all6-user","anonymousId":"all6-anon","type":"identify","traits":{"plan":"free"},"context":{"library":{"name":"analytics.js","version":"1.68.0"}},"messageId":"all6-identify","timestamp":"2024-06-01T12:04:00.000Z"}`,
			},
			{
				endpoint: "/v1/track",
				payload:  `{"userId":"all6-user","anonymousId":"all6-anon","type":"track","event":"Test Event","properties":{"key":"val"},"context":{"library":{"name":"analytics.js","version":"1.68.0"}},"messageId":"all6-track","timestamp":"2024-06-01T12:04:01.000Z"}`,
			},
			{
				endpoint: "/v1/page",
				payload:  `{"userId":"all6-user","anonymousId":"all6-anon","type":"page","name":"Test Page","properties":{"url":"https://example.com"},"context":{"library":{"name":"analytics.js","version":"1.68.0"}},"messageId":"all6-page","timestamp":"2024-06-01T12:04:02.000Z"}`,
			},
			{
				endpoint: "/v1/screen",
				payload:  `{"userId":"all6-user","anonymousId":"all6-anon","type":"screen","name":"Test Screen","context":{"library":{"name":"analytics.js","version":"1.68.0"}},"messageId":"all6-screen","timestamp":"2024-06-01T12:04:03.000Z"}`,
			},
			{
				endpoint: "/v1/group",
				payload:  `{"userId":"all6-user","anonymousId":"all6-anon","type":"group","groupId":"grp-1","traits":{"name":"Test Group"},"context":{"library":{"name":"analytics.js","version":"1.68.0"}},"messageId":"all6-group","timestamp":"2024-06-01T12:04:04.000Z"}`,
			},
			{
				endpoint: "/v1/alias",
				payload:  `{"userId":"all6-user","anonymousId":"all6-anon","type":"alias","previousId":"old-id","context":{"library":{"name":"analytics.js","version":"1.68.0"}},"messageId":"all6-alias","timestamp":"2024-06-01T12:04:05.000Z"}`,
			},
		}
		for _, evt := range eventTypes {
			sendSDKEvent(t, evt.endpoint, evt.payload)
		}
		totalExpectedJobs += 6
		requireJobsCount(t, postgresContainer.DB, "gw", jobsdb.Unprocessed.State, totalExpectedJobs)
	})

	// Subtest 6: Write Key Basic Auth from multiple SDK platforms
	// Validates that events from different Segment SDK platforms (JavaScript,
	// iOS, Android, Node.js) are all accepted when using the same writeKey
	// with the standard Basic Auth scheme: Authorization: Basic base64(writeKey:).
	t.Run("Write Key Basic Auth from multiple SDK platforms", func(t *testing.T) {
		sdkPlatforms := []struct {
			libraryName    string
			libraryVersion string
		}{
			{"analytics.js", "1.68.0"},
			{"analytics-ios", "4.1.0"},
			{"analytics-android", "4.10.0"},
			{"analytics-node", "6.2.0"},
		}
		for _, sdk := range sdkPlatforms {
			payload := fmt.Sprintf(`{
				"userId": "multi-sdk-user",
				"anonymousId": "multi-sdk-anon",
				"type": "track",
				"event": "SDK Test Event",
				"properties": {"sdk": "%s"},
				"context": {"library": {"name": "%s", "version": "%s"}},
				"messageId": "multi-sdk-%s",
				"timestamp": "2024-06-01T12:05:00.000Z"
			}`, sdk.libraryName, sdk.libraryName, sdk.libraryVersion, sdk.libraryName)
			sendSDKEvent(t, "/v1/track", payload)
		}
		totalExpectedJobs += 4
		requireJobsCount(t, postgresContainer.DB, "gw", jobsdb.Unprocessed.State, totalExpectedJobs)
	})
}

func runGateway(
	ctx context.Context,
	cancel context.CancelFunc,
	port int,
	postgresContainer *postgres.Resource,
	cbURL, transformerURL, tmpDir string,
) (err error) {
	// first run node migrations
	mg := &migrator.Migrator{
		Handle:                     postgresContainer.DB,
		MigrationsTable:            "node_migrations",
		ShouldForceSetLowerVersion: config.GetBool("SQLMigrator.forceSetLowerVersion", true),
	}

	err = mg.Migrate("node")
	if err != nil {
		return fmt.Errorf("unable to run the migrations for the node, err: %w", err)
	}

	// then start the server
	config.Set("CONFIG_BACKEND_URL", cbURL)
	config.Set("WORKSPACE_TOKEN", "token")
	config.Set("DB.host", postgresContainer.Host)
	config.Set("DB.port", postgresContainer.Port)
	config.Set("DB.user", postgresContainer.User)
	config.Set("DB.name", postgresContainer.Database)
	config.Set("DB.password", postgresContainer.Password)
	config.Set("DEST_TRANSFORM_URL", transformerURL)

	config.Set("APP_TYPE", "gateway")

	config.Set("Gateway.webPort", strconv.Itoa(port))
	config.Set("JobsDB.backup.enabled", false)
	config.Set("JobsDB.migrateDSLoopSleepDuration", "60m")
	config.Set("RUDDER_TMPDIR", os.TempDir())
	config.Set("recovery.storagePath", path.Join(tmpDir, "/recovery_data.json"))
	config.Set("recovery.enabled", false)
	config.Set("Profiler.Enabled", false)
	config.Set("Gateway.enableSuppressUserFeature", false)

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panicked: %v", r)
		}
	}()
	r := runner.New(runner.ReleaseInfo{EnterpriseToken: "DUMMY"})
	c := r.Run(ctx, cancel, []string{"rudder-gw"})
	if c != 0 {
		err = fmt.Errorf("rudder-server exited with a non-0 exit code: %d", c)
	}
	return err
}

// nolint: unparam
func requireJobsCount(
	t *testing.T,
	db *sql.DB,
	queue, state string,
	expectedCount int,
) {
	t.Helper()

	query := fmt.Sprintf(`SELECT count(*) FROM unionjobsdbmetadata('%s',1) WHERE job_state = '%s';`, queue, state)
	if state == jobsdb.Unprocessed.State {
		query = fmt.Sprintf(`SELECT count(*) FROM unionjobsdbmetadata('%s',1) WHERE job_state IS NULL;`, queue)
	}
	require.Eventually(t, func() bool {
		var jobsCount int
		require.NoError(t, db.QueryRow(query).Scan(&jobsCount))
		t.Logf("%s %sJobCount: %d", queue, state, jobsCount)
		return jobsCount == expectedCount
	},
		20*time.Second,
		1*time.Second,
		fmt.Sprintf("%d %s events should be in %s state", expectedCount, queue, state),
	)
}
