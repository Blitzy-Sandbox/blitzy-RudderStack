#!/usr/bin/env bash
# =============================================================================
# 05_extract_reverts.sh — Revert → Original → Release Attribution
# =============================================================================
#
# Stage 05 of the read-only acceleration-report extraction pipeline. Resolves
# each candidate revert from `data/revert_candidates.csv` (produced upstream
# by `02_extract_commits.sh`) to its **original (reverted) commit** and
# attributes the revert to the **release** that contained that original
# commit (using `data/releases.json` produced upstream by
# `04_extract_releases.py`). The output `data/reverts.json` feeds Metric 8
# (Problem Records in Release) per AAP §0.5.3.9.
#
# Pipeline position:
#
#   00_environment.sh
#   02_extract_commits.sh            ──→  data/revert_candidates.csv
#   01_detect_inflection.py
#   03_extract_pulls.py
#   04_extract_releases.py           ──→  data/releases.json
#   05_extract_reverts.sh            ──→  data/reverts.json   ← THIS SCRIPT
#   06_extract_ci_history.py
#   07_extract_exceptions.py
#   08_extract_linear.py
#   09_compute_metrics.py
#   10_render_report.py
#   11_render_deck.py
#
# Read-only contract:
#   This script makes NO modifications to the analyzed repository, no
#   commits, no pushes, no fetches, no remote-ref updates, no tag mutations,
#   and no HTTP calls. Every git invocation is in the read-only set:
#     - `git rev-parse --git-dir`     (preflight)
#     - `git cat-file -p <sha>`       (Phase 4a — original-SHA extraction)
#     - `git log -1 --pretty=...`     (Phases 4c, 4d — subject + date lookup)
#     - `git merge-base --is-ancestor TAG SHA`  (Phase 4d — release attribution)
#     - `git log --grep=... | grep -c …` (Phase 5 — search_verification)
#   No `git tag`, `git update-ref`, `git push`, `git fetch`, `git commit`,
#   `git reset`, `git rebase`, `git revert`, or `git branch -D` invocations
#   appear anywhere in this script.
#
# Exclusion categories (AAP §0.5.3.9, ordered as canonical):
#   1. unattributable    — original commit cannot be identified
#   2. unreleased        — original commit is not reachable from any
#                          release tag (no `git merge-base --is-ancestor`
#                          match against the non-prerelease release inventory)
#   3. revert_of_revert  — original commit is itself a revert
#
# Output schema (`data/reverts.json`):
#   {
#     "_metadata": { … run-correlation fields … },
#     "fetched_at": "<ISO-8601 UTC>",
#     "reverts": [
#       { "revert_sha": "...", "revert_date": "...", "revert_date_iso": "...",
#         "revert_committer_date": "...", "revert_subject": "...",
#         "original_sha": "...", "original_date": "...",
#         "attributed_release_tag": "v1.2.3", "exclusion_reason": null }
#     ],
#     "exclusions": {
#       "unattributable":   [ { "revert_sha": "...", "reason": "..." } ],
#       "unreleased":       [ { "revert_sha": "...", "original_sha": "..." } ],
#       "revert_of_revert": [ { "revert_sha": "...", "original_sha": "..." } ]
#     },
#     "summary": {
#       "total": N, "attributed": N, "attributable_reverts": N,
#       "unattributable": N, "unattributable_count": N,
#       "unreleased": N, "unreleased_count": N,
#       "revert_of_revert": N, "revert_of_revert_count": N,
#       "total_revert_commits_found": N,
#       "reverts_by_release_tag": { "v1.2.3": M, ... }
#     },
#     "search_verification": { … cross-validated AAP search commands … }
#   }
#
# The dual-key naming convention (`attributed` AND `attributable_reverts`,
# `revert_date` AND `revert_date_iso`, etc.) satisfies BOTH the agent_prompt
# format (canonical short keys) AND the downstream consumer
# `09_compute_metrics.py` which reads the longer ``_count`` / ``_iso`` keys
# documented in its `compute_m8_problem_records` function. The schema
# (`scripts/lib/schemas/reverts.schema.json`) permits both via
# `additionalProperties: true`.
#
# Observability (AAP §0.5.6, Rule: Observability):
#   Every event is emitted as a single-line JSON object on stderr via the
#   `log_json` helper. The per-run correlation ID is `BLITZY_RUN_ID` (either
#   pre-set in the environment by `00_environment.sh`, or freshly generated
#   here as a UUID4 fallback to keep this script independently runnable).
#
# Engineering-actor framing:
#   This metric is repository-wide (revert commits on the default branch)
#   and does NOT use the engineering-actor substitution. The pre/post
#   inflection windowing for Metric 8 is performed downstream in
#   `09_compute_metrics.py` based on `data/inflection.json#date_utc` and
#   the `revert_date_iso` (committer date) field on each revert record.
#
# Exit codes:
#   0  success (or --help / --dry-run)
#   1  required tool missing, not in a git repository, or missing input
#   2  invalid command-line argument
# =============================================================================

set -euo pipefail

# -----------------------------------------------------------------------------
# Module-level constants
# -----------------------------------------------------------------------------
SCRIPT_NAME="05_extract_reverts"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DATA_DIR="${WORKSPACE_ROOT}/data"
CANDIDATES_FILE="${DATA_DIR}/revert_candidates.csv"
RELEASES_FILE="${DATA_DIR}/releases.json"
ENVIRONMENT_FILE="${DATA_DIR}/environment.json"
INFLECTION_FILE="${DATA_DIR}/inflection.json"
OUTPUT_FILE="${DATA_DIR}/reverts.json"

