# PostgreSQL Connector Guide

RudderStack's PostgreSQL warehouse connector loads event data into PostgreSQL databases using the high-performance `pq.CopyIn` streaming protocol. The connector supports namespace-based schema isolation via PostgreSQL `search_path`, SSH tunnelling for secure network connectivity, merge-aware deletes for deduplication on identity tables, and `verify-ca` SSL mode for encrypted connections with certificate verification.

PostgreSQL is a mature, ACID-compliant relational database widely used as a warehouse target for small-to-medium event volumes. The RudderStack connector leverages PostgreSQL's native `COPY` protocol for bulk ingestion, achieving significantly higher throughput than row-by-row `INSERT` statements.

**Related Documentation:**

[Warehouse Overview](overview.md) | [Schema Evolution](schema-evolution.md) | [Encoding Formats](encoding-formats.md)

> Source: `warehouse/integrations/postgres/postgres.go`

**Provider Constants:**

| Constant | Value | Description |
|----------|-------|-------------|
| `provider` | `warehouseutils.POSTGRES` | Provider identifier used throughout the warehouse pipeline |
| `tableNameLimit` | `127` | Maximum table name length; staging table names are truncated to this limit |

> Source: `warehouse/integrations/postgres/postgres.go:28-35`

---

## Prerequisites

Before configuring the PostgreSQL warehouse connector, ensure the following requirements are met:

- **PostgreSQL Server** — Version 10 or later is recommended. The connector uses features available in PostgreSQL 10+ including `COPY` protocol, `search_path` configuration, `TEMPORARY TABLE` with `ON COMMIT PRESERVE ROWS`, and `ROW_NUMBER()` window functions.
- **Database User Permissions** — The configured database user must have:
  - `CREATE` on the target database (for schema creation via `CREATE SCHEMA IF NOT EXISTS`)
  - `CREATE TABLE` within the target schema
  - `INSERT` on all warehouse-managed tables
  - `SELECT` on `INFORMATION_SCHEMA.COLUMNS` (for schema introspection via `FetchSchema`)
  - `DELETE` on merge-target tables (`users`, `identifies`, `discards`)
  - `USAGE` on the target schema
- **Network Connectivity** — Direct TCP connectivity to the PostgreSQL server on its configured port (default `5432`), or an SSH tunnel host for secure connectivity through firewalled environments.
- **SSL Certificate** (optional) — For `verify-ca` SSL mode, the following certificate files must be accessible:
  - `server-ca.pem` — CA certificate for server verification
  - `client-cert.pem` — Client certificate for mutual TLS
  - `client-key.pem` — Client private key

> Source: `warehouse/integrations/postgres/postgres.go:23-25` (verifyCA const), `warehouse/integrations/postgres/postgres.go:192-235` (connect function)

---

## Connection Configuration

The PostgreSQL connector accepts the following connection settings through the RudderStack destination configuration:

### Basic Connection

| Setting | Config Key | Description |
|---------|-----------|-------------|
| Host | `host` | PostgreSQL server hostname or IP address |
| Port | `port` | PostgreSQL server port (default: `5432`) |
| Database | `database` | Target database name |
| User | `user` | Database username for authentication |
| Password | `password` | Database password for authentication |
| SSL Mode | `sslMode` | SSL connection mode: `disable`, `require`, `verify-ca`, or `verify-full` |

> Source: `warehouse/integrations/postgres/postgres.go:237-253`

The connector constructs a PostgreSQL DSN URL in the format:

```
postgres://user:password@host:port/database?sslmode=<mode>&connect_timeout=<seconds>
```

When `sslMode` is set to `verify-ca`, the connector additionally includes the SSL certificate paths in the connection string:

```
sslrootcert=<sslDir>/server-ca.pem
sslcert=<sslDir>/client-cert.pem
sslkey=<sslDir>/client-key.pem
```

> Source: `warehouse/integrations/postgres/postgres.go:192-214`

### Namespace Configuration

