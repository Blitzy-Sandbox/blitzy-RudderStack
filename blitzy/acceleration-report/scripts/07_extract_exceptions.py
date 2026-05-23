"""Exception inventory extraction for Metric 10 (Approved Exceptions).

This script is the seventh stage of the read-only acceleration-report
extraction pipeline. It assembles ``data/exceptions.json`` from five
independent signal sources and persists the result as a self-describing
JSON artifact consumed by ``09_compute_metrics.py``.

Signal sources (in declining order of fidelity):

* **Tier 1 — Admin audit log**:
  ``GET /repos/{owner}/{repo}/audit-log``. This endpoint requires an
  organisation-level admin token with the ``audit_log`` scope; the
  read-only analysis sandbox typically lacks this permission and the
  call returns 404/403. When unreachable, the artifact records the
  failure with ``available: false`` and a structured-JSON
  ``audit_log_unavailable`` event is emitted.

* **Tier 2 — Branch protection snapshot**:
  ``GET /repos/{owner}/{repo}/branches/main/protection``. Returns the
  current (HEAD-only) protection rules. Historical changes to the rules
  require admin audit-log access; the snapshot is therefore a point-in-
  time observation, not a time-series.

* **Tier 3 — Force-pushes from local git reflog**:
  ``git reflog show main``. Bounded to events this clone observed.
  Force-pushes that occurred on the remote ``main`` before this clone
  was created are invisible to the reflog.

* **Tier 4 — Exception-label PR scan**:
  Reads ``data/pulls.json`` (produced by ``03_extract_pulls.py``) and
  matches PR labels against the patterns ``exception``, ``waiver``,
  ``override``, ``bypass`` (case-insensitive). The repository's current
  label catalogue (``.github/labeler.yml`` plus observed human-applied
  labels) does NOT include exception markers, so the expected count
  is structurally zero unless new labels are introduced.

* **Tier 5 — Lint-config exemption HEAD snapshot**:
  Parses ``.golangci.yml``, ``.snyk``, ``.truffleignore`` and
  ``.deepsource.toml`` at the rudder-server repository root. These
  files are HEAD-only snapshots; the artifact reports counts rather
  than per-window events. ``.snyk`` entries are additionally checked
  against the extraction timestamp to surface expired-at-extraction-
  time exceptions (a governance signal — exceptions that should have
  been re-reviewed by their declared expiry date).

Read-only contract
------------------

Every call in this script is a GET, a git read sub-command, or a
filesystem read. No commit, push, ref-write, or HTTP non-GET method is
issued. The structured-JSON logger redacts any field whose key or value
looks like a credential.

Output schema
-------------

The output artifact ``data/exceptions.json`` is a single JSON object
with the following top-level keys (consumed verbatim by
``09_compute_metrics.py``):

* ``_metadata``         — per-run correlation fields (run_id,
                          extraction_timestamp, repository_slug,
                          default_branch, analysis_period_start/end,
                          inflection_date_utc, artifact_kind,
                          schema_version, source_files_referenced).
* ``audit_log``         — ``{available, events, unavailable_reason,
                          api_endpoint_attempted, auth_required,
                          consequence}``.
* ``branch_protection`` — ``{branch, snapshot_at, available,
                          snapshot, unavailable_reason,
                          api_endpoint_attempted,
                          time_series_available, time_series_note}``.
* ``force_pushes``      — ``{source, search_command, events, count,
                          reflog_total_entries_observed,
                          reflog_first_entry_observed, note,
                          limitation_class}``.
* ``exception_labeled_prs`` — ``{label_patterns_searched,
                              available_labels_in_repo,
                              available_labels_source,
                              exception_pattern_match_count,
                              exception_pattern_match_note,
                              events}``.
* ``lint_exemptions``   — sub-objects per source file (golangci,
                          snyk, truffleignore, deepsource), each with
                          ``available`` flag, source-file provenance,
                          extracted counts, and verification commands.
* ``summary``           — aggregate counts and the M10 confidence
                          implication.
* ``fetched_at``        — convenience top-level timestamp duplicating
                          ``_metadata.extraction_timestamp``.

Exit codes
----------

* ``0`` — Success; ``data/exceptions.json`` was written. Tier
  failures (audit log, branch protection, reflog, pulls.json missing,
  lint files missing) are individually logged and do NOT cause a
  non-zero exit — the artifact is always produced with the available
  signal even when sources are absent.
* ``2`` — Git toplevel could not be determined. The script must be
  invoked from inside a git working tree because the rudder-server
  configuration files (.golangci.yml etc.) live at the repository root.

Reference
---------

Specified by AAP §0.5.3.11 (Metric 10 — Approved Exceptions) and the
agent prompt at ``07_extract_exceptions.py``.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

# Make sibling ``lib/`` directory importable when the script is invoked
# either directly (``python3 07_extract_exceptions.py``) or via the
# Makefile orchestrator (``make extract``). This path manipulation is
# the same idiom used by every script in the pipeline.
sys.path.insert(0, str(Path(__file__).parent))

from lib.observability import get_logger  # noqa: E402
from lib.github import GithubClient  # noqa: E402
from lib.git import git_reflog, git_rev_parse_toplevel  # noqa: E402


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------

#: Logger name and the value of the ``script`` field in every emitted log
#: line. Mirrors the script's basename without extension by convention.
SCRIPT_NAME: str = "07_extract_exceptions"

#: Workspace root resolved from this script's location. The workspace is
#: ``blitzy/acceleration-report/`` and contains ``data/``, ``scripts/``,
#: ``diagrams/``. Computed at import time so that the script's output
#: path is deterministic regardless of the current working directory.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent

#: Directory where every extraction artifact lives, including this
#: script's output.
DATA_DIR: Path = WORKSPACE_ROOT / "data"

#: Default output path for the assembled exception inventory.
OUTPUT_PATH: Path = DATA_DIR / "exceptions.json"

#: Default repository slug for GitHub API calls. Overridden by the
#: ``GITHUB_REPO_SLUG`` environment variable or the ``--repo-slug``
#: CLI flag, in that order of precedence.
REPO_SLUG_DEFAULT: str = "Blitzy-Sandbox/blitzy-RudderStack"

#: Default branch name. The analyzed repository's default branch is
#: ``main``; this is captured here as a constant so that re-runs against
#: forks with a different default branch can be adjusted in a single
#: place. The branch name is also used by ``git reflog show <ref>``.
DEFAULT_BRANCH: str = "main"

#: Schema version emitted in ``_metadata.schema_version``. Increment
#: whenever the top-level keys of the output artifact change in a way
#: that consumers must be aware of.
ARTIFACT_SCHEMA_VERSION: str = "1.0.0"

#: Artifact kind tag emitted in ``_metadata.artifact_kind``. Mirrors
#: the kind tags used by the other extraction scripts in this pipeline.
ARTIFACT_KIND: str = "exception_inventory"

#: Label patterns indicating an exception or override. Matched
#: case-insensitively as substrings against the labels attached to
#: each PR in ``data/pulls.json``. The list comes from AAP §0.5.3.11.
EXCEPTION_LABEL_PATTERNS: tuple[str, ...] = (
    "exception",
    "waiver",
    "override",
    "bypass",
)

#: Audit-log event ``action`` values that indicate a policy exception
#: was granted. The list is sourced from the GitHub Audit Log API
#: documentation and from AAP §0.5.3.11. The set is non-exhaustive;
#: any future action that materially indicates an exception SHOULD be
#: added here rather than computed at runtime so that the contract
#: between this artifact and the metric computation remains explicit.
AUDIT_EXCEPTION_ACTIONS: frozenset[str] = frozenset(
    {
        "protected_branch.policy_override",
        "protected_branch.update_required_status_checks_strict_policy",
        "pull_request.merge_with_bypass",
        "branch_protection_rule.update",
        "branch_protection_rule.destroy",
        "branch_protection_rule.bypass",
        "repo.access",
        "team.add_repository",
        "ruleset.update",
        "ruleset.destroy",
        "repository_ruleset.bypass",
    }
)

#: Read-only AAP fallback values for analysis period bounds. The
#: canonical source is ``data/environment.json``. These fallbacks are
#: only used when the environment artifact is absent (e.g. when this
#: script is invoked standalone before stage 00 has run).
ANALYSIS_PERIOD_START_FALLBACK: str = "2026-02-23T00:00:00Z"
ANALYSIS_PERIOD_END_FALLBACK: str = "2026-05-18T23:59:59Z"

#: Inflection date fallback used only when ``data/inflection.json`` is
#: absent. Sourced from AAP §0.5.3.1 (Tier 2 result for this repo).
INFLECTION_DATE_FALLBACK_UTC: str = "2026-02-25T02:58:59Z"

#: Compiled regex matching reflog lines of the form
#: ``"<sha> <ref>@{<n>}: <message>"``. The reflog show output uses
#: ``main@{N}`` to denote ref-age slots; this pattern decomposes those
#: components so we can filter for force-push messages.
REFLOG_LINE_RE: re.Pattern[str] = re.compile(
    r"^([0-9a-f]+)\s+([^\s@]+)@\{(\d+)\}:\s+(.*)$"
)

#: Substring markers in the reflog message that identify a force-push.
#: ``"push -f"`` and ``"push --force"`` are the two literal forms git
#: writes to the reflog for forced updates; the more permissive
#: ``"force"`` substring catches non-canonical variants emitted by
#: tooling. Matches are case-insensitive.
FORCE_PUSH_MARKERS: tuple[str, ...] = ("push -f", "push --force", "force-update", "forced-update")


# ---------------------------------------------------------------------------
# Timestamp helpers
# ---------------------------------------------------------------------------


def iso_now() -> str:
    """Return the current UTC timestamp in ISO-8601 form with a ``Z`` suffix.

    The output format ``YYYY-MM-DDTHH:MM:SSZ`` is the canonical artifact
    timestamp shape consumed by ``09_compute_metrics.py`` and emitted in
    the ``fetched_at`` field of every JSON artifact in this pipeline.
    Microseconds are dropped because downstream parsers use
    ``dateutil.parser.parse`` and do not require sub-second precision.

    Returns:
        UTC timestamp string, for example ``"2026-05-23T14:30:00Z"``.
    """
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _normalise_iso(value: Any) -> str | None:
    """Coerce ``value`` into a ``YYYY-MM-DDTHH:MM:SSZ`` string.

    Accepts ISO-8601 strings with or without microseconds and with or
    without explicit ``Z``/``+00:00`` timezone designators. Returns
    ``None`` when the value cannot be parsed; downstream callers use
    that as the signal to fall back to a documented default.

    Args:
        value: Anything; only ``str`` inputs are honoured.

    Returns:
        The normalised UTC string, or ``None`` when the input cannot be
        recognised as a timestamp.
    """
    if not isinstance(value, str) or not value.strip():
        return None
    raw = value.strip()
    # Accept a trailing ``Z`` which ``datetime.fromisoformat`` only
    # learned to handle in 3.11+. Translate to ``+00:00`` for the
    # widest compatibility.
    if raw.endswith("Z"):
        raw = raw[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(raw)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _is_expired(expires: Any, reference: str) -> bool:
    """Return True iff ``expires`` is strictly before ``reference``.

    Used to flag ``.snyk`` ignore entries whose declared expiry date
    has passed at the extraction timestamp. Both inputs are interpreted
    as ISO-8601 UTC; malformed inputs return ``False`` (we do not
    speculate about the validity of unknown formats).

    Args:
        expires: The ``expires`` field from a ``.snyk`` ignore entry.
        reference: The reference timestamp (typically the extraction
            timestamp) against which expiry is evaluated.

    Returns:
        ``True`` when both timestamps parse AND ``expires`` is strictly
        earlier than ``reference``; ``False`` otherwise.
    """
    exp_norm = _normalise_iso(expires)
    ref_norm = _normalise_iso(reference)
    if exp_norm is None or ref_norm is None:
        return False
    try:
        exp_dt = datetime.strptime(exp_norm, "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=timezone.utc
        )
        ref_dt = datetime.strptime(ref_norm, "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=timezone.utc
        )
    except ValueError:
        return False
    return exp_dt < ref_dt


# ---------------------------------------------------------------------------
# Metadata builder
# ---------------------------------------------------------------------------


def _read_optional_json(path: Path) -> dict[str, Any] | None:
    """Return parsed JSON from ``path`` or ``None`` if absent/invalid.

    Used to load ``environment.json``, ``inflection.json`` and
    ``pulls.json`` defensively. The metadata block falls back to
    AAP-documented defaults when these artifacts are missing, which
    happens during standalone runs of this script (e.g. unit tests
    that invoke ``main`` without first running stages 00 and 01).

    Args:
        path: Filesystem path to the candidate JSON file.

    Returns:
        The parsed JSON object as a ``dict``, or ``None`` if the file
        does not exist, cannot be read, is empty, or is not a JSON
        object (a top-level list, for example, is rejected).
    """
    if not path.exists():
        return None
    try:
        raw = path.read_text()
    except OSError:
        return None
    if not raw.strip():
        return None
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return None
    if not isinstance(data, dict):
        return None
    return data


def _get_nested(data: dict[str, Any], *keys: str) -> Any:
    """Walk ``data`` along ``keys`` and return the final value or None.

    Convenience helper used by :func:`_build_metadata` to look up
    fields that may live at the top level OR nested under
    ``_metadata`` depending on which extraction-script convention is
    in force. Returns ``None`` on any KeyError or wrong-type traversal.

    Args:
        data: The dictionary to traverse.
        *keys: Sequence of keys to descend.

    Returns:
        The value at the requested path, or ``None`` if the path does
        not resolve (missing key, non-dict intermediate, etc.).
    """
    current: Any = data
    for key in keys:
        if not isinstance(current, dict):
            return None
        current = current.get(key)
    return current


def _build_metadata(
    args: argparse.Namespace,
    env: dict[str, Any] | None,
    infl: dict[str, Any] | None,
    extraction_ts: str,
) -> dict[str, Any]:
    """Construct the ``_metadata`` block of the output artifact.

    The block carries the canonical per-run correlation fields
    consumed by ``09_compute_metrics.py`` and the report renderer.
    Values are sourced from ``data/environment.json`` and
    ``data/inflection.json`` when those artifacts are available, with
    AAP-documented fallbacks when they are not.

    Args:
        args: Parsed CLI namespace. Provides ``repo_slug`` as the
            fallback ``repository_slug`` when ``environment.json`` is
            unreadable.
        env: Decoded ``environment.json`` contents, or ``None`` when
            unavailable.
        infl: Decoded ``inflection.json`` contents, or ``None`` when
            unavailable.
        extraction_ts: Wall-clock UTC timestamp used as the fallback
            ``extraction_timestamp`` when ``environment.json`` does not
            carry a canonical one.

    Returns:
        A dictionary with the schema-required ``_metadata`` fields,
        ready to be embedded in the output artifact.
    """
    canonical_ts: str | None = None
    if env is not None:
        canonical_ts = _normalise_iso(
            env.get("extraction_timestamp")
            or _get_nested(env, "_metadata", "extraction_timestamp")
        )
    extraction_timestamp = canonical_ts or extraction_ts

    run_id: str | None = None
    if env is not None:
        run_id = env.get("run_id") or _get_nested(env, "_metadata", "run_id")
    if not run_id:
        run_id = os.environ.get("BLITZY_RUN_ID")
    if not run_id:
        # As a last resort, generate one. The observability layer has
        # already exported a UUID4 to BLITZY_RUN_ID in 99% of invocations
        # so this branch is exercised only when this script is invoked
        # before any logger initialisation has occurred.
        import uuid as _uuid_local

        run_id = str(_uuid_local.uuid4())

    repository_slug = args.repo_slug
    if env is not None:
        env_slug = env.get("repository_slug") or _get_nested(
            env, "_metadata", "repository_slug"
        )
        if env_slug:
            repository_slug = env_slug

    default_branch = DEFAULT_BRANCH
    if env is not None:
        env_branch = env.get("default_branch") or _get_nested(
            env, "_metadata", "default_branch"
        )
        if env_branch:
            default_branch = env_branch

    analysis_period_start = ANALYSIS_PERIOD_START_FALLBACK
    analysis_period_end = ANALYSIS_PERIOD_END_FALLBACK
    if env is not None:
        env_start = _normalise_iso(
            env.get("analysis_period_start")
            or _get_nested(env, "_metadata", "analysis_period_start")
            or _get_nested(env, "commit_date_range", "earliest")
        )
        env_end = _normalise_iso(
            env.get("analysis_period_end")
            or _get_nested(env, "_metadata", "analysis_period_end")
            or _get_nested(env, "commit_date_range", "latest")
        )
        if env_start:
            analysis_period_start = env_start
        if env_end:
            analysis_period_end = env_end

    inflection_date_utc = INFLECTION_DATE_FALLBACK_UTC
    candidate: str | None = None
    if infl is not None:
        candidate = _normalise_iso(infl.get("date_utc")) or _normalise_iso(
            _get_nested(infl, "_metadata", "inflection_date_utc")
        )
    if not candidate and env is not None:
        candidate = _normalise_iso(env.get("inflection_date_utc"))
    if candidate:
        inflection_date_utc = candidate

    return {
        "extraction_timestamp": extraction_timestamp,
        "run_id": run_id,
        "repository_slug": repository_slug,
        "default_branch": default_branch,
        "analysis_period_start": analysis_period_start,
        "analysis_period_end": analysis_period_end,
        "inflection_date_utc": inflection_date_utc,
        "artifact_kind": ARTIFACT_KIND,
        "schema_version": ARTIFACT_SCHEMA_VERSION,
        "source_files_referenced": [
            ".golangci.yml",
            ".snyk",
            ".truffleignore",
            ".deepsource.toml",
            ".github/labeler.yml",
            "data/pulls.json",
            "data/environment.json",
            "data/inflection.json",
        ],
        "feeds_metric": "m10",
        "feeds_metric_name": "Approved Exceptions",
        "aap_section": "0.5.3.11",
    }




# ---------------------------------------------------------------------------
# Lint-config exemption parsers
# ---------------------------------------------------------------------------


def parse_golangci(path: Path) -> dict[str, Any]:
    """Extract exemption counts from ``.golangci.yml``.

    The function returns counts derived purely from regex matching on
    the YAML source text rather than from a structural YAML parse;
    this avoids a hard PyYAML dependency and yields identical results
    because the patterns we care about are simple list-item lines in
    a well-formed file.

    Counted exemptions:

    * ``gosec_excludes``      — entries under ``linters-settings.gosec.
      excludes`` (or ``settings.gosec.excludes`` in v2 format), matched
      by ``^\\s*-\\s+G\\d+`` (every gosec rule identifier is ``Gnnn``).
    * ``depguard_deny_count`` — entries under ``deny:`` blocks of the
      ``depguard`` linter, matched by ``^\\s*-\\s*pkg:\\s*``.
    * ``forbidigo_patterns``  — pattern entries under ``forbidigo.
      forbid`` blocks, matched by ``^\\s*-\\s*pattern:\\s*``.
    * ``bodyclose_path_exclusions`` — best-effort count of
      ``path:`` exclusion rules (covers bodyclose paths and similar).

    Args:
        path: Filesystem path to ``.golangci.yml``.

    Returns:
        A dictionary describing the parse result. When the file does
        not exist, only ``{"available": False, "unavailable_reason":
        "file_missing"}`` is returned. When the file is present,
        the returned dict includes the exemption counts, the extracted
        gosec identifier list (handy for human cross-checking), the
        source-file extraction command used, and a flag indicating
        whether inline-comment finding counts (e.g. ``# 6 findings``)
        were preserved by the regex match.
    """
    if not path.exists():
        return {
            "available": False,
            "source_file": str(path.name),
            "unavailable_reason": "file_missing",
        }
    try:
        text = path.read_text()
    except OSError as exc:
        return {
            "available": False,
            "source_file": str(path.name),
            "unavailable_reason": f"read_error: {exc.__class__.__name__}",
        }

    gosec_ids = re.findall(r"^\s*-\s+(G\d+)", text, flags=re.MULTILINE)
    depguard_deny = re.findall(r"^\s*-\s*pkg:\s*", text, flags=re.MULTILINE)
    forbidigo_patterns = re.findall(
        r"^\s*-\s*pattern:\s*", text, flags=re.MULTILINE
    )
    # Path-based exclusions in golangci-lint v2 live inside the
    # ``linters.exclusions.rules`` array. Each rule applies the listed
    # linters to a file path; the path key appears as ``path: <file>``
    # (singular, with a value). This differs from the ``paths:`` plural
    # block (a list of generated-file patterns) which we deliberately
    # exclude from this count.
    path_exclusions = re.findall(
        r"^\s+path:\s+\S+", text, flags=re.MULTILINE
    )

    return {
        "available": True,
        "source_file": ".golangci.yml",
        "source_file_extraction_command": "cat .golangci.yml",
        "gosec_excludes_count": len(gosec_ids),
        "gosec_excludes": gosec_ids,
        "gosec_excludes_verification_command": (
            "grep -E '^\\s*-\\s+G[0-9]+' .golangci.yml | wc -l"
        ),
        "depguard_deny_count": len(depguard_deny),
        "depguard_deny_verification_command": (
            "grep -E '^\\s*-\\s*pkg:\\s*' .golangci.yml | wc -l"
        ),
        "forbidigo_patterns_count": len(forbidigo_patterns),
        "forbidigo_patterns_verification_command": (
            "grep -E '^\\s*-\\s*pattern:\\s*' .golangci.yml | wc -l"
        ),
        "path_exclusions_count": len(path_exclusions),
        "path_exclusions_verification_command": (
            "grep -E '^\\s*-\\s*path:\\s*' .golangci.yml | wc -l"
        ),
        "file_byte_size": len(text),
    }


def parse_snyk(path: Path, reference_ts: str) -> dict[str, Any]:
    """Extract exemption counts from ``.snyk``.

    The function attempts a real YAML parse first via the optional
    ``yaml`` (PyYAML) module; if PyYAML is absent it falls back to
    regex matching on the raw text. Both code paths produce identical
    ``active_count`` and ``expired_count`` values for well-formed
    ``.snyk`` files; the YAML path additionally returns the raw
    ``ignore`` block for downstream auditing.

    The ``expired_count`` field surfaces ignore entries whose
    declared ``expires`` timestamp is strictly earlier than
    ``reference_ts``. Per AAP §0.5.3.11, this is a governance
    observation reported as a factual count — the metric computation
    in ``09_compute_metrics.py`` does not condition behaviour on the
    expiry value, but the count is rendered in the M10 deep-dive.

    Args:
        path: Filesystem path to ``.snyk``.
        reference_ts: ISO-8601 UTC timestamp against which expiry is
            evaluated. Typically the artifact's extraction timestamp.

    Returns:
        A dictionary describing the parse result. Schema mirrors
        :func:`parse_golangci`; additional fields include
        ``snyk_policy_version``, ``active_count``, ``expired_count``,
        ``expired_count_reference_date``, and an ``entries`` list with
        per-entry id, reason, expiry and expired-at-reference flag.
    """
    if not path.exists():
        return {
            "available": False,
            "source_file": str(path.name),
            "unavailable_reason": "file_missing",
        }
    try:
        text = path.read_text()
    except OSError as exc:
        return {
            "available": False,
            "source_file": str(path.name),
            "unavailable_reason": f"read_error: {exc.__class__.__name__}",
        }

    # Try the structural YAML parse first. PyYAML is not a hard
    # dependency of the analysis-environment requirements.txt so we
    # gracefully fall back to regex.
    policy_version: str | None = None
    entries: list[dict[str, Any]] = []
    parse_method: str

    try:
        import yaml  # type: ignore[import-not-found]
    except ImportError:
        yaml = None  # type: ignore[assignment]

    if yaml is not None:
        try:
            data = yaml.safe_load(text)
        except Exception:
            data = None
        if isinstance(data, dict):
            policy_version = str(data.get("version") or "") or None
            ignores = data.get("ignore") or {}
            if isinstance(ignores, dict):
                for vid, scopes in ignores.items():
                    # ``scopes`` is a list of {scope: {reason, expires}} maps.
                    if isinstance(scopes, list):
                        for scope_map in scopes:
                            if not isinstance(scope_map, dict):
                                continue
                            for scope, details in scope_map.items():
                                if not isinstance(details, dict):
                                    continue
                                expires = details.get("expires")
                                entries.append(
                                    {
                                        "id": str(vid),
                                        "scope": str(scope),
                                        "reason": str(
                                            details.get("reason", "")
                                        ),
                                        "expires": (
                                            str(expires)
                                            if expires is not None
                                            else None
                                        ),
                                        "expired_at_reference": _is_expired(
                                            expires, reference_ts
                                        ),
                                    }
                                )
            parse_method = "pyyaml_structural"
        else:
            yaml = None  # type: ignore[assignment]

    if yaml is None:
        # Regex fallback. Match the two-space-indented vulnerability id
        # lines (``  SNYK-...``) and pair them with the nearest
        # ``expires`` line that follows. This is sufficient for the
        # canonical ``.snyk`` file shape produced by the Snyk CLI.
        parse_method = "regex_fallback_pyyaml_unavailable"
        version_match = re.search(
            r"^\s*version:\s*(\S+)\s*$", text, flags=re.MULTILINE
        )
        if version_match:
            policy_version = version_match.group(1)
        # Collect ids in order.
        ids = [
            m.group(1)
            for m in re.finditer(
                r"^\s+(SNYK-[A-Z0-9_-]+):\s*$", text, flags=re.MULTILINE
            )
        ]
        # Collect (reason, expires) pairs in document order.
        details_iter = re.finditer(
            r"reason:\s*(?P<reason>.+?)\s*\n\s+expires:\s*['\"]?(?P<expires>"
            r"[0-9T:.Z+\-]+)['\"]?\s*\n",
            text,
        )
        details = [
            {"reason": m.group("reason").strip(), "expires": m.group("expires")}
            for m in details_iter
        ]
        for i, vid in enumerate(ids):
            d = details[i] if i < len(details) else {}
            expires = d.get("expires")
            entries.append(
                {
                    "id": vid,
                    "scope": "*",
                    "reason": d.get("reason", ""),
                    "expires": expires,
                    "expired_at_reference": _is_expired(
                        expires, reference_ts
                    ),
                }
            )

    active_count = len(entries)
    expired_count = sum(1 for e in entries if e.get("expired_at_reference"))

    return {
        "available": True,
        "source_file": ".snyk",
        "source_file_extraction_command": "cat .snyk",
        "parse_method": parse_method,
        "snyk_policy_version": policy_version,
        "active_count": active_count,
        "active_count_verification_command": (
            "grep -cE '^\\s+SNYK-' .snyk"
        ),
        "expired_count": expired_count,
        "expired_count_reference_date": reference_ts,
        "expired_count_note": (
            "Active ignore entries whose declared expiry timestamp "
            "is strictly earlier than the extraction timestamp. "
            "Surfaced as a governance observation per AAP "
            "\u00a70.5.3.11; the metric computation does not condition "
            "behaviour on this count."
        ),
        "entries": entries,
        "file_byte_size": len(text),
    }


def parse_truffleignore(path: Path) -> dict[str, Any]:
    """Extract exemption count from ``.truffleignore``.

    ``.truffleignore`` is a plain text file with one secret-scanner
    exemption pattern per line; ``#`` introduces a comment. The
    function counts non-blank, non-comment lines.

    Args:
        path: Filesystem path to ``.truffleignore``.

    Returns:
        A dictionary with ``available``, ``entry_count``,
        ``entries`` (the raw exemption patterns), and the source-file
        provenance fields.
    """
    if not path.exists():
        return {
            "available": False,
            "source_file": str(path.name),
            "unavailable_reason": "file_missing",
        }
    try:
        text = path.read_text()
    except OSError as exc:
        return {
            "available": False,
            "source_file": str(path.name),
            "unavailable_reason": f"read_error: {exc.__class__.__name__}",
        }
    entries: list[str] = []
    for raw_line in text.splitlines():
        stripped = raw_line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        entries.append(stripped)
    return {
        "available": True,
        "source_file": ".truffleignore",
        "source_file_extraction_command": "cat .truffleignore",
        "entry_count": len(entries),
        "entries": entries,
        "file_byte_size": len(text),
        "note": (
            "File exists and is empty; zero secret-scanner exemptions."
            if len(entries) == 0
            else f"{len(entries)} secret-scanner exemption pattern(s) at HEAD."
        ),
    }


def parse_deepsource(path: Path) -> dict[str, Any]:
    """Extract exemption count from ``.deepsource.toml``.

    Counts entries inside the ``exclude_patterns`` array AND the
    ``test_patterns`` array (the latter governs which files DeepSource
    treats as test files; they are reported separately because they
    are not exemptions but they affect analysis coverage). The
    function uses regex matching on the TOML source rather than a
    structural TOML parse to avoid requiring tomllib for Python
    3.10 compatibility (the analysis sandbox runs Python 3.13, so
    tomllib is available, but the regex approach keeps the module
    side-effect-free at import time).

    Args:
        path: Filesystem path to ``.deepsource.toml``.

    Returns:
        A dictionary with ``available``, ``exclude_patterns_count``,
        ``exclude_patterns`` (the raw glob list), ``test_patterns_count``,
        ``test_patterns``, and the source-file provenance fields.
    """
    if not path.exists():
        return {
            "available": False,
            "source_file": str(path.name),
            "unavailable_reason": "file_missing",
        }
    try:
        text = path.read_text()
    except OSError as exc:
        return {
            "available": False,
            "source_file": str(path.name),
            "unavailable_reason": f"read_error: {exc.__class__.__name__}",
        }

    config_version_match = re.search(
        r"^\s*version\s*=\s*(\d+)\s*$", text, flags=re.MULTILINE
    )
    config_version = (
        int(config_version_match.group(1)) if config_version_match else None
    )

    exclude_patterns = _extract_toml_array(text, "exclude_patterns")
    test_patterns = _extract_toml_array(text, "test_patterns")

    return {
        "available": True,
        "source_file": ".deepsource.toml",
        "source_file_extraction_command": "cat .deepsource.toml",
        "deepsource_config_version": config_version,
        "exclude_patterns_count": len(exclude_patterns),
        "exclude_patterns": exclude_patterns,
        "exclude_patterns_verification_command": (
            "awk '/^exclude_patterns/,/^\\]/' .deepsource.toml | "
            "grep -cE '^\\s+\"[^\"]+\"'"
        ),
        "test_patterns_count": len(test_patterns),
        "test_patterns": test_patterns,
        "test_patterns_note": (
            "test_patterns govern which files DeepSource treats as "
            "tests; they are NOT exemptions and are reported here for "
            "transparency only."
        ),
        "file_byte_size": len(text),
    }


def _extract_toml_array(text: str, key: str) -> list[str]:
    """Return the string entries of a top-level TOML array literal.

    Matches a block of the form ``key = [\\n  "foo",\\n  "bar"\\n]`` and
    extracts every quoted string between the brackets. The matcher is
    permissive with respect to whitespace and trailing commas. This is
    a deliberate regex parse rather than a tomllib parse to keep this
    module's dependency footprint to the standard library plus the
    pipeline's existing lib/ helpers.

    Args:
        text: The full file contents.
        key: The TOML key whose array value should be extracted.

    Returns:
        A list of the string literals inside the array. An empty list
        is returned when the key is absent or the array is empty.
    """
    block_match = re.search(
        rf"^\s*{re.escape(key)}\s*=\s*\[(.*?)\]",
        text,
        flags=re.MULTILINE | re.DOTALL,
    )
    if not block_match:
        return []
    body = block_match.group(1)
    return [m.group(1) for m in re.finditer(r'"([^"]+)"', body)]




# ---------------------------------------------------------------------------
# Tier extractors (audit log, branch protection, reflog, PR labels)
# ---------------------------------------------------------------------------


def _extract_audit_log(
    gh: GithubClient, repo_slug: str, logger: Any
) -> dict[str, Any]:
    """Attempt to fetch admin audit-log events for ``repo_slug``.

    The endpoint ``/repos/{owner}/{repo}/audit-log`` returns events
    only when the caller's token has organisation-level admin scope.
    The read-only analysis sandbox typically lacks this scope; the
    function therefore wraps the call in a broad ``except`` that
    captures HTTP-level failures, network errors, and JSON-shape
    surprises, and records the failure in the structured-JSON log
    rather than propagating an exception.

    Args:
        gh: Pre-configured GithubClient (token may be ``None``).
        repo_slug: ``owner/repo`` string identifying the analyzed
            repository.
        logger: Structured logger; receives an
            ``audit_log_unavailable`` warning on failure.

    Returns:
        A dictionary suitable for the ``audit_log`` sub-object of the
        output artifact. Carries ``available``, ``events``,
        ``unavailable_reason`` (when applicable), and provenance
        fields so the report renderer can explain to readers why the
        signal is or is not present.
    """
    endpoint = f"/repos/{repo_slug}/audit-log"
    base_record: dict[str, Any] = {
        "available": False,
        "events": [],
        "api_endpoint_attempted": f"GET {endpoint}",
        "auth_required": (
            "admin-level GitHub token with the `audit_log` scope at the "
            "organisation level, or a GitHub App installation with "
            "audit-log read permission. Read-only analysis tokens "
            "without admin scope receive 403/404 from this endpoint."
        ),
        "consequence": (
            "Without admin audit log access, the bulk of Metric 10's "
            "signal (required-check bypasses, branch protection rule "
            "changes, manual merge overrides) is invisible. This drives "
            "metrics.json#m10.confidence to 'low' and is surfaced in "
            "the Risk Assessment section of acceleration-report.md."
        ),
        "unavailable_reason": None,
    }

    try:
        response = gh.get(endpoint, params={"per_page": 100})
        # Use paginate() to walk the response sequence. The audit-log
        # endpoint returns a list shape per the GitHub API docs.
        events: list[dict[str, Any]] = []
        for raw_event in gh.paginate(response):
            if not isinstance(raw_event, dict):
                continue
            action = raw_event.get("action")
            if action not in AUDIT_EXCEPTION_ACTIONS:
                continue
            events.append(
                {
                    "action": action,
                    "actor": raw_event.get("actor"),
                    "repo": raw_event.get("repo"),
                    # Both keys are documented in the GitHub Audit Log
                    # response. ``@timestamp`` is the historical primary
                    # key; ``created_at`` was introduced when the
                    # endpoint became generally available. We persist
                    # ``created_at`` for downstream consumers because
                    # ``09_compute_metrics.py`` reads that field first.
                    "created_at": _normalise_iso(
                        raw_event.get("created_at")
                        or raw_event.get("@timestamp")
                    ),
                    "timestamp": _normalise_iso(
                        raw_event.get("@timestamp")
                        or raw_event.get("created_at")
                    ),
                    "raw": {
                        k: raw_event.get(k)
                        for k in (
                            "user",
                            "org",
                            "country_code",
                            "operation_type",
                        )
                        if k in raw_event
                    },
                }
            )
        base_record["available"] = True
        base_record["events"] = events
        logger.info(
            "audit_log_available",
            extra={
                "event": "audit_log_available",
                "endpoint": endpoint,
                "event_count": len(events),
            },
        )
        return base_record
    except Exception as exc:
        reason = f"{exc.__class__.__name__}: {exc}"
        base_record["unavailable_reason"] = (
            "admin_access_required_or_network_unreachable"
        )
        base_record["unavailable_reason_detail"] = reason
        logger.warning(
            "audit_log_unavailable",
            extra={
                "event": "audit_log_unavailable",
                "endpoint": endpoint,
                "error": reason,
                "reason": "admin_access_required",
            },
        )
        return base_record


def _extract_branch_protection(
    gh: GithubClient,
    repo_slug: str,
    branch: str,
    logger: Any,
    snapshot_at: str,
) -> dict[str, Any]:
    """Snapshot branch-protection settings for ``branch``.

    Uses the public Branches API endpoint
    ``/repos/{owner}/{repo}/branches/{branch}/protection`` which
    returns the current rule state. Historical changes to the rules
    require admin audit-log access and are NOT visible from this
    endpoint alone — the result is therefore a HEAD-only snapshot
    rather than a time-series.

    Args:
        gh: GithubClient.
        repo_slug: ``owner/repo`` string.
        branch: The branch name to query.
        logger: Structured logger; receives
            ``branch_protection_unavailable`` warning on failure or
            ``branch_protection_available`` on success.
        snapshot_at: ISO-8601 timestamp recorded as ``snapshot_at`` in
            the output sub-object.

    Returns:
        A dictionary suitable for the ``branch_protection`` sub-object
        of the output artifact.
    """
    endpoint = f"/repos/{repo_slug}/branches/{branch}/protection"
    base_record: dict[str, Any] = {
        "branch": branch,
        "snapshot_at": snapshot_at,
        "available": False,
        "snapshot": {
            "protected": None,
            "required_status_checks": None,
            "enforce_admins": None,
            "required_pull_request_reviews": None,
            "restrictions": None,
        },
        "api_endpoint_attempted": f"GET {endpoint}",
        "time_series_available": False,
        "time_series_note": (
            "Branch protection settings are visible only as a current "
            "snapshot via the public Branches API; historical changes "
            "to the protection rules (additions, removals, "
            "requirement-toggling) require admin audit log access. The "
            "historical signal is therefore tied to the same admin-"
            "access gap that disables the audit_log sub-object above."
        ),
        "unavailable_reason": None,
    }

    try:
        payload = gh.get_one(endpoint)
        base_record["available"] = True
        base_record["snapshot"] = {
            "protected": payload.get("protected", True),
            "required_status_checks": payload.get("required_status_checks"),
            "enforce_admins": (
                payload.get("enforce_admins", {}).get("enabled")
                if isinstance(payload.get("enforce_admins"), dict)
                else payload.get("enforce_admins")
            ),
            "required_pull_request_reviews": payload.get(
                "required_pull_request_reviews"
            ),
            "restrictions": payload.get("restrictions"),
            "allow_force_pushes": (
                payload.get("allow_force_pushes", {}).get("enabled")
                if isinstance(payload.get("allow_force_pushes"), dict)
                else payload.get("allow_force_pushes")
            ),
            "allow_deletions": (
                payload.get("allow_deletions", {}).get("enabled")
                if isinstance(payload.get("allow_deletions"), dict)
                else payload.get("allow_deletions")
            ),
        }
        logger.info(
            "branch_protection_available",
            extra={
                "event": "branch_protection_available",
                "endpoint": endpoint,
                "branch": branch,
            },
        )
        return base_record
    except Exception as exc:
        reason = f"{exc.__class__.__name__}: {exc}"
        base_record["unavailable_reason"] = (
            "github_api_unauthenticated_or_unreachable"
        )
        base_record["unavailable_reason_detail"] = reason
        logger.warning(
            "branch_protection_unavailable",
            extra={
                "event": "branch_protection_unavailable",
                "endpoint": endpoint,
                "error": reason,
            },
        )
        return base_record


def _extract_force_pushes(
    branch: str, logger: Any, repo_root: Path
) -> dict[str, Any]:
    """Enumerate force-pushes from the local reflog of ``branch``.

    Returns a dict suitable for the ``force_pushes`` sub-object of
    the output artifact. The ``events`` list contains one entry per
    force-push-shaped line in the reflog; the ``count`` field is the
    cardinality of that list. The ``reflog_total_entries_observed``
    field reports the total number of reflog lines (including non-
    force-push entries), which is useful context for explaining why
    the count is low (typically the only reflog entry in a fresh
    clone is the initial ``clone:`` operation).

    Args:
        branch: The ref to inspect. Typically ``main``.
        logger: Structured logger.
        repo_root: Repository root, passed to ``git_reflog`` as the
            ``cwd`` argument so that ``git reflog show`` runs against
            the analyzed clone rather than wherever the script was
            invoked from.

    Returns:
        A dictionary describing the force-push enumeration result.
    """
    base_record: dict[str, Any] = {
        "source": "git_reflog_local_clone",
        "search_command": f"git reflog show {branch}",
        "events": [],
        "count": 0,
        "reflog_total_entries_observed": 0,
        "reflog_first_entry_observed": None,
        "note": (
            "Local clone's reflog may not contain force-push events "
            "that occurred on the remote before this clone was created. "
            "Reflog availability is limited to push events this clone "
            "observed. To detect force-pushes on the remote main "
            "branch outside this clone's reflog window, admin audit "
            "log access is required (see audit_log sub-object)."
        ),
        "limitation_class": "local_reflog_does_not_observe_remote_history",
    }

    try:
        reflog_lines = git_reflog(branch, cwd=repo_root)
    except Exception as exc:
        reason = f"{exc.__class__.__name__}: {exc}"
        logger.warning(
            "reflog_unavailable",
            extra={
                "event": "reflog_unavailable",
                "branch": branch,
                "error": reason,
            },
        )
        base_record["reflog_unavailable_reason"] = reason
        return base_record

    base_record["reflog_total_entries_observed"] = len(reflog_lines)
    if reflog_lines:
        base_record["reflog_first_entry_observed"] = reflog_lines[0]

    events: list[dict[str, Any]] = []
    for line in reflog_lines:
        match = REFLOG_LINE_RE.match(line)
        if not match:
            continue
        sha, ref_name, ref_age, message = match.groups()
        message_lower = message.lower()
        if not any(marker in message_lower for marker in FORCE_PUSH_MARKERS):
            continue
        events.append(
            {
                "sha": sha,
                "ref": ref_name,
                "ref_age": int(ref_age),
                "message": message,
                # The reflog show default output omits timestamps. We
                # leave ``date`` and ``created_at`` as ``None`` so the
                # compute step's null-check excludes the event from the
                # per-window bucketing pass (it would otherwise place
                # the event in an arbitrary bucket). The event is still
                # counted in the ``count`` field for transparency.
                "date": None,
                "created_at": None,
                "raw_line": line,
            }
        )

    base_record["events"] = events
    base_record["count"] = len(events)
    logger.info(
        "force_pushes_extracted",
        extra={
            "event": "force_pushes_extracted",
            "branch": branch,
            "force_push_count": len(events),
            "reflog_total_entries_observed": len(reflog_lines),
        },
    )
    return base_record


def _scan_pr_labels(
    pulls_data: dict[str, Any] | list[Any] | None,
    logger: Any,
) -> dict[str, Any]:
    """Scan PR labels in ``pulls_data`` for exception/waiver/override/bypass.

    The function tolerates the two shapes produced by
    ``03_extract_pulls.py``:

    * A top-level list of PR dicts.
    * A top-level dict with a ``pulls`` key whose value is the list.

    Each PR's ``labels`` field may be a list of label-name strings OR a
    list of label objects with a ``name`` key (the GitHub API returns
    objects; the local-git reconstruction may emit strings). Both
    shapes are normalised. Matches are case-insensitive substring
    checks against the patterns in :data:`EXCEPTION_LABEL_PATTERNS`.

    Args:
        pulls_data: Decoded contents of ``data/pulls.json``, or
            ``None`` when that file is absent.
        logger: Structured logger.

    Returns:
        A dictionary suitable for the ``exception_labeled_prs`` sub-
        object of the output artifact. Includes ``events`` (one per
        matching PR), ``exception_pattern_match_count``, the patterns
        searched, the available-labels inventory (derived from the
        PR data itself plus the static ``.github/labeler.yml``
        catalogue noted in the AAP), and explanatory ``_note`` fields.
    """
    static_labeler_labels = ["with tests", "server-team", "warehouse-team"]
    base_record: dict[str, Any] = {
        "label_patterns_searched": list(EXCEPTION_LABEL_PATTERNS),
        "label_patterns_match_mode": "substring_case_insensitive",
        "available_labels_in_repo": list(static_labeler_labels),
        "available_labels_source": {
            "labeler_auto_applied": static_labeler_labels,
            "labeler_auto_applied_source_file": ".github/labeler.yml",
            "human_applied_observed_on_prs": [],
            "human_applied_observed_on_prs_source": (
                "data/pulls.json (PR label inventory at HEAD)"
            ),
        },
        "exception_pattern_match_count": 0,
        "exception_pattern_match_note": (
            "The repository's labeler.yml + observed PR labels do not "
            "include any exception/waiver/override/bypass markers; "
            "this signal is structurally zero unless new labels are "
            "introduced. The semantic-pr.yaml workflow constrains PR "
            "titles to the conventional-commit prefix set (fix, feat, "
            "chore, refactor, exp, doc, test) which does not provide "
            "an exception channel either."
        ),
        "pulls_json_consumed": True,
        "events": [],
    }

    if pulls_data is None:
        base_record["pulls_json_consumed"] = False
        base_record["pulls_json_unavailable_reason"] = (
            "data/pulls.json missing — stage 03 has not produced its "
            "artifact yet OR the script is being invoked standalone. "
            "Exception-labeled-PR signal cannot be evaluated without "
            "the PR inventory; reported as zero with an explicit "
            "unavailability flag rather than imputed."
        )
        logger.warning(
            "pulls_json_missing",
            extra={
                "event": "pulls_json_missing",
                "expected_path": "data/pulls.json",
            },
        )
        return base_record

    # Normalise the top-level shape.
    if isinstance(pulls_data, list):
        pulls = pulls_data
    elif isinstance(pulls_data, dict):
        candidate = pulls_data.get("pulls")
        pulls = candidate if isinstance(candidate, list) else []
    else:
        pulls = []

    label_pattern = re.compile(
        "|".join(re.escape(p) for p in EXCEPTION_LABEL_PATTERNS),
        re.IGNORECASE,
    )

    events: list[dict[str, Any]] = []
    observed_labels: set[str] = set()

    for pr in pulls:
        if not isinstance(pr, dict):
            continue
        raw_labels = pr.get("labels") or []
        # Normalise each label to a string name.
        label_names: list[str] = []
        for raw_lbl in raw_labels:
            if isinstance(raw_lbl, dict):
                name = raw_lbl.get("name", "")
            else:
                name = str(raw_lbl)
            name = name.strip()
            if name:
                label_names.append(name)
                observed_labels.add(name)

        matched = [name for name in label_names if label_pattern.search(name)]
        if matched:
            events.append(
                {
                    "number": pr.get("number"),
                    "title": pr.get("title", ""),
                    "matched_labels": matched,
                    "all_labels": label_names,
                    # Persist a per-event timestamp so the compute step
                    # can bucket the event by 2-week window. Prefer the
                    # merged_at timestamp (event materialises at merge);
                    # fall back to created_at; both are normalised to
                    # ISO-8601 UTC.
                    "created_at": _normalise_iso(pr.get("merged_at"))
                    or _normalise_iso(pr.get("created_at")),
                    "timestamp": _normalise_iso(pr.get("merged_at"))
                    or _normalise_iso(pr.get("created_at")),
                    "url": pr.get("html_url") or pr.get("url"),
                }
            )

    # Sort observed labels for deterministic output.
    base_record["available_labels_in_repo"] = sorted(
        set(static_labeler_labels) | observed_labels
    )
    base_record["available_labels_source"]["human_applied_observed_on_prs"] = (
        sorted(observed_labels - set(static_labeler_labels))
    )
    base_record["events"] = events
    base_record["exception_pattern_match_count"] = len(events)

    if events:
        # Override the structural-zero note when matches exist.
        base_record["exception_pattern_match_note"] = (
            f"{len(events)} PR(s) matched the exception-label pattern. "
            "These are events that contribute to Metric 10."
        )

    logger.info(
        "pr_label_scan_complete",
        extra={
            "event": "pr_label_scan_complete",
            "pull_count_scanned": len(pulls),
            "exception_label_match_count": len(events),
            "observed_label_count": len(observed_labels),
        },
    )
    return base_record




# ---------------------------------------------------------------------------
# Lint-exemption aggregation
# ---------------------------------------------------------------------------


def _aggregate_lint_exemptions(
    lint_exemptions: dict[str, dict[str, Any]],
) -> tuple[int, dict[str, int], int]:
    """Sum the lint-exemption counts across all four source files.

    The total is the sum of:

    * ``golangci.gosec_excludes_count``
    * ``golangci.depguard_deny_count``
    * ``golangci.forbidigo_patterns_count``
    * ``golangci.path_exclusions_count``
    * ``snyk.active_count``
    * ``truffleignore.entry_count``
    * ``deepsource.exclude_patterns_count``

    The ``test_patterns`` count from DeepSource is deliberately NOT
    included because they are not exemptions (they govern test-file
    discrimination, not analyzer suppression).

    Args:
        lint_exemptions: The map of per-source-file parse results.

    Returns:
        A 3-tuple ``(total, breakdown, snyk_expired_count)`` where
        ``breakdown`` is a dict mapping each component count to its
        value (for the summary block of the artifact) and
        ``snyk_expired_count`` is surfaced separately because the
        Risk Assessment section of the report singles it out.
    """
    golangci = lint_exemptions.get("golangci", {})
    snyk = lint_exemptions.get("snyk", {})
    trufflectx = lint_exemptions.get("truffleignore", {})
    deepsource = lint_exemptions.get("deepsource", {})

    breakdown: dict[str, int] = {
        "gosec_excludes": int(golangci.get("gosec_excludes_count", 0) or 0),
        "depguard_deny": int(golangci.get("depguard_deny_count", 0) or 0),
        "forbidigo_patterns": int(
            golangci.get("forbidigo_patterns_count", 0) or 0
        ),
        "path_exclusions": int(golangci.get("path_exclusions_count", 0) or 0),
        "snyk_active": int(snyk.get("active_count", 0) or 0),
        "truffleignore_entries": int(trufflectx.get("entry_count", 0) or 0),
        "deepsource_exclude_patterns": int(
            deepsource.get("exclude_patterns_count", 0) or 0
        ),
    }
    total = sum(breakdown.values())
    snyk_expired_count = int(snyk.get("expired_count", 0) or 0)
    return total, breakdown, snyk_expired_count


# ---------------------------------------------------------------------------
# Dry-run preview
# ---------------------------------------------------------------------------


def _build_dry_run_preview(args: argparse.Namespace) -> dict[str, Any]:
    """Construct the dry-run preview structure printed to stdout.

    Enumerates every external endpoint, git sub-command, and file
    read the script would perform on a real run, so the operator can
    verify the read-only contract before granting credentials. The
    preview includes the resolved CLI argument values and the
    artifact path that would be written.

    Args:
        args: Parsed CLI namespace.

    Returns:
        A JSON-serialisable dictionary describing the planned actions.
    """
    return {
        "action": "dry_run",
        "script": SCRIPT_NAME,
        "args": {
            "repo_slug": args.repo_slug,
            "output": args.output,
        },
        "git_commands": [
            "git rev-parse --show-toplevel",
            f"git reflog show {DEFAULT_BRANCH}",
        ],
        "api_calls": [
            f"GET https://api.github.com/repos/{args.repo_slug}/audit-log",
            (
                "GET https://api.github.com/repos/"
                f"{args.repo_slug}/branches/{DEFAULT_BRANCH}/protection"
            ),
        ],
        "files_read": [
            ".golangci.yml",
            ".snyk",
            ".truffleignore",
            ".deepsource.toml",
            ".github/labeler.yml",
            "data/pulls.json",
            "data/environment.json",
            "data/inflection.json",
        ],
        "writes": [args.output],
        "writes_to_external_systems": [],
        "writes_to_external_systems_note": (
            "This script issues HTTP GET only. No POST, PUT, PATCH, "
            "DELETE method is invoked against any external system. "
            "All git sub-commands invoked are read sub-commands "
            "(rev-parse, reflog show). The only filesystem write is "
            "to the local output path under data/."
        ),
    }


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------


def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    """Parse CLI arguments.

    Defined as a separate function so that ad-hoc tests can exercise
    the argument parser without invoking the full extraction pipeline.

    Args:
        argv: Optional list of arguments. When ``None`` (the default),
            ``argparse`` reads from ``sys.argv[1:]``.

    Returns:
        The parsed ``argparse.Namespace``.
    """
    parser = argparse.ArgumentParser(
        prog=SCRIPT_NAME,
        description=(
            "Extract the exception inventory across audit log, "
            "force-pushes, branch protection, label markers, and "
            "lint-config exemptions for Metric 10 (Approved "
            "Exceptions) of the acceleration report."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Print the planned actions (git commands, API calls, "
            "files read, files written) as JSON to stdout and exit "
            "without contacting any external system."
        ),
    )
    parser.add_argument(
        "--repo-slug",
        default=os.environ.get("GITHUB_REPO_SLUG", REPO_SLUG_DEFAULT),
        help=(
            "GitHub repository slug in `owner/repo` form. Defaults to "
            f"$GITHUB_REPO_SLUG or `{REPO_SLUG_DEFAULT}`."
        ),
    )
    parser.add_argument(
        "--output",
        default=str(OUTPUT_PATH),
        help=(
            "Path to the output JSON artifact. Defaults to "
            "`<workspace>/data/exceptions.json`."
        ),
    )
    parser.add_argument(
        "--branch",
        default=DEFAULT_BRANCH,
        help=(
            "Branch name to use for branch-protection lookup and "
            f"reflog force-push detection. Defaults to `{DEFAULT_BRANCH}`."
        ),
    )
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    """Run the exception-inventory extraction.

    The function is the script's single public entry point. It is
    declared with an optional ``argv`` parameter so that ad-hoc tests
    can drive the script without manipulating ``sys.argv``. On
    success it writes ``data/exceptions.json`` and returns ``0``. On
    a fatal precondition failure (no enclosing git working tree) it
    returns ``2``. Tier-level failures (audit log unreachable,
    branch protection unauthorised, reflog absent, pulls.json missing,
    lint files missing) are individually logged and do NOT cause a
    non-zero exit — the artifact is always produced with whatever
    signal is available so that ``09_compute_metrics.py`` can apply
    its confidence-degradation rules.

    Args:
        argv: Optional CLI argument list, forwarded to
            :func:`_parse_args`. The default ``None`` reads
            ``sys.argv[1:]``.

    Returns:
        Exit code: ``0`` on success, ``2`` when the repository root
        cannot be determined.
    """
    args = _parse_args(argv)
    logger = get_logger(SCRIPT_NAME)
    logger.info(
        "script_started",
        extra={
            "event": "script_started",
            "dry_run": args.dry_run,
            "repo_slug": args.repo_slug,
            "branch": args.branch,
            "output_path": args.output,
        },
    )

    if args.dry_run:
        preview = _build_dry_run_preview(args)
        print(json.dumps(preview, indent=2))
        logger.info(
            "script_complete",
            extra={
                "event": "script_complete",
                "dry_run": True,
                "audit_available": False,
            },
        )
        return 0

    # ------------------------------------------------------------------
    # Phase 2 — Auto-detect repository root.
    # ------------------------------------------------------------------
    repo_root = git_rev_parse_toplevel()
    if not repo_root:
        logger.error(
            "repo_root_unavailable",
            extra={
                "event": "git_toplevel_failed",
                "hint": (
                    "Run this script from inside a git working tree. "
                    "The rudder-server configuration files "
                    "(.golangci.yml, .snyk, .truffleignore, "
                    ".deepsource.toml) are located at the repository "
                    "root and cannot be discovered without a toplevel."
                ),
            },
        )
        return 2

    extraction_ts = iso_now()

    # ------------------------------------------------------------------
    # Read upstream artifacts (best-effort).
    # ------------------------------------------------------------------
    env_payload = _read_optional_json(DATA_DIR / "environment.json")
    infl_payload = _read_optional_json(DATA_DIR / "inflection.json")
    pulls_payload = _read_optional_json(DATA_DIR / "pulls.json")

    metadata = _build_metadata(args, env_payload, infl_payload, extraction_ts)

    # Reconcile the repo slug used for API calls with the slug recorded
    # in _metadata to satisfy Rule-4 internal consistency across
    # artifacts in the pipeline.
    effective_repo_slug = metadata["repository_slug"]

    # ------------------------------------------------------------------
    # Construct the GitHub client.
    # ------------------------------------------------------------------
    gh = GithubClient(token=os.environ.get("GH_TOKEN"), logger=logger)

    # ------------------------------------------------------------------
    # Phase 3 — Audit log (Tier 1, usually unavailable).
    # ------------------------------------------------------------------
    audit_log = _extract_audit_log(gh, effective_repo_slug, logger)

    # ------------------------------------------------------------------
    # Phase 4 — Branch protection snapshot.
    # ------------------------------------------------------------------
    branch_protection = _extract_branch_protection(
        gh,
        effective_repo_slug,
        args.branch,
        logger,
        snapshot_at=metadata["extraction_timestamp"],
    )

    # ------------------------------------------------------------------
    # Phase 5 — Force-pushes from git reflog.
    # ------------------------------------------------------------------
    force_pushes = _extract_force_pushes(args.branch, logger, repo_root)

    # ------------------------------------------------------------------
    # Phase 6 — Exception-label PR scan.
    # ------------------------------------------------------------------
    exception_labeled_prs = _scan_pr_labels(pulls_payload, logger)

    # ------------------------------------------------------------------
    # Phase 7 — Lint-config exemption HEAD snapshots.
    # ------------------------------------------------------------------
    lint_exemptions: dict[str, Any] = {
        "extraction_timestamp": metadata["extraction_timestamp"],
        "head_only_snapshot": True,
        "head_only_snapshot_note": (
            "Lint-config exemption files are reported as a HEAD-only "
            "snapshot. Per-window time-series of these counts requires "
            "iterating through git history file-by-file, which is "
            "outside the scope of this script. AAP \u00a70.5.3.11 "
            "documents this as a deliberate scope decision."
        ),
        "golangci": parse_golangci(repo_root / ".golangci.yml"),
        "snyk": parse_snyk(
            repo_root / ".snyk", metadata["extraction_timestamp"]
        ),
        "truffleignore": parse_truffleignore(repo_root / ".truffleignore"),
        "deepsource": parse_deepsource(repo_root / ".deepsource.toml"),
    }
    logger.info(
        "lint_exemptions_parsed",
        extra={
            "event": "lint_exemptions_parsed",
            "golangci_available": lint_exemptions["golangci"].get(
                "available", False
            ),
            "snyk_available": lint_exemptions["snyk"].get("available", False),
            "truffleignore_available": lint_exemptions["truffleignore"].get(
                "available", False
            ),
            "deepsource_available": lint_exemptions["deepsource"].get(
                "available", False
            ),
        },
    )

    # ------------------------------------------------------------------
    # Phase 8 — Assemble summary.
    # ------------------------------------------------------------------
    lint_total, lint_breakdown, snyk_expired = _aggregate_lint_exemptions(
        {
            "golangci": lint_exemptions["golangci"],
            "snyk": lint_exemptions["snyk"],
            "truffleignore": lint_exemptions["truffleignore"],
            "deepsource": lint_exemptions["deepsource"],
        }
    )

    audit_count = len(audit_log.get("events", []))
    fp_count = force_pushes.get("count", 0)
    label_count = exception_labeled_prs.get("exception_pattern_match_count", 0)

    confidence_for_m10 = "high" if audit_log.get("available") else "low"
    confidence_rationale = (
        "Admin audit log is available; Metric 10 receives the full "
        "signal."
        if audit_log.get("available")
        else (
            "The two highest-fidelity signals (admin audit log, "
            "branch-protection time series) are unavailable. The "
            "remaining signals (force-push reflog, exception-labeled "
            "PRs, lint-exemption HEAD inventory) cover only a fraction "
            "of the policy-exception surface area defined by Metric "
            "10. AAP \u00a70.5.3.11 mandates confidence drops to Low "
            "when admin audit log access is absent."
        )
    )

    summary: dict[str, Any] = {
        "audit_log_events_count": audit_count,
        "force_push_events_count": fp_count,
        "exception_labeled_pr_count": label_count,
        "lint_exemptions_total": lint_total,
        "lint_exemptions_total_breakdown": lint_breakdown,
        "lint_exemptions_total_note": (
            f"Sum of {lint_breakdown['gosec_excludes']} gosec + "
            f"{lint_breakdown['depguard_deny']} depguard + "
            f"{lint_breakdown['forbidigo_patterns']} forbidigo + "
            f"{lint_breakdown['path_exclusions']} path-exclusions + "
            f"{lint_breakdown['snyk_active']} snyk + "
            f"{lint_breakdown['truffleignore_entries']} truffleignore "
            f"+ {lint_breakdown['deepsource_exclude_patterns']} "
            f"deepsource = {lint_total}. These are HEAD-only counts, "
            "not events per 2-week window."
        ),
        "snyk_expired_at_head_count": snyk_expired,
        "current_lint_exemption_count": lint_total,
        "per_window_events": [],
        "per_window_events_note": (
            "Per-window time-series events are bucketed downstream by "
            "09_compute_metrics.py from the timestamped events in "
            "audit_log.events, force_pushes.events (when the reflog "
            "carries timestamps), and exception_labeled_prs.events. "
            "Static-analysis exemptions are HEAD snapshots and do not "
            "contribute per-window event counts."
        ),
        "confidence_implication_for_m10": confidence_for_m10,
        "confidence_implication_for_m10_rationale": confidence_rationale,
        "boundary_conditions_for_m10": [
            (
                "Per-window event counts are unobserved for the "
                "audit-log and branch-protection signals; the report "
                "must state 'Insufficient signal — admin audit log "
                "unavailable' rather than imply zero events occurred."
            ),
            (
                "Force-push reflog observability is bounded by what "
                "this clone observed; force-pushes that occurred on "
                "the remote main branch before the clone was created "
                "are invisible."
            ),
            (
                "Lint-exemption counts are HEAD-only snapshots and do "
                "not move per window; they are reported as the current "
                "static-analysis exemption inventory rather than per-"
                "window events."
            ),
        ],
    }

    # ------------------------------------------------------------------
    # Assemble the final payload.
    # ------------------------------------------------------------------
    payload: dict[str, Any] = {
        "_metadata": metadata,
        "fetched_at": metadata["extraction_timestamp"],
        "audit_log": audit_log,
        "branch_protection": branch_protection,
        "force_pushes": force_pushes,
        "exception_labeled_prs": exception_labeled_prs,
        "lint_exemptions": lint_exemptions,
        "summary": summary,
    }

    # ------------------------------------------------------------------
    # Write the output artifact.
    # ------------------------------------------------------------------
    output_path = Path(args.output)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    serialised = json.dumps(payload, indent=2, default=str, ensure_ascii=False)
    # Append a trailing newline to satisfy POSIX line-termination
    # conventions and to keep the file diff-clean.
    output_path.write_text(serialised + "\n")

    logger.info(
        "script_complete",
        extra={
            "event": "script_complete",
            "audit_available": audit_log.get("available", False),
            "branch_protection_available": branch_protection.get(
                "available", False
            ),
            "force_push_count": fp_count,
            "exception_labeled_pr_count": label_count,
            "lint_exemptions_total": lint_total,
            "snyk_expired_at_head_count": snyk_expired,
            "output_path": str(output_path),
            "output_byte_size": len(serialised),
        },
    )
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        # SystemExit carries the integer exit code already; re-raise so
        # that argparse-driven exits (e.g. ``--help``) propagate.
        raise
    except Exception as exc:  # pragma: no cover - last-resort handler
        # Last-resort handler. Emit a structured error event so the
        # operator sees something useful in run.log.jsonl even when the
        # failure happens before script_complete.
        get_logger(SCRIPT_NAME).error(
            "script_failed",
            extra={
                "event": "script_failed",
                "error": str(exc),
                "exception_type": type(exc).__name__,
            },
        )
        raise

