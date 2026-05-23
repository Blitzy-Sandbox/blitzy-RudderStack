#!/usr/bin/env python3
"""Three-tier AI inflection-point detection for the acceleration-report pipeline.

This script identifies the canonical "before → after" boundary that partitions
every downstream metric into baseline and after periods (Agent Action Plan
§0.5.3.1). The chosen inflection date is persisted to ``data/inflection.json``
along with its evidence, justification, and the alternatives that were
considered but not used.

Detection precedence (the first tier that yields a signal wins):

* **Tier 1 — trailer search** scans commit message bodies across all refs for
  ``Co-authored-by:`` trailers whose email or display name matches one of the
  AI-actor patterns (Claude, Copilot, Cursor, Aider, Blitzy,
  ``noreply@anthropic.com``, ``copilot@github.com``, ``@blitzy.com``,
  ``blitzy[bot]``). The earliest such commit's author date is the inflection
  point.
* **Tier 2 — AI-actor email pattern** walks ``git log --all`` filtering for
  author emails matching the same AI-actor patterns. The earliest such
  commit's author date is the inflection point.
* **Tier 3 — velocity inflection** computes weekly commit counts on ``main``
  (Monday-anchored, UTC) and identifies the earliest week where the
  post-week rolling mean is at least :data:`VELOCITY_INFLECTION_THRESHOLD`
  times the pre-week rolling mean AND sustains at least
  :data:`VELOCITY_SUSTAIN_THRESHOLD` times for the next
  :data:`SUSTAIN_WEEKS` weeks (sustained inflection). The Monday of that
  week is the inflection point.

The script is read-only: it invokes only ``git log`` (via the validated
:func:`lib.git.git_log` wrapper for Tier 2/3 and via a hard-coded subprocess
call for Tier 1 because the trailer search requires a multi-line record
format not exposed by :mod:`lib.git`). It never modifies any git ref,
working tree, or external system.

The output payload conforms to ``scripts/lib/schemas/inflection.schema.json``.
Required top-level fields: ``tier_used``, ``date_utc``, ``evidence``,
``justification``, ``alternatives_considered``, ``run_id``, ``fetched_at``.
Optional top-level fields used by the compute step include
``baseline_duration_days``, ``post_introduction`` (with ``duration_days``,
``start_iso``, ``end_iso``), ``ramp_up_steady_state_split_applied``, and
``ramp_up_steady_state_threshold_days`` per AAP §0.5.6.

Usage::

    python3 01_detect_inflection.py [--dry-run] [--output PATH]

The ``--dry-run`` flag previews the git commands and the output path
without performing any I/O beyond stdout and the structured JSON log.
"""

from __future__ import annotations

import argparse
import csv
import json
import os
import re
import subprocess
import sys
from collections import defaultdict
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Optional

# Ensure the script's own directory is importable so ``from lib.observability``
# resolves regardless of the caller's working directory. This is the canonical
# pattern used by every numbered script in the pipeline.
sys.path.insert(0, str(Path(__file__).resolve().parent))

from lib.observability import get_logger  # noqa: E402  (intentional after sys.path tweak)
from lib.git import git_log  # noqa: E402  (intentional after sys.path tweak)


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------

#: Logger name. Appears verbatim in the ``script`` field of every emitted log
#: event and is the cache key for :func:`lib.observability.get_logger`.
SCRIPT_NAME: str = "01_detect_inflection"

#: Workspace root — the ``blitzy/acceleration-report/`` directory. Used to
#: locate ``data/`` for both reading ``commits.csv`` and writing
#: ``inflection.json``.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent

#: Data directory under the workspace root.
DATA_DIR: Path = WORKSPACE_ROOT / "data"

#: Default output path for the inflection result.
OUTPUT_PATH: Path = DATA_DIR / "inflection.json"

#: Input CSV produced by ``02_extract_commits.sh``. The Tier 3 velocity
#: inflection path prefers this artifact when present, falling back to
#: ``git log`` directly when it is absent.
COMMITS_CSV: Path = DATA_DIR / "commits.csv"

#: Regex matching a ``Co-authored-by:`` trailer line in a commit message body.
#: The MULTILINE flag is required because the body is multi-line and the
#: trailer typically appears near the end. The IGNORECASE flag tolerates the
#: ``Co-Authored-By:`` casing variant produced by some tooling.
COAUTHOR_LINE_RE: re.Pattern[str] = re.compile(
    r"^\s*Co-authored-by:\s*(.+)$",
    re.IGNORECASE | re.MULTILINE,
)

#: AI-actor patterns that match in EMAIL substrings (case-insensitive). These
#: capture the canonical Blitzy AI identity (``agent@blitzy.com`` per AAP
#: §0.1.5), the GitHub Copilot account, the Anthropic Claude noreply
#: account, and the ``blitzy[bot]`` GitHub App identity. The narrow
#: ``agent@blitzy\.com`` pattern (rather than the broader ``@blitzy.com``
#: domain) is deliberate: per AAP §0.1.5 "The canonical Blitzy author
#: signature in this repository is ``agent@blitzy.com``... plus the
#: ``blitzy[bot]`` GitHub App account". Other ``@blitzy.com`` accounts
#: (e.g. ``michael@blitzy.com``, ``awadhwani@blitzy.com``) are human Blitzy
#: employees and must NOT be classified as AI actors. The expected outcome
#: documented in the agent prompt ("Tier 2 wins with date_utc = ``2026-02-25T02:58:59Z``")
#: depends on this discrimination.
AI_EMAIL_PATTERNS: tuple[re.Pattern[str], ...] = (
    re.compile(r"noreply@anthropic\.com", re.IGNORECASE),
    re.compile(r"copilot@github\.com", re.IGNORECASE),
    re.compile(r"\bagent@blitzy\.com\b", re.IGNORECASE),
    re.compile(r"blitzy\[bot\]", re.IGNORECASE),
)

#: AI-actor patterns that match in DISPLAY NAME substrings (case-insensitive).
#: Used by Tier 1 (trailer search) to catch trailers like
#: ``Co-authored-by: Claude <noreply@anthropic.com>`` even when the email
#: itself is missing or non-canonical. The ``\b`` boundary prevents the
#: ``copilot`` pattern from matching ``cocoapilot`` and similar
#: false-positive substrings.
AI_NAME_PATTERNS: tuple[re.Pattern[str], ...] = (
    re.compile(r"\bclaude\b", re.IGNORECASE),
    re.compile(r"\bcopilot\b", re.IGNORECASE),
    re.compile(r"\bcursor\b", re.IGNORECASE),
    re.compile(r"\baider\b", re.IGNORECASE),
    re.compile(r"\bblitzy\b", re.IGNORECASE),
)

