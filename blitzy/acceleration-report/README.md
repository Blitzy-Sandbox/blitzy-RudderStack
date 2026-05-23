# blitzy/acceleration-report — Development Acceleration Measurement Workspace

*Self-contained, read-only measurement-and-reporting workspace that produces a fully reproducible measurement deliverable comparing twelve flow and operational metrics on the `Blitzy-Sandbox/blitzy-RudderStack` repository before and after the introduction of AI assistance.*

---

## Read-Only Contract

> "Read-only operations only. MUST NOT modify the repository or external systems."

This workspace consumes signal from git history, the GitHub REST API, the Linear GraphQL API, CI workflow definitions, and in-repository configuration files. Every consumer is a read path — no commit is written to the analyzed repository's branches, no tag is created or moved, no remote ref is pushed, and no HTTP POST/PUT/PATCH/DELETE call is issued against any external system. The full rationale for this contract is recorded in [`decision-log.md`](./decision-log.md) under entry `DL-001: No-write contract`.

---

## Directory Layout

```
blitzy/acceleration-report/
├── acceleration-report.md          # Canonical measurement report (11 sections)
├── executive-summary.html          # Self-contained reveal.js executive deck
├── decision-log.md                 # Non-trivial decisions with rationale (Explainability rule)
├── README.md                       # This file
├── Makefile                        # Top-level orchestrator (setup, extract, compute, render, all, verify)
├── requirements.txt                # Pinned Python dependencies
├── .env.example                    # Environment variable template (GH_TOKEN, LINEAR_API_KEY, etc.)
├── data/                           # Raw + computed data artifacts (provenance trail — Rule 1)
│   ├── environment.json
│   ├── inflection.json
│   ├── commits.csv
│   ├── revert_candidates.csv
│   ├── pulls.json
│   ├── reviews.json
│   ├── pull_events.json
│   ├── releases.json
│   ├── reverts.json
│   ├── ci_runs.json
│   ├── test_transitions.json
│   ├── exceptions.json
│   ├── issues.json
│   ├── slas.json
│   ├── metrics.json                # Single source of truth — feeds both renderers (Rule 4)
│   └── per_engineer.json
├── diagrams/                       # Mermaid diagram sources (Visual Architecture rule)
│   ├── data-source-topology.mmd
│   ├── temporal-phases-timeline.mmd
│   ├── engineering-actor-framing.mmd
│   ├── acceleration-curve.mmd
│   └── extraction-pipeline.mmd
├── scripts/                        # Extraction → Compute → Render pipeline
│   ├── 00_environment.sh           # Rule 6: Environment Verification preamble
│   ├── 01_detect_inflection.py     # Three-tier AI inflection detection
│   ├── 02_extract_commits.sh       # Full commit roster + revert candidates
│   ├── 03_extract_pulls.py         # GitHub Pulls/Reviews/Commits/Events APIs
│   ├── 04_extract_releases.py      # Releases API + tag scan
│   ├── 05_extract_reverts.sh       # Revert resolution + release attribution
│   ├── 06_extract_ci_history.py    # Actions Runs API + JUnit artifacts
│   ├── 07_extract_exceptions.py    # Audit log + force-pushes + label scan
│   ├── 08_extract_linear.py        # Linear GraphQL (no-op without LINEAR_API_KEY)
│   ├── 09_compute_metrics.py       # Deterministic compute from data/*.json
│   ├── 10_render_report.py         # Renders acceleration-report.md
│   ├── 11_render_deck.py           # Renders executive-summary.html
│   └── lib/
│       ├── observability.py        # Structured-JSON logger (BLITZY_RUN_ID)
│       ├── github.py               # GitHub REST client (rate limits, pagination)
│       ├── git.py                  # Subprocess-based git helpers
│       └── schemas/                # JSON schemas for data/*.json validation
│           ├── environment.schema.json
│           ├── inflection.schema.json
│           ├── pulls.schema.json
│           ├── releases.schema.json
│           └── metrics.schema.json
└── onboarding/
    └── rerun-and-observability.md  # Clean-machine rerun instructions (Onboarding rule)
```

