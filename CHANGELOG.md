# Changelog

Notable changes to Knowledge-BA46-Flow are documented here. Versions follow
Semantic Versioning; unreleased work stays under `Unreleased` until a release
tag is prepared.

<!-- markdownlint-disable MD024 -->
<!-- Added/Changed/Security headings intentionally repeat for each changelog version. -->

## [Unreleased]

### Added

- `KBM_DEV_STATIC=<dir>` dev mode for `serve`: the three UI pages and all
  `/static/` assets are then read from that directory on every request, so
  frontend edits show on a browser refresh without rebuilding the binary.
  Unset or non-directory values fall back to the embedded assets.
- `freshness --web-verify` (also `tasks --web-verify` and
  `"web_verify": true` in `/api/ops` for `freshness`/`all`): each flagged
  claim is queried against Ollama's hosted web search API and a second model
  pass (`prompts/freshness-verify.md`) re-judges it `stale`, `current` or
  `unverifiable` with evidence and source recorded in `kbm.freshness`.
  The verification pass is its own task lane: `ollama_task_models.verify`
  sets the model that re-judges evidence independently of the model that
  flagged (external lane → `ollama_model` fallback), and its choice is
  recorded as `verify_model`. Evidence-backed corrections arrive as normal
  reviewable suggestions; a
  search or verdict failure degrades to the unverified first-pass result.
  Requires `OLLAMA_API_KEY` — claim text, never the whole document, leaves
  the machine and `no_llm_context` documents are skipped as usual.
- A `.env` file next to the JSON config is now read for env-style values the
  real environment does not carry (real env always wins) — the compose-style
  file that already shared `OLLAMA_BASE_URL`/`KBM_*` between KBM-flow and the
  MCP server, and the place `OLLAMA_API_KEY` belongs, since the key is
  deliberately not a `config.json` field.
- i18n plumbing (`internal/i18n`, go-i18n + `golang.org/x/text` negotiation):
  all web-UI chrome strings on `/`, `/knowledge` and `/report` now resolve
  through a `t()` helper fed by `GET /api/i18n` (`Accept-Language`
  negotiation, `Vary` set), and CLI `Short`/flag-usage text goes through the
  same catalog via `i18n.T` after env-locale resolution
  (`KBM_LANG` → `LC_ALL` → `LC_MESSAGES` → `LANG`). Catalogs live in
  `internal/i18n/locales/active.<tag>.yaml` (`en` + a complete `da`
  translation); a new file localizes both surfaces without code changes.
  On the web pages the language is chosen by a flag dropdown in the header
  (flag-icons `gb`/`dk`, persisted in `localStorage`) or negotiated from the
  browser's `Accept-Language` when left on automatic; the picker overrides
  via `GET /api/i18n?lang=<tag>` so a Danish browser can still run the UI in
  English. Scope boundary: server data values (statuses, job details,
  reasons), diagnostic detail blocks (`repoDetails`, fleet health dumps),
  runtime log/error output and `errors.New` text stay English.
  `scripts/check-i18n.mjs` (wired into `npm run check:js`) fails on ids with
  no catalog entry, dead catalog keys, secondary-catalog keys unknown to
  English, and plural/string shape mismatches.

### Changed

- The `/knowledge` progress chart no longer plots the cumulative
  `suggestions`/`handled` counters — monotone totals that never return to
  zero duplicated each other's climb and obscured the trend. They now render
  as compact totals (e.g. "7.6K suggestions · 7.6K handled") beneath the
  "Suggestions to review" stat; the chart keeps notes, analyses, applied
  suggestions and the review-backlog line (`suggestions − handled`).
- Pending and accepted suggestions for `missing` documents — files the
  scanner no longer sees, like a deleted or moved-away directory — no longer
  appear in the review list or the apply queue; they resurface if the file
  returns. A source file that vanished before apply now ends `superseded`
  rather than `failed`, and a missing document that reappears unchanged has
  its `analyzed` status restored instead of staying `missing`.
- The review-backlog line on `/knowledge` no longer stays inflated by
  superseded suggestions: leaving the pending queue (supersede, apply
  failure) now stamps `decided_at`, and existing superseded/failed rows are
  backfilled to their creation day so historical backlog approximates what
  was actually open.
- Scan recognizes renames by canonical hash: a moved file keeps its analysis
  lineage, embeddings and pending suggestions instead of being treated as a
  deletion plus a brand-new document (only for an unambiguous single match —
  moved-and-edited files, duplicated content and vanished same-hash pairs
  stay separate documents).
