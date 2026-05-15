# Config A Baseline Security Audit · Decision Log

> Companion to `findings-config-a.json` (repository root) and `blitzy-audit/config-a-executive-summary.html`.
> Native agent analysis only — no external scanners invoked. Read-only at source, additive-only at artifact level.

## 1. Audit Methodology

### 1.1 Scope Statement

This is the **Config A — Bare Blitzy Baseline** arm of a multi-config security tool comparison. The audit is a read-only static + dataflow + dependency analysis of the `rudder-server` Go monorepo (module `github.com/rudderlabs/rudder-server`, Go toolchain `1.26.1`). No source files are modified. No CI workflows are added. No dependencies are added, updated, or removed. The audit produces three NEW artifacts: `findings-config-a.json` (machine-readable findings, repository root), this decision log at `blitzy-audit/config-a-decision-log.md`, and the reveal.js executive summary at `blitzy-audit/config-a-executive-summary.html`. The audit's primary contract is the JSON findings file; the decision log and executive deck are mandated by the Explainability and Executive Presentation rules respectively, both of which apply independently of the user's "1 new file" wording.

### 1.2 Exclusion List

Every one of the following is OUT OF SCOPE for Config A. No tool listed below has been invoked during this audit:

- Snyk
- Semgrep
- CodeQL
- Trivy
- gitleaks
- govulncheck
- npm-audit
- retire.js
- OWASP Dependency-Check
- Bandit
- Direct `gosec` invocation outside the existing `golangci-lint` run (gosec is referenced ONLY as a baseline-control note in section 1.5; it is not invoked here)
- Exploit frameworks (Metasploit, sqlmap, and equivalents)
- Fuzzers (go-fuzz, AFL, libFuzzer, and equivalents)
- Dynamic Application Security Testing (DAST) tools
- Any other vulnerability scanner or SCA tool not explicitly enabled in `.golangci.yml`

The project's `go.mod` already contains a `replace` block overriding several Snyk-flagged indirect dependencies (`cyphar/filepath-securejoin`, `gin-gonic/gin`, `go-jose`, and others), which confirms Snyk is the codebase's habitual scanner. Config A measures what an unaided agent finds **without** Snyk — or any other scanner — as the baseline for comparing against scanner-assisted configs (Config B, Config C, and downstream).

### 1.3 Domain Inventory

Eleven security-relevant domains were analyzed. Each was inspected as REFERENCE material for the audit:

- **HTTP ingress** — `gateway/handle_http*.go`, `gateway/handle_webhook.go`, `gateway/handle_http_auth.go`. Internet-facing event ingestion on port 8080; write-key and source-ID authentication; webhook payloads from untrusted third parties.
- **Outbound delivery** — `router/network.go`, `router/worker.go`, `router/handle.go`, `router/batchrouter/`, `router/customdestinationmanager/`. Tenant-configured destination egress; SSRF surface; private-IP guard live here.
- **Persistent storage** — `jobsdb/*.go`, `sql/migrations/*.sql`. PostgreSQL JobsDB; 100 SQL migrations; SQL-injection surface for dynamic statements.
- **Configuration** — `config/config.yaml`, `config/sample.env`, `build/docker.env`. Default credentials; TLS posture; secrets exposure.
- **Auth and control-plane** — `services/oauth/`, `services/controlplane/`, `services/validators/`, `backend-config/`. OAuth flows for destinations; workspace token handling; gRPC client to the Control Plane.
- **Admin / RPC** — `admin/admin.go`. UNIX-socket RPC server at `/tmp/rudder-server.sock`; exposes `SetLogLevel` and `GetLoggingConfig`; file-permission posture of the socket.
- **Internal subsystems** — `internal/drain-config/`, `internal/enricher/`, `internal/pulsar/`, `internal/transformer-client/`. Drain-config HTTP `PUT`, MaxMind DB S3 download, Pulsar messaging, transformer HTTP client.
- **Enterprise features** — `enterprise/config-env/`, `enterprise/reporting/`, `enterprise/suppress-user/`, `enterprise/trackedusers/`. Elastic License 2.0 modules; env-var substitution; error normalization.
- **Warehouse subsystem** — `warehouse/` (admin, api, archive, backfill, bcm, client, constraints, encoding, healthmonitor, integrations, internal, multitenant, replay, router, safeguard, schema, selectivesync, slave, source, validations, identity). Largest subsystem; multi-tenant warehouse loaders; destination credentials handling.
- **Build and deployment** — `Dockerfile`, `docker-compose.yml`, `rudder-docker.yml`, `build/docker-entrypoint.sh`, `build/docker-go-version.sh`, `build/nginx.backend.conf`, `build/nginx.transformer.conf`, `scripts/start_server.sh`. Container hardening posture; entrypoint privilege; NGINX reverse-proxy configuration.
- **CI / supply chain** — `.github/workflows/*` (13 workflows), `.github/dependabot.yml`, `.golangci.yml`. Workflow token permissions; action SHA pinning; lint coverage; Dependabot ecosystem scope.

### 1.4 Analysis Lens Catalog

Each domain above was analyzed through the following ten lenses. Findings produced by any lens map to a single most-specific CWE per the policy in section 3.

