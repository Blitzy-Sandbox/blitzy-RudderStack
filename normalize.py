#!/usr/bin/env python3
"""normalize.py: OSV-Scanner -> findings-config-f.json post-processor.

Reads OSV-Scanner JSON output (results-osv.json) and emits a flat array of
finding objects conforming to the user-fixed schema
``{file, line, severity, cwe, description}`` as minified single-line UTF-8
JSON on stdout. The driver redirects stdout to ``findings-config-f.json``.

CLI::

    python3 normalize.py results-osv.json [<repo-root-prefix>]

Exit codes:
    0  success
    1  input file unreadable or not valid JSON
    2  usage error (missing required argument)

This module is deterministic and idempotent: given identical input bytes,
the emitted output is byte-identical across runs.

See ``decision-log.md`` for rationale on every non-trivial choice.
"""
import json
import os
import re
import sys
from pathlib import Path

SEVERITY_RANK = {"critical": 4, "high": 3, "medium": 2, "low": 1}
CVE_RE = re.compile(r"^CVE-\d{4}-\d+$")
GHSA_QUAL_MAP = {
    "CRITICAL": "critical",
    "HIGH": "high",
    "MODERATE": "medium",
    "LOW": "low",
}
WHITESPACE_RE = re.compile(r"\s+")

