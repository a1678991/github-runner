# Docker backend (gVisor-sandboxed) — Design

**Date:** 2026-06-11
**Status:** Approved (brainstorming phase)
**Extends:** [2026-06-10-qemu-runner-design.md](2026-06-10-qemu-runner-design.md)

## What this is

A second execution backend for `github-qemu-runner`: each job runs in a
disposable Docker container sandboxed by gVisor (`runsc`) instead of a
QEMU/KVM virtual machine. It trades the VM isolation boundary for a
userspace-kernel sandbox in exchange for much wider platform support — any
Linux host with Docker, including VMs without nested virtualization (e.g.
OCI Ampere A1 free-tier instances) and any architecture Docker and the
actions-runner support (arm64, x86_64).

QEMU remains the flagship, most-secure backend; the project name stays
`github-qemu-runner`. The Docker backend is the documented fallback for
hosts without `/dev/kvm`.

Key decisions (settled during brainstorming):

| Decision | Choice |
|---|---|
| Integration | New provisioner behind the existing `pool.Provisioner`/`pool.VM` interfaces; slot loop untouched |
| Backend selection | Per-pool `backend: qemu \| docker`, default `qemu` |
| Docker-in-job | Yes — dockerd runs *inside* the runner container, inside the gVisor sandbox (DinD) |
| Sandbox | gVisor `runsc` by default; explicit `runtime: runc` opt-out with loud warnings |
| Runner image | Built locally by `refresh-image` from an embedded Dockerfile (no registry dependency) |
| Docker control | Shell out to the `docker` CLI (consistent with qemu/qemu-img usage; no Docker SDK dependency) |
| Naming | Project name unchanged |

## Architecture

```
┌─ host (any systemd Linux with Docker + gVisor) ────────────────────┐
│  github-qemu-runner controller  (systemd service, user: gh-runner) │
│  ├── pool "fmt"   (backend: docker, count=2, 2cpu/2G)              │
│  │     └── slots loop: JIT-register → docker run (runsc, DinD)     │
│  │                     → wait for container exit → teardown → loop │
│  └── pool "build" (backend: qemu, ...)   ← unchanged               │
│                                                                    │
│  /var/lib/github-qemu-runner/                                      │
│    images/base.qcow2           ← qemu backend (as today)           │
│    images/docker-base.json     ← docker image provenance sidecar   │
│    run/<name>/                 ← per-job workdir (jit file)        │
│                                                                    │
│  docker image ghq-runner-base:latest  ← baked by `refresh-image`   │
└────────────────────────────────────────────────────────────────────┘
```

### New/changed components

| Component | Change |
|---|---|
| `internal/config` | Pool gains `backend` field; new top-level `docker:` section (`runtime: runsc \| runc`, default `runsc`) |
| `internal/dockerbackend` (new) | `Provisioner` (implements `pool.Provisioner`) + `Container` (implements `pool.VM`); embedded Dockerfile + entrypoint; image bake via `docker build`; orphan-container reaping helper |
| `internal/controller` | Per-backend preflight: QEMU checks only when a `qemu` pool exists; Docker checks (CLI present, daemon reachable, image present, runtime registered) only when a `docker` pool exists. Wires the right provisioner per pool |
| `internal/controller/reap` | Additionally force-removes containers matching label `ghq.managed=true` |
| `refresh-image` | Bakes images for every backend the config uses: qcow2 as today, plus `docker build` → `ghq-runner-base:latest` + `images/docker-base.json` provenance |
| `setup` | Docker preflights added, same all-"ok" report style; warns prominently when `runtime: runc` |

`internal/pool`, `internal/github`, `internal/qemu`, `internal/seed`,
`internal/imagebake` are untouched.

## Container lifecycle (one slot iteration)

1. **JIT register** — unchanged (`pool` package).
2. **Provision** — write the JIT blob to `run/<name>/jit/config` (0600),
   then:
   ```
   docker run -d --name <ghq-pool-shortid> \
     --runtime=runsc --privileged \
     --cpus <pool.cpus> --memory <pool.memory_mb>m \
     --label ghq.managed=true \
     -v run/<name>/jit:/jit:ro \
     ghq-runner-base:latest
   ```
   No published ports: outbound-only networking on the default bridge,
   matching the slirp posture of the QEMU backend.
3. **Entrypoint** (baked into the image) — start `dockerd` inside the
   sandbox, wait for its socket, then run
   `run.sh --jitconfig $(cat /jit/config)` as the unprivileged `runner`
   user (docker group). The JIT runner takes exactly one job and exits;
   the entrypoint exits; the container stops.
4. **Liveness gate / wait / drain** — unchanged slot-loop logic via the
   `pool.VM` interface:
   - `Done()` — goroutine blocked on `docker wait`
   - `Powerdown(timeout)` — `docker stop -t <timeout>` (SIGTERM to the
     entrypoint, which forwards to the runner)
   - `Kill()` — `docker rm -f`
   - `ConsoleTail()` — `docker logs --tail`
5. **Teardown** — `docker rm` the exited container (destroying its
   anonymous volumes, including the inner `/var/lib/docker`), delete
   `run/<name>/`, best-effort runner-record delete, loop.

### Docker-in-Docker inside gVisor

