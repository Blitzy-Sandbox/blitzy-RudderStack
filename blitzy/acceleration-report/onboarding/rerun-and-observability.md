# Clean-Machine Rerun and Observability — blitzy-acceleration-report Workspace

*Analyst-onboarding document for the `blitzy/acceleration-report/` measurement pipeline. Enables a clean-machine rerun without questions.*

---

## 1. Welcome

Welcome to the `blitzy/acceleration-report/` measurement workspace. This document is your single source of truth for re-running the acceleration-measurement pipeline end-to-end. By the time you finish reading, you should be able to clone the repository onto a clean machine, install prerequisites, run `make all`, run `make verify`, and have a freshly-rendered `acceleration-report.md` plus `executive-summary.html` on disk. If anything below is unclear, the canonical reference documents are: [README.md](../README.md) for the workspace overview, [decision-log.md](../decision-log.md) for non-trivial decisions and their rationale, and [acceleration-report.md](../acceleration-report.md) for the rendered measurement deliverable.

---

## 2. Prerequisites (Clean-Machine Setup)

The pipeline runs on Linux and macOS. Windows is supported under WSL2. The bullets below enumerate every binary and credential the pipeline consults, separated into REQUIRED (the pipeline will not run without them) and OPTIONAL (the pipeline degrades to a documented fallback when absent).

### Required

- **Python 3.12 or later** — Verify with `python3 --version`. Install via:
  - macOS: `brew install python@3.12`
  - Debian/Ubuntu: `sudo apt install python3.12 python3.12-venv`
  - Via pyenv: `pyenv install 3.12.3 && pyenv global 3.12.3`
- **git 2.43.0 or later** — Verify with `git --version`. Install via:
  - macOS: `brew install git`
  - Debian/Ubuntu: `sudo apt install git`
- **bash 5.0 or later** — Verify with `bash --version`. **macOS users**: the system `/bin/bash` is version 3.2. Install bash 5.x via `brew install bash` and ensure `/opt/homebrew/bin` (Apple Silicon) or `/usr/local/bin` (Intel) appears in your `PATH` BEFORE `/bin`.
- **make** — Verify with `make --version`. Pre-installed on macOS with Xcode Command Line Tools; install via `sudo apt install make` on Debian/Ubuntu.

### Optional

- **GitHub Personal Access Token (`GH_TOKEN`)** — Required for higher API rate limits (5,000 requests/hour authenticated vs. 60/hour unauthenticated). Generate at https://github.com/settings/tokens with scopes:
  - `repo:read` (for reading PRs, commits, releases, issues)
  - `actions:read` (for reading workflow runs and artifacts)
  - Provide via the `.env` file: `GH_TOKEN=<github-personal-access-token>` (NEVER paste the literal token value into a tracked file; use a placeholder of the form `<github-personal-access-token>` only, per the CP2 review Security Hygiene finding. The literal value is loaded at runtime from your `.env` which is gitignored.)
  - When absent, the pipeline runs in offline mode using only local-git data; confidence tiers may drop accordingly. Risk Assessment in the rendered report documents the degradation.
- **Linear API key (`LINEAR_API_KEY`)** — Required for Metric 6 issue-label classification and Metric 12 SLA-tier resolution. Generate at https://linear.app/settings/api.
  - When absent, Metric 12 reports "Insufficient signal — no SLA source" and Metric 6 falls back to conventional-commit-prefix classification only.
- **(Optional) jq 1.6+** — Helpful for pretty-printing `data/*.json` files. When absent, scripts fall back to Python `json.tool`. Install via `brew install jq` (macOS) or `sudo apt install jq` (Debian/Ubuntu).
- **(Optional) Node.js 18+ with `@mermaid-js/mermaid-cli`** — Only required if you wish to render `diagrams/*.mmd` to SVG locally for offline review. Install via `npm install -g @mermaid-js/mermaid-cli`. Not required for any extraction, compute, or render step.

The workspace ships a template at [`../.env.example`](../.env.example) listing every supported environment variable with safe placeholders.

---

## 3. Clone-and-Setup

This section walks through the exact steps to take a clean machine from "nothing installed" to "ready to run `make all`". Every command is read-only with respect to the analyzed repository (no commits, pushes, or external system writes). Section 4 below shows the one-command rerun that uses the workspace built here.

**Step 1 — Clone the repository.** The analyzed repository lives at `Blitzy-Sandbox/blitzy-RudderStack` on GitHub. Clone via HTTPS (no special credentials needed for read-only clone of the public-or-org-readable repo) [`catalog-info.yaml:metadata.annotations.github.com/project-slug`]:

```bash
git clone https://github.com/Blitzy-Sandbox/blitzy-RudderStack.git
cd blitzy-RudderStack
```

Alternatively, if you have SSH set up, `git clone git@github.com:Blitzy-Sandbox/blitzy-RudderStack.git`. The analysis pipeline reads only the local clone's git history (`.git/`) plus the in-repo workflow and config files; nothing is pushed back to the remote.

**Step 2 — Check out the working branch.** The CP2 work lives on the per-session feature branch. To re-run the same analysis on the exact same state, check out the matching branch (e.g., `git checkout blitzy-721d7d10-a0c0-47d3-b010-cfb636cb8bd8`). To re-run against the latest `main`, stay on `main` (the default after clone) [`blitzy/acceleration-report/data/environment.json:default_branch`].

**Step 3 — Enter the analysis workspace.**

```bash
cd blitzy/acceleration-report
```

