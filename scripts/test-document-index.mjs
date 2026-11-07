import assert from "node:assert/strict";
import { evalComponent } from "./eval-component.mjs";

const dashboard = evalComponent("dashboard.js").window.knowledgeDashboard();
const requests = [];
let total = 45;
dashboard.requestJSON = async (url) => {
  requests.push(url);
  const query = new URLSearchParams(url.split("?")[1]);
  const offset = Number(query.get("offset"));
  const limit = Number(query.get("limit"));
  return {
    total,
    documents: Array.from(
      { length: Math.max(0, Math.min(limit, total - offset)) },
      (_, i) => ({ path: `doc-${offset + i}.md` }),
    ),
  };
};

await dashboard.loadDocuments();
assert.equal(dashboard.documentTotal, 45);
assert.equal(dashboard.documents.length, 20);
assert.equal(dashboard.documentPageLabel(), "1–20 of 45");
assert.equal(requests.length, 1);
assert.equal(dashboard.canNextDocumentPage(), true);

await dashboard.changeDocumentPage(1);
assert.equal(dashboard.documents[0].path, "doc-20.md");
assert.equal(dashboard.documentPageLabel(), "21–40 of 45");
await dashboard.changeDocumentPage(1);
assert.equal(dashboard.documents.length, 5);
assert.equal(dashboard.documentPageLabel(), "41–45 of 45");
assert.equal(dashboard.canNextDocumentPage(), false);
await dashboard.changeDocumentPage(1);
assert.equal(requests.length, 3);

total = 3;
await dashboard.loadDocuments();
assert.equal(dashboard.documentPage, 0);
assert.equal(dashboard.documentTotal, 3);
assert.equal(dashboard.documents.length, 3);
assert.equal(dashboard.documentsLoaded, true);
assert.equal(requests.length, 5); // stale offset retried at the new last page

console.log("Document index pagination passed.");
