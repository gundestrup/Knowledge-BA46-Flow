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
  // The post-confirm queue refresh re-pulls suggestions and counts — stub
  // those too so the test observes the light refresh path, not full data.
  if (url.startsWith("/api/suggestions")) return [];
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

// Optimistic decide: the decided card leaves the visible list as soon as
// the POST resolves — the UI must not wait for a refetch. Counts adjust
// locally, the card's cached proposal is dropped, and the background
// refresh is queue-scoped (never documents or the progress chart).
dashboard.statusFilter = "pending";
dashboard.suggestions = [
  {
    id: 10,
    document_path: "x.md",
    status: "pending",
    origin: "analysis",
    change_types: ["frontmatter"],
  },
  {
    id: 11,
    document_path: "y.md",
    status: "pending",
    origin: "analysis",
    change_types: ["body"],
  },
];
dashboard.pendingCount = 2;
dashboard.acceptedCount = 0;
dashboard.proposals[10] = { stale: true };
const fetched = [];
dashboard.requestJSON = async (url) => {
  fetched.push(url);
  if (url.startsWith("/api/suggestions")) return [];
  throw new Error(`unexpected fetch ${url}`);
};
dashboard.postJSON = async () => ({});
await dashboard.decide(10, "accepted");
assert.deepEqual(
  dashboard.suggestions.map((s) => s.id),
  [11],
  "decided card must leave the list immediately",
);
assert.equal(dashboard.pendingCount, 1);
assert.equal(dashboard.acceptedCount, 1);
assert.equal(dashboard.proposals[10], undefined);
await new Promise((resolve) => setTimeout(resolve, 0));
assert.ok(
  fetched.length > 0 && fetched.every((u) => u.startsWith("/api/suggestions")),
  `decide refresh must stay queue-scoped: ${fetched}`,
);

// Withdraw (accepted → pending) reverses the local counts the same way.
dashboard.statusFilter = "accepted";
dashboard.suggestions = [
  {
    id: 12,
    document_path: "w.md",
    status: "accepted",
    origin: "analysis",
    change_types: [],
  },
];
dashboard.acceptedCount = 1;
dashboard.pendingCount = 0;
await dashboard.decide(12, "pending");
assert.equal(dashboard.suggestions.length, 0);
assert.equal(dashboard.acceptedCount, 0);
assert.equal(dashboard.pendingCount, 1);

// A failed POST keeps the card — the server is the source of truth.
dashboard.statusFilter = "pending";
dashboard.suggestions = [
  {
    id: 13,
    document_path: "z.md",
    status: "pending",
    origin: "analysis",
    change_types: [],
  },
];
dashboard.postJSON = async () => {
  throw new Error("source changed");
};
await dashboard.decide(13, "accepted");
assert.equal(
  dashboard.suggestions.length,
  1,
  "failed decide must not drop the card",
);
assert.match(dashboard.error, /source changed/);
console.log(
  "Note grouping, facets, queue confirmation and optimistic decide passed.",
);

// In-context help: usage.md splits into "### slug" fragments rendered as
// blocks; the help toggle keeps a single fragment open at a time.
import { readFileSync } from "node:fs";
const usageMd = readFileSync(
  new URL("../internal/server/static/usage.md", import.meta.url),
  "utf8",
);
const frags = dashboard.parseUsage(usageMd);
for (const key of [
  "expand",
  "reanalyze",
  "external",
  "allow-cloud",
  "web-verify",
  "estimate",
  "lanes",
  "revalidate",
  "write-scope",
  "document-id",
]) {
  assert.ok(frags[key]?.length > 0, `missing usage fragment ${key}`);
}
assert.ok(frags.reanalyze.some((b) => b.t === "p"));
assert.ok(frags.reanalyze.some((b) => b.t === "li"));
dashboard.toggleHelp("external");
assert.equal(dashboard.helpOpen, "external");
dashboard.toggleHelp("reanalyze");
assert.equal(dashboard.helpOpen, "reanalyze", "only one help open");
dashboard.toggleHelp("reanalyze");
assert.equal(dashboard.helpOpen, null);
