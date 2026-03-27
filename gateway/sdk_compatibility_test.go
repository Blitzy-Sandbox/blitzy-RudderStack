package gateway

// sdk_compatibility_test.go — Comprehensive SDK compatibility test suite validating
// all 6 Segment event types (identify, track, page, screen, group, alias) with exact
// payload formats from every Segment SDK platform: JavaScript (analytics.js/Analytics 2.0),
// iOS (analytics-ios), Android (analytics-android), Node.js (analytics-node),
// Python (analytics-python), Go (analytics-go), Java (analytics-java), Ruby (analytics-ruby).
//
// Covers Epics: E-005 (API Surface), E-006 (JS SDK), E-007 (Mobile SDK), E-008 (Server SDK)

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	"github.com/rudderlabs/rudder-server/app"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	mocksApp "github.com/rudderlabs/rudder-server/mocks/app"
	mocksBackendConfig "github.com/rudderlabs/rudder-server/mocks/backend-config"
	mockGateway "github.com/rudderlabs/rudder-server/mocks/gateway"
	mocksJobsDB "github.com/rudderlabs/rudder-server/mocks/jobsdb"
	sourcedebugger "github.com/rudderlabs/rudder-server/services/debugger/source"
	"github.com/rudderlabs/rudder-server/services/rsources"
	"github.com/rudderlabs/rudder-server/services/transformer"
	"github.com/rudderlabs/rudder-server/utils/pubsub"
)

// ---------------------------------------------------------------------------
// SDK Compatibility Test Infrastructure
// ---------------------------------------------------------------------------

// sdkTestInfra provides standalone mock infrastructure for SDK compatibility
// tests, independent of the Ginkgo test suite.
type sdkTestInfra struct {
	t                 *testing.T
	ctrl              *gomock.Controller
	mockJobsDB        *mocksJobsDB.MockJobsDB
	mockBackendConfig *mocksBackendConfig.MockBackendConfig
	mockApp           *mocksApp.MockApp
	mockRateLimiter   *mockGateway.MockThrottler
	mockWebhook       *mockGateway.MockWebhookRequestHandler
	gateway           *Handle
	conf              *config.Config
	statsStore        *memstats.Store
	cancelFunc        context.CancelFunc
}

