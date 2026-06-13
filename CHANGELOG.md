# Changelog

## [0.3.0](https://github.com/a1678991/github-qemu-runner/compare/v0.2.0...v0.3.0) (2026-06-13)


### Features

* **bake:** build dind/slim image variants per configured isolation ([d95b436](https://github.com/a1678991/github-qemu-runner/commit/d95b436d0e887951cec9d4b1e4efd73af74d4529))
* **config:** per-pool isolation and seccomp_profile for docker pools ([6cda8c4](https://github.com/a1678991/github-qemu-runner/commit/6cda8c4f4e1ab9515df8173e555f78d9f69280a3))
* **controller:** isolation-aware image and seccomp-profile preflights ([1bc8a4f](https://github.com/a1678991/github-qemu-runner/commit/1bc8a4f8b3e2ad1cc771008addc329ccffb73c34))
* **dockerbackend:** seccomp-isolation run args and slim image tag ([dd30006](https://github.com/a1678991/github-qemu-runner/commit/dd3000684ae19b89581c1808bd341f9d2bd00e72))
* **images:** multi-stage Dockerfile with docker-engine-free slim variant ([87ae63d](https://github.com/a1678991/github-qemu-runner/commit/87ae63ddd1d3390fb18c5d3b6a4bdf3474ce21f6))
* seccomp isolation mode for docker pools ([6d58467](https://github.com/a1678991/github-qemu-runner/commit/6d58467912951e587cd7071594c786f9357bd9fa))
* **setup:** gate runsc check on gvisor pools; per-isolation connectivity checks ([857f9b5](https://github.com/a1678991/github-qemu-runner/commit/857f9b5b8e6076fd420b2a157808eae6d540070d))


### Bug Fixes

* **images:** grant runner passwordless sudo in both backends ([eb5196b](https://github.com/a1678991/github-qemu-runner/commit/eb5196b4f21ccc6cda8f4841e45ae5a1918a59ae))
* **images:** grant runner passwordless sudo in both backends ([86df3b3](https://github.com/a1678991/github-qemu-runner/commit/86df3b36bb56e56869e15419543faf04a48a49a5))

## [0.2.0](https://github.com/a1678991/github-qemu-runner/compare/v0.1.0...v0.2.0) (2026-06-11)


### Features

* **cli:** docker-aware refresh-image and setup preflights ([1d15637](https://github.com/a1678991/github-qemu-runner/commit/1d15637b4673541558f54608a7f94f6d0d3fdf1f))
* **config:** per-pool backend field and docker.runtime section ([3663e11](https://github.com/a1678991/github-qemu-runner/commit/3663e113a997340fec3fc6a81c2f671a6a56f13e))
* **controller:** per-backend preflight and provisioner wiring ([9500454](https://github.com/a1678991/github-qemu-runner/commit/9500454dc58b1be66ac1dd45ba86a610e1d11cde))
* Docker backend with gVisor-sandboxed ephemeral runners ([2c1f4b4](https://github.com/a1678991/github-qemu-runner/commit/2c1f4b47802cdcc362b211c52c74c6e5c1f7d759))
* **dockerbackend:** bake runner image via docker build ([dfa9ac7](https://github.com/a1678991/github-qemu-runner/commit/dfa9ac7d87e2d6e8f77e9c430e2a8dc353e0a8a2))
* **dockerbackend:** container supervisor implementing pool.VM ([197f473](https://github.com/a1678991/github-qemu-runner/commit/197f473ee2e59f46d5bb6227768157ef0e58ce57))
* **dockerbackend:** docker run argv builder and arch mapping ([45460f8](https://github.com/a1678991/github-qemu-runner/commit/45460f856a4d669e59b99303d0796fbd353c2091))
* **dockerbackend:** embedded runner-image Dockerfile and entrypoint ([d3ba3e9](https://github.com/a1678991/github-qemu-runner/commit/d3ba3e95a4f2d5013e9f90295c0344cb53eb5697))
* **dockerbackend:** orphan container reaping ([18af92c](https://github.com/a1678991/github-qemu-runner/commit/18af92c7916b711127e9d249bef25742299a2ccb))
* **dockerbackend:** per-job container provisioner ([b9ab8c3](https://github.com/a1678991/github-qemu-runner/commit/b9ab8c3ce58896bc5b4142d242a2dd1fbfd7d97f))
* **imagebake:** parameterize LatestRunner by runner arch ([f9f2b27](https://github.com/a1678991/github-qemu-runner/commit/f9f2b272c560aca85f858da7dc96899e51f0a795))
* **packaging:** deb package via nfpm (arm64 + amd64) ([1b687d8](https://github.com/a1678991/github-qemu-runner/commit/1b687d8e5a32a127a624bb57f90cd3d0c02a1e05))
* **packaging:** Debian package for Ubuntu 24.04 (arm64 + amd64) ([d0ee1cd](https://github.com/a1678991/github-qemu-runner/commit/d0ee1cdc4655f71358447ade1980252323aa45dd))


### Bug Fixes

* **dockerbackend:** bound Kill against hung daemon, surface docker wait stderr ([6665a50](https://github.com/a1678991/github-qemu-runner/commit/6665a50fed35413665b3740d6996b825cac808c2))
* **dockerbackend:** harden runner image per code review ([ba1f704](https://github.com/a1678991/github-qemu-runner/commit/ba1f70473939d7aaea38cdabfa8b1f9cecccca3a))
* **dockerbackend:** tighten Powerdown post-stop wait, document signal path ([c7b9917](https://github.com/a1678991/github-qemu-runner/commit/c7b9917dc99c7557af1e02bd0c7d0760bfed4bf3))
* **imagebake:** scrape tarball SHA from checksum markers, not first hex match ([ac035d3](https://github.com/a1678991/github-qemu-runner/commit/ac035d39fe24f73556dc5954b3bd83c50e7ba70c))

## 0.1.0 (2026-06-11)


### Features

* add Arch Linux -git PKGBUILD with sysusers and tmpfiles ([dbbfcc7](https://github.com/a1678991/github-qemu-runner/commit/dbbfcc77c31c9e700646701e4f9d62a157c3347c))
* add config package with validation and defaults ([4b0bd42](https://github.com/a1678991/github-qemu-runner/commit/4b0bd42be4b6c46113371b733fd1754b6bd0f9a4))
* add GitHub App JWT minting and key parsing ([403b3a0](https://github.com/a1678991/github-qemu-runner/commit/403b3a08e3ea68bf5ab8281cb4304f3ba7abfa60))
* add GitHub REST client with JIT config and runner CRUD ([59da395](https://github.com/a1678991/github-qemu-runner/commit/59da39578458421caea10cac81437561023c678f))
* add guest bake and run-one-job scripts with embeds ([5c465b9](https://github.com/a1678991/github-qemu-runner/commit/5c465b97e639aecff22cd7d27b30b16382b06af8))
* add image bake pipeline with checksum verification ([c3f3923](https://github.com/a1678991/github-qemu-runner/commit/c3f3923aaff281f9392fcb8ade66ef7bb0fc5c08))
* add Nix flake with buildGoModule package ([707fa7a](https://github.com/a1678991/github-qemu-runner/commit/707fa7a018cf0a9c47f19b6f35bb41da2a79699c))
* add NixOS module and flake checks ([15c759a](https://github.com/a1678991/github-qemu-runner/commit/15c759aa011c0b32bb017e4d5bcb2f44c04062cb))
* add NoCloud seed rendering and ISO builder ([693e4bf](https://github.com/a1678991/github-qemu-runner/commit/693e4bfbfb9a7db664989111142b13ae592848d4))
* add orphan reaping and controller wiring ([2195299](https://github.com/a1678991/github-qemu-runner/commit/219529956f30dea1d9819f19210d3a42359924e8))
* add pool slot supervisor with liveness gate and drain ([5145442](https://github.com/a1678991/github-qemu-runner/commit/5145442031f7d007a063c1c6bed7e42bafc83148))
* add qcow2 overlay creation and QEMU argv builder ([bd4629b](https://github.com/a1678991/github-qemu-runner/commit/bd4629b0d070d57da6c2c7a2262c7e3672e7756e))
* add QEMU provisioner composing overlay, seed, and boot ([7d9db54](https://github.com/a1678991/github-qemu-runner/commit/7d9db54986534cda2ae187ef9be8bdecbc09fbdb))
* add systemd unit, example config, and README ([5e43e0a](https://github.com/a1678991/github-qemu-runner/commit/5e43e0ac86673daf78622e574f8f0c9fac444131))
* add versioned release PKGBUILD ([a5bf30d](https://github.com/a1678991/github-qemu-runner/commit/a5bf30d73e66f4a4a1098894115845f5d1554417))
* add VM process supervision and QMP powerdown ([aa9b69a](https://github.com/a1678991/github-qemu-runner/commit/aa9b69a6268e6e1d379e1b574c210520fa7b9b28))
* Arch and Nix packaging ([328fa74](https://github.com/a1678991/github-qemu-runner/commit/328fa74504cd6a493dfaf5ad70059fe56b14edfd))
* ephemeral GitHub Actions runners on QEMU/KVM ([c5dfc38](https://github.com/a1678991/github-qemu-runner/commit/c5dfc38d58011ba20d71fcbba36361dbba6188fd))
* release-please pipeline with Arch package and tarball assets ([a910948](https://github.com/a1678991/github-qemu-runner/commit/a910948e8a5d80c2c515fe9b97a8e52a9521e47b))
* wire controller, refresh-image, and setup subcommands ([0cdf88e](https://github.com/a1678991/github-qemu-runner/commit/0cdf88e4b62dfc4c1fbc19ef969c812a79b889f8))


### Bug Fixes

* add JWT expiry headroom and per-label config validation ([d512c29](https://github.com/a1678991/github-qemu-runner/commit/d512c299f363402056dc57a89bb504b9f952df2b))
* disable Arch debug package splitting and pass file list to nix fmt ([52e5951](https://github.com/a1678991/github-qemu-runner/commit/52e59518b050d0771ae0df7bfa6675a0655fcdee))
* graceful drain on API errors and shutdown during liveness gate ([5846bbb](https://github.com/a1678991/github-qemu-runner/commit/5846bbbc3c92c1b669df34b49188edd555a13bb9))
* never shrink overlay below base image size; document LoadCredential scope ([c020f98](https://github.com/a1678991/github-qemu-runner/commit/c020f98ded7367b16f25510059f4529a0bb574b8))
* point packaged systemd unit at /usr/bin and guard in CI ([9df67ff](https://github.com/a1678991/github-qemu-runner/commit/9df67ff1f0c0d5d94e320c751a32cdb0895faaa2))
* restrict seed ISO permissions and close VM lifecycle test gaps ([7077044](https://github.com/a1678991/github-qemu-runner/commit/7077044f352c6a8b0c4a3a2e15a3a5133046a95b))
* use minor-only go directive and default buildGoModule phases ([0a9763e](https://github.com/a1678991/github-qemu-runner/commit/0a9763e595a6a39771772f8e4971efd19c2bfb73))
