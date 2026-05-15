#!/usr/bin/env python3
"""Normalize Semgrep SARIF output to the five-field minified JSON shape.

Usage:
    python3 normalize-sarif.py <sarif_input_path> <json_output_path>

Reads a SARIF 2.1.0 document and writes a single-line JSON array of records
with keys: file, line, severity, cwe, description.

Exit codes:
    0 - success; findings-config-b.json written and all post-conditions passed
    1 - generic failure (file not found, JSON parse error, etc.)
    2 - usage error (wrong number of arguments)
    3 - post-condition assertion failure
"""

import json
import re
import subprocess
import sys
from pathlib import Path, PurePosixPath
from urllib.parse import unquote, urlparse


SEVERITY_MAP = {
    "error":   "critical",
    "warning": "high",
    "note":    "medium",
    "info":    "low",
    "none":    "low",
}

CWE_RE = re.compile(r"CWE-(\d+)", re.IGNORECASE)

DESCRIPTION_KEYWORD_TO_CWE = [
    (re.compile(r"\bsql injection\b", re.IGNORECASE), "CWE-89"),
    (re.compile(r"\bhardcoded (credential|password|secret|api[-_ ]?key|token)\b", re.IGNORECASE), "CWE-798"),
    (re.compile(r"\bsecret in (source|code)\b", re.IGNORECASE), "CWE-798"),
    (re.compile(r"\bcommand injection\b", re.IGNORECASE), "CWE-78"),
    (re.compile(r"\b(xss|cross[- ]site scripting)\b", re.IGNORECASE), "CWE-79"),
    (re.compile(r"\bpath traversal\b", re.IGNORECASE), "CWE-22"),
    (re.compile(r"\bdirectory traversal\b", re.IGNORECASE), "CWE-22"),
    (re.compile(r"\b(weak crypto|insecure hash|md5|sha1)\b", re.IGNORECASE), "CWE-327"),
    (re.compile(r"\bopen redirect\b", re.IGNORECASE), "CWE-601"),
    (re.compile(r"\b(ssrf|server[- ]side request forgery)\b", re.IGNORECASE), "CWE-918"),
    (re.compile(r"\b(xxe|xml external entity)\b", re.IGNORECASE), "CWE-611"),
    (re.compile(r"\binsecure deserialization\b", re.IGNORECASE), "CWE-502"),
    (re.compile(r"\b(csrf|cross[- ]site request forgery)\b", re.IGNORECASE), "CWE-352"),
    (re.compile(r"\bnull pointer\b", re.IGNORECASE), "CWE-476"),
    (re.compile(r"\b(insecure (transport|http)|cleartext)\b", re.IGNORECASE), "CWE-319"),
]

DESCRIPTION_MAX_CHARS = 200
ALLOWED_SEVERITIES = ("critical", "high", "medium", "low")
REQUIRED_KEYS = ("file", "line", "severity", "cwe", "description")


def load_sarif(path):
    """Load and return a SARIF JSON document from the given path."""
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def index_rules(run):
    """Return the rules list from a SARIF run, defaulting to []."""
    return run.get("tool", {}).get("driver", {}).get("rules", [])


def _resolve_rule(result, rules):
    """Resolve the SARIF rule object for a result.

    Prefers result.ruleIndex when it is a valid index into rules;
    falls back to a linear scan keyed on result.ruleId against rule.id.
    Returns the rule dict or None if no match.
    """
    rule_index = result.get("ruleIndex")
    if isinstance(rule_index, bool):
        rule_index = None
    if isinstance(rule_index, int) and 0 <= rule_index < len(rules):
        return rules[rule_index]
    rule_id = result.get("ruleId")
    if rule_id:
        for r in rules:
            if r.get("id") == rule_id:
                return r
    return None


def map_severity(level, rule):
    """Map SARIF level to the four-tier canonical severity vocabulary.

    error -> critical, warning -> high, note -> medium, info -> low.
    If level is absent, falls back to rule.defaultConfiguration.level.
    If still absent, defaults to 'low'.
    """
    if level and isinstance(level, str) and level.lower() in SEVERITY_MAP:
        return SEVERITY_MAP[level.lower()]
    if rule:
        default_level = rule.get("defaultConfiguration", {}).get("level")
        if default_level and isinstance(default_level, str) and default_level.lower() in SEVERITY_MAP:
            return SEVERITY_MAP[default_level.lower()]
    return "low"


def extract_cwe_from_metadata(rule, result):
    """Read CWE from rule metadata or result properties.

    Implements Steps 1-3 of the CWE algorithm.
    Returns a 'CWE-<digits>' string, or None if no CWE token is found.
    """
    if rule:
        props = rule.get("properties", {}) or {}
        cwe_field = props.get("cwe")
        if cwe_field:
            if isinstance(cwe_field, str):
                m = CWE_RE.search(cwe_field)
                if m:
                    return f"CWE-{m.group(1)}"
            elif isinstance(cwe_field, list):
                for item in cwe_field:
                    if isinstance(item, str):
                        m = CWE_RE.search(item)
                        if m:
                            return f"CWE-{m.group(1)}"
        tags = props.get("tags", []) or []
        if isinstance(tags, list):
            for tag in tags:
                if isinstance(tag, str):
                    m = CWE_RE.search(tag)
                    if m:
                        return f"CWE-{m.group(1)}"
    rprops = result.get("properties", {}) or {}
    if isinstance(rprops, dict):
        for value in rprops.values():
            if isinstance(value, str):
                m = CWE_RE.search(value)
                if m:
                    return f"CWE-{m.group(1)}"
            elif isinstance(value, list):
                for item in value:
                    if isinstance(item, str):
                        m = CWE_RE.search(item)
                        if m:
                            return f"CWE-{m.group(1)}"
    return None


