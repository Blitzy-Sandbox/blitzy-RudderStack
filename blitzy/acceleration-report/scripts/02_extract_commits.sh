#!/usr/bin/env bash
# =============================================================================
# 02_extract_commits.sh — Full Commit Roster + Revert Candidates
# =============================================================================
#
# Second script in the blitzy/acceleration-report/ extraction pipeline (logical
# order; the numeric prefix `02` is positional within the topological order
# 00 → 02 → 01 → 03..08 → 09 → 10,11 — see AAP §0.5.2).
#
# Emits two read-only data artifacts derived from `git log --all` (every
# local and remote-tracking ref, per AAP §0.5.3 and §0.6.2):
#
#   data/commits.csv             Authoritative full commit roster.
#                                Header:
#                                  sha|author_email|author_name|author_date|
#                                  committer_email|committer_name|
#                                  committer_date|parents|subject
#                                Pipe-delimited (`|`). One row per commit.
#                                In this repo: 538 body rows.
#
#   data/revert_candidates.csv   Commits whose subject begins with `Revert "`.
#                                Header:  sha|date|subject
#                                In this repo: 0 body rows.
#
# These artifacts feed Metrics 1, 2, 3, 4, 5, 6, 7, 8, 10 and the Tier-3
# velocity-inflection fallback used by 01_detect_inflection.py (which is why
# 02 runs BEFORE 01 in the topological order even though the file names
# suggest the opposite).
#
# Read-only contract:
#   This script makes NO modifications to the repository, no commits, no
#   pushes, no fetches, no remote-ref updates, and no HTTP calls. Every git
#   invocation is in the read-only set: `git log`, `git rev-parse`.
#
# Observability:
#   Every event is emitted as a single-line JSON object on stderr via the
#   log_json helper. The per-run correlation ID is BLITZY_RUN_ID (either
#   pre-set in the environment by 00_environment.sh, or freshly generated
#   here as a UUID4 fallback per AAP §0.5.6 Rule: Observability).
#
# Exit codes:
#   0  success (or --help / --dry-run)
#   1  required tool missing or not in a git repository
#   2  invalid command-line argument
# =============================================================================

set -euo pipefail

# -----------------------------------------------------------------------------
# Module-level constants
# -----------------------------------------------------------------------------
SCRIPT_NAME="02_extract_commits"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DATA_DIR="${WORKSPACE_ROOT}/data"
COMMITS_FILE="${DATA_DIR}/commits.csv"
CANDIDATES_FILE="${DATA_DIR}/revert_candidates.csv"

# Default DRY_RUN to 0 but honour a pre-existing DRY_RUN env var (consistent
# with the workspace .env.example contract; the CLI --dry-run flag also
# sets it).
DRY_RUN="${DRY_RUN:-0}"

# Rev-list scope. Per AAP §0.5.3 and §0.6.2, this script extracts the FULL
# commit roster across ALL refs by default (`--all`), which is required for
# inflection detection, author-roster discovery, module weighting, and
# metric provenance to observe commits on every branch (main, release/*,
# blitzy-* feature branches, and all remote-tracking refs).
#
# An optional --branch <name> override may be supplied to narrow extraction
# to a single rev-spec for debugging on a fork or feature branch; in that
# mode the rev-spec replaces `--all` in the underlying `git log` calls.
# Default (and AAP-compliant production) behaviour is `--all`.
REV_SCOPE="--all"

