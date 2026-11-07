#!/usr/bin/env bash
# Run a command against the C: drive of a Windows job VM, read-only, from the
# runner host. Works on a running VM: qemu-nbd opens the overlay read-only with
# force-share=on and the guest is not touched. Files the guest changed in the
# last few seconds may read as stale, since qemu has not flushed them.
#
#   sudo bash win-guest-fs.sh <run-dir>/<vm-name>/overlay.qcow2 'find actions-runner/_diag'
#
# The command runs through bash with its working directory at the root of C:.
# Loads the nbd and ntfs3 modules if needed, and unloads the ones it loaded.
set -euo pipefail

overlay=$1
command=$2

mnt=$(mktemp -d /run/win-guest-fs.XXXXXX)
dev=
loaded_nbd=
loaded_ntfs3=

cleanup() {
  mountpoint -q "$mnt" && umount "$mnt"
  [ -n "$dev" ] && qemu-nbd -d "$dev" >/dev/null
  rmdir "$mnt"
  sleep 1
  [ -n "$loaded_ntfs3" ] && modprobe -r ntfs3
  [ -n "$loaded_nbd" ] && modprobe -r nbd
  return 0
}
trap cleanup EXIT

if ! lsmod | grep -q '^nbd '; then
  modprobe nbd max_part=16
  loaded_nbd=1
fi
if ! lsmod | grep -q '^ntfs3 '; then
  modprobe ntfs3
  loaded_ntfs3=1
fi

for candidate in /sys/class/block/nbd*; do
  name=${candidate##*/}
  case $name in *p*) continue ;; esac
  if [ "$(cat "$candidate/size")" = 0 ]; then
    dev=/dev/$name
    break
  fi
done
[ -n "$dev" ] || {
  echo "no free nbd device" >&2
  exit 1
}

qemu-nbd --read-only --connect="$dev" --image-opts \
  "driver=qcow2,read-only=on,force-share=on,file.driver=file,file.filename=$overlay"
udevadm settle

# --connect can return before the kernel has scanned the partition table.
# C: is the largest partition on the disk.
part=
for _ in $(seq 30); do
  udevadm settle
  part=$(lsblk -rnbo NAME,SIZE,TYPE "$dev" | awk '$3 == "part" { print $2, $1 }' | sort -n | tail -1 | cut -d' ' -f2)
  [ -n "$part" ] && break
  sleep 1
done
[ -n "$part" ] || {
  echo "no partitions found on $dev" >&2
  lsblk "$dev" >&2
  exit 1
}

mount -t ntfs3 -o ro "/dev/$part" "$mnt"
cd "$mnt"
bash -c "$command"
cd /
