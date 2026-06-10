package qemu

import (
	"context"
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
	if err := CreateOverlay(context.Background(), base, overlay, 10); err != nil {
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
	if err := CreateOverlay(context.Background(), base, overlay, 10); err != nil {
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
