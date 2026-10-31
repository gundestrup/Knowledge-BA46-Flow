# Tests

Queue-policy regressions cover default and legacy-accepted write blocks,
read-only reconciliation previews, current-token/explicit HTTP confirmation,
machine audit versus human feedback, unavailable taxonomy validation, withdrawal,
and note/type grouping. Identity tests cover reviewed byte-preserving UUID
insertion, rename-plus-edit freshness, duplicate IDs, live UUID replacement,
incoming references, outgoing-link warnings and live-copy ordering.
Semantic-hash tests verify graph-only link edits do not invalidate task
lineages but still stale full-source proposals; visible text changes do.
Prompt tests verify a task prompt edit invalidates only that task. Ordered
frontmatter tests pin `document_id`, title, graph/retrieval fields and `kbm`
placement while preserving values/body. Task-entry tests verify a preflight
scan catches post-apply edits before an LLM prompt is built.

The opt-in production replay also previews and confirms reconciliation only in
the sandbox, checks retirement/audit counts and unchanged note bytes, then
accepts/applies one valid expansion per note. Its report includes `write_scope`,
`queue_counts`, `reconciled`, and `review_events`. No live queue is cleaned up.

Run the standard quality gate:

```bash
make check
```

`make check` validates `VERSION`, the README badge, and changelog structure;
verifies `gofmt`; runs `go vet` and `golangci-lint` (when installed); executes
tests with the race detector; and builds the CLI. Run individual checks while
developing:

```bash
go test ./...
go vet ./...
go test -race ./...
go build ./...
```

## Production snapshot replay

An explicit opt-in test reuses real stored proposals without changing the
production database or documents:

```bash
KBM_REPLAY_CONFIG=../engineering-memory/.kbm/config.json \
KBM_REPLAY_REPORT="/tmp/kbm-replay-$(date -u +%Y%m%dT%H%M%SZ).json" \
go test ./internal/review -run '^TestProductionReplay$' -count=1 -v
```

`KBM_REPLAY_CONFIG` is a test-only environment variable, not a normal config
override. The test is skipped unless explicitly enabled; ordinary CI never
needs a real knowledge repository. `KBM_REPLAY_REPORT` is optional and its
parent directory must exist. Reports are private (`0600`), contain IDs/paths
and validation errors rather than proposal bodies, and refuse to overwrite an
existing file. Do not commit production fixtures or reports.

The source SQLite connection uses `mode=ro` and the online backup API, including
committed WAL pages. Only the private temporary copy is passed to
`database.Open` and migrated. A raw `cp state.db` can miss WAL data; opening the
original through the application can migrate a database an older server still
holds. Unit tests cover WAL inclusion and preservation of deny policies.

The repository snapshot copies regular configured documents, taxonomy and
mutation policies, excluding credentials, hidden runtime/index/Git state and
symlinks. Validation differences caused by excluded files are reported as
incomplete snapshot coverage, not as proof of an invalid production proposal.
The database snapshot is transactional, but copying files is not globally
atomic: wait for writers, and use a filesystem snapshot or pause mutations when
a strict cross-resource point-in-time copy is required. The test refuses active
jobs and compares source document/database/WAL hashes before and after.

Pending, accepted and historical failed rows are preflighted separately against
the copied current content. Invalid and stale rows are expected classifications,
not test failures. Only the earliest valid accepted/pending expansion per
document is selected for simulated review and apply; failed history is never
selected. Moves/deletes/merges remain excluded even if previously accepted.
The test asserts that every selected expansion applies and unselected files
remain unchanged. Repository/database paths point to temporary copies, the
reindex trigger is disabled, and no model/MCP/network call runs. Temporary
copies are removed after the test.

## Curated example corpora

`example-knowledge/basic`, `intermediate`, and `e2e` are isolated shelves with
2, 7, and 12 scanned Markdown documents. Their directory layout follows a
normal knowledge shelf and includes supporting scripts, hidden/trashed files,
and configured exclusions. The cases cover an omitted relationship, missing
frontmatter, invalid and unresolved links, an old note, `no_llm_context`,
model-stage errors, duplicate content, contradictory claims with dated sources,
and the write-policy gate. The `basic` shelf additionally pins the exclusion
mechanisms: hidden entries, the MCP ingest filter, `exclude_directories`, and
per-shelf `exclude`/`exclude_extensions` values declared in its
`expected.json`.

