# ============================================================================
# Makefile for blitzy/acceleration-report/ - Top-Level Pipeline Orchestrator
# ============================================================================
#
# This is the single entrypoint for the development-acceleration measurement
# pipeline (Agent Action Plan section 0.5.1). It drives a deterministic
# three-stage pipeline (extract -> compute -> render) entirely against
# read-only data sources: git history on the local clone, the GitHub REST
# API, the Linear GraphQL API (when LINEAR_API_KEY is available), and
# in-repository configuration files.
#
# REQUIRES GNU MAKE 4.0 OR LATER:
#   This Makefile intentionally uses GNU-make-specific constructs (per
#   decision-log entry DL-022) that are NOT POSIX or BSD-make compatible:
#     - `.DEFAULT_GOAL`  (the help-as-default-target idiom)
#     - `MAKEFLAGS += --warn-undefined-variables`  (typo-safety guard)
#     - `$(wildcard ...)`  (optional .env include detection)
#     - `$(shell ...)`     (BLITZY_RUN_ID generation)
#     - `$(MAKEFILE_LIST)` (help target auto-discovery)
#   The portable alternatives are documented in decision-log DL-022 and were
#   evaluated; the chosen course preserves readability, observability, and
#   the per-recipe strict-mode contract while requiring GNU make. On macOS,
#   install via `brew install make` and invoke as `gmake`; on Linux/CI the
#   system `make` is GNU make 4.x. A guard near the top of the file refuses
#   to proceed with a clear error message if a non-GNU make is detected.
#
# Read-only contract (AAP section 0.3.2):
#   "Read-only operations only. MUST NOT modify the repository or external
#    systems." Running any target in this Makefile has ZERO effect on the
#    analyzed repository's source tree, refs, tags, remote, the GitHub
#    Releases/Actions/Branches APIs, the Linear issue tracker, codecov,
#    or any other external system. No recipe issues HTTP POST/PUT/PATCH/
#    DELETE calls.
#
# Relationship to the rudder-server root Makefile:
#   This file is workspace-local and unrelated to the Go build at the
#   repository root. The two Makefiles never invoke each other; running
#   any target here has no effect on the rudder-server build, test
#   suite, container image, or CI/CD pipeline.
#
# Strict-mode contract:
#   Every recipe runs under `bash -eu -o pipefail` (see SHELL/.SHELLFLAGS).
#   MAKEFLAGS adds --warn-undefined-variables so a typo in a Make variable
#   surfaces as a warning rather than a silent empty expansion.
#
# Observability contract (AAP section 0.7.1.1):
#   BLITZY_RUN_ID is a UUID4 per-run correlation ID. It is generated at
#   Makefile parse time (or honoured from the calling environment / .env),
#   then exported so every subprocess invoked by any recipe inherits the
#   same identifier. The structured-JSON log feed at data/run.log.jsonl
#   is correlated by this ID. The first script (00_environment.sh) also
#   honours a pre-set BLITZY_RUN_ID per its own contract; Make's exported
#   value flows through unchanged.
#
# Topological order of the extract stage (AAP section 0.5.2, with the
# 02 -> 01 dependency edge restored per decision-log entry DL-029):
#   00_environment.sh        -> data/environment.json
#   02_extract_commits.sh    -> data/commits.csv + data/revert_candidates.csv
#   01_detect_inflection.py  -> data/inflection.json
#   03_extract_pulls.py      -> data/pulls.json + data/reviews.json + data/pull_events.json
#   04_extract_releases.py   -> data/releases.json
#   05_extract_reverts.sh    -> data/reverts.json
#   06_extract_ci_history.py -> data/ci_runs.json + data/test_transitions.json
#   07_extract_exceptions.py -> data/exceptions.json
#   08_extract_linear.py     -> data/issues.json + data/slas.json
#
# IMPORTANT (DL-029): 02_extract_commits.sh MUST run BEFORE
# 01_detect_inflection.py. The Tier-3 velocity-inflection fallback in
# 01_detect_inflection.py reads commits.csv when the Tier-1 trailer search
# and the Tier-2 AI-actor email match both yield no signal. In this
# repository the Tier-2 path succeeds for `agent@blitzy.com` so the
# dependency is currently masked, but the AAP-documented invariant must
# be honoured regardless of repository specifics.
#
# Quick reference:
#   make help     - show all targets (default goal)
#   make setup    - provision the workspace virtualenv from requirements.txt
#   make extract  - invoke scripts 00-08 in topological order
#   make compute  - invoke 09_compute_metrics.py
#   make render   - invoke 10_render_report.py and 11_render_deck.py
#   make all      - full pipeline (setup -> extract -> compute -> render)
#   make verify   - re-apply every Rules 1-6 quality-gate check (idempotent)
#   make clean    - remove .venv/ and data/*.json|csv|jsonl
#   make lint     - bash -n + python -m py_compile against scripts/
#
# See README.md for prerequisites and onboarding/rerun-and-observability.md
# for a full clean-machine rerun walkthrough.
# ============================================================================

