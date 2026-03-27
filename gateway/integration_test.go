package gateway

import (
	"context"
	b64 "encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ory/dockertest/v3"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	kithelper "github.com/rudderlabs/rudder-go-kit/testhelper"
	"github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/postgres"
	transformertest "github.com/rudderlabs/rudder-go-kit/testhelper/docker/resource/transformer"
	"github.com/rudderlabs/rudder-go-kit/testhelper/rand"
	"github.com/rudderlabs/rudder-server/app"
	"github.com/rudderlabs/rudder-server/testhelper"
	"github.com/rudderlabs/rudder-server/testhelper/health"
	"github.com/rudderlabs/rudder-server/testhelper/rudderserver"
	whUtil "github.com/rudderlabs/rudder-server/testhelper/webhook"
	"github.com/rudderlabs/rudder-server/utils/httputil"
)

func TestGatewayIntegration(t *testing.T) {
	rsBinaryPath := filepath.Join(t.TempDir(), "rudder-server-binary")
	rudderserver.BuildRudderServerBinary(t, "../main.go", rsBinaryPath)

	for _, appType := range []string{app.GATEWAY, app.EMBEDDED} {
		t.Run(appType, func(t *testing.T) {
			testGatewayByAppType(t, appType, rsBinaryPath)
		})
	}
}

