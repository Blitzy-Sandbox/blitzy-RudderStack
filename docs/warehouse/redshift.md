# Redshift Connector Guide

RudderStack's Redshift connector loads event data into Amazon Redshift via **S3 manifest-based COPY commands** with support for both IAM role-based and password-based authentication. The connector implements merge and append loading strategies, query group-based resource management, configurable deduplication windows, and automatic schema evolution with DISTSTYLE KEY distribution and SORTKEY optimization.

Data flows through a staging pipeline where load files are first written to Amazon S3, then a manifest JSON document is generated listing all files for a given table, and finally Redshift's `COPY` command ingests the data referencing the manifest. For dedup-eligible tables (users, identifies, discards), the connector uses a staging table with transactional delete-then-insert merge semantics. For event tables, data is appended directly.

**Related Documentation:**

[Warehouse Overview](overview.md) | [Schema Evolution](schema-evolution.md) | [Encoding Formats](encoding-formats.md)

> Source: `warehouse/integrations/redshift/redshift.go`

---

## Prerequisites

Before configuring the Redshift connector, ensure the following requirements are met:

| Requirement | Details |
|-------------|---------|
| **Amazon Redshift Cluster** | A provisioned Redshift cluster (classic) or Redshift Serverless workgroup with network access from the RudderStack server |
| **Amazon S3 Bucket** | An S3 bucket in the same AWS region as the Redshift cluster for staging load files and manifests |
| **IAM Role or Access Keys** | Either an IAM role ARN with S3 read access and Redshift temporary credentials permission, or AWS access key/secret key pair for S3 staging |
| **Redshift User Privileges** | A database user with `CREATE TABLE`, `INSERT`, `SELECT`, `DELETE`, `DROP TABLE`, `ALTER TABLE`, and `CREATE SCHEMA` privileges on the target database and schema |
| **Network Connectivity** | Redshift cluster must be accessible from the RudderStack server on the configured port (default 5439). SSH tunnel configuration is supported for private clusters |
| **SSL** | SSL is required for password-based connections (`sslmode=require` is enforced) |

**Driver Note:** The Redshift connector uses the `lib/pq` PostgreSQL driver for password-based connections and the `sqlconnect-go` library for IAM role-based connections. No additional driver installation is required — both are bundled with the RudderStack server binary.

> Source: `warehouse/integrations/redshift/redshift.go:17` (pq import), `warehouse/integrations/redshift/redshift.go:35-36` (sqlconnect imports)

---

## Setup Steps

### Step 1: Configure Authentication

Choose one of the two supported authentication methods:

#### Option A: IAM Role Authentication (Recommended)

IAM role-based authentication uses AWS STS to obtain temporary credentials for both Redshift access and S3 staging operations. This method eliminates the need to manage long-lived passwords.

Required configuration parameters:

| Parameter | Description |
|-----------|-------------|
| `useIAMForAuth` | Set to `true` to enable IAM authentication |
| `database` | Redshift database name |
| `user` | Redshift database user (for classic clusters) |
| `iamRoleARNForAuth` | IAM role ARN with permissions for `redshift:GetClusterCredentials` (classic) or `redshift-serverless:GetCredentials` (serverless) and S3 access |
| `clusterId` | Redshift cluster identifier (classic clusters only) |
| `clusterRegion` | AWS region where the cluster is deployed |
| `useServerless` | Set to `true` for Redshift Serverless; `false` for classic provisioned clusters |
| `workgroupName` | Serverless workgroup name (serverless only) |

The IAM role is assumed with the workspace ID as the external ID, and temporary credentials have a 1-hour expiry.

> Source: `warehouse/integrations/redshift/redshift.go:1039-1076`

#### Option B: Password Authentication

Password-based authentication connects directly to Redshift using a PostgreSQL-compatible DSN with SSL required.

Required configuration parameters:

| Parameter | Description |
|-----------|-------------|
| `host` | Redshift cluster endpoint hostname |
| `port` | Redshift cluster port (default: `5439`) |
| `database` | Redshift database name |
| `user` | Redshift database user |
| `password` | Redshift database password |

The connector constructs a PostgreSQL DSN in the format:
```
postgres://user:password@host:port/database?sslmode=require
```

SSH tunnel connectivity is supported for clusters in private subnets. When tunnel configuration is provided in the destination config, the connector routes the connection through the specified SSH tunnel.

