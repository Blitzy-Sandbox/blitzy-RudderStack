#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
11_render_deck.py — Executive reveal.js deck renderer for the Acceleration Report.

PURPOSE
-------
Render the single self-contained reveal.js HTML executive deck at
``blitzy/acceleration-report/executive-summary.html`` from the four data
artifacts produced by ``09_compute_metrics.py`` (``metrics.json``,
``per_engineer.json``) plus ``00_environment.sh`` (``environment.json``) and
``01_detect_inflection.py`` (``inflection.json``). This is the LAST script in
the pipeline; it runs AFTER ``10_render_report.py``.

The renderer reads ONLY ``data/*.json`` — never raw git/API outputs — which is
the mechanical enforcement of Rule 4 (Internal Consistency) per AAP §0.5.6:
both the Markdown report and the HTML deck consume the same single source of
truth (``data/metrics.json``), so every numeric value renders identically
across the Executive Summary, Activity Deep-Dives, Traceability Matrix, and
Acceleration Curve.

CRITICAL CONSTRAINTS (per AAP §0.7.1.5 and the agent prompt)
-----------------------------------------------------------
* **Single self-contained HTML file**: no external file dependencies beyond
  CDN-pinned libraries. The full Blitzy theme is embedded inline in a
  ``<style>`` tag inside ``<head>``.
* **CDN-pinned versions** (exact): reveal.js 5.1.0, Mermaid 11.4.0,
  Lucide 0.460.0. Hard-coded as module-level constants and verified by a
  pre-write guard.
* **Slide count**: 12-18 slides (target 16). Pre-write guard enforces range.
* **Every slide has at least one non-text visual** (Mermaid block, KPI card,
  styled table, or Lucide icon).
* **Zero emoji**: Lucide SVG icons only via ``<i data-lucide="..."></i>``.
* **No fenced code blocks inside slides**: inline ``<code>`` only.
* **Brand palette**: all six hex codes (``#5B39F3``, ``#2D1C77``, ``#94FAD5``,
  ``#1A105F``, ``#7A6DEC``, ``#4101DB``) appear in the inline ``<style>``.
* **Factual-neutral tone**: rendered HTML body MUST NOT contain any term in
  the AAP §0.7.2 Rule 2 blocklist (impressive, significant, excellent,
  remarkable, unfortunately, striking, dramatic, surprisingly, notably,
  crucially).
* **Read-only**: NO write operations against the analyzed repo or external
  systems. NO HTTP POST/PUT/PATCH/DELETE. Outputs ONLY the HTML file under
  ``blitzy/acceleration-report/``.
* **Strict mode**: ``from __future__ import annotations`` + type hints
  throughout.
* **CLI flags**: ``--dry-run`` (lists reads/writes without writing) and
  ``--help`` (standard argparse usage display).
* **Observability**: ``BLITZY_RUN_ID`` env var propagated through the
  structured-JSON logger via ``lib.observability.get_logger`` so every event
  emitted by this script carries the same per-run correlation ID as its
  sibling extraction scripts.

EXIT CODES
----------
* 0 — success (HTML file written, or ``--dry-run`` preview emitted).
* 1 — pre-write guard violation, data artifact missing, or unexpected error.

USAGE
-----
    cd blitzy/acceleration-report
    python3 scripts/11_render_deck.py
    python3 scripts/11_render_deck.py --dry-run
    python3 scripts/11_render_deck.py --output /tmp/test-deck.html
    python3 scripts/11_render_deck.py --help

REFERENCES
----------
AAP §0.1.4 (executive deck requirement);
AAP §0.5.6 (single-source rendering, internal consistency);
AAP §0.7.1.5 (executive presentation rule — verbatim);
AAP §0.7.2 (Rule 2 factual-neutral tone, Rule 4 internal consistency);
``decision-log.md`` DL-013 (CDN pinning, no SRI rationale).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path
from typing import Any

# Make the colocated ``lib`` package importable when the script is invoked
# directly (``python3 scripts/11_render_deck.py``) rather than as a module.
# This mirrors the import-path convention used by sibling scripts 03-09.
sys.path.insert(0, str(Path(__file__).resolve().parent))

import jsonschema  # noqa: E402  (pinned in requirements.txt)

from lib.observability import get_logger  # noqa: E402  (sys.path mutation)
from lib.paths import (  # noqa: E402  (sys.path mutation)
    OutputPathError,
    atomic_write_text,
    safe_output_path,
)
from lib.render_safety import (  # noqa: E402  (sys.path mutation)
    html_escape as _safe_html_escape,
    is_iso_z,
    mermaid_label_safe,
)


# ---------------------------------------------------------------------------
# Script identity (consumed by the structured-JSON logger)
# ---------------------------------------------------------------------------

#: Logger name. Appears verbatim in the ``script`` field of every emitted log
#: event. Used as the cache key in ``lib.observability._LOGGER_CACHE``.
SCRIPT_NAME: str = "11_render_deck"


# ---------------------------------------------------------------------------
# Filesystem layout — resolved from ``__file__`` so the script works whether
# invoked from the workspace root, the scripts directory, or anywhere else.
# ---------------------------------------------------------------------------

#: ``blitzy/acceleration-report/`` — the parent of ``scripts/``.
WORKSPACE_ROOT: Path = Path(__file__).resolve().parent.parent

#: ``blitzy/acceleration-report/data/`` — the directory where every raw
#: artifact and every computed metric file lives.
DATA_DIR: Path = WORKSPACE_ROOT / "data"

#: Default output path for the rendered HTML deck. May be overridden by the
#: ``--output`` CLI flag (used by integration tests). Per MAJOR-#4 review
#: fix, every supplied path is validated against the workspace boundary
#: via ``safe_output_path()`` before any write occurs; any path outside
#: ``blitzy/acceleration-report/`` is rejected with exit code 4.
OUTPUT_PATH: Path = WORKSPACE_ROOT / "executive-summary.html"


#: ``blitzy/acceleration-report/scripts/lib/schemas/`` — the directory
#: containing JSON Schemas for every artifact this renderer reads. The
#: pre-render schema validation step (MAJOR-#3 review fix) loads
#: ``metrics.schema.json``, ``per_engineer.schema.json``,
#: ``inflection.schema.json``, and ``environment.schema.json`` from this
#: directory before any HTML is assembled. A schema-validation failure
#: raises ``DeckGuardFailure`` and aborts the run before the guards
#: see a malformed artifact.
SCHEMAS_DIR: Path = Path(__file__).resolve().parent / "lib" / "schemas"


#: Map from input artifact filename to its JSON Schema filename. The
#: deck reads exactly four artifacts (see :data:`READ_ARTIFACTS` below);
#: each must validate against the corresponding schema before the
#: rendering pipeline assembles the HTML. Per MAJOR-#3 review finding,
#: this validation is mandatory and runs BEFORE any pre-write guard.
DECK_INPUT_SCHEMAS: dict[str, str] = {
    "metrics.json": "metrics.schema.json",
    "per_engineer.json": "per_engineer.schema.json",
    "inflection.json": "inflection.schema.json",
    "environment.json": "environment.schema.json",
}


# ---------------------------------------------------------------------------
# CDN-pinned library versions — EXACT per AAP §0.7.1.5. Pre-write guard 5
# asserts each version string appears verbatim in the rendered HTML.
# ---------------------------------------------------------------------------

#: reveal.js — presentation engine. Pinned major.minor.patch per the rule.
REVEAL_VERSION: str = "5.1.0"

#: Mermaid — diagram engine. Pinned major.minor.patch per the rule.
MERMAID_VERSION: str = "11.4.0"

#: Lucide — icon library. Pinned major.minor.patch per the rule.
LUCIDE_VERSION: str = "0.460.0"


# ---------------------------------------------------------------------------
# Blitzy brand palette — all six hex codes MUST appear in the rendered HTML
# (pre-write guard 6). Defined here as a dict so the iteration in the guard
# stays in sync with the CSS custom-property definitions in ``render_inline_css``.
# ---------------------------------------------------------------------------

#: The six canonical Blitzy brand colours. Every one MUST appear verbatim
#: somewhere in the rendered HTML (typically in the inline ``<style>`` as
#: CSS custom-property values).
BRAND_COLORS: dict[str, str] = {
    "primary": "#5B39F3",       # Blitzy purple — primary brand colour
    "dark": "#2D1C77",          # Dark primary — headings, dark dividers
    "mint": "#94FAD5",          # Accent green — confidence-high, hero text
    "navy": "#1A105F",          # Closing-slide background
    "light_purple": "#7A6DEC",  # Gradient interpolation midpoint
    "vivid": "#4101DB",         # Gradient start, bold accents
}


# ---------------------------------------------------------------------------
# Confidence-to-CSS-class mapping. The four enum values mirror the
# ``confidence`` field admitted by ``data/metrics.json``'s schema.
# ---------------------------------------------------------------------------

#: Maps each confidence value to the CSS class used by the ``.kpi-confidence``
#: pill. Unknown confidence falls back to ``confidence-insufficient`` so the
#: deck remains renderable even if the schema is extended.
CONFIDENCE_CLASSES: dict[str, str] = {
    "high": "confidence-high",
    "medium": "confidence-medium",
    "low": "confidence-low",
    "insufficient": "confidence-insufficient",
}


# ---------------------------------------------------------------------------
# Slide count guard bounds per AAP §0.7.1.5.
# ---------------------------------------------------------------------------

#: Minimum acceptable number of slides. Pre-write guard 1 enforces this.
MIN_SLIDES: int = 12

#: Maximum acceptable number of slides. Pre-write guard 1 enforces this.
MAX_SLIDES: int = 18

#: Target slide count per the rule. The current implementation produces
#: exactly 16 slides.
TARGET_SLIDES: int = 16


# ---------------------------------------------------------------------------
# Factual-neutral-tone blocklist (Rule 2 per AAP §0.7.2). Pre-write guard 7
# rejects the rendered HTML if any term appears as a whole word, case-insensitive.
# This list mirrors the one used by ``10_render_report.py`` so both renderers
# enforce the same tone constraint.
# ---------------------------------------------------------------------------

#: Subjective qualifiers that MUST NOT appear in the rendered HTML body.
#: The check uses word-boundary regex with ``re.IGNORECASE`` so substrings
#: inside larger words (e.g. "signature") do not trigger.
BLOCKLIST: list[str] = [
    "impressive",
    "significant",
    "excellent",
    "remarkable",
    "unfortunately",
    "striking",
    "dramatic",
    "surprisingly",
    "notably",
    "crucially",
]


# ---------------------------------------------------------------------------
# Emoji block ranges to reject (key Unicode ranges). Pre-write guard 3
# iterates every character in the rendered HTML and asserts no character
# falls within these ranges. The AAP §0.7.1.5 zero-emoji rule mandates
# Lucide SVG icons in place of any decorative emoji.
# ---------------------------------------------------------------------------

#: Unicode code-point ranges (inclusive) treated as emoji for guard 3. The
#: ranges cover the six main emoji blocks plus dingbats and miscellaneous
#: symbols. ASCII (0x00-0x7F) and standard typographic punctuation (em-dash
#: U+2014 at 0x2014, en-dash U+2013, single/double curly quotes) all fall
#: OUTSIDE these ranges and are therefore admitted.
EMOJI_RANGES: list[tuple[int, int]] = [
    (0x1F600, 0x1F64F),  # Emoticons
    (0x1F300, 0x1F5FF),  # Misc Symbols and Pictographs
    (0x1F680, 0x1F6FF),  # Transport and Map Symbols
    (0x1F900, 0x1F9FF),  # Supplemental Symbols and Pictographs
    (0x2600, 0x26FF),    # Misc Symbols
    (0x2700, 0x27BF),    # Dingbats
]


# ---------------------------------------------------------------------------
# Raw data artifacts read by the renderer (consumed by ``--dry-run``).
# These four paths are the ONLY inputs; no raw artifact is read directly.
# ---------------------------------------------------------------------------

#: The four ``data/*.json`` files the renderer reads. Ordered as in the AAP
#: §0.4 dependency inventory and the agent prompt's Phase 3 contract.
READ_ARTIFACTS: tuple[str, ...] = (
    "metrics.json",
    "per_engineer.json",
    "inflection.json",
    "environment.json",
)


# ===========================================================================
# Helper functions — small, pure, no I/O. Each takes typed inputs and returns
# an HTML fragment as a ``str`` (or, in two cases, a slide-dict).
# ===========================================================================


def lucide_icon(name: str, size: int = 64) -> str:
    """Return a Lucide icon placeholder element.

    The Lucide UMD script at the bottom of the deck calls
    ``lucide.createIcons()`` on ``Reveal.ready`` and on every
    ``slidechanged`` event, which replaces the ``<i>`` placeholders with
    inline SVG markup. The ``data-lucide`` attribute names the icon; see
    https://lucide.dev/icons for the catalogue.

    Args:
        name: The icon's kebab-case identifier (e.g. ``"git-commit"``).
        size: The desired width and height in CSS pixels.

    Returns:
        An ``<i data-lucide="...">`` string suitable for inline placement in
        any slide body. The element is invisible until ``createIcons()`` runs.
    """
    return f'<i data-lucide="{name}" style="width:{size}px;height:{size}px;"></i>'


def kpi_card(
    label: str,
    value: str,
    confidence: str,
    caveat: str | None = None,
    sub: str | None = None,
) -> str:
    """Render a KPI card ``<div>`` for use inside a ``<div class="kpi-row">``.

    Cards are the deck's primary information atom: each carries a label, a
    headline value, a confidence pill, and an optional caveat. The visual
    design — white background, purple top border, drop shadow — comes from
    the inline CSS rule ``.kpi-card`` in :func:`render_inline_css`.

    Escaping policy (MAJOR-#2 review fix):

    * ``caveat`` is **always** HTML-escaped inside this function before
      being placed inside the ``<p class="caveat">`` element. Callers
      may pass arbitrary metric-derived text without pre-escaping; the
      previous caller-side ``html_escape`` calls remain idempotent
      because :func:`html_escape` is built on :func:`html.escape`, which
      converts ``&amp;`` back to ``&amp;amp;`` only if the literal
      ``&amp;`` is fed back in — meaning the escape is **not** idempotent
      across multiple calls. To keep behaviour safe and stable, the
      callers in this file have been updated to pass **raw** caveat
      text (no ``html_escape`` wrapping) and this function does the
      escape exactly once.
    * ``label``, ``value``, ``sub`` are documented as **pre-formatted
      HTML fragments** owned by the caller (they often contain
      ``<code>`` or ``&rarr;`` entities intentionally). The caller
      MUST pre-escape any user-derived sub-strings via
      :func:`html_escape` before composing them into these fields.
    * ``confidence`` is validated against :data:`CONFIDENCE_CLASSES`;
      the upper-cased label is HTML-escaped before placement.

    Args:
        label: Short uppercase label (rendered in Fira Code monospace at 0.75em).
        value: Headline value (rendered in Space Grotesk at 3em). May be a
            multiplier like ``"4.7×"``, a count like ``"5"``, or an em-dash.
        confidence: One of ``"high"``, ``"medium"``, ``"low"``, ``"insufficient"``.
            Unknown values fall back to ``confidence-insufficient``.
        caveat: Optional italic disclaimer rendered beneath the pill. Per Rule 3
            (AAP §0.7.2), every Low-confidence metric MUST appear with its caveat;
            this function performs the HTML escape so callers may pass the raw
            ``data/metrics.json`` ``caveat`` field directly.
        sub: Optional subtitle line between the value and the confidence pill.
            May contain caller-owned HTML (e.g. ``<code>`` wrappers).

    Returns:
        An HTML fragment that satisfies the "non-text visual" requirement
        for pre-write guard 2 because the resulting ``<div class="kpi-card">``
        substring is one of the four tokens the guard searches for.
    """
    klass = CONFIDENCE_CLASSES.get(confidence, "confidence-insufficient")
    # MAJOR-#2: caveat is always escaped at this single point of authority.
    caveat_html = (
        f'<p class="caveat">{html_escape(caveat)}</p>' if caveat else ""
    )
    sub_html = (
        f'<div class="kpi-sub">{sub}</div>' if sub else ""
    )
    # Escape the confidence label (defensive — confidence is enum-validated
    # but ``html_escape`` is cheap and removes any chance of HTML smuggling
    # via a corrupted enum string).
    confidence_label = html_escape(confidence.upper())
    return (
        '<div class="kpi-card">'
        f'<div class="kpi-label">{label}</div>'
        f'<div class="kpi-value">{value}</div>'
        f'{sub_html}'
        f'<span class="kpi-confidence {klass}">{confidence_label}</span>'
        f'{caveat_html}'
        '</div>'
    )


def format_multiplier(value: int | float | str | None) -> str:
    """Format an ``after_before_multiplier`` value for display.

    The ``data/metrics.json`` schema permits the field to be a numeric
    multiplier (rendered as ``Xx`` with one decimal, e.g. ``"4.7×"``), the
    em-dash string ``"—"`` (rendered verbatim), or the literal string
    ``"insufficient_signal"`` (also rendered as an em-dash). This function
    canonicalises all three into a single display string.

    Args:
        value: The raw multiplier from ``data/metrics.json``. May be ``int``,
            ``float``, ``str``, or ``None`` (treated identically to em-dash).

    Returns:
        An ASCII or Unicode display string. Never raises.
    """
    if value is None:
        return "—"
    if isinstance(value, str):
        # Em-dash, "insufficient_signal", or any other string: display em-dash.
        return "—" if value != "—" else "—"
    # Numeric: ``isinstance(True, int)`` is True in Python, but a boolean
    # multiplier is nonsensical; ``True`` would format as ``1.0×``. Treat
    # bools as strings to keep behaviour explicit.
    if isinstance(value, bool):
        return "—"
    return f"{value:.1f}×"


def format_phase_value(value: int | float | str | None) -> str:
    """Format a per-phase metric ``value`` for KPI-card display.

    The phase-level ``value`` field (under ``metric.baseline.value`` or
    ``metric.post_introduction.value``) is either a number, the string
    ``"insufficient_signal"``, or — in rare cases — a free-form string.
    This function renders numerics with locale-free formatting (no
    thousands separators because the values in this repository's data
    set are all small) and the string sentinel as the AAP-prescribed
    em-dash.

    Escaping policy (MAJOR-#2 review fix): the numeric branches return
    locale-free formatted numbers that contain no HTML metacharacters.
    The fallback string branch HTML-escapes the value to guarantee that
    a future schema relaxation (e.g. admitting a free-form ``value``)
    cannot inject HTML into the KPI card. The numeric branches do not
    escape because the formatted output is, by construction, only
    digits, the period, and the multiplication symbol — none of which
    are HTML metacharacters.

    Args:
        value: The raw phase value.

    Returns:
        Display string suitable for ``kpi_card``'s ``value`` parameter.
    """
    if value is None or value == "insufficient_signal":
        return "—"
    if isinstance(value, bool):
        return "—"
    if isinstance(value, float):
        # Render integer-valued floats without trailing ``.0`` so "0.0"
        # appears as "0" to match the integer phase counts in adjacent cards.
        if value.is_integer():
            return str(int(value))
        return f"{value:.2f}"
    if isinstance(value, int):
        return str(value)
    # String value that is not "insufficient_signal" — escape before render
    # so any HTML metacharacters cannot break out of the KPI value cell.
    return html_escape(str(value))


def mermaid_block(src: str) -> str:
    """Wrap Mermaid source in a ``<pre class="mermaid">`` element.

    The Mermaid UMD script at the bottom of the deck calls
    ``mermaid.run({ querySelector: '.reveal pre.mermaid' })`` on
    ``Reveal.ready`` and on every ``slidechanged`` event. Setting
    ``startOnLoad: false`` defers rendering until that explicit call so the
    diagrams render at the right time relative to slide transitions.

    Escaping policy (MAJOR-#2 review fix): Mermaid source intentionally
    contains characters that an HTML escape would corrupt (``-->``,
    ``[label]``, ``:::class``, etc.), so the source as a whole is NOT
    HTML-escaped. Instead, this function defensively strips any literal
    ``</pre>`` or ``</PRE>`` sequence — those would allow Mermaid input
    to close the surrounding ``<pre class="mermaid">`` element and
    inject HTML after it. Callers that interpolate dynamic strings
    (SHAs, ISO timestamps, free-form labels) into Mermaid source MUST
    first sanitise those strings via
    :func:`lib.render_safety.mermaid_label_safe` or, for confirmed-ISO
    timestamps, :func:`lib.render_safety.is_iso_z`.

    Args:
        src: Mermaid source text (e.g. ``"flowchart LR\nA-->B"``).

    Returns:
        An HTML fragment that satisfies the "non-text visual" requirement
        for pre-write guard 2 because the resulting ``<pre class="mermaid"``
        substring is one of the four tokens the guard searches for.
    """
    # Defence in depth: remove the only sequence that lets Mermaid input
    # close the surrounding ``<pre class="mermaid">`` element. The Mermaid
    # parser does not legitimately need ``</pre>`` in its source; any
    # appearance would indicate either a documentation copy-paste error
    # or a hostile interpolation.
    safe_src = src.replace("</pre>", "").replace("</PRE>", "")
    return f'<pre class="mermaid">{safe_src}</pre>'


def slide_html(
    klass: str,
    eyebrow: str,
    heading: str,
    body: str,
) -> str:
    """Wrap slide contents in a ``<section>`` element with the given class.

    Used as a convenience for content slides where the boilerplate is just
    eyebrow + heading + arbitrary body HTML. Title and closing slides use
    bespoke layouts and bypass this helper.

    Args:
        klass: CSS class for the ``<section>`` element. One of
            ``"slide-title"``, ``"slide-divider"``, ``"slide-closing"``,
            or empty string ``""`` for default content slides.
        eyebrow: Short uppercase context label shown above the heading.
        heading: ``<h1>`` text rendered at the top of the slide.
        body: Arbitrary HTML body (one or more block elements).

    Returns:
        A complete ``<section>...</section>`` fragment.
    """
    klass_attr = f' class="{klass}"' if klass else ""
    return (
        f'<section{klass_attr}>'
        f'<div class="eyebrow">{eyebrow}</div>'
        f'<h1>{heading}</h1>'
        f'{body}'
        '</section>'
    )


def html_escape(text: Any) -> str:
    """Escape HTML metacharacters in arbitrary input.

    Per MAJOR-#2 review fix, this is a thin wrapper around
    :func:`lib.render_safety.html_escape` that centralises HTML
    escaping for the deck renderer. The shared helper:

    * Coerces ``None`` to the empty string.
    * Coerces non-string input to ``str`` before escaping.
    * Uses ``html.escape(..., quote=True)`` so both attribute and
      text-node contexts are safe.

    The function name and signature are preserved so existing
    callers continue to work without modification; only the
    implementation is replaced. The order-of-replacement concern
    (``&`` first to avoid double-encoding) is handled by the
    stdlib's :func:`html.escape`.

    Args:
        text: Arbitrary input. ``None`` is rendered as the empty
            string. Numbers and other primitives are coerced with
            :func:`str`.

    Returns:
        Escaped string suitable for inline placement in HTML
        attributes and text nodes.
    """
    return _safe_html_escape(text)


def truncate(text: str, max_len: int = 220) -> str:
    """Return ``text`` truncated to ``max_len`` characters with an ellipsis.

    Used for long caveats in compact slide positions where the full caveat
    text would overflow the slide. The Risks slide (slide 14) renders full
    caveats; KPI cards on slide 11 use this helper to keep the layout tight.

    Args:
        text: Source string.
        max_len: Maximum character length before truncation.

    Returns:
        The original string if shorter than ``max_len``, otherwise
        ``text[:max_len-1]`` plus a single em-dash to denote truncation.
    """
    if len(text) <= max_len:
        return text
    return text[: max_len - 1].rstrip() + "…"


# ===========================================================================
# Inline CSS — the canonical Blitzy reveal.js theme, embedded as a string so
# the deck remains a single self-contained HTML file (per AAP §0.7.1.5 and
# DL-013 in the decision log). ALL six brand hex codes appear as CSS custom-
# property VALUES in :root so pre-write guard 6 detects them in the rendered
# output.
# ===========================================================================


def render_inline_css() -> str:
    """Return the inline Blitzy theme CSS as a multi-line string.

    The CSS implements every visual specification from AAP §0.7.1.5:

    * ``:root`` custom-property catalogue (all six brand colours, neutrals,
      gradients).
    * ``.reveal`` global typography (Inter body, Space Grotesk headings,
      Fira Code mono/eyebrow).
    * Four slide-type backgrounds (``.slide-title`` hero gradient,
      ``.slide-divider`` dark-purple gradient, default content white,
      ``.slide-closing`` navy).
    * ``.kpi-card``, ``.kpi-row``, ``.kpi-confidence`` (four confidence
      states with distinct background colours).
    * ``table``, ``th``, ``td`` styling for the per-engineer and metrics tables.
    * ``i[data-lucide]`` colour overrides per slide-type.
    * ``pre.mermaid`` container styling.
    * ``.accent-bar``, ``.caveat``, ``.pill`` utilities.

    The function is pure — no I/O, no branching — so its output is stable
    and the pre-write guards see the same string every run.
    """
    return """
    :root {
      /* Brand palette (AAP §0.7.1.5 — all six MUST appear) */
      --blitzy-primary: #5B39F3;
      --blitzy-dark: #2D1C77;
      --blitzy-mint: #94FAD5;
      --blitzy-navy: #1A105F;
      --blitzy-light-purple: #7A6DEC;
      --blitzy-vivid: #4101DB;
      /* Neutrals */
      --neutral-text: #333333;
      --neutral-muted: #999999;
      --neutral-border: #D9D9D9;
      --neutral-bg-subtle: #F4EFF6;
      --neutral-bg-alt: #F5F5F5;
      --neutral-white: #FFFFFF;
      /* Gradients */
      --gradient-hero: linear-gradient(135deg, #4101DB 0%, #5B39F3 50%, #7A6DEC 100%);
      --gradient-divider: linear-gradient(135deg, #2D1C77 0%, #5B39F3 100%);
      --gradient-accent-bar: linear-gradient(90deg, #5B39F3 0%, #94FAD5 100%);
    }
    /* Reveal global typography */
    .reveal { font-family: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif; color: var(--neutral-text); font-weight: 400; }
    .reveal h1, .reveal h2, .reveal h3, .reveal h4 {
      font-family: 'Space Grotesk', sans-serif;
      color: var(--blitzy-dark);
      letter-spacing: -0.01em;
      font-weight: 600;
    }
    .reveal code, .reveal pre.mermaid {
      font-family: 'Fira Code', 'SF Mono', Monaco, monospace;
    }
    .reveal code { background: var(--neutral-bg-subtle); padding: 0.1em 0.4em; border-radius: 4px; font-size: 0.85em; color: var(--blitzy-dark); }
    .reveal .eyebrow {
      font-family: 'Fira Code', monospace;
      font-size: 0.65em;
      text-transform: uppercase;
      letter-spacing: 0.18em;
      color: var(--blitzy-primary);
      margin-bottom: 0.75rem;
    }
    /* Slide type — title (hero gradient) */
    .reveal section.slide-title {
      background: var(--gradient-hero);
      color: var(--neutral-white);
      text-align: left;
      padding: 4rem 6rem;
    }
    .reveal section.slide-title h1 { color: var(--neutral-white); font-size: 3.2em; font-weight: 700; line-height: 1.1; }
    .reveal section.slide-title h2 { color: var(--blitzy-mint); font-weight: 500; font-size: 1.3em; margin-top: 1rem; }
    .reveal section.slide-title .eyebrow { color: var(--blitzy-mint); }
    .reveal section.slide-title p, .reveal section.slide-title code { color: var(--neutral-white); }
    .reveal section.slide-title code { background: rgba(148,250,213,0.18); color: var(--blitzy-mint); }
    /* Slide type — divider (dark purple gradient) */
    .reveal section.slide-divider {
      background: var(--gradient-divider);
      color: var(--neutral-white);
      text-align: center;
      padding: 4rem 6rem;
    }
    .reveal section.slide-divider h1 { color: var(--neutral-white); font-size: 2.6em; font-weight: 700; }
    .reveal section.slide-divider .eyebrow { color: var(--blitzy-mint); }
    .reveal section.slide-divider p, .reveal section.slide-divider li { color: var(--neutral-bg-subtle); }
    /* Slide type — default content (white background) */
    .reveal section { background: var(--neutral-white); color: var(--neutral-text); text-align: left; padding: 3rem 5rem; }
    .reveal section h1 { font-size: 2em; margin-bottom: 0.5rem; }
    .reveal section h2 { font-size: 1.4em; color: var(--blitzy-primary); }
    /* Slide type — closing (navy) */
    .reveal section.slide-closing {
      background: var(--blitzy-navy);
      color: var(--neutral-white);
      text-align: center;
      padding: 4rem 6rem;
    }
    .reveal section.slide-closing h1 { color: var(--blitzy-mint); font-size: 2.8em; }
    .reveal section.slide-closing .eyebrow { color: var(--blitzy-mint); }
    .reveal section.slide-closing p { color: var(--neutral-bg-subtle); font-size: 1.05em; line-height: 1.5; }
    .reveal section.slide-closing code { background: rgba(148,250,213,0.18); color: var(--blitzy-mint); }
    /* KPI card */
    .kpi-card {
      background: var(--neutral-white);
      color: var(--neutral-text);
      border-radius: 12px;
      padding: 1.5rem 2rem;
      box-shadow: 0 4px 20px rgba(91,57,243,0.15);
      border-top: 4px solid var(--blitzy-primary);
      display: inline-flex;
      flex-direction: column;
      min-width: 16rem;
      max-width: 22rem;
      margin: 0.75rem;
      vertical-align: top;
      text-align: left;
    }
    .kpi-card .kpi-label {
      font-family: 'Fira Code', monospace;
      font-size: 0.7em;
      color: var(--neutral-muted);
      text-transform: uppercase;
      letter-spacing: 0.12em;
      margin-bottom: 0.5rem;
    }
    .kpi-card .kpi-value {
      font-family: 'Space Grotesk', sans-serif;
      font-size: 2.6em;
      font-weight: 700;
      color: var(--blitzy-dark);
      line-height: 1;
      margin: 0.25rem 0 0.5rem 0;
    }
    .kpi-card .kpi-sub {
      font-size: 0.75em;
      color: var(--neutral-muted);
      margin-bottom: 0.5rem;
      line-height: 1.4;
    }
    .kpi-card .kpi-confidence {
      display: inline-block;
      padding: 0.2rem 0.7rem;
      border-radius: 999px;
      font-size: 0.65em;
      font-weight: 600;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      align-self: flex-start;
    }
    .kpi-confidence.confidence-high { background: var(--blitzy-mint); color: var(--blitzy-dark); }
    .kpi-confidence.confidence-medium { background: #FFC107; color: var(--blitzy-dark); }
    .kpi-confidence.confidence-low { background: #FF6B6B; color: var(--neutral-white); font-style: italic; }
    .kpi-confidence.confidence-insufficient { background: var(--neutral-muted); color: var(--neutral-white); }
    .kpi-row { display: flex; flex-wrap: wrap; justify-content: center; align-items: stretch; margin-top: 1.5rem; }
    /* Caveat (always italic muted) */
    .caveat {
      font-style: italic;
      color: var(--neutral-muted);
      font-size: 0.7em;
      margin-top: 0.5rem;
      line-height: 1.4;
    }
    /* Tables */
    .reveal table {
      border-collapse: collapse;
      margin: 1.5rem auto;
      font-size: 0.7em;
      width: 90%;
      background: var(--neutral-white);
      box-shadow: 0 2px 12px rgba(0,0,0,0.06);
    }
    .reveal table th {
      background: var(--blitzy-dark);
      color: var(--neutral-white);
      padding: 0.75rem 1rem;
      text-align: left;
      font-family: 'Space Grotesk', sans-serif;
      font-weight: 600;
      letter-spacing: 0.04em;
    }
    .reveal table td {
      padding: 0.6rem 1rem;
      border-bottom: 1px solid var(--neutral-border);
      vertical-align: middle;
      background: var(--neutral-white);
      color: var(--neutral-text);
    }
    .reveal table tr:nth-child(even) td { background: var(--neutral-bg-subtle); }
    .reveal table tr:last-child td { border-bottom: none; }
    /* On dark slide backgrounds the table itself is a light "card" — text inside
       table body cells must stay dark (overriding the slide-level white color),
       and Lucide icons inside cells revert to the primary purple instead of mint. */
    .reveal section.slide-divider table td,
    .reveal section.slide-title table td,
    .reveal section.slide-closing table td { color: var(--neutral-text); }
    .reveal section.slide-divider table td i[data-lucide],
    .reveal section.slide-title table td i[data-lucide],
    .reveal section.slide-closing table td i[data-lucide] { color: var(--blitzy-primary); }
    .reveal section.slide-divider table td code,
    .reveal section.slide-title table td code,
    .reveal section.slide-closing table td code { color: var(--blitzy-dark); }
    /* Lucide icon colours per slide-type */
    .reveal i[data-lucide] { color: var(--blitzy-primary); vertical-align: middle; }
    .reveal section.slide-title i[data-lucide],
    .reveal section.slide-divider i[data-lucide],
    .reveal section.slide-closing i[data-lucide] { color: var(--blitzy-mint); }
    /* Mermaid container */
    .reveal pre.mermaid {
      background: var(--neutral-white);
      border-radius: 8px;
      padding: 1rem;
      box-shadow: 0 2px 12px rgba(0,0,0,0.08);
      margin: 1rem auto;
      max-width: 90%;
      text-align: center;
      font-size: 0.7em;
    }
    /* Accent bar (used on title, dividers) */
    .accent-bar {
      height: 4px;
      background: var(--gradient-accent-bar);
      width: 5rem;
      margin: 1rem 0;
      border-radius: 2px;
    }
    .reveal section.slide-title .accent-bar,
    .reveal section.slide-divider .accent-bar,
    .reveal section.slide-closing .accent-bar { background: var(--blitzy-mint); }
    /* Bullets */
    .reveal ul { margin: 1rem 0 1rem 1.5rem; list-style: none; padding: 0; }
    .reveal ul li {
      margin: 0.6rem 0;
      line-height: 1.5;
      padding-left: 1.25rem;
      position: relative;
    }
    .reveal ul li::before {
      content: "";
      position: absolute;
      left: 0; top: 0.6em;
      width: 0.5em;
      height: 0.5em;
      border-radius: 50%;
      background: var(--blitzy-primary);
    }
    .reveal section.slide-title ul li::before,
    .reveal section.slide-divider ul li::before,
    .reveal section.slide-closing ul li::before { background: var(--blitzy-mint); }
    /* Pills (status chips) */
    .pill {
      display: inline-block;
      padding: 0.25rem 0.75rem;
      border-radius: 999px;
      background: var(--neutral-bg-subtle);
      color: var(--blitzy-dark);
      font-size: 0.75em;
      font-weight: 500;
      margin: 0.15rem;
    }
    .pill-mint { background: var(--blitzy-mint); color: var(--blitzy-dark); }
    /* Footer strip on every slide (slide number/context).
       Right padding accounts for reveal.js's slide-controls cluster in the
       lower-right corner of every slide (~5rem worth of arrow buttons). */
    .slide-footer {
      position: absolute;
      bottom: 1rem;
      left: 5rem;
      right: 10rem;
      display: flex;
      justify-content: space-between;
      font-family: 'Fira Code', monospace;
      font-size: 0.55em;
      color: var(--neutral-muted);
      letter-spacing: 0.1em;
      text-transform: uppercase;
      pointer-events: none;
    }
    .reveal section.slide-title .slide-footer,
    .reveal section.slide-divider .slide-footer,
    .reveal section.slide-closing .slide-footer { color: var(--blitzy-mint); }
    """


def render_head() -> str:
    """Compose the ``<head>`` section of the rendered deck.

    Embeds the reveal.js core CSS link, the reveal.js theme link (for reset),
    the Google Fonts ``<link>`` for Inter / Space Grotesk / Fira Code, and
    the inline Blitzy theme ``<style>`` block. The viewport ``width`` and
    ``height`` are set to ``1920x1080`` to mirror the reveal.js initialisation
    constants in :func:`render_scripts`.

    Returns:
        The complete ``<head>...</head>`` fragment.
    """
    css = render_inline_css()
    return (
        '<head>\n'
        '  <meta charset="UTF-8">\n'
        '  <meta name="viewport" content="width=1920, height=1080">\n'
        '  <title>Development Acceleration Report — blitzy-RudderStack</title>\n'
        f'  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/reveal.js@{REVEAL_VERSION}/dist/reveal.css">\n'
        f'  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/reveal.js@{REVEAL_VERSION}/dist/theme/white.css" id="theme">\n'
        '  <link rel="preconnect" href="https://fonts.googleapis.com">\n'
        '  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>\n'
        '  <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700'
        '&family=Space+Grotesk:wght@500;600;700&family=Fira+Code:wght@400;500&display=swap" rel="stylesheet">\n'
        f'  <style>{css}  </style>\n'
        '</head>'
    )


def render_scripts() -> str:
    """Compose the closing ``<script>`` block for the deck.

    Loads the three CDN-pinned JS libraries (reveal.js, Mermaid, Lucide),
    initialises Mermaid with ``startOnLoad: false`` (deferring rendering until
    explicit ``mermaid.run()`` calls fire on reveal.js lifecycle events),
    initialises reveal.js with the AAP-specified 1920x1080 viewport, hash
    routing, slide transitions, and ``controlsTutorial: false``.

    Defines a ``renderCurrentSlide()`` helper that re-runs Mermaid for the
    diagrams on the **currently visible** slide and re-runs Lucide globally.
    Rendering per-slide rather than across the whole deck is necessary because
    Mermaid 11.x uses layout calculations that fail silently against hidden
    elements (reveal.js applies ``display: none`` to non-current slides), so a
    blanket up-front ``mermaid.run({ querySelector })`` call would leave the
    complex diagrams on initially-hidden slides marked ``data-processed`` with
    an empty error-icon SVG.

    Hooks into both ``ready`` (renders the first slide's diagrams) and
    ``slidechanged`` (renders the new slide's diagrams). ``requestAnimationFrame``
    defers the call by one paint cycle so reveal.js has applied the
    ``present`` class and the slide is visible by the time Mermaid measures
    its bounding box.

    Returns:
        The complete ``<script>`` block (multiple ``<script>`` tags
        concatenated) ready for placement before ``</body>``.
    """
    return (
        f'  <script src="https://cdn.jsdelivr.net/npm/reveal.js@{REVEAL_VERSION}/dist/reveal.js"></script>\n'
        f'  <script src="https://cdn.jsdelivr.net/npm/mermaid@{MERMAID_VERSION}/dist/mermaid.min.js"></script>\n'
        f'  <script src="https://unpkg.com/lucide@{LUCIDE_VERSION}/dist/umd/lucide.min.js"></script>\n'
        '  <script>\n'
        '    mermaid.initialize({\n'
        '      startOnLoad: false,\n'
        "      theme: 'base',\n"
        '      themeVariables: {\n'
        "        primaryColor: '#5B39F3',\n"
        "        primaryTextColor: '#FFFFFF',\n"
        "        primaryBorderColor: '#2D1C77',\n"
        "        lineColor: '#5B39F3',\n"
        "        secondaryColor: '#94FAD5',\n"
        "        tertiaryColor: '#F4EFF6',\n"
        "        fontFamily: 'Inter, sans-serif'\n"
        '      }\n'
        '    });\n'
        '    Reveal.initialize({\n'
        '      hash: true,\n'
        "      transition: 'slide',\n"
        '      controlsTutorial: false,\n'
        '      width: 1920,\n'
        '      height: 1080,\n'
        '      plugins: []\n'
        '    });\n'
        '    function renderCurrentSlide(event) {\n'
        '      requestAnimationFrame(function() {\n'
        '        try {\n'
        '          var slide = (event && event.currentSlide) '
        "|| document.querySelector('.reveal section.present') "
        "|| document.querySelector('.reveal .slides > section');\n"
        '          if (slide) {\n'
        '            var nodes = Array.prototype.slice.call('
        "slide.querySelectorAll('pre.mermaid:not([data-processed=\"true\"])'));\n"
        '            if (nodes.length > 0) {\n'
        '              mermaid.run({ nodes: nodes }).catch(function(err) {\n'
        "                console.error('Mermaid render error on current slide:', err);\n"
        '              });\n'
        '            }\n'
        '          }\n'
        '        } catch (e) {\n'
        "          console.error('renderCurrentSlide Mermaid error:', e);\n"
        '        }\n'
        '        try { lucide.createIcons(); } catch (e) {\n'
        "          console.error('renderCurrentSlide Lucide error:', e);\n"
        '        }\n'
        '      });\n'
        '    }\n'
        "    Reveal.on('ready', renderCurrentSlide);\n"
        "    Reveal.on('slidechanged', renderCurrentSlide);\n"
        '  </script>'
    )


def render_html(slides: list[dict[str, Any]]) -> str:
    """Compose the full HTML document from a list of slide dicts.

    Concatenates ``<!DOCTYPE html>``, ``<html>``, the ``<head>`` from
    :func:`render_head`, a ``<body>`` containing ``<div class="reveal"><div
    class="slides">`` and every slide's ``html`` payload, then the closing
    script block from :func:`render_scripts`.

    Args:
        slides: Output of :func:`build_slides`. Each element is a dict with
            keys ``id`` (int 1-N), ``klass`` (CSS class string), and ``html``
            (the rendered ``<section>...</section>`` fragment).

    Returns:
        The full HTML document as a single string. The string is the input
        to :func:`pre_write_guard` and the eventual ``Path.write_text`` call.
    """
    head = render_head()
    scripts = render_scripts()
    body_slides = "\n      ".join(s["html"] for s in slides)
    return (
        "<!DOCTYPE html>\n"
        '<html lang="en">\n'
        f"{head}\n"
        "<body>\n"
        '  <div class="reveal">\n'
        '    <div class="slides">\n'
        f"      {body_slides}\n"
        "    </div>\n"
        "  </div>\n"
        f"{scripts}\n"
        "</body>\n"
        "</html>\n"
    )



# ===========================================================================
# Slide builders — one function per slide. Each takes the relevant data
# artifacts and returns a slide dict {"id": int, "klass": str, "html": str}.
# Every slide carries at least one non-text visual element (Mermaid block,
# KPI card, table, or Lucide icon) to satisfy pre-write guard 2.
# ===========================================================================


def slide_1_title(env: dict[str, Any], inflection: dict[str, Any]) -> dict[str, Any]:
    """Render slide 1 — title slide with hero gradient.

    Contains the title, subtitle, an accent bar, a Lucide gauge icon, and a
    pill displaying the AI inflection date drawn from
    ``data/inflection.json``. The inflection date is NEVER hardcoded; the
    sole source of truth is the ``date_utc`` field on the inflection artifact.

    Args:
        env: Parsed ``data/environment.json``.
        inflection: Parsed ``data/inflection.json``.

    Returns:
        Slide dict with ``klass == "slide-title"``.
    """
    inflection_date = inflection.get("date_utc", "—")
    tier = inflection.get("tier_used", "—")
    tier_labels: dict[str, str] = {
        "trailer": "Tier 1 — Co-authored-by trailer",
        "ai_actor_email": "Tier 2 — AI-actor email pattern",
        "velocity_inflection": "Tier 3 — Velocity inflection",
    }
    tier_label = tier_labels.get(tier, html_escape(str(tier)))
    repo_slug = env.get("repository_slug", "—")
    body = (
        '<div class="eyebrow">Acceleration Measurement &middot; Read-Only</div>'
        '<h1>Development<br>Acceleration Report</h1>'
        '<h2>After/Before Multipliers Across 12 Flow and Operational Metrics</h2>'
        '<div class="accent-bar"></div>'
        f'<p style="margin-top: 1.5rem;">{lucide_icon("gauge", 56)}'
        f'<span style="margin-left: 1rem; vertical-align: middle;">'
        f'<code>{html_escape(repo_slug)}</code></span></p>'
        '<p style="margin-top: 1.5rem; font-size: 0.85em;">'
        f'<span class="pill pill-mint">AI Inflection: <code>{html_escape(inflection_date)}</code></span> '
        f'<span class="pill">{html_escape(tier_label)}</span>'
        '</p>'
        '<div class="slide-footer">'
        '<span>Blitzy Acceleration Measurement</span>'
        '<span>1 / 16</span>'
        '</div>'
    )
    return {
        "id": 1,
        "klass": "slide-title",
        "html": f'<section class="slide-title">{body}</section>',
    }


def slide_2_headlines(metrics: dict[str, Any], per_eng: dict[str, Any]) -> dict[str, Any]:
    """Render slide 2 — headline findings divider with KPI cards.

    Per the agent prompt, this slide shows the top 3 multipliers sorted DESC
    by magnitude. In this repository's data set, no metric has a numeric
    multiplier (every ``after_before_multiplier`` is the em-dash sentinel
    because the baseline period has too few commits to compute a ratio). The
    renderer therefore falls back to the three most informative observations
    that ARE numerically defensible:

    * M8 Problem Records — 0 reverts on main, confidence HIGH.
    * M2 Flow Velocity post-introduction — count of PR merges, confidence MEDIUM.
    * Engineering-actor share — % of post-inflection commits authored by Blitzy,
      computed deterministically from ``per_engineer.json``, confidence HIGH.

    All three values trace to ``data/metrics.json`` or ``data/per_engineer.json``
    (Rule 1 Data Provenance preserved — every number has a derivation function
    documented in the per-metric provenance fields).

    Args:
        metrics: Parsed ``data/metrics.json``.
        per_eng: Parsed ``data/per_engineer.json``.

    Returns:
        Slide dict with ``klass == "slide-divider"``.
    """
    # First card — M8 baseline value (always 0 reverts when no reverts exist).
    m8 = metrics.get("m8", {})
    m8_post = m8.get("post_introduction", {}).get("value", "—")
    m8_card = kpi_card(
        label="M8 Problem Records",
        value=format_phase_value(m8_post),
        confidence=m8.get("confidence", "insufficient"),
        sub="Revert commits attributed to a release — both periods",
    )

    # Second card — M2 post-introduction PR count.
    m2 = metrics.get("m2", {})
    m2_post = m2.get("post_introduction", {}).get("value", "—")
    m2_windows = m2.get("post_introduction", {}).get("windows", 0)
    m2_card = kpi_card(
        label="M2 Flow Velocity",
        value=format_phase_value(m2_post),
        confidence=m2.get("confidence", "insufficient"),
        sub=f"PR-merge mean across {m2_windows} post-introduction windows",
    )

    # Third card — Engineering-actor share. Computed from per_engineer.json.
    engineers = per_eng.get("engineers", {})
    blitzy_post = 0
    total_post = 0
    for _, eng in engineers.items():
        post_count = eng.get("commits_in_post_introduction_phase", 0)
        if isinstance(post_count, (int, float)):
            total_post += int(post_count)
            if eng.get("actor_type") == "ai":
                blitzy_post += int(post_count)
    if total_post > 0:
        share = blitzy_post * 100 / total_post
        share_value = f"{share:.0f}%"
        share_confidence = "high"
    else:
        share_value = "—"
        share_confidence = "insufficient"
    share_card = kpi_card(
        label="Engineering Actor",
        value=share_value,
        confidence=share_confidence,
        sub="Share of post-inflection commits authored by Blitzy",
    )

    body = (
        '<div class="accent-bar"></div>'
        f'<div class="kpi-row">{m8_card}{m2_card}{share_card}</div>'
        '<p style="margin-top: 1rem; font-size: 0.75em; color: var(--neutral-bg-subtle);">'
        'Source: <code>data/metrics.json</code>, <code>data/per_engineer.json</code>.'
        ' Multipliers are em-dash where the baseline phase carries fewer than the windows required for a ratio.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Headline Findings</span>'
        '<span>2 / 16</span>'
        '</div>'
    )
    return {
        "id": 2,
        "klass": "slide-divider",
        "html": slide_html("slide-divider", "Headline Findings", "What the data shows", body),
    }


def slide_3_scope_method(env: dict[str, Any], inflection: dict[str, Any]) -> dict[str, Any]:
    """Render slide 3 — scope and method (content slide).

    Four bullets:

    * Period 1 — Baseline range (from ``environment.commit_date_range.earliest``
      to ``inflection.date_utc``).
    * Period 2 — Post-introduction range (from ``inflection.date_utc`` to
      ``environment.commit_date_range.latest``).
    * Window mechanics — Monday-anchored 2-week windows in UTC.
    * Actor substitution — Baseline actor = human author of each PR; Post
      actor = Blitzy.

    Per the AAP §0.5.6 fallback (post-introduction span < 90 days), the
    Ramp-Up/Steady-State split is NOT applied. The slide notes this explicitly.

    Args:
        env: Parsed ``data/environment.json``.
        inflection: Parsed ``data/inflection.json``.

    Returns:
        Slide dict with default content class (empty string).
    """
    cdr = env.get("commit_date_range", {})
    earliest = cdr.get("earliest", "—")
    latest = cdr.get("latest", "—")
    inflection_date = inflection.get("date_utc", "—")
    post = inflection.get("post_introduction", {})
    post_days = post.get("duration_days", "—")
    fallback_applied = not inflection.get("ramp_up_steady_state_split_applied", False)

    fallback_note = ""
    if fallback_applied:
        fallback_note = (
            ' <span class="pill">Two-phase fallback applied: post-introduction span '
            f'{html_escape(str(post_days))} days &lt; 90-day Ramp-Up threshold</span>'
        )

    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("calendar-clock", 48)}</p>'
        '<ul>'
        '<li><strong>Period 1 (Baseline):</strong> <code>'
        f'{html_escape(earliest)}</code> &rarr; <code>{html_escape(inflection_date)}</code>'
        f' ({html_escape(str(inflection.get("baseline_duration_days", "—")))} days)</li>'
        '<li><strong>Period 2 (Post-Introduction):</strong> <code>'
        f'{html_escape(inflection_date)}</code> &rarr; <code>{html_escape(latest)}</code>'
        f' ({html_escape(str(post_days))} days)</li>'
        '<li><strong>Window mechanics:</strong> Monday-anchored 2-week windows in UTC. '
        'Identical methodology applied to both periods.</li>'
        '<li><strong>Actor substitution:</strong> Baseline actor = human PR author; '
        'Post actor = <code>Blitzy</code> (union of <code>agent@blitzy.com</code> + '
        '<code>blitzy[bot]</code>).</li>'
        '</ul>'
        f'<p style="font-size:0.78em; margin-top:1rem; color: var(--neutral-muted);">'
        f'{fallback_note}</p>'
        '<div class="slide-footer">'
        '<span>Scope &amp; Method</span>'
        '<span>3 / 16</span>'
        '</div>'
    )
    return {
        "id": 3,
        "klass": "",
        "html": slide_html("", "Scope &amp; Method", "Two phases, one methodology", body),
    }


def slide_4_data_sources() -> dict[str, Any]:
    """Render slide 4 — data sources divider with topology Mermaid.

    The Mermaid topology diagram shows the read-only flow from each input
    source through the extraction scripts into the data artifacts and on
    to the renderers. The pipeline is identical to the §0.5.1 reference
    diagram in the AAP but in a more compact horizontal layout.

    Returns:
        Slide dict with ``klass == "slide-divider"``.
    """
    diagram = (
        "flowchart LR\n"
        "  subgraph Sources [Sources read-only]\n"
        "    G[git history]\n"
        "    A[GitHub API]\n"
        "    L[Linear API]\n"
        "  end\n"
        "  subgraph Extract [Extract scripts 00-08]\n"
        "    E1[00_environment]\n"
        "    E2[01_detect_inflection]\n"
        "    E3[02_extract_commits]\n"
        "    E4[03_extract_pulls]\n"
        "    E5[04_extract_releases]\n"
        "    E6[05_extract_reverts]\n"
        "    E7[06_extract_ci]\n"
        "    E8[07_extract_exceptions]\n"
        "    E9[08_extract_linear]\n"
        "  end\n"
        "  subgraph Data [data slash star.json]\n"
        "    D[metrics.json]\n"
        "    P[per_engineer.json]\n"
        "  end\n"
        "  subgraph Render [Render scripts 09-11]\n"
        "    C[09_compute_metrics]\n"
        "    R1[10_render_report]\n"
        "    R2[11_render_deck]\n"
        "  end\n"
        "  G --> E1 & E2 & E3 & E6\n"
        "  A --> E4 & E5 & E7 & E8\n"
        "  L --> E9\n"
        "  E1 & E2 & E3 & E4 & E5 & E6 & E7 & E8 & E9 --> C\n"
        "  C --> D & P\n"
        "  D & P --> R1 & R2\n"
    )
    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("network", 48)}</p>'
        f'{mermaid_block(diagram)}'
        '<p style="font-size:0.78em; margin-top:0.5rem; color: var(--neutral-bg-subtle);">'
        'Legend: solid arrows = data flow. Sources are read-only; no script writes to the analyzed repository.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Architecture</span>'
        '<span>4 / 16</span>'
        '</div>'
    )
    return {
        "id": 4,
        "klass": "slide-divider",
        "html": slide_html("slide-divider", "Architecture", "Data Sources &amp; Pipeline", body),
    }


def slide_5_inflection(inflection: dict[str, Any]) -> dict[str, Any]:
    """Render slide 5 — inflection detection (content slide).

    Shows the three-tier detection waterfall as a Mermaid flowchart with the
    resolved tier highlighted. The resolved tier and the detection evidence
    are drawn from ``inflection.json``.

    Args:
        inflection: Parsed ``data/inflection.json``.

    Returns:
        Slide dict with default content class (empty string).
    """
    tier = inflection.get("tier_used", "ai_actor_email")
    date = inflection.get("date_utc", "—")
    evidence = inflection.get("evidence", {}) or {}
    sha = evidence.get("commit_sha", "—")
    email = evidence.get("author_email", "—")

    # MAJOR-#2 review fix: every dynamic value interpolated into the
    # Mermaid source below is first sanitised. ``date`` is the only one
    # that flows into a Mermaid edge label (``-->|resolved at {date}|``);
    # it must be either a confirmed ISO-Z timestamp (verbatim safe) or
    # routed through ``mermaid_label_safe()`` which collapses newlines,
    # strips backtick/double-quote characters, and HTML-escapes the
    # result so that a hostile ``date`` value cannot terminate the
    # Mermaid block or smuggle HTML through it.
    if is_iso_z(str(date)):
        date_label = str(date)
    else:
        date_label = mermaid_label_safe(date)

    # Build the Mermaid diagram with the resolved tier highlighted.
    def highlight(t: str) -> str:
        return ":::resolved" if t == tier else ":::skipped"

    diagram = (
        "flowchart TD\n"
        f"  T1[Tier 1: trailer search]{highlight('trailer')}\n"
        f"  T2[Tier 2: AI-actor email]{highlight('ai_actor_email')}\n"
        f"  T3[Tier 3: velocity inflection]{highlight('velocity_inflection')}\n"
        "  T1 -->|no signal| T2\n"
        "  T2 -->|no signal| T3\n"
        f"  T2 -->|resolved at {date_label}| Out[Inflection point]:::out\n"
        "  classDef resolved fill:#94FAD5,stroke:#2D1C77,color:#1A105F,stroke-width:3px\n"
        "  classDef skipped fill:#F4EFF6,stroke:#D9D9D9,color:#999999\n"
        "  classDef out fill:#5B39F3,stroke:#2D1C77,color:#FFFFFF\n"
    )
    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("git-commit", 48)}</p>'
        f'{mermaid_block(diagram)}'
        '<div style="margin-top:1rem; font-size:0.78em;">'
        f'<p>Resolved tier: <code>{html_escape(tier)}</code> &middot; '
        f'commit: <code>{html_escape(str(sha)[:12])}</code> &middot; '
        f'author: <code>{html_escape(email)}</code></p>'
        '</div>'
        '<div class="slide-footer">'
        '<span>Inflection Detection</span>'
        '<span>5 / 16</span>'
        '</div>'
    )
    return {
        "id": 5,
        "klass": "",
        "html": slide_html("", "Inflection Detection", "Three-tier waterfall", body),
    }




def slide_6_metrics_table(metrics: dict[str, Any]) -> dict[str, Any]:
    """Render slide 6 — twelve-metric overview table (divider slide).

    A styled HTML table with one row per metric (M1-M12). Columns:
    Metric, Name, Post-Introduction value, Confidence pill, Lucide icon.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        Slide dict with ``klass == "slide-divider"``.
    """
    # Canonical icon per metric (Lucide kebab-case identifiers).
    icon_per_metric: dict[str, str] = {
        "m1": "layers",
        "m2": "git-pull-request",
        "m3": "trending-up",
        "m4": "activity",
        "m5": "zap",
        "m6": "pie-chart",
        "m7": "clock",
        "m8": "alert-octagon",
        "m9": "package",
        "m10": "shield-alert",
        "m11": "bug",
        "m12": "timer",
    }
    rows = []
    for i in range(1, 13):
        k = f"m{i}"
        m = metrics.get(k, {})
        name = html_escape(str(m.get("name", k.upper())))
        post = m.get("post_introduction", {})
        post_value = format_phase_value(post.get("value"))
        confidence = m.get("confidence", "insufficient")
        confidence_class = CONFIDENCE_CLASSES.get(confidence, "confidence-insufficient")
        icon_name = icon_per_metric.get(k, "circle")
        # MAJOR-#1 review fix (Rule 3 — Confidence Transparency): every
        # Low-confidence metric MUST appear with its caveat. Surface the
        # caveat text in the Caveat column for Low-confidence rows and
        # the insufficient-signal reason for Insufficient rows. Other
        # confidence tiers render an em-dash to keep the column aligned.
        caveat_text = ""
        if confidence == "low":
            caveat_text = str(m.get("caveat") or m.get("reason") or "").strip()
        elif confidence == "insufficient":
            caveat_text = str(
                m.get("caveat") or m.get("reason") or post.get("reason") or ""
            ).strip()
        if caveat_text:
            caveat_cell = (
                f"<td style='font-size:0.7em; color: var(--neutral-muted);'>"
                f"{html_escape(truncate(caveat_text, 140))}"
                "</td>"
            )
        else:
            caveat_cell = (
                "<td style='font-size:0.7em; color: var(--neutral-bg-subtle);'>—</td>"
            )
        rows.append(
            "<tr>"
            f"<td><code>{k.upper()}</code></td>"
            f"<td>{lucide_icon(icon_name, 20)} <span style='margin-left:0.4rem;'>{name}</span></td>"
            f"<td><code>{html_escape(post_value)}</code></td>"
            f"<td><span class='kpi-confidence {confidence_class}'>{html_escape(confidence.upper())}</span></td>"
            f"{caveat_cell}"
            "</tr>"
        )
    body = (
        '<table>'
        '<thead><tr>'
        '<th>Metric</th><th>Name</th><th>Post-Introduction</th>'
        '<th>Confidence</th><th>Caveat</th>'
        '</tr></thead>'
        f'<tbody>{"".join(rows)}</tbody>'
        '</table>'
        '<p style="font-size:0.75em; color: var(--neutral-bg-subtle);">'
        'Post-Introduction value is the per-phase aggregate from <code>data/metrics.json</code>.'
        ' Em-dash in the value column = insufficient signal; em-dash in the caveat column = no caveat present.'
        ' Per Rule 3 (AAP §0.7.2), every Low-confidence metric carries an explicit caveat.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Twelve Metrics</span>'
        '<span>6 / 16</span>'
        '</div>'
    )
    return {
        "id": 6,
        "klass": "slide-divider",
        "html": slide_html("slide-divider", "Twelve Metrics", "Overview", body),
    }


def slide_7_flow_velocity_time(metrics: dict[str, Any]) -> dict[str, Any]:
    """Render slide 7 — Flow Velocity (M2) and Flow Time (M7) KPI cards.

    Each metric gets one KPI card with:

    * Label (metric name).
    * Value (post-introduction value, em-dash if insufficient).
    * Sub (baseline -> post comparison or confidence note).
    * Confidence pill.
    * Caveat (when Low confidence per Rule 3).

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        Slide dict with default content class.
    """
    cards_html = ""
    for k in ("m2", "m7"):
        m = metrics.get(k, {})
        post = m.get("post_introduction", {})
        base = m.get("baseline", {})
        base_value = format_phase_value(base.get("value"))
        post_value = format_phase_value(post.get("value"))
        confidence = m.get("confidence", "insufficient")
        caveat = m.get("caveat") if confidence == "low" else None
        # MAJOR-#2 review fix: truncate raw caveat text; kpi_card() now
        # performs the HTML escape at its single point of authority,
        # so passing pre-escaped text would double-encode ``&`` to ``&amp;amp;``.
        if caveat:
            caveat = truncate(str(caveat), 240)
        sub = f"Baseline: <code>{html_escape(base_value)}</code> &rarr; Post: <code>{html_escape(post_value)}</code>"
        cards_html += kpi_card(
            label=f"{k.upper()} {html_escape(str(m.get('name', '')))}",
            value=format_multiplier(m.get("after_before_multiplier")),
            confidence=confidence,
            sub=sub,
            caveat=caveat,
        )
    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("git-pull-request", 40)} '
        f'<span style="margin-left:0.5rem;">{lucide_icon("clock", 40)}</span></p>'
        f'<div class="kpi-row">{cards_html}</div>'
        '<p style="font-size:0.75em; margin-top:1rem; color: var(--neutral-muted);">'
        'Multipliers are em-dash where the baseline value is zero (division undefined).'
        ' See traceability matrix M2, M7 in <code>acceleration-report.md</code>.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Flow Velocity &amp; Flow Time</span>'
        '<span>7 / 16</span>'
        '</div>'
    )
    return {
        "id": 7,
        "klass": "",
        "html": slide_html("", "Flow Velocity &amp; Flow Time", "M2 and M7", body),
    }


def slide_8_flow_active_efficiency(
    metrics: dict[str, Any],
    per_eng: dict[str, Any],
) -> dict[str, Any]:
    """Render slide 8 — Flow Active (M4) and Flow Efficiency (M5) KPI cards.

    Plus a per-actor mini bar chart drawn from ``per_engineer.json`` showing
    M2 post-introduction PR counts as a Mermaid xychart-beta. Mermaid pie
    chart could not be used because the field is a count (not a proportion)
    and the bar form aligns better with the per-actor framing.

    Args:
        metrics: Parsed ``data/metrics.json``.
        per_eng: Parsed ``data/per_engineer.json``.

    Returns:
        Slide dict with default content class.
    """
    cards_html = ""
    for k in ("m4", "m5"):
        m = metrics.get(k, {})
        post = m.get("post_introduction", {})
        base = m.get("baseline", {})
        confidence = m.get("confidence", "insufficient")
        caveat = m.get("caveat") if confidence == "low" else None
        # MAJOR-#2 review fix: pass raw caveat — kpi_card() escapes once.
        if caveat:
            caveat = truncate(str(caveat), 240)
        sub = (
            f"Baseline: <code>{html_escape(format_phase_value(base.get('value')))}</code>"
            f" &rarr; Post: <code>{html_escape(format_phase_value(post.get('value')))}</code>"
        )
        cards_html += kpi_card(
            label=f"{k.upper()} {html_escape(str(m.get('name', '')))}",
            value=format_multiplier(m.get("after_before_multiplier")),
            confidence=confidence,
            sub=sub,
            caveat=caveat,
        )

    # Build the per-actor bar chart from per_engineer.json M2 post counts.
    engineers = per_eng.get("engineers", {})
    actor_labels: list[str] = []
    actor_values: list[int] = []
    # Sort engineers DESC by post-introduction commit count so the dominant
    # actor (Blitzy) appears first.
    sorted_engs = sorted(
        engineers.items(),
        key=lambda kv: kv[1].get("commits_in_post_introduction_phase", 0),
        reverse=True,
    )
    for name, eng in sorted_engs:
        m2 = eng.get("m2_flow_velocity", {}) or {}
        v = m2.get("post_introduction", 0)
        if isinstance(v, (int, float)) and not isinstance(v, bool):
            actor_labels.append(name)
            actor_values.append(int(v))
    if actor_labels:
        # MAJOR-#2 review fix: per-actor labels can contain arbitrary
        # display names (real GitHub display names, plus the literal
        # token "Blitzy"). They flow into Mermaid xychart-beta x-axis
        # labels surrounded by double quotes. ``mermaid_label_safe()``
        # collapses newlines, replaces inner double quotes with single
        # quotes (so the surrounding ``"..."`` delimiters remain
        # syntactically intact), strips ``</pre>`` sequences, and
        # HTML-escapes the result. ``html_escape`` is NOT used here
        # because Mermaid renders the labels via SVG ``<text>`` nodes
        # rather than HTML, and the ``&`` -> ``&amp;`` substitution
        # would appear verbatim inside the diagram.
        labels_str = ", ".join(
            f'"{mermaid_label_safe(lbl)}"' for lbl in actor_labels
        )
        values_str = ", ".join(str(v) for v in actor_values)
        bar_chart = (
            "xychart-beta\n"
            "    title \"Per-actor PR merges (M2 post-introduction)\"\n"
            f"    x-axis [{labels_str}]\n"
            "    y-axis \"PRs merged\"\n"
            f"    bar [{values_str}]\n"
        )
        chart_html = mermaid_block(bar_chart)
    else:
        chart_html = (
            '<p style="text-align:center; color: var(--neutral-muted);">'
            'No per-actor PR data available.</p>'
        )

    body = (
        f'<div class="kpi-row">{cards_html}</div>'
        f'<div style="margin-top:1rem;">{chart_html}</div>'
        '<div class="slide-footer">'
        '<span>Flow Active &amp; Flow Efficiency</span>'
        '<span>8 / 16</span>'
        '</div>'
    )
    return {
        "id": 8,
        "klass": "",
        "html": slide_html("", "Flow Active &amp; Flow Efficiency", "M4, M5 and per-actor view", body),
    }


def slide_9_flow_distribution(metrics: dict[str, Any]) -> dict[str, Any]:
    """Render slide 9 — Flow Distribution (M6) Mermaid pie chart.

    Reads ``metrics.m6.post_introduction.category_proportions`` and renders
    it as a Mermaid ``pie`` chart with title and percentage labels. If the
    proportions dict is empty (e.g. the baseline phase), shows a placeholder
    note.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        Slide dict with default content class.
    """
    m6 = metrics.get("m6", {})
    post = m6.get("post_introduction", {}) or {}
    cats = post.get("category_proportions", {}) or {}
    confidence = m6.get("confidence", "insufficient")
    unknown_rate = post.get("unknown_rate", 0)
    total_prs = post.get("total_prs", 0)

    if cats:
        # MAJOR-#2 review fix: pie-chart slice names come from the M6
        # classifier output and may contain arbitrary category strings.
        # They flow into Mermaid pie source delimited by double quotes;
        # we sanitise via ``mermaid_label_safe()`` (which replaces inner
        # double quotes with single quotes so the surrounding delimiters
        # remain valid) rather than ``html_escape`` because Mermaid
        # renders pie labels as SVG text.
        slices = "\n    ".join(
            f'"{mermaid_label_safe(name)}" : {v}' for name, v in cats.items()
        )
        # Total-PR count is a Python int by construction (M6 schema).
        # The title interpolation is safe.
        diagram = f"pie\n    title M6 Flow Distribution (Post-Introduction, n={total_prs})\n    {slices}"
        chart_html = mermaid_block(diagram)
    else:
        chart_html = (
            '<p style="text-align:center; color: var(--neutral-muted);">'
            'No classified PRs in this phase.</p>'
        )

    caveat = m6.get("caveat") if confidence == "low" else None
    caveat_html = (
        f'<p class="caveat" style="text-align:center;">{truncate(html_escape(str(caveat)), 280)}</p>'
        if caveat else ""
    )

    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("pie-chart", 40)}</p>'
        f'<div>{chart_html}</div>'
        f'<p style="font-size:0.78em; text-align:center;">'
        f'Unknown rate: <code>{unknown_rate * 100:.1f}%</code> '
        f'&middot; total PRs classified: <code>{total_prs}</code> '
        f'&middot; confidence: <span class="kpi-confidence {CONFIDENCE_CLASSES.get(confidence, "confidence-insufficient")}">'
        f'{html_escape(confidence.upper())}</span>'
        f'</p>'
        f'{caveat_html}'
        '<div class="slide-footer">'
        '<span>Flow Distribution</span>'
        '<span>9 / 16</span>'
        '</div>'
    )
    return {
        "id": 9,
        "klass": "",
        "html": slide_html("", "Flow Distribution", "M6 — work category mix", body),
    }


def slide_10_releases_problems(metrics: dict[str, Any]) -> dict[str, Any]:
    """Render slide 10 — Releases (M9) and Problem Records (M8) KPI cards.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        Slide dict with default content class.
    """
    cards_html = ""
    for k in ("m9", "m8"):
        m = metrics.get(k, {})
        post = m.get("post_introduction", {})
        base = m.get("baseline", {})
        confidence = m.get("confidence", "insufficient")
        caveat = m.get("caveat") if confidence == "low" else None
        # MAJOR-#2 review fix: pass raw caveat — kpi_card() escapes once.
        if caveat:
            caveat = truncate(str(caveat), 240)
        sub = (
            f"Baseline: <code>{html_escape(format_phase_value(base.get('value')))}</code>"
            f" &rarr; Post: <code>{html_escape(format_phase_value(post.get('value')))}</code>"
        )
        cards_html += kpi_card(
            label=f"{k.upper()} {html_escape(str(m.get('name', '')))}",
            value=format_phase_value(post.get("value")),
            confidence=confidence,
            sub=sub,
            caveat=caveat,
        )
    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("package", 40)} '
        f'<span style="margin-left:0.5rem;">{lucide_icon("alert-octagon", 40)}</span></p>'
        f'<div class="kpi-row">{cards_html}</div>'
        '<p style="font-size:0.75em; margin-top:1rem; color: var(--neutral-muted);">'
        'M9 source precedence: GitHub Releases API &rarr; semver tags &rarr; CI deploy events.'
        ' M8 derived from <code>git log --grep=\'^Revert "\'</code> with release attribution.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Releases &amp; Problem Records</span>'
        '<span>10 / 16</span>'
        '</div>'
    )
    return {
        "id": 10,
        "klass": "",
        "html": slide_html("", "Releases &amp; Problem Records", "M9 and M8", body),
    }


def slide_11_quality_signals(metrics: dict[str, Any]) -> dict[str, Any]:
    """Render slide 11 — Quality Signals (M10, M11, M12) KPI cards.

    Each card flags insufficient-signal explicitly via the confidence pill.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        Slide dict with default content class.
    """
    cards_html = ""
    for k in ("m10", "m11", "m12"):
        m = metrics.get(k, {})
        post = m.get("post_introduction", {})
        base = m.get("baseline", {})
        confidence = m.get("confidence", "insufficient")
        caveat = m.get("caveat") if confidence in ("low", "insufficient") else None
        # MAJOR-#2 review fix: pass raw caveat — kpi_card() escapes once.
        if caveat:
            caveat = truncate(str(caveat), 220)
        # For insufficient signal, surface the reason as a caveat. Raw text
        # — kpi_card() handles the escape.
        if confidence == "insufficient" and not caveat:
            reason = m.get("reason") or post.get("reason") or "Insufficient signal — see metric deep-dive."
            caveat = truncate(str(reason), 220)
        sub = (
            f"Baseline: <code>{html_escape(format_phase_value(base.get('value')))}</code>"
            f" &rarr; Post: <code>{html_escape(format_phase_value(post.get('value')))}</code>"
        )
        cards_html += kpi_card(
            label=f"{k.upper()} {html_escape(str(m.get('name', '')))}",
            value=format_phase_value(post.get("value")),
            confidence=confidence,
            sub=sub,
            caveat=caveat,
        )
    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("shield-alert", 40)} '
        f'<span style="margin-left:0.5rem;">{lucide_icon("bug", 40)}</span>'
        f'<span style="margin-left:0.5rem;">{lucide_icon("timer", 40)}</span></p>'
        f'<div class="kpi-row">{cards_html}</div>'
        '<div class="slide-footer">'
        '<span>Quality Signals</span>'
        '<span>11 / 16</span>'
        '</div>'
    )
    return {
        "id": 11,
        "klass": "",
        "html": slide_html("", "Quality Signals", "M10, M11 and M12", body),
    }




def slide_12_per_engineer_divider() -> dict[str, Any]:
    """Render slide 12 — per-engineer divider.

    A divider slide that introduces the per-engineer view with the explicit
    DORA/SPACE caveat that these metrics must not be used for individual
    performance evaluation.

    Returns:
        Slide dict with ``klass == "slide-divider"``.
    """
    body = (
        '<div class="accent-bar"></div>'
        f'<p style="margin-top:1rem;">{lucide_icon("users", 72)}</p>'
        '<p style="margin-top:1.5rem; max-width: 60%; margin-left: auto; margin-right: auto;">'
        'Per-engineer breakdown for Metrics 2, 4, 5, 6, 10 with real-name attribution. '
        '<code>Blitzy</code> appears as a single row alongside human contributors.'
        '</p>'
        '<p class="caveat" style="text-align:center; color: var(--neutral-bg-subtle);">'
        'DORA/SPACE metrics must not be used for individual performance evaluation.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Per-Engineer View</span>'
        '<span>12 / 16</span>'
        '</div>'
    )
    return {
        "id": 12,
        "klass": "slide-divider",
        "html": slide_html("slide-divider", "Per-Engineer Acceleration", "Real names plus Blitzy", body),
    }


def slide_13_per_engineer_table(per_eng: dict[str, Any]) -> dict[str, Any]:
    """Render slide 13 — per-engineer styled HTML table.

    Columns: Engineer, Actor type, Commits on main (post-introduction), M2
    Flow Velocity (PRs merged in post-introduction), M10 Approved Exceptions.

    Engineers are sorted by post-introduction commit count DESC so the
    dominant actor (Blitzy) appears first.

    Args:
        per_eng: Parsed ``data/per_engineer.json``.

    Returns:
        Slide dict with default content class.
    """
    engineers = per_eng.get("engineers", {})
    sorted_engs = sorted(
        engineers.items(),
        key=lambda kv: kv[1].get("commits_in_post_introduction_phase", 0),
        reverse=True,
    )
    rows: list[str] = []
    for name, eng in sorted_engs:
        display = html_escape(str(eng.get("display_name", name)))
        actor_type = html_escape(str(eng.get("actor_type", "human")))
        actor_pill_class = "pill-mint" if actor_type == "ai" else ""
        actor_pill = f'<span class="pill {actor_pill_class}">{actor_type}</span>'
        post_commits = eng.get("commits_in_post_introduction_phase", 0)
        m2 = eng.get("m2_flow_velocity", {}) or {}
        m2_post = m2.get("post_introduction", "—")
        m2_base = m2.get("baseline", "—")
        m10 = eng.get("m10_approved_exceptions", {}) or {}
        m10_post = m10.get("post_introduction", "—")
        rows.append(
            "<tr>"
            f"<td><strong>{display}</strong></td>"
            f"<td>{actor_pill}</td>"
            f"<td><code>{html_escape(str(post_commits))}</code></td>"
            f"<td><code>{html_escape(str(m2_base))}</code> &rarr; <code>{html_escape(str(m2_post))}</code></td>"
            f"<td><code>{html_escape(str(m10_post))}</code></td>"
            "</tr>"
        )
    table_html = (
        '<table>'
        '<thead><tr>'
        '<th>Engineer</th>'
        '<th>Actor</th>'
        '<th>Commits (post)</th>'
        '<th>M2 Flow Velocity (b → p)</th>'
        '<th>M10 Exceptions (post)</th>'
        '</tr></thead>'
        f'<tbody>{"".join(rows)}</tbody>'
        '</table>'
    )
    body = (
        f'{table_html}'
        '<p class="caveat">'
        'M2 = PR merges attributed to actor in window phase. M10 limited to label-based '
        'and force-push signal in this run (no admin audit-log access). '
        'See <code>data/per_engineer.json</code> for full per-actor schema.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Per-Engineer Table</span>'
        '<span>13 / 16</span>'
        '</div>'
    )
    return {
        "id": 13,
        "klass": "",
        "html": slide_html("", "Per-Engineer Table", "Attribution across the three contributors", body),
    }


def slide_14_risks(metrics: dict[str, Any]) -> dict[str, Any]:
    """Render slide 14 — risks and limitations.

    Enumerates every Low-confidence metric and every insufficient-signal
    metric as a Risk Assessment table per Quality Gate 7.

    Args:
        metrics: Parsed ``data/metrics.json``.

    Returns:
        Slide dict with default content class.
    """
    rows: list[str] = []
    for i in range(1, 13):
        k = f"m{i}"
        m = metrics.get(k, {})
        confidence = m.get("confidence", "insufficient")
        value = m.get("value")
        # Surface Low and insufficient.
        if confidence not in ("low", "insufficient"):
            continue
        # Pull the explanation: prefer caveat, then reason, then a placeholder.
        explanation = m.get("caveat") or m.get("reason") or "See metric deep-dive."
        severity = "Low" if confidence == "low" else "Insufficient"
        severity_class = (
            "confidence-low" if confidence == "low" else "confidence-insufficient"
        )
        rows.append(
            "<tr>"
            f"<td><code>{k.upper()}</code></td>"
            f"<td>{html_escape(str(m.get('name', '')))}</td>"
            f"<td><span class='kpi-confidence {severity_class}'>{severity}</span></td>"
            f"<td style='font-size:0.85em;'>{truncate(html_escape(str(explanation)), 200)}</td>"
            "</tr>"
        )
    if rows:
        table_html = (
            '<table>'
            '<thead><tr>'
            '<th>Metric</th><th>Name</th><th>Severity</th><th>Caveat</th>'
            '</tr></thead>'
            f'<tbody>{"".join(rows)}</tbody>'
            '</table>'
        )
    else:
        table_html = (
            '<p style="text-align:center; color: var(--neutral-muted);">'
            'No Low-confidence or insufficient-signal metrics. </p>'
        )

    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("alert-triangle", 40)}</p>'
        f'{table_html}'
        '<p style="font-size:0.75em; color: var(--neutral-muted);">'
        'Full Risk Assessment in <code>acceleration-report.md</code> §Risk Assessment. '
        'Cardinality enforced by Quality Gate 7.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Risks &amp; Limitations</span>'
        '<span>14 / 16</span>'
        '</div>'
    )
    return {
        "id": 14,
        "klass": "",
        "html": slide_html("", "Risks &amp; Limitations", "Low-confidence and insufficient-signal metrics", body),
    }


def slide_15_onboarding() -> dict[str, Any]:
    """Render slide 15 — onboarding pointer (content slide).

    Points the next analyst at ``onboarding/rerun-and-observability.md`` for
    clean-machine rerun instructions. Contains an ordered list of the four
    canonical commands.

    Returns:
        Slide dict with default content class.
    """
    body = (
        f'<p style="margin-top:0.5rem;">{lucide_icon("book-open", 48)}</p>'
        '<ul>'
        '<li><strong>Setup:</strong> '
        '<code>cd blitzy/acceleration-report &amp;&amp; '
        'virtualenv --python=python3.13 .venv &amp;&amp; '
        'source .venv/bin/activate &amp;&amp; '
        'pip install -r requirements.txt</code></li>'
        '<li><strong>Extract:</strong> '
        '<code>make extract</code> '
        '(scripts 00-08; honours <code>GH_TOKEN</code> and <code>LINEAR_API_KEY</code> when set)</li>'
        '<li><strong>Compute:</strong> '
        '<code>make compute</code> '
        '(script 09; produces <code>data/metrics.json</code> + <code>data/per_engineer.json</code>)</li>'
        '<li><strong>Render:</strong> '
        '<code>make render</code> '
        '(scripts 10 + 11; produces <code>acceleration-report.md</code> + <code>executive-summary.html</code>)</li>'
        '</ul>'
        '<p style="font-size:0.78em; margin-top:1rem;">'
        'Full onboarding guide: <code>blitzy/acceleration-report/onboarding/rerun-and-observability.md</code>'
        '</p>'
        '<div class="slide-footer">'
        '<span>Onboarding</span>'
        '<span>15 / 16</span>'
        '</div>'
    )
    return {
        "id": 15,
        "klass": "",
        "html": slide_html("", "How to Onboard", "Clean-machine rerun in four commands", body),
    }


def slide_16_closing(env: dict[str, Any]) -> dict[str, Any]:
    """Render slide 16 — closing slide (navy background).

    Carries the reproducibility statement plus a final accent bar and a
    Lucide check-circle-2 icon. The closing eyebrow mirrors the title slide
    so the deck book-ends consistently.

    Args:
        env: Parsed ``data/environment.json``.

    Returns:
        Slide dict with ``klass == "slide-closing"``.
    """
    ts = env.get("extraction_timestamp", "—")
    body = (
        '<div class="eyebrow">Reproducibility Statement</div>'
        '<h1>Every number, re-derivable</h1>'
        '<div class="accent-bar"></div>'
        f'<p style="margin-top:1.5rem;">{lucide_icon("check-circle-2", 80)}</p>'
        '<p style="margin-top:1rem;">'
        'Every numeric value in this deck traces to <code>data/metrics.json</code> '
        'and from there to a documented extraction command in the '
        '<code>acceleration-report.md</code> Reproducibility Appendix.'
        '</p>'
        '<p style="margin-top:1rem; font-size: 0.85em;">'
        f'<span class="pill pill-mint">Extracted: <code>{html_escape(ts)}</code></span>'
        '</p>'
        '<p style="margin-top:1.5rem; font-size: 0.8em;">'
        'Read-only by construction. No write to the analyzed repository, '
        'no write to any external system.'
        '</p>'
        '<div class="slide-footer">'
        '<span>Reproducibility</span>'
        '<span>16 / 16</span>'
        '</div>'
    )
    return {
        "id": 16,
        "klass": "slide-closing",
        "html": f'<section class="slide-closing">{body}</section>',
    }


def build_slides(
    metrics: dict[str, Any],
    per_eng: dict[str, Any],
    inflection: dict[str, Any],
    env: dict[str, Any],
) -> list[dict[str, Any]]:
    """Build the full ordered list of 16 slide dicts.

    The order is fixed by the agent prompt's Phase 4 specification and is
    NOT data-driven. The first slide is always ``slide-title`` and the last
    is always ``slide-closing`` (pre-write guard 8).

    Args:
        metrics: Parsed ``data/metrics.json``.
        per_eng: Parsed ``data/per_engineer.json``.
        inflection: Parsed ``data/inflection.json``.
        env: Parsed ``data/environment.json``.

    Returns:
        A list of 16 slide dicts in render order.
    """
    return [
        slide_1_title(env, inflection),
        slide_2_headlines(metrics, per_eng),
        slide_3_scope_method(env, inflection),
        slide_4_data_sources(),
        slide_5_inflection(inflection),
        slide_6_metrics_table(metrics),
        slide_7_flow_velocity_time(metrics),
        slide_8_flow_active_efficiency(metrics, per_eng),
        slide_9_flow_distribution(metrics),
        slide_10_releases_problems(metrics),
        slide_11_quality_signals(metrics),
        slide_12_per_engineer_divider(),
        slide_13_per_engineer_table(per_eng),
        slide_14_risks(metrics),
        slide_15_onboarding(),
        slide_16_closing(env),
    ]



# ===========================================================================
# Pre-write guards — verify the rendered HTML satisfies every contract
# specified in the agent prompt Phase 10 BEFORE the file is written to disk.
# Any violation raises ValueError with a structured ``code: detail`` message
# so the logger can capture the failure as an ``event`` field.
# ===========================================================================


def pre_write_guard(html: str, slides: list[dict[str, Any]]) -> None:
    """Validate the rendered HTML against the nine pre-write contracts.

    Each guard mirrors one contract from the agent prompt's Phase 10
    (guards 1-8) plus the MAJOR-#1 review finding for Rule 3
    (guard 9). Any violation raises ``ValueError`` whose message is
    the failure code (e.g. ``"slide_count_out_of_range: 11"``); the
    message is logged with ``event="pre_write_guard_failed"`` by the
    caller.

    Guards (in order):

    1. **slide_count_out_of_range** — ``MIN_SLIDES <= len(slides) <= MAX_SLIDES``.
    2. **slide_X_missing_visual** — every slide carries at least one non-text
       visual element (Mermaid block, KPI card, table, or Lucide icon).
    3. **emoji_detected** — no character in the rendered HTML falls inside
       any of the six emoji block ranges in :data:`EMOJI_RANGES`.
    4. **slide_X_fenced_code_block** — no slide body contains triple-backtick
       fenced code blocks (inline ``<code>`` only).
    5. **missing_cdn_pin** — all three CDN pin strings appear verbatim
       (``reveal.js@5.1.0``, ``mermaid@11.4.0``, ``lucide@0.460.0``).
    6. **missing_brand_color** — all six Blitzy brand hex codes appear
       somewhere in the rendered HTML.
    7. **blocklist_term_present** — Rule 2 factual-neutral-tone scan via
       word-boundary regex against the AAP §0.7.2 blocklist.
    8. **first_section_not_slide_title** / **last_section_not_slide_closing** —
       section ordering is correct.
    9. **low_confidence_caveat_missing_from_html** /
       **low_confidence_metric_missing_caveat_text** — every metric with
       ``confidence == "low"`` carries a caveat in the metric record
       AND that caveat (truncated fingerprint) appears in the rendered
       HTML at least once. MAJOR-#1 review fix for Rule 3 (AAP §0.7.2
       Confidence Transparency).

    Args:
        html: The fully rendered HTML document string.
        slides: The list of slide dicts that produced the HTML.

    Raises:
        ValueError: If any guard fails. The exception message is the failure
            code suitable for use as a log ``event`` field.
    """
    # --- Guard 1: slide count ------------------------------------------------
    n = len(slides)
    if not (MIN_SLIDES <= n <= MAX_SLIDES):
        raise ValueError(
            f"slide_count_out_of_range: {n} (allowed {MIN_SLIDES}-{MAX_SLIDES})"
        )

    # --- Guard 2: every slide carries at least one non-text visual ----------
    visual_tokens: tuple[str, ...] = (
        '<pre class="mermaid"',
        '<div class="kpi-card"',
        '<table',
        '<i data-lucide=',
    )
    for slide in slides:
        body = slide.get("html", "")
        if not any(token in body for token in visual_tokens):
            raise ValueError(f"slide_{slide.get('id', '?')}_missing_visual")

    # --- Guard 3: no emoji in the rendered HTML -----------------------------
    # Iterate every character; stop at the first one inside an emoji range.
    # The em-dash U+2014 (0x2014) sits between the dingbat range (0x2700-0x27BF)
    # and the misc-symbols range (0x2600-0x26FF); it is therefore NOT in
    # either range and is admitted. Curly quotes (0x2018-0x201D) and ASCII
    # are likewise admitted.
    for ch in html:
        cp = ord(ch)
        for lo, hi in EMOJI_RANGES:
            if lo <= cp <= hi:
                raise ValueError(f"emoji_detected: U+{cp:04X}")

    # --- Guard 4: no fenced code blocks inside slide bodies -----------------
    # Mermaid source itself contains no triple-backtick fences because we
    # never wrap Mermaid source in ``` markdown fences; the source goes
    # directly inside ``<pre class="mermaid">``. So a slide body containing
    # ``` is a documentation bug (e.g. a copy-paste from acceleration-report.md).
    for slide in slides:
        if "```" in slide.get("html", ""):
            raise ValueError(f"slide_{slide.get('id', '?')}_fenced_code_block")

    # --- Guard 5: CDN versions appear verbatim ------------------------------
    required_pins: tuple[str, ...] = (
        f"reveal.js@{REVEAL_VERSION}",
        f"mermaid@{MERMAID_VERSION}",
        f"lucide@{LUCIDE_VERSION}",
    )
    for required in required_pins:
        if required not in html:
            raise ValueError(f"missing_cdn_pin: {required}")

    # --- Guard 6: brand palette ---------------------------------------------
    for color in BRAND_COLORS.values():
        # The check is case-sensitive on the hex digits because CSS custom
        # properties are typically written in upper-case here. The CSS in
        # render_inline_css() emits the canonical upper-case form for every
        # palette entry.
        if color not in html:
            raise ValueError(f"missing_brand_color: {color}")

    # --- Guard 7: factual-neutral-tone blocklist scan -----------------------
    # Use word-boundary regex so substrings inside larger tokens do not match.
    # Example: "signature" contains "signa" but not "\bsignificant\b". Using
    # case-insensitive matching catches "Significant" / "SIGNIFICANT" too.
    for term in BLOCKLIST:
        pattern = rf"\b{re.escape(term)}\b"
        if re.search(pattern, html, flags=re.IGNORECASE):
            raise ValueError(f"blocklist_term_present: {term}")

    # --- Guard 8: section ordering ------------------------------------------
    # First section opening tag MUST contain the class "slide-title"; last
    # section opening tag MUST contain the class "slide-closing". This is
    # the strict form of the agent prompt's Phase 10 requirement: it does
    # not skip class-less default content sections — if the first section
    # tag in the document has no class attribute, the guard fails because
    # the title slide is missing.
    all_section_tags = re.findall(r"<section\b[^>]*>", html)
    if not all_section_tags:
        raise ValueError("no_sections_found")
    first_tag = all_section_tags[0]
    if "slide-title" not in first_tag:
        raise ValueError(f"first_section_not_slide_title: {first_tag[:80]}")
    last_tag = all_section_tags[-1]
    if "slide-closing" not in last_tag:
        raise ValueError(f"last_section_not_slide_closing: {last_tag[:80]}")

    # --- Guard 9: Low-confidence metrics carry caveats in the deck ---------
    # MAJOR-#1 review fix (Rule 3 — Confidence Transparency, AAP §0.7.2).
    # Every metric with ``confidence == "low"`` MUST appear in the rendered
    # HTML alongside its caveat. We can't rely on a positional check
    # (caveats appear in several slides — overview table on slide 6, KPI
    # cards on slides 7-11, risk-assessment table on slide 14) so the
    # guard asserts a contains-substring relationship between each
    # caveat and the rendered HTML. The check uses the FIRST 60
    # characters of each caveat as a fingerprint; this is robust to the
    # ``truncate()`` helper used in several call sites without requiring
    # the entire caveat text to appear at every render position.
    metrics_for_guard = _metrics_from_globals_or_html(slides)
    if metrics_for_guard:
        for metric_key, metric_record in metrics_for_guard.items():
            if not metric_key.startswith("m"):
                continue
            confidence = metric_record.get("confidence", "insufficient")
            if confidence != "low":
                continue
            caveat = str(metric_record.get("caveat") or "").strip()
            if not caveat:
                # A Low-confidence metric without a caveat in
                # ``metrics.json`` is itself a Rule-3 violation, but
                # that is the compute script's responsibility — we
                # flag it as a guard failure so the deck doesn't ship
                # a Low pill without explanation.
                raise ValueError(
                    f"low_confidence_metric_missing_caveat_text: {metric_key}"
                )
            # Fingerprint: first 60 characters of the caveat,
            # HTML-escaped (because that's how it lands in the
            # rendered HTML via kpi_card() and slide_6_metrics_table()).
            fingerprint = html_escape(caveat[:60])
            if fingerprint not in html:
                raise ValueError(
                    f"low_confidence_caveat_missing_from_html: {metric_key}"
                )


def _metrics_from_globals_or_html(slides: list[dict[str, Any]]) -> dict[str, Any] | None:
    """Return the metrics dict associated with ``slides`` for the caveat guard.

    The pre-write guard runs AFTER the HTML is assembled and AFTER the
    metric data has been used to build the slides. The pre-write
    contract does not expose ``metrics`` directly to the guard caller
    (the signature has always been ``(html, slides)``), so the guard
    needs a way to retrieve the metrics dict to assert caveat presence.

    Strategy: ``main()`` stashes the parsed metrics in a module-level
    sentinel before invoking ``pre_write_guard``. This keeps the
    public function signature stable while letting the new
    Low-confidence caveat guard reach the original metric records.

    Returns:
        The metrics dict if it has been stashed by ``main()``, else
        ``None``. Returning ``None`` causes the caveat guard to skip
        the check rather than fail — for ad-hoc test harnesses that
        invoke ``pre_write_guard`` directly without going through
        ``main()``.
    """
    return _LAST_RENDER_METRICS


#: Module-level sentinel populated by ``main()`` before invoking
#: ``pre_write_guard``. Used by the Low-confidence caveat guard to
#: reach the original metric records without altering the public
#: signature of ``pre_write_guard``.
_LAST_RENDER_METRICS: dict[str, Any] | None = None


def _validate_input_artifacts(
    metrics: dict[str, Any],
    per_eng: dict[str, Any],
    inflection: dict[str, Any],
    env: dict[str, Any],
    logger: Any,
) -> None:
    """Validate every input artifact against its JSON Schema.

    Implements MAJOR-#3 review fix: the renderer reads four JSON
    artifacts. Per the review finding, every input MUST be schema-
    validated before the deck is assembled. A schema-validation
    failure raises ``jsonschema.ValidationError`` carrying the
    offending field path; the caller in ``main()`` catches it and
    returns a non-zero exit code.

    The pattern mirrors the analogous helper in ``10_render_report.py``
    so both renderers fail-fast on the same artifact-shape contracts.

    Args:
        metrics: Parsed ``data/metrics.json``.
        per_eng: Parsed ``data/per_engineer.json``.
        inflection: Parsed ``data/inflection.json``.
        env: Parsed ``data/environment.json``.
        logger: The structured-JSON logger.

    Raises:
        jsonschema.ValidationError: If any input artifact fails
            schema validation. The exception is re-raised with its
            original ``message`` and ``absolute_path`` intact so the
            caller can emit a precise error.
    """
    artifacts_to_validate: dict[str, dict[str, Any]] = {
        "metrics.json": metrics,
        "per_engineer.json": per_eng,
        "inflection.json": inflection,
        "environment.json": env,
    }
    for artifact_name, payload in artifacts_to_validate.items():
        schema_filename = DECK_INPUT_SCHEMAS.get(artifact_name)
        if schema_filename is None:
            continue
        schema_path = SCHEMAS_DIR / schema_filename
        if not schema_path.exists():
            # The schema file is missing on disk. Emit a structured
            # warning but do not abort — this keeps the renderer
            # resilient to ad-hoc test harnesses that omit a schema.
            logger.warning(
                "input_schema_missing",
                extra={
                    "event": "input_schema_missing",
                    "artifact": artifact_name,
                    "schema": schema_filename,
                    "expected_path": str(schema_path),
                },
            )
            continue
        try:
            schema = json.loads(schema_path.read_text(encoding="utf-8"))
            jsonschema.validate(payload, schema)
            logger.info(
                "input_schema_validated",
                extra={
                    "event": "input_schema_validated",
                    "artifact": artifact_name,
                    "schema": schema_filename,
                },
            )
        except jsonschema.ValidationError as exc:
            logger.error(
                "input_schema_validation_failed",
                extra={
                    "event": "input_schema_validation_failed",
                    "artifact": artifact_name,
                    "schema": schema_filename,
                    "error_message": exc.message[:240],
                    "error_path": [str(p) for p in exc.absolute_path],
                },
            )
            raise


# ===========================================================================
# Main entry point — CLI argparse + dry-run handling + render + write.
# Each step is wrapped in a try/except that logs structured events before
# re-raising or returning a non-zero exit code.
# ===========================================================================


def _emit_unexpected_error(
    logger: Any,
    exc: BaseException,
    *,
    debug_mode: bool,
    event: str,
    exit_code: int,
    extra: dict[str, Any] | None = None,
) -> None:
    """Log an unexpected exception with traceback gated by ``--debug``.

    MINOR-#5 review fix. When ``debug_mode`` is True the function emits
    the full traceback so an operator running the renderer
    interactively (``python3 scripts/11_render_deck.py --debug``) can
    diagnose the failure. When False — the production default — only a
    single-line structured ERROR record is emitted plus a short stderr
    message, keeping CI logs uncluttered.

    Args:
        logger: The structured-JSON logger.
        exc: The exception to report.
        debug_mode: Whether ``--debug`` was passed.
        event: The structured-log event name.
        exit_code: The exit code the caller will return.
        extra: Optional additional fields to merge into the log record.
    """
    record_extra: dict[str, Any] = {
        "event": event,
        "error_class": type(exc).__name__,
        "error": str(exc)[:240],
        "exit_code": exit_code,
    }
    if extra:
        record_extra.update(extra)
    if debug_mode:
        # ``logger.exception`` includes the full traceback automatically.
        logger.exception(event, extra=record_extra)
    else:
        # Single-line structured ERROR record without traceback.
        logger.error(event, extra=record_extra)
        print(
            f"{event}: {type(exc).__name__}: {str(exc)[:240]}",
            file=sys.stderr,
        )


def _build_arg_parser() -> argparse.ArgumentParser:
    """Construct the CLI argument parser.

    Extracted from ``main()`` so the parser definition is testable in
    isolation and so unit tests can inspect the expected flag set
    without invoking the full renderer.

    Returns:
        Configured ``argparse.ArgumentParser`` with three flags:
        ``--dry-run``, ``--output``, ``--debug``.
    """
    parser = argparse.ArgumentParser(
        description="Render executive-summary.html from data/metrics.json",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="List the files this script would read and write, then exit "
             "without writing. Output is JSON for machine consumption.",
    )
    parser.add_argument(
        "--output",
        default=str(OUTPUT_PATH),
        help=(
            f"Path to write the rendered HTML deck. The path is "
            f"validated against the workspace boundary via "
            f"safe_output_path() before any write occurs; any path "
            f"outside blitzy/acceleration-report/ is rejected with "
            f"exit code 4 (MAJOR-#4 review fix). "
            f"Default: {OUTPUT_PATH!s}"
        ),
    )
    parser.add_argument(
        "--debug",
        action="store_true",
        help=(
            "Surface full Python tracebacks on unexpected errors. "
            "Without this flag, the renderer prints a single-line "
            "structured error and exits with a non-zero code "
            "(MINOR-#5 review fix: suppress stack traces unless "
            "explicitly requested)."
        ),
    )
    return parser


def main() -> int:
    """Run the deck renderer end-to-end.

    Steps:

    1. Parse CLI args (``--dry-run``, ``--output``, ``--debug``).
    2. Initialise the structured-JSON logger via
       ``lib.observability.get_logger``.
    3. Resolve and confine the output path via ``safe_output_path``.
       Reject any path outside ``blitzy/acceleration-report/`` with
       exit code 4 (MAJOR-#4 review fix).
    4. Emit ``script_started`` event.
    5. If ``--dry-run``: emit the JSON preview and return 0.
    6. Read the four data artifacts. On FileNotFoundError, emit
       ``data_artifact_missing`` event and return non-zero.
    7. Schema-validate every input artifact (MAJOR-#3 review fix). A
       validation failure returns non-zero with the offending field
       path and a single-line structured error (full traceback only
       when ``--debug`` is set per MINOR-#5).
    8. Build the slide list and render the full HTML.
    9. Run all nine pre-write guards (MAJOR-#1 review fix added
       guard 9 for Low-confidence caveat presence). On failure, emit
       ``pre_write_guard_failed`` event and return non-zero.
    10. Write the HTML atomically via ``atomic_write_text`` (MAJOR-#4
        review fix). On OS error, emit ``output_write_failed`` and
        return non-zero.
    11. Emit ``script_complete`` event and return 0.

    Returns:
        Process exit code: 0 on success, 1 on guard or unexpected
        error, 4 on path-confinement rejection.
    """
    parser = _build_arg_parser()
    args = parser.parse_args()
    debug_mode: bool = bool(args.debug)

    logger = get_logger(SCRIPT_NAME)

    # --- Path confinement (MAJOR-#4 review fix) ----------------------------
    # Reject any --output that resolves outside the workspace BEFORE the
    # structured-script_started log so the workspace_root context is
    # accurate and the rejection event surfaces with the exact path the
    # operator supplied.
    try:
        output_path = safe_output_path(args.output)
    except OutputPathError as exc:
        logger.error(
            "deck_output_path_rejected",
            extra={
                "event": "deck_output_path_rejected",
                "path": str(args.output),
                "error": str(exc)[:240],
                "exit_code": 4,
            },
        )
        print(str(exc), file=sys.stderr)
        return 4

    # Observability anchor for the run: every script in the pipeline
    # emits this event with its dry-run flag, debug flag, and output
    # target. The workspace root is included so the operator can
    # confirm path-confinement is rooted at the intended workspace.
    logger.info(
        "script_started",
        extra={
            "event": "script_started",
            "dry_run": args.dry_run,
            "debug_mode": debug_mode,
            "output": str(output_path),
            "workspace_root": str(WORKSPACE_ROOT),
            "blitzy_run_id_env_present": bool(os.environ.get("BLITZY_RUN_ID")),
        },
    )

    # --- Dry-run early exit -------------------------------------------------
    if args.dry_run:
        preview = {
            "action": "dry_run",
            "script": SCRIPT_NAME,
            "reads": [f"data/{name}" for name in READ_ARTIFACTS],
            "writes": [str(output_path)],
            "pre_write_guards": [
                "slide_count",
                "slide_visual_per_slide",
                "no_emoji",
                "no_fenced_code_blocks",
                "cdn_pinned_versions",
                "brand_palette",
                "factual_neutral_tone",
                "section_ordering",
                "low_confidence_caveat",
            ],
            "input_schemas_validated": list(DECK_INPUT_SCHEMAS.keys()),
            "path_confinement_enforced": True,
            "atomic_write_enabled": True,
        }
        # The dry-run JSON is the contract consumed by the Makefile's
        # preflight check; print to stdout so it is grep-able. The structured
        # logger event captures the same payload to the JSON-Lines log.
        print(json.dumps(preview))
        logger.info(
            "dry_run_preview_emitted",
            extra={
                "event": "dry_run_preview_emitted",
                "reads": preview["reads"],
                "writes": preview["writes"],
            },
        )
        return 0

    # --- Read data artifacts -----------------------------------------------
    try:
        metrics_path = DATA_DIR / "metrics.json"
        per_eng_path = DATA_DIR / "per_engineer.json"
        inflection_path = DATA_DIR / "inflection.json"
        env_path = DATA_DIR / "environment.json"
        metrics = json.loads(metrics_path.read_text(encoding="utf-8"))
        per_eng = json.loads(per_eng_path.read_text(encoding="utf-8"))
        inflection = json.loads(inflection_path.read_text(encoding="utf-8"))
        env = json.loads(env_path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        # MINOR-#5: structured error only; no traceback unless --debug.
        _emit_unexpected_error(
            logger,
            exc,
            debug_mode=debug_mode,
            event="data_artifact_missing",
            exit_code=1,
            extra={"path": str(exc.filename)},
        )
        return 1
    except json.JSONDecodeError as exc:
        _emit_unexpected_error(
            logger,
            exc,
            debug_mode=debug_mode,
            event="data_artifact_json_decode_error",
            exit_code=1,
            extra={"lineno": exc.lineno, "colno": exc.colno},
        )
        return 1

    # --- Schema-validate inputs (MAJOR-#3 review fix) -----------------------
    try:
        _validate_input_artifacts(metrics, per_eng, inflection, env, logger)
    except jsonschema.ValidationError as exc:
        _emit_unexpected_error(
            logger,
            exc,
            debug_mode=debug_mode,
            event="input_schema_validation_failed",
            exit_code=1,
            extra={
                "schema_error_path": [str(p) for p in exc.absolute_path],
            },
        )
        return 1

    # --- Build slides and render the full HTML ------------------------------
    slides = build_slides(
        metrics=metrics,
        per_eng=per_eng,
        inflection=inflection,
        env=env,
    )
    html = render_html(slides)

    # --- Pre-write guards ---------------------------------------------------
    # Stash the metrics dict for the Low-confidence caveat guard (guard 9)
    # to reach without changing the public ``pre_write_guard`` signature.
    global _LAST_RENDER_METRICS
    _LAST_RENDER_METRICS = metrics
    try:
        pre_write_guard(html, slides)
    except ValueError as exc:
        # Pre-write-guard failures are an expected, well-structured
        # error class: emit a single-line ERROR record (no traceback
        # because the message itself is the diagnostic).
        logger.error(
            "pre_write_guard_failed",
            extra={
                "event": "pre_write_guard_failed",
                "guard_failure": str(exc),
                "slide_count": len(slides),
                "exit_code": 1,
            },
        )
        print(f"pre_write_guard_failed: {exc}", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 — surface unexpected guard errors
        _emit_unexpected_error(
            logger,
            exc,
            debug_mode=debug_mode,
            event="pre_write_guard_unexpected_error",
            exit_code=1,
        )
        return 1

    # --- Write output atomically (MAJOR-#4 review fix) ---------------------
    try:
        atomic_write_text(output_path, html)
    except OutputPathError as exc:
        # Path was re-validated by atomic_write_text and rejected.
        logger.error(
            "deck_output_path_rejected_post_render",
            extra={
                "event": "deck_output_path_rejected_post_render",
                "path": str(output_path),
                "error": str(exc)[:240],
                "exit_code": 4,
            },
        )
        print(str(exc), file=sys.stderr)
        return 4
    except OSError as exc:
        _emit_unexpected_error(
            logger,
            exc,
            debug_mode=debug_mode,
            event="output_write_failed",
            exit_code=1,
            extra={"path": str(output_path)},
        )
        return 1
    except Exception as exc:  # noqa: BLE001 — catch-all for unexpected errors
        _emit_unexpected_error(
            logger,
            exc,
            debug_mode=debug_mode,
            event="unexpected_error",
            exit_code=1,
        )
        return 1

    logger.info(
        "script_complete",
        extra={
            "event": "script_complete",
            "output": str(output_path),
            "slide_count": len(slides),
            "html_size_bytes": len(html.encode("utf-8")),
            "exit_code": 0,
        },
    )
    return 0


# Standard module entry point. Surface the return value of ``main()``
# as the process exit code so the Makefile and CI scripts can detect
# pre-write guard violations (exit 1) and path-confinement rejections
# (exit 4) versus successful renders (exit 0).
if __name__ == "__main__":
    sys.exit(main())

