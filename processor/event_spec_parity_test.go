package processor

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/processor/isolation"
	"github.com/rudderlabs/rudder-server/processor/transformer"
	"github.com/rudderlabs/rudder-server/processor/types"
)

// segmentSpecCommonContext returns a JSON string representing the full Segment Spec
// context object with all 18+ standard fields, including Client Hints (userAgentData).
// This is the authoritative test fixture referenced from refs/segment-docs/src/connections/spec/common.md.
func segmentSpecCommonContext() string {
	return `{
		"active": true,
		"app": {
			"name": "TestApp",
			"version": "1.5.0",
			"build": "2200"
		},
		"campaign": {
			"name": "summer-sale",
			"source": "google",
			"medium": "cpc",
			"term": "analytics",
			"content": "hero-banner"
		},
		"device": {
			"id": "device-abc-123",
			"advertisingId": "ad-id-456",
			"manufacturer": "Apple",
			"model": "iPhone 15 Pro",
			"name": "Alice iPhone",
			"type": "ios",
			"version": "17.4"
		},
		"ip": "203.0.113.42",
		"library": {
			"name": "analytics.js",
			"version": "2.11.1"
		},
		"locale": "en-US",
		"network": {
			"bluetooth": false,
			"carrier": "T-Mobile",
			"cellular": true,
			"wifi": false
		},
		"os": {
			"name": "iOS",
			"version": "17.4"
		},
		"page": {
			"path": "/products/widget",
			"referrer": "https://google.com/search?q=widget",
			"search": "?color=blue",
			"title": "Premium Widget | TestStore",
			"url": "https://teststore.example.com/products/widget?color=blue"
		},
		"referrer": {
			"type": "search",
			"name": "Google",
			"url": "https://google.com/search?q=widget",
			"link": "https://google.com/search?q=widget"
		},
		"screen": {
			"density": 3,
			"height": 2556,
			"width": 1179
		},
		"timezone": "America/Los_Angeles",
		"groupId": "grp-999",
		"traits": {
			"plan": "enterprise"
		},
		"userAgent": "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1",
		"userAgentData": {
			"brands": [
				{"brand": "Chromium", "version": "124"},
				{"brand": "Google Chrome", "version": "124"},
				{"brand": "Not-A.Brand", "version": "99"}
			],
			"mobile": true,
			"platform": "iOS",
			"bitness": "64",
			"model": "iPhone 15 Pro",
			"platformVersion": "17.4",
			"uaFullVersion": "124.0.6367.60",
			"fullVersionList": [
				{"brand": "Chromium", "version": "124.0.6367.60"},
				{"brand": "Google Chrome", "version": "124.0.6367.60"},
				{"brand": "Not-A.Brand", "version": "99.0.0.0"}
			],
			"wow64": false
		},
		"channel": "browser"
	}`
}

// eventSpecParityPayload builds a full Segment Spec event payload for the given
// event type.  It includes every common field from the Segment Spec plus the
// event-type-specific fields.  messageID and timestamps are injected from the
// caller so that assertions can reference deterministic values.
func eventSpecParityPayload(eventType, messageID, ts, sentAt, origTS string) string {
	base := fmt.Sprintf(`{
		"anonymousId": "anon-spec-parity-001",
		"userId": "user-spec-parity-42",
		"messageId": %q,
		"type": %q,
		"channel": "browser",
		"version": 1,
		"timestamp": %q,
		"sentAt": %q,
		"originalTimestamp": %q,
		"context": %s,
		"integrations": {
			"All": true,
			"Google Analytics": false,
			"Mixpanel": {
				"apiKey": "mix-key-123"
			}
		},
		"rudderId": "rudder-id-spec-parity"
	}`,
		messageID,
		eventType,
		ts,
		sentAt,
		origTS,
		segmentSpecCommonContext(),
	)
	return base
}

// mustSet wraps sjson.Set and panics on error, ensuring test payload construction
// failures are surfaced immediately rather than silently ignored.
func mustSet(json, path, value string) string {
	result, err := sjson.Set(json, path, value)
	if err != nil {
		panic(fmt.Sprintf("sjson.Set(%q, %q) failed: %v", path, value, err))
	}
	return result
}

// mustSetRaw wraps sjson.SetRaw and panics on error, ensuring test payload
// construction failures are surfaced immediately rather than silently ignored.
func mustSetRaw(json, path, rawValue string) string {
	result, err := sjson.SetRaw(json, path, rawValue)
	if err != nil {
		panic(fmt.Sprintf("sjson.SetRaw(%q) failed: %v", path, err))
	}
	return result
}

// identifyPayload returns a complete Segment Spec Identify payload with all 17 reserved traits.
func identifyPayload(messageID, ts, sentAt, origTS string) string {
	base := eventSpecParityPayload("identify", messageID, ts, sentAt, origTS)
	// Add identify-specific traits containing all 17 Segment reserved identify traits
	traitsJSON := `{
		"address": {
			"city": "San Francisco",
			"country": "US",
			"postalCode": "94107",
			"state": "CA",
			"street": "123 Market St"
		},
		"age": 32,
		"avatar": "https://example.com/avatars/alice.png",
		"birthday": "1992-06-15",
		"company": {
			"name": "TestCorp",
			"id": "company-001",
			"industry": "Technology",
			"employee_count": 500,
			"plan": "enterprise"
		},
		"createdAt": "2023-01-15T08:00:00.000Z",
		"description": "A test user for Segment Spec parity validation",
		"email": "alice@testcorp.example.com",
		"firstName": "Alice",
		"gender": "female",
		"id": "user-spec-parity-42",
		"lastName": "Tester",
		"name": "Alice Tester",
		"phone": "+14155551234",
		"title": "Sr. Engineer",
		"username": "alice_tester",
		"website": "https://alice.example.com"
	}`
	return mustSetRaw(base, "traits", traitsJSON)
}

