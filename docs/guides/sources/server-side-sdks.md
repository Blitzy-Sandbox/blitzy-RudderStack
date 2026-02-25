# Server-Side SDK Integration Guide

Server-side SDKs enable you to send event data from backend applications directly to the RudderStack Customer Data Platform. This guide covers installation, initialization, event transmission, batching, and Segment compatibility for all five supported server-side languages: **Node.js**, **Python**, **Go**, **Java**, and **Ruby**.

All server-side SDKs transmit events via HTTP POST to the RudderStack Gateway on port `8080` using Basic Auth (`writeKeyAuth`). The SDKs use the same Segment-compatible API surface — only the endpoint URL and write key need to change when migrating from Segment. Events are queued in non-blocking, in-memory buffers and flushed in batches for high throughput with minimal latency impact on your application.

> **Source:** `gateway/openapi.yaml` — OpenAPI 3.0.3 specification defining all event endpoints and payload schemas.

## Prerequisites

Before integrating any server-side SDK, ensure the following:

- A **running RudderStack instance** with an accessible data plane URL (e.g., `https://your-data-plane.example.com:8080`)
- A **Source write key** obtained from your RudderStack workspace configuration
- The target **language runtime** installed on your server (Node.js 18+, Python 3.x, Go 1.18+, Java 8+, or Ruby 2.4+)

---

## Architecture

All server-side SDKs follow the same architectural pattern: events are enqueued into an in-memory buffer, batched, and flushed to the RudderStack Gateway over HTTP.

```mermaid
flowchart LR
    Server["Server Application"] -->|"SDK Enqueue"| Buffer["In-Memory Buffer"]
    Buffer -->|"Batch HTTP POST<br/>/v1/batch"| Gateway["Gateway :8080"]
    Gateway -->|"writeKeyAuth<br/>(Basic Auth)"| Auth["Authentication"]
    Auth -->|"Validated"| Processor["Processor Pipeline"]
    Processor --> Router["Router"]
    Router --> Destinations["Destinations"]
    Processor --> Warehouse["Warehouse"]
```

**Key architectural properties:**

- **Non-blocking enqueue:** SDK methods return immediately; events are placed onto an internal queue. Your application code is never blocked by network I/O.
- **Automatic batching:** Events are grouped into batches (default: 20 events per batch in most SDKs) and sent via `POST /v1/batch` to minimize HTTP overhead.
- **Periodic flushing:** If the batch threshold is not reached, the SDK flushes on a configurable timer (typically 5–10 seconds).
- **Thread-safe / goroutine-safe:** All SDKs are designed for concurrent use — multiple threads, goroutines, or async handlers can safely call SDK methods simultaneously.
- **No automatic context collection:** Unlike mobile SDKs, server-side SDKs do not auto-collect device or browser context. You supply `userId`, `traits`, and `properties` explicitly.
- **Authentication:** Every batch request includes HTTP Basic Auth with the source write key as the username and an empty password.

> **Source:** `gateway/openapi.yaml` — `/v1/batch` endpoint definition and `writeKeyAuth` security scheme.
> **Source:** `gateway/handle_http_auth.go:24-57` — `writeKeyAuth` middleware implementation validating Basic Auth credentials.

---

## SDK Quick Comparison

| Feature | Node.js | Python | Go | Java | Ruby |
|---------|---------|--------|----|------|------|
| **Package** | `@segment/analytics-node` | `segment-analytics-python` | `analytics-go/v3` | `analytics-java` | `analytics-ruby` |
| **Install** | npm / yarn / pnpm | pip | go get | Maven / Gradle | gem |
| **Min Runtime** | Node.js 18+ | Python 3.x | Go 1.18+ | Java 8+ | Ruby 2.4+ |
| **Async Model** | Built-in (event loop) | Background thread | Goroutine + channel buffer | Internal thread pool | Background thread |
| **Default Batch Size** | 20 events | 100 events | 20 events | 250 events | 100 events |
| **Default Flush Interval** | 10 s | 0.5 s | 5 s | 10 s | Queue-based |
| **Regional Endpoint** | Yes (`host`) | Yes (`host`) | Yes (`Endpoint`) | Yes (`.endpoint()`) | Yes (`host`) |