This is the workspace root for everything that follows. The analyzed repository's source tree (`admin/`, `app/`, `gateway/`, … `warehouse/`) is read-only context for the extraction scripts; the workspace files (`scripts/`, `data/`, `diagrams/`, `onboarding/`, this document) are the analysis pipeline.

**Step 4 — Create and activate the Python virtual environment.** The workspace ships a pinned `requirements.txt` [`blitzy/acceleration-report/requirements.txt`] with 7 dependencies (requests, python-dateutil, tzdata, tabulate, jinja2, jsonschema, gql). The `setup` Makefile target wraps this in one command:

```bash
make setup
```

Behind the scenes, `make setup` runs `python3 -m venv .venv && .venv/bin/pip install -r requirements.txt`. On Ubuntu 24+ systems where `python3 -m venv` may fail because of a broken `ensurepip` (the system Python ships without `pip` and you encounter `error: externally-managed-environment`), the fallback is `virtualenv --python=python3.13 .venv && ./.venv/bin/pip install -r requirements.txt`.

**Step 5 — Populate the `.env` file with optional secrets.**

```bash
cp .env.example .env
# Edit .env with your editor; uncomment and fill in GH_TOKEN and LINEAR_API_KEY if desired.
```

The `.env.example` template [`blitzy/acceleration-report/.env.example`] documents the safe placeholder syntax for every supported environment variable. NEVER paste a literal token value; use the `<github-personal-access-token>` and `<linear-api-key>` placeholders and load the actual values via your shell environment. The `.env` file itself is gitignored, so committing your local `.env` is impossible by accident.

**Step 6 — Run the readiness preflight (recommended).**

```bash
DRY_RUN=1 make extract
```

This invokes every extraction script with the `--dry-run` flag, printing the exact set of HTTP endpoints and git commands the pipeline would issue, then exiting without performing them. The dry-run is the analytics equivalent of a readiness gate (see Section 8.3 below) and validates that every required tool and token is configured before the actual run.

After Step 6 succeeds with zero errors, jump to Section 4 (One-Command Rerun) below.

---

## 4. One-Command Rerun

After Section 2 prerequisites are installed, the complete pipeline runs in four commands:

```bash
cd blitzy/acceleration-report
cp .env.example .env       # Edit .env to add GH_TOKEN and LINEAR_API_KEY (both optional)
make all                   # Runs setup -> extract -> compute -> render
make verify                # Re-applies all rule checks
```

What each step does:

- `make setup` (invoked transitively by `make all`) — Creates `.venv/` and installs the [`../requirements.txt`](../requirements.txt) pinned versions.
- `make extract` (invoked transitively by `make all`) — Runs extraction scripts 00 through 08 in topological order; writes `data/*.json` and `data/*.csv` artifacts.
- `make compute` (invoked transitively by `make all`) — Reads raw data artifacts and writes `data/metrics.json` plus `data/per_engineer.json`.
- `make render` (invoked transitively by `make all`) — Reads computed metrics and writes `acceleration-report.md` plus `executive-summary.html`.
- `make verify` — Re-applies all rule checks (Rule 1 provenance, Rule 2 factual-neutral tone, Rule 3 confidence transparency, Rule 4 internal consistency, Rule 5 reproducibility, Rule 6 environment-first); exits non-zero on any failure.

After `make all` completes, the deliverables appear at:

- `acceleration-report.md` — canonical 11-section measurement report
- `executive-summary.html` — single self-contained reveal.js deck (open in browser)
- `decision-log.md` — already exists; lists non-trivial decisions
- `data/metrics.json` — single source of truth feeding both renderers
- `data/run.log.jsonl` — structured JSON log feed for the pipeline run

The complete target catalogue lives in the workspace [`../Makefile`](../Makefile); read it for advanced targets such as `make clean`, `make lint`, and per-stage invocation.

---

## 5. Domain Context (rudder-server)

This section summarizes the analyzed system at a level sufficient to interpret the metrics. The two long-form references that ground this summary are `blitzy/documentation/Project Guide.md` (operational status and coordination record across 25 epics) and `blitzy/documentation/Technical Specifications.md` (sequenced engineering plan and authoritative design reference). Read those if you need deeper context than this section provides — but you do NOT need to read them to re-run the pipeline.

`rudder-server` is the open-source customer data platform (CDP) at the heart of RudderStack. It is a Go monorepo (`go 1.26.1` per `go.mod`) that ingests events from SDKs, applies transformations, and routes events to 150+ downstream destinations and 9 warehouse connectors. The analyzed repository is a fork hosted under the `Blitzy-Sandbox` GitHub organisation, derived from the upstream `rudderlabs/rudder-server` v1.68.1 baseline.

The repository under analysis represents the **Blitzy-assisted parity effort** to close feature gaps against Segment across five dimensions: destination connectors, functions/transformations, protocols enforcement, identity resolution, and operational tooling. The effort comprises 25 epics (E-010 through E-039) organized into five sprint clusters. The AI inflection date — when Blitzy was introduced into the codebase — is `2026-02-25 02:58:59 UTC`, derived via Tier 2 (AI-actor email pattern) detection (Tier 1 trailer search returned no hits in the local clone). See [`decision-log.md`](../decision-log.md) entry `DL-002` for the detection-method choice.

The repository has a single default branch `main` plus **8 `blitzy-*` feature branches** (one per Blitzy sandbox session, named `blitzy-<UUID>`) and `release/*` branches for the release-please workflow. The `blitzy-*` prefix convention is the canonical Blitzy-PR marker referenced by Metric 1 (Flow Load) in-progress detection.