// trackPayload returns a complete Segment Spec Track payload with a semantic event name.
func trackPayload(messageID, ts, sentAt, origTS string) string {
	base := eventSpecParityPayload("track", messageID, ts, sentAt, origTS)
	b := mustSet(base, "event", "Product Viewed")
	propsJSON := `{
		"product_id": "prod-spec-001",
		"sku": "SKU-SPEC-A",
		"name": "Spec Parity Widget",
		"category": "Widgets",
		"price": 49.99,
		"brand": "WidgetCo",
		"variant": "Blue",
		"quantity": 2,
		"coupon": "PARITY10",
		"url": "https://teststore.example.com/products/spec-widget",
		"image_url": "https://teststore.example.com/images/spec-widget.png",
		"currency": "USD",
		"value": 99.98,
		"revenue": 89.98,
		"position": 3
	}`
	return mustSetRaw(b, "properties", propsJSON)
}

// pagePayload returns a complete Segment Spec Page payload.
func pagePayload(messageID, ts, sentAt, origTS string) string {
	base := eventSpecParityPayload("page", messageID, ts, sentAt, origTS)
	b := mustSet(base, "name", "Premium Widget")
	b = mustSet(b, "category", "Product Pages")
	propsJSON := `{
		"title": "Premium Widget | TestStore",
		"url": "https://teststore.example.com/products/widget?color=blue",
		"path": "/products/widget",
		"referrer": "https://google.com/search?q=widget",
		"search": "?color=blue",
		"keywords": ["widget","premium","blue"]
	}`
	return mustSetRaw(b, "properties", propsJSON)
}

// screenPayload returns a complete Segment Spec Screen payload.
func screenPayload(messageID, ts, sentAt, origTS string) string {
	base := eventSpecParityPayload("screen", messageID, ts, sentAt, origTS)
	b := mustSet(base, "name", "ProductDetail")
	b = mustSet(b, "category", "Product")
	propsJSON := `{
		"variation": "blue-variant",
		"loginStatus": true,
		"itemCount": 5
	}`
	return mustSetRaw(b, "properties", propsJSON)
}

// groupPayload returns a complete Segment Spec Group payload with all 12 reserved group traits.
func groupPayload(messageID, ts, sentAt, origTS string) string {
	base := eventSpecParityPayload("group", messageID, ts, sentAt, origTS)
	b := mustSet(base, "groupId", "grp-spec-parity-001")
	traitsJSON := `{
		"address": {
			"city": "San Francisco",
			"country": "US",
			"postalCode": "94107",
			"state": "CA",
			"street": "456 Mission St"
		},
		"avatar": "https://example.com/logos/testcorp.png",
		"createdAt": "2020-03-10T12:00:00.000Z",
		"description": "A technology company for testing",
		"email": "contact@testcorp.example.com",
		"employees": "500",
		"id": "grp-spec-parity-001",
		"industry": "Technology",
		"name": "TestCorp Inc.",
		"phone": "+14155559876",
		"website": "https://testcorp.example.com",
		"plan": "enterprise"
	}`
	return mustSetRaw(b, "traits", traitsJSON)
}

// aliasPayload returns a complete Segment Spec Alias payload.
func aliasPayload(messageID, ts, sentAt, origTS string) string {
	base := eventSpecParityPayload("alias", messageID, ts, sentAt, origTS)
	return mustSet(base, "previousId", "old-anon-id-999")
}

