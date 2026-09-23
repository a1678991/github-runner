package qemu

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testSpec(dir string) Spec {
	return Spec{
		Name:        "ghq-fmt-ab12",
		CPUs:        2,
		MemoryMB:    2048,
		OverlayPath: filepath.Join(dir, "overlay.qcow2"),
		SeedISOPath: filepath.Join(dir, "seed.iso"),
		ConsoleLog:  filepath.Join(dir, "console.log"),
		QMPSocket:   filepath.Join(dir, "qmp.sock"),
		PIDFile:     filepath.Join(dir, "qemu.pid"),
	}
}

func TestArgs(t *testing.T) {
	dir := t.TempDir()
	args := Args(testSpec(dir))
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-accel kvm",
		"-cpu host",
		"-machine q35",
		"-smp 2",
		"-m 2048",
		"file=" + filepath.Join(dir, "overlay.qcow2") + ",if=virtio,format=qcow2",
		"file=" + filepath.Join(dir, "seed.iso") + ",if=virtio,format=raw,readonly=on",
		"-netdev user,id=n0",
		"-device virtio-net-pci,netdev=n0",
		"-display none",
		"-serial file:" + filepath.Join(dir, "console.log"),
		"-qmp unix:" + filepath.Join(dir, "qmp.sock") + ",server=on,wait=off",
		"-pidfile " + filepath.Join(dir, "qemu.pid"),
		"-no-reboot",
		"-name ghq-fmt-ab12",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\nargs: %s", want, joined)
		}
	}
	// -nographic must NOT be used: it would fight -serial file:
	if slices.Contains(args, "-nographic") {
		t.Error("args must not contain -nographic")
	}
}

func TestCreateOverlaySmallerThanBase(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", base, "30G").CombinedOutput(); err != nil {
		t.Fatalf("create base: %v: %s", err, out)
	}
	overlay := filepath.Join(dir, "overlay.qcow2")
	// Requesting less than the backing size must succeed (no shrink attempt)
	// and keep the backing image's virtual size.
	if err := CreateOverlay(context.Background(), base, "qcow2", overlay, 10); err != nil {
		t.Fatal(err)
	}
	info, err := exec.Command("qemu-img", "info", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v: %s", err, info)
	}
	if !strings.Contains(string(info), "30 GiB") {
		t.Errorf("virtual size changed:\n%s", info)
	}
}

func TestCreateOverlay(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	out, err := exec.Command("qemu-img", "create", "-f", "qcow2", base, "1G").CombinedOutput()
	if err != nil {
		t.Fatalf("create base: %v: %s", err, out)
	}
	overlay := filepath.Join(dir, "overlay.qcow2")
	if err := CreateOverlay(context.Background(), base, "qcow2", overlay, 10); err != nil {
		t.Fatal(err)
	}
	info, err := exec.Command("qemu-img", "info", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v: %s", err, info)
	}
	s := string(info)
	if !strings.Contains(s, "backing file: "+base) {
		t.Errorf("no backing file in info:\n%s", s)
	}
	if !strings.Contains(s, "10 GiB") {
		t.Errorf("not resized to 10 GiB:\n%s", s)
	}
}

// TestArgsLinuxUnchanged pins the exact Linux argv: Windows support must
// not perturb it.
func TestArgsLinuxUnchanged(t *testing.T) {
	dir := "/run/x"
	got := strings.Join(Args(testSpec(dir)), " ")
	want := "-accel kvm -cpu host -machine q35 -smp 2 -m 2048 " +
		"-drive file=/run/x/overlay.qcow2,if=virtio,format=qcow2 " +
		"-drive file=/run/x/seed.iso,if=virtio,format=raw,readonly=on " +
		"-netdev user,id=n0 -device virtio-net-pci,netdev=n0 -display none " +
		"-serial file:/run/x/console.log -qmp unix:/run/x/qmp.sock,server=on,wait=off " +
		"-pidfile /run/x/qemu.pid -no-reboot -name ghq-fmt-ab12"
	if got != want {
		t.Errorf("linux argv changed:\n got: %s\nwant: %s", got, want)
	}
}

