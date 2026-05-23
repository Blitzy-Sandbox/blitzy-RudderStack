#!/usr/bin/env python3
"""Release inventory extractor for the acceleration-report pipeline.

This script is stage 04 of the read-only extraction pipeline documented in
``blitzy/acceleration-report/``. It feeds Metric 9 (Releases) and Metric 8
(release attribution for reverts) by producing a single canonical artifact
``data/releases.json`` that enumerates the release inventory of
``Blitzy-Sandbox/blitzy-RudderStack`` using three-tier source precedence:

1. **Tier 1 — GitHub Releases API** (``GET /repos/{owner}/{repo}/releases``).
   The authoritative source when the GitHub API is reachable. Returns the
   primary release records together with the ``prerelease`` boolean
   already set by the upstream system.
2. **Tier 2 — Local annotated git tags** (``git for-each-ref refs/tags/``).
   The fallback source when the API is unreachable. Matches the semver
   pattern ``v?\\d+\\.\\d+\\.\\d+`` declared in
   :data:`SEMVER_TAG_RE`. This fork is known to publish ZERO annotated
   tags at present (verified by ``git tag -l | wc -l`` returning ``0``),
   so the tier produces no signal in this repository — but the helper is
   retained for future-proofing and for parity with the AAP's source
   precedence list.
3. **Tier 3 — CI deploy events** (``GET /repos/{owner}/{repo}/actions/runs``).
   The last-resort source when neither the Releases API nor local tags
   yield data. Filters workflow runs whose name or path contains one of
   :data:`DEPLOY_WORKFLOW_NAMES` (``dispatch-deploy-event-dev``,
   ``release-please``, ``sync-release``) and whose conclusion is
   ``"success"``. Each surviving run is synthesised into a release record
   tagged ``deploy-{run_id}``. This tier is invoked only when Tier 1 is
   reachable AND returned an empty release list AND Tier 2 produced no
   matches.

Read-only enforcement
---------------------

Every HTTP request is dispatched through :class:`lib.github.GithubClient`
whose private ``_request`` method statically rejects any verb other than
``GET``. Every git invocation goes through :func:`lib.git.git_for_each_ref`,
which runs through the read-only validator gate of ``lib/git.py`` that
rejects every mutating sub-command. The script never writes to the
working tree of the analyzed repository or any external system — only
to ``blitzy/acceleration-report/data/releases.json``.

Schema conformance
------------------

The output validates against ``scripts/lib/schemas/releases.schema.json``.
The schema requires a ``_metadata`` block carrying the canonical per-run
correlation fields (``extraction_timestamp``, ``run_id``,
``repository_slug``, ``default_branch``, ``analysis_period_start``,
``analysis_period_end``, ``inflection_date_utc``). These values are
read from ``data/environment.json`` and ``data/inflection.json`` when
those files exist; sensible defaults are computed otherwise so the
script remains useful for ad-hoc invocations that have not yet been
preceded by stages 00 and 01.

Prerelease segregation
----------------------

A release is classified as a prerelease and routed to the
``prereleases`` array when EITHER of the following holds:

* The upstream system (GitHub Releases API) sets ``prerelease == True``.
  This is the trusted signal for Tier 1.
* The tag name matches :data:`PRERELEASE_RE` (any of ``-alpha``,
  ``-beta``, ``-rc``, ``-dev`` segments). This is the fallback signal
  for Tier 2 (annotated tags) and Tier 3 (synthesised deploy-event
  records).

Per AAP §0.5.3.10, prereleases are reported separately from the
primary count and never enter the ``releases`` array.

Engineering-actor framing
-------------------------

This metric is repository-wide and does not require an engineering-actor
substitution. The script emits a single inventory; the per-phase
windowing and confidence assignment for Metric 9 are performed
downstream in ``09_compute_metrics.py`` based on
``_metadata.inflection_date_utc`` and ``releases[*].published_at``.

Observability
-------------

The script acquires a structured-JSON logger via
:func:`lib.observability.get_logger`, which propagates the per-run
correlation ID through the ``BLITZY_RUN_ID`` environment variable. Every
major branch (Tier 1 success/failure, Tier 2 scan result, Tier 3
fallback invocation, final tier selection, write completion) emits a
single ``event``-tagged log line so that ``data/run.log.jsonl`` carries
a complete audit trail of the run. The ``--dry-run`` flag exits 0 after
printing the planned API calls and git commands without executing any
network or git activity, satisfying the Rule-1 (Observability)
readiness preflight requirement.

Exit codes
----------

* ``0`` — Successful execution OR dry-run completion OR graceful
  degradation (all three tiers unavailable).
* ``1`` — Unexpected error in :func:`main` not handled by per-tier
  ``try``/``except`` blocks. The traceback is logged before exit.

The ``0`` exit code on graceful degradation is intentional per AAP
§0.8.4: the absence of releases is a valid measurement outcome and
must surface as ``chosen_tier == "none"`` plus empty ``releases``
and ``prereleases`` arrays — not as a non-zero exit.
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

# ---------------------------------------------------------------------------
# Make the workspace-local ``lib/`` package importable when the script is
# invoked directly with ``python3 04_extract_releases.py``. Without this,
# ``from lib.observability import get_logger`` would fail because the
# script's own directory is not necessarily on ``sys.path``. The insert
# at position 0 takes precedence over any system-wide ``lib`` package.
# ---------------------------------------------------------------------------

sys.path.insert(0, str(Path(__file__).resolve().parent))

from lib.git import git_for_each_ref  # noqa: E402
from lib.github import GithubClient  # noqa: E402
from lib.observability import get_logger  # noqa: E402
from lib.paths import (  # noqa: E402
    safe_output_path,
    OutputPathError,
)


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------


#: Canonical script identifier. Surfaces as the ``script`` field of every
#: emitted log line and as the ``producing_script`` reference in the
#: provenance block of the output artifact.
SCRIPT_NAME: str = "04_extract_releases"

#: Workspace root directory resolved from this script's location. The
#: ``parent.parent`` walk traverses ``scripts/`` -> ``acceleration-report/``.
#: All path constants below are anchored to this root so the script works
#: identically from any current working directory.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent

#: Directory under the workspace where raw data artifacts are persisted.
#: Created lazily by :func:`main` before the final write.
DATA_DIR: Path = WORKSPACE_ROOT / "data"

#: Default output path for the release inventory artifact. Overridable via
#: the ``--output`` CLI flag for ad-hoc runs against alternative
#: destinations (typically used by integration tests).
OUTPUT_PATH: Path = DATA_DIR / "releases.json"

#: Path to the canonical environment artifact emitted by
#: ``00_environment.sh``. The fields required by the ``_metadata`` block
#: (``extraction_timestamp``, ``run_id``, ``repository_slug``,
#: ``default_branch``, ``analysis_period_start``, ``analysis_period_end``)
#: are read from this file when it exists. The script falls back to
#: sensible defaults when the file is absent, so it remains useful for
#: standalone invocations that have not yet run stage 00.
ENVIRONMENT_PATH: Path = DATA_DIR / "environment.json"

#: Path to the canonical inflection artifact emitted by
#: ``01_detect_inflection.py``. The ``inflection_date_utc`` field
#: required by the ``_metadata`` block is read from this file when it
#: exists; otherwise a sentinel default is used.
INFLECTION_PATH: Path = DATA_DIR / "inflection.json"

#: Default GitHub repository slug. Overridable via the ``--repo-slug``
#: CLI flag or the ``GITHUB_REPO_SLUG`` environment variable. The default
#: matches the analyzed repository documented in the AAP.
REPO_SLUG_DEFAULT: str = "Blitzy-Sandbox/blitzy-RudderStack"

#: Default branch name used when no environment.json is available. The
#: AAP confirms ``main`` as the default branch.
DEFAULT_BRANCH_FALLBACK: str = "main"

#: Sentinel inflection date used when ``data/inflection.json`` is absent.
#: This is the AAP-observed Tier 2 inflection point for this repository
#: (the first ``agent@blitzy.com`` commit). Recorded here so that the
#: ``_metadata`` block always carries a syntactically valid ISO-8601
#: timestamp matching the schema's pattern; the canonical value flows
#: through ``data/inflection.json#date_utc`` when stage 01 has run.
INFLECTION_DATE_FALLBACK_UTC: str = "2026-02-25T02:58:59Z"

#: Sentinel analysis-period bounds used when ``data/environment.json``
#: is absent. The window matches the analysis range documented in the
#: AAP §0.2.3 (2026-02-23 → 2026-05-21).
ANALYSIS_PERIOD_START_FALLBACK: str = "2026-02-23T00:00:00Z"
ANALYSIS_PERIOD_END_FALLBACK: str = "2026-05-21T23:59:59Z"

#: Schema version this extractor emits. Bumped only when the output
#: shape changes in a backward-incompatible way.
ARTIFACT_SCHEMA_VERSION: str = "1.0.0"

#: Compiled regex for semver tag identification. Matches ``v1.2.3``,
#: ``1.2.3``, and ``v1.2.3-rc.1`` style tag names. The leading ``v`` is
#: optional. The trailing ``(?:[-+].*)?`` permits prerelease and build
#: metadata suffixes. ``tag_name.lstrip('v')`` is applied before the
#: match to normalize the optional ``v`` prefix.
SEMVER_TAG_RE: re.Pattern[str] = re.compile(
    r"^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$"
)

#: Compiled regex for prerelease tag identification. Matches any tag
#: name containing a ``-alpha``, ``-beta``, ``-rc``, or ``-dev`` segment
#: bounded by a word boundary. Case-insensitive. Used as the FALLBACK
#: signal for Tier 2 (annotated tags); the API's ``prerelease`` field
#: takes precedence for Tier 1.
PRERELEASE_RE: re.Pattern[str] = re.compile(
    r"-(?:alpha|beta|rc|dev)\b", re.IGNORECASE
)

#: Workflow names and path substrings that indicate a deploy-event run.
#: Matched against ``run.name`` and ``run.path`` (both lowercased) for
#: Tier 3. The list mirrors the AAP §0.5.3.10 specification and the
#: ``source_workflow_files`` field of the hand-authored seed.
DEPLOY_WORKFLOW_NAMES: list[str] = [
    "dispatch-deploy-event-dev",
    "release-please",
    "sync-release",
]

#: GitHub Actions Runs API page size. The endpoint caps at 100; matching
#: the cap minimizes round-trips during Tier 3 fallback enumeration.
DEFAULT_PAGE_SIZE: int = 100

#: Default rate-limit state recorded for Tier 1 when the API is
#: unavailable. Sentinel ``None`` values signal "not observed" rather
#: than "zero remaining".
DEFAULT_RATE_LIMIT_STATE: dict[str, Any] = {
    "limit": None,
    "remaining": None,
    "reset_iso": None,
}


# ---------------------------------------------------------------------------
# Time helpers
# ---------------------------------------------------------------------------


def iso_now() -> str:
    """Return the current UTC instant as an ISO-8601 string with ``Z`` suffix.

    The format matches the schema-pattern ``^\\d{4}-\\d{2}-\\d{2}T\\d{2}:
    \\d{2}:\\d{2}Z$`` required by ``_metadata.extraction_timestamp`` and by
    every other ISO timestamp field in ``releases.schema.json``. Wall-clock
    UTC is used so log lines and artifact fields collate identically
    across multiple machines and time zones during a pipeline rerun.

    The function is deterministic against ``datetime.now`` and carries no
    randomness; tests that fix ``datetime.now`` via ``unittest.mock`` will
    observe a stable return value.

    Returns:
        A string of the form ``"2026-05-23T14:32:11Z"``. Sub-second
        precision is intentionally dropped to keep the timestamp format
        compact and to match the schema pattern, which requires exactly
        ``HH:MM:SS`` without fractional seconds.
    """
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _normalise_iso(ts: str | None) -> str | None:
    """Coerce an arbitrary ISO-8601 timestamp into the schema-required form.

    GitHub Releases API timestamps can arrive as ``"2024-05-02T18:30:00Z"``
    (already valid for the schema) or with a UTC offset like
    ``"2024-05-02T18:30:00+00:00"`` (still ISO-8601 but rejected by the
    schema's stricter pattern). This helper converts the latter to the
    former by parsing through ``datetime.fromisoformat`` and re-emitting
    with the ``Z`` suffix.

    Returns ``None`` when ``ts`` is ``None`` or fails to parse, preserving
    the caller's option to omit the field rather than fabricate one.

    Args:
        ts: ISO-8601 timestamp string, or ``None``.

    Returns:
        A normalised timestamp matching ``^\\d{4}-\\d{2}-\\d{2}T
        \\d{2}:\\d{2}:\\d{2}Z$``, or ``None`` if ``ts`` is invalid or
        absent.
    """
    if not ts or not isinstance(ts, str):
        return None
    # Try the trivial fast path first: already in the canonical form.
    if re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", ts):
        return ts
    # Robust parse fallback for offset-suffixed and fractional inputs.
    try:
        # Replace trailing 'Z' with '+00:00' so fromisoformat accepts it
        # under Python 3.10 and earlier; harmless for Python 3.11+.
        parsed = datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    else:
        parsed = parsed.astimezone(timezone.utc)
    return parsed.strftime("%Y-%m-%dT%H:%M:%SZ")


# ---------------------------------------------------------------------------
# Metadata loaders (Rule 4: Internal Consistency)
# ---------------------------------------------------------------------------


def _load_json_safe(path: Path, logger: Any) -> dict[str, Any] | None:
    """Return the JSON-decoded contents of ``path`` or ``None`` on any failure.

    Emits a single warning log line on read or parse failure and returns
    ``None``. This pattern lets the caller decide whether the absence of
    a sibling artifact is fatal (it is not, in this script — the
    ``_metadata`` block falls back to defaults) without scattering
    ``try/except`` around every load.

    Args:
        path: Absolute path to the JSON file.
        logger: Structured-JSON logger acquired via
            :func:`lib.observability.get_logger`.

    Returns:
        The parsed top-level JSON object on success. ``None`` when the
        file does not exist, cannot be opened, or fails to parse. The
        helper accepts only JSON objects at the top level; arrays or
        scalars return ``None`` with a warning.
    """
    if not path.exists():
        logger.info(
            "sibling_artifact_absent",
            extra={
                "event": "sibling_artifact_absent",
                "path": str(path),
                "consequence": "falling back to module-level default values",
            },
        )
        return None
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        logger.warning(
            "sibling_artifact_unreadable",
            extra={
                "event": "sibling_artifact_unreadable",
                "path": str(path),
                "error": str(exc),
            },
        )
        return None
    try:
        payload = json.loads(text)
    except json.JSONDecodeError as exc:
        logger.warning(
            "sibling_artifact_invalid_json",
            extra={
                "event": "sibling_artifact_invalid_json",
                "path": str(path),
                "error": str(exc),
            },
        )
        return None
    if not isinstance(payload, dict):
        logger.warning(
            "sibling_artifact_wrong_shape",
            extra={
                "event": "sibling_artifact_wrong_shape",
                "path": str(path),
                "type": type(payload).__name__,
            },
        )
        return None
    return payload


def _get_nested(obj: dict[str, Any], *keys: str) -> Any:
    """Return ``obj[k1][k2]...`` or ``None`` if any key is missing.

    Both ``environment.json`` and ``inflection.json`` may carry the
    canonical correlation fields either at the top level (legacy live-
    extractor shape) or nested under ``_metadata`` (seed shape). This
    helper lets callers query both layouts with a single fall-through
    chain.

    Args:
        obj: The container dictionary.
        *keys: Successive keys to traverse.

    Returns:
        The terminal value, or ``None`` if any step encounters a missing
        key or a non-dict intermediate.
    """
    current: Any = obj
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
    """Construct the ``_metadata`` block required by ``releases.schema.json``.

    Reads the canonical per-run correlation fields from
    ``data/environment.json`` (top-level OR ``_metadata`` nested) and
    ``data/inflection.json`` (top-level ``date_utc`` field), falling back
    to AAP-documented defaults when those artifacts are absent. The
    fallback path is exercised when this script is invoked standalone
    before stages 00 and 01 have run; the canonical path is exercised
    during normal pipeline operation.

    Args:
        args: Parsed CLI namespace; ``repo_slug`` is used as the
            ``repository_slug`` fallback when ``environment.json`` is
            unavailable.
        env: Decoded contents of ``data/environment.json`` or ``None``.
        infl: Decoded contents of ``data/inflection.json`` or ``None``.
        extraction_ts: Wall-clock UTC timestamp used as
            ``extraction_timestamp`` when no canonical run timestamp is
            available from ``environment.json``.

    Returns:
        A dictionary suitable for the ``_metadata`` block of the output
        artifact. All schema-required fields are populated.
    """
    # extraction_timestamp: prefer the canonical one from environment.json
    # so that this artifact correlates with the rest of the pipeline run.
    canonical_ts: str | None = None
    if env is not None:
        canonical_ts = _normalise_iso(
            env.get("extraction_timestamp")
            or _get_nested(env, "_metadata", "extraction_timestamp")
        )
    extraction_timestamp = canonical_ts or extraction_ts

    # run_id: prefer the environment's canonical run_id (which itself was
    # propagated from BLITZY_RUN_ID by stage 00). If absent, the logger
    # factory has already ensured BLITZY_RUN_ID is set on this process;
    # read it from there.
    run_id: str | None = None
    if env is not None:
        run_id = (
            env.get("run_id")
            or _get_nested(env, "_metadata", "run_id")
        )
    if not run_id:
        run_id = os.environ.get("BLITZY_RUN_ID")
    if not run_id:
        # Last-resort fallback: synthesise a fresh UUID4. Encoded inline
        # to avoid importing ``uuid`` twice (the observability layer
        # already imports it as part of get_logger()'s setup).
        import uuid as _uuid_local  # local import keeps top-level minimal
        run_id = str(_uuid_local.uuid4())

    # repository_slug: argparse default already resolves --repo-slug,
    # GITHUB_REPO_SLUG env var, and the module-level constant in that
    # order. Prefer the environment artifact's value when it differs
    # (this guarantees Rule-4 internal consistency across artifacts).
    repository_slug = args.repo_slug
    if env is not None:
        env_slug = (
            env.get("repository_slug")
            or _get_nested(env, "_metadata", "repository_slug")
        )
        if env_slug:
            repository_slug = env_slug

    default_branch = DEFAULT_BRANCH_FALLBACK
    if env is not None:
        env_branch = (
            env.get("default_branch")
            or _get_nested(env, "_metadata", "default_branch")
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

    # inflection_date_utc: prefer inflection.json's top-level date_utc,
    # then its _metadata nested form, then environment.json's pre-baked
    # inflection_date_utc, then the AAP-documented fallback.
    inflection_date_utc = INFLECTION_DATE_FALLBACK_UTC
    candidate: str | None = None
    if infl is not None:
        candidate = (
            _normalise_iso(infl.get("date_utc"))
            or _normalise_iso(_get_nested(infl, "_metadata", "inflection_date_utc"))
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
        "artifact_kind": "releases_inventory",
        "schema_version": ARTIFACT_SCHEMA_VERSION,
    }




# ---------------------------------------------------------------------------
# Tier 1 — GitHub Releases API
# ---------------------------------------------------------------------------


def _shape_api_release(rel: dict[str, Any]) -> dict[str, Any]:
    """Normalise one GitHub Releases API record into the artifact's shape.

    The Releases API returns a rich object with author info, asset
    inventory, body markdown, and so on. The schema-required fields plus
    a documented "pipeline-useful" subset are extracted here; everything
    else is dropped to keep the artifact compact. Timestamps are passed
    through :func:`_normalise_iso` so the schema pattern
    ``^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}Z$`` is satisfied.

    Args:
        rel: A single decoded release object from the API.

    Returns:
        A dict with ``tag_name``, ``name``, ``target_commitish``,
        ``draft``, ``prerelease``, ``created_at``, ``published_at``,
        ``html_url``, and ``source_tier`` keys. The ``source_tier`` field
        is hard-coded to ``"github_releases_api"`` so downstream consumers
        can tell which tier produced each record without consulting the
        artifact's ``chosen_tier`` field.
    """
    return {
        "tag_name": rel.get("tag_name") or "",
        "name": rel.get("name"),
        "target_commitish": rel.get("target_commitish"),
        "draft": bool(rel.get("draft", False)),
        "prerelease": bool(rel.get("prerelease", False)),
        "created_at": _normalise_iso(rel.get("created_at")),
        "published_at": _normalise_iso(rel.get("published_at"))
        or _normalise_iso(rel.get("created_at"))
        or "",
        "html_url": rel.get("html_url"),
        "body": rel.get("body"),
        "source_tier": "github_releases_api",
    }


def _fetch_tier1_releases(
    gh: GithubClient,
    repo_slug: str,
    logger: Any,
) -> tuple[list[dict[str, Any]], bool, str | None, int]:
    """Fetch every release via the GitHub Releases API.

    Iterates the paginated ``GET /repos/{owner}/{repo}/releases`` endpoint
    through :meth:`GithubClient.paginate_endpoint`. The page size is
    pinned at :data:`DEFAULT_PAGE_SIZE` (100), the maximum permitted by
    GitHub. Every error condition — network failure, 4xx, 5xx after
    retry exhaustion, schema mismatch — is caught and recorded; the
    function never raises. Callers inspect the ``api_available`` flag to
    decide whether to fall back to Tier 2 / Tier 3.

    Args:
        gh: A pre-constructed :class:`GithubClient` instance.
        repo_slug: ``owner/repo`` for the target repository.
        logger: Structured-JSON logger.

    Returns:
        A 4-tuple ``(api_releases, api_available, api_error, pages_fetched)``:

        * ``api_releases`` — list of shaped release dicts (possibly
          empty if the endpoint returned zero rows).
        * ``api_available`` — ``True`` iff the endpoint responded at all,
          ``False`` on network/auth/permission failure.
        * ``api_error`` — short, human-readable error message when
          ``api_available`` is ``False``; ``None`` on success.
        * ``pages_fetched`` — best-effort count of pages traversed
          (used in the ``tier_availability`` block for transparency).
    """
    api_releases: list[dict[str, Any]] = []
    pages_fetched = 0
    try:
        endpoint = f"/repos/{repo_slug}/releases"
        # The paginate_endpoint generator yields one ITEM per iteration,
        # not one PAGE. Counting pages would require a custom loop using
        # :meth:`get` + :meth:`paginate`; for transparency we approximate
        # pages by ceil(items / per_page) after collection.
        for rel in gh.paginate_endpoint(
            endpoint,
            params={"per_page": DEFAULT_PAGE_SIZE},
        ):
            api_releases.append(_shape_api_release(rel))
        # Approximate pages from item count.
        if api_releases:
            pages_fetched = (
                (len(api_releases) + DEFAULT_PAGE_SIZE - 1)
                // DEFAULT_PAGE_SIZE
            )
        else:
            pages_fetched = 1
        logger.info(
            "tier1_releases_api_succeeded",
            extra={
                "event": "releases_api_ok",
                "endpoint": endpoint,
                "release_count": len(api_releases),
                "pages_fetched": pages_fetched,
            },
        )
        return api_releases, True, None, pages_fetched
    except Exception as exc:  # noqa: BLE001 — read-only fallback discipline
        # Catch broad here is intentional: the AAP requires graceful
        # degradation on ANY API failure. ``GithubClient`` already
        # narrows what it raises; everything else is a downstream
        # failure mode (DNS, TLS, requests.RequestException, schema
        # mismatch). The error message is short enough to embed in the
        # tier_availability block.
        error_message = f"{type(exc).__name__}: {exc}"
        logger.warning(
            "tier1_releases_api_unavailable",
            extra={
                "event": "releases_api_failed",
                "error": error_message,
            },
        )
        return [], False, error_message, 0


# ---------------------------------------------------------------------------
# Tier 2 — Local annotated git tags
# ---------------------------------------------------------------------------


def _fetch_tier2_tags(logger: Any) -> tuple[list[dict[str, Any]], int]:
    """Enumerate local annotated git tags matching the semver pattern.

    Wraps :func:`lib.git.git_for_each_ref` over ``refs/tags/`` with a
    pipe-separated format string that yields, per ref:
    ``%(refname:short)|%(creatordate:iso-strict)|%(objectname)|%(*objectname)|%(taggerdate:iso-strict)``.
    Refs whose tag name does not match :data:`SEMVER_TAG_RE` (with the
    optional leading ``v`` stripped) are silently filtered. Tags whose
    name matches :data:`PRERELEASE_RE` are classified as prereleases via
    the ``prerelease`` field on the returned dicts.

    Note: at the time the AAP was authored this fork carried ZERO
    annotated tags (``git tag -l | wc -l`` returned ``0``). The helper
    is implemented in full so that any future tag publication is
    automatically picked up.

    Args:
        logger: Structured-JSON logger.

    Returns:
        A 2-tuple ``(tag_releases, total_tags_found)`` where
        ``tag_releases`` is the list of shaped release dicts (already
        including tags that look like prereleases by name) and
        ``total_tags_found`` is the total count of semver-matching tags
        observed (i.e. ``len(tag_releases)``).

        The function never raises. Read failures from the git
        subprocess are caught and logged; the return is then
        ``([], 0)``.
    """
    fmt = (
        "%(refname:short)|%(creatordate:iso-strict)|"
        "%(objectname)|%(*objectname)|%(taggerdate:iso-strict)"
    )
    tag_releases: list[dict[str, Any]] = []
    try:
        lines = git_for_each_ref("refs/tags/", fmt)
    except Exception as exc:  # noqa: BLE001 — read-only fallback
        # ``git_for_each_ref`` raises ``GitReadOnlyViolation``
        # (subclass of ``ValueError``) or
        # ``subprocess.CalledProcessError``. Either is treated as a
        # tier-unavailable signal here; the caller will skip Tier 2.
        logger.warning(
            "tier2_tag_scan_failed",
            extra={
                "event": "tag_scan_failed",
                "error": f"{type(exc).__name__}: {exc}",
            },
        )
        return [], 0

    for line in lines:
        parts = line.split("|")
        if len(parts) < 3:
            # Defensive: a corrupt format string or a tag with embedded
            # pipes (extremely unusual) would land here. Skip silently
            # because tag enumeration is best-effort.
            continue
        tag_name = parts[0]
        creator_date = parts[1] if len(parts) > 1 else ""
        objname = parts[2] if len(parts) > 2 else ""
        # `*objectname` is the dereferenced target SHA for annotated
        # tags; empty for lightweight tags pointing directly at the
        # commit. Prefer it when present (gives the actual commit).
        target_sha = (parts[3] if len(parts) > 3 and parts[3] else objname)
        tagger_date = parts[4] if len(parts) > 4 else ""

        # Apply the semver pattern AFTER stripping the optional leading
        # ``v`` so both ``v1.2.3`` and ``1.2.3`` match. Tags that do not
        # match the pattern (e.g. ``release-2024-05``) are excluded
        # from the release inventory per AAP §0.5.3.10.
        if not SEMVER_TAG_RE.match(tag_name.lstrip("v")):
            continue

        is_prerelease = bool(PRERELEASE_RE.search(tag_name))
        published_at = (
            _normalise_iso(tagger_date)
            or _normalise_iso(creator_date)
            or ""
        )
        tag_releases.append({
            "tag_name": tag_name,
            "name": tag_name,
            "target_commitish": target_sha or None,
            "draft": False,
            "prerelease": is_prerelease,
            "created_at": _normalise_iso(creator_date),
            "published_at": published_at,
            "html_url": None,
            "body": None,
            "source_tier": "git_tag_scan",
        })

    logger.info(
        "tier2_tag_scan_complete",
        extra={
            "event": "tag_scan_ok",
            "tag_count": len(tag_releases),
            "lines_scanned": len(lines),
        },
    )
    return tag_releases, len(tag_releases)


# ---------------------------------------------------------------------------
# Tier 3 — CI deploy events
# ---------------------------------------------------------------------------


def _fetch_tier3_deploy_events(
    gh: GithubClient,
    repo_slug: str,
    logger: Any,
) -> tuple[list[dict[str, Any]], bool, str | None]:
    """Synthesise release records from successful deploy-event workflow runs.

    Iterates the paginated ``GET /repos/{owner}/{repo}/actions/runs``
    endpoint (which wraps the rows under a ``workflow_runs`` key —
    hence ``item_key="workflow_runs"``) and selects runs whose
    lowercased ``name`` or ``path`` field contains any of
    :data:`DEPLOY_WORKFLOW_NAMES`. Runs with non-``success`` conclusion
    are excluded. Each surviving run is synthesised into a release
    record with the synthetic tag ``deploy-<run_id>`` so downstream
    consumers can tell deploy-event records apart from API and tag
    records by name alone.

    This tier is the lowest-confidence source (Low per AAP §0.5.3.10)
    and is intended only as a last-resort signal when neither the
    Releases API nor local tags yield anything. The caller in
    :func:`main` honours this precedence.

    Args:
        gh: A pre-constructed :class:`GithubClient` instance.
        repo_slug: ``owner/repo`` for the target repository.
        logger: Structured-JSON logger.

    Returns:
        A 3-tuple ``(deploy_releases, available, error)``:

        * ``deploy_releases`` — list of synthesised release dicts.
        * ``available`` — ``True`` iff the endpoint responded at all.
        * ``error`` — short error message when ``available`` is ``False``,
          else ``None``.
    """
    deploy_releases: list[dict[str, Any]] = []
    try:
        endpoint = f"/repos/{repo_slug}/actions/runs"
        for run in gh.paginate_endpoint(
            endpoint,
            params={"per_page": DEFAULT_PAGE_SIZE},
            item_key="workflow_runs",
        ):
            name = (run.get("name") or "").lower()
            wf_path = (run.get("path") or "").lower()
            matches = any(
                dep in name or dep in wf_path
                for dep in DEPLOY_WORKFLOW_NAMES
            )
            if not matches:
                continue
            if run.get("conclusion") != "success":
                continue
            run_id_field = run.get("id")
            synthetic_tag = (
                f"deploy-{run_id_field}"
                if run_id_field is not None
                else "deploy-unknown"
            )
            published_at = (
                _normalise_iso(run.get("run_started_at"))
                or _normalise_iso(run.get("created_at"))
                or ""
            )
            deploy_releases.append({
                "tag_name": synthetic_tag,
                "name": run.get("name"),
                "target_commitish": run.get("head_sha"),
                "draft": False,
                "prerelease": False,
                "created_at": _normalise_iso(run.get("created_at")),
                "published_at": published_at,
                "html_url": run.get("html_url"),
                "body": None,
                "source_tier": "ci_deploy_event",
                "workflow_path": run.get("path"),
                "workflow_run_id": run_id_field,
            })
        logger.info(
            "tier3_deploy_scan_complete",
            extra={
                "event": "deploy_scan_ok",
                "endpoint": endpoint,
                "match_count": len(deploy_releases),
            },
        )
        return deploy_releases, True, None
    except Exception as exc:  # noqa: BLE001 — read-only fallback
        error_message = f"{type(exc).__name__}: {exc}"
        logger.warning(
            "tier3_deploy_scan_failed",
            extra={
                "event": "deploy_scan_failed",
                "error": error_message,
            },
        )
        return [], False, error_message




# ---------------------------------------------------------------------------
# Tier selection and prerelease partitioning
# ---------------------------------------------------------------------------


def _choose_tier(
    api_available: bool,
    api_releases: list[dict[str, Any]],
    tag_releases: list[dict[str, Any]],
    deploy_releases: list[dict[str, Any]],
) -> tuple[str, list[dict[str, Any]]]:
    """Apply the AAP §0.5.3.10 source-precedence rule.

    The precedence is strictly: Tier 1 (API) -> Tier 2 (tags) ->
    Tier 3 (deploy events) -> ``"none"``. A tier is "the chosen one"
    when it is the first tier in this order with a non-empty release
    list. The chosen tier's records become the primary list; downstream
    consumers segregate prereleases via :func:`_partition_prereleases`.

    Args:
        api_available: Whether the Releases API responded.
        api_releases: Output of :func:`_fetch_tier1_releases`.
        tag_releases: Output of :func:`_fetch_tier2_tags`.
        deploy_releases: Output of :func:`_fetch_tier3_deploy_events`.

    Returns:
        A 2-tuple ``(chosen_tier, primary)``. ``chosen_tier`` is one of
        the schema-enum strings ``"github_releases_api"``,
        ``"git_tag_scan"``, ``"ci_deploy_event"``, or ``"none"``.
        ``primary`` is the list of release dicts produced by the chosen
        tier (empty when ``chosen_tier == "none"``).
    """
    if api_available and api_releases:
        return "github_releases_api", api_releases
    if tag_releases:
        return "git_tag_scan", tag_releases
    if deploy_releases:
        return "ci_deploy_event", deploy_releases
    return "none", []


def _partition_prereleases(
    primary: list[dict[str, Any]],
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Split the primary list into ``releases`` and ``prereleases``.

    A record is routed to ``prereleases`` when its ``prerelease`` flag
    is truthy; the flag is set by:

    * The upstream GitHub Releases API (Tier 1).
    * :func:`_fetch_tier2_tags` based on :data:`PRERELEASE_RE` matching
      against the tag name (Tier 2).
    * Always ``False`` for Tier 3 — synthesised ``deploy-<id>`` tags
      carry no semver suffix.

    Drafts (``draft == True``) are excluded from both lists per AAP
    §0.5.3.10: a draft release is not yet "published" and would
    misrepresent the count.

    Args:
        primary: The list of shaped release dicts chosen by
            :func:`_choose_tier`.

    Returns:
        A 2-tuple ``(releases, prereleases)`` where both are
        chronologically ordered (newest first by ``published_at``) and
        disjoint.
    """
    releases: list[dict[str, Any]] = []
    prereleases: list[dict[str, Any]] = []
    for rec in primary:
        if rec.get("draft"):
            continue
        if rec.get("prerelease"):
            prereleases.append(rec)
        else:
            releases.append(rec)
    # Sort newest first by published_at. Records with empty published_at
    # sort to the end (string compare on "" is the minimum), which is
    # the safe default for downstream window-bucketing.
    releases.sort(key=lambda r: r.get("published_at") or "", reverse=True)
    prereleases.sort(key=lambda r: r.get("published_at") or "", reverse=True)
    return releases, prereleases


