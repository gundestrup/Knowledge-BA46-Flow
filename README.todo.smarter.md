# TODO — Smarter evidence dependents (stop re-analyzing the corpus on index edits)

Status: Phase 0 + Phase A implemented on branch `smarter-evidence-dependents`
(marker rows for unindexable sources; inbound evidence = frontmatter graph
edges only on both query paths). Phases B/C (tiered sweep, apply-originated
edge handling, separate dependent counts) remain.

## 1. The observed problem

On 2026-11-12, `run` queued **463 documents** (~80% of the 574-doc
engineering-memory corpus) although only ~10 files changed on disk.
Measured against `.kbm/state.db`:

- Direct queue (`analysis_input_sha256 <> analyzed_sha256`): ~10 docs —
  the actually edited/new files.
- Everything else was **evidence dependents**: docs whose recorded
  analysis context drifted even though their own bytes did not change.
- Arm breakdown (sampled mid-run, ~160 docs already processed):
  - inbound-set drift from **body links only** (catalog/index tables):
    **113 docs** — sources: `lessons-learned/README.md` (168 outbound
    edges), `databases/README.md` (30), `frameworks/javascript/README.md`
    (24), `frameworks/css/README.md` (83 outbound), `tools/README.md` (55)
  - inbound-set drift from **fm `related`/`contradicted_by` edges**: 9 docs
  - recorded-sibling removal: 0 docs
  - validation-context drift: 1 doc
- Cost: `analysis` stage averages **~107 s/doc** on local `gemma3:12b` —
  the full queue ≈ **14 hours** of inference (job 130 was still running
  at ~160/463 after ~5 h).
- The 371 `status='error'` docs are stale noise from the 2026-11-05 cloud
  run (`gpt-oss:120b-cloud`) that hit the Ollama monthly quota (HTTP 429)
  and **failed 487 docs** — the backlog was never drained, so evidence
  drift accumulated on top.

## 2. Why it happens (mechanics)

1. At analysis time each doc records a v2 evidence set into
   `analysis_evidence_json`: `fm` (displayed candidates), `full` (consumed
   bodies), `inbound` (who links to me), `siblings`, `validation_context`.
   See `internal/analyzer/evidence.go` and
   `analyzer.go:marshalAnalysisEvidence`.
2. The dependents sweep (`evidenceDependents`) re-evaluates every doc:
   `recordedContextChanged` compares `ev.Inbound` against the _current_
   `llmVisibleInbound`, siblings, and validation context. Any diff →
   dependent → full LLM re-analysis.
3. `InboundLinks` counts `document_references` rows of **all kinds** —
   `kind='body'` (plain markdown links, incl. index/catalog tables) _and_
   fm `related`/`contradicted_by`/`links`. `IndexedInboundLinks`
   (`internal/database/references.go:220`) applies no kind filter, while
   its doc comment still claims frontmatter-only — a real inconsistency.
4. So appending a row to an index README changes the inbound set of the
   newly-linked target → the target is queued for a full ~107 s analysis.
   A bulk graph edit (the Nov 7–8 commits: 500 + 232 files, new index
   tables, corpus-wide `related:` edges) invalidates hundreds at once.

The 0.3.0 fix (`TestCatalogIndexEditRefreshesOnlyNewInboundTargets`)
already protects _unchanged_ catalog linkers — only docs whose inbound
set actually changed queue. That is correct-but-expensive: a mass index
edit still costs one full analysis per newly-linked target.

## 3. Second observation (2026-11-13): ~95 docs, and the index-poisoning bug

Job 130 finished: `Analyzed 421; queue 5 direct + 458 dependents` —
confirming the model. After ~8 accepted `related`-type suggestions were
applied, job 131 queued **95 docs**. Decomposition:

- **~19 genuine fm-edge dependents** — targets of the accepted edits
  (new `related`/`links` sources: `lessons-learned/README.md` 6,
  `repos/logo-generator-flow.md` 5, `libraries/ruby/lightningcss.md` 3,
  `lessons-learned/alpine-init-listener-script-order.md` 3, …).
- Bounded arms — embedding neighbors (≤16/changed doc), entity mentions,
  new-file inbound, consumed-evidence hits.

### Root-cause bug found: one malformed file poisons the whole index

