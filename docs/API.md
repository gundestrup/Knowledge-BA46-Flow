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

Returns the landing-page summaries: changed documents awaiting analysis, the
last successful `run` cycle and age in days, plus the latest repository
snapshot summary and optional timestamped comparison totals.

### `GET /api/documents`

Every tracked document with analysis metadata (path, title, summary, tags,
status, hashes). Ordered by path.

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

### `GET /api/suggestions/{id}`

Full suggestion row including `proposed_content`. `404` if the id does not exist.

### `POST /api/suggestions/{id}/decide`

Body: `{"status": "accepted"}` or `{"status": "rejected"}` — the HTTP
equivalent of `accept ID` / `reject ID`. An optional `"note"` string stores a
short reviewer reason on rejections; it is kept on the row (returned as
`decision_note` by `GET /api/suggestions/{id}`) and shown back to the model as
review feedback on later runs for that document. Returns `409` if the
suggestion is not pending or does not exist.

### `POST /api/apply`

Executes every `accepted` suggestion — the HTTP equivalent of `apply`.
Returns a result per suggestion:

```json
[{ "id": 3, "action": "move", "status": "applied", "applied": true }]
```

Each result has `status` `applied`, `failed` or `superseded`. A source changed
since its analysis is marked superseded and left untouched. Other suggestions
still run (same semantics as the CLI). When `reindex_trigger_path` is
configured and at least one suggestion applied, a final
`{"action": "reindex-trigger", ...}` row reports the trigger-file touch for
the MCP server's reindex watcher.

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
`reanalyze` and `reanalyze_all`. Model capability and remote-data consent are
validated before the background job is accepted.

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

For `analyze`/`run`, `"reanalyze": true` additionally reprocesses documents
whose last analysis ran under a different model than the configured
`ollama_model`. `"reanalyze_all": true` explicitly forces all non-missing
documents through the pass; use it after a prompt/taxonomy change only when a
full refresh is worth the cost. `"external": true` runs the lane-capable ops
(`analyze`, `run`, `expand`, `freshness`, `all`) with
`ollama_model_external` — enhanced lane for analysis, preferred lane for the
enhanced tasks. Without an explicit `"model"` it requires
`ollama_model_external`; a verified explicit override can select another
cloud model while retaining external analysis lineage.

`"model": "<name>"` selects the configured model for an op's generation
stage(s); for `all`, it pins every generation task to that model. The `embed`
stage uses the independent `"embed_model": "<name>"` override or configured
embedding default. `embed_model` is supported on `analyze`, `run`, `all` and
standalone `embed`; the latter also accepts its old `"model"` alias, but not
both fields together. A configured model or an
installed model with the required capability is accepted; cloud generation
requires `external: true`, while remote embedding/enrichment requires
`allow_cloud: true`. Unknown, incompatible or unconsented choices return `400`.

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
  "warnings": [
    "embed uses qwen3-embedding:4b, but the latest pass on 521 document(s) ran with embeddinggemma:300m — every embedded document is re-vectorized and dependent analysis can expand"
  ]
}
```

`pending` lists the hash/model-stale queue per stage the op would run;
`dependents`/`analyze_total` count the unchanged notes the analysis lane adds
because their evidence (links, mentions, overlap, calibrated embedding
neighbors) touched a changed document. `warnings` flags model choices that
differ from the dominant recorded lineage — the `stamps` block — most
importantly an embedding model switch, which re-vectorizes the whole corpus.
Deterministic ops (`scan`, `link-check`) reject `dry_run` with `400`.

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
