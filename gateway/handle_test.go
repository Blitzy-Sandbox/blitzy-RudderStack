package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-server/gateway/validator"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"
	"github.com/rudderlabs/rudder-schemas/go/stream"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	mocks_gateway "github.com/rudderlabs/rudder-server/mocks/gateway"
)

// createTestGateway creates a minimal Handle instance for testing event blocking
func createTestGateway(t *testing.T, eventBlockingSettings backendconfig.EventBlocking) *Handle {
	statsStore, err := memstats.New()
	require.NoError(t, err)

	configData := make(map[string]backendconfig.ConfigT)

	configData["workspace1"] = backendconfig.ConfigT{
		Settings: backendconfig.Settings{
			EventBlocking: eventBlockingSettings,
		},
		Sources: []backendconfig.SourceT{
			{
				ID:       "source-id-1",
				WriteKey: "write-key-1",
				SourceDefinition: backendconfig.SourceDefinitionT{
					Name:     "JavaScript",
					Category: "", // event stream source
				},
				Name:    "JS Source",
				Enabled: true,
			},
			{
				ID:       "source-id-2",
				WriteKey: "write-key-2",
				SourceDefinition: backendconfig.SourceDefinitionT{
					Name:     "Webhook",
					Category: "webhook", // event stream source
				},
				Name:    "Webhook Source",
				Enabled: true,
			},
			{
				ID:       "warehouse-source-id-1",
				WriteKey: "warehouse-write-key",
				SourceDefinition: backendconfig.SourceDefinitionT{
					Name:     "Warehouse",
					Category: "warehouse", // non-event stream source
				},
				Name:    "Warehouse Source",
				Enabled: true,
			},
		},
	}

	// Create a mock controller and webhook handler
	mockCtrl := gomock.NewController(t)
	t.Cleanup(mockCtrl.Finish)
	mockWebhook := mocks_gateway.NewMockWebhookRequestHandler(mockCtrl)
	mockWebhook.EXPECT().Register(gomock.Any()).AnyTimes() // Allow any number of Register calls

	gw := &Handle{
		stats:   statsStore,
		logger:  logger.NOP,
		webhook: mockWebhook,
		conf: struct {
			webPort, maxUserWebRequestWorkerProcess, maxDBWriterProcess                       int
			maxUserWebRequestBatchSize, maxDBBatchSize, maxHeaderBytes, maxConcurrentRequests int
			userWebRequestBatchTimeout, dbBatchWriteTimeout                                   config.ValueLoader[time.Duration]
			maxReqSize                                                                        config.ValueLoader[int]
			enableRateLimit                                                                   config.ValueLoader[bool]
			enableSuppressUserFeature                                                         bool
			diagnosisTickerTime                                                               time.Duration
			ReadTimeout                                                                       time.Duration
			ReadHeaderTimeout                                                                 time.Duration
			WriteTimeout                                                                      time.Duration
			IdleTimeout                                                                       time.Duration
			allowReqsWithoutUserIDAndAnonymousID                                              config.ValueLoader[bool]
			gwAllowPartialWriteWithErrors                                                     config.ValueLoader[bool]
			webhookV2HandlerEnabled                                                           bool
		}{
			webhookV2HandlerEnabled: false,
		},
		configSubscriberLock: sync.RWMutex{},
		requestSizeStat:      statsStore.NewStat("gateway.request_size", stats.HistogramType),
	}

	// Use the same logic as backendConfigSubscriber to process the config data
	gw.processBackendConfig(configData)

	gw.msgValidator = validator.NewValidateMediator(gw.logger, stream.NewMessagePropertiesValidator())

	return gw
}

func TestIsEventBlocked(t *testing.T) {
	tests := []struct {
		name                  string
		workspaceID           string
		sourceID              string
		eventType             string
		eventName             string
		eventBlockingSettings backendconfig.EventBlocking
		expected              bool
		description           string
	}{
		{
			name:        "empty event name",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expected:    false,
			description: "Empty event names should not be blocked",
		},
		{
			name:        "non-track event type",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "identify",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expected:    false,
			description: "Non-track events should not be blocked",
		},
		{
			name:        "blocked event",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expected:    true,
			description: "Event should be blocked when it matches the blocked events list",
		},
		{
			name:        "non-blocked event",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "PageView",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expected:    false,
			description: "Event should not be blocked when it's not in the blocked events list",
		},
		{
			name:        "case sensitive event matching",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "purchase", // lowercase
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"}, // uppercase
				},
			},
			expected:    false,
			description: "Event matching should be case sensitive",
		},
		{
			name:        "events map is nil",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: nil,
			},
			expected:    false,
			description: "When Events map is nil, no events should be blocked",
		},
		{
			name:        "workspace not found",
			workspaceID: "nonexistent",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expected:    false,
			description: "When workspace is not found, events should not be blocked",
		},
		{
			name:        "non-event stream source",
			workspaceID: "workspace1",
			sourceID:    "warehouse-source-id-1",
			eventType:   "track",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expected:    false,
			description: "Events from non-event stream sources should not be blocked",
		},
		{
			name:        "event stream source - blocked event",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expected:    true,
			description: "Events from event stream sources should be blocked when they match the blocked events list",
		},
		{
			name:        "empty track event list",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {},
				},
			},
			expected:    false,
			description: "track event list is empty",
		},
		{
			name:        "empty events map",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{},
			},
			expected:    false,
			description: "events map is empty",
		},
		{
			name:        "track event with large blocked events list",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "AddToCart",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase", "AddToCart", "RemoveFromCart", "Checkout", "PaymentInfo", "OrderComplete"},
				},
			},
			expected:    true,
			description: "Event should be blocked when present in a large list of blocked track events",
		},
		{
			name:        "track event not in large blocked events list",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "ProductView",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase", "AddToCart", "RemoveFromCart", "Checkout", "PaymentInfo", "OrderComplete"},
				},
			},
			expected:    false,
			description: "Event should not be blocked when not present in a large list of blocked track events",
		},
		{
			name:        "track event with special characters - blocked",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Product Purchased - $100",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Product Purchased - $100", "Cart Abandoned!", "Sign-Up Complete"},
				},
			},
			expected:    true,
			description: "Track event names with special characters should be blocked when they match exactly",
		},
		{
			name:        "track event with special characters - not blocked",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Product Purchased - $200",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Product Purchased - $100", "Cart Abandoned!", "Sign-Up Complete"},
				},
			},
			expected:    false,
			description: "Track event names with special characters should not be blocked when they don't match exactly",
		},
		{
			name:        "track event with unicode characters - blocked",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "购买商品",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"购买商品", "添加到购物车", "查看产品"},
				},
			},
			expected:    true,
			description: "Track event names with unicode characters should be blocked when they match exactly",
		},
		{
			name:        "track event with whitespace - blocked",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "  Purchase  ",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"  Purchase  ", "AddToCart", " Checkout "},
				},
			},
			expected:    true,
			description: "Track event names with whitespace should be blocked when they match exactly including whitespace",
		},
		{
			name:        "track event with whitespace - not blocked due to trim difference",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "Purchase",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"  Purchase  ", "AddToCart", " Checkout "},
				},
			},
			expected:    false,
			description: "Track event names should not be blocked when whitespace doesn't match exactly",
		},
		{
			name:        "track event - empty string in blocked events list",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"", "Purchase", "AddToCart"},
				},
			},
			expected:    false,
			description: "Empty track event names should not be blocked even if empty string is in the blocked list",
		},
		{
			name:        "track event with very long name - blocked",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "User_Completed_Very_Detailed_Product_Configuration_With_Multiple_Options_And_Customizations_Before_Adding_To_Cart",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"User_Completed_Very_Detailed_Product_Configuration_With_Multiple_Options_And_Customizations_Before_Adding_To_Cart", "Purchase"},
				},
			},
			expected:    true,
			description: "Very long track event names should be blocked when they match exactly",
		},
		{
			name:        "track event with numeric name - blocked",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "12345",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"12345", "67890", "Purchase"},
				},
			},
			expected:    true,
			description: "Track event names that are purely numeric should be blocked when they match exactly",
		},
		{
			name:        "track event with mixed case in blocked list - exact match",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "PuRcHaSe",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"PuRcHaSe", "AddToCart", "checkout"},
				},
			},
			expected:    true,
			description: "Track event names with mixed case should be blocked when they match exactly",
		},
		{
			name:        "track event with single character name - blocked",
			workspaceID: "workspace1",
			sourceID:    "source-id-1",
			eventType:   "track",
			eventName:   "A",
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"A", "B", "Purchase"},
				},
			},
			expected:    true,
			description: "Single character track event names should be blocked when they match exactly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := createTestGateway(t, tt.eventBlockingSettings)

			result := gw.isEventBlocked(tt.workspaceID, tt.sourceID, tt.eventType, tt.eventName)
			require.Equal(t, tt.expected, result, tt.description)
		})
	}
}

