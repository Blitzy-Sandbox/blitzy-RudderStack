"""Subprocess-based git helpers for the acceleration-report extraction pipeline.

This module is the canonical wrapper around read-only git CLI invocations used
by every extraction script under ``blitzy/acceleration-report/scripts/``. It
exposes typed, narrowly scoped helpers for the standard read-only git verbs
(``log``, ``rev-list``, ``rev-parse``, ``for-each-ref``, ``merge-base``,
``reflog``, ``cat-file``, ``diff``, ``show``) plus a small handful of
auxiliary helpers (``git_version``, ``git_diff_shortstat``,
``git_show_summary``).

Read-only enforcement
---------------------

Every public helper is built on the private ``_run_git`` and
``_run_git_checked`` wrappers, which in turn run every argument vector through
the module-level :func:`_validate_command` gate. The gate enforces four
distinct invariants before any subprocess fork:

1. The first positional token of ``argv`` must appear in
   :data:`ALLOWED_SUBCOMMANDS`. This forbids any verb the module does not
   intentionally expose.
2. No token anywhere in ``argv`` may match an entry in :data:`DENY_FLAGS`.
   This catches mutating switches that some otherwise-read-only verbs accept
   (for example ``git fetch --prune`` or ``git diff --force``).
3. The second positional token (``argv[1]``) must not appear in
   :data:`DENY_SUBCOMMANDS`. This catches nested mutating verbs such as
   ``git submodule update`` or ``git submodule add`` when the outer verb is
   allowed but the nested verb would mutate state.
4. When ``argv[0] == "submodule"``, the second positional token must not
   appear in :data:`DENY_SUBMODULE_VERBS`. This is the submodule-specific
   safety net that rejects every mutating sub-verb of ``git submodule``
   (``deinit``, ``sync``, ``foreach``, ``update``, ``add``, ``set-branch``,
   ``set-url``, ``absorbgitdirs``, ``init``). The only permitted submodule
   sub-verb is ``status``, which ``00_environment.sh`` consumes for the
   Rule-6 environment snapshot.

Violations of the allow-list / deny-list gate raise
:class:`GitReadOnlyViolation`, a dedicated exception subclass of
:class:`ValueError`. Subclassing :class:`ValueError` preserves backward
compatibility with callers that catch :class:`ValueError` while letting
new callers pattern-match the precise read-only contract violation. The
exception is part of the module's public API (see :data:`__all__`).

The single exception to this discipline is :func:`git_version`, which invokes
``git --version`` directly because ``--version`` is a top-level git flag and
not a sub-command — the validator's allow-list contract cannot apply. The
exception is documented inline on the function and is the only function in
the module that bypasses :func:`_validate_command`.

Subprocess discipline
---------------------

Every subprocess invocation uses ``subprocess.run(..., capture_output=True,
text=True, check=False)``. The unchecked :func:`_run_git` wrapper deliberately
keeps ``check=False`` because two public helpers consume exit codes as
semantic information rather than as success/failure:

* :func:`git_merge_base_is_ancestor` — exit code ``0`` means "is ancestor",
  exit code ``1`` means "is not ancestor", and any other exit code is a
  failure. The helper inspects :attr:`returncode` directly and returns a
  :class:`bool` rather than raising.
* :func:`git_reflog` — exit code ``128`` with an ``"unknown revision"``
  ``stderr`` is the documented "no reflog for this ref" outcome (typical
  on freshly-cloned bare clones or remote-only refs). The helper returns
  an empty list in that case rather than raising.

Every other helper opts into the strict-success contract via
:func:`_run_git_checked`, which raises :class:`subprocess.CalledProcessError`
on any non-zero exit. New helpers SHOULD use :func:`_run_git_checked` unless
they have a documented semantic reason to consume the exit code directly.

Dependency surface
------------------

This module uses ONLY the Python standard library
(``subprocess``, ``pathlib``, ``typing``, ``__future__``). It does NOT import
any other ``lib/`` module and therefore introduces no cycles into the
extraction-pipeline import graph.
"""

from __future__ import annotations

import subprocess
from pathlib import Path
from typing import Sequence


# ---------------------------------------------------------------------------
# Module-level constants
# ---------------------------------------------------------------------------