// assertCommonFieldPreservation verifies every common Segment Spec field exists
// and is preserved in the captured TransformerEvent message serialised as JSON.
func assertCommonFieldPreservation(eventStr string) {
	// ---- Top-level common fields ----
	Expect(gjson.Get(eventStr, "anonymousId").Exists()).To(BeTrue(), "anonymousId must exist")
	Expect(gjson.Get(eventStr, "anonymousId").String()).To(Equal("anon-spec-parity-001"),
		"anonymousId value must be preserved")

	Expect(gjson.Get(eventStr, "userId").Exists()).To(BeTrue(), "userId must exist")
	Expect(gjson.Get(eventStr, "userId").String()).To(Equal("user-spec-parity-42"),
		"userId value must be preserved")

	Expect(gjson.Get(eventStr, "messageId").Exists()).To(BeTrue(), "messageId must exist")

	Expect(gjson.Get(eventStr, "type").Exists()).To(BeTrue(), "type must exist")

	// Timestamp fields: the processor may normalise to ISO-8601 / RFC3339 but must not drop them.
	Expect(gjson.Get(eventStr, "originalTimestamp").Exists()).To(BeTrue(), "originalTimestamp must exist")
	Expect(gjson.Get(eventStr, "sentAt").Exists()).To(BeTrue(), "sentAt must exist")
	Expect(gjson.Get(eventStr, "timestamp").Exists()).To(BeTrue(), "timestamp must exist (computed by processor)")
	Expect(gjson.Get(eventStr, "receivedAt").Exists()).To(BeTrue(), "receivedAt must exist (set by processor)")

	// ---- context object ----
	Expect(gjson.Get(eventStr, "context").Exists()).To(BeTrue(), "context must exist")

	// context.ip
	Expect(gjson.Get(eventStr, "context.ip").Exists()).To(BeTrue(), "context.ip must exist")

	// context.library
	Expect(gjson.Get(eventStr, "context.library.name").Exists()).To(BeTrue(), "context.library.name must exist")
	Expect(gjson.Get(eventStr, "context.library.name").String()).To(Equal("analytics.js"),
		"context.library.name value must be preserved")
	Expect(gjson.Get(eventStr, "context.library.version").Exists()).To(BeTrue(), "context.library.version must exist")
	Expect(gjson.Get(eventStr, "context.library.version").String()).To(Equal("2.11.1"),
		"context.library.version value must be preserved")

	// context.userAgent (string UA)
	Expect(gjson.Get(eventStr, "context.userAgent").Exists()).To(BeTrue(), "context.userAgent must exist")

	// --- Client Hints (context.userAgentData) – ES-001 ---
	Expect(gjson.Get(eventStr, "context.userAgentData").Exists()).To(BeTrue(),
		"context.userAgentData must be preserved (Client Hints ES-001)")
	Expect(gjson.Get(eventStr, "context.userAgentData.brands").IsArray()).To(BeTrue(),
		"context.userAgentData.brands must be an array")
	Expect(gjson.Get(eventStr, "context.userAgentData.brands.#").Int()).To(BeNumerically("==", 3),
		"context.userAgentData.brands must contain all 3 entries")
	Expect(gjson.Get(eventStr, "context.userAgentData.brands.0.brand").String()).To(Equal("Chromium"),
		"brands[0].brand must be preserved")
	Expect(gjson.Get(eventStr, "context.userAgentData.brands.0.version").String()).To(Equal("124"),
		"brands[0].version must be preserved")
	Expect(gjson.Get(eventStr, "context.userAgentData.mobile").Bool()).To(BeTrue(),
		"context.userAgentData.mobile must be preserved as true")
	Expect(gjson.Get(eventStr, "context.userAgentData.platform").String()).To(Equal("iOS"),
		"context.userAgentData.platform must be preserved")
	// High-entropy hints
	Expect(gjson.Get(eventStr, "context.userAgentData.bitness").String()).To(Equal("64"),
		"context.userAgentData.bitness must be preserved")
	Expect(gjson.Get(eventStr, "context.userAgentData.model").String()).To(Equal("iPhone 15 Pro"),
		"context.userAgentData.model must be preserved")
	Expect(gjson.Get(eventStr, "context.userAgentData.platformVersion").String()).To(Equal("17.4"),
		"context.userAgentData.platformVersion must be preserved")
	Expect(gjson.Get(eventStr, "context.userAgentData.uaFullVersion").String()).To(Equal("124.0.6367.60"),
		"context.userAgentData.uaFullVersion must be preserved")
	Expect(gjson.Get(eventStr, "context.userAgentData.fullVersionList").IsArray()).To(BeTrue(),
		"context.userAgentData.fullVersionList must be an array")
	Expect(gjson.Get(eventStr, "context.userAgentData.wow64").Bool()).To(BeFalse(),
		"context.userAgentData.wow64 must be preserved as false")

	// --- context.channel – ES-007 ---
	Expect(gjson.Get(eventStr, "context.channel").Exists()).To(BeTrue(),
		"context.channel must be preserved (ES-007)")
	Expect(gjson.Get(eventStr, "context.channel").String()).To(Equal("browser"),
		"context.channel value must be preserved as 'browser'")

	// context.locale
	Expect(gjson.Get(eventStr, "context.locale").Exists()).To(BeTrue(), "context.locale must exist")
	Expect(gjson.Get(eventStr, "context.locale").String()).To(Equal("en-US"),
		"context.locale value must be preserved")

	// context.timezone
	Expect(gjson.Get(eventStr, "context.timezone").Exists()).To(BeTrue(), "context.timezone must exist")
	Expect(gjson.Get(eventStr, "context.timezone").String()).To(Equal("America/Los_Angeles"),
		"context.timezone value must be preserved")

	// context.app
	Expect(gjson.Get(eventStr, "context.app.name").Exists()).To(BeTrue(), "context.app.name must exist")
	Expect(gjson.Get(eventStr, "context.app.name").String()).To(Equal("TestApp"),
		"context.app.name value must be preserved")
	Expect(gjson.Get(eventStr, "context.app.version").Exists()).To(BeTrue(), "context.app.version must exist")
	Expect(gjson.Get(eventStr, "context.app.build").Exists()).To(BeTrue(), "context.app.build must exist")

	// context.device
	Expect(gjson.Get(eventStr, "context.device.manufacturer").Exists()).To(BeTrue(), "context.device.manufacturer must exist")
	Expect(gjson.Get(eventStr, "context.device.manufacturer").String()).To(Equal("Apple"),
		"context.device.manufacturer value must be preserved")
	Expect(gjson.Get(eventStr, "context.device.model").Exists()).To(BeTrue(), "context.device.model must exist")
	Expect(gjson.Get(eventStr, "context.device.type").Exists()).To(BeTrue(), "context.device.type must exist")

	// context.os
	Expect(gjson.Get(eventStr, "context.os.name").Exists()).To(BeTrue(), "context.os.name must exist")
	Expect(gjson.Get(eventStr, "context.os.name").String()).To(Equal("iOS"),
		"context.os.name value must be preserved")
	Expect(gjson.Get(eventStr, "context.os.version").Exists()).To(BeTrue(), "context.os.version must exist")

	// context.network
	Expect(gjson.Get(eventStr, "context.network.carrier").Exists()).To(BeTrue(), "context.network.carrier must exist")
	Expect(gjson.Get(eventStr, "context.network.carrier").String()).To(Equal("T-Mobile"),
		"context.network.carrier value must be preserved")
	Expect(gjson.Get(eventStr, "context.network.cellular").Exists()).To(BeTrue(), "context.network.cellular must exist")
	Expect(gjson.Get(eventStr, "context.network.wifi").Exists()).To(BeTrue(), "context.network.wifi must exist")

	// context.page
	Expect(gjson.Get(eventStr, "context.page.url").Exists()).To(BeTrue(), "context.page.url must exist")
	Expect(gjson.Get(eventStr, "context.page.path").Exists()).To(BeTrue(), "context.page.path must exist")
	Expect(gjson.Get(eventStr, "context.page.title").Exists()).To(BeTrue(), "context.page.title must exist")
	Expect(gjson.Get(eventStr, "context.page.referrer").Exists()).To(BeTrue(), "context.page.referrer must exist")

	// context.referrer
	Expect(gjson.Get(eventStr, "context.referrer.type").Exists()).To(BeTrue(), "context.referrer.type must exist")
	Expect(gjson.Get(eventStr, "context.referrer.type").String()).To(Equal("search"),
		"context.referrer.type value must be preserved")

	// context.screen
	Expect(gjson.Get(eventStr, "context.screen.width").Exists()).To(BeTrue(), "context.screen.width must exist")
	Expect(gjson.Get(eventStr, "context.screen.height").Exists()).To(BeTrue(), "context.screen.height must exist")
	Expect(gjson.Get(eventStr, "context.screen.density").Exists()).To(BeTrue(), "context.screen.density must exist")

	// context.campaign
	Expect(gjson.Get(eventStr, "context.campaign.name").Exists()).To(BeTrue(), "context.campaign.name must exist")
	Expect(gjson.Get(eventStr, "context.campaign.name").String()).To(Equal("summer-sale"),
		"context.campaign.name value must be preserved")
	Expect(gjson.Get(eventStr, "context.campaign.source").Exists()).To(BeTrue(), "context.campaign.source must exist")
	Expect(gjson.Get(eventStr, "context.campaign.medium").Exists()).To(BeTrue(), "context.campaign.medium must exist")

	// context.traits
	Expect(gjson.Get(eventStr, "context.traits").Exists()).To(BeTrue(), "context.traits must exist")
	Expect(gjson.Get(eventStr, "context.traits.plan").String()).To(Equal("enterprise"),
		"context.traits.plan value must be preserved")

	// context.groupId
	Expect(gjson.Get(eventStr, "context.groupId").Exists()).To(BeTrue(), "context.groupId must exist")
	Expect(gjson.Get(eventStr, "context.groupId").String()).To(Equal("grp-999"),
		"context.groupId value must be preserved")

	// context.active
	Expect(gjson.Get(eventStr, "context.active").Exists()).To(BeTrue(), "context.active must exist")
	Expect(gjson.Get(eventStr, "context.active").Bool()).To(BeTrue(),
		"context.active value must be preserved as true")

	// ---- integrations object ----
	Expect(gjson.Get(eventStr, "integrations").Exists()).To(BeTrue(), "integrations must exist")
	Expect(gjson.Get(eventStr, "integrations.All").Bool()).To(BeTrue(),
		"integrations.All should default to true")
}

