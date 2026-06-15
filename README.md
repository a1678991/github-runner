# github-qemu-runner

Ephemeral GitHub Actions self-hosted runners on Linux. Every job runs in a
disposable QEMU/KVM virtual machine that is destroyed afterwards — the VM is
the isolation boundary. A sandboxed Docker backend (gVisor by default, or a
faster seccomp mode) is available as a fallback for hosts without `/dev/kvm`
(see below). Linux sibling of
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

`refresh-image` bakes the base image: Ubuntu 24.04 cloud image + Docker +
actions-runner (latest, checksum-verified), flattened to
`/var/lib/github-qemu-runner/images/base.qcow2`.

## Docker backend (hosts without /dev/kvm)

Pools with `backend: docker` run each job in a disposable Docker container
sandboxed by gVisor instead of a VM — for hosts without nested
virtualization (e.g. OCI Ampere A1 free-tier instances) and for arm64.
Jobs keep full Docker support: a private dockerd runs *inside* the
sandboxed container (DinD), so `container:` jobs, service containers, and
`docker build` work as on the QEMU backend.

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
- **No Docker inside jobs:** `container:` jobs, service containers, and
  `docker build` fail (the slim image `ghq-runner-slim:latest` ships no
  Docker Engine). Keep such jobs on a gvisor pool — one host can run both.
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

## Requirements

- Linux host with `/dev/kvm`, systemd
- `qemu-system-x86_64`, `qemu-img`, `genisoimage` on PATH
  (Arch: `pacman -S qemu-base cdrtools`; Debian/Ubuntu: `apt install qemu-system-x86 qemu-utils genisoimage`)
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
| `paths.images` | no | `<state_dir>/images` | Absolute path. Holds `base.qcow2`, `base.json`, the cloud image download, the bake working dir, and `docker-base.json`. Operator must create + chown to the runner user when outside `<state_dir>` (systemd `StateDirectory=` does not cover it) |
| `paths.run` | no | `<state_dir>/run` | Absolute path. Holds per-VM workdirs (QEMU) and jit-config mount staging (Docker). Same ownership caveat as `paths.images` |
| `docker.runtime` | no | `runsc` | Runtime for docker-backend job containers: `runsc` (gVisor) or `runc` (no sandbox — read the Docker backend section first) |

### Pools

Each pool is a fixed set of `count` slots; every slot runs one VM/container
at a time, forever. Labels may overlap across pools.

| Key | Required | Default | Notes |
|---|---|---|---|
| `name` | yes | | Lowercase alphanumeric + hyphens, max 20 chars; feeds runner/VM names (`ghq-<pool>-<id>`) |
| `backend` | no | `qemu` | `qemu` or `docker` |
| `isolation` | no | `gvisor` | Docker pools only: `gvisor` (default) or `seccomp` |
| `seccomp_profile` | no | | Seccomp pools only: optional absolute path to a custom seccomp profile |
| `scope` | yes | | `org` or `repo` |
| `org` | with `scope: org` | | Organization login |
| `repo` | with `scope: repo` | | `owner/name` |
| `count` | yes | | Concurrent slots, ≥ 1 |
| `cpus` | yes | | vCPUs per VM, ≥ 1 |
| `memory_mb` | yes | | RAM per VM, ≥ 256 |
| `disk_gb` | yes | | Disk per VM, ≥ 10; advisory (not enforced) on docker pools |
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
| `refresh-image` | Bakes (or re-bakes) the base images for whichever backends the pools use. Run after install and then periodically |
| `controller` | Runs the pools (the systemd service; also the default when no command is given) |

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
| Image provenance | `<paths.images>/base.json` (qemu), `<paths.images>/docker-base.json` (docker); defaults to `/var/lib/github-qemu-runner/images/` |
| Stop (drains) | `systemctl stop github-qemu-runner` — idle runners are deregistered immediately; busy ones get `drain_timeout` (default 30 min) to finish |
| Crash recovery | automatic: systemd restarts; startup reaping kills orphan VMs and deletes stale `ghq-*` runner records |

## Security notes

- The VM is the isolation boundary; the guest `runner` user has no sudo
  (Docker group membership is the same documented trade-off as
  GitHub-hosted runners).
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