Each shelf's `expected.json` is a reviewed golden standard. The
`internal/examplecorpus` tests copy the shelf to a temporary directory, run the
real scanner and staged analyzer against deterministic fake generators, and
assert exact scan sets, proposals, rejection reasons, model visibility, stage
outcomes, and source hashes. The e2e case explicitly accepts one test-only
suggestion and confirms that its deny policy prevents the write. These tests
run as part of `go test ./...` and `make check`; they do not contact Ollama.

Run only the fast corpus checks with:

```bash
make example-check
```

Live model output is variable, so the golden standard specifies review
invariants and scenario-level expectations rather than exact generated prose.
To opt in to local Ollama for one tier:

```bash
make example-ollama TIER=basic
make example-ollama TIER=intermediate
make example-ollama TIER=e2e
make example-ollama TIER=llm-eval
```

This target requires locally installed `gemma3:12b`, `phi4-mini:latest`, and
`embeddinggemma:300m` by default. Override the generation models with
`KBM_OLLAMA_MODEL` and `KBM_OLLAMA_MODEL_FAST`; set `OLLAMA_NO_ENRICH=1` to
compare a single analysis pass. The runner uses loopback Ollama only, merges
the tier's declared `exclude`/`exclude_extensions`/`exclude_directories` into
a per-run config (via `scripts/merge_example_config.go`) so live runs scan
the same document set as the tests, stores its database, config, golden copy,
before/after Markdown hashes, status and log under ignored
`data/example-runs/<timestamp>-<tier>/`, and never accepts or applies
suggestions. Each fixture's `.kb-policy.json` also denies writes.
The runner rejects non-loopback URL authorities before invoking `ollama list`;
the Ollama client also has an `httptest` regression for cross-origin redirects.

`TIER=llm-eval` is the separate real-content quality check. It materializes
only the SHA-pinned Markdown allowlist plus its pinned `taxonomy.yaml` sidecar in
`example-knowledge/llm-eval/selection.json` from `../engineering-memory` (or
`REAL_KB_ROOT`) into that run's ignored directory. Preparation fails closed on
hash drift, malformed frontmatter, `no_llm_context`, or obvious credential
patterns; the original knowledge files are never the configured repository.
The manifest is paths and hashes only, so real note bodies are not committed.

## Browser e2e (Chrome DevTools Protocol)

`scripts/e2e/run.mjs` drives the real web UI in the system Chrome/Chromium —
no browser-automation dependency is vendored; Node's built-in WebSocket speaks
the DevTools Protocol directly:

```bash
make e2e        # or: npm run e2e
KBM_E2E_CHROME=/path/to/chrome npm run e2e   # custom browser binary
```

The suite builds the binary, creates a throwaway fixture (four Markdown
documents, a seeded `state.db` via `scripts/e2e/seed`, two `git init` fixture
repositories for the report page, and a config with a dead Ollama URL), starts
`serve` on a free loopback port, and exercises all three pages in one tab:

- **Landing**: dashboard data renders, "Refresh overview" re-fetches
  `/api/dashboard`, and the knowledge portal card navigates to `/knowledge`.
- **Knowledge**: pending/accepted status filters fire the matching
  `/api/suggestions?status=…` requests; the origin filter re-renders locally
  without a fetch; "Inspect proposed content" fetches
  `/api/suggestions/{id}` and renders the proposal (the regression this suite
  was built for — the panel must not stay on "Loading…"); Reject and Accept
  fire `/api/suggestions/{id}/decide` and remove cards; "Apply accepted"
  posts `/api/apply`, and the fixture file on disk is asserted to contain the
  applied paragraph. The operation buttons are exercised for real: "Scan",
  "Link Check", "Run task", and "Scan + analyse" each
  fire `POST /api/ops` with the asserted payload, and "Analyse" with
  `reanalyze`/`allow_cloud` ticked hits the dead fixture Ollama and must
  surface the failures in the job row instead of hanging. "Refresh from
  Ollama" fires `/api/models?discover=1` and surfaces the discovery error in
  the UI.
