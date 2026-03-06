package processor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/tidwall/gjson"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"

	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/processor/isolation"
	"github.com/rudderlabs/rudder-server/processor/transformer"
	"github.com/rudderlabs/rudder-server/processor/types"
)

// reservedIdentifyTraitsJSON returns all 17 Segment Spec reserved identify traits
// as a JSON object string.
// Reference: refs/segment-docs/src/connections/spec/identify.md
func reservedIdentifyTraitsJSON() string {
	return `{
		"address": {"city": "San Francisco", "country": "US", "postalCode": "94107", "state": "CA", "street": "123 Main St"},
		"age": 32,
		"avatar": "https://example.com/avatar.png",
		"birthday": "1992-06-15",
		"company": {"name": "Initech", "id": "comp_123", "industry": "Technology", "employee_count": 150, "plan": "enterprise"},
		"createdAt": "2023-01-15T10:30:00.000Z",
		"description": "Software engineer and open source contributor",
		"email": "peter@example.com",
		"firstName": "Peter",
		"gender": "male",
		"id": "user_abc123",
		"lastName": "Gibbons",
		"name": "Peter Gibbons",
		"phone": "+1-555-867-5309",
		"title": "VP of Engineering",
		"username": "pgibbons",
		"website": "https://pgibbons.dev"
	}`
}

// reservedGroupTraitsJSON returns all 12 Segment Spec reserved group traits
// as a JSON object string.
// Reference: refs/segment-docs/src/connections/spec/group.md
func reservedGroupTraitsJSON() string {
	return `{
		"address": {"city": "Palo Alto", "country": "US", "postalCode": "94301", "state": "CA", "street": "456 Oak Ave"},
		"avatar": "https://example.com/company-logo.png",
		"createdAt": "2020-03-10T08:00:00.000Z",
		"description": "Enterprise software company",
		"email": "info@initech.com",
		"employees": "150",
		"id": "grp_456def",
		"industry": "Technology",
		"name": "Initech Corporation",
		"phone": "+1-555-123-4567",
		"website": "https://initech.com",
		"plan": "enterprise"
	}`
}

// reservedIdentifyTraitEntries returns a table of individual identify trait definitions
// for table-driven isolation testing. Each entry has: name, JSON value, expected gjson type.
func reservedIdentifyTraitEntries() []struct {
	name      string
	jsonValue string
	gjType    gjson.Type
} {
	return []struct {
		name      string
		jsonValue string
		gjType    gjson.Type
	}{
		{"address", `{"city":"San Francisco","country":"US","postalCode":"94107","state":"CA","street":"123 Main St"}`, gjson.JSON},
		{"age", "32", gjson.Number},
		{"avatar", `"https://example.com/avatar.png"`, gjson.String},
		{"birthday", `"1992-06-15"`, gjson.String},
		{"company", `{"name":"Initech","id":"comp_123","industry":"Technology","employee_count":150,"plan":"enterprise"}`, gjson.JSON},
		{"createdAt", `"2023-01-15T10:30:00.000Z"`, gjson.String},
		{"description", `"Software engineer and open source contributor"`, gjson.String},
		{"email", `"peter@example.com"`, gjson.String},
		{"firstName", `"Peter"`, gjson.String},
		{"gender", `"male"`, gjson.String},
		{"id", `"user_abc123"`, gjson.String},
		{"lastName", `"Gibbons"`, gjson.String},
		{"name", `"Peter Gibbons"`, gjson.String},
		{"phone", `"+1-555-867-5309"`, gjson.String},
		{"title", `"VP of Engineering"`, gjson.String},
		{"username", `"pgibbons"`, gjson.String},
		{"website", `"https://pgibbons.dev"`, gjson.String},
	}
}

