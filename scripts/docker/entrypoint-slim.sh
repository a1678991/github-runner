#!/usr/bin/env bash
# PID 1 of the ephemeral job container for isolation: seccomp pools (run
# with --runtime=runc, NO --privileged — Docker's default seccomp profile
# applies). No inner Docker Engine: jobs that need Docker belong on a
# gvisor pool. Runs exactly one GitHub Actions job as the unprivileged
# "runner" user; the container exits when the job finishes — host-side
# teardown depends on that exit.
set -uo pipefail

JIT_FILE=/jit/config
if [ ! -s "$JIT_FILE" ]; then
  echo "entrypoint: $JIT_FILE missing or empty" >&2
  exit 1
fi

cd /opt/actions-runner || exit 1
# The JIT config registers a pre-created ephemeral runner; run.sh executes
# one job, deregisters, and exits. The blob is single-use, so its
# visibility in container-internal argv is harmless.
exec runuser -u runner -- ./run.sh --jitconfig "$(cat "$JIT_FILE")"
