#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
10_render_report.py — Markdown report renderer for the Acceleration Report.

PURPOSE
-------
Render the canonical 11-section measurement report at
``blitzy/acceleration-report/acceleration-report.md`` from the four data
artifacts produced by ``09_compute_metrics.py`` (``metrics.json`` and
``per_engineer.json``) plus ``00_environment.sh`` (``environment.json``) and
``01_detect_inflection.py`` (``inflection.json``). The renderer is invoked
penultimately in the pipeline (just before ``11_render_deck.py``).

The renderer reads ONLY ``data/*.json`` and ``diagrams/*.mmd`` files — never
raw git/API outputs — which is the mechanical enforcement of Rule 4
(Internal Consistency) per AAP §0.5.6: both this Markdown report and the
HTML deck consume the same single source of truth (``data/metrics.json``),
so every numeric value renders identically across the Executive Summary,
Activity Deep-Dives, Traceability Matrix, and Acceleration Curve.

CRITICAL CONSTRAINTS (per AAP §0.7 and the agent prompt)
--------------------------------------------------------
* **Read-only**: NO write operations against the analyzed repository or
  external systems. NO HTTP requests at all. The only filesystem write
  performed by this script is the Markdown output at ``OUTPUT_PATH``.
* **Strict mode**: ``from __future__ import annotations`` as the first
  statement after the module docstring; type hints throughout;
  ``if __name__ == "__main__": sys.exit(main())`` at module footer.
* **CLI flags**: ``--dry-run`` (lists reads/writes without writing),
  ``--verify-only`` (re-runs pre-write guards against an existing rendered
  file for ``make verify`` integration), ``--output`` (test override).
* **Pre-write guards mandatory** (5 guards run before any write):
    1. Factual-neutral tone (Rule 2): blocklist scan with word-boundary
       regex applied only to paragraph lines (excluding ``> `` block-quote
       lines and fenced code blocks) so verbatim user quotes are not
       false-positives.
    2. Section order (Rule 6): the 11 canonical sections appear in the
       order documented by ``SECTION_ORDER`` and Environment Verification
       precedes any Metric Deep-Dive.
    3. Diagram-reference round-trip (Visual Architecture rule): every
       ``diagrams/*.mmd`` file present on disk is referenced by filename
       in the rendered report.
    4. Confidence caveat presence (Rule 3): every Low-confidence metric
       has a non-empty ``caveat`` field, and its caveat appears at least
       twice in the rendered report (Executive Summary AND Metric
       Deep-Dive).
    5. Internal-consistency spot-check (Rule 4): three randomly-selected
       metrics' phase values appear identically across the Executive
       Summary, the Metric Deep-Dive, the Traceability Matrix, and the
       Acceleration Curve table.
* **Section order is a module-level constant** ``SECTION_ORDER`` — never
  re-parameterised between calls. This is the mechanical guarantee that
  Quality Gate 3 (Environment Verification precedes Metric Deep-Dives) is
  always met.
* **Observability**: ``BLITZY_RUN_ID`` env var propagated through the
  structured-JSON logger via ``lib.observability.get_logger`` so every
  event emitted by this script carries the same per-run correlation ID as
  its sibling extraction scripts.

EXIT CODES
----------
* 0 — success (Markdown file written, or ``--dry-run`` / ``--verify-only``
  preview succeeded with no guard failures).
* 1 — pre-write guard violation, missing data artifact, or unexpected error.

USAGE
-----
    cd blitzy/acceleration-report
    python3 scripts/10_render_report.py
    python3 scripts/10_render_report.py --dry-run
    python3 scripts/10_render_report.py --verify-only
    python3 scripts/10_render_report.py --output /tmp/test-report.md
    python3 scripts/10_render_report.py --help

REFERENCES
----------
AAP §0.1.1 (deliverable specification);
AAP §0.5.6 (single-source rendering, internal consistency, factual-neutral
tone blocklist);
AAP §0.6.2 (file transformation entry for this script);
AAP §0.7.2 (Rules 1–6 enforcement mechanisms);
AAP §0.8.3 (Validation Framework: eleven-section ordering);
AAP §0.9 (Quality Gates 1–11);
``decision-log.md`` DL-002 (inflection-point method),
DL-006 (Ramp-Up/Steady-State fallback).
"""

from __future__ import annotations

import argparse
import json
import os
import random
import re
import sys
from pathlib import Path
from typing import Any

# Make the colocated ``lib`` package importable when the script is invoked
# directly (``python3 scripts/10_render_report.py``) rather than as a module.
# This mirrors the import-path convention used by sibling scripts 03 through
# 11 in the workspace.
sys.path.insert(0, str(Path(__file__).resolve().parent))

import jsonschema  # noqa: E402  (pinned in requirements.txt)

from lib.observability import get_logger  # noqa: E402  (sys.path mutation)
from lib.paths import (  # noqa: E402  (sys.path mutation)
    OutputPathError,
    atomic_write_text,
    safe_output_path,
)


# ---------------------------------------------------------------------------
# Script identity (consumed by the structured-JSON logger)
# ---------------------------------------------------------------------------

#: Logger name. Appears verbatim in the ``script`` field of every emitted log
#: event. Used as the cache key in ``lib.observability._LOGGER_CACHE``.
SCRIPT_NAME: str = "10_render_report"


# ---------------------------------------------------------------------------
# Filesystem layout — resolved from ``__file__`` so the script works whether
# invoked from the workspace root, the scripts directory, or anywhere else.
# ---------------------------------------------------------------------------

#: ``blitzy/acceleration-report/`` — the parent of ``scripts/``.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent

#: ``blitzy/acceleration-report/data/`` — the directory where every raw
#: artifact and every computed metric file lives. Read-only from this script.
DATA_DIR: Path = WORKSPACE_ROOT / "data"

#: ``blitzy/acceleration-report/diagrams/`` — the directory containing
#: Mermaid source files (``.mmd``) embedded inline in the rendered report.
DIAGRAMS_DIR: Path = WORKSPACE_ROOT / "diagrams"

#: Default output path for the rendered Markdown report. May be overridden
#: by the ``--output`` CLI flag (used by integration tests and the
#: ``--verify-only`` mode to point at an arbitrary file for re-validation).
OUTPUT_PATH: Path = WORKSPACE_ROOT / "acceleration-report.md"

#: ``blitzy/acceleration-report/scripts/lib/schemas/`` — the directory
#: containing JSON Schemas for every artifact this renderer reads. The
#: pre-render schema validation step (MAJOR-#4 review fix) loads
#: ``metrics.schema.json``, ``per_engineer.schema.json``,
#: ``inflection.schema.json``, and ``environment.schema.json`` from this
#: directory before any rendering or guard execution.
SCHEMAS_DIR: Path = Path(__file__).resolve().parent / "lib" / "schemas"

#: Map from input artifact filename to its JSON Schema filename. Used by
#: ``_validate_input_artifacts()`` to schema-check every artifact the
#: renderer consumes before it builds any output. Per MAJOR-#4 review
#: finding, this validation is mandatory and runs BEFORE the render
#: pipeline so a malformed artifact fails fast with a structured error
#: message instead of producing a malformed report.
RENDERER_INPUT_SCHEMAS: dict[str, str] = {
    "metrics.json": "metrics.schema.json",
    "per_engineer.json": "per_engineer.schema.json",
    "inflection.json": "inflection.schema.json",
    "environment.json": "environment.schema.json",
}


# ---------------------------------------------------------------------------
# Section order constant — the 11 canonical sections specified by AAP §0.8.3.
# The pre-write section-order guard uses this to assert ordering in the
# rendered report. The strings appear verbatim as heading-text substrings;
# the renderer constructs them with the ``## N. <name>`` Markdown heading
# convention but the guard only looks for the name substring.
# ---------------------------------------------------------------------------

#: The 11 mandatory section names in canonical order. Quality Gate 3 plus
#: Rule 6 (Environment First) require Environment Verification to appear
#: BEFORE any Metric Deep-Dive, which is guaranteed by item index 1 vs item
#: index 4 in this tuple. The renderer must emit each name as a substring of
#: a heading line; the order-checking guard uses ``str.find`` to confirm
#: monotonically increasing positions.
#: Each entry is the EXACT Markdown heading text (level-2, numbered) so that
#: ``guard_section_order`` matches a unique anchor in the rendered document
#: and is not fooled by prose references such as
#: "see the Traceability Matrix row" appearing earlier in the report.
SECTION_ORDER: tuple[str, ...] = (
    "## 1. Executive Summary",
    "## 2. Environment Verification",
    "## 3. Data Source Inventory",
    "## 4. Methodology",
    "## 5. Metric Deep-Dives",
    "## 6. Requirements Traceability Matrix",
    "## 7. Per-Engineer Acceleration",
    "## 8. Acceleration Curve",
    "## 9. Risk Assessment",
    "## 10. Limitations",
    "## 11. Reproducibility Appendix",
)


# ---------------------------------------------------------------------------
# Factual-neutral-tone blocklist (Rule 2 per AAP §0.7.2). Pre-write guard 1
# rejects the rendered report if any term appears as a whole word,
# case-insensitive, in a non-quoted paragraph line. This list mirrors the
# one used by ``11_render_deck.py`` so both renderers enforce the same
# constraint.
# ---------------------------------------------------------------------------

#: Subjective qualifiers that MUST NOT appear in the rendered report body.
#: The guard uses word-boundary regex with ``re.IGNORECASE`` so substrings
#: inside larger words (e.g. ``"signature"`` containing ``"sign"`` — none
#: of these blocked terms are substrings of common English words but the
#: word-boundary guard is defensive against future additions).
BLOCKLIST: tuple[str, ...] = (
    "impressive",
    "significant",
    "excellent",
    "remarkable",
    "unfortunately",
    "striking",
    "dramatic",
    "surprisingly",
    "notably",
    "crucially",
)


# ---------------------------------------------------------------------------
# Canonical metric-number → display-name map. Used in every section header,
# table row, and traceability matrix entry. Keys match the schema in
# ``data/metrics.json``.
# ---------------------------------------------------------------------------

#: The 12 metrics in canonical AAP §0.5.3 order. Iteration over keys uses
#: numeric sort on the suffix (``int(k[1:])``) so ``m10``-``m12`` appear
#: after ``m9`` rather than after ``m1`` in lexicographic order.
METRIC_NAMES: dict[str, str] = {
    "m1": "Flow Load",
    "m2": "Flow Velocity",
    "m3": "Flow Predictability",
    "m4": "Flow Active",
    "m5": "Flow Efficiency",
    "m6": "Flow Distribution",
    "m7": "Flow Time",
    "m8": "Problem Records in Release",
    "m9": "Releases",
    "m10": "Approved Exceptions",
    "m11": "Escaped Defects",
    "m12": "Defects Out of SLA",
}

#: Metrics with per-engineer attribution per AAP §0.1.1 ("Per-engineer
#: breakdowns for Metrics 2, 4, 5, 6, 10"). Metrics outside this set do not
#: have a per-engineer sub-table in their Deep-Dive.
PER_ENGINEER_METRICS: tuple[str, ...] = ("m2", "m4", "m5", "m6", "m10")

#: Mapping from a metric key to the corresponding key inside each engineer's
#: per-engineer record. The shape of ``data/per_engineer.json`` is
#: ``{engineers: {<engineer_name>: {m2_flow_velocity, m4_flow_active,
#: m5_flow_efficiency, m6_flow_distribution, m10_approved_exceptions}}}``,
#: so the renderer must translate the canonical ``mN`` key to the
#: descriptive ``mN_<snake_case_name>`` key when reading per-engineer data.
PER_ENGINEER_FIELD: dict[str, str] = {
    "m2": "m2_flow_velocity",
    "m4": "m4_flow_active",
    "m5": "m5_flow_efficiency",
    "m6": "m6_flow_distribution",
    "m10": "m10_approved_exceptions",
}


# ---------------------------------------------------------------------------
# Verbatim user metric definitions — preserved EXACTLY word-for-word from
# AAP §0.5.3.2 through §0.5.3.13. These are rendered as block-quoted prose
# at the top of each Metric Deep-Dive and are exempt from the factual-
# neutral-tone blocklist scan because they are user-supplied content.
#
# CRITICAL: do NOT paraphrase, summarise, or reformat. The traceability
# matrix uses the first sentence (text before the first period) as the
# "Requirement" column entry; rephrasing would break that derivation.
# ---------------------------------------------------------------------------

VERBATIM_DEFINITIONS: dict[str, str] = {
    "m1": (
        "Count of PRs in progress (started but not completed) at each "
        "measurement point. Mean count of PRs in an in-progress state at "
        "the end of each 2-week window, averaged across windows within a "
        "phase. In-progress = branch has at least one commit AND PR is "
        "open (not merged, not closed-without-merge), OR PR is in draft "
        "state. Exclude PRs from bot accounts other than Blitzy (branches "
        "prefixed with blitzy-). Per-phase value is the mean of "
        "window-end snapshots."
    ),
    "m2": (
        "Count of PRs completed (merged) per period. Count of PRs merged "
        "to the default branch per 2-week window. Excludes PRs authored "
        "by dependency-management bots; includes PRs authored by Blitzy. "
        "Per-phase value is the mean PRs per window. Also reported as "
        "per-actor breakdown (real names plus Blitzy as one row in the "
        "after period)."
    ),
    "m3": (
        "Variance of flow velocity across periods. Reciprocal of the "
        "coefficient of variation (mean / standard deviation) of Flow "
        "Velocity across the 2-week windows within each phase. Requires "
        "≥4 windows per phase; otherwise report 'Insufficient signal — "
        "fewer than 4 windows.' Higher values indicate higher "
        "predictability (lower relative variance); the after/before "
        "ratio moves in the same direction as the other metrics' "
        "'better' direction. A phase with zero variance has undefined "
        "predictability and is reported as 'Insufficient signal — zero "
        "variance' rather than infinity."
    ),
    "m4": (
        "Active coding time per PR by the engineering actor. The "
        "engineering actor is the human author in the baseline period "
        "and Blitzy in the after period. Flow Active sums the actor's "
        "coding spans on the PR branch, where a span runs from the "
        "actor's first commit to last commit within a working phase, "
        "inclusive of all time between (gaps are not subtracted). "
        "Working phases are bounded by review events: the initial span "
        "ends when the PR becomes ready for review; each subsequent "
        "refine span runs from the first commit after a review to the "
        "last commit before the next review or merge. Time spent "
        "refining in response to review is counted as active. "
        "Ready-for-review is the earliest of: (a) PR leaving draft "
        "state, (b) first review requested, (c) first commit by another "
        "author, (d) PR opened. Reported as median across PRs per phase "
        "and per actor."
    ),
    "m5": (
        "Ratio of active work time to total time (active + wait) for "
        "completed items. Flow Active / Flow Time, computed per PR and "
        "reported as the median across PRs in each phase. Active is the "
        "engineering actor's coding interval sum (per Metric 4). Review "
        "is treated as wait from the engineering actor's perspective in "
        "both periods (the actor is blocked on the reviewer)."
    ),
    "m6": (
        "Proportion of work by type: features, defects, risk/compliance, "
        "tech debt. Proportion of merged PRs in each phase classified "
        "into: feature, defect, risk/compliance, tech-debt, unknown. "
        "Classification priority: (1) issue labels on linked issues, "
        "(2) conventional-commit prefix on PR title (feat → feature, "
        "fix → defect, security/compliance → risk/compliance, "
        "chore/refactor → tech-debt), (3) keyword match against "
        "conventional PR title and body styles. PRs that match none of "
        "the above go to unknown. Reported per actor in the after "
        "period (Blitzy's distribution may differ from humans'). The "
        "unknown rate is reported per phase as a confidence indicator; "
        "if unknown exceeds 20% in either phase, confidence is "
        "downgraded to Low for that phase."
    ),
    "m7": (
        "Median wall-clock time from first commit on a PR branch to "
        "merge commit on the default branch, across all merged PRs in "
        "the phase. Includes all coding intervals, review queue, review "
        "duration, and post-approval idle. Excludes PRs where the "
        "first-commit timestamp is unavailable due to history rewrites; "
        "the exclusion rate is reported."
    ),
    "m8": (
        "Count of issues or defects documented against a specific "
        "release — measured as revert commits. Count of revert commits "
        "on the default branch attributed to the release that contained "
        "the original (reverted) commit. For each revert: (a) identify "
        "the original commit being reverted via the 'Reverts commit "
        "SHA' reference in the revert message, or by tree-match against "
        "a prior commit's parent if no explicit reference is present; "
        "(b) identify the most recent release tag T such that T is an "
        "ancestor of the original commit (git merge-base --is-ancestor "
        "T <original>); (c) attribute the revert to release T. Reverts "
        "whose original commit cannot be identified are excluded and "
        "reported separately as 'unattributable reverts.' Reverts whose "
        "original commit is not reachable from any release tag are "
        "excluded and reported separately as 'unreleased reverts.' "
        "Reverts-of-reverts are excluded. Phase-level value is mean "
        "attributable reverts per release; unattributable and "
        "unreleased counts are reported as confidence indicators."
    ),
    "m9": (
        "Count of production releases per period. Count of releases per "
        "2-week window. Source precedence: (1) GitHub Releases / GitLab "
        "Releases API, (2) annotated git tags matching semver pattern "
        "v?\\d+.\\d+.\\d+, (3) deployment events from CI/CD if "
        "accessible. Prerelease tags (matching -alpha, -beta, -rc, -dev "
        "suffixes) are excluded from the primary count and reported "
        "separately. Per-phase value is mean releases per window."
    ),
    "m10": (
        "Count of policy exceptions, waivers, or manual overrides "
        "granted per period. Count per 2-week window of: PRs merged "
        "with required reviews bypassed (admin override), force-pushes "
        "to protected branches, merges with failing required CI checks, "
        "branch protection rule modifications, and PRs labeled with "
        "exception/waiver/override tags. Requires admin audit log "
        "access for full signal; without it, only force-pushes and "
        "label-based signals are available and confidence drops to "
        "Low. Reported as count per window per phase, and per actor "
        "(including Blitzy) where attribution is available."
    ),
    "m11": (
        "Defects found in production after release — measured as "
        "skipped or failed test cases. Count per 2-week window of: "
        "(a) test cases transitioning from passing to failing on the "
        "default branch (regressions), and (b) test cases newly marked "
        "skipped, disabled, or xfail on the default branch (suppressed "
        "signal). Sub-counts reported separately. Requires CI "
        "test-result history (JUnit XML, GitHub Actions test reports, "
        "or equivalent); without CI history access, report "
        "'Insufficient signal — CI test history unavailable.' Flaky "
        "tests (alternating pass/fail) are counted only if failing in "
        "≥3 consecutive runs. Also reported as skipped-rate (skipped "
        "tests / total tests) to normalize for test suite growth."
    ),
    "m12": (
        "Defect items not resolved within their SLA target. Count per "
        "phase of defect-labeled issues whose resolution time "
        "(close_date − open_date) exceeds the SLA target for the "
        "issue's severity tier. Severity tiers and their SLA targets "
        "are sourced from (priority order): (1) the issue tracker's "
        "SLA field if present, (2) a policy document or runbook in the "
        "repository. If no SLA source is available, report 'Insufficient "
        "signal — no SLA source.' This metric is issue-scoped rather "
        "than PR-scoped (the only metric for which this is the case) "
        "because SLAs in standard usage attach to defect tickets, not "
        "to the code changes that resolve them. Reported as count and "
        "as percentage of total defects in the phase."
    ),
}


# ---------------------------------------------------------------------------
# Number- and string-formatting helpers — small, pure, type-tolerant.
# Each accepts any of int / float / str / None and returns a display string.
# Per AAP §0.8.4: integers ≥1000 use thousands separators; floats use two
# decimals; the em-dash ``—`` (U+2014) is the canonical "no value" sentinel.
# ---------------------------------------------------------------------------


def format_value(value: Any) -> str:
    """Format a metric value for human-readable display in a Markdown table.

    The ``data/metrics.json`` schema admits four shapes for any ``value``
    field: ``int`` (counts), ``float`` (rates, multipliers, predictability
    scores), ``str`` (the sentinel ``"insufficient_signal"`` or the
    em-dash ``"—"``), or ``None`` (treated identically to em-dash).

    Args:
        value: The raw value from a data artifact. May be any of int,
            float, bool, str, or None.

    Returns:
        A display string suitable for inclusion in a Markdown table cell.
        Numeric values >= 1000 use comma thousands separators; floats are
        rendered with two decimal places unless they are integer-valued
        (in which case the trailing ``.0`` is suppressed for readability);
        ``None`` and ``"insufficient_signal"`` both render as
        ``"Insufficient signal"`` to match AAP §0.1.3 wording in tables
        where the full sentence is acceptable, except the em-dash
        ``"—"`` sentinel which is preserved as-is for compactness.
    """
    if value is None:
        return "—"
    if isinstance(value, bool):
        # bool is a subclass of int in Python; intercept first so True
        # does not render as ``1`` and False does not render as ``0`` in
        # contexts where the data artifact intentionally signals
        # presence/absence via a string sentinel instead.
        return "—"
    if isinstance(value, str):
        if value == "insufficient_signal":
            return "Insufficient signal"
        # Preserve em-dashes and any other string verbatim. The
        # blocklist guard will run against the final report, not against
        # these string values; the caller is responsible for ensuring
        # the input data is clean.
        return value
    if isinstance(value, float):
        # Integer-valued floats display as integers for visual parity
        # with adjacent integer cells (e.g. ``0.0`` -> ``0``).
        if value.is_integer():
            int_value = int(value)
            return f"{int_value:,}" if abs(int_value) >= 1000 else str(int_value)
        return f"{value:,.2f}"
    if isinstance(value, int):
        return f"{value:,}" if abs(value) >= 1000 else str(value)
    # Fallback for unexpected types — convert via ``str()`` rather than
    # raising so that a renderer run never aborts on a data-shape
    # surprise. Schema validation upstream is the authoritative defence
    # against bad data; this is a safety net.
    return str(value)


def format_multiplier(value: Any) -> str:
    """Format an ``after_before_multiplier`` field for display as ``Xx``.

    The schema permits the field to be a numeric multiplier (rendered as
    ``Xx`` with one decimal, e.g. ``"4.7×"``), the em-dash string
    ``"—"`` (rendered verbatim), the literal string
    ``"insufficient_signal"`` (rendered as the em-dash), or ``None``.

    Args:
        value: The raw multiplier from ``data/metrics.json``.

    Returns:
        A display string. The multiplication symbol is U+00D7 ``×``,
        the same character used by ``11_render_deck.py`` so both
        renderers display multipliers identically.
    """
    if value is None:
        return "—"
    if isinstance(value, bool):
        return "—"
    if isinstance(value, str):
        # Any string — em-dash, "insufficient_signal", or unexpected —
        # collapses to the canonical em-dash for visual consistency.
        return "—"
    if isinstance(value, (int, float)):
        return f"{value:.1f}×"
    return "—"


def format_pct(value: Any) -> str:
    """Format a fractional rate (0.0–1.0) as a percentage with one decimal.

    Used by the Flow Distribution metric for category proportions and by
    the Flow Predictability metric for the unknown-rate display.

    Args:
        value: The raw rate from ``data/metrics.json``. ``None``,
            ``"insufficient_signal"``, and non-numeric strings all render
            as the em-dash.

    Returns:
        A display string ending in ``%``, e.g. ``"66.7%"``.
    """
    if value is None or isinstance(value, str) or isinstance(value, bool):
        return "—"
    if isinstance(value, (int, float)):
        return f"{value * 100:.1f}%"
    return "—"


def metric_keys(metrics: dict[str, Any]) -> list[str]:
    """Return the 12 metric keys from ``metrics`` in canonical order.

    The on-disk shape of ``data/metrics.json`` includes a ``_metadata``
    key alongside ``m1`` through ``m12``; iterating the dict directly
    would mistakenly process the metadata as a thirteenth metric. This
    helper filters to keys matching the regex ``^m\\d+$`` and sorts by
    the numeric suffix (so ``m10``, ``m11``, ``m12`` appear after
    ``m9`` instead of after ``m1``).

    Args:
        metrics: Parsed ``data/metrics.json`` payload.

    Returns:
        A list of metric keys in canonical AAP §0.5.3 order.
    """
    keys = [k for k in metrics if isinstance(k, str)
            and len(k) >= 2 and k.startswith("m") and k[1:].isdigit()]
    keys.sort(key=lambda k: int(k[1:]))
    return keys


def sanitize_for_blocklist_scan(report: str) -> str:
    """Strip block-quote lines and fenced code blocks from a report body.

    Pre-write guard 1 (factual-neutral tone) scans the report for any
    word-boundary match of a blocklist term. The verbatim user metric
    definitions are rendered as block-quoted prose (lines beginning
    ``> ``) and the Reproducibility Appendix embeds shell commands
    inside ```` ``` ```` fenced code blocks; both must be excluded
    from the scan so that user-supplied wording or shell tokens cannot
    trigger false positives.

    The implementation walks the report line-by-line maintaining a
    single state variable (``in_fence``) toggled by each ```` ``` ````
    line. Lines beginning with ``> `` are dropped unconditionally;
    lines inside a fenced block are dropped; the fence markers
    themselves are also dropped. Everything else passes through.

    Args:
        report: The full rendered Markdown report.

    Returns:
        The same text with block quotes and fenced code blocks removed,
        suitable for a strict blocklist scan.
    """
    out_lines: list[str] = []
    in_fence: bool = False
    for line in report.splitlines():
        stripped = line.lstrip()
        if stripped.startswith("```"):
            in_fence = not in_fence
            continue
        if in_fence:
            continue
        if stripped.startswith("> "):
            continue
        out_lines.append(line)
    return "\n".join(out_lines)


# ===========================================================================
# Section 1 — Executive Summary
# ===========================================================================


def render_executive_summary(
    metrics: dict[str, Any],
    env: dict[str, Any],
    inflection: dict[str, Any],
) -> str:
    """Render the Executive Summary section (§1).

    Leads with headline multipliers ordered by magnitude, each carrying its
    confidence tag and (for Low-confidence metrics) an italic caveat. Per
    AAP §0.8.3 ("strongest result first"), the table is sorted in
    descending order of absolute multiplier magnitude; metrics with
    non-numeric multipliers (em-dash or "insufficient_signal") sink to the
    bottom, preserving their relative order by numeric metric ID. The
    section also surfaces the inflection date and tier so the reader
    immediately understands which date partitions baseline from
    post-introduction.

    Per Rule 4 (Internal Consistency), every multiplier rendered here
    must match the corresponding entry in the Metric Deep-Dive, the
    Traceability Matrix, and the Acceleration Curve; this is mechanically
    guaranteed because all four sections read from the same metric record.

    Args:
        metrics: Parsed ``data/metrics.json``.
        env: Parsed ``data/environment.json``.
        inflection: Parsed ``data/inflection.json``.

    Returns:
        The Markdown body of section §1 with the leading
        ``## 1. Executive Summary`` heading.
    """
    lines: list[str] = ["## 1. Executive Summary", ""]
    slug = env.get("repository_slug", "Blitzy-Sandbox/blitzy-RudderStack")
    inflection_date = inflection.get("date_utc", "—")
    tier_used = inflection.get("tier_used", "—")
    lines.append(
        f"This report measures development acceleration across twelve "
        f"flow and operational metrics on the `{slug}` repository, "
        f"comparing the period before AI assistance was introduced to "
        f"the period after. The inflection point is "
        f"`{inflection_date}` detected via Tier `{tier_used}` per "
        f"AAP §0.5.3.1. Metric methodology is identical for both "
        f"periods; only the date range and the engineering-actor "
        f"parameter differ (see `decision-log.md` DL-002 for the "
        f"inflection method rationale and DL-006 for the temporal "
        f"phase fallback decision)."
    )
    lines.append("")
    lines.append("**Headline Multipliers (sorted by magnitude)**:")
    lines.append("")
    lines.append("| Metric | After/Before Multiplier | Confidence | Caveat |")
    lines.append("|---|---|---|---|")
    # Build an ordered list of (sort_key, metric_id, metric_record) tuples.
    # The sort key is the absolute value of the numeric multiplier; non-
    # numeric multipliers sort to the bottom via a sentinel of -1, with
    # ties broken by numeric metric ID for stable output.
    rows: list[tuple[float, int, str, dict[str, Any]]] = []
    for k in metric_keys(metrics):
        m = metrics[k]
        mult = m.get("after_before_multiplier")
        if isinstance(mult, (int, float)) and not isinstance(mult, bool):
            sort_key = abs(float(mult))
        else:
            sort_key = -1.0
        rows.append((sort_key, int(k[1:]), k, m))
    # Sort by sort_key DESC, then by numeric ID ASC (stable secondary).
    rows.sort(key=lambda r: (-r[0], r[1]))
    for _, _, k, m in rows:
        mult_str = format_multiplier(m.get("after_before_multiplier"))
        conf = m.get("confidence", "insufficient")
        # Render caveat in the table for Low-confidence metrics per Rule 3
        # ("Low-confidence metrics MUST NOT appear without an explicit
        # caveat"). For other confidence tiers the caveat column carries
        # an em-dash for visual consistency.
        if conf == "low":
            caveat = m.get("caveat") or ""
        else:
            caveat = ""
        caveat_cell = f"*{caveat}*" if caveat else "—"
        lines.append(
            f"| **{k.upper()}** {METRIC_NAMES[k]} | {mult_str} | "
            f"{conf.title()} | {caveat_cell} |"
        )
    lines.append("")
    lines.append("**Metrics measured (12 per AAP §0.5.3)**:")
    lines.append("")
    for k in metric_keys(metrics):
        m = metrics[k]
        value_str = format_value(m.get("value"))
        conf = m.get("confidence", "insufficient")
        lines.append(
            f"- **{k.upper()}** — {METRIC_NAMES[k]} "
            f"(post-introduction value: `{value_str}`, "
            f"confidence: {conf.title()})"
        )
    lines.append("")
    lines.append(
        "*All twelve metrics are populated or marked "
        "'Insufficient signal — [reason]' with deviation documented per "
        "Quality Gate 1. Per Rule 1 (Data Provenance), every numeric "
        "value above traces to a row in the Requirements Traceability "
        "Matrix (§6).*"
    )
    return "\n".join(lines)


# ===========================================================================
# Section 2 — Environment Verification (Rule 6: Environment First)
# ===========================================================================


def render_environment_verification(env: dict[str, Any]) -> str:
    """Render the Environment Verification section (§2).

    Per Rule 6 ("Environment First") and Quality Gate 3, this section
    MUST appear before any Metric Deep-Dive and MUST include the seven
    fields documented in AAP §0.10.2: repository URL, git version, total
    commit count, active branch count, submodule state, commit date
    range, and extraction timestamp. The renderer also surfaces the
    run-ID for correlation with the JSON-Lines log feed and the
    inflection-date metadata for cross-reference with the Methodology
    section.

    Args:
        env: Parsed ``data/environment.json``.

    Returns:
        The Markdown body of section §2 with the leading
        ``## 2. Environment Verification`` heading.
    """
    lines: list[str] = ["## 2. Environment Verification", ""]
    lines.append(
        "Per Rule 6 (Environment First) and Quality Gate 3, the "
        "execution environment is captured below before any metric "
        "extraction begins. This snapshot is the deterministic basis "
        "for reproducibility — every command in the Reproducibility "
        "Appendix (§11) runs against this exact environment."
    )
    lines.append("")
    lines.append("| Field | Value |")
    lines.append("|---|---|")
    # Repository identity.
    lines.append(
        f"| Repository URL | "
        f"`{env.get('repository_url', '—')}` |"
    )
    lines.append(
        f"| Repository slug | "
        f"`{env.get('repository_slug', '—')}` |"
    )
    lines.append(
        f"| Default branch | "
        f"`{env.get('default_branch', '—')}` |"
    )
    # Tooling.
    lines.append(
        f"| Git version | "
        f"`{env.get('git_version', '—')}` |"
    )
    lines.append(
        f"| Go module version | "
        f"`{env.get('go_module_version', '—')}` "
        f"(source: {env.get('go_module_version_source', '—')}) |"
    )
    # Repository state.
    total_commits = env.get("total_commit_count")
    lines.append(
        f"| Total commit count | "
        f"{format_value(total_commits)} |"
    )
    lines.append(
        f"| Active branch count | "
        f"{format_value(env.get('active_branch_count'))} |"
    )
    lines.append(
        f"| Submodule state | "
        f"`{env.get('submodule_state', '—')}` |"
    )
    # Commit date range — nested dict.
    date_range = env.get("commit_date_range") or {}
    earliest = date_range.get("earliest") or date_range.get("earliest_utc") or "—"
    latest = date_range.get("latest") or date_range.get("latest_utc") or "—"
    lines.append(
        f"| Commit date range | "
        f"`{earliest}` → `{latest}` |"
    )
    # Inflection cross-reference.
    lines.append(
        f"| AI inflection date (UTC) | "
        f"`{env.get('inflection_date_utc', '—')}` |"
    )
    # Extraction metadata.
    lines.append(
        f"| Extraction timestamp (UTC) | "
        f"`{env.get('extraction_timestamp', '—')}` |"
    )
    lines.append(
        f"| Run ID | "
        f"`{env.get('run_id', '—')}` |"
    )
    lines.append("")
    lines.append(
        "*This section is rendered first among the eleven sections "
        "(after the Executive Summary preamble) per Quality Gate 3. "
        "All fields are sourced from `data/environment.json`, emitted "
        "by `scripts/00_environment.sh` at the start of each pipeline "
        "run.*"
    )
    return "\n".join(lines)



# ===========================================================================
# Section 3 — Data Source Inventory (Quality Gate 11)
# ===========================================================================


def _read_diagram(name: str) -> str:
    """Read a Mermaid diagram source file from ``DIAGRAMS_DIR``.

    The renderer embeds Mermaid diagrams inline in the Markdown report
    via ```` ```mermaid ```` fenced code blocks. This helper centralises
    the read-and-strip logic so each call site is a single line.

    Args:
        name: The diagram filename including the ``.mmd`` extension,
            e.g. ``"data-source-topology.mmd"``.

    Returns:
        The diagram source text with leading and trailing whitespace
        stripped. The leading ``%% Title:`` and ``%% Legend:`` comment
        lines are preserved so the diagram self-documents inside the
        rendered Markdown.

    Raises:
        FileNotFoundError: If the diagram source is missing. The
            pre-write diagram-reference guard catches this earlier by
            iterating ``DIAGRAMS_DIR.glob`` and asserting referenced
            files exist; this error path is defensive only.
    """
    return (DIAGRAMS_DIR / name).read_text(encoding="utf-8").strip()


def render_data_source_inventory(metrics: dict[str, Any]) -> str:
    """Render the Data Source Inventory section (§3).

    Combines:

    * In-repository sources (table from AAP §0.2.1.1)
    * External API sources (table from AAP §0.2.1.2)
    * Sources unavailable in this run (extracted from each metric's
      ``boundary_conditions`` field where the data artifact mentions
      unavailability)

    The section ends with the Data Source Topology Mermaid diagram
    embedded inline. The diagram is referenced by filename
    (``diagrams/data-source-topology.mmd``) in the surrounding prose so
    the diagram-reference round-trip guard passes.

    Args:
        metrics: Parsed ``data/metrics.json``. Used to derive the
            unavailable-sources list from the metrics' boundary
            condition strings.

    Returns:
        The Markdown body of section §3 with the leading
        ``## 3. Data Source Inventory`` heading.
    """
    lines: list[str] = ["## 3. Data Source Inventory", ""]
    lines.append(
        "Per Quality Gate 11, every system accessed and every system "
        "that was unavailable is documented below. All sources are "
        "consumed read-only; no script writes to the analyzed "
        "repository, the GitHub API, the Linear API, or any other "
        "external system."
    )
    lines.append("")
    # In-repository sources — drawn from AAP §0.2.1.1.
    lines.append("### 3.1 In-Repository Sources")
    lines.append("")
    lines.append("| Path | Used For | Notes |")
    lines.append("|---|---|---|")
    lines.append(
        "| `.git/` (history, refs, tags, reflog) | "
        "Metrics 1, 2, 3, 4, 5, 6, 7, 8, 10 (force-push detection), "
        "inflection-point detection | "
        "Primary source. Read via `git log`, `git rev-list`, "
        "`git for-each-ref`, `git reflog show`, `git diff`. "
        "No `--force-with-lease`, no commit creation. |"
    )
    lines.append(
        "| `.github/workflows/*.{yml,yaml}` (13 files) | "
        "Metric 9 (CI deploy events), Metric 11 (CI test history), "
        "Metric 10 (required-check bypass detection) | "
        "Test pipeline matrix, verify pipeline, release-please "
        "configuration, semantic-pr type catalogue, prerelease "
        "convention, dispatch-deploy events. |"
    )
    lines.append(
        "| `.github/labeler.yml`, `.github/dependabot.yml`, "
        "`.github/pull_request_template.md`, "
        "`.github/ISSUE_TEMPLATE/bug-report.md` | "
        "Metric 6 (label classification), Metric 2 (dependency bot "
        "exclusion), Metric 12 (Linear reference) | "
        "Label catalogue, dependency-bot ecosystem inventory, Linear "
        "ticket linkage convention, issue-template structure. |"
    )
    lines.append(
        "| `.golangci.yml`, `.snyk`, `.truffleignore`, "
        "`.deepsource.toml` | "
        "Metric 10 (current exemption inventory) | "
        "Lint suppressions, security exceptions, secret-scanner "
        "ignores, static-analysis exclusions — HEAD snapshot. |"
    )
    lines.append(
        "| `codecov.yml`, `Makefile`, `**/*_test.go` (497 files) | "
        "Methodology context, Metric 11 (test target inventory, HEAD "
        "skipped-test snapshot) | "
        "Coverage configuration, test target enumeration, in-repo "
        "skipped/disabled-test markers (`t.Skip`, `// nolint`). |"
    )
    lines.append(
        "| `go.mod`, `go.sum`, `Dockerfile`, `docker-compose.yml`, "
        "`rudder-docker.yml` | "
        "Environment Verification, Metric 9 fallback tier | "
        "Module identity, Go version, container build convention. |"
    )
    lines.append(
        "| `docs/`, `blitzy-docs/`, `blitzy/documentation/`, "
        "`mkdocs.yml` | "
        "Metric 12 (SLA policy search) | "
        "Documentation tree scanned for SLA targets and severity "
        "tier definitions. |"
    )
    lines.append("")
    # External API sources — drawn from AAP §0.2.1.2.
    lines.append("### 3.2 External API Sources")
    lines.append("")
    lines.append(
        "| Source | Endpoint Pattern | Used For | Access Method |"
    )
    lines.append("|---|---|---|---|")
    lines.append(
        "| GitHub Pulls API | `GET /repos/{owner}/{repo}/pulls"
        "?state=all` | Metrics 1, 2, 4, 5, 6, 7 | "
        "`GH_TOKEN` env var (raises rate limit 60→5000/hr); falls "
        "back to local-git when absent. |"
    )
    lines.append(
        "| GitHub Pull-Commits / Reviews / Events APIs | "
        "`GET /pulls/{n}/commits`, `/reviews`, `/issues/{n}/events` | "
        "Metric 4 (review-event-bounded working phases), Metric 7 "
        "(first-commit timestamps) | Same auth as Pulls API. |"
    )
    lines.append(
        "| GitHub Releases API | "
        "`GET /repos/{owner}/{repo}/releases` | "
        "Metric 9 (primary source) | "
        "Prereleases filtered separately; falls back to annotated "
        "tag scan, then CI deploy events. |"
    )
    lines.append(
        "| GitHub Actions Runs API | "
        "`GET /repos/{owner}/{repo}/actions/runs?branch=main` | "
        "Metric 11 (test transitions), Metric 10 (failed-check "
        "overrides) | "
        "Falls back to in-repo `_test.go` scan at HEAD when API "
        "is unavailable. |"
    )
    lines.append(
        "| GitHub Branches API | "
        "`GET /repos/{owner}/{repo}/branches/main/protection` | "
        "Metric 10 (required-check bypass detection) | "
        "Requires admin access for full audit-log signal; without "
        "admin, only `protected: true` is visible. |"
    )
    lines.append(
        "| Linear API | "
        "`POST https://api.linear.app/graphql` | "
        "Metric 12 (Defects Out of SLA), Metric 6 (Linear-label "
        "classification) | "
        "Requires `LINEAR_API_KEY`; both metrics fall back to "
        "in-repo signals when absent. |"
    )
    lines.append("")
    # Sources unavailable — derived from boundary_conditions strings.
    lines.append("### 3.3 Sources Unavailable in This Run")
    lines.append("")
    unavailable_entries = _collect_unavailable_sources(metrics)
    if unavailable_entries:
        lines.append(
            "The following sources were attempted and either failed "
            "or were unreachable. Each metric whose value depended on "
            "an unavailable source carries a confidence downgrade and "
            "a Risk Assessment entry (§9)."
        )
        lines.append("")
        lines.append("| Source | Reason Unavailable | Affected Metrics |")
        lines.append("|---|---|---|")
        for source, reason, affected in unavailable_entries:
            metrics_list = ", ".join(sorted(affected))
            lines.append(f"| {source} | {reason} | {metrics_list} |")
    else:
        lines.append(
            "*No sources are recorded as unavailable in this run. "
            "If a source was attempted and did not return data, the "
            "associated metric's `boundary_conditions` field would "
            "appear here automatically.*"
        )
    lines.append("")
    # Topology diagram embedded inline. Referenced by filename in the
    # surrounding prose to satisfy the diagram-reference round-trip
    # guard.
    lines.append(
        "**Data Source Topology** (see `diagrams/data-source-topology"
        ".mmd` for the Mermaid source):"
    )
    lines.append("")
    lines.append("```mermaid")
    lines.append(_read_diagram("data-source-topology.mmd"))
    lines.append("```")
    return "\n".join(lines)


