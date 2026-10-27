# Task

Judge whether the CURRENT document still reads as current knowledge. You
cannot verify live facts — flag claims that LOOK dated so a human knows where
to check: explicit version numbers, "the latest release", dated statements,
deprecated flags or tools, sunset services, dead documentation links.

When the document is fundamentally ABOUT a versioned technology — a tool,
library, framework or language named in its title, tags or opening lines that
is actively released — also emit at most ONE tracking flag, even without a
dated claim, so a verifier can check whether the latest release changes the
note's guidance:

{"claim": "<technology> latest stable release and notable changes",
 "concern": "implicit version tracking — the note does not pin a version; a
  newer release may change what it teaches"}

Skip the tracking flag when another flag already covers that technology, and
for timeless notes (concepts, patterns, opinions) that have no versioned
subject. At most one tracking flag per document.

Return exactly this JSON format:

{
"current": true,
"summary": "one sentence on the document's apparent currency",
"flags": [
{"claim": "the dated claim, quoted briefly", "concern": "why it may be stale"}
],
"suggestions": [
{
"action": "expand",
"target_path": "",
"reason": "the concrete correction and its evidence",
"confidence": 0.0,
"proposed_content": "complete markdown document with the correction applied"
}
]
}

Rules:

- current=false means "worth a human re-check", not "known wrong". Prefer
  flagging over asserting: you have no live data.
- flags must quote a real claim from the document — except the single
  tracking flag, whose claim names the technology, not a quotation. Style,
  taste or missing depth are not freshness problems.
- suggestions are optional and only ever `expand` — body edits stay additive:
  keep every existing heading, paragraph, link and code example in order and
  verbatim. To correct a stale claim, add a dated note ("As of 2026, ...") —
  do not rewrite history the author may have intended.
- `target_path` must always be the empty string: a freshness correction is
  proposed for the current document, never another file.
- Never add or change the `kbm`, `stub` or `kb_policy` frontmatter fields.
- An empty flags array and no suggestions is a successful result. Most healthy
  notes are current.
