# Decision Log — Config H Snyk Scan of `blitzy-RudderStack`

## Metadata

- **Scope**: Config H of a multi-configuration security-tool comparison (Snyk CLI on a Go monorepo).
- **Repository**: `github.com/rudderlabs/rudder-server`, Go 1.26.1 (single Go module rooted at the repository root).
- **Driver**: AAP §0.4 Implementation Design + AAP §0.8.3 (14 enumerated decisions). The Explainability rule (AAP §0.8.1 Rule 1) makes this log a mandatory deliverable.
- **Companion deliverables**: `findings-config-h.json` (primary deliverable — single-line minified JSON), `scripts/normalize-snyk-findings.py` (implementation support — Python 3 stdlib-only normalizer), `blitzy-deck/index.html` (executive deck — reveal.js 5.1.0), `blitzy-deck/README.md` (operator note), `.gitignore` (one-line update to exclude transient scan artifacts).
- **Status**: Plan-time decision log. Refresh post-execution if scan results introduce new decisions; do NOT embed runtime data (finding counts, exit codes, durations) — those belong in execution logs.

## How to Read This Log

Each row in the Decisions table answers four questions in order:

- **Decision** — what was decided (the chosen behavior or value).
- **Alternatives Considered** — what else was on the table and rejected.
- **Rationale** — why the chosen path was selected over the alternatives.
- **Risks** — what could go wrong because of the choice and how the risk is mitigated.

A decision appears here only if a competent engineer could reasonably have chosen differently. Trivial mechanical translations of the user spec (e.g., emitting the literal `[]` when there are zero findings) are not enumerated.

## Decisions

_Bidirectional traceability matrix not applicable: this is an isolated tooling task with zero source-code transformations (see AAP §0.6.1 — explicitly out of scope)._

