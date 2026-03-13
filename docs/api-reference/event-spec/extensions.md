# RudderStack Extensions

RudderStack extends the Segment Spec with additional endpoints, event types, and configuration defaults that serve high-throughput, enterprise, and specialized use cases. These extensions are **not parity gaps** — they are intentional additions that maintain full backward compatibility with existing Segment-compatible SDKs and API integrations.

> **Source references:**
>
> - `gateway/handle_http_replay.go` — Replay endpoint implementation
> - `gateway/handle_http_retl.go` — Reverse ETL endpoint implementation
> - `gateway/handle_http_beacon.go` — Beacon endpoint implementation
> - `gateway/handle_http_pixel.go` — Pixel endpoint implementation
> - `gateway/handle_http.go` — Core HTTP handler wiring for all event types including `merge` and `extract`
> - `gateway/handle_lifecycle.go` — Route registration and chi router configuration
> - `gateway/openapi.yaml` — OpenAPI 3.0.3 specification for all Gateway endpoints
> - `config/config.yaml` — Master runtime configuration

> **Note:** All extension endpoints use the same authentication, validation, and pipeline infrastructure as the core Segment-compatible endpoints. Events ingested through extension endpoints pass through the same Gateway → Processor → Router → Destination pipeline.

---

## Extension Endpoints

### `/internal/v1/replay` — Event Replay Re-Ingestion

The replay endpoint enables re-ingestion of previously-captured events for reprocessing through the full pipeline. This is used for historical data replay and event reprocessing scenarios.

| Property | Value |
|----------|-------|
| **Method** | `POST` |
| **Path** | `/internal/v1/replay` |
| **Authentication** | `replaySourceIDAuth` — Source ID authentication via headers |
| **Call Type Label** | `replay` |
| **Handler** | `webReplayHandler()` → `callType("replay", replaySourceIDAuth(webHandler()))` |

The replay handler composes the shared web handler (`webHandler()`) with replay-specific source ID authentication middleware (`replaySourceIDAuth`) and the `callType("replay")` instrumentation label. This means replay events pass through the same validation, batching, and queueing logic as standard events.

**Route registration:** `gateway/handle_lifecycle.go` registers this endpoint under the `/internal` route group via `r.Post("/v1/replay", gw.webReplayHandler())`.

Source: `gateway/handle_http_replay.go`

---

### `/internal/v1/retl` — Reverse ETL Event Ingestion

The Reverse ETL endpoint accepts events from RudderStack's Reverse ETL pipeline, where data from warehouse sources is transformed into event payloads and sent to destinations.

| Property | Value |
|----------|-------|
| **Method** | `POST` |
| **Path** | `/internal/v1/retl` |
| **Authentication** | `sourceDestIDAuth` — Requires both source and destination identity headers |
| **Call Type Label** | `retl` |
| **Handler** | `webRetlHandler()` → `callType("retl", sourceDestIDAuth(webHandler()))` |

The RETL handler uses `sourceDestIDAuth` middleware, which requires both source and destination identity headers (unlike standard endpoints that only require a write key). This dual authentication ensures that Reverse ETL events are correctly attributed to both the source warehouse and the target destination.

**Route registration:** `gateway/handle_lifecycle.go` registers this endpoint under the `/internal` route group via `r.Post("/v1/retl", gw.webRetlHandler())`.

Source: `gateway/handle_http_retl.go`

---

### `/beacon/v1/batch` — Beacon-Based Tracking

