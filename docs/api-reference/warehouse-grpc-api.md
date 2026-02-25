# Warehouse gRPC API Reference

The **Warehouse gRPC service** exposes 15 unary RPCs for managing warehouse uploads, validating configurations, retrying failed syncs, querying diagnostics, and synchronizing schemas. All RPCs use the **proto3** protocol buffer syntax and communicate over **gRPC (HTTP/2)** on port **8082** by default.

> **Proto Definition:** `proto/warehouse/warehouse.proto`
>
> Source: `proto/warehouse/warehouse.proto:1-8`

| Property | Value |
|----------|-------|
| **Service name** | `proto.Warehouse` |
| **Protocol** | gRPC over HTTP/2 |
| **Default port** | `8082` |
| **Proto syntax** | `proto3` |
| **Go package** | `.;proto` |
| **Protobuf imports** | `google.protobuf.Timestamp`, `google.protobuf.Empty`, `google.protobuf.BoolValue`, `google.protobuf.Struct`, `google.protobuf.DoubleValue` |
| **Generated code** | `proto/warehouse/warehouse_grpc.pb.go` (protoc-gen-go-grpc v1.3.0), `proto/warehouse/warehouse.pb.go` (protoc-gen-go v1.33.0) |

**Related Documentation:**

- [API Overview & Authentication](index.md) — HTTP Gateway authentication and API surface overview
- [Warehouse Overview](../warehouse/overview.md) — Warehouse service architecture and connector guides

---

## Service Overview

The `proto.Warehouse` service is organized into six functional categories. All 15 RPCs are **unary** (single request, single response). No streaming RPCs are defined.

Source: `proto/warehouse/warehouse.proto:10-26`

```mermaid
graph TB
    subgraph "proto.Warehouse Service (Port 8082)"
        direction TB
        subgraph "Health"
            GetHealth["GetHealth"]
        end
        subgraph "Upload Management"
            GetWHUploads["GetWHUploads"]
            GetWHUpload["GetWHUpload"]
            TriggerWHUpload["TriggerWHUpload"]
            TriggerWHUploads["TriggerWHUploads"]
        end
        subgraph "Validation"
            Validate["Validate"]
            ValidateObjStorage["ValidateObjectStorageDestination"]
        end
        subgraph "Retry"
            RetryWHUploads["RetryWHUploads"]
            CountWHUploadsToRetry["CountWHUploadsToRetry"]
            RetryFailedBatches["RetryFailedBatches"]
        end
        subgraph "Diagnostics"
            RetrieveFailedBatches["RetrieveFailedBatches"]
            FirstAborted["GetFirstAbortedUpload..."]
        end
        subgraph "Schema & Telemetry"
            SyncWHSchema["SyncWHSchema"]
            GetSyncLatency["GetSyncLatency"]
            GetDestNS["GetDestinationNamespaces"]
        end
    end
```

### RPC Summary Table

| # | RPC Method | Request Type | Response Type | Category | Description |
|---|-----------|-------------|--------------|----------|-------------|
| 1 | `GetHealth` | `google.protobuf.Empty` | `google.protobuf.BoolValue` | Health | Health check for the warehouse service |
| 2 | `GetWHUploads` | `WHUploadsRequest` | `WHUploadsResponse` | Upload Management | List uploads with filtering and pagination |
| 3 | `GetWHUpload` | `WHUploadRequest` | `WHUploadResponse` | Upload Management | Get a single upload by ID |
| 4 | `TriggerWHUpload` | `WHUploadRequest` | `TriggerWhUploadsResponse` | Upload Trigger | Trigger processing of a single upload |
| 5 | `TriggerWHUploads` | `WHUploadsRequest` | `TriggerWhUploadsResponse` | Upload Trigger | Trigger processing of multiple uploads matching filters |
| 6 | `Validate` | `WHValidationRequest` | `WHValidationResponse` | Validation | Validate warehouse destination configuration |
| 7 | `RetryWHUploads` | `RetryWHUploadsRequest` | `RetryWHUploadsResponse` | Retry | Retry failed or aborted uploads |
| 8 | `CountWHUploadsToRetry` | `RetryWHUploadsRequest` | `RetryWHUploadsResponse` | Retry | Count uploads eligible for retry without executing |
| 9 | `ValidateObjectStorageDestination` | `ValidateObjectStorageRequest` | `ValidateObjectStorageResponse` | Validation | Validate object storage (S3, GCS, Azure Blob) configuration |
| 10 | `RetrieveFailedBatches` | `RetrieveFailedBatchesRequest` | `RetrieveFailedBatchesResponse` | Diagnostics | Retrieve information about failed batch syncs |
| 11 | `RetryFailedBatches` | `RetryFailedBatchesRequest` | `RetryFailedBatchesResponse` | Retry | Retry failed batch syncs with filtering |
| 12 | `GetFirstAbortedUploadInContinuousAbortsByDestination` | `FirstAbortedUploadInContinuousAbortsByDestinationRequest` | `FirstAbortedUploadInContinuousAbortsByDestinationResponse` | Diagnostics | Get the first aborted upload in a continuous abort sequence per destination |
| 13 | `GetSyncLatency` | `SyncLatencyRequest` | `SyncLatencyResponse` | Telemetry | Get sync latency time series data points |
| 14 | `SyncWHSchema` | `SyncWHSchemaRequest` | `google.protobuf.Empty` | Schema | Trigger schema synchronization for a destination |
| 15 | `GetDestinationNamespaces` | `GetDestinationNamespacesRequest` | `GetDestinationNamespacesResponse` | Namespace | Get namespace mappings for a destination |

---

## Message Types

This section documents every protobuf message type used by the Warehouse gRPC service. Field numbers, types, and names match the proto definition exactly.

### Pagination

Pagination metadata returned alongside list responses.

Source: `proto/warehouse/warehouse.proto:28-32`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `total` | `int32` | 1 | `total` | Total number of records matching the query |
| `limit` | `int32` | 2 | `limit` | Maximum records per page |
| `offset` | `int32` | 3 | `offset` | Offset from the start of the result set |

### WHTable

Represents the load status of an individual table within a warehouse upload.

Source: `proto/warehouse/warehouse.proto:34-43`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `id` | `int64` | 1 | `id` | Table record ID |
| `upload_id` | `int64` | 2 | `uploadId` | Associated upload ID |
| `name` | `string` | 3 | `name` | Table name in the warehouse |
| `status` | `string` | 4 | `status` | Table load status (e.g., `waiting`, `executing`, `exported_data`, `aborted`) |
| `error` | `string` | 5 | `error` | Error message if the table load failed |
| `last_exec_at` | `google.protobuf.Timestamp` | 6 | `lastExecAt` | Timestamp of the last execution attempt |
| `count` | `int32` | 7 | `count` | Number of records loaded into this table |
| `duration` | `int32` | 8 | `duration` | Execution duration in seconds |

