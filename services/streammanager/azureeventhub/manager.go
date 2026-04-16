//go:generate mockgen -destination=../../../mocks/services/streammanager/azureeventhub/mock_azureeventhub.go -package mock_azureeventhub github.com/rudderlabs/rudder-server/services/streammanager/azureeventhub AzureEventHubClient

package azureeventhub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
)

var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("streammanager").Child("azureeventhub")
}

// AzureEventHubClient is the minimal interface for Azure Event Hub operations.
// It abstracts the send operation for testability via mock implementations.
type AzureEventHubClient interface {
	// SendEventDataBatch sends a batch of event data to Azure Event Hub
	// with an optional partition key for enhanced partition routing.
	SendEventDataBatch(ctx context.Context, batch []byte, partitionKey string) error
}

// AzureEventHubProducer implements common.StreamProducer for Azure Event Hub.
// It provides enhanced partition support beyond the existing Kafka-based variant
// (AZURE_EVENT_HUB), using Azure Event Hub's native REST API with SAS token
// authentication and explicit partition key routing.
type AzureEventHubProducer struct {
	client       AzureEventHubClient
	namespace    string
	eventHubName string
}

// NewProducer creates an Azure Event Hub producer based on destination config.
//
// Required config fields extracted from destination.Config:
//   - connectionString: Azure Event Hub connection string containing endpoint, SAS key name, and SAS key
//   - eventHubNamespace: The Azure Event Hub namespace (e.g., "my-namespace")
//   - eventHubName: The specific Event Hub name within the namespace
//
// Returns error containing "Azure Event Hub" when validation fails, as expected
// by the factory test suite in streammanager_suite_test.go.
func NewProducer(destination *backendconfig.DestinationT, o common.Opts) (common.StreamProducer, error) {
	// Marshal destination config to JSON bytes for gjson parsing
	// Using jsonrs.Marshal per linter requirements (forbidigo: encoding/json Marshal forbidden)
	configBytes, err := jsonrs.Marshal(destination.Config)
	if err != nil {
		return nil, fmt.Errorf("[AzureEventHub] error :: failed to marshal Azure Event Hub destination config: %w", err)
	}
	parsedConfig := gjson.ParseBytes(configBytes)

	// Validate required connection string field
	connectionString := parsedConfig.Get("connectionString").String()
	if connectionString == "" {
		return nil, fmt.Errorf("[AzureEventHub] error :: Azure Event Hub connectionString is required")
	}

	// Validate required namespace field
	eventHubNamespace := parsedConfig.Get("eventHubNamespace").String()
	if eventHubNamespace == "" {
		return nil, fmt.Errorf("[AzureEventHub] error :: Azure Event Hub eventHubNamespace is required")
	}

	// Validate required event hub name field
	eventHubName := parsedConfig.Get("eventHubName").String()
	if eventHubName == "" {
		return nil, fmt.Errorf("[AzureEventHub] error :: Azure Event Hub eventHubName is required")
	}

	// Create the native Azure Event Hub HTTP client using connection string credentials
	client, err := newAzureEventHubHTTPClient(connectionString, eventHubNamespace, eventHubName, o.Timeout)
	if err != nil {
		return nil, fmt.Errorf("[AzureEventHub] error :: Azure Event Hub client creation failed: %w", err)
	}

	return &AzureEventHubProducer{
		client:       client,
		namespace:    eventHubNamespace,
		eventHubName: eventHubName,
	}, nil
}

