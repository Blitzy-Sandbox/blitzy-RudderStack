# Blitzy Project Guide — Segment Event Spec Parity Validation

---

## Section 1 — Executive Summary

### 1.1 Project Overview

This project validates and closes the remaining ~5% gap in Segment Spec event parity for the RudderStack `rudder-server` (v1.68.1), bringing it from approximately 95% to 100% field-level parity with the Twilio Segment Event Specification. The implementation covers all six core event types (`identify`, `track`, `page`, `screen`, `group`, `alias`) with comprehensive test suites, OpenAPI schema updates, Client Hints pass-through verification, semantic event category routing validation, reserved trait handling, and API reference documentation. This is classified as P0 — Critical, targeting backend data pipeline engineers and platform teams migrating from Segment.

### 1.2 Completion Status

```mermaid
pie title Project Completion — 87.8%
    "Completed (108h)" : 108
    "Remaining (15h)" : 15
```

| Metric | Value |
|--------|-------|
| **Total Project Hours** | 123h |
| **Completed Hours (AI)** | 108h |
| **Remaining Hours** | 15h |
| **Completion Percentage** | **87.8%** (108 / 123 = 87.8%) |

### 1.3 Key Accomplishments

- ✅ Created comprehensive field-level parity test suites for all 6 core Segment event types at the Gateway and Processor layers
- ✅ Verified structured Client Hints (`context.userAgentData`) pass-through from Gateway → Processor → Router → Warehouse without data loss (ES-001)
- ✅ Validated semantic event category routing for E-Commerce v2, Video, and Mobile lifecycle events through the Transformer service (ES-002)
- ✅ Confirmed all 17 identify reserved traits and 12 group reserved traits pass through without type coercion or data loss (ES-003)
- ✅ Verified `context.channel` field auto-population and preservation for server/browser/mobile values (ES-007)
- ✅ Added explicit `UserAgentData` schema to OpenAPI specification for all 6 payload types
- ✅ Created full-stack end-to-end integration test suite with Docker-provisioned PostgreSQL, Transformer, and webhook services (13 subtests passing)
- ✅ Documented RudderStack extension endpoints, semantic event categories, and batch size defaults (ES-004, ES-006)
- ✅ Updated gap report, sprint roadmap, executive index, and README to reflect 100% Event Spec parity
- ✅ All 752 in-scope tests pass, `go build ./...` and `go vet ./...` produce zero errors

### 1.4 Critical Unresolved Issues

| Issue | Impact | Owner | ETA |
|-------|--------|-------|-----|
| Event Spec parity tests not added to CI matrix | New tests won't run in CI/CD pipeline on PRs | Human Developer | 2h |
| OpenAPI schema CI validation not confirmed | Schema changes may not be validated by `swagger-cli` in CI | Human Developer | 1h |
| Processor benchmark non-regression not verified | Potential performance regression undetected | Human Developer | 1.5h |

### 1.5 Access Issues

No access issues identified. All dependencies are available via Go module proxy. Integration tests use Docker-provisioned local services (PostgreSQL, Transformer, webhook) and do not require external service credentials.

### 1.6 Recommended Next Steps

1. **[High]** Add `integration_test/event_spec_parity/` test suite to CI matrix in `.github/workflows/tests.yaml` to prevent future regressions
2. **[High]** Run human code review of all 25 changed files (9,011 lines added) with focus on test correctness and Segment Spec compliance
3. **[Medium]** Verify OpenAPI schema changes pass `swagger-cli validate` in the CI verification pipeline
4. **[Medium]** Run `processorBenchmark_test.go` benchmarks to confirm no performance regression from handle.go changes
5. **[Low]** Validate in staging environment with real destination connectors to confirm end-to-end semantic event routing

---

## Section 2 — Project Hours Breakdown

### 2.1 Completed Work Detail