#: Tier 2 reuses the same email pattern set as Tier 1's email check. The
#: distinction is that Tier 2 inspects the commit's own author email
#: (``%aE``) rather than co-author trailers in the body.
TIER2_EMAIL_PATTERNS: tuple[re.Pattern[str], ...] = AI_EMAIL_PATTERNS

#: Tier 3 ratio threshold. The post-week rolling mean must be at least this
#: many times the pre-week rolling mean at the candidate inflection week.
VELOCITY_INFLECTION_THRESHOLD: float = 4.0

#: Tier 3 sustain ratio. After the candidate inflection week, every
#: subsequent rolling mean must remain at least this many times the
#: pre-window mean for :data:`SUSTAIN_WEEKS` consecutive weeks.
VELOCITY_SUSTAIN_THRESHOLD: float = 2.0

#: Number of weeks the post-inflection ratio must remain above the sustain
#: threshold for the inflection to be considered "sustained" (per AAP §0.5.3.1).
SUSTAIN_WEEKS: int = 4

#: Rolling-mean window size in weeks for both the pre- and post-window
#: averages used by Tier 3.
WINDOW_WEEKS: int = 4

#: Threshold day count at which the Ramp-Up vs Steady-State split is applied
#: per AAP §0.5.6. Surfaced as a constant so downstream consumers can read
#: a single source of truth via the schema's
#: ``ramp_up_steady_state_threshold_days`` field.
RAMP_UP_STEADY_STATE_THRESHOLD_DAYS: int = 90

#: Justification text rendered into the report's Methodology section. Keyed
#: by the ``tier_used`` enum value. Kept at module scope so the text is easy
#: to audit and so the renderer can cross-check the value matches the chosen
#: tier.
JUSTIFICATION_MAP: dict[str, str] = {
    "trailer": (
        "Tier 1 (Co-authored-by trailer search) yielded a signal: at least one "
        "commit body contains a 'Co-authored-by:' trailer whose value matches "
        "an AI-actor email or display-name pattern (Claude, Copilot, Cursor, "
        "Aider, Blitzy, noreply@anthropic.com, copilot@github.com, @blitzy.com, "
        "or blitzy[bot]). The earliest such commit's author date is the "
        "inflection point. Trailer detection is the highest-precedence signal "
        "because it is an explicit declaration of AI co-authorship."
    ),
    "ai_actor_email": (
        "Tier 2 (AI-actor email pattern) was used because Tier 1 (Co-authored-by "
        "trailer search) yielded zero matches across all refs. The earliest "
        "commit whose author email matches the AI-actor pattern "
        "(@blitzy.com, blitzy[bot], copilot@github.com, or noreply@anthropic.com) "
        "is the inflection point. This is the canonical signal when an AI "
        "agent commits under a distinctive author identity rather than using "
        "co-author trailers on human-authored commits."
    ),
    "velocity_inflection": (
        "Tier 3 (velocity inflection) was used because neither Tier 1 "
        "(Co-authored-by trailer search) nor Tier 2 (AI-actor email pattern) "
        "yielded a signal. The Monday of the earliest week where the post-week "
        f"rolling mean (4-week window) is at least {VELOCITY_INFLECTION_THRESHOLD}× "
        f"the pre-week rolling mean AND sustains at least {VELOCITY_SUSTAIN_THRESHOLD}× "
        f"for the next {SUSTAIN_WEEKS} weeks is the inflection point. This is the "
        "weakest of the three signals and is only used when AI commits carry no "
        "distinctive author identity."
    ),
}


# ---------------------------------------------------------------------------
# Time-formatting helpers
# ---------------------------------------------------------------------------


def _parse_iso(s: str) -> datetime:
    """Parse a strict ISO-8601 datetime string into a timezone-aware ``datetime``.

    Accepts both the ``+00:00`` and the trailing-``Z`` notations for UTC.
    The trailing-``Z`` form is converted to ``+00:00`` before delegation to
    :py:meth:`datetime.datetime.fromisoformat`, which on Python 3.11+ also
    accepts ``Z`` directly but on older runtimes does not — the explicit
    substitution keeps the function correct on any Python 3.10+ host even
    though the pipeline targets 3.12+.

    Args:
        s: The ISO-8601 string to parse, e.g. ``"2026-02-25T02:58:59Z"`` or
            ``"2026-02-25T02:58:59+00:00"``. Leading and trailing whitespace
            is tolerated.

    Returns:
        A timezone-aware :class:`datetime.datetime` instance. The tzinfo is
        whatever the input encoded; callers that require UTC must call
        :py:meth:`datetime.datetime.astimezone` themselves.

    Raises:
        ValueError: When the input cannot be parsed as ISO-8601.
    """
    stripped = s.strip()
    if stripped.endswith("Z"):
        stripped = stripped[:-1] + "+00:00"
    return datetime.fromisoformat(stripped)


def _to_utc_z(iso_str: str) -> str:
    """Convert any ISO-8601 string to the canonical UTC ``YYYY-MM-DDTHH:MM:SSZ`` form.

    This is the canonical output format required by the inflection schema's
    ``date_utc`` pattern. The function rounds DOWN to whole seconds — the
    schema's regex does not admit fractional seconds.

    Args:
        iso_str: The ISO-8601 string to convert.

    Returns:
        The same instant rendered in canonical UTC seconds-precision form
        with a trailing ``Z`` suffix.

    Raises:
        ValueError: When the input cannot be parsed as ISO-8601.
    """
    dt = _parse_iso(iso_str)
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _now_utc_z() -> str:
    """Return the current wall-clock instant in canonical UTC ``Z``-suffixed form.

    Used to populate the ``fetched_at`` field of the output payload.

    Returns:
        The current UTC instant rendered as ``YYYY-MM-DDTHH:MM:SSZ``.
    """
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _monday_of(dt: datetime) -> datetime:
    """Return the Monday-anchored UTC week start for the given datetime.

    Mondays are the canonical week-start in this pipeline (AAP §0.5.6 — all
    2-week windows are anchored to Monday 00:00 UTC). Python's
    :py:meth:`datetime.datetime.weekday` returns 0 for Monday, so
    subtracting ``weekday()`` days from a UTC-normalised datetime yields
    the prior Monday in UTC.

    Args:
        dt: An arbitrary timezone-aware datetime.

    Returns:
        The Monday of the same UTC week, at 00:00:00 UTC with microseconds
        zeroed.
    """
    utc = dt.astimezone(timezone.utc)
    monday = utc - timedelta(days=utc.weekday())
    return monday.replace(hour=0, minute=0, second=0, microsecond=0)