> Source: `warehouse/integrations/redshift/redshift.go:1078-1122`

### Step 2: Configure S3 Staging

The Redshift connector requires an S3 bucket for staging load files and manifest documents. Configure the following in your destination settings:

- **S3 Bucket Name** — The bucket where staging files and manifests are uploaded
- **S3 Region** — AWS region of the bucket (defaults to `us-east-1` if not specified)
- **S3 Access Credentials** — Temporary STS credentials are obtained automatically via `GetTemporaryS3Cred` for COPY command authorization

The connector uploads manifests to the path: `rudder-redshift-manifests/<sourceID>/<destinationID>/<date>/<tableName>/<uuid>`

> Source: `warehouse/integrations/redshift/redshift.go:424-448`

### Step 3: Set Query Group

Upon connection, the connector automatically sets the Redshift query group to `'RudderStack'`:

```sql
SET query_group to 'RudderStack'
```

This enables Redshift Workload Management (WLM) rules to be applied to all RudderStack queries, allowing you to allocate dedicated memory and concurrency slots to data loading operations.

> Source: `warehouse/integrations/redshift/redshift.go:1008`

---

## Configuration Parameters

The following configuration parameters control the behavior of the Redshift connector. Parameters are set in `config/config.yaml` under the `Warehouse.redshift` namespace or via environment variables.

| Parameter | Config Key | Default | Type | Description |
|-----------|-----------|---------|------|-------------|
| Max Parallel Loads | `Warehouse.redshift.maxParallelLoads` | `3` | `int` | Maximum number of tables loaded concurrently during a warehouse sync. Increase for clusters with higher concurrency capacity. |
| Allow Merge | `Warehouse.redshift.allowMerge` | `true` | `bool` | Enables merge (dedup) loading strategy. When `false`, all tables use append-only loading. |
| Dedup Window | `Warehouse.redshift.dedupWindow` | `false` | `bool` | When `true`, limits dedup DELETE operations to a time window instead of scanning the entire table. Reduces query cost for large tables. |
| Dedup Window Hours | `Warehouse.redshift.dedupWindowInHours` | `720` (30 days) | `time.Duration` (hours) | The lookback window for dedup DELETE operations when `dedupWindow` is enabled. Only rows with `received_at` within this window are considered for deduplication. |
| Slow Query Threshold | `Warehouse.redshift.slowQueryThreshold` | `5m` | `time.Duration` | Queries exceeding this threshold are logged as slow queries for monitoring and debugging. |
| Skip Dedup Destination IDs | `Warehouse.redshift.skipDedupDestinationIDs` | `nil` | `[]string` | List of destination IDs for which deduplication is skipped, forcing append-only behavior. |
| Skip Computing User Latest Traits | `Warehouse.redshift.skipComputingUserLatestTraits` | `false` | `bool` | When `true`, skips the expensive `FIRST_VALUE` window function computation for the users table. Reduces load time but users table may contain stale trait values. |
| Enable Delete By Jobs | `Warehouse.redshift.enableDeleteByJobs` | `false` | `bool` | Enables source-job-based deletion. When `true`, rows from previous job runs are cleaned up based on `context_sources_job_run_id` and `context_sources_task_run_id`. |
| Load By Folder Path | `Warehouse.redshift.loadByFolderPath` | `false` | `bool` | When `true`, uses S3 folder path for COPY instead of manifest-based loading. Useful when manifest generation encounters issues. |

**Global Warehouse Parameters Affecting Redshift:**

| Parameter | Config Key | Default | Type | Description |
|-----------|-----------|---------|------|-------------|
| Upload Frequency | `Warehouse.uploadFreq` | `1800s` (30 min) | `time.Duration` | Interval between warehouse sync cycles |
| Number of Workers | `Warehouse.noOfWorkers` | `8` | `int` | Number of concurrent warehouse upload workers across all destinations |
| Min Retry Attempts | `Warehouse.minRetryAttempts` | `3` | `int` | Minimum number of retry attempts for failed uploads |
| Min Upload Backoff | `Warehouse.minUploadBackoff` | `60s` | `time.Duration` | Minimum backoff duration between upload retries |
| Max Upload Backoff | `Warehouse.maxUploadBackoff` | `1800s` | `time.Duration` | Maximum backoff duration between upload retries |
| Staging Files Batch Size | `Warehouse.stagingFilesBatchSize` | `960` | `int` | Number of staging files processed per upload batch |

