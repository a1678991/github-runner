# Seccomp isolation mode for docker pools — Design

**Date:** 2026-06-13
**Status:** Approved (brainstorming phase)
**Extends:** [2026-06-11-docker-backend-design.md](2026-06-11-docker-backend-design.md)

## What this is

A per-pool isolation mode for the docker backend that runs job containers
under native `runc` confined by Docker's default seccomp profile, instead
of under gVisor. It removes runsc's syscall-interception overhead for jobs
that do not need Docker inside the job (build / test / lint workloads),
at the cost of sharing the host kernel.

Threat-model ladder, strongest first:

1. **qemu backend** — hardware VM boundary.
2. **docker + `isolation: gvisor`** (default) — userspace-kernel sandbox;
   full Docker-in-job support.
3. **docker + `isolation: seccomp`** (this design) — standard container
   boundary: namespaces + cgroups + Docker's default seccomp allowlist +
   default capability set with extra trims. No Docker inside the job.
4. **docker + `isolation: gvisor` + `docker.runtime: runc`** — privileged
   and unconfined; effectively no boundary (existing loud-warning opt-out).

Note that seccomp mode is strictly stronger than the existing
`docker.runtime: runc` escape hatch: that path keeps `--privileged`
(which disables seccomp entirely), while seccomp mode drops it.

Key decisions (settled during brainstorming):

| Decision | Choice |
|---|---|
| Docker-in-job | Not supported in seccomp pools — DinD requires `--privileged`, which disables seccomp; pools needing `container:` jobs keep gvisor isolation |
| Selection | Per-pool `isolation: gvisor \| seccomp` on docker pools, default `gvisor`; one host can mix fast seccomp pools and DinD gvisor pools |
| Profile | Docker's built-in default seccomp profile (battle-tested allowlist); optional per-pool `seccomp_profile:` path for a custom profile |
| Capabilities | Docker default set minus `NET_RAW` and `MKNOD`; **no** `no-new-privileges` and **no** `cap-drop ALL`, so passwordless sudo / `apt-get install` keep working (GitHub-hosted parity) |
| Runtime | Explicit `--runtime runc` for seccomp pools regardless of `docker.runtime` (which keeps governing gvisor pools only) |
| Image | Separate slim image `ghq-runner-slim:latest` without Docker Engine, built as a stage of the existing Dockerfile |

## Configuration

```yaml
docker:
  runtime: runsc        # unchanged; applies to gvisor-isolation pools only

pools:
  - name: fast
    backend: docker
    isolation: seccomp        # gvisor (default) | seccomp
    # seccomp_profile: /etc/ghq/strict.json   # optional custom profile
    scope: org
    org: my-org
    count: 2
    cpus: 4
    memory_mb: 8192
    disk_gb: 30               # advisory, as for all docker pools
    labels: [self-hosted, linux, x64, fast]
  - name: dind
    backend: docker           # isolation defaults to gvisor — unchanged
    ...
```

Validation:

- `isolation` must be `gvisor` or `seccomp`, and may only be set on
  `backend: docker` pools (qemu pools reject it).
- `seccomp_profile` must be an absolute path and may only be set with
  `isolation: seccomp`. Empty means Docker's built-in default profile.

## Container launch (`internal/dockerbackend`)

`RunArgs` branches on `Pool.Isolation`:

- **gvisor** (default): today's argv, unchanged —
  `--runtime <docker.runtime> --privileged … ghq-runner-base:latest`.
- **seccomp**:

  ```
  docker run -d --name <ghq-pool-shortid> \
    --runtime runc \
    --cap-drop NET_RAW --cap-drop MKNOD \
    [--security-opt seccomp=<pool.seccomp_profile>] \
    --cpus <pool.cpus> --memory <pool.memory_mb>m \
    --label ghq.managed=true \
    -v run/<name>/jit:/jit:ro \
    ghq-runner-slim:latest
  ```

  No `--privileged`, so Docker's default seccomp profile and capability
  bounding apply. No `no-new-privileges` (it would break sudo, which the
  image grants for GitHub-hosted parity). Dropping `NET_RAW` breaks
  `ping` inside jobs — documented divergence from GitHub-hosted runners.

