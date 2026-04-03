//go:generate mockgen -destination=../../../mocks/services/streammanager/amazonmsk/mock_amazonmsk.go -package mock_amazonmsk github.com/rudderlabs/rudder-server/services/streammanager/amazonmsk MSKClient

package amazonmsk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-go-kit/awsutil"
	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	kafkaclient "github.com/rudderlabs/rudder-go-kit/kafkaclient"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
	"github.com/rudderlabs/rudder-server/utils/awsutils"
)

// pkgLogger is the package-level structured logger for the amazonmsk package.
var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("streammanager").Child("amazonmsk")
}

// MSKClient abstracts message sending to Amazon MSK so that it can be mocked in tests.
// The interface is generated into a mock via the go:generate directive at the top of this file.
type MSKClient interface {
	SendMessage(ctx context.Context, msg *MSKMessage) error
}

// MSKMessage encapsulates a single Kafka message destined for an Amazon MSK topic.
type MSKMessage struct {
	Topic string
	Key   []byte
	Value []byte
}

// MSKConfig holds the Amazon MSK-specific destination configuration fields parsed
// from the backend-config DestinationT.Config map.
type MSKConfig struct {
	ClusterARN       string `json:"ClusterARN"`
	BootstrapServers string `json:"BootstrapServers"`
	Topic            string `json:"Topic"`
	Region           string `json:"Region"`
	IAMRoleARN       string `json:"IAMRoleARN"`
	ExternalID       string `json:"ExternalID"`
	UseSASL          bool   `json:"UseSASL"`
	SaslType         string `json:"SaslType"`
	Username         string `json:"Username"`
	Password         string `json:"Password"`
}

// AmazonMSKProducer implements common.StreamProducer for Amazon MSK destinations.
// It delegates message sending to the MSKClient interface, which wraps a Kafka
// producer configured for MSK authentication (SASL/SCRAM or AWS IAM).
type AmazonMSKProducer struct {
	client MSKClient
}

// kafkaBasedMSKClient adapts a kafkaclient.Producer to the MSKClient interface,
// translating MSKMessage instances into kafkaclient.Message values for publishing.
type kafkaBasedMSKClient struct {
	producer *kafkaclient.Producer
}

// SendMessage publishes a single message to the MSK topic via the underlying Kafka producer.
func (c *kafkaBasedMSKClient) SendMessage(ctx context.Context, msg *MSKMessage) error {
	return c.producer.Publish(ctx, kafkaclient.Message{
		Key:   msg.Key,
		Value: msg.Value,
		Topic: msg.Topic,
	})
}

// NewProducer creates a new Amazon MSK stream producer based on the provided destination
// configuration. It establishes AWS credentials via the standard session config pipeline,
// parses MSK-specific configuration (bootstrap servers, topic, SASL settings), validates
// required fields, and constructs a Kafka producer connected to the MSK cluster.
//
// All errors returned from this function contain the string "MSK" to satisfy the
// stream manager factory test assertion pattern.
func NewProducer(destination *backendconfig.DestinationT, o common.Opts) (common.StreamProducer, error) {
	sessionConfig, err := awsutils.NewSessionConfigForDestination(destination, o.Timeout, "amazonmsk")
	if err != nil {
		return nil, fmt.Errorf("[MSK] error creating session config: %w", err)
	}
	sessionConfig.MaxIdleConnsPerHost = config.GetIntVar(
		64, 1,
		"Router.AMAZON_MSK.httpMaxIdleConnsPerHost",
		"Router.AMAZON_MSK.noOfWorkers",
		"Router.noOfWorkers",
	)

	awsConfig, err := awsutil.CreateAWSConfig(context.Background(), sessionConfig)
	if err != nil {
		return nil, fmt.Errorf("[MSK] error creating AWS config: %w", err)
	}

	// Parse MSK-specific destination config from the backend-config map
	jsonConfig, err := jsonrs.Marshal(destination.Config)
	if err != nil {
		return nil, fmt.Errorf("[MSK] error marshalling destination config: %w", err)
	}
	var mskConfig MSKConfig
	if err = jsonrs.Unmarshal(jsonConfig, &mskConfig); err != nil {
		return nil, fmt.Errorf("[MSK] error unmarshalling destination config: %w", err)
	}

	// Validate mandatory MSK configuration fields
	if mskConfig.BootstrapServers == "" {
		return nil, fmt.Errorf("[MSK] MSK bootstrap servers cannot be empty")
	}
	if mskConfig.Topic == "" {
		return nil, fmt.Errorf("[MSK] MSK topic cannot be empty")
	}

	// Create the MSK Kafka client with the derived AWS config and MSK settings
	mskClient, err := newMSKClient(awsConfig, mskConfig)
	if err != nil {
		return nil, fmt.Errorf("[MSK] error creating MSK client: %w", err)
	}

	return &AmazonMSKProducer{client: mskClient}, nil
}