> **Reference:** `refs/segment-docs/src/connections/sources/catalog/libraries/server/node/index.md`,
> `refs/segment-docs/src/connections/sources/catalog/libraries/server/python/index.md`,
> `refs/segment-docs/src/connections/sources/catalog/libraries/server/go/index.md`,
> `refs/segment-docs/src/connections/sources/catalog/libraries/server/java/index.md`

---

## Node.js

### Installation

```bash
# npm
npm install @segment/analytics-node

# yarn
yarn add @segment/analytics-node

# pnpm
pnpm add @segment/analytics-node
```

> **Requires:** Node.js 18 or higher.

### Initialization

```javascript
const { Analytics } = require('@segment/analytics-node')

const analytics = new Analytics({
  writeKey: 'YOUR_WRITE_KEY',
  host: 'https://YOUR_DATA_PLANE_URL:8080'
})
```

Replace `YOUR_WRITE_KEY` with your RudderStack source write key and `YOUR_DATA_PLANE_URL` with your data plane hostname. The default `host` value points to `api.segment.io` — changing this to your RudderStack URL is the only modification needed for migration.

### Identify

Associate a user with their traits. Call this when a user registers, logs in, or updates their profile.

```javascript
analytics.identify({
  userId: 'user-123',
  traits: {
    name: 'Jane Doe',
    email: 'jane@example.com',
    plan: 'Enterprise'
  }
})
```

See [Identify Event Spec](../../api-reference/event-spec/identify.md) for the full payload schema.

### Track

Record a user action. The `event` field is required.

```javascript
analytics.track({
  userId: 'user-123',
  event: 'Order Completed',
  properties: {
    orderId: 'order-456',
    revenue: 99.99,
    currency: 'USD'
  }
})
```

See [Track Event Spec](../../api-reference/event-spec/track.md) for the full payload schema.

### Page

Record a server-side page view. Typically used for server-rendered pages.

```javascript
analytics.page({
  userId: 'user-123',
  name: 'Docs',
  category: 'SDK Guide',
  properties: {
    url: 'https://example.com/docs'
  }
})
```

See [Page Event Spec](../../api-reference/event-spec/page.md) for the full payload schema.

### Group

Associate a user with a company, organization, or account.

```javascript
analytics.group({
  userId: 'user-123',
  groupId: 'company-789',
  traits: {
    name: 'Acme Corp',
    industry: 'Technology'
  }
})
```

See [Group Event Spec](../../api-reference/event-spec/group.md) for the full payload schema.

### Alias

Merge two user identities (e.g., link an anonymous session to a known user).

```javascript
analytics.alias({
  userId: 'user-123',
  previousId: 'anonymous-456'
})
```

See [Alias Event Spec](../../api-reference/event-spec/alias.md) for the full payload schema.

### Flush and Shutdown

Always flush remaining events before your process exits:

```javascript
// Flush and close the client gracefully
await analytics.closeAndFlush()
```

### Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `writeKey` | string | **Required** | Source write key from your RudderStack workspace |
| `host` | string | `"https://api.segment.io"` | RudderStack data plane URL (e.g., `"https://data.example.com:8080"`) |
| `flushAt` | number | `20` | Number of events to queue before sending a batch |
| `flushInterval` | number | `10000` | Milliseconds between automatic flushes |
| `maxEventsInBatch` | number | `20` | Maximum events per single HTTP request |
| `maxRetries` | number | `3` | Maximum retry attempts for failed requests |

> **Reference:** `refs/segment-docs/src/connections/sources/catalog/libraries/server/node/index.md`

---

## Python

### Installation

```bash
pip install segment-analytics-python
```

Pin to `1.X` in your dependency manager to avoid breaking changes on major version bumps.