func TestExtractJobsFromInternalBatchPayload_EventBlocking(t *testing.T) {
	type expectedJob struct {
		eventName              string
		isEventBlocked         bool
		skipLiveEventRecording bool
		shouldBeDropped        bool
	}

	gw := createTestGateway(t, backendconfig.EventBlocking{
		Events: map[string][]string{
			"track": {"Purchase", "batch-request-type-with-type-track", "track-request-type-with-no-type", "batch-request-type-with-no-type"},
		},
	})

	tests := []struct {
		name         string
		messages     []stream.Message
		expectedJobs []expectedJob
		description  string
	}{
		{
			name: "mixed batch - blocked, non-blocked, and non-event stream events",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1", // Event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-2",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-2", // Event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
					},
					Payload: json.RawMessage(`{"type":"track","event":"PageView","messageId":"msg-2","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-3",
						WorkspaceID: "workspace1",
						SourceID:    "warehouse-source-id-1", // Non-event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-3","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "batch",
						RoutingKey:  "routing-key-3",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1", // Event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
					},
					Payload: json.RawMessage(`{"type":"track","event":"batch-request-type-with-type-track","messageId":"msg-3","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "batch",
						RoutingKey:  "routing-key-3",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1", // Event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
					},
					Payload: json.RawMessage(`{"event":"batch-request-type-with-no-type","messageId":"msg-3","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`), // type is not present
				},
			},
			expectedJobs: []expectedJob{
				{
					eventName:              "Purchase",
					isEventBlocked:         true,
					skipLiveEventRecording: true,
					shouldBeDropped:        true,
				},
				{
					eventName:              "PageView",
					isEventBlocked:         false,
					skipLiveEventRecording: false,
					shouldBeDropped:        false,
				},
				{
					eventName:              "Purchase",
					isEventBlocked:         false,
					skipLiveEventRecording: false,
					shouldBeDropped:        false,
				},
				{
					eventName:              "batch-request-type-with-type-track",
					isEventBlocked:         true,
					skipLiveEventRecording: true,
					shouldBeDropped:        true,
				},
				{
					eventName:              "batch-request-type-with-no-type",
					isEventBlocked:         false,
					skipLiveEventRecording: false,
					shouldBeDropped:        false,
				},
			},
			description: "Mixed batch should handle blocked, non-blocked, and non-event stream events correctly",
		},
		{
			name: "live event recording disabled for blocked events",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
			},
			expectedJobs: []expectedJob{
				{
					eventName:              "Purchase",
					isEventBlocked:         true,
					skipLiveEventRecording: true,
					shouldBeDropped:        true,
				},
			},
			description: "Blocked events should have skipLiveEventRecording set to true",
		},
		{
			name: "processor integration - blocked events should be dropped",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-2",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
					},
					Payload: json.RawMessage(`{"type":"track","event":"PageView","messageId":"msg-2","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
			},
			expectedJobs: []expectedJob{
				{
					eventName:              "Purchase",
					isEventBlocked:         true,
					skipLiveEventRecording: true,
					shouldBeDropped:        true,
				},
				{
					eventName:              "PageView",
					isEventBlocked:         false,
					skipLiveEventRecording: false,
					shouldBeDropped:        false,
				},
			},
			description: "Processor should drop blocked events while processing non-blocked events normally",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create JSON payload directly as array of stream.Message
			payloadBytes, err := jsonrs.Marshal(tt.messages)
			require.NoError(t, err)

			// Extract jobs
			jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
			require.NoError(t, err, "extractJobsFromInternalBatchPayload should not return error")

			// Verify we got the expected number of jobs
			require.Len(t, jobs, len(tt.expectedJobs), "Number of jobs should match expected")

			// Verify each job's properties
			for i, expectedJob := range tt.expectedJobs {
				job := jobs[i]

				// Parse the job parameters to check event blocking properties
				var eventParams map[string]any
				err := jsonrs.Unmarshal(job.job.Parameters, &eventParams)
				require.NoError(t, err, "Should be able to parse job parameters")

				// Check IsEventBlocked parameter
				isEventBlocked, exists := eventParams["is_event_blocked"]
				if expectedJob.isEventBlocked {
					require.True(t, exists, "Job %d: is_event_blocked parameter should exist for blocked events", i)
					require.True(t, isEventBlocked.(bool), "Job %d: is_event_blocked should be true", i)
				} else {
					// For non-blocked events, is_event_blocked should either not exist or be false
					if exists {
						require.False(t, isEventBlocked.(bool), "Job %d: is_event_blocked should be false", i)
					}
				}

				// Check skipLiveEventRecording field
				require.Equal(t, expectedJob.skipLiveEventRecording, job.skipLiveEventRecording,
					"Job %d: skipLiveEventRecording should match expected", i)

				// Verify processor behavior (events marked as blocked should be dropped)
				if expectedJob.shouldBeDropped {
					// Verify the job has the is_event_blocked parameter set to true
					// This simulates what the processor would check before dropping
					require.True(t, eventParams["is_event_blocked"].(bool),
						"Job %d: Events that should be dropped must have is_event_blocked=true", i)
				}
			}
		})
	}
}

func TestExtractJobsFromInternalBatchPayload_LiveEventRecording(t *testing.T) {
	type testCase struct {
		name                      string
		messages                  []stream.Message
		eventBlockingSettings     backendconfig.EventBlocking
		expectedSkipLiveEventRecs []bool
		description               string
	}

	tests := []testCase{
		{
			name: "normal events should not skip live event recording",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       false,
					},
					Payload: json.RawMessage(`{"type":"track","event":"PageView","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "identify",
						RoutingKey:  "routing-key-2",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       false,
					},
					Payload: json.RawMessage(`{"type":"identify","messageId":"msg-2","userId":"user1","traits":{"name":"John"},"rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
			},
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{},
			},
			expectedSkipLiveEventRecs: []bool{false, false},
			description:               "Normal events should allow live event recording",
		},
		{
			name: "blocked events should skip live event recording",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1", // Event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       false,
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-2",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       false,
					},
					Payload: json.RawMessage(`{"type":"track","event":"PageView","messageId":"msg-2","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
			},
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expectedSkipLiveEventRecs: []bool{true, false},
			description:               "Blocked events should skip live event recording while non-blocked events should not",
		},
		{
			name: "bot events with drop action should skip live event recording",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       true,
						BotName:     "test-bot",
						BotURL:      "https://test-bot.com",
						BotAction:   "drop",
					},
					Payload: json.RawMessage(`{"type":"track","event":"PageView","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-2",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       false,
					},
					Payload: json.RawMessage(`{"type":"track","event":"AddToCart","messageId":"msg-2","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
			},
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{},
			},
			expectedSkipLiveEventRecs: []bool{true, false},
			description:               "Bot events with 'drop' action should skip live event recording",
		},
		{
			name: "bot events with flag action should not skip live event recording",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       true,
						BotName:     "test-bot",
						BotURL:      "https://test-bot.com",
						BotAction:   "flag",
					},
					Payload: json.RawMessage(`{"type":"track","event":"PageView","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-2",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       true,
						BotName:     "another-bot",
						BotAction:   "disable",
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-2","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
			},
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{},
			},
			expectedSkipLiveEventRecs: []bool{false, false},
			description:               "Bot events with 'flag' or 'disable' actions should allow live event recording",
		},
		{
			name: "mixed scenario - blocked events and bot events with drop action",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1", // Event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       false,
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-2",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       true,
						BotName:     "crawler-bot",
						BotAction:   "drop",
					},
					Payload: json.RawMessage(`{"type":"track","event":"PageView","messageId":"msg-2","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-3",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       true,
						BotName:     "analytics-bot",
						BotAction:   "flag",
					},
					Payload: json.RawMessage(`{"type":"track","event":"AddToCart","messageId":"msg-3","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-4",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       false,
					},
					Payload: json.RawMessage(`{"type":"track","event":"Checkout","messageId":"msg-4","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
			},
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expectedSkipLiveEventRecs: []bool{true, true, false, false},
			description:               "Complex scenario: blocked events and bot drop events skip recording, others don't",
		},
		{
			name: "blocked bot events with drop action and blocked events should skip live event recording",
			messages: []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-1",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1", // Event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       true,
						BotName:     "blocked-bot",
						BotAction:   "drop",
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-1","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-2",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1", // Event stream source
						ReceivedAt:  time.Now(),
						RequestIP:   "1.1.1.1",
						IsBot:       true,
						BotName:     "blocked-bot-2",
						BotAction:   "flag",
					},
					Payload: json.RawMessage(`{"type":"track","event":"Purchase","messageId":"msg-2","userId":"user1","rudderId":"some-rudder-id","request_ip":"[::1]","receivedAt":"2024-01-01T00:00:00Z"}`),
				},
			},
			eventBlockingSettings: backendconfig.EventBlocking{
				Events: map[string][]string{
					"track": {"Purchase"},
				},
			},
			expectedSkipLiveEventRecs: []bool{true, true},
			description:               "Both blocked events and bot drop events should skip live event recording",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := createTestGateway(t, tt.eventBlockingSettings)

			payloadBytes, err := jsonrs.Marshal(tt.messages)
			require.NoError(t, err, "Failed to marshal test messages")

			jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
			require.NoError(t, err, "extractJobsFromInternalBatchPayload should not return error")

			require.Len(t, jobs, len(tt.expectedSkipLiveEventRecs), "Number of jobs should match expected")

			for i, expectedSkip := range tt.expectedSkipLiveEventRecs {
				job := jobs[i]
				require.Equal(t, expectedSkip, job.skipLiveEventRecording,
					"Job %d: skipLiveEventRecording should be %t - %s",
					i, expectedSkip, tt.description)
			}
		})
	}
}