### WHUploadsRequest

Request message for listing uploads with optional filtering and pagination.

Source: `proto/warehouse/warehouse.proto:45-53`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `source_id` | `string` | 1 | `sourceId` | Filter by source ID |
| `destination_id` | `string` | 2 | `destinationId` | Filter by destination ID |
| `destination_type` | `string` | 3 | `destinationType` | Filter by destination type (e.g., `SNOWFLAKE`, `BQ`, `RS`, `CLICKHOUSE`) |
| `status` | `string` | 4 | `status` | Filter by upload status |
| `limit` | `int32` | 5 | `limit` | Maximum number of results to return |
| `offset` | `int32` | 6 | `offset` | Pagination offset |
| `workspace_id` | `string` | 7 | `workspaceId` | Filter by workspace ID |

### WHUploadsResponse

Response containing a paginated list of uploads.

Source: `proto/warehouse/warehouse.proto:55-58`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `uploads` | `repeated WHUploadResponse` | 1 | `uploads` | List of upload records |
| `pagination` | `Pagination` | 2 | `pagination` | Pagination metadata (total, limit, offset) |

### WHUploadRequest

Request message for retrieving or triggering a single upload by ID.

Source: `proto/warehouse/warehouse.proto:60-63`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `upload_id` | `int64` | 1 | `uploadId` | Upload ID to retrieve or trigger |
| `workspace_id` | `string` | 2 | `workspaceId` | Workspace ID for authorization |

### WHUploadResponse

Comprehensive response representing a single warehouse upload record with per-table status.

