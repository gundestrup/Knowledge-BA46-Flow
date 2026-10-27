# HTTP API and web UI

`KBM-flow serve` starts an HTTP server with a landing page and two independent
workspaces. Open `http://127.0.0.1:8080` for the summary cards, `/knowledge`
for the analysis/review UI, and `/report` for the full repository report.
The page and its Bulma/Alpine assets are embedded in the binary, so it works
offline without browser requests to public CDNs.

The listen address is set via `http_addr` in the config (default
`127.0.0.1:8080`). **Security:** `/api/apply` mutates the filesystem, so the
default binds to loopback only. Setting `0.0.0.0` or a LAN address exposes
those mutations to the network — there is no authentication layer.

For Docker, run `docker compose run --rm --service-ports kbm-flow serve`.
The host port remains bound to `127.0.0.1:8080` by the Compose mapping.

All endpoints return JSON. Error responses are `{"error": "..."}`.

## Endpoints

### `GET /api/dashboard`

Returns `changed_files`/`model_drift` for the local lane and
`external_changed_files`/`external_model_drift` for the external lane, plus
the last successful `run` cycle and age in days and the latest repository
snapshot summary. Lane counts are not combined: an external pass does not
silently mark the local lane complete.

### `GET /api/documents`

Every tracked document with analysis metadata (path, title, summary, tags,
status, strict source hash, semantic fingerprint, task input fingerprints and
lineage hashes/models). Ordered by path.

### `GET /api/progress`

Per-day pipeline counts derived from the immutable timestamps on documents
and suggestions — `{"days": [{"date", "notes", "analyzed", "suggestions",
"handled", "applied"}]}` sorted by date. `notes` counts first-seen documents,
`handled` counts suggestions that left the review queue (accepted, rejected,
superseded, or apply-failed). Powers the cumulative progress chart on
`/knowledge`, where the review-backlog line is `suggestions − handled` — an
approximation of suggestions still awaiting review on each date.

### `GET /api/suggestions?status=pending`

Suggestions filtered by status. `status` may be `pending`, `accepted`,
`rejected`, `applied`, `failed` or `superseded`; default is `pending`.
`proposed_content` is omitted from list items (it can be large) — use the
detail endpoint.
Items include persisted `error` details and, after an apply attempt,
`apply_batch_id`, `apply_attempted_at`, and `applied_at` on success. Older
rows may have no apply-history fields.
Active items also include computed `change_types`, `document_id`, `review_state`,
`review_reason` and, for a competing accepted alternative, `accepted_id`.
Historical expansion types are `unknown` because their original before-image
was not archived. The UI groups by note, retaining alternatives when a
type/origin facet matches. This is a projection, not automatic merging.

### `GET /api/suggestions/{id}`

Full suggestion row including `proposed_content`, plus `current_content` —
the file on disk right now, read os.Root-confined and capped at
`max_document_bytes`, so the review UI can render a diff. A read failure is
reported in `current_content_error` instead of failing the request. `404` if
the id does not exist.

### `POST /api/suggestions/{id}/decide`

Body: `{"status": "accepted"}` or `{"status": "rejected"}` — the HTTP
equivalent of `accept ID` / `reject ID`. An optional `"note"` string stores a
short reviewer reason on rejections; it is kept on the row (returned as
`decision_note` by `GET /api/suggestions/{id}`) and shown back to the model as
review feedback on later runs for that document. Returns `409` if the
suggestion is not pending or does not exist, its source has changed, its
expansion is invalid, or another suggestion for that document is already
accepted. Stale sources become `superseded`; conflicting alternatives remain
pending. Rejections do not require a readable/current source.
`{"status":"pending"}` withdraws an existing acceptance; it is not a way to
resurrect applied/rejected/superseded records. Permission blocks return `409`
without accepting the proposal.

### `POST /api/apply`

Executes every `accepted` suggestion — the HTTP equivalent of `apply`.
Returns a result per suggestion:

```json
[{ "id": 3, "action": "move", "status": "applied", "applied": true }]
```

Each result has `status` `applied`, `failed`, `superseded` or `blocked`. A
write-scope or mutation-policy block leaves the row accepted, records the
reason, and performs no write. A source changed
since its analysis is marked superseded and left untouched. Other suggestions
still run (same semantics as the CLI). When `reindex_trigger_path` is
configured and at least one suggestion applied, a final
`{"action": "reindex-trigger", ...}` row reports the trigger-file touch for
the MCP server's reindex watcher.
Suggestion results also include `document_path`, `apply_batch_id`, and
`apply_attempted_at`. The batch ID groups results from one request and is
persisted on each attempted suggestion, including failures and supersessions.
The dashboard exposes per-document results and does not count the
`reindex-trigger` row as another applied/failed suggestion; trigger failures
are reported separately. Successful writes immediately supersede other
pending/accepted snapshots of that source's old content, with an explanation
identifying the applied suggestion.

