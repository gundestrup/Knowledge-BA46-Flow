# Configuration reference

`KBM-flow` is configured by a single JSON file passed with `--config` (default:
`config.json`). Relative paths are resolved against the config file's own
directory, so the file works from any working directory.

Typical workflow:

```bash
cp config.json config.local.json   # config.local.json is gitignored
./KBM-flow run --config config.local.json
```

## Fields

| Field                     | Type     | Default                              | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| ------------------------- | -------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `repository_path`         | string   | — _(required)_                       | Root of the markdown knowledge repository to maintain. For Knowledge Base MCP Server compatibility, point this at one shelf directory (`<KNOWLEDGE_BASES_ROOT_DIR>/<shelf>`), not the parent shelves directory.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `database_path`           | string   | — _(required)_                       | SQLite file tracking documents and suggestions. Created automatically.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `prompt_directory`        | string   | — _(required)_                       | Directory containing `base.md`, `<mcp_server>.md`, `analyze.md`, `enrich.md` (needed only when `ollama_model_fast` differs), `expand.md` and `freshness.md` (needed for the `expand`/`freshness` tasks), and `freshness-verify.md` (needed only for `freshness --web-verify`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `ollama_url`              | string   | — _(required)_                       | Ollama base URL, IP and port in one field, e.g. `http://localhost:11434`. Use `http://host.docker.internal:11434` from a container.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `ollama_model`            | string   | — _(required)_                       | Model name sent to `/api/generate` for the full analysis pass — any model from `ollama list`. See "Choosing a model" in the README.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `ollama_model_fast`       | string   | same as `ollama_model`               | Optional cheaper/faster model for the enrichment pass (title, summary, tags, frontmatter patch) that runs before full analysis. When set to a different model, each changed document is processed by both passes; when unset or equal, only the analysis pass runs.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `ollama_model_external`   | string   | empty (disabled)                     | Optional enhanced analysis model used only by an explicit external run (`--external`, `external: true` in `/api/ops`, or the dashboard checkbox) — e.g. a `-cloud`-tagged model proxied through a signed-in local Ollama. Results stamp `external_sha256`/`external_model` per document, leaving the local `analyzed_*` lineage untouched. Empty fails an external run fast; the flag never triggers accidentally. External models receive document content — see the privacy note in "Choosing a model".                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `ollama_num_ctx`          | int      | `0` (auto)                           | Context window sent as `options.num_ctx`. `0`/unset sizes it per request from the prompt (about prompt tokens × 2 + ~1000, floored at 4096, capped at the model's `context_length` via `/api/show`). Set a positive value to pin it instead — see "Sizing `ollama_num_ctx`".                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `ollama_embed_model`      | string   | empty (disabled)                     | Optional embedding model (e.g. `embeddinggemma:300m`, `nomic-embed-text`). When set, each LLM-visible document gets a stored embedding vector and analysis candidates are ranked by cosine relatedness — semantic ("same topic") rather than literal shingle overlap, which stays in use for the duplicate flag. Vectors backfill lazily at analysis time; `no_llm_context` documents are never embedded.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `ollama_task_models`      | object   | `{}`                                 | Per-task model routing: each key is one of `enrich`, `analyze`, `expand`, `freshness`, `verify`, `embed`, and its value is a non-empty list of model names — the first entry is the task's default, the rest are alternatives selectable per run (`--model`, the dashboard's model picker). A task default overrides the lane fallback: `enrich` → `ollama_model_fast`, `analyze` → `ollama_model`, `embed` → `ollama_embed_model`, `expand`/`freshness`/`verify` → `ollama_model_external` when configured, else `ollama_model`. `verify` is not a standalone task — it is the second pass of `freshness --web-verify`, the model that re-judges flagged claims against web search evidence; keep it separate when a stronger (or cheaper) model should verify than the one that flags. A per-run override or `--external` wins over the list. Example: `"ollama_task_models": {"freshness": ["gpt-oss:120b-cloud", "gemma4:31b-cloud"]}`. |
| `ollama_think`            | bool     | `false`                              | Sent as `think` on full-analysis requests; enrichment always sends `false`. Keep `false` for structured work. Set `true` only for deliberate analysis experiments; reasoning can multiply latency and exceed the request timeout.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `ollama_keep_alive`       | string   | empty (Ollama default, ~5m)          | Sent as `keep_alive` on generate and embed requests — how long Ollama keeps a model loaded after each call. `"0"` unloads immediately (useful when large models alternate on a memory-constrained host, at the cost of a reload per call). Invalid durations fail config validation.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `request_timeout_seconds` | int      | `300`                                | Per-request HTTP timeout for Ollama calls.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `max_document_bytes`      | int      | `200000`                             | Documents larger than this are skipped during analysis (keeps prompts bounded).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `extensions`              | string[] | `[".md", ".markdown"]`               | File extensions to scan (case-insensitive).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `exclude_directories`     | string[] | `[]`                                 | Directory names skipped during the walk — matches by name at any depth. The trash directory is always excluded.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `exclude`                 | string[] | `[]`                                 | Repository-root-anchored paths and glob patterns skipped during the walk: `docs/tmp` excludes exactly that subtree, `*`/`?`/`[...]` work within one path segment, and a whole `**` segment matches any depth (`drafts/**`, `**/private`, `**/*.draft.md`). A matching directory prunes its whole subtree.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `exclude_extensions`      | string[] | `[]`                                 | File types removed from the `extensions` allowlist, e.g. `[".markdown"]`. Case-insensitive; entries need the leading dot. Must leave at least one scannable extension.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `trash_directory`         | string   | `.kbm-trash`                         | Deleted/merged files are moved here, inside the repository — never hard-deleted.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `http_addr`               | string   | `127.0.0.1:8080`                     | Listen address for `KBM-flow serve` — `host:port`, or a bare port like `"9090"` (means `127.0.0.1:9090`). Empty hosts such as `:8080` are rejected; opt in explicitly with `0.0.0.0:8080`. `/api/apply` mutates the filesystem — keep it loopback-only unless deliberately exposed.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `report`                  | object   | disabled                             | Optional source paths for the separate, local repository report.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `report.registry_path`    | string   | empty                                | Path to the read-only `repos.yml` registry. Setting it enables `KBM-flow report` and `/report`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `report.policy_path`      | string   | empty                                | Optional path to the read-only `policy.yml` expectations.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `report.comparison_path`  | string   | empty                                | Optional path to the original Ruby report's `data.json`, retained as a timestamped parity reference and fallback for remote/gem values not yet in the Go snapshot.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `report.remote_enabled`   | boolean  | `true` when a registry is configured | Run the read-only GitHub, Forgejo, CodeFactor, Sonar, Semgrep, RubyGems, npm registry and Node.js release collectors when refreshing the report. Set `false` for local-only runs.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `mcp_server`              | string   | `knowledge-base-mcp-server`          | The knowledge-base MCP server the corpus is curated for. Selects the `prompts/<mcp_server>.md` conventions file; only `knowledge-base-mcp-server` is currently accepted.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `validate`                | object   | disabled                             | Scope for the `KBM-flow validate` structural gate.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `validate.exempt`         | string[] | `[]`                                 | Paths outside knowledge scope for validation — no frontmatter/tag, index back-link, or index-listing requirements, and their non-Markdown siblings need no link. Same root-anchored pattern syntax as `exclude`; matching a directory exempts its subtree. Use for meta and scratch material (`project-template/**`, `temp/**`, root `README.md`/`SECURITY.md`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `reindex_trigger_path`    | string   | empty (disabled)                     | Repository-relative file truncated after every successful apply batch — its mtime signals the MCP server's `REINDEX_TRIGGER_PATH` watcher to reindex. Must resolve inside `repository_path` (validated like a document path); `.reindex-trigger` is the convention since both scanners skip dotfiles.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `log_file`                | string   | empty (stderr only)                  | Also write logs to this file, appended — useful for `serve` left running. Relative paths resolve against the config file's directory. Browser-side errors (uncaught exceptions, unhandled rejections) are relayed to `POST /api/client-log` and land here too.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `log_level`               | string   | `info`                               | `debug` adds one line per HTTP request (method, path, status, duration — `/static/` hits excluded) plus job start/finish detail. Anything else is rejected.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |

## Write permissions

`write_scope` defaults to `metadata-links`: allowlisted frontmatter and exact,
verified rename-link repairs only. It blocks body additions, moves, merges and
deletes, including old accepted suggestions. Use `content` to enable validated
additive body edits without file operations; `all` separately enables reviewed
move/merge/trash actions. Unknown values fail configuration validation. No mode
bypasses source hashes, protected fields or `.kb-policy.json`.

Blocked accepted suggestions remain accepted and can be withdrawn. Existing
configs that omit the field receive the restrictive default after restart.
See [review and migration handling](REVIEW.md) before reconciling an old queue
or preparing document IDs.

## Model stages and validation

Optional-stage limits are independent of the full-analysis request timeout:

| Field                        | Type   | Default       | Purpose                                                                                                                                                                                                                                                                                                                    |
| ---------------------------- | ------ | ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `enrichment_timeout_seconds` | int    | `120`         | Deadline for one enrichment call, including context lookup; 0 selects the default, negative values are rejected.                                                                                                                                                                                                           |
| `enrichment_max_tokens`      | int    | `512`         | Maximum enrichment output tokens (`options.num_predict`); 0 selects the default, negative values are rejected. A reply truncated at this cap is retried once with a 4× cap (up to a learned ceiling of 8192), and a cap that proves sufficient is reused for later calls — raising it manually is only needed beyond that. |
| `enrichment_mode`            | string | `local-first` | `local-first` fills only missing title/tags from clear H1/path evidence and skips fast generation when fields are already valid; `always` calls a distinct fast model for every eligible document.                                                                                                                         |
| `stub_max_body_lines`        | int    | `5`           | Documents with at most this many non-empty body lines (under the frontmatter) are placeholder stubs: scanned, embedded and valid link targets, but skipped by every generation stage. `0`/unset selects the default; a negative value disables size detection so only the explicit `stub` tag applies.                     |

These are corpus- and model-sensitive limits. When a bounded enrichment reply
is truncated, the client automatically retries once with a 4× output cap (up
to 8192) and reuses a cap that works — persistent failures beyond that ceiling
still warrant raising `enrichment_max_tokens` for that configuration. For `stub_max_body_lines`, inspect short notes before
choosing a threshold: concise catalog entries can be valid knowledge rather
than placeholders. The explicit `stub` tag always takes precedence.

In a `tasks --model` batch, the override applies to generation tasks only;
`--embed-model` independently overrides embeddings, otherwise they use the
`ollama_task_models.embed` default or `ollama_embed_model`. `analyze` and `run`
accept the same two flags. Standalone `embed` accepts either `--embed-model`
or the backward-compatible `--model` alias. Alternatives can be configured
without changing the current default:

```json
{
  "ollama_embed_model": "embeddinggemma:300m",
  "ollama_task_models": {
    "embed": ["embeddinggemma:300m", "qwen3-embedding:4b"]
  }
}
```

Remote generation requires the explicit `--external` opt-in; remote
enrichment/embedding requires `--allow-cloud`. The CLI and API preflight
selected models for their required capability and privacy setting. Compatible
installed models need not be configured to be selected for one run. Discovery
and per-run choices never save defaults or download models. Model changes
refresh incompatible embeddings automatically, without implicitly rerunning
generation; see [model selection](MODELS.md).

Changing `stub_max_body_lines` invalidates only documents whose stub/non-stub
classification changes; other documents keep their task lineage. A newly
eligible stub is still skipped by generation until its classification changes
back or its content is expanded.

Use separate model identities for the two generation roles, not aliases of
one model merely to force two passes. In `local-first` mode, existing valid
frontmatter is preserved and only missing title/tags are derived locally from
an unambiguous H1 or a specific shelf path. This works even if no distinct fast
model is configured; when one is configured, ambiguous notes fall through to
it. Set `enrichment_mode: always` to run the fast model for every eligible note
when benchmarking. Enrichment can patch only `title`/`tags`;
full analysis can also patch `related`/`contradicted_by`. Other frontmatter
fields remain read-only. The local validator rejects invalid metadata/links
and body loss; apply rechecks older suggestions too. If a local metadata
proposal is produced before full analysis fails, it remains pending for review;
a later successful analysis consolidates compatible metadata changes.

Go derives missing title/tag fields independently, including in a document
that already has frontmatter. Existing valid values are preserved; provenance,
lifecycle and privacy never receive defaults. Every proposal still requires
human review and `apply`.

If the repository root contains `taxonomy.yaml` (version 1), local path-derived
tags and newly proposed tag values must be listed under its `kind`, `subject`,
or `function` vocabulary. Malformed taxonomy files fail closed; without one,
the analyzer retains the general lowercase/string-list validation.

Enrichment always sends `think:false`. A timeout, generation failure, invalid
reply or output cutoff becomes a retryable warning, not a reason to skip full
analysis. Privacy/read checks still fail closed and parent cancellation stops
the run. Stage outcomes are retained in SQLite's `stage_results`, independently
of the current metadata shown in `documents`.

Embeddings, enrichment and analysis run in batches. Completed embedding and
enrichment models are unloaded at stage boundaries, avoiding per-document
switching while retaining normal `keep_alive` caching within a batch.
`ollama_keep_alive: "0"` still forces a reload after every request and is
usually slower than the stage-boundary unloading.

For an Apple M2 with 24 GB, see `docs/MODELS.md` and the
`example-configs/config.m2-24gb.json` profile: `gemma3:12b` analysis,
`phi4-mini:latest` enrichment and `embeddinggemma:300m` retrieval.
This is a pairing to qualify with a small pilot, not a quality guarantee.

## Sizing `ollama_num_ctx`

The default (`0`) is **automatic**: each generate request sizes its own
window as

```text
num_ctx ≈ (prompt_bytes / 4) × 2 + 1024   →   floored at 4096, capped at
                                              the model's context_length
```

- `bytes / 4` — rough tokens-per-byte for English technical markdown.
- `× 2` — `expand`/`merge` suggestions return `proposed_content`, i.e. the
  _complete_ rewritten document, so the reply can be as large as the input.
- The cap is read once per run from `/api/show` (`model_info`'s
  `*.context_length`); if the query fails, a conservative 16384 fallback
  applies.

Bounded enrichment uses `prompt_bytes / 4 + enrichment_max_tokens + 1024`
instead, with the same floor and model cap: it does not need a document-sized
reply allowance. A positive `ollama_num_ctx` still overrides automatic sizing
for both generation stages. These token estimates are heuristics, not exact
tokenization; qualify code-heavy and long documents before scaling up.

Per-request sizing means a 2 KB doc gets a ~5k window (fast, small KV cache)
while a 30 KB doc automatically gets ~16k — no configuration needed, and no
need to size for the largest document in the corpus.

**When to set a fixed value instead:** pinning `ollama_num_ctx` is an
override for edge cases — e.g. forcing a small window on a RAM-starved host,
or keeping the window constant so Ollama's KV-cache allocation stays stable
across alternating prompt sizes. A pinned value that is too small silently
truncates large prompts; too large wastes memory on every call. `context_length`
upper bounds: `ollama show <model>` — qwen3 ≈ 40k, gemma3 ≈ 131k.

Auto sizing caps at model capability, not free RAM. A model's maximum context
can be impractical on a 24 GB machine. Use a long-document pilot and inspect
memory pressure; avoid both silently undersized windows and unnecessarily
large ones. Metadata patches reduce typical output, but body expands/merges
still need enough reply space. Reported generation-length cutoffs are errors,
not successful analysis results.

`max_document_bytes` remains the hard input bound regardless of mode: files
larger than it fail with a clear error rather than reaching the model.

## Environment overrides

Deployment-specific values can come from the environment instead of the JSON
file — env wins over the file and can satisfy required fields. This is how
`docker-compose.yml` shares one `.env` between this program and the MCP
server. A `.env` file next to the JSON config is honored for direct runs
too: its values fill in whatever the real environment does not set, which is
also where secrets like `OLLAMA_API_KEY` belong — never `config.json` itself.

| Variable                    | Overrides               | Notes                                                                                                                                                                                                                                                                                                                             |
| --------------------------- | ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `OLLAMA_BASE_URL`           | `ollama_url`            | Deliberately the MCP server's own name — one value drives both.                                                                                                                                                                                                                                                                   |
| `OLLAMA_API_KEY`            | — (env only)            | Bearer key for Ollama's hosted web search; enables `freshness --web-verify`. There is deliberately no JSON field, so the key cannot be committed in `config.json`.                                                                                                                                                                |
| `KBM_REPOSITORY_PATH`       | `repository_path`       | e.g. `/knowledge/$KB_SHELF` in compose.                                                                                                                                                                                                                                                                                           |
| `KBM_DATABASE_PATH`         | `database_path`         |                                                                                                                                                                                                                                                                                                                                   |
| `KBM_HTTP_ADDR`             | `http_addr`             | Same validation (`0.0.0.0` must be explicit).                                                                                                                                                                                                                                                                                     |
| `KBM_OLLAMA_MODEL`          | `ollama_model`          | `KBM_` prefix avoids colliding with the server's `OLLAMA_MODEL`.                                                                                                                                                                                                                                                                  |
| `KBM_OLLAMA_MODEL_FAST`     | `ollama_model_fast`     |                                                                                                                                                                                                                                                                                                                                   |
| `KBM_OLLAMA_MODEL_EXTERNAL` | `ollama_model_external` |                                                                                                                                                                                                                                                                                                                                   |
| `KBM_ENRICHMENT_MODE`       | `enrichment_mode`       | `local-first` by default; set `always` for a fast-model comparison.                                                                                                                                                                                                                                                               |
| `KBM_LANG`                  | — (env only)            | UI/CLI locale override (e.g. `da`); falls back to `LC_ALL`/`LC_MESSAGES`/`LANG`. Ships `en` + `da` catalogs (`internal/i18n/locales/`); the web UI negotiates per-request via `Accept-Language`, and the header language picker on each page overrides it (persisted in browser `localStorage`, sent as `?lang=` to `/api/i18n`). |
| `KBM_LOG_FILE`              | `log_file`              | e.g. `logs/kbm.log` in compose — appended, never rotated.                                                                                                                                                                                                                                                                         |
| `KBM_LOG_LEVEL`             | `log_level`             | `info` or `debug`.                                                                                                                                                                                                                                                                                                                |

## Shipped variants

- `config.json` — local run against `./knowledge` with `localhost:11434`.
- `config.docker.json` — container run; absolute paths (`/knowledge`, `/data`,
  `/app/prompts`), `host.docker.internal` for Ollama, and
  `reindex_trigger_path` wired to the compose `REINDEX_TRIGGER_PATH`.
- `example-configs/config.example-data.json` — the complete reference:
  every config key appears here (drift-tested), running against the bundled
  `example-knowledge/` via `../` paths resolved from `example-configs/`.
- `example-configs/config.m2-24gb.json` — optional Gemma/Phi/EmbeddingGemma
  profile using the bundled example corpus and its own database.
- `example-configs/repos.example.yml` and
  `example-configs/policy.example.yml` — report YAML schema examples. Replace
  the sample repository path/hash before using them.

## Notes

- Report source paths are resolved relative to the JSON config. Individual
  repository paths in `repos.yml` are resolved relative to the registry file.
  The report reads those files without invoking `kb-sync`; it does not write
  to registered repositories. The shipped `config.json` points to sibling
  `engineering-memory/repos.yml` and `policy.yml` files; change these paths
  when your workspace layout differs. `report.comparison_path` is a read-only
  Ruby-output reference used for validation; native Go collectors are the
  primary report source. Use `KBM-flow report --no-remote` or set
  `report.remote_enabled` to `false` to avoid remote API calls.
  `mcp_server` selects the supported prompt/conventions file for frontmatter.
- The scanner always skips dot-prefixed entries and the MCP server's
  `logs/`, `tmp/`, `_tmp/`, and workflow-sidecar paths, in addition to
  `exclude_directories`, `exclude` and `exclude_extensions`. Configure
  `repository_path` as one MCP shelf so the scanned files and its
  `.kb-policy.json` share the same policy boundary.
- `exclude` patterns are anchored at the repository root: `tmp` excludes only
  `<repo>/tmp`, while `**/tmp` matches at any depth (the same result as adding
  `tmp` to `exclude_directories`). Entries must stay inside the repository —
  absolute paths, `..` segments and `**` embedded inside a segment are
  rejected at load time. Excluded paths are also hidden from the
  sibling-file context shown to the model for neighbouring documents.
- `.kb-policy.json` is checked again before each accepted filesystem mutation.
  Its supported shape is `{"mutations":"allow"}` or
  `{"mutations":"deny"}`; a missing file allows writes, while malformed,
  unsupported, or unreadable policies block them.
- The Docker and bundled example configs leave `report.registry_path` empty,
  so the report is disabled there by default.
- IP and port are a single `ollama_url` field, not split settings.
- There is no "mode" field — the mode is the subcommand (`scan`, `analyze`,
  `run`, `report`, `list`, `review`, `accept`, `reject`, `apply`).
- Prompts are plain markdown files, so model-specific tuning needs no rebuild.
