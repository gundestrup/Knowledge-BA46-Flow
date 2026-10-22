# Example knowledge and test shelves

The files at this directory's root are a small starter shelf. The isolated
`basic/`, `intermediate/`, and `e2e/` directories are hand-picked test shelves;
they use the same topic folders as `engineering-memory` where relevant:
`frameworks/`, `languages/`, `lessons-learned/`, `patterns/`, and `tools/`.

| Shelf           | Scanned Markdown | Main cases                                                                                                                                                                  |
| --------------- | ---------------: | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `basic/`        |                2 | A missing relationship, an unmentioned sibling script, missing frontmatter, and scanner exclusions (built-in rules, `exclude_directories`, `exclude`, `exclude_extensions`) |
| `intermediate/` |                7 | The basic cases plus a two-year-old note, invalid metadata, an unresolved link, private content, and model-stage failures                                                   |
| `e2e/`          |               12 | The intermediate cases plus duplicate content, time-stamped contradictory claims, and a denied apply attempt                                                                |

Each shelf's `expected.json` is its golden standard: exact scan boundaries,
required safe proposals, required rejections, privacy expectations, and any
known duplicate or contradiction pairs. Live-model output is not compared as
exact prose; the same expectations are used as a review checklist.

Every test shelf contains `.kb-policy.json` with mutations denied. The normal
test suite copies each shelf to a temporary directory, runs the real scanner
and analyzer with deterministic fake models, and never contacts Ollama. The
optional live test uses `run` only, writes its database and log under ignored
`data/example-runs/`, and never accepts or applies suggestions.

The starter example configs explicitly exclude these test shelves and the
`llm-eval/` manifest directory, so they remain separate cases rather than being
mixed into the starter corpus.

`llm-eval/` contains only a path/hash allowlist and a human review rubric. Its
real Markdown snapshots are materialized under ignored `data/` only for an
explicit live Ollama run; the real notes are not copied into this tracked
example tree.

Starter notes:

- [Bulma](frameworks/bulma/README.md)
- [Ollama](tools/ollama/README.md)
- [Docker host access](lessons-learned/docker-host-access.md)
