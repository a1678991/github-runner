# Release pipeline (release-please + Arch binary packages) — Design

**Date:** 2026-06-11
**Status:** Approved (brainstorming phase)
**Companion to:** [2026-06-10-packaging-design.md](2026-06-10-packaging-design.md)

## What this covers

Automated releases driven by release-please, and a CI/CD pipeline that
attaches binary artifacts to each GitHub Release: a versioned Arch Linux
package (`.pkg.tar.zst`) and a plain Linux binary tarball. Conventional
commits are already enforced by commitlint, so version bumps and changelog
entries derive directly from history.

Decisions settled during brainstorming:

| Decision | Choice |
|---|---|
| Distribution | GitHub Release assets only (no AUR publish, no pacman repo — can be added later; the release PKGBUILD is AUR-ready) |
| Extra assets | Yes — `github-qemu-runner_<ver>_linux_amd64.tar.gz` alongside the Arch package |
| Build tool | `makepkg` + a versioned release PKGBUILD, reusing the proven archlinux-container/namcap pattern from `packaging.yml` (goreleaser/nfpm rejected: new tool, duplicates PKGBUILD install logic, bypasses namcap) |
| Workflow shape | Single workflow, asset jobs gated on `release_created` output (researched golden path — see below) |
| First version | `v0.1.0` via one-off `release-as`; `bump-minor-pre-major` for pre-1.0 semantics (feat → minor) |

## Research finding: the release-please golden path