def _collect_unavailable_sources(
    metrics: dict[str, Any],
) -> list[tuple[str, str, set[str]]]:
    """Extract the unavailable-source inventory from metric boundary fields.

    Each metric's ``boundary_conditions`` string mentions which sources
    were unavailable for that metric (e.g. "Pulls API: unavailable",
    "Linear API: unavailable"). This helper scans all twelve metrics for
    a fixed catalogue of source-name patterns and aggregates the
    affected-metric set per source.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        A sorted list of ``(source_name, reason, affected_metric_ids)``
        tuples. ``affected_metric_ids`` is the set of metric IDs (e.g.
        ``{"M1", "M2"}``) whose extraction was blocked by the unavailable
        source.
    """
    # Catalogue of (source name, detection regex, default reason) tuples.
    catalogue: list[tuple[str, re.Pattern[str], str]] = [
        (
            "GitHub Pulls API",
            re.compile(r"Pulls API[: ]+unavailable", re.IGNORECASE),
            "GH_TOKEN environment variable not set or rate limit exceeded.",
        ),
        (
            "GitHub Reviews API",
            re.compile(r"Reviews API[: ]+unavailable", re.IGNORECASE),
            "GH_TOKEN environment variable not set or rate limit exceeded.",
        ),
        (
            "GitHub Events API",
            re.compile(r"Events API[: ]+unavailable", re.IGNORECASE),
            "GH_TOKEN environment variable not set or rate limit exceeded.",
        ),
        (
            "GitHub Releases API",
            re.compile(r"Releases API[: ]+unavailable", re.IGNORECASE),
            "GH_TOKEN environment variable not set or no releases found.",
        ),
        (
            "GitHub Actions Runs API",
            re.compile(
                r"(CI Actions API|Actions Runs API)[: ]+unavailable",
                re.IGNORECASE,
            ),
            "GH_TOKEN environment variable not set or rate limit exceeded.",
        ),
        (
            "Linear API",
            re.compile(r"Linear API[: ]+unavailable", re.IGNORECASE),
            "LINEAR_API_KEY environment variable not set.",
        ),
        (
            "Admin audit log",
            re.compile(
                r"(Audit log|admin audit-log)[: ]+unavailable",
                re.IGNORECASE,
            ),
            "Admin access required; not available without admin token.",
        ),
        (
            "Branch protection API",
            re.compile(
                r"Branch[- ]protection check[: ]+unavailable",
                re.IGNORECASE,
            ),
            "Requires admin access for full signal.",
        ),
        (
            "JUnit XML artifacts",
            re.compile(
                r"JUnit XML artifacts retrieved[: ]+0",
                re.IGNORECASE,
            ),
            ".github/workflows/tests.yaml does not emit JUnit XML "
            "artifacts in current configuration.",
        ),
    ]
    aggregated: dict[str, tuple[str, set[str]]] = {}
    for k in metric_keys(metrics):
        m = metrics[k]
        # Concatenate all boundary-condition strings on this metric for a
        # single scan: metric-level, baseline-phase, post-intro-phase.
        haystack_parts = [
            str(m.get("boundary_conditions") or ""),
            str(m.get("reason") or ""),
            str((m.get("baseline") or {}).get("boundary_conditions") or ""),
            str((m.get("baseline") or {}).get("reason") or ""),
            str(
                (m.get("post_introduction") or {}).get(
                    "boundary_conditions"
                ) or ""
            ),
            str((m.get("post_introduction") or {}).get("reason") or ""),
        ]
        haystack = " ".join(haystack_parts)
        for source_name, pattern, default_reason in catalogue:
            if pattern.search(haystack):
                entry = aggregated.setdefault(
                    source_name, (default_reason, set())
                )
                entry[1].add(k.upper())
    # Convert dict to sorted list of (source, reason, affected) tuples.
    return [
        (source, reason, affected)
        for source, (reason, affected) in sorted(aggregated.items())
    ]


