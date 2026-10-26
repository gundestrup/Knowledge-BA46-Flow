# Model selection and qualification

Beyond the per-lane config fields (`ollama_model`, `ollama_model_fast`,
`ollama_model_external`, `ollama_embed_model`), `ollama_task_models` routes
each optional task (`enrich`, `analyze`, `expand`, `freshness`, `embed`) to a
default model plus alternatives selectable per run — useful when a stronger
cloud model should handle `expand`/`freshness` while local models carry the
routine passes. `GET /api/models` reports the resolved routing; `KBM-flow
tasks --model <name>` runs every generation task through one model. Embeddings
use an independent `--embed-model` override or configured default; a chat model
cannot produce vectors.
Cloud generation requires explicit `--external` or `--allow-remote` consent;
remote enrichment or embedding requires `--allow-cloud` (or `--allow-remote`).
Selected models are checked for the required Ollama capability before a task
starts.

## Independent generation and embedding

`analyze`, `run` and `tasks` accept both model flags. They are per-run overrides,
not configuration edits:

```bash
./KBM-flow tasks --config config.local.json \
  --model gpt-oss:120b-cloud --external \
  --embed-model qwen3-embedding:4b --reanalyze
```

Embedding resolution is `--embed-model` → `ollama_task_models.embed[0]` →
`ollama_embed_model`. Standalone `embed` accepts either `--embed-model` or its
backward-compatible `--model` alias, never both. For example:

```json
{
  "ollama_embed_model": "embeddinggemma:300m",
  "ollama_task_models": {
    "embed": ["embeddinggemma:300m", "qwen3-embedding:4b"]
  }
}
```

`qwen3-embedding:4b` is an optional quality-focused candidate, not a measured
winner for this repository. Compare it with the lightweight EmbeddingGemma
baseline on related-note recall, latency, memory, multilingual notes and long
documents before changing defaults. `qwen3-embedding:8b` is a higher-cost
comparison, not an automatic improvement. Install alternatives separately
through Ollama before selecting them; KBM-flow does not download models.

Changing the embedding model automatically refreshes visible documents'
vectors, even without `--reanalyze` and even when analysis has no pending
documents. Vector bytes, canonical source hash, producing model and audit row
commit together. Ranking never mixes models, including equal-dimensional
vectors or a partially failed refresh. There is one cached vector per document,
not parallel indexes for each model.

Changing embeddings does not itself requeue expensive generation passes.
`--reanalyze` includes generation-model drift; use `--reanalyze-all` explicitly
when you want all suggestions reconsidered using the new retrieval context.
If you replace weights under the same model tag, force `embed --reanalyze-all`
to refresh that tag's cache. Prefix truncation remains enabled; selecting a
larger model does not introduce document chunking.

## Discovery and privacy

The dashboard's separate selectors start with configured role-specific choices.
**Refresh from Ollama** queries `/api/tags`, then `/api/show` with bounded
concurrency, through KBM-flow's backend and configured `ollama_url`.
Generation requires advertised `completion`; embeddings require `embedding`.
Capabilities are not guessed from names. Missing/failed capability metadata
stays unverified; incompatible or unavailable entries cannot be selected after
refresh. A model supporting both capabilities may appear in both lists.

Discovery lists models registered on that server, not the full online catalog
or a guarantee of cloud-account access. It never pulls models, loads weights,
sends note content or saves defaults. Explicit per-run selections, including
unconfigured models, are revalidated before a job starts; configured defaults
are checked lazily before content is sent so empty passes still work offline.

Cloud/remote generation requires `--external` (API `external: true`), which
keeps analysis provenance on `external_*`, even for an explicitly selected
model not in `ollama_model_external`. The narrower `--allow-remote` /
`allow_remote: true` consents to whatever remote models a run resolves —
configured defaults or overrides — on any lane WITHOUT rerouting: task
defaults keep their own model and lineage columns. Embeddings and standalone
enrichment use separate `--allow-cloud` / `allow_cloud: true` consent without
changing the analysis lane. Unknown locality requires consent, not an assumption of local
execution. A non-loopback Ollama endpoint is off-machine even when its model
weights are local to that server. `no_llm_context` remains enforced in every
role.

Gemini Embedding 2 is not interchangeable with the local EmbeddingGemma model.
Only models actually served by the configured Ollama embedding API are usable
here. A Google-only embedding API would need a separate provider integration;
there is no Google API-key/provider flag implemented by this feature.