// TestUserAgentDataPreservation verifies that context.userAgentData (structured Client Hints API data)
// passes through the Gateway Handle pipeline without modification or data loss.
// This covers ES-001: Structured Client Hints Pass-Through Verification.
func TestUserAgentDataPreservation(t *testing.T) {
	t.Run("low-entropy fields preserved", func(t *testing.T) {
		gw := createTestGateway(t, backendconfig.EventBlocking{})
		messages := []stream.Message{
			{
				Properties: stream.MessageProperties{
					RequestType: "track",
					RoutingKey:  "routing-key-uad-1",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.1",
				},
				Payload: json.RawMessage(`{"type":"track","event":"Product Viewed","userId":"user-uad-001","messageId":"msg-uad-001","rudderId":"rudder-uad-001","request_ip":"192.0.2.1","receivedAt":"2024-01-01T00:00:00Z","context":{"userAgentData":{"brands":[{"brand":"Chromium","version":"110"},{"brand":"Google Chrome","version":"110"}],"mobile":false,"platform":"macOS"},"library":{"name":"analytics.js","version":"2.1.0"}}}`),
			},
		}
		payloadBytes, err := jsonrs.Marshal(messages)
		require.NoError(t, err)

		jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
		require.NoError(t, err)
		require.Len(t, jobs, 1)

		jobPayload := string(jobs[0].job.EventPayload)
		uadPath := "batch.0.context.userAgentData"

		// Verify userAgentData object exists
		require.True(t, gjson.Get(jobPayload, uadPath).Exists(),
			"userAgentData should exist in job payload")

		// Verify brands array
		require.True(t, gjson.Get(jobPayload, uadPath+".brands").IsArray(),
			"brands should be an array")
		require.Equal(t, int64(2), gjson.Get(jobPayload, uadPath+".brands.#").Int(),
			"brands should have 2 entries")
		require.Equal(t, "Chromium",
			gjson.Get(jobPayload, uadPath+".brands.0.brand").String(),
			"first brand name should be Chromium")
		require.Equal(t, "110",
			gjson.Get(jobPayload, uadPath+".brands.0.version").String(),
			"first brand version should be 110")
		require.Equal(t, "Google Chrome",
			gjson.Get(jobPayload, uadPath+".brands.1.brand").String(),
			"second brand name should be Google Chrome")
		require.Equal(t, "110",
			gjson.Get(jobPayload, uadPath+".brands.1.version").String(),
			"second brand version should be 110")

		// Verify mobile boolean (false) — explicit existence check since Bool() returns false for absent fields
		mobileResult := gjson.Get(jobPayload, uadPath+".mobile")
		require.True(t, mobileResult.Exists(),
			"mobile field should exist in userAgentData")
		require.Equal(t, false, mobileResult.Bool(),
			"mobile should be false")

		// Verify platform string
		require.Equal(t, "macOS",
			gjson.Get(jobPayload, uadPath+".platform").String(),
			"platform should be macOS")
	})

	t.Run("high-entropy fields preserved", func(t *testing.T) {
		gw := createTestGateway(t, backendconfig.EventBlocking{})
		messages := []stream.Message{
			{
				Properties: stream.MessageProperties{
					RequestType: "track",
					RoutingKey:  "routing-key-uad-2",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.2",
				},
				Payload: json.RawMessage(`{"type":"track","event":"Product Viewed","userId":"user-uad-002","messageId":"msg-uad-002","rudderId":"rudder-uad-002","request_ip":"192.0.2.2","receivedAt":"2024-01-01T00:00:00Z","context":{"userAgentData":{"brands":[{"brand":"Chromium","version":"110"},{"brand":"Google Chrome","version":"110"}],"mobile":false,"platform":"macOS","bitness":"64","model":"","platformVersion":"13.0.0","uaFullVersion":"110.0.5481.77","fullVersionList":[{"brand":"Chromium","version":"110.0.5481.77"},{"brand":"Google Chrome","version":"110.0.5481.77"}],"wow64":false},"library":{"name":"analytics.js","version":"2.1.0"}}}`),
			},
		}
		payloadBytes, err := jsonrs.Marshal(messages)
		require.NoError(t, err)

		jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
		require.NoError(t, err)
		require.Len(t, jobs, 1)

		jobPayload := string(jobs[0].job.EventPayload)
		uadPath := "batch.0.context.userAgentData"

		// Low-entropy fields
		require.True(t, gjson.Get(jobPayload, uadPath+".brands").IsArray(),
			"brands should be preserved as array")
		mobileResult := gjson.Get(jobPayload, uadPath+".mobile")
		require.True(t, mobileResult.Exists(), "mobile field should exist")
		require.Equal(t, false, mobileResult.Bool(), "mobile should be false")
		require.Equal(t, "macOS", gjson.Get(jobPayload, uadPath+".platform").String(),
			"platform should be macOS")

		// High-entropy fields
		require.Equal(t, "64",
			gjson.Get(jobPayload, uadPath+".bitness").String(),
			"bitness should be preserved")
		require.True(t, gjson.Get(jobPayload, uadPath+".model").Exists(),
			"model should exist even when empty string")
		require.Equal(t, "",
			gjson.Get(jobPayload, uadPath+".model").String(),
			"model should be preserved as empty string")
		require.Equal(t, "13.0.0",
			gjson.Get(jobPayload, uadPath+".platformVersion").String(),
			"platformVersion should be preserved")
		require.Equal(t, "110.0.5481.77",
			gjson.Get(jobPayload, uadPath+".uaFullVersion").String(),
			"uaFullVersion should be preserved")

		// fullVersionList array
		require.True(t, gjson.Get(jobPayload, uadPath+".fullVersionList").IsArray(),
			"fullVersionList should be an array")
		require.Equal(t, int64(2),
			gjson.Get(jobPayload, uadPath+".fullVersionList.#").Int(),
			"fullVersionList should have 2 entries")
		require.Equal(t, "Chromium",
			gjson.Get(jobPayload, uadPath+".fullVersionList.0.brand").String(),
			"first fullVersionList brand should be Chromium")
		require.Equal(t, "110.0.5481.77",
			gjson.Get(jobPayload, uadPath+".fullVersionList.0.version").String(),
			"first fullVersionList version should be 110.0.5481.77")
		require.Equal(t, "Google Chrome",
			gjson.Get(jobPayload, uadPath+".fullVersionList.1.brand").String(),
			"second fullVersionList brand should be Google Chrome")

		// wow64 boolean (false) — explicit existence check
		wow64Result := gjson.Get(jobPayload, uadPath+".wow64")
		require.True(t, wow64Result.Exists(), "wow64 field should exist")
		require.Equal(t, false, wow64Result.Bool(), "wow64 should be false")
	})

	t.Run("coexists with userAgent string", func(t *testing.T) {
		gw := createTestGateway(t, backendconfig.EventBlocking{})
		messages := []stream.Message{
			{
				Properties: stream.MessageProperties{
					RequestType: "track",
					RoutingKey:  "routing-key-uad-3",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.3",
				},
				Payload: json.RawMessage(`{"type":"track","event":"Product Viewed","userId":"user-uad-003","messageId":"msg-uad-003","rudderId":"rudder-uad-003","request_ip":"192.0.2.3","receivedAt":"2024-01-01T00:00:00Z","context":{"userAgent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36","userAgentData":{"brands":[{"brand":"Chromium","version":"110"}],"mobile":false,"platform":"macOS"},"library":{"name":"analytics.js","version":"2.1.0"}}}`),
			},
		}
		payloadBytes, err := jsonrs.Marshal(messages)
		require.NoError(t, err)

		jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
		require.NoError(t, err)
		require.Len(t, jobs, 1)

		jobPayload := string(jobs[0].job.EventPayload)

		// Verify userAgent string is preserved
		require.Equal(t,
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
			gjson.Get(jobPayload, "batch.0.context.userAgent").String(),
			"userAgent string should be preserved")

		// Verify userAgentData object is preserved alongside userAgent string
		uadPath := "batch.0.context.userAgentData"
		require.True(t, gjson.Get(jobPayload, uadPath).Exists(),
			"userAgentData object should coexist with userAgent string")
		require.Equal(t, "Chromium",
			gjson.Get(jobPayload, uadPath+".brands.0.brand").String(),
			"userAgentData brands should be preserved alongside userAgent string")
		mobileResult := gjson.Get(jobPayload, uadPath+".mobile")
		require.True(t, mobileResult.Exists(), "mobile field should exist")
		require.Equal(t, false, mobileResult.Bool(),
			"userAgentData mobile should be preserved")
		require.Equal(t, "macOS",
			gjson.Get(jobPayload, uadPath+".platform").String(),
			"userAgentData platform should be preserved")
	})

	t.Run("preserved for all event types", func(t *testing.T) {
		// Common userAgentData context JSON fragment used across all event types
		uadContext := `"userAgentData":{"brands":[{"brand":"Chromium","version":"110"}],"mobile":false,"platform":"macOS"},"library":{"name":"analytics.js","version":"2.1.0"}`

		eventTypes := []struct {
			name    string
			reqType string
			payload string
		}{
			{
				name:    "identify",
				reqType: "identify",
				payload: `{"type":"identify","userId":"user-et-001","messageId":"msg-et-001","rudderId":"rudder-et-001","request_ip":"192.0.2.10","receivedAt":"2024-01-01T00:00:00Z","traits":{"name":"Test User","email":"test@example.com"},"context":{` + uadContext + `}}`,
			},
			{
				name:    "track",
				reqType: "track",
				payload: `{"type":"track","event":"Order Completed","userId":"user-et-002","messageId":"msg-et-002","rudderId":"rudder-et-002","request_ip":"192.0.2.10","receivedAt":"2024-01-01T00:00:00Z","properties":{"revenue":99.99},"context":{` + uadContext + `}}`,
			},
			{
				name:    "page",
				reqType: "page",
				payload: `{"type":"page","name":"Home","userId":"user-et-003","messageId":"msg-et-003","rudderId":"rudder-et-003","request_ip":"192.0.2.10","receivedAt":"2024-01-01T00:00:00Z","properties":{"url":"https://example.com"},"context":{` + uadContext + `}}`,
			},
			{
				name:    "screen",
				reqType: "screen",
				payload: `{"type":"screen","name":"Dashboard","userId":"user-et-004","messageId":"msg-et-004","rudderId":"rudder-et-004","request_ip":"192.0.2.10","receivedAt":"2024-01-01T00:00:00Z","properties":{"screenClass":"Main"},"context":{` + uadContext + `}}`,
			},
			{
				name:    "group",
				reqType: "group",
				payload: `{"type":"group","groupId":"grp-001","userId":"user-et-005","messageId":"msg-et-005","rudderId":"rudder-et-005","request_ip":"192.0.2.10","receivedAt":"2024-01-01T00:00:00Z","traits":{"name":"Acme Corp","industry":"Technology"},"context":{` + uadContext + `}}`,
			},
			{
				name:    "alias",
				reqType: "alias",
				payload: `{"type":"alias","previousId":"old-user-001","userId":"user-et-006","messageId":"msg-et-006","rudderId":"rudder-et-006","request_ip":"192.0.2.10","receivedAt":"2024-01-01T00:00:00Z","context":{` + uadContext + `}}`,
			},
		}

		for _, et := range eventTypes {
			t.Run(et.name, func(t *testing.T) {
				gw := createTestGateway(t, backendconfig.EventBlocking{})
				messages := []stream.Message{
					{
						Properties: stream.MessageProperties{
							RequestType: et.reqType,
							RoutingKey:  "routing-key-et",
							WorkspaceID: "workspace1",
							SourceID:    "source-id-1",
							ReceivedAt:  time.Now(),
							RequestIP:   "192.0.2.10",
						},
						Payload: json.RawMessage(et.payload),
					},
				}
				payloadBytes, err := jsonrs.Marshal(messages)
				require.NoError(t, err)

				jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
				require.NoError(t, err)
				require.Len(t, jobs, 1, "should produce exactly 1 job for %s event", et.name)

				jobPayload := string(jobs[0].job.EventPayload)
				uadPath := "batch.0.context.userAgentData"

				require.True(t, gjson.Get(jobPayload, uadPath).Exists(),
					"userAgentData should be preserved for %s event", et.name)
				require.Equal(t, "Chromium",
					gjson.Get(jobPayload, uadPath+".brands.0.brand").String(),
					"userAgentData brands should be preserved for %s event", et.name)
				mobileResult := gjson.Get(jobPayload, uadPath+".mobile")
				require.True(t, mobileResult.Exists(),
					"mobile field should exist for %s event", et.name)
				require.Equal(t, false, mobileResult.Bool(),
					"userAgentData mobile should be false for %s event", et.name)
				require.Equal(t, "macOS",
					gjson.Get(jobPayload, uadPath+".platform").String(),
					"userAgentData platform should be macOS for %s event", et.name)
			})
		}
	})
}