| # | Decision Point | Chosen Path | Alternatives Considered | Rationale | Risks |
|---|----------------|-------------|--------------------------|-----------|-------|
| 1 | Severity mapping for SARIF `level: none` | Map SARIF `level: none` records to `severity: low` in the normalized output. | (a) Drop the record entirely; (b) Map to `medium` (same as `note`); (c) Emit a sentinel string such as `unknown`. | Per AAP §0.2.4, the user severity table covers `error`/`warning`/`note` only. `none` is rare in Snyk Code output and the user spec is silent on it. Mapping to `low` preserves the record for downstream comparators without distorting the severity distribution upward, and avoids introducing a non-canonical severity vocabulary. | Cross-config comparators that key on severity bands may double-count `none`-level records as `low` while sibling configs (A–G) drop them or assign a different severity. Mitigated because `low` is the most conservative non-discarding choice and Decision #1 is documented here for sibling-config authors to align against. |
| 2 | CWE-vs-CVE fallback ordering for dependency `cwe` field | Prefer `vulnerabilities[*].identifiers.CWE[0]`; fall back to `vulnerabilities[*].identifiers.CVE[0]` only when the `CWE` array is empty or absent. | (a) Prefer CVE first and augment with CWE; (b) Emit both as a concatenated string; (c) Emit the CVE identifier always and never the CWE. | The user wording is "CVE ID; use CWE mapping if available" — the two literal readings are inverses of each other. The chosen order matches parity with the SAST `CWE-<n>` form (which is always a CWE identifier), maximizes semantic value (CWE classifies vulnerability category; CVE identifies a specific instance), and degrades gracefully to CVE when the Snyk database lacks CWE classification. Flagged here as a noted deviation per the Explainability rule. | Records that carry both a CWE and a CVE will surface only the CWE, hiding the CVE from cross-config diffs that key on CVE identifiers. Mitigated because the `description` field still carries the Snyk title, which typically references the CVE inline, preserving discoverability. |
| 3 | Description truncation strategy | Truncate `description` to a hard 200-character cap **after** the `[snyk-code] ` or `[snyk-deps] ` prefix is concatenated. Whitespace (tabs, newlines, carriage returns) is normalized to single spaces **before** truncation. | (a) Truncate before adding the prefix (would allow total length up to 200 + prefix length); (b) Truncate without whitespace normalization (would preserve newlines from SARIF messages); (c) Use a longer budget such as 256 characters. | Newlines inside JSON string values complicate downstream `cat findings-config-h.json \| wc -l == 1` verification and break visual-diff tools that line-orient on `\n`. Prefix-inclusive truncation guarantees the 200-character cap is never exceeded, matching the user spec verbatim. Pre-truncation whitespace normalization ensures the truncated remainder is still useful text rather than a leading whitespace run. | Long Snyk titles or SARIF messages are truncated mid-word, occasionally producing partial sentences. Mitigated because the `cwe` field carries the canonical identifier so the full advisory can be retrieved from the Snyk or MITRE databases via the identifier. |
| 4 | Intermediate-artifact retention | Retain `results-snyk-code.sarif` and `results-snyk-deps.json` on disk for one scan cycle after the merge completes. Do not delete them automatically. Add `.gitignore` patterns to prevent commit. | (a) Delete immediately after the normalizer writes `findings-config-h.json`; (b) Commit them as audit artifacts; (c) Move them to a dedicated `.scan-artifacts/` directory. | Retention enables post-run audit of the normalizer mapping, side-by-side diffing of raw versus merged output, and rerun of the normalizer with different parameters without re-invoking the scanners. `.gitignore` provides durable protection across all Config X siblings without requiring per-run cleanup discipline. | Disk-space growth on repeated runs (SARIF files for a large Go monorepo can reach tens of megabytes). Mitigated because the files are bounded by the scan target size and operators can clean up with `rm results-snyk-*.{sarif,json}` between runs. |
| 5 | `.gitignore` update strategy | Append two wildcard patterns (`results-snyk-*.sarif` and `results-snyk-*.json`) under a section-header comment that identifies them as Snyk scan artifacts. | (a) Do not modify `.gitignore` at all and rely on operator hygiene; (b) Add exact filenames only (`results-snyk-code.sarif`, `results-snyk-deps.json`); (c) Use a broader pattern such as `*.sarif`. | A two-line change is low risk; durable across all Config X siblings; the wildcard form (`results-snyk-*`) covers future scan variants (e.g., a hypothetical `results-snyk-iac.sarif`) without further edits. The chosen patterns are narrow enough to avoid accidentally ignoring unrelated `.sarif` or `.json` files that a developer may legitimately commit elsewhere in the tree. | A future Config that emits a SARIF outside the `results-snyk-` prefix would not be ignored. Mitigated because the naming convention is deliberately consistent across the comparison protocol; sibling configs follow the same `results-<tool>-*` shape by design. |
| 6 | Normalizer language choice | Implement the merge/minify normalizer in Python 3 using only the standard library (`json`, `os`, `sys`, `pathlib`, `re`). | (a) `jq` plus a bash driver script; (b) Node.js (already on host because Snyk CLI is an npm package); (c) Go program (matches the application's host language). | Python's `json.dumps(separators=(',', ':'))` produces byte-exact minification deterministically. `ensure_ascii=False` controls UTF-8 emission so non-ASCII titles survive intact. The standard library provides everything needed (no `pip install`); the host already has Python 3.13 in PATH. The language is well-suited to unit testing and a small script is easier to review than the equivalent `jq` expression. | Adds Python 3 to the runtime contract of the Config H workflow. Mitigated because Python 3 is universally present on modern Linux and macOS hosts, and the host's setup status log confirms `python3` is installed. |
| 7 | Executive deck slide budget | Produce exactly 16 slides — the mid-range of the 12–18 envelope mandated by the Executive Presentation rule. | (a) 12 slides (the rule's minimum, terser); (b) 18 slides (the rule's maximum, more breathing room). | 16 is the rule's explicit target. The slide map (AAP §0.4.8) allocates one slide per major concept (title, scope, methodology, results, risks, onboarding, closing) with four section-divider slides, fitting cleanly into 16 without padding. | Tight pacing for a slow speaker who lingers on each slide. Mitigated because each slide has a clear primary visual element (Mermaid diagram, KPI card, styled table, or Lucide SVG) and the 1920×1080 viewport keeps content legible at typical projection distances. |
| 8 | SAST CWE extraction priority | In the SARIF normalizer, read `rules[ruleId].properties.cwe[0]` first; if that field is missing or empty, scan `rules[ruleId].properties.tags` for `CWE-XXX` patterns. | (a) Scan `tags` first and use `properties.cwe` as the fallback; (b) Read both fields and emit the lower-numbered CWE; (c) Concatenate all matched CWE identifiers. | `properties.cwe` is the canonical typed field in current Snyk Code SARIF output and is preferred by the SARIF 2.1.0 tooling-conventions appendix. `tags` is a string-bag fallback used by older Snyk Code versions and other SARIF producers. Reading the canonical field first ensures stable behavior on the current Snyk CLI release. | Snyk Code SARIF schema may evolve in a future CLI release that drops `properties.cwe`. Mitigated because the tag-scan fallback covers both the older schema and future schema regressions; if both are absent the record carries an empty CWE which is documented as a known gap rather than a defect. |
| 9 | Path-relativity strategy | Use `os.path.relpath(uri, repo_root)` to convert scanner-emitted paths into repository-relative form. Fall back to the raw `uri` when `relpath` would cross a filesystem boundary or raises `ValueError`. | (a) Naive prefix-strip with `str.removeprefix(repo_root + '/')`; (b) `pathlib.Path.relative_to(repo_root)`; (c) Emit absolute paths as-is. | `os.path.relpath` is robust to symlinks, mixed absolute/relative inputs, and trailing-slash inconsistencies between SARIF (which may emit `file://` URIs) and Snyk dependency JSON (which emits manifest paths). It is tested across macOS and Linux and matches the user table's "relative path" requirement. `pathlib.Path.relative_to` raises on cross-tree paths, requiring a try/except wrapper that is functionally equivalent but more verbose. | When SARIF and Snyk JSON disagree on path style (URI vs. POSIX path), `relpath` normalizes both to the same form. Mitigated by post-emit validation in the normalizer that asserts every emitted path is non-empty and does not start with `/`. |
| 10 | Exit-code interpretation | Treat scanner exit code `0` (clean) and `1` (vulnerabilities found) as successful scan completion and proceed to merge. Treat any exit code `≥ 2` as a fatal scan failure that aborts the merge. | (a) Treat any non-zero exit code as fatal; (b) Treat all exit codes as success and rely solely on the structural validity of the output file. | Snyk uses exit code `1` to mean "vulnerabilities found", which is the EXPECTED outcome for any non-trivial scan target — treating it as an error would block effectively every run against a real codebase. Snyk's documentation explicitly defines `2` and above as scan errors (CLI failure, network failure, authentication failure). | A network failure mid-scan might emit `1` together with stale or partial output. Mitigated by verifying the output file's structural integrity in the normalizer (parse-as-JSON, assert presence of `runs[]` array for SARIF and `vulnerabilities[]` array for Snyk JSON) before merging. |
| 11 | Snyk CLI installation method | Install via `npm install -g snyk` as the primary path. | User-listed alternative: `apt install snyk`. | The npm distribution channel is the canonical Snyk channel recommended in Snyk's own documentation. The host already has Node.js v22.22.2 / npm v11.1.0, well above the Snyk-mandated minimum of Node 12+ / npm 7+. The npm release cadence aligns with the upstream Snyk release stream, whereas Linux distribution packages may lag behind. | A future execution host without Node would need the apt fallback. Documented but not chosen for the current host; the alternative remains a one-line operator override. |
| 12 | No upload to Snyk UI (`snyk monitor`) | Skip `snyk monitor` entirely; do not push findings to a Snyk org dashboard. | Run `snyk monitor` after each scan so findings are visible in the Snyk web UI. | Explicitly out of scope per AAP §0.6.2. The multi-configuration comparison is offline-aggregated by sibling `findings-config-X.json` files; uploading would persist findings to a Snyk org that is not part of the comparison protocol and would create accidental noise in operational dashboards. | The Snyk dashboard will not track this run's findings. Mitigated because the JSON deliverable (`findings-config-h.json`) is the canonical artifact for this task and the raw `results-snyk-*` files remain available for ad-hoc inspection. |
| 13 | `.snyk` policy preserved as-is | Leave the existing `.snyk` policy file unchanged, including its five `ignore` rules that all expired on `2025-01-01T00:00:00.000Z`. | (a) Remove the expired rules; (b) Re-issue them with new expiration dates; (c) Delete `.snyk` entirely. | Modifying `.snyk` is explicitly out of scope per AAP §0.6.2. The expired ignore rules no longer suppress findings (Snyk CLI treats an `expires` timestamp in the past as inactive), which is the expected behavior for a "Config H as-is" snapshot of the repository's current posture. | The scan may surface a higher finding count than historical Snyk baselines that ran while the ignore rules were active. Documented as an observation in the executive deck rather than treated as a regression. |
| 14 | Output location for `findings-config-h.json` | Place at the repository root (`./findings-config-h.json`). | (a) Under `scripts/`; (b) Under `blitzy-docs/`; (c) In a new `findings/` directory. | Repository root matches the implied parity with sibling `findings-config-{a..g}.json` files and any future Config X variants. Downstream comparators expect a flat layout discoverable by a single glob (`findings-config-*.json`). | Visual clutter in the repository root listing as additional configurations are added. Mitigated because the files are small, share a clear `findings-config-` prefix, and can be collected into a directory by a downstream aggregator without touching this repository. |

## User-Verbatim Critical Directives

The four directives below are preserved exactly as the user supplied them in AAP §0.8.2. Command strings are reproduced byte-for-byte, including the unconventional ordering of the redirect operator in Directive 3 (the redirect appears mid-command rather than at the end). Shell semantics resolve the redirect correctly; the ordering is intentional and is NOT to be "fixed".

1. **Directive 1 — Install and authenticate the Snyk CLI**
   - Commands (verbatim):

     ```
     npm install -g snyk
     # or: apt install snyk
     ```

   - Authenticate by setting `SNYK_TOKEN` as an environment variable with a valid API token. Snyk requires network access — there is no offline mode.
   - Pass/fail: `snyk auth check` confirms authentication; `snyk --version` returns a version string.

2. **Directive 2 — Snyk Code SAST scan**
   - Command (verbatim):

     ```
     snyk code test --sarif-file-output=results-snyk-code.sarif /path/to/blitzy-RudderStack
     ```

   - Record exit code and scan duration (wall-clock).
   - Pass/fail: `results-snyk-code.sarif` is produced and contains valid JSON.

3. **Directive 3 — Snyk Open Source dependency scan**
   - Command (verbatim):

     ```
     snyk test --json > results-snyk-deps.json /path/to/blitzy-RudderStack
     ```

   - Record exit code and scan duration (wall-clock).
   - Pass/fail: `results-snyk-deps.json` is produced and contains a vulnerabilities array.

4. **Directive 4 — Normalize and merge findings to `findings-config-h.json`**
   - Merge SAST and dependency findings into `findings-config-h.json`. The file MUST be valid JSON minified to a single line. Encoding: UTF-8. If zero findings, write `[]`.
   - Five fields per record: `file`, `line`, `severity`, `cwe`, `description`.
   - Pass/fail: `cat findings-config-h.json | wc -l` returns `1`; valid JSON; every finding has all five fields populated; no `description` exceeds 200 characters.

## Field Mapping Reference

Reproduced verbatim from AAP §0.2.3. The normalizer maps source fields into the unified output schema using the following table.

| Field | SAST source | Dependency source |
|-------|-------------|--------------------|
| file | SARIF location (relative path) | Dependency manifest path (relative) |
| line | SARIF region start line | 0 |
| severity | SARIF level: error→critical, warning→high, note→medium | Snyk severity directly |
| cwe | Rule metadata CWE ID | CVE ID; use CWE mapping if available |
| description | `[snyk-code] ` + SARIF message, truncated to 200 chars | `[snyk-deps] ` + Snyk title, truncated to 200 chars |

## Severity Mapping Quick Reference

The table below mirrors AAP §0.4.4 and consolidates the user-supplied mapping with the project-specific extension for the `none` SARIF level (Decision #1).

| Source | Source value | Normalized `severity` |
|--------|--------------|-----------------------|
| Snyk Code SARIF | `error` | `critical` |
| Snyk Code SARIF | `warning` | `high` |
| Snyk Code SARIF | `note` | `medium` |
| Snyk Code SARIF | `none` (rare) | `low` (Decision #1) |
| Snyk deps JSON | `critical` | `critical` (passthrough) |
| Snyk deps JSON | `high` | `high` (passthrough) |
| Snyk deps JSON | `medium` | `medium` (passthrough) |
| Snyk deps JSON | `low` | `low` (passthrough) |

## Output Schema (Verbatim)

The user-provided canonical shape of `findings-config-h.json`, preserved exactly as written in AAP §0.2.3:

```
[{"file":"<relative path>","line":<integer>,"severity":"<critical|high|medium|low>","cwe":"<CWE-ID>","description":"<max 200 chars>"},...]
```

Empty-state (zero merged findings): `[]`.

Encoding and structural invariants:

- UTF-8 without BOM.
- No trailing newline (the file is a single line; `wc -l` returns either `0` or `1` depending on whether the operating system counts a non-terminated final line as a line).
- Field order on each record is `file`, `line`, `severity`, `cwe`, `description` — preserved by relying on Python's insertion-order-preserving `dict` (Python 3.7+).
- Records appear in source order: SAST records (prefix `[snyk-code] `) precede dependency records (prefix `[snyk-deps] `).

## Operational Prerequisites

The following conditions MUST hold at scan time. Each is an external prerequisite; this task does NOT provision any of them.

- **Network egress** to `https://snyk.io` and `https://downloads.snyk.io` (or `https://static.snyk.io/cli` for the binary wrapper). Snyk has no offline mode; firewall or proxy configuration that blocks these hosts will cause all four directives to fail.
- **`SNYK_TOKEN` environment variable** populated with a valid Snyk API token from the Snyk organization. The setup status log confirms the variable is NOT currently set in the execution environment — the operator MUST export it before invoking any `snyk` command.
- **Node.js ≥ 12** and **npm ≥ 7** for the `npm install -g snyk` installation path. The host satisfies both with Node.js v22.22.2 (or v20.20.2 per the setup status log) and npm v11.1.0.
- **Python 3 ≥ 3.8** for `scripts/normalize-snyk-findings.py`. The host satisfies this with Python 3.13.7. The script is standard-library-only; no `pip install` is required.

## What This Task Does NOT Do

The boundaries below mirror AAP §0.6.2. Each item is explicitly out of scope for the Config H task.

- Does **NOT** fix any underlying vulnerability surfaced by the scans. Updating vulnerable dependencies, patching code, or adding new `replace` directives to `go.mod` is the responsibility of a separate remediation task.
- Does **NOT** modify `.snyk`. The five `ignore` rules that all expired on `2025-01-01T00:00:00.000Z` are preserved as-is; re-issuing, removing, or rewriting them is out of scope.
- Does **NOT** add Snyk to CI/CD. The contents of `.github/workflows/*` are unchanged; no GitHub Action, GitLab pipeline step, or Jenkins job is added.
- Does **NOT** run `snyk container test`, `snyk iac test`, `snyk monitor`, `snyk sbom`, or `snyk aibom`. Only `snyk code test` (SAST) and `snyk test` (Open Source / dependencies) are in scope.
- Does **NOT** generate sibling Config A–G findings files. The naming convention `findings-config-h.json` implies sibling artifacts but those are produced by separate task scopes; cross-config aggregation is a downstream comparator's responsibility.
- Does **NOT** provision the `SNYK_TOKEN`. Creating the Snyk organization, projects, service accounts, or API tokens is an external prerequisite (AAP §0.7.4).
- Does **NOT** modify any Go source file, `go.mod`, `go.sum`, `Dockerfile`, `docker-compose.yml`, `Makefile`, `README.md`, `SECURITY.md`, `.deepsource.toml`, `.golangci.yml`, `codecov.yml`, or any file under `refs/segment-docs/`.
- Does **NOT** include performance optimizations such as caching of `results-snyk-*` between runs or parallelization of the SAST and dependency scans; execution is sequential to simplify exit-code handling.

## File Inventory

The transformation matrix below mirrors AAP §0.5.1 and §0.5.4. Every file touched, referenced, or produced by the Config H task is enumerated.

| Path | Transformation | Purpose |
|------|----------------|---------|
| `findings-config-h.json` | CREATE | Primary deliverable — single-line minified JSON merging SAST and dependency findings. Placed at the repository root for parity with sibling Config X files. |
| `scripts/normalize-snyk-findings.py` | CREATE | Python 3 normalizer that reads SARIF and Snyk deps JSON, applies severity/CWE/description mapping, and writes the single-line output. Standard-library-only; no `pip` dependencies. |
| `DECISIONS.md` | CREATE | This file — decision log mandated by the Explainability rule. |
| `blitzy-deck/index.html` | CREATE | Self-contained reveal.js 5.1.0 executive deck. Pinned CDN versions: reveal.js 5.1.0, Mermaid 11.4.0, Lucide 0.460.0. 16 slides covering scope, methodology, findings, risks, and onboarding. |
| `blitzy-deck/README.md` | CREATE | One-page operator note explaining how to open the deck (no build step) and confirming expected viewing dimensions (1920×1080). |
| `.gitignore` | UPDATE | Append two ignore patterns (`results-snyk-*.sarif`, `results-snyk-*.json`) under a section-header comment to prevent transient scan artifacts from being committed. |
| `results-snyk-code.sarif` | CREATE (transient, gitignored) | SARIF 2.1.0 output of `snyk code test`. Working file consumed by the normalizer; NOT a deliverable; NOT committed. |
| `results-snyk-deps.json` | CREATE (transient, gitignored) | JSON output of `snyk test --json`. Working file consumed by the normalizer; NOT a deliverable; NOT committed. |
| `go.mod`, `go.sum`, `.snyk`, `**/*.go` | REFERENCE | Read-only inputs to the Snyk scans. Not modified. |

## References

- [Snyk CLI installation (npm)](https://docs.snyk.io/developer-tools/snyk-cli/install-or-update-the-snyk-cli/installing-snyk-cli-as-a-binary-using-npm)
- [Snyk CLI authentication](https://docs.snyk.io/snyk-cli/authenticate-to-use-the-cli)
- [Snyk Code documentation](https://docs.snyk.io/scan-with-snyk/snyk-code)
- [Snyk Open Source](https://docs.snyk.io/scan-with-snyk/snyk-open-source)
- [Snyk CLI exit codes](https://docs.snyk.io/snyk-cli/exit-codes)
- [npm registry for `snyk`](https://www.npmjs.com/package/snyk)
- [SARIF 2.1.0 specification](https://docs.oasis-open.org/sarif/sarif/v2.1.0/sarif-v2.1.0.html)
- [CWE list (MITRE)](https://cwe.mitre.org/data/)
- [CVE list (NIST NVD)](https://nvd.nist.gov/vuln/search)

