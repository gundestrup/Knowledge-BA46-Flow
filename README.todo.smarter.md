# Smarter evidence dependents

Status: The branch implementation and review fixes are complete. Analysis
evidence is now v3 and records inbound graph edges by source and kind.
Database schema v20 adds the lexical FTS5 index; v19 provenance remains
source-version-scoped. Both migrations are additive and preserve existing
analysis stamps and evidence.

Historical production-copy measurements on the earlier branch state were
207 → 170 → 128 queued documents after successive bookkeeping and catalog
changes. The 128-item run was stopped after queue reporting; it did not
complete those analyses. A separate older 584-document copy measured 130
queued before the entity-source bound and 98 after it. The latest comparison
used fresh copies of the current production database: `main` estimated 435
items and the current branch worktree estimated 134 (9 direct + 125
dependents). See §12; these are read-only queue estimates, not completed
analysis runs.

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

## 2. Why it happened (pre-fix mechanics)

1. Before the fixes, each analyzed document recorded a v2 evidence set into
   `analysis_evidence_json`: `fm` (displayed candidates), `full` (consumed
   bodies), `inbound` (who links to me), `siblings`, `validation_context`.
   See `internal/analyzer/evidence.go` and
   `analyzer.go:marshalAnalysisEvidence`.
2. The dependents sweep (`evidenceDependents`) re-evaluates every doc:
   `recordedContextChanged` compares `ev.Inbound` against the _current_
   `llmVisibleInbound`, siblings, and validation context. Any diff →
   dependent → full LLM re-analysis.
3. The earlier `InboundLinks` path could count `document_references` rows of
   **all kinds** —
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

This section records the historical failure that motivated the fixes; its
predictions about a future fm-only/fm+body oscillation are no longer current.

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
| Candidate recall                          | Hybrid: MinHash Jaccard + embedding cosine + FTS5 BM25 + entity/dir/fm signals                                | Lexical, literal and dense retrieval signals are combined   |
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
| SQLite FTS5 (already embed `modernc.org/sqlite`) | Implemented as a bounded BM25 lexical rank arm; it breaks low-semantic-score ties and does not displace strong embedding/overlap candidates.    |
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

## 6. Historical options (superseded by the implementation below)

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

## 7. Original plan (superseded)

The phases and estimates in this archival section were written before
implementation. They are not the current status or operational guidance.
The implemented behavior and remaining work are summarized in §7.1.

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

### Historical immediate action (no longer current)

- The malformed-title example and predicted ~400-document oscillation
  describe the pre-fix incident only. The scanner now marks unindexable
  sources, and the inbound contract is stable; this prediction no longer
  applies.

## 7.1 Current implementation, migration, and follow-up

- The scanner writes a `reference_versions` marker even for unreadable,
  malformed, or mid-scan-changed sources. One bad document cannot switch the
  corpus between indexed and fallback inbound results.
- Inbound evidence is frontmatter graph topology only (`related` and
  `contradicted_by`); body links remain indexed for link repair and do not
  fan out to target analysis. Indexed and fallback lookup return the same
  source/kind pairs.
- Evidence v3 stores inbound `(source_path, kind)` edges. A `related` ↔
  `contradicted_by` change, any graph-edge removal, and any unproven addition
  trigger full analysis. Contradiction edges are explicitly labeled in the
  prompt.
- V2 evidence is upgraded lazily. If its visible inbound path set is unchanged
  and has no newly labeled contradiction context, evidence is restamped with
  no LLM call. If an old path disappears, current body links cannot prove what
  the path represented historically, so the document is conservatively
  analyzed once. This replaces the unsafe mixed body/graph restamp rule.
- Accepted `related` additions are restamped only when provenance matches the
  current source version and the edge still exists. A scan that observes edge
  removal deletes the provenance, so a later manual re-add is not
  misclassified. `contradicted_by` additions always analyze.
- Merge provenance is recorded on the merge target; expand provenance is
  recorded on the rewritten source. Moves/deletes are not treated as pure
  edge additions.