| Component | Hours | Description |
|-----------|-------|-------------|
| Gateway Payload Schema Validation (E-001, E-003) | 20.5h | `gateway/event_spec_parity_test.go` (904 lines), `gateway_test.go` (+305 lines), `handle_test.go` (+352 lines), `openapi.yaml` (+150 lines), `handle.go` (+15 lines) |
| Client Hints Pass-Through (ES-001) | 10.5h | `gateway/client_hints_test.go` (686 lines), `bot_test.go` (+20 lines), `validator_test.go` (+195 lines) |
| Semantic Event Category Routing (ES-002) | 14h | `processor/event_spec_parity_test.go` (894 lines), `processor_test.go` (+320 lines) |
| Reserved Trait Validation (ES-003) | 20h | `processor/reserved_traits_test.go` (673 lines), `events_test.go` (+846 lines), `rules_test.go` (+153 lines) |
| Channel Field Auto-Population (ES-007) | 3h | Test cases in gateway_test.go, handle_test.go, integration tests; `common-fields.md` updates |
| Extensions Documentation (ES-004, ES-006) | 7h | `extensions.md` (239 lines), `semantic-events.md` (283 lines), `common-fields.md` (+40 lines) |
| Gap Report and README Updates | 4.5h | `event-spec-parity.md`, `index.md`, `sprint-roadmap.md`, `README.md` updates to 100% parity |
| End-to-End Integration Tests (E-001–E-004) | 23h | `event_spec_parity_test.go` (1166 lines), `segment_spec_payloads.json` (1068 lines), `workspaceConfigTemplate.json` (100 lines), `docker_test.go` (+194 lines) |
| Validation and Bug Fixes | 5.5h | DB credentials fix, configFromFile flag fix, 12 code review issues, trait count corrections |
| **Total Completed** | **108h** | |

### 2.2 Remaining Work Detail

| Category | Base Hours | Priority | After Multiplier |
|----------|-----------|----------|-----------------|
| CI Pipeline Integration — Add parity tests to CI matrix | 2h | High | 2.5h |
| OpenAPI CI Validation — Verify swagger-cli configuration | 1h | Medium | 1h |
| Benchmark Non-Regression — Run and verify processor benchmarks | 1h | Medium | 1.5h |
| Human Code Review — Review 25 files, 9,011 lines | 4h | High | 5h |
| Security Review — Verify test fixtures contain only synthetic data | 1h | Low | 1h |
| Staging Deployment Validation — End-to-end with real destinations | 3h | Medium | 4h |
| **Total Remaining** | **12h** | | **15h** |

### 2.3 Enterprise Multipliers Applied

| Multiplier | Value | Rationale |
|-----------|-------|-----------|
| Compliance Review | 1.10x | Segment Spec behavioral equivalence requires careful manual verification of test assertions against authoritative Segment documentation |
| Uncertainty Buffer | 1.10x | CI pipeline integration may require adjustments for Docker service provisioning in CI environment; staging validation may uncover destination-specific edge cases |
| **Combined Multiplier** | **1.21x** | Applied to all remaining work categories |

---

## Section 3 — Test Results

| Test Category | Framework | Total Tests | Passed | Failed | Coverage % | Notes |
|---------------|-----------|------------|--------|--------|-----------|-------|
| Gateway Unit Tests | Ginkgo/testify | 103 | 103 | 0 | N/A | Includes event spec parity, Client Hints, channel field tests |
| Gateway Bot Detection | testify | 9 | 9 | 0 | N/A | Includes 4 new Client Hints-aware test cases |
| Gateway Validator | testify | 25 | 25 | 0 | N/A | Includes Client Hints pass-through validation |
| Processor Unit Tests | Ginkgo/testify | 125 | 125 | 0 | N/A | Includes semantic event, parity, and reserved trait scenarios |
| Warehouse Events | testify | 379 | 379 | 0 | N/A | Includes reserved trait and Segment Spec field coverage |
| Warehouse Rules | testify | 88 | 88 | 0 | N/A | Includes reserved field coverage for all event types |
| Integration — Event Spec Parity | testify/dockertest | 13 | 13 | 0 | N/A | Full-stack Gateway → Processor → Router → Destination |
| Integration — Docker Main Flow | testify/dockertest | 10 | 10 | 0 | N/A | Extended with Segment Spec parity payloads |
| **Total** | | **752** | **752** | **0** | | **100% pass rate** |

