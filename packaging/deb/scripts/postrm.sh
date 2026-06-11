#!/bin/sh
# Drop the removed unit from systemd's view. The gh-runner user and
# /var/lib/github-qemu-runner are left behind on purpose: baked images and
# the state dir survive reinstalls, matching the Arch package.
set -e

case "$1" in
remove | purge)
  if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
  fi
  ;;
esac
