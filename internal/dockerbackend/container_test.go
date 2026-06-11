package dockerbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDocker writes a docker stand-in script that appends each invocation
// to argv.log and emulates the subcommands Container uses.
// waitExit is what `docker wait` prints (the container's exit code).
func fakeDocker(t *testing.T, waitExit string) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "docker")
	script := `#!/bin/sh
echo "$@" >> ` + argvLog + `
case "$1" in
  wait) echo ` + waitExit + ` ;;
  logs) echo "console output here" ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

func awaitDone(t *testing.T, c *Container) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("container did not report done")
	}
}

func TestContainerCleanExit(t *testing.T) {
	bin, _ := fakeDocker(t, "0")
	c := newContainer(bin, "ghq-x-1")
	awaitDone(t, c)
	if err := c.Err(); err != nil {
		t.Errorf("Err() = %v, want nil for exit 0", err)
	}
}

func TestContainerNonzeroExit(t *testing.T) {
	bin, _ := fakeDocker(t, "137")
	c := newContainer(bin, "ghq-x-2")
	awaitDone(t, c)
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "137") {
		t.Errorf("Err() = %v, want exit-status error containing 137", err)
	}
}

func TestContainerKill(t *testing.T) {
	bin, argvLog := fakeDocker(t, "0")
	c := newContainer(bin, "ghq-x-3")
	if err := c.Kill(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(b), "rm --force --volumes ghq-x-3") {
		t.Errorf("Kill did not force-remove with volumes; argv log:\n%s", b)
	}
}

func TestContainerPowerdown(t *testing.T) {
	bin, argvLog := fakeDocker(t, "0")
	c := newContainer(bin, "ghq-x-4")
	if err := c.Powerdown(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(b), "stop --time 30 ghq-x-4") {
		t.Errorf("Powerdown did not docker stop; argv log:\n%s", b)
	}
}

func TestContainerConsoleTail(t *testing.T) {
	bin, _ := fakeDocker(t, "0")
	c := newContainer(bin, "ghq-x-5")
	if got := c.ConsoleTail(); !strings.Contains(got, "console output here") {
		t.Errorf("ConsoleTail() = %q", got)
	}
}