Beacon endpoints support the [Beacon API](https://developer.mozilla.org/en-US/docs/Web/API/Beacon_API) for reliable event delivery during page unload events. The key difference from standard endpoints is that the write key is passed as a **query parameter** instead of an HTTP header.

| Property | Value |
|----------|-------|
| **Method** | `POST` |
| **Path** | `/beacon/v1/batch` |
| **Authentication** | Write key via `?writeKey=<key>` query parameter (converted to Basic Auth header internally) |
| **Call Type Label** | Inherits from `webBatchHandler` |
| **Handler** | `beaconBatchHandler()` → `beaconInterceptor(webBatchHandler())` |

The `beaconInterceptor` middleware:

1. Extracts the `writeKey` from the URL query parameters
2. Sets it as a Basic Auth header via `Request.SetBasicAuth(writeKey, "")`
3. Removes the `writeKey` from the query string via `delete(queryParams, "writeKey")` to avoid credential leakage in URLs
4. Delegates to the standard `webBatchHandler` for normal processing

If the `writeKey` query parameter is missing or empty, the interceptor returns HTTP 401 Unauthorized with the canonical `response.NoWriteKeyInQueryParams` error message and records the failure in gateway metrics via `gwstats.SourceStat`.

**Route registration:** `gateway/handle_lifecycle.go` registers this endpoint directly on the root mux via `srvMux.Post("/beacon/v1/batch", gw.beaconBatchHandler())`.

Source: `gateway/handle_http_beacon.go`

---

### `/pixel/v1/*` — Pixel Tracking with GIF Response

Pixel endpoints enable tracking via image tags (`<img>` elements) where event data is passed as URL query parameters. The server **always returns a 1×1 transparent GIF image** regardless of the processing outcome, ensuring the tracking pixel renders correctly in all scenarios.

| Property | Value |
|----------|-------|
| **Method** | `GET` |
| **Paths** | `/pixel/v1/page`, `/pixel/v1/track` |
| **Authentication** | Write key via `?writeKey=<key>` query parameter |
| **Response** | Always returns `Content-Type: image/gif` with a 1×1 transparent GIF |
| **Handlers** | `pixelPageHandler()`, `pixelTrackHandler()` |

The `pixelInterceptor` middleware:

1. Always defers writing the GIF response (ensuring it returns regardless of errors)
2. Extracts `writeKey` from query parameters and sets Basic Auth on a newly constructed POST request
3. Forwards the `X-Forwarded-For` header from the original request to preserve client IP attribution
4. Calls `preparePixelPayload()` which:
   - Sets `channel` to `"web"` and `integrations` to `{"All": true}`
   - Generates `originalTimestamp` and `sentAt` from the server clock via `gw.now()`
   - Sanitizes `anonymousId` by stripping surrounding double quotes via regex
   - Maps all remaining query parameters into the JSON payload via `sjson.SetBytes`
   - Sets `type` to the appropriate event type (`page` or `track`)
   - For `page` requests: defaults empty `name` to `"Unknown Page"`
   - For `track` requests: if the `event` query parameter is present but empty, returns error `"track: Mandatory field 'event' missing"`; if `event` is present with a value, it is set in the payload
5. Replaces the request body with the constructed JSON payload via `io.NopCloser(bytes.NewReader(body))`
6. Wraps the ResponseWriter with `pixelHttpWriter` to capture the downstream response without sending it to the client (the GIF response takes priority)

If `writeKey` is missing, returns metrics and logging for `NoWriteKeyInQueryParams`, but still sends the GIF.

**Route registration:** `gateway/handle_lifecycle.go` registers pixel endpoints under the `/pixel/v1` route group:

```
srvMux.Route("/pixel/v1", func(r chi.Router) {
    r.Get("/track", gw.pixelTrackHandler())
    r.Get("/page", gw.pixelPageHandler())
})
```

Source: `gateway/handle_http_pixel.go`

---

### `/internal/v1/extract` — Data Extraction

The data extraction endpoint supports internal data extraction workflows. It uses the standard web handler with write key authentication.

| Property | Value |
|----------|-------|
| **Method** | `POST` |
| **Path** | `/internal/v1/extract` |
| **Authentication** | `writeKeyAuth` — Write Key Basic Authentication |
| **Call Type Label** | `extract` |
| **Handler** | `webExtractHandler()` → `callType("extract", writeKeyAuth(webHandler()))` |

**Route registration:** `gateway/handle_lifecycle.go` registers this endpoint under the `/internal` route group via `r.Post("/v1/extract", gw.webExtractHandler())`.

Source: `gateway/handle_http.go`, `gateway/openapi.yaml`

---

## `merge` Call Type

RudderStack supports a `merge` call type in addition to Segment's six core event types (`identify`, `track`, `page`, `screen`, `group`, `alias`). The `merge` call type is used for identity merging operations where multiple user identifiers need to be consolidated.

| Property | Value |
|----------|-------|
| **Event Type** | `merge` |
| **Method** | `POST` |
| **Path** | `/v1/merge` |
| **Authentication** | `writeKeyAuth` — Write Key Basic Authentication |
| **Segment Equivalent** | None (RudderStack extension) |
| **Handler** | `webMergeHandler()` → `callType("merge", writeKeyAuth(webHandler()))` |
| **Pipeline Behavior** | Processed through Gateway → Processor → Router like all other event types |

The `merge` event type is used internally for identity resolution workflows and is not part of the Segment Spec's six core event types. It uses the same `writeKeyAuth` authentication and `webHandler()` processing pipeline as the core event types.

**Route registration:** `gateway/handle_lifecycle.go` registers this endpoint under the `/v1` route group alongside the six core endpoints via `r.Post("/merge", gw.webMergeHandler())`.

Source: `gateway/handle_http.go`

---

## Batch Size Defaults

RudderStack uses a more permissive default maximum request size compared to Segment's recommendations:

| Configuration | RudderStack Default | Segment Recommendation |
|---------------|---------------------|------------------------|
| Maximum request body size | **4000 KB** (4 MB) | **500 KB** (recommended) |
| Configuration key | `Gateway.maxReqSizeInKB` | N/A |
| Source | `config/config.yaml` line 27 | Segment SDK documentation |

The configuration is loaded at startup in `gateway/handle_lifecycle.go` via:

```go
gw.conf.maxReqSize = config.GetReloadableIntVar(4000, 1024, "Gateway.maxReqSizeInKB")
```

The value `4000` is the default in kilobytes, multiplied by `1024` to convert to bytes for internal use.

The higher default (4000 KB) is designed for **high-throughput enterprise use cases** where large batches of events are sent in a single HTTP request. This allows:

- Fewer HTTP round-trips for high-volume event pipelines
- Better throughput for server-side SDKs processing large event backlogs
- Reduced overhead for batch import operations via `/v1/import`

> **Recommendation:** For maximum compatibility with Segment SDKs, configure client-side SDKs to use batch sizes within Segment's recommended 500 KB limit. Server-side integrations can safely use larger batches up to the 4 MB default.

Source: `config/config.yaml` — `maxReqSizeInKB: 4000`

---

## Backward Compatibility

All RudderStack extensions maintain **full backward compatibility** with existing users and Segment-compatible SDKs:

- **Core endpoints unchanged** — The six core Segment-compatible endpoints (`/v1/identify`, `/v1/track`, `/v1/page`, `/v1/screen`, `/v1/group`, `/v1/alias`) and `/v1/batch` remain fully compatible with Segment SDKs
- **Extension endpoints are additive** — Replay, RETL, beacon, pixel, and extract endpoints do not modify the behavior of core endpoints
- **`merge` call type is additive** — Adding the `merge` event type does not affect processing of the six core event types
- **Batch size is a ceiling** — The 4 MB default maximum applies to all endpoints equally; clients sending smaller payloads are unaffected
- **Shared pipeline infrastructure** — All extension endpoints use the same `webHandler()` processing path, ensuring consistent behavior across core and extension endpoints

> **Gap Report Reference:** These extensions are documented in the Event Spec Parity gap report as ES-004 (additional endpoints/event types) and ES-006 (permissive batch size defaults). They are classified as intentional extensions, not parity gaps.

---

## Extension Summary

| Extension | Type | Segment Equivalent | Purpose |
|-----------|------|--------------------|---------|
| `/internal/v1/replay` | Endpoint | None | Event replay re-ingestion |
| `/internal/v1/retl` | Endpoint | None | Reverse ETL event ingestion |
| `/beacon/v1/batch` | Endpoint | None | Beacon API support for reliable page-unload tracking |
| `/pixel/v1/page` | Endpoint | None | Pixel page tracking via image tags |
| `/pixel/v1/track` | Endpoint | None | Pixel event tracking via image tags |
| `/internal/v1/extract` | Endpoint | None | Data extraction |
| `/v1/merge` | Event Type | None | Identity merging |
| 4000 KB max request size | Configuration | 500 KB recommended | High-throughput batch support |

---

## See Also

- [Common Fields](common-fields.md) — Common fields shared across all event types
- [Semantic Events](semantic-events.md) — Semantic event category documentation
- [Track](track.md) — Track call specification
- [Alias](alias.md) — Alias call specification (related to `merge` identity operations)
- [Gateway HTTP API](../gateway-http-api.md) — Full HTTP API reference including extension endpoints
- [API Overview & Authentication](../index.md) — Authentication guide covering all auth schemes
- [Error Codes](../error-codes.md) — Error response reference