# -----------------------------------------------------------------------------
# usage: human-readable help text
# -----------------------------------------------------------------------------
usage() {
    cat <<EOF
Usage: $(basename "$0") [--dry-run] [--branch REV] [--commits-output PATH] [--candidates-output PATH] [--help]

Extract full commit roster and revert-candidate list across ALL refs.

Default rev scope: --all (every local and remote-tracking ref, per AAP §0.5.3).

Outputs (default paths):
  ${COMMITS_FILE}
      Header: sha|author_email|author_name|author_date|committer_email|committer_name|committer_date|parents|subject
      One row per commit. Pipe-delimited.

  ${CANDIDATES_FILE}
      Header: sha|date|subject
      Subset of commits whose subject begins with the literal prefix \`Revert "\`.

Options:
  --dry-run                       Print a JSON description of the git commands
                                  and writes this script WOULD perform, then
                                  exit 0. No git reads, no file writes.
                                  Equivalent to setting DRY_RUN=1 in the env.
  --branch REV                    Narrow extraction to a single rev-spec
                                  (debug override). Replaces \`--all\` in the
                                  underlying \`git log\` queries. Default
                                  production behaviour is \`--all\`.
  --commits-output PATH           Write the commit roster to PATH instead of
                                  the default ${COMMITS_FILE}. PATH must
                                  resolve under \`${DATA_DIR}\`.
  --candidates-output PATH        Write the revert-candidate list to PATH
                                  instead of the default ${CANDIDATES_FILE}.
                                  PATH must resolve under \`${DATA_DIR}\`.
  --help, -h                      Show this help and exit 0.

Environment variables (all optional):
  BLITZY_RUN_ID    Per-run correlation ID. If unset, a fresh UUID4 is created
                   and exported. Set by 00_environment.sh in normal pipeline use.
  DRY_RUN          When set to "1", behaves identically to --dry-run.

Per AAP §0.5.2: this script MUST run before 01_detect_inflection.py and
03_extract_pulls.py because data/commits.csv feeds the Tier-3 velocity-
inflection fallback and provides the author roster for downstream
extraction filters.
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
# Output goes to stderr; stdout is reserved for --dry-run JSON output.
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
# validate_output_path: reject paths that escape the workspace data/ directory
#
# Purpose: prevent path traversal via `..` segments and reject absolute paths
# that resolve outside `${DATA_DIR}`. This honours the AAP §0.6.2 read-only /
# data-directory-only output contract for bash extraction scripts.
#
# Usage:  validate_output_path PATH ARG_NAME
#         echoes the normalised absolute path on stdout when valid,
#         writes an error to stderr and exits non-zero when not.
#
# Resolution rules:
#   - Relative paths anchor to `${DATA_DIR}`.
#   - Absolute paths are accepted for resolution but must still land
#     under `${DATA_DIR}` after `realpath` normalisation.
#   - `..` segments and symlinks are resolved before the boundary check.
#   - The target must be a file path, not the data directory itself.
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

if os.path.isabs(candidate):
    abs_candidate = candidate
else:
    abs_candidate = os.path.join(data_dir, candidate)

norm = os.path.realpath(abs_candidate)
norm_data = os.path.realpath(data_dir)

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

if norm == norm_data:
    sys.stderr.write(
        f"Error: {arg} path {candidate!r} resolves to the data directory "
        f"itself ({norm_data!r}); expected a file path under it.\n"
    )
    sys.exit(2)

sys.stdout.write(norm)
PYEOF
}

# -----------------------------------------------------------------------------
# main: full commit-roster + revert-candidate extraction driver
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
            --branch)
                if [[ $# -lt 2 ]]; then
                    echo "Error: --branch requires a rev-spec argument" >&2
                    exit 2
                fi
                # Override the default `--all` scope with a single rev-spec.
                # Documented as a debug-mode override per AAP §0.5.3.
                REV_SCOPE="$2"
                shift 2
                ;;
            --commits-output)
                if [[ $# -lt 2 ]]; then
                    echo "Error: --commits-output requires a path argument" >&2
                    exit 2
                fi
                # Reject paths that escape ${DATA_DIR}. The validator
                # writes an error to stderr and exits non-zero if the
                # candidate path is invalid; we propagate that via `||`.
                if ! COMMITS_FILE="$(validate_output_path "$2" "--commits-output")"; then
                    exit 2
                fi
                shift 2
                ;;
            --candidates-output)
                if [[ $# -lt 2 ]]; then
                    echo "Error: --candidates-output requires a path argument" >&2
                    exit 2
                fi
                if ! CANDIDATES_FILE="$(validate_output_path "$2" "--candidates-output")"; then
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
    # BLITZY_RUN_ID: pre-set in env (from 00_environment.sh) or freshly
    # generated as a UUID4 fallback to keep this script independently runnable.
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
    # Emits a JSON document on stdout describing exactly what reads and writes
    # the script would perform if invoked without --dry-run. This is the
    # AAP §0.5.6 "readiness check" for this extraction script — an analyst
    # can pipe `make extract` runs to confirm endpoint / file-path coverage
    # before touching anything.
    if [[ "${DRY_RUN}" -eq 1 ]]; then
        BLITZY_REV_SCOPE="${REV_SCOPE}" \
        BLITZY_COMMITS_FILE="${COMMITS_FILE}" \
        BLITZY_CANDIDATES_FILE="${CANDIDATES_FILE}" \
        BLITZY_RUN_ID_SOURCE="${_run_id_source}" \
        python3 - <<'PYEOF'
import json
import os

rev_scope = os.environ.get("BLITZY_REV_SCOPE", "--all")
out = {
    "action": "dry_run",
    "script": "02_extract_commits",
    "run_id": os.environ.get("BLITZY_RUN_ID", ""),
    "blitzy_run_id_source": os.environ.get("BLITZY_RUN_ID_SOURCE", "generated"),
    "rev_scope": rev_scope,
    "git_commands_read_only": [
        "git rev-parse --git-dir",
        (
            "git log " + rev_scope
            + " --pretty=format:'%H|%aE|%aN|%aI|%cE|%cN|%cI|%P|%s'"
        ),
        (
            "git log " + rev_scope
            + " --grep='^Revert \"' --extended-regexp"
            + " --pretty=format:'%H|%aI|%s'"
        ),
    ],
    "external_endpoints": [],
    "writes": [
        os.environ.get("BLITZY_COMMITS_FILE", ""),
        os.environ.get("BLITZY_CANDIDATES_FILE", ""),
    ],
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

    # When `--branch` was supplied as an override (REV_SCOPE != "--all"), verify
    # the rev resolves. With the default `--all` we skip this check because git
    # treats `--all` as a positional flag, not a ref.
    if [[ "${REV_SCOPE}" != "--all" ]]; then
        if ! git rev-parse --verify --quiet "${REV_SCOPE}" >/dev/null 2>&1; then
            log_json error rev_not_found rev_scope "${REV_SCOPE}" \
                hint "ensure the rev exists locally or pass --branch origin/<name>"
            exit 1
        fi
    fi

    log_json info script_started \
        run_id "${BLITZY_RUN_ID}" \
        run_id_source "${_run_id_source}" \
        rev_scope "${REV_SCOPE}" \
        commits_file "${COMMITS_FILE}" \
        candidates_file "${CANDIDATES_FILE}"

    # -------------------------------------------------------------------------
    # Phase 3 — Ensure data directory exists
    # -------------------------------------------------------------------------
    # `mkdir -p` is idempotent. If the parent directory of either output file
    # was overridden via --commits-output / --candidates-output, ensure those
    # parent directories exist too.
    mkdir -p "${DATA_DIR}"
    mkdir -p "$(dirname "${COMMITS_FILE}")"
    mkdir -p "$(dirname "${CANDIDATES_FILE}")"

    # -------------------------------------------------------------------------
    # Phase 4 — Extract full commit roster on the default branch
    # -------------------------------------------------------------------------
    # Pipe-delimited because git subjects can contain commas, tabs, colons,
    # and quotes (per AAP §0.5.6 — "Author email canonicalisation happens in
    # 09_compute_metrics.py, NOT here; this script extracts raw data exactly
    # as git reports it").
    #
    # Format fields:
    #   %H   full commit hash
    #   %aE  author email (raw, as recorded; not lower-cased here)
    #   %aN  author name (display name)
    #   %aI  author date (strict ISO-8601 with timezone offset)
    #   %cE  committer email
    #   %cN  committer name
    #   %cI  committer date (strict ISO-8601 with timezone offset)
    #   %P   parent hashes (space-separated; 2+ for merge commits)
    #   %s   subject (first line of the commit message)
    #
    # Atomic-write contract: the body is streamed to a .tmp sibling file and
    # only `mv`d into place after the streaming completes. This protects
    # downstream readers from observing a partially-written commits.csv if
    # the script is interrupted mid-extraction.
    local COMMITS_TMP="${COMMITS_FILE}.tmp"
    {
        echo 'sha|author_email|author_name|author_date|committer_email|committer_name|committer_date|parents|subject'
        # The `git log` invocation is read-only. Per AAP §0.5.3 and §0.6.2,
        # default REV_SCOPE is `--all` so that commits on every ref (main,
        # release/*, blitzy-* feature branches, all remote-tracking refs)
        # are captured. The trailing `printf '\n'` ensures the body
        # terminates with a newline (git log --pretty=format omits the
        # trailing newline by design, which would otherwise concat the last
        # commit row directly to EOF without a line terminator and break
        # standard line-counting tools like `wc -l`).
        git log "${REV_SCOPE}" --pretty=format:'%H|%aE|%aN|%aI|%cE|%cN|%cI|%P|%s'
        printf '\n'
    } > "${COMMITS_TMP}"
    mv "${COMMITS_TMP}" "${COMMITS_FILE}"

    # Body row count = total lines - 1 (header). The header always exists, so
    # the minimum count is 0 (empty repository — unusual but defended against).
    local COMMITS_TOTAL COMMITS_COUNT
    COMMITS_TOTAL="$(wc -l < "${COMMITS_FILE}" | tr -d ' [:space:]')"
    if ! [[ "${COMMITS_TOTAL}" =~ ^[0-9]+$ ]]; then
        COMMITS_TOTAL=0
    fi
    if [[ "${COMMITS_TOTAL}" -ge 1 ]]; then
        COMMITS_COUNT=$((COMMITS_TOTAL - 1))
    else
        COMMITS_COUNT=0
    fi

    # -------------------------------------------------------------------------
    # Commit-count sanity check: cross-verify the extracted row count against
    # a freshly computed `git rev-list <REV_SCOPE> --count`. The two numbers
    # MUST agree; a mismatch indicates either a streaming-write race, a
    # `git log` filter inconsistency, or a commit-message containing a literal
    # newline that was misclassified as a row separator. A mismatch is logged
    # as a structured warning (NOT a hard failure) because:
    #   - the expected size grows organically as the repo accumulates commits;
    #   - the canonical reference value (538 body rows in this repo at the AAP
    #     authorship timestamp) is informational, not a contract;
    #   - downstream readers consume `data/commits.csv` directly and benefit
    #     from a continued run even when the sanity number drifts.
    # The structured event surfaces the discrepancy so the analyst can
    # investigate.
    # -------------------------------------------------------------------------
    local EXPECTED_COMMITS_COUNT
    EXPECTED_COMMITS_COUNT="$(
        git rev-list "${REV_SCOPE}" --count 2>/dev/null \
            || printf '0'
    )"
    if ! [[ "${EXPECTED_COMMITS_COUNT}" =~ ^[0-9]+$ ]]; then
        EXPECTED_COMMITS_COUNT=0
    fi
    if [[ "${EXPECTED_COMMITS_COUNT}" -ne "${COMMITS_COUNT}" ]]; then
        log_json warning commits_count_mismatch \
            extracted_count "${COMMITS_COUNT}" \
            expected_count "${EXPECTED_COMMITS_COUNT}" \
            delta $((COMMITS_COUNT - EXPECTED_COMMITS_COUNT)) \
            rev_scope "${REV_SCOPE}" \
            hint "extracted row count differs from \`git rev-list ${REV_SCOPE} --count\`; check for newlines in commit messages or rev-scope drift"
    else
        log_json info commits_count_verified \
            count "${COMMITS_COUNT}" \
            expected_count "${EXPECTED_COMMITS_COUNT}" \
            rev_scope "${REV_SCOPE}"
    fi

    log_json info commits_extracted \
        count "${COMMITS_COUNT}" \
        expected_count "${EXPECTED_COMMITS_COUNT}" \
        rev_scope "${REV_SCOPE}" \
        output "${COMMITS_FILE}"

    # -------------------------------------------------------------------------
    # Phase 5 — Extract revert-candidate list
    # -------------------------------------------------------------------------
    # Revert candidates are commits whose subject line begins with the literal
    # prefix `Revert "`. The `--grep` pattern matches the commit message
    # (subject + body), but anchoring with `^Revert "` ensures we match only
    # commits whose subject begins with that prefix — `git log` evaluates
    # `--grep` against the full message; the `^` anchor is interpreted at the
    # start of the message which is the subject's first character.
    #
    # `--extended-regexp` enables ERE syntax (consistent with the AAP's
    # agent_prompt Phase 5 specification).
    #
    # `--regexp-ignore-case=false` is the default (case-sensitive); we rely on
    # the default to match `Revert "` exactly rather than `revert "` or
    # `REVERT "`. This is the conventional-commit convention generated by
    # `git revert <SHA>`.
    #
    # If no commits match, `git log` exits 0 with empty output. We still want
    # the header row + a clean trailing newline in the output file.
    #
    # Atomic write: same .tmp + mv pattern as Phase 4.
    local CANDIDATES_TMP="${CANDIDATES_FILE}.tmp"
    {
        echo 'sha|date|subject'
        # Capture the body in a subshell so that an empty result (the common
        # case for this repo) does not write a stray newline before the
        # trailing printf. We use `printf '%s'` to avoid `echo`'s
        # platform-dependent backslash handling. Per AAP §0.5.3 and §0.6.2,
        # default REV_SCOPE is `--all` so that reverts on any ref are
        # discovered and can be attributed to the release that contained
        # the original commit (Metric 8).
        local _revert_body
        _revert_body="$(
            git log "${REV_SCOPE}" --grep='^Revert "' --extended-regexp \
                --pretty=format:'%H|%aI|%s' 2>/dev/null \
            || printf ''
        )"
        if [[ -n "${_revert_body}" ]]; then
            printf '%s\n' "${_revert_body}"
        fi
    } > "${CANDIDATES_TMP}"
    mv "${CANDIDATES_TMP}" "${CANDIDATES_FILE}"

    local CANDIDATES_TOTAL CANDIDATES_COUNT
    CANDIDATES_TOTAL="$(wc -l < "${CANDIDATES_FILE}" | tr -d ' [:space:]')"
    if ! [[ "${CANDIDATES_TOTAL}" =~ ^[0-9]+$ ]]; then
        CANDIDATES_TOTAL=0
    fi
    if [[ "${CANDIDATES_TOTAL}" -ge 1 ]]; then
        CANDIDATES_COUNT=$((CANDIDATES_TOTAL - 1))
    else
        CANDIDATES_COUNT=0
    fi

    log_json info revert_candidates_extracted \
        count "${CANDIDATES_COUNT}" \
        rev_scope "${REV_SCOPE}" \
        output "${CANDIDATES_FILE}"

    # -------------------------------------------------------------------------
    # Phase 6 — Final completion log
    # -------------------------------------------------------------------------
    log_json info script_complete \
        commits_count "${COMMITS_COUNT}" \
        revert_candidates_count "${CANDIDATES_COUNT}" \
        commits_file "${COMMITS_FILE}" \
        candidates_file "${CANDIDATES_FILE}" \
        rev_scope "${REV_SCOPE}"
}

# -----------------------------------------------------------------------------
# Dispatch
# -----------------------------------------------------------------------------
main "$@"
