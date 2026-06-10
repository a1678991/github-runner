package qemu

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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

func TestVMPowerdownGraceful(t *testing.T) {
	dir := t.TempDir()
	spec := testSpec(dir)
	// Fake QMP server on the socket qemu would have created.
	ln, err := net.Listen("unix", spec.QMPSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	powerdownReceived := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintln(conn, `{"QMP":{"version":{},"capabilities":[]}}`)
		br := bufio.NewReader(conn)
		for range 2 {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = fmt.Fprintln(conn, `{"return":{}}`)
			if strings.Contains(line, "system_powerdown") {
				close(powerdownReceived)
			}
		}
	}()

	vm, err := Start(context.Background(), fakeQEMU(t, "sleep 60"), spec)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the guest reacting to the ACPI power button: once the QMP
	// command arrives, the "guest" (fake qemu) exits.
	go func() {
		<-powerdownReceived
		_ = vm.cmd.Process.Kill()
	}()
	if err := vm.Powerdown(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("VM still running after graceful powerdown")
	}
	select {
	case <-powerdownReceived:
	case <-time.After(time.Second):
		t.Fatal("system_powerdown never reached the QMP server")
	}
}

func TestConsoleTail(t *testing.T) {
	dir := t.TempDir()
	spec := testSpec(dir)
	vm := &VM{spec: spec}
	if got := vm.ConsoleTail(); got != "" {
		t.Errorf("missing console log: got %q, want empty", got)
	}
	big := strings.Repeat("a", 3000) + "TAIL-MARKER"
	if err := os.WriteFile(spec.ConsoleLog, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	got := vm.ConsoleTail()
	if len(got) != 2048 {
		t.Errorf("tail length = %d, want 2048", len(got))
	}
	if !strings.HasSuffix(got, "TAIL-MARKER") {
		t.Error("tail does not end with the marker")
	}
}
