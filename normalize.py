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
import json, os, re, sys
from pathlib import Path

RANK = {"critical": 4, "high": 3, "medium": 2, "low": 1}
CVE_RE = re.compile(r"^CVE-\d{4}-\d+$")
WS_RE = re.compile(r"\s+")
QUAL = {"CRITICAL": "critical", "HIGH": "high", "MODERATE": "medium", "LOW": "low"}
V3 = {"AV": {"N": .85, "A": .62, "L": .55, "P": .20}, "AC": {"L": .77, "H": .44}, "UI": {"N": .85, "R": .62}, "CIA": {"H": .56, "L": .22, "N": 0.0}, "PRU": {"N": .85, "L": .62, "H": .27}, "PRC": {"N": .85, "L": .68, "H": .50}}
V2 = {"AV": {"L": .395, "A": .646, "N": 1.0}, "AC": {"H": .35, "M": .61, "L": .71}, "Au": {"M": .45, "S": .56, "N": .704}, "CIA": {"N": 0.0, "P": .275, "C": .660}}


def _v3(m):
    u, c = m["S"] == "U", V3["CIA"]
    iss = 1 - (1 - c[m["C"]]) * (1 - c[m["I"]]) * (1 - c[m["A"]])
    imp = 6.42 * iss if u else 7.52 * (iss - 0.029) - 3.25 * (iss - 0.02) ** 15
    if imp <= 0: return 0.0
    exp = 8.22 * V3["AV"][m["AV"]] * V3["AC"][m["AC"]] * V3["PRU" if u else "PRC"][m["PR"]] * V3["UI"][m["UI"]]
    n = int(round(min((imp + exp) if u else 1.08 * (imp + exp), 10.0) * 100000))
    return n / 100000.0 if n % 10000 == 0 else (n // 10000 + 1) / 10.0


def _v2(m):
    c = V2["CIA"]
    imp = 10.41 * (1 - (1 - c[m["C"]]) * (1 - c[m["I"]]) * (1 - c[m["A"]]))
    exp = 20 * V2["AV"][m["AV"]] * V2["AC"][m["AC"]] * V2["Au"][m["Au"]]
    return round(max((0.6 * imp + 0.4 * exp - 1.5) * (1.176 if imp else 0.0), 0.0) * 10) / 10.0


def parse_cvss(score_vector: str) -> float:
    if not isinstance(score_vector, str) or not score_vector.strip(): return -1.0
    s = score_vector.strip()
    v3 = s.upper().startswith("CVSS:3")
    body = s.split("/", 1)[1] if (v3 and "/" in s) else s
    m = {k.strip(): v.strip() for k, v in (p.split(":", 1) for p in body.split("/") if ":" in p)}
    try: return _v3(m) if v3 else _v2(m)
    except (KeyError, ValueError, TypeError, ZeroDivisionError): return -1.0


def bucket(score: float) -> str:
    return "critical" if score >= 9.0 else "high" if score >= 7.0 else "medium" if score >= 4.0 else "low"


def resolve_cwe(vuln: dict) -> str:
    for c in (vuln.get("database_specific") or {}).get("cwe_ids") or []:
        c = str(c).strip()
        if c: return c
    for a in vuln.get("aliases") or []:
        if CVE_RE.match(str(a)): return str(a)
    return str(vuln.get("id") or "UNKNOWN")


def pick_description(vuln: dict) -> str:
    return WS_RE.sub(" ", str(vuln.get("summary") or vuln.get("details") or vuln.get("id") or "")).strip()[:200]


def normalize_path(p: str, prefix: str) -> str:
    if not p: return ""
    s = str(p).replace("\\", "/")
    pre = str(prefix or "").rstrip("/").replace("\\", "/")
    if pre and s.startswith(pre + "/"): return s[len(pre) + 1:].lstrip("/")
    if pre and s == pre: return ""
    return s


def _sev(v):
    s = [x for x in (parse_cvss(e.get("score") or "") for e in v.get("severity") or [] if isinstance(e, dict)) if x >= 0]
    if s: return bucket(max(s))
    q = (v.get("database_specific") or {}).get("severity")
    return QUAL.get(q.upper(), "low") if isinstance(q, str) else "low"


def main() -> int:
    if len(sys.argv) < 2:
        sys.stderr.write("usage: normalize.py <results-osv.json> [<repo-root-prefix>]\n"); return 2
    in_path = Path(sys.argv[1])
    prefix = sys.argv[2] if len(sys.argv) > 2 else os.getcwd()
    try: raw = json.loads(in_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        sys.stderr.write("failed to read {0}: {1}\n".format(in_path, e)); return 1
    if not isinstance(raw, dict): raw = {"results": []}
    seen = {}
    for r in raw.get("results") or []:
        if not isinstance(r, dict): continue
        ps = r.get("source") or r.get("packageSource") or {}
        fp = normalize_path((ps if isinstance(ps, dict) else {}).get("path") or "", prefix)
        for pkg in r.get("packages") or []:
            if not isinstance(pkg, dict): continue
            for v in pkg.get("vulnerabilities") or []:
                if not isinstance(v, dict): continue
                f = {"file": fp, "line": 0, "severity": _sev(v), "cwe": resolve_cwe(v), "description": pick_description(v)}
                k = (f["file"], f["cwe"], f["description"][:80])
                if k not in seen or RANK[f["severity"]] > RANK[seen[k]["severity"]]: seen[k] = f
    findings = sorted(seen.values(), key=lambda f: (f["file"], -RANK[f["severity"]], f["cwe"], f["description"]))
    sys.stdout.write(json.dumps(findings, separators=(",", ":"), ensure_ascii=False) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
