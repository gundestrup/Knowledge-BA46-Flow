import assert from "node:assert/strict";
import { evalComponent } from "./eval-component.mjs";

const dashboard = evalComponent("dashboard.js").window.knowledgeDashboard();
const results = [
  { id: 1, action: "expand", status: "applied", applied: true },
  { id: 2, action: "expand", status: "failed", error: "invalid body" },
  { id: 3, action: "expand", status: "superseded", error: "source changed" },
  { id: 0, action: "reindex-trigger", status: "applied", applied: true },
];
assert.deepEqual(
  { ...dashboard.applySummary(results) },
  { applied: 1, failed: 1, superseded: 1, blocked: 0 },
);
dashboard.acceptedCount = 3;
dashboard.confirmDialog = async () => true;
dashboard.refreshData = async () => {};
dashboard.requestJSON = async () => results;
await dashboard.applyAccepted();
assert.match(dashboard.notice, /1 applied, 1 failed, 1 superseded/);
assert.equal(dashboard.applyResults, results);
assert.equal(dashboard.applying, false);

results[3] = {
  id: 0,
  action: "reindex-trigger",
  status: "failed",
  applied: false,
  error: "permission denied",
};
await dashboard.applyAccepted();
assert.match(dashboard.notice, /1 applied, 1 failed, 1 superseded/);
assert.match(dashboard.error, /reindex trigger failed: permission denied/);
assert.equal(dashboard.applyResults.length, 4);
results.push({
  id: 4,
  action: "move",
  status: "blocked",
  error: "write scope",
});
await dashboard.applyAccepted();
assert.match(dashboard.notice, /1 blocked/);
assert.equal(dashboard.applySummary(results).blocked, 1);
console.log("Apply result accounting and diagnostics passed.");
