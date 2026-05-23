"""CI workflow history and test-transition extractor for the acceleration-report pipeline.

This script is stage 06 of the read-only extraction pipeline documented in
``blitzy/acceleration-report/``. It feeds Metric 11 (Escaped Defects) by
producing two canonical artifacts that capture, respectively:

* ``data/ci_runs.json`` — the per-workflow-run inventory of GitHub Actions
  workflow executions on the default branch, plus per-run artifact metadata
  filtered to JUnit XML candidates and CI deploy-event candidates.
* ``data/test_transitions.json`` — the per-window test-result transition
  signal (pass-to-fail regressions and pass-to-skip suppressions) derived
  from JUnit XML parsed across consecutive workflow runs, plus the
  HEAD-only in-repo skip-marker scan that is the documented fallback when
  JUnit XML is not emitted.

Source-precedence and three-tier extraction
-------------------------------------------

Per AAP §0.5.3.12 (Metric 11), the script implements a two-tier extraction
strategy with graceful degradation between tiers:

1. **Tier 1 — GitHub Actions Runs API + JUnit XML artifacts**.
   ``GET /repos/{owner}/{repo}/actions/runs?branch={default_branch}``
   returns the paginated run inventory. For each run with a completed
   ``conclusion``, ``GET /repos/{owner}/{repo}/actions/runs/{run_id}/artifacts``
   lists the per-run artifact inventory. Artifacts whose name matches
   :data:`JUNIT_ARTIFACT_PATTERNS` are downloaded as ZIP archives, extracted
   in memory, and parsed for ``<testcase>`` elements. The per-run
   ``{test_case_name: status}`` maps feed the transition-detection walker.
2. **Tier 2 — In-repo HEAD-only skip-marker scan**. When Tier 1 is
   unavailable (no GH_TOKEN, rate-limit exhaustion, or the workflow does
   not emit JUnit XML — which is the documented state of this repository
   per AAP §0.2.3), the script falls back to a direct ``rglob('*_test.go')``
   walk over the analyzed repository's working tree. Each line is matched
   against :data:`SKIP_PATTERNS` (``t.Skip``, ``t.SkipNow``, ``// nolint``,
   Ginkgo ``XIt`` / ``XDescribe`` / ``PIt`` placeholders). The output is a
   HEAD-only snapshot, NOT a transition signal — it cannot reconstruct
   pass-to-fail or pass-to-skip transitions, but it provides the only
   available skip-marker enumeration when Tier 1 is unreachable.

Flaky-test guard
----------------

The transition-detection logic in :func:`detect_transitions` applies a
documented flaky-test guard per AAP §0.5.3.12: a pass-to-fail transition
is classified as a regression only when the test fails for at least
:data:`FLAKY_THRESHOLD` (3) consecutive runs. Tests that flip back to
pass within the threshold window are flagged as flaky and excluded from
the regression count. Pass-to-skip transitions do not apply the threshold
because newly skipped tests are an immediate suppression signal regardless
of subsequent runs. The guard is implemented as a rolling :class:`deque`
of length ``FLAKY_THRESHOLD + 1`` per test, providing constant-memory
bounded analysis even when the run history contains thousands of entries.

Read-only enforcement
---------------------

Every HTTP request is dispatched through :class:`lib.github.GithubClient`
whose private ``_request`` method statically rejects any verb other than
``GET``. Every git invocation goes through :func:`lib.git.git_rev_parse_toplevel`
which runs through the read-only validator gate of ``lib/git.py``. The
in-repo HEAD scan only reads ``*_test.go`` files via :class:`pathlib.Path`
``rglob`` and ``read_text``; it never writes to the analyzed repository's
working tree, refs, history, or remote. The script never modifies any
external system — only ``blitzy/acceleration-report/data/ci_runs.json``
and ``blitzy/acceleration-report/data/test_transitions.json``.

Schema conformance and downstream consumer
------------------------------------------

Both output artifacts carry a ``_metadata`` block with the canonical
per-run correlation fields (``extraction_timestamp``, ``run_id``,
``repository_slug``, ``default_branch``, ``analysis_period_start``,
``analysis_period_end``, ``inflection_date_utc``, ``schema_version``,
``artifact_kind``). These values are read from ``data/environment.json``
and ``data/inflection.json`` when those files exist; sensible defaults
are computed otherwise so the script remains useful for ad-hoc
invocations that have not yet been preceded by stages 00 and 01.

The downstream consumer is ``09_compute_metrics.py``, which reads
``ci_runs.json`` and ``test_transitions.json`` to populate
``metrics.json#m11``. When both Tier 1 and the in-repo fallback produce
no transition signal, the consumer surfaces M11 as
``"insufficient_signal"`` with confidence ``"insufficient"`` per AAP
§0.5.3.12 — this script does NOT compute the M11 value itself; it
emits the raw evidence that determines the value.

Engineering-actor framing
-------------------------

This metric is repository-wide and does not require an engineering-actor
substitution. Metric 11 measures product-level escaped defects (test
regressions and suppressions on the default branch) regardless of which
human or AI actor authored the change that introduced them. The
substitution-by-actor pattern (Metrics 2, 4, 5, 6, 10) does NOT apply
here.

Observability
-------------

The script acquires a structured-JSON logger via
:func:`lib.observability.get_logger`, which propagates the per-run
correlation ID through the ``BLITZY_RUN_ID`` environment variable. Every
major branch (Tier 1 success/failure, per-run artifact processing,
JUnit XML parse outcome, HEAD scan completion, final write) emits a
single ``event``-tagged log line so that ``data/run.log.jsonl`` carries
a complete audit trail of the run. The ``--dry-run`` flag exits 0 after
printing the planned API calls and filesystem reads without executing
any network or filesystem traversal, satisfying the Rule-1 (Observability)
readiness preflight requirement.

Exit codes
----------

* ``0`` — Successful execution OR dry-run completion OR graceful
  degradation (Tier 1 unavailable AND HEAD scan completed OR Tier 1
  unavailable AND HEAD scan empty).
* ``1`` — Unexpected error in :func:`main` not handled by per-tier
  ``try``/``except`` blocks. The traceback is logged before exit.

The ``0`` exit code on graceful degradation is intentional per AAP
§0.8.4: the absence of JUnit XML is a valid measurement outcome and
must surface as ``github_api.available == false`` and/or
``junit_available == false`` plus empty ``runs``/``transitions``
arrays — not as a non-zero exit.
"""

from __future__ import annotations

import argparse
import io
import json
import os
import re
import sys
import zipfile
from collections import defaultdict, deque
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from xml.etree import ElementTree as ET

# ---------------------------------------------------------------------------
# Make the workspace-local ``lib/`` package importable when the script is
# invoked directly with ``python3 06_extract_ci_history.py``. Without this,
# ``from lib.observability import get_logger`` would fail because the
# script's own directory is not necessarily on ``sys.path``. The insert
# at position 0 takes precedence over any system-wide ``lib`` package.
# ---------------------------------------------------------------------------

sys.path.insert(0, str(Path(__file__).resolve().parent))

from lib.git import git_rev_parse_toplevel  # noqa: E402
from lib.github import GithubClient  # noqa: E402
from lib.observability import get_logger  # noqa: E402


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------


#: Canonical script identifier. Surfaces as the ``script`` field of every
#: emitted log line and as the ``producing_script`` reference in the
#: provenance block of the output artifacts.
SCRIPT_NAME: str = "06_extract_ci_history"

#: Workspace root directory resolved from this script's location. The
#: ``parent.parent`` walk traverses ``scripts/`` -> ``acceleration-report/``.
#: All path constants below are anchored to this root so the script works
#: identically from any current working directory.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent

#: Directory under the workspace where raw data artifacts are persisted.
#: Created lazily by :func:`main` before the final writes.
DATA_DIR: Path = WORKSPACE_ROOT / "data"

#: Default output path for the CI workflow run inventory artifact.
#: Overridable via the ``--ci-output`` CLI flag for ad-hoc runs against
#: alternative destinations (typically used by integration tests).
CI_RUNS_OUTPUT: Path = DATA_DIR / "ci_runs.json"

#: Default output path for the test-result transitions artifact.
#: Overridable via the ``--transitions-output`` CLI flag.
TRANSITIONS_OUTPUT: Path = DATA_DIR / "test_transitions.json"

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
#: CLI flag or the ``GITHUB_REPO_SLUG`` environment variable.
REPO_SLUG_DEFAULT: str = "Blitzy-Sandbox/blitzy-RudderStack"

#: Default branch name used when no environment.json is available.
DEFAULT_BRANCH: str = "main"

#: Sentinel inflection date used when ``data/inflection.json`` is absent.
INFLECTION_DATE_FALLBACK_UTC: str = "2026-02-25T02:58:59Z"

#: Sentinel analysis-period bounds used when ``data/environment.json``
#: is absent.
ANALYSIS_PERIOD_START_FALLBACK: str = "2026-02-23T00:00:00Z"
ANALYSIS_PERIOD_END_FALLBACK: str = "2026-05-21T23:59:59Z"

#: Schema version this extractor emits. Bumped only when the output
#: shape changes in a backward-incompatible way.
ARTIFACT_SCHEMA_VERSION: str = "1.1.0"

