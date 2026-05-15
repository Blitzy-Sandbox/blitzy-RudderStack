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

# Gate 1: single-line output. `wc -l` counts newline terminators; valid values
# are 0 (no trailing newline) or 1 (one trailing newline). Either passes the
# user's `cat ... | wc -l == 1` contract because cat normalizes to one line.
LINE_COUNT="$(wc -l < "$FINDINGS" | tr -d ' ')"
if [ "$LINE_COUNT" -gt 1 ]; then
  echo "FATAL: $FINDINGS has $LINE_COUNT lines, expected 0 or 1" >&2
  exit 3
fi

# Gate 2: valid JSON.
python3 -c "import json; json.load(open('$FINDINGS'))" \
  || { echo "FATAL: $FINDINGS is not valid JSON" >&2; exit 3; }

# Gate 3: every finding has all 5 required fields.
python3 - "$FINDINGS" <<'PY' \
  || { echo "FATAL: a finding is missing one or more required fields" >&2; exit 3; }
import json, sys
req = {"file", "line", "severity", "cwe", "description"}
data = json.load(open(sys.argv[1]))
for f in data:
    if not req <= set(f.keys()):
        raise SystemExit("missing field(s) in: " + json.dumps(f))
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
# Stage 5a: rewrite the bullet values inside "## Pipeline Metadata" of decision-log.md.
# Each bullet's first backtick-delimited value is replaced; trailing annotations
# and the Per-Ecosystem Finding Counts subsection are preserved. Implemented in
# Python via env-variable passing + quoted heredoc to avoid shell metacharacter
# pitfalls in scanner version strings and command lines.
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

updates = [
    ("OSV-Scanner version", os.environ.get("OSV_VERSION", "")),
    ("Install path used", install_display),
    ("Scan exit code", os.environ.get("SCAN_EXIT_CODE", "")),
    ("Wall-clock duration (s)", os.environ.get("SCAN_DURATION_SECONDS", "")),
    ("Command line", cmdline),
    ("Offline mode", "Yes" if offline_used else "No"),
    ("Scan timestamp (UTC)", os.environ.get("SCAN_TIMESTAMP", "")),
    ("Total findings", os.environ.get("FINDING_COUNT", "")),
    ("Critical", os.environ.get("CRITICAL_COUNT", "")),
    ("High", os.environ.get("HIGH_COUNT", "")),
    ("Medium", os.environ.get("MEDIUM_COUNT", "")),
    ("Low", os.environ.get("LOW_COUNT", "")),
]

text = log_path.read_text(encoding="utf-8")


def replace_first_bullet_value(content: str, label: str, value: str) -> str:
    """Update only the first backtick-delimited value of one labelled bullet.

    Matches a line shaped like ``- **<label>**: `<old>`...`` and rewrites the
    first ``\\`<old>\\``` token, leaving trailing annotation/parentheticals
    intact. The label is anchored to a leading "- **" so labels mentioned in
    surrounding prose are not touched. The replacement uses an inline lambda
    so backslashes/backticks in the value do not invoke regex backrefs.
    """
    pattern = re.compile(
        r"(?m)^(- \*\*" + re.escape(label) + r"\*\*:\s+)`[^`]*`"
    )

    def _sub(match: "re.Match[str]") -> str:
        # Replace embedded backticks in the new value to keep the markdown
        # span well-formed.
        safe = value.replace("`", "'")
        return match.group(1) + "`" + safe + "`"

    return pattern.sub(_sub, content, count=1)


updated = text
for label, value in updates:
    updated = replace_first_bullet_value(updated, label, str(value))

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
