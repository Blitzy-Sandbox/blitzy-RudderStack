# Blitzy Project Guide — Sprint 7–9 Warehouse Feature Enhancement

---

## 1. Executive Summary

### 1.1 Project Overview

This project implements Sprint 7–9: Warehouse Feature Enhancement for the RudderStack `rudder-server` (v1.68.1) Go monorepo, delivering five production epics (E-031 through E-035) that close the warehouse sync parity gap from ~80% to ~95% against Twilio Segment. The implementation spans idempotent sync validation across all 9 warehouse connectors, configurable date-range backfill from archived data, enhanced health monitoring with Prometheus metrics and alerting, per-table/per-column selective sync filtering, and end-to-end warehouse replay from archived events. Target users are data engineering teams operating warehouse-first analytics pipelines on RudderStack infrastructure.

### 1.2 Completion Status

```mermaid
pie title Project Completion Status
    "Completed (176h)" : 176
    "Remaining (30h)" : 30
```

| Metric | Value |
|--------|-------|
| **Total Project Hours** | 206 |
| **Completed Hours (AI)** | 176 |
| **Remaining Hours** | 30 |
| **Completion Percentage** | 85.4% |

**Calculation:** 176 completed hours / (176 + 30 remaining hours) = 176 / 206 = **85.4% complete**

### 1.3 Key Accomplishments

- ✅ **E-031 — Idempotent Sync Validation**: Created comprehensive integration test suite for all 9 warehouse connectors (Snowflake, BigQuery, Redshift, ClickHouse, Delta Lake, PostgreSQL, MSSQL, Azure Synapse, Datalake) with canonical test fixtures and staging file helpers
- ✅ **E-032 — Configurable Backfill**: Delivered full `warehouse/backfill/` package with service orchestrator, HTTP API, repository, SQL migrations, state machine extension, and archiver integration — 68 passing unit tests
- ✅ **E-033 — Health Monitoring**: Delivered full `warehouse/healthmonitor/` package with periodic collection, Prometheus metrics (5 metric types), alerting thresholds, HTTP API, gRPC RPCs, repository, and upload pipeline instrumentation — 55 passing unit tests
- ✅ **E-034 — Selective Sync**: Delivered full `warehouse/selectivesync/` package with per-table/per-column filtering integrated across schema consolidation, load file generation, encoding pipeline, export, and table upload creation — 67 passing unit tests
- ✅ **E-035 — Warehouse Replay**: Delivered full `warehouse/replay/` package with replay handler, archived event retriever, Gateway `X-Warehouse-Replay` header, Processor warehouse-only routing, and backend-config extension — 58 passing unit tests
- ✅ **100% Compilation Success**: All packages compile cleanly with `go build ./...` and `go vet`
- ✅ **823+ Passing Tests**: All in-scope package tests pass with zero failures
- ✅ **Zero Linting Issues**: golangci-lint v2.9.0 reports 0 issues across all changes
- ✅ **Full Cross-Cutting Delivery**: Protobuf/gRPC extensions, configuration, SQL migrations (000042–000045), documentation, gap report updates

### 1.4 Critical Unresolved Issues

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| E-031 integration tests require real cloud connector credentials (Snowflake, BigQuery, Redshift, Delta Lake, Azure Synapse) | Cannot validate idempotent sync against production-like cloud warehouses | Human Developer | 1–2 days |
| End-to-end replay pipeline not tested with real archiver data | Replay feature validated with mocks only; real-data flow untested | Human Developer | 1–2 days |
| Pre-existing gateway/webhook iterable test failure | Unrelated to Sprint 7–9 but present in CI | Human Developer | 1 day |

### 1.5 Access Issues