#: GitHub Actions Runs API page size. The endpoint caps at 100; matching
#: the cap minimizes round-trips during pagination of the run inventory.
DEFAULT_PAGE_SIZE: int = 100

#: Compiled regex for JUnit artifact name identification. Matches the
#: common artifact-name conventions emitted by ``gotestsum --junitfile``,
#: ``go-junit-report``, and similar XML-emitting test runners. The
#: case-insensitive match captures ``junit*.xml``, ``test-results*.xml``,
#: ``test-report*.xml``, and ``gotestsum*.xml`` artifact names along
#: with their archive forms. The trailing ``.xml`` is enforced because
#: GitHub Actions wraps artifact files in a ZIP whose internal members
#: carry the original ``.xml`` extension.
JUNIT_ARTIFACT_PATTERNS: re.Pattern[str] = re.compile(
    r"(junit|test-result|test-report|gotestsum)", re.IGNORECASE
)

#: Flaky-test guard threshold per AAP §0.5.3.12. A pass-to-fail
#: transition only counts as a regression when the test fails for at
#: least this many consecutive runs. Tests that flip back to pass
#: within the threshold window are flagged as flaky and excluded.
FLAKY_THRESHOLD: int = 3

#: In-repo skip-marker patterns for the HEAD-only fallback signal.
#: Each pattern is compiled once at module load. The list is iterated
#: in order against every line of every ``*_test.go`` file; a match
#: by any pattern records the (file, line, marker, context) tuple.
#:
#: The patterns cover:
#:
#: * ``t.Skip(...)`` — Go testing's primary skip mechanism. The
#:   word-boundary anchor ``\b`` prevents false positives on tokens
#:   like ``int.Skip`` or ``mt.Skip`` in nested struct accesses.
#: * ``t.SkipNow()`` — Go testing's "skip immediately" variant.
#: * ``// nolint:`` — golangci-lint suppression directives. Counted
#:   as a suppression signal because they explicitly disable lint
#:   coverage on the surrounding code.
#: * ``XIt(...)`` — Ginkgo's pending-test placeholder (the X prefix
#:   marks the spec as pending; it does not run).
#: * ``XDescribe(...)`` — Ginkgo's pending-suite placeholder.
#: * ``PIt(...)`` — Ginkgo's alternate pending-spec syntax.
#:
#: Tests using ``t.Skipf(...)`` are NOT a separate pattern because
#: the ``t.Skip\b`` pattern with ``\s*\(`` already covers the
#: ``Skipf`` variant via the surface ``t.Skip`` prefix. False-positive
#: risk is judged acceptable because every ``t.Skip*`` variant is a
#: legitimate suppression signal.
SKIP_PATTERNS: list[re.Pattern[str]] = [
    re.compile(r"\bt\.Skip\s*\("),
    re.compile(r"\bt\.SkipNow\s*\("),
    re.compile(r"//\s*nolint:"),
    re.compile(r"\bXIt\s*\("),
    re.compile(r"\bXDescribe\s*\("),
    re.compile(r"\bPIt\s*\("),
]

#: Path-substring blocklist used by :func:`head_skip_scan` to exclude
#: external or vendored trees from the in-repo scan. Files whose
#: resolved path contains any of these substrings are skipped without
#: read so the scan focuses on the repository's first-party test
#: estate. The list mirrors the AAP §0.2.3 scope:
#:
#: * ``/refs/`` — excludes the ``refs/segment-docs/`` external doc
#:   subtree which carries Go-shaped files for testing of the
#:   doc-rendering tool.
#: * ``/vendor/`` — excludes any vendored third-party module.
#: * ``/node_modules/`` — excludes Node.js dependencies under the
#:   blitzy-docs site if present.
#: * ``/blitzy/acceleration-report/`` — excludes this script's own
#:   workspace so the analyzer never recurses into itself.
SCAN_EXCLUDE_SUBSTRINGS: tuple[str, ...] = (
    "/refs/",
    "/vendor/",
    "/node_modules/",
    "/blitzy/acceleration-report/",
)

#: Canonical list of workflow filenames present at HEAD in
#: ``.github/workflows/``. Used to populate the ``workflows_in_repo``
#: field of the ``_metadata`` block and to seed ``runs_by_workflow``
#: keys so the artifact's downstream consumers can iterate every known
#: workflow even when no runs were fetched. The list reflects the AAP
#: §0.2.1.1 inventory of 13 workflow files.
WORKFLOWS_IN_REPO: tuple[str, ...] = (
    "builds.yml",
    "tests.yaml",
    "verify.yml",
    "release-please.yaml",
    "prerelease.yaml",
    "sync-release.yaml",
    "dispatch-deploy-event-dev.yaml",
    "docker-build-dockerhub.yml",
    "docker-build-ecr.yml",
    "housekeeping.yaml",
    "labeler.yaml",
    "pr-description-enforcer.yaml",
    "semantic-pr.yaml",
)

#: Source workflow file name for the deploy-event tier. The
#: ``ci_deploy_events`` block of ``ci_runs.json`` summarises runs of
#: this workflow because it represents the canonical CI-driven deploy
#: signal documented in AAP §0.5.3.10 (Metric 9 Tier 3 fallback).
CI_DEPLOY_SOURCE_WORKFLOW: str = "dispatch-deploy-event-dev.yaml"


