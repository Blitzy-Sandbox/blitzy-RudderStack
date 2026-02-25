# RudderStack Documentation

Comprehensive documentation for the RudderStack Customer Data Platform (CDP) — the open-source alternative to Twilio Segment built on the `rudder-server` v1.68.1 codebase.

RudderStack is a warehouse-first CDP with full Segment API compatibility. It provides durable event pipelines supporting 90+ destination integrations and 9 warehouse connectors, with a six-stage processing pipeline, real-time and batch routing, and identity resolution capabilities.

> **Version:** This documentation corresponds to `rudder-server` **v1.68.1** (Go 1.26.0, Elastic License 2.0).

> **Documentation approach:** Content follows progressive disclosure — high-level architecture overviews lead into detailed component documentation, then into API references and code examples. Start with the [Architecture Overview](architecture/overview.md) for system context, then drill into specific guides for your use case.

---

## Table of Contents

| Section | Description | Entry Point |
|---------|-------------|-------------|
| [Gap Report](#segment-parity-gap-report) | Segment feature parity analysis and sprint roadmap | [gap-report/index.md](gap-report/index.md) |
| [Architecture](#architecture) | System design, data flows, deployment topologies | [architecture/overview.md](architecture/overview.md) |
| [API Reference](#api-reference) | HTTP API, Event Spec, gRPC, Admin, error codes | [api-reference/index.md](api-reference/index.md) |
| [Getting Started](#guides) | Installation, configuration, first events | [guides/getting-started/installation.md](guides/getting-started/installation.md) |
| [Migration from Segment](#guides) | Step-by-step Segment-to-RudderStack migration | [guides/migration/segment-migration.md](guides/migration/segment-migration.md) |
| [Source SDKs](#guides) | JavaScript, iOS, Android, server-side SDK guides | [guides/sources/javascript-sdk.md](guides/sources/javascript-sdk.md) |
| [Destinations](#guides) | Stream, cloud, and warehouse destination catalog | [guides/destinations/index.md](guides/destinations/index.md) |
| [Transformations](#guides) | User transforms, destination transforms, Functions | [guides/transformations/overview.md](guides/transformations/overview.md) |
| [Governance](#guides) | Tracking plans, consent management, event filtering | [guides/governance/tracking-plans.md](guides/governance/tracking-plans.md) |
| [Identity Resolution](#guides) | Cross-touchpoint identity unification and profiles | [guides/identity/identity-resolution.md](guides/identity/identity-resolution.md) |
| [Operations](#guides) | Warehouse sync, replay, privacy, capacity planning | [guides/operations/warehouse-sync.md](guides/operations/warehouse-sync.md) |
| [Warehouse Connectors](#warehouse-connectors) | Per-connector setup and configuration guides | [warehouse/overview.md](warehouse/overview.md) |
| [Reference](#reference) | Configuration parameters, env vars, glossary, FAQ | [reference/config-reference.md](reference/config-reference.md) |
| [Contributing](#contributing) | Development setup, destination onboarding, testing | [contributing/development.md](contributing/development.md) |

---

## Segment Parity Gap Report

The Gap Report is a critical initial deliverable providing a comprehensive, actionable analysis comparing RudderStack capabilities against the Twilio Segment feature set. It identifies capability gaps, quantifies parity coverage, and sequences remediation work into an executable sprint roadmap.

**[Gap Report Executive Summary](gap-report/index.md)** — Start here for the overall parity assessment, feature matrix, and prioritized gap inventory.

### Gap Analysis Dimensions

| Dimension | Document | Scope |
|-----------|----------|-------|
| Event Spec Parity | [event-spec-parity.md](gap-report/event-spec-parity.md) | `track`, `identify`, `page`, `screen`, `group`, `alias` — payload-level comparison |
| Destination Catalog Parity | [destination-catalog-parity.md](gap-report/destination-catalog-parity.md) | Connector coverage comparison across 90+ existing vs. Segment catalog |
| Source Catalog Parity | [source-catalog-parity.md](gap-report/source-catalog-parity.md) | SDK and cloud source compatibility analysis |
| Functions Parity | [functions-parity.md](gap-report/functions-parity.md) | RudderStack Transformations vs. Segment Functions comparison |
| Protocols Parity | [protocols-parity.md](gap-report/protocols-parity.md) | Tracking plan enforcement and schema validation comparison |
| Identity Parity | [identity-parity.md](gap-report/identity-parity.md) | Identity resolution capabilities vs. Segment Unify |
| Warehouse Parity | [warehouse-parity.md](gap-report/warehouse-parity.md) | Warehouse sync features — idempotency, backfill, connector coverage |
| Sprint Roadmap | [sprint-roadmap.md](gap-report/sprint-roadmap.md) | Epic sequencing for autonomous gap closure implementation |

> **Phase 1 scope note:** Segment Engage/Campaigns and Reverse ETL are planned for Phase 2 and are not covered in the current gap analysis.

---

## Architecture

System architecture documentation covering the end-to-end event pipeline, deployment configurations, and internal component design. RudderStack operates as a modular monolith with five core pipeline components (Gateway, Processor, Router, Batch Router, Warehouse) backed by PostgreSQL-based persistent job queues.

- [System Overview](architecture/overview.md) — High-level component topology with Mermaid architecture diagram showing Gateway, Processor, Router, Batch Router, Warehouse, and external dependencies
- [Data Flow](architecture/data-flow.md) — End-to-end event lifecycle from SDK ingestion through processing, routing, and warehouse loading with sequence diagrams
- [Deployment Topologies](architecture/deployment-topologies.md) — Three deployment modes: `EMBEDDED` (all-in-one), `GATEWAY` (ingestion-only), `PROCESSOR` (processing-only) for horizontal scaling
- [Pipeline Stages](architecture/pipeline-stages.md) — Six-stage Processor pipeline: preprocess → source hydration → pre-transform → user transform → destination transform → store
- [Warehouse State Machine](architecture/warehouse-state-machine.md) — Seven-state upload lifecycle managing staging file generation, schema evolution, table loading, and completion
- [Cluster Management](architecture/cluster-management.md) — etcd-based multi-node coordination with NormalMode/DegradedMode state transitions
- [Security](architecture/security.md) — Authentication (5 schemes), AES-GCM encrypted configuration cache, SSRF protection, OAuth v2 integration

---

## API Reference

Complete API reference for all RudderStack HTTP endpoints, event specifications, gRPC services, and administrative operations. The Gateway serves Segment-compatible HTTP APIs on port 8080, the Warehouse exposes gRPC services on port 8082, and the Transformer service runs on port 9090.

### Overview and Authentication

- [API Overview and Authentication](api-reference/index.md) — Five authentication schemes: Basic Auth (Write Key), Bearer Token, OAuth v2, DPAuth (Control Plane), and Anonymous (pixel/beacon)

### Event Specification (Segment Spec Compatible)

The six core event types implementing full Segment Spec API compatibility:

| Event Type | Document | Endpoint |
|------------|----------|----------|
| Common Fields | [common-fields.md](api-reference/event-spec/common-fields.md) | Shared fields across all event types (`anonymousId`, `userId`, `context`, `integrations`, `timestamp`) |
| Identify | [identify.md](api-reference/event-spec/identify.md) | `POST /v1/identify` — Associate traits with a user identity |
| Track | [track.md](api-reference/event-spec/track.md) | `POST /v1/track` — Record a user action with event name and properties |
| Page | [page.md](api-reference/event-spec/page.md) | `POST /v1/page` — Record a web page view with page name and properties |
| Screen | [screen.md](api-reference/event-spec/screen.md) | `POST /v1/screen` — Record a mobile screen view with screen name and properties |
| Group | [group.md](api-reference/event-spec/group.md) | `POST /v1/group` — Associate a user with a group (organization, account) |
| Alias | [alias.md](api-reference/event-spec/alias.md) | `POST /v1/alias` — Merge two user identities |

### Service APIs

- [Gateway HTTP API](api-reference/gateway-http-api.md) — Full HTTP endpoint reference including `/v1/batch`, `/v1/import`, `/v1/replay`, `/v1/retl`, beacon (`/beacon/v1/*`), pixel (`/pixel/v1/*`), and webhook endpoints
- [Warehouse gRPC API](api-reference/warehouse-grpc-api.md) — 15 unary RPCs for warehouse upload management, health checks, trigger operations, and pending events queries
- [Admin API](api-reference/admin-api.md) — UNIX domain socket administrative operations and `rudder-cli` command reference
- [Error Codes](api-reference/error-codes.md) — HTTP response codes and error message reference from the Gateway response handler

---

## Guides

Developer and operator guides organized by workflow. Each guide provides setup instructions, configuration parameters, usage examples, and troubleshooting procedures.

### Getting Started

Set up RudderStack and send your first events:

- [Installation](guides/getting-started/installation.md) — Docker, Kubernetes, and developer machine setup using `docker-compose.yml`
- [Configuration](guides/getting-started/configuration.md) — Essential `config.yaml` parameters and environment variable setup
- [First Events](guides/getting-started/first-events.md) — Tutorial for sending first events using `curl` and the `devtool` CLI

### Migration from Segment

Step-by-step guides for teams migrating from Twilio Segment to RudderStack:

- [Segment Migration Guide](guides/migration/segment-migration.md) — End-to-end migration covering SDK swap, destination re-mapping, tracking plan migration, and warehouse cutover
- [SDK Swap Guide](guides/migration/sdk-swap-guide.md) — Per-platform SDK replacement walkthrough for JavaScript, iOS, Android, and server-side SDKs

### Source SDKs

Client and server-side SDK integration guides for event collection:

- [JavaScript (Web) SDK](guides/sources/javascript-sdk.md) — Browser-based event collection with initialization, configuration, and consent integration
- [iOS SDK](guides/sources/ios-sdk.md) — Native iOS event collection with Swift/Objective-C integration
- [Android SDK](guides/sources/android-sdk.md) — Native Android event collection with Kotlin/Java integration
- [Server-Side SDKs](guides/sources/server-side-sdks.md) — Node.js, Python, Go, Java, and Ruby server-side SDK guides

### Destinations

Destination connector configuration and integration guides:

- [Destination Catalog Overview](guides/destinations/index.md) — Full destination catalog with categorization and connector status
- [Stream Destinations](guides/destinations/stream-destinations.md) — Kafka, Kinesis, Google Pub/Sub, Azure Event Hub, Firehose, EventBridge, Confluent Cloud configuration
- [Cloud Destinations](guides/destinations/cloud-destinations.md) — 90+ cloud integration connectors with setup and payload delivery details
- [Warehouse Destinations](guides/destinations/warehouse-destinations.md) — Warehouse destination overview linking to per-connector guides in the [Warehouse Connectors](#warehouse-connectors) section

### Transformations

Event transformation and custom logic developer guides:

- [Transformation Overview](guides/transformations/overview.md) — Architecture of the transformation system: user transforms (batch size 200), destination transforms (batch size 100), and Transformer service integration
- [User Transforms](guides/transformations/user-transforms.md) — JavaScript and Python custom transformation development, testing, and deployment
- [Destination Transforms](guides/transformations/destination-transforms.md) — Destination-specific payload shaping and field mapping reference
- [Functions](guides/transformations/functions.md) — Segment Functions equivalent — custom source/destination function capabilities with gap analysis

### Governance

Event governance, schema enforcement, and consent management:

- [Tracking Plans](guides/governance/tracking-plans.md) — Tracking plan configuration, event schema definition, and validation enforcement
- [Consent Management](guides/governance/consent-management.md) — OneTrust, Ketch, and Generic CMP integration with OR/AND consent resolution semantics
- [Event Filtering](guides/governance/event-filtering.md) — Event drop and filter rules configuration for selective destination delivery
- [Protocols Enforcement](guides/governance/protocols-enforcement.md) — Schema validation, anomaly detection, and Segment Protocols comparison

### Identity

Cross-touchpoint identity resolution and user profile management:

- [Identity Resolution](guides/identity/identity-resolution.md) — Identity unification via merge-rule resolution pipeline in `warehouse/identity/`
- [Profiles](guides/identity/profiles.md) — User profile construction, traits management, and Segment Unify comparison

### Operations

Production operations, monitoring, and infrastructure management:

- [Warehouse Sync](guides/operations/warehouse-sync.md) — Warehouse sync configuration, upload monitoring, troubleshooting, and idempotent merge strategies
- [Replay](guides/operations/replay.md) — Event replay and replay-on-failure semantics via the Archiver and replay HTTP handler
- [Privacy Compliance](guides/operations/privacy-compliance.md) — GDPR data deletion, user suppression, and regulation worker operation
- [Capacity Planning](guides/operations/capacity-planning.md) — Pipeline tuning for 50,000 events/sec throughput with ordering guarantees — covers Gateway, Processor, Router, and Warehouse worker configuration

---

## Warehouse Connectors

Per-warehouse setup, configuration, schema management, and performance tuning guides. The Warehouse service operates as a state-machine-driven loader with parallel upload capabilities, automatic schema evolution, and support for Parquet, JSON, and CSV staging file formats.

### Service Documentation

- [Warehouse Overview](warehouse/overview.md) — Warehouse service architecture, operational modes (master/slave), upload state machine, and configuration

### Connector Guides

| Warehouse | Document | Key Capabilities |
|-----------|----------|-----------------|
| Snowflake | [snowflake.md](warehouse/snowflake.md) | Key-pair auth, Snowpipe Streaming, internal/external stage loading |
| BigQuery | [bigquery.md](warehouse/bigquery.md) | Service account auth, parallel loading, partitioned tables |
| Redshift | [redshift.md](warehouse/redshift.md) | IAM and password auth, S3 manifest loading, COPY command |
| ClickHouse | [clickhouse.md](warehouse/clickhouse.md) | MergeTree engine, cluster support, bulk insert |
| Databricks | [databricks.md](warehouse/databricks.md) | Delta Lake, merge and append strategies, Unity Catalog |
| PostgreSQL | [postgres.md](warehouse/postgres.md) | Standard PostgreSQL loading with COPY |
| SQL Server | [mssql.md](warehouse/mssql.md) | Bulk CopyIn ingestion, Windows/SQL auth |
| Azure Synapse | [azure-synapse.md](warehouse/azure-synapse.md) | COPY INTO ingestion, Azure Blob staging |
| Datalake | [datalake.md](warehouse/datalake.md) | S3, GCS, and Azure Blob datalake with Parquet exports |

### Cross-Cutting Warehouse Documentation

- [Schema Evolution](warehouse/schema-evolution.md) — Automatic schema management: column addition, type promotion, and schema diff resolution
- [Encoding Formats](warehouse/encoding-formats.md) — Parquet, JSON, and CSV staging file format reference with configuration options

---

## Reference

Comprehensive reference material for configuration, environment variables, terminology, and frequently asked questions.

- [Configuration Reference](reference/config-reference.md) — All 200+ `config.yaml` parameters organized by subsystem (Gateway, Processor, Router, Warehouse, BackendConfig, Logger, Diagnostics) with defaults, types, acceptable ranges, and descriptions
- [Environment Variables](reference/env-var-reference.md) — All environment variables from `config/sample.env` with descriptions, defaults, and allowed values
- [Glossary](reference/glossary.md) — Unified terminology mapping RudderStack and Segment terms (e.g., Source ↔ Source, Destination ↔ Destination, Tracking Plan ↔ Protocols, User Transform ↔ Function)
- [FAQ](reference/faq.md) — Frequently asked questions covering setup, Segment migration, performance tuning, and troubleshooting

---

## Contributing

Guides for contributing to the RudderStack codebase and documentation:

- [Development Setup](contributing/development.md) — Development environment setup, build commands (`make`), dependency management, and local testing with `docker-compose`
- [Destination Onboarding](contributing/destination-onboarding.md) — Step-by-step guide for adding new destination connectors using the AsyncDestinationManager framework and custom destination manager patterns
- [Testing Guidelines](contributing/testing.md) — Test infrastructure, integration test setup via `testhelper/`, test execution commands, and coverage requirements

For general contribution guidelines (CLA, PR process, code review), see the repository root [CONTRIBUTING.md](../CONTRIBUTING.md).

---

## Documentation Standards

This documentation set follows consistent standards to ensure maintainability and accuracy.

### Format and Style

- **File format:** Markdown (`.md`) with embedded Mermaid diagrams (triple-backtick `mermaid` blocks)
- **Target audience:** Senior engineers and data engineering teams — technical depth is prioritized over introductory explanations
- **Progressive disclosure:** Each documentation area follows overview → detailed explanation → API reference → code examples
- **Code examples:** Go 1.26.0, JavaScript (ES6+), Python 3.x, `curl`, and Bash with syntax-highlighted fenced code blocks

### Source Citations

Every technical claim, configuration default, and API specification includes a source citation in the format:

```
Source: /path/to/file.go:LineRange
```

This enables traceability from documentation back to the codebase and facilitates incremental updates as the codebase evolves.

### Cross-References

- All internal links use **relative paths** within the `docs/` directory (e.g., `architecture/overview.md`, not `/docs/architecture/overview.md`)
- External links to the RudderStack documentation site use full URLs: `https://www.rudderstack.com/docs/`
- Segment documentation references cite the local mirror at `refs/segment-docs/`

### Terminology

All documentation uses terms defined in the [Glossary](reference/glossary.md), which cross-maps RudderStack terminology to Segment equivalents where applicable. Consistent terminology ensures clarity across all documents.

### Diagrams

Architecture documents, data flow guides, and complex workflows include Mermaid diagrams. Diagram types used:

- **Flowcharts** — System architecture, deployment topologies, data flows
- **Sequence diagrams** — Event lifecycle, identity resolution, OAuth flow
- **State diagrams** — Warehouse upload state machine, cluster mode transitions

Mermaid diagrams render natively in GitHub and GitLab Markdown renderers.

---

## Scope and Versioning

| Attribute | Value |
|-----------|-------|
| Server version | `rudder-server` v1.68.1 |
| Language | Go 1.26.0 |
| License | Elastic License 2.0 |
| API spec version | OpenAPI 3.0.3 (`gateway/openapi.yaml`) |
| Gateway port | 8080 |
| Warehouse port | 8082 |
| Transformer port | 9090 |

### Phase 1 Coverage

This documentation covers the complete RudderStack CDP platform as of v1.68.1, including:

- Full Segment Spec event API parity (track, identify, page, screen, group, alias)
- 90+ destination connectors (stream, cloud, warehouse)
- 9 warehouse connectors with idempotent sync and backfill support
- Six-stage event processing pipeline with transformation support
- Identity resolution and tracking plan enforcement
- Segment parity gap analysis across 8 dimensions

### Phase 2 Planned (Not Covered)

The following Segment features are explicitly out of scope for Phase 1 documentation:

- **Segment Engage/Campaigns** — Marketing automation and audience management
- **Reverse ETL** — Warehouse-to-destination data sync

These areas are tracked in the [Sprint Roadmap](gap-report/sprint-roadmap.md) for future implementation and documentation.