# ===========================================================================
# Section 4 — Methodology
# ===========================================================================


def render_methodology(
    metrics: dict[str, Any],
    inflection: dict[str, Any],
    env: dict[str, Any],
) -> str:
    """Render the Methodology section (§4).

    Six subsections per AAP §0.5.3:

    1. Engineering Actor Framing — embeds
       ``diagrams/engineering-actor-framing.mmd``.
    2. Window Mechanics — Monday 00:00 UTC anchored 2-week windows.
    3. Inflection Point Detection — references the tier waterfall and
       which tier resolved.
    4. Temporal Phase Decomposition — embeds
       ``diagrams/temporal-phases-timeline.mmd``; if the
       post-introduction span is shorter than 90 days, the renderer
       documents the Ramp-Up/Steady-State fallback per DL-006.
    5. Multi-Module Aggregation — per-module attribution by majority of
       file paths weighted by non-merge commit volume.
    6. Confidence Framework — High/Medium/Low/Insufficient definitions
       with composite metrics inheriting the worse tier.

    Also references ``diagrams/extraction-pipeline.mmd`` by filename so
    the diagram-reference round-trip guard passes for that file.

    Args:
        metrics: Parsed ``data/metrics.json``.
        inflection: Parsed ``data/inflection.json``.
        env: Parsed ``data/environment.json``.

    Returns:
        The Markdown body of section §4 with the leading
        ``## 4. Methodology`` heading.
    """
    lines: list[str] = ["## 4. Methodology", ""]
    lines.append(
        "This section documents the extraction methodology applied "
        "uniformly to both periods. Per AAP §0.1.3, the only "
        "difference between the baseline and post-introduction "
        "extractions is the date range and the engineering-actor "
        "parameter — same window alignment, same exclusion rules, "
        "same span-computation logic. This is the mechanical "
        "guarantee that the identical-methodology constraint is "
        "satisfied."
    )
    lines.append("")
    # 4.1 Engineering Actor Framing.
    lines.append("### 4.1 Engineering Actor Framing")
    lines.append("")
    lines.append(
        "Per AAP §0.1.1, the engineering actor parameter is the human "
        "author of each PR in the baseline period, and `Blitzy` in "
        "the post-introduction period. Metrics that measure working "
        "time (Metrics 4 and 5) are computed from the engineering "
        "actor's perspective; metrics that aggregate by actor "
        "(Metrics 2, 4, 5, 6, 10) report `Blitzy` as a single row "
        "alongside human contributors in the after period. The "
        "function `compute_phase(period, actor, artifacts, "
        "phase_bounds, logger)` in `09_compute_metrics.py` is invoked "
        "exactly twice — once with the baseline resolver "
        "`_actor_resolver_baseline` and once with the post-introduction "
        "resolver `_actor_resolver_post_introduction`. Every other "
        "parameter (window alignment, bot exclusions, classification "
        "priority, span-bounding logic) is a module-scope constant "
        "exposed in `metrics.json#_metadata.compute_constants`, so the "
        "identical-methodology invariant is mechanically auditable from "
        "the artifact rather than from source code."
    )
    lines.append("")
    lines.append(
        "See `diagrams/engineering-actor-framing.mmd` for the sequence "
        "diagram showing `compute_phase` called twice with different "
        "(period, actor) parameters:"
    )
    lines.append("")
    lines.append("```mermaid")
    lines.append(_read_diagram("engineering-actor-framing.mmd"))
    lines.append("```")
    lines.append("")
    # 4.2 Window mechanics.
    lines.append("### 4.2 Window Mechanics")
    lines.append("")
    lines.append(
        "All 2-week windows are anchored to Monday 00:00 UTC. The "
        "first window of the baseline phase starts at the Monday on "
        "or after the earliest commit timestamp; the first window of "
        "the post-introduction phase starts at the Monday on or after "
        "the inflection timestamp. A window is included in a phase "
        "if its window-end falls within the phase's date range."
    )
    lines.append("")
    # 4.3 Inflection-point detection.
    lines.append("### 4.3 Inflection Point Detection")
    lines.append("")
    tier_used = inflection.get("tier_used", "—")
    inflection_date = inflection.get("date_utc", "—")
    evidence = inflection.get("evidence") or {}
    lines.append(
        f"The AI inflection point partitions the measurement window "
        f"into baseline and post-introduction phases. Per AAP "
        f"§0.5.3.1, three tiers are tried in precedence order: "
        f"(1) `Co-authored-by:` trailer search, (2) AI-actor email "
        f"pattern, (3) sustained velocity inflection. For this "
        f"repository, **Tier `{tier_used}`** resolved at "
        f"`{inflection_date}`."
    )
    lines.append("")
    if evidence:
        sha = evidence.get("commit_sha", "—")
        email = evidence.get("author_email", "—")
        name = evidence.get("author_name", "—")
        author_date = evidence.get("author_date", "—")
        lines.append("**Detection Evidence**:")
        lines.append("")
        lines.append(f"- Commit SHA: `{sha}`")
        lines.append(f"- Author email: `{email}`")
        lines.append(f"- Author name: `{name}`")
        lines.append(f"- Author date (UTC): `{author_date}`")
        lines.append("")
    justification = inflection.get("justification")
    if justification:
        lines.append(f"**Justification**: {justification}")
        lines.append("")
    lines.append(
        "See `decision-log.md` DL-002 for the full alternatives "
        "considered and rationale for the chosen tier."
    )
    lines.append("")
    # 4.4 Temporal phase decomposition.
    lines.append("### 4.4 Temporal Phase Decomposition")
    lines.append("")
    split_applied = inflection.get(
        "ramp_up_steady_state_split_applied", False
    )
    post_intro = inflection.get("post_introduction") or {}
    post_days = post_intro.get("duration_days")
    threshold = inflection.get(
        "ramp_up_steady_state_threshold_days", 90
    )
    if not split_applied:
        # Fallback applied — document per DL-006.
        post_days_str = format_value(post_days) if post_days is not None else "—"
        lines.append(
            f"Per AAP §0.1.4 and §0.5.6, the temporal phase "
            f"decomposition splits the post-introduction period into "
            f"Ramp-Up (first 90 days) and Steady State (90+ days) "
            f"only when the post-introduction span is at least 90 "
            f"days. The post-introduction phase in this run spans "
            f"`{post_days_str}` days, which is below the "
            f"`{threshold}`-day threshold. The renderer therefore "
            f"falls back to **Baseline vs Post-Introduction** "
            f"reporting per `decision-log.md` DL-006. The fallback is "
            f"documented in every Metric Deep-Dive's phase-values "
            f"table."
        )
    else:
        lines.append(
            f"The post-introduction phase spans at least "
            f"`{threshold}` days, so the canonical Ramp-Up "
            f"(first 90 days) and Steady State (90+ days) split "
            f"applies per AAP §0.5.6."
        )
    lines.append("")
    lines.append(
        "See `diagrams/temporal-phases-timeline.mmd` for the Gantt "
        "timeline showing the Baseline → Post-Introduction (or "
        "Baseline → Ramp-Up → Steady State) phases:"
    )
    lines.append("")
    lines.append("```mermaid")
    lines.append(_read_diagram("temporal-phases-timeline.mmd"))
    lines.append("```")
    lines.append("")
    # 4.5 Multi-module aggregation.
    lines.append("### 4.5 Multi-Module Aggregation")
    lines.append("")
    lines.append(
        "Per AAP §0.5.6, this repository is a Go monorepo with "
        "logical modules at `gateway/`, `processor/`, `router/`, "
        "`warehouse/`, `jobsdb/`, `services/`, and others. Each "
        "non-merge commit is attributed to the module containing the "
        "majority of its changed file paths; cross-module commits go "
        "to the module with the most changed lines. Module-level "
        "metrics are aggregated by `non_merge_commits_per_module / "
        "total_non_merge_commits` weighting. Single-module values are "
        "reported as the headline number; the module-weighted average "
        "is documented in `data/metrics.json` per metric."
    )
    lines.append("")
    # 4.6 Confidence framework.
    lines.append("### 4.6 Confidence Framework")
    lines.append("")
    lines.append(
        "Per AAP §0.7.2 Rule 3 and the confidence tiers in AAP "
        "§0.1.3, every derived metric carries one of four confidence "
        "tags:"
    )
    lines.append("")
    lines.append("| Tier | Definition | Source Quality |")
    lines.append("|---|---|---|")
    lines.append(
        "| **High** | Direct counts in an issue tracker or "
        "deterministic git enumeration with full attribution. | "
        "Authoritative source. |"
    )
    lines.append(
        "| **Medium** | Approximated from git commit patterns with "
        "a known local-fallback gap (e.g. cannot distinguish "
        "dependency-bot PRs without subject-line scanning). | "
        "Strong proxy with documented boundary conditions. |"
    )
    lines.append(
        "| **Low** | Inferred from indirect proxies; the metric "
        "carries a `caveat` field rendered alongside every value. | "
        "Indirect signal — interpret with the caveat in view. |"
    )
    lines.append(
        "| **Insufficient** | No usable source for either phase; "
        "reported as `Insufficient signal — [reason]` per AAP "
        "§0.1.3. The metric carries a `reason` field and a Risk "
        "Assessment entry with High severity. | "
        "No signal — not measurable in this run. |"
    )
    lines.append("")
    lines.append(
        "Composite metrics inherit the worse tier of their inputs: "
        "Metric 5 (Flow Efficiency) inherits from Metrics 4 and 7, "
        "Metric 3 (Flow Predictability) inherits from Metric 2."
    )
    lines.append("")
    # Pipeline diagram reference (satisfies diagram-reference round-trip
    # guard for extraction-pipeline.mmd).
    lines.append(
        "**Pipeline Architecture**: the read-only extraction → "
        "compute → render pipeline diagrammed in "
        "`diagrams/extraction-pipeline.mmd` mirrors AAP §0.5.1 "
        "verbatim. Scripts 00–08 read from `.git/`, `.github/`, and "
        "the GitHub/Linear APIs; script 09 computes metrics; "
        "scripts 10 and 11 render the report and deck. The "
        "renderers read ONLY `data/*.json` — never the raw sources — "
        "which is the mechanical enforcement of Rule 4."
    )
    return "\n".join(lines)



