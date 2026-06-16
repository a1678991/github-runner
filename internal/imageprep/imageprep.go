// Package imageprep bakes the base/runner images the configured pools
// need. It backs both the `refresh-image` command (force: rebake all)
// and the controller's start-time auto-refresh (missing-only).
package imageprep

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/a1678991/github-qemu-runner/internal/config"
	"github.com/a1678991/github-qemu-runner/internal/dockerbackend"
	"github.com/a1678991/github-qemu-runner/internal/imagebake"
)

// imagePlan is the set of artifacts to bake.
type imagePlan struct {
	QEMU     bool
	Variants []string // subset of {"dind","slim"}, in that order
}

// plan decides what to bake. present reports whether a given artifact is
// already on disk: "qemu" (base.qcow2), "dind"/"slim" (docker images).
// With force, presence is ignored and every used artifact is selected.
func plan(cfg *config.Config, force bool, present func(artifact string) bool) imagePlan {
	var p imagePlan
	if cfg.HasBackend("qemu") && (force || !present("qemu")) {
		p.QEMU = true
	}
	if cfg.HasBackend("docker") {
		for _, v := range []struct{ name, mode string }{
			{"dind", "gvisor"},
			{"slim", "seccomp"},
		} {
			if !cfg.HasDockerIsolation(v.mode) {
				continue
			}
			if force || !present(v.name) {
				p.Variants = append(p.Variants, v.name)
			}
		}
	}
	return p
}

// Ensure bakes the images the configured pools require. With force every
// used artifact is rebaked; otherwise only the missing ones.
func Ensure(ctx context.Context, cfg *config.Config, log *slog.Logger, force bool) error {
	var qemuBin, dockerBin string
	var err error
	if cfg.HasBackend("qemu") {
		if qemuBin, err = exec.LookPath("qemu-system-x86_64"); err != nil {
			return fmt.Errorf("qemu-system-x86_64 not found: %w", err)
		}
	}
	if cfg.HasBackend("docker") {
		if dockerBin, err = exec.LookPath("docker"); err != nil {
			return fmt.Errorf("docker not found: %w", err)
		}
	}

	present := func(artifact string) bool {
		switch artifact {
		case "qemu":
			_, statErr := os.Stat(filepath.Join(cfg.Paths.Images, "base.qcow2"))
			return statErr == nil
		case "dind":
			return exec.CommandContext(ctx, dockerBin, "image", "inspect", dockerbackend.Image).Run() == nil
		case "slim":
			return exec.CommandContext(ctx, dockerBin, "image", "inspect", dockerbackend.SlimImage).Run() == nil
		}
		return false
	}

	p := plan(cfg, force, present)

	if p.QEMU {
		if err := imagebake.Bake(ctx, imagebake.Options{
			ImageDir: cfg.Paths.Images,
			APIBase:  cfg.GitHub.APIBaseURL,
			QEMUBin:  qemuBin,
			Log:      log,
		}); err != nil {
			return err
		}
	}
	if len(p.Variants) > 0 {
		if err := dockerbackend.Bake(ctx, dockerbackend.BakeOptions{
			ImageDir:  cfg.Paths.Images,
			APIBase:   cfg.GitHub.APIBaseURL,
			DockerBin: dockerBin,
			Variants:  p.Variants,
			Log:       log,
		}); err != nil {
			return err
		}
	}
	return nil
}
