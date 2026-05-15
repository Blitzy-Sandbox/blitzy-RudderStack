#!/usr/bin/env python3

"""Normalize Gosec SARIF 2.1.0 output into the contractual single-line JSON
array deliverable findings-config-d.json. See decision-log.md for the why.

Usage:
    python3 normalize-findings.py <sarif_in> <json_out> [--repo-root REPO_ROOT]

Example:
    python3 scripts/normalize-findings.py results-gosec.sarif findings-config-d.json
"""

import argparse
import json
import os
import re
import sys
from pathlib import Path
from urllib.parse import unquote, urlparse


SEVERITY_MAP = {
    "error": "critical",
    "warning": "high",
    "note": "medium",
    "info": "low",
}

GOSEC_RULE_TO_CWE = {
    "G101": "798", "G102": "200", "G103": "242", "G104": "703",
    "G106": "322", "G107": "88",  "G108": "200", "G109": "190",
    "G110": "409",
    "G201": "89",  "G202": "89",  "G203": "79",  "G204": "78",
    "G301": "276", "G302": "276", "G303": "377", "G304": "22",
    "G305": "22",  "G306": "276", "G307": "703",
    "G401": "326", "G402": "295", "G403": "310", "G404": "338",
    "G501": "327", "G502": "327", "G503": "327", "G504": "327", "G505": "327",
    "G601": "118",
}

REQUIRED_FIELDS = ("file", "line", "severity", "cwe", "description")
ALLOWED_SEVERITIES = {"critical", "high", "medium", "low"}
CWE_PATTERN = re.compile(r"^CWE-(\d+|Unknown)$")
WHITESPACE_PATTERN = re.compile(r"\s+")
MAX_DESCRIPTION_LENGTH = 200


def _translate_severity(level):
    if level is None:
        return None
    return SEVERITY_MAP.get(level.lower() if isinstance(level, str) else "")


def _normalize_description(text):
    if not text:
        return ""
    collapsed = WHITESPACE_PATTERN.sub(" ", text).strip()
    return collapsed[:MAX_DESCRIPTION_LENGTH]


def _format_cwe(value):
    if value is None:
        return "CWE-Unknown"
    s = str(value).strip()
    if not s:
        return "CWE-Unknown"
    if s.upper().startswith("CWE-"):
        return "CWE-" + s[4:]
    if s.isdigit():
        return f"CWE-{s}"
    match = re.search(r"(\d+)", s)
    if match:
        return f"CWE-{match.group(1)}"
    return "CWE-Unknown"


def _strip_uri(uri, base_uris, repo_root):
    if not uri:
        return ""

    if isinstance(uri, tuple):
        uri_str, uri_base_id = uri
    else:
        uri_str, uri_base_id = uri, None

    if not uri_str:
        return ""

    resolved_base = ""
    if uri_base_id and uri_base_id in base_uris:
        base_entry = base_uris[uri_base_id]
        if isinstance(base_entry, dict):
            base_uri = base_entry.get("uri", "")
            nested_base_id = base_entry.get("uriBaseId")
            if nested_base_id and nested_base_id in base_uris:
                resolved_base = _strip_uri(
                    (base_uri, nested_base_id), base_uris, repo_root
                )
            else:
                resolved_base = base_uri
        elif isinstance(base_entry, str):
            resolved_base = base_entry

    if uri_str.startswith("file://"):
        parsed = urlparse(uri_str)
        path = unquote(parsed.path)
    else:
        path = unquote(uri_str)

    if resolved_base:
        if resolved_base.startswith("file://"):
            base_parsed = urlparse(resolved_base)
            base_path = unquote(base_parsed.path)
        else:
            base_path = unquote(resolved_base)
        if not os.path.isabs(path):
            path = os.path.join(base_path, path)

    if os.path.isabs(path):
        try:
            path = os.path.relpath(path, repo_root)
        except ValueError:
            pass

    path = path.replace("\\", "/")

    while path.startswith("./"):
        path = path[2:]

    return path


def _build_rules_index(run):
    driver = (run.get("tool", {}) or {}).get("driver", {}) or {}
    rules = driver.get("rules", []) or []
    index = {}
    for rule in rules:
        if isinstance(rule, dict):
            rule_id = rule.get("id")
            if rule_id:
                index[rule_id] = rule
    return index


def _resolve_cwe(rule_id, rules_index):
    rule = rules_index.get(rule_id, {}) if rule_id else {}
    if not isinstance(rule, dict):
        rule = {}

    props = rule.get("properties", {}) or {}
    if not isinstance(props, dict):
        props = {}

    cwe_prop = props.get("cwe")
    if isinstance(cwe_prop, dict):
        cwe_id = cwe_prop.get("id")
        if cwe_id:
            return _format_cwe(cwe_id)
    elif isinstance(cwe_prop, str) and cwe_prop:
        return _format_cwe(cwe_prop)

    for rel in rule.get("relationships", []) or []:
        if not isinstance(rel, dict):
            continue
        target = rel.get("target", {}) or {}
        if not isinstance(target, dict):
            continue
        tool_component = target.get("toolComponent", {}) or {}
        if not isinstance(tool_component, dict):
            tool_component = {}
        component_name = (tool_component.get("name") or "").upper()
        if "CWE" in component_name:
            target_id = target.get("id")
            if target_id:
                return _format_cwe(target_id)

    tags = props.get("tags", []) or []
    for tag in tags:
        if isinstance(tag, str):
            match = re.search(r"cwe[-/](\d+)", tag, re.IGNORECASE)
            if match:
                return f"CWE-{match.group(1)}"

    if rule_id and rule_id in GOSEC_RULE_TO_CWE:
        return f"CWE-{GOSEC_RULE_TO_CWE[rule_id]}"

    if rule_id:
        print(f"warning: unmapped rule {rule_id}", file=sys.stderr)
    return "CWE-Unknown"


