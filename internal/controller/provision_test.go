package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/qemu"
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

func TestProvisionWindows(t *testing.T) {
	for _, bin := range []string{"qemu-img", "genisoimage"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base-windows.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", base, "1G").CombinedOutput(); err != nil {
		t.Fatalf("create base: %v: %s", err, out)
	}
	vars := filepath.Join(dir, "OVMF_VARS.fd")
	if err := os.WriteFile(vars, []byte("pristine"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fake qemu records its argv and exits.
	argv := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "fake-qemu")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > "+argv+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")
	q := &QEMUProvisioner{
		RunDir: runDir, BasePath: filepath.Join(dir, "missing-linux-base.qcow2"), QEMUBin: fake,
		WindowsBasePath: base,
		Firmware:        &qemu.Firmware{Code: filepath.Join(dir, "OVMF_CODE.fd"), Vars: vars},
	}
	pcfg := config.Pool{Name: "win", OS: "windows", CPUs: 2, MemoryMB: 2048, DiskGB: 10}
	vm, cleanup, err := q.Provision(context.Background(), "ghq-win-test", pcfg, "JITBLOB")
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(runDir, "ghq-win-test")
	for _, f := range []string{"overlay.qcow2", "seed.iso", "runner-jit.conf", "vars.fd"} {
		if _, err := os.Stat(filepath.Join(workdir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(workdir, "vars.fd")); string(b) != "pristine" {
		t.Error("vars.fd must be a copy of the pristine OVMF_VARS")
	}
	if _, err := os.Stat(filepath.Join(workdir, "user-data")); !os.IsNotExist(err) {
		t.Error("windows VMs must not get cloud-init user-data")
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("fake qemu did not exit")
	}
	got, _ := os.ReadFile(argv)
	for _, want := range []string{
		"-device virtio-blk-pci,drive=boot,bootindex=0",
		"-device ide-cd,drive=seed,bus=ide.1",
		"if=pflash,format=raw,file=" + filepath.Join(workdir, "vars.fd"),
		"hv_time",
		"-no-reboot",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("argv missing %q:\n%s", want, got)
		}
	}
	cleanup()
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Errorf("workdir not removed: %v", err)
	}
}

func TestProvisionWindowsUninitialisedCleansUp(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	// No WindowsBasePath/Firmware: a windows pool must fail before boot.
	q := &QEMUProvisioner{RunDir: runDir, BasePath: filepath.Join(dir, "base.qcow2"), QEMUBin: "/bin/false"}
	_, _, err := q.Provision(context.Background(), "ghq-win-y", config.Pool{OS: "windows", DiskGB: 10}, "J")
	if err == nil || !strings.Contains(err.Error(), "not initialised") {
		t.Fatalf("want uninitialised error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(runDir, "ghq-win-y")); !os.IsNotExist(statErr) {
		t.Error("failed windows provision left workdir behind")
	}
}