def _summary_extremes(
    releases: list[dict[str, Any]],
) -> tuple[str | None, str | None]:
    """Return ``(earliest_published_at, latest_published_at)`` for the list.

    Used by the ``summary`` block in the output artifact. Both fields
    are nullable per the schema and become ``None`` when the list is
    empty. Each record's ``published_at`` is already normalised to the
    schema ISO pattern, so direct string min/max ordering is
    lexicographically correct.

    Args:
        releases: Final ``releases`` list (post-partition).

    Returns:
        A 2-tuple of ISO-8601 timestamps or ``None`` values.
    """
    published_dates = [
        r["published_at"]
        for r in releases
        if r.get("published_at")
    ]
    if not published_dates:
        return None, None
    return min(published_dates), max(published_dates)


# ---------------------------------------------------------------------------
# Payload assembly and write
# ---------------------------------------------------------------------------


def _build_tier_availability(
    api_available: bool,
    api_releases: list[dict[str, Any]],
    api_error: str | None,
    api_pages_fetched: int,
    tag_releases: list[dict[str, Any]],
    deploy_releases: list[dict[str, Any]],
    deploy_available: bool,
    deploy_error: str | None,
    repo_slug: str,
    deploy_tier_invoked: bool,
) -> dict[str, Any]:
    """Build the rich ``tier_availability`` dict-of-dicts block.

    The shape matches the seed artifact's per-tier nested form, which
    is what :func:`compute_m9_releases` in
    ``09_compute_metrics.py`` iterates: each value is a dict with at
    least an ``available`` boolean and (when unavailable) an
    ``unavailable_reason`` string. This keeps the live extractor's
    output compatible with the downstream consumer while still
    conforming to ``releases.schema.json`` (which permits this nested
    layout under ``additionalProperties: true``).

    Args:
        api_available: Whether Tier 1 responded.
        api_releases: Tier 1 release list (used for counts).
        api_error: Tier 1 error message, or ``None``.
        api_pages_fetched: Number of pages traversed by Tier 1.
        tag_releases: Tier 2 release list.
        deploy_releases: Tier 3 release list.
        deploy_available: Whether Tier 3 responded.
        deploy_error: Tier 3 error message, or ``None``.
        repo_slug: ``owner/repo`` for endpoint string composition.
        deploy_tier_invoked: ``True`` if Tier 3 was attempted (governed
            by the AAP precedence rule); ``False`` when it was skipped
            because an earlier tier produced data.

    Returns:
        The fully populated ``tier_availability`` dict ready for the
        output payload.
    """
    api_non_pre = sum(
        1 for r in api_releases if not r.get("prerelease")
    )
    api_pre = sum(1 for r in api_releases if r.get("prerelease"))

    tier1: dict[str, Any] = {
        "available": api_available,
        "endpoint": f"GET /repos/{repo_slug}/releases",
        "fetched_at": iso_now() if api_available else None,
        "total_releases_count": len(api_releases),
        "non_prerelease_count": api_non_pre,
        "prerelease_count": api_pre,
        "pagination": {
            "per_page": DEFAULT_PAGE_SIZE,
            "pages_fetched": api_pages_fetched,
        },
        "rate_limit_state": dict(DEFAULT_RATE_LIMIT_STATE),
    }
    if not api_available and api_error:
        tier1["unavailable_reason"] = api_error

    tier2: dict[str, Any] = {
        "available": len(tag_releases) > 0,
        "source": "local_git",
        "search_pattern": SEMVER_TAG_RE.pattern,
        "search_command": (
            "git for-each-ref refs/tags/ "
            "--format='%(refname:short)|%(creatordate:iso-strict)|%(objectname)|%(*objectname)|%(taggerdate:iso-strict)'"
        ),
        "verification_command": "git tag -l | wc -l",
        "tags_found": len(tag_releases),
        "tags": [r.get("tag_name") for r in tag_releases],
    }
    if not tier2["available"]:
        tier2["unavailable_reason"] = (
            "Zero local annotated tags match the semver pattern "
            f"{SEMVER_TAG_RE.pattern!r}."
        )

    tier3: dict[str, Any] = {
        "available": deploy_available and len(deploy_releases) > 0,
        "source_workflow_files": [
            "dispatch-deploy-event-dev.yaml",
            "release-please.yaml",
            "sync-release.yaml",
        ],
        "endpoint": f"GET /repos/{repo_slug}/actions/runs",
        "events_count": len(deploy_releases),
        "invoked": deploy_tier_invoked,
    }
    if not deploy_tier_invoked:
        tier3["unavailable_reason"] = (
            "Tier 3 skipped per AAP precedence rule: a higher tier "
            "produced release data."
        )
    elif not deploy_available and deploy_error:
        tier3["unavailable_reason"] = deploy_error
    elif deploy_available and len(deploy_releases) == 0:
        tier3["unavailable_reason"] = (
            "Tier 3 invoked but no workflow runs matched any of "
            f"{DEPLOY_WORKFLOW_NAMES!r} with conclusion 'success'."
        )

    return {
        "tier_1_github_releases_api": tier1,
        "tier_2_annotated_tags": tier2,
        "tier_3_ci_deploy_events": tier3,
    }