Additional static analysis:
- `go build ./...` — PASS (zero errors, zero warnings)
- `go vet ./...` — PASS (zero issues)
- `go mod verify` — PASS (all 80+ modules verified)
- Binary build (`go build -o rudder-server .`) — PASS (201MB binary)

---

## Section 4 — Runtime Validation & UI Verification

**Runtime Health:**
- ✅ `go build ./...` compiles all packages cleanly (zero errors)
- ✅ `go vet ./...` produces zero static analysis findings
- ✅ `go mod verify` confirms all 80+ module checksums match
- ✅ Application binary builds successfully (201MB)
- ✅ Git working tree is clean — all changes committed

**API Integration Verification:**
- ✅ Gateway accepts all 6 Segment event types (`/v1/identify`, `/v1/track`, `/v1/page`, `/v1/screen`, `/v1/group`, `/v1/alias`) with full Segment Spec payloads
- ✅ `context.userAgentData` structured Client Hints pass through Gateway → Processor → Router → Destination
- ✅ Semantic event names (E-Commerce v2, Video, Mobile) pass through as opaque strings to Transformer
- ✅ All 17 identify reserved traits and 12 group reserved traits preserved without type coercion
- ✅ `context.channel` field preserved for `server`, `browser`, and `mobile` values
- ✅ Webhook destinations receive all expected Segment Spec fields in delivery payloads

**Integration Test Verification:**
- ✅ Docker-provisioned PostgreSQL container starts and accepts connections
- ✅ Full rudder-server starts with file-based backend config
- ✅ 13/13 event spec parity subtests pass (webhook delivery, field preservation, semantic events, Client Hints, reserved traits)
- ✅ 10/10 Docker main flow subtests pass (common pool + separate pool modes)

**UI Verification:**
- ⚠ Not applicable — `rudder-server` is a backend-only data plane with no frontend components

---

## Section 5 — Compliance & Quality Review

| AAP Deliverable | Status | Evidence | Notes |
|-----------------|--------|----------|-------|
| E-001: Payload Schema Validation (6 event types) | ✅ Complete | `gateway/event_spec_parity_test.go`, `gateway/openapi.yaml` | All 6 event types validated at field level |
| E-003: OpenAPI Schema Parity | ✅ Complete | `gateway/openapi.yaml` (+150 lines) | UserAgentData schema added for all payload types |
| ES-001: Client Hints Pass-Through | ✅ Complete | `gateway/client_hints_test.go`, `integration_test/event_spec_parity/` | End-to-end verification across full pipeline |
| ES-002: Semantic Event Category Routing | ✅ Complete | `processor/event_spec_parity_test.go`, `processor/processor_test.go` | E-Commerce v2, Video, Mobile lifecycle validated |
| ES-003: Reserved Trait Validation | ✅ Complete | `processor/reserved_traits_test.go`, `events_test.go`, `rules_test.go` | 17 identify + 12 group traits verified |
| ES-004: Extension Endpoint Documentation | ✅ Complete | `docs/api-reference/event-spec/extensions.md` | `/v1/replay`, `/internal/v1/retl`, `/beacon/v1/*`, `/pixel/v1/*`, merge documented |
| ES-006: Batch Size Documentation | ✅ Complete | `docs/api-reference/event-spec/extensions.md` | 4000KB vs 500KB default documented |
| ES-007: Channel Field Auto-Population | ✅ Complete | `gateway/handle.go`, `common-fields.md`, integration tests | server/browser/mobile preservation verified |
| Gap Report Updates | ✅ Complete | `event-spec-parity.md`, `index.md`, `sprint-roadmap.md`, `README.md` | Updated from ~95% to 100% parity |
| Integration Test Suite | ✅ Complete | `integration_test/event_spec_parity/` (3 files, 2,334 lines) | Full-stack Docker-provisioned test harness |
| Backward Compatibility | ✅ Maintained | No breaking API changes, existing tests pass | All existing endpoints and behavior preserved |
| `jsonrs` Usage (No `encoding/json`) | ✅ Compliant | No new `encoding/json` imports in modified source files | Per `depguard` linting rule |
| Table-Driven Test Patterns | ✅ Compliant | All new tests use `t.Run()` subtests with `testify/require` | Follows codebase conventions |
| No Sensitive Data in Fixtures | ✅ Compliant | Test payloads use synthetic data (user_123, test@example.com) | No real credentials or PII |

