// Drift guard for the i18n catalog: every t('ui.*') call in the embedded
// frontend and every i18n.T("cli.*") call in the CLI must resolve against
// locales/active.en.yaml, and the catalog must not carry dead keys.
// Runs inside `npm run check:js`.
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";

const catalogPath = "internal/i18n/locales/active.en.yaml";
const localeDir = "internal/i18n/locales";
const uiFiles = [
  "internal/server/static/knowledge.html",
  "internal/server/static/common.js",
  "internal/server/static/dashboard.js",
  "internal/server/static/index.html",
  "internal/server/static/landing.js",
  "internal/server/static/report.html",
  "internal/server/static/report.js",
];
const cliFile = "cmd/KBM-flow/main.go";

// Top-level keys of a flat catalog (nested one:/other: plural maps count
// under their parent id — indented lines are skipped). Value entries are
// "str" or "plural" so secondary catalogs must match the English shape.
const loadCatalog = (path) => {
  const keys = new Map();
  let cur = null;
  for (const line of readFileSync(path, "utf8").split("\n")) {
    if (!line.trim() || line.startsWith("#")) continue;
    const top = /^(\S[^:]*):\s*(.*)$/.exec(line); // NOSONAR — linear match on short key lines
    if (top) {
      cur = top[1];
      keys.set(cur, top[2].trim() ? "str" : "pending");
      continue;
    }
    if (/^\s+(one|other|zero|two|few|many):/.test(line) && cur) {
      keys.set(cur, "plural");
    }
  }
  for (const [k, v] of keys) if (v === "pending") keys.set(k, "str");
  return keys;
};

const catalog = loadCatalog(catalogPath);
const catalogKeys = new Set(catalog.keys());

const callRe = /\bt\(\s*(['"])([^'"]+)\1\s*,|i18n\.T\(\s*"([^"]+)"/g;
const used = new Map(); // id -> file
const scan = (file, prefix) => {
  const src = readFileSync(file, "utf8");
  for (const m of src.matchAll(callRe)) {
    const id = m[2] ?? m[3];
    if (id.startsWith(prefix)) {
      if (!used.has(id)) used.set(id, file);
    }
  }
};
for (const f of uiFiles) scan(f, "ui.");
scan(cliFile, "cli.");

const missing = [...used].filter(([id]) => !catalogKeys.has(id));
assert.deepEqual(
  missing,
  [],
  `t()/i18n.T ids missing from ${catalogPath}:\n${missing.map(([id, f]) => `  ${id} (${f})`).join("\n")}`,
);

const dead = [...catalogKeys]
  .filter((k) => k.startsWith("ui.") || k.startsWith("cli."))
  .filter((k) => !used.has(k));
assert.deepEqual(
  dead,
  [],
  `catalog keys with no call site (stale strings):\n${dead.map((k) => `  ${k}`).join("\n")}`,
);

// Secondary catalogs (active.<tag>.yaml): keys must exist in English (a
// typo'd key silently renders English), and plural/string shapes must match
// so the UI's t() picks the right branch. Missing keys are allowed — they
// fall back to English — but are reported for the translator.
for (const file of readdirSync(localeDir)) {
  const m = /^active\.([a-z-]+)\.yaml$/i.exec(file);
  if (!m || m[1] === "en") continue;
  const other = loadCatalog(`${localeDir}/${file}`);
  const unknown = [...other.keys()].filter((k) => !catalog.has(k));
  assert.deepEqual(
    unknown,
    [],
    `${file}: keys with no English counterpart (typos render English):\n${unknown.map((k) => `  ${k}`).join("\n")}`,
  );
  const mismatched = [...other.keys()].filter(
    (k) => catalog.has(k) && catalog.get(k) !== other.get(k),
  );
  assert.deepEqual(
    mismatched,
    [],
    `${file}: plural/string shape differs from English:\n${mismatched.map((k) => `  ${k} (${catalog.get(k)} -> ${other.get(k)})`).join("\n")}`,
  );
  const missing = [...catalog.keys()].filter((k) => !other.has(k));
  console.log(
    `i18n check: ${file} — ${other.size}/${catalog.size} keys translated` +
      (missing.length ? `, ${missing.length} missing (English fallback)` : ""),
  );
}

console.log(
  `i18n check: ${used.size} ids resolve against ${catalogKeys.size} catalog keys`,
);
