# Decision Log — Gosec Security Scan · Config D · blitzy-RudderStack

This document captures every non-trivial implementation decision made while
executing Config D of the multi-config security tool comparison study against
the `github.com/rudderlabs/rudder-server` repository. It is mandated by the
**Explainability rule** (Agent Action Plan §0.7.1), which requires that *every
non-trivial implementation decision MUST be documented with rationale* and that
*the decision log is the single source of truth for "why" decisions* — i.e.
rationale is intentionally **NOT** embedded as comments in
`scripts/normalize-findings.py` or any other source file.

A decision is treated as non-trivial whenever a competent engineer could
reasonably have chosen differently. The seventeen rows below enumerate every
such decision encountered in this task.

## Run Metadata

- **User directive identifier:** `Config D — Gosec | blitzy-RudderStack`
- **User-stated scope budget:** `[3 directives | ~0 files modified | 1 new file]`
- **Repository under scan:** `github.com/rudderlabs/rudder-server`
- **Branch:** `blitzy-acbce301-6272-4059-8e0e-27d625fdc58d`
- **Date / run-id:** `2026-03-19` *(placeholder — replace with the actual UTC
  timestamp at run time, e.g. via `date -u +%Y-%m-%dT%H:%M:%SZ`)*
- **Gosec resolved version:** `v2.26.1 (resolved via @latest at supporting setup time)`
  *(placeholder — replace with the exact string emitted by `gosec --version`
  at install time; note that `go install …@latest` typically reports
  `Version: dev` because release tags are not injected by the module loader,
  in which case record the underlying module pseudo-version from
  `go list -m -json github.com/securego/gosec/v2`. The embedded
  rule-ID→CWE fallback table in `scripts/normalize-findings.py` is mirrored
  byte-for-byte from v2.26.1's `issue/issue.go` `ruleToCWE` map — see Row 4
  and Row 16.)*