- **Report**: "Refresh report" posts `/api/report/run` and the snapshot rows
  appear; a column-header click re-sorts the table; the name filter narrows
  rows; per-repo "Refresh this repo" posts `repo=`, "Local checks only" posts
  `no_remote=1`, and "Force remote refresh" posts `force=1`; expanding a row
  renders `repoDetails(repo, snapshot)` content.
- **Shutdown**: "Stop server" posts `/api/shutdown` and the server process is
  asserted to exit (the suite waits for running jobs first — the endpoint
  returns 409 while an op holds the server lock).
- Every page navigation asserts a zero page-error budget: uncaught
  exceptions, `console.error`, and `Log` errors from the application origin
  fail the run (browser-extension noise is filtered out).

The suite exits `0` with a `SKIP` notice when no Chrome/Chromium is found, so
it stays runnable on headless machines; set `KBM_E2E_CHROME` (or
`CHROME_PATH`) to a browser binary. It never contacts Ollama or external
services and deletes its temporary fixture on exit. `window.confirm` is
stubbed via CDP for the apply confirmation — the only dialog in the app.

## Screencast recording (demo videos)

`scripts/e2e/screencast-knowlege.mjs` and `scripts/e2e/screencast-report.mjs`
reuse the same CDP stack to produce demo videos of the knowledge and report
pages. Shared fixture/recording/encode code lives in
`scripts/e2e/lib/screencast-demo.mjs`:

```bash
node scripts/e2e/screencast-knowlege.mjs   # -> docs/assets/screencasts/screencast-knowlege.mp4
node scripts/e2e/screencast-report.mjs     # -> docs/assets/screencasts/screencast-report.mp4
KBM_DEMO_OUT=/tmp/demo.mp4 node scripts/e2e/screencast-knowlege.mjs
```

- Capture is `Page.startScreencast` with `format: "png"` — lossless frames,
  emitted on repaint only (frame count varies per take; wall-clock and video
  time diverge).
- Each `step("name")` in a script becomes an **mp4 chapter**: chapter time is
  frame-index / 2.5 fps, never wall clock; zero-length chapters are dropped.
- Frames persist under `data/screencast-frames/<name>/` (gitignored) so the
  approve-then-convert gap survives reboots.
- The mp4 gets `-movflags +faststart` for progressive playback.
- The fixture corpus is a curated set of themed notes (varied
  titles/tags/status/links) padded with generated filler —
  `KBM_DEMO_DOCS=<n>` controls total size — and `seed -rich` seeds
  suggestions across the expand/move/merge actions and all origins.

After reviewing the mp4, `scripts/e2e/screencast-convert.mjs` derives the
animated web formats — feed it the durable frames dir (lossless source, no
mp4 generation loss) or the mp4 as fallback:

```bash
node scripts/e2e/screencast-convert.mjs data/screencast-frames/knowlege \
    --name screencast-knowlege          # -> .apng .webp .avif .svg
```

Knobs: `--scale 640` (embed width), `--fps` (playback speed, not
smoothness), `--crf 35` (AVIF, SVT-AV1 `-preset 4` via `--preset`),
`--quality 80` (WebP lossy, `-m 6` effort), `--svg-payload webp|avif`.
Benchmarks and the rejected-alternative list are in the engineering-memory
note `workflows/ui-screencast-animated-formats.md`.

The e2e suite is not part of `make check`: it needs a system browser and a
longer runtime. Run it after frontend changes, and in particular after any
edit to Alpine expressions or component methods.

## Frontend dev loop (serve UI from disk)

The UI pages and assets are embedded in the binary, so a rebuild is normally
needed before an edit is visible. Point `KBM_DEV_STATIC` at the static
directory and `serve` reads `index.html`, `knowledge.html`, `report.html`,
and every `/static/` file from disk on each request — save the file and
refresh the browser; no rebuild or process restart:

```bash
KBM_DEV_STATIC=internal/server/static ./KBM-flow serve --config config.json
```

`KBM_DEV_STATIC` is for development only; an unset value or a path that is
not a directory falls back to the embedded assets.

## Test-log audit

`go test` buffers a passing package's output and discards it, so a test can
log warnings or even panic-adjacent noise and still report `ok`. The audit
finds that class:

```bash
make test-logs   # go test -json ./... | go run ./scripts/audit-test-logs
```

It fails on suspicious lines inside otherwise-passing tests — `panic:`,
`WARNING:`, relayed `client error`/`client stack`, `DATA RACE`, fatal
errors. Tests that emit error output on purpose (e.g. the `/api/client-log`
endpoint test) are allowlisted inside `scripts/audit-test-logs/main.go`;
extend the list only after deciding the output is intended. In CI the
coverage job audits the `test-report.json` stream `make coverage-ci`
already produces, so the gate costs no extra test run.

Related drift checks keep test _coverage_ honest structurally:
`TestConfigFieldsHaveBehavioralTests` fails when a config field is not
referenced by any `*_test.go` (JSON key or Go field name) — it proved
`ollama_think`/`ollama_keep_alive` had no coverage until the wire-level
tests were added. Presence is a floor, not a ceiling: the meaningful
behavioral assertion stays the reviewer's job.

## Fault injection

Deterministic fault injection happens at the **data seam**, the same
approach as the knowledge-base-mcp-server chaos suite — not by mocking the
`database/sql` driver. SQLite triggers do the injection inside the real
transaction path:

```sql
CREATE TRIGGER fail_suggestion_insert BEFORE INSERT ON suggestions
BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END
```

Each scenario asserts a **recovery contract**, not just an error:

- `TestSaveTaskFailureLeavesNoPartialState` — an INSERT failure inside
  `SaveTask`'s transaction must roll back the lineage stamp _and_ the
  suggestions: the document stays pending, not half-done.
- `TestUpsertScannedFailureLeavesNoPartialRow` — an UPDATE failure keeps
  the previously committed row byte-identical.
- `TestOpenRejectsCorruptDatabaseFile` — a non-SQLite file at the
  database path fails `Open` cleanly.
- `TestReportEndpointRejectsCorruptStoredSnapshot` — torn JSON in
  `repo_report_snapshot.data_json` (which `SaveRepoReportSnapshot` validates
  at write time, so only direct corruption can produce it) answers 500
  instead of serving corrupt JSON.
- `TestOpenRejectsNewerSchemaVersion` — a `user_version` stamp newer than
  this build fails closed rather than half-migrating a future schema.

Prefer these deterministic injections over wall-clock sleeps or real
signals; new scenarios belong next to the tests they exercise.

## Linting (golangci-lint)

`make lint` runs the curated analyzer set from `.golangci.yml`:
`errcheck`, `govet`, `staticcheck`, `ineffassign`, `unused`, `revive`,
`gocritic`, `gocyclo`, `dupl`, `gosec`, and `bodyclose` plus `gofmt`/`goimports`
formatters. Install the binary with `brew install golangci-lint`; the target
self-skips when it is missing (`LINT_REQUIRED=1` fails closed). The dedicated
CI `lint` job runs the pinned `golangci-lint-action` so the gate always
applies, and also runs an advisory `govulncheck` pass (see below).

Suppressions: prefer fixing the code; document a deliberate skip inline with
`//nolint:<linter> // reason` or `// #nosec <rule> -- reason` rather than
widening `.golangci.yml` exclusions.

## Vulnerability scan (govulncheck)

`make vuln-check` runs a pinned `govulncheck` (`go run
golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...`) and fails on _reachable_
advisories. It needs network access to the Go vulnerability database, so it is
kept out of `make check`, but it is a **hard CI gate**: the Ollama SDK was
replaced with a thin `net/http` client (`internal/ollama` talks generate/show/
embed directly), so the tree no longer carries the server-side code whose open
advisories made the scan advisory-only. Keep it clean — a new reachable
advisory fails the job.

