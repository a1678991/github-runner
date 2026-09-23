package imagebake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/a1678991/github-qemu-runner/internal/qemu"
	"github.com/a1678991/github-qemu-runner/internal/seed"
	"github.com/a1678991/github-qemu-runner/scripts"
)

// WindowsOptions configures BakeWindows. Zero values for HTTP, APIBase,
// CPUs, MemoryMB, Timeout, and Log take the same defaults as Options.
type WindowsOptions struct {
	ImageDir        string
	HTTP            *http.Client
	APIBase         string
	ImageURL        string
	ImageSHA256     string
	VirtioWinURL    string
	VirtioWinSHA256 string
	OVMFCode        string
	OVMFVars        string
	QEMUBin         string
	CPUs            int
	MemoryMB        int
	Timeout         time.Duration
	Log             *slog.Logger
}

func (o *WindowsOptions) defaults() {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Minute} // 11 GB image
	}
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.CPUs == 0 {
		o.CPUs = 4
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = 8192
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
}

// Windows image artifact names under ImageDir.
const (
	WindowsVHDX     = "windows-base.vhdx"
	VirtioWinISO    = "virtio-win.iso"
	WindowsBase     = "base-windows.qcow2"
	WindowsBaseMeta = "base-windows.json"
	windowsBakeDir  = "bake-windows"
)

