# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities **privately** via GitHub's
[private vulnerability reporting](https://github.com/a1678991/github-qemu-runner/security/advisories/new)
on this repository. Do not open a public issue for security reports.

If private reporting is unavailable, email the maintainer at the address on
their GitHub profile, with `[github-qemu-runner security]` in the subject.

Please include:

- the version (release tag, commit, or `.deb`/`.pkg.tar.zst` filename),
- the host backend (`qemu`, or `docker` with `isolation: gvisor|seccomp`),
- the smallest reproducer you can share, and
- the expected vs. observed behavior.

Expect an acknowledgement within a week. There is no SLA for fixes — this
is a hobby project with one maintainer — but credible reports will be
triaged and addressed.

## Scope

In scope:

- Sandbox escapes from a guest VM/container to the host beyond what
  `README.md` and the design docs already document as the isolation level
  (e.g. a runc-runtime job *should* be assumed root-equivalent; an escape
  from a gVisor-isolated docker pool *is* in scope).
- Credential leaks (App private key, installation tokens) into job logs,
  workdirs, runner records, or other observable state.
- Privilege escalation paths between the `gh-runner` host user and other
  unprivileged users on the same host.
- Crash/DoS paths reachable from a malicious workflow that affect other
  pools or the controller itself.

Out of scope:

- Risks the README already calls out as inherent: VM/container escapes
  from a kernel 0-day in a seccomp pool, attaching runners to public
  repositories (fork-PR risk), `docker.runtime: runc` removing the
  sandbox, `disk_gb` not being enforced on docker pools.
- Vulnerabilities in upstream components (QEMU, Docker, gVisor, the Go
  toolchain, the actions-runner agent, the Ubuntu cloud image) — please
  report those to the respective projects. If the integration here makes
  an upstream issue substantially worse, that *is* in scope.

## Supported versions

Only the latest tagged release is supported. Fixes are not backported.

## Hardening recommendations

The `README.md` "Security notes" section and the docker-backend
isolation-ladder documentation describe the intended operating posture —
treat deviations from it as your own risk surface, not a bug in this
project.