> Source: `warehouse/integrations/redshift/redshift.go:184-192` (connector config), `config/config.yaml:145-161` (global warehouse config)

---

## Data Type Mappings

### RudderStack to Redshift Type Mapping

The following table shows how RudderStack data types are mapped to Redshift column types during table creation and schema evolution.

| RudderStack Type | Redshift Type | Notes |
|-----------------|---------------|-------|
| `boolean` | `boolean encode runlength` | Run-length encoding optimized for low-cardinality boolean columns |
| `int` | `bigint` | All integers stored as 64-bit for range safety |
| `bigint` | `bigint` | Direct mapping |
| `float` | `double precision` | 64-bit floating point |
| `string` | `varchar(65535)` | Maximum VARCHAR length; strings > 512 chars are classified as `text` during reverse mapping |
| `text` | `varchar(65535)` | Same storage as `string`; distinguished during schema fetch by character length |
| `datetime` | `timestamp` | Timestamp without time zone; COPY uses `DATEFORMAT 'auto'` and `TIMEFORMAT 'auto'` |
| `json` | `super` | Redshift SUPER type for semi-structured JSON data |

> Source: `warehouse/integrations/redshift/redshift.go:92-101`

### Redshift to RudderStack Reverse Mapping

When fetching the existing warehouse schema, Redshift column types are mapped back to RudderStack types. The `character_maximum_length` is used to distinguish `string` from `text` — columns with length > 512 are classified as `text`.

| Redshift Type | RudderStack Type |
|---------------|-----------------|
| `int`, `int2`, `int4`, `int8`, `bigint` | `int` |
| `float`, `float4`, `float8`, `numeric`, `double precision` | `float` |
| `boolean`, `bool` | `boolean` |
| `text`, `character varying`, `nchar`, `bpchar`, `character`, `nvarchar`, `string` | `string` (or `text` if char length > 512) |
| `date`, `timestamp without time zone`, `timestamp with time zone` | `datetime` |
| `super` | `json` |

> Source: `warehouse/integrations/redshift/redshift.go:103-127`, `warehouse/integrations/redshift/redshift.go:1344-1352` (calculateDataType)

---

## Loading Strategies

The Redshift connector supports two loading strategies: **merge** (for dedup-eligible tables) and **append** (for event tables). The strategy is determined per-table by the `ShouldMerge()` logic, which considers the `allowMerge` config, table type, `preferAppend` destination setting, and per-destination skip lists.

### S3 Manifest-Based Loading

The primary loading mechanism uses S3 manifests — a JSON document listing all staging files for a given table along with metadata. This provides Redshift with an explicit, atomic file list for the COPY command, ensuring exactly the intended files are loaded.

#### Manifest Structure

```json
{
  "entries": [
    {
      "url": "s3://bucket/path/to/load-file-001.csv.gz",
      "mandatory": true,
      "meta": {
        "content_length": 524288
      }
    },
    {
      "url": "s3://bucket/path/to/load-file-002.csv.gz",
      "mandatory": true,
      "meta": {
        "content_length": 131072
      }
    }
  ]
}
```

Each entry contains:
- **`url`** — S3 location of the load file
- **`mandatory`** — Always `true`; COPY fails if any listed file is missing
- **`meta.content_length`** — File size in bytes (when available from load file metadata); enables Redshift to optimize read planning

> Source: `warehouse/integrations/redshift/redshift.go:163-175` (manifest structs)

#### Manifest Generation Flow

The `generateManifest()` function:

1. Retrieves load file metadata for the target table from the uploader
2. Converts load file locations to S3 paths
3. Builds manifest entries with URL, mandatory flag, and content length metadata
4. Serializes the manifest to JSON
5. Writes the manifest to a temporary local file
6. Uploads the manifest to S3 at `rudder-redshift-manifests/<sourceID>/<destinationID>/<date>/<tableName>/<uuid>`
7. Returns the S3 location of the uploaded manifest

> Source: `warehouse/integrations/redshift/redshift.go:364-448`

### COPY Command Execution

The COPY command varies based on the load file format:

**CSV (default):**
```sql
COPY "namespace"."table"("col1","col2",...)
FROM 's3://bucket/path/manifest.json'
CSV GZIP
ACCESS_KEY_ID '***'
SECRET_ACCESS_KEY '***'
SESSION_TOKEN '***'
REGION 'us-east-1'
DATEFORMAT 'auto'
TIMEFORMAT 'auto'
MANIFEST TRUNCATECOLUMNS EMPTYASNULL BLANKSASNULL FILLRECORD
ACCEPTANYDATE TRIMBLANKS ACCEPTINVCHARS
COMPUPDATE OFF
STATUPDATE OFF;
```

**Parquet:**
```sql
COPY "namespace"."table"
FROM 's3://bucket/path/manifest.json'
ACCESS_KEY_ID '***'
SECRET_ACCESS_KEY '***'
SESSION_TOKEN '***'
MANIFEST FORMAT PARQUET;
```

Key COPY options explained:

| Option | Purpose |
|--------|---------|
| `MANIFEST` | Instructs Redshift to interpret the FROM path as a manifest file |
| `CSV GZIP` | Specifies gzip-compressed CSV format |
| `TRUNCATECOLUMNS` | Truncates values exceeding column width instead of failing |
| `EMPTYASNULL` | Treats empty strings as NULL |
| `BLANKSASNULL` | Treats whitespace-only strings as NULL |
| `FILLRECORD` | Fills missing trailing columns with NULL |
| `ACCEPTANYDATE` | Accepts any date format without error |
| `TRIMBLANKS` | Removes trailing whitespace from VARCHAR fields |
| `ACCEPTINVCHARS` | Replaces invalid UTF-8 characters instead of failing |
| `COMPUPDATE OFF` | Disables automatic compression analysis (faster loads) |
| `STATUPDATE OFF` | Disables automatic statistics update (faster loads) |

> Source: `warehouse/integrations/redshift/redshift.go:598-686`

### Merge Strategy (Dedup Tables)

For tables that require deduplication (users, identifies, discards), the connector uses a transactional staging-table approach:

```mermaid
flowchart TD
    A[Load Files in S3] --> B[Generate S3 Manifest]
    B --> C[Upload Manifest to S3]
    C --> D{Table Strategy}
    D -->|Merge| E[Create Staging Table]
    E --> F[COPY INTO Staging via Manifest]
    F --> G[DELETE Matching Rows from Target]
    G --> H[INSERT Deduplicated Rows from Staging]
    H --> I[COMMIT Transaction]
    I --> J[Drop Staging Table]
    D -->|Append| K[COPY INTO Target via Manifest]
```

**Merge workflow details:**

1. **Create staging table** — `CREATE TABLE namespace.staging_table (LIKE namespace.target_table INCLUDING DEFAULTS)`
2. **COPY into staging** — Load all data from S3 manifest into the staging table
3. **BEGIN transaction** — All subsequent operations are transactional
4. **DELETE from target** — Remove rows from the target table where the primary key matches rows in staging:
   ```sql
   DELETE FROM namespace.target
   USING namespace.staging _source
   WHERE _source.id = namespace.target.id
   ```
   For the discards table, the DELETE also matches on `table_name` and `column_name`.
   When `dedupWindow` is enabled, the DELETE is scoped to rows within the configured time window:
   ```sql
   AND namespace.target.received_at > GETDATE() - INTERVAL '720 HOUR'
   ```
5. **INSERT from staging** — Insert deduplicated rows using `ROW_NUMBER()` window function:
   ```sql
   INSERT INTO namespace.target (columns)
   SELECT columns FROM (
     SELECT *, row_number() OVER (
       PARTITION BY partition_key
       ORDER BY received_at DESC
     ) AS _rudder_staging_row_number
     FROM namespace.staging
   ) WHERE _rudder_staging_row_number = 1
   ```
6. **COMMIT** — Atomically applies the delete and insert
7. **Drop staging table** — Cleanup after successful commit

**Primary and Partition Keys:**

| Table | Primary Key | Partition Key |
|-------|------------|---------------|
| `users` | `id` | `id` |
| `identifies` | `id` | `id` |
| `discards` | `row_id` | `row_id, column_name, table_name` |
| All other tables | `id` (default) | `id` (default) |

> Source: `warehouse/integrations/redshift/redshift.go:481-592` (loadTable), `warehouse/integrations/redshift/redshift.go:688-734` (deleteFromLoadTable), `warehouse/integrations/redshift/redshift.go:736-779` (insertIntoLoadTable), `warehouse/integrations/redshift/redshift.go:129-139` (key maps)

