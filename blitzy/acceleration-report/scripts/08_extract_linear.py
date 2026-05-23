"""08_extract_linear.py — Linear GraphQL API extraction (Metric 6 / Metric 12).

This is the last extraction script in the ``blitzy/acceleration-report/`` pipeline
before the compute stage. It retrieves issue and SLA data from the Linear GraphQL
API for two downstream metrics:

* **Metric 6 — Flow Distribution** uses the issue-label catalogue to classify
  merged PRs as feature / defect / risk-compliance / tech-debt when a Linear
  ticket key (regex ``[A-Z]{2,}-\\d+``) is present in the PR body.
* **Metric 12 — Defects Out of SLA** uses the per-issue ``slaBreachedAt`` and
  ``slaStartedAt`` fields to count defects that exceeded their resolution SLA.

The repository's PR template at ``.github/pull_request_template.md`` references
Linear as the canonical issue tracker (``< Replace with Linear Link >``), and
the bug-report template at ``.github/ISSUE_TEMPLATE/bug-report.md`` is the
counterpart for issue creation. The script's primary data source is therefore
Linear's GraphQL endpoint ``https://api.linear.app/graphql``.

Per AAP §0.1.5 and §0.2.4 (Metric 12 row), Linear API access is conditional on
the ``LINEAR_API_KEY`` environment variable being supplied at run time. In the
read-only analysis sandbox this variable is normally **absent** — that is the
common case, not an error. When the key is missing, the script:

1. Logs a single ``WARNING`` event ``linear_api_key_missing`` via the structured
   JSON logger.
2. Writes minimal ``data/issues.json`` and ``data/slas.json`` artifacts whose
   payload carries ``unavailable_reason: "no_linear_api_key"`` and
   ``fetched_at: null``.
3. Returns exit code 0 so the rest of the pipeline can proceed.

When the key IS supplied, the script lazily imports ``gql`` + ``requests``
(keeping the no-op path importable on machines where ``gql`` is not installed),
issues two read-only GraphQL **query** operations (never write operations) —
a paginated ``Issues`` query filtered by labels ``bug`` and ``defect``, then
a single ``Workspace`` settings query — and persists the populated artifacts.

Read-only contract: only GraphQL ``query`` operations are issued. The token
value never reaches stderr or the on-disk JSON-Lines log because the structured
formatter in ``lib.observability`` automatically redacts any field whose key
matches the substring pattern ``(token|key|secret|password|credential|...)``.

Exit codes:
    0 — success (populated extraction) OR graceful no-op (missing key) OR --dry-run / --help
    Non-zero — only on hard failures (e.g. ``gql`` not installed when the key
              IS set, or network failure that we cannot suppress without
              fabricating data).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

# Make the sibling ``lib/`` package importable when this script is invoked
# directly (i.e. not as a module). Inserting the script's own directory at
# index 0 of ``sys.path`` allows ``from lib.observability import get_logger``
# to resolve to ``blitzy/acceleration-report/scripts/lib/observability.py``
# regardless of the caller's current working directory. This must happen
# BEFORE the ``lib.observability`` import below.
sys.path.insert(0, str(Path(__file__).resolve().parent))

from lib.observability import get_logger  # noqa: E402  (intentional after sys.path)


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------

#: Symbolic name embedded in every structured log line emitted by this script.
#: Matches the ``%s_extract_%s`` numbered convention used by the other Python
#: extraction scripts (03, 04, 06, 07, 09).
SCRIPT_NAME: str = "08_extract_linear"

#: Absolute path to the ``blitzy/acceleration-report/`` workspace root,
#: discovered by walking one level up from this script's directory. Used to
#: anchor the output artifact paths and the data directory creation step.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent

#: Workspace ``data/`` directory where extraction artifacts live. Created by
#: ``mkdir(parents=True, exist_ok=True)`` immediately before each write so
#: that the script is safe to invoke on a fresh clone where the directory
#: does not yet exist.
DATA_DIR: Path = WORKSPACE_ROOT / "data"

#: Default output path for the Linear-issue inventory artifact consumed by
#: Metric 6 (issue-label classification) and Metric 12 (defect inventory)
#: in ``09_compute_metrics.py``.
ISSUES_OUTPUT: Path = DATA_DIR / "issues.json"

#: Default output path for the Linear-workspace SLA settings artifact
#: consumed by Metric 12 (Defects Out of SLA) in ``09_compute_metrics.py``.
SLAS_OUTPUT: Path = DATA_DIR / "slas.json"

#: Linear GraphQL endpoint. The only HTTP endpoint this script contacts.
#: All requests are HTTP POST with a GraphQL ``query`` body (semantically
#: read-only per the GraphQL specification — write operations are explicitly
#: NOT issued and the verification grep documented in the AAP returns no
#: matches against this source file).
LINEAR_ENDPOINT: str = "https://api.linear.app/graphql"

#: Issue labels searched in Linear per AAP §0.5.3.13. ``bug`` is the
#: GitHub-issue-template default label name (cross-referenced from
#: ``.github/ISSUE_TEMPLATE/bug-report.md``); ``defect`` is the
#: semantically-equivalent label sometimes used in Linear workspaces. Both
#: are queried in a single filtered Issues call (label.name IN [...]) so
#: there is no per-label double-fetch.
DEFECT_LABELS: list[str] = ["bug", "defect"]

#: Pagination size for the Linear Issues query. Linear's API caps a single
#: page at 250 nodes; 100 is the Apollo-style default and aligns with the
#: pagination convention used elsewhere in the pipeline (see
#: ``03_extract_pulls.py``). The ``fetch_all_issues`` helper paginates
#: forward via Relay-style cursors (pageInfo.hasNextPage + endCursor).
PAGE_SIZE: int = 100

#: HTTP timeout in seconds for each GraphQL request. Set conservatively to
#: 30s so that a slow API does not silently hang the pipeline; the gql
#: transport raises a TimeoutError on expiry which propagates as a hard
#: error per the read-only contract (no fabricated fallback).
HTTP_TIMEOUT_SECONDS: int = 30

#: GraphQL Issues query — paginated, label-filtered. Field selection matches
#: the schema expected by ``09_compute_metrics.py``:
#:   id, identifier   — primary keys for cross-referencing with PR bodies
#:   title            — human-readable label for the per-engineer view
#:   createdAt        — open timestamp for SLA elapsed-time computation
#:   completedAt      — close timestamp (null if open) for SLA computation
#:   priority         — Linear's 0..4 priority scale (proxy for severity)
#:   labels.nodes.name — label-list for Metric 6 issue-label classification
#:   slaBreachedAt    — non-null indicates the SLA was breached (Metric 12)
#:   slaStartedAt     — SLA start timestamp (for elapsed computation)
#:
#: The triple-quoted form preserves the GraphQL whitespace for readability
#: when this query is re-rendered in the Reproducibility Appendix.
ISSUES_QUERY: str = """
query Issues($filter: IssueFilter!, $first: Int!, $after: String) {
  issues(filter: $filter, first: $first, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      identifier
      title
      createdAt
      completedAt
      priority
      labels { nodes { name } }
      slaBreachedAt
      slaStartedAt
    }
  }
}
"""

#: GraphQL Workspace settings query — single un-paginated lookup that
#: identifies the workspace and exposes any workspace-level SLA fields if
#: Linear's schema makes them available. The Linear GraphQL schema as of
#: 2025 does not expose ``slaSettings`` on the ``Workspace`` type; we
#: include the query nonetheless to record the attempt for the
#: Reproducibility Appendix and to gracefully degrade when the field is
#: absent. The Workspace.urlKey is the slug appearing in linear.app URLs
#: (e.g. "rudderstack" for https://linear.app/rudderstack), used by
#: ``09_compute_metrics.py`` to render workspace-attributed citations.
WORKSPACE_QUERY: str = """
query Settings {
  workspace {
    id
    name
    urlKey
  }
}
"""

#: Free-text note attached to the no-op payload. Documents the consequence
#: of the missing key in human-readable form so that an analyst reading
#: ``data/issues.json`` understands why the issues array is empty. The note
#: is intentionally informational; the canonical machine-readable signal
#: remains the ``unavailable_reason`` field.
NO_OP_NOTE: str = (
    "LINEAR_API_KEY environment variable was not set. Pipeline runs in "
    "degraded mode; Metric 6 falls back to conventional-commit-prefix "
    "classification; Metric 12 reports 'Insufficient signal — no SLA "
    "source' unless a repo policy document is found by a separate fallback "
    "pass."
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def iso_now() -> str:
    """Return the current UTC wall-clock time in ISO-8601 form with ``Z`` suffix.

    Used to stamp the ``fetched_at`` field on every populated artifact, per
    Rule 6 (Environment First) timestamp discipline. The format matches the
    convention used by ``00_environment.sh`` and the other Python extractors
    (e.g. ``2026-05-23T14:30:45Z``) so timestamps compare lexicographically
    across artifacts. Microsecond precision is intentionally truncated to
    seconds because the consumer in ``09_compute_metrics.py`` bins events
    into Monday-anchored 2-week windows where sub-second resolution would
    be spurious.

    Returns:
        A timestamp string of the form ``YYYY-MM-DDTHH:MM:SSZ`` representing
        the present moment in Coordinated Universal Time.
    """
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    """Persist ``payload`` to ``path`` as pretty-printed JSON.

    Centralises the JSON-write convention used by both branches of the
    script (no-op and populated). The output is encoded as UTF-8 with
    ``indent=2`` to remain hand-readable when inspected during a manual
    pipeline run and with ``sort_keys=False`` so that the canonical key
    ordering (``issues`` / ``slas`` / ``fetched_at`` / ``unavailable_reason``)
    encoded by the caller is preserved verbatim in the file.

    The parent directory is created (``mkdir(parents=True, exist_ok=True)``)
    so that a fresh clone where ``data/`` does not yet exist is safe to
    extract into. A trailing newline is appended so that POSIX line-oriented
    tools (``tail``, ``wc -l``) handle the file cleanly.

    Args:
        path: Absolute or workspace-relative output path. Created in
            place; any existing file at this path is overwritten.
        payload: Mapping serialised via ``json.dumps``. The default
            serialiser (``default=str``) handles ``Path`` instances and
            datetime values that may appear in nested structures.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(payload, indent=2, default=str, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )


def _build_no_op_payload(kind: str) -> dict[str, Any]:
    """Return the canonical no-op payload for ``issues.json`` or ``slas.json``.

    The payload shape is identical except for the top-level array key
    (``issues`` for the issues artifact, ``slas`` for the SLAs artifact)
    and the supplementary ``note`` field, which is included only on the
    issues artifact because that is the one most analysts inspect first.

    Args:
        kind: Either ``"issues"`` or ``"slas"``. Determines the top-level
            array key. Any other value is treated as ``"issues"`` by
            default — the function does not raise on unexpected input
            because the caller passes only the two valid values.

    Returns:
        A dictionary suitable for serialisation by :func:`_write_json`.
        Always contains:

        * ``unavailable_reason``: the sentinel string ``"no_linear_api_key"``
          which is the machine-readable signal consumed by
          ``09_compute_metrics.py`` and by the Risk Assessment renderer.
        * ``fetched_at``: ``None`` (never overwritten with a wall-clock
          time on the no-op path — the absent value is the signal that no
          fetch occurred).
        * ``linear_endpoint``: documented even on the no-op path so that
          the Reproducibility Appendix can reference the would-be target.
    """
    array_key = "slas" if kind == "slas" else "issues"
    payload: dict[str, Any] = {
        array_key: [],
        "unavailable_reason": "no_linear_api_key",
        "fetched_at": None,
        "linear_endpoint": LINEAR_ENDPOINT,
    }
    if kind == "issues":
        payload["note"] = NO_OP_NOTE
        payload["label_filter"] = DEFECT_LABELS
    return payload