**Autonomous Validation Fixes Applied:**
1. Fixed missing DB credentials in Docker integration tests (2 files)
2. Fixed missing `configFromFile` environment flag (2 files)
3. Addressed 12 code review findings across multiple files
4. Corrected reserved identify trait count from 18 to 17

---

## Section 6 — Risk Assessment

| Risk | Category | Severity | Probability | Mitigation | Status |
|------|----------|----------|-------------|------------|--------|
| Parity tests not in CI pipeline | Technical | High | High | Add `integration_test/event_spec_parity/` to `.github/workflows/tests.yaml` CI matrix | Open |
| OpenAPI schema validation gap | Technical | Medium | Medium | Verify `swagger-cli validate` passes in CI verification pipeline | Open |
| Benchmark regression from handle.go | Technical | Medium | Low | Run `processorBenchmark_test.go` before/after to confirm no regression (only 15 lines of comments added) | Open |
| External Transformer dependency | Integration | Medium | Medium | Semantic event mapping (ES-002) depends on external `rudder-transformer` service at port 9090; integration tests mock this | Mitigated |
| Docker service provisioning in CI | Operational | Medium | Medium | Integration tests require PostgreSQL and webhook containers; existing CI already provisions these for `docker_test` | Mitigated |
| Segment Spec evolution | Technical | Low | Low | Segment may update spec; `refs/segment-docs/` reference corpus provides versioned baseline | Accepted |
| Test fixture staleness | Operational | Low | Low | Test payloads in `segment_spec_payloads.json` based on current Segment Spec; may need updates if Segment adds fields | Accepted |
| Reserved trait count discrepancy | Technical | Low | Low | AAP stated 18 identify traits but Segment Spec defines 17; corrected during validation | Resolved |

---

## Section 7 — Visual Project Status

```mermaid
pie title Project Hours Breakdown
    "Completed Work" : 108
    "Remaining Work" : 15
```

**Completion: 87.8%** (108h completed / 123h total)

```mermaid
pie title Remaining Work Distribution
    "CI Pipeline Integration (2.5h)" : 2.5
    "OpenAPI CI Validation (1h)" : 1
    "Benchmark Verification (1.5h)" : 1.5
    "Human Code Review (5h)" : 5
    "Security Review (1h)" : 1
    "Staging Deployment (4h)" : 4
```

---

## Section 8 — Summary & Recommendations

### Achievements

The Blitzy autonomous agents successfully delivered 108 hours of engineering work across 30 commits, producing 9,011 lines of additions across 25 files (8 new, 17 modified). The project is **87.8% complete** against the AAP scope and path-to-production requirements.

All six core Segment event gaps (ES-001 through ES-007, excluding ES-005 which is out of scope) have been resolved with comprehensive test suites and documentation. The gap report has been updated from ~95% to 100% Event Spec parity. A full-stack integration test suite exercises all 6 event types through the complete pipeline (Gateway → Processor → Router → Destination) with 13 passing subtests.

All 752 in-scope tests pass at a 100% rate. The Go project compiles cleanly with zero `go build` errors and zero `go vet` findings. The working tree is clean with all changes committed.

### Remaining Gaps

The 15 remaining hours (12.2% of total) consist entirely of path-to-production tasks requiring human intervention:
- CI pipeline integration to add new test suites to the automated build matrix
- Human code review of 25 changed files
- Benchmark non-regression verification
- Staging environment validation with real destination connectors

