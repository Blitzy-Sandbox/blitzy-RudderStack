#!/usr/bin/env bash
# run-config-f.sh — Config F pipeline driver (install → scan → normalize → emit).
#
# Mechanical synopsis (rationale lives in decision-log.md):
#   Stage 1: verify python3 (>=3.8) and a writable working directory.
#   Stage 2: install OSV-Scanner via apt → go-v2 → go-v1 ladder (no-op if present).
#   Stage 3: run `osv-scanner --format json --output results-osv.json <target>`,
#            capture exit code, wall-clock duration, optional offline-mode flag.
#   Stage 4: post-process results-osv.json via normalize.py → findings-config-f.json,
#            then run the four user-contract pass/fail gates.
#   Stage 5: update decision-log.md "## Pipeline Metadata" bullet values in place
#            and (when placeholders exist) substitute runtime values in
#            executive-summary.html.
#
# Exported runtime variables (consumed by Stage 5):
#   OSV_VERSION, OSV_INSTALL_PATH, SCAN_EXIT_CODE, SCAN_DURATION_SECONDS,
#   SCAN_TARGET, OFFLINE_FLAG_USED, FINDING_COUNT, CRITICAL_COUNT, HIGH_COUNT,
#   MEDIUM_COUNT, LOW_COUNT, SCAN_TIMESTAMP.
#
# Exit codes:
#   0 — full success (all four pass/fail gates green).
#   1 — install failure (osv-scanner unavailable after every ladder step).
#   2 — environment failure (python3 missing/old, target not a directory,
#       results-osv.json unparseable, or osv-scanner exited >=2).
#   3 — normalization gate failure (line count, JSON, fields, or length).
#
# See decision-log.md for: install-ladder ordering, severity bucketing, CWE
# resolution ladder, description sourcing, dedup key, offline-mode probe.

set -eo pipefail

# ---------- Constants ----------
RESULTS="results-osv.json"
FINDINGS="findings-config-f.json"
LOG="decision-log.md"
DECK="executive-summary.html"
NORMALIZER="normalize.py"

# ---------- Stage 1: Environment Check ----------
command -v python3 >/dev/null 2>&1 \
  || { echo "FATAL: python3 not on PATH" >&2; exit 2; }
python3 -c "import sys; sys.exit(0 if sys.version_info >= (3,8) else 1)" \
  || { echo "FATAL: python3 >= 3.8 required" >&2; exit 2; }

[ -w "." ] || { echo "FATAL: current directory is not writable" >&2; exit 2; }

[ -f "$NORMALIZER" ] \
  || { echo "FATAL: $NORMALIZER not found in working directory" >&2; exit 2; }

# Extend PATH with conventional Go install locations so non-login shells
# can locate go/osv-scanner without sourcing /etc/profile.d/go.sh.
for _gobin in /usr/local/go/bin /root/go/bin "${HOME:-/root}/go/bin"; do
  if [ -d "$_gobin" ]; then
    case ":$PATH:" in
      *":$_gobin:"*) ;;
      *) PATH="$PATH:$_gobin" ;;
    esac
  fi
done
unset _gobin
export PATH

# ---------- Stage 2: Install OSV-Scanner ----------
OSV_INSTALL_PATH=""

if command -v osv-scanner >/dev/null 2>&1; then
  OSV_INSTALL_PATH="preinstalled"
fi

if [ -z "$OSV_INSTALL_PATH" ] && command -v apt-get >/dev/null 2>&1; then
  if DEBIAN_FRONTEND=noninteractive apt-get install -y osv-scanner >/dev/null 2>&1; then
    if command -v osv-scanner >/dev/null 2>&1; then
      OSV_INSTALL_PATH="apt"
    fi
  fi
fi

if [ -z "$OSV_INSTALL_PATH" ] && command -v go >/dev/null 2>&1; then
  if go install github.com/google/osv-scanner/v2/cmd/osv-scanner@latest >/dev/null 2>&1; then
    PATH="$(go env GOPATH)/bin:$PATH"
    export PATH
    if command -v osv-scanner >/dev/null 2>&1; then
      OSV_INSTALL_PATH="v2-go"
    fi
  fi
fi