- `ModelStamps` (the dashboard's "last used" lineage) ignores missing
  documents so a deleted directory can no longer dominate the dominant-model
  answer.
- Detected renames are recorded (`renames` table, schema v10) and the next
  pipeline run deterministically proposes retargeting `related`/
  `contradicted_by` entries and body links that still point at the old path —
  following move chains to the live file, spending no model calls — rather
  than dropping the links as unresolved. Pending and accepted `merge`
  suggestions follow the renamed target in the same transaction; `move`
  destinations intentionally stay, since the vacated slot is still valid.

### Added

- Reviewer feedback loop: rejecting a suggestion may carry a short reason —
  `reject ID --note "why"`, `POST /api/suggestions/{id}/decide` `{"note": ...}`,
  or the dashboard's reject prompt. The note is stored on the row
  (`decision_note`, schema v9) and quoted back to the model on later analysis,
  expand and freshness runs for that document only, and a re-proposal matching
  a rejected suggestion (same action+target, normalized reason, or proposed
  content) is suppressed instead of queued again.
- Pre-flight work estimate: `POST /api/ops` accepts `dry_run`, reporting the
  documents each stage would queue, the unchanged dependents the evidence
  sweep would add, and warnings when a selection differs from the recorded
  model lineage. The dashboard gains an "Estimate work" preview, and the
  model selectors mark which model produced the latest pass (`stamps` and
  `external_default` in `/api/models`), warning on generation- or
  embedding-model switches — including an unset override that resolves to a
  different configured default.
- Drift tests that keep the three option surfaces honest in both directions:
  every `/api/ops` op maps to a CLI command and vice versa, every CLI flag
  and `/api/ops` request field appears in the docs, every `config.json` key
  (including `report.*`/`validate.*`) is documented, and flags/fields the
  docs mention must exist in the tree. `report --remote` and
  `prune --trash-days` are now documented too.
- Independent generation and embedding selection: `analyze`, `run` and
  `tasks` accept `--model` plus `--embed-model`, `/api/ops` accepts
  `embed_model`, and the dashboard has separate role-specific selectors.
  Explicit installed models may be used without saving configuration.
- Opt-in **Refresh from Ollama** model discovery through `/api/tags` and
  `/api/show`: bounded, metadata-only queries populate generation/embedding
  choices by advertised capabilities and show local/cloud/unverified status.
  Discovery never downloads models or sends note content.
- Embedding model changes refresh incompatible vectors automatically,
  including analysis runs with no pending generation. Atomic vector/hash/model
  persistence and model-filtered ranking prevent mixed embedding spaces after
  interrupted refreshes; managed `kbm` writes remain hash-neutral.
- Split frontmatter/body hashing: alongside the canonical `sha256`, scan now
  stores `fm_sha256` (frontmatter minus `kbm`) and `body_sha256` per document,
  plus `seen_fm_sha256`/`seen_body_sha256` stamped at the last analysis —
  schema v7, additive migration. A frontmatter-only edit is now
  distinguishable from a body edit at hash speed, the dashboard labels
  documents "metadata changed" vs "content changed", and the freshness task
  keys its lineage on the body hash so metadata churn never re-dates content.
- Optional per-task LLM passes with their own lineage, CLI commands,
  `/api/ops` ops and dashboard pickers: `enrich` (standalone fast-model
  metadata pass), `embed` (explicit embedding backfill), `expand` and
  `freshness`, plus `tasks`/`all` to run every configured LLM task in one
  job — cheapest first, skipping tasks without a configured model. Each task
  stamps `<task>_sha256`/`<task>_model` per document, so runs stay
  incremental and `--reanalyze` model drift works per task.
- `expand` task: grows stub documents into reviewable `expand` suggestions
  using the nearest ancestor `STUB.md` as the category skeleton — a stub
  with no applicable template is refused, not improvised into generic prose.
  `STUB.md` files are excluded from scanning and validation. Proposals pass
  the same additive-edit/frontmatter/link guards as analysis.
- `freshness` task: heuristic currentness review — the model flags dated
  claims it cannot verify (version pins, "latest release", ageing dates)
  using the note, its modification date and `kbm.link_check` as context. The
  verdict, timestamp and flags are recorded in the note's `kbm.freshness`
  block — the same tool-owned, hash-immune carve-out as `kbm.link_check` —
  and concrete corrections arrive as ordinary `freshness` suggestions.
- Per-task model routing via `ollama_task_models` (JSON config): each task
  (`enrich`, `analyze`, `expand`, `freshness`, `embed`) takes a default plus
  alternatives; resolution order is per-run `--model`/`"model"` override →
  `--external` on lane-capable tasks → task default → lane fallback.
  `expand`/`freshness` prefer `ollama_model_external` when configured. A new
  `GET /api/models` endpoint reports the routing to the dashboard, which
  gained a task picker, a per-run model picker, and per-document
  link-check/freshness/change-kind badges.
- Browser e2e suite (`make e2e` / `npm run e2e`): `scripts/e2e/run.mjs` drives
  the real UI in system Chrome via the DevTools Protocol over Node's built-in
  WebSocket — no automation dependency. It builds the binary, seeds a
  throwaway fixture (`scripts/e2e/seed` writes deterministic suggestions into
  a scanned `state.db`; two `git init` repos feed the report), and presses
  every button on all three pages: filters, proposal inspection,
  decide/apply (asserting the file actually changes), all operation buttons
  with their POST payloads, dead-Ollama graceful failure, report
  refresh/sort/filter/details/per-repo/local-only/force runs, graceful
  shutdown, and a zero page-error budget per navigation. Skips cleanly
  without a browser; `KBM_E2E_CHROME` selects the binary.
- Report table filters: a **working tree** select (all/dirty/clean) and a
  **checks** select (all/has/none, matching the Checks column's count) join
  the existing signal select, which now also filters by Dependabot alerts,
  GitHub code-scanning alerts, and CodeFactor grades below B− — the same
  threshold `codeFactorNeedsAttention` uses for findings.
- `gems.hold` and `npm.hold` policy maps: name a package plus a reason to
  move it out of the outdated list into `gem_held`/`package_held`, rendered
  as `Held name — reason` in the report. Holds complement the automatic
  `constrained` severity (which only fires when a lockfile dependency edge
  records the blocker) by documenting deliberate pins such as
  `liquid: "stay on 4.x — jekyll does not support liquid 5 yet"`.
- Brand icons in the report's Checks column: scanner chips (`semgrep`,
  `sonar`, `forgejo`, `gh-actions`, `lefthook`), always-visible
  CodeFactor/Codecov lines for GitHub repos, the forge link, and the
  working-tree tag now show Simple Icons/Semgrep marks from the embedded
  `/static/icons/` set. Scanner chips link to the tool's site (sonar to the
  detected project dashboard) with a hover tooltip; the Semgrep license tier
  folds into the chip (`semgrep CE`) instead of a duplicate line. Findings
  get a red/green Lucide status icon plus a count, and action buttons and
  sort indicators use Lucide icons.