// TestChannelFieldPreservation verifies that context.channel field values (server, browser, mobile)
// pass through the Gateway Handle pipeline without modification, and that an absent channel field
// is not auto-populated by the Gateway. This covers ES-007: Channel Field Auto-Population.
func TestChannelFieldPreservation(t *testing.T) {
	tests := []struct {
		name          string
		channel       string
		expectPresent bool
	}{
		{
			name:          "server channel preserved",
			channel:       "server",
			expectPresent: true,
		},
		{
			name:          "browser channel preserved",
			channel:       "browser",
			expectPresent: true,
		},
		{
			name:          "mobile channel preserved",
			channel:       "mobile",
			expectPresent: true,
		},
		{
			name:          "channel field absent stays absent",
			channel:       "",
			expectPresent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := createTestGateway(t, backendconfig.EventBlocking{})

			// Build the event payload — include context.channel only when expectPresent is true
			var payloadStr string
			if tt.expectPresent {
				payloadStr = fmt.Sprintf(
					`{"type":"track","event":"Test Event","userId":"user-ch-001","messageId":"msg-ch-001","rudderId":"rudder-ch-001","request_ip":"192.0.2.20","receivedAt":"2024-01-01T00:00:00Z","context":{"channel":%q,"library":{"name":"analytics.js","version":"2.1.0"}}}`,
					tt.channel,
				)
			} else {
				payloadStr = `{"type":"track","event":"Test Event","userId":"user-ch-001","messageId":"msg-ch-001","rudderId":"rudder-ch-001","request_ip":"192.0.2.20","receivedAt":"2024-01-01T00:00:00Z","context":{"library":{"name":"analytics.js","version":"2.1.0"}}}`
			}

			messages := []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: "track",
						RoutingKey:  "routing-key-ch",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "192.0.2.20",
					},
					Payload: json.RawMessage(payloadStr),
				},
			}
			payloadBytes, err := jsonrs.Marshal(messages)
			require.NoError(t, err)

			jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
			require.NoError(t, err)
			require.Len(t, jobs, 1)

			jobPayload := string(jobs[0].job.EventPayload)
			channelResult := gjson.Get(jobPayload, "batch.0.context.channel")

			if tt.expectPresent {
				require.True(t, channelResult.Exists(),
					"context.channel should be present in output")
				require.Equal(t, tt.channel, channelResult.String(),
					"context.channel value should be preserved as %q", tt.channel)
			} else {
				require.False(t, channelResult.Exists(),
					"context.channel should not be present when not in input")
			}
		})
	}
}

