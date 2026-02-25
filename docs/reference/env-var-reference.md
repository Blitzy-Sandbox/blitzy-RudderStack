# Environment Variable Reference

Complete reference for all environment variables used by RudderStack (`rudder-server` v1.68.1). Environment variables override values set in `config/config.yaml` and provide the primary mechanism for configuring RudderStack in containerized and cloud deployments.

> **Source:** `config/sample.env` (103 lines)

> **Note:** Variables marked as **Optional** are commented out in `config/sample.env` and shown as templates. Uncomment and set values only when the corresponding feature is required.

**See also:**

- [Configuration Reference](./config-reference.md) — all 200+ YAML configuration parameters in `config/config.yaml`
- [Glossary](./glossary.md) — unified terminology for RudderStack and Segment concepts

---

## Table of Contents

- [Core Configuration](#core-configuration)
- [Jobs Database Configuration](#jobs-database-configuration)
- [Warehouse Database Configuration](#warehouse-database-configuration)
- [Backend Configuration & Authentication](#backend-configuration--authentication)
- [Service URLs](#service-urls)
- [Object Storage & Bucket Configuration](#object-storage--bucket-configuration)
- [Jobs Backup Storage Configuration](#jobs-backup-storage-configuration)
  - [AWS S3](#aws-s3)
  - [Azure Blob Storage](#azure-blob-storage)
  - [Google Cloud Storage](#google-cloud-storage)
  - [MinIO](#minio)
  - [DigitalOcean Spaces](#digitalocean-spaces)
- [Monitoring & Alerting](#monitoring--alerting)
- [Security & SSL Configuration](#security--ssl-configuration)
- [RSERVER_ Override Convention](#rserver_-override-convention)

---

## Core Configuration

Core runtime parameters controlling the RudderStack server environment, logging, and instance identification.

> Source: `config/sample.env:1,16-19`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `CONFIG_PATH` | `./config/config.yaml` | Yes | Path to the main YAML configuration file. All subsystem parameters are loaded from this file at startup. |
| `GO_ENV` | `production` | No | Runtime environment identifier. Accepted values: `production`, `development`. Affects logging defaults and diagnostic behavior. |
| `LOG_LEVEL` | `INFO` | No | Logging verbosity level. Accepted values: `DEBUG`, `INFO`, `WARN`, `ERROR`. Lower levels produce more verbose output. |
| `INSTANCE_ID` | `1` | No | Unique instance identifier for multi-instance deployments. Used in metrics tagging and log correlation to distinguish between RudderStack server replicas. |

---

## Jobs Database Configuration

PostgreSQL connection parameters for the primary Jobs Database (JobsDB). The JobsDB is the durable event queue that persists all ingested events, processing state, and delivery status.

> Source: `config/sample.env:2-7`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `JOBS_DB_HOST` | `localhost` | Yes | PostgreSQL host address for the primary jobs database. |
| `JOBS_DB_USER` | `rudder` | Yes | PostgreSQL username for database authentication. |
| `JOBS_DB_PASSWORD` | `rudder` | Yes | PostgreSQL password for database authentication. Use a strong, unique password in production environments. |
| `JOBS_DB_PORT` | `5432` | Yes | PostgreSQL port number. |
| `JOBS_DB_DB_NAME` | `jobsdb` | Yes | PostgreSQL database name for the primary jobs database. |
| `JOBS_DB_SSL_MODE` | `disable` | No | PostgreSQL SSL connection mode. Accepted values: `disable`, `require`, `verify-ca`, `verify-full`. Use `verify-full` in production for encrypted and authenticated connections. |

---

## Warehouse Database Configuration

PostgreSQL connection parameters for the Warehouse Jobs Database. The warehouse service uses a separate database for managing warehouse upload jobs, schema state, and staging metadata. In single-node deployments, this may point to the same PostgreSQL instance as the primary JobsDB.

> Source: `config/sample.env:83-90`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `WAREHOUSE_JOBS_DB_HOST` | `localhost` | Yes | PostgreSQL host address for the warehouse jobs database. |
| `WAREHOUSE_JOBS_DB_USER` | `rudder` | Yes | PostgreSQL username for warehouse database authentication. |
| `WAREHOUSE_JOBS_DB_PASSWORD` | `rudder` | Yes | PostgreSQL password for warehouse database authentication. Use a strong, unique password in production environments. |
| `WAREHOUSE_JOBS_DB_SSL_MODE` | `disable` | No | PostgreSQL SSL connection mode. Accepted values: `disable`, `require`, `verify-ca`, `verify-full`. |
| `WAREHOUSE_JOBS_DB_PORT` | `5432` | Yes | PostgreSQL port number for the warehouse database. |
| `WAREHOUSE_JOBS_DB_DB_NAME` | `jobsdb` | Yes | PostgreSQL database name for warehouse jobs. |
| `WAREHOUSE_URL` | `http://localhost:8082` | No | Warehouse service HTTP/gRPC API endpoint URL. The warehouse service exposes its API on this address for upload management, schema queries, and health checks. |

---

## Backend Configuration & Authentication

Parameters for connecting to the RudderStack Control Plane backend, workspace authentication, and optional file-based configuration loading.

> Source: `config/sample.env:12-14,27-29,91`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `CONFIG_BACKEND_URL` | `https://api.rudderstack.com` | Yes | URL for the RudderStack Control Plane configuration backend API. The server polls this endpoint every 5 seconds for workspace configuration updates including source definitions, destination connections, and tracking plans. |
| `CONFIG_BACKEND_TOKEN` | *(none)* | Deprecated | ⚠️ **Deprecated:** Migrate to `WORKSPACE_TOKEN`. This variable will be removed in a future release. |
| `WORKSPACE_TOKEN` | *(none)* | Yes | Workspace authentication token for the Control Plane backend API. Obtain this token from the RudderStack dashboard under **Settings > Workspace Token**. Required for all deployments that fetch configuration from the Control Plane. |
| `RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE` | `false` | No | Enable loading workspace configuration from a local JSON file instead of polling the Control Plane API. Set to `true` for air-gapped or offline deployments. When enabled, set `RSERVER_BACKEND_CONFIG_CONFIG_JSONPATH` to the file path. |
| `RSERVER_BACKEND_CONFIG_CONFIG_JSONPATH` | `/home/user/workspaceConfig.json` | No | Absolute path to the local workspace configuration JSON file. Only used when `RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE` is set to `true`. The file must contain a valid workspace configuration export. |
| `CP_ROUTER_USE_TLS` | `true` | No | Enable TLS for Control Plane router gRPC communication. Set to `false` only in development environments with local Control Plane instances. |

---

## Service URLs

URLs for external services that the RudderStack server depends on at runtime.

> Source: `config/sample.env:9-10`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `DEST_TRANSFORM_URL` | `http://localhost:9090` | Yes | URL for the Transformer service. The Transformer handles JavaScript and Python user transformations (batch size 200) and destination transformations (batch size 100). In Docker Compose deployments, this points to the `rudder-transformer` container. |
| `TEST_SINK_URL` | `http://localhost:8181` | No | URL for the test event sink service. Used in development and testing environments only. Events routed to the test sink destination are delivered to this endpoint for verification. Not required in production. |

---

## Object Storage & Bucket Configuration

Folder name configuration for object storage paths used by warehouse staging, load objects, destination logs, and connection testing. These variables control the directory structure within your configured cloud storage buckets (S3, GCS, Azure Blob, MinIO).

> Source: `config/sample.env:23-25,93`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `WAREHOUSE_STAGING_BUCKET_FOLDER_NAME` | `rudder-warehouse-staging-logs` | No | Folder name within the object storage bucket where warehouse staging files are written. Staging files contain batched event data in JSON/CSV/Parquet format before loading into the warehouse. |
| `WAREHOUSE_BUCKET_LOAD_OBJECTS_FOLDER_NAME` | `rudder-warehouse-load-objects` | No | Folder name for warehouse load object artifacts. These are the finalized files prepared for the warehouse COPY/LOAD operation after encoding and compression. |
| `DESTINATION_BUCKET_FOLDER_NAME` | `rudder-logs` | No | Folder name for destination log artifacts. Used by batch routing and archival to organize event payloads per destination. |
| `RUDDER_CONNECTION_TESTING_BUCKET_FOLDER_NAME` | `rudder-test-payload` | No | Folder name for connection testing payloads. Used by the destination connection testing feature to write and verify test payloads to cloud storage. |

---

## Jobs Backup Storage Configuration

Configure one of the following storage providers for JobsDB table backup exports. JobsDB backups provide disaster recovery and data retention capabilities by exporting completed job tables to object storage. Uncomment the relevant provider section in `config/sample.env` and fill in credentials.

> **Important:** Only one storage provider should be configured at a time. Each provider requires `JOBS_BACKUP_STORAGE_PROVIDER` to be set to the corresponding identifier.

### AWS S3

Amazon S3 configuration for JobsDB table backups.

> Source: `config/sample.env:42-46`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `JOBS_BACKUP_STORAGE_PROVIDER` | `S3` | Yes (if using S3) | Storage provider identifier. Must be set to `S3` for Amazon S3 backups. |
| `JOBS_BACKUP_BUCKET` | *(none)* | Yes | S3 bucket name for storing backup archives. The bucket must exist and the configured IAM credentials must have `s3:PutObject` and `s3:GetObject` permissions. |
| `JOBS_BACKUP_PREFIX` | *(none)* | No | Key prefix for backup objects within the bucket. Use this to organize backups under a specific path (e.g., `backups/rudder/`). |
| `AWS_ACCESS_KEY_ID` | *(none)* | Yes | AWS IAM access key ID. Alternatively, use IAM instance roles or IRSA (IAM Roles for Service Accounts) in EKS environments. |
| `AWS_SECRET_ACCESS_KEY` | *(none)* | Yes | AWS IAM secret access key. Pair with `AWS_ACCESS_KEY_ID` for static credential authentication. |

### Azure Blob Storage

Azure Blob Storage configuration for JobsDB table backups.

> Source: `config/sample.env:50-54`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `JOBS_BACKUP_STORAGE_PROVIDER` | `AZURE_BLOB` | Yes (if using Azure) | Storage provider identifier. Must be set to `AZURE_BLOB` for Azure Blob Storage backups. |
| `JOBS_BACKUP_BUCKET` | *(none)* | Yes | Azure Blob container name for storing backup archives. The container must exist in the specified storage account. |
| `JOBS_BACKUP_PREFIX` | *(none)* | No | Blob prefix for backup objects within the container. |
| `AZURE_STORAGE_ACCOUNT` | *(none)* | Yes | Azure Storage account name. |
| `AZURE_STORAGE_ACCESS_KEY` | *(none)* | Yes | Azure Storage account access key. Obtain from the Azure portal under **Storage Account > Access Keys**. |

### Google Cloud Storage

Google Cloud Storage (GCS) configuration for JobsDB table backups.

> Source: `config/sample.env:58-61`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `JOBS_BACKUP_STORAGE_PROVIDER` | `GCS` | Yes (if using GCS) | Storage provider identifier. Must be set to `GCS` for Google Cloud Storage backups. |
| `JOBS_BACKUP_BUCKET` | *(none)* | Yes | GCS bucket name for storing backup archives. The bucket must exist and the service account must have `storage.objects.create` permission. |
| `JOBS_BACKUP_PREFIX` | *(none)* | No | Object prefix for backup files within the GCS bucket. |
| `GOOGLE_APPLICATION_CREDENTIALS` | *(none)* | Yes | Absolute path to the Google Cloud service account credentials JSON file. The service account must have `Storage Object Creator` role on the target bucket. |

### MinIO

MinIO (S3-compatible) configuration for JobsDB table backups. Used for self-hosted object storage and local development environments.

> Source: `config/sample.env:65-71`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `JOBS_BACKUP_STORAGE_PROVIDER` | `MINIO` | Yes (if using MinIO) | Storage provider identifier. Must be set to `MINIO` for MinIO backups. |
| `JOBS_BACKUP_BUCKET` | *(none)* | Yes | MinIO bucket name for storing backup archives. |
| `JOBS_BACKUP_PREFIX` | *(none)* | No | Object prefix for backup files within the MinIO bucket. |
| `MINIO_ENDPOINT` | `localhost:9000` | Yes | MinIO server endpoint in `host:port` format. In Docker Compose deployments, this typically points to the `minio` container (e.g., `minio:9000`). |
| `MINIO_ACCESS_KEY_ID` | *(none)* | Yes | MinIO access key for authentication. |
| `MINIO_SECRET_ACCESS_KEY` | *(none)* | Yes | MinIO secret key for authentication. |
| `MINIO_SSL` | *(none)* | No | Enable SSL/TLS for MinIO connections. Set to `true` if MinIO is configured with TLS certificates. Accepted values: `true`, `false`. |

### DigitalOcean Spaces

DigitalOcean Spaces (S3-compatible) configuration for JobsDB table backups.

> Source: `config/sample.env:75-80`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `JOBS_BACKUP_STORAGE_PROVIDER` | `DIGITAL_OCEAN_SPACES` | Yes (if using Spaces) | Storage provider identifier. Must be set to `DIGITAL_OCEAN_SPACES` for DigitalOcean Spaces backups. |
| `JOBS_BACKUP_BUCKET` | *(none)* | Yes | DigitalOcean Spaces bucket name for storing backup archives. |
| `JOBS_BACKUP_PREFIX` | *(none)* | No | Object prefix for backup files within the Spaces bucket. |
| `DO_SPACES_ENDPOINT` | *(none)* | Yes | DigitalOcean Spaces endpoint URL. Format: `<region>.digitaloceanspaces.com` (e.g., `nyc3.digitaloceanspaces.com`). |
| `DO_SPACES_ACCESS_KEY_ID` | *(none)* | Yes | DigitalOcean Spaces access key. Generate from the DigitalOcean dashboard under **API > Spaces Keys**. |
| `DO_SPACES_SECRET_ACCESS_KEY` | *(none)* | Yes | DigitalOcean Spaces secret key. Pair with `DO_SPACES_ACCESS_KEY_ID`. |

---

## Monitoring & Alerting

Configuration for metrics export and alerting integrations. RudderStack supports StatsD for metrics collection and PagerDuty or VictorOps for alert routing.

> Source: `config/sample.env:21,33-38`

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `STATSD_SERVER_URL` | *(none)* | No | StatsD server URL for metrics export. Format: `host:port` (e.g., `statsd.monitoring.svc:8125`). When set, RudderStack exports pipeline throughput, latency, error rates, and component health metrics via the StatsD protocol. |
| `ALERT_PROVIDER` | `pagerduty` | No | Alerting provider for operational alerts. Accepted values: `pagerduty`, `victorops`. Determines which alert routing key variable is used. |
| `PG_ROUTING_KEY` | *(none)* | No | PagerDuty integration/routing key for alert delivery. Required when `ALERT_PROVIDER=pagerduty`. Obtain from PagerDuty under **Services > Integrations > Events API v2**. |
| `VICTOROPS_ROUTING_KEY` | *(none)* | No | VictorOps (Splunk On-Call) routing key for alert delivery. Required when `ALERT_PROVIDER=victorops`. |

---

## Security & SSL Configuration

Optional security variables for enabling two-way SSL on Kafka connections and configuring dedicated AWS credentials for S3/Redshift COPY operations.

> Source: `config/sample.env:97-103`

> **Note:** These variables are optional and should only be set when the corresponding feature is required. Ensure that valid certificate and key files exist at the specified paths before enabling Kafka SSL.

| Variable Name | Default Value | Required | Description |
|---|---|---|---|
| `KAFKA_SSL_CERTIFICATE_FILE_PATH` | *(none)* | No | Absolute path to the SSL certificate file (PEM format) for Kafka two-way SSL authentication. Required when the Kafka destination broker mandates mutual TLS (mTLS). |
| `KAFKA_SSL_KEY_FILE_PATH` | *(none)* | No | Absolute path to the SSL private key file (PEM format) for Kafka two-way SSL authentication. Must correspond to the certificate specified in `KAFKA_SSL_CERTIFICATE_FILE_PATH`. |
| `RUDDER_AWS_S3_COPY_USER_ACCESS_KEY_ID` | *(none)* | No | AWS IAM access key ID dedicated to S3 upload/download and Redshift COPY operations. Use this to provide separate credentials for warehouse loading without exposing primary AWS keys in the Control Plane. |
| `RUDDER_AWS_S3_COPY_USER_ACCESS_KEY` | *(none)* | No | AWS IAM secret access key paired with `RUDDER_AWS_S3_COPY_USER_ACCESS_KEY_ID`. Dedicated to S3/Redshift COPY operations. |

---

## RSERVER_ Override Convention

Any YAML configuration parameter defined in `config/config.yaml` can be overridden at runtime via an environment variable using the `RSERVER_` prefix convention. This enables per-deployment tuning without modifying the configuration file.

**Convention rules:**

1. Prefix the parameter path with `RSERVER_`
2. Replace YAML path separators (`.`) with underscores (`_`)
3. Expand camelCase segments to UPPER_SNAKE_CASE

**Examples:**

| YAML Parameter | Environment Variable Override | Default |
|---|---|---|
| `Gateway.webPort` | `RSERVER_GATEWAY_WEB_PORT` | `8080` |
| `Router.noOfWorkers` | `RSERVER_ROUTER_NO_OF_WORKERS` | `64` |
| `Warehouse.mode` | `RSERVER_WAREHOUSE_MODE` | `embedded` |
| `Processor.transformBatchSize` | `RSERVER_PROCESSOR_TRANSFORM_BATCH_SIZE` | `200` |
| `Gateway.maxDBWriterProcess` | `RSERVER_GATEWAY_MAX_DB_WRITER_PROCESS` | `256` |
| `BatchRouter.uploadFreq` | `RSERVER_BATCH_ROUTER_UPLOAD_FREQ` | `30s` |

> **Note:** This convention applies to all 200+ parameters documented in [Configuration Reference](./config-reference.md). The `RSERVER_` prefix variables take precedence over values in `config/config.yaml`, which in turn take precedence over compiled defaults.

**Precedence order (highest to lowest):**

1. `RSERVER_*` environment variables
2. Values in `config/config.yaml`
3. Compiled default values in source code

For the complete catalog of YAML parameters with defaults, types, ranges, and descriptions, see [Configuration Reference](./config-reference.md).

---

## Variable Quick-Reference Index

Alphabetical index of all environment variables documented in this reference for quick lookup.

| Variable Name | Section |
|---|---|
| `ALERT_PROVIDER` | [Monitoring & Alerting](#monitoring--alerting) |
| `AWS_ACCESS_KEY_ID` | [AWS S3](#aws-s3) |
| `AWS_SECRET_ACCESS_KEY` | [AWS S3](#aws-s3) |
| `AZURE_STORAGE_ACCESS_KEY` | [Azure Blob Storage](#azure-blob-storage) |
| `AZURE_STORAGE_ACCOUNT` | [Azure Blob Storage](#azure-blob-storage) |
| `CONFIG_BACKEND_TOKEN` | [Backend Configuration & Authentication](#backend-configuration--authentication) |
| `CONFIG_BACKEND_URL` | [Backend Configuration & Authentication](#backend-configuration--authentication) |
| `CONFIG_PATH` | [Core Configuration](#core-configuration) |
| `CP_ROUTER_USE_TLS` | [Backend Configuration & Authentication](#backend-configuration--authentication) |
| `DEST_TRANSFORM_URL` | [Service URLs](#service-urls) |
| `DESTINATION_BUCKET_FOLDER_NAME` | [Object Storage & Bucket Configuration](#object-storage--bucket-configuration) |
| `DO_SPACES_ACCESS_KEY_ID` | [DigitalOcean Spaces](#digitalocean-spaces) |
| `DO_SPACES_ENDPOINT` | [DigitalOcean Spaces](#digitalocean-spaces) |
| `DO_SPACES_SECRET_ACCESS_KEY` | [DigitalOcean Spaces](#digitalocean-spaces) |
| `GO_ENV` | [Core Configuration](#core-configuration) |
| `GOOGLE_APPLICATION_CREDENTIALS` | [Google Cloud Storage](#google-cloud-storage) |
| `INSTANCE_ID` | [Core Configuration](#core-configuration) |
| `JOBS_BACKUP_BUCKET` | [Jobs Backup Storage Configuration](#jobs-backup-storage-configuration) |
| `JOBS_BACKUP_PREFIX` | [Jobs Backup Storage Configuration](#jobs-backup-storage-configuration) |
| `JOBS_BACKUP_STORAGE_PROVIDER` | [Jobs Backup Storage Configuration](#jobs-backup-storage-configuration) |
| `JOBS_DB_DB_NAME` | [Jobs Database Configuration](#jobs-database-configuration) |
| `JOBS_DB_HOST` | [Jobs Database Configuration](#jobs-database-configuration) |
| `JOBS_DB_PASSWORD` | [Jobs Database Configuration](#jobs-database-configuration) |
| `JOBS_DB_PORT` | [Jobs Database Configuration](#jobs-database-configuration) |
| `JOBS_DB_SSL_MODE` | [Jobs Database Configuration](#jobs-database-configuration) |
| `JOBS_DB_USER` | [Jobs Database Configuration](#jobs-database-configuration) |
| `KAFKA_SSL_CERTIFICATE_FILE_PATH` | [Security & SSL Configuration](#security--ssl-configuration) |
| `KAFKA_SSL_KEY_FILE_PATH` | [Security & SSL Configuration](#security--ssl-configuration) |
| `LOG_LEVEL` | [Core Configuration](#core-configuration) |
| `MINIO_ACCESS_KEY_ID` | [MinIO](#minio) |
| `MINIO_ENDPOINT` | [MinIO](#minio) |
| `MINIO_SECRET_ACCESS_KEY` | [MinIO](#minio) |
| `MINIO_SSL` | [MinIO](#minio) |
| `PG_ROUTING_KEY` | [Monitoring & Alerting](#monitoring--alerting) |
| `RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE` | [Backend Configuration & Authentication](#backend-configuration--authentication) |
| `RSERVER_BACKEND_CONFIG_CONFIG_JSONPATH` | [Backend Configuration & Authentication](#backend-configuration--authentication) |
| `RUDDER_AWS_S3_COPY_USER_ACCESS_KEY` | [Security & SSL Configuration](#security--ssl-configuration) |
| `RUDDER_AWS_S3_COPY_USER_ACCESS_KEY_ID` | [Security & SSL Configuration](#security--ssl-configuration) |
| `RUDDER_CONNECTION_TESTING_BUCKET_FOLDER_NAME` | [Object Storage & Bucket Configuration](#object-storage--bucket-configuration) |
| `STATSD_SERVER_URL` | [Monitoring & Alerting](#monitoring--alerting) |
| `TEST_SINK_URL` | [Service URLs](#service-urls) |
| `VICTOROPS_ROUTING_KEY` | [Monitoring & Alerting](#monitoring--alerting) |
| `WAREHOUSE_BUCKET_LOAD_OBJECTS_FOLDER_NAME` | [Object Storage & Bucket Configuration](#object-storage--bucket-configuration) |
| `WAREHOUSE_JOBS_DB_DB_NAME` | [Warehouse Database Configuration](#warehouse-database-configuration) |
| `WAREHOUSE_JOBS_DB_HOST` | [Warehouse Database Configuration](#warehouse-database-configuration) |
| `WAREHOUSE_JOBS_DB_PASSWORD` | [Warehouse Database Configuration](#warehouse-database-configuration) |
| `WAREHOUSE_JOBS_DB_PORT` | [Warehouse Database Configuration](#warehouse-database-configuration) |
| `WAREHOUSE_JOBS_DB_SSL_MODE` | [Warehouse Database Configuration](#warehouse-database-configuration) |
| `WAREHOUSE_JOBS_DB_USER` | [Warehouse Database Configuration](#warehouse-database-configuration) |
| `WAREHOUSE_STAGING_BUCKET_FOLDER_NAME` | [Object Storage & Bucket Configuration](#object-storage--bucket-configuration) |
| `WAREHOUSE_URL` | [Warehouse Database Configuration](#warehouse-database-configuration) |
| `WORKSPACE_TOKEN` | [Backend Configuration & Authentication](#backend-configuration--authentication) |
