#!/usr/bin/env bash
# Runs as root from cloud-init runcmd inside the ephemeral job VM.
# Runs exactly one GitHub Actions job as the unprivileged "runner" user,
# then powers the guest off NO MATTER WHAT — host-side teardown depends on
# the qemu process exiting.
set -uo pipefail
trap 'poweroff' EXIT

JIT_FILE=/etc/runner-jit.conf
if [ ! -s "$JIT_FILE" ]; then
  echo "run-one-job: $JIT_FILE missing or empty" >/dev/console
  exit 1
fi

cd /opt/actions-runner || exit 1
# The JIT config registers a pre-created ephemeral runner; run.sh executes
# one job, deregisters, and exits. The blob is single-use, so its visibility
# in guest argv is harmless (the guest is destroyed right after).
runuser -u runner -- ./run.sh --jitconfig "$(cat "$JIT_FILE")"
