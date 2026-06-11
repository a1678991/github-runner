# Release Pipeline (release-please + Arch binary packages) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automated releases via release-please, with a versioned Arch package (`.pkg.tar.zst`) and a Linux binary tarball attached to every GitHub Release.

**Architecture:** Single `release.yml` workflow on push to main: a release-please job creates release PRs / tags / releases; asset jobs gated on its `release_created` output build artifacts and `gh release upload` them (golden path — `GITHUB_TOKEN`-created releases cannot trigger `release:`-event workflows). A new versioned `packaging/arch/release/PKGBUILD` builds from the tag tarball, its `pkgver=` kept in sync by release-please's generic `extra-files` updater. A PR-time job in `packaging.yml` builds that PKGBUILD from local source so it cannot rot.

**Tech Stack:** GitHub Actions, `googleapis/release-please-action@v4.4.1`, makepkg/namcap in `archlinux:base-devel` container, Go 1.26 (mise), `gh` CLI.

**Spec:** `docs/superpowers/specs/2026-06-11-release-pipeline-design.md`

**Conventions (match existing repo):** all actions SHA-pinned with `# vX.Y.Z` comment, `persist-credentials: false` on every checkout, top-level `permissions: contents: read` with per-job escalation, conventional commits (commitlint enforced by lefthook).

---

### Task 0: Branch

- [ ] **Step 1: Create the feature branch**

```bash
git switch -c feat/release-pipeline
```

(If executing via a worktree skill, the worktree replaces this step; the branch name stays `feat/release-pipeline`.)

---

### Task 1: Ignore stray makepkg artifacts

The working tree contains untracked local makepkg build leftovers (`packaging/arch/pkg/`, `packaging/arch/src/`, `packaging/arch/github-qemu-runner-git/`, `packaging/arch/*.pkg.tar.zst`). They must never be committed, including the equivalents the new `release/` dir will produce.

**Files:**
- Modify: `.gitignore`

- [ ] **Step 1: Append makepkg patterns to `.gitignore`**

Append this block to the end of `.gitignore`:

```gitignore

# makepkg artifacts (local builds)
packaging/arch/pkg/
packaging/arch/src/
packaging/arch/github-qemu-runner-git/
packaging/arch/*.pkg.tar.zst
packaging/arch/release/pkg/
packaging/arch/release/src/
packaging/arch/release/*.pkg.tar.zst
packaging/arch/release/*.tar.gz
```

- [ ] **Step 2: Verify the artifacts are now ignored**

Run: `git status --short packaging/arch/`
Expected: no output (everything under `packaging/arch/` is either tracked-and-clean or ignored).

Run: `git check-ignore packaging/arch/pkg packaging/arch/src packaging/arch/github-qemu-runner-git`
Expected: prints all three paths (exit 0).

- [ ] **Step 3: Commit**

```bash
git add .gitignore
git commit -m 'chore: ignore local makepkg build artifacts'
```

---

### Task 2: Versioned release PKGBUILD

A non-git `github-qemu-runner` package built from the tagged GitHub tarball. Mirrors `packaging/arch/PKGBUILD` (the `-git` one, which stays untouched) except: static `pkgver` (release-please-managed), no `pkgver()`, no `git` makedep, no `provides`/`conflicts` (this IS the canonical name), and `cd "$pkgname-$pkgver"` (tarball extracts to `github-qemu-runner-<ver>/`).

`sha256sums=('SKIP')` stays committed; `updpkgsums` fills the real checksum at release-build time (GitHub archive checksums are not guaranteed stable forever, so we never commit them).

**Files:**
- Create: `packaging/arch/release/PKGBUILD`

- [ ] **Step 1: Write `packaging/arch/release/PKGBUILD`**

