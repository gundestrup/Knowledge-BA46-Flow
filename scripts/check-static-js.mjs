// Syntax-check every internal/server/static/*.js file. `node --check` only
// reads its first file argument, so the per-file command chain in package.json
// used to enumerate every script by hand — and drifted out of sync when new
// files appeared. Globbing the directory here makes the filesystem the single
// source of truth: a new static script is checked automatically.
import { execFileSync } from "node:child_process";
import { readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

const dir = new URL("../internal/server/static/", import.meta.url);
const files = readdirSync(dir)
  .filter((name) => name.endsWith(".js"))
  .sort();

for (const name of files) {
  execFileSync(
    process.execPath,
    ["--check", fileURLToPath(new URL(name, dir))],
    {
      stdio: "inherit",
    },
  );
}
console.log(`node --check: ${files.length} static scripts passed`);
