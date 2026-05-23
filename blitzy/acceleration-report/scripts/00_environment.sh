#!/usr/bin/env bash
# =============================================================================
# 00_environment.sh — Rule 6 Environment Verification Preamble
# =============================================================================
#
# First script in the blitzy/acceleration-report/ extraction pipeline.
# Captures the read-only execution environment context that every downstream
# metric extraction depends on, per AAP §0.7.2 Rule 6 (verbatim):
#
#   "Document execution environment (repository URL, git version, total commit
#    count, active branch count, submodule state, commit date range, extraction
#    timestamp) before any metric extraction."
#
# Behaviour:
#   1. Generates a fresh BLITZY_RUN_ID (UUID4) if not already in the environment,
#      and exports it for downstream scripts to inherit.
#   2. Collects the seven Rule-6 fields using ONLY read-only git invocations.
#   3. Scrubs any embedded auth token from the repository URL before persisting.
#   4. Normalises commit-date timestamps to UTC ISO-8601 with the Z suffix.
#   5. Writes data/environment.json (atomic via .tmp + rename).
#   6. Emits structured-JSON observability events to stderr.
#   7. Prints "BLITZY_RUN_ID=<uuid>" as the sole stdout line, so the orchestrator
#      can capture and propagate it via `eval $(./00_environment.sh)`.
#
# Read-only contract:
#   This script makes NO modifications to the repository, no commits, no pushes,
#   no fetches, no remote-ref updates, and no HTTP calls. Every git invocation
#   is in the read-only set: --version, remote get-url, rev-parse, rev-list,
#   for-each-ref, submodule status, and log.
#
# Exit codes:
#   0  success (or --help / --dry-run)
#   1  required tool missing or not in a git repository
#   2  invalid command-line argument
#
# Per the prompt's gating-prerequisite note: if this script fails, downstream
# extraction scripts MUST refuse to proceed.
# =============================================================================

set -euo pipefail

# -----------------------------------------------------------------------------
# Module-level constants
# -----------------------------------------------------------------------------
SCRIPT_NAME="00_environment"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DATA_DIR="${WORKSPACE_ROOT}/data"
OUTPUT_FILE="${DATA_DIR}/environment.json"

# Default DRY_RUN to 0 but honour a pre-existing DRY_RUN env var (consistent
# with the workspace .env.example contract; the CLI --dry-run flag also sets it).
DRY_RUN="${DRY_RUN:-0}"