The PostgreSQL connector maps the RudderStack **namespace** concept to a PostgreSQL **schema**. Each warehouse destination is isolated into its own PostgreSQL schema, and all table operations use `SET search_path TO "<namespace>"` to ensure schema-level isolation.

- The namespace is set during `Setup()` from `warehouse.Namespace`
- Schema creation uses `CREATE SCHEMA IF NOT EXISTS "<namespace>"`
- Table creation and queries use fully-qualified names: `"<namespace>"."<table_name>"`
- The `search_path` is set at the beginning of each load transaction to scope all operations

```sql
-- Schema creation
CREATE SCHEMA IF NOT EXISTS "my_namespace";

-- Search path isolation during loads
SET search_path TO "my_namespace";
```

> Source: `warehouse/integrations/postgres/postgres.go:306-335`, `warehouse/integrations/postgres/load.go:76-81`

### SSL/TLS Configuration

The connector supports four SSL modes:

| SSL Mode | Description |
|----------|-------------|
| `disable` | No SSL encryption. Not recommended for production. |
| `require` | SSL encryption required, but server certificate is not verified. |
| `verify-ca` | SSL encryption with server certificate verification against a CA certificate. Requires `server-ca.pem`, `client-cert.pem`, and `client-key.pem`. |
| `verify-full` | SSL encryption with full server hostname verification. Most secure mode. |

When `verify-ca` is selected, the connector writes SSL key files to a destination-specific directory before establishing the connection. The `TestConnection` method also handles SSL key writing for connection validation.

> Source: `warehouse/integrations/postgres/postgres.go:208-211`, `warehouse/integrations/postgres/postgres.go:417-433`

---

## Configuration Parameters

The following configuration parameters control PostgreSQL connector behavior. These are set in `config.yaml` under the `Warehouse.postgres` namespace or via environment variables.

| Parameter | Default | Type | Description |
|-----------|---------|------|-------------|
| `Warehouse.postgres.maxParallelLoads` | `3` | `int` | Maximum number of tables loaded in parallel during a single upload cycle. Controls concurrency to avoid overwhelming the PostgreSQL server. |
| `Warehouse.postgres.enableSQLStatementExecutionPlan` | `false` | `bool` | When enabled, logs SQL execution plans for debugging slow queries. |
| `Warehouse.postgres.allowMerge` | `true` | `bool` | Enables merge (delete-then-insert) behavior for deduplication. When `false`, all loads use append-only mode. |
| `Warehouse.postgres.enableDeleteByJobs` | `false` | `bool` | Enables the `DeleteBy` operation for job-based data cleanup. Used for source-specific data purging by `context_sources_job_run_id`. |
| `Warehouse.postgres.numWorkersDownloadLoadFiles` | `1` | `int` | Number of parallel workers for downloading staging load files from object storage before loading. |
| `Warehouse.postgres.slowQueryThreshold` | `5m` | `duration` | Queries exceeding this duration are logged as slow queries by the SQL middleware layer. |
| `Warehouse.postgres.txnRollbackTimeout` | `30s` | `duration` | Timeout for transaction rollback operations. Prevents indefinite waits during error recovery. |
| `Warehouse.postgres.skipDedupDestinationIDs` | `[]` | `[]string` | List of destination IDs for which deduplication (merge) is skipped, reverting to append-only behavior. |
| `Warehouse.postgres.skipComputingUserLatestTraits` | `false` | `bool` | When `true`, skips the expensive UNION-based latest-traits computation for the `users` table, loading user data via the standard `loadTable` path instead. |
| `Warehouse.postgres.skipComputingUserLatestTraitsWorkspaceIDs` | `[]` | `[]string` | List of workspace IDs for which latest-traits computation is skipped, allowing per-workspace optimization. |

> Source: `warehouse/integrations/postgres/postgres.go:154-171`, `config/config.yaml:168-170`

### PostgreSQL Hard Limits