// TestMobileSDKContextPreservation validates that iOS and Android SDK payloads preserve all mobile
// context fields (device, os, app, network, screen) and lifecycle events through the Gateway pipeline.
// This covers E-007: iOS and Android Mobile SDK Compatibility Testing.
func TestMobileSDKContextPreservation(t *testing.T) {
	tests := []struct {
		name       string
		reqType    string
		payload    string
		assertions func(t *testing.T, jobPayload string)
	}{
		{
			name:    "analytics-ios identify with full mobile context",
			reqType: "identify",
			payload: `{"type":"identify","userId":"user-ios-001","anonymousId":"anon-ios-001","messageId":"msg-ios-001","rudderId":"rudder-ios-001","request_ip":"192.0.2.50","receivedAt":"2024-01-01T00:00:00Z","traits":{"name":"iOS Test User","email":"ios@test.com"},"context":{"library":{"name":"analytics-ios","version":"4.1.0"},"device":{"id":"device-ios-123","manufacturer":"Apple","model":"iPhone 15","name":"Test iPhone","type":"ios"},"os":{"name":"iOS","version":"17.2"},"app":{"name":"TestApp","version":"2.1.0","build":"1234","namespace":"com.test.app"},"network":{"bluetooth":false,"carrier":"T-Mobile","cellular":true,"wifi":true},"screen":{"density":3,"height":2532,"width":1170}}}`,
			assertions: func(t *testing.T, jobPayload string) {
				ctx := "batch.0.context"

				// Library metadata
				require.Equal(t, "analytics-ios",
					gjson.Get(jobPayload, ctx+".library.name").String(),
					"context.library.name should be analytics-ios")
				require.Equal(t, "4.1.0",
					gjson.Get(jobPayload, ctx+".library.version").String(),
					"context.library.version should be 4.1.0")

				// Device fields
				require.Equal(t, "device-ios-123",
					gjson.Get(jobPayload, ctx+".device.id").String(),
					"context.device.id should be preserved")
				require.Equal(t, "Apple",
					gjson.Get(jobPayload, ctx+".device.manufacturer").String(),
					"context.device.manufacturer should be preserved")
				require.Equal(t, "iPhone 15",
					gjson.Get(jobPayload, ctx+".device.model").String(),
					"context.device.model should be preserved")
				require.Equal(t, "Test iPhone",
					gjson.Get(jobPayload, ctx+".device.name").String(),
					"context.device.name should be preserved")
				require.Equal(t, "ios",
					gjson.Get(jobPayload, ctx+".device.type").String(),
					"context.device.type should be preserved")

				// OS fields
				require.Equal(t, "iOS",
					gjson.Get(jobPayload, ctx+".os.name").String(),
					"context.os.name should be preserved")
				require.Equal(t, "17.2",
					gjson.Get(jobPayload, ctx+".os.version").String(),
					"context.os.version should be preserved")

				// App fields
				require.Equal(t, "TestApp",
					gjson.Get(jobPayload, ctx+".app.name").String(),
					"context.app.name should be preserved")
				require.Equal(t, "2.1.0",
					gjson.Get(jobPayload, ctx+".app.version").String(),
					"context.app.version should be preserved")
				require.Equal(t, "1234",
					gjson.Get(jobPayload, ctx+".app.build").String(),
					"context.app.build should be preserved")
				require.Equal(t, "com.test.app",
					gjson.Get(jobPayload, ctx+".app.namespace").String(),
					"context.app.namespace should be preserved")

				// Network fields — explicit existence checks for booleans
				bluetoothResult := gjson.Get(jobPayload, ctx+".network.bluetooth")
				require.True(t, bluetoothResult.Exists(),
					"context.network.bluetooth should exist")
				require.Equal(t, false, bluetoothResult.Bool(),
					"context.network.bluetooth should be false")
				require.Equal(t, "T-Mobile",
					gjson.Get(jobPayload, ctx+".network.carrier").String(),
					"context.network.carrier should be preserved")
				cellularResult := gjson.Get(jobPayload, ctx+".network.cellular")
				require.True(t, cellularResult.Exists(),
					"context.network.cellular should exist")
				require.Equal(t, true, cellularResult.Bool(),
					"context.network.cellular should be true")
				wifiResult := gjson.Get(jobPayload, ctx+".network.wifi")
				require.True(t, wifiResult.Exists(),
					"context.network.wifi should exist")
				require.Equal(t, true, wifiResult.Bool(),
					"context.network.wifi should be true")

				// Screen fields
				require.Equal(t, float64(3),
					gjson.Get(jobPayload, ctx+".screen.density").Float(),
					"context.screen.density should be preserved")
				require.Equal(t, float64(2532),
					gjson.Get(jobPayload, ctx+".screen.height").Float(),
					"context.screen.height should be preserved")
				require.Equal(t, float64(1170),
					gjson.Get(jobPayload, ctx+".screen.width").Float(),
					"context.screen.width should be preserved")
			},
		},
		{
			name:    "analytics-android track with full mobile context",
			reqType: "track",
			payload: `{"type":"track","event":"Product Viewed","userId":"user-android-001","anonymousId":"anon-android-001","messageId":"msg-android-001","rudderId":"rudder-android-001","request_ip":"192.0.2.51","receivedAt":"2024-01-01T00:00:00Z","properties":{"product_id":"SKU-123","name":"Test Product","price":29.99},"context":{"library":{"name":"analytics-android","version":"4.11.3"},"device":{"id":"device-android-456","manufacturer":"Samsung","model":"Galaxy S24","name":"Test Galaxy","type":"android"},"os":{"name":"Android","version":"14"},"app":{"name":"TestApp","version":"3.0.0","build":"5678","namespace":"com.test.androidapp"},"network":{"bluetooth":true,"carrier":"Verizon","cellular":true,"wifi":false},"screen":{"density":2.75,"height":2340,"width":1080}}}`,
			assertions: func(t *testing.T, jobPayload string) {
				ctx := "batch.0.context"

				// Library metadata
				require.Equal(t, "analytics-android",
					gjson.Get(jobPayload, ctx+".library.name").String(),
					"context.library.name should be analytics-android")
				require.Equal(t, "4.11.3",
					gjson.Get(jobPayload, ctx+".library.version").String(),
					"context.library.version should be 4.11.3")

				// Device fields
				require.Equal(t, "device-android-456",
					gjson.Get(jobPayload, ctx+".device.id").String(),
					"context.device.id should be preserved")
				require.Equal(t, "Samsung",
					gjson.Get(jobPayload, ctx+".device.manufacturer").String(),
					"context.device.manufacturer should be preserved")
				require.Equal(t, "Galaxy S24",
					gjson.Get(jobPayload, ctx+".device.model").String(),
					"context.device.model should be preserved")
				require.Equal(t, "Test Galaxy",
					gjson.Get(jobPayload, ctx+".device.name").String(),
					"context.device.name should be preserved")
				require.Equal(t, "android",
					gjson.Get(jobPayload, ctx+".device.type").String(),
					"context.device.type should be preserved")

				// OS fields
				require.Equal(t, "Android",
					gjson.Get(jobPayload, ctx+".os.name").String(),
					"context.os.name should be preserved")
				require.Equal(t, "14",
					gjson.Get(jobPayload, ctx+".os.version").String(),
					"context.os.version should be preserved")

				// App fields
				require.Equal(t, "TestApp",
					gjson.Get(jobPayload, ctx+".app.name").String(),
					"context.app.name should be preserved")
				require.Equal(t, "3.0.0",
					gjson.Get(jobPayload, ctx+".app.version").String(),
					"context.app.version should be preserved")
				require.Equal(t, "5678",
					gjson.Get(jobPayload, ctx+".app.build").String(),
					"context.app.build should be preserved")
				require.Equal(t, "com.test.androidapp",
					gjson.Get(jobPayload, ctx+".app.namespace").String(),
					"context.app.namespace should be preserved")

				// Network fields
				bluetoothResult := gjson.Get(jobPayload, ctx+".network.bluetooth")
				require.True(t, bluetoothResult.Exists(),
					"context.network.bluetooth should exist")
				require.Equal(t, true, bluetoothResult.Bool(),
					"context.network.bluetooth should be true")
				require.Equal(t, "Verizon",
					gjson.Get(jobPayload, ctx+".network.carrier").String(),
					"context.network.carrier should be preserved")
				cellularResult := gjson.Get(jobPayload, ctx+".network.cellular")
				require.True(t, cellularResult.Exists(),
					"context.network.cellular should exist")
				require.Equal(t, true, cellularResult.Bool(),
					"context.network.cellular should be true")
				wifiResult := gjson.Get(jobPayload, ctx+".network.wifi")
				require.True(t, wifiResult.Exists(),
					"context.network.wifi should exist")
				require.Equal(t, false, wifiResult.Bool(),
					"context.network.wifi should be false")

				// Screen fields
				require.InDelta(t, 2.75,
					gjson.Get(jobPayload, ctx+".screen.density").Float(), 0.01,
					"context.screen.density should be preserved")
				require.Equal(t, float64(2340),
					gjson.Get(jobPayload, ctx+".screen.height").Float(),
					"context.screen.height should be preserved")
				require.Equal(t, float64(1080),
					gjson.Get(jobPayload, ctx+".screen.width").Float(),
					"context.screen.width should be preserved")

				// Also verify the track event properties are preserved
				require.Equal(t, "Product Viewed",
					gjson.Get(jobPayload, "batch.0.event").String(),
					"event name should be preserved")
				require.Equal(t, "SKU-123",
					gjson.Get(jobPayload, "batch.0.properties.product_id").String(),
					"properties.product_id should be preserved")
			},
		},
		{
			name:    "iOS screen call with category and name",
			reqType: "screen",
			payload: `{"type":"screen","name":"Home Screen","userId":"user-ios-002","anonymousId":"anon-ios-002","messageId":"msg-ios-screen-001","rudderId":"rudder-ios-screen-001","request_ip":"192.0.2.52","receivedAt":"2024-01-01T00:00:00Z","properties":{"category":"Main","name":"Home Screen"},"context":{"library":{"name":"analytics-ios","version":"4.1.0"},"device":{"id":"device-ios-789","manufacturer":"Apple","model":"iPad Pro","name":"Test iPad","type":"ios"},"os":{"name":"iOS","version":"17.2"},"app":{"name":"TestApp","version":"2.1.0","build":"1234","namespace":"com.test.app"}}}`,
			assertions: func(t *testing.T, jobPayload string) {
				// Screen-specific fields
				require.Equal(t, "screen",
					gjson.Get(jobPayload, "batch.0.type").String(),
					"type should be screen")
				require.Equal(t, "Home Screen",
					gjson.Get(jobPayload, "batch.0.name").String(),
					"screen name should be preserved")
				require.Equal(t, "Main",
					gjson.Get(jobPayload, "batch.0.properties.category").String(),
					"properties.category should be preserved")
				require.Equal(t, "Home Screen",
					gjson.Get(jobPayload, "batch.0.properties.name").String(),
					"properties.name should be preserved")

				// iOS mobile context fields
				ctx := "batch.0.context"
				require.Equal(t, "analytics-ios",
					gjson.Get(jobPayload, ctx+".library.name").String(),
					"context.library.name should be analytics-ios")
				require.Equal(t, "device-ios-789",
					gjson.Get(jobPayload, ctx+".device.id").String(),
					"context.device.id should be preserved")
				require.Equal(t, "iPad Pro",
					gjson.Get(jobPayload, ctx+".device.model").String(),
					"context.device.model should be preserved")
				require.Equal(t, "iOS",
					gjson.Get(jobPayload, ctx+".os.name").String(),
					"context.os.name should be preserved")
			},
		},
		{
			name:    "Application Opened lifecycle event",
			reqType: "track",
			payload: `{"type":"track","event":"Application Opened","userId":"user-ios-003","anonymousId":"anon-ios-003","messageId":"msg-lifecycle-001","rudderId":"rudder-lifecycle-001","request_ip":"192.0.2.53","receivedAt":"2024-01-01T00:00:00Z","properties":{"from_background":false,"version":"2.1.0","build":"1234"},"context":{"library":{"name":"analytics-ios","version":"4.1.0"},"device":{"id":"device-ios-lc-001","manufacturer":"Apple","model":"iPhone 15","type":"ios"},"os":{"name":"iOS","version":"17.2"},"app":{"name":"TestApp","version":"2.1.0","build":"1234","namespace":"com.test.app"}}}`,
			assertions: func(t *testing.T, jobPayload string) {
				// Lifecycle event fields
				require.Equal(t, "Application Opened",
					gjson.Get(jobPayload, "batch.0.event").String(),
					"lifecycle event name should be preserved")
				require.Equal(t, "track",
					gjson.Get(jobPayload, "batch.0.type").String(),
					"type should be track for lifecycle events")

				// Lifecycle event properties
				fromBgResult := gjson.Get(jobPayload, "batch.0.properties.from_background")
				require.True(t, fromBgResult.Exists(),
					"properties.from_background should exist")
				require.Equal(t, false, fromBgResult.Bool(),
					"properties.from_background should be false")
				require.Equal(t, "2.1.0",
					gjson.Get(jobPayload, "batch.0.properties.version").String(),
					"properties.version should be preserved")
				require.Equal(t, "1234",
					gjson.Get(jobPayload, "batch.0.properties.build").String(),
					"properties.build should be preserved")

				// Mobile context preserved with lifecycle event
				require.Equal(t, "analytics-ios",
					gjson.Get(jobPayload, "batch.0.context.library.name").String(),
					"context.library.name should be analytics-ios")
				require.Equal(t, "device-ios-lc-001",
					gjson.Get(jobPayload, "batch.0.context.device.id").String(),
					"context.device.id should be preserved")
			},
		},
		{
			name:    "Application Backgrounded lifecycle event",
			reqType: "track",
			payload: `{"type":"track","event":"Application Backgrounded","userId":"user-android-002","anonymousId":"anon-android-002","messageId":"msg-lifecycle-002","rudderId":"rudder-lifecycle-002","request_ip":"192.0.2.54","receivedAt":"2024-01-01T00:00:00Z","properties":{},"context":{"library":{"name":"analytics-android","version":"4.11.3"},"device":{"id":"device-android-lc-002","manufacturer":"Samsung","model":"Galaxy S24","type":"android"},"os":{"name":"Android","version":"14"},"app":{"name":"TestApp","version":"3.0.0","build":"5678","namespace":"com.test.androidapp"}}}`,
			assertions: func(t *testing.T, jobPayload string) {
				// Lifecycle event name
				require.Equal(t, "Application Backgrounded",
					gjson.Get(jobPayload, "batch.0.event").String(),
					"lifecycle event name should be preserved")
				require.Equal(t, "track",
					gjson.Get(jobPayload, "batch.0.type").String(),
					"type should be track for lifecycle events")

				// Android context preserved
				require.Equal(t, "analytics-android",
					gjson.Get(jobPayload, "batch.0.context.library.name").String(),
					"context.library.name should be analytics-android")
				require.Equal(t, "device-android-lc-002",
					gjson.Get(jobPayload, "batch.0.context.device.id").String(),
					"context.device.id should be preserved")
				require.Equal(t, "Android",
					gjson.Get(jobPayload, "batch.0.context.os.name").String(),
					"context.os.name should be preserved")
			},
		},
		{
			name:    "Application Updated lifecycle event",
			reqType: "track",
			payload: `{"type":"track","event":"Application Updated","userId":"user-ios-004","anonymousId":"anon-ios-004","messageId":"msg-lifecycle-003","rudderId":"rudder-lifecycle-003","request_ip":"192.0.2.55","receivedAt":"2024-01-01T00:00:00Z","properties":{"previous_version":"1.0.0","previous_build":"100","version":"2.0.0","build":"200"},"context":{"library":{"name":"analytics-ios","version":"4.1.0"},"device":{"id":"device-ios-lc-003","manufacturer":"Apple","model":"iPhone 15","type":"ios"},"os":{"name":"iOS","version":"17.2"},"app":{"name":"TestApp","version":"2.0.0","build":"200","namespace":"com.test.app"}}}`,
			assertions: func(t *testing.T, jobPayload string) {
				// Lifecycle event name
				require.Equal(t, "Application Updated",
					gjson.Get(jobPayload, "batch.0.event").String(),
					"lifecycle event name should be preserved")

				// Update-specific properties
				require.Equal(t, "1.0.0",
					gjson.Get(jobPayload, "batch.0.properties.previous_version").String(),
					"properties.previous_version should be preserved")
				require.Equal(t, "100",
					gjson.Get(jobPayload, "batch.0.properties.previous_build").String(),
					"properties.previous_build should be preserved")
				require.Equal(t, "2.0.0",
					gjson.Get(jobPayload, "batch.0.properties.version").String(),
					"properties.version should be preserved")
				require.Equal(t, "200",
					gjson.Get(jobPayload, "batch.0.properties.build").String(),
					"properties.build should be preserved")

				// Mobile context
				require.Equal(t, "analytics-ios",
					gjson.Get(jobPayload, "batch.0.context.library.name").String(),
					"context.library.name should be analytics-ios")
			},
		},
		{
			name:    "Application Installed lifecycle event",
			reqType: "track",
			payload: `{"type":"track","event":"Application Installed","userId":"user-android-003","anonymousId":"anon-android-003","messageId":"msg-lifecycle-004","rudderId":"rudder-lifecycle-004","request_ip":"192.0.2.56","receivedAt":"2024-01-01T00:00:00Z","properties":{"version":"1.0.0","build":"100"},"context":{"library":{"name":"analytics-android","version":"4.11.3"},"device":{"id":"device-android-lc-004","manufacturer":"Google","model":"Pixel 8","type":"android"},"os":{"name":"Android","version":"14"},"app":{"name":"TestApp","version":"1.0.0","build":"100","namespace":"com.test.androidapp"}}}`,
			assertions: func(t *testing.T, jobPayload string) {
				// Lifecycle event name
				require.Equal(t, "Application Installed",
					gjson.Get(jobPayload, "batch.0.event").String(),
					"lifecycle event name should be preserved")
				require.Equal(t, "track",
					gjson.Get(jobPayload, "batch.0.type").String(),
					"type should be track for lifecycle events")

				// Install properties
				require.Equal(t, "1.0.0",
					gjson.Get(jobPayload, "batch.0.properties.version").String(),
					"properties.version should be preserved")
				require.Equal(t, "100",
					gjson.Get(jobPayload, "batch.0.properties.build").String(),
					"properties.build should be preserved")

				// Android context preserved
				require.Equal(t, "analytics-android",
					gjson.Get(jobPayload, "batch.0.context.library.name").String(),
					"context.library.name should be analytics-android")
				require.Equal(t, "Google",
					gjson.Get(jobPayload, "batch.0.context.device.manufacturer").String(),
					"context.device.manufacturer should be preserved")
				require.Equal(t, "Pixel 8",
					gjson.Get(jobPayload, "batch.0.context.device.model").String(),
					"context.device.model should be preserved")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := createTestGateway(t, backendconfig.EventBlocking{})
			messages := []stream.Message{
				{
					Properties: stream.MessageProperties{
						RequestType: tt.reqType,
						RoutingKey:  "routing-key-mobile-sdk",
						WorkspaceID: "workspace1",
						SourceID:    "source-id-1",
						ReceivedAt:  time.Now(),
						RequestIP:   "192.0.2.50",
					},
					Payload: json.RawMessage(tt.payload),
				},
			}
			payloadBytes, err := jsonrs.Marshal(messages)
			require.NoError(t, err)

			jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
			require.NoError(t, err)
			require.Len(t, jobs, 1, "should produce exactly 1 job")

			jobPayload := string(jobs[0].job.EventPayload)
			tt.assertions(t, jobPayload)
		})
	}
}

