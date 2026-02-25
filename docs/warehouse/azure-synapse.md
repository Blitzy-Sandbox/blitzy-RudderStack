# Azure Synapse Connector Guide

RudderStack's Azure Synapse connector loads event data into Azure Synapse Analytics dedicated SQL pools using **bulk copy ingestion** via the TDS protocol, SQL Server-compatible APIs, **diacritic-safe UCS-2 string handling** for international characters, and **staging/merge routines** that guarantee idempotent, deduplicated loads. The connector leverages the `go-mssqldb` driver's bulk copy interface (`mssql.CopyIn`) to stream rows from gzip-compressed CSV staging files into a temporary staging table, then applies a delete-and-insert strategy with `ROW_NUMBER()` partitioning for deduplication before committing the final data into the target table.

**Related Documentation:**

[Warehouse Overview](overview.md) | [Schema Evolution](schema-evolution.md) | [Encoding Formats](encoding-formats.md)

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go`

---

## Table of Contents

- [Prerequisites](#prerequisites)
- [Setup and Connection](#setup-and-connection)
- [Configuration Parameters](#configuration-parameters)
- [Data Type Mappings](#data-type-mappings)
- [Loading Strategy](#loading-strategy)
- [UCS-2 String Handling](#ucs-2-string-handling)
- [User Table Loading](#user-table-loading)
- [Schema Management](#schema-management)
- [Idempotency and Backfill](#idempotency-and-backfill)
- [Error Handling and Troubleshooting](#error-handling-and-troubleshooting)
- [Performance Tuning](#performance-tuning)

---

## Prerequisites

Before configuring the Azure Synapse connector, ensure the following resources and permissions are in place:

| Requirement | Description |
|-------------|-------------|
| **Azure Synapse Analytics Workspace** | An active Synapse workspace with at least one dedicated SQL pool provisioned. Serverless SQL pools are not supported for bulk copy operations. |
| **Dedicated SQL Pool** | A running dedicated SQL pool (formerly SQL Data Warehouse) with sufficient DWU capacity for your expected data volume. |
| **Azure Blob Storage or ADLS Gen2** | A storage account accessible from the Synapse workspace for staging load files. RudderStack uploads gzip-compressed CSV files to this storage before loading. |
| **Database User with Permissions** | A SQL user with `CREATE TABLE`, `ALTER TABLE`, `INSERT`, `SELECT`, `DELETE`, and `CREATE SCHEMA` permissions on the target database. |
| **Network Connectivity** | The RudderStack server must be able to reach the Synapse SQL endpoint on the configured port (default: `1433`) over TCP. |
| **SSL/TLS Configuration** | The connector connects via the `sqlserver://` protocol with `TrustServerCertificate=true` by default. Configure `sslMode` to control encryption behavior. |

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:145-189` (connect method)

---

## Setup and Connection

### Connection Configuration

The Azure Synapse connector requires the following destination configuration settings, which are retrieved from the RudderStack workspace configuration at runtime:

| Setting | Config Key | Description |
|---------|-----------|-------------|
| **Host** | `host` | The fully qualified SQL endpoint hostname of your Azure Synapse dedicated SQL pool (e.g., `myworkspace.sql.azuresynapse.net`). |
| **Port** | `port` | TCP port for the SQL endpoint. Default for Azure Synapse is `1433`. |
| **Database** | `database` | The name of the dedicated SQL pool database. |
| **User** | `user` | SQL authentication username with the required permissions. |
| **Password** | `password` | SQL authentication password. |
| **SSL Mode** | `sslMode` | Controls encryption for the connection. When `TrustServerCertificate` is `true` (default), all SSL modes (`disable`, `false`, `true`) function correctly. If `forceSSL` is set to `1`, the `disable` option is not available. |

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:191-201` (connectionCredentials), `warehouse/internal/model/settings.go:21-26`

### Connection URL Construction

The connector builds a SQL Server connection URL using the following format:

```
sqlserver://user:password@host:port?database=dbName&encrypt=sslMode&TrustServerCertificate=true&dial+timeout=seconds
```

Key connection behaviors:

