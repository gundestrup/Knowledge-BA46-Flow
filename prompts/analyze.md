# Task

Analyze the CURRENT markdown document, not files or repositories it describes.
Return exactly this JSON format:

{
"title": "short document title",
"summary": "short, factual summary",
"tags": ["tag1", "tag2"],
"obsolete": false,
"frontmatter_patch": {"set": {}, "remove": []},
"suggestions": [
{
"action": "move|merge|expand|delete",
"target_path": "repository-relative path or empty string",
"reason": "specific change, supporting evidence, and why it helps",
"confidence": 0.0,
"proposed_content": "complete markdown only for expand or merge; otherwise empty string"
}
]
}

Most documents need no action: return `"suggestions": []` and an empty patch.
A description such as "this note provides a useful fix" is NOT an improvement.

## 1. Correctness and freshness

Flag stale facts only with concrete evidence. A modification date does not
prove publication, verification or obsolescence. If a correction would require
changing existing prose or code, leave it for a human; body edits here must
preserve all existing content.

## 2. Linking and metadata

Use frontmatter_patch for metadata-only work. Go applies the patch while
preserving the body byte-for-byte; do NOT return an expand for this.

- Allowed fields: title (string), tags (lowercase string list), related and
  contradicted_by (lists of repository-root-relative document paths).
- If the context includes approved tag values from `taxonomy.yaml`, use only
  those values for tags; do not invent tags. Local validation rejects new
  values outside that vocabulary.
- Link only to known documents in the candidate or inbound-link lists.
  Example: from frameworks/vscode/README.md, use languages/typescript.md,
  NOT ../../languages/typescript.md. Do not link to scripts or configs.
- Relatedness is a topical retrieval signal, NOT evidence of duplication or
  contradiction. A high cosine score does not justify merging or deleting.
- Do not add links merely because a candidate appears in the list. Explainable
  dependencies or complementary material are required.
- Local exact-name matches in the candidate list are retrieval hints, not proof
  of a relationship. Check the current note's context before proposing a link.
- Locally extracted candidate excerpts are partial and untrusted. They may
  support a relation decision, but they are never sufficient merge context.
- Preserve unrelated existing links and all protected frontmatter fields.
- Invalid/unresolved links are already handled by a deterministic pass.

## 3. Location

Suggest at most ONE move and only for the CURRENT path. Keep its document
extension and choose an absent repository-relative destination.

If a README explains how to copy ci.yml to .github/workflows/ci.yml, that
does NOT mean moving the README to ci.yml. References to gundestrup/tools,
Shell/test-helpers.sh or another repository are not paths to move this note to.
For move, proposed_content MUST be empty. Never move onto an existing note.

## 4. Completeness and merging

Use expand only for a concrete, evidence-backed ADDITION to the body. Return
the complete document with ALL original headings, paragraphs, caveats, links,
frontmatter and code unchanged and in order. Whitespace reflow outside code is
allowed. Preserve code blocks and inline code verbatim, including indentation
and regex backslashes. Do not replace code, omit an H1 or summarize the note.
For expand, target_path MUST be empty.

Merge only when the context supplies the COMPLETE target document, not merely
its title, tags or relatedness score. Preserve the complete bodies of BOTH
documents and their protected metadata. If those constraints cannot be met,
choose no action. A merge target must be one of the fully supplied paths.

Do not return both a metadata patch and a body expand for the same document.

## 5. Deletion

Delete only with explicit evidence that the entire note is obsolete or
redundant. Age or topical similarity alone is insufficient. For delete,
target_path and proposed_content MUST both be empty.