#: Sub-commands whose presence anywhere in an argument vector indicates a
#: mutating or otherwise out-of-contract git operation. The validator rejects
#: any ``argv[0]`` that is not in :data:`ALLOWED_SUBCOMMANDS`, but a small
#: number of allowed outer verbs (notably ``submodule``) accept nested
#: sub-verbs that themselves mutate state. This set is the second line of
#: defence: the validator also rejects any ``argv[1]`` that appears here.
#:
#: ``branch`` is included even though ``git branch -l`` is read-only; the
#: helpers do not need branch enumeration via that verb because
#: :func:`git_for_each_ref` with ``refs/heads/`` covers every read-only use.
#:
#: ``update`` is included so that nested ``submodule update`` invocations
#: (which check out the submodule's recorded commit and write to the working
#: tree) are rejected by the second-positional check. The plain ``update``
#: verb has no read-only top-level meaning in git, so denying it costs
#: nothing.
DENY_SUBCOMMANDS: frozenset[str] = frozenset(
    {
        "add",
        "am",
        "apply",
        "archive",
        "branch",
        "checkout",
        "cherry-pick",
        "clean",
        "clone",
        "commit",
        "config",
        "fetch",
        "filter-branch",
        "fsck",
        "gc",
        "init",
        "merge",
        "mv",
        "notes",
        "pack-objects",
        "prune",
        "pull",
        "push",
        "rebase",
        "reflog-write",
        "remote",
        "remove",
        "repack",
        "replace",
        "reset",
        "restore",
        "revert",
        "rm",
        "stash",
        "submodule",
        "switch",
        "tag",
        "update",
        "update-ref",
        "worktree",
    }
)


#: Switches that mutate state even when paired with an otherwise read-only
#: verb. ``--force`` and ``--force-with-lease`` are forbidden because no
#: read-only path requires them; ``--all-tags`` and ``--prune`` are forbidden
#: in conjunction with ``fetch`` (already in DENY_SUBCOMMANDS but defence in
#: depth is preferred); ``--delete``, ``--remove-section``, ``--unset``,
#: ``--unset-all``, ``--edit``, ``--in-place`` are forbidden because every
#: known use of these flags writes to repository state.
DENY_FLAGS: frozenset[str] = frozenset(
    {
        "--force",
        "--force-with-lease",
        "--all-tags",
        "--prune",
        "--delete",
        "--remove-section",
        "--unset",
        "--unset-all",
        "--edit",
        "--in-place",
    }
)


#: Nested sub-verbs of ``git submodule`` that are explicitly mutating. The
#: outer verb ``submodule`` is in :data:`ALLOWED_SUBCOMMANDS` because
#: ``git submodule status`` is the read-only enumeration that
#: ``00_environment.sh`` consumes for the Rule-6 ``submodule_state`` field.
#: Every other ``git submodule <verb>`` invocation either writes to the
#: working tree, mutates ``.gitmodules``, or executes user-supplied commands
#: in submodule directories and is therefore rejected by
#: :func:`_validate_command` when ``argv[0] == "submodule"`` and
#: ``argv[1]`` appears in this set.
#:
#: The entries are:
#:
#: * ``add`` — clone a new submodule and stage ``.gitmodules`` changes.
#: * ``absorbgitdirs`` — move submodule ``.git`` directories into the
#:   superproject's ``modules/`` tree (mutating).
#: * ``deinit`` — unregister submodules, removing them from the working
#:   tree and clearing their ``.git/config`` entries.
#: * ``foreach`` — execute an arbitrary shell command in every submodule.
#:   Even read-only commands violate the read-only contract because the
#:   command string itself is untrusted from the validator's perspective.
#: * ``init`` — register submodules into ``.git/config`` (mutating).
#: * ``set-branch`` — change the recorded branch a submodule tracks
#:   (mutates ``.gitmodules``).
#: * ``set-url`` — change the recorded URL of a submodule (mutates
#:   ``.gitmodules``).
#: * ``sync`` — write the recorded submodule URLs into ``.git/config``.
#: * ``update`` — fetch the submodule and check out its recorded SHA into
#:   the working tree.
#:
#: ``add`` and ``update`` are also entries in :data:`DENY_SUBCOMMANDS` (as
#: outer verbs); their presence here is defence-in-depth so that the
#: nested-verb check fires even if a future contributor expands
#: :data:`ALLOWED_SUBCOMMANDS` or otherwise reorders the validator gate.
#:
#: ``status`` is intentionally absent. ``git submodule status`` is the
#: single read-only sub-verb on the ``submodule`` verb and is the
#: invocation produced by ``00_environment.sh`` for the Rule-6 environment
#: snapshot.
DENY_SUBMODULE_VERBS: frozenset[str] = frozenset(
    {
        "absorbgitdirs",
        "add",
        "deinit",
        "foreach",
        "init",
        "set-branch",
        "set-url",
        "sync",
        "update",
    }
)


