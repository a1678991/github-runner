// Package controller wires config, GitHub client, provisioning, and pools
// into the running daemon.
package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/pool"
	"github.com/a1678991/github-qemu-runner/internal/qemu"
	"github.com/a1678991/github-qemu-runner/internal/seed"
)

// QEMUProvisioner builds a per-VM working directory (overlay + seed ISO)
// under RunDir and boots the VM. Linux pools use BasePath and cloud-init;
// Windows pools use WindowsBasePath, OVMF, and a plain seed CD.
type QEMUProvisioner struct {
	RunDir   string // <Paths.Run>
	BasePath string // <Paths.Images>/base.qcow2, absolute
	QEMUBin  string

	WindowsBasePath string         // <Paths.Images>/base-windows.qcow2, absolute; empty without windows pools
	Firmware        *qemu.Firmware // pristine OVMF pair; each VM copies Vars
}

func (q *QEMUProvisioner) Provision(ctx context.Context, name string, p config.Pool, jitConfig string) (pool.VM, func(), error) {
	dir := filepath.Join(q.RunDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fail := func(err error) (pool.VM, func(), error) {
		cleanup()
		return nil, nil, err
	}

	spec := qemu.Spec{
		Name:        name,
		CPUs:        p.CPUs,
		MemoryMB:    p.MemoryMB,
		OverlayPath: filepath.Join(dir, "overlay.qcow2"),
		ConsoleLog:  filepath.Join(dir, "console.log"),
		QMPSocket:   filepath.Join(dir, "qmp.sock"),
		PIDFile:     filepath.Join(dir, "qemu.pid"),
	}
	var err error
	if p.OS == "windows" {
		err = q.prepareWindows(ctx, dir, p, jitConfig, &spec)
	} else {
		err = q.prepareLinux(ctx, dir, name, p, jitConfig, &spec)
	}
	if err != nil {
		return fail(err)
	}
	vm, err := qemu.Start(ctx, q.QEMUBin, spec)
	if err != nil {
		return fail(fmt.Errorf("start VM: %w", err))
	}
	return vm, cleanup, nil
}

func (q *QEMUProvisioner) prepareLinux(ctx context.Context, dir, name string, p config.Pool, jitConfig string, spec *qemu.Spec) error {
	if err := qemu.CreateOverlay(ctx, q.BasePath, "qcow2", spec.OverlayPath, p.DiskGB); err != nil {
		return err
	}
	ud, err := seed.UserData(jitConfig)
	if err != nil {
		return err
	}
	iso, err := seed.BuildISO(ctx, dir, ud, seed.MetaData(name, name))
	if err != nil {
		return err
	}
	spec.SeedISOPath = iso
	return nil
}

// prepareWindows: virtio-blk boot disk (viostor is boot-start in the
// baked image), virtio-net, the JIT blob on a SATA CD-ROM that the baked
// run-one-job task reads by volume label, UEFI with a private NVRAM copy.
func (q *QEMUProvisioner) prepareWindows(ctx context.Context, dir string, p config.Pool, jitConfig string, spec *qemu.Spec) error {
	if q.WindowsBasePath == "" || q.Firmware == nil {
		return fmt.Errorf("windows pools are not initialised (no base image or firmware)")
	}
	if err := qemu.CreateOverlay(ctx, q.WindowsBasePath, "qcow2", spec.OverlayPath, p.DiskGB); err != nil {
		return err
	}
	iso, err := seed.BuildISOFiles(ctx, dir, "GHQSEED", map[string]string{"runner-jit.conf": jitConfig})
	if err != nil {
		return err
	}
	vars := filepath.Join(dir, "vars.fd")
	if err := qemu.CopyFile(q.Firmware.Vars, vars); err != nil {
		return fmt.Errorf("copy OVMF vars: %w", err)
	}
	spec.SeedISOPath = iso
	spec.Firmware = &qemu.Firmware{Code: q.Firmware.Code, Vars: vars}
	spec.DiskBus = "virtio"
	spec.SeedBus = "ahci"
	spec.HyperV = true
	return nil
}
