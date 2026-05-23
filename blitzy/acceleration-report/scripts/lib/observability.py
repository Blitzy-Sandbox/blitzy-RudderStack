"""Structured-JSON logger for the acceleration-report extraction pipeline.

This module is the single observability primitive consumed by every Python
script under ``blitzy/acceleration-report/scripts/`` and the canonical sink
for JSON-line log events emitted from the surrounding Bash extraction scripts
when they pipe their stderr through to ``data/run.log.jsonl``. The module is
deliberately pure-standard-library so that it can be imported from any Python
3.12+ interpreter without invoking ``pip``. It provides a per-process logger
factory keyed by script name, a UUID4 correlation identifier propagated across
process boundaries through the ``BLITZY_RUN_ID`` environment variable, a JSON
formatter that emits one self-contained event per line to both standard error
and the on-disk ``data/run.log.jsonl`` file, automatic redaction of fields
whose names or values resemble API tokens or other credential material, and
two convenience helpers (``log_data_source`` and ``log_metric_extracted``)
whose sentinel event names are the audit anchors referenced by the Quality
Gates section of the Agent Action Plan.
"""

from __future__ import annotations

import json
import logging
import os
import re
import sys
import threading
import uuid
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterator


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------

#: Name of the environment variable that carries the per-run correlation ID
#: across Bash and Python process boundaries throughout the pipeline.
BLITZY_RUN_ID_ENV: str = "BLITZY_RUN_ID"

#: Filename of the JSON-Lines log appended to by every script in the pipeline.
LOG_FILE_NAME: str = "run.log.jsonl"

#: Name of the data directory under the workspace root where the log file
#: lives and where extraction-script artifacts are persisted.
DATA_DIR_NAME: str = "data"

#: Heuristic marker directories used to identify the workspace root by walking
#: up the filesystem from the calling script's location. A directory that
#: contains at least two of these as direct children is treated as the
#: workspace root.
WORKSPACE_MARKER_DIRS: tuple[str, ...] = ("data", "scripts", "diagrams")

#: Case-insensitive substring match on FIELD NAMES that should be redacted.
#: This catches the most common credential-bearing field names without being
#: so permissive that it redacts legitimate fields. The leading ``.*`` is
#: redundant for ``.search()`` but preserved here verbatim for traceability
#: against the Agent Action Plan.
REDACT_PATTERN: re.Pattern[str] = re.compile(
    r"(?i).*(token|key|secret|password|credential|bearer|auth)"
)

#: Anchored pattern on FIELD VALUES that catches known token formats even
#: when the surrounding field name does not look like a credential field.
#: Covers GitHub Personal Access Tokens (``ghp_``), OAuth (``gho_``), user
#: tokens (``ghu_``), server tokens (``ghs_``), and refresh tokens (``ghr_``),
#: plus the Linear API ``lin_api_`` prefix. The 20-or-more-character suffix
#: discriminates between the real tokens and legitimate strings that happen
#: to start with these letters.
REDACT_VALUE_PATTERN: re.Pattern[str] = re.compile(
    r"^(ghp_|gho_|ghu_|ghs_|ghr_|lin_api_)[A-Za-z0-9_]{20,}$"
)

#: String emitted in place of any redacted field value.
REDACTED_PLACEHOLDER: str = "***REDACTED***"

#: Standard ``LogRecord`` attribute names that must be excluded from the
#: ``extra_fields`` dictionary computed in :class:`JsonFormatter`. Anything in
#: ``record.__dict__`` that is NOT in this set is treated as a user-supplied
#: extra field (i.e. passed via ``extra={...}`` on a logger call).
#:
#: The set includes the standard attributes documented for Python 3.12 plus
#: ``taskName`` which was added in Python 3.12 for asyncio task names and the
#: ``message`` attribute that is set by ``LogRecord.getMessage()``.
_RESERVED_RECORD_ATTRS: frozenset[str] = frozenset(
    {
        "args",
        "asctime",
        "created",
        "exc_info",
        "exc_text",
        "filename",
        "funcName",
        "levelname",
        "levelno",
        "lineno",
        "message",
        "module",
        "msecs",
        "msg",
        "name",
        "pathname",
        "process",
        "processName",
        "relativeCreated",
        "stack_info",
        "taskName",
        "thread",
        "threadName",
    }
)

