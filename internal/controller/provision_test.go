package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

func TestProvision(t *testing.T) {
	for _, bin := range []string{"qemu-img", "genisoimage"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", base, "1G").CombinedOutput(); err != nil {
		t.Fatalf("create base: %v: %s", err, out)
	}
	fake := filepath.Join(dir, "fake-qemu")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")

	q := &QEMUProvisioner{RunDir: runDir, BasePath: base, QEMUBin: fake}
	pcfg := config.Pool{Name: "fmt", CPUs: 1, MemoryMB: 512, DiskGB: 10}
	vm, cleanup, err := q.Provision(context.Background(), "ghq-fmt-test", pcfg, "JITBLOB")
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(runDir, "ghq-fmt-test")
	for _, f := range []string{"overlay.qcow2", "seed.iso", "user-data", "meta-data"} {
		if _, err := os.Stat(filepath.Join(workdir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("fake qemu did not exit")
	}
	cleanup()
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Errorf("workdir not removed: %v", err)
	}
}

func TestProvisionFailureCleansUp(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	q := &QEMUProvisioner{
		RunDir:   runDir,
		BasePath: filepath.Join(dir, "missing-base.qcow2"), // overlay creation fails
		QEMUBin:  "/bin/false",
	}
	_, _, err := q.Provision(context.Background(), "ghq-x-y", config.Pool{DiskGB: 10}, "J")
	if err == nil {
		t.Fatal("want error")
	}
	if _, statErr := os.Stat(filepath.Join(runDir, "ghq-x-y")); !os.IsNotExist(statErr) {
		t.Error("failed provision left workdir behind")
	}
}