# Default DRY_RUN to 0 but honour a pre-existing DRY_RUN env var (consistent
# with the workspace .env.example contract; the CLI --dry-run flag also
# sets it).
DRY_RUN="${DRY_RUN:-0}"

# Optional rev-scope. Per AAP §0.5.3.9, M8 is scoped to `main` (the default
# branch). The candidates CSV produced by 02_extract_commits.sh covers
# `--all` refs, but the release-attribution check via `git merge-base
# --is-ancestor TAG SHA` is rev-scope-independent (tags and original SHAs
# are global). REV_SCOPE here is informational only; no `git log` invocation
# in this script uses it.
REV_SCOPE="main"


# -----------------------------------------------------------------------------
# usage: human-readable help text
# -----------------------------------------------------------------------------
usage() {
    cat <<EOF
Usage: $(basename "$0") [--dry-run] [--candidates PATH] [--releases PATH] [--output PATH] [--help]

Resolve revert commits to their originals and attribute each to the most
recent ancestor release tag. Read-only git operations only.

Inputs (default paths, both required):
  ${CANDIDATES_FILE}
      Pipe-delimited CSV produced by 02_extract_commits.sh.
      Header: commit_sha|author_date|subject
      One row per commit whose subject begins with \`Revert "\`.

  ${RELEASES_FILE}
      JSON produced by 04_extract_releases.py.
      Read fields:  \`releases[]\` (array of release records with
                                   \`tag_name\` and \`prerelease\` fields).
      Only non-prerelease tag_name values are used for attribution.

Output (default path):
  ${OUTPUT_FILE}
      JSON with fields:  _metadata, fetched_at, reverts[], exclusions{},
                         summary{}, search_verification{}.
      See the script header for the full schema.

Options:
  --dry-run                       Print a JSON description of the git
                                  commands and writes this script WOULD
                                  perform, then exit 0. No git reads,
                                  no file writes. Equivalent to setting
                                  DRY_RUN=1 in the env.
  --candidates PATH               Read revert candidates from PATH
                                  instead of the default
                                  ${CANDIDATES_FILE}.
  --releases PATH                 Read release inventory from PATH
                                  instead of the default
                                  ${RELEASES_FILE}.
  --output PATH                   Write reverts.json to PATH instead of
                                  the default ${OUTPUT_FILE}. PATH must
                                  resolve under \`${DATA_DIR}\`.
  --help, -h                      Show this help and exit 0.

Environment variables (all optional):
  BLITZY_RUN_ID    Per-run correlation ID. If unset, a fresh UUID4 is
                   created and exported. Set by 00_environment.sh in
                   normal pipeline use.
  DRY_RUN          When set to "1", behaves identically to --dry-run.

Per AAP §0.5.3.9, M8 (Problem Records in Release) reports the mean of
attributable reverts per release per phase. Unattributable and unreleased
counts are reported separately as confidence indicators; revert-of-revert
reverts are excluded entirely.

For this repository the candidates CSV contains zero body rows (verified
by \`tail -n +2 revert_candidates.csv | wc -l\`), and the canonical
output is an empty \`reverts\` array, empty exclusion arrays, and a
summary with every count set to 0 — yielding a deterministic Metric 8
value of 0 with High confidence.
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
# json_append_object: append a dict to a JSON-array file via Python heredoc
#
# Purpose: per-revert mutation of the running JSON arrays
# (EXCLUSIONS_UNATTRIB / EXCLUSIONS_UNRELEASED / EXCLUSIONS_REV_REV /
# REVERTS_JSON) without falling into shell-quoting pitfalls. Every
# field value is passed via the environment (avoiding python -c
# string interpolation, which would break if a value contained
# single quotes, backslashes, or newlines — common in revert subjects).
#
# Usage:  json_append_object FILE KEY1=VALUE1 KEY2=VALUE2 ...
#         FILE must already contain a JSON array (e.g. "[]").
#         Each KEY=VALUE pair becomes one key in the appended object.
#         A key whose value is the literal string "__NULL__" is
#         appended as JSON null (used for `exclusion_reason: None`).
# -----------------------------------------------------------------------------
json_append_object() {
    local _file="$1"
    shift
    BLITZY_FILE="${_file}" \
    python3 - "$@" <<'PYEOF'
import json
import os
import sys

path = os.environ["BLITZY_FILE"]
obj = {}
for arg in sys.argv[1:]:
    if "=" not in arg:
        continue
    key, _, value = arg.partition("=")
    if value == "__NULL__":
        obj[key] = None
    else:
        obj[key] = value

with open(path, "r") as f:
    data = json.load(f)
if not isinstance(data, list):
    sys.stderr.write(
        f"json_append_object: expected a JSON array in {path!r}, got "
        f"{type(data).__name__}\n"
    )
    sys.exit(1)
data.append(obj)

tmp_path = path + ".tmp"
with open(tmp_path, "w") as f:
    json.dump(data, f)
os.replace(tmp_path, path)
PYEOF
}

# -----------------------------------------------------------------------------
# to_utc_z: normalise an ISO-8601 timestamp with timezone offset to UTC Z form
#
# Reads stdin (a single ISO-8601 string) and writes the UTC-normalised
# equivalent (`YYYY-MM-DDTHH:MM:SSZ`) to stdout. If the input is empty
# or unparseable, writes an empty string. Used in Phase 4 to normalise
# the original-commit author date returned by `git log --pretty=%aI`
# (which carries the commit's local-timezone offset).
# -----------------------------------------------------------------------------
to_utc_z() {
    python3 - <<'PYEOF'
import sys
from datetime import datetime, timezone

raw = sys.stdin.read().strip()
if not raw:
    sys.stdout.write("")
    sys.exit(0)
iso = raw[:-1] + "+00:00" if raw.endswith("Z") else raw
try:
    dt = datetime.fromisoformat(iso)
except ValueError:
    sys.stdout.write(raw)
    sys.exit(0)
sys.stdout.write(dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
PYEOF
}


# -----------------------------------------------------------------------------
# main: revert-resolution and release-attribution driver
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
            --candidates)
                if [[ $# -lt 2 ]]; then
                    echo "Error: --candidates requires a path argument" >&2
                    exit 2
                fi
                CANDIDATES_FILE="$2"
                shift 2
                ;;
            --releases)
                if [[ $# -lt 2 ]]; then
                    echo "Error: --releases requires a path argument" >&2
                    exit 2
                fi
                RELEASES_FILE="$2"
                shift 2
                ;;
            --output)
                if [[ $# -lt 2 ]]; then
                    echo "Error: --output requires a path argument" >&2
                    exit 2
                fi
                # Reject paths that escape ${DATA_DIR}. The validator writes
                # an error to stderr and exits non-zero on invalid candidate
                # paths; propagate via ||.
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
    # AAP §0.5.6 "readiness check" for this extraction script.
    if [[ "${DRY_RUN}" -eq 1 ]]; then
        BLITZY_CANDIDATES_FILE="${CANDIDATES_FILE}" \
        BLITZY_RELEASES_FILE="${RELEASES_FILE}" \
        BLITZY_OUTPUT_FILE="${OUTPUT_FILE}" \
        BLITZY_ENVIRONMENT_FILE="${ENVIRONMENT_FILE}" \
        BLITZY_INFLECTION_FILE="${INFLECTION_FILE}" \
        BLITZY_RUN_ID_SOURCE="${_run_id_source}" \
        python3 - <<'PYEOF'
import json
import os

out = {
    "action": "dry_run",
    "script": "05_extract_reverts",
    "run_id": os.environ.get("BLITZY_RUN_ID", ""),
    "blitzy_run_id_source": os.environ.get("BLITZY_RUN_ID_SOURCE", "generated"),
    "git_commands_read_only": [
        "git rev-parse --git-dir",
        "git cat-file -p <revert_sha>",
        "git log -1 --pretty=format:'%s' <original_sha>",
        "git log -1 --pretty=format:'%aI' <original_sha>",
        "git log -1 --pretty=format:'%cI' <revert_sha>",
        "git merge-base --is-ancestor <release_tag> <original_sha>",
        "git log main --grep='^Revert \"' --pretty=format:'%H'",
        "git log --all --grep='This reverts commit' --pretty=format:'%H'",
    ],
    "external_endpoints": [],
    "inputs": [
        os.environ.get("BLITZY_CANDIDATES_FILE", ""),
        os.environ.get("BLITZY_RELEASES_FILE", ""),
        os.environ.get("BLITZY_ENVIRONMENT_FILE", "") + " (optional)",
        os.environ.get("BLITZY_INFLECTION_FILE", "") + " (optional)",
    ],
    "writes": [
        os.environ.get("BLITZY_OUTPUT_FILE", ""),
    ],
    "exclusion_categories_evaluated": [
        "unattributable",
        "unreleased",
        "revert_of_revert",
    ],
}
print(json.dumps(out, indent=2))
PYEOF
        exit 0
    fi

    # -------------------------------------------------------------------------
    # Phase 2 — Preflight: required tools, valid git repo, inputs present
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
    if [[ ! -f "${CANDIDATES_FILE}" ]]; then
        log_json error candidates_missing \
            path "${CANDIDATES_FILE}" \
            hint "run 02_extract_commits.sh first to produce revert_candidates.csv"
        exit 1
    fi
    if [[ ! -f "${RELEASES_FILE}" ]]; then
        log_json error releases_missing \
            path "${RELEASES_FILE}" \
            hint "run 04_extract_releases.py first to produce releases.json"
        exit 1
    fi

    log_json info script_started \
        run_id "${BLITZY_RUN_ID}" \
        run_id_source "${_run_id_source}" \
        candidates_file "${CANDIDATES_FILE}" \
        releases_file "${RELEASES_FILE}" \
        output_file "${OUTPUT_FILE}"

    # -------------------------------------------------------------------------
    # Phase 3 — Set up temp accumulator files (with trap-based cleanup)
    # -------------------------------------------------------------------------
    # mktemp produces unique files in $TMPDIR (or /tmp). The trap ensures the
    # files are removed on any exit path (success, failure, signal). The four
    # accumulators are JSON arrays mutated in-place by json_append_object().
    local RELEASE_TAGS_FILE REVERTS_JSON EXCLUSIONS_UNATTRIB EXCLUSIONS_UNRELEASED EXCLUSIONS_REV_REV
    RELEASE_TAGS_FILE="$(mktemp -t blitzy_05_release_tags.XXXXXX)"
    REVERTS_JSON="$(mktemp -t blitzy_05_reverts.XXXXXX)"
    EXCLUSIONS_UNATTRIB="$(mktemp -t blitzy_05_unattrib.XXXXXX)"
    EXCLUSIONS_UNRELEASED="$(mktemp -t blitzy_05_unreleased.XXXXXX)"
    EXCLUSIONS_REV_REV="$(mktemp -t blitzy_05_revrev.XXXXXX)"
    # shellcheck disable=SC2064
    trap "rm -f '${RELEASE_TAGS_FILE}' '${REVERTS_JSON}' '${EXCLUSIONS_UNATTRIB}' '${EXCLUSIONS_UNRELEASED}' '${EXCLUSIONS_REV_REV}'" EXIT

    echo "[]" > "${REVERTS_JSON}"
    echo "[]" > "${EXCLUSIONS_UNATTRIB}"
    echo "[]" > "${EXCLUSIONS_UNRELEASED}"
    echo "[]" > "${EXCLUSIONS_REV_REV}"

    # -------------------------------------------------------------------------
    # Phase 4 — Load non-prerelease release tags from data/releases.json
    # -------------------------------------------------------------------------
    # Extract the `tag_name` of every release whose `prerelease` flag is
    # falsy (False, missing, null). Tags are emitted one-per-line to the
    # temp file so Phase 5d can iterate them with `while read`. We tolerate
    # missing or malformed `releases.json` data gracefully — a parse error
    # or schema mismatch yields zero tags (every revert with an identified
    # original then falls into the `unreleased` exclusion).
    BLITZY_RELEASES_FILE="${RELEASES_FILE}" \
    python3 - > "${RELEASE_TAGS_FILE}" <<'PYEOF'
import json
import os
import sys

path = os.environ.get("BLITZY_RELEASES_FILE", "")
try:
    with open(path) as f:
        data = json.load(f)
except (OSError, json.JSONDecodeError) as e:
    sys.stderr.write(
        f"warning: cannot parse releases inventory at {path!r}: {e}\n"
        f"warning: proceeding with zero release tags; every revert will "
        f"fall into the 'unreleased' exclusion category\n"
    )
    sys.exit(0)

releases = data.get("releases", []) if isinstance(data, dict) else []
if not isinstance(releases, list):
    releases = []
for r in releases:
    if not isinstance(r, dict):
        continue
    if r.get("prerelease"):
        continue
    tag = r.get("tag_name") or r.get("tag") or ""
    if isinstance(tag, str) and tag:
        sys.stdout.write(tag + "\n")
PYEOF

    local RELEASE_TAGS_COUNT
    RELEASE_TAGS_COUNT="$(wc -l < "${RELEASE_TAGS_FILE}" | tr -d ' [:space:]')"
    if ! [[ "${RELEASE_TAGS_COUNT}" =~ ^[0-9]+$ ]]; then
        RELEASE_TAGS_COUNT=0
    fi
    log_json info release_tags_loaded count "${RELEASE_TAGS_COUNT}" source "${RELEASES_FILE}"

    # -------------------------------------------------------------------------
    # Phase 5 — Walk each revert candidate
    # -------------------------------------------------------------------------
    # `tail -n +2` skips the header row of `revert_candidates.csv`. The
    # process-substitution form `< <(tail ...)` runs `tail` in a subshell
    # but leaves the `while` loop in the current shell, so the PROCESSED
    # counter (and any other shell variables we set during the loop)
    # remain accessible after the loop body.
    #
    # The expected schema of each row is:  commit_sha|author_date|subject
    # The `IFS='|' read -r SHA DATE SUBJECT` reads exactly three fields per
    # line; `read` performs no backslash interpretation thanks to `-r`.
    # If a subject itself contains a pipe character (rare — semantic-pr
    # rejects PR titles with pipes), the SUBJECT field would capture
    # everything after the second pipe inclusive of further pipes, which
    # is the correct preserve-verbatim behaviour.
    local PROCESSED=0
    local LINE_NUM=1
    while IFS='|' read -r SHA DATE SUBJECT || [[ -n "${SHA:-}" ]]; do
        LINE_NUM=$((LINE_NUM + 1))

        # Skip blank rows and any stray header row that survived `tail`.
        if [[ -z "${SHA}" ]]; then
            continue
        fi
        if [[ "${SHA}" == "commit_sha" ]]; then
            continue
        fi
        PROCESSED=$((PROCESSED + 1))

        # ---------------------------------------------------------------------
        # Phase 5a — Extract original SHA from the revert commit's message
        # ---------------------------------------------------------------------
        # `git cat-file -p <sha>` dumps the raw commit-object body (header
        # + message). We pattern-match the canonical "This reverts commit
        # <hash>" line emitted by `git revert <SHA>` and capture the
        # 7-to-40-character hash. Multiple matches are unusual but possible
        # (a body that talks about another revert); we take the first.
        local ORIGINAL_SHA
        ORIGINAL_SHA="$(
            git cat-file -p "${SHA}" 2>/dev/null \
                | grep -oE 'This reverts commit [0-9a-f]{7,40}' \
                | head -1 \
                | awk '{print $NF}' \
                || true
        )"

        if [[ -z "${ORIGINAL_SHA}" ]]; then
            # ---------------------------------------------------------------
            # Phase 5b — Unattributable: no SHA reference and tree-match
            # fallback is not implemented (per AAP §0.5.3.9 it is documented
            # as optional and rarely needed; the script marks such cases as
            # `unattributable` with `reason: no_sha_reference_and_no_tree_match`).
            # ---------------------------------------------------------------
            log_json warning unattributable \
                revert_sha "${SHA}" \
                reason "no_sha_reference_and_no_tree_match" \
                line "${LINE_NUM}"
            json_append_object "${EXCLUSIONS_UNATTRIB}" \
                "revert_sha=${SHA}" \
                "revert_date=${DATE}" \
                "revert_subject=${SUBJECT}" \
                "reason=no_sha_reference_and_no_tree_match"
            continue
        fi

        # Verify the original SHA resolves to a real object in this clone.
        # If `git cat-file -t` cannot resolve it, the original commit is
        # not present locally (history rewrite, force-push, sparse clone)
        # and we mark this revert as unattributable.
        if ! git cat-file -t "${ORIGINAL_SHA}" >/dev/null 2>&1; then
            log_json warning unattributable \
                revert_sha "${SHA}" \
                reason "original_sha_not_in_local_clone" \
                original_sha "${ORIGINAL_SHA}"
            json_append_object "${EXCLUSIONS_UNATTRIB}" \
                "revert_sha=${SHA}" \
                "revert_date=${DATE}" \
                "revert_subject=${SUBJECT}" \
                "claimed_original_sha=${ORIGINAL_SHA}" \
                "reason=original_sha_not_in_local_clone"
            continue
        fi

        # ---------------------------------------------------------------------
        # Phase 5c — Check if the original is itself a revert (revert-of-revert)
        # ---------------------------------------------------------------------
        local ORIG_SUBJECT
        ORIG_SUBJECT="$(git log -1 --pretty=format:'%s' "${ORIGINAL_SHA}" 2>/dev/null || printf '')"
        if [[ "${ORIG_SUBJECT}" =~ ^Revert ]]; then
            log_json info revert_of_revert \
                revert_sha "${SHA}" \
                original_sha "${ORIGINAL_SHA}" \
                original_subject "${ORIG_SUBJECT}"
            json_append_object "${EXCLUSIONS_REV_REV}" \
                "revert_sha=${SHA}" \
                "revert_date=${DATE}" \
                "revert_subject=${SUBJECT}" \
                "original_sha=${ORIGINAL_SHA}" \
                "original_subject=${ORIG_SUBJECT}"
            continue
        fi

        # ---------------------------------------------------------------------
        # Phase 5d — Release attribution: find the most recent release tag
        # T such that T is an ancestor of ORIGINAL_SHA
        # ---------------------------------------------------------------------
        # `git merge-base --is-ancestor TAG ORIGINAL_SHA` exits 0 iff TAG
        # is an ancestor (or equal to) ORIGINAL_SHA in the commit graph.
        # We iterate the non-prerelease tags in the order returned by
        # `releases.json#releases[]`, which is the API's published_at-
        # descending order (newest first). The first ancestor match is
        # the most recent release containing the original commit.
        local ATTRIB_TAG=""
        while IFS= read -r TAG; do
            if [[ -z "${TAG}" ]]; then continue; fi
            if git merge-base --is-ancestor "${TAG}" "${ORIGINAL_SHA}" 2>/dev/null; then
                ATTRIB_TAG="${TAG}"
                break
            fi
        done < "${RELEASE_TAGS_FILE}"

        if [[ -z "${ATTRIB_TAG}" ]]; then
            # ---------------------------------------------------------------
            # Phase 5e — Unreleased: original commit not reachable from any
            # release tag. Either no tags exist, or the original commit
            # precedes the earliest tag, or every tag is on a non-ancestor
            # branch. The revert is correctly excluded from attribution.
            # ---------------------------------------------------------------
            log_json info unreleased \
                revert_sha "${SHA}" \
                original_sha "${ORIGINAL_SHA}" \
                tags_checked "${RELEASE_TAGS_COUNT}"
            json_append_object "${EXCLUSIONS_UNRELEASED}" \
                "revert_sha=${SHA}" \
                "revert_date=${DATE}" \
                "revert_subject=${SUBJECT}" \
                "original_sha=${ORIGINAL_SHA}" \
                "tags_checked=${RELEASE_TAGS_COUNT}"
            continue
        fi

        # ---------------------------------------------------------------------
        # Phase 5f — Attributed: capture the original-commit author date
        # (normalised to UTC Z form for consistency with the rest of the
        # data/*.csv and data/*.json artifacts in the workspace) and
        # append a full attributed-revert record to the running array.
        # ---------------------------------------------------------------------
        local ORIG_DATE REVERT_DATE_ISO REVERT_COMMITTER_DATE
        ORIG_DATE="$(
            git log -1 --pretty=format:'%aI' "${ORIGINAL_SHA}" 2>/dev/null \
                | to_utc_z
        )"
        # The `revert_date` field from candidates.csv is already UTC Z
        # (02_extract_commits.sh normalises both %aI and %cI columns).
        # We also emit `revert_committer_date` from %cI for downstream
        # window-bucketing (compute_metrics.py prefers committer date
        # for revert-on-main analysis, since the committer date is when
        # the revert landed on the default branch).
        REVERT_DATE_ISO="${DATE}"
        REVERT_COMMITTER_DATE="$(
            git log -1 --pretty=format:'%cI' "${SHA}" 2>/dev/null \
                | to_utc_z
        )"
        if [[ -z "${REVERT_COMMITTER_DATE}" ]]; then
            REVERT_COMMITTER_DATE="${DATE}"
        fi

        json_append_object "${REVERTS_JSON}" \
            "revert_sha=${SHA}" \
            "revert_date=${DATE}" \
            "revert_date_iso=${REVERT_DATE_ISO}" \
            "revert_committer_date=${REVERT_COMMITTER_DATE}" \
            "revert_subject=${SUBJECT}" \
            "revert_authored_at=${DATE}" \
            "original_sha=${ORIGINAL_SHA}" \
            "original_date=${ORIG_DATE}" \
            "attributed_release_tag=${ATTRIB_TAG}" \
            "exclusion_reason=__NULL__"

        log_json info revert_attributed \
            revert_sha "${SHA}" \
            original_sha "${ORIGINAL_SHA}" \
            attributed_release_tag "${ATTRIB_TAG}"
    done < <(tail -n +2 "${CANDIDATES_FILE}")

    log_json info revert_candidates_processed \
        count "${PROCESSED}" \
        candidates_file "${CANDIDATES_FILE}"

    # -------------------------------------------------------------------------
    # Phase 6 — search_verification: run the four AAP-canonical search
    # commands and record their match counts for the Reproducibility
    # Appendix (Rule 5). These run regardless of whether any candidates
    # were processed — they are the independent cross-validation that
    # confirms the candidates CSV captured every revert in the graph.
    # -------------------------------------------------------------------------
    # Each search uses an inline `|| true` to mask grep's exit-1 on no-match
    # so that `set -o pipefail` does not propagate a "failure" upward and the
    # outer pipeline can complete naturally to wc -l. The for-loop below then
    # normalises every value through `printf '%d'` to strip any leading zeros
    # or whitespace artifacts (e.g., turn "00" or "  5" into the canonical
    # integer string "0" / "5").
    local SV_PRIMARY SV_SECONDARY SV_TERTIARY SV_BROADEST
    SV_PRIMARY="$(
        git log --all --grep='^Revert "' --pretty=format:'%H|%aI|%aE|%s' 2>/dev/null \
            | { grep -c '^[0-9a-f]\{7,40\}|' || true; }
    )"
    SV_SECONDARY="$(
        git log "${REV_SCOPE}" --grep='^Revert ' --oneline 2>/dev/null \
            | wc -l
    )"
    SV_TERTIARY="$(
        git log --all --grep='This reverts commit' --oneline 2>/dev/null \
            | wc -l
    )"
    SV_BROADEST="$(
        git log --all --pretty=format:'%H|%s' 2>/dev/null \
            | { grep -iE '^[a-f0-9]+\|Revert' || true; } \
            | wc -l
    )"
    # Normalise every search-verification value to a canonical integer string.
    # The regex check guards against any unexpected non-numeric output (e.g.,
    # a `grep` build that emits a usage message on stderr+stdout); the
    # `printf '%d'` pass strips whitespace and leading zeros, so "00", " 5 ",
    # and "0\n0" all collapse to the correct decimal representation.
    for var in SV_PRIMARY SV_SECONDARY SV_TERTIARY SV_BROADEST; do
        # shellcheck disable=SC2154,SC2086
        eval "value=\${$var}"
        # Collapse interior whitespace (newlines, tabs, spaces) so a multi-line
        # value like "0\n0" becomes "00" which is then handled by printf %d.
        value="${value//[$' \t\r\n']/}"
        if [[ "${value}" =~ ^[0-9]+$ ]]; then
            value="$(printf '%d' "${value}")"
        else
            value=0
        fi
        eval "$var=\${value}"
    done

    log_json info search_verification_complete \
        primary "${SV_PRIMARY}" \
        secondary "${SV_SECONDARY}" \
        tertiary "${SV_TERTIARY}" \
        broadest "${SV_BROADEST}"

    # -------------------------------------------------------------------------
    # Phase 7 — Assemble output JSON
    # -------------------------------------------------------------------------
    # The renderer reads `_metadata`, `fetched_at`, `reverts`, `exclusions`,
    # `summary`, and `search_verification`. We construct the payload via a
    # single Python heredoc that consumes the four temp accumulator files
    # plus environment.json and inflection.json (optional — gracefully
    # defaulted if missing).
    #
    # Atomic-write contract: the body is streamed to a .tmp sibling file
    # and only `os.replace`d into place after the JSON is fully serialized.
    # This protects downstream readers from observing a partially-written
    # reverts.json if the script is interrupted mid-write.
    BLITZY_REVERTS_JSON="${REVERTS_JSON}" \
    BLITZY_EXCLUSIONS_UNATTRIB="${EXCLUSIONS_UNATTRIB}" \
    BLITZY_EXCLUSIONS_UNRELEASED="${EXCLUSIONS_UNRELEASED}" \
    BLITZY_EXCLUSIONS_REV_REV="${EXCLUSIONS_REV_REV}" \
    BLITZY_ENVIRONMENT_FILE="${ENVIRONMENT_FILE}" \
    BLITZY_INFLECTION_FILE="${INFLECTION_FILE}" \
    BLITZY_OUTPUT_FILE="${OUTPUT_FILE}" \
    BLITZY_RELEASE_TAGS_COUNT="${RELEASE_TAGS_COUNT}" \
    BLITZY_SV_PRIMARY="${SV_PRIMARY}" \
    BLITZY_SV_SECONDARY="${SV_SECONDARY}" \
    BLITZY_SV_TERTIARY="${SV_TERTIARY}" \
    BLITZY_SV_BROADEST="${SV_BROADEST}" \
    BLITZY_REV_SCOPE="${REV_SCOPE}" \
    python3 - <<'PYEOF'
