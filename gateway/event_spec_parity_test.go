package gateway

// event_spec_parity_test.go — Comprehensive field-level parity validation for all
// six Segment Spec event types (identify, track, page, screen, group, alias).
// This validates E-001 and E-003 from the Event Spec Parity sprint, ensuring that
// the RudderStack Gateway preserves every field defined in the Segment Spec
// (refs/segment-docs/src/connections/spec/) through the ingestion pipeline.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	"github.com/rudderlabs/rudder-server/jobsdb"
	sourcedebugger "github.com/rudderlabs/rudder-server/services/debugger/source"
	"github.com/rudderlabs/rudder-server/services/rsources"
	"github.com/rudderlabs/rudder-server/services/transformer"

	"go.uber.org/mock/gomock"
)

// ---------------------------------------------------------------------------
// Helper: specParityContext builds the full Segment Spec context JSON object
// with the specified channel value. This includes all 18+ standard context
// fields defined in common.md, plus Client Hints (ES-001 userAgentData).
// IP addresses use RFC 5737 TEST-NET range (203.0.113.0/24).
// ---------------------------------------------------------------------------
func specParityContext(channel string) string {
	return fmt.Sprintf(`{
		"active": true,
		"app": {
			"name": "TestApp",
			"version": "2.1.0",
			"build": "1234"
		},
		"campaign": {
			"name": "spring_sale",
			"source": "google",
			"medium": "cpc",
			"term": "analytics",
			"content": "banner_1"
		},
		"device": {
			"id": "device-abc-123",
			"advertisingId": "ad-id-456",
			"manufacturer": "Apple",
			"model": "iPhone 15",
			"name": "Test iPhone",
			"type": "ios",
			"version": "17.2"
		},
		"ip": "203.0.113.50",
		"library": {
			"name": "analytics.js",
			"version": "2.1.0"
		},
		"locale": "en-US",
		"network": {
			"bluetooth": false,
			"carrier": "T-Mobile",
			"cellular": true,
			"wifi": true
		},
		"os": {
			"name": "iOS",
			"version": "17.2"
		},
		"page": {
			"path": "/products/widget",
			"referrer": "https://www.google.com",
			"search": "?q=widget",
			"title": "Widget Page",
			"url": "https://example.com/products/widget"
		},
		"referrer": {
			"type": "search",
			"name": "Google",
			"url": "https://www.google.com",
			"link": "https://www.google.com/search?q=example"
		},
		"screen": {
			"density": 2,
			"height": 1920,
			"width": 1080
		},
		"timezone": "America/Los_Angeles",
		"groupId": "grp-999",
		"traits": {
			"email": "test@example.com"
		},
		"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Safari/537.36",
		"userAgentData": {
			"brands": [
				{"brand": "Chromium", "version": "110"},
				{"brand": "Google Chrome", "version": "110"},
				{"brand": "Not?A_Brand", "version": "24"}
			],
			"mobile": false,
			"platform": "macOS",
			"bitness": "64",
			"model": "",
			"platformVersion": "13.1.0",
			"uaFullVersion": "110.0.5481.77",
			"fullVersionList": [
				{"brand": "Chromium", "version": "110.0.5481.77"},
				{"brand": "Google Chrome", "version": "110.0.5481.77"}
			],
			"wow64": false
		},
		"channel": %q
	}`, channel)
}

// ---------------------------------------------------------------------------
// Helper: parityMockSetup configures the common mock expectations for job
// storage used across all event spec parity test cases. It configures
// WithStoreSafeTx and StoreEachBatchRetryInTx expectations to capture
// stored event payloads for subsequent field-level assertion.
// Returns a pointer to the captured jobs slice.
// ---------------------------------------------------------------------------
func parityMockSetup(c *testContext) *[][]*jobsdb.JobT {
	var capturedJobs [][]*jobsdb.JobT

	c.mockJobsDB.EXPECT().WithStoreSafeTx(
		gomock.Any(),
		gomock.Any(),
	).Times(1).Do(func(ctx context.Context, f func(jobsdb.StoreSafeTx) error) {
		_ = f(jobsdb.EmptyStoreSafeTx())
	}).Return(nil)

	c.mockJobsDB.EXPECT().StoreEachBatchRetryInTx(
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
	).DoAndReturn(
		func(ctx context.Context, tx jobsdb.StoreSafeTx, jobs [][]*jobsdb.JobT) (map[uuid.UUID]string, error) {
			capturedJobs = append(capturedJobs, jobs...)
			c.asyncHelper.ExpectAndNotifyCallbackWithName("")()
			return map[uuid.UUID]string{}, nil
		},
	).Times(1)

	return &capturedJobs
}