The repository contains 497 `*_test.go` files declaring 859+ test functions and 749+ sub-tests across 29 Ginkgo suites. There are 57 mockgen directives and 47 testdata directories. The primary CI test pipeline is `.github/workflows/tests.yaml`, which executes a 25-job package-unit matrix and a 9-destination warehouse-integration matrix.

The rudder-server emits telemetry in three wire formats: Prometheus client_golang `v1.23.2` (pull HTTP on port `:9102`), StatsD UDP via prom/statsd-exporter `v0.22.4` (port `:9125` to `:9102` bridge), and OpenTelemetry OTLP `v1.40.0`. Structured logging uses `rudder-observability-kit` (obskit) `v0.0.6`. The runtime exposes a `/health` liveness endpoint and six bearer-protected internal endpoints: `/protocols`, `/profiles`, `/monitoring`, `/profiling`, `/alerts`, `/replay`. Alert routing lives under `services/alert/` with a VictorOps default plus PagerDuty, Slack, and Email backends. Section 7 of this document covers the rudder-server observability stack in more detail.

The repository has 13 GitHub Actions workflows under `.github/workflows/`. The key workflows consumed by this measurement are: `tests.yaml` (primary test pipeline; consumed by Metric 11 CI history and Metric 10 required-check identification); `verify.yml` (generate-and-diff with `git diff --exit-code`, plus `golangci-lint v2.9.0`, `gofumpt v0.9.1`, `govulncheck`, and `mockgen v0.6.0`; consumed by Metric 10 for the lint-exemption catalogue); `release-please.yaml` (creates releases on `release/*` branches with `release-type: go` and `bump-minor-pre-major: true`, and dispatches deploy events to `rudderstack-operator` and `rudder-devops`; consumed by Metric 9); and `semantic-pr.yaml` (enforces conventional-commit-typed PR titles with allowed types `fix`, `feat`, `chore`, `refactor`, `exp`, `doc`, `test`; the authority for the Metric 6 conventional-commit category map).

The active engineering-actor roster across `main` is four human authors plus one GitHub App. Per AAP §0.5.3 and [`decision-log.md`](../decision-log.md) entry `DL-003`, the canonical `Blitzy` engineering actor is the UNION of `agent@blitzy.com` (display name `Blitzy Agent`) AND `191547922+blitzy[bot]@users.noreply.github.com` (display name `blitzy[bot]`). Human contributors include `michael@blitzy.com` and `awadhwani@blitzy.com` (display name `ajay-blitzy`). The `dependabot[bot]` account is excluded from Flow Velocity per [`decision-log.md`](../decision-log.md) entry `DL-004`.

---

## 6. Pipeline Stages

The pipeline is a deterministic three-stage extract → compute → render flow. Each stage is independently invocable, idempotent, and produces a checkpointable artifact that downstream stages consume. The separation enforces Rule 1 (Data Provenance) by persisting raw output before any derivation [`blitzy/acceleration-report/scripts/lib/observability.py:_redact_value`], and Rule 4 (Internal Consistency) by ensuring both renderers consume only the compute outputs. The canonical full description lives in [`acceleration-report.md`](../acceleration-report.md) §4 Methodology.

### 6.1 Extract Stage (scripts 00–08)

The extract stage consults nine sources of signal and writes a raw data artifact per source. Scripts are independent; failure of one does not block the others, but each failure is logged with the structured-JSON logger and surfaces in the Risk Assessment of the report.

- `00_environment.sh` — Rule 6 Environment Verification preamble; generates `BLITZY_RUN_ID`; emits `data/environment.json`
- `01_detect_inflection.py` — three-tier AI inflection-point detection (trailer search, AI-actor email pattern, velocity inflection); emits `data/inflection.json`
- `02_extract_commits.sh` — full commit roster plus revert candidates; emits `data/commits.csv` and `data/revert_candidates.csv`
- `03_extract_pulls.py` — GitHub Pulls, Reviews, Commits, and Events APIs; emits `data/pulls.json`, `data/reviews.json`, and `data/pull_events.json`
- `04_extract_releases.py` — GitHub Releases API plus tag scan; emits `data/releases.json`
- `05_extract_reverts.sh` — revert-to-original resolution plus release attribution; emits `data/reverts.json`
- `06_extract_ci_history.py` — GitHub Actions Runs API plus JUnit artifacts; emits `data/ci_runs.json` and `data/test_transitions.json`
- `07_extract_exceptions.py` — audit log, force-pushes, label scan, and lint-config exemptions; emits `data/exceptions.json`
- `08_extract_linear.py` — Linear GraphQL (no-op without `LINEAR_API_KEY`); emits `data/issues.json` and `data/slas.json`

### 6.2 Compute Stage (script 09)

The compute stage is pure (no I/O beyond reading and writing the named files) so that it is exactly reproducible from the data artifacts:

- `09_compute_metrics.py` — deterministic compute step for all 12 metrics plus per-engineer breakdown, temporal phase aggregation, and multi-module aggregation; emits `data/metrics.json` and `data/per_engineer.json` with `{value, confidence, caveat, provenance, boundary_conditions}` fields per metric per phase

### 6.3 Render Stage (scripts 10, 11)

The render stage consumes ONLY the compute outputs — never the raw extraction artifacts directly. This enforces Rule 4 (Internal Consistency) mechanically: both renderers see the same `metrics.json`:

- `10_render_report.py` — renders `acceleration-report.md` from `data/metrics.json`, `data/per_engineer.json`, and `diagrams/*.mmd`; applies the factual-neutral-tone blocklist guard before write
- `11_render_deck.py` — renders `executive-summary.html` from the same data artifacts; uses a Jinja2 template with the inline Blitzy theme and CDN-pinned reveal.js 5.1.0, Mermaid 11.4.0, and Lucide 0.460.0

See [`../diagrams/extraction-pipeline.mmd`](../diagrams/extraction-pipeline.mmd) for the Mermaid topology of this pipeline.

---

## 7. Common Pitfalls

This section lists the failure modes most commonly encountered on first-time runs and their mitigations. The placement of Common Pitfalls BEFORE Observability is intentional: an analyst hitting an issue typically wants the troubleshooting catalogue first, then turns to the observability surfaces to confirm the fix worked. Per the CP2 review item, this section moved from its prior position after observability to its required position before observability.

#### GitHub API Rate Limits

Unauthenticated requests are limited to 60 requests per hour, which is insufficient for the ~538-commit roster plus PR-merge histories plus workflow runs plus releases. With `GH_TOKEN`, the limit rises to 5,000 requests per hour, which is adequate for the full pipeline. The shared `lib/github.py` client implements exponential back-off on `403 Rate Limit Exceeded` responses but may still time out on cold caches [`blitzy/acceleration-report/scripts/lib/github.py`].

**Mitigation**: Set `GH_TOKEN` in `.env` (see Section 2 above). If a run fails with rate-limit errors, rerun `make extract` — the client persists last-success cursors at `data/.cursor.json` so the rerun resumes cleanly from the last successful page.

#### Linear API Unavailable

When `LINEAR_API_KEY` is absent OR the Linear API is unreachable, **Metric 12 (Defects Out of SLA)** reports `"Insufficient signal — no SLA source"` and **Metric 6 (Flow Distribution)** falls back to conventional-commit-prefix classification only (no Linear issue-label classification) [`blitzy/acceleration-report/data/issues.json:unavailable_reason`].

**Mitigation**: Provision `LINEAR_API_KEY` per Section 2 above. When unavailable, this fallback is documented as [`decision-log.md`](../decision-log.md) entry `DL-007` and surfaces in the Risk Assessment section of the rendered report.

#### Admin Audit Log Inaccessible

**Metric 10 (Approved Exceptions)** requires admin audit-log access for the full signal. With a read-only token, only force-pushes (from `git reflog`), label-based exception markers, and HEAD snapshots of `.golangci.yml`, `.snyk`, `.truffleignore`, and `.deepsource.toml` exemptions are available. Confidence drops to Low and a caveat is appended at every appearance [`blitzy/acceleration-report/data/exceptions.json:audit_log.available`].

**Mitigation**: Provision an admin token if higher-fidelity Metric 10 is desired. When unavailable, the degradation is documented as [`decision-log.md`](../decision-log.md) entry `DL-008` and surfaces in the Risk Assessment.

#### History Rewrites (PRs with Force-Pushed-Away First Commits)

PRs whose first commit was force-pushed away cannot be located in the local clone. Metric 7 (Flow Time) reports the exclusion rate; the affected PR numbers appear in `data/pulls.json` with `excluded_reason: "first_commit_unreachable"`.

**Mitigation**: None — this is an artifact of the upstream PR workflow. The exclusion rate is reported transparently per the user's "no fabrication" rule.

#### macOS System Bash (Version 3.2)

The system `/bin/bash` on macOS is version 3.2 (BSD-licensed). The pipeline requires bash 5.0 or later for features such as `[[ -v VAR ]]` existence checks, `${var,,}` lowercase expansion, and associative arrays. Running `make all` with bash 3.2 produces `bad substitution` errors.

**Mitigation**: Install bash 5.x via Homebrew (`brew install bash`) and ensure `/opt/homebrew/bin` (Apple Silicon) or `/usr/local/bin` (Intel) appears in `PATH` BEFORE `/bin`. Verify with `which bash` — it should print the Homebrew path.

#### `jq` Absence

Some pretty-print steps in the documentation and validation scripts use `jq`. When `jq` is not installed, the scripts fall back to `python3 -m json.tool`. The functional behavior is identical; the output formatting may differ slightly.

**Mitigation**: Install `jq` if desired (`brew install jq` or `sudo apt install jq`). It is not required for any extraction, compute, or render step.

#### Submodule Absence

The analyzed rudder-server repository has no git submodules; `git submodule status` returns empty output. This is recorded in `data/environment.json` as `"submodule_state": "no_submodules"` [`blitzy/acceleration-report/data/environment.json:submodule_state`]. If submodules are introduced in the future, the environment snapshot will reflect that change automatically.

**Mitigation**: None — the current state is "no submodules" which the pipeline handles correctly.

#### Python Virtual Environment Not Activated

If you invoke `python scripts/03_extract_pulls.py` directly without activating `.venv/`, you may get `ModuleNotFoundError: No module named 'requests'`. The Makefile invokes scripts via `.venv/bin/python` (referred to as `$(VENV_PY)` in the Makefile) to avoid this; if you bypass the Makefile, activate the venv first.

**Mitigation**: Run scripts via `make extract`, `make compute`, or `make render` (which use `.venv/bin/python`), OR activate the venv before running scripts directly:

```bash
source .venv/bin/activate
python scripts/03_extract_pulls.py
```

#### Clock Skew During Window Boundary Computation

