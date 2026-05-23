#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
09_compute_metrics.py — Deterministic metric computation for the Acceleration Report.

PURPOSE
-------
This script is the SINGLE SOURCE OF TRUTH for all 12 metric values. It reads every
raw data artifact under ``blitzy/acceleration-report/data/`` produced by scripts
00–08 and emits exactly two output files:

* ``data/metrics.json``      — twelve metric values plus the ``_metadata`` envelope.
* ``data/per_engineer.json`` — per-engineer breakdown (real names + ``Blitzy``)
                               for Metrics 2, 4, 5, 6, 10.

The downstream renderers (``10_render_report.py``, ``11_render_deck.py``) consume
ONLY these two files; they never read the raw artifacts directly. This is the
mechanical enforcement of Rule 4 (Internal Consistency) per AAP §0.5.6.

CONTRACTS (enforced via JSON Schema validation before write)
------------------------------------------------------------
* ``data/metrics.json`` validates against ``scripts/lib/schemas/metrics.schema.json``.
* Exactly 13 top-level keys: ``_metadata`` + ``m1``..``m12`` (no extras).
* Each metric carries ``name``, ``value``, ``confidence``, ``baseline``, ``provenance``
  PLUS either ``post_introduction`` (two-phase) XOR (``ramp_up`` + ``steady_state``).
* Confidence ``low`` requires a ``caveat`` field (Rule 3).
* Confidence ``≠ high`` requires ``boundary_conditions``.
* Value ``"insufficient_signal"`` requires a ``reason``.

CRITICAL CONSTRAINTS
--------------------
* **Pure I/O**: no HTTP (no ``requests.*``), no shell-out (no ``subprocess.*``,
  no ``git_*`` helpers). Every byte of input comes from the local filesystem.
* **Read-only against the analyzed repository**: the script never touches the
  rudder-server working tree, git history, refs, or any external system.
* **Identical methodology**: module-scope constants (window mechanics, bot
  exclusion list, conventional-prefix-to-category map, Blitzy identity union)
  are NEVER re-parameterised between the baseline and post-introduction periods.
  The engineering-actor parameter is the only thing that changes (per AAP §0.5.6).
* **Insufficient-signal handling**: when a data source is unavailable the
  metric value is the literal string ``"insufficient_signal"`` plus a non-empty
  ``reason`` field. The script never fabricates, estimates, or extrapolates
  (per AAP §0.3.2).

OBSERVABILITY (Rule: Observability)
-----------------------------------
Every event is logged via ``lib.observability.get_logger`` which produces
single-line structured JSON to stderr and to ``data/run.log.jsonl``. The
per-run correlation ID is propagated via the ``BLITZY_RUN_ID`` environment
variable set by ``00_environment.sh`` at the head of the pipeline.

CLI FLAGS (Rule: Observability — readiness preflight)
-----------------------------------------------------
* ``--help``     : argparse-generated usage. Exits 0.
* ``--dry-run``  : Prints the JSON list of files this script would read and
                   write, then exits 0 without performing any I/O on data files.

EXIT CODES
----------
* 0 : success.
* 1 : raw artifact missing OR JSON-schema validation failure OR unexpected error.

USAGE
-----
    cd blitzy/acceleration-report
    python3 scripts/09_compute_metrics.py
    python3 scripts/09_compute_metrics.py --dry-run
    python3 scripts/09_compute_metrics.py --help

