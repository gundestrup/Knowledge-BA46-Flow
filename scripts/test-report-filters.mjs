import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const dashboardPath = new URL(
  "../internal/server/static/report.js",
  import.meta.url,
);
const context = { window: {} };
vm.runInNewContext(readFileSync(dashboardPath, "utf8"), context);

const dashboard = context.window.repoReportDashboard();
dashboard.snapshot = {
  repos: [
    {
      name: "alpha",
      path: "/alpha",
      dirty: false,
      sync: { drift: 4 },
      scanners: ["sonar"],
      problems: [],
      github: { open_prs: 1, dependabot: 2 },
      semgrep_findings: { open: 0 },
      sonarcloud: "OK",
      codecov: 97.2,
      npm_projects: [{ name: "alpha-tools", direct: { eslint: "^9.0.0" } }],
    },
    {
      name: "beta",
      path: "/beta",
      dirty: true,
      sync: { drift: 1 },
      scanners: ["semgrep"],
      problems: ["semgrep findings"],
      github: { code_scanning: 5 },
      codefactor: "C+",
      semgrep_findings: { open: 2 },
      sonarcloud: "ERROR",
    },
    {
      name: "delta",
      path: "/delta",
      dirty: false,
      sync: { drift: 2 },
      scanners: ["sonar", "semgrep"],
      problems: ["multiple findings"],
      github: { open_prs: 2 },
      semgrep_findings: { open: 1 },
      sonarcloud: "ERROR",
      codecov: 55.0,
      codecov_stale: true,
    },
    {
      name: "gamma",
      path: "/gamma",
      dirty: false,
      sync: { drift: null },
      scanners: [],
      problems: [],
      forgejo: { open_prs: 0 },
      semgrep_findings: { open: 0 },
      sonarcloud: "OK",
    },
  ],
};

const names = () => dashboard.filteredRepos.map((repo) => repo.name);
assert.deepEqual(names(), ["alpha", "beta", "delta", "gamma"]);

dashboard.sortBy("sync");
assert.deepEqual(names(), ["beta", "delta", "alpha", "gamma"]);
dashboard.sortBy("sync");
assert.deepEqual(names(), ["alpha", "delta", "beta", "gamma"]);

dashboard.sortBy("name");
dashboard.signalFilter = "pull-requests";
assert.deepEqual(names(), ["alpha", "delta"]);
dashboard.signalFilter = "semgrep";
assert.deepEqual(names(), ["beta", "delta"]);
dashboard.signalFilter = "sonar";
assert.deepEqual(names(), ["beta", "delta"]);
dashboard.signalFilter = "any";
assert.deepEqual(names(), ["alpha", "beta", "delta"]);
dashboard.signalFilter = "all-signals";
assert.deepEqual(names(), ["delta"]);

dashboard.signalFilter = "dependabot";
assert.deepEqual(names(), ["alpha"]);
dashboard.signalFilter = "code-scanning";
assert.deepEqual(names(), ["beta"]);
dashboard.signalFilter = "codefactor";
assert.deepEqual(names(), ["beta"]);
dashboard.signalFilter = "coverage";
assert.deepEqual(names(), ["delta"]);
// A configured policy floor overrides the 80% default.
dashboard.snapshot.policy_min_coverage = 98;
assert.deepEqual(names(), ["alpha", "delta"]);
delete dashboard.snapshot.policy_min_coverage;

dashboard.signalFilter = "all";
dashboard.treeFilter = "dirty";
assert.deepEqual(names(), ["beta"]);
dashboard.treeFilter = "clean";
assert.deepEqual(names(), ["alpha", "delta", "gamma"]);

dashboard.treeFilter = "all";
dashboard.checksFilter = "none";
assert.deepEqual(names(), ["gamma"]);
dashboard.checksFilter = "has";
assert.deepEqual(names(), ["alpha", "beta", "delta"]);
dashboard.checksFilter = "all";

dashboard.filter = "eslint";
assert.deepEqual(names(), ["alpha"]);

assert.equal(dashboard.codecovLabel({ codecov: 96.72 }), "96.7%");
assert.equal(
  dashboard.codecovLabel({ codecov: 55.0, codecov_stale: true }),
  "55.0% (stale)",
);
assert.equal(dashboard.codecovLabel({}), "not reported");

// Icon/link helpers for the Checks column.
assert.equal(dashboard.scannerIcon("gh-actions"), "githubactions");
assert.equal(dashboard.scannerIcon("overcommit"), undefined);
assert.equal(
  dashboard.scannerLabel({ semgrep_license: "ce" }, "semgrep"),
  "semgrep CE",
);
assert.equal(
  dashboard.scannerLabel({ semgrep_license: "cloud-ci" }, "semgrep"),
  "semgrep cloud CI",
);
assert.equal(dashboard.scannerLabel({}, "sonar"), "sonar");
assert.equal(
  dashboard.scannerUrl({ sonarcloud_url: "https://sonar.example/p" }, "sonar"),
  "https://sonar.example/p",
);
assert.equal(dashboard.scannerUrl({}, "semgrep"), "https://semgrep.dev");
assert.equal(
  dashboard.githubSlug({ url: "https://github.com/owner/repo.git" }),
  "owner/repo",
);
assert.equal(
  dashboard.githubSlug({ url: "git@github.com:owner/repo.git" }),
  "owner/repo",
);
assert.equal(
  dashboard.codecovUrl({ url: "https://github.com/owner/repo.git" }),
  "https://app.codecov.io/gh/owner/repo",
);
assert.equal(
  dashboard.codefactorUrl({ url: "https://github.com/owner/repo.git" }),
  "https://www.codefactor.io/repository/github/owner/repo",
);
assert.equal(dashboard.sortIcon("name"), "arrow-up");
assert.equal(dashboard.sortIcon("checks"), "chevrons-up-down");

// History series for the progress-over-time chart: nullable fields keep
// nulls (gaps) instead of fabricating zeros.
dashboard.reportHistory = [
  {
    generated_at: "2026-10-18T10:00:00Z",
    summary: { problems: 4, dirty: 2, open_prs: 3 },
  },
  {
    generated_at: "2026-10-19T10:00:00Z",
    summary: {
      problems: 1,
      dirty: 0,
      open_prs: null,
      semgrep_open_findings: 5,
    },
  },
];
const series = dashboard.reportHistorySeries();
assert.equal(series.labels.length, 2);
assert.deepEqual(series.problems, [4, 1]);
assert.deepEqual(series.dirty, [2, 0]);
assert.deepEqual(series.openPRs, [3, null]);
assert.deepEqual(series.findings, [null, 5]);
// renderHistoryChart is a no-op without Chart.js/canvas in the vm context —
// chart instances live in module scope, not on the reactive component.
dashboard.renderHistoryChart();

console.log("Report sorting and signal filters passed.");
