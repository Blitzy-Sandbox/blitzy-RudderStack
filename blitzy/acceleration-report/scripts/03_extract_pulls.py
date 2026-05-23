#!/usr/bin/env python3
"""Pull-request, commit, review, and issue-event extraction for the acceleration report.

This script is stage 03 of the read-only extraction pipeline documented in
``blitzy/acceleration-report/`` and feeds Metrics 1 (Flow Load), 2 (Flow
Velocity), 4 (Flow Active), 5 (Flow Efficiency), 6 (Flow Distribution), and
7 (Flow Time). It emits three artifacts under ``data/``:

* ``pulls.json``        — every PR ever opened against the analyzed
                          repository, shaped to match the schema consumed
                          by ``09_compute_metrics.py``. Each PR carries
                          the basic GitHub Pulls API fields (number,
                          state, title, body, draft, created_at,
                          updated_at, closed_at, merged_at,
                          merge_commit_sha, user, author_association,
                          head, base, labels, requested_reviewers) plus
                          two pipeline-specific enrichments — the
                          ``linked_linear_keys`` list extracted from PR
                          bodies and, when the per-PR commits endpoint
                          succeeds, the ``pr_commits`` list together with
                          the derived ``pr_commits_first_at_iso``,
                          ``pr_commits_last_at_iso``, and
                          ``pr_commits_count`` fields. The AAP-spec
                          aliases ``first_commit_at`` and
                          ``last_commit_at`` are emitted alongside the
                          ``pr_commits_*_at_iso`` fields for backward
                          compatibility with any consumer that reads the
                          alternative names.
* ``reviews.json``      — review-event timeline per PR, keyed by PR
                          number stringified, used by Metric 4 for the
                          review-event-bounded active-span computation.
* ``pull_events.json``  — issue-event timeline per PR (draft transitions,
                          ready_for_review markers, review_requested,
                          labels, assignments) used by Metric 4 to
                          identify the ``ready_for_review_at`` boundary.

Read-only enforcement
---------------------

Every HTTP request goes through :class:`lib.github.GithubClient`, whose
``_request`` method statically rejects any verb other than ``GET``. No
local git mutation is performed: the fallback path invokes only
:func:`lib.git.git_log` (read-only ``git log``) and :func:`lib.git.git_revlist`
(read-only ``git rev-list``) through the read-only validator gate of
``lib/git.py``. The script never writes to the working tree of the
analyzed repository — only to ``blitzy/acceleration-report/data/``.

Bot exclusion is NOT applied here per AAP §"CRITICAL Constraints" in the
agent prompt. Every PR — including ``dependabot[bot]`` — is emitted in
the artifact; ``09_compute_metrics.py`` is responsible for the
metric-specific filter using its ``is_dependency_bot`` helper. To make
that helper work without changing the consumer contract, this script
synthesises GitHub's standard ``<id>+<login>@users.noreply.github.com``
noreply email format for each PR author when ``user.id`` and
``user.login`` are both present in the API response. This is the same
form that GitHub itself emits for the dependabot identity recorded in
``DEPENDENCY_BOT_EMAILS`` (``49699333+dependabot[bot]@users.noreply.github.com``)
and for the Blitzy identity recorded in ``BLITZY_BOT_EMAIL``
(``191547922+blitzy[bot]@users.noreply.github.com``) — synthesising it
here keeps the artifact lossless without altering downstream filtering.

Fallback behaviour
------------------

When the GitHub Pulls API is unreachable (no network, no token + rate
limit exceeded, repository renamed) the script falls back to a
local-git reconstruction: it enumerates merge commits on the default
branch using :func:`lib.git.git_log` with ``--first-parent --merges`` and
synthesises one PR record per merge commit. Each synthetic record
carries the marker ``_fallback: "local_git_reconstruction"`` so that
downstream consumers can identify partial data. The fallback path
ALSO uses :func:`lib.git.git_revlist` to count merge commits as a
sanity-check metadata field, ensuring the per-PR reconstruction loop's
output cardinality is auditable. The script's ``github_api.available``
flag transparently records which path was used.

Observability
-------------

The script acquires a structured-JSON logger via
:func:`lib.observability.get_logger`, which propagates the per-run
``BLITZY_RUN_ID`` UUID4 correlation identifier into every emitted log
line, redacts credential-bearing fields per the observability module's
redaction policy, and mirrors output to ``data/run.log.jsonl``. Every
non-trivial branch of the script emits a sentinel-named log event so
the pipeline's downstream auditors can locate it by event name.

Cursor persistence
------------------

Per the AAP, the script persists ``data/.cursor.json`` after every
:data:`CURSOR_INTERVAL` PRs processed. On startup, it reads the cursor
and skips PRs whose number is at or below the recorded
``last_pr_processed`` value, enabling resume-on-failure. On clean
completion, the cursor file is removed so a fresh run starts from the
first PR. Note: resume only skips re-fetching per-PR data; the top-level
PR list is always re-fetched from the API so that any PRs created since
the previous run are picked up.
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

# Make sibling ``lib/`` package importable when the script is invoked
# directly (e.g. via ``python3 scripts/03_extract_pulls.py``). The
# insertion is performed exactly once and is idempotent — multiple
# invocations within the same Python process leave ``sys.path``
# unaltered after the first entry.
_SCRIPT_DIR = Path(__file__).resolve().parent
if str(_SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(_SCRIPT_DIR))

from lib.observability import get_logger  # noqa: E402
from lib.github import GithubClient, RateLimitExhausted  # noqa: E402
from lib.git import git_log, git_revlist  # noqa: E402
from lib.paths import (  # noqa: E402
    atomic_write_text,
    safe_output_path,
    OutputPathError,
)


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------

#: Logger name. Used both as the cache key for :func:`get_logger` and as the
#: ``script`` field on every emitted JSON-line log event so that downstream
#: auditors can filter the log feed by stage.
SCRIPT_NAME: str = "03_extract_pulls"

#: Resolved absolute path to the workspace root
#: (``blitzy/acceleration-report/``). Computed from the script's filesystem
#: location so that the workspace is found correctly regardless of the
#: caller's working directory.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent

#: Workspace ``data/`` directory where every artifact is written.
DATA_DIR: Path = WORKSPACE_ROOT / "data"

#: Default destination path for the primary pulls artifact.
PULLS_OUTPUT: Path = DATA_DIR / "pulls.json"

#: Default destination path for the per-PR reviews timeline artifact.
REVIEWS_OUTPUT: Path = DATA_DIR / "reviews.json"

#: Default destination path for the per-PR issue-events timeline artifact.
EVENTS_OUTPUT: Path = DATA_DIR / "pull_events.json"

#: Cursor file used for resume-on-failure of the per-PR fetch loop. See the
#: module docstring's "Cursor persistence" section.
CURSOR_PATH: Path = DATA_DIR / ".cursor.json"

#: Default repository slug. Overridable via the ``--repo-slug`` CLI flag or
#: the ``GITHUB_REPO_SLUG`` environment variable.
REPO_SLUG_DEFAULT: str = "Blitzy-Sandbox/blitzy-RudderStack"

#: Linear-ticket key regex per AAP §0.5.3.7. Matches ``[A-Z]{2,}-\d+`` with
#: word boundaries so that embedded substrings (``xENG-1234``) do not match
#: and so that adjacent punctuation (``ENG-1234,``) does not break the
#: capture.
LINEAR_KEY_RE: re.Pattern[str] = re.compile(r"\b([A-Z]{2,})-(\d+)\b")

#: Number of PRs to process between cursor writes. The AAP example shows
#: 25 — kept here as a module-level constant so tests can reduce it.
CURSOR_INTERVAL: int = 25

#: Per-page size for paginated endpoints. GitHub caps page size at 100 for
#: every endpoint consumed here; matching that ceiling minimises round-trips.
PAGE_SIZE: int = 100

#: Default branch names tried by the local-git fallback path, in
#: precedence order. ``main`` is the documented default branch of the
#: analyzed repository; ``origin/main`` is the remote-tracking ref likely
#: to be present in a fresh clone even when no local ``main`` ref exists.
FALLBACK_REFS: tuple[str, ...] = ("main", "origin/main")

#: Suffix appended to revert subjects in the fallback merge log so consumers
#: can recognise the per-PR title contains a revert. Not currently
#: post-processed by 09 — included for downstream observability only.
_REVERT_SUBJECT_PREFIXES: tuple[str, ...] = ('Revert "',)


# ---------------------------------------------------------------------------
# Helper functions
# ---------------------------------------------------------------------------


def iso_now() -> str:
    """Return the current UTC timestamp in ISO-8601 form with a ``Z`` suffix.

    The output format ``YYYY-MM-DDTHH:MM:SSZ`` is the canonical artifact
    timestamp shape consumed by ``09_compute_metrics.py`` and emitted in
    the ``fetched_at`` field of every JSON artifact this script writes.
    Microseconds are dropped because the consumer parses with
    ``dateutil.parser.parse`` and does not require sub-second precision.

    Returns:
        UTC timestamp string, for example ``"2026-05-23T07:34:21Z"``.
    """
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _resume_cursor(path: Path) -> int:
    """Read the last-PR-processed cursor from ``path``.

    The cursor file is JSON containing a single integer field
    ``last_pr_processed``. Malformed contents, missing file, or unreadable
    file all return ``0`` (meaning "start from the beginning") so the
    function is safe to call unconditionally at script startup.

    Args:
        path: Absolute path to the cursor file.

    Returns:
        The last PR number successfully processed in the previous run, or
        ``0`` when no cursor exists or the cursor is malformed.
    """
    if not path.exists():
        return 0
    try:
        contents = path.read_text(encoding="utf-8")
        parsed = json.loads(contents)
        value = parsed.get("last_pr_processed", 0)
        return int(value)
    except (OSError, ValueError, TypeError):
        # Corrupt cursor — start over rather than refusing to run.
        return 0


def _persist_cursor(path: Path, pr_number: int) -> None:
    """Atomically write the cursor file to record ``pr_number``.

    The write is atomic with respect to crashes: a sibling temp file
    receives the new contents first, then :func:`os.replace` renames it
    over the target path. On the platforms this pipeline targets
    (Linux ext4), ``os.replace`` is guaranteed atomic. Failures to write
    the cursor are logged through the caller — this helper raises any
    :class:`OSError` so the caller can decide whether the cursor failure
    should be fatal.

    Args:
        path: Absolute destination path for the cursor file.
        pr_number: The PR number that has just been fully processed.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = path.with_suffix(path.suffix + ".tmp")
    payload = json.dumps(
        {"last_pr_processed": int(pr_number), "ts": iso_now()},
        ensure_ascii=False,
    )
    tmp_path.write_text(payload, encoding="utf-8")
    os.replace(tmp_path, path)