.PHONY: help default setup extract compute render all clean distclean lint verify

# ----------------------------------------------------------------------------
# GNU make detection guard
# ----------------------------------------------------------------------------
# The constructs immediately below (.DEFAULT_GOAL, MAKEFLAGS+=, $(wildcard),
# $(shell), $(MAKEFILE_LIST)) are GNU-specific. On a non-GNU make (BSD make,
# nmake) Make would either fail with a parse error or silently produce
# different behaviour. The `$(MAKE_VERSION)` variable is provided only by
# GNU make; on other implementations it expands to the empty string. The
# guard below fails the build immediately with a clear diagnostic when a
# non-GNU make is detected, so the user installs `gmake` on macOS or the
# appropriate package on their platform. This is the runtime safety net
# that accompanies the documented DL-022 decision to require GNU make.
ifeq ($(MAKE_VERSION),)
$(error This Makefile requires GNU make 4.0 or later. Detected non-GNU make. See decision-log.md DL-022. On macOS: brew install make; then run `gmake` instead of `make`.)
endif

.DEFAULT_GOAL := help

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
MAKEFLAGS += --warn-undefined-variables

# ============================================================================
# Variables
# ============================================================================
# Variables are declared once at module scope so they are constants across
# every recipe invocation. This enforces the AAP section 0.5.6 "identical
# methodology" guarantee: every script invocation receives the same paths
# and the same correlation ID.

PYTHON ?= python3
VENV := .venv
VENV_PY := $(VENV)/bin/python
VENV_PIP := $(VENV)/bin/pip
DATA_DIR := data
LOG_FILE := $(DATA_DIR)/run.log.jsonl
SCRIPTS_DIR := scripts
SCHEMAS_DIR := $(SCRIPTS_DIR)/lib/schemas
DIAGRAMS_DIR := diagrams
REPORT_FILE := acceleration-report.md
DECK_FILE := executive-summary.html

# REQUIRED_ARTIFACTS - the canonical set of data/ files that MUST exist for
# the deliverable to be considered complete and reproducible (Rule 1 Data
# Provenance, Rule 5 Reproducibility, AAP Quality Gate 1). The `verify`
# target asserts every name in this list is present in $(DATA_DIR) and exits
# non-zero with a clear error message if any is missing. Every artifact in
# this list also has a corresponding schema under $(SCHEMAS_DIR) when it is
# JSON, and that schema is run against the artifact during verify. There
# is currently NO optional-artifact category in this deliverable: every
# data file produced by the extract+compute pipeline is required for the
# report and deck to render fully. If a future change introduces a truly
# optional artifact, it goes in a separate OPTIONAL_ARTIFACTS list (not
# defined here yet) and is documented as optional in decision-log.md.
REQUIRED_ARTIFACTS := \
	environment.json \
	inflection.json \
	commits.csv \
	revert_candidates.csv \
	pulls.json \
	reviews.json \
	pull_events.json \
	releases.json \
	reverts.json \
	ci_runs.json \
	test_transitions.json \
	exceptions.json \
	issues.json \
	slas.json \
	metrics.json \
	per_engineer.json