// Produce sends data to Azure Event Hub.
// It extracts the "message" field from the JSON payload, serializes it,
// and sends it to the configured Event Hub. An optional "partitionKey" field
// enables enhanced partition routing — the key differentiating feature of
// this producer over the Kafka-based AZURE_EVENT_HUB variant.
//
// Return format follows the StreamProducer convention:
//   - (200, "Success", message) on successful delivery
//   - (400, "Failure", error) for client/validation errors
//   - (500, "Failure", error) for server/transport errors
func (producer *AzureEventHubProducer) Produce(jsonData json.RawMessage, _ any) (int, string, string) {
	// Nil client guard — matches the pattern in firehose, kinesis, and eventbridge producers
	client := producer.client
	if client == nil {
		return 400, "Failure", "[AzureEventHub] error :: Could not create producer for Azure Event Hub"
	}

	// Parse the JSON payload using gjson (matches firehose/kinesis pattern)
	parsedJSON := gjson.ParseBytes(jsonData)

	// Extract the message field from the payload
	data := parsedJSON.Get("message").Value()
	if data == nil {
		return 400, "Failure", "[AzureEventHub] error :: message from payload not found"
	}

	// Serialize the message data using jsonrs (linter-compliant JSON handling)
	value, err := jsonrs.Marshal(data)
	if err != nil {
		pkgLogger.Errorn("[AzureEventHub] error", obskit.Error(err))
		return 400, "Failure", "[AzureEventHub] error :: " + err.Error()
	}

	// Extract optional partition key for enhanced partition routing.
	// This is the key feature that differentiates AZURE_EVENT_HUB_EXTENDED
	// from the Kafka-based AZURE_EVENT_HUB variant, providing explicit
	// control over which Event Hub partition receives the event.
	partitionKey := parsedJSON.Get("partitionKey").String()

	// Send event data to Azure Event Hub via the client interface
	err = client.SendEventDataBatch(context.Background(), value, partitionKey)
	if err != nil {
		statusCode := 500
		respStatus := "Failure"
		responseMessage := err.Error()
		pkgLogger.Errorn("[AzureEventHub] error",
			logger.NewIntField("statusCode", int64(statusCode)),
			logger.NewStringField("respStatus", respStatus),
			logger.NewStringField("responseMessage", responseMessage))
		return statusCode, respStatus, responseMessage
	}

	// Format success message including partition key info when available
	message := fmt.Sprintf("Message delivered to Azure Event Hub %s", producer.eventHubName)
	if partitionKey != "" {
		message += fmt.Sprintf(" with partitionKey: %s", partitionKey)
	}
	return 200, "Success", message
}

// Close cleans up the Azure Event Hub producer resources.
// Returns nil since the HTTP-based client does not require explicit cleanup
// (matches the no-op close pattern used by eventbridge, firehose, and kinesis producers).
func (*AzureEventHubProducer) Close() error {
	return nil
}

// ---------------------------------------------------------------------------
// Internal concrete client implementation using Azure Event Hub REST API
// ---------------------------------------------------------------------------

// azureEventHubHTTPClient implements AzureEventHubClient using Azure Event Hub's
// native REST API with SAS token authentication. This avoids requiring the Azure
// SDK as a dependency while providing full Event Hub functionality including
// explicit partition key routing via the BrokerProperties header.
type azureEventHubHTTPClient struct {
	// endpoint is the HTTPS URL for sending messages:
	// https://<namespace>.servicebus.windows.net/<eventHubName>
	endpoint string
	// sasKeyName is the SharedAccessKeyName from the connection string
	sasKeyName string
	// sasKey is the SharedAccessKey from the connection string (base64-encoded)
	sasKey string
	// httpClient is the HTTP client used for sending requests with a configured timeout
	httpClient *http.Client
}