### Initialization

```python
import segment.analytics as analytics

analytics.write_key = 'YOUR_WRITE_KEY'
analytics.host = 'https://YOUR_DATA_PLANE_URL:8080'

# Development settings (optional)
analytics.debug = True
analytics.on_error = lambda error, items: print("Error:", error)
```

The library uses a background thread for asynchronous event flushing. Set `debug = True` and attach an `on_error` handler during development for visibility into delivery failures.

### Identify

```python
analytics.identify('user-123', {
    'name': 'Jane Doe',
    'email': 'jane@example.com',
    'plan': 'Enterprise'
})
```

See [Identify Event Spec](../../api-reference/event-spec/identify.md) for the full payload schema.

### Track

```python
analytics.track('user-123', 'Order Completed', {
    'orderId': 'order-456',
    'revenue': 99.99,
    'currency': 'USD'
})
```

See [Track Event Spec](../../api-reference/event-spec/track.md) for the full payload schema.

### Page

```python
analytics.page('user-123', 'SDK Guide', 'Docs', {
    'url': 'https://example.com/docs'
})
```

See [Page Event Spec](../../api-reference/event-spec/page.md) for the full payload schema.

### Group

```python
analytics.group('user-123', 'company-789', {
    'name': 'Acme Corp',
    'industry': 'Technology'
})
```

See [Group Event Spec](../../api-reference/event-spec/group.md) for the full payload schema.

### Alias

```python
analytics.alias('anonymous-456', 'user-123')
```

The first argument is the `previous_id` and the second is the `user_id` to alias to.

See [Alias Event Spec](../../api-reference/event-spec/alias.md) for the full payload schema.

### Flush and Shutdown

```python
analytics.flush()     # Send all queued events immediately
analytics.shutdown()  # Clean shutdown of the background thread
```

Always call `flush()` followed by `shutdown()` before your process exits to avoid data loss.

### Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `write_key` | str | **Required** | Source write key from your RudderStack workspace |
| `host` | str | `"https://api.segment.io"` | RudderStack data plane URL (e.g., `"https://data.example.com:8080"`) |
| `debug` | bool | `False` | Enable debug logging to the Python logger |
| `on_error` | callable | `None` | Error callback: `fn(error, items)` invoked on delivery failure |
| `max_queue_size` | int | `10000` | Maximum number of events in the in-memory queue |
| `upload_size` | int | `100` | Number of events per batch request |
| `upload_interval` | float | `0.5` | Seconds between automatic flushes by the background thread |
| `gzip` | bool | `False` | Enable gzip compression for outbound requests |
| `send` | bool | `True` | Set to `False` to suppress all network calls (useful in tests) |

> **Reference:** `refs/segment-docs/src/connections/sources/catalog/libraries/server/python/index.md`

---

## Go

### Installation

```bash
go get github.com/segmentio/analytics-go/v3
```

### Initialization

```go
package main

import (
    "time"
    "github.com/segmentio/analytics-go/v3"
)

func main() {
    // Simple initialization (uses default Segment endpoint)
    client := analytics.New("YOUR_WRITE_KEY")
    defer client.Close()

    // Initialization pointing to RudderStack
    client, err := analytics.NewWithConfig("YOUR_WRITE_KEY", analytics.Config{
        Endpoint:  "https://YOUR_DATA_PLANE_URL:8080",
        BatchSize: 20,
        Interval:  5 * time.Second,
    })
    if err != nil {
        panic(err)
    }
    defer client.Close()
}
```

> **Critical:** Always call `defer client.Close()` immediately after creating the client. This ensures all buffered events are flushed before your program exits.

### Identify

```go
client.Enqueue(analytics.Identify{
    UserId: "user-123",
    Traits: analytics.NewTraits().
        SetName("Jane Doe").
        SetEmail("jane@example.com").
        Set("plan", "Enterprise"),
})
```

See [Identify Event Spec](../../api-reference/event-spec/identify.md) for the full payload schema.

### Track