## Semgrep SAST

`make semgrep` runs the same scan as the CI `semgrep` job:
`semgrep scan --config p/golang --config p/security-audit --metrics=off
--error`. It needs a local semgrep CLI (`brew install semgrep` or
`pipx install semgrep`) and network access to semgrep.dev for the registry
rulesets. `.semgrepignore` excludes vendored frontend assets, the
intentionally-odd example corpus, and `data/` runtime state. Both rulesets
currently report zero findings — `--error` makes any future finding block CI.

The CI `container` job also scans the built image with Trivy
(`aquasecurity/trivy-action`, SHA-pinned), failing on HIGH/CRITICAL — this
covers OS packages inside the image, which `govulncheck` cannot see.

Run with coverage:

```bash
go test -coverpkg=./... -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go tool cover -html=coverage.out -o coverage.html
```

`-coverpkg=./...` instruments every package in each test binary. Without it,
a line exercised only by another package's tests reports as uncovered, so
Codecov and SonarQube under-attribute coverage.

Coverage is a guide for finding untested behavior, not a score to maximize.
Prioritize boundary/error cases and safety-critical code (filesystem changes,
database state transitions, and HTTP actions). CI reports coverage but does not
fail the suite on a percentage threshold.

## Coverage services (Codecov / SonarQube)

The CI `coverage` job runs `make coverage-ci`, which produces two artifacts:

- `coverage.out` — the standard Go coverprofile, consumed natively by Codecov
  and SonarQube's Go analyzer (`sonar.go.coverage.reportPaths`).
- `test-report.json` — the `go test -json` stream, consumed by SonarQube as
  test execution data (`sonar.go.tests.reportPaths`).

Both paths live in `sonar-project.properties`; no format conversion is needed.

Required secrets, all optional — the steps degrade gracefully when absent:

| Secret           | Purpose                                                                                |
| ---------------- | -------------------------------------------------------------------------------------- |
| `CODECOV_TOKEN`  | Codecov uploads (tokenless works for public repos via OIDC; the upload never fails CI) |
| `SONAR_TOKEN`    | SonarQube/SonarCloud token — the scan step is skipped when unset                       |
| `SONAR_HOST_URL` | Self-hosted SonarQube base URL; defaults to SonarCloud when unset                      |

For SonarCloud also set `sonar.organization` in `sonar-project.properties` and
use the project key SonarCloud assigns; for self-hosted SonarQube, set
`SONAR_HOST_URL` and change `sonar.projectKey` to match the server-side project.

The tests cover:

- configuration validation, defaults and config-file-relative path resolution
- local and remote repository report collectors (mocked GitHub, Forgejo,
  Sonar, Semgrep, RubyGems and npm registry responses), credential-source
  selection, stale-value retention, Ruby gem and npm package
  inventory/version/policy analysis, Node runtime-pin and npm semver checks,
  original-report fallback, example YAML schemas, SQLite persistence and
  landing aggregates
- HTTP listen address defaults and validation
- prompt composition, including missing prompt files
- Ollama HTTP request (JSON-schema format + `num_ctx` options), error responses,
  malformed replies and request timeouts via `httptest`
- the analysis pipeline end-to-end with a fake LLM (no real Ollama needed),
  including self-move dropping and tag normalization
- the two-pass pipeline: enrichment with a distinct fast model, `kb_policy`
  and provenance/lifecycle patch protection, enrich-only reruns for already-analyzed
  documents, and deterministic unresolved-link fixes (no LLM)
- batched embedding/enrichment/analysis order, stage-boundary model unloading,
  refreshed candidate metadata, enrichment failures, cancellation and live
  privacy changes made after scanning
- local-first metadata derivation with and without a fast model, preservation of
  valid frontmatter, and retention/combination of deterministic proposals when
  full analysis succeeds or fails
- taxonomy allowlist parsing, per-document prompt-scope tag validation,
  scoped-rule fingerprint invalidation, and rejection of globally allowed but
  off-scope tags during generation and apply
- exact entity-match ranking from visible document names and explicit Markdown
  links; curated-catalog target resolution, evidence-dependent reanalysis,
  punctuation-bearing language names, and fenced/indented-code exclusion
