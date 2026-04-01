package pulsar

import (
	"context"
	"errors"
	"testing"
	"time"

	pulsarclient "github.com/apache/pulsar-client-go/pulsar"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger/mock_logger"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
)

var (
	sampleServiceURL = "pulsar://localhost:6650"
	sampleTopic      = "persistent://public/default/test-topic"
	sampleMessage    = "sample message payload"
)

// mockMessageID is a minimal implementation of pulsarclient.MessageID for test assertions.
type mockMessageID struct{}

func (mockMessageID) Serialize() []byte    { return []byte("mock") }
func (mockMessageID) LedgerID() int64      { return 0 }
func (mockMessageID) EntryID() int64       { return 0 }
func (mockMessageID) BatchIdx() int32      { return 0 }
func (mockMessageID) PartitionIdx() int32  { return 0 }
func (mockMessageID) BatchSize() int32     { return 0 }
func (mockMessageID) String() string       { return "mock-message-id" }

// mockPulsarSender is a manual mock implementing the PulsarSender interface
// defined in manager.go. It allows injecting custom Send and Close behavior
// for deterministic unit testing without an actual Pulsar broker connection.
type mockPulsarSender struct {
	sendFunc  func(ctx context.Context, msg *pulsarclient.ProducerMessage) (pulsarclient.MessageID, error)
	closeFunc func()
}

// Send delegates to the injected sendFunc or returns a valid mockMessageID if no function is set.
func (m *mockPulsarSender) Send(ctx context.Context, msg *pulsarclient.ProducerMessage) (pulsarclient.MessageID, error) {
	if m.sendFunc != nil {
		return m.sendFunc(ctx, msg)
	}
	return mockMessageID{}, nil
}

// Close delegates to the injected closeFunc if set.
func (m *mockPulsarSender) Close() {
	if m.closeFunc != nil {
		m.closeFunc()
	}
}

// TestNewProducerWithMissingServiceURL verifies that NewProducer returns an error
// containing "Pulsar" when the serviceURL key is absent from the destination config.
func TestNewProducerWithMissingServiceURL(t *testing.T) {
	destination := backendconfig.DestinationT{
		Config: map[string]any{
			"topic": sampleTopic,
		},
		WorkspaceID: "sampleWorkspaceID",
	}
	producer, err := NewProducer(&destination, common.Opts{Timeout: 10 * time.Second})
	assert.NotNil(t, err)
	assert.Nil(t, producer)
	assert.ErrorContains(t, err, "Pulsar")
}

// TestNewProducerWithMissingTopic verifies that NewProducer returns an error
// containing "Pulsar" when the topic key is absent from the destination config.
func TestNewProducerWithMissingTopic(t *testing.T) {
	destination := backendconfig.DestinationT{
		Config: map[string]any{
			"serviceURL": sampleServiceURL,
		},
		WorkspaceID: "sampleWorkspaceID",
	}
	producer, err := NewProducer(&destination, common.Opts{Timeout: 10 * time.Second})
	assert.NotNil(t, err)
	assert.Nil(t, producer)
	assert.ErrorContains(t, err, "Pulsar")
}

// TestNewProducerWithEmptyServiceURL verifies that NewProducer returns an error
// containing "Pulsar" when the serviceURL is present but is an empty string.
func TestNewProducerWithEmptyServiceURL(t *testing.T) {
	destination := backendconfig.DestinationT{
		Config: map[string]any{
			"serviceURL": "",
			"topic":      sampleTopic,
		},
		WorkspaceID: "sampleWorkspaceID",
	}
	producer, err := NewProducer(&destination, common.Opts{Timeout: 10 * time.Second})
	assert.NotNil(t, err)
	assert.Nil(t, producer)
	assert.ErrorContains(t, err, "Pulsar")
}

// TestNewProducerWithEmptyTopic verifies that NewProducer returns an error
// containing "Pulsar" when the topic is present but is an empty string.
func TestNewProducerWithEmptyTopic(t *testing.T) {
	destination := backendconfig.DestinationT{
		Config: map[string]any{
			"serviceURL": sampleServiceURL,
			"topic":      "",
		},
		WorkspaceID: "sampleWorkspaceID",
	}
	producer, err := NewProducer(&destination, common.Opts{Timeout: 10 * time.Second})
	assert.NotNil(t, err)
	assert.Nil(t, producer)
	assert.ErrorContains(t, err, "Pulsar")
}

// TestNewProducerWithInvalidConfig verifies that NewProducer returns an error
// containing "Pulsar" when the destination config cannot be marshalled to JSON.
// Using a channel value in the config map triggers a JSON marshal failure.
func TestNewProducerWithInvalidConfig(t *testing.T) {
	destination := backendconfig.DestinationT{
		Config: map[string]any{
			"serviceURL": make(chan int), // channels cannot be marshalled to JSON
		},
		WorkspaceID: "sampleWorkspaceID",
	}
	producer, err := NewProducer(&destination, common.Opts{Timeout: 10 * time.Second})
	assert.NotNil(t, err)
	assert.Nil(t, producer)
	assert.ErrorContains(t, err, "Pulsar")
}

// TestProduceWithInvalidClient verifies that Produce returns a 400 status code
// with a "Failure" status and an error message containing "[Pulsar] error" when
// the PulsarProducer has a nil producer (sender). This mirrors the nil-client
// guard pattern from firehose/kinesis tests.
func TestProduceWithInvalidClient(t *testing.T) {
	producer := &PulsarProducer{}
	sampleEventJson := []byte("{}")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[Pulsar] error")
	assert.Contains(t, respMsg, "Could not create producer")
}