def _build_chosen_tier_rationale(
    chosen_tier: str,
    api_available: bool,
    api_releases: list[dict[str, Any]],
    tag_releases: list[dict[str, Any]],
    deploy_releases: list[dict[str, Any]],
) -> str:
    """Compose the free-text ``chosen_tier_rationale`` field.

    The schema permits this field as an optional explanation of the
    ``chosen_tier`` value, surfaced in the report's Risk Assessment
    section when ``chosen_tier`` is non-primary or ``"none"``. The
    rationale documents:

    * Which tier was selected.
    * Why higher-precedence tiers were skipped.
    * The observed counts at each tier.

    Args:
        chosen_tier: One of the schema-enum values.
        api_available: Whether Tier 1 responded.
        api_releases: Tier 1 release list.
        tag_releases: Tier 2 release list.
        deploy_releases: Tier 3 release list.

    Returns:
        A human-readable single-paragraph rationale string.
    """
    if chosen_tier == "github_releases_api":
        return (
            f"Tier 1 (GitHub Releases API) yielded {len(api_releases)} "
            "release record(s); selected per AAP §0.5.3.10 precedence "
            "as the authoritative source."
        )
    if chosen_tier == "git_tag_scan":
        if api_available:
            return (
                "Tier 1 (GitHub Releases API) responded but returned "
                "zero releases; fell back to Tier 2 (annotated git "
                f"tags) which yielded {len(tag_releases)} tag(s) "
                f"matching {SEMVER_TAG_RE.pattern!r}."
            )
        return (
            "Tier 1 (GitHub Releases API) was unavailable; fell back "
            f"to Tier 2 (annotated git tags) which yielded "
            f"{len(tag_releases)} tag(s) matching "
            f"{SEMVER_TAG_RE.pattern!r}."
        )
    if chosen_tier == "ci_deploy_event":
        return (
            "Tiers 1 and 2 yielded no releases; fell back to Tier 3 "
            "(CI deploy events) which synthesised "
            f"{len(deploy_releases)} record(s) from successful "
            "workflow runs matching "
            f"{DEPLOY_WORKFLOW_NAMES!r}. Confidence for Metric 9 is "
            "Low per AAP §0.5.3.10."
        )
    # "none"
    return (
        "All three tiers yielded zero or were unavailable; Metric 9 "
        "will report 'insufficient_signal' per AAP §0.5.3.10. Tier 1 "
        f"(GitHub Releases API): {'available' if api_available else 'unavailable'}, "
        f"{len(api_releases)} record(s). Tier 2 (annotated git tags): "
        f"{len(tag_releases)} tag(s) matched. Tier 3 (CI deploy "
        f"events): {len(deploy_releases)} record(s)."
    )