# -----------------------------------------------------------------------------
# usage: human-readable help text
# -----------------------------------------------------------------------------
usage() {
    cat <<EOF
Usage: $(basename "$0") [--dry-run] [--output PATH] [--help]

Rule 6 Environment Verification Preamble (first script in the pipeline).

Emits ${OUTPUT_FILE} with:
  repository_url        Git remote URL with any auth token scrubbed
  git_version           Installed git version (from \`git --version\`)
  total_commit_count    \`git rev-list --count HEAD\`
  active_branch_count   Count of refs under refs/heads/ + refs/remotes/
                        (excluding refs/remotes/*/HEAD symbolic refs)
  submodule_state       "clean" | "no_submodules" | "dirty"
  commit_date_range     { earliest, latest } across --all branches, UTC Z-form
  extraction_timestamp  Wall-clock UTC at script-run time
  run_id                BLITZY_RUN_ID (env var if pre-set, else fresh UUID4)

Also prints "BLITZY_RUN_ID=<uuid>" as the only stdout line so an orchestrator
can capture it via:  eval \$(./00_environment.sh)

Options:
  --dry-run        Print a JSON description of the git commands and writes
                   this script WOULD perform, then exit 0. No git reads, no
                   file writes. Equivalent to setting DRY_RUN=1 in the env.
  --output PATH    Write the JSON payload to PATH instead of the default.
  --help, -h       Show this help and exit 0.

Environment variables (all optional):
  BLITZY_RUN_ID    Per-run correlation ID. If unset, a fresh UUID4 is created
                   and exported. Always echoed to stdout at the end.
  DRY_RUN          When set to "1", behaves identically to --dry-run.

Per Rule 6 (AAP §0.7.2): this script MUST run before any metric extraction.
EOF
}

# -----------------------------------------------------------------------------
# log_json: emit a single-line structured JSON event to stderr
#
# Usage:  log_json LEVEL EVENT [key value [key value ...]]
#
# Values are passed via argv (NOT shell-interpolated into the Python source),
# so embedded quotes, backslashes, and newlines are safe. The BLITZY_RUN_ID
# from the surrounding script context is included automatically via env.
# -----------------------------------------------------------------------------
log_json() {
    BLITZY_LOG_SCRIPT="${SCRIPT_NAME}" \
    python3 - "$@" <<'PYEOF'
import json
import os
import sys
from datetime import datetime, timezone

argv = sys.argv[1:]
level = argv[0] if len(argv) > 0 else "info"
event = argv[1] if len(argv) > 1 else ""
extras = {}
i = 2
while i + 1 < len(argv):
    extras[str(argv[i])] = argv[i + 1]
    i += 2

event_obj = {
    "run_id": os.environ.get("BLITZY_RUN_ID", ""),
    "ts": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ"),
    "script": os.environ.get("BLITZY_LOG_SCRIPT", ""),
    "level": level,
    "event": event,
}
event_obj.update(extras)
sys.stderr.write(json.dumps(event_obj, default=str) + "\n")
sys.stderr.flush()
PYEOF
}

# -----------------------------------------------------------------------------
# main: Rule-6 environment extraction driver
# -----------------------------------------------------------------------------
main() {
    # -------------------------------------------------------------------------
    # Argument parsing
    # -------------------------------------------------------------------------
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --dry-run)
                DRY_RUN=1
                shift
                ;;
            --output)
                if [[ $# -lt 2 ]]; then
                    echo "Error: --output requires a path argument" >&2
                    exit 2
                fi
                OUTPUT_FILE="$2"
                shift 2
                ;;
            --help|-h)
                usage
                exit 0
                ;;
            --)
                shift
                break
                ;;
            *)
                echo "Unknown argument: $1" >&2
                usage >&2
                exit 2
                ;;
        esac
    done

    # -------------------------------------------------------------------------
    # BLITZY_RUN_ID: pre-set in env (from a parent run) or freshly generated.
    # -------------------------------------------------------------------------
    local _run_id_source
    if [[ -n "${BLITZY_RUN_ID:-}" ]]; then
        _run_id_source="pre-set"
    else
        _run_id_source="generated"
        BLITZY_RUN_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
    fi
    export BLITZY_RUN_ID

    # -------------------------------------------------------------------------
    # Phase 1 — Dry-run branch (no external reads, no writes)
    # -------------------------------------------------------------------------
    if [[ "${DRY_RUN}" -eq 1 ]]; then
        BLITZY_OUTPUT_FILE="${OUTPUT_FILE}" \
        BLITZY_RUN_ID_SOURCE="${_run_id_source}" \
        python3 - <<'PYEOF'
import json
import os

out = {
    "action": "dry_run",
    "script": "00_environment",
    "run_id": os.environ.get("BLITZY_RUN_ID", ""),
    "blitzy_run_id_source": os.environ.get("BLITZY_RUN_ID_SOURCE", "generated"),
    "git_commands_read_only": [
        "git --version",
        "git rev-parse --git-dir",
        "git remote get-url origin",
        "git rev-list --count HEAD",
        "git for-each-ref --format='%(refname)' refs/heads/ refs/remotes/",
        "git submodule status",
        "git log --all --reverse --pretty=format:'%aI' | head -1",
        "git log --all --pretty=format:'%aI' | head -1",
    ],
    "external_endpoints": [],
    "writes": [os.environ.get("BLITZY_OUTPUT_FILE", "")],
}
print(json.dumps(out, indent=2))
PYEOF
        exit 0
    fi

    # -------------------------------------------------------------------------
    # Phase 2 — Preflight: required tools and a valid git repository
    # -------------------------------------------------------------------------
    if ! command -v git >/dev/null 2>&1; then
        log_json error git_unavailable hint "install git 2.43+"
        exit 1
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        log_json error python3_unavailable hint "install python3 3.12+"
        exit 1
    fi
    if ! git rev-parse --git-dir >/dev/null 2>&1; then
        log_json error not_a_git_repo cwd "$(pwd)"
        exit 1
    fi

    log_json info script_started \
        run_id "${BLITZY_RUN_ID}" \
        run_id_source "${_run_id_source}" \
        workspace "${WORKSPACE_ROOT}" \
        output "${OUTPUT_FILE}"

    # -------------------------------------------------------------------------
    # Phase 3 — Repository URL (token-scrubbed)
    # -------------------------------------------------------------------------
    # Strip any embedded auth in the userinfo segment of an https URL. Matches:
    #   https://user:token@host/path
    #   https://token@host/path
    # while leaving plain https://host/path unchanged. SSH and git:// URLs are
    # already token-free so they pass through.
    local RAW_URL REPO_URL
    RAW_URL="$(git remote get-url origin 2>/dev/null || printf 'unknown')"
    REPO_URL="$(printf '%s' "${RAW_URL}" | sed -E 's|^(https?://)[^/@[:space:]]+@|\1|')"

    # -------------------------------------------------------------------------
    # Phase 4 — Git version (third field of `git --version` output)
    # -------------------------------------------------------------------------
    local GIT_VERSION
    GIT_VERSION="$(git --version | awk '{print $3}')"

    # -------------------------------------------------------------------------
    # Phase 5 — Total commit count on HEAD
    # -------------------------------------------------------------------------
    local TOTAL_COMMIT_COUNT
    TOTAL_COMMIT_COUNT="$(git rev-list --count HEAD 2>/dev/null || printf '0')"
    if ! [[ "${TOTAL_COMMIT_COUNT}" =~ ^[0-9]+$ ]]; then
        TOTAL_COMMIT_COUNT=0
    fi

    # -------------------------------------------------------------------------
    # Phase 6 — Active branch count (local + remote-tracking, excluding HEAD)
    # -------------------------------------------------------------------------
    # The `... | grep -v '/HEAD$'` removes symbolic refs like
    # refs/remotes/origin/HEAD that point to default branches (otherwise we
    # would double-count the default branch on every remote).
    local ACTIVE_BRANCH_COUNT
    ACTIVE_BRANCH_COUNT="$(
        git for-each-ref --format='%(refname)' refs/heads/ refs/remotes/ 2>/dev/null \
        | grep -Ev '/HEAD$' \
        | wc -l \
        | tr -d ' [:space:]' \
        || printf '0'
    )"
    if ! [[ "${ACTIVE_BRANCH_COUNT}" =~ ^[0-9]+$ ]]; then
        ACTIVE_BRANCH_COUNT=0
    fi

    # -------------------------------------------------------------------------
    # Phase 7 — Submodule state
    # -------------------------------------------------------------------------
    # `git submodule status` output:
    #   '-<sha>' = uninitialised
    #   '+<sha>' = checked-out SHA differs from index
    #   'U<sha>' = merge conflict
    #   ' <sha>' = clean
    # An empty result means no submodules are defined.
    local SUBMODULE_OUTPUT SUBMODULE_STATE
    SUBMODULE_OUTPUT="$(git submodule status 2>/dev/null || printf '')"
    if [[ -z "${SUBMODULE_OUTPUT}" ]]; then
        SUBMODULE_STATE="no_submodules"
    elif printf '%s\n' "${SUBMODULE_OUTPUT}" | grep -qE '^[-+U]'; then
        SUBMODULE_STATE="dirty"
    else
        SUBMODULE_STATE="clean"
    fi

    # -------------------------------------------------------------------------
    # Phase 8 — Commit date range (across --all branches)
    # -------------------------------------------------------------------------
    # `|| true` defends against SIGPIPE on the upstream `git log` when `head -1`
    # closes the pipe early under `set -o pipefail`. The kernel pipe buffer
    # typically absorbs the full output before head reads it, but defending
    # against the worst case is cheap.
    local EARLIEST_RAW LATEST_RAW
    EARLIEST_RAW="$(git log --all --reverse --pretty=format:'%aI' 2>/dev/null | head -1 || true)"
    LATEST_RAW="$(git log --all --pretty=format:'%aI' 2>/dev/null | head -1 || true)"

    # Normalise both timestamps to UTC ISO-8601 with the Z suffix. Values are
    # passed via env vars so no shell-interpolated user data ends up inside the
    # Python source string (defence in depth against quote/backslash injection).
    local EARLIEST_UTC LATEST_UTC
    EARLIEST_UTC="$(
        BLITZY_TS_RAW="${EARLIEST_RAW}" python3 -c '
import os
from datetime import datetime, timezone
s = os.environ.get("BLITZY_TS_RAW", "").strip()
if not s:
    print("")
else:
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    dt = datetime.fromisoformat(s)
    print(dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
'
    )"
    LATEST_UTC="$(
        BLITZY_TS_RAW="${LATEST_RAW}" python3 -c '
import os
from datetime import datetime, timezone
s = os.environ.get("BLITZY_TS_RAW", "").strip()
if not s:
    print("")
else:
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    dt = datetime.fromisoformat(s)
    print(dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
'
    )"

    # -------------------------------------------------------------------------
    # Phase 9 — Extraction timestamp (wall clock, UTC, Z-suffix ISO-8601)
    # -------------------------------------------------------------------------
    local EXTRACTION_TS
    EXTRACTION_TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

    # -------------------------------------------------------------------------
    # Phase 10 — Assemble JSON payload and atomically write data/environment.json
    # -------------------------------------------------------------------------
    # No jq dependency per AAP §0.4.3 — python3 assembles the JSON.
    # All values are passed via env vars (defence in depth against injection).
    # The write is atomic: write to .tmp then os.replace() to the final path.
    mkdir -p "${DATA_DIR}"

    BLITZY_REPO_URL="${REPO_URL}" \
    BLITZY_GIT_VERSION="${GIT_VERSION}" \
    BLITZY_TOTAL_COMMITS="${TOTAL_COMMIT_COUNT}" \
    BLITZY_BRANCH_COUNT="${ACTIVE_BRANCH_COUNT}" \
    BLITZY_SUBMODULE_STATE="${SUBMODULE_STATE}" \
    BLITZY_EARLIEST="${EARLIEST_UTC}" \
    BLITZY_LATEST="${LATEST_UTC}" \
    BLITZY_EXTRACTION_TS="${EXTRACTION_TS}" \
    BLITZY_OUTPUT_PATH="${OUTPUT_FILE}" \
    python3 - <<'PYEOF'
import json
import os
import sys


def _int(name: str) -> int:
    v = os.environ.get(name, "0").strip()
    try:
        return int(v)
    except ValueError:
        return 0


payload = {
    "repository_url": os.environ.get("BLITZY_REPO_URL", ""),
    "git_version": os.environ.get("BLITZY_GIT_VERSION", ""),
    "total_commit_count": _int("BLITZY_TOTAL_COMMITS"),
    "active_branch_count": _int("BLITZY_BRANCH_COUNT"),
    "submodule_state": os.environ.get("BLITZY_SUBMODULE_STATE", "no_submodules"),
    "commit_date_range": {
        "earliest": os.environ.get("BLITZY_EARLIEST", ""),
        "latest": os.environ.get("BLITZY_LATEST", ""),
    },
    "extraction_timestamp": os.environ.get("BLITZY_EXTRACTION_TS", ""),
    "run_id": os.environ.get("BLITZY_RUN_ID", ""),
}

output_path = os.environ.get("BLITZY_OUTPUT_PATH", "")
if not output_path:
    sys.stderr.write("Error: BLITZY_OUTPUT_PATH is empty\n")
    sys.exit(1)

tmp_path = output_path + ".tmp"
with open(tmp_path, "w", encoding="utf-8") as fh:
    json.dump(payload, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
os.replace(tmp_path, output_path)
PYEOF

    # -------------------------------------------------------------------------
    # Phase 11 — Final log line + BLITZY_RUN_ID echo to stdout
    # -------------------------------------------------------------------------
    log_json info script_complete \
        repo_url "${REPO_URL}" \
        git_version "${GIT_VERSION}" \
        total_commits "${TOTAL_COMMIT_COUNT}" \
        branches "${ACTIVE_BRANCH_COUNT}" \
        submodules "${SUBMODULE_STATE}" \
        earliest_commit "${EARLIEST_UTC}" \
        latest_commit "${LATEST_UTC}" \
        extraction_timestamp "${EXTRACTION_TS}" \
        output "${OUTPUT_FILE}"

    # Single-line key=value form so `eval $(./00_environment.sh)` propagates
    # BLITZY_RUN_ID into the orchestrator's environment for downstream scripts.
    echo "BLITZY_RUN_ID=${BLITZY_RUN_ID}"
}

# -----------------------------------------------------------------------------
# Dispatch
# -----------------------------------------------------------------------------
main "$@"
