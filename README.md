# github-qemu-runner

Ephemeral GitHub Actions self-hosted runners on a Linux host. Every job runs
in a disposable QEMU/KVM virtual machine — a Linux or a Windows guest — that
is destroyed afterwards; the VM is the isolation boundary. A sandboxed
Docker backend (gVisor by default, or a faster seccomp mode) is available as
a fallback for hosts without `/dev/kvm` (see below). Linux sibling of
[github-tart-runner](https://github.com/a1678991/github-tart-runner) (macOS).

Design: `docs/superpowers/specs/2026-06-10-qemu-runner-design.md`.

## Features

- One disposable VM (or gVisor-sandboxed container) per job; the runner is
  pre-registered ephemeral via GitHub App JIT config — no PATs, no
  registration tokens, nothing long-lived inside the guest
- Static pools with per-pool sizing (`count`, `cpus`, `memory_mb`,
  `disk_gb`), labels, and `org` or `repo` registration scope
- Optional [runner groups](#runner-groups) on org-scoped pools
- Optional [Docker backend](#docker-backend-hosts-without-devkvm) for hosts
  without KVM and for arm64, with per-pool `isolation: gvisor | seccomp`
  (seccomp = no sandbox overhead, for jobs that don't need Docker inside)
- Optional [Windows pools](#windows-pools) on the qemu backend
  (`os: windows`), baked from the Windows Server evaluation image
- GitHub Enterprise Server support via `github.api_base_url`
- Graceful drain on stop (busy runners get `drain_timeout` to finish);
  automatic crash recovery with orphan VM/record reaping on startup
- systemd-native: hardened unit, optional `LoadCredential` key handling;
  packages for Arch, Debian/Ubuntu, and NixOS

## How it works

A Go daemon (`controller`) supervises static pools of runner slots. Per
slot, forever:

1. `POST .../generate-jitconfig` — pre-register an ephemeral runner (GitHub App auth)
2. `qemu-img create` a copy-on-write overlay of the baked base image
3. Build a cloud-init NoCloud seed ISO carrying the JIT config
4. Boot `qemu-system-x86_64` (KVM, user-mode networking, no inbound)
5. The guest runs exactly one job, then powers off; the QEMU process exits
6. Delete the workdir + runner record, loop

`refresh-image` bakes the base image: Ubuntu 24.04 cloud image + Docker CE
(with the buildx and compose v2 plugins, so `docker build` and
`docker compose` work in jobs) + actions-runner (latest,
checksum-verified), flattened to
`/var/lib/github-qemu-runner/images/base.qcow2`. OS package updates are
applied during the bake, and unattended-upgrades and the apt-daily timers
are removed from the image — guest OS updates arrive via image refresh,
so `apt-get` in a job never races a background upgrade for the apt lock.

## Docker backend (hosts without /dev/kvm)

Pools with `backend: docker` run each job in a disposable Docker container
sandboxed by gVisor instead of a VM — for hosts without nested
virtualization (e.g. OCI Ampere A1 free-tier instances) and for arm64.
Jobs keep full Docker support: a private dockerd runs *inside* the
sandboxed container (DinD), so `container:` jobs, service containers,
`docker build`, and `docker compose` work as on the QEMU backend.

Security trade-off, explicitly: gVisor is a userspace-kernel sandbox —
weaker than a KVM VM, far stronger than a plain container. The job
container runs `--privileged` so the inner dockerd works; under `runsc`
those privileges apply to gVisor's synthetic kernel, not the host. Setting
`docker.runtime: runc` removes the sandbox entirely and `--privileged`
becomes root on the host — never do this on a machine you care about.
`disk_gb` is advisory on docker pools (standard storage drivers cannot
enforce per-container quotas); a runaway job can fill the host filesystem.

### Seccomp isolation mode (higher performance, no Docker-in-job)

Docker pools that don't need Docker inside the job (build / test / lint
workloads) can opt into `isolation: seccomp` per pool:

```yaml
pools:
  - name: fast
    backend: docker
    isolation: seccomp   # gvisor (default) | seccomp
    # seccomp_profile: /etc/ghq/strict.json   # optional custom profile
    ...
```

The job container then runs under native `runc` **without** `--privileged`,
so Docker's default seccomp profile and capability bounding apply (plus
`NET_RAW`/`MKNOD` dropped — `ping` won't work inside jobs). This removes
gVisor's syscall-interception overhead entirely; gVisor is not required
on the host if every docker pool uses seccomp isolation.

Trade-offs, explicitly:

- **Weaker than gVisor:** the job shares the host kernel behind the standard
  container boundary (namespaces + cgroups + seccomp allowlist + capability
  bounding). A kernel 0-day reachable through allowlisted syscalls escapes.
  Isolation ladder: qemu > gvisor > seccomp > `docker.runtime: runc`
  (privileged + unconfined). Note that seccomp mode is strictly stronger
  than the `runtime: runc` escape hatch.
- **No Docker inside jobs:** `container:` jobs, service containers,
  `docker build`, and `docker compose` fail (the slim image
  `ghq-runner-slim:latest` ships no Docker Engine). Keep such jobs on a
  gvisor pool — one host can run both.
- `sudo`/`apt-get` keep working (GitHub-hosted parity); `docker.runtime` is
  ignored by seccomp pools.
- `seccomp_profile` (absolute path) swaps in a custom profile instead of
  Docker's built-in default; it can tighten the sandbox, never disable it.

Host prerequisites:

1. Docker Engine, with the `gh-runner` user in the `docker` group
   (docker-socket access is root-equivalent — this is the documented
   widening vs. the qemu backend's kvm-group-only posture).
2. **gvisor-isolation pools only:** gVisor (`runsc`) from
   [gvisor.dev](https://gvisor.dev/docs/user_guide/install/),
   registered in `/etc/docker/daemon.json`:

   ```json
   {
     "runtimes": {
       "runsc": {
         "path": "/usr/bin/runsc",
         "runtimeArgs": ["--net-raw", "--allow-packet-socket-write"]
       }
     }
   }
   ```

   Both runtimeArgs are required for networking inside the inner dockerd
   (the second is mandatory with Docker 28+). Restart docker after editing.
3. **OCI Ubuntu images only:** the stock nftables `inet filter` table has
   a `forward` chain with policy DROP that silently kills all container
   traffic *before* Docker's own rules. Persist accept rules (e.g. in
   `/etc/nftables.conf`):

   ```
   nft add rule inet filter forward ct state related,established accept
   nft add rule inet filter forward iifname docker0 accept
   ```

   `setup` runs an outbound-connectivity check from inside a container to
   catch exactly this.

Then: `setup` → `refresh-image` (builds the image variants the configured
pools need natively, so the arch always matches the host) → `systemctl enable --now
github-qemu-runner`. Label docker pools with the real architecture (e.g.
`arm64`), and as with the qemu backend: never attach runners to public
repositories.

## Windows pools

`backend: qemu` pools can run Windows guests with `os: windows`. The base
image is baked from Microsoft's **Windows Server 2025 evaluation** VHDX
(English, x64): `refresh-image` downloads it (11 GB, cached across bakes
via ETag), boots it once under UEFI with an answer file that completes
OOBE unattended, installs virtio drivers from the virtio-win ISO, Git for
Windows, the toolchain described below, and the actions runner (win-x64)
— the Git installer and the runner zip are verified against the SHA-256
upstream publishes in its release notes, and WinGet checks each package
installer against the SHA-256 in its manifest; the VHDX and the driver
ISO are TLS-only (with an https→http downgrade guard) unless you pin
`windows.image_sha256` / `windows.virtio_win_sha256` — and flattens the
result to `base-windows.qcow2`. Job VMs then clone it exactly like Linux
pools: virtio-blk + virtio-net, the JIT config on a seed CD-ROM, one job,
power off.

```yaml
pools:
  - name: win
    os: windows
    scope: org
    org: my-org
    count: 1
    cpus: 4
    memory_mb: 8192            # >= 2048 on windows pools
    disk_gb: 80                # floor: the image's 64 GiB virtual size
    labels: [self-hosted, windows, x64]

# optional overrides; every key has a default
windows:
  # image: https://go.microsoft.com/fwlink/?linkid=2345826   # Server 2025 eval VHDX;
  #                           # an absolute path to a local image works too
  # image_sha256: ""          # verify when set; Microsoft publishes no checksum file
  # virtio_win: ...           # versioned https URL of the virtio-win ISO, or an absolute path
  # virtio_win_sha256: ""     # verify the virtio-win ISO when set
  # ovmf_dir: /usr/share/edk2/x64   # auto-detected on Arch and Debian/Ubuntu
  # packages: [Microsoft.PowerShell, GitHub.cli, jqlang.jq, 7zip.7zip, LLVM.LLVM]   # WinGet IDs; [] installs none
  # build_tools: true         # VS 2022 Build Tools (C++) + Windows 11 SDK (signtool)
  # disable_defender: true    # hosted-image Defender posture; false leaves Defender at its defaults
```

`windows.image` and `windows.virtio_win` each take an http(s) URL — then
the file is downloaded into `paths.images` and cached across bakes — or an
absolute path to a file already on the host, which the bake reads where it
lies and never copies. `setup` reports whether a local path exists and is a
regular file, and the controller checks the same at startup; a bake
triggered by `images.auto_refresh` validates it again itself.

Inside the guest, jobs run as the local administrator `runner` in an
interactive session, on an image built to behave like GitHub's
`windows-2025` hosted image where CI depends on it:

- **On the machine PATH:** Git (with Git LFS, symlinks enabled,
  `safe.directory *`) and its `bash`, PowerShell 7 (`pwsh`, so
  `shell: pwsh` and the default Windows shell work), `gh`, `jq`, `7z`,
  and LLVM's `clang` and `lld-link` (`C:\Program Files\LLVM\bin`, as on
  the hosted image) — the `windows.packages` default list, installed with
  WinGet at bake time, with WinGet told to prefer MSI/EXE installers over
  MSIX, as on the hosted image. Add any WinGet package ID
  (`winget search <name>` finds them) or trim the list; `[]` installs
  none. A package is installed machine-wide when its manifest offers a
  machine-scope installer; otherwise with the installer's default scope,
  which may put the tool in the bake user's profile, where jobs (running
  as `runner`) cannot see it.
- **C++ toolchain:** Visual Studio 2022 Build Tools with the C++ workload
  (MSVC, MSBuild) and the Windows 11 SDK, so `signtool`, `link.exe` and
  Rust's `x86_64-pc-windows-msvc` target work
  (`windows.build_tools: false` skips them). The VC++ 2005–2015+
  runtimes are installed either way.
- **Posture like the hosted image:** Windows Update, telemetry, SysMain
  and background maintenance off; no UAC prompt; long paths on;
  Microsoft Defender installed but with real-time, behaviour, script and
  download scanning off and `C:\` excluded
  (`windows.disable_defender: false` leaves Defender at its defaults).

Tool versions float: each bake installs the current WinGet release, as the
hosted image's weekly refresh does, and `base-windows.json` records the
bake's toolchain summary. What the hosted image has and this one
deliberately lacks: Visual Studio Enterprise, the pre-populated tool
cache for `actions/setup-*`, Docker, browsers and WebDrivers, Android
SDK, databases. Before publishing, the bake fails unless `git`, `bash`
(Git's `bin\bash.exe`, first on the machine PATH as on the hosted image)
and the commands of the default packages it knows (`pwsh`, `gh`, `jq`,
`7z`, `clang` and `lld-link`, each only when listed) run from the machine
PATH, plus MSVC and the Windows SDK with `windows.build_tools` and
Defender's real-time protection being off with `windows.disable_defender`.
Other packages are verified only by WinGet's result. Changing any `windows.*` bake key takes
effect at the next `refresh-image`; existing images are not rebaked
automatically.

A bake with the default options takes 25–45 minutes (Build Tools
dominates). Besides the GitHub API and release downloads every bake
already makes (Git and the runner), the guest needs outbound HTTPS to
the PowerShell Gallery and the NuGet provider bootstrap (for the WinGet
client module), GitHub releases (the current WinGet client), the WinGet
source, and each package's own download host: GitHub releases for
PowerShell, `gh`, `jq` and LLVM, 7-Zip's site, and Microsoft's download
servers and Visual Studio CDN for the VC++ runtimes and Build Tools.
Behind an egress allowlist, an unreachable host fails the bake.

Disk footprint in `paths.images`: about 30 GB steady state per windows base
(11 GB cached VHDX + 0.9 GB virtio-win ISO + ~18 GB baked
`base-windows.qcow2`), peaking near 50 GB during a bake, when the overlay and
the `base-windows.qcow2.new` being converted coexist with the previous base.
Budget 50 GB on top of the Linux images. Local `windows.image` /
`windows.virtio_win` files are not copied there, so with both pointing at
local paths only the baked image counts: roughly 18 GB steady state and
36 GB during a bake.

Host prerequisites on top of the Linux qemu backend: OVMF firmware
(Arch: `pacman -S edk2-ovmf`; Debian/Ubuntu: `apt install ovmf`; NixOS:
`services.github-qemu-runner.windows.enable = true`). `setup` checks for
it when a Windows pool is configured.

Licensing: the image is Microsoft's *evaluation* edition. It is time-limited
(180 days for Server 2025) and is not a production licence — read Microsoft's
evaluation terms and decide whether your use is covered before enabling a
windows pool. Each `refresh-image` bakes a fresh installation from the
pristine download rather than ageing one in place. Set `image` to a
different VHDX (e.g. Server 2022 eval, or your own licensed and generalised
image with the same layout) to change the base.

### Custom images

A local `windows.image` may be any UEFI/GPT disk image `qemu-img` can use
as a backing file — VHDX, qcow2 or raw; an http(s) source is downloaded and
used as a VHDX, so a differently formatted image has to be fetched to the
host first. Either way the image must be **generalised** (sysprepped, OOBE
pending): the
bake boots it with the seed CD's `Unattend.xml`, which completes setup
unattended and runs `bake.ps1` (virtio drivers, Git, the toolchain, the
runner). An image
captured mid-session, or one that has already been through OOBE, never
reaches the sentinel and the bake fails.

A local path must be readable by the service user and outside the trees the
units replace: `/home`, `/root` and `/run/user` (`ProtectHome=yes`) and
`/tmp`, `/var/tmp` (`PrivateTmp=yes`). The service sees those empty even
when the operator can read the file, so put the image somewhere like
`/srv` or `/var/lib`. `setup` warns about such a path instead of failing,
since it runs as you. Local files are used in place, so `paths.images`
holds only the baked `base-windows.qcow2`; pin `windows.image_sha256` if
you want the file verified on every bake.

The backing format follows the file: `.vhdx` → `vhdx`, `.vhd` → `vpc`,
`.qcow2` → `qcow2`, `.img`/`.raw` → `raw`, anything else is probed with
`qemu-img info`. An http(s) source is always treated as a VHDX. Name the
file for what it contains — a qcow2 called `disk.img` would be handed to
`qemu-img` as raw.

Supplying your own licensed image also removes the evaluation-edition
caveats above: nothing is time-limited and no evaluation terms apply.

## Requirements

- Linux host with `/dev/kvm`, systemd
- `qemu-system-x86_64`, `qemu-img`, `genisoimage` on PATH
  (Arch: `pacman -S qemu-base cdrtools`; Debian/Ubuntu: `apt install qemu-system-x86 qemu-utils genisoimage`)
- OVMF firmware for windows pools (see "Windows pools")
- A GitHub App with **Self-hosted runners: Read & write** (org) and/or
  **Administration: Read & write** (repo), installed on the target org/repos

The docker backend has different host prerequisites — see "Docker backend" above.

## Configuration

The controller reads `/etc/github-qemu-runner/config.yaml` (override with
`-config PATH`). Unknown keys are rejected at startup, so typos fail loudly
instead of being ignored. `packaging/config.example.yaml` is a commented
starting point.

A standard configuration is the `github` block plus one or more pools:

```yaml
github:
  app_id: 123456
  installation_id: 7890123
  private_key_path: /etc/github-qemu-runner/app-key.pem

pools:
  - name: build
    scope: org
    org: my-org
    count: 2
    cpus: 8
    memory_mb: 16384
    disk_gb: 60
    labels: [self-hosted, linux, x64, build]
```

### `github` (required)

| Key | Required | Default | Notes |
|---|---|---|---|
| `app_id` | yes | | GitHub App ID |
| `installation_id` | yes | | Installation of that App on the target org/account |
| `private_key_path` | yes | | App private key (PEM). Environment variables are expanded, so `${CREDENTIALS_DIRECTORY}/app-key.pem` works with systemd `LoadCredential` |
| `api_base_url` | no | `https://api.github.com` | Set to `https://HOST/api/v3` for GitHub Enterprise Server |

### Top level

| Key | Required | Default | Notes |
|---|---|---|---|
| `state_dir` | no | `/var/lib/github-qemu-runner` | Base for the default `paths.*` directories; also holds anything outside the configurable paths |
| `paths.images` | no | `<state_dir>/images` | Absolute path. Holds `base.qcow2`, `base.json`, the cloud image download, the bake working dir, and `docker-base.json`; with a windows pool also `base-windows.qcow2`, `base-windows.json`, and the cached `windows-base.vhdx` (+ its `.meta` validator sidecar) and `virtio-win.iso` downloads (only for http(s) sources — a local `windows.image`/`windows.virtio_win` is read where it lies). Operator must create + chown to the runner user when outside `<state_dir>` (systemd `StateDirectory=` does not cover it) |
| `paths.run` | no | `<state_dir>/run` | Absolute path. Holds per-VM workdirs (QEMU) and jit-config mount staging (Docker). Same ownership caveat as `paths.images` |
| `docker.runtime` | no | `runsc` | Runtime for docker-backend job containers: `runsc` (gVisor) or `runc` (no sandbox — read the Docker backend section first) |
| `images.auto_refresh` | no | `true` | When the controller starts and a required image is missing, bake it instead of failing. Set `false` to restore fail-fast (`refresh-image` must be run manually first) |
| `windows.image` | no | `https://go.microsoft.com/fwlink/?linkid=2345826` | Windows base image (Server 2025 evaluation VHDX). http(s) URL or absolute path; local files are used in place, never copied — see "Windows pools" |
| `windows.image_sha256` | no | | Checksum-verifies the image when set (an http(s) source without it falls back to conditional ETag caching; a local one is then used unverified) |
| `windows.virtio_win` | no | `https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/virtio-win-0.1.302.iso` | virtio-win driver ISO installed during the Windows bake. http(s) URL or absolute path; local files are used in place, never copied |
| `windows.virtio_win_sha256` | no | | Checksum-verifies the virtio-win ISO when set |
| `windows.ovmf_dir` | no | auto-detected | Absolute path holding `OVMF_CODE*.fd`/`OVMF_VARS*.fd`; auto-detection tries `/usr/share/edk2/x64`, `/usr/share/OVMF`, `/usr/share/edk2-ovmf/x64` — see "Windows pools" |
| `windows.packages` | no | `[Microsoft.PowerShell, GitHub.cli, jqlang.jq, 7zip.7zip, LLVM.LLVM]` | WinGet package IDs baked into the Windows image; `[]` installs none. Installed machine-wide when the manifest offers a machine-scope installer, otherwise with the installer's default scope, which may leave the tool in the bake user's profile where jobs cannot see it — see "Windows pools" |
| `windows.build_tools` | no | `true` | Bake VS 2022 Build Tools (C++ workload) and the Windows 11 SDK |
| `windows.disable_defender` | no | `true` | Apply the GitHub-hosted image's Defender posture (scanning off, `C:\` excluded); `false` leaves Defender at its defaults |

### Pools

Each pool is a fixed set of `count` slots; every slot runs one VM/container
at a time, forever. Labels may overlap across pools.

| Key | Required | Default | Notes |
|---|---|---|---|
| `name` | yes | | Lowercase alphanumeric + hyphens, max 20 chars; feeds runner/VM names (`ghq-<pool>-<id>`) |
| `backend` | no | `qemu` | `qemu` or `docker` |
| `os` | no | `linux` | `linux` or `windows`; qemu backend only — see "Windows pools" |
| `isolation` | no | `gvisor` | Docker pools only: `gvisor` (default) or `seccomp` |
| `seccomp_profile` | no | | Seccomp pools only: optional absolute path to a custom seccomp profile |
| `scope` | yes | | `org` or `repo` |
| `org` | with `scope: org` | | Organization login |
| `repo` | with `scope: repo` | | `owner/name` |
| `count` | yes | | Concurrent slots, ≥ 1 |
| `cpus` | yes | | vCPUs per VM, ≥ 1 |
| `memory_mb` | yes | | RAM per VM, ≥ 256 (≥ 2048 on windows pools) |
| `disk_gb` | yes | | Disk per VM, ≥ 10; advisory (not enforced) on docker pools. The base image's virtual size is the floor — 64 GiB on windows pools, so smaller values have no effect |
| `labels` | yes | | At least one; runners are targeted by `runs-on` matching all labels |
| `runner_group` | no | `Default` | Org-scoped pools only — see below |
| `liveness_timeout` | no | `5m` | How long a freshly booted runner may take to show up online before the slot is torn down and recycled |
| `drain_timeout` | no | `30m` | On shutdown, how long a busy runner may finish its job before being powered down (the job then fails — GitHub does not requeue jobs from vanished ephemeral runners). systemd `TimeoutStopSec` (35m in the shipped unit) must exceed the largest pool value |

Oversubscription (`sum(count × cpus/memory)` beyond the host) is allowed
but warned about by `setup` and at controller startup.

### Runner groups

Org-scoped pools can register their runners into a named
[runner group](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/managing-access-to-self-hosted-runners-using-groups)
to control which repositories (and which workflows) may use them:

```yaml
pools:
  - name: private
    scope: org
    org: my-org
    count: 1
    cpus: 4
    memory_mb: 8192
    disk_gb: 40
    labels: [self-hosted, linux, x64, private]
    runner_group: private-builders   # optional; defaults to "Default"
```

- The group must already exist (org **Settings → Actions → Runner
  groups**); the controller resolves the name to its ID at each runner
  registration and the slot fails with `runner group "..." not found` if it
  does not. Creating groups beyond `Default` requires a GitHub Team or
  Enterprise plan.
- Repository visibility and "allow public repositories" are properties of
  the group, managed on GitHub — the controller only places runners into
  it.
- `repo`-scoped pools cannot set this: the API has no repo-level runner
  groups (repo runners always belong to the default group), and config
  validation rejects anything but `Default` there.
- No extra App permission is needed; org **Self-hosted runners: Read &
  write** covers listing groups.

## Commands

```
github-qemu-runner [-config PATH] <controller|refresh-image|setup>
```

| Command | What it does |
|---|---|
| `setup` | Preflight: config parses, binaries on PATH, `/dev/kvm` (or docker + runsc) usable, App key parses and authenticates, base image present, capacity warnings. All lines `ok` → ready |
| `refresh-image` | Bakes (or re-bakes) the base images for whichever backends the pools use, including the Windows base when a windows pool exists. Run after install and then periodically |
| `controller` | Runs the pools (the systemd service; also the default when no command is given) |

## Scheduled image refresh

`images.auto_refresh` only bakes images that are *missing* at controller
start. To keep images current (new actions/runner or Ubuntu releases),
enable the bundled timer — shipped by the Arch/Debian packages (and the
manual install above), **off by default**:

```bash
sudo systemctl enable --now github-qemu-runner-refresh.timer
```

It runs `refresh-image` weekly. Change the schedule with a drop-in:

```bash
sudo systemctl edit github-qemu-runner-refresh.timer
# [Timer]
# OnCalendar=daily
```

Running VMs/containers are unaffected; new ones pick up the rebaked image.
On NixOS use the module options instead (see Install (NixOS)).

## Install (manual)

```bash
go build -o github-qemu-runner ./cmd/github-qemu-runner
sudo install -m 0755 github-qemu-runner /usr/local/bin/

sudo useradd --system --home-dir /var/lib/github-qemu-runner \
  --shell /usr/sbin/nologin --groups kvm gh-runner
sudo mkdir -p /etc/github-qemu-runner /var/lib/github-qemu-runner
sudo chown gh-runner:gh-runner /var/lib/github-qemu-runner

sudo cp packaging/config.example.yaml /etc/github-qemu-runner/config.yaml
sudoedit /etc/github-qemu-runner/config.yaml   # app_id, installation_id, pools
sudo install -m 0600 -o gh-runner -g gh-runner \
  /path/to/app-private-key.pem /etc/github-qemu-runner/app-key.pem

sudo -u gh-runner github-qemu-runner setup          # preflight: all "ok"?
sudo -u gh-runner github-qemu-runner refresh-image  # bake base image (~10 min)

sudo cp packaging/github-qemu-runner.service /etc/systemd/system/
# Optional: scheduled image refresh (see "Scheduled image refresh"); off until enabled
sudo cp packaging/github-qemu-runner-refresh.service \
  packaging/github-qemu-runner-refresh.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now github-qemu-runner
```

## Install (Arch Linux)

```bash
cd packaging/arch
makepkg --cleanbuild --syncdeps
sudo pacman -U github-qemu-runner-git-*.pkg.tar.zst
```

The package ships sysusers.d/tmpfiles.d fragments, so the `gh-runner` user
(in the `kvm` group) and `/var/lib/github-qemu-runner` are created by
pacman's hooks — no manual `useradd`. Then:

```bash
sudo cp /etc/github-qemu-runner/config.example.yaml /etc/github-qemu-runner/config.yaml
sudoedit /etc/github-qemu-runner/config.yaml
sudo install -m 0600 -o gh-runner -g gh-runner /path/to/app-private-key.pem /etc/github-qemu-runner/app-key.pem
sudo -u gh-runner github-qemu-runner setup
sudo -u gh-runner github-qemu-runner refresh-image
sudo systemctl enable --now github-qemu-runner
```

## Install (Ubuntu / Debian)

Download the `.deb` for your architecture (`amd64`, `arm64`) from
[Releases](https://github.com/a1678991/github-qemu-runner/releases), or
build it locally with `mise x -- packaging/deb/build.sh <arch>` (output in
`packaging/deb/dist/`). Then:

```bash
sudo dpkg -i github-qemu-runner_*_arm64.deb
```

The package applies the same sysusers.d/tmpfiles.d fragments as the Arch
package on install, so the `gh-runner` user and state directory exist
afterwards; the config/key/setup steps are identical to the Arch section
above. On arm64 hosts use the docker backend (see "Docker backend" above):
install Docker + gVisor and add `gh-runner` to the `docker` group before
`setup`.

Package upgrades never stop or restart a running service — a restart
drains every runner slot, so restart manually when convenient.

## Install (NixOS)

```nix
{
  inputs.github-qemu-runner.url = "github:a1678991/github-qemu-runner";

  # In your nixosSystem modules:
  imports = [ inputs.github-qemu-runner.nixosModules.default ];

  services.github-qemu-runner = {
    enable = true;
    # String path, NOT a Nix path literal (a literal copies the key into
    # the world-readable store).
    privateKeyFile = "/run/secrets/app-key.pem";
    settings = {
      github = {
        app_id = 123456;
        installation_id = 7890123;
      };
      pools = [
        {
          name = "build";
          scope = "org";
          org = "my-org";
          count = 1;
          cpus = 8;
          memory_mb = 16384;
          disk_gb = 60;
          labels = [ "self-hosted" "linux" "x64" "build" ];
        }
      ];
    };
  };
}
```

To enable the periodic image-refresh timer (off by default), add:

```nix
services.github-qemu-runner.refresh = {
  enable = true;
  schedule = "daily"; # any systemd OnCalendar value; default "weekly"
};
```

The module wires the key via systemd `LoadCredential`; for manual
`setup`/`refresh-image` runs use
`systemd-run -P --wait -p LoadCredential=app-key.pem:/run/secrets/app-key.pem github-qemu-runner ... setup`.

## Use from a workflow

Target a pool by listing its labels in `runs-on` (a job matches a runner
only if the runner has *all* the requested labels):

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64, build]
```

For pools in a non-default runner group, nothing changes in the workflow —
the group only controls which repositories are allowed to reach those
runners.

## Runbook

| | |
|---|---|
| Logs | `journalctl -u github-qemu-runner -f` |
| Per-VM console | `<paths.run>/<vm>/console.log` (gone after teardown); defaults to `/var/lib/github-qemu-runner/run/<vm>/console.log` |
| Refresh base image | `sudo -u gh-runner github-qemu-runner refresh-image` (monthly, or after runner/Ubuntu releases; running VMs are unaffected, new VMs pick it up) |
| Scheduled refresh | enable `github-qemu-runner-refresh.timer` (off by default; weekly) — see "Scheduled image refresh" |
| Image provenance | `<paths.images>/base.json` (qemu), `<paths.images>/base-windows.json` (windows), `<paths.images>/docker-base.json` (docker); defaults to `/var/lib/github-qemu-runner/images/` |
| Stop (drains) | `systemctl stop github-qemu-runner` — idle runners are deregistered immediately; busy ones get `drain_timeout` (default 30 min) to finish |
| Crash recovery | automatic: systemd restarts; startup reaping kills orphan VMs and deletes stale `ghq-*` runner records |

## Security notes

- The VM is the isolation boundary; on Linux pools the guest `runner` user
  has no sudo (Docker group membership is the same documented trade-off as
  GitHub-hosted runners). On **windows** pools `runner` *is* a local
  Administrator, matching GitHub-hosted Windows runners — so in-guest
  privilege separation buys nothing there and the VM is the only boundary.
- Outbound-only user-mode networking; nothing can connect into a guest.
- JIT configs are single-use and bound to one pre-created runner record;
  they exist on disk only inside a per-VM seed ISO (0600) that is deleted
  on teardown.
- Do not attach these runners to public repositories (fork-PR risk — see
  GitHub's self-hosted runner hardening guide). On org pools, a
  non-default runner group with restricted repository visibility limits
  who can reach the runners at all.
- Hardening option: pass the App key via systemd `LoadCredential` (see the
  commented lines in the unit file). Note this applies only to the
  controller service: `setup` and `refresh-image` run outside systemd where
  `${CREDENTIALS_DIRECTORY}` is unset, so keep a root-readable key path for
  manual commands or wrap them with `systemd-run -p LoadCredential=...`.
- Run exactly ONE controller instance per org/repo scope. Startup reaping
  deletes offline `ghq-*` runner records in its scopes and would tear down a
  second instance's records.