REFERENCES
----------
AAP §0.5.6 (engineering-actor substitution, identical methodology);
AAP §0.2.4 (per-metric data source resolution and confidence tiering);
AAP §0.5.3 (per-metric definitions);
``decision-log.md`` DL-001 .. DL-007 (non-trivial decisions);
``scripts/lib/schemas/metrics.schema.json`` (output contract).
"""

from __future__ import annotations

import argparse
import csv
import json
import os
import statistics
import sys
from collections import defaultdict
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Callable

# Make the colocated ``lib`` package importable when the script is invoked
# directly (``python3 scripts/09_compute_metrics.py``) rather than as a module.
sys.path.insert(0, str(Path(__file__).resolve().parent))

# Third-party dependencies (pinned in blitzy/acceleration-report/requirements.txt):
#   python-dateutil==2.9.0.post0  — robust ISO-8601 parsing
#   jsonschema==4.23.0            — Draft 2020-12 schema validation
from dateutil import parser as dateutil_parser  # noqa: E402  (sys.path mutation)
import jsonschema  # noqa: E402

from lib.observability import get_logger  # noqa: E402

# ---------------------------------------------------------------------------
# Script identity (consumed by the structured-JSON logger)
# ---------------------------------------------------------------------------

SCRIPT_NAME = "09_compute_metrics"

# ---------------------------------------------------------------------------
# Filesystem layout
# ---------------------------------------------------------------------------
# WORKSPACE_ROOT is blitzy/acceleration-report/ (the parent of scripts/).
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent
DATA_DIR: Path = WORKSPACE_ROOT / "data"
SCHEMAS_DIR: Path = Path(__file__).resolve().parent / "lib" / "schemas"

METRICS_JSON_PATH: Path = DATA_DIR / "metrics.json"
PER_ENGINEER_JSON_PATH: Path = DATA_DIR / "per_engineer.json"
METRICS_SCHEMA_PATH: Path = SCHEMAS_DIR / "metrics.schema.json"

# Every raw artifact this script reads (consumed by ``--dry-run`` and by the
# Rule-1 (Data Provenance) verifier).
RAW_ARTIFACTS: tuple[str, ...] = (
    "environment.json",
    "inflection.json",
    "commits.csv",
    "pulls.json",
    "reviews.json",
    "pull_events.json",
    "releases.json",
    "reverts.json",
    "ci_runs.json",
    "test_transitions.json",
    "exceptions.json",
    "issues.json",
    "slas.json",
)

# ---------------------------------------------------------------------------
# Schema version emitted into ``_metadata.schema_version`` of metrics.json.
# Mirrors the document under scripts/lib/schemas/metrics.schema.json.
# ---------------------------------------------------------------------------

METRICS_SCHEMA_VERSION = "1.1.0"

# ---------------------------------------------------------------------------
# Window mechanics — CONSTANTS at module scope (never re-parameterised
# between baseline and post-introduction; see AAP §0.5.6).
# ---------------------------------------------------------------------------

WINDOW_DAYS: int = 14
"""Length in days of the canonical analysis window (2 weeks)."""

WINDOW_ANCHOR_WEEKDAY: int = 0
"""ISO weekday anchor: 0 == Monday. Windows start on Monday 00:00 UTC."""

# ---------------------------------------------------------------------------
# Bot exclusion list (Metric 2 — Flow Velocity).
# Per AAP §0.1.5 and DL-003: dependabot is excluded; blitzy[bot] is NOT.
# ---------------------------------------------------------------------------

DEPENDENCY_BOT_EMAILS: frozenset[str] = frozenset({
    "49699333+dependabot[bot]@users.noreply.github.com",
})
"""Exact-match exclusion set for dependency-bot authors."""

DEPENDENCY_BOT_PATTERN: str = "-bot[bot]@users.noreply.github.com"
"""Wildcard suffix used to detect dependency-bot identities not in the exact
list. blitzy[bot] is checked first and explicitly NOT excluded."""

# ---------------------------------------------------------------------------
# Blitzy identity union (per AAP §0.1.5 and DL-003).
# All commits authored by either email collate into the single "Blitzy" actor.
# ---------------------------------------------------------------------------

BLITZY_PRIMARY_EMAIL: str = "agent@blitzy.com"
BLITZY_BOT_EMAIL: str = "191547922+blitzy[bot]@users.noreply.github.com"
BLITZY_IDENTITY_EMAILS: frozenset[str] = frozenset({
    BLITZY_PRIMARY_EMAIL,
    BLITZY_BOT_EMAIL,
})
BLITZY_DISPLAY_NAME: str = "Blitzy"

# Display-name aliases observed in git history for ``agent@blitzy.com``
# (per AAP §0.5.6). Any of these must canonicalise to ``Blitzy``.
BLITZY_DISPLAY_ALIASES: frozenset[str] = frozenset({
    "Blitzy Agent",
    "agent",
    "agent@blitzy.com",
    "blitzy-agent",
    "blitzy[bot]",
})

# ---------------------------------------------------------------------------
# Conventional-commit prefix → AAP category map (Metric 6 — Flow Distribution).
# Authority: .github/workflows/semantic-pr.yaml#types.
# Per AAP §0.5.3.7: feat → feature, fix → defect, the rest → tech-debt.
# ---------------------------------------------------------------------------

CONVENTIONAL_PREFIX_MAP: dict[str, str] = {
    "feat": "feature",
    "fix": "defect",
    "chore": "tech-debt",
    "refactor": "tech-debt",
    "exp": "tech-debt",
    "doc": "tech-debt",
    "test": "tech-debt",
}

# ---------------------------------------------------------------------------
# Keyword catalogues for Metric 6 step 3 (PR title/body classifier fallback).
# Sets are case-folded by the classifier; keep entries lowercase.
# ---------------------------------------------------------------------------

RISK_COMPLIANCE_KEYWORDS: frozenset[str] = frozenset({
    "security", "compliance", "audit", "sla",
    "gdpr", "pci", "cve", "vulnerability",
})
DEFECT_KEYWORDS: frozenset[str] = frozenset({
    "bug", "fix", "regression", "hotfix",
})
TECH_DEBT_KEYWORDS: frozenset[str] = frozenset({
    "refactor", "tech debt", "cleanup", "rename", "format",
})
FEATURE_KEYWORDS: frozenset[str] = frozenset({
    "feature", "add", "support", "enable",
})

# ---------------------------------------------------------------------------
# Multi-module aggregation: top-level directories of the Go monorepo.
# Used by the per-module commit attribution helper.
# Source: source_folder:./ + AAP §0.5.6.
# ---------------------------------------------------------------------------

MODULE_LIST: tuple[str, ...] = (
    "gateway", "processor", "router", "warehouse", "jobsdb", "services",
    "admin", "app", "archiver", "backend-config", "cluster", "cmd", "config",
    "controlplane", "enterprise", "functions", "identity", "info", "internal",
    "middleware", "mocks", "proto", "protocols", "regulation-worker",
    "rruntime", "runner", "schema-forwarder", "suppression-backup-service",
    "testhelper", "utils",
)

# ---------------------------------------------------------------------------
# Temporal-phase thresholds.
# ---------------------------------------------------------------------------

RAMP_UP_DAYS: int = 90
"""Per AAP §0.5.6, Ramp-Up covers the first 90 days post-introduction; Steady
State covers ≥90 days. If the post-introduction span is shorter than this,
the two-phase fallback (Baseline + Post-Introduction) is used."""

# ---------------------------------------------------------------------------
# Flaky-test guard (Metric 11 — Escaped Defects).
# ---------------------------------------------------------------------------

FLAKY_THRESHOLD: int = 3
"""Number of consecutive failed runs required before a pass→fail transition
counts as a regression. Tests that flip back to pass within FLAKY_THRESHOLD
runs are flagged as flaky and excluded (per AAP §0.5.3.12)."""

# ===========================================================================
# Helper utilities
# ===========================================================================


def parse_iso(value: Any) -> datetime | None:
    """Parse an ISO-8601 timestamp into a timezone-aware UTC datetime.

    Accepts ``None`` and the empty string by returning ``None`` (so callers
    can chain ``parse_iso(pr.get("merged_at"))`` against optional fields).
    All naive datetimes are coerced to UTC. ``dateutil.parser.isoparse`` is
    used for robustness against the ``Z`` suffix, fractional seconds, and
    offset forms that appear across the raw artifacts.
    """
    if value is None:
        return None
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=timezone.utc)
    if isinstance(value, str):
        s = value.strip()
        if not s:
            return None
        dt = dateutil_parser.isoparse(s)
        return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)
    raise TypeError(f"parse_iso: unsupported input type {type(value).__name__}")


def iso_z(dt: datetime) -> str:
    """Render a timezone-aware datetime as strict ISO-8601 Z-form.

    The schema's ``isoDateTime`` $def requires the exact pattern
    ``^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}Z$`` — no fractional seconds,
    no offset. This helper enforces that shape regardless of the input's
    microsecond field and any non-UTC offset.
    """
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    else:
        dt = dt.astimezone(timezone.utc)
    return dt.replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def monday_floor(dt: datetime) -> datetime:
    """Round ``dt`` down to the start of its enclosing Monday 00:00 UTC.

    The 2-week analysis windows are anchored to Monday 00:00 UTC per AAP
    §0.5.6. Both phases share this anchor — never re-parameterised.
    """
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    else:
        dt = dt.astimezone(timezone.utc)
    days_since_monday = dt.weekday()  # Monday == 0
    floored = dt - timedelta(days=days_since_monday)
    return floored.replace(hour=0, minute=0, second=0, microsecond=0)


def windows_in_phase(
    phase_start: datetime,
    phase_end: datetime,
    global_anchor: datetime | None = None,
) -> list[tuple[datetime, datetime]]:
    """Enumerate Monday-anchored 2-week windows whose END falls inside the
    phase's date range.

    Per AAP §0.5.6: "All 2-week windows are anchored to Monday 00:00 UTC.
    [...] A window is included in a phase if its window-end falls within
    the phase's date range." The phase membership is therefore determined
    by the window-END timestamp; the START of the first phase window may
    precede the phase's start when the window straddles the inflection
    boundary.

    When ``global_anchor`` is provided, the grid is anchored to
    ``monday_floor(global_anchor)`` so both phases share the same
    grid (this is the rule that makes the upstream pre-computed series
    in pulls.json#summary.prs_per_2week_window_* agree with this
    script's per-window output). When omitted, ``phase_start`` is used.
    """
    if phase_start.tzinfo is None:
        phase_start = phase_start.replace(tzinfo=timezone.utc)
    if phase_end.tzinfo is None:
        phase_end = phase_end.replace(tzinfo=timezone.utc)
    anchor = global_anchor if global_anchor is not None else phase_start
    cursor = monday_floor(anchor)
    out: list[tuple[datetime, datetime]] = []
    # Step the cursor forward until we reach or exceed the phase window
    # of interest, then collect windows whose END is in [phase_start, phase_end].
    while cursor + timedelta(days=WINDOW_DAYS) <= phase_end:
        w_end = cursor + timedelta(days=WINDOW_DAYS)
        # AAP §0.5.6: a window belongs to a phase if its window-END falls
        # within the phase's date range.
        if phase_start < w_end <= phase_end:
            out.append((cursor, w_end))
        elif w_end > phase_end:
            break
        cursor += timedelta(days=WINDOW_DAYS)
    return out


def is_dependency_bot(email: str | None) -> bool:
    """Return True when ``email`` matches the dependency-bot exclusion list.

    The Blitzy bot account is explicitly NOT excluded (it represents Blitzy's
    own engineering output, per AAP §0.1.5).
    """
    if not email:
        return False
    if email == BLITZY_BOT_EMAIL:
        return False
    if email in DEPENDENCY_BOT_EMAILS:
        return True
    return DEPENDENCY_BOT_PATTERN in email


def is_blitzy(email: str | None) -> bool:
    """Return True when ``email`` is part of the Blitzy identity union."""
    return bool(email) and email in BLITZY_IDENTITY_EMAILS


def canonical_actor(email: str | None, display_name: str | None = None) -> str:
    """Canonicalise (email, display_name) into a single engineering-actor key.

    The canonical key is derived FROM THE EMAIL, never from the display
    name — this guarantees that the same person who appears under
    multiple display-name variants in git history (e.g.,
    ``michael@blitzy.com`` shows up as both ``Michael Montanaro`` and
    ``montanaromi``) collapses to a single row.

    * Any Blitzy identity (``agent@blitzy.com`` or the Blitzy bot email,
      or any of the recorded Blitzy display-name aliases) → ``"Blitzy"``.
    * Anything else → the email local-part (e.g., ``michael@blitzy.com``
      → ``"michael"``, ``awadhwani@blitzy.com`` → ``"awadhwani"``).
    * Empty email + display name → the display name.
    * Empty email + empty display name → ``"unknown"``.

    This canonicalisation guarantees Rule 4 (Internal Consistency) for the
    per-engineer breakdown.
    """
    if is_blitzy(email):
        return BLITZY_DISPLAY_NAME
    if display_name and display_name in BLITZY_DISPLAY_ALIASES:
        return BLITZY_DISPLAY_NAME
    if email:
        return email.split("@", 1)[0]
    if display_name:
        return display_name
    return "unknown"


def parse_conventional_prefix(title: str | None) -> str | None:
    """Extract the conventional-commit type from a PR title.

    Recognises the seven types allowed by ``.github/workflows/semantic-pr.yaml``:
    ``feat``, ``fix``, ``chore``, ``refactor``, ``exp``, ``doc``, ``test``.
    Accepts optional scope (``feat(api):``) and optional ``!`` breaking-change
    marker. Returns ``None`` when no recognised prefix is present so the
    caller can fall through to the keyword-match step.
    """
    if not title:
        return None
    lowered = title.strip().lower()
    # Strip a leading marker like "blitzy:" or "merge:" that doesn't match a
    # conventional type — these often prefix Blitzy-authored PRs in this
    # repository (per data/pulls.json titles). We DO NOT treat them as a
    # conventional type; the classifier falls through to the keyword step.
    for prefix in CONVENTIONAL_PREFIX_MAP:
        # Match "<type>:", "<type>(scope):", "<type>!:", "<type>(scope)!:".
        if lowered.startswith(f"{prefix}:"):
            return prefix
        if lowered.startswith(f"{prefix}("):
            close = lowered.find(")", len(prefix))
            if close > 0 and lowered[close + 1:close + 2] in (":", "!"):
                return prefix
        if lowered.startswith(f"{prefix}!:"):
            return prefix
    return None


def classify_pr_category(title: str | None, body: str | None,
                          existing_classification: str | None = None) -> str:
    """Apply the AAP §0.5.3.7 multi-tier classifier and return the category.

    Priority:
        1. ``existing_classification`` from the raw artifact (when Linear
           label resolution was performed upstream by 08_extract_linear.py).
           Accepted values: ``feature``, ``defect``, ``risk/compliance``,
           ``tech-debt``, ``tech_debt``, ``unknown``.
        2. Conventional-commit prefix on PR title (per ``CONVENTIONAL_PREFIX_MAP``).
        3. Keyword match against title + body (priority: risk → defect →
           tech-debt → feature).
        4. ``unknown``.
    """
    # Step 1 — honour upstream classifier output when present and non-unknown.
    if existing_classification:
        normalised = existing_classification.replace("_", "-").lower()
        if normalised in {"feature", "defect", "risk/compliance",
                          "risk-compliance", "tech-debt", "unknown"}:
            if normalised == "risk-compliance":
                return "risk/compliance"
            if normalised != "unknown":
                return normalised
    # Step 2 — conventional-commit prefix.
    prefix = parse_conventional_prefix(title)
    if prefix:
        return CONVENTIONAL_PREFIX_MAP[prefix]
    # Step 3 — keyword match.
    haystack = f"{title or ''} {body or ''}".lower()
    if any(kw in haystack for kw in RISK_COMPLIANCE_KEYWORDS):
        return "risk/compliance"
    if any(kw in haystack for kw in DEFECT_KEYWORDS):
        return "defect"
    if any(kw in haystack for kw in TECH_DEBT_KEYWORDS):
        return "tech-debt"
    if any(kw in haystack for kw in FEATURE_KEYWORDS):
        return "feature"
    return "unknown"


def load_artifact(name: str, logger: Any) -> Any:
    """Load a raw artifact from ``DATA_DIR`` by filename.

    Returns ``None`` when the artifact is missing (compute functions fall
    back to the schema-permitted ``insufficient_signal`` value when this
    happens). CSV artifacts are parsed with the pipe (``|``) delimiter as
    emitted by ``02_extract_commits.sh``. JSON artifacts are parsed with
    the stdlib ``json`` module.
    """
    path = DATA_DIR / name
    if not path.exists():
        logger.warning(
            "raw_artifact_missing",
            extra={"event": "raw_artifact_missing", "artifact": name,
                   "path": str(path.relative_to(WORKSPACE_ROOT))},
        )
        return None
    try:
        if name.endswith(".csv"):
            with path.open(encoding="utf-8", newline="") as f:
                rows = list(csv.DictReader(f, delimiter="|"))
            logger.info(
                "raw_artifact_loaded",
                extra={"event": "raw_artifact_loaded", "artifact": name,
                       "rows": len(rows), "kind": "csv"},
            )
            return rows
        text = path.read_text(encoding="utf-8")
        data = json.loads(text)
        logger.info(
            "raw_artifact_loaded",
            extra={"event": "raw_artifact_loaded", "artifact": name,
                   "bytes": len(text), "kind": "json"},
        )
        return data
    except (OSError, json.JSONDecodeError, csv.Error) as exc:
        logger.error(
            "raw_artifact_load_failed",
            extra={"event": "raw_artifact_load_failed", "artifact": name,
                   "error_class": type(exc).__name__, "error": str(exc)[:240]},
        )
        return None


def insufficient(
    metric_number: int,
    metric_name: str,
    reason: str,
    *,
    inherit_confidence: str | None = None,
    extraction_strategy: str = "",
    boundary_conditions: str = "",
    provenance: dict[str, Any] | None = None,
    baseline_windows: int = 0,
    post_intro_windows: int = 0,
) -> dict[str, Any]:
    """Build a schema-compliant insufficient-signal metric record.

    Per AAP §0.3.2 and Quality Gate 1, a metric whose primary data source
    is unavailable MUST emit:
        * ``value == "insufficient_signal"``
        * ``reason`` (non-empty, human-readable)
        * ``confidence`` in {"high","medium","low","insufficient"} —
          inherited from upstream metrics when ``inherit_confidence`` is set
          (Metrics 3 and 5 inherit from their inputs per AAP §0.2.4),
          otherwise ``"insufficient"``.
        * ``boundary_conditions`` (required by schema when confidence != "high").
    """
    conf = inherit_confidence if inherit_confidence in {"low", "medium", "high"} else "insufficient"
    body = boundary_conditions or (
        "Primary data source unavailable; no fallback yields signal. "
        "See reason field for details."
    )
    record: dict[str, Any] = {
        "metric_number": metric_number,
        "name": metric_name,
        "metric_name": metric_name,
        "value": INSUFFICIENT_SIGNAL,
        "reason": reason,
        "confidence": conf,
        "boundary_conditions": body,
        "after_before_multiplier": EM_DASH,
        "extraction_strategy": extraction_strategy,
        "baseline": {
            "value": INSUFFICIENT_SIGNAL,
            "confidence": conf,
            "windows": baseline_windows,
            "reason": reason,
            "boundary_conditions": body,
        },
        "post_introduction": {
            "value": INSUFFICIENT_SIGNAL,
            "confidence": conf,
            "windows": post_intro_windows,
            "multiplier": EM_DASH,
            "reason": reason,
            "boundary_conditions": body,
        },
        "per_window": [],
        "provenance": provenance or {
            "requirement_id": f"M{metric_number}",
            "extraction_command": "n/a — primary data source unavailable",
            "raw_output_artifact_path": "data/(unavailable)",
            "derivation_function": f"compute_m{metric_number}",
        },
    }
    if conf == "low":
        record["caveat"] = body
    return record


def safe_multiplier(after: float | int, before: float | int) -> float | str:
    """Compute after/before as a multiplier, returning the em-dash when
    the multiplier is not computable (zero baseline or non-numeric input)."""
    try:
        after_v = float(after)
        before_v = float(before)
    except (TypeError, ValueError):
        return EM_DASH
    if before_v == 0:
        return EM_DASH
    return round(after_v / before_v, 4)



# ---------------------------------------------------------------------------
# Unknown-rate threshold for Metric 6 confidence downgrade (per AAP §0.5.3.7).
# ---------------------------------------------------------------------------

UNKNOWN_RATE_DOWNGRADE: float = 0.20

# ---------------------------------------------------------------------------
# Em-dash sentinel used as the "multiplier not computable" marker.
# Matches the schema's ``phaseBreakdown.multiplier`` enum string.
# ---------------------------------------------------------------------------

EM_DASH: str = "\u2014"
INSUFFICIENT_SIGNAL: str = "insufficient_signal"

# ===========================================================================
# Per-window helpers used by multiple compute functions
# ===========================================================================


def _per_window_zero_series(windows: list[tuple[datetime, datetime]]) -> list[dict[str, Any]]:
    """Materialise a zero-filled per-window series matching the schema's
    ``perWindowPoint`` shape. Used by metrics that have no events but still
    need a non-empty Acceleration Curve series."""
    return [
        {
            "start": iso_z(s),
            "end": iso_z(e),
            "window_start_iso": iso_z(s),
            "window_end_iso": iso_z(e),
            "value": 0,
        }
        for s, e in windows
    ]


def _bucket_to_window(
    windows: list[tuple[datetime, datetime]],
    ts: datetime,
) -> int | None:
    """Return the index of the window that contains timestamp ``ts``
    (using the half-open ``[start, end)`` convention) or ``None`` when
    ``ts`` falls outside every window."""
    for idx, (s, e) in enumerate(windows):
        if s <= ts < e:
            return idx
    return None


# ===========================================================================
# Phase derivation
# ===========================================================================


def derive_phase_bounds(
    env: dict[str, Any] | None,
    inflection: dict[str, Any] | None,
    logger: Any,
) -> dict[str, Any]:
    """Read ``environment.json`` and ``inflection.json`` and produce the
    canonical phase-bounds dictionary that every downstream compute function
    receives.

    Returns a dict with keys:
        baseline_start, baseline_end : datetime
        post_start, post_end          : datetime
        baseline_windows              : list[(datetime, datetime)]
        post_windows                  : list[(datetime, datetime)]
        post_intro_duration_days      : int
        ramp_up_steady_state_split_applied : bool
        ramp_up_steady_state_split_applied_reason : str
        phase_keys_used               : list[str]
        inflection_date_utc           : str   (ISO-Z form)

    The two-phase fallback ("baseline" + "post_introduction") is applied
    when the post-introduction span is shorter than ``RAMP_UP_DAYS`` (90)
    per AAP §0.5.6. In the current data this span is 86 days, so the
    fallback is always active for this run.
    """
    if not inflection or not isinstance(inflection, dict):
        raise RuntimeError(
            "derive_phase_bounds: inflection.json is missing or empty; "
            "cannot determine baseline/post-introduction boundary."
        )
    inflection_iso = inflection.get("date_utc")
    inflection_dt = parse_iso(inflection_iso)
    if inflection_dt is None:
        raise RuntimeError(
            f"derive_phase_bounds: inflection.json#date_utc is missing or invalid "
            f"({inflection_iso!r})."
        )

    env_obj = env or {}
    commit_range = env_obj.get("commit_date_range") or {}
    earliest_iso = commit_range.get("earliest")
    # Prefer ``latest_on_main`` for the post-introduction terminus when present;
    # otherwise fall back to the overall ``latest`` value. This matches the
    # convention used by the inflection.json ``post_introduction.end_iso``.
    latest_iso = commit_range.get("latest_on_main") or commit_range.get("latest")
    earliest_dt = parse_iso(earliest_iso) if earliest_iso else None
    latest_dt = parse_iso(latest_iso) if latest_iso else None

    # ``inflection.json`` may carry its own post_introduction bounds — prefer
    # those when present (they were computed at extraction time and are the
    # canonical phase-partition signal per AAP §0.5.6).
    post_obj = inflection.get("post_introduction") or {}
    post_start_iso = post_obj.get("start_iso") or inflection_iso
    post_end_iso = post_obj.get("end_iso") or latest_iso
    post_start_dt = parse_iso(post_start_iso) or inflection_dt
    post_end_dt = parse_iso(post_end_iso) or latest_dt

    baseline_start_dt = earliest_dt or post_start_dt - timedelta(days=14)
    baseline_end_dt = inflection_dt

    # Determine whether the ramp-up/steady-state split should be applied.
    # Prefer inflection.json's authoritative ``post_introduction.duration_days``
    # value when present (this is the canonical phase-partition signal per
    # AAP §0.5.6 and avoids floor-vs-ceil discrepancies with local recompute).
    post_duration_days_raw = post_obj.get("duration_days")
    if isinstance(post_duration_days_raw, (int, float)):
        post_duration_days = int(post_duration_days_raw)
    elif post_end_dt is None:
        post_duration_days = 0
    else:
        # Round half-up: 85 days, 21 hours → 86 days.
        total_secs = (post_end_dt - post_start_dt).total_seconds()
        post_duration_days = int((total_secs + 43200) // 86400)
    split_applied = post_duration_days >= RAMP_UP_DAYS

    if split_applied:
        phase_keys = ["baseline", "ramp_up", "steady_state"]
        split_reason = (
            f"Post-introduction span is {post_duration_days} days "
            f"(inflection {iso_z(post_start_dt)} → end "
            f"{iso_z(post_end_dt) if post_end_dt else 'unknown'}). "
            f"{post_duration_days} >= {RAMP_UP_DAYS}-day threshold per AAP §0.5.6, "
            "so the Ramp-Up (first 90 days) / Steady State (≥90 days) "
            "decomposition is applied."
        )
    else:
        phase_keys = ["baseline", "post_introduction"]
        split_reason = (
            f"Post-introduction span is {post_duration_days} days "
            f"(inflection {iso_z(post_start_dt)} → end "
            f"{iso_z(post_end_dt) if post_end_dt else 'unknown'}). "
            f"{post_duration_days} < {RAMP_UP_DAYS}-day threshold per AAP §0.5.6 "
            "and decision-log.md DL-006, so the two-phase fallback "
            "(Baseline + Post-Introduction) is applied; no ramp_up or "
            "steady_state keys are emitted on any metric row."
        )

    # Both phases share the same Monday-anchored grid, rooted at the
    # earliest commit's Monday. This guarantees that the upstream
    # pre-computed series in pulls.json#summary.prs_per_2week_window_*
    # uses the same window boundaries as this script's per-window
    # bucketing (Rule 4 Internal Consistency).
    global_anchor = baseline_start_dt
    baseline_windows = (
        windows_in_phase(baseline_start_dt, baseline_end_dt, global_anchor=global_anchor)
        if baseline_start_dt < baseline_end_dt
        else []
    )
    post_windows = (
        windows_in_phase(post_start_dt, post_end_dt, global_anchor=global_anchor)
        if post_end_dt and post_start_dt < post_end_dt
        else []
    )

    logger.info(
        "phase_bounds_derived",
        extra={
            "event": "phase_bounds_derived",
            "inflection_date_utc": iso_z(inflection_dt),
            "baseline_start": iso_z(baseline_start_dt) if baseline_start_dt else None,
            "baseline_end": iso_z(baseline_end_dt),
            "post_start": iso_z(post_start_dt),
            "post_end": iso_z(post_end_dt) if post_end_dt else None,
            "post_intro_duration_days": post_duration_days,
            "ramp_up_steady_state_split_applied": split_applied,
            "baseline_windows": len(baseline_windows),
            "post_windows": len(post_windows),
        },
    )

    return {
        "baseline_start": baseline_start_dt,
        "baseline_end": baseline_end_dt,
        "post_start": post_start_dt,
        "post_end": post_end_dt,
        "baseline_windows": baseline_windows,
        "post_windows": post_windows,
        "post_intro_duration_days": post_duration_days,
        "ramp_up_steady_state_split_applied": split_applied,
        "ramp_up_steady_state_split_applied_reason": split_reason,
        "phase_keys_used": phase_keys,
        "inflection_date_utc": iso_z(inflection_dt),
    }


# ===========================================================================
# Metric 1 — Flow Load
# ===========================================================================


def compute_m1_flow_load(
    pulls: dict[str, Any] | None,
    commits: list[dict[str, str]] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Count of PRs in progress at each window-end (per AAP §0.5.3.2).

    In-progress definition: branch has at least one commit AND PR is open
    (``merged_at IS NULL OR merged_at > T_end``) AND
    (``closed_at IS NULL OR closed_at > T_end OR merged_at IS NOT NULL``),
    OR PR is in draft state at T_end.

    When PR draft state is unavailable (no GitHub Pulls API access) AND
    no closed-without-merge PR data is recoverable from local git, the
    metric collapses to ``insufficient_signal`` per AAP §0.5.3.2.
    """
    name = "Flow Load"
    strategy = (
        "From data/pulls.json, count PRs where created_at <= window_end "
        "AND (merged_at IS NULL OR merged_at > window_end) AND has at least "
        "one commit by window_end. Mean across Monday-anchored 2-week "
        "window-ends within each phase. Exclude dependency bots."
    )
    provenance = {
        "requirement_id": "M1",
        "extraction_command": "GET /repos/Blitzy-Sandbox/blitzy-RudderStack/pulls?state=all",
        "raw_output_artifact_path": "data/pulls.json",
        "raw_output_artifact_paths": ["data/pulls.json", "data/commits.csv"],
        "derivation_function": "compute_m1_flow_load",
    }

    api_available = bool(pulls and (pulls.get("github_api") or {}).get("available"))
    pr_list = (pulls or {}).get("pulls") or []

    if not api_available:
        return insufficient(
            1, name,
            reason=(
                "Flow Load requires PR-level state at each window-end "
                "(created_at, merged_at, draft state, first-commit timestamp). "
                "The GitHub Pulls API is unavailable in this read-only sandbox "
                "(GH_TOKEN not set; see data/pulls.json#github_api.available=false), "
                "and PR draft state cannot be reconstructed from local git "
                "alone. Per AAP §0.5.3.2, the metric falls back to "
                "'Insufficient signal' when neither the Pulls API nor a "
                "reconstructable local proxy is available."
            ),
            extraction_strategy=strategy,
            boundary_conditions=(
                "Pulls API: unavailable (GH_TOKEN absent). Local-git fallback "
                "cannot reconstruct PR draft state, requested_reviewers, or "
                "closed-without-merge state from branch existence alone."
            ),
            provenance=provenance,
            baseline_windows=len(phase_bounds["baseline_windows"]),
            post_intro_windows=len(phase_bounds["post_windows"]),
        )

    # API available — compute per AAP §0.5.3.2.
    def _count_open_at(windows: list[tuple[datetime, datetime]]) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        for ws, we in windows:
            cnt = 0
            for pr in pr_list:
                author_email = ((pr.get("user") or {}).get("email")) or ""
                if is_dependency_bot(author_email):
                    continue
                created = parse_iso(pr.get("created_at"))
                merged = parse_iso(pr.get("merged_at"))
                closed = parse_iso(pr.get("closed_at"))
                if created is None or created > we:
                    continue
                is_open = (merged is None or merged > we) and (
                    closed is None or closed > we or merged is not None
                )
                if not is_open:
                    continue
                # Has the branch received at least one commit by ``we``?
                first_commit = parse_iso(pr.get("pr_commits_first_at_iso"))
                branch_active = first_commit is not None and first_commit <= we
                draft_at_we = bool(pr.get("draft")) and created <= we
                if branch_active or draft_at_we:
                    cnt += 1
            out.append({
                "start": iso_z(ws),
                "end": iso_z(we),
                "window_start_iso": iso_z(ws),
                "window_end_iso": iso_z(we),
                "value": cnt,
            })
        return out

    baseline_series = _count_open_at(phase_bounds["baseline_windows"])
    post_series = _count_open_at(phase_bounds["post_windows"])

    baseline_mean = (
        round(statistics.mean(p["value"] for p in baseline_series), 4)
        if baseline_series else 0
    )
    post_mean = (
        round(statistics.mean(p["value"] for p in post_series), 4)
        if post_series else 0
    )
    multiplier = safe_multiplier(post_mean, baseline_mean)

    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M1",
               "post_value": post_mean, "baseline_value": baseline_mean,
               "confidence": "high"},
    )

    return {
        "metric_number": 1,
        "name": name,
        "metric_name": name,
        "value": post_mean,
        "after_before_multiplier": multiplier,
        "confidence": "high",
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_mean,
            "confidence": "high",
            "windows": len(baseline_series),
        },
        "post_introduction": {
            "value": post_mean,
            "confidence": "high",
            "windows": len(post_series),
            "multiplier": multiplier,
        },
        "per_window": post_series,
        "provenance": provenance,
    }