- Codecov coverage collection: remote report runs fetch `totals.coverage`
  for each GitHub repository's default branch (new `default_branch` field)
  via the public Codecov v2 API — `CODECOV_TOKEN` covers private repos.
  The percentage shows in each repository's detail view, a "Codecov below
  target" signal filter compares it against `checks.min_coverage`
  (80% when unset), and that optional policy key turns below-floor coverage
  into a `codecov coverage …` finding. Lookup failures keep the last-known
  value flagged stale; "not reported" is never treated as zero coverage.
- Progress-over-time charts: Chart.js v4.4.9 is vendored into the embedded
  assets and both dashboards chart real history. `/report` keeps a durable
  `repo_report_history` table (schema v8) — every report run appends its
  summary, capped at the 365 most recent runs — and the "Durable history"
  section renders problems/dirty/open-PRs/findings trends via
  `GET /api/report/history`. `/knowledge` renders a cumulative pipeline chart
  (notes, analyzed, suggestions, handled, applied, review backlog) from
  `GET /api/progress`, which aggregates the immutable timestamps on
  documents and suggestions per day. Buttons and suggestion-action tags on
  `/knowledge` now use the local Lucide icon set.
- Structured logging: `log_file` appends every log line to a file in
  addition to stderr (relative paths resolve against the config dir), and
  `log_level: "debug"` adds per-request HTTP lines (method, path, status,
  duration — `/static/` excluded) plus job start/finish detail. Both are
  overridable via `KBM_LOG_FILE`/`KBM_LOG_LEVEL`. The web UI relays
  uncaught exceptions and unhandled rejections to `POST /api/client-log`
  (new `static/clientlog.js`), so browser-side failures land in the same
  log with their stack instead of staying invisible to the Go process.
- A `Knowledge` button in the `/report` header, mirroring the report link
  on `/knowledge`.
- `example-configs/config.example-data.json` now lists every config option,
  and drift tests enforce it: `TestExampleConfigCoversAllFields` walks the
  `Config` JSON tags against the example, and
  `TestShippedYAMLExamplesCoverAllSchemaKeys` walks the `repos.yml`/
  `policy.yml` schema tags against the example files — new fields and
  schema keys can no longer ship undocumented.
- The e2e suite now budgets the server log as well as the browser console:
  after all UI checks it fails on any `panic:`/`WARNING:`/relayed
  `client error` line (the deliberate error-relay probe excluded).
- Test-hygiene gates: `TestConfigFieldsHaveBehavioralTests` requires every
  config field to be referenced by a test (it caught untested
  `ollama_think`/`ollama_keep_alive` — now covered by wire-level and
  validation tests), and `make test-logs` audits `go test -json` output for
  errors inside otherwise-passing tests (panics, WARNINGs, relayed client
  errors) — the CI coverage job audits the `test-report.json` stream it
  already produces. The audit caught a real job/teardown race in
  `TestOpsAcceptsTaskAndAll` (background `FinishJob` hitting a closed DB);
  the test now waits on `waitForJob` before teardown.
- Report version policy, generalized: `versions.*` gains `python`, `go`,
  `bundler`, and `rubygems` alongside `ruby`/`node`, all evaluated with range
  semantics (`"3.4.x"` family prefixes, `">=3.4"` floors, `||`/`-` ranges —
  exact pins unchanged). Values may also be channels resolved upstream on
  remote runs: `stable`, `lts` (Node), and `eol-floor` via endoflife.date,
  `stable` via RubyGems for bundler/rubygems. The resolved version is stamped
  into `policy_versions` (`node lts → 24.22.0`), cached 24h, carried stale on
  local-only runs, and unresolved channels explain themselves instead of
  flagging every repo. New pin detection: `.python-version`, `.go-version`,
  `go.mod` (`go`/`toolchain` directives), `Gemfile.lock` `BUNDLED WITH`,
  `.tool-versions` (all products incl. `nodejs`), and `rubygems:` CI-workflow
  keys — mismatches surface as `toolchain_issues` per repo and in the
  report's policy header. `versions.php` adds PHP (`composer.json`
  `require.php` checked as a constraint like `engines.node`, plus
  `config.platform.php`), and `versions.custom` is a generic adapter: any
  product name gets identical expression/channel semantics (endoflife slug =
  the key) against `.tool-versions` and `.<name>-version` pins — TypeScript
  stays out of scope since it runs on the repo's Node version and its
  package version is already npm dependency data.
- DeepWiki integration: the official `deepwiki.com/badge.svg` README badge
  (which also opts the repo into periodic wiki refresh), a `.devin/wiki.json`
  steering file shaping the generated wiki's page tree and repo notes, and
  `.devin/mcp_config.json` giving agents the DeepWiki MCP server for
  `ask_question`/`read_wiki` over the repo wiki. Indexing starts once the
  repo is pushed and public — or on first visit to its deepwiki.com page.
- Link checking, two layers. **Internal** (mandatory, every `scan`): markdown
  body links `](...)` — fenced/inline code excluded — resolve against the
  scanned document set (`.md`/`.markdown` targets) or the repository
  filesystem (asset targets); unresolved targets are stored in the new
  `link_issues` column, shown in the dashboard and surfaced to the analysis
  prompt as "Link check:" lines. **External** (opt-in): `KBM-flow
check-links`, `{"op":"link-check"}` in `/api/ops`, or the dashboard's
  _Check external links_ button probes every `http(s)://` URL in document
  bodies (HEAD with GET fallback, 20s timeout, 8 workers, per-URL dedup) and
  writes the verdict into each note's `kbm.link_check` frontmatter block —
  `at`, `dead` (only 404/410), `unverified` (auth walls, 5xx, timeouts).
