// Package dockerbackend runs ephemeral runner jobs in Docker containers
// sandboxed by gVisor (runsc), for hosts without /dev/kvm. One job = one
// container; the container exiting is the job-done signal (the docker
// analogue of guest poweroff in the qemu backend).
package dockerbackend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/pool"
)

// Image is the locally-built runner image tag (see Bake).
const Image = "ghq-runner-base:latest"

// managedLabel marks containers owned by this controller for reaping.
const managedLabel = "ghq.managed=true"

// RunnerArch maps GOARCH to the actions-runner release arch suffix. Only
// amd64 and arm64 hosts are supported; anything else falls back to x64.
func RunnerArch(goarch string) string {
	if goarch == "arm64" {
		return "arm64"
	}
	return "x64"
}

// RunArgs builds the `docker run` argv for one job container.
// --privileged is required by the inner dockerd (DinD); under runtime
// runsc it grants capabilities inside gVisor's sandbox, not on the host.
// jitDir must be an absolute path without ':' (the bind-mount spec is
// colon-delimited).
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

// Provisioner creates one Docker container per job. It implements
// pool.Provisioner; the per-job workdir under RunDir holds only the JIT
// config file (bind-mounted read-only into the container).
type Provisioner struct {
	RunDir    string // <state>/run
	DockerBin string
	Runtime   string // "runsc" or "runc"
}

func (d *Provisioner) Provision(ctx context.Context, name string, p config.Pool, jitConfig string) (pool.VM, func(), error) {
	dir := filepath.Join(d.RunDir, name)
	jitDir := filepath.Join(dir, "jit")
	if err := os.MkdirAll(jitDir, 0o700); err != nil {
		return nil, nil, err
	}
	// rm of a container that never started is a harmless no-op, so one
	// cleanup covers every exit path.
	cleanup := func() {
		_ = exec.Command(d.DockerBin, "rm", "--force", "--volumes", name).Run()
		_ = os.RemoveAll(dir)
	}
	fail := func(err error) (pool.VM, func(), error) {
		cleanup()
		return nil, nil, err
	}

	if err := os.WriteFile(filepath.Join(jitDir, "config"), []byte(jitConfig), 0o600); err != nil {
		return fail(err)
	}
	absJitDir, err := filepath.Abs(jitDir)
	if err != nil {
		return fail(err)
	}
	if out, err := exec.CommandContext(ctx, d.DockerBin,
		RunArgs(name, d.Runtime, p, absJitDir)...).CombinedOutput(); err != nil {
		return fail(fmt.Errorf("docker run %s: %v: %s", name, err, out))
	}
	return newContainer(d.DockerBin, name), cleanup, nil
}
