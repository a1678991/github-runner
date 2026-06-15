package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/dockerbackend"
	"github.com/a1678991/github-qemu-runner/internal/github"
	"github.com/a1678991/github-qemu-runner/internal/pool"
)

// Run wires everything together and blocks until ctx is cancelled and all
// pools have drained.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	keyPEM, err := os.ReadFile(cfg.GitHub.PrivateKeyPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	key, err := github.ParseRSAPrivateKey(keyPEM)
	if err != nil {
		return err
	}
	gh := github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.AppID, cfg.GitHub.InstallationID, key)

	runDir := cfg.Paths.Run
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}

	var qemuProv *QEMUProvisioner
	if cfg.HasBackend("qemu") {
		qemuBin, err := exec.LookPath("qemu-system-x86_64")
		if err != nil {
			return fmt.Errorf("qemu-system-x86_64 not found: %w", err)
		}
		basePath, err := filepath.Abs(filepath.Join(cfg.Paths.Images, "base.qcow2"))
		if err != nil {
			return err
		}
		if _, err := os.Stat(basePath); err != nil {
			return fmt.Errorf("base image missing (run `github-qemu-runner refresh-image` first): %w", err)
		}
		qemuProv = &QEMUProvisioner{RunDir: runDir, BasePath: basePath, QEMUBin: qemuBin}
	}

	var dockerProv *dockerbackend.Provisioner
	if cfg.HasBackend("docker") {
		dockerBin, err := exec.LookPath("docker")
		if err != nil {
			return fmt.Errorf("docker not found: %w", err)
		}
		for _, v := range []struct {
			image string
			used  bool
		}{
			{dockerbackend.Image, cfg.HasDockerIsolation("gvisor")},
			{dockerbackend.SlimImage, cfg.HasDockerIsolation("seccomp")},
		} {
			if !v.used {
				continue
			}
			if err := exec.CommandContext(ctx, dockerBin, "image", "inspect", v.image).Run(); err != nil {
				return fmt.Errorf("runner image %s missing (run `github-qemu-runner refresh-image` first): %w", v.image, err)
			}
		}
		// Fail at startup, not at the first job, on a missing custom profile.
		for _, p := range cfg.Pools {
			if p.SeccompProfile == "" {
				continue
			}
			if _, err := os.Stat(p.SeccompProfile); err != nil {
				return fmt.Errorf("pool %s: seccomp_profile: %w", p.Name, err)
			}
		}
		if cfg.Docker.Runtime == "runc" && cfg.HasDockerIsolation("gvisor") {
			log.Warn("gvisor-isolation pools run WITHOUT gVisor (docker.runtime: runc): " +
				"--privileged DinD containers have effectively no isolation boundary")
		}
		// Containers reference jit mounts under runDir, so reap them
		// before ReapOrphans deletes the workdirs.
		dockerbackend.ReapContainers(ctx, dockerBin, log)
		dockerProv = &dockerbackend.Provisioner{RunDir: runDir, DockerBin: dockerBin, Runtime: cfg.Docker.Runtime}
	}

	for _, w := range cfg.CapacityWarnings(runtime.NumCPU(), HostMemMB()) {
		log.Warn(w)
	}

	ReapOrphans(ctx, runDir, gh, apiPrefixes(cfg), log)

	var wg sync.WaitGroup
	for _, pc := range cfg.Pools {
		var prov pool.Provisioner = qemuProv
		if pc.Backend == "docker" {
			prov = dockerProv
		}
		p := &pool.Pool{Cfg: pc, GH: gh, Prov: prov, Log: log}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Run(ctx)
		}()
	}
	log.Info("controller running", "pools", len(cfg.Pools))
	wg.Wait()
	log.Info("all pools drained; exiting")
	return nil
}

// apiPrefixes returns the deduplicated registration scopes of all pools.
func apiPrefixes(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range cfg.Pools {
		pre := p.APIPrefix()
		if !seen[pre] {
			seen[pre] = true
			out = append(out, pre)
		}
	}
	return out
}

// HostMemMB reads MemTotal from /proc/meminfo. Returns 0 on failure, which
// CapacityWarnings treats as "unknown, don't warn".
func HostMemMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(b)) {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			fields := strings.Fields(rest)
			if len(fields) >= 1 {
				if kb, err := strconv.Atoi(fields[0]); err == nil {
					return kb / 1024
				}
			}
		}
	}
	return 0
}
