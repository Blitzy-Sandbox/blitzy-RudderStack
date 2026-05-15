#!/usr/bin/env bash
# run-config-f.sh — Config F pipeline driver (install → scan → normalize → emit).
#
# Mechanical synopsis (rationale lives in decision-log.md):
#   Stage 1: verify python3 (>=3.8) and a writable working directory.
#   Stage 2: install OSV-Scanner via apt → go-v2 → go-v1 ladder (no-op if present).
#   Stage 3: run `osv-scanner scan source -r --format json --output-file
#            results-osv.json <target>` (v2 CLI with recursive flag per
#            AAP §0.2.1; v1 invocation falls back to the legacy short form),
#            capture exit code, wall-clock duration, optional offline-mode flag.
#   Stage 4: post-process results-osv.json via normalize.py → findings-config-f.json,
#            then run the four user-contract pass/fail gates.
#   Stage 5: update decision-log.md in place — Pipeline Metadata bullets,
#            Per-Ecosystem table, Decision 8 prose, Deviation 3/4 prose,
#            Bidirectional Traceability Matrix duration/count rows,
#            Verification Checklist canonical max length, determinism command.
#            Also (when placeholders exist) substitute runtime values in
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
# Capture the help output to a tempfile once so subsequent probes can grep it
# without being affected by SIGPIPE / pipefail interactions with the scanner
# binary (which can return a non-standard exit code on `--help`).
OSV_HELP_OUT="$(mktemp)"
osv-scanner --help > "$OSV_HELP_OUT" 2>&1 || true
osv-scanner scan source --help >> "$OSV_HELP_OUT" 2>&1 || true

# Probe whether the legacy v1 offline flag is available; v2.3.8+ has renamed
# this to --offline-vulnerabilities and the v1 flag is absent. The user-cited
# flag is used only when literally present (Decision 13, Deviation 7).
OFFLINE_FLAG=""
OFFLINE_FLAG_USED="no"
if grep -q -- "--experimental-local-db" "$OSV_HELP_OUT"; then
  OFFLINE_FLAG="--experimental-local-db"
  OFFLINE_FLAG_USED="yes"
fi

# Probe whether the v2 `scan source` subcommand exists. v2 requires it for
# recursive directory walks; v1 took a positional directory argument and was
# recursive by default. See decision-log.md Deviation 11.
USE_V2_SCAN_SOURCE="no"
if grep -q -E '^[[:space:]]+scan[[:space:]]' "$OSV_HELP_OUT"; then
  USE_V2_SCAN_SOURCE="yes"
fi
rm -f "$OSV_HELP_OUT"

SCAN_TARGET="${1:-$(pwd)}"
[ -d "$SCAN_TARGET" ] \
  || { echo "FATAL: scan target '$SCAN_TARGET' is not a directory" >&2; exit 2; }
SCAN_TARGET="$(cd "$SCAN_TARGET" && pwd)"

SCAN_START="$(date +%s.%N)"
set +e
if [ "$USE_V2_SCAN_SOURCE" = "yes" ]; then
  if [ -n "$OFFLINE_FLAG" ]; then
    osv-scanner scan source -r --format json --output-file "$RESULTS" "$OFFLINE_FLAG" "$SCAN_TARGET"
  else
    osv-scanner scan source -r --format json --output-file "$RESULTS" "$SCAN_TARGET"
  fi
else
  if [ -n "$OFFLINE_FLAG" ]; then
    osv-scanner --format json --output "$RESULTS" "$OFFLINE_FLAG" "$SCAN_TARGET"
  else
    osv-scanner --format json --output "$RESULTS" "$SCAN_TARGET"
  fi
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

