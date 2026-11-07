import assert from "node:assert/strict";
import { evalComponent } from "./eval-component.mjs";

const context = evalComponent("report.js");

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
assert.equal(dashboard.scannerIcon("gh-actions"), "icon-githubactions");
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
  dashboard.scannerUrl(
    { semgrep_findings: { url: "https://semgrep.dev/orgs/x/findings?repo=y" } },
    "semgrep",
  ),
  "https://semgrep.dev/orgs/x/findings?repo=y",
);
assert.equal(
  dashboard.scannerUrl(
    { github: {}, web_url: "https://github.com/o/r" },
    "gh-actions",
  ),
  "https://github.com/o/r/actions",
);
assert.equal(
  dashboard.scannerUrl(
    { forgejo: {}, web_url: "https://git.example/o/r" },
    "forgejo",
  ),
  "https://git.example/o/r/actions",
);
assert.equal(
  dashboard.scannerUrl({}, "gh-actions"),
  "https://github.com/features/actions",
);
assert.equal(
  dashboard.scannerUrl(
    { github: { dependabot: 3 }, web_url: "https://github.com/o/r" },
    "dependabot",
  ),
  "https://github.com/o/r/pulls",
);
assert.equal(
  dashboard.scannerUrl(
    { github: { dependabot: 0 }, web_url: "https://github.com/o/r" },
    "dependabot",
  ),
  "https://github.com/o/r/security",
);
assert.equal(
  dashboard.scannerUrl({ web_url: "https://github.com/o/r" }, "dependabot"),
  "https://github.com/o/r/security",
);
assert.equal(
  dashboard.scannerUrl({}, "dependabot"),
  "https://github.com/dependabot",
);
assert.equal(
  dashboard
    .repoScanners({ scanners: ["sonar"], github: { dependabot: 0 } })
    .join(","),
  "sonar,dependabot",
);
assert.equal(
  dashboard
    .repoScanners({ scanners: ["dependabot"], github: { dependabot: 0 } })
    .join(","),
  "dependabot",
);
assert.equal(dashboard.repoScanners({}).join(","), "");
assert.equal(
  dashboard.scannerTitle(
    { semgrep_findings: { error: "remote HTTP 404" } },
    "semgrep",
  ),
  "Semgrep findings unavailable — remote HTTP 404",
);
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
assert.equal(dashboard.sortIcon("name"), "icon-arrow-up");
assert.equal(dashboard.sortIcon("checks"), "icon-chevrons-up-down");

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

dashboard.filter = "";
dashboard.snapshot.repos = Array.from({ length: 19 }, (_, i) => ({
  name: `repo-${String(i).padStart(2, "0")}`,
  path: `/repo-${i}`,
  problems: [],
  npm_projects: [],
}));
assert.equal(dashboard.visibleRepos.length, 8);
assert.equal(dashboard.reportPageLabel(), "1–8 of 19");
dashboard.changeReportPage(1);
assert.equal(dashboard.visibleRepos[0].name, "repo-08");
assert.equal(dashboard.reportPageLabel(), "9–16 of 19");
dashboard.changeReportPage(1);
assert.equal(dashboard.visibleRepos.length, 3);
assert.equal(dashboard.canNextReportPage(), false);
dashboard.sortBy("name");
assert.equal(dashboard.reportPage, 0);
assert.equal(dashboard.visibleRepos.length, 8);

const polling = context.window.repoReportDashboard();
polling.busy = true;
polling.notice = "Report started";
let running = true;
let pollsScheduled = 0;
const requests = [];
polling.schedulePoll = () => {
  pollsScheduled++;
};
polling.requestJSON = async (url) => {
  requests.push(url);
  if (url === "/api/report/jobs")
    return [{ status: running ? "running" : "done" }];
  if (url === "/api/ops") return [];
  if (url === "/api/report")
    return { configured: true, repos: [], summary: {}, generated_at: "now" };
  if (url === "/api/report/history") return { points: [] };
  throw new Error(`unexpected request: ${url}`);
};
await polling.pollJobs();
await polling.pollJobs();
assert.equal(polling.busy, true);
assert.equal(pollsScheduled, 2);
assert.deepEqual(requests, [
  "/api/report/jobs",
  "/api/ops",
  "/api/report/jobs",
  "/api/ops",
]);
running = false;
await polling.pollJobs();
assert.equal(polling.busy, false);
assert.equal(polling.notice, "");
assert.equal(requests.filter((url) => url === "/api/report").length, 1);
assert.equal(requests.filter((url) => url === "/api/report/history").length, 1);

console.log("Report sorting and signal filters passed.");
