#!/bin/sh
# Tests for the version/release scripts. They run against a throwaway copy of
# the release metadata (VERSION, package.json, package-lock.json, README.md,
# CHANGELOG.md) so the real files are never touched. Requires node on PATH.
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/scripts"
cp "$repo/scripts/bump-version.mjs" \
  "$repo/scripts/sync-version-metadata.mjs" \
  "$repo/scripts/check-version.sh" \
  "$tmp/scripts/"
for f in VERSION package.json package-lock.json README.md CHANGELOG.md; do
  cp "$repo/$f" "$tmp/$f"
done

version() { tr -d '\r\n' <"$tmp/VERSION"; }
fail() { echo "FAIL: $1" >&2; exit 1; }

# Assert that VERSION, package.json, both package-lock.json version fields, the
# README badge and check-version.sh all agree on $1.
assert_version() {
  [ "$(version)" = "$1" ] || fail "VERSION is $(version), expected $1"
  grep -q "\"version\": \"$1\"" "$tmp/package.json" ||
    fail "package.json not synced to $1"
  [ "$(grep -c "\"version\": \"$1\"" "$tmp/package-lock.json")" -ge 2 ] ||
    fail "package-lock.json not synced to $1"
  grep -q "version-$1-blue.svg" "$tmp/README.md" ||
    fail "README badge not synced to $1"
  sh "$tmp/scripts/check-version.sh" >/dev/null ||
    fail "check-version.sh rejected consistent metadata for $1"
}

# A deterministic starting point; sync propagates it to every metadata file.
printf '1.2.3\n' >"$tmp/VERSION"
node "$tmp/scripts/sync-version-metadata.mjs" >/dev/null
assert_version 1.2.3
echo "ok - version-sync propagates VERSION to package metadata and README badge"

# SemVer bumps: patch increments patch, minor resets patch, major resets both.
node "$tmp/scripts/bump-version.mjs" patch >/dev/null && assert_version 1.2.4
node "$tmp/scripts/bump-version.mjs" minor >/dev/null && assert_version 1.3.0
node "$tmp/scripts/bump-version.mjs" major >/dev/null && assert_version 2.0.0
echo "ok - patch/minor/major bumps follow SemVer 2.0 and stay synchronized"

# Unknown bump types are rejected.
if node "$tmp/scripts/bump-version.mjs" bogus >/dev/null 2>&1; then
  fail "bump-version.mjs accepted an unknown bump type"
fi
echo "ok - bump-version.mjs rejects unknown bump types"

# Drifted metadata must fail until version-sync repairs it.
mv "$tmp/package.json" "$tmp/package.json.orig"
sed 's/"version": "2.0.0"/"version": "9.9.9"/' "$tmp/package.json.orig" \
  >"$tmp/package.json"
rm "$tmp/package.json.orig"
if sh "$tmp/scripts/check-version.sh" >/dev/null 2>&1; then
  fail "check-version.sh accepted a drifted package.json"
fi
node "$tmp/scripts/sync-version-metadata.mjs" >/dev/null
sh "$tmp/scripts/check-version.sh" >/dev/null ||
  fail "check-version.sh still failing after version-sync"
echo "ok - metadata drift is detected and repaired by version-sync"

# A matching release tag requires a dated changelog heading.
if sh "$tmp/scripts/check-version.sh" v2.0.0 >/dev/null 2>&1; then
  fail "release check passed without a dated changelog heading"
fi
printf '\n## [2.0.0] - 2026-01-15\n\n- Example release notes.\n' >>"$tmp/CHANGELOG.md"
sh "$tmp/scripts/check-version.sh" v2.0.0 >/dev/null ||
  fail "release check rejected a prepared release"
if sh "$tmp/scripts/check-version.sh" v2.0.1 >/dev/null 2>&1; then
  fail "release check accepted a tag that does not match VERSION"
fi
echo "ok - release tags must match VERSION and a dated changelog heading"

# The ## [Unreleased] heading is required on every check.
mv "$tmp/CHANGELOG.md" "$tmp/CHANGELOG.md.orig"
grep -v '^## \[Unreleased\]$' "$tmp/CHANGELOG.md.orig" >"$tmp/CHANGELOG.md"
if sh "$tmp/scripts/check-version.sh" >/dev/null 2>&1; then
  fail "check-version.sh passed without an [Unreleased] section"
fi
mv "$tmp/CHANGELOG.md.orig" "$tmp/CHANGELOG.md"
echo "ok - check-version.sh requires an [Unreleased] section"

# Malformed versions — including leading zeroes and pre-release suffixes our
# strict major.minor.patch policy rejects — must fail both scripts. (Trailing
# whitespace is trimmed, so a normal trailing newline is fine.)
for bad in "1.02.3" "1.2" "v1.2.3" "1.2.3-alpha.1"; do
  printf '%s\n' "$bad" >"$tmp/VERSION"
  if sh "$tmp/scripts/check-version.sh" >/dev/null 2>&1; then
    fail "check-version.sh accepted invalid VERSION '$bad'"
  fi
  if node "$tmp/scripts/bump-version.mjs" patch >/dev/null 2>&1; then
    fail "bump-version.mjs accepted invalid VERSION '$bad'"
  fi
done
echo "ok - malformed VERSION values are rejected by both scripts"

echo "All version script tests passed."