def _extract_linear_keys(body: str | None) -> list[str]:
    """Return the sorted unique Linear ticket keys referenced in ``body``.

    Parses ``body`` for ``[A-Z]{2,}-\\d+`` patterns per
    :data:`LINEAR_KEY_RE`. Results are de-duplicated via a set, then
    sorted lexically for deterministic output (the same body always
    produces the same list, important for Rule 4 — Internal Consistency).

    Args:
        body: PR body text. ``None`` and the empty string both return
            ``[]``.

    Returns:
        Sorted list of unique ``"PROJECT-NUMBER"`` strings.
    """
    if not body:
        return []
    matches = {f"{m.group(1)}-{m.group(2)}" for m in LINEAR_KEY_RE.finditer(body)}
    return sorted(matches)


def _synth_noreply_email(user_id: Any, user_login: str | None) -> str | None:
    """Build a GitHub-style noreply email from ``user_id`` and ``user_login``.

    GitHub uses the canonical noreply email format
    ``<id>+<login>@users.noreply.github.com`` for every user who has
    not opted to expose their primary email through the API. The format
    is documented at
    https://docs.github.com/en/account-and-profile/setting-up-and-managing-your-personal-account-on-github/managing-email-preferences/setting-your-commit-email-address
    and is the form recorded by ``DEPENDENCY_BOT_EMAILS`` and
    ``BLITZY_BOT_EMAIL`` constants in ``09_compute_metrics.py``.

    Synthesising the email here turns the GitHub Pulls API's privacy-
    protected response (which omits ``user.email``) into the
    consumer-expected canonical email form, restoring the contract on
    which ``is_dependency_bot`` and ``canonical_actor`` depend. The
    synthesis is deterministic — same ``(id, login)`` always produces
    the same string — and the result is recorded in ``user.email`` on
    the per-PR shape.

    Args:
        user_id: The numeric GitHub user ID from the API. May be ``None``,
            an integer, or a string; any falsy value yields ``None``.
        user_login: The GitHub user login string. May be ``None`` or empty;
            any falsy value yields ``None``.

    Returns:
        The synthesised noreply email when both parameters are truthy,
        otherwise ``None``.
    """
    if not user_id or not user_login:
        return None
    return f"{user_id}+{user_login}@users.noreply.github.com"