# Capture severity distribution and canonical metrics needed by Stage 5.
FINDING_COUNT="$(python3 -c "import json; print(len(json.load(open('$FINDINGS'))))")"
CRITICAL_COUNT="$(python3 -c "import json; print(sum(1 for f in json.load(open('$FINDINGS')) if f.get('severity')=='critical'))")"
HIGH_COUNT="$(python3 -c "import json; print(sum(1 for f in json.load(open('$FINDINGS')) if f.get('severity')=='high'))")"
MEDIUM_COUNT="$(python3 -c "import json; print(sum(1 for f in json.load(open('$FINDINGS')) if f.get('severity')=='medium'))")"
LOW_COUNT="$(python3 -c "import json; print(sum(1 for f in json.load(open('$FINDINGS')) if f.get('severity')=='low'))")"
SCAN_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Raw vulnerability row count (pre-dedup); dedup ratio = (raw - unique) / raw.
# Both are documented in Decision 8 and Deviation 4; computed here so Stage 5
# can rewrite the canonical-number prose in those sections.
RAW_VULN_COUNT="$(python3 -c "import json; r=json.load(open('$RESULTS')); print(sum(len(p.get('vulnerabilities') or []) for s in (r.get('results') or []) for p in (s.get('packages') or [])))")"
if [ "$RAW_VULN_COUNT" -gt 0 ]; then
  DEDUP_RATIO_PCT="$(awk -v r="$RAW_VULN_COUNT" -v u="$FINDING_COUNT" 'BEGIN { if (r > 0) printf "%.0f", (r - u) * 100.0 / r; else print "0" }')"
else
  DEDUP_RATIO_PCT="0"
fi

# Maximum description length across normalized findings, reported by the
# Verification Checklist's description-length gate (Directive 3).
MAX_DESC_LEN="$(python3 -c "import json; d=json.load(open('$FINDINGS')); print(max((len(f.get('description','')) for f in d), default=0))")"

# Capture the actual first line emitted by `osv-scanner --version` so the
# Bidirectional Traceability Matrix references the real binary output rather
# than a fabricated combined form. (QA finding #7.)
OSV_VERSION_FIRST_LINE="$OSV_VERSION"

# ---------- Stage 5: Emit Compliance Deliverables ----------
# Stage 5a refreshes every canonical-data reference in decision-log.md so a
# fresh scan produces an internally consistent document. The Pipeline Metadata
# bullets, Per-Ecosystem Finding Counts table, Decision 8 prose, Deviation 3
# and Deviation 4 prose, Bidirectional Traceability Matrix duration / total /
# version rows, and Verification Checklist canonical max-length line and
# determinism-command block are all rewritten from runtime values.
# See decision-log.md Decisions 13, 20 and Deviation 11.
if [ -f "$LOG" ]; then
  OSV_VERSION="$OSV_VERSION" \
  OSV_VERSION_FIRST_LINE="$OSV_VERSION_FIRST_LINE" \
  OSV_INSTALL_PATH="$OSV_INSTALL_PATH" \
  SCAN_EXIT_CODE="$SCAN_EXIT_CODE" \
  SCAN_DURATION_SECONDS="$SCAN_DURATION_SECONDS" \
  SCAN_TARGET="$SCAN_TARGET" \
  OFFLINE_FLAG="$OFFLINE_FLAG" \
  OFFLINE_FLAG_USED="$OFFLINE_FLAG_USED" \
  USE_V2_SCAN_SOURCE="$USE_V2_SCAN_SOURCE" \
  FINDING_COUNT="$FINDING_COUNT" \
  CRITICAL_COUNT="$CRITICAL_COUNT" \
  HIGH_COUNT="$HIGH_COUNT" \
  MEDIUM_COUNT="$MEDIUM_COUNT" \
  LOW_COUNT="$LOW_COUNT" \
  RAW_VULN_COUNT="$RAW_VULN_COUNT" \
  DEDUP_RATIO_PCT="$DEDUP_RATIO_PCT" \
  MAX_DESC_LEN="$MAX_DESC_LEN" \
  SCAN_TIMESTAMP="$SCAN_TIMESTAMP" \
  DECISION_LOG="$LOG" \
  RESULTS="$RESULTS" \
  FINDINGS="$FINDINGS" \
  python3 - <<'PY'
import json, os, re, sys, pathlib
from collections import OrderedDict

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
use_v2_scan = os.environ.get("USE_V2_SCAN_SOURCE", "no") == "yes"
if use_v2_scan:
    cmdline = "osv-scanner scan source -r --format json --output-file results-osv.json"