func windowsJobSpec() Spec {
	s := testSpec("/run/w")
	s.Name = "ghq-win-ab12"
	s.Firmware = &Firmware{Code: "/fw/OVMF_CODE.fd", Vars: "/run/w/vars.fd"}
	s.DiskBus = "virtio"
	s.SeedBus = "ahci"
	s.HyperV = true
	return s
}

func TestArgsWindowsJob(t *testing.T) {
	got := strings.Join(Args(windowsJobSpec()), " ")
	want := "-accel kvm -cpu host,hv_relaxed,hv_vapic,hv_spinlocks=0x1fff,hv_time -machine q35 -smp 2 -m 2048 " +
		"-drive if=pflash,format=raw,readonly=on,file=/fw/OVMF_CODE.fd " +
		"-drive if=pflash,format=raw,file=/run/w/vars.fd " +
		"-drive file=/run/w/overlay.qcow2,if=none,id=boot,format=qcow2 " +
		"-device virtio-blk-pci,drive=boot,bootindex=0 " +
		"-drive file=/run/w/seed.iso,if=none,id=seed,format=raw,readonly=on,media=cdrom " +
		"-device ide-cd,drive=seed,bus=ide.1 " +
		"-netdev user,id=n0 -device virtio-net-pci,netdev=n0 -display none " +
		"-serial file:/run/w/console.log -qmp unix:/run/w/qmp.sock,server=on,wait=off " +
		"-pidfile /run/w/qemu.pid -no-reboot -name ghq-win-ab12"
	if got != want {
		t.Errorf("windows job argv:\n got: %s\nwant: %s", got, want)
	}
}

func TestArgsWindowsBake(t *testing.T) {
	s := windowsJobSpec()
	s.DiskBus = "ahci"
	s.AllowReboot = true
	s.ExtraDisks = []Disk{{Path: "/run/w/dummy.raw", Format: "raw"}}
	s.CDROMs = []string{"/img/virtio-win.iso"}
	got := strings.Join(Args(s), " ")
	for _, want := range []string{
		"-drive file=/run/w/overlay.qcow2,if=none,id=boot,format=qcow2 -device ide-hd,drive=boot,bus=ide.0,bootindex=0",
		"-drive file=/run/w/dummy.raw,if=none,id=extra0,format=raw -device virtio-blk-pci,drive=extra0",
		"-drive file=/img/virtio-win.iso,if=none,id=cd0,format=raw,readonly=on,media=cdrom -device ide-cd,drive=cd0,bus=ide.2",
		"-device ide-cd,drive=seed,bus=ide.1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bake argv missing %q\nargs: %s", want, got)
		}
	}
	if slices.Contains(Args(s), "-no-reboot") {
		t.Error("AllowReboot must drop -no-reboot (OOBE reboots)")
	}
}

func TestCreateOverlayBackingFormat(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.vhdx")
	if out, err := exec.Command("qemu-img", "create", "-f", "vhdx", base, "64M").CombinedOutput(); err != nil {
		t.Fatalf("create vhdx: %v: %s", err, out)
	}
	overlay := filepath.Join(dir, "overlay.qcow2")
	if err := CreateOverlay(context.Background(), base, "vhdx", overlay, 1); err != nil {
		t.Fatal(err)
	}
	info, err := exec.Command("qemu-img", "info", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v: %s", err, info)
	}
	if !strings.Contains(string(info), "backing file format: vhdx") {
		t.Errorf("backing format not vhdx:\n%s", info)
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	if err := os.WriteFile(src, []byte("vars"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "sub", "b")
	if err := CopyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "vars" {
		t.Errorf("copy = %q, %v", b, err)
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
}
