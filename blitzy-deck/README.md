# Config H Executive Deck — Operator Note

This README accompanies `blitzy-deck/index.html`, a self-contained reveal.js 5.1.0 presentation
summarizing the Config H Snyk scan of `blitzy-RudderStack` for non-technical leadership. The deck
pulls reveal.js, Mermaid, and Lucide from CDN; no build step is required.

## Opening the deck

The deck is a single HTML file with CDN-loaded dependencies. Opening it directly in a modern
browser is the entire workflow — there is no install command, no bundler, and no local server.

- macOS: `open blitzy-deck/index.html`
- Linux: `xdg-open blitzy-deck/index.html`
- Windows: double-click `blitzy-deck\index.html` in File Explorer, or run `start blitzy-deck\index.html`
  from the Command Prompt

## Requirements

- A modern browser: Chromium 100 or later, Firefox 100 or later, or Safari 15 or later
- Internet access on first load — the browser fetches reveal.js 5.1.0, Mermaid 11.4.0,
  Lucide 0.460.0, and Google Fonts (Inter, Space Grotesk, Fira Code) from CDN
- No local Node.js, Python, or other tooling is required to view the deck
- Subsequent loads can use the browser cache; an offline second viewing is supported once the
  CDN assets are cached

The Mermaid 11.4.0 pin is mandated by AAP §0.8.1 Rule 2 (Executive Presentation) and is retained
under a formal security exception recorded in `DECISIONS.md` (Decision 15). All Mermaid diagram
sources in this deck are hardcoded by the deck author — no user-controlled diagram source ever
reaches Mermaid at runtime — so the deck's threat model does not exercise the public-CDN
Mermaid vulnerability surface for this release. See `DECISIONS.md` for the full risk analysis
and mitigations.

## Viewing Dimensions

- The deck is designed for **1920x1080** (Full HD landscape) per AAP §0.8.1 Rule 2
  (reveal.js is initialized with `width: 1920` and `height: 1080`)
- Reveal.js scales the slides to fit the browser window automatically — viewing on smaller
  screens is supported and the 1920x1080 aspect ratio is preserved with letterboxing where
  needed
- For presentation use, switch the browser into full-screen mode: `F11` on Windows and Linux,
  or `Ctrl+Cmd+F` on macOS

## Navigation

Reveal.js keyboard controls:

- `Space` or `Right arrow` — next slide
- `Left arrow` — previous slide
- `Esc` — toggle the slide overview (grid view of all slides)
- `S` — open the speaker view in a separate window (no speaker notes are configured in this
  deck, so the speaker pane will be empty)
- `?` — show all keybindings

The URL hash updates to `#/<slide-index>` as the operator navigates, so direct links to any
individual slide are shareable.

## Slide-ordering convention

The deck uses four slide types per AAP §0.8.1 Rule 2:

- **Title** (`slide-title`, 1 slide): hero gradient background, project name, and the brand
  Lucide icon
- **Content** (default, 10 slides): white background with accent-bar headings, KPI cards,
  styled tables, Mermaid diagrams, and Lucide icons
- **Section dividers** (`slide-divider`, 4 slides): dark gradient background with a large
  Lucide icon and the section title
- **Closing** (`slide-closing`, 1 slide): gradient background, three action bullets, and the
  brand lockup

Total: 16 slides, the mid-range of the 12 to 18 envelope per AAP §0.4.8.

## Contents

The deck is structured as 16 sequential slides. The list below mirrors the rendered `<h1>` / `<h2>` text in `index.html` byte-for-byte.

1. Config H (Title)
2. Why a multi-config security comparison? (Content)
3. Architecture overview (Content)
4. Scope (Divider)
5. What was scanned (Content)
6. What was NOT scanned (boundaries) (Content)
7. Methodology (Divider)
8. Snyk Code (SAST) (Content)
9. Snyk Open Source (deps) (Content)
10. Normalization & schema (Content)
11. Results (Divider)
12. Findings summary by severity (Content)
13. Notable patterns / hotspots (Content)
14. Risk & onboarding (Divider)
15. Risks & mitigations (Content)
16. Take action (Closing)

## Relationship to other deliverables

- This deck **summarizes** but does not duplicate `DECISIONS.md` — detailed rationale for every
  non-trivial implementation decision stays in the decision log at the repository root
- This deck **does not include** raw findings — those live in `findings-config-h.json` at the
  repository root as a minified single-line JSON array
- The normalizer script `scripts/normalize-snyk-findings.py` is referenced only as an inline
  command example on the closing slide; the implementation itself is not reproduced in the
  deck

## Editing the deck

- The deck is a single self-contained HTML file. Edit `blitzy-deck/index.html` directly in any
  text editor — no build step, no preprocessor
- Adding or removing slides changes the `<section>` count. Stay within the 12 to 18 envelope
  (target 16) per AAP §0.8.1 Rule 2; update the `## Contents` list in this file in lockstep
- All styling lives in the inline `<style>` block at the top of `blitzy-deck/index.html`;
  there is no external CSS file
- Mermaid diagrams are embedded as `<div class="mermaid">...</div>` blocks; the Mermaid 11.4.0
  runtime renders them on page load and on every `slidechanged` event
- Lucide icons are `<i data-lucide="icon-name"></i>` elements; the Lucide 0.460.0 runtime
  replaces them with inline SVG on page load and on every `slidechanged` event

## Out of scope for this folder

The `blitzy-deck/` folder intentionally contains only `index.html` and this `README.md`. The
following are not present and are not required:

- No `package.json` — the deck is build-step-free
- No `node_modules/` — all runtime dependencies load from CDN
- No `assets/` subfolder for images — Lucide SVGs render at runtime from the icon library
- No `styles.css` — the theme CSS is inlined inside `index.html`
- No alternate-format exports — no PDF, no Keynote, no PowerPoint conversions are committed
- No speaker notes — the deck is designed for live narration, not self-paced reading
