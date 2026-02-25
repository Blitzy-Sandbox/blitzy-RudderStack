# Stream Destinations

Stream destinations deliver events in **real-time** to message queues, event buses, streaming platforms, and serverless compute services. Unlike cloud destinations that use RESTful HTTP delivery, stream destinations establish persistent producer connections to their target systems, enabling high-throughput, low-latency event delivery with protocol-native semantics.

RudderStack supports **13 stream destinations** managed by the `CustomManagerT` custom destination manager via the `services/streammanager/` package. Each stream destination is dispatched by the Router to a provider-specific producer that implements the common `StreamProducer` interface.

> **Source:** `router/customdestinationmanager/customdestinationmanager.go`

## Destination Type Classification

The Custom Destination Manager classifies managed destinations into two categories using type constants:

| Type Constant | Value | Description |
|---------------|-------|-------------|
| `STREAM` | `"stream"` | Stream destinations — 13 integrations delivering events via producer-based protocols (Kafka, AWS SDK, gRPC, HTTPS) |
| `KV` | `"kv"` | Key-value store destinations — 1 integration (Redis) delivering events via KV operations (HSET, HMSET) |

> **Source:** `router/customdestinationmanager/customdestinationmanager.go:25-27`

Stream destinations are the primary category. All 13 stream destinations are registered in the `ObjectStreamDestinations` slice:

```go
ObjectStreamDestinations = []string{
    "KINESIS", "KAFKA", "AZURE_EVENT_HUB", "FIREHOSE", "EVENTBRIDGE",
    "GOOGLEPUBSUB", "CONFLUENT_CLOUD", "PERSONALIZE", "GOOGLESHEETS",
    "BQSTREAM", "LAMBDA", "GOOGLE_CLOUD_FUNCTION", "WUNDERKIND",
}
```

> **Source:** `router/customdestinationmanager/customdestinationmanager.go:79`

## StreamProducer Interface

All stream destinations implement the common `StreamProducer` interface, which provides a uniform contract for event delivery and resource cleanup:

```go
type StreamProducer interface {
    io.Closer
    Produce(jsonData json.RawMessage, destConfig any) (int, string, string)
}
```

| Method | Parameters | Returns | Description |
|--------|-----------|---------|-------------|
| `Produce` | `jsonData json.RawMessage` — serialized event payload; `destConfig any` — destination configuration map | `(statusCode int, respStatus string, responseMessage string)` | Delivers a single event to the destination. Returns HTTP-style status code, status label, and response message. |
| `Close` | *(none)* | `error` | Releases producer resources (connections, goroutines, buffers). Called when the destination config changes or the manager shuts down. |

> **Source:** `services/streammanager/common/common.go:20-23`

## Event Delivery Flow

The following diagram shows how events flow from the Router through the Custom Destination Manager to stream destinations:

```mermaid
flowchart LR
    subgraph Router
        RW[Router Worker]
    end

    subgraph "Custom Destination Manager"
        CDM[CustomManagerT]
        CB[Circuit Breaker<br/>gobreaker]
        CACHE[Client Cache<br/>per destination ID]
    end

    subgraph "Stream Manager"
        SM["streammanager.NewProducer()"]
    end

    subgraph "Provider Producers"
        KAFKA[Kafka Producer]
        KINESIS[Kinesis Producer]
        PUBSUB[Pub/Sub Producer]
        EB[EventBridge Producer]
        FH[Firehose Producer]
        BQ[BigQuery Stream Producer]
        LAMBDA[Lambda Producer]
        GCF[Cloud Function Producer]
        GS[Google Sheets Producer]
        PERS[Personalize Producer]
        WK[Wunderkind Producer]
    end

    subgraph Destinations
        KAFKA_D[Apache Kafka]
        KINESIS_D[Amazon Kinesis]
        PUBSUB_D[Google Pub/Sub]
        EB_D[Amazon EventBridge]
        FH_D[Amazon Firehose]
        BQ_D[BigQuery]
        LAMBDA_D[AWS Lambda]
        GCF_D[Google Cloud Function]
        GS_D[Google Sheets]
        PERS_D[Amazon Personalize]
        WK_D[Wunderkind]
    end

    RW -->|SendData| CDM
    CDM --> CB
    CB --> CACHE
    CACHE -->|newClient| SM
    SM --> KAFKA & KINESIS & PUBSUB & EB & FH & BQ & LAMBDA & GCF & GS & PERS & WK

    KAFKA --> KAFKA_D
    KINESIS --> KINESIS_D
    PUBSUB --> PUBSUB_D
    EB --> EB_D
    FH --> FH_D
    BQ --> BQ_D
    LAMBDA --> LAMBDA_D
    GCF --> GCF_D
    GS --> GS_D
    PERS --> PERS_D
    WK --> WK_D
```

**Flow description:**

1. The **Router Worker** calls `CustomManagerT.SendData(jsonData, destID)` for each event targeted at a stream destination.
2. The **Custom Destination Manager** acquires a per-destination read lock and retrieves the cached producer client.
3. If no client exists, the manager creates one by calling `streammanager.NewProducer()` through the **circuit breaker** (`gobreaker.CircuitBreaker`).
4. The `NewProducer()` factory dispatches to the correct provider-specific constructor based on `destination.DestinationDefinition.Name`.
5. The **provider producer** delivers the event using its native protocol (Kafka protocol, AWS SDK, gRPC, HTTPS).
6. Status codes, response status, and messages are propagated back through the chain for retry/error handling.

> **Source:** `router/customdestinationmanager/customdestinationmanager.go:86-123` (newClient), `router/customdestinationmanager/customdestinationmanager.go:125-158` (send), `services/streammanager/streammanager.go:24-58` (NewProducer)

---

## Stream Destination Catalog

The following table summarizes all 13 stream destinations with their internal names, producer packages, authentication methods, and delivery protocols:

| # | Destination | Internal Name | Producer Constructor | Auth Method | Protocol |
|---|-------------|--------------|---------------------|-------------|----------|
| 1 | [Apache Kafka](#apache-kafka) | `KAFKA` | `kafka.NewProducer` | SASL / SSL / SSH tunnel | Kafka protocol (TCP) |
| 2 | [Amazon Kinesis](#amazon-kinesis) | `KINESIS` | `kinesis.NewProducer` | AWS IAM (access key / role) | AWS SDK v2 |
| 3 | [Google Pub/Sub](#google-pubsub) | `GOOGLEPUBSUB` | `googlepubsub.NewProducer` | GCP Service Account JSON | gRPC |
| 4 | [Amazon EventBridge](#amazon-eventbridge) | `EVENTBRIDGE` | `eventbridge.NewProducer` | AWS IAM (access key / role) | AWS SDK v2 |
| 5 | [Amazon Firehose](#amazon-firehose) | `FIREHOSE` | `firehose.NewProducer` | AWS IAM (access key / role) | AWS SDK v2 |
| 6 | [Azure Event Hub](#azure-event-hub) | `AZURE_EVENT_HUB` | `kafka.NewProducerForAzureEventHubs` | Kafka-compatible (SASL) | Kafka protocol (TCP) |
| 7 | [Confluent Cloud](#confluent-cloud) | `CONFLUENT_CLOUD` | `kafka.NewProducerForConfluentCloud` | Kafka-compatible (SASL/SSL) | Kafka protocol (TCP) |
| 8 | [BigQuery Stream](#bigquery-stream) | `BQSTREAM` | `bqstream.NewProducer` | GCP Service Account JSON | BigQuery Streaming API |
| 9 | [AWS Lambda](#aws-lambda) | `LAMBDA` | `lambda.NewProducer` | AWS IAM (access key / role) | AWS SDK v2 |
| 10 | [Google Cloud Function](#google-cloud-function) | `GOOGLE_CLOUD_FUNCTION` | `googlecloudfunction.NewProducer` | GCP Service Account JSON + ID Token | HTTPS |
| 11 | [Google Sheets](#google-sheets) | `GOOGLESHEETS` | `googlesheets.NewProducer` | Google OAuth 2.0 | Google Sheets API v4 |
| 12 | [Amazon Personalize](#amazon-personalize) | `PERSONALIZE` | `personalize.NewProducer` | AWS IAM (access key / role) | AWS SDK v2 |
| 13 | [Wunderkind](#wunderkind) | `WUNDERKIND` | `wunderkind.NewProducer` | AWS IAM (role-based, server-side config) | AWS Lambda Invoke |

> **Source:** `services/streammanager/streammanager.go:24-58`

> **Note:** Azure Event Hub and Confluent Cloud share the Apache Kafka producer package with specialized constructors. See [Azure Event Hub](#azure-event-hub) and [Confluent Cloud](#confluent-cloud) for details.

---

## Per-Destination Configuration Guides

### Apache Kafka

Apache Kafka is the most fully-featured stream destination, supporting SSL/TLS encryption, SASL authentication, AVRO serialization with schema registry integration, SSH tunneling, and per-topic routing.

> **Source:** `services/streammanager/kafka/kafkamanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `Topic` | `string` | Yes | Default Kafka topic to publish messages to. Can be overridden per-event via the `topic` field in the event payload. |
| `HostName` | `string` | Yes | Comma-separated list of Kafka broker hostnames. |
| `Port` | `string` | Yes | Kafka broker port (typically `9092` for plaintext, `9093` for SSL). |
| `SslEnabled` | `bool` | No | Enable SSL/TLS encryption for the Kafka connection. |
| `CACertificate` | `string` | No | PEM-encoded CA certificate for SSL. If empty and SSL is enabled, the system certificate pool is used. |
| `UseSASL` | `bool` | No | Enable SASL authentication. Only available when `SslEnabled` is `true`. |
| `SaslType` | `string` | No | SASL mechanism type. Supported: `plain`, `scram-sha-256`, `scram-sha-512`. |
| `Username` | `string` | No | SASL username for authentication. |
| `Password` | `string` | No | SASL password for authentication. |
| `ConvertToAvro` | `bool` | No | Enable AVRO serialization for messages. Requires `AvroSchemas` to be configured. |
| `EmbedAvroSchemaID` | `bool` | No | Embed the AVRO schema ID in the Confluent wire format header (magic byte + 4-byte schema ID). |
| `AvroSchemas` | `array` | No | Array of `{SchemaId, Schema}` objects defining AVRO schemas for serialization. Each schema must have a non-empty `SchemaId`. |
| `UseSSH` | `bool` | No | Enable SSH tunnel to reach the Kafka broker. |
| `SSHHost` | `string` | No | SSH bastion host address. Required when `UseSSH` is `true`. |
| `SSHPort` | `string` | No | SSH bastion port. Required when `UseSSH` is `true`. |
| `SSHUser` | `string` | No | SSH username. Required when `UseSSH` is `true`. |

> **Source:** `services/streammanager/kafka/kafkamanager.go:38-58`

#### Configuration Example

```json
{
  "Topic": "analytics-events",
  "HostName": "kafka-broker-1.example.com,kafka-broker-2.example.com",
  "Port": "9093",
  "SslEnabled": true,
  "CACertificate": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----",
  "UseSASL": true,
  "SaslType": "scram-sha-512",
  "Username": "rudderstack-producer",
  "Password": "your-sasl-password",
  "ConvertToAvro": false,
  "EmbedAvroSchemaID": false,
  "AvroSchemas": [],
  "UseSSH": false
}
```

#### Key Behaviors

- **Per-event topic routing:** Events can override the default topic by including a `topic` field in the JSON payload. If not present, the configured `Topic` is used.
- **AVRO serialization:** When enabled, the producer uses the `goavro` library to serialize messages. The `schemaId` field in the event payload selects which AVRO schema to apply. If `EmbedAvroSchemaID` is true, the Confluent wire format header (magic byte `0x00` + 4-byte big-endian schema ID) is prepended.
- **Message key:** The Kafka message key is set to the `userId` field from the event payload for consistent partition routing.
- **Dial timeout:** Configurable via `Router.KAFKA.dialTimeout` (default: 10 seconds).
- **Publish timeout:** Default 10 seconds, configurable via the router timeout setting.
- **SSL client certificates:** Optional client cert/key can be provided via `KAFKA_SSL_CERTIFICATE_FILE_PATH` and `KAFKA_SSL_KEY_FILE_PATH` environment variables.

> **Source:** `services/streammanager/kafka/kafkamanager.go:232-341` (NewProducer), `services/streammanager/kafka/kafkamanager.go:528-555` (Produce), `services/streammanager/kafka/kafkamanager.go:557-605` (sendMessage)

#### Kafka-Compatible Variants

Azure Event Hub and Confluent Cloud share the Kafka producer with specialized constructors:

- **Azure Event Hub:** `kafka.NewProducerForAzureEventHubs` — uses Event Hubs connection string for SASL authentication
- **Confluent Cloud:** `kafka.NewProducerForConfluentCloud` — uses Confluent Cloud API key/secret for SASL/SSL authentication

> **Source:** `services/streammanager/streammanager.go:29-32`

---

### Amazon Kinesis

Amazon Kinesis delivers events to Kinesis Data Streams using the AWS SDK v2 `PutRecord` API. Each event is delivered as a single record with configurable partition key semantics.

> **Source:** `services/streammanager/kinesis/kinesismanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `Stream` | `string` | Yes | Name of the Kinesis stream to write records to. Extracted from the destination config at produce time. |
| `UseMessageID` | `bool` | No | When `true`, uses `message.messageId` as the partition key instead of `userId`. Falls back to `userId` if `messageId` is empty. |
| `region` | `string` | Yes | AWS region where the Kinesis stream is hosted (e.g., `us-east-1`). Provided via AWS session config. |
| `accessKeyID` | `string` | Conditional | AWS access key ID. Required unless using IAM role-based auth. |
| `accessKey` | `string` | Conditional | AWS secret access key. Required unless using IAM role-based auth. |
| `roleARN` | `string` | Conditional | IAM role ARN for cross-account or role-based authentication. |

> **Source:** `services/streammanager/kinesis/kinesismanager.go:32-44`, `services/streammanager/kinesis/kinesismanager_utils.go:8-11`

#### Configuration Example

```json
{
  "region": "us-east-1",
  "accessKeyID": "AKIAIOSFODNN7EXAMPLE",
  "accessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  "stream": "rudderstack-events",
  "useMessageId": false
}
```

#### Key Behaviors

- **Single-record delivery:** Uses the `PutRecord` API (not `PutRecords` batch API) for each event.
- **Partition key:** Defaults to `userId` from the event payload. When `UseMessageID` is `true`, `message.messageId` is used first, falling back to `userId` if empty.
- **Throughput error handling:** `ProvisionedThroughputExceededException` is mapped to HTTP 429, triggering the Router's retry mechanism with backoff.
- **Max idle connections:** Configurable via `Router.KINESIS.httpMaxIdleConnsPerHost` (default: 64, falls back to `Router.KINESIS.noOfWorkers` then `Router.noOfWorkers`).

> **Source:** `services/streammanager/kinesis/kinesismanager.go:38` (httpMaxIdleConnsPerHost), `services/streammanager/kinesis/kinesismanager.go:47-53` (error mapping)

---

### Google Pub/Sub

Google Pub/Sub delivers events to Google Cloud Pub/Sub topics using the `cloud.google.com/go/pubsub/v2` client library over gRPC. It supports event-to-topic mapping, allowing different event types to be routed to different topics.

> **Source:** `services/streammanager/googlepubsub/googlepubsubmanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `Credentials` | `string` | Yes | GCP Service Account JSON credentials string for authentication. |
| `ProjectId` | `string` | Yes | Google Cloud project ID containing the Pub/Sub topics. |
| `EventToTopicMap` | `array` | Yes | Array of `{"from": "<event_type>", "to": "<topic_name>"}` mappings that route specific event types to designated Pub/Sub topics. |

> **Source:** `services/streammanager/googlepubsub/googlepubsubmanager.go:32-37`

#### Configuration Example

```json
{
  "credentials": "{\"type\":\"service_account\",\"project_id\":\"my-project\",...}",
  "projectId": "my-gcp-project",
  "eventToTopicMap": [
    {"from": "track", "to": "analytics-track-events"},
    {"from": "identify", "to": "analytics-identify-events"},
    {"from": "*", "to": "analytics-all-events"}
  ]
}
```

#### Key Behaviors

- **Event-to-topic routing:** The `EventToTopicMap` allows fine-grained routing of different event types to separate Pub/Sub topics. The `topicId` field in the event payload determines which topic receives the message.
- **Publisher tuning:** Per-topic publishers are configured with:
  - `NumGoroutines`: 25 (config key: `Router.GOOGLEPUBSUB.Client.NumGoroutines`)
  - `DelayThreshold`: 10ms (config key: `Router.GOOGLEPUBSUB.Client.DelayThreshold`)
  - `CountThreshold`: 64 (config key: `Router.GOOGLEPUBSUB.Client.CountThreshold`)
  - `ByteThreshold`: 10MB (config key: `Router.GOOGLEPUBSUB.Client.ByteThreshold`)
  - `MaxOutstandingMessages`: 1000 (config key: `Router.GOOGLEPUBSUB.Client.MaxOutstandingMessages`)
  - `MaxOutstandingBytes`: -1 (unlimited) (config key: `Router.GOOGLEPUBSUB.Client.MaxOutstandingBytes`)
  - `FlowControl`: `FlowControlBlock` (blocks when limits are reached)
- **Message attributes:** Events can include an `attributes` map in the payload, which are attached to the Pub/Sub message as key-value string attributes.
- **Retry with backoff:** Optional retry for transient authentication errors, configurable via:
  - `Router.GOOGLEPUBSUB.Client.EnableRetries` (default: `false`)
  - `Router.GOOGLEPUBSUB.Client.Retry.InitialInterval` (default: 10ms)
  - `Router.GOOGLEPUBSUB.Client.Retry.MaxInterval` (default: 500ms)
  - `Router.GOOGLEPUBSUB.Client.Retry.MaxElapsedTime` (default: 2s)
  - `Router.GOOGLEPUBSUB.Client.Retry.MaxRetries` (default: 3)
- **gRPC error mapping:** gRPC status codes are mapped to HTTP-style codes (e.g., `ResourceExhausted` → 429, `Unavailable` → 503, `PermissionDenied` → 403, `DeadlineExceeded` → 504).

> **Source:** `services/streammanager/googlepubsub/googlepubsubmanager.go:67-126` (NewProducer), `services/streammanager/googlepubsub/googlepubsubmanager.go:171-263` (Produce), `services/streammanager/googlepubsub/googlepubsubmanager.go:282-314` (error mapping)

---

### Amazon EventBridge

Amazon EventBridge delivers events to an EventBridge event bus using the AWS SDK v2 `PutEvents` API. Events are sent as `PutEventsRequestEntry` objects, enabling rule-based event routing on the AWS side.

> **Source:** `services/streammanager/eventbridge/eventbridgemanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | `string` | Yes | AWS region for the EventBridge event bus. Provided via AWS session config. |
| `accessKeyID` | `string` | Conditional | AWS access key ID. Required unless using IAM role-based auth. |
| `accessKey` | `string` | Conditional | AWS secret access key. Required unless using IAM role-based auth. |
| `roleARN` | `string` | Conditional | IAM role ARN for cross-account authentication. |
| `Detail` | `string` | Yes | JSON string containing the event detail (from event payload). |
| `DetailType` | `string` | Yes | Event detail type for EventBridge rule matching. |
| `Source` | `string` | Yes | Source identifier for EventBridge rule matching. |

> **Source:** `services/streammanager/eventbridge/eventbridgemanager.go:31-41`

#### Configuration Example

```json
{
  "region": "us-east-1",
  "accessKeyID": "AKIAIOSFODNN7EXAMPLE",
  "accessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
}
```

#### Key Behaviors

- **Single-event delivery:** Events are sent one at a time via `PutEvents` with a single `PutEventsRequestEntry`.
- **Required fields:** The event payload must include `Detail`, `DetailType`, and `Source` fields. Missing required fields result in entry-level errors (not API-level errors) — `PutEvents` succeeds but the entry contains `ErrorCode` and `ErrorMessage`.
- **Max idle connections:** Configurable via `Router.EVENTBRIDGE.httpMaxIdleConnsPerHost` (default: 64).
- **Error handling:** Uses `common.ParseAWSError` for API-level errors. Entry-level errors (missing required fields) are checked via `outputEntry.ErrorCode` and `outputEntry.ErrorMessage`.

> **Source:** `services/streammanager/eventbridge/eventbridgemanager.go:31-41` (NewProducer), `services/streammanager/eventbridge/eventbridgemanager.go:45-98` (Produce)

---

### Amazon Firehose

Amazon Firehose (now Amazon Data Firehose) delivers events to a Firehose delivery stream using the AWS SDK v2 `PutRecord` API. Firehose can deliver data to downstream destinations including Amazon S3, Amazon Redshift, Amazon OpenSearch, Splunk, and custom HTTP endpoints.

> **Source:** `services/streammanager/firehose/firehosemanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | `string` | Yes | AWS region for the Firehose delivery stream. Provided via AWS session config. |
| `accessKeyID` | `string` | Conditional | AWS access key ID. Required unless using IAM role-based auth. |
| `accessKey` | `string` | Conditional | AWS secret access key. Required unless using IAM role-based auth. |
| `roleARN` | `string` | Conditional | IAM role ARN for cross-account authentication. |
| `deliveryStreamMapTo` | `string` | Yes | Delivery stream name. Extracted from the event payload at produce time (not from destination config). |

> **Source:** `services/streammanager/firehose/firehosemanager.go:34-44`

#### Configuration Example

```json
{
  "region": "us-east-1",
  "accessKeyID": "AKIAIOSFODNN7EXAMPLE",
  "accessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
}
```

#### Key Behaviors

- **Single-record delivery:** Uses the `PutRecord` API with each event's `message` field serialized as the record data.
- **Delivery stream mapping:** The target delivery stream name is extracted from the `deliveryStreamMapTo` field in the event payload (set by the Transformer).
- **Max idle connections:** Configurable via `Router.FIREHOSE.httpMaxIdleConnsPerHost` (default: 64).
- **Downstream destinations:** Firehose can fan out to S3, Redshift, OpenSearch, Splunk, and HTTP endpoints — configured on the AWS console, not in RudderStack.

> **Source:** `services/streammanager/firehose/firehosemanager.go:34-44` (NewProducer), `services/streammanager/firehose/firehosemanager.go:48-93` (Produce)

---

### Azure Event Hub

Azure Event Hub uses the **Kafka-compatible protocol** by leveraging the shared Kafka producer with a specialized constructor. Azure Event Hubs provides a Kafka surface at the Standard tier and above, enabling RudderStack to produce events using the same Kafka client library.

> **Source:** `services/streammanager/streammanager.go:29-30`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `Topic` | `string` | Yes | Name of the Event Hub (not the Event Hubs Namespace). |
| `BootstrapServer` | `string` | Yes | Event Hub bootstrap server in `host:port` format (port is typically `9093`). |
| `EventHubsConnectionString` | `string` | Yes | Connection string starting with `Endpoint=sb://` containing the `SharedAccessKey`. |

> **Source:** `services/streammanager/kafka/kafkamanager.go:86-93` (azureEventHubConfig)

#### Configuration Example

```json
{
  "Topic": "analytics-events",
  "BootstrapServer": "my-namespace.servicebus.windows.net:9093",
  "EventHubsConnectionString": "Endpoint=sb://my-namespace.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=your-key"
}
```

#### Key Behaviors

- **Kafka-compatible protocol:** Uses the same Kafka producer internally, with SASL PLAIN authentication derived from the Event Hubs connection string.
- **Minimum tier:** Standard tier or above is required, as the Basic tier does not support the Kafka protocol surface.
- **Dial timeout:** Configurable via `Router.AZURE_EVENT_HUB.dialTimeout` (default: 10 seconds, falls back to `Router.kafkaDialTimeout`).
- **Validation:** `Topic`, `BootstrapServer`, and `EventHubsConnectionString` are all required and validated before the producer is created.

> **Source:** `services/streammanager/kafka/kafkamanager.go:343-391` (NewProducerForAzureEventHubs)

---

### Confluent Cloud

Confluent Cloud uses the **Kafka-compatible protocol** with API key/secret authentication via SASL/SSL. It leverages the shared Kafka producer with a specialized constructor that configures Confluent Cloud's authentication mechanism.

> **Source:** `services/streammanager/streammanager.go:31-32`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `Topic` | `string` | Yes | Confluent Cloud topic name. |
| `BootstrapServer` | `string` | Yes | Confluent Cloud bootstrap server in `host:port` format. |
| `APIKey` | `string` | Yes | Confluent Cloud API key for SASL authentication. |
| `APISecret` | `string` | Yes | Confluent Cloud API secret for SASL authentication. |

> **Source:** `services/streammanager/kafka/kafkamanager.go:109-114` (confluentCloudConfig)

#### Configuration Example

```json
{
  "Topic": "analytics-events",
  "BootstrapServer": "pkc-xxxxx.us-east-1.aws.confluent.cloud:9092",
  "APIKey": "your-confluent-api-key",
  "APISecret": "your-confluent-api-secret"
}
```

#### Key Behaviors

- **SASL/SSL authentication:** Uses Confluent Cloud's API key and secret for SASL PLAIN over SSL/TLS.
- **Dial timeout:** Configurable via `Router.CONFLUENT_CLOUD.dialTimeout` (default: 10 seconds, falls back to `Router.kafkaDialTimeout`).
- **Validation:** All four parameters (`Topic`, `BootstrapServer`, `APIKey`, `APISecret`) are required and validated before the producer is created.

> **Source:** `services/streammanager/kafka/kafkamanager.go:393-443` (NewProducerForConfluentCloud)

---

### BigQuery Stream

BigQuery Stream provides **real-time streaming insertion** into Google BigQuery tables using the BigQuery Streaming API (`tabledata.insertAll`). Events are inserted as generic records that map directly to BigQuery table columns.

> **Source:** `services/streammanager/bqstream/bqstreammanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `credentials` | `string` | Yes | GCP Service Account JSON credentials with `bigquery.insertdata` scope. |
| `projectId` | `string` | Yes | Google Cloud project ID containing the BigQuery dataset. |
| `datasetId` | `string` | Yes | BigQuery dataset ID. Extracted from the event payload at produce time. |
| `tableId` | `string` | Yes | BigQuery table ID. Extracted from the event payload at produce time. |

> **Source:** `services/streammanager/bqstream/bqstreammanager.go:26-31`

#### Configuration Example

```json
{
  "credentials": "{\"type\":\"service_account\",\"project_id\":\"my-project\",...}",
  "projectId": "my-gcp-project",
  "datasetId": "analytics",
  "tableId": "events"
}
```

#### Key Behaviors

- **Generic record insertion:** Events are unmarshalled into `GenericRecord` (a `map[string]bigquery.Value`), allowing insertion into any BigQuery table schema without pre-defined Go structs.
- **Insert ID deduplication:** If the event properties include an `insertId` field, it is used as the BigQuery insert ID for best-effort deduplication.
- **Batch support:** The producer accepts both single objects and arrays of objects in the `properties` field of the event payload.
- **Timeout handling:** Uses a context timeout set by the producer's `Opts.Timeout`. Timeout errors return HTTP 504 (Gateway Timeout).
- **Scopes:** Authenticates with the `bigquery.insertdata` scope for streaming insertions.

> **Source:** `services/streammanager/bqstream/bqstreammanager.go:78-106` (NewProducer), `services/streammanager/bqstream/bqstreammanager.go:108-144` (Produce)

---

### AWS Lambda

AWS Lambda invokes Lambda functions with event data as the invocation payload using the AWS SDK v2 `Invoke` API. This enables serverless event processing where each event triggers a Lambda function execution.

> **Source:** `services/streammanager/lambda/lambdamanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | `string` | Yes | AWS region where the Lambda function is deployed. Provided via AWS session config. |
| `accessKeyID` | `string` | Conditional | AWS access key ID. Required unless using IAM role-based auth. |
| `accessKey` | `string` | Conditional | AWS secret access key. Required unless using IAM role-based auth. |
| `roleARN` | `string` | Conditional | IAM role ARN for cross-account authentication. |
| `lambda` | `string` | Yes | Name or ARN of the Lambda function to invoke. |
| `invocationType` | `string` | No | Lambda invocation type: `Event` (async, default), `RequestResponse` (sync), or `DryRun`. Defaults to `Event`. |
| `clientContext` | `string` | No | Base64-encoded JSON client context to pass to the Lambda function. |

> **Source:** `services/streammanager/lambda/lambdamanager.go:31-41`, `services/streammanager/lambda/lambdamanager_utils.go:6-10`

#### Configuration Example

```json
{
  "region": "us-east-1",
  "accessKeyID": "AKIAIOSFODNN7EXAMPLE",
  "accessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  "lambda": "my-event-processor",
  "invocationType": "Event",
  "clientContext": ""
}
```

#### Key Behaviors

- **Invocation types:** Supports all three Lambda invocation types: `Event` (default, asynchronous — Lambda queues the event), `RequestResponse` (synchronous — waits for response), and `DryRun` (validation only).
- **Payload extraction:** The `payload` field is extracted from the event JSON and sent as the Lambda invocation payload.
- **Max idle connections:** Configurable via `Router.LAMBDA.httpMaxIdleConnsPerHost` (default: 64).
- **Error handling:** Uses `common.ParseAWSError` for AWS API errors.

> **Source:** `services/streammanager/lambda/lambdamanager.go:31-41` (NewProducer), `services/streammanager/lambda/lambdamanager.go:45-86` (Produce)

---

### Google Cloud Function

Google Cloud Function invokes HTTPS-triggered Cloud Functions with event data via HTTP POST requests. Authentication is handled via GCP Service Account ID tokens for secured function endpoints.

> **Source:** `services/streammanager/googlecloudfunction/googlecloudfunction.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `credentials` | `string` | Yes | GCP Service Account JSON credentials for token generation. |
| `requireAuthentication` | `bool` | No | When `true`, generates and attaches an ID token for authenticated function invocation. |
| `googleCloudFunctionUrl` | `string` | Yes | The HTTPS URL of the Cloud Function trigger endpoint. |

> **Source:** `services/streammanager/googlecloudfunction/googlecloudfunction.go:29-36`

#### Configuration Example

```json
{
  "credentials": "{\"type\":\"service_account\",\"project_id\":\"my-project\",...}",
  "requireAuthentication": true,
  "googleCloudFunctionUrl": "https://us-central1-my-project.cloudfunctions.net/process-events"
}
```

#### Key Behaviors

- **HTTP POST delivery:** Events are sent as JSON POST requests to the Cloud Function URL with `Content-Type: application/json`.
- **ID token authentication:** When `requireAuthentication` is true, an ID token is generated using `idtoken.NewTokenSource` from the GCP credentials and attached as a `Bearer` token in the `Authorization` header.
- **Token caching:** The ID token is cached and reused. A new token is generated when the token age exceeds the configurable timeout (default: 55 minutes, config key: `google.cloudfunction.token.timeout`).
- **Timeout tolerance:** HTTP timeout errors are treated as successful delivery (HTTP 202) because Cloud Functions may process the event after the HTTP connection closes.
- **HTTP client timeout:** Default 1 second for the HTTP client.

> **Source:** `services/streammanager/googlecloudfunction/googlecloudfunction.go:98-117` (NewProducer), `services/streammanager/googlecloudfunction/googlecloudfunction.go:119-169` (Produce)

---

### Google Sheets

Google Sheets appends event data as rows to a Google Sheets spreadsheet using the Google Sheets API v4. Events are mapped to spreadsheet columns via a configurable key mapping, with automatic header row management.

> **Source:** `services/streammanager/googlesheets/googlesheetsmanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `credentials` | `string` | Yes | Google OAuth credentials (JSON) for Sheets API access. |
| `sheetId` | `string` | Yes | Google Sheets spreadsheet ID (from the spreadsheet URL). |
| `sheetName` | `string` | Yes | Name of the specific sheet/tab within the spreadsheet. |
| `eventKeyMap` | `array` | Yes | Array of `{"from": "<event_field>", "to": "<column_header>"}` mappings that map event properties to spreadsheet columns. |

> **Source:** `services/streammanager/googlesheets/googlesheetsmanager.go:27-33`

#### Configuration Example

```json
{
  "credentials": "{\"type\":\"authorized_user\",...}",
  "sheetId": "1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms",
  "sheetName": "Sheet1",
  "eventKeyMap": [
    {"from": "firstName", "to": "First Name"},
    {"from": "lastName", "to": "Last Name"},
    {"from": "email", "to": "Email"},
    {"from": "product", "to": "Product Purchased"}
  ]
}
```

#### Key Behaviors

- **Automatic header management:** On the first event, the producer inserts a header row with column names derived from the `eventKeyMap` "to" values. The `messageId` column is always the first column.
- **Batched row insertion:** Event data is appended to the sheet using the Sheets API `Append` method. Supports both single events and batched events.
- **Thread-safe header update:** Uses `sync.RWMutex` to ensure the header is inserted exactly once, even under concurrent access.
- **Column positioning:** Event data is inserted into columns at positions matching the `eventKeyMap` order, with `messageId` at position 0.

> **Source:** `services/streammanager/googlesheets/googlesheetsmanager.go:60-84` (NewProducer), `services/streammanager/googlesheets/googlesheetsmanager.go:119-155` (Produce)

---

### Amazon Personalize

Amazon Personalize sends event data to Amazon Personalize for real-time recommendations using the AWS SDK v2 Personalize Events API. The producer supports three operation types: `PutEvents`, `PutUsers`, and `PutItems`.

> **Source:** `services/streammanager/personalize/personalizemanager.go`

#### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `region` | `string` | Yes | AWS region for the Personalize service. Provided via AWS session config. |
| `accessKeyID` | `string` | Conditional | AWS access key ID. Required unless using IAM role-based auth. |
| `accessKey` | `string` | Conditional | AWS secret access key. Required unless using IAM role-based auth. |
| `roleARN` | `string` | Conditional | IAM role ARN for cross-account authentication. |

> **Source:** `services/streammanager/personalize/personalizemanager.go:32-42`

#### Configuration Example

```json
{
  "region": "us-east-1",
  "accessKeyID": "AKIAIOSFODNN7EXAMPLE",
  "accessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
}
```

#### Key Behaviors

- **Three operation types:** Supports `PutEvents` (track user interactions), `PutUsers` (import user profiles), and `PutItems` (import item catalog). The operation type is determined by the `choice` field in the event payload.
- **Default operation:** If no `choice` is specified, the producer defaults to `PutEvents`.
- **Payload extraction:** The `payload` field in the event JSON contains the Personalize-specific input structure (tracking ID, session ID, event list, etc.).
- **Max idle connections:** Configurable via `Router.PERSONALIZE.httpMaxIdleConnsPerHost` (default: 64).

> **Source:** `services/streammanager/personalize/personalizemanager.go:32-42` (NewProducer), `services/streammanager/personalize/personalizemanager.go:45-95` (Produce)

---

### Wunderkind

Wunderkind delivers events to the Wunderkind marketing platform via an **AWS Lambda function invocation**. Unlike the generic Lambda destination, Wunderkind uses server-side environment variable configuration for AWS credentials and a specific Lambda function ARN, with `RequestResponse` (synchronous) invocation type.

> **Source:** `services/streammanager/wunderkind/wunderkindmanager.go`

#### Configuration Parameters

Wunderkind uses **server-side environment variables** instead of per-destination config for AWS authentication:

| Parameter | Source | Description |
|-----------|--------|-------------|
| `WUNDERKIND_REGION` | Environment variable | AWS region for the Lambda function. |
| `WUNDERKIND_IAM_ROLE_ARN` | Environment variable | IAM role ARN for role-based authentication. |
| `WUNDERKIND_EXTERNAL_ID` | Environment variable | External ID for cross-account IAM role assumption. |
| `WUNDERKIND_LAMBDA` | Environment variable | Name or ARN of the Wunderkind Lambda function. |

> **Source:** `services/streammanager/wunderkind/wunderkind_utils.go:4-8`

#### Key Behaviors

- **Role-based auth only:** Unlike other AWS destinations, Wunderkind exclusively uses IAM role-based authentication with `RoleBasedAuth: true`.
- **Synchronous invocation:** Uses `RequestResponse` invocation type (synchronous) with `LogType: "Tail"`, unlike the generic Lambda destination which defaults to `Event` (async).
- **Function error handling:** Checks `response.FunctionError` for payload-level errors even when the Lambda invocation itself succeeds (HTTP 200 from AWS but logical failure in the function).
- **Max idle connections:** Configurable via `Router.WUNDERKIND.httpMaxIdleConnsPerHost` (default: 64).

> **Source:** `services/streammanager/wunderkind/wunderkindmanager.go:34-55` (NewProducer), `services/streammanager/wunderkind/wunderkindmanager.go:58-111` (Produce)

---

## Circuit Breaker and Connection Management

The Custom Destination Manager (`CustomManagerT`) implements a **circuit breaker pattern** using the `sony/gobreaker` library to prevent cascading failures when a destination becomes unavailable.

### Circuit Breaker Behavior

Each destination ID has its own `gobreaker.CircuitBreaker` instance, configured with a timeout after which the circuit breaker transitions from **Open** (failing fast) back to **Half-Open** (allowing a trial request):

```go
customManager.breaker[destID] = &breakerHolder{
    config: newDestConfig,
    breaker: gobreaker.NewCircuitBreaker(gobreaker.Settings{
        Name:    destID,
        Timeout: customManager.breakerTimeout,
    }),
}
```

> **Source:** `router/customdestinationmanager/customdestinationmanager.go:248-254`

### Per-Destination Client Caching

Producer clients are cached per destination ID and protected by per-destination `sync.RWMutex` locks:

| Component | Type | Purpose |
|-----------|------|---------|
| `config` | `map[string]backendconfig.DestinationT` | Stores the latest backend config for each destination |
| `breaker` | `map[string]*breakerHolder` | Per-destination circuit breaker with config snapshot |
| `clientMu` | `map[string]*sync.RWMutex` | Per-destination read/write mutex for client access |
| `client` | `map[string]*clientHolder` | Cached producer client with config snapshot |

> **Source:** `router/customdestinationmanager/customdestinationmanager.go:46-59`

### Client Lifecycle

1. **Creation:** When `SendData` is called and no client exists, `newClient()` creates a new producer through the circuit breaker. If the circuit is open, the call fails fast with the last recorded error.
2. **Config change detection:** When backend config changes, `onConfigChange()` compares the new config against the cached config (excluding `eventDeliveryTS` and `eventDelivery` fields). If the config changed, the existing client is closed and a new one is created.
3. **Cleanup:** When a client is replaced or removed, `close()` calls `StreamProducer.Close()` to release the underlying producer resources and removes the entry from the cache.

> **Source:** `router/customdestinationmanager/customdestinationmanager.go:86-123` (newClient), `router/customdestinationmanager/customdestinationmanager.go:197-208` (close), `router/customdestinationmanager/customdestinationmanager.go:226-261` (onConfigChange)

---

## KV Store Destinations

In addition to stream destinations, the Custom Destination Manager also manages **KV store destinations** — currently limited to **Redis**.

Redis is dispatched via `kvstoremanager.New` instead of `streammanager.NewProducer`:

```go
KVStoreDestinations = []string{"REDIS"}
```

KV store destinations support three delivery modes:
- **JSON send** — `kvManager.SendDataAsJSON()` for raw JSON storage
- **HSET** — `kvManager.HSet(hash, key, value)` for hash field operations
- **HMSET** — `kvManager.HMSet(key, fields)` for multiple hash field operations

The delivery mode is selected automatically based on the event structure and destination configuration.

> **Source:** `router/customdestinationmanager/customdestinationmanager.go:80` (KVStoreDestinations), `router/customdestinationmanager/customdestinationmanager.go:106-112` (KV client creation), `router/customdestinationmanager/customdestinationmanager.go:133-152` (KV send)

---

## Configuration Reference

The following table lists all Router configuration parameters that affect stream destination behavior. These parameters are set in `config/config.yaml` or via environment variables.

### Per-Destination HTTP Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `Router.<DEST_TYPE>.httpMaxIdleConnsPerHost` | `int` | `64` | Maximum number of idle HTTP connections per host for AWS SDK-based destinations. Falls back to `Router.<DEST_TYPE>.noOfWorkers`, then `Router.noOfWorkers`. |
| `Router.<DEST_TYPE>.noOfWorkers` | `int` | Varies | Number of router workers for a specific destination type. |
| `Router.noOfWorkers` | `int` | Varies | Default number of router workers (global fallback). |

Replace `<DEST_TYPE>` with the destination's internal name (e.g., `KINESIS`, `FIREHOSE`, `EVENTBRIDGE`, `LAMBDA`, `PERSONALIZE`, `WUNDERKIND`).

### Kafka-Specific Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `Router.KAFKA.dialTimeout` | `duration` | `10s` | TCP dial timeout for connecting to Kafka brokers. Falls back to `Router.kafkaDialTimeout`. |
| `Router.AZURE_EVENT_HUB.dialTimeout` | `duration` | `10s` | TCP dial timeout for Azure Event Hub connections. Falls back to `Router.kafkaDialTimeout`. |
| `Router.CONFLUENT_CLOUD.dialTimeout` | `duration` | `10s` | TCP dial timeout for Confluent Cloud connections. Falls back to `Router.kafkaDialTimeout`. |
| `KAFKA_SSL_CERTIFICATE_FILE_PATH` | `string` | `""` | Path to the SSL client certificate file for Kafka mTLS. |
| `KAFKA_SSL_KEY_FILE_PATH` | `string` | `""` | Path to the SSL client key file for Kafka mTLS. |

### Google Pub/Sub Publisher Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `Router.GOOGLEPUBSUB.Client.NumGoroutines` | `int` | `25` | Number of goroutines for message publishing per topic. |
| `Router.GOOGLEPUBSUB.Client.DelayThreshold` | `duration` | `10ms` | Maximum delay before a batch is published. |
| `Router.GOOGLEPUBSUB.Client.CountThreshold` | `int` | `64` | Maximum number of messages per batch. Falls back to `Router.GOOGLEPUBSUB.noOfWorkers`. |
| `Router.GOOGLEPUBSUB.Client.ByteThreshold` | `int` | `10485760` (10MB) | Maximum byte size per batch. |
| `Router.GOOGLEPUBSUB.Client.MaxOutstandingMessages` | `int` | `1000` | Maximum messages in flight before flow control blocks. |
| `Router.GOOGLEPUBSUB.Client.MaxOutstandingBytes` | `int` | `-1` (unlimited) | Maximum bytes in flight before flow control blocks. |
| `Router.GOOGLEPUBSUB.Client.EnableRetries` | `bool` | `false` | Enable retry with backoff for transient authentication errors. |
| `Router.GOOGLEPUBSUB.Client.Retry.InitialInterval` | `duration` | `10ms` | Initial retry backoff interval. |
| `Router.GOOGLEPUBSUB.Client.Retry.MaxInterval` | `duration` | `500ms` | Maximum retry backoff interval. |
| `Router.GOOGLEPUBSUB.Client.Retry.MaxElapsedTime` | `duration` | `2s` | Maximum total time for retries. |
| `Router.GOOGLEPUBSUB.Client.Retry.MaxRetries` | `int` | `3` | Maximum number of retry attempts. |

### Google Cloud Function Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `google.cloudfunction.token.timeout` | `duration` | `55m` | Token refresh interval for GCP ID token authentication. |

> **Source:** `services/streammanager/kafka/kafkamanager.go:285`, `services/streammanager/kinesis/kinesismanager.go:38`, `services/streammanager/googlepubsub/googlepubsubmanager.go:104-124`, `services/streammanager/googlecloudfunction/googlecloudfunction.go:93`

---

## Error Handling

### Common AWS Error Mapping

All AWS SDK-based stream destinations (Kinesis, EventBridge, Firehose, Lambda, Personalize, Wunderkind) share the `common.ParseAWSError` function for error classification. This function maps AWS API errors to HTTP-style status codes for the Router's retry logic:

| Error Type | Condition | Status Code | Router Behavior |
|------------|-----------|-------------|-----------------|
| `ThrottlingException` | AWS rate limiting (`errorMessage` contains "Throttling") | **429** | Retry with backoff |
| `RequestExpired` | Expired AWS request signature (`errorMessage` contains "RequestExpired") | **500** | Retry |
| Client fault | `smithy.FaultClient` (invalid request, auth error) | **400** | No retry (permanent failure) |
| Server fault | `smithy.FaultServer` (AWS internal error) | **500** | Retry |
| Operation error | `smithy.OperationError` (SDK-level error) | **400** (with throttling check) | Depends on error message |
| Unknown error | Fallback | **500** | Retry |

> **Source:** `services/streammanager/common/common.go:29-73`

### Per-Destination Error Handling

| Destination | Error Condition | Status Code | Notes |
|-------------|----------------|-------------|-------|
| **Kinesis** | `ProvisionedThroughputExceededException` | **429** | Overrides default AWS mapping for stream throughput limits |
| **Google Pub/Sub** | `ResourceExhausted` (gRPC) | **429** | gRPC-native throttling |
| **Google Pub/Sub** | `Unavailable` (gRPC) | **503** | Transient gRPC failure |
| **Google Pub/Sub** | `DeadlineExceeded` (gRPC/context) | **504** | Timeout |
| **Google Pub/Sub** | `PermissionDenied` (gRPC) | **403** | Auth failure |
| **Google Pub/Sub** | `Unauthenticated` (gRPC) | **401** | Missing/invalid credentials |
| **Google Cloud Function** | HTTP timeout (`os.IsTimeout`) | **202** | Treated as success (function may still process) |
| **BigQuery Stream** | Context deadline exceeded | **504** | Timeout on BQ API call |
| **Wunderkind** | `response.FunctionError != nil` | **400** | Lambda succeeded but function returned error |
| **EventBridge** | Entry-level `ErrorCode` | **400** | Missing required fields (Detail/DetailType/Source) |

> **Source:** `services/streammanager/kinesis/kinesismanager.go:47-53`, `services/streammanager/googlepubsub/googlepubsubmanager.go:282-314`, `services/streammanager/googlecloudfunction/googlecloudfunction.go:149-158`, `services/streammanager/bqstream/bqstreammanager.go:136-141`, `services/streammanager/wunderkind/wunderkindmanager.go:97-108`

---

## Cross-References

- **[End-to-End Data Flow](../../architecture/data-flow.md)** — Understand how events reach stream destinations through the complete pipeline
- **[System Architecture Overview](../../architecture/overview.md)** — High-level system topology and component relationships
- **[Destination Catalog](./index.md)** — Overview of all destination categories (cloud, stream, warehouse, KV store)
- **[Destination Catalog Parity](../../gap-report/destination-catalog-parity.md)** — Gap analysis comparing RudderStack and Segment destination catalogs
- **[Capacity Planning](../operations/capacity-planning.md)** — Throughput tuning for 50k events/sec including router worker configuration
- **[Cloud Destinations](./cloud-destinations.md)** — REST/HTTP-based destination integrations
- **[Warehouse Destinations](./warehouse-destinations.md)** — Batch warehouse loading connectors
