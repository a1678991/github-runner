# Debian packaging (Ubuntu 24.04, arm64 + amd64) — Design

**Date:** 2026-06-12
**Status:** Approved (brainstorming phase)
**Companion to:** [2026-06-10-packaging-design.md](2026-06-10-packaging-design.md),
[2026-06-11-release-pipeline-design.md](2026-06-11-release-pipeline-design.md)

## What this covers

A `.deb` package for Ubuntu/Debian hosts — primarily Ubuntu 24.04 arm64
(OCI Ampere A1, the docker-backend target the README documents), with an
amd64 sibling since the build is arch-parameterized and amd64 gives a
native-execution smoke test of the identical packaging logic. Built on
every push/PR in `packaging.yml` and attached to GitHub Releases as
`github-qemu-runner_<ver>_<arch>.deb` (+ `.sha256`) by `release.yml`.

Decisions settled during brainstorming:

| Decision | Choice |
|---|---|
| Build tool | `nfpm` (mise-pinned) packaging a prebuilt cross-compiled binary; `lintian` as the native linter gate |
| Why not debhelper | `go.mod` requires Go 1.26; noble's `golang-go` is 1.22, so a policy-clean `Build-Depends` is impossible — the distro-native path degenerates into the same "inject a prebuilt binary" shape with far more machinery (`debian/` dir, strict changelog format that release-please's generic updater cannot write) |
| Why nfpm is OK here despite the release-pipeline rejection | That rejection was Arch-specific: nfpm would have duplicated existing PKGBUILD install logic and bypassed namcap. For deb there is no existing logic to duplicate, and lintian runs on nfpm output just fine |
| Architectures | `arm64` + `amd64` matrix; one `nfpm.yaml` parameterized via env vars (`DEB_ARCH`, `DEB_VERSION`) |
| Version source | Env var, set by CI: release jobs use `${TAG#v}`; PR/dev builds use `0.0.0~r<count>.<shorthash>` (`~` sorts before any release). No release-please `extra-files` entry needed |
| Dependencies | None (CGO_ENABLED=0 static binary). Backend prerequisites stay documented host setup: gVisor is not in Ubuntu's archive, and a `docker.io` Recommends would fight the common docker-ce install. README already covers both |
| Service on upgrade | Never auto-restart/stop on upgrade — a restart drains every runner slot (TimeoutStopSec=35m). Stop only on `prerm remove`. Never auto-enable (config must exist first; matches the Arch package) |
| sysusers/tmpfiles | Reuse `packaging/arch/github-qemu-runner.{sysusers,tmpfiles}` in place — they are distro-agnostic; moving them would churn both PKGBUILDs (one release-managed) for no functional gain |

## Package contents (mirrors the Arch package)

- `/usr/bin/github-qemu-runner` — `CGO_ENABLED=0 GOARCH=<arch> go build -trimpath`
- `/usr/lib/systemd/system/github-qemu-runner.service` — shared unit with
  ExecStart sed'd from `/usr/local/bin` to `/usr/bin` (same transform as the
  PKGBUILDs)
- `/etc/github-qemu-runner/config.example.yaml` — conffile
- `/usr/lib/sysusers.d/github-qemu-runner.conf`, `/usr/lib/tmpfiles.d/github-qemu-runner.conf`
- `/usr/share/doc/github-qemu-runner/copyright` (LICENSE verbatim),
  `README.md`, and a one-stanza `changelog.gz` generated from the build
  version (plain `changelog.gz`, not `changelog.Debian.gz`: the version
  carries no Debian revision, so lintian treats the package as native)
- `/usr/share/lintian/overrides/github-qemu-runner` — overrides
  `statically-linked-binary`, which is the point of a CGO_ENABLED=0 build

> **Amendment (2026-06-12, after local lintian):** the binary is built with
> `-ldflags '-s -w'` to clear `unstripped-binary-or-object` (panic
> backtraces are unaffected — Go resolves frames via pclntab), and the
> description gained an extended paragraph to clear
> `extended-description-is-empty`.

