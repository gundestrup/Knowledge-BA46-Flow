import assert from "node:assert/strict";
import { evalComponent } from "./eval-component.mjs";

const dashboard = evalComponent("dashboard.js").window.knowledgeDashboard();
dashboard.suggestions = [
  {
    id: 1,
    document_path: "b.md",
    origin: "analysis",
    change_types: ["body"],
  },
  {
    id: 2,
    document_path: "a.md",
    origin: "analysis",
    change_types: ["frontmatter"],
  },
  {
    id: 3,
    document_path: "a.md",
    origin: "deterministic",
    change_types: ["links"],
  },
];
assert.deepEqual(
  [...dashboard.noteGroups().map((note) => note.path)],
  ["a.md", "b.md"],
);
dashboard.typeFilter = "frontmatter";
let groups = dashboard.noteGroups();
assert.equal(groups.length, 1);
assert.deepEqual(
  [...groups[0].suggestions.map((suggestion) => suggestion.id)],
  [2, 3],
  "type facets must retain competing whole-file alternatives",
);
dashboard.originFilter = "deterministic";
assert.equal(dashboard.noteGroups().length, 0);
dashboard.typeFilter = "links";
groups = dashboard.noteGroups();
assert.equal(groups.length, 1);
assert.equal(groups[0].suggestions.length, 2);

const plan = {
  token: "reviewed-token",
  retire_count: 1,
  counts: { invalid: 1, blocked: 1 },
  items: [
    { id: 2, retire: true, state: "invalid" },
    { id: 3, retire: false, state: "blocked" },
  ],
};
let posts = 0;
dashboard.requestJSON = async (url) => {
  assert.equal(url, "/api/reconcile");
  return plan;
};
dashboard.postJSON = async (url, body) => {
  assert.equal(url, "/api/reconcile");
  assert.deepEqual({ ...body }, { confirm: true, token: "reviewed-token" });
  posts++;
  return { retired: 1 };
};
dashboard.refreshData = async () => {};
await dashboard.previewReconciliation();
assert.equal(posts, 0, "preview must never confirm implicitly");
assert.equal(dashboard.retireItems().length, 1);
dashboard.confirmDialog = async () => false;
await dashboard.confirmReconciliation();
assert.equal(posts, 0);
dashboard.confirmDialog = async () => true;
await dashboard.confirmReconciliation();
assert.equal(posts, 1);
assert.equal(dashboard.queuePlan, null);
assert.match(dashboard.notice, /No notes were changed/);

dashboard.queuePlan = plan;
dashboard.postJSON = async () => {
  throw new Error("preview changed; review again");
};
await dashboard.confirmReconciliation();
assert.match(dashboard.error, /preview changed/);
assert.equal(dashboard.queuePlan, plan);
assert.equal(dashboard.reconciling, false);
console.log(
  "Note grouping, type/origin facets and explicit queue confirmation passed.",
);
