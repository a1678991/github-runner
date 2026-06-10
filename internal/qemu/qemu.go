// Package qemu creates copy-on-write disks and supervises
// qemu-system-x86_64 child processes. One VM = one process; guest poweroff
// (with -no-reboot) makes the process exit, which is the job-done signal.
package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

type Spec struct {
	Name        string
	CPUs        int
	MemoryMB    int
	OverlayPath string
	SeedISOPath string
	ConsoleLog  string
	QMPSocket   string
	PIDFile     string
}

// Args builds the qemu-system-x86_64 argv. -display none (not -nographic:
// that would redirect the serial port to stdio and fight -serial file:).
func Args(s Spec) []string {
	return []string{
		"-accel", "kvm",
		"-cpu", "host",
		"-machine", "q35",
		"-smp", strconv.Itoa(s.CPUs),
		"-m", strconv.Itoa(s.MemoryMB),
		"-drive", fmt.Sprintf("file=%s,if=virtio,format=qcow2", s.OverlayPath),
		"-drive", fmt.Sprintf("file=%s,if=virtio,format=raw,readonly=on", s.SeedISOPath),
		"-netdev", "user,id=n0",
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none",
		"-serial", "file:" + s.ConsoleLog,
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", s.QMPSocket),
		"-pidfile", s.PIDFile,
		"-no-reboot",
		"-name", s.Name,
	}
}

// CreateOverlay makes a qcow2 overlay backed by base (which must be an
// absolute path — qemu resolves relative backing paths against the overlay's
// directory) and grows its virtual size to diskGB. The guest's cloud-init
// growpart expands the root filesystem into the new space on boot.
func CreateOverlay(ctx context.Context, base, dest string, diskGB int) error {
	if out, err := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2", "-b", base, "-F", "qcow2", dest).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create %s: %v: %s", dest, err, out)
	}
	if out, err := exec.CommandContext(ctx, "qemu-img", "resize",
		dest, fmt.Sprintf("%dG", diskGB)).CombinedOutput(); err != nil {
		_ = os.Remove(dest) // best-effort cleanup; resize failure is the real error
		return fmt.Errorf("qemu-img resize %s: %v: %s", dest, err, out)
	}
	return nil
}