The 2-week windows are anchored to Monday 00:00 UTC. If the host machine's clock is misaligned with UTC (for example, running in a stale Docker container with a frozen clock), the window-end snapshots used for Metric 1 (Flow Load) may misalign. The pipeline does NOT rely on the host clock for any window math — all window boundaries are derived from git and API timestamps which are UTC ISO-8601. The host clock only affects the `extraction_timestamp` field in `data/environment.json`.

**Mitigation**: Ensure `date -u` returns the actual current UTC time on the host.

---

## 8. Observability Surfaces of the Analysis Pipeline

The analysis pipeline is itself observable. Per Rule 1 (Observability, AAP §0.7.1.1), the interpretation for an analytics deliverable comprises four surfaces: a structured JSON log feed (the analytics equivalent of distributed tracing), a pipeline counters summary (the analytics equivalent of a metrics endpoint), a readiness preflight (the analytics equivalent of a health gate), and a dashboard template (the analytics equivalent of a Grafana dashboard).

### 8.1 Structured JSON Log Feed

Every extraction, compute, and render script imports `lib.observability.get_logger(run_id)` and emits single-line JSON events to `data/run.log.jsonl`. Each event carries the schema:

```json
{
  "run_id": "<uuid4>",
  "ts": "<iso8601 utc>",
  "script": "<script filename>",
  "level": "DEBUG | INFO | WARNING | ERROR",
  "event": "<short event name>",
  "...context": "<additional structured fields per event>"
}
```

The `run_id` is propagated via the `BLITZY_RUN_ID` environment variable. It is a UUID4 generated by `00_environment.sh` and exported by the Makefile for every subsequent script invocation, ensuring all events from a single pipeline invocation share the same correlation ID. The logger implementation lives at [`../scripts/lib/observability.py`](../scripts/lib/observability.py).

For live observability during a run, open a second terminal and tail the log:

```bash
tail -F data/run.log.jsonl | jq .
```

When `jq` is not installed, fall back to processing the stream with Python:

```bash
tail -F data/run.log.jsonl | while read -r line; do echo "$line" | python3 -m json.tool; done
```

Sensitive fields are REDACTED by the logger: any key matching `*token*` or `*key*` is replaced with `"[REDACTED]"` before serialization. The `GH_TOKEN` and `LINEAR_API_KEY` values therefore never appear in `data/run.log.jsonl` or in any other committed artifact.

### 8.2 Pipeline Counters Summary

On completion, each script prints a stdout summary block. Examples:

- `03_extract_pulls.py` prints total PRs fetched, total reviews fetched, total events fetched, and exclusion counts (force-pushed PRs, dependency-bot PRs).
- `06_extract_ci_history.py` prints total workflow runs fetched, total artifacts downloaded, and runs by conclusion (`success`, `failure`, `cancelled`).
- `09_compute_metrics.py` prints per-metric value summaries (for example, `M1 baseline X.X, after X.X, multiplier X.Xx, confidence`; `M2 ...`).

These stdout summaries are the analytics-pipeline equivalent of a metrics surface. They are designed to be skimmed at the end of a run for a rapid health check; the structured JSON log feed (Section 6.1) is the source of truth for detailed inspection.

### 8.3 Readiness Preflight (`--dry-run` flag)

Every script supports a `--dry-run` flag that lists every external endpoint and every git command it WOULD invoke, then exits 0 without performing any of them. This is the analytics-pipeline equivalent of a readiness or health gate.

Examples:

```bash
# Single-script dry-run
python scripts/03_extract_pulls.py --dry-run

# Full-pipeline dry-run via env var
DRY_RUN=1 make extract
```

A dry-run produces an output report (to stdout AND to `data/run.log.jsonl` with `event: "dry_run_plan"`) listing:

- every HTTP endpoint pattern the script would call (for example, `GET /repos/{owner}/{repo}/pulls?state=all`)
- every git command the script would invoke (for example, `git log --all --pretty=format:'%H|%aE|%aN|%aI|%cE|%cN|%cI|%P|%s'`)
- every input file the script would read (for example, `data/inflection.json`)
- every output file the script would write (for example, `data/pulls.json`)

Use the dry-run BEFORE running the full extraction on a new machine to validate that all required tooling and tokens are configured.

### 8.4 Dashboard Template

The Mermaid diagram at [`../diagrams/extraction-pipeline.mmd`](../diagrams/extraction-pipeline.mmd) is the analytics-pipeline equivalent of a Grafana dashboard. Render it to SVG via the Mermaid CLI:

```bash
# Install mermaid-cli (one-time)
npm install -g @mermaid-js/mermaid-cli

# Render the dashboard
mmdc -i diagrams/extraction-pipeline.mmd -o /tmp/pipeline.svg
```

The diagram shows: data sources (GitHub APIs, Linear API, local git, in-repo files) → extraction scripts → raw `data/*.json` artifacts → renderer → final deliverables. Reviewing it side-by-side with the actual log feed gives a synoptic view of the pipeline's progress.

---

## 9. rudder-server's Own Observability Stack (Existing, Reused for Context)

Per the Observability rule's "Check if the project already has logging, tracing, metrics, or health checks. Use what exists. Document what you reused and what you added." directive (AAP §0.7.1.1), this section documents the EXISTING observability stack of the analyzed rudder-server. The measurement pipeline does NOT exercise these endpoints; they are READ-ONLY context for analysts who wish to stand up rudder-server locally to gather Metric 11 signal or inspect runtime metrics directly.

### 9.1 Three Wire Formats