| Limit | Value | Description |
|-------|-------|-------------|
| Maximum columns per table | `1600` | PostgreSQL enforces a hard limit of 1600 columns per table. The connector surfaces this as a `ColumnCountError` when exceeded. |
| Maximum table name length | `127` characters | Staging table names are truncated to 127 characters to comply with PostgreSQL identifier length limits. |

> Source: `warehouse/integrations/postgres/postgres.go:34`, `warehouse/integrations/postgres/postgres.go:67-69`

---

## Data Type Mappings

The connector maps RudderStack data types to PostgreSQL-native types during schema evolution and table creation.

### RudderStack → PostgreSQL

| RudderStack Type | PostgreSQL Type | Notes |
|------------------|-----------------|-------|
| `int` | `bigint` | 64-bit integer for maximum range |
| `float` | `numeric` | Arbitrary-precision decimal |
| `string` | `text` | Variable-length text with no limit |
| `datetime` | `timestamptz` | Timestamp with time zone |
| `boolean` | `boolean` | Standard boolean |
| `json` | `jsonb` | Binary JSON for efficient querying |

> Source: `warehouse/integrations/postgres/postgres.go:80-87`

### PostgreSQL → RudderStack (Reverse Mapping)

The reverse mapping is used during `FetchSchema` when introspecting existing warehouse tables via `INFORMATION_SCHEMA.COLUMNS`:

| PostgreSQL Type | RudderStack Type |
|-----------------|------------------|
| `integer` | `int` |
| `smallint` | `int` |
| `bigint` | `int` |
| `double precision` | `float` |
| `numeric` | `float` |
| `real` | `float` |
| `text` | `string` |
| `varchar` | `string` |
| `char` | `string` |
| `timestamptz` | `datetime` |
| `timestamp with time zone` | `datetime` |
| `timestamp` | `datetime` |
| `boolean` | `boolean` |
| `jsonb` | `json` |

Any PostgreSQL column type not listed in this reverse mapping is reported as an unrecognized data type via the `RudderMissingDatatype` statistics counter.

> Source: `warehouse/integrations/postgres/postgres.go:89-104`

---

## Loading Strategy — pq.CopyIn Streaming

The PostgreSQL connector uses the `pq.CopyIn` protocol from the `github.com/lib/pq` driver to stream data from staging files into the database. This approach leverages PostgreSQL's native `COPY` command for bulk data ingestion, which is significantly faster than individual `INSERT` statements.

### Load Pipeline Overview

```mermaid
flowchart TD
    A["Staging Files<br/>(Gzipped CSV)"] --> B["Download &<br/>Decompress"]
    B --> C["Create Temporary<br/>Staging Table"]
    C --> D["pq.CopyIn<br/>Streaming"]
    D --> E{"Merge or<br/>Append?"}
    E -->|"Merge"| F["DELETE matching rows<br/>from target table"]
    F --> G["INSERT from staging<br/>with ROW_NUMBER dedup"]
    E -->|"Append"| G
    G --> H["Commit<br/>Transaction"]
```

### Detailed Load Sequence

The `loadTable` function orchestrates the following steps within a single database transaction:

1. **Set Search Path** — Executes `SET search_path TO "<namespace>"` to scope all subsequent operations to the correct PostgreSQL schema.

2. **Download Load Files** — Downloads staging files from object storage using configurable parallel workers (`numWorkersDownloadLoadFiles`). Files are gzip-compressed CSV format.

3. **Create Temporary Staging Table** — Creates a temporary table mirroring the target table's structure:
   ```sql
   CREATE TEMPORARY TABLE <staging_table> (LIKE "<namespace>"."<table>")
   ON COMMIT PRESERVE ROWS;
   ```

4. **Stream Data via CopyIn** — Opens a `pq.CopyIn` prepared statement targeting the staging table and streams rows from each gzipped CSV file:
   - Reads each CSV record sequentially
   - Converts empty/whitespace values to `NULL`
   - Validates column count matches expected schema
   - Executes each row through the CopyIn protocol