- non-blocking enrichment generation failures, independent deadlines, bounded
  output/cutoff rejection, enrichment-only retries after successful analysis,
  missing-field proposal construction, distinct patch outcomes and retained
  per-stage metadata/errors; source failures and live privacy still fail closed
- embedding invalidation for changed source hashes without discarding vectors
  for unchanged documents
- independent CLI/API generation and embedding overrides, verified
  unconfigured models, role/capability rejection, separate cloud embedding
  consent and preservation of server/config-file defaults
- metadata-only Ollama discovery, local/cloud/unknown classification and
  fail-closed capability/privacy checks before any content is sent
- automatic embedding model refresh without generation reruns, exclusion of
  incompatible equal-dimensional cached vectors, atomic audit/lineage rollback
  and canonical managed-block immunity
- dashboard model selection/discovery/consent payloads and stale/offline
  selection handling via `scripts/test-model-selection.mjs` (`npm run check:js`)
- report history round-trip/limit/pruning, per-day knowledge progress
  aggregation, `GET /api/report/history` + `GET /api/progress` payloads, and
  chart series shapes via `scripts/test-knowledge-progress.mjs` and the
  report-history block in `scripts/test-report-filters.mjs`
- i18n catalog drift via `scripts/check-i18n.mjs` (`npm run check:js`): every
  `t('ui.*')` binding in the embedded pages and every `i18n.T("cli.*")` id in
  the CLI must resolve in `internal/i18n/locales/active.en.yaml`, and the
  catalog may not carry dead keys
- drift tests (`cmd/KBM-flow/drift_test.go`, `internal/server/ops_drift_test.go`)
  keeping the option surfaces honest in both directions: `/api/ops` ops map to
  CLI commands and back, every CLI flag and op request field is documented,
  every config JSON key (including `report.*`/`validate.*`) appears in the
  docs, and flags/fields named in the docs exist in the command tree
- template lint (`internal/server/template_globals_test.go`) scanning Alpine
  attribute expressions in the embedded HTML for host globals — the full
  ECMAScript built-in set (`JSON`, `Math`, `Date`, `parseInt`, …) plus DOM
  globals (`window`, `fetch`, `localStorage`, …) — the scoped evaluator
  cannot resolve; the class of bug Go tests and API tests cannot see
- schema-v3-to-v4 preservation of populated documents, embeddings and pending
  suggestions, plus atomic stage-audit/pass writes and retained attempt history
- schema-v14-to-v15 migration adds the independent freshness verifier prompt
  stamp without invalidating existing base freshness lineage
- strict source hashing versus semantic task fingerprints: full-source
  proposal checks remain strict, graph-only link edits do not requeue semantic
  tasks, task prompt/policy changes do, taxonomy edits invalidate only changed
  document projections, and legacy lineages migrate only when the complete
  old recipe is provable (unavailable prior taxonomy bytes never justify
  blind restamping)
- effect-scoped configuration inputs: changing `stub_max_body_lines` queues
  only documents whose stub classification flips; changing a `STUB.md`
  template queues only stubs that use it; verifier-prompt edits do not queue
  ordinary freshness runs, and opted-in verification refreshes saved flags
  without calling the base freshness model again; no-flag verifier changes
  advance the verifier stamp without model/search calls
- missing core prompts abort scanning before document fingerprints are
  written; unavailable optional task prompts affect only their task input, and
  `freshness-verify.md` is required only for `--web-verify`
- freshness proposals preserve tags at both generation and apply, keeping
  freshness fingerprints independent of taxonomy tag-validation changes
- preflight scans on CLI/HTTP task entry points and a live-source guard that
  refuses to send bytes newer than the indexed source snapshot to a model
- frontmatter rendering and proposal normalization: stable `document_id`/title/
  graph/tag ordering, `kbm` last, values/body preserved, and MCP-compatible
  typed fields remain at top level
- managed `kbm` block writes (`kbm.link_check`, `kbm.freshness`): surgical
  byte-preserving updates, sibling-key retention, `os.Root`-confined atomic
  writes, and immunity from pending requeues