#: Canonical fields written by :meth:`JsonFormatter.format` that must not be
#: clobbered by ``extra={...}`` keyword arguments on logger calls.
_CANONICAL_FIELDS: frozenset[str] = frozenset(
    {"run_id", "ts", "script", "level", "event"}
)

#: Lock protecting :data:`_LOGGER_CACHE` mutation during the double-checked
#: locking pattern in :func:`get_logger`. Ensures that even under concurrent
#: invocation from multiple threads, a given ``script_name`` is associated
#: with exactly one ``logging.Logger`` instance with one set of handlers.
_FILE_HANDLER_LOCK: threading.Lock = threading.Lock()

#: Per-script-name singleton cache of configured loggers. Re-acquiring a
#: logger for the same name returns the cached instance, preventing duplicate
#: handler attachment that would otherwise cause every log line to be emitted
#: N times.
_LOGGER_CACHE: dict[str, logging.Logger] = {}

#: Module-level flag that ensures the workspace-root discovery warning is
#: emitted at most once per process lifetime, preventing log spam if many
#: scripts call :func:`get_logger` from a working directory outside the
#: workspace tree.
_WORKSPACE_WARNING_EMITTED: bool = False

#: Module-level flag protecting :data:`_WORKSPACE_WARNING_EMITTED`.
_WORKSPACE_WARNING_LOCK: threading.Lock = threading.Lock()

#: Maximum number of ancestor directories to inspect when discovering the
#: workspace root. Six is chosen as a balance between robustness (covering
#: ``scripts/lib/`` invocation, ``scripts/`` invocation, and ``acceleration-
#: report/`` invocation) and bounded cost (no walk to filesystem root).
_WORKSPACE_DISCOVERY_DEPTH: int = 6


# ---------------------------------------------------------------------------
# Run-ID provisioning
# ---------------------------------------------------------------------------


def _ensure_run_id() -> str:
    """Return a stable UUID4 correlation identifier for the current process.

    The function reads the ``BLITZY_RUN_ID`` environment variable. If the
    value is present and parses as a version-4 UUID, it is returned as-is.
    Otherwise a fresh UUID4 is generated, written back to ``os.environ`` so
    that any child process inherits the same identifier, and returned.

    This idempotent contract is what makes the pipeline's log lines
    correlate-able: ``00_environment.sh`` sets ``BLITZY_RUN_ID`` once, every
    subsequent script — Bash or Python — sees the same value, and every
    ``run_id`` field in ``data/run.log.jsonl`` carries that identifier.

    Returns:
        The active UUID4 string for the current run, e.g.
        ``"3f8d2a4c-1b6e-4f25-9d8a-c1e7b3a5f0d2"``.
    """
    existing = os.environ.get(BLITZY_RUN_ID_ENV)
    if existing:
        try:
            parsed = uuid.UUID(existing)
            if parsed.version == 4:
                return existing
        except (ValueError, AttributeError, TypeError):
            # Malformed value — fall through to regenerate.
            pass
    new_id = str(uuid.uuid4())
    os.environ[BLITZY_RUN_ID_ENV] = new_id
    return new_id


def get_run_id() -> str:
    """Public accessor for the current run's correlation identifier.

    This is a stable alias for :func:`_ensure_run_id` exposed for scripts
    that want to embed the run ID in artifact filenames, HTTP headers, or
    other observability surfaces outside the logger itself.

    Returns:
        The active UUID4 string for the current run.
    """
    return _ensure_run_id()


# ---------------------------------------------------------------------------
# Workspace-root discovery
# ---------------------------------------------------------------------------