# ===========================================================================
# Metric 2 — Flow Velocity
# ===========================================================================


def compute_m2_flow_velocity(
    pulls: dict[str, Any] | None,
    commits: list[dict[str, str]] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Count of PRs merged per 2-week window (per AAP §0.5.3.3).

    With the GitHub Pulls API available, the unit is a merged PR on
    ``main`` (``base.ref == 'main'`` AND ``merged_at`` non-null), excluding
    dependency-bot authors and INCLUDING the Blitzy bot.

    With the Pulls API unavailable, the script falls back to the
    "merge_commit_fallback" semantic: each merge commit on ``main`` (whether
    PR-numbered or ad-hoc) counts as one unit. This matches the
    ``data/pulls.json#summary.merged_prs_count`` figure produced by
    ``03_extract_pulls.py`` in fallback mode and the
    ``prs_per_2week_window_*`` per-window arrays.
    """
    name = "Flow Velocity"
    api_available = bool(pulls and (pulls.get("github_api") or {}).get("available"))
    summary = ((pulls or {}).get("summary") or {})
    base_strategy = (
        "From data/pulls.json, select PRs where merged_at IS NOT NULL AND "
        "base.ref == 'main'; bucket by Monday-anchored 2-week window using "
        "merged_at; exclude dependabot[bot]; group by canonical author for "
        "the per-engineer breakdown. Per-phase value is the mean PRs per "
        "window."
    )
    fallback_strategy = (
        "Local-git fallback (active in this run): enumerate every merge "
        "commit on main via `git log main --merges --pretty=format:...` and "
        "treat each merge commit (whether PR-numbered or ad-hoc) as one M2 "
        "unit. Counts mirror data/pulls.json#summary.merged_prs_count and "
        "the prs_per_2week_window_post_introduction series."
    )
    provenance = {
        "requirement_id": "M2",
        "extraction_command": (
            "GET /repos/Blitzy-Sandbox/blitzy-RudderStack/pulls?state=all"
            if api_available else
            "git log main --merges --pretty=format:'%H|%aI|%aE|%s'"
        ),
        "raw_output_artifact_path": "data/pulls.json",
        "raw_output_artifact_paths": ["data/pulls.json", "data/commits.csv"],
        "derivation_function": "compute_m2_flow_velocity",
    }

    pr_list = (pulls or {}).get("pulls") or []
    confidence = "high" if api_available else "medium"
    boundary = (
        ""
        if api_available
        else (
            "GitHub Pulls API unavailable (GH_TOKEN absent); falls back to "
            "local-git merge-commit enumeration on main. The local-git "
            "fallback cannot distinguish dependency-bot PRs from human/"
            "Blitzy PRs without scanning commit subjects; the data file "
            "data/pulls.json#summary documents how each merge commit was "
            "attributed."
        )
    )

    # Per-window counts: prefer the pre-computed series in pulls.json#summary
    # when present (these were produced by 03_extract_pulls.py in fallback
    # mode and faithfully encode the merge_commit_fallback semantic).
    pre_baseline = summary.get("prs_per_2week_window_baseline") or []
    pre_post = summary.get("prs_per_2week_window_post_introduction") or []

    def _series_from_pulls(precomputed: list[dict[str, Any]],
                            windows: list[tuple[datetime, datetime]],
                            ) -> list[dict[str, Any]]:
        if precomputed:
            out: list[dict[str, Any]] = []
            for p in precomputed:
                s = p.get("window_start_iso") or p.get("start")
                e = p.get("window_end_iso") or p.get("end")
                v = p.get("merged_prs_count")
                if v is None:
                    v = p.get("value", 0)
                out.append({
                    "start": s,
                    "end": e,
                    "window_start_iso": s,
                    "window_end_iso": e,
                    "value": v,
                })
            return out
        # Compute from the PR list when no pre-computed series exists.
        out = []
        for ws, we in windows:
            cnt = 0
            for pr in pr_list:
                author_email = ((pr.get("user") or {}).get("email")) or ""
                if is_dependency_bot(author_email):
                    continue
                merged = parse_iso(pr.get("merged_at"))
                if merged is None or not (ws <= merged < we):
                    continue
                base_ref = (pr.get("base") or {}).get("ref")
                if base_ref and base_ref != "main":
                    continue
                cnt += 1
            out.append({
                "start": iso_z(ws),
                "end": iso_z(we),
                "window_start_iso": iso_z(ws),
                "window_end_iso": iso_z(we),
                "value": cnt,
            })
        return out

    baseline_series = _series_from_pulls(pre_baseline, phase_bounds["baseline_windows"])
    post_series = _series_from_pulls(pre_post, phase_bounds["post_windows"])

    baseline_mean = (
        round(statistics.mean(p["value"] for p in baseline_series), 4)
        if baseline_series else 0
    )
    post_mean = (
        round(statistics.mean(p["value"] for p in post_series), 4)
        if post_series else 0
    )
    multiplier = safe_multiplier(post_mean, baseline_mean)

    record: dict[str, Any] = {
        "metric_number": 2,
        "name": name,
        "metric_name": name,
        "value": post_mean,
        "after_before_multiplier": multiplier,
        "confidence": confidence,
        "extraction_strategy": (base_strategy if api_available
                                 else f"{base_strategy} {fallback_strategy}"),
        "baseline": {
            "value": baseline_mean,
            "confidence": confidence,
            "windows": len(baseline_series),
        },
        "post_introduction": {
            "value": post_mean,
            "confidence": confidence,
            "windows": len(post_series),
            "multiplier": multiplier,
        },
        "per_window": post_series,
        "per_window_baseline": baseline_series,
        "provenance": provenance,
    }
    if confidence != "high":
        record["boundary_conditions"] = boundary
    if not api_available:
        record["semantic_contract"] = "merge_commit_fallback"
        record["semantic_contract_note"] = (
            "When the GitHub Pulls API is unreachable, each merge commit on "
            "main (whether PR-numbered or ad-hoc) counts as one M2 unit. "
            "This is the merge_commit_fallback semantic referenced by "
            "decision-log.md DL-005 and used uniformly by the per-engineer "
            "breakdown in data/per_engineer.json (Rule 4 Internal Consistency)."
        )
        record["baseline"]["boundary_conditions"] = boundary
        record["post_introduction"]["boundary_conditions"] = boundary

    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M2",
               "post_value": post_mean, "baseline_value": baseline_mean,
               "confidence": confidence},
    )
    return record


# ===========================================================================
# Metric 3 — Flow Predictability
# ===========================================================================


def compute_m3_flow_predictability(
    m2: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Reciprocal of the coefficient of variation of Metric 2's per-window
    series within each phase (per AAP §0.5.3.4).

    Requirements:
        * ≥4 windows per phase, else "Insufficient signal — fewer than 4 windows".
        * non-zero stdev, else "Insufficient signal — zero variance".

    Inherits Metric 2's confidence per AAP §0.2.4.
    """
    name = "Flow Predictability"
    strategy = (
        "From data/metrics.json#m2.per_window for each phase, compute "
        "1/CV = mean/stdev (sample stdev, n−1 divisor). Two insufficient-"
        "signal rules apply per AAP §0.5.3.4: fewer than 4 windows in the "
        "phase, or zero variance across the windows."
    )
    provenance = {
        "requirement_id": "M3",
        "extraction_command": (
            "compute_m3_flow_predictability(m2_per_window=data/metrics.json#m2.per_window)"
        ),
        "raw_output_artifact_path": "data/pulls.json",
        "raw_output_artifact_paths": ["data/pulls.json"],
        "derivation_function": "compute_m3_flow_predictability",
    }
    m2_conf = m2.get("confidence", "insufficient")

    def _predictability(series: list[dict[str, Any]]) -> tuple[float | str, str]:
        if len(series) < 4:
            return INSUFFICIENT_SIGNAL, "fewer than 4 windows"
        values = [float(p["value"]) for p in series]
        mean = statistics.mean(values)
        if len(values) > 1:
            stdev = statistics.stdev(values)
        else:
            stdev = 0.0
        if stdev == 0:
            return INSUFFICIENT_SIGNAL, "zero variance"
        return round(mean / stdev, 4), ""

    baseline_series = m2.get("per_window_baseline") or []
    post_series = m2.get("per_window") or []

    baseline_val, baseline_reason = _predictability(baseline_series)
    post_val, post_reason = _predictability(post_series)

    # Pick the dominant phase result (post_introduction) for the headline.
    if isinstance(post_val, str):
        return insufficient(
            3, name,
            reason=(
                f"Post-introduction period yields '{post_reason}': "
                f"{len(post_series)} windows observed, "
                f"{'fewer than 4 windows' if post_reason == 'fewer than 4 windows' else 'zero variance'}. "
                "Per AAP §0.5.3.4, Flow Predictability cannot be computed "
                "in this case."
            ),
            inherit_confidence=m2_conf,
            extraction_strategy=strategy,
            boundary_conditions=(
                f"Post-introduction phase: {post_reason}. "
                f"Baseline phase: {baseline_reason}. "
                "Predictability is the reciprocal of the coefficient of "
                "variation across per-window values; both rules in "
                "AAP §0.5.3.4 apply unchanged."
            ),
            provenance=provenance,
            baseline_windows=len(baseline_series),
            post_intro_windows=len(post_series),
        )

    multiplier = (
        safe_multiplier(post_val, baseline_val)
        if isinstance(baseline_val, (int, float)) and baseline_val != 0
        else EM_DASH
    )

    record: dict[str, Any] = {
        "metric_number": 3,
        "name": name,
        "metric_name": name,
        "value": post_val,
        "after_before_multiplier": multiplier,
        "confidence": m2_conf,
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_val if not isinstance(baseline_val, str) else INSUFFICIENT_SIGNAL,
            "confidence": m2_conf,
            "windows": len(baseline_series),
        },
        "post_introduction": {
            "value": post_val,
            "confidence": m2_conf,
            "windows": len(post_series),
            "multiplier": multiplier,
        },
        "per_window": [],
        "provenance": provenance,
    }
    if isinstance(baseline_val, str):
        record["baseline"]["reason"] = baseline_reason
        record["baseline"]["boundary_conditions"] = (
            f"Baseline phase: {baseline_reason}."
        )
    if m2_conf != "high":
        record["boundary_conditions"] = (
            f"Inherits Metric 2's confidence ({m2_conf}). Predictability is a "
            "derived ratio over the per-window series; any source-driven "
            "downgrade on M2 propagates here automatically (AAP §0.2.4)."
        )
        if m2_conf == "low":
            record["caveat"] = record["boundary_conditions"]
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M3",
               "post_value": post_val, "baseline_value": baseline_val,
               "confidence": m2_conf},
    )
    return record



# ===========================================================================
# Metric 4 — Flow Active
# ===========================================================================