- Canonical content hashing: `sha256` now covers the document with the
  tool-owned `kbm` frontmatter block excised byte-for-byte, so check
  bookkeeping never marks a document changed, never requeues analysis and
  never invalidates accepted suggestions (`apply` verifies the same
  canonical hash). Documents without `kbm` hash identically to before — no
  migration churn. `frontmatter.SetManaged`/`Canonical` own the block;
  `Validate` requires it to be a mapping, prompts forbid models from
  touching it, and `EditableField` keeps it out of every model patch.
  Writing `kbm` is the single documented file mutation outside `apply`.
- Three-lane model configuration: `ollama_model` (normal local analysis),
  `ollama_model_fast` (optional local enrichment) and the new optional
  `ollama_model_external` for explicitly requested enhanced runs (e.g. a
  `-cloud`-tagged model proxied through a signed-in local Ollama). An
  external run is opt-in via `--external` on `analyze`/`run`,
  `external: true` in `/api/ops`, or a dashboard checkbox; it fails fast
  when no external model is configured and never triggers accidentally.
  External results stamp new per-document `external_sha256`/`external_model`
  columns (schema v5, additive migration) instead of `analyzed_*`, so the
  local lineage is untouched and a following normal run does not treat the
  corpus as new or model-drifted. Each lane is incremental against its own
  markers — `--reanalyze`/`--reanalyze-all` apply per lane — while
  enrichment stays the shared local stage and suggestions continue to land
  in the one review queue (external passes supersede pending `analysis`
  suggestions; accepted/rejected history is kept). An external model
  receives document content, so the lane is documented as a privacy
  boundary; `no_llm_context` and stub documents stay excluded in every
  lane.
- `stub` semantics for placeholder notes. A document tagged `stub` — or one
  with at most `stub_max_body_lines` non-empty body lines (new config field,
  default 5, negative disables) — is scanned, embedded, ranked as a candidate
  and accepted as a `related`/`contradicted_by` target — its whole job is to
  exist for other notes to link to — but every generation stage (local
  enrichment, fast enrichment, full analysis) skips it. The skip is stamped
  like a policy skip so the document does not stay pending, and the
  deterministic unresolved-link repair still runs so a stub's links stay
  honest. The tag is human-owned; models are instructed never to add or
  remove it, and `taxonomy.yaml` gates whether a model may propose it at all.
  Run summaries report stub skips separately from MCP-policy skips.
- `KBM-flow validate`: a read-only structural gate for the repository. It
  enforces the shelf's documented conventions — frontmatter `tags` on every
  in-scope note, tags constrained by `taxonomy.yaml`, exactly one kind tag,
  `requires-language` on entity notes (index `README.md` files are exempt),
  index back-links and index completeness (nearest-ancestor shadowing),
  language↔catalog edges (`languages/x.md` ↔ `libraries/x/` /
  `frameworks/x/`), and the rule that non-Markdown snippets inside a note
  tree are referenced by name from a note. Meta/scratch paths opt out via
  `validate.exempt` patterns; exits non-zero listing every violation.
- Local-first metadata enrichment: clear missing title/tags can be derived
  from H1/path evidence even without a fast model; valid frontmatter is
  preserved. A root `taxonomy.yaml` supplies approved path-derived tags and
  constrains new tag proposals during analysis and apply. When a distinct fast
  model is configured, ambiguous notes still use it. The deterministic proposal is
  folded into a compatible full-analysis metadata/body proposal to avoid
  duplicate review actions; a move, merge or delete takes precedence.
  Enrichment-only retries still produce a deterministic suggestion. Candidate
  ranking uses exact document names and explicit Markdown/catalog links as
  evidence hints, without treating a mention as an automatic link.
- Local Ollama fixture runs strictly parse loopback URL authorities, reject
  credentials, paths and invalid ports, and the Ollama HTTP client blocks
  cross-origin redirects so prompts cannot be forwarded by a redirect.
- Curated `example-knowledge/basic`, `intermediate`, and `e2e` shelves with
  2/7/12 hand-picked Markdown documents and per-shelf golden manifests. Fast
  scanner/analyzer integration tests use deterministic fake models; an
  explicit `make example-ollama TIER=...` target opts into loopback-only live
  runs with isolated databases, logs, and source-hash audits. Test shelves
  deny writes; the live runner never accepts or applies suggestions, and the
  e2e test accepts one isolated temporary-copy suggestion only to verify the
  deny policy.
- Optional real-content LLM evaluation uses a SHA-pinned allowlist and rubric
  under `example-knowledge/llm-eval`; the runner copies only those notes and
  the pinned taxonomy sidecar from a local Engineering Memory shelf into an
  ignored per-run corpus, fails closed on hash drift, private-policy notes or
  credential-like strings, and keeps the source repository outside KBM-flow's
  writable/read root.
- Scanner exclusion controls. `exclude` accepts repository-root-anchored
  literal paths and glob patterns (`*`, `?`, `[...]` within a path segment,
  `**` as a whole segment for any depth — e.g. `docs/tmp`, `drafts/**`,
  `**/*.draft.md`); a matching directory prunes its whole subtree.
  `exclude_extensions` removes file types from the `extensions` allowlist
  (case-insensitive, leading dot required). Both are validated at load time,
  excluded entries are hidden from analysis sibling-file context, and
  `exclude_directories` keeps its unchanged name-at-any-depth matching.
- Per-attempt `stage_results` audit records (schema v4): parsed model metadata
  and patches, missing/empty/unchanged/derived/suggested/rejected patch outcomes,
  locally derived fields, rejection reasons, latency and durable model errors.
  Successful records commit with the pass; later analysis cannot overwrite
  enrichment evidence. Existing databases retain their documents, embeddings
  and review decisions; historical replies are not fabricated.
