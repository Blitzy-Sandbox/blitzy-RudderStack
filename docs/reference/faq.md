# Frequently Asked Questions

Common questions from developers and operators working with RudderStack CDP (`rudder-server` v1.68.1). This FAQ covers deployment options, Segment API compatibility, warehouse sync behavior, transformation capabilities, performance tuning, authentication, and troubleshooting.

> **Configuration details:** For complete parameter documentation, see [Configuration Reference](./config-reference.md) and [Environment Variables Reference](./env-var-reference.md).
>
> **Terminology:** For definitions of terms used throughout this document, see the [Glossary](./glossary.md).

---

## Table of Contents

- [Deployment and Setup](#deployment-and-setup)
- [Segment API Compatibility](#segment-api-compatibility)
- [Warehouse Sync](#warehouse-sync)
- [Transformations](#transformations)
- [Performance and Capacity](#performance-and-capacity)
- [Operations and Troubleshooting](#operations-and-troubleshooting)

---

## Deployment and Setup

### **Q: What are the deployment options for RudderStack?**

RudderStack supports three deployment modes that control which pipeline components run within a given server instance:

| Mode | Description | Use Case |
|---|---|---|
| **EMBEDDED** | All-in-one mode — Gateway, Processor, and Router run in a single process | Development, small-to-medium workloads |
| **GATEWAY** | Ingestion-only — only the Gateway HTTP server runs, forwarding events to a shared JobsDB | Horizontal scaling of ingestion tier |
| **PROCESSOR** | Processing-only — only the Processor and Router run, consuming from JobsDB | Horizontal scaling of processing/routing tier |

Deployment mode is set via the application handlers in `app/app.go` and `app/apphandlers/`. Docker and Kubernetes deployments are both supported. PostgreSQL is the only required external runtime dependency; the Transformer service (port 9090) is optional and needed only for custom JavaScript/Python transformations.

> Source: `app/app.go`, `app/apphandlers/`, `README.md` — "independent, stand-alone system with a dependency only on the database (PostgreSQL)"

**See also:**

- [Installation Guide](../guides/getting-started/installation.md)
- [Deployment Topologies](../architecture/deployment-topologies.md)

---

### **Q: What is the minimum infrastructure required?**

The minimum infrastructure for a RudderStack deployment consists of:

1. **PostgreSQL database** — used as the persistent job queue (JobsDB) for durable event storage
2. **rudder-server binary** (or Docker container) — the core RudderStack server process
3. **rudder-transformer service** *(optional)* — required only for custom JavaScript/Python transformations (runs on port 9090)

The reference `docker-compose.yml` defines five services for a complete local development environment:

| Service | Image | Purpose |
|---|---|---|
| `db` | `postgres:15-alpine` | PostgreSQL database for JobsDB |
| `backend` | `rudder-server` | Core RudderStack server |
| `transformer` | `rudder-transformer` | JavaScript/Python transformation executor |
| `minio` | MinIO | Object storage (staging files, archival) |
| `etcd` | etcd | Cluster coordination (multi-node deployments) |

> Source: `docker-compose.yml`, `README.md:106` — "independent, stand-alone system with a dependency only on the database (PostgreSQL). Its backend is written in Go with a rich UI written in React.js."

---

### **Q: How do I configure RudderStack?**

RudderStack is configured through two complementary mechanisms:

1. **YAML configuration file** (`config/config.yaml`) — contains 200+ parameters organized by subsystem (Gateway, Router, Warehouse, Processor, etc.)
2. **Environment variables** (`config/sample.env`) — override any YAML parameter using the `RSERVER_` prefix convention

Environment variables always take precedence over YAML values. The `CONFIG_PATH` environment variable specifies the path to the YAML configuration file (default: `./config/config.yaml`).

**Override convention:** YAML paths are converted to environment variable names by prepending `RSERVER_`, replacing path separators with underscores, and expanding camelCase to UPPER_SNAKE_CASE. For example:

| YAML Path | Environment Variable |
|---|---|
| `Gateway.webPort` | `RSERVER_GATEWAY_WEB_PORT` |
| `Router.noOfWorkers` | `RSERVER_ROUTER_NO_OF_WORKERS` |
| `Warehouse.mode` | `RSERVER_WAREHOUSE_MODE` |

> Source: `config/config.yaml` (251 lines), `config/sample.env:1` — `CONFIG_PATH=./config/config.yaml`

**See also:**

- [Configuration Reference](./config-reference.md) — all 200+ YAML parameters with defaults, types, and descriptions
- [Environment Variables Reference](./env-var-reference.md) — complete environment variable listing

---

## Segment API Compatibility

### **Q: Is RudderStack compatible with the Segment API?**

Yes. RudderStack implements all six Segment Spec event types with full payload compatibility. The Gateway (default port 8080) accepts Segment-compatible payloads at the following endpoints:

| Endpoint | Event Type | Description |
|---|---|---|
| `POST /v1/identify` | Identify | Associate a user with their traits |
| `POST /v1/track` | Track | Record a user action with properties |
| `POST /v1/page` | Page | Record a web page view |
| `POST /v1/screen` | Screen | Record a mobile screen view |
| `POST /v1/group` | Group | Associate a user with a group/organization |
| `POST /v1/alias` | Alias | Merge two user identities |
| `POST /v1/batch` | Batch | Send multiple events in a single request |

All endpoints accept `application/json` request bodies and use `writeKeyAuth` (Basic Auth with the WriteKey as the username and an empty password) for authentication.

> Source: `gateway/openapi.yaml` (OpenAPI 3.0.3), `README.md:78` — "RudderStack is fully compatible with the Segment API. So you don't need to change your app if you are using Segment"

**See also:**

- [API Reference](../api-reference/index.md)
- [Event Spec — Common Fields](../api-reference/event-spec/common-fields.md)
- [Gap Report — Event Spec Parity](../gap-report/event-spec-parity.md)

---

### **Q: Can I use existing Segment SDKs with RudderStack?**

Segment SDKs can be pointed to RudderStack's Gateway endpoint by changing the API host configuration in the SDK initialization. The Gateway accepts standard Segment payloads with Basic Auth authentication using your WriteKey as the username (password is empty).

**Example — Redirecting Segment's Analytics.js to RudderStack:**

```javascript
// Change the API host from Segment to RudderStack Gateway
analytics.load("YOUR_WRITE_KEY", {
  integrations: { "Segment.io": { apiHost: "your-rudderstack-host:8080/v1" } }
});
```

RudderStack also provides its own first-party SDKs for JavaScript (web), iOS, Android, and server-side languages (Node.js, Python, Go, Java, Ruby) with native support for RudderStack-specific features.

> Source: `gateway/handle_http_auth.go`, `gateway/openapi.yaml` — `writeKeyAuth` security scheme

**See also:**

- [Segment Migration Guide](../guides/migration/segment-migration.md)
- [SDK Swap Guide](../guides/migration/sdk-swap-guide.md)
- [Source SDK Guides](../guides/sources/javascript-sdk.md)

---

### **Q: What authentication methods does the Gateway support?**

The Gateway supports multiple authentication schemes for event ingestion:

| Auth Scheme | Method | Details |
|---|---|---|
| **Basic Auth** | `Authorization: Basic base64(writeKey:)` | WriteKey as username, empty password. Primary method. |
| **Bearer Token** | `Authorization: Bearer <writeKey>` | WriteKey sent as bearer token |
| **URL Query Parameter** | `?writeKey=<writeKey>` | WriteKey as a URL query parameter |
| **Embedded Path** | `/v1/track/<writeKey>` | WriteKey embedded in the URL path |

The WriteKey uniquely identifies a Source configuration and determines which destinations receive the routed events. All authentication schemes extract the WriteKey and validate it against the backend configuration.

> Source: `gateway/handle_http_auth.go`

**See also:**

- [API Overview & Authentication](../api-reference/index.md)

---

### **Q: What Segment features are not yet supported?**

RudderStack achieves full or partial parity with most Segment features. The following features have known gaps:

| Segment Feature | RudderStack Status | Notes |
|---|---|---|
| Functions (Source/Destination) | **Partial** | User Transforms and Destination Transforms cover core use cases; custom source ingestion functions are not yet equivalent |
| Protocols (Advanced) | **Partial** | Tracking plan enforcement via `processor/trackingplan.go`; anomaly detection is not yet available |
| Unify (Identity Graph) | **Partial** | Identity resolution via `warehouse/identity/`; full profile sync and data graph are not yet equivalent |
| Computed Traits | **Gap** | Server-side per-user/account trait computation not yet available |
| SQL Traits | **Gap** | Warehouse SQL-derived traits not yet available |
| Engage / Campaigns | **Phase 2** | Explicitly out of scope for Phase 1 |
| Reverse ETL | **Phase 2** | Explicitly out of scope for Phase 1 |

> Source: `refs/segment-docs/src/connections/functions/`, `refs/segment-docs/src/protocols/`, `refs/segment-docs/src/unify/`

**See also:**

- [Gap Report — Executive Summary](../gap-report/index.md)
- [Functions Parity](../gap-report/functions-parity.md)
- [Protocols Parity](../gap-report/protocols-parity.md)
- [Identity Parity](../gap-report/identity-parity.md)

---

## Warehouse Sync

### **Q: Which warehouses does RudderStack support?**

RudderStack includes nine warehouse connectors with direct database loading:

| Warehouse | Integration Directory | Key Feature |
|---|---|---|
| **Snowflake** | `warehouse/integrations/snowflake/` | Snowpipe Streaming, parallel loads (default: 3) |
| **BigQuery** | `warehouse/integrations/bigquery/` | Parallel loads (default: 20), streaming insert support |
| **Redshift** | `warehouse/integrations/redshift/` | IAM/password auth, manifest-based COPY loading |
| **PostgreSQL** | `warehouse/integrations/postgres/` | Parallel loads (default: 3), SQL execution plans |
| **ClickHouse** | `warehouse/integrations/clickhouse/` | MergeTree engine, cluster support, array column support |
| **Databricks (Delta Lake)** | `warehouse/integrations/deltalake/` | MERGE/APPEND load strategies |
| **SQL Server (MSSQL)** | `warehouse/integrations/mssql/` | Bulk CopyIn ingestion |
| **Azure Synapse** | `warehouse/integrations/azure-synapse/` | COPY INTO ingestion |
| **Datalake (S3/GCS/Azure)** | `warehouse/integrations/datalake/` | Parquet/JSON exports to object storage |

The Warehouse service runs on port 8082 (configurable via `Warehouse.webPort`) and operates in `embedded` mode by default within the main rudder-server process.

> Source: `warehouse/integrations/`, `config/config.yaml:145-183`

**See also:**

- [Warehouse Overview](../warehouse/overview.md)
- [Snowflake Guide](../warehouse/snowflake.md) | [BigQuery Guide](../warehouse/bigquery.md) | [Redshift Guide](../warehouse/redshift.md)

---

### **Q: Is warehouse sync idempotent?**

Yes. The warehouse upload pipeline implements idempotent operations with merge and deduplication strategies. Each upload follows a 7-state state machine that ensures exactly-once semantics for successful loads:

```
Waiting → GeneratedUploadSchema → CreatedTableUploads → GeneratedLoadFiles
    → UpdatedTableUploadsCounts → ExportedData → ExportedUserAttributes
    ↘ Aborted (on unrecoverable failure)
```

Failed uploads retry with exponential backoff:

| Parameter | Default | Config Path |
|---|---|---|
| Minimum backoff | 60 seconds | `Warehouse.minUploadBackoff` |
| Maximum backoff | 1800 seconds (30 min) | `Warehouse.maxUploadBackoff` |
| Minimum retry attempts | 3 | `Warehouse.minRetryAttempts` |
| Retry time window | 180 minutes | `Warehouse.retryTimeWindow` |

The state machine transitions are managed by `warehouse/router/state.go`, which tracks upload progress and handles failure recovery with idempotent retry semantics.

> Source: `warehouse/router/state.go`, `config/config.yaml:152-155` — `minRetryAttempts: 3`, `minUploadBackoff: 60s`, `maxUploadBackoff: 1800s`

---

### **Q: Does warehouse sync support backfill?**

Yes. The warehouse service supports backfill through staging file re-processing. Key backfill mechanisms include:

1. **Staging file organization** — Staging files are stored in object storage organized by source, date, and hour (gzipped JSONL format), enabling targeted re-processing of historical data
2. **Batch processing** — The `stagingFilesBatchSize` parameter (default: 960) controls how many staging files are processed per upload cycle
3. **Automatic schema evolution** — New columns are automatically added to destination tables when previously unseen event properties are detected, ensuring backfilled data with new fields integrates cleanly

| Parameter | Default | Config Path |
|---|---|---|
| Staging files batch size | 960 | `Warehouse.stagingFilesBatchSize` |
| Upload frequency | 1800 seconds (30 min) | `Warehouse.uploadFreq` |
| Sync pre-fetch count | 10 | `Warehouse.warehouseSyncPreFetchCount` |

> Source: `warehouse/schema/`, `config/config.yaml:148,156-158` — `uploadFreq: 1800s`, `stagingFilesBatchSize: 960`

**See also:**

- [Warehouse Sync Operations](../guides/operations/warehouse-sync.md)
- [Schema Evolution](../warehouse/schema-evolution.md)
- [Encoding Formats](../warehouse/encoding-formats.md)

---

## Transformations

### **Q: What transformation capabilities does RudderStack support?**

RudderStack provides two transformation stages within the Processor pipeline:

| Stage | Purpose | Batch Size | Language Support |
|---|---|---|---|
| **User Transforms** | Custom event transformation logic defined by the user | 200 events/batch | JavaScript, Python |
| **Destination Transforms** | Payload shaping to match destination API requirements | 100 events/batch | JavaScript (built-in per destination) |

Both stages are executed by the external **Transformer service** (default URL: `http://localhost:9090`, configurable via the `DEST_TRANSFORM_URL` environment variable). The Transformer service runs as a separate Node.js process and handles JavaScript/Python code execution in a sandboxed environment.

The Processor pipeline executes transformations in order: User Transforms run first (allowing custom enrichment and filtering), followed by Destination Transforms (which shape the payload for each specific destination's API contract).

> Source: `config/config.yaml:191-192` — `transformBatchSize: 100`, `userTransformBatchSize: 200`; `config/sample.env:9` — `DEST_TRANSFORM_URL=http://localhost:9090`

**See also:**

- [Transformations Overview](../guides/transformations/overview.md)
- [User Transforms Guide](../guides/transformations/user-transforms.md)
- [Destination Transforms Guide](../guides/transformations/destination-transforms.md)

---

### **Q: How do RudderStack transformations compare to Segment Functions?**

RudderStack's transformation framework provides a partial equivalent to Segment Functions:

| Segment Functions Feature | RudderStack Equivalent | Parity |
|---|---|---|
| **Source Functions** (custom event ingestion) | User Transforms (post-ingestion) | Partial — transforms run after ingestion, not during |
| **Destination Functions** (custom delivery) | Destination Transforms | Partial — built-in per-destination; custom destination functions require Transformer extensions |
| **Insert Functions** (mid-pipeline injection) | User Transforms with filtering | Partial — can filter/modify events mid-pipeline |

Key differences:

- **Execution model:** Segment Functions run as serverless functions in Segment's infrastructure; RudderStack transforms run in the self-hosted Transformer service
- **Language support:** Segment Functions support JavaScript; RudderStack supports both JavaScript and Python
- **Batch processing:** RudderStack processes transforms in configurable batches (200 for user, 100 for destination), which may improve throughput for high-volume pipelines

> Source: `refs/segment-docs/src/connections/functions/`, `processor/pipeline_worker.go`

**See also:**

- [Functions Parity Analysis](../gap-report/functions-parity.md)
- [Glossary — Functions](./glossary.md)

---

## Performance and Capacity

### **Q: What throughput can RudderStack sustain?**

RudderStack targets **50,000 events per second** with per-user ordering guarantees. Achieving this throughput requires tuning the worker pool sizes across all pipeline stages:

| Component | Parameter | Default | Config Path |
|---|---|---|---|
| Gateway web workers | `maxUserWebRequestWorkerProcess` | 64 | `Gateway.maxUserWebRequestWorkerProcess` |
| Gateway DB writers | `maxDBWriterProcess` | 256 | `Gateway.maxDBWriterProcess` |
| Router workers | `noOfWorkers` | 64 | `Router.noOfWorkers` |
| Batch Router workers | `noOfWorkers` | 8 | `BatchRouter.noOfWorkers` |
| Warehouse workers | `noOfWorkers` | 8 | `Warehouse.noOfWorkers` |
| Warehouse slave routines | `noOfSlaveWorkerRoutines` | 4 | `Warehouse.noOfSlaveWorkerRoutines` |

For deployments exceeding the default capacity, consider splitting into **GATEWAY + PROCESSOR** mode to horizontally scale ingestion and processing independently.

> Source: `config/config.yaml:20-21` — `maxUserWebRequestWorkerProcess: 64`, `maxDBWriterProcess: 256`; `config/config.yaml:109` — `Router.noOfWorkers: 64`; `config/config.yaml:142` — `BatchRouter.noOfWorkers: 8`; `config/config.yaml:149` — `Warehouse.noOfWorkers: 8`

**See also:**

- [Capacity Planning Guide](../guides/operations/capacity-planning.md)
- [Configuration Reference — Gateway](./config-reference.md)

---

### **Q: How does event ordering work?**

RudderStack guarantees **per-user event ordering** throughout the pipeline. Events for the same user (identified by `userId` or `anonymousId`) are processed and delivered to destinations in the order they were received.

The ordering guarantee is controlled by the `guaranteeUserEventOrder` configuration parameter (default: `true`). When enabled, the Router ensures that:

1. Events for the same user are not delivered concurrently to the same destination
2. If delivery fails for a user's event, subsequent events for that user are held until the failed event is retried successfully or aborted
3. The maximum number of aborted user jobs before processing resumes is controlled by `allowAbortedUserJobsCountForProcessing` (default: 1)

| Parameter | Default | Config Path |
|---|---|---|
| Guarantee user event order | `true` | `Router.guaranteeUserEventOrder` |
| Aborted jobs threshold | 1 | `Router.allowAbortedUserJobsCountForProcessing` |
| Max failed count per job | 3 | `Router.maxFailedCountForJob` |
| Retry time window | 180 minutes | `Router.retryTimeWindow` |

> Source: `config/config.yaml:104` — `guaranteeUserEventOrder: true`; `config/config.yaml:110-112` — `allowAbortedUserJobsCountForProcessing: 1`, `maxFailedCountForJob: 3`, `retryTimeWindow: 180m`

---

### **Q: How does rate limiting work?**

RudderStack implements rate limiting at two levels:

**1. Global API Rate Limiting (Gateway)**

The Gateway enforces per-source rate limiting to prevent individual sources from overwhelming the pipeline:

| Parameter | Default | Config Path |
|---|---|---|
| Event limit per window | 1000 | `RateLimit.eventLimit` |
| Rate limit window | 60 minutes | `RateLimit.rateLimitWindow` |
| Number of buckets | 12 | `RateLimit.noOfBucketsInWindow` |
| Enable rate limiting | `false` | `Gateway.enableRateLimit` |

> **Note:** Gateway rate limiting is disabled by default (`enableRateLimit: false`). Enable it in production environments where per-source throttling is required.

**2. Per-Destination Throttling (Router)**

The Router uses the **GCRA (Generic Cell Rate Algorithm)** to enforce per-destination delivery rate limits. Throttling can be configured globally or per integration:

| Integration | Limit | Time Window | Config Path |
|---|---|---|---|
| MARKETO | 45 requests | 20 seconds | `Router.throttler.MARKETO` |
| *(custom)* | Configurable | Configurable | `Router.throttler.<DESTINATION_NAME>` |

Per-destination throttling can also be applied at the `destinationID` level for fine-grained control.

> Source: `config/config.yaml:14-17` — `eventLimit: 1000`, `rateLimitWindow: 60m`, `noOfBucketsInWindow: 12`; `config/config.yaml:28` — `enableRateLimit: false`; `config/config.yaml:122,127-129` — `algorithm: gcra`, `MARKETO: limit: 45, timeWindow: 20s`

---

## Operations and Troubleshooting

### **Q: How does event replay work?**

The Archiver component periodically archives processed events to object storage in gzipped JSONL format. Archived events are organized by source, date, and hour, with a default retention period of 10 days.

**Archival configuration:**

| Parameter | Default | Config Path |
|---|---|---|
| Archival time in days | 10 | `JobsDB.archivalTimeInDays` |
| Archiver ticker time | 1440 minutes (24h) | `JobsDB.archiverTickerTime` |
| Backup rows batch size | 1000 | `JobsDB.backupRowsBatchSize` |
| Backup enabled | `true` | `JobsDB.backup.enabled` |

**Replay process:**

Replay is triggered via the `POST /v1/replay` HTTP endpoint on the Gateway. This re-ingests archived events through the full pipeline (processing, routing, warehouse loading), allowing historical data to be reprocessed against updated transformations, new destinations, or corrected configurations.

Replay types are defined in `backend-config/replay_types.go` and support filtering by source, date range, and destination.

> Source: `archiver/`, `gateway/handle_http_replay.go`, `backend-config/replay_types.go`, `config/config.yaml:76-78` — `archivalTimeInDays: 10`, `archiverTickerTime: 1440m`

**See also:**

- [Replay Operations Guide](../guides/operations/replay.md)

---

### **Q: How does RudderStack handle GDPR compliance?**

RudderStack provides comprehensive GDPR compliance through three mechanisms:

1. **Regulation Worker** — Enforces data deletion requests through three strategies:
   - **API deletion** — Sends deletion requests to destination APIs
   - **Batch deletion** — Processes bulk deletion files for batch destinations
   - **KV store deletion** — Removes data from key-value store destinations

2. **User Suppression** — Prevents data collection for suppressed users. When a user is added to the suppression list, the Gateway drops all incoming events for that user before they enter the pipeline. Controlled by `Gateway.enableSuppressUserFeature` (default: `true`).

3. **OAuth v2 Integration** — The regulation worker integrates with OAuth v2 for authenticated deletion requests to destinations that require OAuth credentials.

| Parameter | Default | Config Path |
|---|---|---|
| Enable suppress user feature | `true` | `Gateway.enableSuppressUserFeature` |
| Regulations poll interval | 300 seconds | `BackendConfig.regulationsPollInterval` |
| Max regulations per request | 1000 | `BackendConfig.maxRegulationsPerRequest` |

> Source: `regulation-worker/`, `enterprise/suppress-user/`, `config/config.yaml:29` — `enableSuppressUserFeature: true`; `config/config.yaml:212-213` — `regulationsPollInterval: 300s`, `maxRegulationsPerRequest: 1000`

**See also:**

- [Privacy Compliance Guide](../guides/operations/privacy-compliance.md)

---

### **Q: How do I monitor RudderStack?**

RudderStack exposes diagnostics and runtime metrics through a built-in instrumentation framework:

**Diagnostics** (enabled by default):

| Metric Category | Config Path | Default Period |
|---|---|---|
| Gateway metrics | `Diagnostics.enableGatewayMetric` | 60 seconds |
| Router metrics | `Diagnostics.enableRouterMetric` | 60 seconds |
| Batch Router metrics | `Diagnostics.enableBatchRouterMetric` | 60 seconds |
| Destination failure metrics | `Diagnostics.enableDestinationFailuresMetric` | Enabled |
| Server start metrics | `Diagnostics.enableServerStartMetric` | Enabled |
| Config processed metrics | `Diagnostics.enableConfigProcessedMetric` | Enabled |

**Runtime Statistics:**

Runtime stats collection is enabled by default and gathers system-level metrics every 10 seconds:

| Parameter | Default | Config Path |
|---|---|---|
| Stats collection interval | 10 seconds | `RuntimeStats.statsCollectionInterval` |
| CPU stats | Enabled | `RuntimeStats.enableCPUStats` |
| Memory stats | Enabled | `RuntimeStats.enableMemStats` |
| GC stats | Enabled | `RuntimeStats.enableGCStats` |

**StatsD Export:**

Metrics can be exported to a StatsD-compatible backend by setting the `STATSD_SERVER_URL` environment variable. The stats tags format defaults to `influxdb` (configurable via `statsTagsFormat`).

> Source: `config/config.yaml:227-245` — Diagnostics and RuntimeStats configuration; `config/sample.env:21` — `STATSD_SERVER_URL`; `config/config.yaml:5` — `statsTagsFormat: influxdb`

---

### **Q: What logging options are available?**

RudderStack provides configurable logging with the following options:

| Parameter | Default | Config Path |
|---|---|---|
| Enable console logging | `true` | `Logger.enableConsole` |
| Enable file logging | `false` | `Logger.enableFile` |
| Console JSON format | `false` | `Logger.consoleJsonFormat` |
| File JSON format | `false` | `Logger.fileJsonFormat` |
| Log file location | `/tmp/rudder_log.log` | `Logger.logFileLocation` |
| Log file size (MB) | 100 | `Logger.logFileSize` |
| Enable timestamps | `true` | `Logger.enableTimestamp` |
| Enable filename in log | `true` | `Logger.enableFileNameInLog` |
| Enable stack trace | `false` | `Logger.enableStackTrace` |

The log level is controlled via the `LOG_LEVEL` environment variable (default: `INFO`). Accepted values: `DEBUG`, `INFO`, `WARN`, `ERROR`.

> Source: `config/config.yaml:217-226`, `config/sample.env:18` — `LOG_LEVEL=INFO`

---

### **Q: How does deduplication work?**

RudderStack includes an optional event deduplication service that prevents duplicate events from being processed:

| Parameter | Default | Config Path |
|---|---|---|
| Enable dedup | `false` | `Dedup.enableDedup` |
| Dedup window | 3600 seconds (1 hour) | `Dedup.dedupWindow` |
| Memory optimized | `true` | `Dedup.memOptimized` |

When enabled, the dedup service maintains a sliding window of recently seen message IDs. Duplicate events (identified by the `messageId` field) received within the dedup window are dropped before entering the processing pipeline. The service supports BadgerDB-backed and KeyDB-backed implementations with configurable memory optimization.

> Source: `config/config.yaml:204-207` — `enableDedup: false`, `dedupWindow: 3600s`, `memOptimized: true`; `services/dedup/`

---

### **Q: How does backend configuration work?**

RudderStack dynamically loads workspace configuration from a control plane backend:

| Parameter | Default | Config Path |
|---|---|---|
| Config from file | `false` | `BackendConfig.configFromFile` |
| Config JSON path | `/etc/rudderstack/workspaceConfig.json` | `BackendConfig.configJSONPath` |
| Poll interval | 5 seconds | `BackendConfig.pollInterval` |

In production mode, the server polls the control plane (URL set via `CONFIG_BACKEND_URL`, default: `https://api.rudderstack.com`) every 5 seconds for updated workspace configuration. Authentication is performed using the `WORKSPACE_TOKEN` environment variable.

For air-gapped or development deployments, configuration can be loaded from a local JSON file by setting `RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE=true` and providing the file path via `RSERVER_BACKEND_CONFIG_CONFIG_JSONPATH`.

The backend configuration includes AES-GCM encrypted caching for resilience — if the control plane becomes unavailable, the server falls back to the most recently cached configuration.

> Source: `config/config.yaml:208-213` — `configFromFile: false`, `pollInterval: 5s`; `config/sample.env:12-14` — `CONFIG_BACKEND_URL`, `WORKSPACE_TOKEN`; `backend-config/`

---

### **Q: How do I contribute to RudderStack?**

Contributing to RudderStack requires the following steps:

1. **Sign the CLA** — Complete the [Contributor License Agreement](https://forms.gle/845JRGVZaC6kPZy68) before your first commit
2. **Fork the repository** — Create a personal fork of `rudder-server` on GitHub
3. **Submit a pull request** — Squashed or rebased commits are preferred so all changes from a branch appear as a single commit on master
4. **Destination connectors** — For new destination integrations, work with the [`rudder-transformer`](https://github.com/rudderlabs/rudder-transformer) repository, which contains the JavaScript/Python transformation code for each destination

> Source: `CONTRIBUTING.md` — "We prefer squash or rebase commits so that all changes from a branch are committed to master as a single commit"

**See also:**

- [Development Setup Guide](../contributing/development.md)
- [Destination Onboarding Guide](../contributing/destination-onboarding.md)
- [Testing Guide](../contributing/testing.md)

---

### **Q: What are the key environment variables I need to set?**

The essential environment variables for a production RudderStack deployment:

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `WORKSPACE_TOKEN` | **Yes** | *(none)* | Authentication token for control plane API |
| `CONFIG_BACKEND_URL` | No | `https://api.rudderstack.com` | Control plane backend URL |
| `CONFIG_PATH` | No | `./config/config.yaml` | Path to YAML configuration file |
| `JOBS_DB_HOST` | **Yes** | `localhost` | PostgreSQL host for JobsDB |
| `JOBS_DB_USER` | **Yes** | `rudder` | PostgreSQL username |
| `JOBS_DB_PASSWORD` | **Yes** | `rudder` | PostgreSQL password |
| `JOBS_DB_DB_NAME` | **Yes** | `jobsdb` | PostgreSQL database name |
| `JOBS_DB_PORT` | No | `5432` | PostgreSQL port |
| `JOBS_DB_SSL_MODE` | No | `disable` | PostgreSQL SSL mode |
| `DEST_TRANSFORM_URL` | No | `http://localhost:9090` | Transformer service URL |
| `LOG_LEVEL` | No | `INFO` | Logging verbosity (`DEBUG`, `INFO`, `WARN`, `ERROR`) |
| `STATSD_SERVER_URL` | No | *(disabled)* | StatsD metrics endpoint |

> Source: `config/sample.env:1-21`

**See also:**

- [Environment Variables Reference](./env-var-reference.md)

---

*For terminology clarification, see the [Glossary](./glossary.md). For complete configuration parameter documentation, see the [Configuration Reference](./config-reference.md).*