The inner dockerd serves `container:` jobs, service containers, container
actions, and `docker build` — feature parity with the QEMU guest.
`--privileged` is required for the inner dockerd, and is acceptable
*because* it is granted inside the gVisor sandbox: the capabilities apply
to gVisor's userspace kernel, not the host kernel. The inner
`/var/lib/docker` lives in an anonymous volume created and destroyed with
the container, so no Docker state (images, caches, containers) survives a
job.

With the `runtime: runc` opt-out, `--privileged` is root-equivalent on the
host: `runc` + DinD has effectively **no isolation boundary**. The
controller and `setup` log a prominent warning for this configuration and
the README states it plainly. It exists only for hosts where gVisor cannot
run and the operator explicitly accepts the risk.

## Image bake (`refresh-image`, docker mode)

1. **Resolve runner version** — same policy as the qcow2 bake: query
   `actions/runner` latest release, select the tarball matching the host
   architecture (`linux-x64` / `linux-arm64`), verify the published
   SHA256.
2. **Build** — `docker build` from an embedded Dockerfile
   (Ubuntu 24.04 base + Docker Engine + actions-runner under
   `/opt/actions-runner` + entrypoint; `runner` user, no sudo, docker
   group). The resolved version and checksum are passed as build args.
3. **Tag + provenance** — tag `ghq-runner-base:latest`; write
   `images/docker-base.json` (runner version, base image digest, build
   timestamp), mirroring `base.json`. Running containers are unaffected;
   new slots pick up the new image on their next iteration.

Architecture support falls out naturally: the build runs on the target
host, so an arm64 host produces an arm64 image. Pool labels should
advertise the real architecture (e.g. `arm64` instead of `x64`).

## Configuration

```yaml
docker:            # optional; only read by docker-backend pools
  runtime: runsc   # runsc (default) | runc (loud-warning opt-out)

pools:
  - name: oci-arm
    backend: docker          # qemu (default) | docker
    scope: org
    org: my-org
    count: 2
    cpus: 2
    memory_mb: 8192
    disk_gb: 30              # advisory for docker pools (see below)
    labels: [self-hosted, linux, arm64, oci]
```

Validation: `backend` must be `qemu` or `docker`; `docker.runtime` must be
`runsc` or `runc`. Existing pool validation applies unchanged.

**`disk_gb` is advisory for docker pools.** Docker's standard storage
drivers cannot enforce a per-container disk quota, so the value is used
only for capacity warnings; the README documents that a runaway job can
fill the host filesystem. (The QEMU backend genuinely enforces it.)

## Host requirements (docker backend)

- Docker Engine, with gVisor's `runsc` registered as a runtime in
  `/etc/docker/daemon.json`
- gVisor installation is a documented manual prerequisite per distro
  (like QEMU is for the qemu backend); the default systrap platform needs
  no `/dev/kvm` and supports arm64 and x86_64
- `setup` preflights: docker CLI on PATH, daemon reachable, `runsc`
  registered (via `docker info`), `ghq-runner-base:latest` present

## Error handling

Identical shape to the QEMU backend, because the slot loop is shared:

- Provision failures clean their workdir and back off exponentially.
- Startup reaping force-removes containers labeled `ghq.managed=true` and
  deletes stale `ghq-*` runner records (existing logic).
- Graceful shutdown drains via the existing idle/busy logic; `docker stop`
  replaces QMP powerdown.
- A container that exits before the runner comes online fails the liveness
  gate; `docker logs` tail is surfaced before teardown.

## Security posture (vs. QEMU backend)

Accepted trade-off: a userspace-kernel sandbox instead of a hardware VM.

- **gVisor is the isolation boundary.** Workflows get a synthetic kernel
  (systrap platform); host-kernel attack surface is reduced to gVisor's
  narrow host-syscall profile. Weaker than KVM, far stronger than plain
  containers.
- `--privileged` applies inside the sandbox only (see DinD section);
  with `runtime: runc` it is host-root-equivalent — loud-warning opt-out.
- JIT blob delivered via a 0600 file bind-mounted read-only, deleted on
  teardown — never in host-side argv or container env, so it is not
  visible in `docker inspect` (inside the sandbox it briefly appears in
  the entrypoint's argv, same as the QEMU guest's `run-one-job`).
- Outbound-only networking; no published ports.
- Same operational rules as the QEMU backend: dedicated `gh-runner` user,
  runner groups to scope repos, never attach to public repositories.
- The `gh-runner` user needs access to the Docker daemon (docker group),
  which is root-equivalent on the host; the systemd hardening directives
  still apply but the docker socket is the documented widening vs. the
  QEMU backend's kvm-group-only posture.

## Testing

- **Unit** (no real Docker in CI, consistent with existing fake/golden
  style): config validation tables for `backend`/`docker.runtime`;
  `docker run`/`stop`/`rm`/`logs` argument-construction golden tests;
  entrypoint/Dockerfile rendering golden tests; reap filter tests.
- **Integration smoke** (manual): `setup` → `refresh-image` → one-slot
  docker pool against a scratch repo → trivial workflow incl. a
  `container:` job (exercises inner dockerd) → assert success and full
  teardown. Natural target: the OCI A1 host.

## Out of scope

- Installing gVisor/Docker on hosts (documented prerequisite, not code).
- Per-container disk quotas (storage-driver dependent; documented).
- Registry-published runner images (local build only, by decision).
- Autoscaling (same deferral as the QEMU backend).
