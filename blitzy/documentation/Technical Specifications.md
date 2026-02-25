# Technical Specification

# 0. Agent Action Plan

## 0.1 Intent Clarification

### 0.1.1 Core Documentation Objective

Based on the provided requirements, the Blitzy platform understands that the documentation objective is to **create comprehensive, production-ready documentation for a Customer Data Platform (CDP) that achieves full functional parity with Twilio Segment**, built upon the RudderStack OSS codebase (`rudder-server` v1.68.1). The documentation must cover the complete platform lifecycle — from gap analysis and capability mapping through architecture documentation, API references, integration guides, and operational runbooks — enabling autonomous implementation of all capability gaps until Segment parity is validated.

**Documentation Category:** Create new documentation | Update existing documentation | Fix documentation gaps | Improve documentation coverage

**Documentation Types Required:**
- Architecture documentation (system design, data flows, component relationships)
- API reference documentation (Segment Spec event API parity: track, identify, page, group, alias, screen)
- Integration guides (Source SDK compatibility, destination connector coverage)
- Technical specifications (transformation logic, tracking plan enforcement, identity resolution)
- Migration guides (Segment-to-RudderStack migration path)
- Gap analysis reports (capability gap inventory between RudderStack and Segment)
- Operational guides (warehouse sync, replay semantics, pipeline configuration)
- Developer guides (Functions logic, Profiles API, Protocols enforcement)

**Detailed Requirements with Enhanced Clarity:**
- **Full Segment Spec Event Parity** — Document the six core API calls (`track`, `identify`, `page`, `screen`, `group`, `alias`) with payload schemas, common fields, semantic definitions, and behavioral parity with Segment's specification as referenced in `refs/segment-docs/src/connections/spec/`
- **Source SDK Compatibility** — Document SDK integration surfaces for JavaScript (web), iOS, Android, and server-side (Node.js, Python, Ruby, Go, Java) with configuration, initialization, and event transmission patterns
- **Destination Connector Coverage** — Document the full destination catalog matching Segment's connector catalog, covering the 90+ existing integrations referenced in `README.md` plus gap analysis for missing connectors referenced in `refs/segment-docs/src/connections/destinations/catalog/`
- **Transformation and Functions Logic** — Document the JavaScript/Python transformation framework, user transforms, destination transforms, and custom Functions equivalent to Segment Functions (`refs/segment-docs/src/connections/functions/`)
- **Tracking Plan / Protocols Enforcement** — Document tracking plan validation, schema enforcement, anomaly detection, and event governance equivalent to Segment Protocols (`refs/segment-docs/src/protocols/`)
- **Identity Resolution and Profiles** — Document cross-touchpoint identity unification, profile API, traits management, and data graph equivalent to Segment Unify (`refs/segment-docs/src/unify/`)
- **Warehouse Sync** — Document Snowflake (`warehouse/integrations/snowflake/`), BigQuery (`warehouse/integrations/bigquery/`), and Redshift (`warehouse/integrations/redshift/`) sync pipelines with idempotent, backfill-capable semantics
- **Replay and Replay-on-Failure Semantics** — Document event replay capabilities via the Archiver (`archiver/`) and replay handlers (`gateway/handle_http_replay.go`, `backend-config/replay_types.go`)

**Implicit Documentation Needs Surfaced:**
- Gap Report documenting the delta between current RudderStack capabilities and Segment feature set
- Sprint roadmap documentation for autonomous epic implementation sequencing
- Pipeline performance documentation covering the 50k events/sec throughput constraint with ordering guarantees
- Idempotency and backfill documentation for warehouse sync operations
- Payload parity documentation validating identical output between Segment and RudderStack destination connectors

### 0.1.2 Special Instructions and Constraints

**Critical Directives:**
- All Segment Spec events must route and transform identically to Segment behavior — documentation must validate and annotate behavioral parity at the payload level
- Destination connectors must maintain payload parity with Segment's connector output — documentation must include payload comparison schemas
- Pipeline must sustain 50,000 events/second with ordering guarantees — documentation must cover performance architecture, worker pool tuning, and capacity planning
- Warehouse sync must be idempotent and support backfill — documentation must specify merge strategies, dedup logic, and staging file formats per warehouse destination
- The Gap Report and sprint roadmap are initial-run deliverables — documentation must structure these as self-contained, actionable artifacts
- **Segment Engage/Campaigns and Reverse ETL are explicitly out of scope** for Phase 1

**Template Requirements:**
- Follow the existing RudderStack documentation style as observed in `README.md`, `CONTRIBUTING.md`, and sub-module READMEs (`services/oauth/README.md`, `router/batchrouter/asyncdestinationmanager/README.md`)
- Use the Segment documentation reference structure from `refs/segment-docs/` as the authoritative source for Segment feature catalog and spec definitions
- Leverage the existing OpenAPI contract at `gateway/openapi.yaml` as the baseline for API reference documentation

**Style Preferences:**
- Technical depth targeting senior engineers and data engineering teams
- Progressive disclosure: high-level architecture → detailed component docs → API reference
- Consistent terminology aligned with both RudderStack and Segment glossaries
- Mermaid diagrams for all architectural and data flow visualizations
- Code examples in Go (server-side), JavaScript (SDK/transforms), and Python (transforms)

### 0.1.3 Technical Interpretation

These documentation requirements translate to the following technical documentation strategy:

- To **document Segment Spec event parity**, we will create API reference docs for each event type (`track`, `identify`, `page`, `screen`, `group`, `alias`) by extracting endpoint definitions from `gateway/openapi.yaml`, handler implementations from `gateway/handle_http.go`, and payload schemas from `gateway/types.go`, cross-referenced against Segment spec definitions in `refs/segment-docs/src/connections/spec/`
- To **document Source SDK compatibility**, we will create SDK integration guides referencing the Gateway's Segment-compatible API surface (port 8080) and authentication schemes from `gateway/handle_http_auth.go`, covering JS, iOS, Android, and server-side SDK initialization patterns
- To **document destination connector coverage**, we will create a destination catalog by mapping existing connectors from `router/customdestinationmanager/`, `services/streammanager/`, and `warehouse/integrations/` against the Segment destination catalog in `refs/segment-docs/src/connections/destinations/catalog/`
- To **document transformation and Functions logic**, we will create developer guides covering the Processor's six-stage pipeline (`processor/pipeline_worker.go`), user transformations (batch size 200), destination transformations (batch size 100), and the external Transformer service integration
- To **document Tracking Plan / Protocols enforcement**, we will create governance guides based on `processor/trackingplan.go`, consent filtering in `processor/consent.go`, and validator logic in `gateway/validator/`
- To **document identity resolution and Profiles**, we will create identity documentation based on `warehouse/identity/` and the `alias` event handler, mapping capabilities against Segment Unify features from `refs/segment-docs/src/unify/`
- To **document warehouse sync**, we will create per-warehouse guides for Snowflake, BigQuery, and Redshift from `warehouse/integrations/`, the upload state machine in `warehouse/router/`, and encoding formats in `warehouse/encoding/`
- To **document replay semantics**, we will create operational guides covering the Archiver (`archiver/`), replay types (`backend-config/replay_types.go`), and replay HTTP handler (`gateway/handle_http_replay.go`)

### 0.1.4 Inferred Documentation Needs

Based on comprehensive code analysis, the following additional documentation needs have been identified:

- **Based on code analysis:** The `gateway/` module contains a full OpenAPI specification (`gateway/openapi.yaml`) but lacks corresponding developer-facing API reference documentation beyond the embedded `/docs` endpoint — a comprehensive external API reference is needed
- **Based on code analysis:** The `processor/consent.go` module implements OneTrust, Ketch, and Generic consent management with OR/AND semantics, but there is no user-facing documentation explaining consent configuration or provider setup
- **Based on structure:** The warehouse service spans 22+ sub-packages (`warehouse/router/`, `warehouse/schema/`, `warehouse/slave/`, `warehouse/encoding/`, `warehouse/identity/`, etc.) requiring consolidated architecture documentation with data flow diagrams
- **Based on dependencies:** The interaction between Processor, Transformer service (port 9090), and Router requires interface documentation covering batch sizing, retry semantics, and failure handling
- **Based on user journey:** New Segment-to-RudderStack migration requires a step-by-step migration guide covering SDK swap, destination re-mapping, tracking plan migration, and warehouse cutover
- **Based on configuration:** The `config/config.yaml` file contains 200+ tunable parameters across all subsystems — a configuration reference guide is needed documenting each parameter, its default, acceptable range, and impact
- **Based on gap analysis:** Segment features such as Functions (custom source/destination functions), Protocols (advanced tracking plan enforcement with anomaly detection), and Unify (identity graph, profile sync, traits) have partial or no equivalents in the current codebase — gap documentation is required

## 0.2 Documentation Discovery and Analysis

### 0.2.1 Existing Documentation Infrastructure Assessment

Repository analysis reveals a **sparse, module-local documentation structure** with no centralized documentation framework or generator. Documentation exists as scattered Markdown README files at the root and within select sub-modules, supplemented by a comprehensive Segment documentation reference repository at `refs/segment-docs/`.

