// Real-fixture demo setup: clones the production state.db numbers (document
// and suggestion counts, statuses, origins, confidences) with every
// content-bearing field scrubbed to anonymous placeholders — filenames,
// titles, bodies, reasons and repo names never hit the recording.
// Corpus files are generated placeholders at the anonymized paths; the
// document/directory structure comes from production.
// Usage: KBM_DEMO_FIXTURE=real node scripts/e2e/screencast-knowlege.mjs
//        KBM_REAL_DB=data/example-real-full.db (default)

import { spawn, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { createServer } from "node:net";
import { dirname, join } from "node:path";
import { launchBrowser, waitHTTP } from "./browser.mjs";
import { root } from "./screencast-demo.mjs";

const REAL_DB =
  process.env.KBM_REAL_DB || join(root, "data", "example-real-full.db");

const sql = (db, q) =>
  spawnSync("sqlite3", ["-batch", db, q], { encoding: "utf8" }).stdout.trim();

const columns = (db, table) =>
  sql(db, `PRAGMA table_info(${table})`)
    .split("\n")
    .map((l) => l.split("|")[1])
    .filter(Boolean);

async function freePort() {
  const server = createServer();
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  await new Promise((resolve) => server.close(resolve));
  return port;
}

// Same three plausible fixture repos as the synthetic fixture — renamed to
// neutral names, since production registry entries must not appear on
// camera. Refresh runs real local checks against these dirs.
const REPOS = [
  {
    name: "acme-web",
    files: {
      "README.md": "# acme-web\n",
      "package.json": '{"name":"acme-web","version":"1.0.0"}\n',
      ".github/workflows/ci.yml": "name: ci\non: push\njobs: {}\n",
      ".nvmrc": "22\n",
    },
  },
  { name: "acme-docs", files: { "README.md": "# acme-docs\n" } },
  { name: "acme-cli", files: { "go.mod": "module acme-cli\n\ngo 1.24\n" } },
];

function git(args, cwd) {
  spawnSync("git", args, { cwd, encoding: "utf8" });
}

function writeRepos(reportDir) {
  const entries = [];
  for (const repo of REPOS) {
    const dir = join(reportDir, repo.name);
    for (const [file, content] of Object.entries(repo.files)) {
      mkdirSync(join(dir, dirname(file)), { recursive: true });
      writeFileSync(join(dir, file), content);
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
  writeFileSync(registryPath, `repos:\n${entries.join("\n")}\n`);
  return registryPath;
}

export async function setupRealDemo() {
  const httpPort = await freePort();
  const cdpPort = await freePort();
  const baseURL = `http://127.0.0.1:${httpPort}`;
  const tmp = mkdtempSync(join(tmpdir(), "kbm-demo-real-"));
  const dbPath = join(tmp, "state.db");
  const repoDir = join(tmp, "repo");

  // 1. Clone the production db (backup API so WAL state is consistent).
  spawnSync("sqlite3", [REAL_DB, `.backup '${dbPath}'`], { encoding: "utf8" });

  // 2. Scrub documents: keep counts, structure and numeric lineage;
  //    replace every human-readable field.
  const oldPaths = sql(dbPath, "SELECT path FROM documents ORDER BY path")
    .split("\n")
    .filter(Boolean);
  const pathMap = new Map();
  oldPaths.forEach((p, i) => {
    const dir = dirname(p).replace(/^\./, "docs");
    pathMap.set(
      p,
      `${dir === "." ? "docs" : dir}/doc-${String(i).padStart(4, "0")}.md`,
    );
  });
  const esc = (s) => s.replaceAll("'", "''");
  for (const [oldP, newP] of pathMap) {
    sql(
      dbPath,
      `UPDATE documents SET path='${esc(newP)}', title='Document ${newP.slice(0, -3)}', summary='Generic summary for ${newP.slice(0, -3)}' WHERE path='${esc(oldP)}'`,
    );
  }
  // Drop any JSON/blob columns that could carry content verbatim.
  for (const col of columns(dbPath, "documents")) {
    if (["frontmatter_json", "embedding", "sketch"].includes(col))
      sql(dbPath, `UPDATE documents SET ${col}=NULL`);
  }

  // 3. Scrub suggestions: preserve count/status/origin/action/confidence,
  //    rewrite paths/reasons/content.
  const suggCols = columns(dbPath, "suggestions");
  const suggIds = sql(dbPath, "SELECT id FROM suggestions ORDER BY id")
    .split("\n")
    .filter(Boolean);
  for (const id of suggIds) {
    const oldDoc = sql(
      dbPath,
      `SELECT document_path FROM suggestions WHERE id=${id}`,
    );
    const newDoc = pathMap.get(oldDoc) ?? oldDoc;
    const sets = [`document_path='${esc(newDoc)}'`, `reason='demo-anonymized'`];
    if (suggCols.includes("target_path"))
      sets.push(`target_path='docs/moved-${id}.md'`);
    if (suggCols.includes("proposed_content"))
      sets.push(
        `proposed_content='---\\ntitle: Document\\n---\\n\\n# Document\\n\\nAnonymized proposed body.\\n'`,
      );
    if (suggCols.includes("proposed_frontmatter"))
      sets.push(`proposed_frontmatter=NULL`);
    for (const col of ["frontmatter_json", "sig", "details", "payload"])
      if (suggCols.includes(col)) sets.push(`${col}=NULL`);
    sql(dbPath, `UPDATE suggestions SET ${sets.join(",")} WHERE id=${id}`);
  }
  // Keep job rows — the "Pipeline progress" graph and "Background work"
  // card read them — but null any column that could carry paths, doc
  // names or payload text.
  const jobKeep = new Set([
    "id",
    "kind",
    "op",
    "status",
    "phase",
    "progress",
    "created_at",
    "started_at",
    "finished_at",
    "duration_ms",
  ]);
  for (const col of columns(dbPath, "jobs")) {
    if (!jobKeep.has(col)) sql(dbPath, `UPDATE jobs SET ${col}=NULL`);
  }

  // 4. Corpus: placeholder files at the anonymized paths so scan/link
  //    behaviour is real but no note content exists anywhere.
  for (const newP of pathMap.values()) {
    const f = join(repoDir, newP);
    mkdirSync(dirname(f), { recursive: true });
    writeFileSync(
      f,
      `---\ntitle: ${newP.slice(0, -3)}\ntags: [demo]\nstatus: active\n---\n\n# ${newP.slice(0, -3)}\n\nPlaceholder body for the demo fixture.\n`,
    );
  }

  const registryPath = writeRepos(join(tmp, "report"));

  const binary = join(tmp, "KBM-flow");
  const configPath = join(tmp, "config.json");
  writeFileSync(
    configPath,
    JSON.stringify(
      {
        repository_path: repoDir,
        write_scope: "all",
        database_path: dbPath,
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
  // No scan/seed here — the copied db already holds production counts and
  // suggestion state; scanning would only add document_ids to placeholders.

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