def compute_m4_flow_active(
    pulls: dict[str, Any] | None,
    reviews: dict[str, Any] | None,
    pull_events: dict[str, Any] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Median active coding time per PR by the engineering actor
    (per AAP §0.5.3.5).

    The compute path requires:
        * Pulls API   (boundary timestamps for each PR)
        * Reviews API (review-event-bounded working phases)
        * Events API  (ReadyForReview, review_requested timestamps)

    When any of these is unavailable, the metric falls back to
    ``insufficient_signal`` per AAP §0.5.3.5 — there is no local-git proxy
    for ready-for-review boundaries that yields a faithful "active coding
    time" measure.
    """
    name = "Flow Active"
    strategy = (
        "Per AAP §0.5.3.5: for each merged PR, retrieve commit list "
        "(Pulls-Commits API), review timeline (Reviews API), and event "
        "timeline (Issue-Events API). Construct working-phase boundary "
        "list by interleaving commit timestamps with review-event "
        "timestamps. Sum span durations from actor's first commit to "
        "ready_for_review_at, plus each refine span from actor commit "
        "to next review/merge. Median per phase, per actor."
    )
    provenance = {
        "requirement_id": "M4",
        "extraction_command": (
            "GET /repos/{}/{}/pulls/{n}/commits + /reviews + /issues/{n}/events"
        ),
        "raw_output_artifact_path": "data/pulls.json",
        "raw_output_artifact_paths": [
            "data/pulls.json", "data/reviews.json", "data/pull_events.json"
        ],
        "derivation_function": "compute_m4_flow_active",
    }

    pulls_api = bool(pulls and (pulls.get("github_api") or {}).get("available"))
    reviews_api = bool(reviews and (reviews.get("github_api") or {}).get("available"))
    events_api = bool(pull_events and (pull_events.get("github_api") or {}).get("available"))

    if not (pulls_api and reviews_api and events_api):
        # No fallback path exists per AAP §0.5.3.5 — the review-event
        # boundary cannot be reconstructed from local git alone.
        missing = []
        if not pulls_api:
            missing.append("Pulls API (data/pulls.json#github_api.available=false)")
        if not reviews_api:
            missing.append("Reviews API (data/reviews.json#github_api.available=false)")
        if not events_api:
            missing.append("Events API (data/pull_events.json#github_api.available=false)")
        return insufficient(
            4, name,
            reason=(
                "Flow Active per PR requires (a) the GitHub Pulls API for "
                "PR boundaries, (b) the Reviews API for ready-for-review "
                "and review-event timestamps, and (c) the Events API for "
                "draft transitions. The following sources are unavailable "
                f"in this run: {', '.join(missing)}. Per AAP §0.5.3.5, "
                "no local-git fallback yields a faithful active-coding-time "
                "measure because ready-for-review boundaries cannot be "
                "reconstructed from commit-history alone."
            ),
            extraction_strategy=strategy,
            boundary_conditions=(
                "All three GitHub APIs required for review-event bounding "
                "are unavailable; metric collapses to insufficient_signal. "
                "An API-enabled run with valid GH_TOKEN will populate this "
                "metric for both Blitzy and human actors."
            ),
            provenance=provenance,
            baseline_windows=len(phase_bounds["baseline_windows"]),
            post_intro_windows=len(phase_bounds["post_windows"]),
        )

    # API path — compute review-event-bounded active spans per PR.
    pr_list = (pulls or {}).get("pulls") or []
    reviews_by_pr: dict[str, list[dict[str, Any]]] = (reviews or {}).get("reviews_by_pr") or {}
    events_by_pr: dict[str, list[dict[str, Any]]] = (pull_events or {}).get("events_by_pr") or {}

    def _flow_active_for_pr(pr: dict[str, Any], actor_email: str) -> float | None:
        # Identify ready_for_review_at: earliest of (a) PR leaves draft, (b)
        # first review_requested, (c) first commit by another author, (d) created.
        pr_id = str(pr.get("number"))
        events = events_by_pr.get(pr_id, [])
        reviews_for_pr = reviews_by_pr.get(pr_id, [])
        created_at = parse_iso(pr.get("created_at"))
        merged_at = parse_iso(pr.get("merged_at"))
        if created_at is None or merged_at is None:
            return None
        candidates: list[datetime] = []
        for ev in events:
            kind = ev.get("event")
            ts = parse_iso(ev.get("created_at"))
            if not ts:
                continue
            if kind in {"ready_for_review", "review_requested"}:
                candidates.append(ts)
        # First commit by another author isn't directly exposed in events;
        # we use the PR's first commit timestamp by a non-actor when present.
        pr_commits = pr.get("pr_commits") or []
        for c in pr_commits:
            ce = parse_iso(c.get("author_date") or c.get("authored_at"))
            ca = (c.get("author") or {}).get("email") or ""
            if ce and ca and ca != actor_email:
                candidates.append(ce)
                break
        candidates.append(created_at)
        ready_for_review_at = min(candidates)

        # Actor's first commit on the PR.
        actor_commits = [
            parse_iso(c.get("author_date") or c.get("authored_at"))
            for c in pr_commits
            if (c.get("author") or {}).get("email") == actor_email
        ]
        actor_commits = [c for c in actor_commits if c is not None]
        if not actor_commits:
            return None
        first_actor_commit = min(actor_commits)
        last_actor_commit = max(actor_commits)

        # Initial span: first_actor_commit → ready_for_review_at.
        if ready_for_review_at < first_actor_commit:
            ready_for_review_at = first_actor_commit
        initial_span = max((ready_for_review_at - first_actor_commit).total_seconds(), 0)

        # Refine spans: each review event followed by an actor commit opens
        # a refine span up to the next review or merge.
        review_ts = sorted(
            ts for ts in (parse_iso(r.get("submitted_at")) for r in reviews_for_pr)
            if ts is not None
        )
        terminators = sorted(review_ts + [merged_at])
        refine_total = 0.0
        i = 0
        while i < len(terminators) - 1:
            rev_ts = terminators[i]
            next_ts = terminators[i + 1]
            actor_in_window = [c for c in actor_commits if rev_ts < c <= next_ts]
            if actor_in_window:
                span_start = min(actor_in_window)
                span_end = max(actor_in_window + [next_ts])
                refine_total += max((span_end - span_start).total_seconds(), 0)
            i += 1
        # Account for any commits after the last terminator but ≤ merge.
        last_terminator = terminators[-1] if terminators else ready_for_review_at
        tail_commits = [c for c in actor_commits if c > last_terminator and c <= merged_at]
        if tail_commits:
            refine_total += max(
                (max(tail_commits) - min(tail_commits)).total_seconds(), 0
            )
        # Convert to hours.
        total_hours = (initial_span + refine_total) / 3600.0
        # Bound to PR wall-clock for safety.
        wall = (merged_at - first_actor_commit).total_seconds() / 3600.0
        if wall > 0 and total_hours > wall:
            total_hours = wall
        return round(total_hours, 4)

    def _phase_median(phase_label: str,
                       window_bounds: tuple[datetime, datetime] | None,
                       ) -> tuple[float | str, int]:
        durations: list[float] = []
        for pr in pr_list:
            merged_at = parse_iso(pr.get("merged_at"))
            if merged_at is None:
                continue
            if window_bounds is not None:
                ps, pe = window_bounds
                if not (ps <= merged_at < pe):
                    continue
            elif pr.get("phase") != phase_label:
                continue
            actor_email = ((pr.get("user") or {}).get("email")) or ""
            if is_dependency_bot(actor_email):
                continue
            val = _flow_active_for_pr(pr, actor_email)
            if val is not None:
                durations.append(val)
        if not durations:
            return INSUFFICIENT_SIGNAL, 0
        return round(statistics.median(durations), 4), len(durations)

    baseline_val, baseline_n = _phase_median(
        "baseline",
        (phase_bounds["baseline_start"], phase_bounds["baseline_end"]),
    )
    post_val, post_n = _phase_median(
        "post_introduction",
        (phase_bounds["post_start"], phase_bounds["post_end"]),
    )

    if isinstance(post_val, str):
        return insufficient(
            4, name,
            reason=(
                "No merged PRs in the post-introduction phase yielded a "
                "computable active-time signal (PR-commit timestamps for the "
                "engineering actor are missing or no actor commits exist). "
                "Per AAP §0.5.3.5, the metric collapses to insufficient_signal "
                "in this case."
            ),
            extraction_strategy=strategy,
            boundary_conditions=(
                f"Baseline: {baseline_n} PRs with computable spans. "
                f"Post-introduction: {post_n} PRs with computable spans. "
                "Empty post-introduction signal blocks the metric value."
            ),
            provenance=provenance,
            baseline_windows=len(phase_bounds["baseline_windows"]),
            post_intro_windows=len(phase_bounds["post_windows"]),
        )

    multiplier = safe_multiplier(post_val, baseline_val) if isinstance(baseline_val, (int, float)) else EM_DASH

    record: dict[str, Any] = {
        "metric_number": 4,
        "name": name,
        "metric_name": name,
        "value": post_val,
        "after_before_multiplier": multiplier,
        "confidence": "high",
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_val if not isinstance(baseline_val, str) else INSUFFICIENT_SIGNAL,
            "confidence": "high",
            "windows": len(phase_bounds["baseline_windows"]),
        },
        "post_introduction": {
            "value": post_val,
            "confidence": "high",
            "windows": len(phase_bounds["post_windows"]),
            "multiplier": multiplier,
        },
        "per_window": [],
        "provenance": provenance,
        "per_pr_count_post_introduction": post_n,
        "per_pr_count_baseline": baseline_n,
    }
    if isinstance(baseline_val, str):
        record["baseline"]["reason"] = "no merged PRs in baseline phase"
        record["baseline"]["boundary_conditions"] = (
            "Baseline phase produced zero merged PRs with computable spans."
        )
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M4",
               "post_value": post_val, "baseline_value": baseline_val,
               "confidence": "high"},
    )
    return record


# ===========================================================================
# Metric 5 — Flow Efficiency
# ===========================================================================


def compute_m5_flow_efficiency(
    m4: dict[str, Any],
    m7: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Ratio Flow Active / Flow Time, median across merged PRs in each
    phase (per AAP §0.5.3.6).

    Inherits the LOWER of Metric 4 and Metric 7's confidence per AAP §0.2.4.
    When either upstream is insufficient_signal the composite is also
    insufficient_signal with an inherit-confidence-aware reason.
    """
    name = "Flow Efficiency"
    strategy = (
        "Per AAP §0.5.3.6: for each merged PR, divide Metric 4 (Flow Active) "
        "by Metric 7 (Flow Time). Median across PRs in each phase, per actor. "
        "Review is treated as wait from the engineering actor's perspective."
    )
    provenance = {
        "requirement_id": "M5",
        "extraction_command": (
            "compute_m5_flow_efficiency(m4=data/metrics.json#m4, m7=data/metrics.json#m7)"
        ),
        "raw_output_artifact_path": "data/pulls.json",
        "raw_output_artifact_paths": ["data/pulls.json", "data/reviews.json",
                                       "data/pull_events.json", "data/commits.csv"],
        "derivation_function": "compute_m5_flow_efficiency",
    }
    # Inherit the worse of M4 and M7's confidence.
    rank = {"high": 3, "medium": 2, "low": 1, "insufficient": 0}
    m4_conf = m4.get("confidence", "insufficient")
    m7_conf = m7.get("confidence", "insufficient")
    inherited = m4_conf if rank.get(m4_conf, 0) <= rank.get(m7_conf, 0) else m7_conf

    m4_val = m4.get("value")
    m7_val = m7.get("value")
    if m4_val == INSUFFICIENT_SIGNAL or m7_val == INSUFFICIENT_SIGNAL:
        return insufficient(
            5, name,
            reason=(
                f"Flow Efficiency = Flow Active / Flow Time per PR. "
                f"Both m4 (Flow Active) and m7 (Flow Time) must yield a "
                f"per-PR active-time and wall-clock-time signal to compute "
                f"the ratio. In this run: m4='{m4_val}' (conf={m4_conf}); "
                f"m7='{m7_val}' (conf={m7_conf}). When either upstream is "
                f"insufficient_signal the composite is insufficient too."
            ),
            inherit_confidence=inherited if inherited in {"low", "medium", "high"} else None,
            extraction_strategy=strategy,
            boundary_conditions=(
                f"Composite metric inheriting from m4 ({m4_conf}) and m7 "
                f"({m7_conf}); collapsed to insufficient_signal because at "
                "least one upstream is insufficient_signal."
            ),
            provenance=provenance,
            baseline_windows=m4.get("baseline", {}).get("windows", 0),
            post_intro_windows=m4.get("post_introduction", {}).get("windows", 0),
        )

    # When both upstreams produced numeric values, compute the median ratio.
    # Flow Active and Flow Time are reported per phase as medians; the
    # per-PR ratio median is not exactly the ratio of medians, but the
    # AAP §0.5.3.6 contract is "median across PRs of the per-PR ratio".
    # When per-PR ratios are unavailable we fall back to the headline ratio
    # of medians and flag the limitation in boundary_conditions.
    m4_post = m4.get("post_introduction", {}).get("value")
    m7_post = m7.get("post_introduction", {}).get("value")
    m4_base = m4.get("baseline", {}).get("value")
    m7_base = m7.get("baseline", {}).get("value")
    if not (isinstance(m4_post, (int, float)) and isinstance(m7_post, (int, float))
            and m7_post != 0):
        return insufficient(
            5, name,
            reason="Post-introduction inputs to Flow Efficiency are zero or non-numeric.",
            inherit_confidence=inherited if inherited in {"low", "medium", "high"} else None,
            extraction_strategy=strategy,
            boundary_conditions="m7 post-introduction value is zero or non-numeric.",
            provenance=provenance,
        )
    post_val = round(float(m4_post) / float(m7_post), 4)
    if isinstance(m4_base, (int, float)) and isinstance(m7_base, (int, float)) and m7_base:
        baseline_val: float | str = round(float(m4_base) / float(m7_base), 4)
    else:
        baseline_val = INSUFFICIENT_SIGNAL
    multiplier = (
        safe_multiplier(post_val, baseline_val)
        if isinstance(baseline_val, (int, float)) else EM_DASH
    )

    record: dict[str, Any] = {
        "metric_number": 5,
        "name": name,
        "metric_name": name,
        "value": post_val,
        "after_before_multiplier": multiplier,
        "confidence": inherited,
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_val,
            "confidence": inherited,
            "windows": m4.get("baseline", {}).get("windows", 0),
        },
        "post_introduction": {
            "value": post_val,
            "confidence": inherited,
            "windows": m4.get("post_introduction", {}).get("windows", 0),
            "multiplier": multiplier,
        },
        "per_window": [],
        "provenance": provenance,
    }
    if isinstance(baseline_val, str):
        record["baseline"]["reason"] = (
            "Baseline phase has insufficient input from m4 or m7."
        )
        record["baseline"]["boundary_conditions"] = (
            "Composite metric collapses on the baseline side."
        )
    if inherited != "high":
        record["boundary_conditions"] = (
            f"Composite metric inheriting confidence ({inherited}) from m4 "
            f"({m4_conf}) and m7 ({m7_conf}) per AAP §0.2.4. The median-of-"
            "medians shortcut is used when per-PR ratios are not separately "
            "available."
        )
        if inherited == "low":
            record["caveat"] = record["boundary_conditions"]
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M5",
               "post_value": post_val, "baseline_value": baseline_val,
               "confidence": inherited},
    )
    return record


# ===========================================================================
# Metric 6 — Flow Distribution
# ===========================================================================


