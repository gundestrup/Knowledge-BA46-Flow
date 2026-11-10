// Report demo-loop screencast: same fixture/recording stack as
// screencast-report.mjs, but a tighter tour for the README animation —
// report page → refresh run → summary cards → repositories table →
// expand a repo row. Output feeds docs/assets/demo/report-flow.* via
// scripts/docs-demo.sh (screencast-convert.mjs on the durable frames).
// Usage: KBM_DEMO_OUT=demo.mp4 node scripts/e2e/screencast-report-flow.mjs

import { join } from "node:path";
import {
  encodeMp4,
  framesDirFor,
  root,
  setupDemo,
  startRecording,
  tour,
} from "./lib/screencast-demo.mjs";
import { setupRealDemo } from "./lib/screencast-real.mjs";

const out =
  process.env.KBM_DEMO_OUT ||
  join(root, "docs", "assets", "demo", "report-flow.mp4");

const demo =
  process.env.KBM_DEMO_FIXTURE === "real"
    ? await setupRealDemo()
    : await setupDemo();
const { page, baseURL } = demo;
const framesDir = framesDirFor("report-flow");
const frames = await startRecording(page, framesDir);
const { pause, goto, step } = tour(page, baseURL, frames);
try {
  const clickText = async (re) => {
    const hit = await page.eval(
      `(() => { const b = [...document.querySelectorAll("button, summary")].find(x => ${re}.test(x.textContent)); if (b) { window.__lastClick = b; b.click(); } return b ? b.textContent.trim().slice(0, 40) : null; })()`,
    );
    process.stderr.write(`  click ${re} -> ${hit ?? "MISS"}\n`);
  };

  step("report");
  await goto("/report");
  await pause(2000);
  step("report-run");
  await page.eval(
    `[...document.querySelectorAll("button")].find(b=>b.querySelector(".icon-refresh-cw"))?.click()`,
  );
  // Wait for the run to finish: the refresh button is disabled while busy
  // and returns to "Refresh report" once the snapshot lands.
  await page.waitFor(
    `[...document.querySelectorAll("button")].some(b => b.textContent.includes("Refresh report") && !b.disabled)`,
    90000,
    "report refresh finished",
  );
  await pause(1800);
  step("repos");
  await page.eval(
    `document.querySelector(".report-controls")?.scrollIntoView({ block: "start" })`,
  );
  await pause(1800);
  step("expand-repo");
  await clickText("/^\\s*expand\\s*$/i");
  await page.eval(`window.__lastClick?.scrollIntoView({ block: "center" })`);
  await pause(2500);
} finally {
  await page.client.send("Page.stopScreencast").catch(() => {});
  demo.cleanup();
}

encodeMp4(framesDir, out, frames.chapters, frames.n);
console.log(
  `screencast: ${out} (${frames.n} frames, ${frames.chapters.length} chapters)`,
);
console.log(`frames (lossless, for screencast-convert.mjs): ${framesDir}`);