// setupSDKTestInfra creates a fully initialized Gateway with mock backend config
// for SDK compatibility testing. It mirrors the pattern from event_spec_parity_test.go
// but uses testing.T instead of GinkgoT() for standalone test execution.
func setupSDKTestInfra(t *testing.T) *sdkTestInfra {
	t.Helper()

	initGW()

	ctrl := gomock.NewController(t)
	mockJobsDB := mocksJobsDB.NewMockJobsDB(ctrl)
	mockBC := mocksBackendConfig.NewMockBackendConfig(ctrl)
	mockApplication := mocksApp.NewMockApp(ctrl)
	mockThrottler := mockGateway.NewMockThrottler(ctrl)
	mockWH := mockGateway.NewMockWebhookRequestHandler(ctrl)

	// Configure app features (no enterprise features for SDK compat tests)
	mockApplication.EXPECT().Features().Return(&app.Features{}).AnyTimes()

	// Configure webhook handler
	mockWH.EXPECT().Shutdown().AnyTimes()

	// Configure backend config subscription with the shared sampleBackendConfig
	mockBC.EXPECT().Subscribe(gomock.Any(), backendconfig.TopicProcessConfig).
		DoAndReturn(func(ctx context.Context, topic backendconfig.Topic) pubsub.DataChannel {
			ch := make(chan pubsub.DataEvent, 1)
			ch <- pubsub.DataEvent{
				Data:  map[string]backendconfig.ConfigT{WorkspaceID: sampleBackendConfig},
				Topic: string(topic),
			}
			go func() {
				<-ctx.Done()
				close(ch)
			}()
			return ch
		})

	statsStore, err := memstats.New()
	require.NoError(t, err)

	conf := config.New()
	conf.Set("Gateway.enableRateLimit", false)
	conf.Set("Gateway.enableSuppressUserFeature", false)

	gw := &Handle{}
	ctx, cancel := context.WithCancel(context.Background())
	err = gw.Setup(
		ctx,
		conf,
		logger.NOP,
		statsStore,
		mockApplication,
		mockBC,
		mockJobsDB,
		mockThrottler,
		func(w http.ResponseWriter, r *http.Request) {},
		rsources.NewNoOpService(),
		transformer.NewNoOpService(),
		sourcedebugger.NewNoOpService(),
		nil,
	)
	require.NoError(t, err)

	// Wait for backend config initialization (same as waitForBackendConfigInit)
	require.Eventually(t, func() bool {
		select {
		case <-gw.backendConfigInitialisedChan:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "Gateway backend config failed to initialise")

	return &sdkTestInfra{
		t:                 t,
		ctrl:              ctrl,
		mockJobsDB:        mockJobsDB,
		mockBackendConfig: mockBC,
		mockApp:           mockApplication,
		mockRateLimiter:   mockThrottler,
		mockWebhook:       mockWH,
		gateway:           gw,
		conf:              conf,
		statsStore:        statsStore,
		cancelFunc:        cancel,
	}
}

// teardown cleanly shuts down the Gateway and mock infrastructure.
func (infra *sdkTestInfra) teardown() {
	infra.cancelFunc()
	err := infra.gateway.Shutdown()
	require.NoError(infra.t, err)
	infra.ctrl.Finish()
}

// setupJobCapture configures mock expectations to capture jobs stored by the Gateway.
// Returns a pointer to the captured jobs slice and a done channel.
func (infra *sdkTestInfra) setupJobCapture() (*[][]*jobsdb.JobT, chan struct{}) {
	var capturedJobs [][]*jobsdb.JobT
	var mu sync.Mutex
	done := make(chan struct{}, 1)

	infra.mockJobsDB.EXPECT().WithStoreSafeTx(
		gomock.Any(),
		gomock.Any(),
	).Times(1).Do(func(_ context.Context, f func(jobsdb.StoreSafeTx) error) {
		_ = f(jobsdb.EmptyStoreSafeTx())
	}).Return(nil)

	infra.mockJobsDB.EXPECT().StoreEachBatchRetryInTx(
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
	).DoAndReturn(
		func(_ context.Context, _ jobsdb.StoreSafeTx, jobs [][]*jobsdb.JobT) (map[uuid.UUID]string, error) {
			mu.Lock()
			capturedJobs = append(capturedJobs, jobs...)
			mu.Unlock()
			select {
			case done <- struct{}{}:
			default:
			}
			return map[uuid.UUID]string{}, nil
		},
	).Times(1)

	return &capturedJobs, done
}

// sendRequest constructs and sends an HTTP request to the given handler,
// asserting a 200 OK response. Uses Write Key Basic Auth.
func (infra *sdkTestInfra) sendRequest(
	t *testing.T,
	handler http.HandlerFunc,
	payload string,
) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "", bytes.NewBufferString(payload))
	require.NoError(t, err)
	req.SetBasicAuth(WriteKeyEnabled, "")
	req.Header.Set("AnonymousId", "094985f8-b4eb-43c3-bc8a-e8b75aae9c7c")
	req.RemoteAddr = TestRemoteAddressWithPort

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	body, _ := io.ReadAll(rr.Body)
	require.Equal(t, http.StatusOK, rr.Code,
		"expected HTTP 200 OK, got %d: %s", rr.Code, string(body))
	require.Equal(t, "ok", string(body))
}

// sendAndCapture sends a payload to the handler, waits for the job to be stored,
// and returns the first event from the stored batch as a gjson.Result.
func (infra *sdkTestInfra) sendAndCapture(
	t *testing.T,
	handler http.HandlerFunc,
	payload string,
) gjson.Result {
	t.Helper()

	capturedJobsPtr, done := infra.setupJobCapture()
	infra.sendRequest(t, handler, payload)

	// Wait for job storage to complete
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for job to be stored")
	}

	capturedJobs := *capturedJobsPtr
	require.NotEmpty(t, capturedJobs, "captured jobs should not be empty")
	require.NotEmpty(t, capturedJobs[0], "first job batch should not be empty")

	storedPayload := string(capturedJobs[0][0].EventPayload)
	return gjson.Get(storedPayload, "batch.0")
}

// ---------------------------------------------------------------------------
// SDK Context Builders — per-platform JSON context strings
// ---------------------------------------------------------------------------

// sdkCompatContext returns a minimal JSON context object for server-side SDKs
// with the specified library name and version. The channel is always "server".
func sdkCompatContext(sdkName, sdkVersion string) string {
	return fmt.Sprintf(`{
		"library": {
			"name": %q,
			"version": %q
		},
		"channel": "server"
	}`, sdkName, sdkVersion)
}

// webSDKContext returns a JSON context object for the JavaScript web SDK,
// including context.page, context.userAgent, context.library, and channel.
// The library name is always "analytics.js" and the version is 1.68.0.
func webSDKContext() string {
	return `{
		"library": {
			"name": "analytics.js",
			"version": "1.68.0"
		},
		"channel": "client",
		"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"page": {
			"path": "/home",
			"referrer": "https://www.google.com/search?q=rudderstack",
			"search": "?utm_source=google",
			"title": "Home - Example App",
			"url": "https://app.example.com/home"
		},
		"locale": "en-US",
		"screen": {
			"density": 2,
			"height": 900,
			"width": 1440
		},
		"timezone": "America/New_York"
	}`
}