// TestServerSDKBatchCompatibility validates server-side SDK batch payload processing through the
// Gateway pipeline, including mixed event type batches, large batches, duplicate messageId handling,
// and per-platform library metadata preservation.
// This covers E-008: Server-Side SDK Compatibility Testing.
func TestServerSDKBatchCompatibility(t *testing.T) {
	t.Run("mixed event type batch with all 6 call types", func(t *testing.T) {
		gw := createTestGateway(t, backendconfig.EventBlocking{})

		// Server-side SDKs send mixed event types in a single batch request.
		// Each event has a different type field in the payload.
		messages := []stream.Message{
			{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  "routing-key-batch-mixed",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.60",
				},
				Payload: json.RawMessage(`{"type":"identify","userId":"user-srv-001","messageId":"msg-batch-id","rudderId":"rudder-batch-id","request_ip":"192.0.2.60","receivedAt":"2024-01-01T00:00:00Z","traits":{"name":"Server User","email":"srv@test.com"},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`),
			},
			{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  "routing-key-batch-mixed",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.60",
				},
				Payload: json.RawMessage(`{"type":"track","event":"Order Completed","userId":"user-srv-001","messageId":"msg-batch-trk","rudderId":"rudder-batch-trk","request_ip":"192.0.2.60","receivedAt":"2024-01-01T00:00:00Z","properties":{"revenue":99.99,"orderId":"ORD-001"},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`),
			},
			{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  "routing-key-batch-mixed",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.60",
				},
				Payload: json.RawMessage(`{"type":"page","name":"Pricing","userId":"user-srv-001","messageId":"msg-batch-pg","rudderId":"rudder-batch-pg","request_ip":"192.0.2.60","receivedAt":"2024-01-01T00:00:00Z","properties":{"url":"https://example.com/pricing","title":"Pricing Page"},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`),
			},
			{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  "routing-key-batch-mixed",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.60",
				},
				Payload: json.RawMessage(`{"type":"screen","name":"Dashboard","userId":"user-srv-001","messageId":"msg-batch-scr","rudderId":"rudder-batch-scr","request_ip":"192.0.2.60","receivedAt":"2024-01-01T00:00:00Z","properties":{"screenClass":"Main"},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`),
			},
			{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  "routing-key-batch-mixed",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.60",
				},
				Payload: json.RawMessage(`{"type":"group","groupId":"grp-srv-001","userId":"user-srv-001","messageId":"msg-batch-grp","rudderId":"rudder-batch-grp","request_ip":"192.0.2.60","receivedAt":"2024-01-01T00:00:00Z","traits":{"name":"Acme Corp","plan":"enterprise"},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`),
			},
			{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  "routing-key-batch-mixed",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.60",
				},
				Payload: json.RawMessage(`{"type":"alias","previousId":"old-user-srv","userId":"user-srv-001","messageId":"msg-batch-als","rudderId":"rudder-batch-als","request_ip":"192.0.2.60","receivedAt":"2024-01-01T00:00:00Z","context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`),
			},
		}

		payloadBytes, err := jsonrs.Marshal(messages)
		require.NoError(t, err)

		jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
		require.NoError(t, err)
		require.Len(t, jobs, 6, "all 6 events in the mixed batch should produce jobs")

		// Verify each event type is preserved in the output
		expectedTypes := []string{"identify", "track", "page", "screen", "group", "alias"}
		for i, expectedType := range expectedTypes {
			jobPayload := string(jobs[i].job.EventPayload)
			actualType := gjson.Get(jobPayload, "batch.0.type").String()
			require.Equal(t, expectedType, actualType,
				"job %d: event type should be %q", i, expectedType)
		}
	})

	t.Run("batch payload size limit (4MB)", func(t *testing.T) {
		// The 4MB request size limit is enforced at the HTTP transport layer (handle.go webRequestHandler).
		// At the extractJobsFromInternalBatchPayload level, large payloads should be processed correctly
		// as long as they pass validation. This test creates a batch with events containing large property
		// values (~3MB total) to validate the pipeline handles substantial payloads without truncation.
		gw := createTestGateway(t, backendconfig.EventBlocking{})

		// Create a large property string (~60KB per event, 50 events ≈ 3MB)
		largeValue := strings.Repeat("x", 60000)
		messages := make([]stream.Message, 0, 50)
		for i := 0; i < 50; i++ {
			payload := fmt.Sprintf(
				`{"type":"track","event":"Large Event %d","userId":"user-large-%d","messageId":"msg-large-%d","rudderId":"rudder-large-%d","request_ip":"192.0.2.70","receivedAt":"2024-01-01T00:00:00Z","properties":{"large_data":"%s","index":%d},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`,
				i, i, i, i, largeValue, i,
			)
			messages = append(messages, stream.Message{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  fmt.Sprintf("routing-key-large-%d", i),
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.70",
				},
				Payload: json.RawMessage(payload),
			})
		}

		payloadBytes, err := jsonrs.Marshal(messages)
		require.NoError(t, err)

		// Verify the payload is substantial (at least 2MB to confirm we're testing near the limit)
		require.Greater(t, len(payloadBytes), 2*1024*1024,
			"test payload should be at least 2MB to validate large batch handling")

		jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
		require.NoError(t, err)
		require.Len(t, jobs, 50, "all 50 events in the large batch should produce jobs")

		// Verify first and last events are intact
		firstPayload := string(jobs[0].job.EventPayload)
		require.Equal(t, "Large Event 0",
			gjson.Get(firstPayload, "batch.0.event").String(),
			"first event name should be preserved")
		require.Equal(t, float64(0),
			gjson.Get(firstPayload, "batch.0.properties.index").Float(),
			"first event index should be preserved")

		lastPayload := string(jobs[49].job.EventPayload)
		require.Equal(t, "Large Event 49",
			gjson.Get(lastPayload, "batch.0.event").String(),
			"last event name should be preserved")
		require.Equal(t, float64(49),
			gjson.Get(lastPayload, "batch.0.properties.index").Float(),
			"last event index should be preserved")
	})

	t.Run("large batch with many events", func(t *testing.T) {
		gw := createTestGateway(t, backendconfig.EventBlocking{})

		// Create a batch with 120 events to validate the pipeline handles high-cardinality batches
		const eventCount = 120
		messages := make([]stream.Message, 0, eventCount)
		for i := 0; i < eventCount; i++ {
			payload := fmt.Sprintf(
				`{"type":"track","event":"Batch Event","userId":"user-many-%d","messageId":"msg-many-%d","rudderId":"rudder-many-%d","request_ip":"192.0.2.71","receivedAt":"2024-01-01T00:00:00Z","properties":{"index":%d},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`,
				i, i, i, i,
			)
			messages = append(messages, stream.Message{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  fmt.Sprintf("routing-key-many-%d", i),
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.71",
				},
				Payload: json.RawMessage(payload),
			})
		}

		payloadBytes, err := jsonrs.Marshal(messages)
		require.NoError(t, err)

		jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
		require.NoError(t, err)
		require.Len(t, jobs, eventCount, "all %d events should produce jobs", eventCount)

		// Spot-check events at different positions to verify ordering and data preservation
		for _, idx := range []int{0, 25, 50, 75, 99, eventCount - 1} {
			jobPayload := string(jobs[idx].job.EventPayload)
			require.Equal(t, float64(idx),
				gjson.Get(jobPayload, "batch.0.properties.index").Float(),
				"event at position %d should have correct index", idx)
			require.Equal(t, "Batch Event",
				gjson.Get(jobPayload, "batch.0.event").String(),
				"event at position %d should have correct event name", idx)
		}
	})

	t.Run("duplicate messageId handling in batch", func(t *testing.T) {
		// The Gateway does not deduplicate at ingestion — duplicate messageIds should both be accepted.
		gw := createTestGateway(t, backendconfig.EventBlocking{})

		sharedMessageID := "msg-duplicate-001"
		messages := []stream.Message{
			{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  "routing-key-dup-1",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.72",
				},
				Payload: json.RawMessage(fmt.Sprintf(
					`{"type":"track","event":"First Event","userId":"user-dup-001","messageId":"%s","rudderId":"rudder-dup-001","request_ip":"192.0.2.72","receivedAt":"2024-01-01T00:00:00Z","properties":{"order":1},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`,
					sharedMessageID,
				)),
			},
			{
				Properties: stream.MessageProperties{
					RequestType: "batch",
					RoutingKey:  "routing-key-dup-2",
					WorkspaceID: "workspace1",
					SourceID:    "source-id-1",
					ReceivedAt:  time.Now(),
					RequestIP:   "192.0.2.72",
				},
				Payload: json.RawMessage(fmt.Sprintf(
					`{"type":"track","event":"Second Event","userId":"user-dup-002","messageId":"%s","rudderId":"rudder-dup-002","request_ip":"192.0.2.72","receivedAt":"2024-01-01T00:00:00Z","properties":{"order":2},"context":{"library":{"name":"analytics-node","version":"6.2.0"}}}`,
					sharedMessageID,
				)),
			},
		}

		payloadBytes, err := jsonrs.Marshal(messages)
		require.NoError(t, err)

		jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
		require.NoError(t, err)
		require.Len(t, jobs, 2, "both events with duplicate messageId should produce jobs")

		// Verify both events are distinct despite sharing a messageId
		firstPayload := string(jobs[0].job.EventPayload)
		secondPayload := string(jobs[1].job.EventPayload)

		require.Equal(t, "First Event",
			gjson.Get(firstPayload, "batch.0.event").String(),
			"first event should be preserved")
		require.Equal(t, float64(1),
			gjson.Get(firstPayload, "batch.0.properties.order").Float(),
			"first event order should be 1")

		require.Equal(t, "Second Event",
			gjson.Get(secondPayload, "batch.0.event").String(),
			"second event should be preserved")
		require.Equal(t, float64(2),
			gjson.Get(secondPayload, "batch.0.properties.order").Float(),
			"second event order should be 2")
	})

	t.Run("server-side SDK library metadata per platform", func(t *testing.T) {
		// Each server-side SDK sets a distinct context.library.name and version.
		// Validate that the Gateway preserves these metadata fields for all 5 platforms.
		platforms := []struct {
			name           string
			libraryName    string
			libraryVersion string
		}{
			{name: "Node.js", libraryName: "analytics-node", libraryVersion: "6.2.0"},
			{name: "Python", libraryName: "analytics-python", libraryVersion: "2.2.3"},
			{name: "Go", libraryName: "analytics-go", libraryVersion: "3.3.0"},
			{name: "Java", libraryName: "analytics-java", libraryVersion: "3.5.0"},
			{name: "Ruby", libraryName: "analytics-ruby", libraryVersion: "2.4.0"},
		}

		for _, platform := range platforms {
			t.Run(platform.name, func(t *testing.T) {
				gw := createTestGateway(t, backendconfig.EventBlocking{})

				payload := fmt.Sprintf(
					`{"type":"track","event":"Server Event","userId":"user-%s","messageId":"msg-%s","rudderId":"rudder-%s","request_ip":"192.0.2.80","receivedAt":"2024-01-01T00:00:00Z","properties":{"sdk":"%s"},"context":{"library":{"name":"%s","version":"%s"},"channel":"server"}}`,
					platform.libraryName, platform.libraryName, platform.libraryName,
					platform.name, platform.libraryName, platform.libraryVersion,
				)

				messages := []stream.Message{
					{
						Properties: stream.MessageProperties{
							RequestType: "batch",
							RoutingKey:  "routing-key-srv-" + platform.libraryName,
							WorkspaceID: "workspace1",
							SourceID:    "source-id-1",
							ReceivedAt:  time.Now(),
							RequestIP:   "192.0.2.80",
						},
						Payload: json.RawMessage(payload),
					},
				}

				payloadBytes, err := jsonrs.Marshal(messages)
				require.NoError(t, err)

				jobs, err := gw.extractJobsFromInternalBatchPayload("batch", payloadBytes)
				require.NoError(t, err)
				require.Len(t, jobs, 1, "should produce exactly 1 job for %s SDK", platform.name)

				jobPayload := string(jobs[0].job.EventPayload)

				// Verify library name
				require.Equal(t, platform.libraryName,
					gjson.Get(jobPayload, "batch.0.context.library.name").String(),
					"%s SDK: context.library.name should be %q", platform.name, platform.libraryName)

				// Verify library version
				require.Equal(t, platform.libraryVersion,
					gjson.Get(jobPayload, "batch.0.context.library.version").String(),
					"%s SDK: context.library.version should be %q", platform.name, platform.libraryVersion)

				// Verify channel field is preserved for server-side SDKs
				require.Equal(t, "server",
					gjson.Get(jobPayload, "batch.0.context.channel").String(),
					"%s SDK: context.channel should be 'server'", platform.name)
			})
		}
	})
}