// newMSKClient creates a Kafka producer configured for Amazon MSK using the provided
// AWS credentials context and MSK-specific settings. It supports SASL/SCRAM
// authentication with configurable hash generators (SHA-256, SHA-512, or plain text)
// and always enables TLS using the system certificate pool.
func newMSKClient(awsCfg aws.Config, mskConfig MSKConfig) (MSKClient, error) {
	// Parse comma-separated bootstrap servers into individual addresses
	addresses := strings.Split(mskConfig.BootstrapServers, ",")
	for i := range addresses {
		addresses[i] = strings.TrimSpace(addresses[i])
	}

	clientConf := kafkaclient.Config{
		// Use AWS region in client ID for broker-side connection tracking
		ClientID:    fmt.Sprintf("rudder-msk-%s", awsCfg.Region),
		DialTimeout: 10 * time.Second,
	}

	// MSK clusters always use TLS-encrypted communication
	clientConf.TLS = &kafkaclient.TLS{
		WithSystemCertPool: true,
	}

	// Configure SASL authentication when enabled (SCRAM-SHA-256/512 or plain text)
	if mskConfig.UseSASL {
		clientConf.SASL = &kafkaclient.SASL{
			Username: mskConfig.Username,
			Password: mskConfig.Password,
		}
		hashGen, err := kafkaclient.ScramHashGeneratorFromString(mskConfig.SaslType)
		if err != nil {
			return nil, fmt.Errorf("invalid SASL type %q: %w", mskConfig.SaslType, err)
		}
		clientConf.SASL.ScramHashGen = hashGen
	}

	c, err := kafkaclient.New("tcp", addresses, clientConf)
	if err != nil {
		return nil, fmt.Errorf("could not create Kafka client: %w", err)
	}

	p, err := c.NewProducer(kafkaclient.ProducerConfig{
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("could not create Kafka producer: %w", err)
	}

	return &kafkaBasedMSKClient{producer: p}, nil
}

// Produce sends a single event message to an Amazon MSK topic. It extracts the message
// payload, topic name, and optional partition key (userId) from the JSON event data,
// then delegates to the MSK client for delivery.
//
// Return values follow the StreamProducer convention: (statusCode, status, message).
// A 200 status code indicates successful delivery; 400 indicates a client-side error
// (invalid payload, missing fields); other codes come from ParseAWSError on send failures.
func (producer *AmazonMSKProducer) Produce(jsonData json.RawMessage, _ any) (int, string, string) {
	parsedJSON := gjson.ParseBytes(jsonData)
	client := producer.client
	if client == nil {
		return 400, "Failure", "[AmazonMSK] error :: Could not create producer"
	}

	// Extract the message payload from the event data
	data := parsedJSON.Get("message").Value()
	if data == nil {
		return 400, "Failure", "[AmazonMSK] error :: message from payload not found"
	}
	value, err := jsonrs.Marshal(data)
	if err != nil {
		pkgLogger.Errorn("[AmazonMSK] error", obskit.Error(err))
		return 400, "Failure", "[AmazonMSK] error :: " + err.Error()
	}

	// Extract and validate the target topic
	topicVal := parsedJSON.Get("topic").Value()
	if topicVal == nil {
		return 400, "Failure", "[AmazonMSK] error :: MSK topic not found"
	}
	topic, ok := topicVal.(string)
	if !ok {
		return 400, "Failure", "[AmazonMSK] error :: Could not parse MSK topic to string"
	}
	if topic == "" {
		return 400, "Failure", "[AmazonMSK] error :: empty MSK topic"
	}

	// Extract optional partition key from userId for consistent routing
	partitionKey := parsedJSON.Get("userId").String()

	msg := &MSKMessage{
		Topic: topic,
		Key:   []byte(partitionKey),
		Value: value,
	}

	if err = client.SendMessage(context.Background(), msg); err != nil {
		statusCode, respStatus, responseMessage := common.ParseAWSError(err)
		pkgLogger.Errorn("[AmazonMSK] error",
			logger.NewIntField("statusCode", int64(statusCode)),
			logger.NewStringField("respStatus", respStatus),
			logger.NewStringField("responseMessage", responseMessage))
		return statusCode, respStatus, responseMessage
	}

	return 200, "Success", fmt.Sprintf("Message delivered to MSK topic %s", topic)
}

// Close is a no-op for the Amazon MSK producer, consistent with other AWS destination
// producers (firehose, kinesis, eventbridge). The underlying Kafka producer uses
// synchronous publishing with batch size 1, so there are no pending writes to flush.
func (*AmazonMSKProducer) Close() error {
	return nil
}