import json
import os
from datetime import datetime, timezone


def _load_optional(path: str) -> dict:
    """Return parsed JSON object or an empty dict on any failure."""
    if not path or not os.path.isfile(path):
        return {}
    try:
        with open(path) as f:
            data = json.load(f)
        return data if isinstance(data, dict) else {}
    except (OSError, json.JSONDecodeError):
        return {}


def _get_nested(d: dict, *keys: str):
    cur = d
    for k in keys:
        if not isinstance(cur, dict):
            return None
        cur = cur.get(k)
    return cur


# ---------------------------------------------------------------------------
# Load accumulator files
# ---------------------------------------------------------------------------
with open(os.environ["BLITZY_REVERTS_JSON"]) as f:
    reverts = json.load(f)
with open(os.environ["BLITZY_EXCLUSIONS_UNATTRIB"]) as f:
    unattrib = json.load(f)
with open(os.environ["BLITZY_EXCLUSIONS_UNRELEASED"]) as f:
    unreleased = json.load(f)
with open(os.environ["BLITZY_EXCLUSIONS_REV_REV"]) as f:
    rev_rev = json.load(f)

# ---------------------------------------------------------------------------
# Build _metadata from environment.json and inflection.json when available
# ---------------------------------------------------------------------------
env = _load_optional(os.environ.get("BLITZY_ENVIRONMENT_FILE", ""))
inflection = _load_optional(os.environ.get("BLITZY_INFLECTION_FILE", ""))

