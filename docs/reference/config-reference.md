# Configuration Reference

Complete reference for all 200+ configuration parameters in RudderStack (`rudder-server` v1.68.1). Parameters are defined in `config/config.yaml` and organized by subsystem.

> **Source:** `config/config.yaml` (250 lines)

**Environment variable overrides:** Any YAML configuration parameter can be overridden via environment variable using the `RSERVER_` prefix with path segments separated by underscores and camelCase expanded to UPPER_SNAKE_CASE. For example:

| YAML Parameter | Environment Variable |
|---|---|
| `Gateway.webPort` | `RSERVER_GATEWAY_WEB_PORT` |
| `Router.noOfWorkers` | `RSERVER_ROUTER_NO_OF_WORKERS` |
| `Warehouse.mode` | `RSERVER_WAREHOUSE_MODE` |
| `Processor.transformBatchSize` | `RSERVER_PROCESSOR_TRANSFORM_BATCH_SIZE` |

For the complete environment variable reference, see [Environment Variable Reference](./env-var-reference.md).

For terminology used in this document, see [Glossary](./glossary.md).

---

## Table of Contents

- [Global Parameters](#global-parameters)
- [HTTP Client & Server](#http-client--server)
- [Rate Limiting](#rate-limiting)
- [Gateway](#gateway)
  - [Gateway Webhook Configuration](#gateway-webhook-configuration)
- [Event Schemas & Debugging](#event-schemas--debugging)
  - [Event Schemas](#event-schemas)
  - [Debugger](#debugger)
  - [Live Event Cache](#live-event-cache)
  - [Component Debugger Toggles](#component-debugger-toggles)
- [Archiver](#archiver)
- [JobsDB](#jobsdb)
  - [JobsDB Backup Configuration](#jobsdb-backup-configuration)
  - [JobsDB Gateway Writer](#jobsdb-gateway-writer)
- [Router](#router)
  - [Router Integration-Specific Settings](#router-integration-specific-settings)
  - [Router Throttling](#router-throttling)
- [Batch Router](#batch-router)
- [Warehouse](#warehouse)
  - [Warehouse Per-Connector Settings](#warehouse-per-connector-settings)
- [Processor](#processor)
- [Deduplication](#deduplication)
- [Backend Configuration](#backend-configuration)
- [Logger](#logger)
- [Diagnostics](#diagnostics)
- [Runtime Statistics](#runtime-statistics)
- [PostgreSQL Notifier](#postgresql-notifier)

---

## Global Parameters

Root-level parameters controlling system-wide behavior and component enablement.

> Source: `config/config.yaml:1-5`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `maxProcess` | `12` | integer | ≥ 1 | Maximum number of OS processes (GOMAXPROCS). Controls CPU parallelism for the Go runtime. |
| `enableProcessor` | `true` | boolean | `true` / `false` | Enable the Processor component. Set to `false` in GATEWAY-only deployment mode. |
| `enableRouter` | `true` | boolean | `true` / `false` | Enable the Router component. Set to `false` in GATEWAY-only deployment mode. |
| `enableStats` | `true` | boolean | `true` / `false` | Enable the statistics collection subsystem for metrics export. |
| `statsTagsFormat` | `influxdb` | string | `influxdb` | Stats tag format for metric labels. Used by the internal stats instrumentation. |

---

## HTTP Client & Server

Global HTTP client timeout and HTTP server configuration for all RudderStack endpoints.

> Source: `config/config.yaml:6-13`

### HTTP Client

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `HttpClient.timeout` | `30s` | duration | ≥ 0s | Global timeout for outbound HTTP client requests. Applies to all HTTP calls made by the server (e.g., to Transformer service, destination APIs). A value of `0s` disables the timeout. |

### HTTP Server

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Http.ReadTimeout` | `0s` | duration | ≥ 0s | Maximum duration for reading the entire HTTP request, including the body. `0s` means no timeout. |
| `Http.ReadHeaderTimeout` | `0s` | duration | ≥ 0s | Maximum duration for reading HTTP request headers. `0s` means no timeout. |
| `Http.WriteTimeout` | `10s` | duration | ≥ 0s | Maximum duration before timing out writes of the HTTP response. |
| `Http.IdleTimeout` | `720s` | duration | ≥ 0s | Maximum duration an idle (keep-alive) connection will remain open. 720s = 12 minutes. |
| `Http.MaxHeaderBytes` | `524288` | integer | ≥ 1 | Maximum size of HTTP request headers in bytes. Default is 512 KB (524,288 bytes). |

---

## Rate Limiting

Global API rate limiting applied at the Gateway ingestion layer. These settings control the overall event acceptance rate across all sources.

> **Note:** This is the global API rate limit. Per-destination throttling is configured under [Router Throttling](#router-throttling). Rate limiting must be explicitly enabled via `Gateway.enableRateLimit`.

> Source: `config/config.yaml:14-17`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `RateLimit.eventLimit` | `1000` | integer | ≥ 1 | Maximum number of events accepted per rate limit window. |
| `RateLimit.rateLimitWindow` | `60m` | duration | > 0s | Sliding window duration for rate limit calculations. Default is 60 minutes. |
| `RateLimit.noOfBucketsInWindow` | `12` | integer | ≥ 1 | Number of sub-buckets dividing the rate limit window. Higher values provide finer-grained rate limiting. With default values, each bucket covers 5 minutes. |

---

## Gateway

The Gateway is the HTTP ingestion component (default port 8080) accepting Segment-compatible event payloads. These parameters control worker pools, batching, request validation, and feature toggles.

> Source: `config/config.yaml:18-40`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Gateway.webPort` | `8080` | integer | 1–65535 | HTTP listen port for the Gateway. All event ingestion endpoints are served on this port. |
| `Gateway.maxUserWebRequestWorkerProcess` | `64` | integer | ≥ 1 | Size of the worker pool processing incoming web requests. Each worker handles request parsing, validation, and enqueuing. |
| `Gateway.maxDBWriterProcess` | `256` | integer | ≥ 1 | Size of the worker pool writing accepted events to JobsDB. Higher values improve write throughput under load. |
| `Gateway.CustomVal` | `GW` | string | — | Custom value identifier stamped on jobs created by the Gateway. Used internally for job classification. |
| `Gateway.maxUserRequestBatchSize` | `128` | integer | ≥ 1 | Maximum number of user requests accumulated in a single batch before flushing to processing. |
| `Gateway.maxDBBatchSize` | `128` | integer | ≥ 1 | Maximum number of events written to the database in a single batch operation. |
| `Gateway.userWebRequestBatchTimeout` | `15ms` | duration | ≥ 0ms | Maximum time to wait for accumulating a full request batch before flushing a partial batch. |
| `Gateway.dbBatchWriteTimeout` | `5ms` | duration | ≥ 0ms | Maximum time to wait for accumulating a full DB write batch before flushing. |
| `Gateway.maxReqSizeInKB` | `4000` | integer | ≥ 1 | Maximum allowed request body size in kilobytes. Default is 4,000 KB (~4 MB). Requests exceeding this size are rejected. |
| `Gateway.enableRateLimit` | `false` | boolean | `true` / `false` | Enable the global API rate limiting feature. When `false`, rate limit parameters under `RateLimit.*` have no effect. |
| `Gateway.enableSuppressUserFeature` | `true` | boolean | `true` / `false` | Enable user suppression checks. When enabled, events from suppressed user IDs are dropped at the Gateway. |
| `Gateway.allowPartialWriteWithErrors` | `true` | boolean | `true` / `false` | When `true`, a batch request partially succeeds if some events are valid but others fail validation. When `false`, the entire batch is rejected if any event fails. |
| `Gateway.allowReqsWithoutUserIDAndAnonymousID` | `false` | boolean | `true` / `false` | When `false`, events lacking both `userId` and `anonymousId` are rejected. Set to `true` to accept events without user identifiers. |

### Gateway Webhook Configuration

Configuration for the webhook ingestion subsystem within the Gateway.

> Source: `config/config.yaml:32-40`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Gateway.webhook.batchTimeout` | `20ms` | duration | ≥ 0ms | Maximum time to accumulate webhook events into a batch before dispatching to the Transformer. |
| `Gateway.webhook.maxBatchSize` | `32` | integer | ≥ 1 | Maximum number of webhook events per batch sent to the Transformer service. |
| `Gateway.webhook.maxTransformerProcess` | `64` | integer | ≥ 1 | Number of concurrent workers sending webhook batches to the Transformer for payload conversion. |
| `Gateway.webhook.maxRetry` | `5` | integer | ≥ 0 | Maximum number of retry attempts for failed webhook deliveries to the Transformer. |
| `Gateway.webhook.maxRetryTime` | `10s` | duration | ≥ 0s | Maximum total time allowed for webhook retry attempts before the batch is dropped. |
| `Gateway.webhook.sourceListForParsingParams` | `[shopify, adjust]` | string list | — | List of webhook source types for which query parameters and path parameters are parsed and included in the event payload. |

---

## Event Schemas & Debugging

Parameters controlling event schema syncing, debug event collection, live event caching, and per-component debug toggles.

### Event Schemas

> Source: `config/config.yaml:41-44`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `EventSchemas.enableEventSchemasFeature` | `false` | boolean | `true` / `false` | Enable the event schema discovery and synchronization feature. When enabled, observed event schemas are tracked and synced to the control plane. |
| `EventSchemas.syncInterval` | `240s` | duration | ≥ 1s | Interval between event schema synchronization cycles with the control plane. Default is 4 minutes. |
| `EventSchemas.noOfWorkers` | `128` | integer | ≥ 1 | Number of concurrent workers processing event schema observations. |

### Debugger

General debugger settings for the event inspection subsystem.

> Source: `config/config.yaml:45-50`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Debugger.maxBatchSize` | `32` | integer | ≥ 1 | Maximum number of debug events accumulated per batch before uploading. |
| `Debugger.maxESQueueSize` | `1024` | integer | ≥ 1 | Maximum capacity of the in-memory debug event queue. Events beyond this limit are dropped. |
| `Debugger.maxRetry` | `3` | integer | ≥ 0 | Maximum retry attempts for failed debug event uploads. |
| `Debugger.batchTimeout` | `2s` | duration | ≥ 0s | Maximum time to accumulate debug events before flushing a partial batch. |
| `Debugger.retrySleep` | `100ms` | duration | ≥ 0ms | Sleep duration between debug upload retry attempts. |

### Live Event Cache

In-memory cache for live event inspection via the debugger UI.

> Source: `config/config.yaml:51-55`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `LiveEvent.cache.size` | `3` | integer | ≥ 1 | Maximum number of live event entries retained in the cache per source. |
| `LiveEvent.cache.ttl` | `20d` | duration | ≥ 1s | Time-to-live for cached live events. Default is 20 days. |
| `LiveEvent.cache.clearFreq` | `5s` | duration | ≥ 1s | Frequency at which expired cache entries are cleared. |

### Component Debugger Toggles

Per-component toggles to disable debug event uploads for specific pipeline stages.

> Source: `config/config.yaml:56-61`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `SourceDebugger.disableEventUploads` | `false` | boolean | `true` / `false` | Disable source-level debug event uploads. Set to `true` to reduce overhead in production. |
| `DestinationDebugger.disableEventDeliveryStatusUploads` | `false` | boolean | `true` / `false` | Disable destination delivery status debug uploads. |
| `TransformationDebugger.disableTransformationStatusUploads` | `false` | boolean | `true` / `false` | Disable transformation status debug uploads. |

---

## Archiver

Configuration for the event archival subsystem. The Archiver periodically exports processed events to object storage for long-term retention and replay capability.

> Source: `config/config.yaml:62-63`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Archiver.backupRowsBatchSize` | `100` | integer | ≥ 1 | Number of rows per batch during archival backup operations. Controls memory usage during the backup export process. |

---

## JobsDB

The JobsDB is the PostgreSQL-backed persistent job queue that durably stores events at each pipeline stage. These parameters control dataset partitioning, migration thresholds, archival, and backup behavior.

> Source: `config/config.yaml:64-91`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `JobsDB.jobDoneMigrateThres` | `0.8` | float | 0.0–1.0 | Ratio of completed jobs in a dataset that triggers migration. When 80% of jobs are done, the dataset becomes eligible for migration. |
| `JobsDB.jobStatusMigrateThres` | `5` | integer | ≥ 1 | Number of status records per job that triggers migration eligibility. |
| `JobsDB.maxDSSize` | `100000` | integer | ≥ 1 | Maximum number of rows in a single dataset partition. When exceeded, a new dataset is created. |
| `JobsDB.maxMigrateOnce` | `10` | integer | ≥ 1 | Maximum number of datasets migrated in a single migration cycle. |
| `JobsDB.maxMigrateDSProbe` | `10` | integer | ≥ 1 | Maximum number of datasets probed during migration eligibility checks. |
| `JobsDB.maxTableSizeInMB` | `300` | integer | ≥ 1 (MB) | Maximum table size in megabytes before rotation to a new dataset. |
| `JobsDB.migrateDSLoopSleepDuration` | `30s` | duration | ≥ 1s | Sleep duration between dataset migration loop iterations. |
| `JobsDB.addNewDSLoopSleepDuration` | `5s` | duration | ≥ 1s | Sleep duration between new dataset creation checks. |
| `JobsDB.refreshDSListLoopSleepDuration` | `5s` | duration | ≥ 1s | Sleep duration between dataset list refresh cycles. |
| `JobsDB.backupCheckSleepDuration` | `5s` | duration | ≥ 1s | Sleep duration between backup eligibility checks. |
| `JobsDB.backupRowsBatchSize` | `1000` | integer | ≥ 1 | Number of rows per batch during JobsDB backup export. |
| `JobsDB.archivalTimeInDays` | `10` | integer | ≥ 1 (days) | Number of days after which completed datasets are archived. Default retention is 10 days. |
| `JobsDB.archiverTickerTime` | `1440m` | duration | ≥ 1m | Interval between archiver runs. Default is 1440 minutes (24 hours). |

### JobsDB Backup Configuration

Controls which pipeline stages have their JobsDB tables backed up to object storage.

> Source: `config/config.yaml:78-88`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `JobsDB.backup.enabled` | `true` | boolean | `true` / `false` | Master toggle for all JobsDB backup operations. |
| `JobsDB.backup.gw.enabled` | `true` | boolean | `true` / `false` | Enable backup of Gateway JobsDB tables. |
| `JobsDB.backup.gw.pathPrefix` | `""` | string | — | Object storage path prefix for Gateway backups. Empty string uses the default path. |
| `JobsDB.backup.rt.enabled` | `true` | boolean | `true` / `false` | Enable backup of Router JobsDB tables. |
| `JobsDB.backup.rt.failedOnly` | `true` | boolean | `true` / `false` | When `true`, only failed Router jobs are backed up (reduces storage usage). |
| `JobsDB.backup.batch_rt.enabled` | `false` | boolean | `true` / `false` | Enable backup of Batch Router JobsDB tables. Disabled by default. |
| `JobsDB.backup.batch_rt.failedOnly` | `false` | boolean | `true` / `false` | When `true`, only failed Batch Router jobs are backed up. |

### JobsDB Gateway Writer

Controls the Gateway's database writer queue behavior.

> Source: `config/config.yaml:89-91`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `JobsDB.gw.enableWriterQueue` | `false` | boolean | `true` / `false` | Enable a dedicated writer queue for Gateway DB operations. When disabled, writes are synchronous. |
| `JobsDB.gw.maxOpenConnections` | `64` | integer | ≥ 1 | Maximum number of open PostgreSQL connections for the Gateway JobsDB writer. |

---

## Router

The Router handles real-time delivery of events to destination APIs. It manages per-destination worker pools, batching, retry logic, ordering guarantees, and adaptive sleep patterns.

> Source: `config/config.yaml:92-137`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Router.jobQueryBatchSize` | `10000` | integer | ≥ 1 | Number of jobs fetched from JobsDB per query batch. Higher values reduce DB round trips but increase memory usage. |
| `Router.updateStatusBatchSize` | `1000` | integer | ≥ 1 | Number of job status updates batched in a single DB write operation. |
| `Router.readSleep` | `1000ms` | duration | ≥ 0ms | Sleep duration between read loop iterations when no jobs are found. |
| `Router.fixedLoopSleep` | `0ms` | duration | ≥ 0ms | Fixed sleep between processing loops. `0ms` enables adaptive sleep (adjusts based on job availability). |
| `Router.noOfJobsPerChannel` | `1000` | integer | ≥ 1 | Maximum number of jobs buffered in each worker's channel. Controls backpressure between job fetcher and workers. |
| `Router.noOfJobsToBatchInAWorker` | `20` | integer | ≥ 1 | Number of jobs batched together in a single worker before dispatching to the destination API. |
| `Router.jobsBatchTimeout` | `5s` | duration | ≥ 0ms | Maximum time a worker waits to accumulate a full batch before flushing a partial batch. |
| `Router.maxSleep` | `60s` | duration | ≥ 0s | Maximum adaptive sleep duration when no jobs are available. The Router backs off up to this limit. |
| `Router.minSleep` | `0s` | duration | ≥ 0s | Minimum adaptive sleep duration. The Router does not sleep less than this value between loops. |
| `Router.maxStatusUpdateWait` | `5s` | duration | ≥ 0s | Maximum time to wait for aggregating status updates before flushing to JobsDB. |
| `Router.useTestSink` | `false` | boolean | `true` / `false` | Route all events to a test sink endpoint instead of real destinations. For development and testing only. |
| `Router.guaranteeUserEventOrder` | `true` | boolean | `true` / `false` | Guarantee per-user event ordering during delivery. When `true`, events for the same user are delivered sequentially. Disabling improves throughput at the cost of ordering. |
| `Router.kafkaWriteTimeout` | `2s` | duration | ≥ 0s | Write timeout for Kafka producer operations. |
| `Router.kafkaDialTimeout` | `10s` | duration | ≥ 0s | Connection dial timeout for Kafka brokers. |
| `Router.minRetryBackoff` | `10s` | duration | ≥ 0s | Minimum backoff duration between retry attempts for failed deliveries. |
| `Router.maxRetryBackoff` | `300s` | duration | ≥ 0s | Maximum backoff duration between retry attempts. Default is 5 minutes. Backoff increases exponentially from `minRetryBackoff` to this ceiling. |
| `Router.noOfWorkers` | `64` | integer | ≥ 1 | Size of the Router worker pool. Each worker handles delivery to a specific destination. Scale this based on the number of active destinations and required throughput. |
| `Router.allowAbortedUserJobsCountForProcessing` | `1` | integer | ≥ 0 | Number of aborted jobs for a user that are allowed before blocking further processing for that user. Prevents a single failing user from consuming all retry resources. |
| `Router.maxFailedCountForJob` | `3` | integer | ≥ 1 | Maximum number of delivery failures before a job is permanently aborted. |
| `Router.retryTimeWindow` | `180m` | duration | ≥ 0m | Total time window during which failed jobs are retried. Default is 3 hours. After this window, undelivered jobs are aborted. |
| `Router.failedKeysEnabled` | `true` | boolean | `true` / `false` | Enable tracking of failed user/destination key pairs to optimize retry scheduling. |
| `Router.saveDestinationResponseOverride` | `false` | boolean | `true` / `false` | Save full destination API responses for debugging. Increases storage usage when enabled. |
| `Router.transformerProxy` | `false` | boolean | `true` / `false` | Route destination API calls through the Transformer service as a proxy. Useful for destinations requiring complex request construction. |
| `Router.transformerProxyRetryCount` | `15` | integer | ≥ 0 | Number of retry attempts when using the Transformer as a proxy. |

### Router Integration-Specific Settings

Per-destination overrides for worker pool sizes and HTTP client behavior. These settings allow fine-tuning for destinations with specific rate limits or performance characteristics.

> Source: `config/config.yaml:117-137`

**Worker Pool Overrides:**

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Router.GOOGLESHEETS.noOfWorkers` | `1` | integer | ≥ 1 | Dedicated worker count for Google Sheets. Set to 1 due to strict API rate limits. |
| `Router.MARKETO.noOfWorkers` | `4` | integer | ≥ 1 | Dedicated worker count for Marketo. Reduced from the global default due to Marketo API rate limits. |

**Braze-Specific Settings:**

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Router.BRAZE.forceHTTP1` | `true` | boolean | `true` / `false` | Force HTTP/1.1 for Braze API requests. Set to `true` to avoid HTTP/2 compatibility issues with the Braze API. |
| `Router.BRAZE.httpTimeout` | `120s` | duration | ≥ 1s | HTTP timeout for Braze API requests. Extended to 2 minutes due to Braze's response latency profile. |
| `Router.BRAZE.httpMaxIdleConnsPerHost` | `32` | integer | ≥ 1 | Maximum idle HTTP connections per Braze API host. Controls connection pool size for persistent connections. |

> **Custom overrides:** Any destination type can have dedicated settings by using the pattern `Router.<DESTINATION_TYPE>.<setting>`. For example: `Router.AMPLITUDE.noOfWorkers: 32`.

### Router Throttling

Rate limiting applied per-destination using the Generic Cell Rate Algorithm (GCRA). The throttler prevents overwhelming destination APIs with excessive request rates.

> Source: `config/config.yaml:121-133`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Router.throttler.algorithm` | `gcra` | string | `gcra` | Throttling algorithm. GCRA (Generic Cell Rate Algorithm) provides smooth, token-bucket-style rate limiting. |
| `Router.throttler.MARKETO.limit` | `45` | integer | ≥ 1 | Maximum number of requests allowed per time window for Marketo destinations. |
| `Router.throttler.MARKETO.timeWindow` | `20s` | duration | ≥ 1s | Time window for the Marketo rate limit. Combined with `limit`, this allows 45 requests per 20 seconds. |

> **Per-destination throttling:** You can configure throttling for any destination type using the pattern:
> ```yaml
> Router:
>   throttler:
>     <DESTINATION_TYPE>:
>       limit: <max_requests>
>       timeWindow: <window_duration>
> ```
>
> **Per-destination-ID throttling:** For granular control, throttle by individual destination connection ID:
> ```yaml
> Router:
>   throttler:
>     <DESTINATION_TYPE>:
>       <destinationID>:
>         limit: 90
>         timeWindow: 10s
> ```
> Source: `config/config.yaml:130-133` (commented example)

---

## Batch Router

The Batch Router handles bulk delivery of events to batch-oriented destinations (warehouses, cloud storage, batch APIs). Events are accumulated and uploaded in periodic batches rather than real-time.

> Source: `config/config.yaml:138-144`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `BatchRouter.jobQueryBatchSize` | `100000` | integer | ≥ 1 | Number of jobs fetched from JobsDB per query batch. Larger than Router's default (10,000) because batch operations process more jobs per cycle. |
| `BatchRouter.uploadFreq` | `30s` | duration | ≥ 1s | Upload cadence — how frequently the Batch Router flushes accumulated events to destinations. |
| `BatchRouter.warehouseServiceMaxRetryTime` | `3h` | duration | ≥ 0s | Maximum total retry duration for warehouse service interactions before giving up. |
| `BatchRouter.noOfWorkers` | `8` | integer | ≥ 1 | Number of Batch Router workers. Each worker handles uploads for a partition of batch destinations. |
| `BatchRouter.maxFailedCountForJob` | `128` | integer | ≥ 1 | Maximum number of failures before a batch job is permanently aborted. Higher than Router's default (3) because batch operations experience transient failures more frequently. |
| `BatchRouter.retryTimeWindow` | `180m` | duration | ≥ 0m | Total time window during which failed batch jobs are retried. Default is 3 hours. |

---

## Warehouse

The Warehouse service manages loading events into data warehouses. It operates as an embedded service (default) or standalone service (port 8082), orchestrating the 7-state upload lifecycle: staging → schema management → parallel loading → verification.

> Source: `config/config.yaml:145-183`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Warehouse.mode` | `embedded` | string | `embedded`, `off`, `master`, `slave`, `master_and_slave` | Warehouse operating mode. `embedded` runs within the main process. `off` disables the warehouse service. `master`/`slave` modes enable distributed processing. |
| `Warehouse.webPort` | `8082` | integer | 1–65535 | HTTP and gRPC listen port for the Warehouse service. Used for warehouse API endpoints and inter-service communication. |
| `Warehouse.uploadFreq` | `1800s` | duration | ≥ 1s | Frequency of warehouse upload cycles. Default is 30 minutes. Controls how often new staging files are processed into warehouse loads. |
| `Warehouse.noOfWorkers` | `8` | integer | ≥ 1 | Number of warehouse upload workers. Each worker manages upload lifecycles for a partition of warehouse destinations. |
| `Warehouse.noOfSlaveWorkerRoutines` | `4` | integer | ≥ 1 | Number of goroutines per slave worker for parallel load file generation. Only applicable in `slave` or `master_and_slave` modes. |
| `Warehouse.mainLoopSleep` | `5s` | duration | ≥ 1s | Sleep duration between main orchestration loop iterations. |
| `Warehouse.minRetryAttempts` | `3` | integer | ≥ 1 | Minimum number of retry attempts for failed warehouse uploads before exponential backoff kicks in. |
| `Warehouse.retryTimeWindow` | `180m` | duration | ≥ 0m | Total time window during which failed uploads are retried. Default is 3 hours. |
| `Warehouse.minUploadBackoff` | `60s` | duration | ≥ 0s | Minimum backoff duration between upload retry attempts. |
| `Warehouse.maxUploadBackoff` | `1800s` | duration | ≥ 0s | Maximum backoff duration between upload retry attempts. Default is 30 minutes. Backoff increases exponentially from `minUploadBackoff` to this ceiling. |
| `Warehouse.warehouseSyncPreFetchCount` | `10` | integer | ≥ 1 | Number of warehouse upload jobs prefetched for scheduling. |
| `Warehouse.warehouseSyncFreqIgnore` | `false` | boolean | `true` / `false` | Ignore the configured sync frequency and process uploads as soon as staging files are available. Useful for development and testing. |
| `Warehouse.stagingFilesBatchSize` | `960` | integer | ≥ 1 | Number of staging files processed per upload batch. Controls how many staging files are combined in a single warehouse load operation. |
| `Warehouse.enableIDResolution` | `false` | boolean | `true` / `false` | Enable the identity resolution pipeline in the Warehouse service. When enabled, merge rules from `alias` events are applied to unify user identities. |
| `Warehouse.populateHistoricIdentities` | `false` | boolean | `true` / `false` | Backfill historic identity resolution data from existing warehouse records. Only relevant when `enableIDResolution` is `true`. |
| `Warehouse.enableJitterForSyncs` | `false` | boolean | `true` / `false` | Add random jitter to sync scheduling to prevent thundering herd when multiple warehouse destinations sync simultaneously. |

### Warehouse Per-Connector Settings

Per-connector tuning for parallel load concurrency and connector-specific behavior.

> Source: `config/config.yaml:162-183`

**Maximum Parallel Loads:**

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Warehouse.redshift.maxParallelLoads` | `3` | integer | ≥ 1 | Maximum concurrent table loads for Amazon Redshift. Limited due to Redshift's concurrent query constraints. |
| `Warehouse.snowflake.maxParallelLoads` | `3` | integer | ≥ 1 | Maximum concurrent table loads for Snowflake. |
| `Warehouse.bigquery.maxParallelLoads` | `20` | integer | ≥ 1 | Maximum concurrent table loads for Google BigQuery. Higher default due to BigQuery's native support for concurrent load operations. |
| `Warehouse.postgres.maxParallelLoads` | `3` | integer | ≥ 1 | Maximum concurrent table loads for PostgreSQL. |
| `Warehouse.mssql.maxParallelLoads` | `3` | integer | ≥ 1 | Maximum concurrent table loads for Microsoft SQL Server. |
| `Warehouse.azure_synapse.maxParallelLoads` | `3` | integer | ≥ 1 | Maximum concurrent table loads for Azure Synapse Analytics. |
| `Warehouse.clickhouse.maxParallelLoads` | `3` | integer | ≥ 1 | Maximum concurrent table loads for ClickHouse. |

**PostgreSQL-Specific Settings:**

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Warehouse.postgres.enableSQLStatementExecutionPlan` | `false` | boolean | `true` / `false` | Enable SQL statement execution plan logging for PostgreSQL warehouse loads. Useful for query performance analysis. |

**ClickHouse-Specific Settings:**

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Warehouse.clickhouse.queryDebugLogs` | `false` | boolean | `true` / `false` | Enable debug logging for ClickHouse SQL queries. |
| `Warehouse.clickhouse.blockSize` | `1000` | integer | ≥ 1 | Number of rows per block for ClickHouse bulk insert operations. |
| `Warehouse.clickhouse.poolSize` | `10` | integer | ≥ 1 | Connection pool size for ClickHouse database connections. |
| `Warehouse.clickhouse.disableNullable` | `false` | boolean | `true` / `false` | Disable ClickHouse `Nullable` column types. When `true`, columns use default zero-values instead of `NULL`. Can improve query performance at the cost of null semantics. |
| `Warehouse.clickhouse.enableArraySupport` | `false` | boolean | `true` / `false` | Enable ClickHouse `Array` column type support for array-valued event properties. |

**Delta Lake (Databricks) Settings:**

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Warehouse.deltalake.loadTableStrategy` | `MERGE` | string | `MERGE`, `APPEND` | Load strategy for Databricks Delta Lake tables. `MERGE` performs upsert-style deduplication. `APPEND` adds new rows without deduplication. |

---

## Processor

The Processor executes the six-stage event processing pipeline: preprocess → source hydration → pre-transform → user transform → destination transform → store. These parameters control loop timing, batch sizes, connection pooling, and error handling.

> **Note:** `transformBatchSize` (100) applies to destination transforms executed by the Transformer service. `userTransformBatchSize` (200) applies to user-defined custom transforms. The larger user transform batch size reduces HTTP round trips to the Transformer service for custom code execution.

> Source: `config/config.yaml:184-203`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Processor.webPort` | `8086` | integer | 1–65535 | HTTP listen port for the Processor service. Used for health checks and internal APIs when running in PROCESSOR deployment mode. |
| `Processor.loopSleep` | `10ms` | duration | ≥ 0ms | Base sleep duration between processing loop iterations. The Processor adapts this value based on event availability. |
| `Processor.maxLoopSleep` | `5000ms` | duration | ≥ 0ms | Maximum adaptive sleep duration when no events are available. Default is 5 seconds. |
| `Processor.fixedLoopSleep` | `0ms` | duration | ≥ 0ms | Fixed sleep between processing loops. `0ms` enables adaptive sleep behavior. Set to a non-zero value to override adaptive logic. |
| `Processor.storeTimeout` | `5m` | duration | ≥ 0s | Timeout for store (database write) operations after event processing. Default is 5 minutes. |
| `Processor.maxLoopProcessEvents` | `10000` | integer | ≥ 1 | Maximum number of events processed in a single loop iteration. Controls memory usage and latency characteristics. |
| `Processor.transformBatchSize` | `100` | integer | ≥ 1 | Batch size for destination transform requests sent to the Transformer service (port 9090). |
| `Processor.userTransformBatchSize` | `200` | integer | ≥ 1 | Batch size for user-defined custom transform requests sent to the Transformer service. Larger than `transformBatchSize` to amortize custom code execution overhead. |
| `Processor.maxHTTPConnections` | `100` | integer | ≥ 1 | Maximum total HTTP connections to the Transformer service. |
| `Processor.maxHTTPIdleConnections` | `50` | integer | ≥ 1 | Maximum idle HTTP connections maintained to the Transformer service. Keeps connections warm for faster request dispatch. |
| `Processor.maxRetry` | `30` | integer | ≥ 0 | Maximum retry attempts for failed transformation requests. |
| `Processor.retrySleep` | `100ms` | duration | ≥ 0ms | Sleep duration between transformation retry attempts. |
| `Processor.errReadLoopSleep` | `30s` | duration | ≥ 1s | Sleep duration between error read loop iterations. The error loop processes events that failed transformation. |
| `Processor.errDBReadBatchSize` | `1000` | integer | ≥ 1 | Number of error records read per batch from the error database. |
| `Processor.noOfErrStashWorkers` | `2` | integer | ≥ 1 | Number of workers stashing failed events into the error database for later retry or inspection. |
| `Processor.maxFailedCountForErrJob` | `3` | integer | ≥ 1 | Maximum failure count for error jobs before they are permanently dropped. |
| `Processor.enableEventCount` | `true` | boolean | `true` / `false` | Enable tracking of event counts per source and destination for diagnostic reporting. |
| `Processor.Stats.captureEventName` | `false` | boolean | `true` / `false` | Include event names (e.g., `Product Viewed`, `Order Completed`) in stats tags. Enabling this creates high-cardinality metrics — use with caution in production. |

---

## Deduplication

Event deduplication at the Gateway level to prevent processing duplicate events. Uses an in-memory or external key-value store to track seen message IDs within a configurable time window.

> **Note:** Deduplication is disabled by default. Enable with `Dedup.enableDedup: true`. When enabled, duplicate events (same `messageId`) received within the dedup window are silently dropped.

> Source: `config/config.yaml:204-207`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Dedup.enableDedup` | `false` | boolean | `true` / `false` | Enable event deduplication. When enabled, the Gateway checks each event's `messageId` against a store of recently seen IDs. |
| `Dedup.dedupWindow` | `3600s` | duration | ≥ 1s | Time window for deduplication tracking. Default is 1 hour (3600 seconds). Events with the same `messageId` arriving within this window are dropped. |
| `Dedup.memOptimized` | `true` | boolean | `true` / `false` | Enable memory-optimized deduplication mode. Uses a more compact data structure to reduce memory footprint at the cost of marginally higher CPU usage. |

---

## Backend Configuration

Controls how the server fetches and caches workspace configuration from the RudderStack Control Plane. The Backend Config module polls the control plane at regular intervals and distributes configuration updates to all pipeline components.

> **Note:** Configuration is polled every 5 seconds from the control plane by default. GDPR regulations are polled every 300 seconds (5 minutes). For air-gapped deployments, set `configFromFile: true` and provide a local workspace configuration JSON file.

> Source: `config/config.yaml:208-216`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `BackendConfig.configFromFile` | `false` | boolean | `true` / `false` | Load workspace configuration from a local JSON file instead of polling the control plane API. Required for air-gapped deployments. |
| `BackendConfig.configJSONPath` | `/etc/rudderstack/workspaceConfig.json` | string | valid file path | File path for the local workspace configuration JSON. Only used when `configFromFile` is `true`. |
| `BackendConfig.pollInterval` | `5s` | duration | ≥ 1s | Interval between workspace configuration polls from the control plane. Lower values provide faster config propagation; higher values reduce API load. |
| `BackendConfig.regulationsPollInterval` | `300s` | duration | ≥ 1s | Interval between GDPR regulation polls from the control plane. Default is 5 minutes. |
| `BackendConfig.maxRegulationsPerRequest` | `1000` | integer | ≥ 1 | Maximum number of regulation records fetched per API request. |
| `BackendConfig.Regulations.pageSize` | `50` | integer | ≥ 1 | Page size for paginated regulation API requests. |
| `BackendConfig.Regulations.pollInterval` | `300s` | duration | ≥ 1s | Poll interval for the regulations subsystem. Default is 5 minutes (300 seconds). |

---

## Logger

Logging configuration for the RudderStack server. Supports console output and file-based logging with configurable formats and verbosity.

> **Note:** By default, logging is to console only. Enable file logging with `Logger.enableFile: true`. Log level is controlled via the `LOG_LEVEL` environment variable (default: `INFO`). See [Environment Variable Reference](./env-var-reference.md) for details.

> Source: `config/config.yaml:217-226`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Logger.enableConsole` | `true` | boolean | `true` / `false` | Enable logging to standard output (console). |
| `Logger.enableFile` | `false` | boolean | `true` / `false` | Enable logging to a file. When enabled, logs are written to the path specified by `logFileLocation`. |
| `Logger.consoleJsonFormat` | `false` | boolean | `true` / `false` | Output console logs in JSON format. Enable for structured logging in containerized deployments (e.g., for log aggregation by Fluentd, Logstash). |
| `Logger.fileJsonFormat` | `false` | boolean | `true` / `false` | Output file logs in JSON format. |
| `Logger.logFileLocation` | `/tmp/rudder_log.log` | string | valid file path | File path for log output when file logging is enabled. Ensure the directory exists and is writable. |
| `Logger.logFileSize` | `100` | integer | ≥ 1 (MB) | Maximum log file size in megabytes before rotation. |
| `Logger.enableTimestamp` | `true` | boolean | `true` / `false` | Include timestamps in log entries. |
| `Logger.enableFileNameInLog` | `true` | boolean | `true` / `false` | Include the source file name and line number in log entries. Useful for debugging but adds overhead. |
| `Logger.enableStackTrace` | `false` | boolean | `true` / `false` | Include stack traces in error-level log entries. Enable for debugging; disable in production for performance. |

---

## Diagnostics

Telemetry and diagnostics configuration controlling which pipeline metrics are collected and reported. Diagnostics data is used for monitoring health, performance, and operational visibility.

> Source: `config/config.yaml:227-239`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `Diagnostics.enableDiagnostics` | `true` | boolean | `true` / `false` | Master toggle for the diagnostics subsystem. When `false`, all diagnostic metrics are disabled. |
| `Diagnostics.gatewayTimePeriod` | `60s` | duration | ≥ 1s | Aggregation period for Gateway diagnostic metrics. |
| `Diagnostics.routerTimePeriod` | `60s` | duration | ≥ 1s | Aggregation period for Router diagnostic metrics. |
| `Diagnostics.batchRouterTimePeriod` | `60s` | duration | ≥ 1s | Aggregation period for Batch Router diagnostic metrics. **Note:** The codebase value on line 231 contains a typo (`6l`); the intended value is `60s`. |
| `Diagnostics.enableServerStartMetric` | `true` | boolean | `true` / `false` | Emit a diagnostic metric when the server begins starting. |
| `Diagnostics.enableConfigIdentifyMetric` | `true` | boolean | `true` / `false` | Emit a diagnostic metric when workspace configuration is first identified. |
| `Diagnostics.enableServerStartedMetric` | `true` | boolean | `true` / `false` | Emit a diagnostic metric when the server has fully started and is ready to accept events. |
| `Diagnostics.enableConfigProcessedMetric` | `true` | boolean | `true` / `false` | Emit a diagnostic metric when workspace configuration is processed and applied. |
| `Diagnostics.enableGatewayMetric` | `true` | boolean | `true` / `false` | Enable Gateway throughput and latency metrics. |
| `Diagnostics.enableRouterMetric` | `true` | boolean | `true` / `false` | Enable Router delivery and retry metrics. |
| `Diagnostics.enableBatchRouterMetric` | `true` | boolean | `true` / `false` | Enable Batch Router upload and failure metrics. |
| `Diagnostics.enableDestinationFailuresMetric` | `true` | boolean | `true` / `false` | Enable per-destination failure tracking metrics. |

---

## Runtime Statistics

Go runtime statistics collection for CPU, memory, and garbage collection monitoring.

> Source: `config/config.yaml:240-245`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `RuntimeStats.enabled` | `true` | boolean | `true` / `false` | Enable periodic Go runtime statistics collection. |
| `RuntimeStats.statsCollectionInterval` | `10` | integer | ≥ 1 (seconds) | Interval in seconds between runtime stats collection snapshots. |
| `RuntimeStats.enableCPUStats` | `true` | boolean | `true` / `false` | Collect CPU utilization statistics (goroutine count, CGO calls). |
| `RuntimeStats.enableMemStats` | `true` | boolean | `true` / `false` | Collect memory allocation statistics (heap size, allocations, GC pressure). |
| `RuntimeStats.enableGCStats` | `true` | boolean | `true` / `false` | Collect garbage collection statistics (GC pause times, GC frequency). |

---

## PostgreSQL Notifier

Configuration for the PostgreSQL notification subsystem used by the Warehouse service for coordinating distributed load operations between master and slave nodes.

> Source: `config/config.yaml:246-250`

| Parameter | Default | Type | Range | Description |
|---|---|---|---|---|
| `PgNotifier.retriggerInterval` | `2s` | duration | ≥ 1s | Interval between notification retrigger attempts when a PostgreSQL NOTIFY message is not acknowledged. |
| `PgNotifier.retriggerCount` | `500` | integer | ≥ 1 | Maximum number of retrigger attempts before a notification is abandoned. |
| `PgNotifier.trackBatchInterval` | `2s` | duration | ≥ 1s | Interval between batch tracking polls to check the status of dispatched load operations. |
| `PgNotifier.maxAttempt` | `3` | integer | ≥ 1 | Maximum number of attempts for processing a single notification before marking it as failed. |

---

## See Also

- [Environment Variable Reference](./env-var-reference.md) — Environment variables that override these configuration parameters
- [Glossary](./glossary.md) — Unified terminology reference
- [Capacity Planning](../guides/operations/capacity-planning.md) — Tuning guide for 50,000 events/sec throughput
- [Architecture Overview](../architecture/overview.md) — System architecture and component relationships
- [Warehouse Overview](../warehouse/overview.md) — Warehouse service architecture and connector guides
