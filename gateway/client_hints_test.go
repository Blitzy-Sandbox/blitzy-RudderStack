package gateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	"github.com/rudderlabs/rudder-server/jobsdb"
	sourcedebugger "github.com/rudderlabs/rudder-server/services/debugger/source"
	"github.com/rudderlabs/rudder-server/services/rsources"
	"github.com/rudderlabs/rudder-server/services/transformer"

	"go.uber.org/mock/gomock"
)

// clientHintsMockSetup configures the common mock expectations for job storage
// used across all Client Hints pass-through test cases. It returns a pointer to
// the captured jobs slice so callers can inspect stored payloads after the
// Gateway handler processes a request.
func clientHintsMockSetup(c *testContext) *[][]*jobsdb.JobT {
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

// extractBatchEvent extracts the first event from the first captured job's
// EventPayload using gjson. The stored payload structure is:
//
//	{"batch":[<event>], "writeKey":"...", "requestIP":"...", "receivedAt":"..."}
func extractBatchEvent(capturedJobs [][]*jobsdb.JobT) gjson.Result {
	Expect(capturedJobs).ToNot(BeEmpty(), "captured jobs should not be empty")
	Expect(capturedJobs[0]).ToNot(BeEmpty(), "first job batch should not be empty")
	storedPayload := string(capturedJobs[0][0].EventPayload)
	return gjson.Get(storedPayload, "batch.0")
}

var _ = Describe("Client Hints Pass-Through", func() {
	initGW()

	var (
		c             *testContext
		clientHintsGW *Handle
		chStatsStore  *memstats.Store
		chConf        *config.Config
	)

	BeforeEach(func() {
		c = &testContext{}
		c.Setup()
		c.initializeAppFeatures()

		var err error
		chStatsStore, err = memstats.New()
		Expect(err).To(BeNil())

		chConf = config.New()
		chConf.Set("Gateway.enableRateLimit", false)
		chConf.Set("Gateway.enableSuppressUserFeature", false)

		clientHintsGW = &Handle{}
		err = clientHintsGW.Setup(
			context.Background(),
			chConf,
			logger.NOP,
			chStatsStore,
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
		waitForBackendConfigInit(clientHintsGW)
	})

	AfterEach(func() {
		Expect(clientHintsGW.Shutdown()).To(BeNil())
		c.Finish()
	})

	// -------------------------------------------------------------------------
	// Context 1: Low-entropy Client Hints (required fields only)
	// -------------------------------------------------------------------------
	Context("Low-entropy Client Hints", func() {
		It("should preserve brands, mobile, and platform fields", func() {
			capturedJobsPtr := clientHintsMockSetup(c)

			payload := fmt.Sprintf(`{
				"batch": [{
					"userId": "client-hints-user-001",
					"type": "track",
					"event": "Low Entropy Test",
					"context": {
						"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
						"userAgentData": {
							"brands": [
								{"brand": "Chromium", "version": "110"},
								{"brand": "Google Chrome", "version": "110"},
								{"brand": "Not?A_Brand", "version": "24"}
							],
							"mobile": false,
							"platform": "macOS"
						},
						"library": {"name": %q, "version": %q}
					}
				}]
			}`, sdkLibrary, sdkVersion)

			expectHandlerResponse(
				clientHintsGW.webBatchHandler(),
				authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
				http.StatusOK,
				"ok",
				"batch",
			)

			batchEvent := extractBatchEvent(*capturedJobsPtr)

			// Verify userAgentData object exists
			Expect(batchEvent.Get("context.userAgentData").Exists()).To(BeTrue())

			// Verify brands array
			Expect(batchEvent.Get("context.userAgentData.brands").IsArray()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.brands.#").Int()).To(Equal(int64(3)))
			Expect(batchEvent.Get("context.userAgentData.brands.0.brand").String()).To(Equal("Chromium"))
			Expect(batchEvent.Get("context.userAgentData.brands.0.version").String()).To(Equal("110"))
			Expect(batchEvent.Get("context.userAgentData.brands.1.brand").String()).To(Equal("Google Chrome"))
			Expect(batchEvent.Get("context.userAgentData.brands.1.version").String()).To(Equal("110"))
			Expect(batchEvent.Get("context.userAgentData.brands.2.brand").String()).To(Equal("Not?A_Brand"))
			Expect(batchEvent.Get("context.userAgentData.brands.2.version").String()).To(Equal("24"))

			// Verify mobile boolean (false)
			Expect(batchEvent.Get("context.userAgentData.mobile").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.mobile").Bool()).To(BeFalse())

			// Verify platform string
			Expect(batchEvent.Get("context.userAgentData.platform").String()).To(Equal("macOS"))
		})
	})

	// -------------------------------------------------------------------------
	// Context 2: High-entropy Client Hints (all optional fields)
	// -------------------------------------------------------------------------
	Context("High-entropy Client Hints", func() {
		It("should preserve all high-entropy fields alongside low-entropy fields", func() {
			capturedJobsPtr := clientHintsMockSetup(c)

			payload := fmt.Sprintf(`{
				"batch": [{
					"userId": "client-hints-user-002",
					"type": "track",
					"event": "High Entropy Test",
					"context": {
						"userAgent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
						"userAgentData": {
							"brands": [
								{"brand": "Chromium", "version": "110"},
								{"brand": "Google Chrome", "version": "110"}
							],
							"mobile": false,
							"platform": "Windows",
							"bitness": "64",
							"model": "",
							"platformVersion": "15.0.0",
							"uaFullVersion": "110.0.5481.178",
							"fullVersionList": [
								{"brand": "Chromium", "version": "110.0.5481.178"},
								{"brand": "Google Chrome", "version": "110.0.5481.178"}
							],
							"wow64": false
						},
						"library": {"name": %q, "version": %q}
					}
				}]
			}`, sdkLibrary, sdkVersion)

			expectHandlerResponse(
				clientHintsGW.webBatchHandler(),
				authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
				http.StatusOK,
				"ok",
				"batch",
			)

			batchEvent := extractBatchEvent(*capturedJobsPtr)

			// Low-entropy fields still present
			Expect(batchEvent.Get("context.userAgentData.brands").IsArray()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.brands.#").Int()).To(Equal(int64(2)))
			Expect(batchEvent.Get("context.userAgentData.mobile").Bool()).To(BeFalse())
			Expect(batchEvent.Get("context.userAgentData.platform").String()).To(Equal("Windows"))

			// High-entropy: bitness
			Expect(batchEvent.Get("context.userAgentData.bitness").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.bitness").String()).To(Equal("64"))

			// High-entropy: model (empty string preserved)
			Expect(batchEvent.Get("context.userAgentData.model").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.model").String()).To(Equal(""))

			// High-entropy: platformVersion
			Expect(batchEvent.Get("context.userAgentData.platformVersion").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.platformVersion").String()).To(Equal("15.0.0"))

			// High-entropy: uaFullVersion
			Expect(batchEvent.Get("context.userAgentData.uaFullVersion").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.uaFullVersion").String()).To(Equal("110.0.5481.178"))

			// High-entropy: fullVersionList array
			Expect(batchEvent.Get("context.userAgentData.fullVersionList").IsArray()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.fullVersionList.#").Int()).To(Equal(int64(2)))
			Expect(batchEvent.Get("context.userAgentData.fullVersionList.0.brand").String()).To(Equal("Chromium"))
			Expect(batchEvent.Get("context.userAgentData.fullVersionList.0.version").String()).To(Equal("110.0.5481.178"))
			Expect(batchEvent.Get("context.userAgentData.fullVersionList.1.brand").String()).To(Equal("Google Chrome"))
			Expect(batchEvent.Get("context.userAgentData.fullVersionList.1.version").String()).To(Equal("110.0.5481.178"))

			// High-entropy: wow64 boolean (false)
			Expect(batchEvent.Get("context.userAgentData.wow64").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.wow64").Bool()).To(BeFalse())
		})
	})

	// -------------------------------------------------------------------------
	// Context 3: UserAgent string coexistence with userAgentData object
	// -------------------------------------------------------------------------
	Context("UserAgent string coexistence", func() {
		It("should preserve both userAgent string and userAgentData object simultaneously", func() {
			capturedJobsPtr := clientHintsMockSetup(c)

			payload := fmt.Sprintf(`{
				"batch": [{
					"userId": "client-hints-user-003",
					"type": "track",
					"event": "Coexistence Test",
					"context": {
						"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Safari/537.36",
						"userAgentData": {
							"brands": [
								{"brand": "Chromium", "version": "110"},
								{"brand": "Google Chrome", "version": "110"}
							],
							"mobile": false,
							"platform": "macOS"
						},
						"library": {"name": %q, "version": %q}
					}
				}]
			}`, sdkLibrary, sdkVersion)

			expectHandlerResponse(
				clientHintsGW.webBatchHandler(),
				authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
				http.StatusOK,
				"ok",
				"batch",
			)

			batchEvent := extractBatchEvent(*capturedJobsPtr)

			// Verify userAgent string is preserved
			Expect(batchEvent.Get("context.userAgent").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgent").String()).To(ContainSubstring("AppleWebKit"))
			Expect(batchEvent.Get("context.userAgent").String()).To(ContainSubstring("Chrome/110"))

			// Verify userAgentData object is also preserved
			Expect(batchEvent.Get("context.userAgentData").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.brands").IsArray()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.brands.#").Int()).To(Equal(int64(2)))
			Expect(batchEvent.Get("context.userAgentData.mobile").Bool()).To(BeFalse())
			Expect(batchEvent.Get("context.userAgentData.platform").String()).To(Equal("macOS"))

			// Verify both fields are independent — userAgent is a string, userAgentData is an object
			Expect(batchEvent.Get("context.userAgent").Type.String()).To(Equal("String"))
			Expect(batchEvent.Get("context.userAgentData").Type.String()).To(Equal("JSON"))
		})
	})

	// -------------------------------------------------------------------------
	// Context 4: Client Hints across all 6 event types
	// -------------------------------------------------------------------------
	Context("Client Hints across all event types", func() {
		type eventTypeFixture struct {
			eventType    string
			typeSpecific string // event-type-specific JSON fields
		}
		eventFixtures := []eventTypeFixture{
			{
				eventType:    "identify",
				typeSpecific: `"userId":"ch-u1","traits":{"email":"ch-test@example.com"}`,
			},
			{
				eventType:    "track",
				typeSpecific: `"userId":"ch-u1","event":"CH Test Event","properties":{"key":"value"}`,
			},
			{
				eventType:    "page",
				typeSpecific: `"userId":"ch-u1","name":"CH Home","properties":{"title":"CH Home Page"}`,
			},
			{
				eventType:    "screen",
				typeSpecific: `"userId":"ch-u1","name":"CH Main Screen","properties":{"variation":"blue"}`,
			},
			{
				eventType:    "group",
				typeSpecific: `"userId":"ch-u1","groupId":"ch-g1","traits":{"name":"CH Test Group"}`,
			},
			{
				eventType:    "alias",
				typeSpecific: `"userId":"ch-u1","previousId":"ch-anon1"`,
			},
		}

		for _, fixture := range eventFixtures {
			fixture := fixture // capture range variable for goroutine safety
			It(fmt.Sprintf("should preserve userAgentData for %s events", fixture.eventType), func() {
				capturedJobsPtr := clientHintsMockSetup(c)

				payload := fmt.Sprintf(`{
					"batch": [{
						"type": %q,
						%s,
						"context": {
							"userAgentData": {
								"brands": [{"brand": "Chrome", "version": "110"}],
								"mobile": false,
								"platform": "macOS"
							},
							"library": {"name": %q, "version": %q}
						}
					}]
				}`, fixture.eventType, fixture.typeSpecific, sdkLibrary, sdkVersion)

				expectHandlerResponse(
					clientHintsGW.webBatchHandler(),
					authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
					http.StatusOK,
					"ok",
					"batch",
				)

				batchEvent := extractBatchEvent(*capturedJobsPtr)

				// Verify userAgentData is preserved regardless of event type
				Expect(batchEvent.Get("context.userAgentData").Exists()).To(
					BeTrue(),
					fmt.Sprintf("userAgentData missing for %s event type", fixture.eventType),
				)
				Expect(batchEvent.Get("context.userAgentData.brands").IsArray()).To(
					BeTrue(),
					fmt.Sprintf("brands not an array for %s event type", fixture.eventType),
				)
				Expect(batchEvent.Get("context.userAgentData.brands.#").Int()).To(
					Equal(int64(1)),
					fmt.Sprintf("brands count mismatch for %s event type", fixture.eventType),
				)
				Expect(batchEvent.Get("context.userAgentData.brands.0.brand").String()).To(
					Equal("Chrome"),
					fmt.Sprintf("brand name mismatch for %s event type", fixture.eventType),
				)
				Expect(batchEvent.Get("context.userAgentData.brands.0.version").String()).To(
					Equal("110"),
					fmt.Sprintf("brand version mismatch for %s event type", fixture.eventType),
				)
				Expect(batchEvent.Get("context.userAgentData.mobile").Bool()).To(
					BeFalse(),
					fmt.Sprintf("mobile flag mismatch for %s event type", fixture.eventType),
				)
				Expect(batchEvent.Get("context.userAgentData.platform").String()).To(
					Equal("macOS"),
					fmt.Sprintf("platform mismatch for %s event type", fixture.eventType),
				)

				// Verify the event type itself is correct
				Expect(batchEvent.Get("type").String()).To(Equal(fixture.eventType))
			})
		}
	})

	// -------------------------------------------------------------------------
	// Context 5: Mobile Client Hints
	// -------------------------------------------------------------------------
	Context("Mobile Client Hints", func() {
		It("should preserve mobile Client Hints data with mobile true", func() {
			capturedJobsPtr := clientHintsMockSetup(c)

			payload := fmt.Sprintf(`{
				"batch": [{
					"userId": "mobile-user-001",
					"type": "track",
					"event": "Mobile Event",
					"context": {
						"userAgent": "Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Mobile Safari/537.36",
						"userAgentData": {
							"brands": [
								{"brand": "Chromium", "version": "110"},
								{"brand": "Google Chrome", "version": "110"}
							],
							"mobile": true,
							"platform": "Android",
							"model": "Pixel 7",
							"platformVersion": "13.0.0"
						},
						"library": {"name": %q, "version": %q}
					}
				}]
			}`, sdkLibrary, sdkVersion)

			expectHandlerResponse(
				clientHintsGW.webBatchHandler(),
				authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
				http.StatusOK,
				"ok",
				"batch",
			)

			batchEvent := extractBatchEvent(*capturedJobsPtr)

			// Verify mobile == true (critical: boolean true not just truthy)
			Expect(batchEvent.Get("context.userAgentData.mobile").Exists()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.mobile").Bool()).To(BeTrue())

			// Verify Android-specific fields
			Expect(batchEvent.Get("context.userAgentData.platform").String()).To(Equal("Android"))
			Expect(batchEvent.Get("context.userAgentData.model").String()).To(Equal("Pixel 7"))
			Expect(batchEvent.Get("context.userAgentData.platformVersion").String()).To(Equal("13.0.0"))

			// Verify brands array preserved
			Expect(batchEvent.Get("context.userAgentData.brands").IsArray()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.brands.#").Int()).To(Equal(int64(2)))
			Expect(batchEvent.Get("context.userAgentData.brands.0.brand").String()).To(Equal("Chromium"))

			// Verify userAgent string also preserved alongside mobile hints
			Expect(batchEvent.Get("context.userAgent").String()).To(ContainSubstring("Android"))
			Expect(batchEvent.Get("context.userAgent").String()).To(ContainSubstring("Pixel 7"))
		})
	})

	// -------------------------------------------------------------------------
	// Context 6: Bot detection with Client Hints present
	// -------------------------------------------------------------------------
	Context("Bot detection with Client Hints", func() {
		It("should still detect bots based on userAgent string when userAgentData is present", func() {
			capturedJobsPtr := clientHintsMockSetup(c)

			payload := fmt.Sprintf(`{
				"batch": [
					{
						"userId": "bot-user-ch",
						"type": "track",
						"event": "Bot Event With Hints",
						"context": {
							"userAgent": "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
							"userAgentData": {
								"brands": [{"brand": "Googlebot", "version": "2.1"}],
								"mobile": false,
								"platform": "Linux"
							},
							"library": {"name": %[1]q, "version": %[2]q}
						}
					},
					{
						"userId": "normal-user-ch",
						"type": "track",
						"event": "Normal Event With Hints",
						"context": {
							"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
							"userAgentData": {
								"brands": [{"brand": "Chrome", "version": "110"}],
								"mobile": false,
								"platform": "macOS"
							},
							"library": {"name": %[1]q, "version": %[2]q}
						}
					}
				]
			}`, sdkLibrary, sdkVersion)

			expectHandlerResponse(
				clientHintsGW.webBatchHandler(),
				authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
				http.StatusOK,
				"ok",
				"batch",
			)

			// Verify bot events metric: exactly 1 bot event detected
			// The Gateway detects bots via context.userAgent STRING, not userAgentData
			chBotTags := stats.Tags{
				"source":        rCtxEnabled.SourceTag(),
				"sourceID":      rCtxEnabled.SourceID,
				"workspaceId":   rCtxEnabled.WorkspaceID,
				"writeKey":      rCtxEnabled.WriteKey,
				"reqType":       "batch",
				"sourceType":    rCtxEnabled.SourceCategory,
				"sdkVersion":    sdkStatTag,
				"sourceDefName": rCtxEnabled.SourceDefName,
			}

			Eventually(func() bool {
				stat := chStatsStore.Get("gateway.write_key_bot_events", chBotTags)
				return stat != nil && stat.LastValue() == float64(1)
			}, 2*time.Second).Should(BeTrue(), "expected exactly 1 bot event detected")

			// Verify total events metric: 2 events total
			Eventually(func() bool {
				stat := chStatsStore.Get("gateway.write_key_events", chBotTags)
				return stat != nil && stat.LastValue() == float64(2)
			}, 2*time.Second).Should(BeTrue(), "expected 2 total events")

			// Verify userAgentData is preserved for BOTH events (bot and non-bot).
			// The Gateway splits batch events by userId into separate job batches,
			// so each user's events are stored in their own job entry. We iterate
			// over all captured job batches to find both the bot and normal events.
			Expect(*capturedJobsPtr).ToNot(BeEmpty())

			var foundBot, foundNormal bool
			for _, batch := range *capturedJobsPtr {
				for _, job := range batch {
					payload := string(job.EventPayload)
					evt := gjson.Get(payload, "batch.0")
					userID := evt.Get("userId").String()
					switch userID {
					case "bot-user-ch":
						foundBot = true
						Expect(evt.Get("context.userAgentData").Exists()).To(BeTrue(),
							"bot event should preserve userAgentData")
						Expect(evt.Get("context.userAgentData.brands.0.brand").String()).To(Equal("Googlebot"))
						Expect(evt.Get("context.userAgentData.platform").String()).To(Equal("Linux"))
					case "normal-user-ch":
						foundNormal = true
						Expect(evt.Get("context.userAgentData").Exists()).To(BeTrue(),
							"normal event should preserve userAgentData")
						Expect(evt.Get("context.userAgentData.brands.0.brand").String()).To(Equal("Chrome"))
						Expect(evt.Get("context.userAgentData.platform").String()).To(Equal("macOS"))
					}
				}
			}
			Expect(foundBot).To(BeTrue(), "bot event job should be captured")
			Expect(foundNormal).To(BeTrue(), "normal event job should be captured")
		})
	})

	// -------------------------------------------------------------------------
	// Context 7: Edge cases — empty brands array and null/missing optional fields
	// Validates ES-001 robustness for boundary conditions in userAgentData.
	// -------------------------------------------------------------------------
	Context("Edge cases for userAgentData", func() {
		It("should preserve an empty brands array without error", func() {
			capturedJobsPtr := clientHintsMockSetup(c)

			payload := fmt.Sprintf(`{
				"batch": [{
					"userId": "edge-case-user-001",
					"type": "track",
					"event": "Empty Brands Test",
					"context": {
						"userAgentData": {
							"brands": [],
							"mobile": false,
							"platform": "macOS"
						},
						"library": {"name": %q, "version": %q}
					}
				}]
			}`, sdkLibrary, sdkVersion)

			expectHandlerResponse(
				clientHintsGW.webBatchHandler(),
				authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
				http.StatusOK,
				"ok",
				"batch",
			)

			batchEvent := extractBatchEvent(*capturedJobsPtr)

			// Verify userAgentData object exists
			Expect(batchEvent.Get("context.userAgentData").Exists()).To(BeTrue(),
				"userAgentData should exist even with empty brands")

			// Verify brands is an empty array (not null, not missing)
			Expect(batchEvent.Get("context.userAgentData.brands").Exists()).To(BeTrue(),
				"brands field should exist")
			Expect(batchEvent.Get("context.userAgentData.brands").IsArray()).To(BeTrue(),
				"brands should still be an array")
			Expect(batchEvent.Get("context.userAgentData.brands.#").Int()).To(Equal(int64(0)),
				"brands array should be empty")

			// Verify required fields preserved
			Expect(batchEvent.Get("context.userAgentData.mobile").Bool()).To(BeFalse())
			Expect(batchEvent.Get("context.userAgentData.platform").String()).To(Equal("macOS"))
		})

		It("should preserve userAgentData when optional high-entropy fields are null", func() {
			capturedJobsPtr := clientHintsMockSetup(c)

			payload := fmt.Sprintf(`{
				"batch": [{
					"userId": "edge-case-user-002",
					"type": "track",
					"event": "Null Optional Fields Test",
					"context": {
						"userAgentData": {
							"brands": [{"brand": "Chromium", "version": "110"}],
							"mobile": false,
							"platform": "macOS",
							"bitness": null,
							"model": null,
							"platformVersion": null,
							"uaFullVersion": null,
							"fullVersionList": null,
							"wow64": null
						},
						"library": {"name": %q, "version": %q}
					}
				}]
			}`, sdkLibrary, sdkVersion)

			expectHandlerResponse(
				clientHintsGW.webBatchHandler(),
				authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(payload)),
				http.StatusOK,
				"ok",
				"batch",
			)

			batchEvent := extractBatchEvent(*capturedJobsPtr)

			// Verify userAgentData object is preserved
			Expect(batchEvent.Get("context.userAgentData").Exists()).To(BeTrue(),
				"userAgentData should exist with null optional fields")

			// Verify required low-entropy fields
			Expect(batchEvent.Get("context.userAgentData.brands").IsArray()).To(BeTrue())
			Expect(batchEvent.Get("context.userAgentData.brands.#").Int()).To(Equal(int64(1)))
			Expect(batchEvent.Get("context.userAgentData.brands.0.brand").String()).To(Equal("Chromium"))
			Expect(batchEvent.Get("context.userAgentData.mobile").Bool()).To(BeFalse())
			Expect(batchEvent.Get("context.userAgentData.platform").String()).To(Equal("macOS"))

			// Verify null optional fields are preserved as null (not stripped)
			// gjson treats JSON null values as existing with Type == Null
			for _, field := range []string{"bitness", "model", "platformVersion", "uaFullVersion", "fullVersionList", "wow64"} {
				result := batchEvent.Get("context.userAgentData." + field)
				Expect(result.Exists()).To(BeTrue(),
					fmt.Sprintf("optional field %q should exist even when null", field))
				Expect(result.Type).To(Equal(gjson.Null),
					fmt.Sprintf("optional field %q should be null", field))
			}
		})
	})
})