- Two-stage LLM pipeline for `analyze`/`run`. An optional cheaper model
  (`ollama_model_fast`) now handles a fast enrichment pass — title, summary,
  tags and a `frontmatter_patch` merged locally into an `expand` suggestion —
  before the configured `ollama_model` runs the full analysis. Enrichment is
  tracked per document (`enriched_sha256`/`enriched_model`) so enabling or
  changing the fast model does not force a full re-analysis; without it the
  pipeline behaves exactly as before.
- Deterministic frontmatter fixes with no LLM involvement: unresolved or
  invalid `related`/`contradicted_by` targets are removed and offered as
  ordinary pending `expand` suggestions (`origin: deterministic`), including
  for `no_llm_context` documents where they never reach a model.
- Optional semantic candidate ranking via `ollama_embed_model` (e.g.
  `embeddinggemma:300m`). When configured, LLM-visible documents get a stored
  embedding vector (schema v3, backfilled lazily at analysis time —
  `no_llm_context` docs are never embedded) and analysis candidates rank by
  cosine relatedness instead of text overlap alone. Measured on the real
  corpus: `languages/jekyll.md` ↔ `frameworks/jekyll/README.md` went from
  0% (shingles) to ~70% (embeddings) — same-topic docs with different
  vocabulary now surface as link candidates. The shingle sketch still drives
  the ≥50% duplicate flag, which embeddings are blunter at.
- `ollama_keep_alive` request option: `"0"` unloads each model immediately
  after a call — relevant when large models alternate on memory-constrained
  hosts (Ollama otherwise keeps every used model resident for ~5 minutes).
- Content-similarity retrieval for the analysis prompt: `scan` now computes a
  bottom-k minwise signature over word 3-shingles per document (stored as a
  `sig` blob, DB-agnostic — no FTS dependency), candidates are ranked by
  estimated overlap and injected annotated with title, tags and similarity,
  and pairs above ~50% are flagged as probable duplicates. Merge/link
  decisions now rest on measured text overlap rather than filenames.
- Suggestions record which stage produced them (`origin`: `analysis`,
  `enrich`, `deterministic`); the review UI shows non-analysis origins as a
  tag, and re-running a stage supersedes only the pending suggestions it
  regenerates.
- Shared proposal validation before review and again at apply, including
  older accepted suggestions: body edits must retain existing text in order
  and preserve code verbatim; merge edits must retain both documents;
  provenance/lifecycle/privacy/custom metadata is read-only; invalid
  frontmatter and unresolved graph targets are rejected rather than annotated.
  Moves must preserve the document extension and cannot overwrite or contain
  proposed content. Unsafe edits are counted with per-document diagnostics.
- Full analysis can now return a locally rendered `frontmatter_patch` for
  title/tags/related/contradicted_by without regenerating the document body.
  Enrichment is restricted to title/tags. Prompt contracts explicitly separate
  the current note from the copy/install paths described inside it.
- `docs/MODELS.md` and an optional `config.m2-24gb.json` example document
  the stopped Phi corpus test, Gemma/Phi role split, a smaller Qwen2.5
  comparison candidate and a review-first qualification checklist.
- Go fuzz tests over the untrusted parsers (`FuzzParse`,
  `FuzzCanonicalParts`, `FuzzSetManaged`, `FuzzExternalURLs`,
  `FuzzInternalTargets`) asserting real invariants — canonical
  split-integrity, `kbm` hash-immunity, no invented link targets — plus a
  malformed-frontmatter fail-closed test matrix and benchmarks for the hot
  paths (similarity sketching, cosine distance, canonical hashing,
  incremental rescan). Discovered crashers live in `testdata/fuzz/` as a
  permanent regression corpus.

### Fixed

- Evidence-dependents sweep no longer collapses under a dense embedding
  model: the absolute cosine threshold (0.72) is replaced by a per-run
  calibrated gate — each changed note contributes at most its 16 nearest
  neighbors, and only pairs above the 95th percentile of the corpus's own
  sampled cosine distribution. Cosine scores are model-specific, so a fixed
  bar tuned on one model re-analyzed nearly the whole repository after a
  denser model was selected.
- Dashboard "Inspect proposed content" stayed on "Loading…" forever, from
  two distinct CSP-build issues in `knowledge.html`. First, expressions
  calling `JSON.stringify`/`Math.round` threw "Undefined variable" — the
  `@alpinejs/csp` evaluator resolves no host globals. Second and deeper:
  `x-text="proposalText(suggestion.id)"` never updated because the CSP
  evaluator calls template-invoked methods with `this` bound to a scope
  object rather than the reactive component proxy, so `this.proposals[id]`
  reads inside the method were never tracked and the binding went stale.
  Expressions now read reactive state directly
  (`proposalText(proposals[suggestion.id])`, with `proposalText` a pure
  formatter), global access moved into `linkCheckTooltip`,
  `freshnessTooltip`, and `confidenceLabel` component methods, and
  `TestTemplatesAvoidForbiddenGlobals` lints Alpine attribute expressions
  against a full host-global denylist. Verified end-to-end in a real
  browser via Chrome DevTools Protocol.
- Same class audited across every template: two more `x-for`-scoped methods
  read mutable `this.*` state — `:disabled="modelUnavailable(m)"` read
  `this.discovered` and `repoDetails(repo)` read `this.snapshot` — both
  could go stale the same way. Both now receive the reactive value as an
  explicit argument (`modelUnavailable(m, discovered)`,
  `repoDetails(repo, snapshot)`). Argument-pure helpers, component-scope
  calls (like `filteredSuggestions()`), and event handlers were verified
  unaffected — handlers get the reactive `this`, writes propagate.
- `frontmatter.SetManaged` could emit a `kbm` block the parser silently
  drops: when the frontmatter's top-level mapping is indented (valid but
  unusual YAML), the column-0 `kbm:` insertion landed outside the mapping.
  The write is now self-verifying — the result must re-parse with `kbm`
  reachable and the canonical hash unchanged — and frontmatter shapes that
  cannot host a block fail closed instead of losing the write.