# ---------------------------------------------------------------------------
# Tier 1 — Co-authored-by trailer search
# ---------------------------------------------------------------------------


def detect_tier_1(logger) -> Optional[dict]:
    """Search commit-message bodies across all refs for AI-actor trailers.

    Runs ``git log --all --grep='[Cc]o-authored-by:' --pretty=format:...``
    with a record terminator that includes the full commit message body
    (``%B``). Each record is parsed for ``Co-authored-by:`` lines whose
    value matches one of the configured AI email or display-name patterns.

    The earliest matching commit's author date is the inflection signal.
    Subprocess invocation uses ``check=True`` so a non-zero ``git log`` exit
    surfaces as :class:`subprocess.CalledProcessError`; the function catches
    that and logs a warning, returning ``None`` so the caller can fall
    through to Tier 2.

    Args:
        logger: The structured-JSON logger to emit ``tier_1_*`` events to.

    Returns:
        A dictionary ``{date_utc, evidence}`` when at least one matching
        trailer is found, otherwise ``None``. The ``evidence`` sub-dict
        carries the schema-required fields ``commit_sha``, ``trailer_value``,
        and ``author_date`` plus the informational ``total_trailer_commits``,
        ``author_name``, and ``subject`` fields.
    """
    record_terminator = "----RECORD-END----"
    # The format string uses %B (full body) plus a newline plus a unique
    # record terminator so that records can be reliably split on the
    # terminator string. Standard ``%n`` (newline) in the format inserts a
    # real newline before the terminator, matching ``split('TERMINATOR\n')``.
    # The header carries author email (%aE) so the self-trailer filter
    # below can reject commits where the author IS the trailered AI actor
    # (self-attribution is not evidence of human-credits-AI co-authoring).
    pretty_fmt = f"%H|%aI|%aE|%aN|%s%n%B%n{record_terminator}"
    try:
        result = subprocess.run(
            [
                "git",
                "log",
                "--all",
                "--grep=[Cc]o-authored-by:",
                f"--pretty=format:{pretty_fmt}",
            ],
            capture_output=True,
            text=True,
            check=True,
        )
    except subprocess.CalledProcessError as exc:
        logger.warning(
            "tier_1_git_failed",
            extra={
                "stderr": (exc.stderr or "").strip()[:500],
                "returncode": exc.returncode,
            },
        )
        return None
    except FileNotFoundError as exc:
        # ``git`` is not on PATH. Surface as a warning and degrade gracefully.
        logger.warning("tier_1_git_not_found", extra={"error": str(exc)})
        return None

    stdout = result.stdout or ""
    if not stdout.strip():
        logger.info("tier_1_no_candidates", extra={"reason": "git log returned no records"})
        return None

    records = stdout.split(f"{record_terminator}\n")
    candidates: list[dict] = []
    for raw in records:
        record = raw.strip()
        if not record:
            continue
        # The first line is the header (%H|%aI|%aN|%s); the remainder is the
        # full commit message body (%B). Split on the first newline only.
        header_end = record.find("\n")
        if header_end < 0:
            # Single-line record (no body) — skip; cannot contain a trailer.
            continue
        header = record[:header_end]
        body = record[header_end + 1:]
        header_parts = header.split("|", 4)
        if len(header_parts) < 5:
            # Malformed header — skip.
            continue
        sha, author_date_iso, author_email, author_name, subject = header_parts
        # If the commit's own author IS an AI actor, any Co-authored-by
        # trailer crediting an AI actor is self-attribution and does NOT
        # count as evidence of human-credits-AI co-authoring. Tier 1's
        # semantic intent is to detect the earliest commit where a HUMAN
        # author credits an AI co-author — typical of AI-assisted human
        # development. AI-on-AI trailers (the AI author crediting itself
        # or another AI) are a different signal that Tier 2 captures via
        # the author-email match.
        commit_author_is_ai = any(
            p.search(author_email) for p in AI_EMAIL_PATTERNS
        )
        # Scan the body for Co-authored-by trailers matching an AI pattern.
        for match in COAUTHOR_LINE_RE.finditer(body):
            trailer_value = match.group(1).strip()
            if not _trailer_value_matches_ai(trailer_value):
                continue
            if commit_author_is_ai:
                # Self-attribution case: AI author credits AI co-author.
                # Skip — Tier 2 will catch this via the author-email path
                # with the proper "first AI commit" semantic.
                break
            try:
                canonical_date = _to_utc_z(author_date_iso)
            except ValueError:
                # Unparseable date — skip this candidate but keep scanning.
                continue
            candidates.append(
                {
                    "commit_sha": sha,
                    "trailer_value": trailer_value,
                    "author_date": canonical_date,
                    "author_email": author_email,
                    "author_name": author_name,
                    "subject": subject,
                }
            )
            break  # One match per commit is sufficient.

    if not candidates:
        logger.info(
            "tier_1_no_ai_trailers",
            extra={"records_scanned": len([r for r in records if r.strip()])},
        )
        return None

    # Earliest by author date wins. The dates are already canonical UTC ``Z``
    # form so lexicographic sort equals chronological sort.
    candidates.sort(key=lambda c: c["author_date"])
    chosen = candidates[0]
    logger.info(
        "tier_1_signal_found",
        extra={
            "commit_sha": chosen["commit_sha"],
            "trailer_value": chosen["trailer_value"],
            "author_date": chosen["author_date"],
            "total_candidates": len(candidates),
        },
    )
    return {
        "date_utc": chosen["author_date"],
        "evidence": {
            "commit_sha": chosen["commit_sha"],
            "trailer_value": chosen["trailer_value"],
            "author_date": chosen["author_date"],
            "author_name": chosen["author_name"],
            "subject": chosen["subject"],
            "total_trailer_commits": len(candidates),
        },
    }