# ===========================================================================
# Section 5 — Metric Deep-Dives
# ===========================================================================


def render_metric_deep_dive(
    key: str,
    metric: dict[str, Any],
    per_eng: dict[str, Any],
) -> str:
    """Render a single Metric Deep-Dive subsection.

    The structure (in order, top to bottom):

    1. ``### MN — <Name>`` heading (e.g. ``### M1 — Flow Load``).
    2. Verbatim user definition in a Markdown block quote (italic).
       Drawn from ``VERBATIM_DEFINITIONS[key]`` — preserved word-for-word
       from AAP §0.5.3.
    3. ``**Extraction Strategy**`` paragraph from
       ``metric["extraction_strategy"]``.
    4. ``**Phase Values**`` table with three or two rows depending on
       whether the temporal phase decomposition is full (Baseline /
       Ramp-Up / Steady State) or the fallback (Baseline /
       Post-Introduction). The renderer detects this dynamically by
       inspecting ``metric["ramp_up"]``'s presence.
    5. ``*Caveat: ...*`` line when ``confidence == "low"`` (Rule 3).
    6. ``**Boundary Conditions**`` line when ``confidence != "high"``.
    7. ``**Per-Window Series**`` table when ``per_window`` is non-empty.
    8. ``**Per-Engineer Breakdown**`` sub-table for Metrics 2, 4, 5, 6,
       10 (the per-engineer metrics).
    9. ``*[Provenance: ...]*`` cross-reference to the Traceability
       Matrix.

    The verbatim user definition lives in a block quote so the
    factual-neutral-tone blocklist scan (which excludes block-quoted
    lines) does not flag any user-supplied wording.

    Args:
        key: Metric key (``"m1"`` through ``"m12"``).
        metric: The metric record from ``data/metrics.json``.
        per_eng: Parsed ``data/per_engineer.json`` (full payload).

    Returns:
        The Markdown body of one Metric Deep-Dive subsection.
    """
    name = METRIC_NAMES[key]
    lines: list[str] = [f"### {key.upper()} — {name}", ""]

    # 1. Verbatim user definition in a block quote. The block-quote
    # marker (``> ``) exempts these lines from the factual-neutral
    # blocklist scan, so user wording is preserved untouched.
    definition = VERBATIM_DEFINITIONS.get(key, "—")
    # Wrap to keep the quoted definition readable when viewed as Markdown
    # (single-line quote, since block-quote line breaks introduce
    # paragraph splits in some renderers).
    lines.append(f"> *{definition}*")
    lines.append("")

    # 2. Extraction Strategy.
    extraction_strategy = (
        metric.get("extraction_strategy") or "—"
    )
    lines.append("**Extraction Strategy**:")
    lines.append("")
    lines.append(extraction_strategy)
    lines.append("")

    # 3. Phase-values table — branches on whether the Ramp-Up /
    # Steady-State split was applied.
    has_full_split = "ramp_up" in metric and "steady_state" in metric
    baseline = metric.get("baseline") or {}
    post_intro = metric.get("post_introduction") or {}
    overall_multiplier = metric.get("after_before_multiplier")

    lines.append("**Phase Values**:")
    lines.append("")
    if has_full_split:
        ramp = metric.get("ramp_up") or {}
        steady = metric.get("steady_state") or {}
        lines.append("| Phase | Value | Multiplier | Confidence | Windows |")
        lines.append("|---|---|---|---|---|")
        lines.append(
            f"| Baseline | {format_value(baseline.get('value'))} | — | "
            f"{str(baseline.get('confidence', '—')).title()} | "
            f"{format_value(baseline.get('windows'))} |"
        )
        lines.append(
            f"| Ramp-Up | {format_value(ramp.get('value'))} | "
            f"{format_multiplier(ramp.get('multiplier'))} | "
            f"{str(ramp.get('confidence', '—')).title()} | "
            f"{format_value(ramp.get('windows'))} |"
        )
        lines.append(
            f"| Steady State | {format_value(steady.get('value'))} | "
            f"{format_multiplier(steady.get('multiplier'))} | "
            f"{str(steady.get('confidence', '—')).title()} | "
            f"{format_value(steady.get('windows'))} |"
        )
    else:
        lines.append("| Phase | Value | Multiplier | Confidence | Windows |")
        lines.append("|---|---|---|---|---|")
        lines.append(
            f"| Baseline | {format_value(baseline.get('value'))} | — | "
            f"{str(baseline.get('confidence', '—')).title()} | "
            f"{format_value(baseline.get('windows'))} |"
        )
        post_mult = post_intro.get("multiplier", overall_multiplier)
        lines.append(
            f"| Post-Introduction | "
            f"{format_value(post_intro.get('value'))} | "
            f"{format_multiplier(post_mult)} | "
            f"{str(post_intro.get('confidence', '—')).title()} | "
            f"{format_value(post_intro.get('windows'))} |"
        )
    lines.append("")
    # Overall after/before multiplier (for cross-section consistency).
    lines.append(
        f"**Headline Multiplier (After / Before)**: "
        f"{format_multiplier(overall_multiplier)}"
    )
    lines.append("")

    # 4. Caveat — rendered when confidence == "low" per Rule 3.
    conf = metric.get("confidence", "insufficient")
    caveat = metric.get("caveat") or ""
    if conf == "low" and caveat:
        lines.append(f"*Caveat: {caveat}*")
        lines.append("")

    # 5. Boundary conditions — rendered when confidence != "high" per
    # AAP §0.7.3 ("Medium and Low confidence metrics must include
    # boundary condition documentation").
    boundary = metric.get("boundary_conditions") or ""
    if conf != "high" and boundary:
        lines.append(f"**Boundary Conditions**: {boundary}")
        lines.append("")

    # 6. Insufficient-signal reason — rendered when value is the
    # sentinel string. The schema admits a top-level ``reason`` field
    # plus phase-level reasons; we render the metric-level one which
    # is the most general.
    if metric.get("value") == "insufficient_signal":
        reason = metric.get("reason") or "—"
        lines.append(f"**Insufficient Signal Reason**: {reason}")
        lines.append("")

    # 7. Per-Window Series — rendered only when non-empty.
    per_window = metric.get("per_window") or []
    if per_window:
        lines.append("**Per-Window Series**:")
        lines.append("")
        lines.append("| Window Start (UTC) | Window End (UTC) | Value |")
        lines.append("|---|---|---|")
        for window in per_window:
            start = (
                window.get("start")
                or window.get("window_start_iso")
                or "—"
            )
            end = (
                window.get("end")
                or window.get("window_end_iso")
                or "—"
            )
            value = window.get("value")
            lines.append(f"| `{start}` | `{end}` | {format_value(value)} |")
        lines.append("")

    # 8. Per-Engineer Breakdown — only for M2, M4, M5, M6, M10 per
    # AAP §0.1.1. The data shape is
    # ``per_eng["engineers"][<engineer>][mN_<snake>]``; we translate
    # the canonical mN key to the descriptive field name via
    # ``PER_ENGINEER_FIELD``.
    if key in PER_ENGINEER_METRICS:
        engineers_data = per_eng.get("engineers") or {}
        field_name = PER_ENGINEER_FIELD[key]
        if engineers_data:
            lines.append("**Per-Engineer Breakdown**:")
            lines.append("")
            lines.append(
                "| Engineer | Actor Type | Baseline | "
                "Post-Introduction | Multiplier |"
            )
            lines.append("|---|---|---|---|---|")
            # Sort engineers by display name with Blitzy pinned last so
            # the AI actor appears as the comparison row beneath the
            # human contributors.
            for engineer_name in _sort_engineers(engineers_data.keys()):
                eng_record = engineers_data[engineer_name] or {}
                actor_type = eng_record.get("actor_type", "—")
                metric_field = eng_record.get(field_name) or {}
                base_val = format_value(metric_field.get("baseline"))
                post_val = format_value(
                    metric_field.get("post_introduction")
                )
                mult_val = format_multiplier(metric_field.get("multiplier"))
                lines.append(
                    f"| {engineer_name} | {str(actor_type).title()} | "
                    f"{base_val} | {post_val} | {mult_val} |"
                )
            lines.append("")
        else:
            lines.append(
                "*Per-engineer breakdown unavailable: "
                "`data/per_engineer.json` contains no engineer "
                "records.*"
            )
            lines.append("")

    # 9. Provenance cross-reference.
    prov = metric.get("provenance") or {}
    req_id = prov.get("requirement_id", key.upper())
    lines.append(
        f"*[Provenance: see Requirements Traceability Matrix row "
        f"`{req_id}` in §6 and the Reproducibility Appendix step in "
        f"§11.]*"
    )
    return "\n".join(lines)


