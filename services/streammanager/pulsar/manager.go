package pulsar

import (
	"context"
	"encoding/json"
	"fmt"

	pulsarclient "github.com/apache/pulsar-client-go/pulsar"
	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
)

var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("streammanager").Child("pulsar")
}

// PulsarConfig holds the configuration for Apache Pulsar destination.
// Fields are deserialized from the destination's Config map via jsonrs Marshal/Unmarshal.
type PulsarConfig struct {
	ServiceURL string `json:"serviceURL"`
	Topic      string `json:"topic"`
}

// PulsarSender abstracts the Pulsar producer for testability.
// The pulsarclient.Producer type natively satisfies this interface.
type PulsarSender interface {
	Send(ctx context.Context, msg *pulsarclient.ProducerMessage) (pulsarclient.MessageID, error)
	Close()
}

// PulsarProducer implements common.StreamProducer for the Apache Pulsar destination.
// It holds a Pulsar client (for lifecycle management), a PulsarSender (for message dispatch),
// and the producer options carrying timeout configuration.
type PulsarProducer struct {
	client   pulsarclient.Client
	producer PulsarSender
	opts     common.Opts
}

// NewProducer creates a Pulsar producer based on the supplied destination config.
// It validates that serviceURL and topic are present, creates a Pulsar client, then creates
// a Pulsar producer bound to the configured topic. All errors contain the string "[Pulsar]"
// so the streammanager factory test can assert the correct producer was invoked.
func NewProducer(destination *backendconfig.DestinationT, opts common.Opts) (common.StreamProducer, error) {
	var pulsarConfig PulsarConfig

	// Marshal the destination Config map to JSON bytes, then unmarshal into PulsarConfig struct.
	// This follows the same pattern used by googlepubsub and other stream producers.
	jsonConfig, err := jsonrs.Marshal(destination.Config)
	if err != nil {
		return nil, fmt.Errorf("[Pulsar] error :: error while marshalling destination config: %w", err)
	}
	err = jsonrs.Unmarshal(jsonConfig, &pulsarConfig)
	if err != nil {
		return nil, fmt.Errorf("[Pulsar] error :: error while unmarshalling destination config: %w", err)
	}

	// Validate required configuration fields.
	if pulsarConfig.ServiceURL == "" {
		return nil, fmt.Errorf("[Pulsar] error :: serviceURL is required")
	}
	if pulsarConfig.Topic == "" {
		return nil, fmt.Errorf("[Pulsar] error :: topic is required")
	}

	// Create Pulsar client with the service URL and operation timeout from producer opts.
	clientOpts := pulsarclient.ClientOptions{
		URL:              pulsarConfig.ServiceURL,
		OperationTimeout: opts.Timeout,
	}
	client, err := pulsarclient.NewClient(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("[Pulsar] error :: failed to create client: %w", err)
	}

	// Create Pulsar producer bound to the configured topic.
	// If producer creation fails, close the client to avoid resource leaks.
	producer, err := client.CreateProducer(pulsarclient.ProducerOptions{
		Topic: pulsarConfig.Topic,
	})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("[Pulsar] error :: failed to create producer: %w", err)
	}

	return &PulsarProducer{
		client:   client,
		producer: producer,
		opts:     opts,
	}, nil
}

// Produce sends a message to the configured Pulsar topic.
// It extracts the "message" field from the JSON payload using gjson, marshals it to bytes,
// and sends it via the Pulsar producer with a context deadline derived from opts.Timeout.
// Returns HTTP-style (statusCode, respStatus, responseMessage) tuples following the
// common.StreamProducer contract.
func (p *PulsarProducer) Produce(jsonData json.RawMessage, _ any) (int, string, string) {
	// Guard against nil producer — this mirrors the nil-client guard in firehose/kinesis producers.
	if p.producer == nil {
		return 400, "Failure", "[Pulsar] error :: Could not create producer"
	}

	// Parse the incoming JSON payload and extract the "message" field.
	parsedJSON := gjson.ParseBytes(jsonData)
	data := parsedJSON.Get("message").Value()
	if data == nil {
		return 400, "Failure", "[Pulsar] error :: message from payload not found"
	}

	// Marshal the extracted message data to JSON bytes for the Pulsar producer message payload.
	value, err := jsonrs.Marshal(data)
	if err != nil {
		pkgLogger.Errorn("[Pulsar] error :: failed to marshal message payload", obskit.Error(err))
		return 400, "Failure", "[Pulsar] error :: " + err.Error()
	}

	// Construct the Pulsar producer message with the serialized payload.
	msg := &pulsarclient.ProducerMessage{
		Payload: value,
	}

	// Create a context with timeout if opts.Timeout is configured, otherwise use background context.
	ctx := context.Background()
	if p.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.opts.Timeout)
		defer cancel()
	}

	// Send the message to the Pulsar topic. On success, return the message ID in the response.
	msgID, err := p.producer.Send(ctx, msg)
	if err != nil {
		pkgLogger.Errorn("[Pulsar] error :: failed to send message", obskit.Error(err))
		return 500, "Failure", "[Pulsar] error :: " + err.Error()
	}

	return 200, "Success", fmt.Sprintf("Message delivered to Pulsar topic with ID: %v", msgID)
}

// Close closes the Pulsar producer and client, releasing all associated resources.
// It safely handles nil producer and client to allow calling Close on partially-initialized
// instances. Returns nil following the pattern established by other stream producers.
func (p *PulsarProducer) Close() error {
	if p.producer != nil {
		p.producer.Close()
	}
	if p.client != nil {
		p.client.Close()
	}
	return nil
}