def infer_cwe_from_description(rule, fallback_text):
    """Infer the most specific CWE from textual descriptions.

    Implements Step 4 of the CWE algorithm. Concatenates the rule's
    shortDescription, fullDescription, help text/markdown, and the
    fallback message text. Returns a (cwe_string, source_text) tuple,
    where cwe_string is 'CWE-Other' when no keyword pattern matches.
    """
    candidates = []
    if rule:
        for key in ("shortDescription", "fullDescription"):
            section = rule.get(key, {}) or {}
            t = section.get("text") if isinstance(section, dict) else None
            if t:
                candidates.append(t)
        help_section = rule.get("help", {}) or {}
        if isinstance(help_section, dict):
            help_text = help_section.get("text") or help_section.get("markdown")
            if help_text:
                candidates.append(help_text)
    if fallback_text:
        candidates.append(fallback_text)
    haystack = " ".join(candidates)
    for pattern, cwe in DESCRIPTION_KEYWORD_TO_CWE:
        if pattern.search(haystack):
            return (cwe, haystack[:300])
    return ("CWE-Other", haystack[:300] if haystack else "(no description text)")


def sanitize_description(text):
    """Collapse whitespace runs to single spaces and truncate to 200 chars."""
    if not text:
        return ""
    flattened = " ".join(text.split())
    return flattened[:DESCRIPTION_MAX_CHARS]


def extract_scan_roots(run):
    """Return the absolute filesystem roots declared in SARIF originalUriBaseIds.

    Walks the run's originalUriBaseIds dictionary and returns the list of
    absolute POSIX paths declared as base URIs (e.g. %SRCROOT% -> file:///repo).
    Returned paths are POSIX-normalized strings with the file:// scheme and
    URL-encoding removed. An empty list is returned when no roots are declared
    (which is the common case for Semgrep CE SARIF that already emits relative
    URIs).
    """
    roots = []
    base_ids = run.get("originalUriBaseIds") or {}
    if not isinstance(base_ids, dict):
        return roots
    for base in base_ids.values():
        if not isinstance(base, dict):
            continue
        uri = base.get("uri")
        if not isinstance(uri, str) or not uri:
            continue
        decoded = _strip_file_scheme(uri)
        if decoded and decoded.startswith("/"):
            roots.append(decoded.rstrip("/"))
    return roots


def _strip_file_scheme(uri):
    """Strip the file:// scheme and URL-decode a SARIF URI to a plain path."""
    if not isinstance(uri, str):
        return ""
    parsed = urlparse(uri)
    if parsed.scheme == "file":
        return unquote(parsed.path)
    return unquote(uri)


def extract_file_path(location, scan_roots=None):
    """Read the relative path from a SARIF location.

    Handles three SARIF URI forms emitted in practice:

    1. Relative URI (Semgrep CE default; uriBaseId = %SRCROOT%):
       returned as-is after URL-decoding.
    2. file:// absolute URI (some tools emit these): scheme is stripped,
       URL-decoding is applied, and the path is made scan-root-relative
       when any declared root prefixes it.
    3. POSIX absolute path (e.g. /tmp/repo/gateway/foo.go): made
       scan-root-relative when any declared root prefixes it; otherwise
       returned with leading slashes stripped so the emitted value remains
       a relative path string suitable for cross-configuration comparison.

    Preserves deterministic, relative paths in the emitted findings so that
    the five-field schema's 'file' value is intercompatible with other
    configurations regardless of which absolute path the operator scanned.
    """
    raw = (
        location.get("physicalLocation", {})
        .get("artifactLocation", {})
        .get("uri", "")
    )
    if not isinstance(raw, str) or not raw:
        return ""

    decoded = _strip_file_scheme(raw)

    if not decoded.startswith("/"):
        return decoded

    if scan_roots:
        candidate = PurePosixPath(decoded)
        for root in scan_roots:
            try:
                relative = candidate.relative_to(PurePosixPath(root))
            except ValueError:
                continue
            return relative.as_posix()

    return decoded.lstrip("/")


def extract_line(location):
    """Read region.startLine from a SARIF location, coerced to int.

    Returns 0 when startLine is absent or non-coercible.
    """
    region = location.get("physicalLocation", {}).get("region", {}) or {}
    start_line = region.get("startLine", 0)
    try:
        return int(start_line)
    except (TypeError, ValueError):
        return 0


