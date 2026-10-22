# Local real-document LLM evaluation

This directory stores only an allowlist of source paths, SHA-256 pins, and a
review rubric. It does not vendor Engineering Memory content into this
application repository.

`make example-ollama TIER=llm-eval` copies the pinned Markdown files and
`taxonomy.yaml` sidecar from `../engineering-memory` (override with
`REAL_KB_ROOT`) into a fresh, gitignored `data/example-runs/` corpus.
Preparation verifies each source hash, validates the pinned tag vocabulary,
rejects malformed or `kb_policy.no_llm_context` notes and obvious credential
patterns, preserves the shelf paths, and installs a deny-write policy.
Each run has its own database, configuration, source hashes, expected rubric,
and log. The runner uses `run` only; it never accepts or applies suggestions.

If a source document changes, the pinned hash check stops before Ollama is
called. Review the source, update the expected rubric and hash deliberately,
then rerun. This is a local model-quality check, not part of `go test ./...`;
the `basic`, `intermediate`, and `e2e` shelves remain the fast, deterministic
fake-model tests.