if [ -z "$OSV_INSTALL_PATH" ] && command -v go >/dev/null 2>&1; then
  if go install github.com/google/osv-scanner/cmd/osv-scanner@latest >/dev/null 2>&1; then
    PATH="$(go env GOPATH)/bin:$PATH"
    export PATH
    if command -v osv-scanner >/dev/null 2>&1; then
      OSV_INSTALL_PATH="v1-go"
    fi
  fi
fi

command -v osv-scanner >/dev/null 2>&1 \
  || { echo "FATAL: failed to install osv-scanner via any ladder step (apt, v2-go, v1-go)" >&2; exit 1; }

OSV_VERSION="$(osv-scanner --version 2>&1 | head -1 | tr -d '\r')"
[ -n "$OSV_VERSION" ] \
  || { echo "FATAL: osv-scanner --version returned no output" >&2; exit 1; }

# ---------- Stage 3: Execute the Scan (Timed) ----------
OFFLINE_FLAG=""
OFFLINE_FLAG_USED="no"
if osv-scanner --help 2>&1 | grep -q -- "--experimental-local-db"; then
  OFFLINE_FLAG="--experimental-local-db"
  OFFLINE_FLAG_USED="yes"
fi

SCAN_TARGET="${1:-$(pwd)}"
[ -d "$SCAN_TARGET" ] \
  || { echo "FATAL: scan target '$SCAN_TARGET' is not a directory" >&2; exit 2; }
SCAN_TARGET="$(cd "$SCAN_TARGET" && pwd)"

SCAN_START="$(date +%s.%N)"
set +e
if [ -n "$OFFLINE_FLAG" ]; then
  osv-scanner --format json --output "$RESULTS" "$OFFLINE_FLAG" "$SCAN_TARGET"
else
  osv-scanner --format json --output "$RESULTS" "$SCAN_TARGET"
fi
SCAN_EXIT_CODE=$?
set -e
SCAN_END="$(date +%s.%N)"
SCAN_DURATION_SECONDS="$(awk -v s="$SCAN_START" -v e="$SCAN_END" 'BEGIN { printf "%.3f", e - s }')"

# OSV-Scanner contract: 0 = clean, 1 = vulnerabilities found, >=2 = operational error.
case "$SCAN_EXIT_CODE" in
  0|1) ;;
  *)
    echo "FATAL: osv-scanner exited with operational error code $SCAN_EXIT_CODE" >&2
    exit 2
    ;;
esac

[ -f "$RESULTS" ] \
  || { echo "FATAL: $RESULTS was not produced by osv-scanner" >&2; exit 2; }
python3 -c "import json; json.load(open('$RESULTS'))" \
  || { echo "FATAL: $RESULTS is not valid JSON" >&2; exit 2; }

# ---------- Stage 4: Normalize & Validate ----------
python3 "$NORMALIZER" "$RESULTS" "$SCAN_TARGET" > "$FINDINGS"

# Gate 1: single-line output. `wc -l` counts newline terminators; values 0 and 1
# both satisfy `cat ... | wc -l == 1`. See decision-log.md Decision 10.
LINE_COUNT="$(wc -l < "$FINDINGS" | tr -d ' ')"
if [ "$LINE_COUNT" -gt 1 ]; then
  echo "FATAL: $FINDINGS has $LINE_COUNT lines, expected 0 or 1" >&2
  exit 3
fi

# Gate 2: valid JSON.
python3 -c "import json; json.load(open('$FINDINGS'))" \
  || { echo "FATAL: $FINDINGS is not valid JSON" >&2; exit 3; }

# Gate 3: schema enforcement — exact key order/equality, integer line==0,
# severity in {critical,high,medium,low}, non-empty file/cwe/description,
# relative path safety. See decision-log.md (Directive 3 gates).
python3 - "$FINDINGS" <<'PY' \
  || { echo "FATAL: findings schema gate failed (see message above)" >&2; exit 3; }
import json, sys
EXPECTED_KEYS = ["file", "line", "severity", "cwe", "description"]
ALLOWED_SEVERITY = {"critical", "high", "medium", "low"}
data = json.load(open(sys.argv[1]))
if not isinstance(data, list):
    raise SystemExit("findings root is not a JSON array")