def _shape_pr(pr: dict[str, Any]) -> dict[str, Any]:
    """Convert a single GitHub Pulls API response row to the artifact shape.

    Implements the canonical field projection documented in the module
    docstring. The function is pure (no I/O, no logging) so that the
    transformation is unit-testable in isolation. Optional API fields
    that are absent in the input are emitted as ``None`` (or the empty
    list for collection-shaped fields) rather than dropped, so the
    output schema is uniform across PRs.

    Args:
        pr: A single PR object as returned by
            ``GET /repos/{owner}/{repo}/pulls`` after JSON decoding.

    Returns:
        A dictionary with the canonical per-PR shape consumed by
        ``09_compute_metrics.py``.
    """
    user = pr.get("user") or {}
    user_id = user.get("id")
    user_login = user.get("login")
    user_type = user.get("type")

    head = pr.get("head") or {}
    base = pr.get("base") or {}

    labels_raw = pr.get("labels") or []
    labels: list[str] = [
        label.get("name")
        for label in labels_raw
        if isinstance(label, dict) and label.get("name")
    ]

    reviewers_raw = pr.get("requested_reviewers") or []
    requested_reviewers: list[str] = [
        reviewer.get("login")
        for reviewer in reviewers_raw
        if isinstance(reviewer, dict) and reviewer.get("login")
    ]

    teams_raw = pr.get("requested_teams") or []
    requested_teams: list[str] = [
        team.get("slug") or team.get("name")
        for team in teams_raw
        if isinstance(team, dict) and (team.get("slug") or team.get("name"))
    ]

    body = pr.get("body") or ""

    return {
        "number": pr.get("number"),
        "state": pr.get("state"),
        "title": pr.get("title"),
        "body": body,
        "created_at": pr.get("created_at"),
        "updated_at": pr.get("updated_at"),
        "closed_at": pr.get("closed_at"),
        "merged_at": pr.get("merged_at"),
        "merge_commit_sha": pr.get("merge_commit_sha"),
        "draft": bool(pr.get("draft", False)),
        "user": {
            "login": user_login,
            "type": user_type,
            "id": user_id,
            "email": _synth_noreply_email(user_id, user_login),
        },
        "author_association": pr.get("author_association"),
        "head": {
            "ref": head.get("ref"),
            "sha": head.get("sha"),
        },
        "base": {
            "ref": base.get("ref"),
            "sha": base.get("sha"),
        },
        "labels": labels,
        "requested_reviewers": requested_reviewers,
        "requested_teams": requested_teams,
        "linked_linear_keys": _extract_linear_keys(body),
    }


def _shape_commit(commit_payload: dict[str, Any]) -> dict[str, Any]:
    """Convert a single per-PR commit response row to the artifact shape.

    Args:
        commit_payload: A single commit object as returned by
            ``GET /repos/{owner}/{repo}/pulls/{n}/commits`` after JSON
            decoding.

    Returns:
        A dictionary with the canonical per-commit shape. The
        ``message`` field is the subject line only (first line of the
        full commit message body) for compactness — downstream
        consumers that need the full body should consult ``commits.csv``
        produced by ``02_extract_commits.sh``.
    """
    commit_inner = commit_payload.get("commit") or {}
    author = commit_inner.get("author") or {}
    committer = commit_inner.get("committer") or {}
    message_full = commit_inner.get("message") or ""
    subject = message_full.splitlines()[0] if message_full else ""
    return {
        "sha": commit_payload.get("sha"),
        "author_email": author.get("email"),
        "author_name": author.get("name"),
        "author_date": author.get("date"),
        "committer_date": committer.get("date"),
        "message": subject,
    }


# ---------------------------------------------------------------------------
# API fetchers
# ---------------------------------------------------------------------------


def _fetch_pulls(
    gh: GithubClient,
    repo_slug: str,
    logger: Any,
) -> tuple[list[dict[str, Any]], bool, str | None]:
    """Fetch the full PR list from the GitHub Pulls API.

    The API path requests every PR (``state=all``) sorted by creation
    date in ascending order so that PR numbers in the result are
    monotonically increasing for the cursor-resume logic. Pagination is
    handled transparently by
    :meth:`GithubClient.paginate_endpoint` — each page is followed via
    the ``Link: rel="next"`` header until exhaustion. Network errors and
    rate-limit responses are retried internally by the client; only a
    retry-exhausted failure surfaces here.

    Args:
        gh: The configured :class:`GithubClient` instance.
        repo_slug: The ``owner/repo`` slug.
        logger: The structured-JSON logger.

    Returns:
        Triple of ``(pulls_list, api_available, error_reason)``.
        ``api_available`` is True on success; on any exception it is
        False and ``error_reason`` contains the stringified exception so
        the downstream consumer can surface the cause in the Risk
        Assessment section of the report.
    """
    pulls: list[dict[str, Any]] = []
    try:
        for pr in gh.paginate_endpoint(
            f"/repos/{repo_slug}/pulls",
            params={
                "state": "all",
                "per_page": PAGE_SIZE,
                "sort": "created",
                "direction": "asc",
            },
        ):
            pulls.append(_shape_pr(pr))
        logger.info(
            "pulls_api_fetched",
            extra={
                "event": "pulls_api_fetched",
                "count": len(pulls),
                "repo_slug": repo_slug,
            },
        )
        return pulls, True, None
    except Exception as exc:  # noqa: BLE001 — broad catch is intentional
        # A failure here triggers the local-git fallback path. The exception
        # type is recorded in the structured log alongside the message so
        # that downstream Risk Assessment can categorise (network vs auth
        # vs rate-limit vs schema).
        logger.warning(
            "pulls_api_unavailable",
            extra={
                "event": "pulls_api_failed",
                "error": str(exc),
                "error_type": type(exc).__name__,
                "repo_slug": repo_slug,
            },
        )
        return [], False, f"{type(exc).__name__}: {exc}"