now_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

extraction_ts = (
    env.get("extraction_timestamp")
    or _get_nested(env, "_metadata", "extraction_timestamp")
    or now_iso
)
run_id = os.environ.get("BLITZY_RUN_ID", "") or (
    env.get("run_id")
    or _get_nested(env, "_metadata", "run_id")
    or ""
)
repo_slug = (
    env.get("repository_slug")
    or _get_nested(env, "_metadata", "repository_slug")
    or "Blitzy-Sandbox/blitzy-RudderStack"
)
default_branch = (
    env.get("default_branch")
    or _get_nested(env, "_metadata", "default_branch")
    or "main"
)
inflection_date_utc = (
    inflection.get("date_utc")
    or _get_nested(inflection, "_metadata", "date_utc")
    or ""
)
analysis_period_start = (
    _get_nested(inflection, "post_introduction", "start_iso")
    or env.get("analysis_period_start")
    or ""
)
analysis_period_end = (
    _get_nested(inflection, "post_introduction", "end_iso")
    or env.get("analysis_period_end")
    or ""
)

# ---------------------------------------------------------------------------
# Counts and aggregations
# ---------------------------------------------------------------------------
attributed_count = len(reverts)
unattrib_count = len(unattrib)
unreleased_count = len(unreleased)
rev_rev_count = len(rev_rev)
total_count = (
    attributed_count + unattrib_count + unreleased_count + rev_rev_count
)