A release created by `googleapis/release-please-action` with the default
`GITHUB_TOKEN` does **not** trigger separate workflows listening on
`release: published` (GitHub's recursive-workflow prevention). The
documented pattern is therefore a **single workflow**: the release-please
job runs on every push to main, and asset-building jobs run in the same
workflow gated on `outputs.release_created`, uploading with
`gh release upload ${{ outputs.tag_name }}`.

Keeping a version string inside an arbitrary file (the release PKGBUILD's
`pkgver=` line) in sync uses the manifest-config style with an
`extra-files` generic updater and an inline `# x-release-please-version`
annotation.

## release-please configuration

### `release-please-config.json`

- Single package `"."`, `release-type: go` (updates `CHANGELOG.md`, tags
  `vX.Y.Z`; nothing module-specific to break)
- `bump-minor-pre-major: true` — pre-1.0, breaking changes bump minor;
  `feat` still bumps minor (default; `bump-patch-for-minor-pre-major` not
  set)
- `release-as: "0.1.0"` for the first release; **remove after the first
  release PR merges** (documented one-off override)
- `extra-files`: `[{"type": "generic", "path": "packaging/arch/release/PKGBUILD"}]`

### `.release-please-manifest.json`

Starts as `{}`; release-please maintains it thereafter.

### `CHANGELOG.md`

Generated and maintained by release-please at the repo root.

## New workflow: `.github/workflows/release.yml`

`on: push` to `main`. `permissions: contents: write, pull-requests: write`
(needed to open release PRs and create releases/tags). All actions
SHA-pinned, `persist-credentials: false` on checkouts, consistent with the
existing workflows (zizmor-checked).

### Job `release-please`

`googleapis/release-please-action@v4` (SHA-pinned). Exposes
`release_created` and `tag_name` as job outputs. No checkout needed.

### Job `arch-package` — `if: release_created`

Runs in `archlinux:base-devel` container (same as `packaging.yml`):

1. Install `git go qemu-base cdrtools namcap github-cli`
2. Checkout `ref: tag_name`
3. Non-root builder user; `cd packaging/arch/release`
4. `git archive` the checked-out tag into
   `github-qemu-runner-<ver>.tar.gz` next to the PKGBUILD — makepkg uses
   a pre-existing source file as-is, `sha256sums` stays `SKIP` (see
   amendment below); a guard asserts the PKGBUILD `pkgver` matches the
   released tag
5. `makepkg --noconfirm`
6. namcap gate (fail on ` E: ` only) + install smoke test (`pacman -U`,
   exit-code check, sysusers/tmpfiles presence, ExecStart path) — same
   checks as the existing `-git` job
7. `gh release upload <tag> *.pkg.tar.zst *.pkg.tar.zst.sha256 --clobber`
   (sha256 sibling generated with `sha256sum`)

> **Amendment (2026-06-11, after first release):** the original design had
> this job run `updpkgsums` against the PKGBUILD's public archive URL. The
> repo is private, so that URL returns 404 to the anonymous curl
> makepkg/updpkgsums use — the v0.1.0 run failed exactly there. The job now
> provides the source tarball from its own authenticated checkout of the
> tag (`git archive`, same local-source trick as the PR-time job). The
> PKGBUILD itself is unchanged and its source URL becomes valid for end
> users if/when the repo goes public.

### Job `tarball` — `if: release_created`

`ubuntu-latest`, mise-action for the Go toolchain:

1. Checkout `ref: tag_name`
2. `CGO_ENABLED=0 go build -trimpath ./cmd/github-qemu-runner`
3. Smoke test the binary (`nonsense` subcommand → exit 2)
4. Pack `github-qemu-runner_<ver>_linux_amd64.tar.gz` containing: the
   binary, `LICENSE`, `README.md`, `packaging/config.example.yaml`,
   `packaging/github-qemu-runner.service`
5. Generate `.sha256` sibling; `gh release upload ... --clobber`

`--clobber` makes re-running a failed asset job idempotent: the release
and tag survive a job failure; fix and re-run uploads the assets without
redoing the release.

## New `packaging/arch/release/PKGBUILD`

Versioned `github-qemu-runner` package; the existing `-git` PKGBUILD stays
untouched (it already declares `provides`/`conflicts` against this name).

- `pkgname=github-qemu-runner`
- `pkgver=0.1.0 # x-release-please-version` — bumped by release-please in
  each release PR
- `source=("$pkgname-$pkgver.tar.gz::$url/archive/v$pkgver.tar.gz")`
- `sha256sums=('SKIP')` committed in-repo; `updpkgsums` fills the real
  checksum at release-build time (and for any future AUR publish)
- No `pkgver()` function, no `git` in `makedepends`; otherwise identical
  `build()`/`check()`/`package()` to the `-git` PKGBUILD (PIE/trimpath Go
  flags, systemd unit with `/usr/bin` ExecStart, sysusers, tmpfiles,
  config example, license, README)

## PR-time validation (extend `packaging.yml`)

The release PKGBUILD would otherwise only be exercised at release time. Add
a job to `packaging.yml` that builds it on every push/PR:

- `git archive` the checkout into a local tarball
- `sed` the PKGBUILD `source=` to point at that file
- `makepkg` + the same namcap/install smoke test

This mirrors the local-source trick the `-git` job already uses, so a
broken release PKGBUILD is caught before a release is cut.

## Housekeeping

Add stray local `makepkg` artifacts to `.gitignore`:
`packaging/arch/pkg/`, `packaging/arch/src/`, `packaging/arch/*.pkg.tar.zst`,
`packaging/arch/github-qemu-runner-git/` (and equivalents under
`packaging/arch/release/`).

## Error handling

- Asset job failure → release/tag exist without assets; re-run the failed
  job (idempotent via `--clobber`).
- `updpkgsums` failure (tarball not yet available) → job fails visibly;
  re-run. No retry loop — GitHub archive URLs are available immediately
  after tag creation in practice.
- release-please PR conflicts → release-please force-updates its own
  branch on the next push to main; no manual intervention.

## Testing

- PR-time: new `packaging.yml` job builds + smoke-tests the release
  PKGBUILD from local source; existing jobs unchanged.
- Release-time: namcap gate + `pacman -U` install smoke test before
  upload; tarball job verifies the binary's exit-code contract
  (`nonsense` → exit 2) before packing.
- First release validates the end-to-end pipeline at `v0.1.0`.