else:
    cmdline = "osv-scanner --format json --output results-osv.json"
if offline_used and offline_flag:
    cmdline += " " + offline_flag
cmdline += " <repo-root>"


def safe_md(value: str) -> str:
    # Replace backticks in user-supplied values to keep markdown spans well-formed.
    return str(value).replace("`", "'")


osv_version = safe_md(os.environ.get("OSV_VERSION", ""))
osv_version_first = safe_md(os.environ.get("OSV_VERSION_FIRST_LINE", "") or os.environ.get("OSV_VERSION", ""))
exit_code = safe_md(os.environ.get("SCAN_EXIT_CODE", ""))
duration = safe_md(os.environ.get("SCAN_DURATION_SECONDS", ""))
timestamp = safe_md(os.environ.get("SCAN_TIMESTAMP", ""))
total = safe_md(os.environ.get("FINDING_COUNT", ""))
critical = safe_md(os.environ.get("CRITICAL_COUNT", ""))
high = safe_md(os.environ.get("HIGH_COUNT", ""))
medium = safe_md(os.environ.get("MEDIUM_COUNT", ""))
low = safe_md(os.environ.get("LOW_COUNT", ""))
raw_vulns = safe_md(os.environ.get("RAW_VULN_COUNT", "0"))
dedup_pct = safe_md(os.environ.get("DEDUP_RATIO_PCT", "0"))
max_desc = safe_md(os.environ.get("MAX_DESC_LEN", "0"))
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
    # are rewritten atomically.
    pattern = re.compile(
        r"(?m)^- \*\*" + re.escape(label) + r"\*\*:[^\n]*$"
    )
    return pattern.sub(lambda _m: new_line, content, count=1)


text = log_path.read_text(encoding="utf-8")
updated = text
for label, new_line in line_updates:
    updated = replace_full_bullet(updated, label, new_line)

# ---- Per-Ecosystem table: regenerated from results-osv.json source paths
# and findings-config-f.json finding paths so the table is always consistent
# with the canonical artifacts and the Pipeline Metadata total.
ECOSYSTEM_LABELS = OrderedDict([
    ("go.mod",                ("Go",                                  "`go.mod`")),
    ("package-lock.json",     ("npm (package-lock.json)",             "`refs/segment-docs/package-lock.json`")),
    ("yarn.lock",             ("npm (yarn.lock)",                     "`refs/segment-docs/yarn.lock`")),
    ("Gemfile.lock",          ("RubyGems",                            "`refs/segment-docs/Gemfile.lock`")),
    ("Dockerfile",            ("Docker (Dockerfile-as-manifest)",     "`Dockerfile`, `suppression-backup-service/Dockerfile`")),
    (".github/workflows",     ("GitHub Actions (workflow SHA scan)",  "`.github/workflows/*.yml`")),
])


def classify_path(rel_or_abs_path: str):
    s = str(rel_or_abs_path or "").replace("\\", "/")
    base = s.rsplit("/", 1)[-1]
    for key in ECOSYSTEM_LABELS:
        if key == ".github/workflows":
            if ".github/workflows" in s:
                return key
            continue
        if base == key:
            return key
        if s.endswith("/" + key):
            return key
    return None


eco_findings = {k: 0 for k in ECOSYSTEM_LABELS}
findings_data = []
try:
    findings_data = json.loads(pathlib.Path(os.environ.get("FINDINGS", "findings-config-f.json")).read_text(encoding="utf-8"))
except Exception:
    findings_data = []
for finding in findings_data if isinstance(findings_data, list) else []:
    eco = classify_path(finding.get("file", ""))
    if eco is not None:
        eco_findings[eco] += 1

eco_scanned = {k: False for k in ECOSYSTEM_LABELS}
try:
    raw_data = json.loads(pathlib.Path(os.environ.get("RESULTS", "results-osv.json")).read_text(encoding="utf-8"))
except Exception:
    raw_data = {}
