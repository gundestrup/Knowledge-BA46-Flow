# Functions and data flow

## `scan`

Scans the configured extensions recursively (minus `exclude_extensions`),
skips dot-prefixed entries, the Knowledge Base MCP Server's `logs/`, `tmp/`,
`_tmp/` and workflow-sidecar paths, plus the configured `exclude_directories`
and root-anchored `exclude` paths/patterns, then computes SHA-256 and a content-similarity
sketch (word 3-shingles, bottom-k minwise signature) in the same streaming
pass and syncs SQLite. Changed content invalidates its cached embedding; a later
analysis backfills it, while unchanged documents keep their vectors.
No Ollama calls and no file changes.

The stored `sha256` is the _canonical_ content hash: the document with the
tool-owned `kbm` frontmatter block excised byte-for-byte. `kbm` holds
operational metadata KBM-flow writes itself (link-check verdicts and similar),
so updating it never marks a document changed, never requeues analysis and
never invalidates an accepted suggestion — `apply` verifies the same canonical
hash. Documents without a `kbm` block hash exactly as their raw bytes always
did, so existing databases see no churn.

Scan also stores the canonical hash **split at the frontmatter boundary** —
`fm_sha256` over the frontmatter (minus `kbm`) and `body_sha256` over the
body — so a frontmatter-only edit is distinguishable from a content edit at a
glance. The freshness task keys its lineage on the body hash (metadata churn
is not staleness), the dashboard labels documents "metadata changed" vs.
"content changed" against the `seen_*` hashes stamped at the last analysis,
and every hash check stays a fast local operation that targets reprocessing
at the documents that actually changed.

Every scan also runs the **mandatory internal link check** — no model, no
network: `[text](target)` body links (fenced/inline code excluded) resolve
against the scanned document set for `.md`/`.markdown` targets or against the
repository filesystem for assets. Unresolved targets land in the document's
`link_issues` column, surface in the analysis prompt as "Link check:" lines
(so the model may propose a fix) and display in the dashboard. Frontmatter
`related`/`contradicted_by` targets were always checked by the deterministic
repair; this extends the same guarantee to body links.

Files the scanner no longer sees are marked `missing` rather than deleted —
the row (and its history and suggestions) survives a temporary move or a
git branch switch. Pending and accepted suggestions for missing documents
stay out of the review and apply queues while the file is gone; if the file
reappears unchanged its status — and those suggestions — resurface.

A moved or renamed file is recognized by its canonical hash: when content
seen at a new path matches exactly one document that vanished in the same
scan, the database row is re-keyed — analysis stamps, embeddings, and
suggestions follow the move (an unchanged file is never re-analyzed for a
rename alone). A move combined with an edit, an ambiguous match (several
vanished files with identical content), or a live duplicate at the old path
is treated as a separate document instead.

Each detected rename is also recorded in a `renames` table. The next
pipeline run — no model needed — finds documents whose `related`/
`contradicted_by` entries or body links still point at the old path and
proposes a deterministic `expand` that retargets them to the new location
(following move chains to the live endpoint) instead of dropping the links
as unresolved. Like every suggestion it waits for a human accept.

## `check-links`

The optional **external** link check — an explicit opt-in (`KBM-flow
check-links`, the dashboard's _Check external links_ button, or
`{"op":"link-check"}` in `/api/ops`). It extracts every `http(s)://` URL from
document bodies (fenced/inline code skipped), probes each unique URL once
with a bounded client (HEAD, GET fallback for 405/501, 20s timeout, 8
workers), and records the verdict per document:

```yaml
kbm:
  link_check:
    at: "2026-10-20T07:15:00Z"
    dead: ["https://example.com/old-doc"] # 404/410 — definitive
    unverified: ["https://bot-walled.example/x"] # 401/403/429/5xx/timeout
```

Only unambiguous `404`/`410` count as dead; auth walls, rate limits and
network errors stay `unverified` so a person — not the network weather —
decides. Writing `kbm.link_check` is the single documented exception to "only
`apply` mutates files": the `kbm` block is tool-owned operational metadata,
confined writes still go through `os.Root` + atomic rename, and the canonical
hash means the write is invisible to the pipeline's change detection. The
stored `frontmatter_json` is refreshed in the same step so the dashboard
shows the check immediately. This op sends no document content anywhere — it
only fetches the URLs the notes already reference; destination hosts see a
plain HTTP request from this machine.

