# Review, Queue Reconciliation and Document Identity

## Write scope

`write_scope` is a config field enforced during model proposal validation,
acceptance and apply, including proposals accepted by an older binary:

| Value                      | Allowed edits                                                                                          |
| -------------------------- | ------------------------------------------------------------------------------------------------------ |
| `metadata-links` (default) | Allowlisted frontmatter and exact, recorded-rename link repairs; no body additions or filesystem moves |
| `content`                  | The same, plus validated additive body changes; no moves, merges or deletes                            |
| `all`                      | Explicit opt-in to reviewed moves, merges and trash-based deletion, with all existing safety checks    |

No scope permits changing protected provenance, lifecycle, privacy or custom
metadata. `document_id` is hash-neutral like `kbm`: scans assign it in place,
proposals preserve it automatically, and `identify --prepare` migration
proposals pass a separate exact-insertion validator.
Policy files and source hashes are always checked. Blocked accepted proposals
stay accepted; apply reports them as `blocked` rather than losing the decision.
Withdraw acceptance with `withdraw` to choose another alternative.

## Review by note

The web review groups proposals by source note. Change-type facets are
frontmatter, links, body, identity and file operations. Origin remains a separate
facet (analysis, enrichment, deterministic, expansion, freshness, identity).
Filters select notes and retain their other alternatives so conflicts are not
hidden. A type facet is not permission to apply only that part of a full-file
replacement. Choose one validated snapshot; other old versions are superseded
after it applies.

## Reconcile existing proposals

Restart the production server with the new backend before using this workflow.
No cleanup runs automatically during startup.

```bash
./KBM-flow reconcile --config /path/to/.kbm/config.json
./KBM-flow reconcile --config /path/to/.kbm/config.json --confirm PREVIEW_TOKEN
./KBM-flow withdraw ID --config /path/to/.kbm/config.json
```

`reconcile` only previews unless explicitly confirmed. It classifies the complete pending/accepted
queue, including missing sources, as valid, conflicting, blocked, invalid, stale,
missing or unavailable. Only invalid/stale/missing proposals are candidates for
retirement. Permission blocks, conflicts and I/O errors are not obsolete edits.
The second command requires the current preview token and revalidates live
content, policy, rename evidence and queue state. Changed previews must be
reviewed again. Retirement is atomic, keeps the rows as `superseded`, and records
machine reasons and batch IDs in `review_events`; it never fabricates human
rejection feedback or accepts/applies a proposal.

The web UI exposes the same preview and explicit confirmation. No model,
embedding, MCP write, repository edit or reindex trigger is involved.

## Explicit identity initialization

```bash
./KBM-flow identify --config /path/to/.kbm/config.json
./KBM-flow identify --config /path/to/.kbm/config.json --prepare --note notes/example.md
```

`identify` previews scanned notes. `--prepare` creates pending UUID initialization
proposals for eligible notes; it does not write frontmatter. Notes with existing
pending/accepted proposals are blocked so initialization cannot invalidate a
review queue silently. Review and accept each identity proposal normally, then
run `apply` and `scan`. Existing UUIDs are preserved; duplicates require manual
resolution. Normal scanning never generates IDs.

Identity proposals may only insert a top-level `document_id` UUID. Other metadata,
body, links and live `kbm` data remain unchanged. The ID participates in the
canonical hash, so later edits still require version checks. The MCP server
accepts an unknown string field but does not use it as native identity.

After IDs are registered by a scan, a unique UUID can connect an externally
renamed-and-edited document to its previous database row. Its changed hash still
supersedes old proposals. Reverse references are indexed by target path/UUID and
source version. Repairs recheck the live target UUID and each referring note's
hash, preserve Markdown-relative paths, queries, fragments and code, and never
rename a document.

Duplicate IDs and reused/live old paths are not guessed through. Copies need
their own reviewed identity. Outgoing links of a moved referring note require
particular care: captured identity changes are reported, not automatically
rewritten when a syntactically valid URL could mean a different document.
Rescan before repair and review outgoing links after any external move.