- `frontmatter.CanonicalParts` kept an empty `---\n---\n` block as literal
  bytes while `Parse` consumed it as an empty mapping, so a managed write
  into an empty block changed the canonical hash. Empty blocks now collapse
  like `kbm`-only ones, keeping managed writes hash-neutral.
- `links.ExternalURLs` treated any scheme starting with `http` (e.g.
  `http0://`) as an external URL in link targets; only `http://`/`https://`
  qualify now — matching the bare-URL pass.
- `tasks`/`all --model` applied the override to the embed lane too, so a
  chat model was sent to `/api/embed` and the batch aborted. The override
  now covers generation lanes only; embed uses its configured lane or an
  independent `--embed-model` override (standalone `embed --model` remains a
  backward-compatible alias).
- Freshness structured output now constrains `target_path` to the empty
  string, matching the in-place-only correction contract. The local proposal
  validator remains authoritative for non-additive body edits and code changes.
- CLI task model selection now uses the same capability/privacy preflight as
  analyze/run and the HTTP API; cloud generation requires `--external`, while
  remote enrichment/embedding requires `--allow-cloud`.

### Changed

- The report's combined "Node and gem health" section is now two sections —
  **Gem health** and **Node and npm health** — rendered by `gemFleetDetails()`
  and `nodeFleetDetails()` respectively. The gem section leads with the
  RubyGems latest-version coverage count. Constraint-held packages
  (`severity: constrained` — transitive pins no depender allows upgrading)
  collapse into a `<details>` group summarised by count, so the visible
  `Behind` list shows only actionable updates; the npm stale-cache warning
  now lists at most 12 package names instead of dumping the entire cache.
- The `github.com/ollama/ollama` SDK dependency was replaced with a thin
  `net/http` client in `internal/ollama` (generate/show/embed over the
  documented wire format — same request fields, same error semantics,
  stream-tolerant decoding). The dependency tree shrank by ~95 modules and
  no longer carries Ollama server code, so `govulncheck` reports zero
  reachable advisories and the CI vulnerability scan is now a hard gate
  instead of advisory-only.
- New CI `semgrep` job: `p/golang` + `p/security-audit` registry rulesets
  in the pinned `semgrep/semgrep` image, `--error` so any finding fails CI
  (currently zero findings across Go, JS, Dockerfile and config; local
  parity via `make semgrep`).
- `make coverage`/`make coverage-ci` now run with `-coverpkg=./...` so the
  coverprofile attributes lines exercised by other packages' tests to the
  files that own them — Codecov and SonarQube previously under-reported
  cross-package coverage (~80% shown vs 85.5% real).

- `frontmatter.SetManaged` (the `kbm` writer) is now byte-preserving: only
  the `kbm` mapping is re-emitted, every other frontmatter byte and the body
  pass through verbatim. Previously it re-rendered the whole frontmatter, so
  a managed write on a human-formatted file changed `fm_sha256` and looked
  like a metadata edit; managed writes are now invisible to all three
  content hashes.
- Malformed analysis JSON now gets one annotated retry: the re-prompt names
  the parser error (e.g. an unescaped backslash) instead of blindly repeating
  a temperature-0 request that would produce the same slip. A second failure
  is still recorded per document; retry latency counts toward the stage
  duration. Enrichment keeps its existing next-run retry instead.
- Optional enrichment generation failures no longer prevent full analysis.
  They remain separately counted retryable warnings; source/privacy checks
  still fail closed and cancellation still aborts. Successful full analysis
  is not repeated merely to retry failed enrichment on a later run, and its
  document metadata is not overwritten by that late enrichment-only retry.
  Unloading an absent optional model is idempotent rather than blocking analysis.
- Enrichment has its own configurable 120-second deadline and 512-token output
  limit, always sends `think:false`, and reserves only a bounded reply context.
  Extracted title/tags produce locally validated missing-field proposals even
  without frontmatter; existing fields and explicit patch decisions are
  preserved. Both source and candidate annotations refresh between stages.
- A scan change refreshes unchanged semantic reviews when it changes an
  explicit link/name dependency or a high-confidence duplicate/embedding
  candidate. Unrelated documents are not automatically re-analyzed.
- `--reanalyze-all` (or `reanalyze_all: true` in `/api/ops`) explicitly refreshes
  all non-missing notes after prompt/taxonomy policy changes; normal runs do
  not repeat the full local-model pass.
- Incremental scans invalidate embeddings when the source hash changes, so
  semantic retrieval cannot reuse a vector for an older version of a note.
  Unchanged documents retain their cached vectors.
- Model stages now run in batches (embeddings, enrichment, analysis) rather
  than switching generation models for every document. Completed embedding/
  enrichment models unload at stage boundaries; analysis candidate metadata
  is refreshed after enrichment. Candidate context is bounded to 16 notes,
  with at most two complete high-overlap merge bodies in a 12 KB budget.
- Structured generation uses temperature `0`; length/context-truncated
  responses fail instead of being recorded as successful analysis. Schemas
  allowlist metadata patch fields, with matching local validation.
- CLI SIGINT/SIGTERM now cancels requests and records interrupted jobs cleanly.
  Live file privacy is rechecked before embedding/generation, so a policy
  added after scanning does not expose the document through those requests.
- Remote collectors (GitHub, Forgejo, Semgrep, Sonar, CodeFactor, RubyGems,
  npm, Node.js releases) now issue requests through `resty.dev/v3` (pinned
  to the v3.0.0 release candidate; the v2 line was the stable alternative)
  instead of a hand-rolled `net/http` wrapper. The shared helper decodes
  JSON directly into typed results, so unmarshal failures surface as
  errors instead of silently behaving like empty responses.