# CVSS v3.0/v3.1 metric value tables (shared between v3.0 and v3.1)
_V3_AV = {"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.20}
_V3_AC = {"L": 0.77, "H": 0.44}
_V3_PR_U = {"N": 0.85, "L": 0.62, "H": 0.27}  # Scope == Unchanged
_V3_PR_C = {"N": 0.85, "L": 0.68, "H": 0.50}  # Scope == Changed
_V3_UI = {"N": 0.85, "R": 0.62}
_V3_CIA = {"H": 0.56, "L": 0.22, "N": 0.0}

# CVSS v2 metric value tables
_V2_AV = {"L": 0.395, "A": 0.646, "N": 1.0}
_V2_AC = {"H": 0.35, "M": 0.61, "L": 0.71}
_V2_AU = {"M": 0.45, "S": 0.56, "N": 0.704}
_V2_CIA = {"N": 0.0, "P": 0.275, "C": 0.660}


def _roundup(x):
    # CVSS v3.1 roundup spec: ceil to 1dp while sidestepping FP edge cases.
    n = int(round(x * 100000))
    if n % 10000 == 0:
        return n / 100000.0
    return (n // 10000 + 1) / 10.0


def _parse_v3(metrics):
    scope = metrics["S"]
    pr_table = _V3_PR_C if scope == "C" else _V3_PR_U
    av = _V3_AV[metrics["AV"]]
    ac = _V3_AC[metrics["AC"]]
    pr = pr_table[metrics["PR"]]
    ui = _V3_UI[metrics["UI"]]
    c = _V3_CIA[metrics["C"]]
    i = _V3_CIA[metrics["I"]]
    a = _V3_CIA[metrics["A"]]
    iss = 1.0 - ((1.0 - c) * (1.0 - i) * (1.0 - a))
    if scope == "U":
        impact = 6.42 * iss
    else:
        impact = 7.52 * (iss - 0.029) - 3.25 * (iss - 0.02) ** 15
    exploit = 8.22 * av * ac * pr * ui
    if impact <= 0:
        return 0.0
    if scope == "U":
        return _roundup(min(impact + exploit, 10.0))
    return _roundup(min(1.08 * (impact + exploit), 10.0))


def _parse_v2(metrics):
    av = _V2_AV[metrics["AV"]]
    ac = _V2_AC[metrics["AC"]]
    au = _V2_AU[metrics["Au"]]
    c = _V2_CIA[metrics["C"]]
    i = _V2_CIA[metrics["I"]]
    a = _V2_CIA[metrics["A"]]
    impact = 10.41 * (1.0 - (1.0 - c) * (1.0 - i) * (1.0 - a))
    exploit = 20.0 * av * ac * au
    f_imp = 0.0 if impact == 0 else 1.176
    base = (0.6 * impact + 0.4 * exploit - 1.5) * f_imp
    if base < 0:
        base = 0.0
    return round(base * 10) / 10.0


def parse_cvss(score_vector):
    """Extract a CVSS base score from a vector string.

    Supports CVSS v3.0/v3.1 vectors (prefix ``CVSS:3.x/``) and CVSS v2
    vectors (no prefix). Returns ``-1.0`` on any parse failure so callers
    can fall back to qualitative severity.
    """
    if not isinstance(score_vector, str) or not score_vector.strip():
        return -1.0
    s = score_vector.strip()
    is_v3 = s.upper().startswith("CVSS:3")
    body = s.split("/", 1)[1] if (is_v3 and "/" in s) else s
    metrics = {}
    for part in body.split("/"):
        if ":" in part:
            k, v = part.split(":", 1)
            metrics[k.strip()] = v.strip()
    try:
        return _parse_v3(metrics) if is_v3 else _parse_v2(metrics)
    except (KeyError, ValueError, TypeError, ZeroDivisionError):
        return -1.0


def bucket(score):
    """Map a CVSS base score to one of ``critical``/``high``/``medium``/``low``."""
    if score >= 9.0:
        return "critical"
    if score >= 7.0:
        return "high"
    if score >= 4.0:
        return "medium"
    return "low"


def resolve_cwe(vuln):
    """Apply the three-step CWE resolution ladder; never returns empty string.

    1. ``database_specific.cwe_ids[0]`` if present.
    2. First ``aliases[]`` entry matching ``^CVE-\\d{4}-\\d+$``.
    3. The OSV ``id`` itself (e.g., ``GHSA-...``, ``GO-...``).
    """
    cwes = (vuln.get("database_specific") or {}).get("cwe_ids") or []
    if cwes:
        return str(cwes[0])
    for alias in vuln.get("aliases") or []:
        if CVE_RE.match(str(alias)):
            return str(alias)
    return str(vuln.get("id") or "UNKNOWN")


def pick_description(vuln):
    """Pick description, collapse whitespace, truncate to 200 code points."""
    desc = vuln.get("summary") or vuln.get("details") or vuln.get("id") or ""
    desc = WHITESPACE_RE.sub(" ", str(desc)).strip()
    return desc[:200]


def normalize_path(p, prefix):
    """Strip the repo-root prefix from an absolute path.

    Returns the relative path with forward slashes. Falls back to the
    absolute path (sans leading slash) when the prefix does not match.
    """
    if not p:
        return ""
    s = str(p).replace("\\", "/")
    pre = str(prefix or "").rstrip("/").replace("\\", "/")
    if pre and s.startswith(pre + "/"):
        s = s[len(pre) + 1:]
    elif pre and s == pre:
        s = ""
    return s.lstrip("/")


def compute_severity(vuln):
    scores = []
    for sev in vuln.get("severity") or []:
        v = parse_cvss(sev.get("score") or "")
        if v >= 0:
            scores.append(v)
    if scores:
        return bucket(max(scores))
    qual = (vuln.get("database_specific") or {}).get("severity")
    if isinstance(qual, str) and qual.upper() in GHSA_QUAL_MAP:
        return GHSA_QUAL_MAP[qual.upper()]
    return "low"


def collect_findings(raw, prefix):
    out = []
    for result in raw.get("results") or []:
        # OSV-Scanner v2 emits ``source`` while v1 emitted ``packageSource``;
        # accept either for forward and backward compatibility.
        ps = result.get("source") or result.get("packageSource") or {}
        file_path = normalize_path(ps.get("path") or "", prefix)
        for pkg in result.get("packages") or []:
            for vuln in pkg.get("vulnerabilities") or []:
                out.append({
                    "file": file_path,
                    "line": 0,
                    "severity": compute_severity(vuln),
                    "cwe": resolve_cwe(vuln),
                    "description": pick_description(vuln),
                })
    return out


def dedupe(findings):
    seen = {}
    for f in findings:
        key = (f["file"], f["cwe"], f["description"][:80])
        prev = seen.get(key)
        if prev is None or SEVERITY_RANK[f["severity"]] > SEVERITY_RANK[prev["severity"]]:
            seen[key] = f
    return list(seen.values())


def sort_findings(findings):
    return sorted(
        findings,
        key=lambda f: (f["file"], -SEVERITY_RANK[f["severity"]], f["cwe"], f["description"]),
    )


def main():
    """Read ``results-osv.json`` from ``argv[1]``; write findings JSON to stdout."""
    if len(sys.argv) < 2:
        sys.stderr.write("usage: normalize.py <results-osv.json> [<repo-root-prefix>]\n")
        return 2
    in_path = Path(sys.argv[1])
    prefix = sys.argv[2] if len(sys.argv) > 2 else os.getcwd()
    try:
        with in_path.open("r", encoding="utf-8") as fh:
            raw = json.load(fh)
    except (OSError, json.JSONDecodeError) as e:
        sys.stderr.write("failed to read {0}: {1}\n".format(in_path, e))
        return 1
    if not isinstance(raw, dict):
        raw = {"results": []}
    findings = sort_findings(dedupe(collect_findings(raw, prefix)))
    sys.stdout.write(json.dumps(findings, separators=(",", ":"), ensure_ascii=False))
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