Source: `proto/warehouse/warehouse.proto:65-82`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `id` | `int64` | 1 | `id` | Upload record ID |
| `source_id` | `string` | 2 | `sourceId` | Source identifier |
| `destination_id` | `string` | 3 | `destinationId` | Destination identifier |
| `destination_type` | `string` | 4 | `destinationType` | Destination type (e.g., `SNOWFLAKE`, `BQ`, `RS`, `CLICKHOUSE`, `DELTALAKE`, `POSTGRES`, `MSSQL`, `AZURE_SYNAPSE`, `S3_DATALAKE`) |
| `namespace` | `string` | 5 | `namespace` | Database namespace or schema name |
| `error` | `string` | 6 | `error` | Error message if the upload failed |
| `attempt` | `int32` | 7 | `attempt` | Current retry attempt number (starts at 0) |
| `status` | `string` | 8 | `status` | Upload status (see [Upload Status Values](#upload-status-values) below) |
| `created_at` | `google.protobuf.Timestamp` | 9 | `createdAt` | Upload creation timestamp |
| `first_event_at` | `google.protobuf.Timestamp` | 10 | `firstEventAt` | Timestamp of the first event in the batch |
| `last_event_at` | `google.protobuf.Timestamp` | 11 | `lastEventAt` | Timestamp of the last event in the batch |
| `last_exec_at` | `google.protobuf.Timestamp` | 12 | `lastExecAt` | Timestamp of the last execution attempt |
| `next_retry_time` | `google.protobuf.Timestamp` | 13 | `nextRetryTime` | Scheduled time for the next retry |
| `duration` | `int32` | 14 | `duration` | Execution duration in seconds |
| `tables` | `repeated WHTable` | 15 | `tables` | Per-table load status breakdown |
| `isArchivedUpload` | `bool` | 16 | `isArchivedUpload` | Whether this upload has been archived |

#### Upload Status Values

| Status | Description |
|--------|-------------|
| `waiting` | Upload is queued and waiting for processing |
| `executing` | Upload is currently being processed |
| `exported_data` | Data has been exported to staging files |
| `aborted` | Upload was aborted after exceeding retry limits |
| `failed` | Upload failed during processing |
| `succeeded` | Upload completed successfully |

### TriggerWhUploadsResponse

Response returned when triggering one or more uploads.

Source: `proto/warehouse/warehouse.proto:84-87`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `message` | `string` | 1 | `message` | Human-readable response message |
| `status_code` | `int32` | 2 | `statusCode` | Status code (200 for success) |

### WHValidationRequest

Request message for validating a warehouse destination configuration.

Source: `proto/warehouse/warehouse.proto:89-94`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `role` | `string` | 1 | `role` | Validation role identifier |
| `path` | `string` | 2 | `path` | Validation path (endpoint or resource path) |
| `step` | `string` | 3 | `step` | Validation step name (e.g., connection test, permission check) |
| `body` | `string` | 4 | `body` | Request body as a JSON string containing destination configuration |

### WHValidationResponse

Response from a warehouse configuration validation request.

Source: `proto/warehouse/warehouse.proto:96-99`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `error` | `string` | 1 | `error` | Error message if validation failed (empty on success) |
| `data` | `string` | 2 | `data` | Validation result data as a JSON string |

### RetryWHUploadsRequest

Request message for retrying or counting retryable warehouse uploads.

Source: `proto/warehouse/warehouse.proto:101-109`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `workspaceId` | `string` | 1 | `workspaceId` | Workspace identifier |
| `sourceId` | `string` | 2 | `sourceId` | Filter by source identifier |
| `destinationId` | `string` | 3 | `destinationId` | Filter by destination identifier |
| `destinationType` | `string` | 4 | `destinationType` | Filter by destination type |
| `intervalInHours` | `int64` | 5 | `intervalInHours` | Retry interval — only retry uploads failed within this many hours |
| `uploadIds` | `repeated int64` | 6 | `uploadIds` | Specific upload IDs to retry (overrides filter fields) |
| `forceRetry` | `bool` | 7 | `forceRetry` | Force retry even for aborted uploads that exceeded retry limits |

### RetryWHUploadsResponse

Response from a retry or count-to-retry operation.

Source: `proto/warehouse/warehouse.proto:111-115`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `message` | `string` | 1 | `message` | Human-readable response message |
| `count` | `int64` | 2 | `count` | Number of uploads retried or eligible for retry |
| `status_code` | `int32` | 3 | `statusCode` | Status code (200 for success) |

### ValidateObjectStorageRequest

Request message for validating an object storage destination configuration (S3, GCS, Azure Blob).

Source: `proto/warehouse/warehouse.proto:117-120`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `type` | `string` | 1 | `type` | Object storage type (e.g., `S3`, `GCS`, `AZURE_BLOB`) |
| `config` | `google.protobuf.Struct` | 2 | `config` | Configuration parameters as a JSON-compatible Struct (bucket, credentials, region, etc.) |

### ValidateObjectStorageResponse

Response from an object storage validation request.

Source: `proto/warehouse/warehouse.proto:122-125`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `isValid` | `bool` | 2 | `isValid` | Whether the object storage configuration is valid |
| `error` | `string` | 3 | `error` | Error message if validation failed (empty on success) |

> **Note:** Field numbers start at 2 — field number 1 is reserved in this message.

### FailedBatchInfo

Represents diagnostic information about a failed batch sync.

Source: `proto/warehouse/warehouse.proto:127-136`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `error` | `string` | 1 | `error` | Error message describing the failure |
| `errorCategory` | `string` | 2 | `errorCategory` | Error category classification (e.g., `permission_error`, `config_error`, `resource_not_found`) |
| `sourceID` | `string` | 3 | `sourceID` | Source identifier associated with the failed batch |
| `failedEventsCount` | `int64` | 4 | `failedEventsCount` | Total number of events that failed |
| `failedSyncsCount` | `int64` | 5 | `failedSyncsCount` | Total number of sync operations that failed |
| `lastHappened` | `google.protobuf.Timestamp` | 6 | `lastHappened` | Timestamp of the most recent occurrence |
| `firstHappened` | `google.protobuf.Timestamp` | 7 | `firstHappened` | Timestamp of the first occurrence |
| `status` | `string` | 8 | `status` | Current batch status |

### RetrieveFailedBatchesRequest

Request message for retrieving failed batch information within a time range.

Source: `proto/warehouse/warehouse.proto:138-143`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `workspaceID` | `string` | 1 | `workspaceID` | Workspace identifier |
| `destinationID` | `string` | 2 | `destinationID` | Destination identifier |
| `start` | `string` | 3 | `start` | Start time for the query range (ISO 8601 format) |
| `end` | `string` | 4 | `end` | End time for the query range (ISO 8601 format) |

### RetrieveFailedBatchesResponse

Response containing a list of failed batch records.

Source: `proto/warehouse/warehouse.proto:145-147`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `failedBatches` | `repeated FailedBatchInfo` | 1 | `failedBatches` | List of failed batch information records |

### RetryFailedBatchesRequest

Request message for retrying failed batch syncs with optional filtering.

Source: `proto/warehouse/warehouse.proto:149-157`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `workspaceID` | `string` | 1 | `workspaceID` | Workspace identifier |
| `destinationID` | `string` | 2 | `destinationID` | Destination identifier |
| `start` | `string` | 3 | `start` | Start time range (ISO 8601 format) |
| `end` | `string` | 4 | `end` | End time range (ISO 8601 format) |
| `errorCategory` | `string` | 5 | `errorCategory` | Filter retries by error category |
| `sourceID` | `string` | 6 | `sourceID` | Filter retries by source ID |
| `status` | `string` | 7 | `status` | Filter retries by status |

### RetryFailedBatchesResponse

Response from a failed batch retry operation.

Source: `proto/warehouse/warehouse.proto:159-161`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `retriedSyncsCount` | `int64` | 1 | `retriedSyncsCount` | Number of sync operations that were retried |

### FirstAbortedUploadInContinuousAbortsByDestinationRequest

Request message for identifying the first aborted upload in a continuous sequence of aborts per destination.

Source: `proto/warehouse/warehouse.proto:163-166`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `workspace_id` | `string` | 1 | `workspaceId` | Workspace identifier |
| `start` | `string` | 2 | `start` | Start time range for the query (ISO 8601 format) |

### SyncWHSchemaRequest

Request message for triggering a schema synchronization for a specific destination.

Source: `proto/warehouse/warehouse.proto:168-170`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `destination_id` | `string` | 1 | `destinationId` | Destination ID to synchronize schema for |

### FirstAbortedUploadResponse

Represents a single aborted upload record returned by the continuous-abort diagnostic RPC.

Source: `proto/warehouse/warehouse.proto:172-179`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `id` | `int64` | 1 | `id` | Upload ID |
| `source_id` | `string` | 2 | `sourceId` | Source identifier |
| `destination_id` | `string` | 3 | `destinationId` | Destination identifier |
| `created_at` | `google.protobuf.Timestamp` | 4 | `createdAt` | Upload creation timestamp |
| `first_event_at` | `google.protobuf.Timestamp` | 5 | `firstEventAt` | Timestamp of the first event in the batch |
| `last_event_at` | `google.protobuf.Timestamp` | 6 | `lastEventAt` | Timestamp of the last event in the batch |

### FirstAbortedUploadInContinuousAbortsByDestinationResponse

Response containing the first aborted upload per destination in a continuous abort sequence.

Source: `proto/warehouse/warehouse.proto:181-183`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `uploads` | `repeated FirstAbortedUploadResponse` | 1 | `uploads` | List of first aborted uploads, one per destination experiencing continuous aborts |

### SyncLatencyRequest

Request message for querying warehouse sync latency time series data.

Source: `proto/warehouse/warehouse.proto:185-191`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `destination_id` | `string` | 1 | `destinationId` | Destination identifier |
| `workspace_id` | `string` | 2 | `workspaceId` | Workspace identifier |
| `start_time` | `string` | 3 | `startTime` | Start time for the latency query (ISO 8601 format) |
| `aggregation_minutes` | `string` | 4 | `aggregationMinutes` | Aggregation window in minutes (e.g., `"15"`, `"60"`) |
| `source_id` | `string` | 5 | `sourceId` | Source identifier |

### SyncLatencyResponse

Response containing time series latency data points for warehouse sync operations.

Source: `proto/warehouse/warehouse.proto:193-195`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `time_series_data_points` | `repeated LatencyTimeSeriesDataPoint` | 1 | `graph` | Latency time series data points (JSON serialized as `"graph"`) |

### LatencyTimeSeriesDataPoint

A single data point in the sync latency time series.

Source: `proto/warehouse/warehouse.proto:197-200`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `timestamp_millis` | `google.protobuf.DoubleValue` | 1 | `bucket` | Timestamp in milliseconds since epoch (JSON serialized as `"bucket"`) |
| `latency_seconds` | `google.protobuf.DoubleValue` | 2 | `latency` | Sync latency in seconds (JSON serialized as `"latency"`) |

### GetDestinationNamespacesRequest

Request message for retrieving namespace mappings for a destination.

Source: `proto/warehouse/warehouse.proto:202-204`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `destination_id` | `string` | 1 | `destinationId` | Destination identifier |

### GetDestinationNamespacesResponse

Response containing namespace mappings for a destination.

Source: `proto/warehouse/warehouse.proto:206-208`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `namespace_mappings` | `repeated NamespaceMapping` | 1 | `namespaceMappings` | List of source-to-namespace mappings |

### NamespaceMapping

Maps a source to its warehouse namespace (schema name).

Source: `proto/warehouse/warehouse.proto:210-213`

| Field | Type | Number | JSON Name | Description |
|-------|------|--------|-----------|-------------|
| `source_id` | `string` | 1 | `sourceId` | Source identifier |
| `namespace` | `string` | 2 | `namespace` | Namespace or schema name in the warehouse |

---

## RPC Methods

This section provides detailed documentation for each of the 15 unary RPCs, including full gRPC method paths, request/response schemas, `grpcurl` examples, and Go client examples.

> **Full method path constants** are defined in `proto/warehouse/warehouse_grpc.pb.go:23-38`.

---

### 1. GetHealth

Performs a health check on the warehouse service, returning a boolean value indicating whether the service is healthy and ready to accept requests.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/GetHealth` |
| **Request** | `google.protobuf.Empty` |
| **Response** | `google.protobuf.BoolValue` |
| **Category** | Health |

Source: `proto/warehouse/warehouse.proto:11`, `proto/warehouse/warehouse_grpc.pb.go:24`

**grpcurl Example:**

```bash
# Check warehouse service health
grpcurl -plaintext localhost:8082 proto.Warehouse/GetHealth
```

**Expected Response:**

```json
{
  "value": true
}
```

**Go Client Example:**

```go
health, err := client.GetHealth(ctx, &emptypb.Empty{})
if err != nil {
    log.Fatalf("health check failed: %v", err)
}
fmt.Printf("Warehouse healthy: %v\n", health.GetValue())
```

---

### 2. GetWHUploads

Lists warehouse uploads with optional filtering by source, destination, destination type, status, and workspace. Results are paginated.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/GetWHUploads` |
| **Request** | `WHUploadsRequest` |
| **Response** | `WHUploadsResponse` |
| **Category** | Upload Management |

Source: `proto/warehouse/warehouse.proto:12`, `proto/warehouse/warehouse_grpc.pb.go:25`

**grpcurl Example:**

```bash
# List the 10 most recent Snowflake uploads
grpcurl -plaintext -d '{
  "destination_type": "SNOWFLAKE",
  "limit": 10,
  "offset": 0
}' localhost:8082 proto.Warehouse/GetWHUploads
```

```bash
# List failed uploads for a specific source and destination
grpcurl -plaintext -d '{
  "source_id": "source_abc123",
  "destination_id": "dest_xyz789",
  "status": "aborted",
  "limit": 25,
  "offset": 0,
  "workspace_id": "workspace_001"
}' localhost:8082 proto.Warehouse/GetWHUploads
```

**Go Client Example:**

```go
resp, err := client.GetWHUploads(ctx, &pb.WHUploadsRequest{
    DestinationType: "SNOWFLAKE",
    Status:          "aborted",
    Limit:           10,
    Offset:          0,
})
if err != nil {
    log.Fatalf("failed to list uploads: %v", err)
}
fmt.Printf("Total uploads: %d\n", resp.GetPagination().GetTotal())
for _, upload := range resp.GetUploads() {
    fmt.Printf("Upload %d: status=%s, destination=%s\n",
        upload.GetId(), upload.GetStatus(), upload.GetDestinationType())
}
```

---

### 3. GetWHUpload

Retrieves detailed information about a single warehouse upload by its ID, including per-table load status breakdown.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/GetWHUpload` |
| **Request** | `WHUploadRequest` |
| **Response** | `WHUploadResponse` |
| **Category** | Upload Management |

Source: `proto/warehouse/warehouse.proto:13`, `proto/warehouse/warehouse_grpc.pb.go:26`

**grpcurl Example:**

```bash
# Get details for upload ID 42
grpcurl -plaintext -d '{
  "upload_id": 42,
  "workspace_id": "workspace_001"
}' localhost:8082 proto.Warehouse/GetWHUpload
```

**Go Client Example:**

```go
upload, err := client.GetWHUpload(ctx, &pb.WHUploadRequest{
    UploadId:    42,
    WorkspaceId: "workspace_001",
})
if err != nil {
    log.Fatalf("failed to get upload: %v", err)
}
fmt.Printf("Upload %d: status=%s, attempt=%d, tables=%d\n",
    upload.GetId(), upload.GetStatus(), upload.GetAttempt(), len(upload.GetTables()))
for _, table := range upload.GetTables() {
    fmt.Printf("  Table %s: status=%s, count=%d\n",
        table.GetName(), table.GetStatus(), table.GetCount())
}
```

---

### 4. TriggerWHUpload

Triggers processing of a single warehouse upload by its ID. The upload is moved to the front of the processing queue.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/TriggerWHUpload` |
| **Request** | `WHUploadRequest` |
| **Response** | `TriggerWhUploadsResponse` |
| **Category** | Upload Trigger |

Source: `proto/warehouse/warehouse.proto:14`, `proto/warehouse/warehouse_grpc.pb.go:27`

**grpcurl Example:**

```bash
# Trigger upload ID 42
grpcurl -plaintext -d '{
  "upload_id": 42,
  "workspace_id": "workspace_001"
}' localhost:8082 proto.Warehouse/TriggerWHUpload
```

**Go Client Example:**

```go
resp, err := client.TriggerWHUpload(ctx, &pb.WHUploadRequest{
    UploadId:    42,
    WorkspaceId: "workspace_001",
})
if err != nil {
    log.Fatalf("failed to trigger upload: %v", err)
}
fmt.Printf("Trigger result: %s (code %d)\n", resp.GetMessage(), resp.GetStatusCode())
```

---

### 5. TriggerWHUploads

Triggers processing of multiple warehouse uploads matching the given filter criteria. All matching uploads are queued for immediate processing.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/TriggerWHUploads` |
| **Request** | `WHUploadsRequest` |
| **Response** | `TriggerWhUploadsResponse` |
| **Category** | Upload Trigger |

Source: `proto/warehouse/warehouse.proto:15`, `proto/warehouse/warehouse_grpc.pb.go:28`

**grpcurl Example:**

```bash
# Trigger all waiting Snowflake uploads for a workspace
grpcurl -plaintext -d '{
  "destination_type": "SNOWFLAKE",
  "status": "waiting",
  "workspace_id": "workspace_001"
}' localhost:8082 proto.Warehouse/TriggerWHUploads
```

**Go Client Example:**

```go
resp, err := client.TriggerWHUploads(ctx, &pb.WHUploadsRequest{
    DestinationType: "SNOWFLAKE",
    Status:          "waiting",
    WorkspaceId:     "workspace_001",
})
if err != nil {
    log.Fatalf("failed to trigger uploads: %v", err)
}
fmt.Printf("Trigger result: %s (code %d)\n", resp.GetMessage(), resp.GetStatusCode())
```

---

### 6. Validate

Validates a warehouse destination configuration by executing a specific validation step (e.g., connectivity test, permission check, schema verification).

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/Validate` |
| **Request** | `WHValidationRequest` |
| **Response** | `WHValidationResponse` |
| **Category** | Validation |