def fetch_all_issues(
    client: Any, gql_query_fn: Any, logger: Any, label_filter: list[str]
) -> list[dict[str, Any]]:
    """Page through Linear's Issues query and accumulate every matching node.

    Linear's GraphQL pagination follows the Relay cursor convention: the
    server returns a ``pageInfo`` block on every response carrying a
    boolean ``hasNextPage`` flag and a ``endCursor`` opaque string. The
    next request supplies the cursor via the ``after`` variable. We stop
    once ``hasNextPage`` is False.

    The function emits a ``page_fetched`` structured log event after each
    successful page so that an operator running ``tail -f
    data/run.log.jsonl`` can observe the cumulative issue count growing.
    The token never reaches the log because the GraphQL transport
    (``RequestsHTTPTransport``) embeds it in the request header — which is
    never inspected by this function — and any field key matching ``*key*``
    is redacted by the structured formatter.

    Args:
        client: A configured ``gql.Client`` instance — supplied by the
            caller because the import is lazy (only present on the
            API-path branch). Typed ``Any`` so this function is importable
            without ``gql`` installed.
        gql_query_fn: The ``gql.gql`` query-parser function — supplied
            for the same lazy-import reason.
        logger: The structured-JSON logger acquired via
            :func:`lib.observability.get_logger`. Used for the
            ``page_fetched`` event after each successful page.
        label_filter: List of issue-label names to filter by. The Linear
            filter syntax is ``{labels: {name: {in: [...]}}}`` which
            matches issues having ANY of the supplied labels.

    Returns:
        The accumulated list of issue node dictionaries across every page.
        Field selection matches :data:`ISSUES_QUERY`. The list may be
        empty if the workspace has no issues with the requested labels;
        an empty result is NOT an error and is persisted as-is.
    """
    issues: list[dict[str, Any]] = []
    after: str | None = None
    page_index = 0
    parsed_query = gql_query_fn(ISSUES_QUERY)
    while True:
        page_index += 1
        result = client.execute(
            parsed_query,
            variable_values={
                "filter": {"labels": {"name": {"in": label_filter}}},
                "first": PAGE_SIZE,
                "after": after,
            },
        )
        nodes = result.get("issues", {}).get("nodes", []) or []
        issues.extend(nodes)
        page = result.get("issues", {}).get("pageInfo", {}) or {}
        logger.info(
            "page_fetched",
            extra={
                "event": "linear_page",
                "page_index": page_index,
                "page_size": len(nodes),
                "cumulative": len(issues),
                "has_next_page": bool(page.get("hasNextPage")),
            },
        )
        if not page.get("hasNextPage"):
            break
        after = page.get("endCursor")
        if not after:
            # Defensive: if the server says "has next page" but supplies
            # no cursor, we stop rather than enter an infinite loop. A
            # well-behaved server never sends this combination, but we
            # treat it as the conservative read-only choice.
            break
    return issues