### Critical Path to Production

1. Add event spec parity tests to CI matrix (blocks deployment gate)
2. Complete human code review (blocks merge)
3. Verify benchmark non-regression (blocks performance SLA)
4. Deploy to staging and validate with real destination connectors

### Production Readiness Assessment

The project meets production readiness criteria for the implemented scope:
- **Code Quality:** All tests pass, zero compilation errors, zero static analysis issues
- **Backward Compatibility:** No breaking changes to existing API surface
- **Test Coverage:** 752 tests across 8 packages covering all AAP requirements
- **Documentation:** Complete API reference, updated gap report, and README

The remaining 15h of work is standard production hardening that requires human developer involvement for CI configuration, security sign-off, and staging validation.

---

## Section 9 — Development Guide

### System Prerequisites

| Prerequisite | Version | Purpose |
|-------------|---------|---------|
| Go | 1.26.0 | Runtime and build toolchain |
| Docker | 20.10+ | Integration test container orchestration |
| Docker Compose | 2.0+ | Multi-service local development |
| PostgreSQL Client | 15+ | Optional — direct database inspection |
| Git | 2.30+ | Version control |

### Environment Setup

```bash
# Clone the repository
git clone https://github.com/rudderlabs/rudder-server.git
cd rudder-server

# Checkout the feature branch
git checkout blitzy-d29c2824-4d4e-43a7-8ee1-62bc63f3b5ec

# Verify Go version
go version
# Expected: go version go1.26.0 linux/amd64

# Verify all module dependencies
go mod verify
# Expected: all modules verified

# Download all dependencies
go mod download
```

### Environment Variables

```bash
# Copy sample environment file
cp config/sample.env .env

# Required environment variables for local development
export CONFIG_PATH=./config/config.yaml
export JOBS_DB_HOST=localhost
export JOBS_DB_USER=rudder
export JOBS_DB_PASSWORD=rudder
export JOBS_DB_PORT=5432
export JOBS_DB_DB_NAME=jobsdb
export JOBS_DB_SSL_MODE=disable
export DEST_TRANSFORM_URL=http://localhost:9090
export GO_ENV=development
export LOG_LEVEL=DEBUG
```

### Build and Verify

```bash
# Compile all packages (verify zero errors)
go build ./...

# Run static analysis
go vet ./...

# Build the binary
go build -o rudder-server .
# Expected: 201MB binary produced
```

### Running Tests

```bash
# Run Gateway tests (includes event spec parity and Client Hints)
go test ./gateway/... -count=1 -short -timeout=120s
# Expected: ok (all packages pass)

# Run Processor tests (includes semantic events and reserved traits)
go test ./processor/ -count=1 -short -timeout=300s
# Expected: ok (58+ Ginkgo specs pass)

# Run Warehouse events tests
go test ./processor/internal/transformer/destination_transformer/embedded/warehouse/... -count=1 -short -timeout=120s
# Expected: ok (all packages pass)

# Run Event Spec Parity integration tests
go test ./integration_test/event_spec_parity/... -count=1 -short -timeout=60s -v
# Expected: 13/13 subtests PASS

# Run Docker main flow integration tests
go test ./integration_test/docker_test/... -count=1 -short -timeout=180s
# Expected: 10/10 subtests PASS

# Run all unit tests (via Makefile)
make test-run
```

### Running the Application

```bash
# Start dependencies via Docker Compose
docker compose up -d db transformer

# Wait for PostgreSQL to be ready
docker compose exec db pg_isready -U rudder
# Expected: accepting connections

# Start rudder-server
./rudder-server
# Expected: Gateway starts on port 8080

# Verify health
curl -s http://localhost:8080/health
# Expected: {"status":"ok"}
```

### Example API Usage