// ---------------------------------------------------------------------------
// Helper: extractParityEvent extracts a single event from the captured jobs.
// The stored payload structure is:
//
//	{"batch":[<event0>, <event1>, ...], "writeKey":"...", "requestIP":"...", "receivedAt":"..."}
//
// batchIdx selects which job batch, jobIdx selects the job within the batch,
// and eventIdx selects the event within that job's batch array.
// ---------------------------------------------------------------------------
func extractParityEvent(capturedJobs [][]*jobsdb.JobT, jobIdx, eventIdx int) gjson.Result {
	Expect(capturedJobs).ToNot(BeEmpty(), "captured jobs should not be empty")
	Expect(capturedJobs[0]).ToNot(BeEmpty(), "first job batch should not be empty")
	storedPayload := string(capturedJobs[0][jobIdx].EventPayload)
	return gjson.Get(storedPayload, fmt.Sprintf("batch.%d", eventIdx))
}

// ---------------------------------------------------------------------------
// Helper: assertSpecCommonFields verifies that common Segment Spec fields
// are present and correct in the stored event payload for a given event type.
// Fields verified: type, userId, messageId (exists + non-empty), receivedAt
// (exists, set by Gateway), rudderId (exists, computed by Gateway).
// ---------------------------------------------------------------------------
func assertSpecCommonFields(event gjson.Result, eventType string) {
	Expect(event.Get("type").String()).To(Equal(eventType),
		"event type should match handler type")
	Expect(event.Get("userId").String()).To(Equal("user-spec-001"),
		"userId should be preserved")
	Expect(event.Get("messageId").Exists()).To(BeTrue(),
		"messageId should exist")
	Expect(event.Get("messageId").String()).ToNot(BeEmpty(),
		"messageId should not be empty")
	Expect(event.Get("receivedAt").Exists()).To(BeTrue(),
		"receivedAt should be set by Gateway")
	Expect(event.Get("rudderId").Exists()).To(BeTrue(),
		"rudderId should be computed by Gateway")
}