The Provisioner already receives the `config.Pool`, so the branch is
local to `RunArgs` plus image selection. Lifecycle (`docker wait` /
`stop` / `rm` / `logs`), the JIT bind-mount, reaping by the
`ghq.managed=true` label, and the slot loop are all unchanged.

## Slim image

The embedded Dockerfile becomes multi-stage:

- `base` — Ubuntu 24.04 + ca-certificates/curl/git/sudo, `runner` user
  (uid 1001, passwordless sudo), actions-runner under
  `/opt/actions-runner`.
- `dind` — `base` + Docker Engine + docker group membership +
  `VOLUME /var/lib/docker` + the existing entrypoint. Tagged
  `ghq-runner-base:latest` (name unchanged for compatibility).
- `slim` — `base` + new `entrypoint-slim.sh` (same JIT-file check and
  `runuser … run.sh --jitconfig` exec, no dockerd block). Tagged
  `ghq-runner-slim:latest`. ~400 MB smaller; a job that runs `docker`
  gets "command not found", which is the accurate failure.

`entrypoint-slim.sh` is a separate embedded file rather than an env
switch in the shared script: with no dockerd installed there is nothing
to condition on, and the dind entrypoint keeps failing loudly when its
dockerd cannot start.

`refresh-image` builds only the variants the config's pools actually use
(`docker build --target dind|slim`); both targets share the `base` layer
cache, so mixed hosts pay the base build once. The
`images/docker-base.json` provenance sidecar gains a `variants` field
listing what was baked.

## Preflight

- **Controller start:** image-presence check per variant in use
  (`ghq-runner-base` for gvisor pools, `ghq-runner-slim` for seccomp
  pools); existence check for every configured `seccomp_profile` file —
  fail at startup, not at the first job.
- **`setup`:** the "runsc registered" check fires only when some docker
  pool uses gvisor isolation, so a seccomp-only host never needs gVisor
  installed. The outbound-connectivity check runs once per isolation
  mode in use, each with its matching runtime and image
  (runsc + `ghq-runner-base`, runc + `ghq-runner-slim`).

## Security posture

- The boundary is the standard container one: namespaces, cgroups,
  Docker's default seccomp allowlist (blocks `mount`, `bpf`, `kexec`,
  `keyctl`, userns-creation tricks, etc.), and the trimmed default
  capability set. The host kernel is shared — a kernel 0-day reachable
  through the allowlisted syscalls escapes; gVisor pools do not have this
  exposure. The README states this plainly and keeps gvisor the default.
- `sudo` inside the container reaches container-root confined by the
  same seccomp profile and capability bounding set; this is the accepted
  cost of GitHub-hosted parity, identical in kind to the gvisor image.
- JIT-blob handling, outbound-only networking, no published ports,
  runner-group scoping, and the never-public-repos rule are unchanged
  from the docker-backend design.
- The custom `seccomp_profile` knob can only tighten or change the
  profile, never disable it (`unconfined` is not a path and is not
  accepted).

## Error handling

Unchanged shape — shared slot loop, same provision-failure backoff, same
reaping, same drain. One addition: controller startup fails fast on a
missing slim image or missing custom profile file (see Preflight).

## Testing

- **Unit:** config validation tables for `isolation` /
  `seccomp_profile` (defaults, bad values, qemu-pool rejection,
  relative-path rejection); `RunArgs` golden argv for both isolation
  modes, with and without a custom profile; bake target selection per
  config; embed test for `entrypoint-slim.sh`; setup-check gating tests.
- **Integration smoke** (manual): `refresh-image` on a host with both
  pool kinds → one seccomp slot runs a trivial workflow (incl.
  `sudo apt-get install` to prove the sudo path) → assert success, full
  teardown, and that a `docker ps` step fails with command-not-found.

## Out of scope

- Docker support inside seccomp jobs (rootless DinD / sysbox — possible
  future work, separate design).
- Shipping a project-maintained strict seccomp profile (the
  `seccomp_profile` knob lets operators bring their own).
- Per-container disk quotas (same advisory `disk_gb` as all docker
  pools).