Source: `proto/warehouse/warehouse.proto:16`, `proto/warehouse/warehouse_grpc.pb.go:29`

**grpcurl Example:**

```bash
# Validate a Snowflake destination configuration
grpcurl -plaintext -d '{
  "role": "admin",
  "path": "/validate/destination",
  "step": "test_connection",
  "body": "{\"type\":\"SNOWFLAKE\",\"settings\":{\"account\":\"myaccount.snowflakecomputing.com\",\"warehouse\":\"COMPUTE_WH\",\"database\":\"RUDDER_DB\",\"user\":\"rudder_user\",\"password\":\"secret\"}}"
}' localhost:8082 proto.Warehouse/Validate
```

**Go Client Example:**

```go
resp, err := client.Validate(ctx, &pb.WHValidationRequest{
    Role: "admin",
    Path: "/validate/destination",
    Step: "test_connection",
    Body: `{"type":"SNOWFLAKE","settings":{"account":"myaccount","warehouse":"COMPUTE_WH","database":"RUDDER_DB"}}`,
})
if err != nil {
    log.Fatalf("validation RPC failed: %v", err)
}
if resp.GetError() != "" {
    fmt.Printf("Validation failed: %s\n", resp.GetError())
} else {
    fmt.Printf("Validation passed: %s\n", resp.GetData())
}
```

