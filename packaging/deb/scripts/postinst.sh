#!/bin/sh
# Debian has no pacman-style sysusers/tmpfiles install hooks, so apply the
# packaged fragments here. Guards keep installs inside minimal containers
# (no systemd) working. The service is never enabled or started here —
# config.yaml must exist first; the operator runs `systemctl enable --now`.
set -e

case "$1" in
configure)
  if command -v systemd-sysusers >/dev/null 2>&1; then
    systemd-sysusers github-qemu-runner.conf
  fi
  if command -v systemd-tmpfiles >/dev/null 2>&1; then
    systemd-tmpfiles --create github-qemu-runner.conf
  fi
  if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
  fi
  ;;
esac