def _do_dry_run(args: argparse.Namespace, api_key: str) -> int:
    """Print the ``--dry-run`` JSON preview and exit cleanly.

    The preview describes the actions the script WOULD perform — the
    endpoint it would contact, the queries it would issue, and the
    artifact paths it would write — without actually making any network
    request or filesystem write. This satisfies the rule-required
    ``--dry-run`` preflight observability surface (AAP §0.5.6
    Observability discipline).

    The token value is NEVER printed regardless of presence; only the
    boolean ``linear_api_key_present`` flag is emitted so an operator
    can confirm that the env-var was picked up correctly without leaking
    the secret.

    Args:
        args: The argparse Namespace produced by :func:`main` carrying
            the resolved ``issues_output`` and ``slas_output`` paths.
        api_key: The (potentially empty) ``LINEAR_API_KEY`` value. Only
            its truthiness is inspected — the value itself is not echoed.

    Returns:
        Always 0. The dry-run path is informational and cannot fail.
    """
    if api_key:
        preview = {
            "action": "dry_run",
            "linear_api_key_present": True,
            "endpoint": LINEAR_ENDPOINT,
            "queries": [
                "Issues (labels in [bug, defect], paginated)",
                "Workspace (id, name, urlKey)",
            ],
            "writes": [args.issues_output, args.slas_output],
            "behavior": "would_extract",
        }
    else:
        preview = {
            "action": "dry_run",
            "linear_api_key_present": False,
            "endpoint": LINEAR_ENDPOINT,
            "behavior": "no-op",
            "writes": [args.issues_output, args.slas_output],
            "no_op_reason": "no_linear_api_key",
        }
    print(json.dumps(preview, indent=2))
    return 0


