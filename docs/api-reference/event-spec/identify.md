# Identify

The Identify call lets you associate a visiting user to their actions and record any associated traits about them. It includes a unique User ID and/or Anonymous ID, plus any optional traits you know about the user — such as their email, name, account plan, or login count.

When you make an Identify call, RudderStack saves the user traits to the persistent job queue and forwards them to all enabled destinations, where the user profile is created or updated accordingly.

> **Source references:**
>
> - `gateway/openapi.yaml:15-74` — POST `/v1/identify` endpoint definition and response codes
> - `gateway/handle_http.go:37-39` — `webIdentifyHandler` wires `callType("identify", writeKeyAuth(webHandler()))`
> - `gateway/types.go:19-31` — `webRequestT` struct used for all incoming web requests

> **Segment Behavioral Parity:** RudderStack's Identify call is fully compatible with the Segment Identify specification. Payload fields, trait handling, and identity resolution follow identical semantics. Any payload accepted by Segment's `/v1/identify` endpoint is accepted and processed identically by RudderStack.
>
> Reference: `refs/segment-docs/src/connections/spec/identify.md` — Segment's canonical Identify specification

**When to make an Identify call:**

- **After a user first registers** — capture their initial profile traits (name, email, plan)
- **After a user logs in** — associate the anonymous session with the known user
- **When a user updates their info** — for example, they change or add a new address, update their plan, or modify contact details

For shared fields common to all event types (such as `anonymousId`, `context`, `integrations`, `messageId`, and timestamp fields), see [Common Fields](common-fields.md).

---

## HTTP API

### Endpoint Details

| Property | Value |
|----------|-------|
| **Method** | `POST` |
| **Path** | `/v1/identify` |
| **Port** | `8080` (Gateway default) |
| **Authentication** | Basic Auth with WriteKey (`writeKeyAuth`) |
| **Content-Type** | `application/json` |

Source: `gateway/openapi.yaml:15-74`

The Identify endpoint uses **WriteKey Basic Authentication** — the source Write Key is sent as the username in an HTTP Basic Authentication header, with the password left empty. For full authentication details including all five auth schemes, see [API Overview & Authentication](../index.md).

### Response Codes

| HTTP Code | Status | Description |
|-----------|--------|-------------|
| 200 | OK | Request successfully processed and enqueued for downstream delivery |
| 400 | Bad Request | Invalid request format, missing required fields, or malformed JSON payload |
| 401 | Unauthorized | Missing or invalid WriteKey in the Authorization header |
| 404 | Not Found | Source does not accept webhook events or source is disabled |
| 413 | Request Entity Too Large | Request payload size exceeds the configured maximum limit |
| 429 | Too Many Requests | Rate limit exceeded — retry after the indicated backoff period |

Source: `gateway/openapi.yaml:30-74`

---

## Fields

