# Installation

## Local installation

Requirements:

- Go 1.26 or newer
- Ollama, running outside the container
- A local Ollama model, e.g. the model set in `config.local.json`

```bash
cd Knowledge-BA46-Flow
cp config.json config.local.json
```

Edit at least these fields in `config.local.json`:

```json
{
  "repository_path": "/absolute/path/to/knowledge",
  "ollama_url": "http://localhost:11434",
  "ollama_model": "your-installed-model"
}
```

Relative paths in the config file are resolved relative to the config file's location, so `KBM-flow` can be run from any directory.

When sharing a corpus with the Knowledge Base MCP Server, set
`repository_path` to one shelf directory inside `KNOWLEDGE_BASES_ROOT_DIR`
(for example, `/home/user/knowledge_bases/engineering`), not to the parent
directory containing several shelves. This keeps scanning, the shelf's
`.kb-policy.json`, and MCP indexing aligned.

### Choosing a model

`ollama_model` accepts models shown by `ollama list`. For an Apple M2 with
24 GB, try `gemma3:12b` Q4_K_M for analysis and keep `phi4-mini` for the bounded
enrichment stage; `qwen2.5:7b` Q4_K_M is a lower-memory analysis candidate to
benchmark separately. `qwen3:14b` remains the shipped default, not a proven
best choice for every machine. See `docs/MODELS.md` for the stopped-run findings,
model-selection caveats and small-pilot checklist.

The optional `example-configs/config.m2-24gb.json` profile selects the three
roles and a separate example database. Models must already be installed;
the application never downloads them automatically. The pipeline unloads
completed embedding/enrichment stages before starting the next model.

`ollama_num_ctx` defaults to `0` — each request auto-sizes its context window
from the prompt, capped at the model's `context_length` (`ollama show
<model>`). Set a positive value only to pin the window. The prompt files in
`prompts/` can be tuned per model without recompiling.

Build and test:

```bash
go mod tidy
go test ./...
make build
```

`VERSION` is the application's version source; `make build` embeds it. Direct
`go run` commands report `dev`. Use `KBM-flow --version` to inspect a built
binary.

Run:

```bash
./KBM-flow run --config config.local.json
./KBM-flow list --config config.local.json
./KBM-flow review --config config.local.json
./KBM-flow apply --config config.local.json
```

## Docker

`docker-compose.yml` runs the complete stack: this program plus the
knowledge-base-mcp-server (built from the `v0.2.1` tag; point
`build.context` at a local checkout for development). Both containers mount
`./data/knowledge` (your markdown shelves) and their own state directories,
so the corpus, FAISS indexes and SQLite state live outside the images.
Both run as `user: "1000:1000"` — set that to your host uid:gid if it
differs, or the shared volume permissions will not line up.

```bash
cp .env.example .env   # set KB_SHELF and MCP_AUTH_TOKEN
docker compose build
docker compose up -d
```

One `.env` feeds both services: `OLLAMA_BASE_URL` reaches the MCP server
natively and KBM-flow through a `config.Load` environment override (as do
`KBM_REPOSITORY_PATH`, `KBM_DATABASE_PATH`, `KBM_HTTP_ADDR`,
`KBM_OLLAMA_MODEL`, `KBM_OLLAMA_MODEL_FAST`). Deployment-specific values
live in `.env`; stable settings live in `config.docker.json`.

After `up -d`:

- Dashboard at `http://127.0.0.1:8080` (loopback only — `/api/apply` mutates
  the filesystem).
- MCP server at `http://127.0.0.1:8081` (`MCP_TRANSPORT=http`, Bearer
  `MCP_AUTH_TOKEN`).
- `docker compose run --rm kbm-flow run` for a batch scan+analyze.

`config.docker.json` uses `http://host.docker.internal:11434`. Ollama must
be able to receive connections from the container. Keep the exposure local
and firewall-protected.

### Automatic reindex after apply

`config.docker.json` sets `reindex_trigger_path` to `.reindex-trigger` and
compose points the MCP server's `REINDEX_TRIGGER_PATH` at the same file
inside the shelf. After each successful apply batch, KBM-flow truncates the
file; the server's mtime watcher picks that up and reindexes — no manual
`kb reindex` needed. The file is a dotfile inside the shelf: both scanners
skip dot-prefixed entries, and the path stays inside KBM-flow's `os.Root`
jail.