def _discover_workspace_root(start: Path | None = None) -> Path:
    """Locate the ``blitzy/acceleration-report/`` workspace root.

    Walks up from ``start`` (default: :func:`Path.cwd`) checking each
    ancestor for the presence of at least two directories from
    :data:`WORKSPACE_MARKER_DIRS` as direct children. The first ancestor
    satisfying the threshold is returned. The search depth is bounded by
    :data:`_WORKSPACE_DISCOVERY_DEPTH` to avoid walking to the filesystem
    root in pathological cases.

    If no matching ancestor is found, the function emits a one-time warning
    to ``sys.stderr`` (NOT through the logger, to avoid recursion during
    handler initialisation) and returns ``Path.cwd()`` so that callers can
    still proceed using a relative ``./data/run.log.jsonl`` path.

    Args:
        start: Directory from which to begin the upward walk. ``None`` means
            ``Path.cwd()``.

    Returns:
        The discovered workspace root, or ``Path.cwd()`` as a fallback.
    """
    candidate = (start if start is not None else Path.cwd()).resolve()
    for _ in range(_WORKSPACE_DISCOVERY_DEPTH + 1):
        marker_hits = sum(
            1 for marker in WORKSPACE_MARKER_DIRS if (candidate / marker).is_dir()
        )
        if marker_hits >= 2:
            return candidate
        parent = candidate.parent
        if parent == candidate:
            # Reached filesystem root.
            break
        candidate = parent
    _warn_workspace_not_found(start)
    return Path.cwd().resolve()


def _warn_workspace_not_found(start: Path | None) -> None:
    """Emit a one-time stderr warning when workspace discovery fails.

    Direct ``sys.stderr`` writes are used here instead of the logger to
    prevent recursion: this function may be called during logger
    initialisation, before any handlers are attached.

    Args:
        start: The starting directory passed to the discovery routine, used
            only for the warning message.
    """
    global _WORKSPACE_WARNING_EMITTED
    with _WORKSPACE_WARNING_LOCK:
        if _WORKSPACE_WARNING_EMITTED:
            return
        _WORKSPACE_WARNING_EMITTED = True
    msg = (
        "[observability] workspace root not found from "
        f"{start or Path.cwd()!s}; falling back to cwd "
        f"{Path.cwd().resolve()!s}. Log file will be written to "
        f"./{DATA_DIR_NAME}/{LOG_FILE_NAME} relative to the current directory.\n"
    )
    try:
        sys.stderr.write(msg)
        sys.stderr.flush()
    except (OSError, ValueError):
        # Best-effort warning — if stderr itself is closed, swallow.
        pass


def _log_file_path() -> Path:
    """Return the absolute path to the JSON-Lines run log file.

    The path is composed as ``<workspace_root>/data/run.log.jsonl`` where
    ``workspace_root`` is the result of :func:`_discover_workspace_root`.

    Returns:
        Absolute path to the run log file. The parent directory is NOT
        created by this function; callers (specifically the file-handler
        creation block in :func:`get_logger`) are responsible for that.
    """
    return _discover_workspace_root() / DATA_DIR_NAME / LOG_FILE_NAME


# ---------------------------------------------------------------------------
# Secret redaction
# ---------------------------------------------------------------------------