// TestProduceWithInvalidData verifies payload validation when the Pulsar sender
// is available but the incoming event data is invalid or missing the "message" field.
func TestProduceWithInvalidData(t *testing.T) {
	// Create a mock sender so the nil-producer guard passes and we test payload parsing
	mockSender := &mockPulsarSender{
		sendFunc: func(_ context.Context, _ *pulsarclient.ProducerMessage) (pulsarclient.MessageID, error) {
			t.Fatal("Send should not be called for invalid data")
			return mockMessageID{}, errors.New("should not be reached")
		},
	}
	producer := &PulsarProducer{producer: mockSender}

	// Test with invalid JSON — gjson parses gracefully but "message" field is nil
	sampleEventJson := []byte("invalid json")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[Pulsar] error")
	assert.Contains(t, respMsg, "message from payload not found")

	// Test with empty JSON object — "message" field is absent
	sampleEventJson = []byte("{}")
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[Pulsar] error")
	assert.Contains(t, respMsg, "message from payload not found")

	// Test with JSON where "message" is explicitly null
	sampleEventJson = []byte(`{"message": null}`)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[Pulsar] error")
	assert.Contains(t, respMsg, "message from payload not found")
}

// TestProduceWithServiceResponse verifies the behavior of Produce when the
// Pulsar sender returns success or failure. Uses gomock for the logger to verify
// error logging on send failure, following the firehose/kinesis test patterns.
func TestProduceWithServiceResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockLogger := mock_logger.NewMockLogger(ctrl)

	// Save and restore the original package logger
	originalLogger := pkgLogger
	pkgLogger = mockLogger
	defer func() { pkgLogger = originalLogger }()

	// Build a valid event payload with a "message" field
	sampleEventJson, err := jsonrs.Marshal(map[string]string{
		"message": sampleMessage,
	})
	assert.Nil(t, err)

	// Test success case — mock sender returns a valid message ID with nil error
	mockSender := &mockPulsarSender{
		sendFunc: func(_ context.Context, msg *pulsarclient.ProducerMessage) (pulsarclient.MessageID, error) {
			// Verify the message payload was populated
			assert.NotNil(t, msg)
			assert.NotEmpty(t, msg.Payload)
			return mockMessageID{}, nil
		},
	}
	producer := &PulsarProducer{producer: mockSender}
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 200, statusCode)
	assert.Equal(t, "Success", statusMsg)
	assert.NotEmpty(t, respMsg)
	assert.Contains(t, respMsg, "Message delivered")

	// Test send failure — mock sender returns an error, logger.Errorn should be called
	sendErr := errors.New("connection refused")
	mockSender = &mockPulsarSender{
		sendFunc: func(_ context.Context, _ *pulsarclient.ProducerMessage) (pulsarclient.MessageID, error) {
			return nil, sendErr
		},
	}
	// Expect Errorn to be called once with any message and any fields for the send failure
	mockLogger.EXPECT().Errorn(gomock.Any(), gomock.Any()).Times(1)
	producer = &PulsarProducer{producer: mockSender}
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 500, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "connection refused")
	assert.Contains(t, respMsg, "[Pulsar] error")
}

// TestProduceWithTimeout verifies that the Produce method correctly applies the
// configured timeout when sending messages. It validates that the context passed
// to the Send method has a deadline when opts.Timeout is set.
func TestProduceWithTimeout(t *testing.T) {
	// Build a valid event payload
	sampleEventJson, err := jsonrs.Marshal(map[string]string{
		"message": sampleMessage,
	})
	assert.Nil(t, err)

	// Track whether context has deadline
	var receivedCtxHasDeadline bool
	mockSender := &mockPulsarSender{
		sendFunc: func(ctx context.Context, _ *pulsarclient.ProducerMessage) (pulsarclient.MessageID, error) {
			_, receivedCtxHasDeadline = ctx.Deadline()
			return mockMessageID{}, nil
		},
	}

	// With timeout configured, context should have a deadline
	producer := &PulsarProducer{
		producer: mockSender,
		opts:     common.Opts{Timeout: 5 * time.Second},
	}
	statusCode, statusMsg, _ := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 200, statusCode)
	assert.Equal(t, "Success", statusMsg)
	assert.True(t, receivedCtxHasDeadline, "context should have a deadline when timeout is set")

	// With zero timeout, context should NOT have a deadline
	receivedCtxHasDeadline = false
	producer = &PulsarProducer{
		producer: mockSender,
		opts:     common.Opts{Timeout: 0},
	}
	statusCode, statusMsg, _ = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 200, statusCode)
	assert.Equal(t, "Success", statusMsg)
	assert.False(t, receivedCtxHasDeadline, "context should NOT have a deadline when timeout is zero")
}

// TestCloseWithNilProducer verifies that Close does not panic and returns nil
// when called on a PulsarProducer with nil producer and client fields.
func TestCloseWithNilProducer(t *testing.T) {
	producer := &PulsarProducer{}
	err := producer.Close()
	assert.Nil(t, err)
}

// TestCloseWithMockSender verifies that Close properly calls the Close method
// on the underlying PulsarSender when it is non-nil.
func TestCloseWithMockSender(t *testing.T) {
	closeCalled := false
	mockSender := &mockPulsarSender{
		closeFunc: func() {
			closeCalled = true
		},
	}
	producer := &PulsarProducer{producer: mockSender}
	err := producer.Close()
	assert.Nil(t, err)
	assert.True(t, closeCalled, "Close should call the underlying sender's Close method")
}
