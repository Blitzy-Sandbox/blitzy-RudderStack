# Server-Side SDK Compatibility Guide

Server-side SDKs enable you to send event data from backend applications directly to the RudderStack Gateway. This guide documents the full compatibility status and migration procedures for all five Segment server-side SDKs: **Node.js** (`analytics-node`), **Python** (`analytics-python`), **Go** (`analytics-go`), **Java** (`analytics-java`), and **Ruby** (`analytics-ruby`).

**Key migration principle:** To migrate from Segment to RudderStack, change **only** the `host`/`endpoint` URL and write key — all event payloads, batch semantics, retry behavior, and authentication are identical. No code changes to event calls are required.

RudderStack's Gateway exposes a Segment-compatible HTTP API on port `8080`. All six core event types (`identify`, `track`, `page`, `screen`, `group`, `alias`) use identical payload schemas. The `/v1/batch` endpoint accepts mixed-type batches exactly as all server-side SDKs produce them.

> **Source references:**
> - `gateway/openapi.yaml` — OpenAPI 3.0.3 specification defining all event endpoints and payload schemas
> - `gateway/handle_http.go` — HTTP handler wiring for all event type handlers (`webIdentifyHandler`, `webTrackHandler`, `webPageHandler`, `webScreenHandler`, `webGroupHandler`, `webAliasHandler`, `webBatchHandler`)
> - `gateway/handle_http_auth.go:24-58` — `writeKeyAuth` middleware implementing HTTP Basic Auth with write key
> - `gateway/handle.go` — Core request handler processing SDK payloads through the Gateway pipeline

---

## Table of Contents