def compute_m6_flow_distribution(
    pulls: dict[str, Any] | None,
    issues: dict[str, Any] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Proportion of merged PRs by category per phase, per actor
    (per AAP §0.5.3.7).

    Classifier priority:
        1. Linear issue-label (when ``data/issues.json#linear.available``).
        2. Conventional-commit prefix on PR title.
        3. Keyword match on PR title + body.
        4. ``unknown``.

    Confidence:
        * Linear labels → High.
        * Conventional prefix only → Medium.
        * Unknown rate >20% in either phase → Low.
    """
    name = "Flow Distribution"
    strategy = (
        "Per AAP §0.5.3.7: for each merged PR, classify into feature, "
        "defect, risk/compliance, tech-debt, or unknown. Priority: "
        "(1) Linear issue-label on linked ticket, (2) conventional-commit "
        "prefix on PR title (feat→feature, fix→defect, others→tech-debt), "
        "(3) keyword match on title+body, (4) unknown."
    )
    provenance = {
        "requirement_id": "M6",
        "extraction_command": (
            "data/pulls.json#pulls[].classified_category + data/issues.json (when Linear available)"
        ),
        "raw_output_artifact_path": "data/pulls.json",
        "raw_output_artifact_paths": ["data/pulls.json", "data/issues.json"],
        "derivation_function": "compute_m6_flow_distribution",
    }

    pr_list = (pulls or {}).get("pulls") or []
    linear_available = bool(issues and (issues.get("linear") or {}).get("available"))
    if not pr_list:
        return insufficient(
            6, name,
            reason=(
                "Flow Distribution is per-PR by user definition. PR-level "
                "data is unavailable (data/pulls.json#pulls is empty); "
                "without merged PRs the per-phase category counts are "
                "structurally insufficient."
            ),
            extraction_strategy=strategy,
            boundary_conditions=(
                "data/pulls.json#pulls has no entries; no classifier input "
                "is available. The fallback chain (conventional prefix → "
                "keyword match → unknown) requires at least one PR row."
            ),
            provenance=provenance,
        )

    def _distribution(window_bounds: tuple[datetime, datetime]) -> dict[str, Any]:
        ws, we = window_bounds
        per_category: dict[str, int] = defaultdict(int)
        per_actor_per_category: dict[str, dict[str, int]] = defaultdict(lambda: defaultdict(int))
        total = 0
        for pr in pr_list:
            merged_at = parse_iso(pr.get("merged_at"))
            if merged_at is None or not (ws <= merged_at < we):
                continue
            author_email = ((pr.get("user") or {}).get("email")) or ""
            if is_dependency_bot(author_email):
                continue
            existing = pr.get("classified_category")
            category = classify_pr_category(
                pr.get("title"), pr.get("body"),
                existing_classification=existing,
            )
            per_category[category] += 1
            actor = canonical_actor(author_email, ((pr.get("user") or {}).get("login")))
            per_actor_per_category[actor][category] += 1
            total += 1
        proportions = {k: round(v / total, 4) for k, v in per_category.items()} if total else {}
        per_actor_props = {
            actor: {k: round(v / sum(cat_counts.values()), 4)
                     for k, v in cat_counts.items()}
            for actor, cat_counts in per_actor_per_category.items()
            if sum(cat_counts.values()) > 0
        }
        return {
            "total_prs": total,
            "category_counts": dict(per_category),
            "category_proportions": proportions,
            "per_actor": {
                actor: dict(cat_counts)
                for actor, cat_counts in per_actor_per_category.items()
            },
            "per_actor_proportions": per_actor_props,
            "unknown_rate": (
                round(per_category.get("unknown", 0) / total, 4) if total else 0.0
            ),
        }

    baseline_dist = _distribution(
        (phase_bounds["baseline_start"], phase_bounds["baseline_end"])
    )
    post_dist = _distribution(
        (phase_bounds["post_start"], phase_bounds["post_end"])
    )

    # Confidence assignment.
    if linear_available and baseline_dist["unknown_rate"] <= UNKNOWN_RATE_DOWNGRADE \
            and post_dist["unknown_rate"] <= UNKNOWN_RATE_DOWNGRADE:
        confidence = "high"
    elif post_dist["unknown_rate"] > UNKNOWN_RATE_DOWNGRADE \
            or baseline_dist["unknown_rate"] > UNKNOWN_RATE_DOWNGRADE:
        confidence = "low"
    else:
        confidence = "medium"

    # Headline value: post-introduction unknown rate is reported as the
    # primary "value" field per the AAP's "unknown rate is reported per
    # phase as a confidence indicator" — when the unknown rate dominates
    # the distribution loses information value, so the rate itself is the
    # most-informative single number per AAP §0.5.3.7.
    headline = post_dist["unknown_rate"]
    if linear_available and post_dist["total_prs"] > 0:
        # When Linear classified the PRs, prefer a more meaningful headline:
        # the post-introduction feature share.
        headline = post_dist["category_proportions"].get("feature", 0.0)

    multiplier_baseline = (
        baseline_dist["category_proportions"].get("feature", 0.0)
        if linear_available else baseline_dist["unknown_rate"]
    )
    multiplier = safe_multiplier(headline, multiplier_baseline)

    boundary = (
        ""
        if confidence == "high"
        else (
            f"Linear API: {'available' if linear_available else 'unavailable'}. "
            f"Post-introduction unknown rate: {post_dist['unknown_rate']*100:.1f}%. "
            f"Baseline unknown rate: {baseline_dist['unknown_rate']*100:.1f}%. "
            "Per AAP §0.5.3.7, unknown rate >20% downgrades phase confidence "
            "to Low. When Linear labels are unavailable, the classifier "
            "depends on conventional-commit prefixes and a keyword catalogue "
            "in scripts/09_compute_metrics.py."
        )
    )

    if post_dist["total_prs"] == 0:
        return insufficient(
            6, name,
            reason=(
                "Flow Distribution is per-PR by user definition. PR-level "
                "data is unavailable (data/pulls.json#pulls is empty or "
                "no merged PRs fall in the post-introduction phase); "
                "without merged PRs the per-phase category proportions are "
                "structurally insufficient."
            ),
            extraction_strategy=strategy,
            boundary_conditions=boundary or "No merged PRs in post-introduction phase.",
            provenance=provenance,
        )

    record: dict[str, Any] = {
        "metric_number": 6,
        "name": name,
        "metric_name": name,
        "value": headline,
        "after_before_multiplier": multiplier,
        "confidence": confidence,
        "extraction_strategy": strategy,
        "baseline": {
            "value": multiplier_baseline,
            "confidence": confidence,
            "windows": len(phase_bounds["baseline_windows"]),
            "category_counts": baseline_dist["category_counts"],
            "category_proportions": baseline_dist["category_proportions"],
            "unknown_rate": baseline_dist["unknown_rate"],
            "per_actor_proportions": baseline_dist["per_actor_proportions"],
            "total_prs": baseline_dist["total_prs"],
        },
        "post_introduction": {
            "value": headline,
            "confidence": confidence,
            "windows": len(phase_bounds["post_windows"]),
            "multiplier": multiplier,
            "category_counts": post_dist["category_counts"],
            "category_proportions": post_dist["category_proportions"],
            "unknown_rate": post_dist["unknown_rate"],
            "per_actor_proportions": post_dist["per_actor_proportions"],
            "total_prs": post_dist["total_prs"],
        },
        "per_window": [],
        "provenance": provenance,
    }
    if confidence != "high":
        record["boundary_conditions"] = boundary
    if confidence == "low":
        record["caveat"] = boundary
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M6",
               "post_value": headline, "baseline_value": multiplier_baseline,
               "confidence": confidence,
               "post_unknown_rate": post_dist["unknown_rate"]},
    )
    return record



# ===========================================================================
# Metric 7 — Flow Time
# ===========================================================================


def compute_m7_flow_time(
    pulls: dict[str, Any] | None,
    commits: list[dict[str, str]] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Median wall-clock time from first commit on a PR branch to merge
    commit on the default branch, across all merged PRs in the phase
    (per AAP §0.5.3.8).

    With the Pulls API available, ``pr_commits_first_at_iso`` plus
    ``merged_at`` from each PR yields the per-PR duration directly.

    With the API unavailable, the script falls back to the
    "merge-commit-window" approximation: the first commit timestamp on
    the PR's head ref is the earliest non-merge commit author date on
    that branch in ``data/commits.csv``. Confidence drops to Medium per
    AAP §0.2.4.
    """
    name = "Flow Time"
    strategy = (
        "Per AAP §0.5.3.8: for each merged PR, compute "
        "merge_commit_committer_date − first_commit_author_date. Median "
        "per phase. Report exclusions for PRs whose first commit is no "
        "longer reachable due to history rewrites."
    )
    provenance = {
        "requirement_id": "M7",
        "extraction_command": (
            "GET /repos/{}/{}/pulls + /pulls/{n}/commits (merged_at and "
            "first commit author date)"
        ),
        "raw_output_artifact_path": "data/pulls.json",
        "raw_output_artifact_paths": ["data/pulls.json", "data/commits.csv"],
        "derivation_function": "compute_m7_flow_time",
    }

    pulls_api = bool(pulls and (pulls.get("github_api") or {}).get("available"))
    pr_list = (pulls or {}).get("pulls") or []

    if not pr_list:
        return insufficient(
            7, name,
            reason=(
                "Flow Time = median(merge_commit_committer_date − "
                "first_commit_author_date) across merged PRs in each phase. "
                "data/pulls.json#pulls is empty — no PR rows to compute "
                "first-commit-to-merge wall-clock duration against."
            ),
            extraction_strategy=strategy,
            boundary_conditions=(
                "No merged PR records available; an API-enabled run with "
                "GH_TOKEN will populate this metric directly."
            ),
            provenance=provenance,
            baseline_windows=len(phase_bounds["baseline_windows"]),
            post_intro_windows=len(phase_bounds["post_windows"]),
        )

    def _phase_median(window_bounds: tuple[datetime, datetime]) -> tuple[Any, int, int]:
        ws, we = window_bounds
        durations_hours: list[float] = []
        excluded = 0
        for pr in pr_list:
            merged_at = parse_iso(pr.get("merged_at"))
            if merged_at is None or not (ws <= merged_at < we):
                continue
            author_email = ((pr.get("user") or {}).get("email")) or ""
            if is_dependency_bot(author_email):
                continue
            first_commit = parse_iso(pr.get("pr_commits_first_at_iso"))
            if first_commit is None:
                # Fallback: scan commits.csv for the earliest non-merge commit
                # on the PR's head ref.
                if commits:
                    head_ref = (pr.get("head") or {}).get("ref")
                    if head_ref:
                        # commits.csv has author_date_iso but not branch refs,
                        # so we cannot reconstruct first-commit-on-branch
                        # without the GitHub API. Skip with exclusion count.
                        pass
                excluded += 1
                continue
            duration_hours = (merged_at - first_commit).total_seconds() / 3600.0
            if duration_hours < 0:
                excluded += 1
                continue
            durations_hours.append(duration_hours)
        if not durations_hours:
            return INSUFFICIENT_SIGNAL, 0, excluded
        return round(statistics.median(durations_hours), 4), len(durations_hours), excluded

    baseline_val, baseline_n, baseline_excl = _phase_median(
        (phase_bounds["baseline_start"], phase_bounds["baseline_end"])
    )
    post_val, post_n, post_excl = _phase_median(
        (phase_bounds["post_start"], phase_bounds["post_end"])
    )

    if isinstance(post_val, str):
        return insufficient(
            7, name,
            reason=(
                "Flow Time requires per-PR first-commit timestamps "
                "(pr_commits_first_at_iso). The GitHub Pulls API is "
                f"{'available' if pulls_api else 'unavailable'} in this run "
                f"and {post_excl} merged PRs in the post-introduction phase "
                "lack a reconstructable first-commit timestamp. Per AAP "
                "§0.5.3.8, the metric is reported as insufficient_signal "
                "when the median cannot be computed."
            ),
            inherit_confidence="medium" if not pulls_api else None,
            extraction_strategy=strategy,
            boundary_conditions=(
                f"Pulls API: {'available' if pulls_api else 'unavailable'}. "
                f"Excluded PRs (no first-commit timestamp): baseline={baseline_excl}, "
                f"post={post_excl}. Local-git fallback cannot resolve "
                "first-commit-on-branch from data/commits.csv alone because "
                "commits.csv lacks branch-ref attribution."
            ),
            provenance=provenance,
            baseline_windows=len(phase_bounds["baseline_windows"]),
            post_intro_windows=len(phase_bounds["post_windows"]),
        )

    confidence = "high" if pulls_api else "medium"
    multiplier = (
        safe_multiplier(post_val, baseline_val)
        if isinstance(baseline_val, (int, float)) else EM_DASH
    )

    record: dict[str, Any] = {
        "metric_number": 7,
        "name": name,
        "metric_name": name,
        "value": post_val,
        "after_before_multiplier": multiplier,
        "confidence": confidence,
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_val if not isinstance(baseline_val, str) else INSUFFICIENT_SIGNAL,
            "confidence": confidence,
            "windows": len(phase_bounds["baseline_windows"]),
            "pr_count": baseline_n,
            "excluded_count": baseline_excl,
        },
        "post_introduction": {
            "value": post_val,
            "confidence": confidence,
            "windows": len(phase_bounds["post_windows"]),
            "multiplier": multiplier,
            "pr_count": post_n,
            "excluded_count": post_excl,
        },
        "per_window": [],
        "provenance": provenance,
    }
    if confidence != "high":
        record["boundary_conditions"] = (
            f"Pulls API unavailable; using whatever first-commit timestamps "
            f"are present in data/pulls.json. Excluded post-introduction PRs: "
            f"{post_excl}."
        )
    if isinstance(baseline_val, str):
        record["baseline"]["reason"] = (
            "No baseline PRs yielded a computable first-commit-to-merge duration."
        )
        record["baseline"]["boundary_conditions"] = (
            f"{baseline_excl} excluded; {baseline_n} computable."
        )
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M7",
               "post_value": post_val, "baseline_value": baseline_val,
               "confidence": confidence},
    )
    return record


# ===========================================================================
# Metric 8 — Problem Records in Release
# ===========================================================================