def _redact_fields(payload: dict[str, Any]) -> dict[str, Any]:
    """Return a deep copy of ``payload`` with credential-bearing values masked.

    The walk is fully recursive: nested dictionaries are descended into,
    and lists are walked element-wise. The function is non-mutating — the
    input is left intact and a new structure is returned. This is important
    because the caller (:meth:`JsonFormatter.format`) reads from the live
    ``LogRecord.__dict__`` and must not corrupt the record.

    Redaction policy:

    1. If the key (case-insensitively) matches :data:`REDACT_PATTERN`, the
       associated value is replaced with :data:`REDACTED_PLACEHOLDER`
       regardless of its content or type.
    2. Otherwise, if the value is a string matching
       :data:`REDACT_VALUE_PATTERN`, it is replaced with
       :data:`REDACTED_PLACEHOLDER`. This catches the common case of a
       credential being passed under a benign field name.
    3. Otherwise, if the value is a ``dict``, this function recurses.
    4. Otherwise, if the value is a ``list`` or ``tuple``, each element is
       passed through a per-element recursion that treats dicts the same way.
    5. Otherwise, the value is preserved as-is.

    Args:
        payload: Mapping of field name to field value.

    Returns:
        A new dictionary with the same shape as ``payload`` but with any
        credential-bearing values replaced by :data:`REDACTED_PLACEHOLDER`.
    """
    redacted: dict[str, Any] = {}
    for key, value in payload.items():
        key_str = str(key)
        if REDACT_PATTERN.search(key_str) is not None:
            redacted[key_str] = REDACTED_PLACEHOLDER
            continue
        redacted[key_str] = _redact_value(value)
    return redacted


def _redact_value(value: Any) -> Any:
    """Apply value-side redaction policy to a single value.

    Splits the value handling into a separate helper so that both dict
    values (handled by :func:`_redact_fields`) and list elements share the
    same logic without duplicating the recursion.

    Args:
        value: The value to inspect and potentially redact.

    Returns:
        Either the redacted placeholder string, a recursively-redacted
        container, or the input value unchanged.
    """
    if isinstance(value, str):
        if REDACT_VALUE_PATTERN.match(value) is not None:
            return REDACTED_PLACEHOLDER
        return value
    if isinstance(value, dict):
        return _redact_fields(value)
    if isinstance(value, list):
        return [_redact_value(item) for item in value]
    if isinstance(value, tuple):
        return tuple(_redact_value(item) for item in value)
    return value


# ---------------------------------------------------------------------------
# JSON formatter
# ---------------------------------------------------------------------------