// ---------------------------------------------------------------------------
// Helper: assertSpecContextFields verifies that all standard Segment Spec
// context fields (18+ fields from common.md) are preserved in the stored
// event payload, including the Client Hints userAgentData object (ES-001)
// and the channel field (ES-007).
// ---------------------------------------------------------------------------
func assertSpecContextFields(event gjson.Result, channel string) {
	// Top-level context fields
	Expect(event.Get("context.ip").String()).To(Equal("203.0.113.50"),
		"context.ip should be preserved")
	Expect(event.Get("context.locale").String()).To(Equal("en-US"),
		"context.locale should be preserved")
	Expect(event.Get("context.timezone").String()).To(Equal("America/Los_Angeles"),
		"context.timezone should be preserved")
	Expect(event.Get("context.active").Bool()).To(BeTrue(),
		"context.active should be preserved")
	Expect(event.Get("context.groupId").String()).To(Equal("grp-999"),
		"context.groupId should be preserved")

	// ES-007: context.channel auto-population verification
	Expect(event.Get("context.channel").String()).To(Equal(channel),
		"context.channel should be preserved (ES-007)")

	// context.app
	Expect(event.Get("context.app.name").String()).To(Equal("TestApp"))
	Expect(event.Get("context.app.version").String()).To(Equal("2.1.0"))
	Expect(event.Get("context.app.build").String()).To(Equal("1234"))

	// context.campaign (UTM parameters)
	Expect(event.Get("context.campaign.name").String()).To(Equal("spring_sale"))
	Expect(event.Get("context.campaign.source").String()).To(Equal("google"))
	Expect(event.Get("context.campaign.medium").String()).To(Equal("cpc"))
	Expect(event.Get("context.campaign.term").String()).To(Equal("analytics"))
	Expect(event.Get("context.campaign.content").String()).To(Equal("banner_1"))

	// context.device
	Expect(event.Get("context.device.id").String()).To(Equal("device-abc-123"))
	Expect(event.Get("context.device.manufacturer").String()).To(Equal("Apple"))
	Expect(event.Get("context.device.model").String()).To(Equal("iPhone 15"))
	Expect(event.Get("context.device.name").String()).To(Equal("Test iPhone"))
	Expect(event.Get("context.device.type").String()).To(Equal("ios"))
	Expect(event.Get("context.device.version").String()).To(Equal("17.2"))
	Expect(event.Get("context.device.advertisingId").String()).To(Equal("ad-id-456"))

	// context.library
	Expect(event.Get("context.library.name").String()).To(Equal("analytics.js"))
	Expect(event.Get("context.library.version").String()).To(Equal("2.1.0"))

	// context.network
	Expect(event.Get("context.network.carrier").String()).To(Equal("T-Mobile"))
	Expect(event.Get("context.network.wifi").Bool()).To(BeTrue())
	Expect(event.Get("context.network.cellular").Bool()).To(BeTrue())
	Expect(event.Get("context.network.bluetooth").Bool()).To(BeFalse())

	// context.os
	Expect(event.Get("context.os.name").String()).To(Equal("iOS"))
	Expect(event.Get("context.os.version").String()).To(Equal("17.2"))

	// context.page
	Expect(event.Get("context.page.path").String()).To(Equal("/products/widget"))
	Expect(event.Get("context.page.referrer").String()).To(Equal("https://www.google.com"))
	Expect(event.Get("context.page.search").String()).To(Equal("?q=widget"))
	Expect(event.Get("context.page.title").String()).To(Equal("Widget Page"))
	Expect(event.Get("context.page.url").String()).To(Equal("https://example.com/products/widget"))

	// context.referrer
	Expect(event.Get("context.referrer.type").String()).To(Equal("search"))
	Expect(event.Get("context.referrer.name").String()).To(Equal("Google"))
	Expect(event.Get("context.referrer.url").String()).To(Equal("https://www.google.com"))

	// context.screen
	Expect(event.Get("context.screen.density").Float()).To(Equal(float64(2)))
	Expect(event.Get("context.screen.height").Float()).To(Equal(float64(1920)))
	Expect(event.Get("context.screen.width").Float()).To(Equal(float64(1080)))

	// context.traits
	Expect(event.Get("context.traits.email").String()).To(Equal("test@example.com"))

	// context.userAgent (string)
	Expect(event.Get("context.userAgent").String()).To(ContainSubstring("Chrome/110"))

	// ES-001: context.userAgentData (Client Hints) — full structured pass-through
	Expect(event.Get("context.userAgentData").Exists()).To(BeTrue(),
		"context.userAgentData should exist (ES-001)")
	Expect(event.Get("context.userAgentData.brands").IsArray()).To(BeTrue(),
		"userAgentData.brands should be an array")
	Expect(event.Get("context.userAgentData.brands.#").Int()).To(Equal(int64(3)),
		"userAgentData.brands should have 3 entries")
	Expect(event.Get("context.userAgentData.brands.0.brand").String()).To(Equal("Chromium"))
	Expect(event.Get("context.userAgentData.brands.0.version").String()).To(Equal("110"))
	Expect(event.Get("context.userAgentData.brands.1.brand").String()).To(Equal("Google Chrome"))
	Expect(event.Get("context.userAgentData.brands.2.brand").String()).To(Equal("Not?A_Brand"))
	Expect(event.Get("context.userAgentData.mobile").Bool()).To(BeFalse(),
		"userAgentData.mobile should be preserved")
	Expect(event.Get("context.userAgentData.platform").String()).To(Equal("macOS"),
		"userAgentData.platform should be preserved")
	Expect(event.Get("context.userAgentData.bitness").String()).To(Equal("64"),
		"userAgentData.bitness should be preserved")
	Expect(event.Get("context.userAgentData.platformVersion").String()).To(Equal("13.1.0"))
	Expect(event.Get("context.userAgentData.uaFullVersion").String()).To(Equal("110.0.5481.77"))
	Expect(event.Get("context.userAgentData.fullVersionList").IsArray()).To(BeTrue(),
		"userAgentData.fullVersionList should be an array")
	Expect(event.Get("context.userAgentData.fullVersionList.#").Int()).To(Equal(int64(2)))
	Expect(event.Get("context.userAgentData.fullVersionList.0.brand").String()).To(Equal("Chromium"))
	Expect(event.Get("context.userAgentData.fullVersionList.0.version").String()).To(Equal("110.0.5481.77"))
	Expect(event.Get("context.userAgentData.wow64").Bool()).To(BeFalse(),
		"userAgentData.wow64 should be preserved")
}

