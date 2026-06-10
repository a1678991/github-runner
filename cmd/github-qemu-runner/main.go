// Command github-qemu-runner runs ephemeral GitHub Actions runners in
// QEMU/KVM virtual machines: `controller` supervises runner pools,
// `refresh-image` (re)bakes the base VM image, `setup` runs preflight
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
	"syscall"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/controller"
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
	qemuBin, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return err
	}
	return imagebake.Bake(ctx, imagebake.Options{
		StateDir: cfg.StateDir,
		APIBase:  cfg.GitHub.APIBaseURL,
		QEMUBin:  qemuBin,
		Log:      log,
	})
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

	for _, bin := range []string{"qemu-system-x86_64", "qemu-img", "genisoimage"} {
		_, lookErr := exec.LookPath(bin)
		check(bin+" on PATH", lookErr)
	}

	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err == nil {
		_ = kvm.Close()
	}
	check("/dev/kvm read-write access", err)

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

	base := filepath.Join(cfg.StateDir, "images", "base.qcow2")
	if _, err := os.Stat(base); err != nil {
		fmt.Printf("note  base image missing; run `github-qemu-runner refresh-image`\n")
	} else {
		fmt.Printf("ok    base image %s\n", base)
	}

	for _, w := range cfg.CapacityWarnings(runtime.NumCPU(), controller.HostMemMB()) {
		fmt.Printf("warn  %s\n", w)
	}

	if failed {
		return fmt.Errorf("setup found problems")
	}
	return nil
}