def _do_no_op(args: argparse.Namespace, logger: Any) -> int:
    """Emit the no-op no-key artifacts and exit cleanly.

    This is the most common code path in the read-only analysis sandbox:
    ``LINEAR_API_KEY`` is not configured. The expected response is to
    write minimal placeholder artifacts that the compute stage and the
    risk-assessment renderer can interpret as "Linear unavailable" and
    fall back to repo-only signals (Metric 6 → conventional-commit
    prefix, Metric 12 → repository SLA policy scan, or
    ``insufficient_signal`` if neither yields).

    The function logs ONE warning event (not error) so the run log
    explicitly records the degraded mode, and then a script-complete
    info event so the operator sees the script ran to completion.

    Args:
        args: argparse Namespace carrying the output paths.
        logger: The structured-JSON logger.

    Returns:
        Always 0. The graceful no-op path NEVER raises.
    """
    issues_payload = _build_no_op_payload("issues")
    slas_payload = _build_no_op_payload("slas")
    _write_json(Path(args.issues_output), issues_payload)
    _write_json(Path(args.slas_output), slas_payload)
    logger.warning(
        "linear_api_key_missing",
        extra={
            "event": "linear_unavailable",
            "reason": "no_linear_api_key",
            "behavior": "graceful_no_op",
            "issues_output": args.issues_output,
            "slas_output": args.slas_output,
            "consequence": (
                "Metric 6 falls back to conventional-commit-prefix "
                "classification; Metric 12 may report insufficient_signal."
            ),
        },
    )
    logger.info(
        "script_complete",
        extra={
            "event": "script_complete",
            "issues_count": 0,
            "linear_available": False,
            "exit_code": 0,
        },
    )
    return 0