#: Outer sub-commands the validator will accept as ``argv[0]``. Every helper
#: function in this module emits a vector whose first token is in this set.
#: Adding a new verb here without also exposing a typed public helper for it
#: is treated as a defect.
#:
#: ``submodule`` is admitted to support ``git submodule status`` (a read-only
#: enumeration of submodule state). The nested-verb safety net in
#: :func:`_validate_command` ensures that ``submodule update`` or
#: ``submodule add`` invocations are still rejected.
ALLOWED_SUBCOMMANDS: frozenset[str] = frozenset(
    {
        "log",
        "rev-list",
        "rev-parse",
        "for-each-ref",
        "merge-base",
        "reflog",
        "cat-file",
        "diff",
        "show",
        "ls-tree",
        "ls-files",
        "describe",
        "name-rev",
        "shortlog",
        "blame",
        "submodule",
    }
)


#: Public export surface. The list groups exports by kind: exception classes
#: first, functions next (alphabetical within the group), constants last.
#: This is the contract consumed by the extraction scripts via
#: ``from lib.git import ...`` (or equivalently ``from .git import ...`` when
#: referenced as a package submodule).
__all__ = [
    "GitReadOnlyViolation",
    "git_log",
    "git_log_raw",
    "git_revlist",
    "git_rev_parse",
    "git_rev_parse_toplevel",
    "git_for_each_ref",
    "git_merge_base_is_ancestor",
    "git_reflog",
    "git_cat_file",
    "git_diff_shortstat",
    "git_show_summary",
    "git_version",
    "DENY_SUBCOMMANDS",
    "DENY_FLAGS",
    "DENY_SUBMODULE_VERBS",
    "ALLOWED_SUBCOMMANDS",
]


# ---------------------------------------------------------------------------
# Validation gate
# ---------------------------------------------------------------------------


class GitReadOnlyViolation(ValueError):
    """Raised by :func:`_validate_command` when a git argv violates the gate.

    This exception identifies a specific class of failure — an argument vector
    that violates the read-only allow-list / deny-list contract enforced by
    :func:`_validate_command`. Concrete violations include:

    * an empty ``argv``,
    * an ``argv[0]`` (outer sub-command) that is not in
      :data:`ALLOWED_SUBCOMMANDS`,
    * any token in ``argv`` that is in :data:`DENY_FLAGS`,
    * an ``argv[1]`` (nested sub-command) that is in :data:`DENY_SUBCOMMANDS`,
      or
    * ``argv[0] == "submodule"`` and ``argv[1]`` is in
      :data:`DENY_SUBMODULE_VERBS` (the submodule-specific safety net).

    The class subclasses :class:`ValueError` for backward compatibility with
    callers that broadly catch ``except ValueError`` (such legacy code
    continues to work unchanged). Callers that need to distinguish a read-only
    contract violation from any other ``ValueError`` can catch this exception
    specifically.

    Every error message produced by :func:`_validate_command` includes either
    the substring ``"allow list"`` or ``"deny list"`` so message-based
    matching also works for callers that prefer not to import the exception
    type. The dedicated class is the preferred discriminator.
    """


def _validate_command(argv: Sequence[str]) -> None:
    """Apply the read-only allow-list / deny-list gate to a git argument vector.

    The gate enforces four invariants on ``argv``:

    * ``argv`` is non-empty.
    * ``argv[0]`` is a member of :data:`ALLOWED_SUBCOMMANDS`.
    * No token in ``argv`` is a member of :data:`DENY_FLAGS`.
    * ``argv[1]`` (when present) is not a member of :data:`DENY_SUBCOMMANDS`.
    * When ``argv[0] == "submodule"`` and ``argv[1]`` is present, ``argv[1]``
      is not a member of :data:`DENY_SUBMODULE_VERBS`. This is the
      submodule-specific safety net that rejects mutating nested verbs such
      as ``deinit``, ``sync``, ``foreach``, ``update``, ``add``,
      ``set-branch``, ``set-url``, and ``absorbgitdirs`` even though they
      do not appear in :data:`DENY_SUBCOMMANDS` (the outer-verb deny list).
      The only permitted submodule sub-verb is ``status``.

    The function returns ``None`` if every invariant holds and raises
    :class:`GitReadOnlyViolation` (a :class:`ValueError` subclass) otherwise.
    It performs no I/O.

    Args:
        argv: The git argument vector as it would be passed to
            ``subprocess.run(["git", *argv])``. Must NOT include the leading
            ``"git"`` token — the validator expects ``argv[0]`` to be the
            sub-command (e.g. ``"log"``, ``"rev-list"``).

    Raises:
        GitReadOnlyViolation: When ``argv`` is empty, when ``argv[0]`` is not
            in the allow list, when any token is in :data:`DENY_FLAGS`, when
            ``argv[1]`` is in :data:`DENY_SUBCOMMANDS`, or when
            ``argv[0] == "submodule"`` and ``argv[1]`` is in
            :data:`DENY_SUBMODULE_VERBS`. The exception subclasses
            :class:`ValueError`, so legacy callers that catch
            :class:`ValueError` continue to work without modification. Every
            error message contains either the substring ``"allow list"`` or
            ``"deny list"`` so callers can pattern-match without parsing
            positional detail.
    """
    if not argv:
        raise GitReadOnlyViolation("git argv must be non-empty")

    subcommand = argv[0]
    if subcommand not in ALLOWED_SUBCOMMANDS:
        raise GitReadOnlyViolation(
            f"git sub-command {subcommand!r} is not in the allow list"
        )

    for token in argv:
        if token in DENY_FLAGS:
            raise GitReadOnlyViolation(
                f"git flag {token!r} is in the deny list"
            )

    if len(argv) >= 2:
        nested = argv[1]
        if nested in DENY_SUBCOMMANDS:
            raise GitReadOnlyViolation(
                f"git nested sub-command {nested!r} is in the deny list"
            )
        # Submodule-specific safety net: reject every mutating sub-verb of
        # ``git submodule`` even though those verbs do not appear in the
        # outer-verb deny list. The only permitted nested verb is
        # ``status``, which is what ``00_environment.sh`` invokes for the
        # Rule-6 environment snapshot. Any other nested verb is rejected
        # with a message that explicitly references the submodule deny
        # list so the failure is unambiguous downstream.
        if subcommand == "submodule" and nested in DENY_SUBMODULE_VERBS:
            raise GitReadOnlyViolation(
                f"git submodule sub-verb {nested!r} is in the deny list "
                f"(submodule deny list)"
            )


