# Glossary

Unified terminology reference for RudderStack CDP documentation (`rudder-server` v1.68.1). This glossary combines RudderStack platform terms and Segment terms, providing cross-mapping between the two platforms to ensure consistent usage across all documentation.

> **Canonical Source:** This glossary serves as the canonical terminology source for all documentation in the `docs/` tree. All other documents reference this file for consistent terminology. When Segment and RudderStack use different terms for the same concept, prefer the RudderStack term and note the Segment equivalent.

**Notation conventions:**

- Terms marked with **(Segment)** indicate Segment-specific terminology with no direct RudderStack equivalent.
- Terms marked with **(RudderStack)** are specific to the RudderStack platform.
- Unmarked terms are industry-standard or shared between both platforms.
- **Source** citations reference file paths within the `rudder-server` repository for traceability.

**See also:**

- [Architecture Overview](../architecture/overview.md) — high-level system components
- [API Reference](../api-reference/index.md) — endpoint specifications and authentication

---

## Terminology Cross-Reference

Quick-reference mapping of Segment terminology to RudderStack equivalents. Use this table when migrating documentation, configurations, or mental models from Segment to RudderStack.

| Segment Term | RudderStack Equivalent | Parity | Notes |
|---|---|---|---|
| Analytics.js | JavaScript SDK | Full | Compatible API surface; drop-in replacement |
| Functions (Source) | User Transforms | Partial | Custom JavaScript/Python code executed per-event (batch size 200) |
| Functions (Destination) | Destination Transforms | Partial | Payload shaping per-destination (batch size 100) |
| Protocols | Tracking Plans + Consent Management | Partial | Schema enforcement via `processor/trackingplan.go`; consent via `processor/consent.go` |
| Unify | Identity Resolution | Partial | Cross-touchpoint unification via `warehouse/identity/` |
| Engage | *(Phase 2 — out of scope)* | — | Segment Engage/Campaigns not covered in Phase 1 |
| Personas | Profiles | Partial | User profile management with traits |
| Reverse ETL | *(Phase 2 — out of scope)* | — | Not covered in Phase 1 |
| Cloud Mode | Cloud Mode / Server-Side | Full | Same concept — data routed through server infrastructure |
| Device Mode | Device Mode / Client-Side | Full | Same concept — data sent directly from client to destination |
| Workspace | Workspace | Full | Same concept, same term — isolated tenant environment |
| Write Key | WriteKey | Full | Authentication key for source identification |
| Catalog | Destination/Source Catalog | Full | Available integrations listing |
| Replay | Replay | Full | Re-sending archived events through the pipeline |
| MTU (Monthly Tracked Users) | Tracked Users | Similar | Similar concept via `enterprise/trackedusers/` (HyperLogLog-based) |
| Debugger | Debugger | Full | Event flow visualization (SourceDebugger, DestinationDebugger, TransformationDebugger) |
| Computed Trait | *(Not yet available)* | Gap | Per-user/account traits computed server-side — see gap report |
| SQL Trait | *(Not yet available)* | Gap | Traits derived from warehouse SQL queries — see gap report |

---

## Terms

### Alias

**Category:** Event Type
**Segment equivalent:** Alias

Event type for merging two user identities, linking a new identity to an existing one. One of the six Segment Spec event types. Typically used when an anonymous user logs in and you want to combine their anonymous activity with their known identity.

```json
{
  "type": "alias",
  "previousId": "anonymous-123",
  "userId": "known-user-456"
}
```

Source: `gateway/openapi.yaml`

---

### Analytics.js (Segment)

**Category:** Integration
**RudderStack equivalent:** JavaScript SDK

Segment's JavaScript library for web analytics. Provides a wrapper API (`analytics.track()`, `analytics.identify()`, etc.) for sending data from websites. The RudderStack JavaScript SDK offers a compatible API surface, enabling drop-in replacement during migration.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### AnonymousId

**Category:** Platform Concept

Auto-generated unique identifier assigned to unidentified users before identity resolution. Persisted client-side (via cookies or local storage) to maintain consistent tracking across sessions. Combined with `userId` after an `identify` call to enable cross-session identity stitching.

Source: `gateway/openapi.yaml`, `warehouse/identity/`

---

### API

**Category:** Industry Standard

Application Programming Interface. In the RudderStack context, this refers to the six core Segment Spec calls — `track`, `identify`, `page`, `screen`, `group`, and `alias` — exposed via the Gateway HTTP API on port 8080. RudderStack also provides the Warehouse gRPC API (port 8082) and an Admin RPC API.

Source: `gateway/openapi.yaml`

---

### App (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** Control Plane UI