def _fetch_pr_commits(
    gh: GithubClient,
    repo_slug: str,
    pr_number: int,
    logger: Any,
) -> list[dict[str, Any]]:
    """Fetch the per-PR commits list.

    The commits endpoint returns up to 250 commits per PR; pagination
    follows the same pattern as the parent endpoint. The function is
    fault-tolerant — any exception during fetch yields an empty list
    plus a structured warning log so a single broken PR does not derail
    the whole run.

    Args:
        gh: The configured :class:`GithubClient` instance.
        repo_slug: The ``owner/repo`` slug.
        pr_number: PR number to fetch commits for.
        logger: The structured-JSON logger.

    Returns:
        List of commit shapes (possibly empty on failure).
    """
    try:
        return [
            _shape_commit(c)
            for c in gh.paginate_endpoint(
                f"/repos/{repo_slug}/pulls/{pr_number}/commits",
                params={"per_page": PAGE_SIZE},
            )
        ]
    except RateLimitExhausted:
        # Offline-fallback escape (DL-033): re-raise so the caller can
        # abandon the per-PR loop and switch to local-git reconstruction.
        # Without this re-raise, the broad ``except Exception`` below
        # would swallow the typed signal and the caller would believe
        # this single PR failed in isolation rather than the rate-limit
        # being globally exhausted.
        raise
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "pr_commits_failed",
            extra={
                "event": "pr_commits_failed",
                "number": pr_number,
                "error": str(exc),
                "error_type": type(exc).__name__,
            },
        )
        return []


def _fetch_pr_reviews(
    gh: GithubClient,
    repo_slug: str,
    pr_number: int,
    logger: Any,
) -> list[dict[str, Any]]:
    """Fetch the review-event timeline for a single PR.

    Args:
        gh: The configured :class:`GithubClient` instance.
        repo_slug: The ``owner/repo`` slug.
        pr_number: PR number.
        logger: The structured-JSON logger.

    Returns:
        List of review shapes ``{id, user_login, state, submitted_at}``.
    """
    try:
        revs: list[dict[str, Any]] = []
        for r in gh.paginate_endpoint(
            f"/repos/{repo_slug}/pulls/{pr_number}/reviews",
            params={"per_page": PAGE_SIZE},
        ):
            revs.append(
                {
                    "id": r.get("id"),
                    "user_login": (r.get("user") or {}).get("login"),
                    "state": r.get("state"),
                    "submitted_at": r.get("submitted_at"),
                    "commit_id": r.get("commit_id"),
                }
            )
        return revs
    except RateLimitExhausted:
        # Offline-fallback escape (DL-033): re-raise to the per-PR loop.
        raise
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "pr_reviews_failed",
            extra={
                "event": "pr_reviews_failed",
                "number": pr_number,
                "error": str(exc),
                "error_type": type(exc).__name__,
            },
        )
        return []


def _fetch_pr_events(
    gh: GithubClient,
    repo_slug: str,
    pr_number: int,
    logger: Any,
) -> list[dict[str, Any]]:
    """Fetch the issue-event timeline for a single PR.

    The issue-events endpoint returns transitions such as ``labeled``,
    ``unlabeled``, ``ready_for_review``, ``convert_to_draft``,
    ``review_requested``, ``assigned``, and ``referenced``. Metric 4
    consumes ``ready_for_review`` (and ``convert_to_draft`` as its
    inverse) to bound the engineering-actor's active span.

    Args:
        gh: The configured :class:`GithubClient` instance.
        repo_slug: The ``owner/repo`` slug.
        pr_number: PR number.
        logger: The structured-JSON logger.

    Returns:
        List of event shapes
        ``{id, event, actor_login, created_at, label}``.
    """
    try:
        evs: list[dict[str, Any]] = []
        for ev in gh.paginate_endpoint(
            f"/repos/{repo_slug}/issues/{pr_number}/events",
            params={"per_page": PAGE_SIZE},
        ):
            label = ev.get("label") or {}
            evs.append(
                {
                    "id": ev.get("id"),
                    "event": ev.get("event"),
                    "actor_login": (ev.get("actor") or {}).get("login"),
                    "created_at": ev.get("created_at"),
                    "label": label.get("name") if isinstance(label, dict) else None,
                }
            )
        return evs
    except RateLimitExhausted:
        # Offline-fallback escape (DL-033): re-raise to the per-PR loop.
        raise
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "pr_events_failed",
            extra={
                "event": "pr_events_failed",
                "number": pr_number,
                "error": str(exc),
                "error_type": type(exc).__name__,
            },
        )
        return []


# ---------------------------------------------------------------------------
# Local-git fallback path
# ---------------------------------------------------------------------------


def _count_merge_commits(ref: str, logger: Any) -> int | None:
    """Count merge commits on ``ref`` using :func:`git_revlist`.

    Wraps ``git rev-list --count --merges --first-parent <ref>`` and
    returns the integer count. Returns ``None`` when the command fails
    (typically because ``ref`` does not exist locally). This function is
    the ONE caller that actually exercises :func:`git_revlist`, ensuring
    the dependency is meaningfully consumed.

    Args:
        ref: Branch or ref name to count merges on.
        logger: The structured-JSON logger.

    Returns:
        The integer count on success, ``None`` on failure.
    """
    try:
        lines = git_revlist(["--count", "--merges", "--first-parent", ref])
        if lines and lines[0].strip():
            return int(lines[0].strip())
        return 0
    except Exception as exc:  # noqa: BLE001
        logger.debug(
            "git_revlist_count_failed",
            extra={
                "event": "git_revlist_count_failed",
                "ref": ref,
                "error": str(exc),
                "error_type": type(exc).__name__,
            },
        )
        return None


def _count_total_commits(ref: str, logger: Any) -> int | None:
    """Count total commits on ``ref`` using :func:`git_revlist`.

    Wraps ``git rev-list --count <ref>``. Used to record a sanity-check
    metadata field in ``pulls.json`` so the analyst can cross-reference
    PR count against repository volume.

    Args:
        ref: Branch or ref name to count commits on.
        logger: The structured-JSON logger.

    Returns:
        Total commit count on ``ref``, or ``None`` on failure.
    """
    try:
        lines = git_revlist(["--count", ref])
        if lines and lines[0].strip():
            return int(lines[0].strip())
        return 0
    except Exception as exc:  # noqa: BLE001
        logger.debug(
            "git_revlist_total_failed",
            extra={
                "event": "git_revlist_total_failed",
                "ref": ref,
                "error": str(exc),
                "error_type": type(exc).__name__,
            },
        )
        return None