- Catalog detection uses at least 15 indexed Markdown body links, not graph degree.
  Catalogs remain in relation candidates, but their complete bodies are
  excluded from merge context. Evidence tracks merge bodies separately from
  body-derived candidate scores; catalog-only score drift can be restamped.
- The dry-run estimate is read-only and returns `dependent_reasons`, matching
  the cause breakdown in run summaries. The dashboard shows those causes.
- SQLite v18 → v19 adds `applied_edges.source_sha256` with an empty default;
  v19 → v20 adds the FTS5 virtual table. It backfills on scan without
  recalculating analysis.
  Legacy provenance rows intentionally cannot suppress invalidation. The
  additive migration preserves analysis hashes, lane stamps, and evidence;
  it does not recalculate the corpus.
- Existing v2 evidence needs no blanket migration: unchanged related-only
  paths are restamped; ambiguous path removals and newly visible contradiction
  labels are analyzed once. Estimates preview this without writing.
- Regression coverage includes mixed body/graph removals, edge-kind changes
  in both directions, applied contradiction additions, provenance retirement
  after removal/manual re-add, merge-target provenance, catalog candidate
  retention/body exclusion, migration preservation, and estimate cause
  reporting.
- Same-snapshot dry-run before the entity bound: 9 direct + 121 dependents
  (130 total). After the bound: 9 direct + 89 dependents (98 total), a
  reduction of 32 queued documents. The entity-source cause fell from 73 to
  35; the 16-consumer limit is per changed source, so overlapping sources can
  still select more than 16 unique consumers. Seven more documents appear
  under the embedding-neighbor cause after the entity arm is bounded; these
  remain queued rather than being silently skipped. The cap is heuristic
  projection, not proof that lower-ranked consumers are irrelevant.
- Post-bound cause counts: 17 consumed-evidence changes, 22 embedding
  neighbors, 1 entity-evidence change, 35 entity-source changes, and 14
  inbound-link changes. The FTS5 table was added to the isolated DB by
  migration but not backfilled during this read-only dry-run; the next scan
  fills it. Scanner/index lifecycle and retrieval are covered by local tests.
- These measurements used an earlier saved copy and are superseded for
  current-production comparison by §12.

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
else); KBM-flow now tiers the same way (semantic graph changes → re-analyze,
provable bookkeeping changes → restamp).

Things KBM-flow could still borrow:

- **Quiesce check:** the server's changed-during-scan retry is analogous
  to KBM's "source changed since scan; rescan" failure — KBM already has
  the equivalent via strict-hash verify; aligned behavior.
- **Per-model storage isolation:** server stores indexes per
  `(provider, model)` — mirrors KBM's "model stamps are atomic; ranking
  must never mix incompatible models."
- **Hybrid retrieval:** the current upstream README/docs describe dense,
  lexical, hybrid, and auto modes. Hybrid fuses BM25 and dense result ranks
  with RRF; optional cross-encoder reranking and section/metadata/summary
  views remain retrieval features, not document-invalidation signals. See
  §12 before considering these for KBM-flow.

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

## 10. Remaining follow-up

- The fresh production-copy comparison is recorded in §12. Do not infer
  actual runtime savings from the dry-run; no analysis models were called.
- Investigate the 51 legacy inbound dependents. Only restamp the old path-only
  records if historical edge kinds can be proved; otherwise allow this likely
  one-time v2-to-v3 convergence to analyze conservatively.
- Add per-document dry-run explanations, including primary and secondary
  causes, trigger paths, inbound edge deltas, and projection ranks. Current
  counts identify categories but not all overlaps or the exact affected notes.
- Measure entity-source and embedding projection precision on a labeled,
  local corpus before introducing a corpus-global fan-out budget or a deferred
  no-LLM refresh lane.
- Verify the MCP reindex trigger only fires after a complete apply/run, not
  while analysis is still mutating the queue.
- Evaluate FTS5 and any hybrid reranking with candidate-recall metrics.
  Retrieval changes are not expected to reduce the analysis queue by
  themselves. Goldmark and server-index reuse remain optional, measurement-led
  work.

## 11. Implementation files

