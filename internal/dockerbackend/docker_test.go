package dockerbackend

import (
	"reflect"
	"testing"

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
