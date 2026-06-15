package dockerbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/imagebake"
	"github.com/a1678991/github-qemu-runner/scripts"
)

type BakeOptions struct {
	ImageDir  string
	HTTP      *http.Client
	APIBase   string
	DockerBin string
	// Variants selects which Dockerfile targets to build: "dind"
	// (-> Image, for gvisor pools) and/or "slim" (-> SlimImage, for
	// seccomp pools). Empty means ["dind"].
	Variants []string
	Log      *slog.Logger
}

func (o *BakeOptions) defaults() {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 15 * time.Minute}
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if len(o.Variants) == 0 {
		o.Variants = []string{"dind"}
	}
}

// Bake builds the runner container image variants selected by o.Variants
// from the embedded Dockerfile, tagging Image (dind) and/or SlimImage
// (slim). The build runs natively on this host, so the image arch
// always matches (arm64 hosts get linux-arm64 runners). A provenance
// sidecar lands at <ImageDir>/docker-base.json, mirroring base.json.
func Bake(ctx context.Context, o BakeOptions) error {
	o.defaults()
	tags := map[string]string{"dind": Image, "slim": SlimImage}
	for _, v := range o.Variants {
		if _, ok := tags[v]; !ok {
			return fmt.Errorf("unknown image variant %q", v)
		}
	}
	rel, err := imagebake.LatestRunner(ctx, o.HTTP, o.APIBase, RunnerArch(runtime.GOARCH))
	if err != nil {
		return fmt.Errorf("resolve runner release: %w", err)
	}
	if rel.SHA256 == "" {
		o.Log.Warn("runner tarball SHA not found in release notes; relying on TLS only")
	}

	buildDir, err := os.MkdirTemp("", "ghq-docker-bake-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(scripts.Dockerfile), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, "entrypoint.sh"), []byte(scripts.DockerEntrypoint), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, "entrypoint-slim.sh"), []byte(scripts.DockerEntrypointSlim), 0o755); err != nil {
		return err
	}

	for _, v := range o.Variants {
		tag := tags[v]
		o.Log.Info("building runner image", "runner_version", rel.Version, "variant", v, "tag", tag)
		cmd := exec.CommandContext(ctx, o.DockerBin, "build", "--pull",
			"--target", v,
			"--build-arg", "RUNNER_VERSION="+rel.Version,
			"--build-arg", "RUNNER_TARBALL_URL="+rel.TarballURL,
			"--build-arg", "RUNNER_TARBALL_SHA256="+rel.SHA256,
			"--tag", tag, buildDir)
		cmd.Stdout = os.Stderr // long build; stream progress instead of buffering
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("docker build %s: %w", v, err)
		}
	}

	imagesDir := o.ImageDir
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return err
	}
	meta, err := json.MarshalIndent(map[string]any{
		"runner_version": rel.Version,
		"arch":           RunnerArch(runtime.GOARCH),
		"variants":       o.Variants,
		"baked_at":       time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "docker-base.json"), append(meta, '\n'), 0o644); err != nil {
		return err
	}
	o.Log.Info("runner images built", "variants", o.Variants)
	return nil
}
