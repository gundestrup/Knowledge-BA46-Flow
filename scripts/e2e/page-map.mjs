// Prints a document-order outline of a KBM-flow page: anchored sections
// top-to-bottom and, inside each, the interactive elements in the order
// the page builds them (buttons, links, checkboxes, selects, headings) —
// with x/y position, size, disabled state and title text. Use it to pick
// scroll anchors and clickText matchers when writing screencast tours.
// Usage:
//   node scripts/e2e/page-map.mjs /knowledge   (default curated fixture)
//   KBM_DEMO_FIXTURE=real node scripts/e2e/page-map.mjs /report

import { setupDemo } from "./lib/screencast-demo.mjs";
import { setupRealDemo } from "./lib/screencast-real.mjs";

const path = process.argv[2] ?? "/knowledge";

const demo =
  process.env.KBM_DEMO_FIXTURE === "real"
    ? await setupRealDemo()
    : await setupDemo();
const { page, baseURL } = demo;
try {
  await page.client.send("Page.navigate", { url: `${baseURL}${path}` });
  await new Promise((r) => setTimeout(r, 3000));

  const probe = await page.eval(`(() => {
    const txt = (el, n) =>
      (el.textContent || "").replace(/\\s+/g, " ").trim().slice(0, n);
    const items = [];
    for (const el of document.querySelectorAll(
      "section[id],main[id],article[id],div[id],nav,h1,h2,h3,h4,button,summary,a[href],input,select,textarea"
    )) {
      const r = el.getBoundingClientRect();
      if (r.width < 12 || r.height < 8) continue;
      const anchor = el.closest("section[id],main[id],article[id],div[id]");
      items.push({
        t: el.tagName.toLowerCase(),
        id: el.id || "",
        text: txt(el, 60),
        title: el.getAttribute("title") || "",
        dis: el.disabled === true || el.getAttribute("aria-disabled") === "true",
        sec: anchor && anchor !== el ? anchor.id : "",
        x: Math.round(r.x), y: Math.round(r.y),
        w: Math.round(r.width), h: Math.round(r.height),
      });
    }
    return JSON.stringify({
      items,
      winW: innerWidth, winH: innerHeight,
      bodyH: document.body.scrollHeight,
    });
  })()`);

  const { items, winW, winH, bodyH } = JSON.parse(probe);
  console.log(`=== ${path} @ ${winW}x${winH} (page ${bodyH}px tall) ===`);

  const SHORT = {
    button: "btn",
    a: "lnk",
    input: "chk",
    select: "sel",
    textarea: "txt",
    summary: "sum",
  };
  let lastSec = null;
  let printed = 0;
  const seenSections = [];
  for (const it of items) {
    const isAnchor =
      (it.t === "section" ||
        it.t === "main" ||
        it.t === "article" ||
        (it.t === "div" && it.id)) &&
      it.id;
    if (isAnchor) {
      lastSec = it.id;
      seenSections.push(it);
      console.log(
        `\n#${it.id}  y=${it.y}  ${it.w}x${it.h}  — ${it.text.slice(0, 50)}`,
      );
      printed = 0;
      continue;
    }
    if (printed >= 60) continue;
    printed++;
    if (it.t === "nav") {
      console.log(`  [nav] y=${it.y}  "${it.text.slice(0, 60)}"`);
      continue;
    }
    if (/^h[1-4]$/.test(it.t)) {
      console.log(`  ${it.t}  y=${it.y}  "${it.text.slice(0, 60)}"`);
      continue;
    }
    const tag = SHORT[it.t] ?? it.t;
    const meta = [
      `${it.x},${it.y} ${it.w}x${it.h}`,
      it.dis ? "disabled" : "",
      it.title ? `title="${it.title.slice(0, 45)}"` : "",
      it.id ? `#${it.id}` : "",
    ]
      .filter(Boolean)
      .join(" ");
    console.log(`  [${tag}] ${meta}  "${it.text.slice(0, 55)}"`);
  }
} finally {
  demo.cleanup();
}