| Lens | Representative CWEs | What we look for |
|---|---|---|
| Injection | CWE-79, CWE-89, CWE-78, CWE-94, CWE-77, CWE-91, CWE-917 | Unsafe string concatenation into SQL; shell exec with untrusted input; template rendering of user data; header injection. |
| AuthN / AuthZ | CWE-287, CWE-862, CWE-863, CWE-284, CWE-306, CWE-798 | Missing auth on admin/RPC endpoints; predictable tokens; hardcoded credentials; role bypass on enterprise paths. |
| Cryptography | CWE-327, CWE-326, CWE-330, CWE-338, CWE-295, CWE-319, CWE-916 | Weak ciphers/hashes (MD5/SHA1 for auth); `math/rand` for security; `InsecureSkipVerify`; plaintext transport. |
| SSRF / egress | CWE-918 | Outbound HTTP whose target URL derives from untrusted input; bypass of private-IP filters. |
| Path traversal / SSTI | CWE-22, CWE-23, CWE-35, CWE-73 | File operations with attacker-controlled paths; template execution of untrusted strings. |
| Resource exhaustion | CWE-400, CWE-770, CWE-834, CWE-674, CWE-409 | Unbounded `io.ReadAll`; missing body-size caps; missing timeouts; unbounded goroutines; gzip-bomb amplification. |
| Secrets exposure | CWE-798, CWE-256, CWE-532, CWE-312 | Hardcoded credentials; secrets logged at debug; plaintext on disk; credentials in error responses. |
| Dependency vulnerabilities | CWE-1104, CWE-1395, CWE-937 | Outdated packages with known CVEs; missing `replace` for known-bad versions; deprecated runtimes. |
| Misconfiguration | CWE-732, CWE-16, CWE-250, CWE-276, CWE-1004, CWE-1392, CWE-693 | Overly permissive container settings; running as root; missing security headers; default credentials. |
| Error handling / info leak | CWE-209, CWE-200, CWE-703, CWE-754 | Stack traces in responses; unhandled errors in security-relevant paths; verbose 5xx bodies. |

### 1.5 Existing Security Controls (Baseline Context — Not Findings)

The repository already carries several defensive controls. These are NOT findings; they shape the audit's expected hit rate and explain why this audit concentrates on dataflow-, configuration-, and design-level issues rather than AST-detectable bugs:

- `gosec` is enabled via `golangci-lint` in `.golangci.yml`. Classic AST-detectable Go security bugs are already filtered out in CI.
- `depguard` denies `github.com/gofrs/uuid`, `golang.org/x/exp/slices`, `github.com/json-iterator/go`, `github.com/rudderlabs/sonnet`, and `github.com/aws/aws-sdk-go` v1, forcing migration to vetted alternatives.
- `forbidigo` blocks direct use of `json.Marshal` / `json.Unmarshal` / `json.NewDecoder` / `json.NewEncoder` (callers must use the `rudder-go-kit/jsonrs` wrapper), the sugared `Logger.Debug` / `Info` / `Warn` / `Error` / `Fatal` legacy methods, and `cenkalti/backoff` v1–v4.
- The `go.mod` `replace` block overrides versions of `cyphar/filepath-securejoin`, `gin-gonic/gin`, `go-jose/go-jose/v3`, and other indirect dependencies that Snyk has previously flagged.
- `.github/dependabot.yml` configures daily update PRs for `gomod`, `github-actions`, and `docker` ecosystems.
- The `Dockerfile` SHA-pins its `golang:1.26.1-alpine3.23` and `alpine:3.23` base images, applies `apk --no-cache upgrade`, and switches to an unprivileged `rudder` user before runtime.
- `SECURITY.md` defines a responsible-disclosure channel (`security@rudderstack.com`) and a supported-version policy.

Because the AST-level gate is already in place, this audit emphasizes dataflow-, configuration-, and design-level issues that AST linting does not catch.

---

## 2. Severity Rubric

The user input mandates the four literal values `critical | high | medium | low` but does NOT define their boundaries. The rubric below defines them for this audit and is applied uniformly across all findings in `findings-config-a.json`.

| Severity | Definition | Examples in scope |
|---|---|---|
| **critical** | Pre-authentication RCE; production secret committed to repository; broken auth on internet-exposed endpoint; full data exfiltration path with no compensating control. | OS command injection in the regulation-worker GDPR delete pipeline (F-001). |
| **high** | Auth bypass on enterprise/protected path; SQL injection in dynamic statement; SSRF to internal services; missing TLS verification; insecure deserialization. | Unparameterized SQL in JobsDB read path (F-007); `InsecureSkipVerify: true` in Google Sheets destination test path (F-006); SSRF private-IP default off (F-005); unbounded gzip decompression (F-004); unauthenticated drain-config endpoint (F-003); unbounded request-body buffer (F-002). |
| **medium** | Information leak through error responses; missing rate limit on auth endpoint; weak randomness for non-security purpose; dependency CVE with available workaround; default credentials in shipped config. | Default `JOBS_DB_PASSWORD=password` (F-009); `JOBS_DB_SSL_MODE=disable` (F-010, F-011); `ALLOW_NONE_AUTHENTICATION=yes` for etcd (F-013); `MINIO_ROOT_PASSWORD=password` (F-012); stack trace leaked via `http.Error(w, err.Error(), 500)` (F-015); workspace-config logged on parse failure (F-014); processor `ReadTimeout=0` (F-008). |
| **low** | Hardening recommendation; missing security header; deprecated cipher allowance; defense-in-depth gap; verbose logging of non-sensitive metadata; secure-by-default miss in demo asset. | UNIX socket created without explicit `chmod` (F-016); unquoted `$RUDDER_TMPDIR` in entrypoint (F-017); NGINX `error_log debug` (F-018); missing security response headers in NGINX (F-019); `POSTGRES_PASSWORD=password` in demo compose (F-020). |