// BakeWindows produces <ImageDir>/base-windows.qcow2: download the
// evaluation VHDX and the virtio-win ISO, resolve the runner and Git
// releases, boot an overlay of the VHDX under OVMF with an Unattend.xml
// seed CD that completes OOBE and runs bake.ps1, require the BAKE-OK
// serial sentinel, flatten, swap.
func BakeWindows(ctx context.Context, o WindowsOptions) error {
	o.defaults()
	if err := os.MkdirAll(o.ImageDir, 0o755); err != nil {
		return err
	}
	vhdx := filepath.Join(o.ImageDir, WindowsVHDX)
	if err := o.fetch(ctx, o.ImageURL, vhdx, o.ImageSHA256, "windows image"); err != nil {
		return err
	}
	iso := filepath.Join(o.ImageDir, VirtioWinISO)
	if err := o.fetch(ctx, o.VirtioWinURL, iso, o.VirtioWinSHA256, "virtio-win ISO"); err != nil {
		return err
	}

	runner, err := LatestRunner(ctx, o.HTTP, o.APIBase, "win", "x64")
	if err != nil {
		return fmt.Errorf("resolve runner release: %w", err)
	}
	if runner.SHA256 == "" {
		o.Log.Warn("runner zip SHA not found in release notes; relying on TLS only")
	}
	git, err := LatestGitForWindows(ctx, o.HTTP, o.APIBase)
	if err != nil {
		return fmt.Errorf("resolve git-for-windows release: %w", err)
	}
	if git.SHA256 == "" {
		o.Log.Warn("git installer SHA not found in release notes; relying on TLS only")
	}
	o.Log.Info("baking windows image", "runner_version", runner.Version, "git_version", git.Version)

	bakeDir := filepath.Join(o.ImageDir, windowsBakeDir)
	if err := os.RemoveAll(bakeDir); err != nil {
		return err
	}
	if err := os.MkdirAll(bakeDir, 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(bakeDir) }()

	absVHDX, err := filepath.Abs(vhdx)
	if err != nil {
		return err
	}
	overlay := filepath.Join(bakeDir, "bake-overlay.qcow2")
	if err := qemu.CreateOverlay(ctx, absVHDX, "vhdx", overlay, 0); err != nil {
		return err
	}
	dummy := filepath.Join(bakeDir, "dummy.raw")
	if out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "raw", dummy, "1G").CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create dummy: %v: %s", err, out)
	}
	vars := filepath.Join(bakeDir, "vars.fd")
	if err := qemu.CopyFile(o.OVMFVars, vars); err != nil {
		return fmt.Errorf("copy OVMF vars: %w", err)
	}

	password, err := RandomPassword(32)
	if err != nil {
		return err
	}
	unattend, err := RenderUnattend(password)
	if err != nil {
		return err
	}
	env, err := json.MarshalIndent(map[string]string{
		"runner_version": runner.Version,
		"runner_url":     runner.TarballURL,
		"runner_sha256":  runner.SHA256,
		"git_version":    git.Version,
		"git_url":        git.TarballURL,
		"git_sha256":     git.SHA256,
	}, "", "  ")
	if err != nil {
		return err
	}
	seedISO, err := seed.BuildISOFiles(ctx, bakeDir, "GHQSEED", map[string]string{
		"Unattend.xml":    unattend,
		"bake.ps1":        scripts.WindowsBake,
		"run-one-job.ps1": scripts.WindowsRunOneJob,
		"bake-env.json":   string(env),
	})
	if err != nil {
		return err
	}

	console := filepath.Join(bakeDir, "console.log")
	vm, err := qemu.Start(ctx, o.QEMUBin, qemu.Spec{
		Name: "ghq-bake-windows", CPUs: o.CPUs, MemoryMB: o.MemoryMB,
		OverlayPath: overlay, SeedISOPath: seedISO, ConsoleLog: console,
		QMPSocket:   filepath.Join(bakeDir, "qmp.sock"),
		PIDFile:     filepath.Join(bakeDir, "qemu.pid"),
		Firmware:    &qemu.Firmware{Code: o.OVMFCode, Vars: vars},
		DiskBus:     "ahci", // the VHDX boots from SATA; viostor is installed during this boot
		SeedBus:     "ahci",
		CDROMs:      []string{iso},
		ExtraDisks:  []qemu.Disk{{Path: dummy, Format: "raw"}}, // makes viostor bind → boot-start
		HyperV:      true,
		AllowReboot: true, // OOBE reboots between specialize and oobeSystem
	})
	if err != nil {
		return err
	}
	select {
	case <-vm.Done():
	case <-time.After(o.Timeout):
		_ = vm.Kill()
		return fmt.Errorf("windows bake timed out after %v; console tail:\n%s", o.Timeout, consoleTail(console))
	case <-ctx.Done():
		_ = vm.Kill()
		return ctx.Err()
	}
	consoleOut, err := os.ReadFile(console)
	if err != nil {
		return fmt.Errorf("read console log: %w", err)
	}
	if !bytes.Contains(consoleOut, []byte("BAKE-OK")) {
		return fmt.Errorf("windows bake failed (no BAKE-OK sentinel); console tail:\n%s", lastBytes(consoleOut, 2000))
	}

	newBase := filepath.Join(o.ImageDir, WindowsBase+".new")
	if out, err := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", overlay, newBase).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %v: %s", err, out)
	}
	if err := os.Rename(newBase, filepath.Join(o.ImageDir, WindowsBase)); err != nil {
		return err
	}
	var imgMeta downloadMeta
	if mb, err := os.ReadFile(vhdx + ".meta"); err == nil {
		_ = json.Unmarshal(mb, &imgMeta)
	}
	meta, err := json.MarshalIndent(map[string]string{
		"runner_version": runner.Version,
		"git_version":    git.Version,
		"image_url":      o.ImageURL,
		"image_etag":     imgMeta.ETag,
		"baked_at":       time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.ImageDir, WindowsBaseMeta), append(meta, '\n'), 0o644); err != nil {
		return err
	}
	o.Log.Info("windows bake complete", "base", filepath.Join(o.ImageDir, WindowsBase))
	return nil
}

// fetch downloads url to dest: checksum-verified when sha is set,
// otherwise conditionally (ETag/Last-Modified). A network failure on the
// conditional path keeps an existing file, so an upstream outage does
// not block a rebake.
func (o *WindowsOptions) fetch(ctx context.Context, url, dest, sha, what string) error {
	o.Log.Info("downloading "+what+" (cached if unchanged)", "url", url)
	if sha != "" {
		return DownloadVerified(ctx, o.HTTP, url, dest, sha)
	}
	cached, err := DownloadConditional(ctx, o.HTTP, url, dest)
	if err != nil {
		if _, statErr := os.Stat(dest); statErr == nil {
			o.Log.Warn("conditional download failed; using the cached file", "what", what, "err", err)
			return nil
		}
		return fmt.Errorf("download %s: %w", what, err)
	}
	if cached {
		o.Log.Info(what + " unchanged upstream; using cached file")
	}
	return nil
}