# Optional workspace-local .env (gitignored). Variables defined here are
# included into the Make scope AND exported to recipes via `export`. The
# `ifneq` guard prevents Make from erroring on a missing .env file on the
# first run before the analyst has copied .env.example to .env.
# The .env include is placed BEFORE the BLITZY_RUN_ID conditional default
# below so a `BLITZY_RUN_ID=<uuid>` line in .env correctly overrides the
# auto-generated value (a reviewer can replay or compare a prior run by
# pinning the ID).
# Variables expected (all optional per AAP section 0.8.5):
#   GITHUB_REPO_SLUG, GH_TOKEN, LINEAR_API_KEY,
#   ANALYSIS_START_DATE, ANALYSIS_END_DATE, BLITZY_RUN_ID, LOG_LEVEL, DRY_RUN
ifneq (,$(wildcard ./.env))
include .env
export
endif

# BLITZY_RUN_ID - per-run UUID4 correlation ID for the structured-JSON logger.
# Generated ONCE at Makefile parse time using uuidgen when available,
# falling back to Python's uuid module (uuidgen is absent from minimal
# container images such as the analysis sandbox).
#
# CRITICAL: the conditional pattern below uses `ifndef` + `:=` (eager
# assignment) rather than `?=` (recursive) because `?=` re-evaluates its
# right-hand side every time the variable is referenced, which would
# generate a NEW UUID per reference and break the per-run correlation
# contract. The `ifndef` form respects values supplied via the calling
# environment, .env (included above), and `make BLITZY_RUN_ID=...` on the
# command line - so a reviewer can replay or compare runs by pinning the
# ID. `export` propagates the resolved value to every subprocess invoked
# by any recipe.
ifndef BLITZY_RUN_ID
BLITZY_RUN_ID := $(shell uuidgen 2>/dev/null || python3 -c 'import uuid; print(uuid.uuid4())')
endif
export BLITZY_RUN_ID

# ============================================================================
# Targets
# ============================================================================

help: ## Show this help message
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---- setup -----------------------------------------------------------------
# Provision the workspace virtualenv from requirements.txt. The sentinel
# file .venv/.touch lets Make track creation idempotently - subsequent
# `make setup` calls are no-ops when requirements.txt is unchanged.
# The order-only-like behaviour is preserved through the sentinel pattern
# because Make compares timestamps: .touch is newer than requirements.txt
# after install, so re-running setup is fast.
#
# Venv provisioning fallback (DL-030):
#   The recipe first attempts `$(PYTHON) -m venv $(VENV)`. On some
#   Linux base images (notably Ubuntu 24+ minimal containers) the
#   system Python's bundled `ensurepip` is broken or absent, so the
#   stdlib venv module fails with an opaque subprocess error. When
#   that happens, the recipe falls back to the standalone `virtualenv`
#   tool, which is the documented workaround in onboarding/rerun-and-
#   observability.md §2 Step 4. If BOTH paths fail, the recipe exits
#   non-zero with an actionable error message naming the installation
#   command for the standalone virtualenv tool. The fallback uses an
#   `||` chain inside a single recipe line so Make's strict-mode
#   contract (every recipe runs under `bash -eu -o pipefail`) still
#   sees a single subprocess exit code.

setup: $(VENV)/.touch ## Create venv and install pinned Python dependencies

$(VENV)/.touch: requirements.txt
	@echo "[setup] Provisioning $(VENV) using $(PYTHON)..."
	@# Three-tier venv-creation: stdlib venv -> standalone virtualenv -> fatal error.
	@# The `command -v` check on virtualenv runs before the invocation so the
	@# error message can distinguish "venv broken AND virtualenv missing" from
	@# "venv broken AND virtualenv failed".
	@if ! $(PYTHON) -m venv $(VENV) 2>/dev/null; then \
		echo "[setup] python -m venv failed; trying standalone virtualenv..."; \
		if command -v virtualenv >/dev/null 2>&1; then \
			virtualenv --python=$(PYTHON) $(VENV); \
		else \
			echo "[setup] FATAL: both 'python3 -m venv' and 'virtualenv' are unavailable." >&2; \
			echo "[setup]        Install the standalone virtualenv tool:" >&2; \
			echo "[setup]            pip install --user virtualenv" >&2; \
			echo "[setup]        Then re-run 'make setup'. See decision-log.md DL-030 for context." >&2; \
			exit 1; \
		fi; \
	fi
	$(VENV_PIP) install --upgrade pip
	$(VENV_PIP) install -r requirements.txt
	@touch $(VENV)/.touch
	@echo "[setup] Virtualenv ready at $(VENV)"