func testGatewayByAppType(t *testing.T, appType, rsBinaryPath string) {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	var (
		group                errgroup.Group
		postgresContainer    *postgres.Resource
		transformerContainer *transformertest.Resource
		workspaceToken       = "workspace-token"
	)

	group.Go(func() (err error) {
		postgresContainer, err = postgres.Setup(pool, t)
		if err != nil {
			return fmt.Errorf("could not start postgres: %v", err)
		}
		return nil
	})
	group.Go(func() (err error) {
		transformerContainer, err = transformertest.Setup(pool, t)
		if err != nil {
			return fmt.Errorf("could not start transformer: %v", err)
		}
		return nil
	})
	require.NoError(t, group.Wait())

	webhook := whUtil.NewRecorder()
	t.Cleanup(webhook.Close)

	writeKey := rand.String(27)
	workspaceID := rand.String(27)
	marshalledWorkspaces := testhelper.FillTemplateAndReturn(t, "../integration_test/multi_tenant_test/testdata/mtGatewayTest02.json", map[string]string{
		"writeKey":    writeKey,
		"workspaceId": workspaceID,
		"webhookUrl":  webhook.Server.URL,
	})
	require.NoError(t, err)
	sourceID := "xxxyyyzzEaEurW247ad9WYZLUyk" // sourceID from the workspace config template

	beConfigRouter := chi.NewMux()
	if testing.Verbose() {
		beConfigRouter.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Logf("BackendConfig server call: %+v", r)
				next.ServeHTTP(w, r)
			})
		})
	}

	backedConfigHandler := func(w http.ResponseWriter, r *http.Request) {
		u, _, ok := r.BasicAuth()
		require.True(t, ok, "Auth should be present")
		require.Equalf(t, workspaceToken, u,
			"Expected HTTP basic authentication to be %q, got %q instead",
			workspaceToken, u)

		n, err := w.Write(marshalledWorkspaces.Bytes())
		require.NoError(t, err)
		require.Equal(t, marshalledWorkspaces.Len(), n)
	}
	controlPlaneHandler := func(w http.ResponseWriter, r *http.Request) {}

	beConfigRouter.Get("/workspaceConfig", backedConfigHandler)
	beConfigRouter.Post("/data-plane/v1/workspaces/{workspaceID}/settings", controlPlaneHandler)
	beConfigRouter.NotFound(func(w http.ResponseWriter, r *http.Request) {
		require.FailNowf(t, "backend config", "unexpected request to backend config, not found: %+v", r.URL)
		w.WriteHeader(http.StatusNotFound)
	})

	backendConfigSrv := httptest.NewServer(beConfigRouter)
	t.Logf("BackendConfig server listening on: %s", backendConfigSrv.URL)
	t.Cleanup(backendConfigSrv.Close)

	httpPort, err := kithelper.GetFreePort()
	require.NoError(t, err)
	debugPort, err := kithelper.GetFreePort()
	require.NoError(t, err)

	rudderTmpDir, err := os.MkdirTemp("", "rudder_server_*_test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(rudderTmpDir) })

	releaseName := t.Name() + "_" + appType
	envArr := []string{
		fmt.Sprintf("APP_TYPE=%s", appType),
		fmt.Sprintf("INSTANCE_ID=%s", "rudderstackmt-v0-rudderstack-0"),
		fmt.Sprintf("RELEASE_NAME=%s", releaseName),
		fmt.Sprintf("JOBS_DB_HOST=%s", postgresContainer.Host),
		fmt.Sprintf("JOBS_DB_PORT=%s", postgresContainer.Port),
		fmt.Sprintf("JOBS_DB_USER=%s", postgresContainer.User),
		fmt.Sprintf("JOBS_DB_DB_NAME=%s", postgresContainer.Database),
		fmt.Sprintf("JOBS_DB_PASSWORD=%s", postgresContainer.Password),
		fmt.Sprintf("CONFIG_BACKEND_URL=%s", backendConfigSrv.URL),
		fmt.Sprintf("RSERVER_GATEWAY_WEB_PORT=%d", httpPort),
		fmt.Sprintf("RSERVER_PROFILER_PORT=%d", debugPort),
		fmt.Sprintf("RSERVER_ENABLE_STATS=%s", "false"),
		fmt.Sprintf("RUDDER_TMPDIR=%s", rudderTmpDir),
		fmt.Sprintf("DEST_TRANSFORM_URL=%s", transformerContainer.TransformerURL),
		fmt.Sprintf("WORKSPACE_TOKEN=%s", workspaceToken),
	}
	if testing.Verbose() {
		envArr = append(envArr, "LOG_LEVEL=debug")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		g := &errgroup.Group{}
		envs := append(append([]string{}, os.Environ()...), envArr...)
		rudderserver.StartRudderServer(t, ctx, g, "gw-"+strings.ToLower(appType), rsBinaryPath, map[string]string{}, envs...)
		err := g.Wait()
		if err != nil {
			t.Errorf("Error running rudder-server: %v", err)
			return
		}
		t.Log("rudder-server exited")
	}()
	t.Cleanup(func() { cancel(); <-done })

	healthEndpoint := fmt.Sprintf("http://localhost:%d/health", httpPort)
	resp, err := http.Get(healthEndpoint)
	require.ErrorContains(t, err, "connection refused")
	require.Nil(t, resp)
	defer func() { httputil.CloseResponse(resp) }()

	// Checking now that the configuration has been processed and the server can start
	t.Log("Checking health endpoint at", healthEndpoint)
	health.WaitUntilReady(ctx, t,
		healthEndpoint,
		3*time.Minute,
		100*time.Millisecond,
		t.Name(),
	)

	cleanupGwJobs := func() {
		_, _ = postgresContainer.DB.ExecContext(ctx, `DELETE FROM gw_job_status_1 WHERE job_id in (SELECT job_id from gw_jobs_1 WHERE workspace_id = $1)`, workspaceID)
		_, _ = postgresContainer.DB.ExecContext(ctx, `DELETE FROM gw_jobs_1 WHERE workspace_id = $1`, workspaceID)
	}

	// Test basic Gateway happy path
	t.Run("events are received in gateway", func(t *testing.T) {
		require.Empty(t, webhook.Requests(), "webhook should have no requests before sending the events")
		sendEventsToGateway(t, httpPort, writeKey, sourceID, workspaceID)
		t.Cleanup(cleanupGwJobs)

		var (
			eventPayload string
			message      map[string]any
		)
		require.Eventually(t, func() bool {
			return postgresContainer.DB.QueryRowContext(ctx,
				"SELECT event_payload FROM gw_jobs_1 WHERE workspace_id = $1", workspaceID,
			).Scan(&eventPayload) == nil
		}, time.Minute, 50*time.Millisecond)
		require.NoError(t, jsonrs.Unmarshal([]byte(eventPayload), &message))

		var userId string
		err = postgresContainer.DB.QueryRowContext(ctx,
			"SELECT user_id FROM gw_jobs_1 WHERE workspace_id = $1", workspaceID,
		).Scan(&userId)
		require.NoError(t, err)
		require.Equal(t, "anonymousId_header<<>>anonymousId_1<<>>identified_user_id", userId)

		batch, ok := message["batch"].([]any)
		require.True(t, ok)
		require.Len(t, batch, 1)
		require.Equal(t, message["writeKey"], writeKey)
		for _, msg := range batch {
			m, ok := msg.(map[string]any)
			require.True(t, ok)
			require.Equal(t, "anonymousId_1", m["anonymousId"])
			require.Equal(t, "identified_user_id", m["userId"])
			require.Equal(t, "identify", m["type"])
			require.Equal(t, "1", m["eventOrderNo"])
			require.Equal(t, "messageId_1", m["messageId"])
		}

		// Only the Gateway is running, so we don't expect any destinations to be hit.
		require.Empty(t, webhook.Requests(), "webhook should have no requests because there is no processor")
	})

	if appType == app.EMBEDDED {
		// Trigger normal mode for the processor to start
		t.Run("switch to normal mode", func(t *testing.T) {
			sendEventsToGateway(t, httpPort, writeKey, sourceID, workspaceID)
			t.Cleanup(cleanupGwJobs)

			require.Eventuallyf(t, func() bool {
				return webhook.RequestsCount() == 2
			}, 60*time.Second, 100*time.Millisecond, "Webhook should have received %d requests on %d", webhook.RequestsCount(), httpPort)
		})

		// Trigger degraded mode, the Gateway should still work
		t.Run("switch to degraded mode", func(t *testing.T) {
			sendEventsToGateway(t, httpPort, writeKey, sourceID, workspaceID)
			t.Cleanup(cleanupGwJobs)

			var count int
			err = postgresContainer.DB.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM gw_jobs_1 WHERE workspace_id = $1", workspaceID,
			).Scan(&count)
			require.NoError(t, err)
			require.Equal(t, 2, count)

			var userId string
			err = postgresContainer.DB.QueryRowContext(ctx,
				"SELECT user_id FROM gw_jobs_1 WHERE workspace_id = $1", workspaceID,
			).Scan(&userId)
			require.NoError(t, err)
			require.Equal(t, "anonymousId_header<<>>anonymousId_1<<>>identified_user_id", userId)
		})
	}
}