# ---------------------------------------------------------------------------
# Core subprocess wrappers
# ---------------------------------------------------------------------------


def _run_git(
    argv: Sequence[str],
    cwd: Path | None = None,
) -> subprocess.CompletedProcess[str]:
    """Validate ``argv`` and execute ``git`` with it, returning the result.

    The function applies :func:`_validate_command` first, then dispatches to
    :func:`subprocess.run` with ``capture_output=True``, ``text=True`` and
    ``check=False``. Non-zero return codes do NOT raise — the caller decides
    whether to treat them as errors or as semantic signals (the latter is the
    contract for :func:`git_merge_base_is_ancestor` and :func:`git_reflog`).

    Args:
        argv: The git argument vector. The leading ``"git"`` binary name is
            prepended by this function and MUST NOT appear in ``argv``.
        cwd: Optional working directory for the subprocess. When omitted the
            current process's working directory is used. The git CLI walks
            up from this directory looking for a ``.git`` marker.

    Returns:
        The :class:`subprocess.CompletedProcess` instance with ``stdout``,
        ``stderr`` and ``returncode`` populated. Both stream attributes are
        ``str`` (not ``bytes``) because ``text=True`` is passed.

    Raises:
        GitReadOnlyViolation: Propagated from :func:`_validate_command` when
            the argument vector violates the allow-list or deny-list
            invariants. This is a :class:`ValueError` subclass, so callers
            using ``except ValueError`` continue to work.
    """
    _validate_command(argv)
    return subprocess.run(
        ["git", *argv],
        cwd=cwd,
        capture_output=True,
        text=True,
        check=False,
    )


def _run_git_checked(
    argv: Sequence[str],
    cwd: Path | None = None,
) -> str:
    """Run ``git`` with ``argv`` and return stdout, raising on non-zero exit.

    This is the standard wrapper for helpers whose contract is "the command
    must succeed". On a zero exit code the function returns the captured
    stdout. On any non-zero exit code it raises
    :class:`subprocess.CalledProcessError` with the captured stdout and
    stderr attached so the caller can introspect the failure.

    Args:
        argv: The git argument vector (without the leading ``"git"`` token).
        cwd: Optional working directory for the subprocess.

    Returns:
        The captured stdout of the subprocess as a ``str``.

    Raises:
        GitReadOnlyViolation: Propagated from :func:`_validate_command`.
            This is a :class:`ValueError` subclass, so callers using
            ``except ValueError`` continue to work.
        subprocess.CalledProcessError: When ``git`` exits with a non-zero
            return code. The ``output`` attribute carries stdout and the
            ``stderr`` attribute carries stderr.
    """
    result = _run_git(argv, cwd=cwd)
    if result.returncode != 0:
        raise subprocess.CalledProcessError(
            returncode=result.returncode,
            cmd=["git", *argv],
            output=result.stdout,
            stderr=result.stderr,
        )
    return result.stdout


# ---------------------------------------------------------------------------
# Output-shaping helpers
# ---------------------------------------------------------------------------


def _splitlines_drop_trailing_blank(text: str) -> list[str]:
    """Split ``text`` on newlines and drop a single trailing empty line.

    ``str.splitlines()`` already handles a single trailing newline correctly,
    but git output occasionally arrives with an extra blank line at the end
    (for example from formats that end with ``%n``). This helper normalises
    both cases so downstream code sees a clean list of non-trailing content
    lines.

    Args:
        text: Raw subprocess stdout.

    Returns:
        A list of lines with at most one trailing empty entry stripped.
    """
    lines = text.splitlines()
    if lines and lines[-1] == "":
        lines = lines[:-1]
    return lines