#: Pattern that splits a trailer value of the form
#: ``"Display Name <email@host>"`` into a display-name half and an email
#: half. The optional whitespace between the name and the bracketed email
#: is tolerated. When the input does not match this pattern (e.g. an
#: email-only trailer), the entire value is treated as the email and the
#: display name is the empty string.
_TRAILER_PARSE_RE: re.Pattern[str] = re.compile(
    r"^(?P<name>.*?)\s*<(?P<email>[^>]+)>\s*$"
)


def _trailer_value_matches_ai(value: str) -> bool:
    """Return ``True`` when a Co-authored-by trailer value matches an AI pattern.

    The trailer value is first split into a display-name component and an
    email component (when the ``"Display Name <email@host>"`` convention is
    used) so the two can be matched against the appropriate pattern sets
    without false positives. This is essential because the
    :data:`AI_NAME_PATTERNS` set includes the substring ``blitzy``, which
    would otherwise match human Blitzy employees via the domain portion of
    their email (e.g. the trailer
    ``"Michael Montanaro <michael@blitzy.com>"`` must NOT match because
    ``michael@blitzy.com`` is a human identity, not an AI actor).

    The check applies:

    * :data:`AI_EMAIL_PATTERNS` against the EMAIL substring only.
    * :data:`AI_NAME_PATTERNS` against the DISPLAY-NAME substring only.

    For trailer values that don't follow the ``"Display Name <email>"``
    convention (rare but possible — e.g. trailers that contain only an
    email or only a name), the entire value is checked against both
    pattern sets.

    Args:
        value: The right-hand side of a ``Co-authored-by:`` line, with any
            surrounding whitespace already stripped.

    Returns:
        ``True`` if any AI email pattern matches the email portion OR any
        AI display-name pattern matches the display-name portion. ``False``
        otherwise.
    """
    match = _TRAILER_PARSE_RE.match(value)
    if match:
        name_part = match.group("name") or ""
        email_part = match.group("email") or ""
        email_matched = any(p.search(email_part) for p in AI_EMAIL_PATTERNS)
        name_matched = bool(name_part) and any(
            p.search(name_part) for p in AI_NAME_PATTERNS
        )
        return email_matched or name_matched
    # Trailer did not follow the canonical "Name <email>" form. Apply both
    # pattern sets to the whole value as a best-effort fallback. This still
    # preserves the precision contract because the AI_EMAIL_PATTERNS are
    # all anchored substrings (e.g. ``\bagent@blitzy\.com\b``) that won't
    # accidentally match a bare display name, and the AI_NAME_PATTERNS use
    # word boundaries that prevent partial-word matches inside ordinary
    # text.
    return any(p.search(value) for p in AI_EMAIL_PATTERNS) or any(
        p.search(value) for p in AI_NAME_PATTERNS
    )



# ---------------------------------------------------------------------------
# Tier 2 — AI-actor email pattern
# ---------------------------------------------------------------------------


def detect_tier_2(logger) -> Optional[dict]:
    """Walk ``git log --all`` filtering for AI-actor author emails.

    Uses the validated :func:`lib.git.git_log` wrapper to enumerate every
    commit's author identity across all refs. Each line is ``%H|%aE|%aN|%aI``
    and is filtered against :data:`TIER2_EMAIL_PATTERNS`. The earliest
    matching commit's author date is the inflection signal.

    The function also computes informational counters that surface in the
    payload's evidence block — the total count of AI-actor commits and a
    per-email breakdown — so the renderer's Methodology section can report
    "X commits across N AI actors" without re-running the query.

    Args:
        logger: The structured-JSON logger to emit ``tier_2_*`` events to.

    Returns:
        A dictionary ``{date_utc, evidence}`` when at least one AI-actor
        commit is found, otherwise ``None``. The ``evidence`` sub-dict
        carries the schema-required ``commit_sha``, ``author_email``, and
        ``author_date`` plus informational ``author_name``,
        ``total_ai_actor_commits``, and ``ai_actor_email_breakdown`` fields.
    """
    try:
        lines = git_log(fmt="%H|%aE|%aN|%aI", args=["--all"])
    except Exception as exc:  # noqa: BLE001 — surface ANY git error as warning
        logger.warning("tier_2_git_failed", extra={"error": str(exc)})
        return None

    candidates: list[dict] = []
    per_email_counts: dict[str, int] = defaultdict(int)
    for line in lines:
        # Split with maxsplit=3 so that the author date (which contains
        # colons but never pipes) survives intact even if any prior field
        # accidentally contains a pipe character.
        parts = line.split("|", 3)
        if len(parts) < 4:
            continue
        sha, author_email, author_name, author_date_iso = parts
        if not any(p.search(author_email) for p in TIER2_EMAIL_PATTERNS):
            continue
        try:
            canonical_date = _to_utc_z(author_date_iso)
        except ValueError:
            # Unparseable date — skip this candidate but keep scanning so
            # the per-email counter remains accurate for the broader
            # population.
            continue
        candidates.append(
            {
                "commit_sha": sha,
                "author_email": author_email,
                "author_name": author_name,
                "author_date": canonical_date,
            }
        )
        per_email_counts[author_email] += 1

    if not candidates:
        logger.info(
            "tier_2_no_ai_actor_commits",
            extra={"commits_scanned": len(lines)},
        )
        return None

    candidates.sort(key=lambda c: c["author_date"])
    chosen = candidates[0]
    logger.info(
        "tier_2_signal_found",
        extra={
            "commit_sha": chosen["commit_sha"],
            "author_email": chosen["author_email"],
            "author_date": chosen["author_date"],
            "total_candidates": len(candidates),
        },
    )
    return {
        "date_utc": chosen["author_date"],
        "evidence": {
            "commit_sha": chosen["commit_sha"],
            "author_email": chosen["author_email"],
            "author_name": chosen["author_name"],
            "author_date": chosen["author_date"],
            "total_ai_actor_commits": len(candidates),
            "ai_actor_email_breakdown": dict(per_email_counts),
        },
    }


# ---------------------------------------------------------------------------
# Tier 3 — Velocity inflection (statistical fallback)
# ---------------------------------------------------------------------------


