# Target server: Knowledge Base MCP Server

The repository is consumed by `knowledge-base-mcp-server`
(github.com/jeanibarz/knowledge-base-mcp-server), which indexes notes for
semantic + filtered retrieval. Its conventions are authoritative for this
corpus:

## Frontmatter whitelist

Only these keys are lifted onto indexed chunks; other keys are stored as
non-filterable "extras". Write them exactly as named:

- Provenance: `title`, `authors`, `published`, `arxiv_id`, `ingested_at`
- Lifecycle: `status`, `review_status`, `tier`, `confidence`,
  `last_verified_at`, `manual_edits`, `contradicted_by`, `promote_model`
- Retrieval: `tags`, `kb_policy`
- Tool-local extra (curated here, ignored by the server): `related`

## Allowed values

- `status`: draft | active | approved | published — or the deprecated family:
  archived | deprecated | dormant | obsolete | retired | stale | superseded
- `review_status`: approved | needs-review (deprecated-family values are
  also meaningful)
- `tier`: working | validated | wisdom — written by `kb promote`; do not set
- `confidence`, `relevance_score`: numbers (0-1 for confidence)
- `last_verified_at`, `published`, `ingested_at`: ISO dates or datetimes
- `manual_edits`, `kb_policy.no_llm_context`: true | false
- `tags`, `related`, `contradicted_by`: lists of strings — `related` and
  `contradicted_by` hold repository-relative document paths
- `kb_policy.resource_read`: allow | local_only | deny;
  `kb_policy.sensitivity`: string

## Usage rules

- Frontmatter `tags` power the `tags` filter only; the embedding never sees
  them. Important tag terms must also appear in the body text.
- Only `title`, `tags`, `related` and `contradicted_by` may be changed by the
  full-analysis model. The enrichment model may change only `title` and `tags`.
- All other frontmatter fields are read-only. Never add, remove or change
  provenance (`authors`, `published`, `arxiv_id`, `ingested_at`), lifecycle
  (`status`, `last_verified_at`, `review_status`, `tier`, `manual_edits`,
  `confidence`, `promote_model`), privacy or unknown/custom fields.
- Do not derive publication/verification dates from file modification times
  or examples in the document. Do not default lifecycle fields to draft.
- Never add, remove or change `kb_policy.no_llm_context`; it is a human-owned
  privacy control. KBM-flow does not send a document to the model when this
  value is true or when its `kb_policy` cannot be interpreted safely.
- Never add or remove the `stub` tag; it is a human-owned marker for
  placeholder notes that exist only as link targets. Stub notes are never
  sent to the model, so a patch touching `stub` is always suspect.
- Never add, remove or change `kbm`; it is the tool's operational metadata
  block (link-check results and similar bookkeeping), not note content.
- Preserve every existing frontmatter key you are not deliberately changing,
  including keys outside the whitelist.
- Frontmatter links are relative to the repository root, not to the note:
  use `languages/typescript.md`, never `../../languages/typescript.md`.
  Link only to known document paths. Scripts and configs are not note targets.
- Keep note bodies small and single-topic with an intro sentence and `##`
  sections — that is how the server chunks and retrieves them.