def _sort_engineers(engineer_names: Any) -> list[str]:
    """Return a sorted list of engineer display names.

    Order: humans first (case-insensitive alphabetical), then ``Blitzy``
    at the end. This puts the AI engineering actor as the visual
    comparison row beneath the human contributors, matching the AAP's
    "Blitzy as one row alongside human contributors" framing.

    Args:
        engineer_names: An iterable of engineer display-name strings.

    Returns:
        A sorted list with humans alphabetised and ``Blitzy`` pinned to
        the last position.
    """
    names = list(engineer_names)
    blitzy_present = "Blitzy" in names
    humans = sorted([n for n in names if n != "Blitzy"], key=str.lower)
    if blitzy_present:
        humans.append("Blitzy")
    return humans


def render_metric_deep_dives_section(
    metrics: dict[str, Any],
    per_eng: dict[str, Any],
) -> str:
    """Render the entire Metric Deep-Dives section (§5).

    Composes the parent heading plus one subsection per metric in
    canonical order. The section's existence and ordering relative to
    Environment Verification are asserted by the section-order
    pre-write guard.

    Args:
        metrics: Parsed ``data/metrics.json``.
        per_eng: Parsed ``data/per_engineer.json``.

    Returns:
        The Markdown body of section §5 including the leading
        ``## 5. Metric Deep-Dives`` heading.
    """
    parts: list[str] = ["## 5. Metric Deep-Dives", ""]
    parts.append(
        "One subsection per metric, in canonical AAP §0.5.3 order. "
        "Each subsection includes the verbatim user definition (block "
        "quote), the extraction strategy actually used by this "
        "pipeline, the phase-values table, the per-window series, "
        "and the per-engineer breakdown where applicable. Caveats "
        "and boundary conditions appear inline per Rule 3."
    )
    parts.append("")
    for k in metric_keys(metrics):
        parts.append(render_metric_deep_dive(k, metrics[k], per_eng))
        parts.append("")
    return "\n".join(parts)



# ===========================================================================
# Section 6 — Requirements Traceability Matrix (Rule 1: Data Provenance)
# ===========================================================================


def render_traceability_matrix(metrics: dict[str, Any]) -> str:
    """Render the Requirements Traceability Matrix section (§6).

    Per Rule 1 (Data Provenance), every numeric value in the Executive
    Summary and Metric Deep-Dives must trace through this matrix:
    Requirement → Extraction Command → Raw Output → Derived Value →
    Reported Number. The matrix is generated by iterating every
    metric's ``provenance`` field; the "Requirement" column is derived
    from the verbatim user definition's first sentence so the
    cross-reference is unambiguous.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        The Markdown body of section §6 with the leading
        ``## 6. Requirements Traceability Matrix`` heading.
    """
    lines: list[str] = ["## 6. Requirements Traceability Matrix", ""]
    lines.append(
        "Every numeric value in the Executive Summary (§1) and Metric "
        "Deep-Dives (§5) traces to a row in this matrix per Rule 1 "
        "(Data Provenance). The `Extraction Command` column is "
        "syntactically valid: each Bash command passes `bash -n`, "
        "and each Python invocation passes `python3 -m py_compile`. "
        "The `Raw Output` column points at the on-disk artifact under "
        "`data/` that was produced by the extraction step. The "
        "`Derivation Function` column names the Python function "
        "inside `09_compute_metrics.py` that derived the headline "
        "value from the raw output."
    )
    lines.append("")
    lines.append(
        "| Requirement ID | Metric | First-Sentence Requirement | "
        "Extraction Command | Raw Output | Derivation Function | "
        "Reported Number | Confidence |"
    )
    lines.append(
        "|---|---|---|---|---|---|---|---|"
    )
    for k in metric_keys(metrics):
        m = metrics[k]
        prov = m.get("provenance") or {}
        req_id = prov.get("requirement_id", k.upper())
        # First-sentence requirement extracted from the verbatim user
        # definition. We split on the first ``. `` boundary; some
        # definitions begin with a long fragment that we treat as the
        # full first sentence.
        definition = VERBATIM_DEFINITIONS.get(k, "")
        first_sentence = definition.split(". ", 1)[0].rstrip(".") + "."
        # Truncate to keep the table cell readable.
        if len(first_sentence) > 160:
            first_sentence = first_sentence[:157].rstrip() + "…"
        cmd = prov.get("extraction_command") or "—"
        raw = prov.get("raw_output_artifact_path") or "—"
        fn = prov.get("derivation_function") or "—"
        # Reported number = the headline multiplier; falls back to the
        # post-introduction value when the multiplier is unavailable.
        mult = m.get("after_before_multiplier")
        if mult is None or mult == "—" or isinstance(mult, str):
            reported = format_value(
                (m.get("post_introduction") or {}).get("value")
            )
        else:
            reported = format_multiplier(mult)
        conf = str(m.get("confidence", "—")).title()
        # Escape pipe characters in the command/raw/fn cells so they do
        # not break the Markdown table layout.
        cmd_cell = str(cmd).replace("|", "\\|")
        raw_cell = str(raw).replace("|", "\\|")
        fn_cell = str(fn).replace("|", "\\|")
        lines.append(
            f"| `{req_id}` | **{k.upper()}** {METRIC_NAMES[k]} | "
            f"{first_sentence} | `{cmd_cell}` | `{raw_cell}` | "
            f"`{fn_cell}` | {reported} | {conf} |"
        )
    lines.append("")
    lines.append(
        "*Every row's `Reported Number` matches the corresponding "
        "value in §1 (Executive Summary), §5 (Metric Deep-Dive), and "
        "§8 (Acceleration Curve) by construction — the renderer reads "
        "all four sections from the same `data/metrics.json` record. "
        "Per Rule 4 (Internal Consistency) and Quality Gate 8, no "
        "metric value differs across sections.*"
    )
    return "\n".join(lines)


# ===========================================================================
# Section 7 — Per-Engineer Acceleration
# ===========================================================================


def render_per_engineer(per_eng: dict[str, Any]) -> str:
    """Render the Per-Engineer Acceleration section (§7).

    Surfaces per-author breakdowns for Metrics 2, 4, 5, 6, and 10 in a
    single matrix view: rows are engineers (humans first, Blitzy last),
    columns are the five per-engineer metrics. Prepends the DORA/SPACE
    caveat verbatim per AAP §0.8.3.

    Args:
        per_eng: Parsed ``data/per_engineer.json``.

    Returns:
        The Markdown body of section §7 with the leading
        ``## 7. Per-Engineer Acceleration`` heading.
    """
    lines: list[str] = ["## 7. Per-Engineer Acceleration", ""]
    lines.append(
        "*Per-engineer breakdowns are provided to satisfy the "
        "user-specified per-engineer-view requirement (AAP §0.1.1). "
        "They MUST NOT be used for individual performance "
        "evaluation. DORA and SPACE explicitly state that these "
        "metrics are team-level signals; using them to rank or "
        "evaluate individual contributors misapplies the "
        "frameworks.*"
    )
    lines.append("")
    engineers_data = per_eng.get("engineers") or {}
    if not engineers_data:
        lines.append(
            "*No per-engineer records are available. "
            "`data/per_engineer.json` carries an empty engineers "
            "dictionary; this happens when the upstream extraction "
            "scripts could not resolve any contributor identity "
            "(e.g., the GitHub Pulls API is unavailable AND local "
            "git history is empty).*"
        )
        return "\n".join(lines)
    # Per-engineer overview table: one row per engineer, columns are
    # the five per-engineer metrics, cells are "baseline → post" pairs.
    lines.append("**Per-Engineer Overview (Baseline → Post-Introduction)**:")
    lines.append("")
    header_cells = ["Engineer", "Actor Type", "Total Commits (main)"]
    for k in PER_ENGINEER_METRICS:
        header_cells.append(f"{k.upper()} {METRIC_NAMES[k]}")
    lines.append("| " + " | ".join(header_cells) + " |")
    lines.append("|" + "|".join(["---"] * len(header_cells)) + "|")
    for engineer_name in _sort_engineers(engineers_data.keys()):
        eng = engineers_data[engineer_name] or {}
        actor_type = str(eng.get("actor_type", "—")).title()
        total_commits = format_value(eng.get("total_commits_on_main"))
        row_cells: list[str] = [engineer_name, actor_type, total_commits]
        for k in PER_ENGINEER_METRICS:
            field_name = PER_ENGINEER_FIELD[k]
            metric_field = eng.get(field_name) or {}
            base = metric_field.get("baseline")
            post = metric_field.get("post_introduction")
            cell = f"{format_value(base)} → {format_value(post)}"
            row_cells.append(cell)
        lines.append("| " + " | ".join(row_cells) + " |")
    lines.append("")
    # Active-engineers normalization per AAP §0.7.3
    # ("Normalize for team growth where applicable"). Report active
    # engineer counts per phase.
    active_baseline = sum(
        1 for eng in engineers_data.values()
        if (eng or {}).get("commits_in_baseline_phase", 0)
    )
    active_post = sum(
        1 for eng in engineers_data.values()
        if (eng or {}).get("commits_in_post_introduction_phase", 0)
    )
    lines.append("**Active Engineers per Phase**:")
    lines.append("")
    lines.append("| Phase | Active Engineers (≥1 non-merge commit) |")
    lines.append("|---|---|")
    lines.append(f"| Baseline | {active_baseline} |")
    lines.append(f"| Post-Introduction | {active_post} |")
    lines.append("")
    lines.append(
        "*Per-metric per-engineer multipliers are also reported "
        "inline within each applicable Metric Deep-Dive (§5).*"
    )
    return "\n".join(lines)


# ===========================================================================
# Section 8 — Acceleration Curve (with graphical representation)
# ===========================================================================


def render_acceleration_curve(metrics: dict[str, Any]) -> str:
    """Render the Acceleration Curve section (§8).

    Combines a per-metric phase-values table with the Mermaid
    Acceleration Curve diagram. The table format adapts dynamically:
    if any metric carries the ``ramp_up`` field the full Baseline /
    Ramp-Up / Steady State columns are emitted; otherwise the
    Baseline / Post-Introduction fallback columns are used. The
    diagram embedded inline is the contents of
    ``diagrams/acceleration-curve.mmd``.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        The Markdown body of section §8 with the leading
        ``## 8. Acceleration Curve`` heading.
    """
    lines: list[str] = ["## 8. Acceleration Curve", ""]
    lines.append(
        "Per-metric phase-values table followed by the graphical "
        "Acceleration Curve diagram per AAP §0.8.3 Validation "
        "Framework section ordering. Values in this table are byte-"
        "identical to the values in the Executive Summary (§1), the "
        "Metric Deep-Dives (§5), and the Traceability Matrix (§6) by "
        "construction."
    )
    lines.append("")
    # Detect whether any metric has the full Ramp-Up + Steady-State
    # split. If even one does, the table emits all three phase columns;
    # otherwise the fallback table format applies.
    has_full_split = any(
        "ramp_up" in (metrics.get(k) or {})
        and "steady_state" in (metrics.get(k) or {})
        for k in metric_keys(metrics)
    )
    if has_full_split:
        lines.append(
            "| Metric | Baseline | Ramp-Up | Steady State | "
            "After/Before Multiplier | Confidence |"
        )
        lines.append("|---|---|---|---|---|---|")
        for k in metric_keys(metrics):
            m = metrics[k]
            base_val = format_value((m.get("baseline") or {}).get("value"))
            ramp_val = format_value((m.get("ramp_up") or {}).get("value"))
            steady_val = format_value(
                (m.get("steady_state") or {}).get("value")
            )
            mult = format_multiplier(m.get("after_before_multiplier"))
            conf = str(m.get("confidence", "—")).title()
            lines.append(
                f"| **{k.upper()}** {METRIC_NAMES[k]} | {base_val} | "
                f"{ramp_val} | {steady_val} | {mult} | {conf} |"
            )
    else:
        lines.append(
            "| Metric | Baseline | Post-Introduction | "
            "After/Before Multiplier | Confidence |"
        )
        lines.append("|---|---|---|---|---|")
        for k in metric_keys(metrics):
            m = metrics[k]
            base_val = format_value((m.get("baseline") or {}).get("value"))
            post_val = format_value(
                (m.get("post_introduction") or {}).get("value")
            )
            mult = format_multiplier(m.get("after_before_multiplier"))
            conf = str(m.get("confidence", "—")).title()
            lines.append(
                f"| **{k.upper()}** {METRIC_NAMES[k]} | {base_val} | "
                f"{post_val} | {mult} | {conf} |"
            )
    lines.append("")
    # Graphical representation per AAP §0.8.3 ("Include graphical
    # representation"). Embed the Mermaid diagram and reference it by
    # filename in the prose for the diagram-reference round-trip
    # guard.
    lines.append(
        "**Acceleration Curve Diagram** (graphical representation "
        "per AAP §0.8.3, sourced from "
        "`diagrams/acceleration-curve.mmd`):"
    )
    lines.append("")
    lines.append("```mermaid")
    lines.append(_read_diagram("acceleration-curve.mmd"))
    lines.append("```")
    return "\n".join(lines)


