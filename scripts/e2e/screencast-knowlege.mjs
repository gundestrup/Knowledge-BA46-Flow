// Knowledge-page demo screencast: builds KBM-flow, serves an isolated
// fixture corpus, clicks through the knowledge UI in real headless Chrome,
// and stitches the captured screencast frames into an MP4 with ffmpeg.
// Usage: KBM_DEMO_OUT=demo.mp4 node scripts/e2e/screencast-knowlege.mjs

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
  process.env.KBM_DEMO_OUT || join(screencastsDir, "screencast-knowlege.mp4");

const demo =
  process.env.KBM_DEMO_FIXTURE === "real"
    ? await setupRealDemo()
    : await setupDemo();
const { page, baseURL } = demo;
const framesDir = framesDirFor("knowlege");
const frames = await startRecording(page, framesDir);
const { pause, goto, step } = tour(page, baseURL, frames);
try {
  // Tour: landing (let dashboard populate) → knowledge → slow scroll to
  // the progress graph → slow scroll to suggestions → expand a
  // suggestion → collapse it → accept → apply.
  const clickText = async (re) => {
    const hit = await page.eval(
      `(() => { const b = [...document.querySelectorAll("button, summary")].find(x => ${re}.test(x.textContent)); if (b) { window.__lastClick = b; b.click(); } return b ? b.textContent.trim().slice(0, 40) : null; })()`,
    );
    process.stderr.write(`  click ${re} -> ${hit ?? "MISS"}\n`);
  };
  // Programmatic clicks don't move focus, so document.activeElement is
  // body — scrolling THAT into view jumps to the top. Re-center the
  // element we actually clicked instead.
  const centerLast = () =>
    page.eval(`window.__lastClick?.scrollIntoView({ block: "center" })`);
  // Stepped scroll: animates over `ms` so the motion is visible on camera
  // and each increment emits a repaint frame.
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

  step("landing");
  await goto("/");
  await pause(3000);
  step("knowledge");
  await goto("/knowledge");
  step("llm-config");
  await clickText("/llm configuration/i");
  await pause(1500);
  await clickText("/llm configuration/i");
  await pause(800);
  step("scroll-graph");
  await scrollToEl("#graph");
  await pause(1500);
  step("scroll-suggestions");
  await scrollToEl("#suggestions");
  await pause(1500);
  step("expand");
  await clickText("/proposed/i");
  // The expanding card grows the page and shifts the viewport — re-center
  // the open card so the frame reads as a deliberate move, not a jump.
  await centerLast();
  await pause(1500);
  step("collapse");
  await clickText("/proposed/i");
  await centerLast();
  await pause(500);
  step("accept");
  await clickText("/accept suggestion/i");
  await pause(1500);
  step("apply");
  await clickText("/apply accepted/i");
  await pause(500);
  // The shared confirm modal needs an explicit confirm click.
  await page.eval(`document.querySelector("[data-dialog-confirm]")?.click()`);
  await pause(2000);
} finally {
  await page.client.send("Page.stopScreencast").catch(() => {});
  demo.cleanup();
}

encodeMp4(framesDir, out, frames.chapters, frames.n);
console.log(
  `screencast: ${out} (${frames.n} frames, ${frames.chapters.length} chapters)`,
);
console.log(`frames (lossless, for screencast-convert.mjs): ${framesDir}`);