- internal and external link checks: body-link extraction (code excluded),
  unresolved-target recording, bounded external probes with
  dead/unverified classification against a local `httptest` server
- per-task model routing: `ollama_task_models` precedence
  (override → external → task default → lane fallback), option lists,
  `ErrNoTaskModel` skips for unconfigured lanes, and config validation of
  unknown tasks/empty lists/blank models
- the optional task passes end-to-end with fake generators: `expand`
  (nearest-ancestor `STUB.md` lookup, refusal without a template, stub-only
  scope, private-doc skips, reviewable suggestion output), `freshness`
  (`kbm.freshness` write, flag extraction, stub/private skips, empty
  `target_path` schema constraint and local rejection, plus the
  `--web-verify` second pass: fake searcher + dedicated verify-model client,
  verified verdicts/`verify_model` in `kbm.freshness`, search-failure
  degradation) and `embed` (lineage
  stamping); plus `/api/models` shape and `/api/ops` task/model validation
- the ops and CLI layers end-to-end against an `httptest` Ollama server:
  `/api/ops` dispatch for `enrich`/`embed`/`expand`/`freshness`/`all`
  (per-lane lineage, managed writes, unconfigured-lane skips, embedding-model
  preservation under a generation-model override) and the `tasks`, `validate`,
  and `check-links` commands (per-task model routing by request model, job
  recording, model capability/remote-consent preflight and `--external`
  rejection without a configured external model)
- fuzz targets over the untrusted parsers asserting invariants, not just
  no-panic: `FuzzParse` (deterministic output), `FuzzCanonicalParts`
  (fm+body always concatenates to the canonical form), `FuzzSetManaged`
  (successful writes are hash-neutral and re-parseable), `FuzzExternalURLs`
  and `FuzzInternalTargets` (every reported target is a verbatim substring
  of the source), `FuzzAssertMutationAllowed` (every `.kb-policy.json`
  rejection wraps `ErrDenied`), `FuzzValidateRewrite` (deterministic, and a
  byte-identical non-blank original is always valid), `FuzzValidateMove`
  (deterministic, identical source/target always rejected) and taxonomy
  `FuzzParse` (a nil error always yields a usable vocabulary).
  Fuzzer-discovered crashers persist in `testdata/fuzz/`
  and replay on every `go test` run; deeper runs use
  `go test -fuzz=FuzzX -fuzztime=30s` locally.
- a malformed-frontmatter matrix: invalid UTF-8, NUL bytes, duplicate keys
  and unclosed flow collections must fail `Parse` _and_ `SetManaged` closed;
  non-map `kbm` shapes stay namespaced; non-UTF-8 bodies pass through
  byte-for-byte
- benchmarks for the hot paths the suite must keep cheap:
  `BenchmarkSignature`/`BenchmarkCosine` (similarity),
  `BenchmarkCanonicalParts` (per-file hashing), `BenchmarkScanRescan`
  (incremental re-scan) — all use the Go 1.24+ `for b.Loop()` form
- `testing/synctest` for the ticker-driven job heartbeat:
  `TestRunHeartbeatTicksUntilStopped` advances the bubble's fake clock with
  `time.Sleep` instead of polling for up to 2s on the real clock. HTTP
  polls of real background jobs (`waitForJob`, model-selection ops) stay
  real-time — they wait on work outside any bubble
- `t.Parallel()` on the pure-function tests in `internal/similarity`,
  `internal/links` and `internal/taxonomy`
- shared proposal guards in analysis and apply, including old accepted rows:
  missing headings/caveats, padded lossy rewrites, regex/indentation corruption,
  invented provenance, invalid/unresolved/traversing graph links, README-to-script
  moves, complete merge-target context and preservation of both merge bodies
- real-database rename resolution and deterministic link repair through apply:
  no-rename/live links are unchanged, genuine repairs remain document-relative,
  and code/privacy fields survive; managed metadata updates are preserved even
  when a stored proposal carries an old or injected `kbm` block
