#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

version=$(tr -d '\r\n' < VERSION)
if ! printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  echo "VERSION must contain a SemVer 2.0.0 major.minor.patch core version; got: $version" >&2
  exit 1
fi

package_version=$(sed -n 's/^  "version": "\([^"]*\)".*/\1/p' package.json | head -n 1)
lock_version=$(sed -n 's/^  "version": "\([^"]*\)".*/\1/p' package-lock.json | head -n 1)
lock_package_version=$(sed -n '/^    "": {/,/^    },/s/^      "version": "\([^"]*\)".*/\1/p' package-lock.json | head -n 1)
for metadata_version in "$package_version" "$lock_version" "$lock_package_version"; do
  if [ "$metadata_version" != "$version" ]; then
    echo "package.json/package-lock.json version metadata must match VERSION ($version); run make version-sync" >&2
    exit 1
  fi
done

if ! grep -Fxq '## [Unreleased]' CHANGELOG.md; then
  echo "CHANGELOG.md must contain an ## [Unreleased] section" >&2
  exit 1
fi

if ! grep -Fq "version-${version}-blue.svg" README.md; then
  echo "README.md version badge does not match VERSION ($version)" >&2
  exit 1
fi

tag=${1:-}
if [ -z "$tag" ] && command -v git >/dev/null 2>&1; then
  tag=$(git describe --exact-match --tags HEAD 2>/dev/null || true)
fi
tag=${tag#refs/tags/}
if [ -z "$tag" ]; then
  echo "Version metadata is consistent: $version (no release tag on HEAD)"
  exit 0
fi

if [ "$tag" != "v$version" ]; then
  echo "Release tag $tag does not match VERSION v$version" >&2
  exit 1
fi

if ! grep -Eq "^## \[$version\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$" CHANGELOG.md; then
  echo "CHANGELOG.md needs a dated ## [$version] - YYYY-MM-DD release heading" >&2
  exit 1
fi

echo "Release tag, VERSION, README badge, and changelog agree: $tag"
