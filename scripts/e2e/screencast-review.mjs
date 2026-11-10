// Review-loop demo screencast: same fixture/recording stack as
// screencast-knowlege.mjs, but a tighter tour for the README animation —
// knowledge dashboard → LLM configuration → suggestions → accept/reject
// → apply → results. Output feeds docs/assets/demo/review-flow.* via
// scripts/docs-demo.sh (screencast-convert.mjs on the durable frames).
// Usage: KBM_DEMO_OUT=demo.mp4 node scripts/e2e/screencast-review.mjs

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
  join(root, "docs", "assets", "demo", "review-flow.mp4");

const demo =
  process.env.KBM_DEMO_FIXTURE === "real"
    ? await setupRealDemo()
    : await setupDemo();
const { page, baseURL } = demo;
const framesDir = framesDirFor("review");
const frames = await startRecording(page, framesDir);
const { pause, goto, step } = tour(page, baseURL, frames);
try {
  const clickText = async (re) => {
    const hit = await page.eval(
      `(() => { const b = [...document.querySelectorAll("button, summary")].find(x => ${re}.test(x.textContent)); if (b) { window.__lastClick = b; b.click(); } return b ? b.textContent.trim().slice(0, 40) : null; })()`,
    );
    process.stderr.write(`  click ${re} -> ${hit ?? "MISS"}\n`);
  };
  const centerLast = () =>
    page.eval(`window.__lastClick?.scrollIntoView({ block: "center" })`);
  const scrollToEl = async (sel, ms = 1000, offset = 80) => {
    const top = await page.eval(
      `(() => { const el = document.querySelector(${JSON.stringify(sel)}); return el ? el.getBoundingClientRect().top + window.scrollY - ${offset} : document.body.scrollHeight; })()`,
    );
    const n = 10;
    for (let i = 1; i <= n; i++) {
      await page.eval(`window.scrollTo(0, ${Math.round((top * i) / n)})`);
      await pause(ms / n);
    }
  };

  step("knowledge");
  await goto("/knowledge");
  await pause(2200);
  step("llm-config");
  await clickText("/llm configuration/i");
  await pause(1800);
  await clickText("/llm configuration/i");
  await pause(600);
  step("suggestions");
  await scrollToEl("#suggestions");
  await pause(1800);
  step("expand");
  await clickText("/proposed/i");
  await centerLast();
  await pause(2000);
  step("accept");
  await clickText("/accept suggestion/i");
  await pause(1600);
  step("reject");
  await clickText("/^\\s*reject\\s*$/i");
  await pause(1200);
  // The reject prompt dialog shares the modal — type the note on camera so
  // the feedback-to-model channel is visible, then confirm.
  await page.eval(`(() => {
    const i = document.querySelector(".modal-card .input");
    if (i) {
      i.value = "move needs the target dir first";
      i.dispatchEvent(new Event("input", { bubbles: true }));
    }
  })()`);
  await pause(1200);
  await page.eval(`document.querySelector("[data-dialog-confirm]")?.click()`);
  await pause(1600);
  step("apply");
  await clickText("/apply accepted/i");
  await pause(600);
  await page.eval(`document.querySelector("[data-dialog-confirm]")?.click()`);
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
