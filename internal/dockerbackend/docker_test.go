package dockerbackend

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

func TestRunnerArch(t *testing.T) {
	for goarch, want := range map[string]string{
		"amd64": "x64",
		"arm64": "arm64",
	} {
		if got := RunnerArch(goarch); got != want {
			t.Errorf("RunnerArch(%q) = %q, want %q", goarch, got, want)
		}
	}
}

func TestRunArgs(t *testing.T) {
	p := config.Pool{CPUs: 2, MemoryMB: 2048}
	got := RunArgs("ghq-oci-ab12", "runsc", p, "/var/lib/x/run/ghq-oci-ab12/jit")
	want := []string{
		"run", "--detach",
		"--name", "ghq-oci-ab12",
		"--runtime", "runsc",
		"--privileged",
		"--cpus", "2",
		"--memory", "2048m",
		"--label", "ghq.managed=true",
		"--volume", "/var/lib/x/run/ghq-oci-ab12/jit:/jit:ro",
		"ghq-runner-base:latest",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunArgs:\n got %q\nwant %q", got, want)
	}
}

func TestProvisionLifecycle(t *testing.T) {
	bin, argvLog := fakeDocker(t, "0")
	runDir := filepath.Join(t.TempDir(), "run")
	d := &Provisioner{RunDir: runDir, DockerBin: bin, Runtime: "runsc"}
	p := config.Pool{Name: "oci", CPUs: 2, MemoryMB: 2048}

	vm, cleanup, err := d.Provision(context.Background(), "ghq-oci-ab12", p, "JITBLOB")
	if err != nil {
		t.Fatal(err)
	}
	jitFile := filepath.Join(runDir, "ghq-oci-ab12", "jit", "config")
	b, err := os.ReadFile(jitFile)
	if err != nil || string(b) != "JITBLOB" {
		t.Errorf("jit file: %v, content %q", err, b)
	}
	if fi, _ := os.Stat(jitFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("jit file mode = %v, want 0600", fi.Mode().Perm())
	}
	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "run --detach --name ghq-oci-ab12 --runtime runsc --privileged") {
		t.Errorf("docker run argv wrong:\n%s", argv)
	}

	select {
	case <-vm.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("fake container did not exit")
	}
	cleanup()
	if _, err := os.Stat(filepath.Join(runDir, "ghq-oci-ab12")); !os.IsNotExist(err) {
		t.Error("workdir not removed by cleanup")
	}
	argv, _ = os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "rm --force --volumes ghq-oci-ab12") {
		t.Errorf("cleanup did not remove container; argv log:\n%s", argv)
	}
}

func TestProvisionFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	failingDocker := filepath.Join(dir, "docker")
	// `docker run` fails; `docker rm` (cleanup) succeeds.
	script := "#!/bin/sh\ncase \"$1\" in run) exit 1 ;; esac\nexit 0\n"
	if err := os.WriteFile(failingDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")
	d := &Provisioner{RunDir: runDir, DockerBin: failingDocker, Runtime: "runsc"}
	_, _, err := d.Provision(context.Background(), "ghq-x-y", config.Pool{CPUs: 1, MemoryMB: 512}, "J")
	if err == nil {
		t.Fatal("want error from failed docker run")
	}
	if _, statErr := os.Stat(filepath.Join(runDir, "ghq-x-y")); !os.IsNotExist(statErr) {
		t.Error("failed provision left workdir behind")
	}
}
