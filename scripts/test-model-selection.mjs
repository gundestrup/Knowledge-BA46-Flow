import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const context = { window: {} };
vm.runInNewContext(
  readFileSync(
    new URL("../internal/server/static/dashboard.js", import.meta.url),
    "utf8",
  ),
  context,
);
const dashboard = context.window.knowledgeDashboard();
const choice = (name, locality = "local") => ({
  name,
  locality,
  configured: false,
  verified: true,
});
const payload = {
  generation_models: [choice("chat")],
  embedding_models: [choice("embed")],
  tasks: { embed: { default: "configured-embed" } },
  discovered: false,
};
const requests = [];
dashboard.requestJSON = async (path, options) => {
  requests.push({ path, body: options?.body && JSON.parse(options.body) });
  return payload;
};
dashboard.refreshJobs = async () => {};

await dashboard.loadModels();
assert.equal(requests.at(-1).path, "/api/models");
assert.equal(dashboard.generationChoices()[0].name, "chat");
assert.equal(dashboard.embeddingChoices()[0].name, "embed");
assert.match(dashboard.embeddingDefaultLabel(), /configured-embed/);
await dashboard.loadModels(true);
assert.equal(requests.at(-1).path, "/api/models?discover=1");

dashboard.model = "chat";
dashboard.embedModel = "embed";
await dashboard.startOperation("all");
assert.equal(requests.at(-1).body.model, "chat");
assert.equal(requests.at(-1).body.embed_model, "embed");
await dashboard.startOperation("run");
assert.equal(requests.at(-1).body.model, "chat");
assert.equal(requests.at(-1).body.embed_model, "embed");
await dashboard.startOperation("embed");
assert.equal(requests.at(-1).body.model, "");
assert.equal(requests.at(-1).body.embed_model, "embed");
await dashboard.startOperation("enrich");
assert.equal(requests.at(-1).body.model, "chat");
assert.equal(requests.at(-1).body.embed_model, "");
await dashboard.startOperation("scan");
assert.equal(requests.at(-1).body.model, "");
assert.equal(requests.at(-1).body.embed_model, "");
assert.equal(requests.at(-1).body.allow_cloud, false);

dashboard.generationModels = [choice("cloud-chat", "cloud")];
dashboard.embeddingModels = [choice("cloud-embed", "cloud")];
dashboard.model = "cloud-chat";
dashboard.embedModel = "cloud-embed";
let count = requests.length;
await dashboard.startOperation("all");
assert.equal(requests.length, count);
assert.match(dashboard.error, /external generation lane/);
dashboard.external = true;
await dashboard.startOperation("all");
assert.equal(requests.length, count);
assert.match(dashboard.error, /cloud\/remote embeddings/);
dashboard.allowCloud = true;
await dashboard.startOperation("all");
assert.equal(requests.at(-1).body.external, true);
assert.equal(requests.at(-1).body.allow_cloud, true);
assert.equal(requests.at(-1).body.embed_model, "cloud-embed");
await dashboard.startOperation("embed");
assert.equal(requests.at(-1).body.external, false);
assert.equal(requests.at(-1).body.allow_cloud, true);
assert.equal(requests.at(-1).body.model, "");

const previousModels = dashboard.generationModels;
dashboard.requestJSON = async () => ({ discovery_error: "Ollama unavailable" });
await dashboard.loadModels(true);
assert.equal(dashboard.generationModels, previousModels);
assert.equal(dashboard.model, "cloud-chat");
assert.equal(dashboard.embedModel, "cloud-embed");
assert.match(dashboard.error, /Ollama unavailable/);
assert.equal(dashboard.discovering, false);

dashboard.generationModels = [choice("different-chat")];
const stale = dashboard
  .generationChoices()
  .find((m) => m.name === "cloud-chat");
assert.ok(dashboard.modelUnavailable(stale, dashboard.discovered));
assert.equal(dashboard.model, "cloud-chat");
count = requests.length;
await dashboard.startOperation("all");
assert.equal(requests.length, count);
assert.match(dashboard.error, /unavailable or unverified/);