| System/Resource | Type of Access | Issue Description | Resolution Status | Owner |
|-----------------|---------------|-------------------|-------------------|-------|
| Snowflake | Warehouse credentials | Integration tests (E-031) need Snowflake account, role, warehouse | Unresolved — requires secrets configuration | Human Developer |
| BigQuery | Service account JSON | Integration tests (E-031) need GCP service account with BQ access | Unresolved — requires secrets configuration | Human Developer |
| Redshift | Cluster credentials | Integration tests (E-031) need Redshift endpoint, user, database | Unresolved — requires secrets configuration | Human Developer |
| Delta Lake / Databricks | Workspace token | Integration tests (E-031) need Databricks SQL warehouse access | Unresolved — requires secrets configuration | Human Developer |
| Azure Synapse | Connection string | Integration tests (E-031) need Synapse workspace credentials | Unresolved — requires secrets configuration | Human Developer |
| AWS S3/MinIO | Object storage credentials | Backfill and replay staging file retrieval | Unresolved — requires secrets configuration | Human Developer |

### 1.6 Recommended Next Steps

1. **[High]** Configure warehouse connector credentials in CI/CD environment and validate E-031 integration tests against real cloud instances
2. **[High]** Execute end-to-end integration testing of the backfill and replay pipelines with a staging environment and real archiver data
3. **[High]** Set up environment variables and secrets for all new `Warehouse.backfill.*`, `Warehouse.healthMonitor.*`, `Warehouse.selectiveSync.*`, `Warehouse.replay.*` configuration keys
4. **[Medium]** Provision Grafana dashboards consuming the new Prometheus metrics (`warehouse_sync_duration_seconds`, `warehouse_sync_rows_total`, `warehouse_sync_errors_total`, `warehouse_sync_status`, `warehouse_schema_changes_total`)
5. **[Medium]** Conduct security review of new API endpoints (`POST /v1/warehouse/backfill`, `GET /v1/warehouse/health`, `PUT /v1/warehouse/selective-sync`, `POST /v1/warehouse/replay`) and implement rate limiting

---

## 2. Project Hours Breakdown

### 2.1 Completed Work Detail

| Component | Hours | Description |
|-----------|-------|-------------|
| E-031: Idempotent Sync Test Suite | 26 | 10 connector-specific integration test files (Snowflake, BigQuery, Redshift, ClickHouse, Delta Lake, PostgreSQL, MSSQL, Azure Synapse, Datalake), master test suite, canonical fixtures, staging file helper |
| E-032: Backfill Service Package | 34 | `warehouse/backfill/` — service orchestrator (678 LOC), HTTP handler, repository, model, config, SQL migration 000042, state machine extension, archiver integration, 68 passing tests |
| E-033: Health Monitoring Package | 32 | `warehouse/healthmonitor/` — monitor (218 LOC), alerting (370 LOC), metrics (224 LOC), repository (582 LOC), handler, model, config, SQL migrations 000044–000045, gRPC RPCs, upload pipeline instrumentation, 55 passing tests |
| E-034: Selective Sync Package | 30 | `warehouse/selectivesync/` — service (328 LOC), handler, repository, model, config, SQL migration 000043, pipeline integration across schema, encoding, loadfiles, 6 state handlers, backend-config parsing, 67 passing tests |
| E-035: Warehouse Replay Package | 28 | `warehouse/replay/` — handler (700 LOC), retriever (324 LOC), file downloader, gateway client, model, config, gateway/processor/backend-config modifications, 58 passing tests |
| Cross-Cutting: Proto & gRPC | 5 | Protobuf schema extension (78 new lines in .proto), warehouse.pb.go and warehouse_grpc.pb.go regeneration with 10 new message types |
| Cross-Cutting: Configuration | 3 | config.yaml (29 new parameters), docker.env (36 new lines), app.go wiring (283 new lines) |
| Cross-Cutting: Documentation | 6 | 4 feature docs (backfill, health-monitoring, selective-sync, replay — 1,822 LOC total), gap report updates, README update |
| Cross-Cutting: Test Extensions | 4 | Extended existing test suites — schema_test.go (+269 lines), encoding_test.go (+361 lines), http_test.go (+726 lines), grpc_test.go (+220 lines), state_test.go (+173 lines), replay_types_test.go (+174 lines) |
| Bug Fixes & Validation | 8 | 13 fix commits resolving QA findings (security headers, null byte validation, TOCTOU race, bounded queries, MSSQL date format, replay data-flow bug, migration schema drift) |
| **Total Completed** | **176** | |