## Apple M2 with 24 GB unified memory

Use `enrichment_mode: local-first` before assigning routine metadata work to
`phi4-mini`: valid existing metadata is preserved and clear missing H1/path
fields are derived locally; Phi remains the fallback for ambiguous notes.
Use `gemma3:12b` for full semantic analysis and
`embeddinggemma:300m` for candidate retrieval. These roles are distinct; an
embedding model cannot generate suggestions.

Two full-analysis candidates worth comparing:

| Model                | Role and tradeoff                                                                                                      | Evidence                                                                                     |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `gemma3:12b`, Q4_K_M | Primary candidate: larger instruction model, multilingual technical text; about 8.1 GB installed weights               | Installed locally; full-corpus edit quality is not established                               |
| `qwen2.5:7b`, Q4_K_M | Lower-memory alternative: instruction/code-oriented model without Qwen3's thinking-mode toggle; roughly 4-5 GB weights | Recommendation to benchmark, not a local measurement; requires a separate download if absent |

Phi's installed Q4_K_M weights are about 2.5 GB; the embedder is about 0.6 GB.
File sizes are not runtime RAM requirements. Context/KV cache, inference
buffers, macOS, Chrome and the IDE also consume unified memory.
Gemma's advertised 131k context does not imply that window fits on this Mac.
Do not start parallel document workers or keep several large models resident.
The pipeline runs batches and unloads its embedding/enrichment models between stages.

The installed `embeddinggemma:300m` reports a 2048-token input context.
KBM-flow currently permits server-side prefix truncation for embeddings, so
one vector does not represent every section of a long note. Include a long
note in retrieval testing; generation context and embedding context are separate.

Start from `example-configs/config.m2-24gb.json`, replacing repository/database
paths for your own copy. It does not modify the normal/default model choice.
Automatic context sizing remains available (`ollama_num_ctx: 0`); it caps at
model capability, not available RAM. Inspect memory pressure on the longest
pilot document before attempting the full corpus. A manually pinned smaller
window can truncate input, so check prompt size rather than blindly lowering it.
Do not use the full model context as a performance setting.

## What the stopped Phi run showed

The local test scanned and embedded 278 Markdown files from
`example-knowledge-real-test`. It ran enrichment and analysis against the same
Phi model using two aliases. Before stopping at document 59:

- 55 documents completed both generation stages.
- Three documents hit the 600-second request timeout.
- 20 pending suggestions remained: 10 analysis and 10 enrichment.
- Nothing was accepted or applied by the run.

Invalid edits included moving a README to script/config paths mentioned in its
body, removing headings/caveats, corrupting regex backslashes, inventing
publication dates and setting review/lifecycle fields. Self-reported confidence
was often 0.9 or 1.0 despite those errors.

This does not prove a larger model fixes the problem. The previous prompts
also encouraged metadata-only full-document regeneration and asked for merges
without supplying target bodies. Both the contract and local enforcement need
to improve before comparing models.

## Bounded Gemma/Phi pilot (2026-10-18)

A four-document copy finished in 472 seconds with the new prompts and guards.
Phi enriched three notes in about 13-16 seconds each, but timed out after
180 seconds on `languages/shell.md` (3979 source bytes). That note did not
reach full analysis, so this was not a clean four-document pass.

Gemma completed the other three analysis stages in approximately 73, 86 and
87 seconds, including request/load overhead. The two pending suggestions were
reviewed as sensible metadata-only changes: link the shell-assets README to
the shell-language note, and tag the CodeQL/ReDoS note. Both bodies, including
the regex escapes, remained byte-for-byte unchanged.

One Chromium-note link to `tools/rspec.md` was rejected because that target
was absent from the small copy. It exists in the original corpus and was
already cited by the source note; this is a fixture-coverage limitation, not
evidence that Gemma hallucinated the path. Include reference targets in the
next qualification set.

Only Gemma was listed as resident during a sampled analysis request. Peak
memory was not measured. Nothing was accepted/applied, and the overall
10-minute limit was not reached. These results support a larger controlled
pilot, not restarting all 278 documents yet: Phi's timeout still needs
investigation, and long-document behavior remains unqualified.

## Full analysis-only corpus run (2026-10-18/19)

