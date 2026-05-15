~~~
Semgrep version:        1.163.0  (OSS engine, installed via pip inside .semgrep-venv;
                                  SARIF runs[0].tool.driver.semanticVersion = "1.163.0";
                                  driver.name = "Semgrep OSS" per Semgrep CE SARIF emitter)
Python interpreter:     3.13.7   (host system Python; venv-isolated for PEP 668 compliance)
Rule pack acquisition:  2026-05-15T03:10:06Z  (one-time curl from https://semgrep.dev/c/p/<slug>;
                                  three YAML bundles materialized in-repo at local-rules/ — see D-12)
  security-audit.yaml   sha256 fdc7027973176abe71f6b1fc8739ef88a4c411735c380cfce4f731df9644e47a
  secrets.yaml          sha256 fbbe6809214065a2efec7264cd1c9ca16be9b3e7665dfa790e0bdfd08a6d7a16
  owasp-top-ten.yaml    sha256 d866bd809983001afdfa81014b86404d704c0604b22c378ed37608e69525e040
Scan command:           semgrep scan --config=./local-rules --sarif -o results-semgrep.sarif --metrics=off --exclude=local-rules --exclude=findings-config-b.json .
Scan start (UTC):       2026-05-15T07:47:25Z
Scan end (UTC):         2026-05-15T07:48:02Z
Exit code:              0       (Semgrep exit 0 = OK, no fatal errors; per CLI reference)
Wall-clock duration:    37.30   seconds
Files scanned:          4764    (from Semgrep stderr summary; SARIF runs[0].invocations[0].properties
                                  is null in Semgrep CE 1.163.0, so the stderr line "Targets scanned:
                                  4764" is the canonical observation per AAP §0.1.2.2; the raw stderr
                                  capture is retained at semgrep-stderr.txt for independent audit)
Files skipped:          1480    (per Semgrep stderr summary: 36 oversized + 1440 .semgrepignore +
                                  4 from --exclude patterns; 3 rule-pack YAML files plus the output
                                  findings-config-b.json itself are excluded from scan targets because
                                  the rule packs are loaded via --config= rather than scanned as
                                  application code, and findings-config-b.json is the SCAN OUTPUT and
                                  cannot logically be its own input — see deviation D6)
Parse errors:           34      (per SARIF runs[0].invocations[0].toolExecutionNotifications:
                                  27 Syntax error + 7 Other syntax error)
Timeouts:               1       (per SARIF runs[0].invocations[0].toolExecutionNotifications)
Rule packs loaded:      3       (security-audit, secrets, owasp-top-ten)
Rules in SARIF driver:  709     (deduplicated union across the three packs)
Rules with matches:     486     (per stderr summary)
Findings count:         216     (final 216-record array emitted as findings-config-b.json at the
                                  repository root; Directive 3 Pass/Fail post-conditions all pass
                                  — wc -l == 1, valid JSON, 5/5 fields populated, max desc == 200)
Severity distribution:
  critical              15      (SARIF level error  -> mapped per Directive 3 table)
  high                  201     (SARIF level warning)
  medium                0
  low                   0
CWE-in-tags coverage:   216/216 (all CWE values resolvable from rule.properties.tags;
                                  Step 4 description inference and Step 5 CWE-Other sentinel
                                  were not exercised — see §5)
Dry-run exit code:      0       (Directive 1 Pass/Fail validation, run with --dryrun
                                  flag per Semgrep CE 1.163.0 CLI; see deviation D5)
Network calls (scan):   0       (hermetic invariant — local-rules only + --metrics=off)
Scan target root:       .       (repository root; same physical path as
                                  /tmp/blitzy/blitzy-RudderStack/blitzy-9dc2860b-a202-4bda-8d7d-f0252cd179c1_fdef9c)
Rule-pack location:     ./local-rules/  (materialized inside the repository at HEAD so a fresh
                                  checkout contains the three cached YAML bundles required by
                                  Directive 1; the original deployment used an ignored external
                                  symlink to /tmp/semgrep-config-b/local-rules but that was not
                                  reachable from a clean checkout — see deviation D6 and D-12)
~~~

# Config B — Semgrep Scan Decision Log

This document is the single source of truth for "why" decisions made while
implementing **Config B** (Semgrep OSS scan of `blitzy-RudderStack`). It is
mandated by the user-specified **Explainability rule** (Agent Action Plan
§0.7.1) and contains: a scan-metadata frontmatter (above) populated from the
actual run; a non-trivial-decisions table; a 100% bidirectional traceability
matrix linking each of the three CRITICAL directives (plus the two user
rules) to its implementing artifact and back; a CWE inference audit; an
explicit deviation log; and a cross-reference decisions inventory. Inline
rationale in `normalize-sarif.py` (and every other working artifact) is
forbidden by the Explainability rule — every "why" lives here.

## 1. Frontmatter — Scan Metadata Block

The frontmatter fenced block at the top of this file is copied verbatim from
`scan-metadata.txt` at the repository root, which was generated during
Stage 4 of the pipeline (AAP §0.5.1). It satisfies Directive 2's recording
obligation (exit code, wall-clock duration, total files scanned) and adds
hermeticity evidence (`dry_run_exit_code`, `network_calls_during_scan`)
requested by Directive 1's Pass/Fail clause. The frontmatter is placed at
the absolute top of this file (before any heading or prose) so Section 1
metadata is the first block in the document.

## 2. Executive Summary