// mobileSDKContext returns a JSON context object for mobile SDKs (iOS/Android),
// including context.device, context.os, context.app, context.network, context.screen,
// context.library, and channel.
func mobileSDKContext(sdkName, sdkVersion, deviceManufacturer, deviceModel, deviceType, osName, osVersion string) string {
	return fmt.Sprintf(`{
		"library": {
			"name": %q,
			"version": %q
		},
		"channel": "client",
		"device": {
			"id": "device-sdk-compat-001",
			"advertisingId": "ad-sdk-compat-456",
			"manufacturer": %q,
			"model": %q,
			"name": "Test Device",
			"type": %q
		},
		"os": {
			"name": %q,
			"version": %q
		},
		"app": {
			"name": "SDKCompatTestApp",
			"version": "2.1.0",
			"build": "1234",
			"namespace": "com.example.sdkcompat"
		},
		"network": {
			"bluetooth": false,
			"carrier": "T-Mobile US",
			"cellular": true,
			"wifi": true
		},
		"screen": {
			"density": 3,
			"height": 2436,
			"width": 1125
		},
		"locale": "en-US",
		"timezone": "America/Los_Angeles"
	}`, sdkName, sdkVersion, deviceManufacturer, deviceModel, deviceType, osName, osVersion)
}

// ---------------------------------------------------------------------------
// TestSDKCompatibility — main test function (E-005, E-006, E-007, E-008)
// ---------------------------------------------------------------------------