---

### 7. RetryWHUploads

Retries failed or aborted warehouse uploads matching the specified filter criteria. Supports targeting specific upload IDs or using filter-based selection.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/RetryWHUploads` |
| **Request** | `RetryWHUploadsRequest` |
| **Response** | `RetryWHUploadsResponse` |
| **Category** | Retry |

Source: `proto/warehouse/warehouse.proto:17`, `proto/warehouse/warehouse_grpc.pb.go:30`

**grpcurl Example:**

```bash
# Retry all uploads failed in the last 24 hours for a specific destination
grpcurl -plaintext -d '{
  "workspaceId": "workspace_001",
  "destinationId": "dest_xyz789",
  "destinationType": "SNOWFLAKE",
  "intervalInHours": 24
}' localhost:8082 proto.Warehouse/RetryWHUploads
```

```bash
# Force retry specific upload IDs (including aborted uploads)
grpcurl -plaintext -d '{
  "workspaceId": "workspace_001",
  "uploadIds": [42, 43, 44],
  "forceRetry": true
}' localhost:8082 proto.Warehouse/RetryWHUploads
```

**Go Client Example:**

```go
resp, err := client.RetryWHUploads(ctx, &pb.RetryWHUploadsRequest{
    WorkspaceId:     "workspace_001",
    DestinationId:   "dest_xyz789",
    DestinationType: "SNOWFLAKE",
    IntervalInHours: 24,
    ForceRetry:      false,
})
if err != nil {
    log.Fatalf("retry failed: %v", err)
}
fmt.Printf("Retried %d uploads: %s\n", resp.GetCount(), resp.GetMessage())
```

---

### 8. CountWHUploadsToRetry

Counts the number of uploads eligible for retry without actually executing the retry. Uses the same request parameters as `RetryWHUploads` for a dry-run count.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/CountWHUploadsToRetry` |
| **Request** | `RetryWHUploadsRequest` |
| **Response** | `RetryWHUploadsResponse` |
| **Category** | Retry |

Source: `proto/warehouse/warehouse.proto:18`, `proto/warehouse/warehouse_grpc.pb.go:31`

**grpcurl Example:**

```bash
# Count uploads eligible for retry in the last 48 hours
grpcurl -plaintext -d '{
  "workspaceId": "workspace_001",
  "destinationType": "BQ",
  "intervalInHours": 48
}' localhost:8082 proto.Warehouse/CountWHUploadsToRetry
```

**Go Client Example:**

```go
resp, err := client.CountWHUploadsToRetry(ctx, &pb.RetryWHUploadsRequest{
    WorkspaceId:     "workspace_001",
    DestinationType: "BQ",
    IntervalInHours: 48,
})
if err != nil {
    log.Fatalf("count failed: %v", err)
}
fmt.Printf("Uploads eligible for retry: %d\n", resp.GetCount())
```

---

### 9. ValidateObjectStorageDestination

Validates an object storage destination configuration by testing connectivity and permissions against S3, GCS, or Azure Blob Storage.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/ValidateObjectStorageDestination` |
| **Request** | `ValidateObjectStorageRequest` |
| **Response** | `ValidateObjectStorageResponse` |
| **Category** | Validation |

Source: `proto/warehouse/warehouse.proto:19`, `proto/warehouse/warehouse_grpc.pb.go:32`

**grpcurl Example:**

```bash
# Validate an S3 bucket configuration
grpcurl -plaintext -d '{
  "type": "S3",
  "config": {
    "bucketName": "my-rudder-bucket",
    "accessKeyID": "AKIAIOSFODNN7EXAMPLE",
    "accessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
    "region": "us-east-1"
  }
}' localhost:8082 proto.Warehouse/ValidateObjectStorageDestination
```

```bash
# Validate a GCS bucket configuration
grpcurl -plaintext -d '{
  "type": "GCS",
  "config": {
    "bucketName": "my-rudder-gcs-bucket",
    "credentials": "{\"type\":\"service_account\",\"project_id\":\"my-project\"}"
  }
}' localhost:8082 proto.Warehouse/ValidateObjectStorageDestination
```

**Go Client Example:**

```go
config, _ := structpb.NewStruct(map[string]interface{}{
    "bucketName":  "my-rudder-bucket",
    "accessKeyID": "AKIAIOSFODNN7EXAMPLE",
    "accessKey":   "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
    "region":      "us-east-1",
})
resp, err := client.ValidateObjectStorageDestination(ctx, &pb.ValidateObjectStorageRequest{
    Type:   "S3",
    Config: config,
})
if err != nil {
    log.Fatalf("validation RPC failed: %v", err)
}
if resp.GetIsValid() {
    fmt.Println("Object storage configuration is valid")
} else {
    fmt.Printf("Validation failed: %s\n", resp.GetError())
}
```

---

### 10. RetrieveFailedBatches

Retrieves diagnostic information about failed batch syncs within a specified time range for a given workspace and destination.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/RetrieveFailedBatches` |
| **Request** | `RetrieveFailedBatchesRequest` |
| **Response** | `RetrieveFailedBatchesResponse` |
| **Category** | Diagnostics |