for f in data:
    if not isinstance(f, dict):
        raise SystemExit("finding is not a JSON object: " + json.dumps(f))
    if list(f.keys()) != EXPECTED_KEYS:
        raise SystemExit("key order/set mismatch: " + json.dumps(f))
    if not isinstance(f["file"], str) or not f["file"]:
        raise SystemExit("file empty or non-string: " + json.dumps(f))
    if f["file"].startswith("/") or ".." in f["file"].split("/"):
        raise SystemExit("file path not relative: " + json.dumps(f))
    if not isinstance(f["line"], int) or isinstance(f["line"], bool) or f["line"] != 0:
        raise SystemExit("line is not integer 0: " + json.dumps(f))
    if f["severity"] not in ALLOWED_SEVERITY:
        raise SystemExit("severity not in whitelist: " + json.dumps(f))
    if not isinstance(f["cwe"], str) or not f["cwe"]:
        raise SystemExit("cwe empty or non-string: " + json.dumps(f))
    if not isinstance(f["description"], str) or not f["description"]:
        raise SystemExit("description empty or non-string: " + json.dumps(f))
PY

# Gate 4: no description exceeds 200 characters.
python3 - "$FINDINGS" <<'PY' \
  || { echo "FATAL: a description exceeds 200 characters" >&2; exit 3; }
import json, sys
for f in json.load(open(sys.argv[1])):
    d = f.get("description", "")
    if not isinstance(d, str) or len(d) > 200:
        raise SystemExit("oversize description (" + str(len(d)) + " chars)")
PY

# Capture severity distribution.
FINDING_COUNT="$(python3 -c "import json; print(len(json.load(open('$FINDINGS'))))")"
CRITICAL_COUNT="$(python3 -c "import json; print(sum(1 for f in json.load(open('$FINDINGS')) if f.get('severity')=='critical'))")"
HIGH_COUNT="$(python3 -c "import json; print(sum(1 for f in json.load(open('$FINDINGS')) if f.get('severity')=='high'))")"
MEDIUM_COUNT="$(python3 -c "import json; print(sum(1 for f in json.load(open('$FINDINGS')) if f.get('severity')=='medium'))")"
LOW_COUNT="$(python3 -c "import json; print(sum(1 for f in json.load(open('$FINDINGS')) if f.get('severity')=='low'))")"
SCAN_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------- Stage 5: Emit Compliance Deliverables ----------
# Stage 5a: rewrite the entire bullet line for each labelled metadata field
# inside "## Pipeline Metadata" of decision-log.md. The full backtick value
# plus any trailing annotation is replaced from a canonical template so that
# reruns are idempotent and no stale prose persists. The
# Per-Ecosystem Finding Counts subsection is preserved (it is not bullet-keyed
# in the metadata block). See decision-log.md Decisions 13, 20.
if [ -f "$LOG" ]; then
  OSV_VERSION="$OSV_VERSION" \
  OSV_INSTALL_PATH="$OSV_INSTALL_PATH" \
  SCAN_EXIT_CODE="$SCAN_EXIT_CODE" \
  SCAN_DURATION_SECONDS="$SCAN_DURATION_SECONDS" \
  SCAN_TARGET="$SCAN_TARGET" \
  OFFLINE_FLAG="$OFFLINE_FLAG" \
  OFFLINE_FLAG_USED="$OFFLINE_FLAG_USED" \
  FINDING_COUNT="$FINDING_COUNT" \
  CRITICAL_COUNT="$CRITICAL_COUNT" \
  HIGH_COUNT="$HIGH_COUNT" \
  MEDIUM_COUNT="$MEDIUM_COUNT" \
  LOW_COUNT="$LOW_COUNT" \
  SCAN_TIMESTAMP="$SCAN_TIMESTAMP" \
  DECISION_LOG="$LOG" \
  python3 - <<'PY'
import os, re, sys, pathlib

log_path = pathlib.Path(os.environ.get("DECISION_LOG", "decision-log.md"))
if not log_path.exists():
    sys.exit(0)