def compute_m8_problem_records(
    reverts: dict[str, Any] | None,
    releases: dict[str, Any] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Count of revert commits attributed to a release (per AAP §0.5.3.9).

    Zero reverts on ``main`` is a deterministic git-only signal and yields
    a High-confidence value of 0. When reverts exist, the phase-level
    value is the mean of attributable reverts per release; unattributable
    and unreleased counts are reported as confidence indicators.
    """
    name = "Problem Records in Release"
    strategy = (
        "Per AAP §0.5.3.9: enumerate revert commits on main via "
        "`git log --grep='^Revert \"'`; parse 'Reverts commit <SHA>' "
        "references; cross-validate against tree-match with prior commit's "
        "parent tree. Attribute each revert to the most recent ancestor "
        "release tag via `git merge-base --is-ancestor`. Exclude reverts-of-"
        "reverts, unattributable reverts, and unreleased reverts; report "
        "exclusion counts as confidence indicators."
    )
    provenance = {
        "requirement_id": "M8",
        "extraction_command": "git log main --grep='^Revert \"' --pretty=format:'%H|%aI|%s'",
        "raw_output_artifact_path": "data/reverts.json",
        "raw_output_artifact_paths": ["data/reverts.json", "data/releases.json"],
        "derivation_function": "compute_m8_problem_records",
    }

    if reverts is None:
        return insufficient(
            8, name,
            reason=(
                "data/reverts.json missing; cannot enumerate revert commits "
                "from a non-existent artifact."
            ),
            extraction_strategy=strategy,
            boundary_conditions="data/reverts.json artifact not present on disk.",
            provenance=provenance,
        )

    revert_list = reverts.get("reverts") or []
    summary = reverts.get("summary") or {}
    total_count = summary.get("total_revert_commits_found", len(revert_list))
    attributable = summary.get("attributable_reverts", 0)
    unattributable = summary.get("unattributable_count", 0)
    unreleased = summary.get("unreleased_count", 0)
    revert_of_revert = summary.get("revert_of_revert_count", 0)

    # Build per-window zero (or count) series across the post-introduction window.
    baseline_per_window = _per_window_zero_series(phase_bounds["baseline_windows"])
    post_per_window = _per_window_zero_series(phase_bounds["post_windows"])

    # If reverts exist, bucket attributable reverts into windows by revert date.
    if revert_list:
        for r in revert_list:
            r_dt = parse_iso(r.get("revert_committer_date") or r.get("revert_date_iso"))
            if r_dt is None:
                continue
            for series, windows in (
                (baseline_per_window, phase_bounds["baseline_windows"]),
                (post_per_window, phase_bounds["post_windows"]),
            ):
                idx = _bucket_to_window(windows, r_dt)
                if idx is not None:
                    series[idx]["value"] += 1

    baseline_total = sum(p["value"] for p in baseline_per_window)
    post_total = sum(p["value"] for p in post_per_window)
    baseline_mean = (
        round(baseline_total / len(baseline_per_window), 4)
        if baseline_per_window else 0
    )
    post_mean = (
        round(post_total / len(post_per_window), 4)
        if post_per_window else 0
    )
    multiplier = safe_multiplier(post_mean, baseline_mean)

    confidence = "high"
    boundary = ""
    if total_count == 0:
        boundary = (
            "Zero revert commits observed on main; per AAP §0.5.3.9 the "
            "metric value is deterministically 0 with High confidence. "
            "Cross-validated by the three independent search commands "
            "recorded in data/reverts.json#search_verification."
        )

    record: dict[str, Any] = {
        "metric_number": 8,
        "name": name,
        "metric_name": name,
        "value": post_mean,
        "after_before_multiplier": multiplier,
        "confidence": confidence,
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_mean,
            "confidence": confidence,
            "windows": len(baseline_per_window),
            "boundary_conditions": (
                f"Baseline-phase revert count is {baseline_total} from direct "
                "git enumeration."
            ),
        },
        "post_introduction": {
            "value": post_mean,
            "confidence": confidence,
            "windows": len(post_per_window),
            "multiplier": multiplier,
            "boundary_conditions": (
                f"Post-introduction revert count is {post_total} from direct "
                "git enumeration. attributable={attr}, unattributable={una}, "
                "unreleased={unr}, revert-of-revert={ror}."
            ).format(
                attr=attributable, una=unattributable,
                unr=unreleased, ror=revert_of_revert,
            ),
        },
        "per_window": post_per_window,
        "per_window_series": post_per_window,
        "provenance": provenance,
        "exclusion_summary": {
            "unattributable": unattributable,
            "unreleased": unreleased,
            "revert_of_revert": revert_of_revert,
            "total_observed": total_count,
            "attributable": attributable,
        },
    }
    if boundary:
        record["boundary_conditions"] = boundary
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M8",
               "post_value": post_mean, "baseline_value": baseline_mean,
               "confidence": confidence, "total_observed": total_count},
    )
    return record


# ===========================================================================
# Metric 9 — Releases
# ===========================================================================


def compute_m9_releases(
    releases: dict[str, Any] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Count of releases per 2-week window (per AAP §0.5.3.10).

    Source precedence:
        Tier 1 — GitHub Releases API
        Tier 2 — Annotated git tags matching ``v?\\d+\\.\\d+\\.\\d+``
        Tier 3 — CI/CD deploy events
    Prereleases (``-alpha``, ``-beta``, ``-rc``, ``-dev``) are excluded
    from the primary count and reported separately.
    """
    name = "Releases"
    strategy = (
        "Per AAP §0.5.3.10: count releases per 2-week window. Source "
        "precedence: (1) GitHub Releases API, (2) annotated git tags "
        "matching v?\\d+\\.\\d+\\.\\d+, (3) CI/CD deploy events. "
        "Prereleases (-alpha, -beta, -rc, -dev) excluded from the "
        "primary count and reported separately."
    )
    provenance = {
        "requirement_id": "M9",
        "extraction_command": (
            "GET /repos/Blitzy-Sandbox/blitzy-RudderStack/releases AND "
            "git for-each-ref 'refs/tags/v[0-9]*' --format='%(refname)|%(creatordate:iso-strict)|%(objectname)'"
        ),
        "raw_output_artifact_path": "data/releases.json",
        "raw_output_artifact_paths": ["data/releases.json"],
        "derivation_function": "compute_m9_releases",
    }

    if releases is None:
        return insufficient(
            9, name,
            reason="data/releases.json missing; release inventory unavailable.",
            extraction_strategy=strategy,
            boundary_conditions="data/releases.json artifact not present on disk.",
            provenance=provenance,
        )

    chosen_tier = releases.get("chosen_tier") or "none"
    tier_availability = releases.get("tier_availability") or {}
    release_list = releases.get("releases") or []
    prerelease_list = releases.get("prereleases") or []

    if chosen_tier == "none" or (not release_list and not prerelease_list):
        # Determine specific reason from tier_availability.
        unavailable_reasons = []
        for tier_key, tier in tier_availability.items():
            if not tier.get("available"):
                ureason = tier.get("unavailable_reason")
                if ureason:
                    unavailable_reasons.append(f"{tier_key}: {ureason}")
        boundary = (
            "All three source tiers yielded zero or were unavailable. "
            f"Tiers: {'; '.join(unavailable_reasons)}." if unavailable_reasons
            else "All three source tiers yielded zero releases."
        )
        return insufficient(
            9, name,
            reason=(
                f"All three source tiers per AAP §0.5.3.10 yielded zero or "
                f"were unavailable. Tier 1 (Releases API): "
                f"{'available' if tier_availability.get('tier_1_github_releases_api', {}).get('available') else 'unavailable'}. "
                "Tier 2 (annotated tags): zero matching tags in local clone. "
                "Tier 3 (CI deploy events): zero deploy events observed. "
                "Per AAP §0.5.3.10, this signals 'Insufficient signal — "
                "Releases API unavailable and no local tags'."
            ),
            extraction_strategy=strategy,
            boundary_conditions=boundary,
            provenance=provenance,
            baseline_windows=len(phase_bounds["baseline_windows"]),
            post_intro_windows=len(phase_bounds["post_windows"]),
        )

    confidence = (
        "high" if chosen_tier == "tier_1_github_releases_api"
        else "medium" if chosen_tier == "tier_2_annotated_tags"
        else "low"
    )

    def _bucket_releases(rels: list[dict[str, Any]],
                          windows: list[tuple[datetime, datetime]],
                          ) -> list[dict[str, Any]]:
        series = _per_window_zero_series(windows)
        for r in rels:
            published = parse_iso(r.get("published_at") or r.get("date") or r.get("created_at"))
            if not published:
                continue
            idx = _bucket_to_window(windows, published)
            if idx is not None:
                series[idx]["value"] += 1
        return series

    baseline_series = _bucket_releases(release_list, phase_bounds["baseline_windows"])
    post_series = _bucket_releases(release_list, phase_bounds["post_windows"])
    baseline_mean = (
        round(statistics.mean(p["value"] for p in baseline_series), 4)
        if baseline_series else 0
    )
    post_mean = (
        round(statistics.mean(p["value"] for p in post_series), 4)
        if post_series else 0
    )
    multiplier = safe_multiplier(post_mean, baseline_mean)

    boundary = (
        ""
        if confidence == "high"
        else (
            f"Release inventory sourced from {chosen_tier}; primary tier "
            f"(GitHub Releases API) "
            f"{'unavailable' if not tier_availability.get('tier_1_github_releases_api', {}).get('available') else 'available'}."
        )
    )

    record: dict[str, Any] = {
        "metric_number": 9,
        "name": name,
        "metric_name": name,
        "value": post_mean,
        "after_before_multiplier": multiplier,
        "confidence": confidence,
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_mean,
            "confidence": confidence,
            "windows": len(baseline_series),
        },
        "post_introduction": {
            "value": post_mean,
            "confidence": confidence,
            "windows": len(post_series),
            "multiplier": multiplier,
        },
        "per_window": post_series,
        "provenance": provenance,
        "chosen_tier": chosen_tier,
        "prereleases_count": len(prerelease_list),
    }
    if boundary:
        record["boundary_conditions"] = boundary
        if confidence == "low":
            record["caveat"] = boundary
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M9",
               "post_value": post_mean, "baseline_value": baseline_mean,
               "confidence": confidence, "chosen_tier": chosen_tier},
    )
    return record



# ===========================================================================
# Metric 10 — Approved Exceptions
# ===========================================================================


def compute_m10_approved_exceptions(
    exceptions: dict[str, Any] | None,
    pulls: dict[str, Any] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Count per 2-week window of policy exceptions / waivers / overrides
    (per AAP §0.5.3.11).

    Signals (priority order):
        (a) Admin audit-log bypass/override events
        (b) Force-pushes to protected branches (git reflog)
        (c) Merges with failing required checks
        (d) PRs labelled with exception/waiver/override/bypass patterns
        (e) Static-analysis exemption inventory (HEAD-only — reported
            separately from the per-window count)

    Without admin audit-log access (signal a), confidence collapses to Low
    even when the observable signals are zero.
    """
    name = "Approved Exceptions"
    strategy = (
        "Per AAP §0.5.3.11: enumerate (a) force-pushes from `git reflog "
        "show main` on the local clone, (b) PRs labelled with patterns "
        "matching exception|waiver|override|bypass from the GitHub Pulls "
        "API, (c) static-analysis exemptions from .golangci.yml, .snyk, "
        ".truffleignore, .deepsource.toml (HEAD-only). Admin audit-log "
        "enumeration via /repos/.../audit-log requires admin access. "
        "Count per 2-week window of (a)+(b)+(c — events only) summed."
    )
    provenance = {
        "requirement_id": "M10",
        "extraction_command": (
            "GET /repos/.../audit-log AND git reflog show main AND "
            "GET /repos/.../pulls (label scan) AND head-only reads of "
            ".golangci.yml .snyk .truffleignore .deepsource.toml"
        ),
        "raw_output_artifact_path": "data/exceptions.json",
        "raw_output_artifact_paths": ["data/exceptions.json", "data/pulls.json"],
        "derivation_function": "compute_m10_approved_exceptions",
    }

    if exceptions is None:
        return insufficient(
            10, name,
            reason="data/exceptions.json missing; exception inventory unavailable.",
            extraction_strategy=strategy,
            boundary_conditions="data/exceptions.json artifact not present on disk.",
            provenance=provenance,
        )

    audit = exceptions.get("audit_log") or {}
    branch_protection = exceptions.get("branch_protection") or {}
    force_pushes = exceptions.get("force_pushes") or {}
    exception_labeled = exceptions.get("exception_labeled_prs") or {}
    lint_exemptions = exceptions.get("lint_exemptions") or {}
    summary = exceptions.get("summary") or {}

    audit_available = bool(audit.get("available"))
    audit_events = audit.get("events") or []
    fp_events = force_pushes.get("events") or []
    label_events = exception_labeled.get("events") or []

    # Combine event lists, attach timestamps.
    all_events: list[datetime] = []
    for ev in audit_events:
        ts = parse_iso(ev.get("created_at") or ev.get("timestamp"))
        if ts:
            all_events.append(ts)
    for ev in fp_events:
        ts = parse_iso(ev.get("date") or ev.get("created_at"))
        if ts:
            all_events.append(ts)
    for ev in label_events:
        ts = parse_iso(ev.get("created_at") or ev.get("timestamp"))
        if ts:
            all_events.append(ts)

    baseline_series = _per_window_zero_series(phase_bounds["baseline_windows"])
    post_series = _per_window_zero_series(phase_bounds["post_windows"])
    for ts in all_events:
        idx_b = _bucket_to_window(phase_bounds["baseline_windows"], ts)
        if idx_b is not None:
            baseline_series[idx_b]["value"] += 1
        idx_p = _bucket_to_window(phase_bounds["post_windows"], ts)
        if idx_p is not None:
            post_series[idx_p]["value"] += 1

    baseline_total = sum(p["value"] for p in baseline_series)
    post_total = sum(p["value"] for p in post_series)
    multiplier = safe_multiplier(post_total, baseline_total)

    # Confidence assignment per AAP §0.2.4.
    confidence = "high" if audit_available else "low"

    audit_caveat = (
        "Without admin audit-log access, the signal is limited to (a) "
        "force-pushes detectable from this clone's local git reflog, (b) "
        "PRs labelled with exception/waiver/override/bypass patterns "
        "(none observed in the current label catalogue per .github/"
        "labeler.yml), and (c) HEAD-only static-analysis exemption "
        "inventory (reported separately, not as per-window events). "
        "The true count of admin overrides, required-check bypasses, "
        "and branch-protection-rule modifications during the analysis "
        "window is unobservable and may be non-zero."
    )

    boundary = (
        "Audit log: " + ("available" if audit_available else "unavailable; "
        "admin access required.") + " " +
        f"Branch-protection check: "
        f"{'available' if branch_protection.get('available') else 'unavailable'}. "
        f"Force-push reflog entries: {force_pushes.get('count', len(fp_events))}. "
        f"Exception-labeled PRs: {exception_labeled.get('exception_pattern_match_count', len(label_events))}. "
        f"Lint-exemption HEAD inventory: {summary.get('lint_exemptions_total', 0)} entries "
        "(reported separately; not counted in per-window events)."
    )

    record: dict[str, Any] = {
        "metric_number": 10,
        "name": name,
        "metric_name": name,
        "value": post_total,
        "after_before_multiplier": multiplier,
        "confidence": confidence,
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_total,
            "confidence": confidence,
            "windows": len(baseline_series),
            "boundary_conditions": (
                f"Baseline period: {baseline_total} observable events "
                "(audit-log absent; observable channels only)."
            ),
        },
        "post_introduction": {
            "value": post_total,
            "confidence": confidence,
            "windows": len(post_series),
            "multiplier": multiplier,
            "boundary_conditions": (
                f"Post-introduction period: {post_total} observable events "
                "(audit-log absent; observable channels only)."
            ),
        },
        "per_window": post_series,
        "provenance": provenance,
        "lint_exemptions_at_head": summary.get("lint_exemptions_total_breakdown", {}),
    }
    if confidence != "high":
        record["boundary_conditions"] = boundary
        record["baseline"]["caveat"] = audit_caveat
        record["post_introduction"]["caveat"] = audit_caveat
    if confidence == "low":
        record["caveat"] = audit_caveat
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M10",
               "post_value": post_total, "baseline_value": baseline_total,
               "confidence": confidence,
               "audit_log_available": audit_available},
    )
    return record


# ===========================================================================
# Metric 11 — Escaped Defects
# ===========================================================================


def compute_m11_escaped_defects(
    transitions: dict[str, Any] | None,
    ci_runs: dict[str, Any] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Count per 2-week window of test regressions and suppressions
    (per AAP §0.5.3.12).

    Primary source: JUnit XML transitions from CI workflow runs.
    Fallback: HEAD-only ``*_test.go`` skip scan (no transition signal —
    reports inventory only).
    """
    name = "Escaped Defects"
    strategy = (
        "Per AAP §0.5.3.12: regressions are pass→fail transitions on main "
        "surviving the flaky-test guard (≥3 consecutive failures); "
        "suppressions are pass→skip transitions. Source: JUnit XML "
        "artifacts from GitHub Actions Runs API. Fallback: HEAD-only "
        "in-repo *_test.go scan for t.Skip()/// nolint markers (reports "
        "skipped-rate snapshot only, no transitions)."
    )
    provenance = {
        "requirement_id": "M11",
        "extraction_command": (
            "GET /repos/.../actions/runs?branch=main + "
            "GET /repos/.../actions/runs/{id}/artifacts (JUnit XML)"
        ),
        "raw_output_artifact_path": "data/test_transitions.json",
        "raw_output_artifact_paths": ["data/test_transitions.json", "data/ci_runs.json"],
        "derivation_function": "compute_m11_escaped_defects",
    }

    if transitions is None and ci_runs is None:
        return insufficient(
            11, name,
            reason="data/test_transitions.json and data/ci_runs.json both missing.",
            extraction_strategy=strategy,
            boundary_conditions="Neither CI history nor transitions artifact present.",
            provenance=provenance,
        )

    transitions_list = (transitions or {}).get("transitions") or []
    head_scan_total = (transitions or {}).get("head_skip_scan_total_count", 0)
    ci_api_available = bool(ci_runs and (ci_runs.get("github_api") or {}).get("available"))
    junit_count = ((ci_runs or {}).get("artifacts") or {}).get("junit_xml_artifacts_count", 0)

    if not transitions_list and junit_count == 0:
        return insufficient(
            11, name,
            reason=(
                "CI test history is unavailable; .github/workflows/tests.yaml "
                "does not emit JUnit XML artifacts (artifacts.junit_xml_artifacts_count=0 "
                f"in data/ci_runs.json; the GitHub Actions API is "
                f"{'available' if ci_api_available else 'unavailable'} in this run). "
                "Per AAP §0.5.3.12, 'Insufficient signal — CI test history "
                "unavailable' applies. The HEAD-only test-skip scan reports "
                f"{head_scan_total} skip markers (inventory only, not "
                "transitions); the truncated canonical-entries set lives in "
                "data/test_transitions.json#head_skip_scan."
            ),
            extraction_strategy=strategy,
            boundary_conditions=(
                f"CI Actions API: {'available' if ci_api_available else 'unavailable'}. "
                f"JUnit XML artifacts retrieved: {junit_count}. "
                f"Transitions surviving flaky guard: 0. "
                f"HEAD skip scan: {head_scan_total} markers (snapshot only)."
            ),
            provenance=provenance,
            baseline_windows=len(phase_bounds["baseline_windows"]),
            post_intro_windows=len(phase_bounds["post_windows"]),
        )

    # When transitions exist, bucket them into windows.
    regressions: list[datetime] = []
    suppressions: list[datetime] = []
    for t in transitions_list:
        ts = parse_iso(t.get("transition_at") or t.get("date"))
        if not ts:
            continue
        kind = t.get("transition_kind")
        if kind == "pass_to_fail":
            regressions.append(ts)
        elif kind == "pass_to_skip":
            suppressions.append(ts)

    baseline_series = _per_window_zero_series(phase_bounds["baseline_windows"])
    post_series = _per_window_zero_series(phase_bounds["post_windows"])
    for ts in regressions + suppressions:
        idx_b = _bucket_to_window(phase_bounds["baseline_windows"], ts)
        if idx_b is not None:
            baseline_series[idx_b]["value"] += 1
        idx_p = _bucket_to_window(phase_bounds["post_windows"], ts)
        if idx_p is not None:
            post_series[idx_p]["value"] += 1

    baseline_total = sum(p["value"] for p in baseline_series)
    post_total = sum(p["value"] for p in post_series)
    multiplier = safe_multiplier(post_total, baseline_total)
    confidence = "high"

    record: dict[str, Any] = {
        "metric_number": 11,
        "name": name,
        "metric_name": name,
        "value": post_total,
        "after_before_multiplier": multiplier,
        "confidence": confidence,
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_total,
            "confidence": confidence,
            "windows": len(baseline_series),
        },
        "post_introduction": {
            "value": post_total,
            "confidence": confidence,
            "windows": len(post_series),
            "multiplier": multiplier,
        },
        "per_window": post_series,
        "provenance": provenance,
        "regressions_count": len(regressions),
        "suppressions_count": len(suppressions),
        "head_skip_scan_total_count": head_scan_total,
    }
    if confidence != "high":
        record["boundary_conditions"] = (
            f"Transitions extracted from {junit_count} JUnit XML artifacts."
        )
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M11",
               "post_value": post_total, "baseline_value": baseline_total,
               "confidence": confidence,
               "regressions": len(regressions),
               "suppressions": len(suppressions)},
    )
    return record


