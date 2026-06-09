# github-qemu-runner — Design

**Date:** 2026-06-10
**Status:** Approved (brainstorming phase)
**Reference architecture:** [a1678991/github-tart-runner](https://github.com/a1678991/github-tart-runner) (macOS/Tart equivalent)

## What this is

Ephemeral GitHub Actions self-hosted runners on Linux. Each job runs in a
disposable QEMU/KVM virtual machine that is destroyed afterwards. The VM is
the isolation boundary. A single Go binary, run as a systemd service,
orchestrates everything.

Key decisions (settled during brainstorming):

| Decision | Choice |
|---|---|
| Packaging | Go binary + systemd service (Arch host first; any systemd distro) |
| Guest image | Baked Ubuntu 24.04 cloud image with actions-runner + Docker preinstalled |
| Registration | JIT config (`generate-jitconfig` REST API) via GitHub App |
| Per-VM bootstrap | cloud-init NoCloud seed ISO; guest poweroff signals completion |
| VM management | Direct `qemu-system-x86_64` child processes (no libvirt) |
| Concurrency | Static pools with per-pool roles (labels + resources); autoscale-ready |
| Scope | Configurable per pool: org-level or repo-level |

## Architecture

```
┌─ host (Arch, systemd) ─────────────────────────────────────────────┐
│  github-qemu-runner controller  (systemd service, user: gh-runner) │
│  ├── pool "fmt"   (count=2, 2cpu/2G,  labels: ...,fmt)             │
│  │     ├── slot 0 ──┐                                              │
│  │     └── slot 1 ──┤  each slot loops:                            │
│  └── pool "build" (count=1, 8cpu/16G, labels: ...,build)           │
│        └── slot 0 ──┘  JIT-register → overlay+seed → boot QEMU     │
│                        → wait for process exit → cleanup → repeat  │
│                                                                    │
│  /var/lib/github-qemu-runner/                                      │
│    images/base.qcow2          ← baked by `refresh-image`           │
│    images/base.json           ← bake metadata sidecar              │
│    run/<vm-name>/             ← overlay.qcow2, seed.iso,           │
│                                 console.log, qemu.pid, qmp.sock    │
└────────────────────────────────────────────────────────────────────┘
```

### Subcommands

- **`controller`** — the daemon. Loads YAML config, reaps orphans, starts one
  supervisor goroutine per pool slot. Run by systemd.
- **`refresh-image`** — downloads the Ubuntu 24.04 cloud image, bakes runner +
  Docker into a new `base.qcow2`, atomically swaps it in.
- **`setup`** — preflight checks (KVM access, config validity, GitHub App auth
  works) and initial image bake.

### Go packages

| Package | Responsibility |
|---|---|
| `internal/config` | YAML load + validation |
| `internal/github` | App JWT → installation-token cache → JIT config + runner CRUD API |
| `internal/qemu` | Overlay creation, cmdline build, process supervision, QMP powerdown |
| `internal/seed` | NoCloud seed ISO generation (volume label `CIDATA`) |
| `internal/pool` | Slot supervisor; exposes `SetDesired(n)` — static in v1, the autoscaling hook |
| `internal/imagebake` | Image download, verify, bake boot, flatten, swap |

## VM lifecycle (one slot iteration)

1. **JIT register** — `POST .../actions/runners/generate-jitconfig` (org or
   repo URL per pool config) with name `ghq-<pool>-<shortid>`, the pool's
   labels, and runner group. Returns runner ID + base64 JIT blob.
2. **Provision** — instant copy-on-write clone:
   `qemu-img create -f qcow2 -b images/base.qcow2 -F qcow2 run/<vm>/overlay.qcow2`,
   then `qemu-img resize` to the pool's disk size (guest rootfs auto-grows via
   cloud-init growpart). Build a seed ISO whose `user-data` does exactly two
   things: `write_files` the JIT blob (0600, owner `runner`) and
   `runcmd: [run-one-job]`.