**Documentation Files Discovered:**

| File | Location | Type | Coverage Status |
|------|----------|------|-----------------|
| README.md | Root | Project overview, key features, architecture summary, setup | Moderate — high-level only |
| CONTRIBUTING.md | Root | Contribution guidelines, CLA, PR process | Complete for contribution workflow |
| CHANGELOG.md | Root | Release history (v1.68.1 current) | Complete — auto-maintained |
| CODE_OF_CONDUCT.md | Root | Community behavioral standards | Complete |
| SECURITY.md | Root | Vulnerability reporting process | Complete |
| releases.md | Root | Release cadence documentation | Complete |
| LICENSE | Root | Elastic License 2.0 text | Complete |
| cmd/devtool/README.md | cmd/devtool/ | Developer tool CLI usage (etcd, events, webhooks) | Partial — basic usage only |
| services/oauth/README.md | services/oauth/ | OAuth module architecture, components, request lifecycle | Comprehensive — well-structured |
| router/batchrouter/asyncdestinationmanager/README.md | router/batchrouter/ | Async destination manager architecture, onboarding guide | Comprehensive — includes diagrams |
| regulation-worker/README.md | regulation-worker/ | Environment variable checklist (4 items) | Minimal — config only |
| suppression-backup-service/README.md | suppression-backup-service/ | Service description and setup | Minimal |
| utils/wrk/README.md | utils/wrk/ | Load testing utility documentation | Minimal |
| warehouse/.cursor/docs/snowpipe-streaming.md | warehouse/.cursor/docs/ | Snowpipe Streaming flow, polling, error handling | Moderate — internal working doc |
| warehouse/.cursor/docs/staging-file-flow.md | warehouse/.cursor/docs/ | Staging file pipeline, field propagation | Moderate — internal working doc |

**Documentation Framework Assessment:**
- Current documentation framework: **None** — no documentation generator (mkdocs, Sphinx, Docusaurus, etc.) is configured in the repository
- Documentation generator configuration: **Not present** — no `mkdocs.yml`, `docusaurus.config.js`, `sphinx/conf.py`, or equivalent found
- API documentation tools in use: **OpenAPI 3.0.3** spec at `gateway/openapi.yaml` with embedded HTML docs at `gateway/openapi/index.html` served at the `/docs` endpoint
- Diagram tools detected: **Mermaid** (used in existing internal docs and the async destination manager README)
- Documentation hosting/deployment: **Not configured** — no deployment pipeline for documentation exists in the repository
- Protobuf documentation: Proto definitions exist in `proto/` (cluster, common, event-schema, warehouse) but no generated documentation

**Segment Documentation Reference Assessment:**
The `refs/segment-docs/` directory contains a complete mirror of Segment's documentation site, implemented as a Jekyll/Liquid-based static site with the following structure:
- **Build system:** Jekyll with `_config.yml`, Netlify deployment via `netlify.toml`, Yarn/Bundler dependencies
- **Content structure:** Product-specific directories under `refs/segment-docs/src/` covering connections, engage, unify, protocols, privacy, monitoring, partners, and API reference
- **Catalog data:** Destination/source catalog metadata in `refs/segment-docs/src/_data/catalog/`
- **Spec definitions:** Full Segment Spec documentation in `refs/segment-docs/src/connections/spec/` (identify, track, page, screen, group, alias, common fields, semantic events)
- **Protocols/Tracking Plans:** Enforcement, validation, and tracking plan documentation in `refs/segment-docs/src/protocols/`
- **Unify/Identity Resolution:** Identity resolution, profiles, traits, and data graph in `refs/segment-docs/src/unify/`
- **Functions:** Source functions, destination functions, and insert functions in `refs/segment-docs/src/connections/functions/`

### 0.2.2 Repository Code Analysis for Documentation

**Search Patterns Used for Code to Document:**

- Public APIs: `gateway/openapi.yaml`, `gateway/handle_http.go`, `gateway/handle_http_auth.go`, `gateway/handle_http_import.go`, `gateway/handle_http_replay.go`, `gateway/handle_http_retl.go`, `gateway/handle_http_beacon.go`, `gateway/handle_http_pixel.go`
- Module interfaces: `processor/manager.go`, `router/factory.go`, `warehouse/app.go`, `runner/runner.go`
- Configuration options: `config/config.yaml` (200+ tunable parameters), `config/sample.env` (environment variable reference)
- Protocol definitions: `proto/cluster/`, `proto/common/`, `proto/event-schema/`, `proto/warehouse/` (15 unary RPCs)
- CLI commands: `cmd/devtool/` (etcd management, event sending, webhook simulation), `cmd/rudder-cli/` (admin CLI)
- Warehouse connectors: `warehouse/integrations/snowflake/`, `warehouse/integrations/bigquery/`, `warehouse/integrations/redshift/` plus six additional connectors
- Stream managers: `services/streammanager/` (Kafka, Kinesis, Pub/Sub, Azure Event Hub, Firehose, EventBridge, Confluent Cloud)
- Identity resolution: `warehouse/identity/` (merge-rule resolution pipelines)
- Consent/governance: `processor/consent.go`, `processor/trackingplan.go`, `processor/eventfilter/`

**Key Directories Examined:**

| Directory | Content Type | Documentation Status |
|-----------|-------------|---------------------|
| `gateway/` | HTTP ingestion gateway (handlers, auth, validation, throttling, webhooks) | OpenAPI spec exists; no developer guide |
| `processor/` | Event processing pipeline (6 stages, consent, tracking plans) | No documentation |
| `router/` | Real-time destination routing (throttling, ordering, retry) | No documentation |
| `router/batchrouter/` | Batch routing and staging file generation | Async destination manager README only |
| `warehouse/` | Warehouse loading orchestrator (22+ sub-packages) | Two internal cursor docs only |
| `services/` | 19 shared service packages | OAuth README only |
| `enterprise/` | Enterprise features (reporting, suppression, tracked users) | No documentation |
| `jobsdb/` | Persistent job queue (PostgreSQL-backed) | No documentation |
| `backend-config/` | Dynamic workspace configuration | No documentation |
| `controlplane/` | gRPC-based remote configuration | No documentation |
| `archiver/` | Event archival to object storage | No documentation |
| `regulation-worker/` | GDPR data deletion and regulation enforcement | Minimal env var checklist only |
| `refs/segment-docs/` | Segment documentation reference (complete mirror) | Comprehensive — used as reference |

### 0.2.3 Web Search Research Conducted

Research areas identified for documentation best practices:
- Best practices for CDP/data platform API documentation with Segment-compatible interfaces
- Documentation structure conventions for Go-based data pipeline projects
- Recommended Mermaid diagram types for event-driven pipeline architectures
- Tools and techniques for maintaining API documentation synchronized with OpenAPI specs
- Go documentation conventions (godoc, README patterns, package-level docs)
- Segment-to-RudderStack migration documentation patterns used by the community

## 0.3 Documentation Scope Analysis

### 0.3.1 Code-to-Documentation Mapping

**Core Pipeline Modules Requiring Documentation:**

- **Module: `gateway/` — Event Ingestion Gateway**
  - Public APIs: HTTP endpoints `/v1/identify`, `/v1/track`, `/v1/page`, `/v1/screen`, `/v1/group`, `/v1/alias`, `/v1/batch`, `/v1/import`, `/v1/replay`, `/v1/retl`, `/beacon/v1/*`, `/pixel/v1/*`, webhook endpoints
  - Handler implementations: `handle_http.go`, `handle_http_auth.go`, `handle_http_import.go`, `handle_http_replay.go`, `handle_http_retl.go`, `handle_http_beacon.go`, `handle_http_pixel.go`
  - Current documentation: OpenAPI spec at `gateway/openapi.yaml` — no developer-facing guide
  - Documentation needed: API reference (all endpoints), authentication guide (5 auth schemes), rate limiting guide, webhook integration guide, pixel/beacon tracking guide

- **Module: `processor/` — Event Processing Pipeline**
  - Public APIs: Six-stage pipeline (preprocess → source hydration → pre-transform → user transform → destination transform → store)
  - Key files: `processor.go`, `pipeline_worker.go`, `partition_worker.go`, `consent.go`, `trackingplan.go`, `src_hydration_stage.go`, `eventfilter/`
  - Current documentation: None
  - Documentation needed: Pipeline architecture guide, transformation developer guide, consent management configuration guide, tracking plan enforcement guide, event filtering reference

- **Module: `router/` — Real-Time Destination Routing**
  - Public APIs: Per-destination delivery with throttling, ordering, retry, and adaptive batching
  - Key files: `handle.go`, `worker.go`, `network.go`, `factory.go`, `config.go`, `throttler/`
  - Current documentation: None
  - Documentation needed: Routing architecture guide, throttling configuration guide, event ordering reference, retry policy documentation, destination connector developer guide

- **Module: `router/batchrouter/` — Batch Routing**
  - Public APIs: Bulk delivery with staging file generation, async destination management
  - Key files: Batch router handles, `asyncdestinationmanager/`
  - Current documentation: `asyncdestinationmanager/README.md` (comprehensive)
  - Documentation needed: Batch routing architecture guide, staging file format reference, async destination onboarding guide (extend existing README)