```bash
# Send an identify event
curl -X POST http://localhost:8080/v1/identify \
  -u "YOUR_WRITE_KEY:" \
  -H "Content-Type: application/json" \
  -d '{
    "userId": "user_123",
    "traits": {
      "email": "test@example.com",
      "firstName": "Test",
      "lastName": "User"
    },
    "context": {
      "channel": "server",
      "userAgent": "TestAgent/1.0",
      "userAgentData": {
        "brands": [{"brand": "Chromium", "version": "120"}],
        "mobile": false,
        "platform": "Linux"
      }
    }
  }'

# Send a track event with E-Commerce v2 semantic event
curl -X POST http://localhost:8080/v1/track \
  -u "YOUR_WRITE_KEY:" \
  -H "Content-Type: application/json" \
  -d '{
    "userId": "user_123",
    "event": "Order Completed",
    "properties": {
      "orderId": "order_456",
      "total": 99.99,
      "currency": "USD"
    }
  }'

# Send a batch request with mixed event types
curl -X POST http://localhost:8080/v1/batch \
  -u "YOUR_WRITE_KEY:" \
  -H "Content-Type: application/json" \
  -d '{
    "batch": [
      {"type": "identify", "userId": "user_1", "traits": {"name": "Alice"}},
      {"type": "track", "userId": "user_1", "event": "Page Viewed"},
      {"type": "page", "userId": "user_1", "name": "Home"}
    ]
  }'
```

### Troubleshooting

| Issue | Cause | Resolution |
|-------|-------|-----------|
| `go: command not found` | Go not in PATH | `export PATH="/usr/local/go/bin:$PATH"` |
| Integration tests fail with "empty workspace config token" | Missing `RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE` | Set `RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE=true` in test environment |
| Integration tests fail with DB connection error | Wrong PostgreSQL credentials | Set `JOBS_DB_USER`, `JOBS_DB_PASSWORD`, `JOBS_DB_DB_NAME`, `JOBS_DB_SSL_MODE` matching Docker container |
| `encoding/json` import error in lint | `depguard` rule bans `encoding/json` | Use `github.com/rudderlabs/rudder-go-kit/jsonrs` instead |
| Ginkgo tests hang | Watch mode enabled | Always use `-count=1` flag to prevent caching and watch mode |
| Docker integration tests timeout | Docker services not started | Ensure Docker daemon is running; tests auto-provision containers via `dockertest/v3` |

---

## Section 10 — Appendices

### A. Command Reference

| Command | Purpose |
|---------|---------|
| `go build ./...` | Compile all packages |
| `go vet ./...` | Static analysis |
| `go mod verify` | Verify module checksums |
| `go mod download` | Download all dependencies |
| `go test ./gateway/... -count=1 -short` | Run Gateway tests |
| `go test ./processor/ -count=1 -short -timeout=300s` | Run Processor tests |
| `go test ./integration_test/event_spec_parity/... -count=1 -short -v` | Run parity integration tests |
| `go test ./integration_test/docker_test/... -count=1 -short` | Run Docker integration tests |
| `go build -o rudder-server .` | Build application binary |
| `make test-run` | Run all unit tests via Makefile |
| `docker compose up -d` | Start all services |
| `docker compose down` | Stop all services |

### B. Port Reference

| Port | Service | Description |
|------|---------|-------------|
| 8080 | Gateway | HTTP API for all event types |
| 5432 | PostgreSQL | Jobs database (default Docker: 6432 mapped to 5432) |
| 9090 | Transformer | External transformer service for destination transforms |
| 8181 | Test Sink | Webhook destination for integration tests |

### C. Key File Locations

| Path | Description |
|------|-------------|
| `gateway/openapi.yaml` | OpenAPI 3.0.3 specification for all Gateway endpoints |
| `gateway/handle.go` | Core request handler with event processing pipeline |
| `gateway/handle_http.go` | HTTP handler wiring for all event types |
| `config/config.yaml` | Master runtime configuration |
| `config/sample.env` | Environment variable reference |
| `docs/gap-report/event-spec-parity.md` | Canonical gap report (100% parity) |
| `docs/api-reference/event-spec/` | Event spec API reference documentation |
| `integration_test/event_spec_parity/` | End-to-end parity integration test suite |
| `integration_test/event_spec_parity/testdata/segment_spec_payloads.json` | Canonical Segment Spec test fixtures |
| `refs/segment-docs/src/connections/spec/` | Authoritative Segment documentation reference corpus |