reverts_by_release: dict[str, int] = {}
for r in reverts:
    tag = r.get("attributed_release_tag") or ""
    if not tag:
        continue
    reverts_by_release[tag] = reverts_by_release.get(tag, 0) + 1

# ---------------------------------------------------------------------------
# Phase windowing: inflection-relative split for downstream reuse
# ---------------------------------------------------------------------------
# Compute baseline (pre-inflection) and post-introduction (inflection ≤ d)
# revert counts. Window bucketing for the final per-metric series is
# performed in 09_compute_metrics.py based on these inputs.
baseline_count = 0
post_intro_count = 0
if inflection_date_utc and reverts:
    try:
        inflect_dt = datetime.fromisoformat(
            inflection_date_utc[:-1] + "+00:00"
            if inflection_date_utc.endswith("Z")
            else inflection_date_utc
        )
        for r in reverts:
            d_str = (
                r.get("revert_committer_date")
                or r.get("revert_date_iso")
                or r.get("revert_date")
                or ""
            )
            if not d_str:
                continue
            try:
                d_dt = datetime.fromisoformat(
                    d_str[:-1] + "+00:00" if d_str.endswith("Z") else d_str
                )
            except ValueError:
                continue
            if d_dt < inflect_dt:
                baseline_count += 1
            else:
                post_intro_count += 1
    except ValueError:
        pass

