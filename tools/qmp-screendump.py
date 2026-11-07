#!/usr/bin/env python3
"""Capture a running job VM's screen through its QMP socket, without touching the guest.

Windows job VMs log only a few lines to serial, so the desktop (where run.cmd's
console window lives) is the quickest view of what a stuck guest is doing.

Run on the runner host as the service user (the run directory is 0700):

    sudo -u gh-runner python3 qmp-screendump.py <run-dir>/<vm-name>

Writes screen.png into the VM's run directory and prints its path. QEMU itself
writes the file, so it must be a path the service's sandbox can write to; the
run directory is, and is removed with the VM.
"""
import json
import os
import socket
import sys


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    vm_dir = sys.argv[1]
    out = os.path.join(vm_dir, "screen.png")

    conn = socket.socket(socket.AF_UNIX)
    conn.settimeout(10)
    conn.connect(os.path.join(vm_dir, "qmp.sock"))
    stream = conn.makefile("rw")
    stream.readline()  # greeting

    for cmd in (
        {"execute": "qmp_capabilities"},
        {"execute": "screendump", "arguments": {"filename": out, "format": "png"}},
    ):
        stream.write(json.dumps(cmd) + "\n")
        stream.flush()
        # Asynchronous events can arrive before the command's own reply.
        while True:
            reply = json.loads(stream.readline())
            if "return" in reply:
                break
            if "error" in reply:
                print(f"QMP error: {reply['error']}", file=sys.stderr)
                return 1
    print(out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
