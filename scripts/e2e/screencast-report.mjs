// Report-page demo screencast: builds KBM-flow, serves an isolated fixture
// corpus plus a three-repo registry, runs the repository report in real
// headless Chrome, and stitches the captured screencast frames into an MP4
// with ffmpeg.
// Usage: KBM_DEMO_OUT=demo.mp4 node scripts/e2e/screencast-report.mjs

import { join } from "node:path";
import {
  encodeMp4,
  framesDirFor,
  screencastsDir,
  setupDemo,
  startRecording,
  tour,
} from "./lib/screencast-demo.mjs";
import { setupRealDemo } from "./lib/screencast-real.mjs";

const out =
  process.env.KBM_DEMO_OUT || join(screencastsDir, "screencast-report.mp4");

const demo =
  process.env.KBM_DEMO_FIXTURE === "real"
    ? await setupRealDemo()
    : await setupDemo();
const { page, baseURL } = demo;
const framesDir = framesDirFor("report");
const frames = await startRecording(page, framesDir);
const { pause, goto, step } = tour(page, baseURL, frames);
try {
  // Tour: landing → report (run checks, scroll repos table).
  step("landing");
  await goto("/");
  await pause(1500);
  step("report");
  await goto("/report");
  step("report-run");
  await page.eval(
    `[...document.querySelectorAll("button")].find(b=>b.querySelector(".icon-refresh-cw"))?.click()`,
  );
  await pause(25000);
  step("repos-scroll");
  for (let i = 0; i < 10; i++) {
    await page.eval(
      `(() => { const secs = [...document.querySelectorAll("section.card")]; const el = secs.find((s) => s.querySelector(".report-controls")) || secs[secs.length - 1]; const target = el ? el.offsetTop : document.body.scrollHeight; window.scrollTo(0, target * ${i + 1} / 10); })()`,
    );
    await pause(400);
  }
  await pause(5000);
} finally {
  await page.client.send("Page.stopScreencast").catch(() => {});
  demo.cleanup();
}

encodeMp4(framesDir, out, frames.chapters, frames.n);
console.log(
  `screencast: ${out} (${frames.n} frames, ${frames.chapters.length} chapters)`,
);
console.log(`frames (lossless, for screencast-convert.mjs): ${framesDir}`);