### 2.2 Remaining Work Detail

| Category | Hours | Priority |
|----------|-------|----------|
| E-031: Real Connector Integration Testing | 4 | High |
| End-to-End Pipeline Integration Testing | 6 | High |
| Environment & Credential Configuration | 3 | High |
| Production Deployment Configuration | 4 | Medium |
| Load Testing & Performance Validation | 6 | Medium |
| Security Review & Hardening | 3 | Medium |
| Monitoring & Alerting Infrastructure | 3 | Low |
| Documentation Finalization | 1 | Low |
| **Total Remaining** | **30** | |

### 2.3 Hours Verification

- Section 2.1 Total (Completed): **176 hours**
- Section 2.2 Total (Remaining): **30 hours**
- Sum: 176 + 30 = **206 hours** ✓ (matches Section 1.2 Total Project Hours)

---

## 3. Test Results

All test data below originates from Blitzy's autonomous validation execution during the implementation and final validation phases.

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|--------------|-----------|-------------|--------|--------|------------|-------|
| Unit — warehouse/backfill | testify/require, t.Run | 68 | 68 | 0 | ~85% | Service, handler, repository tests |
| Unit — warehouse/healthmonitor | testify/require, t.Run | 55 | 55 | 0 | ~82% | Monitor, handler, alerting tests |
| Unit — warehouse/selectivesync | testify/require, t.Run | 67 | 67 | 0 | ~88% | Service, handler, repository tests |
| Unit — warehouse/replay | testify/require, t.Run | 58 | 58 | 0 | ~80% | Handler, retriever tests |
| Unit — warehouse/api | testify/require, t.Run | 190 | 190 | 0 | ~75% | HTTP + gRPC endpoint tests (includes new Sprint 7–9 endpoints) |
| Unit — warehouse/router | testify/require, t.Run | 217 | 217 | 0 | ~70% | State machine, scheduling, upload, tracker tests |
| Unit — warehouse/schema | testify/require, t.Run | 44 | 44 | 0 | ~90% | Schema consolidation with selective sync filtering |
| Unit — warehouse/encoding | testify/require, t.Run | 16 | 16 | 0 | ~85% | Column exclusion during event encoding |
| Unit — warehouse/bcm | testify/require, t.Run | 16 | 16 | 0 | ~80% | Backend-config parsing with selective sync |
| Unit — warehouse/archive | testify/require, t.Run | 6 | 6 | 0 | ~75% | Archiver query extensions |
| Unit — archiver | testify/require, t.Run | 3 | 3 | 0 | ~70% | Archiver storage operations |
| Unit — backend-config | testify/require, t.Run | 83 | 83 | 0 | ~80% | Replay types WarehouseOnly extension |
| Compilation — go build | go build ./... | 1 | 1 | 0 | N/A | All packages compile without errors |
| Static Analysis — go vet | go vet ./warehouse/... | 1 | 1 | 0 | N/A | Zero diagnostics |
| Linting — golangci-lint | golangci-lint v2.9.0 | 1 | 1 | 0 | N/A | Zero issues with strict depguard/forbidigo rules |
| **Totals** | | **826** | **826** | **0** | | |

**Pre-existing out-of-scope failures (NOT caused by agent changes):**
- `gateway/webhook/integration_test.go:232` — TestIntegrationWebhook/iterable/test-1: expects 400 gets 200 (zero agent changes to this file)
- `marketo-bulk-upload/utils_test.go` — TestReadJobsFromFile/No_read_permissions: root user bypasses OS permission check (zero agent changes to this file)

---

## 4. Runtime Validation & UI Verification

### Runtime Health