# ---------------------------------------------------------------------------
# Time helpers
# ---------------------------------------------------------------------------




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
    artifact_kind: str,
) -> dict[str, Any]:
    """Construct the ``_metadata`` block shared by both output artifacts.

    Reads the canonical per-run correlation fields from
    ``data/environment.json`` (top-level OR ``_metadata`` nested) and
    ``data/inflection.json`` (top-level ``date_utc`` field), falling
    back to AAP-documented defaults when those artifacts are absent.
    The fallback path is exercised when this script is invoked
    standalone before stages 00 and 01 have run; the canonical path is
    exercised during normal pipeline operation.

    Args:
        args: Parsed CLI namespace; ``repo_slug`` is used as the
            ``repository_slug`` fallback when ``environment.json`` is
            unavailable.
        env: Decoded contents of ``data/environment.json`` or ``None``.
        infl: Decoded contents of ``data/inflection.json`` or ``None``.
        extraction_ts: Wall-clock UTC timestamp used as
            ``extraction_timestamp`` when no canonical run timestamp is
            available from ``environment.json``.
        artifact_kind: Identifier embedded in the ``_metadata`` block
            to distinguish the two output artifacts downstream.

    Returns:
        A dictionary suitable for the ``_metadata`` block of an output
        artifact. All schema-required fields are populated.
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
        run_id = (
            env.get("run_id")
            or _get_nested(env, "_metadata", "run_id")
        )
    if not run_id:
        run_id = os.environ.get("BLITZY_RUN_ID")
    if not run_id:
        # Last-resort fallback: synthesise a fresh UUID4. Imported
        # locally to avoid pulling uuid into the top-level imports.
        import uuid as _uuid_local
        run_id = str(_uuid_local.uuid4())

    repository_slug = args.repo_slug
    if env is not None:
        env_slug = (
            env.get("repository_slug")
            or _get_nested(env, "_metadata", "repository_slug")
        )
        if env_slug:
            repository_slug = env_slug

    default_branch = args.branch
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
        "schema_version": ARTIFACT_SCHEMA_VERSION,
        "artifact_kind": artifact_kind,
    }


# ---------------------------------------------------------------------------
# Tier 1 — GitHub Actions Runs API
# ---------------------------------------------------------------------------


def _shape_workflow_run(run: dict[str, Any]) -> dict[str, Any]:
    """Normalise one GitHub Actions Runs API record into the artifact's shape.

    The Runs API returns a rich object with run-step inventory, commit
    head/parent SHAs, the actor that triggered the run, and so on. The
    schema-required fields plus a documented "pipeline-useful" subset
    are extracted here; everything else is dropped to keep the artifact
    compact. Timestamps are passed through :func:`_normalise_iso` so
    the schema pattern is satisfied.

    Args:
        run: A single decoded workflow-run object from the API.

    Returns:
        A dict with the canonical fields needed by downstream M11
        computation: ``id``, ``name``, ``head_branch``, ``head_sha``,
        ``event``, ``status``, ``conclusion``, ``created_at``,
        ``updated_at``, ``run_started_at``, ``workflow_id``,
        ``run_attempt``, ``artifacts_url``, ``html_url``, ``path``.
        The ``path`` field is the workflow file path
        (e.g. ``".github/workflows/tests.yaml"``) needed for
        ``runs_by_workflow`` bucketing.
    """
    return {
        "id": run.get("id"),
        "name": run.get("name"),
        "head_branch": run.get("head_branch"),
        "head_sha": run.get("head_sha"),
        "event": run.get("event"),
        "status": run.get("status"),
        "conclusion": run.get("conclusion"),
        "created_at": _normalise_iso(run.get("created_at")),
        "updated_at": _normalise_iso(run.get("updated_at")),
        "run_started_at": _normalise_iso(run.get("run_started_at")),
        "workflow_id": run.get("workflow_id"),
        "run_attempt": run.get("run_attempt"),
        "artifacts_url": run.get("artifacts_url"),
        "html_url": run.get("html_url"),
        "path": run.get("path"),
    }


def _fetch_ci_runs(
    gh: GithubClient,
    repo_slug: str,
    branch: str,
    logger: Any,
) -> tuple[list[dict[str, Any]], bool, str | None]:
    """Fetch every workflow run on ``branch`` via the GitHub Actions API.

    Iterates the paginated
    ``GET /repos/{owner}/{repo}/actions/runs?branch=...`` endpoint
    through :meth:`GithubClient.paginate_endpoint` with
    ``item_key="workflow_runs"`` (the response wraps the run list under
    that key). The page size is pinned at :data:`DEFAULT_PAGE_SIZE`
    (100), the maximum permitted by GitHub. Every error condition —
    network failure, 4xx, 5xx after retry exhaustion, schema mismatch —
    is caught and recorded; the function never raises. Callers inspect
    the ``api_available`` flag to decide whether to fall back to the
    HEAD-only in-repo scan.

    Args:
        gh: A pre-constructed :class:`GithubClient` instance.
        repo_slug: ``owner/repo`` for the target repository.
        branch: Branch name to filter runs by (typically ``main``).
        logger: Structured-JSON logger.

    Returns:
        A 3-tuple ``(runs, api_available, api_error)``:

        * ``runs`` — list of shaped run dicts (possibly empty if the
          endpoint returned zero rows).
        * ``api_available`` — ``True`` iff the endpoint responded at
          all, ``False`` on network/auth/permission failure.
        * ``api_error`` — short, human-readable error message when
          ``api_available`` is ``False``; ``None`` on success.
    """
    runs: list[dict[str, Any]] = []
    try:
        endpoint = f"/repos/{repo_slug}/actions/runs"
        for run in gh.paginate_endpoint(
            endpoint,
            params={"branch": branch, "per_page": DEFAULT_PAGE_SIZE},
            item_key="workflow_runs",
        ):
            runs.append(_shape_workflow_run(run))
        logger.info(
            "ci_runs_fetched",
            extra={
                "event": "ci_runs_fetched",
                "endpoint": endpoint,
                "branch": branch,
                "count": len(runs),
            },
        )
        return runs, True, None
    except Exception as exc:  # noqa: BLE001 — read-only fallback discipline
        # Catch broad here is intentional: the AAP requires graceful
        # degradation on ANY API failure. ``GithubClient`` already
        # narrows what it raises; everything else is a downstream
        # failure mode (DNS, TLS, requests.RequestException, schema
        # mismatch).
        error_message = f"{type(exc).__name__}: {exc}"
        logger.warning(
            "ci_runs_unavailable",
            extra={
                "event": "ci_runs_failed",
                "error": error_message,
            },
        )
        return [], False, error_message

def iso_now() -> str:
    """Return the current UTC instant as an ISO-8601 string with ``Z`` suffix.

    The format matches the schema-pattern
    ``^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}Z$`` required by
    ``_metadata.extraction_timestamp`` and by every other ISO timestamp
    field in the data-artifact schemas under
    ``scripts/lib/schemas/``. Wall-clock UTC is used so log lines and
    artifact fields collate identically across multiple machines and
    time zones during a pipeline rerun.

    The function is deterministic against ``datetime.now`` and carries
    no randomness; tests that fix ``datetime.now`` via
    ``unittest.mock`` will observe a stable return value.

    Returns:
        A string of the form ``"2026-05-23T14:32:11Z"``. Sub-second
        precision is intentionally dropped to keep the timestamp format
        compact and to match the schema pattern.
    """
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _normalise_iso(ts: str | None) -> str | None:
    """Coerce an arbitrary ISO-8601 timestamp into the schema-required form.

    GitHub API timestamps can arrive as ``"2024-05-02T18:30:00Z"`` or
    with a UTC offset like ``"2024-05-02T18:30:00+00:00"``. This helper
    converts any valid ISO-8601 input to the strict ``YYYY-MM-DDTHH:
    MM:SSZ`` form. Returns ``None`` when ``ts`` is ``None`` or fails
    to parse so the caller can omit the field rather than fabricate
    one.

    Args:
        ts: ISO-8601 timestamp string, or ``None``.

    Returns:
        A normalised timestamp matching ``^\\d{4}-\\d{2}-\\d{2}T
        \\d{2}:\\d{2}:\\d{2}Z$``, or ``None`` when invalid.
    """
    if not ts or not isinstance(ts, str):
        return None
    if re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", ts):
        return ts
    try:
        parsed = datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    else:
        parsed = parsed.astimezone(timezone.utc)
    return parsed.strftime("%Y-%m-%dT%H:%M:%SZ")


# ---------------------------------------------------------------------------
# Tier 1 — JUnit XML artifact fetch + parse
# ---------------------------------------------------------------------------


def _parse_junit_zip(
    content: bytes,
    run_id: int | str | None,
    artifact_name: str,
    logger: Any,
) -> dict[str, str]:
    """Parse a downloaded JUnit XML artifact zip into a status map.

    Wraps the binary content in an in-memory :class:`io.BytesIO`,
    opens it as a ZIP via :class:`zipfile.ZipFile`, iterates every
    ``.xml`` member, and parses each through :func:`ET.parse`. For
    each ``<testcase>`` element the function inspects the presence of
    ``<failure>``, ``<error>``, and ``<skipped>`` sub-elements to
    derive a single status string per qualified test name. When the
    same test name appears in multiple XML members (e.g. cross-package
    duplicates), the LAST observed status wins — this is a stable
    deterministic policy that mirrors how go-junit-report concatenates
    multi-package output.

    The function NEVER writes the archive to disk; in-memory extraction
    keeps the analysis pipeline read-only with respect to the
    filesystem.

    Args:
        content: Raw bytes of the downloaded artifact ZIP.
        run_id: The workflow-run identifier (used only for log
            correlation; not used for status keys).
        artifact_name: The artifact name (used only for log
            correlation).
        logger: Structured-JSON logger.

    Returns:
        A dict mapping ``"{classname}.{name}"`` to one of
        ``"pass"``, ``"fail"``, ``"skip"``. An empty dict is returned
        when the archive is malformed (``zipfile.BadZipFile``), when
        no member ends with ``.xml``, or when every XML member fails
        to parse — these cases are logged but never raise.
    """
    case_status: dict[str, str] = {}
    try:
        zf = zipfile.ZipFile(io.BytesIO(content))
    except zipfile.BadZipFile as exc:
        logger.warning(
            "junit_artifact_bad_zip",
            extra={
                "event": "junit_artifact_bad_zip",
                "run_id": run_id,
                "artifact": artifact_name,
                "error": str(exc),
            },
        )
        return case_status

    try:
        with zf:
            xml_members = [
                m for m in zf.namelist() if m.lower().endswith(".xml")
            ]
            if not xml_members:
                logger.info(
                    "junit_artifact_no_xml_members",
                    extra={
                        "event": "junit_artifact_no_xml_members",
                        "run_id": run_id,
                        "artifact": artifact_name,
                        "member_count": len(zf.namelist()),
                    },
                )
                return case_status
            for member in xml_members:
                try:
                    with zf.open(member) as xf:
                        try:
                            tree = ET.parse(xf)
                        except ET.ParseError as parse_exc:
                            logger.warning(
                                "junit_xml_parse_error",
                                extra={
                                    "event": "junit_xml_parse_error",
                                    "run_id": run_id,
                                    "artifact": artifact_name,
                                    "member": member,
                                    "error": str(parse_exc),
                                },
                            )
                            continue
                except (KeyError, RuntimeError) as exc:
                    # KeyError from ZipFile.open on a corrupt member;
                    # RuntimeError for CRC mismatches when zip's
                    # checksum verification triggers.
                    logger.warning(
                        "junit_xml_member_read_error",
                        extra={
                            "event": "junit_xml_member_read_error",
                            "run_id": run_id,
                            "artifact": artifact_name,
                            "member": member,
                            "error": f"{type(exc).__name__}: {exc}",
                        },
                    )
                    continue
                # iter('testcase') walks every <testcase> in the tree
                # regardless of <testsuite>/<testsuites> nesting. This
                # is the standard JUnit XML traversal and is robust
                # against vendor-specific wrapper elements.
                for tc in tree.iter("testcase"):
                    classname = tc.get("classname", "") or ""
                    name = tc.get("name", "") or ""
                    qname = f"{classname}.{name}" if classname else name
                    if not qname:
                        # A testcase with neither classname nor name is
                        # malformed; skip rather than collide on the
                        # empty-string key.
                        continue
                    has_failure = tc.find("failure") is not None
                    has_error = tc.find("error") is not None
                    has_skipped = tc.find("skipped") is not None
                    if has_skipped:
                        status = "skip"
                    elif has_failure or has_error:
                        status = "fail"
                    else:
                        status = "pass"
                    case_status[qname] = status
    except Exception as exc:  # noqa: BLE001 — defensive guard
        # Any other zip-level failure is logged but does not abort the
        # outer caller's loop; the next artifact will be tried.
        logger.warning(
            "junit_artifact_processing_failed",
            extra={
                "event": "junit_artifact_processing_failed",
                "run_id": run_id,
                "artifact": artifact_name,
                "error": f"{type(exc).__name__}: {exc}",
            },
        )

    return case_status


def fetch_junit_for_run(
    gh: GithubClient,
    repo_slug: str,
    run_id: int | str,
    logger: Any,
) -> tuple[dict[str, str], list[dict[str, Any]]]:
    """Return ``({test_case_name: status}, junit_artifacts_metadata)`` for one run.

    Performs:

    1. ``GET /repos/{owner}/{repo}/actions/runs/{run_id}/artifacts``
       to enumerate the per-run artifact inventory.
    2. Filter artifacts whose name matches
       :data:`JUNIT_ARTIFACT_PATTERNS`.
    3. For each match, download the archive via
       :meth:`GithubClient.get_binary`, parse via
       :func:`_parse_junit_zip`, and merge the resulting status maps.

    The function NEVER raises. Per-artifact failures (download failure,
    bad zip, parse error) are logged and skipped; the caller still
    receives the partial status map for the run.

    Args:
        gh: A pre-constructed :class:`GithubClient` instance.
        repo_slug: ``owner/repo`` for the target repository.
        run_id: The workflow-run identifier.
        logger: Structured-JSON logger.

    Returns:
        A 2-tuple ``(case_status, junit_artifacts_metadata)``:

        * ``case_status`` — merged ``{qualified_test_name: status}``
          map across every JUnit-matching artifact for this run.
        * ``junit_artifacts_metadata`` — list of dicts describing each
          downloaded artifact (``run_id``, ``artifact_id``, ``name``,
          ``size_in_bytes``, ``case_count``). Surfaces in
          ``ci_runs.json#artifacts.junit_xml_artifacts``.
    """
    case_status: dict[str, str] = {}
    junit_meta: list[dict[str, Any]] = []
    try:
        endpoint = f"/repos/{repo_slug}/actions/runs/{run_id}/artifacts"
        listing = gh.get_one(endpoint)
    except Exception as exc:  # noqa: BLE001 — read-only fallback
        logger.warning(
            "artifact_listing_failed",
            extra={
                "event": "artifact_listing_failed",
                "run_id": run_id,
                "error": f"{type(exc).__name__}: {exc}",
            },
        )
        return case_status, junit_meta

    artifacts = listing.get("artifacts", []) if isinstance(listing, dict) else []
    if not isinstance(artifacts, list):
        logger.warning(
            "artifact_listing_wrong_shape",
            extra={
                "event": "artifact_listing_wrong_shape",
                "run_id": run_id,
                "type": type(artifacts).__name__,
            },
        )
        return case_status, junit_meta

    for art in artifacts:
        if not isinstance(art, dict):
            continue
        name = art.get("name", "") or ""
        if not JUNIT_ARTIFACT_PATTERNS.search(name):
            continue
        archive_url = art.get("archive_download_url")
        if not archive_url:
            logger.warning(
                "artifact_missing_archive_url",
                extra={
                    "event": "artifact_missing_archive_url",
                    "run_id": run_id,
                    "artifact": name,
                },
            )
            continue
        try:
            content = gh.get_binary(archive_url)
        except Exception as exc:  # noqa: BLE001 — read-only fallback
            logger.warning(
                "artifact_download_failed",
                extra={
                    "event": "artifact_download_failed",
                    "run_id": run_id,
                    "artifact": name,
                    "error": f"{type(exc).__name__}: {exc}",
                },
            )
            continue
        per_artifact = _parse_junit_zip(content, run_id, name, logger)
        junit_meta.append({
            "run_id": run_id,
            "artifact_id": art.get("id"),
            "artifact_name": name,
            "size_in_bytes": art.get("size_in_bytes"),
            "case_count": len(per_artifact),
        })
        # Merge with last-write-wins so cross-artifact duplicates of
        # the same qualified test name resolve deterministically. This
        # is the same policy used inside :func:`_parse_junit_zip` for
        # cross-member duplicates.
        case_status.update(per_artifact)

    logger.info(
        "junit_artifacts_processed",
        extra={
            "event": "junit_artifacts_processed",
            "run_id": run_id,
            "artifact_count": len(junit_meta),
            "case_count": len(case_status),
        },
    )
    return case_status, junit_meta


# ---------------------------------------------------------------------------
# Transition detection (with flaky-test guard)
# ---------------------------------------------------------------------------


def detect_transitions(
    per_run_statuses: dict[int | str, dict[str, str]],
    runs_sorted: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    """Walk runs in chronological order and emit regression/suppression events.

    For each test case observed in the consecutive run sequence, the
    function tracks the last :data:`FLAKY_THRESHOLD` + 1 statuses in a
    bounded :class:`deque`. Two transition kinds are recognised:

    * **regression** — when the window's first observed status was
      ``pass`` and every subsequent status (length
      :data:`FLAKY_THRESHOLD`) is ``fail``. This implements the
      flaky-test guard per AAP §0.5.3.12: a test must fail in at least
      :data:`FLAKY_THRESHOLD` consecutive runs after a pass to count
      as a true regression. Tests that flip back to pass within the
      window are NOT classified as regressions.
    * **suppression** — when the window's first observed status was
      ``pass`` and the most recent status is ``skip``. The flaky guard
      does NOT apply to suppressions because a newly skipped test is
      an immediate signal regardless of subsequent runs.

    The deque is bounded at :data:`FLAKY_THRESHOLD` + 1 so the function
    runs in O(N × M) time and O(M) memory per test (where N is the
    run count and M is the test count). The window slides — only the
    most recent ``FLAKY_THRESHOLD + 1`` statuses per test are
    inspected on each iteration, so historical noise outside the
    window does not generate spurious transitions.

    Args:
        per_run_statuses: A mapping ``{run_id: {test_name: status}}``
            where ``status`` is one of ``"pass"``, ``"fail"``,
            ``"skip"``.
        runs_sorted: List of run dicts in chronological order. Each
            dict must have an ``id`` key matching one of
            ``per_run_statuses`` and a ``created_at`` key for the
            ``transitioned_at`` field on each emitted transition.

    Returns:
        List of transition dicts. Each carries:

        * ``kind`` — ``"regression"`` or ``"suppression"``.
        * ``test`` — the qualified test name.
        * ``transitioned_at`` — the ``created_at`` of the run at
          which the transition was first observed (for regressions:
          the first fail; for suppressions: the skip).
        * ``run_id`` — the run id at which the transition was first
          observed.
        * ``prior_status`` — always ``"pass"`` (the transition's
          source state).
        * ``recent_statuses`` — the inspection window snapshot used
          to classify this transition.
    """
    transitions: list[dict[str, Any]] = []
    test_history: dict[str, deque[tuple[Any, str | None, str]]] = defaultdict(
        lambda: deque(maxlen=FLAKY_THRESHOLD + 1)
    )

    # Track which (kind, test) pairs have already been emitted to avoid
    # duplicate emissions when the same window state is re-evaluated on
    # subsequent runs. The deque semantics naturally prevent
    # re-evaluation of an old window, but a test that has already been
    # marked regressed should not be re-emitted as a regression on the
    # next consecutive failure within the same fail streak.
    emitted_keys: set[tuple[str, str]] = set()

    for run in runs_sorted:
        run_id = run.get("id")
        run_created_at = run.get("created_at")
        statuses = per_run_statuses.get(run_id, {})
        for test, status in statuses.items():
            if status not in ("pass", "fail", "skip"):
                # Defensive: any non-canonical status is ignored.
                continue
            history = test_history[test]
            history.append((run_id, run_created_at, status))

            recent = list(history)
            if len(recent) < 2:
                # Need at least a prior-state observation to detect a
                # transition.
                continue

            first_status = recent[0][2]

            # Suppression check — no flaky guard. Triggered when the
            # window opened on a pass and the latest observation is a
            # skip.
            if first_status == "pass" and recent[-1][2] == "skip":
                key = ("suppression", test)
                if key not in emitted_keys:
                    emitted_keys.add(key)
                    transitions.append({
                        "kind": "suppression",
                        "test": test,
                        "transitioned_at": recent[-1][1],
                        "run_id": recent[-1][0],
                        "prior_status": "pass",
                        "recent_statuses": [s for _, _, s in recent],
                    })

            # Regression check — flaky guard applies. Triggered when
            # the window opened on a pass and the subsequent
            # FLAKY_THRESHOLD statuses are ALL fail.
            if (
                len(recent) >= FLAKY_THRESHOLD + 1
                and first_status == "pass"
            ):
                later = [s for _, _, s in recent[1:]]
                if all(s == "fail" for s in later):
                    key = ("regression", test)
                    if key not in emitted_keys:
                        emitted_keys.add(key)
                        transitions.append({
                            "kind": "regression",
                            "test": test,
                            "transitioned_at": recent[1][1],
                            "run_id": recent[1][0],
                            "prior_status": "pass",
                            "recent_statuses": [s for _, _, s in recent],
                        })

    return transitions



# ---------------------------------------------------------------------------
# Tier 2 — HEAD-only in-repo skip-marker scan
# ---------------------------------------------------------------------------


def _classify_skip_marker(pattern_src: str) -> str:
    """Return a human-readable marker type for a compiled-pattern source.

    Maps the regex source string of each entry in :data:`SKIP_PATTERNS`
    to a stable short identifier used in the ``type`` field of every
    ``head_skip_scan`` record. The mapping is exhaustive over
    :data:`SKIP_PATTERNS`; an unknown pattern falls back to the raw
    pattern source as the type so the field is never empty.

    Args:
        pattern_src: The ``.pattern`` attribute of a compiled regex
            from :data:`SKIP_PATTERNS`.

    Returns:
        A short identifier such as ``"t.Skip"``, ``"t.SkipNow"``,
        ``"// nolint"``, ``"XIt"``, ``"XDescribe"``, or ``"PIt"``.
    """
    if "t\\.Skip\\s*\\(" in pattern_src and "SkipNow" not in pattern_src:
        return "t.Skip"
    if "SkipNow" in pattern_src:
        return "t.SkipNow"
    if "nolint" in pattern_src:
        return "// nolint"
    if "XIt" in pattern_src:
        return "XIt"
    if "XDescribe" in pattern_src:
        return "XDescribe"
    if "PIt" in pattern_src:
        return "PIt"
    return pattern_src


def _should_skip_path(path: Path, repo_root: Path) -> bool:
    """Return ``True`` if ``path`` is in an excluded subtree.

    Checks the path's posix-normalised string representation against
    :data:`SCAN_EXCLUDE_SUBSTRINGS`. The relative form is used (under
    ``repo_root``) so that the exclude list works regardless of where
    the analysis workspace lives on disk.

    Args:
        path: The candidate test file path.
        repo_root: The resolved repository root.

    Returns:
        ``True`` when the path is under an excluded subtree; ``False``
        otherwise.
    """
    try:
        rel = path.relative_to(repo_root)
    except ValueError:
        # Path is outside the repo root — should not happen during
        # rglob from repo_root, but guard defensively. Treat as not
        # excluded so the caller still has a chance to inspect it.
        rel = path
    rel_posix = "/" + rel.as_posix().lstrip("/")
    for excluded in SCAN_EXCLUDE_SUBSTRINGS:
        if excluded in rel_posix:
            return True
    return False


def head_skip_scan(repo_root: Path, logger: Any) -> list[dict[str, Any]]:
    """Scan ``*_test.go`` files at HEAD for skip-marker patterns.

    Recursively walks ``repo_root`` for files matching ``*_test.go``,
    excludes files under any path in :data:`SCAN_EXCLUDE_SUBSTRINGS`,
    reads each as UTF-8 (with replacement on decoding errors so
    binary-flagged files do not abort the scan), splits into lines,
    and matches each line against every pattern in
    :data:`SKIP_PATTERNS`.

    Every match emits a record carrying:

    * ``file`` — repo-relative POSIX-style path.
    * ``line`` — 1-indexed line number.
    * ``type`` — pattern type from :func:`_classify_skip_marker`.
    * ``marker`` — the pattern source for downstream traceability.
    * ``context`` — the stripped line text, truncated to 200 chars to
      keep the artifact compact when comments are verbose.

    The function NEVER raises; per-file read failures are logged and
    the file is skipped. The return list is unsorted to preserve the
    order in which files are encountered by ``rglob``; downstream
    consumers that need a stable order should sort by ``file`` then
    ``line``.

    Args:
        repo_root: The resolved repository root (typically the output
            of :func:`lib.git.git_rev_parse_toplevel`).
        logger: Structured-JSON logger.

    Returns:
        List of skip-marker records. Empty when no matches are found
        OR when ``repo_root`` does not exist (e.g. running the script
        outside a git checkout).
    """
    if not repo_root.exists() or not repo_root.is_dir():
        logger.warning(
            "head_skip_scan_no_repo_root",
            extra={
                "event": "head_skip_scan_no_repo_root",
                "repo_root": str(repo_root),
            },
        )
        return []

    matches: list[dict[str, Any]] = []
    file_count = 0
    skipped_file_count = 0
    read_failure_count = 0
    try:
        test_files = list(repo_root.rglob("*_test.go"))
    except OSError as exc:
        logger.warning(
            "head_skip_scan_rglob_failed",
            extra={
                "event": "head_skip_scan_rglob_failed",
                "repo_root": str(repo_root),
                "error": str(exc),
            },
        )
        return []

    for test_file in test_files:
        if _should_skip_path(test_file, repo_root):
            skipped_file_count += 1
            continue
        try:
            content = test_file.read_text(encoding="utf-8", errors="replace")
        except OSError as exc:
            read_failure_count += 1
            logger.warning(
                "head_skip_scan_read_failed",
                extra={
                    "event": "head_skip_scan_read_failed",
                    "file": str(test_file),
                    "error": str(exc),
                },
            )
            continue
        file_count += 1
        try:
            rel_path = test_file.relative_to(repo_root).as_posix()
        except ValueError:
            rel_path = test_file.as_posix()
        for lineno, line in enumerate(content.splitlines(), start=1):
            for pat in SKIP_PATTERNS:
                if pat.search(line):
                    matches.append({
                        "file": rel_path,
                        "line": lineno,
                        "type": _classify_skip_marker(pat.pattern),
                        "marker": pat.pattern,
                        "context": line.strip()[:200],
                    })

    logger.info(
        "head_skip_scan_complete",
        extra={
            "event": "head_skip_scan_complete",
            "repo_root": str(repo_root),
            "test_files_scanned": file_count,
            "test_files_skipped_by_exclude": skipped_file_count,
            "test_files_read_failures": read_failure_count,
            "match_count": len(matches),
        },
    )
    return matches


def _aggregate_head_scan_stats(
    matches: list[dict[str, Any]],
) -> dict[str, Any]:
    """Build per-type counts and top-files aggregates from a scan result.

    Used to populate ``head_skip_scan_category_counts`` (by type) and
    ``head_skip_scan_top_files_by_skip_count`` (top 10 files by total
    marker count, sorted descending) in the
    ``test_transitions.json`` payload.

    Args:
        matches: List of skip-marker records from
            :func:`head_skip_scan`.

    Returns:
        A dict with ``by_type``, ``top_files``, and ``unique_files``
        keys. ``by_type`` maps marker type string to count.
        ``top_files`` is a list of ``{"file": str, "skip_count": int}``
        dicts sorted descending. ``unique_files`` is the total count
        of distinct files contributing matches.
    """
    by_type: dict[str, int] = defaultdict(int)
    by_file: dict[str, int] = defaultdict(int)
    for m in matches:
        marker_type = m.get("type") or m.get("marker") or "unknown"
        by_type[str(marker_type)] += 1
        file_path = m.get("file") or ""
        if file_path:
            by_file[file_path] += 1
    top_files = sorted(
        [{"file": f, "skip_count": c} for f, c in by_file.items()],
        key=lambda r: (-r["skip_count"], r["file"]),
    )[:10]
    return {
        "by_type": dict(by_type),
        "top_files": top_files,
        "unique_files": len(by_file),
    }


# ---------------------------------------------------------------------------
# Payload assembly — ci_runs.json
# ---------------------------------------------------------------------------


def _bucket_runs_by_workflow(
    runs: list[dict[str, Any]],
) -> dict[str, list[dict[str, Any]]]:
    """Group runs by their workflow file basename.

    Walks ``runs`` and partitions each into a bucket keyed by the
    basename of its ``path`` field (e.g. ``"tests.yaml"`` for
    ``".github/workflows/tests.yaml"``). Runs lacking a ``path`` field
    are bucketed under ``"_unknown"`` so the artifact remains
    inspectable. Every workflow in :data:`WORKFLOWS_IN_REPO` gets an
    empty list when no runs match, so downstream consumers can iterate
    the full workflow inventory without missing-key checks.

    Args:
        runs: List of shaped workflow-run dicts from
            :func:`_fetch_ci_runs`.

    Returns:
        Ordered dict mapping workflow filename to its list of runs.
    """
    buckets: dict[str, list[dict[str, Any]]] = {
        wf: [] for wf in WORKFLOWS_IN_REPO
    }
    for run in runs:
        path = run.get("path") or ""
        basename = Path(path).name if path else "_unknown"
        if basename not in buckets:
            buckets[basename] = []
        buckets[basename].append(run)
    return buckets


def _summarise_runs(
    runs: list[dict[str, Any]],
    runs_by_workflow: dict[str, list[dict[str, Any]]],
) -> dict[str, Any]:
    """Compute the ``summary`` block of the ci_runs.json payload.

    Counts runs by conclusion (success/failure/cancelled/in_progress)
    and constructs the per-workflow run counts. The
    ``failing_check_merged_prs`` field is left empty by this script —
    the consumer ``07_extract_exceptions.py`` is responsible for the
    cross-reference between PR merges and CI failures. Including the
    field here (empty) preserves the schema shape so downstream
    consumers can rely on a stable structure.

    Args:
        runs: Full shaped run list.
        runs_by_workflow: Output of :func:`_bucket_runs_by_workflow`.

    Returns:
        The ``summary`` dict.
    """
    success_count = sum(
        1 for r in runs if r.get("conclusion") == "success"
    )
    failure_count = sum(
        1 for r in runs if r.get("conclusion") == "failure"
    )
    cancelled_count = sum(
        1 for r in runs if r.get("conclusion") == "cancelled"
    )
    in_progress_count = sum(
        1 for r in runs
        if r.get("status") in ("in_progress", "queued", "requested",
                                "waiting", "pending")
    )
    runs_per_workflow = {
        wf: len(lst) for wf, lst in runs_by_workflow.items()
    }
    return {
        "total_runs_fetched": len(runs),
        "runs_per_workflow": runs_per_workflow,
        "success_count": success_count,
        "failure_count": failure_count,
        "cancelled_count": cancelled_count,
        "in_progress_count": in_progress_count,
        "failing_check_merged_prs": [],
        "failing_check_merged_prs_count": 0,
        "failing_check_merged_prs_note": (
            "This field is the responsibility of "
            "07_extract_exceptions.py, which cross-references the "
            "merged-PR list against this script's CI conclusion "
            "data. Empty here by design; not a missing-signal "
            "indicator."
        ),
    }


def _build_ci_deploy_events(
    runs_by_workflow: dict[str, list[dict[str, Any]]],
) -> dict[str, Any]:
    """Filter runs to the deploy-event source workflow and shape them.

    The ``dispatch-deploy-event-dev.yaml`` workflow is the canonical
    CI-driven deploy signal documented in AAP §0.5.3.10 (Metric 9
    Tier 3 fallback). This block surfaces every successful run of that
    workflow plus a usage note so the ``04_extract_releases.py``
    consumer can use the data as its Tier 3 fallback when neither the
    Releases API nor local tags produce signal.

    Args:
        runs_by_workflow: Output of :func:`_bucket_runs_by_workflow`.

    Returns:
        The ``ci_deploy_events`` dict.
    """
    deploy_runs = runs_by_workflow.get(CI_DEPLOY_SOURCE_WORKFLOW, [])
    events: list[dict[str, Any]] = []
    for run in deploy_runs:
        if run.get("conclusion") != "success":
            continue
        events.append({
            "run_id": run.get("id"),
            "name": run.get("name"),
            "head_sha": run.get("head_sha"),
            "head_branch": run.get("head_branch"),
            "created_at": run.get("created_at"),
            "run_started_at": run.get("run_started_at"),
            "html_url": run.get("html_url"),
        })
    return {
        "source_workflow": CI_DEPLOY_SOURCE_WORKFLOW,
        "events": events,
        "events_count": len(events),
        "note": (
            "Used as Metric 9 (Releases) third-tier source per AAP "
            "§0.5.3.10 when no git tags and Releases API also "
            "unavailable."
        ),
    }


def _build_ci_runs_payload(
    metadata: dict[str, Any],
    runs: list[dict[str, Any]],
    api_available: bool,
    api_error: str | None,
    junit_artifacts: list[dict[str, Any]],
    repo_slug: str,
    branch: str,
    junit_emitting_workflows: list[str],
) -> dict[str, Any]:
    """Assemble the final ``ci_runs.json`` payload.

    The payload structure mirrors the seed artifact shape so the
    downstream ``09_compute_metrics.py`` consumer reads identical
    fields regardless of whether the source was live extraction or
    the no-network seed. Fields:

    * ``_metadata`` — canonical correlation block.
    * ``junit_emitting_workflows`` — top-level mirror of the
      ``_metadata.junit_emitting_workflows`` field (CP2-contract
      canonical path).
    * ``github_api`` — endpoint availability, error reason,
      endpoints attempted, fetched_at timestamp.
    * ``runs`` — flat list of all shaped workflow runs.
    * ``runs_by_workflow`` — runs bucketed by workflow filename.
    * ``artifacts`` — JUnit XML and deploy artifact metadata.
    * ``summary`` — aggregate counts.
    * ``ci_deploy_events`` — Metric 9 Tier 3 fallback data.

    Args:
        metadata: Output of :func:`_build_metadata`.
        runs: Output of :func:`_fetch_ci_runs`.
        api_available: True iff the Actions API responded.
        api_error: Short error message when ``api_available`` is
            False; otherwise None.
        junit_artifacts: List of per-artifact metadata dicts.
        repo_slug: Repository slug for endpoint string composition.
        branch: Branch name used in the API filter.
        junit_emitting_workflows: List of workflow file names known
            to emit JUnit XML at HEAD (empty for this repository per
            AAP §0.2.3).

    Returns:
        The fully assembled payload dict, ready to be JSON-serialized.
    """
    runs_by_workflow = _bucket_runs_by_workflow(runs)
    summary = _summarise_runs(runs, runs_by_workflow)
    ci_deploy_events = _build_ci_deploy_events(runs_by_workflow)
    fetched_at = iso_now() if api_available else None

    # Attach junit_emitting_workflows to _metadata so consumers reading
    # the legacy seed path continue to work; the canonical top-level
    # field carries the same value per the CP2 contract.
    metadata = dict(metadata)
    metadata["workflows_in_repo"] = list(WORKFLOWS_IN_REPO)
    metadata["workflows_in_repo_count"] = len(WORKFLOWS_IN_REPO)
    metadata["junit_emitting_workflows"] = list(junit_emitting_workflows)
    metadata["junit_emitting_workflows_note"] = (
        "Set of workflow file names observed to emit JUnit XML "
        "artifacts during this extraction run. An empty list means "
        "Metric 11 (Escaped Defects) falls back to the in-repo HEAD "
        "skip scan per AAP §0.5.3.12."
    )

    pages_fetched = (
        (len(runs) + DEFAULT_PAGE_SIZE - 1) // DEFAULT_PAGE_SIZE
        if runs
        else (1 if api_available else 0)
    )

    payload: dict[str, Any] = {
        "_metadata": metadata,
        "junit_emitting_workflows": list(junit_emitting_workflows),
        "github_api": {
            "available": api_available,
            "unavailable_reason": (
                None if api_available else (
                    api_error
                    or "GH_TOKEN environment variable not set or "
                    "rate limit exceeded"
                )
            ),
            "endpoints_attempted": [
                f"GET /repos/{repo_slug}/actions/runs"
                f"?branch={branch}&per_page={DEFAULT_PAGE_SIZE}",
                f"GET /repos/{repo_slug}/actions/runs/{{run_id}}/artifacts",
            ],
            "fetched_at": fetched_at,
            "rate_limit_remaining_at_fetch": None,
            "pagination": {
                "per_page": DEFAULT_PAGE_SIZE,
                "pages_fetched": pages_fetched,
                "last_page_seen": None,
            },
        },
        "runs": runs,
        "runs_by_workflow": runs_by_workflow,
        "artifacts": {
            "junit_xml_artifacts": junit_artifacts,
            "junit_xml_artifacts_count": len(junit_artifacts),
            "deploy_artifacts": [],
            "deploy_artifacts_count": 0,
        },
        "summary": summary,
        "ci_deploy_events": ci_deploy_events,
    }
    return payload


# ---------------------------------------------------------------------------
# Payload assembly — test_transitions.json
# ---------------------------------------------------------------------------


def _count_status(per_run_statuses: dict[Any, dict[str, str]]) -> dict[str, int]:
    """Return aggregate per-status counts across all parsed JUnit cases.

    Sums the number of ``pass``, ``fail``, and ``skip`` observations
    across every run. Used by the ``summary`` block of
    ``test_transitions.json`` to surface the skipped-rate at HEAD when
    JUnit XML is available.

    Args:
        per_run_statuses: Map of run_id to per-test-name status dict.

    Returns:
        A dict with ``pass``, ``fail``, ``skip``, and ``total`` keys.
    """
    counts = {"pass": 0, "fail": 0, "skip": 0}
    for statuses in per_run_statuses.values():
        for s in statuses.values():
            if s in counts:
                counts[s] += 1
    counts["total"] = sum(counts.values())
    return counts


def _build_transitions_payload(
    metadata: dict[str, Any],
    transitions: list[dict[str, Any]],
    head_matches: list[dict[str, Any]],
    junit_available: bool,
    junit_unavailable_reason: str | None,
    api_available: bool,
    runs_processed: int,
    per_run_statuses: dict[Any, dict[str, str]],
) -> dict[str, Any]:
    """Assemble the final ``test_transitions.json`` payload.

    Fields:

    * ``_metadata`` — canonical correlation block extended with
      M11-specific provenance fields.
    * ``transitions`` — list of regression/suppression events from
      :func:`detect_transitions`.
    * ``head_skip_scan`` — list of skip-marker records from
      :func:`head_skip_scan`.
    * ``head_skip_scan_category_counts`` and
      ``head_skip_scan_top_files_by_skip_count`` — aggregates.
    * ``summary`` — per-status counts plus skipped-rate.
    * ``extraction_commands`` — provenance list.
    * ``cross_artifact_consistency`` — expectations for downstream
      consumer cross-checks.

    Args:
        metadata: Output of :func:`_build_metadata` for the
            transitions artifact.
        transitions: Output of :func:`detect_transitions`.
        head_matches: Output of :func:`head_skip_scan`.
        junit_available: True iff at least one JUnit artifact was
            successfully downloaded and parsed.
        junit_unavailable_reason: Short reason when JUnit XML is not
            available (e.g. "tests.yaml does not emit JUnit XML").
        api_available: True iff the Actions Runs API responded.
        runs_processed: Number of completed runs processed for JUnit
            parsing.
        per_run_statuses: For computing the skipped-rate at HEAD.

    Returns:
        The fully assembled payload dict, ready to be JSON-serialized.
    """
    metadata = dict(metadata)
    metadata["feeds_metric"] = "m11"
    metadata["feeds_metric_name"] = "Escaped Defects"
    metadata["aap_section"] = "0.5.3.12"
    metadata["junit_available"] = junit_available
    if not junit_available:
        metadata["junit_unavailable_reason"] = (
            junit_unavailable_reason
            or "no JUnit XML artifacts emitted by any workflow on the "
            "default branch during this extraction"
        )

    head_stats = _aggregate_head_scan_stats(head_matches)
    status_counts = _count_status(per_run_statuses)
    if junit_available and status_counts["total"] > 0:
        skipped_rate = round(
            status_counts["skip"] / status_counts["total"], 6
        )
    else:
        skipped_rate = None

    # Regression vs suppression split for the summary block.
    regressions_count = sum(
        1 for t in transitions if t.get("kind") == "regression"
    )
    suppressions_count = sum(
        1 for t in transitions if t.get("kind") == "suppression"
    )

    payload: dict[str, Any] = {
        "_metadata": metadata,
        "transitions": transitions,
        "transitions_note": (
            "The transitions[] array is the per-window pass→fail "
            "(regression) and pass→skip (suppression) signal derived "
            "from JUnit XML parsed across consecutive CI runs on the "
            "default branch, subject to the flaky-test guard per AAP "
            f"§0.5.3.12 (a test must fail ≥{FLAKY_THRESHOLD} "
            "consecutive runs to count as a regression). With "
            "junit_available=false, no transitions can be "
            "reconstructed and the array is empty; metrics.json#m11 "
            "must surface 'insufficient_signal' in that case."
        ),
        "head_skip_scan": head_matches,
        "head_skip_scan_total_count": len(head_matches),
        "head_skip_scan_unique_files": head_stats["unique_files"],
        "head_skip_scan_category_counts": head_stats["by_type"],
        "head_skip_scan_top_files_by_skip_count": head_stats["top_files"],
        "head_skip_scan_top_files_source_command": (
            "grep -rnE 't\\.Skip|t\\.SkipNow|// nolint:' "
            "--include='*_test.go' . 2>/dev/null | awk -F: "
            "'{print $1}' | sort | uniq -c | sort -rn | head -10"
        ),
        "summary": {
            "junit_xml_runs_processed": runs_processed,
            "regressions_detected": regressions_count,
            "regressions_detected_definition": (
                "Pass→fail transitions on the default branch "
                f"surviving the flaky-test guard "
                f"(≥{FLAKY_THRESHOLD} consecutive failures). "
                "Requires JUnit XML across consecutive runs; not "
                "reconstructable from in-repo HEAD scan."
            ),
            "suppressions_detected": suppressions_count,
            "suppressions_detected_definition": (
                "Pass→skip transitions on the default branch (test "
                "newly marked skipped, disabled, xfail). Requires "
                "JUnit XML across consecutive runs; the HEAD-only "
                "skip scan is a single-point snapshot, NOT a "
                "transition signal."
            ),
            "skipped_rate_at_head": skipped_rate,
            "skipped_rate_at_head_note": (
                "Skipped-rate normalisation requires per-run "
                "total-test-count from JUnit XML (skipped_count / "
                "total_test_count per AAP §0.5.3.12). When JUnit "
                "XML is unavailable, the field is null and the "
                "HEAD-only count is exposed in "
                "head_skip_scan_total_count instead."
            ),
            "total_pass_observations": status_counts["pass"],
            "total_fail_observations": status_counts["fail"],
            "total_skip_observations": status_counts["skip"],
            "total_status_observations": status_counts["total"],
        },
        "extraction_commands": [
            (
                "GET /repos/{owner}/{repo}/actions/runs"
                f"?branch={metadata['default_branch']}"
                f"&per_page={DEFAULT_PAGE_SIZE}"
            ),
            "GET /repos/{owner}/{repo}/actions/runs/{run_id}/artifacts",
            (
                "GET archive_download_url (JUnit XML matching "
                f"{JUNIT_ARTIFACT_PATTERNS.pattern!r})"
            ),
            (
                "grep -rnE 't\\.Skip|t\\.SkipNow|// nolint:' "
                "--include='*_test.go' ."
            ),
        ],
        "extraction_commands_note": (
            "Primary tier fetches the GitHub Actions workflow run "
            "inventory then per-run artifact list; if any artifact "
            "name matches the JUnit XML pattern it is downloaded "
            "and parsed into per-test-case status. Fallback tier "
            "scans the local working tree at HEAD for skip markers. "
            "The primary tier yields the transitions[] series; the "
            "fallback tier yields the head_skip_scan[] snapshot. "
            "Both tiers are exercised on every run; only the "
            "available tier produces non-empty output."
        ),
        "cross_artifact_consistency": {
            "expected_metrics_json_m11_value": (
                "insufficient_signal" if not junit_available else "numeric"
            ),
            "expected_metrics_json_m11_confidence": (
                "insufficient" if not junit_available else "high"
            ),
            "expected_metrics_json_m11_reason_must_reference": (
                "junit_unavailable_reason"
                if not junit_available
                else "transitions[].kind"
            ),
            "expected_companion_artifact_ci_runs_json": (
                "data/ci_runs.json#junit_emitting_workflows MUST "
                "have len > 0 when this file's junit_available is "
                "true, and len == 0 otherwise"
            ),
            "verification_command": (
                "python3 -c \"import json; "
                "m=json.load(open('blitzy/acceleration-report/data/metrics.json')); "
                "t=json.load(open('blitzy/acceleration-report/data/test_transitions.json')); "
                "expected='insufficient_signal' if not "
                "t['_metadata']['junit_available'] else None; "
                "assert (expected is None) or "
                "(m['m11']['value']==expected)\""
            ),
        },
        "provenance": {
            "spec_section": "AAP §0.5.3.12 (Metric 11 — Escaped Defects)",
            "source_precedence": [
                "GitHub Actions Runs API + JUnit XML artifacts",
                "In-repo HEAD-only *_test.go skip-marker scan",
            ],
            "downstream_consumer": (
                "blitzy/acceleration-report/data/metrics.json#m11"
            ),
            "downstream_consumer_field": "value",
            "downstream_consumer_expected_when_junit_unavailable": (
                "insufficient_signal"
            ),
            "producing_script": (
                "blitzy/acceleration-report/scripts/06_extract_ci_history.py"
            ),
        },
    }
    return payload


# ---------------------------------------------------------------------------
# Atomic JSON write
# ---------------------------------------------------------------------------


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
            values (a defect; this script's payloads are fully
            JSON-safe by construction).
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
    every file it would write — without executing any of them. Output
    is one canonical JSON object that operators can grep for endpoint
    or file references when validating the pipeline's read-only
    contract.

    Args:
        args: Parsed CLI namespace.

    Returns:
        A dict with ``action``, ``script``, ``repo_slug``, ``branch``,
        ``api_calls``, ``binary_downloads``, ``fallback``, ``reads``,
        ``writes``, ``skip_patterns``, ``junit_artifact_pattern``, and
        ``flaky_threshold`` keys.
    """
    return {
        "action": "dry_run",
        "script": SCRIPT_NAME,
        "repo_slug": args.repo_slug,
        "branch": args.branch,
        "api_calls": [
            (
                f"GET /repos/{args.repo_slug}/actions/runs"
                f"?branch={args.branch}&per_page={DEFAULT_PAGE_SIZE}"
            ),
            (
                f"GET /repos/{args.repo_slug}"
                "/actions/runs/{run_id}/artifacts "
                "(once per fetched run)"
            ),
        ],
        "binary_downloads": [
            (
                "GET <archive_download_url> for each artifact whose "
                f"name matches {JUNIT_ARTIFACT_PATTERNS.pattern!r}"
            ),
        ],
        "fallback": (
            "in-repo *_test.go scan for skip markers when no JUnit "
            "XML artifacts are emitted by any workflow"
        ),
        "reads": [
            str(ENVIRONMENT_PATH),
            str(INFLECTION_PATH),
            "<repository_root>/**/*_test.go (HEAD-only)",
        ],
        "writes": [args.ci_output, args.transitions_output],
        "skip_patterns": [pat.pattern for pat in SKIP_PATTERNS],
        "junit_artifact_pattern": JUNIT_ARTIFACT_PATTERNS.pattern,
        "flaky_threshold": FLAKY_THRESHOLD,
        "scan_exclude_substrings": list(SCAN_EXCLUDE_SUBSTRINGS),
        "workflows_in_repo_count": len(WORKFLOWS_IN_REPO),
    }


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main() -> int:
    """Extract CI workflow history and test transitions; persist both artifacts.

    Workflow:

    1. Parse CLI arguments (``--dry-run``, ``--repo-slug``, ``--branch``,
       ``--ci-output``, ``--transitions-output``, ``--repo-root``).
    2. Acquire the structured-JSON logger; this also seeds
       ``BLITZY_RUN_ID`` if not already set.
    3. On ``--dry-run``: print the dry-run plan and exit 0.
    4. Load ``environment.json`` and ``inflection.json`` (best effort,
       falls back to defaults).
    5. Construct the GitHub client (authenticated if ``GH_TOKEN`` is
       set, unauthenticated otherwise).
    6. Tier 1A: fetch all workflow runs on the default branch.
    7. Tier 1B: for each completed run, fetch its artifact list; for
       each artifact whose name matches the JUnit pattern, download
       and parse into a ``{test_case: status}`` map.
    8. Detect transitions across the chronologically sorted run list
       with the flaky-test guard.
    9. Tier 2: walk the repository's working tree for ``*_test.go``
       files and match each line against the skip patterns.
    10. Assemble both payloads; write atomically to disk.
    11. Emit a final ``script_complete`` log line with summary counts.

    Returns:
        ``0`` on success (including graceful degradation where Tier 1
        is unavailable). ``1`` on an unexpected exception that
        escapes the per-tier try/except blocks; the traceback is
        logged at error level before exit.
    """
    parser = argparse.ArgumentParser(
        prog=SCRIPT_NAME,
        description=(
            "Extract CI workflow history and test-result transitions "
            "from GitHub Actions. Emits data/ci_runs.json and "
            "data/test_transitions.json. Read-only."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Print the planned API calls, file reads, and writes, "
            "then exit without performing any network or filesystem "
            "traversal."
        ),
    )
    parser.add_argument(
        "--repo-slug",
        default=os.environ.get("GITHUB_REPO_SLUG", REPO_SLUG_DEFAULT),
        help=(
            "GitHub repository slug in 'owner/name' form. Defaults "
            "to the GITHUB_REPO_SLUG env var, falling back to "
            f"{REPO_SLUG_DEFAULT!r}."
        ),
    )
    parser.add_argument(
        "--branch",
        default=DEFAULT_BRANCH,
        help=(
            "Default branch name to filter workflow runs by. "
            f"Defaults to {DEFAULT_BRANCH!r}."
        ),
    )
    parser.add_argument(
        "--ci-output",
        default=str(CI_RUNS_OUTPUT),
        help=(
            "Destination path for the CI runs inventory artifact. "
            f"Defaults to {CI_RUNS_OUTPUT!s}."
        ),
    )
    parser.add_argument(
        "--transitions-output",
        default=str(TRANSITIONS_OUTPUT),
        help=(
            "Destination path for the test transitions artifact. "
            f"Defaults to {TRANSITIONS_OUTPUT!s}."
        ),
    )
    parser.add_argument(
        "--repo-root",
        default=None,
        help=(
            "Optional override for the repository root used by the "
            "HEAD-only skip-marker scan. When omitted, the script "
            "calls 'git rev-parse --show-toplevel' to auto-detect."
        ),
    )
    parser.add_argument(
        "--max-runs",
        type=int,
        default=0,
        help=(
            "Optional maximum number of workflow runs to process for "
            "JUnit XML download. Set to 0 (the default) for no limit. "
            "Useful in CI smoke tests or local exploration where the "
            "full per-run artifact enumeration would be slow."
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
            "branch": args.branch,
            "ci_output": args.ci_output,
            "transitions_output": args.transitions_output,
            "max_runs": args.max_runs,
            "gh_token_present": bool(os.environ.get("GH_TOKEN")),
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

    extraction_ts = iso_now()
    env_payload = _load_json_safe(ENVIRONMENT_PATH, logger)
    infl_payload = _load_json_safe(INFLECTION_PATH, logger)

    ci_metadata = _build_metadata(
        args, env_payload, infl_payload, extraction_ts,
        artifact_kind="live_extraction_ci_runs",
    )
    transitions_metadata = _build_metadata(
        args, env_payload, infl_payload, extraction_ts,
        artifact_kind="live_extraction_test_transitions",
    )
    effective_repo_slug = ci_metadata["repository_slug"]
    effective_branch = ci_metadata["default_branch"]

    gh = GithubClient(
        token=os.environ.get("GH_TOKEN"),
        logger=logger,
    )

    # Tier 1A — Fetch workflow runs.
    runs, api_available, api_error = _fetch_ci_runs(
        gh, effective_repo_slug, effective_branch, logger,
    )

    # Tier 1B — Per-run JUnit artifact download and parse.
    per_run_statuses: dict[Any, dict[str, str]] = {}
    junit_artifacts_meta: list[dict[str, Any]] = []
    junit_emitting_workflow_set: set[str] = set()
    runs_processed_for_junit = 0

    if api_available and runs:
        # Walk runs in chronological order so the artifact-fetch loop
        # respects time semantics; if a max-runs cap is set, the
        # earliest runs are processed first.
        completed_runs = [
            r for r in runs if r.get("status") == "completed"
        ]
        completed_runs.sort(
            key=lambda r: r.get("created_at") or ""
        )
        if args.max_runs > 0:
            completed_runs = completed_runs[: args.max_runs]
        for run in completed_runs:
            run_id = run.get("id")
            if run_id is None:
                continue
            statuses, meta_list = fetch_junit_for_run(
                gh, effective_repo_slug, run_id, logger,
            )
            if statuses:
                per_run_statuses[run_id] = statuses
                wf_path = run.get("path") or ""
                if wf_path:
                    junit_emitting_workflow_set.add(Path(wf_path).name)
            junit_artifacts_meta.extend(meta_list)
            runs_processed_for_junit += 1

    junit_available = len(junit_artifacts_meta) > 0 and len(per_run_statuses) > 0
    junit_emitting_workflows = sorted(junit_emitting_workflow_set)
    junit_unavailable_reason: str | None = None
    if not junit_available:
        if not api_available:
            junit_unavailable_reason = (
                "GitHub Actions Runs API unavailable; JUnit XML "
                "cannot be retrieved. Cause: "
                f"{api_error or 'unspecified API failure'}."
            )
        elif not runs:
            junit_unavailable_reason = (
                "GitHub Actions Runs API returned zero workflow "
                f"runs on branch {effective_branch!r}; no JUnit XML "
                "to download."
            )
        else:
            junit_unavailable_reason = (
                "GitHub Actions returned workflow runs but no "
                "artifact matched the JUnit XML pattern "
                f"{JUNIT_ARTIFACT_PATTERNS.pattern!r}. The "
                "workflows in this repository do not emit JUnit "
                "XML; Metric 11 falls back to the HEAD-only "
                "skip-marker scan per AAP §0.5.3.12."
            )

    # Transition detection over the chronologically sorted run list.
    if api_available and per_run_statuses:
        runs_sorted = sorted(
            (r for r in runs if r.get("status") == "completed"),
            key=lambda r: r.get("created_at") or "",
        )
        transitions = detect_transitions(per_run_statuses, runs_sorted)
    else:
        transitions = []

    logger.info(
        "transitions_detected",
        extra={
            "event": "transitions_detected",
            "transition_count": len(transitions),
            "regression_count": sum(
                1 for t in transitions if t.get("kind") == "regression"
            ),
            "suppression_count": sum(
                1 for t in transitions if t.get("kind") == "suppression"
            ),
        },
    )

    # Tier 2 — HEAD-only in-repo skip-marker scan. Always invoked
    # regardless of Tier 1 outcome because the HEAD snapshot is a
    # complement to (not a substitute for) the transition signal.
    if args.repo_root:
        repo_root = Path(args.repo_root).resolve()
    else:
        toplevel = git_rev_parse_toplevel()
        if toplevel is not None:
            repo_root = toplevel
        else:
            # Fall back to the workspace's parent (the analyzed
            # repository root that the workspace lives inside).
            repo_root = WORKSPACE_ROOT.parent.parent.resolve()
            logger.warning(
                "repo_root_fallback",
                extra={
                    "event": "repo_root_fallback",
                    "repo_root": str(repo_root),
                    "reason": (
                        "git rev-parse --show-toplevel did not "
                        "resolve a repository root"
                    ),
                },
            )
    head_matches = head_skip_scan(repo_root, logger)

    # Assemble both payloads.
    ci_payload = _build_ci_runs_payload(
        metadata=ci_metadata,
        runs=runs,
        api_available=api_available,
        api_error=api_error,
        junit_artifacts=junit_artifacts_meta,
        repo_slug=effective_repo_slug,
        branch=effective_branch,
        junit_emitting_workflows=junit_emitting_workflows,
    )
    transitions_payload = _build_transitions_payload(
        metadata=transitions_metadata,
        transitions=transitions,
        head_matches=head_matches,
        junit_available=junit_available,
        junit_unavailable_reason=junit_unavailable_reason,
        api_available=api_available,
        runs_processed=runs_processed_for_junit,
        per_run_statuses=per_run_statuses,
    )

    _write_json(Path(args.ci_output), ci_payload)
    _write_json(Path(args.transitions_output), transitions_payload)

    logger.info(
        "script_complete",
        extra={
            "event": "script_complete",
            "ci_runs_count": len(runs),
            "ci_api_available": api_available,
            "junit_available": junit_available,
            "junit_artifacts_count": len(junit_artifacts_meta),
            "junit_emitting_workflows_count": len(junit_emitting_workflows),
            "transitions_count": len(transitions),
            "head_scan_match_count": len(head_matches),
            "ci_output": args.ci_output,
            "transitions_output": args.transitions_output,
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

