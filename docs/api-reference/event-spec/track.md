# Track

The **Track** API call is how you record any actions your users perform, along with any properties that describe the action. Each action is known as an **event**. Each event has a name (for example, `"User Registered"`) and a dictionary of properties (for example, `plan`, `accountType`). Calling Track is one of the first steps to instrumenting your application with RudderStack.

> **Segment Behavioral Parity:** RudderStack's Track call is **fully compatible** with the Segment Track spec. Event names, properties, reserved fields, and semantic events follow identical conventions. Any payload that works with Segment's `/v1/track` endpoint works identically with RudderStack — no field-level modifications are required.

> **Source references:**
>
> - `gateway/openapi.yaml:75-134` — OpenAPI 3.0.3 endpoint definition for `POST /v1/track`
> - `gateway/handle_http.go:42-44` — `webTrackHandler` wires `callType("track", writeKeyAuth(webHandler()))`
> - `refs/segment-docs/src/connections/spec/track.md` — Segment's canonical Track specification

For the fields shared across all event types (such as `anonymousId`, `context`, `integrations`, `timestamp`), see [Common Fields](common-fields.md).

For authentication details required to send events, see the [API Overview & Authentication](../index.md).

---

## HTTP API

### Endpoint Details

| Property | Value |
|----------|-------|
| **Method** | `POST` |
| **Path** | `/v1/track` |
| **Authentication** | Basic Auth with Write Key (`writeKeyAuth`) |
| **Content-Type** | `application/json` |
| **Default Port** | `8080` (Gateway) |

Source: `gateway/openapi.yaml:75-134` — Track endpoint definition  
Source: `gateway/openapi.yaml:678-682` — `writeKeyAuth` security scheme (HTTP Basic)

**Authentication:** Send your source **Write Key** as the HTTP Basic Auth username with an empty password. The `curl` flag `-u "YOUR_WRITE_KEY:"` handles encoding automatically. For programmatic access, Base64-encode the string `YOUR_WRITE_KEY:` (note the trailing colon) and send it as `Authorization: Basic <encoded>`.