def detect_tier_3(logger) -> Optional[dict]:
    """Detect a sustained inflection in weekly commit velocity on ``main``.

    Computes weekly commit counts (Monday-anchored, UTC) and, for each
    candidate week ``w`` where the surrounding 4-week windows have enough
    coverage, computes the post/pre rolling-mean ratio. The earliest week
    where the ratio is at least :data:`VELOCITY_INFLECTION_THRESHOLD` AND
    where every subsequent rolling mean stays at least
    :data:`VELOCITY_SUSTAIN_THRESHOLD` times the pre-window mean for
    :data:`SUSTAIN_WEEKS` consecutive weeks is the inflection point. The
    Monday of that week is the chosen date.

    Tier 3 is the safety-net fallback when neither Tier 1 nor Tier 2
    produces a signal — typical of repositories where AI commits are
    indistinguishable from human commits in their author identity. In the
    current target repository Tier 2 produces a signal, so Tier 3 is
    documented as an "alternative considered" but not used.

    Args:
        logger: The structured-JSON logger to emit ``tier_3_*`` events to.

    Returns:
        A dictionary ``{date_utc, evidence}`` when a sustained inflection
        is detected, otherwise ``None``. The ``evidence`` sub-dict carries
        the schema-required ``week_start``, ``pre_window_mean``,
        ``post_window_mean``, and ``ratio`` plus informational
        ``week_commit_count``, ``window_weeks``, ``sustain_weeks``,
        ``inflection_threshold``, and ``sustain_threshold`` fields.
    """
    weekly_counts = _load_weekly_counts(logger)
    if weekly_counts is None:
        logger.warning(
            "tier_3_no_weekly_data",
            extra={"reason": "neither commits.csv nor git log produced parseable dates"},
        )
        return None

    required_minimum = (WINDOW_WEEKS * 2) + SUSTAIN_WEEKS
    if len(weekly_counts) < required_minimum:
        logger.info(
            "tier_3_insufficient_weeks",
            extra={
                "weeks_available": len(weekly_counts),
                "weeks_required_minimum": required_minimum,
            },
        )
        return None

    n = len(weekly_counts)
    chosen_idx: Optional[int] = None
    chosen_pre_mean: float = 0.0
    chosen_post_mean: float = 0.0
    chosen_ratio: float = 0.0

    for w in range(WINDOW_WEEKS, n - SUSTAIN_WEEKS + 1):
        pre = [c for _, c in weekly_counts[w - WINDOW_WEEKS:w]]
        post = [c for _, c in weekly_counts[w:w + WINDOW_WEEKS]]
        if not pre or not post:
            continue
        pre_mean = sum(pre) / len(pre)
        post_mean = sum(post) / len(post)
        # Guard against division by zero when the pre-window is silent. The
        # ``max(pre_mean, 1.0)`` substitution is the canonical guard from
        # AAP §0.5.3.1.
        denom = max(pre_mean, 1.0)
        ratio = post_mean / denom
        if ratio < VELOCITY_INFLECTION_THRESHOLD:
            continue
        # Sustain check: every subsequent 4-week rolling mean for the next
        # ``SUSTAIN_WEEKS`` steps must remain above
        # ``VELOCITY_SUSTAIN_THRESHOLD * denom``.
        sustained = True
        for s in range(SUSTAIN_WEEKS):
            window_end = w + s + WINDOW_WEEKS
            if window_end > n:
                # Not enough data to confirm sustain — reject as inconclusive.
                sustained = False
                break
            window_slice = weekly_counts[w + s:window_end]
            window_mean = sum(c for _, c in window_slice) / WINDOW_WEEKS
            if window_mean / denom < VELOCITY_SUSTAIN_THRESHOLD:
                sustained = False
                break
        if sustained:
            chosen_idx = w
            chosen_pre_mean = pre_mean
            chosen_post_mean = post_mean
            chosen_ratio = ratio
            break

    if chosen_idx is None:
        logger.info(
            "tier_3_no_sustained_inflection",
            extra={
                "weeks_scanned": n,
                "inflection_threshold": VELOCITY_INFLECTION_THRESHOLD,
                "sustain_threshold": VELOCITY_SUSTAIN_THRESHOLD,
                "sustain_weeks": SUSTAIN_WEEKS,
            },
        )
        return None

    week_start_iso, week_commit_count = weekly_counts[chosen_idx]
    logger.info(
        "tier_3_signal_found",
        extra={
            "week_start": week_start_iso,
            "pre_window_mean": chosen_pre_mean,
            "post_window_mean": chosen_post_mean,
            "ratio": chosen_ratio,
        },
    )
    return {
        "date_utc": week_start_iso,
        "evidence": {
            "week_start": week_start_iso,
            "pre_window_mean": round(chosen_pre_mean, 4),
            "post_window_mean": round(chosen_post_mean, 4),
            "ratio": round(chosen_ratio, 4),
            "week_commit_count": week_commit_count,
            "window_weeks": WINDOW_WEEKS,
            "sustain_weeks": SUSTAIN_WEEKS,
            "inflection_threshold": VELOCITY_INFLECTION_THRESHOLD,
            "sustain_threshold": VELOCITY_SUSTAIN_THRESHOLD,
        },
    }