// ===========================================================================
// Event Spec Parity — Ginkgo BDD Test Suite
// Validates field-level preservation for all 6 Segment Spec event types
// through the RudderStack Gateway ingestion pipeline.
// ===========================================================================
var _ = Describe("Event Spec Parity", func() {
	initGW()

	var (
		c              *testContext
		parityGW       *Handle
		parityStats    *memstats.Store
		parityConf     *config.Config
	)

	BeforeEach(func() {
		c = &testContext{}
		c.Setup()
		c.initializeAppFeatures()

		var err error
		parityStats, err = memstats.New()
		Expect(err).To(BeNil())

		parityConf = config.New()
		parityConf.Set("Gateway.enableRateLimit", false)
		parityConf.Set("Gateway.enableSuppressUserFeature", false)

		parityGW = &Handle{}
		err = parityGW.Setup(
			context.Background(),
			parityConf,
			logger.NOP,
			parityStats,
			c.mockApp,
			c.mockBackendConfig,
			c.mockJobsDB,
			c.mockRateLimiter,
			c.mockVersionHandler,
			rsources.NewNoOpService(),
			transformer.NewNoOpService(),
			sourcedebugger.NewNoOpService(),
			nil,
		)
		Expect(err).To(BeNil())
		waitForBackendConfigInit(parityGW)
	})

	AfterEach(func() {
		Expect(parityGW.Shutdown()).To(BeNil())
		c.Finish()
	})

	// sendAndCapture sends an event via the given per-type handler, captures the
	// stored job, and returns the first event in the stored batch as gjson.Result.
	sendAndCapture := func(handler http.HandlerFunc, payload, reqType string) gjson.Result {
		capturedJobsPtr := parityMockSetup(c)
		expectHandlerResponse(
			handler,
			authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
			http.StatusOK, "ok", reqType,
		)
		return extractParityEvent(*capturedJobsPtr, 0, 0)
	}

	// -----------------------------------------------------------------
	// Identify event field preservation (Segment Spec: identify.md)
	// -----------------------------------------------------------------
	Context("Identify event field preservation", func() {
		It("should preserve all Segment Spec identify fields through the Gateway", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"messageId": "msg-identify-001",
				"timestamp": "2024-06-15T10:30:00.000Z",
				"sentAt": "2024-06-15T10:30:01.000Z",
				"originalTimestamp": "2024-06-15T10:30:00.000Z",
				"channel": "browser",
				"version": 1,
				"traits": {
					"name": "Peter Gibbons",
					"email": "peter@example.com",
					"age": 32,
					"phone": "+1-555-867-5309",
					"address": {
						"city": "San Francisco",
						"state": "CA",
						"country": "US"
					}
				},
				"integrations": {
					"All": true,
					"Google Analytics": false
				},
				"context": %s
			}`, specParityContext("browser"))

			event := sendAndCapture(parityGW.webIdentifyHandler(), payload, "identify")

			// Common fields
			assertSpecCommonFields(event, "identify")
			Expect(event.Get("anonymousId").String()).To(Equal("anon-spec-001"))
			Expect(event.Get("originalTimestamp").String()).To(Equal("2024-06-15T10:30:00.000Z"))
			Expect(event.Get("sentAt").String()).To(Equal("2024-06-15T10:30:01.000Z"))
			Expect(event.Get("channel").String()).To(Equal("browser"))

			// Context fields (all 18+ standard fields including Client Hints)
			assertSpecContextFields(event, "browser")

			// Integrations (Segment Spec: All:true default with per-destination toggles)
			Expect(event.Get("integrations.All").Bool()).To(BeTrue())
			Expect(event.Get("integrations.Google Analytics").Bool()).To(BeFalse())

			// Identify-specific: traits (ES-003 reserved traits)
			Expect(event.Get("traits.name").String()).To(Equal("Peter Gibbons"))
			Expect(event.Get("traits.email").String()).To(Equal("peter@example.com"))
			Expect(event.Get("traits.age").Float()).To(Equal(float64(32)))
			Expect(event.Get("traits.phone").String()).To(Equal("+1-555-867-5309"))
			Expect(event.Get("traits.address.city").String()).To(Equal("San Francisco"))
			Expect(event.Get("traits.address.state").String()).To(Equal("CA"))
			Expect(event.Get("traits.address.country").String()).To(Equal("US"))
		})
	})

	// -----------------------------------------------------------------
	// Track event field preservation (Segment Spec: track.md)
	// -----------------------------------------------------------------
	Context("Track event field preservation", func() {
		It("should preserve all Segment Spec track fields through the Gateway", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"messageId": "msg-track-001",
				"event": "Product Viewed",
				"properties": {
					"product_id": "prod-123",
					"sku": "SKU-WIDGET-001",
					"name": "Premium Widget",
					"category": "Widgets",
					"price": 29.99,
					"brand": "WidgetCo",
					"currency": "USD"
				},
				"timestamp": "2024-06-15T10:31:00.000Z",
				"sentAt": "2024-06-15T10:31:01.000Z",
				"originalTimestamp": "2024-06-15T10:31:00.000Z",
				"channel": "browser",
				"version": 1,
				"integrations": {"All": true},
				"context": %s
			}`, specParityContext("browser"))

			event := sendAndCapture(parityGW.webTrackHandler(), payload, "track")

			// Common fields
			assertSpecCommonFields(event, "track")
			Expect(event.Get("anonymousId").String()).To(Equal("anon-spec-001"))
			Expect(event.Get("originalTimestamp").String()).To(Equal("2024-06-15T10:31:00.000Z"))
			Expect(event.Get("sentAt").String()).To(Equal("2024-06-15T10:31:01.000Z"))

			// Context fields
			assertSpecContextFields(event, "browser")

			// Integrations
			Expect(event.Get("integrations.All").Bool()).To(BeTrue())

			// Track-specific: event name (ES-002 semantic event category)
			Expect(event.Get("event").String()).To(Equal("Product Viewed"),
				"E-Commerce v2 semantic event name should be preserved")

			// Track-specific: properties
			Expect(event.Get("properties.product_id").String()).To(Equal("prod-123"))
			Expect(event.Get("properties.sku").String()).To(Equal("SKU-WIDGET-001"))
			Expect(event.Get("properties.name").String()).To(Equal("Premium Widget"))
			Expect(event.Get("properties.category").String()).To(Equal("Widgets"))
			Expect(event.Get("properties.price").Float()).To(Equal(29.99))
			Expect(event.Get("properties.brand").String()).To(Equal("WidgetCo"))
			Expect(event.Get("properties.currency").String()).To(Equal("USD"))
		})
	})

	// -----------------------------------------------------------------
	// Page event field preservation (Segment Spec: page.md)
	// -----------------------------------------------------------------
	Context("Page event field preservation", func() {
		It("should preserve all Segment Spec page fields through the Gateway", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"messageId": "msg-page-001",
				"name": "Product Detail",
				"category": "Ecommerce",
				"properties": {
					"title": "Premium Widget - WidgetCo",
					"url": "https://example.com/products/widget",
					"path": "/products/widget",
					"referrer": "https://www.google.com",
					"search": "?q=widget",
					"keywords": ["widget", "premium"]
				},
				"timestamp": "2024-06-15T10:32:00.000Z",
				"sentAt": "2024-06-15T10:32:01.000Z",
				"originalTimestamp": "2024-06-15T10:32:00.000Z",
				"channel": "browser",
				"version": 1,
				"integrations": {"All": true},
				"context": %s
			}`, specParityContext("browser"))

			event := sendAndCapture(parityGW.webPageHandler(), payload, "page")

			// Common fields
			assertSpecCommonFields(event, "page")
			Expect(event.Get("anonymousId").String()).To(Equal("anon-spec-001"))
			Expect(event.Get("originalTimestamp").String()).To(Equal("2024-06-15T10:32:00.000Z"))
			Expect(event.Get("sentAt").String()).To(Equal("2024-06-15T10:32:01.000Z"))

			// Context fields
			assertSpecContextFields(event, "browser")

			// Page-specific: name and category
			Expect(event.Get("name").String()).To(Equal("Product Detail"))
			Expect(event.Get("category").String()).To(Equal("Ecommerce"))

			// Page-specific: properties
			Expect(event.Get("properties.title").String()).To(Equal("Premium Widget - WidgetCo"))
			Expect(event.Get("properties.url").String()).To(Equal("https://example.com/products/widget"))
			Expect(event.Get("properties.path").String()).To(Equal("/products/widget"))
			Expect(event.Get("properties.referrer").String()).To(Equal("https://www.google.com"))
			Expect(event.Get("properties.search").String()).To(Equal("?q=widget"))
			Expect(event.Get("properties.keywords").IsArray()).To(BeTrue(),
				"keywords should be preserved as an array")
			Expect(event.Get("properties.keywords.#").Int()).To(Equal(int64(2)))
			Expect(event.Get("properties.keywords.0").String()).To(Equal("widget"))
			Expect(event.Get("properties.keywords.1").String()).To(Equal("premium"))
		})
	})

	// -----------------------------------------------------------------
	// Screen event field preservation (Segment Spec: screen.md)
	// -----------------------------------------------------------------
	Context("Screen event field preservation", func() {
		It("should preserve all Segment Spec screen fields through the Gateway", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"messageId": "msg-screen-001",
				"name": "Product Detail Screen",
				"category": "Ecommerce",
				"properties": {
					"variation": "blue_theme"
				},
				"timestamp": "2024-06-15T10:33:00.000Z",
				"sentAt": "2024-06-15T10:33:01.000Z",
				"originalTimestamp": "2024-06-15T10:33:00.000Z",
				"channel": "mobile",
				"version": 1,
				"integrations": {"All": true},
				"context": %s
			}`, specParityContext("mobile"))

			event := sendAndCapture(parityGW.webScreenHandler(), payload, "screen")

			// Common fields
			assertSpecCommonFields(event, "screen")
			Expect(event.Get("anonymousId").String()).To(Equal("anon-spec-001"))
			Expect(event.Get("originalTimestamp").String()).To(Equal("2024-06-15T10:33:00.000Z"))
			Expect(event.Get("sentAt").String()).To(Equal("2024-06-15T10:33:01.000Z"))

			// ES-007: channel="mobile" for screen events
			Expect(event.Get("channel").String()).To(Equal("mobile"),
				"channel should be preserved as 'mobile' for screen events (ES-007)")

			// Context fields with mobile channel
			assertSpecContextFields(event, "mobile")

			// Screen-specific: name and category
			Expect(event.Get("name").String()).To(Equal("Product Detail Screen"))
			Expect(event.Get("category").String()).To(Equal("Ecommerce"))

			// Screen-specific: properties
			Expect(event.Get("properties.variation").String()).To(Equal("blue_theme"))
		})
	})

	// -----------------------------------------------------------------
	// Group event field preservation (Segment Spec: group.md)
	// -----------------------------------------------------------------
	Context("Group event field preservation", func() {
		It("should preserve all Segment Spec group fields through the Gateway", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"messageId": "msg-group-001",
				"groupId": "grp-initech-001",
				"traits": {
					"name": "Initech Corporation",
					"email": "info@initech.com",
					"industry": "Technology",
					"employees": "150",
					"plan": "enterprise",
					"website": "https://initech.com"
				},
				"timestamp": "2024-06-15T10:34:00.000Z",
				"sentAt": "2024-06-15T10:34:01.000Z",
				"originalTimestamp": "2024-06-15T10:34:00.000Z",
				"channel": "server",
				"version": 1,
				"integrations": {"All": true},
				"context": %s
			}`, specParityContext("server"))

			event := sendAndCapture(parityGW.webGroupHandler(), payload, "group")

			// Common fields
			assertSpecCommonFields(event, "group")
			Expect(event.Get("anonymousId").String()).To(Equal("anon-spec-001"))
			Expect(event.Get("originalTimestamp").String()).To(Equal("2024-06-15T10:34:00.000Z"))
			Expect(event.Get("sentAt").String()).To(Equal("2024-06-15T10:34:01.000Z"))

			// ES-007: channel="server" for server-side group events
			Expect(event.Get("channel").String()).To(Equal("server"),
				"channel should be preserved as 'server' for group events (ES-007)")

			// Context fields with server channel
			assertSpecContextFields(event, "server")

			// Group-specific: groupId
			Expect(event.Get("groupId").String()).To(Equal("grp-initech-001"),
				"groupId should be preserved")

			// Group-specific: traits (ES-003 reserved group traits)
			Expect(event.Get("traits.name").String()).To(Equal("Initech Corporation"))
			Expect(event.Get("traits.email").String()).To(Equal("info@initech.com"))
			Expect(event.Get("traits.industry").String()).To(Equal("Technology"))
			Expect(event.Get("traits.employees").String()).To(Equal("150"))
			Expect(event.Get("traits.plan").String()).To(Equal("enterprise"))
			Expect(event.Get("traits.website").String()).To(Equal("https://initech.com"))
		})
	})

	// -----------------------------------------------------------------
	// Alias event field preservation (Segment Spec: alias.md)
	// -----------------------------------------------------------------
	Context("Alias event field preservation", func() {
		It("should preserve all Segment Spec alias fields through the Gateway", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"previousId": "anon-spec-001",
				"messageId": "msg-alias-001",
				"timestamp": "2024-06-15T10:35:00.000Z",
				"sentAt": "2024-06-15T10:35:01.000Z",
				"originalTimestamp": "2024-06-15T10:35:00.000Z",
				"channel": "browser",
				"version": 1,
				"integrations": {"All": true},
				"context": %s
			}`, specParityContext("browser"))

			event := sendAndCapture(parityGW.webAliasHandler(), payload, "alias")

			// Common fields
			assertSpecCommonFields(event, "alias")
			Expect(event.Get("anonymousId").String()).To(Equal("anon-spec-001"))
			Expect(event.Get("originalTimestamp").String()).To(Equal("2024-06-15T10:35:00.000Z"))
			Expect(event.Get("sentAt").String()).To(Equal("2024-06-15T10:35:01.000Z"))

			// Context fields
			assertSpecContextFields(event, "browser")

			// Alias-specific: previousId (required per Segment Spec)
			Expect(event.Get("previousId").String()).To(Equal("anon-spec-001"),
				"previousId should be preserved for alias events")
		})
	})

	// -----------------------------------------------------------------
	// Batch endpoint with all 6 event types (E-001 comprehensive test)
	// Verifies that a single batch containing all Segment Spec event types
	// preserves every field through the Gateway pipeline.
	// -----------------------------------------------------------------
	Context("Batch endpoint with all 6 event types", func() {
		It("should preserve fields for all event types in a single batch", func() {
			batchPayload := fmt.Sprintf(`{"batch": [
				{
					"type": "identify",
					"userId": "user-spec-001",
					"anonymousId": "anon-spec-001",
					"messageId": "msg-batch-identify",
					"traits": {"name": "Peter Gibbons", "email": "peter@example.com"},
					"originalTimestamp": "2024-06-15T10:30:00.000Z",
					"sentAt": "2024-06-15T10:30:01.000Z",
					"channel": "browser",
					"integrations": {"All": true},
					"context": %[1]s
				},
				{
					"type": "track",
					"userId": "user-spec-001",
					"anonymousId": "anon-spec-001",
					"messageId": "msg-batch-track",
					"event": "Order Completed",
					"properties": {"orderId": "ord-999", "total": 125.50, "currency": "USD"},
					"originalTimestamp": "2024-06-15T10:31:00.000Z",
					"sentAt": "2024-06-15T10:31:01.000Z",
					"channel": "browser",
					"integrations": {"All": true},
					"context": %[1]s
				},
				{
					"type": "page",
					"userId": "user-spec-001",
					"anonymousId": "anon-spec-001",
					"messageId": "msg-batch-page",
					"name": "Checkout",
					"properties": {"url": "https://example.com/checkout", "title": "Checkout Page"},
					"originalTimestamp": "2024-06-15T10:32:00.000Z",
					"sentAt": "2024-06-15T10:32:01.000Z",
					"channel": "browser",
					"integrations": {"All": true},
					"context": %[1]s
				},
				{
					"type": "screen",
					"userId": "user-spec-001",
					"anonymousId": "anon-spec-001",
					"messageId": "msg-batch-screen",
					"name": "Home Screen",
					"properties": {"variation": "dark"},
					"originalTimestamp": "2024-06-15T10:33:00.000Z",
					"sentAt": "2024-06-15T10:33:01.000Z",
					"channel": "browser",
					"integrations": {"All": true},
					"context": %[1]s
				},
				{
					"type": "group",
					"userId": "user-spec-001",
					"anonymousId": "anon-spec-001",
					"messageId": "msg-batch-group",
					"groupId": "grp-initech",
					"traits": {"name": "Initech", "plan": "enterprise"},
					"originalTimestamp": "2024-06-15T10:34:00.000Z",
					"sentAt": "2024-06-15T10:34:01.000Z",
					"channel": "browser",
					"integrations": {"All": true},
					"context": %[1]s
				},
				{
					"type": "alias",
					"userId": "user-spec-001",
					"anonymousId": "anon-spec-001",
					"messageId": "msg-batch-alias",
					"previousId": "old-anon-001",
					"originalTimestamp": "2024-06-15T10:35:00.000Z",
					"sentAt": "2024-06-15T10:35:01.000Z",
					"channel": "browser",
					"integrations": {"All": true},
					"context": %[1]s
				}
			]}`, specParityContext("browser"))

			capturedJobsPtr := parityMockSetup(c)
			expectHandlerResponse(
				parityGW.webBatchHandler(),
				authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(batchPayload)),
				http.StatusOK, "ok", "batch",
			)

			// The Gateway creates a separate job for each event in the batch.
			// Each job stores one event: {"batch":[event], "writeKey":"...", ...}
			Expect(*capturedJobsPtr).ToNot(BeEmpty())
			allJobs := (*capturedJobsPtr)[0]
			Expect(allJobs).To(HaveLen(6),
				"batch should produce 6 jobs (one per event)")

			// Verify writeKey and requestIP at the envelope level for the first job
			firstPayload := string(allJobs[0].EventPayload)
			Expect(gjson.Get(firstPayload, "writeKey").String()).To(Equal(WriteKeyEnabled))
			Expect(gjson.Get(firstPayload, "requestIP").Exists()).To(BeTrue())
			Expect(gjson.Get(firstPayload, "receivedAt").Exists()).To(BeTrue())

			// Verify each event type is preserved with correct fields.
			// Each job stores exactly one event at batch.0.
			eventTypes := []struct {
				jobIdx    int
				eventType string
				check     func(gjson.Result)
			}{
				{0, "identify", func(e gjson.Result) {
					Expect(e.Get("traits.name").String()).To(Equal("Peter Gibbons"))
					Expect(e.Get("traits.email").String()).To(Equal("peter@example.com"))
				}},
				{1, "track", func(e gjson.Result) {
					Expect(e.Get("event").String()).To(Equal("Order Completed"))
					Expect(e.Get("properties.orderId").String()).To(Equal("ord-999"))
					Expect(e.Get("properties.total").Float()).To(Equal(125.50))
					Expect(e.Get("properties.currency").String()).To(Equal("USD"))
				}},
				{2, "page", func(e gjson.Result) {
					Expect(e.Get("name").String()).To(Equal("Checkout"))
					Expect(e.Get("properties.url").String()).To(Equal("https://example.com/checkout"))
					Expect(e.Get("properties.title").String()).To(Equal("Checkout Page"))
				}},
				{3, "screen", func(e gjson.Result) {
					Expect(e.Get("name").String()).To(Equal("Home Screen"))
					Expect(e.Get("properties.variation").String()).To(Equal("dark"))
				}},
				{4, "group", func(e gjson.Result) {
					Expect(e.Get("groupId").String()).To(Equal("grp-initech"))
					Expect(e.Get("traits.name").String()).To(Equal("Initech"))
					Expect(e.Get("traits.plan").String()).To(Equal("enterprise"))
				}},
				{5, "alias", func(e gjson.Result) {
					Expect(e.Get("previousId").String()).To(Equal("old-anon-001"))
				}},
			}

			for _, tc := range eventTypes {
				storedPayload := string(allJobs[tc.jobIdx].EventPayload)
				evt := gjson.Get(storedPayload, "batch.0")
				Expect(evt.Get("type").String()).To(Equal(tc.eventType),
					fmt.Sprintf("job %d event should have type %q", tc.jobIdx, tc.eventType))
				Expect(evt.Get("userId").String()).To(Equal("user-spec-001"),
					fmt.Sprintf("job %d userId should be preserved", tc.jobIdx))
				Expect(evt.Get("messageId").Exists()).To(BeTrue(),
					fmt.Sprintf("job %d messageId should exist", tc.jobIdx))
				Expect(evt.Get("rudderId").Exists()).To(BeTrue(),
					fmt.Sprintf("job %d rudderId should be set", tc.jobIdx))

				// Verify Client Hints pass-through for every event in the batch
				Expect(evt.Get("context.userAgentData").Exists()).To(BeTrue(),
					fmt.Sprintf("job %d should preserve userAgentData (ES-001)", tc.jobIdx))
				Expect(evt.Get("context.userAgentData.brands").IsArray()).To(BeTrue())
				Expect(evt.Get("context.channel").String()).To(Equal("browser"),
					fmt.Sprintf("job %d should preserve context.channel (ES-007)", tc.jobIdx))

				// Integrations
				Expect(evt.Get("integrations.All").Bool()).To(BeTrue(),
					fmt.Sprintf("job %d should preserve integrations", tc.jobIdx))

				// Event-type-specific assertions
				tc.check(evt)
			}
		})
	})

	// -----------------------------------------------------------------
	// ES-007: Channel field verification across all three channel values
	// Verifies that context.channel is preserved for "server", "browser",
	// and "mobile" values as defined in the Segment Spec.
	// -----------------------------------------------------------------
	Context("Channel field auto-population verification (ES-007)", func() {
		It("should preserve context.channel='server' for server-side events", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"messageId": "msg-channel-server",
				"channel": "server",
				"context": %s
			}`, specParityContext("server"))

			event := sendAndCapture(parityGW.webTrackHandler(), payload, "track")
			Expect(event.Get("channel").String()).To(Equal("server"))
			Expect(event.Get("context.channel").String()).To(Equal("server"))
		})

		It("should preserve context.channel='browser' for browser events", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"messageId": "msg-channel-browser",
				"channel": "browser",
				"context": %s
			}`, specParityContext("browser"))

			event := sendAndCapture(parityGW.webPageHandler(), payload, "page")
			Expect(event.Get("channel").String()).To(Equal("browser"))
			Expect(event.Get("context.channel").String()).To(Equal("browser"))
		})

		It("should preserve context.channel='mobile' for mobile events", func() {
			payload := fmt.Sprintf(`{
				"userId": "user-spec-001",
				"anonymousId": "anon-spec-001",
				"messageId": "msg-channel-mobile",
				"channel": "mobile",
				"context": %s
			}`, specParityContext("mobile"))

			event := sendAndCapture(parityGW.webScreenHandler(), payload, "screen")
			Expect(event.Get("channel").String()).To(Equal("mobile"))
			Expect(event.Get("context.channel").String()).To(Equal("mobile"))
		})
	})
})