- **Module: `warehouse/` — Warehouse Loading Service**
  - Public APIs: gRPC and HTTP APIs on port 8082, upload state machine (7 states), schema evolution, parallel loading
  - Key directories: `warehouse/router/` (state machine), `warehouse/schema/` (schema management), `warehouse/integrations/` (9 connectors), `warehouse/encoding/` (Parquet/JSON/CSV), `warehouse/identity/` (identity resolution), `warehouse/slave/` (distributed processing), `warehouse/api/` (gRPC/HTTP endpoints)
  - Current documentation: Two internal cursor docs (`snowpipe-streaming.md`, `staging-file-flow.md`)
  - Documentation needed: Warehouse architecture guide, per-connector configuration guides (Snowflake, BigQuery, Redshift, ClickHouse, Databricks, MSSQL, PostgreSQL, Datalake, Azure Synapse), schema evolution reference, identity resolution guide, encoding format reference, master/slave deployment guide

- **Module: `services/streammanager/` — Stream Destination Management**
  - Supported streams: Kafka, Kinesis, Firehose, EventBridge, Google Pub/Sub, Azure Event Hub, Confluent Cloud, BigQuery Stream, Google Sheets, Lambda, Google Cloud Function, Wunderkind, Redis
  - Current documentation: None
  - Documentation needed: Stream destination integration guides, producer configuration reference, per-stream setup guides

- **Module: `services/dedup/` — Deduplication Service**
  - Implementations: BadgerDB-backed and KeyDB-backed with mirror mode
  - Current documentation: None
  - Documentation needed: Dedup configuration guide, TTL settings, backend selection guide

- **Module: `services/oauth/` — OAuth Integration**
  - Key files: `v2/http/client.go`, `v2/http/transport.go`, `v2/oauth.go`, `v2/controlplane/cp_connector.go`
  - Current documentation: `services/oauth/README.md` (comprehensive)
  - Documentation needed: Update existing README to cover gap analysis context

- **Module: `enterprise/` — Enterprise Features**
  - Sub-modules: `enterprise/reporting/` (telemetry, error extraction/grouping), `enterprise/suppress-user/` (user suppression), `enterprise/trackedusers/` (HyperLogLog tracking), `enterprise/config-env/` (environment variable substitution)
  - Current documentation: None
  - Documentation needed: Enterprise feature overview, reporting configuration guide, user suppression guide, tracked users guide

- **Module: `jobsdb/` — Persistent Job Queue**
  - Key capabilities: Partitioned datasets, priority pools, pending events registry, distinct values cache, COPY IN bulk inserts
  - Current documentation: None
  - Documentation needed: JobsDB architecture guide, partitioning reference, migration guide, performance tuning guide

- **Module: `backend-config/` — Dynamic Configuration**
  - Key capabilities: 5-second polling, AES-GCM encrypted caching, pub/sub distribution, namespace/single-workspace modes
  - Current documentation: None
  - Documentation needed: Configuration management architecture guide, encrypted cache reference, namespace configuration guide

- **Module: `archiver/` — Event Archival and Replay**
  - Key capabilities: Partition-aware archival to object storage, gzipped JSONL, 10-day retention, source/date/hour organization
  - Current documentation: None
  - Documentation needed: Archival architecture guide, replay semantics guide, retention policy reference

- **Module: `regulation-worker/` — Data Regulation**
  - Key capabilities: API/batch/KV store deletion, GDPR compliance, OAuthv2 integration
  - Current documentation: Minimal env var checklist
  - Documentation needed: GDPR compliance guide, deletion strategy reference, regulation worker operational guide

**Configuration Options Requiring Documentation:**

- Config file: `config/config.yaml`
  - Options documented: Approximately 10/200+ (5%)
  - Missing documentation: Gateway tuning (web workers, DB writers, batch sizes, rate limiting), Router tuning (workers, batch sizes, retry windows, throttling), Warehouse tuning (modes, workers, parallel loads, backoff), Processor tuning (transform batch sizes, dedup, consent), BackendConfig polling, Logger settings, Diagnostics toggles

- Config file: `config/sample.env`
  - Options documented: Partially through inline comments
  - Missing documentation: Structured environment variable reference guide with descriptions, defaults, and allowed values

### 0.3.2 Documentation Gap Analysis

Given the requirements and repository analysis, documentation gaps include:

**Critical Undocumented Public APIs:**
- All Gateway HTTP endpoints beyond the OpenAPI spec (no developer guides, no usage examples)
- Warehouse gRPC API (15 unary RPCs defined in `proto/warehouse/`)
- Admin RPC server (UNIX domain socket operations)
- Control Plane DPAuth service (authentication token distribution)

**Missing User Guides:**
- Segment-to-RudderStack migration guide
- Source SDK integration guides (JS, iOS, Android, server-side)
- Destination connector setup guides (90+ connectors)
- Transformation developer guide (JavaScript/Python custom transforms)
- Tracking Plan / Protocols configuration and enforcement guide
- Identity resolution and Profiles guide
- Warehouse sync configuration and operational guide (per-warehouse)
- Replay and replay-on-failure operational guide
- Pipeline capacity planning and performance tuning guide

**Incomplete Architecture Documentation:**
- End-to-end data flow architecture (ingestion → processing → routing → warehouse)
- Six-stage Processor pipeline architecture with Mermaid diagrams
- Warehouse 7-state upload state machine documentation
- Cluster management and deployment topology documentation
- Multi-tenant deployment architecture

**Outdated or Missing Reference Documentation:**
- Configuration parameter reference (200+ parameters in `config/config.yaml`)
- Environment variable reference (50+ variables in `config/sample.env`)
- Error code and response reference (from `gateway/response/`)
- Protobuf service definitions documentation

**Gap Report — Segment Feature Parity:**
- Segment Functions (source/destination custom functions) — partial equivalent via Transformer, needs gap documentation
- Segment Protocols (advanced tracking plan enforcement, anomaly detection) — partial via `processor/trackingplan.go`, needs gap documentation
- Segment Unify (identity graph, profile sync, traits, data graph) — partial via `warehouse/identity/`, needs extensive gap documentation
- Segment Destination Catalog (300+ destinations) — current 90+ destinations, needs catalog comparison documentation
- Segment Source Catalog (cloud sources, auto-instrumentation) — needs catalog comparison documentation

## 0.4 Documentation Implementation Design

### 0.4.1 Documentation Structure Planning

The documentation will be organized into a hierarchical structure optimized for developer-first navigation, following progressive disclosure from overview to detail:

```
docs/
├── README.md (project overview, quick start, key features)
├── gap-report/
│   ├── index.md (executive summary of Segment parity gaps)
│   ├── event-spec-parity.md (track, identify, page, screen, group, alias)
│   ├── destination-catalog-parity.md (connector gap analysis)
│   ├── source-catalog-parity.md (SDK and source gap analysis)
│   ├── functions-parity.md (transformation/Functions gap analysis)
│   ├── protocols-parity.md (tracking plan/Protocols gap analysis)
│   ├── identity-parity.md (identity resolution/Unify gap analysis)
│   ├── warehouse-parity.md (warehouse sync gap analysis)
│   └── sprint-roadmap.md (epic sequencing for gap closure)
├── architecture/
│   ├── overview.md (high-level system architecture)
│   ├── data-flow.md (end-to-end event pipeline with Mermaid diagrams)
│   ├── deployment-topologies.md (EMBEDDED, GATEWAY, PROCESSOR modes)
│   ├── pipeline-stages.md (6-stage Processor pipeline detail)
│   ├── warehouse-state-machine.md (7-state upload lifecycle)
│   ├── cluster-management.md (etcd-based multi-tenant coordination)
│   └── security.md (auth, encryption, SSRF protection, OAuth)
├── api-reference/
│   ├── index.md (API overview and authentication)
│   ├── event-spec/
│   │   ├── common-fields.md (shared event fields reference)
│   │   ├── identify.md (identify call specification)
│   │   ├── track.md (track call specification)
│   │   ├── page.md (page call specification)
│   │   ├── screen.md (screen call specification)
│   │   ├── group.md (group call specification)
│   │   └── alias.md (alias call specification)
│   ├── gateway-http-api.md (full HTTP API reference from OpenAPI)
│   ├── warehouse-grpc-api.md (15 unary RPCs reference)
│   ├── admin-api.md (UNIX socket admin operations)
│   └── error-codes.md (response codes and error reference)
├── guides/
│   ├── getting-started/
│   │   ├── installation.md (Docker, Kubernetes, developer machine)
│   │   ├── configuration.md (config.yaml and env var reference)
│   │   └── first-events.md (sending first events tutorial)
│   ├── migration/
│   │   ├── segment-migration.md (Segment-to-RudderStack migration)
│   │   └── sdk-swap-guide.md (SDK replacement walkthrough)
│   ├── sources/
│   │   ├── javascript-sdk.md (web SDK integration)
│   │   ├── ios-sdk.md (iOS SDK integration)
│   │   ├── android-sdk.md (Android SDK integration)
│   │   └── server-side-sdks.md (Node.js, Python, Go, etc.)
│   ├── destinations/
│   │   ├── index.md (destination catalog overview)
│   │   ├── stream-destinations.md (Kafka, Kinesis, Pub/Sub, etc.)
│   │   ├── cloud-destinations.md (90+ cloud integrations)
│   │   └── warehouse-destinations.md (warehouse integration overview)
│   ├── transformations/
│   │   ├── overview.md (transformation architecture)
│   │   ├── user-transforms.md (JavaScript/Python custom transforms)
│   │   ├── destination-transforms.md (payload shaping)
│   │   └── functions.md (Segment Functions equivalent)
│   ├── governance/
│   │   ├── tracking-plans.md (tracking plan configuration)
│   │   ├── consent-management.md (OneTrust, Ketch, Generic CMP)
│   │   ├── event-filtering.md (event drop/filter rules)
│   │   └── protocols-enforcement.md (schema validation)
│   ├── identity/
│   │   ├── identity-resolution.md (cross-touchpoint unification)
│   │   └── profiles.md (user profiles and traits)
│   └── operations/
│       ├── warehouse-sync.md (sync configuration and monitoring)
│       ├── replay.md (event replay and replay-on-failure)
│       ├── privacy-compliance.md (GDPR deletion, user suppression)
│       └── capacity-planning.md (throughput tuning for 50k events/sec)
├── warehouse/
│   ├── overview.md (warehouse service architecture)
│   ├── snowflake.md (Snowflake connector guide)
│   ├── bigquery.md (BigQuery connector guide)
│   ├── redshift.md (Redshift connector guide)
│   ├── clickhouse.md (ClickHouse connector guide)
│   ├── databricks.md (Databricks Delta Lake guide)
│   ├── postgres.md (PostgreSQL connector guide)
│   ├── mssql.md (SQL Server connector guide)
│   ├── azure-synapse.md (Azure Synapse connector guide)
│   ├── datalake.md (S3/GCS/Azure Datalake guide)
│   ├── schema-evolution.md (automatic schema management)
│   └── encoding-formats.md (Parquet, JSON, CSV reference)
├── reference/
│   ├── config-reference.md (all 200+ config.yaml parameters)
│   ├── env-var-reference.md (environment variable reference)
│   ├── glossary.md (unified terminology)
│   └── faq.md (frequently asked questions)
└── contributing/
    ├── development.md (development environment setup)
    ├── destination-onboarding.md (adding new destination connectors)
    └── testing.md (test infrastructure and guidelines)
```

### 0.4.2 Content Generation Strategy

**Information Extraction Approach:**
- Extract API endpoint signatures from `gateway/openapi.yaml` and HTTP handler files in `gateway/handle_http*.go`
- Generate event spec documentation by cross-referencing `gateway/types.go` payload structures with Segment spec definitions from `refs/segment-docs/src/connections/spec/`
- Extract configuration parameter descriptions from `config/config.yaml` inline comments and code-level `config.GetReloadable*` calls throughout the codebase
- Create architecture diagrams by mapping component relationships from `runner/runner.go` (lifecycle orchestrator) and data flow patterns documented in tech spec sections 5.1 and 6.1
- Generate warehouse connector guides from integration implementations in `warehouse/integrations/*/` cross-referenced with per-destination test suites
- Extract gap analysis data by comparing Segment destination catalog in `refs/segment-docs/src/connections/destinations/catalog/` against registered connectors in `router/customdestinationmanager/` and `services/streammanager/`