5. **Merge-Aware Delete** (conditional) — If the table requires merge behavior (`shouldMerge` returns `true`), deletes existing rows from the target table that have matching primary keys in the staging table:
   ```sql
   DELETE FROM "<namespace>"."<table>" USING "<staging>" AS _source
   WHERE _source.<primary_key> = "<namespace>"."<table>".<primary_key>;
   ```

6. **Insert with Deduplication** — Inserts deduplicated rows from the staging table into the target table using `ROW_NUMBER()` window function, keeping only the most recent record per partition key:
   ```sql
   INSERT INTO "<namespace>"."<table>" (<columns>)
   SELECT <columns>
   FROM (
     SELECT *, ROW_NUMBER() OVER (
       PARTITION BY <partition_key>
       ORDER BY received_at DESC
     ) AS _rudder_staging_row_number
     FROM "<staging>"
   ) AS _
   WHERE _rudder_staging_row_number = 1;
   ```

7. **Commit Transaction** — The entire load operation is wrapped in a single transaction via `DB.WithTx`, ensuring atomicity.

> Source: `warehouse/integrations/postgres/load.go:56-162`

### Primary Key and Partition Key Maps

The connector uses per-table primary and partition keys for deduplication:

| Table | Primary Key | Partition Key |
|-------|-------------|---------------|
| `users` | `id` | `id` |
| `identifies` | `id` | `id` |
| `discards` | `row_id` | `row_id, column_name, table_name` |
| All other tables | `id` (default) | `id` (default) |

For the `discards` table, the delete operation includes an additional join clause on `table_name` and `column_name` to ensure composite-key deduplication.

> Source: `warehouse/integrations/postgres/postgres.go:142-152`, `warehouse/integrations/postgres/load.go:220-261`

### Merge vs. Append Strategy

The connector determines whether to use merge (delete-then-insert) or append-only behavior through the `shouldMerge` method:

```mermaid
flowchart TD
    A["shouldMerge(tableName)"] --> B{"allowMerge<br/>config?"}
    B -->|"false"| C["Return false<br/>(Append Only)"]
    B -->|"true"| D{"Is users<br/>table?"}
    D -->|"yes"| E{"skipDedupDestinationIDs<br/>contains dest?"}
    E -->|"yes"| C
    E -->|"no"| F["Return true<br/>(Merge)"]
    D -->|"no"| G{"Uploader.CanAppend()?"}
    G -->|"no"| F
    G -->|"yes"| H{"preferAppend AND<br/>not in skipDedup?"}
    H -->|"yes"| C
    H -->|"no"| F
```

- **Merge mode** (default): Deletes existing rows by primary key, then inserts new rows. Ensures deduplication for identity tables (`users`, `identifies`, `discards`).
- **Append mode**: Directly inserts rows without deleting. Used when `preferAppend` is enabled on the warehouse destination or when `allowMerge` is disabled.

> Source: `warehouse/integrations/postgres/load.go:579-594`

### Users Table Loading

The `users` and `identifies` tables receive special handling through `LoadUserTables`, which executes both table loads within a single transaction:

1. Load the `identifies` table using the standard `loadTable` flow
2. If `skipComputingUserLatestTraits` is enabled, load the `users` table via the standard flow
3. Otherwise, compute the latest user traits by:
   - Creating a UNION of existing `users` rows and new `identifies` rows (by `user_id`)
   - Building a staging table with the most recent non-null value for each trait column (via correlated subqueries ordered by `received_at DESC`)
   - Deleting existing user records that appear in the staging table
   - Inserting the computed latest-traits rows into the `users` table

This ensures that the `users` table always reflects the most recent trait values across all `identify` calls for each user.

> Source: `warehouse/integrations/postgres/load.go:311-577`

---

## SSH Tunnelling

The PostgreSQL connector supports SSH tunnel connections for reaching database servers behind firewalls or in private networks. SSH tunnelling is provided by the shared `warehouse/integrations/tunnelling` package.

### Configuration