## Optional task passes: `enrich`, `embed`, `expand`, `freshness`, `tasks`

Beyond the `scan`/`analyze`/`run` cycle, five focused passes run individual
LLM tasks. Each is incremental against its own lineage columns (per-document
`<task>_sha256`/`<task>_model`), accepts `--reanalyze`, `--reanalyze-all`,
`--model <name>` (per-run override — a configured or installed model).
`--external` is available on `expand`, `freshness` and `tasks`, and
`--web-verify` on `freshness` and `tasks`. `enrich` and
`embed` accept `--allow-cloud` for explicitly selected remote models. Model
capability and privacy are checked before generation. The dashboard's task +
model pickers run the same ops via
`{"op":"enrich"|"embed"|"expand"|"freshness"|"all", ...}` in `/api/ops`.

- **`enrich`** — the fast-model metadata pass (title/tags) on its own, without
  analysis. Lane default: `ollama_model_fast`.
- **`embed`** — recompute embedding vectors for documents whose canonical hash
  moved past `embedded_sha256` or whose producing model differs. Uses an
  independent `--embed-model`, the `ollama_task_models.embed` default or
  `ollama_embed_model`; standalone `--model` is an alias for `--embed-model`
  (use only one). Compatible installed models can be selected without adding
  them to configuration. The standalone op fails loudly on transport errors
  instead of degrading like the in-pipeline backfill.
- **`expand`** — grows placeholder stubs into reviewable documents. Only stubs
  (`stub` tag or minimal body) are candidates, and each stub is expanded
  against the nearest ancestor `STUB.md` template (walking the stub's
  directory up to the repository root) — the category skeleton keeps expansion
  concrete instead of Wikipedia-shaped. A stub with no applicable `STUB.md`
  is refused, not improvised. The proposal arrives as an ordinary `expand`
  suggestion through the same proposal guards; `STUB.md` files are excluded
  from scanning and validation. Lane default: `ollama_model_external` when
  configured, else `ollama_model`.
- **`freshness`** — a heuristic currentness review: the model reads the note
  plus its modification date and `kbm.link_check` result, then flags claims
  that may be dated (version pins, "latest release", ageing dates). Without
  further options it cannot verify live facts — flags mean "check this",
  never "this is dead". The
  verdict, timestamp and flag list are recorded in the note's
  `kbm.freshness` block — tool-owned operational metadata, the same
  documented carve-out as `kbm.link_check`, so the write never invalidates
  the content hash — and concrete corrections arrive as normal `freshness`
  suggestions. Lineage stamps `body_sha256`: frontmatter churn does not
  re-date content. Lane default: `ollama_model_external` when configured.
  With `--web-verify` each flag's claim is sent as a query to Ollama's hosted
  web search API (bounded to 8 claims × 5 results per document) and a second
  model pass re-judges every flag against the evidence: `stale`, `current`
  or `unverifiable` verdicts land in `kbm.freshness` with the evidence and
  source URL, `verified: true` and `verify_model` mark a completed check, and
  corrections the evidence supports arrive as ordinary suggestions. The
  second pass is its own model lane — `ollama_task_models.verify` when
  configured, else the external lane, else `ollama_model` — so a different
  model can verify than the one that flags. Only claim text — never
  the document — leaves the machine, `no_llm_context` documents are skipped
  before any call as usual, and a search or verdict failure degrades to the
  unverified first-pass result (`verify_error` in the block) rather than
  failing the document. Requires `OLLAMA_API_KEY` in the environment or a
  `.env` file next to the config; `tasks --web-verify` applies the same
  stage in the batch run.
- **`tasks`** (op `all`) — run every configured LLM task in one job, cheapest
  first: enrich → embed → expand → freshness → analyze. Tasks without a
  configured model are skipped unless `--model` is given, in which case one
  model runs every generation stage. The `embed` stage uses its independent
  `--embed-model` override or configured default; chat models cannot produce
  vectors. An unconfigured embedding lane is skipped even with a generation
  override.