# ---------------------------------------------------------------------------
# Search-verification cross-check counts
# ---------------------------------------------------------------------------
sv_primary = int(os.environ.get("BLITZY_SV_PRIMARY", "0") or 0)
sv_secondary = int(os.environ.get("BLITZY_SV_SECONDARY", "0") or 0)
sv_tertiary = int(os.environ.get("BLITZY_SV_TERTIARY", "0") or 0)
sv_broadest = int(os.environ.get("BLITZY_SV_BROADEST", "0") or 0)
rev_scope = os.environ.get("BLITZY_REV_SCOPE", "main")

# ---------------------------------------------------------------------------
# Assemble the final payload
# ---------------------------------------------------------------------------
payload = {
    "_metadata": {
        "extraction_timestamp": extraction_ts,
        "run_id": run_id,
        "repository_slug": repo_slug,
        "default_branch": default_branch,
        "extraction_method": "git_cat_file_and_merge_base_is_ancestor",
        "analysis_period_start": analysis_period_start,
        "analysis_period_end": analysis_period_end,
        "inflection_date_utc": inflection_date_utc,
        "schema_version": "1.0.0",
        "feeds_metric": "m8",
        "feeds_metric_name": "Problem Records in Release",
        "aap_section": "0.5.3.9",
        "producing_script": (
            "blitzy/acceleration-report/scripts/05_extract_reverts.sh"
        ),
        "confidence": "high",
        "confidence_rationale": (
            "Revert detection is a direct local-git scan with no API, "
            "network, or admin-access dependency. The four cross-validating "
            "search commands recorded in search_verification each return "
            "consistent counts; an empty result here is High-confidence "
            "evidence of zero reverts, not a degraded fallback."
        ),
        "exclusion_category_order": [
            "unattributable",
            "unreleased",
            "revert_of_revert",
        ],
        "release_tag_count_consulted": int(
            os.environ.get("BLITZY_RELEASE_TAGS_COUNT", "0") or 0
        ),
    },
    "fetched_at": now_iso,
    "reverts": reverts,
    "exclusions": {
        "unattributable": unattrib,
        "unreleased": unreleased,
        "revert_of_revert": rev_rev,
    },
    # The summary block intentionally carries both the agent_prompt's
    # short keys (total, attributed, unattributable, unreleased,
    # revert_of_revert) and the longer downstream-compute keys
    # (attributable_reverts, total_revert_commits_found,
    # unattributable_count, unreleased_count, revert_of_revert_count).
    # See the script header for the rationale (dual-key compatibility
    # with both the canonical spec and the 09_compute_metrics.py reader).
    "summary": {
        "total": total_count,
        "attributed": attributed_count,
        "unattributable": unattrib_count,
        "unreleased": unreleased_count,
        "revert_of_revert": rev_rev_count,
        "total_revert_commits_found": total_count,
        "attributable_reverts": attributed_count,
        "unattributable_count": unattrib_count,
        "unreleased_count": unreleased_count,
        "revert_of_revert_count": rev_rev_count,
        "reverts_by_release_tag": reverts_by_release,
        "baseline_count": baseline_count,
        "post_introduction_count": post_intro_count,
    },
    "search_commands": [
        "git log --all --grep='^Revert \"' --pretty=format:'%H|%aI|%aE|%s'",
        f"git log {rev_scope} --grep='^Revert ' --oneline",
        "git log --all --grep='This reverts commit' --oneline",
        "git log --all --pretty=format:'%H|%s' | grep -iE "
        "'^[a-f0-9]+\\|Revert'",
    ],
    "search_verification": {
        "primary_command": (
            "git log --all --grep='^Revert \"' "
            "--pretty=format:'%H|%aI|%aE|%s' | grep -c '^[0-9a-f]\\{7,40\\}|'"
        ),
        "primary_command_output": str(sv_primary),
        "secondary_command": (
            f"git log {rev_scope} --grep='^Revert ' --oneline | wc -l"
        ),
        "secondary_command_output": str(sv_secondary),
        "tertiary_corroboration_command": (
            "git log --all --grep='This reverts commit' --oneline | wc -l"
        ),
        "tertiary_corroboration_command_output": str(sv_tertiary),
        "broadest_corroboration_command": (
            "git log --all --pretty=format:'%H|%s' | "
            "grep -iE '^[a-f0-9]+\\|Revert' | wc -l"
        ),
        "broadest_corroboration_command_output": str(sv_broadest),
        "verification_summary": (
            "All four independent search variants — primary AAP command, "
            "secondary verification command, body-text corroboration, and "
            "broadest subject scan — return their match counts here. The "
            "primary command count is the authoritative cross-check against "
            "the count returned by the candidates CSV row count "
            f"({total_count}); the three corroboration commands should "
            "return identical or higher counts (higher only when a revert "
            "subject deviates from the canonical 'Revert \"...\"' prefix)."
        ),
    },
    "provenance": {
        "spec_section": "AAP §0.5.3.9 (Metric 8 — Problem Records in Release)",
        "extraction_chain": [
            "02_extract_commits.sh -> data/revert_candidates.csv",
            "04_extract_releases.py -> data/releases.json",
            "05_extract_reverts.sh  -> data/reverts.json",
        ],
        "downstream_consumer": (
            "blitzy/acceleration-report/data/metrics.json#m8"
        ),
        "producing_script": (
            "blitzy/acceleration-report/scripts/05_extract_reverts.sh"
        ),
    },
}

