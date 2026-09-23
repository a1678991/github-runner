// Package qemu creates copy-on-write disks and supervises
// qemu-system-x86_64 child processes. One VM = one process; guest poweroff
// (with -no-reboot) makes the process exit, which is the job-done signal.
package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Firmware is a UEFI firmware pair. Vars must be a per-VM writable copy
// (OVMF writes boot entries into it).
type Firmware struct {
	Code string
	Vars string
}

// Disk is an additional virtio-blk disk (raw or qcow2).
type Disk struct {
	Path   string
	Format string
}

type Spec struct {
	Name        string
	CPUs        int
	MemoryMB    int
	OverlayPath string
	SeedISOPath string
	ConsoleLog  string
	QMPSocket   string
	PIDFile     string

	// Firmware selects UEFI (OVMF) when non-nil; nil keeps SeaBIOS.
	Firmware *Firmware
	// DiskBus: "" keeps the legacy if=virtio drive (Linux); "virtio" is
	// an explicit virtio-blk-pci device with bootindex=0; "ahci" is a
	// SATA disk on ide.0 (Windows bake, before viostor is installed).
	DiskBus string
	// SeedBus: "" keeps the legacy if=virtio raw drive; "ahci" attaches
	// the seed ISO as a SATA CD-ROM on ide.1 (Windows has no virtio-blk
	// driver for the seed before the bake, and CD-ROM is what Windows
	// searches for Unattend.xml).
	SeedBus string
	// CDROMs are extra ISOs attached as SATA CD-ROMs on ide.2, ide.3, ...
	CDROMs []string
	// ExtraDisks are attached as virtio-blk-pci devices (the bake's dummy
	// disk that makes viostor bind as a boot-start driver).
	ExtraDisks []Disk
	// HyperV enables the Hyper-V enlightenments Windows guests expect.
	HyperV bool
	// AllowReboot omits -no-reboot. Only the Windows bake sets it: OOBE
	// reboots between the specialize and oobeSystem passes. Job VMs keep
	// -no-reboot so a guest reboot ends the job instead of looping.
	AllowReboot bool
}

// Args builds the qemu-system-x86_64 argv. -display none (not -nographic:
// that would redirect the serial port to stdio and fight -serial file:).
// With every optional Spec field zero the result is the historical Linux
// argv, byte for byte.
func Args(s Spec) []string {
	cpu := "host"
	if s.HyperV {
		cpu += ",hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time"
	}
	args := []string{
		"-accel", "kvm",
		"-cpu", cpu,
		"-machine", "q35",
		"-smp", strconv.Itoa(s.CPUs),
		"-m", strconv.Itoa(s.MemoryMB),
	}
	if s.Firmware != nil {
		args = append(args,
			"-drive", "if=pflash,format=raw,readonly=on,file="+s.Firmware.Code,
			"-drive", "if=pflash,format=raw,file="+s.Firmware.Vars)
	}
	switch s.DiskBus {
	case "virtio":
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=boot,format=qcow2", s.OverlayPath),
			"-device", "virtio-blk-pci,drive=boot,bootindex=0")
	case "ahci":
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=boot,format=qcow2", s.OverlayPath),
			"-device", "ide-hd,drive=boot,bus=ide.0,bootindex=0")
	default:
		args = append(args, "-drive", fmt.Sprintf("file=%s,if=virtio,format=qcow2", s.OverlayPath))
	}
	for i, d := range s.ExtraDisks {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=extra%d,format=%s", d.Path, i, d.Format),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=extra%d", i))
	}
	if s.SeedBus == "ahci" {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=seed,format=raw,readonly=on,media=cdrom", s.SeedISOPath),
			"-device", "ide-cd,drive=seed,bus=ide.1")
	} else {
		args = append(args, "-drive", fmt.Sprintf("file=%s,if=virtio,format=raw,readonly=on", s.SeedISOPath))
	}
	for i, iso := range s.CDROMs {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=none,id=cd%d,format=raw,readonly=on,media=cdrom", iso, i),
			"-device", fmt.Sprintf("ide-cd,drive=cd%d,bus=ide.%d", i, i+2))
	}
	args = append(args,
		"-netdev", "user,id=n0",
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none",
		"-serial", "file:"+s.ConsoleLog,
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", s.QMPSocket),
		"-pidfile", s.PIDFile,
	)
	if !s.AllowReboot {
		args = append(args, "-no-reboot")
	}
	return append(args, "-name", s.Name)
}

// CreateOverlay makes a qcow2 overlay backed by base (which must be an
// absolute path — qemu resolves relative backing paths against the overlay's
// directory) in backingFormat ("qcow2", "vhdx", ...) and grows its virtual
// size to diskGB when that exceeds the backing image's size. The backing
// image sets the floor: qemu-img refuses to shrink without --shrink, and
// shrinking a CoW view of a filesystem would corrupt it anyway. The guest's
// cloud-init growpart expands the root filesystem into any new space on boot.
func CreateOverlay(ctx context.Context, base, backingFormat, dest string, diskGB int) error {
	if out, err := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2", "-b", base, "-F", backingFormat, dest).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create %s: %v: %s", dest, err, out)
	}
	cur, err := virtualSize(ctx, dest)
	if err != nil {
		_ = os.Remove(dest)
		return err
	}
	if int64(diskGB)*1024*1024*1024 <= cur {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "qemu-img", "resize",
		dest, fmt.Sprintf("%dG", diskGB)).CombinedOutput(); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("qemu-img resize %s: %v: %s", dest, err, out)
	}
	return nil
}

// virtualSize reads a qcow2 image's virtual size in bytes.
func virtualSize(ctx context.Context, path string) (int64, error) {
	out, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", path).Output()
	if err != nil {
		return 0, fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, fmt.Errorf("parse qemu-img info for %s: %w", path, err)
	}
	return info.VirtualSize, nil
}

// CopyFile copies src to dst (mode 0600, parent created). Used for the
// per-VM OVMF variable store: the firmware writes to it, so VMs must
// not share the distro's pristine file.
func CopyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
