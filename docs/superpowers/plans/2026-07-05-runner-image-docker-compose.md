# Docker Compose (and buildx) in the Job Images — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Jobs get `docker compose` (v2 plugin) and BuildKit-native `docker build` (buildx) on both docker-capable runner images, installed from Docker's official apt repo on both.

**Architecture:** Two embedded image-build scripts change: `scripts/guest/bake.sh` (QEMU base image) switches from Ubuntu's `docker.io` to Docker CE + buildx + compose plugins from download.docker.com, with in-bake `docker buildx version` / `docker compose version` checks so a broken plugin install fails the bake before `BAKE-OK`; `scripts/docker/Dockerfile` (dind stage) adds `docker-compose-plugin` to its existing Docker-repo install line. No Go code changes — the scripts are embedded verbatim via `scripts/embed.go`.

**Tech Stack:** Bash (cloud-init runcmd script), Dockerfile, Docker apt repo. Linting via lefthook: `shfmt` + `shellcheck` on `.sh`, commitlint on messages.

**Spec:** `docs/superpowers/specs/2026-07-05-runner-image-docker-compose-design.md`

## Global Constraints

- Docker packages come from Docker's official apt repo (`https://download.docker.com/linux/ubuntu`), not Ubuntu's archive, on both images.
- Package set: `docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin` (QEMU image); dind image adds only `docker-compose-plugin` to its existing list.
- No hyphenated `docker-compose` shim — `docker compose` only (matches GitHub-hosted ubuntu-24.04).
- Slim image stays Docker-free; do not touch `entrypoint-slim.sh` or the `slim` Dockerfile stage.
- No version pinning; images track latest at each bake.
- Conventional commits (commitlint enforced); work on branch `feat/runner-image-docker-compose`.
- All commands run from the repo root `/home/a1678991/dev/github-runner`.

---

### Task 1: QEMU base image — Docker CE + plugins in `bake.sh`

**Files:**
- Modify: `scripts/guest/bake.sh:35-40`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: a bake script whose success (the `BAKE-OK` sentinel) now guarantees `docker buildx version` and `docker compose version` work in the image. Task 4 relies on that guarantee.

**Context for the implementer:** This script runs as root inside a one-shot Ubuntu 24.04 VM during `refresh-image`. It is embedded into the Go binary (`scripts/embed.go`) and delivered via cloud-init; any non-zero exit aborts before `echo "BAKE-OK"` and the host rejects the bake, keeping the previous image in service. There is no unit-test harness for shell in this repo — verification is shellcheck/shfmt plus the end-to-end bake (Task 4).

- [ ] **Step 1: Replace the Docker install block**

In `scripts/guest/bake.sh`, replace exactly these lines (currently lines 35–40):

```bash
apt-get update
apt-get install -y --no-install-recommends \
  git curl ca-certificates jq build-essential sudo docker.io unzip
echo 'APT::Get::Assume-Yes "true";' >/etc/apt/apt.conf.d/90assume-yes
usermod -aG docker runner
systemctl enable docker
```

with:

```bash
apt-get update
apt-get install -y --no-install-recommends \
  git curl ca-certificates jq build-essential sudo unzip
echo 'APT::Get::Assume-Yes "true";' >/etc/apt/apt.conf.d/90assume-yes

# Docker CE from Docker's official repo — same distribution as the docker
# backend's dind image, and the only one shipping the buildx + compose v2
# CLI plugins (Ubuntu's docker.io has neither). Codename and arch are read
# from the guest so a non-noble imagebake.image_url keeps working.
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
# shellcheck disable=SC1091
. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
  >/etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y --no-install-recommends \
  docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
usermod -aG docker runner
systemctl enable docker

# Plugin discovery must work or the bake fails (no BAKE-OK): these are the
# commands jobs will run.
docker buildx version
docker compose version
```

Notes:
- `git curl ca-certificates … unzip` line: only `docker.io` is removed; curl and ca-certificates must be installed before fetching the GPG key, which is why the Docker repo setup comes after this first install.
- `# shellcheck disable=SC1091` is required: shellcheck cannot follow `/etc/os-release` and the repo's pre-commit hook runs shellcheck with default severity (same pattern as the existing directive at the `source /run/bake-env` line).
- Do not touch anything else in the file (trap, runner user, tarball install, `cloud-init clean`, `BAKE-OK`).

- [ ] **Step 2: Lint the script**

