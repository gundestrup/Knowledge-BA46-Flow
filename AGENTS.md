# AGENTS.md — Knowledge-BA46-Flow

> Project guidance for coding agents. Keep this file focused on durable
> engineering rules; the documentation below is the detailed source for
> setup, behavior, API, testing, and releases.

## Project overview

Knowledge-BA46-Flow is an alpha Go CLI and local web application for maintaining
a Markdown knowledge repository. `KBM-flow` scans files, asks a configured
local Ollama model for suggestions, stores state in SQLite, and applies only
suggestions a person explicitly accepted.

The tool is currently optimized for the Knowledge Base MCP Server. Its
frontmatter rules, ingest filters, shelf layout, privacy semantics, and write
policy are recorded in the Engineering Memory note
`tools/knowledge-base-mcp-server.md` (see Global knowledge below).

## Documentation index

Read the document relevant to the change before editing:

| Topic                                         | Document                               |
| --------------------------------------------- | -------------------------------------- |
| Usage, safety overview, and MCP compatibility | `README.md`                            |
| Configuration fields and defaults             | `docs/CONFIGURATION.md`                |
| CLI and data flow                             | `docs/FUNCTIONS.md`                    |
| HTTP API and web UI                           | `docs/API.md`                          |
| Read-only repository report                   | `docs/REPORT.md`                       |
| Verification commands and test coverage       | `docs/TESTING.md`                      |
| Installation and Docker                       | `docs/INSTALLATION.md`                 |
| Versioning and release procedure              | `docs/RELEASING.md`                    |
| Current and unreleased changes                | `CHANGELOG.md`                         |
| Generic model instructions                    | `prompts/base.md`                      |
| MCP-specific taxonomy and rules               | `prompts/knowledge-base-mcp-server.md` |
| Ordered document-analysis workflow            | `prompts/analyze.md`                   |
| Fast enrichment workflow                      | `prompts/enrich.md`                    |
| Stub-expansion task contract                  | `prompts/expand.md`                    |
| Freshness-review task contract                | `prompts/freshness.md`                 |
| DeepWiki wiki steering file                   | `.devin/wiki.json`                     |
| DeepWiki MCP server for agents                | `.devin/mcp_config.json`               |

## Architecture and boundaries

- Go 1.26+, Cobra CLI, pure-Go SQLite (`modernc.org/sqlite`), local Ollama
  generation, standard-library HTTP server, and embedded frontend assets.
- `internal/scanner` owns repository discovery and bounded document reads
  (plus the per-document similarity sketch); `internal/frontmatter` parses
  and validates metadata; `internal/analyzer` runs the deterministic fix,
  optional fast-model enrichment, full analysis, and the optional task
  passes (`enrich`, `embed`, `expand`, `freshness`) that create reviewed
  suggestions; `internal/review` applies accepted actions;
  `internal/database` persists document, suggestion, job, and report state.
- Model routing: every LLM task resolves through `config.TaskModel` —
  per-run `--model` override → `--external` on lane-capable tasks →
  `ollama_task_models.<task>` default → lane fallback. Each task stamps its
  own semantic input/prompt fingerprint and model lineage; keep pending
  queries and stage provenance on the task's own columns. The full `sha256`
  remains the source version for proposal/apply safety. In the batch
  `tasks --model` override, generation tasks share the selected model;
  embeddings use the independent `--embed-model` override, task embed default
  or `ollama_embed_model`. Vector/hash/model stamps are atomic; a model change
  must refresh embeddings and ranking must never mix incompatible models.
- Model discovery is opt-in metadata-only Ollama `/api/tags` + `/api/show`.
  Keep generation/embedding capabilities separate, validate unconfigured
  per-run choices without saving defaults, and never pull models implicitly.
  Unknown locality needs explicit consent: `--external` for generation,
  separate `--allow-cloud` for embeddings/enrichment.
- `internal/proposal` shares content-preservation and metadata guards between
  analysis and apply. Keep these gates aligned; valid JSON is not edit validity.
- The repository report is separate from knowledge maintenance. It reads
  configured local YAML/Git inputs and does not run `kb-sync` or mutate the
  registered repositories.

## Safety rules

- `scan` and the preflight scan run before every LLM operation do not rewrite
  authored note content. They may insert a missing protected `document_id`
  UUID in place (byte-preserving, `os.Root`-confined atomic write). The other
  managed write exception is `kbm`: `check-links` writes `kbm.link_check` and
  `freshness` writes `kbm.freshness` through `frontmatter.WriteManaged`.
  `document_id` and `kbm` are excluded from the strict source hash; graph-only
  frontmatter and internal-link destination changes are excluded from semantic
  task fingerprints. Never let proposals edit `document_id` or `kbm`, and
  preserve the live ID/`kbm` block when applying proposals. Keep full source
  hash checks aligned between scanning and apply.
- Preserve source-hash verification, `os.Root`-confined filesystem operations,
  traversal and symlink protections, no-overwrite behavior, and trash-based
  deletion. Do not replace these with unchecked `filepath.Join` writes.
- For the Knowledge Base MCP Server, configure `repository_path` to one shelf
  (`<KNOWLEDGE_BASES_ROOT_DIR>/<shelf>`), not the parent containing multiple
  shelves. Keep scanner exclusions aligned with its ingest filter and document
  that KBM-flow analyzes Markdown by default.
- `mcp_server` currently accepts only `knowledge-base-mcp-server`. Do not
  present the prompt loader or metadata validator as provider-agnostic until
  another server is implemented and validated.