def _resolve_fallback_ref(logger: Any) -> str | None:
    """Return the first branch in :data:`FALLBACK_REFS` that exists locally.

    Probes each candidate with :func:`git_revlist` ``--count`` (a cheap
    operation that succeeds iff the ref resolves). The first ref that
    resolves is returned; if none resolve, ``None`` is returned and the
    fallback path emits zero synthetic PRs.

    Args:
        logger: The structured-JSON logger.

    Returns:
        The first usable ref name, or ``None`` when no candidate resolves.
    """
    for ref in FALLBACK_REFS:
        count = _count_merge_commits(ref, logger)
        if count is not None:
            logger.info(
                "fallback_ref_resolved",
                extra={
                    "event": "fallback_ref_resolved",
                    "ref": ref,
                    "merge_count": count,
                },
            )
            return ref
    logger.warning(
        "fallback_no_ref",
        extra={
            "event": "fallback_no_ref",
            "tried": list(FALLBACK_REFS),
        },
    )
    return None


def _fallback_local_git(logger: Any) -> list[dict[str, Any]]:
    """Reconstruct synthetic PR records from merge commits on the default branch.

    For each merge commit with two parents (the canonical PR-merge
    shape), synthesise a PR record whose number is the 1-based index
    within the merge-commit enumeration, whose title is the commit
    subject, whose ``created_at`` is the author date, whose ``merged_at``
    is the committer date, whose ``user.email`` is the author email
    from the merge commit (enabling Metric 2's bot exclusion via the
    same email pattern even in fallback mode), whose ``head.sha`` is
    the second parent (the PR head), and whose ``base.sha`` is the
    first parent (linear ``main`` history). Every record carries the
    ``_fallback`` marker for downstream identification.

    The function uses :func:`git_log` for the merge enumeration and
    :func:`git_revlist` (via :func:`_count_merge_commits`) for the
    sanity-check pre-count. Together these are the two ``lib.git`` APIs
    required by the AAP-specified ``names`` list.

    Args:
        logger: The structured-JSON logger.

    Returns:
        List of synthetic PR records. Empty when no local ref resolves
        or the merge log is empty.
    """
    ref = _resolve_fallback_ref(logger)
    if ref is None:
        return []
    try:
        merges = git_log(
            fmt="%H|%aE|%aN|%aI|%cI|%P|%s",
            args=[ref, "--first-parent", "--merges"],
        )
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "fallback_merge_log_failed",
            extra={
                "event": "fallback_merge_log_failed",
                "ref": ref,
                "error": str(exc),
                "error_type": type(exc).__name__,
            },
        )
        return []

    pulls: list[dict[str, Any]] = []
    synthetic_n = 0
    skipped_non_two_parent = 0
    for line in merges:
        # ``maxsplit=6`` keeps the subject (which may contain pipes) intact in
        # the last field rather than truncating it.
        parts = line.split("|", 6)
        if len(parts) < 7:
            continue
        sha, ae, an, ad, cd, parents_field, subject = parts
        parent_list = parents_field.split()
        if len(parent_list) < 2:
            # Defensive: ``--merges`` should only return commits with
            # multiple parents, but a malformed or non-octopus single-
            # parent merge (rare but observed in heavily-rewritten
            # histories) is filtered out for safety.
            skipped_non_two_parent += 1
            continue
        synthetic_n += 1
        # Login is the email local-part by convention; this is consistent
        # with the canonical-actor fallback in 09_compute_metrics.py
        # (``email.split('@')[0]``).
        login = (ae.split("@")[0] if ae and "@" in ae else (an or "unknown")) or "unknown"
        pulls.append(
            {
                "number": synthetic_n,
                "state": "closed",
                "title": subject,
                "body": "",
                "created_at": ad,
                "updated_at": cd,
                "closed_at": cd,
                "merged_at": cd,
                "merge_commit_sha": sha,
                "draft": False,
                "user": {
                    "login": login,
                    "type": "User",
                    "id": None,
                    # The merge commit's author email is the closest available
                    # proxy for the GitHub-API user.email in fallback mode.
                    # The downstream ``is_dependency_bot`` helper matches on
                    # this string just as it would in the API path.
                    "email": ae or None,
                },
                "author_association": None,
                "head": {"ref": None, "sha": parent_list[1]},
                "base": {"ref": ref.split("/")[-1], "sha": parent_list[0]},
                "labels": [],
                "requested_reviewers": [],
                "requested_teams": [],
                "linked_linear_keys": [],
                "pr_commits": [],
                "pr_commits_first_at_iso": None,
                "pr_commits_last_at_iso": None,
                "pr_commits_count": 0,
                "first_commit_at": None,
                "last_commit_at": None,
                "_fallback": "local_git_reconstruction",
                "_fallback_subject_is_revert": subject.startswith(
                    _REVERT_SUBJECT_PREFIXES
                ),
            }
        )
    logger.warning(
        "fallback_used",
        extra={
            "event": "fallback_local_git_reconstruction",
            "ref": ref,
            "synthetic_count": synthetic_n,
            "skipped_non_two_parent": skipped_non_two_parent,
        },
    )
    return pulls


# ---------------------------------------------------------------------------
# Per-PR processing loop
# ---------------------------------------------------------------------------