// reservedGroupTraitEntries returns a table of individual group trait definitions
// for table-driven isolation testing.
func reservedGroupTraitEntries() []struct {
	name      string
	jsonValue string
	gjType    gjson.Type
} {
	return []struct {
		name      string
		jsonValue string
		gjType    gjson.Type
	}{
		{"address", `{"city":"Palo Alto","country":"US","postalCode":"94301","state":"CA","street":"456 Oak Ave"}`, gjson.JSON},
		{"avatar", `"https://example.com/company-logo.png"`, gjson.String},
		{"createdAt", `"2020-03-10T08:00:00.000Z"`, gjson.String},
		{"description", `"Enterprise software company"`, gjson.String},
		{"email", `"info@initech.com"`, gjson.String},
		{"employees", `"150"`, gjson.String},
		{"id", `"grp_456def"`, gjson.String},
		{"industry", `"Technology"`, gjson.String},
		{"name", `"Initech Corporation"`, gjson.String},
		{"phone", `"+1-555-123-4567"`, gjson.String},
		{"website", `"https://initech.com"`, gjson.String},
		{"plan", `"enterprise"`, gjson.String},
	}
}

var _ = Describe("Reserved Traits Parity", Ordered, func() {
	initProcessor()

	var c *testContext

	// prepareHandleForTraits configures a processor Handle with isolation and archival disabled
	// for reserved traits testing, following the established pattern from processor_test.go.
	prepareHandleForTraits := func(proc *Handle) *Handle {
		isolationStrategy, err := isolation.GetStrategy(isolation.ModeNone)
		Expect(err).To(BeNil())
		proc.isolationStrategy = isolationStrategy
		proc.config.enableConcurrentStore = config.SingleValueLoader(false)
		proc.config.archivalEnabled = config.SingleValueLoader(false)
		return proc
	}

	BeforeEach(func() {
		c = &testContext{}
		c.Setup()
		// Crash recovery check expectation required by processor.Setup
		c.mockGatewayJobsDB.EXPECT().DeleteExecuting().Times(1)
	})

	AfterEach(func() {
		c.Finish()
	})

	// runPipelineStagesForTraits executes the first 3 processor pipeline stages (preprocess,
	// srcHydration, pretransform) and returns captured TransformerEvent objects from the observer.
	// This validates that traits survive through core event processing.
	runPipelineStagesForTraits := func(payload []byte, eventCount int, sourceID string) []types.TransformerEvent {
		mockTransformerClients := transformer.NewSimpleClients()
		processor := prepareHandleForTraits(NewHandle(config.Default, mockTransformerClients))

		Setup(processor, c, false, false)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		Expect(processor.config.asyncInit.WaitContext(ctx)).To(BeNil())

		job := &jobsdb.JobT{
			UUID:          uuid.New(),
			JobID:         9001,
			CreatedAt:     time.Now(),
			ExpireAt:      time.Now(),
			CustomVal:     gatewayCustomVal[0],
			EventPayload:  payload,
			EventCount:    eventCount,
			LastJobStatus: jobsdb.JobStatusT{},
			Parameters:    createBatchParameters(sourceID),
			WorkspaceId:   sampleWorkspaceID,
		}

		srcHydrationMsg, err := processor.preprocessStage(
			"",
			subJob{ctx: ctx, subJobs: []*jobsdb.JobT{job}},
			0,
		)
		Expect(err).To(BeNil())
		preTransMessage, err := processor.srcHydrationStage("", srcHydrationMsg)
		Expect(err).To(BeNil())
		_, pretransErr := processor.pretransformStage("", preTransMessage)
		Expect(pretransErr).To(BeNil(), "pretransformStage should not return an error")

		// Collect all events from the observer
		var allEvents []types.TransformerEvent
		for _, call := range c.MockObserver.calls {
			allEvents = append(allEvents, call.events...)
		}
		return allEvents
	}

	// -----------------------------------------------------------------
	// Context: Identify reserved traits
	// -----------------------------------------------------------------
	Context("Identify reserved traits", func() {
		It("should preserve all 17 reserved identify traits through the full pipeline", func() {
			// Build an identify event with ALL 17 reserved traits
			identifyEvent := fmt.Sprintf(`{
				"rudderId": "some-rudder-id",
				"messageId": "identify-all-reserved-traits",
				"type": "identify",
				"userId": "user_abc123",
				"anonymousId": "anon-id-1",
				"traits": %s,
				"context": {},
				"integrations": {"All": true},
				"originalTimestamp": "2024-01-15T10:00:00.000Z",
				"sentAt": "2024-01-15T10:00:00.000Z"
			}`, reservedIdentifyTraitsJSON())

			receivedAt := "2024-01-15T10:00:05.000Z"
			batchPayload := fmt.Appendf(nil,
				`{"writeKey":%q,"batch":[%s],"requestIP":"1.2.3.4","receivedAt":%q}`,
				WriteKeyEnabledNoUT, identifyEvent, receivedAt,
			)

			// Set up mocks for the full pipeline (handlePendingGatewayJobs)
			mockTransformerClients := transformer.NewSimpleClients()
			processor := prepareHandleForTraits(NewHandle(config.Default, mockTransformerClients))

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:          uuid.New(),
					JobID:         8001,
					CreatedAt:     time.Now(),
					ExpireAt:      time.Now(),
					CustomVal:     gatewayCustomVal[0],
					EventPayload:  batchPayload,
					EventCount:    1,
					LastJobStatus: jobsdb.JobStatusT{},
					Parameters:    createBatchParameters(SourceIDEnabledNoUT),
					WorkspaceId:   sampleWorkspaceID,
				},
			}

			// Capture events arriving at the destination transformer
			var capturedIdentifyEvents []types.TransformerEvent
			mockTransformerClients.WithDynamicDestinationTransform(
				func(ctx context.Context, clientEvents []types.TransformerEvent) types.Response {
					defer GinkgoRecover()
					capturedIdentifyEvents = append(capturedIdentifyEvents, clientEvents...)
					// Return each event as a successful transform response
					responses := make([]types.TransformerResponse, len(clientEvents))
					for i, event := range clientEvents {
						responses[i] = types.TransformerResponse{
							Output:     event.Message,
							Metadata:   event.Metadata,
							StatusCode: 200,
						}
					}
					return types.Response{Events: responses}
				},
			)

			// Mock gateway DB: return our test job
			c.mockGatewayJobsDB.EXPECT().GetUnprocessed(gomock.Any(), gomock.Any()).
				Return(jobsdb.JobsResult{Jobs: unprocessedJobsList}, nil).Times(1)

			// Mock router DB: accept stored jobs (destination A is a router type)
			c.mockRouterJobsDB.EXPECT().WithStoreSafeTx(gomock.Any(), gomock.Any()).Times(1).
				Do(func(ctx context.Context, f func(jobsdb.StoreSafeTx) error) {
					_ = f(jobsdb.EmptyStoreSafeTx())
				}).Return(nil)
			callStoreRouter := c.mockRouterJobsDB.EXPECT().StoreInTx(gomock.Any(), gomock.Any(), gomock.Any()).Times(1)

			// Mock archival DB (may be called even if archival is disabled)
			c.mockArchivalDB.EXPECT().WithStoreSafeTx(gomock.Any(), gomock.Any()).AnyTimes().
				Do(func(ctx context.Context, f func(jobsdb.StoreSafeTx) error) {
					_ = f(jobsdb.EmptyStoreSafeTx())
				}).Return(nil)
			c.mockArchivalDB.EXPECT().StoreInTx(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

			// Mock gateway DB: accept status update
			c.mockGatewayJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any(), gomock.Any()).
				Do(func(ctx context.Context, f func(tx jobsdb.UpdateSafeTx) error) {
					_ = f(jobsdb.EmptyUpdateSafeTx())
				}).Return(nil).Times(1)
			c.mockGatewayJobsDB.EXPECT().
				UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Len(len(unprocessedJobsList))).
				Times(1).After(callStoreRouter)

			// Execute the full pipeline
			processorSetupAndAssertJobHandling(processor, c)

			// ----- Assertions: all 17 reserved identify traits must be preserved -----
			Expect(capturedIdentifyEvents).ToNot(BeEmpty(),
				"destination transformer should receive at least one identify event")

			eventJSON, err := jsonrs.Marshal(capturedIdentifyEvents[0].Message)
			Expect(err).To(BeNil())
			eventStr := string(eventJSON)

			// String traits
			Expect(gjson.Get(eventStr, "traits.avatar").String()).To(Equal("https://example.com/avatar.png"))
			Expect(gjson.Get(eventStr, "traits.birthday").String()).To(Equal("1992-06-15"))
			Expect(gjson.Get(eventStr, "traits.createdAt").String()).To(Equal("2023-01-15T10:30:00.000Z"))
			Expect(gjson.Get(eventStr, "traits.description").String()).To(Equal("Software engineer and open source contributor"))
			Expect(gjson.Get(eventStr, "traits.email").String()).To(Equal("peter@example.com"))
			Expect(gjson.Get(eventStr, "traits.firstName").String()).To(Equal("Peter"))
			Expect(gjson.Get(eventStr, "traits.gender").String()).To(Equal("male"))
			Expect(gjson.Get(eventStr, "traits.id").String()).To(Equal("user_abc123"))
			Expect(gjson.Get(eventStr, "traits.lastName").String()).To(Equal("Gibbons"))
			Expect(gjson.Get(eventStr, "traits.name").String()).To(Equal("Peter Gibbons"))
			Expect(gjson.Get(eventStr, "traits.phone").String()).To(Equal("+1-555-867-5309"))
			Expect(gjson.Get(eventStr, "traits.title").String()).To(Equal("VP of Engineering"))
			Expect(gjson.Get(eventStr, "traits.username").String()).To(Equal("pgibbons"))
			Expect(gjson.Get(eventStr, "traits.website").String()).To(Equal("https://pgibbons.dev"))

			// Number trait — age must remain a number, not coerced to string
			Expect(gjson.Get(eventStr, "traits.age").Type).To(Equal(gjson.Number))
			Expect(gjson.Get(eventStr, "traits.age").Float()).To(Equal(float64(32)))

			// Object traits — address and company must remain nested JSON objects
			Expect(gjson.Get(eventStr, "traits.address").Type).To(Equal(gjson.JSON))
			Expect(gjson.Get(eventStr, "traits.address.city").String()).To(Equal("San Francisco"))
			Expect(gjson.Get(eventStr, "traits.address.country").String()).To(Equal("US"))
			Expect(gjson.Get(eventStr, "traits.address.postalCode").String()).To(Equal("94107"))
			Expect(gjson.Get(eventStr, "traits.address.state").String()).To(Equal("CA"))
			Expect(gjson.Get(eventStr, "traits.address.street").String()).To(Equal("123 Main St"))

			Expect(gjson.Get(eventStr, "traits.company").Type).To(Equal(gjson.JSON))
			Expect(gjson.Get(eventStr, "traits.company.name").String()).To(Equal("Initech"))
			Expect(gjson.Get(eventStr, "traits.company.id").String()).To(Equal("comp_123"))
			Expect(gjson.Get(eventStr, "traits.company.industry").String()).To(Equal("Technology"))
			Expect(gjson.Get(eventStr, "traits.company.employee_count").Float()).To(Equal(float64(150)))
			Expect(gjson.Get(eventStr, "traits.company.plan").String()).To(Equal("enterprise"))
		})

		It("should preserve individual reserved identify traits in isolation", func() {
			traitEntries := reservedIdentifyTraitEntries()

			// Build one event per trait, each containing a single reserved trait
			eventJSONs := make([]string, len(traitEntries))
			for i, t := range traitEntries {
				eventJSONs[i] = fmt.Sprintf(`{
					"rudderId":"some-rudder-id",
					"messageId":"identify-trait-%d-%s",
					"type":"identify",
					"userId":"test-user-%d",
					"traits":{%q:%s},
					"context":{},
					"integrations":{"All":true},
					"originalTimestamp":"2024-01-15T10:00:00.000Z",
					"sentAt":"2024-01-15T10:00:00.000Z"
				}`, i, t.name, i, t.name, t.jsonValue)
			}

			batchStr := strings.Join(eventJSONs, ",")
			batchPayload := fmt.Appendf(nil,
				`{"writeKey":%q,"batch":[%s],"requestIP":"1.2.3.4","receivedAt":"2024-01-15T10:00:05.000Z"}`,
				WriteKeyEnabledNoUT, batchStr,
			)

			capturedEvents := runPipelineStagesForTraits(batchPayload, len(traitEntries), SourceIDEnabledNoUT)
			Expect(capturedEvents).To(HaveLen(len(traitEntries)),
				"each identify trait event should produce a TransformerEvent")

			// Assert each individual trait is preserved
			for i, t := range traitEntries {
				eventBytes, err := jsonrs.Marshal(capturedEvents[i].Message)
				Expect(err).To(BeNil())

				result := gjson.GetBytes(eventBytes, "traits."+t.name)
				Expect(result.Exists()).To(BeTrue(),
					"identify trait %q should exist in pipeline output for event %d", t.name, i)
				Expect(result.Type).To(Equal(t.gjType),
					"identify trait %q should have gjson type %v, got %v", t.name, t.gjType, result.Type)
			}
		})
	})

	// -----------------------------------------------------------------
	// Context: Group reserved traits
	// -----------------------------------------------------------------
	Context("Group reserved traits", func() {
		It("should preserve all 12 reserved group traits through the full pipeline", func() {
			// Build a group event with ALL 12 reserved traits
			groupEvent := fmt.Sprintf(`{
				"rudderId": "some-rudder-id",
				"messageId": "group-all-reserved-traits",
				"type": "group",
				"userId": "user_abc123",
				"anonymousId": "anon-id-1",
				"groupId": "grp_456def",
				"traits": %s,
				"context": {},
				"integrations": {"All": true},
				"originalTimestamp": "2024-01-15T10:00:00.000Z",
				"sentAt": "2024-01-15T10:00:00.000Z"
			}`, reservedGroupTraitsJSON())

			receivedAt := "2024-01-15T10:00:05.000Z"
			batchPayload := fmt.Appendf(nil,
				`{"writeKey":%q,"batch":[%s],"requestIP":"1.2.3.4","receivedAt":%q}`,
				WriteKeyEnabledNoUT, groupEvent, receivedAt,
			)

			// Set up mocks for the full pipeline
			mockTransformerClients := transformer.NewSimpleClients()
			processor := prepareHandleForTraits(NewHandle(config.Default, mockTransformerClients))

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:          uuid.New(),
					JobID:         8002,
					CreatedAt:     time.Now(),
					ExpireAt:      time.Now(),
					CustomVal:     gatewayCustomVal[0],
					EventPayload:  batchPayload,
					EventCount:    1,
					LastJobStatus: jobsdb.JobStatusT{},
					Parameters:    createBatchParameters(SourceIDEnabledNoUT),
					WorkspaceId:   sampleWorkspaceID,
				},
			}

			// Capture events arriving at the destination transformer
			var capturedGroupEvents []types.TransformerEvent
			mockTransformerClients.WithDynamicDestinationTransform(
				func(ctx context.Context, clientEvents []types.TransformerEvent) types.Response {
					defer GinkgoRecover()
					capturedGroupEvents = append(capturedGroupEvents, clientEvents...)
					responses := make([]types.TransformerResponse, len(clientEvents))
					for i, event := range clientEvents {
						responses[i] = types.TransformerResponse{
							Output:     event.Message,
							Metadata:   event.Metadata,
							StatusCode: 200,
						}
					}
					return types.Response{Events: responses}
				},
			)

			// Mock gateway DB
			c.mockGatewayJobsDB.EXPECT().GetUnprocessed(gomock.Any(), gomock.Any()).
				Return(jobsdb.JobsResult{Jobs: unprocessedJobsList}, nil).Times(1)

			// Mock router DB
			c.mockRouterJobsDB.EXPECT().WithStoreSafeTx(gomock.Any(), gomock.Any()).Times(1).
				Do(func(ctx context.Context, f func(jobsdb.StoreSafeTx) error) {
					_ = f(jobsdb.EmptyStoreSafeTx())
				}).Return(nil)
			callStoreRouter := c.mockRouterJobsDB.EXPECT().StoreInTx(gomock.Any(), gomock.Any(), gomock.Any()).Times(1)

			// Mock archival DB
			c.mockArchivalDB.EXPECT().WithStoreSafeTx(gomock.Any(), gomock.Any()).AnyTimes().
				Do(func(ctx context.Context, f func(jobsdb.StoreSafeTx) error) {
					_ = f(jobsdb.EmptyStoreSafeTx())
				}).Return(nil)
			c.mockArchivalDB.EXPECT().StoreInTx(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

			// Mock gateway DB status update
			c.mockGatewayJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any(), gomock.Any()).
				Do(func(ctx context.Context, f func(tx jobsdb.UpdateSafeTx) error) {
					_ = f(jobsdb.EmptyUpdateSafeTx())
				}).Return(nil).Times(1)
			c.mockGatewayJobsDB.EXPECT().
				UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Len(len(unprocessedJobsList))).
				Times(1).After(callStoreRouter)

			// Execute the full pipeline
			processorSetupAndAssertJobHandling(processor, c)

			// ----- Assertions: all 12 reserved group traits must be preserved -----
			Expect(capturedGroupEvents).ToNot(BeEmpty(),
				"destination transformer should receive at least one group event")

			eventJSON, err := jsonrs.Marshal(capturedGroupEvents[0].Message)
			Expect(err).To(BeNil())
			eventStr := string(eventJSON)

			// String traits
			Expect(gjson.Get(eventStr, "traits.avatar").String()).To(Equal("https://example.com/company-logo.png"))
			Expect(gjson.Get(eventStr, "traits.createdAt").String()).To(Equal("2020-03-10T08:00:00.000Z"))
			Expect(gjson.Get(eventStr, "traits.description").String()).To(Equal("Enterprise software company"))
			Expect(gjson.Get(eventStr, "traits.email").String()).To(Equal("info@initech.com"))
			Expect(gjson.Get(eventStr, "traits.id").String()).To(Equal("grp_456def"))
			Expect(gjson.Get(eventStr, "traits.industry").String()).To(Equal("Technology"))
			Expect(gjson.Get(eventStr, "traits.name").String()).To(Equal("Initech Corporation"))
			Expect(gjson.Get(eventStr, "traits.phone").String()).To(Equal("+1-555-123-4567"))
			Expect(gjson.Get(eventStr, "traits.website").String()).To(Equal("https://initech.com"))
			Expect(gjson.Get(eventStr, "traits.plan").String()).To(Equal("enterprise"))

			// employees must remain a String per Segment Spec (even though it represents a count)
			Expect(gjson.Get(eventStr, "traits.employees").Type).To(Equal(gjson.String))
			Expect(gjson.Get(eventStr, "traits.employees").String()).To(Equal("150"))

			// Object trait — address must remain a nested JSON object
			Expect(gjson.Get(eventStr, "traits.address").Type).To(Equal(gjson.JSON))
			Expect(gjson.Get(eventStr, "traits.address.city").String()).To(Equal("Palo Alto"))
			Expect(gjson.Get(eventStr, "traits.address.country").String()).To(Equal("US"))
			Expect(gjson.Get(eventStr, "traits.address.postalCode").String()).To(Equal("94301"))
			Expect(gjson.Get(eventStr, "traits.address.state").String()).To(Equal("CA"))
			Expect(gjson.Get(eventStr, "traits.address.street").String()).To(Equal("456 Oak Ave"))

			// Verify groupId is also preserved
			Expect(gjson.Get(eventStr, "groupId").String()).To(Equal("grp_456def"))
		})

		It("should preserve individual reserved group traits in isolation", func() {
			traitEntries := reservedGroupTraitEntries()

			// Build one event per trait, each containing a single reserved group trait
			eventJSONs := make([]string, len(traitEntries))
			for i, t := range traitEntries {
				eventJSONs[i] = fmt.Sprintf(`{
					"rudderId":"some-rudder-id",
					"messageId":"group-trait-%d-%s",
					"type":"group",
					"userId":"test-user-%d",
					"groupId":"grp_isolation_%d",
					"traits":{%q:%s},
					"context":{},
					"integrations":{"All":true},
					"originalTimestamp":"2024-01-15T10:00:00.000Z",
					"sentAt":"2024-01-15T10:00:00.000Z"
				}`, i, t.name, i, i, t.name, t.jsonValue)
			}

			batchStr := strings.Join(eventJSONs, ",")
			batchPayload := fmt.Appendf(nil,
				`{"writeKey":%q,"batch":[%s],"requestIP":"1.2.3.4","receivedAt":"2024-01-15T10:00:05.000Z"}`,
				WriteKeyEnabledNoUT, batchStr,
			)

			capturedEvents := runPipelineStagesForTraits(batchPayload, len(traitEntries), SourceIDEnabledNoUT)
			Expect(capturedEvents).To(HaveLen(len(traitEntries)),
				"each group trait event should produce a TransformerEvent")

			// Assert each individual trait is preserved
			for i, t := range traitEntries {
				eventBytes, err := jsonrs.Marshal(capturedEvents[i].Message)
				Expect(err).To(BeNil())

				result := gjson.GetBytes(eventBytes, "traits."+t.name)
				Expect(result.Exists()).To(BeTrue(),
					"group trait %q should exist in pipeline output for event %d", t.name, i)
				Expect(result.Type).To(Equal(t.gjType),
					"group trait %q should have gjson type %v, got %v", t.name, t.gjType, result.Type)
			}
		})
	})

	// -----------------------------------------------------------------
	// Context: Type preservation (critical edge cases)
	// -----------------------------------------------------------------
	Context("Type preservation", func() {
		It("should preserve identify trait types: objects stay objects, numbers stay numbers", func() {
			// Specifically test the critical type-preservation cases for identify traits:
			// address (Object), age (Number), birthday (String date), company (Object)
			identifyEvent := fmt.Sprintf(`{
				"rudderId": "some-rudder-id",
				"messageId": "identify-type-check",
				"type": "identify",
				"userId": "type-test-user",
				"traits": %s,
				"context": {},
				"integrations": {"All": true},
				"originalTimestamp": "2024-01-15T10:00:00.000Z",
				"sentAt": "2024-01-15T10:00:00.000Z"
			}`, reservedIdentifyTraitsJSON())

			batchPayload := fmt.Appendf(nil,
				`{"writeKey":%q,"batch":[%s],"requestIP":"1.2.3.4","receivedAt":"2024-01-15T10:00:05.000Z"}`,
				WriteKeyEnabledNoUT, identifyEvent,
			)

			capturedEvents := runPipelineStagesForTraits(batchPayload, 1, SourceIDEnabledNoUT)
			Expect(capturedEvents).ToNot(BeEmpty())

			eventBytes, err := jsonrs.Marshal(capturedEvents[0].Message)
			Expect(err).To(BeNil())

			// TYPE CHECK: address must be a JSON object (not flattened to a string)
			addressResult := gjson.GetBytes(eventBytes, "traits.address")
			Expect(addressResult.Type).To(Equal(gjson.JSON),
				"address must remain a JSON object, not be flattened to a string")
			Expect(addressResult.Get("city").Exists()).To(BeTrue(),
				"address sub-field 'city' must be accessible")

			// TYPE CHECK: age must be a JSON number (not converted to string)
			ageResult := gjson.GetBytes(eventBytes, "traits.age")
			Expect(ageResult.Type).To(Equal(gjson.Number),
				"age must remain a JSON number, not be converted to a string")
			Expect(ageResult.Float()).To(Equal(float64(32)))

			// TYPE CHECK: birthday must remain a date string (not parsed or converted)
			birthdayResult := gjson.GetBytes(eventBytes, "traits.birthday")
			Expect(birthdayResult.Type).To(Equal(gjson.String),
				"birthday must remain a string date, not be converted")
			Expect(birthdayResult.String()).To(Equal("1992-06-15"))

			// TYPE CHECK: company must be a JSON object with nested fields preserved
			companyResult := gjson.GetBytes(eventBytes, "traits.company")
			Expect(companyResult.Type).To(Equal(gjson.JSON),
				"company must remain a JSON object, not be flattened")
			Expect(companyResult.Get("name").String()).To(Equal("Initech"))
			Expect(companyResult.Get("employee_count").Float()).To(Equal(float64(150)),
				"company.employee_count must remain a number inside the object")

			// TYPE CHECK: createdAt must be a string (ISO 8601 date, not parsed to time object)
			createdAtResult := gjson.GetBytes(eventBytes, "traits.createdAt")
			Expect(createdAtResult.Type).To(Equal(gjson.String),
				"createdAt must remain an ISO 8601 string")
		})

		It("should preserve group trait types: employees stays string, address stays object", func() {
			groupEvent := fmt.Sprintf(`{
				"rudderId": "some-rudder-id",
				"messageId": "group-type-check",
				"type": "group",
				"userId": "type-test-user",
				"groupId": "grp_type_test",
				"traits": %s,
				"context": {},
				"integrations": {"All": true},
				"originalTimestamp": "2024-01-15T10:00:00.000Z",
				"sentAt": "2024-01-15T10:00:00.000Z"
			}`, reservedGroupTraitsJSON())

			batchPayload := fmt.Appendf(nil,
				`{"writeKey":%q,"batch":[%s],"requestIP":"1.2.3.4","receivedAt":"2024-01-15T10:00:05.000Z"}`,
				WriteKeyEnabledNoUT, groupEvent,
			)

			capturedEvents := runPipelineStagesForTraits(batchPayload, 1, SourceIDEnabledNoUT)
			Expect(capturedEvents).ToNot(BeEmpty())

			eventBytes, err := jsonrs.Marshal(capturedEvents[0].Message)
			Expect(err).To(BeNil())

			// TYPE CHECK: employees must remain a String per Segment Spec (even though it is a count)
			employeesResult := gjson.GetBytes(eventBytes, "traits.employees")
			Expect(employeesResult.Type).To(Equal(gjson.String),
				"employees must remain a string per Segment Spec, not be converted to a number")
			Expect(employeesResult.String()).To(Equal("150"))

			// TYPE CHECK: address must remain a JSON object
			addressResult := gjson.GetBytes(eventBytes, "traits.address")
			Expect(addressResult.Type).To(Equal(gjson.JSON),
				"address must remain a JSON object, not be flattened to a string")
			Expect(addressResult.Get("city").String()).To(Equal("Palo Alto"))
			Expect(addressResult.Get("street").String()).To(Equal("456 Oak Ave"))

			// TYPE CHECK: createdAt must remain an ISO 8601 string
			createdAtResult := gjson.GetBytes(eventBytes, "traits.createdAt")
			Expect(createdAtResult.Type).To(Equal(gjson.String),
				"createdAt must remain an ISO 8601 string, not parsed")
			Expect(createdAtResult.String()).To(Equal("2020-03-10T08:00:00.000Z"))

			// TYPE CHECK: all other string traits must remain strings
			for _, traitName := range []string{"avatar", "description", "email", "id", "industry", "name", "phone", "website", "plan"} {
				traitResult := gjson.GetBytes(eventBytes, "traits."+traitName)
				Expect(traitResult.Type).To(Equal(gjson.String),
					"group trait %q must remain a string type", traitName)
				Expect(traitResult.Exists()).To(BeTrue(),
					"group trait %q must exist in pipeline output", traitName)
			}
		})
	})
})