- **Protocol:** Uses the `sqlserver://` URL scheme with the `go-mssqldb` driver.
- **TrustServerCertificate:** Always set to `true` for compatibility with Azure-managed certificates.
- **Dial Timeout:** Configured via the `connectTimeout` parameter; included in the connection URL only when greater than zero.
- **Connection Pooling:** Uses Go's `database/sql` connection pool with middleware wrapping for stats collection, query logging, and slow query detection.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:145-189`

### Namespace Configuration

Azure Synapse uses SQL Server schemas as namespaces (unlike PostgreSQL's `search_path`). The connector creates the schema if it does not already exist:

```sql
IF NOT EXISTS (SELECT * FROM sys.schemas WHERE name = N'<namespace>')
EXEC('CREATE SCHEMA [<namespace>]');
```

All tables are created within this namespace using the `<namespace>.<tableName>` convention.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:798-810` (CreateSchema)

### Storage Account Configuration

RudderStack uploads staging files to Azure Blob Storage or ADLS Gen2 before loading into Synapse. The object storage type is determined automatically based on the destination configuration and whether RudderStack-managed storage is being used.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:920` (objectStorage assignment in Setup)

---

## Configuration Parameters

The following configuration parameters control the Azure Synapse connector's behavior. Parameters are set in `config/config.yaml` under the `Warehouse.azure_synapse` namespace or via corresponding environment variables.

| Parameter | Default | Type | Range | Description |
|-----------|---------|------|-------|-------------|
| `Warehouse.azure_synapse.maxParallelLoads` | `3` | int | ≥ 1 | Maximum number of tables loaded concurrently during a single upload cycle. Higher values increase throughput but consume more SQL pool connections. |
| `Warehouse.azure_synapse.columnCountLimit` | `1024` | int | ≥ 1 | Maximum number of columns allowed per table. Tables exceeding this limit will not accept new columns via schema evolution. Azure Synapse supports up to 1024 columns per table. |
| `Warehouse.azure_synapse.numWorkersDownloadLoadFiles` | `1` | int | ≥ 1 | Number of concurrent workers used to download load files from object storage before bulk copy. Increase for higher throughput when network bandwidth permits. |
| `Warehouse.azure_synapse.slowQueryThreshold` | `5m` | duration | ≥ 0s | Queries exceeding this duration are flagged in logs as slow queries. Useful for identifying performance bottlenecks in the bulk copy or merge operations. |
| Table Name Limit (hardcoded) | `127` | int | Fixed | Maximum length for table names, including staging table names. Azure Synapse enforces a 128-character identifier limit; the connector uses 127 to allow for prefix characters. |
| Default Varchar Length (hardcoded) | `512` | int | Fixed | Default maximum length for `varchar` columns when the column length is not explicitly set in the warehouse schema. |

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:39-44` (constants), `warehouse/integrations/azure-synapse/azure-synapse.go:127-138` (New constructor), `warehouse/integrations/config/config.go:25` (columnCountLimit), `config/config.yaml:173-174`

---

## Data Type Mappings

### RudderStack → Azure Synapse

The connector maps RudderStack's internal data types to Azure Synapse SQL types when creating or altering tables:

| RudderStack Type | Azure Synapse Type | Notes |
|------------------|--------------------|-------|
| `int` | `bigint` | 64-bit integer for maximum range compatibility. |
| `float` | `decimal(28,10)` | High-precision decimal with 28 digits total and 10 decimal places. |
| `string` | `varchar(512)` | Default 512-character varchar. Column length can be extended via schema evolution. UCS-2 encoding is applied for strings containing diacritics. |
| `datetime` | `datetimeoffset` | Timezone-aware datetime preserving UTC offset from RFC 3339 formatted input. |
| `boolean` | `bit` | SQL Server bit type (0 or 1). |
| `json` | `jsonb` | Stored as `jsonb` type for structured JSON data. |

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:48-55` (`rudderDataTypesMapToAzureSynapse`)

### Azure Synapse → RudderStack

When fetching the warehouse schema via `INFORMATION_SCHEMA.COLUMNS`, the connector maps Azure Synapse native types back to RudderStack types:

| Azure Synapse Type | RudderStack Type |
|--------------------|------------------|
| `integer`, `smallint`, `bigint`, `tinyint` | `int` |
| `double precision`, `numeric`, `decimal`, `real`, `float` | `float` |
| `text`, `varchar`, `nvarchar`, `ntext`, `nchar`, `char` | `string` |
| `datetimeoffset`, `date`, `datetime2`, `timestamp with time zone`, `timestamp` | `datetime` |
| `jsonb` | `json` |
| `bit` | `boolean` |

The reverse mapping covers all common Azure Synapse/SQL Server column types to ensure accurate schema representation when reading existing warehouse schemas. Any unmapped column types are counted via the `rudder_missing_datatype` stats counter.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:57-80` (`azureSynapseDataTypesMapToRudder`), `warehouse/integrations/azure-synapse/azure-synapse.go:1012-1016` (missing datatype handling)

---

## Loading Strategy

The Azure Synapse connector implements a **staging-table-based bulk copy** strategy that ensures deduplication and transactional consistency. The overall flow involves downloading compressed CSV files from object storage, bulk-inserting them into a temporary staging table, then performing a transactional delete-and-insert against the target table.

### Loading Flow

```mermaid
flowchart TD
    A["Download gzip CSV Files<br/>from Object Storage"] --> B["Create Staging Table<br/>(SELECT TOP 0 * INTO staging FROM target)"]
    B --> C["Bulk Copy into Staging<br/>(mssql.CopyIn via TDS protocol)"]
    C --> D["Begin Transaction"]
    D --> E["DELETE from Target<br/>WHERE primary key matches staging"]
    E --> F["INSERT into Target<br/>SELECT with ROW_NUMBER() dedup"]
    F --> G{"Commit Transaction"}
    G -->|Success| H["Drop Staging Table"]
    G -->|Failure| I["Rollback Transaction"]
    I --> H
```

### Step-by-Step Loading Process

**Step 1: Download Load Files**

The connector downloads gzip-compressed CSV load files from the configured object storage (Azure Blob Storage or ADLS Gen2) using parallel download workers. The number of workers is controlled by the `numWorkersDownloadLoadFiles` configuration parameter (default: `1`).

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:231-237`

**Step 2: Create Staging Table**

A staging table is created as a structural clone of the target table using the `SELECT TOP 0 * INTO` pattern:

```sql
SELECT TOP 0 * INTO <namespace>.<staging_table>
FROM <namespace>.<target_table>;
```

Normal tables are used instead of temporary tables (`#temp`) because SQL Server temporary tables have limited scope with prepared statements and are automatically purged after the transaction commits, which is incompatible with the multi-step bulk copy workflow.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:245-265`

**Step 3: Bulk Copy into Staging Table**

Data is streamed into the staging table using the `go-mssqldb` driver's `CopyIn` interface, which leverages the TDS protocol's bulk copy mechanism for high-throughput data insertion:

1. A `CopyIn` prepared statement is created with `CheckConstraints: false` for all upload columns plus any extra columns present in the warehouse schema.
2. Each gzip CSV file is decompressed and read row by row.
3. Each column value is processed through `ProcessColumnValue()`, which handles type conversion (int, float, datetime, boolean) and UCS-2 encoding for strings with diacritics.
4. Null values (empty strings after trimming) are passed as `nil`.
5. Extra columns (present in warehouse but not in upload) receive `nil` values to maintain column count alignment.
6. Rows are executed sequentially into the bulk copy stream.
7. A final `ExecContext()` call flushes the remaining buffered rows.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:296-326` (CopyIn), `warehouse/integrations/azure-synapse/azure-synapse.go:400-504` (loadDataIntoStagingTable)

**Step 4: Deduplication via Delete-and-Insert**

Within a single transaction, the connector performs deduplication:

1. **DELETE:** Rows in the target table with matching primary keys in the staging table are deleted:

```sql
DELETE FROM <namespace>.<target_table>
FROM <namespace>.<staging_table> AS _source
WHERE _source.<primary_key> = <namespace>.<target_table>.<primary_key>;
```

For the `_discard` table, additional join clauses on `table_name` and `column_name` ensure composite-key deduplication.

2. **INSERT with ROW_NUMBER():** Deduplicated rows from the staging table are inserted into the target, keeping only the latest row per partition key (ordered by `received_at DESC`):

```sql
INSERT INTO <namespace>.<target_table> (<columns>)
SELECT <columns>
FROM (
    SELECT *,
        ROW_NUMBER() OVER (
            PARTITION BY <partition_key>
            ORDER BY received_at DESC
        ) AS _rudder_staging_row_number
    FROM <namespace>.<staging_table>
) AS _
WHERE _rudder_staging_row_number = 1;
```

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:547-637` (deleteFromLoadTable, insertIntoLoadTable)

### Primary and Partition Keys

The connector uses table-specific keys for deduplication:

| Table | Primary Key | Partition Key |
|-------|------------|---------------|
| `users` | `id` | `id` |
| `identifies` | `id` | `id` |
| `_discard` | `row_id` | `row_id, column_name, table_name` |
| All other tables | `id` (default) | `id` (default) |

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:115-125` (primaryKeyMap, partitionKeyMap)

**Step 5: Commit and Cleanup**

After the delete-and-insert transaction commits successfully, the staging table is dropped. If the transaction fails, it is rolled back and the staging table is still cleaned up.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:346-356`

---

## UCS-2 String Handling

Azure Synapse uses SQL Server's string encoding model where `varchar` columns store single-byte characters while `nvarchar` columns store Unicode data in UCS-2 encoding. When RudderStack data contains **diacritics** (characters with multi-byte UTF-8 encoding, such as accented letters like `é`, `ñ`, `ü`, or non-Latin scripts), the connector must convert these strings to UCS-2 byte arrays to prevent data corruption or truncation during bulk copy.

### Diacritic Detection

The `hasDiacritics()` function scans each rune in a string and returns `true` if any character requires more than one byte in UTF-8 encoding (i.e., `utf8.RuneLen(x) > 1`):

```go
func hasDiacritics(str string) bool {
    for _, x := range str {
        if utf8.RuneLen(x) > 1 {
            return true
        }
    }
    return false
}
```

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:650-657`

### UCS-2 Conversion

When diacritics are detected, the `str2ucs2()` function converts the string to a UCS-2 byte array using Go's `unicode/utf16` package:

```go
func str2ucs2(s string) []byte {
    res := utf16.Encode([]rune(s))
    ucs2 := make([]byte, 2*len(res))
    for i := range res {
        ucs2[2*i] = byte(res[i])
        ucs2[2*i+1] = byte(res[i] >> 8)
    }
    return ucs2
}
```

This encoding ensures that the `go-mssqldb` driver transmits the data correctly over the TDS protocol, preserving all international characters without loss.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:639-648`

### String Truncation Rules

String processing applies the following length enforcement:

1. If the varchar column has `CHARACTER_MAXIMUM_LENGTH = -1` (i.e., `varchar(max)`), the string is passed through without truncation.
2. Otherwise, the effective maximum length is `max(varcharLength, 512)` (the `varcharDefaultLength` constant).
3. For non-diacritic strings, the string is truncated to `maxStringLength` characters.
4. For diacritic strings, the UCS-2 byte array is truncated to `maxStringLength` bytes.
5. The varchar column lengths are queried per-table via `INFORMATION_SCHEMA.COLUMNS` before each load to ensure accurate enforcement.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:506-545` (ProcessColumnValue), `warehouse/integrations/azure-synapse/azure-synapse.go:359-398` (getVarcharLengthMap)

---

## User Table Loading

The Azure Synapse connector implements a specialized loading strategy for the `users` and `identifies` tables that merges data from both tables to maintain a consistent user profile view.

### User Table Merge Flow

```mermaid
flowchart TD
    A["Load identifies table<br/>(retain staging table)"] --> B["Create union staging table<br/>(users + identifies)"]
    B --> C["Apply FIRST_VALUE per column<br/>(latest non-null value per user)"]
    C --> D["Begin Transaction"]
    D --> E["DELETE matching users<br/>from target"]
    E --> F["INSERT merged users<br/>into target"]
    F --> G["Commit Transaction"]
    G --> H["Drop all staging tables"]
```

The process:

1. **Load identifies first:** The `identifies` table is loaded using the standard staging/merge flow, but the staging table is retained (not dropped) for the union step.
2. **Union staging:** A union table is created combining rows from the existing `users` table (for users referenced in the new identifies data) and the identifies staging table.
3. **Column aggregation:** For each user `id`, each column gets the most recent non-null value using a correlated subquery ordered by `received_at DESC`.
4. **Transactional merge:** A transaction deletes matching user rows from the target `users` table and inserts the merged rows.
5. **Cleanup:** All staging tables (identifies staging, union staging, users staging) are dropped.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:659-792` (loadUserTables)

---

## Schema Management

### Table Creation

Tables are created with an `IF NOT EXISTS` guard using SQL Server's `sys.objects` catalog:

```sql
IF NOT EXISTS (
    SELECT 1 FROM sys.objects
    WHERE object_id = OBJECT_ID(N'<namespace>.<tableName>')
    AND type = N'U'
)
CREATE TABLE <namespace>.<tableName> (<column definitions>);
```

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:825-835`

### Column Addition

New columns are added via `ALTER TABLE ADD` with an existence check for single-column additions:

```sql
IF NOT EXISTS (
    SELECT 1 FROM SYS.COLUMNS
    WHERE OBJECT_ID = OBJECT_ID(N'<namespace>.<tableName>')
    AND name = '<columnName>'
)
ALTER TABLE <namespace>.<tableName> ADD "<columnName>" <dataType>;
```

For multi-column additions, the existence check is omitted and all columns are added in a single `ALTER TABLE` statement.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:853-898` (AddColumns)

### Column Alteration

The Azure Synapse connector does not support column type alteration (`AlterColumn` returns an empty response). Column types are fixed once created. To change a column type, the column must be deprecated and recreated through the schema evolution process.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:900-902`

### Schema Fetching

The connector retrieves the current warehouse schema from `INFORMATION_SCHEMA.COLUMNS`, filtering out staging tables (prefixed with the provider-specific staging prefix). Column types are mapped back to RudderStack types using the reverse type mapping.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:974-1023` (FetchSchema)

For comprehensive schema evolution documentation, see [Schema Evolution](schema-evolution.md).

---

## Idempotency and Backfill

The Azure Synapse connector is designed for **idempotent loading** and supports **backfill operations** through the following mechanisms:

### Idempotent Loading

Every load operation follows the delete-and-insert pattern within a single transaction:

1. **Delete existing rows** that match the primary key(s) of incoming staging data.
2. **Insert new rows** with `ROW_NUMBER()` deduplication, ensuring only the latest version (by `received_at`) of each row is retained.
3. **Transaction isolation** ensures that the delete and insert are atomic — either both succeed or neither takes effect.

This means running the same load file(s) multiple times produces the same final state in the warehouse, regardless of how many times the operation is retried.

### Backfill Support

Backfill scenarios are supported natively:

- **Re-processing historical data:** The staging/merge approach ensures that historical data replayed through the pipeline is merged correctly with existing data based on primary keys.
- **Schema evolution during backfill:** New columns introduced during backfill are added via `ALTER TABLE ADD`, and existing columns in the warehouse that are not present in the backfill upload receive `nil` values (handled via the `extraColumns` mechanism).
- **Object storage-backed staging:** All load files are stored in Azure Blob Storage before loading, enabling retry of failed loads without re-generating staging data.

### Staging Table Cleanup

Dangling staging tables from interrupted loads are cleaned up during the `Cleanup()` phase. The connector queries `INFORMATION_SCHEMA.TABLES` for tables matching the staging prefix pattern and drops them:

```sql
SELECT table_name FROM information_schema.tables
WHERE table_schema = '<namespace>'
AND table_name LIKE '<staging_prefix>%';
```

Each matched staging table is dropped individually. This prevents accumulation of orphaned staging tables from crash recovery scenarios.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:929-971` (dropDanglingStagingTables), `warehouse/integrations/azure-synapse/azure-synapse.go:1039-1056` (Cleanup)

---

## Error Handling and Troubleshooting

### Common Error Patterns

| Error | Cause | Resolution |
|-------|-------|------------|
| `invalid port "<port>": ...` | Non-numeric port value in destination configuration. | Verify the `port` setting is a valid integer (default: `1433`). |
| `opening connection: ...` | SQL Server driver failed to establish a TCP connection. | Check network connectivity, firewall rules, and ensure the Synapse SQL endpoint is active. |
| `connection timeout: ...` | The `PingContext` call exceeded the deadline. | Increase `connectTimeout`, check Synapse SQL pool status (may be paused), and verify network latency. |
| `creating temporary table: ...` | Failed to create the staging table clone via `SELECT TOP 0 * INTO`. | Ensure the database user has `CREATE TABLE` permissions and the target table exists. |
| `Column count in target table does not match column count specified in input` | Mismatch between columns in the CopyIn statement and the destination table. | This is handled automatically by the connector's `extraColumns` mechanism. If it persists, verify the schema is synchronized. |
| `mismatch in number of columns: actual count: X, expected count: Y` | CSV load file has a different number of columns than expected by the schema. | Indicates a corrupted or stale load file. Re-trigger the upload to regenerate staging files. |
| `preparing copyIn statement: ...` | The bulk copy statement preparation failed. | Check that the table schema matches expectations and all column names are valid identifiers. |
| `exec statement record: ...` | A row-level error during bulk copy (e.g., data type conversion failure). | Inspect the specific row data. Check for datetime format issues (must be RFC 3339) or numeric overflow. |
| `deleting from main table: ...` | The deduplication DELETE failed. | Check for locks on the target table or insufficient permissions. |
| `inserting into main table: ...` | The INSERT from staging failed. | Verify that the staging table was populated correctly and that the column set matches. |
| `commit transaction: ...` | The transaction commit failed. | Possible causes include deadlocks or SQL pool resource exhaustion. Retry the operation. |
| `dropping dangling staging table: ...` | Failed to clean up a leftover staging table. | Manually drop the staging table if it persists, or check user permissions on `DROP TABLE`. |
| `fetching schema: ...` | Failed to query `INFORMATION_SCHEMA.COLUMNS`. | Check database permissions and connectivity. An `io.EOF` error is treated as an empty schema (no tables). |

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go` (error messages throughout the file)

### Diagnostic Logging

The connector emits structured log messages at multiple levels:

- **INFO:** Load start/complete, data insertion, row deletion, staging table operations.
- **DEBUG:** Staging table creation, prepared statement creation, nil value detection with column name and type.
- **WARN:** Data type mismatches during column value processing (logged with column name, type, and error).
- **ERROR:** Staging table drop failures, user table merge failures, transaction errors.

All log messages include contextual fields: `sourceID`, `sourceType`, `destinationID`, `destinationType`, `workspaceID`, `namespace`, and `tableName` where applicable.

### Slow Query Detection

Queries exceeding the `slowQueryThreshold` (default: 5 minutes) are automatically flagged in logs by the SQL query wrapper middleware. This helps identify performance issues with bulk copy, merge, or schema operations.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:135` (slowQueryThreshold config), `warehouse/integrations/azure-synapse/azure-synapse.go:173-187` (middleware setup)

---

## Performance Tuning

### Parallel Loads

The `maxParallelLoads` parameter (default: `3`) controls how many tables are loaded concurrently during a single upload cycle. Increasing this value can significantly reduce total upload time when loading many tables, but each parallel load consumes a separate SQL pool connection and competing DWU resources.

**Recommendation:** Start with the default of `3` and increase to `5-8` if your dedicated SQL pool has sufficient DWU capacity (DW400c or higher) and you are loading more than 10 tables per cycle.

> Source: `config/config.yaml:173-174`

### Download Worker Tuning

The `numWorkersDownloadLoadFiles` parameter (default: `1`) controls concurrent file downloads from object storage. When staging files are large or numerous, increasing this to `2-4` can reduce download time, especially when the object storage and Synapse workspace are in the same Azure region.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:134`

### Bulk Copy Optimization

The connector uses `mssql.CopyIn` with `CheckConstraints: false` to maximize bulk copy throughput. Constraints are not checked during the staging table load because the staging table is a structural clone without constraints.

Key tuning considerations:

- **Batch size:** Rows are streamed row-by-row through the TDS bulk copy protocol. The connector does not implement explicit batch sizing — the `go-mssqldb` driver manages internal buffering.
- **Column count:** Keep the number of columns per table below the `columnCountLimit` (default: `1024`). Tables approaching this limit may experience slower bulk copy performance.
- **String length:** Long string values (> 512 characters) should use `varchar(max)` to avoid truncation and UCS-2 overhead. The varchar length is queried from `INFORMATION_SCHEMA.COLUMNS` before each load.

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:296-302` (CopyIn with BulkOptions)

### Storage Account Proximity

For optimal load performance, ensure that the Azure Blob Storage account used for staging files is in the **same Azure region** as the Synapse workspace. Cross-region data transfer adds latency to both the file upload (from RudderStack) and the load file download (to the connector).

### DWU Capacity Planning

Azure Synapse dedicated SQL pools use DWU (Data Warehouse Units) as a measure of compute capacity. For RudderStack warehouse loads:

| DWU Level | Recommended Max Parallel Loads | Notes |
|-----------|-------------------------------|-------|
| DW100c | 1-2 | Minimal compute; limit concurrency. |
| DW200c-DW400c | 3-5 | Default configuration is appropriate. |
| DW500c-DW1000c | 5-10 | Increase `maxParallelLoads` for faster cycles. |
| DW1500c+ | 10-15 | High concurrency supported; monitor tempdb usage. |

### Connection Timeout

The `connectTimeout` is applied as the `dial timeout` parameter in the SQL Server connection URL. If your Synapse SQL pool is configured with auto-pause, ensure the timeout is long enough to allow the pool to resume (typically 5-10 minutes).

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:157-159` (dial timeout)

---

## Identity Resolution

Azure Synapse is **not** included in the `IdentityEnabledWarehouses` list. The identity resolution operations are no-ops for Azure Synapse:

- `LoadIdentityMergeRulesTable` — No-op (identity merge rules are not loaded to Azure Synapse)
- `LoadIdentityMappingsTable` — No-op (identity mappings are not loaded to Azure Synapse)

Identity resolution for Azure Synapse warehouse destinations is not supported. Cross-touchpoint user unification must be handled at the application layer or via external identity processing pipelines. Only Snowflake and BigQuery support dedicated identity resolution tables.

For full identity resolution documentation, see the [Warehouse Overview](overview.md) and [Identity Resolution](../guides/identity/identity-resolution.md).

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:1058-1068`

---

## Limitations

| Limitation | Description |
|------------|-------------|
| **No Column Alteration** | The connector does not support altering existing column types. Columns must be deprecated and recreated. |
| **No DeleteBy Support** | The `DeleteBy` operation is not implemented for Azure Synapse (returns `NotImplementedErrorCode`). |
| **No Identity Resolution Tables** | `LoadIdentityMergeRulesTable` and `LoadIdentityMappingsTable` are no-ops. Identity resolution is not supported for Azure Synapse. |
| **Table Name Length** | Table names (including staging table names) are limited to 127 characters. |
| **No FIRST_VALUE IGNORE NULLS** | Azure Synapse does not support `FIRST_VALUE ... IGNORE NULLS` (available only in Azure SQL Edge). The connector uses a correlated subquery workaround for user table column aggregation. |

> Source: `warehouse/integrations/azure-synapse/azure-synapse.go:794-796` (DeleteBy), `warehouse/integrations/azure-synapse/azure-synapse.go:1058-1068` (identity stubs), `warehouse/integrations/azure-synapse/azure-synapse.go:43` (tableNameLimit), `warehouse/integrations/azure-synapse/azure-synapse.go:694-696` (FIRST_VALUE comment)