- Ollama communication now goes through the official `ollama/ollama/api`
  client instead of a hand-rolled HTTP wrapper, keeping the same
  `internal/ollama.Client` surface (model, timeout, `num_ctx`,
  structured-output `format`). Streaming and `keep_alive` are now
  available through the upstream client for future use. Requests send
  `think: false` by default (new `ollama_think` opt-in): measured on
  qwen3:14b, thinking mode took ~375s per analysis document where
  `think: false` took ~59s — the difference between timing out and
  completing under the default 300s request timeout.
- `ollama_num_ctx` now auto-sizes per request when unset/0: roughly
  `prompt_tokens × 2 + ~1000` tokens (expand/merge replies can be as large as
  the input document), floored at 4096 and capped at the model's
  `context_length` from `/api/show` (16384 fallback if the query fails). Small
  documents get small windows and faster KV-cache allocation; large ones fit
  without manual tuning. A positive value still pins the window as before.
- Version comparison in the repository report now uses the aquasecurity
  `go-gem-version` and `go-npm-version` libraries (the same engines Trivy
  uses) instead of hand-rolled parsers, covering RubyGems prerelease/`~>`
  semantics and npm `^`/`~`/`||` range rules correctly. Unparseable
  requirements stay non-blocking, and npm hyphen ranges (`X - Y`) remain
  supported via expansion. Behavior is pinned by a parity regression test.
- The bounded remote-fetch worker pools now use `golang.org/x/sync/errgroup`
  with `SetLimit` instead of hand-rolled `WaitGroup`/channel plumbing, and
  the report package migrated to stdlib `slices`/`maps`/`cmp`
  (`slices.Sort`, `slices.SortFunc`, `slices.Sorted(maps.Keys)`,
  `maps.Clone`, `cmp.Or`) in place of the `sort` package and small custom
  helpers.
- Path validation consolidated: the three private `cleanRelative` copies in
  scanner, review, and mcppolicy now share `internal/paths.CleanRelative`.
  The unified check is the strictest of the three — paths containing a `..`
  segment are rejected outright even when they would clean back inside the
  repository.
- Shared collection helpers for the report package: a generic `appendUnique`
  (replacing three type-specific copies), `addFleetLabel`/`sortFleetVersions`
  for the name→version→labels map shape, `sortVersionStrings` for the
  parse-aware version ordering, and `markStaleFields` for the GitHub/Forgejo
  stale-field marking.
- npm supply-chain hardening: `.npmrc` sets `min-release-age=7`, so freshly
  published package versions (< 7 days old) are not installed — matching the
  documented "prefer releases at least a week old" dependency policy.
- Semgrep suppressions for four `w.Write` XSS audit findings in
  `internal/server`: the three embedded static UI pages and the
  `application/json` report snapshot carry no user input, so the findings are
  documented false positives (`nosemgrep: no-direct-write-to-responsewriter`).
- Baseline HTTP security headers on every route: `Content-Security-Policy`
  (`default-src 'self'; script-src 'self'` — the vendored `@alpinejs/csp`
  build evaluates directives without `new Function`, so `'unsafe-eval'` is
  not granted), `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
  and `Referrer-Policy: no-referrer`.
- Frontend Alpine components are registered via `Alpine.data` in
  `alpine:init` listeners (required by the CSP build), and the three
  template-literal `x-text` expressions the CSP evaluator cannot parse moved
  into component methods.
- The container CI job now scans the built image with Trivy
  (`aquasecurity/trivy-action` v0.36.0, SHA-pinned), failing on HIGH/CRITICAL
  findings — OS-package coverage `govulncheck` cannot provide.
- `BenchmarkSketch` pins the per-document similarity-sketch cost that runs on
  every scanned file.
- `docker-compose.yml` is now the complete stack: KBM-flow plus
  knowledge-base-mcp-server (built from the pinned `v0.2.1` git tag, local
  checkout supported). Both services share the `./data/knowledge` corpus
  volume, keep their own state dirs (`./data/faiss`, `./data/kbm`), run as
  `user: 1000:1000`, and read shared values from one `.env`
  (`.env.example` provided). New `reindex_trigger_path` config: after each
  successful apply batch KBM-flow truncates a repository-relative trigger
  file; pointing the server's `REINDEX_TRIGGER_PATH` at the same file makes
  reindex automatic — compose wires exactly that, replacing the manual
  `kb reindex` step. `config.Load` now accepts environment overrides
  (`OLLAMA_BASE_URL` — deliberately the server's own variable —
  `KBM_REPOSITORY_PATH`, `KBM_DATABASE_PATH`, `KBM_HTTP_ADDR`,
  `KBM_OLLAMA_MODEL`, `KBM_OLLAMA_MODEL_FAST`), applied before required-field
  validation so a shared `.env` can drive both programs.
- `database.Open` takes a `context.Context` and threads it through the
  pragma/migration path (`ExecContext`, `QueryContext`, `BeginTx`) — the last
  ctx-blind blocking calls in the codebase. `.golangci.yml` now enables
  `noctx` (keeping every DB/HTTP/exec call context-aware) and `exhaustive`
  (guarding the action/status enum switches); `contextcheck`, `funlen`,
  `cyclop` and `gofumpt` were evaluated and rejected with the rationale
  recorded as comments in the config.
- `golangci-lint` wired into the quality gate: `.golangci.yml` enables
  errcheck/staticcheck/ineffassign/unused/revive/gocritic/gocyclo(>25)/dupl/
  gosec/bodyclose plus gofmt/goimports, `make lint` runs it (self-skipping
  without the binary, `LINT_REQUIRED=1` fails closed), `make check` includes
  it, and a dedicated CI `lint` job runs the pinned action. First-run
  cleanups: unified the identical GitHub/Forgejo pull structs into `pull` +
  `pullFields`, shared `jobByID` between the two job endpoints, split
  `deriveProblems` into per-domain helpers, tightened DB dir permissions to
  0750, and marked the two background-job goroutines `#nosec G118`
  (durable jobs intentionally outlive their request).