# ---------------------------------------------------------------------------
# Public helpers
# ---------------------------------------------------------------------------


def git_log(
    fmt: str,
    args: list[str] | None = None,
    cwd: Path | None = None,
) -> list[str]:
    """Run ``git log --pretty=format:<fmt>`` and return one line per commit.

    The function uses the ``--pretty=format:`` flavour (not
    ``--pretty=tformat:``), which separates entries with newlines but does
    not append a trailing newline. ``fmt`` accepts standard git format
    placeholders — the most common in this pipeline are:

    ============ ================================================
    Placeholder  Meaning
    ============ ================================================
    ``%H``       Full 40-character commit SHA
    ``%h``       Abbreviated commit SHA
    ``%aE``      Author email (lower-case)
    ``%aN``      Author display name (with .mailmap applied)
    ``%aI``      Author date in strict ISO-8601 format
    ``%cE``      Committer email
    ``%cN``      Committer display name
    ``%cI``      Committer date in strict ISO-8601 format
    ``%P``       Parent SHAs (space-separated)
    ``%s``       Commit subject (first line of message)
    ``%B``       Full commit message body (multi-line)
    ============ ================================================

    Args:
        fmt: The format string passed to ``--pretty=format:``. Must NOT
            include the leading ``--pretty=format:`` token — only the
            placeholder string. Multi-line formats (those containing ``%B``
            or explicit ``%n`` separators) yield more lines per commit than
            single-line formats; callers using such formats must reassemble
            commits using an explicit delimiter encoded into the format.
        args: Additional positional arguments to forward to ``git log`` after
            the ``--pretty=format:`` token. Useful for revision ranges
            (e.g. ``["main"]``), pathspecs, ``--no-merges``, ``--all``,
            ``-1`` for limiting to a single commit, and so on.
        cwd: Optional working directory passed through to the subprocess.

    Returns:
        The captured stdout split into a list of lines with any single
        trailing blank line stripped. An empty list indicates that ``git
        log`` produced no output (typically because the revision range
        matched no commits).

    Raises:
        GitReadOnlyViolation: When ``args`` contains a deny-list flag
            (subclass of :class:`ValueError`; legacy ``except ValueError``
            still catches it).
        subprocess.CalledProcessError: When ``git log`` exits non-zero.
    """
    argv: list[str] = ["log", f"--pretty=format:{fmt}", *(args or [])]
    text = _run_git_checked(argv, cwd=cwd)
    return _splitlines_drop_trailing_blank(text)


def git_log_raw(
    fmt: str,
    args: list[str] | None = None,
    cwd: Path | None = None,
) -> str:
    """Run ``git log --pretty=format:<fmt>`` and return the raw stdout string.

    Identical guarantees and validation as :func:`git_log` but returns the
    captured stdout as a single string instead of split lines. Required by
    callers that use ``%B`` (full body) or any other multi-line format
    placeholder, where naive line splitting destroys record boundaries.
    Such callers typically encode a record terminator into the format
    (e.g. ``"%H|%aI%n%B%n----END----"``) and split the raw output on the
    terminator themselves.

    The same allow-list / deny-list enforcement as :func:`git_log` applies
    through :func:`_run_git_checked` → :func:`_validate_command`. Any
    deny-listed flag in ``args`` raises :class:`GitReadOnlyViolation`.

    Args:
        fmt: The format string passed to ``--pretty=format:``. Multi-line
            formats are explicitly supported.
        args: Additional positional arguments forwarded to ``git log`` after
            the ``--pretty=format:`` token.
        cwd: Optional working directory passed through to the subprocess.

    Returns:
        The captured stdout as a single ``str``. The string is unmodified
        — no trailing-newline normalisation is applied, since the caller
        likely needs the exact byte stream to split on its own delimiter.

    Raises:
        GitReadOnlyViolation: When ``args`` contains a deny-list flag.
        subprocess.CalledProcessError: When ``git log`` exits non-zero.
    """
    argv: list[str] = ["log", f"--pretty=format:{fmt}", *(args or [])]
    return _run_git_checked(argv, cwd=cwd)