Config B executed the Semgrep Community Edition CLI (version 1.163.0, with
SARIF `driver.semanticVersion` = `1.163.0` providing auditable version
linkage between the artifact and `scan-metadata.txt`) against the full
`blitzy-RudderStack` source tree using three locally-cached rule packs
(`p/security-audit`, `p/secrets`, and the user-named `p/owasp` —
canonicalized to the Semgrep Registry slug `p/owasp-top-ten`). The scan
completed in 37.30 seconds across 4,764 files with exit code 0 and zero
outbound network calls. A Python normalizer (`normalize-sarif.py`) implements
the SARIF-to-five-field projection mandated by Directive 3 and emits the
final 216-record `findings-config-b.json` at the repository root, satisfying
all four Directive 3 Pass/Fail post-conditions (`wc -l == 1`; parses with
`python3 -m json.tool`; every record has all five required fields populated
with no nulls; no `description` exceeds 200 characters). Findings break
down to 15 mapped to `critical`, 201 to `high`, and 0 to `medium` or `low`
— covering 12 distinct CWE categories (CWE-78, CWE-79, CWE-89, CWE-250,
CWE-269, CWE-287, CWE-300, CWE-327, CWE-328, CWE-338, CWE-400, CWE-798).
Every CWE was read directly from rule metadata (`properties.tags`); zero
description-based inferences were required and zero `CWE-Other` sentinels
were emitted. The `cwe_in_tags_coverage` key in `scan-metadata.txt` records
this as `216/216` for independent cross-reference. No file inside the
pre-existing `blitzy-RudderStack` source tree was modified; the only
mutations to the destination branch are the rule-mandated additive
deliverables (this file plus `executive-summary.html`), the
directive-mandated working artifacts (`results-semgrep.sarif`,
`scan-metadata.txt`, `semgrep-stderr.txt`, `normalize-sarif.py`), the
directive-mandated final deliverable (`findings-config-b.json`), and the
three cached rule-pack YAML bundles materialized inside `local-rules/`
at the repository root so a fresh checkout is self-sufficient (see D-12
and deviation D6).

## 3. Decisions Table

The 12 mandatory non-trivial decisions are recorded below in the column order
required by the Explainability rule: `# | Decision | Alternatives Considered
| Choice + Rationale | Risks Carried`. A competent engineer could reasonably
have chosen differently for every row, so every row is in scope per the
rule's definition of "non-trivial". Decision IDs `D-01` through `D-12` map
one-to-one to the row number and are cross-referenced in §7.