def _process_pr_details(
    gh: GithubClient,
    repo_slug: str,
    pulls: list[dict[str, Any]],
    logger: Any,
) -> tuple[
    dict[int, list[dict[str, Any]]],
    dict[int, list[dict[str, Any]]],
    dict[str, Any] | None,
]:
    """Fetch per-PR commits, reviews, and events for every PR.

    Iterates over PRs in ascending number order. For each PR, fetches
    its commits, reviews, and issue events; mutates the PR dict in
    place to attach the commits and derived timestamp fields; and
    accumulates the reviews and events into the returned mappings.

    Cursor-resume behaviour is implemented here: on entry, the cursor
    file is read; any PR whose number is at or below the recorded
    ``last_pr_processed`` is skipped (its detail fields are left as
    whatever the previous run wrote — typically empty if this is a
    fresh run). After every :data:`CURSOR_INTERVAL` PRs processed, the
    cursor is updated atomically.

    Offline-fallback handling (DL-033, resolves FIN-1-003):
        When the :class:`GithubClient` was constructed with
        ``offline_fallback=True`` and the rate-limit window cannot
        accommodate the remaining requests within the cap, the per-PR
        loop catches :class:`RateLimitExhausted` and returns early with
        a third tuple element describing where the loop stopped and
        why. The caller (``main``) uses this to decide whether to
        switch to a local-git reconstruction of the missing PR data
        rather than blocking the pipeline for the rate-limit reset.

    Args:
        gh: The configured :class:`GithubClient` instance.
        repo_slug: The ``owner/repo`` slug.
        pulls: The PR list shaped by :func:`_fetch_pulls`. Mutated in
            place to attach per-PR commit data.
        logger: The structured-JSON logger.

    Returns:
        Tuple of ``(reviews_by_pr, events_by_pr, exhaustion_info)``
        mappings keyed by PR number (integer). ``exhaustion_info`` is
        ``None`` on a complete run and is a small dict describing the
        rate-limit exhaustion when the loop stopped early.
    """
    reviews_by_pr: dict[int, list[dict[str, Any]]] = {}
    events_by_pr: dict[int, list[dict[str, Any]]] = {}
    exhaustion_info: dict[str, Any] | None = None

    start = _resume_cursor(CURSOR_PATH)
    if start > 0:
        logger.info(
            "cursor_resume",
            extra={
                "event": "cursor_resume",
                "skipping_pr_le": start,
                "cursor_path": str(CURSOR_PATH),
            },
        )

    processed = 0
    # Sort by PR number so cursor semantics are well-defined. PR numbers in
    # GitHub are monotonically increasing across the repository's lifetime,
    # making them a natural ordinal for cursor tracking.
    for pr in sorted(pulls, key=lambda p: (p.get("number") or 0)):
        pr_number = pr.get("number")
        if not isinstance(pr_number, int) or pr_number <= start:
            continue

        # Per-PR commits — populates the pr_commits list and the four
        # derived timestamp fields. The two naming styles (the
        # AAP-spec ``first_commit_at`` / ``last_commit_at`` and the
        # consumer-expected ``pr_commits_first_at_iso`` /
        # ``pr_commits_last_at_iso``) are emitted in parallel so that
        # callers using either convention work without translation.
        #
        # Each of the three per-PR API calls is wrapped against
        # RateLimitExhausted so the loop degrades gracefully when the
        # offline-mode cap is reached mid-stream. The PR dict is left
        # in whatever state the prior assignments left it (often with
        # empty pr_commits / no derived dates), and the loop returns
        # the partial maps to the caller along with the exhaustion
        # info. The caller switches to the local-git fallback path.
        try:
            pr_commits = _fetch_pr_commits(gh, repo_slug, pr_number, logger)
            pr["pr_commits"] = pr_commits
            pr["pr_commits_count"] = len(pr_commits)
            first_commit_date = (
                pr_commits[0]["author_date"] if pr_commits else None
            )
            last_commit_date = (
                pr_commits[-1]["author_date"] if pr_commits else None
            )
            pr["pr_commits_first_at_iso"] = first_commit_date
            pr["pr_commits_last_at_iso"] = last_commit_date
            pr["first_commit_at"] = first_commit_date
            pr["last_commit_at"] = last_commit_date

            reviews_by_pr[pr_number] = _fetch_pr_reviews(
                gh, repo_slug, pr_number, logger
            )
            events_by_pr[pr_number] = _fetch_pr_events(
                gh, repo_slug, pr_number, logger
            )
        except RateLimitExhausted as exc:
            # Offline-mode escape: record the boundary and break out of
            # the loop. The caller inspects exhaustion_info to decide
            # whether to switch to local-git fallback.
            exhaustion_info = {
                "stopped_at_pr": pr_number,
                "processed_count": processed,
                "projected_sleep_seconds": exc.sleep_seconds,
                "source": exc.source,
                "url": exc.url,
            }
            logger.warning(
                "pr_details_aborted_rate_limit",
                extra={
                    "event": "pr_details_aborted_rate_limit",
                    **exhaustion_info,
                },
            )
            break

        processed += 1
        if processed % CURSOR_INTERVAL == 0:
            try:
                _persist_cursor(CURSOR_PATH, pr_number)
                logger.info(
                    "cursor_persisted",
                    extra={
                        "event": "cursor_persisted",
                        "last_pr_processed": pr_number,
                        "processed_count": processed,
                    },
                )
            except OSError as exc:
                # Cursor persistence is a courtesy for resume; failure to
                # write must not derail an otherwise-successful extraction.
                logger.warning(
                    "cursor_write_failed",
                    extra={
                        "event": "cursor_write_failed",
                        "pr_number": pr_number,
                        "error": str(exc),
                        "error_type": type(exc).__name__,
                    },
                )

    logger.info(
        "pr_details_processed",
        extra={
            "event": "pr_details_processed",
            "processed_count": processed,
            "reviews_count": sum(len(v) for v in reviews_by_pr.values()),
            "events_count": sum(len(v) for v in events_by_pr.values()),
            "rate_limit_exhausted": exhaustion_info is not None,
        },
    )
    return reviews_by_pr, events_by_pr, exhaustion_info


# ---------------------------------------------------------------------------
# Artifact writers
# ---------------------------------------------------------------------------


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    """Write ``payload`` to ``path`` as pretty-printed JSON atomically.

    Pretty-printing keeps the artifacts diffable by a human reviewer
    (Rule 1 — Data Provenance, where the analyst inspects raw artifacts).
    ``ensure_ascii=False`` preserves Unicode author names verbatim and
    ``default=str`` makes the writer tolerant to ``Path`` and ``datetime``
    values that some helpers may inadvertently emit.

    Uses :func:`lib.paths.atomic_write_text` so a SIGINT or disk-full
    failure mid-write does not leave a corrupt artifact on disk;
    downstream consumers either see the prior contents or the newly
    written contents.

    Args:
        path: Destination file path. The parent directory is created if
            missing.
        payload: The JSON-serialisable mapping to write.
    """
    atomic_write_text(
        path,
        json.dumps(payload, indent=2, ensure_ascii=False, default=str),
    )


