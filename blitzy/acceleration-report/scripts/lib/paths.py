"""Shared path-confinement and atomic-write helpers for the analysis pipeline.

This module centralises two security-and-reliability concerns that every
extraction and renderer script in the pipeline must enforce:

1. **Output path confinement** — every script accepts a ``--output`` (or
   similar) argument so the orchestration layer can redirect artifacts.
   Without confinement, a misconfigured caller could write outside the
   workspace tree, overwrite repository sources, or corrupt the operator's
   home directory. :func:`safe_output_path` resolves the caller-supplied
   path against the workspace root and raises :class:`OutputPathError`
   when the resolved path escapes the workspace tree (or, when stricter,
   the workspace ``data/`` directory).

2. **Atomic writes** — partial writes interrupted by SIGINT, OOM, or disk
   exhaustion previously left half-written JSON/Markdown/HTML on disk that
   downstream consumers happily parsed as corrupt. The
   :func:`atomic_write_text` and :func:`atomic_write_json` helpers write
   to a temporary sibling and then call :func:`os.replace`, which is
   atomic on POSIX filesystems for files on the same filesystem (the
   temp file is created via :func:`tempfile.NamedTemporaryFile` in the
   destination's parent directory).

Read-only contract
------------------

This module is pure and side-effect-free at import time. The only
filesystem interactions occur inside the named helpers when callers
invoke them with explicit path arguments.

Dependency surface
------------------

Standard library only (``json``, ``os``, ``pathlib``, ``tempfile``,
``typing``). No third-party imports.
"""

from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path
from typing import Any


#: Workspace root resolved from this file's path
#: (``blitzy/acceleration-report/``). Re-resolving from the module file
#: makes the constant correct regardless of the caller's working
#: directory.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent.parent


#: Canonical workspace data directory used as the default confinement
#: scope for raw data artifacts.
DATA_DIR: Path = WORKSPACE_ROOT / "data"


class OutputPathError(ValueError):
    """Raised when a caller-supplied output path is outside the allowed scope.

    Subclasses :class:`ValueError` so legacy callers that catch
    :class:`ValueError` still handle the failure, while new callers can
    pattern-match the precise policy violation.
    """


def safe_output_path(
    candidate: str | os.PathLike[str],
    *,
    allowed_root: Path | None = None,
    must_be_inside_data: bool = False,
) -> Path:
    """Resolve and validate a caller-supplied output path.

    The candidate path is resolved against the current working directory
    (matching argparse default behaviour and the Makefile invocation
    pattern of running scripts from the workspace root) and then checked
    against the chosen confinement scope. A path is accepted only when
    its resolved form is inside ``allowed_root`` (or :data:`WORKSPACE_ROOT`
    if ``allowed_root`` is ``None``). When ``must_be_inside_data`` is
    ``True``, the more restrictive ``data/`` scope is enforced.

    Symlinks are resolved before the check via :meth:`Path.resolve`,
    preventing symlink-based escapes. Parents that do not yet exist are
    handled by walking the path upward until an existing ancestor is
    found and then resolving that ancestor; the not-yet-existing tail is
    re-attached. This keeps the helper usable when the target file's
    parent directory has not yet been created.

    Args:
        candidate: Path supplied by the caller (e.g., the ``--output``
            argparse argument). May be relative or absolute. May refer to
            a file that does not yet exist.
        allowed_root: Optional alternative scope. Defaults to the
            workspace root.
        must_be_inside_data: When ``True``, restricts the allowed scope to
            ``allowed_root / "data"`` (or :data:`DATA_DIR` if
            ``allowed_root`` is ``None``).

    Returns:
        The resolved, validated absolute path.

    Raises:
        OutputPathError: When the resolved path escapes the allowed scope.
    """
    root = (allowed_root or WORKSPACE_ROOT).resolve()
    if must_be_inside_data:
        # data/ is always relative to the chosen root; when the caller
        # passes a custom ``allowed_root`` that is itself the data
        # directory, this is idempotent.
        if root.name == "data":
            allowed = root
        else:
            allowed = (root / "data").resolve()
    else:
        allowed = root

    # Resolve the candidate. ``Path.resolve(strict=False)`` returns an
    # absolute path even when the target does not exist; we walk up to the
    # first existing ancestor for safety because some platforms differ in
    # how they resolve non-existing tails.
    raw = Path(candidate)
    if not raw.is_absolute():
        raw = Path.cwd() / raw

    # Find the deepest existing ancestor to resolve symlinks against.
    existing = raw
    while not existing.exists() and existing != existing.parent:
        existing = existing.parent
    resolved_existing = existing.resolve()
    if existing == raw:
        resolved = resolved_existing
    else:
        # Re-attach the non-existing tail to the resolved ancestor.
        try:
            tail = raw.relative_to(existing)
        except ValueError:
            # Should never happen — ``existing`` is an ancestor by
            # construction — but defend defensively.
            tail = Path(*raw.parts[len(existing.parts):])
        resolved = (resolved_existing / tail).resolve()

    try:
        resolved.relative_to(allowed)
    except ValueError as exc:
        raise OutputPathError(
            "Output path is outside the allowed scope. "
            f"resolved={resolved!s}; allowed_root={allowed!s}; "
            "supply a path inside the workspace "
            f"({'data/' if must_be_inside_data else 'blitzy/acceleration-report/'})."
        ) from exc

    return resolved


