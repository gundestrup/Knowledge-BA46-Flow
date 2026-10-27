#!/usr/bin/env node
// Browser e2e suite for the KBM-flow web UI.
//
// Boots a fully deterministic fixture — a small markdown repository, a seeded
// state.db, two git repositories for the report page — then drives the real
// system Chrome through the DevTools Protocol and asserts DOM state plus the
// API traffic each button produces. No browser-automation dependency is
// vendored; Node's built-in WebSocket speaks CDP directly.
//
// Usage:
//   npm run e2e            # or: node scripts/e2e/run.mjs
//   KBM_E2E_CHROME=/path/to/chrome node scripts/e2e/run.mjs
//
// The suite exits 0 with a SKIP notice when no Chrome/Chromium is installed,
// so `make e2e` stays usable on machines without a browser.

import { spawn, spawnSync } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { findChrome, launchBrowser, waitHTTP } from "./lib/browser.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const APPLY_MARKER = "Seeded e2e paragraph — apply marker.";

let failures = 0;
function check(name, condition, detail = "") {
  if (condition) {
    console.log(`  ok   ${name}`);
  } else {
    failures++;
    console.log(`  FAIL ${name}${detail ? ` — ${detail}` : ""}`);
  }
}

async function freePort() {
  const server = createServer();
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  await new Promise((resolve) => server.close(resolve));
  return port;
}