- `internal/scanner/references.go` and `internal/database/references.go` —
  stable reference readiness, typed inbound graph queries, body-link hub
  counts, and applied-provenance cleanup.
- `internal/database/migrate.go` and `internal/database/apply.go` — additive
  v19 provenance migration and source-version-scoped records.
- `internal/analyzer/evidence.go`, `validation.go`, and `analyzer.go` —
  evidence v3, kind-aware prompt context, conservative legacy handling,
  catalog body exclusion, and cause reporting.
- `internal/analyzer/estimate.go` and `internal/server/ops.go` — read-only
  dependent-cause estimates exposed to the dashboard/API.
- `internal/database/search.go` and `internal/database/search_test.go` —
  optional migration-safe FTS5 candidate retrieval and lifecycle coverage.
- `internal/review/review.go` — expand and merge-target provenance.
- Analyzer, database, review, scanner, and server tests cover regressions and
  additive migration state preservation. User-visible behavior is documented
  in `docs/FUNCTIONS.md`, `docs/API.md`, and `CHANGELOG.md`.

## 12. Current production-copy dependent audit and retrieval references

### Branch comparison

On 2026-11-14, separate SQLite backups of the production `state.db` were
opened with `main` at `f9fe99a` and the current `smarter-evidence-dependents`
worktree (including its uncommitted changes). The source database was schema
v17 with 628 rows, 44 marked missing; both dry-run APIs considered 584
documents. `main` estimated 9 direct + 426 dependents = 435 total. The current
worktree estimated 9 direct + 125 dependents = 134 total: 301 fewer queued
analyses (69.2%). No scan or model generation ran. The current copy migrated
to schema v20; the production database remained v17 and untouched.

The corpus working tree had modified and untracked files at measurement time.
These estimates used the existing scan snapshot and current files without
running a scan, so a real run's preflight scan may change the counts.
`main` did not expose a cause breakdown; the table below describes the current
branch only.

The dependent reason counts are first-hit labels: each queued dependent gets
one primary reason, so they sum to 125 but do not show every trigger that may
also apply to the same note:

| Primary cause              | Dependents | Interpretation                                                |
| -------------------------- | ---------: | ------------------------------------------------------------- |
| Inbound links changed      |         51 | Mostly the legacy path-only evidence transition; see below.   |
| Entity source changed      |         35 | Speculative, prompt-visible entity-candidate projection.      |
| Embedding neighbor changed |         22 | Speculative, corpus-calibrated nearest-neighbor projection.   |
| Consumed evidence changed  |         16 | A recorded candidate/body/score input changed.                |
| Entity evidence changed    |          1 | A changed entity source was visible in the candidate context. |

### The 51 inbound dependents

All 578 stored local evidence records in the production copy are v2. A
read-only comparison of their old path-only `inbound` sets with current
frontmatter graph references found 53 target records with path-set deltas:
109 old paths are no longer graph edges, while 27 graph paths are new. The
removed paths still resolve to current `kind='body'` references; 41 are from
`languages/README.md`, 5 from `frameworks/css/README.md`, and 3 from
`cms/README.md`. The dry-run marks 51 unchanged targets as dependents; direct
pending items and visibility/queue rules make the raw target count differ.
All active sources had a current `reference_versions` marker, so the old
indexed query path was in use. The 27 new graph paths are not body-link
bookkeeping; without historical applied-edge provenance, their additions
remain valid analysis triggers.

This strongly indicates a one-time representation-convergence wave. The old
`main` indexed inbound query did not filter `document_references.kind`, while
the current query uses only `related` and `contradicted_by`. However, v2 saved
only source paths, not edge kinds, and this database has no historical
reference snapshot or applied-edge provenance for that old state. A source
could have had both a body link and a graph edge when the target was analyzed,
then lost the graph edge while retaining its body link. Therefore the current
body-only reference is not proof that the old evidence was body-only.
Restamping all 51 would repeat the mixed-edge safety bug; do it only if an
archived source/reference snapshot proves the old kinds. Otherwise this is a
conservative, likely one-time cost: after v3 evidence is written, the same
path-set delta should not recur. In a stable repeat dry-run, at most these 51
primary-attributed items disappear; fewer may leave the queue if other
evidence arms also select them. That post-run behavior has not yet been
measured.