# ---- extract ---------------------------------------------------------------
# Invoke scripts 00-08 in topological dependency order (AAP section 0.5.2).
# Each script writes a dedicated data/*.json or data/*.csv artifact and
# exits non-zero on failure; strict-mode propagates through Make's
# recipe-line semantics so any script failure aborts the pipeline.
# BLITZY_RUN_ID is inherited from the Make export above; mkdir -p is
# idempotent and present for fresh-clone safety.

extract: setup ## Run extraction scripts 00-08 in topological order
	@mkdir -p $(DATA_DIR)
	@echo "[extract] Run ID: $(BLITZY_RUN_ID)"
	bash $(SCRIPTS_DIR)/00_environment.sh
	bash $(SCRIPTS_DIR)/02_extract_commits.sh
	$(VENV_PY) $(SCRIPTS_DIR)/01_detect_inflection.py
	$(VENV_PY) $(SCRIPTS_DIR)/03_extract_pulls.py
	$(VENV_PY) $(SCRIPTS_DIR)/04_extract_releases.py
	bash $(SCRIPTS_DIR)/05_extract_reverts.sh
	$(VENV_PY) $(SCRIPTS_DIR)/06_extract_ci_history.py
	$(VENV_PY) $(SCRIPTS_DIR)/07_extract_exceptions.py
	$(VENV_PY) $(SCRIPTS_DIR)/08_extract_linear.py
	@echo "[extract] Done. Artifacts under $(DATA_DIR)/"

# ---- compute ---------------------------------------------------------------
# Deterministic compute step: reads every data/*.json + data/*.csv artifact
# and emits the single source of truth (data/metrics.json) plus the
# per-engineer breakdown (data/per_engineer.json). Pure I/O - no external
# calls - so re-running it from the same data artifacts is byte-identical
# modulo the embedded extraction timestamp.

compute: extract ## Compute metrics.json + per_engineer.json from raw artifacts
	$(VENV_PY) $(SCRIPTS_DIR)/09_compute_metrics.py
	@echo "[compute] Metrics computed: $(DATA_DIR)/metrics.json + $(DATA_DIR)/per_engineer.json"

# ---- render ----------------------------------------------------------------
# Renderers read ONLY data/metrics.json and data/per_engineer.json (this is
# how Rule 4 Internal Consistency is mechanically satisfied: both renderers
# consume the same artifact). They perform NO arithmetic - only formatting -
# so the numbers cannot diverge between the Markdown report and the HTML
# deck. Each renderer runs its own pre-write guards (factual-neutral tone
# blocklist, section ordering, schema validation) before any file write.

render: compute ## Render acceleration-report.md and executive-summary.html
	$(VENV_PY) $(SCRIPTS_DIR)/10_render_report.py
	$(VENV_PY) $(SCRIPTS_DIR)/11_render_deck.py
	@echo "[render] Rendered: $(REPORT_FILE) + $(DECK_FILE)"

# ---- all -------------------------------------------------------------------
# Convenience target: full end-to-end pipeline via transitive dependencies.
# The dependency chain expands: all -> render -> compute -> extract -> setup.

all: render ## End-to-end pipeline: setup -> extract -> compute -> render
	@echo "[all] Pipeline complete. Run ID: $(BLITZY_RUN_ID)"

# ---- clean / distclean -----------------------------------------------------
# Per decision-log entry DL-031, this workspace ships its 16 deliverable
# data artifacts as committed seed files. The original `clean` target's
# glob `rm -f $(DATA_DIR)/*.json *.csv *.jsonl` matched the committed
# seed AND any freshly generated artifacts indistinguishably, deleting
# both. An analyst running `make clean` to clear cache state would
# unexpectedly lose the committed baseline (recoverable via
# `git checkout -- data/` but a surprise nonetheless).
#
# The fix splits the destructive behaviour across two targets:
#   * `clean`     — preserves the committed seed; removes only the venv
#                   and the append-only journal `data/run.log.jsonl`,
#                   plus the cursor file `data/.cursor.json` and any
#                   `.tmp` write-buffer remnant. The 16 REQUIRED_ARTIFACTS
#                   files (committed seed) are LEFT IN PLACE. Re-running
#                   `make all` after `make clean` will overwrite them
#                   with byte-identical contents modulo the extraction
#                   timestamp; until then `make verify` will still pass
#                   against the committed seed.
#   * `distclean` — the previous behaviour: full wipe of venv AND every
#                   `data/*.json|csv|jsonl` file. Use when explicitly
#                   trying to reproduce the live-pipeline path from
#                   scratch. The recipe echoes a clear warning about
#                   the destructive effect on committed seed data and
#                   points to `git checkout -- data/` for recovery.
#
# A small helper script computes the exclusion set so the clean glob is
# auditable: REQUIRED_ARTIFACTS is the same module-level constant the
# verify target consults, ensuring the two targets stay in sync.

