"""HTML and Mermaid escaping helpers for the renderer scripts.

Centralises the small set of escaping concerns that the report renderer
(``10_render_report.py``) and the executive-deck renderer
(``11_render_deck.py``) repeat across many call sites:

* :func:`html_escape` — HTML metacharacter escape used in every
  caller-supplied attribute and text node.
* :func:`mermaid_label_safe` — replaces the small set of characters that
  break Mermaid node-label parsing (``<``, ``>``, ``"``, backticks,
  ``</pre>`` sequences) without disturbing the visible label content.

The escaping policy is deliberately conservative: the deck renderer is
the visible product of the analysis pipeline. Although the data ingested
by the renderers comes from local raw artifacts under the workspace
``data/`` directory and therefore originates from the same operator
running the pipeline, defence-in-depth requires that no caller-side
escaping mistake can inject script tags, break out of the ``<pre>``
container that wraps Mermaid sources, or otherwise corrupt the rendered
HTML.

Dependency surface
------------------

Standard library only (``html``, ``re``, ``typing``).
"""

from __future__ import annotations

import html
import re
from typing import Any


#: Sequences that must never appear verbatim inside a Mermaid block;
#: a leak would let an upstream label end the ``<pre class="mermaid">``
#: container and inject arbitrary HTML after it. Each entry is replaced
#: with a visually-distinct U+FFFD-style sentinel when encountered.
_MERMAID_FORBIDDEN: tuple[tuple[str, str], ...] = (
    ("</pre>", "[REDACTED-CLOSE-PRE]"),
    ("</PRE>", "[REDACTED-CLOSE-PRE]"),
    ("<script", "[REDACTED-SCRIPT]"),
    ("</script", "[REDACTED-SCRIPT]"),
)


#: Mermaid node-label syntax uses square brackets and parentheses as
#: structural delimiters. We escape these in dynamic labels to prevent
#: a quoted SHA or email from accidentally terminating the surrounding
#: node syntax. The substitutions are intentionally visible (``&#91;``)
#: rather than removed so the operator can audit the label.
_MERMAID_LABEL_SUBS: tuple[tuple[str, str], ...] = (
    ("\n", " "),
    ("\r", " "),
    ("\"", "'"),
    ("`", "'"),
)


def html_escape(value: Any) -> str:
    """Escape HTML metacharacters in arbitrary input.

    Wraps :func:`html.escape` with ``quote=True`` so both attribute and
    text-node contexts are safe. Coerces non-string input to ``str``
    before escaping (matches the behaviour expected by the renderer's
    f-string call sites).

    Args:
        value: The value to escape. ``None`` is rendered as the empty
            string. Numbers and other primitives are coerced with
            :func:`str`.

    Returns:
        The escaped string.
    """
    if value is None:
        return ""
    return html.escape(str(value), quote=True)


def mermaid_label_safe(value: Any) -> str:
    """Escape a dynamic Mermaid-diagram label.

    Mermaid node labels do not tolerate raw newlines, square brackets,
    parentheses, or quote characters. The renderer interpolates short
    contextual values (SHAs, ISO timestamps, single-word identifiers)
    into Mermaid sources; this helper sanitises those interpolations.

    The function:
        1. Coerces ``value`` to a string (``None`` → empty).
        2. Strips forbidden sequences that could break out of the
           surrounding ``<pre class="mermaid">`` block.
        3. Applies the small set of character substitutions in
           :data:`_MERMAID_LABEL_SUBS`.
        4. Escapes HTML metacharacters so the label cannot smuggle
           tags through the Mermaid renderer when it falls back to
           rendering label text into the DOM.

    Args:
        value: Caller-supplied label content.

    Returns:
        A Mermaid-safe label string.
    """
    if value is None:
        return ""
    text = str(value)
    for needle, replacement in _MERMAID_FORBIDDEN:
        text = text.replace(needle, replacement)
    for needle, replacement in _MERMAID_LABEL_SUBS:
        text = text.replace(needle, replacement)
    return html.escape(text, quote=True)


#: ISO-8601 date-time pattern (UTC ``Z``) used as the trusted shape for
#: dynamic Mermaid date interpolations.
_ISO_Z_RE: re.Pattern[str] = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$"
)


def is_iso_z(value: Any) -> bool:
    """Return ``True`` when ``value`` is a strict ISO-8601 ``Z`` timestamp.

    The renderers use this as a precondition when interpolating timestamps
    into Mermaid sources: a confirmed-ISO value is safe to embed without
    further escaping. Non-conforming values must go through
    :func:`mermaid_label_safe`.

    Args:
        value: Candidate timestamp.

    Returns:
        ``True`` for strings of the form ``YYYY-MM-DDTHH:MM:SSZ``.
    """
    if not isinstance(value, str):
        return False
    return bool(_ISO_Z_RE.match(value))


#: Short-SHA pattern (7-40 hex chars) used as the trusted shape for
#: Mermaid SHA interpolations.
_SHORT_SHA_RE: re.Pattern[str] = re.compile(r"^[a-f0-9]{7,40}$")


def is_short_sha(value: Any) -> bool:
    """Return ``True`` when ``value`` is a 7-40 char lowercase hex SHA."""
    if not isinstance(value, str):
        return False
    return bool(_SHORT_SHA_RE.match(value))


__all__ = [
    "html_escape",
    "is_iso_z",
    "is_short_sha",
    "mermaid_label_safe",
]