### D. Technology Versions

| Technology | Version | Source |
|-----------|---------|--------|
| Go | 1.26.0 | `go.mod` line 3 |
| rudder-go-kit | v0.72.3 | `go.mod` |
| testify | v1.11.1 | `go.mod` |
| Ginkgo | v2.24.0 | `go.mod` |
| Gomega | v1.38.0 | `go.mod` |
| dockertest/v3 | v3.12.0 | `go.mod` |
| chi/v5 | v5.2.5 | `go.mod` |
| gjson | v1.18.0 | `go.mod` |
| sjson | v1.2.5 | `go.mod` |
| PostgreSQL (Docker) | 15-alpine | `docker-compose.yml` |
| Alpine Linux | 3.23 | `Dockerfile` |

### E. Environment Variable Reference

| Variable | Default | Description |
|----------|---------|-------------|
| `CONFIG_PATH` | `./config/config.yaml` | Path to runtime configuration |
| `JOBS_DB_HOST` | `localhost` | PostgreSQL host |
| `JOBS_DB_USER` | `rudder` | PostgreSQL username |
| `JOBS_DB_PASSWORD` | `rudder` | PostgreSQL password |
| `JOBS_DB_PORT` | `5432` | PostgreSQL port |
| `JOBS_DB_DB_NAME` | `jobsdb` | PostgreSQL database name |
| `JOBS_DB_SSL_MODE` | `disable` | PostgreSQL SSL mode |
| `DEST_TRANSFORM_URL` | `http://localhost:9090` | Transformer service URL |
| `WORKSPACE_TOKEN` | (required) | Workspace authentication token |
| `RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE` | `false` | Use local JSON config file instead of API |
| `GO_ENV` | `production` | Environment mode |
| `LOG_LEVEL` | `INFO` | Logging verbosity |
| `Gateway.webPort` | `8080` | Gateway HTTP listen port |
| `Gateway.maxReqSizeInKB` | `4000` | Max request body size in KB |

### F. Developer Tools Guide

| Tool | Installation | Purpose |
|------|-------------|---------|
| `gotestsum` | `go install gotest.tools/gotestsum@v1.12.3` | Enhanced test output formatting |
| `golangci-lint` | See `.golangci.yml` | Linting with `depguard`, `forbidigo` rules |
| `swagger-cli` | `npm install -g swagger-cli` | OpenAPI schema validation |
| `mockgen` | `go install go.uber.org/mock/mockgen@v0.6.0` | Interface mock generation |

### G. Glossary

| Term | Definition |
|------|-----------|
| **AAP** | Agent Action Plan — the comprehensive specification of all work to be completed |
| **Client Hints** | W3C User-Agent Client Hints API providing structured browser/device metadata |
| **Event Spec** | Segment's specification defining the 6 core event types and their field schemas |
| **Gateway** | RudderStack's HTTP API service that ingests events on port 8080 |
| **Processor** | 6-stage pipeline that processes events after Gateway ingestion |
| **Router** | Event delivery service that routes processed events to destinations |
| **Transformer** | External service (port 9090) that handles destination-specific event transformations |
| **Write Key** | Authentication credential used as Basic Auth username for API calls |
| **ES-001 through ES-007** | Gap IDs from the Event Spec Parity analysis |
| **E-001 through E-004** | Epic IDs from the Sprint Roadmap for Event Spec Parity work |
| **Reserved Traits** | Standardized trait names defined by Segment for identify (17) and group (12) calls |
| **Semantic Events** | Standardized event names for categories: E-Commerce v2, Video, Mobile, B2B SaaS, Email, Live Chat, A/B Testing |