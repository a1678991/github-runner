// Package dockerbackend runs ephemeral runner jobs in Docker containers
// sandboxed by gVisor (runsc), for hosts without /dev/kvm. One job = one
// container; the container exiting is the job-done signal (the docker
// analogue of guest poweroff in the qemu backend).
package dockerbackend

import (
	"strconv"

	"github.com/a1678991/github-qemu-runner/internal/config"
)

// Image is the locally-built runner image tag (see Bake).
const Image = "ghq-runner-base:latest"

// managedLabel marks containers owned by this controller for reaping.
const managedLabel = "ghq.managed=true"

// RunnerArch maps GOARCH to the actions-runner release arch suffix.
func RunnerArch(goarch string) string {
	if goarch == "arm64" {
		return "arm64"
	}
	return "x64"
}

// RunArgs builds the `docker run` argv for one job container.
// --privileged is required by the inner dockerd (DinD); under runtime
// runsc it grants capabilities inside gVisor's sandbox, not on the host.
func RunArgs(name, runtime string, p config.Pool, jitDir string) []string {
	return []string{
		"run", "--detach",
		"--name", name,
		"--runtime", runtime,
		"--privileged",
		"--cpus", strconv.Itoa(p.CPUs),
		"--memory", strconv.Itoa(p.MemoryMB) + "m",
		"--label", managedLabel,
		"--volume", jitDir + ":/jit:ro",
		Image,
	}
}