- ✅ **Binary Build**: Production binary builds successfully (`go build -a -installsuffix cgo -ldflags="-s -w"` produces 138MB binary)
- ✅ **Module Verification**: `go mod verify` — all modules verified, `go mod tidy` — no changes needed
- ✅ **Compilation**: `go build ./...` completes with zero errors across all packages
- ✅ **Static Analysis**: `go vet` clean across all modified packages (warehouse/*, processor/*, gateway/*, backend-config/*, archiver/*)
- ✅ **Git Status**: Working tree clean, no uncommitted changes

### API Endpoint Verification

- ✅ `POST /v1/warehouse/backfill` — Handler registered, request validation tested (date ranges, source/dest ID, concurrent limits)
- ✅ `GET /v1/warehouse/backfill/{jobID}` — Handler registered, status retrieval tested
- ✅ `GET /v1/warehouse/health` — Handler registered, health summary JSON response tested
- ✅ `GET /v1/warehouse/health/{sourceID}/{destID}` — Handler registered, per-pair health tested
- ✅ `PUT /v1/warehouse/selective-sync` — Handler registered, config upsert tested
- ✅ `GET /v1/warehouse/selective-sync/{sourceID}/{destID}` — Handler registered, config retrieval tested
- ✅ `POST /v1/warehouse/replay` — Handler registered, replay trigger tested
- ✅ `GET /v1/warehouse/replay/{jobID}` — Handler registered, status retrieval tested

### gRPC Verification

- ✅ `GetSyncHealth` RPC — Implemented and tested with workspace-scoped access control
- ✅ `GetHealthSummary` RPC — Implemented and tested with source filtering

### Pipeline Integration Verification

- ✅ **State Machine Extension**: Backfill state transitions registered; backward compatibility preserved (all existing state transitions unchanged)
- ✅ **Selective Sync Pipeline**: Table/column exclusion verified through schema consolidation → load file generation → encoding → export → table upload creation
- ✅ **Processor Routing**: `WarehouseOnly` flag detection and warehouse-targeted routing tested in processor
- ✅ **Gateway Replay Header**: `X-Warehouse-Replay` header case-insensitive detection tested

### UI Verification

This project is entirely backend/API-driven. No frontend UI components were in scope. All interactions occur through REST API, gRPC, Prometheus metrics, and backend-config.

---

## 5. Compliance & Quality Review

| Compliance Area | Requirement | Status | Notes |
|----------------|-------------|--------|-------|
| JSON Serialization | Use `jsonrs` exclusively (depguard rule) | ✅ Pass | All new packages use `github.com/rudderlabs/rudder-go-kit/jsonrs`; zero `encoding/json` imports in new code |
| Test Patterns | Table-driven tests with `t.Run()` and `testify/require` | ✅ Pass | All 248 new test cases follow table-driven subtests |
| Configuration Pattern | `config.GetReloadable*Var()` pattern | ✅ Pass | All new config keys use reloadable config variables |
| Stats Emission | `statsFactory.NewTaggedStat()` with standard tags | ✅ Pass | All new metrics include `module`, `workspaceId`, `destID`, `destType`, `sourceID`, `sourceType` tags |
| Repository Pattern | Struct with `*sqlquerywrapper.DB`, typed models | ✅ Pass | backfill, healthmonitor, selectivesync repositories follow established patterns |
| Error Handling | Categorized error types from model/upload.go | ✅ Pass | New error categories added for backfill, replay, health monitoring |
| Context Propagation | `context.Context` as first parameter | ✅ Pass | All long-running operations accept and respect context cancellation |
| HTTP Handler Pattern | Chi router middleware chain | ✅ Pass | All new endpoints use StatMiddleware, structured error responses |
| Backward Compatibility — API | Existing endpoints unchanged | ✅ Pass | All existing `/v1/warehouse/*` endpoints function without modification |
| Backward Compatibility — State Machine | Existing uploads unaffected | ✅ Pass | Backfill state only entered when `BackfillJobID` is non-nil; existing transitions preserved |
| Backward Compatibility — Schema | Additive migrations only | ✅ Pass | 4 new migrations add tables/columns; no drops, renames, or type changes |
| Backward Compatibility — Config | Safe defaults | ✅ Pass | Backfill, selective sync, replay default to `enabled: false`; health monitoring defaults to `enabled: true` |
| Backward Compatibility — Backend-Config | Graceful missing config | ✅ Pass | Absent `selectiveSync` block defaults to no exclusions |
| Security — Replay Spoofing | `X-Warehouse-Replay` header validation | ✅ Pass | Header only accepted from internal replay handler; spoofing mitigation implemented |
| Security — Null Byte Validation | Input sanitization | ✅ Pass | Null byte validation added to backfill/replay request inputs |
| QA Fixes Applied | TOCTOU race, bounded queries, batch purge | ✅ Pass | 13 fix commits resolved security, performance, and correctness findings |
| Linting | golangci-lint v2.9.0 zero issues | ✅ Pass | Strict rules including depguard, forbidigo enforced |

---

## 6. Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| E-031 integration tests cannot validate cloud connectors (Snowflake, BigQuery, Redshift, Delta Lake, Azure Synapse) without credentials | Integration | High | High | Test code is complete; requires cloud credentials in CI/CD secrets. Local connectors (PostgreSQL, ClickHouse, MSSQL) can run with Docker. | Open |
| Replay pipeline untested with real archived event data | Technical | High | Medium | Unit tests and mock-based integration tests pass; requires staging environment with real archiver output for end-to-end validation. | Open |
| Backfill archiver integration depends on staging file storage availability | Operational | Medium | Medium | Archiver `ListArchivedStagingFiles` and `QueryArchivedEvents` methods are implemented; require valid object storage credentials at runtime. | Open |
| Health monitoring overhead at high upload volume | Technical | Medium | Low | Collection interval configurable (`Warehouse.healthMonitor.collectionIntervalSeconds`); bounded queries with LIMIT prevent unbounded scans. QA fix applied for bounded queries. | Mitigated |
| Selective sync config cache staleness | Technical | Low | Low | Cache TTL configurable via `Warehouse.selectiveSync.cacheRefreshMinutes` (default 5 min). Backend-config subscription provides near-real-time updates. | Mitigated |
| New API endpoints lack rate limiting | Security | Medium | Medium | New endpoints inherit existing Chi middleware auth chain but lack explicit rate limiting. Recommend adding rate limits before production deployment. | Open |
| SQL migration ordering conflicts if other branches add migrations 000042+ | Operational | Medium | Low | Coordinate migration numbering with other active development branches before merge. | Open |
| Proto/gRPC changes may conflict with upstream protobuf regeneration | Technical | Low | Low | Proto files manually extended; verify with `protoc` regeneration if upstream changes occur. | Open |
| Pre-existing test failures in gateway/webhook may confuse CI reporting | Operational | Low | High | Document as pre-existing; these failures are in unmodified files and predate Sprint 7–9 work. | Documented |

---

## 7. Visual Project Status

```mermaid
pie title Project Hours Breakdown
    "Completed Work" : 176
    "Remaining Work" : 30
```

### Hours by Epic (Completed)

| Epic | Completed Hours | % of Total Completed |
|------|----------------|---------------------|
| E-031: Idempotent Sync | 26 | 14.8% |
| E-032: Backfill | 34 | 19.3% |
| E-033: Health Monitoring | 32 | 18.2% |
| E-034: Selective Sync | 30 | 17.0% |
| E-035: Replay | 28 | 15.9% |
| Cross-Cutting | 18 | 10.2% |
| Bug Fixes & Validation | 8 | 4.5% |

### Remaining Hours by Category

| Category | Hours |
|----------|-------|
| Real Connector Integration Testing | 4 |
| End-to-End Pipeline Testing | 6 |
| Environment & Credential Config | 3 |
| Production Deployment Config | 4 |
| Load Testing & Performance | 6 |
| Security Review & Hardening | 3 |
| Monitoring & Alerting Infra | 3 |
| Documentation Finalization | 1 |
| **Total Remaining** | **30** |

---

## 8. Summary & Recommendations

### Achievements

The Sprint 7–9 Warehouse Feature Enhancement is **85.4% complete** (176 hours completed out of 206 total project hours). All five epics (E-031 through E-035) have been fully implemented with production-grade code, comprehensive test coverage, and thorough documentation. The implementation delivers:

- **35,013 lines of new code** across 117 files (60 created, 57 modified)
- **248 new unit tests** in four new packages with 100% pass rate
- **823+ total passing tests** across all affected packages
- **Zero compilation errors**, zero linting issues, zero `go vet` diagnostics
- **Full backward compatibility** — all existing warehouse configurations, API contracts, and state machine transitions preserved
- **4 SQL migrations** (000042–000045) extending the warehouse schema with backfill tracking, selective sync config, and health monitoring tables
- **Protobuf/gRPC extensions** with 10 new message types for backfill and health monitoring RPCs

### Remaining Gaps

The remaining 30 hours (14.6%) consist primarily of path-to-production activities that require infrastructure access unavailable to autonomous agents:

1. **Cloud connector credential configuration** for E-031 integration test validation (4h)
2. **End-to-end pipeline testing** with real archiver data in a staging environment (6h)
3. **Production deployment configuration** including Kubernetes/Helm chart updates (4h)
4. **Load testing** to validate performance under production-scale workloads (6h)

### Production Readiness Assessment

The codebase is **production-ready from a code quality perspective**. All implemented features compile, pass tests, and follow established architectural conventions. The path to production deployment requires human-driven infrastructure configuration (credentials, deployment manifests, monitoring dashboards) and end-to-end validation with real cloud warehouse instances.

### Recommendations

1. **Prioritize credential setup** — Configuring cloud warehouse credentials is the critical path to unblocking E-031 validation and backfill/replay integration testing
2. **Enable features incrementally** — Backfill, selective sync, and replay default to `enabled: false`; enable one at a time with monitoring
3. **Add rate limiting** — New API endpoints should have rate limits configured before production exposure
4. **Coordinate migration numbering** — Verify SQL migration numbers (000042–000045) do not conflict with other active branches before merge

---

## 9. Development Guide

### System Prerequisites

| Software | Version | Purpose |
|----------|---------|---------|
| Go | 1.26.0 | Runtime and build toolchain |
| PostgreSQL | 15+ | Warehouse metadata storage (wh_uploads, wh_staging_files, etc.) |
| Docker | 24+ | Integration test containers (PostgreSQL, ClickHouse, MSSQL, MinIO) |
| Git | 2.30+ | Version control |
| golangci-lint | v2.9.0 | Code quality enforcement |

### Environment Setup

```bash
# Clone and checkout the branch
git clone https://github.com/rudderlabs/rudder-server.git
cd rudder-server
git checkout blitzy-23cec72d-1996-489e-8d5f-bcd3c6f98c15

# Verify Go version
go version
# Expected: go version go1.26.0 linux/amd64

# Verify module integrity
go mod verify
# Expected: all modules verified

# Set up environment variables (copy template and edit)
cp build/docker.env .env
# Edit .env with your warehouse credentials:
#   RUDDER_WAREHOUSE_BACKFILL_ENABLED=false
#   RUDDER_WAREHOUSE_HEALTH_MONITOR_ENABLED=true
#   RUDDER_WAREHOUSE_SELECTIVE_SYNC_ENABLED=false
#   RUDDER_WAREHOUSE_REPLAY_ENABLED=false
```

### Dependency Installation

```bash
# Download Go module dependencies
go mod download

# Verify all packages compile
go build ./...
# Expected: no output (success)

# Run static analysis
go vet ./warehouse/...
# Expected: no output (success)
```

### Running Tests

```bash
# Run all new package tests (Sprint 7-9)
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
SLOW=0 go test -count=1 -timeout 5m -short ./warehouse/backfill/...
SLOW=0 go test -count=1 -timeout 5m -short ./warehouse/healthmonitor/...
SLOW=0 go test -count=1 -timeout 5m -short ./warehouse/selectivesync/...
SLOW=0 go test -count=1 -timeout 5m -short ./warehouse/replay/...

# Run all warehouse tests
SLOW=0 go test -count=1 -timeout 15m -short ./warehouse/...

# Run full affected test suite
SLOW=0 go test -count=1 -timeout 15m -short \
  ./warehouse/... \
  ./processor/... \
  ./backend-config/... \
  ./archiver/... \
  ./gateway/...

# Run linting
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.9.0 run -v
```

### Building the Application

```bash
# Development build
go build -o rudder-server .

# Production build (smaller binary, stripped debug info)
go build -a -installsuffix cgo -ldflags="-s -w" -o rudder-server .
# Expected: ~138MB binary

# Verify binary
./rudder-server --version
```

### Application Startup

```bash
# Start PostgreSQL (required for warehouse metadata)
docker compose up -d db

# Start the server
./rudder-server
# Default ports:
#   8080 — Gateway HTTP
#   8082 — Warehouse API (HTTP + gRPC)
```

### Verification Steps

```bash
# Verify warehouse health endpoint
curl -s http://localhost:8082/v1/warehouse/health | python3 -m json.tool

# Verify backfill endpoint (requires enabled: true in config)
curl -X POST http://localhost:8082/v1/warehouse/backfill \
  -H "Content-Type: application/json" \
  -d '{"sourceID":"src_123","destinationID":"dst_456","startDate":"2025-01-01T00:00:00Z","endDate":"2025-01-31T23:59:59Z"}'

# Verify selective sync endpoint
curl -X PUT http://localhost:8082/v1/warehouse/selective-sync \
  -H "Content-Type: application/json" \
  -d '{"sourceID":"src_123","destinationID":"dst_456","workspaceID":"ws_789","excludedTables":["users"],"excludedColumns":{"tracks":["ip"]}}'
```

### Troubleshooting

| Issue | Resolution |
|-------|-----------|
| `warehouse/internal/repo` tests timeout at 5m | Increase timeout: `go test -timeout 10m ./warehouse/internal/repo/...` — these tests perform extensive DB operations and need ~5–6 minutes |
| `gateway/webhook` iterable test fails | Pre-existing issue; not caused by Sprint 7–9 changes. Safe to ignore or skip with `-run "^(?!.*iterable)"` |
| Module download slow | Run `go mod download` once before testing to pre-cache dependencies |
| Docker not available for integration tests | Start Docker daemon first: `sudo systemctl start docker` |

---

## 10. Appendices

### A. Command Reference

| Command | Purpose |
|---------|---------|
| `go build ./...` | Compile all packages |
| `go build -a -installsuffix cgo -ldflags="-s -w"` | Build production binary |
| `go vet ./warehouse/...` | Static analysis on warehouse packages |
| `SLOW=0 go test -count=1 -timeout 15m -short ./warehouse/...` | Run all warehouse tests |
| `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.9.0 run -v` | Lint all changes |
| `go mod verify` | Verify module checksums |
| `go mod tidy` | Clean up module dependencies |

### B. Port Reference

| Port | Service | Protocol |
|------|---------|----------|
| 8080 | Gateway HTTP API | HTTP |
| 8082 | Warehouse API (HTTP + gRPC) | HTTP/gRPC |
| 5432 | PostgreSQL (metadata) | TCP |

### C. Key File Locations

| Category | Path | Description |
|----------|------|-------------|
| Backfill service | `warehouse/backfill/` | Backfill orchestrator, handler, repository, model, config |
| Health monitoring | `warehouse/healthmonitor/` | Monitor, alerting, metrics, repository, handler, model |
| Selective sync | `warehouse/selectivesync/` | Service, handler, repository, model, config |
| Replay handler | `warehouse/replay/` | Handler, retriever, file downloader, gateway client |
| Integration tests | `integration_test/warehouse/` | Idempotent sync (9 connectors), backfill, selective sync, replay |
| SQL migrations | `sql/migrations/warehouse/000042–000045` | Backfill tracking, selective sync config, health monitoring |
| Protobuf definitions | `proto/warehouse/warehouse.proto` | gRPC service with new backfill and health RPCs |
| Configuration | `config/config.yaml` | New parameters under `Warehouse.backfill.*`, `Warehouse.healthMonitor.*`, `Warehouse.selectiveSync.*`, `Warehouse.replay.*` |
| Feature documentation | `docs/warehouse/` | backfill.md, health-monitoring.md, selective-sync.md, replay.md |

### D. Technology Versions

| Technology | Version | Source |
|------------|---------|--------|
| Go | 1.26.0 | `go.mod` |
| rudder-go-kit | v0.72.3 | `go.mod` |
| chi/v5 | v5.2.5 | `go.mod` |
| testify | v1.11.1 | `go.mod` |
| dockertest/v3 | v3.12.0 | `go.mod` |
| golangci-lint | v2.9.0 | CI configuration |
| golang-migrate/v4 | v4.18.3 | `go.mod` |
| PostgreSQL (target) | 15+ | Recommended |

### E. Environment Variable Reference

| Variable | Default | Description |
|----------|---------|-------------|
| `Warehouse.backfill.enabled` | `false` | Enable/disable backfill feature |
| `Warehouse.backfill.maxDateRangeDays` | `90` | Maximum backfill date range in days |
| `Warehouse.backfill.maxConcurrentJobs` | `3` | Maximum concurrent backfill jobs |
| `Warehouse.backfill.monitorIntervalSeconds` | `60` | Backfill job monitor polling interval |
| `Warehouse.healthMonitor.enabled` | `true` | Enable/disable health monitoring |
| `Warehouse.healthMonitor.collectionIntervalSeconds` | `60` | Health metrics collection interval |
| `Warehouse.healthMonitor.retentionDays` | `30` | Health record retention period |
| `Warehouse.selectiveSync.enabled` | `false` | Enable/disable selective sync |
| `Warehouse.selectiveSync.cacheRefreshMinutes` | `5` | Selective sync config cache TTL |
| `Warehouse.replay.enabled` | `false` | Enable/disable warehouse replay |
| `Warehouse.replay.maxConcurrentReplays` | `2` | Maximum concurrent replay jobs |
| `Warehouse.replay.batchSize` | `1000` | Replay event batch size |

### F. Developer Tools Guide

| Tool | Command | Purpose |
|------|---------|---------|
| Go test verbose | `go test -v -run "TestBackfillService" ./warehouse/backfill/...` | Run specific test with verbose output |
| Test coverage | `go test -coverprofile=cover.out ./warehouse/backfill/... && go tool cover -html=cover.out` | Generate HTML coverage report |
| Race detector | `go test -race ./warehouse/backfill/...` | Detect data races |
| CPU profiling | `go test -cpuprofile=cpu.out -bench=. ./warehouse/...` | CPU performance profiling |
| Module graph | `go mod graph \| grep backfill` | Inspect module dependency graph |

### G. Glossary

| Term | Definition |
|------|-----------|
| **Backfill** | Re-processing historical data for a specified date range through the warehouse pipeline |
| **Selective Sync** | Per-table and per-column filtering that excludes specific data from warehouse sync |
| **Warehouse Replay** | Re-processing archived events through the warehouse pipeline while bypassing real-time Router delivery |
| **Idempotent Sync** | Property ensuring that replaying/retrying sync operations produces identical warehouse state |
| **Health Monitor** | Subsystem tracking per-upload metrics (duration, row counts, errors) with Prometheus emission and alerting |
| **Staging Files** | Intermediate files containing event data prepared for warehouse loading |
| **Load Files** | Files generated from staging files in the format required by the target warehouse connector |
| **Upload State Machine** | 7-state (now 8 with backfill) linked list governing the warehouse upload lifecycle |
| **Backend-Config** | Configuration distribution system delivering workspace/destination settings via pub/sub |
| **wh_backfill_jobs** | New database table tracking backfill job metadata and lifecycle |
| **wh_selective_sync** | New database table storing per-source/destination selective sync exclusion rules |
| **wh_sync_health** | New database table persisting per-upload health metrics for historical analysis |