# Versioning and releases

## Version sources

- Release versions use the SemVer 2.0.0 `MAJOR.MINOR.PATCH` core format;
  Git tags use the conventional `v` prefix. The checker rejects leading zeroes.
- The current tooling intentionally does not accept SemVer pre-release
  identifiers (`-alpha.1`) or build metadata (`+build.1`). The planned `0.1.0`
  alpha is a valid SemVer version; mark the GitHub Release as a pre-release and
  include the alpha warning in its notes.
- SemVer defines `0.y.z` as initial development: the public API is not stable
  and may change. `0.1.0` does not imply production readiness.
- `VERSION` is the single source of truth for the CLI and Docker image version.
- `internal/version.Version` defaults to `dev`; Make and Docker inject `VERSION`
  at build time. `KBM-flow --version` and `KBM-flow version` print it.
- `package.json` is a private frontend-tool manifest (`private: true`), not an
  independently published package. Its version and the lockfile root version
  are synchronized from `VERSION`; `make check` fails if they drift. The
  manifest also records the project author and AGPL license.

## Changelog

Keep new work under `## [Unreleased]` in `CHANGELOG.md`. When preparing a
release:

1. For a later patch/minor/major release, run
   `make version-bump TYPE=patch|minor|major`. It increments `VERSION` and
   synchronizes `package.json`, `package-lock.json`, and the README badge.
   The initial target is already `0.1.0`.
2. Move the included changes from `Unreleased` to a dated
   `## [X.Y.Z] - YYYY-MM-DD` section; leave a fresh `Unreleased` heading above it.
3. Run `make check`, `make frontend-check`, and
   `make release-check RELEASE_TAG=vX.Y.Z`.
4. Build and inspect `./KBM-flow --version`; verify the Docker image reports the
   same version.
5. Commit and push the release tree, then create and push the annotated `vX.Y.Z`
   tag on that commit.

Two workflows run on `v*` tag pushes. `release-check` rejects a tag unless
its version matches `VERSION` and a dated changelog section exists — it is
the version-consistency gate. `release` cross-compiles the four
darwin/linux amd64+arm64 binaries (same ldflags version stamp as `make
build`), extracts the tag's changelog section for notes, and creates the
GitHub release with binaries attached — or `--clobber`s onto an existing
release on re-runs. Add GoReleaser or an image-publishing workflow only
when you decide to distribute release artifacts.

The initial `VERSION` is `0.1.0`; the repository currently has no release tags.
When it is ready, publish the first GitHub Release as a pre-release and begin
the release notes with **ALPHA — NOT FOR PRODUCTION**. The project is still
under local testing; do not publish a GHCR image until that testing is complete.
If container distribution is later wanted, publish to GHCR from a validated
release tag (for example, `ghcr.io/gundestrup/knowledge-ba46-flow:v0.1.0`).
The current CI only builds and smoke-tests Docker images; it does not publish.