### `GET /api/reconcile`

Read-only preview of every pending/accepted proposal, including missing sources.
Returns `token`, `write_scope`, `counts`, `retire_count`, and `items` containing
IDs, paths, current source hashes, change types, reasons and `retire` flags.
States are `valid`, `conflict`, `blocked`, `invalid`, `stale`, `missing` or
`unavailable`; only invalid/stale/missing rows may be retired.

### `POST /api/reconcile`

Body: `{"confirm":true,"token":"PREVIEW_TOKEN"}`. Both fields are required;
unknown fields and trailing JSON are rejected. The backend recomputes the
preview, refuses running jobs or changed tokens (`409`), then atomically retires
the planned rows as `superseded`. The response adds `retired` and `batch_id`.
`review_events` records actor `system`, classification, before/after statuses,
reason, current hash and time. Human `decision_note` is untouched.

No notes, IDs, model defaults, acceptance decisions, MCP state or reindex trigger
are changed. UUID preparation is the explicit CLI `identify` workflow; the web
UI reviews those proposals through the normal endpoints.

## Operations (scan / analyze / run / tasks)

Operations are started as **background jobs** — an `analyze` against a local
LLM can take minutes, far too long for a synchronous request. Only one
server-side operation runs at a time, including review decisions and `apply`
(SQLite is single-writer and concurrent file changes are unsafe). Jobs are
persisted in the `jobs` table, so history survives restarts; work itself is
not resumed, and a job left `running` when the server stops is marked `failed`
on the next boot.

### `POST /api/ops`

Body: `{"op": "<op>"}` where `<op>` is `scan`, `analyze`, `run`,
`link-check`, `enrich`, `embed`, `expand`, `freshness` or `all`.
Optional fields include `model`, `embed_model`, `external`, `allow_cloud`,
`allow_remote`, `web_verify`, `reanalyze` and `reanalyze_all`. Model capability and
remote-data consent are validated before the background job is accepted.
Every generation operation scans before selecting work, so hashes and frontmatter
are refreshed after an apply. A scan may insert a missing hash-neutral
`document_id`; it never normalizes the rest of an existing note.

- `link-check` probes external body URLs and writes the verdicts into each
  note's `kbm.link_check` frontmatter block (tool-owned; never changes the
  canonical content hash).
- `enrich`, `embed`, `expand`, `freshness` run the corresponding optional LLM
  pass on its own incremental lineage; `all` runs every configured LLM task
  in one job (enrich → embed → expand → freshness → analyze), skipping tasks
  without a configured model.
- `expand` grows stub documents against their nearest ancestor `STUB.md`
  template; `freshness` records a currentness verdict in `kbm.freshness` and
  may emit ordinary suggestions.

`"reanalyze": true` additionally reprocesses documents whose active-lane model
differs from the selected model. Same model, same semantic input, same prompt
fingerprint queues no analysis. Prompt, taxonomy, write-scope and semantic
content changes are detected independently. `"reanalyze_all": true` explicitly
forces all non-missing documents through the pass; use it only when a full
refresh is worth the cost. `"external": true` runs the lane-capable ops
(`analyze`, `run`, `expand`, `freshness`, `all`) with
`ollama_model_external` — enhanced lane for analysis, preferred lane for the
enhanced tasks. Without an explicit `"model"` it requires
`ollama_model_external`; a verified explicit override can select another
cloud model while retaining external analysis lineage.

For `freshness` and `all`, `"web_verify": true` checks each flagged claim
against Ollama's hosted web search: claim text (never the whole document) is
sent to `ollama.com`, and a second model pass judges every flag `stale`,
`current` or `unverifiable` with evidence and source recorded in
`kbm.freshness`. That pass runs on its own model lane —
`ollama_task_models.verify`, falling back to the external lane and then
`ollama_model`; a remote verify model needs `external: true` like any cloud
generation. It requires `OLLAMA_API_KEY` in the environment or a `.env`
file next to the config; other ops reject it with `400`.

`"model": "<name>"` selects the configured model for an op's generation
stage(s); for `all`, it pins every generation task to that model. The `embed`
stage uses the independent `"embed_model": "<name>"` override or configured
embedding default. `embed_model` is supported on `analyze`, `run`, `all` and
standalone `embed`; the latter also accepts its old `"model"` alias, but not
both fields together. A configured model or an
installed model with the required capability is accepted; cloud generation
requires `external: true` or `allow_remote: true`, while remote
embedding/enrichment requires `allow_cloud: true` or `allow_remote: true`.
`allow_remote` only consents — it never reroutes a task to the external lane
model, so resolved defaults keep their own model and lineage stamps. Unknown,
incompatible or unconsented choices return `400`.