| Setting | Config Key | Description |
|---------|-----------|-------------|
| Enable SSH | `useSSH` | Boolean flag to enable SSH tunnel connectivity |
| SSH Host | `sshHost` | Hostname or IP of the SSH bastion/jump server |
| SSH Port | `sshPort` | SSH server port (typically `22`) |
| SSH User | `sshUser` | SSH username for authentication |
| SSH Private Key | `sshPrivateKey` | PEM-encoded private key for SSH authentication |

> Source: `warehouse/integrations/tunnelling/connect.go:20-24`

### How It Works

When `useSSH` is enabled in the destination configuration:

1. The `ExtractTunnelInfoFromDestinationConfig` function extracts tunnel parameters from the destination config
2. Instead of opening a direct `sql.Open("postgres", dsn)` connection, the connector calls `tunnelling.Connect(dsn, config)`
3. The tunnelling package establishes an SSH tunnel and opens the database connection through the `sql+ssh` driver
4. The database connection is transparently proxied through the SSH tunnel

```mermaid
flowchart LR
    RS["RudderStack<br/>Server"] -->|"SSH Tunnel"| BASTION["SSH Bastion<br/>Host"]
    BASTION -->|"Direct TCP"| PG["PostgreSQL<br/>Server"]

    style RS fill:#4A90D9,color:#fff
    style BASTION fill:#F5A623,color:#fff
    style PG fill:#7ED321,color:#fff
```

> Source: `warehouse/integrations/postgres/postgres.go:220-228`, `warehouse/integrations/tunnelling/connect.go:46-65`

---

## Error Handling and Troubleshooting

The PostgreSQL connector defines structured error mappings that classify database errors into actionable categories. These mappings are used by the warehouse upload state machine to determine retry behavior and error reporting.

### Error Mappings

| Error Category | Error Pattern | Cause | Resolution |
|----------------|---------------|-------|------------|
| `ResourceNotFoundError` | `dial tcp: lookup ... no such host` | DNS resolution failed for the PostgreSQL host | Verify the `host` configuration value is correct and the hostname is resolvable from the RudderStack server |
| `PermissionError` | `dial tcp ... connect: connection refused` | TCP connection to PostgreSQL server was refused | Verify the PostgreSQL server is running and accepting connections on the configured `port`. Check firewall rules and security groups |
| `ResourceNotFoundError` | `pq: database ... does not exist` | The configured database name does not exist on the server | Create the database or correct the `database` configuration value |
| `ResourceNotFoundError` | `pq: the database system is starting up` | PostgreSQL server is still initializing | Wait for the server to complete startup and retry |
| `ResourceNotFoundError` | `pq: the database system is shutting down` | PostgreSQL server is in the process of shutting down | Wait for the server to restart and retry |
| `ResourceNotFoundError` | `pq: relation ... does not exist` | A referenced table does not exist in the target schema | This typically indicates a schema mismatch. The warehouse service will auto-create missing tables on the next sync cycle |
| `ResourceNotFoundError` | `pq: cannot set transaction read-write mode during recovery` | Connection is to a read-only replica or the server is in recovery mode | Connect to the primary/writable server instance |
| `ColumnCountError` | `pq: tables can have at most 1600 columns` | Attempted to add columns beyond PostgreSQL's 1600-column hard limit | Review your event schema to reduce the number of distinct properties. Consider using JSON columns for high-cardinality property sets |
| `PermissionError` | `pq: password authentication failed for user` | Invalid username or password | Verify the `user` and `password` configuration values. Check `pg_hba.conf` authentication rules |
| `PermissionError` | `pq: permission denied` | The database user lacks required permissions | Grant the necessary permissions: `CREATE`, `INSERT`, `SELECT`, `DELETE` on the target schema and tables |

> Source: `warehouse/integrations/postgres/postgres.go:37-78`

### Error Category Behavior

| Category | Retry Behavior | Description |
|----------|---------------|-------------|
| `ResourceNotFoundError` | Retryable with backoff | Resource may become available after infrastructure recovery |
| `PermissionError` | Non-retryable | Requires manual configuration correction |
| `ColumnCountError` | Non-retryable | Requires schema redesign to reduce column count |