def git_revlist(
    args: list[str],
    cwd: Path | None = None,
) -> list[str]:
    """Run ``git rev-list <args...>`` and return one line per emitted SHA.

    ``git rev-list`` is the workhorse for revision-range enumeration. Common
    invocations in this pipeline include ``["--count", "HEAD"]`` to count
    total commits on the current branch, ``["--no-merges", "main"]`` to
    enumerate non-merge commits, and ``["main", "--not", "<tag>"]`` to list
    commits on ``main`` that are not yet reachable from ``<tag>``.

    Args:
        args: Positional arguments for ``git rev-list``. The function passes
            them verbatim after the ``rev-list`` sub-command token.
        cwd: Optional working directory.

    Returns:
        The captured stdout split into a list of lines with any single
        trailing blank line stripped. When the args include ``--count`` the
        list contains a single element holding the decimal count as a
        string.

    Raises:
        GitReadOnlyViolation: When ``args`` contains a deny-list flag
            (subclass of :class:`ValueError`).
        subprocess.CalledProcessError: When ``git rev-list`` exits non-zero.
    """
    argv: list[str] = ["rev-list", *args]
    text = _run_git_checked(argv, cwd=cwd)
    return _splitlines_drop_trailing_blank(text)


def git_rev_parse(
    args: list[str],
    cwd: Path | None = None,
) -> str:
    """Run ``git rev-parse <args...>`` and return the stripped stdout.

    The function is intentionally narrow: it returns the entire stripped
    stdout as a single string. Callers that pass multiple revisions (for
    example ``["HEAD", "main"]``) receive a newline-separated string and are
    responsible for splitting; callers that pass a single revision receive
    the resolved SHA (or other rev-parse output, such as the path returned
    by ``--show-toplevel``).

    Args:
        args: Positional arguments for ``git rev-parse``.
        cwd: Optional working directory.

    Returns:
        The captured stdout with leading and trailing whitespace stripped.

    Raises:
        GitReadOnlyViolation: When ``args`` contains a deny-list flag
            (subclass of :class:`ValueError`).
        subprocess.CalledProcessError: When ``git rev-parse`` exits non-zero.
            Common causes include: the requested revision does not exist,
            the current working directory is not inside a git repository,
            or the argument is malformed.
    """
    argv: list[str] = ["rev-parse", *args]
    text = _run_git_checked(argv, cwd=cwd)
    return text.strip()


def git_rev_parse_toplevel(cwd: Path | None = None) -> Path | None:
    """Return the absolute path of the enclosing git repository, or ``None``.

    Wraps ``git rev-parse --show-toplevel``. The function does NOT raise
    when the working directory is outside any git repository — that is a
    semantic signal handled by returning ``None``. Other failure modes
    (corrupt repository, permission errors) also surface as ``None`` because
    no scenario in the calling extraction scripts treats them as anything
    other than "this is not the path you want".

    Args:
        cwd: Optional working directory to use as the starting point for
            the toplevel search. When omitted the current process working
            directory is used.

    Returns:
        A :class:`pathlib.Path` for the repository root when the working
        directory is inside a git repository; ``None`` otherwise. The path
        is the value reported by ``git rev-parse --show-toplevel`` after
        whitespace stripping.

    Raises:
        GitReadOnlyViolation: Should not occur in practice — the argument
            vector constructed by this function is statically known to satisfy
            the
            validator. The exception is documented for completeness so
            callers can pattern-match on it if needed.
    """
    result = _run_git(["rev-parse", "--show-toplevel"], cwd=cwd)
    if result.returncode == 0:
        stripped = result.stdout.strip()
        if stripped:
            return Path(stripped)
        return None
    return None


def git_for_each_ref(
    pattern: str,
    format: str = "%(refname)",
    cwd: Path | None = None,
) -> list[str]:
    """Enumerate refs matching ``pattern`` formatted by ``format``.

    Wraps ``git for-each-ref --format=<format> <pattern>``. The function is
    the read-only alternative to ``git branch -l`` / ``git tag -l``; the
    extraction scripts use it to enumerate ``refs/heads/``,
    ``refs/remotes/``, and ``refs/tags/`` without invoking the
    DENY-listed ``branch`` or ``tag`` sub-commands.

    Args:
        pattern: A glob pattern over the ref namespace (for example
            ``"refs/heads/"``, ``"refs/tags/v[0-9]*"``, or
            ``"refs/remotes/origin/blitzy-*"``).
        format: The format string passed to ``--format=``. Common
            placeholders include ``%(refname)``, ``%(refname:short)``,
            ``%(objectname)``, ``%(creatordate:iso-strict)``,
            ``%(committerdate:iso-strict)``, and ``%(*objectname)`` for
            dereferenced tag objects. Pipe-separated multi-field formats
            (for example ``"%(refname)|%(objectname)|%(creatordate:iso-strict)"``)
            are the convention in this pipeline.
        cwd: Optional working directory.

    Returns:
        A list of formatted refs, one per line. An empty list means no
        ref matched the pattern.

    Raises:
        GitReadOnlyViolation: When ``pattern`` or ``format`` is a deny-list
            flag (this would be highly unusual but is enforced for
            completeness; subclass of :class:`ValueError`).
        subprocess.CalledProcessError: When ``git for-each-ref`` exits
            non-zero (typically because the pattern is syntactically
            invalid).
    """
    argv: list[str] = ["for-each-ref", f"--format={format}", pattern]
    text = _run_git_checked(argv, cwd=cwd)
    return _splitlines_drop_trailing_blank(text)