```go
client.Enqueue(analytics.Track{
    UserId: "user-123",
    Event:  "Order Completed",
    Properties: analytics.NewProperties().
        Set("orderId", "order-456").
        SetRevenue(99.99).
        SetCurrency("USD"),
})
```

See [Track Event Spec](../../api-reference/event-spec/track.md) for the full payload schema.

### Page

```go
client.Enqueue(analytics.Page{
    UserId:   "user-123",
    Name:     "SDK Guide",
    Category: "Docs",
    Properties: analytics.NewProperties().
        Set("url", "https://example.com/docs"),
})
```

See [Page Event Spec](../../api-reference/event-spec/page.md) for the full payload schema.

### Group

```go
client.Enqueue(analytics.Group{
    UserId:  "user-123",
    GroupId: "company-789",
    Traits: analytics.NewTraits().
        SetName("Acme Corp").
        Set("industry", "Technology"),
})
```

See [Group Event Spec](../../api-reference/event-spec/group.md) for the full payload schema.

### Alias

```go
client.Enqueue(analytics.Alias{
    PreviousId: "anonymous-456",
    UserId:     "user-123",
})
```

See [Alias Event Spec](../../api-reference/event-spec/alias.md) for the full payload schema.

### Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `WriteKey` | string | **Required** | Source write key (passed as first argument) |
| `Endpoint` | string | `"https://api.segment.io"` | RudderStack data plane URL |
| `BatchSize` | int | `20` | Number of events per batch request |
| `Interval` | time.Duration | `5s` | Maximum time between automatic flushes |
| `Verbose` | bool | `false` | Enable verbose logging of requests and responses |
| `RetryAfter` | func(int) time.Duration | Default backoff | Custom retry delay function (receives attempt number) |
| `Logger` | Logger | `nil` | Custom logger interface for SDK diagnostic output |
| `DefaultContext` | *Context | `nil` | Default context fields applied to every event |

> **Reference:** `refs/segment-docs/src/connections/sources/catalog/libraries/server/go/index.md`

---

## Java

### Installation

**Maven:**

```xml
<dependency>
    <groupId>com.segment.analytics.java</groupId>
    <artifactId>analytics</artifactId>
    <version>LATEST</version>
</dependency>
```

**Gradle:**

```groovy
implementation 'com.segment.analytics.java:analytics:+'
```