### Append Strategy (Event Tables)

For non-dedup tables where `ShouldMerge()` returns `false`, data is loaded directly into the target table via COPY without a staging table:

```sql
COPY "namespace"."target_table"("col1","col2",...)
FROM 's3://manifest-location'
...
MANIFEST ...;
```

This is the most efficient loading path — no staging table creation, no transaction overhead, no deduplication queries.

> Source: `warehouse/integrations/redshift/redshift.go:506-512`

### Users Table Special Handling

The users table receives special treatment to compute the latest trait values across both the existing `users` table and new `identifies` events:

1. Load the `identifies` table first (using merge or append based on config)
2. Create a users staging table with a `UNION` query combining:
   - Existing user rows from `users` table (for user IDs present in new identifies)
   - New rows from the `identifies` staging table
3. Apply `FIRST_VALUE(column IGNORE NULLS) OVER (PARTITION BY id ORDER BY received_at DESC)` to compute the latest non-null value for each trait column
4. Delete matching users from the target table
5. Insert the computed latest-trait rows
6. Commit the transaction

This behavior can be disabled by setting `skipComputingUserLatestTraits: true`, which falls back to standard merge/append logic for the users table.

> Source: `warehouse/integrations/redshift/redshift.go:781-991`

---

## Authentication

### IAM Role Authentication

IAM authentication uses the `sqlconnect-go` library to obtain temporary Redshift credentials via AWS STS role assumption. This method supports both classic provisioned clusters and Redshift Serverless workgroups.

**Classic Cluster Configuration:**
- Requires: `database`, `user`, `clusterId`, `clusterRegion`, `iamRoleARNForAuth`
- The IAM role is assumed with the workspace ID as the external ID
- Temporary credentials are requested with a 1-hour expiry (`RoleARNExpiry: time.Hour`)

**Serverless Configuration:**
- Requires: `database`, `workgroupName`, `clusterRegion`, `iamRoleARNForAuth`
- Set `useServerless: true` in the destination config
- The `user` and `clusterId` parameters are not used for serverless

**Required IAM Permissions:**

For classic clusters:
```json
{
  "Effect": "Allow",
  "Action": [
    "redshift:GetClusterCredentials",
    "redshift:DescribeClusters"
  ],
  "Resource": "arn:aws:redshift:region:account:cluster:cluster-id"
}
```

For serverless:
```json
{
  "Effect": "Allow",
  "Action": [
    "redshift-serverless:GetCredentials",
    "redshift-serverless:GetWorkgroup"
  ],
  "Resource": "arn:aws:redshift-serverless:region:account:workgroup/*"
}
```

Additionally, the IAM role needs S3 access for staging file operations:
```json
{
  "Effect": "Allow",
  "Action": [
    "s3:GetObject",
    "s3:PutObject",
    "s3:ListBucket"
  ],
  "Resource": [
    "arn:aws:s3:::staging-bucket",
    "arn:aws:s3:::staging-bucket/*"
  ]
}
```

> Source: `warehouse/integrations/redshift/redshift.go:1035-1076`

### Password Authentication

Password authentication connects using a standard PostgreSQL DSN with SSL enforced:

- **Protocol:** PostgreSQL wire protocol via `lib/pq` driver
- **SSL Mode:** `require` (hardcoded — cannot be disabled)
- **Connection Timeout:** Configurable; must be ≥ 1 second if set
- **SSH Tunnel:** Supported for clusters in private subnets; tunnel configuration is extracted from the destination config

> Source: `warehouse/integrations/redshift/redshift.go:1078-1122`

### Query Group Resource Management

After connecting (regardless of authentication method), the connector sets:

```sql
SET query_group to 'RudderStack'
```

Configure Redshift WLM (Workload Management) to assign dedicated resources to the `RudderStack` query group:

1. In the Redshift console, navigate to **Workload management**
2. Create or modify a WLM queue with the query group `RudderStack`
3. Allocate memory percentage and concurrency slots appropriate for your loading workload
4. This isolates RudderStack loading queries from analytical workloads

> Source: `warehouse/integrations/redshift/redshift.go:1008-1011`

---

## Table Creation and Distribution

When creating new tables, the connector applies Redshift-specific optimizations:

**Distribution Style:**
- Tables with an `id` column use `DISTSTYLE KEY DISTKEY("id")` for co-located joins
- Tables without an `id` column use Redshift's default distribution (EVEN)

**Sort Key Selection (priority order):**
1. `received_at` — if the column exists in the schema
2. `uuid_ts` — fallback if `received_at` is absent
3. `id` — final fallback

**Table Name Limit:** 127 characters (Redshift identifier limit)

**Example DDL:**
```sql
CREATE TABLE IF NOT EXISTS "namespace"."tracks" (
  "id" varchar(65535),
  "received_at" timestamp,
  "event" varchar(65535),
  ...
) DISTSTYLE KEY DISTKEY("id") SORTKEY("received_at")
```

> Source: `warehouse/integrations/redshift/redshift.go:86-90` (constants), `warehouse/integrations/redshift/redshift.go:215-235` (CreateTable)

---

## Schema Evolution

The Redshift connector supports automatic schema evolution through column additions and type alterations, integrated with the warehouse schema management system.

### Column Addition

New columns are added via `ALTER TABLE ADD COLUMN`. If a column already exists (PostgreSQL error code `42701`), the error is silently ignored to support idempotent schema evolution.

> Source: `warehouse/integrations/redshift/redshift.go:253-295`

### Column Type Alteration

Redshift does not support `ALTER COLUMN TYPE` directly. The connector uses a multi-step transactional approach:

1. **Add staging column** — Create a new column with the target type and a UUID-suffixed staging name
2. **Populate staging column** — `UPDATE ... SET staging_col = CAST(original_col AS new_type)`
3. **Rename original to deprecated** — Rename the original column with a `-deprecated-<uuid>` suffix
4. **Rename staging to original** — Rename the staging column to the original column name
5. **Drop deprecated column** — Attempt to drop the deprecated column outside the transaction
6. If the drop fails due to view dependencies (PostgreSQL error code `2BP01`), return a `CASCADE` drop query for manual execution

For details on the schema evolution system, see [Schema Evolution](schema-evolution.md).

> Source: `warehouse/integrations/redshift/redshift.go:1181-1292`

---

## Error Handling and Troubleshooting

The Redshift connector maps known error patterns to structured error types for automated retry and alerting. The following error mappings are registered:

| Error Type | Error Pattern | Description | Resolution |
|-----------|---------------|-------------|------------|
| `AlterColumnError` | `pq: cannot alter type of a column used by a view or rule` | A column type alteration failed because a view or rule depends on the column | Drop or recreate dependent views before retrying the schema alteration. The connector returns a `CASCADE` drop query in the `AlterTableResponse` |
| `InsufficientResourceError` | `pq: Disk Full` | The Redshift cluster has exhausted disk space | Scale up the cluster by adding nodes, increase disk space, or run `VACUUM DELETE` to reclaim space from deleted rows |
| `PermissionError` | `redshift set query_group error : EOF` | The connection was terminated when setting the query group, typically indicating an authentication or network issue | Verify network connectivity and Redshift user permissions. Check if the cluster is available and accepting connections |
| `ConcurrentQueriesError` | `pq: 1023` | Redshift has reached its concurrent query execution limit | Reduce `maxParallelLoads` or configure WLM queues with higher concurrency slots. Stagger loading windows across destinations |
| `ColumnSizeError` | `pq: Value too long for character type` | A value exceeds the maximum column width | The COPY command uses `TRUNCATECOLUMNS` to prevent this during loading. If encountered during INSERT/UPDATE operations, check source data for oversized values |
| `PermissionError` | `pq: permission denied for database` | The Redshift user lacks database-level permissions | Grant the required permissions: `GRANT ALL ON DATABASE dbname TO username` |
| `PermissionError` | `pq: must be owner of relation` | The Redshift user is not the owner of the table being modified | Transfer table ownership or use a user with owner privileges: `ALTER TABLE tablename OWNER TO username` |
| `ResourceNotFoundError` | `pq: Cannot execute write query because system is in resize mode` | The Redshift cluster is undergoing a resize operation and is temporarily read-only | Wait for the resize operation to complete before retrying. Monitor cluster status in the AWS console |
| `PermissionError` | `pq: SSL is not enabled on the server` | The Redshift cluster does not have SSL enabled, but the connector requires it | Enable SSL on the Redshift cluster. The connector enforces `sslmode=require` for all password-based connections |
| `ResourceNotFoundError` | `Bucket .* not found` | The configured S3 staging bucket does not exist or is inaccessible | Verify the S3 bucket name and region in the destination config. Ensure the IAM role or access keys have `s3:ListBucket` permission |
| `ColumnCountError` | `pq: tables can have at most 1600 columns` | The table has reached Redshift's 1600-column limit | Review your event schema to reduce the number of properties. Consider using the `super` (JSON) type for high-cardinality property sets |