- stale/duplicate acceptance, immediate retirement of old source snapshots,
  independent unique-document applies, persisted apply batches/reasons, and
  the schema-v11 apply-history migration; dashboard result accounting and
  separate reindex-trigger failures via `scripts/test-apply-results.mjs`
- content-similarity sketches: shingle signatures, Jaccard estimation,
  marshal round-trips, and similarity-ranked merge/relation candidates
- the HTTP API: suggestion validation, decide/apply, embedded UI assets,
  background op and report jobs, job-endpoint isolation, 404/400/503 responses,
  landing/knowledge/report page routing, dashboard summaries, and server
  startup/shutdown (stale-job recovery, bind errors)
- frontmatter parsing and whitelist validation (fences, BOM/CRLF, malformed
  YAML, enum and date checks, link-path rules), per-document storage,
  `no_llm_context` fail-closed behavior, migration of legacy `documents`
  tables, and `mcp_server` defaults/rejection
- recursive scanning, MCP hidden/workflow-path exclusions, SHA change
  detection, case-insensitive extensions, trash exclusion and path tags
- SQLite document/suggestion lifecycle, job history and filtering, stale-job
  recovery, report snapshots, and the legacy jobs-table migration
- safe path validation, symlink containment (files and parent directories),
  trash-directory validation, and re-hashing an accepted suggestion's source
  before apply
- apply success and failure paths for expand, move, delete and merge, including
  concurrent applies, temporary-file cleanup, MCP mutation-policy denial and
  protected `.kb-policy.json` files
- Cobra command registration, argument validation, missing-config failures, and
  an end-to-end accept/reject/apply run
- the version scripts (`bump-version.mjs`, `sync-version-metadata.mjs`,
  `check-version.sh`) exercised against a throwaway metadata copy by
  `npm run version:test` / `make version-test`

The Ollama test uses a local mock HTTP server and therefore does not require a real model.

The frontend uses pinned npm dependencies. With Node.js 26.12+ and npm
(`.node-version`), check the vendored Bulma/Alpine assets, JavaScript syntax,
CSS lint, and formatting with:

```bash
make frontend-check
```

CI also runs `npm audit --audit-level=high` against the locked frontend tools.

After a reviewed npm dependency update, refresh the embedded files with
`npm run assets:vendor`, update `THIRD-PARTY-NOTICES.md` if a version changed,
and commit those changes with `package.json` and `package-lock.json`.

## Quality gates and git hooks

`make quality` runs the full local gate — `check`, `frontend-check`,
`semgrep`, `vuln-check`, `coverage-ci`, and `sonar`. It needs network
access (semgrep rulesets, the Go vuln database, sonarcloud.io). The
`sonar` target uploads through the local `sonar` CLI when `SONAR_TOKEN`
is exported and skips with a note otherwise; CI remains the
authoritative scan. `sonar-project.properties` binds the
`gundestrup_Knowledge-BA46-Flow` project, organization and cloud host.

Git hooks live in `hooks/` and are activated once per clone with
`make install-hooks` (sets `core.hooksPath`). `pre-commit` runs
staged-scope gofmt, `go vet`, prettier and semgrep on staged files;
`pre-push` runs `check`, `frontend-check`, `semgrep` and `vuln-check`.
`SKIP_HOOKS=1` and `SEMGREP_SKIP=1` are the escape hatches.

## In-app help and job progress

The dashboard's `?` buttons open fragments of
`internal/server/static/usage.md` — split at `### slug` headings into
paragraph/list blocks rendered via `x-for`/`x-text` (no `x-html`, so no
sanitization under the strict CSP). `docs/USAGE.md` symlinks to it —
edit the static copy; `go:embed` cannot follow symlinks or read outside
the package tree.

Both pages show a "Background work" card merging `/api/ops` and
`/api/report/jobs` — the ops manager is single-slot, so a job from
either lane blocks the other and must be visible. Detail lines carry a
global `k/T` prefix plus a phase `i/N` (`"12/27: remote 3/12: repo —
sonar"`): the first bar accumulates total progress, the second tracks
the current operation. `busy`→idle clears the started-notice and
triggers one final refresh.