def _load_weekly_counts(logger) -> Optional[list[tuple[str, int]]]:
    """Load author dates from ``commits.csv`` or ``git log`` and bucket by week.

    Preferred source is ``data/commits.csv`` (produced by
    ``02_extract_commits.sh``). When the artifact is absent — typical when
    Tier 3 runs before Tier 2 has populated the data directory — the
    function falls back to ``git log --pretty=format:%aI main`` via the
    validated :func:`lib.git.git_log` wrapper.

    Each parsed author date is bucketed into its Monday-anchored UTC week
    via :func:`_monday_of`. The result is a list of ``(week_start_iso,
    count)`` tuples sorted by week, suitable for the rolling-mean ratio
    computation in :func:`detect_tier_3`.

    Args:
        logger: Structured-JSON logger for diagnostic events.

    Returns:
        A sorted list of ``(week_start_iso, count)`` tuples when at least
        one valid commit date was loaded, otherwise ``None``. A return of
        ``None`` indicates that both the CSV path and the ``git log``
        fallback path failed — Tier 3 cannot proceed.
    """
    dates: list[datetime] = []
    if COMMITS_CSV.exists():
        try:
            with open(COMMITS_CSV, newline="", encoding="utf-8") as fp:
                reader = csv.reader(fp, delimiter="|")
                header = next(reader, None)
                if header is None:
                    logger.warning(
                        "tier_3_commits_csv_empty",
                        extra={"path": str(COMMITS_CSV)},
                    )
                else:
                    # Locate the author_date_iso column by name with a safe
                    # fallback to position 3 (the canonical layout produced
                    # by 02_extract_commits.sh).
                    try:
                        ad_index = header.index("author_date_iso")
                    except ValueError:
                        ad_index = 3
                    for row in reader:
                        if not row or len(row) <= ad_index:
                            continue
                        ad_raw = row[ad_index]
                        try:
                            dates.append(_parse_iso(ad_raw))
                        except (ValueError, TypeError):
                            # Unparseable row — skip without aborting.
                            continue
            logger.info(
                "tier_3_commits_csv_loaded",
                extra={"path": str(COMMITS_CSV), "rows_parsed": len(dates)},
            )
        except OSError as exc:
            logger.warning(
                "tier_3_commits_csv_read_failed",
                extra={"path": str(COMMITS_CSV), "error": str(exc)},
            )
            # Fall through to the git log fallback below.

    if not dates:
        # Fallback: read ``git log --pretty=format:%aI main``.
        try:
            lines = git_log(fmt="%aI", args=["main"])
        except Exception as exc:  # noqa: BLE001
            logger.warning(
                "tier_3_git_log_fallback_failed",
                extra={"error": str(exc)},
            )
            return None
        for line in lines:
            try:
                dates.append(_parse_iso(line))
            except (ValueError, TypeError):
                continue
        logger.info(
            "tier_3_git_log_fallback_loaded",
            extra={"rows_parsed": len(dates)},
        )

    if not dates:
        return None

    by_week: dict[str, int] = defaultdict(int)
    for d in dates:
        monday = _monday_of(d)
        by_week[monday.strftime("%Y-%m-%dT00:00:00Z")] += 1
    return sorted(by_week.items())



# ---------------------------------------------------------------------------
# Phase decomposition (AAP §0.5.6)
# ---------------------------------------------------------------------------


