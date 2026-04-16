package amazonmsk

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger/mock_logger"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
)

var (
	sampleTopic   = "sampleMSKTopic"
	sampleMessage = "sample MSK message"
)

// TestNewProducer validates that the NewProducer constructor correctly creates an
// AmazonMSKProducer when given valid destination configuration including Region,
// IAMRoleARN, ExternalID, ClusterARN, BootstrapServers, and Topic.
func TestNewProducer(t *testing.T) {
	destinationConfig := map[string]any{
		"Region":           "us-east-1",
		"IAMRoleARN":       "sampleRoleArn",
		"ExternalID":       "sampleExternalID",
		"ClusterARN":       "arn:aws:kafka:us-east-1:123456789012:cluster/sample-cluster/abc-123",
		"BootstrapServers": "b-1.sample-cluster.abc123.c1.kafka.us-east-1.amazonaws.com:9098",
		"Topic":            sampleTopic,
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

// TestProduceWithInvalidClient validates that Produce returns a 400 status code
// with an appropriate error message when the MSK client is nil (producer was not
// properly initialized).
func TestProduceWithInvalidClient(t *testing.T) {
	producer := &AmazonMSKProducer{}
	sampleEventJson := []byte("{}")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AmazonMSK] error :: Could not create producer", respMsg)
}

// TestProduceWithInvalidData validates error handling for various invalid payload
// scenarios: invalid JSON, empty payload, missing topic, empty topic, and
// non-string topic values. Each case should return 400 with "Failure" status
// and a descriptive error message.
func TestProduceWithInvalidData(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockClient := NewMockMSKClient(ctrl)
	producer := &AmazonMSKProducer{client: mockClient}

	// Invalid Payload — non-JSON data should fail message extraction
	sampleEventJson := []byte("invalid json")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AmazonMSK] error :: message from payload not found", respMsg)

	// Empty Payload — empty JSON object has no "message" field
	sampleEventJson = []byte("{}")
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AmazonMSK] error :: message from payload not found", respMsg)

	// Payload without topic — message present but no topic field
	sampleEventJson, _ = jsonrs.Marshal(map[string]string{
		"message": sampleMessage,
	})
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AmazonMSK] error :: MSK topic not found", respMsg)

	// Payload with empty topic — topic field present but empty string
	sampleEventJson, _ = jsonrs.Marshal(map[string]any{
		"message": sampleMessage,
		"topic":   "",
	})
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AmazonMSK] error :: empty MSK topic", respMsg)

	// Payload with non-string topic — topic field present but wrong type
	sampleEventJson, _ = jsonrs.Marshal(map[string]any{
		"message": sampleMessage,
		"topic":   1,
	})
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[AmazonMSK] error :: Could not parse MSK topic to string", respMsg)
}

// TestProduceWithServiceResponse validates the Produce method's behavior when
// the MSK client returns different responses: success (nil error), general error
// (plain error), and AWS API error (smithy.GenericAPIError with FaultClient).
// Verifies status codes, status messages, response messages, and error logging.
func TestProduceWithServiceResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockClient := NewMockMSKClient(ctrl)
	producer := &AmazonMSKProducer{client: mockClient}
	mockLogger := mock_logger.NewMockLogger(ctrl)
	pkgLogger = mockLogger

	sampleEventJson, _ := jsonrs.Marshal(map[string]string{
		"message": sampleMessage,
		"topic":   sampleTopic,
	})

	// Success scenario — client returns nil error, expect 200/Success
	mockClient.
		EXPECT().
		SendMessage(gomock.Any(), gomock.Any()).
		Return(nil)
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 200, statusCode)
	assert.Equal(t, "Success", statusMsg)
	assert.NotEmpty(t, respMsg)

	// General error — non-AWS error maps to 500/Failure via common.ParseAWSError
	errorCode := "errorCode"
	mockClient.
		EXPECT().
		SendMessage(gomock.Any(), gomock.Any()).
		Return(errors.New(errorCode))
	mockLogger.EXPECT().Errorn(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 500, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.NotEmpty(t, respMsg)

	// AWS API error — client-fault error maps to 400 via common.ParseAWSError
	mockClient.
		EXPECT().
		SendMessage(gomock.Any(), gomock.Any()).
		Return(&smithy.GenericAPIError{
			Code:    errorCode,
			Message: errorCode,
			Fault:   smithy.FaultClient,
		})
	mockLogger.EXPECT().Errorn(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, errorCode, statusMsg)
	assert.NotEmpty(t, respMsg)
}

// TestClose validates that Close returns nil (no-op) consistent with other
// AWS destination producers (firehose, kinesis, eventbridge).
func TestClose(t *testing.T) {
	producer := &AmazonMSKProducer{}
	err := producer.Close()
	assert.Nil(t, err)
}
