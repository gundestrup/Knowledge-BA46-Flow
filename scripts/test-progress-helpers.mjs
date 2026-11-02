import assert from "node:assert/strict";
import { evalComponent } from "./eval-component.mjs";

// Job progress bars must never bind a non-finite value — a detail like
// "N/0" (zero total) made jobPercent return Infinity and Alpine crashed
// setting <progress>.value. Both component copies must guard it.
for (const [file, factory] of [
  ["dashboard.js", "knowledgeDashboard"],
  ["report.js", "repoReportDashboard"],
]) {
  const component = evalComponent(file).window[factory]();
  assert.equal(component.jobPercent({ detail: "0/0" }), null, file);
  assert.equal(component.jobPercent({ detail: "5/10" }), 50, file);
  assert.equal(component.jobPercent({ detail: "done" }), null, file);
  assert.equal(component.jobPercent({}), null, file);
  assert.equal(component.jobPercent(null), null, file);
  assert.equal(component.jobPhase({ detail: "analyzing 1/91: x" }), null, file);
  assert.equal(
    component.jobPhase({ detail: "12/27: remote 3/12: repo" }),
    25,
    file,
  );
  assert.equal(component.jobPhase({ detail: "1/2: phase 3/0" }), null, file);
}
console.log("Job progress helpers passed.");