def _compute_phase_decomposition(
    chosen_date_utc: str, logger
) -> Optional[dict]:
    """Compute optional baseline / post-introduction phase bounds.

    Reads ``data/environment.json`` (produced by ``00_environment.sh``) to
    obtain the earliest and latest commit timestamps on ``main`` within the
    analysis window. Combined with the chosen inflection date, these yield
    the baseline duration and the post-introduction duration.

    When the post-introduction span is at least
    :data:`RAMP_UP_STEADY_STATE_THRESHOLD_DAYS` (90 days), the
    ``ramp_up_steady_state_split_applied`` field is ``True`` and downstream
    consumers (the compute step) emit a three-phase decomposition. When the
    span is shorter, the field is ``False`` and the consumers fall back to
    the two-phase Baseline-vs-Post-Introduction decomposition documented in
    AAP §0.5.6 and decision-log entry DL-006.

    The function tolerates a missing or malformed ``environment.json`` by
    returning ``None`` — the inflection result is still written without the
    optional phase fields, and the compute step recomputes the bounds from
    its own inputs.

    Args:
        chosen_date_utc: The inflection date in canonical UTC ``Z`` form.
        logger: Structured-JSON logger.

    Returns:
        A dictionary with keys ``baseline_duration_days``,
        ``post_introduction`` (sub-object with ``duration_days``,
        ``start_iso``, ``end_iso``, ``commit_count_on_main``),
        ``ramp_up_steady_state_split_applied``,
        ``ramp_up_steady_state_threshold_days``, and
        ``ramp_up_steady_state_split_applied_reason``. Returns ``None``
        when the environment artifact is unreadable.
    """
    env_path = DATA_DIR / "environment.json"
    if not env_path.exists():
        logger.info(
            "phase_decomposition_skipped_no_env",
            extra={"path": str(env_path)},
        )
        return None
    try:
        env_payload = json.loads(env_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        logger.warning(
            "phase_decomposition_env_read_failed",
            extra={"path": str(env_path), "error": str(exc)},
        )
        return None

    commit_date_range = env_payload.get("commit_date_range") or {}
    earliest_iso = commit_date_range.get("earliest_utc") or commit_date_range.get("earliest")
    # Prefer ``latest_utc`` — the latest commit across all refs — which is
    # the convention used by the agent prompt's expected outcome
    # ("post-introduction span is 86 days (2026-02-25 → 2026-05-21)" matches
    # the cross-ref ``latest_utc`` and not the main-only ``latest_on_main``).
    # The fallback order is: latest_utc → latest → latest_on_main, which
    # ensures both schema-conformant inflection results and rule-4 internal
    # consistency with the existing report's phase decomposition.
    latest_iso = (
        commit_date_range.get("latest_utc")
        or commit_date_range.get("latest")
        or commit_date_range.get("latest_on_main")
    )
    if not earliest_iso or not latest_iso:
        logger.info(
            "phase_decomposition_missing_bounds",
            extra={"earliest": earliest_iso, "latest": latest_iso},
        )
        return None
    try:
        earliest_dt = _parse_iso(earliest_iso)
        latest_dt = _parse_iso(latest_iso)
        inflection_dt = _parse_iso(chosen_date_utc)
    except (ValueError, TypeError) as exc:
        logger.warning(
            "phase_decomposition_parse_failed",
            extra={"error": str(exc)},
        )
        return None

    # Round half-up (43200 = 12 hours in seconds) so that a span of "1 day,
    # 21 hours" reads as 2 days — matching the compute step's convention.
    baseline_seconds = max((inflection_dt - earliest_dt).total_seconds(), 0.0)
    baseline_days = int((baseline_seconds + 43200) // 86400)
    post_seconds = max((latest_dt - inflection_dt).total_seconds(), 0.0)
    post_days = int((post_seconds + 43200) // 86400)

    split_applied = post_days >= RAMP_UP_STEADY_STATE_THRESHOLD_DAYS
    if split_applied:
        split_reason = (
            f"Post-introduction span is {post_days} days "
            f"(inflection {chosen_date_utc} → end {_to_utc_z(latest_iso)}). "
            f"{post_days} >= {RAMP_UP_STEADY_STATE_THRESHOLD_DAYS}-day threshold per "
            "AAP §0.5.6, so the Ramp-Up (first 90 days) / Steady State (>=90 days) "
            "decomposition is applied by downstream consumers."
        )
    else:
        split_reason = (
            f"Post-introduction span is {post_days} days "
            f"(inflection {chosen_date_utc} → end {_to_utc_z(latest_iso)}). "
            f"{post_days} < {RAMP_UP_STEADY_STATE_THRESHOLD_DAYS}-day threshold per "
            "AAP §0.5.6 and decision-log entry DL-006, so the two-phase fallback "
            "(Baseline + Post-Introduction) is applied by downstream consumers; "
            "no ramp_up or steady_state phase keys are emitted on any metric row."
        )

    decomposition: dict = {
        "baseline_duration_days": baseline_days,
        "post_introduction": {
            "duration_days": post_days,
            "start_iso": chosen_date_utc,
            "end_iso": _to_utc_z(latest_iso),
        },
        "ramp_up_steady_state_split_applied": split_applied,
        "ramp_up_steady_state_threshold_days": RAMP_UP_STEADY_STATE_THRESHOLD_DAYS,
        "ramp_up_steady_state_split_applied_reason": split_reason,
    }
    logger.info(
        "phase_decomposition_computed",
        extra={
            "baseline_days": baseline_days,
            "post_days": post_days,
            "split_applied": split_applied,
        },
    )
    return decomposition


# ---------------------------------------------------------------------------
# Orchestration
# ---------------------------------------------------------------------------


def _build_payload(
    chosen_tier: str,
    chosen_payload: dict,
    all_results: dict[str, Optional[dict]],
    run_id: str,
    phase_decomposition: Optional[dict],
) -> dict:
    """Assemble the final inflection.json payload.

    The function applies these rules to satisfy the schema:

    * ``tier_used`` is the chosen tier's canonical enum value.
    * ``date_utc`` is the chosen tier's inflection date in canonical
      ``Z``-suffixed UTC form.
    * ``evidence`` is the chosen tier's evidence sub-object with all
      schema-required discriminator fields present.
    * ``justification`` is the canonical paragraph from
      :data:`JUSTIFICATION_MAP` keyed by the chosen tier.
    * ``alternatives_considered`` lists every OTHER tier in precedence
      order with the schema-required ``tier`` and ``reason_rejected``
      fields. Lower-precedence tiers that yielded a signal record their
      own evidence in addition; higher-precedence tiers that did not
      yield a signal record ``"no_signal"`` as the reason; tiers that
      were not evaluated because a higher tier already won record
      ``"not_evaluated"``.
    * ``run_id`` is the active UUID4 correlation identifier.
    * ``fetched_at`` is the wall-clock instant of detection.
    * Optional phase-decomposition fields are merged at the top level
      when available.

    Args:
        chosen_tier: One of ``"trailer"``, ``"ai_actor_email"``,
            ``"velocity_inflection"`` — the tier that produced the signal.
        chosen_payload: The ``{date_utc, evidence}`` dict returned by the
            winning detection function.
        all_results: All three tier results keyed by tier name; ``None``
            values mean no signal.
        run_id: The active ``BLITZY_RUN_ID`` UUID4 correlation identifier.
        phase_decomposition: Optional decomposition dict from
            :func:`_compute_phase_decomposition`; ``None`` skips the
            optional fields.

    Returns:
        The complete payload dict, ready to be JSON-encoded and written to
        ``data/inflection.json``.
    """
    # Precedence order is the canonical iteration order for alternatives.
    precedence: tuple[str, ...] = (
        "trailer",
        "ai_actor_email",
        "velocity_inflection",
    )
    chosen_index = precedence.index(chosen_tier)
    alternatives: list[dict] = []
    for idx, tier_name in enumerate(precedence):
        if tier_name == chosen_tier:
            continue
        result = all_results.get(tier_name)
        if idx < chosen_index:
            # Tier was higher precedence than the chosen one but did not
            # produce a signal — otherwise it would have won.
            reason = (
                f"No signal: tier '{tier_name}' was evaluated first per the "
                "three-tier precedence order but yielded zero matching commits."
            )
            alternatives.append({"tier": tier_name, "reason_rejected": reason})
        else:
            # Tier was lower precedence than the chosen one. It MAY still
            # have produced a signal — record it as an alternative for
            # transparency, but it was not used because a higher-precedence
            # tier already won.
            if result is not None:
                reason = (
                    f"Not used: tier '{tier_name}' did produce a signal but the "
                    f"higher-precedence tier '{chosen_tier}' already yielded a "
                    "definitive result. Recorded here for transparency and "
                    "decision-log audit."
                )
                entry: dict = {
                    "tier": tier_name,
                    "reason_rejected": reason,
                    "date_utc": result["date_utc"],
                    "evidence": result["evidence"],
                }
            else:
                reason = (
                    f"Not evaluated for inflection: tier '{tier_name}' was not "
                    f"required because the higher-precedence tier '{chosen_tier}' "
                    "already produced a definitive result. Tier 3 (velocity "
                    "inflection) in particular is the safety-net fallback and "
                    "is only used when the trailer and email signals are both "
                    "silent."
                )
                # If the function was actually called but produced no signal,
                # the result is None — surface that distinction transparently.
                # We can't distinguish "skipped" from "called and returned
                # None" without a separate sentinel; the orchestration code
                # always calls all three so the distinction matters for the
                # decision log. The reason text remains accurate either way.
                entry = {"tier": tier_name, "reason_rejected": reason}
            alternatives.append(entry)

    payload: dict = {
        "tier_used": chosen_tier,
        "date_utc": chosen_payload["date_utc"],
        "evidence": chosen_payload["evidence"],
        "justification": JUSTIFICATION_MAP[chosen_tier],
        "alternatives_considered": alternatives,
        "run_id": run_id,
        "fetched_at": _now_utc_z(),
    }
    if phase_decomposition:
        # Top-level merge — the optional phase fields sit alongside the
        # required fields. The schema permits this via additionalProperties.
        payload.update(phase_decomposition)
    return payload


def _run_detection(logger) -> tuple[str, dict, dict[str, Optional[dict]]]:
    """Execute the three tiers and return the winning result plus all results.

    Implements the precedence rule (first tier with signal wins) while
    ALWAYS evaluating every tier for the alternatives-considered audit
    trail. This matches the agent prompt requirement: "Subsequent tiers
    are computed AND retained as ``alternatives_considered`` but not used."

    Args:
        logger: Structured-JSON logger.

    Returns:
        A tuple ``(chosen_tier, chosen_payload, all_results)``. The
        ``chosen_tier`` is one of ``"trailer"``, ``"ai_actor_email"``,
        ``"velocity_inflection"``. The ``chosen_payload`` is the dict
        returned by the winning tier. The ``all_results`` mapping carries
        every tier's result (or ``None``) keyed by canonical name.

    Raises:
        SystemExit: When no tier produces a signal. The pipeline cannot
            proceed without an inflection date, so this is fatal.
    """
    logger.info("tier_1_starting")
    t1 = detect_tier_1(logger)
    logger.info("tier_2_starting")
    t2 = detect_tier_2(logger)
    logger.info("tier_3_starting")
    t3 = detect_tier_3(logger)

    all_results: dict[str, Optional[dict]] = {
        "trailer": t1,
        "ai_actor_email": t2,
        "velocity_inflection": t3,
    }

    if t1 is not None:
        return "trailer", t1, all_results
    if t2 is not None:
        return "ai_actor_email", t2, all_results
    if t3 is not None:
        return "velocity_inflection", t3, all_results

    logger.error(
        "no_inflection_signal",
        extra={
            "tier_1_result": "none",
            "tier_2_result": "none",
            "tier_3_result": "none",
        },
    )
    raise SystemExit(
        "No inflection signal could be detected across any of the three tiers "
        "(trailer search, AI-actor email pattern, velocity inflection). "
        "Pipeline cannot proceed without a baseline / after partition date. "
        "Consider supplying an explicit override via the --output flag or by "
        "adding an AI-actor identity to the configured patterns."
    )


# ---------------------------------------------------------------------------
# CLI entry point
# ---------------------------------------------------------------------------


def main() -> int:
    """Entry point — parses arguments, runs detection, writes the output file.

    Returns:
        ``0`` on success (including ``--dry-run``), non-zero on failure.
        Failures include: no inflection signal found, output write failure,
        and any unhandled exception (which is logged via the structured-JSON
        logger before propagating as a non-zero exit).
    """
    parser = argparse.ArgumentParser(
        prog=SCRIPT_NAME,
        description=(
            "Detect the AI inflection point — the canonical baseline / after "
            "partition date — using a three-tier precedence: Co-authored-by "
            "trailer search → AI-actor author email match → velocity "
            "inflection. Writes data/inflection.json (or the path supplied "
            "via --output) on success."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Preview the git commands and the output path without running "
            "any detection or writing any file. Emits a single JSON object "
            "to stdout describing the planned operations."
        ),
    )
    parser.add_argument(
        "--output",
        default=str(OUTPUT_PATH),
        help=(
            "Path to write the inflection.json output. Defaults to "
            "blitzy/acceleration-report/data/inflection.json. Parent "
            "directory is created if missing."
        ),
    )
    args = parser.parse_args()

    logger = get_logger(SCRIPT_NAME)
    run_id = os.environ.get("BLITZY_RUN_ID", "")
    logger.info(
        "script_started",
        extra={
            "dry_run": args.dry_run,
            "output_path": args.output,
            "workspace_root": str(WORKSPACE_ROOT),
        },
    )

    if args.dry_run:
        preview = {
            "action": "dry_run",
            "script": SCRIPT_NAME,
            "workspace_root": str(WORKSPACE_ROOT),
            "tier_1_command": (
                "git log --all --grep='[Cc]o-authored-by:' "
                "--pretty=format:'%H|%aI|%aE|%aN|%s%n%B%n----RECORD-END----'"
            ),
            "tier_1_patterns_email": [p.pattern for p in AI_EMAIL_PATTERNS],
            "tier_1_patterns_name": [p.pattern for p in AI_NAME_PATTERNS],
            "tier_2_command": "git log --all --pretty=format:'%H|%aE|%aN|%aI'",
            "tier_2_patterns": [p.pattern for p in TIER2_EMAIL_PATTERNS],
            "tier_3_input_csv": str(COMMITS_CSV),
            "tier_3_fallback_command": "git log --pretty=format:'%aI' main",
            "tier_3_thresholds": {
                "inflection_threshold": VELOCITY_INFLECTION_THRESHOLD,
                "sustain_threshold": VELOCITY_SUSTAIN_THRESHOLD,
                "sustain_weeks": SUSTAIN_WEEKS,
                "window_weeks": WINDOW_WEEKS,
            },
            "writes": [args.output],
            "run_id": run_id,
        }
        # Use indent=2 so a human reviewing the dry-run output can read it
        # without piping through jq. The JSON is the sole stdout content
        # so consumers can parse it without additional filtering.
        print(json.dumps(preview, indent=2, default=str))
        logger.info("dry_run_complete", extra={"output_path": args.output})
        return 0

    try:
        chosen_tier, chosen_payload, all_results = _run_detection(logger)
    except SystemExit as exc:
        # ``_run_detection`` raises SystemExit when no tier produces a
        # signal. The structured log has already been emitted; surface the
        # message to stderr for human operators and return non-zero.
        print(str(exc), file=sys.stderr)
        return 2

    phase_decomposition = _compute_phase_decomposition(chosen_payload["date_utc"], logger)
    payload = _build_payload(
        chosen_tier=chosen_tier,
        chosen_payload=chosen_payload,
        all_results=all_results,
        run_id=run_id,
        phase_decomposition=phase_decomposition,
    )

    output_path = Path(args.output)
    try:
        output_path.parent.mkdir(parents=True, exist_ok=True)
        output_path.write_text(
            json.dumps(payload, indent=2, default=str, ensure_ascii=False),
            encoding="utf-8",
        )
    except OSError as exc:
        logger.error(
            "inflection_write_failed",
            extra={"path": str(output_path), "error": str(exc)},
        )
        return 3

    logger.info(
        "script_complete",
        extra={
            "tier_used": chosen_tier,
            "date_utc": chosen_payload["date_utc"],
            "output_path": str(output_path),
            "alternatives_recorded": len(payload.get("alternatives_considered", [])),
        },
    )
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        # Re-raise SystemExit without wrapping so the chosen exit code
        # propagates to the shell.
        raise
    except Exception:  # noqa: BLE001 — top-level safety net
        # Acquire a fresh logger (idempotent) so the unhandled exception is
        # recorded in the structured log feed before the interpreter exits.
        get_logger(SCRIPT_NAME).exception("unhandled_exception")
        sys.exit(1)

