package redisstream

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger/mock_logger"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	mock_redisstream "github.com/rudderlabs/rudder-server/mocks/services/streammanager/redisstream"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
)

var (
	sampleStreamName = "test-stream"
	sampleMessage    = "sample message"
	sampleMessageID  = "1234567890-0"
)

// TestNewProducer verifies that NewProducer correctly constructs a
// RedisStreamProducer when a valid destination configuration with both
// the required "address" and "stream" fields is provided.
func TestNewProducer(t *testing.T) {
	destinationConfig := map[string]any{
		"address": "localhost:6379",
		"stream":  "test-stream",
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

// TestNewProducerMissingAddress verifies that NewProducer returns an error
// containing "Redis" when the destination config omits the required "address"
// field. The error string must contain "Redis" because the stream-manager
// factory tests assert ErrorContains(t, err, "Redis") for dispatch validation.
func TestNewProducerMissingAddress(t *testing.T) {
	destinationConfig := map[string]any{
		"stream": "test-stream",
	}
	destination := backendconfig.DestinationT{
		Config:      destinationConfig,
		WorkspaceID: "sampleWorkspaceID",
	}
	producer, err := NewProducer(&destination, common.Opts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Redis")
	assert.Nil(t, producer)
}

// TestNewProducerMissingStream verifies that NewProducer returns an error
// containing "Redis" when the destination config omits the required "stream"
// field. This guarantees that both mandatory config fields are validated
// before a producer is constructed.
func TestNewProducerMissingStream(t *testing.T) {
	destinationConfig := map[string]any{
		"address": "localhost:6379",
	}
	destination := backendconfig.DestinationT{
		Config:      destinationConfig,
		WorkspaceID: "sampleWorkspaceID",
	}
	producer, err := NewProducer(&destination, common.Opts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Redis")
	assert.Nil(t, producer)
}

// TestProduceWithInvalidClient verifies that Produce returns a 400 status
// with a descriptive error message when the producer's internal Redis client
// is nil — indicating that the producer was not properly initialised.
func TestProduceWithInvalidClient(t *testing.T) {
	producer := &RedisStreamProducer{}
	sampleEventJson := []byte("{}")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[RedisStream] error :: Could not create producer", respMsg)
}

// TestProduceWithInvalidData verifies that Produce returns a 400 status for
// payloads that either cannot be parsed as JSON or do not contain the required
// "message" field. Both invalid JSON strings and empty JSON objects must be
// rejected with the same "message from payload not found" error.
func TestProduceWithInvalidData(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockClient := mock_redisstream.NewMockRedisStreamClient(ctrl)
	producer := &RedisStreamProducer{client: mockClient, streamName: sampleStreamName}

	// Invalid Payload — malformed JSON
	sampleEventJson := []byte("invalid json")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[RedisStream] error :: message from payload not found", respMsg)

	// Empty Payload — valid JSON but no "message" key
	sampleEventJson = []byte("{}")
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[RedisStream] error :: message from payload not found", respMsg)
}

// TestProduceWithServiceResponse verifies the Produce method's behaviour for
// both successful and failed Redis XADD operations.
//
// Success case: the mock client returns a valid message ID and nil error,
// resulting in a 200 status and a response containing "Message delivered".
//
// Error case: the mock client returns an error (simulating a Redis NOPERM
// response), triggering an Errorn log call and a 500 failure response.
func TestProduceWithServiceResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockClient := mock_redisstream.NewMockRedisStreamClient(ctrl)
	mockLogger := mock_logger.NewMockLogger(ctrl)
	pkgLogger = mockLogger
	producer := &RedisStreamProducer{client: mockClient, streamName: sampleStreamName}

	sampleEventJson, _ := jsonrs.Marshal(map[string]string{
		"message": sampleMessage,
		"userId":  "someUser",
	})

	// Return success response — mock XADD returns a valid stream entry ID
	mockClient.EXPECT().XAdd(gomock.Any(), sampleStreamName, gomock.Any()).Return(sampleMessageID, nil)
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 200, statusCode)
	assert.Equal(t, "Success", statusMsg)
	assert.Contains(t, respMsg, "Message delivered")

	// Return error response — mock XADD returns a Redis NOPERM error
	mockClient.EXPECT().XAdd(gomock.Any(), sampleStreamName, gomock.Any()).Return("", errors.New("NOPERM this user has no permissions to run the 'xadd' command"))
	mockLogger.EXPECT().Errorn(gomock.Any(), gomock.Any()).Times(1)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, nil)
	assert.Equal(t, 500, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[RedisStream] error")
}

// TestClose verifies the Close method for both nil and non-nil client states.
// A producer with a nil client (uninitialised) should return nil without
// panicking. A producer with a valid mock client should delegate the Close
// call and return the client's result.
func TestClose(t *testing.T) {
	// nil client — safe no-op
	producer := &RedisStreamProducer{}
	err := producer.Close()
	assert.Nil(t, err)

	// with mock client — delegates Close to the client
	ctrl := gomock.NewController(t)
	mockClient := mock_redisstream.NewMockRedisStreamClient(ctrl)
	mockClient.EXPECT().Close().Return(nil)
	producer = &RedisStreamProducer{client: mockClient, streamName: sampleStreamName}
	err = producer.Close()
	assert.Nil(t, err)
}