- [Compatibility Status Overview](#compatibility-status-overview)
- [Common Patterns](#common-patterns)
- [Batch Semantics](#batch-semantics)
- [Node.js (analytics-node)](#nodejs-analytics-node)
- [Python (analytics-python)](#python-analytics-python)
- [Go (analytics-go)](#go-analytics-go)
- [Java (analytics-java)](#java-analytics-java)
- [Ruby (analytics-ruby)](#ruby-analytics-ruby)
- [Retry Behavior](#retry-behavior)
- [Authentication Deep-Dive](#authentication-deep-dive)
- [context.library Verification](#contextlibrary-verification)
- [Troubleshooting](#troubleshooting)
- [Cross-References](#cross-references)

---

## Compatibility Status Overview

All five Segment server-side SDKs are **fully compatible** with the RudderStack Gateway. Each SDK has been verified to work with endpoint URL substitution and write key replacement only — no SDK code changes or payload modifications are required.

| Feature | Node.js | Python | Go | Java | Ruby |
|---------|---------|--------|----|------|------|
| **Package** | `@segment/analytics-node` | `segment-analytics-python` | `analytics-go/v3` | `analytics-java` | `analytics-ruby` |
| **Min Runtime** | Node.js 18+ | Python 3.x | Go 1.18+ | Java 8+ | Ruby 2.4+ |
| **Default Batch Size** | 20 | 100 | 20 | 250 | 100 |
| **Default Flush Interval** | 10s | 0.5s | 5s | 10s | Queue-based |
| **Compatibility Status** | ✅ Verified | ✅ Verified | ✅ Verified | ✅ Verified | ✅ Verified |

**Verified capabilities per SDK:**

| Capability | Node.js | Python | Go | Java | Ruby |
|------------|---------|--------|----|------|------|
| `identify` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `track` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `page` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `screen` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `group` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `alias` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `/v1/batch` | ✅ | ✅ | ✅ | ✅ | ✅ |
| Write Key Basic Auth | ✅ | ✅ | ✅ | ✅ | ✅ |
| Retry on 5xx | ✅ | ✅ | ✅ | ✅ | ✅ |
| Graceful shutdown | ✅ | ✅ | ✅ | ✅ | ✅ |

---

## Common Patterns

All server-side SDKs share the following behavioral patterns when communicating with the RudderStack Gateway:

### Batch Endpoint

All server-side SDKs use **`POST /v1/batch`** as their primary transport mechanism. Individual event methods (`identify`, `track`, `page`, `screen`, `group`, `alias`) are enqueued into an in-memory buffer and flushed as a single batch HTTP request. This minimizes network overhead and provides high-throughput event delivery.

> **Source:** `gateway/handle_http.go:28-29` — `webBatchHandler` wraps `writeKeyAuth` around the batch processing pipeline.

### Write Key Basic Auth

All server-side SDKs authenticate using HTTP Basic Auth with the **write key as the username** and an **empty password**. The `Authorization` header format is:

```
Authorization: Basic base64(writeKey + ":")
```

The trailing colon after the write key is critical — it denotes the empty password field in the Basic Auth `username:password` format. All five SDKs set this header automatically from the `writeKey` constructor argument.

> **Source:** `gateway/handle_http_auth.go:24-58` — The `writeKeyAuth` middleware extracts the write key via `r.BasicAuth()`, validates against the source map, checks `SourceEnabled`, and populates `AuthRequestContext`.

### Mixed Event Type Batches

The `/v1/batch` endpoint accepts a `batch` array containing **mixed event types** in a single request. A single batch can include `identify`, `track`, `page`, `screen`, `group`, and `alias` events in any order. This is the standard behavior for all server-side SDKs that flush events in batches.

```json
{
  "batch": [
    {
      "type": "identify",
      "userId": "user-f47ac10b",
      "traits": { "name": "Jane Smith", "email": "jane@example.com" },
      "timestamp": "2024-01-15T10:30:00.000Z",
      "messageId": "adb5b2c0-1a3f-4c8e-9b1d-5e7f8a2c3d4e"
    },
    {
      "type": "track",
      "userId": "user-f47ac10b",
      "event": "Order Completed",
      "properties": { "orderId": "ord-98765", "revenue": 149.99, "currency": "USD" },
      "timestamp": "2024-01-15T10:31:00.000Z",
      "messageId": "b1c2d3e4-f5a6-7890-abcd-ef1234567890"
    },
    {
      "type": "page",
      "userId": "user-f47ac10b",
      "name": "Dashboard",
      "properties": { "url": "https://app.example.com/dashboard" },
      "timestamp": "2024-01-15T10:32:00.000Z",
      "messageId": "c2d3e4f5-a6b7-8901-bcde-f12345678901"
    }
  ],
  "sentAt": "2024-01-15T10:32:05.000Z"
}
```

### context.channel

All server-side SDKs set `context.channel` to `"server"`. This distinguishes server-side events from client-side events (`"client"` for web and mobile SDKs) in the downstream pipeline.

### context.library Metadata

Each SDK automatically populates `context.library` with its platform identifier and version. The Gateway preserves this metadata through the entire processing pipeline. See the [context.library Verification](#contextlibrary-verification) section for exact values per SDK.

### Architecture

```mermaid
flowchart LR
    subgraph SDKs["Server-Side SDKs"]
        NODE["Node.js<br/>analytics-node"]
        PY["Python<br/>analytics-python"]
        GO["Go<br/>analytics-go"]
        JAVA["Java<br/>analytics-java"]
        RUBY["Ruby<br/>analytics-ruby"]
    end

    subgraph Gateway["RudderStack Gateway :8080"]
        AUTH["writeKeyAuth<br/>Basic Auth"]
        BATCH["/v1/batch"]
        VALID["Payload Validation"]
    end

    subgraph Pipeline["Processing Pipeline"]
        JOBSDB["JobsDB<br/>(PostgreSQL)"]
        PROC["Processor"]
        ROUTER["Router"]
        DEST["Destinations"]
    end

    NODE -->|"POST /v1/batch<br/>Authorization: Basic"| AUTH
    PY -->|"POST /v1/batch<br/>Authorization: Basic"| AUTH
    GO -->|"POST /v1/batch<br/>Authorization: Basic"| AUTH
    JAVA -->|"POST /v1/batch<br/>Authorization: Basic"| AUTH
    RUBY -->|"POST /v1/batch<br/>Authorization: Basic"| AUTH

    AUTH --> BATCH
    BATCH --> VALID
    VALID --> JOBSDB
    JOBSDB --> PROC
    PROC --> ROUTER
    ROUTER --> DEST
```

---

## Batch Semantics

Understanding batch semantics is critical for server-side SDK integration. All SDKs batch events in memory and flush them periodically to the Gateway.

### Batch Size Limits

Each SDK has a default batch size (number of events per HTTP request). The Gateway enforces a **4 MB maximum request body size** (configured in `config/config.yaml`). If a batch exceeds this limit, the Gateway responds with `413 Request Entity Too Large`.

| SDK | Default Batch Size | Max Request Body |
|-----|-------------------|-----------------|
| Node.js | 20 events | 4 MB (Gateway limit) |
| Python | 100 events | 4 MB (Gateway limit) |
| Go | 20 events | 4 MB (Gateway limit) |
| Java | 250 events | 4 MB (Gateway limit) |
| Ruby | 100 events | 4 MB (Gateway limit) |

Reduce the SDK batch size if your events contain large payloads (extensive properties or traits) that may cause the aggregate batch to exceed 4 MB.

### Flush Intervals

If the batch size threshold is not reached, the SDK flushes automatically on a timer:

| SDK | Default Flush Interval | Configurable |
|-----|----------------------|--------------|
| Node.js | 10 seconds | Yes (`flushInterval`) |
| Python | 0.5 seconds | Yes (`upload_interval`) |
| Go | 5 seconds | Yes (`Interval`) |
| Java | 10 seconds | Yes (`.flushInterval()`) |
| Ruby | Queue-based | Yes (internal queue drain) |

### Duplicate messageId Handling

The Gateway **accepts duplicate `messageId` values** without rejection. Deduplication, if configured, happens downstream in the processing pipeline. Server-side SDKs auto-generate a UUID v4 `messageId` for each event — duplicates typically indicate SDK retry behavior after a network timeout where the original request actually succeeded.

### Mixed Event Type Batches

The `batch` array may contain `identify`, `track`, `page`, `screen`, `group`, and `alias` events **in any order** within a single request. The Gateway processes each event individually based on its `type` field. No sorting or grouping by event type is required.

### sentAt Field

The `sentAt` field is set by the SDK at the moment the batch HTTP request is sent. The Gateway uses `sentAt` in conjunction with `timestamp` (set at event creation time) to correct for **clock drift** between the SDK host and the Gateway server. Both fields use ISO 8601 format with millisecond precision.

### timestamp Field

The `timestamp` field is set by the SDK at event creation time and is **preserved through the entire pipeline** without modification. If omitted, the Gateway assigns `receivedAt` as the effective timestamp.

### Batch Payload Format

All server-side SDKs produce the following batch payload structure:

```json
{
  "batch": [
    {
      "type": "identify",
      "userId": "user-a1b2c3d4",
      "traits": {
        "name": "Alex Johnson",
        "email": "alex@example.com",
        "plan": "enterprise"
      },
      "context": {
        "library": { "name": "analytics-node", "version": "2.1.0" },
        "channel": "server"
      },
      "timestamp": "2024-01-15T12:00:00.000Z",
      "messageId": "d4e5f6a7-b8c9-0123-4567-89abcdef0123"
    },
    {
      "type": "track",
      "userId": "user-a1b2c3d4",
      "event": "Invoice Paid",
      "properties": {
        "invoiceId": "inv-7890",
        "amount": 250.00,
        "currency": "USD"
      },
      "context": {
        "library": { "name": "analytics-node", "version": "2.1.0" },
        "channel": "server"
      },
      "timestamp": "2024-01-15T12:01:00.000Z",
      "messageId": "e5f6a7b8-c9d0-1234-5678-9abcdef01234"
    }
  ],
  "sentAt": "2024-01-15T12:01:05.000Z"
}
```

---

## Node.js (analytics-node)

### Migration Configuration

To migrate from Segment to RudderStack, change only the `host` parameter:

**Before (Segment):**

```javascript
const { Analytics } = require('@segment/analytics-node')

const analytics = new Analytics({
  writeKey: 'SEGMENT_WRITE_KEY'
  // host defaults to https://api.segment.io
})
```

**After (RudderStack):**

```javascript
const { Analytics } = require('@segment/analytics-node')

const analytics = new Analytics({
  writeKey: 'YOUR_RUDDERSTACK_WRITE_KEY',
  host: 'https://YOUR_DATA_PLANE_URL:8080'
})
```

All event methods (`identify`, `track`, `page`, `screen`, `group`, `alias`) remain identical — no code changes are needed beyond initialization.

### context.library

The Node.js SDK automatically sets:

```json
{
  "context": {
    "library": {
      "name": "analytics-node",
      "version": "2.1.0"
    },
    "channel": "server"
  }
}
```

The `version` field reflects the installed SDK version.

### Batch Behavior

| Parameter | Default | Description |
|-----------|---------|-------------|
| `flushAt` | 20 | Number of events to queue before sending a batch |
| `flushInterval` | 10000 ms | Milliseconds between automatic flushes |
| `maxEventsInBatch` | 20 | Maximum events per single HTTP request |
| `messageId` | Auto (UUID v4) | Automatically generated for each event if not provided |

### Retry Semantics

| Parameter | Default | Description |
|-----------|---------|-------------|
| `maxRetries` | 3 | Maximum retry attempts for failed requests |
| Backoff | Exponential | Increasing delay between retry attempts |
| Retry on 5xx | Yes | Server errors trigger automatic retry |
| Retry on 4xx | No | Client errors (400, 401, 404) are not retried |

### Graceful Shutdown

Always flush remaining events before your process exits:

```javascript
// Flush and close the client gracefully
await analytics.closeAndFlush()
```

Failing to call `closeAndFlush()` before process exit may result in data loss for events still in the in-memory buffer.

### Example Payload

A track event sent from `analytics-node` produces the following payload at the Gateway:

```json
{
  "batch": [
    {
      "type": "track",
      "userId": "user-node-001",
      "event": "Report Generated",
      "properties": {
        "reportId": "rpt-12345",
        "format": "pdf",
        "pages": 42
      },
      "context": {
        "library": { "name": "analytics-node", "version": "2.1.0" },
        "channel": "server"
      },
      "timestamp": "2024-01-15T14:30:00.000Z",
      "messageId": "f6a7b8c9-d0e1-2345-6789-abcdef012345"
    }
  ],
  "sentAt": "2024-01-15T14:30:01.000Z"
}
```

---

## Python (analytics-python)

### Migration Configuration

To migrate from Segment to RudderStack, set the `host` attribute:

**Before (Segment):**

```python
import segment.analytics as analytics

analytics.write_key = 'SEGMENT_WRITE_KEY'
# host defaults to https://api.segment.io
```

**After (RudderStack):**

```python
import segment.analytics as analytics

analytics.write_key = 'YOUR_RUDDERSTACK_WRITE_KEY'
analytics.host = 'https://YOUR_DATA_PLANE_URL:8080'
```

All event methods (`identify`, `track`, `page`, `screen`, `group`, `alias`) remain identical.

### context.library

The Python SDK automatically sets:

```json
{
  "context": {
    "library": {
      "name": "analytics-python",
      "version": "2.2.3"
    },
    "channel": "server"
  }
}
```

### Batch Behavior

| Parameter | Default | Description |
|-----------|---------|-------------|
| `upload_size` | 100 | Number of events per batch request |
| `upload_interval` | 0.5 s | Seconds between automatic flushes by the background thread |
| `max_queue_size` | 10000 | Maximum number of events in the in-memory queue |
| `gzip` | False | Enable gzip compression for outbound requests |

### Retry Semantics

| Parameter | Default | Description |
|-----------|---------|-------------|
| Max retries | 3 | Background thread retries on 5xx errors |
| Backoff | Linear | Fixed delay between retry attempts |
| Retry on 5xx | Yes | Server errors trigger automatic retry |
| Retry on 4xx | No | Client errors are not retried |

### Graceful Shutdown

Always flush and shut down the background thread before process exit:

```python
analytics.flush()     # Send all queued events immediately
analytics.shutdown()  # Clean shutdown of the background thread
```

### Example Payload

A track event sent from `analytics-python` produces:

```json
{
  "batch": [
    {
      "type": "track",
      "userId": "user-py-001",
      "event": "Model Training Started",
      "properties": {
        "modelId": "mdl-56789",
        "framework": "pytorch",
        "epochs": 100
      },
      "context": {
        "library": { "name": "analytics-python", "version": "2.2.3" },
        "channel": "server"
      },
      "timestamp": "2024-01-15T15:00:00.000Z",
      "messageId": "a7b8c9d0-e1f2-3456-7890-bcdef0123456"
    }
  ],
  "sentAt": "2024-01-15T15:00:00.500Z"
}
```

---

## Go (analytics-go)

### Migration Configuration

To migrate from Segment to RudderStack, set the `Endpoint` field in the config:

**Before (Segment):**

```go
import "github.com/segmentio/analytics-go/v3"

// Simple initialization (uses default Segment endpoint)
client := analytics.New("SEGMENT_WRITE_KEY")
defer client.Close()
```

**After (RudderStack):**

```go
import "github.com/segmentio/analytics-go/v3"

client, err := analytics.NewWithConfig("YOUR_RUDDERSTACK_WRITE_KEY", analytics.Config{
    Endpoint: "https://YOUR_DATA_PLANE_URL:8080",
})
if err != nil {
    log.Fatal(err)
}
defer client.Close()
```

All event methods (`Enqueue` with `analytics.Identify`, `analytics.Track`, `analytics.Page`, `analytics.Screen`, `analytics.Group`, `analytics.Alias`) remain identical.

### context.library

The Go SDK automatically sets:

```json
{
  "context": {
    "library": {
      "name": "analytics-go",
      "version": "3.3.0"
    },
    "channel": "server"
  }
}
```

### Batch Behavior

| Parameter | Default | Description |
|-----------|---------|-------------|
| `BatchSize` | 20 | Number of events per batch request |
| `Interval` | 5 seconds | Maximum time between automatic flushes |
| `DefaultContext` | nil | Default context fields applied to every event |

### Enqueue Pattern

The Go SDK uses a goroutine-safe `Enqueue` pattern. Multiple goroutines can safely call `Enqueue` concurrently:

```go
client.Enqueue(analytics.Track{
    UserId: "user-go-001",
    Event:  "Build Completed",
    Properties: analytics.NewProperties().
        Set("buildId", "bld-11111").
        Set("duration", 120).
        Set("status", "success"),
})
```

### Graceful Shutdown

`client.Close()` flushes all remaining buffered events before returning:

```go
defer client.Close() // Flushes remaining events on program exit
```

### Retry Semantics

| Parameter | Default | Description |
|-----------|---------|-------------|
| Max retries | 10 | Higher default retry count than other SDKs |
| Backoff | Exponential | Increasing delay between retry attempts |
| `RetryAfter` | Default backoff | Custom retry delay function (receives attempt number) |
| Retry on 5xx | Yes | Server errors trigger automatic retry |
| Retry on 4xx | No | Client errors are not retried |

### Example Payload

A track event sent from `analytics-go` produces:

```json
{
  "batch": [
    {
      "type": "track",
      "userId": "user-go-001",
      "event": "Build Completed",
      "properties": {
        "buildId": "bld-11111",
        "duration": 120,
        "status": "success"
      },
      "context": {
        "library": { "name": "analytics-go", "version": "3.3.0" },
        "channel": "server"
      },
      "timestamp": "2024-01-15T16:00:00.000Z",
      "messageId": "b8c9d0e1-f2a3-4567-8901-cdef01234567"
    }
  ],
  "sentAt": "2024-01-15T16:00:02.000Z"
}
```

---

## Java (analytics-java)

### Migration Configuration

To migrate from Segment to RudderStack, set the `.endpoint()` in the builder:

**Before (Segment):**

```java
import com.segment.analytics.Analytics;

Analytics analytics = Analytics.builder("SEGMENT_WRITE_KEY")
    .build();
// endpoint defaults to https://api.segment.io
```

**After (RudderStack):**

```java
import com.segment.analytics.Analytics;

Analytics analytics = Analytics.builder("YOUR_RUDDERSTACK_WRITE_KEY")
    .endpoint("https://YOUR_DATA_PLANE_URL:8080")
    .build();
```

All event methods (`enqueue` with `IdentifyMessage`, `TrackMessage`, `PageMessage`, `ScreenMessage`, `GroupMessage`, `AliasMessage`) remain identical.

### context.library

The Java SDK automatically sets:

```json
{
  "context": {
    "library": {
      "name": "analytics-java",
      "version": "3.5.0"
    },
    "channel": "server"
  }
}
```

### Batch Behavior

| Parameter | Default | Description |
|-----------|---------|-------------|
| `.flushQueueSize()` | 250 | Number of events to queue before sending a batch |
| `.flushInterval()` | 10 seconds | Maximum time between automatic flushes |
| Builder pattern | Yes | All configuration via `Analytics.builder()` fluent API |

The Java SDK has the highest default batch size (250 events) among all server-side SDKs. Monitor the 4 MB Gateway request body limit when using large event payloads.

### Retry Semantics

| Parameter | Default | Description |
|-----------|---------|-------------|
| Max retries | 3 | Internal thread pool retries on failure |
| Backoff | Exponential | Increasing delay between retry attempts |
| Retry on 5xx | Yes | Server errors trigger automatic retry |
| Retry on 4xx | No | Client errors are not retried |

### Graceful Shutdown

Shut down with a timeout to ensure all events are flushed:

```java
analytics.shutdown();
// Or with explicit flush before shutdown:
analytics.flush();
analytics.shutdown();
```

### Example Payload

A track event sent from `analytics-java` produces:

```json
{
  "batch": [
    {
      "type": "track",
      "userId": "user-java-001",
      "event": "Payment Processed",
      "properties": {
        "paymentId": "pay-22222",
        "amount": 500.00,
        "currency": "EUR",
        "gateway": "stripe"
      },
      "context": {
        "library": { "name": "analytics-java", "version": "3.5.0" },
        "channel": "server"
      },
      "timestamp": "2024-01-15T17:00:00.000Z",
      "messageId": "c9d0e1f2-a3b4-5678-9012-def012345678"
    }
  ],
  "sentAt": "2024-01-15T17:00:05.000Z"
}
```

---

## Ruby (analytics-ruby)

### Migration Configuration

To migrate from Segment to RudderStack, set the `host` option:

**Before (Segment):**

```ruby
require 'segment/analytics'

analytics = Segment::Analytics.new({
  write_key: 'SEGMENT_WRITE_KEY'
  # host defaults to https://api.segment.io
})
```

**After (RudderStack):**

```ruby
require 'segment/analytics'

analytics = Segment::Analytics.new({
  write_key: 'YOUR_RUDDERSTACK_WRITE_KEY',
  host: 'https://YOUR_DATA_PLANE_URL:8080'
})
```

All event methods (`identify`, `track`, `page`, `screen`, `group`, `alias`) remain identical.

### context.library

The Ruby SDK automatically sets:

```json
{
  "context": {
    "library": {
      "name": "analytics-ruby",
      "version": "2.4.0"
    },
    "channel": "server"
  }
}
```

### Batch Behavior

| Parameter | Default | Description |
|-----------|---------|-------------|
| Batch size | 100 | Number of events per batch request |
| Flush mode | Queue-based | Events are drained from an internal thread-safe queue |
| `on_error` | nil | Error callback: `Proc.new { |status, msg| ... }` |

### Retry Semantics

| Parameter | Default | Description |
|-----------|---------|-------------|
| Max retries | 3 | Background thread retries on failure |
| Backoff | Exponential | Increasing delay between retry attempts |
| Retry on 5xx | Yes | Server errors trigger automatic retry |
| Retry on 4xx | No | Client errors are not retried |

### Graceful Shutdown

Flush events before process exit:

```ruby
analytics.flush
# Always call flush before process termination to avoid data loss
```

### Example Payload

A track event sent from `analytics-ruby` produces:

```json
{
  "batch": [
    {
      "type": "track",
      "userId": "user-ruby-001",
      "event": "Subscription Renewed",
      "properties": {
        "subscriptionId": "sub-33333",
        "plan": "pro",
        "mrr": 99.00,
        "currency": "USD"
      },
      "context": {
        "library": { "name": "analytics-ruby", "version": "2.4.0" },
        "channel": "server"
      },
      "timestamp": "2024-01-15T18:00:00.000Z",
      "messageId": "d0e1f2a3-b4c5-6789-0123-ef0123456789"
    }
  ],
  "sentAt": "2024-01-15T18:00:01.000Z"
}
```

---

## Retry Behavior

All server-side SDKs implement automatic retry logic for transient failures. Understanding retry behavior is important for ensuring reliable event delivery.

### Common Retry Semantics

The following rules apply across all five SDKs:

- **Retry on 5xx server errors:** All SDKs automatically retry when the Gateway returns a 500, 502, 503, or 504 response
- **Retry on network timeouts:** Connection timeouts and read timeouts trigger automatic retries
- **Do NOT retry on 4xx client errors:** Responses such as 400 (Bad Request), 401 (Unauthorized), 404 (Not Found), and 413 (Request Entity Too Large) are not retried — they indicate a permanent error that must be fixed in the SDK configuration or event payload
- **Exponential backoff:** Most SDKs use exponential backoff between retries to avoid overwhelming the Gateway during outages

### Per-SDK Retry Configuration

| SDK | Max Retries Default | Backoff Strategy | Configurable |
|-----|-------------------|-----------------|--------------|
| Node.js | 3 | Exponential | Yes (`maxRetries`) |
| Python | 3 | Linear | Yes (custom `on_error` handler) |
| Go | 10 | Exponential | Yes (`RetryAfter` function) |
| Java | 3 | Exponential | Yes (builder configuration) |
| Ruby | 3 | Exponential | Yes (custom `on_error` callback) |

### Gateway Response Codes

The Gateway returns the following HTTP status codes that affect retry behavior:

| Status Code | Meaning | SDK Retry? |
|------------|---------|------------|
| `200 OK` | Event accepted successfully | No (success) |
| `400 Bad Request` | Invalid payload format | No |
| `401 Unauthorized` | Invalid or missing write key | No |
| `404 Not Found` | Source disabled or not found | No |
| `413 Request Entity Too Large` | Batch exceeds 4 MB limit | No |
| `429 Too Many Requests` | Rate limit exceeded | Yes (with backoff) |
| `500 Internal Server Error` | Gateway processing error | Yes |
| `502 Bad Gateway` | Upstream service unavailable | Yes |
| `503 Service Unavailable` | Gateway temporarily overloaded | Yes |
| `504 Gateway Timeout` | Upstream service timeout | Yes |

> **Source:** `gateway/openapi.yaml` — Response status codes defined for all endpoints.

---

## Authentication Deep-Dive

### Write Key Basic Auth Format

All server-side SDKs use HTTP Basic Authentication. The authorization header is constructed as:

```
Authorization: Basic base64(writeKey + ":")
```

**Important:** Note the trailing colon (`:`) after the write key. This represents the empty password field in the standard `username:password` Basic Auth format. The write key is the username; the password is an empty string.

For example, given a write key of `2MkJ7xJaq4hgMFBEPVMfOacpKae`:

```
# Step 1: Construct the credentials string (writeKey + ":")
2MkJ7xJaq4hgMFBEPVMfOacpKae:

# Step 2: Base64-encode the credentials string
Mk1KN3hKYXE0aGdNRkJFUFZNZk9hY3BLYWU6

# Step 3: Set the Authorization header
Authorization: Basic Mk1KN3hKYXE0aGdNRkJFUFZNZk9hY3BLYWU6
```

All five server-side SDKs construct this header automatically from the `writeKey` parameter passed during initialization. No manual header construction is needed.

### Manual Testing with curl

For debugging and verification, you can send events directly using curl:

```bash
# Send a track event with Basic Auth
curl -X POST https://YOUR_DATA_PLANE_URL:8080/v1/track \
  -u 'YOUR_WRITE_KEY:' \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "track",
    "userId": "test-user-001",
    "event": "Test Event",
    "properties": { "source": "curl" },
    "timestamp": "2024-01-15T12:00:00.000Z",
    "messageId": "test-msg-001"
  }'

# Send a batch with Basic Auth
curl -X POST https://YOUR_DATA_PLANE_URL:8080/v1/batch \
  -u 'YOUR_WRITE_KEY:' \
  -H 'Content-Type: application/json' \
  -d '{
    "batch": [
      {
        "type": "identify",
        "userId": "test-user-001",
        "traits": { "name": "Test User" }
      },
      {
        "type": "track",
        "userId": "test-user-001",
        "event": "Test Event",
        "properties": { "source": "curl" }
      }
    ],
    "sentAt": "2024-01-15T12:00:00.000Z"
  }'
```

The `-u 'YOUR_WRITE_KEY:'` flag in curl sets the Basic Auth header with the write key as username and an empty password (note the trailing colon).

### Gateway Auth Implementation

The `writeKeyAuth` middleware in `gateway/handle_http_auth.go:24-58` processes authentication as follows:

1. Extract write key and password from the `Authorization` header via `r.BasicAuth()`
2. If Basic Auth is missing or the write key is empty, respond with `401` ("No write key in Basic Auth")
3. Look up the write key in the `enabledWriteKeySourceMap` (populated from backend-config)
4. If the write key is not found, respond with `401` ("Invalid Write Key") and record an `invalidWriteKey` stat
5. If the source is disabled, respond with `404` ("Source is disabled")
6. If valid, attach the `AuthRequestContext` (containing `SourceID`, `WorkspaceID`, `SourceCategory`, `WriteKey`) to the request context and delegate to the handler

### Error Responses

| Scenario | HTTP Status | Response Body |
|----------|------------|---------------|
| Missing or empty write key | `401 Unauthorized` | `"No write key in Basic Auth"` |
| Invalid write key | `401 Unauthorized` | `"Invalid Write Key"` |
| Source disabled | `404 Not Found` | `"Source is disabled"` |
| Valid write key, source enabled | `200 OK` | `"OK"` |

---

## context.library Verification

The following table shows the exact `context.library` values that each server-side SDK automatically includes in every event payload. The Gateway preserves these values through the entire processing pipeline without modification.

| SDK Platform | context.library.name | context.library.version | Notes |
|-------------|---------------------|------------------------|-------|
| Node.js | `analytics-node` | Installed SDK version (e.g., `2.1.0`) | Set automatically by the SDK |
| Python | `analytics-python` | Installed SDK version (e.g., `2.2.3`) | Set automatically by the SDK |
| Go | `analytics-go` | Installed SDK version (e.g., `3.3.0`) | Set automatically by the SDK |
| Java | `analytics-java` | Installed SDK version (e.g., `3.5.0`) | Set automatically by the SDK |
| Ruby | `analytics-ruby` | Installed SDK version (e.g., `2.4.0`) | Set automatically by the SDK |

### Verification Method

To verify that `context.library` values are preserved through the pipeline, check the event payload at your destination. The `context.library.name` field should match the SDK platform you are using, and the `context.library.version` should match the installed SDK version.

```json
{
  "context": {
    "library": {
      "name": "analytics-node",
      "version": "2.1.0"
    },
    "channel": "server"
  }
}
```

If `context.library` is missing or incorrect, verify that you are using an official Segment SDK package and not a custom HTTP client implementation.

---

## Troubleshooting

### Common Issues

| Symptom | Cause | Resolution |
|---------|-------|------------|
| **401 Unauthorized** | Invalid or missing write key | Verify the write key value is correct. Ensure the `Authorization` header uses Basic Auth format: `base64(writeKey + ":")`. Check that the trailing colon is present for the empty password. |
| **413 Request Entity Too Large** | Batch payload exceeds 4 MB Gateway limit | Reduce the SDK batch size configuration. For Node.js, lower `flushAt`/`maxEventsInBatch`. For Java, lower `.flushQueueSize()`. Consider reducing the size of `properties` and `traits` objects in your events. |
| **Events not arriving at destinations** | Incorrect host URL or network connectivity issue | Verify the `host`/`endpoint` URL includes the correct port (default: `8080`). Test connectivity with `curl -I https://YOUR_DATA_PLANE_URL:8080/version`. Check firewall rules allow outbound HTTPS on port 8080. |
| **Duplicate events at destination** | SDK retry after timeout where original request succeeded | Check `messageId` uniqueness in destination logs. Enable deduplication in the downstream pipeline if available. Verify SDK shutdown logic calls `closeAndFlush()`/`shutdown()`/`Close()` before exit. |
| **Process exits before flush** | Events still in buffer when process terminates | Always call the appropriate shutdown method before exit: Node.js: `await analytics.closeAndFlush()`, Python: `analytics.flush(); analytics.shutdown()`, Go: `client.Close()`, Java: `analytics.shutdown()`, Ruby: `analytics.flush`. |
| **Timeout errors** | Data plane unreachable or slow response | Increase the HTTP timeout in SDK configuration. Verify data plane connectivity from the server. Check if a proxy or load balancer is interfering with long-polling connections. |
| **429 Too Many Requests** | Gateway rate limit exceeded | Reduce the flush interval or batch size. Implement client-side rate limiting. Contact your RudderStack administrator to adjust Gateway rate limit configuration. |
| **Empty events or missing fields** | SDK not properly initialized | Verify that `writeKey` and `host`/`endpoint` are set before any event calls. Check that `userId` or `anonymousId` is provided for each event. |

### Diagnostic Checklist

1. **Verify connectivity:** `curl -s https://YOUR_DATA_PLANE_URL:8080/version` should return the Gateway version
2. **Verify authentication:** `curl -s -u 'YOUR_WRITE_KEY:' https://YOUR_DATA_PLANE_URL:8080/v1/batch -X POST -H 'Content-Type: application/json' -d '{"batch":[]}'` should return `200 OK`
3. **Check SDK logs:** Enable debug/verbose mode in your SDK to see HTTP request and response details
4. **Inspect batch payloads:** Log the outbound batch JSON to verify event structure before sending
5. **Monitor Gateway health:** Check `/health` endpoint for Gateway readiness status

---

## Cross-References

- **Master Migration Guide:** [Segment SDK Migration Guide](./segment-sdk-migration.md) — Complete per-SDK migration instructions with endpoint URL and write key substitution
- **Server-Side Sources Guide:** [Server-Side SDK Integration Guide](../sources/server-side-sdks.md) — Detailed installation, initialization, and event method documentation for all five SDKs
- **SDK Swap Guide:** [SDK Swap Guide: Segment to RudderStack](../migration/sdk-swap-guide.md) — Step-by-step package replacement and initialization change guide
- **Web SDK Guide:** [Web SDK Compatibility Guide](./web-sdk-guide.md) — JavaScript / Analytics 2.0 compatibility documentation including beacon and pixel endpoints
- **Mobile SDK Guide:** [Mobile SDK Compatibility Guide](./mobile-sdk-guide.md) — iOS and Android SDK compatibility documentation including lifecycle events and context auto-collection