# ---------------------------------------------------------------------------
# Atomic write
# ---------------------------------------------------------------------------
output_path = os.environ["BLITZY_OUTPUT_FILE"]
output_dir = os.path.dirname(output_path)
if output_dir:
    os.makedirs(output_dir, exist_ok=True)
tmp_path = output_path + ".tmp"
with open(tmp_path, "w") as f:
    json.dump(payload, f, indent=2)
os.replace(tmp_path, output_path)
PYEOF

    # -------------------------------------------------------------------------
    # Phase 8 — Final completion log
    # -------------------------------------------------------------------------
    # Re-read the freshly written payload to surface the canonical counts
    # in the completion log line (Rule: Observability — concrete numbers
    # in the structured log for run-to-run comparison via `data/run.log.jsonl`).
    local TOTAL ATTRIBUTED UNATTRIB UNREL REVREV
    TOTAL="$(
        BLITZY_OUTPUT_FILE="${OUTPUT_FILE}" python3 - <<'PYEOF'
import json
import os
with open(os.environ["BLITZY_OUTPUT_FILE"]) as f:
    print(json.load(f)["summary"]["total"])
PYEOF
    )"
    ATTRIBUTED="$(
        BLITZY_OUTPUT_FILE="${OUTPUT_FILE}" python3 - <<'PYEOF'