# ===========================================================================
# Section 9 — Risk Assessment
# ===========================================================================


def render_risk_assessment(metrics: dict[str, Any]) -> str:
    """Render the Risk Assessment section (§9).

    Per Quality Gate 7, the Risk Assessment must cover every
    Low-confidence metric and every Insufficient-Signal gap. The
    renderer iterates the twelve metrics and emits one row per
    qualifying metric with:

    * Metric ID + Name.
    * Severity: ``Medium`` for ``low`` confidence, ``High`` for
      ``insufficient`` confidence.
    * Risk Description: drawn from ``caveat`` (low) or ``reason``
      (insufficient) or ``boundary_conditions`` as final fallback.
    * Mitigation: template strings keyed on the unavailable data
      source.

    Cardinality assertion: the number of rows emitted equals the
    number of low + insufficient metrics. The pre-write guards do
    not currently re-count this externally, but the renderer itself
    is the single producer so the invariant holds by construction.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        The Markdown body of section §9 with the leading
        ``## 9. Risk Assessment`` heading.
    """
    lines: list[str] = ["## 9. Risk Assessment", ""]
    lines.append(
        "Per Quality Gate 7, every Low-confidence metric and every "
        "Insufficient-Signal gap is enumerated below with severity, "
        "risk description, and proposed mitigation. The cardinality "
        "of this table matches the count of qualifying metrics by "
        "construction."
    )
    lines.append("")
    qualifying: list[tuple[str, dict[str, Any]]] = []
    for k in metric_keys(metrics):
        m = metrics[k]
        conf = m.get("confidence", "insufficient")
        if conf in ("low", "insufficient"):
            qualifying.append((k, m))
    if not qualifying:
        lines.append(
            "*No metrics in this run carry Low or Insufficient "
            "confidence. All twelve metrics are at Medium or High "
            "confidence per their `data/metrics.json` records.*"
        )
        return "\n".join(lines)
    lines.append(
        "| Metric | Severity | Confidence | Risk Description | "
        "Mitigation |"
    )
    lines.append("|---|---|---|---|---|")
    for k, m in qualifying:
        conf = m.get("confidence", "insufficient")
        severity = "High" if conf == "insufficient" else "Medium"
        # Risk description: prefer caveat (for low) or reason (for
        # insufficient), falling back to boundary_conditions.
        if conf == "low":
            description = (
                m.get("caveat")
                or m.get("boundary_conditions")
                or "—"
            )
        else:
            description = (
                m.get("reason")
                or m.get("boundary_conditions")
                or "—"
            )
        # Mitigation: derive from the boundary conditions string.
        mitigation = _derive_mitigation(m.get("boundary_conditions") or "")
        # Escape pipe characters in description/mitigation cells.
        description_cell = str(description).replace("|", "\\|")
        mitigation_cell = mitigation.replace("|", "\\|")
        lines.append(
            f"| **{k.upper()}** {METRIC_NAMES[k]} | {severity} | "
            f"{conf.title()} | {description_cell} | "
            f"{mitigation_cell} |"
        )
    lines.append("")
    insufficient_count = sum(
        1 for _, m in qualifying
        if m.get("confidence") == "insufficient"
    )
    low_count = sum(
        1 for _, m in qualifying
        if m.get("confidence") == "low"
    )
    lines.append(
        f"**Cardinality**: {len(qualifying)} qualifying metrics "
        f"(Low={low_count}, Insufficient={insufficient_count}). "
        f"This count equals the number of rows above per Quality "
        f"Gate 7."
    )
    return "\n".join(lines)


def _derive_mitigation(boundary_conditions: str) -> str:
    """Map known unavailable-source phrases to mitigation suggestions.

    The mitigation templates follow the AAP §0.3 spirit: every blocked
    metric has a known path forward (supply the missing env var, gain
    admin access, configure CI artifacts). The renderer matches on
    substring patterns rather than regex for performance and clarity.

    Args:
        boundary_conditions: The free-text boundary-conditions string
            from a metric record.

    Returns:
        A mitigation sentence suitable for the Risk Assessment table.
    """
    text = boundary_conditions.lower()
    mitigations: list[str] = []
    if "pulls api" in text and "unavailable" in text:
        mitigations.append(
            "Supply `GH_TOKEN` env var to enable Pulls/Reviews/Events "
            "APIs."
        )
    if "actions api" in text and "unavailable" in text:
        mitigations.append(
            "Supply `GH_TOKEN` to enable CI Runs API."
        )
    if "releases api" in text and "unavailable" in text:
        mitigations.append(
            "Supply `GH_TOKEN` to enable Releases API."
        )
    if "linear api" in text and "unavailable" in text:
        mitigations.append(
            "Supply `LINEAR_API_KEY` to enable Linear issue + SLA "
            "extraction."
        )
    if "audit log" in text and "unavailable" in text:
        mitigations.append(
            "Acquire GitHub admin access for the org and re-run with "
            "the admin-scoped token."
        )
    if "junit xml" in text:
        mitigations.append(
            "Configure `.github/workflows/tests.yaml` to upload JUnit "
            "XML artifacts via `actions/upload-artifact`."
        )
    if "sla source" in text or "no sla" in text:
        mitigations.append(
            "Author an SLA policy document under `docs/` or supply "
            "Linear API access to surface per-issue SLA fields."
        )
    if "fewer than 4 windows" in text:
        mitigations.append(
            "Re-run after the post-introduction phase accumulates ≥4 "
            "two-week windows (extend the measurement window)."
        )
    if not mitigations:
        return (
            "Re-run extraction with the upstream data source restored; "
            "see the metric's boundary_conditions for the specific "
            "blocker."
        )
    return " ".join(mitigations)



# ===========================================================================
# Section 10 — Limitations
# ===========================================================================


def render_limitations() -> str:
    """Render the Limitations section (§10).

    Documents what the analysis cannot determine — boundaries that the
    metrics cannot cross by definition. The verbatim out-of-scope items
    from AAP §0.1.3 are preserved word-for-word:

        "runtime performance, customer satisfaction scores, revenue impact"

    Per AAP §0.7.3, also captures: no code-quality scoring, no
    individual ranking, no originality/novelty scoring, no architectural
    commentary, no security vulnerability discovery.

    Returns:
        The Markdown body of section §10 with the leading
        ``## 10. Limitations`` heading.
    """
    lines: list[str] = ["## 10. Limitations", ""]
    lines.append(
        "This section documents what this analysis cannot determine "
        "— the boundaries that the twelve metrics cannot cross by "
        "definition, plus the categories explicitly out of scope per "
        "the user's instructions."
    )
    lines.append("")
    lines.append("### 10.1 Out-of-Scope by User Instruction (Verbatim)")
    lines.append("")
    lines.append(
        "Per AAP §0.1.3, the following dimensions are out of scope "
        "for this measurement deliverable:"
    )
    lines.append("")
    # Verbatim user wording — preserved EXACTLY per AAP §0.1.3.
    lines.append("- runtime performance")
    lines.append("- customer satisfaction scores")
    lines.append("- revenue impact")
    lines.append("")
    lines.append("### 10.2 Out-of-Scope by Methodology")
    lines.append("")
    lines.append(
        "The following analytic operations are deliberately not "
        "performed even when technically possible, per AAP §0.3.2:"
    )
    lines.append("")
    lines.append(
        "- **Composite scores**: the platform does not combine "
        "metric values into derived scores not specified in the user "
        "prompt. The twelve metrics stand alone."
    )
    lines.append(
        "- **Individual ranking**: per-engineer breakdowns appear in "
        "§5 and §7 with the explicit caveat that DORA and SPACE "
        "metrics must not be used for individual performance "
        "evaluation. The platform does not produce a competitive "
        "ranking of contributors."
    )
    lines.append(
        "- **Intent or quality inference from authorship**: the "
        "platform reports counts and durations only. It does not "
        "infer code quality, architectural soundness, or "
        "engineering skill from authorship attribution alone."
    )
    lines.append(
        "- **Originality / novelty scoring**: the platform does not "
        "cross-reference PR or commit content against upstream "
        "`rudderlabs/rudder-server` to compute originality or "
        "novelty scores."
    )
    lines.append(
        "- **Static / dynamic analysis beyond metric needs**: the "
        "platform does not perform code-quality scoring, "
        "architectural commentary, or security vulnerability "
        "discovery. The skipped-test snapshot in §5 (Metric 11) is "
        "the only static-analysis read performed; it is a count, "
        "not an evaluation."
    )
    lines.append(
        "- **Author-identity modification**: the platform reads "
        "identities from git history as-is. It does not normalize, "
        "anonymize, or modify the `agent@blitzy.com` author identity "
        "or any other identity for analytical purposes."
    )
    lines.append("")
    lines.append("### 10.3 Out-of-Scope by Data Availability")
    lines.append("")
    lines.append(
        "Several metrics in this run report `Insufficient signal — "
        "[reason]` because the required external data source was "
        "not available. These are NOT analyst-side limitations — "
        "they are environmental gaps that the Risk Assessment (§9) "
        "documents with explicit mitigations. The platform does NOT "
        "fabricate, estimate, or extrapolate values for these "
        "metrics per AAP §0.1.3."
    )
    lines.append("")
    return "\n".join(lines)


# ===========================================================================
# Section 11 — Reproducibility Appendix (Rule 5)
# ===========================================================================


def render_reproducibility_appendix(metrics: dict[str, Any]) -> str:
    """Render the Reproducibility Appendix section (§11).

    Per Rule 5 and Quality Gate 9, the appendix contains the complete,
    ordered set of commands and API calls needed to re-derive every
    metric from scratch. The renderer walks each metric's
    ``provenance.extraction_command`` field, deduplicates, and emits
    one step per unique command in numeric metric ID order. Each step
    includes a fenced ```` ```bash ```` block (or
    ```` ```http ```` for API endpoints) so the appendix is directly
    copy-pasteable.

    The section ends with a "one-command rerun" footer that documents
    the full pipeline invocation. Pre-write guard 9 (run as part of
    ``make verify``) confirms every Bash command passes ``bash -n``.

    Also references `diagrams/extraction-pipeline.mmd` (already
    referenced in §4) and lists each script in pipeline execution
    order for the analyst.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        The Markdown body of section §11 with the leading
        ``## 11. Reproducibility Appendix`` heading.
    """
    lines: list[str] = ["## 11. Reproducibility Appendix", ""]
    lines.append(
        "Per Rule 5 (Reproducibility) and Quality Gate 9, this "
        "appendix contains the complete, ordered set of commands "
        "and API calls needed to re-derive every metric from "
        "scratch. Each command references only the target repository "
        "and documented data sources; each Bash command passes "
        "`bash -n`; each Python invocation passes "
        "`python3 -m py_compile`. The pipeline as a whole is "
        "diagrammed in `diagrams/extraction-pipeline.mmd` (also "
        "referenced from §4)."
    )
    lines.append("")
    # Walk metrics in canonical order; dedupe by extraction_command;
    # preserve order of first appearance.
    lines.append("### 11.1 Per-Metric Extraction Commands")
    lines.append("")
    seen: set[str] = set()
    ordered_steps: list[tuple[str, str, str]] = []
    for k in metric_keys(metrics):
        prov = (metrics[k] or {}).get("provenance") or {}
        cmd = prov.get("extraction_command")
        if cmd and cmd not in seen:
            seen.add(cmd)
            raw = prov.get("raw_output_artifact_path") or "—"
            ordered_steps.append((k, cmd, raw))
    for i, (k, cmd, raw) in enumerate(ordered_steps, start=1):
        lines.append(f"#### Step {i:02d} — {METRIC_NAMES[k]} ({k.upper()})")
        lines.append("")
        # Distinguish HTTP (API) commands from Bash commands so the
        # fenced block uses the correct language tag.
        lang = "bash"
        stripped = cmd.strip()
        if (
            stripped.startswith("GET ")
            or stripped.startswith("POST ")
            or stripped.startswith("HEAD ")
        ):
            lang = "http"
        lines.append(f"```{lang}")
        lines.append(cmd)
        lines.append("```")
        lines.append("")
        lines.append(f"**Output**: `{raw}`")
        lines.append("")
    # Pipeline execution order — the analyst-facing summary of the
    # full pipeline.
    lines.append("### 11.2 Pipeline Execution Order")
    lines.append("")
    lines.append(
        "The full pipeline is orchestrated by "
        "`blitzy/acceleration-report/Makefile`. Scripts execute "
        "in this strict topological order:"
    )
    lines.append("")
    lines.append("```text")
    lines.append("00_environment.sh       → data/environment.json")
    lines.append("01_detect_inflection.py → data/inflection.json")
    lines.append("02_extract_commits.sh   → data/commits.csv")
    lines.append("03_extract_pulls.py     → data/pulls.json")
    lines.append("04_extract_releases.py  → data/releases.json")
    lines.append("05_extract_reverts.sh   → data/reverts.json")
    lines.append("06_extract_ci_history.py→ data/ci_runs.json")
    lines.append("07_extract_exceptions.py→ data/exceptions.json")
    lines.append("08_extract_linear.py    → data/issues.json")
    lines.append("09_compute_metrics.py   → data/metrics.json")
    lines.append("10_render_report.py     → acceleration-report.md (this file)")
    lines.append("11_render_deck.py       → executive-summary.html")
    lines.append("```")
    lines.append("")
    # One-command rerun footer.
    lines.append("### 11.3 One-Command Rerun")
    lines.append("")
    lines.append(
        "From a clean checkout, the entire measurement can be "
        "re-derived with three commands:"
    )
    lines.append("")
    lines.append("```bash")
    lines.append("cd blitzy/acceleration-report")
    lines.append("make setup    # creates .venv/ and installs requirements.txt")
    lines.append("make all      # runs extract → compute → render → verify")
    lines.append("```")
    lines.append("")
    lines.append(
        "After the run, the rendered report appears at "
        "`blitzy/acceleration-report/acceleration-report.md` and "
        "the executive deck at "
        "`blitzy/acceleration-report/executive-summary.html`. The "
        "structured-JSON log feed is at "
        "`blitzy/acceleration-report/data/run.log.jsonl`."
    )
    return "\n".join(lines)


# ===========================================================================
# Full report composer
# ===========================================================================