- **Go toolchain version:** `1.26.1` *(matches the repository's `go.mod` `go`
  directive; the host MAY have a newer compatible Go — e.g. `1.26.3` — provided
  it is `≥ 1.26.1` and `≥ 1.25.0` per Gosec's own prerequisite)*
- **Scan command (verbatim):** `gosec -fmt=sarif -out=results-gosec.sarif ./...`
- **Final pass/fail gate (verbatim):** `cat findings-config-d.json | wc -l`
  returns `1`. Valid JSON. Every finding has all 5 fields populated. No
  description exceeds 200 characters.

## CRITICAL Directives (Reproduced Verbatim)

The three CRITICAL directives below are reproduced byte-for-byte from the
user's input. They are the authoritative source for Config D and are never
paraphrased anywhere in this log, in the executive summary deck, or in the
implementation. The bracketed labels (Directive 1, 2, 3) are added here only
as section anchors and are not part of the verbatim payload.

### Directive 1 — Install Gosec

> **CRITICAL Directive 1: Install Gosec**
>
> `go install github.com/securego/gosec/v2/cmd/gosec@latest`
>
> Requires Go installed. Ships with all rules built in, no additional downloads.
>
> **Pass/fail:** `gosec --version` returns a version string.

### Directive 2 — Execute Gosec scan

> **CRITICAL Directive 2: Execute Gosec scan**
>
> Run from the repo root:
>
> `gosec -fmt=sarif -out=results-gosec.sarif ./...`
>
> Record exit code, scan duration (wall-clock), and total files scanned.
>
> **Pass/fail:** `results-gosec.sarif` is produced and contains valid JSON.

### Directive 3 — Normalize findings to single-line JSON

> **CRITICAL Directive 3: Normalize findings to single-line JSON**
>
> Extract findings from the SARIF output and compile into `findings-config-d.json`.
> The file MUST be valid JSON minified to a single line. Encoding: UTF-8.
> If zero findings, write `[]`.

### Final Pass/Fail Gate (Verbatim)

> `cat findings-config-d.json | wc -l` returns `1`. Valid JSON. Every finding
> has all 5 fields populated. No description exceeds 200 characters.

## Field Mapping (Reproduced Verbatim)

The five-field schema below is reproduced byte-for-byte from the user's input.
It is the contract that `scripts/normalize-findings.py` enforces on every
emitted object in `findings-config-d.json`.

| Field | Source |
| --- | --- |
| file | SARIF location (relative path) |
| line | SARIF region start line |
| severity | SARIF level: error→critical, warning→high, note→medium, info→low |
| cwe | Rule metadata CWE ID. If absent, map from Gosec rule ID (e.g. G101→CWE-798, G201→CWE-89) |
| description | SARIF message text, truncated to 200 characters |

## Output Shape (Reproduced Verbatim)

The output shape below is reproduced byte-for-byte from the user's input.
Each emitted object in `findings-config-d.json` conforms to this layout;
the file is a single-line minified array of zero or more such objects.

```plaintext
[{"file":"<relative path>","line":<integer>,"severity":"<critical|high|medium|low>","cwe":"<CWE-ID>","description":"<max 200 chars>"},...]
```

## Severity Translation Table (Full)

The contractual translation from SARIF `level` to the closed output severity
vocabulary `{critical, high, medium, low}`. Row order matches the user's
field-mapping table; the additional rows for absent / `none` SARIF levels
document the implementation's handling of SARIF results that are not findings.

| SARIF `level` | Output `severity` | Notes |
| --- | --- | --- |
| `error` | `critical` | Verbatim from user's field-mapping table. |
| `warning` | `high` | Verbatim from user's field-mapping table. |
| `note` | `medium` | Verbatim from user's field-mapping table. |
| `info` | `low` | Verbatim from user's field-mapping table. Gosec rarely emits this level; preserved for tool-agnostic safety. |
| absent / `none` | result skipped | Not in the user's translation table; treated as `kind != "fail"` and excluded from findings. |

## Decisions

The table uses the column headers mandated by the Explainability rule —
`Decision | Alternatives | Why | Risks` — with one row per non-trivial
decision. Cells are single-line; `<br>` is used to render visual line breaks
inside cells without introducing literal newlines into the Markdown source.

| Decision | Alternatives | Why | Risks |
| --- | --- | --- | --- |
| **Install source for Gosec.** Install via `go install github.com/securego/gosec/v2/cmd/gosec@latest` on the executor host. | (a) Homebrew: `brew install gosec`.<br>(b) Docker image: `docker pull securego/gosec:latest`.<br>(c) Pinned tag: `go install github.com/securego/gosec/v2/cmd/gosec@v2.26.1`.<br>(d) Build from source: `git clone … && make build`. | The user's **CRITICAL Directive 1** explicitly names `go install …@latest` verbatim. Deviating from this exact command would violate the AAP's verbatim-preservation rule and break the multi-config comparison contract (every config exercises its tool's own canonical install path). | `@latest` is unpinned — a future Gosec release could change rule output, rule IDs, SARIF formatting, or the canonical `ruleToCWE` map (cf. Row 16) between runs. **Mitigation:** the resolved version string from `gosec --version` (or the underlying module pseudo-version) is recorded in this log's **Operational Telemetry** section and rendered as a KPI card on `executive-summary.html` so the exact build is forensically reproducible; the embedded CWE fallback table in the normalization script is dated to match the resolved Gosec line (currently v2.26.1). |
| **Normalization language and runtime.** Implement `scripts/normalize-findings.py` in Python 3 using only the standard library (`json`, `argparse`, `os`, `re`, `sys`, `pathlib`, plus `urllib.parse` for URI parsing). | (a) `jq` one-liner — fast but cannot cleanly enforce the post-condition self-checks (exact 5-field set, closed severity vocabulary, 200-char ceiling, `CWE-<n>` format).<br>(b) Go program — would add a build step and either introduce a Go-module dependency or require a separate `go.mod` for the script.<br>(c) Bash + `python -c` heredoc — opaque, unmaintainable, hard to test. | Python is pre-installed on essentially every modern CI runner; `json.dumps(..., separators=(",", ":"), ensure_ascii=False)` produces the contract-compliant minified UTF-8 representation in one call; assertions inside the script support fail-fast on post-condition violation; zero third-party dependencies means `pip install` is never required. | Python `≥ 3.8` is assumed (for `:=` walrus is not used and only stdlib features stable since 3.8 are required); if the runner has only Python 2 or a very old Python 3 the script may not run. **Mitigation:** the shebang `#!/usr/bin/env python3` resolves to the host's Python 3 toolchain; using only mature stdlib features keeps the floor low; the post-condition self-check raises a clear `AssertionError` and exits non-zero on any contract violation regardless of Python minor version. The host in the current environment has Python 3.13.7, well above the floor. |
| **SARIF URI normalization strategy.** Strip any `file://` scheme prefix, resolve relative URIs against `runs[].originalUriBaseIds[].uri` when present, then compute `os.path.relpath` against the captured repository root (defaulting to the script's CWD if `--repo-root` is not supplied). | (a) Use the raw `artifactLocation.uri` value unchanged — leaves `file://` prefix or absolute paths in output, breaking the "relative path" contract.<br>(b) Hard-code `os.getcwd()` — fragile if the script is invoked from a directory other than the repo root.<br>(c) String-replace a hard-coded `/work` or `/app` prefix — non-portable across runners. | The user's field-mapping table mandates `file: SARIF location (relative path)`. Producing **repo-relative POSIX paths** (e.g. `gateway/handler.go`, never `file:///home/runner/work/blitzy-RudderStack/gateway/handler.go`) satisfies that contract and matches what every downstream comparison consumer expects. | Symlinked paths or absolute paths outside the repo root yield relative paths containing `..` segments. **Mitigation:** rare for Go modules under `./...`; the output still parses as JSON and the path is still a valid relative reference. |
| **CWE resolution order.** Four-step resolution, evaluated in order until one succeeds. (1) Look up the rule in `runs[].tool.driver.rules[]` by `ruleId` and read `properties.cwe` (accepting either a string `"<n>"`/`"CWE-<n>"` form or a dict with an `id` key, mirroring SARIF property-bag conventions). (2) Scan `rule.relationships[]` for an entry whose `target.toolComponent.name` contains the substring `"CWE"` and use its `target.id` — this is the path Gosec v2.x's `report/sarif` package populates, with `Kinds: ["superset"]`. (3) Scan `properties.tags` for any tag matching the regex `cwe[-/](\d+)` (case-insensitive) and use the captured digits. (4) If all three metadata paths miss, fall back to the embedded `GOSEC_RULE_TO_CWE` table seeded from Gosec v2.26.1's canonical `ruleToCWE` map (61 entries: G101–G124, G201–G204, G301–G307, G401–G408, G501–G507, G601–G602, G701–G710 — see Row 16 for the refresh history). (5) If everything fails, emit `CWE-Unknown` and log the unmapped `ruleId` to stderr. | (a) Rely solely on rule metadata — Gosec's SARIF generator does not populate CWE under every schema variant.<br>(b) Rely solely on the static fallback table — leaves no path for future Gosec rules and would ignore CWE-correctness signals already embedded in SARIF.<br>(c) Omit the `cwe` field when unresolved — violates the contract that all five fields are populated.<br>(d) Inspect `properties["security-severity"]` — this property carries a numeric CVSS-style score, not a CWE identifier, so it would produce wrong values; it is intentionally **not** consulted. | The user's field-mapping table specifies *"Rule metadata CWE ID. If absent, map from Gosec rule ID (e.g. G101→CWE-798, G201→CWE-89)."* The four-step strategy is the literal implementation of that directive, ordered from most specific (`properties.cwe`) to most general (`relationships`, `tags`), with the static table as the safety net; `CWE-Unknown` as a final fallback guarantees the five-field schema is never violated even when an unknown future rule fires. | Future Gosec rules not yet in the embedded table will be tagged `CWE-Unknown` until the table is refreshed. **Mitigation:** the script logs the unmapped `ruleId` values to stderr so the gap is visible in run logs; the embedded table is regenerated each time the resolved `@latest` Gosec version advances (cf. Row 1 and Row 16). |
| **Missing `region.startLine` handling.** When `locations[0].physicalLocation.region.startLine` is missing or non-integer, emit `line: 0` and continue. | (a) Drop the result entirely — loses information about a real finding.<br>(b) Emit `line: null` — violates the contract that `line` is an integer.<br>(c) Emit `line: -1` — non-conventional sentinel that downstream consumers might misinterpret. | `0` is a conventional "unknown / not applicable" sentinel for line numbers (real source lines are 1-indexed). It preserves the finding, keeps `line` typed as `int`, and is trivially filterable by consumers that want only located findings (`line > 0`). | A downstream consumer that filters strictly on `line > 0` will drop these entries. This is acceptable behavior for partially-located findings and parallels how other SAST consumers treat unlocated results. |
| **Description sanitization and truncation strategy.** Collapse all internal whitespace (newlines, tabs, multiple spaces) to single spaces, strip leading/trailing whitespace, then slice to the first 200 Unicode code points. No ellipsis is appended. | (a) Truncate to 200 bytes — produces invalid UTF-8 at multi-byte boundaries.<br>(b) Append `"..."` or `"…"` to truncated descriptions — adds 1–3 extra characters and the directive's exact phrasing ("truncated to 200 characters") specifies no ellipsis.<br>(c) Preserve original whitespace — long messages span multiple visual lines in JSON viewers and frustrate diff tooling across runs and configs. | Whitespace normalization makes diffing across runs deterministic; **code-point** slicing satisfies the contract gate that *no description exceeds 200 characters* without risking UTF-8 corruption. | Truncation at 200 code points can occur mid-word or mid-sentence. Acceptable for a comparison artifact where the truncated tail is rarely the discriminating signal — the `file`, `line`, `cwe`, and `ruleId` (encoded in `description` prefix in Gosec's message format) provide the discriminating information. |
| **Gosec flag tuning.** Pass **no flags** beyond `-fmt=sarif -out=results-gosec.sarif`. Do not pass `-tests`, `-exclude-generated`, `-exclude`, `-exclude-dir`, `-include`, `-track-suppressions`, `-severity`, `-confidence`, `-conf`, `-tags`, or `-no-fail`. | Each of the omitted flags could be set to narrow the scan, raise/lower the severity gate, exclude generated code, surface suppressed findings, or alter file-set selection. | The **multi-config comparison contract** requires every config to exercise its tool's defaults so that the cross-tool differential is attributable to the tool's behavior, not to flag tuning. The user's CRITICAL Directive 2 is explicit and literal: `gosec -fmt=sarif -out=results-gosec.sarif ./...` — no other flags. | Default behavior excludes `_test.go` files at the Gosec layer and includes generated code; this introduces some noise that other configs may not exhibit identically. The noise is consistent across runs of this config and is documented here as a known property. |
| **Empty-set sentinel.** When `runs[].results[]` is empty across every run, write the literal three-byte payload `[]\n` to `findings-config-d.json` — the two-byte JSON array `[]` followed by exactly one POSIX line-terminator newline byte. | (a) Skip emission — breaks the comparison pipeline because `findings-config-d.json` is then absent and the downstream aggregator cannot locate it.<br>(b) Emit `null` or `{}` — violates the JSON-array contract; the schema is `[{...}, ...]`, never an object or null.<br>(c) Emit a single object with placeholder fields — pollutes the dataset with synthetic findings.<br>(d) Emit `[]` with no trailing newline (two bytes) — produces `wc -l == 0`, failing the user's literal final pass/fail gate. | The user's CRITICAL Directive 3 is literal: *"If zero findings, write `[]`."* The user's **final pass/fail gate** is equally literal: *"`cat findings-config-d.json \| wc -l` returns `1`."* POSIX `wc -l` counts newline terminators, so the only output that simultaneously honors the `[]` payload requirement, the single-line minification requirement, and the literal `wc -l == 1` gate is the three-byte sequence `[]\n` (the JSON payload `[]` with exactly one terminating newline byte and zero embedded newlines). The post-condition self-check (see Row 11) enforces both invariants atomically — exactly one trailing newline, zero embedded newlines — so the contract is byte-deterministic. | None substantive — POSIX file conventions treat a trailing newline as the standard line terminator, so the artifact is well-formed for every downstream POSIX tool. The hex dump of the empty-case file is `5b 5d 0a`, and the populated case follows the same `<JSON>\n` invariant (cf. Row 10). |
| **Field order in output objects.** Emit each finding object with key order `file, line, severity, cwe, description`, matching the user's field-mapping table. | (a) Sort keys alphabetically (`cwe, description, file, line, severity`) — breaks parity with the directive's documented order.<br>(b) Allow Python dict insertion order to vary by accident — produces non-deterministic output across runs. | Stable, directive-aligned key order makes the output **diffable across runs and across configs** in the multi-config comparison study. The user's table is the natural canonical order; downstream tooling that pretty-prints for visual inspection sees the same columnar ordering across every config. | None — JSON consumers are key-name-driven, not order-driven; the determinism is purely a developer-ergonomics property. |
| **JSON minification settings.** Call `json.dumps(items, separators=(",", ":"), ensure_ascii=False)` and write the result with `f.write(payload.encode("utf-8") + b"\n")` — minified JSON payload plus exactly one terminating newline byte. | (a) Default `json.dumps` separators (`", ", ": "`) — adds whitespace between every token, violating the single-line contract.<br>(b) `ensure_ascii=True` — bloats any non-ASCII description into `\uXXXX` escapes; still valid JSON but a larger artifact and harder to grep.<br>(c) `f.write(payload.encode("utf-8"))` with **no trailing newline** — leaves the artifact with zero `\n` bytes, producing `wc -l == 0` and failing the user's literal final pass/fail gate.<br>(d) Append `\r\n` instead of `\n` — non-POSIX and produces unpredictable `wc -l` behavior across runners. | These are the canonical settings for the most compact UTF-8 JSON representation. The single `\n` terminator makes the output a well-formed POSIX text line: the artifact contains **zero embedded newlines** and **exactly one trailing newline**, so `cat findings-config-d.json \| wc -l` reports `1` in both the empty (`[]\n`) and populated (`[{...}, ...]\n`) cases. This satisfies the user's literal final pass/fail gate and matches POSIX text-file conventions. See Row 8 ("Empty-set sentinel") for the empty-case contract and Row 11 for the post-condition self-check that enforces both invariants. | None — these are the canonical settings for byte-deterministic minified JSON plus a single POSIX line terminator. Any external consumer that genuinely requires a newline-free byte stream can trivially strip the terminator (e.g., `head -c -1 findings-config-d.json`). |
| **Post-condition self-check inside the script.** After writing the output file, re-open it and assert: (a) the byte stream ends with exactly one `\n` terminator; (b) zero embedded newlines (`data[:-1].count(b"\n") == 0`); (c) `json.loads` succeeds and returns a `list`; (d) every element has exactly the key-set `{file, line, severity, cwe, description}`; (e) `line` is `int` (not `bool`); (f) `severity` ∈ `{critical, high, medium, low}`; (g) `cwe` matches `^CWE-(\d+\|Unknown)$`; (h) every `description` ≤ 200 chars. Exit non-zero on any failure. | Defer all validation to an external check, manual inspection, or rely solely on the operator running `cat findings-config-d.json \| wc -l`. | The user's final pass/fail gate is binary and objective; **fail-fast inside the script** means a downstream agent or CI step sees a clear non-zero exit and a stderr trace immediately rather than discovering a silently-corrupt JSON during cross-config aggregation. The single-trailing-newline assertion plus zero-embedded-newline assertion together guarantee `wc -l == 1`. | A bug in the self-check could theoretically let bad output through. **Mitigation:** the operator still runs the external gate `cat findings-config-d.json \| wc -l` as a final independent check (it must report `1` per Rows 8 and 10); the script's internal post-conditions and the external POSIX gate are designed to agree. |
| **No `#nosec` suppression handling.** Do not pass `-nosec` or `-track-suppressions` to Gosec. The scan honors any pre-existing `#nosec` annotations in source as the upstream repository intends. | Pass `-track-suppressions` to surface suppressed findings as a separate output stream and audit suppression hygiene. | The directive does not request suppression tracking. The **multi-config comparison contract** would be inconsistent if some configs surfaced suppressions and others did not — every config must apply the source tree's intended suppression semantics identically. | Suppressed findings are invisible in this output. Acceptable for a baseline scan; suppression auditing is an out-of-scope task that would be its own config (e.g. Config F). |
| **No modification of `.golangci.yml`.** Leave `.golangci.yml` byte-for-byte unchanged, even though it already enables `gosec` at `[L8]` as a `golangci-lint` sub-linter. | (a) Enable additional gosec rules in `.golangci.yml` to align with the standalone scan.<br>(b) Switch the linter to SARIF output via `golangci-lint run --out-format sarif`. | The user's directive specifies the **standalone `gosec` binary** (which is the only path that emits a full SARIF 2.1.0 report consumable by the cross-tool comparison). Modifying lint config would expand scope beyond `[~0 files modified]` and the standalone tool produces SARIF independently of the lint configuration. | None — the two tools coexist; the existing `golangci-lint` configuration is unaffected by either the install of the standalone `gosec` binary or the running of the scan. |
| **Rule-mandated artifacts beyond the user's "1 new file" budget.** Emit `decision-log.md` (this file) and `executive-summary.html` in addition to the user-mentioned `findings-config-d.json`, plus `scripts/normalize-findings.py` and the transient `results-gosec.sarif`. | Treat the user's `[1 new file]` budget as absolute and omit `decision-log.md` and `executive-summary.html`. | The repository's **Explainability** rule (§0.7.1) and **Executive Presentation** rule (§0.7.2) are unconditional ("MUST be documented", "MUST include an executive summary … independent of any other documentation"). Rule precedence requires emission. This is a controlled deviation from a literal directive interpretation and is explicitly documented here per the Explainability rule's own requirement that *any deviation from a literal or obvious interpretation of the requirements MUST have an explicit entry in the decision log*. | None substantive — the additional files are added at the repository root, modify no existing files, and do not affect the primary deliverable contract (the schema, encoding, and gates of `findings-config-d.json` are untouched). |
| **Traceability matrix non-inclusion.** Do not include a bidirectional traceability matrix mapping source constructs to target implementations. | Produce a matrix mapping every input SARIF result to its emitted finding object and every Gosec rule ID to its CWE assignment. | The Explainability rule states the matrix clause applies *"for migrations or refactors."* This task is neither — it is an **additive scan-and-normalize task** producing net-new artifacts with no pre-existing target implementation to map onto. A matrix would be vacuous (every input element maps to itself). | None. The non-inclusion is explicit and rationale-bearing here, so a reviewer cannot mistake the omission for an oversight. |
| **Embedded CWE fallback table refreshed to Gosec v2.26.1 canonical `ruleToCWE`.** The `GOSEC_RULE_TO_CWE` dict in `scripts/normalize-findings.py` is mirrored byte-for-byte from the **`var ruleToCWE`** map in `github.com/securego/gosec/v2@v2.26.1/issue/issue.go` — 61 entries covering `G101`–`G124`, `G201`–`G204`, `G301`–`G307`, `G401`–`G408`, `G501`–`G507`, `G601`–`G602`, and `G701`–`G710`. Two pre-existing entries were corrected against the canonical source: `G307: 703 → 276` and `G401: 326 → 328`. | (a) Keep the older 30-entry table (seeded from the historical `IssueToCWE` map plus a `cosmos/gosec` fork extension) — would emit `CWE-Unknown` for valid current Gosec rules `G111`–`G124`, `G405`–`G408`, `G506`–`G507`, `G602`, and `G701`–`G710`, and would emit objectively-wrong CWE IDs for `G307` and `G401`.<br>(b) Generate the table dynamically at run time by parsing the installed module's `issue.go` — adds a moving dependency on Gosec's internal source layout and complicates testing.<br>(c) Drop the fallback entirely and rely solely on SARIF rule metadata — see Row 4 for why this is not viable. | The user's field-mapping table is explicit: *"If absent, map from Gosec rule ID."* Mirroring the **current** canonical Gosec map (not a historical or forked variant) is the only way to map every rule the **installed** scanner can emit. Two corrections (`G307`, `G401`) also fix objectively-wrong CWE assignments that would have misclassified real security findings. | The mirror drifts when Gosec adds or renumbers rules in a future release. **Mitigation:** Row 1 records the resolved Gosec version against which the table was seeded; the script logs unmapped `ruleId` values to stderr so any drift is immediately visible; the table is regenerated in a follow-up PR when `gosec --version` advances. |
| **Mermaid CDN pin honors AAP-literal `11.4.0`.** Pin `mermaid@11.4.0` in `executive-summary.html` exactly as named verbatim by AAP §0.7.2 (Executive Presentation rule), accepting the disclosed-CVE risk surface and mitigating it via the deck's narrow, static usage pattern (see Risks column). | (a) Upgrade to `11.15.0` (the lowest stable release that patches all six disclosed advisories) — would be a security improvement but constitutes a contractual deviation from the AAP-literal pin; QA testing explicitly flagged this exact upgrade as a MAJOR-severity violation of the Executive Presentation rule.<br>(b) Pin to an interim release `11.10.0` — also a deviation; would address only the 2025 advisories.<br>(c) Use a `mermaid@^11.x` floating range — non-deterministic and forbidden by the Executive Presentation rule's "CDN versions pinned" clause.<br>(d) Remove Mermaid entirely — would force the deck to drop the two diagrams (slide 3 architecture, slide 10 contract), violating the rule's "every slide MUST include at least one non-text visual element" clause. | **AAP precedence.** AAP §0.7.2 names `mermaid@11.4.0` verbatim and AAP §0.8.1 (Special Execution Instructions) requires "Verbatim directive preservation" of every CDN pin. The multi-config security comparison contract is built on every config exercising the AAP-literal toolchain; substituting a different version — even a security-improved one — breaks parity with the contract and was flagged by QA as a MAJOR contractual deviation. The Explainability rule (AAP §0.7.1) requires documenting deviations *with rationale*, but it does not authorize an implementation to override AAP-literal pins based on engineer judgement. Documenting the decision to honor the pin (this row) is the correct application of the rule. | The `11.4.0` line has six disclosed CVEs at the time of writing: CVE-2026-41148 (`classDef` CSS injection), CVE-2026-41149 (`classDef` state-diagram HTML injection), CVE-2026-41150 (Gantt-chart DoS), CVE-2026-41159 (config CSS injection via `fontFamily`/`themeCSS`/`altFontFamily`), CVE-2025-54880 (architecture-diagram `iconText` XSS), and CVE-2025-54881 (sequence-diagram label XSS). **Exposure surface mitigation:** the deck's two Mermaid diagrams use only the `flowchart TD` syntax with static, trusted node/edge labels authored at build time; **no** `classDef` directives, **no** Gantt chart, **no** state diagram, **no** architecture diagram, **no** sequence diagram, and **no** user-supplied input ever reach the Mermaid renderer. The Blitzy theme variables passed to `mermaid.initialize()` (`primaryColor`, `primaryTextColor`, `primaryBorderColor`, `lineColor`, `secondaryColor`, `fontFamily`, `fontSize`) are static string literals, not user-controlled. The deck is served as a static offline artifact for executive viewing, not as a multi-tenant or interactive web application. The attack vectors required to exploit each CVE are therefore not reachable by the deck's content. **Future revision path:** a future AAP §0.7.2 amendment can advance the pin to a patched release (e.g. `11.15.0`); when that happens, this row is updated and the single-file CDN URL on `executive-summary.html` line 986 is bumped accordingly. |

## Deviations from a Literal Directive Interpretation

- The user's compact scope budget states `[3 directives | ~0 files modified | 1 new file]`.
- The implementation emits **five** new top-level files (plus one transient SARIF that is named by Directive 2 itself):
  1. `findings-config-d.json` — the user-described primary deliverable, single-line minified UTF-8 JSON array.
  2. `results-gosec.sarif` — the intermediate SARIF 2.1.0 report produced by Directive 2 itself; explicitly named in the user's command line and therefore expected.
  3. `scripts/normalize-findings.py` — a transformation helper that the user's three directives implicitly require. Directive 3 specifies *what* normalization to perform but does not specify the *language* or *location*; externalizing the transform to a script (rather than inlining it into shell or a one-shot command) makes the work testable, idempotent, and re-runnable.
  4. `decision-log.md` — this file. Mandated by the Explainability rule (AAP §0.7.1).
  5. `executive-summary.html` — a single self-contained reveal.js deck. Mandated by the Executive Presentation rule (AAP §0.7.2).
- **No existing file in `github.com/rudderlabs/rudder-server` is modified.** Every `.go` source, every CI workflow, `.golangci.yml`, `go.mod`, `go.sum`, `SECURITY.md`, `docs/**`, `blitzy-docs/**`, `blitzy/documentation/**`, and every other pre-existing artifact remains byte-for-byte unchanged. The `[~0 files modified]` dimension of the budget is therefore fully preserved.
- The deviation on the `[1 new file]` dimension is **not optional**: the Explainability and Executive Presentation rules are unconditional. Per the Explainability rule's own text — *"Any deviation from a literal or obvious interpretation of the requirements MUST have an explicit entry in the decision log. Unexplained deviations are treated as defects."* — this section IS the explicit entry.
- **Mermaid CDN pin honored verbatim:** The Executive Presentation rule (AAP §0.7.2) names `mermaid@11.4.0` verbatim and the implementation pins exactly `mermaid@11.4.0` in `executive-summary.html`. **There is no deviation on the Mermaid CDN pin.** Row 17 of the decision table documents the decision to honor the literal pin in spite of six disclosed CVEs in the `11.4.0` line — the deck's narrow, static `flowchart TD` usage does not exercise any of the CVE attack vectors, so the residual risk is acceptable. (An earlier draft of this artifact had pinned `11.15.0` for security and documented the upgrade as a deviation; that earlier choice was reversed at QA review per the AAP-precedence rule.)
- **Embedded CWE fallback table:** The script's `GOSEC_RULE_TO_CWE` dict was refreshed to mirror Gosec v2.26.1's canonical `ruleToCWE` map (61 entries). Two pre-existing entries were corrected (`G307: 703 → 276`, `G401: 326 → 328`) and 32 entries were added (`G111`–`G124`, `G405`–`G408`, `G506`–`G507`, `G602`, `G701`–`G710`). Row 16 of the decision table documents the source and the rationale.

## Traceability Matrix

This task is neither a migration nor a refactor — it is an additive
scan-and-normalize pass that produces net-new artifacts without altering any
existing implementation. The Explainability rule's bidirectional traceability
matrix clause is therefore **not exercised**, and no matrix is included.

## Operational Telemetry

The placeholder values below are filled in by the operator **at run time**,
immediately after executing CRITICAL Directives 1, 2, and 3 in sequence. This
`decision-log.md` is a static authored artifact and does not query
`findings-config-d.json` at render time; the operator transcribes the observed
values into the table after the scan completes. The same five values are also
rendered as KPI cards on `executive-summary.html` (the rule-mandated leadership
deck) so non-technical readers see the operational story without reading this
log.

| Metric | Value |
| --- | --- |
| Gosec resolved version | _&lt;to-fill: output of `gosec --version`&gt;_ |
| Gosec process exit code | _&lt;to-fill: `0` or `1`&gt;_ |
| Wall-clock duration (seconds) | _&lt;to-fill: as measured by `time gosec -fmt=sarif -out=results-gosec.sarif ./...`&gt;_ |
| Total files scanned | _&lt;to-fill: parsed from Gosec's `Files: N` summary line on stderr&gt;_ |
| Total findings emitted | _&lt;to-fill: `python3 -c 'import json; print(len(json.load(open("findings-config-d.json"))))'`&gt;_ |

**How to fill these values:**

1. Run `gosec --version 2>&1 | tee /tmp/gosec-version.txt`. Copy the version string into the first row. If the output is `Version: dev` (the expected output from `go install …@latest`, because release tags are not injected by the module loader), record that exact string and **additionally** record the underlying module pseudo-version from `go list -m -json github.com/securego/gosec/v2 | jq -r .Version` so the build is forensically reproducible. For example, at the time the supporting setup was performed the underlying module resolved to `gosec/v2@v2.26.1`.
2. From the repo root, run `time gosec -fmt=sarif -out=results-gosec.sarif ./... 2> /tmp/gosec-run.log; echo "exit=$?" >> /tmp/gosec-run.log`. Read the `exit=` line into row 2, the `real` time into row 3 (convert to seconds), and `grep '^Files: ' /tmp/gosec-run.log` into row 4. Per the AAP, Gosec exits `0` when no unsuppressed findings or errors occur and `1` when at least one unsuppressed finding or processing error is observed; **both `0` and `1` are valid terminal states** for Directive 2 — only the production of a valid SARIF file is the gating success criterion.
3. Run `python3 scripts/normalize-findings.py results-gosec.sarif findings-config-d.json` (Directive 3). Read row 5 using the inline `python3 -c …` snippet shown above.
4. Verify the final gate: per the user's literal directive, `cat findings-config-d.json | wc -l` MUST return `1`. Per Rows 8 and 10 of the decision table above, the implementation emits exactly one trailing `\n` byte and zero embedded newlines in both the empty (`[]\n`) and populated (`[{...}, ...]\n`) cases, so the gate is satisfied byte-deterministically. The script's internal post-condition self-check (single-trailing-newline assertion, zero-embedded-newline assertion, plus all five-field assertions) is designed to agree with the external POSIX gate: a clean exit code `0` from `scripts/normalize-findings.py` implies `wc -l == 1`. The operator should record both readings (the `wc -l` value AND the script's exit code) to confirm they agree.

## References

- **Agent Action Plan (canonical):** `blitzy/documentation/Technical Specifications.md`
- **Agent Action Plan (mirrored):** `blitzy-docs/technical-specifications.md`
- **Primary deliverable (single-line minified JSON):** `findings-config-d.json`
- **Intermediate SARIF report (Gosec output):** `results-gosec.sarif`
- **Normalization script (SARIF → contract JSON transformer):** `scripts/normalize-findings.py`
- **Leadership deck (rule-mandated):** `executive-summary.html`
- **Gosec project:** <https://github.com/securego/gosec>
- **Gosec installation & setup docs:** <https://github.com/securego/gosec#install>
- **Gosec rule-to-CWE map source (seed for the embedded fallback table):** <https://github.com/securego/gosec/blob/v2.26.1/issue/issue.go> (canonical `ruleToCWE` map at the v2.26.1 release tag — the **authoritative source**; 61 entries covering G101–G124, G201–G204, G301–G307, G401–G408, G501–G507, G601–G602, and G701–G710). Earlier historical sources (e.g. the legacy `IssueToCWE` v1 map and the `cosmos/gosec` fork's extended map) are **not** used as seeds — they predate or diverge from the current canonical map.
- **SARIF 2.1.0 specification (OASIS):** <https://docs.oasis-open.org/sarif/sarif/v2.1.0/sarif-v2.1.0.html>
- **SARIF schema URL emitted by Gosec:** <https://raw.githubusercontent.com/oasis-tcs/sarif-spec/main/sarif-2.1/schema/sarif-schema-2.1.0.json>
- **Repository linter configuration (read-only reference, NOT modified):** `.golangci.yml` (enables `gosec` at `[L8]` as a `golangci-lint` sub-linter, complementing — not replacing — the standalone scan run here)
- **Repository security policy (read-only reference, NOT modified):** `SECURITY.md`
- **In-application security architecture documentation (read-only reference, NOT modified):** `docs/architecture/security.md`
- **Known security advisories in the AAP-pinned Mermaid `11.4.0` line (Row 17 — exposure surface analyzed):**
    - CVE-2026-41148 — improper sanitization of `classDef` in diagrams leads to CSS injection (first patched in 11.15.0 / 10.9.6). *Not reachable in the deck:* the deck uses no `classDef` directives.
    - CVE-2026-41149 — improper sanitization of `classDef` in state diagrams leads to HTML injection (first patched in 11.15.0 / 10.9.6). *Not reachable in the deck:* the deck uses no state diagrams and no `classDef` directives.
    - CVE-2026-41150 — Gantt-chart infinite-loop denial of service (first patched in 11.15.0 / 10.9.6). *Not reachable in the deck:* the deck uses no Gantt charts.
    - CVE-2026-41159 — improper sanitization of configuration (`fontFamily`, `themeCSS`, `altFontFamily`) leads to CSS injection (first patched in 11.15.0 / 10.9.6). *Not reachable in the deck:* the `mermaid.initialize()` config passes static string literals for `fontFamily`; `themeCSS` and `altFontFamily` are not set.
    - CVE-2025-54880 — architecture-diagram `iconText` XSS via `d3.html()` (first patched in 11.10.0+). *Not reachable in the deck:* the deck uses no architecture diagrams.
    - CVE-2025-54881 — sequence-diagram label XSS (first patched in 11.10.0+). *Not reachable in the deck:* the deck uses no sequence diagrams.
    - **Earliest patched landing release** is `11.15.0`. A future revision of AAP §0.7.2 can advance the pin to that release; until that revision is published, the AAP-literal `11.4.0` pin stands and the exposure surface is mitigated by the deck's narrow usage as described above.