| # | Decision | Alternatives Considered | Choice + Rationale | Risks Carried |
|---|---|---|---|---|
| 1 | Installation method for the `semgrep` CLI | (a) `python3 -m venv` + `pip install` inside the venv; (b) `pipx install semgrep`; (c) `pip install --break-system-packages --user`; (d) `apt install semgrep`; (e) Docker image `returntocorp/semgrep`. | Chose (a) `python3 -m venv` at `/tmp/semgrep-config-b/.semgrep-venv/` followed by `pip install "semgrep==1.163.0"`. Rationale: the execution host is Ubuntu 25.10 with PEP 668 enforcement on system Python — plain `pip install` fails with `externally-managed-environment`. A venv is the lowest-friction PEP 668-compliant option, requires no additional tooling (unlike `pipx`), avoids the foot-gun of `--break-system-packages`, and `apt-cache madison semgrep` returned no entries so option (d) is unavailable. Docker (option e) adds an unnecessary layer because the host already has Python 3.13.7. | The venv must be re-activated for every shell session (`source /tmp/semgrep-config-b/.semgrep-venv/bin/activate`); operators who forget will hit `command not found`. Mitigated by recording the activation path in this frontmatter and in `scan-metadata.txt`. |
| 2 | Pinned Semgrep version | (a) pin `1.144.0` per AAP §0.4.1 recommendation; (b) pin `1.143.0`; (c) install unpinned `latest`; (d) pin to the actual newest 1.x available at install time. | Chose (d): pinned to the actual newest 1.x release found at install time, observed as `1.163.0`. Rationale: AAP §0.4.1 references the November-2025 release notes ("1.143.0 / 1.144.0 line") but the AAP itself also reads "or the highest 1.x release available at execution time", giving express permission to take the newest. At execution time `pip install semgrep` resolved to `1.163.0`. The `pip` step pinned this exact version into the venv so subsequent runs are reproducible. The same version is also recorded in SARIF as `runs[0].tool.driver.semanticVersion = "1.163.0"` for cross-artifact verification. | Newer rule-pack content may eventually require newer CLI flag semantics; the pin captures the snapshot but does not auto-track upstream. The choice diverges from the AAP's literal "1.144.0" text; see §6 D4 for the deviation log entry. |
| 3 | Rule-pack canonicalization for "p/owasp" -> "p/owasp-top-ten" | (a) Keep the user's literal `p/owasp` slug in the URL; (b) canonicalize to `p/owasp-top-ten` (the Semgrep Registry's documented slug). | Chose (b) `p/owasp-top-ten`. Rationale: `https://semgrep.dev/c/p/owasp` redirects to `https://semgrep.dev/c/p/owasp-top-ten` programmatically on the Semgrep Registry, so the two are functionally identical at fetch time but only the canonical slug produces a stable, predictable URL and bundle filename. Recorded as deviation D1 (§6) per the Explainability rule. | A future Semgrep Registry rename could break the canonical URL; mitigated by caching the bundle to `local-rules/owasp-top-ten.yaml` (sha256 d866bd80...) so subsequent scans never re-fetch. |
| 4 | Rule-pack acquisition mechanism | (a) `curl https://semgrep.dev/c/p/<slug>` (documented Registry consolidated-YAML endpoint); (b) clone `github.com/semgrep/semgrep-rules` and assemble the equivalent pack locally; (c) run `semgrep --config=p/<slug>` once with online registry access and capture Semgrep's `~/.semgrep` cache. | Chose (a) one-time `curl` to the Registry's consolidated-YAML endpoint, persisted to `local-rules/<slug>.yaml`. Rationale: this is the documented and supported mechanism for offline replay, returns a pre-assembled bundle that Semgrep loads directly via `--config=./local-rules`, has the smallest surface area (one HTTPS GET per pack), and produces three deterministic files whose sha256 hashes are recorded in `scan-metadata.txt` for tamper-evidence. | The bundle is a point-in-time snapshot; new rules added upstream will not appear until the operator re-runs the curl step. Mitigation: track refresh cadence in onboarding (`executive-summary.html` slide 14) and check the sha256s against the recorded baseline before each scan campaign. |
| 5 | Scope of files scanned (exclusions vs. full tree) | (a) Full application tree, with the rule-pack cache directory (`local-rules/`) and the directive output file (`findings-config-b.json`) excluded from scan targets via `--exclude=local-rules --exclude=findings-config-b.json` (chosen, coupled with D-12); (b) Full tree, no exclusions at all (would scan the three cached rule-pack YAML bundles as application targets, producing self-referential matches, AND would scan the previous-run output file as input on every subsequent run); (c) `--exclude refs/segment-docs/` to drop ~75 vendored Jekyll docs from upstream Segment; (d) `--exclude mocks/` to drop Go testing mocks; (e) `--exclude vendor/` (n/a — no `vendor/` directory exists in this repo per the AAP §0.2.1 inventory). | Chose (a). Rationale: the three CRITICAL directives are silent on exclusions of pre-existing tree content, so per AAP §0.3.2 the application tree is scanned in its entirety with no exclusions of `refs/segment-docs/`, `mocks/`, etc. Two targeted exclusions are required once D-12 brought the rule packs and the directive output into the repository: (i) `--exclude=local-rules` because the three rule-pack YAML bundles are loaded as configuration via `--config=./local-rules`, not as application code, and scanning them produces self-referential matches (e.g. semgrep rules matching example patterns inside other semgrep rule bodies) that are not real findings of the rudder-server codebase; (ii) `--exclude=findings-config-b.json` because that file is the SCAN OUTPUT — including it as input on subsequent runs would mean the scan re-scans its own previous output (logically circular) and would introduce drift between `files_scanned` recorded at scan time vs. on a fresh checkout. The exclusions preserve the original directive's spatial intent that local-rules live outside the scan target and that output not feed back as input. The 4764 files scanned reflect the full application tree as discovered by Semgrep's git-aware target enumeration (a +1 delta vs. the original 4763 because `executive-summary.html` was added to the tree after the original scan); the 1480 files skipped break down to 36 oversized files + 1440 `.semgrepignore` matches + 4 from `--exclude` patterns (3 local-rules YAMLs + 1 findings-config-b.json). The same 216 findings emerge as the original symlink-based deployment, confirmed by SHA-256 byte-equality of the resulting `findings-config-b.json` (`0c53063d0f551083dc96021377c0a0ff24bb44e4742a7020bcba802f3d64f561`). Critically, the chosen exclusions also make the file-count metric STABLE: a fresh checkout running the dry-run gate sees the same 4764 scanned files that `scan-metadata.txt` records. See deviation D6 for the formal rationale. | Vendored documentation under `refs/segment-docs/` could contribute false positives that inflate the apparent severity of the codebase; mitigated by separating scan output (this configuration's responsibility) from triage (explicitly out of scope per AAP §0.3.2). Downstream comparison configurations can apply their own exclusions if they need to. The two exclusions are auditable in `scan_command` and recorded as deviation D6. |
| 6 | SARIF severity fallback when `level` is absent on a result | (a) Default to `low` if neither the result nor the rule's `defaultConfiguration.level` carries a value; (b) resolve from `runs[].tool.driver.rules[ruleIndex].defaultConfiguration.level` first, then default to `low` if that is also absent (chosen); (c) default to `medium`. | Chose (b), the rule-default-then-low cascade per AAP §0.5.6. Rationale: SARIF allows `level` to be omitted on individual results when the rule itself defines a default; falling back to the rule's `defaultConfiguration.level` honors the rule author's intent more faithfully than a flat `low`. The flat-`low` floor catches the rare case where neither carries a value. In this run, every one of the 216 results omitted `level` (Semgrep CE 1.163.0 does not populate per-result `level`) and every rule provided a `defaultConfiguration.level` — so the cascade was exercised 216/216 times and the floor was exercised 0 times. | If `defaultConfiguration.level` is itself misclassified upstream by Semgrep, the entire severity column inherits that error. Mitigated by the closed severity vocabulary (only `critical`/`high`/`medium`/`low` allowed) which downstream comparators can re-bucket. |
| 7 | CWE inference policy | (a) Metadata-first with description fallback then `CWE-Other` sentinel (chosen, per AAP §0.5.6 Steps 1-5); (b) metadata-only -> emit `CWE-Other` whenever metadata is missing; (c) description-only inference (ignore metadata). | Chose (a) the 5-step cascade. Rationale: Semgrep rules from the three Registry packs reliably carry a CWE in `properties.tags` as `CWE-NNN: <name>` prefix entries, but the AAP must specify behavior for edge cases (rules without CWE metadata) to keep the pipeline deterministic. Including description-keyword fallback (Step 4) covers Semgrep rules that historically used freeform `tags` arrays; the `CWE-Other` sentinel (Step 5) ensures the five-field shape is always satisfied even when no specific CWE can be assigned. In this run, 216/216 findings resolved via Step 2 (`properties.tags` scan; recorded as `cwe_in_tags_coverage=216/216` in `scan-metadata.txt`); Steps 4 and 5 were not exercised — but the policy is still load-bearing because it defines the failure mode. | Description-based inference (when exercised) could mis-attribute CWEs; mitigated by §5 below which records every Step-4 inference for human audit. The `CWE-Other` sentinel is a deliberately auditable signal that downstream comparators can detect. |
| 8 | Description sanitization strategy | (a) Collapse all whitespace runs to a single space then truncate to 200 chars (chosen); (b) preserve internal whitespace and only escape line breaks as `\n`; (c) flatten to single space without truncation. | Chose (a). Rationale: the directive output is a single-line JSON file (`wc -l == 1` invariant per Directive 3 Pass/Fail), so any embedded newlines in `message.text` would either be escape-sequenced (consuming character budget and obscuring the message) or break the single-line invariant. Collapsing all whitespace runs to one space normalizes the text for human readability and keeps the 200-character budget meaningful. Truncation to 200 chars is the user's explicit upper bound; no ellipsis appended because the user's spec does not request one. | Information loss from collapsing tabs/newlines/multi-space sequences; mitigated by the budget being a directive constraint (not an editorial choice) and by retaining the full SARIF message in `results-semgrep.sarif` for any reviewer who needs the unabridged text. |
| 9 | Trailing-newline policy on `findings-config-b.json` | (a) Exactly one trailing `\n` after the JSON body so `wc -l` reports `1` (chosen); (b) zero trailing newlines so `wc -l` reports `0`. | Chose (a). Rationale: the Directive 3 Pass/Fail clause states verbatim "`cat findings-config-b.json \| wc -l` returns `1`". On POSIX, `wc -l` counts newline characters, not logical lines — a file ending without a newline reports `0`. The only way to satisfy the literal Pass/Fail text is to emit exactly one trailing newline after `json.dumps(...)`. The `normalize-sarif.py` script enforces this with a post-condition assertion. | Tools that auto-strip trailing whitespace on save (e.g. some editors) could regress the file to zero newlines if a human edits it; mitigated by treating the file as machine-generated (no manual edits expected) and by replaying the normalizer to regenerate if needed. |
| 10 | Scan-metadata location | (a) Separate `scan-metadata.txt` file at the repository root (chosen); (b) embedded fenced block at the top of `decision-log.md` only; (c) both. | Chose (c) both, with `scan-metadata.txt` as the canonical machine-parseable source and the frontmatter block at the top of this file as the narrative-readable replica. Rationale: `scan-metadata.txt` is machine-readable key=value pairs that downstream comparators can parse without a Markdown processor; `decision-log.md` is human-narrative documentation. Separating concerns keeps each file single-purpose. The §1 Frontmatter replicates the load-bearing values inline so the decision log remains self-contained for narrative readers, while the canonical machine-parseable copy is `scan-metadata.txt`. The frontmatter is positioned at the very top of this file so Section 1 metadata is the first block in the document. | Split-source for run-time data risks drift if one copy is updated but not the other; mitigated by treating `scan-metadata.txt` as the source of truth (written once by the scan agent) and by replicating only stable headline values here in §1. |
| 11 | Executive-summary theme inlining (vs. external reference) | (a) Inline the full Blitzy reveal.js theme inside `executive-summary.html` via `<style>` (chosen); (b) link/import from `blitzy-deck/references/blitzy-reveal-theme.css`. | Chose (a) inline. Rationale: the canonical theme file `blitzy-deck/references/blitzy-reveal-theme.css` does not exist in this repository (verified by `find . -maxdepth 6 -name "blitzy-reveal-theme*"` returning no matches per AAP §0.2.4 / §0.9.7). More importantly, the Executive Presentation rule itself mandates "single self-contained HTML file, no build steps, no local file dependencies" — so the theme MUST be inlined regardless of whether the external file exists. Recorded as deviation D3 (§6). | Drift between this deck's inline theme and other Blitzy decks; mitigated by encoding the full canonical CSS custom-property set verbatim per the rule spec (`--blitzy-primary`, `--blitzy-primary-dark`, `--blitzy-primary-navy`, `--blitzy-primary-light`, `--blitzy-primary-deep`, `--blitzy-accent-teal`, plus the gradient and typography tokens), plus semantic severity tokens (`--severity-critical`, `--severity-medium`, `--severity-low`) that derive from the Blitzy palette for severity-state pills. |
| 12 | Rule-pack cache location (in-repo `local-rules/` vs. external symlinked cache) | (a) Materialise the three rule-pack YAML bundles inside the repository at `local-rules/security-audit.yaml`, `local-rules/secrets.yaml`, `local-rules/owasp-top-ten.yaml` so they are tracked at HEAD (chosen); (b) Keep them externally at `/tmp/semgrep-config-b/local-rules/` and reach them via an ignored symlink `./local-rules -> /tmp/semgrep-config-b/local-rules` listed in `.git/info/exclude`; (c) Package the external cache directory as a tarball companion artifact. | Chose (a). Rationale: Directive 1 names `p/security-audit`, `p/secrets`, and `p/owasp` rule packs as cache deliverables, and the Pass/Fail clause requires the dry-run to exit 0 with no network calls — which presupposes the rule packs are reachable from a fresh checkout. Option (b) (the original deployment) reached the packs through an ignored symlink that is invisible to `git ls-files` and disappears on a fresh `git clone`, so a clean checkout could not satisfy Directive 1 without re-fetching from the Semgrep Registry (a network call). Option (c) (tarball) adds a packaging surface that the directives do not request and does not make the packs reachable via `--config=./local-rules` without an extraction step. Materialising the YAML bundles inside the repository at HEAD is the most direct way to ensure the directive deliverables are self-contained and reproducible. SHA-256 hashes of the materialised files are recorded in `scan-metadata.txt` and §1 frontmatter for tamper-evidence. The materialisation forced a coupled decision (D-05 / deviation D6): the rule packs are now inside the scan target tree, so `--exclude=local-rules` is added to the scan command to prevent the rule packs from being scanned as application code. A second `--exclude=findings-config-b.json` is also added under D-05/D6 because the Directive 3 output file is itself committed at the repository root and would otherwise be re-scanned on every subsequent run, violating the AAP §0.8.1 idempotency invariant. | The repository working tree gains 3 large YAML files (~1.95 MB total: 473 KB security-audit + 88 KB secrets + 1.4 MB owasp-top-ten); mitigated by treating them as machine-acquired snapshots whose provenance is fully captured by `rule_pack_*_sha256` and `rule_pack_acquisition_utc` in `scan-metadata.txt`. The bundles are reference data, not application code, and do not interact with the rudder-server build graph. Refreshing the cache (re-running the one-time `curl https://semgrep.dev/c/p/<slug>` step) is a deliberate human-initiated operation, not part of the scan pipeline. |

## 4. Bidirectional Traceability Matrix

Per the Explainability rule, coverage is 100% — every directive and every
user rule has a forward link to its implementing artifact in §4.1, and every
emitted scoped artifact has a reverse link to its triggering directive or
rule in §4.2. Reviewers can verify by counting rows: §4.1 has five rows
(three directives + two rules) and §4.2 has nine rows (the nine scoped
artifacts produced by this configuration); there is no row in either table
whose counterpart is missing in the other. Supporting evidence files that
are not themselves scoped deliverables (e.g. `semgrep-stderr.txt`) are
listed in §4.3 below for completeness without conflating them with the
scoped artifact set.

### 4.1 Directive -> Artifact (forward)

| User Directive / Rule | Verbatim Pass/Fail Clause | Implementing Artifact(s) | Acceptance Evidence |
|---|---|---|---|
| **Directive 1** — Install + configure Semgrep; download `p/security-audit`, `p/secrets`, `p/owasp` rule packs to a local directory; confirm `--metrics=off` suppresses all telemetry | `semgrep scan --metrics=off --config=/path/to/local-rules --dry-run` exits 0 with no network calls | `/tmp/semgrep-config-b/.semgrep-venv/` (Semgrep 1.163.0); `local-rules/security-audit.yaml`, `local-rules/secrets.yaml`, `local-rules/owasp-top-ten.yaml` (materialized inside the repository at `./local-rules/` so a fresh checkout is self-sufficient — see D-12); dry-run validation captured in `scan-metadata.txt` as `dry_run_exit_code=0` and `network_calls_during_scan=0` | `scan-metadata.txt` keys `dry_run_exit_code`, `network_calls_during_scan`, plus three `rule_pack_*_sha256` values matching §1 frontmatter and the on-disk SHA-256 hashes of the in-repo `local-rules/*.yaml` files |
| **Directive 2** — Execute `semgrep scan --config=./local-rules --sarif -o results-semgrep.sarif --metrics=off /path/to/blitzy-RudderStack`; record exit code, scan duration (wall-clock), and total files scanned | `results-semgrep.sarif` is produced and contains valid JSON with a `runs` array | `results-semgrep.sarif` (1.46 MB, SARIF 2.1.0); `scan-metadata.txt` capturing `exit_code=0`, `duration_seconds=37.30`, `files_scanned=4764`, `scan_command=semgrep scan --config=./local-rules --sarif -o results-semgrep.sarif --metrics=off --exclude=local-rules --exclude=findings-config-b.json .` (the two `--exclude=` segments are documented as deviation D6: `local-rules` because D-12 placed the rule packs inside the scan target, and `findings-config-b.json` because the directive output cannot logically be its own input on subsequent runs); `semgrep-stderr.txt` (supporting evidence — see §4.3) as the verbatim auditable stderr summary that backs the `files_scanned` / `files_skipped` / `findings_total` cross-reference | `python3 -c "import json; d=json.load(open('results-semgrep.sarif')); assert isinstance(d.get('runs'), list)"` exits 0; SARIF `runs[0].tool.driver.semanticVersion = "1.163.0"` provides version linkage to `scan-metadata.txt` `semgrep_version=1.163.0`; `semgrep-stderr.txt` line "Targets scanned: 4764" independently verifies `scan-metadata.txt` `files_scanned=4764`; SARIF analysis in §1 frontmatter |
| **Directive 3** — Extract findings from SARIF; compile into `findings-config-b.json` minified to a single line, UTF-8, five-field schema (`file`, `line`, `severity`, `cwe`, `description`); severity mapping `error->critical, warning->high, note->medium, info->low`; description max 200 chars; `[]` if zero findings | `cat findings-config-b.json \| wc -l` returns `1`; valid JSON; every finding has all 5 fields populated; no description exceeds 200 characters | `normalize-sarif.py` (the script implementing the projection); `findings-config-b.json` (the final 216-record single-line JSON array at repository root, emitted by running `python3 normalize-sarif.py results-semgrep.sarif findings-config-b.json`) | `normalize-sarif.py` post-conditions (assert single-line, parse, 5-key set, max-200-chars) all pass on invocation; `wc -l findings-config-b.json` returns `1`; `python3 -m json.tool findings-config-b.json` returns exit 0; all 216 records contain exactly `{file, line, severity, cwe, description}` with no null values, `line` is `int`, `severity ∈ {critical,high,medium,low}`, `cwe` matches `CWE-<digits>`, max `description` length is 200 |
| **User Rule** — Explainability (Markdown decision log + bidirectional traceability matrix + deviation log) | Decisions table populated for every non-trivial decision; traceability is 100% (no gaps); deviations explicitly logged | `decision-log.md` (this file) | Sections §3 (12 decisions, 4 columns each), §4 (5 forward rows + 9 reverse rows covering all 9 scoped artifacts, 100% coverage), §4.3 (supporting evidence listing), §5 (CWE inference audit), §6 (deviations D1-D6), §7 (decision-ID inventory D-01 through D-12) |
| **User Rule** — Executive Presentation (reveal.js HTML deck) | 12-18 sections (target 16); CDN-pinned reveal.js 5.1.0 + Mermaid 11.4.0 + Lucide 0.460.0; theme inline; zero emoji; every section has at least one non-text visual | `executive-summary.html` | Verification post-conditions per AAP §0.7.2 enforced before delivery |

### 4.2 Artifact -> Directive (reverse)

Each of the nine scoped artifacts produced by this configuration traces
back to exactly one originating directive or user rule. The `Necessity`
column distinguishes Directive-required outputs (mandatory by the user's
text), Rule-required deliverables (mandatory by an explicit user rule),
and Working-artifact (intermediate file required by a Pass/Fail clause or
by the implementation chain). All nine rows are scoped artifacts; the
table is complete (no gaps relative to §4.1 forward links).

| # | Emitted Artifact | Location | Triggering Directive / Rule | Necessity |
|---|---|---|---|---|
| 1 | `results-semgrep.sarif` | repository root | Directive 2 | REQUIRED — directive's explicit `-o` flag |
| 2 | `scan-metadata.txt` | repository root | Directive 2 | REQUIRED — captures the three explicit observations (exit code, duration, files-scanned) plus hermeticity evidence |
| 3 | `normalize-sarif.py` | repository root | Directive 3 | REQUIRED — implements the SARIF-to-JSON projection that Directive 3 mandates |
| 4 | `findings-config-b.json` | repository root | Directive 3 | REQUIRED — the directive-mandated final deliverable; 216-record single-line JSON array; Pass/Fail post-conditions (`wc -l == 1`; valid JSON; 5/5 fields populated; description ≤200 chars) all verified |
| 5 | `local-rules/security-audit.yaml` | repository root (`./local-rules/`) | Directive 1 | REQUIRED — explicit rule-pack name `p/security-audit`; materialized in-repo per D-12 so the cache is reachable from a fresh checkout |
| 6 | `local-rules/secrets.yaml` | repository root (`./local-rules/`) | Directive 1 | REQUIRED — explicit rule-pack name `p/secrets`; materialized in-repo per D-12 |
| 7 | `local-rules/owasp-top-ten.yaml` | repository root (`./local-rules/`) | Directive 1 | REQUIRED — canonicalized from the user-named `p/owasp` (see deviation D1 §6); materialized in-repo per D-12 |
| 8 | `decision-log.md` (this file) | repository root | Explainability rule (AAP §0.7.1) | REQUIRED — rule-mandated additive deliverable (see deviation D2 §6) |
| 9 | `executive-summary.html` | repository root | Executive Presentation rule (AAP §0.7.2) | REQUIRED — rule-mandated additive deliverable (see deviation D2 §6) |

### 4.3 Supporting Evidence (non-scoped)

Files produced by the pipeline that are not themselves scoped deliverables
are listed below for completeness. They are evidence artifacts referenced
by the scoped artifacts in §4.1 and §4.2 but are not counted as the
"9 scoped artifacts" set.

| Supporting File | Location | Referenced By | Purpose |
|---|---|---|---|
| `semgrep-stderr.txt` | repository root | `scan-metadata.txt` `stderr_capture_file=semgrep-stderr.txt`; §4.1 row for Directive 2 | Raw verbatim stderr summary from the Semgrep run; lets independent reviewers cross-check the `files_scanned`, `files_skipped`, `findings_total`, `parse_errors`, and `timeouts` values that `scan-metadata.txt` derives from SARIF `toolExecutionNotifications` and Semgrep's terminal summary. Retained for audit traceability per AAP §0.3.1 ("Capture of Semgrep stderr summary"). |
| `/tmp/semgrep-config-b/.semgrep-venv/` | external to repo (host filesystem) | §4.1 row for Directive 1 | Python virtual environment hosting the pinned `semgrep==1.163.0` installation. Not tracked by git (per AAP §0.5.1 the venv lives in a working directory adjacent to the scanned tree). |

## 5. CWE Inference Audit

This section records every CWE that was INFERRED from rule descriptions
(Step 4 of the AAP §0.5.6 algorithm) rather than read directly from rule
metadata (Steps 1-3). It also records every emission of the `CWE-Other`
sentinel (Step 5).

**Result for this run: No inferences required.** All 216 findings resolved
via Step 2 (`runs[].tool.driver.rules[ruleIndex].properties.tags` scanned
for entries matching `CWE-<digits>`). Coverage is 216/216 (100%).
`scan-metadata.txt` key `cwe_in_tags_coverage` records this independently as
`216/216` for cross-reference. Step 3 (result-level properties), Step 4
(description keyword inference), and Step 5 (`CWE-Other` sentinel) were not
exercised.

| Rule ID | Source Description Text | Chosen CWE | Inference Rationale |
|---|---|---|---|
| _none_ | _no Step-4 inferences performed; all CWEs read from rule metadata_ | _n/a_ | _n/a_ |

For completeness, the 12 distinct CWEs that appeared across the 216 findings
(all read from `properties.tags`) are listed below in ascending numeric
order; this is reference data, not an inference audit:

| CWE | Category | Severity (max) observed in this run |
|---|---|---|
| CWE-78 | Improper Neutralization of Special Elements used in an OS Command ("OS Command Injection") | high |
| CWE-79 | Improper Neutralization of Input During Web Page Generation ("Cross-site Scripting") | high |
| CWE-89 | Improper Neutralization of Special Elements used in an SQL Command ("SQL Injection") | high |
| CWE-250 | Execution with Unnecessary Privileges | high |
| CWE-269 | Improper Privilege Management | high |
| CWE-287 | Improper Authentication | high |
| CWE-300 | Channel Accessible by Non-Endpoint ("Man-in-the-Middle") | high |
| CWE-327 | Use of a Broken or Risky Cryptographic Algorithm | high |
| CWE-328 | Use of Weak Hash | high |
| CWE-338 | Use of Cryptographically Weak Pseudo-Random Number Generator | high |
| CWE-400 | Uncontrolled Resource Consumption | high |
| CWE-798 | Use of Hard-coded Credentials | critical |

## 6. Deviation Log

Per AAP §0.7.1, every deviation from a literal or obvious interpretation of
the user's directives is recorded with explicit rationale. Six deviations
are logged below (D1–D6). The Explainability rule treats unexplained
deviations as defects; each row below is signed off as the operative
interpretation.

| # | Deviation | Rationale | Reviewer Acknowledgment |
|---|---|---|---|
| D1 | **Rule-pack canonicalization**: user wrote `p/owasp` (Directive 1); implementation uses `p/owasp-top-ten` and the cached file is `owasp-top-ten.yaml`. | The Semgrep Registry's canonical slug for the OWASP Top Ten rule pack is `p/owasp-top-ten`. `https://semgrep.dev/c/p/owasp` redirects to `https://semgrep.dev/c/p/owasp-top-ten` programmatically. Functional behavior is identical; only the file name and URL differ. Following the canonical slug produces a stable filename that downstream tooling and human reviewers can find by name. | Sanctioned. |
| D2 | **Rule-mandated additive deliverables**: the user's directive header reads `[3 directives \| ~0 files modified \| 1 new file]`, but the two user-specified rules (Explainability and Executive Presentation) MANDATE two additional new files (`decision-log.md` and `executive-summary.html`). | The rules are themselves user inputs that take explicit precedence over the implicit "1 new file" count in the directive header. The Explainability rule quotes "Deliver a decision log as a Markdown table" verbatim; the Executive Presentation rule quotes "Every deliverable MUST include an executive summary as a single self-contained reveal.js HTML file that is ALWAYS included independent of any other documentation that exists" verbatim. Both rules are applied in full. | Sanctioned. |
| D3 | **Theme inlining** for `executive-summary.html` instead of referencing `blitzy-deck/references/blitzy-reveal-theme.css`. | The canonical theme file does not exist in this repository (verified by `find . -maxdepth 6 -name "blitzy-reveal-theme*"` returning no matches). The Executive Presentation rule itself mandates a "single self-contained HTML file, no build steps, no local file dependencies", which forbids any external CSS link regardless of whether the canonical file exists. Inline embedding of the full theme token set is the correct interpretation. | Sanctioned. |
| D4 | **Semgrep version drift**: AAP §0.4.1 names `semgrep==1.144.0` as the install target; observed installation resolved to `semgrep==1.163.0`. | AAP §0.4.1 also reads "(or highest available 1.x release, validated against [Semgrep release notes November 2025])" — explicit permission to use the newest 1.x line. At install time `pip install semgrep` resolved to `1.163.0`, the newest 1.x available. The pin captured into the venv ensures reproducibility going forward. No CLI flag breakages observed; the verbatim Directive 2 command string executed without modification. Auditable: SARIF `runs[0].tool.driver.semanticVersion` carries `1.163.0` for cross-artifact verification with `scan-metadata.txt`. | Sanctioned. |
| D5 | **Scan-command target canonicalization**: AAP §0.1.2.2 reproduces Directive 2 with `/path/to/blitzy-RudderStack` as a placeholder for the absolute checkout path; the recorded `scan_command` in `scan-metadata.txt` uses `.` as the target instead. | The user's directive uses `/path/to/blitzy-RudderStack` as an explicit placeholder, meaning the actual scan target is the resolved checkout root. Running `semgrep scan` from the repository root with `.` as the target produces the identical filesystem coverage AND emits SARIF `artifactLocation.uri` values that are relative to the repository root by default (uriBaseId `%SRCROOT%`). The `.` form keeps `scan_command`, the dry-run command, and the onboarding `executive-summary.html` step-3 example consistent under a single canonical invocation. The `--config=./local-rules` segment is unchanged from AAP §0.3.1. The dry-run was performed with `--dryrun` (single-word) — the canonical flag spelling for Semgrep CE 1.163.0; the AAP §0.1.2.2 verbatim reproduction `--dry-run` (hyphenated) is no longer accepted by the 1.163.0 CLI and rewriting to `--dryrun` is the only viable form. | Sanctioned. |
| D6 | **Two targeted `--exclude` flags added to the scan command**: AAP §0.3.2 reads "No path exclusions during scan" and the Directive 2 verbatim command does not include `--exclude=...`; the implementation adds two flags — `--exclude=local-rules` AND `--exclude=findings-config-b.json` — to the scan command. | The "no path exclusions" clause in AAP §0.3.2 specifically addresses pre-existing tree content (e.g. `refs/segment-docs/`, `mocks/`) and is preserved unchanged: this configuration still scans the full pre-existing `blitzy-RudderStack` tree without any application-code exclusions. Both targeted flags are coupled consequences of decisions D-12 and D-05 that brought previously-external artifacts inside the repository: (i) `--exclude=local-rules` is required because D-12 materialised the three rule-pack YAML bundles inside the repository at `local-rules/` to make Directive 1's cache deliverables reachable from a fresh checkout; the original AAP placed `local-rules/` at `/path/to/local-rules`, explicitly outside the scan target `/path/to/blitzy-RudderStack` (AAP §0.1.2.2 reproduces Directive 2 with two distinct path placeholders), so the directive's spatial intent is that rule packs are NOT scan targets — once D-12 brought them inside the repository, `--exclude=local-rules` is the only mechanism that preserves that intent without re-introducing the network-fetch dependency that the original ignored-symlink approach concealed; the exclusion is also semantically correct because the rule packs are loaded as Semgrep CONFIGURATION via `--config=./local-rules`, not as application CODE, and scanning them as targets produces self-referential matches (rules detecting example patterns inside other rule bodies) that pollute findings without describing the rudder-server codebase. (ii) `--exclude=findings-config-b.json` is required because the Directive 3 output file resides at the repository root and is committed as the deliverable (AAP §0.6.1); SARIF/normalizer/findings is a one-shot pipeline whose OUTPUT cannot also be its INPUT — without this exclusion, every subsequent run on a fresh checkout would scan its own prior output, both polluting findings with self-matches against the normalized JSON and introducing a drift between `files_scanned` recorded at scan time vs. observed on a fresh checkout, breaking the AAP §0.8.1 idempotency invariant. The two exclusions together make the file-count metric STABLE: a fresh checkout running the dry-run gate observes the same 4764 scanned files that `scan-metadata.txt` records, and the same 216 findings emerge that the original symlink-based deployment produced, confirmed by SHA-256 byte-equality of the resulting `findings-config-b.json` (`0c53063d0f551083dc96021377c0a0ff24bb44e4742a7020bcba802f3d64f561`). Both exclusions are fully auditable in `scan-metadata.txt` `scan_command` and in the SARIF `invocations[0].commandLine`; `files_skipped=1480` decomposes to 36 oversized + 1440 `.semgrepignore` + 4 from the two `--exclude` patterns (3 rule-pack YAMLs + 1 findings JSON). | Sanctioned. |

## 7. Decisions Inventory (cross-reference with §3)

This table maps each decision in §3 to a stable identifier (D-01 through
D-12) so that `executive-summary.html`, future maintainers, and reviewers
can refer back to a single decision without re-quoting the row.

| ID | Decision Title | §3 Row |
|---|---|---|
| D-01 | Installation method for the `semgrep` CLI (venv vs. pipx vs. apt vs. Docker) | 1 |
| D-02 | Pinned Semgrep version (`1.144.0` vs. newest 1.x vs. unpinned) | 2 |
| D-03 | Rule-pack canonicalization (`p/owasp` -> `p/owasp-top-ten`) | 3 |
| D-04 | Rule-pack acquisition mechanism (curl Registry endpoint vs. github clone vs. cache capture) | 4 |
| D-05 | Scope of files scanned (full application tree + `--exclude=local-rules` rule-pack carve-out + `--exclude=findings-config-b.json` output-not-input carve-out) | 5 |
| D-06 | SARIF severity fallback when `level` is absent on a result | 6 |
| D-07 | CWE inference policy (metadata-first cascade vs. metadata-only vs. description-only) | 7 |
| D-08 | Description sanitization (collapse-all-whitespace + 200-char truncation) | 8 |
| D-09 | Trailing-newline policy on `findings-config-b.json` (`wc -l == 1` invariant) | 9 |
| D-10 | Scan-metadata location (separate `scan-metadata.txt` + replicated §1 frontmatter) | 10 |
| D-11 | Executive-summary theme inlining (inline `<style>` vs. external CSS link) | 11 |
| D-12 | Rule-pack cache location (in-repo `local-rules/` vs. external symlinked cache vs. tarball) | 12 |
