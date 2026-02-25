# Development Environment Setup

Complete guide for setting up a RudderStack development environment. Covers prerequisites, Go toolchain installation, Docker Compose services, build system, local development workflow, and debugging tools. RudderStack is a Go 1.26.0 application with PostgreSQL, Transformer, and optional MinIO/etcd dependencies.

> This guide targets `rudder-server` v1.68.1 with Go 1.26.0 runtime.

**Source:** `README.md`, `Makefile`, `docker-compose.yml`, `Dockerfile`, `config/sample.env`, `build/docker.env`

---

## Table of Contents

- [Prerequisites](#prerequisites)
- [Go Toolchain Setup](#go-toolchain-setup)
- [Repository Setup](#repository-setup)
- [Docker Compose Services](#docker-compose-services)
- [Building the Project](#building-the-project)
- [Running Locally](#running-locally)
- [Makefile Reference](#makefile-reference)
- [Configuration](#configuration)
- [Developer Tools](#developer-tools)
- [Debugging](#debugging)
- [IDE Setup](#ide-setup)
- [Contributing Workflow](#contributing-workflow)
- [Related Documentation](#related-documentation)

---

## Prerequisites

The following software must be installed before you can build and run RudderStack locally.

### Required Software

| Software | Version | Purpose |
|----------|---------|---------|
| Go | 1.26.0 | Primary language runtime |
| Docker | 20.10+ | Container runtime for services |
| Docker Compose | 2.0+ (v3.7 format) | Service orchestration |
| PostgreSQL client | 15+ | Database CLI tools (included in Docker) |
| Git | 2.x | Version control |
| Make | 3.81+ | Build automation |
| protoc | 3.x | Protocol buffer compiler (for proto generation) |

> **Source:** `go.mod:3` (Go 1.26.0), `docker-compose.yml:1` (v3.7 format), `Dockerfile:5` (GO_VERSION=1.26.0)

### Optional but Recommended

| Tool | Purpose |
|------|---------|
| `curl` | Testing Gateway API endpoints (port 8080) |
| `psql` | Direct database inspection (PostgreSQL 15) |
| `etcdctl` | Multi-tenant development (etcd v3) |
| VS Code or GoLand | Recommended IDEs with Go support |
| Delve (`dlv`) | Interactive Go debugger |

---

## Go Toolchain Setup

### Installing Go

Download and install Go 1.26.0 from the official site:

```bash
# Download and install Go 1.26.0
# Visit https://go.dev/dl/ for platform-specific instructions

# Verify installation
go version
# Expected: go version go1.26.0 linux/amd64 (or your OS/arch)

# Ensure GOPATH and GOBIN are configured
export GOPATH=$HOME/go
export PATH=$PATH:$GOPATH/bin
```

Add the `GOPATH` and `PATH` exports to your shell profile (`~/.bashrc`, `~/.zshrc`, etc.) for persistence across sessions.

### Required Development Tools

The following tools are installed automatically via `make install-tools`:

| Tool | Package | Version | Purpose |
|------|---------|---------|---------|
| mockgen | `go.uber.org/mock/mockgen` | v0.6.0 | Mock code generation for unit tests |
| protoc-gen-go | `google.golang.org/protobuf/cmd/protoc-gen-go` | v1.33.0 | Protobuf Go code generator |
| protoc-gen-go-grpc | `google.golang.org/grpc/cmd/protoc-gen-go-grpc` | v1.3.0 | gRPC Go code generator |
| gotestsum | `gotest.tools/gotestsum` | v1.12.3 | Enhanced test runner with formatted output |

> **Source:** `Makefile:9-16, 101-106`

Install all required tools with a single command:

```bash
make install-tools
```

### Additional Development Tools

These tools are used by the `lint`, `fmt`, and `sec` Makefile targets but are invoked via `go run` and do not require prior installation:

| Tool | Package | Version | Purpose |
|------|---------|---------|---------|
| golangci-lint | `github.com/golangci/golangci-lint/v2/cmd/golangci-lint` | v2.9.0 | Linter aggregator |
| gofumpt | `mvdan.cc/gofumpt` | v0.9.1 | Strict Go formatter |
| goimports | `golang.org/x/tools/cmd/goimports` | latest | Import organizer |
| govulncheck | `golang.org/x/vuln/cmd/govulncheck` | latest | Vulnerability scanner |
| actionlint | `github.com/rhysd/actionlint/cmd/actionlint` | latest | GitHub Actions linter |
| gitleaks | `github.com/zricethezav/gitleaks/v8` | v8.21.2 | Secret scanner |

> **Source:** `Makefile:9-18`

These tools are run on-demand via `go run <package>@<version>` by the Makefile, so they download automatically when you invoke `make lint`, `make fmt`, or `make sec`.

---

## Repository Setup

### Clone and Initialize

```bash
# Clone the repository
git clone https://github.com/rudderlabs/rudder-server.git
cd rudder-server

# Download Go module dependencies
go mod download

# Install required development tools
make install-tools

# Verify the build compiles successfully
make build
```

### Contributor License Agreement (CLA)

First-time contributors must sign the **Contributor License Agreement (CLA)** before any pull request can be merged:

- **CLA Form:** [https://forms.gle/845JRGVZaC6kPZy68](https://forms.gle/845JRGVZaC6kPZy68)
- The CLA must be signed for your first commit to the project.
- Once signed, you are added to the list of approved contributors and all future contributions are covered.

> **Source:** `CONTRIBUTING.md:14-17`

### Repository Structure Overview

After cloning, the key directories relevant to development are:

| Directory | Description |
|-----------|-------------|
| `gateway/` | HTTP ingestion gateway (event API, auth, validation, webhooks) |
| `processor/` | Event processing pipeline (6-stage, transforms, consent, tracking plans) |
| `router/` | Real-time destination routing (throttling, ordering, retry) |
| `router/batchrouter/` | Batch routing and staging file generation |
| `warehouse/` | Warehouse loading service (9 connectors, schema evolution) |
| `services/` | 19 shared service packages (dedup, OAuth, transformer, etc.) |
| `jobsdb/` | PostgreSQL-backed persistent job queue |
| `backend-config/` | Dynamic workspace configuration management |
| `config/` | Configuration files (`config.yaml`, `sample.env`) |
| `proto/` | Protocol Buffer definitions (cluster, common, event-schema, warehouse) |
| `cmd/` | CLI tools (`devtool`, `rudder-cli`) |
| `build/` | Docker and build support files |

For a detailed understanding of the system architecture, see [Architecture Overview](../architecture/overview.md).

---

## Docker Compose Services

RudderStack uses Docker Compose (v3.7 format) to orchestrate its runtime dependencies. The `docker-compose.yml` defines five services, two of which are behind optional profiles.

### Service Definitions

| Service | Image | Port Mapping | Purpose |
|---------|-------|-------------|---------|
| `db` | `postgres:15-alpine` | `6432:5432` | PostgreSQL database for JobsDB persistent queue |
| `backend` | Built from `Dockerfile` | `8080:8080` | RudderStack server (Gateway + Processor + Router) |
| `transformer` | `rudderstack/rudder-transformer:latest` | `9090:9090` | External event transformation service |
| `minio` | `minio/minio` | `9000:9000`, `9001:9001` | S3-compatible object storage (profile: `storage`) |
| `etcd` | `docker.io/bitnami/etcd:3` | `2379:2379` | Cluster state management (profile: `multi-tenant`) |

> **Source:** `docker-compose.yml:1-54`

### Service Dependencies

The `backend` service depends on both `db` and `transformer`:

```
backend → db (PostgreSQL)
backend → transformer (rudder-transformer)
```

The `backend` entry point uses `wait-for` to ensure PostgreSQL is available before starting the server: `sh -c '/wait-for db:5432 -- ./rudder-server'`.

> **Source:** `docker-compose.yml:18`

### Startup Commands

```bash
# Start core services only (database + transformer)
docker-compose up -d db transformer

# Start all default services (db + backend + transformer)
docker-compose up -d

# Start with MinIO for object storage testing
docker-compose --profile storage up -d

# Start with etcd for multi-tenant development
docker-compose --profile multi-tenant up -d

# Start all services including optional profiles
docker-compose --profile storage --profile multi-tenant up -d

# View backend logs
docker-compose logs -f backend

# Stop all services and remove containers
docker-compose down

# Stop and remove volumes (full reset including database)
docker-compose down -v
```

### Docker Environment Variables

The `build/docker.env` file configures services when running within Docker Compose:

| Variable | Docker Value | Description |
|----------|-------------|-------------|
| `POSTGRES_USER` | `rudder` | PostgreSQL superuser name |
| `POSTGRES_PASSWORD` | `password` | PostgreSQL superuser password |
| `POSTGRES_DB` | `jobsdb` | Default database created on init |
| `DEST_TRANSFORM_URL` | `http://d-transformer:9090` | Transformer service URL (Docker network) |
| `JOBS_DB_HOST` | `db` | PostgreSQL hostname (Docker service name) |
| `JOBS_DB_PORT` | `5432` | PostgreSQL port (internal) |
| `JOBS_DB_DB_NAME` | `jobsdb` | Jobs database name |
| `JOBS_DB_USER` | `rudder` | Database connection user |
| `JOBS_DB_PASSWORD` | `password` | Database connection password |
| `JOBS_DB_SSL_MODE` | `disable` | SSL mode for database connections |
| `CONFIG_PATH` | `/app/config/config.yaml` | Configuration file path (container path) |
| `WORKSPACE_TOKEN` | `<your_token_here>` | Control plane workspace authentication token |
| `GO_ENV` | `production` | Runtime environment |
| `LOG_LEVEL` | `INFO` | Logging verbosity |

> **Source:** `build/docker.env:1-23`

### Local Development Environment Variables

For running the server outside of Docker (via `go run` or the compiled binary), use `config/sample.env` as a template:

| Variable | Local Value | Description |
|----------|------------|-------------|
| `JOBS_DB_HOST` | `localhost` | PostgreSQL host (connecting to Docker-exposed port) |
| `JOBS_DB_PORT` | `5432` | PostgreSQL port |
| `DEST_TRANSFORM_URL` | `http://localhost:9090` | Transformer URL (exposed Docker port) |
| `WORKSPACE_TOKEN` | `<your_token_here>` | Required for control plane connectivity |
| `CONFIG_PATH` | `./config/config.yaml` | Configuration file path (local filesystem) |

> **Source:** `config/sample.env:1-18`

Copy the sample environment file and customize for your setup:

```bash
cp config/sample.env .env
# Edit .env and set your WORKSPACE_TOKEN
```

---

## Building the Project

### Docker Image Build (Multi-Stage)

The `Dockerfile` uses a multi-stage build to produce a minimal production image:

**Builder Stage** — `golang:1.26.0-alpine3.23`:
- Sets `CGO_ENABLED=0` for static binary compilation
- Installs `make`, `tzdata`, and `ca-certificates`
- Downloads Go module dependencies
- Builds five binaries: `rudder-server`, `wait-for-go`, `regulation-worker`, `devtool`, `rudder-cli`
- Injects version metadata via LDFLAGS: `main.version`, `main.commit`, `main.buildDate`, `main.builtBy`, `main.enterpriseToken`

**Runtime Stage** — `alpine:3.23`:
- Minimal Alpine image with `tzdata`, `ca-certificates`, `postgresql-client`, `curl`, `bash`
- Copies only the compiled binaries from the builder stage
- Includes `docker-entrypoint.sh`, `wait-for` script, and event generation scripts
- Default entrypoint: `/docker-entrypoint.sh` → `/rudder-server`

> **Source:** `Dockerfile:1-53`

### Local Binary Build

Build the server binary and supporting tools directly on your development machine:

```bash
# Build all binaries (rudder-server, wait-for-go, regulation-worker)
make build

# Output binaries:
#   ./rudder-server              — main server binary
#   ./build/wait-for-go/wait-for-go  — dependency waiter utility
#   ./build/regulation-worker    — GDPR regulation worker

# Build flags applied: -a -installsuffix cgo -ldflags="-s -w"
```

> **Source:** `Makefile:80-88`

### Build with Race Detector

Enable the Go race detector for development builds to catch data races:

```bash
# Build with race detection enabled
RACE_ENABLED=TRUE make build

# Output: ./rudder-server-with-race (separate binary with race instrumentation)
```

> **Note:** Race-enabled binaries are significantly slower and use more memory. Use only during development and testing, never in production.

### Version Injection

The build system supports injecting version metadata via LDFLAGS. This is automatically handled in the Dockerfile but can be used for local builds:

```bash
# Build with explicit version metadata
LDFLAGS="-s -w -X main.version=v1.68.1 -X main.commit=$(git rev-parse HEAD) -X main.buildDate=$(date +%F,%T)" make build
```

> **Source:** `Dockerfile:29-31`

---

## Running Locally

Three options are available for running RudderStack in a development environment, from simplest to most flexible.

### Option 1: Docker Compose (Recommended for Quick Start)

The fastest way to get a fully operational RudderStack instance:

```bash
# Start all services
docker-compose up -d

# Verify services are running
docker-compose ps

# Service endpoints:
#   Gateway (HTTP API):  http://localhost:8080
#   Transformer:         http://localhost:9090
#   PostgreSQL:          localhost:6432
```

### Option 2: Go Run (Development Iteration)

Run the server binary directly via `go run` for fast development iteration. This requires PostgreSQL and Transformer to be running as Docker services:

```bash
# Step 1: Start infrastructure dependencies
docker-compose up -d db transformer

# Step 2: Set environment variables (or source .env file)
export JOBS_DB_HOST=localhost
export JOBS_DB_PORT=6432
export JOBS_DB_USER=rudder
export JOBS_DB_PASSWORD=password
export JOBS_DB_DB_NAME=jobsdb
export DEST_TRANSFORM_URL=http://localhost:9090
export WORKSPACE_TOKEN=<your_workspace_token>

# Step 3: Run the server
make run
# Equivalent to: go run main.go
```

> **Source:** `Makefile:89-90`

### Option 3: Multi-Tenant Mode

For developing multi-tenant features, run with etcd cluster coordination:

```bash
# Step 1: Start etcd alongside core dependencies
docker-compose --profile multi-tenant up -d db transformer etcd

# Step 2: Run in multi-tenant mode
make run-mt

# This executes three commands sequentially:
#   1. go run ./cmd/devtool etcd mode --no-wait normal
#   2. go run ./cmd/devtool etcd workspaces --no-wait none
#   3. DEPLOYMENT_TYPE=MULTITENANT go run main.go
```

> **Source:** `Makefile:92-95`

### Health Check and Verification

After starting the server, verify it is running and accepting events:

```bash
# Check server health endpoint
curl -s http://localhost:8080/health

# Send a test track event
curl -X POST http://localhost:8080/v1/track \
  -H "Content-Type: application/json" \
  -H "Authorization: Basic <base64_encoded_write_key>:" \
  -d '{
    "userId": "user123",
    "event": "Test Event",
    "properties": {
      "key": "value"
    }
  }'

# Expected: HTTP 200 with JSON response
```

> **Note:** The `Authorization` header uses HTTP Basic Auth with the Write Key as the username and an empty password. Encode your write key as `base64(writeKey:)`.

### Workspace Configuration

The server requires workspace configuration to define sources, destinations, and connections. Two approaches are available:

**Option A: Control Plane Connection (Default)**

Set the `WORKSPACE_TOKEN` environment variable to connect to the RudderStack Control Plane:

```bash
export WORKSPACE_TOKEN=<your_workspace_token>
```

Sign up for a free workspace at [https://app.rudderstack.com/signup?type=freetrial](https://app.rudderstack.com/signup?type=freetrial).

**Option B: Local Workspace Configuration File**

For offline or air-gapped development, load workspace configuration from a local JSON file:

```bash
# Set environment variables
export RSERVER_BACKEND_CONFIG_CONFIG_FROM_FILE=true
export RSERVER_BACKEND_CONFIG_CONFIG_JSONPATH=/path/to/workspaceConfig.json
```

When using Docker Compose, uncomment the volumes section in `docker-compose.yml`:

```yaml
# In docker-compose.yml, under the backend service:
volumes:
  - <absolute_path_to_workspace_config>:/etc/rudderstack/workspaceConfig.json
```

> **Source:** `docker-compose.yml:25-27`, `config/sample.env:27-29`

---

## Makefile Reference

The project includes a comprehensive Makefile with targets for building, testing, linting, formatting, and code generation. Run `make help` to see all targets with descriptions.

### Build Targets

| Target | Description |
|--------|-------------|
| `make build` | Build `rudder-server`, `wait-for-go`, and `regulation-worker` binaries. Supports `RACE_ENABLED=TRUE` for race detector builds. Build flags: `-a -installsuffix cgo -ldflags="-s -w"`. |
| `make run` | Run `rudder-server` via `go run main.go`. Requires database and transformer to be running. |
| `make run-mt` | Run in multi-tenant mode. Configures etcd, then launches with `DEPLOYMENT_TYPE=MULTITENANT`. |

> **Source:** `Makefile:80-95`

### Test Targets

| Target | Description |
|--------|-------------|
| `make test` | Run all unit tests with `gotestsum`. Installs tools, runs tests, then tears down (consolidates coverage). |
| `make test-run` | Execute the test suite. Uses `gotestsum --format pkgname-and-test-fails` with coverage profiling. Options: `-p=1 -v -failfast -shuffle=on -coverprofile=profile.out -covermode=atomic -vet=all --timeout=15m`. Supports `RACE_ENABLED=true`, `package=<path>`, `exclude=<pattern>`. |
| `make test-teardown` | Consolidate coverage profiles into `coverage.txt` and clean up temporary files. Exits with error if tests failed. |
| `make test-warehouse` | Run warehouse integration tests. Pattern: `TestIntegration`, timeout: 30m, parallelism: 8. |
| `make test-with-coverage` | Run `test` followed by `coverage` to generate an HTML coverage report. |
| `make coverage` | Generate HTML coverage report from `coverage.txt` → `coverage.html`. |

> **Source:** `Makefile:25-78`

### Code Quality Targets

| Target | Description |
|--------|-------------|
| `make lint` | Run all linters: `golangci-lint` (v2.9.0), `actionlint`, then security checks via `make sec`. Runs `make fmt` first. |
| `make fmt` | Format all Go files using `gofumpt` (v0.9.1) with `-extra` flag, `goimports` for import organization (local prefix: `github.com/rudderlabs`), `go fix`, and matrix checker. Also validates Docker Go version. |
| `make sec` | Run security checks: `gitleaks` (v8.21.2) for secret detection, `govulncheck` for vulnerability scanning. |

> **Source:** `Makefile:107-141`

### Code Generation Targets

| Target | Description |
|--------|-------------|
| `make mocks` | Regenerate all mock files via `go generate ./...`. Requires `mockgen` (installed by `install-tools`). |
| `make proto` | Generate Go code from Protocol Buffer definitions in `proto/**/*.proto`. Uses `protoc-gen-go` (v1.33.0) and `protoc-gen-go-grpc` (v1.3.0) with `paths=source_relative` option. |
| `make generate-openapi-spec` | Generate HTML API documentation from `gateway/openapi.yaml` using `openapitools/openapi-generator-cli:v7.3.0`. Output directory: `gateway/openapi/`. |

> **Source:** `Makefile:22-24, 121-136`

### Utility Targets

| Target | Description |
|--------|-------------|
| `make install-tools` | Install required Go development tools: `mockgen`, `protoc-gen-go`, `protoc-gen-go-grpc`, `gotestsum`. |
| `make bench-kafka` | Run Kafka compression benchmarks: `go test -count 1 -run BenchmarkCompression -bench=. -benchmem ./services/streammanager/kafka/client`. |
| `make help` | Display all available Makefile targets with descriptions (targets with `## comment` annotations). |

> **Source:** `Makefile:97-142`

---

## Configuration

RudderStack uses a layered configuration system with three priority levels. Higher-priority sources override lower-priority values.

### Configuration Priority Hierarchy

```
1. Environment Variables    (highest priority — always wins)
2. config/config.yaml       (file-based configuration)
3. Default Values in Code   (lowest priority — hardcoded fallbacks)
```

Any YAML configuration parameter can be overridden via an environment variable using the `RSERVER_` prefix convention. Path segments are separated by underscores, and camelCase is expanded to UPPER_SNAKE_CASE:

| YAML Parameter | Environment Variable Override |
|---|---|
| `Gateway.webPort` | `RSERVER_GATEWAY_WEB_PORT` |
| `Router.noOfWorkers` | `RSERVER_ROUTER_NO_OF_WORKERS` |
| `Warehouse.mode` | `RSERVER_WAREHOUSE_MODE` |
| `Processor.transformBatchSize` | `RSERVER_PROCESSOR_TRANSFORM_BATCH_SIZE` |

### Essential Development Parameters

The following parameters are most relevant during local development:

| Parameter | Default | Environment Variable | Description |
|-----------|---------|---------------------|-------------|
| Database host | `localhost` | `JOBS_DB_HOST` | PostgreSQL host address |
| Database port | `5432` | `JOBS_DB_PORT` | PostgreSQL port number |
| Database name | `jobsdb` | `JOBS_DB_DB_NAME` | Jobs database name |
| Database user | `rudder` | `JOBS_DB_USER` | PostgreSQL connection username |
| Database password | `rudder` | `JOBS_DB_PASSWORD` | PostgreSQL connection password |
| Database SSL mode | `disable` | `JOBS_DB_SSL_MODE` | SSL mode for database connections |
| Transformer URL | `http://localhost:9090` | `DEST_TRANSFORM_URL` | Event transformation service endpoint |
| Config file path | `./config/config.yaml` | `CONFIG_PATH` | Path to the YAML configuration file |
| Workspace token | *(required)* | `WORKSPACE_TOKEN` | Control plane workspace authentication token |
| Log level | `INFO` | `LOG_LEVEL` | Logging verbosity (`DEBUG`, `INFO`, `WARN`, `ERROR`) |
| Runtime environment | `production` | `GO_ENV` | Runtime environment identifier (`production`, `development`) |
| Gateway port | `8080` | `RSERVER_GATEWAY_WEB_PORT` | HTTP API listening port |

> **Source:** `config/sample.env:1-18`, `build/docker.env:1-23`, `config/config.yaml:19`

### Key config/config.yaml Parameters for Development

The `config/config.yaml` file contains 200+ tunable parameters. The most impactful for development are:

| YAML Path | Default | Description |
|-----------|---------|-------------|
| `maxProcess` | `12` | Maximum number of processor goroutines |
| `Gateway.webPort` | `8080` | HTTP API listening port |
| `Gateway.maxUserWebRequestWorkerProcess` | `64` | Worker pool size for HTTP request processing |
| `Gateway.maxDBWriterProcess` | `256` | Worker pool size for database batch writing |
| `Gateway.maxUserRequestBatchSize` | `128` | Maximum events per user request batch |
| `Gateway.maxDBBatchSize` | `128` | Maximum events per database write batch |
| `Gateway.maxReqSizeInKB` | `4000` | Maximum request payload size in kilobytes |
| `RateLimit.eventLimit` | `1000` | Events per rate limit window per source |
| `RateLimit.rateLimitWindow` | `60m` | Rate limit time window duration |

> **Source:** `config/config.yaml:1-28`

For the complete 200+ parameter reference, see [Configuration Reference](../reference/config-reference.md).

For all environment variables including backup storage, warehouse, alerting, and SSL configuration, see [Environment Variable Reference](../reference/env-var-reference.md).

---

## Developer Tools

### devtool CLI

The `devtool` CLI (`cmd/devtool/`) provides utilities for local development and testing:

```bash
# Build devtool
go build -o devtool ./cmd/devtool/

# --- etcd Management (multi-tenant development) ---

# Set etcd mode to normal
./devtool etcd mode --no-wait normal

# Configure etcd workspaces
./devtool etcd workspaces --no-wait none

# --- Event Sending ---

# Send test events to the Gateway
./devtool events send

# --- Webhook Simulation ---

# Start a webhook receiver for testing
./devtool webhooks
```

> The `devtool` binary is also built as part of the Docker image build process.
>
> **Source:** `Dockerfile:33`, `cmd/devtool/`

### rudder-cli Admin Tool

The `rudder-cli` tool (`cmd/rudder-cli/`) provides administrative operations via a UNIX domain socket connection to the running server:

```bash
# Build rudder-cli
go build -o rudder-cli ./cmd/rudder-cli/

# Run admin commands (server must be running)
./rudder-cli <command>
```

The `rudder-cli` binary is installed to `/usr/bin/rudder-cli` in the Docker image.

> **Source:** `Dockerfile:34, 45`

### OpenAPI Documentation Generation

Generate HTML API documentation from the OpenAPI specification:

```bash
# Generate HTML docs from gateway/openapi.yaml
make generate-openapi-spec

# This runs openapitools/openapi-generator-cli:v7.3.0 via Docker:
#   Input:  gateway/openapi.yaml (OpenAPI 3.0.3)
#   Output: gateway/openapi/ (HTML2 format)
```

The generated documentation is served at the `/docs` endpoint when the server is running.

> **Source:** `Makefile:130-136`

### Protocol Buffer Code Generation

Generate Go code from Protocol Buffer definitions:

```bash
# Generate Go structs and gRPC service code
make proto

# This runs:
#   protoc --go_out=paths=source_relative:. proto/**/*.proto
#   protoc --go-grpc_out=paths=source_relative:. proto/**/*.proto
#
# Proto definitions are in:
#   proto/cluster/   — Cluster partition migration RPCs
#   proto/common/    — DPAuth service definitions
#   proto/event-schema/ — Event schema key/message types
#   proto/warehouse/ — Warehouse service (15 unary RPCs)
```

> **Source:** `Makefile:121-124`

### Mock Generation

Regenerate mock files for unit testing:

```bash
# Regenerate all mocks
make mocks

# This runs: go generate ./...
# Requires mockgen (installed by make install-tools)
```

> **Source:** `Makefile:22-24`

---

## Debugging

### Log Level Adjustment

Increase logging verbosity for debugging:

```bash
# Set debug-level logging
export LOG_LEVEL=DEBUG

# Run the server with debug logs
LOG_LEVEL=DEBUG make run
```

Available log levels (from most to least verbose): `DEBUG`, `INFO`, `WARN`, `ERROR`.

> **Source:** `config/sample.env:18`

### Race Detector

The Go race detector helps identify data races during development and testing:

```bash
# Build with race detection
RACE_ENABLED=TRUE make build
# Produces: ./rudder-server-with-race

# Run tests with race detection
RACE_ENABLED=true make test
```

> **Note:** Race detection adds significant CPU and memory overhead (typically 5-10x slowdown). Enable only during development and CI testing.

### Interactive Debugging with Delve

Use the [Delve](https://github.com/go-delve/delve) debugger for interactive step-through debugging:

```bash
# Install Delve
go install github.com/go-delve/delve/cmd/dlv@latest

# Debug the main server
dlv debug main.go

# Debug with arguments
dlv debug main.go -- --config ./config/config.yaml

# Attach to a running process
dlv attach <pid>
```

### Performance Profiling

RudderStack includes Go's standard profiling capabilities:

```bash
# Run CPU profiling benchmarks
go test -cpuprofile cpu.out -memprofile mem.out -bench=. ./path/to/package/

# Analyze profiles
go tool pprof cpu.out
go tool pprof mem.out
```

### Database Inspection

Connect directly to the PostgreSQL database for debugging job queue state:

```bash
# Connect to PostgreSQL (Docker Compose port mapping: 6432 → 5432)
psql -h localhost -p 6432 -U rudder -d jobsdb

# Useful queries for debugging:
# List job tables
\dt gw_*

# Check pending jobs
SELECT job_id, workspace_id, custom_val, job_state, error_code
FROM gw_jobs_1
ORDER BY job_id DESC LIMIT 10;

# Check job status counts
SELECT job_state, COUNT(*)
FROM gw_job_status_1
GROUP BY job_state;
```

### Event Inspection

Test and debug event ingestion using curl:

```bash
# Send an identify event
curl -v -X POST http://localhost:8080/v1/identify \
  -H "Content-Type: application/json" \
  -H "Authorization: Basic <base64_write_key>:" \
  -d '{
    "userId": "user123",
    "traits": {
      "email": "user@example.com",
      "name": "Test User"
    }
  }'

# Send a batch of events
curl -v -X POST http://localhost:8080/v1/batch \
  -H "Content-Type: application/json" \
  -H "Authorization: Basic <base64_write_key>:" \
  -d '{
    "batch": [
      {"type": "track", "userId": "user123", "event": "Event 1"},
      {"type": "track", "userId": "user123", "event": "Event 2"}
    ]
  }'

# Check API documentation
curl http://localhost:8080/docs
```

---

## IDE Setup

### VS Code

Recommended VS Code extensions for RudderStack development:

| Extension | Extension ID | Purpose |
|-----------|-------------|---------|
| [Go](https://marketplace.visualstudio.com/items?itemName=golang.go) | `golang.go` | Go language support (IntelliSense, debugging, testing) |
| [Docker](https://marketplace.visualstudio.com/items?itemName=ms-azuretools.vscode-docker) | `ms-azuretools.vscode-docker` | Docker Compose and container management |
| [YAML](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml) | `redhat.vscode-yaml` | YAML language support for config files |
| [Protocol Buffers](https://marketplace.visualstudio.com/items?itemName=zxh404.vscode-proto3) | `zxh404.vscode-proto3` | Proto3 syntax highlighting and linting |
| [Mermaid](https://marketplace.visualstudio.com/items?itemName=bierner.markdown-mermaid) | `bierner.markdown-mermaid` | Mermaid diagram preview in Markdown |

> **Tip:** You can install these extensions directly from the VS Code command palette using `ext install <Extension ID>` (e.g., `ext install golang.go`). The marketplace URLs above require a browser to resolve and may not respond to automated HTTP HEAD requests.

Recommended VS Code `settings.json` configuration:

```json
{
  "go.toolsManagement.autoUpdate": true,
  "go.useLanguageServer": true,
  "go.lintTool": "golangci-lint",
  "go.lintFlags": ["-v"],
  "go.testFlags": ["-v", "-count=1"],
  "go.testTimeout": "15m",
  "editor.formatOnSave": true,
  "[go]": {
    "editor.defaultFormatter": "golang.go",
    "editor.codeActionsOnSave": {
      "source.organizeImports": "explicit"
    }
  }
}
```

### GoLand / IntelliJ IDEA

For JetBrains IDE users:

1. Open the project root directory (`rudder-server/`)
2. GoLand automatically detects the Go module from `go.mod`
3. Configure the Go SDK to version 1.26.0 in **File → Settings → Go → GOROOT**
4. Set the working directory to the project root for run configurations
5. Add environment variables from `config/sample.env` to run configurations:
   - **Run → Edit Configurations → Environment Variables**
   - Load from `.env` file or set individually

### Launch Configuration (VS Code)

Create `.vscode/launch.json` for debugging:

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Launch RudderStack Server",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/main.go",
      "envFile": "${workspaceFolder}/.env",
      "env": {
        "LOG_LEVEL": "DEBUG"
      }
    },
    {
      "name": "Launch DevTool",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/cmd/devtool/",
      "args": ["events", "send"]
    }
  ]
}
```

---

## Contributing Workflow

### Pull Request Submission Process

Follow these steps to submit a contribution to RudderStack:

1. **Fork the repository** and create a feature branch from `master`:
   ```bash
   git checkout -b feature/your-feature-name
   ```

2. **Sign the CLA** (first-time contributors only):
   - Complete the form at [https://forms.gle/845JRGVZaC6kPZy68](https://forms.gle/845JRGVZaC6kPZy68)

3. **Make your changes** following coding standards and project conventions.

4. **Format code** to ensure consistent style:
   ```bash
   make fmt
   ```

5. **Run linters** to check for code quality issues:
   ```bash
   make lint
   ```

6. **Run the full test suite** to verify nothing is broken:
   ```bash
   make test
   ```

7. **Submit your pull request** against the `master` branch.

> **Source:** `CONTRIBUTING.md:33-41`

### Commit Conventions

- **Squash or rebase** commits are preferred so that all changes from a branch are committed as a single logical unit.
- All pull requests are **squashed when merged** by default.
- **Rebasing** prior to merge gives you better control over the final commit message.

> **Source:** `CONTRIBUTING.md:39-41`

### Integration Contributions

To contribute a new destination integration:

- The primary repository for integration work is [**rudder-transformer**](https://github.com/rudderlabs/rudder-transformer)
- For detailed onboarding instructions, see [Destination Onboarding](./destination-onboarding.md)
- Follow the integration PR submission guide at [docs.rudderstack.com](https://docs.rudderstack.com/user-guides/how-to-guides/how-to-submit-an-integration-pull-request)

> **Source:** `CONTRIBUTING.md:23-31`

### Getting Help

- **Slack Community:** [https://www.rudderstack.com/join-rudderstack-slack-community/](https://www.rudderstack.com/join-rudderstack-slack-community/)
- **GitHub Issues:** [https://github.com/rudderlabs/rudder-server/issues](https://github.com/rudderlabs/rudder-server/issues)
- **Documentation:** [https://www.rudderstack.com/docs/](https://www.rudderstack.com/docs/)

---

## Related Documentation

| Document | Description |
|----------|-------------|
| [Testing Guidelines](./testing.md) | Comprehensive testing documentation including unit tests, integration tests, and test infrastructure |
| [Destination Onboarding](./destination-onboarding.md) | Guide for adding new destination connectors to the platform |
| [Architecture Overview](../architecture/overview.md) | High-level system architecture, component topology, and deployment modes |
| [Configuration Reference](../reference/config-reference.md) | Complete reference for all 200+ configuration parameters in `config/config.yaml` |
| [Environment Variable Reference](../reference/env-var-reference.md) | Complete reference for all environment variables in `config/sample.env` |
| [Getting Started: Installation](../guides/getting-started/installation.md) | Production installation guide for Docker, Kubernetes, and bare-metal deployments |