```bash
# Maintainer: a1678991
pkgname=github-qemu-runner
pkgver=0.1.0 # x-release-please-version
pkgrel=1
pkgdesc='Ephemeral GitHub Actions self-hosted runners in disposable QEMU/KVM VMs'
arch=('x86_64')
url='https://github.com/a1678991/github-qemu-runner'
license=('MIT')
depends=('qemu-base' 'cdrtools')
makedepends=('go')
# Go binaries gain nothing from Arch's split debug packages (broken
# .build-id symlinks with -trimpath) or LTO flags.
options=('!debug' '!lto')
source=("$pkgname-$pkgver.tar.gz::$url/archive/v$pkgver.tar.gz")
# Filled by updpkgsums in the release workflow; GitHub archive checksums
# are not guaranteed stable, so the real sum is never committed.
sha256sums=('SKIP')

build() {
  cd "$pkgname-$pkgver"
  export GOPATH="$srcdir/gopath"
  export CGO_CPPFLAGS="${CPPFLAGS}"
  export CGO_CFLAGS="${CFLAGS}"
  export CGO_CXXFLAGS="${CXXFLAGS}"
  export CGO_LDFLAGS="${LDFLAGS}"
  export GOFLAGS="-buildmode=pie -trimpath -mod=readonly -modcacherw"
  go build -o github-qemu-runner ./cmd/github-qemu-runner
}

check() {
  cd "$pkgname-$pkgver"
  export GOPATH="$srcdir/gopath"
  export GOFLAGS="-mod=readonly -modcacherw"
  go test ./...
}

package() {
  cd "$pkgname-$pkgver"
  install -Dm755 github-qemu-runner "$pkgdir/usr/bin/github-qemu-runner"
  install -Dm644 packaging/github-qemu-runner.service \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner.service"
  # The shared unit targets the manual-install path (/usr/local/bin); the
  # packaged binary lives in /usr/bin.
  sed -i 's|/usr/local/bin/github-qemu-runner|/usr/bin/github-qemu-runner|' \
    "$pkgdir/usr/lib/systemd/system/github-qemu-runner.service"
  install -Dm644 packaging/config.example.yaml \
    "$pkgdir/etc/github-qemu-runner/config.example.yaml"
  install -Dm644 packaging/arch/github-qemu-runner.sysusers \
    "$pkgdir/usr/lib/sysusers.d/github-qemu-runner.conf"
  install -Dm644 packaging/arch/github-qemu-runner.tmpfiles \
    "$pkgdir/usr/lib/tmpfiles.d/github-qemu-runner.conf"
  install -Dm644 LICENSE "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
  install -Dm644 README.md "$pkgdir/usr/share/doc/$pkgname/README.md"
}
```

The ` # x-release-please-version` trailing comment on the `pkgver=` line is the release-please generic-updater annotation: each release PR rewrites the version on that line. It is valid bash (assignment ends at the space).

- [ ] **Step 2: Validate PKGBUILD syntax**

Run (host is Arch, makepkg is available; `--printsrcinfo` only sources the file, no deps needed):

```bash
cd packaging/arch/release && makepkg --printsrcinfo && cd -
```

Expected: `.SRCINFO` text output including `pkgver = 0.1.0` and `source = github-qemu-runner-0.1.0.tar.gz::https://github.com/a1678991/github-qemu-runner/archive/v0.1.0.tar.gz`. No errors.

- [ ] **Step 3: Commit**

```bash
git add packaging/arch/release/PKGBUILD
git commit -m 'feat: add versioned release PKGBUILD'
```

---

### Task 3: PR-time validation job for the release PKGBUILD

The release PKGBUILD is otherwise only exercised when a release is cut. This job builds it on every push/PR from a `git archive` tarball of the checkout (the local-source trick, like the existing `-git` job), then runs the same namcap/install gates. This is the test that keeps Task 2 honest.

**Files:**
- Modify: `.github/workflows/packaging.yml` (append a job after `arch-package`, before `nix`)

- [ ] **Step 1: Add the `arch-release-package` job to `packaging.yml`**

Insert between the `arch-package` and `nix` jobs (same indentation level):

