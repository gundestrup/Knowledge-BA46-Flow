import assert from "node:assert/strict";
import { evalComponent } from "./eval-component.mjs";

const context = evalComponent("dashboard.js");
const dashboard = context.window.knowledgeDashboard();
dashboard.t = (_id, fallback) => fallback;

// vm-run code returns foreign-prototype arrays — compare as strings.
const ops = (a, b) =>
  dashboard
    .diffLines(a, b)
    .map((o) => o.t)
    .join(",");

// Identical inputs are all context — the "no changes" path.
assert.equal(ops("a\nb\nc", "a\nb\nc"), "ctx,ctx,ctx");

// An inserted line shows as add surrounded by context.
assert.equal(ops("a\nc", "a\nb\nc"), "ctx,add,ctx");

// A removed line shows as del.
assert.equal(ops("a\nb\nc", "a\nc"), "ctx,del,ctx");

// A changed line is a del/add pair — the reviewer's "changed" signal.
assert.equal(ops("a\nb\nc", "a\nB\nc"), "ctx,del,add,ctx");

// Common suffix stays context even when the middle rewrites.
{
  const got = dashboard.diffLines("h1\nold\n\nend", "h1\nnew\n\nend");
  assert.equal(got.at(-1).t, "ctx");
  assert.equal(got.at(-1).s, "end");
  assert.equal(got.map((o) => o.t).join(","), "ctx,del,add,ctx,ctx");
}

// proposalHasDiff needs a real string current_content and no read error.
assert.equal(dashboard.proposalHasDiff(null), false);
assert.equal(dashboard.proposalHasDiff({ proposed: "x" }), false);
assert.equal(
  dashboard.proposalHasDiff({ proposed: "x", currentError: "missing" }),
  false,
);
assert.equal(dashboard.proposalHasDiff({ proposed: "x", current: "x" }), true);

// Ops carry raw text (x-text escapes nothing — markup stays literal text).
{
  const got = dashboard.diffLines("line <one>", "line <two>");
  assert.equal(got[0].s, "line <one>");
  assert.equal(got[1].s, "line <two>");
}

// toggleProposal stores both texts and defaults to the diff view.
const requests = [];
dashboard.requestJSON = async (path) => {
  requests.push(path);
  return {
    proposed_content: "# New\n\nadded",
    current_content: "# New",
  };
};
await dashboard.toggleProposal(7);
assert.deepEqual(requests, ["/api/suggestions/7"]);
assert.equal(dashboard.proposals[7].proposed, "# New\n\nadded");
assert.equal(dashboard.proposals[7].current, "# New");
assert.equal(dashboard.proposalView[7], "diff");
assert.equal(
  dashboard.proposals[7].diff.map((o) => o.t).join(","),
  "ctx,add,add",
);
assert.equal(dashboard.proposals[7].diff[2].s, "added");
assert.equal(dashboard.proposals[7].unchanged, false);

// Identical current/proposed marks unchanged — the UI shows the note.
dashboard.requestJSON = async () => ({
  proposed_content: "same",
  current_content: "same",
});
await dashboard.toggleProposal(8);
assert.equal(dashboard.proposals[8].unchanged, true);

// A read error on the server degrades to the full-text view.
dashboard.requestJSON = async () => ({
  proposed_content: "x",
  current_content_error: "file not found or unreadable",
});
await dashboard.toggleProposal(9);
assert.equal(dashboard.proposalView[9], "full");

// A failed fetch still surfaces the translated error as the content.
dashboard.requestJSON = async () => {
  throw new Error("boom");
};
await dashboard.toggleProposal(11);
assert.match(
  dashboard.proposals[11].proposed,
  /Unable to load proposed content/,
);
assert.equal(dashboard.proposalView[11], "full");

console.log("Proposal diff view passed.");