The boundaries above are derived from CVSS thinking — Confidentiality / Integrity / Availability impact × Exploitability × Production reachability — but COMPRESSED to four discrete buckets to honor the user input's literal severity values. The full numeric CVSS scale is intentionally not used; the four labels above are exhaustive.

---

## 3. CWE Selection Policy

Every CWE assigned to a finding in `findings-config-a.json` is governed by this policy, which translates the user directive "the most specific CWE you are confident about" into operational rules:

- **Leaf-CWE preference.** Choose the most specific CWE the evidence supports. Prefer CWE-89 (SQL Injection) over CWE-707 (Improper Neutralization); prefer CWE-918 (SSRF) over CWE-20 (Improper Input Validation); prefer CWE-862 (Missing Authorization) over CWE-284 (Improper Access Control).
- **Evidence-bound assignment.** Assign a CWE only when source, configuration, or manifest evidence supports the call. Do not assign on speculation.
- **One CWE per record.** Where multiple CWEs apply, pick the one most aligned with the proximal cause; the alternative goes into the considered-but-not-flagged log in section 6 if it is informative.
- **Format validation.** Every `cwe` value matches `^CWE-\d+$`. The integer is a real MITRE-assigned identifier; we do not invent IDs.
- **Confidence threshold.** A finding may exist without a CWE only in section 6 (Considered but Not Flagged). Once promoted to `findings-config-a.json`, a CWE is mandatory.

---

## 4. Decision Log

The twelve decisions below are the binding methodology choices of this audit. They are referenced by Decision ID elsewhere in this document and by the executive summary deck. Decisions D-001 through D-009 cover the original audit methodology and the Checkpoint 2 reconciliations (JSON byte-boundary contract and Mermaid lifecycle). Decisions D-010 through D-012 cover the Checkpoint 3 visual-fidelity resolutions for the executive summary deck.