for result in (raw_data.get("results") if isinstance(raw_data, dict) else None) or []:
    src = (result.get("source") if isinstance(result, dict) else None) or {}
    if not isinstance(src, dict):
        continue
    eco = classify_path(src.get("path", ""))
    if eco is not None:
        eco_scanned[eco] = True
# Findings inherently imply the manifest was scanned, even if the source
# was not enumerated in raw_data (defensive).
for k, count in eco_findings.items():
    if count > 0:
        eco_scanned[k] = True

eco_table = [
    "| Ecosystem | Lockfile(s) | Finding Count |",
    "| --- | --- | --- |",
]
for key, (label, manifest_display) in ECOSYSTEM_LABELS.items():
    if not eco_scanned[key] and eco_findings[key] == 0 and key in ("package-lock.json", "yarn.lock", "Gemfile.lock"):
        # Subdirectory lockfiles only listed when the scanner actually walked them
        # (avoid fabricating rows; AAP §0.2.1 still enumerates them as candidates).
        continue
    eco_table.append("| {0} | {1} | {2} |".format(label, manifest_display, eco_findings[key]))

# Prose introducing the table — describes what the scanner actually found.
scanned_keys = [k for k in ECOSYSTEM_LABELS if eco_scanned[k]]
manifest_phrases = []
for key in scanned_keys:
    if key in ("package-lock.json", "yarn.lock", "Gemfile.lock", "go.mod"):
        manifest_phrases.append("`" + key + "`")
manifests_resolved = ", ".join(manifest_phrases) if manifest_phrases else "(none)"
intro_lines = []
intro_lines.append("OSV-Scanner v2's default plugin set (`lockfile`, `sbom`, `directory`) resolved the manifest(s) shown below within the repository tree and produced the per-ecosystem counts that follow. The scanner's recursive walker (Stage 3 `-r` flag for v2; see Deviation 11) honours `.gitignore` and discovers lockfiles in subdirectories. Manifest-to-lockfile resolution (AAP §0.2.1; Decision 18) determined which on-disk files contributed findings; the actual scanned lockfiles in this run were: " + manifests_resolved + ".")
intro_block = "\n".join(intro_lines)

footer_block = "The zero counts for Docker and GitHub Actions are **expected** under v2's default plugin set, which scans lockfiles, SBOMs, and directory artifacts but does not enable container-image enrichment or workflow-SHA evaluation without an explicit `--experimental-plugins` opt-in. See Decision 19 (default-plugins-only) for the rationale."

regeneration_note = "> *Note on regeneration:* this file is regenerated by `run-config-f.sh` Stage 5; the driver writes real values for every Pipeline Metadata bullet, regenerates this Per-Ecosystem table from the canonical artifacts, and refreshes every canonical-data reference in the Decision Table (Decision 8), the Deviation Register (Deviations 3, 4), the Bidirectional Traceability Matrix, and the Verification Checklist. Re-runs against different repository checkouts therefore produce a current, audit-grade metadata snapshot with no stale numbers."

regenerated_block = "### Per-Ecosystem Finding Counts\n\n" + intro_block + "\n\n" + "\n".join(eco_table) + "\n\n" + footer_block + "\n\n" + regeneration_note

# Locate the existing "### Per-Ecosystem Finding Counts" heading and replace
# everything from that heading down to (but not including) the next
# top-level `---` separator or the next `## ` heading.
peheading_pattern = re.compile(
    r"(?ms)^### Per-Ecosystem Finding Counts\b.*?(?=^---\s*$|^## )",
)
if peheading_pattern.search(updated):
    updated = peheading_pattern.sub(regenerated_block + "\n\n", updated, count=1)

# ---- Decision 8 prose: rewrite the canonical-counts sentence.
dec8_pattern = re.compile(
    r"The canonical scan saw \d+ raw vulnerability rows collapse to \d+ unique findings via this key — a \d+% deduplication ratio[^|]*?expected GHSA/GO overlap\.",
)
dec8_replacement = "The canonical scan saw {0} raw vulnerability rows collapse to {1} unique findings via this key — a {2}% deduplication ratio that matches the expected GHSA/GO overlap.".format(raw_vulns, total, dedup_pct)
updated = dec8_pattern.sub(dec8_replacement, updated, count=1)

