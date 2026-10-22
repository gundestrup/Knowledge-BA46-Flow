# Repository report

The repository report is a separate workspace from knowledge review. It uses
the existing Go server, SQLite database, and embedded Bulma/Alpine assets, with
its own dashboard at `/report`, API namespace, background jobs, and collector
package. It replaces the reporting work in `repo-report.rb`; it does not port
or invoke `kb-sync`.

## Configure sources

Set `report.registry_path` to the existing `repos.yml`. `report.policy_path`
is optional. When a registry is configured, remote checks default to enabled;
set `report.remote_enabled` to `false` to make normal refreshes local-only.
`report.comparison_path` is an optional, read-only Ruby report snapshot used
for side-by-side validation during cutover. Native Go collectors and the Go
SQLite snapshot are the primary report data.

```json
{
  "report": {
    "registry_path": "../engineering-memory/repos.yml",
    "policy_path": "../engineering-memory/policy.yml",
    "comparison_path": "../engineering-memory/report/data.json",
    "remote_enabled": true
  }
}
```

These JSON-config paths resolve relative to the JSON file. Repository paths in
`repos.yml` resolve relative to the registry file. Both YAML inputs are read
in place and never rewritten. Current schema examples are
`example-configs/repos.example.yml` and `example-configs/policy.example.yml`;
the policy example contains the fields KBM-flow currently understands.

## Migrated checks

- Local repository path, Git HEAD, dirty state, commits ahead of `repo_commit`,
  `.engineering-memory.yml` marker, Ruby version policy, scanner configuration,
  Semgrep licensing classification, and `Procfile.dev` runner checks.
- GitHub topics, open PR count/titles/links, latest Actions run, tag/release
  mismatches, code-scanning alerts, and Dependabot alerts.
- Forgejo open PRs, tag/release mismatches, and Actions task counts.
- CodeFactor grades for GitHub repositories.
- SonarCloud project gates, SonarCloud AutoScan keys for GitHub repos, and the
  configured local SonarQube fallback. Gate failures include metric details.
- Semgrep AppSec Platform projects and open SAST findings, grouped by severity
  and associated with registered repositories.
- Root and two-level nested `Gemfile.lock` inventory, dependency edges,
  root-level gemspec dependencies, lockfile checks, per-repo conflicts,
  watched gems, slot notes, added/removed gems, fleet drift/exceptions,
  outdated-version blockers (all detected constraints), and upstream
  staleness/dormancy exceptions.
- Node/npm project discovery across root and nested `package.json` files,
  `.nvmrc`/`.node-version` and `engines.node` policy checks, npm lockfile
  versions and dependency edges, package drift/exceptions, and outdated
  package versions from the npm registry. Exact Node pins are compared with
  current releases of the same major; floating major pins such as `.nvmrc`
  `22` are not treated as stale patch pins.
- Node dependency resolution currently reads `package-lock.json` and
  `npm-shrinkwrap.json`; the registered Node projects use npm lockfiles.
  Yarn and pnpm lockfile parsing is not included.
- Tag/release policy exceptions and the Ruby report's stale-value behavior:
  failed or skipped remote refreshes preserve last-known values rather than
  silently clearing findings. Unchanged repositories reuse GitHub/Forgejo
  results while volatile checks refresh; `--force` bypasses that optimization
  and refreshes the 24-hour RubyGems/npm caches and Node release index.

`repo_commit` drift is part of `repo-report.rb` and is implemented. `kb_commit`
status belongs to the separate `kb-sync status` workflow and remains out of
scope. KBM-flow does not create markers, update YAML, commit, or modify
registered repositories.

## Fleet findings and policy

The `/report` page renders two fleet sections — **Gem health** and **Node and
npm health** — summarizing version drift, outdated packages, and upstream
health across all registered repositories.

Findings carry a severity that answers "should I update?":

- `warn` / `info` / `dim` — behind latest; urgency ranks by how far the
  newest installed version lags (same-segment bump vs. major/minor).
- `constrained` — behind latest but a depender's lockfile requirement blocks
  the upgrade (e.g. `liquid` held by `jekyll ~> 4.0`). The finding lists the
  blocking edges (`held by jekyll ~> 4.0 (repo)`) so the _why_ is visible;
  it is a notice, not an actionable update.
- `ok` — accepted by policy (`drift_ok`, `dormant_ok`).