- `make vuln-check` runs pinned `govulncheck` (reachable advisories, strict
  locally; advisory in CI because open `github.com/ollama/ollama` advisories
  have no fixed release). `golang.org/x/crypto` bumped to v0.57.0 to clear
  the one fixable finding (ssh.ParsePrivateKey, reachable only transitively).
- `containsString` and the identical `githubPull`/`forgejoPull` structs
  replaced by `slices.Contains` and a shared `pull`/`pullFields` pair.

### Added

- Per-document analysis progress: the `analyze`/`run` CLI commands print a
  `[i/N] path` line plus the per-document outcome, and HTTP analyze/run jobs
  update their `detail` while running so the dashboard shows the document in
  flight instead of a silent "In progress…".
- Live dashboard updates: `/knowledge` keeps polling `/api/ops` while idle
  (10s cadence) so CLI-started runs appear without a manual refresh, and
  refreshes documents/suggestions during a running op.
- Cross-process operation guard: `scan`/`analyze`/`run` now record durable
  jobs with a heartbeat; starting any op while a live job runs — from CLI or
  HTTP — is refused (`job #N (op) is already running`, HTTP 409). Jobs whose
  heartbeat freezes for 90s (e.g. a killed process) are swept to `failed`
  instead of blocking future runs. `/api/apply` is also refused while a
  background job is live.
- Graceful shutdown from the UI: `POST /api/shutdown` (loopback only) and a
  Stop server button on the knowledge dashboard. Refuses with `409` while an
  operation runs in this process unless `?force=1` is sent.
- Job-history retention: the `jobs` table (which also holds report run
  history) is bounded automatically — every new job prunes finished rows
  beyond the newest 200 — and `KBM-flow prune --keep N` applies a manual bound
  and sweeps stale `running` rows to `failed`. Running jobs are never pruned.
- Analysis provenance: each document records `analyzed_model` — the model that
  produced its last analysis — shown in the dashboard and in job summaries.
  `analyze --reanalyze` / `run --reanalyze` (or `reanalyze: true` in the
  `/api/ops` body / dashboard checkbox) redoes documents analyzed under a
  different model; re-analysis supersedes the document's still-pending
  suggestions while keeping accepted/rejected decisions. The landing page
  reports how many documents were analyzed with another model.

- A landing page at `/` with Bulma summary cards for the knowledge workflow and
  repository fleet; the full knowledge dashboard now lives at `/knowledge`,
  and the repository report remains at `/report`.
- A Go repository report with local checks, GitHub/Forgejo/CodeFactor/Sonar/
  Semgrep collectors, Ruby gem-fleet analysis, and Node/npm lockfile,
  runtime-version, drift, and outdated-package checks. It is configured
  through `report.registry_path` and optional `report.policy_path`; it remains
  read-only toward registered repos and does not run `kb-sync`.
- Report CLI controls for local-only refresh, one-repository refresh, forced
  remote/cache refresh, and full JSON output.
- Sortable repository columns and report-signal filters for open pull requests,
  Semgrep findings, Sonar gate failures, any selected signal, or repositories
  matching all three signals.
- Safe browser links from report rows to HTTP(S) repository remotes.
- Optional timestamped reference totals and seed cache from the original Ruby
  report's `data.json` (`report.comparison_path`) for migration validation.
- A dashboard summary API for changed documents, days since the last
  successful `run` cycle, repository count, scanner totals, and available
  comparison metrics.
- Frontmatter curation: `scan` parses and validates a document's YAML
  frontmatter against the knowledge-base-mcp-server field whitelist, stores
  the parsed fields and local issues per document, feeds them to the analyzer,
  and applies LLM-proposed corrections through the normal `expand`/`merge`
  review flow with a strict re-check at apply time.
- `mcp_server` config option selects the target-server prompt file
  (`prompts/<mcp_server>.md`); it defaults to `knowledge-base-mcp-server` and
  rejects unknown values until more servers are supported.
- The analyzer prompt now runs ordered review passes (freshness, similarity,
  linking/backlinks, location, completeness/frontmatter, deletion) with
  deterministic context: inbound `related` links, unreferenced sibling files,
  file modification date, and ranked merge candidates (same directory and
  already-linked documents first).
- Versioned CLI builds from the root `VERSION` file, with version/tag/changelog
  consistency checks before release tags.
- Monthly Dependabot updates for Go modules, npm frontend assets, Docker base
  images, and GitHub Actions.

### Changed

- CLI `run` cycles are persisted in SQLite so the landing page reports the
  last successful cycle consistently for CLI and web runs.
- MCP-targeted scanning skips dot-prefixed entries, the server's built-in
  temporary directories, and its ingest workflow sidecars.
- Analysis honors `kb_policy.no_llm_context` locally: policy-excluded
  documents are not read into prompts or sent to Ollama.
- Accepted filesystem changes honor shelf `.kb-policy.json` mutation policy;
  malformed policies fail closed and the policy file cannot be changed by
  KBM-flow.
- MCP compatibility guidance now recommends configuring one shelf as
  `repository_path`, rather than scanning the parent shelves directory.
- The browser dashboard uses pinned, locally embedded Bulma and Alpine.js
  assets with CI checks for asset consistency, JavaScript, CSS, and formatting.
- Docker builder and runtime images use pinned Debian Trixie-based images.

### Security

- Accepted suggestions are re-hashed before application; stale suggestions are
  marked superseded rather than applied.
- Filesystem changes are constrained to the repository root, including
  symlink-aware containment.