clean: ## Remove venv + log + cursor; PRESERVE the committed data/ seed artifacts (use distclean for full wipe)
	rm -rf $(VENV)
	rm -f $(DATA_DIR)/run.log.jsonl
	rm -f $(DATA_DIR)/.cursor.json
	@# Remove any .tmp atomic-write buffers left behind by a crash.
	@find $(DATA_DIR) -maxdepth 1 -name '*.tmp' -type f -delete 2>/dev/null || true
	@echo "[clean] Virtualenv, run log, cursor, and *.tmp buffers removed."
	@echo "[clean] The 16 committed seed artifacts under $(DATA_DIR)/ were PRESERVED."
	@echo "[clean] Run 'make distclean' for a full wipe (deletes committed seed too)."

distclean: clean ## Full wipe: remove venv AND every data artifact (committed seed included)
	@# This target is intentionally destructive: it removes the committed
	@# seed artifacts as well as the regenerable ones. The diagnostic
	@# echo line names the git-checkout recovery path so an analyst who
	@# runs distclean accidentally has a one-line recovery.
	rm -f $(DATA_DIR)/*.json $(DATA_DIR)/*.csv $(DATA_DIR)/*.jsonl
	@echo "[distclean] WARNING: also removed the 16 committed seed artifacts under $(DATA_DIR)/."
	@echo "[distclean] Recover the committed seed via: git checkout -- $(DATA_DIR)/"
	@echo "[distclean] See decision-log.md DL-031 for the seed/regenerable split rationale."

# ---- lint ------------------------------------------------------------------
# Lightweight syntactic validation:
#   - python -m py_compile on every Python script (scripts/*.py + scripts/lib/*.py)
#   - bash -n on every shell script (scripts/*.sh)
# This is the fast feedback loop used during development; `make verify`
# is the full Rules 1-6 quality-gate check used pre-merge.
# Both inner loops abort on the first failure via `|| exit 1`.

lint: setup ## Lint Python and Bash scripts (syntactic validation)
	@echo "[lint] Compiling Python scripts..."
	@for f in $(SCRIPTS_DIR)/*.py $(SCRIPTS_DIR)/lib/*.py; do \
		$(VENV_PY) -m py_compile "$$f" || exit 1; \
	done
	@echo "[lint] Parsing Bash scripts..."
	@for f in $(SCRIPTS_DIR)/*.sh; do \
		bash -n "$$f" || exit 1; \
	done
	@echo "[lint] All scripts compile/parse cleanly."

# ---- verify ----------------------------------------------------------------
# Re-apply every Rules 1-6 verification check (AAP sections 0.7.2 and 0.9.4):
#
#   * JSON schema validation of every data/*.json against its schema
#     (Rule 1 Data Provenance, Rule 3 Confidence Transparency mechanics)
#   * Factual-neutral-tone blocklist on the rendered Markdown (Rule 2)
#   * Bash + Python syntactic validity for every script referenced in the
#     Reproducibility Appendix (Rule 5)
#   * Section-order check via 10_render_report.py --verify-only (Rule 6)
#   * Diagram round-trip: every diagrams/*.mmd referenced in the report
#     (Visual Architecture Documentation rule)
#
# Idempotent. Exits 0 when every check passes; exits non-zero on the first
# failure with a diagnostic message identifying the failing artifact.
# Operates only on rendered artifacts and data/ - does NOT re-run
# extraction. Depends on `setup` so a clean-machine reviewer can run
# `make verify` directly without first running `make all` provided the
# data/ artifacts are present (they are committed per the workspace
# .gitignore policy).

verify: setup ## Re-run all Rules 1-6 checks and quality gates (idempotent)
	@echo "[verify] Required-artifact presence check (Quality Gate 1, Rule 5)..."
	@missing_count=0; \
	for artifact in $(REQUIRED_ARTIFACTS); do \
		if [ -f "$(DATA_DIR)/$$artifact" ]; then \
			echo "  [ok]   $$artifact"; \
		else \
			echo "  [FAIL] REQUIRED artifact $(DATA_DIR)/$$artifact is missing"; \
			missing_count=$$((missing_count + 1)); \
		fi; \
	done; \
	if [ "$$missing_count" -gt 0 ]; then \
		echo "  [FAIL] $$missing_count required artifact(s) missing — verify cannot proceed."; \
		echo "         Run 'make all' (or at minimum 'make compute') to regenerate the missing artifacts."; \
		echo "         If an artifact is intentionally absent, see decision-log.md DL-025 for the required/optional contract."; \
		exit 1; \
	fi
	@echo "[verify] JSON schema validation (data/*.json against $(SCHEMAS_DIR)/*.schema.json)..."
	@for schema in $(SCHEMAS_DIR)/*.schema.json; do \
		artifact=$$(basename "$$schema" .schema.json).json; \
		if [ ! -f "$(DATA_DIR)/$$artifact" ]; then \
			echo "  [FAIL] schema $$schema exists but $(DATA_DIR)/$$artifact is missing"; \
			echo "         Schemas under $(SCHEMAS_DIR) implicitly require the matching artifact."; \
			echo "         If a schema is intended to be a forward-looking placeholder without a current artifact, document it in decision-log.md DL-025 and remove the schema until the artifact exists."; \
			exit 1; \
		fi; \
		if $(VENV_PY) -c "import json,jsonschema; jsonschema.validate(json.load(open('$(DATA_DIR)/'+'$$artifact')), json.load(open('$$schema')))" 2>/dev/null; then \
			echo "  [ok]   $$artifact"; \
		else \
			echo "  [FAIL] $$artifact failed schema validation against $$schema"; \
			echo "         Re-run for details: $(VENV_PY) -c \"import json,jsonschema; jsonschema.validate(json.load(open('$(DATA_DIR)/$$artifact')), json.load(open('$$schema')))\""; \
			exit 1; \
		fi; \
	done
	@echo "[verify] Factual-neutral-tone blocklist check (Rule 2)..."
	@if grep -iE '(impressive|significant|excellent|remarkable|unfortunately|striking|dramatic|surprisingly|notably|crucially)' $(REPORT_FILE); then \
		echo "  [FAIL] Subjective qualifier matched in $(REPORT_FILE) - see grep output above"; \
		exit 1; \
	else \
		echo "  [ok]   No subjective qualifiers found in $(REPORT_FILE)"; \
	fi
	@echo "[verify] Bash syntactic validity (Rule 5 - appendix commands)..."
	@for f in $(SCRIPTS_DIR)/*.sh; do \
		bash -n "$$f" || { echo "  [FAIL] $$f"; exit 1; }; \
	done
	@echo "  [ok]   All Bash scripts parse cleanly"
	@echo "[verify] Python syntactic validity (Rule 5 - appendix commands)..."
	@for f in $(SCRIPTS_DIR)/*.py $(SCRIPTS_DIR)/lib/*.py; do \
		$(VENV_PY) -m py_compile "$$f" || { echo "  [FAIL] $$f"; exit 1; }; \
	done
	@echo "  [ok]   All Python scripts compile cleanly"
	@echo "[verify] Section-order and pre-write guards via 10_render_report.py --verify-only (Rule 6)..."
	@$(VENV_PY) $(SCRIPTS_DIR)/10_render_report.py --verify-only
	@echo "[verify] Diagram round-trip check (every $(DIAGRAMS_DIR)/*.mmd referenced in $(REPORT_FILE))..."
	@for d in $(DIAGRAMS_DIR)/*.mmd; do \
		name=$$(basename "$$d"); \
		if grep -q "$$name" $(REPORT_FILE); then \
			echo "  [ok]   $$name referenced"; \
		else \
			echo "  [FAIL] $$name NOT referenced in $(REPORT_FILE)"; \
			exit 1; \
		fi; \
	done
	@echo "[verify] All rule checks passed."