Model routing resolves in order: per-run `--model`/`"model"` override →
`--external` (for lane-capable tasks) → `ollama_task_models.<task>` default →
the lane fallback. Every stage result records the model that produced it, so
`--reanalyze` model drift works per task. `no_llm_context` documents are
skipped by every task without an LLM call; stubs additionally skip freshness.

For `analyze`, `run` and `tasks`, embedding resolution is independent:
`--embed-model` → `ollama_task_models.embed` default → `ollama_embed_model`.
Both explicit choices are capability/privacy-checked through Ollama metadata
before work starts. Configured defaults are checked lazily before sending
content, preserving offline no-op passes. Cloud generation requires
`--external`; embedding/enrichment consent is separately `--allow-cloud`.

Embedding model changes refresh the visible corpus automatically, even with
no pending analysis and without `--reanalyze`. Vector and lineage updates are
atomic, and incompatible cached vectors never enter the same ranking after a
failed refresh. This does not requeue generation; use `--reanalyze-all`
explicitly if suggestions should be reconsidered with the new retrieval
context.

## `validate`

The read-only structural gate — same rules the shelf README documents as the
repository conventions. Walks the repository with the same scoping as `scan`
(`exclude_directories`, `exclude`, MCP ingest exclusions, dotfiles and
symlinks skipped) plus the `validate.exempt` scope for meta/scratch paths.
For every in-scope note it reports:

- malformed frontmatter, a missing `tags` list, tags outside `taxonomy.yaml`,
  zero or multiple kind tags, and dependent kinds (`framework`, `library`,
  `cms`) without a language subject — index `README.md` files are exempt
  from the language rule since they aggregate subjects;
- a missing index back-link (the note never mentions `README.md`), a missing
  index ancestor, or an index that does not name the note — the nearest
  ancestor `README.md` shadows parents, so `libraries/ruby/README.md` owns
  `libraries/ruby/*` while `databases/README.md` owns `databases/*/*.md`;
- missing language↔catalog edges: entries and index READMEs under
  `libraries/<lang>/` and `frameworks/<lang>/` must reference
  `languages/<lang>.md`, and the language note must reference each existing
  catalog;
- non-Markdown files inside a note tree that no note names (scripts and
  assets are snippets, not standalone notes).

Exits non-zero listing every violation when any exist. No Ollama calls, no
database, no file changes.

## `analyze`