def _build_payload(
    metadata: dict[str, Any],
    chosen_tier: str,
    tier_availability: dict[str, Any],
    releases: list[dict[str, Any]],
    prereleases: list[dict[str, Any]],
    chosen_tier_rationale: str,
) -> dict[str, Any]:
    """Assemble the final ``releases.json`` payload.

    The payload conforms to ``scripts/lib/schemas/releases.schema.json``:
    only the schema-permitted root-level keys are present; the
    ``_metadata`` block is the canonical correlation envelope; the
    ``releases`` and ``prereleases`` arrays carry the schema-required
    fields per record; ``chosen_tier`` is one of the enum values.

    Args:
        metadata: The ``_metadata`` block from :func:`_build_metadata`.
        chosen_tier: Schema-enum string from :func:`_choose_tier`.
        tier_availability: From :func:`_build_tier_availability`.
        releases: Final non-prerelease list.
        prereleases: Final prerelease list.
        chosen_tier_rationale: From
            :func:`_build_chosen_tier_rationale`.

    Returns:
        The final payload dict, ready to be JSON-serialized.
    """
    earliest, latest = _summary_extremes(releases)
    summary: dict[str, Any] = {
        "total_releases": len(releases),
        "total_prereleases": len(prereleases),
        "earliest_release_iso": earliest,
        "latest_release_iso": latest,
        "release_frequency_mean_per_window": None,
        "release_frequency_mean_per_window_reason": (
            "Per-window aggregation is the responsibility of "
            "09_compute_metrics.py; this artifact carries the raw "
            "inventory only."
        ),
    }

    provenance: dict[str, Any] = {
        "spec_section": "AAP §0.5.3.10 (Metric 9 — Releases)",
        "source_precedence": [
            "GitHub Releases / GitLab Releases API",
            "Annotated git tags matching v?\\d+\\.\\d+\\.\\d+",
            "Deployment events from CI/CD",
        ],
        "downstream_consumer": (
            "blitzy/acceleration-report/data/metrics.json#m9"
        ),
        "downstream_consumer_field": "value",
        "downstream_consumer_expected_when_chosen_tier_is_none": (
            "insufficient_signal"
        ),
        "producing_script": (
            "blitzy/acceleration-report/scripts/04_extract_releases.py"
        ),
    }

    payload: dict[str, Any] = {
        "_metadata": metadata,
        "tier_availability": tier_availability,
        "chosen_tier": chosen_tier,
        "chosen_tier_rationale": chosen_tier_rationale,
        "releases": releases,
        "prereleases": prereleases,
        "summary": summary,
        "prerelease_suffix_pattern": PRERELEASE_RE.pattern,
        "prerelease_suffix_pattern_source": "AAP §0.5.3.10",
        "provenance": provenance,
    }
    return payload


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    """Persist ``payload`` to ``path`` as pretty-printed UTF-8 JSON.

    Creates the parent directory if necessary. Writes through a
    temporary file in the same directory and uses ``os.replace`` for
    an atomic rename so that a concurrent reader never observes a
    partial file.

    Args:
        path: Destination file path.
        payload: JSON-serializable dict to persist.

    Raises:
        OSError: When the destination directory cannot be created or
            the rename fails.
        TypeError: When ``payload`` contains non-JSON-serializable
            values (a defect; this script's payload is fully JSON-safe
            by construction).
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = path.with_suffix(path.suffix + ".tmp")
    serialized = json.dumps(
        payload,
        indent=2,
        ensure_ascii=False,
        sort_keys=False,
    )
    tmp_path.write_text(serialized + "\n", encoding="utf-8")
    os.replace(tmp_path, path)


# ---------------------------------------------------------------------------
# Dry-run plan
# ---------------------------------------------------------------------------


def _build_dry_run_plan(args: argparse.Namespace) -> dict[str, Any]:
    """Return a JSON-serializable description of what a live run would do.

    The plan is printed by ``--dry-run`` and serves as the Rule-1
    Observability readiness preflight. It lists every external endpoint
    the script would contact, every git command it would invoke, and
    every file it would write — without executing any of them.

    Args:
        args: Parsed CLI namespace.

    Returns:
        A dict with ``action``, ``script``, ``repo_slug``, ``api_calls``,
        ``git_commands``, ``reads``, and ``writes`` keys.
    """
    return {
        "action": "dry_run",
        "script": SCRIPT_NAME,
        "repo_slug": args.repo_slug,
        "api_calls": [
            f"GET /repos/{args.repo_slug}/releases?per_page={DEFAULT_PAGE_SIZE}",
            f"GET /repos/{args.repo_slug}/actions/runs?per_page={DEFAULT_PAGE_SIZE} "
            "(only invoked when Tier 1 returned empty AND Tier 2 returned empty)",
        ],
        "git_commands": [
            (
                "git for-each-ref refs/tags/ "
                "--format='%(refname:short)|%(creatordate:iso-strict)|"
                "%(objectname)|%(*objectname)|%(taggerdate:iso-strict)'"
            ),
        ],
        "reads": [
            str(ENVIRONMENT_PATH),
            str(INFLECTION_PATH),
        ],
        "writes": [args.output],
        "tier_precedence": [
            "github_releases_api",
            "git_tag_scan",
            "ci_deploy_event",
        ],
        "deploy_workflow_names": list(DEPLOY_WORKFLOW_NAMES),
        "prerelease_suffix_pattern": PRERELEASE_RE.pattern,
        "semver_tag_pattern": SEMVER_TAG_RE.pattern,
    }


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main() -> int:
    """Extract the release inventory and persist ``data/releases.json``.

    Workflow:

    1. Parse CLI arguments (``--dry-run``, ``--repo-slug``, ``--output``).
    2. Acquire the structured-JSON logger; this also seeds
       ``BLITZY_RUN_ID`` if not already set.
    3. On ``--dry-run``: print the dry-run plan and exit 0.
    4. Load ``environment.json`` and ``inflection.json`` (best effort,
       falls back to defaults).
    5. Construct the GitHub client (authenticated if ``GH_TOKEN`` is
       set, unauthenticated otherwise).
    6. Tier 1: fetch releases via the API.
    7. Tier 2: enumerate semver-matching annotated tags.
    8. Tier 3: if and only if Tier 1 was reachable AND returned empty
       AND Tier 2 also returned empty, fetch deploy events from the
       Actions Runs API.
    9. Apply the precedence rule: select the chosen tier.
    10. Partition the chosen list into ``releases`` and ``prereleases``.
    11. Assemble the payload and write it atomically to disk.
    12. Emit a final ``script_complete`` log line.

    Returns:
        ``0`` on success (including graceful degradation where all
        tiers yield empty). ``1`` on an unexpected exception that
        escapes the per-tier try/except blocks; the traceback is
        logged at error level before exit.
    """
    parser = argparse.ArgumentParser(
        prog=SCRIPT_NAME,
        description=(
            "Extract release inventory with tiered source precedence "
            "(GitHub Releases API -> annotated git tags -> CI deploy "
            "events). Emits data/releases.json. Read-only."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Print the planned API calls and git commands, then exit "
            "without performing any network or git activity."
        ),
    )
    parser.add_argument(
        "--repo-slug",
        default=os.environ.get("GITHUB_REPO_SLUG", REPO_SLUG_DEFAULT),
        help=(
            "GitHub repository slug in 'owner/name' form. Defaults to "
            "the GITHUB_REPO_SLUG env var, falling back to "
            f"{REPO_SLUG_DEFAULT!r}."
        ),
    )
    parser.add_argument(
        "--output",
        default=str(OUTPUT_PATH),
        help=(
            f"Destination path for the JSON artifact. Defaults to "
            f"{OUTPUT_PATH!s}."
        ),
    )
    args = parser.parse_args()

    logger = get_logger(SCRIPT_NAME)
    logger.info(
        "script_started",
        extra={
            "event": "script_started",
            "dry_run": args.dry_run,
            "repo_slug": args.repo_slug,
            "output": args.output,
            "authenticated": bool(os.environ.get("GH_TOKEN")),
        },
    )

    if args.dry_run:
        plan = _build_dry_run_plan(args)
        print(json.dumps(plan, indent=2, ensure_ascii=False))
        logger.info(
            "script_complete",
            extra={
                "event": "script_complete",
                "dry_run": True,
            },
        )
        return 0

    # Capture the run-start timestamp so the same value is used as the
    # extraction_timestamp fallback in _build_metadata. Without this the
    # function would call iso_now() a second time and the timestamps
    # could differ by a second.
    extraction_ts = iso_now()

    env_payload = _load_json_safe(ENVIRONMENT_PATH, logger)
    infl_payload = _load_json_safe(INFLECTION_PATH, logger)
    metadata = _build_metadata(args, env_payload, infl_payload, extraction_ts)

    # The repository slug used by every API call must align with the
    # one recorded in _metadata so the artifact is self-consistent
    # (Rule 4). The metadata builder reconciles the args.repo_slug with
    # the environment.json value; use the reconciled value below.
    effective_repo_slug = metadata["repository_slug"]

    gh = GithubClient(
        token=os.environ.get("GH_TOKEN"),
        logger=logger,
    )

    # Tier 1
    api_releases, api_available, api_error, api_pages = _fetch_tier1_releases(
        gh, effective_repo_slug, logger
    )

    # Tier 2 — always attempted; cheap and useful as a cross-check.
    tag_releases, _tag_count = _fetch_tier2_tags(logger)

    # Tier 3 — only attempted when Tier 1 produced no signal AND Tier 2
    # produced no signal AND the API client is reachable (no point
    # attempting another API call if Tier 1 already failed at the
    # transport layer).
    deploy_tier_invoked = (
        api_available and len(api_releases) == 0 and len(tag_releases) == 0
    )
    deploy_releases: list[dict[str, Any]] = []
    deploy_available = False
    deploy_error: str | None = None
    if deploy_tier_invoked:
        deploy_releases, deploy_available, deploy_error = (
            _fetch_tier3_deploy_events(gh, effective_repo_slug, logger)
        )

    # Tier selection
    chosen_tier, primary = _choose_tier(
        api_available, api_releases, tag_releases, deploy_releases
    )

    # Prerelease partition
    releases, prereleases = _partition_prereleases(primary)

    tier_availability = _build_tier_availability(
        api_available=api_available,
        api_releases=api_releases,
        api_error=api_error,
        api_pages_fetched=api_pages,
        tag_releases=tag_releases,
        deploy_releases=deploy_releases,
        deploy_available=deploy_available,
        deploy_error=deploy_error,
        repo_slug=effective_repo_slug,
        deploy_tier_invoked=deploy_tier_invoked,
    )

    rationale = _build_chosen_tier_rationale(
        chosen_tier,
        api_available,
        api_releases,
        tag_releases,
        deploy_releases,
    )

    payload = _build_payload(
        metadata=metadata,
        chosen_tier=chosen_tier,
        tier_availability=tier_availability,
        releases=releases,
        prereleases=prereleases,
        chosen_tier_rationale=rationale,
    )

    # Resolve the caller-supplied output path under workspace path
    # confinement. ``safe_output_path`` rejects any path outside the
    # workspace tree to prevent misconfigured callers from writing
    # outside the analysis workspace (the rejection raises
    # :class:`OutputPathError`, a :class:`ValueError` subclass).
    try:
        output_path = safe_output_path(args.output)
    except OutputPathError as exc:
        logger.error(
            "output_path_rejected",
            extra={"path": str(args.output), "error": str(exc)},
        )
        print(str(exc), file=sys.stderr)
        return 4

    _write_json(output_path, payload)

    logger.info(
        "script_complete",
        extra={
            "event": "script_complete",
            "chosen_tier": chosen_tier,
            "release_count": len(releases),
            "prerelease_count": len(prereleases),
            "tier1_count": len(api_releases),
            "tier1_available": api_available,
            "tier2_count": len(tag_releases),
            "tier3_invoked": deploy_tier_invoked,
            "tier3_count": len(deploy_releases),
            "output": args.output,
        },
    )
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        # Allow argparse's normal sys.exit(0) and sys.exit(2) paths.
        raise
    except BaseException as exc:  # noqa: BLE001 — top-level last resort
        # Log the unexpected exception through the structured logger
        # so the failure is visible in data/run.log.jsonl. The bare
        # ``BaseException`` catch deliberately includes
        # ``KeyboardInterrupt`` and ``SystemExit`` subclasses raised
        # outside the controlled paths above.
        try:
            _logger = get_logger(SCRIPT_NAME)
            _logger.error(
                "script_unhandled_exception",
                extra={
                    "event": "script_unhandled_exception",
                    "error_type": type(exc).__name__,
                    "error": str(exc),
                },
                exc_info=True,
            )
        except Exception:
            # Logging itself failed; fall back to stderr so the user
            # still sees the cause when invoking the script
            # interactively.
            import traceback as _tb
            _tb.print_exc()
        sys.exit(1)