## Maintainer scripts — `packaging/deb/scripts/`

Debian has no pacman-style sysusers/tmpfiles hooks, so `postinst configure`
runs `systemd-sysusers github-qemu-runner.conf` and
`systemd-tmpfiles --create github-qemu-runner.conf`, plus
`systemctl daemon-reload`. Every call is guarded (`command -v` /
`-d /run/systemd/system`) so installation inside a systemd-less container
(the arm64 smoke test) degrades gracefully. `prerm` stops the service only
when `$1 = remove`; `postrm` reloads the unit list. No enable, no restart
on upgrade (see table).

## Build script — `packaging/deb/build.sh`

One entry point used identically by CI and local builds:

```
packaging/deb/build.sh <arch>     # arch ∈ {amd64, arm64}
```

1. Resolve version: `DEB_VERSION` env if set, else `0.0.0~r<count>.<hash>`
2. `CGO_ENABLED=0 GOARCH=<arch> go build -trimpath` into a scratch dir
3. Generate the sed'd service unit, copyright, gzipped changelog stanza
4. `nfpm package -p deb -f packaging/deb/nfpm.yaml -t packaging/deb/dist/`

shellcheck/shfmt clean (lefthook already lints `scripts/`-style shell).

## CI

### `packaging.yml` — new job `deb-package`, matrix `[amd64, arm64]`

1. checkout (`persist-credentials: false`), mise-action (Go + nfpm)
2. `packaging/deb/build.sh $arch`
3. `lintian` (apt-installed) — same gate philosophy as namcap: log
   everything, fail only on `E:` lines
4. Install smoke test:
   - **amd64**: `sudo dpkg -i` on the runner itself (real systemd →
     postinst's sysusers/tmpfiles paths execute), `github-qemu-runner
     nonsense` → exit 2, `getent passwd gh-runner`, state dir exists,
     ExecStart path is executable
   - **arm64**: `docker/setup-qemu-action` (SHA-pinned) + `dpkg -i` and the
     exit-2 check inside an `ubuntu:24.04` `--platform linux/arm64`
     container — real arm64 dpkg + binary under qemu-user; the
     systemd-dependent postinst branches are skipped by their guards
5. Upload `.deb` artifacts

### `release.yml` — new job `deb`, gated on `release_created`

Same build + lintian + smoke steps (matrix again — the repo already accepts
this duplication between the two workflows for the Arch jobs), then
`gh release upload <tag> *.deb *.deb.sha256 --clobber`. `DEB_VERSION` is
`${TAG#v}`, and a guard asserts the built filename matches the tag.

## Repo additions

- `mise.toml` — pin `nfpm`
- `.gitignore` — `packaging/deb/dist/`
- `README.md` — "Install (Ubuntu/Debian)" section: download the `.deb` from
  Releases (or build via `packaging/deb/build.sh`), `dpkg -i`, then the same
  config/setup steps as Arch; note the arm64 + docker-backend pairing

## Verification (local-first, mirroring CI)

1. `build.sh amd64` + `build.sh arm64` locally
2. lintian inside an `ubuntu:24.04` container (not packaged for Arch)
3. `dpkg -i` + exit-2 + sysusers/tmpfiles/unit checks in amd64 and
   (binfmt-emulated) arm64 `ubuntu:24.04` containers
4. Existing lint matrix green (shellcheck/shfmt on build.sh, actionlint/
   zizmor/pinact on the workflows)

## Out of scope

- An apt repository / PPA (Release assets only, same stance as Arch)
- Auto-enabling or restarting the service from maintainer scripts
- Ubuntu-version-specific packages — the static binary plus systemd ≥ 245
  works on any supported Debian/Ubuntu; "Ubuntu 24.04 arm64" is the target
  host, not a package constraint
