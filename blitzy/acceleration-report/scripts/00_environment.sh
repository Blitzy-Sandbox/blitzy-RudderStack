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
# Path to the canonical structured-JSON log feed (DL-032). Bash extraction
# scripts append every log_json event to this file in addition to stderr,
# so the run.log.jsonl feed is a complete record of every script's events
# (matching the Python scripts' behaviour via lib.observability.get_logger).
LOG_FILE="${DATA_DIR}/run.log.jsonl"

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
# log_json: emit a single-line structured JSON event to BOTH stderr and the
# canonical run-log file at data/run.log.jsonl (DL-032).
#
# Usage:  log_json LEVEL EVENT [key value [key value ...]]
#
# Values are passed via argv (NOT shell-interpolated into the Python source),
# so embedded quotes, backslashes, and newlines are safe. The BLITZY_RUN_ID
# from the surrounding script context is included automatically via env.
#
# Persistence to data/run.log.jsonl (DL-032):
#   The CP-FIN-1 QA review (finding FIN-1-005) observed that Bash scripts
#   00/02/05 emitted JSON events ONLY to stderr, so the run.log.jsonl feed
#   was incomplete — only the 9 Python scripts (which use lib.observability)
#   appeared in the journal. The fix mirrors every event to the file in
#   addition to stderr, so the journal is now a complete record of every
#   script's events regardless of language. The file is opened in append
#   mode with UTF-8 encoding inside the same Python heredoc so a single
#   process writes both destinations atomically with respect to the event.
#   The file path is taken from $BLITZY_LOG_FILE (exported below) so the
#   recipe-side caller can override it for tests; failure to open the
#   file (e.g., read-only filesystem) is silently swallowed at the Python
#   layer rather than aborting the recipe — losing the journal entry is
#   strictly less harmful than failing the extraction. Stderr writing is
#   always attempted regardless of file-write success.
# -----------------------------------------------------------------------------
log_json() {
    BLITZY_LOG_SCRIPT="${SCRIPT_NAME}" \
    BLITZY_LOG_FILE="${LOG_FILE:-}" \
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
line = json.dumps(event_obj, default=str) + "\n"

# Persist to the canonical run-log file when BLITZY_LOG_FILE is set and
# non-empty. The parent directory is created if missing so a fresh
# invocation (where data/ does not yet exist) still records its events.
# Any I/O error during persistence is intentionally swallowed: losing a
# log line is strictly less harmful than failing the extraction.
log_file = os.environ.get("BLITZY_LOG_FILE", "")
if log_file:
    try:
        parent = os.path.dirname(log_file)
        if parent:
            os.makedirs(parent, exist_ok=True)
        with open(log_file, "a", encoding="utf-8") as fh:
            fh.write(line)
    except (OSError, ValueError):
        pass

sys.stderr.write(line)
sys.stderr.flush()
PYEOF
}