```json
{
  "op": "all",
  "model": "gpt-oss:120b-cloud",
  "embed_model": "qwen3-embedding:4b",
  "external": true,
  "reanalyze": true
}
```

Selections are per-run and never rewrite configuration. The embedding model
does not inherit the generation override or its external consent.
`allow_cloud: true` explicitly permits off-machine embeddings/enrichment
without changing the analysis lane. An embedding model change refreshes
incompatible vectors without requiring `reanalyze` or implicitly requeuing
generation; `reanalyze_all` remains an explicit expensive refresh.

Adding `"dry_run": true` returns a queue estimate instead of starting a job —
the dashboard's "Estimate work" preview. The same model validation runs first,
then `200` answers with the documents the selection would process:

```json
{
  "op": "all",
  "documents": 521,
  "pending": { "enrich": 4, "embed": 521, "freshness": 3, "analyze": 12 },
  "dependents": 87,
  "analyze_total": 99,
  "stamps": {
    "analyzed": { "model": "gemma3:12b", "docs": 400 },
    "embedded": { "model": "embeddinggemma:300m", "docs": 521 }
  },
  "analysis_reasons": {
    "semantic_changes": 8,
    "prompt_policy_changes": 2,
    "model_drift": 1,
    "unstamped": 0
  },
  "warnings": [
    "Counts use the latest scan snapshot; execution scans again and counts may change if files changed.",
    "Analysis queue reasons (overlapping facets): semantic changes 8; prompt/policy changes 2; model drift 1; previously unstamped 0.",
    "embed uses qwen3-embedding:4b, but the latest pass on 521 document(s) ran with embeddinggemma:300m — every embedded document is re-vectorized and dependent analysis can expand"
  ]
}
```

`pending.analyze` is the direct semantic/prompt/model queue;
`dependents`/`analyze_total` show additional unchanged notes selected because
the substantive content of evidence they rely on changed. The `analysis_reasons`
facets may overlap. Graph-only link edits do not count as changed evidence.
Estimates use the latest scan snapshot, while execution scans again before work
starts. `warnings` flags stale-snapshot risk and model choices that differ from
the dominant recorded lineage — the `stamps` block — most importantly an
embedding model switch, which re-vectorizes the corpus. Deterministic ops
(`scan`, `link-check`) reject `dry_run` with `400`.

Returns `202` with the running job, `409` if another op is already running:

```json
{ "id": 1, "op": "run", "status": "running", "started": "…" }
```

### `GET /api/ops`

Scan/analyze/run jobs, newest first (last 100 returned). Report jobs are kept
under `/api/report/jobs`. Each job carries `status`
(`running|done|failed`), a `detail` summary (same text the CLI prints) or
`error`, and `started`/`finished` timestamps.

### `GET /api/ops/{id}`

Single job by id — poll this to watch progress. `404` for unknown ids.

### `GET /api/models`

Reports configured routing without contacting Ollama (routing excerpt):

```json
{
  "models": ["qwen3:8b", "gemma4:31b-cloud", "embeddinggemma:300m"],
  "tasks": {
    "enrich": { "default": "qwen3:8b", "options": ["qwen3:8b"] },
    "analyze": { "default": "qwen3:8b", "options": ["qwen3:8b", "alt"] },
    "expand": {
      "default": "gemma4:31b-cloud",
      "options": ["gemma4:31b-cloud"]
    },
    "freshness": {
      "default": "gemma4:31b-cloud",
      "options": ["gemma4:31b-cloud"]
    },
    "embed": {
      "default": "embeddinggemma:300m",
      "options": ["embeddinggemma:300m"]
    }
  }
}
```

`models` is the union of configured names, not a closed allowlist. Each task's
`default` is the resolved model (`ollama_task_models` default, else the lane
fallback) and `options` its configured alternatives. `stamps` records the
dominant model per lineage column (`analyzed`, `external`, `enriched`,
`embedded`, `expand`, `freshness`) with its document count — the "what ran
last time" the UI marks against the current selection. `external_default`
carries `ollama_model_external` so the UI can compare an empty generation
override on the enhanced lane. The response also
carries `generation_models` and `embedding_models` arrays. Entries have `name`,
`capabilities`, `locality` (`local`, `cloud`, `remote`, `unknown`), `configured`,
`verified` and optional `digest`, `size`, `context_length` and `error`.
Without discovery, configured choices are unverified and separated by their
configured roles.

