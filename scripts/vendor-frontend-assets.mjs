import { copyFile, mkdir, readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const project = JSON.parse(
  await readFile(path.join(root, "package.json"), "utf8"),
);
const notices = await readFile(
  path.join(root, "THIRD-PARTY-NOTICES.md"),
  "utf8",
);
const readme = await readFile(path.join(root, "README.md"), "utf8");
const checkOnly = process.argv.includes("--check");
const assets = [
  {
    packageName: "bulma",
    noticeName: "Bulma",
    source: "css/bulma.min.css",
    destination: "internal/server/static/vendor/bulma.min.css",
  },
  {
    packageName: "@alpinejs/csp",
    noticeName: "Alpine.js",
    source: "dist/cdn.min.js",
    destination: "internal/server/static/vendor/alpine.min.js",
  },
  {
    packageName: "chart.js",
    noticeName: "Chart.js",
    source: "dist/chart.umd.js",
    destination: "internal/server/static/vendor/chart.umd.js",
  },
];

for (const asset of assets) {
  const packageDirectory = path.join(root, "node_modules", asset.packageName);
  const packageInfo = JSON.parse(
    await readFile(path.join(packageDirectory, "package.json"), "utf8"),
  );
  const pinnedVersion = project.devDependencies[asset.packageName];

  if (packageInfo.version !== pinnedVersion) {
    throw new Error(
      `${asset.packageName}: installed ${packageInfo.version}, pinned ${pinnedVersion}`,
    );
  }
  if (!notices.includes(`${asset.noticeName} v${pinnedVersion}`)) {
    throw new Error(
      `THIRD-PARTY-NOTICES.md must record ${asset.noticeName} v${pinnedVersion}`,
    );
  }
  if (!readme.includes(`${asset.noticeName} v${pinnedVersion}`)) {
    throw new Error(
      `README.md must record ${asset.noticeName} v${pinnedVersion}`,
    );
  }

  const source = await readFile(path.join(packageDirectory, asset.source));
  const destination = path.join(root, asset.destination);

  if (checkOnly) {
    const vendored = await readFile(destination);
    if (!source.equals(vendored)) {
      throw new Error(
        `${asset.destination} differs from ${asset.packageName}@${pinnedVersion}; run npm run assets:vendor`,
      );
    }
    console.log(
      `${asset.packageName}@${pinnedVersion}: vendored asset is current`,
    );
    continue;
  }

  await mkdir(path.dirname(destination), { recursive: true });
  await copyFile(path.join(packageDirectory, asset.source), destination);
  console.log(
    `${asset.packageName}@${pinnedVersion}: copied ${asset.destination}`,
  );
}