func sendEventsToGateway(t *testing.T, httpPort int, writeKey, sourceID, workspaceID string) {
	event := `{
		"userId": "identified_user_id",
		"anonymousId":"anonymousId_1",
		"messageId":"messageId_1",
		"type": "identify",
		"eventOrderNo":"1",
		"context": {
			"traits": {
				"trait1": "new-val"
			},
			"ip": "14.5.67.21",
			"library": {
				"name": "http"
			}
		},
		"timestamp": "2020-02-02T00:23:09.544Z",
		"rudderId": "some-rudder-id",
		"request_ip": "[::1]",
		"receivedAt": "2024-01-01T01:01:01.000000001Z"
	}`
	payload1 := strings.NewReader(event)
	sendEvent(t, httpPort, payload1, "identify", writeKey)
	internalBatchPayload := fmt.Sprintf(`[{
			"properties": {
				"requestType": "track",
				"messageID": "messageID",
				"routingKey": "anonymousId_header<<>>anonymousId_1<<>>identified_user_id",
				"workspaceID": %q,
				"userID": "identified_user_id",
				"sourceID": %q,
				"sourceJobRunID": "sourceJobRunID",
				"sourceTaskRunID": "sourceTaskRunID",
				"receivedAt": "2024-01-01T01:01:01.000000001Z",
				"requestIP": "1.1.1.1",
				"traceID": "traceID"
			},
			"payload": %s
			}]`, workspaceID, sourceID, event)
	payload2 := strings.NewReader(internalBatchPayload)
	sendInternalBatch(t, httpPort, payload2)
}