dashboard.discovered = true;
assert.equal(
  dashboard.modelUnavailable(
    {
      name: "unknown",
      configured: true,
      verified: false,
    },
    dashboard.discovered,
  ),
  true,
);
// Model lineage: stamps mark the "last used" option and drive the mismatch
// warnings; the estimate preview posts a dry_run and renders queue lines.
dashboard.external = false;
dashboard.error = "";
dashboard.stamps = {
  analyzed: { model: "chat", docs: 400 },
  embedded: { model: "embed", docs: 500 },
};
dashboard.taskInfo = {
  analyze: { default: "chat", options: ["chat"] },
  embed: { default: "configured-embed", options: ["configured-embed"] },
};
dashboard.task = "all";
dashboard.model = "chat";
dashboard.embedModel = "embed";
assert.match(dashboard.generationLabel(choice("chat")), /last used/);
assert.doesNotMatch(dashboard.generationLabel(choice("other")), /last used/);
assert.match(dashboard.embedLabel(choice("embed")), /last used/);
assert.equal(dashboard.generationMismatch(), false);
assert.equal(dashboard.embedMismatch(), false);

dashboard.model = "other";
assert.equal(dashboard.generationMismatch(), true);
// An empty override falls back to the configured task default — a default
// that differs from the stamp still warns.
dashboard.model = "";
dashboard.taskInfo.analyze.default = "other";
assert.equal(dashboard.generationMismatch(), true);
dashboard.taskInfo.analyze.default = "chat";
dashboard.embedModel = "";
assert.equal(dashboard.embedMismatch(), true); // configured-embed vs embed stamp
// The stamp follows the lane: the external checkbox swaps analyzed→external.
dashboard.stamps.external = { model: "ext-chat", docs: 10 };
dashboard.externalDefault = "ext-default";
dashboard.external = true;
assert.equal(dashboard.generationMismatch(), true); // ext-default ≠ ext-chat
assert.match(dashboard.generationStampLine(), /ext-chat/);
// Same external default as the stamp → no mismatch.
dashboard.externalDefault = "ext-chat";
assert.equal(dashboard.generationMismatch(), false);
dashboard.external = false;
dashboard.task = "expand";
dashboard.stamps.expand = { model: "chat", docs: 9 };
assert.match(dashboard.generationStampLine(), /chat.*9 docs/);

// The estimate preview posts a dry run for the current selection and renders
// the queue — the "before and after" the warning is about.
const estimatePayload = {
  documents: 500,
  pending: { embed: 500, analyze: 10 },
  dependents: 87,
  analyze_total: 97,
  warnings: ["embed uses emb-b — re-vectorized"],
};
dashboard.requestJSON = async (path, options) => {
  requests.push({ path, body: options?.body && JSON.parse(options.body) });
  return estimatePayload;
};
dashboard.task = "all";
dashboard.model = "";
dashboard.embedModel = "";
await dashboard.previewWork();
const estimateReq = requests.at(-1);
assert.equal(estimateReq.path, "/api/ops");
assert.equal(estimateReq.body.op, "all");
assert.equal(estimateReq.body.dry_run, true);
assert.deepEqual(dashboard.estimateWarnings(), [
  "embed uses emb-b — re-vectorized",
]);
const lines = dashboard.estimateLines();
assert.match(
  lines.find((l) => l.startsWith("embed")),
  /500/,
);
assert.match(
  lines.find((l) => l.startsWith("analyze")),
  /10 document\(s\) queued \+87 unchanged.*97 total/,
);
// A failed estimate surfaces the error and clears stale numbers.
dashboard.requestJSON = async () => {
  throw new Error("boom");
};
await dashboard.previewWork();
assert.equal(dashboard.estimate, null);
assert.equal(dashboard.error, "boom");

const markup = readFileSync(
  new URL("../internal/server/static/knowledge.html", import.meta.url),
  "utf8",
);
assert.match(markup, /id="generation-model"/);
assert.match(markup, /id="embedding-model"/);
assert.match(markup, /Refresh from Ollama/);
// Rejection captures the reviewer's optional "why" and sends it with the
// decision; Cancel aborts without touching the server; accept sends no note.
dashboard.requestJSON = async (path, options) => {
  requests.push({ path, body: options?.body && JSON.parse(options.body) });
  return payload;
};
dashboard.refreshData = async () => {};
context.window.prompt = () => "b.md is the canonical note";
await dashboard.decide(7, "rejected");
assert.deepEqual(requests.at(-1), {
  path: "/api/suggestions/7/decide",
  body: { status: "rejected", note: "b.md is the canonical note" },
});
context.window.prompt = () => null; // Cancel
{
  const count = requests.length;
  await dashboard.decide(8, "rejected");
  assert.equal(requests.length, count);
}
await dashboard.decide(9, "accepted");
assert.equal(requests.at(-1).body.note, "");

console.log(
  "Independent model selection, discovery, cloud consent, lineage/estimate preview, and rejection notes passed.",
);