The Identify call accepts the following fields. In addition to these Identify-specific fields, all [common fields](common-fields.md) (such as `anonymousId`, `context`, `integrations`, `messageId`, `sentAt`, `receivedAt`) are also accepted.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | String | **Yes** | Must be `"identify"`. Identifies this event as an Identify call. |
| `userId` | String | Optional\* | Unique identifier for the user in your database. \*Either `userId` or `anonymousId` is required. |
| `anonymousId` | String | Optional\* | A pseudo-unique identifier for cases where there is no unique user identifier. \*Either `userId` or `anonymousId` is required. |
| `traits` | Object | Optional | Dictionary of traits you know about the user. Common traits include `email`, `name`, `plan`, etc. See [Traits](#traits) below. |
| `context` | Object | Optional | Dictionary of extra information providing context about the event. Contains nested objects such as `traits` (context-level traits), `ip` (String), and `library` (Object with `name`). See [Common Fields](common-fields.md) for the full context specification. |
| `timestamp` | String (ISO 8601) | Optional | The timestamp of the message's creation. Format: ISO 8601 date-time (e.g., `"2024-01-15T10:30:00.000Z"`). |
| `integrations` | Object | Optional | Dictionary of destinations to enable or disable for this specific event. Use `"All": false` to disable all, then selectively enable. |
| `messageId` | String | Optional | Unique identifier for this message. Auto-generated as a UUID if not provided by the client. |

Source: `gateway/openapi.yaml:688-721` — `IdentifyPayload` schema definition

> **Important:** Every Identify call **must** include either a `userId` or an `anonymousId` (or both). Requests containing neither will be rejected with a `400 Bad Request` error (`"request neither has anonymousId nor userId"`).

> **Segment Parity Annotation:** The RudderStack `IdentifyPayload` schema is structurally identical to Segment's Identify payload. All fields listed above are accepted and routed to downstream destinations with the same semantics. No field-level modifications are required when migrating from Segment.

---

## Traits

Traits are pieces of information you know about a user that are included in an Identify call. These could be demographics like `age` or `gender`, account-specific attributes like `plan`, or behavioral indicators like `logins`.

RudderStack treats certain trait names with **semantic meaning**, matching Segment's reserved traits. These reserved traits are handled specially by downstream destinations — for example, the `email` trait is used by destinations like Mailchimp that require an email address for their tracking.

### Reserved Traits

The following traits are standardized and have semantic meaning. RudderStack forwards them to downstream destinations using the same field mappings as Segment.

| Trait | Type | Description |
|-------|------|-------------|
| `address` | Object | Street address of the user, optionally containing: `street`, `city`, `state`, `postalCode`, `country` |
| `age` | Number | Age of the user |
| `avatar` | String | URL to an avatar image for the user |
| `birthday` | Date | The user's birthday (ISO 8601 date format) |
| `company` | Object | The company the user represents, optionally containing: `name` (String), `id` (String or Number), `industry` (String), `employee_count` (Number), `plan` (String) |
| `createdAt` | Date | Date the user's account was first created (ISO 8601 date string recommended) |
| `description` | String | Description of the user |
| `email` | String | Email address of the user |
| `firstName` | String | First name of the user |
| `gender` | String | Gender of the user |
| `id` | String | Unique ID in your database for the user |
| `lastName` | String | Last name of the user |
| `name` | String | Full name of the user. If you only pass `firstName` and `lastName`, the full name can be automatically composed. |
| `phone` | String | Phone number of the user |
| `title` | String | Title of the user, usually related to their position at a specific company (e.g., "VP of Engineering") |
| `username` | String | Username of the user. This should be unique to each user, like Twitter or GitHub usernames. |
| `website` | String | Website of the user |

Source: `refs/segment-docs/src/connections/spec/identify.md:143-161` — Segment reserved trait definitions

> **Segment Parity Note:** RudderStack accepts all Segment reserved traits with identical semantics. Traits are forwarded to downstream destinations using the same field mappings. RudderStack automatically handles destination-specific trait name conversions (e.g., `createdAt` → `$created` for Mixpanel, `createdAt` → `created_at` for Intercom).

> **Warning:** Use reserved traits only for their intended semantics. You can pass these reserved traits using either camelCase (`firstName`) or snake_case (`first_name`) to match your codebase conventions. However, sending the same reserved trait in both camelCase and snake_case may create duplicate trait entries in some downstream destinations.

In addition to reserved traits, you can send any **custom traits** as key-value pairs in the `traits` object. Custom traits are forwarded to all enabled destinations and stored in your data warehouse.

---

## Examples

### Minimal Identify Payload

A basic Identify call with a `userId` and a few traits:

```json
{
  "type": "identify",
  "traits": {
    "name": "Peter Gibbons",
    "email": "peter@example.com",
    "plan": "premium",
    "logins": 5
  },
  "userId": "97980cfea0067"
}
```

### curl Example

Send an Identify call to the RudderStack Gateway using curl with Basic Auth:

```bash
curl -X POST http://localhost:8080/v1/identify \
  -u "YOUR_WRITE_KEY:" \
  -H "Content-Type: application/json" \
  -d '{
    "userId": "97980cfea0067",
    "traits": {
      "name": "Peter Gibbons",
      "email": "peter@example.com",
      "plan": "premium",
      "logins": 5
    }
  }'
```

> **Note:** The `-u "YOUR_WRITE_KEY:"` flag sends the Write Key as the Basic Auth username with an empty password (note the trailing colon). Replace `YOUR_WRITE_KEY` with your actual source Write Key. The Gateway listens on port **8080** by default.

### JavaScript SDK Example

Using the RudderStack JavaScript SDK (or any Segment-compatible analytics.js library):

```javascript
analytics.identify("97980cfea0067", {
  name: "Peter Gibbons",
  email: "peter@example.com",
  plan: "premium",
  logins: 5
});
```

### Anonymous Identify Example

When you don't know the user's identity but want to record traits for later merging:

```javascript
analytics.identify({
  subscriptionStatus: "inactive"
});
```

In this case, the SDK automatically generates an `anonymousId` and associates the traits with the anonymous user. When the user later logs in, the Identify call with a `userId` merges the anonymous profile.

### Full Identify Call Payload

A complete Identify call with all common fields included:

```json
{
  "anonymousId": "507f191e810c19729de860ea",
  "channel": "browser",
  "context": {
    "ip": "8.8.8.8",
    "userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_9_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/40.0.2214.115 Safari/537.36"
  },
  "integrations": {
    "All": false,
    "Mixpanel": true,
    "Salesforce": true
  },
  "messageId": "022bb90c-bbac-11e4-8dfc-aa07a5b093db",
  "receivedAt": "2015-02-23T22:28:55.387Z",
  "sentAt": "2015-02-23T22:28:55.111Z",
  "timestamp": "2015-02-23T22:28:55.111Z",
  "traits": {
    "name": "Peter Gibbons",
    "email": "peter@example.com",
    "plan": "premium",
    "logins": 5,
    "address": {
      "street": "6th St",
      "city": "San Francisco",
      "state": "CA",
      "postalCode": "94103",
      "country": "USA"
    }
  },
  "type": "identify",
  "userId": "97980cfea0067",
  "version": "1.1"
}
```

Source: `refs/segment-docs/src/connections/spec/identify.md:59-92`

**Field Notes for the Full Example:**

| Field | Description |
|-------|-------------|
| `anonymousId` | Pre-existing anonymous session ID, will be merged with the `userId` |
| `channel` | The channel the event originated from (e.g., `"browser"`, `"server"`, `"mobile"`) |
| `context.ip` | Client IP address, used for geolocation enrichment |
| `context.userAgent` | Browser user agent string, parsed for device/browser information |
| `integrations` | Selectively routes this event to Mixpanel and Salesforce only (all others disabled) |
| `messageId` | Client-generated UUID for deduplication |
| `receivedAt` | Server-side timestamp when the Gateway received the event |
| `sentAt` | Client-side timestamp when the event was dispatched |
| `timestamp` | Computed timestamp: `receivedAt - (sentAt - originalTimestamp)` |
| `traits.address` | Nested address object using the reserved `address` trait structure |
| `version` | Spec version (currently `"1.1"`) |

---

## Identities

Every Identify call must have either a **User ID** or an **Anonymous ID** (or both). These identifiers are used to stitch together a unified user profile across sessions, devices, and channels.

### Anonymous ID

An Anonymous ID is used when you cannot identify the user with a permanent database identifier. This is common in scenarios such as:

- Newsletter signups before account creation
- Anonymous page views and browsing sessions
- Pre-authentication mobile app usage

The Anonymous ID can be any pseudo-unique identifier — a session ID, a cookie value, or a randomly generated UUID. RudderStack recommends using **UUIDv4 format** for consistency.

> **SDK Auto-Generation:** RudderStack's web and mobile SDKs (JavaScript, iOS, Android) automatically generate and manage Anonymous IDs. You do not need to set them manually when using these SDKs — the library creates a persistent anonymous identifier and includes it in every event until the user is identified.

**Example — Anonymous Identify call:**

```javascript
// The SDK automatically generates and attaches an anonymousId
analytics.identify({
  subscriptionStatus: "inactive"
});
```

### User ID

A User ID is a permanent, database-level identifier that uniquely represents a user across their entire lifecycle. It persists across sessions, devices, and channels, making it the most reliable identifier for user profile unification.

**Best practices for User IDs:**

- Use your database primary key (e.g., MongoDB ObjectId, PostgreSQL serial/UUID)
- Prefer UUIDv4 format for new systems
- **Do not** use email addresses as User IDs — email addresses can change, breaking profile continuity. Instead, send email as a `trait`
- **Do not** use usernames as User IDs — usernames may change or be recycled

**Example — Identified user:**

```javascript
analytics.identify("97980cfea0067", {
  name: "Peter Gibbons",
  email: "peter@example.com"
});
```

> **Segment Parity:** RudderStack handles Anonymous ID and User ID identically to Segment. Identity stitching follows the same resolution logic — when a user with an existing anonymous profile makes an Identify call with a `userId`, the anonymous profile is merged into the identified user profile.

---

## Segment Behavioral Parity

RudderStack's Identify call maintains **full behavioral parity** with Segment's Identify specification. The following table provides a field-by-field comparison confirming identical behavior across both platforms.

| Field | Segment Behavior | RudderStack Behavior | Parity Status |
|-------|-----------------|---------------------|---------------|
| `userId` | Required for identified users; string identifier | Required for identified users; string identifier | ✅ Full Parity |
| `anonymousId` | Auto-generated by SDKs; pseudo-unique string | Auto-generated by SDKs; pseudo-unique string | ✅ Full Parity |
| `traits` | Forwarded to destinations with semantic mapping for reserved traits | Forwarded to destinations with semantic mapping for reserved traits | ✅ Full Parity |
| `context` | Enrichment metadata (IP, user agent, library, device, etc.) | Enrichment metadata (IP, user agent, library, device, etc.) | ✅ Full Parity |
| `context.traits` | Available for downstream destination trait enrichment | Available for downstream destination trait enrichment | ✅ Full Parity |
| `integrations` | Controls per-destination routing (`"All": false` disables all) | Controls per-destination routing (`"All": false` disables all) | ✅ Full Parity |
| `timestamp` | ISO 8601; computed as `receivedAt - (sentAt - originalTimestamp)` | ISO 8601; same computation logic | ✅ Full Parity |
| `messageId` | UUID auto-generated if not provided; used for deduplication | UUID auto-generated if not provided; used for deduplication | ✅ Full Parity |
| Reserved traits | 17 standardized traits with semantic destination mapping | 17 standardized traits with identical semantic destination mapping | ✅ Full Parity |
| Identity stitching | Anonymous-to-identified user merge on `userId` presence | Anonymous-to-identified user merge on `userId` presence | ✅ Full Parity |

> **Migration Note:** If you are migrating from Segment, your existing Identify call payloads require **zero modifications**. Simply update the endpoint URL from Segment's API to your RudderStack Gateway (`http://<your-gateway>:8080/v1/identify`) and replace the Write Key. All trait handling, identity resolution, and destination routing behavior remains identical.

Source: `gateway/openapi.yaml:688-721`, `refs/segment-docs/src/connections/spec/identify.md`

---

## See Also

- [Common Fields](common-fields.md) — Shared event fields reference (anonymousId, context, integrations, timestamps)
- [Track](track.md) — Track event specification for recording user actions
- [Page](page.md) — Page event specification for recording page views
- [Screen](screen.md) — Screen event specification for recording mobile screen views
- [Group](group.md) — Group event specification for associating users with groups
- [Alias](alias.md) — Alias event specification for merging user identities
- [Gateway HTTP API](../gateway-http-api.md) — Full HTTP API reference for all Gateway endpoints
- [API Overview & Authentication](../index.md) — Authentication guide covering all five auth schemes