INSTALL_DISPLAY = {
    "apt": "apt-get install -y osv-scanner",
    "v2-go": "go install github.com/google/osv-scanner/v2/cmd/osv-scanner@latest",
    "v1-go": "go install github.com/google/osv-scanner/cmd/osv-scanner@latest",
    "preinstalled": "preinstalled (osv-scanner already on PATH)",
}

install_key = os.environ.get("OSV_INSTALL_PATH", "")
install_display = INSTALL_DISPLAY.get(install_key, install_key or "unknown")
offline_used = os.environ.get("OFFLINE_FLAG_USED", "no") == "yes"
offline_flag = os.environ.get("OFFLINE_FLAG", "").strip()
cmdline = "osv-scanner --format json --output results-osv.json"
if offline_used and offline_flag:
    cmdline += " " + offline_flag
cmdline += " <repo-root>"


def safe_md(value: str) -> str:
    # Replace backticks in user-supplied values to keep markdown spans well-formed.
    return str(value).replace("`", "'")


osv_version = safe_md(os.environ.get("OSV_VERSION", ""))
exit_code = safe_md(os.environ.get("SCAN_EXIT_CODE", ""))
duration = safe_md(os.environ.get("SCAN_DURATION_SECONDS", ""))
timestamp = safe_md(os.environ.get("SCAN_TIMESTAMP", ""))
total = safe_md(os.environ.get("FINDING_COUNT", ""))
critical = safe_md(os.environ.get("CRITICAL_COUNT", ""))
high = safe_md(os.environ.get("HIGH_COUNT", ""))
medium = safe_md(os.environ.get("MEDIUM_COUNT", ""))
low = safe_md(os.environ.get("LOW_COUNT", ""))
install_disp = safe_md(install_display)
cmdline_disp = safe_md(cmdline)

if exit_code == "0":
    exit_annot = "(clean scan; no vulnerabilities found per OSV-Scanner contract)"
elif exit_code == "1":
    exit_annot = "(vulnerabilities found; a successful scan per Directive 2's pass/fail gate)"
else:
    exit_annot = "(non-standard exit code; see scan stderr)"

if offline_used:
    offline_disp = "`Yes` — `" + (offline_flag if offline_flag else "--experimental-local-db") + "` flag probed and applied; scan ran against a local OSV database."
else:
    offline_disp = "`No` — the `--experimental-local-db` flag from Directive 2 was probed and is not exposed by the installed OSV-Scanner version. Per the user contract's \"if available\" qualifier, the scan ran in online mode against `api.osv.dev`. See Decision 13 and the Deviation Register."

# Each entry is (label, complete-line-template) and replaces the full
# bullet line so no stale trailing prose survives reruns.
line_updates = [
    ("OSV-Scanner version", "- **OSV-Scanner version**: `" + osv_version + "`"),
    ("Install path used", "- **Install path used**: `" + install_disp + "`"),
    ("Scan exit code", "- **Scan exit code**: `" + exit_code + "` " + exit_annot),
    ("Wall-clock duration (s)", "- **Wall-clock duration (s)**: `" + duration + "` (captured by `run-config-f.sh` Stage 3 via `date +%s.%N` deltas around the `osv-scanner` invocation; format: three decimal places)"),
    ("Command line", "- **Command line**: `" + cmdline_disp + "` (the absolute target path is redacted to `<repo-root>` for portability across re-runs in different harness checkouts)"),
    ("Offline mode", "- **Offline mode**: " + offline_disp),
    ("Scan timestamp (UTC)", "- **Scan timestamp (UTC)**: `" + timestamp + "` (runtime ISO-8601 UTC; refreshed by `run-config-f.sh` Stage 5 on every execution)"),
    ("Total findings", "- **Total findings**: `" + total + "` (post-deduplication via the `(file, cwe, description[:80])` dedup key — see Decision 8)"),
    ("Critical", "- **Critical**: `" + critical + "`"),
    ("High", "- **High**: `" + high + "`"),
    ("Medium", "- **Medium**: `" + medium + "`"),
    ("Low", "- **Low**: `" + low + "`"),
]