### Further queue optimization

- The 35 entity-source dependents are already filtered to prompt-visible
  candidates and capped at 16 per changed source. Several changed sources can
  still select 35 unique notes. A corpus-global cap could reduce this arm, but
  would skip lower-ranked possible context; do not silently restamp them.
- The 22 embedding dependents are also a union of per-source top-16 sets above
  the calibrated 95th-percentile cosine floor. A global budget, stricter
  threshold, or explicit deferred-refresh lane could reduce immediate LLM
  work, but needs measured relevance and an honest stale/deferred state.
- The 16 consumed-evidence and 1 entity-evidence items have stronger
  prompt-evidence reasons. Keep them on the analysis path unless per-note
  diagnostics show the changed field was not actually prompt-visible.
- Highest-value next diagnostic: expose per-note primary and secondary
  triggers in dry-run, including source path, edge kind/addition/removal,
  evidence version, candidate rank, cosine/overlap score, and whether the
  candidate body or only its annotation changed. This is low risk and enables
  measurement before changing invalidation semantics.

### What the retrieval projects contribute

Reviewed upstream documentation on 2026-11-14:

- [`knowledge-base-mcp-server` README](https://github.com/jeanibarz/knowledge-base-mcp-server)
  and [authoring/retrieval notes](https://github.com/jeanibarz/knowledge-base-mcp-server/blob/main/docs/authoring-knowledge.md)
  describe SHA256-based per-file incremental indexing, heading-aware Markdown
  chunks (1000-character size, 200-character overlap by default), dense FAISS
  plus BM25, and `dense`/`lexical`/`hybrid`/`auto` retrieval. Hybrid uses
  Reciprocal Rank Fusion (RRF), so rank lists with incomparable raw score
  scales can be combined. Optional reranking and
  multi-view section/metadata/summary retrieval map results back to canonical
  source chunks ([reranker/feature flags](https://github.com/jeanibarz/knowledge-base-mcp-server/blob/main/docs/feature-flags.md),
  [multi-view change](https://github.com/jeanibarz/knowledge-base-mcp-server/pull/580)).
- [txtai's hybrid-search example](https://github.com/neuml/txtai/blob/master/examples/48_Benefits_of_hybrid_search.ipynb),
  [search implementation](https://github.com/neuml/txtai/blob/master/src/python/txtai/embeddings/search/base.py),
  [fusion implementation](https://github.com/neuml/txtai/blob/master/src/python/txtai/embeddings/search/hybrid.py),
  and [scoring documentation](https://neuml.github.io/txtai/embeddings/configuration/scoring/)
  show sparse BM25 and dense retrieval over-fetching up to 10x the requested
  result count before fusion. txtai uses rank fusion for unnormalized scores,
  weighted fusion for normalized scores, and log-odds fusion for calibrated
  BB25 scores; its examples compare methods with nDCG, MAP, recall, and
  precision metrics.
- These are retrieval/candidate-quality patterns, not solutions to
  cross-document analysis invalidation. KBM-flow already has incremental
  hashes, FTS5, and embeddings. The useful next lesson is evaluation: maintain
  a small local set of changed-note → relevant-candidate judgments and compare
  Recall@16/nDCG@16 for the current ranker against RRF/over-fetch variants
  before changing prompt candidates. KBM's lexical arm currently breaks ties
  only when candidates lack strong embedding/shingle scores; RRF is a
  plausible retrieval-quality experiment, not a proven queue reduction. Keep
  chunk or multi-view hits mapped to their canonical document; evidence stamps
  and merge/apply safety remain document-level. Do not add txtai (Python) as a
  dependency to this Go application.
- Hybrid ranking, chunking, reranking, or MCP index reuse may improve retrieval
  quality or embedding cost, but none is expected to remove the 51 legacy
  inbound cases or directly lower the analysis queue without separate
  invalidation changes. FTS5 was not backfilled in this dry-run because that
  would require scanning; its production-corpus retrieval benefit remains
  unmeasured.