Finds rows where `sha256 != analyzed_sha256`, reads the document, composes prompts, calls Ollama and stores title, summary, tags and suggestions. Each analysis records the producing model in `analyzed_model` (shown in the dashboard's status column and in job summaries).

Analysis runs in up to three stages per document:

1. **Deterministic frontmatter fixes (no LLM).** `related`/`contradicted_by`
   entries that are invalid repository-relative paths or resolve to no known
   document are removed. The result is stored as a normal `pending`
   `expand` suggestion (`origin: deterministic`, confidence 1.0) — the
   reviewer accepts or rejects it like any model suggestion; nothing is
   auto-applied.
2. **Local-first enrichment.** `enrichment_mode: local-first` runs with or
   without a distinct `ollama_model_fast`. It preserves valid existing
   frontmatter and derives only missing title/tags when an H1 and specific
   shelf path provide clear evidence. Proposals stay pending; a successful
   full-analysis patch/body expansion consolidates compatible local fields.
   If a fast model is configured, ambiguous metadata falls through to the
   bounded `prompts/enrich.md` pass; `enrichment_mode: always` forces that
   pass for comparison.
   A root `taxonomy.yaml` restricts locally derived and newly proposed tags to
   the human-approved vocabulary. The same check runs again when applying an
   accepted suggestion, including suggestions stored by older binaries.
3. **Analysis pass (`ollama_model`).** Every document still needing semantic
   analysis receives the full `prompts/analyze.md` task; local shortcuts do
   not mark it analyzed. Candidate ranking combines local exact-name matches,
   embeddings and content overlap, and candidate annotations identify exact
   terms mentioned in the current note as hints, not proof of a relationship.
   Aliases come from visible document leaf paths, frontmatter titles, H1
   headings, explicit local Markdown links, and the language/framework
   inventories in `frameworks/README.md` and linked catalog READMEs. Matches
   in fenced or indented code and `no_llm_context` documents are excluded.
   The matcher is a small Go standard-library
   implementation, not an external NLP runtime. Bounded target excerpts are
   hints only; merge still requires the complete target.
   When `ollama_embed_model` is configured, stored
   embedding vectors (backfilled lazily at analysis time) drive cosine
   relatedness — semantic "same topic" matching; a bottom-k minwise sketch
   over word 3-shingles computed during `scan` provides literal-overlap
   similarity and the ≥50% probable-duplicate flag. Both blobs live in
   SQLite, no FTS dependency. `no_llm_context` documents are never embedded
   or shown as candidates, but still count as valid link targets. The prompt
   lists at most 16 ranked candidates. At most two high-overlap candidates
   are also supplied in full within a shared 12 KB body budget; only these
   may be merge targets. Partial target bodies are never sent as complete.

When scan finds a new or changed visible note, the analyzer also refreshes
unchanged notes with a direct mention/link to that evidence, a high-overlap
duplicate match, or an unusually close embedding neighbor. The embedding arm is
rank-based rather than an absolute score: each changed note contributes at most
its 16 nearest neighbors, and only pairs that also clear the 95th percentile of
the corpus's own sampled cosine distribution — calibrated per run against the
stored vectors, since cosine thresholds are not portable across embedding
models. This dependency refresh is local; it does not claim that a missing
lexical match proves the documents are semantically unrelated.

Stages run in batches, not alternating models per document: backfill embeddings,
enrich the eligible corpus, refresh candidate metadata, then run full analysis.
The Ollama clients unload completed embedding/enrichment stages between batches,
while retaining normal per-request caching within a batch. Optional enrichment
generation failures, including malformed replies and output cutoffs, are recorded
as retryable warnings and do not block full analysis. Source/read/parse failures,
privacy restrictions, cancellation and systemic prompt-loading errors do not use
that fallback. Live privacy is checked again before analysis.

Enrichment defaults to a separate 120-second deadline and 512 output tokens
(`enrichment_timeout_seconds`, `enrichment_max_tokens`), always with `think:false`.
Analysis retains `request_timeout_seconds` and its existing output budget.
Failed enrichment does not stamp a successful hash: a later incremental run
retries it without repeating full analysis if that already succeeded. Summaries
separate optional enrichment failures from terminal document failures.
A late enrichment-only retry preserves the successful full-analysis metadata
in `documents`; its newly extracted metadata is retained in `stage_results`.

The full-analysis reply also accepts `frontmatter_patch` for `title`, `tags`,
`related` and `contradicted_by`. Linking work no longer requires regenerating
the entire document. Other metadata is read-only in both model stages.
Patches with invalid values or unresolved links are rejected, not merely annotated.

Body changes are conservative and additive: existing words must remain in
order (whitespace reflow outside code is allowed), and fenced/inline code must
remain verbatim apart from CRLF normalization. Merges must retain both sources;
incompatible protected metadata needs manual resolution. Moves must preserve
the document extension, have an absent target and no proposed content;
conflicting moves of one document are rejected. Rejected edits are counted in
progress/job summaries with per-document reasons. This is structural protection,
not a factual verifier of new prose or model reasoning.

Each stage's suggestions carry an `origin` marker (`analysis`, `enrich`,
`deterministic`) shown as a tag in the review UI; re-running a stage
supersedes only the pending suggestions it regenerates.
Suggestions from different stages may concern the same source file. Review
them individually; applying one changes the source hash and makes a second
proposal based on the old file stale rather than silently overwriting it.

With `--reanalyze` (CLI) or `reanalyze: true` in the `/api/ops` body — a checkbox in the dashboard — documents whose `analyzed_model` differs from the configured `ollama_model` are reprocessed too: a different model may produce different suggestions, so redoing the corpus is an explicit choice, not automatic. Each re-analysis supersedes that document's still-`pending` suggestions (accepted/rejected decisions are kept). The landing page's _analyzed with another model_ metric counts how much of the corpus would drift under the current model.