- **Prometheus** — scrape endpoint at `http://localhost:9102/metrics` (pull HTTP, `prometheus/client_golang` v1.23.2). Example series names: `gateway_event_latency_seconds`, `router_destination_delivery_total`, `processor_pipeline_duration_seconds`.
- **StatsD** — UDP datagram listener on port `9125`, bridged to Prometheus on `9102` via `prom/statsd-exporter` v0.22.4. Used for legacy compatibility with existing dashboards.
- **OpenTelemetry OTLP** — trace and metric export via `go.opentelemetry.io/otel` v1.40.0. Configurable OTLP receiver endpoint.

### 9.2 Structured Logging

`rudder-observability-kit` (`obskit`) v0.0.6 — the canonical structured logger across the codebase. It provides field-tagged log lines with standardized keys such as `destination_id`, `source_id`, `workspace_id`, and `job_id`.

### 9.3 HTTP Endpoints

- `GET /health` — liveness probe (returns `200 OK` with JSON `{"server":"UP", "db":"UP", "acceptingEvents":"TRUE"}` when healthy). Mounted at `gateway/handle_lifecycle.go`.
- Six bearer-protected internal endpoints (all require Bearer-token auth):
  - `/protocols` — tracking-plan enforcement state
  - `/profiles` — identity-graph profile queries
  - `/monitoring` — per-destination delivery metrics
  - `/profiling` — pipeline performance profiling
  - `/alerts` — alerting-rule state
  - `/replay` — advanced replay control

### 9.4 Alert Routing

`services/alert/` wraps four notification backends:

- VictorOps (default)
- PagerDuty
- Slack
- Email (SMTP)

Configuration lives in `config/config.yaml` and is propagated via backend-config subscription.

### 9.5 How to Exercise Locally

If you wish to inspect the runtime metrics surface to corroborate Metric 11 signal:

```bash
# In a SEPARATE terminal (NOT the analysis workspace), stand up rudder-server:
cd /path/to/rudder-server   # the analyzed repository root
docker compose up -d        # starts PostgreSQL, Transformer, MinIO, etcd
go run main.go              # starts rudder-server on default ports

# In a THIRD terminal, query the runtime metrics:
curl http://localhost:8080/health     # liveness JSON
curl http://localhost:9102/metrics    # Prometheus scrape text
```

The analysis pipeline does NOT depend on rudder-server being running. The pipeline reads only from git history, the GitHub API, and (optionally) the Linear API. Standing up rudder-server is purely OPTIONAL context-gathering.

---

## 10. Reference Map

The Reference Map is the cross-walk between the entities a new analyst will encounter (data artifacts, scripts, schemas, decisions, rules) and the canonical location of each. Use this as the lookup index when a section of the rendered report references "see X" — the locator here resolves "X" to a file path with line- or section-locator. Every row uses the `[<path>:<locator>]` citation discipline mandated by AAP §0.10.1.

### 10.1 Data Artifacts (the provenance trail per Rule 1)

| Artifact | Canonical Path | Produced By | Consumed By |
|---|---|---|---|
| Environment snapshot (Rule 6) | [`blitzy/acceleration-report/data/environment.json`] | `scripts/00_environment.sh` | every downstream script for `BLITZY_RUN_ID` plus the renderer for the Environment Verification section |
| Inflection result | [`blitzy/acceleration-report/data/inflection.json`] | `scripts/01_detect_inflection.py` | every downstream script for the inflection date plus the renderer for the Methodology section |
| Full commit roster | [`blitzy/acceleration-report/data/commits.csv`] | `scripts/02_extract_commits.sh` | `scripts/09_compute_metrics.py` (Flow Velocity, multi-module aggregation) |
| Revert-candidate commits | [`blitzy/acceleration-report/data/revert_candidates.csv`] | `scripts/02_extract_commits.sh` | `scripts/05_extract_reverts.sh` |
| PR inventory | [`blitzy/acceleration-report/data/pulls.json`] | `scripts/03_extract_pulls.py` (API) or local-git fallback | `scripts/09_compute_metrics.py` (Metrics 1, 2, 4, 5, 6, 7, 10) |
| PR review timelines | [`blitzy/acceleration-report/data/reviews.json`] | `scripts/03_extract_pulls.py` | `scripts/09_compute_metrics.py` (Metric 4 ready-for-review bounds) |
| PR event timelines | [`blitzy/acceleration-report/data/pull_events.json`] | `scripts/03_extract_pulls.py` | `scripts/09_compute_metrics.py` (Metric 4) |
| Release inventory | [`blitzy/acceleration-report/data/releases.json`] | `scripts/04_extract_releases.py` | `scripts/09_compute_metrics.py` (Metric 9), `scripts/05_extract_reverts.sh` |
| Revert resolutions | [`blitzy/acceleration-report/data/reverts.json`] | `scripts/05_extract_reverts.sh` | `scripts/09_compute_metrics.py` (Metric 8) |
| CI workflow run history | [`blitzy/acceleration-report/data/ci_runs.json`] | `scripts/06_extract_ci_history.py` | `scripts/09_compute_metrics.py` (Metric 11) |
| Test transitions | [`blitzy/acceleration-report/data/test_transitions.json`] | `scripts/06_extract_ci_history.py` | `scripts/09_compute_metrics.py` (Metric 11) |
| Exception inventory | [`blitzy/acceleration-report/data/exceptions.json`] | `scripts/07_extract_exceptions.py` | `scripts/09_compute_metrics.py` (Metric 10) |
| Linear issue inventory | [`blitzy/acceleration-report/data/issues.json`] | `scripts/08_extract_linear.py` (or empty with reason) | `scripts/09_compute_metrics.py` (Metric 6 Linear-label classifier, Metric 12) |
| Linear SLA targets | [`blitzy/acceleration-report/data/slas.json`] | `scripts/08_extract_linear.py` (or empty with reason) | `scripts/09_compute_metrics.py` (Metric 12) |
| Computed metrics | [`blitzy/acceleration-report/data/metrics.json`] | `scripts/09_compute_metrics.py` | `scripts/10_render_report.py`, `scripts/11_render_deck.py` |
| Per-engineer breakdown | [`blitzy/acceleration-report/data/per_engineer.json`] | `scripts/09_compute_metrics.py` | `scripts/10_render_report.py`, `scripts/11_render_deck.py` |
| Run log feed | [`blitzy/acceleration-report/data/run.log.jsonl`] | every script via `lib/observability.py` | analyst live-tail; CI verify target |