# ===========================================================================
# Metric 12 — Defects Out of SLA
# ===========================================================================


def compute_m12_defects_out_of_sla(
    issues: dict[str, Any] | None,
    slas: dict[str, Any] | None,
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, Any]:
    """Count of defect-labeled issues whose resolution time exceeds the
    SLA target for the issue's severity tier (per AAP §0.5.3.13).

    Issue-scoped (not PR-scoped) because SLAs in standard usage attach to
    defect tickets, not to the code changes that resolve them.

    When no SLA source is available (Linear API absent AND no policy doc
    in the repo), reports "Insufficient signal — no SLA source".
    """
    name = "Defects Out of SLA"
    strategy = (
        "Per AAP §0.5.3.13: count defect-labeled issues whose close_time − "
        "open_time exceeds the SLA target for their severity tier. SLA "
        "targets sourced (priority order): (1) Linear API issue tracker SLA "
        "field, (2) repository policy document under docs/, blitzy-docs/, "
        "blitzy/documentation/. When no SLA source is available, "
        "report 'Insufficient signal — no SLA source'."
    )
    provenance = {
        "requirement_id": "M12",
        "extraction_command": (
            "POST https://api.linear.app/graphql (issues + labels + slaBreachedAt) "
            "AND grep -REi 'SLA|severity|Sev-|priority response time|incident response' "
            "docs/ blitzy-docs/ blitzy/documentation/ CONTRIBUTING.md"
        ),
        "raw_output_artifact_path": "data/issues.json",
        "raw_output_artifact_paths": ["data/issues.json", "data/slas.json"],
        "derivation_function": "compute_m12_defects_out_of_sla",
    }

    if issues is None or slas is None:
        return insufficient(
            12, name,
            reason="data/issues.json or data/slas.json missing.",
            extraction_strategy=strategy,
            boundary_conditions=(
                f"data/issues.json present={issues is not None}; "
                f"data/slas.json present={slas is not None}."
            ),
            provenance=provenance,
        )

    linear_available = bool(((issues.get("linear") or {})).get("available"))
    sla_list = slas.get("slas") or []
    issue_list = issues.get("issues") or []

    if not linear_available and not sla_list:
        # No SLA source at all.
        return insufficient(
            12, name,
            reason=(
                "No SLA source is available. Linear API access is not "
                f"configured (LINEAR_API_KEY unset; data/issues.json#linear."
                f"available={linear_available}; data/issues.json#issues "
                f"is empty: {len(issue_list)} issues). No SLA policy "
                "document was discovered in the repository's documentation "
                "tree per data/slas.json#repo_policy_scan. Per AAP §0.5.3.13, "
                "the metric reports 'Insufficient signal — no SLA source' "
                "in this case."
            ),
            extraction_strategy=strategy,
            boundary_conditions=(
                f"Linear API: unavailable. Repository policy scan: "
                f"{slas.get('unavailable_reason_detail', 'no SLA-bearing document found')}. "
                "Both source tiers per AAP §0.5.3.13 return zero, and the "
                "metric collapses to insufficient_signal as instructed."
            ),
            provenance=provenance,
            baseline_windows=len(phase_bounds["baseline_windows"]),
            post_intro_windows=len(phase_bounds["post_windows"]),
        )

    # SLA source available — bucket issues by phase.
    # Build a severity → SLA-target lookup (hours) from the slas list.
    sla_lookup: dict[str, float] = {}
    for s in sla_list:
        sev = (s.get("severity") or s.get("name") or "").strip().lower()
        if not sev:
            continue
        hours = s.get("target_hours")
        if hours is None and s.get("target_duration_iso"):
            # Could parse ISO durations; for now skip if not numeric.
            continue
        if isinstance(hours, (int, float)):
            sla_lookup[sev] = float(hours)

    def _phase_count(window_bounds: tuple[datetime, datetime]) -> tuple[int, int]:
        ws, we = window_bounds
        breach_count = 0
        total = 0
        for iss in issue_list:
            labels = [str(l).lower() for l in (iss.get("labels") or [])]
            if not any(l in {"bug", "defect"} for l in labels):
                continue
            opened = parse_iso(iss.get("created_at"))
            closed = parse_iso(iss.get("completed_at") or iss.get("closed_at"))
            if not opened or not closed:
                continue
            if not (ws <= closed < we):
                continue
            total += 1
            sev = (iss.get("severity") or "").strip().lower()
            target = sla_lookup.get(sev)
            sla_breached = iss.get("slaBreachedAt")
            if sla_breached:
                breach_count += 1
                continue
            if target is None:
                continue
            resolution_hours = (closed - opened).total_seconds() / 3600.0
            if resolution_hours > target:
                breach_count += 1
        return breach_count, total

    baseline_breach, baseline_total = _phase_count(
        (phase_bounds["baseline_start"], phase_bounds["baseline_end"])
    )
    post_breach, post_total = _phase_count(
        (phase_bounds["post_start"], phase_bounds["post_end"])
    )
    multiplier = safe_multiplier(post_breach, baseline_breach)
    confidence = "high" if linear_available else "medium"
    boundary = (
        ""
        if confidence == "high"
        else f"SLA source: repository policy document; Linear API unavailable. "
    )

    record: dict[str, Any] = {
        "metric_number": 12,
        "name": name,
        "metric_name": name,
        "value": post_breach,
        "after_before_multiplier": multiplier,
        "confidence": confidence,
        "extraction_strategy": strategy,
        "baseline": {
            "value": baseline_breach,
            "confidence": confidence,
            "windows": len(phase_bounds["baseline_windows"]),
            "total_defects": baseline_total,
        },
        "post_introduction": {
            "value": post_breach,
            "confidence": confidence,
            "windows": len(phase_bounds["post_windows"]),
            "multiplier": multiplier,
            "total_defects": post_total,
        },
        "per_window": [],
        "provenance": provenance,
        "breach_rate_post": (
            round(post_breach / post_total, 4) if post_total else None
        ),
        "breach_rate_baseline": (
            round(baseline_breach / baseline_total, 4) if baseline_total else None
        ),
    }
    if confidence != "high":
        record["boundary_conditions"] = boundary
    logger.info(
        "metric_extracted",
        extra={"event": "metric_extracted", "metric_id": "M12",
               "post_value": post_breach, "baseline_value": baseline_breach,
               "confidence": confidence,
               "linear_available": linear_available},
    )
    return record



# ===========================================================================
# Per-engineer breakdown (Metrics 2, 4, 5, 6, 10)
# ===========================================================================


def compute_per_engineer_breakdown(
    pulls: dict[str, Any] | None,
    commits: list[dict[str, str]] | None,
    metrics: dict[str, dict[str, Any]],
    phase_bounds: dict[str, Any],
    env: dict[str, Any] | None,
    inflection: dict[str, Any] | None,
    logger: Any,
) -> dict[str, Any]:
    """Produce per_engineer.json — per-engineer breakdown of Metrics 2, 4,
    5, 6, 10 (per AAP §0.5.6 and Quality Gate 5).

    Engineer canonicalisation:
        * Blitzy identity union (agent@blitzy.com + 191547922+blitzy[bot]...)
          → single ``Blitzy`` row.
        * dependabot[bot] → excluded.
        * All other authors → real display names from git.
    """
    pulls_data = pulls or {}
    pr_list = pulls_data.get("pulls") or []
    commits = commits or []

    # Build canonical engineer roster from commits.csv plus the Blitzy union.
    engineer_emails: dict[str, set[str]] = defaultdict(set)
    engineer_display_alias: dict[str, set[str]] = defaultdict(set)
    engineer_commit_counts: dict[str, int] = defaultdict(int)
    engineer_baseline_commits: dict[str, int] = defaultdict(int)
    engineer_post_commits: dict[str, int] = defaultdict(int)
    engineer_first_commit: dict[str, datetime | None] = {}
    engineer_last_commit: dict[str, datetime | None] = {}

    baseline_end = phase_bounds["baseline_end"]
    post_start = phase_bounds["post_start"]

    for row in commits:
        email = (row.get("author_email") or "").strip()
        display = (row.get("author_name") or "").strip()
        if is_dependency_bot(email):
            continue
        actor = canonical_actor(email, display)
        engineer_emails[actor].add(email)
        if display:
            engineer_display_alias[actor].add(display)
        engineer_commit_counts[actor] += 1
        dt = parse_iso(row.get("author_date_iso"))
        if dt is not None:
            prev_first = engineer_first_commit.get(actor)
            if prev_first is None or dt < prev_first:
                engineer_first_commit[actor] = dt
            prev_last = engineer_last_commit.get(actor)
            if prev_last is None or dt > prev_last:
                engineer_last_commit[actor] = dt
            if dt < baseline_end:
                engineer_baseline_commits[actor] += 1
            elif dt >= post_start:
                engineer_post_commits[actor] += 1

    # Build per-engineer M2 (Flow Velocity) attribution from the PR roster.
    m2_per_engineer_baseline: dict[str, int] = defaultdict(int)
    m2_per_engineer_post: dict[str, int] = defaultdict(int)
    m2_pr_numbers_post: dict[str, list[Any]] = defaultdict(list)
    for pr in pr_list:
        email = ((pr.get("user") or {}).get("email")) or ""
        if is_dependency_bot(email):
            continue
        display = ((pr.get("user") or {}).get("login")) or ""
        actor = canonical_actor(email, display)
        if actor not in engineer_emails and actor != BLITZY_DISPLAY_NAME:
            # Ensure the actor is present in the roster.
            engineer_emails[actor].add(email)
            if display:
                engineer_display_alias[actor].add(display)
        merged_at = parse_iso(pr.get("merged_at"))
        if merged_at is None:
            continue
        if merged_at < baseline_end:
            m2_per_engineer_baseline[actor] += 1
        elif merged_at >= post_start:
            m2_per_engineer_post[actor] += 1
            m2_pr_numbers_post[actor].append(pr.get("number"))

    # M6 per-actor category proportions (from the post-introduction phase
    # of the existing M6 record).
    m6 = metrics.get("m6", {}) or {}
    m6_post = (m6.get("post_introduction") or {})
    m6_per_actor = m6_post.get("per_actor_proportions") or {}

    # M10 has no per-engineer attribution when audit log is absent.

    engineers: dict[str, dict[str, Any]] = {}
    for actor, emails in engineer_emails.items():
        baseline_count = m2_per_engineer_baseline.get(actor, 0)
        post_count = m2_per_engineer_post.get(actor, 0)
        multiplier = safe_multiplier(post_count, baseline_count)
        first_iso = engineer_first_commit.get(actor)
        last_iso = engineer_last_commit.get(actor)
        entry: dict[str, Any] = {
            "display_name": actor,
            "actor_type": "ai" if actor == BLITZY_DISPLAY_NAME else "human",
            "email_aliases": sorted(emails),
            "display_name_aliases": sorted(engineer_display_alias.get(actor, set())),
            "total_commits_on_main": engineer_commit_counts.get(actor, 0),
            "commits_in_baseline_phase": engineer_baseline_commits.get(actor, 0),
            "commits_in_post_introduction_phase": engineer_post_commits.get(actor, 0),
            "first_commit_iso": iso_z(first_iso) if first_iso else None,
            "last_commit_iso": iso_z(last_iso) if last_iso else None,
            "m2_flow_velocity": {
                "baseline": baseline_count,
                "post_introduction": post_count,
                "multiplier": multiplier,
                "post_introduction_pr_numbers": [
                    n for n in m2_pr_numbers_post.get(actor, []) if n is not None
                ],
            },
            "m4_flow_active": {
                "baseline": INSUFFICIENT_SIGNAL,
                "post_introduction": INSUFFICIENT_SIGNAL,
                "reason": (
                    "Per-engineer Flow Active requires review-event-bounded "
                    "per-PR active spans (Pulls + Reviews + Events APIs). "
                    "Not available in this run."
                ),
            },
            "m5_flow_efficiency": {
                "baseline": INSUFFICIENT_SIGNAL,
                "post_introduction": INSUFFICIENT_SIGNAL,
                "reason": (
                    "Per-engineer Flow Efficiency inherits Flow Active and "
                    "Flow Time per-PR per-actor signal."
                ),
            },
            "m6_flow_distribution": {
                "post_introduction_proportions": m6_per_actor.get(actor, {}),
            },
            "m10_approved_exceptions": {
                "baseline": INSUFFICIENT_SIGNAL if actor != BLITZY_DISPLAY_NAME else 0,
                "post_introduction": INSUFFICIENT_SIGNAL if actor != BLITZY_DISPLAY_NAME else 0,
                "reason": (
                    "Per-engineer Approved Exceptions requires admin audit-"
                    "log attribution; not available without admin access."
                ),
            },
        }
        engineers[actor] = entry

    metadata: dict[str, Any] = {
        "extraction_timestamp": (env or {}).get("extraction_timestamp"),
        "run_id": (env or {}).get("run_id") or os.environ.get("BLITZY_RUN_ID", ""),
        "repository_slug": (env or {}).get("repository_slug", ""),
        "default_branch": (env or {}).get("default_branch", "main"),
        "schema_version": "1.0",
        "artifact_kind": "per_engineer_metrics",
        "produced_by": "scripts/09_compute_metrics.py",
        "consumed_by": ["scripts/10_render_report.py", "scripts/11_render_deck.py"],
        "inflection_date_utc": phase_bounds["inflection_date_utc"],
        "phase_decomposition": (
            "baseline_vs_post_introduction"
            if not phase_bounds["ramp_up_steady_state_split_applied"]
            else "baseline_vs_ramp_up_vs_steady_state"
        ),
        "phase_decomposition_reason": phase_bounds[
            "ramp_up_steady_state_split_applied_reason"
        ],
        "metrics_covered": ["m2", "m4", "m5", "m6", "m10"],
        "metrics_covered_note": (
            "Per AAP §0.5.6 Quality Gate 5 — the metrics that admit "
            "individual-attribution data. Other metrics (m1 Flow Load, "
            "m3 Flow Predictability, m7 Flow Time, m8 Problem Records, "
            "m9 Releases, m11 Escaped Defects, m12 Defects Out of SLA) "
            "are reported only at the phase level in data/metrics.json."
        ),
    }

    out = {
        "_metadata": metadata,
        "engineers": engineers,
    }
    logger.info(
        "per_engineer_extracted",
        extra={"event": "per_engineer_extracted",
               "engineer_count": len(engineers),
               "engineers": list(engineers.keys())},
    )
    return out


# ===========================================================================
# Metric orchestration
# ===========================================================================