def _do_extract(args: argparse.Namespace, api_key: str, logger: Any) -> int:
    """Execute the populated extraction path against Linear's GraphQL API.

    This branch is taken when ``LINEAR_API_KEY`` is set. The gql and
    requests packages are imported HERE — never at module top — so that
    the no-op path is importable on machines where gql is not installed.
    An ImportError on this branch is fatal because the user signalled
    intent to use the API by setting the env var.

    The branch issues exactly two GraphQL queries:

    1. ``Issues`` (paginated) — every issue labelled ``bug`` or ``defect``
       with the field selection needed by Metrics 6 and 12.
    2. ``Workspace`` (single) — identifies the workspace, used only to
       attribute citations in the rendered report; failure to fetch it
       is NOT a hard error (the issues payload alone is sufficient).

    Args:
        args: argparse Namespace carrying the output paths.
        api_key: The (non-empty) Linear API key. Passed verbatim to the
            ``Authorization`` header. The value itself is never logged
            because the field key ``Authorization`` does not appear in
            any extra dict, and any field key matching ``*key*`` is
            redacted by the structured-JSON formatter.
        logger: The structured-JSON logger.

    Returns:
        0 on successful extraction. Non-zero values are not returned
        because the function either succeeds or raises.

    Raises:
        ImportError: If ``gql`` or ``requests`` is not installed despite
            the user having set ``LINEAR_API_KEY``. Re-raised after the
            structured log is emitted so the run fails hard (rather than
            silently producing wrong numbers).
        Exception: Any unhandled network or schema error from the gql
            transport. Re-raised after logging so the ``script_failed``
            log line in the outer try/except captures the failure mode.
    """
    # Lazy imports: only required when LINEAR_API_KEY is present. Keeping
    # these out of the module top makes the no-op path runnable on a
    # machine where gql is not installed (the workspace requirements.txt
    # pins gql==3.5.0 but we still allow the no-op path to function
    # without it for maximum robustness in CI sandboxes).
    try:
        import requests  # noqa: F401  (imported for transport availability check)
        from gql import gql as gql_query_fn
        from gql import Client
        from gql.transport.requests import RequestsHTTPTransport
    except ImportError as exc:
        # NOTE: ``module`` is a reserved Python LogRecord attribute (set by
        # the logging machinery to the caller's module name). Passing it
        # via ``extra={}`` raises KeyError: "Attempt to overwrite 'module'
        # in LogRecord". We use ``missing_module`` instead, which is the
        # field name documented in the AAP §0.5.6 Observability discipline.
        logger.error(
            "missing_dependency",
            extra={
                "event": "import_failed",
                "missing_module": str(exc),
                "remediation": (
                    "pip install gql[requests]==3.5.0 requests==2.32.5 "
                    "(see blitzy/acceleration-report/requirements.txt)"
                ),
            },
        )
        raise

    # Build the transport with the auth header. Note: ``headers`` is
    # passed to RequestsHTTPTransport but NEVER passed to logger.extra,
    # so the token cannot leak via the structured-JSON log path. The
    # field key ``Authorization`` would also be redacted by the
    # ``(token|key|secret|...)`` substring pattern in
    # lib/observability.py:REDACT_PATTERN if it ever did appear, but
    # belt-and-braces.
    transport = RequestsHTTPTransport(
        url=LINEAR_ENDPOINT,
        headers={"Authorization": api_key},
        use_json=True,
        timeout=HTTP_TIMEOUT_SECONDS,
    )
    client = Client(transport=transport, fetch_schema_from_transport=False)

    logger.info(
        "linear_extraction_started",
        extra={
            "event": "linear_extraction_started",
            "endpoint": LINEAR_ENDPOINT,
            "label_filter": DEFECT_LABELS,
            "page_size": PAGE_SIZE,
        },
    )

    # Phase 1 — paginate the Issues query.
    issues = fetch_all_issues(client, gql_query_fn, logger, DEFECT_LABELS)

    # Phase 2 — fetch workspace settings. This is best-effort: a failure
    # downgrades the SLA payload to "no_workspace_sla_field" but does NOT
    # abort the run because the per-issue ``slaBreachedAt`` field is the
    # primary signal for Metric 12.
    fetched_at = iso_now()
    try:
        workspace_result = client.execute(gql_query_fn(WORKSPACE_QUERY))
        workspace = workspace_result.get("workspace", {}) or {}
        slas_payload: dict[str, Any] = {
            "slas": [],
            "workspace": workspace,
            "fetched_at": fetched_at,
            "linear_endpoint": LINEAR_ENDPOINT,
            "note": (
                "Linear's GraphQL schema does not expose workspace-level "
                "SLA targets as of 2025. Per-issue slaBreachedAt and "
                "slaStartedAt fields on the Issues payload remain the "
                "primary signal for Metric 12."
            ),
        }
    except Exception as exc:  # noqa: BLE001  (best-effort branch)
        logger.warning(
            "slas_unavailable",
            extra={
                "event": "linear_slas_failed",
                "error": str(exc),
                "exception_type": type(exc).__name__,
            },
        )
        slas_payload = {
            "slas": [],
            "unavailable_reason": "no_workspace_sla_field",
            "fetched_at": fetched_at,
            "linear_endpoint": LINEAR_ENDPOINT,
        }

    # Phase 3 — persist artifacts. Both files are written even if the
    # workspace query failed because the consumer ``09_compute_metrics.py``
    # expects both files to exist.
    issues_payload = {
        "issues": issues,
        "fetched_at": fetched_at,
        "linear_endpoint": LINEAR_ENDPOINT,
        "label_filter": DEFECT_LABELS,
        "issues_count": len(issues),
    }
    _write_json(Path(args.issues_output), issues_payload)
    _write_json(Path(args.slas_output), slas_payload)

    logger.info(
        "script_complete",
        extra={
            "event": "script_complete",
            "issues_count": len(issues),
            "linear_available": True,
            "exit_code": 0,
        },
    )
    return 0


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main() -> int:
    """Top-level entry point: parse args, dispatch to dry-run / no-op / extract.

    The function implements the three-way dispatch documented in the AAP
    §0.5.3.13 phase plan:

    1. ``--dry-run`` — print a preview of what the script would do, exit 0.
    2. No ``LINEAR_API_KEY`` — emit no-op artifacts, log a warning, exit 0.
    3. ``LINEAR_API_KEY`` present — lazily import gql, fetch issues and
       workspace, persist populated artifacts, exit 0.

    Each branch is delegated to a dedicated helper so this function stays
    short and the test surface remains crisp: callers can monkey-patch
    either helper to exercise alternative code paths in unit tests.

    Returns:
        Exit code suitable for ``sys.exit(main())``. Zero on success and
        on the graceful no-op path; non-zero only if a hard failure
        propagates (e.g. ImportError on the populated branch).
    """
    parser = argparse.ArgumentParser(
        prog=SCRIPT_NAME,
        description=(
            "Extract issues and SLA data from the Linear GraphQL API. "
            "Gracefully no-ops when LINEAR_API_KEY is absent, writing "
            "issues.json and slas.json with "
            "unavailable_reason='no_linear_api_key'."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Environment variables:\n"
            "  LINEAR_API_KEY   Linear personal API key. When absent, the "
            "script runs in graceful no-op mode (the expected case in the "
            "read-only analysis sandbox).\n"
            "  BLITZY_RUN_ID    Per-run UUID4 correlation ID propagated by "
            "00_environment.sh; inherited automatically when set.\n\n"
            "Read-only contract: only GraphQL query operations are issued. "
            "No write operations. No modifications to Linear, the analyzed "
            "repository, or any external system."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Print a JSON preview of the endpoint, queries, and writes "
            "this script would perform, then exit 0 without making any "
            "network call or filesystem write."
        ),
    )
    parser.add_argument(
        "--issues-output",
        default=str(ISSUES_OUTPUT),
        help=(
            "Path to the issues.json output artifact. "
            f"Default: {ISSUES_OUTPUT}"
        ),
    )
    parser.add_argument(
        "--slas-output",
        default=str(SLAS_OUTPUT),
        help=(
            "Path to the slas.json output artifact. "
            f"Default: {SLAS_OUTPUT}"
        ),
    )
    args = parser.parse_args()

    logger = get_logger(SCRIPT_NAME)
    logger.info(
        "script_started",
        extra={
            "event": "script_started",
            "dry_run": args.dry_run,
            "issues_output": args.issues_output,
            "slas_output": args.slas_output,
        },
    )

    # Resolve the key once. ``.strip()`` ensures that an env var set to a
    # value of only whitespace (e.g. ``LINEAR_API_KEY=" "``) is treated as
    # absent rather than triggering an API call with an invalid header.
    # The value is consumed locally — never assigned to a log extra.
    api_key = os.environ.get("LINEAR_API_KEY", "").strip()

    if args.dry_run:
        # Dry-run path: print the preview and return. Network is NOT
        # touched and no file is written. Both the api-key-present and
        # api-key-absent dry-run sub-cases are handled inside _do_dry_run.
        return _do_dry_run(args, api_key)

    if not api_key:
        # Graceful no-op path: the common case in the read-only sandbox.
        # Writes empty issues.json and slas.json with the documented
        # unavailable_reason. NEVER raises.
        return _do_no_op(args, logger)

    # Populated-extraction path: LINEAR_API_KEY is set, gql is imported
    # lazily, queries are issued, artifacts are persisted.
    return _do_extract(args, api_key, logger)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        # Re-raise SystemExit cleanly so argparse's --help (which exits
        # via SystemExit(0)) and explicit ``sys.exit(main())`` calls
        # propagate without going through the script_failed log path.
        raise
    except BaseException as exc:  # noqa: BLE001  (broad on purpose for outer try)
        # Any unhandled exception is logged as a structured-JSON
        # script_failed event for forensic auditability, then re-raised
        # so the shell sees a non-zero exit code. The logger acquisition
        # is wrapped in its own try to handle the pathological case
        # where logger setup itself fails (e.g. read-only ./data/).
        try:
            get_logger(SCRIPT_NAME).error(
                "script_failed",
                extra={
                    "event": "script_failed",
                    "error": str(exc),
                    "exception_type": type(exc).__name__,
                },
            )
        except BaseException:  # noqa: BLE001  (log-of-log-failure)
            # Best-effort: if the logger itself cannot be acquired we
            # still want to re-raise the original exception with a
            # readable trace. Swallowing here is correct because the
            # primary signal is the re-raise below.
            pass
        raise