| Decision ID | What Was Decided | Alternatives Considered | Why This Choice | Risks |
|---|---|---|---|---|
| D-001 | Define a 4-bucket severity rubric keyed on exploitability + blast radius. | CVSS v3.1 numeric scores; exploitability-only scale; scanner default severities. | User input mandates the four literal values `critical \| high \| medium \| low` but does not define them; an explicit rubric is required for consistency across findings. | Different graders may still disagree on boundary cases; the rubric narrows but does not eliminate this. |
| D-002 | Prefer leaf CWE over umbrella IDs. | Always assign Top 25; always assign closest Top 25 parent. | "Most specific CWE you are confident about" maps directly to leaf preference. | Some leaves are obscure; downstream consumers may need a parent CWE for aggregation. Mitigated by recording parent-class rollups in this log. |
| D-003 | Expand deliverable set from 1 file to 3 files. | Produce only `findings-config-a.json`. | The Explainability rule MANDATES a decision log Markdown table. The Executive Presentation rule MANDATES a self-contained reveal.js HTML summary. Both rules are authoritative and apply independently of the user's "1 new file" wording. | Reviewer may not expect the extras; mitigated by explicit AAP documentation in §0.3.3 and this row. |
| D-004 | Place audit artifacts under `blitzy-audit/`. | Repo root; `docs/`; existing `blitzy/`. | A dedicated folder isolates audit-comparative-study artifacts from product source and avoids collision with `blitzy/documentation/` content which describes a separate program (the 25-epic delivery initiative). | New folder must be created; mitigated by mkdir semantics during artifact write. |
| D-005 | Disallow Snyk / Semgrep / CodeQL / direct `gosec` invocation / DAST. | Run scanners and treat output as evidence. | Directive 1 mandates native agent analysis only; Config A is the baseline arm of a comparative study designed to measure agent-only recall. | Lower recall than scanner-assisted configs; that is the experimental point of Config A. Mitigated by subsequent Config B / Config C runs. |
| D-006 | Embed Blitzy reveal.js theme CSS inline in the HTML artifact. | Link to `blitzy-deck/references/blitzy-reveal-theme.css`. | That reference file does NOT exist in this repository. Inline CSS keeps the deliverable self-contained per the Executive Presentation rule's "single self-contained HTML file" requirement. | Inline CSS is longer and not DRY; mitigated by the single-file delivery requirement. |
| D-007 | Anchor non-line findings (dependency, config-file-level) to the representative declaration line. | `line: 0`; `line: null`; range like `"line": "12-13"`. | The JSON contract requires `line` as a positive integer; the declaration line is the closest legitimate anchor. | Reviewer may interpret `line` as exact locus rather than anchor; mitigated by description wording that names the construct (e.g., "JOBS_DB_PASSWORD=password ships as the default credential…"). |
| D-008 | Serialize `findings-config-a.json` with a **single trailing newline byte** (POSIX line termination), preserving zero internal newlines within the JSON content. The file is the minified JSON array followed by exactly one `\n`; final byte layout is `…]\n`. | (a) Zero trailing newline at all (prior state at commit `d640e1d`; caused POSIX `wc -l` to return `0` and failed the literal user Directive 2 pass/fail probe `cat findings-config-a.json \| wc -l` returns `1`); (b) Multiple trailing newlines or internal pretty-printing whitespace (violates "minified to a single line, no pretty-printing"); (c) Trailing `\r\n` (CRLF) for Windows compatibility (introduces a non-LF byte; inconsistent with the project's Unix-native posture and Go `bufio.ScanLines`). | User Directive 2 contains two clauses that appear inconsistent under naïve reading: (i) "minified to a single line, no pretty-printing, no newlines" and (ii) the pass/fail probe `cat findings-config-a.json \| wc -l` returns `1`. The reconciliation is rooted in POSIX text-file semantics: `wc -l` reports the number of *completely-terminated* lines, which equals the number of `\n` bytes; a file with exactly one trailing newline has one terminated line and `wc -l` returns `1`. The trailing newline is therefore line *termination*, not content. The "no newlines" clause is correctly read as forbidding *internal* newlines within the JSON content (no pretty-printing whitespace). With the single trailing newline the byte-boundary contract is: `raw[:1] == b'['`, `raw[-2:-1] == b']'`, `raw[-1:] == b'\n'`, and `b'\n' not in raw[:-1]`. Both Directive 2 clauses are satisfied simultaneously: the JSON payload is single-line and minified with zero internal newlines, while the file is a well-formed POSIX text file with `wc -l = 1`. This reverses the prior decision (commits `33127e5` → `d640e1d`) and restores the state at `33127e5` with documented justification. The empty-result form, if produced, follows the same rule: `[]\n` (three bytes) — `wc -l` returns `1` consistently. | Some JSON parsers from the early 2000s rejected trailing whitespace; modern parsers (Go `encoding/json`, Python `json`, JavaScript `JSON.parse`) all accept JSON followed by whitespace per RFC 8259 §2 ("Insignificant whitespace is allowed before or after any of the six structural characters"). Mitigated by the universal modern-parser behavior. The earlier AAP §0.3.4 author's note "no trailing newline so that `wc -l` returns `1`" carried the inverse-POSIX misconception; this decision supersedes that note. |
| D-009 | Use a minimal DOM-source normalization step before calling the mandated `mermaid.run({ querySelector: '.reveal pre.mermaid' })`. | (a) Implement a fully custom render loop calling `mermaid.render()` per diagram (prior state, deviated from AAP §0.3.6 lifecycle mandate); (b) call `mermaid.run()` directly without normalization (would lose `<br/>` line breaks because the browser parses literal `<br/>` inside `<pre>` as `<br>` DOM elements that are then stripped from `textContent`); (c) replace `<br/>` in Mermaid sources with newline characters (would break Mermaid's outer line-by-line parser since flowcharts use real newlines as statement separators); (d) replace `<br/>` with HTML entities like `&lt;br/&gt;` (would re-introduce HTML entities into Mermaid blocks, which the checkpoint forbids). | AAP §0.3.6 and the Executive Presentation rule both mandate `mermaid.run({ querySelector: '.reveal pre.mermaid' })` after `ready` and after every `slidechanged`. Mermaid 11.x reads diagram source via `Element.textContent`, which drops `<br>` element nodes. The browser parses literal `<br/>` inside a `<pre>` as `<br>` DOM elements. The minimal-risk solution is therefore to walk each `<pre class="mermaid">` once before calling `mermaid.run()`, replace the parsed `<br>` elements with the literal text `<br/>` in `textContent`, then invoke the mandated `mermaid.run()` call. This preserves both the Mermaid lifecycle contract and the raw-Mermaid-syntax line-break behavior. | Future Mermaid major versions may change source-extraction semantics; mitigated by the pinned CDN version `mermaid@11.4.0`. The normalization is idempotent (gated on a `data-mm-normalized` attribute) so repeat invocations on `slidechanged` are safe. |
| D-010 | Compact Slide 8 (Top Weakness Classes Observed) by tightening `.audit-table.audit-table-cwe` row geometry — font-size 17px, padding 9px 20px, line-height 1.35; count column font-size 20px line-height 1.1; `.cwe-bar` height 12px border-radius 6px — so all 15 CWE rows fit within the AAP-mandated 1080px slide canvas. | (a) Truncate the table to the top 13 CWE entries (lost CWE-88 and CWE-693 from the executive view — silently downgraded the deliverable); (b) enable per-slide vertical scroll on reveal.js (would violate the "every slide a single static composition" expectation and break the 1080px canvas contract); (c) shrink the table header overhead (header overhead is shared with other slides — would alter the design system everywhere); (d) move some rows to a follow-up slide (changes deck slide count, violates the 16-slide structure documented in AAP §0.3.6). | The QA report (Checkpoint 3 Issue 1) measured `lastRow.bottom = 1206px > viewport 1080px` at 1920×1080, clipping CWE-88 and CWE-693 entirely. The AAP describes "PIXEL-PERFECT VISUAL FIDELITY" with "Zero tolerance for deviation" from the 1920×1080 slide canvas. The chosen approach localizes the compaction to only the CWE-bar table (`.audit-table-cwe` modifier), preserving the design system on every other content slide. Post-fix runtime measurement: `lastRow.bottom = 920.6px` (within section bottom 1058.4px and viewport 1080px) at 1920×1080, and 613.7px ≤ 720px at 1280×720, so the fix scales across viewports. | Slightly tighter row spacing may marginally reduce readability for users with low-vision needs viewing the deck at small zoom levels; mitigated because the body text remains at 17px (well above the WCAG 16px baseline) and the COUNT column maintains its visual emphasis at 20px. |
| D-011 | Apply `overflow: visible` to `.reveal pre.mermaid foreignObject` and `padding-right: 8px` to `.reveal pre.mermaid foreignObject .nodeLabel` ONLY (not `.edgeLabel`), to absorb the Mermaid v11.4.0 sub-pixel measurement quirk that clips trailing glyphs against the foreignObject's right edge. | (a) Apply padding to both `.nodeLabel` AND `.edgeLabel` (initial attempt — caused a regression where 7 empty edgeLabel placeholders on slide 3 rendered as small purple ghost rectangles because their `.labelBkg` background expanded to fit the artificially-padded span; reverted); (b) shorten node labels (e.g., "BQ · Snowflake" instead of "BQ · Snowflake · Redshift") to eliminate the overflow at the content level (loses information communicated by the audit); (c) downgrade to a smaller Mermaid font-size in `themeVariables.fontSize` (would change the deck-wide visual rhythm and is not honored uniformly across Mermaid node shapes); (d) document the clipping as a known library limitation and ship as-is (the AAP describes "PIXEL-PERFECT VISUAL FIDELITY" — known-library-limitation is not a sanctioned escape). | The QA report (Checkpoint 3 Issue 2) measured 5–11px label overflow on slides 3, 6, and 12 against the foreignObject containers Mermaid generated. Mermaid v11.4.0 sizes its node `<rect>` and `<path>` shape elements with enough internal whitespace (30+ px slack on most nodes) that the trailing glyphs only need a small horizontal allowance to render fully inside the visible shape boundary. `overflow: visible` lifts the foreignObject's clipping, and `padding-right: 8px` on the nodeLabel pushes content into that pre-existing slack. Restricting the rule to `.nodeLabel` ensures empty `.edgeLabel` placeholders (which Mermaid emits for every edge regardless of whether the edge carries a text label) remain invisible 0-width elements. Post-fix runtime measurement: every node label on slides 3, 6, 12 has `textInsideShape = true` with slack 0.2px–37.8px. | (1) On the trapezoidal "Data Warehouses" node on slide 3, the trailing "t" in "Redshift" lands 0.2px past the path boundary — visually imperceptible. (2) If a future Mermaid edge label is empty but uses `.nodeLabel` instead of `.edgeLabel` for any reason, it would gain visible 8px padding; mitigated by the CDN version pin to `mermaid@11.4.0`. |
| D-012 | Apply Blitzy-palette overrides to the reveal.js framework chrome — `.reveal .progress` (background `rgba(217,217,217,0.4)` derived from `--blitzy-border` #D9D9D9, color `var(--blitzy-primary)`, span background `var(--gradient-accent-bar)`), `.reveal .speaker-notes` (color `var(--blitzy-text)`, background `var(--blitzy-surface-0)`, font `var(--ff-body)`), `.reveal .resume-button` (color & border `var(--blitzy-text-muted)`, hover state `var(--blitzy-primary)`). | (a) Leave the framework chrome at reveal.js defaults (progress bar grey rgba(212,212,212,0.4), speaker-notes #222222, resume-button #CCCCCC — off the Blitzy palette and flagged by QA as a LOW hardening item); (b) hide the framework chrome entirely (`display: none` on `.progress`, `.controls`, etc. — would remove navigation affordances and break keyboard accessibility); (c) restyle every framework element to match a different palette (would break the Blitzy brand identity contract). | The QA report (Checkpoint 3 Issue 3) flagged reveal.js default chrome colors (#D4D4D4 progress bar, #222222 speaker-notes, #CCCCCC resume-button) as off-palette LOW-severity hardening items. The chosen overrides use exclusively Blitzy palette tokens (no hardcoded hex outside the singular rgba(217,217,217,0.4) which is the rgba expansion of the existing `--blitzy-border` token #D9D9D9). The `.progress span` now uses `var(--gradient-accent-bar)` so the progress fill matches the title-slide accent bar gradient and the Slide 16 accent bar — three visually-related elements unified on one token. | The progress-bar background token had to be expressed as `rgba(217,217,217,0.4)` literal because CSS lacks a one-liner for "this CSS variable, but with alpha = 0.4." Mitigated by the inline comment naming the source token `--blitzy-border #D9D9D9`, so future palette changes can be propagated by hand. |

Additional finding-specific decisions are encoded by the `severity` and `cwe` columns of section 5 below; the rationale is the analysis lens for that finding.

---


## 5. Bidirectional Traceability Matrix

This matrix maps every finding in `findings-config-a.json` to its source location, CWE, severity, the analysis lens that produced it, and a brief justification. 100% coverage with no gaps — there are 20 rows for 20 findings, and every row corresponds to exactly one record in the JSON file.

| Finding ID | File | Line | Severity | CWE | Analysis Lens | Brief Justification |
|---|---|---|---|---|---|---|
| F-001 | `regulation-worker/internal/delete/batch/filehandler/gzip.go` | 85 | critical | CWE-78 | Injection | `exec.CommandContext("bash","-c", fmt.Sprintf(...))` interpolates `regexp.QuoteMeta`-only-escaped user `attribute.ID`; regex-escaping does not neutralize shell metacharacters or single-quote breakout. |
| F-002 | `gateway/handle.go` | 677 | high | CWE-770 | Resource exhaustion | `io.ReadAll(r.Body)` without `http.MaxBytesReader` wrapper; the size check at line 494 fires only after the body is already fully buffered. |
| F-003 | `internal/drain-config/http.go` | 12 | high | CWE-306 | AuthN / AuthZ | `srvMux.Put("/job/{job_run_id}", d.drainJob)` mounts a state-mutating endpoint with no authentication middleware. |
| F-004 | `middleware/uncompress.go` | 13 | high | CWE-409 | Resource exhaustion | `r.Body = &gzipReader{body: r.Body}` followed by `gzip.NewReader` has no decompressed-size cap, enabling gzip-bomb amplification. |
| F-005 | `router/network.go` | 301 | high | CWE-918 | SSRF / egress | `network.blockPrivateIPs = getRouterConfigBool("blockPrivateIPs", network.destType, false)` defaults the SSRF guard to OFF per destination type. |
| F-006 | `services/streammanager/googlesheets/googlesheetsmanager.go` | 308 | high | CWE-295 | Cryptography | `InsecureSkipVerify: true` inside a `tls.Config` literal in `testClientOptions`; reachable in production via tenant-supplied `TestConfig.Endpoint`. |
| F-007 | `warehouse/identity/identity.go` | 297 | high | CWE-89 | Injection | `fmt.Sprintf("merge_property_type='%s' AND merge_property_value=%s", prop.Type, ...)` interpolates `prop.Type` without `pq.QuoteLiteral`; `prop.Value` is correctly quoted, `prop.Type` is not. |
| F-008 | `app/apphandlers/processorAppHandler.go` | 68 | medium | CWE-400 | Resource exhaustion | `ReadTimeout` and `ReadHeaderTimeout` default to `0` for the processor HTTP server config — unlimited slow-read budget. |
| F-009 | `build/docker.env` | 12 | medium | CWE-1392 | Misconfiguration | `JOBS_DB_PASSWORD=password` ships as the default credential in the canonical container env file. |
| F-010 | `build/docker.env` | 13 | medium | CWE-319 | Cryptography | `JOBS_DB_SSL_MODE=disable` forces unencrypted PostgreSQL traffic between rudder-server and JobsDB. |
| F-011 | `config/sample.env` | 7 | medium | CWE-319 | Cryptography | `JOBS_DB_SSL_MODE=disable` in the published sample env propagates into typical deployments. |
| F-012 | `docker-compose.yml` | 42 | medium | CWE-1392 | Misconfiguration | `- MINIO_ROOT_PASSWORD=password` is a trivially-guessable default for the storage profile. |
| F-013 | `docker-compose.yml` | 49 | medium | CWE-306 | AuthN / AuthZ | `- ALLOW_NONE_AUTHENTICATION=yes` disables etcd auth entirely in the multi-tenant profile. |
| F-014 | `enterprise/config-env/configEnv.go` | 35 | medium | CWE-532 | Secrets exposure | `logger.NewStringField("workspaceConfig", string(workspaceConfig))` logs the full workspace config payload — including destination secrets — on JSON parse error. |
| F-015 | `internal/drain-config/http.go` | 25 | medium | CWE-209 | Error handling / info leak | `http.Error(w, err.Error(), http.StatusInternalServerError)` returns the raw PostgreSQL driver error to the HTTP client. |
| F-016 | `admin/admin.go` | 114 | low | CWE-732 | Misconfiguration | UNIX socket created at `/tmp/rudder-server.sock` via `net.Listen("unix", ...)` with no explicit `os.Chmod` to restrict permissions. |
| F-017 | `build/docker-entrypoint.sh` | 16 | low | CWE-88 | Injection | `mkdir -p $RUDDER_TMPDIR 2>/dev/null` uses unquoted variable expansion; shell word-splitting can cause argument injection. |
| F-018 | `build/nginx.backend.conf` | 1 | low | CWE-532 | Secrets exposure | `error_log /var/log/nginx/error.log debug;` increases the risk of sensitive payload fragments reaching disk logs. |
| F-019 | `build/nginx.backend.conf` | 12 | low | CWE-693 | Misconfiguration | The `server { }` block defines no `add_header Strict-Transport-Security` / `X-Content-Type-Options` / `X-Frame-Options` / `Content-Security-Policy` directives. |
| F-020 | `rudder-docker.yml` | 8 | low | CWE-1392 | Misconfiguration | `- POSTGRES_PASSWORD=password` is a weak default credential in the demo compose template. |

**Coverage statement.** Every finding in `findings-config-a.json` has a row here; every row here references a real finding in the JSON. Bidirectional traceability is 100% — no gaps.

**Severity totals (sanity check).** 1 critical + 6 high + 8 medium + 5 low = 20.

**Top CWE classes.** CWE-1392 (3), CWE-319 (2), CWE-306 (2), CWE-532 (2), plus 11 singletons (CWE-78, CWE-770, CWE-409, CWE-918, CWE-295, CWE-89, CWE-400, CWE-209, CWE-732, CWE-88, CWE-693).

---


## 6. Considered but Not Flagged

Observations evaluated during the audit but rejected from `findings-config-a.json` due to insufficient evidence, false-positive characterization, or compensating controls. These are recorded here for transparency and for downstream configurations (Config B, Config C, and beyond) to revisit. Entries here are NOT findings; they are the audit's audit trail of negative results.

| Locator | Observation | Reason Not Flagged |
|---|---|---|
| `utils/misc/misc.go:116` | MD5 hash usage. | Annotated `// skipcq: GO-S1023`; used for non-cryptographic partitioning/keying, not authentication or integrity protection. The MD5 collision class does not produce privilege escalation in this context. |
| `proto/event-schema/types.helper.go:52` | MD5 hash usage. | Schema content hashing — non-security use; collisions on user-controlled input do not yield privilege gain or authentication bypass. |
| `services/kvstoremanager/redis/redis.go:65` | `InsecureSkipVerify=true` set on Redis TLS dialer. | Gated behind a tenant `skipVerify` boolean config field; opt-in misconfiguration rather than a hardcoded vulnerability. Recorded for future hardening; current evidence does not support a confident finding because the field is tenant-controlled, not server-controlled. |
| `warehouse/identity/identity.go:340` | `UPDATE %s SET rudder_id='%s' ...` with `newRudderID` interpolated via `fmt.Sprintf`. | `newRudderID` is a server-generated UUID, so not currently exploitable; flagged for future hardening but not in JSON because evidence is insufficient for a confident high-severity SQL-injection call without taint tracing. The companion finding F-007 covers the analogous unsafe interpolation that IS reachable from tenant input. |
| `.github/workflows/*` (all 13) | CI workflow security posture. | All 13 workflows declare explicit `permissions:` blocks AND SHA-pin third-party actions (notably `step-security/harden-runner`); no concrete finding emitted under this configuration. |
| Memory-safety class (CWE-119 / CWE-787 / CWE-125) | Buffer overruns / out-of-bounds reads / writes. | Go runtime is bounds-checked; no exploitable `unsafe.Pointer` or `cgo` paths observed during inspection. Reported as a known gap of Config A in section 8 because confident detection would require a memory-safety scanner. |
| Dependency CVEs (CWE-1104 / CWE-1395) | Outstanding CVEs in `go.sum`-pinned indirect dependencies. | Would require govulncheck or Snyk to enumerate confidently; deliberately excluded per Directive 1. Reported as a known gap of Config A in section 8. |
| `build/nginx.transformer.conf` | NGINX reverse-proxy config for transformer. | Reviewed; same `error_log debug` and missing security-headers patterns as `nginx.backend.conf`, but the transformer is internal-only and not exposed by default. F-018 and F-019 against the backend NGINX serve as the canonical pattern instance for this audit. |
| `services/oauth/*` | OAuth flow implementation. | Reviewed; no concrete hardcoded credential observed, no observed token leakage in logged structured fields, and the gRPC client to the control plane uses TLS. No concrete finding emitted under this configuration. |
| `internal/enricher/` — MaxMind DB S3 download | Remote file download from S3. | The download path uses `filemanager.S3` with checksum verification; SSRF surface is bounded by the static S3 bucket configuration, not tenant input. No concrete finding emitted. |
| `Dockerfile` | Container hardening posture. | Reviewed; base images are SHA-pinned, `apk --no-cache upgrade` runs, an unprivileged `rudder` user is created via `addgroup -S rudder && adduser -S rudder -G rudder`, and the entrypoint runs as that user. No concrete finding against the Dockerfile itself; F-009/F-010 cover the env file that the container consumes. |
| `SECURITY.md` | Disclosure policy. | Reviewed; defines the disclosure channel (`security@rudderstack.com`) and supported-version policy. Presence of this policy is a positive control, not a finding. |

---

## 7. Deviations from Literal Interpretation

This section records every place where literal reading of the user input was modified during this audit, with rationale. The deviations are catalogued here so a downstream Config B / Config C reviewer can reproduce the same artifact set against the same input.

| Deviation | Literal Reading | This Audit's Reading |
|---|---|---|
| **Scope expansion: 1 file → 3 files.** | User input mentions "1 new file" (`findings-config-a.json`). | The Explainability and Executive Presentation rules each MANDATE an additional artifact; the total deliverable set is `findings-config-a.json` + `blitzy-audit/config-a-decision-log.md` + `blitzy-audit/config-a-executive-summary.html`. Rules are authoritative. (See Decision D-003.) |
| **Severity rubric definition.** | User input lists `critical \| high \| medium \| low` without boundary definitions. | This audit defines the boundaries in section 2 above, anchored on exploitability and blast radius. (See Decision D-001.) |
| **Line-anchor policy.** | The JSON contract names a `line` field of integer type, implicitly suggesting an exact line locus. | For config-file-level and dependency-level findings (F-005 — runtime default; F-008 — config default; F-019 — header policy across a server block), the `line` anchors to the proximal declaration line of the weakness rather than to an abstract "configuration in general." (See Decision D-007.) |
| **CSS embedding for executive deck.** | The Executive Presentation rule references `blitzy-deck/references/blitzy-reveal-theme.css`. | That reference file does NOT exist in this repository. To keep the deliverable self-contained per the same rule, the Blitzy theme CSS is embedded inline in the HTML. (See Decision D-006.) |
| **Folder placement.** | User input does not specify a location for supplementary artifacts. | Artifacts placed under a NEW `blitzy-audit/` folder to isolate them from the existing `blitzy/documentation/` content (which describes a separate program). (See Decision D-004.) |
| **Lens catalog scope.** | User input does not enumerate analysis lenses. | This audit derives a 10-lens catalog (section 1.4) from CWE Top 25 prevalence and from the security-relevant surface of `rudder-server`. The catalog is reproducible and recorded here. |
| **Trailing-newline policy for the JSON artifact.** | User Directive 2 simultaneously asserts (a) "minified to a single line, no pretty-printing, no newlines" and (b) the pass/fail probe `cat findings-config-a.json \| wc -l` returns `1`. A naïve reading treats "no newlines" as a ban on every `\n` byte, which would make POSIX `wc -l` return `0` and conflict with the literal user probe. The AAP §0.3.4 author's note "no trailing newline so that `wc -l` returns `1`" carried the inverse misconception (it had POSIX `wc -l` semantics backwards: `wc -l` returns `1` when there IS one terminating newline, not zero). | The file is serialized with **exactly one trailing newline byte** (POSIX line termination) and **zero internal newlines** within the JSON content. The reading is: "no newlines" refers to newlines as *content* (no pretty-printing whitespace within the JSON array), while the single trailing `\n` is *line termination* under POSIX text-file convention — `wc -l` counts completely-terminated lines, so a file ending with `]\n` has one terminated line and `wc -l` returns `1`. Both Directive 2 clauses are satisfied simultaneously. The byte-boundary contract becomes `raw[:1]==b'['`, `raw[-2:-1]==b']'`, `raw[-1:]==b'\n'`, and `b'\n' not in raw[:-1]`. The empty-result form is `[]\n` (three bytes) for the same reason. See Decision D-008 for the full rationale and the supersession of the AAP §0.3.4 author's note. |

---

## 8. Limitations of Native Agent Analysis

Honest accounting of what Config A cannot detect. These limitations are not failures — they are the experimental design of the baseline. The entire purpose of Config A is to measure agent-only recall versus scanner-assisted recall, so the gaps below are the comparison surface for subsequent configurations.

- **No taint-graph engine.** Multi-hop dataflow vulnerabilities that require following untrusted input across function boundaries with high confidence are at risk of being missed. Concrete example: a sink-side string concatenation that becomes unsafe only when reached via a specific source-side branch is hard to call without a taint tracer.
- **No symbolic execution.** Input-domain assumptions (e.g., "this string is always UTF-8 valid because of an upstream validator") cannot be exhaustively explored. The audit relies on locally-readable evidence in the cited file.
- **No cross-binary CVE feed.** Outstanding CVEs in `go.sum`-pinned indirect dependencies are not enumerated. The `go.mod` `replace` block records that some have already been addressed (`cyphar/filepath-securejoin`, `gin-gonic/gin`, `go-jose`, and others), but the audit does not produce a current CVE roster without govulncheck or Snyk.
- **No runtime DAST.** Endpoint behavior under malformed input is inferred from source, not observed. A gateway endpoint that *should* reject oversize bodies but actually accepts them due to a subtle bug would not surface from static reading alone.
- **No fuzz harness.** Input-mutation classes (e.g., panic via malformed protobuf, decoder error mishandling) are not exercised under this configuration.
- **Limited cross-file pattern detection.** Without semantic indexing, identical anti-patterns repeated across many files may be flagged inconsistently; the audit prefers reporting the canonical instance and recording duplicates in the considered-but-not-flagged log (see F-018 / F-019 vs. `build/nginx.transformer.conf` in section 6).
- **No SBOM correlation.** Cross-references between dependency versions and published CVEs (CWE-1104 / CWE-1395) are out of scope for this configuration.
- **No memory-safety analysis.** CWE-119 / CWE-787 / CWE-125 class issues in `unsafe`/`cgo` paths require a memory-safety tool to detect with confidence. Go's bounds-checked runtime limits exposure, but the absence of those tools means residual risk in any non-trivial `unsafe.Pointer` usage is not characterized here.

These gaps will be measured against subsequent scanner-assisted configurations to produce a recall-and-precision comparison.

---

## 9. Pass/Fail Verification Record

The literal verification commands and their expected results. Every probe below is expected to PASS on the artifact set as delivered; deviation from any expected result indicates a contract violation. The probe roster has been reconciled with Decision D-008 (single trailing newline on `findings-config-a.json` for POSIX line termination; zero internal newlines within the JSON content).

| Probe | Expected Result |
|---|---|
| `head -c 1 findings-config-a.json && tail -c 2 findings-config-a.json` | `[]` followed by a single trailing newline (the file's literal final byte). The first byte is `[`, the second-to-last byte is `]`, and the last byte is `\n` (POSIX line termination). |
| `cat findings-config-a.json \| wc -l` | `1`. The file contains exactly one newline byte (the POSIX line terminator at the very end); POSIX `wc -l` reports completely-terminated lines, so this yields `1` — matching the literal user Directive 2 pass/fail probe. See Decision D-008 and section 7 for the rationale by which both "no newlines" (read as: no internal newlines within the JSON content) and `wc -l = 1` (POSIX line termination) are satisfied simultaneously. |
| `python3 -c "import json; d = json.load(open('findings-config-a.json')); print(len(d))"` | `20`. |
| `python3 -c "import json; d = json.load(open('findings-config-a.json')); assert all(set(r) == {'file','line','severity','cwe','description'} for r in d); assert all(1 <= len(r['description']) <= 200 for r in d); print('OK')"` | `OK`. |
| `python3 -c "import json,re; d = json.load(open('findings-config-a.json')); assert all(re.fullmatch(r'CWE-\\d+', r['cwe']) for r in d); print('CWE-FMT-OK')"` | `CWE-FMT-OK`. |
| `python3 -c "import json; d = json.load(open('findings-config-a.json')); assert all(isinstance(r['line'], int) and r['line'] >= 1 for r in d); print('LINE-OK')"` | `LINE-OK`. |
| `python3 -c "import json; d = json.load(open('findings-config-a.json')); allowed = {'critical','high','medium','low'}; assert all(r['severity'] in allowed for r in d); print('SEV-OK')"` | `SEV-OK`. |
| `python3 -c "raw = open('findings-config-a.json','rb').read(); assert raw[:1] == b'[' and raw[-2:] == b']\n' and b'\n' not in raw[:-1]; print('BYTES-OK')"` | `BYTES-OK`. The full byte-boundary contract: starts with `[`, ends with `]\n` (closing bracket followed by POSIX line terminator), and contains zero newline bytes anywhere in the JSON content (i.e., no internal newlines). |
| `test -f blitzy-audit/config-a-decision-log.md && echo PRESENT` | `PRESENT`. |
| `test -f blitzy-audit/config-a-executive-summary.html && echo PRESENT` | `PRESENT`. |

All probes are expected to PASS on the artifact set as delivered.