Files that are generated per run (`data/*.json`, `data/*.csv`, `data/run.log.jsonl`) and the workspace-local Python virtualenv (`.venv/`) are gitignored per the workspace `.gitignore`. The directory paths themselves are preserved on a fresh clone via `.gitkeep` markers so the pipeline writes into existing locations on the first run.

---

## Prerequisites

- Python 3.12 or later
- git 2.43.0 or later
- bash 5.0 or later
- (Optional) `GH_TOKEN` — GitHub Personal Access Token with `repo:read` and `actions:read` scope, for higher API rate limits. The pipeline falls back to local-git when absent and reduces confidence tiers per metric accordingly.
- (Optional) `LINEAR_API_KEY` — Linear API token, for Metric 6 issue-label classification and Metric 12 SLA-tier resolution. The pipeline falls back to repository-only signals when absent; Metric 12 reports `"Insufficient signal — no SLA source"` when no policy document is found in the repository either.

---

## One-Command Rerun

```bash
cd blitzy/acceleration-report
cp .env.example .env       # Edit .env to add GH_TOKEN and LINEAR_API_KEY (both optional)
make all                   # Runs setup → extract → compute → render
make verify                # Re-applies all rule checks (factual-neutral, JSON schema, …)
```

After `make all`, the deliverables appear at:

- `acceleration-report.md` (canonical report, 11 sections)
- `executive-summary.html` (open in a browser to view)
- `data/metrics.json` (single source of truth for both renderers)
- `data/run.log.jsonl` (structured JSON log feed)

A full rerun walkthrough — including offline-mode behaviour, rate-limit handling, and dry-run preflight — lives in [`onboarding/rerun-and-observability.md`](./onboarding/rerun-and-observability.md).

---

## Deliverables

- **[`acceleration-report.md`](./acceleration-report.md)** — Eleven-section measurement report following the user-prompt Validation Framework section order (Executive Summary → Environment Verification → Data Source Inventory → Methodology → Metric Deep-Dives ×12 → Requirements Traceability Matrix → Per-Engineer Acceleration → Acceleration Curve → Risk Assessment → Limitations → Reproducibility Appendix). Every numeric claim is traceable to a command and a raw data artifact.
- **[`executive-summary.html`](./executive-summary.html)** — Single self-contained reveal.js deck (12–18 slides, target 16). Opens in any evergreen browser; CDN-pinned libraries (reveal.js 5.1.0, Mermaid 11.4.0, Lucide 0.460.0); Blitzy brand theme embedded inline.
- **[`decision-log.md`](./decision-log.md)** — Markdown table of non-trivial decisions per the Explainability rule. Each row records the decision, the alternatives considered, the rationale, and the risks.
- **[`onboarding/rerun-and-observability.md`](./onboarding/rerun-and-observability.md)** — Clean-machine rerun instructions and observability surfaces per the Onboarding & Continued Development rule.

---

## Rules & Quality Gates

Two rule families govern the workspace. Each rule is enforced mechanically by the renderer or the workspace scaffolding and is re-checked by `make verify`.

### User-Prompt Rules 1–6 (Reproducibility Framework — AAP §0.7.2)

These six rules govern the content and form of the measurement report itself:

1. **Rule 1 — Data Provenance**: every number traces to a command in `acceleration-report.md` §11 Reproducibility Appendix.
2. **Rule 2 — Factual-Neutral Tone**: zero subjective qualifiers in the report body (blocklist enforced before write).
3. **Rule 3 — Confidence Transparency**: every metric carries a confidence tag (High/Medium/Low/Insufficient); Low values display the caveat at every appearance.
4. **Rule 4 — Internal Consistency**: every metric value is identical across §1 Executive Summary, §5 Metric Deep-Dive, §6 Traceability Matrix, §8 Acceleration Curve.
5. **Rule 5 — Reproducibility**: `make verify` re-runs all the rule checks; `make all` re-derives every number from scratch.
6. **Rule 6 — Environment First**: §2 Environment Verification precedes all Metric Deep-Dives.