### Common Troubleshooting Scenarios

**Connection Timeout:**
- Symptom: Loads fail with context deadline exceeded
- Check: Verify network connectivity, firewall rules, and `connect_timeout` in the connection string
- Resolution: Increase `connectTimeout` or use SSH tunnelling for network-restricted environments

**Slow Loads:**
- Symptom: Upload cycles take longer than expected
- Check: Review slow query logs (threshold controlled by `Warehouse.postgres.slowQueryThreshold`)
- Resolution: Tune `maxParallelLoads`, review PostgreSQL server resources (CPU, memory, I/O), check for table bloat requiring `VACUUM`

**Schema Mismatch:**
- Symptom: `pq: relation ... does not exist` errors
- Check: Verify the namespace (schema) exists in PostgreSQL
- Resolution: The connector auto-creates schemas via `CREATE SCHEMA IF NOT EXISTS`. If the error persists, verify database user permissions include `CREATE` on the database

---

## Idempotency and Backfill

The PostgreSQL connector is designed for idempotent warehouse loading, ensuring that re-executing a load with the same staging files produces the same result without data duplication.

### Delete-Then-Insert Pattern

For merge-enabled tables (`users`, `identifies`, `discards`, and non-append tables), the connector uses a **delete-then-insert** pattern within a single transaction:

1. All rows in the target table with matching primary keys in the staging data are deleted
2. Deduplicated rows from the staging table (keeping the latest by `received_at`) are inserted

This pattern ensures that re-loading the same staging files will:
- Delete the previously inserted rows (matched by primary key)
- Re-insert the same data
- Produce an identical final state

### Transactional Atomicity

All load operations are wrapped in a PostgreSQL transaction (`DB.WithTx`). If any step fails:
- The entire transaction is rolled back
- No partial data is committed to the target table
- The upload can be safely retried

The `safeguard.MustStop` mechanism provides a 5-minute timeout for each table load, preventing indefinite hangs.

> Source: `warehouse/integrations/postgres/load.go:34-54`

### Backfill via Staging File Replay

Backfill operations are supported through the warehouse upload state machine's replay capability:
- Staging files are retained in object storage for the configured retention period
- The warehouse router can re-trigger uploads for historical staging files
- Each replay follows the same delete-then-insert pattern, ensuring idempotent backfill
- The `ROW_NUMBER()` deduplication ensures that even overlapping staging files produce correct results

### DeleteBy for Source-Specific Cleanup

The connector supports job-based data deletion via the `DeleteBy` method, which removes rows matching specific source criteria:

```sql
DELETE FROM "<namespace>"."<table>"
WHERE context_sources_job_run_id <> $1
  AND context_sources_task_run_id <> $2
  AND context_source_id = $3
  AND received_at < $4;
```

This operation is gated by the `Warehouse.postgres.enableDeleteByJobs` configuration flag (default: `false`).

> Source: `warehouse/integrations/postgres/postgres.go:269-304`

---

## Performance Tuning

### Parallel Load Configuration

The `maxParallelLoads` parameter controls how many tables are loaded concurrently during a single upload cycle. For PostgreSQL:

| Scenario | Recommended Value | Rationale |
|----------|-------------------|-----------|
| Small instance (2-4 vCPUs) | `1-2` | Prevents connection and I/O saturation |
| Medium instance (8-16 vCPUs) | `3` (default) | Balanced concurrency for typical workloads |
| Large instance (32+ vCPUs) | `5-8` | Higher parallelism for dedicated warehouse servers |

> Source: `config/config.yaml:169`

### Load File Download Workers

The `numWorkersDownloadLoadFiles` parameter (default: `1`) controls parallel downloads of staging files from object storage. Increasing this value can reduce the download phase duration when many staging files are pending.

### CopyIn Streaming Optimization

The `pq.CopyIn` protocol is the most efficient bulk loading mechanism available in PostgreSQL. Key characteristics:

- **Row-at-a-time streaming**: Each CSV record is streamed to PostgreSQL without buffering the entire file in memory
- **NULL handling**: Empty or whitespace-only values are automatically converted to `NULL`
- **Column validation**: The connector validates that each CSV record has the expected number of columns, failing fast on mismatches
- **Staging table approach**: Data is first loaded into a temporary staging table, then moved to the target table with deduplication — this avoids holding locks on the target table during the bulk load phase

### Connection Pooling

The connector opens a single database connection per load cycle via the `connect()` function. For high-throughput scenarios:

- Ensure the PostgreSQL server's `max_connections` accommodates the expected number of concurrent RudderStack warehouse workers
- Consider using PgBouncer or similar connection poolers between RudderStack and PostgreSQL for environments with many warehouse destinations
- The `connectTimeout` setting prevents indefinite connection waits in congested environments

### Query Monitoring

The SQL middleware layer provides built-in query monitoring:

- Queries exceeding `slowQueryThreshold` (default: `5m`) are logged with full query text
- Transaction rollback operations are bounded by `txnRollbackTimeout` (default: `30s`)
- All queries are instrumented with source, destination, and workspace context for tracing

> Source: `warehouse/integrations/postgres/postgres.go:173-190`

---

## Schema Operations

The connector implements the following schema management operations:

### Schema Creation

```sql
CREATE SCHEMA IF NOT EXISTS "<namespace>";
```

Before creating a schema, the connector checks for existence via `pg_catalog.pg_namespace`:

```sql
SELECT EXISTS (
  SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = '<namespace>'
);
```

> Source: `warehouse/integrations/postgres/postgres.go:306-335`

### Table Creation

Tables are created within the target namespace with columns typed according to the RudderStack → PostgreSQL type mapping:

```sql
SET search_path to "<namespace>";
CREATE TABLE IF NOT EXISTS "<namespace>"."<table>" (<columns>);
```

> Source: `warehouse/integrations/postgres/postgres.go:337-361`

### Column Addition

New columns are added via `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, ensuring idempotency:

```sql
ALTER TABLE <namespace>.<table>
  ADD COLUMN IF NOT EXISTS "<column_name>" <postgres_type>,
  ADD COLUMN IF NOT EXISTS "<column_name>" <postgres_type>;
```

> Source: `warehouse/integrations/postgres/postgres.go:373-411`

### Column Alteration

The PostgreSQL connector does not support column type alteration. The `AlterColumn` method returns an empty response without performing any operation. This prevents potentially destructive type changes in production warehouses.

> Source: `warehouse/integrations/postgres/postgres.go:413-415`

### Schema Introspection

The connector introspects the current warehouse schema by querying `INFORMATION_SCHEMA.COLUMNS`:

```sql
SELECT table_name, column_name, data_type
FROM INFORMATION_SCHEMA.COLUMNS
WHERE table_schema = $1
  AND table_name NOT LIKE '<staging_prefix>%';
```

Results are mapped back to RudderStack types using the PostgreSQL → RudderStack reverse type mapping. Unrecognized column types are tracked via the `RudderMissingDatatype` statistics counter.

> Source: `warehouse/integrations/postgres/postgres.go:447-496`

---

## Identity Resolution

The PostgreSQL connector includes stub implementations for identity resolution operations:

- `LoadIdentityMergeRulesTable` — No-op (identity merge rules not supported for PostgreSQL)
- `LoadIdentityMappingsTable` — No-op (identity mappings not supported for PostgreSQL)
- `DownloadIdentityRules` — No-op (identity rule download not supported for PostgreSQL)

Identity resolution for PostgreSQL warehouse destinations is handled at the application layer through the `users` and `identifies` table loading logic described in the [Loading Strategy](#loading-strategy--pqcopyin-streaming) section, rather than through dedicated identity tables.

For full identity resolution documentation, see the [Warehouse Overview](overview.md) and the identity resolution section of the architecture documentation.

> Source: `warehouse/integrations/postgres/postgres.go:504-514`