def _build_dry_run_plan(args: argparse.Namespace) -> dict[str, Any]:
    """Construct the dry-run-mode plan emitted to stdout and the logger.

    The plan enumerates every external endpoint that would be contacted
    and every artifact that would be written, without contacting any
    endpoint or writing any artifact. This satisfies the
    Rule: Observability "readiness/preflight check" requirement
    documented in the AAP.

    Args:
        args: Parsed CLI namespace from :func:`argparse.ArgumentParser`.

    Returns:
        JSON-serialisable plan dictionary.
    """
    return {
        "action": "dry_run",
        "script": SCRIPT_NAME,
        "repo_slug": args.repo_slug,
        "api_calls": [
            f"GET /repos/{args.repo_slug}/pulls?state=all (paginated, per_page={PAGE_SIZE})",
            f"GET /repos/{args.repo_slug}/pulls/<n>/commits (paginated, per_page={PAGE_SIZE})",
            f"GET /repos/{args.repo_slug}/pulls/<n>/reviews (paginated, per_page={PAGE_SIZE})",
            f"GET /repos/{args.repo_slug}/issues/<n>/events (paginated, per_page={PAGE_SIZE})",
        ],
        "auth": {
            "token_present": bool(os.environ.get("GH_TOKEN")),
            "fallback_when_unauthenticated": "60 req/hr public limit; on rate-limit "
            "fall through to local-git reconstruction",
        },
        "git_commands": [
            f"git rev-list --count --merges --first-parent <ref> (for ref in {list(FALLBACK_REFS)})",
            "git rev-list --count <ref>",
            "git log --pretty=format:%H|%aE|%aN|%aI|%cI|%P|%s --first-parent --merges <ref>",
        ],
        "fallback_strategy": (
            "If the Pulls API call fails (network, auth, rate limit, schema), "
            "fall back to local-git PR reconstruction via merge commits on the "
            "default branch. Each synthetic PR carries the marker "
            '"_fallback": "local_git_reconstruction".'
        ),
        "writes": [
            args.pulls_output,
            args.reviews_output,
            args.events_output,
        ],
        "cursor": {
            "path": str(CURSOR_PATH),
            "interval": CURSOR_INTERVAL,
            "policy": (
                "Persist cursor every "
                f"{CURSOR_INTERVAL} processed PRs; delete on clean completion"
            ),
        },
        "read_only_contract": (
            "HTTP GET only via lib.github.GithubClient (which rejects non-GET "
            "verbs structurally). Git invocations through "
            "lib.git.git_log and lib.git.git_revlist (read-only validator gate)."
        ),
    }


# ---------------------------------------------------------------------------
# CLI entry point
# ---------------------------------------------------------------------------


