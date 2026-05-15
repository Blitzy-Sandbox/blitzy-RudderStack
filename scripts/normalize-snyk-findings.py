#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Normalize Snyk SAST + dependency scan outputs into a single-line minified JSON array.

Inputs:
  * results-snyk-code.sarif  - Snyk Code SAST output (SARIF 2.1.0)
  * results-snyk-deps.json   - Snyk Open Source dependency scan output (--json)

Output:
  * findings-config-h.json   - UTF-8, single minified line + one trailing newline
                               (so `cat findings-config-h.json | wc -l` returns 1)

Exit codes:
  * 0 - success
  * 2 - missing or malformed input

Example invocation:
  python3 scripts/normalize-snyk-findings.py --sarif results-snyk-code.sarif \
      --deps results-snyk-deps.json --out findings-config-h.json --repo-root .

See DECISIONS.md at the repository root for rationale on every non-trivial
decision (severity mapping for `none`, CWE/CVE fallback, prefix-inclusive
200-char truncation, path-relativity, exit-code semantics).
"""
import argparse
import json
import os
import re
import sys
from pathlib import Path  # noqa: F401  # Imported per agent_prompt Phase 2 import contract.

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


# ── Helpers ──────────────────────────────────────────────────────────────────

def truncate_utf8(text, max_chars=MAX_DESCRIPTION_CHARS):
    """Normalize whitespace runs to single spaces, strip, and character-truncate.

    See DECISIONS.md (Decision #3) for the prefix-inclusive truncation contract.

    Treats None and empty string identically: returns ''.
    Collapses any run of whitespace (spaces, tabs, newlines, etc.) to a single
    space character, then strips leading/trailing whitespace, then truncates
    to at most max_chars characters.
    """
    if not text:
        return ''
    normalized = re.sub(r'\s+', ' ', text).strip()
    if len(normalized) <= max_chars:
        return normalized
    return normalized[:max_chars]


def to_relative_path(uri, repo_root):
    """Convert an absolute or root-anchored SARIF/Snyk URI to a repo-relative path.

    See DECISIONS.md (Decision #9) for the relpath strategy and fallback
    behavior on cross-filesystem boundaries.

    Strips a leading "file://" scheme if present, then either relativizes
    against the resolved repo_root (when the URI is absolute) or normalizes
    the path (when it is already relative). On any path-resolution error,
    returns the (post-scheme-strip) URI unchanged. NEVER raises.
    """
    if not uri:
        return ''
    if uri.startswith('file://'):
        uri = uri[len('file://'):]
    try:
        abs_root = os.path.abspath(repo_root)
        if os.path.isabs(uri):
            return os.path.relpath(uri, abs_root)
        return os.path.normpath(uri)
    except (ValueError, OSError):
        return uri


def extract_cwe_from_rule(rule):
    """Extract a CWE-<n> identifier from a SARIF rule dict.

    See DECISIONS.md (Decision #8) for the SAST CWE extraction priority order.

    Priority:
      1. rule.properties.cwe[0]  - canonical typed field
      2. rule.properties.tags    - scan for CWE-<n> pattern
      3. UNKNOWN_CWE fallback
    """
    props = (rule or {}).get('properties', {}) or {}
    cwe_list = props.get('cwe') or []
    if cwe_list:
        cwe_id = cwe_list[0]
        return cwe_id if str(cwe_id).startswith('CWE-') else f'CWE-{cwe_id}'
    for tag in props.get('tags', []) or []:
        m = CWE_TAG_PATTERN.search(str(tag))
        if m:
            return m.group(0)
    return UNKNOWN_CWE


def extract_cwe_or_cve(identifiers):
    """Return the first CWE/CVE identifier from a Snyk vulnerability's identifiers map.

    See DECISIONS.md (Decision #2) for the CWE-first, CVE-fallback rationale.

    Priority:
      1. identifiers.CWE[0]
      2. identifiers.CVE[0]
      3. UNKNOWN_CWE
    """
    ids = identifiers or {}
    cwe_list = ids.get('CWE') or []
    if cwe_list:
        return str(cwe_list[0])
    cve_list = ids.get('CVE') or []
    if cve_list:
        return str(cve_list[0])
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

    Defensive `or {}` / `or []` patterns absorb missing fields in real-world
    SARIF inputs. Field-emission order is fixed: file, line, severity, cwe,
    description (Python 3.7+ preserves dict insertion order).
    """
    out = []
    runs = (sarif_data or {}).get('runs', []) or []
    for run in runs:
        # Build ruleId → rule lookup scoped to this run.
        rules = (((run or {}).get('tool') or {}).get('driver') or {}).get('rules', []) or []
        rules_by_id = {r.get('id', ''): r for r in rules if isinstance(r, dict)}
        for res in (run or {}).get('results', []) or []:
            level = (res or {}).get('level', 'note')
            severity = SARIF_LEVEL_TO_SEVERITY.get(level, 'low')
            rule = rules_by_id.get((res or {}).get('ruleId', ''), {}) or {}
            cwe = extract_cwe_from_rule(rule)
            locations = (res or {}).get('locations') or [{}]
            loc0 = locations[0] if locations else {}
            phys = ((loc0 or {}).get('physicalLocation') or {})
            artifact = (phys.get('artifactLocation') or {})
            uri = artifact.get('uri', '') or ''
            region = (phys.get('region') or {})
            start_line_raw = region.get('startLine', 0)
            try:
                line = int(start_line_raw)
            except (TypeError, ValueError):
                line = 0
            msg = ((res or {}).get('message') or {}).get('text', '') or ''
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
      severity    := vulnerability.severity (pass through; critical|high|medium|low)
      cwe         := extract_cwe_or_cve(identifiers)  (Decision #2: CWE first, CVE fallback)
      description := truncate_utf8(PREFIX_DEPS + title)
    """
    out = []
    target_raw = (deps_data or {}).get('displayTargetFile', 'go.mod') or 'go.mod'
    target = to_relative_path(target_raw, repo_root)
    for v in (deps_data or {}).get('vulnerabilities', []) or []:
        severity = (v or {}).get('severity', 'low') or 'low'
        cwe = extract_cwe_or_cve((v or {}).get('identifiers') or {})
        title = (v or {}).get('title', '') or ''
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
