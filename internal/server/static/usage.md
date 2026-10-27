# Knowledge

## Run options

### expand
Stub expansion grows placeholder notes — `stub`-tagged notes or notes
with at most `stub_max_body_lines` non-empty body lines (default 5) —
against the `STUB.md` template found in their category directory.

- Skipped stubs stay visible as link targets and candidates.
- Proposals land in the review queue like any other suggestion.

### reanalyze
Re-analyse documents whose *stamped* model differs from the selected
one. This is not a force-all: documents already evaluated by the same
model with the same inputs stay skipped.

- Compares `analyzed_model`/`external_model` against your selection.
- Combines with the pending queue — model drift unions with
  content/prompt drift.

### external
External generation lane. Document content leaves this machine — the
model runs at its provider (e.g. `gpt-oss:120b-cloud` via Ollama).

- Stamps `external_sha256`/`external_model`, leaving the local
  `analyzed_*` lineage untouched — the two ledgers stay separate.
- With identical configured models on both lanes, each ledger needs one
  independent catch-up run, then stays incremental.
- `no_llm_context` notes are never sent.

### allow-cloud
Separate consent for off-machine embeddings and enrichment. It does
not switch the generation lane — it only permits `embed`/`enrich`
stages to use remote models for this run.

- Independent of `--external`, which governs generation.
- Remote embedding of a corpus is itself a privacy decision.

### web-verify
Verifies freshness flags against Ollama's hosted web search. Flagged
claim text — never the whole document — is sent to `ollama.com`.

- Requires `OLLAMA_API_KEY` in the server environment or `.env`.
- A second verify model judges each flag `current`, `stale`, or
  `unverifiable` with a cited source.
- The `verify` lane is separately routable in `ollama_task_models`.

### estimate
Dry-run the current selection before spending model calls.

- Counts are computed from the latest scan snapshot; the task scans
  again before running, so totals may shift.
- Reason facets distinguish semantic changes, prompt/policy drift,
  model drift and never-stamped documents, plus dependent notes pulled
  in by changed evidence.

### lanes
Two independent ledgers track which model evaluated what, so a run
never silently credits one lane with the other's work.

- Local lane: `analyzed_*` stamps, the default `all`/`run` path.
- External lane: `external_*` stamps, behind the external checkbox.
- Same model + same inputs on either lane means "already evaluated" —
  but each ledger must converge once on its own.

## Review

### write-scope
The `write_scope` config limits what an accepted suggestion may change
even after a human accepts it. `metadata-links` (default) allows
frontmatter and verified link repairs only; `content`/`all` unlock body
and file operations.

### document-id
`document_id` is a stable UUID assigned in place during scanning when a
note lacks one. It is excluded from the content hash — assignment never
invalidates proposals — and proposals can never change it.

# Report
