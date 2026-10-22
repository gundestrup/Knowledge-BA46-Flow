import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const version = (await readFile(path.join(root, "VERSION"), "utf8")).trim();

if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(version)) {
  throw new Error(
    `VERSION must be a SemVer 2.0.0 major.minor.patch core version; got ${version}`,
  );
}

const packagePath = path.join(root, "package.json");
const lockPath = path.join(root, "package-lock.json");
const readmePath = path.join(root, "README.md");
const packageMetadata = JSON.parse(await readFile(packagePath, "utf8"));
const lockMetadata = JSON.parse(await readFile(lockPath, "utf8"));
const readme = await readFile(readmePath, "utf8");
const badgePattern =
  /^\[version-badge\]: https:\/\/img\.shields\.io\/badge\/version-[0-9.]+-blue\.svg$/m;

if (!badgePattern.test(readme)) {
  throw new Error("README.md does not contain the expected version badge");
}

packageMetadata.version = version;
lockMetadata.version = version;
if (lockMetadata.packages?.[""]) {
  lockMetadata.packages[""].version = version;
}
const updatedReadme = readme.replace(
  badgePattern,
  `[version-badge]: https://img.shields.io/badge/version-${version}-blue.svg`,
);

await writeFile(packagePath, `${JSON.stringify(packageMetadata, null, 2)}\n`);
await writeFile(lockPath, `${JSON.stringify(lockMetadata, null, 2)}\n`);
await writeFile(readmePath, updatedReadme);
console.log(
  `Synchronized package.json, package-lock.json, and README.md to ${version}`,
);