# -----------------------------------------------------------------------------
# validate_output_path: reject paths that escape the workspace data/ directory
#
# Purpose: prevent path traversal via `..` segments and rejection of absolute
# paths that resolve outside `${DATA_DIR}`. This honours the AAP §0.6.2
# read-only / data-directory-only output contract for bash extraction scripts.
#
# Usage:  validate_output_path PATH ARG_NAME
#         echoes the normalised absolute path on stdout when valid,
#         writes an error to stderr and exits non-zero when not.
#
# Resolution rules:
#   - Relative paths anchor to `${DATA_DIR}` (so `--output foo.json` means
#     `${DATA_DIR}/foo.json`).
#   - Absolute paths are accepted as-is for resolution but must still
#     resolve under `${DATA_DIR}` after normalisation.
#   - `..` segments are normalised by `os.path.realpath`; a path that
#     resolves outside `${DATA_DIR}` is rejected with exit code 2.
#   - Symlinks are resolved (`os.path.realpath`) before the boundary check
#     so a symlink target outside `${DATA_DIR}` is also rejected.
#   - The path itself must be a file path, not the data directory itself.
# -----------------------------------------------------------------------------
validate_output_path() {
    BLITZY_PATH_CANDIDATE="${1:-}" \
    BLITZY_PATH_DATA_DIR="${DATA_DIR}" \
    BLITZY_PATH_ARG_NAME="${2:-output}" \
    python3 - <<'PYEOF'
import os
import sys

candidate = os.environ.get("BLITZY_PATH_CANDIDATE", "")
data_dir = os.environ.get("BLITZY_PATH_DATA_DIR", "")
arg = os.environ.get("BLITZY_PATH_ARG_NAME", "output")

if not candidate:
    sys.stderr.write(f"Error: {arg} requires a non-empty path argument\n")
    sys.exit(2)

# Relative paths anchor to the workspace data directory. Absolute paths
# are accepted for resolution but must still land under data_dir.
if os.path.isabs(candidate):
    abs_candidate = candidate
else:
    abs_candidate = os.path.join(data_dir, candidate)

# realpath resolves symlinks and normalises `..` and `.` segments.
norm = os.path.realpath(abs_candidate)
norm_data = os.path.realpath(data_dir)

# Boundary check. commonpath raises ValueError when paths share no root
# (e.g., different drives on Windows or one path is empty).
try:
    common = os.path.commonpath([norm, norm_data])
except ValueError:
    sys.stderr.write(
        f"Error: {arg} path {candidate!r} resolves to {norm!r} which is "
        f"not comparable to the workspace data directory {norm_data!r}.\n"
    )
    sys.exit(2)

if common != norm_data:
    sys.stderr.write(
        f"Error: {arg} path {candidate!r} resolves to {norm!r} which is "
        f"OUTSIDE the workspace data directory {norm_data!r}. The script's "
        f"read-only contract requires all outputs to live under data/.\n"
    )
    sys.exit(2)

# A file path is required; the directory itself is not a valid output.
if norm == norm_data:
    sys.stderr.write(
        f"Error: {arg} path {candidate!r} resolves to the data directory "
        f"itself ({norm_data!r}); expected a file path under it.\n"
    )
    sys.exit(2)

# Emit the validated, normalised absolute path. Caller captures via $(...).
sys.stdout.write(norm)
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
                # Reject paths that escape ${DATA_DIR}. The validator
                # writes an error to stderr and exits non-zero if the
                # candidate path is invalid; we propagate that via `||`.
                if ! OUTPUT_FILE="$(validate_output_path "$2" "--output")"; then
                    exit 2
                fi
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
    # Phase 3 — Repository URL (token-scrubbed) and slug (owner/repo)
    # -------------------------------------------------------------------------
    # Strip any embedded auth in the userinfo segment of an https URL. Matches:
    #   https://user:token@host/path
    #   https://token@host/path
    # while leaving plain https://host/path unchanged. SSH and git:// URLs are
    # already token-free so they pass through.
    local RAW_URL REPO_URL REPO_SLUG
    RAW_URL="$(git remote get-url origin 2>/dev/null || printf 'unknown')"
    REPO_URL="$(printf '%s' "${RAW_URL}" | sed -E 's|^(https?://)[^/@[:space:]]+@|\1|')"
    # Derive owner/repo slug from the (token-scrubbed) URL. Handles the four
    # canonical Git remote forms: https://host/owner/repo[.git], ssh://...,
    # git@host:owner/repo[.git], and SCP-style host:owner/repo[.git]. The
    # tail of the URL (after the last ':' for ssh-style, or after the host
    # for https) is normalised by stripping leading/trailing slashes and a
    # trailing '.git' so the slug is always 'owner/repo'.
    REPO_SLUG="$(
        printf '%s' "${REPO_URL}" \
        | sed -E -e 's|^[a-z]+://[^/]+/||' -e 's|^[^@]+@[^:]+:||' -e 's|\.git$||' -e 's|^/||' -e 's|/$||'
    )"

    # -------------------------------------------------------------------------
    # Phase 3b — Default branch name (best-effort)
    # -------------------------------------------------------------------------
    # Probe the symbolic-ref of origin/HEAD first (most reliable when the
    # remote was cloned from a server that publishes a default-branch hint).
    # Fall back to 'main' if the symbolic-ref is missing, and finally to the
    # current branch's local short name.
    local DEFAULT_BRANCH
    DEFAULT_BRANCH="$(
        git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null \
        | sed -E 's|^origin/||'
    )"
    if [[ -z "${DEFAULT_BRANCH}" ]]; then
        if git show-ref --verify --quiet refs/heads/main 2>/dev/null; then
            DEFAULT_BRANCH="main"
        elif git show-ref --verify --quiet refs/heads/master 2>/dev/null; then
            DEFAULT_BRANCH="master"
        else
            DEFAULT_BRANCH="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || printf 'main')"
        fi
    fi

    # -------------------------------------------------------------------------
    # Phase 3c — Go module version (from go.mod, if present)
    # -------------------------------------------------------------------------
    # The `go` directive in go.mod (e.g., 'go 1.26.1') is the language
    # version pin for the rudder-server toolchain. The value is recorded
    # as part of Environment Verification because the build toolchain
    # version is provenance evidence per Rule 1 in the Reproducibility
    # Appendix. An empty value is preserved as the literal empty string —
    # the renderer surfaces it as "(absent)" when consumed.
    local GO_MODULE_VERSION
    GO_MODULE_VERSION=""
    if [[ -f "${WORKSPACE_ROOT}/../../go.mod" ]]; then
        GO_MODULE_VERSION="$(
            awk '/^go [0-9]+(\.[0-9]+){1,2}/ { print $2; exit }' \
                "${WORKSPACE_ROOT}/../../go.mod" 2>/dev/null \
            || printf ''
        )"
    elif git ls-files -- '*go.mod' >/dev/null 2>&1; then
        # Locate the closest go.mod via git ls-files (relative to repo root).
        local GO_MOD_PATH
        GO_MOD_PATH="$(git ls-files -- 'go.mod' | head -1 || true)"
        if [[ -n "${GO_MOD_PATH}" && -f "${GO_MOD_PATH}" ]]; then
            GO_MODULE_VERSION="$(
                awk '/^go [0-9]+(\.[0-9]+){1,2}/ { print $2; exit }' \
                    "${GO_MOD_PATH}" 2>/dev/null \
                || printf ''
            )"
        fi
    fi

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
    # Phase 8 — Commit date range (across --all branches and on default branch)
    # -------------------------------------------------------------------------
    # `|| true` defends against SIGPIPE on the upstream `git log` when `head -1`
    # closes the pipe early under `set -o pipefail`. The kernel pipe buffer
    # typically absorbs the full output before head reads it, but defending
    # against the worst case is cheap.
    local EARLIEST_RAW LATEST_RAW LATEST_ON_MAIN_RAW
    EARLIEST_RAW="$(git log --all --reverse --pretty=format:'%aI' 2>/dev/null | head -1 || true)"
    LATEST_RAW="$(git log --all --pretty=format:'%aI' 2>/dev/null | head -1 || true)"
    # latest_on_main is the HEAD committer timestamp of the default branch.
    # Distinct from LATEST_RAW which spans --all refs and may include
    # feature-branch activity beyond the most recent main merge.
    LATEST_ON_MAIN_RAW="$(git log -1 "${DEFAULT_BRANCH}" --pretty=format:'%cI' 2>/dev/null || true)"

    # Normalise all three timestamps to UTC ISO-8601 with the Z suffix. Values
    # are passed via env vars so no shell-interpolated user data ends up inside
    # the Python source string (defence in depth against quote/backslash
    # injection).
    local EARLIEST_UTC LATEST_UTC LATEST_ON_MAIN_UTC
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
    LATEST_ON_MAIN_UTC="$(
        BLITZY_TS_RAW="${LATEST_ON_MAIN_RAW}" python3 -c '
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
    BLITZY_REPO_SLUG="${REPO_SLUG}" \
    BLITZY_DEFAULT_BRANCH="${DEFAULT_BRANCH}" \
    BLITZY_GO_VERSION="${GO_MODULE_VERSION}" \
    BLITZY_GIT_VERSION="${GIT_VERSION}" \
    BLITZY_TOTAL_COMMITS="${TOTAL_COMMIT_COUNT}" \
    BLITZY_BRANCH_COUNT="${ACTIVE_BRANCH_COUNT}" \
    BLITZY_SUBMODULE_STATE="${SUBMODULE_STATE}" \
    BLITZY_EARLIEST="${EARLIEST_UTC}" \
    BLITZY_LATEST="${LATEST_UTC}" \
    BLITZY_LATEST_ON_MAIN="${LATEST_ON_MAIN_UTC}" \
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