Run:
```bash
mise exec -- shfmt -d scripts/guest/bake.sh && mise exec -- shellcheck scripts/guest/bake.sh
```
Expected: no output from either (exit 0). If shfmt prints a diff, apply it with `mise exec -- shfmt -w scripts/guest/bake.sh` and re-run.

- [ ] **Step 3: Confirm the embed still builds and tests pass**

Run:
```bash
go build ./... && go test ./scripts/... ./internal/imagebake/...
```
Expected: `ok` for both packages (the scripts are embedded via `go:embed`; no test pins package names, so nothing should need updating — if a test fails, stop and report rather than editing tests to fit).

- [ ] **Step 4: Commit**

```bash
git add scripts/guest/bake.sh
git commit -m "feat(bake): install Docker CE with buildx and compose v2 plugins

Jobs on the QEMU backend get docker compose and BuildKit-native docker
build, matching GitHub-hosted ubuntu-24.04 runners and the dind image's
package source. The bake now fails before BAKE-OK if the CLI cannot
discover either plugin.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```
Expected: lefthook pre-commit passes (shfmt, shellcheck, secretlint) and commitlint accepts the message.

---

### Task 2: dind image — add `docker-compose-plugin`

**Files:**
- Modify: `scripts/docker/Dockerfile:41-42`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: the `dind` build target (`ghq-runner-base:latest`) ships the compose v2 plugin. Task 4 verifies it via `docker run`.

**Context for the implementer:** This Dockerfile is embedded (`scripts/embed.go`) and built locally by `refresh-image` for `backend: docker` pools with `isolation: gvisor`. The `dind` stage already installs Docker CE + buildx from Docker's official repo; only compose is missing. The `slim` stage must stay Docker-free by design.

- [ ] **Step 1: Add the package**

In `scripts/docker/Dockerfile`, in the `FROM base AS dind` stage, replace:

```dockerfile
    apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin && \
```

with:

```dockerfile
    apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin && \
```

No other lines change. Do not add compose to the `base` or `slim` stages.

- [ ] **Step 2: Confirm the embed still builds and tests pass**

Run:
```bash
go build ./... && go test ./scripts/... ./internal/dockerbackend/...
```
Expected: `ok` for both packages.

- [ ] **Step 3: Commit**

```bash
git add scripts/docker/Dockerfile
git commit -m "feat(docker): add compose v2 plugin to the dind runner image

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```
Expected: lefthook pre-commit and commitlint pass.

---

### Task 3: Documentation

**Files:**
- Modify: `README.md:41-43` and `README.md:50-52`
- Modify: `packaging/arch/src/github-qemu-runner-git/README.md:22-24`

**Interfaces:**
- Consumes: nothing from other tasks (text-only).
- Produces: nothing other tasks rely on.

- [ ] **Step 1: Update the main README**

In `README.md`, replace:

```markdown
`refresh-image` bakes the base image: Ubuntu 24.04 cloud image + Docker +
actions-runner (latest, checksum-verified), flattened to
`/var/lib/github-qemu-runner/images/base.qcow2`.
```

with:

```markdown
`refresh-image` bakes the base image: Ubuntu 24.04 cloud image + Docker CE
(with the buildx and compose v2 plugins, so `docker build` and
`docker compose` work in jobs) + actions-runner (latest,
checksum-verified), flattened to
`/var/lib/github-qemu-runner/images/base.qcow2`.
```

and replace:

```markdown
Jobs keep full Docker support: a private dockerd runs *inside* the
sandboxed container (DinD), so `container:` jobs, service containers, and
`docker build` work as on the QEMU backend.
```

with:

```markdown
Jobs keep full Docker support: a private dockerd runs *inside* the
sandboxed container (DinD), so `container:` jobs, service containers,
`docker build`, and `docker compose` work as on the QEMU backend.
```

- [ ] **Step 2: Update the Arch packaging README**

In `packaging/arch/src/github-qemu-runner-git/README.md`, replace:

```markdown
`refresh-image` bakes the base image: Ubuntu 24.04 cloud image + Docker +
actions-runner (latest, checksum-verified), flattened to
`/var/lib/github-qemu-runner/images/base.qcow2`.
```

with the same replacement text as the main README's first edit (identical five lines).

- [ ] **Step 3: Commit**

```bash
git add README.md packaging/arch/src/github-qemu-runner-git/README.md
git commit -m "docs: note buildx and compose v2 in runner image descriptions

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```
Expected: lefthook pre-commit and commitlint pass.

---

### Task 4: End-to-end verification (no commit)

**Files:**
- Create (scratch only, outside the repo): `$SCRATCH/e2e/config.yaml`, `$SCRATCH/e2e/app-key.pem`