`--external` (or `external: true` in `/api/ops`) runs the same pipeline as an
enhanced lane: the analysis pass uses `ollama_model_external` instead of
`ollama_model`. Results stamp `external_sha256`/`external_model` on each
document rather than `analyzed_*`, so the local lineage is untouched — a
following normal run does not see the corpus as new or model-drifted, and the
external lane is incremental against its own markers (unchanged documents are
skipped on a second external run; `--reanalyze`/`--reanalyze-all` apply per
lane). Enrichment stays the shared local stage in both lanes. Suggestions land
in the same review queue (origin `analysis`) and supersede a document's pending
analysis suggestions whichever lane produced them — accepted/rejected history
is kept. An external model receives document content, so enabling the lane
configures it, but only an explicit flag ever invokes it;
`no_llm_context` and stub documents are skipped in every lane.

Use `--reanalyze-all` (or `reanalyze_all: true` in `/api/ops`) only when a
prompt or taxonomy policy change warrants a complete refresh. It forces both
enrichment and full analysis for every non-missing document and can be very
expensive on local models. Normal scans instead refresh only unchanged notes
whose direct or high-confidence evidence dependencies changed.

Documents whose frontmatter sets `kb_policy.no_llm_context: true` are not read
for prompt construction or sent to Ollama. An invalid `kb_policy` also fails
closed and skips analysis. The skip is recorded locally so an unchanged
document is not retried on every run.

A document tagged `stub` is a placeholder — typically an entity note like
`frameworks/x.md` that only links to the language or catalogue it belongs to.
Notes need not carry the tag explicitly: a document with at most
`stub_max_body_lines` non-empty body lines (default 5; negative disables)
counts as a stub too. Unlike `no_llm_context` a stub stays fully LLM-visible:
it is embedded, ranked as a candidate, and accepted as a link target. But
every generation stage skips it (local enrichment, fast enrichment and full
analysis), the skip is stamped so the note is not retried each run, and the
deterministic unresolved-link repair still runs so its links stay honest.
Declare `stub` in `taxonomy.yaml` as a non-kind tag (e.g. under `function`)
so it survives tag validation; the note keeps its normal kind tag alongside
`stub`.

Ollama is called via `/api/generate` with `format` set to a JSON schema and
temperature `0`. Structured output constrains syntax, not semantic correctness.
Responses reporting a length/context generation cutoff are rejected.

Per-document progress is reported through `analyzer.RunProgress`: the CLI
prints `[i/N] path` plus the outcome (`enriched`/`locally_enriched`/
`enrichment_error`/`analyzed`/`skipped`/`error`), and HTTP
`analyze`/`run` jobs keep their `detail` updated while `running` so the
dashboard's polling shows which document is in flight rather than a silent
"In progress…".

All `scan`/`analyze`/`run` executions — CLI and HTTP — record a durable job
row with a heartbeat refreshed every 15s. Starting a second op while a live
job runs is refused (CLI error, HTTP 409), and `/api/apply` is refused too so
file mutations never race a background run. A heartbeat older than 90s marks
the job `failed` ("interrupted (stale heartbeat)"), so a killed process can
never block future runs. The dashboard polls `/api/ops` every 1.5s while busy
and every 10s while idle, and live-refreshes documents and suggestions during
an active job.

CLI SIGINT/SIGTERM cancels in-flight requests and lets job completion record
the interruption. Already stored suggestions remain available; nothing is applied.

## `run`

Runs `scan` followed by `analyze`, and records a completed or failed full-cycle
job. The landing page uses the latest successful `run` to calculate the age of
the last cycle.

## `report`

Refreshes the separate, read-only repository health report from the YAML paths
in `report.registry_path` and `report.policy_path`. It checks local Git and
repository state, remote hosting/scanner services, Ruby gem fleet, and Node/npm
package/runtime versions. It does not run `kb-sync` or write to registered repositories. Remote checks
default on for configured registries; `report --no-remote` keeps last-known
remote values while refreshing local checks. `--repo`, `--force`, and `--json`
control single-repository refresh, cache bypass, and output. `--remote`
enables the remote collectors for one run even when `report.remote_enabled`
is `false`; it cannot be combined with `--no-remote`. The optional
`report.comparison_path` is a read-only Ruby-output reference for cutover
validation. The latest snapshot and RubyGems/npm/Node release caches are stored
in SQLite.