`policy.yml` documents the decisions the constraint analysis cannot infer:

| Key               | Meaning                                                                  |
| ----------------- | ------------------------------------------------------------------------ |
| `gems.slots`      | `prefer` the wanted gem in a shared slot; `ok` tolerates alternatives    |
| `gems.watch`      | flag the gem wherever it appears                                         |
| `gems.drift_ok`   | accept multi-version drift across the fleet, with a reason               |
| `gems.dormant_ok` | accept a quiet/dead upstream after a dated review                        |
| `gems.hold`       | accept being behind latest, with a reason — renders `Held`, not `Behind` |
| `npm.drift_ok`    | same as `gems.drift_ok`, for npm packages                                |
| `npm.watch`       | same as `gems.watch`, for npm packages                                   |
| `npm.hold`        | same as `gems.hold`, for npm packages                                    |

`hold` complements `constrained`: the blocker edges only capture constraints
recorded in a lockfile, while `hold` records a deliberate decision — e.g.
`liquid: "stay on 4.x — jekyll does not support liquid 5 yet"`. Held entries
keep their `blocked_by` context when edges exist and are excluded from the
outdated list.

`checks.min_coverage` (percentage, optional) flags repositories whose Codecov
coverage falls below the floor as `codecov coverage X% below Y%` findings.
Unset means coverage is displayed but never flagged; repositories without
Codecov data are never treated as below target.

## Version policy

`versions.*` compares repository version pins against policy expressions,
evaluated with range semantics (npm-style constraints including `||` and
`a - b` ranges):

```yaml
versions:
  ruby: "3.4.x" # family — fleet stays on 3.x
  node: ">=22" # floor — anything still supported
  python: "3.15.0" # exact pin
  go: "stable" # channel — resolved upstream
  php: ">=8.2"
  bundler: ">=4.0"
  rubygems: "stable"
  custom: # any other product, same semantics
    terraform: ">=1.6"
    elixir: "stable"
```

Built-in keys: `ruby`, `node`, `python`, `go`, `php`, `bundler`, `rubygems`.
`versions.custom` extends the same semantics to any other product — its key
is the product name (used as the endoflife.date slug for channels) and
matches `.tool-versions` plugin names plus `.<name>-version` files
(`.terraform-version`, `.elixir-version`, …). Keys colliding with a built-in
product are ignored with a note. TypeScript deliberately has no key: it runs
on the repo's Node version and its package version is npm dependency data,
already covered by the npm checks.

Expressions: an exact version (`"3.4.11"`), a family prefix (`"3.4"`,
`"3.4.x"`, `"3.x"`), or a range (`">=3.4 <4"`). Channels resolve against
upstream release data on remote runs:

| Channel     | Meaning                                          |
| ----------- | ------------------------------------------------ |
| `stable`    | newest released cycle                            |
| `lts`       | newest cycle whose LTS window opened (Node only) |
| `eol-floor` | oldest still-supported cycle                     |

Resolution uses endoflife.date for language toolchains (the built-in
slugs `ruby`/`nodejs`/`python`/`go`/`php`, and each `custom` key) and
rubygems.org for bundler/rubygems (which support `stable` only). The
resolved version is recorded in the snapshot's `policy_versions`
(`node lts → 24.22.0`), cached for 24 hours, and carried forward marked
stale on local-only runs — the check evaluates the carried value either
way. An unresolvable channel explains itself in `policy_versions` instead
of flagging every repository.

Detected pins: `.ruby-version`, `.nvmrc`/`.node-version`, any other
`.<name>-version` file (`.python-version`, `.go-version`, `.php-version`,
`.terraform-version`, …), every `.tool-versions` plugin line (`nodejs` and
`golang` alias to `node`/`go`; other plugin names match `custom` keys
directly), `go.mod` (`go` and `toolchain` directives), `Gemfile.lock`'s
`BUNDLED WITH`, `composer.json` (`require.php` as a constraint — checked
in the same direction as `engines.node` — plus `config.platform.php`),
and `rubygems:` keys in GitHub/Forgejo CI workflows (the
`ruby/setup-ruby` input). Repositories without a matching pin file are
not flagged unless the corresponding `require_*` check is set.

## Authentication and network behavior

Remote collection is read-only HTTP/API access. Credentials are not stored in
the JSON config or YAML registry:

- GitHub uses `GH_TOKEN`/`GITHUB_TOKEN`, falling back to `gh auth token` so a
  user's existing `gh auth login` session is reused.
- Forgejo uses `FORGEJO_TOKEN`, falling back to `git credential fill` and the
  configured Git credential helper/keychain. The default trusted host is
  `forgejo.borg-collective.eu`; set `FORGEJO_HOST` when using another trusted
  Forgejo host.
- Semgrep uses `SEMGREP_APP_TOKEN`, falling back to
  `~/.semgrep/settings.yml`.
- SonarCloud requests remain anonymous, matching the Ruby collector.
- CodeFactor and RubyGems use their public endpoints.
- Codecov coverage uses the public v2 API (`totals.coverage` for the
  repository's default branch) and needs no token for public repositories;
  set `CODECOV_TOKEN` when private repositories report coverage. A lookup
  failure keeps the last-known value flagged stale — a repository without
  Codecov shows "not reported", never zero coverage.
- npm package metadata uses the public npm registry; private/custom registries
  are not authenticated by this collector.
- Exact Node.js pins use the public `nodejs.org` release index.
- `versions.*` channels resolve via the public endoflife.date API
  (ruby/node/python/go cycles) and the RubyGems versions API
  (bundler/rubygems-update), cached 24 hours in the snapshot.

If a credential or endpoint is unavailable, the report keeps the last-known
value where one exists and exposes unavailable/stale data instead of reporting
a false clean result. A container does not inherit the host's `gh` login or
keychain; configure credentials explicitly if remote checks are enabled there.

## Run

```bash
KBM-flow report --config config.local.json
KBM-flow report --config config.local.json --no-remote
KBM-flow report --config config.local.json --repo AID --force
KBM-flow report --config config.local.json --json
```

`--no-remote` refreshes local checks and retains cached remote results;
`--repo NAME` refreshes one registered repo; `--force` bypasses unchanged-repo
and RubyGems/npm cache shortcuts; `--json` prints the complete snapshot. Start
`KBM-flow serve` and open `/report` for the dashboard. **Refresh report** uses
the configured remote preference; **Local checks only** avoids outbound API
calls; **Force full refresh** bypasses cache shortcuts. Each repository row
also has a refresh action for that registry entry. The repository table sorts
by repository, working-tree state, sync drift, check count, and finding count.
Three independent filters combine: **working tree** (all/dirty/clean),
**checks** (all/has/none — the same count the Checks column shows), and
**signals** — open PRs, Dependabot alerts, GitHub code-scanning alerts,
Semgrep issues, Sonar gate failures, CodeFactor grades below B−, Codecov
coverage below `checks.min_coverage` (80% when unset), any signal, or the
PR+Semgrep+Sonar conjunction. Each repository's detail view shows the Codecov
percentage next to the other remote checks.

The API returns the latest SQLite snapshot at `GET /api/report`. Start a
background run with `POST /api/report/run`; optional query parameters are
`no_remote=1`, `remote=1`, `force=1`, and `repo=NAME`. Report jobs are listed
and polled through `/api/report/jobs` and `/api/report/jobs/{id}`, separately
from knowledge scan/analyze/run jobs.

Each run also appends its summary (problems, dirty repos, open PRs, open
findings) to a durable history table capped at the 365 most recent runs. The
"Durable history" section on `/report` renders those points as a Chart.js
progress-over-time chart via `GET /api/report/history`, so trends appear
after the second run.

## Storage and safety

The latest Go snapshot, including per-repository remote results and RubyGems/
npm/Node release caches, is stored as one JSON document in SQLite. Report run history
uses durable `report` jobs, and each run's summary is appended to the
`repo_report_history` table (365 most recent runs kept) for the trend chart. `report.comparison_path` remains a read-only
reference and last-known fallback for remote/gem fields not yet present in the
Go snapshot, so the first Go run does not discard the Ruby report's saved
results. It is never rewritten and can be removed after cutover validation.

Git commands disable optional index locks. No remote API mutations are issued,
and repository files, Git metadata, markers, and YAML sources are not changed.
The report does write its own SQLite snapshot/job history. In Docker, the
report is disabled unless the registry, policy, and registered repositories
are mounted. The current distroless image has no Git, `gh`, or host keychain,
so local Git checks and credential-helper auth require a deliberate container
strategy.