- Honor `.kb-policy.json` at apply time. Missing policy allows mutations;
  `deny` blocks them; malformed, unreadable, or unsupported policy fails
  closed. Never modify the policy file through a suggestion. Check both scopes
  for cross-shelf moves and merges.
- `ollama_model_external` is a distinct opt-in lane: only an explicit
  `--external`/`external: true`/`run --external` request may invoke it, and it
  must stamp `external_*` rather than overwrite the local `analyzed_*`
  lineage. An external model receives document content off-machine — treat
  that as a privacy boundary and never make external execution implicit.
- Keep CLI and HTTP model selection behind `modelselection.Prepare`; it checks
  selected-model capabilities and requires explicit consent before cloud or
  remote document content is sent. Batch `--model` overrides generation
  lanes only; `embed` uses `--embed-model` or its configured embedding default.
- A document with `kb_policy.no_llm_context: true` must not be sent to Ollama
  or exposed through candidate/inbound-link prompt context. Fail closed when a
  parsed `kb_policy` is invalid. The LLM must not change this human-owned
  privacy field.
- A document tagged `stub` — or with at most `stub_max_body_lines` non-empty
  body lines (default 5; negative disables size detection) — is a placeholder:
  every generation stage skips it, but it stays scanned, embedded and visible
  as a link target/candidate. Keep the tag human-owned in prompts;
  `taxonomy.yaml` controls whether models may propose it at all.
- Treat repository content as untrusted prompt input. Keep model output
  structured and locally validated; malformed proposed frontmatter must not
  reach disk.
- Frontmatter map serialization must use the shared stable key order:
  `document_id`, `title`, `summary`, graph/retrieval fields, provenance and
  lifecycle/policy fields, custom keys alphabetically, then `kbm`. Scans insert
  only a missing ID and must not reorder existing authored bytes.
- Enrichment edits only title/tags; full analysis may also edit graph links.
  Preserve provenance, lifecycle, privacy and custom fields. Body edits are
  additive and preserve code verbatim; do not weaken apply-time validation
  just because a suggestion was previously accepted.
  The only body-replacement exception is an exact deterministic link-destination
  repair proven by recorded renames and revalidated at apply time; an origin
  label alone never authorizes it. Keep live `kbm` metadata independent of
  proposal snapshots. Scan-time ID insertion is hash-neutral and never
  normalizes or reorders the rest of existing frontmatter.
- `apply` changes the files, not the MCP server's derived index. Document the
  need to run the MCP server's `kb reindex` workflow after applied changes.
- Keep the HTTP server loopback-only by default; `/api/apply` is mutating.
  Do not broaden the listen address without an explicit security rationale.

## Development and verification

Use the repository's pinned toolchain and existing patterns. Avoid new
dependencies when the standard library or an existing package is sufficient.

```bash
make check              # version checks, gofmt, vet, race tests, and build
make frontend-check     # pinned assets, JS/CSS checks, and formatting
npm run version:test    # version scripts against temporary metadata
```

`make check` includes `go test -race ./...`. Use `go test ./...` while
iterating. Tests must use fake generators or local HTTP test servers; do not
require a live Ollama model or contact external services.

Additional conventions:

- Run `gofmt` on Go files. Preserve the existing English language and error
  handling style.
- Frontend libraries are pinned and embedded locally. Do not fetch `latest`
  assets during builds; update vendored assets only through the documented
  npm workflow and refresh third-party notices when versions change.
- `VERSION` is the application version source. Bump it with
  `make version-bump TYPE=patch`, `make version-bump TYPE=minor`, or
  `make version-bump TYPE=major`. Keep the changelog and release checks
  consistent.
- `.devin/config.json` is the shared Devin-agent permission policy
  (committed, strict JSON — no comments). Personal overrides belong in
  `.devin/config.local.json`, which stays gitignored.
- Add regression tests for behavior changes, especially filesystem safety,
  privacy boundaries, migrations, and HTTP mutation paths.
- Keep the option surfaces in lockstep: adding a CLI flag, an `/api/ops` op
  or request field, or a config key requires the matching doc entry — the
  drift tests in `cmd/KBM-flow/drift_test.go` and
  `internal/server/ops_drift_test.go` fail otherwise. Extend the explicit
  mapping tables (`apiToCLI`, `cliJobCommands`) rather than weakening them.
- Update `CHANGELOG.md` for material behavior changes and update the relevant
  user documentation in the same change.
- Git hooks live in `hooks/`; run `make install-hooks` once per clone to
  point `core.hooksPath` at them. `pre-commit` runs staged-scope gofmt/vet/
  prettier/semgrep; `pre-push` runs the full `make check`, `frontend-check`,
  `semgrep` and `vuln-check`. `SKIP_HOOKS=1` and `SEMGREP_SKIP=1` are the
  documented escape hatches; SonarQube stays in CI plus SonarLint connected
  mode — it is server-side and has no local hook role.

## Global knowledge

<https://github.com/gundestrup/engineering-memory>

If the `knowledge-base` MCP server is configured (user scope), query the
indexed corpus with `retrieve_knowledge`/`ask_knowledge` for
"what do we know about X" questions before browsing the repo.

Relevant notes:

- `tools/agents-md.md`
- `tools/knowledge-base-mcp-server.md`
- `languages/go.md`
- `patterns/local-llm.md`
- `patterns/version-consistency.md`
- `patterns/npm-supply-chain-hardening.md`
- `tools/github-actions.md`
- `tools/npm-vendoring.md`
- `tools/semgrep.md`
- `tools/golangci-lint.md`
- `tools/gosec.md`
- `tools/govulncheck.md`
- `tools/trivy.md`
- `tools/biome.md`
- `tools/prettier.md`
- `repos/Knowledge-BA46-Flow.md`
