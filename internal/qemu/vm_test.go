package qemu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeQEMU(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-qemu")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVMExitsCleanly(t *testing.T) {
	vm, err := Start(context.Background(), fakeQEMU(t, "exit 0"), testSpec(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM did not exit")
	}
	if vm.Err() != nil {
		t.Errorf("Err() = %v", vm.Err())
	}
}

func TestVMKill(t *testing.T) {
	vm, err := Start(context.Background(), fakeQEMU(t, "sleep 60"), testSpec(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM did not die after Kill")
	}
	if vm.Err() == nil {
		t.Error("Err() should be non-nil after Kill")
	}
}

func TestVMPowerdownFallsBackToKill(t *testing.T) {
	// No QMP socket exists for the fake binary, so Powerdown must fall back
	// to Kill and still terminate the process.
	vm, err := Start(context.Background(), fakeQEMU(t, "sleep 60"), testSpec(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Powerdown(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM still running after Powerdown fallback")
	}
}