### 10.2 Extraction Scripts

| Script | Path | Purpose |
|---|---|---|
| Environment preamble | [`blitzy/acceleration-report/scripts/00_environment.sh`] | Rule 6 Environment Verification preamble |
| Inflection detector | [`blitzy/acceleration-report/scripts/01_detect_inflection.py`] | Three-tier AI inflection point detection |
| Commit roster | [`blitzy/acceleration-report/scripts/02_extract_commits.sh`] | Full commit and revert-candidate roster |
| PR + review + events | [`blitzy/acceleration-report/scripts/03_extract_pulls.py`] | GitHub Pulls + Reviews + Events APIs |
| Releases | [`blitzy/acceleration-report/scripts/04_extract_releases.py`] | GitHub Releases API + tag scan |
| Reverts | [`blitzy/acceleration-report/scripts/05_extract_reverts.sh`] | Revert-to-original resolution + release attribution |
| CI history | [`blitzy/acceleration-report/scripts/06_extract_ci_history.py`] | Actions Runs API + JUnit XML |
| Exceptions | [`blitzy/acceleration-report/scripts/07_extract_exceptions.py`] | Branch protection + audit log + label scan |
| Linear | [`blitzy/acceleration-report/scripts/08_extract_linear.py`] | Linear GraphQL (no-op when key absent) |
| Compute | [`blitzy/acceleration-report/scripts/09_compute_metrics.py`] | Deterministic metric computation |
| Report renderer | [`blitzy/acceleration-report/scripts/10_render_report.py`] | Markdown report generation |
| Deck renderer | [`blitzy/acceleration-report/scripts/11_render_deck.py`] | reveal.js HTML generation |
| Observability lib | [`blitzy/acceleration-report/scripts/lib/observability.py`] | Structured JSON logger with redaction |
| GitHub client lib | [`blitzy/acceleration-report/scripts/lib/github.py`] | Paginated, rate-limit-aware REST client |
| Git helpers lib | [`blitzy/acceleration-report/scripts/lib/git.py`] | `subprocess`-based git helpers |

### 10.3 JSON Schemas

| Schema | Path |
|---|---|
| Environment | [`blitzy/acceleration-report/scripts/lib/schemas/environment.schema.json`] |
| Inflection | [`blitzy/acceleration-report/scripts/lib/schemas/inflection.schema.json`] |
| Pulls | [`blitzy/acceleration-report/scripts/lib/schemas/pulls.schema.json`] |
| Releases | [`blitzy/acceleration-report/scripts/lib/schemas/releases.schema.json`] |
| Metrics | [`blitzy/acceleration-report/scripts/lib/schemas/metrics.schema.json`] |

### 10.4 Decision Log Entries (Why Each Non-Trivial Choice)

The full table lives in [`decision-log.md`](../decision-log.md). Key entries:

| ID | Subject | Section |
|---|---|---|
| DL-001 | No-write contract (read-only) | [`decision-log.md:DL-001`] |
| DL-002 | Inflection-point detection precedence (Tier 2 used) | [`decision-log.md:DL-002`] |
| DL-003 | Blitzy identity union (agent@blitzy.com + blitzy[bot]) | [`decision-log.md:DL-003`] |
| DL-004 | dependabot[bot] exclusion from Flow Velocity | [`decision-log.md:DL-004`] |
| DL-006 | Two-phase fallback when post-introduction < 90 days | [`decision-log.md:DL-006`] |
| DL-007 | Linear API unavailability handling | [`decision-log.md:DL-007`] |
| DL-008 | Admin audit log unavailability handling | [`decision-log.md:DL-008`] |
| DL-009 | M6 classifier priority order | [`decision-log.md:DL-009`] |
| DL-010 | Factual-neutral-tone blocklist | [`decision-log.md:DL-010`] |
| DL-011 | Multi-module attribution strategy | [`decision-log.md:DL-011`] |
| DL-012 | Engineering-actor substitution as the only baseline/after difference | [`decision-log.md:DL-012`] |
| DL-013 | Executive deck SRI handling | [`decision-log.md:DL-013`] |

### 10.5 Rule Anchors (Cross-Reference to AAP §0.7)

