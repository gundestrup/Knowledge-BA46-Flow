// Chart instances live in module scope, not on the reactive component — a
// reactive proxy around a Chart (which holds circular refs and a DOM canvas)
// is a known "Maximum call stack size exceeded" trigger.
// Chart.getChart(canvas) is Chart.js's own instance registry — used below
// instead of a hand-rolled map so update-vs-recreate follows the library's
// documented lifecycle. Theme-aware chart options come from common.js.

window.repoReportDashboard = () => ({
  snapshot: { configured: false, repos: [], summary: {} },
  jobs: [],
  reportHistory: [],
  busy: false,
  error: "",
  notice: "",
  filter: "",
  signalFilter: "all",
  treeFilter: "all",
  checksFilter: "all",
  sortKey: "name",
  sortDirection: "asc",
  expandedRepo: null,
  pollTimer: null,

  async init() {
    await Promise.all([this.loadI18n(), this.refresh()]);
  },

  ...window.kbm,

  async refresh() {
    try {
      this.snapshot = await this.requestJSON("/api/report");
      this.jobs = await this.requestJSON("/api/report/jobs");
      this.opsJobs = await this.requestJSON("/api/ops");
      this.reportHistory =
        (await this.requestJSON("/api/report/history")).points || [];
      const wasBusy = this.busy;
      this.busy = [...this.jobs, ...(this.opsJobs || [])].some(
        (job) => job.status === "running",
      );
      if (wasBusy && !this.busy) this.notice = "";
      this.schedulePoll();
      this.renderHistoryChart();
    } catch (error) {
      this.error = error.message;
    }
  },

  async startReport(options = {}) {
    if (typeof options === "boolean") options = { localOnly: options };
    this.error = "";
    this.notice = "";
    try {
      const query = new URLSearchParams();
      if (options.localOnly) query.set("no_remote", "1");
      if (options.force) {
        query.set("force", "1");
        if (!options.localOnly) query.set("remote", "1");
      }
      if (options.repo) query.set("repo", options.repo);
      const encodedQuery = query.toString();
      const suffix = encodedQuery ? `?${encodedQuery}` : "";
      const path = `/api/report/run${suffix}`;
      await this.requestJSON(path, { method: "POST" });
      this.busy = true;
      const scope = options.localOnly
        ? this.t("ui.report.scope_local", "Local")
        : this.t("ui.report.scope_full", "Full");
      const target = options.repo
        ? this.t("ui.report.for_repo", " for {{.Repo}}", { Repo: options.repo })
        : "";
      this.notice = this.t(
        "ui.report.report_started",
        "{{.Scope}} report started{{.Target}}.",
        {
          Scope: scope,
          Target: target,
        },
      );
      await this.refresh();
    } catch (error) {
      this.error = error.message;
    }
  },

  // Knowledge ops block report runs too (single job slot) — show both.
  runningJobs() {
    return [...(this.jobs || []), ...(this.opsJobs || [])].filter(
      (j) => j.status === "running",
    );
  },

  jobPercent(job) {
    const m = /(\d+)\/(\d+)/.exec(job?.detail || ""); // NOSONAR — linear match, no backtracking
    return m ? Math.round((+m[1] / +m[2]) * 100) : null;
  },

  // jobPhase parses the second N/M in a detail line — the inner phase
  // counter inside "12/27: remote 3/12: repo — sonar" — for the
  // current-operation bar. Returns null for single-counter details.
  jobPhase(job) {
    const m = (job?.detail || "").match(/\d+\/\d+/g); // NOSONAR — linear match, no backtracking
    if (!m || m.length < 2) return null;
    const [a, b] = m[1].split("/").map(Number);
    return b ? Math.round((a / b) * 100) : null;
  },

  schedulePoll() {
    clearTimeout(this.pollTimer);
    this.pollTimer = this.busy ? setTimeout(() => this.refresh(), 1500) : null;
  },

  statusText() {
    if (!this.snapshot.configured)
      return this.t(
        "ui.report.not_configured",
        "Report sources are not configured",
      );
    if (!this.snapshot.generated_at)
      return this.t("ui.report.no_snapshot", "No report snapshot yet");
    const kind = this.snapshot.remote_enabled
      ? this.t("ui.report.last_full", "Last full report")
      : this.t("ui.report.last_local", "Last local-only report");
    return `${kind}: ${this.formatTime(this.snapshot.generated_at)}`;
  },

  chartLabel(iso) {
    const parsed = new Date(iso);
    if (Number.isNaN(parsed.getTime())) return iso;
    return (
      parsed.toLocaleDateString(undefined, {
        month: "short",
        day: "numeric",
      }) +
      " " +
      parsed.toLocaleTimeString(undefined, {
        hour: "2-digit",
        minute: "2-digit",
      })
    );
  },

  // reportHistorySeries is pure (no Chart dependency) so the frontend test
  // can assert the series shape without a canvas implementation.
  reportHistorySeries() {
    const field = (name) =>
      this.reportHistory.map((p) =>
        p.summary && p.summary[name] != null ? p.summary[name] : null,
      );
    return {
      labels: this.reportHistory.map((p) => this.chartLabel(p.generated_at)),
      problems: field("problems"),
      dirty: field("dirty"),
      openPRs: field("open_prs"),
      findings: field("semgrep_open_findings"),
    };
  },

  renderHistoryChart() {
    if (typeof window === "undefined" || typeof window.Chart === "undefined") {
      return;
    }
    const canvas = document.getElementById("reportHistoryChart");
    if (!canvas) return;
    const series = this.reportHistorySeries();
    const existing = window.Chart.getChart(canvas);
    if (series.labels.length === 0) {
      existing?.destroy();
      return;
    }
    if (existing) {
      existing.data.labels = series.labels;
      existing.data.datasets[0].data = series.problems;
      existing.data.datasets[1].data = series.dirty;
      existing.data.datasets[2].data = series.openPRs;
      existing.data.datasets[3].data = series.findings;
      existing.update();
      return;
    }
    const dataset = (label, data, color) =>
      this.lineDataset(label, data, color, { tension: 0.25, spanGaps: true });
    new window.Chart(canvas.getContext("2d"), {
      type: "line",
      data: {
        labels: series.labels,
        datasets: [
          dataset("Problems", series.problems, "#a64035"),
          dataset("Dirty repos", series.dirty, "#d87536"),
          dataset("Open PRs", series.openPRs, "#68756e"),
          dataset("Open findings", series.findings, "#285444"),
        ],
      },
      options: this.lineChartOptions(),
    });
  },

  get filteredRepos() {
    const needle = this.filter.trim().toLowerCase();
    const repos = (this.snapshot.repos || []).filter((repo) => {
      const npmSearch = (repo.npm_projects || []).flatMap((project) => [
        project.name,
        project.path,
        ...Object.keys(project.direct || {}),
      ]);
      const matchesText =
        !needle ||
        [repo.name, repo.path, repo.url, ...(repo.problems || []), ...npmSearch]
          .join(" ")
          .toLowerCase()
          .includes(needle);
      return (
        matchesText &&
        this.matchesTreeFilter(repo) &&
        this.matchesChecksFilter(repo) &&
        this.matchesSignalFilter(repo)
      );
    });
    return repos.slice().sort((left, right) => {
      const a = this.repositorySortValue(left, this.sortKey);
      const b = this.repositorySortValue(right, this.sortKey);
      if (a == null) return b == null ? 0 : 1;
      if (b == null) return -1;
      const result =
        typeof a === "string"
          ? a.localeCompare(String(b), undefined, {
              numeric: true,
              sensitivity: "base",
            })
          : a - b;
      if (result === 0) {
        return left.name.localeCompare(right.name, undefined, {
          numeric: true,
          sensitivity: "base",
        });
      }
      return this.sortDirection === "asc" ? result : -result;
    });
  },

  hasOpenPullRequest(repo) {
    const count = repo.github?.open_prs ?? repo.forgejo?.open_prs;
    return count != null && count > 0;
  },

  hasSemgrepIssue(repo) {
    const findings = repo.semgrep_findings;
    return (
      (findings?.open != null && findings.open > 0) ||
      (repo.semgrep_license === "cloud-ci" && findings?.project == null)
    );
  },

  hasSonarIssue(repo) {
    return (
      (repo.sonarcloud != null && repo.sonarcloud !== "OK") ||
      (repo.sonarcloud_failures || []).length > 0
    );
  },

  matchesTreeFilter(repo) {
    switch (this.treeFilter) {
      case "dirty":
        return Boolean(repo.dirty);
      case "clean":
        return !repo.dirty;
      default:
        return true;
    }
  },

  // toggleExpand reveals the full-report row for one repo at a time; clicking
  // the open row's button collapses it again.
  toggleExpand(name) {
    this.expandedRepo = this.expandedRepo === name ? null : name;
  },

  // repoScanners unions the locally detected scanner configs with Dependabot
  // when the GitHub remote reports an alert count — Dependabot can be enabled
  // in repository settings without a dependabot.yml file.
  repoScanners(repo) {
    const scanners = [...(repo.scanners || [])];
    if (repo.github?.dependabot != null && !scanners.includes("dependabot"))
      scanners.push("dependabot");
    return scanners;
  },

  // checkCount mirrors the Checks column: detected scanners plus Ruby, Node,
  // toolchain, and npm findings discovered during local checks.
  checkCount(repo) {
    return (
      this.repoScanners(repo).length +
      (repo.ruby_issues || []).length +
      (repo.node_issues || []).length +
      (repo.toolchain_issues || []).length +
      (repo.npm_projects || []).length
    );
  },

  // policyPins renders the configured versions.* policy in the same slot the
  // Ruby policy line used: literal values display as written; channel values
  // display "policy → resolved" so a finding can be traced to the version
  // that produced it. Unresolved channels show their note instead.
  policyPins() {
    const versions = this.snapshot?.policy_versions || {};
    const pins = [];
    for (const product of Object.keys(versions).sort((left, right) =>
      left.localeCompare(right),
    )) {
      const pin = versions[product];
      if (!pin?.policy) continue;
      let value = pin.policy;
      if (pin.resolved && pin.resolved !== pin.policy)
        value = `${pin.policy} → ${pin.resolved}`;
      if (pin.stale) value += " (stale)";
      if (pin.note) value += ` — ${pin.note}`;
      pins.push({ key: product, text: `${product} policy ${value}` });
    }
    return pins;
  },

  matchesChecksFilter(repo) {
    switch (this.checksFilter) {
      case "has":
        return this.checkCount(repo) > 0;
      case "none":
        return this.checkCount(repo) === 0;
      default:
        return true;
    }
  },

  hasDependabotAlert(repo) {
    return (repo.github?.dependabot ?? 0) > 0;
  },

  hasCodeScanningAlert(repo) {
    return (repo.github?.code_scanning ?? 0) > 0;
  },

  // codeFactorAlert mirrors codeFactorNeedsAttention in internal/report:
  // grades ranked below B- are findings.
  hasCodeFactorAlert(repo) {
    const order = [
      "A+",
      "A",
      "A-",
      "B+",
      "B",
      "B-",
      "C+",
      "C",
      "C-",
      "D+",
      "D",
      "D-",
      "E",
      "F",
    ];
    const rank = order.indexOf(repo.codefactor);
    return rank >= 0 && rank > order.indexOf("B-");
  },

  // coverageFloor is the Codecov percentage below which a repository counts
  // as a coverage finding: the policy's checks.min_coverage when configured,
  // otherwise a conventional 80%.
  coverageFloor() {
    const floor = this.snapshot.policy_min_coverage;
    return floor != null ? floor : 80;
  },

  hasLowCoverage(repo) {
    return repo.codecov != null && repo.codecov < this.coverageFloor();
  },

  codecovLabel(repo) {
    if (repo.codecov == null)
      return this.t("ui.report.not_reported", "not reported");
    const stale = repo.codecov_stale
      ? this.t("ui.report.stale_suffix", " (stale)")
      : "";
    return `${Number(repo.codecov).toFixed(1)}%${stale}`;
  },

  // scannerIcon maps the scanner names checks.go detects to the icon-*
  // registry classes in base.css. Names without a brand mark render as
  // text only.
  scannerIcon(name) {
    const icons = {
      semgrep: "icon-semgrep",
      sonar: "icon-sonarqubecloud",
      forgejo: "icon-forgejo",
      "gh-actions": "icon-githubactions",
      lefthook: "icon-lefthook",
      dependabot: "icon-dependabot",
    };
    return icons[name];
  },

  // scannerLabel folds the detected Semgrep license tier into the chip so the
  // standalone "Semgrep: <license>" line does not duplicate the display.
  scannerLabel(repo, name) {
    if (name !== "semgrep") return name;
    const tiers = { ce: "CE", registry: "registry", "cloud-ci": "cloud CI" };
    const tier = tiers[repo.semgrep_license] || repo.semgrep_license;
    return tier ? `${name} ${tier}` : name;
  },

  // scannerUrl links a chip to the project-specific check page when one was
  // collected (Sonar dashboard, Semgrep findings, CI runs), falling back to
  // the tool's site otherwise.
  scannerUrl(repo, name) {
    if (name === "sonar" && repo.sonarcloud_url) return repo.sonarcloud_url;
    if (name === "semgrep" && repo.semgrep_findings?.url)
      return repo.semgrep_findings.url;
    if (
      ((name === "gh-actions" && repo.github) ||
        (name === "forgejo" && repo.forgejo)) &&
      repo.web_url
    )
      return `${repo.web_url}/actions`;
    if (name === "dependabot" && repo.web_url)
      return (repo.github?.dependabot ?? 0) > 0
        ? `${repo.web_url}/pulls`
        : `${repo.web_url}/security`;
    const urls = {
      semgrep: "https://semgrep.dev",
      sonar: "https://www.sonarsource.com/products/sonarqube/",
      forgejo: "https://forgejo.org",
      "gh-actions": "https://github.com/features/actions",
      lefthook: "https://lefthook.dev",
      overcommit: "https://github.com/sds/overcommit",
      dependabot: "https://github.com/dependabot",
    };
    return urls[name];
  },

  scannerTitle(repo, name) {
    if (name === "semgrep" && repo.semgrep_findings?.error)
      return `Semgrep findings unavailable — ${repo.semgrep_findings.error}`;
    const names = {
      semgrep: "Semgrep static analysis",
      sonar: "SonarQube analysis",
      forgejo: "Forgejo Actions CI",
      "gh-actions": "GitHub Actions CI",
      lefthook: "Lefthook Git hooks",
      overcommit: "Overcommit Git hooks",
      dependabot: "Dependabot security alerts",
    };
    const url = this.scannerUrl(repo, name);
    const label = names[name] || name;
    return url ? `${label} — ${url}` : label;
  },

  // githubSlug extracts "owner/repo" from a github.com remote URL for the
  // per-service Codecov and CodeFactor dashboard links.
  githubSlug(repo) {
    const match = /github\.com[/:]([^\s:]+?)(\.git)?\/?$/.exec(repo.url || "");
    return match ? match[1] : "";
  },

  codecovUrl(repo) {
    const slug = this.githubSlug(repo);
    return slug ? `https://app.codecov.io/gh/${slug}` : "";
  },

  codefactorUrl(repo) {
    const slug = this.githubSlug(repo);
    return slug ? `https://www.codefactor.io/repository/github/${slug}` : "";
  },

  // sortIcon names the Lucide arrow rendered next to the active sort column.
  sortIcon(key) {
    if (this.sortKey !== key) return "icon-chevrons-up-down";
    return this.sortDirection === "asc" ? "icon-arrow-up" : "icon-arrow-down";
  },

  matchesSignalFilter(repo) {
    const hasPR = this.hasOpenPullRequest(repo);
    const hasSemgrep = this.hasSemgrepIssue(repo);
    const hasSonar = this.hasSonarIssue(repo);
    switch (this.signalFilter) {
      case "pull-requests":
        return hasPR;
      case "dependabot":
        return this.hasDependabotAlert(repo);
      case "code-scanning":
        return this.hasCodeScanningAlert(repo);
      case "semgrep":
        return hasSemgrep;
      case "sonar":
        return hasSonar;
      case "codefactor":
        return this.hasCodeFactorAlert(repo);
      case "coverage":
        return this.hasLowCoverage(repo);
      case "any":
        return (
          hasPR ||
          hasSemgrep ||
          hasSonar ||
          this.hasDependabotAlert(repo) ||
          this.hasCodeScanningAlert(repo) ||
          this.hasCodeFactorAlert(repo) ||
          this.hasLowCoverage(repo)
        );
      case "all-signals":
        return hasPR && hasSemgrep && hasSonar;
      default:
        return true;
    }
  },

  repositorySortValue(repo, key) {
    switch (key) {
      case "working-tree":
        return repo.dirty ? 1 : 0;
      case "sync":
        return repo.sync?.drift;
      case "checks":
        return this.checkCount(repo);
      case "findings":
        return (repo.problems || []).length;
      default:
        return repo.name || "";
    }
  },

  sortBy(key) {
    if (this.sortKey === key) {
      this.sortDirection = this.sortDirection === "asc" ? "desc" : "asc";
      return;
    }
    this.sortKey = key;
    this.sortDirection = "asc";
  },

  sortAria(key) {
    if (this.sortKey !== key) return "none";
    return this.sortDirection === "asc" ? "ascending" : "descending";
  },

  sortIndicator(key) {
    if (this.sortKey !== key) return "↕";
    return this.sortDirection === "asc" ? "↑" : "↓";
  },

  // `snapshot` is passed in from the template: reads through `this`
  // inside x-for-scoped method calls are not tracked by the CSP evaluator.
  repoDetails(repo, snapshot) {
    const exception =
      snapshot.policy_exceptions?.tag_release?.[repo.name] || {};
    const exceptionList = (values, excluded) => {
      if (values === null || values === undefined) return "n/a";
      if (values.length === 0) return "none";
      return values
        .map((value) =>
          excluded.includes("*") || excluded.includes(value)
            ? `${value} (documented exception)`
            : `${value} (unexcepted)`,
        )
        .join(", ");
    };
    const lines = [
      `URL: ${repo.url || "—"}`,
      `HEAD: ${repo.head_sha || "unavailable"}`,
      `Working tree: ${repo.dirty ? "dirty" : "clean"}`,
      `Sync marker: ${repo.sync?.marker || "unknown"}`,
      `Commits ahead of baseline: ${repo.sync?.drift ?? "unknown"}`,
      `Scanners: ${this.repoScanners(repo).join(", ") || "none detected"}`,
    ];
    for (const issue of repo.node_issues || [])
      lines.push(`Node issue: ${issue}`);
    for (const pin of repo.toolchain_pins || [])
      lines.push(`Toolchain pin: ${pin.product} ${pin.version} (${pin.file})`);
    for (const issue of repo.toolchain_issues || [])
      lines.push(`Toolchain issue: ${issue}`);
    for (const project of repo.npm_projects || []) {
      lines.push(
        "",
        `Node project: ${project.name || project.path} (${project.path || "."})`,
        `  package.json: ${project.version || "private/unspecified version"}`,
        `  lockfile: ${project.lockfile || "missing"}${project.lockfile_version ? ` v${project.lockfile_version}` : ""}`,
        `  Node pin: ${project.node_version || "not pinned"}${project.node_version_file ? ` (${project.node_version_file})` : ""}`,
        `  engines.node: ${project.node_engine || "not declared"}`,
        `  direct packages: ${Object.keys(project.direct || {}).length}`,
      );
      for (const [name, versions] of Object.entries(
        project.package_versions || {},
      ).sort(([left], [right]) => left.localeCompare(right))) {
        lines.push(`  ${name}: ${versions.join(" / ")}`);
      }
      for (const versions of project.multiple_versions || []) {
        lines.push(`  multiple resolved versions: ${versions}`);
      }
      for (const [name, reason] of Object.entries(project.watched || {}).sort(
        ([left], [right]) => left.localeCompare(right),
      )) {
        lines.push(`  watched npm package: ${name} — ${reason}`);
      }
    }
    const github = repo.github;
    const forgejo = repo.forgejo;
    const remote = github || forgejo;
    if (github) {
      lines.push("", "GitHub");
      if (github.auth_unavailable) {
        lines.push("GitHub auth unavailable; use gh auth login or GH_TOKEN");
      }
      lines.push(`Open PRs: ${github.open_prs ?? "n/a"}`);
      if (github.ci) lines.push(`Latest CI: ${github.ci}`);
      lines.push(
        `Topics: ${github.topics === null ? "n/a" : (github.topics || []).join(", ") || "none"}`,
      );
      lines.push(`Code-scanning alerts: ${github.code_scanning ?? "n/a"}`);
      lines.push(`Dependabot alerts: ${github.dependabot ?? "n/a"}`);
      lines.push(
        `Tags without release: ${exceptionList(github.tags_without_release, exception.tags_without_release || [])}`,
      );
      lines.push(
        `Releases without tag: ${exceptionList(github.releases_without_tag, exception.releases_without_tag || [])}`,
      );
      for (const pull of github.prs || []) {
        lines.push(`PR #${pull.n}: ${pull.t}${pull.u ? ` — ${pull.u}` : ""}`);
      }
    } else if (forgejo) {
      lines.push("", "Forgejo");
      if (forgejo.auth_unavailable) {
        lines.push(
          "Forgejo auth unavailable; check FORGEJO_TOKEN or Git credentials",
        );
      }
      lines.push(`Open PRs: ${forgejo.open_prs ?? "n/a"}`);
      lines.push(`Action runs: ${forgejo.action_runs ?? "n/a"}`);
      lines.push(
        `Tags without release: ${exceptionList(forgejo.tags_without_release, exception.tags_without_release || [])}`,
      );
      for (const pull of forgejo.prs || []) {
        lines.push(`PR #${pull.n}: ${pull.t}${pull.u ? ` — ${pull.u}` : ""}`);
      }
    } else {
      lines.push("", "Remote host checks: unavailable");
    }
    if (remote?.stale_fields?.length) {
      lines.push(`Carried forward as stale: ${remote.stale_fields.join(", ")}`);
    }
    lines.push(
      `CodeFactor: ${repo.codefactor || "n/a"}${repo.codefactor_stale ? " (stale)" : ""}`,
    );
    lines.push(`Codecov: ${this.codecovLabel(repo)}`);
    if (repo.sonarcloud) {
      lines.push(
        `Sonar gate: ${repo.sonarcloud} (${repo.sonarcloud_kind || "unknown"})${repo.sonar_stale ? " (stale)" : ""}`,
      );
      for (const failure of repo.sonarcloud_failures || []) {
        lines.push(`  - ${failure}`);
      }
      if (repo.sonarcloud_url)
        lines.push(`Sonar project: ${repo.sonarcloud_url}`);
    } else {
      lines.push("Sonar gate: not reported");
    }
    if (repo.semgrep_findings) {
      const findings = repo.semgrep_findings;
      if (findings.error) {
        lines.push(`Semgrep findings unavailable: ${findings.error}`);
      } else {
        lines.push(
          `Semgrep: ${findings.project || "project unavailable"}; ${findings.open ?? "n/a"} open`,
        );
        if (findings.severities) {
          lines.push(
            `  Severity: ${Object.entries(findings.severities)
              .map(([severity, count]) => `${severity}:${count}`)
              .join(" ")}`,
          );
        }
        if (findings.url) lines.push(`  Findings: ${findings.url}`);
        if (findings.stale)
          lines.push("  Values carried forward from the previous report");
      }
    } else {
      lines.push("Semgrep findings: not collected");
    }
    const gems = Object.entries(repo.gems || {}).sort(([left], [right]) =>
      left.localeCompare(right),
    );
    lines.push("", `Locked gems (${gems.length}):`);
    for (const [name, version] of gems) lines.push(`  ${name} ${version}`);
    for (const conflict of repo.gem_conflicts || []) {
      lines.push(`Gem conflict: ${conflict}`);
    }
    if (repo.lockfile_missing)
      lines.push("Gemfile without a committed lockfile");
    for (const [name, reason] of Object.entries(repo.gems_watch || {})) {
      lines.push(`Watched gem: ${name} — ${reason}`);
    }
    for (const [name, dependency] of Object.entries(repo.gemspec_deps || {})) {
      lines.push(
        `Gemspec ${dependency.dev ? "development" : "runtime"} dependency: ${name} ${dependency.req}`,
      );
    }
    for (const note of repo.gem_notes || []) lines.push(`Gem note: ${note}`);
    if (repo.gem_changes) {
      lines.push(
        `Added gems: ${(repo.gem_changes.added || []).join(", ") || "none"}`,
      );
      lines.push(
        `Removed gems: ${(repo.gem_changes.removed || []).join(", ") || "none"}`,
      );
    }
    return lines.join("\n");
  },

  repoLinks(repo) {
    const links = [];
    const remote = repo.github || repo.forgejo;
    for (const pull of remote?.prs || []) {
      if (pull.u) {
        links.push({
          url: pull.u,
          label: `PR #${pull.n} ${pull.t}`,
        });
      }
    }
    if (repo.sonarcloud_url) {
      links.push({ url: repo.sonarcloud_url, label: "Sonar project" });
    }
    if (repo.semgrep_findings?.url) {
      links.push({ url: repo.semgrep_findings.url, label: "Semgrep findings" });
    }
    return links;
  },

  versionList(versions) {
    return Object.entries(versions || {})
      .map(([version, repos]) => `${version} (${repos.join("/")})`)
      .join(" vs ");
  },

  nodeFleetDetails() {
    const fleet = this.snapshot.fleet || {};
    const lines = [];
    const versionList = this.versionList;
    const npm = fleet.npm || {};
    if (this.snapshot.summary?.npm_packages) {
      const checked = this.snapshot.summary.npm_checked_packages || 0;
      lines.push(
        `npm latest versions checked: ${checked}/${this.snapshot.summary.npm_packages}`,
      );
      if (checked === 0) {
        lines.push("npm registry latest versions have not been collected.");
      }
      if (checked > 0 && checked < this.snapshot.summary.npm_packages) {
        lines.push(
          "Some npm latest versions are unavailable or not yet cached.",
        );
      }
    }
    if (this.snapshot.node_releases?.stale) {
      lines.push(
        "Node.js release cache is stale; latest version lookup failed or was skipped.",
      );
    }
    const nodeVersions = Object.entries(npm.node_version_drift || {}).sort(
      ([left], [right]) =>
        left.localeCompare(right, undefined, { numeric: true }),
    );
    const staleNpmLatest = Object.entries(
      this.snapshot.npm_latest || {},
    ).filter(([, entry]) => entry.stale);
    if (staleNpmLatest.length) {
      const names = staleNpmLatest.map(([name]) => name);
      const shown = names.slice(0, 12).join(", ");
      const rest = names.length > 12 ? ` … +${names.length - 12} more` : "";
      lines.push(
        `npm latest cache stale for ${names.length} packages: ${shown}${rest}`,
      );
    }
    if (nodeVersions.length) {
      lines.push(
        "Node runtime pins:",
        ...nodeVersions.map(
          ([version, projects]) => `  ${version}: ${projects.join("/")}`,
        ),
      );
    }
    for (const [project, item] of Object.entries(npm.node_outdated || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    )) {
      const lts = item.lts ? `; ${item.lts} LTS` : "";
      lines.push(
        `Node ${project}: ${item.pinned} → ${item.latest} [${item.severity}]${lts}`,
      );
    }
    for (const [name, drift] of Object.entries(npm.package_drift || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    )) {
      lines.push(
        `Drift ${name}: ${versionList(drift.versions)} [${drift.severity}]`,
      );
    }
    for (const [name, drift] of Object.entries(npm.package_drift_ok || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    )) {
      lines.push(
        `Allowed drift ${name}: ${versionList(drift.versions)} — ${drift.reason}`,
      );
    }
    const { actionable } = this.outdatedSplit(npm.package_outdated);
    for (const [name, item] of actionable) {
      const blockers = item.blocked_by?.length
        ? `; held by ${item.blocked_by.join("; ")}`
        : "";
      lines.push(
        `Behind ${name}: ${versionList(item.versions)} → ${item.latest} [${item.severity}]${blockers}`,
      );
    }
    for (const [name, item] of Object.entries(npm.package_held || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    )) {
      const blockers = item.blocked_by?.length
        ? `; constrained by ${item.blocked_by.join("; ")}`
        : "";
      lines.push(
        `Held ${name}: ${versionList(item.versions)} → ${item.latest} — ${item.reason}${blockers}`,
      );
    }
    return lines.length
      ? lines.join("\n")
      : "No Node.js or npm fleet findings.";
  },

  nodeFleetConstrained() {
    const npm = this.snapshot.fleet?.npm || {};
    const { constrained } = this.outdatedSplit(npm.package_outdated);
    return constrained
      .map(
        ([name, item]) =>
          `${name}: ${this.versionList(item.versions)} → ${item.latest}; held by ${(item.blocked_by || []).join("; ")}`,
      )
      .join("\n");
  },

  nodeFleetConstrainedSummary() {
    const npm = this.snapshot.fleet?.npm || {};
    const { constrained } = this.outdatedSplit(npm.package_outdated);
    if (!constrained.length) return "";
    return `${constrained.length} package${constrained.length === 1 ? "" : "s"} held by dependency constraints — transitive pins, not directly actionable`;
  },

  gemFleetDetails() {
    const fleet = this.snapshot.fleet || {};
    const lines = [];
    const versionList = this.versionList;
    const gemLatest = this.snapshot.gem_latest || {};
    const checked = Object.values(gemLatest).filter(
      (entry) => entry.checked_at && entry.v,
    ).length;
    lines.push(
      `gem latest versions checked: ${checked}/${Object.keys(gemLatest).length}`,
    );
    for (const [name, drift] of Object.entries(fleet.gem_drift || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    )) {
      lines.push(
        `Drift ${name}: ${versionList(drift.versions)} [${drift.severity}]`,
      );
    }
    for (const [name, drift] of Object.entries(fleet.gem_drift_ok || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    )) {
      lines.push(
        `Allowed drift ${name}: ${versionList(drift.versions)} — ${drift.reason}`,
      );
    }
    const { actionable } = this.outdatedSplit(fleet.gem_outdated);
    for (const [name, item] of actionable) {
      const blockers = item.blocked_by?.length
        ? `; held by ${item.blocked_by.join("; ")}`
        : "";
      lines.push(
        `Behind ${name}: ${versionList(item.versions)} → ${item.latest} [${item.severity}]${blockers}`,
      );
    }
    for (const [name, item] of Object.entries(fleet.gem_held || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    )) {
      const blockers = item.blocked_by?.length
        ? `; constrained by ${item.blocked_by.join("; ")}`
        : "";
      lines.push(
        `Held ${name}: ${versionList(item.versions)} → ${item.latest} — ${item.reason}${blockers}`,
      );
    }
    for (const [name, item] of Object.entries(fleet.gem_stale || {}).sort(
      ([left], [right]) => left.localeCompare(right),
    )) {
      const note = item.note ? ` — ${item.note}` : "";
      lines.push(
        `Upstream ${name}: ${item.age_months} months since ${item.latest} [${item.severity}]${note}`,
      );
    }
    return lines.length ? lines.join("\n") : "No Ruby gem fleet findings.";
  },

  gemFleetConstrained() {
    const fleet = this.snapshot.fleet || {};
    const { constrained } = this.outdatedSplit(fleet.gem_outdated);
    return constrained
      .map(
        ([name, item]) =>
          `${name}: ${this.versionList(item.versions)} → ${item.latest}; held by ${(item.blocked_by || []).join("; ")}`,
      )
      .join("\n");
  },

  gemFleetConstrainedSummary() {
    const fleet = this.snapshot.fleet || {};
    const { constrained } = this.outdatedSplit(fleet.gem_outdated);
    if (!constrained.length) return "";
    return `${constrained.length} gem${constrained.length === 1 ? "" : "s"} held by dependency constraints — held pins, not directly actionable`;
  },

  // outdatedSplit separates actionable "behind" entries from constraint-held
  // ones — transitive depender pins are noise unless deliberately grouped.
  outdatedSplit(map) {
    const actionable = [];
    const constrained = [];
    for (const entry of Object.entries(map || {}).sort(([left], [right]) =>
      left.localeCompare(right),
    )) {
      (entry[1].severity === "constrained" ? constrained : actionable).push(
        entry,
      );
    }
    return { actionable, constrained };
  },

  repoCountText() {
    return this.t("ui.report.repo_count", "{{.Shown}} / {{.Total}} repos", {
      Shown: this.filteredRepos.length,
      Total: (this.snapshot.repos || []).length,
    });
  },

  driftText(repo) {
    return repo.sync.drift === null
      ? this.t("ui.report.no_baseline", "no baseline")
      : this.t("ui.report.drift_commits", "+{{.Count}} commits", {
          Count: repo.sync.drift,
        });
  },

  npmProjectText(repo) {
    return this.t("ui.report.npm_projects", "{{.Count}} Node/npm project(s)", {
      Count: (repo.npm_projects || []).length,
    });
  },
});

if (typeof document !== "undefined") {
  document.addEventListener("alpine:init", () => {
    Alpine.data("repoReportDashboard", window.repoReportDashboard);
  });
}