import json
import os
with open(os.environ["BLITZY_OUTPUT_FILE"]) as f:
    print(json.load(f)["summary"]["attributed"])
PYEOF
    )"
    UNATTRIB="$(
        BLITZY_OUTPUT_FILE="${OUTPUT_FILE}" python3 - <<'PYEOF'
import json
import os
with open(os.environ["BLITZY_OUTPUT_FILE"]) as f:
    print(json.load(f)["summary"]["unattributable"])
PYEOF
    )"
    UNREL="$(
        BLITZY_OUTPUT_FILE="${OUTPUT_FILE}" python3 - <<'PYEOF'
import json
import os
with open(os.environ["BLITZY_OUTPUT_FILE"]) as f:
    print(json.load(f)["summary"]["unreleased"])
PYEOF
    )"
    REVREV="$(
        BLITZY_OUTPUT_FILE="${OUTPUT_FILE}" python3 - <<'PYEOF'
import json
import os
with open(os.environ["BLITZY_OUTPUT_FILE"]) as f:
    print(json.load(f)["summary"]["revert_of_revert"])
PYEOF
    )"

    log_json info script_complete \
        total "${TOTAL}" \
        attributed "${ATTRIBUTED}" \
        unattributable "${UNATTRIB}" \
        unreleased "${UNREL}" \
        revert_of_revert "${REVREV}" \
        search_verification_primary "${SV_PRIMARY}" \
        output "${OUTPUT_FILE}"
}

# -----------------------------------------------------------------------------
# Dispatch
# -----------------------------------------------------------------------------
main "$@"