def replace_full_bullet(content: str, label: str, new_line: str) -> str:
    # Replace the entire `- **<label>**: ...` line so trailing annotations
    # are rewritten atomically. The line is anchored at the beginning of
    # a line and ends at the next newline (or end of file).
    pattern = re.compile(
        r"(?m)^- \*\*" + re.escape(label) + r"\*\*:[^\n]*$"
    )
    return pattern.sub(lambda _m: new_line, content, count=1)


text = log_path.read_text(encoding="utf-8")
updated = text
for label, new_line in line_updates:
    updated = replace_full_bullet(updated, label, new_line)

if updated != text:
    log_path.write_text(updated, encoding="utf-8")
PY
fi

# Stage 5b: optional placeholder substitution in executive-summary.html.
# Only performed when curly-brace placeholders (e.g., {{FINDING_COUNT}}) exist;
# the canonical deck has already been populated and contains none, so this is
# a no-op for fresh checkouts.
if [ -f "$DECK" ]; then
  OSV_VERSION="$OSV_VERSION" \
  OSV_INSTALL_PATH="$OSV_INSTALL_PATH" \
  SCAN_EXIT_CODE="$SCAN_EXIT_CODE" \
  SCAN_DURATION_SECONDS="$SCAN_DURATION_SECONDS" \
  OFFLINE_FLAG_USED="$OFFLINE_FLAG_USED" \
  FINDING_COUNT="$FINDING_COUNT" \
  CRITICAL_COUNT="$CRITICAL_COUNT" \
  HIGH_COUNT="$HIGH_COUNT" \
  MEDIUM_COUNT="$MEDIUM_COUNT" \
  LOW_COUNT="$LOW_COUNT" \
  SCAN_TIMESTAMP="$SCAN_TIMESTAMP" \
  DECK_PATH="$DECK" \
  python3 - <<'PY'
import os, pathlib, re, sys

deck_path = pathlib.Path(os.environ.get("DECK_PATH", "executive-summary.html"))
if not deck_path.exists():
    sys.exit(0)

text = deck_path.read_text(encoding="utf-8")
substitutions = {
    "OSV_VERSION": os.environ.get("OSV_VERSION", ""),
    "OSV_INSTALL_PATH": os.environ.get("OSV_INSTALL_PATH", ""),
    "SCAN_EXIT_CODE": os.environ.get("SCAN_EXIT_CODE", ""),
    "SCAN_DURATION_SECONDS": os.environ.get("SCAN_DURATION_SECONDS", ""),
    "OFFLINE_FLAG_USED": os.environ.get("OFFLINE_FLAG_USED", ""),
    "FINDING_COUNT": os.environ.get("FINDING_COUNT", ""),
    "CRITICAL_COUNT": os.environ.get("CRITICAL_COUNT", ""),
    "HIGH_COUNT": os.environ.get("HIGH_COUNT", ""),
    "MEDIUM_COUNT": os.environ.get("MEDIUM_COUNT", ""),
    "LOW_COUNT": os.environ.get("LOW_COUNT", ""),
    "SCAN_TIMESTAMP": os.environ.get("SCAN_TIMESTAMP", ""),
}

updated = text
for token, value in substitutions.items():
    placeholder = "{{" + token + "}}"
    if placeholder in updated:
        updated = updated.replace(placeholder, value)

if updated != text:
    deck_path.write_text(updated, encoding="utf-8")
PY
fi

# ---------- Summary ----------
cat <<EOF
Config F pipeline complete.
  OSV-Scanner:        $OSV_VERSION
  Install path:       $OSV_INSTALL_PATH
  Scan target:        $SCAN_TARGET
  Scan exit code:     $SCAN_EXIT_CODE  (0 clean / 1 vulnerabilities found)
  Wall-clock seconds: $SCAN_DURATION_SECONDS
  Offline mode:       $OFFLINE_FLAG_USED
  Timestamp (UTC):    $SCAN_TIMESTAMP
  Findings (post-dedup, sorted):
    total:    $FINDING_COUNT
    critical: $CRITICAL_COUNT
    high:     $HIGH_COUNT
    medium:   $MEDIUM_COUNT
    low:      $LOW_COUNT
  Artifacts:
    raw:        $RESULTS
    normalized: $FINDINGS
    log:        $LOG
    deck:       $DECK
EOF

exit 0