One local run scanned and embedded 289 Markdown documents from
`engineering-memory`, using Phi enrichment before Gemma analysis. Measured
elapsed times include request/loading overhead and are not model specifications:

- Total monotonic elapsed time: about 13h51m.
- Phi stage: about 5h57m; 264 successes, 24 ten-minute timeouts and one
  token-repeat abort. Those 25 documents did not reach Gemma under the old
  fail-before-analysis behavior.
- Gemma stage: about 7h53m; all 264 eligible documents completed without a
  per-document generation error.
- 181 pending suggestions: 168 locally rendered metadata patches and 13
  moves. All 168 patch bodies matched their original files byte-for-byte;
  this is structural preservation, not a semantic approval of the proposals.
- Local guards rejected 40 unsafe edits. No suggestions were accepted or
  applied, and all 289 scanned Markdown files retained their source hashes.

Phi produced zero actionable patches and zero recorded patch rejections. Missing
frontmatter does not prohibit enrichment proposals. Successful raw replies were
not retained, and Gemma replaced the shared document metadata, so the exact
reason for those no-op patches cannot be recovered from the final database.
The run does not measure whether enrichment improved suggestion quality;
that historical run ranked candidates with embeddings/shingles only. The current
code also uses exact local-name matches as candidate evidence; its effect on
link precision/recall has not yet been measured against live Ollama.

The revised process keeps Phi optional after generation failures and bounds it
to 120 seconds/512 output tokens by default. In `local-first` mode, valid
frontmatter is preserved and only missing title/tags with clear H1/path
evidence bypass Phi; ambiguous notes still use it when configured. The local
stage also works when no distinct fast model is configured.
The local entity index uses visible note names and the optional
`frameworks/README.md` catalog as candidate hints. These changes are regression-
tested with fakes, not yet benchmarked against live Ollama. Compare a fixed,
hand-picked real-document set with `enrichment_mode: always` and `local-first`
before another full run; the analysis model still runs for all pending documents.

The optional `example-knowledge/llm-eval` set pins 14 real note paths and hashes
plus the `taxonomy.yaml` sidecar without checking note bodies into the
application repo. `make example-ollama
TIER=llm-eval` materializes that allowlist under ignored `data/` and runs
analysis-only against the copy. Run it sequentially on the local M2 and review
its rubric and timings; do not treat a 14-note sample as a full-corpus quality
claim.

## Safeguards and their limits

- Enrichment may patch only `title` and `tags`; full analysis may also patch
  `related` and `contradicted_by`. Go renders the complete metadata edit while
  preserving the original body.
- Other fields, including provenance, lifecycle, privacy and custom fields,
  must remain unchanged. Missing fields are not an invitation to invent values.
- Invalid values and unresolved/traversing graph paths cannot reach apply.
- Body proposals must preserve the original words in order, and code spans/
  blocks verbatim. Merges must preserve both bodies and require complete
  target context. Corrections or intentional shortening require manual edits.
- Moves preserve the document extension, cannot overwrite, and cannot treat
  instructions inside a note as actions on other files.
- The same content checks protect old accepted rows at apply time. Rejected
  model edits are counted and logged with reasons.

Temperature `0` reduces sampling variation; JSON schemas constrain syntax.
Neither validates facts, link usefulness, move rationale or deletion evidence.
Review diffs and retain backups before applying.

## Qualify before scaling

1. Use a separate copy and fresh database. Include the shell-template README,
   the regex note, the Chromium CI note, a positive link case and a long note.
2. Keep the same prompts, retrieval model, context settings and documents for
   each full-analysis candidate. Do not compare different-sized prompts as
   a speed ranking.
3. Inspect every proposed diff: no lost text/code, invented facts/provenance,
   wrong-file moves or unsupported relationships. Zero suggestions is valid
   for an already-correct document, but does not prove useful recall.
4. Record per-stage latency, timeouts, rejected edits, reviewer acceptance,
   context size, quantization and memory pressure. Separate cold loading from
   warm inference when reporting timings.
5. Run all documents only after this pilot is acceptable. Do not accept or
   apply the stopped Phi batch merely because a new model is selected.

Upstream references for checking capabilities/tags:
[Gemma 3](https://ollama.com/library/gemma3),
[Qwen2.5](https://ollama.com/library/qwen2.5),
[Phi-4-mini](https://ollama.com/library/phi4-mini).
Capability claims are not benchmark results for this repository.