## `list`

Shows all suggestions with status `pending`.

## `review`

Interactive walkthrough. `y` accepts, `n` rejects — followed by an optional
one-line "why" stored as reviewer feedback — `q` stops, any other input skips
the suggestion for now.

## `accept ID` and `reject ID`

Makes it possible to decide a single suggestion without interactive review.
`reject ID --note "why this is wrong"` stores a short reviewer reason with the
decision. Rejected proposals — including their notes — are shown back to the
model as context on later analysis, expand and freshness runs for that
document, and an identical re-proposal is suppressed instead of queued again.

## `apply`

Executes only suggestions with status `accepted`.

- `move`: moves without overwriting an existing destination file.
- `expand`: atomically replaces the source document with `proposed_content`.
- `merge`: replaces the target file with merged content and moves the source file to `.kbm-trash`.
- `delete`: moves the source document to `.kbm-trash`.

Before each action, `apply` re-hashes the source file. If it changed after LLM
analysis — or no longer exists — the suggestion is marked `superseded` and
not applied.

`apply` also revalidates old accepted suggestions: protected metadata must be
unchanged, frontmatter values must be valid, graph links must resolve to regular
configured document types inside `os.Root`, and both merge bodies must survive.
Body rewrites that remove text or change existing code fail before any write.
These checks can reject an old suggestion that was previously accepted.

Before mutating any source or destination, `apply` checks the applicable
`.kb-policy.json` file. `{"mutations":"deny"}` blocks the operation;
malformed/unsupported policy files fail closed, and KBM-flow never changes the
policy file itself.

After `apply`, `run` should be executed again so SQLite reflects the new files and hashes.
If the same shelf is indexed by the Knowledge Base MCP Server, its FAISS
index must be refreshed after changes. Set `reindex_trigger_path` (see
`docs/CONFIGURATION.md`) to a repository-relative file and point the server's
`REINDEX_TRIGGER_PATH` at it — `apply` then touches the file after every
successful batch and the server reindexes automatically. Without it, run
the server's `kb reindex` command manually.

## `serve`

Starts an HTTP server on `http_addr` (default `127.0.0.1:8080`) with the
summary landing page at `/`, knowledge review at `/knowledge`, and separate
repository report at `/report`. Review/apply endpoints:
`GET /api/documents`, `GET /api/suggestions[?status=]`,
`GET /api/suggestions/{id}`, `POST /api/suggestions/{id}/decide`,
`POST /api/apply`. Long-running operations run as background jobs persisted in
the `jobs` table (history survives restarts): `POST /api/ops`
(`{"op":"scan|analyze|run"}`), `GET /api/ops`, `GET /api/ops/{id}`.
Report endpoints are `GET /api/report`, `POST /api/report/run`,
`GET /api/report/jobs`, and `GET /api/report/jobs/{id}`.
`POST /api/shutdown` stops the server gracefully from the dashboard's Stop
button — loopback clients only; it returns `409` while an operation is
running unless `?force=1` is sent (the orphaned job is then swept to
`failed` by its frozen heartbeat).
`GET /api/dashboard` serves the two landing-card summaries.
See `docs/API.md`.

The UI is built with locally embedded Bulma CSS and Alpine.js. It uses normal
JavaScript `async`/`await` methods for API calls and job polling; no CDN or
frontend build step is needed.

## `version` and `--version`

Print the application version embedded in the binary. `VERSION` is the single
source of truth; `make build` and Docker builds inject it. Direct `go run`
builds report `dev`.

## Prompts

- `prompts/base.md`: shared role, constraints and safety rules.
- `prompts/<mcp_server>.md`: conventions of the configured target server —
  `knowledge-base-mcp-server.md` documents the frontmatter whitelist, allowed
  values and usage rules for the Knowledge Base MCP Server.
