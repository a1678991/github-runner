#!/usr/bin/env bash
# PID 1 of the ephemeral job container (run with --runtime=runsc
# --privileged). Starts the inner dockerd, then runs exactly one GitHub
# Actions job as the unprivileged "runner" user; the container exits when
# the job finishes — host-side teardown depends on that exit.
set -uo pipefail

JIT_FILE=/jit/config
if [ ! -s "$JIT_FILE" ]; then
  echo "entrypoint: $JIT_FILE missing or empty" >&2
  exit 1
fi

# gVisor exposes no netfilter to the sandbox: without --iptables=false the
# inner dockerd dies at boot ("Failed to initialize nft"). Validated on
# runsc release-20260601.0 / Docker 29.x.
dockerd --iptables=false --ip6tables=false >/var/log/dockerd.log 2>&1 &

for _ in $(seq 1 60); do
  docker info >/dev/null 2>&1 && break
  sleep 1
done
if ! docker info >/dev/null 2>&1; then
  echo "entrypoint: inner dockerd failed to start" >&2
  tail -n 50 /var/log/dockerd.log >&2
  exit 1
fi

cd /opt/actions-runner || exit 1
# The JIT config registers a pre-created ephemeral runner; run.sh executes
# one job, deregisters, and exits. The blob is single-use, so its
# visibility in container-internal argv is harmless.
# On `docker stop`, SIGTERM lands on runuser (PID 1 after exec) and its
# forwarding to the runner is best-effort; docker's TERM->KILL escalation
# is the safety net, same outcome as the qemu backend's drain timeout.
exec runuser -u runner -- ./run.sh --jitconfig "$(cat "$JIT_FILE")"
