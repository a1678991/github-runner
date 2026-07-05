# Docker Compose (and buildx) in the job images

## Problem

Jobs cannot use `docker compose` on either docker-capable runner image:

- The QEMU base image (`scripts/guest/bake.sh`) installs Ubuntu's
  `docker.io` package — Docker Engine only. No compose plugin, and no
  buildx either, so `docker build` falls back to the deprecated legacy
  builder (no BuildKit features).
- The docker-backend dind image (`scripts/docker/Dockerfile`, target
  `dind`) installs `docker-ce` + `docker-buildx-plugin` from Docker's
  official apt repo, but not `docker-compose-plugin`.

GitHub-hosted `ubuntu-24.04` runners ship Docker CE with both the
compose v2 and buildx plugins; workflows using `docker compose` fail on
these self-hosted images when they would pass on hosted runners.

## Goals

- `docker compose` (v2 plugin) works in jobs on the QEMU backend and on
  `isolation: gvisor` docker pools.
- BuildKit-native `docker build` (buildx) on the QEMU backend, matching
  what the dind image already has.
- Both images install Docker from the same distribution (Docker's
  official apt repo) so versions and behaviour stay aligned.

## Non-goals

- The legacy hyphenated `docker-compose` command. Hosted `ubuntu-24.04`
  runners dropped it; command-not-found is the accurate signal to
  migrate a workflow to `docker compose`.
- Docker of any kind in the slim image (`isolation: seccomp` pools) —
  shipping no Engine there is a deliberate design decision
  (2026-06-13 seccomp isolation design).
- Pinning Docker package versions. Images track latest at each bake,
  same policy as the actions-runner tarball.

## Design

### QEMU base image — `scripts/guest/bake.sh`

Switch from Ubuntu's `docker.io` to Docker's official apt repo,
mirroring the repo-setup pattern already in the dind Dockerfile:

1. First `apt-get install` line keeps
   `git curl ca-certificates jq build-essential sudo unzip` and drops
   `docker.io` (curl + ca-certificates must be present before fetching
   the Docker GPG key).
2. Repo setup:

   ```sh
   install -m 0755 -d /etc/apt/keyrings
   curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
     -o /etc/apt/keyrings/docker.asc
   . /etc/os-release
   echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
     >/etc/apt/sources.list.d/docker.list
   apt-get update
   apt-get install -y --no-install-recommends \
     docker-ce docker-ce-cli containerd.io \
     docker-buildx-plugin docker-compose-plugin
   ```

   `$VERSION_CODENAME` from `/etc/os-release` rather than a hard-coded
   `noble`, so the script keeps working if `imagebake.image_url` is
   pointed at a different Ubuntu release. `dpkg --print-architecture`
   rather than a hard-coded `amd64` for the same reason (and parity
   with the Dockerfile).
3. `usermod -aG docker runner` and `systemctl enable docker` stay
   as-is (enable is redundant with docker-ce's postinst but harmless
   and explicit).

Failure handling is unchanged: any apt or network error aborts the
script before `BAKE-OK` is printed, the host rejects the bake, and the
previous `base.qcow2` stays in service.

### dind image — `scripts/docker/Dockerfile`

Add `docker-compose-plugin` to the existing dind-stage package list
(the `apt-get install` in the `FROM base AS dind` stage). Engine,
buildx, and the repo setup are already present.

### Slim image

Unchanged.

### Code touchpoints

None. The scripts are embedded verbatim (`scripts/embed.go`); no Go
code, config schema, or provenance sidecar (`base.json`,
`docker-base.json`) changes.

### Rollout

Existing deployments pick the change up automatically: images are
re-baked on controller start when stale and on the scheduled refresh
timer (2026-06-16 auto-image-refresh design). No migration step.

## Testing

- `shellcheck`/`shfmt` on `bake.sh` via lefthook (already wired; the
  scripts are kept as real `.sh` files exactly so linters cover them).
- `go test ./...` — embed tests only; no assertions pin package names.
- End-to-end (manual): run `github-qemu-runner refresh-image` with both
  variants, then verify inside each image that `docker compose version`
  and `docker buildx version` succeed and a job-level
  `docker compose up` works.

## Documentation

- `README.md`:
  - Line ~41 ("Ubuntu 24.04 cloud image + Docker + actions-runner") —
    mention Docker CE with compose v2 + buildx plugins.
  - Docker-backend section ("`container:` jobs, service containers, and
    `docker build` work…") — add `docker compose` to the list.
- ~~`packaging/arch/src/github-qemu-runner-git/README.md` line ~22 —
  same "bakes the base image" sentence, same update.~~
  **Correction (2026-07-05, discovered during execution):** that path is
  a gitignored makepkg build artifact (`packaging/arch/.gitignore`
  ignores `/src/`), not a tracked file. `packaging/arch/PKGBUILD`
  installs the repo-root `README.md` into the package at build time
  (`install -Dm644 README.md …`), so no second tracked README exists and
  the main-README edit above is sufficient.

## Open questions

None outstanding — design approved 2026-07-05 (scope: compose in job
images; source: Docker official repo; plugin only, no hyphen shim).