var _ = Describe("Event Spec Parity", Ordered, func() {
	initProcessor()

	var c *testContext

	prepareHandle := func(proc *Handle) *Handle {
		isolationStrategy, err := isolation.GetStrategy(isolation.ModeNone)
		Expect(err).To(BeNil())
		proc.isolationStrategy = isolationStrategy
		proc.config.enableConcurrentStore = config.SingleValueLoader(false)
		return proc
	}

	BeforeEach(func() {
		c = &testContext{}
		c.Setup()
	})

	AfterEach(func() {
		c.Finish()
	})

	// runEventSpecParityTest is a shared helper that:
	//   1. Wraps the given event JSON inside a Gateway batch payload.
	//   2. Injects it as a mock unprocessed job.
	//   3. Runs the full Processor handlePendingGatewayJobs pipeline.
	//   4. Captures the TransformerEvent that arrives at the destination
	//      transform stage.
	//   5. Serialises the captured Message to JSON and returns the string
	//      so the caller can assert field-level preservation.
	runEventSpecParityTest := func(eventJSON string) string {
		// Build batch payload around the single event
		payload := fmt.Appendf(nil,
			`{"writeKey":%q,"batch":[%s],"requestIP":"203.0.113.42","receivedAt":"2024-01-15T10:30:00.000Z"}`,
			WriteKeyEnabledNoUT, eventJSON,
		)

		unprocessedJobsList := []*jobsdb.JobT{
			{
				UUID:          uuid.New(),
				JobID:         8001,
				CreatedAt:     time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				ExpireAt:      time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				CustomVal:     gatewayCustomVal[0],
				EventPayload:  payload,
				EventCount:    1,
				LastJobStatus: jobsdb.JobStatusT{},
				Parameters:    createBatchParameters(SourceIDEnabledNoUT),
				WorkspaceId:   sampleWorkspaceID,
			},
		}

		// Wire up mock transformer clients with a dynamic destination transform
		// that captures events for post-run assertions.
		mockTransformerClients := transformer.NewSimpleClients()
		processor := prepareHandle(NewHandle(config.Default, mockTransformerClients))

		var capturedEvents []types.TransformerEvent
		mockTransformerClients.WithDynamicDestinationTransform(
			func(ctx context.Context, events []types.TransformerEvent) types.Response {
				defer GinkgoRecover()
				capturedEvents = append(capturedEvents, events...)
				// Return a pass-through response so the pipeline completes
				responses := make([]types.TransformerResponse, len(events))
				for i, event := range events {
					responses[i] = types.TransformerResponse{
						Output:   event.Message,
						Metadata: event.Metadata,
					}
				}
				return types.Response{Events: responses}
			},
		)

		// --- Mock expectations ---

		// Crash recovery check — required by processor.Setup internals
		c.mockGatewayJobsDB.EXPECT().DeleteExecuting().Times(1)

		c.mockGatewayJobsDB.EXPECT().GetUnprocessed(
			gomock.Any(),
			jobsdb.GetQueryParams{
				CustomValFilters: gatewayCustomVal,
				JobsLimit:        processor.config.maxEventsToProcess.Load(),
				EventsLimit:      processor.config.maxEventsToProcess.Load(),
				PayloadSizeLimit: processor.payloadLimit.Load(),
			},
		).Return(jobsdb.JobsResult{Jobs: unprocessedJobsList}, nil).Times(1)

		// Router store
		c.mockRouterJobsDB.EXPECT().WithStoreSafeTx(gomock.Any(), gomock.Any()).Times(1).
			Do(func(ctx context.Context, f func(jobsdb.StoreSafeTx) error) {
				_ = f(jobsdb.EmptyStoreSafeTx())
			}).Return(nil)
		callStoreRouter := c.mockRouterJobsDB.EXPECT().StoreInTx(gomock.Any(), gomock.Any(), gomock.Any()).Times(1)

		// Archival
		c.mockArchivalDB.EXPECT().WithStoreSafeTx(gomock.Any(), gomock.Any()).AnyTimes().
			Do(func(ctx context.Context, f func(jobsdb.StoreSafeTx) error) {
				_ = f(jobsdb.EmptyStoreSafeTx())
			}).Return(nil)
		c.mockArchivalDB.EXPECT().StoreInTx(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

		// Gateway status update
		c.mockGatewayJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any(), gomock.Any()).
			Do(func(ctx context.Context, f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
			}).Return(nil).Times(1)
		c.mockGatewayJobsDB.EXPECT().UpdateJobStatusInTx(
			gomock.Any(), gomock.Any(), gomock.Len(len(unprocessedJobsList)),
		).Times(1).After(callStoreRouter).
			Do(func(ctx context.Context, txn jobsdb.UpdateSafeTx, statuses []*jobsdb.JobStatusT) {
				assertJobStatus(unprocessedJobsList[0], statuses[0], jobsdb.Succeeded.State)
			})

		// Execute the processor pipeline
		processorSetupAndAssertJobHandling(processor, c)

		// Assertions on captured events
		Expect(capturedEvents).To(HaveLen(1),
			"exactly one event should reach destination transform")

		// Serialise the captured TransformerEvent.Message for gjson assertions
		eventBytes, err := jsonrs.Marshal(capturedEvents[0].Message)
		Expect(err).To(BeNil(), "should be able to marshal captured event message")
		return string(eventBytes)
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// Identify Event — Full Segment Spec Parity (E-001, ES-003)
	// Reference: refs/segment-docs/src/connections/spec/identify.md
	// ═══════════════════════════════════════════════════════════════════════════
	Context("Identify event field preservation", func() {
		It("should preserve all common fields and 17 reserved identify traits through the pipeline", func() {
			msgID := uuid.New().String()
			ts := "2024-01-15T10:29:59.000Z"
			sentAt := "2024-01-15T10:29:58.000Z"
			origTS := "2024-01-15T10:29:57.000Z"

			eventStr := runEventSpecParityTest(identifyPayload(msgID, ts, sentAt, origTS))

			// Common fields
			assertCommonFieldPreservation(eventStr)

			// Identify-specific: type
			Expect(gjson.Get(eventStr, "type").String()).To(Equal("identify"),
				"event type must be 'identify'")

			// Identify-specific: traits object with all 17 reserved traits
			Expect(gjson.Get(eventStr, "traits").Exists()).To(BeTrue(), "traits must exist")
			Expect(gjson.Get(eventStr, "traits.email").String()).To(Equal("alice@testcorp.example.com"),
				"traits.email must be preserved")
			Expect(gjson.Get(eventStr, "traits.firstName").String()).To(Equal("Alice"),
				"traits.firstName must be preserved")
			Expect(gjson.Get(eventStr, "traits.lastName").String()).To(Equal("Tester"),
				"traits.lastName must be preserved")
			Expect(gjson.Get(eventStr, "traits.name").String()).To(Equal("Alice Tester"),
				"traits.name must be preserved")
			Expect(gjson.Get(eventStr, "traits.phone").String()).To(Equal("+14155551234"),
				"traits.phone must be preserved")
			Expect(gjson.Get(eventStr, "traits.username").String()).To(Equal("alice_tester"),
				"traits.username must be preserved")
			Expect(gjson.Get(eventStr, "traits.website").String()).To(Equal("https://alice.example.com"),
				"traits.website must be preserved")
			Expect(gjson.Get(eventStr, "traits.title").String()).To(Equal("Sr. Engineer"),
				"traits.title must be preserved")
			Expect(gjson.Get(eventStr, "traits.gender").String()).To(Equal("female"),
				"traits.gender must be preserved")
			Expect(gjson.Get(eventStr, "traits.age").Int()).To(BeNumerically("==", 32),
				"traits.age must be preserved")
			Expect(gjson.Get(eventStr, "traits.birthday").String()).To(Equal("1992-06-15"),
				"traits.birthday must be preserved")
			Expect(gjson.Get(eventStr, "traits.avatar").String()).To(Equal("https://example.com/avatars/alice.png"),
				"traits.avatar must be preserved")
			Expect(gjson.Get(eventStr, "traits.description").String()).To(Equal("A test user for Segment Spec parity validation"),
				"traits.description must be preserved")
			Expect(gjson.Get(eventStr, "traits.id").String()).To(Equal("user-spec-parity-42"),
				"traits.id must be preserved")
			Expect(gjson.Get(eventStr, "traits.createdAt").String()).To(Equal("2023-01-15T08:00:00.000Z"),
				"traits.createdAt must be preserved")
			Expect(gjson.Get(eventStr, "traits.address.city").String()).To(Equal("San Francisco"),
				"traits.address.city must be preserved")
			Expect(gjson.Get(eventStr, "traits.address.country").String()).To(Equal("US"),
				"traits.address.country must be preserved")
			Expect(gjson.Get(eventStr, "traits.company.name").String()).To(Equal("TestCorp"),
				"traits.company.name must be preserved")
			Expect(gjson.Get(eventStr, "traits.company.industry").String()).To(Equal("Technology"),
				"traits.company.industry must be preserved")
		})
	})

	// ═══════════════════════════════════════════════════════════════════════════
	// Track Event — Full Segment Spec Parity (E-001, ES-002)
	// Reference: refs/segment-docs/src/connections/spec/track.md
	// ═══════════════════════════════════════════════════════════════════════════
	Context("Track event field preservation", func() {
		It("should preserve all common fields, event name, and properties through the pipeline", func() {
			msgID := uuid.New().String()
			ts := "2024-01-15T10:29:59.000Z"
			sentAt := "2024-01-15T10:29:58.000Z"
			origTS := "2024-01-15T10:29:57.000Z"

			eventStr := runEventSpecParityTest(trackPayload(msgID, ts, sentAt, origTS))

			// Common fields
			assertCommonFieldPreservation(eventStr)

			// Track-specific: type
			Expect(gjson.Get(eventStr, "type").String()).To(Equal("track"),
				"event type must be 'track'")

			// Track-specific: event name
			Expect(gjson.Get(eventStr, "event").Exists()).To(BeTrue(), "event must exist")
			Expect(gjson.Get(eventStr, "event").String()).To(Equal("Product Viewed"),
				"event name must be preserved exactly")

			// Track-specific: properties
			Expect(gjson.Get(eventStr, "properties").Exists()).To(BeTrue(), "properties must exist")
			Expect(gjson.Get(eventStr, "properties.product_id").String()).To(Equal("prod-spec-001"),
				"properties.product_id must be preserved")
			Expect(gjson.Get(eventStr, "properties.sku").String()).To(Equal("SKU-SPEC-A"),
				"properties.sku must be preserved")
			Expect(gjson.Get(eventStr, "properties.name").String()).To(Equal("Spec Parity Widget"),
				"properties.name must be preserved")
			Expect(gjson.Get(eventStr, "properties.category").String()).To(Equal("Widgets"),
				"properties.category must be preserved")
			Expect(gjson.Get(eventStr, "properties.price").Float()).To(BeNumerically("==", 49.99),
				"properties.price must be preserved")
			Expect(gjson.Get(eventStr, "properties.brand").String()).To(Equal("WidgetCo"),
				"properties.brand must be preserved")
			Expect(gjson.Get(eventStr, "properties.variant").String()).To(Equal("Blue"),
				"properties.variant must be preserved")
			Expect(gjson.Get(eventStr, "properties.quantity").Int()).To(BeNumerically("==", 2),
				"properties.quantity must be preserved")
			Expect(gjson.Get(eventStr, "properties.coupon").String()).To(Equal("PARITY10"),
				"properties.coupon must be preserved")
			Expect(gjson.Get(eventStr, "properties.revenue").Float()).To(BeNumerically("==", 89.98),
				"properties.revenue must be preserved")
			Expect(gjson.Get(eventStr, "properties.currency").String()).To(Equal("USD"),
				"properties.currency must be preserved")
		})
	})

	// ═══════════════════════════════════════════════════════════════════════════
	// Page Event — Full Segment Spec Parity (E-001)
	// Reference: refs/segment-docs/src/connections/spec/page.md
	// ═══════════════════════════════════════════════════════════════════════════
	Context("Page event field preservation", func() {
		It("should preserve all common fields, name, category, and page properties through the pipeline", func() {
			msgID := uuid.New().String()
			ts := "2024-01-15T10:29:59.000Z"
			sentAt := "2024-01-15T10:29:58.000Z"
			origTS := "2024-01-15T10:29:57.000Z"

			eventStr := runEventSpecParityTest(pagePayload(msgID, ts, sentAt, origTS))

			// Common fields
			assertCommonFieldPreservation(eventStr)

			// Page-specific: type
			Expect(gjson.Get(eventStr, "type").String()).To(Equal("page"),
				"event type must be 'page'")

			// Page-specific: name
			Expect(gjson.Get(eventStr, "name").Exists()).To(BeTrue(), "name must exist")
			Expect(gjson.Get(eventStr, "name").String()).To(Equal("Premium Widget"),
				"page name must be preserved")

			// Page-specific: category
			Expect(gjson.Get(eventStr, "category").Exists()).To(BeTrue(), "category must exist")
			Expect(gjson.Get(eventStr, "category").String()).To(Equal("Product Pages"),
				"page category must be preserved")

			// Page-specific: properties
			Expect(gjson.Get(eventStr, "properties").Exists()).To(BeTrue(), "properties must exist")
			Expect(gjson.Get(eventStr, "properties.title").String()).To(Equal("Premium Widget | TestStore"),
				"properties.title must be preserved")
			Expect(gjson.Get(eventStr, "properties.url").String()).To(Equal("https://teststore.example.com/products/widget?color=blue"),
				"properties.url must be preserved")
			Expect(gjson.Get(eventStr, "properties.path").String()).To(Equal("/products/widget"),
				"properties.path must be preserved")
			Expect(gjson.Get(eventStr, "properties.referrer").String()).To(Equal("https://google.com/search?q=widget"),
				"properties.referrer must be preserved")
			Expect(gjson.Get(eventStr, "properties.search").String()).To(Equal("?color=blue"),
				"properties.search must be preserved")
			Expect(gjson.Get(eventStr, "properties.keywords").IsArray()).To(BeTrue(),
				"properties.keywords must be an array")
			Expect(gjson.Get(eventStr, "properties.keywords.#").Int()).To(BeNumerically("==", 3),
				"properties.keywords should have 3 entries")
		})
	})

	// ═══════════════════════════════════════════════════════════════════════════
	// Screen Event — Full Segment Spec Parity (E-001)
	// Reference: refs/segment-docs/src/connections/spec/screen.md
	// ═══════════════════════════════════════════════════════════════════════════
	Context("Screen event field preservation", func() {
		It("should preserve all common fields, name, category, and screen properties through the pipeline", func() {
			msgID := uuid.New().String()
			ts := "2024-01-15T10:29:59.000Z"
			sentAt := "2024-01-15T10:29:58.000Z"
			origTS := "2024-01-15T10:29:57.000Z"

			eventStr := runEventSpecParityTest(screenPayload(msgID, ts, sentAt, origTS))

			// Common fields
			assertCommonFieldPreservation(eventStr)

			// Screen-specific: type
			Expect(gjson.Get(eventStr, "type").String()).To(Equal("screen"),
				"event type must be 'screen'")

			// Screen-specific: name
			Expect(gjson.Get(eventStr, "name").Exists()).To(BeTrue(), "name must exist")
			Expect(gjson.Get(eventStr, "name").String()).To(Equal("ProductDetail"),
				"screen name must be preserved")

			// Screen-specific: category
			Expect(gjson.Get(eventStr, "category").Exists()).To(BeTrue(), "category must exist")
			Expect(gjson.Get(eventStr, "category").String()).To(Equal("Product"),
				"screen category must be preserved")

			// Screen-specific: properties
			Expect(gjson.Get(eventStr, "properties").Exists()).To(BeTrue(), "properties must exist")
			Expect(gjson.Get(eventStr, "properties.variation").String()).To(Equal("blue-variant"),
				"properties.variation must be preserved")
			Expect(gjson.Get(eventStr, "properties.loginStatus").Bool()).To(BeTrue(),
				"properties.loginStatus must be preserved")
			Expect(gjson.Get(eventStr, "properties.itemCount").Int()).To(BeNumerically("==", 5),
				"properties.itemCount must be preserved")
		})
	})

	// ═══════════════════════════════════════════════════════════════════════════
	// Group Event — Full Segment Spec Parity (E-001, ES-003)
	// Reference: refs/segment-docs/src/connections/spec/group.md
	// ═══════════════════════════════════════════════════════════════════════════
	Context("Group event field preservation", func() {
		It("should preserve all common fields, groupId, and 12 reserved group traits through the pipeline", func() {
			msgID := uuid.New().String()
			ts := "2024-01-15T10:29:59.000Z"
			sentAt := "2024-01-15T10:29:58.000Z"
			origTS := "2024-01-15T10:29:57.000Z"

			eventStr := runEventSpecParityTest(groupPayload(msgID, ts, sentAt, origTS))

			// Common fields
			assertCommonFieldPreservation(eventStr)

			// Group-specific: type
			Expect(gjson.Get(eventStr, "type").String()).To(Equal("group"),
				"event type must be 'group'")

			// Group-specific: groupId
			Expect(gjson.Get(eventStr, "groupId").Exists()).To(BeTrue(), "groupId must exist")
			Expect(gjson.Get(eventStr, "groupId").String()).To(Equal("grp-spec-parity-001"),
				"groupId must be preserved")

			// Group-specific: traits with all 12 reserved group traits
			Expect(gjson.Get(eventStr, "traits").Exists()).To(BeTrue(), "traits must exist")
			Expect(gjson.Get(eventStr, "traits.name").String()).To(Equal("TestCorp Inc."),
				"traits.name must be preserved")
			Expect(gjson.Get(eventStr, "traits.email").String()).To(Equal("contact@testcorp.example.com"),
				"traits.email must be preserved")
			Expect(gjson.Get(eventStr, "traits.phone").String()).To(Equal("+14155559876"),
				"traits.phone must be preserved")
			Expect(gjson.Get(eventStr, "traits.website").String()).To(Equal("https://testcorp.example.com"),
				"traits.website must be preserved")
			Expect(gjson.Get(eventStr, "traits.industry").String()).To(Equal("Technology"),
				"traits.industry must be preserved")
			Expect(gjson.Get(eventStr, "traits.employees").String()).To(Equal("500"),
				"traits.employees must be preserved as String per Segment Spec")
			Expect(gjson.Get(eventStr, "traits.plan").String()).To(Equal("enterprise"),
				"traits.plan must be preserved")
			Expect(gjson.Get(eventStr, "traits.id").String()).To(Equal("grp-spec-parity-001"),
				"traits.id must be preserved")
			Expect(gjson.Get(eventStr, "traits.avatar").String()).To(Equal("https://example.com/logos/testcorp.png"),
				"traits.avatar must be preserved")
			Expect(gjson.Get(eventStr, "traits.description").String()).To(Equal("A technology company for testing"),
				"traits.description must be preserved")
			Expect(gjson.Get(eventStr, "traits.createdAt").String()).To(Equal("2020-03-10T12:00:00.000Z"),
				"traits.createdAt must be preserved")
			Expect(gjson.Get(eventStr, "traits.address.city").String()).To(Equal("San Francisco"),
				"traits.address.city must be preserved")
		})
	})

	// ═══════════════════════════════════════════════════════════════════════════
	// Alias Event — Full Segment Spec Parity (E-001)
	// Reference: refs/segment-docs/src/connections/spec/alias.md
	// ═══════════════════════════════════════════════════════════════════════════
	Context("Alias event field preservation", func() {
		It("should preserve all common fields, previousId, and userId through the pipeline", func() {
			msgID := uuid.New().String()
			ts := "2024-01-15T10:29:59.000Z"
			sentAt := "2024-01-15T10:29:58.000Z"
			origTS := "2024-01-15T10:29:57.000Z"

			eventStr := runEventSpecParityTest(aliasPayload(msgID, ts, sentAt, origTS))

			// Common fields
			assertCommonFieldPreservation(eventStr)

			// Alias-specific: type
			Expect(gjson.Get(eventStr, "type").String()).To(Equal("alias"),
				"event type must be 'alias'")

			// Alias-specific: previousId
			Expect(gjson.Get(eventStr, "previousId").Exists()).To(BeTrue(), "previousId must exist")
			Expect(gjson.Get(eventStr, "previousId").String()).To(Equal("old-anon-id-999"),
				"previousId must be preserved")

			// Alias-specific: userId
			Expect(gjson.Get(eventStr, "userId").String()).To(Equal("user-spec-parity-42"),
				"userId must be preserved for alias events")
		})
	})

	// ═══════════════════════════════════════════════════════════════════════════
	// Channel Field Auto-Population — ES-007
	// Verify that context.channel values "server", "browser", "mobile" are
	// preserved through the pipeline.
	// ═══════════════════════════════════════════════════════════════════════════
	Context("Channel field preservation (ES-007)", func() {
		for _, channelVal := range []string{"server", "browser", "mobile"} {
			It(fmt.Sprintf("should preserve context.channel=%q through the pipeline", channelVal), func() {
				msgID := uuid.New().String()
				ts := "2024-01-15T10:29:59.000Z"
				sentAt := "2024-01-15T10:29:58.000Z"
				origTS := "2024-01-15T10:29:57.000Z"

				// Build a track payload and override the channel value
				eventJSON := trackPayload(msgID, ts, sentAt, origTS)
				eventJSON = mustSet(eventJSON, "channel", channelVal)
				eventJSON = mustSet(eventJSON, "context.channel", channelVal)

				eventStr := runEventSpecParityTest(eventJSON)

				// Verify the channel is preserved
				Expect(gjson.Get(eventStr, "context.channel").Exists()).To(BeTrue(),
					"context.channel must exist")
				Expect(gjson.Get(eventStr, "context.channel").String()).To(Equal(channelVal),
					fmt.Sprintf("context.channel value must be preserved as %q", channelVal))
			})
		}
	})
})