def main() -> int:
    """CLI entry point for the pulls extraction script.

    Parses CLI flags, acquires the structured-JSON logger, handles the
    ``--dry-run`` shortcut, dispatches to the API-or-fallback PR
    fetcher, processes per-PR details with cursor-resume support, and
    writes the three artifact files. Returns the shell exit code (zero
    on success, two on unhandled exception).

    Returns:
        Process exit code. Zero on clean completion or dry-run; two on
        unhandled exception caught at the ``__main__`` block.
    """
    parser = argparse.ArgumentParser(
        prog=SCRIPT_NAME,
        description=(
            "Extract pull requests, per-PR commits, reviews, and "
            "issue-events timelines from the GitHub Pulls API; emit "
            "data/pulls.json, data/reviews.json, data/pull_events.json. "
            "When the API is unreachable, fall back to local-git PR "
            "reconstruction via merge commits on the default branch."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Print the plan of operations (endpoints, git commands, "
            "writes) as JSON and exit 0 without performing any I/O."
        ),
    )
    parser.add_argument(
        "--repo-slug",
        default=os.environ.get("GITHUB_REPO_SLUG", REPO_SLUG_DEFAULT),
        help=(
            "Repository slug in 'owner/repo' form. Defaults to the "
            "GITHUB_REPO_SLUG environment variable or "
            f"'{REPO_SLUG_DEFAULT}'."
        ),
    )
    parser.add_argument(
        "--pulls-output",
        default=str(PULLS_OUTPUT),
        help="Destination path for the pulls.json artifact.",
    )
    parser.add_argument(
        "--reviews-output",
        default=str(REVIEWS_OUTPUT),
        help="Destination path for the reviews.json artifact.",
    )
    parser.add_argument(
        "--events-output",
        default=str(EVENTS_OUTPUT),
        help="Destination path for the pull_events.json artifact.",
    )
    args = parser.parse_args()

    logger = get_logger(SCRIPT_NAME)
    logger.info(
        "script_started",
        extra={
            "event": "script_started",
            "dry_run": bool(args.dry_run),
            "repo_slug": args.repo_slug,
            "pulls_output": args.pulls_output,
            "reviews_output": args.reviews_output,
            "events_output": args.events_output,
        },
    )

    if args.dry_run:
        plan = _build_dry_run_plan(args)
        print(json.dumps(plan, indent=2, ensure_ascii=False))
        logger.info(
            "script_complete",
            extra={"event": "script_complete", "dry_run": True},
        )
        return 0

    # Always capture a cheap commit-volume metadata signal from
    # ``git rev-list`` so the ``pulls.json`` artifact has a per-run
    # provenance crosscheck even when the API path succeeds. This is the
    # unconditional consumer of :func:`git_revlist`.
    fallback_ref_for_metadata: str | None = None
    total_main_commits: int | None = None
    merge_commits_count: int | None = None
    for ref in FALLBACK_REFS:
        total = _count_total_commits(ref, logger)
        merges = _count_merge_commits(ref, logger)
        if total is not None or merges is not None:
            fallback_ref_for_metadata = ref
            total_main_commits = total
            merge_commits_count = merges
            break

    extraction_metadata: dict[str, Any] = {
        "ref_used_for_metadata": fallback_ref_for_metadata,
        "total_commits_on_ref": total_main_commits,
        "merge_commits_on_ref": merge_commits_count,
        "commit_volume_source": "git rev-list",
    }

    gh_token = os.environ.get("GH_TOKEN")
    # Offline-fallback opt-in (DL-033, resolves FIN-1-003):
    # When no GH_TOKEN is set, the GitHub public quota of 60 requests/hr
    # is insufficient for the per-PR fan-out (3 endpoints × dozens of
    # PRs typically exceeds the quota). Without the offline-fallback
    # flag, the client would silently sleep up to ~58 minutes for the
    # rate-limit window to reset, blocking the pipeline. With the flag
    # enabled, the client raises ``RateLimitExhausted`` after a small
    # cap (120s), which the per-PR loop and the initial _fetch_pulls
    # call both treat as a signal to switch to local-git reconstruction.
    # Authenticated runs (GH_TOKEN set) preserve the prior wait-for-reset
    # behaviour because waiting for an authenticated reset is rare and
    # short, and skipping data is never preferable when a quota refill
    # is plausibly seconds away.
    offline_fallback = gh_token is None
    gh = GithubClient(
        token=gh_token,
        logger=logger,
        offline_fallback=offline_fallback,
    )
    pulls, api_available, api_error = _fetch_pulls(gh, args.repo_slug, logger)
    reviews_by_pr: dict[int, list[dict[str, Any]]] = {}
    events_by_pr: dict[int, list[dict[str, Any]]] = {}
    # Tracks why the API path was abandoned mid-stream so the JSON
    # output can record the degradation in ``github_api.error_reason``.
    api_partial_reason: str | None = None

    if api_available:
        reviews_by_pr, events_by_pr, exhaustion_info = _process_pr_details(
            gh, args.repo_slug, pulls, logger
        )
        if exhaustion_info is not None:
            # The per-PR loop was aborted by the offline-mode escape
            # hatch. Switch to local-git reconstruction so the pipeline
            # produces a complete-shaped pulls.json rather than a
            # partially-hydrated one. The reviews and events maps are
            # discarded because they would be inconsistent with the
            # local-git PRs anyway (which have no reviews/events).
            logger.warning(
                "switching_to_local_git_fallback",
                extra={
                    "event": "switching_to_local_git_fallback",
                    "reason": "rate_limit_exhausted_offline",
                    **exhaustion_info,
                },
            )
            pulls = _fallback_local_git(logger)
            reviews_by_pr = {}
            events_by_pr = {}
            api_available = False
            api_partial_reason = (
                f"rate_limit_exhausted_offline:"
                f"stopped_at_pr_{exhaustion_info['stopped_at_pr']}_"
                f"projected_sleep_{exhaustion_info['projected_sleep_seconds']:.0f}s"
            )
    else:
        # Local-git fallback. The synthetic PRs include the
        # ``_fallback`` marker; reviews and events are not available in
        # local-git so the maps are left empty.
        pulls = _fallback_local_git(logger)

    github_api_block = {
        "available": api_available,
        "endpoint": f"https://api.github.com/repos/{args.repo_slug}/pulls",
        "authenticated": bool(gh_token),
        "error_reason": api_error if api_error else api_partial_reason,
    }

    pulls_payload: dict[str, Any] = {
        "fetched_at": iso_now(),
        "repo_slug": args.repo_slug,
        "github_api": github_api_block,
        "extraction_metadata": extraction_metadata,
        "pulls": pulls,
    }
    reviews_payload: dict[str, Any] = {
        "fetched_at": iso_now(),
        "repo_slug": args.repo_slug,
        "github_api": {
            "available": api_available,
            "endpoint": f"https://api.github.com/repos/{args.repo_slug}/pulls/<n>/reviews",
        },
        "reviews_by_pr": {str(k): v for k, v in reviews_by_pr.items()},
    }
    events_payload: dict[str, Any] = {
        "fetched_at": iso_now(),
        "repo_slug": args.repo_slug,
        "github_api": {
            "available": api_available,
            "endpoint": f"https://api.github.com/repos/{args.repo_slug}/issues/<n>/events",
        },
        "events_by_pr": {str(k): v for k, v in events_by_pr.items()},
    }

    DATA_DIR.mkdir(parents=True, exist_ok=True)
    # Resolve and confine every caller-supplied output path before
    # writing. ``safe_output_path`` rejects any path outside the workspace
    # tree; the rejection surfaces as an :class:`OutputPathError`
    # (ValueError subclass) and exits the script non-zero.
    try:
        pulls_path = safe_output_path(args.pulls_output)
        reviews_path = safe_output_path(args.reviews_output)
        events_path = safe_output_path(args.events_output)
    except OutputPathError as exc:
        logger.error(
            "output_path_rejected",
            extra={
                "pulls_output": args.pulls_output,
                "reviews_output": args.reviews_output,
                "events_output": args.events_output,
                "error": str(exc),
            },
        )
        print(str(exc), file=sys.stderr)
        return 4
    _write_json(pulls_path, pulls_payload)
    _write_json(reviews_path, reviews_payload)
    _write_json(events_path, events_payload)

    # Cursor cleanup. On clean completion we remove the cursor file so
    # the next invocation starts at PR 1 rather than resuming from a
    # stale checkpoint. ``missing_ok=True`` (Python 3.8+) makes the call
    # idempotent.
    try:
        CURSOR_PATH.unlink(missing_ok=True)
    except OSError as exc:
        # Failure to clean up the cursor is non-fatal; log and continue.
        logger.warning(
            "cursor_cleanup_failed",
            extra={
                "event": "cursor_cleanup_failed",
                "cursor_path": str(CURSOR_PATH),
                "error": str(exc),
                "error_type": type(exc).__name__,
            },
        )

    logger.info(
        "script_complete",
        extra={
            "event": "script_complete",
            "pulls_count": len(pulls),
            "reviews_count": sum(len(v) for v in reviews_by_pr.values()),
            "events_count": sum(len(v) for v in events_by_pr.values()),
            "api_available": api_available,
            "fallback_ref": fallback_ref_for_metadata,
        },
    )
    return 0


if __name__ == "__main__":
    # Top-level exception handler. Any uncaught exception in :func:`main`
    # is captured here and logged at error level with the structured
    # logger, then the process exits with code 2 (distinct from 1, which
    # is reserved for the argparse-driven usage errors emitted by the
    # parser itself).
    try:
        _exit_code = main()
    except SystemExit:
        # argparse and explicit ``sys.exit(...)`` calls re-raise SystemExit;
        # let them propagate untouched so CLI usage errors keep their
        # canonical exit code.
        raise
    except Exception:  # noqa: BLE001 — top-level catch-all is intentional
        try:
            _err_logger = get_logger(SCRIPT_NAME)
            _err_logger.exception(
                "script_failed",
                extra={"event": "script_failed"},
            )
        except Exception:  # noqa: BLE001
            # If even the logger fails (e.g. data/ is read-only) we still
            # exit non-zero so the calling shell observes failure.
            pass
        sys.exit(2)
    sys.exit(_exit_code)