class JsonFormatter(logging.Formatter):
    """Formatter that renders ``LogRecord`` instances as single-line JSON.

    The output is a JSON object on a single line, terminated by the newline
    that :class:`logging.StreamHandler` and :class:`logging.FileHandler` add
    automatically. The canonical fields appear first in this fixed order for
    human readability when reading the log file directly:

    * ``run_id`` — per-run UUID4 correlation identifier (constant across the
      entire run, identical to the value emitted by Bash extraction scripts).
    * ``ts`` — UTC timestamp in ISO-8601 form with microsecond precision and
      a trailing ``Z`` to denote zero offset.
    * ``script`` — logger name, typically the calling script's module name.
    * ``level`` — Python log level name (``INFO``, ``WARNING``, ``ERROR``,
      etc.) preserved with uppercase casing.
    * ``event`` — the log message string passed as the first positional
      argument to the logger call.

    User-supplied ``extra={...}`` fields are merged after these canonical
    fields. Canonical fields are NEVER overwritten by extras — if a caller
    accidentally passes ``extra={"run_id": "x"}``, the canonical run_id is
    preserved. This makes the run_id field a reliable correlation anchor.

    Exception information attached to the record via ``exc_info=True`` or
    ``logger.exception(...)`` is rendered into a multi-line ``exception``
    string field via :meth:`Formatter.formatException`.
    """

    def __init__(self, run_id: str) -> None:
        """Initialise the formatter with a fixed run identifier.

        The run_id is captured at construction time so that every record
        emitted through this formatter instance carries the same value, even
        if the ``BLITZY_RUN_ID`` environment variable is changed mid-process
        by a :func:`with_run_id_context` block. The recommended usage is for
        callers to acquire one logger per script via :func:`get_logger`,
        which constructs the formatter with the current value of
        :data:`BLITZY_RUN_ID_ENV` at logger-creation time.

        Args:
            run_id: The UUID4 correlation identifier to embed on every
                record. Stored verbatim — no validation is performed here
                because :func:`get_logger` is the only intended caller and
                it always passes the validated output of
                :func:`_ensure_run_id`.
        """
        super().__init__()
        self._run_id = run_id

    def format(self, record: logging.LogRecord) -> str:
        """Serialise a single ``LogRecord`` into a one-line JSON object.

        The implementation reads user-supplied extras from the record's
        ``__dict__`` minus the reserved standard attribute names, applies
        :func:`_redact_fields` to mask any credential-bearing fields, and
        then assembles the final event dictionary in the canonical field
        order. The output is encoded with ``ensure_ascii=False`` so that
        non-ASCII content (e.g. Unicode author names) is preserved verbatim
        rather than escaped, and with ``default=str`` so that complex
        objects (e.g. ``Path``, ``datetime``) serialise via their string
        representation instead of raising.

        Args:
            record: The log record to format.

        Returns:
            A single-line JSON string representing the log event.
        """
        # Collect user-supplied extras: every attribute set on the record
        # via the ``extra={...}`` keyword argument appears in __dict__ as a
        # top-level key. Filter out the standard library's own attributes
        # to leave only the user payload.
        extras: dict[str, Any] = {
            key: value
            for key, value in record.__dict__.items()
            if key not in _RESERVED_RECORD_ATTRS
        }
        redacted_extras = _redact_fields(extras) if extras else {}

        # Build the canonical event in the documented field order. Python
        # dictionaries preserve insertion order since 3.7, so this controls
        # the JSON key ordering exactly.
        event: dict[str, Any] = {
            "run_id": self._run_id,
            "ts": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ"),
            "script": record.name,
            "level": record.levelname,
            "event": record.getMessage(),
        }

        # Merge extras WITHOUT overwriting canonical fields. The defensive
        # filter here protects the run_id correlation anchor from any
        # caller (legitimate or otherwise) who passes
        # ``extra={"run_id": "something_else"}``.
        for key, value in redacted_extras.items():
            if key not in _CANONICAL_FIELDS:
                event[key] = value

        # Attach formatted exception traceback when present. The standard
        # formatter's formatException returns a multi-line string; embedded
        # newlines are encoded as \n inside the JSON string by json.dumps.
        if record.exc_info:
            event["exception"] = self.formatException(record.exc_info)
        elif record.exc_text:
            # exc_text is the pre-formatted traceback cached on the record
            # when exc_info has already been formatted by another handler.
            event["exception"] = record.exc_text

        if record.stack_info:
            event["stack"] = self.formatStack(record.stack_info)

        return json.dumps(event, default=str, ensure_ascii=False)




# ---------------------------------------------------------------------------
# Logger factory
# ---------------------------------------------------------------------------


def get_logger(
    script_name: str, level: int = logging.INFO
) -> logging.Logger:
    """Return a configured :class:`logging.Logger` for ``script_name``.

    The factory implements a per-script-name singleton via
    :data:`_LOGGER_CACHE` and double-checked locking on
    :data:`_FILE_HANDLER_LOCK`. The first call for a given name acquires the
    lock, materialises a :class:`logging.Logger`, attaches a
    :class:`logging.StreamHandler` directed at ``sys.stderr`` and a
    :class:`logging.FileHandler` directed at ``<workspace>/data/run.log.jsonl``,
    and caches the logger. Subsequent calls for the same name return the
    cached instance without acquiring the lock.

    The returned logger has ``propagate = False`` so that records do not
    bubble up to the root logger, where a third-party-installed handler
    might emit them as plain text and defeat the structured-JSON contract.

    Args:
        script_name: Logger name. By convention this is the calling script's
            basename without extension, e.g. ``"03_extract_pulls"``. The
            name appears verbatim in the ``script`` field of every emitted
            log event and is the cache key.
        level: Python logging level threshold. Defaults to :data:`logging.INFO`.
            Records below this level are dropped before formatting.

    Returns:
        A fully configured ``logging.Logger`` instance ready for use.

    Raises:
        OSError: If the JSON-Lines log file cannot be created (e.g. the
            workspace ``data/`` directory is on a read-only filesystem).
            The caller may catch this and fall back to a stderr-only
            logger by passing ``script_name`` again after working around
            the filesystem issue, since the first failed call does not
            populate the cache.
    """
    cached = _LOGGER_CACHE.get(script_name)
    if cached is not None:
        return cached
    with _FILE_HANDLER_LOCK:
        # Re-check the cache under the lock to close the race window
        # between the unsynchronised read above and the lock acquisition.
        cached = _LOGGER_CACHE.get(script_name)
        if cached is not None:
            return cached
        logger = _build_logger(script_name, level)
        _LOGGER_CACHE[script_name] = logger
        return logger