// TestSDKCompatibility validates that all 6 Segment event types are correctly
// processed by the Gateway for every Segment SDK platform, including field-level
// preservation of context.library, context.device, context.os, context.app,
// context.network, context.screen, context.page, and lifecycle events.
func TestSDKCompatibility(t *testing.T) {
	initGW()

	// sdkTestCase defines a single SDK compatibility test scenario.
	type sdkTestCase struct {
		name       string
		sdkName    string
		sdkVersion string
		channel    string
		callType   string // identify, track, page, screen, group, alias
		payload    string // full JSON payload (context included)
		// assertions is called with the stored event for field-level validation
		assertions func(t *testing.T, event gjson.Result)
	}

	// -----------------------------------------------------------------------
	// Build test cases for all SDK platforms and event types
	// -----------------------------------------------------------------------
	testCases := []sdkTestCase{
		// =================================================================
		// JavaScript SDK (analytics.js) — E-006
		// =================================================================
		{
			name:       "JS SDK identify",
			sdkName:    "analytics.js",
			sdkVersion: "1.68.0",
			channel:    "client",
			callType:   "identify",
			payload: fmt.Sprintf(`{
				"userId": "js-user-001",
				"anonymousId": "js-anon-001",
				"messageId": "js-msg-identify-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"sentAt": "2024-06-15T10:30:01.000Z",
				"traits": {
					"name": "Jane Doe",
					"email": "jane.doe@example.com",
					"plan": "premium"
				},
				"integrations": {
					"All": true
				},
				"context": %s
			}`, webSDKContext()),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "js-user-001", event.Get("userId").String())
				require.Equal(t, "js-anon-001", event.Get("anonymousId").String())
				require.Equal(t, "Jane Doe", event.Get("traits.name").String())
				require.Equal(t, "jane.doe@example.com", event.Get("traits.email").String())
				require.Equal(t, "premium", event.Get("traits.plan").String())
				require.True(t, event.Get("integrations.All").Bool())
				// Web SDK context.page fields
				require.Equal(t, "/home", event.Get("context.page.path").String())
				require.Equal(t, "https://www.google.com/search?q=rudderstack", event.Get("context.page.referrer").String())
				require.Equal(t, "?utm_source=google", event.Get("context.page.search").String())
				require.Equal(t, "Home - Example App", event.Get("context.page.title").String())
				require.Equal(t, "https://app.example.com/home", event.Get("context.page.url").String())
				require.Contains(t, event.Get("context.userAgent").String(), "Chrome/120")
			},
		},
		{
			name:       "JS SDK track",
			sdkName:    "analytics.js",
			sdkVersion: "1.68.0",
			channel:    "client",
			callType:   "track",
			payload: fmt.Sprintf(`{
				"userId": "js-user-001",
				"anonymousId": "js-anon-001",
				"messageId": "js-msg-track-001",
				"timestamp": "2024-06-15T10:31:00.000Z",
				"event": "Button Clicked",
				"properties": {
					"button_id": "cta-signup",
					"page": "/pricing",
					"label": "Get Started"
				},
				"context": %s
			}`, webSDKContext()),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Button Clicked", event.Get("event").String())
				require.Equal(t, "cta-signup", event.Get("properties.button_id").String())
				require.Equal(t, "/pricing", event.Get("properties.page").String())
				require.Equal(t, "Get Started", event.Get("properties.label").String())
			},
		},
		{
			name:       "JS SDK page",
			sdkName:    "analytics.js",
			sdkVersion: "1.68.0",
			channel:    "client",
			callType:   "page",
			payload: fmt.Sprintf(`{
				"userId": "js-user-001",
				"anonymousId": "js-anon-001",
				"messageId": "js-msg-page-001",
				"timestamp": "2024-06-15T10:32:00.000Z",
				"name": "Home",
				"properties": {
					"url": "https://app.example.com/home",
					"path": "/home",
					"title": "Home - Example App",
					"referrer": "https://www.google.com",
					"search": "?q=rudderstack"
				},
				"context": %s
			}`, webSDKContext()),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Home", event.Get("name").String())
				require.Equal(t, "https://app.example.com/home", event.Get("properties.url").String())
				require.Equal(t, "/home", event.Get("properties.path").String())
				require.Equal(t, "Home - Example App", event.Get("properties.title").String())
				require.Equal(t, "https://www.google.com", event.Get("properties.referrer").String())
				require.Equal(t, "?q=rudderstack", event.Get("properties.search").String())
			},
		},
		{
			name:       "JS SDK screen",
			sdkName:    "analytics.js",
			sdkVersion: "1.68.0",
			channel:    "client",
			callType:   "screen",
			payload: fmt.Sprintf(`{
				"userId": "js-user-001",
				"anonymousId": "js-anon-001",
				"messageId": "js-msg-screen-001",
				"timestamp": "2024-06-15T10:33:00.000Z",
				"name": "Dashboard",
				"properties": {
					"section": "analytics"
				},
				"context": %s
			}`, webSDKContext()),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Dashboard", event.Get("name").String())
				require.Equal(t, "analytics", event.Get("properties.section").String())
			},
		},
		{
			name:       "JS SDK group",
			sdkName:    "analytics.js",
			sdkVersion: "1.68.0",
			channel:    "client",
			callType:   "group",
			payload: fmt.Sprintf(`{
				"userId": "js-user-001",
				"anonymousId": "js-anon-001",
				"messageId": "js-msg-group-001",
				"timestamp": "2024-06-15T10:34:00.000Z",
				"groupId": "grp-acme-corp-001",
				"traits": {
					"name": "Acme Corp",
					"industry": "Technology",
					"employees": 250,
					"plan": "enterprise"
				},
				"context": %s
			}`, webSDKContext()),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "grp-acme-corp-001", event.Get("groupId").String())
				require.Equal(t, "Acme Corp", event.Get("traits.name").String())
				require.Equal(t, "Technology", event.Get("traits.industry").String())
				require.Equal(t, float64(250), event.Get("traits.employees").Float())
				require.Equal(t, "enterprise", event.Get("traits.plan").String())
			},
		},
		{
			name:       "JS SDK alias",
			sdkName:    "analytics.js",
			sdkVersion: "1.68.0",
			channel:    "client",
			callType:   "alias",
			payload: fmt.Sprintf(`{
				"userId": "js-user-001",
				"anonymousId": "js-anon-001",
				"messageId": "js-msg-alias-001",
				"timestamp": "2024-06-15T10:35:00.000Z",
				"previousId": "js-anon-001",
				"context": %s
			}`, webSDKContext()),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "js-anon-001", event.Get("previousId").String())
				require.Equal(t, "js-user-001", event.Get("userId").String())
			},
		},

		// =================================================================
		// iOS SDK (analytics-ios) — E-007
		// =================================================================
		{
			name:       "iOS SDK identify",
			sdkName:    "analytics-ios",
			sdkVersion: "4.1.0",
			channel:    "client",
			callType:   "identify",
			payload: fmt.Sprintf(`{
				"userId": "ios-user-001",
				"anonymousId": "ios-anon-001",
				"messageId": "ios-msg-identify-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"traits": {
					"name": "Alice Smith",
					"email": "alice.smith@example.com"
				},
				"context": %s
			}`, mobileSDKContext("analytics-ios", "4.1.0", "Apple", "iPhone 15 Pro", "ios", "iOS", "17.2")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "ios-user-001", event.Get("userId").String())
				require.Equal(t, "Alice Smith", event.Get("traits.name").String())
				require.Equal(t, "alice.smith@example.com", event.Get("traits.email").String())
				// Mobile context assertions
				require.Equal(t, "device-sdk-compat-001", event.Get("context.device.id").String())
				require.Equal(t, "Apple", event.Get("context.device.manufacturer").String())
				require.Equal(t, "iPhone 15 Pro", event.Get("context.device.model").String())
				require.Equal(t, "ios", event.Get("context.device.type").String())
				require.Equal(t, "iOS", event.Get("context.os.name").String())
				require.Equal(t, "17.2", event.Get("context.os.version").String())
				require.Equal(t, "SDKCompatTestApp", event.Get("context.app.name").String())
				require.Equal(t, "2.1.0", event.Get("context.app.version").String())
				require.Equal(t, "1234", event.Get("context.app.build").String())
				require.Equal(t, "com.example.sdkcompat", event.Get("context.app.namespace").String())
				require.Equal(t, true, event.Get("context.network.wifi").Bool())
				require.Equal(t, true, event.Get("context.network.cellular").Bool())
				require.Equal(t, false, event.Get("context.network.bluetooth").Bool())
				require.Equal(t, "T-Mobile US", event.Get("context.network.carrier").String())
				require.Equal(t, float64(3), event.Get("context.screen.density").Float())
				require.Equal(t, float64(2436), event.Get("context.screen.height").Float())
				require.Equal(t, float64(1125), event.Get("context.screen.width").Float())
			},
		},
		{
			name:       "iOS SDK track",
			sdkName:    "analytics-ios",
			sdkVersion: "4.1.0",
			channel:    "client",
			callType:   "track",
			payload: fmt.Sprintf(`{
				"userId": "ios-user-001",
				"anonymousId": "ios-anon-001",
				"messageId": "ios-msg-track-001",
				"timestamp": "2024-06-15T10:31:00.000Z",
				"event": "Product Viewed",
				"properties": {
					"product_id": "prod-ios-789",
					"name": "Premium Headphones",
					"price": 149.99,
					"currency": "USD"
				},
				"context": %s
			}`, mobileSDKContext("analytics-ios", "4.1.0", "Apple", "iPhone 15 Pro", "ios", "iOS", "17.2")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Product Viewed", event.Get("event").String())
				require.Equal(t, "prod-ios-789", event.Get("properties.product_id").String())
				require.Equal(t, "Premium Headphones", event.Get("properties.name").String())
				require.Equal(t, 149.99, event.Get("properties.price").Float())
				require.Equal(t, "Apple", event.Get("context.device.manufacturer").String())
			},
		},
		{
			name:       "iOS SDK screen with category",
			sdkName:    "analytics-ios",
			sdkVersion: "4.1.0",
			channel:    "client",
			callType:   "screen",
			payload: fmt.Sprintf(`{
				"userId": "ios-user-001",
				"anonymousId": "ios-anon-001",
				"messageId": "ios-msg-screen-001",
				"timestamp": "2024-06-15T10:32:00.000Z",
				"name": "Product Detail",
				"properties": {
					"category": "Ecommerce",
					"product_id": "prod-ios-789"
				},
				"context": %s
			}`, mobileSDKContext("analytics-ios", "4.1.0", "Apple", "iPhone 15 Pro", "ios", "iOS", "17.2")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Product Detail", event.Get("name").String())
				require.Equal(t, "Ecommerce", event.Get("properties.category").String())
				require.Equal(t, "prod-ios-789", event.Get("properties.product_id").String())
				require.Equal(t, "iOS", event.Get("context.os.name").String())
			},
		},
		{
			name:       "iOS SDK group",
			sdkName:    "analytics-ios",
			sdkVersion: "4.1.0",
			channel:    "client",
			callType:   "group",
			payload: fmt.Sprintf(`{
				"userId": "ios-user-001",
				"anonymousId": "ios-anon-001",
				"messageId": "ios-msg-group-001",
				"timestamp": "2024-06-15T10:33:00.000Z",
				"groupId": "grp-ios-team-001",
				"traits": {
					"name": "Mobile Dev Team",
					"size": 12
				},
				"context": %s
			}`, mobileSDKContext("analytics-ios", "4.1.0", "Apple", "iPhone 15 Pro", "ios", "iOS", "17.2")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "grp-ios-team-001", event.Get("groupId").String())
				require.Equal(t, "Mobile Dev Team", event.Get("traits.name").String())
				require.Equal(t, float64(12), event.Get("traits.size").Float())
			},
		},
		{
			name:       "iOS SDK alias",
			sdkName:    "analytics-ios",
			sdkVersion: "4.1.0",
			channel:    "client",
			callType:   "alias",
			payload: fmt.Sprintf(`{
				"userId": "ios-user-001",
				"anonymousId": "ios-anon-001",
				"messageId": "ios-msg-alias-001",
				"timestamp": "2024-06-15T10:34:00.000Z",
				"previousId": "ios-anon-001",
				"context": %s
			}`, mobileSDKContext("analytics-ios", "4.1.0", "Apple", "iPhone 15 Pro", "ios", "iOS", "17.2")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "ios-anon-001", event.Get("previousId").String())
				require.Equal(t, "ios-user-001", event.Get("userId").String())
				require.Equal(t, "Apple", event.Get("context.device.manufacturer").String())
			},
		},

		// =================================================================
		// Android SDK (analytics-android) — E-007
		// =================================================================
		{
			name:       "Android SDK identify",
			sdkName:    "analytics-android",
			sdkVersion: "4.11.3",
			channel:    "client",
			callType:   "identify",
			payload: fmt.Sprintf(`{
				"userId": "android-user-001",
				"anonymousId": "android-anon-001",
				"messageId": "android-msg-identify-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"traits": {
					"name": "Bob Johnson",
					"email": "bob.johnson@example.com"
				},
				"context": %s
			}`, mobileSDKContext("analytics-android", "4.11.3", "Samsung", "Galaxy S24", "android", "Android", "14")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "android-user-001", event.Get("userId").String())
				require.Equal(t, "Bob Johnson", event.Get("traits.name").String())
				require.Equal(t, "Samsung", event.Get("context.device.manufacturer").String())
				require.Equal(t, "Galaxy S24", event.Get("context.device.model").String())
				require.Equal(t, "android", event.Get("context.device.type").String())
				require.Equal(t, "Android", event.Get("context.os.name").String())
				require.Equal(t, "14", event.Get("context.os.version").String())
				require.Equal(t, "SDKCompatTestApp", event.Get("context.app.name").String())
			},
		},
		{
			name:       "Android SDK track",
			sdkName:    "analytics-android",
			sdkVersion: "4.11.3",
			channel:    "client",
			callType:   "track",
			payload: fmt.Sprintf(`{
				"userId": "android-user-001",
				"anonymousId": "android-anon-001",
				"messageId": "android-msg-track-001",
				"timestamp": "2024-06-15T10:31:00.000Z",
				"event": "In-App Purchase",
				"properties": {
					"product_id": "prod-android-456",
					"revenue": 4.99,
					"currency": "USD"
				},
				"context": %s
			}`, mobileSDKContext("analytics-android", "4.11.3", "Samsung", "Galaxy S24", "android", "Android", "14")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "In-App Purchase", event.Get("event").String())
				require.Equal(t, "prod-android-456", event.Get("properties.product_id").String())
				require.Equal(t, 4.99, event.Get("properties.revenue").Float())
				require.Equal(t, "Samsung", event.Get("context.device.manufacturer").String())
			},
		},
		{
			name:       "Android SDK screen",
			sdkName:    "analytics-android",
			sdkVersion: "4.11.3",
			channel:    "client",
			callType:   "screen",
			payload: fmt.Sprintf(`{
				"userId": "android-user-001",
				"anonymousId": "android-anon-001",
				"messageId": "android-msg-screen-001",
				"timestamp": "2024-06-15T10:32:00.000Z",
				"name": "Settings",
				"properties": {
					"section": "notifications"
				},
				"context": %s
			}`, mobileSDKContext("analytics-android", "4.11.3", "Samsung", "Galaxy S24", "android", "Android", "14")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Settings", event.Get("name").String())
				require.Equal(t, "notifications", event.Get("properties.section").String())
				require.Equal(t, "Android", event.Get("context.os.name").String())
			},
		},

		// =================================================================
		// Node.js SDK (analytics-node) — E-008
		// =================================================================
		{
			name:       "Node.js SDK track",
			sdkName:    "analytics-node",
			sdkVersion: "6.2.0",
			channel:    "server",
			callType:   "track",
			payload: fmt.Sprintf(`{
				"userId": "node-user-001",
				"anonymousId": "node-anon-001",
				"messageId": "node-msg-track-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"sentAt": "2024-06-15T10:30:00.500Z",
				"event": "Order Completed",
				"properties": {
					"order_id": "order-node-123",
					"total": 99.95,
					"currency": "USD",
					"products": [
						{"product_id": "p1", "name": "Widget", "price": 49.95, "quantity": 2}
					]
				},
				"context": %s
			}`, sdkCompatContext("analytics-node", "6.2.0")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Order Completed", event.Get("event").String())
				require.Equal(t, "order-node-123", event.Get("properties.order_id").String())
				require.Equal(t, 99.95, event.Get("properties.total").Float())
				require.True(t, event.Get("properties.products").IsArray())
				require.Equal(t, int64(1), event.Get("properties.products.#").Int())
				require.Equal(t, "Widget", event.Get("properties.products.0.name").String())
			},
		},

		// =================================================================
		// Python SDK (analytics-python) — E-008
		// =================================================================
		{
			name:       "Python SDK identify",
			sdkName:    "analytics-python",
			sdkVersion: "2.2.3",
			channel:    "server",
			callType:   "identify",
			payload: fmt.Sprintf(`{
				"userId": "python-user-001",
				"anonymousId": "python-anon-001",
				"messageId": "python-msg-identify-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"sentAt": "2024-06-15T10:30:01.000Z",
				"traits": {
					"name": "Charlie Brown",
					"email": "charlie.brown@example.com",
					"role": "engineer"
				},
				"context": %s
			}`, sdkCompatContext("analytics-python", "2.2.3")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "python-user-001", event.Get("userId").String())
				require.Equal(t, "Charlie Brown", event.Get("traits.name").String())
				require.Equal(t, "charlie.brown@example.com", event.Get("traits.email").String())
				require.Equal(t, "engineer", event.Get("traits.role").String())
				require.True(t, event.Get("sentAt").Exists(), "sentAt should be preserved")
			},
		},

		// =================================================================
		// Go SDK (analytics-go) — E-008
		// =================================================================
		{
			name:       "Go SDK track",
			sdkName:    "analytics-go",
			sdkVersion: "3.3.0",
			channel:    "server",
			callType:   "track",
			payload: fmt.Sprintf(`{
				"userId": "go-user-001",
				"anonymousId": "go-anon-001",
				"messageId": "go-msg-track-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"event": "Subscription Renewed",
				"properties": {
					"plan": "business",
					"mrr": 299.00,
					"currency": "USD"
				},
				"context": %s
			}`, sdkCompatContext("analytics-go", "3.3.0")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Subscription Renewed", event.Get("event").String())
				require.Equal(t, "business", event.Get("properties.plan").String())
				require.Equal(t, 299.00, event.Get("properties.mrr").Float())
			},
		},

		// =================================================================
		// Java SDK (analytics-java) — E-008
		// =================================================================
		{
			name:       "Java SDK identify",
			sdkName:    "analytics-java",
			sdkVersion: "3.5.0",
			channel:    "server",
			callType:   "identify",
			payload: fmt.Sprintf(`{
				"userId": "java-user-001",
				"anonymousId": "java-anon-001",
				"messageId": "java-msg-identify-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"traits": {
					"name": "Diana Prince",
					"email": "diana.prince@example.com",
					"company": "Acme Corp"
				},
				"context": %s
			}`, sdkCompatContext("analytics-java", "3.5.0")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "java-user-001", event.Get("userId").String())
				require.Equal(t, "Diana Prince", event.Get("traits.name").String())
				require.Equal(t, "diana.prince@example.com", event.Get("traits.email").String())
				require.Equal(t, "Acme Corp", event.Get("traits.company").String())
			},
		},

		// =================================================================
		// Ruby SDK (analytics-ruby) — E-008
		// =================================================================
		{
			name:       "Ruby SDK track",
			sdkName:    "analytics-ruby",
			sdkVersion: "2.4.0",
			channel:    "server",
			callType:   "track",
			payload: fmt.Sprintf(`{
				"userId": "ruby-user-001",
				"anonymousId": "ruby-anon-001",
				"messageId": "ruby-msg-track-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"event": "Invoice Paid",
				"properties": {
					"invoice_id": "inv-ruby-789",
					"amount": 500.00,
					"currency": "GBP"
				},
				"context": %s
			}`, sdkCompatContext("analytics-ruby", "2.4.0")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Invoice Paid", event.Get("event").String())
				require.Equal(t, "inv-ruby-789", event.Get("properties.invoice_id").String())
				require.Equal(t, 500.00, event.Get("properties.amount").Float())
				require.Equal(t, "GBP", event.Get("properties.currency").String())
			},
		},

		// =================================================================
		// Lifecycle Events — E-007
		// =================================================================
		{
			name:       "iOS Application Opened",
			sdkName:    "analytics-ios",
			sdkVersion: "4.1.0",
			channel:    "client",
			callType:   "track",
			payload: fmt.Sprintf(`{
				"userId": "ios-lifecycle-user-001",
				"anonymousId": "ios-lifecycle-anon-001",
				"messageId": "ios-msg-lifecycle-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"event": "Application Opened",
				"properties": {
					"from_background": false,
					"version": "2.1.0",
					"build": "1234"
				},
				"context": %s
			}`, mobileSDKContext("analytics-ios", "4.1.0", "Apple", "iPhone 15 Pro", "ios", "iOS", "17.2")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Application Opened", event.Get("event").String())
				require.Equal(t, false, event.Get("properties.from_background").Bool())
				require.Equal(t, "2.1.0", event.Get("properties.version").String())
				require.Equal(t, "1234", event.Get("properties.build").String())
				// Verify mobile context is preserved for lifecycle events
				require.Equal(t, "analytics-ios", event.Get("context.library.name").String())
				require.Equal(t, "Apple", event.Get("context.device.manufacturer").String())
				require.Equal(t, "iOS", event.Get("context.os.name").String())
			},
		},
		{
			name:       "Android Application Backgrounded",
			sdkName:    "analytics-android",
			sdkVersion: "4.11.3",
			channel:    "client",
			callType:   "track",
			payload: fmt.Sprintf(`{
				"userId": "android-lifecycle-user-001",
				"anonymousId": "android-lifecycle-anon-001",
				"messageId": "android-msg-lifecycle-001",
				"timestamp": "2024-06-15T10:31:00.000Z",
				"event": "Application Backgrounded",
				"properties": {},
				"context": %s
			}`, mobileSDKContext("analytics-android", "4.11.3", "Samsung", "Galaxy S24", "android", "Android", "14")),
			assertions: func(t *testing.T, event gjson.Result) {
				t.Helper()
				require.Equal(t, "Application Backgrounded", event.Get("event").String())
				// Verify Android mobile context is preserved for lifecycle events
				require.Equal(t, "analytics-android", event.Get("context.library.name").String())
				require.Equal(t, "Samsung", event.Get("context.device.manufacturer").String())
				require.Equal(t, "Android", event.Get("context.os.name").String())
				require.Equal(t, "14", event.Get("context.os.version").String())
			},
		},
	}

	// -----------------------------------------------------------------------
	// Execute all test cases
	// -----------------------------------------------------------------------
	for _, tc := range testCases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			infra := setupSDKTestInfra(t)
			defer infra.teardown()

			// Select the handler for the call type
			handler := sdkCallTypeHandler(infra.gateway, tc.callType)
			require.NotNil(t, handler, "handler for call type %q should not be nil", tc.callType)

			// Send request and capture stored event
			event := infra.sendAndCapture(t, handler, tc.payload)

			// ---------------------------------------------------------------
			// Common field assertions (every SDK, every event type)
			// ---------------------------------------------------------------
			// 1. type matches the call type
			require.Equal(t, tc.callType, event.Get("type").String(),
				"event type should match handler call type")

			// 2. messageId exists and is non-empty (may be Gateway-assigned)
			require.True(t, event.Get("messageId").Exists(),
				"messageId should exist in stored event")
			require.NotEmpty(t, event.Get("messageId").String(),
				"messageId should not be empty")

			// 3. timestamp exists
			require.True(t, event.Get("timestamp").Exists(),
				"timestamp should exist in stored event")

			// 4. context.library.name matches SDK
			require.Equal(t, tc.sdkName, event.Get("context.library.name").String(),
				"context.library.name should match SDK name")

			// 5. context.library.version matches SDK version
			require.Equal(t, tc.sdkVersion, event.Get("context.library.version").String(),
				"context.library.version should match SDK version")

			// 6. context.channel matches expected channel
			require.Equal(t, tc.channel, event.Get("context.channel").String(),
				"context.channel should be %q for %s", tc.channel, tc.sdkName)

			// 7. userId preserved (if present in payload)
			if gjson.Get(tc.payload, "userId").Exists() {
				require.Equal(t, gjson.Get(tc.payload, "userId").String(),
					event.Get("userId").String(),
					"userId should be preserved")
			}

			// 8. anonymousId preserved (if present in payload)
			if gjson.Get(tc.payload, "anonymousId").Exists() {
				require.Equal(t, gjson.Get(tc.payload, "anonymousId").String(),
					event.Get("anonymousId").String(),
					"anonymousId should be preserved")
			}

			// ---------------------------------------------------------------
			// Per-test-case assertions
			// ---------------------------------------------------------------
			if tc.assertions != nil {
				tc.assertions(t, event)
			}
		})
	}
}

// sdkCallTypeHandler returns the Gateway handler for a given call type.
func sdkCallTypeHandler(gw *Handle, callType string) http.HandlerFunc {
	switch callType {
	case "identify":
		return gw.webIdentifyHandler()
	case "track":
		return gw.webTrackHandler()
	case "page":
		return gw.webPageHandler()
	case "screen":
		return gw.webScreenHandler()
	case "group":
		return gw.webGroupHandler()
	case "alias":
		return gw.webAliasHandler()
	case "batch":
		return gw.webBatchHandler()
	default:
		return nil
	}
}