def project_record(result, rules, scan_roots=None):
    """Project a single SARIF result into the five-field findings record.

    Returns a tuple (record_dict, inference_record). inference_record is
    None when the CWE came from metadata, or a (rule_id, source_text, cwe)
    tuple when the CWE was inferred from description text.

    scan_roots is forwarded to extract_file_path so absolute SARIF URIs are
    normalized to scan-root-relative paths when the run declares
    originalUriBaseIds.
    """
    locations = result.get("locations", []) or []
    location = locations[0] if locations else {}

    rule = _resolve_rule(result, rules)

    file_path = extract_file_path(location, scan_roots=scan_roots)
    line = extract_line(location)

    severity = map_severity(result.get("level"), rule)

    cwe = extract_cwe_from_metadata(rule, result)
    inference_record = None
    if cwe is None:
        message_text = (result.get("message", {}) or {}).get("text", "")
        cwe, source_text = infer_cwe_from_description(rule, message_text)
        rule_id = (rule or {}).get("id") or result.get("ruleId") or "<unknown>"
        inference_record = (rule_id, source_text, cwe)

    description = sanitize_description((result.get("message", {}) or {}).get("text", ""))

    record = {
        "file":        file_path,
        "line":        line,
        "severity":    severity,
        "cwe":         cwe,
        "description": description,
    }
    return record, inference_record


def _fail_postcondition(message):
    """Print a post-condition failure message and exit with code 3."""
    print(f"FAIL: {message}", file=sys.stderr)
    sys.exit(3)


def assert_postconditions(records, output_path):
    """Enforce all Directive 3 pass/fail constraints on the output file.

    On any constraint failure, prints 'FAIL: <reason>' to stderr and
    exits the process with code 3.
    """
    path_str = str(output_path)

    wc_out = subprocess.check_output(["wc", "-l", path_str]).split()
    observed = wc_out[0].decode() if wc_out else "<empty>"
    if not wc_out or wc_out[0] != b"1":
        _fail_postcondition(f"wc -l == {observed}, expected 1")

    with open(output_path, "r", encoding="utf-8") as fh:
        data = json.load(fh)
    if not isinstance(data, list):
        _fail_postcondition("top-level is not a JSON array")

    required = set(REQUIRED_KEYS)
    for i, r in enumerate(data):
        if not isinstance(r, dict):
            _fail_postcondition(f"record {i} is not an object")
        if set(r.keys()) != required:
            _fail_postcondition(
                f"record {i} keys = {sorted(r.keys())}, expected {sorted(required)}"
            )
        for k in REQUIRED_KEYS:
            if r[k] is None:
                _fail_postcondition(f"record {i} field '{k}' is null")
        if not isinstance(r["line"], int) or isinstance(r["line"], bool):
            _fail_postcondition(
                f"record {i} line is not int: {type(r['line']).__name__}"
            )
        if r["severity"] not in ALLOWED_SEVERITIES:
            _fail_postcondition(
                f"record {i} severity '{r['severity']}' not in {ALLOWED_SEVERITIES}"
            )
        if not isinstance(r["cwe"], str) or not r["cwe"].startswith("CWE-"):
            _fail_postcondition(
                f"record {i} cwe '{r['cwe']}' does not start with CWE-"
            )
        if not isinstance(r["description"], str):
            _fail_postcondition(
                f"record {i} description is not str: {type(r['description']).__name__}"
            )
        if len(r["description"]) > DESCRIPTION_MAX_CHARS:
            _fail_postcondition(
                f"record {i} description length {len(r['description'])} > {DESCRIPTION_MAX_CHARS}"
            )
        if not isinstance(r["file"], str):
            _fail_postcondition(
                f"record {i} file is not str: {type(r['file']).__name__}"
            )


def main():
    """Entry point: read SARIF, project records, write minified JSON, validate."""
    if len(sys.argv) != 3:
        print(
            "Usage: python3 normalize-sarif.py <sarif_input> <json_output>",
            file=sys.stderr,
        )
        sys.exit(2)

    sarif_path = Path(sys.argv[1])
    output_path = Path(sys.argv[2])

    sarif = load_sarif(sarif_path)
    runs = sarif.get("runs", []) or []

    records = []
    inferences = []

    for run in runs:
        rules = index_rules(run)
        scan_roots = extract_scan_roots(run)
        for result in run.get("results", []) or []:
            record, inference = project_record(result, rules, scan_roots=scan_roots)
            records.append(record)
            if inference is not None:
                inferences.append(inference)

    if records:
        body = json.dumps(records, separators=(",", ":"), ensure_ascii=False)
    else:
        body = "[]"

    with open(output_path, "w", encoding="utf-8", newline="") as fh:
        fh.write(body)
        fh.write("\n")

    if inferences:
        print("CWE_INFERENCES:", file=sys.stderr)
        for rule_id, source, chosen in inferences:
            print(
                f"  rule_id={rule_id}\tchosen={chosen}\tsource={source[:100]}",
                file=sys.stderr,
            )

    assert_postconditions(records, output_path)

    print(f"Wrote {len(records)} record(s) to {output_path}", file=sys.stderr)
    sys.exit(0)


if __name__ == "__main__":
    main()