The Segment web application where users configure sources, destinations, view the debugger, and manage tracking plans. In RudderStack, equivalent functionality is provided by the Control Plane UI and hosted dashboard.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Archiver (RudderStack)

**Category:** Pipeline Component

RudderStack component responsible for event archival to object storage. Archives events as gzipped JSONL files organized by source, date, and hour with a configurable retention period (default: 10 days). Archived events can be replayed through the pipeline via the Replay API.

Configuration: `JobsDB.archivalTimeInDays` (default: `10`), `Archiver.backupRowsBatchSize` (default: `100`)

Source: `archiver/`, `config/config.yaml:62-63,76`

---

### Asynchronous

**Category:** Industry Standard

Non-sequential processing pattern where operations execute without blocking the caller. Used extensively throughout the RudderStack pipeline — the Gateway accepts events asynchronously and writes them to JobsDB, allowing the Processor and Router to consume them independently at their own pace.

---

### Audience (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** *(Phase 2 — part of Engage)*

Segment Personas Audiences allow defining cohorts of users or accounts based on event behavior and traits that are kept up-to-date over time. This feature is part of Segment Engage, which is out of scope for Phase 1.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### AWS

**Category:** Industry Standard

Amazon Web Services. Cloud infrastructure provider. RudderStack integrates with multiple AWS services including Redshift (warehouse), Kinesis (streaming), S3 (object storage/datalake), Firehose (streaming delivery), EventBridge (event bus), and Lambda (serverless compute).

---

### Backend Config (RudderStack)

**Category:** Pipeline Component

Dynamic workspace configuration service that polls the Control Plane every 5 seconds for updated source, destination, and connection configurations. Maintains an AES-GCM encrypted local cache for resilience. Distributes configuration updates to all pipeline components via a pub/sub mechanism.

Configuration: `BackendConfig.pollInterval` (default: `5s`), `BackendConfig.configFromFile` (default: `false`)

Source: `backend-config/`, `config/config.yaml:208-215`

---

### Batch Router (RudderStack)

**Category:** Pipeline Component

RudderStack component for bulk delivery of events to destinations that prefer or require batch processing. Generates staging files for warehouse destinations and manages async delivery to batch-oriented cloud destinations. Operates with configurable upload frequency and worker counts.

Configuration: `BatchRouter.uploadFreq` (default: `30s`), `BatchRouter.noOfWorkers` (default: `8`), `BatchRouter.maxFailedCountForJob` (default: `128`)

Source: `router/batchrouter/`, `config/config.yaml:138-144`

---

### BatchSize (RudderStack)

**Category:** Configuration

Number of events processed in a single batch operation. Key configurable values in the pipeline:

| Parameter | Default | Component |
|---|---|---|
| `Processor.transformBatchSize` | 100 | Destination transform batch size |
| `Processor.userTransformBatchSize` | 200 | User transform batch size |
| `Gateway.maxUserRequestBatchSize` | 128 | Gateway request batching |
| `Gateway.maxDBBatchSize` | 128 | Gateway DB write batching |

Source: `config/config.yaml:23-24,191-192`

---

### Catalog

**Category:** Platform Concept

The list of available sources, destinations, and warehouses that can be configured in the platform. RudderStack currently supports 90+ destination integrations and 9 warehouse connectors. The Segment catalog is used as the reference baseline for parity analysis.

Source: `refs/segment-docs/src/_data/glossary.yml`, `README.md`

---

### CDN

**Category:** Industry Standard

Content Delivery Network. A geographically distributed network of servers that accelerates content delivery by serving files from locations closer to the end user. Relevant to SDK distribution — client-side SDKs are typically loaded from a CDN.

---

### CDP

**Category:** Industry Standard

Customer Data Platform. A central platform that brokers the flow of customer data through application infrastructure, enabling unified customer understanding, regulatory compliance, and optimized customer interactions. RudderStack is an open-source CDP providing warehouse-first data pipelines.

Source: `README.md:53`

---

### Client Side

**Category:** Platform Concept
**Also known as:** Device Mode

Libraries and SDKs that run on the user's device (web browser, mobile app). Client-side libraries can collect contextual data about the user (device info, screen size, cookies) and maintain a local cache of identity information (`anonymousId`, `userId`, traits). Contrast with [Server Side](#server-side).

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Cloud Mode

**Category:** Platform Concept
**Also known as:** Server Side