> Source: `warehouse/integrations/redshift/redshift.go:39-84`

### Troubleshooting Checklist

1. **Connection failures** — Verify host, port, and security group rules. For IAM auth, verify the role ARN, cluster ID, and region. For password auth, verify SSL is enabled on the cluster.
2. **Slow loads** — Check the `slowQueryThreshold` metric logs. Consider increasing `maxParallelLoads` if the cluster has capacity. Enable `dedupWindow` to limit the scope of DELETE operations.
3. **Staging table leaks** — The connector automatically cleans up dangling staging tables (prefixed with `rudderstack_staging_`) during the `Cleanup()` phase. If staging tables accumulate, check for interrupted upload cycles.
4. **Schema fetch issues** — The connector queries `INFORMATION_SCHEMA.COLUMNS` excluding staging tables. Verify the Redshift user has `SELECT` access to `INFORMATION_SCHEMA`.
5. **Manifest errors** — If manifest upload fails, check S3 bucket permissions and region configuration. Enable `loadByFolderPath: true` as a fallback to bypass manifest-based loading.

> Source: `warehouse/integrations/redshift/redshift.go:1124-1160` (dangling staging cleanup), `warehouse/integrations/redshift/redshift.go:1294-1342` (FetchSchema)

---

## Idempotency and Backfill

The Redshift connector is designed for **idempotent loading** and supports backfill operations through its manifest-based staging pipeline.

### Merge-Based Idempotency

For dedup-eligible tables (users, identifies, discards), the merge strategy ensures idempotent loading:

- **DELETE + INSERT within a transaction** — If the same data is loaded twice, the first pass inserts rows; the second pass deletes matching rows (by primary key) and re-inserts them, resulting in identical final state
- **ROW_NUMBER() deduplication** — Within each load batch, duplicate rows (by partition key) are deduplicated using `ROW_NUMBER() OVER (PARTITION BY key ORDER BY received_at DESC)`, keeping only the most recent version
- **Dedup window optimization** — When `dedupWindow` is enabled, DELETE operations are scoped to a configurable time window (default 720 hours / 30 days), reducing the scan footprint while maintaining correctness for recent data

### Append-Based Idempotency

For append-only tables, the connector relies on the upstream warehouse upload state machine to prevent duplicate loading:

- Each staging file is tracked by the warehouse upload lifecycle
- A staging file is only loaded once per upload cycle
- If an upload fails mid-way, the entire upload is retried from the beginning with new staging files
- The `context_sources_job_run_id` and `context_sources_task_run_id` fields enable source-job-level deduplication when `enableDeleteByJobs` is enabled

### Backfill Support

Backfill operations are natively supported through the staging pipeline:

1. **Historical data** is ingested through the standard event pipeline and written to staging files
2. **The warehouse sync cycle** picks up staging files and generates load files
3. **Manifest-based COPY** loads all files atomically — the manifest contains the complete set of files for each table
4. **Merge tables** (users, identifies) automatically deduplicate backfilled data against existing rows using the primary key
5. **Append tables** (tracks, pages, screens, groups) accumulate backfilled rows alongside existing data

For warehouse sync configuration and monitoring, see [Warehouse Overview](overview.md).

> Source: `warehouse/integrations/redshift/redshift.go:481-592` (merge idempotency), `warehouse/integrations/redshift/redshift.go:310-352` (DeleteBy for job-level cleanup)

---

## Performance Tuning

### Parallel Load Configuration

The `maxParallelLoads` parameter controls how many tables are loaded concurrently within a single upload cycle. The default of `3` is conservative; for clusters with more WLM concurrency slots, increase this value.

```yaml
Warehouse:
  redshift:
    maxParallelLoads: 5  # Increase for higher-concurrency clusters
```