3. **Boot** — spawn as a direct child process:
   ```
   qemu-system-x86_64 -accel kvm -cpu host -machine q35
     -smp <pool.cpus> -m <pool.memory_mb>
     -drive file=overlay.qcow2,if=virtio
     -drive file=seed.iso,if=virtio,format=raw,readonly=on
     -netdev user,id=n0 -device virtio-net-pci,netdev=n0
     -nographic -serial file:console.log
     -qmp unix:qmp.sock,server=on,wait=off
     -pidfile qemu.pid -no-reboot -name <vm-name>
   ```
   User-mode (slirp) networking: outbound-only, unprivileged, zero network
   setup. The runner only long-polls GitHub over HTTPS; nothing connects
   *into* the guest.
4. **Liveness gate** — poll the GitHub API until the runner reports `online`
   (deadline: 5 min default, configurable). A guest that wedges before the runner
   connects is killed, its runner record deleted, and the slot retries with
   backoff.
5. **Wait** — `cmd.Wait()`. The JIT runner takes exactly one job (ephemeral
   semantics), `run-one-job` powers the guest off unconditionally afterwards,
   and `-no-reboot` guarantees the QEMU process exits. Idle runners simply
   wait inside their VM — that is the static pool.
6. **Teardown** — remove `run/<vm-name>/` (overlay, seed containing the JIT
   blob, logs; console.log is surfaced to journald on failure), best-effort
   delete of the runner record if it still exists, loop back to step 1.

### In-guest job script (`run-one-job`, baked into the image)

Runs `/opt/actions-runner/run.sh --jitconfig $(cat /etc/runner-jit.conf)` as
the unprivileged `runner` user, then `systemctl poweroff` unconditionally
(success or failure).

### Secrets hygiene

The JIT blob exists only inside the per-VM seed ISO (0600, deleted on
teardown) and never appears in argv. Unlike a registration token, the blob is
bound to one pre-created runner record and cannot register anything else.
This matches the tart version's stdin-piping posture with a smaller blast
radius.

## Image bake (`refresh-image`)

1. **Download** `noble-server-cloudimg-amd64.img` from
   cloud-images.ubuntu.com; verify against published `SHA256SUMS`. Cached and
   skipped when upstream is unchanged.
2. **Resolve runner version** — query the `actions/runner` latest release,
   download `actions-runner-linux-x64-<ver>.tar.gz`, verify the published
   SHA256.
3. **Bake boot** — create a build overlay on the cloud image; boot once with a
   *bake* seed ISO whose user-data:
   - creates the `runner` user (no sudo),
   - installs Docker Engine and runner dependencies (`git`, `libicu`, build
     essentials; `./bin/installdependencies.sh`),
   - unpacks actions-runner to `/opt/actions-runner`,
   - installs `run-one-job`, adds `runner` to the `docker` group,
   - runs `cloud-init clean --machine-id`, then powers off.
   The bake script is `set -euo pipefail`; because cloud-init swallows runcmd
   failures, success is signalled via a sentinel written to the serial log and
   checked by the host.
4. **Flatten + swap** — `qemu-img convert -O qcow2` the (cloudimg + overlay)
   chain into a standalone `base.qcow2.new`, then atomic `rename()` over
   `base.qcow2`. Running VMs keep the old base inode open (Linux fd
   semantics); new slots pick up the new base on their next iteration. A
   `base.json` sidecar records runner version, Ubuntu image serial, and bake
   timestamp.

No SSH and no host-key pinning exist anywhere in the system — the trust
bootstrapping problem from the tart version disappears because nothing ever
connects to the guest.

## Configuration

`/etc/github-qemu-runner/config.yaml`:

```yaml
github:
  app_id: 123456
  installation_id: 7890123
  private_key_path: /etc/github-qemu-runner/app-key.pem  # or systemd LoadCredential

pools:
  - name: fmt
    scope: org              # org | repo
    org: my-org             # repo: "owner/name" when scope: repo
    count: 2
    cpus: 2
    memory_mb: 2048
    disk_gb: 20
    labels: [self-hosted, linux, x64, fmt]
    runner_group: Default
  - name: build
    scope: org
    org: my-org
    count: 1
    cpus: 8
    memory_mb: 16384
    disk_gb: 60
    labels: [self-hosted, linux, x64, build]
```

Startup validation: schema/required fields; label overlap across pools is
allowed (legitimate); `sum(count × resources)` is checked against host
capacity and warns when oversubscribed. v1 has one shared base image; a
per-pool `image` override is a later extension.

GitHub App permissions required: org-level pools need **organization
"Self-hosted runners: Read & Write"**; repo-level pools need **repository
"Administration: Read & Write"**.

## Error handling

- **Slot failures** (API error, base image missing, boot wedge): exponential
  backoff per slot (15 s → 5 min cap), structured logs to journald. Other
  slots keep running.
- **Controller restart/crash**: systemd `Restart=on-failure`. On startup,
  orphan reaping: kill PIDs from leftover `run/*/qemu.pid`, delete stale
  workdirs, delete offline GitHub runner records matching the `ghq-` prefix.
- **Graceful shutdown (SIGTERM)**: per slot — if the runner is idle, delete
  its runner record first (prevents a job landing mid-shutdown), then QMP
  `system_powerdown`; if busy, wait up to a configurable drain timeout, then
  powerdown; SIGKILL fallback. Killing a busy ephemeral runner fails that job
  permanently (GitHub does not requeue) — documented; drain timeout default
  30 min, configurable (systemd `TimeoutStopSec` must exceed it).
- **Job duration**: bounded by GitHub's own job timeout (6 h default). No
  host-side idle timeout in v1; the liveness gate covers boot wedges.

## Security posture

- The **VM is the isolation boundary** (same thesis as the tart version).
  Workflows get a throwaway kernel, filesystem, and network namespace.
- Dedicated `gh-runner` system user in the `kvm` group; systemd hardening
  (`ProtectSystem=strict`, `ReadWritePaths=` state dir only,
  `NoNewPrivileges`, `PrivateTmp`). App private key delivered via systemd
  `LoadCredential` (root-owned at rest).
- Outbound-only slirp networking; no inbound path to guests.
- In-guest `runner` user is unprivileged (no sudo). Docker-group membership
  is the documented escape-hatch trade-off, same as GitHub-hosted runners.
- Runner groups scope which repos may use org-level runners. Do not attach
  these runners to public repos (fork-PR risk, per GitHub hardening docs).

## Autoscaling margin (explicitly out of scope for v1)

The pool supervisor separates *desired count* from *slot supervision* via
`SetDesired(n)`. v1 wires it to the static config value. A later
webhook-driven component (`workflow_job` queued events) can adjust desired
counts at runtime without restructuring the controller.

## Testing

- **Unit**: config validation table tests; GitHub client against `httptest`
  mocks (JWT shape, jitconfig request/response, installation-token cache
  expiry); QEMU cmdline builder golden tests; user-data/seed rendering golden
  tests.
- **Integration smoke** (manual first; CI-able later since GitHub-hosted
  runners expose `/dev/kvm`): `setup` preflight → bake → one-slot pool against
  a scratch repo → run a trivial workflow → assert job success and full
  teardown.
- **Lint**: `go vet` + `golangci-lint` in CI.

## Open items deferred to implementation planning

- Seed ISO generation: pure-Go iso9660 library vs shelling out to
  `genisoimage` (both viable; host has genisoimage).
- Exact actions-runner version pinning strategy (latest-at-bake vs pinned in
  config).
- Networking upgrade path (passt or tap/bridge) if slirp throughput becomes a
  bottleneck for artifact-heavy jobs.
