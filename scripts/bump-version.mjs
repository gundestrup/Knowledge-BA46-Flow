import { readFile, writeFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const bump = process.argv[2];
const current = (await readFile(path.join(root, "VERSION"), "utf8")).trim();
const match = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.exec(current);

if (!["major", "minor", "patch"].includes(bump)) {
  throw new Error("Usage: node scripts/bump-version.mjs major|minor|patch");
}
if (!match) {
  throw new Error(
    `VERSION must be a SemVer 2.0.0 major.minor.patch core version; got ${current}`,
  );
}

let [major, minor, patch] = match.slice(1).map(Number);
if (bump === "major") {
  major += 1;
  minor = 0;
  patch = 0;
} else if (bump === "minor") {
  minor += 1;
  patch = 0;
} else {
  patch += 1;
}

const next = `${major}.${minor}.${patch}`;
const readme = await readFile(path.join(root, "README.md"), "utf8");
if (
  !/^\[version-badge\]: https:\/\/img\.shields\.io\/badge\/version-[0-9.]+-blue\.svg$/m.test(
    readme,
  )
) {
  throw new Error("README.md does not contain the expected version badge");
}

await writeFile(path.join(root, "VERSION"), `${next}\n`);
const sync = spawnSync(
  process.execPath,
  [path.join(root, "scripts/sync-version-metadata.mjs")],
  { cwd: root, stdio: "inherit" },
);
if (sync.status !== 0) {
  throw new Error(`Could not synchronize release metadata to ${next}`);
}

console.log(`Bumped application version ${current} -> ${next}`);
console.log("Update CHANGELOG.md, then run make check and make release-check.");