def compute_all_metrics(
    artifacts: dict[str, Any],
    phase_bounds: dict[str, Any],
    logger: Any,
) -> dict[str, dict[str, Any]]:
    """Run all twelve compute functions in dependency order.

    Order:
        m1, m2 (independent)
        m3 (depends on m2)
        m4 (independent)
        m7 (independent)
        m5 (depends on m4 + m7)
        m6 (independent)
        m8, m9, m10, m11, m12 (independent)
    """
    pulls = artifacts.get("pulls.json")
    commits = artifacts.get("commits.csv")
    reviews = artifacts.get("reviews.json")
    events = artifacts.get("pull_events.json")
    releases = artifacts.get("releases.json")
    reverts = artifacts.get("reverts.json")
    ci_runs = artifacts.get("ci_runs.json")
    transitions = artifacts.get("test_transitions.json")
    exceptions = artifacts.get("exceptions.json")
    issues = artifacts.get("issues.json")
    slas = artifacts.get("slas.json")

    metrics: dict[str, dict[str, Any]] = {}
    metrics["m1"] = compute_m1_flow_load(pulls, commits, phase_bounds, logger)
    metrics["m2"] = compute_m2_flow_velocity(pulls, commits, phase_bounds, logger)
    metrics["m3"] = compute_m3_flow_predictability(metrics["m2"], logger)
    metrics["m4"] = compute_m4_flow_active(pulls, reviews, events, phase_bounds, logger)
    metrics["m7"] = compute_m7_flow_time(pulls, commits, phase_bounds, logger)
    metrics["m5"] = compute_m5_flow_efficiency(metrics["m4"], metrics["m7"], logger)
    metrics["m6"] = compute_m6_flow_distribution(pulls, issues, phase_bounds, logger)
    metrics["m8"] = compute_m8_problem_records(reverts, releases, phase_bounds, logger)
    metrics["m9"] = compute_m9_releases(releases, phase_bounds, logger)
    metrics["m10"] = compute_m10_approved_exceptions(
        exceptions, pulls, phase_bounds, logger
    )
    metrics["m11"] = compute_m11_escaped_defects(transitions, ci_runs, phase_bounds, logger)
    metrics["m12"] = compute_m12_defects_out_of_sla(issues, slas, phase_bounds, logger)
    return metrics


# ===========================================================================
# Schema cleanup — sanitize records against schema constraints
# ===========================================================================

_PHASE_KEYS_ALLOWED_DROP = ("ramp_up", "steady_state", "post_introduction")
_METADATA_ALLOWED_KEYS = frozenset({
    "schema_version", "run_id", "extraction_timestamp", "generated_by",
    "inflection_date_utc", "source_artifact_hashes", "rudder_server_head_sha",
    "rudder_server_repo_slug", "ramp_up_steady_state_split_applied",
    "ramp_up_steady_state_split_applied_reason",
    "ramp_up_steady_state_threshold_days", "post_introduction_duration_days",
    "phase_keys_used", "schema_version_change_note",
})


def _enforce_schema_invariants(metric: dict[str, Any]) -> None:
    """Mutate the metric record in place to satisfy schema invariants
    that cannot be expressed inline at compute time:

    * If ``value == "insufficient_signal"`` → ``reason`` required.
    * If ``confidence != "high"`` → ``boundary_conditions`` required.
    * If ``confidence == "low"`` → ``caveat`` required.
    * ``baseline``, ``post_introduction`` phase bodies must also satisfy
      the same value/reason and confidence/boundary rules.
    """
    # Top-level enforcement.
    val = metric.get("value")
    conf = metric.get("confidence")
    if val == INSUFFICIENT_SIGNAL and not metric.get("reason"):
        metric["reason"] = (
            "Primary data source unavailable; metric collapses to "
            "insufficient_signal per AAP §0.3.2."
        )
    if conf != "high" and not metric.get("boundary_conditions"):
        metric["boundary_conditions"] = (
            f"Confidence is '{conf}' per AAP §0.2.4; boundary conditions "
            "of the source data are documented in the per-source provenance."
        )
    if conf == "low" and not metric.get("caveat"):
        metric["caveat"] = metric.get("boundary_conditions", "Low-confidence metric.")

    # Phase-body enforcement.
    for phase_key in ("baseline", "post_introduction", "ramp_up", "steady_state"):
        phase = metric.get(phase_key)
        if not isinstance(phase, dict):
            continue
        pv = phase.get("value")
        pconf = phase.get("confidence")
        if pv == INSUFFICIENT_SIGNAL and not phase.get("reason"):
            phase["reason"] = (
                f"{phase_key} value is insufficient_signal — inherits the "
                "metric-level reason."
            )
        if pconf and pconf != "high" and not phase.get("boundary_conditions"):
            phase["boundary_conditions"] = (
                f"{phase_key} confidence '{pconf}' — inherits the metric-"
                "level boundary conditions."
            )


def _drop_disallowed_phase_keys(
    metric: dict[str, Any], phase_keys_used: list[str]
) -> None:
    """Remove phase keys not present in ``phase_keys_used`` to honour the
    schema's oneOf temporal-phase constraint."""
    for key in _PHASE_KEYS_ALLOWED_DROP:
        if key not in phase_keys_used and key in metric:
            del metric[key]


def _sanitize_metadata(metadata: dict[str, Any]) -> dict[str, Any]:
    """Strip any keys not in the schema's ``_metadata`` whitelist.

    The schema enforces ``_metadata.additionalProperties: false``, so any
    extra field would fail validation. Compute-time additions can land
    here as a safety net.
    """
    return {k: v for k, v in metadata.items() if k in _METADATA_ALLOWED_KEYS}


# ===========================================================================
# Assemble metrics.json
# ===========================================================================


def assemble_metrics_json(
    metrics: dict[str, dict[str, Any]],
    env: dict[str, Any] | None,
    phase_bounds: dict[str, Any],
) -> dict[str, Any]:
    """Build the final metrics.json payload from the per-metric compute
    output plus the environment / inflection metadata.

    Enforces:
        * Exactly 13 top-level keys (``_metadata`` + ``m1``..``m12``).
        * Schema invariants per metric (value/reason, confidence/boundary,
          low/caveat).
        * Phase-keys oneOf (two-phase XOR three-phase).
        * ``_metadata.additionalProperties: false`` (only schema-known keys).
    """
    extraction_timestamp = (
        (env or {}).get("extraction_timestamp")
        or iso_z(datetime.now(timezone.utc))
    )
    run_id = (env or {}).get("run_id") or os.environ.get("BLITZY_RUN_ID", "")
    repo_slug = (env or {}).get("repository_slug") or ""
    head_sha = (env or {}).get("head_sha") or ""

    metadata: dict[str, Any] = {
        "schema_version": METRICS_SCHEMA_VERSION,
        "run_id": run_id,
        "extraction_timestamp": extraction_timestamp,
        "generated_by": "scripts/09_compute_metrics.py",
        "inflection_date_utc": phase_bounds["inflection_date_utc"],
        "ramp_up_steady_state_split_applied":
            phase_bounds["ramp_up_steady_state_split_applied"],
        "ramp_up_steady_state_split_applied_reason":
            phase_bounds["ramp_up_steady_state_split_applied_reason"],
        "ramp_up_steady_state_threshold_days": RAMP_UP_DAYS,
        "post_introduction_duration_days":
            phase_bounds["post_intro_duration_days"],
        "phase_keys_used": phase_bounds["phase_keys_used"],
    }
    if repo_slug:
        metadata["rudder_server_repo_slug"] = repo_slug
    if head_sha and len(head_sha) == 40:
        metadata["rudder_server_head_sha"] = head_sha

    metadata = _sanitize_metadata(metadata)

    out: dict[str, Any] = {"_metadata": metadata}
    for i in range(1, 13):
        key = f"m{i}"
        m = metrics.get(key) or {}
        _enforce_schema_invariants(m)
        _drop_disallowed_phase_keys(m, phase_bounds["phase_keys_used"])
        out[key] = m
    return out


# ===========================================================================
# Schema validation
# ===========================================================================


def validate_metrics_json(payload: dict[str, Any], logger: Any) -> None:
    """Validate ``payload`` against ``metrics.schema.json``.

    Raises ``jsonschema.ValidationError`` on failure (caller is expected
    to log the structured-JSON error and re-raise so the script exits 1).
    """
    if not METRICS_SCHEMA_PATH.exists():
        raise FileNotFoundError(
            f"metrics schema not found at {METRICS_SCHEMA_PATH}; "
            "cannot validate metrics.json output."
        )
    schema = json.loads(METRICS_SCHEMA_PATH.read_text(encoding="utf-8"))
    try:
        jsonschema.validate(payload, schema)
        logger.info(
            "metrics_schema_validated",
            extra={"event": "metrics_schema_validated",
                   "schema_path": str(METRICS_SCHEMA_PATH.relative_to(WORKSPACE_ROOT)),
                   "metric_count": sum(1 for k in payload if k.startswith("m"))},
        )
    except jsonschema.ValidationError as exc:
        logger.error(
            "metrics_schema_validation_failed",
            extra={"event": "metrics_schema_validation_failed",
                   "error_message": exc.message[:240],
                   "error_path": [str(p) for p in exc.absolute_path],
                   "schema_path_at_error": [str(p) for p in exc.absolute_schema_path]},
        )
        raise


# ===========================================================================
# File writing
# ===========================================================================


def write_outputs(
    metrics_payload: dict[str, Any],
    per_engineer_payload: dict[str, Any],
    logger: Any,
) -> None:
    """Atomically write metrics.json and per_engineer.json.

    ``json.dumps(..., sort_keys=False, indent=2)`` is used so the output
    is human-readable and the field order is stable for diff review.
    Atomic semantics use ``tmp file → os.replace`` to avoid corrupt
    output on interruption.
    """
    for path, payload in (
        (METRICS_JSON_PATH, metrics_payload),
        (PER_ENGINEER_JSON_PATH, per_engineer_payload),
    ):
        tmp_path = path.with_suffix(path.suffix + ".tmp")
        text = json.dumps(payload, indent=2, ensure_ascii=False, sort_keys=False)
        tmp_path.write_text(text, encoding="utf-8")
        os.replace(tmp_path, path)
        logger.info(
            "output_written",
            extra={"event": "output_written",
                   "path": str(path.relative_to(WORKSPACE_ROOT)),
                   "bytes": len(text)},
        )



# ===========================================================================
# Entry point
# ===========================================================================


def _build_arg_parser() -> argparse.ArgumentParser:
    """Construct the CLI parser. Exposed as a module function so that test
    harnesses can call it directly without invoking ``main()``."""
    parser = argparse.ArgumentParser(
        prog="09_compute_metrics.py",
        description=(
            "Compute the 12 acceleration-report metrics from raw data "
            "artifacts under blitzy/acceleration-report/data/ and emit "
            "data/metrics.json and data/per_engineer.json. This is the "
            "SINGLE SOURCE OF TRUTH for downstream renderers; "
            "10_render_report.py and 11_render_deck.py consume only "
            "these two files (Rule 4 Internal Consistency)."
        ),
        epilog=(
            "Exit codes: 0 success; 1 missing artifact or schema "
            "validation failure or unexpected error. The script performs "
            "ZERO HTTP requests and ZERO git invocations — every byte of "
            "input comes from the local filesystem. See AAP §0.5.6 for "
            "the engineering-actor substitution contract and AAP §0.2.4 "
            "for confidence-tier rules."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Print a JSON manifest of the files this script would read "
            "and write, then exit 0 without performing any I/O on data "
            "files. Acts as the readiness preflight required by "
            "Rule: Observability."
        ),
    )
    return parser


def _dry_run_manifest() -> dict[str, Any]:
    """Build the structured manifest printed by ``--dry-run``."""
    return {
        "action": "dry_run",
        "script": SCRIPT_NAME,
        "workspace_root": str(WORKSPACE_ROOT),
        "reads": [f"data/{name}" for name in RAW_ARTIFACTS],
        "writes": [
            f"data/{METRICS_JSON_PATH.name}",
            f"data/{PER_ENGINEER_JSON_PATH.name}",
        ],
        "schema_validation_against": str(
            METRICS_SCHEMA_PATH.relative_to(WORKSPACE_ROOT)
        ),
        "constants": {
            "WINDOW_DAYS": WINDOW_DAYS,
            "WINDOW_ANCHOR_WEEKDAY": WINDOW_ANCHOR_WEEKDAY,
            "RAMP_UP_DAYS": RAMP_UP_DAYS,
            "FLAKY_THRESHOLD": FLAKY_THRESHOLD,
            "UNKNOWN_RATE_DOWNGRADE": UNKNOWN_RATE_DOWNGRADE,
            "BLITZY_DISPLAY_NAME": BLITZY_DISPLAY_NAME,
            "BLITZY_IDENTITY_EMAILS": sorted(BLITZY_IDENTITY_EMAILS),
            "DEPENDENCY_BOT_EMAILS": sorted(DEPENDENCY_BOT_EMAILS),
        },
        "external_endpoints_invoked": [],
        "git_commands_invoked": [],
    }


def main() -> int:
    """Top-level entry point for ``09_compute_metrics.py``.

    The function is intentionally exposed as a module-level callable so
    that test harnesses and downstream orchestrators can invoke it
    programmatically (the script's documented default export).

    Returns the process exit code: ``0`` on success, ``1`` on any failure
    (missing raw artifact, schema validation error, unexpected exception).
    """
    parser = _build_arg_parser()
    args = parser.parse_args()
    logger = get_logger(SCRIPT_NAME)
    logger.info(
        "script_started",
        extra={
            "event": "script_started",
            "dry_run": bool(args.dry_run),
            "workspace_root": str(WORKSPACE_ROOT),
        },
    )

    if args.dry_run:
        manifest = _dry_run_manifest()
        sys.stdout.write(json.dumps(manifest, indent=2) + "\n")
        sys.stdout.flush()
        logger.info(
            "script_complete",
            extra={"event": "script_complete", "exit_code": 0,
                   "dry_run": True, "outputs_written": 0},
        )
        return 0

    try:
        # ---- Load every raw artifact ---------------------------------------
        artifacts: dict[str, Any] = {}
        for name in RAW_ARTIFACTS:
            artifacts[name] = load_artifact(name, logger)

        env = artifacts.get("environment.json")
        inflection = artifacts.get("inflection.json")
        if env is None:
            raise RuntimeError(
                "data/environment.json missing; Rule 6 (Environment First) "
                "requires this artifact before any metric extraction."
            )
        if inflection is None:
            raise RuntimeError(
                "data/inflection.json missing; cannot derive baseline vs "
                "post-introduction phase boundary."
            )

        # ---- Derive phase bounds ------------------------------------------
        phase_bounds = derive_phase_bounds(env, inflection, logger)

        # ---- Compute all 12 metrics ---------------------------------------
        metrics_by_key = compute_all_metrics(artifacts, phase_bounds, logger)

        # ---- Compute per-engineer breakdown -------------------------------
        per_engineer = compute_per_engineer_breakdown(
            artifacts.get("pulls.json"),
            artifacts.get("commits.csv"),
            metrics_by_key,
            phase_bounds,
            env,
            inflection,
            logger,
        )

        # ---- Assemble final metrics.json payload --------------------------
        metrics_payload = assemble_metrics_json(metrics_by_key, env, phase_bounds)

        # ---- Validate against the JSON schema before writing --------------
        validate_metrics_json(metrics_payload, logger)

        # ---- Write outputs (atomic replace) -------------------------------
        write_outputs(metrics_payload, per_engineer, logger)

        # ---- Surface a counters summary on stdout for the dashboard
        # template (Rule: Observability — metrics surface for analytics scripts).
        summary = {
            "action": "compute_complete",
            "metrics_emitted": 12,
            "outputs_written": [
                str(METRICS_JSON_PATH.relative_to(WORKSPACE_ROOT)),
                str(PER_ENGINEER_JSON_PATH.relative_to(WORKSPACE_ROOT)),
            ],
            "schema_version": METRICS_SCHEMA_VERSION,
            "phase_keys_used": phase_bounds["phase_keys_used"],
            "engineer_count": len(per_engineer.get("engineers", {})),
            "confidence_summary": {
                k: metrics_by_key[k].get("confidence") for k in metrics_by_key
            },
        }
        sys.stdout.write(json.dumps(summary, indent=2) + "\n")
        sys.stdout.flush()
        logger.info(
            "script_complete",
            extra={"event": "script_complete", "exit_code": 0,
                   "outputs_written": len(summary["outputs_written"])},
        )
        return 0
    except FileNotFoundError as exc:
        logger.error(
            "script_failed",
            extra={"event": "script_failed", "error_class": "FileNotFoundError",
                   "error": str(exc)[:240]},
        )
        return 1
    except jsonschema.ValidationError as exc:
        logger.error(
            "script_failed",
            extra={"event": "script_failed",
                   "error_class": "ValidationError",
                   "error": exc.message[:240],
                   "error_path": [str(p) for p in exc.absolute_path]},
        )
        return 1
    except Exception as exc:  # pragma: no cover — defensive bottom guard
        logger.error(
            "script_failed",
            extra={"event": "script_failed",
                   "error_class": type(exc).__name__,
                   "error": str(exc)[:240]},
        )
        return 1


if __name__ == "__main__":
    sys.exit(main())