`lessons-learned/go-build-vcs-stamping-ci-containers.md` (new at exactly
job 130's scan start) has malformed frontmatter:

```yaml
title: go build "error obtaining VCS status: exit status 128" in CI containers
```

Unquoted `"` → `yaml: mapping values are not allowed`. Cascade:

1. `indexDocumentLinks` returns early on parse failure **without** writing
   `reference_versions` (`internal/scanner/references.go:46-49`).
2. `IndexedInboundLinks`' readiness check is **global and binary**: one
   unindexed doc → `incomplete=1` → the whole corpus falls back to the
   fm-only `InboundLinks` scan.
3. Therefore **all 421 of job 130's analyses recorded fm-only
   `ev.Inbound`** (verified: `languages/php.md` recorded 3 fm linkers;
   the index actually holds 10 inbound sources incl. `kind='body'`).
4. The doc is still malformed → job 131 keeps recording fm-only sets.

### The latent wave — fixing the YAML triggers another ~400-doc queue

389 docs now hold fm-only inbound records. The moment the malformed
YAML is repaired, the index heals → `InboundLinks` returns fm+body →
all 389 mismatch → **the next run re-queues ~400 docs**. The evidence
record does not capture a stable semantic fact; it captures whichever
representation (fm-only vs fm+body) the index happened to offer that
run — a representation bug, not conservative correctness.

## 4. Design assessment

- **Hub vs authority (HITS intuition):** a page with 50-200 outbound
  links is a catalog; its edges carry almost no per-target signal. The
  codebase already applies this discount to _entity mentions_
  (`entities.go` — "Catalog back-links are navigational boilerplate")
  but not to inbound edges.
- **Cost mismatch:** the only output usually wanted from inbound drift is
  reciprocal-`related`/backlink reconsideration — a set comparison that
  does not need an LLM.
- **`related` is tool-local:** the MCP server ignores it
  (`prompts/knowledge-base-mcp-server.md` — "curated here, ignored by the
  server"). Most of this churn maintains KBM-flow's own graph, not
  server-visible metadata. `contradicted_by` is the exception (real
  server field, high signal — keep full analysis for it).
- **Quality, not just cost:** showing the model "index page links to
  you" as inbound context invites `related: <index>` junk proposals —
  navigational noise pollutes the curated graph.
- **Representation stability (new):** evidence records must capture a
  _stable_ semantic fact. `ev.Inbound` currently flips between fm-only
  and fm+body depending on global index readiness (one malformed file!),
  producing phantom drift in both directions — a bug regardless of the
  tiering decision (§3).

## 5. Top-down technique audit — is each stage using the smartest tool?

Per-stage review of the whole pipeline (deterministic → heuristic NLP →
embeddings → LLM), answering "are we using NLP instead of LLM where
possible":

| Stage                                     | Technique in use                                                                                              | Verdict                                                     |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------- |
| Scan / dedup / fingerprints               | SHA256 variants + MinHash sketch (word 3-shingles, bottom-64)                                                 | Correct — cheap, no model                                   |
| fm validation, link repair, `document_id` | Fully deterministic + guarded                                                                                 | Correct — provable, reviewable                              |
| Entity linking                            | Closed-domain gazetteer (aliases from filename/title/H1/link labels/framework catalog, DF-gating, case rules) | Correct — generic NER libs would be _worse_ on this domain  |
| Candidate recall                          | Hybrid: MinHash Jaccard (literal overlap → merge) + embedding cosine (topical → link) + entity/dir/fm signals | Correct — hybrid lexical+dense retrieval is best practice   |
| Enrich                                    | Deterministic first (H1→title, path→tags), fast model only on miss                                            | Correctly tiered already                                    |
| Analyze                                   | Full LLM (merge/link/contradiction/summary judgment)                                                          | Irreducible — judgment calls                                |
| Expand / freshness                        | Generative + verification tasks                                                                               | LLM-appropriate                                             |
| **Dependents sweep**                      | **Every context drift → same ~107 s full analysis**                                                           | **The flaw** — bookkeeping drift pays judgment-stage prices |

Conclusion: the pipeline already applies "NLP/deterministic before LLM"
correctly almost everywhere. The defect is _routing_ — bookkeeping drift
is sent to the judgment stage — not a wrong analyzer choice. See
§4 and the plan in §7 for the fix.

### Library/tool survey (don't reinvent the wheel)

| Option                                           | Assessment                                                                                                                                      |
| ------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| SQLite FTS5 (already embed `modernc.org/sqlite`) | BM25-ish lexical recall nearly free; could add a lexical arm to candidates. Nice-to-have — the MinHash+embedding hybrid covers the space.       |
| Bleve (Lucene-in-Go)                             | Heavier; FTS5 covers the need with no new dep. Skip.                                                                                            |
| gonum/graph (HITS/PageRank)                      | Hub detection needs only an out-degree count — two lines, not a dependency. Skip.                                                               |
| goldmark (CommonMark AST)                        | The one real reuse candidate: `links.go`/`entities.go` hand-roll fence/inline-code/link parsing. Robustness win when touching that code anyway. |
| Reuse kb-mcp-server index (`find_related`)       | Biggest reuse win — replaces KBM's duplicate `embed` pass for candidate recall; one embedder, not two (§9).                                     |
| Go NLP libs (prose, go-porterstemmer…)           | Unmaintained or wrong domain (corpus is a closed dictionary). Skip.                                                                             |

**Re-evaluation with the survey in hand:** no library change alters the
core finding — there is no off-the-shelf "should this doc re-analyze"
tool; recorded-evidence invalidation is already ahead of common
practice. The needed fixes are design changes (Phases 0/A/B + the
apply-edge amendment), not dependencies.

## 6. Options (updated after the Nov-13 findings)

| Option                                         | Change                                                                                                                                                    | Effect                                                                                                                             |
| ---------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| 0. Index-poisoning fix (**new, prerequisite**) | `indexDocumentLinks` writes a `reference_versions` marker (empty refs) for unparseable/unreadable docs, or the readiness check tolerates unindexable docs | Stops one malformed file flipping the whole corpus between fm-only and fm+body representations — kills the oscillation at the root |
| 1. Kind filter                                 | `IndexedInboundLinks` filters to fm edge kinds (or drops `kind='body'`); **same fm-only contract on the fallback path** so both paths agree               | ~90% of dependents become free AND `ev.Inbound` becomes representation-stable; smallest diff                                       |
| 2. Out-degree cap                              | linkers with >K (~15-20) outbound edges don't count as inbound evidence                                                                                   | catches fm-based catalogs too; robust                                                                                              |
| 3. Tiered dependents (recommended)             | fm-graph drift → full analysis; body/catalog drift → deterministic backlink suggestion + silent evidence restamp                                          | ~75-85% fewer model calls; keeps semantic refresh; best long-term shape                                                            |
| 4. Apply-originated edge tier (**new**)        | fm-edge _additions_ whose source edge came from an accepted suggestion → deterministic reciprocal-backlink suggestion + restamp, not full analysis        | Covers today's ~19-doc apply fan-out; the human already made the link decision                                                     |

Note: option 1 is now proven _necessary_ beyond cost — recording fm-only
inbound makes the indexed and fallback `InboundLinks` paths agree, which
is the only way `ev.Inbound` stays stable across index-readiness flips.

## 7. Recommended plan (option 3 + amendments)

### Phase 0 — stop the oscillation (smallest, highest value)

1. In `indexDocumentLinks` (`internal/scanner/references.go`): on
   `frontmatter.Parse` failure (and the `hash != wantHash` /
   `ReadDocument` early returns), still record a `reference_versions`
   marker row for the current hash so the doc does not poison global
   index readiness — or make `IndexedInboundLinks`' readiness check
   tolerate docs that cannot be indexed (e.g. exclude parse-failed docs
   from `incomplete` rather than treating them as "not yet indexed").
2. Regression test: one malformed-frontmatter doc → other docs' inbound
   lookups still use the indexed path.

### Phase A — stable, cheap inbound contract (option 1)

1. Filter inbound evidence to fm edge kinds. Decide between:
   - drop `kind='body'` from `IndexedInboundLinks` (also fixes the
     doc-comment mismatch), or
   - keep body edges in `InboundLinks` for prompt display but record only
     fm linkers in `ev.Inbound`.
     The second preserves the "who mentions me" prompt display while
     removing the invalidation trigger — the safer change.
     **Either way, ensure the indexed and fallback paths return the same
     set** — that agreement is what makes `ev.Inbound` stable across
     readiness flips (see §3).
2. Regression test: index page gains a catalog row → target is _not_
   re-analyzed (extends `TestCatalogIndexEditRefreshesOnlyNewInboundTargets`).

### Phase B — tiered dependents (option 3 + option 4)

1. Split the recorded inbound set: `ev.Inbound` → `{fm: [...], body: [...]}`
   (schema bump for `analysis_evidence_json`; legacy v2 rows treat
   existing `inbound` as fm until re-stamped — conservative, may queue
   once, then converges).
2. Sweep rules:
   - fm-edge drift (from non-catalog linkers) → full analysis (unchanged),
     **except apply-originated additions** → deterministic reciprocal
     suggestion + restamp (the human just approved the edge — the target
     rarely needs a full pass to see it).
   - fm-edge _removals_ and `contradicted_by` changes → always full
     analysis (semantic, high-signal).
   - body/catalog drift → **silent restamp**: update `ev.Inbound` +
     `seen_*` stamps, emit a deterministic suggestion
     ("`X` now links here — consider reciprocal `related`") via the
     existing `SaveLinkFix`/deterministic-suggestion path, no model call.
   - Optional out-degree cap (~15-20) so even fm `related:` lists on index
     pages don't count as semantic edges.
3. Reuse the existing machinery: `db.RestampEvidence`, deterministic
   suggestion origin, `MarkRemovalChecked` pattern for ack-style stamps.
4. Update `docs/FUNCTIONS.md` + `CHANGELOG.md`; keep the `dependent`
   progress events but add a `detail` like
   `catalog inbound drift (restamped)`.

### Phase C — polish

- Surface the split in the estimate/dry-run (`dependents` →
  `dependents_model` + `dependents_free`) so the UI explains itself.
- Consider gating the deterministic backlink suggestion behind
  `write_scope` (it proposes fm link edits).

### Expected gain (measured corpus)

- Job 130 (~463): → **~60-100 model calls** (~75-85% less), ~14 h → ~2-3 h.
- Job 131 (~95): → **~20-40 model calls** (fm-edge stragglers +
  bounded arms) instead of ~95.
- Routine index maintenance (add one README row): ~0 model calls.
- Latent wave avoided: the 389 fm-only records **never oscillate** once
  inbound is fm-only by contract (Phase A) or split by kind (Phase B).
- Side benefits: no cloud-quota burn on bulk edits, smaller review queue,
  less `related: <index>` noise in suggestions.

### Immediate user action (no code change)

- Fix `lessons-learned/go-build-vcs-stamping-ci-containers.md` frontmatter
  — quote the title:
  `title: 'go build "error obtaining VCS status: exit status 128" in CI containers'`
  — but **expect the ~400-doc wave on the next run** (the fm-only →
  fm+body flip). Until Phases 0/A land, that wave is unavoidable; do it
  once rather than repeatedly.

## 8. How `knowledge-base-mcp-server` does indexing — is it better?

Architecture (`src/FaissIndexManager.ts`, `src/triggerWatcher.ts`,
repo CLAUDE.md):

- **Per-file content addressing:** every doc gets a SHA256 + a chunk
  manifest at `<kb>/.index/<path>.chunks.json`. Refresh compares the
  stored hash to the file — unchanged files are skipped entirely.
- **Incremental cost:** re-indexing pays _embedding calls only for what
  moved_ — O(changed files), not O(corpus). No cross-doc invalidation.
- **Two change surfaces:**
  - `REINDEX_TRIGGER_PATH` dotfile poller (`.reindex-trigger`) — external
    writers `touch` it; KBM-flow's `reindex_trigger_path` config writes it
    after `run`/`apply`. Root-level, deliberately outside the walker.
  - Opt-in recursive `fs.watch` (`KB_FS_WATCH=1`) for inline edits.
- **Quiesce + safety:** stat/hash re-checks during refresh skip files that
  changed mid-scan (`KB_REFRESH_FILE_CHANGED_DURING_SCAN`, retried next
  refresh); per-model write lock; versioned index dirs under
  `$FAISS_INDEX_PATH/models/<id>/` selected by `active.txt`; atomic
  version saves — concurrent writers can't corrupt the index.
- **Deletions:** missing chunk manifests drop the file's chunks.

### Is it better practice? — Mostly yes, but it solves an easier problem

| Aspect                    | MCP server index                                       | KBM-flow analysis                                                          |
| ------------------------- | ------------------------------------------------------ | -------------------------------------------------------------------------- |
| Output per doc            | pure function of the file's bytes (chunks + embedding) | function of the **corpus** (candidates, inbound links, siblings, taxonomy) |
| Invalidation              | trivially per-file (hash compare)                      | inherently cross-file — needs evidence tracking                            |
| Compute per change        | embedding only, no LLM                                 | full LLM reasoning pass                                                    |
| Cost of over-invalidation | small — re-embed one file                              | large — ~107 s/doc                                                         |

So the server can afford the simple "changed hash → re-embed" rule
because its derived state is _per-file mechanical_. KBM-flow can't copy
the rule directly — its output genuinely depends on other docs — which
is exactly why the evidence-record machinery exists. The lesson to take
isn't "be per-file" but **match invalidation cost to change
significance**: the server already does this (hash → re-embed, nothing
else); KBM-flow should tier the same way (fm-edge → re-analyze,
catalog-edge → restamp).

Things KBM-flow could still borrow:

- **Quiesce check:** the server's changed-during-scan retry is analogous
  to KBM's "source changed since scan; rescan" failure — KBM already has
  the equivalent via strict-hash verify; aligned behavior.
- **Per-model storage isolation:** server stores indexes per
  `(provider, model)` — mirrors KBM's "model stamps are atomic; ranking
  must never mix incompatible models."

## 9. Can we integrate better?

The boundary today is clean and correct: **server = read/retrieval
layer, KBM-flow = maintenance/write layer**, joined by
`reindex_trigger_path` (already wired — the trigger fired today at
18:36 while the run was still going, which is worth checking: the trigger
should ideally fire on _apply_, not mid-run).

Integration opportunities, ranked:

1. **Reuse the server's retrieval for candidates.** The server exposes
   `find_related` / `retrieve_knowledge` with hybrid embeddings. KBM-flow
   currently maintains its own similarity sketch + `embed` task +
   `embedded_sha256` stamps for candidate ranking. If KBM could query the
   server's index (when reachable, optional fallback to local), the
   `embed` pass and per-corpus embedding work could be dropped entirely —
   one embedder instead of two. Caveats: requires same embedding model
   family for comparability, a reachable server, and KBM's
   standalone/CLI mode must keep working offline.
2. **Share change detection.** The `.index/*.chunks.json` manifests are a
   per-file hash sidecar the server already maintains — KBM could use
   them as a cheap pre-filter before its own semantic hashing. Low value:
   KBM's scan is already fast; skip unless scans become a bottleneck.
3. **Do NOT reuse the server's index for link topology.** The server
   doesn't track document links — `document_references` is KBM-owned and
   stays that way.
4. **Trigger timing.** Verify `.reindex-trigger` is touched at the right
   moment (after `apply`/run completion, not during analysis) so the
   server doesn't index half-applied state.

## 10. Open questions before implementing

- Should `kind='body'` edges stay visible as prompt _display_ context
  (annotation) while excluded from invalidation? (Recommended: yes.)
- Out-degree cap value — measure out-degree distribution first
  (current max: `lessons-learned/README.md` = 168).
- Does the deterministic backlink suggestion belong in `write_scope`
  `metadata-links` only, or all scopes?
- Evidence schema: bump `v` in `analysis_evidence_json` or extend with an
  optional `inbound_body` field (backward-compatible, cheaper)?
- Should the sweep also restamp `enriched_*`/external lanes, or local
  only?
- Index readiness semantics: marker-row vs exclude-unindexable — which
  keeps the "fail closed on real staleness" guarantee?
- Apply-originated edges: can the dependents sweep tell "edge added by
  apply" from "edge added by hand"? If not, apply should write a marker
  (e.g. per-edge `restamp` flag or an acknowledged-generation bump)
  rather than trying to infer provenance later.

## 11. Files touched (when implementing)

- `internal/scanner/references.go` — `indexDocumentLinks` early-return
  paths (index-poisoning fix)
- `internal/database/references.go` — kind filter / kind-aware inbound
  query; readiness-check semantics; doc-comment fix
- `internal/analyzer/evidence.go` — split inbound arms, silent restamp
  path, apply-originated tier
- `internal/analyzer/evidence_state.go` (database) — evidence schema v3
- `internal/analyzer/analyzer.go` — `marshalAnalysisEvidence` split sets
- `internal/review/` (apply path) — mark apply-originated edges if the
  tiered rule needs provenance
- `internal/analyzer/analyzer_test.go` — extend catalog test; new tier
  tests; malformed-doc readiness regression
- `docs/FUNCTIONS.md`, `CHANGELOG.md`, possibly `docs/CONFIGURATION.md`
  (out-degree cap knob)