| Rule | AAP Section | Enforcement Location |
|---|---|---|
| Observability | AAP §0.7.1.1 | [`scripts/lib/observability.py`] + this onboarding doc Section 8 |
| Onboarding | AAP §0.7.1.2 | This document itself |
| Explainability | AAP §0.7.1.3 | [`decision-log.md`] |
| Visual Architecture | AAP §0.7.1.4 | [`diagrams/*.mmd`] referenced in [`acceleration-report.md`] |
| Executive Presentation | AAP §0.7.1.5 | [`executive-summary.html`] |
| Rule 1 Data Provenance | AAP §0.7.2 row 1 | Every `provenance` field in [`data/metrics.json`] |
| Rule 2 Factual-Neutral Tone | AAP §0.7.2 row 2 | Pre-write blocklist in `scripts/10_render_report.py` |
| Rule 3 Confidence Transparency | AAP §0.7.2 row 3 | `confidence` + `caveat` fields in `metrics.json` |
| Rule 4 Internal Consistency | AAP §0.7.2 row 4 | Single-source rendering from `metrics.json` |
| Rule 5 Reproducibility | AAP §0.7.2 row 5 | `extraction_command` field on every provenance + Reproducibility Appendix |
| Rule 6 Environment First | AAP §0.7.2 row 6 | [`data/environment.json`] + renderer section-order constant |

### 10.6 Analyzed Repository Anchors

| Subject | Path with Locator |
|---|---|
| Go module version | [`go.mod:L3`] |
| Default branch | [`blitzy/acceleration-report/data/environment.json:default_branch`] |
| Repository slug | [`catalog-info.yaml:metadata.annotations.github.com/project-slug`] |
| Allowed conventional-commit types | [`.github/workflows/semantic-pr.yaml:types`] |
| Release configuration | [`.github/workflows/release-please.yaml:release-type: go`] |
| Lint exemption catalogue | [`.golangci.yml:linters-settings.gosec.excludes`] |
| Security exception catalogue | [`.snyk:exclude`] |
| Test target inventory | [`Makefile:test`] |
| `/health` endpoint | [`gateway/handle.go`] |

---

## 11. Extending the Pipeline (Out of Scope but Admitted)

Extensions to the pipeline fall into three categories: disallowed, allowed, and methodology-preserving.

#### Disallowed Extensions

- **Adding a new metric beyond M1–M12 is OUT OF SCOPE** per the user constraint (AAP §0.1.3): "MUST NOT add metrics beyond the 12 specified." The `data/metrics.json` schema enumerates exactly twelve metric numbers; emitting any other number raises a JSON-schema validation error.
- **Modifying the analyzed rudder-server source tree is OUT OF SCOPE** per the user constraint "Read-only operations only." See [`decision-log.md`](../decision-log.md) entry `DL-001` for the no-write contract.
- **Writing to external systems is OUT OF SCOPE** — no POST/PUT/PATCH/DELETE calls to GitHub, Linear, or any other API. All extraction is HTTP GET only.

#### Allowed Extensions

- **Extending an extraction script with a richer raw-data field** is permitted, provided the field is added to the corresponding `data/*.json` schema under `scripts/lib/schemas/` FIRST, then the script is updated to populate it.
- **Adding a new visualization** to the rendered report (for example, an additional Mermaid diagram of an existing metric) is permitted, provided the diagram source lives under `diagrams/*.mmd` and is referenced by name in the surrounding prose per the Visual Architecture rule.
- **Replacing a fallback data source with a higher-fidelity one** is permitted, provided the substitution is recorded in [`decision-log.md`](../decision-log.md) with the rationale.

#### Any Change Must Preserve Identical Methodology

Any change MUST preserve identical methodology across baseline and after periods (window alignment, exclusion rules, extraction logic). See [`decision-log.md`](../decision-log.md) entry `DL-012` for the engineering-actor substitution constraint — the actor parameter is the ONLY difference between baseline and after extraction; all other inputs are module-scope constants.

---

## 12. Suggested Next Investigations

Out-of-scope tasks discovered during preparation but worth pursuing later:

- **Provision `LINEAR_API_KEY` and rerun**: restores Metric 6 (Flow Distribution) confidence to High where Linear labels resolve the majority of PRs, and unlocks Metric 12 (Defects Out of SLA) from "Insufficient signal".
- **Provision admin audit log access**: a repo-admin GitHub token allows Metric 10 (Approved Exceptions) to capture required-review bypasses and merge-with-failing-check overrides; confidence rises from Low to High.
- **Add JUnit XML emission to `tests.yaml`**: adding `--junit-out junit.xml` to `gotestsum` invocations would emit a stable test-result artifact uploadable as an Actions Run Artifact, lifting Metric 11 (Escaped Defects) from a HEAD-only signal to a transition-aware metric. NOTE: this is an extension to the analyzed repository and is therefore out of scope for THIS deliverable; it is recorded here for the rudder-server maintainers' consideration.
- **Conventional-commit scope catalogue for Metric 6**: the repository defines scopes (`core`, `gateway`, `jobsdb`, `warehouse`, `processor`, `router`, `batchrouter`, `destination`) — they are not currently used by Metric 6. A future enhancement could classify by scope as well as type, surfacing per-area distribution.
- **Module attribution via Go AST**: the current majority-of-file-paths approach for multi-module aggregation could be replaced with `go list ./...` package-graph analysis for more accurate cross-package commit attribution. See [`decision-log.md`](../decision-log.md) entry `DL-011` for the current strategy.
- **Velocity inflection cross-check**: even though Tier 2 resolved the inflection cleanly, running Tier 3 (velocity inflection) as a cross-check would either corroborate the date OR raise a warning flag. Future runs could log both signals.
- **Re-run with extended date range**: when the post-introduction period exceeds 90 days, the Steady-State phase decomposition will be available (currently Baseline vs Post-Introduction only per [`decision-log.md`](../decision-log.md) entry `DL-006`). A follow-up rerun in 1+ month would unlock the full three-phase view.