### `GET /api/models?discover=1`

Explicit read-only discovery against the configured `ollama_url`:
`GET /api/tags`, then `POST /api/show` (four concurrent requests, at most 256
models, 15-second overall deadline). No caller-supplied server URL is accepted.
Generation/embedding arrays include compatible installed models not present
in configuration and filter by advertised `completion`/`embedding`
capabilities. Unverified entries are reported under `unverified_models` or as
configured entries with an error; they are not guessed to be compatible.

`discovered: true` indicates a successful refresh. On a catalog failure,
configured choices remain available with `discovered: false` and
`discovery_error`; the UI retains its last successful lists and selections.
Discovery never sends notes, loads weights, downloads models, saves defaults,
or enumerates the online catalog. Cloud-account authorization is still checked
by Ollama when the selected model actually runs.

### `GET /api/i18n`

Returns the UI's message catalog for the request's `Accept-Language`
(`{"locale": "en", "locales": ["en", "da"], "messages": {"<id>": "<text>"}}`;
plural entries are `{"one": ..., "other": ...}` objects). A `?lang=<tag>`
query parameter overrides `Accept-Language` — the dashboards use it for the
header locale picker, whose selection persists in `localStorage` ("automatic"
negotiates per request). The response carries `Vary: Accept-Language`.
Shipped catalogs live in `internal/i18n/locales/active.<tag>.yaml` (currently
`en` and `da`); adding a file localizes every `t()` binding in the dashboards
without code changes. Missing ids fall back to English — the frontends also
carry English fallbacks so the page stays readable if the fetch fails.

### `POST /api/client-log`

Receives the UI's uncaught exceptions and unhandled promise rejections
(`static/clientlog.js` posts `{message, source, line, stack}`) and writes
them to the server log — stderr, plus `log_file` when configured — so a
browser-side failure is diagnosable without DevTools access. `204` on
success, `400` for invalid or oversized (>8 KB) payloads.

### `POST /api/shutdown`

Stops the server gracefully — the dashboard's Stop button. Loopback clients
only (`403` otherwise). `409` while an operation runs in this process; retry
with `?force=1` to stop anyway (the orphaned job is later swept to `failed`).
Returns `202` once the shutdown is accepted; the process exits after in-flight
requests finish.

## Repository report

The report is intentionally separate from knowledge review. It reads the
configured `repos.yml` and optional `policy.yml`, runs local Git/scanner/Ruby
checks, native GitHub/Forgejo/CodeFactor/Sonar/Semgrep collectors, and Ruby
gem-fleet analysis. Remote checks default on for a configured registry and
perform read-only requests. The report never runs `kb-sync`, writes markers,
commits, or edits registered repositories. `report.comparison_path` is an
optional, read-only reference to the Ruby report's saved output.

### `GET /api/report`

Returns the latest SQLite-backed report snapshot as JSON. Before the first
run, it returns an empty snapshot. If `report.registry_path` is not configured,
`configured` is `false`.

### `GET /api/report/history`

Returns `{"points": [{"generated_at", "summary"}]}` — one stored summary per
completed report run, oldest first. Each `POST /api/report/run` appends a
point; history is capped at the 365 most recent runs. Powers the
progress-over-time chart in the "Durable history" section of `/report`.

### `POST /api/report/run`

Starts a background report job and returns `202`. Returns `409` while another
server-side operation is running, `400` for an unknown `repo`, or `503` when
no report registry is configured. Optional query parameters:

- `no_remote=1` to run local checks and keep cached remote values
- `remote=1` to enable remote checks even if disabled in config
- `force=1` to bypass unchanged-repository and RubyGems cache shortcuts
- `repo=NAME` to refresh one registered repository

### `GET /api/report/jobs` and `GET /api/report/jobs/{id}`

List or poll report jobs separately from scan/analyze/run jobs. Report job IDs
are not exposed through `/api/ops/{id}`.

## Example

```bash
KBM-flow serve --config config.local.json

curl -s localhost:8080/api/suggestions | jq .
curl -s localhost:8080/api/suggestions/3 | jq .proposed_content
curl -s -X POST localhost:8080/api/suggestions/3/decide -d '{"status":"accepted"}'
curl -s -X POST localhost:8080/api/apply | jq .

curl -s -X POST localhost:8080/api/ops -d '{"op":"run"}'
curl -s localhost:8080/api/ops/1 | jq .

curl -s -X POST localhost:8080/api/report/run
curl -s localhost:8080/api/report | jq .
```