**Documentation Standards:**
- Markdown formatting with proper headers (# for titles, ## for sections, ### for subsections)
- Mermaid diagram integration for all architectural and data flow visualizations
- Code examples in Go, JavaScript, and Python with syntax highlighting
- Source citations as inline references: `Source: /path/to/file.go:LineNumber`
- Tables for parameter descriptions, return values, and comparison matrices
- Consistent terminology aligned with the unified glossary

### 0.4.3 Diagram and Visual Strategy

**Mermaid Diagrams to Create:**

- **System Architecture Diagram** — High-level component topology showing Gateway, Processor, Router, Batch Router, Warehouse, and external dependencies (for `docs/architecture/overview.md`)
- **End-to-End Data Flow Diagram** — Event lifecycle from SDK ingestion through warehouse loading (for `docs/architecture/data-flow.md`)
- **Processor Pipeline Diagram** — Six-stage pipeline with channel orchestration (for `docs/architecture/pipeline-stages.md`)
- **Warehouse Upload State Machine** — 7-state lifecycle diagram (for `docs/architecture/warehouse-state-machine.md`)
- **Deployment Topology Diagrams** — EMBEDDED, GATEWAY, PROCESSOR mode configurations (for `docs/architecture/deployment-topologies.md`)
- **Segment Parity Gap Matrix** — Visual feature comparison chart (for `docs/gap-report/index.md`)
- **Authentication Flow Diagrams** — 5 auth scheme flows (for `docs/api-reference/index.md`)
- **Warehouse Connector Flow Diagrams** — Per-warehouse staging → loading → schema evolution flows (for each warehouse connector guide)
- **Identity Resolution Flow** — Merge-rule resolution pipeline (for `docs/guides/identity/identity-resolution.md`)
- **Consent Filtering Decision Tree** — OR/AND semantics for OneTrust, Ketch, Generic (for `docs/guides/governance/consent-management.md`)
- **Replay Semantics Flow** — Archive → replay → re-ingestion pipeline (for `docs/guides/operations/replay.md`)
- **Configuration Resilience Diagram** — Control Plane polling → encrypted cache → fallback flow (for `docs/architecture/security.md`)

## 0.5 Documentation File Transformation Mapping

### 0.5.1 File-by-File Documentation Plan

The following exhaustive transformation map covers every documentation file to be created, updated, or referenced in this documentation effort. Target documentation files are listed first.

| Target Documentation File | Transformation | Source Code/Docs | Content/Changes |
|---------------------------|----------------|------------------|-----------------|
| docs/gap-report/index.md | CREATE | refs/segment-docs/src/**, README.md, all component dirs | Executive summary of Segment-RudderStack parity gaps with feature matrix and prioritized gap inventory |
| docs/gap-report/event-spec-parity.md | CREATE | gateway/openapi.yaml, gateway/types.go, refs/segment-docs/src/connections/spec/*.md | Segment Spec event-by-event parity analysis (track, identify, page, screen, group, alias) with payload comparison |
| docs/gap-report/destination-catalog-parity.md | CREATE | router/customdestinationmanager/, services/streammanager/, refs/segment-docs/src/connections/destinations/catalog/ | Full destination catalog comparison with coverage percentage and missing connectors list |
| docs/gap-report/source-catalog-parity.md | CREATE | refs/segment-docs/src/connections/sources/catalog/, gateway/handle_http_auth.go | Source SDK and cloud source gap analysis with SDK compatibility matrix |
| docs/gap-report/functions-parity.md | CREATE | processor/usertransformer/, refs/segment-docs/src/connections/functions/ | Transformation/Functions gap analysis comparing RudderStack transforms vs Segment Functions |
| docs/gap-report/protocols-parity.md | CREATE | processor/trackingplan.go, processor/consent.go, refs/segment-docs/src/protocols/ | Tracking Plan/Protocols enforcement gap analysis with feature comparison matrix |
| docs/gap-report/identity-parity.md | CREATE | warehouse/identity/, refs/segment-docs/src/unify/ | Identity resolution/Unify gap analysis covering identity graph, profile sync, traits |
| docs/gap-report/warehouse-parity.md | CREATE | warehouse/integrations/**, refs/segment-docs/src/connections/storage/ | Warehouse sync gap analysis covering idempotency, backfill, and connector features |
| docs/gap-report/sprint-roadmap.md | CREATE | All gap report files | Epic sequencing roadmap for autonomous gap closure implementation |
| docs/architecture/overview.md | CREATE | runner/runner.go, app/app.go, docker-compose.yml | High-level system architecture with Mermaid component diagram |
| docs/architecture/data-flow.md | CREATE | gateway/, processor/, router/, warehouse/ | End-to-end event data flow with Mermaid sequence diagrams |
| docs/architecture/deployment-topologies.md | CREATE | app/app.go, app/apphandlers/, utils/types/deployment/ | EMBEDDED/GATEWAY/PROCESSOR deployment modes with topology diagrams |
| docs/architecture/pipeline-stages.md | CREATE | processor/pipeline_worker.go, processor/partition_worker.go | Six-stage Processor pipeline architecture with channel orchestration diagrams |
| docs/architecture/warehouse-state-machine.md | CREATE | warehouse/router/state.go, warehouse/router/ | 7-state upload lifecycle state machine with transition diagrams |
| docs/architecture/cluster-management.md | CREATE | app/cluster/dynamic.go, controlplane/ | etcd-based cluster state management with NormalMode/DegradedMode diagrams |
| docs/architecture/security.md | CREATE | gateway/handle_http_auth.go, backend-config/internal/, services/oauth/, router/network.go | Security architecture covering auth, encryption, SSRF protection, OAuth |
| docs/api-reference/index.md | CREATE | gateway/openapi.yaml, gateway/handle_http_auth.go | API overview with authentication guide covering 5 auth schemes |
| docs/api-reference/event-spec/common-fields.md | CREATE | gateway/types.go, refs/segment-docs/src/connections/spec/common.md | Common event fields reference with RudderStack-specific extensions |
| docs/api-reference/event-spec/identify.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/spec/identify.md | Identify call specification with payload schema and examples |
| docs/api-reference/event-spec/track.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/spec/track.md | Track call specification with payload schema and examples |
| docs/api-reference/event-spec/page.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/spec/page.md | Page call specification with payload schema and examples |
| docs/api-reference/event-spec/screen.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/spec/screen.md | Screen call specification with payload schema and examples |
| docs/api-reference/event-spec/group.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/spec/group.md | Group call specification with payload schema and examples |
| docs/api-reference/event-spec/alias.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/spec/alias.md | Alias call specification with payload schema and examples |
| docs/api-reference/gateway-http-api.md | CREATE | gateway/openapi.yaml, gateway/handle_http*.go | Full HTTP API reference for all Gateway endpoints |
| docs/api-reference/warehouse-grpc-api.md | CREATE | proto/warehouse/ | Warehouse gRPC service reference (15 unary RPCs) |
| docs/api-reference/admin-api.md | CREATE | admin/, cmd/rudder-cli/ | Admin RPC and CLI operations reference |
| docs/api-reference/error-codes.md | CREATE | gateway/response/ | HTTP response codes and error message reference |
| docs/guides/getting-started/installation.md | CREATE | docker-compose.yml, Dockerfile, README.md | Docker, Kubernetes, and developer machine installation guide |
| docs/guides/getting-started/configuration.md | CREATE | config/config.yaml, config/sample.env | Configuration quickstart with essential parameters |
| docs/guides/getting-started/first-events.md | CREATE | gateway/openapi.yaml, cmd/devtool/ | Tutorial for sending first events with curl and devtool |
| docs/guides/migration/segment-migration.md | CREATE | refs/segment-docs/src/connections/spec/, gateway/openapi.yaml | Step-by-step Segment-to-RudderStack migration guide |
| docs/guides/migration/sdk-swap-guide.md | CREATE | refs/segment-docs/src/connections/sources/, gateway/handle_http_auth.go | SDK replacement walkthrough for JS, iOS, Android, server-side |
| docs/guides/sources/javascript-sdk.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/sources/ | JavaScript web SDK integration guide |
| docs/guides/sources/ios-sdk.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/sources/ | iOS SDK integration guide |
| docs/guides/sources/android-sdk.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/sources/ | Android SDK integration guide |
| docs/guides/sources/server-side-sdks.md | CREATE | gateway/openapi.yaml, refs/segment-docs/src/connections/sources/ | Server-side SDK guide (Node.js, Python, Go, Java, Ruby) |
| docs/guides/destinations/index.md | CREATE | router/customdestinationmanager/, services/streammanager/ | Destination catalog overview with categorization |
| docs/guides/destinations/stream-destinations.md | CREATE | services/streammanager/ | Stream destination configuration guides (Kafka, Kinesis, Pub/Sub, etc.) |
| docs/guides/destinations/cloud-destinations.md | CREATE | router/network.go, router/handle.go | Cloud destination integration overview |
| docs/guides/destinations/warehouse-destinations.md | CREATE | warehouse/integrations/, warehouse/app.go | Warehouse destination overview linking to per-connector guides |
| docs/guides/transformations/overview.md | CREATE | processor/pipeline_worker.go, processor/manager.go | Transformation system architecture overview |
| docs/guides/transformations/user-transforms.md | CREATE | processor/usertransformer/, processor/processor.go | JavaScript/Python custom transformation developer guide |
| docs/guides/transformations/destination-transforms.md | CREATE | processor/transformer/, router/transformer/ | Destination-specific payload transformation reference |
| docs/guides/transformations/functions.md | CREATE | refs/segment-docs/src/connections/functions/, processor/ | Segment Functions equivalent documentation with gap analysis |
| docs/guides/governance/tracking-plans.md | CREATE | processor/trackingplan.go, refs/segment-docs/src/protocols/tracking-plan/ | Tracking plan configuration and enforcement guide |
| docs/guides/governance/consent-management.md | CREATE | processor/consent.go | OneTrust, Ketch, and Generic CMP consent management guide |
| docs/guides/governance/event-filtering.md | CREATE | processor/eventfilter/ | Event drop and filter rules configuration guide |
| docs/guides/governance/protocols-enforcement.md | CREATE | processor/trackingplan.go, gateway/validator/, refs/segment-docs/src/protocols/ | Schema validation and Protocols enforcement guide |
| docs/guides/identity/identity-resolution.md | CREATE | warehouse/identity/, refs/segment-docs/src/unify/identity-resolution/ | Cross-touchpoint identity unification guide |
| docs/guides/identity/profiles.md | CREATE | warehouse/identity/, refs/segment-docs/src/unify/ | User profiles and traits management guide |
| docs/guides/operations/warehouse-sync.md | CREATE | warehouse/router/, warehouse/app.go, config/config.yaml | Warehouse sync configuration, monitoring, and troubleshooting guide |
| docs/guides/operations/replay.md | CREATE | archiver/, gateway/handle_http_replay.go, backend-config/replay_types.go | Event replay and replay-on-failure operational guide |
| docs/guides/operations/privacy-compliance.md | CREATE | regulation-worker/, enterprise/suppress-user/ | GDPR compliance, data deletion, and user suppression guide |
| docs/guides/operations/capacity-planning.md | CREATE | config/config.yaml, router/throttler/, gateway/throttler/ | Pipeline capacity planning guide for 50k events/sec target |
| docs/warehouse/overview.md | CREATE | warehouse/app.go, warehouse/router/ | Warehouse service architecture and operational modes |
| docs/warehouse/snowflake.md | CREATE | warehouse/integrations/snowflake/, warehouse/.cursor/docs/snowpipe-streaming.md | Snowflake connector setup, configuration, and Snowpipe Streaming guide |
| docs/warehouse/bigquery.md | CREATE | warehouse/integrations/bigquery/ | BigQuery connector setup, configuration, and parallel loading guide |
| docs/warehouse/redshift.md | CREATE | warehouse/integrations/redshift/ | Redshift connector setup with IAM/password auth and manifest loading |
| docs/warehouse/clickhouse.md | CREATE | warehouse/integrations/clickhouse/ | ClickHouse connector setup with MergeTree engine and cluster support |
| docs/warehouse/databricks.md | CREATE | warehouse/integrations/deltalake/ | Databricks Delta Lake connector with merge/append strategies |
| docs/warehouse/postgres.md | CREATE | warehouse/integrations/postgres/ | PostgreSQL warehouse connector guide |
| docs/warehouse/mssql.md | CREATE | warehouse/integrations/mssql/ | SQL Server connector with bulk CopyIn ingestion |
| docs/warehouse/azure-synapse.md | CREATE | warehouse/integrations/azure-synapse/ | Azure Synapse connector with COPY INTO ingestion |
| docs/warehouse/datalake.md | CREATE | warehouse/integrations/datalake/ | S3/GCS/Azure Datalake connector with Parquet exports |
| docs/warehouse/schema-evolution.md | CREATE | warehouse/schema/ | Automatic schema management and evolution reference |
| docs/warehouse/encoding-formats.md | CREATE | warehouse/encoding/ | Parquet, JSON, CSV encoding format reference |
| docs/reference/config-reference.md | CREATE | config/config.yaml | Complete configuration parameter reference (200+ parameters) |
| docs/reference/env-var-reference.md | CREATE | config/sample.env | Environment variable reference with descriptions and defaults |
| docs/reference/glossary.md | CREATE | refs/segment-docs/src/glossary.md, README.md | Unified terminology glossary (RudderStack + Segment terms) |
| docs/reference/faq.md | CREATE | README.md, CONTRIBUTING.md | Frequently asked questions for developers and operators |
| docs/contributing/development.md | CREATE | Makefile, CONTRIBUTING.md, docker-compose.yml | Development environment setup and build guide |
| docs/contributing/destination-onboarding.md | CREATE | router/batchrouter/asyncdestinationmanager/README.md | New destination connector onboarding developer guide |
| docs/contributing/testing.md | CREATE | integration_test/, testhelper/ | Test infrastructure and guidelines |
| README.md | UPDATE | README.md | Add documentation section linking to docs/, update architecture diagram reference, add gap report section |
| CONTRIBUTING.md | UPDATE | CONTRIBUTING.md | Add documentation contribution guidelines section |

### 0.5.2 New Documentation Files Detail

**File: docs/gap-report/index.md**
- Type: Gap Analysis Report
- Source Code: All component directories + refs/segment-docs/
- Sections:
  - Executive Summary (overall parity assessment)
  - Feature Parity Matrix (Segment feature → RudderStack status)
  - Critical Gaps (features requiring implementation)
  - Partial Implementations (features needing enhancement)
  - Sprint Roadmap Summary (link to detailed roadmap)
- Diagrams:
  - Feature parity radar chart (Mermaid)
  - Gap severity heat map
- Key Citations: refs/segment-docs/src/connections/spec/, gateway/openapi.yaml, warehouse/integrations/

**File: docs/architecture/data-flow.md**
- Type: Architecture Documentation
- Source Code: gateway/, processor/, router/, warehouse/, jobsdb/
- Sections:
  - End-to-End Event Pipeline (ingestion → processing → routing → warehouse)
  - Stage 1: Ingestion (Gateway worker pool, auth, validation, batching)
  - Stage 2: Processing (6-stage pipeline with channel orchestration)
  - Stage 3: Real-Time Routing (worker pool, throttling, ordering, retry)
  - Stage 4: Batch Routing (bulk delivery, staging file generation)
  - Stage 5: Warehouse Loading (7-state upload machine, schema evolution)
  - Supporting Flows (config, dedup, suppression, archival, schema forwarding)
- Diagrams:
  - Sequence diagram: Event lifecycle from SDK to warehouse
  - Flowchart: Processor 6-stage pipeline
  - State diagram: Warehouse upload state machine
  - Flowchart: Router retry and backoff logic
- Key Citations: processor/pipeline_worker.go, warehouse/router/state.go, router/handle.go, gateway/handle.go

**File: docs/guides/operations/capacity-planning.md**
- Type: Operations Guide
- Source Code: config/config.yaml, gateway/throttler/, router/throttler/, router/worker_buffer_calculator.go
- Sections:
  - Target Throughput (50k events/sec with ordering guarantees)
  - Gateway Tuning (web workers, DB writers, batch sizes, rate limiting)
  - Processor Tuning (transform batch sizes, worker partitions)
  - Router Tuning (workers, GCRA throttling, retry windows, buffer sizing)
  - Warehouse Tuning (parallel loads, backoff, worker counts)
  - Deployment Scaling (GATEWAY/PROCESSOR split for horizontal scaling)
  - Monitoring and Alerting (Prometheus metrics, stats instrumentation)
- Key Citations: config/config.yaml, router/config.go, processor/partition_worker.go

### 0.5.3 Documentation Files to Update Detail

- **README.md** — Add documentation section and gap report references
  - New sections: Documentation link section pointing to `docs/` directory
  - Updated sections: Architecture section with links to detailed docs
  - Added content: Gap report summary and link to `docs/gap-report/index.md`
  - Updated: Table of contents

- **CONTRIBUTING.md** — Add documentation contribution guidelines
  - New sections: Documentation contribution workflow
  - Added content: Documentation style guide reference, how to contribute docs, documentation PR requirements

### 0.5.4 Cross-Documentation Dependencies

- **Shared terminology:** `docs/reference/glossary.md` serves as the canonical terminology source referenced by all other documents
- **Navigation links:** All gap report files cross-reference each other and link to corresponding architecture/guide docs
- **Architecture docs:** Referenced by all guide and reference documents as prerequisite reading
- **Event spec docs:** Referenced by migration guides, SDK guides, and gap report
- **Config reference:** Referenced by all operational guides and capacity planning documentation
- **Warehouse connector guides:** Each references the shared `docs/warehouse/overview.md`, `docs/warehouse/schema-evolution.md`, and `docs/warehouse/encoding-formats.md`

## 0.6 Dependency Inventory

### 0.6.1 Documentation Dependencies

The following documentation tools and packages are relevant to this documentation exercise. All versions are derived from the repository's dependency manifests or verified against the codebase.

| Registry | Package Name | Version | Purpose |
|----------|--------------|---------|---------|
| go.mod | Go (runtime) | 1.26.0 | Primary application language; code examples and API extraction |
| Docker Hub | postgres | 15-alpine | PostgreSQL database used by JobsDB; documented in docker-compose.yml |
| Docker Hub | rudder-transformer | latest | External Transformer service; documented in docker-compose.yml |
| go.mod | cloud.google.com/go/bigquery | 1.72.0 | BigQuery SDK for warehouse connector documentation |
| go.mod | github.com/aws/aws-sdk-go-v2 | 1.41.1 | AWS SDK for Redshift, S3, Kinesis connector documentation |
| go.mod | github.com/ClickHouse/clickhouse-go | 1.5.4 | ClickHouse driver for warehouse connector documentation |
| go.mod | github.com/apache/pulsar-client-go | 0.18.0 | Pulsar client for schema forwarding documentation |
| go.mod | google.golang.org/grpc | (from go.mod) | gRPC framework for warehouse API documentation |
| go.mod | google.golang.org/protobuf | (from go.mod) | Protobuf for proto service definition documentation |
| npm (refs) | jekyll | (from Gemfile) | Jekyll static site generator used by refs/segment-docs |
| npm (refs) | webpack | (from package.json) | Webpack bundler used by refs/segment-docs |
| proto | protoc-gen-go | 1.33.0 | Protobuf Go code generator (from Makefile) |
| proto | protoc-gen-go-grpc | 1.3.0 | gRPC Go code generator (from Makefile) |
| OpenAPI | OpenAPI Specification | 3.0.3 | API spec version used in gateway/openapi.yaml |
| Diagram | Mermaid | latest | Diagram rendering tool for documentation visualizations |

### 0.6.2 Documentation Reference Updates

Documentation files requiring link updates after the new documentation structure is created:

- **README.md** — Add links to new `docs/` directory structure
  - Add: `[Documentation](docs/README.md)` in the navigation section
  - Add: `[Gap Report](docs/gap-report/index.md)` in the project overview
  - Add: `[Architecture](docs/architecture/overview.md)` under Architecture section
  - Add: `[API Reference](docs/api-reference/index.md)` in relevant sections

- **CONTRIBUTING.md** — Add links to documentation contribution resources
  - Add: `[Documentation Guidelines](docs/contributing/development.md)` under contribution types
  - Add: `[Destination Onboarding](docs/contributing/destination-onboarding.md)` under integration contribution

- **Link transformation rules:**
  - All internal documentation links use relative paths from the `docs/` root
  - Cross-references between gap report files use relative paths: `[Event Spec Parity](./event-spec-parity.md)`
  - Architecture references from guides use relative upward navigation: `[Architecture Overview](../../architecture/overview.md)`
  - External links to RudderStack documentation site preserved as-is: `https://www.rudderstack.com/docs/`
  - Segment documentation references cite the local mirror: `Source: refs/segment-docs/src/connections/spec/track.md`

## 0.7 Coverage and Quality Targets

### 0.7.1 Documentation Coverage Metrics

**Current Coverage Analysis:**

| Documentation Domain | Documented | Total | Coverage |
|---------------------|------------|-------|----------|
| Public HTTP API endpoints | 1 (OpenAPI spec) | 15+ endpoints | ~7% |
| Core pipeline components (Gateway, Processor, Router, Batch Router, Warehouse) | 0 architecture docs | 5 components | 0% |
| Warehouse connectors | 0 connector guides | 9 connectors | 0% |
| Stream destination integrations | 0 integration guides | 13 stream destinations | 0% |
| Configuration parameters | ~10 (inline comments) | 200+ parameters | ~5% |
| Infrastructure components (JobsDB, Backend Config, Control Plane, Runner) | 0 docs | 4 components | 0% |
| Supporting services (19 service packages) | 1 (OAuth README) | 19 services | ~5% |
| Enterprise features | 0 docs | 4 sub-modules | 0% |
| Event spec calls (identify, track, page, screen, group, alias) | 1 (OpenAPI) | 6 event types | ~17% |
| Operational guides (replay, compliance, capacity) | 0 guides | 4 areas | 0% |

**Target Coverage:**

| Documentation Domain | Target Coverage | Justification |
|---------------------|----------------|---------------|
| Public HTTP API endpoints | 100% | All endpoints must have developer reference docs with examples |
| Core pipeline components | 100% | Architecture docs required for all 5 pipeline components |
| Warehouse connectors (Snowflake, BigQuery, Redshift) | 100% | Priority connectors per user requirements — full guides with setup, config, troubleshooting |
| Warehouse connectors (remaining 6) | 100% | Complete connector documentation for all supported warehouses |
| Stream destination integrations | 100% | All stream destinations require configuration guides |
| Configuration parameters | 100% | All 200+ parameters documented in reference guide |
| Event spec calls | 100% | Full Segment Spec parity documentation for all 6 event types |
| Gap report (Segment parity) | 100% | Complete gap analysis across all 8 parity dimensions |
| Operational guides | 100% | All operational areas documented for production readiness |

**Coverage Gaps to Address:**

| Area | Current | Target | Priority |
|------|---------|--------|----------|
| Gap Report (Segment Parity) | 0% | 100% | Critical — initial run deliverable |
| Event Spec API Reference | 17% | 100% | Critical — core Segment compatibility |
| Pipeline Architecture Docs | 0% | 100% | Critical — foundation for all other docs |
| Warehouse Connector Guides | 0% | 100% | High — key differentiator |
| Transformation Developer Guide | 0% | 100% | High — core developer workflow |
| Migration Guide | 0% | 100% | High — Segment switching enablement |
| Configuration Reference | 5% | 100% | High — operational necessity |
| Identity Resolution Docs | 0% | 100% | Medium — gap analysis dependency |
| Protocols/Governance Docs | 0% | 100% | Medium — gap analysis dependency |

### 0.7.2 Documentation Quality Criteria

**Completeness Requirements:**
- All public API endpoints have descriptions, request/response schemas, authentication requirements, and curl examples
- All architecture documents include Mermaid diagrams showing component relationships and data flows
- All warehouse connector guides include prerequisites, setup steps, configuration parameters, schema management, troubleshooting, and performance tuning
- All gap report sections include feature comparison matrices with Segment reference links, current RudderStack status, and remediation recommendations
- All operational guides include monitoring commands, log interpretation, and failure recovery procedures

**Accuracy Validation:**
- API payload schemas must match the current `gateway/openapi.yaml` specification (OpenAPI 3.0.3)
- Configuration parameter defaults must match values in `config/config.yaml` (verified against codebase)
- Event spec definitions must be cross-validated against both `gateway/types.go` and Segment spec in `refs/segment-docs/src/connections/spec/`
- Warehouse connector configuration parameters must match integration source code in `warehouse/integrations/*/`
- Architecture diagrams must reflect the actual component wiring in `runner/runner.go` and deployment types in `app/app.go`

**Clarity Standards:**
- Technical accuracy with language accessible to senior engineers and data engineering teams
- Progressive disclosure: overview → detailed explanation → API reference → code examples
- Consistent terminology from the unified glossary (`docs/reference/glossary.md`)
- Every code example includes language annotation and source file citation
- Tables for all parameter references, comparison matrices, and configuration options

**Maintainability:**
- Source citations for traceability: every technical claim references a specific file path and line range
- Clear structure enabling incremental updates as gaps are closed
- Template-based consistency across all warehouse connector guides and event spec docs
- Cross-references between related documents for navigability

### 0.7.3 Example and Diagram Requirements

| Requirement | Target | Documentation Area |
|-------------|--------|-------------------|
| Minimum code examples per API endpoint | 2 (curl + SDK) | API Reference |
| Minimum code examples per warehouse connector | 3 (setup, query, troubleshoot) | Warehouse Guides |
| Mermaid architecture diagrams | 12+ | Architecture docs |
| Mermaid sequence diagrams | 6+ | Data flow, identity, replay |
| Mermaid state diagrams | 2+ | Warehouse state machine, cluster state |
| Feature comparison tables | 8 | Gap report sections |
| Configuration parameter tables | 10+ | Config reference, operational guides |
| Payload schema examples | 12+ (2 per event type) | Event spec reference |

**Diagram Types Required:**
- Flowcharts: System architecture, deployment topologies, data flows
- Sequence diagrams: Event lifecycle, identity resolution, OAuth flow
- State diagrams: Warehouse upload state machine, cluster mode transitions
- Class diagrams: Destination connector hierarchy, service dependency graph
- Gantt-style: Sprint roadmap for gap closure (in gap report)

**Code Example Validation:**
- All curl examples must reference correct Gateway port (8080) and authentication headers
- All Go code examples must compile with Go 1.26.0
- All JavaScript transformation examples must be valid for the Transformer service
- All configuration examples must use parameters from `config/config.yaml` with correct types and ranges

## 0.8 Scope Boundaries

### 0.8.1 Exhaustively In Scope

**New Documentation Files:**
- `docs/gap-report/**/*.md` — All Segment parity gap analysis documentation (9 files)
- `docs/architecture/**/*.md` — All system architecture documentation (7 files)
- `docs/api-reference/**/*.md` — All API reference documentation including event spec sub-directory (11 files)
- `docs/guides/getting-started/**/*.md` — All getting-started and onboarding documentation (3 files)
- `docs/guides/migration/**/*.md` — All Segment-to-RudderStack migration documentation (2 files)
- `docs/guides/sources/**/*.md` — All source SDK integration guides (4 files)
- `docs/guides/destinations/**/*.md` — All destination integration guides (4 files)
- `docs/guides/transformations/**/*.md` — All transformation and Functions documentation (4 files)
- `docs/guides/governance/**/*.md` — All tracking plan and consent governance documentation (4 files)
- `docs/guides/identity/**/*.md` — All identity resolution and profiles documentation (2 files)
- `docs/guides/operations/**/*.md` — All operational guides (4 files)
- `docs/warehouse/**/*.md` — All warehouse connector and service documentation (12 files)
- `docs/reference/**/*.md` — All reference documentation (4 files)
- `docs/contributing/**/*.md` — All developer contribution documentation (3 files)
- `docs/README.md` — Documentation landing page and navigation

**Documentation File Updates:**
- `README.md` — Add documentation section linking to docs/ and gap report
- `CONTRIBUTING.md` — Add documentation contribution guidelines

**Documentation Assets:**
- Mermaid diagrams embedded within Markdown files (no separate image assets required)
- Code examples embedded within documentation files (Go, JavaScript, Python, curl)

**Source Reference Materials:**
- `refs/segment-docs/src/**` — Segment documentation reference (read-only, used as comparison baseline)
- `gateway/openapi.yaml` — OpenAPI 3.0.3 specification (read-only, used as API extraction source)
- `config/config.yaml` — Configuration parameter reference (read-only, used as config extraction source)
- `config/sample.env` — Environment variable reference (read-only, used as env var extraction source)
- `proto/**/*.proto` — Protocol buffer definitions (read-only, used as gRPC API extraction source)

### 0.8.2 Explicitly Out of Scope

**Source Code Modifications:**
- No modifications to Go source code files (`.go` files) — this is a documentation-only effort
- No modifications to test files (`*_test.go`) — testing documentation is created but test code is not modified
- No addition of Go doc comments or inline code documentation — unless explicitly requested in a future phase

**Feature Development:**
- No implementation of missing Segment features identified in the gap report — the gap report documents gaps for future implementation phases
- No code changes to close identified parity gaps — implementation is deferred to subsequent autonomous runs
- No destination connector code additions — only documentation of existing and missing connectors
- No transformation framework code changes — only documentation of current capabilities and gaps

**Excluded Segment Features (Phase 1):**
- Segment Engage/Campaigns documentation — explicitly excluded per user instructions
- Reverse ETL documentation — explicitly excluded per user instructions

**Infrastructure Changes:**
- No documentation deployment pipeline setup (mkdocs, Docusaurus, Sphinx configuration) — documentation is authored as static Markdown files
- No CI/CD pipeline modifications for documentation builds
- No Docker/Kubernetes configuration changes
- No database schema modifications

**Unrelated Documentation:**
- No documentation for external repositories (rudder-transformer, client SDKs, Helm charts)
- No documentation for the Control Plane UI (React.js frontend)
- No documentation for RudderStack Cloud managed service infrastructure
- No third-party tool documentation (only references to RudderStack's usage of tools)

## 0.9 Execution Parameters

### 0.9.1 Documentation-Specific Instructions

| Parameter | Value |
|-----------|-------|
| **Default format** | Markdown (.md) with Mermaid diagrams |
| **Diagram tool** | Mermaid (embedded in Markdown via triple-backtick mermaid blocks) |
| **Code example languages** | Go 1.26.0, JavaScript (ES6+), Python 3.x, curl, Bash |
| **Citation requirement** | Every technical section must reference source files with path and line range |
| **Style guide** | Follow existing RudderStack documentation style (README.md, services/oauth/README.md, router/batchrouter/asyncdestinationmanager/README.md) |
| **Terminology source** | Unified glossary combining RudderStack terms and Segment terms |
| **API spec baseline** | gateway/openapi.yaml (OpenAPI 3.0.3) |
| **Segment reference baseline** | refs/segment-docs/src/ (complete Segment documentation mirror) |
| **Gap analysis methodology** | Feature-by-feature comparison against Segment documentation catalog |

**Documentation Build and Preview:**
- No documentation build system configured — documents are static Markdown files
- Preview via any Markdown renderer (VS Code, GitHub, GitLab)
- Mermaid diagrams render natively in GitHub/GitLab Markdown and VS Code with Mermaid extension
- No documentation deployment pipeline — files are committed to the `docs/` directory in the repository

**Documentation Validation:**
- Markdown lint: Validate Markdown formatting consistency across all files
- Link checking: Verify all internal cross-references resolve correctly within the `docs/` hierarchy
- Code example validation: Ensure all curl examples reference correct ports (8080 for Gateway, 8082 for Warehouse, 9090 for Transformer) and authentication headers
- Terminology consistency: Verify all documents use terms from `docs/reference/glossary.md`
- Diagram validation: Verify all Mermaid diagrams render correctly without syntax errors

## 0.10 Rules for Documentation

The following rules and directives are explicitly derived from user requirements and must be observed throughout all documentation generation:

- **Segment behavioral parity is the acceptance criterion:** All Segment Spec events (track, identify, page, screen, group, alias) must route and transform identically to Segment behavior — documentation must validate and annotate this parity at the payload field level, including common fields and context objects
- **Destination payload parity is mandatory:** Destination connectors must maintain payload parity with Segment's connector output — documentation must include payload comparison schemas showing field-by-field equivalence
- **Throughput constraint must be documented:** Pipeline must sustain 50,000 events/second with ordering guarantees — capacity planning documentation must specify worker pool sizes, batch sizes, and configuration parameters required to achieve this throughput
- **Warehouse idempotency and backfill are non-negotiable:** Warehouse sync must be idempotent and support backfill — documentation must specify merge strategies (append vs. merge/dedup), staging file handling, and failure recovery procedures for each supported warehouse
- **Gap Report is an initial-run deliverable:** The Gap Report and sprint roadmap must be self-contained, actionable documents that can be consumed independently to drive autonomous implementation
- **Follow existing documentation style:** All new documentation must follow the established patterns observed in `services/oauth/README.md` (comprehensive architecture with component breakdowns) and `router/batchrouter/asyncdestinationmanager/README.md` (developer onboarding with architecture diagrams, interfaces, and examples)
- **Include Mermaid diagrams for all architectural and workflow documentation:** Every architecture document, data flow guide, and complex workflow must include at least one Mermaid diagram
- **Provide source code citations for all technical details:** Every technical claim, configuration default, and API specification must include a citation in the format `Source: /path/to/file.go:LineRange`
- **Document all configuration options in table format:** Configuration parameters must be presented in tables with columns for parameter name, default value, type, acceptable range, and description
- **Maintain minimal changes to existing documentation:** Updates to `README.md` and `CONTRIBUTING.md` should add sections without modifying existing content structure
- **Use consistent terminology from the unified glossary:** All documentation must use terms defined in `docs/reference/glossary.md`, cross-mapping Segment terminology to RudderStack equivalents where applicable
- **Phase 1 exclusions are strict:** Segment Engage/Campaigns and Reverse ETL must not be documented in this phase — these areas should be mentioned only as "Phase 2" items in the gap report
- **Documentation must be developer-audience focused:** Target audience is senior engineers and data engineering teams — avoid marketing language, prioritize technical precision and code examples

## 0.11 References

### 0.11.1 Repository Files and Folders Searched

The following files and directories were searched and analyzed to derive the conclusions documented in this Agent Action Plan:

**Root-Level Files Inspected:**
- `README.md` — Project overview, key features, architecture summary, setup instructions, licensing
- `CONTRIBUTING.md` — Contribution guidelines, CLA requirements, PR submission process
- `CHANGELOG.md` — Release history, current version v1.68.1 (2026-02-18)
- `go.mod` — Go 1.26.0 module definition, dependency declarations and version pins
- `docker-compose.yml` — Runtime topology (5 services: db, backend, transformer, minio, etcd)
- `Dockerfile` — Multi-stage build definition, Go 1.26.0-alpine3.23 builder image, CGO_ENABLED=0
- `Makefile` — Build workflows, protoc-gen-go v1.33.0, protoc-gen-go-grpc v1.3.0
- `config/config.yaml` — Master configuration (200+ parameters across all subsystems)
- `config/sample.env` — Environment variable reference with inline documentation

**Core Pipeline Directories Inspected:**
- `gateway/` — HTTP ingestion gateway (25+ files including handlers, auth, throttler, validator, webhook, openapi)
- `processor/` — Event processing pipeline (24+ files including pipeline_worker, partition_worker, consent, trackingplan, transformer)
- `router/` — Real-time destination routing (30+ files including handle, worker, network, factory, throttler, batchrouter, customdestinationmanager)
- `router/batchrouter/` — Batch routing subsystem (includes asyncdestinationmanager with comprehensive README)
- `warehouse/` — Warehouse loading service (22+ sub-packages including router, integrations, schema, slave, encoding, identity, api, archive)
- `warehouse/integrations/` — 9 warehouse connectors (snowflake, bigquery, redshift, clickhouse, deltalake, postgres, mssql, azure-synapse, datalake) plus shared layers (manager, middleware, tunnelling, types, config)

**Infrastructure Directories Inspected:**
- `jobsdb/` — Persistent job queue (27+ files covering partitioning, migration, priority pools, pending events, caching)
- `backend-config/` — Dynamic workspace configuration (16+ files covering single-workspace, namespace, dynamic config, replay types, encrypted cache)
- `controlplane/` — gRPC-based remote configuration
- `runner/` — Central lifecycle orchestrator

**Service Directories Inspected:**
- `services/` — 19 shared service packages (streammanager, dedup, oauth, transformer, fileuploader, geolocation, debugger, diagnostics, kvstoremanager, notifier, alert, alerta, archiver, rmetrics, rsources, sql-migrator, controlplane, transientsource, validators)
- `enterprise/` — Enterprise features (reporting, suppress-user, trackedusers, config-env, LICENSE)
- `archiver/` — Event archival system (5 files covering worker, options, lifecycle, tests)
- `regulation-worker/` — GDPR regulation enforcement (internal packages for destination, model, service, client, delete plus cmd)

**Reference Documentation Inspected:**
- `refs/segment-docs/` — Complete Segment documentation mirror (Jekyll-based static site)
- `refs/segment-docs/src/connections/spec/` — Segment Spec definitions (identify, track, page, screen, group, alias, common)
- `refs/segment-docs/src/connections/destinations/` — Segment destination catalog
- `refs/segment-docs/src/connections/sources/` — Segment source catalog
- `refs/segment-docs/src/connections/functions/` — Segment Functions documentation
- `refs/segment-docs/src/protocols/` — Segment Protocols/Tracking Plans documentation
- `refs/segment-docs/src/unify/` — Segment Unify/Identity Resolution documentation
- `refs/segment-docs/src/connections/storage/` — Segment warehouse storage documentation

**Existing Documentation Files Inspected:**
- `cmd/devtool/README.md` — Developer tool CLI usage
- `services/oauth/README.md` — OAuth module architecture and components
- `router/batchrouter/asyncdestinationmanager/README.md` — Async destination manager architecture and onboarding
- `regulation-worker/README.md` — Environment variable checklist
- `warehouse/.cursor/docs/snowpipe-streaming.md` — Snowpipe Streaming internal documentation
- `warehouse/.cursor/docs/staging-file-flow.md` — Staging file pipeline internal documentation

**Proto Definitions Inspected:**
- `proto/cluster/` — Cluster partition migration streaming RPC
- `proto/common/` — DPAuth service credential distribution
- `proto/event-schema/` — Event schema key/message types
- `proto/warehouse/` — Warehouse service with 15 unary RPCs

**Other Directories Inspected:**
- `app/` — Application type definitions and deployment handlers
- `admin/` — Admin RPC server
- `cmd/` — CLI tools (devtool, rudder-cli)
- `internal/` — Internal shared utilities
- `middleware/` — HTTP middleware (gzip, semaphore)
- `.github/` — GitHub templates (issue, PR)

### 0.11.2 Tech Spec Sections Referenced

The following technical specification sections were retrieved and analyzed for context:

| Section | Title | Key Information Extracted |
|---------|-------|--------------------------|
| 1.1 | Executive Summary | Project overview, v1.68.1 release, Go 1.26.0, Elastic License 2.0, stakeholder groups, value propositions |
| 1.2 | System Overview | Project context, high-level architecture, deployment types (EMBEDDED/GATEWAY/PROCESSOR), success criteria, KPIs |
| 1.3 | Scope | In-scope features (14 categories), essential integrations (9 warehouse + 13 stream destinations), data domains (6), out-of-scope exclusions |
| 2.1 | Feature Catalog Overview | 19 features across 7 categories, priority distribution (7 Critical, 8 High, 4 Medium) |
| 2.2 | Core Data Pipeline Features | Detailed specifications for F-001 through F-005 (Gateway, Processor, Router, Batch Router, Warehouse) with sub-features, dependencies, and functional requirements |
| 2.5 | Privacy and Compliance Features | F-006 (Data Regulation) and F-017 (Suppression Backup Service) with GDPR compliance details |
| 3.1 | Programming Languages | Go 1.26.0 primary language, Protocol Buffers proto3, Shell/Bash operational scripts |
| 5.1 | High-Level Architecture | Durable pipeline architecture, system boundaries, data flow stages, external integration points |
| 6.1 | Core Services Architecture | Modular monolith classification, 5 pipeline + 4 infrastructure + 19 supporting components, inter-service communication patterns, scalability design, resilience patterns |

### 0.11.3 Attachments and External References

No attachments were provided for this project.

**External Documentation References (from repository):**
- RudderStack Documentation Site: `https://www.rudderstack.com/docs/`
- RudderStack Data Learning Center: `https://www.rudderstack.com/learn/`
- RudderStack Cloud Signup: `https://app.rudderstack.com/signup?type=freetrial`
- RudderStack Slack Community: `https://www.rudderstack.com/join-rudderstack-slack-community/`
- Go Report Card: `https://goreportcard.com/report/github.com/rudderlabs/rudder-server`
- Segment Documentation (referenced via local mirror): `refs/segment-docs/`