func sendEvent(t *testing.T, httpPort int, payload *strings.Reader, callType, writeKey string) {
	t.Helper()
	t.Logf("Sending %s Event", callType)

	var (
		httpClient = &http.Client{}
		method     = "POST"
		url        = fmt.Sprintf("http://localhost:%d/v1/%s", httpPort, callType)
	)

	req, err := http.NewRequest(method, url, payload)
	require.NoError(t, err)

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", b64.StdEncoding.EncodeToString(
		fmt.Appendf(nil, "%s:", writeKey),
	)))
	req.Header.Add("AnonymousId", "anonymousId_header")

	res, err := httpClient.Do(req)
	require.NoError(t, err)
	defer func() { httputil.CloseResponse(res) }()

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)

	t.Logf("Event Sent Successfully: (%s)", body)
}

func sendInternalBatch(t *testing.T, httpPort int, payload *strings.Reader) {
	t.Helper()
	t.Logf("Sending Internal Batch")

	var (
		httpClient = &http.Client{}
		method     = "POST"
		url        = fmt.Sprintf("http://localhost:%d/internal/v1/batch", httpPort)
	)

	req, err := http.NewRequest(method, url, payload)
	require.NoError(t, err)

	req.Header.Add("Content-Type", "application/json")

	res, err := httpClient.Do(req)
	require.NoError(t, err)
	defer func() { httputil.CloseResponse(res) }()

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)

	t.Logf("Internal Batch Sent Successfully: (%s)", body)
}

// sendSDKEvent constructs a payload with SDK-specific context.library metadata and sends
// it to the Gateway /v1/{callType} endpoint. It returns the HTTP status code received.
// The extraContext parameter allows injecting additional context fields (e.g., mobile
// device info, page info) as a raw JSON fragment that is merged into the context object.
func sendSDKEvent(
	t *testing.T,
	httpPort int,
	callType, writeKey,
	sdkName, sdkVersion, channel string,
	extraContext string,
) int {
	t.Helper()
	t.Logf("Sending SDK %s event (library=%s/%s, channel=%s)", callType, sdkName, sdkVersion, channel)

	// Build extra context fragment. If the caller supplied additional context fields we
	// prepend a comma so the fragment can be spliced into the outer context JSON object.
	ctxExtra := ""
	if extraContext != "" {
		ctxExtra = ", " + extraContext
	}

	payload := fmt.Sprintf(`{
		"userId": "sdk-test-user-001",
		"anonymousId": "sdk-test-anon-001",
		"messageId": "sdk-msg-%s-%s",
		"type": %q,
		"context": {
			"channel": %q,
			"library": {
				"name": %q,
				"version": %q
			}%s
		},
		"timestamp": "2024-06-15T10:30:00.000Z",
		"sentAt": "2024-06-15T10:30:01.000Z"
	}`, sdkName, callType, callType, channel, sdkName, sdkVersion, ctxExtra)

	var (
		httpClient = &http.Client{}
		method     = "POST"
		url        = fmt.Sprintf("http://localhost:%d/v1/%s", httpPort, callType)
	)

	req, err := http.NewRequest(method, url, strings.NewReader(payload))
	require.NoError(t, err)

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", b64.StdEncoding.EncodeToString(
		fmt.Appendf(nil, "%s:", writeKey),
	)))
	req.Header.Add("AnonymousId", "sdk-test-anon-001")

	res, err := httpClient.Do(req)
	require.NoError(t, err)
	defer func() { httputil.CloseResponse(res) }()

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	t.Logf("SDK %s event response: status=%d body=%s", callType, res.StatusCode, string(body))
	return res.StatusCode
}