def atomic_write_text(
    path: str | os.PathLike[str],
    content: str,
    *,
    encoding: str = "utf-8",
) -> Path:
    """Write ``content`` to ``path`` atomically via temp-file + os.replace.

    Steps:
        1. Ensure the parent directory exists (mkdir parents=True).
        2. Create a :class:`tempfile.NamedTemporaryFile` in the same
           parent directory so the rename is on the same filesystem.
        3. Write the content to the temp file and flush.
        4. :func:`os.replace` the temp file onto the destination path.
        5. On any exception, remove the temp file before re-raising.

    Args:
        path: Destination path. Must be a file path, not a directory.
        content: Text to write.
        encoding: Text encoding. Defaults to ``utf-8``.

    Returns:
        The destination path as a :class:`Path`.

    Raises:
        OSError: When the directory cannot be created or the write fails.
    """
    dest = Path(path)
    dest.parent.mkdir(parents=True, exist_ok=True)
    # ``delete=False`` because we need the file to persist past the
    # context-manager exit so :func:`os.replace` can move it onto ``dest``.
    tmp_fd, tmp_name = tempfile.mkstemp(
        prefix=dest.name + ".",
        suffix=".tmp",
        dir=str(dest.parent),
    )
    tmp_path = Path(tmp_name)
    try:
        with os.fdopen(tmp_fd, "w", encoding=encoding) as fh:
            fh.write(content)
            fh.flush()
            try:
                os.fsync(fh.fileno())
            except OSError:
                # fsync is best-effort; some filesystems (tmpfs, some
                # FUSE filesystems) do not support it but the rename
                # itself remains atomic.
                pass
        os.replace(tmp_name, dest)
    except BaseException:
        # Clean up the temp file on any error (including KeyboardInterrupt
        # and SystemExit, which BaseException catches).
        try:
            tmp_path.unlink()
        except FileNotFoundError:
            pass
        raise
    return dest


def atomic_write_json(
    path: str | os.PathLike[str],
    payload: Any,
    *,
    indent: int = 2,
    ensure_ascii: bool = False,
    sort_keys: bool = False,
    trailing_newline: bool = True,
    default: Any = None,
) -> Path:
    """Write ``payload`` as JSON to ``path`` atomically.

    Convenience wrapper around :func:`atomic_write_text` that serialises
    ``payload`` with :func:`json.dumps` first. The default arguments
    match the conventions used by the pipeline's existing artifacts
    (2-space indent, no ASCII escapes, optional trailing newline).

    Args:
        path: Destination path.
        payload: Any JSON-serialisable Python object.
        indent: ``json.dumps`` indent. Defaults to ``2``.
        ensure_ascii: ``json.dumps`` ensure_ascii. Defaults to ``False``.
        sort_keys: ``json.dumps`` sort_keys. Defaults to ``False``.
        trailing_newline: Append a single ``\\n`` after the JSON body so
            the file ends with a newline. Defaults to ``True``.
        default: Optional ``json.dumps`` default function for non-JSON
            types (e.g. :class:`datetime`). Defaults to ``str`` when
            ``None``.

    Returns:
        The destination path.
    """
    serialised = json.dumps(
        payload,
        indent=indent,
        ensure_ascii=ensure_ascii,
        sort_keys=sort_keys,
        default=default if default is not None else str,
    )
    if trailing_newline and not serialised.endswith("\n"):
        serialised += "\n"
    return atomic_write_text(path, serialised)


__all__ = [
    "DATA_DIR",
    "WORKSPACE_ROOT",
    "OutputPathError",
    "atomic_write_json",
    "atomic_write_text",
    "safe_output_path",
]