**Guidance:** Set `maxParallelLoads` to no more than 50% of your WLM queue's concurrency slots to leave capacity for analytical queries.

> Source: `config/config.yaml:162-163`

### Query Group Resource Management

Use Redshift WLM to allocate dedicated resources to RudderStack loading operations:

1. Create a WLM queue with query group `RudderStack`
2. Allocate 20-40% of cluster memory to this queue
3. Set concurrency to at least `maxParallelLoads + 2` (extra slots for schema operations)
4. Enable short query acceleration (SQA) for metadata queries

### S3 Manifest Optimization

The manifest includes `content_length` metadata when available from load file records. This enables Redshift to:
- Pre-allocate memory for the COPY operation
- Optimize read scheduling across cluster nodes
- Provide more accurate progress tracking

Ensure load files include content length metadata for optimal COPY performance.

> Source: `warehouse/integrations/redshift/redshift.go:383-386`

### Dedup Window Tuning

For large tables with millions of rows, enable the dedup window to limit the scope of DELETE operations:

```yaml
Warehouse:
  redshift:
    dedupWindow: true
    dedupWindowInHours: 168  # 7 days - adjust based on your reprocessing window
```

This adds a time-bounded filter to the DELETE statement:
```sql
AND namespace.target.received_at > GETDATE() - INTERVAL '168 HOUR'
```

**Trade-off:** A smaller window reduces query cost but may miss deduplication of very old reprocessed events.

> Source: `warehouse/integrations/redshift/redshift.go:709-718`

### Slow Query Monitoring

Configure the slow query threshold to identify performance bottlenecks:

```yaml
Warehouse:
  redshift:
    slowQueryThreshold: 3m  # Log queries exceeding 3 minutes
```

Slow queries are logged with full query text (credentials redacted) via the SQL middleware layer, enabling identification of expensive COPY, DELETE, or INSERT operations.

> Source: `warehouse/integrations/redshift/redshift.go:190`

### COPY Command Performance Options

The connector configures several COPY options that affect loading performance:

- **`COMPUPDATE OFF`** — Disables automatic compression encoding analysis during COPY. This significantly speeds up loading when the table already has optimal encoding. Compression encoding should be set during table creation or via `ALTER TABLE`.
- **`STATUPDATE OFF`** — Disables automatic statistics update during COPY. Run `ANALYZE` manually after large loads if query performance degrades.

These options are hardcoded in the connector for maximum loading throughput.

> Source: `warehouse/integrations/redshift/redshift.go:669-670`

### Table Distribution and Sort Key Optimization

The connector applies Redshift best practices for physical table layout:

- **DISTSTYLE KEY on `id`** — Co-locates rows with the same `id` on the same node slice, optimizing merge joins during dedup operations
- **SORTKEY on `received_at`** — Enables zone map filtering for time-range queries and dedup window operations
- **Run-length encoding on `boolean`** — Optimizes storage for boolean columns with low cardinality

These settings are applied automatically during table creation and cannot be overridden via configuration.

> Source: `warehouse/integrations/redshift/redshift.go:215-235`

---

## Merge Strategy Decision Logic

The `ShouldMerge()` method determines whether a table uses merge or append loading. The decision follows this logic:

```mermaid
flowchart TD
    A[ShouldMerge called] --> B{allowMerge config}
    B -->|false| C[Return false: Append]
    B -->|true| D{Is users table?}
    D -->|yes| E{skipDedupDestinationIDs<br/>contains this dest?}
    E -->|yes| F[Return false: Append]
    E -->|no| G[Return true: Merge]
    D -->|no| H{Is table in<br/>appendOnlyTables?}
    H -->|yes| I[Return false: Append]
    H -->|no| J{Uploader.CanAppend?}
    J -->|no| K[Return true: Merge]
    J -->|yes| L{preferAppend setting<br/>OR skipDedup list?}
    L -->|either true| M[Return false: Append]
    L -->|both false| N[Return true: Merge]
```

**Key behaviors:**
- `allowMerge: false` globally disables merge for all tables
- Per-destination append-only tables can be configured via `Warehouse.redshift.appendOnlyTables.<destinationID>`
- The `preferAppend` destination setting enables append-only for all non-system tables
- `skipDedupDestinationIDs` provides backward-compatible per-destination dedup bypass

> Source: `warehouse/integrations/redshift/redshift.go:1500-1525`