// sendSDKBatchEvent sends a batch payload to /v1/batch with multiple events, each carrying
// SDK-specific context.library metadata. This validates the server-side SDK batch pattern.
func sendSDKBatchEvent(
	t *testing.T,
	httpPort int,
	writeKey, sdkName, sdkVersion string,
	batchJSON string,
) int {
	t.Helper()
	t.Logf("Sending SDK batch event (library=%s/%s)", sdkName, sdkVersion)

	var (
		httpClient = &http.Client{}
		method     = "POST"
		url        = fmt.Sprintf("http://localhost:%d/v1/batch", httpPort)
	)

	req, err := http.NewRequest(method, url, strings.NewReader(batchJSON))
	require.NoError(t, err)

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", b64.StdEncoding.EncodeToString(
		fmt.Appendf(nil, "%s:", writeKey),
	)))

	res, err := httpClient.Do(req)
	require.NoError(t, err)
	defer func() { httputil.CloseResponse(res) }()

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	t.Logf("SDK batch event response: status=%d body=%s", res.StatusCode, string(body))
	return res.StatusCode
}

// TestGatewaySDKCompatibility validates that the Gateway correctly accepts payloads
// formatted by each major Segment SDK platform. It builds the rudder-server binary,
// provisions Docker containers (PostgreSQL + Transformer), starts the server, and
// sends SDK-specific payloads through the /v1/{type} and /v1/batch endpoints.
//
// This test covers E-005 (API surface validation), E-006 (JavaScript SDK), E-007
// (iOS/Android mobile SDKs), and E-008 (server-side SDKs: Node.js, Python, Go, Java, Ruby).
func TestGatewaySDKCompatibility(t *testing.T) {
	// --- binary build (shared across subtests) ---
	rsBinaryPath := filepath.Join(t.TempDir(), "rudder-server-binary")
	rudderserver.BuildRudderServerBinary(t, "../main.go", rsBinaryPath)

	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	// --- Docker infrastructure ---
	pool, err := dockertest.NewPool("")
	require.NoError(t, err)

	var (
		group                errgroup.Group
		postgresContainer    *postgres.Resource
		transformerContainer *transformertest.Resource
		workspaceToken       = "sdk-compat-workspace-token"
	)

	group.Go(func() (err error) {
		postgresContainer, err = postgres.Setup(pool, t)
		if err != nil {
			return fmt.Errorf("could not start postgres: %v", err)
		}
		return nil
	})
	group.Go(func() (err error) {
		transformerContainer, err = transformertest.Setup(pool, t)
		if err != nil {
			return fmt.Errorf("could not start transformer: %v", err)
		}
		return nil
	})
	require.NoError(t, group.Wait())

	webhook := whUtil.NewRecorder()
	t.Cleanup(webhook.Close)

	writeKey := rand.String(27)
	workspaceID := rand.String(27)

	marshalledWorkspaces := testhelper.FillTemplateAndReturn(t,
		"../integration_test/multi_tenant_test/testdata/mtGatewayTest02.json",
		map[string]string{
			"writeKey":    writeKey,
			"workspaceId": workspaceID,
			"webhookUrl":  webhook.Server.URL,
		},
	)

	// --- Backend config mock ---
	beConfigRouter := chi.NewMux()
	if testing.Verbose() {
		beConfigRouter.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Logf("BackendConfig server call: %+v", r)
				next.ServeHTTP(w, r)
			})
		})
	}

	beConfigRouter.Get("/workspaceConfig", func(w http.ResponseWriter, r *http.Request) {
		u, _, ok := r.BasicAuth()
		require.True(t, ok, "Auth should be present")
		require.Equalf(t, workspaceToken, u,
			"Expected HTTP basic authentication to be %q, got %q instead",
			workspaceToken, u)
		n, err := w.Write(marshalledWorkspaces.Bytes())
		require.NoError(t, err)
		require.Equal(t, marshalledWorkspaces.Len(), n)
	})
	beConfigRouter.Post("/data-plane/v1/workspaces/{workspaceID}/settings", func(w http.ResponseWriter, r *http.Request) {})
	beConfigRouter.NotFound(func(w http.ResponseWriter, r *http.Request) {
		require.FailNowf(t, "backend config", "unexpected request to backend config, not found: %+v", r.URL)
		w.WriteHeader(http.StatusNotFound)
	})

	backendConfigSrv := httptest.NewServer(beConfigRouter)
	t.Logf("BackendConfig server listening on: %s", backendConfigSrv.URL)
	t.Cleanup(backendConfigSrv.Close)

	httpPort, err := kithelper.GetFreePort()
	require.NoError(t, err)
	debugPort, err := kithelper.GetFreePort()
	require.NoError(t, err)

	rudderTmpDir, err := os.MkdirTemp("", "rudder_server_sdk_compat_*_test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(rudderTmpDir) })

	envArr := []string{
		fmt.Sprintf("APP_TYPE=%s", app.GATEWAY),
		fmt.Sprintf("INSTANCE_ID=%s", "sdk-compat-instance-0"),
		fmt.Sprintf("RELEASE_NAME=%s", t.Name()),
		fmt.Sprintf("JOBS_DB_HOST=%s", postgresContainer.Host),
		fmt.Sprintf("JOBS_DB_PORT=%s", postgresContainer.Port),
		fmt.Sprintf("JOBS_DB_USER=%s", postgresContainer.User),
		fmt.Sprintf("JOBS_DB_DB_NAME=%s", postgresContainer.Database),
		fmt.Sprintf("JOBS_DB_PASSWORD=%s", postgresContainer.Password),
		fmt.Sprintf("CONFIG_BACKEND_URL=%s", backendConfigSrv.URL),
		fmt.Sprintf("RSERVER_GATEWAY_WEB_PORT=%d", httpPort),
		fmt.Sprintf("RSERVER_PROFILER_PORT=%d", debugPort),
		fmt.Sprintf("RSERVER_ENABLE_STATS=%s", "false"),
		fmt.Sprintf("RUDDER_TMPDIR=%s", rudderTmpDir),
		fmt.Sprintf("DEST_TRANSFORM_URL=%s", transformerContainer.TransformerURL),
		fmt.Sprintf("WORKSPACE_TOKEN=%s", workspaceToken),
	}
	if testing.Verbose() {
		envArr = append(envArr, "LOG_LEVEL=debug")
	}

	// --- Start rudder-server ---
	done := make(chan struct{})
	go func() {
		defer close(done)
		g := &errgroup.Group{}
		envs := append(append([]string{}, os.Environ()...), envArr...)
		rudderserver.StartRudderServer(t, ctx, g, "sdk-compat", rsBinaryPath, map[string]string{}, envs...)
		if err := g.Wait(); err != nil {
			t.Errorf("Error running rudder-server: %v", err)
			return
		}
		t.Log("rudder-server exited")
	}()
	t.Cleanup(func() { cancel(); <-done })

	healthEndpoint := fmt.Sprintf("http://localhost:%d/health", httpPort)
	t.Log("Checking health endpoint at", healthEndpoint)
	health.WaitUntilReady(ctx, t,
		healthEndpoint,
		3*time.Minute,
		100*time.Millisecond,
		t.Name(),
	)

	// -----------------------------------------------------------------------
	// SDK Compatibility Subtests
	// -----------------------------------------------------------------------

	// 1. Segment JavaScript SDK payload format via /v1/track
	t.Run("Segment JS SDK payload format via /v1/track", func(t *testing.T) {
		jsPageContext := `
			"page": {
				"path": "/academy/",
				"referrer": "https://www.google.com/",
				"search": "",
				"title": "Analytics Academy",
				"url": "https://segment.com/academy/"
			},
			"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
		`
		statusCode := sendSDKEvent(t, httpPort, "track", writeKey,
			"analytics.js", "1.68.0", "client", jsPageContext)
		require.Equal(t, http.StatusOK, statusCode,
			"Gateway should accept analytics.js track payload")
	})

	// 2. Segment iOS SDK payload format via /v1/identify
	t.Run("Segment iOS SDK payload format via /v1/identify", func(t *testing.T) {
		iosContext := `
			"device": {
				"id": "B5372DB0-C21E-11E4-8DFC-AA07A5B093DB",
				"manufacturer": "Apple",
				"model": "iPhone15,2",
				"name": "iPhone",
				"type": "ios"
			},
			"os": {
				"name": "iOS",
				"version": "17.5.1"
			},
			"app": {
				"name": "TestApp",
				"version": "2.5.0",
				"build": "1250",
				"namespace": "com.example.testapp"
			},
			"network": {
				"bluetooth": false,
				"carrier": "T-Mobile",
				"cellular": true,
				"wifi": true
			},
			"screen": {
				"density": 3.0,
				"height": 2556,
				"width": 1179
			},
			"traits": {
				"email": "ios-user@example.com",
				"name": "iOS Test User"
			}
		`
		statusCode := sendSDKEvent(t, httpPort, "identify", writeKey,
			"analytics-ios", "4.1.0", "mobile", iosContext)
		require.Equal(t, http.StatusOK, statusCode,
			"Gateway should accept analytics-ios identify payload with mobile context")
	})

	// 3. Segment Android SDK payload format via /v1/screen
	t.Run("Segment Android SDK payload format via /v1/screen", func(t *testing.T) {
		androidContext := `
			"device": {
				"id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
				"manufacturer": "Google",
				"model": "Pixel 8 Pro",
				"name": "Pixel 8 Pro",
				"type": "android"
			},
			"os": {
				"name": "Android",
				"version": "14"
			},
			"app": {
				"name": "TestApp",
				"version": "3.1.0",
				"build": "310",
				"namespace": "com.example.testapp"
			},
			"network": {
				"bluetooth": true,
				"carrier": "Verizon",
				"cellular": true,
				"wifi": false
			},
			"screen": {
				"density": 3.5,
				"height": 2400,
				"width": 1080
			}
		`
		statusCode := sendSDKEvent(t, httpPort, "screen", writeKey,
			"analytics-android", "4.11.3", "mobile", androidContext)
		require.Equal(t, http.StatusOK, statusCode,
			"Gateway should accept analytics-android screen payload with mobile context")
	})

	// 4. Segment Node.js SDK batch via /v1/batch
	t.Run("Segment Node.js SDK batch via /v1/batch", func(t *testing.T) {
		batchPayload := `{
			"batch": [
				{
					"type": "identify",
					"userId": "node-user-001",
					"anonymousId": "node-anon-001",
					"messageId": "node-batch-msg-1",
					"traits": {
						"email": "node-user@example.com",
						"plan": "enterprise"
					},
					"context": {
						"library": {
							"name": "analytics-node",
							"version": "6.2.0"
						}
					},
					"timestamp": "2024-06-15T12:00:00.000Z"
				},
				{
					"type": "track",
					"userId": "node-user-001",
					"anonymousId": "node-anon-001",
					"messageId": "node-batch-msg-2",
					"event": "Order Completed",
					"properties": {
						"orderId": "ORD-12345",
						"revenue": 99.99,
						"currency": "USD"
					},
					"context": {
						"library": {
							"name": "analytics-node",
							"version": "6.2.0"
						}
					},
					"timestamp": "2024-06-15T12:00:01.000Z"
				}
			],
			"sentAt": "2024-06-15T12:00:02.000Z"
		}`
		statusCode := sendSDKBatchEvent(t, httpPort, writeKey,
			"analytics-node", "6.2.0", batchPayload)
		require.Equal(t, http.StatusOK, statusCode,
			"Gateway should accept analytics-node batch payload with mixed identify+track events")
	})

	// 5. All SDK platforms sequential — one event each from 8 SDK platforms
	t.Run("All SDK platforms sequential", func(t *testing.T) {
		type sdkPlatform struct {
			name    string
			version string
			channel string
			call    string
		}
		platforms := []sdkPlatform{
			{name: "analytics.js", version: "1.68.0", channel: "client", call: "track"},
			{name: "analytics-ios", version: "4.1.0", channel: "mobile", call: "identify"},
			{name: "analytics-android", version: "4.11.3", channel: "mobile", call: "screen"},
			{name: "analytics-node", version: "6.2.0", channel: "server", call: "track"},
			{name: "analytics-python", version: "2.2.3", channel: "server", call: "identify"},
			{name: "analytics-go", version: "3.5.0", channel: "server", call: "track"},
			{name: "analytics-java", version: "3.4.0", channel: "server", call: "group"},
			{name: "analytics-ruby", version: "2.5.0", channel: "server", call: "page"},
		}

		for _, p := range platforms {
			t.Run(fmt.Sprintf("%s/%s", p.name, p.call), func(t *testing.T) {
				statusCode := sendSDKEvent(t, httpPort, p.call, writeKey,
					p.name, p.version, p.channel, "")
				require.Equal(t, http.StatusOK, statusCode,
					"Gateway should accept %s %s payload", p.name, p.call)
			})
		}
	})

	// 6. Lifecycle event via mobile SDK — Application Opened
	t.Run("lifecycle event via mobile SDK", func(t *testing.T) {
		// Application Opened is a standard Segment lifecycle event tracked automatically
		// by analytics-ios and analytics-android. It uses the "track" call type with a
		// specific event name and includes mobile-specific context fields.
		iosLifecycleContext := `
			"device": {
				"id": "B5372DB0-C21E-11E4-8DFC-AA07A5B093DB",
				"manufacturer": "Apple",
				"model": "iPhone15,2",
				"name": "iPhone",
				"type": "ios"
			},
			"os": {
				"name": "iOS",
				"version": "17.5.1"
			},
			"app": {
				"name": "TestApp",
				"version": "2.5.0",
				"build": "1250",
				"namespace": "com.example.testapp"
			}
		`
		// Build a lifecycle-specific payload with the "Application Opened" event name
		lifecyclePayload := fmt.Sprintf(`{
			"userId": "sdk-lifecycle-user-001",
			"anonymousId": "sdk-lifecycle-anon-001",
			"messageId": "sdk-lifecycle-msg-001",
			"type": "track",
			"event": "Application Opened",
			"properties": {
				"from_background": false,
				"version": "2.5.0",
				"build": "1250"
			},
			"context": {
				"channel": "mobile",
				"library": {
					"name": "analytics-ios",
					"version": "4.1.0"
				},
				%s
			},
			"timestamp": "2024-06-15T14:00:00.000Z",
			"sentAt": "2024-06-15T14:00:01.000Z"
		}`, iosLifecycleContext)

		var (
			httpClient = &http.Client{}
			method     = "POST"
			url        = fmt.Sprintf("http://localhost:%d/v1/track", httpPort)
		)

		req, err := http.NewRequest(method, url, strings.NewReader(lifecyclePayload))
		require.NoError(t, err)

		req.Header.Add("Content-Type", "application/json")
		req.Header.Add("Authorization", fmt.Sprintf("Basic %s", b64.StdEncoding.EncodeToString(
			fmt.Appendf(nil, "%s:", writeKey),
		)))
		req.Header.Add("AnonymousId", "sdk-lifecycle-anon-001")

		res, err := httpClient.Do(req)
		require.NoError(t, err)
		defer func() { httputil.CloseResponse(res) }()

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode,
			"Gateway should accept iOS lifecycle 'Application Opened' track event, got body: %s", string(body))
	})
}