```yaml
  arch-release-package:
    runs-on: ubuntu-latest
    container: archlinux:base-devel
    steps:
      # git must exist before checkout; base-devel does not include it
      - run: pacman -Syu --noconfirm git go qemu-base cdrtools namcap
      - uses: actions/checkout@93cb6efe18208431cddfb8368fd83d5badbf9bfd # v5.0.1
        with:
          persist-credentials: false
      - name: Prepare non-root builder copy
        run: |
          useradd -m builder
          git config --system --add safe.directory '*'
          ver=$(sed -n 's/^pkgver=\([0-9.]*\).*/\1/p' packaging/arch/release/PKGBUILD)
          cp -r packaging/arch/release /home/builder/build
          # The release PKGBUILD sources the tag tarball, which does not
          # exist for unreleased code; build from this checkout instead.
          git archive --format=tar.gz --prefix="github-qemu-runner-$ver/" \
            -o "/home/builder/build/github-qemu-runner-$ver.tar.gz" HEAD
          sed -i 's|::$url/archive/v$pkgver.tar.gz||' /home/builder/build/PKGBUILD
          chown -R builder:builder /home/builder/build
      - name: Build package
        run: su builder -c 'cd /home/builder/build && makepkg --noconfirm'
      - name: namcap (fail on errors only)
        run: |
          cd /home/builder/build
          namcap ./*.pkg.tar.zst | tee /tmp/namcap.out || true
          if grep ' E: ' /tmp/namcap.out; then
            echo '::error::namcap reported errors'
            exit 1
          fi
      - name: Install and smoke test
        run: |
          cd /home/builder/build
          pacman -U --noconfirm ./*.pkg.tar.zst
          rc=0; github-qemu-runner nonsense || rc=$?
          test "$rc" -eq 2
          pacman -Ql github-qemu-runner | grep sysusers.d
          pacman -Ql github-qemu-runner | grep tmpfiles.d
          execstart=$(grep '^ExecStart=' /usr/lib/systemd/system/github-qemu-runner.service | cut -d= -f2- | awk '{print $1}')
          test -x "$execstart"
```

How the source swap works: the `sed` deletes the `::$url/...` remote half of the source entry, leaving `source=("$pkgname-$pkgver.tar.gz")` — a plain local file that makepkg picks up from the build dir, where `git archive` just wrote it. `sha256sums=('SKIP')` is already committed, so no updpkgsums is needed here. Note the sed pattern is single-quoted so `$url`/`$pkgver` are literal text, not shell expansions.

- [ ] **Step 2: Lint the workflow**

```bash
actionlint && zizmor --offline .github/workflows/
```

Expected: both exit 0, no findings (actionlint runs shellcheck on the run blocks).

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/packaging.yml
git commit -m 'ci: build release PKGBUILD from local source on every PR'
```

---

### Task 4: release-please configuration

Manifest-config style (required for `extra-files`). `release-type: go` (CHANGELOG.md + `vX.Y.Z` tags), `bump-minor-pre-major` for pre-1.0 semantics, `release-as: 0.1.0` as the documented one-off to force the first version (removed in Task 8 after the first release).

**Files:**
- Create: `release-please-config.json`
- Create: `.release-please-manifest.json`

- [ ] **Step 1: Write `release-please-config.json`**

```json
{
  "$schema": "https://raw.githubusercontent.com/googleapis/release-please/main/schemas/config.json",
  "packages": {
    ".": {
      "release-type": "go",
      "bump-minor-pre-major": true,
      "release-as": "0.1.0",
      "extra-files": ["packaging/arch/release/PKGBUILD"]
    }
  }
}
```

(A plain string in `extra-files` selects the generic updater, which rewrites the version on lines annotated `x-release-please-version` — the PKGBUILD's `pkgver=` line from Task 2.)

- [ ] **Step 2: Write `.release-please-manifest.json`**

```json
{}
```

(Empty: no releases exist yet; release-please bootstraps from full history and maintains this file afterwards.)

- [ ] **Step 3: Validate JSON**

```bash
jq . release-please-config.json .release-please-manifest.json
```

Expected: both documents echoed back, exit 0.

- [ ] **Step 4: Commit**

```bash
git add release-please-config.json .release-please-manifest.json
git commit -m 'chore: add release-please manifest configuration'
```

---

### Task 5: Release workflow

Single workflow, golden path: release-please on push to main; `arch-package` and `tarball` jobs gated on `release_created`, uploading assets with `gh release upload --clobber` (idempotent re-runs). A release created with `GITHUB_TOKEN` cannot trigger a separate `release:`-event workflow, hence same-workflow chaining.

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] **Step 1: Write `.github/workflows/release.yml`**

```yaml
name: Release

on:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  release-please:
    runs-on: ubuntu-latest
    permissions:
      contents: write # create tags and releases
      pull-requests: write # open/update release PRs
    outputs:
      release_created: ${{ steps.release.outputs.release_created }}
      tag_name: ${{ steps.release.outputs.tag_name }}
    steps:
      - uses: googleapis/release-please-action@5c625bfb5d1ff62eadeeb3772007f7f66fdcf071 # v4.4.1
        id: release

  arch-package:
    needs: release-please
    if: ${{ needs.release-please.outputs.release_created }}
    runs-on: ubuntu-latest
    permissions:
      contents: write # gh release upload
    container: archlinux:base-devel
    steps:
      # git must exist before checkout; base-devel does not include it.
      # pacman-contrib provides updpkgsums, github-cli provides gh.
      - run: pacman -Syu --noconfirm git go qemu-base cdrtools namcap pacman-contrib github-cli
      - uses: actions/checkout@93cb6efe18208431cddfb8368fd83d5badbf9bfd # v5.0.1
        with:
          ref: ${{ needs.release-please.outputs.tag_name }}
          persist-credentials: false
      - name: Prepare non-root builder copy
        run: |
          useradd -m builder
          cp -r packaging/arch/release /home/builder/build
          chown -R builder:builder /home/builder/build
      - name: Build package
        # updpkgsums fetches the tag tarball (created by the release-please
        # job moments ago) and fills sha256sums before makepkg verifies it.
        run: su builder -c 'cd /home/builder/build && updpkgsums && makepkg --noconfirm'
      - name: namcap (fail on errors only)
        run: |
          cd /home/builder/build
          namcap ./*.pkg.tar.zst | tee /tmp/namcap.out || true
          if grep ' E: ' /tmp/namcap.out; then
            echo '::error::namcap reported errors'
            exit 1
          fi
      - name: Install and smoke test
        run: |
          cd /home/builder/build
          pacman -U --noconfirm ./*.pkg.tar.zst
          rc=0; github-qemu-runner nonsense || rc=$?
          test "$rc" -eq 2
          pacman -Ql github-qemu-runner | grep sysusers.d
          pacman -Ql github-qemu-runner | grep tmpfiles.d
          execstart=$(grep '^ExecStart=' /usr/lib/systemd/system/github-qemu-runner.service | cut -d= -f2- | awk '{print $1}')
          test -x "$execstart"
      - name: Upload to release
        env:
          GH_TOKEN: ${{ github.token }}
          GH_REPO: ${{ github.repository }}
          TAG: ${{ needs.release-please.outputs.tag_name }}
        run: |
          cd /home/builder/build
          for f in *.pkg.tar.zst; do sha256sum "$f" > "$f.sha256"; done
          gh release upload "$TAG" ./*.pkg.tar.zst ./*.pkg.tar.zst.sha256 --clobber

  tarball:
    needs: release-please
    if: ${{ needs.release-please.outputs.release_created }}
    runs-on: ubuntu-latest
    permissions:
      contents: write # gh release upload
    steps:
      - uses: actions/checkout@93cb6efe18208431cddfb8368fd83d5badbf9bfd # v5.0.1
        with:
          ref: ${{ needs.release-please.outputs.tag_name }}
          persist-credentials: false
      - uses: jdx/mise-action@c37c93293d6b742fc901e1406b8f764f6fb19dac # v2.4.4
      - name: Build
        run: CGO_ENABLED=0 go build -trimpath -o github-qemu-runner ./cmd/github-qemu-runner
      - name: Smoke test
        run: |
          rc=0; ./github-qemu-runner nonsense || rc=$?
          test "$rc" -eq 2
      - name: Pack
        env:
          TAG: ${{ needs.release-please.outputs.tag_name }}
        run: |
          dir="github-qemu-runner_${TAG#v}_linux_amd64"
          mkdir "$dir"
          cp github-qemu-runner LICENSE README.md \
            packaging/config.example.yaml packaging/github-qemu-runner.service "$dir/"
          tar -czf "$dir.tar.gz" "$dir"
          sha256sum "$dir.tar.gz" > "$dir.tar.gz.sha256"
      - name: Upload to release
        env:
          GH_TOKEN: ${{ github.token }}
          TAG: ${{ needs.release-please.outputs.tag_name }}
        run: gh release upload "$TAG" ./*.tar.gz ./*.tar.gz.sha256 --clobber
```

Notes for the implementer:
- `tag_name`/`github.token` only ever reach shell via `env:` — never interpolate `${{ }}` directly into `run:` blocks (zizmor template-injection rule).
- `release_created` is the string `"true"` or empty, so the bare `if:` truthiness check is correct.
- The checkout `ref:` pins asset builds to the exact tag even if main has moved on.

- [ ] **Step 2: Lint the workflow**

```bash
actionlint && zizmor --offline .github/workflows/ && pinact run --check
```

Expected: all exit 0. If `pinact run --check` rewrites or complains about the release-please pin, verify the SHA matches v4.4.1: `gh api repos/googleapis/release-please-action/tags --jq '.[] | select(.name=="v4.4.1") | .commit.sha'` → `5c625bfb5d1ff62eadeeb3772007f7f66fdcf071`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m 'ci: add release workflow with Arch package and tarball assets'
```

---

### Task 6: Allow GitHub Actions to create pull requests

release-please opens its release PR with `GITHUB_TOKEN`; the repo setting "Allow GitHub Actions to create and approve pull requests" must be on, or the job fails with `GitHub Actions is not permitted to create or approve pull requests`.

- [ ] **Step 1: Enable the setting**

```bash
gh api -X PUT repos/a1678991/github-qemu-runner/actions/permissions/workflow \
  -f default_workflow_permissions=read \
  -F can_approve_pull_request_reviews=true
```

- [ ] **Step 2: Verify**

```bash
gh api repos/a1678991/github-qemu-runner/actions/permissions/workflow \
  --jq '.can_approve_pull_request_reviews'
```

Expected: `true`. (`default_workflow_permissions=read` is correct — the workflows declare their own write scopes per job.)

---

### Task 7: Push, PR, and CI verification

- [ ] **Step 1: Push the branch and open a PR**

```bash
git push -u origin feat/release-pipeline
gh pr create \
  --title 'feat: release-please pipeline with Arch package and tarball assets' \
  --body "$(cat <<'EOF'
Implements docs/superpowers/specs/2026-06-11-release-pipeline-design.md:

- release-please (manifest config, release-type go, first release v0.1.0)
- release.yml: single workflow; on release_created builds + uploads a
  versioned Arch package (makepkg/namcap, archlinux container) and a
  linux_amd64 binary tarball, each with a .sha256 sibling
- packaging/arch/release/PKGBUILD: versioned package from the tag tarball,
  pkgver kept in sync by release-please extra-files
- packaging.yml: new arch-release-package job builds the release PKGBUILD
  from local source on every PR so it cannot rot
- .gitignore: local makepkg artifacts

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 2: Watch CI — in particular the new `arch-release-package` job**

```bash
gh pr checks --watch
```

Expected: all checks pass, including `Packaging / arch-release-package` (this is the end-to-end test of Task 2 + Task 3). The `Release / release-please` job does not run on PRs (push-to-main trigger only).

If `arch-release-package` fails: pull the log with `gh run view --log-failed`, fix, commit (conventional message), push, re-watch. Likely failure points: the `sed` source swap not matching (check the PKGBUILD `source=` line verbatim) or the `git archive` prefix not matching `$pkgname-$pkgver/`.

- [ ] **Step 3: Merge the PR** (repo uses merge commits, like PRs #1/#2)

```bash
gh pr merge --merge --delete-branch
```

---

### Task 8: First release — end-to-end verification and release-as cleanup

After the merge, `Release / release-please` runs on the push to main and opens a release PR titled `chore(main): release 0.1.0` containing CHANGELOG.md, `.release-please-manifest.json` → `{".":"0.1.0"}`, and the PKGBUILD `pkgver=` line (already 0.1.0, so unchanged or rewritten in place).

- [ ] **Step 1: Confirm the release PR exists**

```bash
gh run list --workflow=Release --limit 3
gh pr list --label 'autorelease: pending'
```

Expected: a successful Release run and one open PR `chore(main): release 0.1.0`. Inspect its diff (`gh pr diff <num>`): CHANGELOG.md added, manifest updated, PKGBUILD `pkgver=0.1.0`.

- [ ] **Step 2: Merge the release PR**

```bash
gh pr merge <num> --merge
```

- [ ] **Step 3: Watch the asset jobs**

```bash
gh run watch "$(gh run list --workflow=Release --limit 1 --json databaseId --jq '.[0].databaseId')"
```

Expected: `release-please` creates tag `v0.1.0` + GitHub Release; `arch-package` and `tarball` jobs both succeed.

- [ ] **Step 4: Verify the release assets**

```bash
gh release view v0.1.0 --json assets --jq '.assets[].name'
```

Expected (4 assets):

```
github-qemu-runner-0.1.0-1-x86_64.pkg.tar.zst
github-qemu-runner-0.1.0-1-x86_64.pkg.tar.zst.sha256
github-qemu-runner_0.1.0_linux_amd64.tar.gz
github-qemu-runner_0.1.0_linux_amd64.tar.gz.sha256
```

If an asset job failed: the release and tag still exist; fix the workflow on a branch, merge, then re-run the failed job from the Actions UI or `gh run rerun <run-id> --failed` — uploads use `--clobber`, so re-runs are safe.

- [ ] **Step 5: Remove the one-off `release-as`**

`release-as` forces every future release to 0.1.0 if left in. Edit `release-please-config.json` to delete the `"release-as": "0.1.0",` line, leaving:

```json
{
  "$schema": "https://raw.githubusercontent.com/googleapis/release-please/main/schemas/config.json",
  "packages": {
    ".": {
      "release-type": "go",
      "bump-minor-pre-major": true,
      "extra-files": ["packaging/arch/release/PKGBUILD"]
    }
  }
}
```

- [ ] **Step 6: Validate, commit on a branch, PR, merge**

```bash
jq . release-please-config.json
git switch -c chore/drop-release-as
git add release-please-config.json
git commit -m 'chore: drop one-off release-as after first release'
git push -u origin chore/drop-release-as
gh pr create --fill
gh pr checks --watch
gh pr merge --merge --delete-branch
```

---

## Self-review notes

- Spec coverage: release-please config (Task 4), release.yml golden-path workflow with both asset jobs (Task 5), release PKGBUILD with annotation (Task 2), PR-time validation job (Task 3), gitignore housekeeping (Task 1), first-release bootstrap + release-as removal (Task 8). Repo-settings prerequisite the spec implies (release PRs via GITHUB_TOKEN) is Task 6.
- The `-git` PKGBUILD and existing `packaging.yml` jobs are deliberately untouched.
- Commit messages are conventional; commitlint runs on every PR.
