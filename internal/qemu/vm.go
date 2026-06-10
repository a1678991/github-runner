package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// VM supervises one qemu child process.
type VM struct {
	cmd  *exec.Cmd
	spec Spec
	done chan struct{}

	mu  sync.Mutex
	err error
}

// Start launches the qemu binary for spec. The process is deliberately NOT
// tied to ctx: shutdown is orchestrated (drain, QMP powerdown), not by
// context kill. ctx only bounds Start itself.
func Start(_ context.Context, binary string, spec Spec) (*VM, error) {
	logf, err := os.Create(spec.ConsoleLog + ".qemu-stderr")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, Args(spec)...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}
	vm := &VM{cmd: cmd, spec: spec, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		_ = logf.Close()
		vm.mu.Lock()
		vm.err = err
		vm.mu.Unlock()
		close(vm.done)
	}()
	return vm, nil
}

// Done is closed when the qemu process has exited.
func (v *VM) Done() <-chan struct{} { return v.done }

// Err reports the process exit error; only meaningful after Done is closed.
func (v *VM) Err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

// Kill terminates the process and waits for it to be reaped.
func (v *VM) Kill() error {
	_ = v.cmd.Process.Kill()
	<-v.done
	return nil
}

// Powerdown asks the guest to shut down via QMP (ACPI power button) and
// waits up to timeout, then falls back to Kill. Always terminates the VM.
func (v *VM) Powerdown(timeout time.Duration) error {
	if err := qmpPowerdown(v.spec.QMPSocket); err != nil {
		return v.Kill()
	}
	select {
	case <-v.done:
		return nil
	case <-time.After(timeout):
		return v.Kill()
	}
}

// ConsoleTail returns the last 2 KiB of the guest serial console, so boot
// wedges and crashes can be surfaced into the journal before the workdir
// (and console.log with it) is deleted.
func (v *VM) ConsoleTail() string {
	b, err := os.ReadFile(v.spec.ConsoleLog)
	if err != nil {
		return ""
	}
	if len(b) > 2048 {
		b = b[len(b)-2048:]
	}
	return string(b)
}