function git(args, cwd) {
  const result = spawnSync("git", args, { cwd, encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(`git ${args.join(" ")} failed: ${result.stderr.trim()}`);
  }
}

const DOCS = {
  "alpha.md": `---
title: Alpha fixture
tags: [e2e]
status: active
related: [beta.md]
---

# Alpha fixture

Alpha body text for the e2e shelf.
`,
  "beta.md": `---
title: Beta fixture
tags: [e2e]
status: active
---

# Beta fixture

Beta body text for the e2e shelf.
`,
  "gamma.md": `---
title: Gamma fixture
tags: [e2e]
status: draft
---

# Gamma fixture

Gamma body text for the e2e shelf.
`,
  "delta.md": `---
title: Delta fixture
tags: [e2e]
status: active
---

# Delta fixture

Delta body text for the e2e shelf.
`,
};

async function buildFixture(tmp) {
  const repoDir = join(tmp, "repo");
  await mkdir(repoDir, { recursive: true });
  for (const [name, content] of Object.entries(DOCS)) {
    await writeFile(join(repoDir, name), content);
  }

  // Two tiny git repositories feed the repository report page.
  const reportDir = join(tmp, "report");
  const repos = ["repo-one", "repo-two"];
  const entries = [];
  for (const name of repos) {
    const dir = join(reportDir, name);
    await mkdir(dir, { recursive: true });
    await writeFile(join(dir, "README.md"), `# ${name}\n`);
    git(["init", "-q"], dir);
    // repo-two commits its README — a clean tree — while repo-one keeps the
    // README untracked so the working-tree filter has one repo per state.
    if (name === "repo-two") git(["add", "README.md"], dir);
    git(
      [
        "-c",
        "user.email=e2e@example.invalid",
        "-c",
        "user.name=e2e",
        "commit",
        "-qm",
        "init",
        "--allow-empty",
      ],
      dir,
    );
    entries.push(
      `  ${name}:\n    path: ${dir}\n    url: https://example.invalid/${name}`,
    );
  }
  const registryPath = join(reportDir, "repos.yml");
  await writeFile(registryPath, `repos:\n${entries.join("\n")}\n`);

  return { repoDir, registryPath };
}

async function main() {
  const chromePath = findChrome();
  if (!chromePath) {
    console.log(
      "SKIP e2e: no Chrome/Chromium found. Set KBM_E2E_CHROME to a Chrome binary to run the browser suite.",
    );
    process.exit(0);
  }

  const tmp = await mkdtemp(join(tmpdir(), "kbm-e2e-"));
  let browser;
  let server;
  const httpPort = await freePort();
  const cdpPort = await freePort();
  const baseURL = `http://127.0.0.1:${httpPort}`;

  try {
    const { repoDir, registryPath } = await buildFixture(tmp);
    const dbPath = join(tmp, "state.db");
    const binary = join(tmp, "KBM-flow");
    const configPath = join(tmp, "config.json");
    await writeFile(
      configPath,
      JSON.stringify(
        {
          repository_path: repoDir,
          write_scope: "all",
          database_path: dbPath,
          prompt_directory: join(root, "prompts"),
          ollama_url: "http://127.0.0.1:1",
          ollama_model: "e2e-unused",
          http_addr: `127.0.0.1:${httpPort}`,
          report: {
            registry_path: registryPath,
            remote_enabled: false,
          },
        },
        null,
        2,
      ),
    );

    console.log("fixture:", tmp);
    const build = spawnSync("go", ["build", "-o", binary, "./cmd/KBM-flow"], {
      cwd: root,
      encoding: "utf8",
    });
    if (build.status !== 0) throw new Error(`go build failed: ${build.stderr}`);

    const scan = spawnSync(binary, ["scan", "--config", configPath], {
      encoding: "utf8",
    });
    if (scan.status !== 0)
      throw new Error(`scan failed: ${scan.stderr}${scan.stdout}`);

    const seed = spawnSync(
      "go",
      ["run", "./scripts/e2e/seed", "-db", dbPath, "-repo", repoDir],
      {
        cwd: root,
        encoding: "utf8",
      },
    );
    if (seed.status !== 0)
      throw new Error(`seed failed: ${seed.stderr}${seed.stdout}`);
    console.log(seed.stdout.trim());

    const serverLog = join(tmp, "server.log");
    server = spawn(binary, ["serve", "--config", configPath], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    let serverOutput = "";
    server.stdout.on("data", (chunk) => (serverOutput += chunk));
    server.stderr.on("data", (chunk) => (serverOutput += chunk));
    try {
      await waitHTTP(`${baseURL}/api/suggestions?status=pending`);
    } catch (error) {
      throw new Error(`${error.message}\n--- server log ---\n${serverOutput}`);
    }
    await writeFile(serverLog, serverOutput);
    console.log(`serve up on ${baseURL} (log: ${serverLog})`);

    browser = await launchBrowser(cdpPort);
    const page = await browser.newPage();
    await page.client.send("Page.navigate", { url: `${baseURL}/` });

    // ---------------------------------------------------------------
    console.log("\nLanding page");
    await page.waitFor(
      `document.querySelectorAll(".portal-freshness").length > 0`,
      10000,
      "landing dashboard rendered",
    );
    const landingText = await page.eval(
      `document.querySelector(".portal-freshness").textContent`,
    );
    check(
      "landing dashboard has freshness text",
      landingText.length > 0,
      landingText,
    );
    check(
      "landing loaded /api/dashboard",
      page.requestsMatching("/api/dashboard").length > 0,
    );
    page.requests = [];
    await page.eval(
      `[...document.querySelectorAll("button")].find(
        (x) => x.textContent.includes("Refresh overview")).click()`,
    );
    await page.waitFor(
      `document.querySelector(".portal-freshness") !== null`,
      5000,
    );
    check(
      "refresh button refetches /api/dashboard",
      page.requestsMatching("/api/dashboard").length > 0,
      page.requestSummary(),
    );
    // Portal card is a real <a href>: following it exercises navigation.
    page.requests = [];
    await page.eval(`document.querySelector("a.knowledge-portal").click()`);
    await page.waitFor(`location.pathname === "/knowledge"`, 5000);
    check(
      "knowledge portal card navigates to /knowledge",
      (await page.eval(`location.pathname`)) === "/knowledge",
    );

    // ---------------------------------------------------------------
    console.log("\nKnowledge page");
    await page.waitFor(
      `document.querySelectorAll(".suggestion-card").length === 3`,
      10000,
      "3 seeded suggestion cards",
    );
    check(
      "pending suggestions API called",
      page.requestsMatching("/api/suggestions?status=pending").length > 0,
    );
    check(
      "documents API called",
      page.requestsMatching("/api/documents").length > 0,
    );
    check(
      "documents stat shows 4",
      (await page.eval(
        `[...document.querySelectorAll(".stat-number")][0].textContent`,
      )) === "4",
    );
    check(
      "documents table lists the 4 fixture files",
      (await page.eval(
        `document.querySelectorAll("table.is-hoverable tbody tr").length`,
      )) === 4,
    );
    check(
      "progress API called",
      page.requestsMatching("/api/progress").length > 0,
      page.requestSummary(),
    );
    await page.waitFor(
      `window.Chart !== undefined &&
        Chart.getChart(document.getElementById("knowledgeProgressChart")) !== undefined`,
      10000,
      "knowledge progress chart rendered",
    );
    check(
      "knowledge button icons load from /static/icons",
      await page.eval(
        `(() => {
          const icons = [...document.querySelectorAll(".kbm-icon")];
          return (
            icons.length >= 8 &&
            icons.every((i) => {
              const style = getComputedStyle(i);
              const mask = style.maskImage || style.webkitMaskImage;
              // Icons inside collapsed sections are display:none — width 0
              // is expected there; the asset check still applies.
              const hidden = i.offsetParent === null;
              return mask.includes("icons/") && (hidden || i.getBoundingClientRect().width > 0);
            })
          );
        })()`,
      ),
    );

    // Browser error relay — a thrown JS error must reach /api/client-log so
    // it lands in the server log (the real "Maximum call stack" incident
    // was invisible to Go-side logging before this).
    page.requests = [];
    await page.eval(
      `window.dispatchEvent(new ErrorEvent("error", { message: "e2e probe failure", filename: "/static/dashboard.js", lineno: 1 }))`,
    );
    await page.eval(`new Promise((r) => setTimeout(r, 600))`);
    check(
      "browser error relays to /api/client-log",
      page.requestsMatching("/api/client-log").length > 0,
      page.requestSummary(),
    );

    // Status filter -> new API call + re-render.
    page.requests = [];
    await page.selectValue(`select[x-model="statusFilter"]`, "accepted");
    await page.waitFor(
      `document.querySelectorAll(".suggestion-card").length === 0`,
      5000,
      "accepted filter renders empty",
    );
    check(
      "status filter fires /api/suggestions?status=accepted",
      page.requestsMatching("/api/suggestions?status=accepted").length > 0,
    );
    await page.selectValue(`select[x-model="statusFilter"]`, "pending");
    await page.waitFor(
      `document.querySelectorAll(".suggestion-card").length === 3`,
      5000,
    );

    // Origin filter -> local re-render, no new fetch.
    page.requests = [];
    await page.selectValue(`select[x-model="originFilter"]`, "analysis");
    await page.waitFor(
      `document.querySelectorAll(".suggestion-card").length === 1`,
      5000,
      "origin filter narrows to 1 card",
    );
    check(
      "origin filter does not refetch suggestions",
      page.requestsMatching("/api/suggestions?status=").length === 0,
    );
    await page.selectValue(`select[x-model="originFilter"]`, "all");
    await page.waitFor(
      `document.querySelectorAll(".suggestion-card").length === 3`,
      5000,
    );

    // Proposal inspection — the regression this suite was built for:
    // x-text must update when proposals[id] populates.
    const proposalCard = `(() => {
      const cards = [...document.querySelectorAll(".suggestion-card")];
      return cards.findIndex((c) => c.textContent.includes("expand"));
    })()`;
    const cardIndex = await page.eval(proposalCard);
    check("expand card present", cardIndex >= 0);
    page.requests = [];
    await page.eval(
      `(() => {
        const card = [...document.querySelectorAll(".suggestion-card")][${cardIndex}];
        const btn = [...card.querySelectorAll("button")].find((b) =>
          b.textContent.includes("Inspect proposed content"),
        );
        if (btn) btn.click();
      })()`,
    );
    await page.waitFor(
      `(() => {
        const pre = [...document.querySelectorAll(".suggestion-card")][${cardIndex}].querySelector("pre.proposal-content");
        return pre && pre.textContent.length > 100;
      })()`,
      10000,
      "proposal panel renders fetched content",
    );
    const pre = await page.eval(
      `[...document.querySelectorAll(".suggestion-card")][${cardIndex}].querySelector("pre.proposal-content").textContent`,
    );
    check(
      "proposal shows fetched content, not Loading…",
      pre.includes(APPLY_MARKER),
      pre.slice(0, 80),
    );
    check(
      "proposal fetch hit /api/suggestions/{id}",
      page
        .requestsMatching("/api/suggestions/")
        .filter((r) => !r.url.includes("?")).length > 0,
    );
    const buttonLabel = await page.eval(
      `[...document.querySelectorAll(".suggestion-card")][${cardIndex}].querySelector("button span[x-text]").textContent`,
    );
    check(
      "button flips to Hide proposed content",
      buttonLabel === "Hide proposed content",
      buttonLabel,
    );
    await page.eval(
      `(() => {
        const card = [...document.querySelectorAll(".suggestion-card")][${cardIndex}];
        const btn = [...card.querySelectorAll("button")].find((b) =>
          b.textContent.includes("Hide proposed content"),
        );
        if (btn) btn.click();
      })()`,
    );
    check(
      "proposal collapses on second click",
      (await page.eval(
        `[...document.querySelectorAll(".suggestion-card")][${cardIndex}].querySelector("button span[x-text]").textContent`,
      )) === "Inspect proposed content",
    );

    // Reject the enrich 'move' card.
    const rejectIndex = await page.eval(
      `(() => {
        const cards = [...document.querySelectorAll(".suggestion-card")];
        return cards.findIndex((c) => c.textContent.includes("rejected during review"));
      })()`,
    );
    check("enrich move card present", rejectIndex >= 0);
    // decide("rejected") opens the shared dialog modal for an optional note;
    // confirming with the input empty matches the old window.prompt("") path.
    page.requests = [];
    await page.eval(
      `[...document.querySelectorAll(".suggestion-card")][${rejectIndex}].querySelector("button.is-danger").click()`,
    );
    await page.waitFor(
      `document.querySelector(".modal.is-active [data-dialog-confirm]")`,
      5000,
      "reject dialog modal opens",
    );
    await page.eval(
      `document.querySelector(".modal.is-active [data-dialog-confirm]").click()`,
    );
    await page.waitFor(
      `document.querySelectorAll(".suggestion-card").length === 2`,
      5000,
      "rejected card leaves pending list",
    );
    check(
      "reject fires POST /api/suggestions/{id}/decide",
      page.requestsMatching("/decide").length > 0,
    );

    // Accept the analysis 'expand' card.
    const acceptIndex = await page.eval(
      `(() => {
        const cards = [...document.querySelectorAll(".suggestion-card")];
        return cards.findIndex((c) => c.textContent.includes("accepted then applied"));
      })()`,
    );
    check("expand card still present", acceptIndex >= 0);
    page.requests = [];
    await page.eval(
      `[...document.querySelectorAll(".suggestion-card")][${acceptIndex}].querySelector("button.is-success").click()`,
    );
    await page.waitFor(
      `document.querySelectorAll(".suggestion-card").length === 1`,
      5000,
      "accepted card leaves pending list",
    );
    check(
      "accept fires decide API",
      page.requestsMatching("/decide").length > 0,
    );

    // In-context option help: one fragment open at a time, with real
    // content from usage.md — guards the slug wiring, not just the JS.
    await page.eval(`document.querySelector(".kbm-help-btn").click()`);
    await page.waitFor(
      `[...document.querySelectorAll(".kbm-help-card")].some(
        (c) => c.offsetParent !== null && c.textContent.trim().length > 10)`,
      5000,
      "help card opens with content",
    );
    check(
      "one help card visible",
      (await page.eval(
        `[...document.querySelectorAll(".kbm-help-card")].filter(
          (c) => c.offsetParent !== null).length`,
      )) === 1,
    );
    const otherBtn = await page.eval(
      `[...document.querySelectorAll(".kbm-help-btn")][1].click(); "ok"`,
    );
    await page.waitFor(
      `[...document.querySelectorAll(".kbm-help-card")].filter(
        (c) => c.offsetParent !== null).length === 1`,
      5000,
      "second help opens, first closes",
    );
    await page.eval(
      `[...document.querySelectorAll(".kbm-help-btn")][1].click()`,
    );
    await page.waitFor(
      `[...document.querySelectorAll(".kbm-help-card")].filter(
        (c) => c.offsetParent !== null).length === 0`,
      5000,
      "help toggles closed",
    );
    // applyLabel() reads this.acceptedCount at component scope — the button
    // must re-render to "Apply accepted (1)".
    await page.waitFor(
      `(() => {
        const b = [...document.querySelectorAll("button")].find(
          (x) => x.textContent.includes("Apply accepted"));
        return b && !b.disabled;
      })()`,
      5000,
      "apply button enabled with count",
    );
    const applyLabel = await page.eval(
      `[...document.querySelectorAll("button")].find(
        (x) => x.textContent.includes("Apply accepted")).textContent.trim()`,
    );
    check("apply label shows count", applyLabel.includes("(1)"), applyLabel);

    // Apply the accepted suggestion — a real filesystem mutation on the
    // fixture repo. The shared dialog modal stands in for window.confirm.
    page.requests = [];
    await page.eval(
      `[...document.querySelectorAll("button")].find(
        (x) => x.textContent.includes("Apply accepted")).click()`,
    );
    await page.waitFor(
      `document.querySelector(".modal.is-active [data-dialog-confirm]")`,
      5000,
      "apply dialog modal opens",
    );
    await page.eval(
      `document.querySelector(".modal.is-active [data-dialog-confirm]").click()`,
    );
    await page.waitFor(
      `(() => {
        const n = document.querySelector(".notification.is-light");
        return n && n.textContent.includes("applied");
      })()`,
      10000,
      "apply notice rendered",
    );
    check(
      "apply fires POST /api/apply",
      page.requestMade("POST", "/api/apply"),
    );
    const notice = await page.eval(
      `document.querySelector(".notification.is-light").textContent`,
    );
    check(
      "apply notice reports 1 applied",
      notice.includes("1 applied"),
      notice,
    );

    // Verify the fixture file was actually mutated.
    const docsList = await page.eval(
      `(async () => (await fetch("/api/suggestions?status=applied")).json())()`,
    );
    const appliedDoc = docsList[0]?.document_path;
    check("applied suggestion recorded", !!appliedDoc);
    const fileContent = await readFile(join(repoDir, appliedDoc), "utf8");
    check(
      "file on disk contains applied marker",
      fileContent.includes(APPLY_MARKER),
    );

    // Model select rendered options through modelUnavailable(m, discovered).
    const modelOptions = await page.eval(
      `document.querySelectorAll("#generation-model option").length`,
    );
    check(
      "generation model options rendered",
      modelOptions >= 1,
      `${modelOptions}`,
    );

    // Jobs "Refresh" button → GET /api/ops.
    page.requests = [];
    await page.eval(
      `[...document.querySelectorAll("button")].find(
        (x) => x.textContent.trim() === "Refresh").click()`,
    );
    await new Promise((resolve) => setTimeout(resolve, 500));
    check(
      "jobs refresh fires GET /api/ops",
      page.requestMade("GET", "/api/ops"),
      page.requestSummary(),
    );

    // "Refresh from Ollama" against the dead fixture URL: the request must
    // fire and the UI must surface a notice instead of throwing.
    page.requests = [];
    await page.eval(
      `[...document.querySelectorAll("button")].find(
        (x) => x.textContent.includes("Refresh from Ollama")).click()`,
    );
    await page.waitFor(
      `(() => {
        const notice = document.querySelector("[x-text='modelNotice']");
        const banner = document.querySelector(".message.is-danger .message-body");
        return (notice && notice.textContent.trim().length > 0) ||
          (banner && banner.textContent.trim().length > 0);
      })()`,
      10000,
      "model discovery result surfaced",
    );
    check(
      "model discovery fires /api/models?discover=1",
      page.requestMade("GET", "/api/models"),
      page.requestSummary(),
    );
    const discoverySurface = await page.eval(
      `(() => {
        const banner = document.querySelector(".message.is-danger .message-body");
        const notice = document.querySelector("[x-text='modelNotice']");
        return (banner && banner.textContent.trim()) ||
          (notice && notice.textContent.trim()) || "";
      })()`,
    );
    check(
      "dead Ollama surfaces an error/notice, not a crash",
      discoverySurface.length > 0,
      discoverySurface,
    );

    // "Scan" — a real non-LLM op against the fixture.
    page.requests = [];
    await page.clickUntilRequest("Scan", "POST", "/api/ops", {
      body: '"op":"scan"',
    });
    await page.waitFor(
      `[...document.querySelectorAll(".job-row")].some(
        (r) => r.textContent.includes("scan") && r.textContent.includes("done"))`,
      20000,
      "scan job completes",
    );
    check(
      "scan fires POST /api/ops with op=scan",
      page.requestMade("POST", "/api/ops") &&
        page.requestBody("/api/ops").includes('"op":"scan"'),
      page.requestBody("/api/ops"),
    );
    check(
      "scan job row reports done",
      await page.eval(
        `[...document.querySelectorAll(".job-row")].some(
          (r) => r.textContent.includes("scan") && r.textContent.includes("done"))`,
      ),
    );

    // "Link Check" — real link-check op; fixture docs have no
    // external links, so it completes quickly with no network calls.
    await page.waitFor(
      `[...document.querySelectorAll("button")].some(
        (x) => x.textContent.includes("Link Check") && !x.disabled)`,
      20000,
      "idle before link-check",
    );
    page.requests = [];
    await page.clickUntilRequest("Link Check", "POST", "/api/ops", {
      body: '"op":"link-check"',
    });
    await page.waitFor(
      `[...document.querySelectorAll(".job-row")].some(
        (r) => r.textContent.includes("link-check") &&
          (r.textContent.includes("done") || r.textContent.includes("failed")))`,
      20000,
      "link-check job terminal",
    );
    check(
      "link-check fires POST /api/ops",
      page.requestMade("POST", "/api/ops") &&
        page.requestBody("/api/ops").includes('"op":"link-check"'),
      page.requestBody("/api/ops"),
    );

    // "Run task" with task=expand — no stubs in the fixture, so the job
    // completes without calling Ollama.
    await page.selectValue(`select[x-model="task"]`, "expand");
    page.requests = [];
    await page.clickUntilRequest("Run task", "POST", "/api/ops", {
      body: '"op":"expand"',
    });
    await page.waitFor(
      `[...document.querySelectorAll(".job-row")].some(
        (r) => r.textContent.includes("expand") && r.textContent.includes("done"))`,
      20000,
      "expand task completes",
    );
    check(
      "run task fires POST /api/ops with op=expand",
      page.requestMade("POST", "/api/ops") &&
        page.requestBody("/api/ops").includes('"op":"expand"'),
      page.requestBody("/api/ops"),
    );

    // Checkbox wiring + graceful LLM failure: reanalyze forces all docs
    // pending, analyze then hits the dead Ollama URL. The job must reach a
    // terminal state and report the failures — never hang or crash the page.
    await page.eval(
      `document.querySelector('input[x-model="reanalyze"]').click()`,
    );
    await page.eval(
      `document.querySelector('input[x-model="allowCloud"]').click()`,
    );
    check(
      "reanalyze checkbox toggles on",
      await page.eval(
        `document.querySelector('input[x-model="reanalyze"]').checked`,
      ),
    );
    page.requests = [];
    await page.clickUntilRequest("Analyse", "POST", "/api/ops", {
      body: '"op":"analyze"',
    });
    await page.waitFor(
      `[...document.querySelectorAll(".job-row")].some(
        (r) => r.textContent.includes("analyze") &&
          (r.textContent.includes("done") || r.textContent.includes("failed")))`,
      30000,
      "analyze job reaches terminal state",
    );
    const analyzeBody = page.requestBody("/api/ops");
    check(
      "analyze payload carries checkbox flags",
      analyzeBody.includes('"reanalyze":true') &&
        analyzeBody.includes('"allow_cloud":true') &&
        analyzeBody.includes('"op":"analyze"'),
      analyzeBody,
    );
    check(
      "analyze job row reports failures against dead Ollama",
      await page.eval(
        `[...document.querySelectorAll(".job-row")].some(
          (r) => r.textContent.includes("analyze") && r.textContent.includes("failed"))`,
      ),
    );

    // "Scan + analyse" — the run composite op. Waits for the previous job to
    // release busy, then scan succeeds and analyze reports failures.
    await page.waitFor(
      `[...document.querySelectorAll("button")].some(
        (x) => x.textContent.includes("Scan + analyse") && !x.disabled)`,
      30000,
      "idle before run",
    );
    page.requests = [];
    await page.clickUntilRequest("Scan + analyse", "POST", "/api/ops", {
      body: '"op":"run"',
    });
    await page.waitFor(
      `[...document.querySelectorAll(".job-row")].some(
        (r) => r.textContent.includes("run") &&
          (r.textContent.includes("done") || r.textContent.includes("failed")))`,
      30000,
      "run job reaches terminal state",
    );
    check(
      "scan+analyze fires POST /api/ops with op=run",
      page.requestMade("POST", "/api/ops") &&
        page.requestBody("/api/ops").includes('"op":"run"'),
      page.requestBody("/api/ops"),
    );

    // ---------------------------------------------------------------
    console.log("\nReport page");
    page.requests = [];
    await page.client.send("Page.navigate", { url: `${baseURL}/report` });
    const reportWait = Date.now();
    while (
      !page.requestMade("GET", "/api/report") &&
      Date.now() - reportWait < 10000
    ) {
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    check("report snapshot API called", page.requestMade("GET", "/api/report"));
    await page.waitFor(
      `(() => {
        const t = document.body.textContent;
        return t.includes("No report snapshot") ||
          t.includes("not configured") ||
          t.includes("report");
      })()`,
      10000,
      "report status text",
    );
    const statusText = await page.eval(
      `(() => {
        const t = document.body.textContent;
        return t.includes("No report snapshot") ||
          t.includes("Last local-only report") ||
          t.includes("Last full report") ||
          t.includes("not configured");
      })()`,
    );
    check("report status text rendered", statusText === true);

    // Refresh report -> POST /api/report/run -> snapshot with repo rows.
    page.requests = [];
    await page.clickUntilRequest("Refresh report", "POST", "/api/report/run");
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length >= 2`,
      30000,
      "report rows rendered after refresh",
    );
    check(
      "refresh fires POST /api/report/run",
      page.requestMade("POST", "/api/report/run"),
      page.requestSummary(),
    );
    check(
      "report history API called",
      page.requestMade("GET", "/api/report/history"),
      page.requestSummary(),
    );
    await page.waitFor(
      `window.Chart !== undefined &&
        Chart.getChart(document.getElementById("reportHistoryChart")) !== undefined`,
      10000,
      "report history chart rendered",
    );

    // Sort: default sortKey is name/asc — one click flips to desc.
    const before = await page.eval(
      `document.querySelectorAll("tbody tr")[0].querySelector("strong").textContent`,
    );
    await page.eval(
      `document.querySelector("thead th button.report-sort-button").click()`,
    );
    const after = await page.eval(
      `document.querySelectorAll("tbody tr")[0].querySelector("strong").textContent`,
    );
    check("sorting reorders rows", before !== after, `${before} -> ${after}`);

    // Expand a repo row — exercises repoDetails(repo, snapshot) inside x-for.
    await page.eval(
      `document.querySelector("tbody tr td button[aria-expanded]").click()`,
    );
    await page.waitFor(
      `document.querySelector("tbody tr.report-expanded-row pre.report-detail-content").textContent.length > 20`,
      5000,
      "repo details rendered",
    );
    const details = await page.eval(
      `document.querySelector("tbody tr.report-expanded-row pre.report-detail-content").textContent`,
    );
    check(
      "repo details show HEAD/URL lines",
      details.includes("URL:") &&
        details.includes("HEAD:") &&
        details.includes("Codecov:"),
      details.slice(0, 80),
    );

    // Text filter narrows the table via the filteredRepos getter.
    await page.eval(`(() => {
      const el = document.querySelector('input[x-model="filter"]');
      el.value = "repo-one";
      el.dispatchEvent(new Event("input", { bubbles: true }));
    })()`);
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length === 1`,
      5000,
      "filter narrows rows",
    );
    check(
      "name filter narrows to 1 row",
      (await page.eval(
        `document.querySelectorAll("tbody tr.report-expanded-row").length`,
      )) === 1,
    );
    await page.eval(`(() => {
      const el = document.querySelector('input[x-model="filter"]');
      el.value = "";
      el.dispatchEvent(new Event("input", { bubbles: true }));
    })()`);
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length >= 2`,
      5000,
    );

    // Working-tree filter: repo-one has an untracked README (dirty),
    // repo-two committed it (clean). Signal select scopes by alert kind.
    const repoRows = () =>
      page.eval(
        `document.querySelectorAll("tbody tr.report-expanded-row").length`,
      );
    await page.selectValue(`select[x-model="treeFilter"]`, "dirty");
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length === 1`,
      5000,
      "dirty filter narrows rows",
    );
    check("working-tree dirty shows 1 row", (await repoRows()) === 1);
    await page.selectValue(`select[x-model="treeFilter"]`, "clean");
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length === 1`,
      5000,
      "clean filter narrows rows",
    );
    check("working-tree clean shows 1 row", (await repoRows()) === 1);
    await page.selectValue(`select[x-model="treeFilter"]`, "all");

    // Both fixture repos run zero checks → "No checks" keeps both, "Has
    // checks" empties the table.
    await page.selectValue(`select[x-model="checksFilter"]`, "none");
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length === 2`,
      5000,
      "no-checks filter keeps rows",
    );
    check("checks none shows both rows", (await repoRows()) === 2);
    await page.selectValue(`select[x-model="checksFilter"]`, "has");
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length === 0`,
      5000,
      "has-checks filter empties rows",
    );
    check("checks has empties the table", (await repoRows()) === 0);
    await page.selectValue(`select[x-model="checksFilter"]`, "all");

    // No remote data in the fixture → dependabot/scanning/codefactor selects
    // each yield zero rows.
    for (const signal of [
      "dependabot",
      "code-scanning",
      "codefactor",
      "coverage",
    ]) {
      await page.selectValue(`select[x-model="signalFilter"]`, signal);
      await page.waitFor(
        `document.querySelectorAll("tbody tr.report-expanded-row").length === 0`,
        5000,
        `${signal} filter empties rows`,
      );
      check(`signal ${signal} shows 0 rows`, (await repoRows()) === 0);
    }
    await page.selectValue(`select[x-model="signalFilter"]`, "all");
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length === 2`,
      5000,
      "signal reset restores rows",
    );

    // Per-row refresh → POST /api/report/run?repo=<name>. Wait for !busy
    // first: the button is disabled while a report job runs.
    await page.waitFor(
      `[...document.querySelectorAll("tbody tr button")].some(
        (x) => x.textContent.includes("Refresh this repo") && !x.disabled)`,
      30000,
      "report idle before per-repo refresh",
    );
    page.requests = [];
    await page.clickUntilRequest(
      "Refresh this repo",
      "POST",
      "/api/report/run?repo=",
    );
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length >= 2`,
      30000,
      "per-repo refresh completes",
    );
    check(
      "per-repo refresh posts repo= param",
      page
        .requestsMatching("/api/report/run")
        .some((r) => r.url.includes("repo=")),
      page.requestSummary(),
    );

    // "Local checks only" → POST /api/report/run?no_remote=1.
    await page.waitFor(
      `[...document.querySelectorAll("button")].some(
        (x) => x.textContent.includes("Local checks only") && !x.disabled)`,
      30000,
      "report idle before local-only run",
    );
    await new Promise((resolve) => setTimeout(resolve, 750));
    page.requests = [];
    await page.clickUntilRequest(
      "Local checks only",
      "POST",
      "/api/report/run?no_remote=1",
    );
    check(
      "local-only button posts no_remote=1",
      page
        .requestsMatching("/api/report/run")
        .some((r) => r.url.includes("no_remote=1")),
      page.requestSummary(),
    );

    // "Force remote refresh" → POST /api/report/run?force=1 (remote is off in
    // the fixture, so this exercises the request path, not remote calls).
    await page.waitFor(
      `[...document.querySelectorAll("button")].some(
        (x) => x.textContent.includes("Force remote refresh") && !x.disabled)`,
      30000,
      "report idle before force run",
    );
    // FinishJob runs just before the op lock is released — the UI can report
    // idle a beat early; settle briefly so the click does not hit a 409.
    await new Promise((resolve) => setTimeout(resolve, 750));
    page.requests = [];
    await page.clickUntilRequest(
      "Force remote refresh",
      "POST",
      "/api/report/run?force=1",
    );
    check(
      "force refresh posts force=1",
      page
        .requestsMatching("/api/report/run")
        .some((r) => r.url.includes("force=1")),
      page.requestSummary(),
    );
    // Let the force job settle so its poll loop is not mid-flight at teardown.
    await page.waitFor(
      `document.querySelectorAll("tbody tr.report-expanded-row").length >= 2`,
      30000,
    );

    // ---------------------------------------------------------------
    console.log("\nConsole-error budget");
    check(
      "no page errors across all pages",
      page.errors.length === 0,
      page.errors.slice(0, 5).join(" | "),
    );

    // Server-log budget — stderr carries warnings, relayed browser errors
    // ("client error"), and panics. The only expected hit is the deliberate
    // error-relay probe; anything else means something broke silently.
    const unexpectedLog = serverOutput
      .split("\n")
      .filter((line) => !line.includes("e2e probe failure"))
      .filter((line) => /panic:|WARNING:|client error|client stack/.test(line));
    check(
      "no errors in server log",
      unexpectedLog.length === 0,
      unexpectedLog.slice(0, 5).join(" | "),
    );

    // ---------------------------------------------------------------
    // "Stop server" is last by necessity: it shuts the server down, and the
    // page's subsequent poll failures are expected, not app errors.
    // The shutdown endpoint 409s while any op holds the server lock, so wait
    // for the report job to fully release it first.
    console.log("\nGraceful shutdown");
    await page.waitFor(
      `[...document.querySelectorAll("button")].some(
        (x) => x.textContent.includes("Force remote refresh") && !x.disabled)`,
      30000,
      "report job fully finished",
    );
    await new Promise((resolve) => setTimeout(resolve, 750));
    page.requests = [];
    // Click the sidebar's Knowledge link — a real <a href>, not a URL
    // navigation — so the nav link itself is covered.
    await page.eval(
      `[...document.querySelectorAll("a[href='/knowledge']")].find(
        (x) => x.textContent.trim() === "Knowledge").click()`,
    );
    await page.waitFor(`location.pathname === "/knowledge"`, 10000);
    check(
      "report Knowledge button navigates to /knowledge",
      (await page.eval(`location.pathname`)) === "/knowledge",
    );
    await page.waitFor(
      `[...document.querySelectorAll("button")].some(
        (x) => x.textContent.includes("Stop server"))`,
      10000,
      "knowledge page reloaded",
    );
    await page.eval(
      `[...document.querySelectorAll("button")].find(
        (x) => x.textContent.includes("Stop server")).click()`,
    );
    await page.waitFor(
      `document.querySelector(".modal.is-active [data-dialog-confirm]")`,
      5000,
      "stop dialog modal opens",
    );
    await page.eval(
      `document.querySelector(".modal.is-active [data-dialog-confirm]").click()`,
    );
    check(
      "stop server fires POST /api/shutdown",
      page.requestMade("POST", "/api/shutdown"),
      page.requestSummary(),
    );
    // Graceful shutdown waits on the page's keep-alive poll connections —
    // close the tab so the server can drain and exit, like a real close.
    page.client.close();
    const exitWait = Date.now();
    while (server.exitCode === null && Date.now() - exitWait < 15000) {
      await new Promise((resolve) => setTimeout(resolve, 150));
    }
    check("server process exits after shutdown", server.exitCode !== null);
    server = server.exitCode === null ? server : null;
  } finally {
    if (browser) await browser.close();
    if (server) server.kill("SIGTERM");
    await rm(tmp, { recursive: true, force: true });
  }

  console.log(
    failures === 0 ? "\nE2E PASS" : `\nE2E FAIL (${failures} failed)`,
  );
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((error) => {
  console.error(`E2E ERROR: ${error.stack || error}`);
  process.exit(1);
});