Source: `proto/warehouse/warehouse.proto:20`, `proto/warehouse/warehouse_grpc.pb.go:33`

**grpcurl Example:**

```bash
# Retrieve failed batches from the last 7 days
grpcurl -plaintext -d '{
  "workspaceID": "workspace_001",
  "destinationID": "dest_xyz789",
  "start": "2025-02-18T00:00:00Z",
  "end": "2025-02-25T23:59:59Z"
}' localhost:8082 proto.Warehouse/RetrieveFailedBatches
```

**Go Client Example:**

```go
resp, err := client.RetrieveFailedBatches(ctx, &pb.RetrieveFailedBatchesRequest{
    WorkspaceID:   "workspace_001",
    DestinationID: "dest_xyz789",
    Start:         "2025-02-18T00:00:00Z",
    End:           "2025-02-25T23:59:59Z",
})
if err != nil {
    log.Fatalf("failed to retrieve failed batches: %v", err)
}
for _, batch := range resp.GetFailedBatches() {
    fmt.Printf("Error: %s (category: %s), source: %s, failed events: %d, failed syncs: %d\n",
        batch.GetError(), batch.GetErrorCategory(), batch.GetSourceID(),
        batch.GetFailedEventsCount(), batch.GetFailedSyncsCount())
}
```

---

### 11. RetryFailedBatches

Retries failed batch syncs matching the specified filters, including optional filtering by error category, source, and status.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/RetryFailedBatches` |
| **Request** | `RetryFailedBatchesRequest` |
| **Response** | `RetryFailedBatchesResponse` |
| **Category** | Retry |

Source: `proto/warehouse/warehouse.proto:21`, `proto/warehouse/warehouse_grpc.pb.go:34`

**grpcurl Example:**

```bash
# Retry all permission-error batch failures in the last week
grpcurl -plaintext -d '{
  "workspaceID": "workspace_001",
  "destinationID": "dest_xyz789",
  "start": "2025-02-18T00:00:00Z",
  "end": "2025-02-25T23:59:59Z",
  "errorCategory": "permission_error"
}' localhost:8082 proto.Warehouse/RetryFailedBatches
```

**Go Client Example:**

```go
resp, err := client.RetryFailedBatches(ctx, &pb.RetryFailedBatchesRequest{
    WorkspaceID:   "workspace_001",
    DestinationID: "dest_xyz789",
    Start:         "2025-02-18T00:00:00Z",
    End:           "2025-02-25T23:59:59Z",
    ErrorCategory: "permission_error",
})
if err != nil {
    log.Fatalf("retry failed: %v", err)
}
fmt.Printf("Retried %d syncs\n", resp.GetRetriedSyncsCount())
```

---

### 12. GetFirstAbortedUploadInContinuousAbortsByDestination

Identifies the first aborted upload in a continuous sequence of aborted uploads for each destination. This diagnostic RPC helps pinpoint when a destination started experiencing persistent failures.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/GetFirstAbortedUploadInContinuousAbortsByDestination` |
| **Request** | `FirstAbortedUploadInContinuousAbortsByDestinationRequest` |
| **Response** | `FirstAbortedUploadInContinuousAbortsByDestinationResponse` |
| **Category** | Diagnostics |

Source: `proto/warehouse/warehouse.proto:22`, `proto/warehouse/warehouse_grpc.pb.go:35`

**grpcurl Example:**

```bash
# Find the first aborted upload per destination since a given date
grpcurl -plaintext -d '{
  "workspace_id": "workspace_001",
  "start": "2025-02-01T00:00:00Z"
}' localhost:8082 proto.Warehouse/GetFirstAbortedUploadInContinuousAbortsByDestination
```

**Go Client Example:**

```go
resp, err := client.GetFirstAbortedUploadInContinuousAbortsByDestination(ctx,
    &pb.FirstAbortedUploadInContinuousAbortsByDestinationRequest{
        WorkspaceId: "workspace_001",
        Start:       "2025-02-01T00:00:00Z",
    })
if err != nil {
    log.Fatalf("failed to get aborted uploads: %v", err)
}
for _, upload := range resp.GetUploads() {
    fmt.Printf("Destination %s: first aborted upload ID %d (created: %v, events: %v to %v)\n",
        upload.GetDestinationId(), upload.GetId(),
        upload.GetCreatedAt().AsTime(),
        upload.GetFirstEventAt().AsTime(),
        upload.GetLastEventAt().AsTime())
}
```

---

### 13. GetSyncLatency