- `prompts/analyze.md`: the ordered analysis passes and expected JSON schema.
- `prompts/enrich.md`: the fast enrichment pass contract — metadata plus a
  `frontmatter_patch` merge, used only when `ollama_model_fast` is configured.
- `prompts/expand.md` and `prompts/freshness.md`: the task contracts for the
  `expand` and `freshness` passes.
- `prompts/freshness-verify.md`: the second freshness pass — re-judges each
  flagged claim against web search evidence, used only with
  `freshness --web-verify`.

Prompts can be changed without recompiling — e.g. to tune them for a different `ollama_model`.

## Frontmatter

Documents may start with a YAML `---` frontmatter block. During `scan`, the
block is parsed and checked locally against the field whitelist used by the
[knowledge-base-mcp-server](https://github.com/jeanibarz/knowledge-base-mcp-server)
(`title`, `tags`, `status`, `review_status`, `tier`, `confidence`,
`last_verified_at`, `contradicted_by`, `kb_policy`, `related`, and the
provenance fields). Parsed fields and validation issues are stored on the
document row (`frontmatter_json`, `frontmatter_issues`) and shown to the LLM
as analysis context — including `related`/`contradicted_by` entries that do
not resolve to a known document.

The model fixes frontmatter through normal `expand`/`merge` suggestions:
`proposed_content` is the complete document including the corrected `---`
block. Proposals with malformed frontmatter YAML are dropped during analysis
and rejected again at apply time; non-fatal value issues are annotated in the
suggestion's reason for the reviewer. Frontmatter-declared `tags` are merged
into the document's stored tags.

For MCP compatibility, configure `repository_path` to one shelf directory
under `KNOWLEDGE_BASES_ROOT_DIR`. If the parent shelves directory is selected
instead, KBM-flow can see root-level Markdown files that the MCP server does
not index, and shelf-specific write policies are harder to reason about.

## `prune`

Bounds the `jobs` table — every `scan`/`analyze`/`run`/`report` appends a row,
and nothing else deletes them. New job creation automatically keeps only the
newest 200 finished rows; `KBM-flow prune --keep N` applies a tighter bound on
demand and also sweeps stale `running` rows (frozen heartbeats) to `failed`
first, so `prune` doubles as a job-table health cleanup. `--trash-days N`
additionally removes trash entries older than N days (0 keeps everything).

## SQLite

`documents` holds file state, hash, analysis hash, metadata, frontmatter and
errors.
`stage_results` records each new enrichment/analysis attempt independently:
source hash, model, outcome, patch disposition, parsed model metadata/patch,
locally derived missing fields, rejected edits, elapsed milliseconds and errors.
Successful records commit atomically with that pass's metadata and suggestions.
Later analysis may replace `documents` metadata, but not the enrichment record.
Optional errors remain visible here even after full analysis succeeds.

Patch dispositions are `missing` (no patch object), `empty` (no operations),
`unchanged` (operations made no effective change), `derived` (local metadata
available before semantic review), `suggested` or `rejected`.
`derived_fields_json` records title/tags inferred from an H1 and known shelf
path. On a full analysis, those values are combined with a compatible metadata
or additive-body proposal to avoid duplicate review actions; an enrichment-only
retry offers them as a deterministic suggestion.
Rejections remain recorded even when other patch fields produced a valid proposal.
The records contain parsed responses, not raw prompts or document bodies.

```sql
SELECT stage, outcome, patch_outcome, count(*) AS attempts
FROM stage_results GROUP BY stage, outcome, patch_outcome;
SELECT document_path, model, error FROM stage_results WHERE outcome='error';
```

Schema v4 adds this table without rebuilding populated document/suggestion
tables. Opening an existing database preserves embeddings, pass markers and
review decisions; historical model replies cannot be recovered or backfilled.
Existing successful hashes are not automatically reprocessed after an upgrade.

`suggestions` holds LLM suggestions and review status. The latest repository
report is stored as one JSON snapshot (no accumulation); report execution
history and the other operations use the durable `jobs` table, retained at the
200 newest finished rows (`prune` for a manual cut). The database is state and
coordination — not the authoritative knowledge source. Markdown and YAML
remain the source of truth.
