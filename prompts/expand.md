# Task

Expand the CURRENT stub document into a useful engineering note by filling in
the category template shown in the repository context. The template's section
structure is the contract — follow it exactly so notes of one category stay
comparable and do not drift into encyclopedic prose.

Return exactly this JSON format:

{
"title": "short document title",
"summary": "short, factual summary",
"tags": ["tag1", "tag2"],
"reason": "what the template required and what the expansion adds",
"confidence": 0.0,
"proposed_content": "the complete expanded markdown document"
}

Rules:

- Keep every template heading and follow its intent. Write concise,
  opinionated engineering notes — what it is, when to reach for it, pitfalls,
  links — never an exhaustive reference or a Wikipedia-style overview.
- Preserve ALL existing stub content verbatim: headings, paragraphs, links and
  code stay in order; expand around them. Preserve code verbatim, including
  indentation and backslashes.
- Do not invent versions, benchmarks, links or facts. Where the template asks
  for facts you do not have, write what the stub itself supports and leave the
  section sparse — a human reviews this.
- `related` and `contradicted_by` must list repository-root-relative paths
  from the provided document list only. Keep existing links; add only ones
  with an explainable relationship (the stub's subject, its language or its
  category index are usually right).
- In proposed frontmatter you may remove the `stub` tag from `tags` when the
  expansion is complete — the tag marks the note as a placeholder, and the
  reviewer decides. Keep every other frontmatter field unchanged, never add
  or change `kbm`, `kb_policy`, `stub` additions, or any protected field.
- The proposed_content is a suggestion reviewed by a human before anything
  is written to disk.