Retrieves sync latency time series data for a given destination and source combination. Data points are aggregated into buckets of the specified minute interval.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/GetSyncLatency` |
| **Request** | `SyncLatencyRequest` |
| **Response** | `SyncLatencyResponse` |
| **Category** | Telemetry |

Source: `proto/warehouse/warehouse.proto:23`, `proto/warehouse/warehouse_grpc.pb.go:36`

**grpcurl Example:**

```bash
# Get sync latency for the last 24 hours with 15-minute aggregation
grpcurl -plaintext -d '{
  "destination_id": "dest_xyz789",
  "workspace_id": "workspace_001",
  "start_time": "2025-02-24T00:00:00Z",
  "aggregation_minutes": "15",
  "source_id": "source_abc123"
}' localhost:8082 proto.Warehouse/GetSyncLatency
```

**Expected Response (JSON):**

```json
{
  "graph": [
    { "bucket": 1740355200000, "latency": 12.5 },
    { "bucket": 1740356100000, "latency": 8.2 },
    { "bucket": 1740357000000, "latency": 15.1 }
  ]
}
```

> **Note:** The JSON response uses the custom field names `"graph"`, `"bucket"`, and `"latency"` as defined by `json_name` annotations in the proto definition.

**Go Client Example:**

```go
resp, err := client.GetSyncLatency(ctx, &pb.SyncLatencyRequest{
    DestinationId:      "dest_xyz789",
    WorkspaceId:        "workspace_001",
    StartTime:          "2025-02-24T00:00:00Z",
    AggregationMinutes: "15",
    SourceId:           "source_abc123",
})
if err != nil {
    log.Fatalf("failed to get sync latency: %v", err)
}
for _, dp := range resp.GetTimeSeriesDataPoints() {
    fmt.Printf("Timestamp: %.0f ms, Latency: %.2f sec\n",
        dp.GetTimestampMillis().GetValue(), dp.GetLatencySeconds().GetValue())
}
```

---

### 14. SyncWHSchema

Triggers an immediate schema synchronization for a specific destination. The service will fetch the latest schema from the warehouse and update its internal schema cache.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/SyncWHSchema` |
| **Request** | `SyncWHSchemaRequest` |
| **Response** | `google.protobuf.Empty` |
| **Category** | Schema |

Source: `proto/warehouse/warehouse.proto:24`, `proto/warehouse/warehouse_grpc.pb.go:37`

**grpcurl Example:**

```bash
# Trigger schema sync for a specific destination
grpcurl -plaintext -d '{
  "destination_id": "dest_xyz789"
}' localhost:8082 proto.Warehouse/SyncWHSchema
```

**Go Client Example:**

```go
_, err := client.SyncWHSchema(ctx, &pb.SyncWHSchemaRequest{
    DestinationId: "dest_xyz789",
})
if err != nil {
    log.Fatalf("schema sync failed: %v", err)
}
fmt.Println("Schema synchronization triggered successfully")
```

---

### 15. GetDestinationNamespaces

Retrieves the namespace (database schema) mappings for a given destination. Each source connected to the destination may map to a different namespace.

| Property | Value |
|----------|-------|
| **Full method path** | `/proto.Warehouse/GetDestinationNamespaces` |
| **Request** | `GetDestinationNamespacesRequest` |
| **Response** | `GetDestinationNamespacesResponse` |
| **Category** | Namespace |

Source: `proto/warehouse/warehouse.proto:25`, `proto/warehouse/warehouse_grpc.pb.go:38`

**grpcurl Example:**

```bash
# Get namespace mappings for a destination
grpcurl -plaintext -d '{
  "destination_id": "dest_xyz789"
}' localhost:8082 proto.Warehouse/GetDestinationNamespaces
```

**Go Client Example:**

```go
resp, err := client.GetDestinationNamespaces(ctx, &pb.GetDestinationNamespacesRequest{
    DestinationId: "dest_xyz789",
})
if err != nil {
    log.Fatalf("failed to get namespaces: %v", err)
}
for _, mapping := range resp.GetNamespaceMappings() {
    fmt.Printf("Source %s → Namespace %s\n",
        mapping.GetSourceId(), mapping.GetNamespace())
}
```

---

## Go Client Usage

The Warehouse gRPC client is generated from the proto definition and available in the `proto/warehouse` package. Below is a complete example demonstrating client initialization and common operations.

Source: `proto/warehouse/warehouse_grpc.pb.go:44-68`

### Client Setup

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    "google.golang.org/protobuf/types/known/emptypb"

    pb "github.com/rudderlabs/rudder-server/proto/warehouse"
)

func main() {
    // Establish gRPC connection to the warehouse service
    conn, err := grpc.NewClient("localhost:8082",
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )
    if err != nil {
        log.Fatalf("failed to connect: %v", err)
    }
    defer conn.Close()

    // Create the Warehouse client
    client := pb.NewWarehouseClient(conn)

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // 1. Health check
    health, err := client.GetHealth(ctx, &emptypb.Empty{})
    if err != nil {
        log.Fatalf("health check failed: %v", err)
    }
    fmt.Printf("Warehouse service healthy: %v\n", health.GetValue())

    // 2. List recent uploads
    uploads, err := client.GetWHUploads(ctx, &pb.WHUploadsRequest{
        DestinationType: "SNOWFLAKE",
        Limit:           10,
    })
    if err != nil {
        log.Fatalf("failed to list uploads: %v", err)
    }
    fmt.Printf("Found %d uploads (total: %d)\n",
        len(uploads.GetUploads()), uploads.GetPagination().GetTotal())

    // 3. Retry failed uploads from the last 24 hours
    retryResp, err := client.RetryWHUploads(ctx, &pb.RetryWHUploadsRequest{
        WorkspaceId:     "workspace_001",
        DestinationType: "SNOWFLAKE",
        IntervalInHours: 24,
    })
    if err != nil {
        log.Fatalf("retry failed: %v", err)
    }
    fmt.Printf("Retried %d uploads\n", retryResp.GetCount())

    // 4. Trigger schema sync
    _, err = client.SyncWHSchema(ctx, &pb.SyncWHSchemaRequest{
        DestinationId: "dest_xyz789",
    })
    if err != nil {
        log.Fatalf("schema sync failed: %v", err)
    }
    fmt.Println("Schema sync triggered")
}
```

### Client Interface

The generated `WarehouseClient` interface exposes all 15 RPCs. All methods follow the standard gRPC pattern of accepting a `context.Context`, a request message, and optional `grpc.CallOption` variadic arguments.

Source: `proto/warehouse/warehouse_grpc.pb.go:44-60`

```go
type WarehouseClient interface {
    GetHealth(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*wrapperspb.BoolValue, error)
    GetWHUploads(ctx context.Context, in *WHUploadsRequest, opts ...grpc.CallOption) (*WHUploadsResponse, error)
    GetWHUpload(ctx context.Context, in *WHUploadRequest, opts ...grpc.CallOption) (*WHUploadResponse, error)
    TriggerWHUpload(ctx context.Context, in *WHUploadRequest, opts ...grpc.CallOption) (*TriggerWhUploadsResponse, error)
    TriggerWHUploads(ctx context.Context, in *WHUploadsRequest, opts ...grpc.CallOption) (*TriggerWhUploadsResponse, error)
    Validate(ctx context.Context, in *WHValidationRequest, opts ...grpc.CallOption) (*WHValidationResponse, error)
    RetryWHUploads(ctx context.Context, in *RetryWHUploadsRequest, opts ...grpc.CallOption) (*RetryWHUploadsResponse, error)
    CountWHUploadsToRetry(ctx context.Context, in *RetryWHUploadsRequest, opts ...grpc.CallOption) (*RetryWHUploadsResponse, error)
    ValidateObjectStorageDestination(ctx context.Context, in *ValidateObjectStorageRequest, opts ...grpc.CallOption) (*ValidateObjectStorageResponse, error)
    RetrieveFailedBatches(ctx context.Context, in *RetrieveFailedBatchesRequest, opts ...grpc.CallOption) (*RetrieveFailedBatchesResponse, error)
    RetryFailedBatches(ctx context.Context, in *RetryFailedBatchesRequest, opts ...grpc.CallOption) (*RetryFailedBatchesResponse, error)
    GetFirstAbortedUploadInContinuousAbortsByDestination(ctx context.Context, in *FirstAbortedUploadInContinuousAbortsByDestinationRequest, opts ...grpc.CallOption) (*FirstAbortedUploadInContinuousAbortsByDestinationResponse, error)
    GetSyncLatency(ctx context.Context, in *SyncLatencyRequest, opts ...grpc.CallOption) (*SyncLatencyResponse, error)
    SyncWHSchema(ctx context.Context, in *SyncWHSchemaRequest, opts ...grpc.CallOption) (*emptypb.Empty, error)
    GetDestinationNamespaces(ctx context.Context, in *GetDestinationNamespacesRequest, opts ...grpc.CallOption) (*GetDestinationNamespacesResponse, error)
}
```

### Server Implementation

To implement the Warehouse gRPC server, embed `UnimplementedWarehouseServer` for forward compatibility and override the desired methods:

Source: `proto/warehouse/warehouse_grpc.pb.go:205-276`

```go
type myWarehouseServer struct {
    pb.UnimplementedWarehouseServer
}