The library is distributed via [Maven Central](https://search.maven.org/artifact/com.segment.analytics.java/analytics).

### Initialization

```java
import com.segment.analytics.Analytics;

Analytics analytics = Analytics.builder("YOUR_WRITE_KEY")
    .endpoint("https://YOUR_DATA_PLANE_URL:8080")
    .build();
```

The builder pattern allows customization of queue sizes, flush intervals, logging, and thread factories.

### Identify

```java
analytics.enqueue(IdentifyMessage.builder()
    .userId("user-123")
    .traits(ImmutableMap.<String, Object>builder()
        .put("name", "Jane Doe")
        .put("email", "jane@example.com")
        .put("plan", "Enterprise")
        .build()));
```

You can use Guava `ImmutableMap` or plain `java.util.HashMap` for traits and properties.

See [Identify Event Spec](../../api-reference/event-spec/identify.md) for the full payload schema.

### Track

```java
analytics.enqueue(TrackMessage.builder("Order Completed")
    .userId("user-123")
    .properties(ImmutableMap.<String, Object>builder()
        .put("orderId", "order-456")
        .put("revenue", 99.99)
        .put("currency", "USD")
        .build()));
```

See [Track Event Spec](../../api-reference/event-spec/track.md) for the full payload schema.

### Page

```java
analytics.enqueue(PageMessage.builder("SDK Guide")
    .userId("user-123")
    .properties(ImmutableMap.<String, Object>builder()
        .put("url", "https://example.com/docs")
        .put("category", "Docs")
        .build()));
```

See [Page Event Spec](../../api-reference/event-spec/page.md) for the full payload schema.

### Group

```java
analytics.enqueue(GroupMessage.builder("company-789")
    .userId("user-123")
    .traits(ImmutableMap.<String, Object>builder()
        .put("name", "Acme Corp")
        .put("industry", "Technology")
        .build()));
```

See [Group Event Spec](../../api-reference/event-spec/group.md) for the full payload schema.

### Alias

```java
analytics.enqueue(AliasMessage.builder("anonymous-456")
    .userId("user-123"));
```

See [Alias Event Spec](../../api-reference/event-spec/alias.md) for the full payload schema.

### Flush and Shutdown

```java
analytics.flush();    // Send all queued events immediately
analytics.shutdown(); // Clean shutdown of internal thread pool
```

### Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `writeKey` | String | **Required** | Source write key (passed to `Analytics.builder()`) |
| `endpoint` | String | `"https://api.segment.io"` | RudderStack data plane URL |
| `flushQueueSize` | int | `250` | Maximum queue size before triggering a flush |
| `flushInterval` | long | `10` (seconds) | Seconds between automatic flushes |
| `retries` | int | `3` | Maximum retry attempts for failed requests |
| `networkExecutor` | ExecutorService | Default | Custom executor for network operations |
| `threadFactory` | ThreadFactory | Default | Custom thread factory for internal threads |
| `log` | Log | `NONE` | Logging implementation (`NONE`, `SystemOut`, or custom) |
| `callback` | Callback | `null` | Callback invoked after each message is processed |

> **Reference:** `refs/segment-docs/src/connections/sources/catalog/libraries/server/java/index.md`

---

## Ruby

### Installation

```bash
gem install analytics-ruby
```

Or add to your `Gemfile`:

```ruby
gem 'analytics-ruby', '~> 2.0'
```

Then run `bundle install`.

### Initialization

```ruby
require 'segment/analytics'

analytics = Segment::Analytics.new({
  write_key: 'YOUR_WRITE_KEY',
  host: 'https://YOUR_DATA_PLANE_URL:8080',
  on_error: proc { |status, msg| print("Error: #{msg}") }
})
```

### Identify

```ruby
analytics.identify(
  user_id: 'user-123',
  traits: {
    name: 'Jane Doe',
    email: 'jane@example.com',
    plan: 'Enterprise'
  }
)
```

See [Identify Event Spec](../../api-reference/event-spec/identify.md) for the full payload schema.

### Track

```ruby
analytics.track(
  user_id: 'user-123',
  event: 'Order Completed',
  properties: {
    order_id: 'order-456',
    revenue: 99.99,
    currency: 'USD'
  }
)
```

See [Track Event Spec](../../api-reference/event-spec/track.md) for the full payload schema.

### Page

```ruby
analytics.page(
  user_id: 'user-123',
  name: 'SDK Guide',
  category: 'Docs',
  properties: {
    url: 'https://example.com/docs'
  }
)
```

See [Page Event Spec](../../api-reference/event-spec/page.md) for the full payload schema.

### Group

```ruby
analytics.group(
  user_id: 'user-123',
  group_id: 'company-789',
  traits: {
    name: 'Acme Corp',
    industry: 'Technology'
  }
)
```

See [Group Event Spec](../../api-reference/event-spec/group.md) for the full payload schema.

### Alias

```ruby
analytics.alias(
  previous_id: 'anonymous-456',
  user_id: 'user-123'
)
```

See [Alias Event Spec](../../api-reference/event-spec/alias.md) for the full payload schema.

### Flush

```ruby
analytics.flush  # Send all queued events immediately
```

Always call `flush` before your Ruby process exits (e.g., in an `at_exit` hook or before `Process.exit`).

### Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `write_key` | String | **Required** | Source write key from your RudderStack workspace |
| `host` | String | `"https://api.segment.io"` | RudderStack data plane URL |
| `on_error` | Proc | `nil` | Error handler: `proc { |status, msg| ... }` |
| `stub` | Boolean | `false` | Stub mode — suppresses all network calls (useful for testing) |
| `batch_size` | Integer | `100` | Number of events per batch request |

---

## Direct HTTP API (No SDK)

For languages without an official SDK, or when you need full control over request construction, you can call the Gateway HTTP API directly. All endpoints accept JSON payloads with Basic Auth.

### Identify

```bash
curl -X POST https://YOUR_DATA_PLANE_URL:8080/v1/identify \
  -H 'Content-Type: application/json' \
  -u YOUR_WRITE_KEY: \
  -d '{
    "userId": "user-123",
    "traits": {
      "name": "Jane Doe",
      "email": "jane@example.com"
    },
    "timestamp": "2024-01-15T10:30:00Z"
  }'
```

### Track

```bash
curl -X POST https://YOUR_DATA_PLANE_URL:8080/v1/track \
  -H 'Content-Type: application/json' \
  -u YOUR_WRITE_KEY: \
  -d '{
    "userId": "user-123",
    "event": "Order Completed",
    "properties": {
      "orderId": "order-456",
      "revenue": 99.99
    },
    "timestamp": "2024-01-15T10:30:00Z"
  }'
```

### Page

```bash
curl -X POST https://YOUR_DATA_PLANE_URL:8080/v1/page \
  -H 'Content-Type: application/json' \
  -u YOUR_WRITE_KEY: \
  -d '{
    "userId": "user-123",
    "name": "SDK Guide",
    "properties": {
      "url": "https://example.com/docs",
      "category": "Docs"
    },
    "timestamp": "2024-01-15T10:30:00Z"
  }'
```

### Group

```bash
curl -X POST https://YOUR_DATA_PLANE_URL:8080/v1/group \
  -H 'Content-Type: application/json' \
  -u YOUR_WRITE_KEY: \
  -d '{
    "userId": "user-123",
    "groupId": "company-789",
    "traits": {
      "name": "Acme Corp",
      "industry": "Technology"
    },
    "timestamp": "2024-01-15T10:30:00Z"
  }'
```

### Alias

```bash
curl -X POST https://YOUR_DATA_PLANE_URL:8080/v1/alias \
  -H 'Content-Type: application/json' \
  -u YOUR_WRITE_KEY: \
  -d '{
    "userId": "user-123",
    "previousId": "anonymous-456",
    "timestamp": "2024-01-15T10:30:00Z"
  }'
```

### Batch

Send multiple events of any type in a single HTTP request:

```bash
curl -X POST https://YOUR_DATA_PLANE_URL:8080/v1/batch \
  -H 'Content-Type: application/json' \
  -u YOUR_WRITE_KEY: \
  -d '{
    "batch": [
      {
        "type": "identify",
        "userId": "user-1",
        "traits": {"name": "Jane Doe"}
      },
      {
        "type": "track",
        "userId": "user-1",
        "event": "Login",
        "properties": {}
      },
      {
        "type": "page",
        "userId": "user-1",
        "name": "Dashboard",
        "properties": {"url": "https://example.com/dashboard"}
      }
    ]
  }'
```

Each item in the `batch` array must include a `type` field (`identify`, `track`, `page`, `screen`, `group`, or `alias`) along with the corresponding payload fields.

> **Source:** `gateway/openapi.yaml` — All endpoint definitions (`/v1/identify`, `/v1/track`, `/v1/page`, `/v1/screen`, `/v1/group`, `/v1/alias`, `/v1/batch`) and the `writeKeyAuth` security scheme.

---

## Authentication

All server-side SDKs and direct HTTP API calls use the **writeKeyAuth** scheme defined in the Gateway's OpenAPI specification.

| Property | Value |
|----------|-------|
| **Type** | HTTP Basic Authentication |
| **Username** | Your source write key |
| **Password** | Empty string |
| **HTTP Header** | `Authorization: Basic <base64(writeKey:)>` |

Each SDK handles authentication automatically when you provide the write key during initialization. For direct HTTP calls, use `-u YOUR_WRITE_KEY:` with curl (the trailing colon denotes an empty password).

**Example — manual header construction:**

```
Write Key: 2MmVGaVSQXmPMnPaBXeesQBxKgj
Base64:    base64("2MmVGaVSQXmPMnPaBXeesQBxKgj:")
Header:    Authorization: Basic Mk1tVkdhVlNRWG1QTW5QYUJYZWVzUUJ4S2dqOg==
```

The Gateway validates the write key against its internal source configuration map. If the key is missing, empty, or does not correspond to an enabled source, the Gateway returns `401 Unauthorized`.

> **Source:** `gateway/openapi.yaml` — `writeKeyAuth` security scheme (`type: http`, `scheme: basic`).
> **Source:** `gateway/handle_http_auth.go:24-57` — `writeKeyAuth` middleware: extracts write key via `r.BasicAuth()`, validates against `writeKeysSourceMap`, checks source is enabled.

---

## Batching and Performance

All server-side SDKs implement internal event queues for non-blocking, high-throughput operation. Understanding the batching behavior across SDKs helps you tune performance for your workload.

### Batching Parameters by SDK

| SDK | Default Batch Size | Default Flush Interval | Queue Type | Thread Safety |
|-----|-------------------|----------------------|------------|---------------|
| **Node.js** | 20 events | 10 s | In-memory array | Single-threaded (Node.js event loop) |
| **Python** | 100 events | 0.5 s | In-memory list | Background thread with `threading.Lock` |
| **Go** | 20 events | 5 s | Buffered channel | Goroutine-safe channel operations |
| **Java** | 250 events | 10 s | Internal `BlockingQueue` | Thread-safe via concurrent queue |
| **Ruby** | 100 events | Queue-based | In-memory array | Thread-safe with mutex |

### Best Practices

- **Always call flush/close/shutdown before process exit.** Failing to do so will lose any events still in the in-memory buffer. Use `defer` (Go), `at_exit` (Ruby), `atexit` (Python), shutdown hooks (Java), or `process.on('exit')` (Node.js).
- **Tune batch size for your event volume.** For high-volume services (>1000 events/sec), increase the batch size to reduce HTTP overhead. For low-volume services, reduce the flush interval to avoid stale data.
- **Monitor for `429 Too Many Requests` responses.** The Gateway returns HTTP 429 when rate limits are exceeded. SDKs retry automatically with exponential backoff, but persistent 429s indicate you should increase the Gateway's rate limit configuration or scale horizontally.
- **Enable gzip compression** (where supported — e.g., Python's `gzip=True`) to reduce bandwidth usage for large payloads.

---

## Error Handling

The Gateway returns standard HTTP response codes for all event endpoints. SDKs surface errors through callbacks, loggers, or error handlers.

### Response Codes

| Code | Status | Response Example | Action |
|------|--------|-----------------|--------|
| `200` | OK | `"OK"` | Event accepted and queued for processing |
| `400` | Bad Request | `"Invalid request"` | Check payload format; ensure required fields are present (`event` for track, `groupId` for group) |
| `401` | Unauthorized | `"Invalid Authorization Header"` | Verify write key is correct and Basic Auth header is properly formed |
| `404` | Not Found | `"Source does not accept webhook events"` | Check the endpoint URL path; ensure the source type supports the event endpoint |
| `413` | Request Entity Too Large | `"Request size too large"` | Reduce batch size or individual event payload size |
| `429` | Too Many Requests | `"Too many requests"` | Gateway rate limit exceeded; SDKs retry automatically with backoff |

### Per-SDK Error Handling

| SDK | Error Surface | Example |
|-----|--------------|---------|
| **Node.js** | Promise rejection / `on('error')` | `analytics.on('error', (err) => console.error(err))` |
| **Python** | `on_error` callback | `analytics.on_error = lambda err, items: log(err)` |
| **Go** | `Callback` interface on Config | `Config{ Callback: myCallback }` |
| **Java** | `Callback` on builder | `.callback(new LoggingCallback())` |
| **Ruby** | `on_error` proc | `on_error: proc { |status, msg| log(msg) }` |

> **Source:** `gateway/openapi.yaml` — Response definitions for all endpoints (200, 400, 401, 404, 413, 429).

---

## Migrating from Segment

Server-side SDKs are the most straightforward to migrate from Segment to RudderStack. The SDKs use the same Segment-compatible libraries — only the endpoint URL and write key need to change.

### Universal Migration Steps

1. **Change the endpoint/host** from `api.segment.io` to your RudderStack data plane URL
2. **Replace the Segment write key** with your RudderStack source write key
3. **No changes to event calls** — `identify`, `track`, `page`, `group`, and `alias` work identically
4. **Test with a single event** to verify connectivity (use the curl examples in the [Direct HTTP API](#direct-http-api-no-sdk) section)

### Language-Specific Migration Diffs

**Node.js:**
```diff
  const analytics = new Analytics({
    writeKey: 'YOUR_RUDDERSTACK_WRITE_KEY',
-   host: 'https://api.segment.io'
+   host: 'https://YOUR_DATA_PLANE_URL:8080'
  })
```

**Python:**
```diff
- analytics.write_key = 'SEGMENT_WRITE_KEY'
+ analytics.write_key = 'RUDDERSTACK_WRITE_KEY'
+ analytics.host = 'https://YOUR_DATA_PLANE_URL:8080'
```

**Go:**
```diff
  client, _ := analytics.NewWithConfig("YOUR_RUDDERSTACK_WRITE_KEY", analytics.Config{
-     Endpoint: "https://api.segment.io",
+     Endpoint: "https://YOUR_DATA_PLANE_URL:8080",
  })
```

**Java:**
```diff
  Analytics analytics = Analytics.builder("YOUR_RUDDERSTACK_WRITE_KEY")
-     .endpoint("https://api.segment.io")
+     .endpoint("https://YOUR_DATA_PLANE_URL:8080")
      .build();
```

**Ruby:**
```diff
  analytics = Segment::Analytics.new({
    write_key: 'YOUR_RUDDERSTACK_WRITE_KEY',
-   host: 'https://api.segment.io'
+   host: 'https://YOUR_DATA_PLANE_URL:8080'
  })
```

For a comprehensive walkthrough covering SDK swap, destination re-mapping, and validation, see the [SDK Swap Migration Guide](../migration/sdk-swap-guide.md).

For a detailed comparison of Segment and RudderStack source SDK capabilities, see the [Source Catalog Parity Analysis](../../gap-report/source-catalog-parity.md).

---

## Related Documentation

### Event Spec Reference
- [Common Fields](../../api-reference/event-spec/common-fields.md) — Shared fields present in all event types (userId, anonymousId, context, timestamp, integrations)
- [Identify](../../api-reference/event-spec/identify.md) — Identify call specification and payload schema
- [Track](../../api-reference/event-spec/track.md) — Track call specification and payload schema
- [Page](../../api-reference/event-spec/page.md) — Page call specification and payload schema
- [Group](../../api-reference/event-spec/group.md) — Group call specification and payload schema
- [Alias](../../api-reference/event-spec/alias.md) — Alias call specification and payload schema

### API Reference
- [Gateway HTTP API Reference](../../api-reference/gateway-http-api.md) — Full HTTP API reference for all Gateway endpoints

### Migration
- [SDK Swap Migration Guide](../migration/sdk-swap-guide.md) — Step-by-step guide for replacing Segment SDKs with RudderStack
- [Source Catalog Parity Analysis](../../gap-report/source-catalog-parity.md) — Detailed comparison of Segment and RudderStack source capabilities

### Other SDK Guides
- [JavaScript Web SDK Guide](./javascript-sdk.md) — Browser-side event tracking
- [iOS SDK Guide](./ios-sdk.md) — iOS and Apple platform event tracking
- [Android SDK Guide](./android-sdk.md) — Android mobile event tracking
