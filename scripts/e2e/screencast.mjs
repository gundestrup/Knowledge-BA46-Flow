// Demo screencast: builds KBM-flow, serves an isolated fixture corpus,
// clicks through the UI in real headless Chrome, and stitches periodic
// Page.captureScreenshot frames into an MP4 with ffmpeg.
// Usage: KBM_DEMO_OUT=demo.mp4 node scripts/e2e/screencast.mjs

import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, mkdir, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { createServer } from "node:net";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { findChrome, launchBrowser, waitHTTP } from "./lib/browser.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const out = process.env.KBM_DEMO_OUT || join(root, "data", "demo.mp4");
const framesDir = join(root, "data", "screencast-frames");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function freePort() {
  const server = createServer();
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  await new Promise((resolve) => server.close(resolve));
  return port;
}

const DOCS = Object.fromEntries(
  Array.from({ length: 150 }, (_, i) => [
    `note-${String(i).padStart(3, "0")}.md`,
    `---\ntitle: Demo note ${i}\ntags: [demo, ${i % 3 === 0 ? "draft" : "active"}]\nstatus: ${i % 3 === 0 ? "draft" : "active"}\n---\n\n# Demo note ${i}\n\nBody text for demo note ${i}.\n`,
  ]),
);

// Three git repos with deliberately different check coverage so the report
// page shows varied gate/signal icons: full JS tooling, bare repo, Go-only.
const REPOS = [
  {
    name: "alpha-node",
    files: {
      "README.md": "# alpha-node\n",
      "package.json": '{"name":"alpha-node","version":"1.0.0"}\n',
      ".github/workflows/ci.yml": "name: ci\non: push\njobs: {}\n",
      ".nvmrc": "22\n",
    },
  },
  { name: "beta-bare", files: { "README.md": "# beta-bare\n" } },
  { name: "gamma-go", files: { "go.mod": "module gamma\n\ngo 1.24\n" } },
];

function git(args, cwd) {
  spawnSync("git", args, { cwd, encoding: "utf8" });
}

let server = null;
let browser = null;
try {
  const httpPort = await freePort();
  const cdpPort = await freePort();
  const baseURL = `http://127.0.0.1:${httpPort}`;

  const tmp = await mkdtemp(join(tmpdir(), "kbm-demo-"));
  const repoDir = join(tmp, "repo");
  await mkdir(repoDir, { recursive: true });
  for (const [name, content] of Object.entries(DOCS))
    await writeFile(join(repoDir, name), content);

  const reportDir = join(tmp, "report");
  const entries = [];
  for (const repo of REPOS) {
    const dir = join(reportDir, repo.name);
    for (const [file, content] of Object.entries(repo.files)) {
      await mkdir(join(dir, dirname(file)), { recursive: true });
      await writeFile(join(dir, file), content);
    }
    git(["init", "-q"], dir);
    git(["add", "-A"], dir);
    git(
      [
        "-c",
        "user.email=demo@example.invalid",
        "-c",
        "user.name=demo",
        "commit",
        "-qm",
        "init",
      ],
      dir,
    );
    entries.push(
      `  ${repo.name}:\n    path: ${dir}\n    url: https://example.invalid/${repo.name}`,
    );
  }
  const registryPath = join(reportDir, "repos.yml");
  await writeFile(registryPath, `repos:\n${entries.join("\n")}\n`);

  const binary = join(tmp, "KBM-flow");
  const configPath = join(tmp, "config.json");
  await writeFile(
    configPath,
    JSON.stringify(
      {
        repository_path: repoDir,
        write_scope: "all",
        database_path: join(tmp, "state.db"),
        prompt_directory: join(root, "prompts"),
        ollama_url: "http://127.0.0.1:1",
        ollama_model: "demo-unused",
        http_addr: `127.0.0.1:${httpPort}`,
        report: { registry_path: registryPath, remote_enabled: false },
      },
      null,
      2,
    ),
  );

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

  server = spawn(binary, ["serve", "--config", configPath], {
    stdio: ["ignore", "pipe", "pipe"],
  });
  await waitHTTP(`${baseURL}/api/ops`);

  browser = await launchBrowser(cdpPort);
  const page = await browser.newPage();
  await page.client.send("Page.navigate", { url: `${baseURL}/` });
  await page.client.send("Page.bringToFront");
  await page.client.send("Emulation.setDeviceMetricsOverride", {
    width: 1440,
    height: 900,
    deviceScaleFactor: 1,
    mobile: false,
  });

  await mkdir(framesDir, { recursive: true });
  let n = 0;
  page.client.on("Page.screencastFrame", (params) => {
    page.client
      .send("Page.screencastFrameAck", { sessionId: params.sessionId })
      .catch(() => {});
    writeFile(
      join(framesDir, `f${String(n++).padStart(4, "0")}.jpg`),
      Buffer.from(params.data, "base64"),
    ).catch(() => {});
  });
  await page.client.send("Page.startScreencast", {
    format: "jpeg",
    quality: 80,
    everyNthFrame: 1,
  });
  const pause = (ms) => sleep(ms);
  const goto = async (url) => {
    await page.client.send("Page.navigate", { url: `${baseURL}${url}` });
    await pause(1800);
  };
  const clickAll = async (sel, max, pauseMs = 1200) => {
    const count = await page.eval(
      `document.querySelectorAll(${JSON.stringify(sel)}).length`,
    );
    for (let i = 0; i < Math.min(count, max); i++) {
      await page.eval(
        `document.querySelectorAll(${JSON.stringify(sel)})[${i}]?.click()`,
      );
      await pause(pauseMs);
    }
  };

  const step = (m) => process.stderr.write(`step: ${m}\n`);
  // Tour: landing → knowledge (help ?, checkbox, Scan job) → report.
  step("landing");
  await goto("/");
  await pause(1500);
  step("knowledge");
  await goto("/knowledge");
  step("help");
  await clickAll(".kbm-help-btn", 3);
  step("checkbox");
  await clickAll('input[type="checkbox"]', 2, 800);
  await page.eval(
    `[...document.querySelectorAll("button")].find(b=>/scan/i.test(b.textContent))?.click()`,
  );
  await pause(6000);
  step("report");
  await goto("/report");
  step("report-run");
  await page.eval(
    `[...document.querySelectorAll("button")].find(b=>b.querySelector(".icon-refresh-cw"))?.click()`,
  );
  await pause(15000);
  await goto("/knowledge");
  await pause(1500);

  await page.client.send("Page.stopScreencast");
  browser.close();
  browser = null;
  server.kill("SIGKILL");
  server = null;

  spawnSync(
    "ffmpeg",
    [
      "-y",
      "-framerate",
      "2.5",
      "-i",
      join(framesDir, "f%04d.jpg"),
      "-c:v",
      "libx264",
      "-pix_fmt",
      "yuv420p",
      "-vf",
      "scale=1280:-2",
      out,
    ],
    { stdio: "inherit" },
  );
  console.log(`screencast: ${out} (${n} frames)`);
} finally {
  if (browser) browser.close();
  if (server) server.kill("SIGKILL");
}