def _build_logger(script_name: str, level: int) -> logging.Logger:
    """Construct and configure a fresh logger.

    Internal helper invoked under :data:`_FILE_HANDLER_LOCK`. Separating
    the construction logic from :func:`get_logger` keeps the cache-check
    critical section short and makes the configuration steps individually
    testable.

    Args:
        script_name: Logger name.
        level: Logging level threshold.

    Returns:
        A configured ``logging.Logger`` with a stderr handler and a file
        handler attached, both using :class:`JsonFormatter`.
    """
    run_id = _ensure_run_id()
    logger = logging.getLogger(script_name)
    logger.setLevel(level)
    # Disable propagation so that our JSON-formatted records are not also
    # emitted through the root logger by some other library's default
    # handler. The cost is that callers cannot rely on root-logger
    # configuration affecting these loggers, which is the intended trade.
    logger.propagate = False
    # Clear any pre-existing handlers (e.g. a basicConfig handler attached
    # by an earlier import) so we never double-emit log lines.
    for existing in list(logger.handlers):
        logger.removeHandler(existing)
        try:
            existing.close()
        except Exception:
            # close() on a handler is a courtesy; failure here must not
            # prevent the logger from being usable.
            pass

    formatter = JsonFormatter(run_id)

    # Stream handler — every record is mirrored to stderr so that interactive
    # invocations of the pipeline (and the Makefile's `tail -f` workflow)
    # produce live output.
    stream_handler = logging.StreamHandler(sys.stderr)
    stream_handler.setLevel(level)
    stream_handler.setFormatter(formatter)
    logger.addHandler(stream_handler)

    # File handler — every record is appended to data/run.log.jsonl in the
    # workspace root. The handler is attached only if the parent directory
    # can be created (or already exists); a failure here is logged via the
    # stream handler that has already been attached, then re-raised, so
    # that callers get a single coherent failure mode rather than a logger
    # that silently drops file output.
    file_path = _log_file_path()
    try:
        file_path.parent.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        logger.warning(
            "log_file_parent_mkdir_failed",
            extra={
                "path": str(file_path.parent),
                "error": str(exc),
            },
        )
        raise

    file_handler = logging.FileHandler(
        filename=str(file_path), mode="a", encoding="utf-8"
    )
    file_handler.setLevel(level)
    file_handler.setFormatter(formatter)
    logger.addHandler(file_handler)

    return logger


# ---------------------------------------------------------------------------
# Run-scoped context manager
# ---------------------------------------------------------------------------


@contextmanager
def with_run_id_context(run_id: str | None = None) -> Iterator[str]:
    """Temporarily set ``BLITZY_RUN_ID`` for the duration of a ``with`` block.

    This is useful when a single Python process needs to execute a
    sub-operation under a distinct correlation identifier, for example
    when re-running an extraction step for a different historical date
    range while the surrounding pipeline retains its own run_id. The
    previous value of the environment variable is captured on entry and
    restored on exit, including the case where the variable was unset on
    entry (in which case the variable is deleted on exit).

    Note: this context manager does NOT invalidate previously cached
    loggers. Loggers created INSIDE the ``with`` block will capture the
    context-scoped run_id, but loggers created OUTSIDE retain their
    original run_id. This matches the documented design that the run_id
    on a given logger is fixed at construction time.

    Args:
        run_id: Explicit UUID4 string to install. If ``None``, the helper
            calls :func:`_ensure_run_id` to either preserve or generate a
            value.

    Yields:
        The active run ID for the duration of the block, suitable for
        embedding in log messages or artifact paths.
    """
    previous = os.environ.get(BLITZY_RUN_ID_ENV)
    if run_id is not None:
        os.environ[BLITZY_RUN_ID_ENV] = run_id
        active = run_id
    else:
        active = _ensure_run_id()
    try:
        yield active
    finally:
        if previous is None:
            # The variable was unset on entry — remove it so the post-exit
            # environment exactly matches the pre-entry environment.
            os.environ.pop(BLITZY_RUN_ID_ENV, None)
        else:
            os.environ[BLITZY_RUN_ID_ENV] = previous


