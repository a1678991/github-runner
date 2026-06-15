// Command github-qemu-runner runs ephemeral GitHub Actions runners in
// QEMU/KVM virtual machines or sandboxed Docker containers (per-pool
// gvisor or seccomp isolation):
// `controller` supervises runner pools, `refresh-image` (re)bakes the
// base VM image and/or runner container image, `setup` runs preflight
// checks. See packaging/config.example.yaml for configuration.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/controller"
	"github.com/a1678991/github-qemu-runner/internal/dockerbackend"
	"github.com/a1678991/github-qemu-runner/internal/github"
	"github.com/a1678991/github-qemu-runner/internal/imagebake"
)

func main() {
	configPath := flag.String("config", "/etc/github-qemu-runner/config.yaml", "path to config file")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "controller"
	}
	var err error
	switch cmd {
	case "controller":
		err = runController(ctx, *configPath, log)
	case "refresh-image":
		err = runRefreshImage(ctx, *configPath, log)
	case "setup":
		err = runSetup(ctx, *configPath)
	default:
		fmt.Fprintln(os.Stderr, "usage: github-qemu-runner [-config PATH] <controller|refresh-image|setup>")
		os.Exit(2)
	}
	if err != nil {
		log.Error(cmd+" failed", "err", err)
		os.Exit(1)
	}
}

func runController(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	return controller.Run(ctx, cfg, log)
}

func runRefreshImage(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.HasBackend("qemu") {
		qemuBin, err := exec.LookPath("qemu-system-x86_64")
		if err != nil {
			return err
		}
		if err := imagebake.Bake(ctx, imagebake.Options{
			ImageDir: cfg.Paths.Images,
			APIBase:  cfg.GitHub.APIBaseURL,
			QEMUBin:  qemuBin,
			Log:      log,
		}); err != nil {
			return err
		}
	}
	if cfg.HasBackend("docker") {
		dockerBin, err := exec.LookPath("docker")
		if err != nil {
			return err
		}
		var variants []string
		if cfg.HasDockerIsolation("gvisor") {
			variants = append(variants, "dind")
		}
		if cfg.HasDockerIsolation("seccomp") {
			variants = append(variants, "slim")
		}
		if err := dockerbackend.Bake(ctx, dockerbackend.BakeOptions{
			ImageDir:  cfg.Paths.Images,
			APIBase:   cfg.GitHub.APIBaseURL,
			DockerBin: dockerBin,
			Variants:  variants,
			Log:       log,
		}); err != nil {
			return err
		}
	}
	return nil
}

func runSetup(ctx context.Context, configPath string) error {
	failed := false
	check := func(name string, err error) {
		if err != nil {
			failed = true
			fmt.Printf("FAIL  %s: %v\n", name, err)
		} else {
			fmt.Printf("ok    %s\n", name)
		}
	}

	cfg, err := config.Load(configPath)
	check("config "+configPath, err)
	if err != nil {
		return fmt.Errorf("setup found problems")
	}

	if cfg.HasBackend("qemu") {
		for _, bin := range []string{"qemu-system-x86_64", "qemu-img", "genisoimage"} {
			_, lookErr := exec.LookPath(bin)
			check(bin+" on PATH", lookErr)
		}
		kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err == nil {
			_ = kvm.Close()
		}
		check("/dev/kvm read-write access", err)
	}

	if cfg.HasBackend("docker") {
		dockerBin, lookErr := exec.LookPath("docker")
		check("docker on PATH", lookErr)
		if lookErr == nil {
			check("docker daemon reachable", exec.CommandContext(ctx, dockerBin, "info").Run())
			if cfg.HasDockerIsolation("gvisor") {
				if cfg.Docker.Runtime == "runsc" {
					out, err := exec.CommandContext(ctx, dockerBin, "info", "--format", "{{json .Runtimes}}").Output()
					if err == nil && !strings.Contains(string(out), `"runsc"`) {
						err = fmt.Errorf(`runsc not in docker runtimes — register it in /etc/docker/daemon.json with runtimeArgs ["--net-raw","--allow-packet-socket-write"], then restart docker`)
					}
					check("runsc runtime registered", err)
				} else {
					fmt.Printf("warn  docker.runtime is runc: gvisor-isolation pools run WITHOUT gVisor; --privileged DinD has effectively no isolation boundary\n")
				}
			}
			// One image + outbound-connectivity check per isolation mode in
			// use, each with its matching runtime and image. Catches
			// host-firewall problems (e.g. OCI's default inet-filter forward
			// DROP) that only bite inside containers.
			type modeCheck struct{ runtime, image string }
			var modes []modeCheck
			if cfg.HasDockerIsolation("gvisor") {
				modes = append(modes, modeCheck{cfg.Docker.Runtime, dockerbackend.Image})
			}
			if cfg.HasDockerIsolation("seccomp") {
				modes = append(modes, modeCheck{"runc", dockerbackend.SlimImage})
			}
			for _, m := range modes {
				if err := exec.CommandContext(ctx, dockerBin, "image", "inspect", m.image).Run(); err != nil {
					fmt.Printf("note  runner image %s missing; run `github-qemu-runner refresh-image`\n", m.image)
					continue
				}
				fmt.Printf("ok    runner image %s\n", m.image)
				check("container outbound connectivity ("+m.image+")", exec.CommandContext(ctx, dockerBin,
					"run", "--rm", "--runtime", m.runtime, "--entrypoint", "curl",
					m.image, "-fsS", "--max-time", "30",
					"-o", "/dev/null", "https://api.github.com").Run())
			}
		}
	}

	keyPEM, err := os.ReadFile(cfg.GitHub.PrivateKeyPath)
	check("private key readable", err)
	if err == nil {
		key, parseErr := github.ParseRSAPrivateKey(keyPEM)
		check("private key parses", parseErr)
		if parseErr == nil {
			gh := github.New(cfg.GitHub.APIBaseURL, cfg.GitHub.AppID, cfg.GitHub.InstallationID, key)
			check("GitHub App auth (installation token)", gh.CheckAuth(ctx))
		}
	}

	if cfg.HasBackend("qemu") {
		base := filepath.Join(cfg.Paths.Images, "base.qcow2")
		if _, err := os.Stat(base); err != nil {
			fmt.Printf("note  base image missing; run `github-qemu-runner refresh-image`\n")
		} else {
			fmt.Printf("ok    base image %s\n", base)
		}
	}

	for _, w := range cfg.CapacityWarnings(runtime.NumCPU(), controller.HostMemMB()) {
		fmt.Printf("warn  %s\n", w)
	}

	if failed {
		return fmt.Errorf("setup found problems")
	}
	return nil
}