func (s *myWarehouseServer) GetHealth(ctx context.Context, _ *emptypb.Empty) (*wrapperspb.BoolValue, error) {
    return &wrapperspb.BoolValue{Value: true}, nil
}

// Register the server
grpcServer := grpc.NewServer()
pb.RegisterWarehouseServer(grpcServer, &myWarehouseServer{})
```

> **Forward Compatibility:** All implementations **must** embed `UnimplementedWarehouseServer`. Unimplemented methods return `codes.Unimplemented` by default, allowing the service to gracefully handle calls to RPCs added in future proto versions.

---

## Error Handling

The Warehouse gRPC service uses standard gRPC status codes for error reporting. All errors are returned as `google.golang.org/grpc/status` errors.

Source: `proto/warehouse/warehouse_grpc.pb.go:227-275`

### Standard gRPC Status Codes

| Code | Name | Usage in Warehouse Service |
|------|------|---------------------------|
| `0` | `OK` | Request completed successfully |
| `2` | `UNKNOWN` | Unexpected internal error |
| `3` | `INVALID_ARGUMENT` | Invalid request parameters (e.g., missing required fields, invalid time range) |
| `5` | `NOT_FOUND` | Requested upload, destination, or workspace not found |
| `7` | `PERMISSION_DENIED` | Insufficient permissions for the requested operation |
| `12` | `UNIMPLEMENTED` | RPC method not implemented (returned by `UnimplementedWarehouseServer` for forward compatibility) |
| `13` | `INTERNAL` | Internal server error during processing |
| `14` | `UNAVAILABLE` | Service temporarily unavailable (e.g., during startup or maintenance) |

### Error Handling in Go

```go
import (
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
)

resp, err := client.GetWHUpload(ctx, &pb.WHUploadRequest{
    UploadId:    999999,
    WorkspaceId: "workspace_001",
})
if err != nil {
    st, ok := status.FromError(err)
    if ok {
        switch st.Code() {
        case codes.NotFound:
            fmt.Printf("Upload not found: %s\n", st.Message())
        case codes.InvalidArgument:
            fmt.Printf("Invalid request: %s\n", st.Message())
        case codes.Unavailable:
            fmt.Printf("Service unavailable, retry later: %s\n", st.Message())
        case codes.Unimplemented:
            fmt.Printf("RPC not implemented on this server version: %s\n", st.Message())
        default:
            fmt.Printf("gRPC error (code %d): %s\n", st.Code(), st.Message())
        }
    } else {
        fmt.Printf("Non-gRPC error: %v\n", err)
    }
}
```

### Error Handling with grpcurl

When using `grpcurl`, gRPC errors are displayed as JSON:

```bash
# Example error response for a non-existent upload
grpcurl -plaintext -d '{"upload_id": 999999, "workspace_id": "ws_001"}' \
  localhost:8082 proto.Warehouse/GetWHUpload

# Output:
# ERROR:
#   Code: NotFound
#   Message: upload 999999 not found
```

### Common Error Scenarios

| RPC Category | Error Scenario | Expected Code | Resolution |
|-------------|----------------|---------------|------------|
| Upload Management | Upload ID does not exist | `NOT_FOUND` | Verify the upload ID using `GetWHUploads` |
| Upload Management | Workspace ID mismatch | `PERMISSION_DENIED` | Ensure the workspace ID matches the upload's workspace |
| Validation | Invalid destination configuration | `INVALID_ARGUMENT` | Check configuration parameters (credentials, endpoints) |
| Validation | Destination unreachable | `UNAVAILABLE` | Verify network connectivity and destination availability |
| Retry | No uploads match retry criteria | `OK` | Returns success with `count: 0` — check filter parameters |
| Schema | Destination not configured | `NOT_FOUND` | Verify destination ID exists in configuration |
| Telemetry | Invalid time range | `INVALID_ARGUMENT` | Ensure `start_time` is a valid ISO 8601 timestamp |

---

## See Also

- [API Overview & Authentication](index.md) — HTTP Gateway authentication and complete API surface overview
- [Admin API](admin-api.md) — Runtime administration operations via UNIX socket RPC
- [Gateway HTTP API](gateway-http-api.md) — Full HTTP API reference for event ingestion
- [Error Codes](error-codes.md) — HTTP response codes and error message reference
- [Warehouse Overview](../warehouse/overview.md) — Warehouse service architecture, upload state machine, and operational modes
- [Warehouse Sync Operations](../guides/operations/warehouse-sync.md) — Sync configuration, monitoring, and troubleshooting
- [Snowflake Connector](../warehouse/snowflake.md) — Snowflake-specific setup and configuration
- [BigQuery Connector](../warehouse/bigquery.md) — BigQuery-specific setup and configuration
- [Redshift Connector](../warehouse/redshift.md) — Redshift-specific setup and configuration
