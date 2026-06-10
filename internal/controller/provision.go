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
// under RunDir and boots the VM.
type QEMUProvisioner struct {
	RunDir   string // <state>/run
	BasePath string // <state>/images/base.qcow2, absolute
	QEMUBin  string
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

	overlay := filepath.Join(dir, "overlay.qcow2")
	if err := qemu.CreateOverlay(ctx, q.BasePath, overlay, p.DiskGB); err != nil {
		return fail(err)
	}
	ud, err := seed.UserData(jitConfig)
	if err != nil {
		return fail(err)
	}
	iso, err := seed.BuildISO(ctx, dir, ud, seed.MetaData(name, name))
	if err != nil {
		return fail(err)
	}
	vm, err := qemu.Start(ctx, q.QEMUBin, qemu.Spec{
		Name:        name,
		CPUs:        p.CPUs,
		MemoryMB:    p.MemoryMB,
		OverlayPath: overlay,
		SeedISOPath: iso,
		ConsoleLog:  filepath.Join(dir, "console.log"),
		QMPSocket:   filepath.Join(dir, "qmp.sock"),
		PIDFile:     filepath.Join(dir, "qemu.pid"),
	})
	if err != nil {
		return fail(fmt.Errorf("start VM: %w", err))
	}
	return vm, cleanup, nil
}