### Engineering Rules (Workspace Quality Standards — AAP §0.7.1)

These five rules govern how the workspace itself is built, documented, observed, and explained:

1. **Observability** (AAP §0.7.1.1) — Structured JSON logging with per-run UUID4 correlation IDs (`BLITZY_RUN_ID`), a `--dry-run` readiness preflight on every script, a metrics surface (counters in stdout summary), and a dashboard template in `diagrams/extraction-pipeline.mmd`. See [`scripts/lib/observability.py`](./scripts/lib/observability.py).
2. **Onboarding & Continued Development** (AAP §0.7.1.2) — Clean-machine rerun instructions live in [`onboarding/rerun-and-observability.md`](./onboarding/rerun-and-observability.md); suggested next investigations are tracked in [`decision-log.md`](./decision-log.md) §3.
3. **Explainability** (AAP §0.7.1.3) — Every non-trivial decision is logged in [`decision-log.md`](./decision-log.md) with what was decided, alternatives, rationale, and risk. Rationale lives ONLY in the decision log; code comments do not duplicate it.
4. **Visual Architecture Documentation** (AAP §0.7.1.4) — All diagrams use Mermaid. Sources under [`diagrams/`](./diagrams/) (data-source topology, temporal phases timeline, engineering-actor framing, acceleration curve, extraction pipeline). Each diagram has a title and legend; each is referenced by name in the report.
5. **Executive Presentation** (AAP §0.7.1.5) — A single self-contained reveal.js HTML deck at [`executive-summary.html`](./executive-summary.html) covering scope, business value, architecture, risks, and onboarding for non-technical leadership.

Full rule text and enforcement detail live in the Agent Action Plan §0.7.1 (engineering rules) and §0.7.2 (user-prompt rules), and in [`decision-log.md`](./decision-log.md).

---

## Where to Next

- New to the workspace? → [`onboarding/rerun-and-observability.md`](./onboarding/rerun-and-observability.md)
- Looking for the report? → [`acceleration-report.md`](./acceleration-report.md)
- Want the executive view? → Open [`executive-summary.html`](./executive-summary.html) in a browser.
- Why a decision was made? → [`decision-log.md`](./decision-log.md)
- Troubleshooting? → [`onboarding/rerun-and-observability.md`](./onboarding/rerun-and-observability.md) "Common Pitfalls" section.

---

## Pipeline Stages

The workspace is a deterministic three-stage pipeline (per Agent Action Plan §0.5.1):

1. **Extract** (`scripts/00`–`08`) — Read-only consumers of git history, the GitHub REST API, the Linear GraphQL API, and in-repository configuration files. Each script emits a dedicated `data/*.json` or `data/*.csv` artifact and exits non-zero on failure.
2. **Compute** (`scripts/09`) — Pure I/O; reads `data/*.json` and `data/*.csv`, applies the same window-alignment and exclusion logic to both baseline and after periods, and writes `data/metrics.json` plus `data/per_engineer.json`.
3. **Render** (`scripts/10`, `scripts/11`) — Reads only the compute outputs and produces `acceleration-report.md` plus `executive-summary.html`. The renderers perform no arithmetic — they only format — which is how Rule 4 (Internal Consistency) is mechanically guaranteed.

---

## Out of Scope

Per Agent Action Plan §0.3.2 and the user-prompt Boundaries & Preservation list, the following are out of scope for this workspace:

- Modification of the analyzed repository's `main` branch, history, refs, tags, or remote.
- Modification of external systems (Linear, GitHub Releases, Actions, Issues, branch protection, dependabot, codecov).
- Fabrication, estimation, or extrapolation of any metric value.
- Adding metrics beyond the twelve specified in the user prompt.
- Runtime performance, customer satisfaction scores, revenue impact (verbatim user instruction).
- Competitive ranking of individual engineers (DORA/SPACE caveat — per-engineer values are reported for attribution, not for evaluation).