earliest = os.environ.get("BLITZY_EARLIEST", "")
latest = os.environ.get("BLITZY_LATEST", "")
latest_on_main = os.environ.get("BLITZY_LATEST_ON_MAIN", "")

payload = {
    "repository_url": os.environ.get("BLITZY_REPO_URL", ""),
    "repository_slug": os.environ.get("BLITZY_REPO_SLUG", ""),
    "default_branch": os.environ.get("BLITZY_DEFAULT_BRANCH", "main"),
    "go_module_version": os.environ.get("BLITZY_GO_VERSION", ""),
    "go_module_version_source": "go.mod 'go' directive",
    "git_version": os.environ.get("BLITZY_GIT_VERSION", ""),
    "total_commit_count": _int("BLITZY_TOTAL_COMMITS"),
    "active_branch_count": _int("BLITZY_BRANCH_COUNT"),
    "submodule_state": os.environ.get("BLITZY_SUBMODULE_STATE", "no_submodules"),
    "commit_date_range": {
        # Compatibility aliases (original Rule 6 contract) are preserved.
        "earliest": earliest,
        "latest": latest,
        # Canonical CP2-contract field names below.
        "earliest_utc": earliest,
        "latest_utc": latest,
        "latest_on_main": latest_on_main,
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
        repo_slug "${REPO_SLUG}" \
        default_branch "${DEFAULT_BRANCH}" \
        go_module_version "${GO_MODULE_VERSION}" \
        git_version "${GIT_VERSION}" \
        total_commits "${TOTAL_COMMIT_COUNT}" \
        branches "${ACTIVE_BRANCH_COUNT}" \
        submodules "${SUBMODULE_STATE}" \
        earliest_commit "${EARLIEST_UTC}" \
        latest_commit "${LATEST_UTC}" \
        latest_on_main "${LATEST_ON_MAIN_UTC}" \
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