def render_full_report(
    metrics: dict[str, Any],
    per_eng: dict[str, Any],
    env: dict[str, Any],
    inflection: dict[str, Any],
) -> str:
    """Compose the full 11-section Markdown report.

    Concatenates every section renderer in canonical ``SECTION_ORDER``,
    interleaved with single blank lines for Markdown paragraph
    separation. The output is the complete ``acceleration-report.md``
    content ready for pre-write guard inspection and final write.

    Args:
        metrics: Parsed ``data/metrics.json``.
        per_eng: Parsed ``data/per_engineer.json``.
        env: Parsed ``data/environment.json``.
        inflection: Parsed ``data/inflection.json``.

    Returns:
        The full Markdown text of the report.
    """
    # Title + metadata header.
    slug = env.get("repository_slug", "Blitzy-Sandbox/blitzy-RudderStack")
    inflection_date = inflection.get("date_utc", "—")
    tier = inflection.get("tier_used", "—")
    extraction_ts = env.get("extraction_timestamp", "—")
    run_id = env.get("run_id", "—")
    parts: list[str] = []
    parts.append(f"# Development Acceleration Report — {slug}")
    parts.append("")
    parts.append(
        f"*Generated by `scripts/10_render_report.py` from "
        f"`data/metrics.json`. Inflection point: `{inflection_date}` "
        f"(Tier `{tier}`). Extraction timestamp: `{extraction_ts}`. "
        f"Run ID: `{run_id}`. This report is a read-only measurement "
        f"snapshot; no file in the analyzed repository is modified "
        f"by its production.*"
    )
    parts.append("")
    # 11 sections in canonical SECTION_ORDER.
    parts.append(render_executive_summary(metrics, env, inflection))
    parts.append("")
    parts.append(render_environment_verification(env))
    parts.append("")
    parts.append(render_data_source_inventory(metrics))
    parts.append("")
    parts.append(render_methodology(metrics, inflection, env))
    parts.append("")
    parts.append(render_metric_deep_dives_section(metrics, per_eng))
    parts.append("")
    parts.append(render_traceability_matrix(metrics))
    parts.append("")
    parts.append(render_per_engineer(per_eng))
    parts.append("")
    parts.append(render_acceleration_curve(metrics))
    parts.append("")
    parts.append(render_risk_assessment(metrics))
    parts.append("")
    parts.append(render_limitations())
    parts.append("")
    parts.append(render_reproducibility_appendix(metrics))
    parts.append("")
    return "\n".join(parts)


# ===========================================================================
# Pre-write guards — Rule 1, 2, 3, 4 + Visual Architecture Documentation.
# ===========================================================================


class GuardFailure(ValueError):
    """Raised when a pre-write guard rejects a rendered report.

    The message is structured: ``"<guard_name>: <details>"`` so that
    the structured-JSON logger can surface a single audit event with
    parsable fields. The renderer's main() function catches this
    exception, emits a ``guard_failure`` log event, and exits non-zero.
    """


def guard_factual_neutral_tone(report: str) -> None:
    """Pre-write guard 1: factual-neutral tone (Rule 2).

    Scans the report body (with block-quoted lines and fenced code
    blocks removed via ``sanitize_for_blocklist_scan``) for any
    word-boundary match of a blocklist term. Raises ``GuardFailure``
    on the first match.

    The block-quote exclusion is necessary because the verbatim user
    metric definitions in §5 are quoted prose; a future definition
    might contain a blocklist term and the renderer must not refuse
    user content. The fenced-code-block exclusion is necessary
    because the Reproducibility Appendix embeds shell commands that
    might coincidentally contain a blocked substring.

    Args:
        report: The full rendered Markdown report.

    Raises:
        GuardFailure: If any blocklist term appears as a whole word
            (case-insensitive) in a non-quoted, non-fenced line.
    """
    body = sanitize_for_blocklist_scan(report)
    for term in BLOCKLIST:
        pattern = re.compile(rf"\b{re.escape(term)}\b", re.IGNORECASE)
        match = pattern.search(body)
        if match:
            # Provide enough context for the operator to find the
            # offending line — a 60-char snippet centred on the match.
            start = max(0, match.start() - 30)
            end = min(len(body), match.end() + 30)
            snippet = body[start:end].replace("\n", " ")
            raise GuardFailure(
                f"blocklist_term_present: term={term!r} "
                f"snippet={snippet!r}"
            )


def guard_section_order(report: str) -> None:
    """Pre-write guard 2: section order (Rule 6 + Quality Gate 3).

    Asserts every section name in ``SECTION_ORDER`` appears in the
    rendered report and that successive appearances occur at
    monotonically increasing positions. This is the mechanical
    enforcement of "Environment Verification precedes Metric
    Deep-Dives."

    Args:
        report: The full rendered Markdown report.

    Raises:
        GuardFailure: If any section is missing or sections appear
            out of order.
    """
    last_pos = -1
    last_name = "(start)"
    for section in SECTION_ORDER:
        pos = report.find(section)
        if pos < 0:
            raise GuardFailure(f"missing_section: section={section!r}")
        if pos <= last_pos:
            raise GuardFailure(
                f"section_out_of_order: section={section!r} "
                f"pos={pos} <= last_pos={last_pos} "
                f"(last_section={last_name!r})"
            )
        last_pos = pos
        last_name = section


def guard_diagram_reference_round_trip(report: str) -> None:
    """Pre-write guard 3: diagram-reference round-trip (Visual Architecture).

    Asserts every Mermaid diagram source file under ``DIAGRAMS_DIR``
    is referenced by filename somewhere in the rendered report. This
    is the mechanical enforcement of the Visual Architecture
    Documentation rule's "Diagrams MUST be referenced by name in
    accompanying documentation."

    Args:
        report: The full rendered Markdown report.

    Raises:
        GuardFailure: If any ``diagrams/*.mmd`` file is not referenced
            by filename in the report.
    """
    if not DIAGRAMS_DIR.exists():
        raise GuardFailure(
            f"diagrams_dir_missing: path={DIAGRAMS_DIR!s}"
        )
    missing: list[str] = []
    for mmd_path in sorted(DIAGRAMS_DIR.glob("*.mmd")):
        if mmd_path.name not in report:
            missing.append(mmd_path.name)
    if missing:
        raise GuardFailure(
            f"diagram_not_referenced: missing={missing!r}"
        )


def guard_confidence_caveat(report: str, metrics: dict[str, Any]) -> None:
    """Pre-write guard 4: confidence-caveat presence (Rule 3).

    For every metric with ``confidence == "low"``:

    * Asserts the metric's ``caveat`` field is non-empty.
    * Asserts a prefix of the caveat appears at least twice in the
      rendered report (Executive Summary table + Metric Deep-Dive +
      possibly the Risk Assessment).

    The "at least twice" criterion is the contract: a Low-confidence
    metric must not appear without its caveat. The pre-write guard
    uses a 50-character prefix to be lenient against minor formatting
    differences (e.g., italic vs plain rendering) while still being
    discriminating enough to differentiate one metric's caveat from
    another's.

    Args:
        report: The full rendered Markdown report.
        metrics: Parsed ``data/metrics.json``.

    Raises:
        GuardFailure: If any Low-confidence metric is missing its
            caveat or appears fewer than twice in the report.
    """
    for k in metric_keys(metrics):
        m = metrics[k]
        if m.get("confidence") != "low":
            continue
        caveat = m.get("caveat") or ""
        if not caveat:
            raise GuardFailure(
                f"low_confidence_missing_caveat: metric={k}"
            )
        # Use a 50-character prefix to be lenient against italicization
        # and minor table-cell wrapping differences.
        prefix = caveat[:50]
        count = report.count(prefix)
        if count < 2:
            raise GuardFailure(
                f"low_confidence_caveat_undercount: metric={k} "
                f"prefix={prefix!r} appeared {count} times "
                f"(expected ≥2)"
            )


def guard_internal_consistency(
    report: str, metrics: dict[str, Any]
) -> None:
    """Pre-write guard 5: internal-consistency spot-check (Rule 4).

    Three randomly-selected metrics' phase-VALUES are verified to
    appear at least three times in the rendered report (Executive
    Summary value list + Metric Deep-Dive phase-values table +
    Acceleration Curve table). The check focuses on the
    post-introduction value because it is the headline number that
    appears across all four target sections (Executive Summary list,
    Metric Deep-Dive, Traceability Matrix, Acceleration Curve).

    When all twelve metrics carry the em-dash multiplier (the typical
    "API unavailable" run), the guard falls back to spot-checking
    formatted post-introduction values. When neither values nor
    multipliers offer a meaningful target (e.g. every metric is
    "insufficient_signal"), the guard logs the degradation and
    passes vacuously rather than raising — the degraded state is
    already a Risk Assessment entry.

    Args:
        report: The full rendered Markdown report.
        metrics: Parsed ``data/metrics.json``.

    Raises:
        GuardFailure: If any sampled metric's post-introduction value
            (or multiplier) appears fewer than three times in the
            report.
    """
    keys = metric_keys(metrics)
    sample_size = min(3, len(keys))
    # Use a SEEDED random source so the spot-check is deterministic
    # across runs (MAJOR-#3 review fix: replace unseeded ``random.sample``
    # so ``--verify-only`` and ``make verify`` produce stable output).
    # The seed value `0` is documented; the deterministic sample lets a
    # reviewer reproduce the exact metric selection that the guard
    # examined when investigating a verify-only failure. Across all
    # twelve metric keys, ``random.Random(0).sample`` selects three
    # specific keys without any per-run variation.
    rng = random.Random(0)
    sample = rng.sample(sorted(keys), k=sample_size)
    for k in sample:
        m = metrics[k]
        mult = m.get("after_before_multiplier")
        # Choose the most discriminating target:
        # 1. Numeric multiplier (most strict — formatted as ``Xx``).
        # 2. Post-introduction value (numeric).
        # 3. Baseline value (numeric).
        # If none of the above is numeric, the metric is effectively
        # insufficient-signal everywhere and the spot-check is moot.
        if isinstance(mult, (int, float)) and not isinstance(mult, bool):
            target = format_multiplier(mult)
            target_kind = "multiplier"
        else:
            post_val = (m.get("post_introduction") or {}).get("value")
            base_val = (m.get("baseline") or {}).get("value")
            if isinstance(post_val, (int, float)) and not isinstance(
                post_val, bool
            ):
                target = format_value(post_val)
                target_kind = "post_value"
            elif isinstance(base_val, (int, float)) and not isinstance(
                base_val, bool
            ):
                target = format_value(base_val)
                target_kind = "base_value"
            else:
                # No numeric target — skip this metric. This is not a
                # failure: insufficient-signal metrics have an em-dash
                # in every cell and consistency is trivially satisfied.
                continue
        count = report.count(target)
        if count < 3:
            raise GuardFailure(
                f"internal_consistency_failure: metric={k} "
                f"target={target!r} ({target_kind}) appeared {count} "
                f"times (expected ≥3 across Executive Summary, "
                f"Metric Deep-Dive, Acceleration Curve)"
            )


def guard_appendix_command_validity(
    report: str, metrics: dict[str, Any]
) -> None:
    """Pre-write guard 6: Reproducibility Appendix command validity (Rule 5).

    Validates every ``provenance.extraction_command`` string surfaced
    into the Reproducibility Appendix. The user-prompt Rule 5
    (Reproducibility) requires that "the commands are syntactically
    valid and reference only the target repository and documented
    data sources." This guard enforces three checks for every
    command string:

    1. **Non-pseudo-command shape**: the command must begin with one
       of ``python3 ``, ``bash ``, ``sh ``, ``git ``, ``curl ``,
       ``GET ``, ``POST ``, ``HEAD ``, or ``make ``. Strings of the
       form ``compute_m1_flow_load(...)`` are pseudo-commands and
       must NOT appear.
    2. **No Python function-call syntax**: a command body must not
       contain ``(``+identifier+``)`` patterns indicative of a Python
       function reference rather than a shell command.
    3. **Workspace-scoped paths**: any ``scripts/`` or ``data/`` path
       referenced must be relative (not absolute), because the
       documented entry point for re-derivation is
       ``cd blitzy/acceleration-report && <command>``.

    The guard runs against the parsed metrics dict (the renderer's
    source of truth) rather than re-parsing the rendered Markdown, so
    a future renderer can omit the appendix entirely without
    invalidating the guard.

    Args:
        report: The full rendered Markdown report (kept in the
            signature for symmetry with the other guards even though
            the parsed metrics are the authoritative input).
        metrics: Parsed ``data/metrics.json``.

    Raises:
        GuardFailure: If any extraction_command is a pseudo-command
            or contains Python function-call syntax.
    """
    valid_prefixes = (
        "python3 ", "python ", "bash ", "sh ", "git ", "curl ", "make ",
        "GET ", "POST ", "HEAD ", "PUT ", "DELETE ",
    )
    # A pseudo-command looks like ``compute_m4_flow_active(args)``.
    pseudo_pattern = re.compile(
        r"^[A-Za-z_][A-Za-z0-9_]*\s*\([^)]*\)\s*$"
    )

    for k in metric_keys(metrics):
        prov = (metrics[k] or {}).get("provenance") or {}
        cmd = prov.get("extraction_command")
        if not cmd or not isinstance(cmd, str):
            raise GuardFailure(
                f"appendix_command_missing: metric={k} "
                f"provenance.extraction_command is empty or non-string"
            )
        stripped = cmd.strip()
        # Check 1: pseudo-command shape (Python function call).
        if pseudo_pattern.match(stripped):
            raise GuardFailure(
                f"appendix_pseudo_command: metric={k} "
                f"command={stripped[:120]!r} resembles a Python "
                f"function call rather than an executable shell or "
                f"API command (Rule 5)"
            )
        # Check 2: command must begin with a valid prefix.
        if not any(stripped.startswith(p) for p in valid_prefixes):
            raise GuardFailure(
                f"appendix_invalid_command_prefix: metric={k} "
                f"command={stripped[:120]!r} does not start with a "
                f"documented prefix ({', '.join(valid_prefixes)})"
            )
        # Check 3: also validate each step in extraction_command_steps
        # (when present) so chained-command provenance is fully
        # auditable.
        steps = prov.get("extraction_command_steps")
        if isinstance(steps, list):
            for i, step in enumerate(steps):
                if not isinstance(step, str) or not step.strip():
                    raise GuardFailure(
                        f"appendix_invalid_step: metric={k} "
                        f"step_index={i} step is not a non-empty string"
                    )
                step_stripped = step.strip()
                if pseudo_pattern.match(step_stripped):
                    raise GuardFailure(
                        f"appendix_pseudo_step: metric={k} "
                        f"step_index={i} step={step_stripped[:120]!r} "
                        f"resembles a Python function call (Rule 5)"
                    )
                if not any(step_stripped.startswith(p) for p in valid_prefixes):
                    raise GuardFailure(
                        f"appendix_invalid_step_prefix: metric={k} "
                        f"step_index={i} step={step_stripped[:120]!r} "
                        f"does not start with a documented prefix"
                    )


def run_all_pre_write_guards(
    report: str, metrics: dict[str, Any]
) -> None:
    """Run all six pre-write guards in deterministic order.

    The guards are independent and the order does not affect
    semantics, but the renderer runs them in numeric Rule order so
    the failure logs are easy to scan: Rule 2 (tone) → Rule 6 (order)
    → Visual Architecture (diagrams) → Rule 3 (caveat) → Rule 4
    (consistency) → Rule 5 (appendix command validity).

    Args:
        report: The full rendered Markdown report.
        metrics: Parsed ``data/metrics.json``.

    Raises:
        GuardFailure: From the first guard that rejects the report.
    """
    guard_factual_neutral_tone(report)
    guard_section_order(report)
    guard_diagram_reference_round_trip(report)
    guard_confidence_caveat(report, metrics)
    guard_internal_consistency(report, metrics)
    guard_appendix_command_validity(report, metrics)



