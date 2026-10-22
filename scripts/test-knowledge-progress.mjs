import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const dashboardPath = new URL(
  "../internal/server/static/dashboard.js",
  import.meta.url,
);
const context = { window: {} };
vm.runInNewContext(readFileSync(dashboardPath, "utf8"), context);

const dashboard = context.window.knowledgeDashboard();
dashboard.progressDays = [
  {
    date: "2026-10-01",
    notes: 2,
    analyzed: 1,
    suggestions: 3,
    handled: 0,
    applied: 0,
  },
  {
    date: "2026-10-02",
    notes: 1,
    analyzed: 2,
    suggestions: 1,
    handled: 2,
    applied: 1,
  },
];
const series = dashboard.progressSeries();
// Spread into host-realm arrays — vm-created arrays carry the sandbox's
// Array prototype, which deepStrictEqual rejects.
assert.deepEqual([...series.labels], ["2026-10-01", "2026-10-02"]);
assert.deepEqual([...series.notes], [2, 3]);
assert.deepEqual([...series.analyzed], [1, 3]);
assert.deepEqual([...series.suggestions], [3, 4]);
assert.deepEqual([...series.handled], [0, 2]);
assert.deepEqual([...series.applied], [0, 1]);
assert.deepEqual([...series.backlog], [3, 2]);

// renderProgressChart is a no-op without Chart.js/canvas in the vm context —
// chart instances live in module scope, not on the reactive component.
dashboard.renderProgressChart();

// Suggestion action icons fall back to the generic file mark.
assert.equal(dashboard.actionIcon("merge"), "/static/icons/merge.svg");
assert.equal(dashboard.actionIcon("delete"), "/static/icons/trash-2.svg");
assert.equal(dashboard.actionIcon("rename"), "/static/icons/file-text.svg");

console.log("Knowledge progress series passed.");
