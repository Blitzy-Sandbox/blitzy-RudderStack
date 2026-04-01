package azureeventhub

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger/mock_logger"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	mock_azureeventhub "github.com/rudderlabs/rudder-server/mocks/services/streammanager/azureeventhub"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
)

var (
	sampleMessage      = "sample message"
	sampleEventHubName = "test-hub"
)

// TestNewProducer validates that NewProducer successfully creates an AzureEventHubProducer
// when provided with a valid destination config containing all required fields:
// connectionString, eventHubNamespace, and eventHubName.
// Pattern follows firehose/TestNewProducer and eventbridge/TestNewProducer.
func TestNewProducer(t *testing.T) {
	destinationConfig := map[string]any{
		"connectionString":  "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=dGVzdGtleTEyMw==",
		"eventHubNamespace": "test-namespace",
		"eventHubName":      sampleEventHubName,
	}
	destination := backendconfig.DestinationT{
		Config:      destinationConfig,
		WorkspaceID: "sampleWorkspaceID",
	}
	timeOut := 10 * time.Second
	producer, err := NewProducer(&destination, common.Opts{Timeout: timeOut})
	assert.Nil(t, err)
	assert.NotNil(t, producer)
}

// TestNewProducerMissingConfig validates that NewProducer returns errors containing
// "Azure Event Hub" for all missing configuration scenarios. This is critical because
// the factory suite test in streammanager_suite_test.go performs:
//
//	assert.ErrorContains(t, err, "Azure Event Hub")
//
// to verify correct factory dispatch to this producer.
func TestNewProducerMissingConfig(t *testing.T) {
	t.Run("missing all config fields", func(t *testing.T) {
		destination := backendconfig.DestinationT{
			Config:      map[string]any{},
			WorkspaceID: "sampleWorkspaceID",
		}
		_, err := NewProducer(&destination, common.Opts{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Azure Event Hub")
	})

	t.Run("missing eventHubNamespace", func(t *testing.T) {
		destination := backendconfig.DestinationT{
			Config: map[string]any{
				"connectionString": "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=dGVzdGtleTEyMw==",
			},
			WorkspaceID: "sampleWorkspaceID",
		}
		_, err := NewProducer(&destination, common.Opts{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Azure Event Hub")
	})

	t.Run("missing eventHubName", func(t *testing.T) {
		destination := backendconfig.DestinationT{
			Config: map[string]any{
				"connectionString":  "Endpoint=sb://test.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=dGVzdGtleTEyMw==",
				"eventHubNamespace": "test-namespace",
			},
			WorkspaceID: "sampleWorkspaceID",
		}
		_, err := NewProducer(&destination, common.Opts{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Azure Event Hub")
	})

	t.Run("invalid connection string missing SAS key name", func(t *testing.T) {
		destination := backendconfig.DestinationT{
			Config: map[string]any{
				"connectionString":  "Endpoint=sb://test.servicebus.windows.net/",
				"eventHubNamespace": "test-namespace",
				"eventHubName":      sampleEventHubName,
			},
			WorkspaceID: "sampleWorkspaceID",
		}
		_, err := NewProducer(&destination, common.Opts{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Azure Event Hub")
	})
}

// TestProduceWithInvalidClient verifies that Produce returns a 400 error with
// "[AzureEventHub] error" prefix when the producer has a nil client. This acts
// as a safety guard matching the pattern from firehose/TestProduceWithInvalidClient.
func TestProduceWithInvalidClient(t *testing.T) {
	producer := &AzureEventHubProducer{}
	sampleEventJson := []byte("{}")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[AzureEventHub] error")
}

// TestProduceWithInvalidData validates that Produce correctly handles invalid
// JSON payloads and payloads missing the required "message" field, returning
// appropriate 400-status error tuples. Uses a mock client to get past the
// nil-client guard, isolating the payload validation logic.
// Pattern follows firehose/TestProduceWithInvalidData.
func TestProduceWithInvalidData(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockClient := mock_azureeventhub.NewMockAzureEventHubClient(ctrl)
	producer := &AzureEventHubProducer{client: mockClient, eventHubName: sampleEventHubName}

	// Invalid JSON payload — gjson parses gracefully but message field will be absent
	sampleEventJson := []byte("invalid json")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AzureEventHub] error :: message from payload not found", respMsg)

	// Empty JSON payload — no "message" key present
	sampleEventJson = []byte("{}")
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AzureEventHub] error :: message from payload not found", respMsg)

	// JSON payload with null message value
	sampleEventJson = []byte(`{"message":null}`)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AzureEventHub] error :: message from payload not found", respMsg)
}

// TestProduceWithServiceResponse tests the Produce method with a mocked
// AzureEventHubClient, covering successful delivery, general errors, and
// partition key routing. Follows the exact pattern from firehose/TestProduceWithServiceResponse.
func TestProduceWithServiceResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockClient := mock_azureeventhub.NewMockAzureEventHubClient(ctrl)
	producer := &AzureEventHubProducer{client: mockClient, eventHubName: sampleEventHubName}
	mockLogger := mock_logger.NewMockLogger(ctrl)
	pkgLogger = mockLogger

	sampleEventJson, _ := jsonrs.Marshal(map[string]string{
		"message": sampleMessage,
	})

	sampleMessageJson, _ := jsonrs.Marshal(sampleMessage)

	// Success case — SendEventDataBatch returns nil, expect 200
	mockClient.
		EXPECT().
		SendEventDataBatch(gomock.Any(), sampleMessageJson, "").
		Return(nil)
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 200, statusCode)
	assert.Equal(t, "Success", statusMsg)
	assert.NotEmpty(t, respMsg)
	assert.Contains(t, respMsg, "Message delivered to Azure Event Hub")

	// General error case — SendEventDataBatch returns error, expect 500 with Failure
	errorMsg := "connection refused"
	mockClient.
		EXPECT().
		SendEventDataBatch(gomock.Any(), sampleMessageJson, "").
		Return(errors.New(errorMsg))
	mockLogger.EXPECT().Errorn(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 500, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.NotEmpty(t, respMsg)
	assert.Contains(t, respMsg, errorMsg)

	// Success with partition key — validates enhanced partition routing feature
	sampleEventWithPK, _ := jsonrs.Marshal(map[string]string{
		"message":      sampleMessage,
		"partitionKey": "pk1",
	})
	mockClient.
		EXPECT().
		SendEventDataBatch(gomock.Any(), sampleMessageJson, "pk1").
		Return(nil)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventWithPK, map[string]string{})
	assert.Equal(t, 200, statusCode)
	assert.Equal(t, "Success", statusMsg)
	assert.Contains(t, respMsg, "partitionKey: pk1")

	// Error with partition key — validates error path includes proper logging
	mockClient.
		EXPECT().
		SendEventDataBatch(gomock.Any(), sampleMessageJson, "pk1").
		Return(errors.New("timeout"))
	mockLogger.EXPECT().Errorn(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventWithPK, map[string]string{})
	assert.Equal(t, 500, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "timeout")
}

// TestClose verifies that Close returns nil for the AzureEventHubProducer.
// The HTTP-based client does not require explicit cleanup, matching the no-op
// close pattern used by eventbridge, firehose, and kinesis producers.
func TestClose(t *testing.T) {
	producer := &AzureEventHubProducer{}
	err := producer.Close()
	assert.Nil(t, err)
}