Data routing pattern where events are sent from the client to the RudderStack server, which then forwards them to destinations. Cloud-mode libraries run on the server and are invisible to end users. They do not maintain local state, so every API call must include all required context. Contrast with [Device Mode](#device-mode).

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Computed Trait (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** *(Not yet available — see gap report)*

Per-user or per-account traits that are computed server-side based on event history. When built, Segment adds computed traits to relevant user profiles. RudderStack does not currently have a direct equivalent; this is documented in the identity parity gap report.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Connection (RudderStack)

**Category:** Platform Concept

A configured data pipeline linking a Source to a Destination. Connections define how events from a specific source are processed, transformed, and delivered to a specific destination. Managed via the Backend Config service.

Source: `backend-config/`

---

### Consent Management (RudderStack)

**Category:** Pipeline Component
**Segment equivalent:** Part of Protocols

Framework for enforcing data collection consent policies. Supports three consent management providers with configurable enforcement semantics:

| Provider | Logic |
|---|---|
| OneTrust | OR-based consent category matching |
| Ketch | OR-based purpose matching |
| Generic CMP | Configurable AND/OR semantics |

Source: `processor/consent.go`

---

### Control Plane (RudderStack)

**Category:** Pipeline Component

gRPC-based remote configuration service that manages workspace configuration, source/destination definitions, and connection settings. The Backend Config service polls the Control Plane for updates. In self-hosted deployments, the Control Plane can be backed by a local configuration file (`BackendConfig.configFromFile: true`).

Source: `controlplane/`, `config/config.yaml:208-209`

---

### Cookies

**Category:** Industry Standard

Small text values stored by the browser, used for session management and identity persistence. RudderStack's JavaScript SDK stores the `anonymousId` and related tracking state in cookies (similar to Segment's `ajs_uid` cookie).

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Custom Trait (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** Traits (from Identify calls)

User or account traits collected from Identify calls. In Segment, these are available in the Profile Explorer and can be used in audiences, computed traits, and SQL traits. In RudderStack, traits from Identify calls are persisted in the warehouse via the identity resolution pipeline.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Debugger

**Category:** Platform Concept

Tool for viewing event flow through the pipeline in real time. RudderStack provides three debugger types:

| Debugger | Purpose |
|---|---|
| SourceDebugger | Inspect events at ingestion |
| DestinationDebugger | Inspect delivery status to destinations |
| TransformationDebugger | Inspect transformation results |

Configuration: Configurable via `Debugger.maxBatchSize` (default: `32`), `Debugger.maxRetry` (default: `3`)

Source: `config/config.yaml:45-61`

---

### DegradedMode (RudderStack)

**Category:** Configuration

Fallback operation mode activated when the etcd cluster is unavailable. In DegradedMode, the cluster operates without dynamic partition management, reverting to a static configuration. Contrast with [NormalMode](#normalmode-rudderstack).

Source: `app/cluster/dynamic.go`

---

### Destination

**Category:** Platform Concept

A target system where RudderStack forwards processed event data. Destinations include cloud services (marketing, analytics, CRM platforms), data warehouses (Snowflake, BigQuery, Redshift), streaming platforms (Kafka, Kinesis, Pub/Sub), and object storage (S3, GCS, Azure Blob). RudderStack currently supports 90+ destination integrations.

Source: `router/`, `README.md:82`

---

### Destination Transform (RudderStack)

**Category:** Pipeline Component
**Segment equivalent:** Destination Functions (partial)

Payload shaping transformation applied per-destination to convert the internal RudderStack event format into the format required by each destination's API. Executed by the external Transformer service (port 9090) in batches of 100 events (configurable via `Processor.transformBatchSize`).

Source: `processor/transformer/`, `config/config.yaml:191`

---

### Device Mode

**Category:** Platform Concept
**Also known as:** Client Side

Client-side library mode where event data is sent directly from the user's device to the destination's API endpoints, without passing through the RudderStack server. Device-mode integrations load the destination's SDK alongside the RudderStack SDK. Contrast with [Cloud Mode](#cloud-mode).

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### DMP

**Category:** Industry Standard

Data Management Platform. A platform for collecting, organizing, and activating audience data, typically used in advertising and marketing contexts.

---

### DSP

**Category:** Industry Standard

Demand-Side Platform. An advertising technology platform used by advertisers to purchase digital ad impressions programmatically.

---

### EMBEDDED Mode (RudderStack)

**Category:** Configuration

RudderStack deployment mode that runs all pipeline components — Gateway, Processor, Router, Batch Router, and Warehouse Service — in a single process. Suitable for development, testing, and small-scale deployments. Contrast with [GATEWAY Mode](#gateway-mode-rudderstack) and [PROCESSOR Mode](#processor-mode-rudderstack).

Source: `app/app.go`

---

### ETL

**Category:** Industry Standard

Extract, Transform, and Load. The process of extracting data from production systems, transforming it into a new format (with enrichment or reshaping), and loading it into a data warehouse for analysis. RudderStack operates as both an event streaming platform and an ELT (Extract, Load, Transform) tool with its warehouse-first architecture.

---

### Event

**Category:** Platform Concept

An action by a user that triggers a data collection call. Events have a `type` (track, identify, page, screen, group, or alias), optional `name` and `properties`, and occur at a specific moment in time. Events are the fundamental unit of data flowing through the RudderStack pipeline.

Source: `gateway/openapi.yaml`, `refs/segment-docs/src/_data/glossary.yml`

---

### Event Spec

**Category:** Platform Concept
**Also known as:** Spec, Segment Specification

The specification defining standard event types, their schemas, common fields, and semantic conventions. RudderStack implements full compatibility with the Segment Specification, supporting all six core event types (`track`, `identify`, `page`, `screen`, `group`, `alias`) with identical payload structures and field semantics.

Source: `gateway/openapi.yaml`, `refs/segment-docs/src/connections/spec/`

---

### Functions (Segment)

**Category:** Integration
**RudderStack equivalent:** User Transforms + Destination Transforms (partial)

Segment's custom code execution framework supporting three function types:

| Segment Function Type | RudderStack Equivalent | Status |
|---|---|---|
| Source Functions | User Transforms | Partial — JavaScript/Python transforms with batch size 200 |
| Destination Functions | Destination Transforms | Partial — payload shaping with batch size 100 |
| Insert Functions | *(No direct equivalent)* | Gap — see functions parity report |

Source: `refs/segment-docs/src/connections/functions/`, `processor/`

---

### Gateway (RudderStack)

**Category:** Pipeline Component

HTTP ingestion component accepting Segment-compatible event payloads on port 8080. Handles authentication (5 schemes), request validation, rate limiting, deduplication, batching, and writing accepted events to JobsDB. Supports standard REST endpoints (`/v1/track`, `/v1/identify`, etc.), batch import (`/v1/batch`, `/v1/import`), webhook ingestion, pixel tracking, and beacon tracking.

Configuration: `Gateway.webPort` (default: `8080`), `Gateway.maxUserWebRequestWorkerProcess` (default: `64`), `Gateway.maxDBWriterProcess` (default: `256`)

Source: `gateway/`, `config/config.yaml:18-40`

---

### GATEWAY Mode (RudderStack)

**Category:** Configuration

Deployment mode running only the Gateway ingestion component. Events are written to the shared JobsDB and consumed by separate PROCESSOR Mode instances. Used for horizontal scaling of ingestion capacity independently from processing. Contrast with [EMBEDDED Mode](#embedded-mode-rudderstack) and [PROCESSOR Mode](#processor-mode-rudderstack).

Source: `app/apphandlers/`

---

### GCRA (RudderStack)

**Category:** Configuration

Generic Cell Rate Algorithm. The default throttling algorithm used by the Router to enforce per-destination rate limits. Prevents overwhelming destination APIs by controlling the rate of event delivery based on configurable limits and time windows.

Configuration: `Router.throttler.algorithm` (default: `gcra`)

Source: `router/throttler/`, `config/config.yaml:121-122`

---

### Group

**Category:** Event Type
**Segment equivalent:** Group

Event type for associating a user with a group, organization, company, or account. One of the six Segment Spec event types. Group calls link a `userId` to a `groupId` and can set group-level `traits`.

```json
{
  "type": "group",
  "userId": "user-123",
  "groupId": "company-456",
  "traits": {
    "name": "Acme Corp",
    "plan": "enterprise"
  }
}
```

Source: `gateway/openapi.yaml`

---

### Identify

**Category:** Event Type
**Segment equivalent:** Identify

Event type for setting user traits and associating identity information with a user. One of the six Segment Spec event types. Identify calls link a `userId` to an `anonymousId` and set `traits` such as email, name, and plan.

```json
{
  "type": "identify",
  "userId": "user-123",
  "traits": {
    "email": "user@example.com",
    "name": "Jane Doe",
    "plan": "premium"
  }
}
```

Source: `gateway/openapi.yaml`

---

### Identity Resolution (RudderStack)

**Category:** Pipeline Component
**Segment equivalent:** Unify

Cross-touchpoint user identity unification system that merges multiple identifiers (anonymousId, userId, device IDs) into a single user profile. Implemented within the warehouse loading pipeline using merge rules to determine how profiles are combined.

Configuration: `Warehouse.enableIDResolution` (default: `false`), `Warehouse.populateHistoricIdentities` (default: `false`)

Source: `warehouse/identity/`, `config/config.yaml:159-160`

---

### JobsDB (RudderStack)

**Category:** Pipeline Component

PostgreSQL-backed persistent job queue that provides durable, exactly-once event delivery guarantees across all pipeline stages. Features partitioned datasets, priority pools, pending events registry, distinct values caching, and COPY IN bulk inserts for high-throughput operation.

Configuration: `JobsDB.maxDSSize` (default: `100000`), `JobsDB.maxTableSizeInMB` (default: `300`), `JobsDB.archivalTimeInDays` (default: `10`)

Source: `jobsdb/`, `config/config.yaml:64-91`

---

### JSON

**Category:** Industry Standard

JavaScript Object Notation. The standard data interchange format used for event payloads throughout the RudderStack pipeline. Events are ingested as JSON via the Gateway HTTP API, stored as JSON in JobsDB, and exported as gzipped JSONL for warehouse staging files.

---

### Library

**Category:** Platform Concept
**Also known as:** SDK

A reusable software package for sending event data to the RudderStack API. Libraries are available for multiple platforms: JavaScript (web), iOS, Android, and server-side languages (Node.js, Python, Go, Java, Ruby). Libraries can operate in either [Device Mode](#device-mode) or [Cloud Mode](#cloud-mode).

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Lookback (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** *(Not yet available — see gap report)*

A time window that limits the period of data considered when calculating a trait or audience. For example, a 7-day lookback window on a `new_users_7_days` audience. This concept is part of Segment Personas/Engage, which is currently in gap status.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Merge Rule (RudderStack)

**Category:** Platform Concept

Identity resolution rule that determines how user profiles are combined when multiple identifiers are encountered for the same user. Merge rules define the precedence and matching logic used during the identity resolution process in the warehouse pipeline.

Source: `warehouse/identity/`

---

### Method

**Category:** Industry Standard

An action or function that can be invoked on an analytics object. In client-side SDKs, methods include `track()`, `identify()`, `page()`, `screen()`, `group()`, and `alias()` — corresponding to the six Segment Spec event types.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### MTU

**Category:** Platform Concept
**Also known as:** Monthly Tracked Users

Metric for counting unique users tracked per month. Calculated by combining the number of unique `userIds` and unique `anonymousIds`. In RudderStack, tracked user counting is implemented via HyperLogLog probabilistic data structures in the enterprise module.

Source: `enterprise/trackedusers/`, `refs/segment-docs/src/_data/glossary.yml`

---

### Namespace (RudderStack)

**Category:** Configuration

Multi-tenant isolation boundary within the RudderStack workspace configuration. Namespaces enable a single RudderStack deployment to serve multiple isolated tenants, each with independent sources, destinations, and configuration. Used in multi-workspace mode.

Source: `backend-config/`

---

### NormalMode (RudderStack)

**Category:** Configuration

Standard cluster operation mode when etcd is available and healthy. In NormalMode, the cluster uses dynamic partition management for load distribution across instances. Contrast with [DegradedMode](#degradedmode-rudderstack).

Source: `app/cluster/dynamic.go`

---

### Object (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** Traits (via Identify/Group calls)

A type of data that persists over time and can be updated, such as a user record or business record. Objects have traits (properties) that may change over time. In RudderStack, persistent object state is maintained through `identify` and `group` calls that update traits in the warehouse.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### OTT

**Category:** Industry Standard

Over the Top. Refers to content providers distributing streaming media directly to viewers over the Internet, bypassing traditional broadcast infrastructure. Relevant in the context of tracking events from OTT applications.

---

### Page

**Category:** Event Type
**Segment equivalent:** Page

Event type recording that a user viewed a web page. One of the six Segment Spec event types. Page calls can optionally include a `name` and `category`, along with page-specific `properties` (URL, title, referrer, path).

```json
{
  "type": "page",
  "name": "Home",
  "properties": {
    "url": "https://example.com",
    "title": "Example - Home",
    "referrer": "https://google.com"
  }
}
```

Source: `gateway/openapi.yaml`

---

### Pipeline (RudderStack)

**Category:** Platform Concept

The complete data processing path that events traverse from ingestion to delivery: **Gateway** (ingestion, auth, validation) → **Processor** (6-stage transformation pipeline) → **Router** (real-time destination delivery) or **Batch Router** (bulk delivery) → **Warehouse Service** (warehouse loading). Each stage uses JobsDB as the durable transfer mechanism.

Source: `runner/runner.go`

---

### Postgres

**Category:** Industry Standard
**Also known as:** PostgreSQL

Open-source relational database server. Used by RudderStack as the backing store for JobsDB (the persistent job queue) and as a supported warehouse destination.

Source: `docker-compose.yml`, `warehouse/integrations/postgres/`

---

### Processor (RudderStack)

**Category:** Pipeline Component

Core event processing component executing a six-stage pipeline:

1. **Preprocess** — initial event validation and enrichment
2. **Source Hydration** — attach source configuration metadata
3. **Pre-Transform** — consent filtering, tracking plan validation, event filtering
4. **User Transform** — execute custom JavaScript/Python transformations (batch size 200)
5. **Destination Transform** — shape payloads per-destination (batch size 100)
6. **Store** — write processed events to Router/Batch Router JobsDB tables

Configuration: `Processor.maxLoopProcessEvents` (default: `10000`), `Processor.transformBatchSize` (default: `100`), `Processor.userTransformBatchSize` (default: `200`)

Source: `processor/`, `config/config.yaml:184-200`

---

### PROCESSOR Mode (RudderStack)

**Category:** Configuration

Deployment mode running only the Processor, Router, Batch Router, and Warehouse Service components (no Gateway). Consumes events from the shared JobsDB written by GATEWAY Mode instances. Used for horizontal scaling of processing capacity independently from ingestion. Contrast with [EMBEDDED Mode](#embedded-mode-rudderstack) and [GATEWAY Mode](#gateway-mode-rudderstack).

Source: `app/apphandlers/`

---

### Profiles (RudderStack)

**Category:** Platform Concept
**Segment equivalent:** Personas / Unify

User profile management with traits and computed properties. In RudderStack, profiles are constructed in the warehouse through identity resolution, combining data from `identify`, `group`, and `track` calls into unified user records. Partial equivalent to Segment's Personas product.

Source: `warehouse/identity/`

---

### Protocols (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** Tracking Plans + Consent Management (partial)

Segment's event governance framework providing schema enforcement, anomaly detection, and event validation. In RudderStack, equivalent functionality is partially provided through tracking plan validation (`processor/trackingplan.go`), consent filtering (`processor/consent.go`), and gateway-level schema validation (`gateway/validator/`).

Source: `refs/segment-docs/src/protocols/`, `processor/trackingplan.go`

---

### Redshift

**Category:** Integration

Amazon Redshift — an analytics data warehouse from AWS. RudderStack supports Redshift as a warehouse destination with IAM and password authentication, manifest-based loading, and parallel load support.

Configuration: `Warehouse.redshift.maxParallelLoads` (default: `3`)

Source: `warehouse/integrations/redshift/`, `config/config.yaml:162-163`

---

### Regulation Worker (RudderStack)

**Category:** Pipeline Component

Component enforcing GDPR and data privacy regulations by executing data deletion requests. Supports three deletion strategies: API-based deletion, batch deletion, and KV store deletion. Integrates with OAuthv2 for authenticated deletion requests to third-party destinations.

Source: `regulation-worker/`

---

### Replay

**Category:** Platform Concept

The ability to re-send previously archived events through the pipeline to new or existing destinations. In RudderStack, events archived by the Archiver (gzipped JSONL in object storage) can be replayed via the dedicated HTTP replay endpoint. Useful for backfilling new destinations or recovering from delivery failures.

Source: `gateway/handle_http_replay.go`, `backend-config/replay_types.go`

---

### Retry

**Category:** Platform Concept

Re-delivery attempt mechanism for events that fail to reach their destination. RudderStack implements sophisticated retry logic with exponential backoff per destination:

| Component | Retry Window | Max Backoff |
|---|---|---|
| Router | 180 minutes | 300 seconds |
| Batch Router | 180 minutes | — |
| Warehouse | 180 minutes | 1800 seconds |

Source: `config/config.yaml:108-112,144,153-155`

---

### Router (RudderStack)

**Category:** Pipeline Component

Real-time destination delivery component with per-destination worker pools, GCRA-based throttling, guaranteed user event ordering, adaptive batching, and retry logic with exponential backoff. Delivers events to streaming and cloud destinations in near real-time.

Configuration: `Router.noOfWorkers` (default: `64`), `Router.guaranteeUserEventOrder` (default: `true`), `Router.retryTimeWindow` (default: `180m`)

Source: `router/`, `config/config.yaml:92-136`

---

### Schema

**Category:** Platform Concept

The structure definition of a database or event, including field names and data types. In RudderStack warehouses, schemas evolve automatically — new columns are added when new event properties or traits appear, and existing columns are preserved. Schema management is handled per-warehouse-type.

Source: `warehouse/schema/`, `refs/segment-docs/src/_data/glossary.yml`

---

### Screen

**Category:** Event Type
**Segment equivalent:** Screen

Event type recording that a user viewed a screen in a mobile application. One of the six Segment Spec event types. Analogous to [Page](#page) for mobile contexts. Screen calls include a `name` and optional `properties`.

```json
{
  "type": "screen",
  "name": "Dashboard",
  "properties": {
    "variation": "blue_button"
  }
}
```

Source: `gateway/openapi.yaml`

---

### SDK

**Category:** Industry Standard
**Also known as:** Library

Software Development Kit. A set of tools and libraries for integrating event tracking into applications. RudderStack provides SDKs for JavaScript (web), iOS, Android, and server-side languages (Node.js, Python, Go, Java, Ruby). SDKs are Segment API-compatible, enabling drop-in replacement.

Source: `README.md:78`

---

### Server Side

**Category:** Platform Concept
**Also known as:** Cloud Mode

Libraries and integrations that run on the server, sending data through the RudderStack cloud infrastructure. Server-side libraries do not maintain client-side state, so every API call must include all required identity and context information. Contrast with [Client Side](#client-side).

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### Source

**Category:** Platform Concept

The origin of event data — a website, server library, mobile SDK, or cloud application that sends data into RudderStack. Each source is identified by a unique [WriteKey](#writekey-rudderstack) used for authentication. Sources are configured in the workspace and linked to destinations via connections.

Source: `refs/segment-docs/src/_data/glossary.yml`, `backend-config/`

---

### Spec

**Category:** Platform Concept
**Also known as:** Event Spec, Segment Specification

Short for "Specification." The standard defining event types, required/optional fields, and semantic conventions for data collection. RudderStack implements full compatibility with the Segment Spec, ensuring events collected via RudderStack SDKs follow the same structure as Segment events.

Source: `refs/segment-docs/src/connections/spec/`, `gateway/openapi.yaml`

---

### SQL

**Category:** Industry Standard

Structured Query Language. The standard language for querying relational databases. Used in RudderStack warehouse contexts for querying loaded event data in Snowflake, BigQuery, Redshift, PostgreSQL, and other supported warehouses.

---

### SQL Trait (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** *(Not yet available — see gap report)*

Per-user or per-account traits created by running SQL queries against a data warehouse. Segment imports results into Personas and appends them to user profiles. RudderStack does not currently have a direct equivalent.

Source: `refs/segment-docs/src/_data/glossary.yml`

---

### SSP

**Category:** Industry Standard

Supply-Side Platform. An advertising technology platform used by publishers to manage, sell, and optimize their available ad inventory.

---

### Staging Files (RudderStack)

**Category:** Pipeline Component

Intermediate files generated during warehouse loading, containing batched event data in gzipped JSONL format. Staging files are created by the Batch Router and consumed by the Warehouse Service's upload state machine. They serve as the durable transfer medium between real-time processing and warehouse bulk loading.

Configuration: `Warehouse.stagingFilesBatchSize` (default: `960`)

Source: `warehouse/encoding/`, `config/config.yaml:158`

---

### Suppression (RudderStack)

**Category:** Platform Concept

The mechanism for preventing data collection and forwarding for specific user IDs. When a user is suppressed, the Gateway drops their events at ingestion time, ensuring no data is processed or delivered. Part of the privacy and compliance framework.

Configuration: `Gateway.enableSuppressUserFeature` (default: `true`)

Source: `enterprise/suppress-user/`, `config/config.yaml:29`

---

### Throughput

**Category:** Platform Concept

The volume of API calls and events processed per unit of time. RudderStack's pipeline is designed to sustain 50,000 events per second with ordering guarantees. Throughput is tunable via Gateway worker counts, Processor batch sizes, Router worker pools, and deployment topology (EMBEDDED vs. GATEWAY+PROCESSOR split).

Source: `config/config.yaml`, `refs/segment-docs/src/_data/glossary.yml`

---

### Track

**Category:** Event Type
**Segment equivalent:** Track

Event type recording user actions with custom properties. One of the six Segment Spec event types and the most commonly used call. Track events have a required `event` name and optional `properties` object.

```json
{
  "type": "track",
  "event": "Product Viewed",
  "properties": {
    "product_id": "507f1f77bcf86cd799439011",
    "name": "Monopoly: 3rd Edition",
    "price": 18.99
  }
}
```

Source: `gateway/openapi.yaml`

---

### Tracking Plan (RudderStack)

**Category:** Platform Concept
**Segment equivalent:** Part of Protocols

Event governance tool that defines expected events, their required/optional properties, and data types. When enabled, the Processor validates incoming events against the tracking plan and can block or flag non-conforming events. Provides schema enforcement similar to (but not fully equivalent to) Segment Protocols.

Source: `processor/trackingplan.go`

---

### Traits

**Category:** Platform Concept

Individual pieces of information about a user or group. Traits are set via `identify` calls (for users) and `group` calls (for organizations). Common traits include `email`, `name`, `plan`, `company`, and `createdAt`. Traits persist over time and are updated when new values are received.

Source: `refs/segment-docs/src/_data/glossary.yml`, `gateway/openapi.yaml`

---

### Transformer (RudderStack)

**Category:** Pipeline Component

External service (default port 9090) responsible for executing JavaScript and Python transformations. The Transformer handles both user-defined custom transformations and built-in destination-specific payload transformations. Deployed as a separate container (`rudder-transformer`) alongside the main `rudder-server`.

Source: `docker-compose.yml`

---

### Unify (Segment)

**Category:** Platform Concept
**RudderStack equivalent:** Identity Resolution (partial)

Segment's identity resolution and profile management product. Provides identity graph construction, profile syncing, traits management, and cross-device identity stitching. In RudderStack, partial equivalent functionality is provided by the `warehouse/identity/` module for identity resolution within warehouse pipelines.

Source: `refs/segment-docs/src/unify/`, `warehouse/identity/`

---

### Upload (RudderStack)

**Category:** Platform Concept

A warehouse loading operation that follows a 7-state state machine lifecycle:

1. **Waiting** — queued for processing
2. **Generating Upload Schema** — merging staging file schemas
3. **Creating Table Uploads** — planning per-table operations
4. **Exporting Data** — extracting from staging files
5. **Exporting Data Failed** — handling export errors (retryable)
6. **Updating Table Uploads** — applying data to warehouse tables
7. **Aborted** / **Exported Data** — terminal states

Source: `warehouse/router/state.go`

---

### User Transform (RudderStack)

**Category:** Pipeline Component
**Segment equivalent:** Source Functions (partial)

Custom JavaScript or Python transformation applied to events during the Processor's pipeline. User transforms execute in the external Transformer service with a batch size of 200 events (configurable via `Processor.userTransformBatchSize`). They allow enrichment, filtering, routing, and reshaping of event data before destination-specific transformations.

Source: `processor/usertransformer/`, `config/config.yaml:192`

---

### Warehouse

**Category:** Platform Concept

A data warehouse destination where RudderStack loads structured event data for analytical querying. Supported warehouses:

| Warehouse | Source Directory |
|---|---|
| Snowflake | `warehouse/integrations/snowflake/` |
| BigQuery | `warehouse/integrations/bigquery/` |
| Redshift | `warehouse/integrations/redshift/` |
| ClickHouse | `warehouse/integrations/clickhouse/` |
| Databricks (Delta Lake) | `warehouse/integrations/deltalake/` |
| PostgreSQL | `warehouse/integrations/postgres/` |
| SQL Server (MSSQL) | `warehouse/integrations/mssql/` |
| Azure Synapse | `warehouse/integrations/azure-synapse/` |
| Datalake (S3/GCS/Azure) | `warehouse/integrations/datalake/` |

Source: `warehouse/integrations/`

---

### Warehouse Service (RudderStack)

**Category:** Pipeline Component

RudderStack component managing warehouse sync operations on port 8082. Orchestrates the complete warehouse loading lifecycle including staging file processing, schema evolution, parallel loading, and the 7-state upload state machine. Supports embedded and standalone deployment modes.

Configuration: `Warehouse.webPort` (default: `8082`), `Warehouse.mode` (default: `embedded`), `Warehouse.noOfWorkers` (default: `8`), `Warehouse.uploadFreq` (default: `1800s`)

Source: `warehouse/app.go`, `config/config.yaml:145-183`

---

### Workspace

**Category:** Platform Concept

An isolated tenant environment containing sources, destinations, connections, and configuration. Each workspace has a unique identifier (`WorkspaceID`) and its own set of write keys, tracking plans, and destination settings. Multi-workspace isolation is achieved through the [Namespace](#namespace-rudderstack) mechanism.

Source: `backend-config/`

---

### WriteKey (RudderStack)

**Category:** Configuration
**Segment equivalent:** Write Key

Authentication key uniquely identifying a source. Used in HTTP Basic Authentication with the WriteKey as the username and an empty password. Every event sent to the Gateway must include a valid WriteKey to be accepted. WriteKeys are configured per-source in the workspace.

```bash
# Example: sending a track event with WriteKey authentication
curl -X POST https://your-rudderstack:8080/v1/track \
  -u "YOUR_WRITE_KEY:" \
  -H "Content-Type: application/json" \
  -d '{"type": "track", "event": "Test Event", "userId": "user-123"}'
```

Source: `gateway/handle_http_auth.go`