def _extract_findings(sarif, repo_root):
    findings = []
    runs = sarif.get("runs", []) or []

    for run in runs:
        if not isinstance(run, dict):
            continue

        rules_index = _build_rules_index(run)
        base_uris = run.get("originalUriBaseIds", {}) or {}
        if not isinstance(base_uris, dict):
            base_uris = {}
        results = run.get("results", []) or []

        for result in results:
            if not isinstance(result, dict):
                continue

            kind = result.get("kind", "fail")
            if kind != "fail":
                continue

            level = result.get("level")
            severity = _translate_severity(level)
            if severity is None:
                continue

            locations = result.get("locations", []) or []
            file_path = ""
            line_number = 0

            if locations and isinstance(locations[0], dict):
                physical = locations[0].get("physicalLocation", {}) or {}
                if not isinstance(physical, dict):
                    physical = {}
                artifact = physical.get("artifactLocation", {}) or {}
                if not isinstance(artifact, dict):
                    artifact = {}
                uri = artifact.get("uri", "")
                uri_base_id = artifact.get("uriBaseId")
                file_path = _strip_uri(
                    (uri, uri_base_id) if uri_base_id else uri,
                    base_uris,
                    repo_root,
                )
                region = physical.get("region", {}) or {}
                if not isinstance(region, dict):
                    region = {}
                start_line = region.get("startLine")
                if isinstance(start_line, bool):
                    line_number = 0
                elif isinstance(start_line, int):
                    line_number = start_line
                elif isinstance(start_line, str) and start_line.isdigit():
                    line_number = int(start_line)

            rule_id = result.get("ruleId", "")
            cwe = _resolve_cwe(rule_id, rules_index)

            message = result.get("message", {}) or {}
            text = message.get("text", "") if isinstance(message, dict) else ""
            description = _normalize_description(text)

            finding = {
                "file": file_path,
                "line": line_number,
                "severity": severity,
                "cwe": cwe,
                "description": description,
            }
            findings.append(finding)

    return findings


def _emit_minified(findings, out_path):
    payload = json.dumps(findings, separators=(",", ":"), ensure_ascii=False)
    with open(out_path, "wb") as f:
        f.write(payload.encode("utf-8"))


def _self_check(out_path):
    with open(out_path, "rb") as f:
        data = f.read()

    newline_count = data.count(b"\n")
    assert newline_count == 0, (
        f"output contains {newline_count} newlines (must be 0 for single-line file)"
    )

    arr = json.loads(data.decode("utf-8"))
    assert isinstance(arr, list), (
        f"output is not a JSON array: {type(arr).__name__}"
    )

    required_keys = set(REQUIRED_FIELDS)
    for i, obj in enumerate(arr):
        assert isinstance(obj, dict), (
            f"finding {i} is not a dict: {type(obj).__name__}"
        )
        actual_keys = set(obj.keys())
        assert actual_keys == required_keys, (
            f"finding {i} key set is {actual_keys}, expected {required_keys}"
        )
        assert isinstance(obj["line"], int) and not isinstance(obj["line"], bool), (
            f"finding {i} line is not int: {type(obj['line']).__name__}"
        )
        assert obj["severity"] in ALLOWED_SEVERITIES, (
            f"finding {i} severity {obj['severity']!r} not in {ALLOWED_SEVERITIES}"
        )
        assert CWE_PATTERN.match(obj["cwe"]), (
            f"finding {i} cwe {obj['cwe']!r} does not match pattern ^CWE-(\\d+|Unknown)$"
        )
        assert len(obj["description"]) <= MAX_DESCRIPTION_LENGTH, (
            f"finding {i} description is {len(obj['description'])} chars (>{MAX_DESCRIPTION_LENGTH})"
        )


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Normalize Gosec SARIF 2.1.0 output to a single-line minified JSON array.",
    )
    parser.add_argument(
        "sarif_in",
        help="Path to the SARIF 2.1.0 input file (e.g., results-gosec.sarif)",
    )
    parser.add_argument(
        "json_out",
        help="Path to the minified JSON output file (e.g., findings-config-d.json)",
    )
    parser.add_argument(
        "--repo-root",
        default=None,
        help="Repository root for relative-path normalization (default: directory of sarif_in)",
    )
    args = parser.parse_args(argv)

    sarif_path = Path(args.sarif_in).resolve()
    out_path = Path(args.json_out)
    repo_root = (
        Path(args.repo_root).resolve()
        if args.repo_root
        else sarif_path.parent
    )

    try:
        with open(sarif_path, "rb") as f:
            sarif = json.loads(f.read().decode("utf-8"))
    except FileNotFoundError:
        print(f"error: SARIF input file not found: {sarif_path}", file=sys.stderr)
        return 1
    except json.JSONDecodeError as e:
        print(f"error: failed to parse SARIF input as JSON: {e}", file=sys.stderr)
        return 1
    except (OSError, UnicodeDecodeError) as e:
        print(f"error: failed to read SARIF input: {e}", file=sys.stderr)
        return 1

    if not isinstance(sarif, dict):
        print(
            f"error: SARIF root is not an object: {type(sarif).__name__}",
            file=sys.stderr,
        )
        return 1

    findings = _extract_findings(sarif, str(repo_root))

    try:
        _emit_minified(findings, out_path)
    except OSError as e:
        print(f"error: failed to write output: {e}", file=sys.stderr)
        return 1

    try:
        _self_check(out_path)
    except AssertionError as e:
        print(f"error: post-condition self-check failed: {e}", file=sys.stderr)
        return 2

    print(f"wrote {len(findings)} findings to {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