def git_merge_base_is_ancestor(
    commit_a: str,
    commit_b: str,
    cwd: Path | None = None,
) -> bool:
    """Return ``True`` when ``commit_a`` is an ancestor of ``commit_b``.

    Wraps ``git merge-base --is-ancestor <commit_a> <commit_b>``. The git
    CLI encodes the answer in its exit code: ``0`` means "yes, ancestor",
    ``1`` means "no, not an ancestor", and any other code indicates a
    genuine error (unknown revision, IO failure, etc.). The function maps
    this onto a Python ``bool`` and raises
    :class:`subprocess.CalledProcessError` for the third case.

    This helper is the foundation of Metric 8 (Problem Records in Release):
    a revert commit's "original" commit is attributed to the most recent
    release tag whose tag commit is an ancestor of the original.

    Args:
        commit_a: Candidate ancestor — typically a revision identifier
            (SHA, tag name, branch name, or symbolic ref).
        commit_b: Candidate descendant — same accepted forms as
            ``commit_a``.
        cwd: Optional working directory.

    Returns:
        ``True`` when ``commit_a`` is an ancestor of ``commit_b`` (git exit
        code 0); ``False`` when it is not (git exit code 1).

    Raises:
        GitReadOnlyViolation: Cannot occur in practice — the argument vector
            is statically known to satisfy the validator (subclass of
            :class:`ValueError`).
        subprocess.CalledProcessError: When git exits with any code other
            than 0 or 1. The most common cause is an unresolvable
            revision name (for example a SHA that does not exist in the
            local clone).
    """
    argv: list[str] = ["merge-base", "--is-ancestor", commit_a, commit_b]
    result = _run_git(argv, cwd=cwd)
    if result.returncode == 0:
        return True
    if result.returncode == 1:
        return False
    raise subprocess.CalledProcessError(
        returncode=result.returncode,
        cmd=["git", *argv],
        output=result.stdout,
        stderr=result.stderr,
    )


def git_reflog(
    ref: str,
    cwd: Path | None = None,
) -> list[str]:
    """Return the reflog entries for ``ref`` as a list of lines.

    Wraps ``git reflog show <ref>``. Reflogs are an optional facility:
    repositories with ``core.logAllRefUpdates=false`` or refs that have
    simply never received an update have no reflog. The function treats a
    non-zero exit code as "no reflog available" rather than as an error,
    returning an empty list. This matches the pipeline's contract for
    Metric 10 (Approved Exceptions): force-push detection from the reflog
    is a Low-confidence signal, and an absent reflog contributes zero
    force-pushes with the confidence caveat already recorded.

    Args:
        ref: The ref to enumerate (for example ``"main"``, ``"HEAD"``, or
            ``"refs/remotes/origin/main"``).
        cwd: Optional working directory.

    Returns:
        A list of reflog lines (each in the standard reflog format
        ``"<sha> <ref>@{<n>}: <message>"``) with any single trailing blank
        line stripped. An empty list indicates that the reflog is unavailable
        or empty; the two cases are not distinguished by this helper.

    Raises:
        GitReadOnlyViolation: Cannot occur in practice — the argument vector
            is statically known to satisfy the validator (subclass of
            :class:`ValueError`).
    """
    argv: list[str] = ["reflog", "show", ref]
    result = _run_git(argv, cwd=cwd)
    if result.returncode != 0:
        return []
    return _splitlines_drop_trailing_blank(result.stdout)


def git_cat_file(
    sha: str,
    kind: str = "-p",
    cwd: Path | None = None,
) -> str:
    """Return the raw object content for ``sha`` as printed by ``cat-file``.

    Wraps ``git cat-file <kind> <sha>``. The default ``kind`` is ``-p``
    (pretty-print), which produces the raw object body for commits, tags,
    trees and blobs. Other useful kinds include ``-t`` (type), ``-s``
    (size), and ``commit`` / ``tag`` / ``tree`` / ``blob`` to retrieve
    an object whose type is known in advance.

    The returned stdout is NOT stripped because the body of a commit or
    tag message may legitimately end with a newline that downstream parsers
    need to see.

    Args:
        sha: The object identifier to print — typically a full SHA, but any
            git-resolvable name is accepted (tags, branches, ``HEAD``).
        kind: The cat-file mode flag or object-type literal. Defaults to
            ``"-p"``.
        cwd: Optional working directory.

    Returns:
        The captured stdout verbatim (no stripping). For ``kind="-p"`` on a
        commit object this is the full commit body including author, committer,
        and message lines.

    Raises:
        GitReadOnlyViolation: When ``kind`` is a deny-list flag (highly
            unusual but enforced for completeness; subclass of
            :class:`ValueError`).
        subprocess.CalledProcessError: When ``git cat-file`` exits non-zero
            (typically because the SHA is unknown to the local clone).
    """
    argv: list[str] = ["cat-file", kind, sha]
    return _run_git_checked(argv, cwd=cwd)


