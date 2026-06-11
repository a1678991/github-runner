#!/bin/sh
# Stop the service only on full removal. Upgrades must never stop or
# restart it: a stop drains every runner slot (TimeoutStopSec=35m) — the
# operator decides when to restart onto the new binary.
set -e

case "$1" in
remove)
  if [ -d /run/systemd/system ]; then
    # deb-systemd-invoke respects policy-rc.d (chroot/image builds).
    if command -v deb-systemd-invoke >/dev/null 2>&1; then
      deb-systemd-invoke stop github-qemu-runner.service || true
    else
      systemctl stop github-qemu-runner.service || true
    fi
  fi
  ;;
esac