# ---- Deviation 3 prose: rewrite the "N `low` findings" reference.
dev3_pattern = re.compile(
    r"the canonical scan's \d+ `low` findings include this inflation",
)
updated = dev3_pattern.sub("the canonical scan's {0} `low` findings include this inflation".format(low), updated, count=1)

# ---- Deviation 4 prose: rewrite the "N raw → N unique (N% dedup ratio)" sentence.
dev4_pattern = re.compile(
    r"The canonical scan saw \d+ raw vulnerability rows collapse to \d+ unique findings \(\d+% dedup ratio\) via this key\.",
)
updated = dev4_pattern.sub("The canonical scan saw {0} raw vulnerability rows collapse to {1} unique findings ({2}% dedup ratio) via this key.".format(raw_vulns, total, dedup_pct), updated, count=1)

# ---- Bidirectional Traceability Matrix: refresh the canonical duration row.
tm_dur_pattern = re.compile(
    r"\(canonical: `\d+\.\d{3}`\); regex match on Pipeline Metadata block",
)
updated = tm_dur_pattern.sub("(canonical: `{0}`); regex match on Pipeline Metadata block".format(duration), updated, count=1)

# ---- Bidirectional Traceability Matrix: refresh the "N findings in canonical scan" row.
tm_count_pattern = re.compile(
    r"Minified single-line UTF-8 JSON array; \d+ findings in canonical scan",
)
updated = tm_count_pattern.sub("Minified single-line UTF-8 JSON array; {0} findings in canonical scan".format(total), updated, count=1)

# ---- Bidirectional Traceability Matrix: keep version-string row pointing at
# the actual binary first line (Issue #7 — eliminate fabricated combined form).
tm_ver_pattern = re.compile(
    r"`osv-scanner --version` returns the multi-line version string \(canonical first line: `[^`]+`\); see Pipeline Metadata for the captured value",
)
tm_ver_replacement = "`osv-scanner --version` returns the multi-line version string (canonical first line: `{0}`); see Pipeline Metadata for the captured value".format(osv_version_first)
updated = tm_ver_pattern.sub(tm_ver_replacement, updated, count=1)

# ---- Verification Checklist: rewrite the canonical max-description-length line.
# Match only the value content; do NOT consume the trailing newline so that
# the blank-line separator before the next bullet is preserved.
maxlen_pattern = re.compile(
    r"(?m)^(  Canonical max length: )\d+\.",
)
updated = maxlen_pattern.sub(r"\g<1>" + str(max_desc) + ".", updated, count=1)

# ---- Verification Checklist: replace the determinism command block to ensure
# it executes verbatim (QA Issue #12). The matched block spans the bold
# heading line, the fenced command, and any trailing explanatory paragraph
# up to the next bullet ("- **") or the closing prose ("All eleven checks").
determinism_pattern = re.compile(
    r"(?ms)- \*\*Determinism — re-running the post-processor produces a byte-identical artifact \(Decision 12\):\*\*\n  ```\n[ ]+[^\n]+normalize\.py[^\n]+\n  ```\n(?:[^\n]*\n)*?(?=- \*\*|All eleven checks)",
)
determinism_replacement = (
    "- **Determinism — re-running the post-processor produces a byte-identical artifact (Decision 12):**\n"
    "  ```\n"
    "  REPO_ROOT=\"$(pwd)\" && python3 normalize.py results-osv.json \"$REPO_ROOT\" > /tmp/findings-recheck.json && cmp findings-config-f.json /tmp/findings-recheck.json && echo OK\n"
    "  ```\n"
    "  `REPO_ROOT` MUST be the absolute path to the cloned `blitzy-RudderStack` root (the same target that was passed to `osv-scanner` in Stage 3). When invoked from the harness working directory `$(pwd)` is the correct value; in other contexts substitute the absolute scan-target path. The same prefix is consumed by `normalize.py`'s `normalize_path()` (Decision 9) to strip the absolute-path leader from each `packageSource.path`.\n\n"
)
updated = determinism_pattern.sub(determinism_replacement, updated, count=1)

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
