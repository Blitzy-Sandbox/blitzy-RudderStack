#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Normalize Snyk SAST + dependency outputs into a single-line minified JSON array.

Inputs: results-snyk-code.sarif (SARIF 2.1.0), results-snyk-deps.json (Snyk --json).
Output: findings-config-h.json (UTF-8, single line + one trailing newline).
Exit codes: 0 success, 2 missing/malformed input. See DECISIONS.md for rationale.

Example invocation:
  python3 scripts/normalize-snyk-findings.py --sarif results-snyk-code.sarif --deps results-snyk-deps.json --out findings-config-h.json --repo-root .
"""
import argparse
import json
import os
import re
import sys

# ── Module-level constants ───────────────────────────────────────────────────

# Per AAP §0.4.4 severity mapping table.
# `none` → `low` is Decision #1 in DECISIONS.md.
SARIF_LEVEL_TO_SEVERITY = {
    'error':   'critical',
    'warning': 'high',
    'note':    'medium',
    'none':    'low',
}

# Per AAP §0.4.6: prefix-inclusive cap on the description field.
# See DECISIONS.md (Decision #3).
MAX_DESCRIPTION_CHARS = 200

# Per AAP §0.4.5; tag-scan regex. See DECISIONS.md (Decision #8).
CWE_TAG_PATTERN = re.compile(r'CWE-\d+')

# Fallback when neither CWE nor CVE is present.
UNKNOWN_CWE = 'UNKNOWN'

# The 5-field schema the AAP guarantees; field order is the verbatim order
# from §0.2.3 and is preserved in the emitted JSON via dict insertion order.
REQUIRED_FIELDS = ('file', 'line', 'severity', 'cwe', 'description')

# Prefixes applied to the description field per the AAP user-supplied
# field-mapping table (§0.2.3).
PREFIX_SAST = '[snyk-code] '
PREFIX_DEPS = '[snyk-deps] '

# Dependency-severity allowlist per AAP §0.2.3 (verbatim user-supplied schema
# constrains the unified `severity` to one of these four values).
# See DECISIONS.md (Decision #17) for the deterministic fallback to 'low' on
# unrecognized severity strings.
DEPS_SEVERITY_ALLOWLIST = frozenset({'critical', 'high', 'medium', 'low'})

# Deterministic fallback severity for unrecognized dependency-severity values.
DEPS_SEVERITY_FALLBACK = 'low'


# ── Helpers ──────────────────────────────────────────────────────────────────

def truncate_utf8(text, max_chars=MAX_DESCRIPTION_CHARS):
    """Normalize whitespace runs to single spaces, strip, and character-truncate.

    See DECISIONS.md (Decision #3) for the prefix-inclusive truncation contract
    and Decision #17 for the non-string coercion contract.

    Treats None and empty string identically: returns ''.
    Non-string inputs (ints, lists, dicts, etc.) are coerced via str(...) so
    this function NEVER raises TypeError on schema-malformed-but-valid JSON
    inputs (Decision #17).
    Collapses any run of whitespace (spaces, tabs, newlines, etc.) to a single
    space character, then strips leading/trailing whitespace, then truncates
    to at most max_chars characters.
    """
    if text is None or text == '':
        return ''
    # Coerce any non-string scalar/object to its str() representation so the
    # downstream re.sub never fails on int/list/dict inputs. See Decision #17.
    if not isinstance(text, str):
        text = str(text)
    normalized = re.sub(r'\s+', ' ', text).strip()
    if len(normalized) <= max_chars:
        return normalized
    return normalized[:max_chars]


def to_relative_path(uri, repo_root):
    """Convert an absolute or root-anchored SARIF/Snyk URI to a repo-relative path.

    See DECISIONS.md (Decision #9) for the relpath strategy and fallback
    behavior on cross-filesystem boundaries, and Decision #17 for the
    non-string coercion contract.

    Strips a leading "file://" scheme if present, then either relativizes
    against the resolved repo_root (when the URI is absolute) or normalizes
    the path (when it is already relative). On any path-resolution error,
    returns the (post-scheme-strip) URI unchanged.

    NEVER raises: non-string inputs are coerced via str(...) before any
    string/path operation, falsy inputs short-circuit to '', and a broad
    Exception catch on the path-resolution branch falls back to the
    (post-scheme-strip) string form (Decision #17).
    """
    if not uri:
        return ''
    # Coerce any non-string input (int, Path, None-like, etc.) to its string
    # form so .startswith, os.path.isabs, and os.path.relpath always receive a
    # str. See Decision #17.
    if not isinstance(uri, str):
        uri = str(uri)
    if uri.startswith('file://'):
        uri = uri[len('file://'):]
    try:
        repo_root_str = repo_root if isinstance(repo_root, str) else str(repo_root or '.')
        abs_root = os.path.abspath(repo_root_str)
        if os.path.isabs(uri):
            return os.path.relpath(uri, abs_root)
        return os.path.normpath(uri)
    except (ValueError, OSError, TypeError):
        return uri


def extract_cwe_from_rule(rule):
    """Extract a CWE-<n> identifier from a SARIF rule dict.

    See DECISIONS.md (Decision #8) for the SAST CWE extraction priority order
    and Decision #17 for the defensive non-dict input contract.

    Priority:
      1. rule.properties.cwe[0]  - canonical typed field
      2. rule.properties.tags    - scan for CWE-<n> pattern
      3. UNKNOWN_CWE fallback

    Non-dict or falsy inputs (rule is None, rule is a string, etc.) return
    UNKNOWN_CWE deterministically rather than raising AttributeError.
    """
    if not isinstance(rule, dict):
        return UNKNOWN_CWE
    props = rule.get('properties') if isinstance(rule.get('properties'), dict) else {}
    cwe_list = props.get('cwe') if isinstance(props.get('cwe'), list) else []
    if cwe_list:
        cwe_id = cwe_list[0]
        cwe_str = str(cwe_id) if cwe_id is not None else ''
        if not cwe_str:
            return UNKNOWN_CWE
        return cwe_str if cwe_str.startswith('CWE-') else f'CWE-{cwe_str}'
    tags = props.get('tags') if isinstance(props.get('tags'), list) else []
    for tag in tags:
        m = CWE_TAG_PATTERN.search(str(tag))
        if m:
            return m.group(0)
    return UNKNOWN_CWE


def extract_cwe_or_cve(identifiers):
    """Return the first CWE/CVE identifier from a Snyk vulnerability's identifiers map.

    See DECISIONS.md (Decision #2) for the CWE-first, CVE-fallback rationale
    and Decision #17 for the defensive non-dict input contract.

    Priority:
      1. identifiers.CWE[0]
      2. identifiers.CVE[0]
      3. UNKNOWN_CWE

    Non-dict inputs and absent/empty arrays degrade gracefully to UNKNOWN_CWE.
    """
    if not isinstance(identifiers, dict):
        return UNKNOWN_CWE
    cwe_list = identifiers.get('CWE') if isinstance(identifiers.get('CWE'), list) else []
    if cwe_list:
        value = cwe_list[0]
        if value is not None and str(value):
            return str(value)
    cve_list = identifiers.get('CVE') if isinstance(identifiers.get('CVE'), list) else []
    if cve_list:
        value = cve_list[0]
        if value is not None and str(value):
            return str(value)
    return UNKNOWN_CWE


# ── Normalizers ──────────────────────────────────────────────────────────────

def normalize_sarif(sarif_data, repo_root):
    """Convert SARIF 2.1.0 results into the 5-field record schema.

    For each runs[*].results[*]:
      file        := relative path from locations[0].physicalLocation.artifactLocation.uri
      line        := int(locations[0].physicalLocation.region.startLine) or 0
      severity    := SARIF_LEVEL_TO_SEVERITY[level]  (Decision #1: 'none' → 'low')
      cwe         := extract_cwe_from_rule(rules_by_id[ruleId])
      description := truncate_utf8(PREFIX_SAST + message.text)

    Defensive guards (per DECISIONS.md Decision #17):
      * `runs`, `results`, `tool.driver.rules` arrays are coerced to [] when
        missing or non-list.
      * Each `run`, `result`, and `rule` is `isinstance(..., dict)`-checked;
        non-dict entries are skipped with `continue` (deterministic fallback —
        no record is emitted for a malformed input rather than raising).
      * `uri` and `msg` are coerced to strings before concatenation/path
        operations (the prefix-string + non-string concatenation crash
        documented in Checkpoint 2 review finding #1 is fully eliminated).

    Field-emission order is fixed: file, line, severity, cwe, description
    (Python 3.7+ preserves dict insertion order).
    """
    out = []
    if not isinstance(sarif_data, dict):
        return out
    runs = sarif_data.get('runs')
    if not isinstance(runs, list):
        return out
    for run in runs:
        # Decision #17: skip non-dict run entries deterministically.
        if not isinstance(run, dict):
            continue
        # Build ruleId → rule lookup scoped to this run.
        tool = run.get('tool') if isinstance(run.get('tool'), dict) else {}
        driver = tool.get('driver') if isinstance(tool.get('driver'), dict) else {}
        rules = driver.get('rules') if isinstance(driver.get('rules'), list) else []
        rules_by_id = {
            r.get('id', ''): r
            for r in rules
            if isinstance(r, dict)
        }
        results = run.get('results') if isinstance(run.get('results'), list) else []
        for res in results:
            # Decision #17: skip non-dict result entries deterministically.
            if not isinstance(res, dict):
                continue
            level = res.get('level', 'note')
            # Coerce non-string level to its str() form before dict lookup so
            # SARIF_LEVEL_TO_SEVERITY.get never receives an unhashable type.
            if not isinstance(level, str):
                level = str(level) if level is not None else 'note'
            severity = SARIF_LEVEL_TO_SEVERITY.get(level, 'low')
            rule = rules_by_id.get(res.get('ruleId', ''), {})
            cwe = extract_cwe_from_rule(rule)
            locations = res.get('locations') if isinstance(res.get('locations'), list) else []
            loc0 = locations[0] if locations and isinstance(locations[0], dict) else {}
            phys = loc0.get('physicalLocation') if isinstance(loc0.get('physicalLocation'), dict) else {}
            artifact = phys.get('artifactLocation') if isinstance(phys.get('artifactLocation'), dict) else {}
            uri = artifact.get('uri', '') or ''
            region = phys.get('region') if isinstance(phys.get('region'), dict) else {}
            start_line_raw = region.get('startLine', 0)
            try:
                line = int(start_line_raw)
            except (TypeError, ValueError):
                line = 0
            message_obj = res.get('message') if isinstance(res.get('message'), dict) else {}
            msg = message_obj.get('text', '') or ''
            # Coerce msg to string before concatenation so the [snyk-code]
            # prefix can never trigger TypeError on non-string message.text.
            if not isinstance(msg, str):
                msg = str(msg)
            description = truncate_utf8(PREFIX_SAST + msg)
            out.append({
                'file': to_relative_path(uri, repo_root),
                'line': line,
                'severity': severity,
                'cwe': cwe,
                'description': description,
            })
    return out


def normalize_deps(deps_data, repo_root):
    """Convert Snyk Open Source `--json` output to the 5-field record schema.

    For each vulnerabilities[*]:
      file        := displayTargetFile (typically 'go.mod'); to_relative_path applied
      line        := 0  (always — per the user-supplied field-mapping table)
      severity    := vulnerability.severity, validated against DEPS_SEVERITY_ALLOWLIST
                     (Decision #17: unrecognized values fall back to DEPS_SEVERITY_FALLBACK)
      cwe         := extract_cwe_or_cve(identifiers)  (Decision #2: CWE first, CVE fallback)
      description := truncate_utf8(PREFIX_DEPS + title)

    Defensive guards (per DECISIONS.md Decision #17):
      * `deps_data` non-dict → returns [].
      * `vulnerabilities` non-list → treated as empty.
      * Each `v` is `isinstance(..., dict)`-checked; non-dict entries are
        skipped with `continue`.
      * `severity` coerced and lower-cased; values outside the allowlist
        {critical, high, medium, low} fall back to 'low' so output severity
        is always one of the four canonical values.
      * `title` coerced to string before [snyk-deps] concatenation.
    """
    out = []
    if not isinstance(deps_data, dict):
        return out
    target_raw = deps_data.get('displayTargetFile', 'go.mod') or 'go.mod'
    target = to_relative_path(target_raw, repo_root)
    vulnerabilities = deps_data.get('vulnerabilities')
    if not isinstance(vulnerabilities, list):
        return out
    for v in vulnerabilities:
        # Decision #17: skip non-dict vulnerability entries deterministically.
        if not isinstance(v, dict):
            continue
        # Severity: coerce to string, lower-case, and constrain to the
        # four-value allowlist. Unknown values degrade to DEPS_SEVERITY_FALLBACK
        # rather than passing arbitrary strings through to the output.
        raw_severity = v.get('severity', DEPS_SEVERITY_FALLBACK)
        if not isinstance(raw_severity, str):
            raw_severity = str(raw_severity) if raw_severity is not None else DEPS_SEVERITY_FALLBACK
        severity_norm = raw_severity.strip().lower() or DEPS_SEVERITY_FALLBACK
        severity = severity_norm if severity_norm in DEPS_SEVERITY_ALLOWLIST else DEPS_SEVERITY_FALLBACK
        identifiers = v.get('identifiers') if isinstance(v.get('identifiers'), dict) else {}
        cwe = extract_cwe_or_cve(identifiers)
        title = v.get('title', '') or ''
        # Coerce title to string before concatenation so the [snyk-deps]
        # prefix never triggers TypeError on non-string title fields.
        if not isinstance(title, str):
            title = str(title)
        description = truncate_utf8(PREFIX_DEPS + title)
        out.append({
            'file': target,
            'line': 0,
            'severity': severity,
            'cwe': cwe,
            'description': description,
        })
    return out


# ── CLI entry ────────────────────────────────────────────────────────────────

def main(argv=None):
    """CLI entry point. Returns the process exit code.

    See DECISIONS.md (Decision #10) for exit-code interpretation.
    """
    parser = argparse.ArgumentParser(
        prog='normalize-snyk-findings',
        description='Normalize Snyk SAST + dependency scan outputs into a single-line minified JSON array.',
    )
    parser.add_argument('--sarif', required=True,
                        help='Path to results-snyk-code.sarif (input).')
    parser.add_argument('--deps', required=True,
                        help='Path to results-snyk-deps.json (input).')
    parser.add_argument('--out', required=True,
                        help='Path to findings-config-h.json (output).')
    parser.add_argument('--repo-root', default='.',
                        help='Repository root for relative-path resolution.')
    args = parser.parse_args(argv)

    # Phase 1: Read SARIF.
    try:
        with open(args.sarif, 'r', encoding='utf-8') as f:
            sarif_data = json.load(f)
    except FileNotFoundError:
        print(f'error: SARIF input not found: {args.sarif}', file=sys.stderr)
        return 2
    except json.JSONDecodeError as e:
        print(f'error: SARIF input is not valid JSON ({args.sarif}): {e}', file=sys.stderr)
        return 2

    # Phase 2: Read deps JSON.
    try:
        with open(args.deps, 'r', encoding='utf-8') as f:
            deps_data = json.load(f)
    except FileNotFoundError:
        print(f'error: deps input not found: {args.deps}', file=sys.stderr)
        return 2
    except json.JSONDecodeError as e:
        print(f'error: deps input is not valid JSON ({args.deps}): {e}', file=sys.stderr)
        return 2

    # Phase 3 + 4: Normalize each stream.
    sast_records = normalize_sarif(sarif_data, args.repo_root)
    dep_records = normalize_deps(deps_data, args.repo_root)

    # Phase 5: Concatenate (SAST first, deps second — deterministic, no sorting).
    all_records = sast_records + dep_records

    # Phase 6: Sanity check — every record must have the 5 required keys.
    for i, r in enumerate(all_records):
        missing = [k for k in REQUIRED_FIELDS if k not in r]
        if missing:
            print(f'error: record {i} missing fields {missing}', file=sys.stderr)
            return 2

    # Phase 7: Emit single-line minified JSON with one trailing newline so
    # `wc -l` returns 1 (AAP §0.2.3 Critical Directive 4 pass/fail).
    output = json.dumps(all_records, separators=(',', ':'), ensure_ascii=False)
    with open(args.out, 'w', encoding='utf-8') as f:
        f.write(output + '\n')

    # Phase 8: Diagnostic summary to stderr.
    print(f'wrote {len(all_records)} records to {args.out}', file=sys.stderr)
    return 0


if __name__ == '__main__':
    sys.exit(main())