// newAzureEventHubHTTPClient creates a new HTTP-based Azure Event Hub client by
// parsing the connection string and constructing the REST API endpoint.
//
// Connection string format:
//
//	Endpoint=sb://<namespace>.servicebus.windows.net/;SharedAccessKeyName=<name>;SharedAccessKey=<key>[;EntityPath=<hub>]
func newAzureEventHubHTTPClient(connectionString, namespace, eventHubName string, timeout time.Duration) (*azureEventHubHTTPClient, error) {
	props, err := parseConnectionString(connectionString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	sasKeyName := props["SharedAccessKeyName"]
	if sasKeyName == "" {
		return nil, fmt.Errorf("SharedAccessKeyName not found in connection string")
	}

	sasKey := props["SharedAccessKey"]
	if sasKey == "" {
		return nil, fmt.Errorf("SharedAccessKey not found in connection string")
	}

	endpoint := fmt.Sprintf("https://%s.servicebus.windows.net/%s", namespace, eventHubName)

	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &azureEventHubHTTPClient{
		endpoint:   endpoint,
		sasKeyName: sasKeyName,
		sasKey:     sasKey,
		httpClient: &http.Client{Timeout: timeout},
	}, nil
}

// SendEventDataBatch sends event data to Azure Event Hub via the REST API.
//
// Azure Event Hub REST API endpoint:
//
//	POST https://<namespace>.servicebus.windows.net/<eventHubName>/messages
//
// Partition key routing is achieved via the BrokerProperties header:
//
//	BrokerProperties: {"PartitionKey": "<key>"}
//
// Authentication uses a Shared Access Signature (SAS) token generated from
// the connection string credentials.
func (c *azureEventHubHTTPClient) SendEventDataBatch(ctx context.Context, batch []byte, partitionKey string) error {
	sendURL := c.endpoint + "/messages"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sendURL, bytes.NewReader(batch))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Generate SAS token for authentication
	token, err := generateSASToken(c.endpoint, c.sasKeyName, c.sasKey)
	if err != nil {
		return fmt.Errorf("failed to generate SAS token: %w", err)
	}

	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	// Set partition key via BrokerProperties header for enhanced partition routing.
	// This is the native Azure Event Hub mechanism for controlling partition assignment,
	// providing more explicit control than the Kafka protocol variant.
	//
	// SECURITY: Use jsonrs.Marshal to construct the JSON header value instead of
	// fmt.Sprintf to prevent JSON injection — partitionKey is derived from
	// user-controlled event fields (userId) and may contain special characters
	// such as double quotes, backslashes, or Unicode escapes.
	if partitionKey != "" {
		brokerProps, marshalErr := jsonrs.Marshal(map[string]string{"PartitionKey": partitionKey})
		if marshalErr != nil {
			return fmt.Errorf("failed to marshal BrokerProperties: %w", marshalErr)
		}
		req.Header.Set("BrokerProperties", string(brokerProps))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send event data to Azure Event Hub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Azure Event Hub returns 201 Created on successful message send
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// Read error response body for diagnostic information
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("azure event hub returned status %d and failed to read response body: %w", resp.StatusCode, readErr)
	}
	return fmt.Errorf("azure event hub returned status %d: %s", resp.StatusCode, string(body))
}

// parseConnectionString parses an Azure Event Hub connection string into key-value pairs.
// The connection string uses semicolons as delimiters and equals signs as key-value separators.
// Only the first equals sign is used for splitting to correctly handle base64-encoded values
// (like SharedAccessKey) that may contain trailing '=' padding characters.
//
// Example connection string:
//
//	Endpoint=sb://mynamespace.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=base64key==
func parseConnectionString(connStr string) (map[string]string, error) {
	props := make(map[string]string)
	parts := strings.Split(connStr, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx < 0 {
			continue
		}
		key := part[:idx]
		value := part[idx+1:]
		props[key] = value
	}
	if len(props) == 0 {
		return nil, fmt.Errorf("empty or invalid connection string")
	}
	return props, nil
}

// generateSASToken generates a Shared Access Signature (SAS) token for Azure Event Hub REST API
// authentication. The token is valid for 1 hour from the time of generation.
//
// Token format:
//
//	SharedAccessSignature sr=<encoded-resource-uri>&sig=<signature>&se=<expiry>&skn=<key-name>
//
// The signature is computed as:
//
//	HMAC-SHA256(base64-decoded-key, url-encoded-lowercase-resource-uri + "\n" + expiry)
func generateSASToken(resourceURI, keyName, key string) (string, error) {
	encodedURI := url.QueryEscape(strings.ToLower(resourceURI))
	expiry := time.Now().Add(1 * time.Hour).Unix()
	stringToSign := fmt.Sprintf("%s\n%d", encodedURI, expiry)

	decodedKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return "", fmt.Errorf("failed to decode SAS key: %w", err)
	}

	mac := hmac.New(sha256.New, decodedKey)
	_, _ = mac.Write([]byte(stringToSign))
	signature := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	token := fmt.Sprintf("SharedAccessSignature sr=%s&sig=%s&se=%d&skn=%s",
		encodedURI, signature, expiry, keyName)
	return token, nil
}