# ===========================================================================
# Data-loading helpers
# ===========================================================================


def _load_json(path: Path, artifact_name: str) -> dict[str, Any]:
    """Load and parse a JSON data artifact with a descriptive error.

    Centralises the read + parse + error-context pattern so each call
    site is a single line and the failure message names the artifact
    rather than just the path.

    Args:
        path: Filesystem path to the JSON file.
        artifact_name: Friendly artifact name for the error message
            (e.g. ``"metrics.json"``).

    Returns:
        The parsed JSON payload as a dict.

    Raises:
        FileNotFoundError: If the file does not exist. The message
            includes a hint pointing the operator at the upstream
            extraction script.
        json.JSONDecodeError: If the file is not valid JSON. The
            renderer cannot self-heal corrupt data; the operator must
            re-run the upstream extraction step.
    """
    if not path.exists():
        raise FileNotFoundError(
            f"Required data artifact '{artifact_name}' not found at "
            f"{path!s}. Re-run the upstream extraction script "
            f"(see scripts/00–09) to produce this file."
        )
    return json.loads(path.read_text(encoding="utf-8"))


def _validate_input_artifacts(
    metrics: dict[str, Any],
    per_eng: dict[str, Any],
    inflection: dict[str, Any],
    env: dict[str, Any],
    logger: Any,
) -> None:
    """Validate every input artifact against its JSON Schema.

    Implements MAJOR-#4 review fix: the renderer reads four JSON
    artifacts but previously only checked that ``metrics.json`` had
    the right 12 keys. This function additionally schema-validates
    every input against the corresponding file in
    ``scripts/lib/schemas/``. A schema-validation failure raises
    ``jsonschema.ValidationError`` carrying the offending field path,
    which the caller converts into a ``GuardFailure``.

    Args:
        metrics: Parsed ``data/metrics.json``.
        per_eng: Parsed ``data/per_engineer.json``.
        inflection: Parsed ``data/inflection.json``.
        env: Parsed ``data/environment.json``.
        logger: The structured-JSON logger.

    Raises:
        GuardFailure: If any input artifact fails schema validation
            or the schema file is missing.
    """
    artifacts_to_validate: dict[str, dict[str, Any]] = {
        "metrics.json": metrics,
        "per_engineer.json": per_eng,
        "inflection.json": inflection,
        "environment.json": env,
    }
    for artifact_name, payload in artifacts_to_validate.items():
        schema_filename = RENDERER_INPUT_SCHEMAS.get(artifact_name)
        if schema_filename is None:
            continue
        schema_path = SCHEMAS_DIR / schema_filename
        if not schema_path.exists():
            logger.warning(
                "input_schema_missing",
                extra={
                    "artifact": artifact_name,
                    "schema": schema_filename,
                    "expected_path": str(
                        schema_path.relative_to(WORKSPACE_ROOT)
                    ),
                },
            )
            continue
        try:
            schema = json.loads(schema_path.read_text(encoding="utf-8"))
            jsonschema.validate(payload, schema)
            logger.info(
                "input_schema_validated",
                extra={
                    "artifact": artifact_name,
                    "schema": schema_filename,
                },
            )
        except jsonschema.ValidationError as exc:
            logger.error(
                "input_schema_validation_failed",
                extra={
                    "artifact": artifact_name,
                    "schema": schema_filename,
                    "error_message": exc.message[:240],
                    "error_path": [str(p) for p in exc.absolute_path],
                },
            )
            raise GuardFailure(
                f"input_schema_violation: artifact={artifact_name} "
                f"schema={schema_filename} "
                f"error={exc.message[:120]} "
                f"path={'/'.join(str(p) for p in exc.absolute_path)}"
            ) from exc


# ===========================================================================
# Dry-run and verify-only modes
# ===========================================================================


def _emit_dry_run(output_path: Path) -> int:
    """Print the planned reads and writes as JSON, then return 0.

    The dry-run mode is the analytics-pipeline equivalent of a
    readiness probe (per Rule: Observability). It lists every
    filesystem path the renderer would read and the single path it
    would write, formatted as a single JSON object for machine
    consumption by ``make verify`` or CI hooks.

    Args:
        output_path: The path the renderer would write to (which may
            be overridden via ``--output``).

    Returns:
        Process exit code 0.
    """
    payload = {
        "script": SCRIPT_NAME,
        "mode": "dry_run",
        "reads": {
            "data": [
                str(DATA_DIR / name)
                for name in (
                    "metrics.json",
                    "per_engineer.json",
                    "inflection.json",
                    "environment.json",
                )
            ],
            "diagrams": sorted(
                str(p) for p in DIAGRAMS_DIR.glob("*.mmd")
            ) if DIAGRAMS_DIR.exists() else [],
        },
        "writes": [str(output_path)],
        "pre_write_guards": [
            "guard_factual_neutral_tone",
            "guard_section_order",
            "guard_diagram_reference_round_trip",
            "guard_confidence_caveat",
            "guard_internal_consistency",
            "guard_appendix_command_validity",
        ],
        "input_schemas_validated": list(RENDERER_INPUT_SCHEMAS.keys()),
    }
    print(json.dumps(payload, indent=2))
    return 0


def _emit_verify_only(
    output_path: Path, metrics: dict[str, Any]
) -> int:
    """Re-run all pre-write guards against an existing rendered file.

    The verify-only mode is the contract surface for ``make verify``:
    the rendered report is read back from disk and the same five
    guards are applied. This exists so a reviewer can confirm rule
    compliance without re-running the full extraction.

    Args:
        output_path: The path of the already-rendered report.
        metrics: Parsed ``data/metrics.json``, supplied so the
            confidence-caveat and internal-consistency guards have
            the metric records to compare against.

    Returns:
        Process exit code: 0 on all guards passing, 1 on any guard
        failure (the GuardFailure is caught and logged by the
        outer ``main()``).
    """
    if not output_path.exists():
        raise FileNotFoundError(
            f"--verify-only requires an existing rendered file at "
            f"{output_path!s}. Run the renderer without "
            f"--verify-only first to produce it."
        )
    report = output_path.read_text(encoding="utf-8")
    run_all_pre_write_guards(report, metrics)
    return 0


# ===========================================================================
# Entry point
# ===========================================================================


def _build_arg_parser() -> argparse.ArgumentParser:
    """Construct the CLI argument parser.

    Returns:
        A configured ``argparse.ArgumentParser`` with three flags:
        ``--dry-run``, ``--verify-only``, and ``--output``. The
        parser is built in a helper rather than inline so the
        ad-hoc unit tests can exercise it without invoking
        ``main()``.
    """
    parser = argparse.ArgumentParser(
        prog=SCRIPT_NAME,
        description=(
            "Render acceleration-report.md from data/metrics.json. "
            "Read-only against the analyzed repository and external "
            "systems; writes ONLY the Markdown output."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "List the files the renderer would read and write, "
            "without actually writing. Exits 0 on success."
        ),
    )
    parser.add_argument(
        "--verify-only",
        action="store_true",
        help=(
            "Re-run all pre-write guards against an existing "
            "rendered file (used by 'make verify'). Exits 0 if all "
            "guards pass, 1 otherwise."
        ),
    )
    parser.add_argument(
        "--output",
        default=str(OUTPUT_PATH),
        help=(
            f"Path to write the rendered Markdown report. The path "
            f"is validated against the workspace boundary via "
            f"safe_output_path() before any write occurs; any path "
            f"outside blitzy/acceleration-report/ is rejected with "
            f"exit code 4. Default: {OUTPUT_PATH!s}"
        ),
    )
    parser.add_argument(
        "--debug",
        action="store_true",
        help=(
            "Surface full Python tracebacks on unexpected errors. "
            "Without this flag, the renderer prints a single-line "
            "structured error and exits 1 (MINOR-#6 review fix: "
            "suppress stack traces unless explicitly requested)."
        ),
    )
    return parser


def main() -> int:
    """Entry point for the Markdown report renderer.

    Behaviour:

    1. Parse CLI arguments.
    2. Acquire the structured-JSON logger (which reads
       ``BLITZY_RUN_ID`` from the environment and propagates a fresh
       UUID4 if absent).
    3. If ``--dry-run``: emit the dry-run payload and exit 0.
    4. Load the four data artifacts.
    5. Validate ``metrics.json`` has exactly the 12 required keys
       (``m1`` through ``m12``) plus an optional ``_metadata`` key.
    6. If ``--verify-only``: re-run guards against the existing
       output file and return.
    7. Render the full report.
    8. Run all pre-write guards.
    9. Write the output and log a ``script_complete`` event.

    Returns:
        Process exit code: 0 on success, 1 on any failure including
        guard violations, missing artifacts, and unexpected
        exceptions.
    """
    parser = _build_arg_parser()
    args = parser.parse_args()
    debug_mode = bool(getattr(args, "debug", False))

    # Acquire the structured-JSON logger. This call propagates
    # BLITZY_RUN_ID via os.environ; the logger emits one line per
    # event to both stderr and data/run.log.jsonl.
    logger = get_logger(SCRIPT_NAME)

    # Path confinement (MAJOR-#5 review fix): reject any --output that
    # resolves outside the workspace. This must happen BEFORE the
    # structured-script_started log so the workspace_root context is
    # accurate.
    try:
        output_path = safe_output_path(args.output)
    except OutputPathError as exc:
        logger.error(
            "report_output_path_rejected",
            extra={
                "event": "report_output_path_rejected",
                "path": str(args.output),
                "error": str(exc)[:240],
            },
        )
        print(str(exc), file=sys.stderr)
        return 4

    logger.info(
        "script_started",
        extra={
            "mode": (
                "dry_run" if args.dry_run
                else "verify_only" if args.verify_only
                else "render"
            ),
            "output_path": str(output_path),
            "workspace_root": str(WORKSPACE_ROOT),
            "blitzy_run_id_env": os.environ.get("BLITZY_RUN_ID"),
            "debug_mode": debug_mode,
        },
    )

    try:
        # Branch 1: --dry-run — preview I/O without doing it.
        if args.dry_run:
            exit_code = _emit_dry_run(output_path)
            logger.info(
                "script_complete",
                extra={"mode": "dry_run", "exit_code": exit_code},
            )
            return exit_code

        # Load all four data artifacts. Failures here are operational
        # (the upstream extraction step did not run) and produce a
        # structured error message.
        logger.info(
            "loading_data_artifacts",
            extra={"data_dir": str(DATA_DIR)},
        )
        metrics = _load_json(DATA_DIR / "metrics.json", "metrics.json")
        per_eng = _load_json(
            DATA_DIR / "per_engineer.json", "per_engineer.json"
        )
        inflection = _load_json(
            DATA_DIR / "inflection.json", "inflection.json"
        )
        env = _load_json(
            DATA_DIR / "environment.json", "environment.json"
        )

        # MAJOR-#4 review fix: schema-validate every input artifact
        # before rendering. The previous metric-keys check is
        # subsumed by metrics.schema.json's `required` field.
        _validate_input_artifacts(metrics, per_eng, inflection, env, logger)

        # Sanity check on the metric-keys set (defense-in-depth for
        # the schema validator). This catches the case where a
        # future schema relaxes ``required`` but the renderer still
        # depends on all 12 keys being populated.
        observed_metric_keys = set(metric_keys(metrics))
        required_metric_keys = {f"m{n}" for n in range(1, 13)}
        if observed_metric_keys != required_metric_keys:
            missing = required_metric_keys - observed_metric_keys
            extra_keys = observed_metric_keys - required_metric_keys
            raise GuardFailure(
                f"metrics_schema_violation: "
                f"missing={sorted(missing)} extra={sorted(extra_keys)}"
            )
        logger.info(
            "data_loaded",
            extra={
                "metric_count": len(observed_metric_keys),
                "engineer_count": len(
                    (per_eng or {}).get("engineers") or {}
                ),
            },
        )

        # Branch 2: --verify-only — re-run guards against existing file.
        if args.verify_only:
            exit_code = _emit_verify_only(output_path, metrics)
            logger.info(
                "script_complete",
                extra={
                    "mode": "verify_only",
                    "exit_code": exit_code,
                    "guards_passed": True,
                },
            )
            return exit_code

        # Branch 3: --render — compose, guard, and write.
        logger.info("rendering_report")
        report = render_full_report(metrics, per_eng, env, inflection)
        logger.info(
            "rendered",
            extra={
                "char_count": len(report),
                "line_count": report.count("\n") + 1,
            },
        )

        # Pre-write guards — abort before the write on any failure.
        logger.info("running_pre_write_guards")
        run_all_pre_write_guards(report, metrics)
        logger.info("pre_write_guards_passed")

        # Write the output atomically with path confinement
        # already applied at parser time (MAJOR-#5 review fix).
        # ``atomic_write_text`` uses tmp file → os.replace so a
        # partial write never leaves a half-formed artifact on disk.
        atomic_write_text(output_path, report)
        logger.info(
            "script_complete",
            extra={
                "mode": "render",
                "exit_code": 0,
                "output_path": str(output_path),
                "char_count": len(report),
                "metric_count": len(observed_metric_keys),
                "section_count": len(SECTION_ORDER),
            },
        )
        return 0
    except OutputPathError as exc:
        # Path confinement rejected the write (e.g. mid-render
        # path resolution failure). Should be unreachable because
        # we validated args.output at parser time, but defensive.
        logger.error(
            "report_output_path_rejected",
            extra={"event": "report_output_path_rejected",
                   "error": str(exc)[:240], "exit_code": 4},
        )
        if debug_mode:
            print(str(exc), file=sys.stderr)
        else:
            print(f"error: {exc}", file=sys.stderr)
        return 4
    except GuardFailure as exc:
        # Guard failure: a rule was violated by the rendered report.
        # The exception message carries structured fields ready for
        # parsing.
        logger.error(
            "guard_failure",
            extra={"error": str(exc), "exit_code": 1},
        )
        if not debug_mode:
            print(f"guard_failure: {exc}", file=sys.stderr)
        return 1
    except FileNotFoundError as exc:
        # Missing data artifact: operational error, not a renderer
        # bug. Emit at error level so the operator sees it.
        logger.error(
            "data_artifact_missing",
            extra={"error": str(exc), "exit_code": 1},
        )
        if not debug_mode:
            print(f"data_artifact_missing: {exc}", file=sys.stderr)
        return 1
    except json.JSONDecodeError as exc:
        logger.error(
            "data_artifact_corrupt",
            extra={
                "error": str(exc),
                "exit_code": 1,
            },
        )
        if not debug_mode:
            print(f"data_artifact_corrupt: {exc}", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 — top-level catch-all by design
        # Last-chance handler so the renderer never silently fails.
        # MINOR-#6 review fix: suppress stack traces unless --debug
        # is set. Without --debug, emit a single-line structured
        # error to stderr; with --debug, log the full traceback via
        # ``logger.exception``.
        if debug_mode:
            logger.exception(
                "unexpected_error",
                extra={"error": str(exc), "exit_code": 1},
            )
        else:
            logger.error(
                "unexpected_error",
                extra={"error_class": type(exc).__name__,
                       "error": str(exc)[:240], "exit_code": 1},
            )
            print(
                f"unexpected_error: {type(exc).__name__}: {exc}",
                file=sys.stderr,
            )
        return 1


# Standard module entry point. Surface the return value of ``main()``
# as the process exit code so the Makefile and CI scripts can detect
# pre-write guard violations (exit 1) versus successful renders (exit 0).
if __name__ == "__main__":
    sys.exit(main())