**Interfaces:**
- Consumes: the Task 1 guarantee that `BAKE-OK` implies working plugins, and the Task 2 image tag `ghq-runner-base:latest`.
- Produces: evidence for the completion claim (bake exit 0 + `base.qcow2` present + compose/buildx versions from the dind image).

**Context for the implementer:** `refresh-image` loads a config, then bakes `base.qcow2` (needs `qemu-system-x86_64`, `qemu-img`, `genisoimage`, writable `/dev/kvm`, ~4 GB free RAM) and the dind image (needs a reachable docker daemon) for whichever backends the pools declare. GitHub App credentials are validated for presence only — baking never authenticates — so a throwaway key file works. The QEMU bake downloads a ~600 MB cloud image and runs apt inside the VM: expect 10–20 minutes total; run it in the background, not with a foreground timeout. Warning: the dind bake retags `ghq-runner-base:latest` on this host — fine on a dev machine, but say so in the report.

- [ ] **Step 1: Preflight host prerequisites**

```bash
command -v qemu-system-x86_64 qemu-img genisoimage docker \
  && test -w /dev/kvm && echo KVM-OK \
  && docker info >/dev/null 2>&1 && echo DOCKER-OK
```
Expected: paths for all four binaries, then `KVM-OK` and `DOCKER-OK`. If any prerequisite is missing, skip the corresponding half of the verification (QEMU half needs qemu+kvm+genisoimage, dind half needs docker), report exactly what was skipped and why, and continue with the rest.

- [ ] **Step 2: Write a minimal throwaway config**

`SCRATCH` is the session scratchpad directory. Include the qemu pool only if the QEMU preflight passed, and the docker pool only if the docker preflight passed:

```bash
S="$SCRATCH/e2e" && mkdir -p "$S/images" "$S/run"
openssl genrsa -out "$S/app-key.pem" 2048
cat >"$S/config.yaml" <<EOF
github:
  app_id: 1
  installation_id: 1
  private_key_path: $S/app-key.pem
state_dir: $S
paths:
  images: $S/images
  run: $S/run
pools:
  - name: e2e-qemu
    scope: org
    org: example
    count: 1
    cpus: 2
    memory_mb: 2048
    disk_gb: 20
    labels: [self-hosted, e2e]
  - name: e2e-dind
    backend: docker
    isolation: gvisor
    scope: org
    org: example
    count: 1
    cpus: 2
    memory_mb: 2048
    disk_gb: 20
    labels: [self-hosted, e2e-dind]
EOF
```

- [ ] **Step 3: Run the bake in the background**

```bash
go run ./cmd/github-qemu-runner -config "$S/config.yaml" refresh-image
```
Run this as a background task and monitor its output. While the QEMU bake runs, progress is visible in `$S/images/bake/console.log` (the VM's serial console — the `docker buildx version` / `docker compose version` lines from Task 1 appear there).
Expected on success: exit 0; log lines `bake complete` and `runner images built`.

- [ ] **Step 4: Assert the artifacts**

```bash
test -s "$S/images/base.qcow2" && echo QCOW2-OK
cat "$S/images/base.json" "$S/images/docker-base.json"
docker run --rm --entrypoint /usr/bin/docker ghq-runner-base:latest compose version
docker run --rm --entrypoint /usr/bin/docker ghq-runner-base:latest buildx version
```
Expected: `QCOW2-OK`; both provenance sidecars show today's `baked_at` and a `runner_version`; the two `docker run` commands print `Docker Compose version v2.…` and `github.com/docker/buildx v0.…`. (`docker compose version` and `docker buildx version` are daemon-less CLI plugin calls, so no dockerd is needed inside the container.)

- [ ] **Step 5: Report**

No commit. Summarize: what ran, the exact version strings observed, anything skipped in preflight, and that `ghq-runner-base:latest` on this host was replaced by the freshly baked image.

---

## Self-Review Notes

- Spec coverage: bake.sh switch (Task 1), dind plugin (Task 2), slim untouched (global constraint), docs including packaging README (Task 3), verification incl. e2e (Tasks 1–4). The spec's "manual" e2e is automated here via a throwaway config; the in-bake version checks implement the spec's "verify inside the guest" requirement permanently.
- No placeholders; every edit shows exact before/after text.
- No cross-task type dependencies (no Go changes); the only cross-task contracts are the `BAKE-OK`-implies-plugins guarantee and the `ghq-runner-base:latest` tag, both stated in Interfaces blocks.
