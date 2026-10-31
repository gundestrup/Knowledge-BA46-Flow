// Shared fixture + recording harness for the demo screencast scripts.
// Builds KBM-flow, serves an isolated fixture corpus, drives real headless
// Chrome via CDP Page.startScreencast, and stitches the captured frames into
// an MP4 with ffmpeg. Each screencast-*.mjs script supplies only its tour.

import { spawn, spawnSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { createServer } from "node:net";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { launchBrowser, waitHTTP } from "./browser.mjs";

export const root = join(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
  "..",
);
export const screencastsDir = join(root, "docs", "assets", "screencasts");
// Encode framerate = playback speed. Frames arrive on repaint, so a lower
// rate stretches each captured frame over more video seconds: KBM_DEMO_FPS=1
// makes the take play 2.5x slower. Chapter times derive from frame/FPS and
// scale automatically.
export const FPS = Number(process.env.KBM_DEMO_FPS ?? 1);
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function freePort() {
  const server = createServer();
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  await new Promise((resolve) => server.close(resolve));
  return port;
}

// Curated fixture corpus — themed docs with varied titles/tags/status and
// real bodies so the demo table, tag filter and suggestion cards show
// variety instead of 150 uniform "Demo note i" rows. KBM_DEMO_DOCS pads
// with generated filler when the table should look bigger; default keeps
// the tour readable on camera.
const md = (title, tags, status, body) =>
  `---\ntitle: ${title}\ntags: [${tags.join(", ")}]\nstatus: ${status}\n---\n\n# ${title}\n\n${body}\n`;

const CURATED = [
  [
    "go-testing-patterns.md",
    md(
      "Go testing patterns",
      ["go", "testing", "code-quality"],
      "active",
      "Table-driven tests, golden files and race-detector habits.\n\nSee also [SQLite migration guide](sqlite-migration-guide.md).",
    ),
  ],
  [
    "deploy-runbook.md",
    md(
      "Production deploy runbook",
      ["ops", "runbook"],
      "active",
      "Pre-flight checks, canary order and rollback steps.",
    ),
  ],
  [
    "api-design-decisions.md",
    md(
      "REST API design decisions",
      ["api", "architecture"],
      "draft",
      "Versioning strategy, error envelope and pagination rules.",
    ),
  ],
  [
    "local-llm-notes.md",
    md(
      "Local LLM serving notes",
      ["llm", "ollama"],
      "active",
      "Model lane defaults, num_ctx sizing and keep-alive tuning.",
    ),
  ],
  [
    "sqlite-migration-guide.md",
    md(
      "SQLite migration guide",
      ["database", "migration"],
      "active",
      "Schema versioning, backfill order and WAL notes.\n\nRelated: [Go testing patterns](go-testing-patterns.md).",
    ),
  ],
  [
    "frontend-a11y-checklist.md",
    md(
      "Frontend accessibility checklist",
      ["a11y", "frontend"],
      "draft",
      "Focus order, aria labels and contrast review steps.",
    ),
  ],
  [
    "release-process.md",
    md(
      "Release process",
      ["release", "git", "packaging"],
      "active",
      "Bump, gate, tag, publish — plus changelog rules.",
    ),
  ],
  [
    "incident-2026-09.md",
    md(
      "Incident report 2026-09",
      ["incident", "postmortem"],
      "active",
      "Timeline, root cause and follow-up actions.",
    ),
  ],
  [
    "duplicate-short-note.md",
    md(
      "Duplicate short note",
      ["draft"],
      "draft",
      "Tiny note that only restates the runbook title.",
    ),
  ],
  [
    "stub-idea.md",
    md("Stub idea", ["stub", "draft"], "draft", "Placeholder.\n"),
  ],
];

const FILLER = Number(process.env.KBM_DEMO_DOCS ?? 36);
const DOCS = Object.fromEntries([
  ...CURATED,
  ...Array.from({ length: Math.max(0, FILLER - CURATED.length) }, (_, i) => [
    `note-${String(i).padStart(3, "0")}.md`,
    `---\ntitle: Reference note ${i}\ntags: [reference, ${i % 4 === 0 ? "draft" : "active"}]\nstatus: ${i % 4 === 0 ? "draft" : "active"}\n---\n\n# Reference note ${i}\n\nBody text for reference note ${i}.\n`,
  ]),
]);

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

// setupDemo builds the binary, seeds an isolated fixture (corpus + report
// registry), starts the HTTP server, and opens a 1440x900 headless page.
// The returned cleanup() must run once the tour finishes or throws.
export async function setupDemo() {
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

  const seed = spawnSync(
    "go",
    [
      "run",
      "./scripts/e2e/seed",
      "-db",
      join(tmp, "state.db"),
      "-repo",
      repoDir,
      "-rich",
    ],
    { cwd: root, encoding: "utf8" },
  );
  if (seed.status !== 0)
    throw new Error(`seed failed: ${seed.stderr}${seed.stdout}`);

  const server = spawn(binary, ["serve", "--config", configPath], {
    stdio: ["ignore", "pipe", "pipe"],
  });
  let browser = null;
  try {
    await waitHTTP(`${baseURL}/api/ops`);
    browser = await launchBrowser(cdpPort);
    if (!browser) throw new Error("no Chrome/Chromium found for screencast");
    const page = await browser.newPage();
    await page.client.send("Page.navigate", { url: `${baseURL}/` });
    await page.client.send("Page.bringToFront");
    await page.client.send("Emulation.setDeviceMetricsOverride", {
      width: 1440,
      height: 900,
      deviceScaleFactor: 1,
      mobile: false,
    });

    return {
      tmp,
      baseURL,
      page,
      cleanup() {
        browser?.close();
        server.kill("SIGKILL");
      },
    };
  } catch (error) {
    browser?.close();
    server.kill("SIGKILL");
    throw error;
  }
}

// tour returns the small interaction vocabulary the screencast scripts use:
// navigate-and-settle, click up to N matches, stepped sleeps, step markers.
// When given the recording counter, each step() also records the current
// frame index so encodeMp4 can emit mp4 chapters — screencast frames arrive
// on repaint only, so chapter time must derive from frame index / FPS,
// never from wall-clock time.
export function tour(page, baseURL, frames) {
  const pause = sleep;
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
  const step = (m) => {
    frames?.chapters.push({ title: m, frame: frames.n });
    process.stderr.write(`step: ${m}\n`);
  };
  return { pause, goto, clickAll, step };
}

// framesDirFor is the durable per-screencast frames location under the
// gitignored data/ dir — kept on disk so the approve -> convert step can
// still read the lossless source after the run (tmp dies on reboot).
export function framesDirFor(name) {
  return join(root, "data", "screencast-frames", name);
}

// startRecording captures Page.startScreencast frames into framesDir
// (wiped first so stale frames never bleed into a later encode). PNG
// frames are lossless — JPEG q80 ringing around UI text would survive
// forever in the lossless APNG/WebP encodes, and PNG frames can feed
// img2webp/apng/avif directly without an extraction pass.
// Returns a frame counter for the completion message.
export async function startRecording(page, framesDir) {
  await rm(framesDir, { recursive: true, force: true });
  await mkdir(framesDir, { recursive: true });
  const counter = { n: 0, chapters: [] };
  page.client.on("Page.screencastFrame", (params) => {
    page.client
      .send("Page.screencastFrameAck", { sessionId: params.sessionId })
      .catch(() => {});
    writeFile(
      join(framesDir, `f${String(counter.n++).padStart(4, "0")}.png`),
      Buffer.from(params.data, "base64"),
    ).catch(() => {});
  });
  await page.client.send("Page.startScreencast", {
    format: "png",
    everyNthFrame: 1,
  });
  return counter;
}

// chapters = [{title, frame}] from tour step() markers; times are derived
// from frame index / FPS so they land on the right video second even though
// the capture wall clock was irregular. First chapter always starts at 0.
export function encodeMp4(framesDir, out, chapters = [], totalFrames = 0) {
  mkdirSync(dirname(out), { recursive: true });
  const metaLines = [";FFMETADATA1"];
  const spans = chapters
    .map((c, i) => {
      const start = Math.max(0, Math.round((c.frame / FPS) * 1000));
      const endFrame = chapters[i + 1]?.frame ?? totalFrames;
      return {
        title: c.title,
        start,
        end: Math.round((endFrame / FPS) * 1000),
      };
    })
    // Steps that ran before any repaint emitted a frame collapse to zero
    // duration — drop them, then re-anchor the first real chapter at 0.
    .filter((c) => c.end > c.start);
  spans.forEach((c, i) => {
    metaLines.push(
      "[CHAPTER]",
      "TIMEBASE=1/1000",
      `START=${i === 0 ? 0 : c.start}`,
      `END=${c.end}`,
      `title=${c.title}`,
    );
  });
  const inputs = [
    "-framerate",
    String(FPS),
    "-i",
    join(framesDir, "f%04d.png"),
  ];
  if (spans.length > 0) {
    const metaPath = join(framesDir, "chapters.ffmeta");
    writeFileSync(metaPath, `${metaLines.join("\n")}\n`);
    inputs.push("-i", metaPath, "-map", "0:v", "-map_chapters", "1");
  }
  spawnSync(
    "ffmpeg",
    [
      "-y",
      ...inputs,
      "-c:v",
      "libx264",
      "-pix_fmt",
      "yuv420p",
      "-movflags",
      "+faststart",
      "-vf",
      "scale=1280:-2",
      out,
    ],
    { stdio: "inherit" },
  );
}