For full details on all five authentication schemes, see [API Overview & Authentication](../index.md#authentication).

### Response Codes

| Status Code | Description | Example Response |
|-------------|-------------|------------------|
| **200** | OK — Event accepted successfully | `"OK"` |
| **400** | Bad Request — Invalid payload or missing required fields | `"Invalid request"` |
| **401** | Unauthorized — Invalid or missing Write Key | `"Invalid Authorization Header"` |
| **404** | Not Found — Source does not accept events | `"Source does not accept webhook events"` |
| **413** | Request Entity Too Large — Payload exceeds size limit | `"Request size too large"` |
| **429** | Too Many Requests — Rate limit exceeded | `"Too many requests"` |

Source: `gateway/openapi.yaml:90-132` — Response definitions for the Track endpoint

---

## Fields

The Track call accepts the following fields in the JSON request body. The `TrackPayload` schema is defined in the OpenAPI specification.

Source: `gateway/openapi.yaml:722-755` — `TrackPayload` schema definition

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | String | Yes | Must be `"track"`. Identifies this payload as a Track event. |
| `userId` | String | Optional* | Unique identifier for the user in your database. ***Either `userId` or `anonymousId` is required.** |
| `anonymousId` | String | Optional* | A pseudonymous identifier for users who have not yet been identified. ***Either `userId` or `anonymousId` is required.** |
| `event` | String | **Yes** | Name of the action the user performed. Use a human-readable name such as `"Product Viewed"` or `"Order Completed"`. See [Event Naming](#event-naming) below. |
| `properties` | Object | Optional | Free-form dictionary of properties associated with the event. See [Properties](#properties) below. |
| `context` | Object | Optional | Dictionary of extra context about the event (e.g., `ip`, `library`, `page`, `userAgent`). See [Common Fields — Context](common-fields.md#context). |
| `timestamp` | String (ISO 8601) | Optional | ISO 8601 date string indicating when the event occurred. If omitted, the server uses the time of receipt. |
| `integrations` | Object | Optional | Dictionary of boolean flags to selectively enable or disable specific destinations. For example, `{"All": true, "Mixpanel": false}`. |
| `messageId` | String | Optional | Unique identifier for this message. If omitted, the server generates one automatically. Useful for deduplication. |

Source: `gateway/openapi.yaml:725-727` — `userId` field  
Source: `gateway/openapi.yaml:728-732` — `anonymousId` field  
Source: `gateway/openapi.yaml:733-735` — `event` field  
Source: `gateway/openapi.yaml:736-738` — `properties` field  
Source: `gateway/openapi.yaml:739-751` — `context` field  
Source: `gateway/openapi.yaml:752-755` — `timestamp` field

> **Identity Resolution:** At least one of `userId` or `anonymousId` must be present in every Track call. When both are provided, RudderStack associates the anonymous activity with the known user profile. See [Common Fields — Identities](common-fields.md#identities) for details.

---

## Event Naming

Every Track call records a single user action. RudderStack (like Segment) recommends human-readable event names that everyone on your team can understand instantly.

### Best Practices

- **Use the Object + Past Tense Verb pattern** — Compose event names from a noun (the object) and a past-tense verb (the action). For example: `"Product Viewed"`, `"Order Completed"`, `"User Registered"`, `"Article Bookmarked"`.
- **Be specific and descriptive** — A name like `"Video Recorded"` is far more useful than `"Event 12"` or `"TMDropd"`.
- **Use Title Case** — Capitalize each word for consistency: `"Checkout Started"`, not `"checkout_started"`.
- **Keep names unique** — Each event name should map to exactly one user action. Avoid reusing generic names for different actions.

### Anti-Patterns to Avoid

| ❌ Avoid | ✅ Use Instead |
|----------|---------------|
| `Event 12` | `Product Added` |
| `TMDropd` | `Campaign Dropped` |
| `click` | `Button Clicked` |
| `purchase` | `Order Completed` |

> **Segment Parity:** RudderStack uses identical event naming conventions to Segment. Event names are case-sensitive strings with no enforced format, but the Object + Past Tense Verb pattern is strongly recommended for both platforms.

For events that have special semantic meaning and standardized property schemas, see [Semantic Events](#semantic-events) below.

---

## Properties

Properties are extra pieces of information you attach to a Track event. They can be any key-value pairs that are useful when analyzing events downstream. RudderStack recommends sending properties whenever possible because they give you a more complete picture of what your users are doing.

### Custom Properties

You can send any arbitrary properties with your Track calls. There are no restrictions on property names (other than JSON key validity), and values can be strings, numbers, booleans, arrays, or nested objects.

```json
{
  "properties": {
    "product_id": "P001",
    "name": "Running Shoes",
    "category": "Footwear",
    "price": 99.99,
    "in_stock": true,
    "tags": ["sale", "featured"]
  }
}
```

### Reserved Properties

RudderStack has standardized the following reserved properties that have special semantic meanings. These are handled in special ways by destination integrations — for example, the `revenue` property is automatically forwarded to revenue-tracking tools.

| Property | Type | Description |
|----------|------|-------------|
| `revenue` | Number | Amount of revenue an event resulted in. This should be a decimal value stripped of currency symbols — a shirt worth $19.99 would result in a `revenue` of `19.99`. Used by e-commerce and revenue-tracking destinations. |
| `currency` | String | Currency of the revenue in [ISO 4217](https://en.wikipedia.org/wiki/ISO_4217) format (e.g., `"USD"`, `"EUR"`, `"GBP"`). If not set, defaults to `"USD"`. |
| `value` | Number | An abstract "value" to associate with an event. Typically used when the event does not generate real-dollar revenue but has intrinsic value to a marketing team, such as newsletter signups. **Do not use for e-commerce** — use `revenue` instead. |

Source: `refs/segment-docs/src/connections/spec/track.md:110-114` — Reserved properties table from Segment specification

> **Important:** Use reserved properties **only** for their intended meanings. RudderStack (like Segment) automatically handles destination-specific conversions for these fields. For example, you do not need to call Mixpanel's `track_charges` method separately — just pass `revenue` and RudderStack will handle the conversion automatically.

---

## Sending Traits in a Track Call

All events can include additional event data in the `context` object. In some scenarios, your team may want to include user traits in a Track event — for example, when a single event needs to trigger multiple downstream actions in an Actions-based destination.

Since user traits are not a standard top-level field for Track events, you pass them inside the `context.traits` object:

```json
{
  "type": "track",
  "event": "Button Clicked",
  "properties": {
    "label": "Sign Up"
  },
  "context": {
    "traits": {
      "username": "peter_gibbons",
      "email": "peter@example.com"
    }
  }
}
```

### How It Works

By adding traits to `context.traits`, downstream Actions destinations can reference those fields in their mappings. For example, you can build:

1. **A Track action** that fires on `Event Name is "Button Clicked"`
2. **An Identify action** that fires on `Event Name is "Button Clicked" AND context.traits exists`

Both actions have access to the `context.traits` fields within their mappings, allowing a single event to trigger both a behavioral track and a profile enrichment in the downstream destination.

> **Identity Profiles Require Identify Calls:** Adding traits to a Track call via `context.traits` lets you send that data to downstream destinations, but it **does not** update the user's profile in the identity resolution system. To update user traits in the identity graph, use an explicit [Identify](identify.md) call instead.

For detailed documentation on the `context` object and all its fields, see [Common Fields — Context](common-fields.md#context).

---

## Semantic Events

RudderStack (like Segment) defines **semantic event specifications** for specific business domains. Semantic events have standardized names and property schemas that ensure consistent mapping across all downstream destinations.

When you use semantic event names, RudderStack automatically maps the event and its properties to the equivalent functionality in each destination — you do not need to handle destination-specific formatting.

### E-commerce Events

Events for tracking online shopping behavior:

| Event Name | Description |
|-----------|-------------|
| `Product Viewed` | User viewed a product detail page |
| `Product Added` | User added a product to cart |
| `Product Removed` | User removed a product from cart |
| `Cart Viewed` | User viewed their shopping cart |
| `Checkout Started` | User initiated the checkout process |
| `Checkout Step Completed` | User completed a step in checkout |
| `Order Completed` | User completed a purchase |
| `Order Refunded` | An order was refunded |
| `Coupon Applied` | User applied a coupon code |
| `Product List Viewed` | User viewed a list/category of products |

### Mobile Lifecycle Events

Events for tracking mobile application lifecycle:

| Event Name | Description |
|-----------|-------------|
| `Application Installed` | User installed the application |
| `Application Opened` | User launched the application |
| `Application Updated` | Application was updated to a new version |
| `Application Backgrounded` | User sent the application to the background |
| `Application Crashed` | Application crashed |
| `Push Notification Received` | Device received a push notification |
| `Push Notification Tapped` | User tapped a push notification |
| `Deep Link Opened` | User opened a deep link |

### Email Events

Events for tracking email engagement:

| Event Name | Description |
|-----------|-------------|
| `Email Delivered` | Email was delivered to the recipient |
| `Email Opened` | Recipient opened the email |
| `Email Link Clicked` | Recipient clicked a link in the email |
| `Email Bounced` | Email bounced (hard or soft) |
| `Email Marked as Spam` | Recipient marked the email as spam |
| `Unsubscribed` | Recipient unsubscribed from emails |

### Video Events

Events for tracking video playback:

| Event Name | Description |
|-----------|-------------|
| `Video Playback Started` | User started playing a video |
| `Video Playback Paused` | User paused video playback |
| `Video Playback Resumed` | User resumed video playback |
| `Video Playback Completed` | Video playback completed |
| `Video Content Playing` | Video content is actively playing (heartbeat) |
| `Video Ad Started` | A video advertisement started |
| `Video Ad Completed` | A video advertisement completed |

> **Note:** These standardized names ensure consistent mapping across downstream destinations. When you use these exact event names, RudderStack (and Segment-compatible tools) automatically recognize and process them with their respective property schemas.

---

## Examples

### Minimal Track Payload

The simplest Track call requires only the `type`, `event`, and at least one identity field:

```json
{
  "type": "track",
  "event": "User Registered",
  "properties": {
    "plan": "Pro Annual",
    "accountType": "Facebook"
  }
}
```

### JavaScript SDK Example

```javascript
analytics.track("User Registered", {
  plan: "Pro Annual",
  accountType: "Facebook"
});
```

### curl Example

```bash
curl -X POST http://localhost:8080/v1/track \
  -u "YOUR_WRITE_KEY:" \
  -H "Content-Type: application/json" \
  -d '{
    "userId": "user123",
    "event": "Product Viewed",
    "properties": {
      "product_id": "P001",
      "name": "Running Shoes",
      "price": 99.99,
      "currency": "USD"
    }
  }'
```

> **Note:** The `-u "YOUR_WRITE_KEY:"` flag sends the Write Key as the HTTP Basic Auth username with an empty password (the trailing colon is required). Replace `YOUR_WRITE_KEY` with your actual source Write Key. The Gateway listens on port **8080** by default.

### Python SDK Example

```python
analytics.track("user123", "Product Viewed", {
    "product_id": "P001",
    "name": "Running Shoes",
    "price": 99.99,
    "currency": "USD"
})
```

### Full Track Call Payload

A complete Track call with all common fields populated:

```json
{
  "anonymousId": "23adfd82-aa0f-45a7-a756-24f2a7a4c895",
  "context": {
    "library": {
      "name": "analytics.js",
      "version": "2.11.1"
    },
    "page": {
      "path": "/academy/",
      "referrer": "",
      "search": "",
      "title": "Analytics Academy",
      "url": "https://segment.com/academy/"
    },
    "userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_11_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/46.0.2490.86 Safari/537.36",
    "ip": "108.0.78.21"
  },
  "event": "Course Clicked",
  "integrations": {},
  "messageId": "ajs-f8ca1e4de5024d9430b3928bd8ac6b96",
  "properties": {
    "title": "Intro to Analytics"
  },
  "receivedAt": "2015-12-12T19:11:01.266Z",
  "sentAt": "2015-12-12T19:11:01.169Z",
  "timestamp": "2015-12-12T19:11:01.249Z",
  "type": "track",
  "userId": "AiUGstSDIg",
  "originalTimestamp": "2015-12-12T19:11:01.152Z"
}
```

Source: `refs/segment-docs/src/connections/spec/track.md:47-76` — Complete Track call example from Segment specification

### E-commerce Track with Revenue

```bash
curl -X POST http://localhost:8080/v1/track \
  -u "YOUR_WRITE_KEY:" \
  -H "Content-Type: application/json" \
  -d '{
    "userId": "user456",
    "event": "Order Completed",
    "properties": {
      "order_id": "ORD-9823",
      "revenue": 149.97,
      "currency": "USD",
      "products": [
        {
          "product_id": "P001",
          "name": "Running Shoes",
          "price": 99.99,
          "quantity": 1
        },
        {
          "product_id": "P045",
          "name": "Sport Socks",
          "price": 24.99,
          "quantity": 2
        }
      ]
    },
    "context": {
      "ip": "203.0.113.42",
      "library": {
        "name": "http",
        "version": "1.0.0"
      }
    }
  }'
```

---

## Segment Behavioral Parity

RudderStack's Track call maintains **full behavioral parity** with the Segment Track specification. The following table documents field-by-field parity at the payload level, as required by the platform's Segment compatibility guarantee.

| Field | Segment Behavior | RudderStack Behavior | Parity Status |
|-------|-----------------|---------------------|---------------|
| `type` | Must be `"track"` | Must be `"track"` | ✅ Full Parity |
| `event` | Required string, human-readable name | Required string, human-readable name | ✅ Full Parity |
| `properties` | Free-form object with reserved fields (`revenue`, `currency`, `value`) | Free-form object with reserved fields (`revenue`, `currency`, `value`) | ✅ Full Parity |
| `properties.revenue` | Numeric, stripped of currency symbols | Numeric, stripped of currency symbols | ✅ Full Parity |
| `properties.currency` | ISO 4217 format, defaults to `"USD"` | ISO 4217 format, defaults to `"USD"` | ✅ Full Parity |
| `properties.value` | Abstract numeric value (non-revenue) | Abstract numeric value (non-revenue) | ✅ Full Parity |
| `context` | Full context object with `ip`, `library`, `page`, `userAgent`, `traits`, etc. | Full context object with identical fields | ✅ Full Parity |
| `context.traits` | Passed to Actions destinations for downstream use | Passed to downstream destinations for downstream use | ✅ Full Parity |
| `userId` | String, at least one of `userId`/`anonymousId` required | String, at least one of `userId`/`anonymousId` required | ✅ Full Parity |
| `anonymousId` | String, at least one of `userId`/`anonymousId` required | String, at least one of `userId`/`anonymousId` required | ✅ Full Parity |
| `integrations` | Object of boolean flags per destination | Object of boolean flags per destination | ✅ Full Parity |
| `messageId` | Auto-generated if not provided | Auto-generated if not provided | ✅ Full Parity |
| `timestamp` | ISO 8601, server uses receipt time if omitted | ISO 8601, server uses receipt time if omitted | ✅ Full Parity |
| `sentAt` | ISO 8601 client clock time | ISO 8601 client clock time | ✅ Full Parity |
| `receivedAt` | Server-populated receipt timestamp | Server-populated receipt timestamp | ✅ Full Parity |
| `originalTimestamp` | Original client-side timestamp | Original client-side timestamp | ✅ Full Parity |
| Semantic events | Standardized event names with property schemas (e-commerce, mobile, email, video) | Standardized event names with identical property schemas | ✅ Full Parity |
| Event naming | Object + Past Tense Verb convention recommended | Object + Past Tense Verb convention recommended | ✅ Full Parity |
| Reserved properties | `revenue`, `currency`, `value` with auto-conversion per destination | `revenue`, `currency`, `value` with auto-conversion per destination | ✅ Full Parity |

> **Migration Note:** When migrating from Segment to RudderStack, Track calls require **zero payload modifications**. Simply change the API endpoint from `https://api.segment.io/v1/track` to `http://<your-rudderstack-gateway>:8080/v1/track` and update the Write Key to your RudderStack source Write Key. All event names, properties, and semantic events are handled identically.

Source: `gateway/openapi.yaml:75-134` — RudderStack Track endpoint definition  
Source: `refs/segment-docs/src/connections/spec/track.md` — Segment Track specification reference

---

## See Also

- [Common Fields](common-fields.md) — Shared fields across all event types (`anonymousId`, `context`, `integrations`, `timestamp`)
- [Identify](identify.md) — Associate a user with their traits and profile attributes
- [Page](page.md) — Record web page views with page-specific properties
- [Screen](screen.md) — Record mobile screen views (mobile equivalent of Page)
- [Group](group.md) — Associate a user with a group, organization, or account
- [Alias](alias.md) — Merge two user identities together
- [Gateway HTTP API](../gateway-http-api.md) — Complete HTTP API reference for all Gateway endpoints
- [API Overview & Authentication](../index.md) — API overview with all five authentication schemes