# ---------------------------------------------------------------------------
# Convenience logging helpers
# ---------------------------------------------------------------------------


def log_data_source(
    logger: logging.Logger,
    source_name: str,
    available: bool,
    reason: str | None = None,
) -> None:
    """Emit a structured ``data_source_status`` event.

    The pipeline's extraction scripts call this helper once per external
    data source they attempt to reach (git, GitHub Pulls API, GitHub
    Actions API, Linear API, branch protection API, etc.). The resulting
    log lines feed the Data Source Inventory section of the final report
    and the audit trail referenced by Quality Gate 11.

    The chosen event name ``data_source_status`` is a sentinel string
    documented in the Quality Gates verification section of the AAP;
    downstream auditors filter the log feed by this exact event value.

    Args:
        logger: The logger to emit through, typically the value returned
            by :func:`get_logger`.
        source_name: Symbolic name of the data source. Examples:
            ``"git_local"``, ``"github_pulls_api"``, ``"linear_api"``.
        available: True if the source returned data, False if it was
            unreachable, unauthorised, or missing required credentials.
        reason: Free-text explanation of unavailability. Required when
            ``available`` is False, ignored when True. Examples:
            ``"LINEAR_API_KEY env var not set"``,
            ``"GitHub API rate limit exceeded"``.
    """
    level = logging.INFO if available else logging.WARNING
    extra = {
        "source": source_name,
        "available": available,
        "reason": reason,
    }
    logger.log(level, "data_source_status", extra=extra)


def log_metric_extracted(
    logger: logging.Logger,
    metric_id: str,
    value: Any,
    confidence: str,
) -> None:
    """Emit a structured ``metric_extracted`` event.

    Called by the compute stage after each of the twelve metrics is
    derived from raw artifacts. The resulting log line records the
    headline value and confidence tier in a single auditable event,
    feeding the audit trail referenced by Quality Gate 4 (Confidence
    Transparency).

    Args:
        logger: The logger to emit through.
        metric_id: Symbolic metric identifier matching the schema keys in
            ``data/metrics.json``. Examples: ``"m1"`` (Flow Load),
            ``"m7"`` (Flow Time), ``"m12"`` (Defects Out of SLA).
        value: The derived metric value. May be a numeric type, the
            string ``"insufficient_signal"``, or a structured dict for
            metrics that carry sub-fields. ``json.dumps(default=str)`` is
            tolerant of the polymorphic shape.
        confidence: One of ``"high"``, ``"medium"``, ``"low"``,
            ``"insufficient"``. The casing here is normalised to lower
            case for consistency with the schema in ``metrics.json``;
            callers should pass values in that form.
    """
    extra = {
        "metric_id": metric_id,
        "value": value,
        "confidence": confidence,
    }
    logger.info("metric_extracted", extra=extra)


# ---------------------------------------------------------------------------
# Public API surface
# ---------------------------------------------------------------------------

#: Tuple of public names exported by this module. Updating this list is the
#: contract surface for consumers; nothing else in the module is part of
#: the public API. Underscore-prefixed names remain importable for tests
#: but are not part of the documented surface.
__all__ = [
    "get_logger",
    "get_run_id",
    "with_run_id_context",
    "log_data_source",
    "log_metric_extracted",
    "JsonFormatter",
    "BLITZY_RUN_ID_ENV",
    "REDACTED_PLACEHOLDER",
]