def git_diff_shortstat(
    rev_range: str,
    cwd: Path | None = None,
) -> str:
    """Return the ``git diff --shortstat <rev_range>`` summary as a string.

    The shortstat summary takes the form
    ``" N files changed, A insertions(+), D deletions(-)"`` (with leading
    whitespace) — the helper strips the surrounding whitespace and returns
    a clean single-line string. When ``rev_range`` describes a range with
    no differences the string is empty.

    Args:
        rev_range: A revision range expression such as ``"main~5..main"``,
            ``"HEAD~1..HEAD"``, or a single revision name (in which case
            the diff is between that revision and the working tree). The
            value is passed verbatim to ``git diff``.
        cwd: Optional working directory.

    Returns:
        The captured stdout with leading and trailing whitespace stripped.
        An empty string indicates no differences in the range.

    Raises:
        GitReadOnlyViolation: When ``rev_range`` is a deny-list flag
            (subclass of :class:`ValueError`).
        subprocess.CalledProcessError: When ``git diff`` exits non-zero
            (typically because the range cannot be resolved).
    """
    argv: list[str] = ["diff", "--shortstat", rev_range]
    text = _run_git_checked(argv, cwd=cwd)
    return text.strip()


def git_show_summary(
    sha: str,
    cwd: Path | None = None,
) -> str:
    """Return a single-line summary of the commit identified by ``sha``.

    Wraps ``git show --no-patch --pretty=format:%H|%aI|%aE|%aN|%s <sha>``.
    The result is a pipe-delimited string with five fields: full SHA, author
    ISO-8601 date, author email, author display name (with .mailmap
    applied), and commit subject. Pipe characters in subjects are rare in
    practice; callers that need to disambiguate should switch to a less
    common delimiter or use :func:`git_log` with an explicit terminator
    sequence.

    Args:
        sha: The commit identifier to summarise — any git-resolvable name.
        cwd: Optional working directory.

    Returns:
        A single-line pipe-delimited string with the five fields described
        above. Leading and trailing whitespace are stripped.

    Raises:
        GitReadOnlyViolation: Cannot occur in practice — the argument vector
            is statically known to satisfy the validator (subclass of
            :class:`ValueError`).
        subprocess.CalledProcessError: When ``git show`` exits non-zero
            (typically because the SHA is unknown).
    """
    argv: list[str] = [
        "show",
        "--no-patch",
        "--pretty=format:%H|%aI|%aE|%aN|%s",
        sha,
    ]
    text = _run_git_checked(argv, cwd=cwd)
    return text.strip()


def git_version(cwd: Path | None = None) -> str:
    """Return the installed git CLI version string.

    This function is the SINGLE EXCEPTION to the read-only validator
    discipline that applies elsewhere in the module. ``git --version`` uses
    a top-level git flag (``--version``) rather than a sub-command, which
    the validator's allow-list contract cannot represent. The function
    invokes :func:`subprocess.run` directly with ``check=True`` and is
    intentionally narrow — it accepts only ``cwd`` and accepts no caller
    argv at all, so no untrusted argument can reach the subprocess.

    The function exists to satisfy Rule 6 (Environment First): the
    Environment Verification section of the report records the git version
    used during extraction, and no other helper in this module can capture
    it.

    Args:
        cwd: Optional working directory passed through to the subprocess.
            Has no observable effect on ``git --version`` output but is
            accepted for parity with the rest of the module's API.

    Returns:
        The output of ``git --version`` with surrounding whitespace
        stripped, for example ``"git version 2.43.0"``.

    Raises:
        subprocess.CalledProcessError: When the git binary is missing or
            cannot be invoked. ``check=True`` is used because a missing
            git binary is a fatal precondition failure — the entire
            extraction pipeline cannot proceed without it.
    """
    # Intentional direct subprocess call: ``--version`` is a top-level git
    # flag and not a sub-command, so the allow-list validator cannot apply.
    # No caller argv reaches this site — only the fixed ``--version`` flag.
    result = subprocess.run(
        ["git", "--version"],
        cwd=cwd,
        capture_output=True,
        text=True,
        check=True,
    )
    return result.stdout.strip()

